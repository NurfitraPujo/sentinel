package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
	guardpkg "github.com/NurfitraPujo/sentinel/tools/sentinel-worker/guard"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/health"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/loop"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/settings"
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

// TestLoadConfig_WorkerSettingsRefreshDefaultAndOverride proves WORKER_SETTINGS_REFRESH (plan
// §4.5, C15/C16: the settings-refresh loop cadence) defaults to 5m and is overridable via the
// same Go duration notation as every other §5 knob.
func TestLoadConfig_WorkerSettingsRefreshDefaultAndOverride(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(nil))
	if len(errs) != 0 {
		t.Fatalf("expected zero validation errors on an empty environment, got: %v", errs)
	}
	if cfg.WorkerSettingsRefresh != 5*time.Minute {
		t.Errorf("WORKER_SETTINGS_REFRESH default = %s, want 5m", cfg.WorkerSettingsRefresh)
	}

	cfg, errs = LoadConfig(fakeEnv(map[string]string{"WORKER_SETTINGS_REFRESH": "90s"}))
	if len(errs) != 0 {
		t.Fatalf("expected zero validation errors for WORKER_SETTINGS_REFRESH=90s, got: %v", errs)
	}
	if cfg.WorkerSettingsRefresh != 90*time.Second {
		t.Errorf("WORKER_SETTINGS_REFRESH = %s, want 90s", cfg.WorkerSettingsRefresh)
	}
}

// TestLoadConfig_WorkerSettingsRefreshMustBePositive proves a non-positive override is rejected,
// mirroring WORKER_SHUTDOWN_TIMEOUT/WORKER_POLL_INTERVAL's own >0 validation.
func TestLoadConfig_WorkerSettingsRefreshMustBePositive(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{"WORKER_SETTINGS_REFRESH": "0s"}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_SETTINGS_REFRESH") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_SETTINGS_REFRESH validation error for 0s, got: %v", errs)
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
	if cfg.WorkerGateMaxVerbatim != 0.25 {
		t.Errorf("WORKER_GATE_MAX_VERBATIM default = %v, want 0.25", cfg.WorkerGateMaxVerbatim)
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

// TestLoadConfig_RejectsUnknownEventType is finding 3's red-first proof: a typo'd
// WORKER_EVENT_TYPES entry must be caught at config load, not discovered only once the server
// starts 400-ing every poll forever (ClassPermanent -> backoff -> circuit wedge).
func TestLoadConfig_RejectsUnknownEventType(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_EVENT_TYPES": "created,ocurrence_burst", // typo: missing 'c'
	}))
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_EVENT_TYPES") && strings.Contains(e, "ocurrence_burst") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_EVENT_TYPES validation error naming the unknown type, got: %v", errs)
	}
}

// TestLoadConfig_AcceptsAllKnownEventTypes proves the full known vocabulary (loop/dispatch.go's
// Classify switch) round-trips with zero WORKER_EVENT_TYPES errors -- a mutation that narrows
// knownEventTypes would turn this red.
func TestLoadConfig_AcceptsAllKnownEventTypes(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_EVENT_TYPES": "created,report_created,occurrence_burst,regressed,question_answered,commented,claim_released,status_changed,issue_deleted",
	}))
	for _, e := range errs {
		if strings.Contains(e, "WORKER_EVENT_TYPES") {
			t.Fatalf("unexpected WORKER_EVENT_TYPES error for a fully valid list: %v", e)
		}
	}
	if len(cfg.WorkerEventTypes) != 9 {
		t.Fatalf("WorkerEventTypes = %v, want 9 entries parsed", cfg.WorkerEventTypes)
	}
}

// TestWarnIfEventTypeFilterOmitsControlPlane is finding 3's second half: a narrow-but-valid
// allowlist that omits a control-plane event type (status_changed/claim_released/issue_deleted)
// must warn, since the dispatcher silently loses that behavior otherwise. An empty filter (no
// restriction configured) must never warn.
func TestWarnIfEventTypeFilterOmitsControlPlane(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	warnIfEventTypeFilterOmitsControlPlane([]string{"created", "commented"}, logger)
	got := buf.String()
	if !strings.Contains(got, "status_changed") || !strings.Contains(got, "claim_released") || !strings.Contains(got, "issue_deleted") {
		t.Fatalf("expected a warning naming all three omitted control-plane types, got log:\n%s", got)
	}

	buf.Reset()
	warnIfEventTypeFilterOmitsControlPlane(nil, logger)
	if buf.Len() != 0 {
		t.Fatalf("expected no warning for an empty (unrestricted) filter, got:\n%s", buf.String())
	}

	buf.Reset()
	warnIfEventTypeFilterOmitsControlPlane([]string{"created", "status_changed", "claim_released", "issue_deleted"}, logger)
	if buf.Len() != 0 {
		t.Fatalf("expected no warning when all control-plane types are present, got:\n%s", buf.String())
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

// TestBuildKeyStore_WritableStateDir_NotReadOnly proves the common case: a normal, writable
// WORKER_STATE_DIR (the file backend, the default) produces a KeyStore with rotation ENABLED.
func TestBuildKeyStore_WritableStateDir_NotReadOnly(t *testing.T) {
	cfg := Config{WorkerKeystore: "file", WorkerStateDir: t.TempDir()}
	_, readOnly := buildKeyStore(context.Background(), cfg)
	if readOnly {
		t.Fatal("expected a writable state dir to yield readOnly=false")
	}
}

// TestBuildKeyStore_ReadOnlyStateDir_DisablesRotation proves the plan §2.5 requirement is actually
// reachable at runtime: a genuinely non-writable state directory (e.g. a read-only-remounted
// volume) makes buildKeyStore report readOnly=true for the file backend, which LoadConfig accepts
// as valid (unlike the kubernetes-secret name/namespace branch, this condition was previously
// impossible to trigger on any config that reaches runPipeline).
func TestBuildKeyStore_ReadOnlyStateDir_DisablesRotation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses directory permission checks")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0o700)

	cfg := Config{WorkerKeystore: "file", WorkerStateDir: dir}
	_, errs := LoadConfig(fakeEnv(map[string]string{"WORKER_STATE_DIR": dir}))
	for _, e := range errs {
		if strings.Contains(e, "WORKER_STATE_DIR") {
			t.Fatalf("did not expect a read-only state dir to fail config validation: %v", errs)
		}
	}

	_, readOnly := buildKeyStore(context.Background(), cfg)
	if !readOnly {
		t.Fatal("expected a read-only state dir to yield readOnly=true (rotation disabled)")
	}
}

// TestWritableKeyStore_UsesTheOptionalWritableSeam proves buildKeyStore's readOnly determination
// (writableKeyStore) is a REAL writability probe delegated to the store -- not merely "was a
// name/namespace configured" (unreachable at runtime for any config LoadConfig accepts) -- by
// exercising the seam directly against both a false- and true-reporting store, plus the
// no-Writable-method fallback used by minimal test doubles like keyguard/guard_test.go's fakeStore.
// keyguard/store_test.go's TestK8sKeyStore_WritableFalseOnRBACDenial and
// TestFileKeyStore_WritableFalseForReadOnlyDir cover the two production backends' actual probes
// (dry-run PATCH / temp-file write) end to end.
func TestWritableKeyStore_UsesTheOptionalWritableSeam(t *testing.T) {
	if got := (writableKeyStore(context.Background(), &fakeWritabilityStore{writable: false})); got {
		t.Fatal("expected writableKeyStore to report false for a store whose Writable() returns false")
	}
	if got := (writableKeyStore(context.Background(), &fakeWritabilityStore{writable: true})); !got {
		t.Fatal("expected writableKeyStore to report true for a store whose Writable() returns true")
	}
	if got := writableKeyStore(context.Background(), noopKeyStore{}); !got {
		t.Fatal("expected a store without the Writable seam to default to writable")
	}
}

// fakeWritabilityStore is a minimal keyguard.KeyStore + keyStoreWritabilityChecker test double.
type fakeWritabilityStore struct {
	writable bool
}

func (fakeWritabilityStore) Load(ctx context.Context) (string, bool, error) { return "", false, nil }
func (fakeWritabilityStore) Persist(ctx context.Context, key string) error  { return nil }
func (s *fakeWritabilityStore) Writable(ctx context.Context) bool           { return s.writable }

// noopKeyStore implements only the base keyguard.KeyStore interface, with no Writable seam, to
// prove writableKeyStore's default-writable fallback for test doubles.
type noopKeyStore struct{}

func (noopKeyStore) Load(ctx context.Context) (string, bool, error) { return "", false, nil }
func (noopKeyStore) Persist(ctx context.Context, key string) error  { return nil }

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

// TestLoadConfig_WorkerExecuteTrueIsAccepted proves N8d's lift of the N8a WORKER_EXECUTE=true
// rejection: buildRunner now wires a real Actor (jobs.RealActor, act.go's compiler), so
// WORKER_EXECUTE=true is a supported config value and must not be rejected at validation time.
func TestLoadConfig_WorkerExecuteTrueIsAccepted(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_EXECUTE": "true",
	}))
	for _, e := range errs {
		if strings.Contains(e, "WORKER_EXECUTE") {
			t.Fatalf("WORKER_EXECUTE=true: unexpected validation error: %v", e)
		}
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
			cfg := Config{WorkerExecute: tc.workerExecute, LLMProvider: "openai", LLMModel: "gpt-4o-mini"}
			client := sentinel.NewClient("http://example.invalid", "key")
			journal := state.OpenJournal(t.TempDir() + "/jobs.journal")
			runner, _, err := buildRunner(cfg, client, journal, "agent-1", settings.NewStore(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
			if err != nil {
				t.Fatalf("buildRunner: %v", err)
			}
			if runner.DryRun != tc.wantDryRun {
				t.Fatalf("WorkerExecute=%v: runner.DryRun = %v, want %v", tc.workerExecute, runner.DryRun, tc.wantDryRun)
			}
		})
	}
}

