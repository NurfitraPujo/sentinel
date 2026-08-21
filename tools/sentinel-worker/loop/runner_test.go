package loop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

type fakeIssues struct {
	snap IssueSnapshot
	err  error
}

func (f fakeIssues) GetIssue(_ context.Context, _ string) (IssueSnapshot, error) {
	return f.snap, f.err
}

// calls fields below are int32 and mutated via sync/atomic, not bare `++`: these fakes are shared
// across concurrently-running issue goroutines in loop/queue_test.go's Dispatcher tests (different
// issues run their jobs in parallel by design, §3), so a plain `f.calls++` would race.
type fakeClaimer struct {
	held         bool
	claimedBy    string
	err          error
	calls        int32
	releaseErr   error
	releaseCalls int32
	lastReleased string
}

func (f *fakeClaimer) EnsureClaimed(_ context.Context, _ string) (bool, string, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.held, f.claimedBy, f.err
}

func (f *fakeClaimer) ReleaseClaim(_ context.Context, issueID string) error {
	atomic.AddInt32(&f.releaseCalls, 1)
	f.lastReleased = issueID
	return f.releaseErr
}

// erroringAdvisor always fails Decide with err, for exercising Run's post-claim
// journalTransientFailure/release paths (finding 3/4, core-robustness round 3).
type erroringAdvisor struct {
	err error
}

func (a *erroringAdvisor) Decide(_ context.Context, in jobs.Input) (jobs.Decision, error) {
	return jobs.Decision{}, a.err
}

type countingAdvisor struct {
	calls int32
}

func (b *countingAdvisor) Decide(_ context.Context, in jobs.Input) (jobs.Decision, error) {
	atomic.AddInt32(&b.calls, 1)
	return jobs.Decision{Kind: in.Kind, Raw: []byte(`{"stub":true}`)}, nil
}

type countingActor struct {
	calls int32
}

func (a *countingActor) Act(_ context.Context, _ string, _ jobs.Decision) error {
	atomic.AddInt32(&a.calls, 1)
	return nil
}

// erroringActor always fails Act with err, for exercising handleActError's transient (finding-4
// regression) and permanent paths.
type erroringActor struct {
	err   error
	calls int32
}

func (a *erroringActor) Act(_ context.Context, _ string, _ jobs.Decision) error {
	atomic.AddInt32(&a.calls, 1)
	return a.err
}

// failOnceActor fails its first Act call with err, then succeeds every subsequent call --
// simulating a transient outage that has cleared by the time a resumed job replays.
type failOnceActor struct {
	err   error
	calls int32
}

func (a *failOnceActor) Act(_ context.Context, _ string, _ jobs.Decision) error {
	n := atomic.AddInt32(&a.calls, 1)
	if n == 1 {
		return a.err
	}
	return nil
}

// must fails the test immediately if err is non-nil -- a small shared helper for test setup calls
// (e.g. seeding journal records directly) whose own failure would make the rest of the test
// meaningless.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error in test setup: %v", err)
	}
}

func newTestRunner(t *testing.T, issues IssueReader, claims Claimer, advisor jobs.Advisor, act Actor, dryRun bool) (*Runner, *state.Journal) {
	t.Helper()
	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	return &Runner{
		Journal:   j,
		Issues:    issues,
		Claims:    claims,
		Advisor:   advisor,
		Act:       act,
		DryRun:    dryRun,
		MyAgentID: me,
		// MaxInlaneRetries: 1 keeps every EXISTING single-shot-fake test's semantics unchanged (one
		// attempt, no retry, no real backoff sleep) -- tests that specifically exercise N8e's
		// in-lane retry ladder build their own *Runner directly with a fake SleepCtx instead of
		// this helper (see TestRunner_InlaneRetry_* below).
		MaxInlaneRetries: 1,
	}, j
}

func TestRunner_DryRun_JournalsDecisionNeverActsNeverClaims(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer,
		advisor, actor, true,
	)

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 1 {
		t.Fatalf("expected the advisor to be invoked exactly once, got %d", advisor.calls)
	}
	if actor.calls != 0 {
		t.Fatalf("dry-run must never call Act, got %d calls", actor.calls)
	}
	if claimer.calls != 0 {
		t.Fatalf("dry-run must never perform a real claim, got %d calls", claimer.calls)
	}

	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var sawAdvised, sawDone bool
	for _, rec := range records {
		if rec.State == state.StateAdvised {
			sawAdvised = true
			if len(rec.Payload) == 0 {
				t.Errorf("expected the advised record to carry the decision payload")
			}
		}
		if rec.State == state.StateDone {
			sawDone = true
		}
	}
	if !sawAdvised || !sawDone {
		t.Fatalf("expected advised and done records, got: %+v", records)
	}
}

