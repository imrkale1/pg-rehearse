// Package fixer rewrites risky migrations into safer equivalents.
//
// Suggest returns one model.Fix per risky statement (plus one for a missing
// lock_timeout); Apply substitutes them into the migration. Everything that is
// not rewritten keeps its original spelling: identifiers, quoting, comments.
//
// Rewritten migrations are meant to run statement by statement in autocommit
// mode (outside a transaction block): CREATE INDEX CONCURRENTLY requires it,
// and NOT VALID + VALIDATE CONSTRAINT only helps when the two run in separate
// transactions. Apply therefore drops explicit BEGIN/COMMIT when it introduces
// such statements.
package fixer

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"

	"github.com/pg-rehearse/pg-rehearse/internal/locks"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// LockTimeoutSQL is the fallback guard prepended when blocking DDL remains that
// could not be wrapped in a retry loop.
const LockTimeoutSQL = "SET lock_timeout = '2s'"

// ruleLockTimeout is the rule id of the prepended lock_timeout fix.
const ruleLockTimeout = locks.RuleMissingLockTimeout

// Lock-retry parameters. A blocking DDL statement waits at most lockRetryTimeout
// for its lock on each attempt, so it can never make application queries queue
// behind it for longer than that. Between attempts it sleeps (holding no lock),
// giving any conflicting transaction — a long report, say — time to finish.
const (
	lockRetryTimeout  = "100ms"
	lockRetryAttempts = 25
	lockRetrySleepSec = 2
)

const retryDesc = "Take the lock in short attempts (SET LOCAL lock_timeout = '" + lockRetryTimeout +
	"' in a retry loop) instead of waiting indefinitely: each attempt blocks application queries for at most " +
	lockRetryTimeout + ", and between attempts a conflicting transaction (e.g. a long-running report) can commit, so a brief DDL lock never turns into an outage."

// wrapRetry wraps a single blocking DDL statement in a PL/pgSQL retry loop.
// The statement runs as its own transaction (a DO block), so pg-rehearse's
// orchestrator — and the user's migration tool — must run it in autocommit mode.
func wrapRetry(stmt string) string {
	stmt = strings.TrimRight(strings.TrimSpace(stmt), ";")
	tmpl := `DO $pgr$
BEGIN
  -- pg-rehearse: acquire the lock in short attempts so a slow concurrent
  -- transaction cannot turn this DDL into an outage (see retryDesc).
  FOR attempt IN 1..__ATTEMPTS__ LOOP
    BEGIN
      SET LOCAL lock_timeout = '__TIMEOUT__';
      __STMT__;
      RETURN;
    EXCEPTION WHEN lock_not_available THEN
      RAISE NOTICE 'pg-rehearse: lock busy, attempt % of __ATTEMPTS__', attempt;
      PERFORM pg_sleep(__SLEEP__);
    END;
  END LOOP;
  RAISE EXCEPTION 'pg-rehearse: could not acquire lock after __ATTEMPTS__ attempts';
END
$pgr$`
	return strings.NewReplacer(
		"__ATTEMPTS__", strconv.Itoa(lockRetryAttempts),
		"__TIMEOUT__", lockRetryTimeout,
		"__SLEEP__", strconv.Itoa(lockRetrySleepSec),
		"__STMT__", stmt,
	).Replace(tmpl)
}

var alterTableHeadRe = regexp.MustCompile(`(?is)^\s*(?:--[^\n]*\n|/\*.*?\*/|\s)*ALTER\s+TABLE\b`)

// needsRetry reports whether a (rewritten) statement takes a lock that blocks
// reads or writes and should therefore be wrapped. CONCURRENTLY statements take
// only ShareUpdateExclusive (no conflict with normal traffic), and a lone
// VALIDATE CONSTRAINT is also ShareUpdateExclusive — neither needs a retry.
func needsRetry(stmt string) bool {
	if strings.Contains(strings.ToUpper(stmt), "CONCURRENTLY") {
		return false
	}
	if !alterTableHeadRe.MatchString(stmt) {
		return false
	}
	st := locks.Parse(stmt)
	if st.Kind != locks.StmtAlterTable || len(st.Actions) == 0 {
		return false
	}
	for _, a := range st.Actions {
		if a.Kind != locks.ActValidate {
			return true
		}
	}
	return false
}

