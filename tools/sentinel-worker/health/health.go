// Package health implements sentinel-worker's health/metrics HTTP surface (plan §6/§7):
// /healthz (process up), /readyz (cursor persisted recently AND auth valid AND config valid), and
// a hand-rolled Prometheus text /metrics — stdlib only, no client library.
package health

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Metric name constants for the §7 metrics N8a actually produces (poll loop + runner), declared
// here so main.go's wiring and this package's golden test cannot silently drift apart (validator
// finding: every metric name previously existed only inside health_test.go, not in any production
// code path). §7 also lists LLM tokens by provider, budget/volume-cap remaining, circuit states,
// heartbeats posted, and fix attempts/PRs opened, plus gate rejections (the prompt-injection
// guard) -- those all belong to seams N8a deliberately does not ship (LLM adapters land in N8d,
// the guard alongside them, budgets/circuits/heartbeats in N8b/N8f per the plan's phase split) and
// are therefore NOT wired here; that is a recorded scope decision, not an oversight.
const (
	// MetricEventsConsumed counts every feed event the poll loop drains and hands to the
	// dispatcher (loop.PollLoop.OnEvent), regardless of job kind or outcome.
	MetricEventsConsumed = "events_consumed"
	// MetricCursorLag is a point-in-time backlog signal set from loop.PollLoop.OnPageDrained: 0
	// once a poll cycle has fully drained the feed (hasMore=false), otherwise the size of the last
	// still-pending page. GET /api/agent/events never reports the feed's true head seq (only the
	// cursor of the page just returned -- see events.ts's listOrgActivity), so this is a "still
	// catching up, roughly this many more in flight" signal, not an exact head-minus-cursor count.
	MetricCursorLag = "cursor_lag"
	// MetricBootstrapEnqueued counts synthetic TRIAGE jobs a bootstrap sweep actually enqueued
	// (backfilled instead of being (re)discovered via feed replay). Kept distinct from
	// MetricBootstrapSkipped: the two used to be conflated under one counter that was incremented
	// with the ENQUEUED count under a "skipped" name (validator finding) -- plan §2.1 wants
	// bootstrap-SKIPPED counted separately, not folded into the enqueued total.
	MetricBootstrapEnqueued = "bootstrap_enqueued"
	// MetricBootstrapSkipped counts issues a bootstrap sweep deliberately did NOT enqueue a
	// synthetic job for -- currently loop.Bootstrap's step 4 sweep-window-delta issues left to the
	// feed (firstSeen not safely older than the head-capture cutoff, see loop/bootstrap.go).
	MetricBootstrapSkipped = "bootstrap_skipped"
	// MetricHeldClaimsAtBootstrap is a gauge of how many issues loop.Bootstrap's step 2 found
	// already claimed by this agent from a previous life (Result.HeldClaimIssueIDs). Previously
	// computed and then discarded (B3 pattern); this is the seam the N8d claim-sweep will consume
	// once it exists, and in the meantime makes the held-claims view observable at /metrics.
	MetricHeldClaimsAtBootstrap = "held_claims_at_bootstrap"
	// MetricJournalCorruptLines counts jobs.journal lines that failed to parse and were skipped
	// (state.Journal.Load's corrupt-line tolerance), accumulated across every maintenance pass'
	// scan so a corrupted durability layer is observable at /metrics, not just in a WARN log line
	// (validator finding: the count used to be computed and then discarded by every caller).
	MetricJournalCorruptLines = "journal_corrupt_lines"
	// MetricRepoConnectionsTotal / MetricRepoConnectionsReady are gauges from
	// settings.Store.Readiness (plan §4.5, N8c): how many projects carry a repo connection, and how
	// many of those currently have a usable credential (server-resolved or env fallback) for their
	// connection's provider.
	MetricRepoConnectionsTotal = "repo_connections_total"
	MetricRepoConnectionsReady = "repo_connections_ready"
	// credentialAvailablePrefix names the per-provider gauge family
	// "credential_available_<provider>" (1 = usable credential available, 0 = not), built via
	// CredentialAvailableMetricName so main.go's wiring and any test agree on the name.
	credentialAvailablePrefix = "credential_available"
	// jobsTotalPrefix names the counter family for "jobs by kind×outcome" (plan §7). There is no
	// label support in the hand-rolled exposition format (renderMetrics), so kind and outcome are
	// folded into the metric name itself via JobsTotalMetricName.
	jobsTotalPrefix = "jobs_total"
	// MetricInlaneRetriesTotal counts every in-lane retry the runner drives a job through (plan
	// §2.4/§9 N8e: a Transient/RateLimited failure re-driven through sentinel.BackoffForAttempt
	// without leaving the per-issue queue) -- incremented once per retry attempt, not once per job.
	MetricInlaneRetriesTotal = "inlane_retries_total"
	// circuitOpenEventsPrefix names the per-scope counter family "circuit_open_events_<scope>"
	// (plan §7 "circuit-open events"), incremented each time a CircuitBreaker transitions from
	// closed to open for that scope. Built via CircuitOpenEventsMetricName.
	circuitOpenEventsPrefix = "circuit_open_events"
	// circuitStatePrefix names the per-scope gauge family "circuit_state_<scope>" (plan §7 "circuit
	// state gauge per scope"): 0=closed, 1=open, 2=half-open (sentinel.CircuitState's own values).
	// Built via CircuitStateMetricName.
	circuitStatePrefix = "circuit_state"
)

