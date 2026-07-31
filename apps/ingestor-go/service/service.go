package service

import (
	"context"
	"fmt"
	"log/slog"

	"buf.build/go/protovalidate"
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

// tracerName is the OTel instrumentation scope name for spans and instruments this package creates. It
// matches the ingestor's obs.Setup/obs.Bootstrap ServiceName ("ingestor-go") so a trace's spans and the
// resource's service.name agree — see docs/plans/OBSERVABILITY_PLAN.md D-b/W1.
const tracerName = "ingestor-go"

// natsSubject mirrors main.go's nats.PublisherConfig.Subject. It exists here purely for span naming/
// attribution (observability), not routing — the actual subject a message is published to is whatever
// the injected *nats.Publisher was constructed with.
const natsSubject = "error_events"

type IngestService struct {
	publisher *nats.Publisher
	validator protovalidate.Validator

	// publishFailureCounter records obs.MetricIngestPublishFailures. May be a degraded (nil-safe, per
	// the OTel metric API's own contract) instrument if creation failed — Add is still guarded with a
	// nil check below since "may fail" here means "may be less than fully functional," never "must
	// crash the ingestor" (docs/plans/OBSERVABILITY_PLAN.md D-b degradation mandate).
	publishFailureCounter metric.Int64Counter
}

func NewIngestService(publisher *nats.Publisher) (*IngestService, error) {
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

	return &IngestService{
		publisher:             publisher,
		validator:             v,
		publishFailureCounter: failureCounter,
	}, nil
}

func (s *IngestService) Ingest(ctx context.Context, payload *validation.ErrorPayload) error {
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
