package unit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

// fullPayload returns an ErrorPayload with every field populated so we can
// assert field-by-field mapping on the resulting proto.
func fullPayload() *validation.ErrorPayload {
	return &validation.ErrorPayload{
		ProjectKey:  "proj-1",
		Platform:    "go",
		Environment: "production",
		Message:     "something went wrong",
		ErrorClass:  "RuntimeError",
		TraceID:     "trace-abc",
		SpanID:      "span-xyz",
		Stacktrace: []validation.StackFrame{
			{File: "main.go", Line: 10, Function: "main", InApp: true},
			{File: "utils.go", Line: 20, Function: "process", InApp: false},
		},
		Metadata: map[string]interface{}{
			"user_id": "u-42",
			"release": "1.2.3",
		},
		Timestamp:  time.Date(2026, 7, 23, 12, 34, 56, 0, time.UTC),
		TraceFlags: 1,
	}
}

func TestMapPayloadToEvent_AllFieldsPopulated(t *testing.T) {
	payload := fullPayload()

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	assert.Equal(t, payload.ProjectKey, got.ProjectKey)
	assert.Equal(t, payload.Platform, got.Platform)
	assert.Equal(t, payload.Environment, got.Environment)
	assert.Equal(t, payload.Message, got.Message)
	assert.Equal(t, payload.ErrorClass, got.ErrorClass)
	assert.Equal(t, payload.TraceID, got.TraceId, "TraceID maps to TraceId")
	assert.Equal(t, payload.SpanID, got.SpanId, "SpanID maps to SpanId")
	assert.Equal(t, payload.TraceFlags, got.TraceFlags)

	require.NotNil(t, got.Timestamp)
	assert.True(t, payload.Timestamp.Equal(got.Timestamp.AsTime()),
		"timestamp AsTime should equal payload.Timestamp, got %v", got.Timestamp.AsTime())

	require.Len(t, got.Stacktrace, len(payload.Stacktrace))
	for i, frame := range got.Stacktrace {
		require.NotNil(t, frame)
		assert.Equal(t, payload.Stacktrace[i].File, frame.File)
		assert.Equal(t, payload.Stacktrace[i].Line, frame.Line)
		assert.Equal(t, payload.Stacktrace[i].Function, frame.Function)
		assert.Equal(t, payload.Stacktrace[i].InApp, frame.InApp)
	}

	require.NotNil(t, got.Metadata)
	require.NotNil(t, got.Metadata.Fields)
	assert.Equal(t, len(payload.Metadata), len(got.Metadata.Fields))
	assert.Equal(t, "u-42", got.Metadata.Fields["user_id"].GetStringValue())
	assert.Equal(t, "1.2.3", got.Metadata.Fields["release"].GetStringValue())
}

func TestMapPayloadToEvent_NilMetadata(t *testing.T) {
	payload := fullPayload()
	payload.Metadata = nil

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	assert.Nil(t, got.Metadata, "nil metadata should map to nil proto metadata")
	// All other fields should still be populated.
	assert.Equal(t, payload.ProjectKey, got.ProjectKey)
	assert.Equal(t, payload.TraceID, got.TraceId)
}

func TestMapPayloadToEvent_EmptyMetadata(t *testing.T) {
	payload := fullPayload()
	payload.Metadata = map[string]interface{}{}

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	require.NotNil(t, got.Metadata, "empty (non-nil) metadata should produce a non-nil struct")
	require.NotNil(t, got.Metadata.Fields)
	assert.Empty(t, got.Metadata.Fields)
}

func TestMapPayloadToEvent_MultipleStacktraceFrames(t *testing.T) {
	payload := fullPayload()
	payload.Stacktrace = []validation.StackFrame{
		{File: "a.go", Line: 1, Function: "alpha", InApp: true},
		{File: "b.go", Line: 2, Function: "beta", InApp: true},
		{File: "c.go", Line: 3, Function: "gamma", InApp: false},
		{File: "d.go", Line: 4, Function: "delta", InApp: false},
	}

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	require.Len(t, got.Stacktrace, 4)

	expected := []struct {
		file, function string
		line           int32
		inApp          bool
	}{
		{"a.go", "alpha", 1, true},
		{"b.go", "beta", 2, true},
		{"c.go", "gamma", 3, false},
		{"d.go", "delta", 4, false},
	}
	for i, want := range expected {
		frame := got.Stacktrace[i]
		require.NotNil(t, frame)
		assert.Equal(t, want.file, frame.File)
		assert.Equal(t, want.line, frame.Line)
		assert.Equal(t, want.function, frame.Function)
		assert.Equal(t, want.inApp, frame.InApp)
	}
}

func TestMapPayloadToEvent_EmptyStacktrace(t *testing.T) {
	payload := fullPayload()
	payload.Stacktrace = []validation.StackFrame{}

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	// The mapper appends into a nil slice, so an empty input yields an empty
	// (possibly nil) output. We accept either nil or empty here.
	assert.Empty(t, got.Stacktrace)
}

func TestMapPayloadToEvent_NilStacktrace(t *testing.T) {
	payload := fullPayload()
	payload.Stacktrace = nil

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	// nil input appends zero entries, so the result should be empty (possibly nil).
	assert.Empty(t, got.Stacktrace)
}

func TestMapPayloadToEvent_TraceIDMapping(t *testing.T) {
	payload := fullPayload()
	payload.TraceID = "the-trace-id"

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	assert.Equal(t, "the-trace-id", got.TraceId,
		"TraceID from payload must be copied into TraceId on the proto")
}

func TestMapPayloadToEvent_SpanIDMapping(t *testing.T) {
	payload := fullPayload()
	payload.SpanID = "the-span-id"

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	assert.Equal(t, "the-span-id", got.SpanId,
		"SpanID from payload must be copied into SpanId on the proto")
}

func TestMapPayloadToEvent_TraceFlagsRoundTrip(t *testing.T) {
	payload := fullPayload()
	payload.TraceFlags = 42

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	assert.Equal(t, uint32(42), got.TraceFlags)
}

func TestMapPayloadToEvent_StacktraceTypeIsProto(t *testing.T) {
	// Compile-time / type-time check that the returned proto uses the
	// generated sentinelv1.StackFrame type rather than the validation type.
	payload := fullPayload()
	got := mapping.MapPayloadToEvent(payload)
	require.NotNil(t, got)
	require.Len(t, got.Stacktrace, len(payload.Stacktrace))
	var _ *sentinelv1.StackFrame = got.Stacktrace[0]
}

func TestMapPayloadToEvent_MetadataPreservesValues(t *testing.T) {
	// Round-trip a metadata value through structpb to ensure the mapping
	// produces equivalent values.
	payload := fullPayload()
	payload.Metadata = map[string]interface{}{
		"is_admin": true,
		"score":    float64(3.14),
	}

	got := mapping.MapPayloadToEvent(payload)

	require.NotNil(t, got)
	require.NotNil(t, got.Metadata)
	require.Contains(t, got.Metadata.Fields, "is_admin")
	require.Contains(t, got.Metadata.Fields, "score")
	assert.Equal(t, true, got.Metadata.Fields["is_admin"].GetBoolValue())
	assert.InDelta(t, 3.14, got.Metadata.Fields["score"].GetNumberValue(), 1e-9)
}
