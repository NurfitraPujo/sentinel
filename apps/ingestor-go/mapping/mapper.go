package mapping

import (
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

func MapPayloadToEvent(payload *validation.ErrorPayload) *sentinelv1.ErrorEvent {
	var stacktrace []*sentinelv1.StackFrame
	for _, frame := range payload.Stacktrace {
		stacktrace = append(stacktrace, &sentinelv1.StackFrame{
			File:     frame.File,
			Line:     frame.Line,
			Function: frame.Function,
			InApp:    frame.InApp,
		})
	}

	var metadata *structpb.Struct
	if payload.Metadata != nil {
		metadata, _ = structpb.NewStruct(payload.Metadata)
	}

	return &sentinelv1.ErrorEvent{
		ProjectKey:  payload.ProjectKey,
		Platform:    payload.Platform,
		Environment: payload.Environment,
		Message:     payload.Message,
		ErrorClass:  payload.ErrorClass,
		TraceId:     payload.TraceID,
		SpanId:      payload.SpanID,
		Stacktrace:  stacktrace,
		Metadata:    metadata,
		Timestamp:   timestamppb.New(payload.Timestamp),
		TraceFlags:  payload.TraceFlags,
	}
}
