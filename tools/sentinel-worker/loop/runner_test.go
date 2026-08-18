package loop

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
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
	held      bool
	claimedBy string
	err       error
	calls     int32
}

func (f *fakeClaimer) EnsureClaimed(_ context.Context, _ string) (bool, string, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.held, f.claimedBy, f.err
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
// must NOT be left stranded at "queued" (a non-terminal state invisible to /metrics and to crash
// recovery's IsDuplicate dedupe) -- it must land on a terminal failed(transient: <class>) record
// and fire OnOutcome exactly once, so the job is both recoverable-as-done-failing and counted.
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
	if !latest.State.IsTerminal() {
		t.Fatalf("expected the job to end in a terminal state after a transient GetIssue error, got %q (stranded non-terminal)", latest.State)
	}
	if latest.State != state.StateFailed {
		t.Fatalf("expected terminal state %q, got %q", state.StateFailed, latest.State)
	}

	found := false
	for _, o := range outcomes {
		if o == string(KindTriage)+":failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected OnOutcome(%q, \"failed\") to fire, got %v", KindTriage, outcomes)
	}
}
