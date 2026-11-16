package load

import (
	"testing"
	"time"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

func TestParseQueryPatterns(t *testing.T) {
	type want struct {
		kind paramKind
		role paramRole
		qual string
		col  string
		pos  int
	}
	cases := []struct {
		sql  string
		want map[int]want
	}{
		{"SELECT id, email FROM users WHERE id = $1", map[int]want{1: {pkColumn, roleLookup, "", "id", 0}}},
		{"SELECT * FROM public.orders o WHERE o.user_id = $1 AND o.status IN ($2, $3) ORDER BY o.id LIMIT $4 OFFSET $5",
			map[int]want{1: {pkColumn, roleLookup, "o", "user_id", 0}, 2: {pkColumn, roleLookup, "o", "status", 0},
				3: {pkColumn, roleLookup, "o", "status", 0}, 4: {kind: pkLimit}, 5: {kind: pkOffset}}},
		{"INSERT INTO orders (user_id, status, total) VALUES ($1, $2, $3) RETURNING id",
			map[int]want{1: {pkColumn, roleInsert, "orders", "user_id", 0}, 2: {pkColumn, roleInsert, "orders", "status", 0},
				3: {pkColumn, roleInsert, "orders", "total", 0}}},
		{"INSERT INTO orders VALUES ($1, $2)", map[int]want{1: {pkColumn, roleInsert, "", "", 1}, 2: {pkColumn, roleInsert, "", "", 2}}},
		{"UPDATE orders SET status = $1, total = $2 WHERE id = $3",
			map[int]want{1: {pkColumn, roleAssign, "", "status", 0}, 2: {pkColumn, roleAssign, "", "total", 0},
				3: {pkColumn, roleLookup, "", "id", 0}}},
		{"SELECT count(*) FROM orders WHERE created_at BETWEEN $1 AND $2 AND total > $3::numeric",
			map[int]want{1: {pkColumn, roleLookup, "", "created_at", 0}, 2: {pkColumn, roleLookup, "", "created_at", 0},
				3: {pkColumn, roleLookup, "", "total", 0}}},
		{"SELECT 1 FROM users u WHERE $1 = u.id", map[int]want{1: {pkColumn, roleLookup, "u", "id", 0}}},
		{"SELECT 1 FROM users WHERE lower(email) = $1", map[int]want{}},
		{`SELECT "Name" FROM "Users" WHERE "Id" = $1 -- comment $9
		  /* $8 */`, map[int]want{1: {pkColumn, roleLookup, "", "Id", 0}}},
	}
	for _, c := range cases {
		pq, err := parseQuery(c.sql)
		if err != nil {
			t.Fatalf("%s: %v", c.sql, err)
		}
		if len(pq.Params) != len(c.want) {
			t.Errorf("%s: got %d params %+v, want %d", c.sql, len(pq.Params), pq.Params, len(c.want))
		}
		for i, w := range c.want {
			p := pq.Params[i]
			if p.Kind != w.kind || (w.kind == pkColumn && (p.Role != w.role || p.Qualifier != w.qual || p.Column != w.col || p.InsertPos != w.pos)) {
				t.Errorf("%s: $%d = %+v, want %+v", c.sql, i, p, w)
			}
		}
	}
	pq, _ := parseQuery("SELECT o.id FROM orders o, items i JOIN users AS u ON u.id = o.user_id WHERE i.x = $1")
	names := map[string]string{}
	for _, tr := range pq.Tables {
		names[tr.Name] = tr.Alias
	}
	if names["orders"] != "o" || names["users"] != "u" || names["items"] != "i" {
		t.Errorf("table aliases = %v", names)
	}
}

func TestStallWindows(t *testing.T) {
	t0 := time.Unix(1000, 0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }
	d := func(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }
	samples := []model.Sample{
		{QueryID: 1, Start: at(0), Latency: d(5)},
		{QueryID: 1, Start: at(1600), Latency: d(2400)}, // blocked until 4000
		{QueryID: 1, Start: at(1500), Latency: d(2500)}, // unsorted, blocked until 4000
		{QueryID: 1, Start: at(3000), Latency: d(1001)}, // inside, extends to 4001
		{QueryID: 2, Start: at(100), Latency: d(9000)},  // other query
		{QueryID: 1, Start: at(4002), Latency: d(3)},    // fast
		{QueryID: 1, Start: at(6000), Latency: d(300)},  // second window
		{QueryID: 1, Start: at(6300), Latency: d(200)},  // touching -> merged
		{QueryID: 1, Start: at(7000), Latency: d(100)},  // == threshold, not counted
	}
	got := StallWindows(samples, 1, d(100))
	want := []Window{{at(1500), at(4001)}, {at(6000), at(6500)}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) {
			t.Errorf("window %d = %v, want %v", i, got[i], want[i])
		}
	}
	if ls := LongestStall(samples, 1, d(100)); ls != d(2501) {
		t.Errorf("LongestStall = %v", ls)
	}
	if w := StallWindows(samples, 3, d(1)); w != nil {
		t.Errorf("unknown query: %v", w)
	}
}

func TestComputeStats(t *testing.T) {
	var s []model.Sample
	for i := 1; i <= 100; i++ {
		s = append(s, model.Sample{QueryID: 7, Latency: time.Duration(i) * time.Millisecond})
	}
	s[0].Err = "boom"
	st, _ := computeStats(model.QueryProfile{QueryID: 7, Feature: "checkout"}, s, nil)
	if st.Count != 100 || st.Errors != 1 || st.P50Ms != 50 || st.P95Ms != 95 || st.P99Ms != 99 || st.MaxMs != 100 || st.Feature != "checkout" {
		t.Errorf("stats = %+v", st)
	}
}
