// inlane_retry_test.go covers plan §2.4/§9 N8e's runner-level hardening: in-lane retry (a
// Transient/RateLimited GetIssue/EnsureClaimed failure re-driven through
// sentinel.BackoffForAttempt without leaving the per-issue queue), the shared sentinel-api
// CircuitBreaker gating those retries, and ctx-cancel-during-backoff promptness. All tests here
// inject a fake SleepCtx that never actually blocks, so this file stays fast regardless of how
// many attempts/backoff steps a case drives through.
package loop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// seqIssues returns errs[i] on the i-th GetIssue call (0-indexed); once errs is exhausted (or the
// entry is nil), it returns snap successfully. calls is safe for concurrent use.
type seqIssues struct {
	mu    sync.Mutex
	calls int
	errs  []error
	snap  IssueSnapshot
}

func (s *seqIssues) GetIssue(_ context.Context, _ string) (IssueSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return IssueSnapshot{}, s.errs[i]
	}
	return s.snap, nil
}

func (s *seqIssues) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// fakeInlaneClock is a SleepCtx that never actually blocks: it records every requested duration
// and (optionally) advances a shared "now" pointer, so a test can drive a CircuitBreaker's
// NowFunc-based half-open transition without a real 2-minute wait.
type fakeInlaneClock struct {
	mu      sync.Mutex
	sleeps  []time.Duration
	now     *time.Time          // advanced by each sleep call when non-nil (single-goroutine tests)
	advance func(time.Duration) // advances a concurrency-safe clock when non-nil (multi-goroutine tests)
}

func (c *fakeInlaneClock) sleep(ctx context.Context, d time.Duration) {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	if c.now != nil {
		*c.now = c.now.Add(d)
	}
	if c.advance != nil {
		c.advance(d)
	}
	c.mu.Unlock()
	// Still honor cancellation like the real sentinel.SleepCtx would, without actually blocking for
	// d -- a cancelled ctx returns instantly rather than after any recorded duration.
	select {
	case <-ctx.Done():
	default:
	}
}

func (c *fakeInlaneClock) sleepCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sleeps)
}

func newInlaneTestRunner(t *testing.T, issues IssueReader, claims Claimer, breaker *sentinel.CircuitBreaker, maxRetries int, sleep sentinel.CtxSleepFunc) (*Runner, *state.Journal, *[]string) {
	t.Helper()
	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	var retryClasses []string
	r := &Runner{
		Journal:          j,
		Issues:           issues,
		Claims:           claims,
		Advisor:          &countingAdvisor{},
		Act:              &countingActor{},
		DryRun:           true,
		MyAgentID:        me,
		Breaker:          breaker,
		MaxInlaneRetries: maxRetries,
		SleepCtx:         sleep,
		OnInlaneRetry: func(kind string, class sentinel.FailureClass) {
			retryClasses = append(retryClasses, kind)
		},
	}
	return r, j, &retryClasses
}

// TestRunner_InlaneRetry_TransientThenSucceeds proves a Transient GetIssue failure is re-driven
// in-lane (never leaving the job's own Run call) through the backoff ladder and eventually
// succeeds, without the job's own journal record ever landing on anything but the normal
// queued->claimed->advised->done trail (no stray failed record from the two failed attempts).
func TestRunner_InlaneRetry_TransientThenSucceeds(t *testing.T) {
	issues := &seqIssues{
		errs: []error{
			&sentinel.StatusError{Status: 503},
			&sentinel.StatusError{Status: 503},
		},
		snap: IssueSnapshot{ID: "i1", Status: "unresolved"},
	}
	clock := &fakeInlaneClock{}
	r, j, retries := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, nil, 5, clock.sleep)

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: expected eventual success after 2 transient retries, got: %v", err)
	}
	if got := issues.callCount(); got != 3 {
		t.Fatalf("expected GetIssue to be called 3 times (2 failures + 1 success), got %d", got)
	}
	if len(*retries) != 2 {
		t.Fatalf("expected exactly 2 in-lane retries, got %d (%v)", len(*retries), *retries)
	}
	if clock.sleepCount() != 2 {
		t.Fatalf("expected 2 backoff sleeps, got %d", clock.sleepCount())
	}
	if got, want := clock.sleeps[0], sentinel.BackoffForAttempt(1); got != want {
		t.Fatalf("first backoff = %v, want %v (BackoffForAttempt(1))", got, want)
	}
	if got, want := clock.sleeps[1], sentinel.BackoffForAttempt(2); got != want {
		t.Fatalf("second backoff = %v, want %v (BackoffForAttempt(2))", got, want)
	}
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last := records[len(records)-1]
	if last.State != state.StateDone {
		t.Fatalf("expected final state done, got %s", last.State)
	}
}

