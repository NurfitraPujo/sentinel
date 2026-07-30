package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const (
	// maxEventBodyBytes caps a single-event request body. The ingestor is the
	// only externally exposed service (see docs/plans/E2E_RECOVERY_PLAN.md
	// P2-5 / P8-1); an unbounded body is a trivial DoS vector even for the
	// single-event path.
	maxEventBodyBytes = 1 << 20 // 1 MiB

	// maxBatchBodyBytes caps a batch request body. Sized generously above
	// maxBatchSize * a realistic single-event payload.
	maxBatchBodyBytes = 5 << 20 // 5 MiB

	// maxBatchSize caps the number of items accepted per batch request
	// (VERIFIED_STATE.md S3/P2-5: handleBatchIngest previously had no cap at
	// all). 500 is the plan's suggested value.
	maxBatchSize = 500
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

	// Do NOT discard this error (VERIFIED_STATE.md S10 / docs/plans/E2E_RECOVERY_PLAN.md P3-3). A
	// discarded error here leaves redisClient == nil, which the rate limiter middleware used to treat
	// as an unconditional "let every request through" — fail-open by accident, not by decision, for the
	// life of the process. An unreachable Redis at boot is now fatal unless an operator explicitly opts
	// out via RATELIMIT_ALLOW_NO_REDIS=true, and that opt-out is logged loudly rather than silently
	// taken.
	redisClient, redisErr := redis.NewClient(ctx, redisCfg)
	if redisErr != nil {
		if getEnv("RATELIMIT_ALLOW_NO_REDIS", "false") == "true" {
			log.Printf("WARNING: Redis unreachable at boot (%v); continuing with rate limiting DISABLED "+
				"and the API-key cache DISABLED because RATELIMIT_ALLOW_NO_REDIS=true was set explicitly. "+
				"This is a deliberate opt-out, not a default — unset RATELIMIT_ALLOW_NO_REDIS to refuse to "+
				"start instead.", redisErr)
			redisClient = nil
		} else {
			log.Fatalf("Redis unreachable at boot (%v); refusing to start with rate limiting silently "+
				"disabled. Set RATELIMIT_ALLOW_NO_REDIS=true to start anyway with rate limiting explicitly "+
				"disabled (an opt-out, not a default).", redisErr)
		}
	}

	subCfg := nats.SubscriberConfig{
		URL:       getEnv("NATS_URL", "nats://localhost:4222"),
		Subject:   "api_key.invalidated",
		Consumer:  "ingestor_apikey_invalidated",
		Stream:    "API_KEYS", // Assuming there's a stream for it
		BatchSize: 10,
	}
	// Do NOT discard this error. NewAPIKeyAuthenticator skips its invalidation goroutine when sub is
	// nil, so a NATS hiccup at startup used to leave revocation permanently dead — with no log line,
	// for the life of the process. Revocation would then silently degrade from "immediate" to "up to
	// APIKEY_CACHE_TTL", which is exactly the guarantee spec 008 and WORKLOG.md advertise as instant.
	subscriber, subErr := nats.NewSubscriber(ctx, subCfg)
	if subErr != nil {
		if getEnv("APIKEY_INVALIDATION_REQUIRED", "true") == "true" {
			log.Fatalf("Failed to subscribe to API key invalidation (set APIKEY_INVALIDATION_REQUIRED=false to start anyway, accepting up to APIKEY_CACHE_TTL revocation latency): %v", subErr)
		}
		log.Printf("WARNING: API key invalidation subscriber unavailable (%v). Revocation will take up to %s to take effect.", subErr, auth.CacheTTLForLogging())
		subscriber = nil
	}

	rateLimiter := middleware.NewRateLimiter(redisClient)
	authenticator := auth.NewAPIKeyAuthenticator(db, redisClient, subscriber)

	ingestHandler := authenticator.Middleware(
		rateLimiter.Middleware(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handleIngest(ingestService, db).ServeHTTP(w, r)
			}),
		),
	)
	batchIngestHandler := authenticator.Middleware(
		rateLimiter.Middleware(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handleBatchIngest(ingestService, db).ServeHTTP(w, r)
			}),
		),
	)
	http.Handle("/ingest", ingestHandler)
	http.Handle("/ingest/batch", batchIngestHandler)
	http.HandleFunc("/health", handleHealth(db))

	addr := getEnv("INGESTOR_ADDR", "")
	if addr == "" {
		// PORT is the conventional single env var (Heroku-style); INGESTOR_ADDR above wins if both are
		// set, since it can also bind a specific host. Default stays ":8080" unchanged either way, so a
		// deployment that sets neither sees no behavior change (docs/plans/E2E_RECOVERY_PLAN.md P3-3 —
		// this only exists so a second, standalone ingestor can be started alongside the compose one for
		// testing without a port clash).
		addr = ":" + getEnv("PORT", "8080")
	}

	srv := &http.Server{
		Addr:    addr,
		Handler: nil,
	}

	go func() {
		log.Printf("Starting ingestor on %s", addr)
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

// batchItemError names a single failed item in a batch ingest response by its
// index in the request payload, so a caller can tell total failure from
// partial or full success (VERIFIED_STATE.md S3/P2-5: previously the batch
// endpoint always returned 202 even when every item failed).
type batchItemError struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

// batchResult is the /ingest/batch response body. The single-event /ingest
// endpoint keeps its pre-existing {"status":"accepted"} response shape
// (docs/sdk-specification.md section 4) — only the batch endpoint's
// all-or-nothing 202 semantics were in scope for this fix (P2-5).
type batchResult struct {
	Ingested int              `json:"ingested"`
	Failed   int              `json:"failed"`
	Errors   []batchItemError `json:"errors,omitempty"`
}

func handleIngest(svc *service.IngestService, db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxEventBodyBytes)

		var payload validation.ErrorPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			if isBodyTooLarge(err) {
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}

		if err := applyAuthenticatedScope(r.Context(), db, &payload); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
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

func handleBatchIngest(svc *service.IngestService, db *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBatchBodyBytes)

		var payloads []validation.ErrorPayload
		if err := json.NewDecoder(r.Body).Decode(&payloads); err != nil {
			if isBodyTooLarge(err) {
				http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Invalid JSON payload array", http.StatusBadRequest)
			return
		}

		if len(payloads) == 0 {
			http.Error(w, "Empty payload batch", http.StatusBadRequest)
			return
		}

		if len(payloads) > maxBatchSize {
			http.Error(w, fmt.Sprintf("Batch too large: %d items exceeds max of %d", len(payloads), maxBatchSize), http.StatusRequestEntityTooLarge)
			return
		}

		result := batchResult{}
		for i := range payloads {
			if err := applyAuthenticatedScope(r.Context(), db, &payloads[i]); err != nil {
				result.Failed++
				result.Errors = append(result.Errors, batchItemError{Index: i, Message: err.Error()})
				continue
			}
			if err := svc.Ingest(r.Context(), &payloads[i]); err != nil {
				log.Printf("Failed to ingest batch item %d: %v", i, err)
				result.Failed++
				result.Errors = append(result.Errors, batchItemError{Index: i, Message: err.Error()})
				continue
			}
			result.Ingested++
		}

		// A batch response is only 2xx if at least one item made it through;
		// total failure must be distinguishable from success by status code
		// alone, not just by inspecting the body (VERIFIED_STATE.md S3/P2-5).
		status := http.StatusAccepted
		if result.Ingested == 0 {
			status = http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(result)
	}
}

// isBodyTooLarge detects the sentinel error http.MaxBytesReader produces once
// its limit is exceeded, so callers can respond 413 instead of the generic
// 400 an ordinary JSON decode error gets.
func isBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return true
	}
	// Fallback for older Go toolchains where http.MaxBytesReader returned an
	// unwrapped, unexported error type with this exact message.
	return strings.Contains(err.Error(), "http: request body too large")
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

