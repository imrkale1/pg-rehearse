package locks

import "strings"

// StmtKind classifies a statement for linting, lock analysis and fixing.
type StmtKind int

const (
	StmtOther StmtKind = iota
	StmtTxnControl
	StmtSet
	StmtCreateTable
	StmtAlterTable
	StmtCreateIndex
	StmtDropIndex
	StmtReindex
	StmtVacuum
	StmtCluster
	StmtDropTable
	StmtTruncate
	StmtLockTable
	StmtCreateTrigger
	StmtRefreshMatView
	StmtAlterTypeAddValue
	StmtNoTxnMisc // CREATE DATABASE, ALTER SYSTEM, ... (cannot run in a txn block)
)

// ActionKind classifies one ALTER TABLE sub-command.
type ActionKind int

const (
	ActOther ActionKind = iota
	ActAddColumn
	ActAddConstraint
	ActAlterType
	ActSetNotNull
	ActDropColumn
	ActRenameColumn
	ActRenameTable
	ActValidate
	ActCheapAlterColumn // SET/DROP DEFAULT, DROP NOT NULL, SET STATISTICS ...
	ActDropConstraint
)

// Action is one comma-separated sub-command of ALTER TABLE.
type Action struct {
	Kind ActionKind
	Text string // exact original text of the action (no trailing comma)

	Column string // column name as written (quotes preserved), if any

	// Constraints (ActAddConstraint):
	ConstraintName string // as written; "" if unnamed
	ConstraintType string // "FOREIGN KEY", "CHECK", "UNIQUE", "PRIMARY KEY", "EXCLUDE"
	NotValid       bool
	UsingIndex     bool
	Columns        []string // column list for UNIQUE / PRIMARY KEY / FOREIGN KEY, as written
	RefTable       string   // FOREIGN KEY ... REFERENCES <RefTable>

	// ADD COLUMN details:
	IfNotExists     bool
	ColumnType      string // type text as written
	Default         string // DEFAULT expression text as written
	VolatileDefault bool   // DEFAULT uses a volatile function or the column is serial/identity/stored-generated
	ColumnRest      string // column constraints other than DEFAULT, as written (may contain NOT NULL)
	InlineUnique    bool   // column definition contains UNIQUE or PRIMARY KEY (builds an index)
	NewName         string // RENAME ... TO <NewName>

	toks []token
}

// Statement is the light-weight parse of one SQL statement.
type Statement struct {
	SQL  string
	Kind StmtKind

	// Table is the target table exactly as written (schema-qualified and
	// quoted as in the source). For CREATE INDEX it is the indexed table; for
	// DROP INDEX / REINDEX INDEX it is the index name.
	Table string
	// Prefix is the original text up to and including the table name, e.g.
	// `ALTER TABLE IF EXISTS ONLY "public"."Orders"`. Used to emit new
	// ALTER TABLE statements against the same table.
	Prefix string

	Actions []Action // ALTER TABLE sub-commands

	Concurrently bool
	Unique       bool   // CREATE UNIQUE INDEX
	IndexName    string // CREATE INDEX name / DROP INDEX name, as written
	Full         bool   // VACUUM FULL
	SetName      string // SET <name> (lowercase)
	SetValue     string // SET name = <value>, as written
	MultiTarget  bool   // DROP INDEX a, b / statements naming several objects
	Cascade      bool

	toks []token // non-comment tokens
}

// NormIdent turns a possibly-qualified identifier as written into a canonical
// lowercase/unquoted form ("public"."Orders" -> public.Orders, Orders -> orders).
func NormIdent(s string) string {
	parts := splitQualified(s)
	for i, p := range parts {
		parts[i] = normPart(p)
	}
	return strings.Join(parts, ".")
}

// BareName returns the last component of a qualified identifier, normalized.
func BareName(s string) string {
	parts := splitQualified(s)
	if len(parts) == 0 {
		return ""
	}
	return normPart(parts[len(parts)-1])
}

