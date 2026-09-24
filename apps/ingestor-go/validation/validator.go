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

	// ---- sentinel-event-schema-v2 (spec §4.1): Sentry-parity metadata, mirroring ErrorEvent fields
	// 14 and 18-29. The JSON names are exactly the proto field names. Decode posture is unchanged: an
	// unknown key is ignored (a future SDK key must not 400 an old ingestor), and a value of the WRONG
	// JSON type fails the whole decode with a 400, as for every field above. ValidatePayload bounds
	// every one of these in place before the proto is built (spec §4.2); none of them ever costs the
	// event.
	Level       string                            `json:"level"`
	Logger      string                            `json:"logger"`
	ServerName  string                            `json:"server_name"`
	Transaction string                            `json:"transaction"`
	Dist        string                            `json:"dist"`
	Source      string                            `json:"source"`
	SDK         *SDKInfo                          `json:"sdk"`
	User        *UserInfo                         `json:"user"`
	Tags        map[string]string                 `json:"tags"`
	Contexts    map[string]map[string]interface{} `json:"contexts"`
	Request     *RequestInfo                      `json:"request"`
	Breadcrumbs []Breadcrumb                      `json:"breadcrumbs"`
	Exception   *ExceptionInterface               `json:"exception"`
	// ReceivedAt is T_recv, stamped by IngestAt and mapped to proto field 30. json:"-": a client can
	// never set it (compare ProjectID's rule above); a body carrying "received_at" is ignored.
	ReceivedAt time.Time `json:"-"`
}

// SDKInfo mirrors sentinel.v1.Sdk.
type SDKInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// UserInfo mirrors sentinel.v1.User. Stored as sent (contract decision 1).
type UserInfo struct {
	ID        string   `json:"id"`
	Username  string   `json:"username"`
	Email     string   `json:"email"`
	IPAddress string   `json:"ip_address"`
	Geo       *GeoInfo `json:"geo"`
}

// GeoInfo mirrors sentinel.v1.Geo.
type GeoInfo struct {
	City        string `json:"city"`
	Region      string `json:"region"`
	CountryCode string `json:"country_code"`
	Subdivision string `json:"subdivision"`
}

// RequestInfo mirrors sentinel.v1.Request. Data is any JSON value.
type RequestInfo struct {
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	QueryString string            `json:"query_string"`
	Headers     map[string]string `json:"headers"`
	Data        interface{}       `json:"data"`
	Env         map[string]string `json:"env"`
}

// Breadcrumb mirrors sentinel.v1.Breadcrumb. Timestamp is RFC 3339, decoded as `timestamp` is; a Unix
// float fails the decode (400), as any type mismatch does.
type Breadcrumb struct {
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"`
	Category  string                 `json:"category"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	Data      map[string]interface{} `json:"data"`
}

// ExceptionInterface mirrors sentinel.v1.ExceptionInterface: Sentry's {"values": [...]}, oldest first
// (the root cause), the exception that was raised LAST.
type ExceptionInterface struct {
	Values []ExceptionValue `json:"values"`
}

// ExceptionValue mirrors sentinel.v1.ExceptionValue. Stacktrace is in Sentinel's order, crashing frame
// first, the same as ErrorPayload.Stacktrace.
type ExceptionValue struct {
	Type       string       `json:"type"`
	Value      string       `json:"value"`
	Mechanism  *Mechanism   `json:"mechanism"`
	Stacktrace []StackFrame `json:"stacktrace"`
}

// Mechanism mirrors sentinel.v1.Mechanism. Handled is tri-state: absent / true / false.
type Mechanism struct {
	Type    string `json:"type"`
	Handled *bool  `json:"handled"`
}

type StackFrame struct {
	File     string `json:"file"`
	Line     int32  `json:"line"`
	Function string `json:"function"`
	InApp    bool   `json:"in_app"`
	// sentinel-sourcemaps §4.3: optional, 1-based as browsers print it; 0 (absent) means
	// "unknown" and the processor passes the frame through. Copied verbatim into the proto's
	// field 5 by mapping.MapPayloadToEvent; a negative value is a 400 from ValidatePayload
	// (`stacktrace[i].column must be >= 0`) -- the proto carries no CEL rule for it.
	Column int32 `json:"column"`
	// sentinel-event-schema-v2 (spec §4.1): proto StackFrame fields 6-10.
	Module      string   `json:"module"`
	AbsPath     string   `json:"abs_path"`
	ContextLine string   `json:"context_line"`
	PreContext  []string `json:"pre_context"`
	PostContext []string `json:"post_context"`
}

// ValidatePayload is the ingestor's per-field bounds step (sentinel-event-schema-v2 spec §4.2-§4.3),
// run by service.IngestAt after the event_id/timestamp resolution and before the proto is built.
//
// It works IN PLACE on payload: every v2 value over its bound is truncated, dropped or replaced
// exactly as the spec's rule table says, every such cut is recorded in the server-owned
// metadata.server_truncated / metadata.server_truncated_keys markers, and an oversized metadata map is
// shrunk. None of that ever costs the event (spec S2).
//
// Valid=false is returned ONLY for a rule the proto cannot express, which is exactly one:
// stacktrace[i].column < 0. Every other rejection (project_key, platform, environment, message,
// error_class, trace/span ids, release_version, fingerprint, frame count, the new v2 CEL tripwires)
// belongs to protovalidate alone, because two overlapping validators with different semantics are how
// a bug hides (error_event.proto's own rule; spec §4.3 lists the four byte-vs-code-point splits the
// old duplicated checks here would have introduced had they been wired).
func ValidatePayload(payload *ErrorPayload) ValidationResult {
	result := ValidationResult{Valid: true}

	for i, frame := range payload.Stacktrace {
		// A negative column is a producer bug, not an unknown: 0 is the "unknown" value
		// (sentinel-sourcemaps §4.3), so nothing below zero has a meaning to store. A top-level frame
		// is not a v2 field, so it keeps its 400; a chain frame's column is clamped instead (bounds.go).
		if frame.Column < 0 {
			result.Valid = false
			result.Errors = append(result.Errors, ValidationError{Field: fmt.Sprintf("stacktrace[%d].column", i), Message: fmt.Sprintf("stacktrace[%d].column must be >= 0", i)})
		}
	}

	applyBounds(payload)

	return result
}

func WriteValidationError(w http.ResponseWriter, result ValidationResult) {
	w.WriteHeader(http.StatusBadRequest)
	w.Write(result.ToJSON())
}
