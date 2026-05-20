package normalizer

import (
	"regexp"
)

var (
	uuidRegex       = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	numericIDRegex  = regexp.MustCompile(`\b\d{6,}\b`)
	hexAddressRegex = regexp.MustCompile(`\b0x[0-9a-fA-F]{6,}\b`)
	emailRegex      = regexp.MustCompile(`[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}`)
	versionRegex    = regexp.MustCompile(`\bv?\d+\.\d+\.\d+(-[a-zA-Z0-9]+)?\b`)
	userPathRegex   = regexp.MustCompile(`/(?:home|Users|U/users)[^/\s]+`)
)

type Normalizer struct{}

func NewNormalizer() *Normalizer {
	return &Normalizer{}
}

func (n *Normalizer) NormalizeString(s string) string {
	s = uuidRegex.ReplaceAllString(s, "<UUID>")
	s = numericIDRegex.ReplaceAllString(s, "<NUMERIC_ID>")
	s = hexAddressRegex.ReplaceAllString(s, "<HEX_ADDR>")
	s = emailRegex.ReplaceAllString(s, "<EMAIL>")
	s = versionRegex.ReplaceAllString(s, "<VERSION>")
	s = userPathRegex.ReplaceAllString(s, "/<USER_PATH>")
	return s
}

func (n *Normalizer) NormalizeMap(m map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	for k, v := range m {
		normalizedKey := n.NormalizeString(k)
		switch val := v.(type) {
		case string:
			result[normalizedKey] = n.NormalizeString(val)
		case map[string]interface{}:
			result[normalizedKey] = n.NormalizeMap(val)
		case []interface{}:
			result[normalizedKey] = n.NormalizeSlice(val)
		default:
			result[normalizedKey] = val
		}
	}
	return result
}

func (n *Normalizer) NormalizeSlice(s []interface{}) []interface{} {
	result := make([]interface{}, len(s))
	for i, v := range s {
		switch val := v.(type) {
		case string:
			result[i] = n.NormalizeString(val)
		case map[string]interface{}:
			result[i] = n.NormalizeMap(val)
		default:
			result[i] = val
		}
	}
	return result
}