func normPart(p string) string {
	p = strings.TrimSpace(p)
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		return strings.ReplaceAll(p[1:len(p)-1], `""`, `"`)
	}
	return strings.ToLower(p)
}

func splitQualified(s string) []string {
	var parts []string
	for _, t := range lex(s) {
		switch t.Kind {
		case tkWord, tkQIdent:
			parts = append(parts, t.Text)
		}
	}
	return parts
}

func nonComment(toks []token) []token {
	out := toks[:0:0]
	for _, t := range toks {
		if t.Kind != tkComment {
			out = append(out, t)
		}
	}
	return out
}

// isIdentTok reports whether t can be (part of) an identifier.
func isIdentTok(t token) bool { return t.Kind == tkWord || t.Kind == tkQIdent }

// readQualified reads ident(.ident)* starting at toks[i]; returns the end index (exclusive).
func readQualified(toks []token, i int) int {
	if i >= len(toks) || !isIdentTok(toks[i]) {
		return i
	}
	j := i + 1
	for j+1 < len(toks) && toks[j].Kind == tkOp && toks[j].Text == "." && isIdentTok(toks[j+1]) {
		j += 2
	}
	return j
}

func (st *Statement) text(from, to int) string {
	if from >= to || from >= len(st.toks) {
		return ""
	}
	return st.SQL[st.toks[from].Pos:st.toks[to-1].End]
}

func wordAt(toks []token, i int) string {
	if i < len(toks) {
		return toks[i].upper()
	}
	return ""
}

func isOp(toks []token, i int, op string) bool {
	return i < len(toks) && toks[i].Kind == tkOp && toks[i].Text == op
}

