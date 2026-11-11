package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/dlqmonitor"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/spanpull"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/spans"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/tagfacets"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/webhooks"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/database"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Deliverable 1 (OBSERVABILITY_PLAN.md W2): slog + OTel bootstrap, first, before anything else
	// opens a connection worth tracing. slog.SetDefault means every package elsewhere in
	// apps/processor-go that logs via the bare slog.InfoContext/WarnContext/ErrorContext package
	// functions (rather than holding a *slog.Logger of its own) picks up this exact handler — JSON vs
	// text, level, and the trace_id/span_id auto-injection — without any constructor threading it
	// through. This keeps every exported signature in this service unchanged (NewProcessorService,
	// NewDispatcher, NewEmailWorker, ... all keep their existing shape, which matters because
	// tests/integration constructs several of them directly and is off limits for this change).
	logger := obs.Setup("processor-go")
	slog.SetDefault(logger)

	providers, obsErr := obs.Bootstrap(ctx, logger, obs.ProvidersConfig{
		ServiceName:    "processor-go",
		ServiceVersion: getEnv("PROCESSOR_VERSION", ""),
	})
	if obsErr != nil {
		// obs.Bootstrap only returns an error for a malformed LOCAL configuration (e.g. bad TLS
		// material for the OTLP exporter) — never because the collector is unreachable; that failure
		// mode is async and already handled inside Bootstrap by a rate-limited warning (see
		// packages/shared-go/obs/provider.go's degradation mandate). A dead collector must never
		// prevent startup, and neither must this — strictly rarer — config error, so this falls back
		// to inert providers (metrics/traces off) rather than log.Fatalf.
		slog.ErrorContext(ctx, "observability bootstrap failed; continuing without tracing/metrics export",
			slog.String("error", obsErr.Error()))
		providers = &obs.Providers{
			MetricsHandler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
			Shutdown:       func(context.Context) error { return nil },
		}
	}
	// Flushed on SIGTERM below (bounded context) — during an incident the LAST spans emitted are the
	// interesting ones, so a missing flush here would defeat the whole point of this plan. Declared
	// this early (right after Bootstrap) so defer's LIFO order runs it AFTER every other resource in
	// this function has already been torn down (db, subscribers, health server), i.e. once nothing can
	// still be generating spans.
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		if err := providers.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(shutdownCtx, "observability providers shutdown reported an error", slog.String("error", err.Error()))
		}
	}()

	tracer := otel.Tracer("processor-go")

	dbCfg := database.Config{
		Host:            getEnv("POSTGRES_HOST", "localhost"),
		Port:            getEnvInt("POSTGRES_PORT", 5432),
		User:            getEnv("POSTGRES_USER", "sentinel"),
		Password:        getEnv("POSTGRES_PASSWORD", "changeme"),
		Database:        getEnv("POSTGRES_DB", "sentinel"),
		MaxConns:        25,
		MinConns:        5,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 10 * time.Minute,
	}

	// sentinel-issue-tag-facets spec §4.1: `processor backfill-tag-facets` counts the occurrences stored
	// before the rollup's triggers existed, run once per environment by an operator as a Job created from
	// the chart's suspended CronJob. It dispatches before the symbolication checks and before NATS, which
	// it does not need; it opens the same pgx pool and nothing else. Exit 2 means another runner holds the
	// lock (a mistaken second Job ends Failed); exit 1 any other failure; a re-run resumes from the cursor.
	if len(os.Args) > 1 && os.Args[1] == "backfill-tag-facets" {
		os.Exit(runTagFacetsBackfill(ctx, logger, dbCfg, os.Args[2:]))
	}

	// sentinel-sourcemaps §4.4 / §12: the six symbolication knobs, each in the syntax the contract
	// states beside its name (decision 27 — the operator turning one reads values.yaml and nothing
	// else): SYMBOLICATE_PLATFORMS a comma-separated lowercase list (`javascript,browser`; unset =
	// that default, set and EMPTY = the stage and Compute's normalisation off — the kill switch);
	// SOURCEMAP_CACHE_BYTES and SOURCEMAP_MAX_PARSE_BYTES plain integer bytes (`134217728`,
	// `52428800`; no Mi/M suffix); SOURCEMAP_NEGATIVE_TTL, SOURCEMAP_POSITIVE_TTL and
	// SYMBOLICATE_DEADLINE Go duration strings (`60s`, `10m`, `5s`). A value set in the wrong syntax
	// stops the process here rather than running with a default it did not ask for.
	symCfg, err := symbolicate.ParseConfig(os.LookupEnv)
	if err != nil {
		log.Fatalf("Invalid symbolication configuration: %v", err)
	}
	// Decision 32 (contract §8, §12, flow S8b): the startup fatal that stands in for the dormant
	// alert. With the stage on and S3_ACCESS_KEY / S3_SECRET_KEY absent this process must not
	// start — log.Fatalf, exactly as the NATS-subscriber construction failure below does, so the
	// pod CrashLoopBackOffs and never consumes an event, and a rollout that forgot the secret refs
	// stalls on the new ReplicaSet instead of replacing pods that work. It runs BEFORE the
	// database is dialled and BEFORE Subscribe: a config check at startup, never a per-event
	// fatal (D7 — storage unreachable at runtime with credentials present is `fetch_error`). The
	// last log line names both variables; that line and `kubectl rollout status` are the signal.
	// No /health or readiness condition is added (§8).
	s3Cfg := symbolicate.S3ConfigFromEnv(os.LookupEnv)
	if err := symbolicate.StartupCheck(symCfg, s3Cfg); err != nil {
		log.Fatalf("Refusing to start: %v", err)
	}

	db, err := database.NewConnection(ctx, dbCfg)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	natsCfg := nats.SubscriberConfig{
		URL:       getEnv("NATS_URL", "nats://localhost:4222"),
		Stream:    "ERROR_EVENTS",
		Subject:   "error_events",
		Consumer:  "processor-consumer",
		BatchSize: processorBatchSize,
		BatchWait: 1 * time.Second,
		// AckWait is set EXPLICITLY (sentinel-sourcemaps §4.4 / §12, decision 37): zero here meant
		// the server's 30 s, and one batch of processorBatchSize events may now spend up to
		// BatchSize × SYMBOLICATE_DEADLINE in the symbolication stage (each event's cold map is
		// bounded by the deadline, and a batch is handled sequentially) plus the write time — 10 ×
		// 5 s = 50 s at the defaults, already past the server default, so a batch still being
		// processed would be redelivered under it (Known limit 6's trigger: the redelivery lands
		// on a second issue — closed by sentinel-occurrence-dedup-project, which dedups the
		// occurrence on (project_id, event_id) so the second issue is never created).
		// symbolicate.AckWait derives it from the deadline the operator actually set — raising
		// SYMBOLICATE_DEADLINE in values.yaml moves this in code, not in the chart — and the
		// arithmetic is recorded in prd status/be.md.
		AckWait: symbolicate.AckWait(processorBatchSize, symCfg.Deadline),
		// MaxDeliver caps redelivery attempts so a single
		// permanently-unprocessable message cannot redeliver forever and
		// starve every subsequent event (VERIFIED_STATE.md S13). Exhausted
		// or explicitly-permanent failures are dead-lettered to
		// DLQSubject/DLQStream instead of looping.
		MaxDeliver: getEnvInt("PROCESSOR_MAX_DELIVER", 7), // ~8.5min recovery window; see retryBackoff
		DLQSubject: getEnv("PROCESSOR_DLQ_SUBJECT", "error_events.dlq"),
		DLQStream:  getEnv("PROCESSOR_DLQ_STREAM", "ERROR_EVENTS_DLQ"),
	}

	subscriber, err := nats.NewSubscriber(ctx, natsCfg)
	if err != nil {
		log.Fatalf("Failed to create NATS subscriber: %v", err)
	}
	defer subscriber.Close()

	registerDLQObservables(subscriber)

	// alert_config.changed lets alerts.Dispatcher reload alert_configs promptly after the dashboard
	// creates/updates/deletes a config, instead of waiting up to alerts.ConfigRefreshInterval() for the
	// backstop ticker (see alerts.Dispatcher.StartInvalidationSubscriber and E2E_RECOVERY_PLAN.md U27).
	//
	// Do NOT discard this error the way redisClient's is discarded elsewhere in this codebase (BUGS.md
	// S10) — mirrors apps/ingestor-go/main.go's api_key.invalidated wiring: an unavailable subscriber
	// must be loud, not silent. Unlike API-key invalidation, correctness here never rests on this
	// subscriber (the ticker backstop runs regardless), so the default is non-fatal.
	alertConfigSubCfg := nats.SubscriberConfig{
		URL:       getEnv("NATS_URL", "nats://localhost:4222"),
		Stream:    getEnv("ALERT_CONFIG_STREAM", "ALERT_CONFIG"),
		Subject:   "alert_config.changed",
		Consumer:  "processor_alert_config_changed",
		BatchSize: 10,
		BatchWait: 1 * time.Second,
	}
	alertConfigSub, alertConfigSubErr := nats.NewSubscriber(ctx, alertConfigSubCfg)
	if alertConfigSubErr != nil {
		if getEnv("ALERT_CONFIG_INVALIDATION_REQUIRED", "false") == "true" {
			log.Fatalf("Failed to subscribe to alert_config.changed (set ALERT_CONFIG_INVALIDATION_REQUIRED=false to start anyway, accepting up to the periodic backstop refresh interval for new/updated alert configs): %v", alertConfigSubErr)
		}
		slog.WarnContext(ctx, "alert_config.changed subscriber unavailable; alert config changes will take up to the periodic backstop to take effect",
			slog.String("error", alertConfigSubErr.Error()), slog.Duration("backstop_interval", alerts.ConfigRefreshInterval()))
		alertConfigSub = nil
	} else {
		defer alertConfigSub.Close()
	}

	proc := service.NewProcessorService(db)
	proc.Alerts().StartInvalidationSubscriber(alertConfigSub)

	// sentinel-span-ingest: the SPANS consumer (D2) and the core-trace pull worker (D3). Both are off unless
	// configured and neither is ever fatal: spans are a view over errors, so a missing stream or an unreachable
	// trace store is logged and the error pipeline runs on.
	startSpans(ctx, db, proc)

	// sentinel-sourcemaps §4.4: the symbolication stage, wired before Subscribe so no event is
	// consumed by a processor that would pass every frame through. The S3 client is built from
	// S3_ENDPOINT / S3_BUCKET / S3_ACCESS_KEY / S3_SECRET_KEY (§12 — the same four the dashboard
	// reads; TASK-01's chart and compose changes deliver them to this container). Only the two
	// credentials are decision 32's startup condition (checked above); an empty endpoint or
	// bucket is a construction error and also fatal here — with the stage on, a processor that
	// cannot address the bucket at all is the same rollout mistake. Storage that is addressable
	// but UNREACHABLE stays a per-map `fetch_error` at runtime (D7): NewS3Fetcher never dials.
	// With SYMBOLICATE_PLATFORMS set and empty none of this runs and no S3 variable is read.
	if symCfg.Enabled() {
		fetcher, err := symbolicate.NewS3Fetcher(s3Cfg, symCfg.MaxParseBytes)
		if err != nil {
			log.Fatalf("Failed to build the sourcemap storage client (%s/%s/%s/%s): %v",
				symbolicate.EnvS3Endpoint, symbolicate.EnvS3Bucket, symbolicate.EnvS3AccessKey, symbolicate.EnvS3SecretKey, err)
		}
		if err := proc.EnableSymbolication(symCfg, fetcher); err != nil {
			log.Fatalf("Failed to wire symbolication: %v", err)
		}
		defer proc.CloseSymbolication()
		slog.InfoContext(ctx, "Symbolication enabled",
			slog.Any("platforms", symCfg.Platforms), slog.Duration("deadline", symCfg.Deadline),
			slog.Int64("cache_bytes", symCfg.CacheBytes), slog.Int64("max_parse_bytes", symCfg.MaxParseBytes),
			slog.Duration("negative_ttl", symCfg.NegativeTTL), slog.Duration("positive_ttl", symCfg.PositiveTTL),
			slog.Duration("ack_wait", natsCfg.AckWait))
	} else {
		slog.InfoContext(ctx, "Symbolication disabled: SYMBOLICATE_PLATFORMS is set and empty; frames are stored as sent and fingerprint input normalisation is off")
	}

	if err := proc.VerifyAuditLogTable(ctx); err != nil {
		log.Fatalf("AUDIT_VERIFICATION_FAILED: audit_logs table is not writable: %v", err)
	}
	slog.InfoContext(ctx, "Audit log table verification passed")

	// Deliverable 3 (OBSERVABILITY_PLAN.md W2, the crux of the whole plan): extract whatever trace
	// context rode in on the NATS message's headers, then open a CONSUMER span parented on it. If this
	// extraction is wrong, every span started below (and every child span
	// service.ProcessEvent/processEventInternal opens) becomes a disconnected new root — every test
	// still passes, but the cross-service trace silently never exists. See the report for how this was
	// verified empirically (a span-recording exporter, not just a reading of this code).
	// subscribed gates /health: not-ready (503) until Subscribe succeeds, so a JetStream outage at startup
	// is retried in-process (subscribeWithRetry) rather than crash-looping the pod.
	var subscribed atomic.Bool

	// dlqDetailer is a second, read-only JetStream connection used only to enrich the DLQ health
	// signal with the age/class of the oldest parked message — data DLQStats does not expose and that
	// packages/shared-go/nats cannot be extended to provide within this change (owned by a parallel
	// change in flight; see dlqmonitor.JetStreamDetailer's doc comment). Best-effort like
	// alertConfigSub above: an unavailable detailer degrades /health and the DLQ monitor to
	// depth/publish-failures-only, it does not fail startup.
	dlqDetailer, dlqDetailerErr := dlqmonitor.NewJetStreamDetailer(natsCfg.URL)
	if dlqDetailerErr != nil {
		slog.WarnContext(ctx, "DLQ detail connection unavailable; /health will report dlq_depth/dlq_publish_failures only, without oldest-message age or class",
			slog.String("error", dlqDetailerErr.Error()))
		dlqDetailer = nil
	} else {
		defer dlqDetailer.Close()
	}

	dlqThresholds := dlqmonitor.ThresholdsFromEnv()

	healthSrv := serveHealth(ctx, getEnv("PROCESSOR_HEALTH_ADDR", ":8081"), db, subscriber, &subscribed, dlqDetailer, dlqThresholds, providers.MetricsHandler)
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = healthSrv.Shutdown(shutdownCtx)
	}()

	subscribeFn := func() error {
		return subscriber.Subscribe(ctx, func(msgCtx context.Context, data []byte, headers nats.Header) error {
			msgCtx = otel.GetTextMapPropagator().Extract(msgCtx, obs.NATSHeaderCarrier(headers))
			// messaging.* attributes mirror the ingestor's producer span (apps/ingestor-go/service/
			// service.go's publish) so a trace backend can group both sides of the hop by the same
			// destination — natsCfg.Subject is the compile-time-fixed literal this consumer was
			// constructed with above, not a per-message value, so there is no cardinality cost.
			msgCtx, span := tracer.Start(msgCtx, "processor.process_event",
				trace.WithSpanKind(trace.SpanKindConsumer),
				trace.WithAttributes(
					attribute.String("messaging.system", "nats"),
					attribute.String("messaging.destination.name", natsCfg.Subject),
				),
			)
			defer span.End()

			err := proc.ProcessEvent(msgCtx, data)
			// A symbolication retry is the design working (the delayed redelivery of sentinel-grouping-v2
			// §5.2), not a failure: it is tagged and its status left alone.
			if err != nil && !service.MarkSymbolicationRetry(span, err) {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
				// This span cannot know whether the subscriber will actually dead-letter this delivery:
				// that decision (packages/shared-go/nats.Subscriber.handleMessage, private, out of scope
				// here) also depends on the message's delivery count, which is not visible at this call
				// site — a transient error on the last allowed delivery dead-letters even though
				// nats.IsPermanent reports false for it (see processor_service.go's ProcessEvent doc
				// comment for the identical caveat on OutcomeRetried vs OutcomeDeadLettered). What IS
				// knowable here, and worth recording, is whether processing classified this failure as
				// permanent (nats.Permanent(...) was returned somewhere on the call path) — a permanent
				// error dead-letters unconditionally on this delivery regardless of count.
				span.SetAttributes(attribute.Bool("sentinel.error_permanent", nats.IsPermanent(err)))
			}
			return err
		})
	}
	if err := subscribeWithRetry(ctx, subscribeFn, &subscribed, subscribeRetryInitial, subscribeRetryMax); err != nil {
		log.Fatalf("Failed to subscribe: %v", err)
	}

	// The DLQ monitor is the "nothing watches that endpoint" fix: /health carries the signal for a
	// human or an external prober, this loop is the in-process watcher that pages through the existing
	// alert dispatcher on a Critical-severity transition (see dlqmonitor.Monitor.CheckOnce). Wiring an
	// AlertConfig is optional (nil when PROCESSOR_DLQ_ALERT_CHANNEL is unset) — the monitor still runs
	// and logs transitions either way, it just does not dispatch.
	dlqMonitor := &dlqmonitor.Monitor{
		Stats:       subscriber,
		Oldest:      dlqDetailer,
		Dispatcher:  proc.Alerts(),
		AlertConfig: alerts.OperationalAlertConfigFromEnv(),
		Thresholds:  dlqThresholds,
		Interval:    dlqmonitor.CheckIntervalFromEnv(),
	}
	go dlqMonitor.Run(ctx)

	// N3b: outbound webhook delivery dispatcher, off by default (WEBHOOK_DISPATCH_ENABLED unset
	// or not exactly "true") — see webhooks.EnabledFromEnv and webhooks/dispatcher.go's package
	// doc for the polling/CAS design. Uses the same *pgxpool.Pool as everything else in this
	// process; store.NewStore's pgStore satisfies store.WebhookStore.
	if webhooks.EnabledFromEnv() {
		webhookStore, ok := store.NewStore(db).(store.WebhookStore)
		if !ok {
			slog.ErrorContext(ctx, "webhooks: store.NewStore does not implement store.WebhookStore; dispatcher not started")
		} else {
			dispatcher := webhooks.NewDispatcher(webhookStore)
			slog.InfoContext(ctx, "webhooks: dispatch enabled",
				slog.Duration("interval", dispatcher.Interval), slog.Int("failure_threshold", dispatcher.FailureThreshold))
			go dispatcher.Run(ctx)
		}
	}

	slog.InfoContext(ctx, "Processor started, waiting for events...")

	go func() {
		for err := range subscriber.Errors() {
			slog.ErrorContext(ctx, "Subscriber error", slog.String("error", err.Error()))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	slog.InfoContext(ctx, "Shutting down processor...")
	cancel()
}

// processorBatchSize is the ERROR_EVENTS consumer's pull batch (nats.SubscriberConfig.BatchSize) —
// a named constant because symbolicate.AckWait's arithmetic is in terms of it (§12: "BatchSize
// is 10").
const processorBatchSize = 10

// startSpans wires sentinel-span-ingest's two processor halves. SPAN_INGEST_ENABLED=true starts the SPANS consumer
// (D2) and the partition keeper; SPAN_PULL_SCOPES + TRACE_STORE_KIND + TRACE_STORE_URL (all set) start the pull
// worker and let the processor enqueue traces (D3). A bad SPAN_PULL value is a configuration error and stops the
// process, like the symbolication knobs: a typo must not silently pull nothing.
func startSpans(ctx context.Context, db *pgxpool.Pool, proc *service.ProcessorService) {
	spanStore := spans.NewStore(db)
	ingestOn := getEnv("SPAN_INGEST_ENABLED", "false") == "true"
	pullCfg, err := spanpull.ParseConfig(os.LookupEnv)
	if err != nil {
		log.Fatalf("Invalid span pull configuration: %v", err)
	}
	if !ingestOn && !pullCfg.Enabled() {
		slog.InfoContext(ctx, "Span ingest and span pull are off")
		return
	}

	// The partition window is also kept by the dashboard's hourly retention cron; doing it here too means a stalled
	// cron cannot leave today without a partition (spans outside the window are dropped, D1).
	keepPartitions := func() {
		if n, err := spanStore.EnsurePartitions(ctx); err != nil {
			slog.WarnContext(ctx, "spans: ensure partitions failed", slog.String("error", err.Error()))
		} else if n > 0 {
			slog.InfoContext(ctx, "spans: created day partitions", slog.Int("created", n))
		}
	}
	keepPartitions()
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				keepPartitions()
			}
		}
	}()

	if ingestOn {
		sub, err := nats.NewSubscriber(ctx, nats.SubscriberConfig{
			URL:       getEnv("NATS_URL", "nats://localhost:4222"),
			Stream:    spanwire.Stream,
			Subject:   spanwire.Subject,
			Consumer:  "processor-spans",
			BatchSize: 50,
			BatchWait: 1 * time.Second,
			// A database outage redelivers with the subscriber's backoff (~8.5 min window at 7); what still fails is
			// parked in a bounded SPANS_DLQ (scripts/nats-init.sh), never in the error DLQ whose depth pages an operator.
			MaxDeliver: 7,
			DLQSubject: "spans.dlq",
			DLQStream:  "SPANS_DLQ",
		})
		if err != nil {
			slog.ErrorContext(ctx, "spans: could not connect the SPANS consumer; mobile spans will not be stored", slog.String("error", err.Error()))
		} else {
			consumer := &spans.Consumer{Store: spanStore}
			if err := sub.Subscribe(ctx, func(msgCtx context.Context, data []byte, _ nats.Header) error {
				return consumer.Handle(msgCtx, data)
			}); err != nil {
				slog.ErrorContext(ctx, "spans: could not subscribe to SPANS (is the stream created?); mobile spans will not be stored", slog.String("error", err.Error()))
				sub.Close()
			}
		}
	}

	if pullCfg.Enabled() {
		traces, err := spanpull.NewTraceStore(pullCfg, &http.Client{})
		if err != nil {
			log.Fatalf("Invalid span pull configuration: %v", err)
		}
		// Nothing is enqueued or pulled until every scope has been checked against the projects table.
		go pullCfg.RunValidation(ctx, db, 15*time.Second)
		queue := spanpull.NewQueue(db, pullCfg)
		proc.SetSpanPull(queue)
		go (&spanpull.Worker{Queue: queue, Spans: spanStore, Traces: traces, Cfg: pullCfg}).Run(ctx)
		slog.InfoContext(ctx, "Span pull enabled", slog.String("store_kind", pullCfg.Kind), slog.Int("orgs", len(pullCfg.Scopes)),
			slog.String("issue_window", pullCfg.IssueWindow.String()))
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return defaultValue
}

