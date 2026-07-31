package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/redis"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
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

	// obs.Setup builds the process-wide structured logger (JSON by default, LOG_FORMAT=text locally;
	// every line carries service="ingestor-go" and trace_id/span_id once a span is in scope). Set as
	// the slog default too, so packages that log without threading this value through (e.g. a helper
	// deep in apps/ingestor-go/auth) still get the same formatting rather than falling back to
	// log/slog's unconfigured default handler (docs/plans/OBSERVABILITY_PLAN.md D-a).
	logger := obs.Setup("ingestor-go")
	slog.SetDefault(logger)

	// obs.Bootstrap wires OpenTelemetry traces+metrics (OTLP/HTTP exporter, Prometheus scrape
	// endpoint) and registers them as the process-wide otel globals. Degradation is load-bearing here,
	// not an afterthought: Bootstrap never blocks on, or fails because of, an unreachable collector —
	// the only way it returns an error is a local misconfiguration (e.g. bad TLS material), which is
	// rare and, per the plan's degradation mandate, still must not prevent the ingestor from serving
	// traffic. So a Bootstrap error is logged and the process continues with tracing/metrics degraded
	// to the otel package's own no-op globals (providers stays nil; every use below is nil-checked)
	// rather than being escalated into a log.Fatalf that would turn an observability hiccup into a
	// full outage.
	providers, obsErr := obs.Bootstrap(ctx, logger, obs.ProvidersConfig{ServiceName: "ingestor-go"})
	if obsErr != nil {
		logger.Error("obs: failed to bootstrap tracing/metrics providers; continuing without them "+
			"(a dead collector or misconfiguration must never block startup)",
			slog.String("error", obsErr.Error()), slog.String(obs.LogKeyEvent, "obs.bootstrap_failed"))
	}

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
			// Message text kept verbatim (fmt.Sprintf reproduces exactly what log.Fatalf would have
			// printed); structure is added via the attr, not by rewording
			// (docs/plans/OBSERVABILITY_PLAN.md §4). This is a boot-time fatal: nothing has served a
			// request yet, so os.Exit(1) after logging is equivalent to log.Fatalf and is not the
			// "dead collector must never be fatal" case obs.Bootstrap governs above.
			logger.ErrorContext(ctx, fmt.Sprintf("Failed to connect to database after 30s timeout: %v", err),
				slog.Any("error", err))
			os.Exit(1)
		case <-time.After(500 * time.Millisecond):
			logger.WarnContext(ctx, fmt.Sprintf("Retrying database connection: %v", err), slog.Any("error", err))
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
		logger.ErrorContext(ctx, fmt.Sprintf("Failed to create NATS publisher: %v", err), slog.Any("error", err))
		os.Exit(1)
	}
	defer publisher.Close()

	ingestService, err := service.NewIngestService(publisher)
	if err != nil {
		logger.ErrorContext(ctx, fmt.Sprintf("Failed to create ingest service: %v", err), slog.Any("error", err))
		os.Exit(1)
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
			// tests/e2e/ratelimit_test.go's U20 greps this service's stdout for the substrings
			// "refus", "disab", "unreachable" (case-insensitively) to prove this decision was actually
			// logged, not silently taken. The message text below is reproduced verbatim via
			// fmt.Sprintf — do not reword it (docs/plans/OBSERVABILITY_PLAN.md §4); the slog attrs are
			// additive structure only.
			msg := fmt.Sprintf("WARNING: Redis unreachable at boot (%v); continuing with rate limiting DISABLED "+
				"and the API-key cache DISABLED because RATELIMIT_ALLOW_NO_REDIS=true was set explicitly. "+
				"This is a deliberate opt-out, not a default — unset RATELIMIT_ALLOW_NO_REDIS to refuse to "+
				"start instead.", redisErr)
			logger.WarnContext(ctx, msg, slog.Any("error", redisErr), slog.String(obs.LogKeyEvent, "redis.boot_optout"))
			redisClient = nil
		} else {
			// Same U20 grep constraint as above applies to this line.
			msg := fmt.Sprintf("Redis unreachable at boot (%v); refusing to start with rate limiting silently "+
				"disabled. Set RATELIMIT_ALLOW_NO_REDIS=true to start anyway with rate limiting explicitly "+
				"disabled (an opt-out, not a default).", redisErr)
			logger.ErrorContext(ctx, msg, slog.Any("error", redisErr), slog.String(obs.LogKeyEvent, "redis.boot_refused"))
			os.Exit(1)
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
			msg := fmt.Sprintf("Failed to subscribe to API key invalidation (set APIKEY_INVALIDATION_REQUIRED=false "+
				"to start anyway, accepting up to APIKEY_CACHE_TTL revocation latency): %v", subErr)
			logger.ErrorContext(ctx, msg, slog.Any("error", subErr))
			os.Exit(1)
		}
		msg := fmt.Sprintf("WARNING: API key invalidation subscriber unavailable (%v). Revocation will take up to %s to take effect.",
			subErr, auth.CacheTTLForLogging())
		logger.WarnContext(ctx, msg, slog.Any("error", subErr), slog.String(obs.LogKeyEvent, "apikey.invalidation_subscriber_unavailable"))
		subscriber = nil
	}

	rateLimiter := middleware.NewRateLimiter(redisClient)
	authenticator := auth.NewAPIKeyAuthenticator(db, redisClient, subscriber)

	// ingestRequestCounter records sentinel_ingest_requests_total{outcome}. Outcome can be decided by
	// three different layers (auth: 401, rate limit: 429, handler: 202/4xx/5xx), so rather than each
	// layer reporting its own metric, observeMiddleware wraps the WHOLE per-request chain and classifies
	// the final status code once. No project_id label (docs/plans/OBSERVABILITY_PLAN.md §5 — unbounded
	// cardinality).
	meter := otel.Meter("ingestor-go")
	ingestRequestCounter, err := meter.Int64Counter(
		obs.MetricIngestRequests,
		metric.WithDescription("Total ingest requests by outcome"),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		logger.ErrorContext(ctx, "obs: failed to create ingest request counter; requests will not be recorded as a metric",
			slog.String("error", err.Error()))
	}

	ingestChain := observeMiddleware(ingestRequestCounter,
		authenticator.Middleware(
			rateLimiter.Middleware(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handleIngest(ingestService, db, logger).ServeHTTP(w, r)
				}),
			),
		),
	)
	batchChain := observeMiddleware(ingestRequestCounter,
		authenticator.Middleware(
			rateLimiter.Middleware(
				http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					handleBatchIngest(ingestService, db, logger).ServeHTTP(w, r)
				}),
			),
		),
	)

	// otelhttp wraps the outermost layer of each chain so every request gets a server span honouring an
	// inbound W3C traceparent (or starting a new root trace when none is present, per D-e). That span is
	// what observeMiddleware reads the trace id from for X-Request-Id, and what the producer span inside
	// service.IngestService.Ingest (the NATS publish) becomes a child of.
	http.Handle("/ingest", otelhttp.NewHandler(ingestChain, "POST /ingest"))
	http.Handle("/ingest/batch", otelhttp.NewHandler(batchChain, "POST /ingest/batch"))
	http.HandleFunc("/health", handleHealth(db))
	if providers != nil {
		// Never auth'd or rate-limited (deliverable #2) — mounted directly, outside both middlewares.
		http.Handle("/metrics", providers.MetricsHandler)
	}

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
		// tests/e2e/ratelimit_test.go's U20 requires this exact, case-sensitive substring
		// ("Starting ingestor") to prove the process reached serving state — keep it verbatim.
		logger.InfoContext(ctx, fmt.Sprintf("Starting ingestor on %s", addr), slog.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.ErrorContext(ctx, fmt.Sprintf("Server failed: %v", err), slog.Any("error", err))
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.InfoContext(ctx, "Shutting down server...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.ErrorContext(ctx, fmt.Sprintf("Server forced to shutdown: %v", err), slog.Any("error", err))
	}

	// Flush the last spans/metrics before the process exits (docs/plans/OBSERVABILITY_PLAN.md finding
	// 6a) — without this, exactly the spans covering the shutdown-triggering incident are dropped
	// instead of exported. Bounded so a dead collector cannot hang shutdown indefinitely.
	if providers != nil {
		obsShutdownCtx, obsShutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer obsShutdownCancel()
		if err := providers.Shutdown(obsShutdownCtx); err != nil {
			logger.Error("obs: error flushing/closing tracing and metrics providers on shutdown",
				slog.String("error", err.Error()))
		}
	}
}

