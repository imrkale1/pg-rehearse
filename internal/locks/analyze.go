package locks

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// StatementTimeout bounds each statement executed by Analyze.
var StatementTimeout = 5 * time.Minute

type relInfo struct {
	name        string // schema-qualified, quoted as needed
	kind        string // model kind
	relkind     string
	relfilenode uint32
}

type lockKey struct {
	oid  uint32
	mode string
}

func kindOf(relkind string) string {
	switch relkind {
	case "r", "p":
		return "table"
	case "i", "I":
		return "index"
	case "v", "m":
		return "view"
	case "S":
		return "sequence"
	}
	return "other"
}

// session is one Analyze run on a dedicated connection inside one transaction.
type session struct {
	conn     *pgxpool.Conn
	rels     map[uint32]*relInfo // user relations that existed before the migration
	held     map[lockKey]bool    // locks already held by this transaction
	version  int
	rewrites []string
	rewrote  map[string]bool
}

const userRelsSQL = `
SELECT c.oid::int8, format('%I.%I', n.nspname, c.relname), c.relkind::text, c.relfilenode::int8
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname <> 'information_schema' AND n.nspname !~ '^pg_'`

const myLocksSQL = `
SELECT l.relation::int8, l.mode
FROM pg_locks l
WHERE l.pid = pg_backend_pid() AND l.locktype = 'relation' AND l.granted
  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())`

// Analyze executes the migration cumulatively inside ONE transaction on the
// (shadow) database, capturing after every statement the relation locks it
// newly acquired (deltas: locks already held by earlier statements of the
// same transaction are not repeated), its duration and any table rewrite
// (relfilenode change), then rolls everything back. Each statement runs in a
// savepoint so a failing statement is reported (rule "statement-failed",
// locks estimated statically) without aborting the rest. Statements that
// cannot run in a transaction block (… CONCURRENTLY, VACUUM, …) are analyzed
// statically; CREATE/DROP INDEX CONCURRENTLY are additionally executed in
// their non-concurrent form so later statements that depend on them work.
// Only locks on relations that existed before the migration are reported:
// temp, catalog and newly created relations are invisible to other sessions.
func Analyze(ctx context.Context, pool *pgxpool.Pool, migrationSQL string) (fp *model.LockFootprint, err error) {
	fp = &model.LockFootprint{Lint: Lint(migrationSQL), Statements: []model.StatementLocks{}, RewrittenTables: []string{}}
	stmts := SplitStatements(migrationSQL)
	if len(stmts) == 0 {
		return fp, nil
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("locks: acquire connection: %w", err)
	}
	healthy := false
	defer func() {
		if !healthy {
			// Unknown transaction state: don't hand this connection back to the pool.
			_ = conn.Conn().Close(context.Background())
		}
		conn.Release()
	}()

	s := &session{conn: conn, rels: map[uint32]*relInfo{}, held: map[lockKey]bool{}, rewrote: map[string]bool{}}
	var vs string
	if err := conn.QueryRow(ctx, "SHOW server_version_num").Scan(&vs); err != nil {
		return nil, fmt.Errorf("locks: server version: %w", err)
	}
	s.version, _ = strconv.Atoi(vs)

	if _, err := conn.Exec(ctx, "BEGIN"); err != nil {
		return nil, fmt.Errorf("locks: begin: %w", err)
	}
	if err := s.snapshotRels(ctx); err != nil {
		return nil, err
	}
	if err := s.refreshHeld(ctx); err != nil {
		return nil, err
	}

	for _, sql := range stmts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st := Parse(sql)
		sl, finding, err := s.analyzeOne(ctx, &st)
		if err != nil {
			return nil, err
		}
		fp.Statements = append(fp.Statements, sl)
		if finding != nil {
			fp.Lint = append(fp.Lint, *finding)
		}
	}
	fp.RewrittenTables = append(fp.RewrittenTables, s.rewrites...)

	if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
		return nil, fmt.Errorf("locks: rollback: %w", err)
	}
	healthy = true
	return fp, nil
}

