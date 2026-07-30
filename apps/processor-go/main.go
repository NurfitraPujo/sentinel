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

	healthSrv := serveHealth(ctx, getEnv("PROCESSOR_HEALTH_ADDR", ":8081"), db, subscriber)
	defer func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = healthSrv.Shutdown(shutdownCtx)
	}()

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
func serveHealth(ctx context.Context, addr string, db *pgxpool.Pool, sub *nats.Subscriber) *http.Server {
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
			stats, err := sub.DLQStats(reqCtx)
			if err != nil {
				body["dlq_error"] = err.Error()
			} else {
				body["dlq_stream"] = stats.Stream
				body["dlq_depth"] = stats.Depth
				body["dlq_publish_failures"] = stats.PublishFailures
				// Parked events are not an outage, so this stays 200 — but it must be visible to
				// whatever scrapes this endpoint rather than buried in a log nobody reads.
				if stats.Depth > 0 || stats.PublishFailures > 0 {
					body["status"] = "attention: dead-lettered events awaiting replay (see tools/dlq)"
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
