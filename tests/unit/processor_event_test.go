package unit

import (
	"encoding/json"
	"strings"
	"testing"
	_ "unsafe"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/normalizer"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"github.com/golang/protobuf/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// These declarations expose package-private helpers to this external test package
// without changing production visibility.
//
//go:linkname validateEvent github.com/NurfitraPujo/sentinel/apps/processor-go/event.validateEvent
func validateEvent(*sentinelv1.ErrorEvent) error

//go:linkname structpbToMap github.com/NurfitraPujo/sentinel/apps/processor-go/event.structpbToMap
func structpbToMap(*structpb.Struct) map[string]interface{}

func validProtoEvent() *sentinelv1.ErrorEvent {
	return &sentinelv1.ErrorEvent{
		ProjectKey:  "project-key",
		Platform:    "go",
		Environment: "production",
		Message:     "request failed",
		ErrorClass:  "RuntimeError",
	}
}

func marshalProtoEvent(t *testing.T, protoEvent *sentinelv1.ErrorEvent) []byte {
	t.Helper()

	data, err := proto.Marshal(protoEvent)
	require.NoError(t, err)
	return data
}

// deserializeAndFingerprint is the processor's order since sentinel-sourcemaps §4.4 (be.md task
// 4): event.Deserialize, then — after processEventInternal has resolved the project and run the
// symbolication stage — ErrorEvent.ComputeFingerprint. The TestDeserialize_*Fingerprint* cases
// below pinned "computed after Normalize, custom fingerprint verbatim, override discards" when
// the computation was the last thing Deserialize did; they moved to this call with it, none
// deleted (contract §11's "existing, moved" boundary). nil is the gate set with the stage off —
// these events are platform `go`, outside it either way, so the hash input is ccc52aa's.
func deserializeAndFingerprint(t *testing.T, protoEvent *sentinelv1.ErrorEvent) *event.ErrorEvent {
	t.Helper()
	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)
	got.ComputeFingerprint(nil)
	return got
}

func TestValidateEvent(t *testing.T) {
	t.Run("fully populated event", func(t *testing.T) {
		assert.NoError(t, validateEvent(validProtoEvent()))
	})

	tests := []struct {
		name      string
		clear     func(*sentinelv1.ErrorEvent)
		wantError string
	}{
		{
			name:      "project key is empty",
			clear:     func(e *sentinelv1.ErrorEvent) { e.ProjectKey = "" },
			wantError: "project_key is required",
		},
		{
			name:      "platform is empty",
			clear:     func(e *sentinelv1.ErrorEvent) { e.Platform = "" },
			wantError: "platform is required",
		},
		{
			name:      "environment is empty",
			clear:     func(e *sentinelv1.ErrorEvent) { e.Environment = "" },
			wantError: "environment is required",
		},
		{
			name:      "error class is empty",
			clear:     func(e *sentinelv1.ErrorEvent) { e.ErrorClass = "" },
			wantError: "error_class is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protoEvent := validProtoEvent()
			tt.clear(protoEvent)

			err := validateEvent(protoEvent)
			require.EqualError(t, err, tt.wantError)
		})
	}
}