// seqActor returns errs[i] on the i-th Act call (0-indexed); once errs is exhausted (or the entry
// is nil), it returns nil (success). Safe for concurrent use.
type seqActor struct {
	mu    sync.Mutex
	calls int
	errs  []error
}

func (s *seqActor) Act(_ context.Context, _ string, _ jobs.Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.errs) && s.errs[i] != nil {
		return s.errs[i]
	}
	return nil
}

func (s *seqActor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestRunner_InlaneRetry_ActTransientThenSucceeds is finding 2's (MAJOR) red-first proof: an
// Act-phase transient failure (a 503, exactly what checkBatchResults now surfaces per finding 1)
// must be retried in-lane -- not journaled failed on its first hiccup -- and the job must still
// reach StateDone once a later attempt succeeds. Before the fix, runner.go called r.Act.Act
// directly (not through runWithInlaneRetry), so a single transient batch failure was terminal.
func TestRunner_InlaneRetry_ActTransientThenSucceeds(t *testing.T) {
	act := &seqActor{errs: []error{
		fmt.Errorf("batch envelope failed: %w", &sentinel.StatusError{Status: 503}),
		fmt.Errorf("batch envelope failed: %w", &sentinel.StatusError{Status: 503}),
	}}
	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	clock := &fakeInlaneClock{}
	r := &Runner{
		Journal:          j,
		Issues:           fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		Claims:           &fakeClaimer{held: true},
		Advisor:          &countingAdvisor{},
		Act:              act,
		DryRun:           false,
		MyAgentID:        me,
		MaxInlaneRetries: 5,
		SleepCtx:         clock.sleep,
	}

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: expected eventual success after 2 transient Act retries, got: %v", err)
	}
	if got := act.callCount(); got != 3 {
		t.Fatalf("FINDING 2: expected Act to be called 3 times (2 transient failures + 1 success), got %d -- Act must be retried in-lane, not terminal on the first failure", got)
	}
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last := records[len(records)-1]
	if last.State != state.StateDone {
		t.Fatalf("expected final state done, got %s", last.State)
	}
	for _, rec := range records {
		if rec.State == state.StateFailed {
			t.Fatalf("FINDING 2: job was journaled failed on a transient Act error that should have been retried in-lane: %+v", rec)
		}
	}
}

// TestRunner_InlaneRetry_ActPermanentJournalsFailed proves the OTHER half of finding 2's fix
// doesn't over-retry: a permanent (401/400-class) Act failure must still journal failed on the
// FIRST attempt, exactly as before -- only Transient/RateLimited get the retry ladder.
func TestRunner_InlaneRetry_ActPermanentJournalsFailed(t *testing.T) {
	act := &seqActor{errs: []error{
		fmt.Errorf("posting question for issue i1: %w", &sentinel.StatusError{Status: 401}),
	}}
	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	clock := &fakeInlaneClock{}
	r := &Runner{
		Journal:          j,
		Issues:           fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		Claims:           &fakeClaimer{held: true},
		Advisor:          &countingAdvisor{},
		Act:              act,
		DryRun:           false,
		MyAgentID:        me,
		MaxInlaneRetries: 5,
		SleepCtx:         clock.sleep,
	}

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err == nil {
		t.Fatal("Run: expected an error for a permanent (401) Act failure")
	}
	if got := act.callCount(); got != 1 {
		t.Fatalf("expected Act to be called exactly once (permanent failures are not retried), got %d", got)
	}
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last := records[len(records)-1]
	if last.State != state.StateFailed {
		t.Fatalf("expected final state failed, got %s", last.State)
	}
}

// TestRunner_InlaneRetry_RateLimitedHonoursRetryAfter proves a 429 in-lane retry sleeps exactly
// the Retry-After duration (plan §2.4's "Rate limited" row), not the generic backoff ladder.
func TestRunner_InlaneRetry_RateLimitedHonoursRetryAfter(t *testing.T) {
	header := map[string][]string{"Retry-After": {"7"}}
	issues := &seqIssues{
		errs: []error{&sentinel.StatusError{Status: 429, Header: header}},
		snap: IssueSnapshot{ID: "i1", Status: "unresolved"},
	}
	clock := &fakeInlaneClock{}
	r, _, _ := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, nil, 5, clock.sleep)

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if clock.sleepCount() != 1 {
		t.Fatalf("expected exactly 1 sleep, got %d", clock.sleepCount())
	}
	if clock.sleeps[0] != 7*time.Second {
		t.Fatalf("expected the 429's Retry-After (7s) to be honored, got %v", clock.sleeps[0])
	}
}

