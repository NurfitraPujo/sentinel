package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/health"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

func fakeEnv(kv map[string]string) envLookup {
	return func(key string) (string, bool) {
		v, ok := kv[key]
		return v, ok
	}
}

// TestLoadConfig_DurationNotationParsesWithZeroErrors proves the plan §5 duration knobs accept
// Go's time.ParseDuration notation (the notation plan §5 itself uses: "10s", "3m", "30m", "12h")
// rather than bare integer seconds — a config using the plan's own literals must not fail
// validation.
func TestLoadConfig_DurationNotationParsesWithZeroErrors(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_ENABLED":           "false",
		"WORKER_POLL_INTERVAL":     "10s",
		"WORKER_TRIAGE_TIMEOUT":    "3m",
		"WORKER_FIX_TIMEOUT":       "30m",
		"WORKER_CLAIM_HEARTBEAT":   "12h",
		"WORKER_SNAPSHOT_INTERVAL": "5m",
		"WORKER_REPO_REFRESH":      "15m",
		"WORKER_SWEEP_INTERVAL":    "1h",
	}))
	if len(errs) != 0 {
		t.Fatalf("expected zero validation errors for plan §5 duration literals, got: %v", errs)
	}
	if cfg.WorkerPollInterval != 10*time.Second {
		t.Errorf("WORKER_POLL_INTERVAL = %s, want 10s", cfg.WorkerPollInterval)
	}
	if cfg.WorkerTriageTimeout != 3*time.Minute {
		t.Errorf("WORKER_TRIAGE_TIMEOUT = %s, want 3m", cfg.WorkerTriageTimeout)
	}
	if cfg.WorkerFixTimeout != 30*time.Minute {
		t.Errorf("WORKER_FIX_TIMEOUT = %s, want 30m", cfg.WorkerFixTimeout)
	}
	if cfg.WorkerClaimHeartbeat != 12*time.Hour {
		t.Errorf("WORKER_CLAIM_HEARTBEAT = %s, want 12h", cfg.WorkerClaimHeartbeat)
	}
	if cfg.WorkerSnapshotInterval != 5*time.Minute {
		t.Errorf("WORKER_SNAPSHOT_INTERVAL = %s, want 5m", cfg.WorkerSnapshotInterval)
	}
	if cfg.WorkerRepoRefresh != 15*time.Minute {
		t.Errorf("WORKER_REPO_REFRESH = %s, want 15m", cfg.WorkerRepoRefresh)
	}
	if cfg.WorkerSweepInterval != time.Hour {
		t.Errorf("WORKER_SWEEP_INTERVAL = %s, want 1h", cfg.WorkerSweepInterval)
	}
}

// TestLoadConfig_WorkerShutdownTimeoutDefaultAndOverride proves WORKER_SHUTDOWN_TIMEOUT (finding
// 1's bounded drain-on-shutdown window) defaults to 30s and is overridable via the same Go
// duration notation as every other §5 knob.
func TestLoadConfig_WorkerShutdownTimeoutDefaultAndOverride(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(nil))
	if len(errs) != 0 {
		t.Fatalf("expected zero validation errors on an empty environment, got: %v", errs)
	}
	if cfg.WorkerShutdownTimeout != 30*time.Second {
		t.Errorf("WORKER_SHUTDOWN_TIMEOUT default = %s, want 30s", cfg.WorkerShutdownTimeout)
	}

	cfg, errs = LoadConfig(fakeEnv(map[string]string{"WORKER_SHUTDOWN_TIMEOUT": "45s"}))
	if len(errs) != 0 {
		t.Fatalf("expected zero validation errors for WORKER_SHUTDOWN_TIMEOUT=45s, got: %v", errs)
	}
	if cfg.WorkerShutdownTimeout != 45*time.Second {
		t.Errorf("WORKER_SHUTDOWN_TIMEOUT = %s, want 45s", cfg.WorkerShutdownTimeout)
	}
}

// TestLoadConfig_WorkerShutdownTimeoutMustBePositive proves a non-positive override is rejected
// (mirrors WORKER_SWEEP_INTERVAL/WORKER_POLL_INTERVAL's own >0 validation) rather than silently
// producing a zero-wait drain.
func TestLoadConfig_WorkerShutdownTimeoutMustBePositive(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{"WORKER_SHUTDOWN_TIMEOUT": "0s"}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_SHUTDOWN_TIMEOUT") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_SHUTDOWN_TIMEOUT validation error for 0s, got: %v", errs)
	}
}