func TestDeserialize_HappyPathMapsAllFields(t *testing.T) {
	timestamp := timestamppb.Now()
	metadata, err := structpb.NewStruct(map[string]interface{}{
		"attempt": float64(3),
		"active":  true,
		"label":   "worker",
	})
	require.NoError(t, err)

	protoEvent := validProtoEvent()
	protoEvent.TraceId = "trace-123"
	protoEvent.SpanId = "span-456"
	protoEvent.TraceFlags = 1
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.Timestamp = timestamp
	protoEvent.Metadata = metadata
	protoEvent.Stacktrace = []*sentinelv1.StackFrame{
		{File: "main.go", Line: 42, Function: "main.process", InApp: true},
		{File: "runtime.go", Line: 7, Function: "runtime.goexit", InApp: false},
		// sentinel-sourcemaps §4.3: proto field 5, a browser frame's 1-based column.
		{File: "/assets/index-3f2a1c.js", Line: 1, Column: 1234, Function: "onClick", InApp: true},
	}
	// event_id (proto field 17, docs/plans/IDEMPOTENCY_PLAN.md W0/W2). This function has silently
	// dropped a newly-added field TWICE before (S5, S6) — asserting it here is what would have caught
	// both.
	protoEvent.EventId = "evt-happy-path-0001"

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)

	assert.Equal(t, protoEvent.ProjectKey, got.ProjectKey)
	assert.Equal(t, protoEvent.Platform, got.Platform)
	assert.Equal(t, protoEvent.Environment, got.Environment)
	assert.Equal(t, protoEvent.Message, got.Message)
	assert.Equal(t, protoEvent.ErrorClass, got.ErrorClass)
	assert.Equal(t, protoEvent.TraceId, got.TraceID)
	assert.Equal(t, protoEvent.SpanId, got.SpanID)
	assert.Equal(t, protoEvent.TraceFlags, got.TraceFlags)
	assert.Equal(t, protoEvent.Fingerprint, got.Fingerprint)
	assert.Equal(t, timestamp.AsTime(), got.Timestamp)
	assert.Equal(t, metadata.AsMap(), got.Metadata)
	assert.Equal(t, protoEvent.EventId, got.EventID)
	assert.Equal(t, []event.StackFrame{
		{File: "main.go", Line: 42, Function: "main.process", InApp: true},
		{File: "runtime.go", Line: 7, Function: "runtime.goexit", InApp: false},
		{File: "/assets/index-3f2a1c.js", Line: 1, Column: 1234, Function: "onClick", InApp: true},
	}, got.Stacktrace)
}

// TestDeserialize_StackFrameColumnRoundTripsAndIsOmittedWhenZero pins sentinel-sourcemaps §4.3's
// processor half: proto field 5 is copied onto event.StackFrame.Column, and the JSON the occurrence
// stores (processor_service.go marshals event.Stacktrace verbatim into error_occurrences.stacktrace)
// carries "column" ONLY when there is one -- a pre-feature message (no field 5) and a no-column
// frame serialise identically, so nothing downstream can tell the two apart or has to.
func TestDeserialize_StackFrameColumnRoundTripsAndIsOmittedWhenZero(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Platform = "javascript"
	protoEvent.Stacktrace = []*sentinelv1.StackFrame{
		{File: "/assets/index-3f2a1c.js", Line: 1, Column: 1234, Function: "onClick", InApp: true},
		{File: "/assets/vendor-9a0d4e.js", Line: 12, Function: "dispatch", InApp: false},
	}

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)
	require.Len(t, got.Stacktrace, 2)
	assert.Equal(t, int32(1234), got.Stacktrace[0].Column)
	assert.Equal(t, int32(0), got.Stacktrace[1].Column)

	stored, err := json.Marshal(got.Stacktrace)
	require.NoError(t, err)
	var frames []map[string]any
	require.NoError(t, json.Unmarshal(stored, &frames))
	require.Len(t, frames, 2)
	assert.Equal(t, float64(1234), frames[0]["column"], "the frame with a column stores it")
	_, hasColumn := frames[1]["column"]
	assert.False(t, hasColumn, "a zero column must not appear in the stored JSONB (omitempty): %s", stored)
	// The other four keys are as before on both frames -- omitempty touches only column.
	for _, frame := range frames {
		for _, key := range []string{"file", "line", "function", "in_app"} {
			assert.Contains(t, frame, key)
		}
	}
}

// TestDeserialize_EventIDSurvivesNormalizeByteIdentical pins D-h
// (docs/plans/IDEMPOTENCY_PLAN.md): a UUIDv4 event_id must come out of Deserialize+Normalize
// byte-for-byte identical to what went in. normalizer.NormalizeString rewrites any string that LOOKS
// like a UUID to the literal "<UUID>" — TraceID/SpanID intentionally get this treatment in Normalize,
// but EventID must never be passed through it, or every event on an issue would collide onto one
// dedup key (the F-TX-1 failure mode, on the fully-upgraded path). This test MUST fail if EventID is
// ever added to Normalize's field list.
func TestDeserialize_EventIDSurvivesNormalizeByteIdentical(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.EventId = "123e4567-e89b-12d3-a456-426614174000" // a real UUIDv4 shape

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)

	assert.Equal(t, "123e4567-e89b-12d3-a456-426614174000", got.EventID,
		"EventID must survive Deserialize+Normalize byte-identical — if this now reads \"<UUID>\", "+
			"EventID was added to Normalize's rewrite list (D-h regression)")
}

