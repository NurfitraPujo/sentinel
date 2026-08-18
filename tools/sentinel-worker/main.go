// sentinel-worker is the Agent Worker harness (plan §0): a durable, provider-agnostic continuous
// agent that polls Sentinel's events feed, dispatches jobs, consults an Advisor, and applies
// decisions. This file wires the full §5 config surface, signal handling, the WORKER_ENABLED /
// WORKER_EXECUTE gate semantics, and the `-healthcheck` self-probe subcommand used by the
// container healthcheck (plan §6).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/health"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/loop"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// Config is the full §5 config surface, typed. Every field has a documented default; Load never
// fails on a single bad value — it collects every validation problem and returns them together
// (plan brief: "validation errors collected not fatal-on-first") so an operator sees every
// misconfiguration in one pass instead of fixing them one exit-code at a time.
type Config struct {
	// Sentinel
	SentinelURL      string
	SentinelAgentKey string

	// Gates (plan §5/§6)
	WorkerEnabled    bool // entrypoint.sh's gate; read here too for the in-process readiness story
	WorkerExecute    bool // false = dry-run: journal decisions, never send mutating calls
	WorkerFixEnabled bool // deployment kill switch; real FIX policy is per-project server-side

	// Loop
	WorkerStateDir      string
	WorkerPollInterval  time.Duration
	WorkerPollJitter    float64
	WorkerConcurrency   int
	WorkerBackfillHours int
	WorkerEventTypes    []string
	WorkerProjects      []string
	WorkerSweepInterval time.Duration
	// WorkerShutdownTimeout bounds how long runWorker waits, after srv.Shutdown, for the
	// dispatcher's per-issue queue goroutines to drain in-flight work (finding 1's WaitGroup
	// drain) before the process exits anyway. Plan §5.
	WorkerShutdownTimeout time.Duration

	// LLM
	LLMProvider         string
	LLMModel            string
	LLMAPIKey           string
	LLMBaseURL          string
	LLMFallbackProvider string
	LLMFallbackModel    string
	LLMFallbackAPIKey   string
	LLMFallbackBaseURL  string

	// Budgets
	WorkerDailyTokenBudget int
	WorkerTriageMaxTurns   int
	WorkerFollowupMaxTurns int
	WorkerMaxOutputTokens  int
	WorkerTriageTimeout    time.Duration
	WorkerFollowupTimeout  time.Duration
	WorkerFixTimeout       time.Duration
	WorkerMaxFixAttempts   int
	WorkerMaxFixJobsPerDay int
	WorkerMaxPRsPerDay     int
	WorkerMaxTriagePerHour int
	WorkerFixConfidence    float64
	WorkerFixMaxFiles      int

	// Claims
	WorkerClaimHeartbeat time.Duration
	WorkerNagDays        int

	// State / snapshots
	WorkerSnapshotBackend    string // none | s3
	WorkerSnapshotInterval   time.Duration
	S3Endpoint               string
	S3Bucket                 string
	S3Prefix                 string
	S3Region                 string
	S3AccessKey              string
	S3SecretKey              string
	WorkerKeystore           string // file | kubernetes-secret
	WorkerKeySecretName      string
	WorkerKeySecretNamespace string

	// Git
	WorkerRepoCacheDir           string
	WorkerRepoRefresh            time.Duration
	GitGitHubToken               string
	GitBitbucketToken            string
	GitBitbucketUser             string
	GitBitbucketAppPassword      string
	FixExecutorCmd               string
	WorkerWorkspaceDir           string
	WorkerKeepFailedWorkspaces   bool
	WorkerWorkspaceRetentionDays int
	WorkerAgentLogMaxMB          int
	WorkerCredentialLabels       []string

	// Keys
	WorkerRotateBeforeHours int
	WorkerRotateEveryDays   int

	// Misc
	WorkerReportFailures  bool
	WorkerGateMaxVerbatim float64
	WorkerHealthAddr      string
}

// envLookup is the minimal os.Getenv-shaped seam Load needs, so tests can supply a fake
// environment without touching process-global state.
type envLookup func(key string) (string, bool)

func fromOSEnv(key string) (string, bool) { return os.LookupEnv(key) }

func getStr(env envLookup, key, def string) string {
	if v, ok := env(key); ok && v != "" {
		return v
	}
	return def
}

func getBool(env envLookup, key string, def bool, errs *[]string) bool {
	v, ok := env(key)
	if !ok || v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: invalid bool %q", key, v))
		return def
	}
	return b
}

func getInt(env envLookup, key string, def int, errs *[]string) int {
	v, ok := env(key)
	if !ok || v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: invalid int %q", key, v))
		return def
	}
	return n
}

func getFloat(env envLookup, key string, def float64, errs *[]string) float64 {
	v, ok := env(key)
	if !ok || v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: invalid float %q", key, v))
		return def
	}
	return f
}

// getDuration parses a Go duration string (plan §5's own notation: "10s", "3m", "30m", "12h", ...)
// via time.ParseDuration, matching the repo-wide convention (apps/processor-go/webhooks/dispatcher.go,
// apps/processor-go/dlqmonitor/monitor.go, apps/ingestor-go/auth/apikey.go, .env.example). A bare
// integer is tolerated as a whole number of seconds for backward compatibility with anything that
// still writes the old bare-seconds form.
func getDuration(env envLookup, key string, def time.Duration, errs *[]string) time.Duration {
	v, ok := env(key)
	if !ok || v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	*errs = append(*errs, fmt.Sprintf("%s: invalid duration %q (want Go duration notation like \"10s\", \"3m\", \"12h\")", key, v))
	return def
}

