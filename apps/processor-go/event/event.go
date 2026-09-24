package event

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/indexer"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/normalizer"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
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
	// Fingerprint is, after Deserialize, the CLIENT's value (proto field fingerprint, "" when none
	// was sent) and, after ComputeFingerprint, issue identity: the client's value verbatim, or the
	// server's hash. Nothing between the two reads it as identity — see ComputeFingerprint.
	Fingerprint string `json:"fingerprint"`
	// FingerprintOverride is the proto's fingerprint_override, carried here (sentinel-sourcemaps
	// §4.4 / be.md task 4) because the fingerprint step no longer runs inside Deserialize: it
	// runs in service.processEventInternal AFTER the project id is resolved and AFTER
	// symbolication, and needs the flag there. The trap in the name is unchanged: true does NOT
	// mean "my fingerprint wins" — it forces a server-side recompute and DISCARDS the value sent
	// with it (event/fingerprint_semantics_test.go pins this).
	FingerprintOverride bool   `json:"fingerprint_override,omitempty"`
	ProjectKey          string `json:"project_key"`
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
	// in Deserialize's struct literal alongside ProjectID/ReleaseVersion, BEFORE Prepare runs, and
	// MUST NEVER be passed through normalizer.NormalizeString or masker.MaskString: the normalizer's
	// regexes rewrite UUIDs to the literal "<UUID>" and >=6-digit runs to "<NUMERIC_ID>" (as TraceID
	// and SpanID were until sentinel-event-schema-v2), which would collide every event in an issue onto one key —
	// the identical failure mode as F-TX-1 (D-b's NULLIF gap), on the fully-upgraded path (F-CT-2, B6
	// verbatim). If you are tempted to add a line normalizing/masking this field here, don't — that is
	// exactly the bug this comment exists to prevent, and it is pinned by a unit test that must fail
	// if EventID is ever added to Prepare's field list.
	EventID string `json:"event_id"`
	// AffectedUserID / AffectedTenantID feed the per-issue affected users/tenants rollup
	// (sentinel-affected-users §4.3, decision 3) and, from the same release, error_search_index's
	// user_id/tenant_id columns (decisions 13/19). They are read in Prepare, from raw values, BEFORE
	// `e.Metadata = masker.MaskMap(e.Metadata)` runs — exactly where the release_version fallback is
	// read. The reason it was introduced (VERIFIED_STATE.md S5 / B6) was that the normalizer, which ran
	// over the stored map until sentinel-event-schema-v2, rewrites every UUID in the map to the literal "<UUID>", every run of >= 6 digits to
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
	// masking either field in Prepare, don't — tests/unit/processor_event_test.go's
	// TestDeserialize_AffectedIDsSurvivePrepareByteIdentical fails the moment you do.
	AffectedUserID           string `json:"affected_user_id"`
	AffectedUserSkipReason   string `json:"affected_user_skip_reason,omitempty"`
	AffectedTenantID         string `json:"affected_tenant_id"`
	AffectedTenantSkipReason string `json:"affected_tenant_skip_reason,omitempty"`

	// ---- sentinel-event-schema-v2 §6.1 (TASK-05): the v2 fields, copied off the proto by Deserialize
	// and stored per occurrence. JSON tags are the wire names with omitempty, so the processor's JSONB
	// marshalling produces spec §5.2's shapes (proto field names, empty values omitted).
	Level       string                 `json:"level,omitempty"`
	Logger      string                 `json:"logger,omitempty"`
	ServerName  string                 `json:"server_name,omitempty"`
	Transaction string                 `json:"transaction,omitempty"`
	Dist        string                 `json:"dist,omitempty"`
	Source      string                 `json:"source,omitempty"`
	SDK         *SDK                   `json:"sdk,omitempty"`
	User        *User                  `json:"user,omitempty"`
	Tags        map[string]string      `json:"tags,omitempty"`
	Contexts    map[string]interface{} `json:"contexts,omitempty"`
	Request     *Request               `json:"request,omitempty"`
	Breadcrumbs []Breadcrumb           `json:"breadcrumbs,omitempty"`
	Exception   *Exception             `json:"exception,omitempty"`
	// ReceivedAt is proto field 30, the ingestor's T_recv; zero for a message published before the
	// ingestor carried it (a DLQ replay), which the occurrence stores as NULL.
	ReceivedAt time.Time `json:"received_at,omitempty"`
}

// SDK mirrors sentinel.v1.Sdk. Stored as sent.
type SDK struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