// TestDeserialize_OversizedEventIDIsDropped pins D-g: a direct/legacy publisher that bypasses the
// ingestor's CEL/resolveEventID guard can still deliver an event_id longer than the storage bound
// (error_occurrences.event_id VARCHAR(64)). The processor must preserve the EVENT and drop only the
// id, replacing it with "" (-> NULL at the store.StoreEvent insert site) rather than let it reach a
// 22001/23514 and dead-letter an otherwise perfectly storable event.
func TestDeserialize_OversizedEventIDIsDropped(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.EventId = strings.Repeat("a", 65) // one rune over the 64-rune bound

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err, "an oversized event_id must not fail the whole event")
	assert.Empty(t, got.EventID, "an oversized event_id must be dropped to \"\", not truncated or kept")
}

// TestDeserialize_ControlCharacterEventIDIsDropped pins D-g's second guard: a control character (NUL
// above all) cannot be stored in a Postgres varchar at all. Passing it through would let a
// client/direct-publisher dead-letter its own events once it reaches the INSERT.
func TestDeserialize_ControlCharacterEventIDIsDropped(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.EventId = "evt-with-a-\x00-nul-byte"

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err, "a control-character event_id must not fail the whole event")
	assert.Empty(t, got.EventID, "a control-character event_id must be dropped to \"\"")
}

// TestDeserialize_EventIDWithinBoundsIsKeptVerbatim is the negative control for the two drop tests
// above: a 64-rune id (exactly at the bound, not over it) must survive unchanged.
func TestDeserialize_EventIDWithinBoundsIsKeptVerbatim(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.EventId = strings.Repeat("b", 64)

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)
	assert.Equal(t, strings.Repeat("b", 64), got.EventID)
}

func TestDeserialize_NilMetadataRemainsNil(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.Metadata = nil

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)
	assert.Nil(t, got.Metadata)
}

func TestDeserialize_TimestampMatchesProtoTime(t *testing.T) {
	timestamp := timestamppb.Now()
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.Timestamp = timestamp

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)
	assert.Equal(t, timestamp.AsTime(), got.Timestamp)
}

func TestDeserialize_MalformedProtoReturnsError(t *testing.T) {
	got, err := event.Deserialize([]byte{0xff, 0xff, 0xff})

	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorContains(t, err, "failed to unmarshal event")
}

func TestDeserialize_InvalidEventReturnsError(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.ProjectKey = ""

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))

	assert.Nil(t, got)
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid event: project_key is required")
}

func TestDeserialize_ComputesFingerprintWhenMissing(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = ""
	protoEvent.FingerprintOverride = false
	protoEvent.Stacktrace = []*sentinelv1.StackFrame{
		{File: "main.go", Line: 42, Function: "main.process", InApp: true},
	}

	got := deserializeAndFingerprint(t, protoEvent)

	// sentinel-project-environments §4.9: ComputeFingerprint passes event.Environment into the
	// config, so the expectation carries it too. Without it this assertion would pass for a call
	// site that silently dropped the environment out of the hash input.
	assert.Equal(t, fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production",
		ErrorClass:  "RuntimeError",
		Stacktrace: []fingerprint.StackFrame{
			{File: "main.go", Line: 42, Function: "main.process", InApp: true},
		},
	}), got.Fingerprint)
}

func TestDeserialize_UsesProvidedFingerprintWithoutOverride(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.FingerprintOverride = false

	got := deserializeAndFingerprint(t, protoEvent)
	assert.Equal(t, "provided-fingerprint", got.Fingerprint)
}

