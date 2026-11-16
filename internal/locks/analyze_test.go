package locks

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

const testSchema = `
CREATE TABLE users (id bigserial PRIMARY KEY, email text NOT NULL, name text);
CREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint, total integer, discount numeric(10,2), status text, created_at timestamptz DEFAULT now());
CREATE TABLE order_items (id bigserial PRIMARY KEY, order_id bigint, sku text, qty int);
CREATE INDEX orders_status_idx ON orders (status);
INSERT INTO users (email, name) SELECT 'u' || g || '@example.test', 'user ' || g FROM generate_series(1, 200) g;
INSERT INTO orders (user_id, total, status) SELECT 1 + g % 200, g, 'paid' FROM generate_series(1, 2000) g;
INSERT INTO order_items (order_id, sku, qty) SELECT 1 + g % 2000, 'sku' || g, 1 FROM generate_series(1, 4000) g;
`

// testDB creates a fresh database with users/orders/order_items; skipped without PGR_TEST_DSN.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("PGR_TEST_DSN")
	if dsn == "" {
		t.Skip("PGR_TEST_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("pgr_locks_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})
	for _, s := range SplitStatements(testSchema) {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("schema: %s: %v", s, err)
		}
	}
	return pool
}

func findLock(sl model.StatementLocks, rel, mode string) *model.RelationLock {
	for i, l := range sl.Locks {
		if l.Relation == rel && l.Mode == mode {
			return &sl.Locks[i]
		}
	}
	return nil
}

func hasRule(fs []model.LintFinding, rule string) bool {
	for _, f := range fs {
		if f.Rule == rule {
			return true
		}
	}
	return false
}

func analyze(t *testing.T, pool *pgxpool.Pool, sql string) *model.LockFootprint {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fp, err := Analyze(ctx, pool, sql)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	return fp
}

func TestAnalyzeAddForeignKey(t *testing.T) {
	pool := testDB(t)
	fp := analyze(t, pool, "ALTER TABLE orders ADD FOREIGN KEY (user_id) REFERENCES users(id);")
	if len(fp.Statements) != 1 {
		t.Fatalf("statements: %+v", fp.Statements)
	}
	sl := fp.Statements[0]
	for _, rel := range []string{"public.orders", "public.users"} {
		l := findLock(sl, rel, ShareRowExclusive)
		if l == nil {
			t.Fatalf("no ShareRowExclusiveLock on %s: %+v", rel, sl.Locks)
		}
		if l.Kind != "table" || !l.BlocksWrites || l.BlocksReads {
			t.Errorf("%s lock flags wrong: %+v", rel, *l)
		}
	}
	if len(fp.RewrittenTables) != 0 {
		t.Errorf("unexpected rewrite: %v", fp.RewrittenTables)
	}
	if sl.DurationMs <= 0 {
		t.Errorf("DurationMs not recorded: %v", sl.DurationMs)
	}
	if !hasRule(fp.Lint, RuleAddFKWithoutNotValid) || !hasRule(fp.Lint, RuleMissingLockTimeout) {
		t.Errorf("lint not filled: %+v", fp.Lint)
	}
	// rolled back
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_constraint WHERE contype = 'f'").Scan(&n); err != nil || n != 0 {
		t.Errorf("FK survived rollback: n=%d err=%v", n, err)
	}
}

func TestAnalyzeAlterColumnTypeRewrites(t *testing.T) {
	pool := testDB(t)
	fp := analyze(t, pool, "ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2)")
	l := findLock(fp.Statements[0], "public.orders", AccessExclusive)
	if l == nil || !l.BlocksReads || !l.BlocksWrites {
		t.Fatalf("want AccessExclusiveLock on orders blocking reads+writes: %+v", fp.Statements[0].Locks)
	}
	if len(fp.RewrittenTables) != 1 || fp.RewrittenTables[0] != "public.orders" {
		t.Errorf("RewrittenTables = %v", fp.RewrittenTables)
	}
	if hasRule(fp.Lint, RuleTableRewrite) {
		t.Errorf("table-rewrite should be covered by alter-column-type: %+v", fp.Lint)
	}
}

// Widening numeric precision is binary compatible: AccessExclusive but no rewrite.
func TestAnalyzeAlterColumnTypeNoRewrite(t *testing.T) {
	pool := testDB(t)
	fp := analyze(t, pool, "ALTER TABLE orders ALTER COLUMN discount TYPE numeric(14,2)")
	if findLock(fp.Statements[0], "public.orders", AccessExclusive) == nil {
		t.Fatalf("want AccessExclusiveLock: %+v", fp.Statements[0].Locks)
	}
	if len(fp.RewrittenTables) != 0 {
		t.Errorf("unexpected rewrite: %v", fp.RewrittenTables)
	}
}