// Suggest proposes safer rewrites for the risky statements of migrationSQL.
// fp (may be nil) is the footprint from locks.Analyze; it only enriches
// descriptions — the rewrites themselves are derived statically.
func Suggest(migrationSQL string, fp *model.LockFootprint) []model.Fix {
	state := locks.NewMigrationState()
	var fixes []model.Fix
	for _, sql := range locks.SplitStatements(migrationSQL) {
		st := locks.Parse(sql)
		if !state.IsNew(st.Table) {
			if f, ok := fixStatement(&st, state, fp); ok {
				fixes = append(fixes, f)
			}
		}
		state.Observe(&st)
	}
	// Does blocking DDL remain without a lock_timeout guarding it?
	rewritten := Apply(migrationSQL, fixes)
	for _, f := range locks.Lint(rewritten) {
		if f.Rule == locks.RuleMissingLockTimeout {
			fixes = append([]model.Fix{{
				Rule:        ruleLockTimeout,
				Description: "Start with SET lock_timeout = '2s' so a DDL statement that cannot get its lock quickly fails (retry it later) instead of queueing every application query on the table behind it.",
				Original:    "",
				Rewritten:   LockTimeoutSQL + ";",
			}}, fixes...)
			break
		}
	}
	return fixes
}

type builder struct {
	st      *locks.Statement
	out     []string
	group   []string
	rules   []string
	descs   []string
	changed bool
}

func (b *builder) rule(rule, desc string) {
	b.rules = append(b.rules, rule)
	b.descs = append(b.descs, desc)
}

func (b *builder) flush() {
	if len(b.group) > 0 {
		b.out = append(b.out, b.st.Prefix+" "+strings.Join(b.group, ", "))
		b.group = nil
	}
}

func (b *builder) emit(stmts ...string) {
	b.flush()
	b.out = append(b.out, stmts...)
	b.changed = true
}

func joinStmts(lead string, stmts []string) string {
	return lead + strings.Join(stmts, ";\n") + ";"
}

func fixStatement(st *locks.Statement, state *locks.MigrationState, fp *model.LockFootprint) (model.Fix, bool) {
	lead := st.LeadingComments()
	switch st.Kind {
	case locks.StmtCreateIndex:
		if st.Concurrently || st.Table == "" {
			return model.Fix{}, false
		}
		sql, ok := st.InsertAfterWord("INDEX", " CONCURRENTLY")
		if !ok {
			return model.Fix{}, false
		}
		return model.Fix{
			Rule:        locks.RuleCreateIndexNotConcurrent,
			Description: fmt.Sprintf("Build the index with CREATE INDEX CONCURRENTLY: it takes only a ShareUpdateExclusiveLock on %s, so reads and writes continue during the build. It must run outside a transaction block; if it fails, DROP INDEX CONCURRENTLY the leftover INVALID index and retry.", st.Table),
			Original:    st.SQL,
			Rewritten:   sql + ";",
		}, true
	case locks.StmtDropIndex:
		if st.Concurrently || st.Cascade || st.MultiTarget {
			return model.Fix{}, false
		}
		sql, ok := st.InsertAfterWord("INDEX", " CONCURRENTLY")
		if !ok {
			return model.Fix{}, false
		}
		return model.Fix{
			Rule:        locks.RuleDropIndexNotConcurrent,
			Description: "Drop the index with DROP INDEX CONCURRENTLY so queries on the table are not blocked (must run outside a transaction block).",
			Original:    st.SQL,
			Rewritten:   sql + ";",
		}, true
	case locks.StmtAlterTable:
		return fixAlterTable(st, state, lead, fp)
	}
	return model.Fix{}, false
}