// BUG-20, the semantic overlap between `fccfc7d feat(ingest): let clients send a custom
// fingerprint` and decision 7, which folds `environment` into `fingerprint.Compute`. A custom
// fingerprint bypasses that computation entirely (event.go:177 only computes when the field is
// empty or the override is set), so the question is whether issue identity is still per-environment
// on that path.
//
// It is, and NOT because of the fingerprint: identity is the tuple
// `UNIQUE (project_id, environment, fingerprint)`
// (packages/db-migrations/migrations/1724400000_add_project_environments.sql:67), and both ON
// CONFLICT clauses name all three columns (apps/processor-go/store/store.go:280 and :444). What
// this test pins is the hop those depend on: Deserialize must carry the event's own environment
// through UNCHANGED alongside a client fingerprint it deliberately leaves alone. Blank the
// environment here, or derive it from the fingerprint, and the tuple collapses to
// (project_id, ”, fingerprint) — one issue for two environments — with no constraint violation to
// notice it.
//
// The end-to-end claim, against the real constraint, is
// tests/integration/event_idempotency_test.go:829
// (TestStoreEvent_CustomFingerprintStillSplitsOnEnvironment).
func TestDeserialize_CustomFingerprintLeavesTheEnvironmentIntact(t *testing.T) {
	const shared = "checkout-timeout"

	prodEvent := validProtoEvent()
	prodEvent.Environment = "production"
	prodEvent.Fingerprint = shared

	stagEvent := validProtoEvent()
	stagEvent.Environment = "staging"
	stagEvent.Fingerprint = shared

	prod := deserializeAndFingerprint(t, prodEvent)
	stag := deserializeAndFingerprint(t, stagEvent)

	// The client's value survives in both — §4.9 left that early return alone, so the identical
	// string does arrive from two environments.
	assert.Equal(t, shared, prod.Fingerprint)
	assert.Equal(t, shared, stag.Fingerprint)

	// And the environment, which is what actually separates them, is the event's own in each.
	assert.Equal(t, "production", prod.Environment)
	assert.Equal(t, "staging", stag.Environment)
	assert.NotEqual(t, prod.Environment, stag.Environment,
		"the identity tuple must differ on the environment axis when the fingerprint does not")
}

// The companion for the computed path, which decision 7 DOES reach: two events identical but for
// their environment must not hash to the same fingerprint. Drop `Environment` from the
// FingerprintConfig in event.ComputeFingerprint and this fails, while every other fingerprint
// test here keeps passing — they each assert one environment at a time.
func TestDeserialize_ComputedFingerprintDiffersByEnvironment(t *testing.T) {
	prodEvent := validProtoEvent()
	prodEvent.Environment = "production"
	prodEvent.Fingerprint = ""

	stagEvent := validProtoEvent()
	stagEvent.Environment = "staging"
	stagEvent.Fingerprint = ""

	prod := deserializeAndFingerprint(t, prodEvent)
	stag := deserializeAndFingerprint(t, stagEvent)

	require.NotEmpty(t, prod.Fingerprint)
	assert.NotEqual(t, prod.Fingerprint, stag.Fingerprint,
		"decision 7 folds the environment into the hash input")
}

func TestDeserialize_ComputesFingerprintWhenOverrideIsTrue(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = ""
	protoEvent.FingerprintOverride = true

	got := deserializeAndFingerprint(t, protoEvent)
	assert.Equal(t, fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production",
		ErrorClass:  "RuntimeError",
	}), got.Fingerprint)
}

func TestDeserialize_RecomputesProvidedFingerprintWhenOverrideIsTrue(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.FingerprintOverride = true

	got := deserializeAndFingerprint(t, protoEvent)
	assert.Equal(t, fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production",
		ErrorClass:  "RuntimeError",
	}), got.Fingerprint)
	assert.NotEqual(t, "provided-fingerprint", got.Fingerprint)
}

