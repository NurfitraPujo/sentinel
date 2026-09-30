package sentrycompat

import (
	"context"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	MetricItems = "sentinel_compat_items_total"
	LabelType   = "type" // one of ItemTypes, or "unknown"

	// outcomes (label obs.LabelOutcome = "outcome")
	OutcomeAccepted  = "accepted"  // an event item IngestAt published
	OutcomeDiscarded = "discarded" // a non-event item, read past
	OutcomeTooLarge  = "too_large" // an event item over MaxEventItemBytes (envelope) or a store body over it, or a
	// mapped event over the publish budget after shedding / refused by NATS (§4.7, D13)
	OutcomeRejected = "rejected" // not a JSON object, refused by Config.Scope, or an IngestAt validation failure
	OutcomeFailed   = "failed"   // any other IngestAt error (publish, marshal)
	OutcomeSkipped  = "skipped"  // a kept event item after the one that stopped the envelope (§4.2 step 6)
)

// ItemTypes is the bounded set of item types the metric labels by; anything else is "unknown" (V6).
var ItemTypes = []string{"event", "transaction", "span", "session", "sessions", "client_report", "replay_event",
	"replay_recording", "replay_video", "attachment", "profile", "profile_chunk", "check_in", "log", "statsd",
	"feedback", "user_report"}

var itemTypeSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(ItemTypes))
	for _, t := range ItemTypes {
		m[t] = struct{}{}
	}
	return m
}()

// ItemTypeLabel maps a client item type onto the bounded label set.
func ItemTypeLabel(t string) string {
	if _, ok := itemTypeSet[t]; ok {
		return t
	}
	return "unknown"
}

// NewItemRecorder creates the Int64Counter MetricItems ("Sentry-compatible envelope items by type and outcome",
// unit "{item}") and returns the function the handlers call. A creation error is returned; main.go logs it and
// passes a no-op recorder, as it does for sentinel_ingest_requests_total.
func NewItemRecorder(meter metric.Meter) (func(ctx context.Context, itemType, outcome string), error) {
	c, err := meter.Int64Counter(MetricItems,
		metric.WithDescription("Sentry-compatible envelope items by type and outcome"),
		metric.WithUnit("{item}"))
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, itemType, outcome string) {
		c.Add(ctx, 1, metric.WithAttributes(
			attribute.String(LabelType, ItemTypeLabel(itemType)),
			attribute.String(obs.LabelOutcome, outcome)))
	}, nil
}
