package fixer

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-rehearse/pg-rehearse/internal/locks"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

const lt = "SET lock_timeout = '2s';\n"

// cases are shared by the unit test (exact output) and the integration test
// (every output executes on a real server, statement by statement).
var cases = []struct {
	name  string
	in    string
	rules []string // rules of the suggested fixes, in order
	want  string   // exact Apply output
}{
	{
		name:  "create index",
		in:    "CREATE INDEX orders_user_idx ON orders (user_id);",
		rules: []string{locks.RuleCreateIndexNotConcurrent},
		want:  "CREATE INDEX CONCURRENTLY orders_user_idx ON orders (user_id);\n",
	},
	{
		name:  "create unique index if not exists, quoted, in txn",
		in:    "BEGIN;\n-- speed up lookups\ncreate unique index if not exists \"Items Idx\" on \"public\".\"Line Items\" (\"Order Id\", sku);\nCOMMIT;",
		rules: []string{locks.RuleCreateIndexNotConcurrent},
		want:  "-- speed up lookups\ncreate unique index CONCURRENTLY if not exists \"Items Idx\" on \"public\".\"Line Items\" (\"Order Id\", sku);\n",
	},
	{
		name:  "foreign key named",
		in:    "ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE;",
		rules: []string{locks.RuleMissingLockTimeout, locks.RuleAddFKWithoutNotValid},
		want: lt + "ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE NOT VALID;\n" +
			"ALTER TABLE orders VALIDATE CONSTRAINT orders_user_fk;\n",
	},
	{
		name:  "foreign key unnamed, quoted",
		in:    `alter table "public"."Line Items" add foreign key ("Order Id") references public.orders(id)`,
		rules: []string{locks.RuleMissingLockTimeout, locks.RuleAddFKWithoutNotValid},
		want: lt + `alter table "public"."Line Items" ADD CONSTRAINT "Line Items_Order Id_fkey" foreign key ("Order Id") references public.orders(id) NOT VALID;` + "\n" +
			`alter table "public"."Line Items" VALIDATE CONSTRAINT "Line Items_Order Id_fkey";` + "\n",
	},
	{
		name:  "check named",
		in:    "ALTER TABLE orders ADD CONSTRAINT total_positive CHECK (total >= 0);",
		rules: []string{locks.RuleMissingLockTimeout, locks.RuleAddCheckWithoutNotValid},
		want: lt + "ALTER TABLE orders ADD CONSTRAINT total_positive CHECK (total >= 0) NOT VALID;\n" +
			"ALTER TABLE orders VALIDATE CONSTRAINT total_positive;\n",
	},
	{
		name:  "set not null",
		in:    "SET lock_timeout = '5s';\nALTER TABLE orders ALTER COLUMN user_id SET NOT NULL;",
		rules: []string{locks.RuleSetNotNull},
		want: "SET lock_timeout = '5s';\n" +
			"ALTER TABLE orders ADD CONSTRAINT orders_user_id_not_null_chk CHECK (user_id IS NOT NULL) NOT VALID;\n" +
			"ALTER TABLE orders VALIDATE CONSTRAINT orders_user_id_not_null_chk;\n" +
			"ALTER TABLE orders ALTER COLUMN user_id SET NOT NULL;\n" +
			"ALTER TABLE orders DROP CONSTRAINT orders_user_id_not_null_chk;\n",
	},
	{
		name:  "set not null quoted",
		in:    `ALTER TABLE "Line Items" ALTER "Order Id" SET NOT NULL`,
		rules: []string{locks.RuleMissingLockTimeout, locks.RuleSetNotNull},
		want: lt + `ALTER TABLE "Line Items" ADD CONSTRAINT "Line Items_Order Id_not_null_chk" CHECK ("Order Id" IS NOT NULL) NOT VALID;` + "\n" +
			`ALTER TABLE "Line Items" VALIDATE CONSTRAINT "Line Items_Order Id_not_null_chk";` + "\n" +
			`ALTER TABLE "Line Items" ALTER "Order Id" SET NOT NULL;` + "\n" +
			`ALTER TABLE "Line Items" DROP CONSTRAINT "Line Items_Order Id_not_null_chk";` + "\n",
	},
	{
		name:  "add unique named",
		in:    "ALTER TABLE users ADD CONSTRAINT users_email_uq UNIQUE (email);",
		rules: []string{locks.RuleMissingLockTimeout, locks.RuleAddUniqueConstraint},
		want: lt + "CREATE UNIQUE INDEX CONCURRENTLY users_email_uq ON users (email);\n" +
			"ALTER TABLE users ADD CONSTRAINT users_email_uq UNIQUE USING INDEX users_email_uq;\n",
	},
	{
		name:  "add unique unnamed with options",
		in:    "ALTER TABLE public.users ADD UNIQUE NULLS NOT DISTINCT (email, name) INCLUDE (id) DEFERRABLE INITIALLY DEFERRED;",
		rules: []string{locks.RuleMissingLockTimeout, locks.RuleAddUniqueConstraint},
		want: lt + "CREATE UNIQUE INDEX CONCURRENTLY users_email_name_key ON public.users (email, name) INCLUDE (id) NULLS NOT DISTINCT;\n" +
			"ALTER TABLE public.users ADD CONSTRAINT users_email_name_key UNIQUE USING INDEX users_email_name_key DEFERRABLE INITIALLY DEFERRED;\n",
	},
	{
		name:  "add column volatile default",
		in:    "ALTER TABLE orders ADD COLUMN IF NOT EXISTS token uuid NOT NULL DEFAULT gen_random_uuid();",
		rules: []string{locks.RuleMissingLockTimeout, locks.RuleAddColumnVolatileDefault},
		want: lt + "ALTER TABLE orders ADD COLUMN IF NOT EXISTS token uuid;\n" +
			"ALTER TABLE orders ALTER COLUMN token SET DEFAULT gen_random_uuid();\n",
	},
	{
		name:  "alter column type: description only",
		in:    "ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2);",
		rules: []string{locks.RuleAlterColumnType},
		want:  lt + "ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2);\n",
	},
	{
		name:  "multi-action alter keeps order and safe actions",
		in:    "ALTER TABLE orders ADD COLUMN note text, ADD CONSTRAINT note_len CHECK (length(note) < 100), ALTER COLUMN status SET DEFAULT 'new', ALTER user_id SET NOT NULL;",
		rules: []string{locks.RuleMissingLockTimeout, locks.RuleAddCheckWithoutNotValid},
		want: lt + "ALTER TABLE orders ADD COLUMN note text;\n" +
			"ALTER TABLE orders ADD CONSTRAINT note_len CHECK (length(note) < 100) NOT VALID;\n" +
			"ALTER TABLE orders VALIDATE CONSTRAINT note_len;\n" +
			"ALTER TABLE orders ALTER COLUMN status SET DEFAULT 'new';\n" +
			"ALTER TABLE orders ADD CONSTRAINT orders_user_id_not_null_chk CHECK (user_id IS NOT NULL) NOT VALID;\n" +
			"ALTER TABLE orders VALIDATE CONSTRAINT orders_user_id_not_null_chk;\n" +
			"ALTER TABLE orders ALTER user_id SET NOT NULL;\n" +
			"ALTER TABLE orders DROP CONSTRAINT orders_user_id_not_null_chk;\n",
	},
	{
		name:  "drop index",
		in:    "DROP INDEX IF EXISTS orders_status_idx;",
		rules: []string{"drop-index-not-concurrent"},
		want:  "DROP INDEX CONCURRENTLY IF EXISTS orders_status_idx;\n",
	},
	{
		name: "whole migration in a transaction",
		in: "BEGIN;\nSET LOCAL lock_timeout = '3s';\n-- index for the dashboard\nCREATE INDEX orders_created_idx ON orders (created_at);\n" +
			"ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id);\n" +
			"ALTER TABLE orders ALTER COLUMN total TYPE bigint;\nALTER TABLE orders ADD COLUMN x int;\nCOMMIT;\n-- done\n",
		rules: []string{locks.RuleCreateIndexNotConcurrent, locks.RuleAddFKWithoutNotValid, locks.RuleAlterColumnType},
		want: "SET lock_timeout = '3s';\n-- index for the dashboard\nCREATE INDEX CONCURRENTLY orders_created_idx ON orders (created_at);\n" +
			"ALTER TABLE orders ADD CONSTRAINT orders_user_fk FOREIGN KEY (user_id) REFERENCES users (id) NOT VALID;\n" +
			"ALTER TABLE orders VALIDATE CONSTRAINT orders_user_fk;\n" +
			lt + "ALTER TABLE orders ALTER COLUMN total TYPE bigint;\nALTER TABLE orders ADD COLUMN x int;\n-- done\n",
	},
	{
		name:  "only lock_timeout missing",
		in:    "ALTER TABLE orders ADD COLUMN x int;\nALTER TABLE users RENAME COLUMN name TO full_name;",
		rules: []string{locks.RuleMissingLockTimeout},
		want:  lt + "ALTER TABLE orders ADD COLUMN x int;\nALTER TABLE users RENAME COLUMN name TO full_name;\n",
	},
	{
		name:  "new table in same migration is left alone",
		in:    "CREATE TABLE audit (id bigint, order_id bigint);\nCREATE INDEX audit_idx ON audit (order_id);\nALTER TABLE audit ADD CONSTRAINT a CHECK (id > 0), ALTER COLUMN id SET NOT NULL;",
		rules: nil,
		want:  "CREATE TABLE audit (id bigint, order_id bigint);\nCREATE INDEX audit_idx ON audit (order_id);\nALTER TABLE audit ADD CONSTRAINT a CHECK (id > 0), ALTER COLUMN id SET NOT NULL;",
	},
	{
		name:  "already safe",
		in:    lt + "CREATE INDEX CONCURRENTLY i ON orders (user_id);\nALTER TABLE orders ADD CONSTRAINT c CHECK (total >= 0) NOT VALID;\nALTER TABLE orders VALIDATE CONSTRAINT c;",
		rules: nil,
		want:  lt + "CREATE INDEX CONCURRENTLY i ON orders (user_id);\nALTER TABLE orders ADD CONSTRAINT c CHECK (total >= 0) NOT VALID;\nALTER TABLE orders VALIDATE CONSTRAINT c;",
	},
}

