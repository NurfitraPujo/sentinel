package unit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	gonats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// newSampledTracer returns a tracer guaranteed to sample every span, independent of whatever the SDK's
// default sampler happens to be, so these tests never depend on that default.
func newSampledTracer(t *testing.T) trace.Tracer {
	t.Helper()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.AlwaysSample()))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp.Tracer("obs-test")
}

// TestNATSHeaderCarrierRoundTrip proves obs.NATSHeaderCarrier is a real, working
// propagation.TextMapCarrier over nats.Header: a trace context injected into a nats.Header on the
// "producer" side is recoverable, byte-for-byte trace/span id, on the "consumer" side after crossing
// through nothing but the header map itself — exactly the hop packages/shared-go/nats.Publisher /
// Subscriber carry a real NATS message across (OBSERVABILITY_PLAN.md D-e).
func TestNATSHeaderCarrierRoundTrip(t *testing.T) {
	propagator := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	tracer := newSampledTracer(t)

	producerCtx, span := tracer.Start(context.Background(), "producer-span")
	defer span.End()
	wantSC := trace.SpanContextFromContext(producerCtx)
	require.True(t, wantSC.IsValid(), "the started span must have a valid span context")

	// Producer side: inject into a freshly allocated nats.Header, exactly as a real publish would.
	headers := gonats.Header{}
	propagator.Inject(producerCtx, obs.NATSHeaderCarrier(headers))
	require.NotEmpty(t, headers.Get("traceparent"), "Inject must have written a traceparent header")

	// Consumer side: a brand-new background context (as handleMessage hands the subscriber's own ctx,
	// not the producer's), extracting solely from the headers that crossed the wire.
	consumerCtx := propagator.Extract(context.Background(), obs.NATSHeaderCarrier(headers))
	gotSC := trace.SpanContextFromContext(consumerCtx)

	require.True(t, gotSC.IsValid(), "extraction must recover a valid span context")
	assert.Equal(t, wantSC.TraceID(), gotSC.TraceID(), "trace id must round-trip exactly")
	assert.Equal(t, wantSC.SpanID(), gotSC.SpanID(), "span id must round-trip exactly")
}

// TestObsBootstrapRegistersTheGlobalPropagator closes the gap that BOTH independent reviews of W1 and W2
// named as the single highest-value missing guard, and it is worth stating why at length, because the
// failure it protects against is invisible in every other test.
//
// OTel's DEFAULT global propagator is a no-op. If nobody calls otel.SetTextMapPropagator, then
// otel.GetTextMapPropagator().Inject writes NOTHING and Extract returns the context unchanged — with no
// error, no panic, and no log line. The ingestor would still open a producer span, the processor would
// still open a consumer span, /metrics would still serve, and every existing assertion would still pass.
// The only observable difference is that the two services' spans land in two disconnected traces, which
// is precisely the one thing the whole plan exists to deliver (D-e).
//
// TestNATSHeaderCarrierRoundTrip above does NOT cover this: it builds its own local propagator
// (line 37) and never touches the global, so deleting the registration in obs.Bootstrap leaves it green.
//
// So this test deliberately installs a no-op propagator FIRST, proves it is a no-op, and only then calls
// Bootstrap — otherwise it would pass on residue from whichever test happened to run before it, since
// the propagator is process-global and this package is one flat package (BUGS.md B4).
func TestObsBootstrapRegistersTheGlobalPropagator(t *testing.T) {
	// OTEL_SDK_DISABLED keeps this hermetic (no exporter, no network). The propagator is registered
	// before that short-circuit precisely so a service can still parse an inbound traceparent for
	// logging with the SDK off — so this path must register it too.
	t.Setenv("OTEL_SDK_DISABLED", "true")

	original := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(original) })

	// A composite over nothing is a genuine no-op: empty Fields, Inject writes nothing.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	require.Empty(t, otel.GetTextMapPropagator().Fields(),
		"precondition: the global propagator must be a no-op before Bootstrap, or this test proves nothing")

	_, err := obs.Bootstrap(context.Background(), slog.New(slog.NewJSONHandler(io.Discard, nil)),
		obs.ProvidersConfig{ServiceName: "unit-test-service"})
	require.NoError(t, err)

	assert.Contains(t, otel.GetTextMapPropagator().Fields(), "traceparent",
		"obs.Bootstrap must register a W3C TraceContext propagator globally; without it every "+
			"Inject/Extract in the ingestor and processor silently becomes a no-op and the two services "+
			"produce disconnected traces (OBSERVABILITY_PLAN.md D-e)")

	// Prove it end-to-end through the GLOBAL propagator over a real nats.Header, the way the services do
	// it: the ingestor injects on publish, the processor extracts on receive. Asserting IsRemote is what
	// distinguishes "the consumer span is a CHILD of the producer" from "the consumer started a new root".
	tracer := newSampledTracer(t)
	producerCtx, span := tracer.Start(context.Background(), "producer-span")
	defer span.End()
	wantSC := trace.SpanContextFromContext(producerCtx)

	headers := gonats.Header{}
	otel.GetTextMapPropagator().Inject(producerCtx, obs.NATSHeaderCarrier(headers))
	require.NotEmpty(t, headers.Get("traceparent"),
		"the globally registered propagator must write a traceparent on Inject")

	gotSC := trace.SpanContextFromContext(
		otel.GetTextMapPropagator().Extract(context.Background(), obs.NATSHeaderCarrier(headers)))
	require.True(t, gotSC.IsValid())
	assert.Equal(t, wantSC.TraceID(), gotSC.TraceID(), "trace id must survive the hop")
	assert.Equal(t, wantSC.SpanID(), gotSC.SpanID(), "the extracted span must be the producer's, i.e. the parent")
	assert.True(t, gotSC.IsRemote(),
		"the extracted span context must be marked remote — that is what makes the consumer span a child "+
			"of the ingestor's producer span rather than a new root")
}

