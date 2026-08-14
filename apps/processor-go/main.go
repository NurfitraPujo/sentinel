package main

import (
	"context"
	"encoding/json"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/dlqmonitor"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/webhooks"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/database"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
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
		BatchSize: 10,
		BatchWait: 1 * time.Second,
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
	err = subscriber.Subscribe(ctx, func(msgCtx context.Context, data []byte, headers nats.Header) error {
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
		if err != nil {
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
	if err != nil {
		log.Fatalf("Failed to subscribe: %v", err)
	}

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

	healthSrv := serveHealth(ctx, getEnv("PROCESSOR_HEALTH_ADDR", ":8081"), db, subscriber, dlqDetailer, dlqThresholds, providers.MetricsHandler)
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = healthSrv.Shutdown(shutdownCtx)
	}()

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
func serveHealth(ctx context.Context, addr string, db *pgxpool.Pool, sub *nats.Subscriber, detailer dlqmonitor.OldestMessageSource, thresholds dlqmonitor.Thresholds, metricsHandler http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		statusCode := http.StatusOK
		body := map[string]any{"status": "healthy"}

		reqCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

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