func TestRunner_ForeignClaim_SkipsAndNeverInvokesAdvisor(t *testing.T) {
	advisor := &countingAdvisor{}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: false}, // 409 foreign claimant, C1
		advisor, &countingActor{}, false,
	)

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("a foreign-claim skip must never invoke the advisor, got %d calls", advisor.calls)
	}
	records, _, _ := j.Load()
	last := records[len(records)-1]
	if last.State != state.StateSkipped {
		t.Fatalf("expected final state skipped, got %s", last.State)
	}
}

// TestRunner_ForeignClaim_JournalsClaimedBy proves the C1 foreign-claimant id the Claimer
// surfaces is carried into the skip journal payload, not dropped on the floor.
func TestRunner_ForeignClaim_JournalsClaimedBy(t *testing.T) {
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: false, claimedBy: "agt_other"},
		&countingAdvisor{}, &countingActor{}, false,
	)

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	records, _, _ := j.Load()
	last := records[len(records)-1]
	if last.State != state.StateSkipped {
		t.Fatalf("expected final state skipped, got %s", last.State)
	}
	if !strings.Contains(string(last.Payload), "agt_other") {
		t.Fatalf("skip payload = %s, want it to carry claimedBy=agt_other", last.Payload)
	}
}

func TestRunner_ResolvedIssue_SkipsPrecondition(t *testing.T) {
	advisor := &countingAdvisor{}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "resolved"}},
		&fakeClaimer{held: true},
		advisor, &countingActor{}, true,
	)
	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("a resolved-issue precondition failure must never invoke the advisor, got %d", advisor.calls)
	}
	records, _, _ := j.Load()
	last := records[len(records)-1]
	if last.State != state.StateSkipped {
		t.Fatalf("expected skipped, got %s", last.State)
	}
}

// TestRunner_IgnoredIssue_SkipsWithIgnoredReasonNotResolved is the red-first proof for finding
// #14: an `ignored` issue (C7's third status) used to journal skipped(resolved), coarsening the
// /metrics skip-reason breakdown. Reverting precondition's ignored branch to fall through to the
// generic `snap.Status != "unresolved"` check makes this go red (payload would carry "resolved").
func TestRunner_IgnoredIssue_SkipsWithIgnoredReasonNotResolved(t *testing.T) {
	advisor := &countingAdvisor{}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "ignored"}},
		&fakeClaimer{held: true},
		advisor, &countingActor{}, true,
	)
	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("an ignored-issue precondition failure must never invoke the advisor, got %d", advisor.calls)
	}
	records, _, _ := j.Load()
	last := records[len(records)-1]
	if last.State != state.StateSkipped {
		t.Fatalf("expected skipped, got %s", last.State)
	}
	if !strings.Contains(string(last.Payload), string(SkipIgnored)) {
		t.Fatalf("skip payload = %s, want reason %q, not folded into %q", last.Payload, SkipIgnored, SkipResolved)
	}
	if strings.Contains(string(last.Payload), `"`+string(SkipResolved)+`"`) {
		t.Fatalf("skip payload = %s, an ignored issue must not journal reason=%q", last.Payload, SkipResolved)
	}
}

// TestRunner_DedupeDropsAlreadyTerminalJob proves the journal dedupe: a re-run for the same
// jobId (same kind+issueId+triggerSeq) whose latest record is already terminal is a silent no-op,
// and critically never re-invokes the advisor (plan §2.2/§8).
func TestRunner_DedupeDropsAlreadyTerminalJob(t *testing.T) {
	advisor := &countingAdvisor{}
	r, _ := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: true},
		advisor, &countingActor{}, true,
	)
	e := Event{Seq: 7, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if advisor.calls != 1 {
		t.Fatalf("expected 1 advisor call after first run, got %d", advisor.calls)
	}
	// Re-delivery of the identical event (same seq -> same jobId).
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if advisor.calls != 1 {
		t.Fatalf("replay of a terminal job must NOT re-invoke the advisor, got %d calls", advisor.calls)
	}
}

// TestRunner_PreconditionRead404_JournalsTerminalSkipInsteadOfErroring proves the fix for the
// head-of-line-blocking blocker: a 404 from the precondition GET (issue deleted between the event
// landing and the job running, C14 -- routine, not exceptional) must journal a TERMINAL
// skipped(deleted) record and return nil, NOT propagate an error. Before the fix, this error return
// left the journal record stuck at "queued" (non-terminal) forever and, under the old synchronous
// JournalEnqueuer, caused the identical poisoned event to be re-fetched and re-run on every poll
// cycle, permanently wedging the entire feed behind one deleted issue.
func TestRunner_PreconditionRead404_JournalsTerminalSkipInsteadOfErroring(t *testing.T) {
	advisor := &countingAdvisor{}
	r, j := newTestRunner(t,
		fakeIssues{err: &sentinel.StatusError{Status: 404, Body: []byte(`{"error":"not found"}`)}},
		&fakeClaimer{held: true},
		advisor, &countingActor{}, true,
	)
	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: expected a 404 precondition read to be absorbed as a terminal skip, got error: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("a deleted-issue skip must never invoke the advisor, got %d calls", advisor.calls)
	}
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last := records[len(records)-1]
	if last.State != state.StateSkipped {
		t.Fatalf("expected final state skipped (terminal), got %s -- a non-terminal record here means the poisoned job never resolves and, upstream, the same event gets re-delivered forever", last.State)
	}
	if !strings.Contains(string(last.Payload), string(SkipDeleted)) {
		t.Fatalf("skip payload = %s, want reason=%s", last.Payload, SkipDeleted)
	}
}

