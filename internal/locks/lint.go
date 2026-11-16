package locks

import (
	"fmt"
	"strings"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// Stable lint rule ids.
const (
	RuleCreateIndexNotConcurrent = "create-index-not-concurrent"
	RuleDropIndexNotConcurrent   = "drop-index-not-concurrent"
	RuleAddFKWithoutNotValid     = "add-foreign-key-without-not-valid"
	RuleAddCheckWithoutNotValid  = "add-check-without-not-valid"
	RuleSetNotNull               = "set-not-null"
	RuleAlterColumnType          = "alter-column-type"
	RuleAddColumnVolatileDefault = "add-column-volatile-default"
	RuleAddUniqueConstraint      = "add-unique-constraint"
	RuleAddPrimaryKey            = "add-primary-key"
	RuleRenameColumn             = "rename-column"
	RuleRenameTable              = "rename-table"
	RuleDropColumn               = "drop-column"
	RuleMissingLockTimeout       = "missing-lock-timeout"
	RuleVacuumFull               = "vacuum-full"
	RuleCluster                  = "cluster"
	RuleMultipleDDLInTxn         = "multiple-ddl-in-transaction"
	RuleConcurrentlyInTxn        = "concurrently-in-transaction"
	// Produced by Analyze only:
	RuleStatementFailed = "statement-failed"
	RuleTableRewrite    = "table-rewrite"
)

// migrationState tracks facts that depend on statement order.
type migrationState struct {
	created map[string]bool // tables created earlier in this migration (normalized, and bare names)
}

func newState() *migrationState { return &migrationState{created: map[string]bool{}} }

func (m *migrationState) observe(st *Statement) {
	if st.Kind == StmtCreateTable && st.Table != "" {
		m.created[NormIdent(st.Table)] = true
		m.created[BareName(st.Table)] = true
	}
}

// isNew reports whether the table was created earlier in the same migration
// (it is empty and invisible to other sessions, so locking it is harmless).
func (m *migrationState) isNew(table string) bool {
	if table == "" {
		return false
	}
	return m.created[NormIdent(table)]
}

// lockTimeoutValueSet reports whether a SET lock_timeout value is a non-zero timeout.
func lockTimeoutValueSet(v string) bool {
	v = strings.Trim(strings.TrimSpace(v), `'"`)
	return v != "" && v != "0" && !strings.EqualFold(v, "default") && !strings.HasPrefix(v, "0 ") && v != "0s" && v != "0ms"
}

// Lint applies static rules to a migration. It never touches a database.
func Lint(migrationSQL string) []model.LintFinding {
	var out []model.LintFinding
	add := func(rule string, sev model.Severity, stmt, msg, sugg string) {
		out = append(out, model.LintFinding{Rule: rule, Severity: sev, Statement: stmt, Message: msg, Suggestion: sugg})
	}
	state := newState()
	hasLockTimeout := false
	warnedTimeout := false
	inTxn := false
	var txnBlocking []string

	for _, sql := range SplitStatements(migrationSQL) {
		st := Parse(sql)
		existing := !state.isNew(st.Table)

		switch st.Kind {
		case StmtTxnControl:
			switch wordAt(st.toks, 0) {
			case "BEGIN", "START":
				inTxn = true
				txnBlocking = nil
			case "COMMIT", "END", "ROLLBACK", "ABORT":
				if inTxn && len(txnBlocking) >= 2 {
					add(RuleMultipleDDLInTxn, model.SeverityWarning, strings.Join(txnBlocking, ";\n"),
						fmt.Sprintf("%d lock-taking DDL statements run in one transaction: every lock is held until COMMIT, so the strongest lock is held for the duration of all of them (and locking several tables in one transaction risks deadlocks with application traffic).", len(txnBlocking)),
						"Run each blocking DDL statement in its own short transaction, each guarded by SET lock_timeout.")
				}
				inTxn = false
				txnBlocking = nil
			}
		case StmtSet:
			if st.SetName == "lock_timeout" {
				hasLockTimeout = lockTimeoutValueSet(st.SetValue)
			}
		}

		if inTxn && st.NeedsNoTransaction(0) {
			add(RuleConcurrentlyInTxn, model.SeverityDanger, sql,
				"This statement cannot run inside a transaction block; the migration will fail with \"cannot run inside a transaction block\".",
				"Move it out of the BEGIN ... COMMIT block (most migration tools have a per-migration 'no transaction' switch).")
		}

		if existing && st.TakesBlockingLock() {
			if !hasLockTimeout && !warnedTimeout {
				warnedTimeout = true
				add(RuleMissingLockTimeout, model.SeverityWarning, sql,
					"The migration takes locks that block application queries but never sets lock_timeout. If the lock is not granted immediately (e.g. a long-running report holds a conflicting lock), the DDL waits in the lock queue and every new query on the table queues behind it: an outage that lasts as long as the longest running transaction.",
					"Start the migration with SET lock_timeout = '2s'; so it fails fast instead of blocking everyone, and retry it.")
			}
			if inTxn {
				txnBlocking = append(txnBlocking, sql)
			}
		}

		if existing {
			lintStatement(&st, add)
		}
		state.observe(&st)
	}
	return out
}

func lintStatement(st *Statement, add func(rule string, sev model.Severity, stmt, msg, sugg string)) {
	sql := st.SQL
	switch st.Kind {
	case StmtCreateIndex:
		if !st.Concurrently {
			add(RuleCreateIndexNotConcurrent, model.SeverityDanger, sql,
				fmt.Sprintf("CREATE INDEX takes a ShareLock on %s for the whole index build: all INSERT/UPDATE/DELETE on the table are blocked until it finishes.", st.Table),
				"Use CREATE INDEX CONCURRENTLY (it must run outside a transaction block; if it fails, drop the INVALID index and retry).")
		}
	case StmtDropIndex:
		if !st.Concurrently {
			add(RuleDropIndexNotConcurrent, model.SeverityWarning, sql,
				"DROP INDEX takes an AccessExclusiveLock on the index's table, blocking reads and writes while it waits for and holds the lock.",
				"Use DROP INDEX CONCURRENTLY (outside a transaction block).")
		}
	case StmtVacuum:
		if st.Full {
			add(RuleVacuumFull, model.SeverityDanger, sql,
				"VACUUM FULL rewrites the whole table under an AccessExclusiveLock: reads and writes are blocked for the entire rewrite.",
				"Use plain VACUUM, or rebuild online with pg_repack / pg_squeeze.")
		}
	case StmtCluster:
		add(RuleCluster, model.SeverityDanger, sql,
			"CLUSTER rewrites the table under an AccessExclusiveLock: reads and writes are blocked for the entire rewrite.",
			"Use pg_repack --order-by for an online re-cluster, or skip it.")
	case StmtAlterTable:
		for _, a := range st.Actions {
			lintAction(st, &a, add)
		}
	}
}

func lintAction(st *Statement, a *Action, add func(rule string, sev model.Severity, stmt, msg, sugg string)) {
	sql := st.SQL
	tbl := st.Table
	switch a.Kind {
	case ActAddConstraint:
		switch a.ConstraintType {
		case "FOREIGN KEY":
			if !a.NotValid {
				add(RuleAddFKWithoutNotValid, model.SeverityDanger, sql,
					fmt.Sprintf("Adding a foreign key takes a ShareRowExclusiveLock on both %s and %s and validates every existing row while holding it: writes to both tables are blocked for the full scan.", tbl, a.RefTable),
					"Add the constraint with NOT VALID (instant), then run ALTER TABLE ... VALIDATE CONSTRAINT in a separate transaction (takes only ShareUpdateExclusive / RowShare locks).")
			}
		case "CHECK":
			if !a.NotValid {
				add(RuleAddCheckWithoutNotValid, model.SeverityDanger, sql,
					fmt.Sprintf("Adding a CHECK constraint holds an AccessExclusiveLock on %s while scanning every row: reads and writes are blocked for the full scan.", tbl),
					"Add the constraint with NOT VALID, then VALIDATE CONSTRAINT in a separate transaction.")
			}
		case "UNIQUE":
			if !a.UsingIndex {
				add(RuleAddUniqueConstraint, model.SeverityDanger, sql,
					fmt.Sprintf("Adding a UNIQUE constraint builds its index while holding an AccessExclusiveLock on %s: reads and writes are blocked for the whole build.", tbl),
					"CREATE UNIQUE INDEX CONCURRENTLY first, then ALTER TABLE ... ADD CONSTRAINT name UNIQUE USING INDEX name (instant).")
			}
		case "PRIMARY KEY":
			if !a.UsingIndex {
				add(RuleAddPrimaryKey, model.SeverityDanger, sql,
					fmt.Sprintf("Adding a PRIMARY KEY builds its index (and checks NOT NULL) while holding an AccessExclusiveLock on %s.", tbl),
					"Make the columns NOT NULL safely, CREATE UNIQUE INDEX CONCURRENTLY, then ADD CONSTRAINT name PRIMARY KEY USING INDEX name.")
			}
		}
	case ActSetNotNull:
		add(RuleSetNotNull, model.SeverityDanger, sql,
			fmt.Sprintf("SET NOT NULL on %s.%s holds an AccessExclusiveLock while scanning the whole table to check for NULLs.", tbl, a.Column),
			"On PG12+: ADD CONSTRAINT ... CHECK (col IS NOT NULL) NOT VALID; VALIDATE CONSTRAINT (separate transaction); then SET NOT NULL skips the scan; finally DROP the CHECK.")
	case ActAlterType:
		add(RuleAlterColumnType, model.SeverityDanger, sql,
			fmt.Sprintf("Changing the type of %s.%s usually rewrites the table and all its indexes under an AccessExclusiveLock (reads and writes blocked). Only binary-compatible changes (e.g. varchar(n) -> text or a larger varchar) skip the rewrite.", tbl, a.Column),
			"Add a new column, backfill it in batches, switch the application over, then drop the old column; or keep the change binary-compatible.")
	case ActAddColumn:
		if a.VolatileDefault {
			add(RuleAddColumnVolatileDefault, model.SeverityDanger, sql,
				fmt.Sprintf("Adding column %s with a volatile default (or serial / identity / stored generated column) rewrites all of %s under an AccessExclusiveLock. (Non-volatile defaults such as constants or now() are instant on PG11+.)", a.Column, tbl),
				"Add the column without a default, then SET DEFAULT for new rows, then backfill existing rows in small batches.")
		}
		if a.InlineUnique {
			add(RuleAddUniqueConstraint, model.SeverityWarning, sql,
				fmt.Sprintf("Adding column %s with UNIQUE/PRIMARY KEY builds an index on %s under an AccessExclusiveLock.", a.Column, tbl),
				"Add the plain column, then CREATE UNIQUE INDEX CONCURRENTLY and ADD CONSTRAINT ... USING INDEX.")
		}
	case ActRenameColumn:
		add(RuleRenameColumn, model.SeverityWarning, sql,
			fmt.Sprintf("Renaming %s.%s breaks every running application instance that still uses the old name.", tbl, a.Column),
			"Expand/contract: add the new column, dual-write, backfill, move reads, then drop the old one (or use a view).")
	case ActRenameTable:
		add(RuleRenameTable, model.SeverityWarning, sql,
			fmt.Sprintf("Renaming %s breaks every running application instance that still uses the old name.", tbl),
			"Rename, then create a compatibility view with the old name until all deployments use the new one.")
	case ActDropColumn:
		add(RuleDropColumn, model.SeverityWarning, sql,
			fmt.Sprintf("Dropping %s.%s is instant but breaks running application code (and ORMs that SELECT *) still referencing it.", tbl, a.Column),
			"First deploy code that no longer reads or writes the column, then drop it in a later migration.")
	}
}
