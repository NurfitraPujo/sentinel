package obs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	promclient "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	nooptrace "go.opentelemetry.io/otel/trace/noop"
)

// defaultOTLPTimeout bounds a single export attempt (and, via sdktrace.WithExportTimeout, a single
// batch flush) when ProvidersConfig.OTLPTimeout is unset. Short deliberately: an unreachable collector
// must never make ingest/process latency depend on it (docs/plans/OBSERVABILITY_PLAN.md D-b
// degradation mandate, Risks: "Exporter backpressure").
const defaultOTLPTimeout = 5 * time.Second

// ProvidersConfig controls Bootstrap.
type ProvidersConfig struct {
	// ServiceName populates the resource's service.name and is the default instrumentation scope name
	// callers should use for otel.Tracer(ServiceName) / otel.Meter(ServiceName). Required.
	ServiceName string
	// ServiceVersion populates the resource's service.version. Optional.
	ServiceVersion string
	// OTLPTimeout bounds a single trace export attempt. Defaults to defaultOTLPTimeout when <= 0.
	OTLPTimeout time.Duration
}

// Providers bundles what a service needs to instrument itself and expose metrics. Bootstrap also
// registers TracerProvider/MeterProvider as the process-wide otel globals (otel.SetTracerProvider /
// otel.SetMeterProvider), so instrumentation elsewhere (otelhttp middleware, a manual span, a meter
// instrument) can use otel.Tracer(...)/otel.Meter(...) directly without this value being threaded
// through every call site. The fields here exist for callers that prefer not to rely on globals (and
// for tests).
type Providers struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	// MetricsHandler serves the Prometheus text-exposition format for this process's meter provider.
	// Mount it at /metrics (ingestor :8080/metrics, processor :8081/metrics per
	// OBSERVABILITY_PLAN.md §2).
	MetricsHandler http.Handler
	// Shutdown flushes and closes both providers. Callers MUST wire this to SIGTERM with a bounded
	// context — without it, a process's last spans (exactly the interesting ones during an incident)
	// are dropped instead of flushed (plan finding 6a). Safe to call exactly once; the underlying SDK
	// providers do not guarantee a second call is safe.
	Shutdown func(context.Context) error
}

