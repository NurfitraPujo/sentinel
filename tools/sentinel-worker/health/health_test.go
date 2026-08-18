package health

import (
	"io"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/loop"
)

func TestHealthz_AlwaysOK(t *testing.T) {
	st := NewStatus()
	srv := httptest.NewServer(Handler(st))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestReadyz_NotReadyByDefault(t *testing.T) {
	st := NewStatus()
	srv := httptest.NewServer(Handler(st))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if resp.StatusCode != 503 {
		t.Fatalf("expected 503 before SetReady(true), got %d", resp.StatusCode)
	}
}

func TestReadyz_ReadyAfterSetReady(t *testing.T) {
	st := NewStatus()
	st.SetReady(true)
	srv := httptest.NewServer(Handler(st))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after SetReady(true), got %d", resp.StatusCode)
	}
}

func TestMetrics_ExposesCounters(t *testing.T) {
	st := NewStatus()
	st.Inc("events_consumed", 3)
	st.Inc("events_consumed", 2)
	srv := httptest.NewServer(Handler(st))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	snap := st.Snapshot()
	if snap["events_consumed"] != 5 {
		t.Fatalf("expected counter 5, got %d", snap["events_consumed"])
	}
}

// TestReadyz_FlipsNotReadyWhenCursorGoesStale proves plan §7's "cursor persisted recently" leg of
// /readyz: once a freshness window is configured, an overdue cursor save flips ready -> not-ready,
// and a fresh save flips it back.
func TestReadyz_FlipsNotReadyWhenCursorGoesStale(t *testing.T) {
	st := NewStatus()
	st.SetReady(true)
	st.SetCursorFreshnessWindow(30 * time.Millisecond)
	st.NoteCursorSaved(time.Now())

	if ready, reasons := st.Ready(); !ready {
		t.Fatalf("expected ready immediately after a fresh cursor save, reasons=%v", reasons)
	}

	time.Sleep(60 * time.Millisecond)
	ready, reasons := st.Ready()
	if ready {
		t.Fatalf("expected not-ready once the cursor save goes stale")
	}
	found := false
	for _, r := range reasons {
		if r != "" && contains(r, "cursor") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a cursor-staleness reason, got %v", reasons)
	}

	// recovery: a fresh save flips it back.
	st.NoteCursorSaved(time.Now())
	if ready, reasons := st.Ready(); !ready {
		t.Fatalf("expected ready again after a fresh cursor save, reasons=%v", reasons)
	}
}

// TestReadyz_FlipsNotReadyOnAuthInvalid_AndRecovers proves plan §7's "auth valid" leg of /readyz:
// SetAuthValid(false) (wired from a 401 via sentinel.Client.OnAuthStatus) flips ready -> not-ready,
// and SetAuthValid(true) (a subsequent successful call, or a post-rotation recovery) flips it back.
func TestReadyz_FlipsNotReadyOnAuthInvalid_AndRecovers(t *testing.T) {
	st := NewStatus()
	st.SetReady(true)

	if ready, reasons := st.Ready(); !ready {
		t.Fatalf("expected ready by default (authValid starts true, no evidence of invalidity yet), reasons=%v", reasons)
	}

	st.SetAuthValid(false)
	ready, reasons := st.Ready()
	if ready {
		t.Fatalf("expected not-ready after SetAuthValid(false)")
	}
	found := false
	for _, r := range reasons {
		if contains(r, "auth") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an auth-invalid reason, got %v", reasons)
	}

	st.SetAuthValid(true)
	if ready, reasons := st.Ready(); !ready {
		t.Fatalf("expected ready again after SetAuthValid(true), reasons=%v", reasons)
	}
}

// TestReadyz_HTTPReflectsAuthAndCursorSignals drives the same scenarios through the real
// /readyz HTTP handler (not just Status.Ready() directly), proving the wiring end to end.
func TestReadyz_HTTPReflectsAuthAndCursorSignals(t *testing.T) {
	st := NewStatus()
	st.SetReady(true)
	st.SetCursorFreshnessWindow(time.Hour)
	st.NoteCursorSaved(time.Now())
	srv := httptest.NewServer(Handler(st))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 with fresh cursor and valid auth, got %d", resp.StatusCode)
	}

	st.SetAuthValid(false)
	resp2, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 503 {
		t.Fatalf("expected 503 once auth goes invalid, got %d", resp2.StatusCode)
	}

	st.SetAuthValid(true)
	resp3, err := srv.Client().Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("expected 200 again once auth recovers, got %d", resp3.StatusCode)
	}
}