// CircuitOpenEventsMetricName builds the flat counter name for one dependency scope's circuit-open
// event count (e.g. circuit_open_events_sentinel_api), same sanitize-then-flatten convention as
// JobsTotalMetricName/CredentialAvailableMetricName.
func CircuitOpenEventsMetricName(scope string) string {
	return sanitizeMetricName(circuitOpenEventsPrefix + "_" + scope)
}

// CircuitStateMetricName builds the flat gauge name for one dependency scope's current
// sentinel.CircuitState (e.g. circuit_state_sentinel_api).
func CircuitStateMetricName(scope string) string {
	return sanitizeMetricName(circuitStatePrefix + "_" + scope)
}

// JobsTotalMetricName builds the flat counter name for one (kind, outcome) pair of plan §7's
// "jobs by kind×outcome" (e.g. jobs_total_triage_done, jobs_total_followup_skipped_foreign_claim).
// Shared by loop.Runner's caller (main.go) and this package's tests so the naming scheme lives in
// exactly one place. loop.Kind and loop.SkipReason values are hyphenated (e.g. "sweep-reconcile",
// "foreign-claim") which is not legal inside a Prometheus metric name
// ([a-zA-Z_:][a-zA-Z0-9_:]*) -- sanitizeMetricName maps every disallowed byte to '_' so the
// resulting family never produces a line that breaks the whole /metrics scrape.
func JobsTotalMetricName(kind, outcome string) string {
	return sanitizeMetricName(jobsTotalPrefix + "_" + kind + "_" + outcome)
}

// CredentialAvailableMetricName builds the flat gauge name for one provider's credential
// availability (e.g. credential_available_github). Shared by main.go's wiring and this package's
// tests, same convention as JobsTotalMetricName.
func CredentialAvailableMetricName(provider string) string {
	return sanitizeMetricName(credentialAvailablePrefix + "_" + provider)
}

// sanitizeMetricName maps a candidate metric name to one that satisfies the Prometheus text
// exposition format's name grammar ^[a-zA-Z_:][a-zA-Z0-9_:]*$: every byte outside
// [a-zA-Z0-9_:] becomes '_', and if the result would start with a digit (or be empty) it is
// prefixed with '_'. Applied inside renderMetrics too (not just JobsTotalMetricName) so any
// future metric source -- not only jobs-by-kind×outcome -- cannot reintroduce an invalid name
// that would silently drop every series in the scrape, not just its own.
func sanitizeMetricName(name string) string {
	if name == "" {
		return "_"
	}
	b := []byte(name)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '_' && c != ':' {
			b[i] = '_'
		}
	}
	if b[0] >= '0' && b[0] <= '9' {
		return "_" + string(b)
	}
	return string(b)
}

