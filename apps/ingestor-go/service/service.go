package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"buf.build/go/protovalidate"
	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/enrich"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"google.golang.org/protobuf/proto"
)

// maxEventIDLength matches error_occurrences.event_id VARCHAR(64) / the proto's "error_event.event_id"
// CEL rule (packages/proto/sentinel/v1/error_event.proto) - see docs/plans/IDEMPOTENCY_PLAN.md D-a/D-b.
const maxEventIDLength = validation.MaxEventIDLength

// MaxFutureSkew and MaxEventAge bound the window around receipt time (T_recv) inside which a
// client-supplied timestamp is believed (sentinel-ingest-timestamp-clamp contract §4.1, decisions 4,
// 8, 9). They are compile-time constants, not configuration, for the same reason maxEventIDLength is:
// a bound that varies per deployment is not stated anywhere a client can read.
//
//   - A value more than MaxFutureSkew AHEAD of T_recv is disbelieved (R2, reason "future"). A clock a
//     minute fast is stored as sent; anything further is stored at T_recv.
//   - A value more than MaxEventAge BEHIND T_recv is disbelieved (R3, reason "ancient"). Equal to the
//     dashboard's default DATA_RETENTION_DAYS (retention-window.ts): an occurrence older than this is
//     deleted by the next sweep whatever the ingestor does. The coupling is stated, not enforced.
const (
	MaxFutureSkew = 60 * time.Second
	MaxEventAge   = 30 * 24 * time.Hour
)

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

	// timestampReplacedCounter records obs.MetricIngestTimestampReplaced{reason}
	// (sentinel-ingest-timestamp-clamp §4.3). Same degradation posture as the two above.
	timestampReplacedCounter metric.Int64Counter

	// fieldsTruncatedCounter records obs.MetricIngestFieldsTruncated{field} (sentinel-event-schema-v2
	// spec §4.2). Same degradation posture as the three above.
	fieldsTruncatedCounter metric.Int64Counter

	// enricher carries the geo lookup and the User-Agent parser enrich.Apply uses
	// (sentinel-event-enrichment spec §7.2). nil means those fills are off; the IP rule always runs.
	enricher *enrich.Enricher
}

// Option configures an IngestService. The variadic form keeps every existing NewIngestService(publisher)
// caller compiling unchanged (tests/unit is one flat package, B4).
type Option func(*IngestService)

// WithEnricher sets the in-process lookups enrich.Apply uses (sentinel-event-enrichment spec §7.2).
func WithEnricher(e *enrich.Enricher) Option {
	return func(s *IngestService) { s.enricher = e }
}

