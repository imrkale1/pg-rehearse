package synth

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// value is one output field in COPY text form (unescaped).
type value struct {
	b    []byte
	null bool
}

// gen fills one or more output fields of a row.
type gen interface {
	fill(r *rand.Rand, row int64, out []value)
}

// valuer produces the text form of a non-null value. q is a quantile in [0,1)
// and k a slot index; when unique is set the valuer must be injective in k.
type valuer interface {
	gen(buf []byte, q float64, k int64, unique bool) []byte
}

// ---------- scalar column generator ----------

type scalarGen struct {
	idx      int
	nullFrac float64
	mcvVals  [][]byte
	mcv      mcvTable
	distinct int64 // number of non-MCV slots; -1 = continuous
	unique   bool
	corr     float64
	rows     int64
	v        valuer
}

func (g *scalarGen) fill(r *rand.Rand, row int64, out []value) {
	o := &out[g.idx]
	o.b = o.b[:0]
	o.null = false
	if g.nullFrac > 0 && r.Float64() < g.nullFrac {
		o.null = true
		return
	}
	if g.unique {
		q := (float64(row) + 0.5) / float64(max(g.rows, 1))
		o.b = g.v.gen(o.b, q, row, true)
		return
	}
	if len(g.mcvVals) > 0 {
		if i := g.mcv.pick(r.Float64()); i >= 0 {
			o.b = append(o.b, g.mcvVals[i]...)
			return
		}
	}
	q := correlatedQuantile(r, row, g.rows, g.corr)
	var k int64
	if g.distinct > 0 {
		k = int64(q * float64(g.distinct))
		if k >= g.distinct {
			k = g.distinct - 1
		}
		q = (float64(k) + 0.5) / float64(g.distinct)
	} else {
		k = int64(r.Uint64() >> 1)
	}
	o.b = g.v.gen(o.b, q, k, false)
}

// constGen always writes NULL (e.g. deferred FK columns, all-null columns).
type nullGen struct{ idxs []int }

func (g *nullGen) fill(_ *rand.Rand, _ int64, out []value) {
	for _, i := range g.idxs {
		out[i].b = out[i].b[:0]
		out[i].null = true
	}
}

// seqGen writes row+start for sequence-backed integer columns (identity /
// serial); sequences are setval'd after the load.
type seqGen struct {
	idx   int
	start int64
	step  int64
}

func (g *seqGen) fill(_ *rand.Rand, row int64, out []value) {
	o := &out[g.idx]
	o.null = false
	o.b = strconv.AppendInt(o.b[:0], g.start+row*g.step, 10)
}

// ---------- foreign key generator ----------

type fkGen struct {
	idxs     []int
	keys     [][]string // slot -> parent key tuple
	picker   skewPicker
	nullFrac float64
	unique   bool
}

func (g *fkGen) fill(r *rand.Rand, row int64, out []value) {
	null := len(g.keys) == 0 || (g.nullFrac > 0 && r.Float64() < g.nullFrac)
	var tup []string
	if !null {
		if g.unique {
			if row < int64(len(g.keys)) {
				tup = g.keys[row]
			} else {
				null = true
			}
		} else {
			tup = g.keys[g.picker.pick(r)]
		}
	}
	for j, i := range g.idxs {
		o := &out[i]
		o.b = o.b[:0]
		o.null = null
		if !null {
			o.b = append(o.b, tup[j]...)
		}
	}
}

// ---------- building a scalar generator from the profile ----------