func ruleList(fs []model.Fix) string {
	var r []string
	for _, f := range fs {
		r = append(r, f.Rule)
	}
	return strings.Join(r, ",")
}

func TestSuggestApply(t *testing.T) {
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fixes := Suggest(c.in, nil)
			if got := ruleList(fixes); got != strings.Join(c.rules, ",") {
				t.Errorf("rules\n got: %s\nwant: %s", got, strings.Join(c.rules, ","))
			}
			for _, f := range fixes {
				if f.Description == "" || f.Rewritten == "" {
					t.Errorf("incomplete fix %+v", f)
				}
				if f.Rule != locks.RuleMissingLockTimeout && !strings.Contains(c.in, f.Original) {
					t.Errorf("Original is not a statement of the migration: %q", f.Original)
				}
			}
			got := Apply(c.in, fixes)
			if got != c.want {
				t.Errorf("Apply\n got:\n%s\nwant:\n%s", got, c.want)
			}
			// The result is stable: nothing left to fix, no lint danger we can rewrite.
			if again := Suggest(got, nil); len(again) != 0 {
				for _, f := range again {
					if f.Rule != locks.RuleAlterColumnType {
						t.Errorf("fixed migration still has a fix: %+v", f)
					}
				}
			}
			// Runs outside a transaction block: no txn control once CONCURRENTLY is present.
			if strings.Contains(strings.ToUpper(got), "CONCURRENTLY") {
				for _, s := range locks.SplitStatements(got) {
					if st := locks.Parse(s); st.Kind == locks.StmtTxnControl {
						t.Errorf("transaction control left in non-transactional migration: %q", s)
					}
				}
			}
		})
	}
}