func TestNormalize_NormalizesThenMasksEventFields(t *testing.T) {
	subject := &event.ErrorEvent{
		Message:    "failed at /home/johndoe/config with api_key: abcdefghijklmnopqrstuv",
		ErrorClass: "Failure 123456789",
		TraceID:    "123e4567-e89b-12d3-a456-426614174000",
		SpanID:     "0xabcdef123456",
		Metadata: map[string]interface{}{
			"path":     "/home/johndoe/project",
			"password": "supersecret",
		},
	}

	subject.Normalize(normalizer.NewNormalizer(), masker.NewMasker())

	assert.Equal(t, "failed at /<USER_PATH>/config with api_key=***REDACTED***", subject.Message)
	assert.Equal(t, "Failure <NUMERIC_ID>", subject.ErrorClass)
	assert.Equal(t, "<UUID>", subject.TraceID)
	assert.Equal(t, "<HEX_ADDR>", subject.SpanID)
	assert.Equal(t, map[string]interface{}{
		"path":     "/<USER_PATH>/project",
		"password": "***REDACTED***",
	}, subject.Metadata)
}

// ---------------------------------------------------------------------------------------------------
// sentinel-affected-users §4.3 — decision 3 (pre-normalisation read) and decision 5 (bounds)
// ---------------------------------------------------------------------------------------------------

// affectedProtoEvent is validProtoEvent with the given metadata attached, for the rollup tests below.
func affectedProtoEvent(t *testing.T, metadata map[string]interface{}) *sentinelv1.ErrorEvent {
	t.Helper()
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	md, err := structpb.NewStruct(metadata)
	require.NoError(t, err)
	protoEvent.Metadata = md
	return protoEvent
}

// TestDeserialize_AffectedIDsSurviveNormalizeByteIdentical is the sentinel-affected-users twin of
// TestDeserialize_EventIDSurvivesNormalizeByteIdentical (contract §11, boundary 3; decisions 3 and 6):
// the three id shapes the normaliser destroys — a UUID ("<UUID>"), a run of >= 6 digits
// ("<NUMERIC_ID>") and an email ("<EMAIL>") — must come out of Deserialize+Normalize on
// AffectedUserID / AffectedTenantID byte-for-byte identical to what went in, while the SAME values
// in the metadata map are rewritten as they always were. This test MUST fail if either field is ever
// added to Normalize's rewrite list; the field comment in event.go names it for exactly that reason.
func TestDeserialize_AffectedIDsSurviveNormalizeByteIdentical(t *testing.T) {
	t.Run("uuid user_id and 7-digit tenant_id", func(t *testing.T) {
		got, err := event.Deserialize(marshalProtoEvent(t, affectedProtoEvent(t, map[string]interface{}{
			"user_id":   "7c9e6679-7425-40de-944b-e07fc1f90ae7",
			"tenant_id": "1234567",
		})))
		require.NoError(t, err)

		assert.Equal(t, "7c9e6679-7425-40de-944b-e07fc1f90ae7", got.AffectedUserID,
			"AffectedUserID must survive Deserialize+Normalize byte-identical — if this now reads \"<UUID>\", "+
				"the field was added to Normalize's rewrite list (decision 3 regression)")
		assert.Equal(t, "1234567", got.AffectedTenantID,
			"AffectedTenantID must survive byte-identical — \"<NUMERIC_ID>\" means it was normalised")
		assert.Empty(t, got.AffectedUserSkipReason)
		assert.Empty(t, got.AffectedTenantSkipReason)

		// The map itself is still normalised — the read happens BEFORE NormalizeMap, it does not
		// exempt the metadata from it (decision 13's security review point iii: the masker and
		// normaliser still run on metadata).
		assert.Equal(t, "<UUID>", got.Metadata["user_id"])
		assert.Equal(t, "<NUMERIC_ID>", got.Metadata["tenant_id"])
	})

	t.Run("email under the user key", func(t *testing.T) {
		got, err := event.Deserialize(marshalProtoEvent(t, affectedProtoEvent(t, map[string]interface{}{
			"user": "alice@example.com",
		})))
		require.NoError(t, err)

		assert.Equal(t, "alice@example.com", got.AffectedUserID,
			"an email id must survive byte-identical — \"<EMAIL>\" means it was normalised or masked")
		assert.Equal(t, "<EMAIL>", got.Metadata["user"])
	})
}

