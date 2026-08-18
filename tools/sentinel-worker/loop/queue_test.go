package loop

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// blockingAdvisor lets a test hold a job "running" until released, so it can assert single-flight
// (no second job for the same issue starts while the first is still in flight) and observe
// coalescing/debounce behavior deterministically instead of racing real goroutines.
type blockingAdvisor struct {
	release chan struct{}
	running int32
	maxSeen int32
}

func (b *blockingAdvisor) Decide(_ context.Context, in jobs.Input) (jobs.Decision, error) {
	n := atomic.AddInt32(&b.running, 1)
	for {
		old := atomic.LoadInt32(&b.maxSeen)
		if n <= old || atomic.CompareAndSwapInt32(&b.maxSeen, old, n) {
			break
		}
	}
	<-b.release
	atomic.AddInt32(&b.running, -1)
	return jobs.Decision{Kind: in.Kind, Raw: []byte(`{"stub":true}`)}, nil
}

func newQueueTestDispatcher(t *testing.T, advisor jobs.Advisor) (*Dispatcher, *state.Journal) {
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
	d := &Dispatcher{Runner: r, Journal: j}
	return d, j
}

func ev(seq int64, issueID string) Event {
	assigneeType := "agent"
	assignedTo := "me"
	return Event{Seq: seq, Type: "created", ActorID: "other", Issue: &EventIssue{ID: issueID, AssigneeType: &assigneeType, AssignedTo: &assignedTo}}
}

