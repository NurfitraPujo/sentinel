package unit

import (
	"testing"
	_ "unsafe"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/normalizer"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
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
	}

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
	assert.Equal(t, []event.StackFrame{
		{File: "main.go", Line: 42, Function: "main.process", InApp: true},
		{File: "runtime.go", Line: 7, Function: "runtime.goexit", InApp: false},
	}, got.Stacktrace)
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

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)

	assert.Equal(t, fingerprint.Compute(fingerprint.FingerprintConfig{
		ErrorClass: "RuntimeError",
		Stacktrace: []fingerprint.StackFrame{
			{File: "main.go", Line: 42, Function: "main.process", InApp: true},
		},
	}), got.Fingerprint)
}

func TestDeserialize_UsesProvidedFingerprintWithoutOverride(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.FingerprintOverride = false

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)
	assert.Equal(t, "provided-fingerprint", got.Fingerprint)
}

func TestDeserialize_ComputesFingerprintWhenOverrideIsTrue(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = ""
	protoEvent.FingerprintOverride = true

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)
	assert.Equal(t, fingerprint.Compute(fingerprint.FingerprintConfig{
		ErrorClass: "RuntimeError",
	}), got.Fingerprint)
}

func TestDeserialize_RecomputesProvidedFingerprintWhenOverrideIsTrue(t *testing.T) {
	protoEvent := validProtoEvent()
	protoEvent.Fingerprint = "provided-fingerprint"
	protoEvent.FingerprintOverride = true

	got, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)
	assert.Equal(t, fingerprint.Compute(fingerprint.FingerprintConfig{
		ErrorClass: "RuntimeError",
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
