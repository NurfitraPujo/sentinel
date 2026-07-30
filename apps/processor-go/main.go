package main

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5/pgxpool"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/dlqmonitor"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/database"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
		log.Printf("WARNING: alert_config.changed subscriber unavailable (%v). Alert config changes will take up to %s (the periodic backstop) to take effect.", alertConfigSubErr, alerts.ConfigRefreshInterval())
		alertConfigSub = nil
	} else {
		defer alertConfigSub.Close()
	}

	proc := service.NewProcessorService(db)
	proc.Alerts().StartInvalidationSubscriber(alertConfigSub)

	if err := proc.VerifyAuditLogTable(ctx); err != nil {
		log.Fatalf("AUDIT_VERIFICATION_FAILED: audit_logs table is not writable: %v", err)
	}
	log.Println("Audit log table verification passed")

	err = subscriber.Subscribe(ctx, func(data []byte) error {
		return proc.ProcessEvent(ctx, data)
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
		log.Printf("WARNING: DLQ detail connection unavailable (%v). /health will report dlq_depth/dlq_publish_failures only, without oldest-message age or class.", dlqDetailerErr)
		dlqDetailer = nil
	} else {
		defer dlqDetailer.Close()
	}

	dlqThresholds := dlqmonitor.ThresholdsFromEnv()

	healthSrv := serveHealth(ctx, getEnv("PROCESSOR_HEALTH_ADDR", ":8081"), db, subscriber, dlqDetailer, dlqThresholds)
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

	log.Println("Processor started, waiting for events...")

	go func() {
		for err := range subscriber.Errors() {
			log.Printf("Subscriber error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down processor...")
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
func serveHealth(ctx context.Context, addr string, db *pgxpool.Pool, sub *nats.Subscriber, detailer dlqmonitor.OldestMessageSource, thresholds dlqmonitor.Thresholds) *http.Server {
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

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		log.Printf("Processor health endpoint listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Health endpoint failed: %v", err)
		}
	}()
	return srv
}
