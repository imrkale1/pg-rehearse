package load

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// Run with e.g. PGR_TEST_DSN=postgres://postgres:pg@localhost:55434/postgres?sslmode=disable

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PGR_TEST_DSN")
	if dsn == "" {
		t.Skip("PGR_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	_, err = pool.Exec(ctx, `
		DROP TABLE IF EXISTS orders, users;
		CREATE TABLE users (
			id bigserial PRIMARY KEY,
			email text NOT NULL UNIQUE,
			name text NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now());
		CREATE TABLE orders (
			id bigserial PRIMARY KEY,
			user_id bigint NOT NULL REFERENCES users(id),
			status text NOT NULL,
			total numeric(10,2) NOT NULL,
			created_at timestamptz NOT NULL DEFAULT now());
		INSERT INTO users (email, name, created_at)
			SELECT 'user' || g || '@example.com', 'User ' || g, now() - g * interval '1 minute'
			FROM generate_series(1, 10000) g;
		INSERT INTO orders (user_id, status, total, created_at)
			SELECT 1 + (g * 7919) % 10000, (ARRAY['pending','paid','shipped','cancelled'])[1 + g % 4],
			       (g % 50000) / 100.0, now() - g * interval '1 second'
			FROM generate_series(1, 50000) g;
		CREATE INDEX orders_user_id_idx ON orders(user_id);
		ANALYZE users; ANALYZE orders;`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

const (
	qUserByID = iota + 1
	qInsertOrder
	qUpdateStatus
	qUserOrders
	qOrdersIn
	qUnsupported
)

func testQueries() []model.QueryProfile {
	return []model.QueryProfile{
		{QueryID: qUserByID, Query: "SELECT id, email, name FROM users WHERE id = $1", CallsPerSec: 100, Feature: "profile"},
		{QueryID: qInsertOrder, Query: "INSERT INTO orders (user_id, status, total, created_at) VALUES ($1, $2, $3, $4)", CallsPerSec: 50, Feature: "checkout"},
		{QueryID: qUpdateStatus, Query: "UPDATE orders SET status = $1 WHERE id = $2", CallsPerSec: 50, Feature: "fulfilment"},
		{QueryID: qUserOrders, Query: "SELECT o.id, o.total, u.email FROM orders o JOIN users u ON u.id = o.user_id WHERE o.user_id = $1 ORDER BY o.created_at DESC LIMIT $2", CallsPerSec: 50, Feature: "order-history"},
		{QueryID: qOrdersIn, Query: "SELECT id, status FROM orders WHERE id IN ($1, $2, $3)", CallsPerSec: 50, Feature: "checkout"},
		{QueryID: qUnsupported, Query: "SELECT id FROM users WHERE lower(email) = $1", CallsPerSec: 10},
	}
}

func TestIntegrationSteadyLoad(t *testing.T) {
	pool := testPool(t)
	r, err := NewRunner(pool, Config{Queries: testQueries(), RateMultiplier: 1, MaxConns: 20})
	if err != nil {
		t.Fatal(err)
	}
	sk := r.Skipped()
	if len(sk) != 1 || sk[0].QueryID != qUnsupported {
		t.Fatalf("skipped = %+v", sk)
	}
	t.Logf("skipped: %s", sk[0].Reason)

	var before int64
	pool.QueryRow(context.Background(), "SELECT count(*) FROM orders").Scan(&before)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dur := res.End.Sub(res.Start).Seconds()
	rate := float64(len(res.Samples)) / dur
	t.Logf("achieved %.1f req/s over %.2fs (%d samples)", rate, dur, len(res.Samples))
	if rate < 300*0.85 || rate > 300*1.15 {
		t.Errorf("achieved rate %.1f not within 15%% of 300", rate)
	}
	errs := 0
	for _, st := range res.PerQuery {
		t.Logf("q%d %-14s n=%4d err=%d p50=%.2fms p95=%.2fms p99=%.2fms max=%.2fms",
			st.QueryID, st.Feature, st.Count, st.Errors, st.P50Ms, st.P95Ms, st.P99Ms, st.MaxMs)
		errs += st.Errors
		if st.Feature == "" {
			t.Errorf("q%d lost its feature", st.QueryID)
		}
	}
	for _, s := range res.Samples {
		if s.Err != "" {
			t.Logf("sample error: %s", s.Err)
			break
		}
	}
	if float64(errs) > 0.01*float64(len(res.Samples)) {
		t.Errorf("%d errors out of %d", errs, len(res.Samples))
	}
	var after int64
	pool.QueryRow(context.Background(), "SELECT count(*) FROM orders").Scan(&after)
	if after <= before {
		t.Errorf("inserts did not land: %d -> %d", before, after)
	}
}

// TestIntegrationLockQueueOutage reproduces the classic outage: a long report
// transaction holds AccessShareLock on orders, an ALTER TABLE queues behind it
// for AccessExclusiveLock, and every orders query queues behind the ALTER.
func TestIntegrationLockQueueOutage(t *testing.T) {
	pool := testPool(t)
	cfg := Config{
		Queries:        testQueries()[:5],
		RateMultiplier: 1,
		MaxConns:       20,
		LongTxns: []model.LongTxn{{
			Query: "SELECT count(*) FROM orders", DurationSec: 3, Tables: []string{"public.orders"},
		}},
		LongTxnStartAfter: time.Second,
	}
	r, err := NewRunner(pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	alterDone := make(chan time.Duration, 1)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Error(err)
			alterDone <- 0
			return
		}
		defer conn.Release()
		t0 := time.Now()
		_, err = conn.Exec(context.Background(), "ALTER TABLE orders ADD COLUMN note text")
		if err != nil {
			t.Errorf("alter: %v", err)
		}
		alterDone <- time.Since(t0)
	}()

	res, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	alterTook := <-alterDone
	t.Logf("ALTER waited %v; long txns: %+v", alterTook, r.LongTxnResults())
	if lt := r.LongTxnResults(); len(lt) != 1 || lt[0].Err != "" || lt[0].Mode != "query" {
		t.Errorf("long txn result: %+v", lt)
	}

	thr := 200 * time.Millisecond
	for _, id := range []int64{qInsertOrder, qUpdateStatus, qUserOrders, qOrdersIn, qUserByID} {
		ws := StallWindows(res.Samples, id, thr)
		var parts []string
		for _, w := range ws {
			parts = append(parts, w.Start.Sub(res.Start).Round(time.Millisecond).String()+"+"+w.Duration().Round(time.Millisecond).String())
		}
		t.Logf("q%d stall windows (>%v): [%s] longest=%v", id, thr, strings.Join(parts, " "),
			LongestStall(res.Samples, id, thr).Round(time.Millisecond))
	}
	for _, id := range []int64{qInsertOrder, qUpdateStatus, qUserOrders, qOrdersIn} {
		if ls := LongestStall(res.Samples, id, thr); ls < 1500*time.Millisecond {
			t.Errorf("q%d: longest stall %v, want >= 1.5s", id, ls)
		}
	}
	for _, st := range res.PerQuery {
		t.Logf("q%d %-14s n=%4d err=%d p50=%.2fms p99=%.2fms max=%.2fms",
			st.QueryID, st.Feature, st.Count, st.Errors, st.P50Ms, st.P99Ms, st.MaxMs)
	}
}
