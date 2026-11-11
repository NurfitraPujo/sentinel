package masker

import (
	"regexp"
	"sort"
	"strings"
)

var (
	ssnRegex        = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	passportRegex   = regexp.MustCompile(`\b[A-Z]{1,2}\d{6,9}\b`)
	apiKeyRegex     = regexp.MustCompile(`(?i)(api[_-]?key|apikey|api[_-]?secret|api[_-]?token)\s*[=:]\s*["']?([a-zA-Z0-9_\-]{20,})["']?`)
	passwordRegex   = regexp.MustCompile(`(?i)(password|passwd|pwd|pass)\s*[=:]\s*["']?([^\s"']{8,})["']?`)
	tokenRegex      = regexp.MustCompile(`(?i)(token|auth[_-]?token|access[_-]?token|refresh[_-]?token)\s*[=:]\s*["']?([a-zA-Z0-9_\-\.]{20,})["']?`)
	credentialRegex = regexp.MustCompile(`(?i)(credential|client[_-]?secret|private[_-]?key)\s*[=:]\s*["']?([a-zA-Z0-9_\-\+\=/]{20,})["']?`)
	bearerRegex     = regexp.MustCompile(`(?i)bearer\s+([a-zA-Z0-9_\-\.]+)`)

	sensitiveKeys = map[string]struct{}{
		"password":      {},
		"passwd":        {},
		"pwd":           {},
		"secret":        {},
		"token":         {},
		"api_key":       {},
		"apikey":        {},
		"auth":          {},
		"credential":    {},
		"credentials":   {},
		"private_key":   {},
		"client_secret": {},
		"access_token":  {},
		"refresh_token": {},
		// Named by docs/sdk-specification.md section 5 but previously absent here, so the
		// server-side second masking layer that PROJECT_CONTEXT.md treats as a durable constraint
		// did not actually cover them.
		"authorization":   {},
		"bearer":          {},
		"credit_card":     {},
		"card_number":     {},
		"cvv":             {},
		"ssn":             {},
		"social_security": {},
		// sentinel-event-schema-v2 TASK-08 (amended decision 4): request headers reach the masker now.
		// Substring match, so `cookie` covers Cookie, Set-Cookie and cookie_consent, and `api-key`
		// covers X-API-Key (Sentinel's own ingest-key header) and x-api-key-id. `api_key`/`apikey`
		// above do not match the hyphenated form.
		"cookie":  {},
		"api-key": {},
	}
)

// PANToken replaces a card number in any string MaskString masks (sentinel-event-schema-v2 §6.7).
// Deliberately distinct from the key-based "***REDACTED***", so a reader can tell which rule fired.
const PANToken = "[REDACTED_PAN]"

type Masker struct{}

func NewMasker() *Masker {
	return &Masker{}
}

func (m *Masker) MaskString(s string) string {
	// The PAN rule runs FIRST, so no earlier rule can consume part of a card number (the SSN pattern
	// matches inside a dash-separated sequence).
	s = maskPANs(s)
	s = ssnRegex.ReplaceAllString(s, "XXX-XX-XXXX")
	s = passportRegex.ReplaceAllString(s, "<PASSPORT>")
	s = apiKeyRegex.ReplaceAllString(s, "$1=***REDACTED***")
	s = passwordRegex.ReplaceAllString(s, "$1=***REDACTED***")
	s = tokenRegex.ReplaceAllString(s, "$1=***REDACTED***")
	s = credentialRegex.ReplaceAllString(s, "$1=***REDACTED***")
	s = bearerRegex.ReplaceAllString(s, "Bearer ***REDACTED***")
	return s
}

func (m *Masker) MaskMap(md map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range md {
		if m.isSensitiveKey(k) {
			result[k] = "***REDACTED***"
			continue
		}

		switch val := v.(type) {
		case string:
			result[k] = m.MaskString(val)
		case map[string]interface{}:
			result[k] = m.MaskMap(val)
		case []interface{}:
			result[k] = m.MaskSlice(val)
		default:
			result[k] = v
		}
	}
	return result
}

func (m *Masker) MaskSlice(s []interface{}) []interface{} {
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case string:
			result[i] = m.MaskString(val)
		case map[string]interface{}:
			result[i] = m.MaskMap(val)
		default:
			result[i] = val
		}
	}
	return result
}