func fixAlterTable(st *locks.Statement, state *locks.MigrationState, lead string, fp *model.LockFootprint) (model.Fix, bool) {
	if st.Prefix == "" || len(st.Actions) == 0 {
		return model.Fix{}, false
	}
	b := &builder{st: st}
	descOnly := false
	tbl := locks.BareName(st.Table)
	for i := range st.Actions {
		a := &st.Actions[i]
		switch {
		case a.Kind == locks.ActAddConstraint && a.ConstraintType == "FOREIGN KEY" && !a.NotValid:
			name, text := namedConstraint(a, tbl+"_"+joinNorm(a.Columns)+"_fkey")
			b.emit(st.Prefix+" "+text+" NOT VALID", st.Prefix+" VALIDATE CONSTRAINT "+name)
			b.rule(locks.RuleAddFKWithoutNotValid, fmt.Sprintf("Add the foreign key as NOT VALID (brief lock, no scan), then VALIDATE CONSTRAINT %s in its own transaction: validation takes only ShareUpdateExclusive on %s and RowShare on %s, so reads and writes keep flowing.", name, st.Table, a.RefTable))
		case a.Kind == locks.ActAddConstraint && a.ConstraintType == "CHECK" && !a.NotValid:
			name, text := namedConstraint(a, tbl+"_"+shortHash(a.Text)+"_check")
			b.emit(st.Prefix+" "+text+" NOT VALID", st.Prefix+" VALIDATE CONSTRAINT "+name)
			b.rule(locks.RuleAddCheckWithoutNotValid, fmt.Sprintf("Add the CHECK constraint as NOT VALID (brief lock, no scan), then VALIDATE CONSTRAINT %s in its own transaction (ShareUpdateExclusiveLock only: reads and writes continue).", name))
		case a.Kind == locks.ActAddConstraint && a.ConstraintType == "UNIQUE" && !a.UsingIndex:
			stmts, name, ok := uniqueViaIndex(st, a, tbl)
			if !ok {
				b.group = append(b.group, a.Text)
				continue
			}
			b.emit(stmts...)
			b.rule(locks.RuleAddUniqueConstraint, fmt.Sprintf("Build the unique index with CREATE UNIQUE INDEX CONCURRENTLY (reads and writes continue), then attach it with ADD CONSTRAINT %s UNIQUE USING INDEX (metadata only, brief lock). If the build fails (duplicates), drop the INVALID index, fix the data and retry.", name))
		case a.Kind == locks.ActSetNotNull && !state.NotNullProven(st.Table, a.Column):
			chk := quoteIdent(truncName(tbl + "_" + locks.BareName(a.Column) + "_not_null_chk"))
			b.emit(
				st.Prefix+" ADD CONSTRAINT "+chk+" CHECK ("+a.Column+" IS NOT NULL) NOT VALID",
				st.Prefix+" VALIDATE CONSTRAINT "+chk,
				st.Prefix+" "+a.Text,
				st.Prefix+" DROP CONSTRAINT "+chk,
			)
			b.rule(locks.RuleSetNotNull, fmt.Sprintf("Prove %s has no NULLs with a NOT VALID CHECK constraint validated in its own transaction (ShareUpdateExclusiveLock, no blocking); SET NOT NULL then uses the validated constraint and skips the full-table scan (PG12+), after which the helper CHECK is dropped.", a.Column))
		case a.Kind == locks.ActAddColumn && a.VolatileDefault && a.Default != "" && !generated(a):
			var parts []string
			parts = append(parts, "ADD COLUMN")
			if a.IfNotExists {
				parts = append(parts, "IF NOT EXISTS")
			}
			parts = append(parts, a.Column)
			if a.ColumnType != "" {
				parts = append(parts, a.ColumnType)
			}
			notNull := false
			for _, c := range a.ColumnClauses {
				if notNullRe.MatchString(c) {
					notNull = true
					continue
				}
				parts = append(parts, c)
			}
			b.emit(st.Prefix+" "+strings.Join(parts, " "), st.Prefix+" ALTER COLUMN "+a.Column+" SET DEFAULT "+a.Default)
			d := fmt.Sprintf("Add %s without a default (instant, no rewrite), then SET DEFAULT %s so only new rows get it. Existing rows stay NULL: backfill them in small batches (e.g. UPDATE %s SET %s = %s WHERE %s IS NULL AND <pk range>) outside the migration.", a.Column, a.Default, st.Table, a.Column, a.Default, a.Column)
			if notNull {
				d += " NOT NULL was dropped from the ADD COLUMN (it would fail on existing rows); add it after the backfill with the CHECK ... NOT VALID / VALIDATE / SET NOT NULL pattern."
			}
			b.rule(locks.RuleAddColumnVolatileDefault, d)
		case a.Kind == locks.ActAlterType:
			b.group = append(b.group, a.Text)
			descOnly = true
			d := fmt.Sprintf("No safe automatic rewrite for changing the type of %s: use expand/contract instead — add a new column with the new type, keep it in sync (trigger or dual writes), backfill it in batches, switch reads and writes to it, then drop the old column and rename. Until then the statement is only guarded by lock_timeout.", a.Column)
			if fp != nil {
				for _, r := range fp.RewrittenTables {
					if locks.BareName(r) == tbl {
						d += fmt.Sprintf(" The rehearsal saw %s rewritten under an AccessExclusiveLock.", r)
						break
					}
				}
			}
			b.rule(locks.RuleAlterColumnType, d)
		default:
			b.group = append(b.group, a.Text)
		}
	}
	b.flush()

	// Decide the statements to emit. With no structural rewrite, keep the
	// original statement verbatim — but it may still take a blocking lock.
	out := b.out
	pureWrap := false
	if !b.changed && !descOnly {
		out = []string{st.SQL}
		pureWrap = true
	}

	// Wrap each remaining blocking statement in a short-lock-timeout retry loop.
	// descOnly (an unrewritable table rewrite) is left to the lock_timeout guard:
	// a retry caps the wait for the lock, not the rewrite that runs once it is held.
	if !descOnly {
		wrapped := false
		for i, s := range out {
			if needsRetry(s) {
				out[i] = wrapRetry(s)
				wrapped = true
			}
		}
		if wrapped {
			b.rule(ruleLockTimeout, retryDesc)
		} else if pureWrap {
			return model.Fix{}, false // nothing structural and nothing blocking
		}
	}

	if len(b.rules) == 0 {
		return model.Fix{}, false
	}

	var rewritten string
	if b.changed || pureWrap {
		rewritten = joinStmts(lead, out)
	} else {
		rewritten = lead + st.SQL + ";"
	}
	if descOnly {
		rewritten = LockTimeoutSQL + ";\n" + rewritten
	}
	return model.Fix{
		Rule:        b.rules[0],
		Description: strings.Join(b.descs, " "),
		Original:    st.SQL,
		Rewritten:   rewritten,
	}, true
}