func getList(env envLookup, key string) []string {
	v, ok := env(key)
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadConfig reads the full §5 surface from env, applying every documented default and collecting
// every validation error (never fatal-on-first). The second return value is nil when config is
// valid; a non-nil, non-empty slice means the caller should NOT exit but should keep the process
// up with /readyz failing (plan §6: "invalid config keeps the process up... rather than exiting
// into a restart loop").
func LoadConfig(env envLookup) (Config, []string) {
	var errs []string

	c := Config{
		SentinelURL:      getStr(env, "SENTINEL_URL", ""),
		SentinelAgentKey: getStr(env, "SENTINEL_AGENT_KEY", ""),

		WorkerEnabled:    getBool(env, "WORKER_ENABLED", false, &errs),
		WorkerExecute:    getBool(env, "WORKER_EXECUTE", false, &errs),
		WorkerFixEnabled: getBool(env, "WORKER_FIX_ENABLED", true, &errs),

		WorkerStateDir:        getStr(env, "WORKER_STATE_DIR", "/var/lib/sentinel-worker"),
		WorkerPollInterval:    getDuration(env, "WORKER_POLL_INTERVAL", 10*time.Second, &errs),
		WorkerPollJitter:      getFloat(env, "WORKER_POLL_JITTER", 0.2, &errs),
		WorkerConcurrency:     getInt(env, "WORKER_CONCURRENCY", 2, &errs),
		WorkerBackfillHours:   getInt(env, "WORKER_BACKFILL_HOURS", 24, &errs),
		WorkerEventTypes:      getList(env, "WORKER_EVENT_TYPES"),
		WorkerProjects:        getList(env, "WORKER_PROJECTS"),
		WorkerSweepInterval:   getDuration(env, "WORKER_SWEEP_INTERVAL", time.Hour, &errs),
		WorkerShutdownTimeout: getDuration(env, "WORKER_SHUTDOWN_TIMEOUT", 30*time.Second, &errs),

		LLMProvider:         getStr(env, "LLM_PROVIDER", "openai"),
		LLMModel:            getStr(env, "LLM_MODEL", ""),
		LLMAPIKey:           getStr(env, "LLM_API_KEY", ""),
		LLMBaseURL:          getStr(env, "LLM_BASE_URL", ""),
		LLMFallbackProvider: getStr(env, "LLM_FALLBACK_PROVIDER", ""),
		LLMFallbackModel:    getStr(env, "LLM_FALLBACK_MODEL", ""),
		LLMFallbackAPIKey:   getStr(env, "LLM_FALLBACK_API_KEY", ""),
		LLMFallbackBaseURL:  getStr(env, "LLM_FALLBACK_BASE_URL", ""),

		WorkerDailyTokenBudget: getInt(env, "WORKER_DAILY_TOKEN_BUDGET", 0, &errs),
		WorkerTriageMaxTurns:   getInt(env, "WORKER_TRIAGE_MAX_TURNS", 6, &errs),
		WorkerFollowupMaxTurns: getInt(env, "WORKER_FOLLOWUP_MAX_TURNS", 4, &errs),
		WorkerMaxOutputTokens:  getInt(env, "WORKER_MAX_OUTPUT_TOKENS", 0, &errs),
		WorkerTriageTimeout:    getDuration(env, "WORKER_TRIAGE_TIMEOUT", 3*time.Minute, &errs),
		WorkerFollowupTimeout:  getDuration(env, "WORKER_FOLLOWUP_TIMEOUT", 2*time.Minute, &errs),
		WorkerFixTimeout:       getDuration(env, "WORKER_FIX_TIMEOUT", 30*time.Minute, &errs),
		WorkerMaxFixAttempts:   getInt(env, "WORKER_MAX_FIX_ATTEMPTS", 2, &errs),
		WorkerMaxFixJobsPerDay: getInt(env, "WORKER_MAX_FIX_JOBS_PER_DAY", 10, &errs),
		WorkerMaxPRsPerDay:     getInt(env, "WORKER_MAX_PRS_PER_DAY", 10, &errs),
		WorkerMaxTriagePerHour: getInt(env, "WORKER_MAX_TRIAGE_PER_HOUR", 60, &errs),
		WorkerFixConfidence:    getFloat(env, "WORKER_FIX_CONFIDENCE", 0.7, &errs),
		WorkerFixMaxFiles:      getInt(env, "WORKER_FIX_MAX_FILES", 20, &errs),

		WorkerClaimHeartbeat: getDuration(env, "WORKER_CLAIM_HEARTBEAT", 12*time.Hour, &errs),
		WorkerNagDays:        getInt(env, "WORKER_NAG_DAYS", 3, &errs),

		WorkerSnapshotBackend:    getStr(env, "WORKER_SNAPSHOT_BACKEND", "none"),
		WorkerSnapshotInterval:   getDuration(env, "WORKER_SNAPSHOT_INTERVAL", 5*time.Minute, &errs),
		S3Endpoint:               getStr(env, "S3_ENDPOINT", ""),
		S3Bucket:                 getStr(env, "S3_BUCKET", ""),
		S3Prefix:                 getStr(env, "S3_PREFIX", ""),
		S3Region:                 getStr(env, "S3_REGION", ""),
		S3AccessKey:              getStr(env, "S3_ACCESS_KEY", ""),
		S3SecretKey:              getStr(env, "S3_SECRET_KEY", ""),
		WorkerKeystore:           getStr(env, "WORKER_KEYSTORE", "file"),
		WorkerKeySecretName:      getStr(env, "WORKER_KEY_SECRET_NAME", ""),
		WorkerKeySecretNamespace: getStr(env, "WORKER_KEY_SECRET_NAMESPACE", ""),

		WorkerRepoCacheDir:      getStr(env, "WORKER_REPO_CACHE_DIR", "/var/cache/sentinel-worker/repos"),
		WorkerRepoRefresh:       getDuration(env, "WORKER_REPO_REFRESH", 15*time.Minute, &errs),
		GitGitHubToken:          getStr(env, "GIT_GITHUB_TOKEN", ""),
		GitBitbucketToken:       getStr(env, "GIT_BITBUCKET_TOKEN", ""),
		GitBitbucketUser:        getStr(env, "GIT_BITBUCKET_USER", ""),
		GitBitbucketAppPassword: getStr(env, "GIT_BITBUCKET_APP_PASSWORD", ""),
		FixExecutorCmd:          getStr(env, "FIX_EXECUTOR_CMD", ""),
		// plan §4.5: Fix Executor workspaces are a trust boundary -- an external coding CLI runs
		// arbitrary repo code there -- and must NEVER be co-located under WORKER_STATE_DIR (that
		// would also drag every clone into §2.8's periodic state-dir snapshot tarball). Default
		// sibling of WORKER_REPO_CACHE_DIR, both under /var/cache, never under /var/lib/sentinel-worker.
		WorkerWorkspaceDir:           getStr(env, "WORKER_WORKSPACE_DIR", "/var/cache/sentinel-worker/workspaces"),
		WorkerKeepFailedWorkspaces:   getBool(env, "WORKER_KEEP_FAILED_WORKSPACES", false, &errs),
		WorkerWorkspaceRetentionDays: getInt(env, "WORKER_WORKSPACE_RETENTION_DAYS", 3, &errs),
		WorkerAgentLogMaxMB:          getInt(env, "WORKER_AGENT_LOG_MAX_MB", 10, &errs),
		WorkerCredentialLabels:       getList(env, "WORKER_CREDENTIAL_LABELS"),

		WorkerRotateBeforeHours: getInt(env, "WORKER_ROTATE_BEFORE_HOURS", 72, &errs),
		WorkerRotateEveryDays:   getInt(env, "WORKER_ROTATE_EVERY_DAYS", 30, &errs),

		WorkerReportFailures:  getBool(env, "WORKER_REPORT_FAILURES", false, &errs),
		WorkerGateMaxVerbatim: getFloat(env, "WORKER_GATE_MAX_VERBATIM", 0.25, &errs),
		WorkerHealthAddr:      getStr(env, "WORKER_HEALTH_ADDR", ":9090"),
	}

	// Cross-field / semantic validation (collected, not fatal-on-first).
	if c.WorkerEnabled {
		// SENTINEL_URL/KEY are only required once the gate is actually on — entrypoint.sh already
		// parks a gated-off container before main() runs at all, but WORKER_ENABLED is re-read
		// in-process too (plan brief) so a programmatic launch without the shell wrapper is safe.
		if c.SentinelURL == "" {
			errs = append(errs, "SENTINEL_URL: required when WORKER_ENABLED=true")
		}
		if c.SentinelAgentKey == "" {
			errs = append(errs, "SENTINEL_AGENT_KEY: required when WORKER_ENABLED=true (or a keystore must provide one)")
		}
	}
	if c.WorkerPollJitter < 0 || c.WorkerPollJitter > 1 {
		errs = append(errs, "WORKER_POLL_JITTER: must be in [0,1]")
	}
	if c.WorkerPollInterval <= 0 {
		errs = append(errs, "WORKER_POLL_INTERVAL: must be > 0")
	}
	if c.WorkerSweepInterval <= 0 {
		errs = append(errs, "WORKER_SWEEP_INTERVAL: must be > 0")
	}
	if c.WorkerShutdownTimeout <= 0 {
		errs = append(errs, "WORKER_SHUTDOWN_TIMEOUT: must be > 0")
	}
	// GET /api/agent/events accepts only a single `project=` value (loop/poll.go's
	// httpEventsClient.GetEvents), so a WORKER_PROJECTS list longer than one silently drops every
	// element but the first with no operator-visible signal. Reject it outright instead.
	if len(c.WorkerProjects) > 1 {
		errs = append(errs, "WORKER_PROJECTS: only one project filter is supported by the events feed")
	}
	if c.WorkerConcurrency < 1 {
		errs = append(errs, "WORKER_CONCURRENCY: must be >= 1")
	}
	// N8a ships buildRunner wired to jobs.NotImplementedActor{} — there is no real Actor yet (the
	// FIX/act engine lands in a later phase). WORKER_EXECUTE=true would still pass DryRun=false
	// into loop.Runner, which claims every triageable issue for real (a genuine, irreversible
	// mutation) and only THEN fails at NotImplementedActor.Act — net effect, the Agent silently
	// claims and squats on the org's entire triage backlog until the server's 24h reaper, with no
	// heartbeat/sweep yet to release it. Reject the flag outright until a real Actor exists; N8a's
	// only supported/proven mode is dry-run (plan §9).
	if c.WorkerExecute {
		errs = append(errs, "WORKER_EXECUTE: must be false — N8a has no real Actor yet (jobs.NotImplementedActor is a stub); setting this to true would claim issues for real and then fail after the claim, per plan §9's dry-run-only scope")
	}
	if c.WorkerFixConfidence < 0 || c.WorkerFixConfidence > 1 {
		errs = append(errs, "WORKER_FIX_CONFIDENCE: must be in [0,1]")
	}
	if c.WorkerGateMaxVerbatim < 0 || c.WorkerGateMaxVerbatim > 1 {
		errs = append(errs, "WORKER_GATE_MAX_VERBATIM: must be in [0,1]")
	}
	switch c.WorkerSnapshotBackend {
	case "none", "s3":
	default:
		errs = append(errs, `WORKER_SNAPSHOT_BACKEND: must be "none" or "s3"`)
	}
	if c.WorkerSnapshotBackend == "s3" && (c.S3Bucket == "" || c.S3Endpoint == "") {
		errs = append(errs, "S3_BUCKET and S3_ENDPOINT: required when WORKER_SNAPSHOT_BACKEND=s3")
	}
	switch c.WorkerKeystore {
	case "file", "kubernetes-secret":
	default:
		errs = append(errs, `WORKER_KEYSTORE: must be "file" or "kubernetes-secret"`)
	}
	if c.WorkerKeystore == "kubernetes-secret" && (c.WorkerKeySecretName == "" || c.WorkerKeySecretNamespace == "") {
		errs = append(errs, "WORKER_KEY_SECRET_NAME and WORKER_KEY_SECRET_NAMESPACE: required when WORKER_KEYSTORE=kubernetes-secret")
	}
	// plan §2.7: "Startup validates WORKER_CLAIM_HEARTBEAT < CLAIM_STALE_HOURS when the latter is
	// known" — CLAIM_STALE_HOURS is a SERVER constant (24h, not worker config), so N8a checks it
	// against the documented value; a future phase may fetch it from the server instead.
	const claimStaleHours = 24 * time.Hour
	if c.WorkerClaimHeartbeat >= claimStaleHours {
		errs = append(errs, fmt.Sprintf("WORKER_CLAIM_HEARTBEAT (%s) must be less than the server's CLAIM_STALE_HOURS (%s)", c.WorkerClaimHeartbeat, claimStaleHours))
	}
	switch c.LLMProvider {
	case "openai", "anthropic", "gemini":
	default:
		errs = append(errs, `LLM_PROVIDER: must be one of "openai", "anthropic", "gemini"`)
	}
	// LLM_FALLBACK_PROVIDER is optional (empty = no fallback configured), but when set it must be
	// a real provider just like LLM_PROVIDER -- this was previously asymmetric: a typo'd fallback
	// provider silently fell through to whatever the fallback-selection code did with an unknown
	// string, discovered only when the primary provider's circuit actually opened (plan §2.4).
	switch c.LLMFallbackProvider {
	case "", "openai", "anthropic", "gemini":
	default:
		errs = append(errs, `LLM_FALLBACK_PROVIDER: must be one of "openai", "anthropic", "gemini" (or empty for no fallback)`)
	}

	// Budget/volume-cap knobs (plan §2.6/§5): negative values are always nonsensical (a negative
	// turn cap, timeout-count, or budget has no meaning), so collect them the same way as every
	// other validation error rather than letting a negative silently propagate into "no cap
	// applied" or worse, an unthrottled loop.
	requireNonNegative(&errs, "WORKER_DAILY_TOKEN_BUDGET", c.WorkerDailyTokenBudget)
	requireNonNegative(&errs, "WORKER_TRIAGE_MAX_TURNS", c.WorkerTriageMaxTurns)
	requireNonNegative(&errs, "WORKER_FOLLOWUP_MAX_TURNS", c.WorkerFollowupMaxTurns)
	requireNonNegative(&errs, "WORKER_MAX_OUTPUT_TOKENS", c.WorkerMaxOutputTokens)
	requireNonNegative(&errs, "WORKER_MAX_FIX_ATTEMPTS", c.WorkerMaxFixAttempts)
	requireNonNegative(&errs, "WORKER_MAX_FIX_JOBS_PER_DAY", c.WorkerMaxFixJobsPerDay)
	requireNonNegative(&errs, "WORKER_MAX_PRS_PER_DAY", c.WorkerMaxPRsPerDay)
	requireNonNegative(&errs, "WORKER_MAX_TRIAGE_PER_HOUR", c.WorkerMaxTriagePerHour)
	requireNonNegative(&errs, "WORKER_FIX_MAX_FILES", c.WorkerFixMaxFiles)
	requireNonNegative(&errs, "WORKER_NAG_DAYS", c.WorkerNagDays)
	requireNonNegative(&errs, "WORKER_WORKSPACE_RETENTION_DAYS", c.WorkerWorkspaceRetentionDays)
	requireNonNegative(&errs, "WORKER_AGENT_LOG_MAX_MB", c.WorkerAgentLogMaxMB)
	requireNonNegative(&errs, "WORKER_ROTATE_BEFORE_HOURS", c.WorkerRotateBeforeHours)
	requireNonNegative(&errs, "WORKER_ROTATE_EVERY_DAYS", c.WorkerRotateEveryDays)

	// plan §4.5: Fix Executor workspaces (and the repo clone cache) must never resolve under
	// WORKER_STATE_DIR -- that directory holds agent-key.json and jobs.journal, and §2.8 tarballs
	// it wholesale on every WORKER_SNAPSHOT_INTERVAL. A misconfigured operator co-locating them
	// would silently reopen both the trust-boundary hole and the oversized-snapshot bug rev 1 had.
	if underDir(c.WorkerWorkspaceDir, c.WorkerStateDir) {
		errs = append(errs, "WORKER_WORKSPACE_DIR: must not be under WORKER_STATE_DIR (plan §4.5 -- Fix Executor workspaces are a trust boundary and must stay off the snapshotted state volume)")
	}
	if underDir(c.WorkerRepoCacheDir, c.WorkerStateDir) {
		errs = append(errs, "WORKER_REPO_CACHE_DIR: must not be under WORKER_STATE_DIR (plan §4.5)")
	}

	return c, errs
}

// requireNonNegative collects a validation error when v < 0 (plan §2.6's budget/volume-cap knobs
// have no meaning when negative).
func requireNonNegative(errs *[]string, name string, v int) {
	if v < 0 {
		*errs = append(*errs, fmt.Sprintf("%s: must be >= 0", name))
	}
}

// underDir reports whether candidate is WORKER_STATE_DIR itself or nested under it, using pure
// lexical path comparison (both configured directories may not exist yet at validation time, so
// filepath.EvalSymlinks/Abs-against-cwd would be misleading). Both are cleaned first so trailing
// slashes and "." segments don't defeat the check.
func underDir(candidate, base string) bool {
	if candidate == "" || base == "" {
		return false
	}
	c := filepath.Clean(candidate)
	b := filepath.Clean(base)
	if c == b {
		return true
	}
	rel, err := filepath.Rel(b, c)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(runHealthcheck(os.Args[2:], os.Stderr))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, errs := LoadConfig(fromOSEnv)
	for _, e := range errs {
		logger.Error("config validation error", "detail", e)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runApp(ctx, cfg, logger, errs)
}

// runApp is main's pipeline entry point past config loading: the in-process WORKER_ENABLED gate,
// then (when enabled) the health server + poll/dispatch pipeline. Split out from main() so a test
// can drive the gate itself end to end -- inverting the `!cfg.WorkerEnabled` condition below must
// turn a passing "pipeline does not start while disabled" test red (mutation-tested, see
// TestWorkerEnabledGate_PipelineDoesNotStart in main_test.go).
func runApp(ctx context.Context, cfg Config, logger *slog.Logger, errs []string) {
	if !cfg.WorkerEnabled {
		// Belt-and-braces: entrypoint.sh already parks a gated-off container before exec'ing this
		// binary at all (plan §6: "checks WORKER_ENABLED FIRST and parks without reading any other
		// config"), but a direct/programmatic launch of the binary must behave identically -- no
		// health server, no poll loop, zero outbound requests, until the process is restarted with
		// WORKER_ENABLED=true.
		logger.Info("WORKER_ENABLED is not true; parking", "workerEnabled", cfg.WorkerEnabled)
		<-ctx.Done()
		return
	}

	logger.Info("sentinel-worker starting",
		"workerExecute", cfg.WorkerExecute,
		"workerFixEnabled", cfg.WorkerFixEnabled,
		"healthAddr", cfg.WorkerHealthAddr,
		"configErrors", len(errs),
	)

	runWorker(ctx, cfg, logger, len(errs) == 0)
}

// runWorker binds the health server and, when config is valid, assembles and runs the
// poll -> dispatch -> run -> journal pipeline (plan §0/§9's N8a proof: "worker against compose
// stack with WORKER_ENABLED=true, WORKER_EXECUTE=false journals decisions"). It blocks until ctx
// is cancelled. main() is the single wiring point per plan §1's main.go doc comment.
func runWorker(ctx context.Context, cfg Config, logger *slog.Logger, configValid bool) {
	st := health.NewStatus()
	if configValid {
		st.SetReady(true)
	} else {
		st.SetReady(false, "invalid configuration")
	}

	srv := &http.Server{Addr: cfg.WorkerHealthAddr, Handler: health.Handler(st)}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("health server exited", "error", err)
		}
	}()

	// dispatcherPtr is how runPipeline (running on its own goroutine) publishes the *loop.Dispatcher
	// it assembles back to runWorker, so the shutdown path below can call Drain on it. atomic.Pointer
	// rather than a plain field: runPipeline's goroutine writes it once, runWorker's goroutine reads
	// it after ctx is cancelled -- concurrent by construction.
	var dispatcherPtr atomic.Pointer[loop.Dispatcher]

	if configValid {
		// The cursor-freshness window MUST be armed here, before runPipeline's goroutine even
		// starts, not from inside runPipeline after resolveAgentID/Bootstrap have both already
		// succeeded (that left the entire pre-loop phase -- identity resolution retrying forever
		// against an unreachable sentinel API, then a slow first bootstrap sweep -- reporting
		// ready:true with zero evidence anything ever worked, because Ready()'s staleness check
		// only applies once cursorFreshness > 0). Status.Ready() already treats a never-set
		// lastCursorSave as stale once a window is armed (health/health.go: "s.lastCursorSave.IsZero()
		// ... ready = false"), so arming it up front makes /readyz correctly 503 until the first
		// cursor persist actually happens, instead of silently skipping that leg of plan §7's
		// "cursor persisted recently AND auth valid AND config valid".
		//
		// The window is sized generously (see cursorFreshnessStartupWindow) so a normal-length
		// identity-resolution retry loop or first bootstrap sweep doesn't flap readiness while
		// legitimately still starting up; a genuinely wedged pipeline still trips it well before
		// an operator would otherwise notice.
		st.SetCursorFreshnessWindow(cursorFreshnessStartupWindow(cfg.WorkerPollInterval))
		go runPipeline(ctx, cfg, logger, st, &dispatcherPtr)
	} else {
		logger.Error("skipping poll/dispatch pipeline: invalid configuration, /readyz will report not-ready")
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	// Drain the dispatcher's per-issue queue goroutines -- including whatever job Runner.Run was
	// mid-execution on when ctx was cancelled -- up to WORKER_SHUTDOWN_TIMEOUT (finding 1: "no
	// WaitGroup/drain exists" meant SIGTERM neither cancelled nor waited for in-flight jobs). The
	// dispatcher's own Ctx (set to this same ctx by runPipeline below) is what actually cancels
	// Runner.Run/the debounce wait; Drain here just bounds how long we wait for that cancellation
	// to be observed and acted on. dispatcherPtr is nil until runPipeline has assembled the
	// dispatcher, which can race a very-early SIGTERM -- nothing to drain in that case.
	if d := dispatcherPtr.Load(); d != nil {
		drainCtx, drainCancel := context.WithTimeout(context.Background(), cfg.WorkerShutdownTimeout)
		defer drainCancel()
		logger.Info("draining in-flight jobs before exit", "timeout", cfg.WorkerShutdownTimeout)
		d.Drain(drainCtx)
	}
	logger.Info("sentinel-worker stopped")
}

// selfResponse is the subset of GET /api/agent/self this worker needs: its own agent id, used for
// dispatch echo-suppression (loop.Classify) and as the claimant identity for precondition checks.
type selfResponse struct {
	AgentID string `json:"agentId"`
}

// resolveAgentID calls GET /api/agent/self to learn this worker's own agent id (plan §13), routed
// through sentinel.Client.GetSelf (not a second hand-rolled c.Do call) so the wire shape this
// package actually sends is covered by client_test.go's goldens.
func resolveAgentID(ctx context.Context, c *sentinel.Client) (string, error) {
	res, err := c.GetSelf(ctx)
	if err != nil {
		return "", err
	}
	if res.Status < 200 || res.Status >= 300 {
		return "", fmt.Errorf("GET /api/agent/self: %d %s", res.Status, sentinel.ErrorMessage(res.Body))
	}
	var self selfResponse
	if err := json.Unmarshal(res.Body, &self); err != nil {
		return "", fmt.Errorf("parsing self response: %w", err)
	}
	if self.AgentID == "" {
		return "", fmt.Errorf("GET /api/agent/self: empty agentId in response")
	}
	return self.AgentID, nil
}

// cursorFreshnessMultiple sets how many WORKER_POLL_INTERVALs may pass without a persisted cursor
// save before /readyz reports not-ready on staleness grounds (plan §7). A small multiple absorbs
// an ordinarily-slow tick or an in-flight bootstrap sweep without flapping readiness, while a
// genuinely wedged poll loop -- or, as of this fix, a pipeline that has never yet completed its
// first cursor persist at all (resolveAgentID retrying forever against an unreachable sentinel
// API, or a bootstrap sweep that has not finished) -- still trips it well before an operator would
// otherwise notice. The same constant and formula are used both here (armed before the pipeline
// goroutine starts, so the pre-loop phase is covered) and once the poll loop is running
// (poller.OnCursorSaved keeps refreshing NoteCursorSaved on every successful persist) -- there is
// deliberately no separate, wider "startup grace" window: that would make /readyz block staleness
// detection for minutes on a fast-polling deployment, trading a real observability gap for
// startup-flap insurance nothing here actually needs (SetReady(false, "invalid configuration")
// already covers config-invalid startup; a merely-slow bootstrap sweep is exactly the "ordinarily
// slow" case this multiple is sized to absorb).
const cursorFreshnessMultiple = 6

func cursorFreshnessStartupWindow(pollInterval time.Duration) time.Duration {
	return cursorFreshnessMultiple * pollInterval
}

// runPipeline assembles and runs the durable poll -> dispatch -> run -> journal pipeline (plan
// §0/§2). It never returns except when ctx is cancelled; any assembly failure (identity lookup,
// corrupt cursor) is logged loudly and retried rather than crashing the process, per plan §6's
// "invalid config keeps the process up" philosophy extended to runtime assembly failures. st, when
// non-nil, is wired for plan §7's /readyz composition: auth validity from the sentinel client's
// 401 signal, and cursor freshness from the poll loop's persisted-save signal. dispatcherOut, when
// non-nil, receives the assembled *loop.Dispatcher the moment it exists, so runWorker's shutdown
// path can Drain it (finding 1) -- a plain out-param rather than a return value because runPipeline
// itself never returns except on ctx cancellation, long after the caller needs the dispatcher.
func runPipeline(ctx context.Context, cfg Config, logger *slog.Logger, st *health.Status, dispatcherOut *atomic.Pointer[loop.Dispatcher]) {
	client := sentinel.NewClient(cfg.SentinelURL, cfg.SentinelAgentKey)
	if st != nil {
		client.OnAuthStatus = st.SetAuthValid
	}

	// Journal open + maintenance/RecoveryScan need no network at all, so they run BEFORE identity
	// resolution rather than after it (finding 6): with the old ordering, an API outage at startup
	// blocked recovery-observability of any in-flight jobs left by a prior crash for as long as
	// GET /api/agent/self kept failing -- an operator staring at logs during an outage would see
	// nothing about recovery until the outage cleared. Resume's actual replay (below) still has to
	// wait for identity, since it needs an authenticated client to re-drive ensure-claimed/Act --
	// but the journal has been opened, compacted, and its in-flight set logged well before that.
	journalPath := filepath.Join(cfg.WorkerStateDir, "jobs.journal")
	journal := state.OpenJournal(journalPath)

	// Recovery (CONTEXT.md: "the startup process after any restart: restore state, scan the
	// journal, then replay or resume each in-flight job") and cleanup (plan §6: "Cleanup is the
	// worker's own job — nothing external prunes for it") both run before the poll loop starts, and
	// cleanup keeps running daily for the life of the process.
	runJournalMaintenance(journal, cfg.WorkerStateDir, cfg.WorkerAgentLogMaxMB, logger, st)
	go journalMaintenanceLoop(ctx, journal, cfg.WorkerStateDir, cfg.WorkerAgentLogMaxMB, logger, st)

	agentID, err := resolveAgentID(ctx, client)
	for attempt := 1; err != nil; attempt++ {
		backoff := sentinel.BackoffForAttempt(attempt)
		logger.Error("resolving agent identity via GET /api/agent/self, retrying", "error", err, "attempt", attempt, "backoff", backoff)
		// sentinel.BackoffForAttempt's ladder (1s -> 5s -> 30s -> 2m -> 5m, plan §2.4 "Transient"
		// row) replaces the old flat WORKER_POLL_INTERVAL retry -- an outage at startup no longer
		// hammers the API at the poll cadence forever.
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		agentID, err = resolveAgentID(ctx, client)
	}
	logger.Info("resolved agent identity", "agentId", agentID)

	cursorPath := filepath.Join(cfg.WorkerStateDir, "cursor.json")
	cursor, cursorErr := state.LoadCursor(cursorPath)
	if cursorErr != nil {
		// A corrupt cursor.json is treated exactly like a missing one: bootstrap, never page the
		// feed from seq 0 (plan §2.1 -- "'page from seq 0' is wrong", C9).
		logger.Error("cursor.json failed to parse, running bootstrap sweep instead of starting from seq 0", "error", cursorErr, "path", cursorPath)
		cursor = nil
	}

	runner := buildRunner(cfg, client, journal, agentID, logger)
	// Shared by both the runner (done/failed/skipped_* outcomes from the pipeline) and the
	// dispatcher (superseded/skipped_* outcomes decided in the queue layer itself, before a job
	// ever reaches Runner.Run) so plan §7's "jobs by kind×outcome" counts every journaled outcome,
	// not just the runner's subset (validator finding: superseded/cancelled outcomes were
	// journaled but never counted).
	var onOutcome func(kind, outcome string)
	if st != nil {
		onOutcome = func(kind, outcome string) { st.Inc(health.JobsTotalMetricName(kind, outcome), 1) }
		runner.OnOutcome = onOutcome
	}

	// Journal-driven resume (CONTEXT.md's Recovery contract, plan §2.2/§8's required proof): every
	// job runJournalMaintenance's RecoveryScan found in-flight above is replayed here, verbatim,
	// through runner.Resume, BEFORE the poll loop (and the dispatcher it feeds) starts -- so a
	// crash/SIGTERM mid-job is actually recovered, not merely logged (validator finding:
	// resumeFromAdvised/RecoveryScan were previously unreachable from main()). This runs
	// synchronously and sequentially, before anything else touches the journal, so there is no
	// concurrency to race against the per-issue dispatcher queues that start afterward.
	if inFlight, _, err := journal.RecoveryScan(); err != nil {
		logger.Error("recovery scan for resume failed", "error", err)
	} else if len(inFlight) > 0 {
		logger.Info("recovery: resuming in-flight jobs from a prior run", "count", len(inFlight))
		for _, job := range inFlight {
			if err := runner.Resume(ctx, job); err != nil {
				logger.Error("recovery: resuming in-flight job failed", "jobId", job.JobID, "issueId", job.IssueID, "kind", job.Kind, "state", job.State, "error", err)
			}
		}
	}

	// The Dispatcher IS the Enqueuer the poll loop hands events to (plan §3): it durably journals
	// the "queued" record synchronously before Enqueue returns (the guarantee PollLoop relies on to
	// advance/persist its cursor), then runs each job asynchronously on its issue's own per-issue
	// serial queue, with coalescing, FOLLOW-UP debounce, and per-job panic recovery. This replaces
	// the earlier degenerate JournalEnqueuer, which ran Runner.Run synchronously inline in the poll
	// loop and let one poisoned job wedge the entire feed (see Dispatcher.Enqueue's doc comment).
	dispatcher := &loop.Dispatcher{
		Runner:    runner,
		Journal:   journal,
		Log:       logger,
		Debounce:  cfg.WorkerPollInterval,
		OnOutcome: onOutcome,
		// Ctx is this same shutdown-aware ctx: cancelling it (SIGTERM/SIGINT via
		// signal.NotifyContext in main()) reaches the debounce wait and Runner.Run for whatever
		// job is in flight, instead of the dispatcher severing it behind its own
		// context.Background() (finding 1).
		Ctx: ctx,
	}
	if dispatcherOut != nil {
		dispatcherOut.Store(dispatcher)
	}
	var enqueuer loop.Enqueuer = dispatcher
	eventsClient := loop.NewEventsClient(client, cfg.WorkerEventTypes, cfg.WorkerProjects)

	var startCursor int64
	if cursor != nil {
		startCursor = cursor.Seq
	} else {
		// Fresh install or lost/corrupt state volume (plan §2.1): backfill synthetic TRIAGE jobs
		// for unresolved/unclaimed issues, seed the held-claims view, and seek the feed's current
		// head -- NEVER page the feed from seq 0, which would replay the entire org activity
		// history as jobs.
		lister := loop.NewIssuesLister(client)
		result, err := loop.Bootstrap(ctx, lister, eventsClient, enqueuer, cfg.WorkerBackfillHours, logger)
		for err != nil {
			logger.Error("bootstrap sweep failed, retrying", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(cfg.WorkerPollInterval):
			}
			result, err = loop.Bootstrap(ctx, lister, eventsClient, enqueuer, cfg.WorkerBackfillHours, logger)
		}
		startCursor = result.HeadSeq
		if st != nil {
			// plan §7: "/metrics counts bootstrap-skipped issues rather than dropping them
			// silently" -- MetricBootstrapEnqueued is the synthetic jobs actually backfilled;
			// MetricBootstrapSkipped is issues Bootstrap deliberately left to the feed (step 4's
			// sweep-window-delta), kept as a SEPARATE counter (validator finding: these two used to
			// be conflated under one name). MetricHeldClaimsAtBootstrap makes step 2's
			// previously-discarded held-claims view observable, ahead of the N8d sweep that will
			// consume it.
			st.Inc(health.MetricBootstrapEnqueued, int64(result.BootstrapJobCount))
			st.Inc(health.MetricBootstrapSkipped, int64(result.SkippedCount))
			st.SetGauge(health.MetricHeldClaimsAtBootstrap, int64(len(result.HeldClaimIssueIDs)))
		}
		logger.Info("bootstrap sweep complete",
			"bootstrapJobs", result.BootstrapJobCount,
			"heldClaims", len(result.HeldClaimIssueIDs),
			"headSeq", result.HeadSeq,
		)
		// Persist the head-seeked cursor immediately: a crash between Bootstrap returning and the
		// first real PollOnce persist must not re-run Bootstrap (its jobIds dedupe safely, but the
		// feed head-seek itself is wasted work every restart otherwise).
		if err := state.SaveCursor(cursorPath, startCursor); err != nil {
			logger.Error("failed to persist post-bootstrap cursor", "error", err, "path", cursorPath)
		} else if st != nil {
			st.NoteCursorSaved(time.Now())
		}
	}

	poller := &loop.PollLoop{
		Client:     eventsClient,
		Journal:    journal,
		CursorPath: cursorPath,
		MyAgentID:  agentID,
		Enqueue:    enqueuer,
		Interval:   cfg.WorkerPollInterval,
		Jitter:     cfg.WorkerPollJitter,
		Log:        logger,
	}
	poller.SetCursor(startCursor)
	if st != nil {
		poller.OnCursorSaved = st.NoteCursorSaved
		poller.OnEvent = func(loop.Event, loop.Kind) { st.Inc(health.MetricEventsConsumed, 1) }
		poller.OnPageDrained = func(pageEvents int, hasMore bool) {
			if hasMore {
				st.SetGauge(health.MetricCursorLag, int64(pageEvents))
			} else {
				st.SetGauge(health.MetricCursorLag, 0)
			}
		}
		// The startup window (runWorker) is intentionally left in place rather than re-narrowed
		// here to cursorFreshnessMultiple*Interval -- see cursorFreshnessStartupWindow's doc.
	}

	logger.Info("starting poll loop",
		"workerExecute", cfg.WorkerExecute,
		"pollInterval", cfg.WorkerPollInterval,
		"startCursor", poller.Cursor(),
	)
	poller.Run(ctx)
}

// journalRetention is the plan §2.2 "rewrite the journal dropping records of jobs terminal for >7
// days" window, shared by both journal Compact and agent-log reaping (plan §2.2's agent-logs
// retention "mirrors journal Compact's 7-day reaping").
const journalRetention = 7 * 24 * time.Hour

// journalMaintenanceInterval is how often runPipeline re-runs Compact/RecoveryScan/ReapAgentLogs
// after the startup pass (plan §2.2: "on start and daily").
const journalMaintenanceInterval = 24 * time.Hour

// runJournalMaintenance performs one pass of Recovery-observability + Cleanup (plan §2.2/§6,
// CONTEXT.md "Recovery"): it scans the journal for in-flight jobs left by a crash and logs them
// (the actual replay/resume of those jobs happens separately, once, in runPipeline via
// runner.Resume — this function also runs on the DAILY maintenance loop where re-resuming would be
// wrong, so it only observes and logs here), compacts old-terminal records out of the journal, and
// reaps/truncates agent-logs for terminal jobs using the SAME "terminal view" both operations need
// (Journal.LatestByJobID), so a job's terminal-ness and its terminal-at time are computed once and
// used consistently by both Compact and ReapAgentLogs.
func runJournalMaintenance(journal *state.Journal, stateDir string, agentLogMaxMB int, logger *slog.Logger, st *health.Status) {
	// Both RecoveryScan and Compact below run their own Load over the SAME on-disk file within
	// this one maintenance pass, so they observe the same corrupt lines. The health counter is
	// incremented once, from Compact's count, at the point those lines are actually erased by the
	// rewrite -- the moment the loss becomes permanent and therefore worth counting. RecoveryScan's
	// count is still logged here (not discarded, per the validator finding) so an operator sees the
	// corruption at recovery time too, before compaction has run.
	inFlight, corrupt, err := journal.RecoveryScan()
	if corrupt > 0 {
		logger.Warn("recovery scan found corrupt journal lines (skipped, not fatal)", "corrupt_lines", corrupt)
	}
	if err != nil {
		logger.Error("recovery scan failed", "error", err)
	} else if len(inFlight) > 0 {
		ids := make([]string, 0, len(inFlight))
		for _, job := range inFlight {
			ids = append(ids, job.JobID+":"+string(job.State))
		}
		logger.Warn("recovery: in-flight jobs found from a prior run, journaled decisions will be replayed verbatim (no Advisor re-consult)",
			"count", len(inFlight), "jobs", ids)
	} else {
		logger.Info("recovery scan found no in-flight jobs")
	}

	cutoff := time.Now().Add(-journalRetention)
	if compactCorrupt, err := journal.Compact(cutoff); err != nil {
		logger.Error("journal compaction failed", "error", err)
	} else if compactCorrupt > 0 {
		// These corrupt lines were skipped by Compact's own Load and are now permanently erased
		// by the rewrite (state.Journal.Compact's doc comment) -- log the loss explicitly, don't
		// let it happen silently.
		logger.Warn("journal compaction permanently dropped corrupt lines", "corrupt_lines", compactCorrupt)
		if st != nil {
			st.Inc(health.MetricJournalCorruptLines, int64(compactCorrupt))
		}
	}

	latest, err := journal.LatestByJobID()
	if err != nil {
		logger.Error("reading journal for agent-log reaping failed", "error", err)
		return
	}
	terminalJobIDs := make(map[string]time.Time, len(latest))
	for jobID, rec := range latest {
		if rec.State.IsTerminal() {
			terminalJobIDs[jobID] = rec.At
		}
	}
	deleted, truncated, err := state.ReapAgentLogs(stateDir, terminalJobIDs, cutoff, int64(agentLogMaxMB)*1024*1024)
	if err != nil {
		logger.Error("agent-log reaping failed", "error", err)
		return
	}
	if deleted > 0 || truncated > 0 {
		logger.Info("agent-log reaping complete", "deleted", deleted, "truncated", truncated)
	}
}

// journalMaintenanceLoop re-runs runJournalMaintenance on journalMaintenanceInterval until ctx is
// cancelled (plan §2.2: "on start and daily"). The startup pass is run by the caller before this
// loop starts, so this loop only fires the SUBSEQUENT daily passes.
func journalMaintenanceLoop(ctx context.Context, journal *state.Journal, stateDir string, agentLogMaxMB int, logger *slog.Logger, st *health.Status) {
	ticker := time.NewTicker(journalMaintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runJournalMaintenance(journal, stateDir, agentLogMaxMB, logger, st)
		}
	}
}