// Parse classifies a single statement (as returned by SplitStatements).
func Parse(sql string) Statement {
	st := Statement{SQL: sql, toks: nonComment(lex(sql))}
	t := st.toks
	w := func(i int) string { return wordAt(t, i) }
	if len(t) == 0 {
		return st
	}
	switch w(0) {
	case "BEGIN", "COMMIT", "END", "ROLLBACK", "ABORT", "SAVEPOINT", "RELEASE", "PREPARE":
		if w(0) == "PREPARE" && w(1) != "TRANSACTION" {
			return st
		}
		st.Kind = StmtTxnControl
	case "START":
		if w(1) == "TRANSACTION" {
			st.Kind = StmtTxnControl
		}
	case "SET":
		st.Kind = StmtSet
		i := 1
		if w(i) == "LOCAL" || w(i) == "SESSION" {
			i++
		}
		if i < len(t) {
			st.SetName = strings.ToLower(t[i].Text)
			j := i + 1
			if isOp(t, j, "=") || w(j) == "TO" {
				j++
			}
			st.SetValue = st.text(j, len(t))
		}
	case "CREATE":
		st.parseCreate()
	case "ALTER":
		switch w(1) {
		case "TABLE":
			st.parseAlterTable()
		case "TYPE":
			for i := 2; i+1 < len(t); i++ {
				if w(i) == "ADD" && w(i+1) == "VALUE" {
					st.Kind = StmtAlterTypeAddValue
				}
			}
		case "SYSTEM":
			st.Kind = StmtNoTxnMisc
		case "DATABASE":
			if w(3) == "SET" && w(4) == "TABLESPACE" {
				st.Kind = StmtNoTxnMisc
			}
		}
	case "DROP":
		switch w(1) {
		case "INDEX":
			st.Kind = StmtDropIndex
			i := 2
			if w(i) == "CONCURRENTLY" {
				st.Concurrently = true
				i++
			}
			if w(i) == "IF" && w(i+1) == "EXISTS" {
				i += 2
			}
			j := readQualified(t, i)
			st.IndexName = st.text(i, j)
			st.Table = st.IndexName
			st.Prefix = st.text(0, j)
			if isOp(t, j, ",") {
				st.MultiTarget = true
			}
			for k := j; k < len(t); k++ {
				if w(k) == "CASCADE" {
					st.Cascade = true
				}
			}
		case "TABLE":
			st.Kind = StmtDropTable
			i := 2
			if w(i) == "IF" && w(i+1) == "EXISTS" {
				i += 2
			}
			j := readQualified(t, i)
			st.Table = st.text(i, j)
		case "DATABASE", "TABLESPACE":
			st.Kind = StmtNoTxnMisc
		}
	case "REINDEX":
		st.Kind = StmtReindex
		i := 1
		if isOp(t, i, "(") {
			i = skipParens(t, i)
		}
		if w(i) == "INDEX" || w(i) == "TABLE" || w(i) == "SCHEMA" || w(i) == "DATABASE" || w(i) == "SYSTEM" {
			st.SetName = strings.ToLower(t[i].Text) // target type
			i++
		}
		if w(i) == "CONCURRENTLY" {
			st.Concurrently = true
			i++
		}
		j := readQualified(t, i)
		st.Table = st.text(i, j)
	case "VACUUM":
		st.Kind = StmtVacuum
		i := 1
		if isOp(t, i, "(") {
			end := skipParens(t, i)
			for k := i + 1; k < end-1; k++ {
				if w(k) == "FULL" {
					// FULL [boolean]
					if v := w(k + 1); v == "FALSE" || v == "OFF" || (k+1 < len(t) && t[k+1].Text == "0") {
						continue
					}
					st.Full = true
				}
			}
			i = end
		} else {
			for w(i) == "FULL" || w(i) == "FREEZE" || w(i) == "VERBOSE" || w(i) == "ANALYZE" || w(i) == "ANALYSE" {
				if w(i) == "FULL" {
					st.Full = true
				}
				i++
			}
		}
		j := readQualified(t, i)
		st.Table = st.text(i, j)
		if isOp(t, j, ",") || (isOp(t, j, "(") && isOp(t, skipParens(t, j), ",")) {
			st.MultiTarget = true
		}
	case "CLUSTER":
		st.Kind = StmtCluster
		i := 1
		if isOp(t, i, "(") {
			i = skipParens(t, i)
		}
		if w(i) == "VERBOSE" {
			i++
		}
		j := readQualified(t, i)
		st.Table = st.text(i, j)
	case "TRUNCATE":
		st.Kind = StmtTruncate
		i := 1
		if w(i) == "TABLE" {
			i++
		}
		if w(i) == "ONLY" {
			i++
		}
		j := readQualified(t, i)
		st.Table = st.text(i, j)
	case "LOCK":
		st.Kind = StmtLockTable
		i := 1
		if w(i) == "TABLE" {
			i++
		}
		if w(i) == "ONLY" {
			i++
		}
		j := readQualified(t, i)
		st.Table = st.text(i, j)
	case "REFRESH":
		if w(1) == "MATERIALIZED" && w(2) == "VIEW" {
			st.Kind = StmtRefreshMatView
			i := 3
			if w(i) == "CONCURRENTLY" {
				st.Concurrently = true
				i++
			}
			j := readQualified(t, i)
			st.Table = st.text(i, j)
		}
	}
	return st
}

