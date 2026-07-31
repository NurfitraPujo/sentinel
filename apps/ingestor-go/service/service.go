package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"google.golang.org/protobuf/proto"
)

// maxEventIDLength matches error_occurrences.event_id VARCHAR(64) / the proto's "error_event.event_id"
// CEL rule (packages/proto/sentinel/v1/error_event.proto) - see docs/plans/IDEMPOTENCY_PLAN.md D-a/D-b.
const maxEventIDLength = 64

// tracerName is the OTel instrumentation scope name for spans and instruments this package creates. It
// matches the ingestor's obs.Setup/obs.Bootstrap ServiceName ("ingestor-go") so a trace's spans and the
// resource's service.name agree — see docs/plans/OBSERVABILITY_PLAN.md D-b/W1.
const tracerName = "ingestor-go"

// natsSubject mirrors main.go's nats.PublisherConfig.Subject. It exists here purely for span naming/
// attribution (observability), not routing — the actual subject a message is published to is whatever
// the injected *nats.Publisher was constructed with.
const natsSubject = "error_events"

// eventPublisher is the narrow slice of *nats.Publisher's API IngestService actually calls. Defined
// here rather than in packages/shared-go so tests/unit can substitute a fake that captures published
// bytes without a real NATS connection - *nats.Publisher already satisfies this interface structurally,
// so no production call site (main.go, tests/integration) changes shape.
type eventPublisher interface {
	PublishWithHeaders(ctx context.Context, data []byte, headers nats.Header) error
}

type IngestService struct {
	publisher eventPublisher
	validator protovalidate.Validator

	// publishFailureCounter records obs.MetricIngestPublishFailures. May be a degraded (nil-safe, per
	// the OTel metric API's own contract) instrument if creation failed — Add is still guarded with a
	// nil check below since "may fail" here means "may be less than fully functional," never "must
	// crash the ingestor" (docs/plans/OBSERVABILITY_PLAN.md D-b degradation mandate).
	publishFailureCounter metric.Int64Counter

	// eventIDReplacedCounter records obs.MetricIngestEventIDReplaced{reason} (docs/plans/
	// IDEMPOTENCY_PLAN.md D-a). Same degradation posture as publishFailureCounter above: nil-safe, Add
	// guarded, never fatal to construction.
	eventIDReplacedCounter metric.Int64Counter
}

func NewIngestService(publisher eventPublisher) (*IngestService, error) {
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize validator: %w", err)
	}

	failureCounter, err := otel.Meter(tracerName).Int64Counter(
		obs.MetricIngestPublishFailures,
		metric.WithDescription("NATS publish failures for the ingest error_events subject"),
		metric.WithUnit("{failure}"),
	)
	if err != nil {
		// A dead collector must never block startup (OBSERVABILITY_PLAN.md D-b); creating this
		// counter can in practice only fail on a malformed instrument name/config, which the fixed
		// obs.MetricIngestPublishFailures constant is not. Degrade to "publish failures are not
		// recorded as a metric" (logged once) rather than failing ingest-service construction.
		slog.Default().Error("obs: failed to create ingest publish-failure counter; publish failures will not be recorded as a metric",
			slog.String("error", err.Error()))
	}

	eventIDReplacedCounter, err := otel.Meter(tracerName).Int64Counter(
		obs.MetricIngestEventIDReplaced,
		metric.WithDescription("Ingest requests whose client-supplied event_id was replaced with a minted UUIDv4"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		// Same degradation posture as the publish-failure counter above (OBSERVABILITY_PLAN.md D-b):
		// creation failing here means the fixed obs.MetricIngestEventIDReplaced constant is somehow
		// malformed, not that anything about a given request is wrong. Degrade to "not recorded as a
		// metric" rather than failing ingest-service construction.
		slog.Default().Error("obs: failed to create ingest event_id-replaced counter; event_id replacements will not be recorded as a metric",
			slog.String("error", err.Error()))
	}

	return &IngestService{
		publisher:              publisher,
		validator:              v,
		publishFailureCounter:  failureCounter,
		eventIDReplacedCounter: eventIDReplacedCounter,
	}, nil
}

