package profile

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

func TestIsSensitive(t *testing.T) {
	yes := []struct{ table, col string }{
		{"users", "email"}, {"users", "email_address"}, {"users", "EmailAddress"},
		{"users", "phone"}, {"users", "phone_number"}, {"users", "mobile"},
		{"users", "name"}, {"users", "first_name"}, {"users", "lastName"}, {"users", "full_name"},
		{"users", "username"}, {"users", "address"}, {"users", "street"}, {"users", "billing_street_2"},
		{"users", "ssn"}, {"users", "password"}, {"users", "password_hash"}, {"sessions", "token"},
		{"sessions", "refresh_token"}, {"apps", "client_secret"}, {"logins", "ip"}, {"logins", "last_ip"},
		{"logins", "ip_address"}, {"logins", "remoteIP"}, {"users", "dob"}, {"users", "birth_date"},
		{"users", "date_of_birth"}, {"payments", "card_number"}, {"payments", "card"}, {"payments", "iban"},
		{"users", "zip"}, {"users", "postal_code"}, {"apps", "api_key"},
	}
	for _, c := range yes {
		if !IsSensitive(c.table, c.col, nil) {
			t.Errorf("IsSensitive(%q,%q) = false, want true", c.table, c.col)
		}
	}
	no := []struct{ table, col string }{
		{"orders", "id"}, {"orders", "user_id"}, {"orders", "status"}, {"orders", "created_at"},
		{"orders", "total_cents"}, {"orders", "shipped"}, {"order_items", "quantity"},
		{"products", "sku"}, {"orders", "tip_amount"}, {"orders", "discarded"},
	}
	for _, c := range no {
		if IsSensitive(c.table, c.col, nil) {
			t.Errorf("IsSensitive(%q,%q) = true, want false", c.table, c.col)
		}
	}
	extra := []string{"notes", "orders.coupon", "public.orders.status", "audit.*"}
	if !IsSensitive("orders", "notes", extra) || !IsSensitive("products", "NOTES", extra) {
		t.Error("bare extra column not matched")
	}
	if !IsSensitive("orders", "coupon", extra) || !IsSensitive("public.orders", "coupon", extra) {
		t.Error("table.column extra not matched")
	}
	if IsSensitive("carts", "coupon", extra) {
		t.Error("table.column extra matched wrong table")
	}
	if !IsSensitive("public.orders", "status", extra) || IsSensitive("other.orders", "status", extra) {
		t.Error("schema.table.column extra mismatch")
	}
	if !IsSensitive("public.audit", "anything", extra) {
		t.Error("table.* extra not matched")
	}
}

func TestApplyPrivacy(t *testing.T) {
	tp := model.TableProfile{Schema: "public", Name: "users", Columns: []model.ColumnProfile{
		{Name: "email", DataType: "text", AvgWidth: 20, NDistinct: -1, MostCommonVals: []string{"a@x"}, MostCommonFreqs: []float64{.1}, HistogramBounds: []string{"a", "b"}},
		{Name: "status", DataType: "text", AvgWidth: 6, NDistinct: 3, MostCommonVals: []string{"new"}, MostCommonFreqs: []float64{.5}, HistogramBounds: []string{"a", "b"}},
		{Name: "bio", DataType: "text", AvgWidth: 300, MostCommonVals: []string{"hi"}, MostCommonFreqs: []float64{.5}, HistogramBounds: []string{"x"}},
		{Name: "region", DataType: "text", AvgWidth: 4, MostCommonVals: []string{"eu"}, MostCommonFreqs: []float64{.5}},
	}}
	applyPrivacy(&tp, []string{"users.region"})
	c := tp.Columns
	if !c[0].Sensitive || c[0].MostCommonVals != nil || c[0].MostCommonFreqs != nil || c[0].HistogramBounds != nil {
		t.Errorf("email not stripped: %+v", c[0])
	}
	if c[0].NDistinct != -1 || c[0].AvgWidth != 20 {
		t.Errorf("email aggregates lost: %+v", c[0])
	}
	if c[1].Sensitive || len(c[1].MostCommonVals) != 1 || len(c[1].HistogramBounds) != 2 {
		t.Errorf("status wrongly stripped: %+v", c[1])
	}
	if c[2].Sensitive || c[2].MostCommonVals != nil || c[2].HistogramBounds != nil {
		t.Errorf("wide free-text not stripped: %+v", c[2])
	}
	if !c[3].Sensitive || c[3].MostCommonVals != nil {
		t.Errorf("configured column not stripped: %+v", c[3])
	}
}

