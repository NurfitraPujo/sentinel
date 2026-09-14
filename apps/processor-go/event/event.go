package event

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/indexer"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/normalizer"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"github.com/golang/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// maxEventIDLength mirrors error_occurrences.event_id VARCHAR(64) / apps/ingestor-go/service/
// service.go's maxEventIDLength / the proto's "error_event.event_id" CEL rule
// (docs/plans/IDEMPOTENCY_PLAN.md D-g).
const maxEventIDLength = 64

// maxAffectedIDBytes is sentinel-affected-users §4.3's decision-5 bound on a user/tenant identifier:
// issue_affected_users.user_id and issue_affected_tenants.tenant_id are VARCHAR(255), matching
// error_search_index.user_id / tenant_id. Counted in BYTES, as the contract states it — stricter
// than the column (which counts characters), never looser, so a value that passes here always fits.
const maxAffectedIDBytes = 255

type ErrorEvent struct {
	Fingerprint string `json:"fingerprint"`
	ProjectKey  string `json:"project_key"`
	// ProjectID is ErrorEvent.project_id (proto field 16, added alongside
	// release_version in P2-1). When non-empty it is the authenticated
	// project's UUID and must be preferred over any ProjectKey-based lookup
	// — see store.ResolveProjectID and VERIFIED_STATE.md S6. It is empty
	// for any producer that has not been updated to send it, in which case
	// callers fall back to the legacy ProjectKey/name lookup.
	ProjectID      string                 `json:"project_id"`
	Platform       string                 `json:"platform"`
	Environment    string                 `json:"environment"`
	ReleaseVersion string                 `json:"release_version"`
	Message        string                 `json:"message"`
	ErrorClass     string                 `json:"error_class"`
	TraceID        string                 `json:"trace_id"`
	SpanID         string                 `json:"span_id"`
	Stacktrace     []StackFrame           `json:"stacktrace"`
	Metadata       map[string]interface{} `json:"metadata"`
	Timestamp      time.Time              `json:"timestamp"`
	TraceFlags     uint32                 `json:"trace_flags"`
	// EventID is the idempotency key (proto field 17, docs/plans/IDEMPOTENCY_PLAN.md D-a/D-h). Copied
	// in Deserialize's struct literal alongside ProjectID/ReleaseVersion, BEFORE Normalize runs, and
	// MUST NEVER be passed through normalizer.NormalizeString or masker.MaskString: the normalizer's
	// regexes rewrite UUIDs to the literal "<UUID>" and >=6-digit runs to "<NUMERIC_ID>" (see
	// TraceID/SpanID below in Normalize), which would collide every event in an issue onto one key —
	// the identical failure mode as F-TX-1 (D-b's NULLIF gap), on the fully-upgraded path (F-CT-2, B6
	// verbatim). If you are tempted to add a line normalizing/masking this field here, don't — that is
	// exactly the bug this comment exists to prevent, and it is pinned by a unit test that must fail
	// if EventID is ever added to Normalize's field list.
	EventID string `json:"event_id"`
	// AffectedUserID / AffectedTenantID feed the per-issue affected users/tenants rollup
	// (sentinel-affected-users §4.3, decision 3) and, from the same release, error_search_index's
	// user_id/tenant_id columns (decisions 13/19). They are read in Normalize BEFORE
	// `e.Metadata = masker.MaskMap(normalizer.NormalizeMap(e.Metadata))` runs — exactly where the
	// release_version fallback is read, and for the same reason (VERIFIED_STATE.md S5 / B6): the
	// normalizer rewrites every UUID in the map to the literal "<UUID>", every run of >= 6 digits to
	// "<NUMERIC_ID>" and every email to "<EMAIL>", so a rollup fed from the post-normalisation map
	// would count exactly one affected user — "<UUID>" — per issue (contract §1's second fact). The
	// key precedence is indexer.UserIDFromMetadata / TenantIDFromMetadata, the search index's own,
	// verbatim (decision 24). The value here is decision-5-bounded by BoundAffectedID (non-empty,
	// <= 255 bytes, no control character; "" means "nothing usable") and the matching *SkipReason
	// field says why it is "" when a key WAS present — the closed set obs.AffectedRollupReason*, ""
	// when the value was usable or no key existed at all.
	//
	// Like EventID above, these MUST NEVER be passed through normalizer.NormalizeString or
	// masker.MaskString: identity is the exact byte string (decision 6), and a normalised id is the
	// bug this feature exists to route around. If you are tempted to add a line normalising or
	// masking either field in Normalize, don't — tests/unit/processor_event_test.go's
	// TestDeserialize_AffectedIDsSurviveNormalizeByteIdentical fails the moment you do.
	AffectedUserID           string `json:"affected_user_id"`
	AffectedUserSkipReason   string `json:"affected_user_skip_reason,omitempty"`
	AffectedTenantID         string `json:"affected_tenant_id"`
	AffectedTenantSkipReason string `json:"affected_tenant_skip_reason,omitempty"`
}

