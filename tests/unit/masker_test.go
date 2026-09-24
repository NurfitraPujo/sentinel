package unit

import (
	"sort"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
)

func TestMaskString(t *testing.T) {
	m := masker.NewMasker()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no masking needed",
			input:    "just a regular error message",
			expected: "just a regular error message",
		},
		{
			name:     "SSN masking",
			input:    "ssn: 123-45-6789",
			expected: "ssn: XXX-XX-XXXX",
		},
		{
			name:     "multiple SSNs",
			input:    "123-45-6789 and 987-65-4321",
			expected: "XXX-XX-XXXX and XXX-XX-XXXX",
		},
		{
			name:     "passport number masking",
			input:    "passport: A123456789",
			expected: "passport: <PASSPORT>",
		},
		{
			name:     "passport two letters",
			input:    "passport: AB12345678",
			expected: "passport: <PASSPORT>",
		},
		{
			name:     "api key masking with colon",
			input:    `api_key: "abcdefghijklmnopqrstuv"`,
			expected: `api_key=***REDACTED***`,
		},
		{
			name:     "api key masking with equals",
			input:    `api_key = "abcdefghijklmnopqrstuv"`,
			expected: `api_key=***REDACTED***`,
		},
		{
			name:     "api key no quotes",
			input:    "api_key: abcdefghijklmnopqrstuvwxyz",
			expected: "api_key=***REDACTED***",
		},
		{
			name:     "apiKey variant",
			input:    `apiKey: "abcdefghijklmnopqrstuvwx"`,
			expected: `apiKey=***REDACTED***`,
		},
		{
			name:     "api-secret variant",
			input:    `api_secret = "abcdefghijklmnopqrstuvwx"`,
			expected: `api_secret=***REDACTED***`,
		},
		{
			name:     "password masking with equals",
			input:    `password = "mysecretpass"`,
			expected: `password=***REDACTED***`,
		},
		{
			name:     "password masking with colon",
			input:    `password: "supersecret123"`,
			expected: `password=***REDACTED***`,
		},
		{
			name:     "pwd variant",
			input:    `pwd: "shortpass"`,
			expected: `pwd=***REDACTED***`,
		},
		{
			name:     "passwd variant",
			input:    `passwd: "password123"`,
			expected: `passwd=***REDACTED***`,
		},
		{
			name:     "token masking",
			input:    `token: "abcdefghijklmnopqrstuvwxyz"`,
			expected: `token=***REDACTED***`,
		},
		{
			name:     "auth_token masking",
			input:    `auth_token = "abcdefghijklmnopqrstuvwx"`,
			expected: `auth_token=***REDACTED***`,
		},
		{
			name:     "access_token masking",
			input:    `access_token: "abcdefghijklmnopqrstuvwx"`,
			expected: `access_token=***REDACTED***`,
		},
		{
			name:     "refresh_token masking",
			input:    `refresh_token: "abcdefghijklmnopqrstuvwx"`,
			expected: `refresh_token=***REDACTED***`,
		},
		{
			name:     "credential masking",
			input:    `credential: "abcdefghijklmnopqrstuvwxyz"`,
			expected: `credential=***REDACTED***`,
		},
		{
			name:     "client_secret masking",
			input:    `client_secret = "abcdefghijklmnopqrstuvwx"`,
			expected: `client_secret=***REDACTED***`,
		},
		{
			name:     "private_key masking",
			input:    `private_key: "abcdefghijklmnopqrstuvwx"`,
			expected: `private_key=***REDACTED***`,
		},
		{
			name:     "bearer token masking",
			input:    "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
			expected: "Authorization: Bearer ***REDACTED***",
		},
		{
			name:     "bearer only",
			input:    "bearer abc.def.ghi",
			expected: "Bearer ***REDACTED***",
		},
		{
			name:     "combined masking patterns",
			input:    `ssn: 123-45-6789, api_key: "secret12345678901234567890"`,
			expected: `ssn: XXX-XX-XXXX, api_key=***REDACTED***`,
		},
		// sentinel-event-schema-v2 §6.7 / V8b: Luhn-checked PAN redaction, run first.
		{name: "PAN 16 digits (visa test number)", input: "4111111111111111", expected: "[REDACTED_PAN]"},
		{name: "PAN 16 digits (mastercard test number)", input: "5500005555555559", expected: "[REDACTED_PAN]"},
		{name: "PAN 15 digits (amex test number)", input: "378282246310005", expected: "[REDACTED_PAN]"},
		{name: "PAN 13 digits, Luhn-valid", input: "1727000000007", expected: "[REDACTED_PAN]"},
		{name: "PAN in free text", input: "charge 4111111111111111 failed", expected: "charge [REDACTED_PAN] failed"},
		{name: "PAN dash-separated, surrounding text kept", input: "card 4111-1111-1111-1111 declined", expected: "card [REDACTED_PAN] declined"},
		{name: "PAN space-separated, surrounding text kept", input: "card 4111 1111 1111 1111 declined", expected: "card [REDACTED_PAN] declined"},
		{name: "longest Luhn-valid candidate: 18 digits fail, 16 pass", input: "4111111111111111 12", expected: "[REDACTED_PAN] 12"},
		{name: "a digit run next to a letter is still a group", input: "id4111111111111111", expected: "id[REDACTED_PAN]"},
		{name: "Luhn-invalid 16 digits left alone", input: "4111111111111112", expected: "4111111111111112"},
		{name: "Luhn-invalid 13-digit millisecond timestamp left alone", input: "1727000000000", expected: "1727000000000"},
		{name: "Luhn-invalid 13 digits left alone", input: "1234567890123", expected: "1234567890123"},
		{name: "more than 19 digits is never a PAN", input: "12345678901234567890", expected: "12345678901234567890"},
		{name: "a Luhn-valid 12-digit run is too short", input: "123456789015", expected: "123456789015"},
		{name: "a double separator breaks the chain", input: "4111--1111-1111-1111", expected: "4111--1111-1111-1111"},
		{name: "two Luhn-failing 13-digit runs stored verbatim (P13)", input: "order 1727000000000 at 1234567890123", expected: "order 1727000000000 at 1234567890123"},
		{name: "a 20-digit api key is left to the api_key rule", input: "api_key: 12345678901234567890", expected: "api_key=***REDACTED***"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.MaskString(tt.input)
			if got != tt.expected {
				t.Errorf("MaskString() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestMaskMap(t *testing.T) {
	m := masker.NewMasker()

	tests := []struct {
		name    string
		input   map[string]interface{}
		checkFn func(map[string]interface{}) bool
	}{
		{
			name:  "sensitive keys masked",
			input: map[string]interface{}{"password": "secret123"},
			checkFn: func(got map[string]interface{}) bool {
				return got["password"] == "***REDACTED***"
			},
		},
		{
			name:  "token key masked",
			input: map[string]interface{}{"token": "abc123def456ghi789jkl"},
			checkFn: func(got map[string]interface{}) bool {
				return got["token"] == "***REDACTED***"
			},
		},
		{
			name:  "api_key key masked",
			input: map[string]interface{}{"api_key": "myapikey12345678901234"},
			checkFn: func(got map[string]interface{}) bool {
				return got["api_key"] == "***REDACTED***"
			},
		},
		{
			name:  "apikey key masked (variant)",
			input: map[string]interface{}{"apikey": "myapikey12345678901234"},
			checkFn: func(got map[string]interface{}) bool {
				return got["apikey"] == "***REDACTED***"
			},
		},
		{
			name:  "secret key masked",
			input: map[string]interface{}{"secret": "dontshowme"},
			checkFn: func(got map[string]interface{}) bool {
				return got["secret"] == "***REDACTED***"
			},
		},
		{
			name:  "auth key masked",
			input: map[string]interface{}{"auth": "credentials"},
			checkFn: func(got map[string]interface{}) bool {
				return got["auth"] == "***REDACTED***"
			},
		},
		{
			name:  "credentials key masked",
			input: map[string]interface{}{"credentials": "sensitive"},
			checkFn: func(got map[string]interface{}) bool {
				return got["credentials"] == "***REDACTED***"
			},
		},
		{
			name:  "non-sensitive key value masked via content",
			input: map[string]interface{}{"message": "user: api_key=secret12345678901234567890"},
			checkFn: func(got map[string]interface{}) bool {
				return got["message"] == "user: api_key=***REDACTED***"
			},
		},
		{
			name:  "nested map masked",
			input: map[string]interface{}{"data": map[string]interface{}{"token": "abc123"}},
			checkFn: func(got map[string]interface{}) bool {
				nested, ok := got["data"].(map[string]interface{})
				return ok && nested["token"] == "***REDACTED***"
			},
		},
		{
			// Uses a NON-sensitive key: this case exists to prove slices are traversed and
			// preserved, not to assert anything about masking. It previously used "tokens",
			// which substring-matches "token" and is now (correctly) redacted wholesale.
			name:  "slice preserved for non-sensitive key",
			input: map[string]interface{}{"items": []interface{}{"abc123", "def456"}},
			checkFn: func(got map[string]interface{}) bool {
				slice, ok := got["items"].([]interface{})
				return ok && len(slice) == 2
			},
		},
		{
			// Substring match: a plural/prefixed sensitive key must not slip through. Exact-match
			// key comparison let "tokens", "user_password" and "x-api-key" pass untouched.
			name:  "sensitive key matched as substring",
			input: map[string]interface{}{"tokens": []interface{}{"abc123"}},
			checkFn: func(got map[string]interface{}) bool {
				return got["tokens"] == "***REDACTED***"
			},
		},
		{
			name:  "sensitive key with prefix masked",
			input: map[string]interface{}{"user_password": "hunter2"},
			checkFn: func(got map[string]interface{}) bool {
				return got["user_password"] == "***REDACTED***"
			},
		},
		{
			name:  "case insensitive sensitive key - uppercase",
			input: map[string]interface{}{"PASSWORD": "secret"},
			checkFn: func(got map[string]interface{}) bool {
				return got["PASSWORD"] == "***REDACTED***"
			},
		},
		{
			name:  "case insensitive sensitive key - mixed case",
			input: map[string]interface{}{"Api_Key": "secret"},
			checkFn: func(got map[string]interface{}) bool {
				return got["Api_Key"] == "***REDACTED***"
			},
		},
		// sentinel-event-schema-v2 §6.7 / V8a: `cookie` and `api-key`, substring and case-insensitive.
		{name: "Cookie", input: map[string]interface{}{"Cookie": "sid=abc"}, checkFn: redactedKey("Cookie")},
		{name: "Set-Cookie", input: map[string]interface{}{"Set-Cookie": "sid=abc; HttpOnly"}, checkFn: redactedKey("Set-Cookie")},
		{name: "cookie_consent", input: map[string]interface{}{"cookie_consent": "yes"}, checkFn: redactedKey("cookie_consent")},
		{name: "X-API-Key", input: map[string]interface{}{"X-API-Key": "sk_live_x"}, checkFn: redactedKey("X-API-Key")},
		{name: "x-api-key-id", input: map[string]interface{}{"x-api-key-id": "k1"}, checkFn: redactedKey("x-api-key-id")},
		{name: "api_key still redacted", input: map[string]interface{}{"api_key": "k"}, checkFn: redactedKey("api_key")},
		{name: "apikey still redacted", input: map[string]interface{}{"apikey": "k"}, checkFn: redactedKey("apikey")},
		{
			name:  "a PAN in a metadata value is redacted by MaskString",
			input: map[string]interface{}{"note": "paid with 4111 1111 1111 1111"},
			checkFn: func(got map[string]interface{}) bool {
				return got["note"] == "paid with [REDACTED_PAN]"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.MaskMap(tt.input)
			if !tt.checkFn(got) {
				t.Errorf("MaskMap() = %v, check failed", got)
			}
		})
	}
}

func TestMaskSlice(t *testing.T) {
	m := masker.NewMasker()

	tests := []struct {
		name    string
		input   []interface{}
		checkFn func([]interface{}) bool
	}{
		{
			name:  "slice with sensitive strings",
			input: []interface{}{"password: secret123", "normal string"},
			checkFn: func(got []interface{}) bool {
				s1, ok := got[0].(string)
				return ok && s1 == "password=***REDACTED***"
			},
		},
		{
			name:  "slice with numbers unchanged",
			input: []interface{}{123, "api_key: secret1234567890", 456},
			checkFn: func(got []interface{}) bool {
				return got[0] == 123 && got[2] == 456
			},
		},
		{
			name:  "empty slice",
			input: []interface{}{},
			checkFn: func(got []interface{}) bool {
				return len(got) == 0
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.MaskSlice(tt.input)
			if !tt.checkFn(got) {
				t.Errorf("MaskSlice() = %v, check failed", got)
			}
		})
	}
}

func redactedKey(key string) func(map[string]interface{}) bool {
	return func(got map[string]interface{}) bool { return got[key] == "***REDACTED***" }
}

// V8d's other half (sentinel-event-schema-v2 §9.2): SensitiveKeys is the sorted key list, including the
// two TASK-08 keys, and a copy the caller cannot use to change the masker.
func TestSensitiveKeys(t *testing.T) {
	keys := masker.SensitiveKeys()
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("SensitiveKeys() is not sorted: %v", keys)
	}
	has := map[string]bool{}
	for _, k := range keys {
		has[k] = true
	}
	for _, k := range []string{"cookie", "api-key", "password", "authorization"} {
		if !has[k] {
			t.Errorf("SensitiveKeys() lacks %q: %v", k, keys)
		}
	}
	keys[0] = "mutated"
	if masker.SensitiveKeys()[0] == "mutated" {
		t.Error("SensitiveKeys() must return a fresh copy")
	}
}
