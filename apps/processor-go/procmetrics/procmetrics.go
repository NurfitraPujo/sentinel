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

	// The four sentinel-project-environments §12 instruments. Every one is UNLABELLED — see the
	// obs.go block that declares their names. The gauge is a synchronous Int64Gauge rather than an
	// observable one because refreshConfigs already computes the number as a side effect of a query
	// it already runs: a callback-based gauge would need a second path to that slice.
	unregisteredEnvironmentEvents  metric.Int64Counter
	environmentRegistryTouchFailed metric.Int64Counter
	alertsSuppressedByEnvironment  metric.Int64Counter
	alertConfigsEnvironmentScoped  metric.Int64Gauge

	// sentinel-affected-users §12: the one instrument that feature adds. Labelled, unlike the four
	// above, but only with the two CLOSED sets obs.go declares (kind ∈ {user, tenant}, reason ∈
	// {empty, too_long, control_char}) — never the identifier itself.
	affectedRollupSkipped metric.Int64Counter

	// sentinel-reporting-and-dashboard D8a: the live per-project counters, recorded after commit by StoreEvent.
	eventsStored         metric.Int64Counter
	issuesCreated        metric.Int64Counter
	issuesRegressed      metric.Int64Counter
	serviceLabelOverflow metric.Int64Counter

	// sentinel-releases-snooze §12: the one instrument that feature adds to the processor. UNLABELLED,
	// like the four environment instruments — and unlike them it has a twin of the same name in the
	// dashboard's registry (metrics.ts), which is why a label would be wrong rather than merely
	// unnecessary: the two series are meant to sum by name at the scrape (decision 30).
	releaseNextRewriteFailed metric.Int64Counter

	// sentinel-sourcemaps §12: the five processor instruments of the symbolication stage
	// (apps/processor-go/symbolicate), plus turbopack-grouping §4.2's sixth below. The two
	// _total counters carry ONE label each from the closed sets obs.go declares
	// (SymbolicationEvent*, SymbolicationFrame*); the cache counter carries obs.LabelKind from
	// obs.SourcemapCache*; the histogram and the gauge are unlabelled. Never a project_id, never
	// a path. The gauge is a synchronous Int64Gauge because the stage reports the
	// byte total on every change through its Observer seam; an observable one would need a second
	// path into the cache.
	symbolicationEvents metric.Int64Counter
	symbolicationFrames metric.Int64Counter
	// sentinel-sourcemaps-turbopack-grouping §4.2: the in_app re-derivation counter, TWO labels
	// (obs.LabelTo ∈ obs.SymbolicationInAppTo*, obs.LabelChanged ∈ obs.SymbolicationInAppChanged*),
	// incremented once per frame beside symbolicationFrames so the two sum by construction (P9).
	symbolicationInAppRederived metric.Int64Counter
	sourcemapFetchDuration      metric.Float64Histogram
	sourcemapCacheBytes         metric.Int64Gauge
	sourcemapCacheEvents        metric.Int64Counter
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

	unregisteredEnvironmentEvents, err = meter.Int64Counter(
		obs.MetricUnregisteredEnvironmentEvents,
		metric.WithDescription("Count of stored occurrences whose environment has no project_environments row (decision 9: accepted and stored anyway)"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create unregistered environment events counter", slog.String("error", err.Error()))
		unregisteredEnvironmentEvents = nil
	}

	environmentRegistryTouchFailed, err = meter.Int64Counter(
		obs.MetricEnvironmentRegistryTouchFailures,
		metric.WithDescription("Count of failures of the project_environments last_seen_at/first_seen_at touch performed inside StoreEvent"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create environment registry touch failure counter", slog.String("error", err.Error()))
		environmentRegistryTouchFailed = nil
	}

	alertsSuppressedByEnvironment, err = meter.Int64Counter(
		obs.MetricAlertsSuppressedByEnvironment,
		metric.WithDescription("Count of occurrences not counted toward an alert config's threshold because their environment is outside the config's scope"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create environment-suppressed alerts counter", slog.String("error", err.Error()))
		alertsSuppressedByEnvironment = nil
	}

	alertConfigsEnvironmentScoped, err = meter.Int64Gauge(
		obs.MetricAlertConfigsEnvironmentScoped,
		metric.WithDescription("Number of enabled alert_configs rows whose environments is non-NULL, as of the last refreshConfigs pass"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create environment-scoped alert configs gauge", slog.String("error", err.Error()))
		alertConfigsEnvironmentScoped = nil
	}

	affectedRollupSkipped, err = meter.Int64Counter(
		obs.MetricAffectedRollupSkipped,
		metric.WithDescription("Count of user/tenant identifiers skipped by the issue affected-users/tenants rollup because the value was unusable (empty, too long, or carrying a control character); the event itself was stored"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create affected rollup skipped counter", slog.String("error", err.Error()))
		affectedRollupSkipped = nil
	}

	eventsStored, err = meter.Int64Counter(obs.MetricEventsStored,
		metric.WithDescription("Occurrences stored, by project, environment, service, D19 group and level (D8a)"))
	if err != nil {
		slog.Error("procmetrics: failed to create events stored counter", slog.String("error", err.Error()))
		eventsStored = nil
	}
	issuesCreated, err = meter.Int64Counter(obs.MetricIssuesCreated,
		metric.WithDescription("Issues created, by project and environment (D8a)"))
	if err != nil {
		slog.Error("procmetrics: failed to create issues created counter", slog.String("error", err.Error()))
		issuesCreated = nil
	}
	issuesRegressed, err = meter.Int64Counter(obs.MetricIssuesRegressed,
		metric.WithDescription("Issues regressed, by project and environment (D8a)"))
	if err != nil {
		slog.Error("procmetrics: failed to create issues regressed counter", slog.String("error", err.Error()))
		issuesRegressed = nil
	}
	serviceLabelOverflow, err = meter.Int64Counter(obs.MetricServiceLabelOverflow,
		metric.WithDescription("Occurrences whose service exceeded the per-project label limit and were recorded as other (D21)"))
	if err != nil {
		slog.Error("procmetrics: failed to create service label overflow counter", slog.String("error", err.Error()))
		serviceLabelOverflow = nil
	}

	releaseNextRewriteFailed, err = meter.Int64Counter(
		obs.MetricReleaseNextRewriteFailures,
		metric.WithDescription("Count of failures of the post-commit rewrite of resolved_in_version = 'next' to a newly seen release version; the event itself was stored and the hourly catch-up self-heals"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create release next rewrite failure counter", slog.String("error", err.Error()))
		releaseNextRewriteFailed = nil
	}

	symbolicationEvents, err = meter.Int64Counter(
		obs.MetricSymbolicationEvents,
		metric.WithDescription("Count of events through the symbolication gate and stage by outcome (applied_all, applied_partial, none, skipped_platform, skipped_no_release, skipped_no_frames)"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create symbolication events counter", slog.String("error", err.Error()))
		symbolicationEvents = nil
	}

	symbolicationFrames, err = meter.Int64Counter(
		obs.MetricSymbolicationFrames,
		metric.WithDescription("Count of stack frames through the symbolication stage by outcome (applied, no_column, no_map, no_mapping, map_error, fetch_error, timeout, oversize); a negative-cache hit re-emits its reason"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create symbolication frames counter", slog.String("error", err.Error()))
		symbolicationFrames = nil
	}

	symbolicationInAppRederived, err = meter.Int64Counter(
		obs.MetricSymbolicationInAppRederived,
		metric.WithDescription("Count of stack frames through the symbolication stage by the in_app value the stage re-derived (to: true, false, or kept when the frame was not rewritten) and whether it differs from the value the SDK sent (changed)"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create symbolication in_app rederived counter", slog.String("error", err.Error()))
		symbolicationInAppRederived = nil
	}

	// Same buckets as processDuration: the cold path is one object GET plus one parse, and §13 M2
	// reads its p95 against a 500 ms shape number on compose.
	sourcemapFetchDuration, err = meter.Float64Histogram(
		obs.MetricSourcemapFetchDuration,
		metric.WithDescription("Time spent fetching and parsing one sourcemap on the cold path (object GET + parse)"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create sourcemap fetch duration histogram", slog.String("error", err.Error()))
		sourcemapFetchDuration = nil
	}

	sourcemapCacheBytes, err = meter.Int64Gauge(
		obs.MetricSourcemapCacheBytes,
		metric.WithDescription("Current cost of the sourcemap positive cache in raw bytes (the rows' size_bytes, not the parsed size)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create sourcemap cache bytes gauge", slog.String("error", err.Error()))
		sourcemapCacheBytes = nil
	}

	sourcemapCacheEvents, err = meter.Int64Counter(
		obs.MetricSourcemapCacheEvents,
		metric.WithDescription("Count of sourcemap cache events by kind (hit, miss, negative_hit, evict, oversize)"),
	)
	if err != nil {
		slog.Error("procmetrics: failed to create sourcemap cache events counter", slog.String("error", err.Error()))
		sourcemapCacheEvents = nil
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

// RecordUnregisteredEnvironmentEvent records one occurrence stored for an environment that has no
// project_environments row. It is deliberately NOT an error signal: decision 9 accepts the event and
// stores it, and this counter is the only thing that says so out loud.
func RecordUnregisteredEnvironmentEvent(ctx context.Context) {
	if unregisteredEnvironmentEvents != nil {
		unregisteredEnvironmentEvents.Add(ctx, 1)
	}
}

// RecordEnvironmentRegistryTouchFailure records one failure of §4.8's project_environments touch.
//
// Its single caller is the touch's error branch in store.StoreEvent (apps/processor-go/store/store.go),
// which logs, records here, and then still COMMITS the transaction — §4.8 point 1. It was recorded
// twice as having zero callers (BUG-12, then BUG-19) because the store returned the UPDATE's error
// instead; the test that now pins the increment against a real failing UPDATE is
// tests/integration's TestStoreEvent_RegistryTouchFailureNeverDropsTheEvent, and it goes red when
// this call is removed from the store. If you delete that call, delete this recorder and the
// constant with it rather than shipping a counter that structurally cannot fire.
func RecordEnvironmentRegistryTouchFailure(ctx context.Context) {
	if environmentRegistryTouchFailed != nil {
		environmentRegistryTouchFailed.Add(ctx, 1)
	}
}

// RecordAffectedRollupSkipped records one identifier that sentinel-affected-users §4.3's rollup did
// NOT write because decision 5 judged the value unusable. kind must be obs.AffectedKindUser or
// obs.AffectedKindTenant and reason one of the obs.AffectedRollupReason* constants — both closed
// sets; the identifier itself is never passed here (D15: a client string is not a label).
//
// Its single caller is store.StoreEvent, AFTER tx.Commit and beside RecordUnregisteredEnvironmentEvent:
// the counter describes identifiers skipped on occurrences that were STORED, so a delivery that rolled
// back (a duplicate, an insert failure) must not appear on it. tests/integration's
// TestAffectedRollup_UnusableIDsStoreTheEventAndCountTheSkip pins the increment per reason and goes
// red when this call is removed from the store.
func RecordAffectedRollupSkipped(ctx context.Context, kind, reason string) {
	if affectedRollupSkipped != nil {
		affectedRollupSkipped.Add(ctx, 1, metric.WithAttributes(
			attribute.String(obs.LabelKind, kind),
			attribute.String(obs.LabelReason, reason),
		))
	}
}

// RecordReleaseNextRewriteFailure records one failed post-commit `next` rewrite (sentinel-releases-
// snooze §4.3, decision 15): the UPDATE that fans a newly inserted release's version out onto the
// project's `resolved_in_version = 'next'` issues did not commit. No label — the counter's twin in
// the dashboard's registry carries the same name for the hourly catch-up's failures and the two sum
// at the scrape (decision 30); the SQLSTATE goes on the log line beside the call, never here.
//
// Its single caller is service.(*ProcessorService).rewriteNextIssues, which runs only after
// store.StoreEvent has COMMITTED with occ.ReleaseInserted = true — so, like RecordAffectedRollupSkipped
// above, a delivery that rolled back can never reach it. tests/integration's
// TestProcessorService_NextRewriteFailureIsCountedAndNeverFailsTheEvent pins the increment against a
// real failing UPDATE and goes red when this call is removed from the service.
func RecordReleaseNextRewriteFailure(ctx context.Context) {
	if releaseNextRewriteFailed != nil {
		releaseNextRewriteFailed.Add(ctx, 1)
	}
}

// RecordAlertSuppressedByEnvironment records one occurrence that an alert config did not count
// toward its frequency threshold because the occurrence's environment is outside the config's
// `environments` scope. Recorded once per (occurrence, config) pair — a config that covers the
// environment records nothing here, and neither does an occurrence for which no config resolved.
func RecordAlertSuppressedByEnvironment(ctx context.Context) {
	if alertsSuppressedByEnvironment != nil {
		alertsSuppressedByEnvironment.Add(ctx, 1)
	}
}

// SetAlertConfigsEnvironmentScoped publishes the number of enabled alert_configs rows carrying a
// non-NULL `environments`, as counted by the refreshConfigs pass that just completed. It is a Set,
// not an Add: a refresh that finds fewer scoped configs than the previous one must move this number
// DOWN, which is the whole reason §12 made it a gauge.
func SetAlertConfigsEnvironmentScoped(ctx context.Context, n int) {
	if alertConfigsEnvironmentScoped != nil {
		alertConfigsEnvironmentScoped.Record(ctx, int64(n))
	}
}

// RecordSymbolicationEvent records one event's pass through sentinel-sourcemaps §4.4's gate and
// stage: outcome is one of obs.SymbolicationEvent* — the derivation of the frames' outcomes
// (applied_all / applied_partial / none) when the stage ran, or the skipped_* reason when the gate
// refused it. Exactly one per delivery that reached the stage, recorded by
// service.(*ProcessorService).symbolicate before the fingerprint is computed; a delivery that
// failed to resolve its project never gets here.
func RecordSymbolicationEvent(ctx context.Context, outcome string) {
	if symbolicationEvents != nil {
		symbolicationEvents.Add(ctx, 1, metric.WithAttributes(attribute.String(obs.LabelOutcome, outcome)))
	}
}

// RecordSymbolicationFrame records one frame's outcome through the stage: obs.SymbolicationFrame*
// (`applied` or one of the seven pass-through reasons). One per frame of every event the gate
// admitted; a negative-cache hit re-emits the reason the entry was cached with, which is what
// makes ongoing breakage visible on this counter (§12).
func RecordSymbolicationFrame(ctx context.Context, outcome string) {
	if symbolicationFrames != nil {
		symbolicationFrames.Add(ctx, 1, metric.WithAttributes(attribute.String(obs.LabelOutcome, outcome)))
	}
}

// RecordInAppRederived records one frame's in_app re-derivation
// (sentinel-sourcemaps-turbopack-grouping §4.2 Metric): to is one of obs.SymbolicationInAppTo*
// (`true`/`false` for a frame the stage rewrote — the value it now carries — or `kept` for any
// other outcome), changed is one of obs.SymbolicationInAppChanged* (`true` when the re-derived
// value differs from raw.in_app; always `false` with `kept`). Exactly one per frame, recorded by
// service.(*ProcessorService).symbolicate in the SAME loop as RecordSymbolicationFrame, so the
// sum over every to×changed series equals frames_total (flow P9). Never called from the stage:
// the stage has overwritten the SDK's value by the time it could report it.
func RecordInAppRederived(ctx context.Context, to, changed string) {
	if symbolicationInAppRederived != nil {
		symbolicationInAppRederived.Add(ctx, 1, metric.WithAttributes(
			attribute.String(obs.LabelTo, to),
			attribute.String(obs.LabelChanged, changed),
		))
	}
}

// RecordSourcemapFetchDuration records one cold-path fetch + parse (§12's
// sentinel_sourcemap_fetch_duration_seconds). Reached through symbolicate.Observer.FetchDuration,
// which carries no context: the fill may be running in the background after the event that
// started it was stored (decision 37), so there is no request context to attach.
func RecordSourcemapFetchDuration(d time.Duration) {
	if sourcemapFetchDuration != nil {
		sourcemapFetchDuration.Record(context.Background(), d.Seconds())
	}
}

// SetSourcemapCacheBytes publishes the positive cache's current raw-byte cost after a change. A
// Set, not an Add: an eviction moves it DOWN, which is why §12 made it a gauge.
func SetSourcemapCacheBytes(n int64) {
	if sourcemapCacheBytes != nil {
		sourcemapCacheBytes.Record(context.Background(), n)
	}
}

// RecordSourcemapCacheEvent records one cache event: kind is one of obs.SourcemapCache* (hit,
// miss, negative_hit, evict, oversize — the last being a map over the CACHE bound, fetched and not
// retained; a row over the parse cap never reaches the cache and is a frame outcome instead).
func RecordSourcemapCacheEvent(kind string) {
	if sourcemapCacheEvents != nil {
		sourcemapCacheEvents.Add(context.Background(), 1, metric.WithAttributes(attribute.String(obs.LabelKind, kind)))
	}
}

// StoredEvent is what StoreEvent hands the D8a counters after a commit. ProjectName is the raw Postgres project
// name (slugified here, D21); the identifiers and message of the event are never part of it.
type StoredEvent struct {
	ProjectName string
	Environment string
	// EnvDeclared is true when the environment has a project_environments row (D21: else "other").
	EnvDeclared bool
	Service     string
	Group       string
	Level       string
	// IssueNew / IssueRegressed are the post-write issue outcome of the same delivery.
	IssueNew       bool
	IssueRegressed bool
}

var serviceBounder = NewServiceBounder()

// RecordStored increments the D8a counters for one committed occurrence. Every label passes through the D21 bounds.
// It never returns an error and never blocks ingest: a nil instrument (failed registration) is a no-op.
func RecordStored(ctx context.Context, e StoredEvent) {
	project := Slug(e.ProjectName)
	env := Environment(e.Environment, e.EnvDeclared)
	svc, overflowed := serviceBounder.Bound(project, e.Service)
	if overflowed && serviceLabelOverflow != nil {
		serviceLabelOverflow.Add(ctx, 1, metric.WithAttributes(attribute.String(obs.LabelProject, project)))
	}
	if eventsStored != nil {
		eventsStored.Add(ctx, 1, metric.WithAttributes(
			attribute.String(obs.LabelProject, project),
			attribute.String(obs.LabelEnvironment, env),
			attribute.String(obs.LabelService, svc),
			attribute.String(obs.LabelGroup, e.Group),
			attribute.String(obs.LabelLevel, Level(e.Level)),
		))
	}
	issueAttrs := metric.WithAttributes(
		attribute.String(obs.LabelProject, project),
		attribute.String(obs.LabelEnvironment, env),
	)
	if e.IssueNew && issuesCreated != nil {
		issuesCreated.Add(ctx, 1, issueAttrs)
	}
	if e.IssueRegressed && issuesRegressed != nil {
		issuesRegressed.Add(ctx, 1, issueAttrs)
	}
}
