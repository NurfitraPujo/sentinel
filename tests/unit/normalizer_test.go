package unit

import (
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/normalizer"
)

func TestNormalizeString(t *testing.T) {
	n := normalizer.NewNormalizer()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "no normalization needed",
			input:    "simple error message",
			expected: "simple error message",
		},
		{
			name:     "UUID replacement",
			input:    "user: 550e8400-e29b-41d4-a716-446655440000 logged in",
			expected: "user: <UUID> logged in",
		},
		{
			name:     "UUID replacement uppercase",
			input:    "550E8400-E29B-41D4-A716-446655440000",
			expected: "<UUID>",
		},
		{
			name:     "numeric ID replacement - 6+ digits",
			input:    "order id: 1234567890",
			expected: "order id: <NUMERIC_ID>",
		},
		{
			name:     "numeric ID replacement - 10 digits",
			input:    "record: 1234567890",
			expected: "record: <NUMERIC_ID>",
		},
		{
			name:     "numeric ID not replaced - fewer than 6 digits",
			input:    "count: 12345",
			expected: "count: 12345",
		},
		{
			name:     "hex address replacement",
			input:    "pointer: 0x7fff5fbff8c0",
			expected: "pointer: <HEX_ADDR>",
		},
		{
			name:     "hex address replacement multiple",
			input:    "addrs: 0xABCDEF 0x123456",
			expected: "addrs: <HEX_ADDR> <HEX_ADDR>",
		},
		{
			name:     "email replacement",
			input:    "contact: john.doe@example.com",
			expected: "contact: <EMAIL>",
		},
		{
			name:     "email replacement with subdomain",
			input:    "admin@mail.company.co.uk",
			expected: "admin@<EMAIL>",
		},
		{
			name:     "version string removal",
			input:    "using v1.2.3",
			expected: "using <VERSION>",
		},
		{
			name:     "version string removal with prefix v",
			input:    "version: v4.2.1",
			expected: "version: <VERSION>",
		},
		{
			name:     "version string removal with prerelease",
			input:    "v2.0.0-beta",
			expected: "<VERSION>",
		},
		{
			name:     "user path replacement - /home",
			input:    "path: /home/johndoe/projects",
			expected: "path: /<USER_PATH>/projects",
		},
		{
			name:     "user path replacement - /Users",
			input:    "path: /Users/johndoe/src",
			expected: "path: /<USER_PATH>/src",
		},
		{
			name:     "user path replacement - /u/users",
			input:    "path: /u/users/admin",
			expected: "path: /<USER_PATH>/admin",
		},
		{
			name:     "combined normalizations",
			input:    "user john@example.com with id 1234567890 at /home/user",
			expected: "user <EMAIL> with id <NUMERIC_ID> at /<USER_PATH>",
		},
		{
			name:     "multiple UUIDs",
			input:    "ids: 550e8400-e29b-41d4-a716-446655440000 and 6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			expected: "ids: <UUID> and <UUID>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := n.NormalizeString(tt.input)
			if got != tt.expected {
				t.Errorf("NormalizeString() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestNormalizeMap(t *testing.T) {
	n := normalizer.NewNormalizer()

	tests := []struct {
		name     string
		input    map[string]interface{}
		expected map[string]interface{}
	}{
		{
			name:     "empty map",
			input:    map[string]interface{}{},
			expected: map[string]interface{}{},
		},
		{
			name: "simple key value",
			input: map[string]interface{}{
				"message": "error in file.go",
			},
			expected: map[string]interface{}{
				"message": "error in file.go",
			},
		},
		{
			name: "nested map",
			input: map[string]interface{}{
				"user": map[string]interface{}{
					"email": "test@example.com",
					"id":    float64(1234567890),
				},
			},
			expected: map[string]interface{}{
				"user": map[string]interface{}{
					"email": "<EMAIL>",
					"id":    float64(1234567890),
				},
			},
		},
		{
			name: "slice of strings",
			input: map[string]interface{}{
				"emails": []interface{}{"a@b.com", "c@d.com"},
			},
			expected: map[string]interface{}{
				"emails": []interface{}{"<EMAIL>", "<EMAIL>"},
			},
		},
		{
			name: "key normalization",
			input: map[string]interface{}{
				"user_id_1234567890": "value",
			},
			expected: map[string]interface{}{
				"<NUMERIC_ID>": "value",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := n.NormalizeMap(tt.input)
			if len(got) != len(tt.expected) {
				t.Errorf("NormalizeMap() length = %v, want %v", len(got), len(tt.expected))
			}
		})
	}
}

func TestNormalizeSlice(t *testing.T) {
	n := normalizer.NewNormalizer()

	tests := []struct {
		name     string
		input    []interface{}
		expected []interface{}
	}{
		{
			name:     "empty slice",
			input:    []interface{}{},
			expected: []interface{}{},
		},
		{
			name: "strings with UUIDs",
			input: []interface{}{
				"550e8400-e29b-41d4-a716-446655440000",
				"plain string",
			},
			expected: []interface{}{
				"<UUID>",
				"plain string",
			},
		},
		{
			name: "mixed types",
			input: []interface{}{
				"email: test@example.com",
				float64(1234567890),
				true,
			},
			expected: []interface{}{
				"email: <EMAIL>",
				float64(1234567890),
				true,
			},
		},
		{
			name: "nested maps in slice",
			input: []interface{}{
				map[string]interface{}{
					"id": "1234567890",
				},
			},
			expected: []interface{}{
				map[string]interface{}{
					"id": "<NUMERIC_ID>",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := n.NormalizeSlice(tt.input)
			if len(got) != len(tt.expected) {
				t.Errorf("NormalizeSlice() length = %v, want %v", len(got), len(tt.expected))
			}
		})
	}
}
