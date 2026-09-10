package sentinel

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestContextHelpers(t *testing.T) {
	ctx := context.Background()
	ctx = WithUser(ctx, "usr_100")
	ctx = WithTenant(ctx, "org_acme")
	ctx = WithTag(ctx, "request_id", "req_999")

	tags := getTagsMap(ctx)
	if tags["user_id"] != "usr_100" {
		t.Errorf("expected user_id usr_100, got %v", tags["user_id"])
	}
	if tags["tenant_id"] != "org_acme" {
		t.Errorf("expected tenant_id org_acme, got %v", tags["tenant_id"])
	}
	if tags["request_id"] != "req_999" {
		t.Errorf("expected request_id req_999, got %v", tags["request_id"])
	}
}

// The server accepts, maps and stores trace_flags end to end
// (ingestor validation -> mapping -> processor), but ExtractTraceIDs read only the
// ids off the SpanContext and dropped the flags that sit on the same object — so
// every SDK-originated event arrived with its sampled bit lost.
func TestExtractTraceContextCarriesSampledFlag(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")

	sampled := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    traceID,
		SpanID:     spanID,
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithSpanContext(context.Background(), sampled)

	gotTrace, gotSpan, gotFlags := ExtractTraceContext(ctx)
	if gotTrace != traceID.String() || gotSpan != spanID.String() {
		t.Fatalf("ids not carried: trace=%q span=%q", gotTrace, gotSpan)
	}
	if gotFlags != uint32(trace.FlagsSampled) {
		t.Fatalf("expected sampled flag %d, got %d", trace.FlagsSampled, gotFlags)
	}

	// An unsampled span must report 0 rather than being indistinguishable from
	// sampled — that distinction is the whole point of sending the field.
	unsampled := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID})
	_, _, gotFlags = ExtractTraceContext(trace.ContextWithSpanContext(context.Background(), unsampled))
	if gotFlags != 0 {
		t.Fatalf("expected 0 flags for an unsampled span, got %d", gotFlags)
	}

	// No span context at all: no ids, no flags, and no panic.
	if tr, sp, fl := ExtractTraceContext(context.Background()); tr != "" || sp != "" || fl != 0 {
		t.Fatalf("expected empty for a context with no span, got %q %q %d", tr, sp, fl)
	}
	if tr, sp, fl := ExtractTraceContext(nil); tr != "" || sp != "" || fl != 0 {
		t.Fatalf("expected empty for a nil context, got %q %q %d", tr, sp, fl)
	}
}

// ExtractTraceIDs is exported and callers outside this repo may depend on its
// two-value signature; it must keep behaving as it did.
func TestExtractTraceIDsUnchanged(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	gotTrace, gotSpan := ExtractTraceIDs(ctx)
	if gotTrace != traceID.String() || gotSpan != spanID.String() {
		t.Fatalf("ExtractTraceIDs changed: trace=%q span=%q", gotTrace, gotSpan)
	}
}
