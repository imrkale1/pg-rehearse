package locks

// Helpers used by internal/fixer to rewrite statements while keeping the
// original spelling (quoting, case, comments) of everything it doesn't touch.

// LeadingComments returns the text before the first non-comment token
// (comments attached to the statement), or "".
func (st *Statement) LeadingComments() string {
	if len(st.toks) == 0 {
		return st.SQL
	}
	return st.SQL[:st.toks[0].Pos]
}

// InsertAfterWord inserts ins right after the first (non-comment) keyword
// token equal to word (case-insensitive). ok is false when word is absent.
func (st *Statement) InsertAfterWord(word, ins string) (string, bool) {
	for _, t := range st.toks {
		if t.upper() == word {
			return st.SQL[:t.End] + ins + st.SQL[t.End:], true
		}
	}
	return st.SQL, false
}

// RemoveWord removes the first keyword token equal to word (and one
// following space).
func (st *Statement) RemoveWord(word string) string {
	for _, t := range st.toks {
		if t.upper() == word {
			end := t.End
			if end < len(st.SQL) && st.SQL[end] == ' ' {
				end++
			}
			return st.SQL[:t.Pos] + st.SQL[end:]
		}
	}
	return st.SQL
}

// UniqueParts splits a UNIQUE / PRIMARY KEY constraint action into the text
// between the constraint keyword and the column list (e.g. "NULLS NOT
// DISTINCT"), the column-list text including parentheses, and the text after
// it (INCLUDE (...), WITH (...), DEFERRABLE ...). ok is false when no column
// list is found (e.g. USING INDEX).
func (a *Action) UniqueParts() (pre, cols, tail string, ok bool) {
	t := a.toks
	if len(t) == 0 {
		return "", "", "", false
	}
	src := func(from, to int) string {
		if from >= to {
			return ""
		}
		return a.Text[t[from].Pos-t[0].Pos : t[to-1].End-t[0].Pos]
	}
	k := 0
	for k < len(t) && t[k].upper() != "UNIQUE" && t[k].upper() != "KEY" {
		k++
	}
	if k == len(t) {
		return "", "", "", false
	}
	start := k + 1
	p := start
	for p < len(t) && !isOp(t, p, "(") {
		p++
	}
	if p == len(t) {
		return "", "", "", false
	}
	end := skipParens(t, p)
	return src(start, p), src(p, end), src(end, len(t)), true
}
