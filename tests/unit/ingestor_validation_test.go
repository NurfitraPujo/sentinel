package unit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validPayload returns a minimal payload that satisfies every validation rule.
func validPayload() *validation.ErrorPayload {
	return &validation.ErrorPayload{
		ProjectKey:  "proj-abc123",
		Platform:    "go",
		Environment: "production",
		Message:     "something happened",
		ErrorClass:  "RuntimeError",
		Stacktrace:  []validation.StackFrame{},
		Metadata:    map[string]interface{}{},
	}
}

func TestValidatePayload_HappyPath(t *testing.T) {
	result := validation.ValidatePayload(validPayload())

	require.True(t, result.Valid, "expected Valid=true for minimal payload")
	require.Empty(t, result.Errors, "expected no errors for minimal payload")
}

func TestValidatePayload_RequiredFields(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(p *validation.ErrorPayload)
		field     string
		wantMsgSub string
	}{
		{
			name: "empty project_key",
			mutate: func(p *validation.ErrorPayload) {
				p.ProjectKey = ""
			},
			field:      "project_key",
			wantMsgSub: "required",
		},
		{
			name: "empty platform",
			mutate: func(p *validation.ErrorPayload) {
				p.Platform = ""
			},
			field:      "platform",
			wantMsgSub: "required",
		},
		{
			name: "empty environment",
			mutate: func(p *validation.ErrorPayload) {
				p.Environment = ""
			},
			field:      "environment",
			wantMsgSub: "required",
		},
		{
			name: "empty error_class",
			mutate: func(p *validation.ErrorPayload) {
				p.ErrorClass = ""
			},
			field:      "error_class",
			wantMsgSub: "required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPayload()
			tt.mutate(p)

			result := validation.ValidatePayload(p)

			require.False(t, result.Valid, "expected Valid=false")
			require.Len(t, result.Errors, 1, "expected exactly one field error")
			assert.Equal(t, tt.field, result.Errors[0].Field)
			assert.Contains(t, strings.ToLower(result.Errors[0].Message), tt.wantMsgSub)
		})
	}
}

func TestValidatePayload_ProjectKeyLength(t *testing.T) {
	p := validPayload()
	p.ProjectKey = strings.Repeat("a", 65)

	result := validation.ValidatePayload(p)

	require.False(t, result.Valid)
	require.Len(t, result.Errors, 1)
	assert.Equal(t, "project_key", result.Errors[0].Field)
	assert.Contains(t, result.Errors[0].Message, "64")
}

func TestValidatePayload_PlatformFormat(t *testing.T) {
	tests := []struct {
		name     string
		platform string
	}{
		{name: "uppercase letters", platform: "Go"},
		{name: "underscore", platform: "go_lang"},
		{name: "hyphen", platform: "go-lang"},
		{name: "non-ASCII", platform: "gö"},
		{name: "space", platform: "go lang"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPayload()
			p.Platform = tt.platform

			result := validation.ValidatePayload(p)

			require.False(t, result.Valid)
			require.Len(t, result.Errors, 1)
			assert.Equal(t, "platform", result.Errors[0].Field)
			assert.Contains(t, result.Errors[0].Message, "lowercase alphanumeric")
		})
	}
}

func TestValidatePayload_EnvironmentFormat(t *testing.T) {
	tests := []struct {
		name string
		env  string
	}{
		{name: "uppercase letters", env: "Production"},
		{name: "underscore", env: "prod_env"},
		{name: "hyphen", env: "prod-env"},
		{name: "non-ASCII", env: "pröd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPayload()
			p.Environment = tt.env

			result := validation.ValidatePayload(p)

			require.False(t, result.Valid)
			require.Len(t, result.Errors, 1)
			assert.Equal(t, "environment", result.Errors[0].Field)
			assert.Contains(t, result.Errors[0].Message, "lowercase alphanumeric")
		})
	}
}

func TestValidatePayload_MessageLength(t *testing.T) {
	t.Run("at the limit passes", func(t *testing.T) {
		p := validPayload()
		p.Message = strings.Repeat("a", 10000)

		result := validation.ValidatePayload(p)

		require.True(t, result.Valid, "expected 10000-char message to pass")
		require.Empty(t, result.Errors)
	})

	t.Run("over the limit fails", func(t *testing.T) {
		p := validPayload()
		p.Message = strings.Repeat("a", 10001)

		result := validation.ValidatePayload(p)

		require.False(t, result.Valid)
		require.Len(t, result.Errors, 1)
		assert.Equal(t, "message", result.Errors[0].Field)
		assert.Contains(t, result.Errors[0].Message, "10000")
	})
}

func TestValidatePayload_StacktraceLength(t *testing.T) {
	t.Run("at the limit passes", func(t *testing.T) {
		p := validPayload()
		frames := make([]validation.StackFrame, 100)
		for i := range frames {
			frames[i] = validation.StackFrame{File: "f.go", Line: int32(i), Function: "f", InApp: false}
		}
		p.Stacktrace = frames

		result := validation.ValidatePayload(p)

		require.True(t, result.Valid, "expected 100 frames to pass")
		require.Empty(t, result.Errors)
	})

	t.Run("over the limit fails", func(t *testing.T) {
		p := validPayload()
		frames := make([]validation.StackFrame, 101)
		for i := range frames {
			frames[i] = validation.StackFrame{File: "f.go", Line: int32(i), Function: "f", InApp: false}
		}
		p.Stacktrace = frames

		result := validation.ValidatePayload(p)

		require.False(t, result.Valid)
		require.Len(t, result.Errors, 1)
		assert.Equal(t, "stacktrace", result.Errors[0].Field)
		assert.Contains(t, result.Errors[0].Message, "100")
	})
}