func TestAlterTypeFix(t *testing.T) {
	in := "ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2)"
	fp := &model.LockFootprint{RewrittenTables: []string{"public.orders"}}
	fixes := Suggest(in, fp)
	if len(fixes) != 1 {
		t.Fatalf("fixes: %+v", fixes)
	}
	f := fixes[0]
	if f.Rewritten != lt+in+";" || f.Original != in {
		t.Errorf("Rewritten = %q", f.Rewritten)
	}
	for _, w := range []string{"expand/contract", "new column", "backfill", "public.orders rewritten"} {
		if !strings.Contains(f.Description, w) {
			t.Errorf("description lacks %q: %s", w, f.Description)
		}
	}
}

func TestVolatileDefaultDescriptionMentionsBackfill(t *testing.T) {
	fixes := Suggest(lt+"ALTER TABLE orders ADD COLUMN r float8 NOT NULL DEFAULT random()", nil)
	if len(fixes) != 1 || !strings.Contains(fixes[0].Description, "backfill") || !strings.Contains(fixes[0].Description, "NOT NULL was dropped") {
		t.Fatalf("fixes: %+v", fixes)
	}
}

func TestApplyEdgeCases(t *testing.T) {
	if got := Apply("SELECT 1;", nil); got != "SELECT 1;" {
		t.Errorf("no fixes must return the input unchanged, got %q", got)
	}
	// duplicate statements: each fix replaces one occurrence, in order
	in := "CREATE INDEX a ON orders (id);\nSELECT 1;\nCREATE INDEX a ON orders (id);"
	fixes := Suggest(in, nil)
	want := "CREATE INDEX CONCURRENTLY a ON orders (id);\nSELECT 1;\nCREATE INDEX CONCURRENTLY a ON orders (id);\n"
	if got := Apply(in, fixes); got != want {
		t.Errorf("got %q", got)
	}
	// semicolons inside strings and dollar bodies survive
	in = "DO $$ BEGIN PERFORM 1; END $$;\nCREATE INDEX b ON orders (status) WHERE status <> 'a;b';"
	want = "DO $$ BEGIN PERFORM 1; END $$;\nCREATE INDEX CONCURRENTLY b ON orders (status) WHERE status <> 'a;b';\n"
	if got := Apply(in, Suggest(in, nil)); got != want {
		t.Errorf("got %q", got)
	}
}

