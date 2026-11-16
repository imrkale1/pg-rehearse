// Package locks answers "what does this migration lock, and is it risky?".
//
// It contains a Postgres-aware SQL lexer/splitter, a light statement parser
// shared with internal/fixer, a static linter, and Analyze, which executes a
// migration inside a rolled-back transaction on a shadow database and reads
// pg_locks after every statement.
package locks

import "strings"

type tokKind int

const (
	tkWord    tokKind = iota // unquoted identifier or keyword
	tkQIdent                 // "quoted identifier"
	tkString                 // 'string', E'string', $$dollar$$ ...
	tkNumber                 // 123, 1.5e3
	tkParam                  // $1
	tkOp                     // any other single character
	tkComment                // -- line or /* block */ comment
)

// token is one lexical unit. Pos/End are byte offsets into the source, so
// callers can always recover the exact original spelling (quoting included).
type token struct {
	Kind tokKind
	Text string
	Pos  int
	End  int
}

// upper returns the uppercased text of a word token, "" otherwise.
func (t token) upper() string {
	if t.Kind != tkWord {
		return ""
	}
	return strings.ToUpper(t.Text)
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// lex tokenizes Postgres SQL. It never fails: unterminated constructs extend
// to the end of input.
func lex(s string) []token {
	var toks []token
	i := 0
	n := len(s)
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && i+1 < n && s[i+1] == '-':
			j := i + 2
			for j < n && s[j] != '\n' {
				j++
			}
			toks = append(toks, token{tkComment, s[i:j], i, j})
			i = j
		case c == '/' && i+1 < n && s[i+1] == '*':
			// Postgres block comments nest.
			depth := 1
			j := i + 2
			for j < n && depth > 0 {
				if s[j] == '/' && j+1 < n && s[j+1] == '*' {
					depth++
					j += 2
				} else if s[j] == '*' && j+1 < n && s[j+1] == '/' {
					depth--
					j += 2
				} else {
					j++
				}
			}
			toks = append(toks, token{tkComment, s[i:j], i, j})
			i = j
		case c == '\'':
			j := scanQuoted(s, i, '\'', false)
			toks = append(toks, token{tkString, s[i:j], i, j})
			i = j
		case c == '"':
			j := scanQuoted(s, i, '"', false)
			toks = append(toks, token{tkQIdent, s[i:j], i, j})
			i = j
		case c == '$':
			if i+1 < n && isDigit(s[i+1]) {
				j := i + 1
				for j < n && isDigit(s[j]) {
					j++
				}
				toks = append(toks, token{tkParam, s[i:j], i, j})
				i = j
				continue
			}
			if tag, ok := dollarTag(s, i); ok {
				end := strings.Index(s[i+len(tag):], tag)
				j := n
				if end >= 0 {
					j = i + len(tag) + end + len(tag)
				}
				toks = append(toks, token{tkString, s[i:j], i, j})
				i = j
				continue
			}
			toks = append(toks, token{tkOp, "$", i, i + 1})
			i++
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentChar(s[j]) {
				j++
			}
			word := s[i:j]
			// E'...' escape strings (backslash escapes) and B'', X'', N'' prefixes.
			if j < n && s[j] == '\'' && len(word) == 1 {
				switch word[0] {
				case 'e', 'E':
					k := scanQuoted(s, j, '\'', true)
					toks = append(toks, token{tkString, s[i:k], i, k})
					i = k
					continue
				case 'b', 'B', 'x', 'X', 'n', 'N':
					k := scanQuoted(s, j, '\'', false)
					toks = append(toks, token{tkString, s[i:k], i, k})
					i = k
					continue
				}
			}
			toks = append(toks, token{tkWord, word, i, j})
			i = j
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(s[i+1])):
			j := i + 1
			for j < n {
				d := s[j]
				if isDigit(d) || d == '.' || d == '_' {
					j++
				} else if (d == 'e' || d == 'E') && j+1 < n && (isDigit(s[j+1]) || ((s[j+1] == '+' || s[j+1] == '-') && j+2 < n && isDigit(s[j+2]))) {
					j += 2
				} else {
					break
				}
			}
			toks = append(toks, token{tkNumber, s[i:j], i, j})
			i = j
		default:
			toks = append(toks, token{tkOp, s[i : i+1], i, i + 1})
			i++
		}
	}
	return toks
}