// TestRunner_InlaneRetry_PermanentNeverRetries proves a Permanent (400/422) classification is
// never retried in-lane, unlike Transient/RateLimited -- one call, one terminal failed(permanent).
func TestRunner_InlaneRetry_PermanentNeverRetries(t *testing.T) {
	issues := &seqIssues{
		errs: []error{&sentinel.StatusError{Status: 400}},
		snap: IssueSnapshot{ID: "i1", Status: "unresolved"},
	}
	clock := &fakeInlaneClock{}
	r, j, retries := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, nil, 5, clock.sleep)

	var outcomes []string
	r.OnOutcome = func(kind, outcome string) { outcomes = append(outcomes, outcome) }

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err == nil {
		t.Fatalf("Run: expected the permanent error to propagate")
	}
	if got := issues.callCount(); got != 1 {
		t.Fatalf("expected exactly 1 GetIssue call (no retry for a permanent class), got %d", got)
	}
	if len(*retries) != 0 {
		t.Fatalf("expected no in-lane retries for a permanent classification, got %d", len(*retries))
	}
	if clock.sleepCount() != 0 {
		t.Fatalf("expected no backoff sleep for a permanent classification, got %d", clock.sleepCount())
	}
	found := false
	for _, o := range outcomes {
		if o == "failed_permanent" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected outcome failed_permanent, got %v", outcomes)
	}
	records, _, _ := j.Load()
	if last := records[len(records)-1]; last.State != state.StateFailed {
		t.Fatalf("expected terminal failed, got %s", last.State)
	}
}

// TestRunner_InlaneRetry_ExhaustsMaxAttemptsThenFails proves the retry ladder is bounded: after
// MaxInlaneRetries attempts, a still-transient failure gives up and journals the NON-terminal
// state.StateRetryableFailed (finding 4, core-robustness round 3) -- recoverable once the outage
// clears, rather than retrying forever OR being lost as a terminal `failed`.
func TestRunner_InlaneRetry_ExhaustsMaxAttemptsThenFails(t *testing.T) {
	issues := &seqIssues{
		errs: []error{
			&sentinel.StatusError{Status: 503},
			&sentinel.StatusError{Status: 503},
			&sentinel.StatusError{Status: 503},
		},
		snap: IssueSnapshot{ID: "i1", Status: "unresolved"},
	}
	clock := &fakeInlaneClock{}
	r, j, retries := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, nil, 3, clock.sleep)

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err == nil {
		t.Fatalf("Run: expected the exhausted-retries error to propagate")
	}
	if got := issues.callCount(); got != 3 {
		t.Fatalf("expected exactly 3 attempts (MaxInlaneRetries=3), got %d", got)
	}
	if len(*retries) != 2 {
		t.Fatalf("expected 2 retries (3 attempts - 1), got %d", len(*retries))
	}
	records, _, _ := j.Load()
	if last := records[len(records)-1]; last.State != state.StateRetryableFailed {
		t.Fatalf("expected non-terminal retryable_failed after exhausting retries, got %s", last.State)
	}
	if last := records[len(records)-1]; last.State.IsTerminal() {
		t.Fatalf("state.StateRetryableFailed must not be terminal (RecoveryScan must still find it)")
	}
}

// TestRunner_InlaneRetry_CtxCancelDuringBackoffReturnsPromptly proves shutdown (ctx cancellation)
// during a pending in-lane backoff wait returns promptly instead of blocking for the wait's full
// duration -- this test uses the REAL sentinel.SleepCtx (not the fake clock) specifically to prove
// the production sleep implementation itself is interruptible, cancelling well before the first
// backoff step (1s) would elapse.
func TestRunner_InlaneRetry_CtxCancelDuringBackoffReturnsPromptly(t *testing.T) {
	issues := &seqIssues{
		errs: []error{
			&sentinel.StatusError{Status: 503},
			&sentinel.StatusError{Status: 503},
		},
		snap: IssueSnapshot{ID: "i1", Status: "unresolved"},
	}
	r, _, _ := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, nil, 5, nil) // nil -> real sentinel.SleepCtx

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	start := time.Now()
	err := r.Run(ctx, e, KindTriage)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Run: expected an error (ctx cancellation), got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: expected context.Canceled, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Run took %v to return after ctx cancellation mid-backoff, expected well under the 1s backoff step", elapsed)
	}
}