// TestBuildRunner_WiresDailyBudgetAndTriageLimiterFromConfig is the wired-from-main proof for plan
// §2.6 finding 1: WORKER_DAILY_TOKEN_BUDGET and WORKER_MAX_TRIAGE_PER_HOUR previously had ZERO
// production callers (llm.NewDailyBudget/llm.NewHourlyCounter were only ever exercised by their own
// package's unit tests) — this drives the REAL buildRunner main.go's runPipeline calls, with a
// journal carrying a real "advised" record (not a hand-seeded budget struct), and asserts the
// returned *loop.Runner's Budget/TriageLimiter are non-nil, correctly seeded from that journal, and
// obey the configured limits.
func TestBuildRunner_WiresDailyBudgetAndTriageLimiterFromConfig(t *testing.T) {
	cfg := Config{
		WorkerExecute:          false,
		LLMProvider:            "openai",
		LLMModel:               "gpt-4o-mini",
		WorkerDailyTokenBudget: 1000,
		WorkerMaxTriagePerHour: 2,
	}
	client := sentinel.NewClient("http://example.invalid", "key")
	journalPath := filepath.Join(t.TempDir(), "jobs.journal")
	journal := state.OpenJournal(journalPath)

	// Seed the journal with a REAL advised record (jobs.Decision, journaled exactly as
	// loop.Runner.Run journals it) reporting 400 tokens spent today -- not a hand-constructed
	// llm.DailyBudget the real boot path never produces.
	dec := jobs.Decision{Kind: "triage", Raw: []byte(`{"stub":true}`), Usage: llm.Usage{InputTokens: 300, OutputTokens: 100}}
	payload, err := json.Marshal(dec)
	if err != nil {
		t.Fatalf("marshal decision: %v", err)
	}
	must(t, journal.Append(state.Record{
		JobID: "seed-job-1", IssueID: "issue-1", Kind: "triage", TriggerSeq: 1,
		State: state.StateAdvised, At: time.Now(), Payload: payload,
	}))
	// A terminal record for the SAME jobId so it isn't mistaken for an in-flight job by anything
	// else touching this journal during the test.
	must(t, journal.Append(state.Record{
		JobID: "seed-job-1", IssueID: "issue-1", Kind: "triage", TriggerSeq: 1,
		State: state.StateDone, At: time.Now(),
	}))

	runner, _, err := buildRunner(cfg, client, journal, "agent-1", settings.NewStore(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatalf("buildRunner: %v", err)
	}

	if runner.Budget == nil {
		t.Fatalf("runner.Budget is nil -- WORKER_DAILY_TOKEN_BUDGET is dead config again")
	}
	budget, ok := runner.Budget.(*llm.DailyBudget)
	if !ok {
		t.Fatalf("runner.Budget is a %T, want *llm.DailyBudget", runner.Budget)
	}
	if got := budget.Spent(); got != 400 {
		t.Fatalf("budget.Spent() = %d, want 400 (seeded from the journal's advised record)", got)
	}
	if budget.Exhausted() {
		t.Fatalf("budget with 400/1000 spent must not report Exhausted")
	}

	if runner.TriageLimiter == nil {
		t.Fatalf("runner.TriageLimiter is nil -- WORKER_MAX_TRIAGE_PER_HOUR is dead config again")
	}
	limiter, ok := runner.TriageLimiter.(*llm.HourlyCounter)
	if !ok {
		t.Fatalf("runner.TriageLimiter is a %T, want *llm.HourlyCounter", runner.TriageLimiter)
	}
	// WORKER_MAX_TRIAGE_PER_HOUR=2: two increments allowed, the third denied.
	if !limiter.TryIncrement() || !limiter.TryIncrement() {
		t.Fatalf("expected the first two TryIncrement calls to succeed against a cap of 2")
	}
	if limiter.TryIncrement() {
		t.Fatalf("expected the third TryIncrement call to be denied against a cap of 2")
	}
}

// TestBuildRunner_SeedsTriageLimiterFromJournaledClaimedRecords is circuit-config-sec finding 3's
// wired-from-main proof: buildRunner must seed runner.TriageLimiter from real state.StateClaimed
// TRIAGE journal records for the CURRENT UTC hour (jobs.SumTriageStarts), not start every process
// at zero, so a crash-loop within the same hour cannot let a caller triage past
// WORKER_MAX_TRIAGE_PER_HOUR by just restarting. Red-first: before this fix buildRunner never
// called SeedCount at all, so this test would see all 2 slots still available despite 2 already
// having been consumed and journaled.
func TestBuildRunner_SeedsTriageLimiterFromJournaledClaimedRecords(t *testing.T) {
	cfg := Config{
		WorkerExecute:          false,
		LLMProvider:            "openai",
		LLMModel:               "gpt-4o-mini",
		WorkerMaxTriagePerHour: 2,
	}
	client := sentinel.NewClient("http://example.invalid", "key")
	journalPath := filepath.Join(t.TempDir(), "jobs.journal")
	journal := state.OpenJournal(journalPath)

	now := time.Now().UTC()
	// Two real TRIAGE jobs that reached StateClaimed (and beyond) THIS hour -- exactly what a
	// crashed process would have left behind after consuming both of its 2 hourly slots.
	must(t, journal.Append(state.Record{JobID: "seed-triage-1", IssueID: "issue-1", Kind: "triage", TriggerSeq: 1, State: state.StateClaimed, At: now}))
	must(t, journal.Append(state.Record{JobID: "seed-triage-1", IssueID: "issue-1", Kind: "triage", TriggerSeq: 1, State: state.StateAdvised, At: now}))
	must(t, journal.Append(state.Record{JobID: "seed-triage-2", IssueID: "issue-2", Kind: "triage", TriggerSeq: 2, State: state.StateClaimed, At: now}))
	must(t, journal.Append(state.Record{JobID: "seed-triage-2", IssueID: "issue-2", Kind: "triage", TriggerSeq: 2, State: state.StateFailed, At: now}))

	runner, _, err := buildRunner(cfg, client, journal, "agent-1", settings.NewStore(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatalf("buildRunner: %v", err)
	}

	limiter, ok := runner.TriageLimiter.(*llm.HourlyCounter)
	if !ok {
		t.Fatalf("runner.TriageLimiter is a %T, want *llm.HourlyCounter", runner.TriageLimiter)
	}
	if got := limiter.Count(); got != 2 {
		t.Fatalf("limiter.Count() after boot = %d, want 2 (seeded from the 2 journaled claimed-this-hour TRIAGE records)", got)
	}
	if limiter.TryIncrement() {
		t.Fatal("expected a 3rd TryIncrement to be denied -- the cap of 2 was already consumed before this process even started")
	}
}

// TestBuildRunner_WiresAndSeedsFollowupLimiterFromConfig is finding 5's (core-robustness round 3)
// wired-from-main proof: buildRunner must construct runner.FollowupLimiter from
// WORKER_MAX_FOLLOWUP_PER_HOUR and seed it from real state.StateClaimed FOLLOW-UP journal records
// for the current UTC hour (jobs.SumFollowupStarts), symmetric to TRIAGE's own wiring proven above.
// Red-first: before this fix, runner.FollowupLimiter did not exist at all -- FOLLOW-UP had no
// hourly cap, and WORKER_DAILY_TOKEN_BUDGET defaults to 0 (unlimited), leaving FOLLOW-UP spend
// effectively unbounded by default.
func TestBuildRunner_WiresAndSeedsFollowupLimiterFromConfig(t *testing.T) {
	cfg := Config{
		WorkerExecute:            false,
		LLMProvider:              "openai",
		LLMModel:                 "gpt-4o-mini",
		WorkerMaxFollowupPerHour: 2,
	}
	client := sentinel.NewClient("http://example.invalid", "key")
	journalPath := filepath.Join(t.TempDir(), "jobs.journal")
	journal := state.OpenJournal(journalPath)

	now := time.Now().UTC()
	must(t, journal.Append(state.Record{JobID: "seed-followup-1", IssueID: "issue-1", Kind: "followup", TriggerSeq: 1, State: state.StateClaimed, At: now}))
	must(t, journal.Append(state.Record{JobID: "seed-followup-1", IssueID: "issue-1", Kind: "followup", TriggerSeq: 1, State: state.StateDone, At: now}))

	runner, _, err := buildRunner(cfg, client, journal, "agent-1", settings.NewStore(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatalf("buildRunner: %v", err)
	}

	if runner.FollowupLimiter == nil {
		t.Fatalf("runner.FollowupLimiter is nil -- WORKER_MAX_FOLLOWUP_PER_HOUR is dead config")
	}
	limiter, ok := runner.FollowupLimiter.(*llm.HourlyCounter)
	if !ok {
		t.Fatalf("runner.FollowupLimiter is a %T, want *llm.HourlyCounter", runner.FollowupLimiter)
	}
	if got := limiter.Count(); got != 1 {
		t.Fatalf("limiter.Count() after boot = %d, want 1 (seeded from the 1 journaled claimed-this-hour FOLLOW-UP record)", got)
	}
	if !limiter.TryIncrement() {
		t.Fatalf("expected the 2nd TryIncrement to succeed against a cap of 2 (1 already seeded)")
	}
	if limiter.TryIncrement() {
		t.Fatalf("expected the 3rd TryIncrement to be denied against a cap of 2")
	}
}

// fakeSeedableSnapshotter is a minimal state.Snapshotter + s3GenerationSeeder double for
// TestSnapshotManager_SeedRemoteGeneration below -- a real state.S3Snapshotter would need an
// httptest server; this proves seedRemoteGeneration's wiring/dispatch logic directly.
type fakeSeedableSnapshotter struct {
	seedGen int64
	seedErr error
	calls   int
}

func (f *fakeSeedableSnapshotter) Upload(ctx context.Context, generation int64, tarball []byte) error {
	return nil
}
func (f *fakeSeedableSnapshotter) RestoreLatest(ctx context.Context) ([]byte, int64, bool, error) {
	return nil, 0, false, nil
}
func (f *fakeSeedableSnapshotter) SeedGeneration(ctx context.Context) (int64, error) {
	f.calls++
	return f.seedGen, f.seedErr
}

// TestSnapshotManager_SeedRemoteGeneration_RaisesNextGen is finding 2's (core-robustness round 3)
// proof for snapshotManager.seedRemoteGeneration: when the underlying Snapshotter is an
// s3GenerationSeeder and reports a generation higher than nextGen's current value (as happens
// when local state SURVIVED a restart -- restoreIfEmpty's restore-on-empty trigger never fires --
// but another writer has since pushed S3's latest generation higher), nextGen must be raised to
// match so the next upload cannot collide with what's already on S3. Red-first: before this fix,
// main.go never called seedRemoteGeneration at all, so nextGen would still read its pre-seed
// value (0) here.
func TestSnapshotManager_SeedRemoteGeneration_RaisesNextGen(t *testing.T) {
	fake := &fakeSeedableSnapshotter{seedGen: 99}
	mgr := newSnapshotManager(fake, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	mgr.seedRemoteGeneration(context.Background())

	if fake.calls != 1 {
		t.Fatalf("expected SeedGeneration to be called exactly once, got %d", fake.calls)
	}
	if got := mgr.nextGen.Load(); got != 99 {
		t.Fatalf("nextGen after seedRemoteGeneration = %d, want 99", got)
	}
}

// TestSnapshotManager_SeedRemoteGeneration_NeverLowersNextGen proves seedRemoteGeneration only
// ever RAISES nextGen: a restore that already set nextGen higher than S3's seedGen report (e.g. a
// stale/lagging replica's `latest` pointer) must not be regressed by this call.
func TestSnapshotManager_SeedRemoteGeneration_NeverLowersNextGen(t *testing.T) {
	fake := &fakeSeedableSnapshotter{seedGen: 3}
	mgr := newSnapshotManager(fake, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.nextGen.Store(50)

	mgr.seedRemoteGeneration(context.Background())

	if got := mgr.nextGen.Load(); got != 50 {
		t.Fatalf("nextGen after seedRemoteGeneration = %d, want unchanged 50 (must never lower)", got)
	}
}

// TestSnapshotManager_SeedRemoteGeneration_NoopForNoneBackend proves seedRemoteGeneration is a
// harmless no-op when the wired Snapshotter (state.NoneSnapshotter, WORKER_SNAPSHOT_BACKEND=none)
// does not implement s3GenerationSeeder at all -- must not panic via a failed type assertion.
func TestSnapshotManager_SeedRemoteGeneration_NoopForNoneBackend(t *testing.T) {
	mgr := newSnapshotManager(state.NoneSnapshotter{}, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	mgr.seedRemoteGeneration(context.Background()) // must not panic
	if got := mgr.nextGen.Load(); got != 0 {
		t.Fatalf("nextGen = %d, want 0 (untouched for a non-seedable backend)", got)
	}
}

// --- resumeInFlightJob: boot-time FIX resume trigger (plan §4.4 step 3b, finding 4) -------------

// mainTestGitRun runs a real git command directly, mirroring jobs/fix_workspace_test.go's own
// runReal helper (unexported there, so duplicated here rather than exported cross-package purely
// for a test fixture).
func mainTestGitRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// newMainTestBareFixtureRepo creates a bare "origin" repo plus a work tree pushed to it with one
// commit on branch main -- the same shape jobs/fix_workspace_test.go's newBareFixtureRepo builds,
// reimplemented here since that helper is unexported to the jobs package.
func newMainTestBareFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bareRepo := filepath.Join(root, "origin.git")
	workRepo := filepath.Join(root, "work")
	mainTestGitRun(t, root, "git", "init", "--bare", "-b", "main", bareRepo)
	mainTestGitRun(t, root, "git", "init", "-b", "main", workRepo)
	mainTestGitRun(t, workRepo, "git", "config", "user.email", "test@example.com")
	mainTestGitRun(t, workRepo, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workRepo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	mainTestGitRun(t, workRepo, "git", "add", ".")
	mainTestGitRun(t, workRepo, "git", "commit", "-m", "seed")
	mainTestGitRun(t, workRepo, "git", "remote", "add", "origin", bareRepo)
	mainTestGitRun(t, workRepo, "git", "push", "origin", "main")
	return bareRepo
}

// mainTestFakeProvider is a minimal gitprovider.Provider fake recording every CreatePR call, for
// asserting a resumed FIX job actually opened a PR (proof that ResumeFix, not merely "some
// function", ran end to end).
type mainTestFakeProvider struct {
	pr      gitprovider.PR
	created []gitprovider.PRSpec
}

func (f *mainTestFakeProvider) Auth() gitprovider.GitCredential {
	return gitprovider.GitHubTokenCredential("")
}
func (f *mainTestFakeProvider) CreatePR(_ context.Context, _ gitprovider.RepoRef, spec gitprovider.PRSpec) (gitprovider.PR, error) {
	f.created = append(f.created, spec)
	return f.pr, nil
}
func (f *mainTestFakeProvider) PRStatus(_ context.Context, _ gitprovider.RepoRef, _ string) (gitprovider.PRState, error) {
	return gitprovider.PRStateOpen, nil
}

// mainTestRecordingSender is a minimal jobs.Sender fake, just enough for ResumeFix's own
// comment/progress/PR batches to have somewhere to land.
type mainTestRecordingSender struct{ calls []string }

func (s *mainTestRecordingSender) PostQuestion(_ context.Context, issueID string, _ map[string]interface{}, key string) (*sentinel.Result, error) {
	s.calls = append(s.calls, "question:"+issueID+":"+key)
	return &sentinel.Result{Status: 201}, nil
}
func (s *mainTestRecordingSender) PostBatch(_ context.Context, _ sentinel.BatchRequest) (*sentinel.Result, error) {
	s.calls = append(s.calls, "batch")
	return &sentinel.Result{Status: 200}, nil
}

// TestResumeInFlightJob_FixRunning_DrivesResumeFix is the RED-FIRST, end-to-end proof for finding
// 4: a journal record left at state.StateFixRunning by a crashed FIX attempt (journalFixRunning,
// jobs/fix_pr.go) must actually get resumed at boot, not silently orphaned with its claim held
// forever. It builds a REAL jobs.FixRunner (no fakes standing in for the seam under test) and
// drives it purely through resumeInFlightJob/RecoveryScan's own InFlightJob shape -- proving the
// wiring this finding is about, not ResumeFix's own internals (already covered by jobs/fix_test.go).
//
// MUTATION-TEST NOTE (per task brief): remove resumeInFlightJob's `if job.Kind == jobs.FixKind &&
// job.State == state.StateFixRunning` branch (falling through to runner.Resume unconditionally, the
// pre-fix behaviour) -- this test must go red, because loop.Runner.Resume rejects a non-loop.Kind
// ("fix") with an error and never reaches CreatePR.
func TestResumeInFlightJob_FixRunning_DrivesResumeFix(t *testing.T) {
	bareRepo := newMainTestBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &mainTestRecordingSender{}
	fp := &mainTestFakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}

	fixRunner := &jobs.FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          jobs.LocalDirArtifactSink{Root: t.TempDir()},
		Caps:          jobs.NewFixCaps(10, 10, 2, nil),
		ResolveRepo: func(projectID string) (jobs.FixRepoConfig, bool, error) {
			return jobs.FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		// No prior live resume-state was ever saved for this jobID (ResumeFix falls back to a fresh
		// RunFix in that case, per its own doc) -- this executor makes that fresh run actually
		// produce a change to commit and push, so the resumed job reaches CreatePR.
		ExecutorCmd: `echo "fix applied" >> fixed.txt && echo "applied fix" >> "$PROGRESS_MD"`,
	}

	in := jobs.FixJobInput{
		JobID:      "job-recovered",
		IssueID:    "issue-recovered",
		ProjectID:  "proj-1",
		ErrorClass: "NilPointerException",
		FixBrief:   "dereference guarded now",
		TriggerSeq: 5,
	}
	payload, err := json.Marshal(struct {
		Input      jobs.FixJobInput `json:"input"`
		BaseCommit string           `json:"baseCommit"`
	}{Input: in, BaseCommit: "deadbeef"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	// Exactly what journalFixRunning appends at RunFix attempt start -- hand-built here (rather than
	// calling journalFixRunning, unexported to this package) to prove resumeInFlightJob reacts to
	// the RECORD SHAPE the journal actually contains after a crash, not to a test-only shortcut.
	if err := journal.Append(state.Record{
		JobID:      in.JobID,
		IssueID:    in.IssueID,
		Kind:       jobs.FixKind,
		TriggerSeq: in.TriggerSeq,
		State:      state.StateFixRunning,
		Payload:    payload,
	}); err != nil {
		t.Fatalf("journal.Append: %v", err)
	}

	inFlight, _, err := journal.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if len(inFlight) != 1 {
		t.Fatalf("expected exactly one in-flight job, got %d", len(inFlight))
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	resumeInFlightJob(context.Background(), nil, fixRunner, inFlight[0], logger)

	// Finding 2 (fix-lifecycle remediation round 2): resumeInFlightJob's FIX branch now runs
	// ResumeFix via DispatchResume -- its own goroutine, tracked by fixRunner's WaitGroup -- rather
	// than calling ResumeFix synchronously (the pre-fix behaviour this test used to assert on
	// directly). Wait for it to finish exactly the way runWorker's graceful-shutdown path does,
	// bounded generously since this is a real (if tiny) git clone+commit+push, not a network call.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	fixRunner.Wait(waitCtx)

	if len(fp.created) != 1 {
		t.Fatalf("expected the recovered FIX job to reach CreatePR exactly once, got %d -- the journal's StateFixRunning record was not actually resumed", len(fp.created))
	}
	if _, found, err := jobs.ResolveOpenFixPR(journal, "issue-recovered"); err != nil || !found {
		t.Fatalf("expected the resumed job to journal an open fix-PR: found=%v err=%v", found, err)
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

	runJournalMaintenance(journal, dir, 10, logger, nil, nil)

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

	runJournalMaintenance(journal, dir, 10, logger, st, nil)

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
	// Wait for the FIRST request rather than checking hits after a fixed sleep: under -race on a
	// loaded CI runner, runApp's startup (health bind, journal open, recovery, bootstrap) can take
	// far longer than any fixed window before it reaches the first GET /api/agent/self, so a
	// fixed-duration ctx made this flake. Signal on first hit, wait up to a generous deadline.
	firstHit := make(chan struct{}, 1)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		select {
		case firstHit <- struct{}{}:
		default:
		}
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	appDone := make(chan struct{})
	go func() {
		runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		close(appDone)
	}()

	select {
	case <-firstHit:
		// Success: the enabled pipeline drove at least one request. Tear down.
	case <-time.After(15 * time.Second):
		cancel()
		<-appDone
		t.Fatalf("expected at least one request to reach the sentinel API while WORKER_ENABLED=true within 15s, got %d -- the gate test proves nothing if this control case can't start the pipeline either", atomic.LoadInt32(&hits))
	}
	cancel()
	<-appDone
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

// TestLoadConfig_WorkerEnabledRequiresLLMModel is circuit-config-sec finding 2's red-first proof:
// before this fix, WORKER_ENABLED=true with LLM_MODEL unset (and LLM_API_KEY unset) passed
// validation, started the pipeline, and reported ready — every TRIAGE/FOLLOW-UP job then failed at
// the first llm.New/RunLoop call, discoverable only from job outcomes, not from startup.
func TestLoadConfig_WorkerEnabledRequiresLLMModel(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_ENABLED":     "true",
		"SENTINEL_URL":       "http://sentinel.example",
		"SENTINEL_AGENT_KEY": "k",
		"LLM_MODEL":          "",
		"LLM_API_KEY":        "",
	}))
	foundModel, foundKey := false, false
	for _, e := range errs {
		if strings.Contains(e, "LLM_MODEL") {
			foundModel = true
		}
		if strings.Contains(e, "LLM_API_KEY") {
			foundKey = true
		}
	}
	if !foundModel || !foundKey {
		t.Fatalf("expected both LLM_MODEL and LLM_API_KEY validation errors when WORKER_ENABLED=true, got: %v", errs)
	}
}

// TestLoadConfig_WorkerEnabledWithLLMModelAndKeyIsAccepted proves the guard above is specific to
// the missing-value case — a fully configured WORKER_ENABLED=true environment must not trip it.
func TestLoadConfig_WorkerEnabledWithLLMModelAndKeyIsAccepted(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_ENABLED":     "true",
		"SENTINEL_URL":       "http://sentinel.example",
		"SENTINEL_AGENT_KEY": "k",
		"LLM_MODEL":          "gpt-4o",
		"LLM_API_KEY":        "sk-test",
	}))
	for _, e := range errs {
		if strings.Contains(e, "LLM_MODEL") || strings.Contains(e, "LLM_API_KEY") {
			t.Fatalf("unexpected LLM validation error with both set: %v", errs)
		}
	}
}

// TestLoadConfig_WorkerDisabledDoesNotRequireLLMModel proves the new checks are gated on
// WORKER_ENABLED, matching every other cross-field check in this block (SENTINEL_URL/KEY above).
func TestLoadConfig_WorkerDisabledDoesNotRequireLLMModel(t *testing.T) {
	_, errs := LoadConfig(fakeEnv(nil))
	for _, e := range errs {
		if strings.Contains(e, "LLM_MODEL") || strings.Contains(e, "LLM_API_KEY") {
			t.Fatalf("did not expect an LLM_MODEL/LLM_API_KEY error when WORKER_ENABLED is unset: %v", errs)
		}
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

// TestLoadConfig_WorkerGateMaxVerbatimRangeChecked proves WORKER_GATE_MAX_VERBATIM (guard's
// exfiltration-coverage threshold, plan §4.6/§5) is range-checked to [0,1]: out-of-range values
// (negative and >1) are rejected, and the endpoints (0, the strictest legal setting -- guard
// treats 0 as "zero tolerance", not "disabled") and the default (0.25) are accepted with no error.
func TestLoadConfig_WorkerGateMaxVerbatimRangeChecked(t *testing.T) {
	cases := []struct {
		value     string
		wantError bool
	}{
		{"-0.1", true},
		{"1.5", true},
		{"0", false},
		{"0.25", false},
		{"1", false},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			_, errs := LoadConfig(fakeEnv(map[string]string{"WORKER_GATE_MAX_VERBATIM": tc.value}))
			found := false
			for _, e := range errs {
				if strings.Contains(e, "WORKER_GATE_MAX_VERBATIM") {
					found = true
				}
			}
			if tc.wantError && !found {
				t.Fatalf("WORKER_GATE_MAX_VERBATIM=%s: expected a validation error, got: %v", tc.value, errs)
			}
			if !tc.wantError && found {
				t.Fatalf("WORKER_GATE_MAX_VERBATIM=%s: did not expect a validation error, got: %v", tc.value, errs)
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

	// 100ms (not 15ms) so the cursor-freshness window (cursorFreshnessMultiple*pollInterval = 600ms)
	// comfortably exceeds a single poll cycle even under -race on a loaded CI runner. At 15ms the
	// window was only 90ms, which a -race'd cycle (HTTP round-trip + journal fsync) could exceed,
	// letting the cursor go stale mid-test and flaking the "stays 200" assertion below.
	const pollInterval = 100 * time.Millisecond
	cfg := Config{
		WorkerEnabled:       true,
		WorkerExecute:       false,
		SentinelURL:         srv.URL,
		SentinelAgentKey:    "test-key",
		WorkerPollInterval:  pollInterval,
		WorkerHealthAddr:    healthAddr,
		WorkerStateDir:      t.TempDir(),
		WorkerBackfillHours: 24,
		LLMProvider:         "openai",
		LLMModel:            "test-model",
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
		LLMProvider:         "openai",
		LLMModel:            "test-model",
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
		LLMProvider:         "openai",
		LLMModel:            "test-model",
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

// TestRunApp_CursorLagReflectsWholeCycleBacklogNotLastPage is circuit-config-sec finding 4's
// wired-from-main, red-first proof (round 2): a genuine backlog is served as TWO pages of
// loop.EventsMaxLimit (200) events each, chained by hasMore=true then hasMore=false, followed by
// a genuinely empty poll. The gauge must report a CUMULATIVE total across the whole drain cycle
// that exceeds EventsMaxLimit while draining a real backlog -- something a single page's count
// (the round-1 fix's behavior, which this test would have failed under: a single page tops out at
// 200, never "> 200") cannot produce -- and only settle back to 0 once the feed genuinely goes
// empty.
func TestRunApp_CursorLagReflectsWholeCycleBacklogNotLastPage(t *testing.T) {
	var pageIdx int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt32(&pageIdx, 1)
		switch idx {
		case 1:
			// Page 1 of a real backlog: a full 200-event page with hasMore=true, chaining to page 2.
			var b strings.Builder
			b.WriteString(`{"events":[`)
			for i := 0; i < 200; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"seq":%d,"eventType":"created","issue":{"id":"iss-%d","status":"unresolved","projectId":"p1"}}`, i+1, i+1)
			}
			b.WriteString(`],"hasMore":true,"cursor":200}`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(b.String()))
			return
		case 2:
			// Page 2: the tail of the same backlog, hasMore=false. Cumulative-cycle total is now
			// 202, which exceeds EventsMaxLimit (200) -- a single page's count never could.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"events":[` +
				`{"seq":201,"eventType":"created","issue":{"id":"iss-201","status":"unresolved","projectId":"p1"}},` +
				`{"seq":202,"eventType":"created","issue":{"id":"iss-202","status":"unresolved","projectId":"p1"}}` +
				`],"hasMore":false,"cursor":202}`))
			return
		default:
			// Every subsequent poll is genuinely empty -- the gauge must settle back to 0 here.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"events":[],"hasMore":false,"cursor":202}`))
		}
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

	stateDir := t.TempDir()
	if err := state.SaveCursor(stateDir+"/cursor.json", 0); err != nil {
		t.Fatalf("seeding cursor.json: %v", err)
	}

	const pollInterval = 10 * time.Millisecond
	cfg := Config{
		WorkerEnabled:       true,
		WorkerExecute:       false,
		SentinelURL:         srv.URL,
		SentinelAgentKey:    "test-key",
		WorkerPollInterval:  pollInterval,
		WorkerHealthAddr:    healthAddr,
		WorkerStateDir:      stateDir,
		WorkerBackfillHours: 24,
		LLMProvider:         "openai",
		LLMModel:            "test-model",
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		close(done)
	}()

	readCursorLag := func() (int64, string, bool) {
		resp, err := http.Get(metricsURLFor(healthAddr))
		if err != nil {
			return 0, "", false
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body := string(b)
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "sentinel_worker_cursor_lag ") {
				var v int64
				if _, err := fmt.Sscanf(line, "sentinel_worker_cursor_lag %d", &v); err == nil {
					return v, body, true
				}
			}
		}
		return 0, body, false
	}

	deadline := time.Now().Add(2 * time.Second)
	var lastBody string
	sawBacklogAboveLimit := false
	for time.Now().Before(deadline) {
		if v, body, ok := readCursorLag(); ok {
			lastBody = body
			if v > loop.EventsMaxLimit {
				sawBacklogAboveLimit = true
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawBacklogAboveLimit {
		t.Fatalf("expected sentinel_worker_cursor_lag to exceed EventsMaxLimit (%d) while draining the real 202-event, 2-page backlog -- a cumulative-cycle total is required to show this, since no single page in this test exceeds 200. last /metrics body:\n%s", loop.EventsMaxLimit, lastBody)
	}

	// After the backlog fully drains, every subsequent poll is genuinely empty -- the gauge must
	// settle back to 0, not get stuck at the last cycle's total.
	settledDeadline := time.Now().Add(2 * time.Second)
	sawSettleToZero := false
	for time.Now().Before(settledDeadline) {
		if v, body, ok := readCursorLag(); ok {
			lastBody = body
			if v == 0 {
				sawSettleToZero = true
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runApp did not return after context cancellation")
	}
	if !sawSettleToZero {
		t.Fatalf("expected sentinel_worker_cursor_lag to settle back to 0 once the feed genuinely emptied out, but it never did. last /metrics body:\n%s", lastBody)
	}
}

func metricsURLFor(healthAddr string) string {
	return "http://" + healthAddr + "/metrics"
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
	runPipeline(ctx, cfg, logger, nil, nil, nil, nil)

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

// TestPublishSettingsGauges_PopulatesCredentialAndRepoConnectionGauges drives publishSettingsGauges
// against a real settings.Store refreshed from an httptest server, asserting /metrics gains
// repo_connections_{total,ready} and a per-provider credential_available_<provider> gauge -- the
// wiring the N8c brief asks for and that had zero test coverage before this change.
func TestPublishSettingsGauges_PopulatesCredentialAndRepoConnectionGauges(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projects":[
		  {"id":"p1","name":"Proj One","agentSettings":{"fixEnabled":true,"maxPrsPerDay":null,
		    "repo":{"provider":"github","owner":"acme","repo":"widgets","defaultBranch":"main","testCmd":"","agentCmd":"","cloneDepth":1}}}
		]}`))
	})
	mux.HandleFunc("/api/agent/repo-credentials", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"tok"}}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := sentinel.NewClient(srv.URL, "test-key")
	store := settings.NewStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Refresh(context.Background(), client, nil, settings.EnvFallback{}, logger); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	st := health.NewStatus()
	publishSettingsGauges(store, st)

	gauges := st.GaugeSnapshot()
	if gauges[health.MetricRepoConnectionsTotal] != 1 {
		t.Errorf("repo_connections_total = %d, want 1", gauges[health.MetricRepoConnectionsTotal])
	}
	if gauges[health.MetricRepoConnectionsReady] != 1 {
		t.Errorf("repo_connections_ready = %d, want 1", gauges[health.MetricRepoConnectionsReady])
	}
	if gauges[health.CredentialAvailableMetricName("github")] != 1 {
		t.Errorf("credential_available_github = %d, want 1", gauges[health.CredentialAvailableMetricName("github")])
	}
}

// TestPublishSettingsGauges_NilStatusIsNoop proves the documented "nil st is a no-op" contract
// doesn't panic.
func TestPublishSettingsGauges_NilStatusIsNoop(t *testing.T) {
	store := settings.NewStore()
	publishSettingsGauges(store, nil) // must not panic
}

// TestRunPipeline_ReadyzCarriesSettingsReadinessDetail drives runPipeline end to end against an
// httptest sentinel server and asserts the health server's /readyz response -- reached through
// runWorker's exact wiring (SetReadyDetail installed from inside runPipeline's goroutine) -- ends
// up carrying settings.ReadinessDetail's shape, not just that the hook was assigned somewhere.
func TestRunPipeline_ReadyzCarriesSettingsReadinessDetail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projects":[]}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"cursor":"","hasMore":false}`))
	})
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	cfg := Config{
		WorkerEnabled:         true,
		SentinelURL:           srv.URL,
		SentinelAgentKey:      "test-key",
		WorkerPollInterval:    20 * time.Millisecond,
		WorkerStateDir:        dir,
		WorkerSettingsRefresh: 20 * time.Millisecond,
	}

	st := health.NewStatus()
	healthSrv := httptest.NewServer(health.Handler(st))
	defer healthSrv.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runPipeline(ctx, cfg, logger, st, nil, nil, nil)
		close(done)
	}()

	// Poll /readyz (through the real health.Handler, exactly as an operator would) until its
	// "detail" key carries the repo-connection shape runPipeline's SetReadyDetail hook installs --
	// proves the hook is actually reachable end to end, not just assigned somewhere in memory.
	deadline := time.Now().Add(3 * time.Second)
	var body string
	for {
		resp, err := healthSrv.Client().Get(healthSrv.URL + "/readyz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			if strings.Contains(body, "repoConnectionsTotal") || strings.Contains(body, "RepoConnectionsTotal") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for /readyz to carry settings readiness detail, last body: %s", body)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runPipeline did not exit within 3s of ctx cancellation")
	}
}

// --- OnSweepReconcile wiring (validator finding: Sweep.ReconcileReaped was unit-tested in
// isolation but unreachable from main() -- loop/queue.go dispatches a
// claim_released(previousAssignee=me) event as KindSweepReconcile and calls
// Dispatcher.OnSweepReconcile only if non-nil, and runPipeline's dispatcher construction never set
// that hook) ---

// TestRunPipeline_WiresOnSweepReconcile_ReclaimsIssueWithOpenQuestion drives the REAL runApp
// (exactly as main() calls it) against a fake sentinel server, feeding one claim_released event
// for an issue the journal shows an open (unanswered) question for. It proves the fix: the event
// reaches Sweep.ReconcileReaped and results in a re-claim POST, which was previously impossible
// because OnSweepReconcile was nil and loop/queue.go silently dropped the dispatch.
func TestRunPipeline_WiresOnSweepReconcile_ReclaimsIssueWithOpenQuestion(t *testing.T) {
	const issueID = "iss-reaped-1"
	var claimHits int32
	var eventServed int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.CompareAndSwapInt32(&eventServed, 0, 1) {
			// One claim_released event, previousAssignee == us, reason == stale (§2.7(c)).
			_, _ = w.Write([]byte(`{"events":[{"seq":1,"eventType":"claim_released","actorId":"system","actorType":"system","createdAt":"2026-08-19T00:00:00Z","issue":{"id":"` + issueID + `","status":"unresolved"},"newValue":{"previousAssignee":"agent-1","reason":"stale"}}],"hasMore":false,"cursor":1}`))
			return
		}
		// Subsequent polls: nothing new.
		_, _ = w.Write([]byte(`{"events":[],"hasMore":false,"cursor":1}`))
	})
	mux.HandleFunc("/api/agent/issues/"+issueID+"/claim", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&claimHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"claimed":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stateDir := t.TempDir()

	// Pre-seed the cursor so runPipeline skips Bootstrap entirely and goes straight to polling
	// (Bootstrap has its own well-tested path; this test is only about the reconcile wiring).
	if err := state.SaveCursor(stateDir+"/cursor.json", 0); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	// Pre-seed the journal with an open question for issueID -- ReconcileReaped only re-claims
	// when the journal shows an open question or open fix (a healthy release must never be
	// reconciled back, per its doc comment).
	journal := state.OpenJournal(stateDir + "/jobs.journal")
	if err := journal.Append(state.Record{JobID: "followup:" + issueID + ":1", IssueID: issueID, Kind: "followup", State: state.StateQuestioned}); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	cfg := Config{
		WorkerEnabled:        true,
		WorkerExecute:        true,
		SentinelURL:          srv.URL,
		SentinelAgentKey:     "test-key",
		WorkerPollInterval:   5 * time.Millisecond,
		WorkerSweepInterval:  time.Hour, // never fires during the test -- only the event-driven arm should
		WorkerClaimHeartbeat: 12 * time.Hour,
		WorkerNagDays:        3,
		WorkerHealthAddr:     "127.0.0.1:0",
		WorkerStateDir:       stateDir,
		LLMProvider:          "openai",
		LLMModel:             "gpt-4o-mini",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	if got := atomic.LoadInt32(&claimHits); got == 0 {
		t.Fatalf("expected the claim_released event (open question in journal) to reach Sweep.ReconcileReaped and re-claim the issue via OnSweepReconcile, got 0 claim POSTs -- the event-driven reconcile arm is unwired")
	}
}

// --- durability-startup remediation: finding 4 (startup free-disk guard) -----------------------

// TestCheckStateDirFreeDisk_RefusesBelowMinimum proves the plan §6 guard with a stubbed statfs
// (no real near-full filesystem needed): below the 100MB minimum it errors; at/above it, it does
// not.
func TestCheckStateDirFreeDisk_RefusesBelowMinimum(t *testing.T) {
	dir := t.TempDir()

	lowFree := func(string) (uint64, error) { return 50 * 1024 * 1024, nil }
	if err := checkStateDirFreeDisk(dir, lowFree); err == nil {
		t.Fatalf("expected an error when available disk (50MB) is below the 100MB minimum")
	}

	highFree := func(string) (uint64, error) { return 500 * 1024 * 1024, nil }
	if err := checkStateDirFreeDisk(dir, highFree); err != nil {
		t.Fatalf("expected no error when available disk (500MB) is above the minimum, got: %v", err)
	}

	exactFree := func(string) (uint64, error) { return minStateDirFreeBytes, nil }
	if err := checkStateDirFreeDisk(dir, exactFree); err != nil {
		t.Fatalf("expected no error when available disk exactly equals the minimum, got: %v", err)
	}
}

// TestRunWorker_LowDiskFlipsReadinessUnhealthy drives the REAL runWorker (exactly as runApp calls
// it) with statfsFree stubbed to report a near-full filesystem, and asserts /readyz reports
// unhealthy -- proving the guard is actually wired into runWorker's startup path, not merely a
// standalone function nothing calls.
func TestRunWorker_LowDiskFlipsReadinessUnhealthy(t *testing.T) {
	origStatfsFree := statfsFree
	statfsFree = func(string) (uint64, error) { return 1024, nil } // 1KB -- deliberately far below the minimum
	defer func() { statfsFree = origStatfsFree }()

	healthLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	healthAddr := healthLn.Addr().String()
	healthLn.Close()

	cfg := Config{
		WorkerEnabled:    false, // parks immediately -- this test is only about the readiness flip, not the pipeline
		WorkerHealthAddr: healthAddr,
		WorkerStateDir:   t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		runWorker(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), true)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	var lastBody string
	var lastStatus int
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + healthAddr + "/readyz")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastBody = string(body)
			lastStatus = resp.StatusCode
			if resp.StatusCode != http.StatusOK {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if lastStatus == http.StatusOK {
		t.Fatalf("expected /readyz to report NOT ready (non-200) once the free-disk guard sees a near-full filesystem, got 200: %s", lastBody)
	}
}

// --- durability-startup remediation: finding 1 (S3 state-snapshot durability) wired from main() --

// fakeS3Objects is a minimal in-memory S3 double shared by the two test servers below: PUT stores
// the body under its request path, GET returns it (404 if absent).
type fakeS3Objects struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newFakeS3Server(t *testing.T) (*httptest.Server, *fakeS3Objects) {
	t.Helper()
	f := &fakeS3Objects{objects: make(map[string][]byte)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.objects[r.URL.Path] = data
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			f.mu.Lock()
			data, ok := f.objects[r.URL.Path]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

// TestRunApp_S3SnapshotBackend_UploadsOnSIGTERMBeforeExit drives the REAL runApp (exactly as
// main() calls it) with WORKER_SNAPSHOT_BACKEND=s3 against a fake S3 server, cancels ctx
// (simulating SIGTERM, exactly like signal.NotifyContext does in main()), and asserts a snapshot
// was uploaded before runApp returned -- proving the S3 snapshotter is actually wired into
// runPipeline/runWorker's shutdown path, not merely constructed and never called.
func TestRunApp_S3SnapshotBackend_UploadsOnSIGTERMBeforeExit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"hasMore":false,"cursor":1}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s3srv, fakeS3 := newFakeS3Server(t)
	stateDir := t.TempDir()
	// Seed the cursor so runPipeline skips Bootstrap and gets straight to a state dir with content
	// worth snapshotting.
	if err := state.SaveCursor(stateDir+"/cursor.json", 0); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	cfg := Config{
		WorkerEnabled:          true,
		WorkerExecute:          true,
		SentinelURL:            srv.URL,
		SentinelAgentKey:       "test-key",
		WorkerPollInterval:     5 * time.Millisecond,
		WorkerSweepInterval:    time.Hour,
		WorkerClaimHeartbeat:   12 * time.Hour,
		WorkerNagDays:          3,
		WorkerHealthAddr:       "127.0.0.1:0",
		WorkerStateDir:         stateDir,
		WorkerShutdownTimeout:  time.Second,
		WorkerSnapshotBackend:  "s3",
		WorkerSnapshotInterval: time.Hour, // never fires during the test -- only the SIGTERM upload should
		S3Endpoint:             s3srv.URL,
		S3Bucket:               "test-bucket",
		S3Prefix:               "worker",
		S3AccessKey:            "AKIAEXAMPLE",
		S3SecretKey:            "secretkeyexample",
		LLMProvider:            "openai",
		LLMModel:               "gpt-4o-mini",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	fakeS3.mu.Lock()
	_, gotLatest := fakeS3.objects["/test-bucket/worker/latest"]
	numObjects := len(fakeS3.objects)
	fakeS3.mu.Unlock()

	if !gotLatest {
		t.Fatalf("expected a `latest` pointer object to have been uploaded to S3 on shutdown (SIGTERM-before-exit), got none of %d objects", numObjects)
	}
}

// TestRunApp_S3SnapshotBackend_RestoresOnEmptyStateDirStartup proves the startup half of the
// wiring: a worker started with WORKER_SNAPSHOT_BACKEND=s3 against an EMPTY state dir restores
// the newest snapshot uploaded by a prior run before it ever reaches the poll loop.
func TestRunApp_S3SnapshotBackend_RestoresOnEmptyStateDirStartup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"hasMore":false,"cursor":1}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	s3srv, _ := newFakeS3Server(t)

	// Pre-seed S3 directly (simulating a prior run's upload) via the same client the production
	// code uses, rather than hand-writing bytes into the fake -- this keeps the test bound to the
	// real Upload/RestoreLatest contract instead of the fake server's internals.
	snap := state.NewS3Snapshotter(state.S3Config{
		Endpoint:  s3srv.URL,
		Bucket:    "test-bucket",
		Prefix:    "worker",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: "secretkeyexample",
	}, s3srv.Client())
	seedDir := t.TempDir()
	if err := state.SaveCursor(seedDir+"/cursor.json", 99); err != nil {
		t.Fatalf("seed SaveCursor: %v", err)
	}
	tarball, err := state.BuildStateTarball(seedDir)
	if err != nil {
		t.Fatalf("BuildStateTarball: %v", err)
	}
	if err := snap.Upload(context.Background(), 1, tarball); err != nil {
		t.Fatalf("seed Upload: %v", err)
	}

	stateDir := t.TempDir() // EMPTY -- nothing local, exactly the emptyDir-after-reschedule case.
	cfg := Config{
		WorkerEnabled:          true,
		WorkerExecute:          true,
		SentinelURL:            srv.URL,
		SentinelAgentKey:       "test-key",
		WorkerPollInterval:     5 * time.Millisecond,
		WorkerSweepInterval:    time.Hour,
		WorkerClaimHeartbeat:   12 * time.Hour,
		WorkerNagDays:          3,
		WorkerHealthAddr:       "127.0.0.1:0",
		WorkerStateDir:         stateDir,
		WorkerShutdownTimeout:  time.Second,
		WorkerSnapshotBackend:  "s3",
		WorkerSnapshotInterval: time.Hour,
		S3Endpoint:             s3srv.URL,
		S3Bucket:               "test-bucket",
		S3Prefix:               "worker",
		S3AccessKey:            "AKIAEXAMPLE",
		S3SecretKey:            "secretkeyexample",
		LLMProvider:            "openai",
		LLMModel:               "gpt-4o-mini",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	restoredCursor, err := state.LoadCursor(stateDir + "/cursor.json")
	if err != nil {
		t.Fatalf("LoadCursor after startup: %v", err)
	}
	if restoredCursor == nil {
		t.Fatalf("expected cursor.json to have been restored from the S3 snapshot into the empty state dir, found none")
	}
	if restoredCursor.Seq != 99 {
		t.Fatalf("restored cursor.Seq = %d, want 99 (the seeded snapshot's value)", restoredCursor.Seq)
	}
}

// --- durability-startup remediation: finding 6 (5 missing /metrics families) wired from main() --

// TestRunApp_HeartbeatMetric_AppearsAtMetricsEndpoint drives the REAL runApp (exactly as main()
// calls it) with a held claim (GET /api/agent/issues?claimed=me returns one issue, with an empty
// journal so LastActivity is the zero Time -- trivially stale) and a short WORKER_SWEEP_INTERVAL,
// then scrapes the real /metrics endpoint over HTTP and asserts heartbeats_posted appears with a
// positive value -- proving jobs.Sweep.OnHeartbeatPosted is actually wired from main(), not merely
// constructed and never called (finding 6).
func TestRunApp_HeartbeatMetric_AppearsAtMetricsEndpoint(t *testing.T) {
	var heartbeatHits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"hasMore":false,"cursor":1}`))
	})
	mux.HandleFunc("/api/agent/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[{"id":"iss-heartbeat-1"}]}`))
	})
	mux.HandleFunc("/api/agent/issues/iss-heartbeat-1/progress", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&heartbeatHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	stateDir := t.TempDir()
	if err := state.SaveCursor(stateDir+"/cursor.json", 0); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	healthLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	healthAddr := healthLn.Addr().String()
	healthLn.Close()

	cfg := Config{
		WorkerEnabled:         true,
		WorkerExecute:         true,
		SentinelURL:           srv.URL,
		SentinelAgentKey:      "test-key",
		WorkerPollInterval:    5 * time.Millisecond,
		WorkerSweepInterval:   10 * time.Millisecond, // fire fast -- this test is about the sweep
		WorkerClaimHeartbeat:  0,                     // 0 duration: any LastActivity age is "stale", heartbeat fires immediately
		WorkerNagDays:         3,
		WorkerHealthAddr:      healthAddr,
		WorkerStateDir:        stateDir,
		WorkerShutdownTimeout: time.Second,
		LLMProvider:           "openai",
		LLMModel:              "gpt-4o-mini",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		close(done)
	}()

	// Poll /metrics until heartbeats_posted appears (or the test's own timeout below fires) --
	// avoids a fixed sleep racing the sweep ticker.
	deadline := time.Now().Add(2 * time.Second)
	var metricsBody string
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + healthAddr + "/metrics")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			metricsBody = string(body)
			if strings.Contains(metricsBody, "sentinel_worker_heartbeats_posted") {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	<-done

	if atomic.LoadInt32(&heartbeatHits) == 0 {
		t.Fatalf("expected at least one POST /api/agent/issues/:id/progress heartbeat, got none")
	}
	if !strings.Contains(metricsBody, "sentinel_worker_heartbeats_posted") {
		t.Fatalf("expected /metrics to expose sentinel_worker_heartbeats_posted after a real heartbeat POST, got:\n%s", metricsBody)
	}
}