// existed reports whether a qualified relation name (as produced by
// resolve) existed before the migration started.
func (s *session) existed(name string) bool {
	for _, ri := range s.rels {
		if ri.name == name {
			return true
		}
	}
	return false
}

func (s *session) snapshotRels(ctx context.Context) error {
	rows, err := s.conn.Query(ctx, userRelsSQL)
	if err != nil {
		return fmt.Errorf("locks: snapshot relations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var oid, fn int64
		var name, relkind string
		if err := rows.Scan(&oid, &name, &relkind, &fn); err != nil {
			return err
		}
		s.rels[uint32(oid)] = &relInfo{name: name, kind: kindOf(relkind), relkind: relkind, relfilenode: uint32(fn)}
	}
	return rows.Err()
}

func (s *session) currentLocks(ctx context.Context) ([]lockKey, error) {
	rows, err := s.conn.Query(ctx, myLocksSQL)
	if err != nil {
		return nil, fmt.Errorf("locks: read pg_locks: %w", err)
	}
	defer rows.Close()
	var out []lockKey
	for rows.Next() {
		var oid int64
		var mode string
		if err := rows.Scan(&oid, &mode); err != nil {
			return nil, err
		}
		out = append(out, lockKey{uint32(oid), mode})
	}
	return out, rows.Err()
}

func (s *session) refreshHeld(ctx context.Context) error {
	ls, err := s.currentLocks(ctx)
	if err != nil {
		return err
	}
	for _, k := range ls {
		s.held[k] = true
	}
	return nil
}

// newLocks returns locks on pre-existing relations acquired since the last call.
func (s *session) newLocks(ctx context.Context) ([]model.RelationLock, error) {
	ls, err := s.currentLocks(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.RelationLock
	for _, k := range ls {
		if s.held[k] {
			continue
		}
		s.held[k] = true
		ri, ok := s.rels[k.oid]
		if !ok {
			continue // created by this migration, or a catalog/temp relation
		}
		out = append(out, relationLock(ri.name, ri.kind, k.mode))
	}
	sortLocks(out)
	return out, nil
}

func relationLock(name, kind, mode string) model.RelationLock {
	return model.RelationLock{Relation: name, Kind: kind, Mode: mode, BlocksReads: BlocksReads(mode), BlocksWrites: BlocksWrites(mode)}
}

func sortLocks(ls []model.RelationLock) {
	sort.SliceStable(ls, func(i, j int) bool {
		if ls[i].Kind != ls[j].Kind {
			return ls[i].Kind == "table" // tables first
		}
		if ls[i].Relation != ls[j].Relation {
			return ls[i].Relation < ls[j].Relation
		}
		return modeIndex[ls[i].Mode] > modeIndex[ls[j].Mode]
	})
}

// checkRewrites compares relfilenodes of pre-existing tables and returns newly rewritten ones.
func (s *session) checkRewrites(ctx context.Context) ([]string, error) {
	rows, err := s.conn.Query(ctx, `SELECT oid::int8, relfilenode::int8 FROM pg_class WHERE relkind IN ('r','m')`)
	if err != nil {
		return nil, fmt.Errorf("locks: read relfilenodes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var oid, fn int64
		if err := rows.Scan(&oid, &fn); err != nil {
			return nil, err
		}
		ri, ok := s.rels[uint32(oid)]
		if !ok || (ri.relkind != "r" && ri.relkind != "m") {
			continue
		}
		if uint32(fn) != ri.relfilenode {
			ri.relfilenode = uint32(fn)
			out = append(out, ri.name)
			if !s.rewrote[ri.name] {
				s.rewrote[ri.name] = true
				s.rewrites = append(s.rewrites, ri.name)
			}
		}
	}
	return out, rows.Err()
}

func (s *session) exec(ctx context.Context, sql string) error {
	_, err := s.conn.Exec(ctx, sql)
	return err
}

// tryExec runs sql in a savepoint; a failure is rolled back to the savepoint
// and returned as stmtErr (err is reserved for infrastructure failures).
func (s *session) tryExec(ctx context.Context, sql string) (dur time.Duration, stmtErr, err error) {
	if err := s.exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", StatementTimeout.Milliseconds())); err != nil {
		return 0, nil, fmt.Errorf("locks: set statement_timeout: %w", err)
	}
	if err := s.exec(ctx, "SAVEPOINT pgr_rehearse_stmt"); err != nil {
		return 0, nil, fmt.Errorf("locks: savepoint: %w", err)
	}
	start := time.Now()
	stmtErr = s.exec(ctx, sql)
	dur = time.Since(start)
	if stmtErr != nil {
		if ctx.Err() != nil {
			return dur, nil, ctx.Err()
		}
		if err := s.exec(ctx, "ROLLBACK TO SAVEPOINT pgr_rehearse_stmt"); err != nil {
			return dur, nil, fmt.Errorf("locks: rollback to savepoint after %q: %v: %w", sql, stmtErr, err)
		}
	}
	if err := s.exec(ctx, "RELEASE SAVEPOINT pgr_rehearse_stmt"); err != nil {
		return dur, nil, fmt.Errorf("locks: release savepoint: %w", err)
	}
	return dur, stmtErr, nil
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func (s *session) analyzeOne(ctx context.Context, st *Statement) (model.StatementLocks, *model.LintFinding, error) {
	sl := model.StatementLocks{SQL: st.SQL, Locks: []model.RelationLock{}}
	switch {
	case st.Kind == StmtTxnControl:
		// The whole migration already runs in our transaction; skip BEGIN/COMMIT.
		return sl, nil, nil
	case st.NeedsNoTransaction(s.version):
		return s.analyzeStatic(ctx, st, sl)
	}
	dur, stmtErr, err := s.tryExec(ctx, st.SQL)
	if err != nil {
		return sl, nil, err
	}
	sl.DurationMs = ms(dur)
	if stmtErr != nil {
		sl.Locks = s.estimateLocks(ctx, st)
		return sl, &model.LintFinding{
			Rule: RuleStatementFailed, Severity: model.SeverityDanger, Statement: st.SQL,
			Message:    "Statement failed on the shadow database: " + stmtErr.Error(),
			Suggestion: "Fix the statement; locks shown for it are estimated statically.",
		}, nil
	}
	if sl.Locks, err = s.newLocks(ctx); err != nil {
		return sl, nil, err
	}
	rw, err := s.checkRewrites(ctx)
	if err != nil {
		return sl, nil, err
	}
	var finding *model.LintFinding
	if len(rw) > 0 && !explainsRewrite(st) {
		finding = &model.LintFinding{
			Rule: RuleTableRewrite, Severity: model.SeverityDanger, Statement: st.SQL,
			Message:    "Statement rewrote " + strings.Join(rw, ", ") + " (relfilenode changed): the whole table is copied while an AccessExclusiveLock blocks reads and writes.",
			Suggestion: "Find a non-rewriting equivalent (e.g. new column + batched backfill).",
		}
	}
	return sl, finding, nil
}

// explainsRewrite reports whether a static rule already covers a rewrite.
func explainsRewrite(st *Statement) bool {
	switch st.Kind {
	case StmtVacuum, StmtCluster:
		return true
	case StmtAlterTable:
		for _, a := range st.Actions {
			if a.Kind == ActAlterType || (a.Kind == ActAddColumn && a.VolatileDefault) {
				return true
			}
		}
	}
	return false
}

// resolve returns the qualified name and kind of a relation as written, or
// (as-written, fallback) when it cannot be found.
func (s *session) resolve(ctx context.Context, name, fallbackKind string) (string, string) {
	var q, relkind string
	err := s.conn.QueryRow(ctx, `SELECT format('%I.%I', n.nspname, c.relname), c.relkind::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.oid = to_regclass($1)`, name).Scan(&q, &relkind)
	if err != nil {
		return name, fallbackKind
	}
	return q, kindOf(relkind)
}

// indexTable returns the qualified table an index belongs to.
func (s *session) indexTable(ctx context.Context, index string) (string, bool) {
	var q string
	err := s.conn.QueryRow(ctx, `SELECT format('%I.%I', n.nspname, c.relname)
		FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE i.indexrelid = to_regclass($1)`, index).Scan(&q)
	return q, err == nil
}

// withoutConcurrently removes the CONCURRENTLY keyword, keeping everything else verbatim.
func withoutConcurrently(st *Statement) string {
	for _, t := range st.toks {
		if t.upper() == "CONCURRENTLY" {
			return st.SQL[:t.Pos] + st.SQL[t.End:]
		}
	}
	return st.SQL
}

// analyzeStatic reports the documented lock modes for statements that cannot
// run inside a transaction block.
func (s *session) analyzeStatic(ctx context.Context, st *Statement, sl model.StatementLocks) (model.StatementLocks, *model.LintFinding, error) {
	add := func(name, kind, mode string) {
		if !s.existed(name) {
			return // created by this migration: invisible to other sessions
		}
		sl.Locks = append(sl.Locks, relationLock(name, kind, mode))
	}
	switch st.Kind {
	case StmtCreateIndex:
		tbl, kind := s.resolve(ctx, st.Table, "table")
		add(tbl, kind, ShareUpdateExclusive)
		// Build it non-concurrently so dependent statements (… USING INDEX) work.
		dur, _, err := s.tryExec(ctx, withoutConcurrently(st))
		if err != nil {
			return sl, nil, err
		}
		sl.DurationMs = ms(dur)
	case StmtDropIndex:
		if tbl, ok := s.indexTable(ctx, st.IndexName); ok {
			add(tbl, "table", ShareUpdateExclusive)
		}
		idx, _ := s.resolve(ctx, st.IndexName, "index")
		add(idx, "index", ShareUpdateExclusive)
		dur, _, err := s.tryExec(ctx, withoutConcurrently(st))
		if err != nil {
			return sl, nil, err
		}
		sl.DurationMs = ms(dur)
	case StmtReindex:
		if st.Table != "" {
			if st.SetName == "index" {
				if tbl, ok := s.indexTable(ctx, st.Table); ok {
					add(tbl, "table", ShareUpdateExclusive)
				}
				idx, _ := s.resolve(ctx, st.Table, "index")
				add(idx, "index", ShareUpdateExclusive)
			} else if st.SetName == "table" || st.SetName == "" {
				tbl, kind := s.resolve(ctx, st.Table, "table")
				add(tbl, kind, ShareUpdateExclusive)
			}
		}
	case StmtVacuum:
		if st.Table != "" {
			tbl, kind := s.resolve(ctx, st.Table, "table")
			if st.Full {
				add(tbl, kind, AccessExclusive)
				if !s.rewrote[tbl] {
					s.rewrote[tbl] = true
					s.rewrites = append(s.rewrites, tbl)
				}
			} else {
				add(tbl, kind, ShareUpdateExclusive)
			}
		}
	}
	// Locks taken by helper queries / the non-concurrent stand-in must not be
	// attributed to later statements.
	if err := s.refreshHeld(ctx); err != nil {
		return sl, nil, err
	}
	sortLocks(sl.Locks)
	return sl, nil, nil
}

// estimateLocks guesses the locks of a statement that failed to execute.
func (s *session) estimateLocks(ctx context.Context, st *Statement) []model.RelationLock {
	mode := ""
	switch st.Kind {
	case StmtAlterTable:
		mode = AccessExclusive
		if !st.TakesBlockingLock() {
			mode = ShareUpdateExclusive
		}
	case StmtCreateIndex:
		mode = Share
		if st.Concurrently {
			mode = ShareUpdateExclusive
		}
	case StmtDropTable, StmtTruncate, StmtCluster, StmtLockTable:
		mode = AccessExclusive
	case StmtCreateTrigger:
		mode = ShareRowExclusive
	}
	if mode == "" || st.Table == "" {
		return []model.RelationLock{}
	}
	tbl, kind := s.resolve(ctx, st.Table, "table")
	ls := []model.RelationLock{relationLock(tbl, kind, mode)}
	if st.Kind == StmtAlterTable {
		for _, a := range st.Actions {
			if a.ConstraintType == "FOREIGN KEY" && a.RefTable != "" {
				rt, rk := s.resolve(ctx, a.RefTable, "table")
				ls = append(ls, relationLock(rt, rk, ShareRowExclusive))
			}
		}
	}
	return ls
}