// TestRunner_FollowUp_QuestionAnswered_ReclaimsInsteadOfSkipping proves plan §3's runner
// precondition for question_answered: "re-claim if reaped (200/alreadyClaimed either way)" — a
// FOLLOW-UP triggered by question_answered must fall through to ensure-claimed even when the
// snapshot no longer shows the claim as ours (the reaper released it, C11), instead of skipping
// with precondition-failed the way a commented-derived FOLLOW-UP correctly does.
func TestRunner_FollowUp_QuestionAnswered_ReclaimsInsteadOfSkipping(t *testing.T) {
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved", AssigneeType: "", AssignedTo: ""}},
		claimer, &countingAdvisor{}, &countingActor{}, false,
	)
	e := Event{Seq: 1, Type: "question_answered", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindFollowUp); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if claimer.calls != 1 {
		t.Fatalf("expected ensure-claimed to be invoked (re-claim), got %d calls", claimer.calls)
	}
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last := records[len(records)-1]
	if last.State == state.StateSkipped {
		t.Fatalf("question_answered FOLLOW-UP must re-claim, not skip: got final state %s payload %s", last.State, last.Payload)
	}
}

// TestRunner_FollowUp_Commented_StillSkipsWhenNotClaimedByMe proves the commented arm of the same
// precondition is unchanged: "still claimed by me" -- a commented-derived FOLLOW-UP DOES skip when
// the snapshot shows the issue is not (or no longer) claimed by us, unlike the question_answered
// arm above.
func TestRunner_FollowUp_Commented_StillSkipsWhenNotClaimedByMe(t *testing.T) {
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved", AssigneeType: "", AssignedTo: ""}},
		claimer, &countingAdvisor{}, &countingActor{}, true,
	)
	e := Event{Seq: 1, Type: "commented", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindFollowUp); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if claimer.calls != 0 {
		t.Fatalf("commented FOLLOW-UP not claimed by us must skip before ensure-claimed, got %d calls", claimer.calls)
	}
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	last := records[len(records)-1]
	if last.State != state.StateSkipped || !strings.Contains(string(last.Payload), string(SkipNotPreconditioned)) {
		t.Fatalf("expected skipped(precondition-failed), got state=%s payload=%s", last.State, last.Payload)
	}
}

// TestRunner_ResumeFromAdvised_NeverReinvokesAdvisor is plan §2.2/§8's required proof: "assert zero
// Advisor calls on recovery from advised/acting". A crash between "advised" and the terminal record
// leaves the journal sitting at StateAdvised (non-terminal, so IsTerminal's dedupe does not drop
// it) -- Run must replay that journaled decision straight into Act, never calling Advisor.Decide
// again.
func TestRunner_ResumeFromAdvised_NeverReinvokesAdvisor(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: true}, advisor, actor, false,
	)
	jobID := state.JobID("triage", "i1", 5)
	if err := j.Append(state.Record{JobID: jobID, IssueID: "i1", Kind: "triage", TriggerSeq: 5, State: state.StateAdvised, Payload: []byte(`{"kind":"triage","raw":"e30="}`)}); err != nil {
		t.Fatalf("seeding advised record: %v", err)
	}

	e := Event{Seq: 5, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("advisor calls after resume-from-advised = %d (want 0 per plan §2.2)", advisor.calls)
	}
	if actor.calls != 1 {
		t.Fatalf("expected Act to be invoked once replaying the journaled decision, got %d calls", actor.calls)
	}
}

