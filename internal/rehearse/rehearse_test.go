package rehearse

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

func TestDefaults(t *testing.T) {
	var o Options
	o.defaults()
	if o.Scale != 1 || o.RateMultiplier != 1 || o.MaxConns != 64 {
		t.Errorf("unexpected numeric defaults: %+v", o)
	}
	if o.Warmup <= 0 || o.Observe <= 0 {
		t.Errorf("durations not defaulted: %+v", o)
	}
	if o.Log == nil {
		t.Error("Log must default to a non-nil writer")
	}
}

func TestVerdict(t *testing.T) {
	cases := []struct {
		name    string
		stallMs float64
		errors  int
		migErr  string
		want    model.Verdict
	}{
		{"quiet", 10, 0, "", model.VerdictSafe},
		{"just under risky", 199, 0, "", model.VerdictSafe},
		{"risky stall", 200, 0, "", model.VerdictRisky},
		{"migration error alone is risky", 5, 0, "boom", model.VerdictRisky},
		{"dangerous stall", 1000, 0, "", model.VerdictDangerous},
		{"any error is dangerous", 5, 1, "", model.VerdictDangerous},
		{"errors outrank a clean migration", 0, 3, "", model.VerdictDangerous},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &model.RehearsalResult{
				MaxStallMs:     c.stallMs,
				MigrationError: c.migErr,
				Impacts:        []model.Impact{{Errors: c.errors}},
			}
			if got := verdict(r); got != c.want {
				t.Errorf("verdict = %q, want %q", got, c.want)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	ds := []time.Duration{5 * time.Millisecond, 1 * time.Millisecond, 3 * time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	if got := percentile(ds, 0.5); got != 3*time.Millisecond {
		t.Errorf("p50 = %v, want 3ms", got)
	}
	// idx = int(0.99*(5-1)) = 3, so p99 of five values is the 4th smallest.
	if got := percentile(ds, 0.99); got != 4*time.Millisecond {
		t.Errorf("p99 = %v, want 4ms", got)
	}
	if got := percentile(ds, 1.0); got != 5*time.Millisecond {
		t.Errorf("p100 = %v, want 5ms", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("empty = %v, want 0", got)
	}
	// input must not be mutated
	if ds[0] != 5*time.Millisecond {
		t.Errorf("percentile mutated its input: %v", ds)
	}
}

func TestImpacts(t *testing.T) {
	base := time.Now()
	from := base.Add(10 * time.Second)
	to := base.Add(20 * time.Second)
	mkBaseline := func(id int64, n int, lat time.Duration) []model.Sample {
		var out []model.Sample
		for i := 0; i < n; i++ {
			out = append(out, model.Sample{QueryID: id, Start: base.Add(time.Duration(i) * time.Second), Latency: lat})
		}
		return out
	}

	var samples []model.Sample
	// q1 (checkout): ~2ms baseline, then one 500ms stall and one error during.
	samples = append(samples, mkBaseline(1, 10, 2*time.Millisecond)...)
	samples = append(samples, model.Sample{QueryID: 1, Start: from.Add(2 * time.Second), Latency: 500 * time.Millisecond})
	samples = append(samples, model.Sample{QueryID: 1, Start: from.Add(4 * time.Second), Latency: 3 * time.Millisecond, Err: "boom"})
	// q3 (product page): smaller 300ms stall, so it sorts after q1.
	samples = append(samples, mkBaseline(3, 5, 1*time.Millisecond)...)
	samples = append(samples, model.Sample{QueryID: 3, Start: from.Add(1 * time.Second), Latency: 300 * time.Millisecond})
	// q2 (admin report): slow query, must be skipped entirely.
	samples = append(samples, model.Sample{QueryID: 2, Start: from.Add(1 * time.Second), Latency: 4 * time.Second})

	qs := []model.QueryProfile{
		{QueryID: 1, Feature: "checkout", MeanMs: 2},
		{QueryID: 2, Feature: "admin report", MeanMs: 500}, // >= slowQueryMs
		{QueryID: 3, Feature: "product page", MeanMs: 2},
	}
	imps := impacts(&model.LoadResult{Samples: samples}, qs, from, to, 0)

	if len(imps) != 2 {
		t.Fatalf("got %d impacts, want 2 (slow query skipped): %+v", len(imps), imps)
	}
	if imps[0].QueryID != 1 || imps[1].QueryID != 3 {
		t.Errorf("impacts not sorted by stall desc: %d then %d", imps[0].QueryID, imps[1].QueryID)
	}
	q1 := imps[0]
	if q1.StallMs < 499 || q1.StallMs > 501 {
		t.Errorf("q1 StallMs = %.1f, want ~500", q1.StallMs)
	}
	if q1.BaselineP99Ms < 1.5 || q1.BaselineP99Ms > 2.5 {
		t.Errorf("q1 BaselineP99Ms = %.2f, want ~2", q1.BaselineP99Ms)
	}
	if q1.Errors != 1 {
		t.Errorf("q1 Errors = %d, want 1", q1.Errors)
	}
	if imps[1].StallMs < 299 || imps[1].StallMs > 301 {
		t.Errorf("q3 StallMs = %.1f, want ~300", imps[1].StallMs)
	}
}

func TestDSNForDB(t *testing.T) {
	got, err := dsnForDB("postgres://shop:shop@localhost:55440/shop?sslmode=disable", "pgr_run_42")
	if err != nil {
		t.Fatal(err)
	}
	want := "postgres://shop:shop@localhost:55440/pgr_run_42?sslmode=disable"
	if got != want {
		t.Errorf("dsnForDB = %q, want %q", got, want)
	}
	if _, err := dsnForDB("::not a dsn::", "x"); err == nil {
		t.Error("expected an error for an unparseable DSN")
	}
}

func TestTemplateName(t *testing.T) {
	opts := Options{
		Profile: &model.Profile{
			SchemaSQL: "CREATE TABLE t (id int);",
			Tables:    []model.TableProfile{{Schema: "public", Name: "t", RowEstimate: 100}},
		},
		Scale: 1, Seed: 1,
	}
	a := templateName(opts)
	if !strings.HasPrefix(a, "pgr_tpl_") {
		t.Errorf("name %q lacks pgr_tpl_ prefix", a)
	}
	if b := templateName(opts); a != b {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
	opts.Seed = 2
	if c := templateName(opts); c == a {
		t.Error("changing the seed must change the template name")
	}
	opts.Seed = 1
	opts.Scale = 0.1
	if c := templateName(opts); c == a {
		t.Error("changing the scale must change the template name")
	}
}

func TestTrafficNote(t *testing.T) {
	note := trafficNote(Options{
		Profile: &model.Profile{
			Queries:  []model.QueryProfile{{CallsPerSec: 100}, {CallsPerSec: 50}},
			LongTxns: []model.LongTxn{{}},
		},
		RateMultiplier: 2,
		Scale:          0.1,
	})
	for _, want := range []string{"300 queries/s", "2 query shapes", "1 long-running", "0.1"} {
		if !strings.Contains(note, want) {
			t.Errorf("traffic note %q lacks %q", note, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("  CREATE INDEX i\nON t (x)"); got != "CREATE INDEX i" {
		t.Errorf("firstLine = %q", got)
	}
	long := strings.Repeat("a", 200)
	got := firstLine(long)
	if len(got) != 80 || !strings.HasSuffix(got, "...") {
		t.Errorf("long line not truncated to 80 with ellipsis: len=%d", len(got))
	}
}

func TestNonTxRegex(t *testing.T) {
	yes := []string{
		"CREATE INDEX CONCURRENTLY i ON t (x)",
		"VACUUM FULL t",
		"ALTER TYPE mood ADD VALUE 'meh'",
	}
	no := []string{
		"ALTER TABLE t ADD COLUMN c int",
		"CREATE INDEX i ON t (x)",
	}
	for _, s := range yes {
		if !nonTxRe.MatchString(s) {
			t.Errorf("expected %q to require autocommit", s)
		}
	}
	for _, s := range no {
		if nonTxRe.MatchString(s) {
			t.Errorf("did not expect %q to require autocommit", s)
		}
	}
}

// ---------- integration: a real rehearsal end to end ----------

func testProfile() *model.Profile {
	return &model.Profile{
		Version:       1,
		ServerVersion: "17",
		SchemaSQL: strings.Join([]string{
			"CREATE TABLE users (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, email text, name text);",
			"CREATE TABLE orders (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, user_id bigint, total integer, status text, created_at timestamptz DEFAULT now());",
			"CREATE INDEX orders_user_idx ON orders (user_id);",
		}, "\n"),
		Tables: []model.TableProfile{
			{Schema: "public", Name: "users", RowEstimate: 200, Columns: []model.ColumnProfile{
				{Name: "id", DataType: "bigint"},
				{Name: "email", DataType: "text", Sensitive: true},
				{Name: "name", DataType: "text", Sensitive: true},
			}},
			{Schema: "public", Name: "orders", RowEstimate: 1000, Columns: []model.ColumnProfile{
				{Name: "id", DataType: "bigint"},
				{Name: "user_id", DataType: "bigint", Nullable: true},
				{Name: "total", DataType: "integer", Nullable: true},
				{Name: "status", DataType: "text", Nullable: true, NDistinct: 3, MostCommonVals: []string{"paid", "pending", "shipped"}, MostCommonFreqs: []float64{0.5, 0.3, 0.2}},
				{Name: "created_at", DataType: "timestamp with time zone", Nullable: true},
			}},
		},
		Queries: []model.QueryProfile{
			{QueryID: 1, Query: "SELECT id, total, status FROM orders WHERE id = $1", CallsPerSec: 50, MeanMs: 1, Tables: []string{"public.orders"}, Feature: "view order"},
			{QueryID: 2, Query: "SELECT id, email FROM users WHERE id = $1", CallsPerSec: 30, MeanMs: 1, Tables: []string{"public.users"}, Feature: "user lookup"},
		},
	}
}

func dropTemplate(t *testing.T, dsn, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
}

func TestRunIntegration(t *testing.T) {
	dsn := os.Getenv("PGR_TEST_DSN")
	if dsn == "" {
		t.Skip("PGR_TEST_DSN not set")
	}
	prof := testProfile()
	base := Options{
		Profile:        prof,
		ShadowDSN:      dsn,
		Scale:          1,
		Seed:           7,
		Warmup:         2 * time.Second,
		Observe:        2 * time.Second,
		RateMultiplier: 0.3,
		MaxConns:       8,
		TestFixes:      true,
		Rebuild:        true,
	}
	t.Cleanup(func() { dropTemplate(t, dsn, templateName(base)) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 1. A safe ADD COLUMN: no danger lint, small table, no long txn -> safe, no fix.
	safe := base
	safe.MigrationSQL = "ALTER TABLE orders ADD COLUMN gift_message text;"
	rep, err := Run(ctx, safe)
	if err != nil {
		t.Fatalf("Run(safe): %v", err)
	}
	if rep.Original.Verdict != model.VerdictSafe {
		t.Errorf("safe migration verdict = %q, want safe", rep.Original.Verdict)
	}
	if locked := lockNames(rep.Original.Footprint); !contains(locked, "public.orders") {
		t.Errorf("footprint should lock public.orders, got %v", locked)
	}
	if len(rep.Fixes) != 0 || rep.FixedResult != nil {
		t.Errorf("safe migration should need no fix, got %d fixes / fixed=%v", len(rep.Fixes), rep.FixedResult != nil)
	}

	// 2. CREATE INDEX (danger lint) -> a fix is suggested and re-tested; the
	// rewrite builds the index CONCURRENTLY.
	idx := base
	idx.Rebuild = false // reuse the template the first run built
	idx.MigrationSQL = "CREATE INDEX orders_status_idx ON orders (status);"
	rep, err = Run(ctx, idx)
	if err != nil {
		t.Fatalf("Run(create index): %v", err)
	}
	if len(rep.Fixes) == 0 {
		t.Fatal("create index should produce a fix")
	}
	if rep.FixedResult == nil {
		t.Fatal("create index fix should be re-tested")
	}
	joined := ""
	for _, f := range rep.Fixes {
		joined += f.Rewritten
	}
	if !strings.Contains(strings.ToUpper(joined), "CONCURRENTLY") {
		t.Errorf("fix should use CONCURRENTLY, got: %s", joined)
	}
}

func lockNames(fp model.LockFootprint) []string {
	var out []string
	for _, s := range fp.Statements {
		for _, l := range s.Locks {
			out = append(out, l.Relation)
		}
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