// statusRecorder wraps http.ResponseWriter to capture the status code a handler chain ultimately wrote,
// so observeMiddleware can classify the outcome after auth, rate limiting, and the handler itself have
// all had a chance to decide it — none of those three layers needs to report the metric itself.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Write mirrors net/http's own "implicit 200 if nobody called WriteHeader" rule, so a handler that only
// calls Write is still recorded as a 200 rather than 0.
func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	return sr.ResponseWriter.Write(b)
}

// outcomeForStatus maps an HTTP status code to one of the fixed obs.Outcome* values
// (docs/plans/OBSERVABILITY_PLAN.md §2/obs.go) so sentinel_ingest_requests_total{outcome} never grows an
// extra time series from a hand-typed string at a second call site.
func outcomeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return obs.OutcomeUnauthorized
	case status == http.StatusTooManyRequests:
		return obs.OutcomeRateLimited
	case status >= 200 && status < 300:
		return obs.OutcomeAccepted
	default:
		return obs.OutcomeRejected
	}
}

// observeMiddleware wraps the whole per-request middleware chain (auth, rate limiting, the handler
// itself) to provide two cross-cutting concerns that no single inner layer owns:
//
//   - X-Request-Id (deliverable #4, plan D-d): the current span's trace id in hex, read from the
//     context otelhttp already populated by the time this runs (observeMiddleware is wrapped BY
//     otelhttp.NewHandler in main(), never the reverse) — set unconditionally so both success and error
//     responses carry it.
//   - sentinel_ingest_requests_total{outcome} (deliverable #5): recorded once per request from the
//     final status code, however far the request got.
//
// counter may be nil (its creation logged and swallowed in main() rather than treated as fatal); Add is
// skipped in that case rather than panicking.
func observeMiddleware(counter metric.Int64Counter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc := trace.SpanContextFromContext(r.Context()); sc.IsValid() {
			w.Header().Set(obs.HTTPRequestIDHeader, sc.TraceID().String())
		}

		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r)
		if sr.status == 0 {
			sr.status = http.StatusOK
		}

		if counter != nil {
			counter.Add(r.Context(), 1, metric.WithAttributes(
				attribute.String(obs.LabelOutcome, outcomeForStatus(sr.status)),
			))
		}
	})
}

