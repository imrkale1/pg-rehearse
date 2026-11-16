package locks

import (
	"sort"
	"strings"
	"testing"
)

func rules(sql string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range Lint(sql) {
		if !seen[f.Rule] {
			seen[f.Rule] = true
			out = append(out, f.Rule)
		}
	}
	sort.Strings(out)
	return out
}

func TestLint(t *testing.T) {
	const lt = "SET lock_timeout = '2s';\n"
	cases := []struct {
		name string
		sql  string
		want []string // exact set of rule ids
	}{
		{"create index", lt + "CREATE INDEX idx ON orders (user_id)", []string{RuleCreateIndexNotConcurrent}},
		{"create index lowercase quoted", lt + "create  unique\n index if not exists \"Idx\" on \"public\".\"Orders\" using btree (user_id)", []string{RuleCreateIndexNotConcurrent}},
		{"create index unnamed", lt + "CREATE INDEX ON public.orders (user_id)", []string{RuleCreateIndexNotConcurrent}},
		{"create index concurrently", "CREATE INDEX CONCURRENTLY IF NOT EXISTS idx ON orders (user_id)", nil},
		{"create index on new table", "CREATE TABLE IF NOT EXISTS public.t (id int); CREATE INDEX i ON public.t (id); CREATE INDEX j ON t (id)", nil},
		{"fk", lt + "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id)", []string{RuleAddFKWithoutNotValid}},
		{"fk unnamed", lt + "alter table only public.orders add foreign key (user_id) references public.users(id) on delete cascade", []string{RuleAddFKWithoutNotValid}},
		{"fk not valid", lt + "ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id) NOT  VALID", nil},
		{"check", lt + "ALTER TABLE IF EXISTS orders ADD CONSTRAINT c CHECK (total >= 0)", []string{RuleAddCheckWithoutNotValid}},
		{"check unnamed", lt + "ALTER TABLE orders ADD CHECK (total >= 0 AND status <> 'not valid')", []string{RuleAddCheckWithoutNotValid}},
		{"check not valid", lt + "ALTER TABLE orders ADD CONSTRAINT c CHECK (total >= 0) not valid", nil},
		{"validate only needs no timeout", "ALTER TABLE orders VALIDATE CONSTRAINT c", nil},
		{"set not null", lt + `ALTER TABLE "orders" ALTER COLUMN "user_id" SET NOT NULL`, []string{RuleSetNotNull}},
		{"set not null no COLUMN", lt + "ALTER TABLE orders ALTER user_id SET NOT NULL", []string{RuleSetNotNull}},
		{"alter type", lt + "ALTER TABLE orders ALTER COLUMN total TYPE numeric(14,2)", []string{RuleAlterColumnType}},
		{"alter set data type", lt + "ALTER TABLE orders ALTER total SET DATA TYPE bigint USING total::bigint", []string{RuleAlterColumnType}},
		{"add column volatile", lt + "ALTER TABLE orders ADD COLUMN token uuid DEFAULT gen_random_uuid()", []string{RuleAddColumnVolatileDefault}},
		{"add column volatile if not exists", lt + "ALTER TABLE orders ADD COLUMN IF NOT EXISTS r float8 NOT NULL DEFAULT random()", []string{RuleAddColumnVolatileDefault}},
		{"add column serial", lt + "ALTER TABLE orders ADD seq bigserial", []string{RuleAddColumnVolatileDefault}},
		{"add column const default", lt + "ALTER TABLE orders ADD COLUMN x int NOT NULL DEFAULT 0", nil},
		{"add column now()", lt + "ALTER TABLE orders ADD COLUMN ts timestamptz DEFAULT now()", nil},
		{"add column string mentions random", lt + "ALTER TABLE orders ADD COLUMN x text DEFAULT 'random()'", nil},
		{"add unique", lt + "ALTER TABLE users ADD CONSTRAINT u UNIQUE (email)", []string{RuleAddUniqueConstraint}},
		{"add unique unnamed", lt + "ALTER TABLE users ADD UNIQUE (email)", []string{RuleAddUniqueConstraint}},
		{"add unique using index", lt + "ALTER TABLE users ADD CONSTRAINT u UNIQUE USING INDEX u", nil},
		{"rename column", lt + "ALTER TABLE users RENAME COLUMN name TO full_name", []string{RuleRenameColumn}},
		{"rename column no COLUMN", lt + "ALTER TABLE users RENAME name TO full_name", []string{RuleRenameColumn}},
		{"rename table", lt + "ALTER TABLE public.users RENAME TO accounts", []string{RuleRenameTable}},
		{"rename constraint is fine", lt + "ALTER TABLE users RENAME CONSTRAINT a TO b", nil},
		{"drop column", lt + "ALTER TABLE users DROP COLUMN IF EXISTS name", []string{RuleDropColumn}},
		{"drop constraint is not drop column", lt + "ALTER TABLE users DROP CONSTRAINT c", nil},
		{"vacuum full", "VACUUM FULL orders", []string{RuleMissingLockTimeout, RuleVacuumFull}},
		{"vacuum (full)", lt + "vacuum (full, analyze) orders", []string{RuleVacuumFull}},
		{"vacuum (full false)", "VACUUM (FULL false) orders", nil},
		{"vacuum plain", "VACUUM ANALYZE orders", nil},
		{"cluster", lt + "CLUSTER orders USING orders_pkey", []string{RuleCluster}},
		{"missing lock timeout", "ALTER TABLE orders ADD COLUMN x int", []string{RuleMissingLockTimeout}},
		{"lock timeout zero", "SET lock_timeout = 0; ALTER TABLE orders ADD COLUMN x int", []string{RuleMissingLockTimeout}},
		{"lock timeout TO", "set local lock_timeout to '500ms'; ALTER TABLE orders ADD COLUMN x int", nil},
		{"lock timeout too late", "ALTER TABLE orders ADD COLUMN x int; SET lock_timeout = '2s'", []string{RuleMissingLockTimeout}},
		{"multi action", lt + "ALTER TABLE orders ALTER COLUMN total TYPE bigint, ALTER COLUMN user_id SET NOT NULL, DROP COLUMN status",
			[]string{RuleAlterColumnType, RuleDropColumn, RuleSetNotNull}},
		{"comments and keywords in strings", lt + "/* CREATE INDEX x ON y(z) */ SELECT 'ALTER TABLE t DROP COLUMN c' -- VACUUM FULL", nil},
		{"concurrently in txn", "BEGIN; CREATE INDEX CONCURRENTLY i ON orders (id); COMMIT", []string{RuleConcurrentlyInTxn}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rules(c.sql)
			want := append([]string(nil), c.want...)
			sort.Strings(want)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("Lint(%q)\n got: %v\nwant: %v", c.sql, got, want)
			}
		})
	}
}