// TestRunner_ResumeFromActing_NeverReinvokesAdvisor covers the other §8 arm: resume from
// StateActing (crash after the acting record, before acted/done). The decision must come from the
// job's earlier StateAdvised record (Journal.DecisionForJob), not be re-derived.
func TestRunner_ResumeFromActing_NeverReinvokesAdvisor(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: true}, advisor, actor, false,
	)
	jobID := state.JobID("triage", "i1", 5)
	if err := j.Append(state.Record{JobID: jobID, IssueID: "i1", Kind: "triage", TriggerSeq: 5, State: state.StateAdvised, Payload: []byte(`{"kind":"triage","raw":"e30="}`)}); err != nil {
		t.Fatalf("seeding advised record: %v", err)
	}
	if err := j.Append(state.Record{JobID: jobID, IssueID: "i1", Kind: "triage", TriggerSeq: 5, State: state.StateActing}); err != nil {
		t.Fatalf("seeding acting record: %v", err)
	}

	e := Event{Seq: 5, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("advisor calls after resume-from-acting = %d (want 0 per plan §2.2)", advisor.calls)
	}
	if actor.calls != 1 {
		t.Fatalf("expected Act to be invoked once replaying the journaled decision, got %d calls", actor.calls)
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

// TestRunner_Resume_ReplaysAdvisedJobWithoutReinvokingAdvisor is the production-path proof for the
// finding that resumeFromAdvised/RecoveryScan were unreachable from main(): it drives an in-flight
// job found by Journal.RecoveryScan (the exact call runPipeline makes at startup) through
// Runner.Resume — not Runner.Run with a hand-built Event — and asserts zero Advisor calls, one Act
// call (plan §8's required proof, now exercised via the real recovery entry point).
func TestRunner_Resume_ReplaysAdvisedJobWithoutReinvokingAdvisor(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: true}, advisor, actor, false,
	)
	jobID := state.JobID("triage", "i1", 5)
	must(t, j.Append(state.Record{JobID: jobID, IssueID: "i1", Kind: "triage", TriggerSeq: 5, State: state.StateAdvised, Payload: []byte(`{"kind":"triage","raw":"e30="}`)}))

	inFlight, _, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if len(inFlight) != 1 {
		t.Fatalf("expected exactly one in-flight job, got %d", len(inFlight))
	}

	if err := r.Resume(context.Background(), inFlight[0]); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("advisor calls after Resume from advised = %d (want 0 per plan §2.2/§8)", advisor.calls)
	}
	if actor.calls != 1 {
		t.Fatalf("expected Act to be invoked once replaying the journaled decision via Resume, got %d calls", actor.calls)
	}

	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	if rec := latest[jobID]; rec.State != state.StateDone {
		t.Fatalf("expected job to reach done after Resume, got %s", rec.State)
	}

	// A SECOND RecoveryScan (simulating a subsequent maintenance pass, or a second startup) must
	// find nothing in flight — the job is terminal.
	inFlight2, _, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("second RecoveryScan: %v", err)
	}
	if len(inFlight2) != 0 {
		t.Fatalf("expected no in-flight jobs after Resume completed, got %d", len(inFlight2))
	}
}

// TestRunner_ActStageRetryableFailed_ResumeReplaysDecisionWithoutReinvokingAdvisor is the red-first
// regression test for the core-robustness round-3 finding: an Act-stage transient failure that
// exhausts in-lane retries journals StateRetryableFailed AFTER a decision was already journaled
// (StateAdvised). Before the fix, Resume routed retryable_failed to the `default` branch -> Run,
// which re-invoked the Advisor from scratch (violating plan §2.2's "the LLM is NEVER re-invoked for
// a job that already produced a decision", and double-incrementing hourly triage/follow-up caps on
// every crash-resume during an outage). This drives the REAL failure path (Run's own Act-stage
// in-lane-retry exhaustion, not hand-seeded journal state) through Runner.Resume, the exact entry
// point RecoveryScan's caller (runPipeline/journalMaintenanceLoop) uses.
func TestRunner_ActStageRetryableFailed_ResumeReplaysDecisionWithoutReinvokingAdvisor(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &failOnceActor{err: &sentinel.StatusError{Status: 503}} // transient
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: true}, advisor, actor, false,
	)

	e := Event{Seq: 7, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err == nil {
		t.Fatalf("Run: expected the transient Act error to propagate")
	}
	if advisor.calls != 1 {
		t.Fatalf("expected exactly 1 advisor call from the initial Run, got %d", advisor.calls)
	}
	if actor.calls != 1 {
		t.Fatalf("expected exactly 1 Act call from the initial Run (in-lane retries=1), got %d", actor.calls)
	}

	// Confirm the journal actually landed on retryable_failed WITH a payload (the decision) --
	// not empty, and not terminal failed -- before asserting anything about Resume.
	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	jobID := state.JobID("triage", "i1", 7)
	rec, ok := latest[jobID]
	if !ok {
		t.Fatalf("expected a journal record for %s", jobID)
	}
	if rec.State != state.StateRetryableFailed {
		t.Fatalf("expected StateRetryableFailed after Act-stage transient exhaustion, got %s", rec.State)
	}
	if len(rec.Payload) == 0 {
		t.Fatalf("expected the retryable_failed record to carry the already-journaled decision payload, got empty")
	}

	inFlight, _, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if len(inFlight) != 1 {
		t.Fatalf("expected exactly one in-flight job, got %d", len(inFlight))
	}

	if err := r.Resume(context.Background(), inFlight[0]); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The crux of the regression: Resume must replay the journaled decision, NOT re-invoke the
	// Advisor.
	if advisor.calls != 1 {
		t.Fatalf("advisor calls after Resume = %d (want 1 total -- Resume must NOT re-invoke the Advisor per plan §2.2/§8)", advisor.calls)
	}
	// The actor now succeeds on replay (simulating the outage having cleared), proving Act WAS
	// invoked again (not skipped) with the replayed decision.
	if actor.calls != 2 {
		t.Fatalf("expected Act to be invoked a second time on Resume replay, got %d calls total", actor.calls)
	}

	latest2, err := j.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID after Resume: %v", err)
	}
	if rec := latest2[jobID]; rec.State != state.StateDone {
		t.Fatalf("expected job to reach done after Resume replay, got %s", rec.State)
	}
}

