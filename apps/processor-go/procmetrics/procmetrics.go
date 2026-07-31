// Package procmetrics holds the processor's OTel metric instruments and the handful of narrow
// recording helpers that use them. It exists as its own small package (rather than living inside
// service, alerts, or notifiers) because the alert-dispatch metric is recorded from two places that
// cannot import each other: apps/processor-go/alerts (which resolves configs and rate-limits) and
// apps/processor-go/notifiers (which alerts imports, and which owns the actual SMTP/Telegram call
// whose outcome is what "notifier accepted it" / "notifier failed" (obs.OutcomeDispatchSent/
// OutcomeDispatchError) actually mean). A shared leaf package both can import, with zero
// apps/processor-go-internal dependencies of its own, avoids that cycle.
//
// See docs/plans/OBSERVABILITY_PLAN.md §2/§4 (W2) for the metric name/label contract this
// implements; the instruments themselves come from packages/shared-go/obs's fixed constants — never
// hand-typed strings, for the same B5 reason obs.go documents.
package procmetrics

import (
	"context"
	"log/slog"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meter is obtained from the otel API's global accessor, not threaded in from main(). This is safe
// before obs.Bootstrap ever runs: go.opentelemetry.io/otel's global package delegates — a Meter (and
// the instruments created from it) obtained before otel.SetMeterProvider is called are transparently
// upgraded in place once Bootstrap calls it, per the OTel Go API's documented deferred/global
// mechanism. Package-level initialization order in Go therefore cannot race Bootstrap: whichever runs
// first, both trace and metric data end up flowing through the real SDK provider once Bootstrap has
// run, and through a working no-op before that (or always, under OTEL_SDK_DISABLED).
var meter = otel.Meter("processor-go")

var (
	processDuration metric.Float64Histogram
	processEvents   metric.Int64Counter
	alertDispatch   metric.Int64Counter
)

func init() {
	var err error

	// Buckets per OBSERVABILITY_PLAN.md §2: "explicit buckets 5ms->10s".
	processDuration, err = meter.Float64Histogram(
		obs.MetricProcessDuration,
		metric.WithDescription("Time spent processing one error event end-to-end (deserialize through index/alert-dispatch)"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		// Instrument creation only fails on a malformed name/config, which cannot happen with a fixed
		// literal constant — but degrading to "this metric silently stops recording" is still strictly
		// better than a nil-pointer panic on the hot path. See RecordProcessed's nil guard below.
		slog.Error("procmetrics: failed to create process duration histogram", slog.String("error", err.Error()))
		processDuration = nil
	}

	processEvents, err = meter.Int64Counter(
		obs.MetricProcessEvents,
		metric.WithDescription("Count of processed events by outcome (stored, duplicate, retried, deadlettered)"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create process events counter", slog.String("error", err.Error()))
		processEvents = nil
	}

	alertDispatch, err = meter.Int64Counter(
		obs.MetricAlertDispatch,
		metric.WithDescription("Count of alert dispatch attempts by channel and outcome"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create alert dispatch counter", slog.String("error", err.Error()))
		alertDispatch = nil
	}
}

// RecordProcessed records one ProcessEvent completion: MetricProcessDuration (seconds) and
// MetricProcessEvents{outcome} in one call, so every call site records both together rather than
// risking one without the other. outcome must be one of obs.OutcomeStored/OutcomeRetried/
// OutcomeDeadLettered.
func RecordProcessed(ctx context.Context, outcome string, d time.Duration) {
	if processDuration != nil {
		processDuration.Record(ctx, d.Seconds())
	}
	if processEvents != nil {
		processEvents.Add(ctx, 1, metric.WithAttributes(attribute.String(obs.LabelOutcome, outcome)))
	}
}

// RecordAlertDispatch records one MetricAlertDispatch{channel,outcome} observation. channel is a
// free-form value (obs.LabelChannel has no fixed permitted-value set, unlike LabelOutcome) — callers
// use the same literal channel strings ("email", "telegram") alerts.AlertConfig.Channel and
// notify.go's BuildSender already switch on. outcome should be obs.OutcomeDispatchSent,
// obs.OutcomeDispatchError, or obs.OutcomeDispatchDropped — the last one is recorded at every place an
// alert never reaches a notifier worker at all (missing routing config, unknown channel, a full queue,
// no sender wired), which is what makes "quiet because healthy" distinguishable from "quiet because
// broken" on this counter (see notify.go and dispatcher.go's sendAlert).
func RecordAlertDispatch(ctx context.Context, channel, outcome string) {
	if alertDispatch != nil {
		alertDispatch.Add(ctx, 1, metric.WithAttributes(
			attribute.String(obs.LabelChannel, channel),
			attribute.String(obs.LabelOutcome, outcome),
		))
	}
}
