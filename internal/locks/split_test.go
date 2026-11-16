package locks

import (
	"reflect"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"only semicolons", " ; ;\n;", nil},
		{"simple", "SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"no trailing semicolon", "SELECT 1;\nSELECT 2", []string{"SELECT 1", "SELECT 2"}},
		{"single quotes", "INSERT INTO t VALUES ('a;b'); SELECT 1", []string{"INSERT INTO t VALUES ('a;b')", "SELECT 1"}},
		{"doubled quote", "SELECT 'it''s; fine'; SELECT 2", []string{"SELECT 'it''s; fine'", "SELECT 2"}},
		{"escape string", `SELECT E'a\';b'; SELECT 2`, []string{`SELECT E'a\';b'`, "SELECT 2"}},
		{"plain string backslash", `SELECT 'a\'; SELECT 2`, []string{`SELECT 'a\'`, "SELECT 2"}},
		{"quoted ident", `ALTER TABLE "we;ird" ADD x int; SELECT 1`, []string{`ALTER TABLE "we;ird" ADD x int`, "SELECT 1"}},
		{"dollar", "DO $$ BEGIN PERFORM 1; END $$; SELECT 1", []string{"DO $$ BEGIN PERFORM 1; END $$", "SELECT 1"}},
		{"dollar tag", "CREATE FUNCTION f() RETURNS int AS $fn$ SELECT 1; -- $$ ; \n $fn$ LANGUAGE sql; SELECT 2",
			[]string{"CREATE FUNCTION f() RETURNS int AS $fn$ SELECT 1; -- $$ ; \n $fn$ LANGUAGE sql", "SELECT 2"}},
		{"nested dollar", "DO $a$ BEGIN EXECUTE $b$ SELECT 1; $b$; END $a$; SELECT 2", []string{"DO $a$ BEGIN EXECUTE $b$ SELECT 1; $b$; END $a$", "SELECT 2"}},
		{"param not dollar quote", "SELECT $1; SELECT $2", []string{"SELECT $1", "SELECT $2"}},
		{"ident with dollar", "SELECT a$b$c; SELECT 2", []string{"SELECT a$b$c", "SELECT 2"}},
		{"line comment", "SELECT 1 -- a; b\n; SELECT 2", []string{"SELECT 1 -- a; b", "SELECT 2"}},
		{"line comment quote", "SELECT 1; -- it's\nSELECT 2;", []string{"SELECT 1", "-- it's\nSELECT 2"}},
		{"block comment", "SELECT /* ; */ 1; SELECT 2", []string{"SELECT /* ; */ 1", "SELECT 2"}},
		{"nested block comment", "SELECT /* a /* b; */ c; */ 1; SELECT 2", []string{"SELECT /* a /* b; */ c; */ 1", "SELECT 2"}},
		{"comment only dropped", "-- hello\n/* x; */\n", nil},
		{"comment then semicolon dropped", "-- hello\n;\nSELECT 1; /* bye */ ;", []string{"SELECT 1"}},
		{"trailing comment dropped", "SELECT 1;\n-- done", []string{"SELECT 1"}},
		{"begin atomic", "CREATE FUNCTION f() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; SELECT CASE WHEN true THEN 1 END; END; SELECT 2",
			[]string{"CREATE FUNCTION f() RETURNS int LANGUAGE sql BEGIN ATOMIC SELECT 1; SELECT CASE WHEN true THEN 1 END; END", "SELECT 2"}},
		{"begin txn", "BEGIN; ALTER TABLE t ADD x int; COMMIT;", []string{"BEGIN", "ALTER TABLE t ADD x int", "COMMIT"}},
		{"unterminated string", "SELECT 'abc; SELECT 2", []string{"SELECT 'abc; SELECT 2"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitStatements(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("SplitStatements(%q)\n got: %q\nwant: %q", c.in, got, c.want)
			}
		})
	}
}

func TestSplitSegmentsKeepsComments(t *testing.T) {
	segs := SplitSegments("-- header\n;\nSELECT 1;\n-- tail")
	want := []Segment{{"-- header", true}, {"SELECT 1", false}, {"-- tail", true}}
	if !reflect.DeepEqual(segs, want) {
		t.Errorf("got %#v want %#v", segs, want)
	}
}

func TestConflictMatrix(t *testing.T) {
	cases := []struct {
		mode          string
		reads, writes bool
	}{
		{AccessShare, false, false}, {RowShare, false, false}, {RowExclusive, false, false},
		{ShareUpdateExclusive, false, false}, {Share, false, true}, {ShareRowExclusive, false, true},
		{Exclusive, false, true}, {AccessExclusive, true, true}, {"BogusLock", true, true},
	}
	for _, c := range cases {
		if BlocksReads(c.mode) != c.reads || BlocksWrites(c.mode) != c.writes {
			t.Errorf("%s: reads=%v writes=%v", c.mode, BlocksReads(c.mode), BlocksWrites(c.mode))
		}
	}
	for a := range modeIndex {
		for b := range modeIndex {
			if Conflicts(a, b) != Conflicts(b, a) {
				t.Errorf("matrix not symmetric for %s/%s", a, b)
			}
		}
	}
}