func TestQuoteIdent(t *testing.T) {
	for in, want := range map[string]string{"orders_x": "orders_x", "Orders": `"Orders"`, "a b": `"a b"`, "user": `"user"`, `q"x`: `"q""x"`, "1a": `"1a"`} {
		if got := quoteIdent(in); got != want {
			t.Errorf("quoteIdent(%q) = %q want %q", in, got, want)
		}
	}
}

// ---------- integration: every fixer output executes on a real server ----------

const testSchema = `
CREATE TABLE users (id bigserial PRIMARY KEY, email text NOT NULL, name text);
CREATE TABLE orders (id bigserial PRIMARY KEY, user_id bigint, total integer, status text, created_at timestamptz DEFAULT now());
CREATE TABLE order_items (id bigserial PRIMARY KEY, order_id bigint, sku text, qty int);
CREATE TABLE "Line Items" (id bigserial PRIMARY KEY, "Order Id" bigint, sku text);
CREATE INDEX orders_status_idx ON orders (status);
INSERT INTO users (email, name) SELECT 'u' || g || '@example.test', 'user ' || g FROM generate_series(1, 200) g;
INSERT INTO orders (user_id, total, status) SELECT 1 + g % 200, g, 'paid' FROM generate_series(1, 2000) g;
INSERT INTO order_items (order_id, sku, qty) SELECT 1 + g % 2000, 'sku' || g, 1 FROM generate_series(1, 4000) g;
INSERT INTO "Line Items" ("Order Id", sku) SELECT 1 + g % 2000, 'sku' || g FROM generate_series(1, 4000) g;
`

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
	name := fmt.Sprintf("pgr_fixer_%d_%d", time.Now().UnixNano(), rand.Intn(1e6))
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
	for _, s := range locks.SplitStatements(testSchema) {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("schema: %s: %v", s, err)
		}
	}
	return pool
}