func TestValidatePayload_MetadataSize(t *testing.T) {
	t.Run("under the limit passes", func(t *testing.T) {
		p := validPayload()
		// Build a JSON-encoded metadata body well under 64 KB.
		p.Metadata = map[string]interface{}{
			"key": strings.Repeat("v", 1000),
		}

		result := validation.ValidatePayload(p)

		require.True(t, result.Valid, "expected small metadata to pass")
		require.Empty(t, result.Errors)
	})

	t.Run("over the limit fails", func(t *testing.T) {
		p := validPayload()
		// Construct a metadata map whose JSON-encoded form exceeds 64 KB.
		// Each entry encodes as ~70 bytes of JSON; 1000 distinct entries
		// produce ~70 KB, comfortably above the 64 KB threshold.
		md := make(map[string]interface{}, 1000)
		for i := 0; i < 1000; i++ {
			md[fmt.Sprintf("key-%04d", i)] = strings.Repeat("v", 60)
		}
		p.Metadata = md

		encoded, _ := json.Marshal(p.Metadata)
		require.Greater(t, len(encoded), 64*1024, "metadata must exceed 64 KB to exercise rule")

		result := validation.ValidatePayload(p)

		require.False(t, result.Valid)
		require.Len(t, result.Errors, 1)
		assert.Equal(t, "metadata", result.Errors[0].Field)
		assert.Contains(t, result.Errors[0].Message, "65536")
	})
}

func TestValidatePayload_StackFrameFileLength(t *testing.T) {
	t.Run("in_app=true and file > 512 chars produces per-frame error", func(t *testing.T) {
		p := validPayload()
		p.Stacktrace = []validation.StackFrame{
			{File: strings.Repeat("a", 513), Line: 1, Function: "f", InApp: true},
		}

		result := validation.ValidatePayload(p)

		require.False(t, result.Valid)
		require.Len(t, result.Errors, 1)
		assert.Equal(t, "stacktrace[0].file", result.Errors[0].Field)
		assert.Contains(t, result.Errors[0].Message, "512")
	})

	t.Run("in_app=false and file > 512 chars does NOT produce an error", func(t *testing.T) {
		p := validPayload()
		p.Stacktrace = []validation.StackFrame{
			{File: strings.Repeat("a", 513), Line: 1, Function: "f", InApp: false},
		}

		result := validation.ValidatePayload(p)

		require.True(t, result.Valid, "in_app=false should bypass the file length rule")
		require.Empty(t, result.Errors)
	})
}

func TestIsValidAlphanumeric(t *testing.T) {
	// isValidAlphanumeric is package-private; we exercise it via the public
	// ValidatePayload path by observing platform/environment error messages.
	tests := []struct {
		name      string
		value     string
		wantValid bool
	}{
		{name: "lowercase alphanumeric", value: "abc123", wantValid: true},
		{name: "uppercase only", value: "ABC", wantValid: false},
		{name: "with hyphen", value: "abc-def", wantValid: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPayload()
			p.Platform = tt.value

			result := validation.ValidatePayload(p)

			if tt.wantValid {
				require.True(t, result.Valid, "expected %q to be valid", tt.value)
				require.Empty(t, result.Errors)
			} else {
				require.False(t, result.Valid, "expected %q to be invalid", tt.value)
				require.Len(t, result.Errors, 1)
				assert.Equal(t, "platform", result.Errors[0].Field)
				assert.Contains(t, result.Errors[0].Message, "lowercase alphanumeric")
			}
		})
	}

	t.Run("empty string is vacuously valid via the format check", func(t *testing.T) {
		// isValidAlphanumeric("") returns true (vacuously valid: the loop
		// never runs). When the platform is empty, ValidatePayload short-
		// circuits on the "required" rule and never reaches the format check,
		// so we observe a "required" error rather than "lowercase alphanumeric".
		// This confirms the empty-string case does not produce a format error.
		p := validPayload()
		p.Platform = ""

		result := validation.ValidatePayload(p)

		require.False(t, result.Valid)
		require.Len(t, result.Errors, 1)
		assert.Equal(t, "platform", result.Errors[0].Field)
		assert.Contains(t, result.Errors[0].Message, "required")
	})
}

func TestWriteValidationError(t *testing.T) {
	rec := httptest.NewRecorder()
	result := validation.ValidationResult{
		Valid: false,
		Errors: []validation.ValidationError{
			{Field: "platform", Message: "platform must be lowercase alphanumeric"},
		},
	}

	validation.WriteValidationError(rec, result)

	// Status must be 400 Bad Request.
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Body must be the JSON-encoded ValidationResult.
	body := rec.Body.Bytes()
	var decoded validation.ValidationResult
	err := json.Unmarshal(body, &decoded)
	require.NoError(t, err, "response body must be valid JSON")

	assert.False(t, decoded.Valid)
	require.Len(t, decoded.Errors, 1)
	assert.Equal(t, "platform", decoded.Errors[0].Field)
	assert.Contains(t, decoded.Errors[0].Message, "lowercase alphanumeric")
}