// TestNormalize_ReadsAffectedIDsBeforeNormalizeMap pins the PLACE of the read (contract §4.3: "before
// the MaskMap(NormalizeMap(…)) line, exactly where release_version is read") on a hand-built event,
// without Deserialize in the way: the fields hold the raw values and the map holds the placeholders
// after one Normalize call.
func TestNormalize_ReadsAffectedIDsBeforeNormalizeMap(t *testing.T) {
	subject := &event.ErrorEvent{
		Metadata: map[string]interface{}{
			"userId":   "9f1c2a3b-4d5e-4f60-8a7b-1c2d3e4f5a6b",
			"tenantId": "9876543",
		},
	}

	subject.Normalize(normalizer.NewNormalizer(), masker.NewMasker())

	assert.Equal(t, "9f1c2a3b-4d5e-4f60-8a7b-1c2d3e4f5a6b", subject.AffectedUserID)
	assert.Equal(t, "9876543", subject.AffectedTenantID)
	assert.Equal(t, "<UUID>", subject.Metadata["userId"])
	assert.Equal(t, "<NUMERIC_ID>", subject.Metadata["tenantId"])
}

// TestNormalize_AffectedIDPrecedenceIsTheIndexersVerbatim pins decision 24: the key precedence is
// indexer.ExtractSearchFields's, copied without correction. An empty string at a higher-precedence
// key WINS and is then skipped as `empty`; a non-string value at a higher key falls through to the
// next; no key at all leaves both the id and the reason empty (nothing is counted).
func TestNormalize_AffectedIDPrecedenceIsTheIndexersVerbatim(t *testing.T) {
	t.Run("empty user_id beats a good userId and is counted as empty", func(t *testing.T) {
		subject := &event.ErrorEvent{Metadata: map[string]interface{}{"user_id": "", "userId": "u1"}}
		subject.Normalize(normalizer.NewNormalizer(), masker.NewMasker())
		assert.Empty(t, subject.AffectedUserID, "the indexer's precedence picks user_id even when it is empty")
		assert.Equal(t, obs.AffectedRollupReasonEmpty, subject.AffectedUserSkipReason)
	})

	t.Run("a non-string user_id falls through to userId", func(t *testing.T) {
		subject := &event.ErrorEvent{Metadata: map[string]interface{}{"user_id": 42.0, "userId": "u1"}}
		subject.Normalize(normalizer.NewNormalizer(), masker.NewMasker())
		assert.Equal(t, "u1", subject.AffectedUserID)
		assert.Empty(t, subject.AffectedUserSkipReason)
	})

	t.Run("organization_id is the last tenant key", func(t *testing.T) {
		subject := &event.ErrorEvent{Metadata: map[string]interface{}{"organization_id": "org-7"}}
		subject.Normalize(normalizer.NewNormalizer(), masker.NewMasker())
		assert.Equal(t, "org-7", subject.AffectedTenantID)
	})

	t.Run("no key at all is absent, not empty", func(t *testing.T) {
		subject := &event.ErrorEvent{Metadata: map[string]interface{}{"path": "/tmp"}}
		subject.Normalize(normalizer.NewNormalizer(), masker.NewMasker())
		assert.Empty(t, subject.AffectedUserID)
		assert.Empty(t, subject.AffectedUserSkipReason, "no key present must increment nothing — reason stays \"\"")
		assert.Empty(t, subject.AffectedTenantID)
		assert.Empty(t, subject.AffectedTenantSkipReason)
	})

	t.Run("nil metadata leaves everything empty", func(t *testing.T) {
		subject := &event.ErrorEvent{}
		subject.Normalize(normalizer.NewNormalizer(), masker.NewMasker())
		assert.Empty(t, subject.AffectedUserID)
		assert.Empty(t, subject.AffectedUserSkipReason)
	})
}