func skipParens(t []token, i int) int {
	depth := 0
	for ; i < len(t); i++ {
		if t[i].Kind == tkOp {
			switch t[i].Text {
			case "(", "[":
				depth++
			case ")", "]":
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
	}
	return i
}

func (st *Statement) parseCreate() {
	t := st.toks
	w := func(i int) string { return wordAt(t, i) }
	i := 1
	if w(i) == "OR" && w(i+1) == "REPLACE" {
		i += 2
	}
	switch w(i) {
	case "DATABASE", "TABLESPACE":
		st.Kind = StmtNoTxnMisc
		return
	}
	// CREATE [GLOBAL|LOCAL] [TEMP|TEMPORARY|UNLOGGED] TABLE
	j := i
	for w(j) == "GLOBAL" || w(j) == "LOCAL" || w(j) == "TEMP" || w(j) == "TEMPORARY" || w(j) == "UNLOGGED" {
		j++
	}
	if w(j) == "TABLE" {
		st.Kind = StmtCreateTable
		j++
		if w(j) == "IF" && w(j+1) == "NOT" && w(j+2) == "EXISTS" {
			j += 3
		}
		e := readQualified(t, j)
		st.Table = st.text(j, e)
		return
	}
	if w(i) == "UNIQUE" {
		st.Unique = true
		i++
	}
	if w(i) == "INDEX" {
		st.Kind = StmtCreateIndex
		i++
		if w(i) == "CONCURRENTLY" {
			st.Concurrently = true
			i++
		}
		if w(i) == "IF" && w(i+1) == "NOT" && w(i+2) == "EXISTS" {
			i += 3
		}
		if w(i) != "ON" {
			e := readQualified(t, i)
			st.IndexName = st.text(i, e)
			i = e
		}
		if w(i) == "ON" {
			i++
			if w(i) == "ONLY" {
				i++
			}
			e := readQualified(t, i)
			st.Table = st.text(i, e)
		}
		return
	}
	// CREATE [CONSTRAINT] TRIGGER name ... ON table
	if w(i) == "CONSTRAINT" {
		i++
	}
	if w(i) == "TRIGGER" {
		st.Kind = StmtCreateTrigger
		for k := i; k < len(t); k++ {
			if w(k) == "ON" {
				e := readQualified(t, k+1)
				st.Table = st.text(k+1, e)
				break
			}
		}
	}
}

func (st *Statement) parseAlterTable() {
	t := st.toks
	w := func(i int) string { return wordAt(t, i) }
	st.Kind = StmtAlterTable
	i := 2
	if w(i) == "ALL" {
		return // ALTER TABLE ALL IN TABLESPACE
	}
	if w(i) == "IF" && w(i+1) == "EXISTS" {
		i += 2
	}
	if w(i) == "ONLY" {
		i++
	}
	e := readQualified(t, i)
	st.Table = st.text(i, e)
	if isOp(t, e, "*") {
		e++
	}
	st.Prefix = st.text(0, e)
	// split actions on top-level commas
	depth := 0
	start := e
	for k := e; k <= len(t); k++ {
		if k == len(t) || (depth == 0 && isOp(t, k, ",")) {
			if k > start {
				st.Actions = append(st.Actions, st.parseAction(t[start:k]))
			}
			start = k + 1
			continue
		}
		if t[k].Kind == tkOp {
			switch t[k].Text {
			case "(", "[":
				depth++
			case ")", "]":
				depth--
			}
		}
	}
}

var volatileFuncs = map[string]bool{
	"RANDOM": true, "CLOCK_TIMESTAMP": true, "TIMEOFDAY": true, "GEN_RANDOM_UUID": true,
	"UUID_GENERATE_V1": true, "UUID_GENERATE_V1MC": true, "UUID_GENERATE_V4": true,
	"UUIDV4": true, "UUIDV7": true, "UUID_GENERATE_V7": true, "NEXTVAL": true, "SETSEED": true,
	"STATEMENT_TIMESTAMP": false, // stable within a statement: evaluated once -> not volatile
	"TXID_CURRENT":        true, "PG_CURRENT_XACT_ID": true, "RANDOM_NORMAL": true,
}

var serialTypes = map[string]bool{"SERIAL": true, "BIGSERIAL": true, "SMALLSERIAL": true, "SERIAL2": true, "SERIAL4": true, "SERIAL8": true}

// columnConstraintStart lists words that end the type of a column definition.
var columnConstraintStart = map[string]bool{
	"DEFAULT": true, "NOT": true, "NULL": true, "CONSTRAINT": true, "CHECK": true, "UNIQUE": true,
	"PRIMARY": true, "REFERENCES": true, "GENERATED": true, "COLLATE": true, "COMPRESSION": true,
	"STORAGE": true, "DEFERRABLE": true, "INITIALLY": true,
}

func (st *Statement) parseAction(t []token) Action {
	a := Action{toks: t, Text: st.SQL[t[0].Pos:t[len(t)-1].End]}
	w := func(i int) string { return wordAt(t, i) }
	txt := func(from, to int) string {
		if from >= to || from >= len(t) {
			return ""
		}
		if to > len(t) {
			to = len(t)
		}
		return st.SQL[t[from].Pos:t[to-1].End]
	}
	switch w(0) {
	case "ADD":
		i := 1
		switch w(1) {
		case "CONSTRAINT", "PRIMARY", "UNIQUE", "FOREIGN", "CHECK", "EXCLUDE":
			a.Kind = ActAddConstraint
			if w(1) == "CONSTRAINT" {
				e := i + 2
				a.ConstraintName = txt(i+1, e)
				i = e
			}
			switch w(i) {
			case "PRIMARY":
				a.ConstraintType = "PRIMARY KEY"
				i += 2
			case "UNIQUE":
				a.ConstraintType = "UNIQUE"
				i++
			case "FOREIGN":
				a.ConstraintType = "FOREIGN KEY"
				i += 2
			case "CHECK":
				a.ConstraintType = "CHECK"
				i++
			case "EXCLUDE":
				a.ConstraintType = "EXCLUDE"
				i++
			}
			if w(i) == "USING" && w(i+1) == "INDEX" {
				a.UsingIndex = true
			}
			// skip NULLS [NOT] DISTINCT to reach the column list
			k := i
			for k < len(t) && !isOp(t, k, "(") && w(k) != "USING" {
				k++
			}
			if isOp(t, k, "(") && a.ConstraintType != "CHECK" && a.ConstraintType != "EXCLUDE" {
				end := skipParens(t, k)
				a.Columns = splitTopLevel(st.SQL, t[k+1:end-1])
				if a.ConstraintType == "FOREIGN KEY" && w(end) == "REFERENCES" {
					e := readQualified(t, end+1)
					a.RefTable = txt(end+1, e)
				}
			}
			n := len(t)
			if n >= 2 && w(n-2) == "NOT" && w(n-1) == "VALID" {
				a.NotValid = true
			}
		default:
			a.Kind = ActAddColumn
			if w(i) == "COLUMN" {
				i++
			}
			if w(i) == "IF" && w(i+1) == "NOT" && w(i+2) == "EXISTS" {
				a.IfNotExists = true
				i += 3
			}
			a.Column = txt(i, i+1)
			i++
			// type: until a column-constraint keyword at depth 0
			ts := i
			depth := 0
			for i < len(t) {
				if t[i].Kind == tkOp && (t[i].Text == "(" || t[i].Text == "[") {
					depth++
				} else if t[i].Kind == tkOp && (t[i].Text == ")" || t[i].Text == "]") {
					depth--
				} else if depth == 0 && columnConstraintStart[w(i)] {
					break
				}
				i++
			}
			a.ColumnType = txt(ts, i)
			if len(t) > ts && serialTypes[w(ts)] {
				a.VolatileDefault = true
			}
			// constraints
			var rest []string
			for i < len(t) {
				// find the end of this constraint clause
				j := i + 1
				depth := 0
				for j < len(t) {
					if t[j].Kind == tkOp && t[j].Text == "(" {
						depth++
					} else if t[j].Kind == tkOp && t[j].Text == ")" {
						depth--
					} else if depth == 0 && w(i) != "GENERATED" && columnConstraintStart[w(j)] &&
						!(w(j) == "NULL" && w(j-1) == "NOT") &&
						!((w(j) == "NULL" || w(j) == "DEFAULT") && w(j-1) == "SET") {
						break
					}
					j++
				}
				clause := txt(i, j)
				switch w(i) {
				case "DEFAULT":
					a.Default = txt(i+1, j)
					for k := i + 1; k < j; k++ {
						if t[k].Kind == tkWord && volatileFuncs[t[k].upper()] && isOp(t, k+1, "(") {
							a.VolatileDefault = true
						}
					}
				case "GENERATED":
					up := strings.ToUpper(clause)
					if strings.Contains(up, "IDENTITY") || strings.Contains(up, "STORED") {
						a.VolatileDefault = true
					}
					rest = append(rest, clause)
				case "UNIQUE", "PRIMARY":
					a.InlineUnique = true
					rest = append(rest, clause)
				default:
					rest = append(rest, clause)
				}
				i = j
			}
			a.ColumnRest = strings.Join(rest, " ")
		}
	case "ALTER":
		i := 1
		if w(i) == "COLUMN" {
			i++
		}
		if w(i) == "CONSTRAINT" { // ALTER CONSTRAINT ... DEFERRABLE
			return a
		}
		a.Column = txt(i, i+1)
		i++
		switch {
		case w(i) == "TYPE" || (w(i) == "SET" && w(i+1) == "DATA" && w(i+2) == "TYPE"):
			a.Kind = ActAlterType
			if w(i) == "SET" {
				i += 3
			} else {
				i++
			}
			a.ColumnType = txt(i, len(t))
		case w(i) == "SET" && w(i+1) == "NOT" && w(i+2) == "NULL":
			a.Kind = ActSetNotNull
		case w(i) == "ADD" && w(i+1) == "GENERATED":
			a.Kind = ActOther
		default:
			a.Kind = ActCheapAlterColumn
		}
	case "DROP":
		i := 1
		if w(i) == "CONSTRAINT" {
			a.Kind = ActDropConstraint
			return a
		}
		if w(i) == "COLUMN" {
			i++
		}
		if w(i) == "IF" && w(i+1) == "EXISTS" {
			i += 2
		}
		a.Kind = ActDropColumn
		a.Column = txt(i, i+1)
	case "RENAME":
		i := 1
		switch w(i) {
		case "TO":
			a.Kind = ActRenameTable
			a.NewName = txt(2, len(t))
		case "CONSTRAINT":
			a.Kind = ActOther
		default:
			if w(i) == "COLUMN" {
				i++
			}
			a.Kind = ActRenameColumn
			a.Column = txt(i, i+1)
			if w(i+1) == "TO" {
				a.NewName = txt(i+2, len(t))
			}
		}
	case "VALIDATE":
		a.Kind = ActValidate
	}
	return a
}

// splitTopLevel splits tokens on depth-0 commas and returns each part's original text.
func splitTopLevel(src string, t []token) []string {
	var out []string
	depth, start := 0, 0
	for k := 0; k <= len(t); k++ {
		if k == len(t) || (depth == 0 && t[k].Kind == tkOp && t[k].Text == ",") {
			if k > start {
				out = append(out, src[t[start].Pos:t[k-1].End])
			}
			start = k + 1
			continue
		}
		if t[k].Kind == tkOp && t[k].Text == "(" {
			depth++
		} else if t[k].Kind == tkOp && t[k].Text == ")" {
			depth--
		}
	}
	return out
}

// NeedsNoTransaction reports whether the statement cannot run inside a
// transaction block on the given server version (server_version_num).
func (st *Statement) NeedsNoTransaction(serverVersionNum int) bool {
	switch st.Kind {
	case StmtCreateIndex, StmtDropIndex, StmtReindex:
		return st.Concurrently
	case StmtVacuum, StmtNoTxnMisc:
		return true
	case StmtCluster:
		return st.Table == ""
	case StmtAlterTypeAddValue:
		return serverVersionNum > 0 && serverVersionNum < 120000
	}
	return false
}

// TakesBlockingLock reports whether the statement takes a lock that conflicts
// with ordinary reads or writes (so it can queue application traffic behind
// it). VALIDATE CONSTRAINT and CONCURRENTLY operations do not.
func (st *Statement) TakesBlockingLock() bool {
	switch st.Kind {
	case StmtAlterTable:
		if len(st.Actions) == 0 {
			return true
		}
		for _, a := range st.Actions {
			if a.Kind != ActValidate {
				return true
			}
		}
		return false
	case StmtCreateIndex, StmtDropIndex, StmtReindex, StmtRefreshMatView:
		return !st.Concurrently
	case StmtVacuum:
		return st.Full
	case StmtCluster, StmtDropTable, StmtTruncate, StmtLockTable, StmtCreateTrigger:
		return true
	}
	return false
}
