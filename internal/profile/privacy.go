package profile

import (
	"strings"
	"unicode"

	"github.com/pg-rehearse/pg-rehearse/internal/model"
)

// maxValueWidth is the avg_width (bytes) above which value arrays are dropped
// even for non-sensitive columns: wide values are almost always free text
// (comments, descriptions, JSON payloads) that may embed personal data.
const maxValueWidth = 64

// sensitiveSubstrings match anywhere in the compacted (lowercase, separators
// removed) column name. They are long enough to rarely produce false positives.
var sensitiveSubstrings = []string{
	"email", "phone", "mobile", "firstname", "lastname", "fullname", "surname",
	"username", "nickname", "middlename", "maidenname", "address", "street",
	"postcode", "postal", "zipcode", "passport", "password", "passwd", "token",
	"secret", "apikey", "privatekey", "credential", "birth", "creditcard",
	"cardnumber", "iban", "swift", "accountnumber", "routingnumber", "taxid",
	"license", "licence", "latitude", "longitude", "ipaddr", "ssn", "socialsecurity",
	"national", "salary", "gender", "ethnicity", "religion", "signature",
}

// sensitiveTokens must equal one whole token of the column name
// (tokens split on '_', '-', ' ', '.', and camelCase boundaries).
var sensitiveTokens = map[string]bool{
	"ip": true, "dob": true, "ssn": true, "sin": true, "cc": true, "cvv": true,
	"cvc": true, "pan": true, "pin": true, "zip": true, "name": true, "card": true,
	"city": true, "addr": true, "pwd": true, "pw": true, "lat": true, "lng": true,
	"lon": true, "geo": true, "tel": true, "fax": true, "bic": true, "vat": true,
	"mail": true, "otp": true, "hash": true, "salt": true,
}

// IsSensitive reports whether a column should be treated as personal data.
// It combines name heuristics with user configuration. Each entry of extra is
// either a bare column name ("note"), "table.column", "schema.table.column",
// or "table.*" to mark every column of a table. Matching is case-insensitive.
// table may be bare ("users") or schema-qualified ("public.users").
func IsSensitive(table, column string, extra []string) bool {
	col := strings.ToLower(column)
	tbl := strings.ToLower(table)
	bareTbl := tbl
	if i := strings.LastIndexByte(tbl, '.'); i >= 0 {
		bareTbl = tbl[i+1:]
	}
	for _, e := range extra {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.Contains(e, ".") {
			if e == col {
				return true
			}
			continue
		}
		dot := strings.LastIndexByte(e, '.')
		et, ec := e[:dot], e[dot+1:]
		if ec != col && ec != "*" {
			continue
		}
		if et == tbl || et == bareTbl {
			return true
		}
	}
	return nameLooksSensitive(column)
}

func nameLooksSensitive(column string) bool {
	tokens := tokenize(column)
	compact := strings.Join(tokens, "")
	for _, s := range sensitiveSubstrings {
		if strings.Contains(compact, s) {
			return true
		}
	}
	for _, t := range tokens {
		if sensitiveTokens[t] {
			return true
		}
		// username, surname, nickname, displayname, ...
		if strings.HasSuffix(t, "name") && len(t) > 4 {
			return true
		}
	}
	return false
}

// tokenize splits a column name into lowercase tokens on separators and
// camelCase boundaries: "billingIPAddr" -> [billing ip addr].
func tokenize(s string) []string {
	var tokens []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			tokens = append(tokens, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if unicode.IsUpper(r) && len(cur) > 0 {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return tokens
}

// applyPrivacy marks sensitive columns and strips value arrays from any column
// whose values could leak personal data. Only aggregate numbers survive.
func applyPrivacy(t *model.TableProfile, extra []string) {
	for i := range t.Columns {
		c := &t.Columns[i]
		c.Sensitive = IsSensitive(t.QualifiedName(), c.Name, extra)
		if c.Sensitive || c.AvgWidth > maxValueWidth || c.DataType == "bytea" {
			c.MostCommonVals = nil
			c.MostCommonFreqs = nil
			c.HistogramBounds = nil
		}
	}
}