// Bootstrap wires OpenTelemetry traces (OTLP/HTTP exporter, W3C TraceContext+Baggage propagation,
// parentbased_always_on sampling by default) and metrics (OTel meter API over a Prometheus exporter)
// for one service.
//
// Degradation is the load-bearing property here, not an afterthought (D-b): the OTLP HTTP exporter
// dials lazily — Bootstrap never blocks on, or fails because of, an unreachable collector. Export
// failures surface asynchronously through the SDK's error-reporting path, which this function redirects
// to a handler that logs exactly ONE warning per process (see newQuietErrorHandler) rather than either
// crashing or flooding the log with one line per failed batch. The batch span processor's queue drops
// spans on overflow rather than blocking the caller, so a dead collector can degrade trace completeness
// but can never add latency to the request/message path it is instrumenting.
//
// OTEL_SDK_DISABLED=true (the standard OTel env var) short-circuits to a real, working no-op setup: a
// no-op TracerProvider/MeterProvider and a /metrics handler that just returns 200 with an empty body.
// This is a supported, first-class path, not a degraded one — some deployments want telemetry off
// entirely.
//
// logger may be nil (Bootstrap falls back to slog.Default()); pass the *slog.Logger from Setup so the
// degradation warning above carries the same service/JSON-vs-text formatting as the rest of the
// process's logs.
func Bootstrap(ctx context.Context, logger *slog.Logger, cfg ProvidersConfig) (*Providers, error) {
	if cfg.ServiceName == "" {
		return nil, fmt.Errorf("obs: ProvidersConfig.ServiceName is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	// Propagator is set regardless of OTEL_SDK_DISABLED: even with tracing/metrics off, a service must
	// still be ABLE to parse an inbound traceparent for logging purposes (D-d/D-e), and setting this
	// twice (once here, once again below in the enabled path) is harmless — otel.SetTextMapPropagator
	// simply overwrites the global.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	if sdkDisabled() {
		logger.Info("obs: OTEL_SDK_DISABLED=true; tracing and metrics are no-ops for this process",
			slog.String(LogKeyEvent, "obs.sdk_disabled"))
		return noopProviders(), nil
	}

	otel.SetErrorHandler(newQuietErrorHandler(logger))

	timeout := cfg.OTLPTimeout
	if timeout <= 0 {
		timeout = defaultOTLPTimeout
	}

	res := buildResource(ctx, logger, cfg)

	traceExporter, err := otlptracehttp.New(ctx, otlptracehttp.WithTimeout(timeout))
	if err != nil {
		// otlptracehttp.New only fails on a malformed local configuration (e.g. bad TLS material) — it
		// never fails because the collector is unreachable; that failure mode is async, per-export,
		// and handled by the quiet error handler set above. A configuration error here is something an
		// operator must fix, so it is returned rather than swallowed; whether that is fatal is main()'s
		// call, not this package's (see the plan's degradation mandate — main() should still prefer
		// "start anyway" here too, but this package cannot make that call for every caller).
		return nil, fmt.Errorf("obs: failed to build OTLP trace exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(samplerFromEnv()),
		// WithBatcher's default queueing is non-blocking (drops on a full queue rather than blocking
		// the span-ending goroutine) — deliberately not overridden with WithBlocking(), so a dead
		// collector can degrade trace completeness but never ingest/process latency.
		sdktrace.WithBatcher(traceExporter, sdktrace.WithExportTimeout(timeout)),
	)
	otel.SetTracerProvider(tp)

	registry := promclient.NewRegistry()
	promExporter, err := prometheus.New(prometheus.WithRegisterer(registry))
	if err != nil {
		return nil, fmt.Errorf("obs: failed to build Prometheus metric exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promExporter),
		sdkmetric.WithView(dropUnboundedAttributes()),
	)
	otel.SetMeterProvider(mp)

	shutdown := func(shutdownCtx context.Context) error {
		var errs []error
		if err := tp.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("tracer provider shutdown: %w", err))
		}
		if err := mp.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("meter provider shutdown: %w", err))
		}
		return errors.Join(errs...)
	}

	return &Providers{
		TracerProvider: tp,
		MeterProvider:  mp,
		MetricsHandler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		Shutdown:       shutdown,
	}, nil
}

// unboundedMetricAttributes are attribute keys that must never reach a metric time series, because
// their values are derived from the REQUEST rather than from the code — and the ingestor's /ingest is
// the one externally exposed endpoint in this system (docker-compose.yml publishes 8080).
//
// The instruments this package's callers define are already low-cardinality by construction
// (obs.go documents why project_id is deliberately absent). The hazard is the instrumentation we
// mount but do not author: otelhttp records http.server.* metrics carrying server.address, taken
// straight from the client-supplied Host header, and client.address, taken from RemoteAddr /
// X-Forwarded-For. Neither is authenticated — otelhttp wraps the OUTERMOST layer of the chain, so it
// records before the authenticator ever runs. An unauthenticated caller varying one header therefore
// mints a new time series per distinct value, across three histograms of ~17 buckets each, inside the
// ingestor process itself. That is unbounded memory growth on the ingest path reachable by anyone who
// can reach the port, not merely an untidy dashboard.
//
// Filtering here rather than at each otelhttp.NewHandler call site is deliberate: otelhttp offers no
// option to REMOVE a built-in semconv attribute (WithMetricAttributesFn only adds), and a View is the
// SDK-native mechanism for exactly this. Applying it in Bootstrap means every current and future
// instrumentation in every service inherits the protection instead of each call site remembering.
// Spans are unaffected — a span attribute costs nothing per distinct value, and server.address is
// genuinely useful there.
var unboundedMetricAttributes = []attribute.Key{
	semconv.ServerAddressKey,
	semconv.ServerPortKey,
	semconv.ClientAddressKey,
	semconv.ClientPortKey,
	semconv.NetworkPeerAddressKey,
	semconv.NetworkPeerPortKey,
	semconv.URLPathKey,
	semconv.URLFullKey,
	semconv.UserAgentOriginalKey,
}