func NewIngestService(publisher eventPublisher, opts ...Option) (*IngestService, error) {
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

	timestampReplacedCounter, err := otel.Meter(tracerName).Int64Counter(
		obs.MetricIngestTimestampReplaced,
		metric.WithDescription("Ingest events whose client-supplied timestamp was replaced with the ingestor's receipt time"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		// Same degradation posture as the two counters above (OBSERVABILITY_PLAN.md D-b; contract
		// §12): "not recorded as a metric", never a refused ingest or a failed construction.
		slog.Default().Error("obs: failed to create ingest timestamp-replaced counter; timestamp replacements will not be recorded as a metric",
			slog.String("error", err.Error()))
	}

	fieldsTruncatedCounter, err := otel.Meter(tracerName).Int64Counter(
		obs.MetricIngestFieldsTruncated,
		metric.WithDescription("Ingest events with a field the bounds step truncated, dropped or replaced, per field"),
		metric.WithUnit("{event}"),
	)
	if err != nil {
		// Same degradation posture as the counters above: "not recorded as a metric", never a refused
		// ingest or a failed construction.
		slog.Default().Error("obs: failed to create ingest fields-truncated counter; field truncations will not be recorded as a metric",
			slog.String("error", err.Error()))
	}

	s := &IngestService{
		publisher:                publisher,
		validator:                v,
		publishFailureCounter:    failureCounter,
		eventIDReplacedCounter:   eventIDReplacedCounter,
		timestampReplacedCounter: timestampReplacedCounter,
		fieldsTruncatedCounter:   fieldsTruncatedCounter,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
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

// resolveTimestamp applies sentinel-ingest-timestamp-clamp §4.1's rule, the timestamp sibling of
// resolveEventID: given the decoded client value ts and the request's receipt time receivedAt
// (T_recv), it returns the EFFECTIVE timestamp - the only one anything downstream ever sees - and the
// reason it was replaced, or "" when the client's value was used as-is. First match wins:
//
//	R1  ts.IsZero()                    -> receivedAt, obs.TimestampReasonEmpty
//	R2  ts > receivedAt + MaxFutureSkew -> receivedAt, obs.TimestampReasonFuture
//	R3  ts < receivedAt - MaxEventAge   -> receivedAt, obs.TimestampReasonAncient   (decision 10)
//	R4  otherwise                       -> ts, verbatim as the same instant in UTC
//
// R1 covers an absent key, an explicit null and the literal "0001-01-01T00:00:00Z" alike: encoding/json
// leaves time.Time at its zero value for all three (decision 1). Pure - it never reads the clock
// itself; the caller reads it ONCE per HTTP request (decision 3) so every item of a batch compares
// against, and under R1-R3 receives, the same instant. Replacing with T_recv rather than clamping to
// the window's edge is deliberate: it is what the processor already writes for releases (LEAST($3,
// now()), decision 50 of sentinel-releases-snooze), and two different future clocks on one row would
// be a third thing to explain. Never a rejection (decision 5): the SDK contract says never retry a
// 4xx, so a 400 here would drop the event this rule exists to keep.
func resolveTimestamp(ts time.Time, receivedAt time.Time) (effective time.Time, reason string) {
	// Every branch returns UTC: the effective value is stamped back onto the payload and echoed with
	// a "Z" suffix (decision 12), whatever location the caller's clock or the client's offset carried.
	receivedAt = receivedAt.UTC()
	switch {
	case ts.IsZero():
		return receivedAt, obs.TimestampReasonEmpty
	case ts.After(receivedAt.Add(MaxFutureSkew)):
		return receivedAt, obs.TimestampReasonFuture
	case ts.Before(receivedAt.Add(-MaxEventAge)):
		return receivedAt, obs.TimestampReasonAncient
	default:
		return ts.UTC(), ""
	}
}

// Ingest resolves, validates and publishes one event using the current wall clock as its receipt
// time. It is IngestAt with receivedAt = time.Now().UTC(); the HTTP handlers call IngestAt directly so
// one clock reading is shared by every item of a batch (decision 3), and this wrapper exists for the
// callers that have no request to share it across.
func (s *IngestService) Ingest(ctx context.Context, payload *validation.ErrorPayload) error {
	return s.IngestAt(ctx, payload, time.Now().UTC())
}

// IngestAt is Ingest with an explicit receipt time (T_recv, sentinel-ingest-timestamp-clamp §3):
// the ingestor's ONE UTC wall-clock reading for the HTTP request this payload arrived in, taken after
// the body was decoded and auth/scope passed, and shared by every item of a batch. The handlers read
// it once and pass it here; tests pass a fixed instant so R1-R4 are deterministic.
func (s *IngestService) IngestAt(ctx context.Context, payload *validation.ErrorPayload, receivedAt time.Time) error {
	// D-a: resolve the effective event_id BEFORE mapping/validate/publish, and stamp it back onto the
	// payload so callers (handleIngest/handleBatchIngest) can echo the effective id in the HTTP
	// response without this method's signature changing - main.go reads payload.EventID after this
	// call returns.
	effectiveID, reason, offendingLength := resolveEventID(payload.EventID)
	if reason != "" {
		s.recordEventIDReplaced(ctx, reason, offendingLength, payload.ProjectKey)
	}
	payload.EventID = effectiveID

	// sentinel-ingest-timestamp-clamp §4.1 (decision 2): resolve the effective timestamp HERE - after
	// resolveEventID, before MapPayloadToEvent - and stamp it back onto the payload in UTC, exactly as
	// the event_id above, so the mapper stays a verbatim copy and the handlers can echo the effective
	// value (decision 12) by reading payload.Timestamp after this call returns.
	effectiveTS, tsReason := resolveTimestamp(payload.Timestamp, receivedAt)
	if tsReason != "" {
		s.recordTimestampReplaced(ctx, tsReason, payload.Timestamp, receivedAt, payload.ProjectKey)
	}
	payload.Timestamp = effectiveTS
	// sentinel-event-schema-v2 (spec §4.1): proto field 30 is T_recv, stamped here and nowhere else.
	// ErrorPayload.ReceivedAt is json:"-", so whatever a client sent was never decoded.
	payload.ReceivedAt = receivedAt.UTC()

	// sentinel-event-enrichment spec §7.1: fill the client IP, geo and User-Agent contexts the SDK left
	// empty, BEFORE the bounds step so a derived value is bounded and marked exactly as a sent one. The
	// per-request Info (caller IP, connection User-Agent) was put on the context by clientip.Middleware;
	// a context without one (unit tests, Ingest) means no caller IP and no connection UA.
	// The handlers also put a per-request enrich.Budget on the context, so every item of a batch shares
	// one bounded User-Agent allowance (enr-code-ua-parse-cpu-amplification).
	info, _ := clientip.RequestInfoFromContext(ctx)
	enrich.ApplyWithBudget(payload, info, s.enricher, enrich.BudgetFromContext(ctx))

	// sentinel-event-schema-v2 spec §4.2-§4.3: the per-field bounds step, run on the live path. It
	// truncates, drops or replaces every oversized v2 value IN PLACE, writes the server_truncated
	// markers into metadata and shrinks an oversized metadata map; the one rule it rejects on is a
	// negative top-level stacktrace[i].column (sentinel-sourcemaps §4.3), which the proto cannot
	// express. Every other rejection is protovalidate's, below. The error text is prefixed
	// "validation failed" so handleIngest/handleBatchIngest map it to 400 exactly as a protovalidate
	// failure.
	if result := validation.ValidatePayload(payload); !result.Valid {
		msgs := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("validation failed: %s", strings.Join(msgs, "; "))
	}
	s.recordFieldsTruncated(ctx, payload)

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

// recordTimestampReplaced is the loud half of §4.1's rule (decision 11): a replaced timestamp is never
// silent. "empty" is the DESIGNED-FOR case - every non-Go client that simply omits the field - so it
// logs at Debug, exactly as recordEventIDReplaced treats an absent event_id; "future" and "ancient"
// mean a client whose clock is wrong and somebody should go and fix it, so they log at Warn. The
// original value is kept NOWHERE but this line (decision 11: not in metadata, not in a column), as an
// RFC 3339 string with the signed delta in whole seconds and the project_key - never the event_id.
// The D15 rule on client input in log fields is about labels and cardinality; a timestamp is neither
// PII nor high-cardinality as a log field, and it is the one thing an engineer asking "what did my
// client actually send" needs. For "empty" there is no original worth printing, so the two fields
// are omitted.
//
// Like its sibling, the counter counts REPLACEMENTS AT RESOLVE TIME (contract §4.3): an item that
// later fails protovalidate or the publish has already been counted, and the series is not expected
// to reconcile with sentinel_ingest_requests_total{outcome="accepted"}.
func (s *IngestService) recordTimestampReplaced(ctx context.Context, reason string, original time.Time, receivedAt time.Time, projectKey string) {
	attrs := []any{
		slog.String("reason", reason),
		slog.String("project_key", projectKey),
	}
	logFn := slog.Default().WarnContext
	if reason == obs.TimestampReasonEmpty {
		logFn = slog.Default().DebugContext
	} else {
		attrs = append(attrs,
			slog.String("original", original.UTC().Format(time.RFC3339)),
			slog.Int64("delta_seconds", int64(original.Sub(receivedAt)/time.Second)),
		)
	}
	logFn(ctx, "ingest: client timestamp replaced with receipt time", attrs...)
	if s.timestampReplacedCounter != nil {
		s.timestampReplacedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(obs.LabelReason, reason)))
	}
}

// recordFieldsTruncated is the loud half of the bounds step (spec §4.2 "Marker"): one counter Add and
// one Debug line per field the step cut, read back from the server-owned metadata.server_truncated
// marker it just wrote - which is why a client value under that key can never inflate the counter:
// the step overwrites or removes it first. Debug, not Warn, for the reason recordTimestampReplaced
// gives for "empty": an SDK that routinely sends oversized context would otherwise log at ingest
// volume. Never the value, its length or the project as a label.
func (s *IngestService) recordFieldsTruncated(ctx context.Context, payload *validation.ErrorPayload) {
	fields, _ := payload.Metadata[validation.MetadataKeyServerTruncated].([]interface{})
	for _, f := range fields {
		field, _ := f.(string)
		slog.Default().DebugContext(ctx, "ingest: field cut by the bounds step",
			slog.String("field", field),
			slog.String("project_key", payload.ProjectKey),
		)
		if s.fieldsTruncatedCounter != nil {
			s.fieldsTruncatedCounter.Add(ctx, 1, metric.WithAttributes(attribute.String(obs.LabelField, field)))
		}
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