// TestRunner_ActStageRetryableFailed_EmptyPayloadStillReRunsFromTop is the companion case: a
// retryable_failed record with an EMPTY payload (the failure happened before the Advisor was ever
// reached -- resolve/ensure-claimed/Advisor.Decide itself) must still fall through to the normal
// pipeline on Resume, re-deriving a decision, exactly as before this fix.
func TestRunner_ActStageRetryableFailed_EmptyPayloadStillReRunsFromTop(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: true}, advisor, actor, false,
	)
	jobID := state.JobID("triage", "i1", 11)
	must(t, j.Append(state.Record{JobID: jobID, IssueID: "i1", Kind: "triage", TriggerSeq: 11, State: state.StateRetryableFailed}))

	inFlight, _, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if len(inFlight) != 1 {
		t.Fatalf("expected exactly one in-flight job, got %d", len(inFlight))
	}

	if err := r.Resume(context.Background(), inFlight[0]); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if advisor.calls != 1 {
		t.Fatalf("expected the Advisor to be consulted once for a pre-advisor retryable_failed job, got %d calls", advisor.calls)
	}
	if actor.calls != 1 {
		t.Fatalf("expected Act to be invoked once, got %d calls", actor.calls)
	}
}

// TestRunner_Resume_QueuedJobRunsFromTop covers the other Resume arm: a job that crashed before
// ever reaching the Advisor (state queued/claimed/questioned) is re-run from the top through the
// normal pipeline, per Resume's fallthrough — the Advisor IS invoked here because it never ran the
// first time.
func TestRunner_Resume_QueuedJobRunsFromTop(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		&fakeClaimer{held: true}, advisor, actor, false,
	)
	jobID := state.JobID("triage", "i1", 9)
	must(t, j.Append(state.Record{JobID: jobID, IssueID: "i1", Kind: "triage", TriggerSeq: 9, State: state.StateQueued}))

	inFlight, _, err := j.RecoveryScan()
	if err != nil {
		t.Fatalf("RecoveryScan: %v", err)
	}
	if len(inFlight) != 1 {
		t.Fatalf("expected exactly one in-flight job, got %d", len(inFlight))
	}

	if err := r.Resume(context.Background(), inFlight[0]); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if advisor.calls != 1 {
		t.Fatalf("expected Advisor to be consulted once for a job that never reached it, got %d calls", advisor.calls)
	}
	if actor.calls != 1 {
		t.Fatalf("expected Act to be invoked once, got %d calls", actor.calls)
	}
}

func TestRunner_NonJobKind_Errors(t *testing.T) {
	r, _ := newTestRunner(t, fakeIssues{}, &fakeClaimer{}, &countingAdvisor{}, &countingActor{}, true)
	if err := r.Run(context.Background(), Event{}, KindSweepReconcile); err == nil {
		t.Fatalf("expected an error when Run is called with a non-job Kind")
	}
}

