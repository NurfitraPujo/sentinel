package unit

import (
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
			input:    `pwd: "short"`,
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
			input: map[string]interface{}{"message": "user: api_key=secret123"},
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
			name:  "slice masked",
			input: map[string]interface{}{"tokens": []interface{}{"abc123", "def456"}},
			checkFn: func(got map[string]interface{}) bool {
				slice, ok := got["tokens"].([]interface{})
				return ok && len(slice) == 2
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