// TestFixedMigrationsExecute runs every case's rewritten migration statement
// by statement in autocommit mode (outside a transaction block) on a fresh
// database, and checks the outcome matches what the original would have done.
func TestFixedMigrationsExecute(t *testing.T) {
	if os.Getenv("PGR_TEST_DSN") == "" {
		t.Skip("PGR_TEST_DSN not set")
	}
	checks := map[string]string{ // case name -> query that must return true afterwards
		"create index":                        `SELECT indisvalid FROM pg_index WHERE indexrelid = 'orders_user_idx'::regclass`,
		"foreign key named":                   `SELECT convalidated FROM pg_constraint WHERE conname = 'orders_user_fk'`,
		"foreign key unnamed, quoted":         `SELECT convalidated AND contype = 'f' FROM pg_constraint WHERE conrelid = '"Line Items"'::regclass AND contype = 'f'`,
		"check named":                         `SELECT convalidated FROM pg_constraint WHERE conname = 'total_positive'`,
		"set not null":                        `SELECT attnotnull AND NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname LIKE '%not_null_chk') FROM pg_attribute WHERE attrelid = 'orders'::regclass AND attname = 'user_id'`,
		"set not null quoted":                 `SELECT attnotnull FROM pg_attribute WHERE attrelid = '"Line Items"'::regclass AND attname = 'Order Id'`,
		"add unique named":                    `SELECT contype = 'u' FROM pg_constraint WHERE conname = 'users_email_uq'`,
		"add unique unnamed with options":     `SELECT contype = 'u' AND condeferrable AND condeferred FROM pg_constraint WHERE conname = 'users_email_name_key'`,
		"add column volatile default":         `SELECT (SELECT count(*) FROM orders WHERE token IS NOT NULL) = 0 AND pg_get_expr(adbin, adrelid) = 'gen_random_uuid()' FROM pg_attrdef WHERE adrelid = 'orders'::regclass AND adnum = (SELECT attnum FROM pg_attribute WHERE attrelid = 'orders'::regclass AND attname = 'token')`,
		"alter column type: description only": `SELECT atttypid = 'numeric'::regtype FROM pg_attribute WHERE attrelid = 'orders'::regclass AND attname = 'total'`,
		"drop index":                          `SELECT to_regclass('orders_status_idx') IS NULL`,
		"whole migration in a transaction":    `SELECT convalidated AND to_regclass('orders_created_idx') IS NOT NULL FROM pg_constraint WHERE conname = 'orders_user_fk'`,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testDB(t)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			fixed := Apply(c.in, Suggest(c.in, nil))
			conn, err := pool.Acquire(ctx) // one session, so SET lock_timeout applies to what follows
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			for _, s := range locks.SplitStatements(fixed) {
				if _, err := conn.Exec(ctx, s); err != nil {
					t.Fatalf("statement failed: %v\n%s\n--- full migration ---\n%s", err, s, fixed)
				}
			}
			if q, ok := checks[c.name]; ok {
				var okv bool
				if err := conn.QueryRow(ctx, q).Scan(&okv); err != nil || !okv {
					t.Errorf("post-condition failed (ok=%v err=%v): %s", okv, err, q)
				}
			}
			// The lock analysis of the fixed migration must also work (static CONCURRENTLY path).
			pool2 := testDB(t)
			fp, err := locks.Analyze(ctx, pool2, fixed)
			if err != nil {
				t.Fatalf("Analyze(fixed): %v", err)
			}
			for _, f := range fp.Lint {
				if f.Rule == locks.RuleStatementFailed {
					t.Errorf("fixed migration fails under Analyze: %s: %s", f.Statement, f.Message)
				}
			}
		})
	}
}