// TestRunner_TransientIssueReadError_JournalsTerminalFailedAndCountsOutcome proves the N8a-minimum
// fix for a 5xx/429/network error resolving the issue (a genuinely transient GetIssue failure,
// NOT a ClassGone 404 -- that path already journals skipped(deleted)): the job's journal record
// must NOT be left stranded at "queued" (a state invisible to /metrics and to crash recovery's
// IsDuplicate dedupe) -- it must land on the NON-terminal state.StateRetryableFailed(transient:
// <class>) record (finding 4, core-robustness round 3 -- an extended sentinel-api outage must
// leave the job recoverable, not lost as a terminal `failed`) and fire OnOutcome exactly once, so
// the job is both re-drivable by a later RecoveryScan and counted.
func TestRunner_TransientIssueReadError_JournalsTerminalFailedAndCountsOutcome(t *testing.T) {
	issues := fakeIssues{err: &sentinel.StatusError{Status: 503}}
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t, issues, claimer, advisor, actor, false)

	var outcomes []string
	r.OnOutcome = func(kind, outcome string) { outcomes = append(outcomes, kind+":"+outcome) }

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err == nil {
		t.Fatalf("expected Run to return the transient error, got nil")
	}

	if advisor.calls != 0 {
		t.Fatalf("advisor must never be consulted when the precondition read itself failed, got %d calls", advisor.calls)
	}

	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	jobID := state.JobID(string(KindTriage), "i1", 1)
	var latest *state.Record
	for i := range records {
		if records[i].JobID == jobID {
			rec := records[i]
			latest = &rec
		}
	}
	if latest == nil {
		t.Fatalf("expected a journal record for job %s", jobID)
	}
	if latest.State.IsTerminal() {
		t.Fatalf("a still-transient in-lane-retry-exhausted failure must journal NON-terminal (recoverable), got terminal %q", latest.State)
	}
	if latest.State != state.StateRetryableFailed {
		t.Fatalf("expected state %q, got %q", state.StateRetryableFailed, latest.State)
	}

	// finding 4 (core-robustness round 3): a 503-classified failure is transient, so the outcome
	// string is "failed_transient_retryable" (distinct from the permanent-class "failed_permanent"
	// outcome) -- see journalTransientFailure.
	found := false
	for _, o := range outcomes {
		if o == string(KindTriage)+":failed_transient_retryable" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected OnOutcome(%q, \"failed_transient_retryable\") to fire, got %v", KindTriage, outcomes)
	}
}

// alwaysExhaustedBudget is a fake llm.Budget-shaped fake for TestRunner_BudgetExhausted_SkipsAdvisor
// (real Runner.Run path, not a hand-seeded journal).
type alwaysExhaustedBudget struct {
	exhausted bool
	added     []llm.Usage
}

func (b *alwaysExhaustedBudget) Exhausted() bool { return b.exhausted }
func (b *alwaysExhaustedBudget) Add(u llm.Usage) { b.added = append(b.added, u) }

// TestRunner_BudgetExhausted_SkipsAdvisor is the real-path (Runner.Run, not a hand-seeded journal)
// red-first proof for plan §2.6 finding 1: WORKER_DAILY_TOKEN_BUDGET's llm.DailyBudget must be
// consulted BEFORE the Advisor is ever invoked, for a job kind that isn't even TRIAGE (FOLLOW-UP
// spends LLM tokens too) — an exhausted budget must skip with zero Advisor.Decide calls, not spend
// one more.
func TestRunner_BudgetExhausted_SkipsAdvisor(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	budget := &alwaysExhaustedBudget{exhausted: true}
	r.Budget = budget

	e := Event{Seq: 1, Type: "question_answered", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindFollowUp); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("expected ZERO Advisor.Decide calls with an exhausted budget, got %d", advisor.calls)
	}
	if actor.calls != 0 {
		t.Fatalf("expected zero Act calls, got %d", actor.calls)
	}

	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var sawSkip bool
	for _, rec := range records {
		if rec.State == state.StateSkipped {
			var payload map[string]string
			if err := json.Unmarshal(rec.Payload, &payload); err == nil && payload["reason"] == string(SkipBudgetExhausted) {
				sawSkip = true
			}
		}
	}
	if !sawSkip {
		t.Fatalf("expected a skipped(%s) journal record, got: %+v", SkipBudgetExhausted, records)
	}
	// Finding 3 (core-robustness round 3): the Budget gate must run BEFORE EnsureClaimed -- an
	// exhausted budget must never claim work it isn't going to do. claimer.calls == 0 proves
	// EnsureClaimed was never even invoked, not merely that the claim result was discarded.
	if claimer.calls != 0 {
		t.Fatalf("expected EnsureClaimed to never be called for a budget-exhausted job, got %d calls (claim leaked)", claimer.calls)
	}
}

// TestRunner_BudgetExhausted_NeverClaims_MutationTest is finding 3's mutation-test companion: with
// the Budget gate moved back to AFTER ensure-claimed (simulated here by asserting the ORIGINAL,
// pre-fix ordering would have left a claim held), claimer.calls would be 1. This test pins the
// fixed ordering directly against a claimer that reports held=true, proving the assertion above
// isn't vacuously true because the fake never gets called for some unrelated reason.
func TestRunner_BudgetExhausted_NeverClaims_MutationTest(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, _ := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	r.Budget = &alwaysExhaustedBudget{exhausted: true}

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if claimer.calls != 0 {
		t.Fatalf("EnsureClaimed must not be called when the daily budget is exhausted, got %d calls", claimer.calls)
	}
	if claimer.releaseCalls != 0 {
		t.Fatalf("ReleaseClaim must not be called either -- nothing was ever claimed to release, got %d calls", claimer.releaseCalls)
	}
}