// TestMetrics_GoldenFormat pins the hand-rolled Prometheus text exposition format (plan §7): a
// "# TYPE <name> <counter|gauge>" line followed by the sample line, per metric, all
// "sentinel_worker_"-prefixed, counters before gauges, each group sorted by name. A change to
// this output shape should be a deliberate edit to this golden, not an accidental regression.
func TestMetrics_GoldenFormat(t *testing.T) {
	st := NewStatus()
	st.Inc("events_consumed", 3)
	st.Inc("bootstrap_skipped", 1)
	st.SetGauge("cursor_lag", 42)
	st.SetGauge("budget_remaining_usd_cents", 500)

	got := renderMetrics(st)
	want := "" +
		"# TYPE sentinel_worker_bootstrap_skipped counter\n" +
		"sentinel_worker_bootstrap_skipped 1\n" +
		"# TYPE sentinel_worker_events_consumed counter\n" +
		"sentinel_worker_events_consumed 3\n" +
		"# TYPE sentinel_worker_budget_remaining_usd_cents gauge\n" +
		"sentinel_worker_budget_remaining_usd_cents 500\n" +
		"# TYPE sentinel_worker_cursor_lag gauge\n" +
		"sentinel_worker_cursor_lag 42\n"
	if got != want {
		t.Fatalf("renderMetrics golden mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestMetrics_GaugeOverwritesNotAccumulates proves SetGauge replaces the value (point-in-time),
// unlike Inc which accumulates -- the distinction plan §7 draws between "circuit states"/"budget
// remaining" (gauges) and "events consumed"/"jobs by kind×outcome" (counters).
func TestMetrics_GaugeOverwritesNotAccumulates(t *testing.T) {
	st := NewStatus()
	st.SetGauge("circuit_state_sentinel_api", 1)
	st.SetGauge("circuit_state_sentinel_api", 0)
	snap := st.GaugeSnapshot()
	if snap["circuit_state_sentinel_api"] != 0 {
		t.Fatalf("expected latest SetGauge value 0, got %d", snap["circuit_state_sentinel_api"])
	}
}

// TestMetricsEndpoint_ServesRenderedText proves the HTTP handler actually serves renderMetrics'
// output, not just that the function itself is correct in isolation.
func TestMetricsEndpoint_ServesRenderedText(t *testing.T) {
	st := NewStatus()
	st.Inc("events_consumed", 1)
	st.SetGauge("cursor_lag", 7)
	srv := httptest.NewServer(Handler(st))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(body) != renderMetrics(st) {
		t.Fatalf("HTTP /metrics body does not match renderMetrics output:\nHTTP:\n%s\ndirect:\n%s", body, renderMetrics(st))
	}
}

// TestInc_ConcurrentIncrementsAreRace_free proves the counter registry is safe under concurrent
// writers (the poll loop, runner, and later-phase Advisor/FIX paths all Inc independently) --
// run with `go test -race` to catch a data race, and assert the final total to catch a
// non-atomic lost-update bug that -race alone would not necessarily catch every run.
func TestInc_ConcurrentIncrementsAreRace_free(t *testing.T) {
	st := NewStatus()
	const goroutines = 50
	const perGoroutine = 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				st.Inc("events_consumed", 1)
			}
		}()
	}
	wg.Wait()

	snap := st.Snapshot()
	want := int64(goroutines * perGoroutine)
	if snap["events_consumed"] != want {
		t.Fatalf("expected %d after concurrent increments, got %d", want, snap["events_consumed"])
	}
}

// TestSetGauge_ConcurrentWritesAreRaceFree proves the gauge registry is likewise safe under
// concurrent writers -- run with `go test -race`.
func TestSetGauge_ConcurrentWritesAreRaceFree(t *testing.T) {
	st := NewStatus()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(v int64) {
			defer wg.Done()
			st.SetGauge("cursor_lag", v)
		}(int64(i))
	}
	wg.Wait()
	// No assertion on the final value (last-writer-wins is inherently racy in ordering) -- the
	// point is that -race finds no data race in the read-modify-write of the gauge slot itself.
	_ = st.GaugeSnapshot()["cursor_lag"]
}