// TestLoadConfig_InvalidDurationIsCollectedAsAnError proves a genuinely malformed duration still
// produces a validation error (not silently ignored) and falls back to the default.
func TestLoadConfig_InvalidDurationIsCollectedAsAnError(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_ENABLED":       "false",
		"WORKER_POLL_INTERVAL": "not-a-duration",
	}))
	if cfg.WorkerPollInterval != 10*time.Second {
		t.Errorf("expected default 10s on invalid input, got %s", cfg.WorkerPollInterval)
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_POLL_INTERVAL") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_POLL_INTERVAL validation error, got: %v", errs)
	}
}

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(nil))
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors on an empty (gate-off) environment, got: %v", errs)
	}
	if cfg.WorkerEnabled {
		t.Errorf("WORKER_ENABLED default should be false")
	}
	if cfg.WorkerExecute {
		t.Errorf("WORKER_EXECUTE default should be false (dry-run)")
	}
	if !cfg.WorkerFixEnabled {
		t.Errorf("WORKER_FIX_ENABLED default should be true (deployment kill switch defaults on; policy is server-side)")
	}
	if cfg.WorkerPollInterval != 10*time.Second {
		t.Errorf("WORKER_POLL_INTERVAL default = %s, want 10s", cfg.WorkerPollInterval)
	}
	if cfg.WorkerPollJitter != 0.2 {
		t.Errorf("WORKER_POLL_JITTER default = %v, want 0.2", cfg.WorkerPollJitter)
	}
	if cfg.WorkerConcurrency != 2 {
		t.Errorf("WORKER_CONCURRENCY default = %d, want 2", cfg.WorkerConcurrency)
	}
	if cfg.WorkerTriageMaxTurns != 6 || cfg.WorkerFollowupMaxTurns != 4 {
		t.Errorf("turn caps = %d/%d, want 6/4", cfg.WorkerTriageMaxTurns, cfg.WorkerFollowupMaxTurns)
	}
	if cfg.WorkerFixConfidence != 0.7 {
		t.Errorf("WORKER_FIX_CONFIDENCE default = %v, want 0.7", cfg.WorkerFixConfidence)
	}
	if cfg.WorkerClaimHeartbeat != 12*time.Hour {
		t.Errorf("WORKER_CLAIM_HEARTBEAT default = %s, want 12h", cfg.WorkerClaimHeartbeat)
	}
	if cfg.WorkerSnapshotBackend != "none" {
		t.Errorf("WORKER_SNAPSHOT_BACKEND default = %q, want none", cfg.WorkerSnapshotBackend)
	}
	if cfg.WorkerKeystore != "file" {
		t.Errorf("WORKER_KEYSTORE default = %q, want file", cfg.WorkerKeystore)
	}
	if cfg.WorkerHealthAddr != ":9090" {
		t.Errorf("WORKER_HEALTH_ADDR default = %q, want :9090", cfg.WorkerHealthAddr)
	}
	if cfg.WorkerRotateBeforeHours != 72 || cfg.WorkerRotateEveryDays != 30 {
		t.Errorf("rotation defaults = %d/%d, want 72/30", cfg.WorkerRotateBeforeHours, cfg.WorkerRotateEveryDays)
	}
	if cfg.LLMProvider != "openai" {
		t.Errorf("LLM_PROVIDER default = %q, want openai", cfg.LLMProvider)
	}
}

// TestLoadConfig_RequiresSentinelURLWhenEnabled proves the "collected, not fatal-on-first"
// validation contract: enabling the worker without SENTINEL_URL/KEY set must surface BOTH
// problems in one call, not just the first one encountered.
func TestLoadConfig_RequiresSentinelURLWhenEnabled(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_ENABLED": "true",
	}))
	if !cfg.WorkerEnabled {
		t.Fatalf("expected WorkerEnabled true")
	}
	foundURL, foundKey := false, false
	for _, e := range errs {
		if strings.Contains(e, "SENTINEL_URL") {
			foundURL = true
		}
		if strings.Contains(e, "SENTINEL_AGENT_KEY") {
			foundKey = true
		}
	}
	if !foundURL || !foundKey {
		t.Fatalf("expected both SENTINEL_URL and SENTINEL_AGENT_KEY errors, got: %v", errs)
	}
}

// TestLoadConfig_CollectsMultipleErrors proves several unrelated bad values are ALL reported
// together, not just the first.
func TestLoadConfig_CollectsMultipleErrors(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_POLL_JITTER":      "5",     // out of [0,1]
		"WORKER_CONCURRENCY":      "0",     // must be >= 1
		"WORKER_FIX_CONFIDENCE":   "abc",   // invalid float -> falls back to default, also errors
		"WORKER_SNAPSHOT_BACKEND": "azure", // invalid enum
		"WORKER_KEYSTORE":         "vault", // invalid enum
		"LLM_PROVIDER":            "grok",  // invalid enum
	}))
	if len(errs) < 6 {
		t.Fatalf("expected at least 6 distinct validation errors collected together, got %d: %v", len(errs), errs)
	}
}

func TestLoadConfig_ClaimHeartbeatMustBeBelowStaleHours(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_CLAIM_HEARTBEAT": "90000", // 25h > server's 24h CLAIM_STALE_HOURS
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_CLAIM_HEARTBEAT") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_CLAIM_HEARTBEAT validation error, got: %v", errs)
	}
}

func TestLoadConfig_S3BackendRequiresBucketAndEndpoint(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_SNAPSHOT_BACKEND": "s3",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "S3_BUCKET") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an S3 bucket/endpoint validation error, got: %v", errs)
	}
}

func TestLoadConfig_KubernetesSecretKeystoreRequiresNameAndNamespace(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_KEYSTORE": "kubernetes-secret",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_KEY_SECRET_NAME") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a kubernetes-secret keystore validation error, got: %v", errs)
	}
}

func TestLoadConfig_EventTypesAndProjectsListParsing(t *testing.T) {
	cfg, _ := LoadConfig(fakeEnv(map[string]string{
		"WORKER_EVENT_TYPES": "created, commented ,question_answered",
		"WORKER_PROJECTS":    "proj-a,proj-b",
	}))
	if len(cfg.WorkerEventTypes) != 3 || cfg.WorkerEventTypes[1] != "commented" {
		t.Fatalf("WORKER_EVENT_TYPES parsed as %#v", cfg.WorkerEventTypes)
	}
	if len(cfg.WorkerProjects) != 2 {
		t.Fatalf("WORKER_PROJECTS parsed as %#v", cfg.WorkerProjects)
	}
}

