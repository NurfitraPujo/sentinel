package validation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationError `json:"errors,omitempty"`
}

func (r ValidationResult) ToJSON() []byte {
	data, _ := json.Marshal(r)
	return data
}

type ErrorPayload struct {
	ProjectKey  string                 `json:"project_key"`
	Platform    string                 `json:"platform"`
	Environment string                 `json:"environment"`
	Message     string                 `json:"message"`
	ErrorClass  string                 `json:"error_class"`
	TraceID     string                 `json:"trace_id"`
	SpanID      string                 `json:"span_id"`
	Stacktrace  []StackFrame           `json:"stacktrace"`
	Metadata    map[string]interface{} `json:"metadata"`
	Timestamp   time.Time              `json:"timestamp"`
	TraceFlags  uint32                 `json:"trace_flags"`
	// ReleaseVersion is the application release/version string (e.g. semver,
	// git SHA). First-class field mirroring ErrorEvent.release_version (proto
	// field 15) so it survives processor-side normalization untouched — see
	// VERIFIED_STATE.md S5.
	ReleaseVersion string `json:"release_version"`
	// ProjectID mirrors ErrorEvent.project_id (proto field 16). It is decoded
	// here so the field exists on the wire type, but it MUST NOT be trusted
	// from the request body in production: it has to be overwritten from the
	// authenticated project resolved by auth.APIKeyAuthenticator.Middleware
	// (see apps/ingestor-go/auth/apikey.go:83-86, which already resolves the
	// authenticated project into the request context and today throws it
	// away — VERIFIED_STATE.md S6/B7). See the TODO(P3-1) at the call sites
	// in main.go for where that override belongs; full enforcement (403 on
	// mismatch, deleting the by-name project lookup) is P3-1's job, not this
	// change's.
	ProjectID string `json:"project_id"`
}

type StackFrame struct {
	File     string `json:"file"`
	Line     int32  `json:"line"`
	Function string `json:"function"`
	InApp    bool   `json:"in_app"`
}

const (
	MaxStacktraceFrames = 100
	MaxMetadataSize     = 64 * 1024
	MaxMessageLength    = 10000
	MaxFilePathLength   = 512
)

func ValidatePayload(payload *ErrorPayload) ValidationResult {
	result := ValidationResult{Valid: true}

	if payload.ProjectKey == "" {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "project_key", Message: "project_key is required"})
	} else if len(payload.ProjectKey) > 64 {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "project_key", Message: "project_key must not exceed 64 characters"})
	}

	if payload.Platform == "" {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "platform", Message: "platform is required"})
	} else if !isValidAlphanumeric(payload.Platform) {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "platform", Message: "platform must be lowercase alphanumeric"})
	}

	if payload.Environment == "" {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "environment", Message: "environment is required"})
	} else if !isValidAlphanumeric(payload.Environment) {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "environment", Message: "environment must be lowercase alphanumeric"})
	}

	if len(payload.Message) > MaxMessageLength {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "message", Message: fmt.Sprintf("message must not exceed %d characters", MaxMessageLength)})
	}

	if payload.ErrorClass == "" {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "error_class", Message: "error_class is required"})
	}

	if len(payload.Stacktrace) > MaxStacktraceFrames {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "stacktrace", Message: fmt.Sprintf("stacktrace must not exceed %d frames", MaxStacktraceFrames)})
	}

	metadataBytes, _ := json.Marshal(payload.Metadata)
	if len(metadataBytes) > MaxMetadataSize {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "metadata", Message: fmt.Sprintf("metadata must not exceed %d bytes", MaxMetadataSize)})
	}

	for i, frame := range payload.Stacktrace {
		if frame.InApp && frame.File != "" && len(frame.File) > MaxFilePathLength {
			result.Valid = false
			result.Errors = append(result.Errors, ValidationError{Field: fmt.Sprintf("stacktrace[%d].file", i), Message: fmt.Sprintf("file path must not exceed %d characters", MaxFilePathLength)})
		}
	}

	return result
}

func isValidAlphanumeric(s string) bool {
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func WriteValidationError(w http.ResponseWriter, result ValidationResult) {
	w.WriteHeader(http.StatusBadRequest)
	w.Write(result.ToJSON())
}