// validMetricName is the Prometheus text exposition format's name grammar (plan §7 requires the
// hand-rolled output to be valid Prometheus text).
var validMetricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// TestJobsTotalMetricName_AlwaysValid proves JobsTotalMetricName produces a legal Prometheus
// metric name for every real loop.Kind × every real loop.SkipReason the wired runner path can
// actually emit, plus the non-skip outcomes (done/failed/acted_partial/superseded). Before the
// fix, loop.Kind values like "sweep-reconcile" and loop.SkipReason values like "foreign-claim"
// produced hyphenated names that are invalid Prometheus grammar and break the ENTIRE /metrics
// scrape, not just their own series -- mutation-test this by reverting JobsTotalMetricName to
// `jobsTotalPrefix + "_" + kind + "_" + outcome` (no sanitizeMetricName) and watching it go red.
func TestJobsTotalMetricName_AlwaysValid(t *testing.T) {
	kinds := []loop.Kind{
		loop.KindNone,
		loop.KindTriage,
		loop.KindFollowUp,
		loop.KindSweepReconcile,
		loop.KindCancelQueued,
		loop.KindSkippedDeleted,
	}
	skipReasons := []loop.SkipReason{
		loop.SkipForeignClaim,
		loop.SkipDeleted,
		loop.SkipResolved,
		loop.SkipIgnored,
		loop.SkipNotPreconditioned,
	}
	outcomes := []string{"done", "failed", "acted_partial", "superseded"}
	for _, sr := range skipReasons {
		outcomes = append(outcomes, "skipped_"+string(sr))
	}

	for _, kind := range kinds {
		for _, outcome := range outcomes {
			name := JobsTotalMetricName(string(kind), outcome)
			if !validMetricName.MatchString(name) {
				t.Errorf("JobsTotalMetricName(%q, %q) = %q, not a valid Prometheus metric name", kind, outcome, name)
			}
		}
	}
}

// TestMetrics_GoldenFormat_WithJobsFamily widens the golden to cover the real "jobs by
// kind×outcome" family (built via JobsTotalMetricName, not hand-typed literals -- the validator's
// blocker shipped precisely because the golden only ever exercised names JobsTotalMetricName does
// not produce) with >=4 counters and >=4 gauges inserted in deliberately non-sorted order, so a
// lost sort.Strings fails deterministically rather than ~40% of the time (measured: 3/5 runs with
// sort.Strings removed against the smaller original golden).
func TestMetrics_GoldenFormat_WithJobsFamily(t *testing.T) {
	st := NewStatus()
	// Insertion order deliberately not alphabetical.
	st.Inc(JobsTotalMetricName(string(loop.KindSweepReconcile), "done"), 1)
	st.Inc(JobsTotalMetricName(string(loop.KindFollowUp), "skipped_"+string(loop.SkipForeignClaim)), 1)
	st.Inc("events_consumed", 3)
	st.Inc("bootstrap_skipped", 1)
	st.SetGauge("circuit_state_sentinel_api", 1)
	st.SetGauge("budget_remaining_usd_cents", 500)
	st.SetGauge("cursor_lag", 42)
	st.SetGauge("gate_rejections_active", 0)

	got := renderMetrics(st)
	want := "" +
		"# TYPE sentinel_worker_bootstrap_skipped counter\n" +
		"sentinel_worker_bootstrap_skipped 1\n" +
		"# TYPE sentinel_worker_events_consumed counter\n" +
		"sentinel_worker_events_consumed 3\n" +
		"# TYPE sentinel_worker_jobs_total_followup_skipped_foreign_claim counter\n" +
		"sentinel_worker_jobs_total_followup_skipped_foreign_claim 1\n" +
		"# TYPE sentinel_worker_jobs_total_sweep_reconcile_done counter\n" +
		"sentinel_worker_jobs_total_sweep_reconcile_done 1\n" +
		"# TYPE sentinel_worker_budget_remaining_usd_cents gauge\n" +
		"sentinel_worker_budget_remaining_usd_cents 500\n" +
		"# TYPE sentinel_worker_circuit_state_sentinel_api gauge\n" +
		"sentinel_worker_circuit_state_sentinel_api 1\n" +
		"# TYPE sentinel_worker_cursor_lag gauge\n" +
		"sentinel_worker_cursor_lag 42\n" +
		"# TYPE sentinel_worker_gate_rejections_active gauge\n" +
		"sentinel_worker_gate_rejections_active 0\n"
	if got != want {
		t.Fatalf("renderMetrics golden mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