// User mirrors sentinel.v1.User. Stored as sent (contract decision 1): never masked.
type User struct {
	ID        string `json:"id,omitempty"`
	Username  string `json:"username,omitempty"`
	Email     string `json:"email,omitempty"`
	IPAddress string `json:"ip_address,omitempty"`
	Geo       *Geo   `json:"geo,omitempty"`
}

// Geo mirrors sentinel.v1.Geo.
type Geo struct {
	City        string `json:"city,omitempty"`
	Region      string `json:"region,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Subdivision string `json:"subdivision,omitempty"`
}

// Request mirrors sentinel.v1.Request. Data is any JSON value (object, array, string, number, bool).
type Request struct {
	Method      string            `json:"method,omitempty"`
	URL         string            `json:"url,omitempty"`
	QueryString string            `json:"query_string,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Data        interface{}       `json:"data,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// Breadcrumb mirrors sentinel.v1.Breadcrumb. Timestamp is RFC 3339 (UTC), "" when the crumb had none.
type Breadcrumb struct {
	Timestamp string                 `json:"timestamp,omitempty"`
	Type      string                 `json:"type,omitempty"`
	Category  string                 `json:"category,omitempty"`
	Level     string                 `json:"level,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// Exception mirrors sentinel.v1.ExceptionInterface, Sentry's {"values": [...]}: oldest first, the
// exception that was raised LAST. Evidence only: it is never a fingerprint input (spec §6.4, OQ-2).
type Exception struct {
	Values []ExceptionValue `json:"values"`
}

// ExceptionValue mirrors sentinel.v1.ExceptionValue. Stacktrace is in Sentinel's frame order,
// crashing frame first, like ErrorEvent.Stacktrace.
type ExceptionValue struct {
	Type       string       `json:"type,omitempty"`
	Value      string       `json:"value,omitempty"`
	Mechanism  *Mechanism   `json:"mechanism,omitempty"`
	Stacktrace []StackFrame `json:"stacktrace,omitempty"`
}

// Mechanism mirrors sentinel.v1.Mechanism. Handled is tri-state: absent / true / false.
type Mechanism struct {
	Type    string `json:"type,omitempty"`
	Handled *bool  `json:"handled,omitempty"`
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
	// Symbolicated and Raw are sentinel-sourcemaps §4.4 step 6's record of a rewrite, written by
	// service.processEventInternal from symbolicate.Frame after the stage ran, and step 7's
	// absence otherwise: both omitempty, so a frame that passed through carries NEITHER key —
	// absent, not false — and a pre-feature occurrence and an unsymbolicated one are
	// byte-identical in error_occurrences.stacktrace. The dashboard reads
	// `frame.symbolicated === true`; Raw holds the file/line/column/function the SDK sent.
	Symbolicated bool                  `json:"symbolicated,omitempty"`
	Raw          *symbolicate.RawFrame `json:"raw,omitempty"`
	// sentinel-event-schema-v2 §3.2: proto StackFrame fields 6-10. omitempty, so a pre-v2 frame
	// serialises byte-identically into error_occurrences.stacktrace (the Column argument above).
	Module      string   `json:"module,omitempty"`
	AbsPath     string   `json:"abs_path,omitempty"`
	ContextLine string   `json:"context_line,omitempty"`
	PreContext  []string `json:"pre_context,omitempty"`
	PostContext []string `json:"post_context,omitempty"`
}

// Prepare is sentinel-event-schema-v2 §6.2 (TASK-06), the replacement for Normalize(n, m): it runs
// the order-sensitive reads first and then masks. It NEVER calls the normalizer — stored values are
// masked and verbatim (contract decision 5); normalization now runs only inside ComputeFingerprint, on
// a copy of ErrorClass, which is the one fingerprint input it ever touched (the §6.2 proof). The order
// is fixed:
//
//  1. the release_version fallback, read from the raw metadata;
//  2. the OQ-3 affected-id reads (readAffectedIDs), from raw values;
//  3. masking: Message through MaskString, Metadata through MaskMap (keys verbatim), the v2 fields per
//     §6.2's table, and tags LAST among the reads' inputs — after step 2, so a PAN-shaped or
//     sensitive-keyed tenant tag reaches the rollup as sent and only the stored copy is redacted.
//
// ErrorClass, TraceID and SpanID are stored raw.
func (e *ErrorEvent) Prepare(masker *masker.Masker) {
	// Read the legacy release_version-in-metadata fallback from the RAW map (VERIFIED_STATE.md S5 /
	// B6: reading it after a rewrite yielded a placeholder, and regression detection never fired). The
	// first-class ErrorEvent.ReleaseVersion field (proto field 15, populated in Deserialize) always
	// wins when present — this is purely a fallback for producers that still smuggle the version
	// through metadata instead of sending the field directly.
	if e.ReleaseVersion == "" {
		if rv, ok := e.Metadata["release_version"].(string); ok {
			e.ReleaseVersion = rv
		}
	}
	// sentinel-affected-users §4.3 (decision 3) + sentinel-event-schema-v2 §6.3 (OQ-3): the affected
	// user/tenant identifiers, read from raw values before anything is masked. Bounded at the read
	// (decision 5) so every consumer — the side tables in store.StoreEvent and the index overwrite in
	// processEventInternal — sees one value; "present but unusable" is what the *SkipReason fields
	// carry to the after-commit counter. Never masked (pinned by a unit test — see the field comment).
	e.readAffectedIDs()

	e.Message = masker.MaskString(e.Message)
	if e.Metadata != nil {
		e.Metadata = masker.MaskMap(e.Metadata)
	}
	e.maskV2(masker)
	// TASK-08 (amended decision 4): tag values are masked like metadata — a sensitive key's value is
	// replaced, every other value goes through MaskString (PAN included). After readAffectedIDs.
	e.Tags = maskStringMap(masker, e.Tags)
}

// readAffectedIDs is sentinel-event-schema-v2 §6.3 (OQ-3) over sentinel-affected-users §4.3: the
// typed value takes precedence, metadata is the fallback. AffectedUserID comes from user.id when it
// is non-empty, otherwise from indexer.UserIDFromMetadata exactly as before; AffectedTenantID comes
// from tags["tenant_id"] when that key is present, otherwise from indexer.TenantIDFromMetadata. A
// typed value that is present but unusable WINS and records its skip reason, with no fallback —
// indexer's own "an empty string at a higher-precedence key wins" rule. It reads RAW values and must
// run before anything masks or normalises (the field comment on AffectedUserID).
func (e *ErrorEvent) readAffectedIDs() {
	if e.User != nil && e.User.ID != "" {
		e.AffectedUserID, e.AffectedUserSkipReason = BoundAffectedID(e.User.ID)
	} else if raw, present := indexer.UserIDFromMetadata(e.Metadata); present {
		e.AffectedUserID, e.AffectedUserSkipReason = BoundAffectedID(raw)
	}
	if raw, present := e.Tags["tenant_id"]; present {
		e.AffectedTenantID, e.AffectedTenantSkipReason = BoundAffectedID(raw)
	} else if raw, present := indexer.TenantIDFromMetadata(e.Metadata); present {
		e.AffectedTenantID, e.AffectedTenantSkipReason = BoundAffectedID(raw)
	}
}

// maskV2 is spec §6.2's table for the v2 fields: free text through MaskString, map-shaped values
// through MaskMap (key-based redaction, recursive). User, SDK, the string labels and frames are
// stored as sent. The v2 fields are masked but NEVER normalised (decision 5).
func (e *ErrorEvent) maskV2(m *masker.Masker) {
	if r := e.Request; r != nil {
		r.URL = m.MaskString(r.URL)
		r.QueryString = m.MaskString(r.QueryString)
		r.Headers = maskStringMap(m, r.Headers)
		r.Env = maskStringMap(m, r.Env)
		switch data := r.Data.(type) {
		case map[string]interface{}:
			r.Data = m.MaskMap(data)
		case []interface{}:
			r.Data = m.MaskSlice(data)
		case string:
			r.Data = m.MaskString(data)
		}
	}
	for i := range e.Breadcrumbs {
		b := &e.Breadcrumbs[i]
		b.Message = m.MaskString(b.Message)
		if b.Data != nil {
			b.Data = m.MaskMap(b.Data)
		}
	}
	if e.Contexts != nil {
		e.Contexts = m.MaskMap(e.Contexts)
	}
	if e.Exception != nil {
		for i := range e.Exception.Values {
			e.Exception.Values[i].Value = m.MaskString(e.Exception.Values[i].Value)
		}
	}
}

// maskStringMap runs MaskMap over a string->string map and back. Every value stays a string: a
// sensitive key's value becomes "***REDACTED***", every other value goes through MaskString.
func maskStringMap(m *masker.Masker, in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	asAny := make(map[string]interface{}, len(in))
	for k, v := range in {
		asAny[k] = v
	}
	out := make(map[string]string, len(in))
	for k, v := range m.MaskMap(asAny) {
		out[k], _ = v.(string)
	}
	return out
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
		// here, before Prepare runs, is what makes the first-class
		// ReleaseVersion field win over the metadata fallback in Prepare
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
		// The override flag rides with the client's value to ComputeFingerprint (see the field).
		FingerprintOverride: protoEvent.FingerprintOverride,
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

	event.Stacktrace = framesFromProto(protoEvent.Stacktrace)
	copyV2Fields(event, &protoEvent)

	event.Prepare(masker.NewMasker())

	// The fingerprint is NOT computed here any more (sentinel-sourcemaps §4.4, be.md task 4): it
	// used to be the last thing Deserialize did, which made it impossible to symbolicate the
	// frames first — symbolication needs the resolved project id and a database/storage read,
	// and Deserialize is a pure function of the bytes. ComputeFingerprint below is the same block,
	// called by service.processEventInternal after ResolveProjectID and after the symbolication
	// stage; until then event.Fingerprint is the client's value (or "") and FingerprintOverride
	// says what to do with it.
	return event, nil
}

// ComputeFingerprint is the fingerprint step that Deserialize used to end with, split out so it
// can run AFTER the project id is resolved and AFTER symbolication rewrote the frames
// (sentinel-sourcemaps §4.4: "symbolicated after the project id is known and before
// fingerprint.Compute is called"). It is the ONLY writer of issue identity on the event:
//
//   - Fingerprint == "" (the normal case) or FingerprintOverride: the server computes one over
//     Environment, ErrorClass and the frames AS THEY ARE NOW — rewritten if the stage applied,
//     as sent otherwise — with each frame's Symbolicated flag, so Compute strips build hashes
//     from the frames that still carry a minified name and from no other (decision 44).
//     FingerprintOverride true DISCARDS a client value sent with it.
//   - otherwise the client's Fingerprint is stored verbatim, whether or not the frames were
//     rewritten (flow S7; Compute's early return, untouched — the custom-fingerprint path is
//     deliberately not environment-prefixed either, see fingerprint.Compute).
//
// normalisePlatforms is §4.4's gate set (symbolicate.Config.Platforms — SYMBOLICATE_PLATFORMS,
// unset = {javascript, browser}, set and empty = off) handed to Compute as
// FingerprintConfig.NormalisePlatforms, so the build-hash normalisation follows the same kill
// switch as the stage. Idempotent on a second call only for the custom path; the computed path
// is a pure function of the event's current fields, so calling it twice on unchanged fields
// yields the same value.
func (e *ErrorEvent) ComputeFingerprint(normalisePlatforms []string) {
	if e.Fingerprint != "" && !e.FingerprintOverride {
		return
	}
	frames := make([]fingerprint.StackFrame, len(e.Stacktrace))
	for i, f := range e.Stacktrace {
		frames[i] = fingerprint.StackFrame{
			File:     f.File,
			Line:     f.Line,
			Function: f.Function,
			InApp:    f.InApp,
			// Decision 44: Compute strips build hashes only from a frame the stage did not
			// rewrite; a rewritten File is a source path. Step 6's record is the signal.
			Symbolicated: f.Symbolicated,
		}
	}
	e.Fingerprint = fingerprint.Compute(fingerprint.FingerprintConfig{
		// sentinel-project-environments §4.9: the environment is part of issue identity, so it is
		// part of the hash input.
		Environment: e.Environment,
		// sentinel-event-schema-v2 §6.2 (TASK-06): the one place normalization still runs. ErrorClass
		// used to reach Compute already normalized by Normalize; it is now stored raw, so the SAME pure,
		// stateless, idempotent function is applied to it here, on the way into the hash. Fingerprints
		// are byte-identical (tests/unit/fingerprint_golden_test.go, whose normalizable-class fixture
		// goes red if this call is removed).
		ErrorClass:         normalizer.NewNormalizer().NormalizeString(e.ErrorClass),
		Stacktrace:         frames,
		Platform:           e.Platform,
		NormalisePlatforms: normalisePlatforms,
	})
}

// Symbolicate runs sentinel-sourcemaps §4.4's stage over this event's frames and writes the
// rewrite back: for every frame the stage applied, File/Line/Column/Function become the original
// position, InApp becomes the value the stage re-derived from that position
// (sentinel-sourcemaps-turbopack-grouping §4.2; the SDK's value is under Raw.InApp) and
// Symbolicated/Raw carry step 6's record; a frame that passed through is left exactly as it was
// — InApp included, no key, not `symbolicated: false` (step 7). projectID MUST be the resolved
// project (store.ResolveProjectID's return), never anything read from a frame; releaseVersion is
// e.ReleaseVersion after Prepare's metadata fallback. The caller has already passed
// symbolicate.Gate and counts the returned per-frame outcomes; this method never fails the
// event (the stage returns a Result, not an error — D7).
//
// Here rather than in the service so the order the contract fixes — Deserialize, resolve,
// Symbolicate, ComputeFingerprint — is exercisable end to end without a database
// (tests/unit/processor_event_test.go drives it with a stubbed Store/Fetcher over the committed
// fixture map); the service adds the gate, the span and the metrics around it.
func (e *ErrorEvent) Symbolicate(ctx context.Context, stage *symbolicate.Service, projectID string) symbolicate.Result {
	frames := make([]symbolicate.Frame, len(e.Stacktrace))
	for i, f := range e.Stacktrace {
		frames[i] = symbolicate.Frame{File: f.File, Line: f.Line, Column: f.Column, Function: f.Function, InApp: f.InApp}
	}
	res := stage.Symbolicate(ctx, projectID, e.ReleaseVersion, frames)
	for i, f := range frames {
		if !f.Symbolicated {
			continue
		}
		e.Stacktrace[i].File = f.File
		e.Stacktrace[i].Line = f.Line
		e.Stacktrace[i].Column = f.Column
		e.Stacktrace[i].Function = f.Function
		e.Stacktrace[i].InApp = f.InApp
		e.Stacktrace[i].Symbolicated = true
		e.Stacktrace[i].Raw = f.Raw
	}
	return res
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

// framesFromProto copies proto frames, the v2 fields (6-10) included. nil for no frames, as before.
func framesFromProto(in []*sentinelv1.StackFrame) []StackFrame {
	var out []StackFrame
	for _, frame := range in {
		out = append(out, StackFrame{
			File:        frame.File,
			Line:        frame.Line,
			Function:    frame.Function,
			InApp:       frame.InApp,
			Column:      frame.Column,
			Module:      frame.Module,
			AbsPath:     frame.AbsPath,
			ContextLine: frame.ContextLine,
			PreContext:  frame.PreContext,
			PostContext: frame.PostContext,
		})
	}
	return out
}

// copyV2Fields is spec §6.1's Deserialize half: every v2 field, source and received_at, off the
// proto. Struct/Value go through AsMap/AsInterface. An absent message stays nil, so the occurrence
// column is NULL.
func copyV2Fields(e *ErrorEvent, p *sentinelv1.ErrorEvent) {
	e.Level = p.Level
	e.Logger = p.Logger
	e.ServerName = p.ServerName
	e.Transaction = p.Transaction
	e.Dist = p.Dist
	e.Source = p.Source
	if p.ReceivedAt != nil {
		e.ReceivedAt = p.ReceivedAt.AsTime()
	}
	if sdk := p.Sdk; sdk != nil {
		e.SDK = &SDK{Name: sdk.Name, Version: sdk.Version}
	}
	if u := p.User; u != nil {
		e.User = &User{ID: u.Id, Username: u.Username, Email: u.Email, IPAddress: u.IpAddress}
		if g := u.Geo; g != nil {
			e.User.Geo = &Geo{City: g.City, Region: g.Region, CountryCode: g.CountryCode, Subdivision: g.Subdivision}
		}
	}
	if len(p.Tags) > 0 {
		e.Tags = make(map[string]string, len(p.Tags))
		for k, v := range p.Tags {
			e.Tags[k] = v
		}
	}
	if len(p.Contexts) > 0 {
		e.Contexts = make(map[string]interface{}, len(p.Contexts))
		for k, v := range p.Contexts {
			e.Contexts[k] = structpbToMap(v)
		}
	}
	if r := p.Request; r != nil {
		e.Request = &Request{Method: r.Method, URL: r.Url, QueryString: r.QueryString, Headers: r.Headers, Env: r.Env}
		if r.Data != nil {
			e.Request.Data = r.Data.AsInterface()
		}
	}
	for _, b := range p.Breadcrumbs {
		crumb := Breadcrumb{Type: b.Type, Category: b.Category, Level: b.Level, Message: b.Message, Data: structpbToMap(b.Data)}
		if b.Timestamp != nil {
			crumb.Timestamp = b.Timestamp.AsTime().UTC().Format(time.RFC3339Nano)
		}
		e.Breadcrumbs = append(e.Breadcrumbs, crumb)
	}
	if ex := p.Exception; ex != nil && len(ex.Values) > 0 {
		e.Exception = &Exception{}
		for _, v := range ex.Values {
			val := ExceptionValue{Type: v.Type, Value: v.Value, Stacktrace: framesFromProto(v.Stacktrace)}
			if mech := v.Mechanism; mech != nil {
				val.Mechanism = &Mechanism{Type: mech.Type, Handled: mech.Handled}
			}
			e.Exception.Values = append(e.Exception.Values, val)
		}
	}
}
