package unit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// sentinel-event-schema-v2 spec §4.3: validation.ValidatePayload is now the live-path bounds step, and
// the rejections it used to DUPLICATE from the proto's CEL rules were deleted from it - protovalidate is
// the single rejection authority. Every row that asserted one of those rejections against ValidatePayload
// now asserts the same input through IngestAt, which is what a request actually meets: an error prefixed
// "validation failed" (main.go maps it to 400) carrying protovalidate's field path and rule id. One
// assertion per rule, at the layer that enforces it.

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

// ingestViolation is one protovalidate violation: its field path ("" for a message-level CEL rule)
// and its rule id.
type ingestViolation struct{ Field, RuleID string }

// ingestViolations runs p through IngestAt, requires a "validation failed" rejection that published
// nothing, and returns the protovalidate violations it carries.
func ingestViolations(t *testing.T, p *validation.ErrorPayload) []ingestViolation {
	t.Helper()
	svc, pub := newTestIngestService(t)
	err := svc.Ingest(context.Background(), p)
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "validation failed: "), "got %q", err)
	assert.Empty(t, pub.published, "a rejected event must never reach the publisher")
	var verr *protovalidate.ValidationError
	require.True(t, errors.As(err, &verr), "want a wrapped *protovalidate.ValidationError, got %T: %v", err, err)
	out := make([]ingestViolation, 0, len(verr.Violations))
	for _, v := range verr.Violations {
		out = append(out, ingestViolation{Field: protovalidate.FieldPathString(v.Proto.GetField()), RuleID: v.Proto.GetRuleId()})
	}
	return out
}

func requireIngestAccepted(t *testing.T, p *validation.ErrorPayload) {
	t.Helper()
	svc, pub := newTestIngestService(t)
	require.NoError(t, svc.Ingest(context.Background(), p))
	require.Len(t, pub.published, 1)
}

func TestValidatePayload_HappyPath(t *testing.T) {
	result := validation.ValidatePayload(validPayload())

	require.True(t, result.Valid, "expected Valid=true for minimal payload")
	require.Empty(t, result.Errors, "expected no errors for minimal payload")
	requireIngestAccepted(t, validPayload())
}

func TestIngestValidation_RequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(p *validation.ErrorPayload)
		want   []ingestViolation
	}{
		// An empty required string fails both its field-level `required` rule and its message-level
		// CEL rule.
		{name: "empty project_key", mutate: func(p *validation.ErrorPayload) { p.ProjectKey = "" }, want: []ingestViolation{{"", "error_event.project_key"}, {"project_key", "required"}}},
		{name: "empty platform", mutate: func(p *validation.ErrorPayload) { p.Platform = "" }, want: []ingestViolation{{"", "error_event.platform"}, {"platform", "required"}}},
		{name: "empty environment", mutate: func(p *validation.ErrorPayload) { p.Environment = "" }, want: []ingestViolation{{"", "error_event.environment"}, {"environment", "required"}}},
		// I12's shape: field path error_class, rule id required (error_event.proto:88) - not the old
		// ValidatePayload sentence "error_class is required", which never ran live.
		{name: "empty error_class", mutate: func(p *validation.ErrorPayload) { p.ErrorClass = "" }, want: []ingestViolation{{"error_class", "required"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPayload()
			tt.mutate(p)
			require.ElementsMatch(t, tt.want, ingestViolations(t, p))
		})
	}
}

func TestIngestValidation_ProjectKeyLength(t *testing.T) {
	p := validPayload()
	p.ProjectKey = strings.Repeat("a", 64)
	requireIngestAccepted(t, p)

	p = validPayload()
	p.ProjectKey = strings.Repeat("a", 65)
	require.Equal(t, []ingestViolation{{"", "error_event.project_key"}}, ingestViolations(t, p))
}

func TestIngestValidation_PlatformFormat(t *testing.T) {
	for _, platform := range []string{"Go", "go_lang", "go-lang", "gö", "go lang"} {
		t.Run(platform, func(t *testing.T) {
			p := validPayload()
			p.Platform = platform
			require.Equal(t, []ingestViolation{{"", "error_event.platform"}}, ingestViolations(t, p))
		})
	}
}

// TestIngestValidation_EnvironmentFormat covers sentinel-project-environments §4.8 / decision 10's
// widened accepted set: ^[a-z0-9][a-z0-9._-]{0,49}$, now asserted where it is enforced (the
// error_event.environment CEL rule).
func TestIngestValidation_EnvironmentFormat(t *testing.T) {
	for _, env := range []string{"pre-prod", "eu-west-1", "staging-2", "prod_eu", "production", "a", "prod.eu"} {
		t.Run("accepted/"+env, func(t *testing.T) {
			p := validPayload()
			p.Environment = env
			requireIngestAccepted(t, p)
		})
	}

	for _, env := range []string{"Production", "-prod", ".prod", "_prod", "pröd", "pre prod", "eu/west", strings.Repeat("a", 51)} {
		t.Run("rejected/"+env, func(t *testing.T) {
			p := validPayload()
			p.Environment = env
			require.Equal(t, []ingestViolation{{"", "error_event.environment"}}, ingestViolations(t, p))
		})
	}
}

// The four names the environment rule accepts must still be rejected as a platform: the platform rule
// (error_event.platform) was not widened alongside it.
func TestIngestValidation_PlatformStillRejectsWidenedEnvironmentNames(t *testing.T) {
	for _, platform := range []string{"pre-prod", "eu-west-1", "staging-2", "prod_eu"} {
		t.Run(platform, func(t *testing.T) {
			p := validPayload()
			p.Platform = platform
			require.Equal(t, []ingestViolation{{"", "error_event.platform"}}, ingestViolations(t, p))
		})
	}
}

