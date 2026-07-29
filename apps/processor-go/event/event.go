package event

import (
	"fmt"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/normalizer"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/golang/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

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
}

type StackFrame struct {
	File     string `json:"file"`
	Line     int32  `json:"line"`
	Function string `json:"function"`
	InApp    bool   `json:"in_app"`
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
		e.Metadata = masker.MaskMap(normalizer.NormalizeMap(e.Metadata))
	}
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
			ErrorClass: event.ErrorClass,
			Stacktrace: frames,
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