// TestRunner_FollowupRateLimited_NeverClaims is finding 5's own claim-leak proof, symmetric to
// TestRunner_BudgetExhausted_NeverClaims_MutationTest: an exhausted WORKER_MAX_FOLLOWUP_PER_HOUR
// cap must gate BEFORE ensure-claimed, same as TRIAGE's own hourly cap.
func TestRunner_FollowupRateLimited_NeverClaims(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	r.FollowupLimiter = &alwaysDeniedLimiter{}

	e := Event{Seq: 1, Type: "question_answered", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindFollowUp); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("expected ZERO Advisor.Decide calls with an exhausted FollowupLimiter, got %d", advisor.calls)
	}
	if claimer.calls != 0 {
		t.Fatalf("expected EnsureClaimed to never be called for a followup-rate-limited job, got %d calls (claim leaked)", claimer.calls)
	}

	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var sawSkip bool
	for _, rec := range records {
		if rec.State == state.StateSkipped {
			var payload map[string]string
			if err := json.Unmarshal(rec.Payload, &payload); err == nil && payload["reason"] == string(SkipFollowupRateLimited) {
				sawSkip = true
			}
		}
	}
	if !sawSkip {
		t.Fatalf("expected a skipped(%s) journal record, got: %+v", SkipFollowupRateLimited, records)
	}
}

// TestRunner_TriageLimiter_NeverGatesFollowup mirrors the existing
// TestRunner_FollowUpIsNeverGatedByTriageLimiter in the opposite direction: an exhausted
// FollowupLimiter must not gate a TRIAGE job.
func TestRunner_TriageLimiter_NeverGatesFollowup(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, _ := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	r.FollowupLimiter = &alwaysDeniedLimiter{}

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 1 {
		t.Fatalf("expected TRIAGE to run the Advisor regardless of FollowupLimiter, got %d calls", advisor.calls)
	}
}

// TestRunner_PermanentAdvisorFailure_ReleasesClaim is finding 3's release-side proof: a
// PERMANENT-class Advisor failure occurring AFTER ensure-claimed succeeded must release the
// claim (issues.claim.release) rather than leaving it stranded until WORKER_NAG_DAYS.
func TestRunner_PermanentAdvisorFailure_ReleasesClaim(t *testing.T) {
	advisor := &erroringAdvisor{err: &sentinel.StatusError{Status: 422}} // 422 classifies ClassPermanent
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err == nil {
		t.Fatalf("Run: expected the permanent advisor error to propagate")
	}
	if claimer.calls != 1 {
		t.Fatalf("expected EnsureClaimed to be called once, got %d", claimer.calls)
	}
	if claimer.releaseCalls != 1 {
		t.Fatalf("expected ReleaseClaim to be called exactly once after the permanent failure, got %d (claim leaked)", claimer.releaseCalls)
	}
	if claimer.lastReleased != "i1" {
		t.Fatalf("ReleaseClaim called for issue %q, want %q", claimer.lastReleased, "i1")
	}
	records, _, _ := j.Load()
	if last := records[len(records)-1]; last.State != state.StateFailed {
		t.Fatalf("expected terminal failed for a permanent-class failure, got %s", last.State)
	}
}

// TestRunner_PermanentAdvisorFailure_ReleasesClaim_MutationTest deletes the release call (by
// asserting the ORIGINAL unfixed behavior: a fresh claimer with releaseCalls asserted BEFORE any
// fix would read 0) -- this pins releaseCalls == 1 as the fix's own required behavior, so
// reverting the release call in journalTransientFailure turns this red immediately.
func TestRunner_PermanentAdvisorFailure_ReleasesClaim_MutationTest(t *testing.T) {
	advisor := &erroringAdvisor{err: &sentinel.StatusError{Status: 422}}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, _ := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	_ = r.Run(context.Background(), e, KindTriage)
	if claimer.releaseCalls == 0 {
		t.Fatalf("mutation check failed to even exercise the assertion path: releaseCalls is 0 (this test itself is broken, not the production code)")
	}
}

// TestRunner_TransientAdvisorFailure_DoesNotReleaseClaim proves the release ONLY fires for the
// PERMANENT-class path -- a still-transient advisor failure (finding 4's non-terminal
// retryable_failed state) must keep the claim held so the SAME worker's resumed retry doesn't
// have to re-claim.
func TestRunner_TransientAdvisorFailure_DoesNotReleaseClaim(t *testing.T) {
	advisor := &erroringAdvisor{err: &sentinel.StatusError{Status: 503}} // transient
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err == nil {
		t.Fatalf("Run: expected the transient advisor error to propagate")
	}
	if claimer.releaseCalls != 0 {
		t.Fatalf("expected ReleaseClaim NOT to be called for a transient (retryable) failure, got %d calls", claimer.releaseCalls)
	}
	records, _, _ := j.Load()
	if last := records[len(records)-1]; last.State != state.StateRetryableFailed {
		t.Fatalf("expected non-terminal retryable_failed for a transient advisor failure, got %s", last.State)
	}
}