// registerDLQObservables wires MetricDLQDepth (a gauge — depth can go back down as tools/dlq drains
// it) and MetricDLQPublishFailures (a monotonic counter) as OTel observable instruments, read on
// every /metrics scrape directly from sub.DLQStats — the SAME source /health already reads (deliverable
// 4: "do not invent a second source of truth for those numbers"). Both instruments share one callback
// so a scrape costs exactly one DLQStats call, not two. Failure to create the instruments, or a failed
// DLQStats call inside the callback, degrades to "this scrape omits these two series" — it must never
// fail metric collection for anything else, and it never touches the request/message path.
func registerDLQObservables(sub *nats.Subscriber) {
	meter := otel.Meter("processor-go")

	depthGauge, err := meter.Int64ObservableGauge(
		obs.MetricDLQDepth,
		metric.WithDescription("Current depth of the dead-letter queue"),
	)
	if err != nil {
		slog.Error("failed to create DLQ depth gauge", slog.String("error", err.Error()))
		return
	}

	publishFailuresCounter, err := meter.Int64ObservableCounter(
		obs.MetricDLQPublishFailures,
		metric.WithDescription("Cumulative count of events that could not be captured in the DLQ and were left in the source stream instead"),
	)
	if err != nil {
		slog.Error("failed to create DLQ publish failures counter", slog.String("error", err.Error()))
		return
	}

	_, err = meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		stats, statsErr := sub.DLQStats(ctx)
		if statsErr != nil {
			// Best-effort: skip this scrape's observation for these two instruments rather than
			// failing collection of every other metric registered on this meter.
			return nil
		}
		o.ObserveInt64(depthGauge, int64(stats.Depth))
		o.ObserveInt64(publishFailuresCounter, int64(stats.PublishFailures))
		return nil
	}, depthGauge, publishFailuresCounter)
	if err != nil {
		slog.Error("failed to register DLQ observable callback", slog.String("error", err.Error()))
	}
}