type StackFrame struct {
	File     string `json:"file"`
	Line     int32  `json:"line"`
	Function string `json:"function"`
	InApp    bool   `json:"in_app"`
	// sentinel-sourcemaps §4.3: copied from the proto's field 5, 1-based as browsers print it;
	// 0 is "unknown" (every pre-feature message, every Go frame) and makes symbolication pass
	// the frame through (`no_column`, §4.4 step 2). omitempty so that a pre-feature frame and a
	// no-column frame serialise identically into error_occurrences.stacktrace -- the JSONB the
	// dashboard reads carries "column" only when there is one.
	Column int32 `json:"column,omitempty"`
}

func (e *ErrorEvent) Normalize(normalizer *normalizer.Normalizer, masker *masker.Masker) {
	e.Message = normalizer.NormalizeString(e.Message)
	e.Message = masker.MaskString(e.Message)
	e.ErrorClass = normalizer.NormalizeString(e.ErrorClass)
	e.TraceID = normalizer.NormalizeString(e.TraceID)
	e.SpanID = normalizer.NormalizeString(e.SpanID)
	if e.Metadata != nil {
		// Read the legacy release_version-in-metadata fallback BEFORE
		// NormalizeMap runs, not after (VERIFIED_STATE.md S5 / B6:
		// normalizer.versionRegex rewrites any semver-looking string to the
		// literal "<VERSION>", so reading it post-normalization always
		// yielded that placeholder and regression detection could never
		// fire). The first-class ErrorEvent.ReleaseVersion field (proto
		// field 15, populated in Deserialize) always wins when present —
		// this is purely a fallback for producers that still smuggle the
		// version through metadata instead of sending the field directly.
		if e.ReleaseVersion == "" {
			if rv, ok := e.Metadata["release_version"].(string); ok {
				e.ReleaseVersion = rv
			}
		}
		// sentinel-affected-users §4.3 (decision 3): the affected user/tenant identifiers, read here
		// for the same reason release_version is — after the next line a UUID reads "<UUID>". See the
		// AffectedUserID field comment. Bounded at the read (decision 5) so every consumer — the
		// side tables in store.StoreEvent and the index overwrite in processEventInternal — sees one
		// value; "present but unusable" is what the *SkipReason fields carry to the after-commit
		// counter. Never normalised, never masked (pinned by a unit test — see the field comment).
		if raw, present := indexer.UserIDFromMetadata(e.Metadata); present {
			e.AffectedUserID, e.AffectedUserSkipReason = BoundAffectedID(raw)
		}
		if raw, present := indexer.TenantIDFromMetadata(e.Metadata); present {
			e.AffectedTenantID, e.AffectedTenantSkipReason = BoundAffectedID(raw)
		}
		e.Metadata = masker.MaskMap(normalizer.NormalizeMap(e.Metadata))
	}
}

// BoundAffectedID is sentinel-affected-users decision 5, modelled on the D-g event_id clamp in
// Deserialize below: a user/tenant identifier is USABLE when it is non-empty, at most
// maxAffectedIDBytes bytes and carries no control character (rune < 0x20 or 0x7f — the same
// predicate D-g applies). A usable value is returned verbatim with reason "". An unusable value is
// returned as "" — treated as absent: no side-table row, no count bump, the event is still stored —
// with the reason from the CLOSED set obs.AffectedRollupReasonEmpty / TooLong / ControlChar, which
// store.StoreEvent records on sentinel_affected_rollup_skipped_total after commit.
//
// Why drop rather than truncate or fail: a truncated identifier is a DIFFERENT identifier and would
// manufacture users; and letting the value reach the VARCHAR(255) / CHECK (<> ”) statements would
// raise a class-22/23 error inside StoreEvent's transaction, which classifyStoreError treats as
// Permanent — dead-lettering a perfectly storable occurrence over a metadata field (the exact
// failure D-g exists to prevent for event_id). Length is counted in bytes, as the contract states
// the bound; NUL in particular cannot be stored in a Postgres varchar at all.
func BoundAffectedID(id string) (string, string) {
	switch {
	case id == "":
		return "", obs.AffectedRollupReasonEmpty
	case len(id) > maxAffectedIDBytes:
		return "", obs.AffectedRollupReasonTooLong
	case strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return "", obs.AffectedRollupReasonControlChar
	}
	return id, ""
}