// newScalarGen builds the generator for column c. It returns (nil, nil) when the
// column should be omitted from COPY (database default takes over).
func newScalarGen(t *tableInfo, c *colInfo, cp *model.ColumnProfile, idx int, rows int64, seed int64) (gen, error) {
	g := &scalarGen{idx: idx, rows: rows, unique: c.unique, distinct: -1}
	if cp != nil && !c.notNull {
		g.nullFrac = clamp01(cp.NullFrac)
	}
	width := 0
	var hist, mcv []string
	var freqs []float64
	sensitive := false
	if cp != nil {
		width = cp.AvgWidth
		sensitive = cp.Sensitive
		g.corr = cp.Correlation
		if !sensitive {
			hist = cp.HistogramBounds
			if len(cp.MostCommonVals) == len(cp.MostCommonFreqs) && !c.unique {
				mcv, freqs = cp.MostCommonVals, cp.MostCommonFreqs
			}
		}
	}
	if c.typ.kind == kEnum {
		mcv, freqs = filterLabels(mcv, freqs, c.typ.labels)
		hist = filterHist(hist, c.typ.labels)
		if len(c.typ.labels) == 0 {
			return nil, fmt.Errorf("table %s column %s: enum type %s has no labels", t.qname(), c.name, c.typeSQL)
		}
	}
	nonNullRows := int64(math.Round(float64(rows) * (1 - g.nullFrac)))
	d := int64(-1)
	if cp != nil {
		d = resolveDistinct(cp.NDistinct, rows)
		if d > 0 && nonNullRows > 0 && d > nonNullRows {
			d = nonNullRows
		}
	}
	if len(mcv) > 0 {
		g.mcvVals = make([][]byte, len(mcv))
		for i, m := range mcv {
			g.mcvVals[i] = []byte(m)
		}
		g.mcv = newMCVTable(freqs, g.nullFrac)
		if d > 0 {
			d -= int64(len(mcv))
			if d < 1 {
				d = 1
			}
		}
	}
	salt := fnv64(t.qname() + "." + c.name)
	v, continuous, err := newValuer(c, hist, width, d, rows, sensitive, salt)
	if err != nil {
		return nil, err
	}
	if v == nil {
		// Unknown type, nothing to copy from.
		if len(g.mcvVals) > 0 && g.mcv.total >= 0.999 {
			v = verbatimValuer{vals: mcv}
		} else if !c.notNull {
			return &nullGen{idxs: []int{idx}}, nil
		} else if c.hasDefault {
			return nil, nil
		} else if len(g.mcvVals) > 0 {
			v = verbatimValuer{vals: mcv}
		} else {
			return nil, fmt.Errorf("table %s column %s: cannot synthesize values of type %s (NOT NULL, no default, no statistics)", t.qname(), c.name, c.typeSQL)
		}
	}
	g.v = v
	switch {
	case d > 0:
		g.distinct = d
	case continuous:
		g.distinct = -1
	default:
		g.distinct = max(nonNullRows, 1)
	}
	return g, nil
}