// TestRunApp_LLMUsage_UpdatesTokenCounterAndBudgetGauge is the wired-from-main proof for the
// OnUsage closure main.go builds around line 2031: once a real Advisor decision flows through
// loop.Runner.Run (a real TriageAdvisor talking to a fake OpenAI-compatible LLM server that
// returns a usage-bearing response), sentinel_worker_llm_tokens_primary and
// sentinel_worker_budget_remaining must appear at /metrics with the expected values. Modeled on
// TestRunApp_HeartbeatMetric_AppearsAtMetricsEndpoint's poll-until-present pattern. Mutation-proof:
// neutering main.go's OnUsage body (~2031-2034) to a no-op makes both assertions below go red.
func TestRunApp_LLMUsage_UpdatesTokenCounterAndBudgetGauge(t *testing.T) {
	var eventsServed int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.CompareAndSwapInt32(&eventsServed, 0, 1) {
			_, _ = w.Write([]byte(`{"events":[{"seq":1,"eventType":"created","issue":{"id":"iss-usage-1","status":"unresolved","projectId":"proj-1"}}],"hasMore":false,"cursor":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[],"hasMore":false,"cursor":1}`))
	})
	mux.HandleFunc("/api/agent/issues/iss-usage-1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"issue": {"id":"iss-usage-1","projectId":"proj-1","message":"boom","errorClass":"Err","issueType":"user_report","status":"unresolved"},
			"report": {"bodyMd":"it broke"},
			"latestOccurrence": null
		}`))
	})
	mux.HandleFunc("/api/agent/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issues":[],"nextCursor":null}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// A minimal OpenAI-compatible chat/completions server: one final (non-tool-call) response
	// carrying a schema-valid TriageDecision plus a known, non-zero token usage.
	llmMux := http.NewServeMux()
	llmMux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {"content": "{\"severity\":\"low\",\"disposition\":\"comment_only\",\"duplicateOf\":null,\"causedBy\":null,\"summary\":\"benign\",\"question\":null,\"fixBrief\":null,\"confidence\":0.5}"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 30, "completion_tokens": 20}
		}`))
	})
	llmSrv := httptest.NewServer(llmMux)
	defer llmSrv.Close()

	stateDir := t.TempDir()
	if err := state.SaveCursor(stateDir+"/cursor.json", 0); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}

	healthLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	healthAddr := healthLn.Addr().String()
	healthLn.Close()

	cfg := Config{
		WorkerEnabled:          true,
		WorkerExecute:          false, // dry-run: OnUsage fires right after Decide, before Act
		SentinelURL:            srv.URL,
		SentinelAgentKey:       "test-key",
		WorkerPollInterval:     5 * time.Millisecond,
		WorkerHealthAddr:       healthAddr,
		WorkerStateDir:         stateDir,
		WorkerBackfillHours:    24,
		WorkerShutdownTimeout:  time.Second,
		WorkerDailyTokenBudget: 1000,
		LLMProvider:            "openai",
		LLMModel:               "test-model",
		LLMAPIKey:              "test-key",
		LLMBaseURL:             llmSrv.URL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		runApp(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		close(done)
	}()

	metricsURL := "http://" + healthAddr + "/metrics"
	deadline := time.Now().Add(2 * time.Second)
	var metricsBody string
	for time.Now().Before(deadline) {
		resp, err := http.Get(metricsURL)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			metricsBody = string(body)
			if strings.Contains(metricsBody, "sentinel_worker_llm_tokens_primary") {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	if !strings.Contains(metricsBody, "sentinel_worker_llm_tokens_primary 50") {
		t.Fatalf("expected /metrics to expose sentinel_worker_llm_tokens_primary 50 (30+20 tokens) after a real Advisor decision, got:\n%s", metricsBody)
	}
	if !strings.Contains(metricsBody, "sentinel_worker_budget_remaining 950") {
		t.Fatalf("expected /metrics to expose sentinel_worker_budget_remaining 950 (1000 - 50 spent), got:\n%s", metricsBody)
	}
}

// TestGuard_OnRejectionHook_FiresOnRealCheckRejection proves finding 6's gate_rejections seam:
// guardpkg.OnRejection fires exactly when guardpkg.Check/CheckWithConfig actually rejects a
// candidate (driven through the REAL Check entry point, not a hand-constructed Violation), and
// does not fire on an accepted candidate. main.go wires this package-level hook to
// health.Status.Inc(health.MetricGateRejections, 1) in runPipeline.
func TestGuard_OnRejectionHook_FiresOnRealCheckRejection(t *testing.T) {
	origHook := guardpkg.OnRejection
	defer func() { guardpkg.OnRejection = origHook }()

	var fired int32
	var lastField guardpkg.PublishedField
	guardpkg.OnRejection = func(field guardpkg.PublishedField, reason guardpkg.ViolationReason) {
		atomic.AddInt32(&fired, 1)
		lastField = field
	}

	// A too-long candidate for the "comment" field triggers the length check -- driven through the
	// REAL guardpkg.Check entry point, exactly as jobs/act.go calls it.
	longText := strings.Repeat("x", 100000)
	if err := guardpkg.Check(guardpkg.FieldSummary, longText, nil, nil); err == nil {
		t.Fatalf("expected guardpkg.Check to reject an oversized candidate")
	}
	if atomic.LoadInt32(&fired) == 0 {
		t.Fatalf("expected guardpkg.OnRejection to fire on a real rejection, got 0 calls")
	}
	if lastField != guardpkg.FieldSummary {
		t.Fatalf("OnRejection field = %v, want %v", lastField, guardpkg.FieldSummary)
	}

	atomic.StoreInt32(&fired, 0)
	if err := guardpkg.Check(guardpkg.FieldSummary, "a short, fine comment", nil, nil); err != nil {
		t.Fatalf("expected a short candidate to pass: %v", err)
	}
	if atomic.LoadInt32(&fired) != 0 {
		t.Fatalf("OnRejection must NOT fire on an accepted candidate, got %d calls", fired)
	}
}

// --- FIX-subsystem remediation: findings 2/3/6 wired from main() -------------------------------

// fixMinimalMux returns an httptest mux answering the minimal sentinel API surface runPipeline
// needs to get all the way through buildFixRunner: /api/agent/self, /api/agent/projects (one
// project with a repo connection so buildRunner's settingsStore has FIX-ready data),
// /api/agent/repo-credentials, and an empty /api/agent/events feed so the poll loop idles quietly.
func fixMinimalMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/self", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1"}`))
	})
	mux.HandleFunc("/api/agent/projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"projects":[{"id":"proj-1","name":"P1","agentSettings":{"fixEnabled":true,"repo":{"provider":"github","owner":"o","repo":"r","defaultBranch":"main"}}}]}`))
	})
	mux.HandleFunc("/api/agent/repo-credentials", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"tok"}}]}`))
	})
	mux.HandleFunc("/api/agent/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[],"cursor":"","hasMore":false}`))
	})
	return mux
}