func TestLintFindingFields(t *testing.T) {
	fs := Lint("ALTER TABLE orders ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users (id);")
	if len(fs) != 2 {
		t.Fatalf("want 2 findings, got %+v", fs)
	}
	for _, f := range fs {
		if f.Statement == "" || f.Message == "" || f.Suggestion == "" || f.Severity == "" {
			t.Errorf("incomplete finding %+v", f)
		}
	}
}

func TestParsePreservesSpelling(t *testing.T) {
	st := Parse(`-- c
ALTER TABLE IF EXISTS ONLY "public"."Orders" ADD CONSTRAINT "Fk" FOREIGN KEY ("UserId") REFERENCES "public"."Users" (id), ALTER "Total" TYPE numeric(14,2)`)
	if st.Table != `"public"."Orders"` || st.Prefix != `ALTER TABLE IF EXISTS ONLY "public"."Orders"` {
		t.Fatalf("table=%q prefix=%q", st.Table, st.Prefix)
	}
	if len(st.Actions) != 2 {
		t.Fatalf("actions: %+v", st.Actions)
	}
	a := st.Actions[0]
	if a.ConstraintName != `"Fk"` || a.RefTable != `"public"."Users"` || len(a.Columns) != 1 || a.Columns[0] != `"UserId"` {
		t.Errorf("fk action %+v", a)
	}
	if st.Actions[1].Kind != ActAlterType || st.Actions[1].Column != `"Total"` {
		t.Errorf("type action %+v", st.Actions[1])
	}
	if NormIdent(st.Table) != "public.Orders" || BareName("Public.Foo") != "foo" {
		t.Errorf("norm")
	}
}

func TestLintSetNotNullAfterValidatedCheck(t *testing.T) {
	safe := `SET lock_timeout = '2s';
ALTER TABLE "Orders" ADD CONSTRAINT nn CHECK ("UserId" IS NOT NULL) NOT VALID;
ALTER TABLE "Orders" VALIDATE CONSTRAINT nn;
ALTER TABLE "Orders" ALTER COLUMN "UserId" SET NOT NULL;
ALTER TABLE "Orders" DROP CONSTRAINT nn;`
	if got := rules(safe); len(got) != 0 {
		t.Errorf("safe pattern flagged: %v", got)
	}
	// not validated, or a different column: still flagged
	for _, sql := range []string{
		"SET lock_timeout = '2s'; ALTER TABLE o ADD CONSTRAINT nn CHECK (a IS NOT NULL) NOT VALID; ALTER TABLE o ALTER a SET NOT NULL",
		"SET lock_timeout = '2s'; ALTER TABLE o ADD CONSTRAINT nn CHECK (a IS NOT NULL) NOT VALID; ALTER TABLE o VALIDATE CONSTRAINT nn; ALTER TABLE o ALTER b SET NOT NULL",
	} {
		if got := rules(sql); len(got) != 1 || got[0] != RuleSetNotNull {
			t.Errorf("%s: %v", sql, got)
		}
	}
}