// message is bounded in CODE POINTS by CEL. The deleted ValidatePayload check counted bytes, so a
// 4000-character CJK message (12000 bytes) is accepted - it would newly 400 had that check been wired.
func TestIngestValidation_MessageLength(t *testing.T) {
	p := validPayload()
	p.Message = strings.Repeat("a", 10000)
	requireIngestAccepted(t, p)

	p = validPayload()
	p.Message = strings.Repeat("界", 4000)
	requireIngestAccepted(t, p)

	p = validPayload()
	p.Message = strings.Repeat("a", 10001)
	require.Equal(t, []ingestViolation{{"", "error_event.message"}}, ingestViolations(t, p))
}

func TestIngestValidation_StacktraceLength(t *testing.T) {
	frames := func(n int) []validation.StackFrame {
		out := make([]validation.StackFrame, n)
		for i := range out {
			out[i] = validation.StackFrame{File: "f.go", Line: int32(i), Function: "f"}
		}
		return out
	}
	p := validPayload()
	p.Stacktrace = frames(100)
	requireIngestAccepted(t, p)

	p = validPayload()
	p.Stacktrace = frames(101)
	require.Equal(t, []ingestViolation{{"stacktrace", "repeated.max_items"}}, ingestViolations(t, p))
}

// metadata over 64 KiB is TRUNCATED by shrink now (spec §4.2, I2), never rejected: the full rule is
// asserted in ingestor_bounds_test.go. What stays here is the old row's input, now accepted.
func TestIngestValidation_MetadataOverLimitIsAcceptedAndShrunk(t *testing.T) {
	p := validPayload()
	md := make(map[string]interface{}, 1000)
	for i := 0; i < 1000; i++ {
		md[fmt.Sprintf("key-%04d", i)] = strings.Repeat("v", 60)
	}
	p.Metadata = md
	encoded, _ := json.Marshal(p.Metadata)
	require.Greater(t, len(encoded), validation.MaxMetadataSize, "metadata must exceed 64 KiB to exercise the rule")

	requireIngestAccepted(t, p)
	encoded, _ = json.Marshal(p.Metadata)
	assert.LessOrEqual(t, len(encoded), validation.MaxMetadataSize)
	assert.Equal(t, []interface{}{"metadata"}, p.Metadata[validation.MetadataKeyServerTruncated])
}

// A top-level in-app file over 512 is NOT enforced by the server (spec §4.2, S4): file is a fingerprint
// input, and cutting or refusing it would change the grouping of every existing issue whose events carry
// a longer path. The deleted ValidatePayload check would have newly 400'd this.
func TestIngestValidation_TopLevelFileOver512IsNotEnforced(t *testing.T) {
	long := strings.Repeat("a", 513)
	p := validPayload()
	p.Stacktrace = []validation.StackFrame{{File: long, Line: 1, Function: "f", InApp: true}}
	requireIngestAccepted(t, p)
	assert.Equal(t, long, p.Stacktrace[0].File, "a top-level file is never cut")
}

// sentinel-sourcemaps §4.3: `column` on the wire frame is optional and 1-based; 0 is "unknown" and
// anything below zero is a producer bug refused with the per-frame field name. This is the one rule
// ValidatePayload still rejects on, because the proto carries no rule for it.
func TestValidatePayload_StackFrameColumn(t *testing.T) {
	t.Run("a 1-based column is accepted", func(t *testing.T) {
		p := validPayload()
		p.Stacktrace = []validation.StackFrame{
			{File: "/assets/index-3f2a1c.js", Line: 1, Column: 1234, Function: "f", InApp: true},
		}

		result := validation.ValidatePayload(p)

		require.True(t, result.Valid)
		require.Empty(t, result.Errors)
	})

	t.Run("column absent (0) is accepted: 0 is the unknown value, never an error", func(t *testing.T) {
		p := validPayload()
		p.Stacktrace = []validation.StackFrame{
			{File: "main.go", Line: 10, Function: "main", InApp: true},
		}

		result := validation.ValidatePayload(p)

		require.True(t, result.Valid)
		require.Empty(t, result.Errors)
	})

	t.Run("a negative column is rejected with field stacktrace[i].column", func(t *testing.T) {
		p := validPayload()
		p.Stacktrace = []validation.StackFrame{
			{File: "/assets/index-3f2a1c.js", Line: 1, Column: 5, Function: "f", InApp: true},
			{File: "/assets/vendor-9a0d4e.js", Line: 3, Column: -1, Function: "g", InApp: true},
		}

		result := validation.ValidatePayload(p)

		require.False(t, result.Valid)
		require.Len(t, result.Errors, 1)
		assert.Equal(t, "stacktrace[1].column", result.Errors[0].Field)
		assert.Equal(t, "stacktrace[1].column must be >= 0", result.Errors[0].Message)
	})

	t.Run("in_app=false does not bypass the column rule", func(t *testing.T) {
		p := validPayload()
		p.Stacktrace = []validation.StackFrame{
			{File: "node_modules/x.js", Line: 1, Column: -7, Function: "f", InApp: false},
		}

		result := validation.ValidatePayload(p)

		require.False(t, result.Valid)
		require.Len(t, result.Errors, 1)
		assert.Equal(t, "stacktrace[0].column", result.Errors[0].Field)
	})

	t.Run("an over-long file on the same frame adds no second error: only the column is rejected", func(t *testing.T) {
		p := validPayload()
		p.Stacktrace = []validation.StackFrame{
			{File: strings.Repeat("a", 513), Line: 1, Column: -1, Function: "f", InApp: true},
		}

		result := validation.ValidatePayload(p)

		require.False(t, result.Valid)
		require.Len(t, result.Errors, 1)
		assert.Equal(t, "stacktrace[0].column", result.Errors[0].Field)
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