// TestLoadConfig_NonPositivePollIntervalIsRejected proves WORKER_POLL_INTERVAL <= 0 is collected as
// a validation error rather than silently accepted, which would otherwise drive an unthrottled
// request loop against the operator's own Sentinel API (loop.PollLoop.jitteredSleep returns
// immediately for base <= 0, and runPipeline's identity-retry loop also uses this interval).
func TestLoadConfig_NonPositivePollIntervalIsRejected(t *testing.T) {
	for _, v := range []string{"0s", "0", "-1s"} {
		_, errs := LoadConfig(fakeEnv(map[string]string{
			"WORKER_POLL_INTERVAL": v,
		}))
		found := false
		for _, e := range errs {
			if strings.Contains(e, "WORKER_POLL_INTERVAL") {
				found = true
			}
		}
		if !found {
			t.Errorf("WORKER_POLL_INTERVAL=%q: expected a validation error, got: %v", v, errs)
		}
	}
}

// TestLoadConfig_NonPositiveSweepIntervalIsRejected mirrors the poll-interval guard for
// WORKER_SWEEP_INTERVAL, the other duration wired directly into a loop.
func TestLoadConfig_NonPositiveSweepIntervalIsRejected(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_SWEEP_INTERVAL": "0s",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_SWEEP_INTERVAL") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_SWEEP_INTERVAL validation error, got: %v", errs)
	}
}

// TestLoadConfig_WorkerExecuteTrueIsRejected proves the validator's second finding is closed:
// N8a wires buildRunner to jobs.NotImplementedActor{} (no real Actor exists until a later phase),
// so WORKER_EXECUTE=true must never reach the running pipeline — it would claim every triageable
// issue for real (loop/runner.go's ensure-claimed gates only on DryRun) and only then fail at
// NotImplementedActor.Act, leaving the claims held with no release path. LoadConfig must reject it
// at validation time rather than let buildRunner silently flip DryRun=false.
func TestLoadConfig_WorkerExecuteTrueIsRejected(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_EXECUTE": "true",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_EXECUTE") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_EXECUTE validation error when WORKER_EXECUTE=true, got: %v", errs)
	}
}

// TestLoadConfig_WorkerExecuteFalseIsAccepted proves the guard above is specific to true — the
// documented default/supported dry-run mode must not trip any new validation error.
func TestLoadConfig_WorkerExecuteFalseIsAccepted(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_EXECUTE": "false",
	}))
	for _, e := range errs {
		if strings.Contains(e, "WORKER_EXECUTE") {
			t.Fatalf("WORKER_EXECUTE=false: unexpected validation error: %v", e)
		}
	}
}

// --- buildRunner / DryRun gate ---

// TestBuildRunner_DryRunGate proves the single most safety-critical wiring in N8a: WORKER_EXECUTE
// controls loop.Runner.DryRun, inverted. WORKER_EXECUTE=false (or unset) must produce DryRun=true
// (journal decisions, never send mutating calls); WORKER_EXECUTE=true must produce DryRun=false.
func TestBuildRunner_DryRunGate(t *testing.T) {
	cases := []struct {
		name          string
		workerExecute bool
		wantDryRun    bool
	}{
		{"unset/false defaults to dry-run", false, true},
		{"true disables dry-run", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{WorkerExecute: tc.workerExecute}
			client := sentinel.NewClient("http://example.invalid", "key")
			journal := state.OpenJournal(t.TempDir() + "/jobs.journal")
			runner := buildRunner(cfg, client, journal, "agent-1", slog.New(slog.NewTextHandler(io.Discard, nil)))
			if runner.DryRun != tc.wantDryRun {
				t.Fatalf("WorkerExecute=%v: runner.DryRun = %v, want %v", tc.workerExecute, runner.DryRun, tc.wantDryRun)
			}
		})
	}
}

// --- runJournalMaintenance (recovery + cleanup wiring, validator finding: Compact/RecoveryScan/
// ReapAgentLogs were reachable only from their own package tests, never from the running worker) ---

// TestRunJournalMaintenance_CompactsStaleAndSurfacesInFlight seeds a journal with (1) an old
// terminal job that Compact must drop and (2) a job crashed mid-flight in "acting" whose decision
// was journaled on an earlier "advised" record — the exact in-flight shape RecoveryScan must surface
// (with its carried-forward payload) so a restart can replay it without re-consulting the Advisor.
// It proves runJournalMaintenance actually invokes Compact and RecoveryScan end to end, not just
// that those methods work in isolation.
func TestRunJournalMaintenance_CompactsStaleAndSurfacesInFlight(t *testing.T) {
	dir := t.TempDir()
	journalPath := dir + "/jobs.journal"
	journal := state.OpenJournal(journalPath)

	old := time.Now().Add(-30 * 24 * time.Hour)
	must(t, journal.Append(state.Record{JobID: "stale-done", IssueID: "i1", Kind: "triage", State: state.StateQueued, At: old}))
	must(t, journal.Append(state.Record{JobID: "stale-done", IssueID: "i1", Kind: "triage", State: state.StateDone, At: old}))

	decision := []byte(`{"disposition":"fixable"}`)
	must(t, journal.Append(state.Record{JobID: "crashed-job", IssueID: "i2", Kind: "triage", State: state.StateQueued}))
	must(t, journal.Append(state.Record{JobID: "crashed-job", IssueID: "i2", Kind: "triage", State: state.StateAdvised, Payload: decision}))
	must(t, journal.Append(state.Record{JobID: "crashed-job", IssueID: "i2", Kind: "triage", State: state.StateActing}))

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	runJournalMaintenance(journal, dir, 10, logger, nil)

	// Compact must have dropped the stale terminal job's records.
	records, _, err := journal.Load()
	if err != nil {
		t.Fatalf("Load after maintenance: %v", err)
	}
	for _, r := range records {
		if r.JobID == "stale-done" {
			t.Fatalf("expected stale-done's records to be compacted away, found: %+v", r)
		}
	}

	// RecoveryScan must have surfaced the in-flight job with its carried-forward decision payload,
	// and runJournalMaintenance must have logged it (no silent recovery).
	logged := logBuf.String()
	if !strings.Contains(logged, "crashed-job") {
		t.Fatalf("expected recovery scan to log the in-flight job crashed-job, got log:\n%s", logged)
	}
	if !strings.Contains(logged, "in-flight jobs found") {
		t.Fatalf("expected a recovery log line, got:\n%s", logged)
	}
}