// TestRunPipeline_WiresFixRunnerAndSeedsCapsFromJournal is findings 1/2's wired-from-main proof:
// runPipeline is driven end to end (exactly as runWorker calls it) against a real journal file
// pre-seeded, via the SAME journal record shapes RunFix itself writes, with today's FIX activity.
// It asserts (a) buildFixRunner's *jobs.FixRunner is actually published through fixRunnerOut (the
// call path runWorker's shutdown reads to find it) and (b) that FixRunner's Caps already reflect
// the seeded journal counts -- proving SeedToday ran as part of runPipeline's boot, not merely
// existing as an unused method.
func TestRunPipeline_WiresFixRunnerAndSeedsCapsFromJournal(t *testing.T) {
	srv := httptest.NewServer(fixMinimalMux())
	defer srv.Close()

	dir := t.TempDir()
	journalPath := filepath.Join(dir, "jobs.journal")
	j := state.OpenJournal(journalPath)
	today := time.Now().UTC()

	// Seed TWO distinct FIX job starts for today via the exact journal shape journalFixRunning
	// writes (Kind=jobs.FixKind, State fix_running), through the exported FixJobInput/JournalFixPROpen
	// surface this package can reach.
	for _, jobID := range []string{"seed-job-1", "seed-job-2"} {
		payload, err := json.Marshal(struct {
			Input      jobs.FixJobInput `json:"input"`
			BaseCommit string           `json:"baseCommit"`
		}{
			Input:      jobs.FixJobInput{JobID: jobID, IssueID: "issue-" + jobID, ProjectID: "proj-1"},
			BaseCommit: "deadbeef",
		})
		if err != nil {
			t.Fatalf("marshal seed payload: %v", err)
		}
		if err := j.Append(state.Record{
			JobID: jobID, IssueID: "issue-" + jobID, Kind: jobs.FixKind, State: state.StateFixRunning,
			At: today, Payload: payload,
		}); err != nil {
			t.Fatalf("seeding journal: %v", err)
		}
	}

	cfg := Config{
		WorkerEnabled:          true,
		WorkerExecute:          true,
		WorkerFixEnabled:       true,
		SentinelURL:            srv.URL,
		SentinelAgentKey:       "test-key",
		WorkerPollInterval:     20 * time.Millisecond,
		WorkerStateDir:         dir,
		WorkerWorkspaceDir:     filepath.Join(dir, "workspaces"),
		FixExecutorCmd:         "true",
		WorkerMaxFixJobsPerDay: 2, // exactly the 2 jobs seeded above -- a 3rd must be refused if seeding worked
		WorkerMaxPRsPerDay:     10,
		WorkerMaxFixAttempts:   2,
		LLMProvider:            "openai",
		LLMModel:               "gpt-4o-mini",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var fixRunnerPtr atomic.Pointer[jobs.FixRunner]

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runPipeline(ctx, cfg, logger, nil, nil, &fixRunnerPtr, nil)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	var fr *jobs.FixRunner
	for time.Now().Before(deadline) {
		if fr = fixRunnerPtr.Load(); fr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fr == nil {
		t.Fatal("runPipeline never published a *jobs.FixRunner through fixRunnerOut -- buildFixRunner is unreachable from runPipeline's real wiring")
	}
	if fr.Caps == nil {
		t.Fatal("published FixRunner has no Caps")
	}
	if fr.Caps.AllowJobStart() {
		t.Fatal("WORKER_MAX_FIX_JOBS_PER_DAY=2 should already be exhausted by the 2 fix_running records seeded in the journal BEFORE runPipeline started -- SeedToday did not run at boot")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runPipeline did not exit within 3s of ctx cancellation")
	}
}

// TestRunPipeline_SweepsOrphanFixWorkspaceAtStartup is finding 6's wired-from-main proof: a
// leftover workspace directory (no journal record at all -- an "unknown" orphan) sitting under
// WORKER_WORKSPACE_DIR before runPipeline ever starts must be gone once buildFixRunner's startup
// sweep has run, and a directory whose jobId IS still in-flight per the journal must survive.
func TestRunPipeline_SweepsOrphanFixWorkspaceAtStartup(t *testing.T) {
	srv := httptest.NewServer(fixMinimalMux())
	defer srv.Close()

	dir := t.TempDir()
	workspaceDir := filepath.Join(dir, "workspaces")
	if err := os.MkdirAll(filepath.Join(workspaceDir, "orphan-job"), 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(workspaceDir, "inflight-job"), 0o755); err != nil {
		t.Fatalf("mkdir inflight: %v", err)
	}

	journalPath := filepath.Join(dir, "jobs.journal")
	j := state.OpenJournal(journalPath)
	payload, _ := json.Marshal(struct {
		Input      jobs.FixJobInput `json:"input"`
		BaseCommit string           `json:"baseCommit"`
	}{Input: jobs.FixJobInput{JobID: "inflight-job", IssueID: "issue-inflight", ProjectID: "proj-1"}, BaseCommit: "x"})
	if err := j.Append(state.Record{
		JobID: "inflight-job", IssueID: "issue-inflight", Kind: jobs.FixKind, State: state.StateFixRunning,
		At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		t.Fatalf("seeding journal: %v", err)
	}

	cfg := Config{
		WorkerEnabled:                true,
		WorkerExecute:                true,
		WorkerFixEnabled:             true,
		SentinelURL:                  srv.URL,
		SentinelAgentKey:             "test-key",
		WorkerPollInterval:           20 * time.Millisecond,
		WorkerStateDir:               dir,
		WorkerWorkspaceDir:           workspaceDir,
		FixExecutorCmd:               "true",
		WorkerMaxFixJobsPerDay:       10,
		WorkerMaxPRsPerDay:           10,
		WorkerMaxFixAttempts:         2,
		WorkerWorkspaceRetentionDays: 3,
		LLMProvider:                  "openai",
		LLMModel:                     "gpt-4o-mini",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Finding 2 (fix-lifecycle remediation round 2): boot-time FIX recovery now runs via
	// DispatchResume, off runPipeline's own goroutine, tracked by fixRunner's WaitGroup -- capture
	// it via fixRunnerOut so this test can wait for that resume attempt to actually finish before
	// asserting on (and before t.TempDir() cleans up) the workspace it touches, instead of racing
	// runPipeline's own ctx-cancellation exit against a still-running background goroutine.
	var fixRunnerOut atomic.Pointer[jobs.FixRunner]
	done := make(chan struct{})
	go func() {
		runPipeline(ctx, cfg, logger, nil, nil, &fixRunnerOut, nil)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(workspaceDir, "orphan-job")); os.IsNotExist(err) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runPipeline did not exit within 3s of ctx cancellation")
	}

	if fr := fixRunnerOut.Load(); fr != nil {
		waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer waitCancel()
		fr.Wait(waitCtx)
	}

	if _, err := os.Stat(filepath.Join(workspaceDir, "orphan-job")); !os.IsNotExist(err) {
		t.Fatalf("orphan-job workspace should have been removed by the startup sweep, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, "inflight-job")); err != nil {
		t.Fatalf("inflight-job workspace (non-terminal journal record) should have survived the startup sweep: %v", err)
	}
}

// --- Fix-lifecycle remediation round 2, finding 4: WORKER_FIX_EXECUTOR_ENV --------------------

// TestLoadConfig_WorkerFixExecutorEnv_ParsesKeyValueAndBareKeyEntries proves the comma/newline
// KEY=VALUE-or-bare-KEY parsing: a literal KEY=VALUE entry is taken verbatim; a bare KEY entry is
// passed through from the worker's own process environment (via the env lookup LoadConfig itself
// uses) if set, and silently omitted if not.
func TestLoadConfig_WorkerFixExecutorEnv_ParsesKeyValueAndBareKeyEntries(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_ENABLED":          "false",
		"WORKER_FIX_EXECUTOR_ENV": "DEEPSEEK_API_KEY=sk-abc123,\nMODEL_NAME\nUNSET_PASSTHROUGH_KEY",
		"MODEL_NAME":              "deepseek-coder",
		// UNSET_PASSTHROUGH_KEY is deliberately NOT set in this env.
	}))
	if len(errs) != 0 {
		t.Fatalf("expected zero validation errors, got: %v", errs)
	}
	if got := cfg.WorkerFixExecutorEnv["DEEPSEEK_API_KEY"]; got != "sk-abc123" {
		t.Errorf("DEEPSEEK_API_KEY = %q, want sk-abc123", got)
	}
	if got := cfg.WorkerFixExecutorEnv["MODEL_NAME"]; got != "deepseek-coder" {
		t.Errorf("MODEL_NAME (bare-key passthrough) = %q, want deepseek-coder", got)
	}
	if _, ok := cfg.WorkerFixExecutorEnv["UNSET_PASSTHROUGH_KEY"]; ok {
		t.Errorf("UNSET_PASSTHROUGH_KEY should have been silently omitted (not set in env), but was present")
	}
}

// TestLoadConfig_WorkerFixExecutorEnv_RejectsForbiddenKey is the RED-FIRST proof that
// WORKER_FIX_EXECUTOR_ENV cannot be used to smuggle the worker's own secrets to the Fix Executor --
// a config-time validation error, not a silent drop or a runtime-only rejection.
//
// MUTATION-TEST NOTE: remove the jobs.IsForbiddenExecutorEnvKey check from parseFixExecutorEnv and
// this test goes red -- errs comes back empty and cfg.WorkerFixExecutorEnv["SENTINEL_AGENT_KEY"]
// carries the leaked value.
func TestLoadConfig_WorkerFixExecutorEnv_RejectsForbiddenKey(t *testing.T) {
	cfg, errs := LoadConfig(fakeEnv(map[string]string{
		"WORKER_ENABLED":          "false",
		"WORKER_FIX_EXECUTOR_ENV": "SENTINEL_AGENT_KEY=leaked-value",
	}))
	if len(errs) == 0 {
		t.Fatal("expected a validation error for a forbidden WORKER_FIX_EXECUTOR_ENV key, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e, "WORKER_FIX_EXECUTOR_ENV") && strings.Contains(e, "SENTINEL_AGENT_KEY") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a WORKER_FIX_EXECUTOR_ENV/SENTINEL_AGENT_KEY validation error, got: %v", errs)
	}
	if _, ok := cfg.WorkerFixExecutorEnv["SENTINEL_AGENT_KEY"]; ok {
		t.Fatal("SENTINEL_AGENT_KEY must never be present in the parsed WorkerFixExecutorEnv map")
	}
}

// TestConfiguredSecrets_IncludesFixExecutorEnvValues is finding 6's RED-FIRST proof: the Fix
// Executor's own credential env (WORKER_FIX_EXECUTOR_ENV, Config.WorkerFixExecutorEnv) must be
// included in configuredSecrets's output -- before this fix, only LLM/git credentials were
// collected, so a coding-agent API key configured this way never reached FixRunner.Secrets and
// never got masked by any redactor built from jobSecrets in jobs.FixRunner.RunFix/ResumeFix.
//
// MUTATION-TEST NOTE: delete the WorkerFixExecutorEnv loop from configuredSecrets and this test
// goes red -- the returned slice no longer contains the executor secret value.
func TestConfiguredSecrets_IncludesFixExecutorEnvValues(t *testing.T) {
	cfg := Config{
		WorkerFixExecutorEnv: map[string]string{
			"DEEPSEEK_API_KEY": "sk-fix-executor-super-secret-token",
			"EMPTY_VALUE_KEY":  "",
		},
	}
	secrets := configuredSecrets(cfg)
	found := false
	for _, s := range secrets {
		if s == "sk-fix-executor-super-secret-token" {
			found = true
		}
		if s == "" {
			t.Fatal("configuredSecrets must never include an empty string, even for an empty-valued executor env entry")
		}
	}
	if !found {
		t.Fatalf("expected configuredSecrets to include the Fix Executor's own env secret, got %v", secrets)
	}
}

// TestBuildFixRunner_ExecutorEnvReachesTheRealExecutorChild is the wired-end-to-end proof for
// finding 4 (BLOCKER-adjacent, plan §4.4): before this fix, FixRunner.ExecutorEnv was NEVER
// populated by main.go's real wiring -- buildFixRunner's returned *jobs.FixRunner always had a nil
// ExecutorEnv, so jobs/fix_executor_test.go's forbidden-key guard test was exercising a dead field
// (nothing in production ever called RunFixExecutor with a non-nil ExtraEnv). This drives
// buildFixRunner (the REAL main.go wiring, not a hand-built jobs.FixRunner) with
// WORKER_FIX_EXECUTOR_ENV configured, then runs an actual FIX attempt through it whose
// $FIX_EXECUTOR_CMD script asserts the configured var is present in ITS OWN environment.
//
// MUTATION-TEST NOTE: remove `ExecutorEnv: cfg.WorkerFixExecutorEnv,` from buildFixRunner's struct
// literal and this test goes red -- the script's `test -n "$DEEPSEEK_API_KEY"` check fails, causing
// execResult.Err != nil and the attempt to release-with-comment instead of reaching CreatePR.
func TestBuildFixRunner_ExecutorEnvReachesTheRealExecutorChild(t *testing.T) {
	bareRepo := newMainTestBareFixtureRepo(t)
	journalPath := filepath.Join(t.TempDir(), "jobs.journal")
	journal := state.OpenJournal(journalPath)
	fp := &mainTestFakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}
	sender := &mainTestRecordingSender{}

	cfg := Config{
		WorkerFixEnabled:       true,
		WorkerExecute:          true,
		FixExecutorCmd:         `if [ "$DEEPSEEK_API_KEY" != "sk-real-value" ]; then echo "missing executor env" >&2; exit 1; fi; echo "fix applied" >> fixed.txt`,
		WorkerFixExecutorEnv:   map[string]string{"DEEPSEEK_API_KEY": "sk-real-value"},
		WorkerWorkspaceDir:     t.TempDir(),
		WorkerStateDir:         t.TempDir(),
		WorkerMaxFixJobsPerDay: 10,
		WorkerMaxPRsPerDay:     10,
		WorkerMaxFixAttempts:   2,
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fixRunner := buildFixRunner(cfg, nil, journal, nil, logger, nil)
	if fixRunner == nil {
		t.Fatal("buildFixRunner returned nil despite WorkerFixEnabled+WorkerExecute+FixExecutorCmd all set")
	}
	fixRunner.Client = sender
	fixRunner.ResolveRepo = func(projectID string) (jobs.FixRepoConfig, bool, error) {
		return jobs.FixRepoConfig{
			Provider:      fp,
			Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
			CloneURL:      bareRepo,
			DefaultBranch: "main",
		}, true, nil
	}

	in := jobs.FixJobInput{JobID: "job-execenv", IssueID: "issue-execenv", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	if err := fixRunner.RunFix(context.Background(), in); err != nil {
		t.Fatalf("RunFix: %v", err)
	}
	if len(fp.created) != 1 {
		t.Fatalf("expected exactly one CreatePR call (the executor's own env-presence check must have passed), got %d", len(fp.created))
	}
}

// TestResumeInFlightJob_SlowFixRunsAsyncAndDoesNotBlockCaller is the RED-FIRST proof for finding 2
// (MAJOR, fix-lifecycle remediation round 2): before this fix, resumeInFlightJob's FIX branch
// called fixRunner.ResumeFix SYNCHRONOUSLY -- so main.go's boot-time recovery loop (runPipeline,
// which calls resumeInFlightJob for every in-flight job BEFORE the poll loop starts) blocked on the
// full duration of a slow/in-progress FIX attempt before ever reaching the poll loop. This drives
// resumeInFlightJob directly (the exact call site runPipeline's recovery loop uses) against a REAL
// jobs.FixRunner whose Fix Executor deliberately sleeps for a multi-second duration, and asserts
// resumeInFlightJob itself returns near-instantly -- proving the caller (runPipeline, and by
// extension the poll loop it starts immediately afterward) is never blocked on the attempt's own
// duration. fixRunner.Wait (the shutdown-time drain path) is used afterward to prove the resumed
// attempt actually did run to completion in the background, not that it was dropped.
//
// MUTATION-TEST NOTE: change resumeInFlightJob's FIX branch back to `if err :=
// fixRunner.ResumeFix(ctx, payload.Input); err != nil { ... }` (its pre-fix, synchronous shape) and
// this test goes red -- resumeInFlightJob does not return until the multi-second sleep completes,
// blowing the generous-but-bounded "returned promptly" deadline this test asserts against.
func TestResumeInFlightJob_SlowFixRunsAsyncAndDoesNotBlockCaller(t *testing.T) {
	bareRepo := newMainTestBareFixtureRepo(t)
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	sender := &mainTestRecordingSender{}
	fp := &mainTestFakeProvider{pr: gitprovider.PR{ID: "1", URL: "https://example/pr/1"}}

	const sleepSeconds = 3
	fixRunner := &jobs.FixRunner{
		WorkspaceRoot: t.TempDir(),
		Journal:       journal,
		Client:        sender,
		Sink:          jobs.LocalDirArtifactSink{Root: t.TempDir()},
		Caps:          jobs.NewFixCaps(10, 10, 2, nil),
		Timeout:       10 * time.Second,
		ResolveRepo: func(projectID string) (jobs.FixRepoConfig, bool, error) {
			return jobs.FixRepoConfig{
				Provider:      fp,
				Repo:          gitprovider.RepoRef{Provider: "github", Owner: "o", Repo: "r"},
				CloneURL:      bareRepo,
				DefaultBranch: "main",
			}, true, nil
		},
		// Deliberately slow: a real coding-agent CLI invocation would take at least this long, and
		// nothing about resumeInFlightJob (the RED-FIRST assertion below) should have to wait for it.
		ExecutorCmd: fmt.Sprintf(`sleep %d && echo "fix applied" >> fixed.txt && echo "applied fix" >> "$PROGRESS_MD"`, sleepSeconds),
	}

	in := jobs.FixJobInput{JobID: "slow-job", IssueID: "issue-slow", ProjectID: "proj-1", FixBrief: "x", TriggerSeq: 1}
	payload, err := json.Marshal(struct {
		Input      jobs.FixJobInput `json:"input"`
		BaseCommit string           `json:"baseCommit"`
	}{Input: in, BaseCommit: "deadbeef"})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := journal.Append(state.Record{
		JobID: in.JobID, IssueID: in.IssueID, Kind: jobs.FixKind, TriggerSeq: in.TriggerSeq,
		State: state.StateFixRunning, Payload: payload,
	}); err != nil {
		t.Fatalf("journal.Append: %v", err)
	}

	inFlight, _, err := journal.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if len(inFlight) != 1 {
		t.Fatalf("expected exactly one in-flight job, got %d", len(inFlight))
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()
	resumeInFlightJob(context.Background(), nil, fixRunner, inFlight[0], logger)
	elapsed := time.Since(start)

	// A generous bound (well under sleepSeconds) that only a SYNCHRONOUS ResumeFix call could ever
	// blow: DispatchResume's own goroutine/timeout/wg bookkeeping is essentially free.
	if elapsed >= (sleepSeconds/2)*time.Second {
		t.Fatalf("resumeInFlightJob took %s to return -- it must return promptly (before the FIX attempt itself finishes), not block on ResumeFix's own duration", elapsed)
	}

	// Prove the resumed attempt genuinely ran to completion in the background (not silently
	// dropped): wait for it, bounded, then assert it reached CreatePR.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	fixRunner.Wait(waitCtx)

	if len(fp.created) != 1 {
		t.Fatalf("expected the resumed FIX job to have reached CreatePR exactly once in the background, got %d", len(fp.created))
	}
}