func clamp01(f float64) float64 {
	if f < 0 || math.IsNaN(f) {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

func filterLabels(vals []string, freqs []float64, labels []string) ([]string, []float64) {
	ok := map[string]bool{}
	for _, l := range labels {
		ok[l] = true
	}
	var ov []string
	var of []float64
	for i, v := range vals {
		if ok[v] {
			ov = append(ov, v)
			of = append(of, freqs[i])
		}
	}
	return ov, of
}

func filterHist(vals []string, labels []string) []string {
	ok := map[string]bool{}
	for _, l := range labels {
		ok[l] = true
	}
	var out []string
	for _, v := range vals {
		if ok[v] {
			out = append(out, v)
		}
	}
	return out
}

// newValuer picks the value family for a column. continuous reports whether
// the valuer interpolates (so an unknown n_distinct may stay unbounded).
func newValuer(c *colInfo, hist []string, width int, distinct, rows int64, sensitive bool, salt uint64) (valuer, bool, error) {
	ti := c.typ
	if sensitive && ti.kind == kText {
		return fakeValuer{kind: classifyFake(c.name), width: textWidth(width), maxLen: ti.maxLen}, false, nil
	}
	if sensitive && ti.kind == kInet {
		return inetValuer{}, false, nil
	}
	return valuerFor(ti, hist, width, distinct, rows, salt)
}

func valuerFor(ti typeInfo, hist []string, width int, distinct, rows int64, salt uint64) (valuer, bool, error) {
	switch ti.kind {
	case kInt:
		v := intValuer{lo: math.MinInt64, hi: math.MaxInt64}
		switch ti.intBits {
		case 16:
			v.lo, v.hi = math.MinInt16, math.MaxInt16
		case 32:
			v.lo, v.hi = math.MinInt32, math.MaxInt32
		}
		if h, ok := parseInts(hist); ok {
			v.hist = h
		}
		return v, v.hist != nil, nil
	case kNumeric, kFloat:
		v := floatValuer{scale: -1, numeric: ti.kind == kNumeric}
		if ti.kind == kNumeric {
			v.scale = ti.scale
			if ti.prec > 0 {
				s := max(ti.scale, 0)
				v.maxAbs = math.Pow10(ti.prec-s) - math.Pow10(-s)
			}
			if v.scale < 0 {
				v.scale = maxDecimals(hist, 2)
			}
		}
		if h, ok := parseFloats(hist); ok {
			v.hist = h
		}
		return v, v.hist != nil, nil
	case kBool:
		return boolValuer{}, false, nil
	case kText:
		v := textValuer{width: textWidth(width), maxLen: ti.maxLen}
		if len(hist) > 0 {
			v.hist = hist
			v.suffix = distinct < 0 && rows > int64(len(hist))*4 || distinct > int64(len(hist))
		}
		return v, false, nil
	case kUUID:
		return uuidValuer{salt: salt}, false, nil
	case kTimestamp, kTimestamptz, kDate, kTime:
		v := timeValuer{kind: ti.kind}
		if h, ok := parseTimes(ti.kind, hist); ok {
			v.hist = h
		} else if len(hist) > 0 {
			return verbatimValuer{vals: hist}, false, nil
		}
		return v, true, nil
	case kEnum:
		if len(hist) > 0 {
			return verbatimValuer{vals: hist}, false, nil
		}
		return verbatimValuer{vals: ti.labels}, false, nil
	case kJSON:
		if len(hist) > 0 {
			return verbatimValuer{vals: hist}, false, nil
		}
		return jsonValuer{width: max(width, 16)}, false, nil
	case kBytea:
		return byteaValuer{width: max(width-1, 4)}, false, nil
	case kInterval:
		if len(hist) > 0 {
			return verbatimValuer{vals: hist}, false, nil
		}
		return intervalValuer{}, false, nil
	case kInet:
		if len(hist) > 0 {
			return verbatimValuer{vals: hist}, false, nil
		}
		return inetValuer{}, false, nil
	case kArray:
		if len(hist) > 0 {
			return verbatimValuer{vals: hist}, false, nil
		}
		if ti.elem == nil || ti.elem.kind == kArray {
			return nil, false, nil
		}
		ev, _, err := valuerFor(*ti.elem, nil, 8, -1, rows, salt^0xa11a)
		if err != nil || ev == nil {
			return nil, false, err
		}
		ew := 8
		switch ti.elem.kind {
		case kInt:
			ew = max(ti.elem.intBits/8, 2)
		case kBool:
			ew = 1
		case kUUID:
			ew = 16
		}
		n := 3
		if width > 24 {
			n = max((width-24)/ew, 1)
		}
		quote := true
		switch ti.elem.kind {
		case kInt, kFloat, kNumeric, kBool:
			quote = false
		}
		return arrayValuer{elem: ev, avgLen: n, quote: quote}, false, nil
	}
	if len(hist) > 0 {
		return verbatimValuer{vals: hist}, false, nil
	}
	return nil, false, nil
}

func textWidth(avgWidth int) int {
	if avgWidth <= 1 {
		return 12
	}
	return avgWidth - 1 // pg_stats avg_width includes the 1-byte varlena header
}

func maxDecimals(vals []string, def int) int {
	best := -1
	for _, v := range vals {
		if i := strings.IndexByte(v, '.'); i >= 0 {
			best = max(best, len(v)-i-1)
		} else if best < 0 {
			best = 0
		}
	}
	if best < 0 {
		return def
	}
	return min(best, 6)
}

func parseInts(vals []string) ([]int64, bool) {
	if len(vals) == 0 {
		return nil, false
	}
	out := make([]int64, len(vals))
	for i, v := range vals {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

func parseFloats(vals []string) ([]float64, bool) {
	if len(vals) == 0 {
		return nil, false
	}
	out := make([]float64, len(vals))
	for i, v := range vals {
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, false
		}
		out[i] = f
	}
	return out, true
}

var tsLayouts = []string{
	"2006-01-02 15:04:05.999999-07",
	"2006-01-02 15:04:05.999999-07:00",
	"2006-01-02 15:04:05.999999-07:00:00",
	"2006-01-02 15:04:05.999999",
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999",
	"2006-01-02",
}

// parseTimes parses histogram bounds into micro-second integers (since epoch,
// or since midnight for time).
func parseTimes(k kind, vals []string) ([]int64, bool) {
	if len(vals) == 0 {
		return nil, false
	}
	out := make([]int64, len(vals))
	for i, v := range vals {
		v = strings.TrimSpace(v)
		if k == kTime {
			tt, err := time.Parse("15:04:05.999999", v)
			if err != nil {
				return nil, false
			}
			out[i] = int64(tt.Hour())*3600e6 + int64(tt.Minute())*60e6 + int64(tt.Second())*1e6 + int64(tt.Nanosecond()/1000)
			continue
		}
		var tt time.Time
		var err error
		for _, l := range tsLayouts {
			if tt, err = time.Parse(l, v); err == nil {
				break
			}
		}
		if err != nil {
			return nil, false
		}
		out[i] = tt.UnixMicro()
	}
	return out, true
}

// ---------- valuers ----------

type intValuer struct {
	hist   []int64
	lo, hi int64
}

func (v intValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	var n int64
	switch {
	case unique:
		base := int64(1)
		if len(v.hist) > 0 && v.hist[0] > v.lo {
			base = v.hist[0]
		}
		n = base + k
		if n > v.hi || n < base { // overflow: fall back to plain index
			n = v.lo + k
		}
	case len(v.hist) > 0:
		n = histQuantileInt(v.hist, q)
	default:
		n = k + 1
	}
	n = min(max(n, v.lo), v.hi)
	return strconv.AppendInt(buf, n, 10)
}

type floatValuer struct {
	hist    []float64
	scale   int // decimals for numeric, -1 = shortest repr
	maxAbs  float64
	numeric bool
}

func (v floatValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	var f float64
	switch {
	case unique:
		step := 1.0
		if v.scale > 0 {
			step = math.Pow10(-v.scale)
		}
		base := 0.0
		if len(v.hist) > 0 {
			base = v.hist[0]
		}
		f = base + float64(k)*step
	case len(v.hist) > 0:
		f = histQuantile(v.hist, q)
	default:
		f = float64(k%100000) + q
	}
	if v.maxAbs > 0 {
		f = math.Max(math.Min(f, v.maxAbs), -v.maxAbs)
	}
	if v.numeric {
		return strconv.AppendFloat(buf, f, 'f', max(v.scale, 0), 64)
	}
	return strconv.AppendFloat(buf, f, 'g', -1, 64)
}

type boolValuer struct{}

func (boolValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	if unique {
		q = float64(k % 2)
	}
	if q < 0.5 {
		return append(buf, 'f')
	}
	return append(buf, 't')
}

type textValuer struct {
	hist   []string
	suffix bool
	width  int
	maxLen int
}

func (v textValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	start := len(buf)
	keepTail := unique || v.suffix
	if len(v.hist) > 0 {
		buf = append(buf, v.hist[histIndex(len(v.hist), q)]...)
		if keepTail {
			buf = append(buf, '-')
			buf = strconv.AppendInt(buf, k, 36)
		}
	} else {
		buf = appendWord(buf, k, v.width)
		keepTail = true
	}
	return truncate(buf, start, v.maxLen, keepTail)
}

// truncate shortens buf[start:] to maxLen characters, keeping the tail (which
// carries the distinguishing suffix) when keepTail is set.
func truncate(buf []byte, start, maxLen int, keepTail bool) []byte {
	if maxLen <= 0 {
		return buf
	}
	s := buf[start:]
	n := utf8.RuneCount(s)
	if n <= maxLen {
		return buf
	}
	if keepTail {
		drop := n - maxLen
		off := 0
		for i := 0; i < drop; i++ {
			_, sz := utf8.DecodeRune(s[off:])
			off += sz
		}
		m := copy(s, s[off:])
		return buf[:start+m]
	}
	off := 0
	for i := 0; i < maxLen; i++ {
		_, sz := utf8.DecodeRune(s[off:])
		off += sz
	}
	return buf[:start+off]
}

type fakeValuer struct {
	kind   fakeKind
	width  int
	maxLen int
}

func (v fakeValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	start := len(buf)
	buf = fakeValue(buf, v.kind, k, v.width, unique)
	return truncate(buf, start, v.maxLen, true)
}

type uuidValuer struct{ salt uint64 }

func (v uuidValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	a := splitmix64(uint64(k) ^ v.salt)
	b := splitmix64(a ^ uint64(k))
	if unique {
		// Make injective: low 64 bits carry k verbatim (mixed bijectively).
		b = splitmix64(uint64(k)) ^ 0 // splitmix64 is a bijection on uint64
	}
	a = (a &^ 0xf000) | 0x4000       // version 4
	b = (b &^ (3 << 62)) | (1 << 63) // variant 10
	if unique {
		// The variant bits overwrote 2 bits of b; fold them into a so that
		// distinct k stay distinct.
		a ^= (splitmix64(uint64(k)) >> 62) << 20
	}
	var raw [16]byte
	for i := 0; i < 8; i++ {
		raw[i] = byte(a >> (56 - 8*i))
		raw[8+i] = byte(b >> (56 - 8*i))
	}
	for i, x := range raw {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf = append(buf, '-')
		}
		buf = append(buf, hexDigits[x>>4], hexDigits[x&15])
	}
	return buf
}

type timeValuer struct {
	kind kind
	hist []int64 // micros
}

var defaultTimeBase = time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro()

const microsPerDay = int64(86400e6)

func (v timeValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	var us int64
	switch {
	case unique:
		step := int64(1e6)
		if v.kind == kDate {
			step = microsPerDay
		}
		base := defaultTimeBase
		if len(v.hist) > 0 {
			base = v.hist[0]
		}
		us = base + k*step
	case len(v.hist) > 0:
		us = histQuantileInt(v.hist, q)
	default:
		span := int64(2 * 365 * microsPerDay)
		if v.kind == kTime {
			us = int64(q * float64(microsPerDay))
		} else {
			us = defaultTimeBase + int64(q*float64(span))
		}
	}
	switch v.kind {
	case kTime:
		us = ((us % microsPerDay) + microsPerDay) % microsPerDay
		tt := time.UnixMicro(us).UTC()
		return tt.AppendFormat(buf, "15:04:05.999999")
	case kDate:
		return time.UnixMicro(us).UTC().AppendFormat(buf, "2006-01-02")
	case kTimestamptz:
		return time.UnixMicro(us).UTC().AppendFormat(buf, "2006-01-02 15:04:05.999999-07:00")
	default:
		return time.UnixMicro(us).UTC().AppendFormat(buf, "2006-01-02 15:04:05.999999")
	}
}

type verbatimValuer struct{ vals []string }

func (v verbatimValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	if unique {
		return append(buf, v.vals[k%int64(len(v.vals))]...)
	}
	return append(buf, v.vals[histIndex(len(v.vals), q)]...)
}

type jsonValuer struct{ width int }

func (v jsonValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	buf = append(buf, `{"id": `...)
	buf = strconv.AppendInt(buf, k, 10)
	buf = append(buf, `, "tag": "`...)
	buf = appendWord(buf, k, max(v.width-20, 4))
	return append(buf, `"}`...)
}

type byteaValuer struct{ width int }

func (v byteaValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	buf = append(buf, `\x`...)
	if unique {
		s := strconv.FormatUint(uint64(k), 16)
		if len(s)%2 == 1 {
			buf = append(buf, '0')
		}
		buf = append(buf, s...)
	}
	return appendHex(buf, splitmix64(uint64(k)), 2*v.width)
}

type intervalValuer struct{}

func (intervalValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	secs := int64(q * 30 * 86400)
	if unique {
		secs = k
	}
	buf = strconv.AppendInt(buf, secs, 10)
	return append(buf, " seconds"...)
}

type inetValuer struct{}

func (inetValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	if !unique {
		k = int64(splitmix64(uint64(k)) & 0xffffff)
	}
	return appendIPv4(buf, k)
}

type arrayValuer struct {
	elem   valuer
	avgLen int
	quote  bool
}

func (v arrayValuer) gen(buf []byte, q float64, k int64, unique bool) []byte {
	h := splitmix64(uint64(k) ^ 0xa5a5)
	n := int(h % uint64(2*v.avgLen+1))
	if unique && n == 0 {
		n = 1
	}
	buf = append(buf, '{')
	for i := 0; i < n; i++ {
		if i > 0 {
			buf = append(buf, ',')
		}
		h = splitmix64(h)
		ek := int64(h >> 33)
		if unique && i == 0 {
			ek = k
		}
		eq := float64(h>>11) / (1 << 53)
		if !v.quote {
			buf = v.elem.gen(buf, eq, ek, unique && i == 0)
			continue
		}
		buf = append(buf, '"')
		start := len(buf)
		buf = v.elem.gen(buf, eq, ek, unique && i == 0)
		// Escape quotes/backslashes inside the element.
		if hasArrayMeta(buf[start:]) {
			el := append([]byte(nil), buf[start:]...)
			buf = buf[:start]
			for _, c := range el {
				if c == '"' || c == '\\' {
					buf = append(buf, '\\')
				}
				buf = append(buf, c)
			}
		}
		buf = append(buf, '"')
	}
	return append(buf, '}')
}

func hasArrayMeta(b []byte) bool {
	for _, c := range b {
		if c == '"' || c == '\\' {
			return true
		}
	}
	return false
}
