package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/database"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/redis"
	"github.com/jackc/pgx/v5/pgxpool"
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
		MaxConns:        10,
		MinConns:        2,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 10 * time.Minute,
	}

	// Retry database connection with timeout
	connCtx, connCancel := context.WithTimeout(ctx, 30*time.Second)
	defer connCancel()

	var db *pgxpool.Pool
	var err error
	for {
		db, err = database.NewConnection(connCtx, dbCfg)
		if err == nil {
			break
		}
		select {
		case <-connCtx.Done():
			log.Fatalf("Failed to connect to database after 30s timeout: %v", err)
		case <-time.After(500 * time.Millisecond):
			log.Printf("Retrying database connection: %v", err)
		}
	}
	defer db.Close()

	natsCfg := nats.PublisherConfig{
		URL:     getEnv("NATS_URL", "nats://localhost:4222"),
		Subject: "error_events",
		Timeout: 5 * time.Second,
	}

	publisher, err := nats.NewPublisher(ctx, natsCfg)
	if err != nil {
		log.Fatalf("Failed to create NATS publisher: %v", err)
	}
	defer publisher.Close()

	ingestService, err := service.NewIngestService(publisher)
	if err != nil {
		log.Fatalf("Failed to create ingest service: %v", err)
	}

	redisCfg := redis.Config{
		Addr:     getEnv("REDIS_ADDR", "localhost:6379"),
		Password: getEnv("REDIS_PASSWORD", ""),
		DB:       getEnvInt("REDIS_DB", 0),
	}

	redisClient, _ := redis.NewClient(ctx, redisCfg)
	rateLimiter := middleware.NewRateLimiter(redisClient, 5000, time.Minute)
	ingestHandler := auth.NewAPIKeyAuthenticator(db).Middleware(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleIngest(ingestService).ServeHTTP(w, r)
		}),
	)
	http.Handle("/ingest", rateLimiter.Middleware(ingestHandler))
	http.HandleFunc("/health", handleHealth(db))

	srv := &http.Server{
		Addr:    ":8080",
		Handler: nil,
	}

	go func() {
		log.Println("Starting ingestor on :8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
}

func handleIngest(svc *service.IngestService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var payload validation.ErrorPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}

		if err := svc.Ingest(r.Context(), &payload); err != nil {
			log.Printf("Failed to ingest error: %v", err)
			if strings.Contains(err.Error(), "validation failed") {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "Failed to ingest error", http.StatusInternalServerError)
			return
		}

		log.Printf("Successfully ingested error: project=%s, class=%s", payload.ProjectKey, payload.ErrorClass)
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
	}
}

func handleHealth(db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := db.Ping(r.Context()); err != nil {
			http.Error(w, "Database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
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