// batchItemError names a single failed item in a batch ingest response by its
// index in the request payload, so a caller can tell total failure from
// partial or full success (VERIFIED_STATE.md S3/P2-5: previously the batch
// endpoint always returned 202 even when every item failed).
type batchItemError struct {
	Index   int    `json:"index"`
	Message string `json:"message"`
}

// batchItemEventID names a single successfully-ingested batch item's EFFECTIVE event_id by its index
// in the request payload (docs/plans/IDEMPOTENCY_PLAN.md D-a/D-f) — mirrors batchItemError's
// index-keyed shape so a client can diff what it sent against what was used, per item.
type batchItemEventID struct {
	Index   int    `json:"index"`
	EventID string `json:"event_id"`
}

// batchResult is the /ingest/batch response body. The single-event /ingest
// endpoint keeps its pre-existing {"status":"accepted"} response shape
// (docs/sdk-specification.md section 4) — only the batch endpoint's
// all-or-nothing 202 semantics were in scope for this fix (P2-5).
//
// EventIDs is additive (docs/plans/IDEMPOTENCY_PLAN.md D-f): Ingested/Failed/Errors keep their
// pre-existing shape and semantics unchanged.
type batchResult struct {
	Ingested int                `json:"ingested"`
	Failed   int                `json:"failed"`
	Errors   []batchItemError   `json:"errors,omitempty"`
	EventIDs []batchItemEventID `json:"event_ids,omitempty"`
}

func handleIngest(svc *service.IngestService, db *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
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
			// Logged with the request's context so the handler wrapping in obs.Handler injects
			// trace_id/span_id automatically (docs/plans/OBSERVABILITY_PLAN.md D-a) — never threaded by
			// hand. Message text kept unchanged; the error is also attached structurally.
			logger.ErrorContext(r.Context(), fmt.Sprintf("Failed to ingest error: %v", err), slog.Any("error", err))
			if strings.Contains(err.Error(), "validation failed") {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			http.Error(w, "Failed to ingest error", http.StatusInternalServerError)
			return
		}

		logger.InfoContext(r.Context(), fmt.Sprintf("Successfully ingested error: project=%s, class=%s", payload.ProjectKey, payload.ErrorClass),
			slog.String("project_key", payload.ProjectKey), slog.String("error_class", payload.ErrorClass))
		w.WriteHeader(http.StatusAccepted)
		// event_id is the EFFECTIVE id (docs/plans/IDEMPOTENCY_PLAN.md D-a/D-f): svc.Ingest mutates
		// payload.EventID in place to whatever was actually published - the client's own value when
		// usable, or a freshly minted UUIDv4 when it was absent/oversized - so a client can diff what it
		// sent against what was used. Additive key; "status" is unchanged.
		json.NewEncoder(w).Encode(map[string]string{"status": "accepted", "event_id": payload.EventID})
	}
}

func handleBatchIngest(svc *service.IngestService, db *pgxpool.Pool, logger *slog.Logger) http.HandlerFunc {
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
				logger.ErrorContext(r.Context(), fmt.Sprintf("Failed to ingest batch item %d: %v", i, err),
					slog.Int("batch_index", i), slog.Any("error", err))
				result.Failed++
				result.Errors = append(result.Errors, batchItemError{Index: i, Message: err.Error()})
				continue
			}
			result.Ingested++
			// payloads[i].EventID is the EFFECTIVE id: svc.Ingest mutated it in place before publish
			// (docs/plans/IDEMPOTENCY_PLAN.md D-a/D-f).
			result.EventIDs = append(result.EventIDs, batchItemEventID{Index: i, EventID: payloads[i].EventID})
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