// serveHealth exposes the processor's liveness and, critically, DLQ depth.
//
// Until now the processor had NO http surface at all, and Subscriber.DLQStats had ZERO call sites —
// implemented, correct, and unreachable, which is bug pattern B3 (docs/memory/BUGS.md) and exactly
// what this project keeps shipping. DECISIONS.md D10 records the consequence plainly: dead-lettered
// events are "preserved but unprocessed... invisible to the product". A parked event nobody can see
// is indistinguishable from a lost one.
//
// dlq_depth > 0 means events are sitting unprocessed and someone must look; replay them with
// `go run ./tools/dlq`. dlq_publish_failures > 0 is worse: those events could not even be captured
// in the DLQ and are parked in the source stream instead.
//
// status now distinguishes healthy/attention/critical (dlqmonitor.Classify) instead of flipping to
// "attention" on the first dead-lettered message: a single poison message is normal operation and stays
// "attention", not "critical" — the same classification the in-process DLQ monitor uses to decide
// whether to page (see main()'s dlqMonitor and dlqmonitor.Monitor.CheckOnce). dlq_threshold and
// dlq_stale_after_seconds are the configured thresholds behind that decision, always reported so the
// status string is never a mystery. dlq_oldest_age_seconds/dlq_oldest_class are best-effort — present
// only when detailer is non-nil, depth > 0, and the underlying JetStream calls succeed; see
// dlqmonitor.JetStreamDetailer for why a full permanent/transient breakdown across the whole backlog is
// deliberately not attempted here.
//
// Existing field names (dlq_depth, dlq_publish_failures, dlq_stream, status, database) are unchanged —
// tests/e2e decodes this body. Only fields are added.
//
// metricsHandler is mounted at /metrics alongside /health (OBSERVABILITY_PLAN.md §2/W2) — additive
// only, /health's own JSON contract is untouched above.
func serveHealth(ctx context.Context, addr string, db *pgxpool.Pool, sub *nats.Subscriber, subscribed *atomic.Bool, detailer dlqmonitor.OldestMessageSource, thresholds dlqmonitor.Thresholds, metricsHandler http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", livezHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		statusCode := http.StatusOK
		body := map[string]any{"status": "healthy"}

		reqCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if !subscribed.Load() {
			statusCode = http.StatusServiceUnavailable
			body["status"] = "starting"
			body["subscription"] = "not ready"
		}

		if err := db.Ping(reqCtx); err != nil {
			statusCode = http.StatusServiceUnavailable
			body["status"] = "degraded"
			body["database"] = err.Error()
		} else {
			body["database"] = "ok"
		}

		if sub != nil {
			body["dlq_threshold"] = thresholds.Depth
			body["dlq_stale_after_seconds"] = thresholds.CriticalAge.Seconds()

			detail, err := dlqmonitor.GetDetail(reqCtx, sub, detailer)
			if err != nil {
				body["dlq_error"] = err.Error()
			} else {
				body["dlq_stream"] = detail.Stats.Stream
				body["dlq_depth"] = detail.Stats.Depth
				body["dlq_publish_failures"] = detail.Stats.PublishFailures
				if detail.HasOldestAge {
					body["dlq_oldest_age_seconds"] = detail.OldestAge.Seconds()
				}

				// D47 revisited: dlq_oldest_class IS reported, deliberately. It was briefly removed
				// on the reasoning that it is the one field here derived from a customer event's
				// payload, and so a narrow leak on an unauthenticated port. tests/e2e U34
				// (dlq_test.go) rejects that: without the oldest message's class, a backlog of
				// PERMANENTLY dead messages — which will never drain on their own — looks identical
				// to a transient one that clears when the database recovers. That distinction is the
				// difference between paging someone and waiting, so the operational value wins.
				// The port-exposure question is handled where it belongs, in docker-compose.yml.
				if detail.OldestClass != "" {
					body["dlq_oldest_class"] = detail.OldestClass
				}

				// Parked events are not an outage, so this stays 200 — but it must be visible to
				// whatever scrapes this endpoint rather than buried in a log nobody reads.
				_, statusMsg := dlqmonitor.Classify(detail, thresholds)
				if statusMsg != dlqmonitor.StatusHealthy {
					body["status"] = statusMsg
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		_ = json.NewEncoder(w).Encode(body)
	})

	if metricsHandler != nil {
		mux.Handle("/metrics", metricsHandler)
	}

	mux.HandleFunc("/dlq", func(w http.ResponseWriter, r *http.Request) {
		reqCtx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		w.Header().Set("Content-Type", "application/json")

		// D12: this endpoint used to include a synthetic "items" array — at most one entry with
		// hardcoded sequence/event_id/org_id/project_id/retry_attempts and a hand-concatenated
		// raw_payload string embedding detail.Stats.Stream unescaped. That was fabricated data, not a
		// sample of the real backlog, and an operator reading it during an incident would mistake it
		// for a real parked event. Fetching real parked messages would require a non-destructive
		// JetStream peek (e.g. an ephemeral pull consumer with explicit ack policy) and would then
		// carry real tenant payloads across this endpoint, which is unauthenticated today and
		// interacts with the access decision made in P1-1 (the dashboard's observability page
		// withholds any DLQ item view entirely for exactly that cross-tenant reason). Per the plan,
		// option (b): the aggregates below stay real; "items" is removed rather than shipping a
		// placeholder shape.
		if sub == nil {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_depth": 0,
			})
			return
		}

		detail, err := dlqmonitor.GetDetail(reqCtx, sub, detailer)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": err.Error(),
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(dlqmonitor.BuildDLQResponse(detail))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.InfoContext(ctx, "Processor health endpoint listening", slog.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.ErrorContext(ctx, "Health endpoint failed", slog.String("error", err.Error()))
		}
	}()
	return srv
}