var notNullRe = regexp.MustCompile(`(?i)^\s*NOT\s+NULL\s*$`)

var generatedRe = regexp.MustCompile(`(?i)^\s*GENERATED\b`)

func generated(a *locks.Action) bool {
	for _, c := range a.ColumnClauses {
		if generatedRe.MatchString(c) {
			return true
		}
	}
	return false
}

// namedConstraint returns the constraint name (as SQL) and the action text
// with an explicit CONSTRAINT name (generating defName when unnamed).
func namedConstraint(a *locks.Action, defName string) (string, string) {
	if a.ConstraintName != "" {
		return a.ConstraintName, a.Text
	}
	name := quoteIdent(truncName(defName))
	rest := strings.TrimSpace(a.Text[len("ADD"):])
	return name, "ADD CONSTRAINT " + name + " " + rest
}

var uniqueTailRe = regexp.MustCompile(`(?is)^\s*(INCLUDE\s*\([^)]*\))?\s*(WITH\s*\([^)]*\))?\s*(USING\s+INDEX\s+TABLESPACE\s+\S+)?\s*(.*?)\s*$`)

// uniqueViaIndex turns ADD [CONSTRAINT n] UNIQUE [NULLS [NOT] DISTINCT] (cols) ...
// into CREATE UNIQUE INDEX CONCURRENTLY + ADD CONSTRAINT n UNIQUE USING INDEX n.
func uniqueViaIndex(st *locks.Statement, a *locks.Action, tbl string) ([]string, string, bool) {
	pre, cols, tail, ok := a.UniqueParts()
	if !ok || st.Table == "" {
		return nil, "", false
	}
	m := uniqueTailRe.FindStringSubmatch(tail)
	if m == nil {
		return nil, "", false
	}
	name := a.ConstraintName
	if name == "" {
		name = quoteIdent(truncName(tbl + "_" + joinNorm(a.Columns) + "_key"))
	}
	idx := []string{"CREATE UNIQUE INDEX CONCURRENTLY", name, "ON", st.Table, cols}
	if m[1] != "" {
		idx = append(idx, m[1])
	}
	if strings.TrimSpace(pre) != "" {
		idx = append(idx, strings.TrimSpace(pre)) // NULLS [NOT] DISTINCT
	}
	if m[2] != "" {
		idx = append(idx, m[2])
	}
	if m[3] != "" {
		f := strings.Fields(m[3])
		idx = append(idx, "TABLESPACE", f[len(f)-1])
	}
	con := st.Prefix + " ADD CONSTRAINT " + name + " UNIQUE USING INDEX " + name
	if m[4] != "" {
		con += " " + m[4] // DEFERRABLE / INITIALLY ...
	}
	return []string{strings.Join(idx, " "), con}, name, true
}

func joinNorm(cols []string) string {
	var p []string
	for _, c := range cols {
		p = append(p, locks.BareName(c))
	}
	return strings.Join(p, "_")
}