// buildRunner assembles a loop.Runner from config and the already-resolved seams (client, journal,
// agent identity). Extracted from runPipeline so the safety-critical WORKER_EXECUTE -> DryRun gate
// (plan §5/§6: EXECUTE=false = dry-run, journal decisions, never send mutating calls) can be
// exercised directly by a unit test without standing up the health server or the poll loop.
func buildRunner(cfg Config, client *sentinel.Client, journal *state.Journal, agentID string, logger *slog.Logger) *loop.Runner {
	return &loop.Runner{
		Journal:   journal,
		Issues:    loop.HTTPIssueReader{Client: client},
		Claims:    loop.HTTPClaimer{Client: client},
		Advisor:   jobs.StubAdvisor{}, // real Advisors ship in N8d
		Act:       jobs.NotImplementedActor{},
		DryRun:    !cfg.WorkerExecute,
		MyAgentID: agentID,
		Log:       logger,
	}
}

// runHealthcheck implements the `-healthcheck` subcommand (plan §6): GET
// http://localhost<WORKER_HEALTH_ADDR>/healthz and exit 0/1, for the container's own HEALTHCHECK
// instruction. It reads WORKER_HEALTH_ADDR from the environment (same variable the running process
// itself binds), defaulting to :9090 like LoadConfig does.
// defaultHealthPort mirrors LoadConfig's WORKER_HEALTH_ADDR default (":9090") -- healthcheckURL
// falls back to it whenever addr carries no discoverable port at all.
const defaultHealthPort = "9090"