// Status is the mutable readiness state other packages update as they learn things (cursor
// persisted, auth failing, config invalid, ...). All fields are safe for concurrent access.
//
// plan §7 defines /readyz as "cursor persisted recently AND auth valid AND config valid" — the
// "cursor persisted recently" leg is computed from lastCursorSave against cursorFreshness, not
// just recorded and left unread (NoteCursorSaved used to be dead API: nothing called Ready()'s
// staleness check because nothing set cursorFreshness or wired a save hook into the poll loop --
// see loop.PollLoop.OnCursorSaved and main.go's runPipeline).
type Status struct {
	mu              sync.RWMutex
	ready           bool
	reasons         []string
	lastCursorSave  time.Time
	cursorFreshness time.Duration // 0 = staleness check disabled (e.g. before the loop starts)
	authValid       bool          // plan §7's "auth valid" leg of /readyz

	counters sync.Map // map[string]*int64, monotonic (Inc), for /metrics as Prometheus "counter"
	gauges   sync.Map // map[string]*int64, point-in-time (SetGauge), for /metrics as Prometheus "gauge"

	// readyDetail, when non-nil, is called on every /readyz request and its return value is
	// embedded under the response's "detail" key. Wired by main.go to settings.Store.Readiness so
	// per-provider credential availability and repo-connection counts (plan §4.5, N8c) are visible
	// on the same endpoint operators already poll for readiness -- never affects the ready/not-ready
	// verdict itself (a provider with no usable credential is reported, not fatal, per C16).
	//
	// Held behind an atomic.Pointer, not a bare field, because the health HTTP server starts
	// serving (main.go's runWorker) before runPipeline's goroutine has assembled settingsStore and
	// can call SetReadyDetail -- the same ordering hazard main.go already solved for its
	// *loop.Dispatcher via dispatcherPtr. A bare field here raced under -race: the /readyz handler's
	// read (readyDetailHook below) and runPipeline's write are on different goroutines with no
	// synchronization between them.
	readyDetail atomic.Pointer[func() any]
}

// NewStatus returns a Status that starts NOT ready (matches plan §6: "invalid config keeps the
// process up with /readyz failing rather than exiting into a restart loop" — readiness must be
// earned, not assumed). authValid starts true: no evidence of an invalid credential exists yet,
// and requiring an explicit SetAuthValid(true) before the first successful call would make
// readyz permanently false until the first request happens to be a 401, which is backwards.
func NewStatus() *Status {
	return &Status{authValid: true}
}

// SetReady marks the process ready or not, with human-readable reasons for the latter (surfaced on
// /readyz for operator debugging).
func (s *Status) SetReady(ready bool, reasons ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ready = ready
	s.reasons = reasons
}

// SetCursorFreshnessWindow configures how old lastCursorSave may get before Ready() reports
// not-ready on staleness grounds (plan §7). Callers pick a small multiple of WORKER_POLL_INTERVAL
// so a merely-slow tick doesn't flap readiness, while a genuinely wedged poll loop does trip it.
func (s *Status) SetCursorFreshnessWindow(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursorFreshness = d
}

// NoteCursorSaved records the time of the most recent successful cursor persist, part of the
// /readyz definition (plan §7). Called from loop.PollLoop via the OnCursorSaved hook after every
// successful state.SaveCursor.
func (s *Status) NoteCursorSaved(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastCursorSave = at
}

// SetAuthValid records whether the most recent sentinel API call authenticated successfully (plan
// §7's "auth valid" leg of /readyz). sentinel.Client.OnAuthStatus plugs into this: a 401 response
// calls SetAuthValid(false); any subsequent successful (2xx) response calls SetAuthValid(true),
// clearing it. This is deliberately NOT latched forever on the first 401 -- keyguard rotation
// (plan §2.5) is expected to recover the credential without a restart, and readyz must reflect
// that recovery, not just the first failure.
func (s *Status) SetAuthValid(valid bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authValid = valid
}

// Ready reports the current readiness state and reasons (plan §7: "cursor persisted recently AND
// auth valid AND config valid"). When a cursor-freshness window is configured
// (SetCursorFreshnessWindow) and the last recorded cursor save is older than that window -- or
// none has ever been recorded -- Ready reports false with a stale-cursor reason layered on top of
// whatever SetReady last recorded, so a wedged poll loop (or one that has not yet completed its
// first successful poll) cannot report ready forever. Likewise, an auth-invalid signal
// (SetAuthValid(false)) layers a reason on top rather than replacing whatever SetReady recorded,
// so operators see every contributing cause at once.
func (s *Status) Ready() (bool, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	reasons := append([]string(nil), s.reasons...)
	ready := s.ready
	if ready && s.cursorFreshness > 0 {
		if s.lastCursorSave.IsZero() || time.Since(s.lastCursorSave) > s.cursorFreshness {
			ready = false
			reasons = append(reasons, "cursor not persisted recently (poll loop may be wedged)")
		}
	}
	if ready && !s.authValid {
		ready = false
		reasons = append(reasons, "auth invalid (401 from sentinel API, awaiting key rotation or recovery)")
	}
	return ready, reasons
}