func TestAnalyzeAddColumnNoRewrite(t *testing.T) {
	pool := testDB(t)
	fp := analyze(t, pool, "ALTER TABLE orders ADD COLUMN x int")
	if findLock(fp.Statements[0], "public.orders", AccessExclusive) == nil {
		t.Fatalf("want AccessExclusiveLock: %+v", fp.Statements[0].Locks)
	}
	if len(fp.RewrittenTables) != 0 {
		t.Errorf("unexpected rewrite: %v", fp.RewrittenTables)
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM information_schema.columns WHERE table_name='orders' AND column_name='x'").Scan(&n); err != nil || n != 0 {
		t.Errorf("column survived rollback: n=%d err=%v", n, err)
	}
}

func TestAnalyzeCumulativeDeltasAndStatic(t *testing.T) {
	pool := testDB(t)
	fp := analyze(t, pool, `
BEGIN;
SET lock_timeout = '2s';
ALTER TABLE orders ADD COLUMN note text;          -- 2
ALTER TABLE orders ADD COLUMN note2 text;         -- 3: lock already held => no delta
CREATE UNIQUE INDEX CONCURRENTLY orders_note_idx ON orders (note);   -- 4: static, depends on stmt 2
ALTER TABLE orders ADD CONSTRAINT orders_note_key UNIQUE USING INDEX orders_note_idx; -- 5: depends on 4
CREATE TABLE brand_new (id int);
CREATE INDEX brand_new_idx ON brand_new (id);     -- 7: new table, no visible locks
ALTER TABLE no_such_table ADD COLUMN y int;       -- 8: fails, rest continues
VACUUM order_items;                               -- 9: static
VACUUM FULL users;                                -- 10: static, rewrite
CREATE INDEX items_sku_idx ON order_items (sku);  -- 11
COMMIT;`)
	if len(fp.Statements) != 13 {
		t.Fatalf("want 13 statements, got %d", len(fp.Statements))
	}
	s := fp.Statements
	if findLock(s[2], "public.orders", AccessExclusive) == nil {
		t.Errorf("stmt2: %+v", s[2].Locks)
	}
	if len(s[3].Locks) != 0 {
		t.Errorf("stmt3 should have no new locks: %+v", s[3].Locks)
	}
	if len(s[4].Locks) != 1 || s[4].Locks[0].Mode != ShareUpdateExclusive || s[4].Locks[0].Relation != "public.orders" || s[4].Locks[0].BlocksWrites {
		t.Errorf("stmt4 (concurrently): %+v", s[4].Locks)
	}
	if len(s[6].Locks) != 0 || len(s[7].Locks) != 0 {
		t.Errorf("new table locks leaked: %+v %+v", s[6].Locks, s[7].Locks)
	}
	if findLock(s[9], "public.order_items", ShareUpdateExclusive) == nil {
		t.Errorf("stmt9 vacuum: %+v", s[9].Locks)
	}
	if findLock(s[10], "public.users", AccessExclusive) == nil {
		t.Errorf("stmt10 vacuum full: %+v", s[10].Locks)
	}
	if l := findLock(s[11], "public.order_items", Share); l == nil || !l.BlocksWrites || l.BlocksReads {
		t.Errorf("stmt11 create index: %+v", s[11].Locks)
	}
	failed := 0
	for _, f := range fp.Lint {
		if f.Rule == RuleStatementFailed {
			failed++
			if f.Statement != s[8].SQL {
				t.Errorf("unexpected failed statement: %s: %s", f.Statement, f.Message)
			}
		}
	}
	if failed != 1 {
		t.Errorf("want exactly 1 statement-failed, got %d: %+v", failed, fp.Lint)
	}
	if len(fp.RewrittenTables) != 1 || fp.RewrittenTables[0] != "public.users" {
		t.Errorf("RewrittenTables = %v", fp.RewrittenTables)
	}
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_class WHERE relname IN ('brand_new','orders_note_idx','items_sku_idx')").Scan(&n); err != nil || n != 0 {
		t.Errorf("objects survived rollback: n=%d err=%v", n, err)
	}
	// the pool must still be usable and not inside a transaction
	if _, err := pool.Exec(context.Background(), "VACUUM order_items"); err != nil {
		t.Errorf("pool unusable after Analyze: %v", err)
	}
}

func TestAnalyzeEmpty(t *testing.T) {
	fp, err := Analyze(context.Background(), nil, " -- nothing\n")
	if err != nil || fp == nil || len(fp.Statements) != 0 {
		t.Fatalf("fp=%+v err=%v", fp, err)
	}
}