// TestObsBootstrapDropsUnboundedMetricAttributes locks in the View added after review found that the
// ingestor's /metrics could be grown without bound by an unauthenticated caller.
//
// otelhttp records http.server.* metrics carrying server.address, taken verbatim from the client-supplied
// Host header, and it wraps the OUTERMOST layer of the handler chain — so it records before the
// authenticator runs, on the one port this system exposes publicly (docker-compose.yml publishes 8080).
// Each distinct value mints a new time series across several histograms inside the process itself, which
// is memory growth on the ingest path, not merely an untidy dashboard.
//
// This asserts on the rendered /metrics output rather than on the View object, because the View is only
// worth anything if it actually reaches the reader the handler serves from.
func TestObsBootstrapDropsUnboundedMetricAttributes(t *testing.T) {
	// Deliberately NOT OTEL_SDK_DISABLED: the no-op path has no reader and no View, so it could not
	// detect a regression here. Bootstrap builds the OTLP exporter lazily and never dials on
	// construction, so this stays hermetic despite the SDK being live.
	t.Setenv("OTEL_SDK_DISABLED", "")

	providers, err := obs.Bootstrap(context.Background(), slog.New(slog.NewJSONHandler(io.Discard, nil)),
		obs.ProvidersConfig{ServiceName: "unit-test-service"})
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = providers.Shutdown(ctx)
	})

	counter, err := providers.MeterProvider.Meter("unit-test").Int64Counter("unit_test_requests_total")
	require.NoError(t, err)

	// One attacker-controlled key that must be filtered, one legitimate low-cardinality key that must
	// survive — a filter that dropped everything would be just as broken as no filter at all.
	counter.Add(context.Background(), 1,
		metric.WithAttributes(
			attribute.String("server.address", "attacker-controlled.example"),
			attribute.String("outcome", "accepted"),
		))

	rec := httptest.NewRecorder()
	providers.MetricsHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	require.Contains(t, body, "unit_test_requests_total",
		"precondition: the instrument must appear in /metrics or this test proves nothing")
	assert.NotContains(t, body, "attacker-controlled.example",
		"server.address is derived from the client-supplied Host header and must never reach a metric "+
			"label — an unauthenticated caller could otherwise grow the registry without bound")
	assert.Contains(t, body, `outcome="accepted"`,
		"the filter must drop only the unbounded keys; low-cardinality labels the services rely on must survive")
}

// TestNATSHeaderCarrierGetIsNilSafe documents and locks in the carrier's nil-map contract for Extract:
// a message published with no headers at all (or by a pre-W0 publisher) must degrade to "found nothing"
// rather than panicking, so extraction always safely falls back to starting a new root trace (D-e).
func TestNATSHeaderCarrierGetIsNilSafe(t *testing.T) {
	var nilHeaders gonats.Header
	carrier := obs.NATSHeaderCarrier(nilHeaders)
	assert.Equal(t, "", carrier.Get("traceparent"))
	assert.Empty(t, carrier.Keys())
}

// TestObsHandlerInjectsTraceAndSpanID proves the key design point of the trace-aware slog.Handler
// (packages/shared-go/obs.Handler): a line logged with a context that carries a recording span gets
// trace_id/span_id attached automatically — call sites never thread the ids by hand, they just log with
// *Context and a context.
func TestObsHandlerInjectsTraceAndSpanID(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(obs.NewHandler(slog.NewJSONHandler(&buf, nil)))

	tracer := newSampledTracer(t)
	ctx, span := tracer.Start(context.Background(), "handled-op")
	defer span.End()
	sc := trace.SpanContextFromContext(ctx)
	require.True(t, sc.IsValid())

	logger.InfoContext(ctx, "hello from a traced context")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, sc.TraceID().String(), decoded[obs.LogKeyTraceID])
	assert.Equal(t, sc.SpanID().String(), decoded[obs.LogKeySpanID])
}