// backfillFlags is the backfill-tag-facets flag set, split out so a test can parse it directly and pin
// the flag names the chart's rendered CronJob args depend on (tfb-helm-backfill-test-weak,
// tfb-backfill-safety-controls-unproven: renaming -duty must fail a test, not just the chart at deploy).
func backfillFlags() (fs *flag.FlagSet, batch *int, pause *time.Duration, duty *float64) {
	fs = flag.NewFlagSet("backfill-tag-facets", flag.ContinueOnError)
	batch = fs.Int("batch", tagfacets.DefaultBatch, "rows per batch transaction (1-5000)")
	pause = fs.Duration("pause", tagfacets.DefaultPause, "minimum sleep between batches")
	duty = fs.Float64("duty", tagfacets.DefaultDuty, "maximum fraction of time a batch holds its issues, in (0, 1]")
	return fs, batch, pause, duty
}

// backfillExitCode maps a tagfacets.Run outcome to the process exit code, split out so a test can pin it
// without a database: ErrAlreadyRunning must map to 2 (not 0), which the deploy Job's Backoff/Complete
// semantics depend on (main's own comment: "so a mistaken second Job ends Failed rather than Complete").
func backfillExitCode(err error) int {
	switch {
	case errors.Is(err, tagfacets.ErrAlreadyRunning):
		return 2
	case err != nil:
		return 1
	default:
		return 0
	}
}