// TestRunJournalMaintenance_SurfacesCorruptLinesToMetrics proves the validator's finding is fixed
// end to end: a corrupt journal line is not just skipped in-package, it is counted at
// health.MetricJournalCorruptLines where an operator can actually see it (/metrics), and it is
// logged before Compact's rewrite permanently erases it.
func TestRunJournalMaintenance_SurfacesCorruptLinesToMetrics(t *testing.T) {
	dir := t.TempDir()
	journalPath := dir + "/jobs.journal"
	journal := state.OpenJournal(journalPath)
	must(t, journal.Append(state.Record{JobID: "good", IssueID: "i1", Kind: "triage", State: state.StateQueued}))

	f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("opening for corruption: %v", err)
	}
	if _, err := f.Write([]byte("{not valid json\n")); err != nil {
		t.Fatalf("writing corrupt line: %v", err)
	}
	must(t, f.Close())

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	st := health.NewStatus()

	runJournalMaintenance(journal, dir, 10, logger, st)

	logged := logBuf.String()
	if !strings.Contains(logged, "corrupt") {
		t.Fatalf("expected a log line mentioning the corrupt journal line, got:\n%s", logged)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	health.Handler(st).ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "journal_corrupt_lines") {
		t.Fatalf("expected /metrics to expose journal_corrupt_lines, got:\n%s", body)
	}
	if strings.Contains(body, "journal_corrupt_lines 0") {
		t.Fatalf("expected journal_corrupt_lines to be nonzero, got:\n%s", body)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- healthcheckURL ---

// TestHealthcheckURL_TableDriven covers all three WORKER_HEALTH_ADDR-shaped forms healthcheckURL
// must handle (finding 6): "host:port", ":port" (bind-all), and a bare host with NO port at all --
// the last form previously mishandled the host as if it were the port number
// (e.g. "myhost" -> "http://localhost:myhost/healthz", which no server answers on).
func TestHealthcheckURL_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"bind-all with port", ":9090", "http://localhost:9090/healthz"},
		{"host:port, host discarded", "0.0.0.0:9090", "http://localhost:9090/healthz"},
		{"loopback host:port", "127.0.0.1:9090", "http://localhost:9090/healthz"},
		{"dns host:port", "sentinel-worker:9090", "http://localhost:9090/healthz"},
		{"bare port, no colon", "9090", "http://localhost:9090/healthz"},
		{"bare host, NO port at all", "myhost", "http://localhost:" + defaultHealthPort + "/healthz"},
		{"bare dotted host, NO port", "worker.internal", "http://localhost:" + defaultHealthPort + "/healthz"},
		{"empty addr falls back to default", "", "http://localhost:" + defaultHealthPort + "/healthz"},
		{"colon with empty port falls back to default", ":", "http://localhost:" + defaultHealthPort + "/healthz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := healthcheckURL(tc.addr); got != tc.want {
				t.Errorf("healthcheckURL(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestRunHealthcheck_ProductionURLPath drives runHealthcheck's REAL production code path — building
// the URL from WORKER_HEALTH_ADDR (env) with no argument override, exactly as the container's
// HEALTHCHECK instruction invokes it (plan §6 passes no argument). Previous coverage only exercised
// the args[0] override branch used solely for testability.
func TestRunHealthcheck_ProductionURLPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("splitting httptest server addr: %v", err)
	}
	if host != "127.0.0.1" && host != "localhost" {
		t.Skipf("httptest server bound to %q, not loopback; skipping production-path probe", host)
	}

	t.Setenv("WORKER_HEALTH_ADDR", ":"+port)
	var stderr bytes.Buffer
	code := runHealthcheck(nil, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 against a live health server via WORKER_HEALTH_ADDR, got %d (stderr: %s)", code, stderr.String())
	}
}

// TestRunHealthcheck_ProductionURLPath_Unreachable proves the same production path exits 1 when
// nothing is listening on the configured WORKER_HEALTH_ADDR.
func TestRunHealthcheck_ProductionURLPath_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("splitting httptest server addr: %v", err)
	}
	srv.Close()

	t.Setenv("WORKER_HEALTH_ADDR", ":"+port)
	var stderr bytes.Buffer
	code := runHealthcheck(nil, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 when nothing is listening, got %d", code)
	}
}

// --- healthcheck subcommand ---

func TestRunHealthcheck_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var stderr bytes.Buffer
	code := runHealthcheck([]string{srv.URL + "/healthz"}, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0 for a healthy server, got %d (stderr: %s)", code, stderr.String())
	}
}

func TestRunHealthcheck_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var stderr bytes.Buffer
	code := runHealthcheck([]string{srv.URL + "/healthz"}, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 for a 503 response, got %d", code)
	}
}

func TestRunHealthcheck_ConnectionRefused(t *testing.T) {
	var stderr bytes.Buffer
	// A closed server: guaranteed nothing is listening.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/healthz"
	srv.Close()

	code := runHealthcheck([]string{url}, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1 when the health server is unreachable, got %d", code)
	}
	if stderr.Len() == 0 {
		t.Errorf("expected a diagnostic message on stderr")
	}
}

// --- WORKER_ENABLED in-process gate (finding 4) ---