// dropUnboundedAttributes returns a View matching every instrument that strips the keys above. The
// "*" instrument name matches all instruments, so this applies to instrumentation added later without
// anyone having to remember to opt in.
func dropUnboundedAttributes() sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: "*"},
		sdkmetric.Stream{AttributeFilter: attribute.NewDenyKeysFilter(unboundedMetricAttributes...)},
	)
}

// buildResource resolves the OTel resource (service.name/version plus environment detection).
// Detector failures (an environment without the expected metadata) degrade to the minimal resource
// built from cfg alone rather than failing Bootstrap — a service must be able to start regardless of
// what the host looks like.
func buildResource(ctx context.Context, logger *slog.Logger, cfg ProvidersConfig) *resource.Resource {
	// Order matters, and the obvious order is wrong. resource.Merge lets the LATER resource win on a
	// shared key — including when its value is empty — so putting WithFromEnv() after WithAttributes lets
	// OTEL_SERVICE_NAME silently override the caller's ServiceName. That is a two-names-for-one-thing
	// hazard (BUGS.md B5): a compose typo would rename service.name on spans while the instrumentation
	// scope name and the slog `service` attribute kept the code value, so a cross-service trace assertion
	// would fail with nothing pointing at the cause. The caller's value is authoritative; env still
	// supplies everything else (OTEL_RESOURCE_ATTRIBUTES and friends).
	// WithProcess() and WithHost() are deliberately NOT used. The Prometheus exporter publishes every
	// resource attribute as labels on `target_info`, and /metrics is mounted outside auth and rate
	// limiting on the ingestor's public :8080 — so those detectors would hand any unauthenticated
	// caller the hostname, the OS user, the pid, the full argv (which in other deployments carries
	// flags and occasionally credentials) and the exact Go toolchain version. That is a free
	// fingerprint for CVE targeting, and it buys nothing here: in compose and in any container
	// deployment the "host" is a disposable container and the pid is always 1. Anything genuinely
	// wanted can still be supplied deliberately via OTEL_RESOURCE_ATTRIBUTES, which WithFromEnv reads.
	//
	// This repo has leaked a credential through a log line once already (the DSN password); the
	// cheapest way not to repeat it is to not collect what we do not need.
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithAttributes(serviceAttributes(cfg)...),
	)
	if err == nil {
		return res
	}

	logger.Warn("obs: resource detection degraded; continuing with service name/version only",
		slog.String("error", err.Error()), slog.String(LogKeyEvent, "obs.resource_degraded"))
	minimal, mergeErr := resource.Merge(resource.Default(), resource.NewSchemaless(serviceAttributes(cfg)...))
	if mergeErr != nil {
		// resource.Merge only fails on a schema URL conflict, which cannot happen between
		// resource.Default() and a schemaless resource. Fall back one more level rather than ever
		// returning nil.
		return resource.NewSchemaless(serviceAttributes(cfg)...)
	}
	return minimal
}

// serviceAttributes builds the identifying resource attributes, omitting service.version when the
// caller did not set one.
//
// Emitting it unconditionally ships a literal `service_version=""` on every series' target_info and on
// every span's resource. An empty-valued attribute is worse than an absent one: it is indistinguishable
// from "the version is genuinely the empty string", and it invites a query like
// `group by (service_version)` to produce a phantom bucket. Absent means absent.
func serviceAttributes(cfg ProvidersConfig) []attribute.KeyValue {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName)}
	if cfg.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.ServiceVersion))
	}
	return attrs
}

