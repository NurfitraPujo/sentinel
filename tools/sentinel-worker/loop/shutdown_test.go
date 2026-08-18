package loop

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// ctxAwareAdvisor blocks until released OR until the ctx it was called with is cancelled --
// whichever comes first -- and records whether cancellation is what unblocked it, so tests can
// prove a running job's context is actually derived from the dispatcher's shutdown ctx (finding
// 1: "Dispatcher severs the shutdown context").
type ctxAwareAdvisor struct {
	release      chan struct{}
	started      chan struct{}
	sawCancelled int32
}

func (a *ctxAwareAdvisor) Decide(ctx context.Context, in jobs.Input) (jobs.Decision, error) {
	if a.started != nil {
		close(a.started)
	}
	select {
	case <-a.release:
	case <-ctx.Done():
		atomic.StoreInt32(&a.sawCancelled, 1)
	}
	return jobs.Decision{Kind: in.Kind, Raw: []byte(`{"stub":true}`)}, ctx.Err()
}

func newShutdownTestDispatcher(t *testing.T, advisor jobs.Advisor) (*Dispatcher, *state.Journal) {
	t.Helper()
	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	r := &Runner{
		Journal:   j,
		Issues:    fakeIssues{snap: IssueSnapshot{Status: "unresolved", AssigneeType: "agent", AssignedTo: "me"}},
		Claims:    &fakeClaimer{held: true},
		Advisor:   advisor,
		Act:       &countingActor{},
		MyAgentID: "me",
	}
	return &Dispatcher{Runner: r, Journal: j}, j
}

// TestDispatcher_ShutdownCancelsInFlightJobContext proves the dispatcher no longer severs the
// shutdown context (finding 1): a running job's ctx must actually be cancelled shortly after the
// dispatcher's own Ctx is cancelled (SIGTERM), not the disconnected context.Background() runOne
// used to hand Runner.Run.
func TestDispatcher_ShutdownCancelsInFlightJobContext(t *testing.T) {
	adv := &ctxAwareAdvisor{release: make(chan struct{}), started: make(chan struct{})}
	d, _ := newShutdownTestDispatcher(t, adv)
	ctx, cancel := context.WithCancel(context.Background())
	d.Ctx = ctx

	d.Dispatch(ctx, ev(1, "issue-1"), KindTriage)

	select {
	case <-adv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for job to start")
	}

	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv.sawCancelled) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("in-flight job's context was never cancelled after the dispatcher's shutdown ctx was cancelled")
		}
		time.Sleep(time.Millisecond)
	}

	// Wait for the worker goroutine to fully exit before the test returns and t.TempDir() cleanup
	// removes the journal's directory out from under a still-running goroutine's Append.
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cleanupCancel()
	d.Drain(cleanupCtx)
}

// TestDispatcher_ShutdownCancelsDebounceWaitPromptly proves a FOLLOW-UP job parked in its
// one-poll-interval debounce wait returns promptly on shutdown instead of blocking for the full
// debounce window (finding 1: the debounce sleep used context.Background(), immune to SIGTERM).
// It uses a ctx-aware advisor and only asserts the wait itself unblocks and the job starts
// running promptly -- whether the job THEN respects cancellation once inside Runner.Run is a
// separate concern (covered by TestDispatcher_ShutdownCancelsInFlightJobContext).
func TestDispatcher_ShutdownCancelsDebounceWaitPromptly(t *testing.T) {
	adv := &ctxAwareAdvisor{release: make(chan struct{}), started: make(chan struct{})}
	d, _ := newShutdownTestDispatcher(t, adv)
	d.Debounce = time.Hour // would hang until the test times out if shutdown didn't cut it short
	ctx, cancel := context.WithCancel(context.Background())
	d.Ctx = ctx

	d.Dispatch(ctx, ev(1, "issue-1"), KindFollowUp)

	// Give the worker goroutine a moment to actually enter the debounce sleep.
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	cancel()

	select {
	case <-adv.started:
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("debounce wait took %v to return after ctx cancellation, expected it to return promptly", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the debounced job to start running after ctx cancellation -- the debounce wait did not observe shutdown")
	}
	close(adv.release)
}

// TestDispatcher_DrainWaitsForRunningJobUpToTimeout proves Drain actually waits for in-flight
// per-issue queue goroutines to finish rather than returning immediately (finding 1: "no
// WaitGroup/drain exists").
func TestDispatcher_DrainWaitsForRunningJobUpToTimeout(t *testing.T) {
	adv := &ctxAwareAdvisor{release: make(chan struct{}), started: make(chan struct{})}
	d, j := newShutdownTestDispatcher(t, adv)
	ctx := context.Background()
	d.Ctx = ctx

	d.Dispatch(ctx, ev(1, "issue-1"), KindTriage)
	select {
	case <-adv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for job to start")
	}

	// Release the job shortly after Drain starts waiting, proving Drain actually waits for
	// in-flight work rather than returning immediately.
	go func() {
		time.Sleep(50 * time.Millisecond)
		close(adv.release)
	}()

	drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	d.Drain(drainCtx)
	elapsed := time.Since(start)
	if elapsed < 40*time.Millisecond {
		t.Fatalf("Drain returned after %v, before the running job released -- it did not wait for in-flight work", elapsed)
	}

	jobID := state.JobID(string(KindTriage), "issue-1", 1)
	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatal(err)
	}
	if rec, ok := latest[jobID]; !ok || !rec.State.IsTerminal() {
		t.Fatalf("expected the job Drain waited for to have reached a terminal state, got %+v", latest[jobID])
	}
}

// TestDispatcher_DrainReturnsAtTimeoutWhenJobNeverFinishes proves Drain's own wait is bounded by
// the ctx it is given (WORKER_SHUTDOWN_TIMEOUT) rather than blocking forever on a job that never
// completes.
func TestDispatcher_DrainReturnsAtTimeoutWhenJobNeverFinishes(t *testing.T) {
	adv := &ctxAwareAdvisor{release: make(chan struct{}), started: make(chan struct{})}
	d, _ := newShutdownTestDispatcher(t, adv)
	ctx := context.Background()
	d.Ctx = ctx

	d.Dispatch(ctx, ev(1, "issue-1"), KindTriage)
	select {
	case <-adv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for job to start")
	}
	// Never release adv.release, and this dispatcher's Ctx (context.Background()) is never
	// cancelled either -- isolates Drain's OWN bound from the ctx-cancellation interaction the
	// other tests cover.

	drainCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	d.Drain(drainCtx)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Drain took %v to return after its ctx's timeout elapsed, expected a prompt bounded return", elapsed)
	}
	close(adv.release) // let the goroutine finish...
	// ...and wait for it to actually exit before the test returns and t.TempDir() cleanup removes
	// the journal's directory out from under a still-running goroutine's Append (otherwise this
	// flakes with "TempDir RemoveAll cleanup: directory not empty" under -race).
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cleanupCancel()
	d.Drain(cleanupCtx)
}
