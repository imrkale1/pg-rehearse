package synth

import (
	"strconv"
	"strings"
)

// Built-in fake data for Sensitive columns. Nothing here is derived from the
// profiled database.

var firstNames = []string{
	"Alex", "Sam", "Jordan", "Taylor", "Morgan", "Casey", "Riley", "Jamie", "Avery", "Quinn",
	"Harper", "Rowan", "Elliot", "Skyler", "Dakota", "Reese", "Emerson", "Finley", "Hayden", "Parker",
	"Sage", "Blake", "Charlie", "Drew", "Robin", "Kai", "Noel", "Ari", "Jules", "Lane",
}

var lastNames = []string{
	"Smith", "Johnson", "Lee", "Garcia", "Brown", "Miller", "Davis", "Wilson", "Moore", "Clark",
	"Lewis", "Walker", "Hall", "Young", "King", "Wright", "Lopez", "Hill", "Green", "Adams",
	"Baker", "Nelson", "Carter", "Mitchell", "Perez", "Roberts", "Turner", "Phillips", "Campbell", "Parker",
}

var streets = []string{"Main", "Oak", "Pine", "Maple", "Cedar", "Elm", "Lake", "Hill", "Park", "River"}

var cities = []string{
	"Springfield", "Riverton", "Fairview", "Lakeside", "Greenville", "Madison", "Georgetown",
	"Franklin", "Clinton", "Salem", "Bristol", "Arlington", "Ashland", "Dover", "Oxford",
}

var syllables = []string{
	"ka", "lo", "mi", "ra", "te", "su", "no", "vi", "da", "pe", "zo", "ri", "ma", "lu", "ne", "to",
	"sa", "ki", "po", "re", "fa", "go", "hu", "je", "bo", "ci", "ya", "we", "xi", "qu",
}

// fakeKind classifies a sensitive column by its name.
type fakeKind int

const (
	fakeGeneric fakeKind = iota
	fakeEmail
	fakeFirstName
	fakeLastName
	fakeFullName
	fakeUsername
	fakePhone
	fakeAddress
	fakeCity
	fakeZip
	fakeSSN
	fakeCard
	fakeIP
	fakePassword
)

func classifyFake(col string) fakeKind {
	c := strings.ToLower(col)
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(c, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("email", "e_mail"):
		return fakeEmail
	case has("first_name", "firstname", "given_name", "fname"):
		return fakeFirstName
	case has("last_name", "lastname", "surname", "family_name", "lname"):
		return fakeLastName
	case has("username", "user_name", "login", "handle", "nickname"):
		return fakeUsername
	case has("phone", "mobile", "msisdn", "fax"):
		return fakePhone
	case has("ssn", "social_security", "national_id", "tax_id", "passport"):
		return fakeSSN
	case has("card", "iban", "account_number", "acct", "routing"):
		return fakeCard
	case has("password", "passwd", "secret", "token", "hash", "salt", "api_key"):
		return fakePassword
	case has("ip_addr", "ipaddr", "ip_address", "remote_addr", "last_ip") || c == "ip":
		return fakeIP
	case has("zip", "postal", "postcode"):
		return fakeZip
	case has("city", "town"):
		return fakeCity
	case has("address", "street", "addr"):
		return fakeAddress
	case has("name"):
		return fakeFullName
	}
	return fakeGeneric
}

// fakeValue appends a fake value for index k (stable: the same k always yields
// the same value, and distinct k yield distinct values when unique is set).
func fakeValue(buf []byte, fk fakeKind, k int64, width int, unique bool) []byte {
	h := splitmix64(uint64(k))
	switch fk {
	case fakeEmail:
		buf = append(buf, "user"...)
		buf = strconv.AppendInt(buf, k, 10)
		return append(buf, "@example.test"...)
	case fakeFirstName:
		buf = append(buf, firstNames[h%uint64(len(firstNames))]...)
	case fakeLastName:
		buf = append(buf, lastNames[h%uint64(len(lastNames))]...)
	case fakeFullName:
		buf = append(buf, firstNames[h%uint64(len(firstNames))]...)
		buf = append(buf, ' ')
		buf = append(buf, lastNames[(h>>16)%uint64(len(lastNames))]...)
	case fakeUsername:
		buf = append(buf, "user_"...)
		return strconv.AppendInt(buf, k, 10)
	case fakePhone:
		buf = append(buf, "+1-555-"...)
		return appendPadded(buf, k%10000000, 7)
	case fakeAddress:
		buf = strconv.AppendUint(buf, 1+h%9999, 10)
		buf = append(buf, ' ')
		buf = append(buf, streets[(h>>20)%uint64(len(streets))]...)
		buf = append(buf, " St"...)
	case fakeCity:
		buf = append(buf, cities[h%uint64(len(cities))]...)
	case fakeZip:
		return appendPadded(buf, k%100000, 5)
	case fakeSSN:
		// Area number 000 is never issued: obviously fake.
		buf = append(buf, "000-"...)
		buf = appendPadded(buf, (k/10000)%100, 2)
		buf = append(buf, '-')
		return appendPadded(buf, k%10000, 4)
	case fakeCard:
		buf = append(buf, "TEST-"...)
		return appendPadded(buf, k, 12)
	case fakeIP:
		return appendIPv4(buf, k)
	case fakePassword:
		buf = append(buf, "fake$"...)
		return appendHex(buf, h, max(width-5, 8))
	default:
		buf = append(buf, "redacted-"...)
		return appendWord(buf, k, max(width-9, 4))
	}
	if unique {
		buf = append(buf, ' ')
		buf = strconv.AppendInt(buf, k, 36)
	}
	return buf
}

func appendPadded(buf []byte, v int64, width int) []byte {
	s := strconv.FormatInt(v, 10)
	for i := len(s); i < width; i++ {
		buf = append(buf, '0')
	}
	return append(buf, s...)
}

func appendIPv4(buf []byte, k int64) []byte {
	buf = append(buf, "10."...)
	buf = strconv.AppendInt(buf, (k>>16)&255, 10)
	buf = append(buf, '.')
	buf = strconv.AppendInt(buf, (k>>8)&255, 10)
	buf = append(buf, '.')
	return strconv.AppendInt(buf, k&255, 10)
}

const hexDigits = "0123456789abcdef"

func appendHex(buf []byte, seed uint64, n int) []byte {
	x := seed
	for i := 0; i < n; i++ {
		if i%16 == 0 && i > 0 {
			x = splitmix64(x)
		}
		buf = append(buf, hexDigits[(x>>(uint(i%16)*4))&15])
	}
	return buf
}

// appendWord appends a pronounceable pseudo-word of roughly n bytes that is a
// pure function of k and ends with base36(k), so distinct k never collide.
func appendWord(buf []byte, k int64, n int) []byte {
	suffix := strconv.FormatInt(k, 36)
	body := n - len(suffix) - 1
	x := splitmix64(uint64(k) ^ 0x5bd1e995)
	written := 0
	for written < body {
		s := syllables[x%uint64(len(syllables))]
		x = splitmix64(x)
		if written+len(s) > body {
			s = s[:body-written]
		}
		buf = append(buf, s...)
		written += len(s)
	}
	if body > 0 {
		buf = append(buf, '-')
	}
	return append(buf, suffix...)
}
