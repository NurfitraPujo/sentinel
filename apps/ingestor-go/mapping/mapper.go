package mapping

import (
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func MapPayloadToEvent(payload *validation.ErrorPayload) *sentinelv1.ErrorEvent {
	stacktrace := mapFrames(payload.Stacktrace)

	var metadata *structpb.Struct
	if payload.Metadata != nil {
		metadata, _ = structpb.NewStruct(payload.Metadata)
	}

	event := &sentinelv1.ErrorEvent{
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
		// sentinel-event-schema-v2 (spec §4.1): source (field 14) and fields 18-29, copied verbatim.
		// ValidatePayload has already bounded every one of them in place.
		Source:      payload.Source,
		Level:       payload.Level,
		Logger:      payload.Logger,
		ServerName:  payload.ServerName,
		Transaction: payload.Transaction,
		Dist:        payload.Dist,
		Tags:        payload.Tags,
		Sdk:         mapSDK(payload.SDK),
		User:        mapUser(payload.User),
		Contexts:    mapContexts(payload.Contexts),
		Request:     mapRequest(payload.Request),
		Breadcrumbs: mapBreadcrumbs(payload.Breadcrumbs),
		Exception:   mapException(payload.Exception),
	}
	// Field 30 is server-stamped (IngestAt sets ReceivedAt to T_recv); a zero value means no stamp.
	if !payload.ReceivedAt.IsZero() {
		event.ReceivedAt = timestamppb.New(payload.ReceivedAt)
	}
	return event
}

func mapFrames(frames []validation.StackFrame) []*sentinelv1.StackFrame {
	var out []*sentinelv1.StackFrame
	for _, frame := range frames {
		out = append(out, &sentinelv1.StackFrame{
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

func mapSDK(sdk *validation.SDKInfo) *sentinelv1.Sdk {
	if sdk == nil {
		return nil
	}
	return &sentinelv1.Sdk{Name: sdk.Name, Version: sdk.Version}
}

func mapUser(user *validation.UserInfo) *sentinelv1.User {
	if user == nil {
		return nil
	}
	out := &sentinelv1.User{Id: user.ID, Username: user.Username, Email: user.Email, IpAddress: user.IPAddress}
	if user.Geo != nil {
		out.Geo = &sentinelv1.Geo{City: user.Geo.City, Region: user.Geo.Region, CountryCode: user.Geo.CountryCode, Subdivision: user.Geo.Subdivision}
	}
	return out
}

// mapContexts goes through structpb.NewStruct and ignores its error exactly as metadata's is ignored
// above: decoded JSON cannot produce a value the constructor rejects.
func mapContexts(contexts map[string]map[string]interface{}) map[string]*structpb.Struct {
	if len(contexts) == 0 {
		return nil
	}
	out := make(map[string]*structpb.Struct, len(contexts))
	for name, ctx := range contexts {
		st, err := structpb.NewStruct(ctx)
		if err != nil || st == nil {
			st = &structpb.Struct{}
		}
		out[name] = st
	}
	return out
}

func mapRequest(req *validation.RequestInfo) *sentinelv1.Request {
	if req == nil {
		return nil
	}
	out := &sentinelv1.Request{Method: req.Method, Url: req.URL, QueryString: req.QueryString, Headers: req.Headers, Env: req.Env}
	if req.Data != nil {
		out.Data, _ = structpb.NewValue(req.Data)
	}
	return out
}

func mapBreadcrumbs(crumbs []validation.Breadcrumb) []*sentinelv1.Breadcrumb {
	var out []*sentinelv1.Breadcrumb
	for _, c := range crumbs {
		b := &sentinelv1.Breadcrumb{Type: c.Type, Category: c.Category, Level: c.Level, Message: c.Message}
		if !c.Timestamp.IsZero() {
			b.Timestamp = timestamppb.New(c.Timestamp)
		}
		if c.Data != nil {
			b.Data, _ = structpb.NewStruct(c.Data)
		}
		out = append(out, b)
	}
	return out
}

func mapException(exc *validation.ExceptionInterface) *sentinelv1.ExceptionInterface {
	if exc == nil {
		return nil
	}
	out := &sentinelv1.ExceptionInterface{}
	for _, v := range exc.Values {
		ev := &sentinelv1.ExceptionValue{Type: v.Type, Value: v.Value, Stacktrace: mapFrames(v.Stacktrace)}
		if v.Mechanism != nil {
			ev.Mechanism = &sentinelv1.Mechanism{Type: v.Mechanism.Type, Handled: v.Mechanism.Handled}
		}
		out.Values = append(out.Values, ev)
	}
	return out
}
