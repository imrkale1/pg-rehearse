package load

import (
	"fmt"
	"strconv"
	"strings"
)

// This file contains a small, dependency-free SQL scanner used to infer what
// each $n placeholder of a normalized (pg_stat_statements) query means.
// It understands a deliberately narrow set of patterns; anything it does not
// understand makes the query "not confidently parameterizable" and it is
// skipped rather than guessed at.

type tokKind int

const (
	tIdent tokKind = iota // identifier or keyword (lower-cased unless quoted)
	tParam                // $n
	tNumber
	tString
	tOp // punctuation / operator
)

type token struct {
	kind   tokKind
	text   string
	quoted bool   // quoted identifier
	param  int    // for tParam
	cast   string // for tParam: type of a directly following ::cast, if any
}

func (t token) is(s string) bool {
	return (t.kind == tIdent && !t.quoted || t.kind == tOp) && t.text == s
}

// tokenize splits SQL into tokens, skipping whitespace and comments.
func tokenize(sql string) ([]token, error) {
	var toks []token
	i, n := 0, len(sql)
	for i < n {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < n && sql[i+1] == '-':
			for i < n && sql[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && sql[i+1] == '*':
			depth := 0
			for i < n {
				if i+1 < n && sql[i] == '/' && sql[i+1] == '*' {
					depth++
					i += 2
				} else if i+1 < n && sql[i] == '*' && sql[i+1] == '/' {
					depth--
					i += 2
					if depth == 0 {
						break
					}
				} else {
					i++
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("unterminated comment")
			}
		case c == '\'' || ((c == 'E' || c == 'e') && i+1 < n && sql[i+1] == '\''):
			if c != '\'' {
				i++
			}
			j := i + 1
			for {
				if j >= n {
					return nil, fmt.Errorf("unterminated string")
				}
				if sql[j] == '\\' && c != '\'' {
					j += 2
					continue
				}
				if sql[j] == '\'' {
					if j+1 < n && sql[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			toks = append(toks, token{kind: tString, text: sql[i : j+1]})
			i = j + 1
		case c == '"':
			j := i + 1
			var b strings.Builder
			for {
				if j >= n {
					return nil, fmt.Errorf("unterminated quoted identifier")
				}
				if sql[j] == '"' {
					if j+1 < n && sql[j+1] == '"' {
						b.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(sql[j])
				j++
			}
			toks = append(toks, token{kind: tIdent, text: b.String(), quoted: true})
			i = j + 1
		case c == '$':
			j := i + 1
			for j < n && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			if j > i+1 && (j >= n || !isIdentChar(sql[j])) {
				p, _ := strconv.Atoi(sql[i+1 : j])
				toks = append(toks, token{kind: tParam, text: sql[i:j], param: p})
				i = j
				continue
			}
			// dollar-quoted string: $tag$ ... $tag$
			k := i + 1
			for k < n && isIdentChar(sql[k]) {
				k++
			}
			if k >= n || sql[k] != '$' {
				return nil, fmt.Errorf("unexpected '$' at %d", i)
			}
			tag := sql[i : k+1]
			end := strings.Index(sql[k+1:], tag)
			if end < 0 {
				return nil, fmt.Errorf("unterminated dollar-quoted string")
			}
			stop := k + 1 + end + len(tag)
			toks = append(toks, token{kind: tString, text: sql[i:stop]})
			i = stop
		case c >= '0' && c <= '9' || (c == '.' && i+1 < n && sql[i+1] >= '0' && sql[i+1] <= '9'):
			j := i
			for j < n && (sql[j] >= '0' && sql[j] <= '9' || sql[j] == '.' || sql[j] == 'e' || sql[j] == 'E' || sql[j] == '_') {
				j++
			}
			toks = append(toks, token{kind: tNumber, text: sql[i:j]})
			i = j
		case isIdentStart(c):
			j := i
			for j < n && isIdentChar(sql[j]) {
				j++
			}
			toks = append(toks, token{kind: tIdent, text: strings.ToLower(sql[i:j])})
			i = j
		default:
			if i+1 < n {
				two := sql[i : i+2]
				switch two {
				case "::", "<=", ">=", "<>", "!=", "||", "->", "~~":
					if two == "->" && i+2 < n && sql[i+2] == '>' {
						toks = append(toks, token{kind: tOp, text: "->>"})
						i += 3
						continue
					}
					toks = append(toks, token{kind: tOp, text: two})
					i += 2
					continue
				}
			}
			toks = append(toks, token{kind: tOp, text: string(c)})
			i++
		}
	}
	return stripCasts(toks), nil
}

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}
func isIdentChar(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' || c == '$' }

var typeWords = map[string]bool{"with": true, "without": true, "time": true, "zone": true, "precision": true, "varying": true}

// stripCasts removes "::type" sequences, remembering the type on a param they follow.
func stripCasts(toks []token) []token {
	out := toks[:0:0]
	for i := 0; i < len(toks); i++ {
		if !toks[i].is("::") {
			out = append(out, toks[i])
			continue
		}
		j := i + 1
		var typ []string
		// optional schema qualification
		if j+2 < len(toks) && toks[j].kind == tIdent && toks[j+1].is(".") {
			j += 2
		}
		if j < len(toks) && toks[j].kind == tIdent {
			typ = append(typ, toks[j].text)
			j++
			for j < len(toks) && toks[j].kind == tIdent && !toks[j].quoted && typeWords[toks[j].text] {
				typ = append(typ, toks[j].text)
				j++
			}
		}
		if j < len(toks) && toks[j].is("(") {
			for j < len(toks) && !toks[j].is(")") {
				j++
			}
			j++
		}
		for j+1 < len(toks) && toks[j].is("[") && toks[j+1].is("]") {
			typ = append(typ, "[]")
			j += 2
		}
		if len(out) > 0 && out[len(out)-1].kind == tParam && out[len(out)-1].cast == "" {
			out[len(out)-1].cast = strings.Join(typ, " ")
		}
		i = j - 1
	}
	return out
}

// paramKind is what a placeholder was inferred to be.
type paramKind int

const (
	pkColumn paramKind = iota // compared to / assigned to / inserted into a column
	pkLimit
	pkOffset
	pkCast // only a cast type is known
)

// paramRole refines pkColumn usage.
type paramRole int

const (
	roleLookup paramRole = iota // WHERE col = $1, IN, BETWEEN, range: sample existing values (skewed)
	roleAssign                  // UPDATE ... SET col = $1: realistic existing values, fresh if unique
	roleInsert                  // INSERT VALUES: fresh if unique, parent key if FK, else realistic
)

type paramRef struct {
	Index int
	Kind  paramKind
	Role  paramRole
	// Column reference: Qualifier is a table alias/name (may be empty), Column the column name.
	Qualifier string
	Column    string
	// For INSERT positional refs with no explicit column list.
	InsertPos int
	Cast      string
}

type tableRef struct {
	Schema, Name, Alias string
}

type parsedQuery struct {
	Tables    []tableRef
	Params    map[int]paramRef
	MaxParam  int
	IsWrite   bool
	InsertTbl *tableRef // target of INSERT, if any
}

var notAlias = map[string]bool{
	"where": true, "join": true, "inner": true, "left": true, "right": true, "full": true, "cross": true,
	"natural": true, "on": true, "using": true, "set": true, "values": true, "group": true, "order": true,
	"limit": true, "offset": true, "having": true, "union": true, "except": true, "intersect": true,
	"returning": true, "for": true, "default": true, "select": true, "window": true, "tablesample": true,
	"outer": true, "lateral": true, "fetch": true, "overriding": true, "do": true,
}

var compareOps = map[string]bool{"=": true, "<>": true, "!=": true, "<": true, ">": true, "<=": true, ">=": true, "like": true, "ilike": true, "~~": true}

// parseQuery tokenizes sql and infers the meaning of each placeholder.
// Placeholders it cannot explain are simply absent from Params.
func parseQuery(sql string) (*parsedQuery, error) {
	toks, err := tokenize(sql)
	if err != nil {
		return nil, err
	}
	pq := &parsedQuery{Params: map[int]paramRef{}}
	if len(toks) > 0 && toks[0].kind == tIdent {
		switch toks[0].text {
		case "insert", "update", "delete", "merge":
			pq.IsWrite = true
		}
	}

	// Pass 1: table references and their aliases; INSERT column list; SET ranges.
	type span struct{ from, to int }
	var setSpans []span
	insertCols := map[int]string{} // token index of param -> column (from VALUES position)
	insertPos := map[int]int{}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tIdent || t.quoted {
			if t.kind == tParam && t.param > pq.MaxParam {
				pq.MaxParam = t.param
			}
			continue
		}
		switch t.text {
		case "from", "join", "update", "into", "using":
			if t.text == "using" && (i+1 >= len(toks) || toks[i+1].is("(")) {
				continue // JOIN ... USING (col)
			}
			j := i + 1
			for {
				if j < len(toks) && toks[j].is("only") {
					j++
				}
				tr, next, ok := readTableRef(toks, j, t.text == "into")
				if !ok {
					break
				}
				pq.Tables = append(pq.Tables, tr)
				if t.text == "into" && i > 0 && toks[i-1].is("insert") {
					trc := tr
					pq.InsertTbl = &trc
					parseInsert(toks, next, insertCols, insertPos)
				}
				j = next
				if t.text == "from" && j < len(toks) && toks[j].is(",") {
					j++
					continue
				}
				break
			}
		case "set":
			// UPDATE t SET ... or ON CONFLICT DO UPDATE SET ...
			if i > 0 {
				depth := 0
				k := i + 1
				for ; k < len(toks); k++ {
					if toks[k].is("(") {
						depth++
					} else if toks[k].is(")") {
						if depth == 0 {
							break
						}
						depth--
					} else if depth == 0 && (toks[k].is("where") || toks[k].is("returning") || toks[k].is("from")) {
						break
					}
				}
				setSpans = append(setSpans, span{i, k})
			}
		}
	}
	inSet := func(i int) bool {
		for _, s := range setSpans {
			if i > s.from && i < s.to {
				return true
			}
		}
		return false
	}

	// Pass 2: each placeholder occurrence.
	for i, t := range toks {
		if t.kind != tParam {
			continue
		}
		if _, done := pq.Params[t.param]; done {
			continue
		}
		ref, ok := inferAt(toks, i)
		if ok {
			if ref.Kind == pkColumn && ref.Role == roleLookup && inSet(i) {
				ref.Role = roleAssign
			}
		} else if col, has := insertCols[i]; has {
			ref, ok = paramRef{Kind: pkColumn, Role: roleInsert, Column: col}, true
			if pq.InsertTbl != nil {
				ref.Qualifier = pq.InsertTbl.Alias
				if ref.Qualifier == "" {
					ref.Qualifier = pq.InsertTbl.Name
				}
			}
		} else if pos, has := insertPos[i]; has {
			ref, ok = paramRef{Kind: pkColumn, Role: roleInsert, InsertPos: pos + 1}, true
		} else if t.cast != "" {
			ref, ok = paramRef{Kind: pkCast}, true
		}
		if ok {
			ref.Index = t.param
			ref.Cast = t.cast
			pq.Params[t.param] = ref
		}
	}
	return pq, nil
}

// readTableRef reads `[schema.]name [[AS] alias]` starting at j.
func readTableRef(toks []token, j int, columnList bool) (tableRef, int, bool) {
	if j >= len(toks) || toks[j].kind != tIdent || (!toks[j].quoted && (notAlias[toks[j].text] || toks[j].text == "lateral")) {
		return tableRef{}, j, false
	}
	var tr tableRef
	tr.Name = toks[j].text
	j++
	if j+1 < len(toks) && toks[j].is(".") && toks[j+1].kind == tIdent {
		tr.Schema, tr.Name = tr.Name, toks[j+1].text
		j += 2
	}
	if j < len(toks) && toks[j].is("(") {
		if columnList {
			return tr, j, true // INSERT INTO t (cols)
		}
		return tableRef{}, j, false // function call in FROM
	}
	if j < len(toks) && toks[j].is("as") {
		j++
		if j < len(toks) && toks[j].kind == tIdent {
			tr.Alias = toks[j].text
			j++
		}
	} else if j < len(toks) && toks[j].kind == tIdent && (toks[j].quoted || !notAlias[toks[j].text]) {
		tr.Alias = toks[j].text
		j++
	}
	return tr, j, true
}

// parseInsert handles `(cols) VALUES (..), (..)` after the INSERT target.
func parseInsert(toks []token, j int, cols map[int]string, pos map[int]int) {
	var names []string
	if j < len(toks) && toks[j].is("(") {
		j++
		for j < len(toks) && !toks[j].is(")") {
			if toks[j].kind == tIdent {
				names = append(names, toks[j].text)
			}
			j++
		}
		j++
	}
	for j < len(toks) && toks[j].kind == tIdent && !toks[j].is("values") {
		j++ // OVERRIDING SYSTEM VALUE etc.
	}
	if j >= len(toks) || !toks[j].is("values") {
		return
	}
	j++
	for j < len(toks) && toks[j].is("(") {
		j++
		k, depth := 0, 0
		elemStart := j
		for j < len(toks) {
			tk := toks[j]
			if tk.is("(") {
				depth++
			} else if tk.is(")") {
				if depth == 0 {
					break
				}
				depth--
			} else if depth == 0 && tk.is(",") {
				k++
				elemStart = j + 1
			} else if tk.kind == tParam && depth == 0 && j == elemStart &&
				(j+1 < len(toks) && (toks[j+1].is(",") || toks[j+1].is(")"))) {
				if names != nil {
					if k < len(names) {
						cols[j] = names[k]
					}
				} else {
					pos[j] = k
				}
			}
			j++
		}
		j++ // past ')'
		if j < len(toks) && toks[j].is(",") {
			j++
			continue
		}
		break
	}
}

// colRefBefore reads a column reference ending at index end (inclusive).
func colRefBefore(toks []token, end int) (qual, col string, ok bool) {
	if end < 0 || toks[end].kind != tIdent {
		return "", "", false
	}
	if !toks[end].quoted && (notAlias[toks[end].text] || toks[end].text == "and" || toks[end].text == "or" || toks[end].text == "not") {
		return "", "", false
	}
	col = toks[end].text
	if end >= 2 && toks[end-1].is(".") && toks[end-2].kind == tIdent {
		qual = toks[end-2].text
	}
	return qual, col, true
}

// colRefAfter reads a column reference starting at index start.
func colRefAfter(toks []token, start int) (qual, col string, ok bool) {
	if start >= len(toks) || toks[start].kind != tIdent {
		return "", "", false
	}
	if start+2 < len(toks) && toks[start+1].is(".") && toks[start+2].kind == tIdent {
		if start+3 < len(toks) && (toks[start+3].is("(") || toks[start+3].is(".")) {
			return "", "", false
		}
		return toks[start].text, toks[start+2].text, true
	}
	if start+1 < len(toks) && (toks[start+1].is("(") || toks[start+1].is(".")) {
		return "", "", false
	}
	if !toks[start].quoted && notAlias[toks[start].text] {
		return "", "", false
	}
	return "", toks[start].text, true
}

func opText(t token) string {
	if t.kind == tOp || (t.kind == tIdent && !t.quoted) {
		return t.text
	}
	return ""
}

// inferAt explains the param at toks[i] from its immediate context.
func inferAt(toks []token, i int) (paramRef, bool) {
	prev := func(k int) token {
		if i-k >= 0 {
			return toks[i-k]
		}
		return token{kind: tOp}
	}
	next := func(k int) token {
		if i+k < len(toks) {
			return toks[i+k]
		}
		return token{kind: tOp}
	}
	switch {
	case prev(1).is("limit"):
		return paramRef{Kind: pkLimit}, true
	case prev(1).is("offset"):
		return paramRef{Kind: pkOffset}, true
	case prev(1).is("fetch") || (prev(1).is("next") || prev(1).is("first")) && prev(2).is("fetch"):
		return paramRef{Kind: pkLimit}, true
	}
	// col <op> $n   (also `col NOT LIKE $n`)
	if op := opText(prev(1)); compareOps[op] {
		end := i - 2
		if end >= 0 && toks[end].is("not") {
			end--
		}
		if q, c, ok := colRefBefore(toks, end); ok {
			return paramRef{Kind: pkColumn, Role: roleLookup, Qualifier: q, Column: c}, true
		}
	}
	// $n <op> col
	if op := opText(next(1)); compareOps[op] && op != "like" && op != "ilike" {
		if q, c, ok := colRefAfter(toks, i+2); ok {
			return paramRef{Kind: pkColumn, Role: roleLookup, Qualifier: q, Column: c}, true
		}
	}
	// col BETWEEN $n AND $m
	if prev(1).is("between") {
		if q, c, ok := colRefBefore(toks, i-2); ok {
			return paramRef{Kind: pkColumn, Role: roleLookup, Qualifier: q, Column: c}, true
		}
	}
	if prev(1).is("and") && prev(3).is("between") {
		if q, c, ok := colRefBefore(toks, i-4); ok {
			return paramRef{Kind: pkColumn, Role: roleLookup, Qualifier: q, Column: c}, true
		}
	}
	// col [NOT] IN ($a, $b, ...)
	j := i
	for j-2 >= 0 && toks[j-1].is(",") && toks[j-2].kind == tParam {
		j -= 2
	}
	if j-2 >= 0 && toks[j-1].is("(") && toks[j-2].is("in") {
		end := j - 3
		if end >= 0 && toks[end].is("not") {
			end--
		}
		if q, c, ok := colRefBefore(toks, end); ok {
			return paramRef{Kind: pkColumn, Role: roleLookup, Qualifier: q, Column: c}, true
		}
	}
	return paramRef{}, false
}
