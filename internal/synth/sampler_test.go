package synth

import (
	"math"
	"regexp"
	"testing"
)

func TestResolveDistinct(t *testing.T) {
	cases := []struct {
		nd   float64
		rows int64
		want int64
	}{
		{0, 100, -1}, {5, 100, 5}, {500, 100, 100}, {-0.5, 100, 50}, {-1, 100, 100}, {-0.001, 100, 1}, {5, 0, 0},
	}
	for _, c := range cases {
		if got := resolveDistinct(c.nd, c.rows); got != c.want {
			t.Errorf("resolveDistinct(%v, %d) = %d, want %d", c.nd, c.rows, got, c.want)
		}
	}
}

func TestMCVTable(t *testing.T) {
	freqs := []float64{0.45, 0.27, 0.09} // of all rows, with 10% nulls
	m := newMCVTable(freqs, 0.1)
	if math.Abs(m.total-0.9) > 1e-9 {
		t.Fatalf("total = %v, want 0.9", m.total)
	}
	r := newRand(1, 2, 3)
	counts := make([]int, 4)
	const n = 200000
	for i := 0; i < n; i++ {
		k := m.pick(r.Float64())
		if k < 0 {
			k = 3
		}
		counts[k]++
	}
	want := []float64{0.5, 0.3, 0.1, 0.1}
	for i, w := range want {
		if got := float64(counts[i]) / n; math.Abs(got-w) > 0.01 {
			t.Errorf("bucket %d: freq %.4f, want %.2f", i, got, w)
		}
	}
	// Inconsistent input is normalised.
	if m := newMCVTable([]float64{0.8, 0.8}, 0); m.total != 1 || m.pick(0.99) != 1 || m.pick(0.1) != 0 {
		t.Errorf("normalisation failed: %+v", m)
	}
	if (mcvTable{}).pick(0.3) != -1 {
		t.Error("empty table must not pick")
	}
}

func TestHistQuantile(t *testing.T) {
	b := []float64{0, 10, 20, 100}
	for q, want := range map[float64]float64{0: 0, 1: 100, 0.5: 15, 1.0 / 3: 10, 5.0 / 6: 60, -1: 0, 2: 100} {
		if got := histQuantile(b, q); math.Abs(got-want) > 1e-9 {
			t.Errorf("histQuantile(%v) = %v, want %v", q, got, want)
		}
	}
	if histQuantile(nil, 0.5) != 0 || histQuantile([]float64{7}, 0.5) != 7 {
		t.Error("degenerate histograms")
	}
	ib := []int64{math.MaxInt64 - 1000, math.MaxInt64 - 500, math.MaxInt64}
	if got := histQuantileInt(ib, 0.25); got != math.MaxInt64-750 {
		t.Errorf("histQuantileInt lost precision: %d", got)
	}
	// Equal-frequency: half the draws fall below the middle bound.
	r := newRand(4, 5, 6)
	below := 0
	for i := 0; i < 100000; i++ {
		if histQuantile([]float64{0, 1, 1000}, r.Float64()) < 1 {
			below++
		}
	}
	if f := float64(below) / 100000; math.Abs(f-0.5) > 0.01 {
		t.Errorf("median bucket share %.3f, want 0.5", f)
	}
	if histIndex(5, 0.999) != 4 || histIndex(5, 0) != 0 || histIndex(0, 0.5) != 0 {
		t.Error("histIndex bounds")
	}
}

func TestSkewPicker(t *testing.T) {
	const slots, n = 1000, 200000
	p := newSkewPicker(slots, []float64{0.2, 0.1}, 0, defaultSkew)
	r := newRand(7, 8, 9)
	counts := make([]int, slots)
	for i := 0; i < n; i++ {
		k := p.pick(r)
		if k < 0 || k >= slots {
			t.Fatalf("pick out of range: %d", k)
		}
		counts[k]++
	}
	if f := float64(counts[0]) / n; math.Abs(f-0.2) > 0.01 {
		t.Errorf("mcv slot 0 freq %.3f, want 0.2", f)
	}
	if f := float64(counts[1]) / n; math.Abs(f-0.1) > 0.01 {
		t.Errorf("mcv slot 1 freq %.3f, want 0.1", f)
	}
	// Power-law tail: the first tenth of the non-MCV slots gets ~sqrt(0.1) of the rest.
	head := 0
	for _, c := range counts[2:102] {
		head += c
	}
	if f := float64(head) / (0.7 * n); math.Abs(f-math.Sqrt(0.1)) > 0.02 {
		t.Errorf("head share %.3f, want %.3f", f, math.Sqrt(0.1))
	}
	used := 0
	for _, c := range counts {
		if c > 0 {
			used++
		}
	}
	if used < slots*9/10 {
		t.Errorf("only %d/%d slots used", used, slots)
	}
	if newSkewPicker(1, nil, 0, 2).pick(r) != 0 {
		t.Error("single slot")
	}
}