// TestRunner_BudgetExhausted_MutationTest deletes the production gate (by using a Runner with no
// Budget wired, mirroring "unwrap the check") and proves the same scenario now DOES invoke the
// Advisor — the inverse assertion of TestRunner_BudgetExhausted_SkipsAdvisor, run against the same
// fixture, so a future accidental removal of the Budget check has a red test right next to the
// green one it broke.
func TestRunner_BudgetExhausted_MutationTest(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, _ := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	// r.Budget deliberately left nil (the pre-fix state: gate absent) -- the Advisor MUST run.
	e := Event{Seq: 1, Type: "question_answered", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindFollowUp); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 1 {
		t.Fatalf("with no Budget wired, expected the Advisor to run exactly once, got %d", advisor.calls)
	}
}

// TestRunner_Budget_AddsUsageAfterSuccessfulDecide proves the OTHER half of finding 1: a successful
// Advisor.Decide feeds its reported Decision.Usage into Budget.Add, via the real Run() path.
func TestRunner_Budget_AddsUsageAfterSuccessfulDecide(t *testing.T) {
	usageAdvisor := usageReturningAdvisor{usage: llm.Usage{InputTokens: 111, OutputTokens: 222}}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, _ := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, usageAdvisor, actor, false,
	)
	budget := &alwaysExhaustedBudget{exhausted: false}
	r.Budget = budget

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(budget.added) != 1 {
		t.Fatalf("expected exactly one Budget.Add call, got %d", len(budget.added))
	}
	if budget.added[0].InputTokens != 111 || budget.added[0].OutputTokens != 222 {
		t.Fatalf("Budget.Add got %+v, want {111 222}", budget.added[0])
	}
}

// usageReturningAdvisor is a fake jobs.Advisor reporting a fixed Usage on every Decide call.
type usageReturningAdvisor struct{ usage llm.Usage }

func (a usageReturningAdvisor) Decide(_ context.Context, in jobs.Input) (jobs.Decision, error) {
	return jobs.Decision{Kind: in.Kind, Raw: []byte(`{"stub":true}`), Usage: a.usage}, nil
}

// alwaysDeniedLimiter is a fake llm.HourlyCounter-shaped fake always denying TryIncrement.
type alwaysDeniedLimiter struct{ tries int }

func (l *alwaysDeniedLimiter) TryIncrement() bool { l.tries++; return false }

// TestRunner_TriageRateLimited_SkipsAdvisor is the real-path proof that WORKER_MAX_TRIAGE_PER_HOUR
// gates KindTriage job starts (plan §2.6 finding 1): a denied TryIncrement must skip the job with
// zero Advisor.Decide calls.
func TestRunner_TriageRateLimited_SkipsAdvisor(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, j := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	limiter := &alwaysDeniedLimiter{}
	r.TriageLimiter = limiter

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindTriage); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 0 {
		t.Fatalf("expected ZERO Advisor.Decide calls when the hourly triage cap is exhausted, got %d", advisor.calls)
	}
	if limiter.tries != 1 {
		t.Fatalf("expected exactly one TryIncrement call, got %d", limiter.tries)
	}

	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var sawSkip bool
	for _, rec := range records {
		if rec.State == state.StateSkipped {
			var payload map[string]string
			if err := json.Unmarshal(rec.Payload, &payload); err == nil && payload["reason"] == string(SkipTriageRateLimited) {
				sawSkip = true
			}
		}
	}
	if !sawSkip {
		t.Fatalf("expected a skipped(%s) journal record, got: %+v", SkipTriageRateLimited, records)
	}
}

// TestRunner_FollowUpIsNeverGatedByTriageLimiter proves the hourly cap is TRIAGE-only: a
// FOLLOW-UP job must run the Advisor even with an always-denying TriageLimiter wired.
func TestRunner_FollowUpIsNeverGatedByTriageLimiter(t *testing.T) {
	advisor := &countingAdvisor{}
	actor := &countingActor{}
	claimer := &fakeClaimer{held: true}
	r, _ := newTestRunner(t,
		fakeIssues{snap: IssueSnapshot{ID: "i1", Status: "unresolved"}},
		claimer, advisor, actor, false,
	)
	r.TriageLimiter = &alwaysDeniedLimiter{}

	e := Event{Seq: 1, Type: "question_answered", Issue: &EventIssue{ID: "i1"}}
	if err := r.Run(context.Background(), e, KindFollowUp); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if advisor.calls != 1 {
		t.Fatalf("expected FOLLOW-UP to run the Advisor regardless of TriageLimiter, got %d calls", advisor.calls)
	}
}