// resolveEventID applies D-a's event_id policy (docs/plans/IDEMPOTENCY_PLAN.md): a client-supplied id
// 1-64 characters long is used verbatim; an absent (empty), oversized (>64 chars), or
// control-character-carrying id is replaced with a fresh UUIDv4 minted here. reason is "" when the
// client's id was used as-is, or one of obs.EventIDReasonEmpty/TooLong/InvalidChars when it was
// replaced - callers use it to decide whether to log/count the replacement. offendingLength is the
// length of the REJECTED client value (never the value itself, per the D15 cardinality/PII rule for
// log fields carrying client input).
//
// The length check counts RUNES, not bytes, deliberately: the other two enforcement points both count
// characters - the proto CEL rule (this.event_id.size() counts code points, proven by execution during
// W0 review) and Postgres VARCHAR(64) (counts characters). A byte count here would reject multibyte
// ids in the 65..128-byte range that both downstream bounds accept, silently stripping those clients
// of dedup with a false "too_long" (F-VW0-1). Safe direction either way - byte-len >= rune-count, so
// nothing this passes can trip CEL or 22001 - but only the rune count keeps the client's valid id.
func resolveEventID(clientEventID string) (effectiveID string, reason string, offendingLength int) {
	switch {
	case clientEventID == "":
		return uuid.New().String(), obs.EventIDReasonEmpty, 0
	case utf8.RuneCountInString(clientEventID) > maxEventIDLength:
		return uuid.New().String(), obs.EventIDReasonTooLong, utf8.RuneCountInString(clientEventID)
	case strings.ContainsFunc(clientEventID, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		// Control characters - above all NUL, which Postgres cannot store in varchar at all. Without
		// this, a client-supplied "a<NUL>b" sails through JSON decoding and protovalidate (proven
		// during W0 review) and, once W2 writes the column, dies at the INSERT with 22001-class errors
		// that classifyStoreError marks Permanent: client-controllable dead-lettering of its own
		// events (F-VW0-2). Minting instead preserves the event and costs only that client's dedup.
		return uuid.New().String(), obs.EventIDReasonInvalidChars, utf8.RuneCountInString(clientEventID)
	default:
		return clientEventID, "", 0
	}
}

func (s *IngestService) Ingest(ctx context.Context, payload *validation.ErrorPayload) error {
	// D-a: resolve the effective event_id BEFORE mapping/validate/publish, and stamp it back onto the
	// payload so callers (handleIngest/handleBatchIngest) can echo the effective id in the HTTP
	// response without this method's signature changing - main.go reads payload.EventID after this
	// call returns.
	effectiveID, reason, offendingLength := resolveEventID(payload.EventID)
	if reason != "" {
		s.recordEventIDReplaced(ctx, reason, offendingLength, payload.ProjectKey)
	}
	payload.EventID = effectiveID

	event := mapping.MapPayloadToEvent(payload)

	if err := s.validator.Validate(event); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}

	data, err := proto.Marshal(event)
	if err != nil {
		return err
	}

	return s.publish(ctx, data)
}

// recordEventIDReplaced is the loud half of D-a's invalid-id policy: never silently re-mint.
//
// Log level is chosen per reason (F-VW0-4): "empty" is the DESIGNED-FOR case — every pre-W0 client,
// every non-Go SDK, every curl — so at WARN it would produce log volume equal to ingest volume for
// normal traffic; it logs at Debug. The genuinely anomalous reasons (too_long, invalid_chars — an
// actual client bug) stay at WARN. The metric counts every reason regardless: the "empty" series is
// the adoption signal ("what fraction of my traffic is dedup-capable"), and two extra fixed series
// cost nothing.
//
// Note for anyone diffing this counter against sentinel_ingest_requests_total{outcome="accepted"}:
// it counts REPLACEMENTS AT RESOLVE TIME, not accepted events — an item that later fails validation
// has already been counted here (F-VW0-5). The two series are not expected to reconcile.
func (s *IngestService) recordEventIDReplaced(ctx context.Context, reason string, offendingLength int, projectKey string) {
	logFn := slog.Default().WarnContext
	if reason == obs.EventIDReasonEmpty {
		logFn = slog.Default().DebugContext
	}
	logFn(ctx, "ingest: client event_id replaced with a minted UUIDv4",
		slog.String("reason", reason),
		slog.Int("event_id_length", offendingLength),
		slog.String("project_key", projectKey),
	)
	if s.eventIDReplacedCounter != nil {
		s.eventIDReplacedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(obs.LabelReason, reason)))
	}
}

// publish opens a producer span around the NATS publish and injects that span's context into the
// message headers as a W3C traceparent (via obs.NATSHeaderCarrier and the global propagator
// obs.Bootstrap installs), so the processor's subscriber (W2) can extract it and open a consumer span
// that is a genuine child of this one rather than a disconnected root — this is the exact hop
// docs/plans/OBSERVABILITY_PLAN.md D-e specifies as the wire contract, and W2 depends on this half
// being correct.
func (s *IngestService) publish(ctx context.Context, data []byte) error {
	ctx, span := otel.Tracer(tracerName).Start(ctx, "publish "+natsSubject,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attribute.String("messaging.destination.name", natsSubject)),
	)
	defer span.End()

	// obs.NATSHeaderCarrier adapts nats.Header (map[string][]string) to propagation.TextMapCarrier.
	// Its Set is NOT nil-safe (mirrors http.Header's carrier contract), so the underlying map must be
	// allocated before Inject writes into it.
	headers := obs.NATSHeaderCarrier(natsgo.Header{})
	otel.GetTextMapPropagator().Inject(ctx, headers)

	if err := s.publisher.PublishWithHeaders(ctx, data, nats.Header(headers)); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		if s.publishFailureCounter != nil {
			s.publishFailureCounter.Add(ctx, 1)
		}
		return err
	}
	return nil
}