// applyAuthenticatedScope forces a payload's tenancy onto the identity resolved from the presented
// API key, and rejects a body that names a different project.
//
// Two shapes are permitted, per spec 008:
//
//   - PROJECT-SCOPED key: the project is fixed by the credential. The body may omit project_key, or
//     may name the same project; naming a DIFFERENT one is 403 (VERIFIED_STATE.md S6 — a valid key
//     could otherwise write into any tenant's project by naming it).
//   - ORGANIZATION-WIDE key: the credential fixes only the organization, so the caller selects the
//     target project by name — via the X-Project-Key header (resolved in the auth middleware) or via
//     the body's project_key, resolved here. Either way resolution is scoped to the key's own
//     organization, so a name belonging to another tenant is 403, not a cross-tenant write.
//
// project_key is a project's unique NAME, not a credential; the secret only ever travels in the
// X-API-Key header. A mismatch is a 403 rather than a silent overwrite because a client naming the
// wrong project is misconfigured, and quietly rewriting its events hides that from both sides.
func applyAuthenticatedScope(ctx context.Context, db *pgxpool.Pool, payload *validation.ErrorPayload) error {
	authProjectKey, haveKey := auth.ProjectKeyFromContext(ctx)
	authProjectID, haveID := auth.ProjectIDFromContext(ctx)
	orgID, haveOrg := auth.OrganizationIDFromContext(ctx)

	if !haveOrg {
		// Middleware always populates this; reaching here means a handler was mounted without it.
		return fmt.Errorf("request is not authenticated")
	}

	if !haveKey && !haveID {
		// Organization-wide key that named no project in the header — the body must name one.
		if payload.ProjectKey == "" {
			return fmt.Errorf("project_key is required for an organization-wide API key")
		}
		resolvedID, err := auth.ResolveProjectInOrg(ctx, db, payload.ProjectKey, orgID)
		if err != nil {
			return fmt.Errorf("project %q not found in this organization", payload.ProjectKey)
		}
		payload.ProjectID = resolvedID
		return nil
	}

	// Org-wide key that already resolved a target from the X-Project-Key header: the header IS the
	// target, so the body's project_key is redundant and simply ignored. Header and body are two
	// spellings of one parameter (spec 008); when both are present the header wins and the body is
	// never consulted. This is not a tenancy hole — the header was itself resolved within the
	// authenticated organization by the middleware.
	//
	// A PROJECT-scoped key is different: its project is fixed by the credential, so a body naming a
	// different project is a misconfigured client and is rejected rather than silently rewritten
	// (VERIFIED_STATE.md S6).
	isOrgWideKey := auth.IsOrgWideKey(ctx)
	if payload.ProjectKey != "" && haveKey && payload.ProjectKey != authProjectKey && !isOrgWideKey {
		return fmt.Errorf("project_key %q does not match the authenticated project", payload.ProjectKey)
	}
	if payload.ProjectID != "" && haveID && payload.ProjectID != authProjectID {
		return fmt.Errorf("project_id %q does not match the authenticated project", payload.ProjectID)
	}

	payload.ProjectKey = authProjectKey
	payload.ProjectID = authProjectID
	return nil
}