// TestRunner_Breaker_OpensPausesThenHalfOpenCloses drives the shared sentinel-api CircuitBreaker
// through its full lifecycle via the runner: 5 consecutive Transient GetIssue failures (across 5
// separate single-attempt jobs, MaxInlaneRetries=1 so each job's one attempt is exactly one
// circuit-failure record) open the circuit; a 6th job's attempt then finds the breaker refusing
// calls and pauses (OnCircuitOpen fires) until the fake clock's advancing sleeps cross the 2m
// half-open probe interval, at which point the probe call is let through, succeeds, and closes
// the circuit.
func TestRunner_Breaker_OpensPausesThenHalfOpenCloses(t *testing.T) {
	now := time.Now()
	breaker := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
	breaker.NowFunc = func() time.Time { return now }

	// Open the circuit directly (5 consecutive failures, plan §2.4) rather than driving 5 full
	// jobs through it -- equivalent end state, far less test machinery.
	for i := 0; i < 5; i++ {
		breaker.RecordFailure()
	}
	if breaker.State() != sentinel.CircuitOpen {
		t.Fatalf("setup: expected breaker Open after 5 consecutive failures, got %v", breaker.State())
	}

	clock := &fakeInlaneClock{now: &now}
	issues := &seqIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}} // succeeds once the probe is let through
	r, j, _ := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, breaker, 3, clock.sleep)

	var circuitOpenEvents int
	r.OnCircuitOpen = func(scope string) {
		circuitOpenEvents++
		if scope != sentinel.ScopeSentinelAPI {
			t.Fatalf("OnCircuitOpen scope = %q, want %q", scope, sentinel.ScopeSentinelAPI)
		}
	}

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: expected the job to eventually succeed once the half-open probe lands, got: %v", err)
	}
	if circuitOpenEvents == 0 {
		t.Fatalf("expected OnCircuitOpen to fire at least once while the breaker was open")
	}
	if breaker.State() != sentinel.CircuitClosed {
		t.Fatalf("expected the breaker to close after the successful half-open probe, got %v", breaker.State())
	}
	if issues.callCount() != 1 {
		t.Fatalf("expected exactly 1 GetIssue call (the successful probe), got %d", issues.callCount())
	}
	records, _, _ := j.Load()
	if last := records[len(records)-1]; last.State != state.StateDone {
		t.Fatalf("expected the job to finish done, got %s", last.State)
	}
}

// TestRunner_Breaker_PermanentFailureDoesNotTripCircuit proves the breaker only tracks
// dependency-availability failures (Transient/RateLimited): a well-formed permanent rejection
// (400/422) must never count against it, or a real client bug would masquerade as a sentinel-api
// outage and pause every OTHER job on this dependency too.
func TestRunner_Breaker_PermanentFailureDoesNotTripCircuit(t *testing.T) {
	breaker := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
	issues := &seqIssues{errs: []error{&sentinel.StatusError{Status: 400}}, snap: IssueSnapshot{ID: "i1", Status: "unresolved"}}
	clock := &fakeInlaneClock{}
	r, _, _ := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, breaker, 5, clock.sleep)

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err == nil {
		t.Fatalf("Run: expected the permanent error to propagate")
	}
	if breaker.State() != sentinel.CircuitClosed {
		t.Fatalf("a permanent (400) failure must not trip the circuit, got state %v", breaker.State())
	}
}