// sdkDisabled reports whether OTEL_SDK_DISABLED is set to a truthy value, per the standard OTel env
// var (https://opentelemetry.io/docs/languages/sdk-configuration/general/#otel_sdk_disabled).
func sdkDisabled() bool {
	b, err := strconv.ParseBool(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")))
	return err == nil && b
}

// noopProviders builds a Providers whose Tracer/Meter calls are real, working no-ops (never nil, never
// panicking) and whose /metrics handler returns 200 with an empty body. Used for OTEL_SDK_DISABLED=true.
func noopProviders() *Providers {
	return &Providers{
		TracerProvider: nooptrace.NewTracerProvider(),
		MeterProvider:  noopmetric.NewMeterProvider(),
		MetricsHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
		Shutdown: func(context.Context) error { return nil },
	}
}

// samplerFromEnv reads the standard OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG env vars
// (https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/#general-sdk-configuration)
// and returns the matching sampler, defaulting to parentbased_always_on (D-b) for an unset or
// unrecognized value — dev/CI traffic is tiny; real sampling policy is a production concern with these
// standard knobs already in place.
func samplerFromEnv() sdktrace.Sampler {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER"))) {
	case "always_on":
		return sdktrace.AlwaysSample()
	case "always_off":
		return sdktrace.NeverSample()
	case "parentbased_always_off":
		return sdktrace.ParentBased(sdktrace.NeverSample())
	case "traceidratio":
		return sdktrace.TraceIDRatioBased(samplerRatioFromEnv())
	case "parentbased_traceidratio":
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(samplerRatioFromEnv()))
	case "", "parentbased_always_on":
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	default:
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
}

// samplerRatioFromEnv reads OTEL_TRACES_SAMPLER_ARG for the *traceidratio samplers, defaulting to 1.0
// (sample everything) for an unset, unparseable, or out-of-[0,1]-range value.
func samplerRatioFromEnv() float64 {
	arg := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG"))
	if arg == "" {
		return 1.0
	}
	ratio, err := strconv.ParseFloat(arg, 64)
	if err != nil || ratio < 0 || ratio > 1 {
		return 1.0
	}
	return ratio
}

// newQuietErrorHandler returns an otel.ErrorHandler that logs the FIRST export error this process
// observes as one slog warning and silently discards every subsequent one. This is the mechanism behind
// D-b's "collector absent -> one warning line, service fully functional": without it, the OTel SDK's
// default error handler logs to stderr on every failed export (a batch span processor exports every few
// seconds by default), which would flood the log pipeline exactly the way S17 already proved a tight
// error loop can (packages/shared-go/nats/subscriber.go's dropLogInterval/shouldLogDrop) — in a codebase
// whose whole failure history is "nobody could see it," losing signal to a flood is worse than losing
// one repeated warning.
func newQuietErrorHandler(logger *slog.Logger) otel.ErrorHandler {
	h := &quietErrorHandler{logger: logger}
	return otel.ErrorHandlerFunc(h.handle)
}

// errorLogInterval rate-limits the handler. It is deliberately NOT a sync.Once.
//
// otel.SetErrorHandler installs ONE handler for every error the SDK reports process-wide — not just trace
// export. exporters/prometheus routes collection failures and duplicate/conflicting instrument
// registrations through the same channel. A sync.Once here meant the first OTLP failure — guaranteed on any
// machine without a collector running, i.e. every development machine — permanently silenced metric errors
// too. In a codebase whose entire failure history is "nobody could see it", a switch that turns off future
// diagnostics after one unrelated event is the wrong trade. Rate-limiting keeps a flood off the log
// pipeline (the S17 concern) while leaving later, different errors visible.
const errorLogInterval = time.Minute

type quietErrorHandler struct {
	mu       sync.Mutex
	lastLog  time.Time
	suppress int
	logger   *slog.Logger
}

func (h *quietErrorHandler) handle(err error) {
	h.mu.Lock()
	now := time.Now()
	first := h.lastLog.IsZero()
	if !first && now.Sub(h.lastLog) < errorLogInterval {
		h.suppress++
		h.mu.Unlock()
		return
	}
	suppressed := h.suppress
	h.suppress = 0
	h.lastLog = now
	h.mu.Unlock()

	attrs := []any{
		slog.String("error", err.Error()),
		slog.String(LogKeyEvent, "obs.export_degraded"),
	}
	if suppressed > 0 {
		attrs = append(attrs, slog.Int("suppressed_since_last", suppressed))
	}
	h.logger.Warn("obs: OpenTelemetry reported an error (the collector may be unreachable, or an "+
		"instrument failed to collect); the service remains fully functional — export failures never "+
		"block or fail the request/message path. Further identical errors are rate-limited to one per "+
		errorLogInterval.String()+".", attrs...)
}