func shortHash(s string) string {
	h := fnv.New32a()
	h.Write([]byte(s))
	return fmt.Sprintf("%08x", h.Sum32())
}

// truncName keeps generated names within NAMEDATALEN-1 (63 bytes).
func truncName(s string) string {
	if len(s) <= 63 {
		return s
	}
	return s[:63]
}

var plainIdent = regexp.MustCompile(`^[a-z_][a-z0-9_$]*$`)

var reserved = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`all analyse analyze and any array as asc asymmetric authorization binary both case cast check
		collate collation column concurrently constraint create cross current_catalog current_date current_role current_schema
		current_time current_timestamp current_user default deferrable desc distinct do else end except false fetch for foreign
		freeze from full grant group having ilike in initially inner intersect into is isnull join lateral leading left like limit
		localtime localtimestamp natural not notnull null offset on only or order outer overlaps placing primary references
		returning right select session_user similar some symmetric system_user table tablesample then to trailing true union
		unique user using variadic verbose when where window with`) {
		reserved[w] = true
	}
}

// quoteIdent renders an (unquoted, normalized) identifier as SQL.
func quoteIdent(s string) string {
	if plainIdent.MatchString(s) && !reserved[s] {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// Apply returns the full rewritten migration with all fixes applied. Each
// fix replaces the first not-yet-replaced statement equal to its Original;
// a fix with an empty Original and rule missing-lock-timeout prepends
// SET lock_timeout. When the result contains statements that cannot run in a
// transaction block (CONCURRENTLY) or split a statement into steps that must
// commit separately, explicit BEGIN/COMMIT statements are removed (and
// SET LOCAL becomes SET) so the migration runs statement by statement.
func Apply(migrationSQL string, fixes []model.Fix) string {
	if len(fixes) == 0 {
		return migrationSQL
	}
	type item struct {
		text    string
		comment bool
	}
	used := make([]bool, len(fixes))
	prependTimeout := false
	split := false
	for i, f := range fixes {
		if strings.TrimSpace(f.Original) == "" {
			used[i] = true
			if f.Rule == ruleLockTimeout || isLockTimeout(f.Rewritten) {
				prependTimeout = true
			}
		}
	}
	var items []item
	for _, seg := range locks.SplitSegments(migrationSQL) {
		if seg.CommentOnly {
			items = append(items, item{seg.Text, true})
			continue
		}
		replaced := false
		for i, f := range fixes {
			if used[i] || normSQL(f.Original) != normSQL(seg.Text) {
				continue
			}
			used[i] = true
			replaced = true
			n := 0
			for _, rs := range locks.SplitSegments(f.Rewritten) {
				items = append(items, item{rs.Text, rs.CommentOnly})
				if !rs.CommentOnly && !isLockTimeout(rs.Text) {
					n++
				}
			}
			if n > 1 {
				split = true
			}
			break
		}
		if !replaced {
			items = append(items, item{seg.Text, false})
		}
	}

	noTxn := split
	for _, it := range items {
		if it.comment {
			continue
		}
		if st := locks.Parse(it.text); st.NeedsNoTransaction(0) {
			noTxn = true
		}
	}

	var b strings.Builder
	write := func(it item) {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(it.text)
		if !it.comment {
			b.WriteString(";")
		}
	}
	seenTimeout := false
	if prependTimeout {
		write(item{LockTimeoutSQL, false})
		seenTimeout = true
	}
	for _, it := range items {
		if !it.comment {
			st := locks.Parse(it.text)
			if noTxn && st.Kind == locks.StmtTxnControl {
				continue
			}
			if isLockTimeout(it.text) {
				if seenTimeout {
					continue
				}
				seenTimeout = true
			}
			if noTxn && st.Kind == locks.StmtSet {
				it.text = setLocalRe.ReplaceAllString(it.text, "${1}SET")
			}
		}
		write(it)
	}
	b.WriteString("\n")
	return b.String()
}

var setLocalRe = regexp.MustCompile(`(?is)^((?:\s|--[^\n]*\n|/\*.*?\*/)*)SET\s+LOCAL\b`)

var spaceRe = regexp.MustCompile(`\s+`)

func normSQL(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ";")
	return spaceRe.ReplaceAllString(strings.TrimSpace(s), " ")
}

// isLockTimeout reports whether sql is exactly the fixer's lock_timeout statement.
func isLockTimeout(sql string) bool {
	return strings.EqualFold(normSQL(sql), LockTimeoutSQL)
}
