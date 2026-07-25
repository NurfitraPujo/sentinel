package sentinel

import "strings"

var defaultPIIKeys = []string{
	"password",
	"token",
	"secret",
	"authorization",
	"credit_card",
	"ssn",
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