// TestObsHandlerPassesThroughWithoutSpan proves the converse: a context carrying no span logs cleanly,
// with neither trace_id nor span_id present — "no trace in scope right now" is not an error.
func TestObsHandlerPassesThroughWithoutSpan(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(obs.NewHandler(slog.NewJSONHandler(&buf, nil)))

	logger.InfoContext(context.Background(), "no span in this context")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	_, hasTraceID := decoded[obs.LogKeyTraceID]
	_, hasSpanID := decoded[obs.LogKeySpanID]
	assert.False(t, hasTraceID)
	assert.False(t, hasSpanID)
}

// TestObsHandlerCarriesServiceAttrViaWithAttrs proves Handler.WithAttrs (the mechanism Setup uses to
// attach LogKeyService once) composes correctly: an attribute attached via WithAttrs appears on every
// subsequent record alongside the per-call trace/span ids.
func TestObsHandlerCarriesServiceAttrViaWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	base := obs.NewHandler(slog.NewJSONHandler(&buf, nil)).WithAttrs([]slog.Attr{slog.String(obs.LogKeyService, "unit-test-service")})
	logger := slog.New(base)

	logger.InfoContext(context.Background(), "service-tagged line")

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "unit-test-service", decoded[obs.LogKeyService])
}

// TestObsSetupReturnsUsableLogger is a smoke test for Setup: it must never return nil and must not
// panic regardless of LOG_FORMAT/LOG_LEVEL, including an unrecognized value for either.
func TestObsSetupReturnsUsableLogger(t *testing.T) {
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("LOG_LEVEL", "not-a-real-level")

	logger := obs.Setup("unit-test-service")
	require.NotNil(t, logger)
	assert.NotPanics(t, func() {
		logger.InfoContext(context.Background(), "setup smoke test")
	})
}

// TestObsSetupAttachesServiceToEveryLine closes a gap found by mutation testing during W0's review: the
// existing coverage exercised NewHandler(...).WithAttrs(...) directly and only asserted that Setup returned
// a non-nil logger, so DELETING the service attribute from Setup left the entire unit suite green — while
// OBSERVABILITY_PLAN.md D-a states "every line carries service" and W1/W2 migrate ~80 log call sites onto
// exactly that promise.
//
// It asserts through Setup's real output, not through the handler in isolation, because Setup is what every
// service actually calls.
func TestObsSetupAttachesServiceToEveryLine(t *testing.T) {
	t.Setenv("LOG_FORMAT", "json")
	t.Setenv("OTEL_SDK_DISABLED", "true")

	var buf bytes.Buffer
	logger := obs.SetupTo(&buf, "unit-test-service")
	if logger == nil {
		t.Fatal("SetupTo returned nil")
	}

	logger.Info("first")
	logger.Warn("second")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %q", len(lines), buf.String())
	}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %d is not JSON: %v\n  %s", i, err, line)
		}
		got, ok := rec[obs.LogKeyService]
		if !ok {
			t.Errorf("line %d has no %q attribute — D-a requires it on EVERY line, and ~80 migrated call "+
				"sites depend on it: %s", i, obs.LogKeyService, line)
			continue
		}
		if got != "unit-test-service" {
			t.Errorf("line %d has %s=%v, want %q", i, obs.LogKeyService, got, "unit-test-service")
		}
	}
}

// TestObsBootstrapIsUsableWhenSDKDisabled covers the other gap W0's review named: obs.Bootstrap had no test
// at all. A nil MeterProvider or TracerProvider here panics at a call site rather than degrading, which is
// the opposite of the degradation mandate — and OTEL_SDK_DISABLED is the path most likely to be exercised
// by someone debugging something else.
func TestObsBootstrapIsUsableWhenSDKDisabled(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")

	providers, err := obs.Bootstrap(context.Background(), slog.New(slog.NewJSONHandler(io.Discard, nil)),
		obs.ProvidersConfig{ServiceName: "unit-test-service"})
	if err != nil {
		t.Fatalf("Bootstrap with the SDK disabled must still succeed: %v", err)
	}
	if providers.TracerProvider == nil || providers.MeterProvider == nil {
		t.Fatal("Bootstrap returned a nil provider — a call site would panic instead of degrading")
	}
	if providers.MetricsHandler == nil {
		t.Fatal("Bootstrap returned a nil MetricsHandler — /metrics would panic")
	}
	if providers.Shutdown == nil {
		t.Fatal("Bootstrap returned a nil Shutdown — SIGTERM wiring would panic")
	}

	// A counter must be creatable and usable; this is what a migrated call site does.
	meter := providers.MeterProvider.Meter("unit-test")
	counter, err := meter.Int64Counter(obs.MetricIngestRequests)
	if err != nil {
		t.Fatalf("creating a counter with the SDK disabled: %v", err)
	}
	counter.Add(context.Background(), 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := providers.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown with the SDK disabled returned %v, want nil", err)
	}
}
