package masker

import (
	"regexp"
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

	sensitiveKeys = []string{
		"password", "passwd", "pwd", "secret", "token", "api_key", "apikey",
		"api_key", "apiKey", "auth", "credential", "credentials", "private_key",
		"client_secret", "access_token", "refresh_token",
	}
)

type Masker struct{}

func NewMasker() *Masker {
	return &Masker{}
}

func (m *Masker) MaskString(s string) string {
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

func (m *Masker) isSensitiveKey(key string) bool {
	lowerKey := strings.ToLower(key)
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(lowerKey, sensitive) {
			return true
		}
	}
	return false
}