// SetReadyDetail installs (or replaces) the hook /readyz calls to populate its "detail" key.
// Safe to call concurrently with /readyz requests and with itself -- see the readyDetail field
// doc comment for why this is atomic.Pointer-backed rather than a bare field.
func (s *Status) SetReadyDetail(fn func() any) {
	s.readyDetail.Store(&fn)
}

// Inc increments a named monotonic counter for /metrics (plan §7's "jobs by kind×outcome",
// "events consumed", "gate rejections", "bootstrap-skipped", "heartbeats posted", "fix attempts/PRs
// opened", "LLM tokens by provider" -- anything that only goes up over the process lifetime).
// Safe for concurrent use from many goroutines (the poll loop, runner, and Advisor/FIX paths of
// later phases all increment independently).
func (s *Status) Inc(name string, delta int64) {
	v, _ := s.counters.LoadOrStore(name, new(int64))
	atomic.AddInt64(v.(*int64), delta)
}

// SetGauge records the current value of a named point-in-time metric for /metrics (plan §7's
// "cursor lag", "budget/volume-cap remaining", "circuit states by scope" -- anything that can go
// up or down and where only the latest value matters, unlike Inc's running total). Safe for
// concurrent use.
func (s *Status) SetGauge(name string, value int64) {
	v, ok := s.gauges.Load(name)
	if !ok {
		v, _ = s.gauges.LoadOrStore(name, new(int64))
	}
	atomic.StoreInt64(v.(*int64), value)
}

// Snapshot returns the current counters (monotonic totals). Sorting by name is left to the caller.
func (s *Status) Snapshot() map[string]int64 {
	out := map[string]int64{}
	s.counters.Range(func(k, v any) bool {
		out[k.(string)] = atomic.LoadInt64(v.(*int64))
		return true
	})
	return out
}

// GaugeSnapshot returns the current gauges (point-in-time values). Sorting by name is left to the
// caller.
func (s *Status) GaugeSnapshot() map[string]int64 {
	out := map[string]int64{}
	s.gauges.Range(func(k, v any) bool {
		out[k.(string)] = atomic.LoadInt64(v.(*int64))
		return true
	})
	return out
}

// Handler builds the health/readiness/metrics mux described in plan §6/§7. Bound to
// WORKER_HEALTH_ADDR by main.go.
func Handler(st *Status) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// /healthz = process up (plan §7) — always 200 once the server itself is serving.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		ready, reasons := st.Ready()
		w.Header().Set("Content-Type", "application/json")
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		var detail any
		if fn := st.readyDetail.Load(); fn != nil {
			detail = (*fn)()
		}
		_ = json.NewEncoder(w).Encode(struct {
			Ready   bool     `json:"ready"`
			Reasons []string `json:"reasons,omitempty"`
			Detail  any      `json:"detail,omitempty"`
		}{Ready: ready, Reasons: reasons, Detail: detail})
	})

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(renderMetrics(st)))
	})

	return mux
}

// renderMetrics hand-rolls Prometheus text exposition format (plan §7: "stdlib only, no client
// library"), one "# TYPE <name> <counter|gauge>" line per metric followed by its sample line, all
// prefixed "sentinel_worker_" and sorted by name for deterministic (golden-testable) output.
func renderMetrics(st *Status) string {
	var b strings.Builder
	counters := st.Snapshot()
	names := make([]string, 0, len(counters))
	for name := range counters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		full := sanitizeMetricName("sentinel_worker_" + name)
		b.WriteString("# TYPE " + full + " counter\n")
		b.WriteString(full + " " + itoa(counters[name]) + "\n")
	}

	gauges := st.GaugeSnapshot()
	gnames := make([]string, 0, len(gauges))
	for name := range gauges {
		gnames = append(gnames, name)
	}
	sort.Strings(gnames)
	for _, name := range gnames {
		full := sanitizeMetricName("sentinel_worker_" + name)
		b.WriteString("# TYPE " + full + " gauge\n")
		b.WriteString(full + " " + itoa(gauges[name]) + "\n")
	}
	return b.String()
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
