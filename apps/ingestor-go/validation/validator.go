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
	// EventID is the client-side idempotency key (docs/plans/IDEMPOTENCY_PLAN.md D-a), mirroring
	// ErrorEvent.event_id (proto field 17). Deliberately typed as a plain string, not
	// *string/json.RawMessage: a client that sends "event_id" as a JSON number fails the whole-body
	// decode with a 400, exactly like any other field's type mismatch - this is a deliberate accepted
	// trade-off (IDEMPOTENCY_PLAN.md D-a), not an oversight, and only this SDK sends the field today
	// (as a string). service.IngestService.Ingest is what applies the present/absent/oversized policy
	// (mint a UUIDv4 when absent or >64 chars) and stamps the effective id back into this field before
	// mapping.MapPayloadToEvent runs - see IngestService.Ingest, not this struct.
	EventID     string                 `json:"event_id"`
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
	// Fingerprint is a client-supplied custom grouping key, mirroring
	// ErrorEvent.fingerprint (proto field 12). When non-empty it is written
	// VERBATIM into issues.fingerprint VARCHAR(64) with no server-side hashing,
	// so the length bound below is not cosmetic: an oversized value would fail
	// at the INSERT, after the event had already been accepted and published.
	//
	// Semantics are the processor's (apps/processor-go/event/event.go:177):
	// a fingerprint is recomputed when it is empty OR when FingerprintOverride
	// is set. Supplying one and leaving the flag false is what "custom
	// grouping" means; see FingerprintOverride below for the trap in its name.
	Fingerprint string `json:"fingerprint"`
	// FingerprintOverride mirrors proto field 13. Read the direction carefully:
	// setting it does NOT mean "my fingerprint overrides yours" — it forces the
	// SERVER to recompute and DISCARDS any Fingerprint supplied alongside it.
	// That is what the processor does today, so it is what this decodes; the
	// name is misleading and is documented rather than changed, because
	// changing it would alter behaviour for anything already sending it.
	FingerprintOverride bool `json:"fingerprint_override"`
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
	// MaxFingerprintLength matches issues.fingerprint VARCHAR(64) (1716508800_init.sql)
	// and the "error_event.fingerprint" CEL rule in the proto. A client fingerprint
	// is stored unhashed, so exceeding the column is a real failure mode, not a
	// theoretical one.
	MaxFingerprintLength = 64
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
	} else if !isValidEnvironmentName(payload.Environment) {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "environment", Message: "environment must match ^[a-z0-9][a-z0-9._-]{0,49}$"})
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

	// Rejected here rather than truncated: silently shortening a grouping key would
	// merge issues the caller meant to keep apart, which is worse than a 400 that
	// says what was wrong.
	if len(payload.Fingerprint) > MaxFingerprintLength {
		result.Valid = false
		result.Errors = append(result.Errors, ValidationError{Field: "fingerprint", Message: fmt.Sprintf("fingerprint must not exceed %d characters", MaxFingerprintLength)})
	}

	for i, frame := range payload.Stacktrace {
		if frame.InApp && frame.File != "" && len(frame.File) > MaxFilePathLength {
			result.Valid = false
			result.Errors = append(result.Errors, ValidationError{Field: fmt.Sprintf("stacktrace[%d].file", i), Message: fmt.Sprintf("file path must not exceed %d characters", MaxFilePathLength)})
		}
	}

	return result
}

// isValidEnvironmentName implements sentinel-project-environments §4.8's widened set, decision 10:
//
//	^[a-z0-9][a-z0-9._-]{0,49}$
//
// i.e. a lowercase-alphanumeric first character followed by up to 49 more characters drawn from
// lowercase alphanumerics, dot, underscore and hyphen. `pre-prod`, `eu-west-1`, `staging-2` and
// `prod_eu` — the four names operators actually use — are accepted; `Production` (uppercase) and
// `-prod` (leading separator) are not. The 50-character bound is folded into the pattern rather than
// checked separately, exactly as the proto CEL rule now does it.
//
// This is a NEW function and isValidAlphanumeric is deliberately left exactly as it was. That
// function is SHARED with payload.Platform, whose own CEL rule (error_event.platform) is unchanged
// by this feature: widening it in place would widen `platform` on the Go side only, leaving the Go
// check and the wire check disagreeing about what a platform is — a split that surfaces months later
// as an event rejected by protovalidate after the ingestor accepted it. §11 makes "platform still
// rejects all four names" an assertion for exactly this reason.
//
// Written as an explicit scan rather than a regexp because this runs on every ingested event and the
// pattern is simple enough that a regexp would be the more surprising choice here.
func isValidEnvironmentName(s string) bool {
	if len(s) == 0 || len(s) > 50 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			continue
		case i > 0 && (c == '.' || c == '_' || c == '-'):
			continue
		default:
			return false
		}
	}
	return true
}

// isValidAlphanumeric is payload.Platform's check and, since decision 10, ONLY payload.Platform's.
// Do not widen it: see isValidEnvironmentName above.
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