// scanQuoted returns the offset just past the closing quote of a quoted
// construct starting at s[start] == q. Doubled quotes are escapes; with
// backslash=true a backslash escapes the next byte too (E” strings).
func scanQuoted(s string, start int, q byte, backslash bool) int {
	j := start + 1
	for j < len(s) {
		switch s[j] {
		case '\\':
			if backslash {
				j += 2
				continue
			}
		case q:
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		}
		j++
	}
	return len(s)
}

// dollarTag reports whether a dollar-quote opening tag ($$ or $tag$) starts at s[i].
func dollarTag(s string, i int) (string, bool) {
	if i > 0 && isIdentChar(s[i-1]) {
		return "", false // part of an identifier like a$b
	}
	j := i + 1
	if j < len(s) && s[j] == '$' {
		return "$$", true
	}
	if j >= len(s) || !isIdentStart(s[j]) {
		return "", false
	}
	for j < len(s) && s[j] != '$' {
		if !isIdentStart(s[j]) && !isDigit(s[j]) {
			return "", false
		}
		j++
	}
	if j >= len(s) {
		return "", false
	}
	return s[i : j+1], true
}

// SplitStatements splits a migration into individual statements. It
// understands single/double quotes, E” escape strings, dollar-quoted bodies
// ($$ and $tag$), line and (nested) block comments, and SQL-standard
// BEGIN ATOMIC ... END function bodies. Returned statements are trimmed, have
// no trailing semicolon, keep their interior/leading comments, and
// comment-only fragments are dropped.
func SplitStatements(sql string) []string {
	var out []string
	for _, seg := range splitSegments(sql) {
		if !seg.commentOnly {
			out = append(out, seg.text)
		}
	}
	return out
}

type segment struct {
	text        string
	commentOnly bool
}

// Segment is one fragment of a migration: a statement (without trailing
// semicolon) or a comment-only fragment.
type Segment struct {
	Text        string
	CommentOnly bool
}

// SplitSegments is SplitStatements but keeps comment-only fragments, so a
// rewriter can preserve explanatory comments.
func SplitSegments(sql string) []Segment {
	var out []Segment
	for _, s := range splitSegments(sql) {
		out = append(out, Segment{Text: s.text, CommentOnly: s.commentOnly})
	}
	return out
}

// splitSegments is SplitStatements but keeps comment-only fragments (used by
// the fixer so explanatory comments survive a round trip).
func splitSegments(sql string) []segment {
	toks := lex(sql)
	var segs []segment
	start := 0       // byte offset where current segment starts
	first := -1      // index of first non-comment token in current segment
	atomicDepth := 0 // nesting inside BEGIN ATOMIC ... END
	prevWord := ""
	flush := func(end int) {
		text := strings.TrimSpace(sql[start:end])
		if text != "" {
			segs = append(segs, segment{text: text, commentOnly: first < 0})
		}
	}
	for idx, t := range toks {
		if t.Kind == tkComment {
			continue
		}
		if first < 0 && !(t.Kind == tkOp && t.Text == ";" && atomicDepth == 0) {
			first = idx
		}
		switch {
		case t.Kind == tkOp && t.Text == ";" && atomicDepth == 0:
			flush(t.Pos)
			start = t.End
			first = -1
			prevWord = ""
			continue
		case t.Kind == tkWord:
			u := t.upper()
			if atomicDepth == 0 {
				if u == "ATOMIC" && prevWord == "BEGIN" && toks[first].upper() == "CREATE" {
					atomicDepth = 1
				}
			} else {
				switch u {
				case "CASE":
					atomicDepth++
				case "END":
					atomicDepth--
				}
			}
			prevWord = u
		default:
			prevWord = ""
		}
	}
	flush(len(sql))
	return segs
}