// TestWorkerEnabledGate_PipelineDoesNotStart proves the in-process WORKER_ENABLED gate: with
// WorkerEnabled=false, runApp (main's pipeline entry point past config loading) must NOT start the
// poll loop -- zero requests may reach the configured SENTINEL_URL for the entire lifetime of the
// call, even though SentinelURL/AgentKey/PollInterval are all otherwise valid. This is the same
// gate main() applies before ever calling runWorker.
func TestWorkerEnabledGate_PipelineDoesNotStart(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"agentId":"agent-1"}`))
	}))
	defer srv.Close()

	cfg := Config{
		WorkerEnabled:      false,
		SentinelURL:        srv.URL,
		SentinelAgentKey:   "test-key",
		WorkerPollInterval: 5 * time.Millisecond,
		WorkerHealthAddr:   "127.0.0.1:0",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("expected ZERO requests to reach the sentinel API while WORKER_ENABLED=false, got %d", got)
	}
}

// TestWorkerEnabledGate_PipelineStartsWhenEnabled is the inverse proof: the SAME config with
// WorkerEnabled=true DOES drive at least one request against the test server (via
// resolveAgentID's GET /api/agent/self during runPipeline), demonstrating the gate test above is
// discriminating on WorkerEnabled and not on some other reason requests never arrive (e.g. a
// broken test server or a ctx that never lets the pipeline run at all).
func TestWorkerEnabledGate_PipelineStartsWhenEnabled(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"agentId":"agent-1"}`))
	}))
	defer srv.Close()

	cfg := Config{
		WorkerEnabled:      true,
		SentinelURL:        srv.URL,
		SentinelAgentKey:   "test-key",
		WorkerPollInterval: 5 * time.Millisecond,
		WorkerHealthAddr:   "127.0.0.1:0",
		WorkerStateDir:     t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Fatalf("expected at least one request to reach the sentinel API while WORKER_ENABLED=true, got 0 -- the gate test proves nothing if this control case can't start the pipeline either")
	}
}

// --- WORKER_WORKSPACE_DIR trust boundary (finding 2) ---

// TestLoadConfig_WorkspaceDirDefaultIsNotUnderStateDir proves the documented default
// (plan §4.5: Fix Executor workspaces must never share a tree with WORKER_STATE_DIR) is actually a
// sibling, not a subdirectory of the default state dir.
func TestLoadConfig_WorkspaceDirDefaultIsNotUnderStateDir(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(nil))
	if len(errs) != 0 {
		t.Fatalf("expected no validation errors on an empty environment, got: %v", errs)
	}
	if underDir(cfg.WorkerWorkspaceDir, cfg.WorkerStateDir) {
		t.Fatalf("default WORKER_WORKSPACE_DIR=%q must not be under default WORKER_STATE_DIR=%q", cfg.WorkerWorkspaceDir, cfg.WorkerStateDir)
	}
	if underDir(cfg.WorkerRepoCacheDir, cfg.WorkerStateDir) {
		t.Fatalf("default WORKER_REPO_CACHE_DIR=%q must not be under default WORKER_STATE_DIR=%q", cfg.WorkerRepoCacheDir, cfg.WorkerStateDir)
	}
}

// TestLoadConfig_WorkspaceDirUnderStateDirIsRejected proves the startup validation error fires
// when an operator misconfigures WORKER_WORKSPACE_DIR (or WORKER_REPO_CACHE_DIR) inside
// WORKER_STATE_DIR -- the Fix Executor workspace trust boundary (plan §4.4/§4.5) must not be
// reopened by a bad config value either.
func TestLoadConfig_WorkspaceDirUnderStateDirIsRejected(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_STATE_DIR":     "/var/lib/sentinel-worker",
		"WORKER_WORKSPACE_DIR": "/var/lib/sentinel-worker/workspaces",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_WORKSPACE_DIR") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_WORKSPACE_DIR validation error, got: %v", errs)
	}
}

func TestLoadConfig_RepoCacheDirUnderStateDirIsRejected(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_STATE_DIR":      "/var/lib/sentinel-worker",
		"WORKER_REPO_CACHE_DIR": "/var/lib/sentinel-worker/repos",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_REPO_CACHE_DIR") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_REPO_CACHE_DIR validation error, got: %v", errs)
	}
}

func TestLoadConfig_WorkspaceDirEqualToStateDirIsRejected(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_STATE_DIR":     "/var/lib/sentinel-worker",
		"WORKER_WORKSPACE_DIR": "/var/lib/sentinel-worker",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_WORKSPACE_DIR") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_WORKSPACE_DIR validation error when it equals WORKER_STATE_DIR exactly, got: %v", errs)
	}
}

// --- LLM_FALLBACK_PROVIDER / budget validation symmetry (finding 7) ---

func TestLoadConfig_LLMFallbackProviderEnumChecked(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"LLM_FALLBACK_PROVIDER": "bogus",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "LLM_FALLBACK_PROVIDER") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an LLM_FALLBACK_PROVIDER validation error, got: %v", errs)
	}
}

func TestLoadConfig_LLMFallbackProviderEmptyIsValid(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(nil))
	for _, e := range errs {
		if strings.Contains(e, "LLM_FALLBACK_PROVIDER") {
			t.Fatalf("did not expect an LLM_FALLBACK_PROVIDER error for the empty (no-fallback) default, got: %v", errs)
		}
	}
}

func TestLoadConfig_LLMFallbackProviderValidValuesAccepted(t *testing.T) {
	for _, v := range []string{"openai", "anthropic", "gemini"} {
		_, errs := LoadConfig(fakeEnv(map[string]string{"LLM_FALLBACK_PROVIDER": v}))
		for _, e := range errs {
			if strings.Contains(e, "LLM_FALLBACK_PROVIDER") {
				t.Fatalf("LLM_FALLBACK_PROVIDER=%q: unexpected error: %v", v, errs)
			}
		}
	}
}