func TestParseArrayLiteral(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`{}`, []string{}},
		{`{1,2,3}`, []string{"1", "2", "3"}},
		{`{a,"b c","d,e","f\"g","h\\i",NULL}`, []string{"a", "b c", "d,e", `f"g`, `h\i`, "NULL"}},
		{`{"2024-01-01 00:00:00+00","2024-06-01 12:30:00+00"}`, []string{"2024-01-01 00:00:00+00", "2024-06-01 12:30:00+00"}},
		{`{"{1,2}","{3}"}`, []string{"{1,2}", "{3}"}},
		{`{{1,2},{3,4}}`, []string{"{1,2}", "{3,4}"}},
		{`[0:1]={x,y}`, []string{"x", "y"}},
		{`{""," lead"}`, []string{"", " lead"}},
		{`{t,f}`, []string{"t", "f"}},
	}
	for _, c := range cases {
		got, err := ParseArrayLiteral(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q want %q", c.in, got, c.want)
		}
	}
	for _, bad := range []string{``, `abc`, `{"unterminated}`, `{{1,2}`} {
		if _, err := ParseArrayLiteral(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
	f, err := parseFloatArray(`{0.5,0.25,1e-05}`)
	if err != nil || !reflect.DeepEqual(f, []float64{0.5, 0.25, 1e-05}) {
		t.Errorf("parseFloatArray: %v %v", f, err)
	}
}

func TestExtractRelations(t *testing.T) {
	cases := []struct {
		q    string
		want []string
	}{
		{"SELECT * FROM users WHERE id = $1", []string{"users"}},
		{"SELECT o.id FROM orders o JOIN public.users u ON u.id = o.user_id LEFT JOIN order_items oi ON oi.order_id = o.id", []string{"orders", "public.users", "order_items"}},
		{"SELECT * FROM a, b AS bb, ONLY c WHERE a.x = b.x", []string{"a", "b", "c"}},
		{"INSERT INTO orders (user_id, total) VALUES ($1, $2) RETURNING id", []string{"orders"}},
		{"UPDATE ONLY users SET name = $1 FROM orders WHERE orders.user_id = users.id", []string{"users", "orders"}},
		{"DELETE FROM sessions USING users WHERE sessions.user_id = users.id", []string{"sessions", "users"}},
		{`SELECT * FROM "MySchema"."Orders"`, []string{"MySchema.Orders"}},
		{"SELECT * FROM generate_series($1, $2) g", nil},
		{"SELECT * FROM (SELECT id FROM users) s", []string{"users"}},
		{"SELECT 'FROM fake' AS x FROM real_t -- FROM commented\n", []string{"real_t"}},
		{"SELECT extract(epoch FROM created_at) FROM orders", []string{"created_at", "orders"}},
	}
	for _, c := range cases {
		got := ExtractRelations(c.q)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.q, got, c.want)
		}
	}
	r := newRelationResolver([]string{"public.users", "public.orders", "billing.orders", "billing.invoices", "MySchema.Orders"})
	got := r.resolve("SELECT extract(epoch FROM created_at) FROM orders JOIN invoices ON true JOIN billing.orders bo ON true JOIN nope ON true")
	want := []string{"billing.invoices", "billing.orders", "public.orders"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("resolve: got %q want %q", got, want)
	}
	if got := r.resolve(`SELECT 1 FROM "MySchema"."Orders"`); !reflect.DeepEqual(got, []string{"MySchema.Orders"}) {
		t.Errorf("resolve quoted: %q", got)
	}
	if got := unquoteQualified(`"MySchema"."Orders"`); got != "MySchema.Orders" {
		t.Errorf("unquoteQualified: %q", got)
	}
}

func TestIsApplicationQuery(t *testing.T) {
	keep := []string{
		"SELECT * FROM users WHERE id = $1",
		"  (SELECT 1)",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"INSERT INTO orders (a) VALUES ($1)",
		"UPDATE users SET x = $1",
		"DELETE FROM sessions WHERE id = $1",
		"SELECT pg_sleep($1)",
	}
	drop := []string{
		"BEGIN", "COMMIT", "SET search_path = public", "SHOW server_version", "VACUUM users",
		"CREATE INDEX i ON t (x)", "ANALYZE", "SELECT * FROM pg_stat_activity",
		"SELECT relname FROM pg_catalog.pg_class", "SELECT * FROM information_schema.tables",
		"/* pg-rehearse:profile */ SELECT 1", "SELECT * FROM pg_stat_statements",
		"select n.nspname from pg_namespace n",
	}
	for _, q := range keep {
		if !isApplicationQuery(q) {
			t.Errorf("dropped %q", q)
		}
	}
	for _, q := range drop {
		if isApplicationQuery(q) {
			t.Errorf("kept %q", q)
		}
	}
}

