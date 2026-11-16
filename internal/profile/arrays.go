package profile

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseArrayLiteral parses the text form of a one-dimensional Postgres array
// (e.g. `{a,"b c","d\"e",NULL}`) into its elements. Quoted elements are
// unquoted and backslash escapes resolved. An unquoted NULL is returned as the
// string "NULL" (pg_stats value arrays never contain SQL NULLs). Nested
// elements (`{{1,2},{3,4}}`) are returned as their raw brace text. An optional
// dimension decoration (`[0:2]={...}`) is ignored.
func ParseArrayLiteral(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		if i := strings.Index(s, "="); i >= 0 {
			s = strings.TrimSpace(s[i+1:])
		}
	}
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return nil, fmt.Errorf("not an array literal: %q", truncate(s, 40))
	}
	body := s[1 : len(s)-1]
	out := []string{}
	if strings.TrimSpace(body) == "" {
		return out, nil
	}
	i := 0
	for {
		// skip whitespace
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n') {
			i++
		}
		if i >= len(body) {
			return nil, fmt.Errorf("unexpected end of array literal")
		}
		var elem string
		switch body[i] {
		case '"':
			var b strings.Builder
			i++
			closed := false
			for i < len(body) {
				c := body[i]
				if c == '\\' && i+1 < len(body) {
					b.WriteByte(body[i+1])
					i += 2
					continue
				}
				if c == '"' {
					i++
					closed = true
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated quoted element")
			}
			elem = b.String()
		case '{':
			start := i
			depth := 0
			inQ := false
			for i < len(body) {
				c := body[i]
				if inQ {
					if c == '\\' {
						i += 2
						continue
					}
					if c == '"' {
						inQ = false
					}
				} else {
					switch c {
					case '"':
						inQ = true
					case '{':
						depth++
					case '}':
						depth--
					}
				}
				i++
				if depth == 0 && !inQ {
					break
				}
			}
			if depth != 0 {
				return nil, fmt.Errorf("unbalanced nested array")
			}
			elem = body[start:i]
		default:
			var b strings.Builder
			for i < len(body) && body[i] != ',' {
				c := body[i]
				if c == '\\' && i+1 < len(body) {
					b.WriteByte(body[i+1])
					i += 2
					continue
				}
				b.WriteByte(c)
				i++
			}
			elem = strings.TrimRight(b.String(), " \t\n")
		}
		out = append(out, elem)
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n') {
			i++
		}
		if i >= len(body) {
			return out, nil
		}
		if body[i] != ',' {
			return nil, fmt.Errorf("expected ',' at offset %d", i+1)
		}
		i++
	}
}

// parseFloatArray parses a float4[]/float8[] text literal.
func parseFloatArray(s string) ([]float64, error) {
	elems, err := ParseArrayLiteral(s)
	if err != nil {
		return nil, err
	}
	out := make([]float64, 0, len(elems))
	for _, e := range elems {
		f, err := strconv.ParseFloat(e, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