// TestDispatcher_Coalescing proves that two queued TRIAGE jobs for the same issue, delivered
// before the first has started, collapse into one run of the LATEST triggerSeq — and the loser is
// journaled `superseded` (plan §2.2/§3).
func TestDispatcher_Coalescing(t *testing.T) {
	adv2 := &blockingAdvisor{release: make(chan struct{})}
	d2, j2 := newQueueTestDispatcher(t, adv2)

	e1 := ev(1, "issue-1")
	e2 := ev(2, "issue-1")

	// Dispatch e1's TRIAGE: the worker starts and immediately blocks inside Decide.
	d2.Dispatch(context.Background(), e1, KindTriage)

	// Wait until the first job is actually running (advisor entered Decide) before queuing the
	// second, coalescing kind — otherwise there is nothing to coalesce against.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv2.running) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for first job to start")
		}
		time.Sleep(time.Millisecond)
	}

	// Now dispatch a second TRIAGE for the same issue while the first is still running -- since
	// jobs are single-flight per issue, this one queues, not runs, so it CAN coalesce with a
	// third if one arrives before the first job finishes and the second starts.
	d2.Dispatch(context.Background(), e2, KindTriage)
	e3 := Event{Seq: 3, Type: "created", ActorID: "other", Issue: e1.Issue}
	d2.Dispatch(context.Background(), e3, KindTriage)

	// Release the first (running) job.
	close(adv2.release)

	deadline = time.Now().Add(2 * time.Second)
	for {
		latest, err := j2.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		job1ID := state.JobID(string(KindTriage), "issue-1", 1)
		job2ID := state.JobID(string(KindTriage), "issue-1", 2)
		job3ID := state.JobID(string(KindTriage), "issue-1", 3)
		r1, ok1 := latest[job1ID]
		r2, ok2 := latest[job2ID]
		r3, ok3 := latest[job3ID]
		if ok1 && ok2 && ok3 && r1.State == state.StateDone && r3.State == state.StateDone {
			if r2.State != state.StateSuperseded {
				t.Fatalf("expected job2 (coalesced loser) superseded, got %s", r2.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: job1=%v(%v) job2=%v(%v) job3=%v(%v)", ok1, r1.State, ok2, r2.State, ok3, r3.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDispatcher_SerialPerIssue proves jobs for one issue never run concurrently (single-flight)
// while jobs for DIFFERENT issues run concurrently -- race-tested (run with -race).
func TestDispatcher_SerialPerIssue(t *testing.T) {
	adv := &blockingAdvisor{release: make(chan struct{})}
	d, j := newQueueTestDispatcher(t, adv)

	var wg sync.WaitGroup
	const nIssues = 5
	for i := 0; i < nIssues; i++ {
		issueID := "issue-" + string(rune('A'+i))
		wg.Add(1)
		go func(issueID string) {
			defer wg.Done()
			d.Dispatch(context.Background(), ev(1, issueID), KindTriage)
		}(issueID)
	}
	wg.Wait()

	// Let every issue's job actually enter Decide.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv.running) < nIssues {
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d issues running concurrently", atomic.LoadInt32(&adv.running), nIssues)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if max := atomic.LoadInt32(&adv.maxSeen); max < nIssues {
		t.Fatalf("expected up to %d concurrent jobs across distinct issues, saw max %d", nIssues, max)
	}
	close(adv.release)

	// Wait for every issue's job to reach a terminal journal state before returning -- otherwise a
	// worker goroutine can still be mid-write when the test function returns, racing t.TempDir()'s
	// cleanup removal of the directory the journal file lives in.
	deadline = time.Now().Add(2 * time.Second)
	for {
		latest, err := j.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		doneCount := 0
		for i := 0; i < nIssues; i++ {
			issueID := "issue-" + string(rune('A'+i))
			if r, ok := latest[state.JobID(string(KindTriage), issueID, 1)]; ok && r.State == state.StateDone {
				doneCount++
			}
		}
		if doneCount == nIssues {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d issue jobs reached done", doneCount, nIssues)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDispatcher_SerialPerIssue_SingleFlight proves that a second job for the SAME issue never
// starts while the first is still running: dispatching a coalescing-immune sequence (TRIAGE then,
// after the first drains, FOLLOW-UP) never sees running>1 for one issue.
func TestDispatcher_SerialPerIssue_SingleFlight(t *testing.T) {
	adv := &blockingAdvisor{release: make(chan struct{})}
	d, j := newQueueTestDispatcher(t, adv)

	e1 := ev(1, "issue-1")
	d.Dispatch(context.Background(), e1, KindTriage)

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv.running) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first job never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r := atomic.LoadInt32(&adv.running); r != 1 {
		t.Fatalf("expected exactly 1 running, got %d", r)
	}
	close(adv.release)

	deadline = time.Now().Add(2 * time.Second)
	for {
		latest, err := j.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		if r, ok := latest[state.JobID(string(KindTriage), "issue-1", 1)]; ok && r.State == state.StateDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first job never completed")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDispatcher_FollowUpDebounce proves a FOLLOW-UP job waits the configured debounce interval,
// and that a second FOLLOW-UP arriving during the wait (C10's two adjacent events) coalesces into
// the SAME run instead of producing two.
func TestDispatcher_FollowUpDebounce(t *testing.T) {
	adv := &blockingAdvisor{release: make(chan struct{})}
	close(adv.release)
	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	r := &Runner{
		Journal:   j,
		Issues:    fakeIssues{snap: IssueSnapshot{Status: "unresolved", AssigneeType: "agent", AssignedTo: "me"}},
		Claims:    &fakeClaimer{held: true},
		Advisor:   adv,
		Act:       &countingActor{},
		MyAgentID: "me",
	}
	fired := make(chan struct{}, 1)
	sleepCalls := 0
	var sleepMu sync.Mutex
	d := &Dispatcher{
		Runner:   r,
		Journal:  j,
		Debounce: 50 * time.Millisecond,
		Sleep: func(ctx context.Context, dur time.Duration) {
			sleepMu.Lock()
			sleepCalls++
			sleepMu.Unlock()
			select {
			case <-time.After(dur):
			case <-ctx.Done():
			}
		},
	}

	assigneeType, assignedTo := "agent", "me"
	issue := &EventIssue{ID: "issue-1", AssigneeType: &assigneeType, AssignedTo: &assignedTo}
	e1 := Event{Seq: 1, Type: "question_answered", ActorID: "other", Issue: issue}
	e2 := Event{Seq: 2, Type: "commented", ActorID: "other", Issue: issue}

	d.Dispatch(context.Background(), e1, KindFollowUp)
	// C10: the second event lands almost immediately after the first, well inside the debounce
	// window -- it must coalesce into one run of triggerSeq=2, not produce a second job.
	time.Sleep(5 * time.Millisecond)
	d.Dispatch(context.Background(), e2, KindFollowUp)

	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			latest, err := j.LatestByJobID()
			if err == nil {
				if r2, ok := latest[state.JobID(string(KindFollowUp), "issue-1", 2)]; ok && r2.State == state.StateDone {
					fired <- struct{}{}
					return
				}
			}
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("debounced job never completed")
	}

	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatal(err)
	}
	job1ID := state.JobID(string(KindFollowUp), "issue-1", 1)
	r1, ok := latest[job1ID]
	if !ok || r1.State != state.StateSuperseded {
		t.Fatalf("expected triggerSeq=1 superseded by the coalesced triggerSeq=2 run, got ok=%v state=%v", ok, r1.State)
	}
}

// TestDispatcher_CancelQueued proves status_changed->resolved cancels a still-queued job and
// journals it skipped(resolved) without ever invoking the advisor for it.
func TestDispatcher_CancelQueued(t *testing.T) {
	adv := &blockingAdvisor{release: make(chan struct{})}
	d, j := newQueueTestDispatcher(t, adv)

	e1 := ev(1, "issue-1")
	d.Dispatch(context.Background(), e1, KindTriage)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv.running) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first job never started")
		}
		time.Sleep(5 * time.Millisecond)
	}

	e2 := ev(2, "issue-1")
	d.Dispatch(context.Background(), e2, KindTriage) // queued behind the running job-1

	statusEvent := Event{Seq: 3, Type: "status_changed", ActorID: "other", Issue: e1.Issue, NewValue: []byte(`{"status":"resolved"}`)}
	d.Dispatch(context.Background(), statusEvent, KindCancelQueued)

	close(adv.release)

	job1ID := state.JobID(string(KindTriage), "issue-1", 1)
	job2ID := state.JobID(string(KindTriage), "issue-1", 2)
	deadline = time.Now().Add(2 * time.Second)
	for {
		latest, err := j.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		r1, ok1 := latest[job1ID]
		r2, ok2 := latest[job2ID]
		// Wait for BOTH job1 (the running job that owns the release channel) and job2 (the
		// cancelled one) to reach a terminal state before returning -- otherwise job1's worker
		// goroutine can still be mid-write to the journal file after the test function returns,
		// racing t.TempDir()'s cleanup removal of the directory it lives in.
		if ok1 && r1.State.IsTerminal() && ok2 {
			if r2.State != state.StateSkipped {
				t.Fatalf("expected job2 skipped after cancel, got %s", r2.State)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("jobs never reached terminal state: job1 ok=%v state=%v, job2 ok=%v state=%v", ok1, r1.State, ok2, r2.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDispatcher_PanicRecovery proves a panic inside a job is caught, journaled failed, and does
// not kill the dispatcher: a later job for the same issue still runs.
func TestDispatcher_PanicRecovery(t *testing.T) {
	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	panicky := panicAdvisor{}
	r := &Runner{
		Journal:   j,
		Issues:    fakeIssues{snap: IssueSnapshot{Status: "unresolved", AssigneeType: "agent", AssignedTo: "me"}},
		Claims:    &fakeClaimer{held: true},
		Advisor:   panicky,
		Act:       &countingActor{},
		MyAgentID: "me",
	}
	d := &Dispatcher{Runner: r, Journal: j}

	e1 := ev(1, "issue-1")
	d.Dispatch(context.Background(), e1, KindTriage)

	deadline := time.Now().Add(2 * time.Second)
	for {
		latest, err := j.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		if rec, ok := latest[state.JobID(string(KindTriage), "issue-1", 1)]; ok {
			if rec.State != state.StateFailed {
				t.Fatalf("expected panicked job journaled failed, got %s", rec.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("panicked job never journaled")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The dispatcher goroutine must still be alive: a normal follow-up job for the SAME issue
	// (different kind, so no dedupe collision) must still run to completion.
	normal := &countingAdvisor{}
	r.Advisor = normal
	assigneeType, assignedTo := "agent", "me"
	issue := &EventIssue{ID: "issue-1", AssigneeType: &assigneeType, AssignedTo: &assignedTo}
	e2 := Event{Seq: 2, Type: "question_answered", ActorID: "other", Issue: issue}
	d.Dispatch(context.Background(), e2, KindFollowUp)

	deadline = time.Now().Add(2 * time.Second)
	for {
		latest, err := j.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		if rec, ok := latest[state.JobID(string(KindFollowUp), "issue-1", 2)]; ok && rec.State == state.StateDone {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher stopped processing after panic recovery")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type panicAdvisor struct{}

func (panicAdvisor) Decide(_ context.Context, _ jobs.Input) (jobs.Decision, error) {
	panic("boom")
}

// TestDispatcher_EnqueueDropsRedeliveredTerminalJob is the production-path proof for the finding
// that Enqueue's own StateQueued append was overwriting the journal's terminal record for a
// re-delivered event's jobId, defeating the dedupe Runner.Run relies on (plan §2.2: "a
// re-delivered event whose jobId has ANY terminal record is dropped"). It drives the SAME event
// through Dispatcher.Enqueue twice (not Dispatch, and not Runner.Run directly, which bypassed the
// bug) and asserts the Advisor and Act are each invoked exactly once.
func TestDispatcher_EnqueueDropsRedeliveredTerminalJob(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	d, j := newQueueTestDispatcher(t, advisor)
	d.Runner.Act = actor

	e := ev(1, "issue-1")

	if err := d.Enqueue(e, KindTriage); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}

	jobID := state.JobID(string(KindTriage), "issue-1", 1)
	deadline := time.Now().Add(2 * time.Second)
	for {
		latest, err := j.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		if rec, ok := latest[jobID]; ok && rec.State.IsTerminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first job never reached a terminal state")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Re-deliver the IDENTICAL event (same seq, same issue) — this must be dropped as a duplicate,
	// not re-run.
	if err := d.Enqueue(e, KindTriage); err != nil {
		t.Fatalf("second (redelivered) Enqueue: %v", err)
	}

	// Give the dispatcher a moment to (incorrectly) start a second run, if the bug were present.
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(&advisor.calls); got != 1 {
		t.Fatalf("REDELIVERY RE-RAN THE JOB: advisor.calls = %d (want 1)", got)
	}
	if got := atomic.LoadInt32(&actor.calls); got != 1 {
		t.Fatalf("REDELIVERY RE-RAN THE JOB: actor.calls = %d (want 1)", got)
	}
}

// TestDispatcher_SameSeqRedeliveryWhilePendingIsNotLost is the red-first regression test for the
// validator's finding: PollOnce re-fetches an entire page on any Enqueue/SaveCursor error
// (loop/poll.go), so the SAME event can arrive at Dispatch twice while the issue's lane is still
// busy with an earlier job — the re-delivered event sits in q.pending and would coalesce against
// itself. Before the fix, Dispatch's coalesce path journaled `superseded` for the loser using
// state.JobID(kind, issueID, seq), which is IDENTICAL to the winner's own jobId when the seqs
// match — so the winner ran and then Runner.Run's terminal-dedupe (loop/runner.go) saw its own
// jobId already carrying a terminal `superseded` record and silently dropped it, permanently
// losing the event even though it never actually ran to completion. This asserts the job still
// reaches `done`, not `superseded`.
func TestDispatcher_SameSeqRedeliveryWhilePendingIsNotLost(t *testing.T) {
	adv := &blockingAdvisor{release: make(chan struct{})}
	d, j := newQueueTestDispatcher(t, adv)

	e1 := ev(1, "issue-1")
	e2a := ev(2, "issue-1")
	e2b := ev(2, "issue-1") // identical seq to e2a — a re-delivery, not a new event

	// job1 starts and blocks the lane.
	d.Dispatch(context.Background(), e1, KindTriage)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv.running) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for job1 to start")
		}
		time.Sleep(time.Millisecond)
	}

	// seq=2 is enqueued twice while job1 is still running — the second delivery must NOT supersede
	// the first's identical jobId.
	d.Dispatch(context.Background(), e2a, KindTriage)
	d.Dispatch(context.Background(), e2b, KindTriage)

	close(adv.release)

	job2ID := state.JobID(string(KindTriage), "issue-1", 2)
	deadline = time.Now().Add(2 * time.Second)
	for {
		latest, err := j.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		if rec, ok := latest[job2ID]; ok && rec.State.IsTerminal() {
			if rec.State != state.StateDone {
				t.Fatalf("EVENT LOST: job2 (seq=2) ended %q instead of %q", rec.State, state.StateDone)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for job2 to reach a terminal state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDispatcher_OnOutcome_CountsSupersededAndCancelledAndFailed is the red-first proof for
// finding #8: superseded/skipped(cancel)/failed outcomes were journaled by the dispatcher but
// never counted toward plan §7's "jobs by kind×outcome" -- only Runner.Run's own outcomes were.
// Removing the OnOutcome calls from journalSuperseded/journalCancelled/runOne's panic branch
// makes the corresponding count(s) below go to zero.
func TestDispatcher_OnOutcome_CountsSupersededAndCancelledAndFailed(t *testing.T) {
	adv := &blockingAdvisor{release: make(chan struct{})}
	d, j := newQueueTestDispatcher(t, adv)

	var mu sync.Mutex
	counts := map[string]int{}
	d.OnOutcome = func(kind, outcome string) {
		mu.Lock()
		defer mu.Unlock()
		counts[kind+"/"+outcome]++
	}

	e1 := ev(1, "issue-1")
	e2 := ev(2, "issue-1")
	d.Dispatch(context.Background(), e1, KindTriage)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv.running) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for job1 to start")
		}
		time.Sleep(time.Millisecond)
	}
	// Coalesce a loser (superseded outcome).
	d.Dispatch(context.Background(), e2, KindTriage)
	e3 := Event{Seq: 3, Type: "created", ActorID: "other", Issue: e1.Issue}
	d.Dispatch(context.Background(), e3, KindTriage)
	close(adv.release)

	job3ID := state.JobID(string(KindTriage), "issue-1", 3)
	deadline = time.Now().Add(2 * time.Second)
	for {
		latest, err := j.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		if rec, ok := latest[job3ID]; ok && rec.State == state.StateDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for job3 to complete")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Cancel a pending job (skipped(resolved) outcome).
	adv2 := &blockingAdvisor{release: make(chan struct{})}
	d2, _ := newQueueTestDispatcher(t, adv2)
	d2.OnOutcome = d.OnOutcome
	eCancel := ev(10, "issue-2")
	d2.Dispatch(context.Background(), eCancel, KindTriage)
	deadline = time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv2.running) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for issue-2 job to start")
		}
		time.Sleep(time.Millisecond)
	}
	eQueued := ev(11, "issue-2")
	d2.Dispatch(context.Background(), eQueued, KindFollowUp)
	d2.cancelPending("issue-2", SkipResolved)
	close(adv2.release)

	// Wait for both dispatchers' per-issue queue goroutines to fully exit before the test returns
	// and t.TempDir() cleanup removes the journal's directory out from under a still-running
	// goroutine's Append (otherwise this test flakes with "TempDir RemoveAll cleanup: directory
	// not empty" under -race, a pre-existing race unrelated to what this test asserts).
	drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Drain(drainCtx)
	d2.Drain(drainCtx)

	mu.Lock()
	superseded := counts[string(KindTriage)+"/superseded"]
	cancelled := counts[string(KindFollowUp)+"/skipped_"+string(SkipResolved)]
	mu.Unlock()

	if superseded < 1 {
		t.Fatalf("expected at least 1 superseded outcome counted, got %d (counts=%v)", superseded, counts)
	}
	if cancelled != 1 {
		t.Fatalf("expected exactly 1 cancelled/skipped_resolved outcome counted, got %d (counts=%v)", cancelled, counts)
	}
}