// TestRunner_Breaker_RateLimitedNeverTripsCircuit proves plan §2.4's "Rate limited ... Never
// counts as a failure" and retry.go's WaitRateLimit doc ("callers must not feed this path into a
// CircuitBreaker or backoff attempt counter"): 5 consecutive 429s across 5 separate single-attempt
// jobs must NOT open the shared sentinel-api circuit, even though 5 consecutive Transient (5xx)
// failures over the exact same shape DO.
func TestRunner_Breaker_RateLimitedNeverTripsCircuit(t *testing.T) {
	breaker := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
	clock := &fakeInlaneClock{}

	for i := 0; i < 5; i++ {
		issues := &seqIssues{
			errs: []error{&sentinel.StatusError{Status: 429, Header: map[string][]string{"Retry-After": {"0"}}}},
			snap: IssueSnapshot{ID: "i1", Status: "unresolved"},
		}
		// MaxInlaneRetries=1 so each job's single attempt produces exactly one failure record,
		// mirroring TestRunner_Breaker_OpensPausesThenHalfOpenCloses's setup shape.
		r, _, _ := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, breaker, 1, clock.sleep)
		e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
		if err := r.Run(context.Background(), e, KindTriage); err == nil {
			t.Fatalf("job %d: expected the exhausted rate-limited attempt to propagate an error", i)
		}
	}

	if breaker.State() != sentinel.CircuitClosed {
		t.Fatalf("5 consecutive 429s must never open the circuit, got state %v", breaker.State())
	}

	// Control: the same shape with Transient (5xx) failures DOES open the circuit, proving the
	// test would actually catch a regression rather than passing vacuously.
	controlBreaker := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
	for i := 0; i < 5; i++ {
		issues := &seqIssues{
			errs: []error{&sentinel.StatusError{Status: 503}},
			snap: IssueSnapshot{ID: "i1", Status: "unresolved"},
		}
		r, _, _ := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, controlBreaker, 1, clock.sleep)
		e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
		_ = r.Run(context.Background(), e, KindTriage)
	}
	if controlBreaker.State() != sentinel.CircuitOpen {
		t.Fatalf("control: 5 consecutive Transient (503) failures should open the circuit, got state %v", controlBreaker.State())
	}
}

// TestCircuitBreaker_ConcurrentRunnersAndGaugeReader_NoRace proves the production wiring is safe:
// the dispatcher shares ONE sentinel.CircuitBreaker across every per-issue runner goroutine
// (loop/queue.go's runIssueWorker, one goroutine per issue, all calling the same *Runner) plus
// main.go's publishCircuitStateGauge concurrently reading State(). Before CircuitBreaker grew its
// internal mutex, `go test -race` on this exact pattern reported a DATA RACE between RecordFailure
// and State (and RecordFailure vs RecordFailure). This test drives several concurrent per-issue
// Runner.Run calls against one shared breaker, plus a concurrent State()-reading goroutine mimicking
// the gauge publisher, and must be run with -race (the mandated gate always does).
func TestCircuitBreaker_ConcurrentRunnersAndGaugeReader_NoRace(t *testing.T) {
	// A goroutine-safe fake clock: sleep() advances it atomically so the breaker's real 2-minute
	// half-open threshold elapses in test time without ever blocking, and NowFunc reads it
	// atomically too -- both are called from many goroutines here, so a plain time.Time var (as
	// the single-goroutine tests above use) would itself race.
	var nowNanos atomic.Int64
	nowNanos.Store(time.Now().UnixNano())
	breaker := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
	breaker.NowFunc = func() time.Time { return time.Unix(0, nowNanos.Load()) }
	clock := &fakeInlaneClock{}
	clock.advance = func(d time.Duration) { nowNanos.Add(int64(d)) }

	const numIssues = 8
	var wg sync.WaitGroup

	// One goroutine per issue, each with its own Runner (as the real dispatcher does one Runner
	// call per issue-worker goroutine), all sharing the single breaker.
	for i := 0; i < numIssues; i++ {
		issueID := "i" + string(rune('a'+i))
		wg.Add(1)
		go func(issueID string) {
			defer wg.Done()
			// Alternate transient failures and successes so both RecordFailure and RecordSuccess
			// fire concurrently across goroutines, and Allow() is exercised while the breaker may
			// be open/half-open from another goroutine's failures.
			for round := 0; round < 20; round++ {
				var errs []error
				if round%2 == 0 {
					errs = []error{&sentinel.StatusError{Status: 503}}
				}
				issues := &seqIssues{errs: errs, snap: IssueSnapshot{ID: issueID, Status: "unresolved"}}
				r, _, _ := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, breaker, 1, clock.sleep)
				e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: issueID}}
				_ = r.Run(context.Background(), e, KindTriage) // error expected on some rounds; only racing matters here
			}
		}(issueID)
	}

	// Mimic main.go's publishCircuitStateGauge: repeated concurrent State() reads while the
	// runners above are calling Allow/RecordSuccess/RecordFailure. This goroutine is NOT part of
	// wg (it only stops once the issue goroutines above are done), so wg.Wait() below waits solely
	// on the per-issue runner goroutines.
	stop := make(chan struct{})
	gaugeDone := make(chan struct{})
	go func() {
		defer close(gaugeDone)
		for {
			select {
			case <-stop:
				return
			default:
				_ = breaker.State()
			}
		}
	}()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(stop)
		<-gaugeDone
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent breaker test did not finish in time (possible deadlock)")
	}
}