// TestBoundAffectedID pins decision 5's bounds function: the three rejections with their reasons
// from the CLOSED set (empty | too_long | control_char), and the pass at exactly the bound. A
// rejected value comes back as "" — never truncated (a truncated id is a different id).
func TestBoundAffectedID(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantID     string
		wantReason string
	}{
		{"empty", "", "", obs.AffectedRollupReasonEmpty},
		{"256 bytes is too long", strings.Repeat("a", 256), "", obs.AffectedRollupReasonTooLong},
		{"300 bytes is too long", strings.Repeat("a", 300), "", obs.AffectedRollupReasonTooLong},
		{"the bound is bytes, not runes", strings.Repeat("é", 128), "", obs.AffectedRollupReasonTooLong}, // 128 runes, 256 bytes
		{"NUL is a control character", "user\x00id", "", obs.AffectedRollupReasonControlChar},
		{"a tab is a control character", "user\tid", "", obs.AffectedRollupReasonControlChar},
		{"DEL is a control character", "user\x7fid", "", obs.AffectedRollupReasonControlChar},
		{"255 bytes passes verbatim", strings.Repeat("b", 255), strings.Repeat("b", 255), ""},
		{"a uuid passes verbatim", "7c9e6679-7425-40de-944b-e07fc1f90ae7", "7c9e6679-7425-40de-944b-e07fc1f90ae7", ""},
		{"an email passes verbatim", "alice@example.com", "alice@example.com", ""},
		{"a space is not a control character", "Alice Smith", "Alice Smith", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, reason := event.BoundAffectedID(tc.in)
			assert.Equal(t, tc.wantID, id)
			assert.Equal(t, tc.wantReason, reason)
		})
	}
	// The reason set is closed at three: whatever a future rejection is, it must map onto one of
	// these or change the contract.
	assert.Equal(t, "empty", obs.AffectedRollupReasonEmpty)
	assert.Equal(t, "too_long", obs.AffectedRollupReasonTooLong)
	assert.Equal(t, "control_char", obs.AffectedRollupReasonControlChar)
	assert.NotEqual(t, obs.EventIDReasonInvalidChars, obs.AffectedRollupReasonControlChar,
		"control_char is a NEW constant, not obs.go's event_id invalid_chars")
}

// TestDeserialize_UnusableAffectedIDsAreDroppedNotFailed is the Deserialize-level view of decision 5:
// an oversized, an empty and a control-character id each leave the event deserialisable, the field
// "" and the reason set — the same shape D-g gives event_id.
func TestDeserialize_UnusableAffectedIDsAreDroppedNotFailed(t *testing.T) {
	got, err := event.Deserialize(marshalProtoEvent(t, affectedProtoEvent(t, map[string]interface{}{
		"user_id":   strings.Repeat("x", 300),
		"tenant_id": "",
	})))
	require.NoError(t, err, "an unusable id must never fail the event")
	assert.Empty(t, got.AffectedUserID)
	assert.Equal(t, obs.AffectedRollupReasonTooLong, got.AffectedUserSkipReason)
	assert.Empty(t, got.AffectedTenantID)
	assert.Equal(t, obs.AffectedRollupReasonEmpty, got.AffectedTenantSkipReason)

	got, err = event.Deserialize(marshalProtoEvent(t, affectedProtoEvent(t, map[string]interface{}{
		"user_id": "usr\x01",
	})))
	require.NoError(t, err)
	assert.Empty(t, got.AffectedUserID)
	assert.Equal(t, obs.AffectedRollupReasonControlChar, got.AffectedUserSkipReason)
}

func TestNormalize_NormalizesBeforeMasking(t *testing.T) {
	subject := &event.ErrorEvent{Message: "api_key: 12345678901234567890"}

	subject.Normalize(normalizer.NewNormalizer(), masker.NewMasker())

	// The numeric value is normalized first. The masker's API-key pattern no
	// longer matches the resulting angle-bracket placeholder.
	assert.Equal(t, "api_key: <NUMERIC_ID>", subject.Message)
}

func TestStructpbToMap_NilReturnsNil(t *testing.T) {
	assert.Nil(t, structpbToMap(nil))
}

func TestStructpbToMap_ConvertsMixedValues(t *testing.T) {
	input := map[string]interface{}{
		"string": "value",
		"number": float64(42),
		"bool":   true,
		"list":   []interface{}{"one", float64(2)},
		"nested": map[string]interface{}{"key": "nested-value"},
	}
	value, err := structpb.NewStruct(input)
	require.NoError(t, err)

	assert.Equal(t, input, structpbToMap(value))
}
