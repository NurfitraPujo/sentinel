package sentinel

import "strings"

// defaultPIIKeys are matched as SUBSTRINGS of the (lowercased) metadata key.
//
// The list must cover every key named in docs/sdk-specification.md section 5 ("Mandatory Masking
// Keys") and specs/007-go-client-sdk/security-constraints.md. It previously omitted pass, passwd,
// bearer, card_number, cvv and social_security. That gap was dormant only because the SDK emitted
// its scrubbed map under the JSON key `context`, which the ingestor never decoded — so no client
// metadata reached Postgres at all. Renaming it to `metadata` (the S4 fix) made the gap live.
var defaultPIIKeys = []string{
	"password",
	"passwd",
	"pass",
	"token",
	"bearer",
	"secret",
	"authorization",
	"auth",
	"credit_card",
	"card_number",
	"cvv",
	"ssn",
	"social_security",
	"api_key",
	"apikey",
}

func ScrubPII(metadata map[string]interface{}) map[string]interface{} {
	if metadata == nil {
		return nil
	}

	result := make(map[string]interface{}, len(metadata))
	for k, v := range metadata {
		if isSensitiveKey(k) {
			result[k] = "[FILTERED]"
			continue
		}

		switch val := v.(type) {
		case map[string]interface{}:
			result[k] = ScrubPII(val)
		default:
			result[k] = v
		}
	}
	return result
}

func isSensitiveKey(key string) bool {
	lowerKey := strings.ToLower(key)
	for _, piiKey := range defaultPIIKeys {
		if strings.Contains(lowerKey, piiKey) {
			return true
		}
	}
	return false
}