// healthcheckURL builds the self-probe URL from a WORKER_HEALTH_ADDR-shaped listen address. It
// always probes localhost regardless of the bind host in addr (WORKER_HEALTH_ADDR configures what
// the server binds to, not where the same-container probe should connect), so a host component
// like "0.0.0.0:9090" or "0.0.0.0" must be stripped rather than concatenated verbatim. Three forms
// are accepted: "host:port" and ":port" (both handled by net.SplitHostPort -- the host, if any, is
// discarded), a bare port with no colon at all (e.g. "9090"), and a bare host with NO port (e.g.
// "myhost") -- the last form previously fell through to the bare-port branch and produced
// "http://localhost:myhost/healthz" (the hostname used as if it were a port number, which no
// server would ever answer on); it now falls back to defaultHealthPort instead, since the probe
// only ever wants localhost's own port regardless of what host WORKER_HEALTH_ADDR names.
func healthcheckURL(addr string) string {
	if addr == "" {
		return "http://localhost:" + defaultHealthPort + "/healthz"
	}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		if port == "" {
			port = defaultHealthPort
		}
		return "http://localhost:" + port + "/healthz"
	}
	// No colon in addr: either a bare port ("9090") or a bare host with no port ("myhost", or a
	// dotted host like "worker.internal"). Only the digits-only case is actually a port.
	if _, err := strconv.Atoi(addr); err == nil {
		return "http://localhost:" + addr + "/healthz"
	}
	return "http://localhost:" + defaultHealthPort + "/healthz"
}

func runHealthcheck(args []string, stderr io.Writer) int {
	addr := os.Getenv("WORKER_HEALTH_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	url := healthcheckURL(addr)
	if len(args) > 0 && args[0] != "" {
		url = args[0]
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(stderr, "healthcheck: GET %s failed: %v\n", url, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(stderr, "healthcheck: GET %s returned %d\n", url, resp.StatusCode)
		return 1
	}
	return 0
}