func TestDiffSnapshots(t *testing.T) {
	t0 := time.Now()
	a := &stmtSnapshot{at: t0, stats: map[int64]stmtCounter{
		1: {query: "SELECT * FROM orders WHERE id = $1", calls: 100, totalMs: 50},
		2: {query: "SELECT * FROM users WHERE id = $1", calls: 10, totalMs: 10},
		3: {query: "BEGIN", calls: 5, totalMs: 0},
		5: {query: "SELECT * FROM users WHERE email = $1", calls: 1000, totalMs: 1000},
	}}
	b := &stmtSnapshot{at: t0.Add(2 * time.Second), stats: map[int64]stmtCounter{
		1: {query: "SELECT * FROM orders WHERE id = $1", calls: 300, totalMs: 250},
		2: {query: "SELECT * FROM users WHERE id = $1", calls: 10, totalMs: 10},   // idle
		3: {query: "BEGIN", calls: 50, totalMs: 1},                                // utility
		4: {query: "UPDATE users SET x = $1 WHERE id = $2", calls: 4, totalMs: 8}, // new
		5: {query: "SELECT * FROM users WHERE email = $1", calls: 6, totalMs: 3},  // reset
	}}
	rules, err := compileFeatureMap(map[string]string{"orders": "checkout", "^update": "profile-edit"})
	if err != nil {
		t.Fatal(err)
	}
	res := newRelationResolver([]string{"public.orders", "public.users"})
	got := diffSnapshots(a, b, 10, rules, res)
	if len(got) != 3 {
		t.Fatalf("got %d queries: %+v", len(got), got)
	}
	if got[0].QueryID != 1 || got[0].CallsPerSec != 100 || got[0].MeanMs != 1 || got[0].Feature != "checkout" ||
		!reflect.DeepEqual(got[0].Tables, []string{"public.orders"}) {
		t.Errorf("q1: %+v", got[0])
	}
	if got[1].QueryID != 5 || got[1].CallsPerSec != 3 || got[1].MeanMs != 0.5 {
		t.Errorf("q5 (reset): %+v", got[1])
	}
	if got[2].QueryID != 4 || got[2].CallsPerSec != 2 || got[2].MeanMs != 2 || got[2].Feature != "profile-edit" {
		t.Errorf("q4: %+v", got[2])
	}
	if top := diffSnapshots(a, b, 1, rules, res); len(top) != 1 || top[0].QueryID != 1 {
		t.Errorf("topN: %+v", top)
	}
	if _, err := compileFeatureMap(map[string]string{"(": "x"}); err == nil {
		t.Error("expected regex error")
	}
}

func TestLongTxnSummarize(t *testing.T) {
	o := newLongTxnObserver(2)
	t0 := time.Now()
	o.seen[txnKey{1, t0}] = &txnObs{query: "SELECT count(*) FROM orders WHERE created_at > '2024-01-01'", maxAge: 8, tables: map[string]bool{"public.orders": true}}
	o.seen[txnKey{2, t0}] = &txnObs{query: "SELECT count(*) FROM orders WHERE created_at > '2024-02-01'", maxAge: 4, tables: map[string]bool{}}
	o.seen[txnKey{3, t0}] = &txnObs{query: "SELECT pg_sleep(30)", maxAge: 3, tables: map[string]bool{"public.users": true}}
	res := newRelationResolver([]string{"public.orders", "public.users"})
	got := o.summarize(10, res)
	if len(got) != 2 {
		t.Fatalf("groups: %+v", got)
	}
	g := got[0]
	// N=2, W=10, D=8, M=2 -> 3600*2/16 = 450
	if g.DurationSec != 8 || g.PerHour != 450 || !reflect.DeepEqual(g.Tables, []string{"public.orders"}) {
		t.Errorf("report group: %+v", g)
	}
	// N=1, D=3: denom = 10+3-2 = 11
	if got[1].PerHour != 3600.0/11 || !reflect.DeepEqual(got[1].Tables, []string{"public.users"}) {
		t.Errorf("sleep group: %+v", got[1])
	}
}

func TestSaveLoad(t *testing.T) {
	p := &model.Profile{Version: 1, CapturedAt: time.Now().UTC().Truncate(time.Second), ServerVersion: "17.2",
		SchemaSQL: "CREATE TABLE public.t (id bigint);",
		Tables:    []model.TableProfile{{Schema: "public", Name: "t", RowEstimate: 5, Columns: []model.ColumnProfile{{Name: "id", DataType: "bigint", MostCommonVals: []string{"1"}, MostCommonFreqs: []float64{1}}}}},
		Queries:   []model.QueryProfile{{QueryID: 7, Query: "SELECT 1", CallsPerSec: 2.5, Tables: []string{}}},
		LongTxns:  []model.LongTxn{{Query: "x", DurationSec: 3, PerHour: 1, Tables: []string{"public.t"}}},
	}
	path := filepath.Join(t.TempDir(), "p.json")
	if err := Save(path, p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Errorf("round trip mismatch:\n%+v\n%+v", got, p)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("expected error")
	}
}

func TestTokenize(t *testing.T) {
	got := strings.Join(tokenize("billingIPAddr_v2"), " ")
	if got != "billing ip addr v2" {
		t.Errorf("tokenize: %q", got)
	}
}