func TestTreeParentAndCorrelation(t *testing.T) {
	r := newRand(1, 1, 1)
	if treeParent(r, 0, 2) != -1 {
		t.Error("row 0 has no parent")
	}
	for i := 1; i < 5000; i++ {
		if p := treeParent(r, i, 2); p < 0 || p >= i {
			t.Fatalf("treeParent(%d) = %d", i, p)
		}
	}
	prev := -1.0
	for row := int64(0); row < 1000; row++ {
		q := correlatedQuantile(r, row, 1000, 1)
		if q <= prev || q < 0 || q >= 1 {
			t.Fatalf("corr=1 must be increasing in [0,1): row %d q %v", row, q)
		}
		prev = q
	}
	for row := int64(0); row < 1000; row++ {
		if q := correlatedQuantile(r, row, 1000, -0.7); q < 0 || q >= 1 {
			t.Fatalf("quantile out of range: %v", q)
		}
	}
}

func TestDeterministicRand(t *testing.T) {
	a, b, c := newRand(42, 1, 0), newRand(42, 1, 0), newRand(43, 1, 0)
	same, diff := true, false
	for i := 0; i < 100; i++ {
		x, y, z := a.Uint64(), b.Uint64(), c.Uint64()
		same = same && x == y
		diff = diff || x != z
	}
	if !same || !diff {
		t.Errorf("same=%v diff=%v", same, diff)
	}
}

func TestValuers(t *testing.T) {
	uuidRe := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	u := uuidValuer{salt: 99}
	for k := int64(0); k < 50000; k++ {
		s := string(u.gen(nil, 0, k, true))
		if !uuidRe.MatchString(s) {
			t.Fatalf("bad uuid %q", s)
		}
		if seen[s] {
			t.Fatalf("duplicate uuid %q", s)
		}
		seen[s] = true
	}
	// Unique fake values stay unique even when truncated to varchar(n).
	for _, fk := range []fakeKind{fakeEmail, fakeFullName, fakeFirstName, fakeUsername, fakeGeneric, fakeAddress, fakeCard} {
		seen := map[string]bool{}
		v := fakeValuer{kind: fk, width: 20, maxLen: 24}
		for k := int64(0); k < 20000; k++ {
			s := string(v.gen(nil, 0, k, true))
			if seen[s] || len(s) > 24 {
				t.Fatalf("fake kind %d: bad value %q (dup=%v)", fk, s, seen[s])
			}
			seen[s] = true
		}
	}
	if classifyFake("email_address") != fakeEmail || classifyFake("full_name") != fakeFullName || classifyFake("phone_number") != fakePhone {
		t.Error("classifyFake")
	}
	// Text width tracks avg_width; numeric honours scale and precision.
	tv := textValuer{width: 30}
	total := 0
	for k := int64(0); k < 1000; k++ {
		total += len(tv.gen(nil, 0, k, false))
	}
	if avg := float64(total) / 1000; math.Abs(avg-30) > 2 {
		t.Errorf("avg text width %.1f, want ~30", avg)
	}
	fv := floatValuer{numeric: true, scale: 2, maxAbs: 999.99, hist: []float64{0, 5000}}
	if got := string(fv.gen(nil, 0.5, 0, false)); got != "999.99" {
		t.Errorf("numeric clamp: %q", got)
	}
	if got := string(truncate([]byte("abcdef"), 0, 3, true)); got != "def" {
		t.Errorf("truncate tail: %q", got)
	}
	if got := string(truncate([]byte("héllo"), 0, 2, false)); got != "hé" {
		t.Errorf("truncate head: %q", got)
	}
}

func TestCopyEscapeAndDedupe(t *testing.T) {
	row := appendCopyRow(nil, []value{{b: []byte("a\tb\\c\nd\r")}, {null: true}, {b: []byte("x")}})
	if want := "a\\tb\\\\c\\nd\\r\t\\N\tx\n"; string(row) != want {
		t.Errorf("copy row = %q, want %q", row, want)
	}
	d := newDeduper([][]int{{0, 1}})
	v := func(a, b string) []value { return []value{{b: []byte(a)}, {b: []byte(b)}} }
	if !d.add(v("1", "2")) || d.add(v("1", "2")) || !d.add(v("12", "")) || !d.add(v("1", "3")) {
		t.Error("deduper")
	}
	if !d.add([]value{{b: []byte("1")}, {null: true}}) || !d.add([]value{{b: []byte("1")}, {null: true}}) {
		t.Error("NULLs never collide")
	}
}