// runTagFacetsBackfill is the `backfill-tag-facets` subcommand (sentinel-issue-tag-facets spec §4.1-§4.2).
func runTagFacetsBackfill(parent context.Context, logger *slog.Logger, dbCfg database.Config, args []string) int {
	fs, batch, pause, duty := backfillFlags()
	if err := fs.Parse(args); err != nil {
		return 1
	}
	ctx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	dbCfg.MaxConns, dbCfg.MinConns = 2, 1
	pool, err := database.NewConnection(ctx, dbCfg)
	if err != nil {
		logger.ErrorContext(ctx, "tag facets backfill: database connection failed",
			slog.String("event", "tag_facets.backfill.failed"), slog.String("error", err.Error()))
		return 1
	}
	defer pool.Close()
	_, err = tagfacets.Run(ctx, pool, tagfacets.Options{Batch: *batch, Pause: *pause, Duty: *duty, Logger: logger})
	if err != nil && !errors.Is(err, tagfacets.ErrAlreadyRunning) {
		logger.ErrorContext(ctx, "tag facets backfill failed",
			slog.String("event", "tag_facets.backfill.failed"), slog.String("error", err.Error()))
	}
	return backfillExitCode(err)
}

// livezHandler is the liveness probe: 200 while the process serves HTTP, with no DB/NATS checks, so a
// dependency outage (e.g. JetStream retry at startup, when /health is 503) never gets the pod killed.
func livezHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"alive"}`))
}
