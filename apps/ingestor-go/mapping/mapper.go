package mapping

import (
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func MapPayloadToEvent(payload *validation.ErrorPayload) *sentinelv1.ErrorEvent {
	var stacktrace []*sentinelv1.StackFrame
	for _, frame := range payload.Stacktrace {
		stacktrace = append(stacktrace, &sentinelv1.StackFrame{
			File:     frame.File,
			Line:     frame.Line,
			Function: frame.Function,
			InApp:    frame.InApp,
			Column:   frame.Column,
		})
	}

	var metadata *structpb.Struct
	if payload.Metadata != nil {
		metadata, _ = structpb.NewStruct(payload.Metadata)
	}

	return &sentinelv1.ErrorEvent{
		// EventId is copied verbatim. By the time this runs, service.IngestService.Ingest has already
		// resolved payload.EventID to its EFFECTIVE value - a usable client id kept as-is, or a freshly
		// minted UUIDv4 when the client's was absent/oversized (docs/plans/IDEMPOTENCY_PLAN.md D-a).
		// This mapper does no minting/validation of its own; it only copies whatever is already there.
		EventId:        payload.EventID,
		ProjectKey:     payload.ProjectKey,
		Platform:       payload.Platform,
		Environment:    payload.Environment,
		Message:        payload.Message,
		ErrorClass:     payload.ErrorClass,
		TraceId:        payload.TraceID,
		SpanId:         payload.SpanID,
		Stacktrace:     stacktrace,
		Metadata:       metadata,
		Timestamp:      timestamppb.New(payload.Timestamp),
		TraceFlags:     payload.TraceFlags,
		ReleaseVersion: payload.ReleaseVersion,
		// Custom grouping. Both fields existed in the proto and were honoured by
		// the processor (event.go:177), but nothing decoded them from JSON — so
		// the capability was reachable only by a hypothetical proto client, and
		// the only ingest path anyone has is JSON.
		Fingerprint:         payload.Fingerprint,
		FingerprintOverride: payload.FingerprintOverride,
		// TODO(P3-1): payload.ProjectID is client-supplied at this point and
		// MUST NOT be trusted as-is — it has to be replaced with the project
		// resolved by auth.APIKeyAuthenticator.Middleware from the
		// authenticated API key (see auth/apikey.go:83-86 and the TODO at the
		// handleIngest/handleBatchIngest call sites in main.go). Passing the
		// body value through here for now only wires the plumbing; it is not
		// yet tenant-safe.
		ProjectId: payload.ProjectID,
	}
}