// TestLoadConfig_NegativeBudgetKnobsAreRejected proves every budget/volume-cap-like knob is
// range-checked symmetrically (plan §2.6): a negative value is always nonsensical and must be
// collected as a validation error, not silently accepted.
func TestLoadConfig_NegativeBudgetKnobsAreRejected(t *testing.T) {
	knobs := []string{
		"WORKER_DAILY_TOKEN_BUDGET",
		"WORKER_TRIAGE_MAX_TURNS",
		"WORKER_FOLLOWUP_MAX_TURNS",
		"WORKER_MAX_OUTPUT_TOKENS",
		"WORKER_MAX_FIX_ATTEMPTS",
		"WORKER_MAX_FIX_JOBS_PER_DAY",
		"WORKER_MAX_PRS_PER_DAY",
		"WORKER_MAX_TRIAGE_PER_HOUR",
		"WORKER_FIX_MAX_FILES",
		"WORKER_NAG_DAYS",
		"WORKER_WORKSPACE_RETENTION_DAYS",
		"WORKER_AGENT_LOG_MAX_MB",
		"WORKER_ROTATE_BEFORE_HOURS",
		"WORKER_ROTATE_EVERY_DAYS",
	}
	for _, knob := range knobs {
		t.Run(knob, func(t *testing.T) {
			_, errs := LoadConfig(fakeEnv(map[string]string{knob: "-1"}))
			found := false
			for _, e := range errs {
				if strings.Contains(e, knob) {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s=-1: expected a validation error, got: %v", knob, errs)
			}
		})
	}
}

func TestLoadConfig_ZeroBudgetKnobsAreAccepted(t *testing.T) {
	// 0 is a legitimate "no budget configured" / "no cap" sentinel for several of these knobs
	// (e.g. WORKER_DAILY_TOKEN_BUDGET's existing default is 0) -- only negative values are
	// rejected.
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_DAILY_TOKEN_BUDGET": "0",
		"WORKER_MAX_OUTPUT_TOKENS":  "0",
	}))
	for _, e := range errs {
		if strings.Contains(e, "WORKER_DAILY_TOKEN_BUDGET") || strings.Contains(e, "WORKER_MAX_OUTPUT_TOKENS") {
			t.Fatalf("did not expect an error for a zero budget, got: %v", errs)
		}
	}
}

// --- WORKER_PROJECTS multi-value rejection (round-4 finding 4) ---

// TestLoadConfig_MultipleWorkerProjectsIsRejected proves an operator who configures more than one
// WORKER_PROJECTS entry gets a loud validation error instead of the events feed silently applying
// only the first (loop/poll.go's httpEventsClient.GetEvents sends a single `project=` value -- the
// server route reads only one). A single project must still be accepted with zero errors.
func TestLoadConfig_MultipleWorkerProjectsIsRejected(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_PROJECTS": "proj-a,proj-b",
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_PROJECTS") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_PROJECTS validation error for two projects, got: %v", errs)
	}
}

func TestLoadConfig_SingleWorkerProjectIsAccepted(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_PROJECTS": "proj-a",
	}))
	for _, e := range errs {
		if strings.Contains(e, "WORKER_PROJECTS") {
			t.Fatalf("did not expect a WORKER_PROJECTS error for a single project, got: %v", errs)
		}
	}
	if len(cfg.WorkerProjects) != 1 || cfg.WorkerProjects[0] != "proj-a" {
		t.Fatalf("WorkerProjects = %#v, want [proj-a]", cfg.WorkerProjects)
	}
}