// isSensitiveKey matches on SUBSTRING, not equality. Exact matching meant "user_password",
// "x-api-key" and "authorization_header" all sailed through untouched — the map's entries only ever
// fired on a key that was exactly the bare word.
func (m *Masker) isSensitiveKey(key string) bool {
	lowerKey := strings.ToLower(key)
	if _, found := sensitiveKeys[lowerKey]; found {
		return true
	}
	for candidate := range sensitiveKeys {
		if strings.Contains(lowerKey, candidate) {
			return true
		}
	}
	return false
}

// SensitiveKeys returns the key-based redaction list, sorted: a fresh copy, so docs and tests can
// compare against it without being able to change it (docs/sdk-specification.md §5, V8d).
func SensitiveKeys() []string {
	keys := make([]string, 0, len(sensitiveKeys))
	for k := range sensitiveKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// maskPANs is sentinel-event-schema-v2 §6.7 item 2 (amended decision 4): a Luhn-valid 13-19 digit card
// number, bare or separated by single spaces or dashes, becomes PANToken. Over ASCII digits only:
//
//   - a GROUP is a maximal run of digits (a run next to a letter is still a group: id4111… is caught);
//   - a CHAIN is a sequence of groups in which each pair of neighbours is separated by exactly one ' '
//     or one '-';
//   - a CANDIDATE is a contiguous sub-sequence of whole groups of one chain holding 13 to 19 digits in
//     total — never starting or ending inside a group, so a group of more than 19 digits yields none;
//   - scanning left to right, for each start group the candidate with the most digits is tried first,
//     then fewer; the first one whose digits pass Luhn is a PAN. Its text from its first digit to its
//     last (inner separators included, neighbouring ones excluded) is replaced, and the scan resumes
//     after it. A candidate that fails Luhn is left alone, which is how ids and millisecond timestamps
//     survive (about one in ten arbitrary runs passes by chance and is redacted — spec §7 item 4).
func maskPANs(s string) string {
	var groups []digitGroup
	for i := 0; i < len(s); {
		if !isASCIIDigit(s[i]) {
			i++
			continue
		}
		j := i
		for j < len(s) && isASCIIDigit(s[j]) {
			j++
		}
		groups = append(groups, digitGroup{i, j})
		i = j
	}

	var out strings.Builder
	copied := 0 // bytes of s already written to out
	for chainStart := 0; chainStart < len(groups); {
		chainEnd := chainStart + 1 // exclusive
		for chainEnd < len(groups) {
			prev, next := groups[chainEnd-1], groups[chainEnd]
			if next.start != prev.end+1 || (s[prev.end] != ' ' && s[prev.end] != '-') {
				break
			}
			chainEnd++
		}
		for i := chainStart; i < chainEnd; {
			// The longest Luhn-valid candidate starting at group i: growing j and keeping the last
			// passing end is the same as trying the most digits first and moving to fewer.
			matched := -1
			for j, total := i, 0; j < chainEnd; j++ {
				total += groups[j].end - groups[j].start
				if total > 19 {
					break
				}
				if total >= 13 && luhnValid(s, groups[i:j+1]) {
					matched = j
				}
			}
			if matched < 0 {
				i++
				continue
			}
			out.WriteString(s[copied:groups[i].start])
			out.WriteString(PANToken)
			copied = groups[matched].end
			i = matched + 1
		}
		chainStart = chainEnd
	}
	if copied == 0 {
		return s
	}
	out.WriteString(s[copied:])
	return out.String()
}

// digitGroup is a maximal run of ASCII digits in a string, as byte offsets [start, end).
type digitGroup struct{ start, end int }

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

// luhnValid runs the mod-10 check over the digits of groups, doubling every second digit from the right.
func luhnValid(s string, groups []digitGroup) bool {
	sum, double := 0, false
	for g := len(groups) - 1; g >= 0; g-- {
		for i := groups[g].end - 1; i >= groups[g].start; i-- {
			d := int(s[i] - '0')
			if double {
				d *= 2
				if d > 9 {
					d -= 9
				}
			}
			sum += d
			double = !double
		}
	}
	return sum%10 == 0
}
