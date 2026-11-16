package profile

import (
	"regexp"
	"sort"
	"strings"
)

// sqlToken is a lexical token of a SQL statement: identifiers are folded to
// lower case unless quoted; strings, numbers and comments are dropped.
type sqlToken struct {
	text   string
	ident  bool // identifier or keyword
	quoted bool
}

func lexSQL(q string) []sqlToken {
	var toks []sqlToken
	i := 0
	for i < len(q) {
		c := q[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			for i < len(q) && q[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			end := strings.Index(q[i+2:], "*/")
			if end < 0 {
				return toks
			}
			i += end + 4
		case c == '\'':
			i++
			for i < len(q) {
				if q[i] == '\'' {
					if i+1 < len(q) && q[i+1] == '\'' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			i++
		case c == '"':
			j := i + 1
			var b strings.Builder
			for j < len(q) {
				if q[j] == '"' {
					if j+1 < len(q) && q[j+1] == '"' {
						b.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(q[j])
				j++
			}
			toks = append(toks, sqlToken{text: b.String(), ident: true, quoted: true})
			i = j + 1
		case c == '$' && i+1 < len(q) && (q[i+1] == '$' || isIdentStart(q[i+1])):
			// dollar-quoted string $tag$...$tag$ (placeholders $1 handled below)
			j := i + 1
			for j < len(q) && q[j] != '$' && isIdentChar(q[j]) {
				j++
			}
			if j < len(q) && q[j] == '$' {
				tag := q[i : j+1]
				end := strings.Index(q[j+1:], tag)
				if end < 0 {
					return toks
				}
				i = j + 1 + end + len(tag)
			} else {
				i = j
			}
		case isIdentStart(c):
			j := i
			for j < len(q) && isIdentChar(q[j]) {
				j++
			}
			toks = append(toks, sqlToken{text: strings.ToLower(q[i:j]), ident: true})
			i = j
		case c >= '0' && c <= '9' || c == '$':
			j := i + 1
			for j < len(q) && (q[j] >= '0' && q[j] <= '9' || q[j] == '.') {
				j++
			}
			toks = append(toks, sqlToken{text: "?"})
			i = j
		default:
			toks = append(toks, sqlToken{text: string(c)})
			i++
		}
	}
	return toks
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

var nonRelationWords = map[string]bool{
	"select": true, "lateral": true, "only": true, "where": true, "set": true, "values": true,
	"on": true, "using": true, "as": true, "join": true, "inner": true, "left": true, "right": true,
	"full": true, "cross": true, "natural": true, "group": true, "order": true, "limit": true,
	"offset": true, "union": true, "returning": true, "default": true, "unnest": true,
}

// ExtractRelations returns the relation names (as written, possibly
// schema-qualified, lowercased unless quoted) that follow FROM, JOIN, UPDATE,
// INTO, USING (DELETE ... USING) or TABLE. Comma-separated FROM lists are
// followed. Function calls and subqueries are skipped. Best effort only.
func ExtractRelations(q string) []string {
	toks := lexSQL(q)
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	// readName parses ident [. ident] at position i; returns name, next index, ok.
	readName := func(i int) (string, int, bool) {
		if i >= len(toks) || !toks[i].ident {
			return "", i, false
		}
		if !toks[i].quoted && nonRelationWords[toks[i].text] {
			return "", i, false
		}
		name := toks[i].text
		i++
		if i+1 < len(toks) && toks[i].text == "." && toks[i+1].ident {
			name += "." + toks[i+1].text
			i += 2
		}
		return name, i, true
	}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if !t.ident || t.quoted {
			continue
		}
		kw := t.text
		if kw != "from" && kw != "join" && kw != "update" && kw != "into" && kw != "using" && kw != "table" {
			continue
		}
		j := i + 1
		if j < len(toks) && toks[j].ident && !toks[j].quoted && toks[j].text == "only" {
			j++
		}
		for {
			name, next, ok := readName(j)
			if !ok {
				break
			}
			// "name(" is a function call, except INSERT INTO t (cols).
			if next < len(toks) && toks[next].text == "(" && kw != "into" {
				break
			}
			add(name)
			if kw != "from" && kw != "using" {
				break
			}
			// skip optional alias, then follow ", next_relation"
			k := next
			if k < len(toks) && toks[k].ident && !toks[k].quoted && toks[k].text == "as" {
				k++
			}
			if k < len(toks) && toks[k].ident && !isClauseWord(toks[k]) {
				k++
			}
			if k < len(toks) && toks[k].text == "," {
				j = k + 1
				if j < len(toks) && toks[j].ident && !toks[j].quoted && toks[j].text == "only" {
					j++
				}
				continue
			}
			break
		}
	}
	return out
}

func isClauseWord(t sqlToken) bool {
	if t.quoted {
		return false
	}
	switch t.text {
	case "where", "join", "inner", "left", "right", "full", "cross", "natural", "on", "using",
		"group", "order", "limit", "offset", "union", "except", "intersect", "returning", "set",
		"for", "having", "window", "fetch", "values", "select", "do", "lateral":
		return true
	}
	return false
}

// relationResolver maps names found in query text to schema-qualified names
// that exist in the profile.
type relationResolver struct {
	qualified map[string]string   // "schema.name" -> canonical
	byName    map[string][]string // "name" -> canonical candidates (sorted, public first)
}

func newRelationResolver(qualifiedNames []string) *relationResolver {
	r := &relationResolver{qualified: map[string]string{}, byName: map[string][]string{}}
	for _, q := range qualifiedNames {
		r.qualified[q] = q
		if i := strings.IndexByte(q, '.'); i >= 0 {
			r.byName[q[i+1:]] = append(r.byName[q[i+1:]], q)
		}
	}
	for k, v := range r.byName {
		sort.Slice(v, func(i, j int) bool {
			pi, pj := strings.HasPrefix(v[i], "public."), strings.HasPrefix(v[j], "public.")
			if pi != pj {
				return pi
			}
			return v[i] < v[j]
		})
		r.byName[k] = v
	}
	return r
}

// resolve returns the sorted, de-duplicated schema-qualified relations
// referenced by query that exist in the profile. Unqualified names prefer
// the public schema (the default search_path), then the first schema by name.
func (r *relationResolver) resolve(query string) []string {
	set := map[string]bool{}
	for _, name := range ExtractRelations(query) {
		if strings.Contains(name, ".") {
			if q, ok := r.qualified[name]; ok {
				set[q] = true
			}
			continue
		}
		if c := r.byName[name]; len(c) > 0 {
			set[c[0]] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

var (
	reCatalogRef   = regexp.MustCompile(`(?i)\b(pg_catalog|information_schema)\s*\.|\bpg_(stat|class|namespace|attribute|index|locks|settings|database|roles|user|tables|views|proc|type|constraint|depend|extension|sequence|trigger|inherits|statistic|stats|indexes|replication|tablespace|authid|am|description|matviews|policies|rewrite|prepared|cursors|timezone|file_settings|hba|shadow|available|publication|subscription)[a-z_]*\b`)
	reCollectorTag = regexp.MustCompile(`pg-rehearse:`)
)

// isApplicationQuery reports whether a pg_stat_statements entry is ordinary
// application DML worth replaying: not a utility statement (SET, BEGIN,
// COMMIT, DDL, VACUUM, ...), not the collector's own queries, and not a query
// against the system catalogs.
func isApplicationQuery(q string) bool {
	if reCollectorTag.MatchString(q) {
		return false
	}
	toks := lexSQL(q)
	first := ""
	for _, t := range toks {
		if t.text == "(" {
			continue
		}
		first = t.text
		break
	}
	switch first {
	case "select", "insert", "update", "delete", "with", "merge", "values", "table":
	default:
		return false
	}
	if reCatalogRef.MatchString(q) {
		return false
	}
	for _, rel := range ExtractRelations(q) {
		if strings.HasPrefix(rel, "pg_") || strings.HasPrefix(rel, "information_schema.") || strings.HasPrefix(rel, "pg_catalog.") {
			return false
		}
	}
	return true
}

// normalizeForGrouping replaces literals with '?' so that recurring
// transactions with different parameters group together.
func normalizeForGrouping(q string) string {
	toks := lexSQL(q)
	var b strings.Builder
	for i, t := range toks {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t.text)
	}
	return b.String()
}
