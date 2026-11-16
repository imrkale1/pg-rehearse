package synth

import (
	"math"
	"math/rand/v2"
	"sort"
)

// This file holds the pure distribution helpers. They know nothing about
// Postgres and are unit tested in sampler_test.go.

// resolveDistinct turns a pg_stats n_distinct value into an absolute number of
// distinct non-null values for a table of the given row count.
// It returns -1 when the profile does not know (n_distinct == 0).
func resolveDistinct(nd float64, rows int64) int64 {
	if rows <= 0 {
		return 0
	}
	var d float64
	switch {
	case nd > 0:
		d = nd
	case nd < 0:
		d = -nd * float64(rows)
	default:
		return -1
	}
	n := int64(math.Round(d))
	if n < 1 {
		n = 1
	}
	if n > rows {
		n = rows
	}
	return n
}

// mcvTable picks most-common values with their observed frequencies.
// Frequencies in pg_stats are fractions of ALL rows (nulls included); the table
// stores cumulative probabilities conditional on the value being non-null.
type mcvTable struct {
	cum   []float64 // cumulative conditional probability, ascending
	total float64   // P(value is one of the MCVs | non-null)
}

func newMCVTable(freqs []float64, nullFrac float64) mcvTable {
	nonNull := 1 - nullFrac
	if nonNull <= 0 {
		nonNull = 1
	}
	t := mcvTable{cum: make([]float64, len(freqs))}
	acc := 0.0
	for i, f := range freqs {
		if f < 0 {
			f = 0
		}
		acc += f / nonNull
		t.cum[i] = acc
	}
	if acc > 1 {
		// Normalise inconsistent input instead of failing.
		for i := range t.cum {
			t.cum[i] /= acc
		}
		acc = 1
	}
	t.total = acc
	return t
}

// pick returns the MCV index for u in [0,1), or -1 if u falls outside the MCV mass.
func (t mcvTable) pick(u float64) int {
	if u >= t.total || len(t.cum) == 0 {
		return -1
	}
	i := sort.SearchFloat64s(t.cum, u)
	// SearchFloat64s returns the first index with cum >= u; we want cum > u.
	for i < len(t.cum) && t.cum[i] <= u {
		i++
	}
	if i >= len(t.cum) {
		i = len(t.cum) - 1
	}
	return i
}

// histQuantile maps q in [0,1] onto an equal-frequency histogram (pg_stats
// histogram_bounds semantics): each bucket holds the same share of rows and
// values are uniform inside a bucket.
func histQuantile(bounds []float64, q float64) float64 {
	n := len(bounds)
	switch n {
	case 0:
		return 0
	case 1:
		return bounds[0]
	}
	if q <= 0 {
		return bounds[0]
	}
	if q >= 1 {
		return bounds[n-1]
	}
	pos := q * float64(n-1)
	i := int(pos)
	if i >= n-1 {
		i = n - 2
	}
	frac := pos - float64(i)
	return bounds[i] + frac*(bounds[i+1]-bounds[i])
}

// histQuantileInt is histQuantile for int64 bounds without float precision loss
// on large values.
func histQuantileInt(bounds []int64, q float64) int64 {
	n := len(bounds)
	switch n {
	case 0:
		return 0
	case 1:
		return bounds[0]
	}
	if q <= 0 {
		return bounds[0]
	}
	if q >= 1 {
		return bounds[n-1]
	}
	pos := q * float64(n-1)
	i := int(pos)
	if i >= n-1 {
		i = n - 2
	}
	frac := pos - float64(i)
	lo, hi := bounds[i], bounds[i+1]
	return lo + int64(math.Floor(frac*float64(hi-lo)+0.5))
}

// histIndex returns the histogram bucket index (lower bound) for q, used for
// types we cannot interpolate (text): we pick a bound value.
func histIndex(n int, q float64) int {
	if n <= 0 {
		return 0
	}
	i := int(q * float64(n))
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return i
}

// defaultSkew is the exponent of the power-law used for foreign keys: index =
// floor(n * u^skew). 1 is uniform; 2 gives a long-tailed "some parents have many
// children" distribution while still touching most parents.
const defaultSkew = 2.0

// skewPicker chooses parent slots for a foreign key column. The first len(mcv)
// slots receive the child column's MCV frequencies (the shape of the hottest
// parents in production); the remaining mass is spread over the remaining
// slots with a power-law.
type skewPicker struct {
	n    int // number of slots (distinct parents to use)
	mcv  mcvTable
	skew float64
}

func newSkewPicker(n int, mcvFreqs []float64, nullFrac, skew float64) skewPicker {
	if len(mcvFreqs) > n {
		mcvFreqs = mcvFreqs[:n]
	}
	if skew <= 0 {
		skew = 1
	}
	return skewPicker{n: n, mcv: newMCVTable(mcvFreqs, nullFrac), skew: skew}
}

func (p skewPicker) pick(r *rand.Rand) int {
	if p.n <= 1 {
		return 0
	}
	if i := p.mcv.pick(r.Float64()); i >= 0 {
		return i
	}
	k := len(p.mcv.cum)
	rest := p.n - k
	if rest <= 0 {
		return r.IntN(p.n)
	}
	u := r.Float64()
	if p.skew != 1 {
		u = math.Pow(u, p.skew)
	}
	j := int(u * float64(rest))
	if j >= rest {
		j = rest - 1
	}
	return k + j
}

// treeParent picks a parent among the rows ordered before row i, skewed
// towards earlier rows, which yields a forest with a few very wide nodes and
// no cycles. It returns -1 for i == 0.
func treeParent(r *rand.Rand, i int, skew float64) int {
	if i <= 0 {
		return -1
	}
	u := r.Float64()
	if skew != 1 {
		u = math.Pow(u, skew)
	}
	j := int(u * float64(i))
	if j >= i {
		j = i - 1
	}
	return j
}

// correlatedQuantile returns the quantile for row `row` of `rows`, honoring the
// pg_stats physical-order correlation: |corr| near 1 means values follow the
// row order (e.g. created_at vs. insertion order).
func correlatedQuantile(r *rand.Rand, row, rows int64, corr float64) float64 {
	if rows <= 0 {
		return r.Float64()
	}
	ac := math.Abs(corr)
	if ac < 0.5 {
		return r.Float64()
	}
	base := (float64(row) + 0.5) / float64(rows)
	if corr < 0 {
		base = 1 - base
	}
	// Blend: the weaker the correlation, the more noise.
	noise := (1 - ac) * (r.Float64()*2 - 1)
	q := base + noise
	if q < 0 {
		q = -q
	}
	if q >= 1 {
		q = 2 - q
		if q >= 1 {
			q = math.Nextafter(1, 0)
		}
	}
	return q
}

// splitmix64 is a fast stateless mixer used to derive per-value pseudo-random
// bits from an index (so value(k) is stable regardless of generation order).
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// fnv64 hashes a string (table/column names) into seed material.
func fnv64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func newRand(seed int64, salt uint64, n uint64) *rand.Rand {
	return rand.New(rand.NewPCG(splitmix64(uint64(seed)^salt), splitmix64(salt+n*0x9e3779b97f4a7c15)))
}