// --- /readyz composition wiring, end to end (round-4 finding 1) ---
//
// Prior to this test, three wiring lines in runPipeline were exercised (the pipeline actually ran)
// but never asserted: client.OnAuthStatus = st.SetAuthValid; poller.OnCursorSaved =
// st.NoteCursorSaved; st.SetCursorFreshnessWindow(...). Deleting any one of them left every
// existing suite green because nothing drove a real runApp far enough to observe /readyz's
// composed answer. This test binds WORKER_HEALTH_ADDR to a real 127.0.0.1 ephemeral port, runs
// runApp against a fake sentinel server, and asserts on /readyz's actual HTTP response across the
// pipeline's real lifetime -- not a fake/short-circuited health.Status.
//
// It proves three things, each sensitive to exactly one of the three wiring lines:
//  1. /readyz becomes 200 once the (bootstrap-seeded) cursor is persisted, and STAYS 200 well past
//     the cursor-freshness window as long as the poll loop keeps running -- this requires
//     poller.OnCursorSaved to keep refreshing health.Status after the one-time post-bootstrap
//     NoteCursorSaved call in runPipeline; deleting that wiring line lets the initial freshness
//     lapse and /readyz goes stale-503 on its own even though the server keeps answering normally.
//  2. Once the fake events endpoint starts erroring (so no further cursor saves happen), /readyz
//     eventually reports 503 citing cursor staleness -- this requires
//     st.SetCursorFreshnessWindow(...) to have been called at all; without it, cursorFreshness
//     stays 0 and health.Status.Ready() never applies the staleness check, so /readyz would stay
//     200 forever regardless of how stale the cursor really is.
//  3. Once the fake server starts answering 401, /readyz reports 503 citing invalid auth -- this
//     requires client.OnAuthStatus = st.SetAuthValid; without it, sentinel.Client's 401 signal
//     never reaches health.Status and authValid stays permanently true.
func TestRunApp_ReadyzComposition_EndToEnd(t *testing.T) {
	const (
		modeOK           int32 = 0
		modeServerError  int32 = 1
		modeUnauthorized int32 = 2
	)
	var mode int32 = modeOK

	respond := func(w http.ResponseWriter, okBody string) bool {
		switch atomic.LoadInt32(&mode) {
		case modeUnauthorized:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return false
		case modeServerError:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return false
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(okBody))
			return true
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		respond(w, `{"agentId":"agent-1"}`)
	})
	mux.HandleFunc("/api/agent/issues", func(w http.ResponseWriter, r *http.Request) {
		respond(w, `{"issues":[],"nextCursor":null}`)
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		respond(w, `{"events":[],"hasMore":false,"cursor":0}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	healthAddr := ln.Addr().String()
	ln.Close()

	const pollInterval = 15 * time.Millisecond
	cfg := Config{
		WorkerEnabled:       true,
		WorkerExecute:       false,
		SentinelURL:         srv.URL,
		SentinelAgentKey:    "test-key",
		WorkerPollInterval:  pollInterval,
		WorkerHealthAddr:    healthAddr,
		WorkerStateDir:      t.TempDir(),
		WorkerBackfillHours: 24,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		close(done)
	}()

	readyzURL := "http://" + healthAddr + "/readyz"
	type readyzResult struct {
		status int
		body   string
	}
	getReadyz := func() (readyzResult, error) {
		resp, err := http.Get(readyzURL)
		if err != nil {
			return readyzResult{}, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return readyzResult{status: resp.StatusCode, body: string(b)}, nil
	}
	waitFor := func(timeout time.Duration, cond func(readyzResult) bool) (readyzResult, bool) {
		deadline := time.Now().Add(timeout)
		var last readyzResult
		for time.Now().Before(deadline) {
			if res, err := getReadyz(); err == nil {
				last = res
				if cond(res) {
					return last, true
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		return last, false
	}

	// (1) Wait for /readyz to become 200 -- the poll loop has run at least once and persisted a
	// cursor.
	if res, ok := waitFor(3*time.Second, func(r readyzResult) bool { return r.status == http.StatusOK }); !ok {
		t.Fatalf("expected /readyz to become 200, last=%+v", res)
	}

	// Still (1): stay 200 well past the cursor-freshness window (6*pollInterval) while the server
	// keeps answering normally -- only true if the poll loop keeps refreshing cursor freshness on
	// every successful persist (poller.OnCursorSaved), not just once at bootstrap.
	time.Sleep(cursorFreshnessMultiple*pollInterval*4 + 100*time.Millisecond)
	if res, err := getReadyz(); err != nil || res.status != http.StatusOK {
		t.Fatalf("expected /readyz to remain 200 across %v of a healthy, running poll loop, got %+v (err=%v)", cursorFreshnessMultiple*pollInterval*4, res, err)
	}

	// (2) Break the events endpoint (500, NOT 401 -- must not touch the auth leg) so no further
	// cursor saves happen; /readyz must eventually go 503 citing cursor staleness on its own. Note
	// health.Status.Ready() short-circuits: once the cursor leg fails, the auth leg is never even
	// checked (by design, see health.go), so this assertion must run BEFORE auth is ever broken, or
	// a later auth failure would be invisible behind the cursor reason.
	atomic.StoreInt32(&mode, modeServerError)
	if res, ok := waitFor(3*time.Second, func(r readyzResult) bool {
		return r.status == http.StatusServiceUnavailable && strings.Contains(r.body, "cursor")
	}); !ok {
		t.Fatalf("expected /readyz to become 503 citing cursor staleness once the poll loop stopped persisting, last=%+v", res)
	}

	// Recover: restore the OK server and wait for /readyz to go back to 200, so the auth assertion
	// below observes a clean, fresh-cursor baseline (same short-circuit reasoning as above, in
	// reverse -- a still-stale cursor would swallow the auth reason).
	atomic.StoreInt32(&mode, modeOK)
	if res, ok := waitFor(3*time.Second, func(r readyzResult) bool { return r.status == http.StatusOK }); !ok {
		t.Fatalf("expected /readyz to recover to 200 once the events endpoint started answering again, last=%+v", res)
	}

	// (3) Switch to 401s; /readyz must report the auth leg specifically, observed while the cursor
	// is still fresh from the recovery above.
	atomic.StoreInt32(&mode, modeUnauthorized)
	if res, ok := waitFor(3*time.Second, func(r readyzResult) bool {
		return r.status == http.StatusServiceUnavailable && strings.Contains(r.body, "auth")
	}); !ok {
		t.Fatalf("expected /readyz to become 503 citing invalid auth once the sentinel API started returning 401, last=%+v", res)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runApp did not return after context cancellation")
	}
}

// TestRunApp_ReadyzStaysNotReady_WhenSentinelUnreachable proves the validator's major finding is
// fixed: with an unreachable SENTINEL_URL, resolveAgentID retries forever and the pipeline never
// completes a single cursor persist, so /readyz must report 503 for the WHOLE run, never flip to
// 200. Before the fix, st.SetCursorFreshnessWindow was only called from inside runPipeline AFTER
// resolveAgentID had already succeeded, so this exact scenario -- and every other pre-loop-phase
// outage -- silently disabled the "cursor persisted recently" leg of plan §7's /readyz composition
// and left it permanently ready:true.
func TestRunApp_ReadyzStaysNotReady_WhenSentinelUnreachable(t *testing.T) {
	// A closed TCP port refuses the connection outright -- a transport-level outage, distinct from
	// (and, per the validator's evidence, NOT covered by) the 401 path sentinel.Client.OnAuthStatus
	// handles.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen (throwaway port to find a free one): %v", err)
	}
	unreachableURL := "http://" + ln.Addr().String()
	ln.Close() // now guaranteed closed/refusing on this machine

	healthLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	healthAddr := healthLn.Addr().String()
	healthLn.Close()

	const pollInterval = 10 * time.Millisecond
	cfg := Config{
		WorkerEnabled:       true,
		WorkerExecute:       false,
		SentinelURL:         unreachableURL,
		SentinelAgentKey:    "test-key",
		WorkerPollInterval:  pollInterval,
		WorkerHealthAddr:    healthAddr,
		WorkerStateDir:      t.TempDir(),
		WorkerBackfillHours: 24,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		close(done)
	}()

	readyzURL := "http://" + healthAddr + "/readyz"
	deadline := time.Now().Add(500 * time.Millisecond) // 50x pollInterval: many retries observed
	sawAny := false
	for time.Now().Before(deadline) {
		resp, err := http.Get(readyzURL)
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		sawAny = true
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected /readyz to stay 503 while sentinel is unreachable and no cursor has ever been persisted, got status=%d body=%s", resp.StatusCode, body)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawAny {
		t.Fatalf("never managed to reach the health server at all")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runApp did not return after context cancellation")
	}
}

// TestRunApp_MetricsWiredEndToEnd proves the validator's second major finding is fixed: driving a
// real event through the poll -> dispatch -> run -> journal pipeline (dry-run, so no mutating
// calls) must increment health.MetricEventsConsumed and a jobs_total_<kind>_<outcome> counter,
// not leave /metrics empty because nothing in production code ever called st.Inc/SetGauge outside
// the bootstrap-only path.
func TestRunApp_MetricsWiredEndToEnd(t *testing.T) {
	var eventsServed int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		if atomic.CompareAndSwapInt32(&eventsServed, 0, 1) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"events":[{"seq":1,"eventType":"created","issue":{"id":"iss-1","status":"unresolved","projectId":"p1"}}],"hasMore":false,"cursor":1}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"events":[],"hasMore":false,"cursor":1}`))
	})
	mux.HandleFunc("/api/agent/issues/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"issue":{"id":"iss-1","status":"unresolved"}}`))
	})
	mux.HandleFunc("/api/agent/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"issues":[],"nextCursor":null}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	healthLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	healthAddr := healthLn.Addr().String()
	healthLn.Close()

	// Pre-seed cursor.json so runPipeline skips Bootstrap's own after=0 head-seek entirely (it
	// pages the feed from after=0 exactly like the poll loop's first real call would, and would
	// otherwise race it for the one seeded event) -- isolating this test to exactly the seam the
	// validator's finding is about: the poll -> dispatch -> run -> journal pipeline's own metric
	// wiring, not Bootstrap's (already-fixed, already-tested) bootstrap_skipped counter.
	stateDir := t.TempDir()
	if err := state.SaveCursor(stateDir+"/cursor.json", 0); err != nil {
		t.Fatalf("seeding cursor.json: %v", err)
	}

	const pollInterval = 10 * time.Millisecond
	cfg := Config{
		WorkerEnabled:       true,
		WorkerExecute:       false, // dry-run: journal decisions, never call Act for real
		SentinelURL:         srv.URL,
		SentinelAgentKey:    "test-key",
		WorkerPollInterval:  pollInterval,
		WorkerHealthAddr:    healthAddr,
		WorkerStateDir:      stateDir,
		WorkerBackfillHours: 24,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		close(done)
	}()

	metricsURL := "http://" + healthAddr + "/metrics"
	deadline := time.Now().Add(2 * time.Second)
	var lastBody string
	for time.Now().Before(deadline) {
		resp, err := http.Get(metricsURL)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastBody = string(b)
			if strings.Contains(lastBody, "sentinel_worker_events_consumed") && strings.Contains(lastBody, "sentinel_worker_jobs_total_") {
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatalf("runApp did not return after context cancellation")
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("expected /metrics to expose sentinel_worker_events_consumed and a sentinel_worker_jobs_total_* counter within 2s of a real event flowing through the pipeline, last body:\n%s", lastBody)
}

// --- startup ordering: journal maintenance/RecoveryScan must not wait on identity (finding 6) ---

// TestRunPipeline_JournalMaintenanceRunsEvenWhenIdentityEndpointIsDown proves runPipeline no
// longer gates journal open + maintenance/RecoveryScan behind the identity-resolution retry loop:
// with GET /api/agent/self permanently failing (an API outage at startup), the journal's
// maintenance pass -- and therefore RecoveryScan's observability of any in-flight jobs left by a
// prior crash -- must still run and be observable in the log, well before identity ever resolves
// (or even if it never does before ctx is cancelled).
func TestRunPipeline_JournalMaintenanceRunsEvenWhenIdentityEndpointIsDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/agent/self is permanently down; anything else (events/issues) would 500 too, but
		// the pipeline should never even get that far while identity is unresolved.
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := Config{
		WorkerEnabled:      true,
		SentinelURL:        srv.URL,
		SentinelAgentKey:   "test-key",
		WorkerPollInterval: 20 * time.Millisecond,
		WorkerStateDir:     dir,
		WorkerHealthAddr:   "127.0.0.1:0",
	}

	var logBuf bytes.Buffer
	var logMu sync.Mutex
	logger := slog.New(slog.NewTextHandler(&syncWriter{w: &logBuf, mu: &logMu}, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	runPipeline(ctx, cfg, logger, nil, nil)

	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()

	if !strings.Contains(logged, "recovery scan found no in-flight jobs") {
		t.Fatalf("expected journal maintenance's RecoveryScan to have run (and logged) even though the identity endpoint never came up, got log:\n%s", logged)
	}
	if !strings.Contains(logged, "resolving agent identity") {
		t.Fatalf("expected the identity-resolution retry loop to also have run and logged its own failures, got log:\n%s", logged)
	}
}

// syncWriter guards a bytes.Buffer with a mutex so runPipeline's own goroutines (journal
// maintenance loop, poll loop) can log concurrently with the test reading logBuf without racing.
type syncWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