func Deserialize(data []byte) (*ErrorEvent, error) {
	var protoEvent sentinelv1.ErrorEvent
	if err := proto.Unmarshal(data, &protoEvent); err != nil {
		return nil, fmt.Errorf("failed to unmarshal event: %w", err)
	}

	if err := validateEvent(&protoEvent); err != nil {
		return nil, fmt.Errorf("invalid event: %w", err)
	}

	event := &ErrorEvent{
		ProjectKey: protoEvent.ProjectKey,
		// ProjectID/ReleaseVersion (proto fields 16/15) were added to the
		// wire contract in P2-1 and populated by the ingestor, but this
		// Deserialize never copied them onto ErrorEvent — so the processor
		// silently dropped both on every event (VERIFIED_STATE.md S5, and
		// the processor half of S6's project_id plumbing). Copying them
		// here, before Normalize runs, is what makes the first-class
		// ReleaseVersion field win over the metadata fallback in Normalize
		// above.
		ProjectID:      protoEvent.ProjectId,
		ReleaseVersion: protoEvent.ReleaseVersion,
		Platform:       protoEvent.Platform,
		Environment:    protoEvent.Environment,
		Message:        protoEvent.Message,
		ErrorClass:     protoEvent.ErrorClass,
		TraceID:        protoEvent.TraceId,
		SpanID:         protoEvent.SpanId,
		TraceFlags:     protoEvent.TraceFlags,
		Fingerprint:    protoEvent.Fingerprint,
		// See the EventID field comment on ErrorEvent: copied here, never normalized/masked (D-h).
		EventID: protoEvent.EventId,
	}

	// D-g: the processor-side bounds guard. The proto CEL bound (this.event_id.size() <= 64) and the
	// ingestor's resolveEventID (apps/ingestor-go/service/service.go) are the contract's front door,
	// but neither runs for a direct/legacy publisher: a replayed pre-W0 DLQ message, a future
	// non-ingestor producer, or a test harness bypassing the ingestor entirely can still deliver an
	// oversized or control-character id straight to this deserializer. Left unchecked, that id would
	// reach the VARCHAR(64) insert and fail with 22001 (or the CHECK constraint's 23514) — both class
	// 22/23, which classifyStoreError treats as Permanent, dead-lettering an otherwise perfectly
	// storable event over a dedup key alone (F-CT-3). Dropping the id and keeping the event is strictly
	// better than losing it; this mirrors the ingestor's resolveEventID length/control-character
	// policy but never mints a replacement — the processor only bounds what it accepts, it does not
	// invent ids.
	if event.EventID != "" {
		length := utf8.RuneCountInString(event.EventID)
		reason := ""
		// The obs.EventIDReason* constants are the ingestor's vocabulary for the same two conditions
		// (sentinel_ingest_event_id_replaced_total{reason}); reusing them here keeps one spelling per
		// state even though this is only a log field, never a metric label (F-VW2-2 — two spellings of
		// one vocabulary is a B5 seed).
		switch {
		case length > maxEventIDLength:
			reason = obs.EventIDReasonTooLong
		case strings.ContainsFunc(event.EventID, func(r rune) bool { return r < 0x20 || r == 0x7f }):
			reason = obs.EventIDReasonInvalidChars
		}
		if reason != "" {
			// Never log the value itself (D15 cardinality/PII rule) — only its length and why it
			// was dropped.
			slog.Warn("processor: event_id exceeds the storage bound or carries a control character; dropping the id and storing the event without a dedup key",
				slog.Int("length", length), slog.String("reason", reason))
			event.EventID = ""
		}
	}

	if protoEvent.Timestamp != nil {
		event.Timestamp = protoEvent.Timestamp.AsTime()
	}

	if protoEvent.Metadata != nil {
		event.Metadata = structpbToMap(protoEvent.Metadata)
	}

	for _, frame := range protoEvent.Stacktrace {
		event.Stacktrace = append(event.Stacktrace, StackFrame{
			File:     frame.File,
			Line:     frame.Line,
			Function: frame.Function,
			InApp:    frame.InApp,
			Column:   frame.Column,
		})
	}

	norm := normalizer.NewNormalizer()
	mask := masker.NewMasker()
	event.Normalize(norm, mask)

	if event.Fingerprint == "" || protoEvent.FingerprintOverride {
		frames := make([]fingerprint.StackFrame, len(event.Stacktrace))
		for i, f := range event.Stacktrace {
			frames[i] = fingerprint.StackFrame{
				File:     f.File,
				Line:     f.Line,
				Function: f.Function,
				InApp:    f.InApp,
			}
		}
		event.Fingerprint = fingerprint.Compute(fingerprint.FingerprintConfig{
			// sentinel-project-environments §4.9: the environment is part of issue identity, so it is
			// part of the hash input.
			Environment: event.Environment,
			ErrorClass:  event.ErrorClass,
			Stacktrace:  frames,
		})
	}

	return event, nil
}

func validateEvent(event *sentinelv1.ErrorEvent) error {
	if event.ProjectKey == "" {
		return fmt.Errorf("project_key is required")
	}
	if event.Platform == "" {
		return fmt.Errorf("platform is required")
	}
	if event.Environment == "" {
		return fmt.Errorf("environment is required")
	}
	if event.ErrorClass == "" {
		return fmt.Errorf("error_class is required")
	}
	return nil
}

func structpbToMap(s *structpb.Struct) map[string]interface{} {
	if s == nil {
		return nil
	}
	result := make(map[string]interface{})
	for k, v := range s.AsMap() {
		result[k] = v
	}
	return result
}