// TestSeparateBreakers_RunnerOpensWhileConcurrentPollLoopKeepsSucceeding is circuit-config-sec
// finding 1's red-first regression proof: N8h Group D's "share the breaker" instruction had main.go
// hand ONE sentinel.CircuitBreaker to both the poll loop and the runner. The poll loop calls
// RecordSuccess on every successful ~10s GetEvents poll; if that success shares the SAME breaker
// instance as the runner's RecordFailure calls, it resets the runner's consecutive-failure streak
// to 0 before 5 straight runner failures can ever accumulate -- so the write-path circuit can
// effectively never open, exactly while the dependency it guards is actually down.
//
// This test drives that exact interleaving -- a PollLoop succeeding repeatedly, concurrently with a
// Runner failing 5 straight jobs -- twice: once with ONE shared breaker (the N8h bug, kept here as
// a control proving this test would actually catch a regression) and once with the CURRENT
// production wiring shape (two separate breakers, one per loop.Runner/loop.PollLoop, as main.go now
// constructs them). Only the separate-breakers case may open.
func TestSeparateBreakers_RunnerOpensWhileConcurrentPollLoopKeepsSucceeding(t *testing.T) {
	runFiveFailuresWhilePolling := func(t *testing.T, runnerBreaker, pollBreaker *sentinel.CircuitBreaker) {
		t.Helper()
		clock := &fakeInlaneClock{}

		poller := &PollLoop{
			Client:     &fakeEventsClient{}, // empty pages, no error -> every PollOnce succeeds
			MyAgentID:  me,
			Enqueue:    &recordingEnqueuer{},
			Breaker:    pollBreaker,
			RetrySleep: clock.sleep,
		}

		// Interleave: one successful poll cycle, then one failed runner job, repeated 5 times -- the
		// exact "poll succeeds every cycle while the runner is failing" shape the finding describes.
		// PollOnce itself never touches the breaker (only PollLoop.Run's outer loop does, on a
		// successful PollOnce, exactly as production wiring drives it) -- reproduced directly here so
		// this test isn't also exercising Run's sleep/backoff machinery, which
		// TestPollLoop_Run_SharedBreakerOpensOnRepeatedFailure already covers.
		for i := 0; i < 5; i++ {
			if _, err := poller.PollOnce(context.Background()); err != nil {
				t.Fatalf("poll %d: unexpected error: %v", i, err)
			}
			poller.breaker().RecordSuccess()

			issues := &seqIssues{
				errs: []error{&sentinel.StatusError{Status: 503}},
				snap: IssueSnapshot{ID: "i1", Status: "unresolved"},
			}
			r, _, _ := newInlaneTestRunner(t, issues, &fakeClaimer{held: true}, runnerBreaker, 1, clock.sleep)
			e := Event{Seq: int64(i + 1), Type: "created", Issue: &EventIssue{ID: "i1"}}
			if err := r.Run(context.Background(), e, KindTriage); err == nil {
				t.Fatalf("job %d: expected the transient runner failure to propagate", i)
			}
		}
	}

	t.Run("shared breaker (N8h bug) never opens", func(t *testing.T) {
		shared := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
		runFiveFailuresWhilePolling(t, shared, shared)
		if shared.State() == sentinel.CircuitOpen {
			t.Fatalf("this is the control case demonstrating the N8h bug: with ONE shared breaker, the poll loop's RecordSuccess should have reset the runner's failure streak on every cycle, so it should NOT have opened -- got %v (if this now opens, the interleaving no longer reproduces the regression this test exists to guard against)", shared.State())
		}
	})

	t.Run("separate breakers (current production wiring) — runner opens", func(t *testing.T) {
		runnerBreaker := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
		pollBreaker := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPIPoll)
		runFiveFailuresWhilePolling(t, runnerBreaker, pollBreaker)

		if runnerBreaker.State() != sentinel.CircuitOpen {
			t.Fatalf("expected the runner's OWN breaker to open after 5 consecutive runner failures despite the concurrent poll loop's successes, got %v", runnerBreaker.State())
		}
		if pollBreaker.State() != sentinel.CircuitClosed {
			t.Fatalf("expected the poll loop's OWN breaker to remain Closed (it only ever saw successes), got %v", pollBreaker.State())
		}
	})
}
