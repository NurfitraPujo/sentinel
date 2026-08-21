package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// batchBody builds a minimal POST /api/agent/batch response body (sentinel.BatchResponse) with
// one results[] entry per given status, matching the real wire shape retry.go's ClassifyBatch
// decodes (BatchOpResult: {ok, status, result?}).
func batchBody(t *testing.T, statuses ...int) []byte {
	t.Helper()
	type opResult struct {
		Ok     bool `json:"ok"`
		Status int  `json:"status"`
	}
	var resp struct {
		Results []opResult `json:"results"`
	}
	for _, s := range statuses {
		resp.Results = append(resp.Results, opResult{Ok: s >= 200 && s < 300, Status: s})
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshalling batch body: %v", err)
	}
	return raw
}

// TestCheckBatchResults_AllSuccess proves the happy path: every op 2xx -> nil error, matching a
// batch where the sever actually did everything the compiled decision asked for.
func TestCheckBatchResults_AllSuccess(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{
		{Op: "issues.comment"},
		{Op: "issues.claim.release"},
	}}
	bRes := &sentinel.Result{Status: 200, Body: batchBody(t, 201, 200)}
	if err := checkBatchResults(compiled, bRes); err != nil {
		t.Fatalf("checkBatchResults: %v, want nil", err)
	}
}

// TestCheckBatchResults_DroppableRelationConflict proves a relation-add 409 (already-exists/cycle,
// ClassConflictDroppable) is NOT a job failure -- it is the expected "drop that op" outcome plan
// §2.3 documents, not a lost write.
func TestCheckBatchResults_DroppableRelationConflict(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{
		{Op: "issues.comment"},
		{Op: "issues.relations.add"},
		{Op: "issues.claim.release"},
	}}
	bRes := &sentinel.Result{Status: 200, Body: batchBody(t, 201, 409, 200)}
	if err := checkBatchResults(compiled, bRes); err != nil {
		t.Fatalf("checkBatchResults: %v, want nil (relation 409 is droppable, not a failure)", err)
	}
}

// TestCheckBatchResults_LoadBearingCommentFailed is the validator's exact scenario: a load-bearing
// op (issues.comment) fails inside a 200-envelope batch (C3: "always HTTP 200, per-op outcomes in
// results[]"). Before this fix, RealActor.Act discarded bRes entirely and returned err == nil,
// which the runner would journal as StateActed/done -- recording a lost comment as a full success.
// checkBatchResults must surface this as an error so the caller does NOT journal success.
func TestCheckBatchResults_LoadBearingCommentFailed(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{
		{Op: "issues.comment"},
		{Op: "issues.claim.release"},
	}}
	// issues.comment (op 0) returns 500 (transient) -- not droppable, not success.
	bRes := &sentinel.Result{Status: 200, Body: batchBody(t, 500, 200)}
	err := checkBatchResults(compiled, bRes)
	if err == nil {
		t.Fatal("checkBatchResults: got nil error, want an error for a failed load-bearing op")
	}
	if !strings.Contains(err.Error(), "issues.comment") {
		t.Errorf("error %q should name the failing op (issues.comment)", err.Error())
	}
}

// TestCheckBatchResults_SeverityPermanentFailure proves a droppable-by-design op family
// (issues.report.severity) is NOT automatically excused: a 400 on it is ClassPermanent, not
// ClassConflictDroppable (only a relation 409 gets that pass), so it must still surface as a
// failure -- otherwise a C8 compile-time bug (severity op on a non-user_report issue) would be
// silently swallowed instead of failing loudly as retry.go's own doc comment says it should.
func TestCheckBatchResults_SeverityPermanentFailure(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{
		{Op: "issues.comment"},
		{Op: "issues.report.severity"},
		{Op: "issues.claim.release"},
	}}
	bRes := &sentinel.Result{Status: 200, Body: batchBody(t, 201, 400, 200)}
	err := checkBatchResults(compiled, bRes)
	if err == nil {
		t.Fatal("checkBatchResults: got nil error, want an error for a permanent severity-op failure")
	}
	if !strings.Contains(err.Error(), "issues.report.severity") {
		t.Errorf("error %q should name the failing op (issues.report.severity)", err.Error())
	}
}

// TestCheckBatchResults_ClaimConflictForeign proves a foreign-claim 409 on issues.claim.release
// (an edge case: the caller lost the claim before the release landed) is surfaced, not silently
// treated as success -- it is not ClassSuccess and not ClassConflictDroppable.
func TestCheckBatchResults_ClaimConflictForeign(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{
		{Op: "issues.comment"},
		{Op: "issues.claim.release"},
	}}
	bRes := &sentinel.Result{Status: 200, Body: batchBody(t, 201, 409)}
	err := checkBatchResults(compiled, bRes)
	if err == nil {
		t.Fatal("checkBatchResults: got nil error, want an error for a foreign-claim conflict on release")
	}
}

// TestCheckBatchResults_NonSuccessEnvelope_429IsTransient is finding 1's (BLOCKER) red-first proof:
// a non-2xx batch ENVELOPE (429 rate-limited) short-circuits sentinel.ClassifyBatch to
// (ClassRateLimited, nil, nil) -- perOp is nil, so the pre-fix loop over perOp did nothing and
// checkBatchResults returned nil, silently treating a rate-limited batch as a full success. The
// fix must surface an error wrapping a *sentinel.StatusError{Status:429} so the runner's
// classifyRunnerFailureClass -> sentinel.ClassifyEnvelope sees ClassRateLimited (retryable), not
// ClassSuccess.
//
// Mutation check: reverting checkBatchResults to discard `overall` (looping only over the always-
// nil perOp) makes this test's err==nil branch fire and fail immediately.
// TestCheckBatchResults_PerOpPermanentIsNotTransient proves the N8i-review minor fix: a per-op
// PERMANENT rejection (a 400 on issues.comment, ClassPermanent) must surface an error wrapping a
// *sentinel.StatusError whose status classifies TERMINAL — so the runner journals failed_permanent
// with ONE Act attempt, not spinning it through MaxInlaneRetries transient resends. Before the fix,
// the default per-op branch returned a bare error (no StatusError) which classifyRunnerFailureClass
// defaults to ClassTransient.
//
// Mutation check: dropping the &sentinel.StatusError wrap from checkBatchResults' default branch
// makes errors.As fail here.
func TestCheckBatchResults_PerOpPermanentIsNotTransient(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{{Op: "issues.comment"}}}
	// A 400 on issues.comment classifies ClassPermanent (not droppable, not gone, not conflict).
	bRes := &sentinel.Result{Status: 200, Body: batchBody(t, 400)}
	err := checkBatchResults(compiled, bRes)
	if err == nil {
		t.Fatal("checkBatchResults: got nil error for a permanent per-op failure, want an error")
	}
	var statusErr *sentinel.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v does not wrap a *sentinel.StatusError (would default to ClassTransient and retry 5x)", err)
	}
	class := sentinel.ClassifyEnvelope(statusErr.Status, false, false)
	if class == sentinel.ClassTransient || class == sentinel.ClassRateLimited {
		t.Errorf("a permanent per-op failure classified %v (retryable); want a terminal class so it is not retried", class)
	}
}

func TestCheckBatchResults_NonSuccessEnvelope_429IsTransient(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{{Op: "issues.comment"}}}
	bRes := &sentinel.Result{Status: 429, Body: []byte(`{"error":"rate limited"}`)}
	err := checkBatchResults(compiled, bRes)
	if err == nil {
		t.Fatal("checkBatchResults: got nil error for a 429 batch envelope, want an error (finding 1: this used to be silently treated as full success)")
	}
	var statusErr *sentinel.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v does not wrap a *sentinel.StatusError", err)
	}
	if statusErr.Status != 429 {
		t.Errorf("StatusError.Status = %d, want 429", statusErr.Status)
	}
	if class := sentinel.ClassifyEnvelope(statusErr.Status, false, false); class != sentinel.ClassRateLimited {
		t.Errorf("ClassifyEnvelope(429) = %v, want ClassRateLimited (so the runner retries)", class)
	}
}

// TestCheckBatchResults_NonSuccessEnvelope_401IsAuth is finding 1's 401 branch: an auth failure
// envelope must classify as ClassAuthFailure (terminal, not retried in-lane), not be swallowed as
// success either.
func TestCheckBatchResults_NonSuccessEnvelope_401IsAuth(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{{Op: "issues.comment"}}}
	bRes := &sentinel.Result{Status: 401, Body: []byte(`{"error":"unauthorized"}`)}
	err := checkBatchResults(compiled, bRes)
	if err == nil {
		t.Fatal("checkBatchResults: got nil error for a 401 batch envelope, want an error")
	}
	var statusErr *sentinel.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v does not wrap a *sentinel.StatusError", err)
	}
	if class := sentinel.ClassifyEnvelope(statusErr.Status, false, false); class != sentinel.ClassAuthFailure {
		t.Errorf("ClassifyEnvelope(401) = %v, want ClassAuthFailure", class)
	}
}

// TestCheckBatchResults_NonSuccessEnvelope_5xxIsTransient mirrors the 429 case for a 500, the
// other half of "401/429/5xx a silent full success" the finding names.
func TestCheckBatchResults_NonSuccessEnvelope_5xxIsTransient(t *testing.T) {
	compiled := Compiled{Ops: []sentinel.BatchOperation{{Op: "issues.comment"}}}
	bRes := &sentinel.Result{Status: 503, Body: []byte(`{"error":"unavailable"}`)}
	err := checkBatchResults(compiled, bRes)
	if err == nil {
		t.Fatal("checkBatchResults: got nil error for a 503 batch envelope, want an error")
	}
	var statusErr *sentinel.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v does not wrap a *sentinel.StatusError", err)
	}
	if class := sentinel.ClassifyEnvelope(statusErr.Status, false, false); class != sentinel.ClassTransient {
		t.Errorf("ClassifyEnvelope(503) = %v, want ClassTransient", class)
	}
}

// TestCheckBatchResults_NoOps proves an empty Ops list (Question-only decisions never build any
// batch ops) never indexes out of range and reports no failure.
func TestCheckBatchResults_NoOps(t *testing.T) {
	if err := checkBatchResults(Compiled{}, &sentinel.Result{Status: 200, Body: batchBody(t)}); err != nil {
		t.Fatalf("checkBatchResults on empty batch: %v, want nil", err)
	}
}

// --- MAJOR: Act journals StateQuestioned on a blocking question (plan §2.2) --------------------

// newActorTestServer fakes GET /api/agent/issues/:id, POST .../questions, POST .../claim, and
// POST /api/agent/batch, just enough for RealActor.Act (and Sweep.ReconcileReaped's re-claim) to
// run against a real *sentinel.Client rather than a hand-rolled Sender fake.
func newActorTestServer(t *testing.T, commentID string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/agent/issues/"):
			w.WriteHeader(200)
			w.Write([]byte(`{"issue":{"projectId":"project-1","issueType":"user_report"}}`))
		case strings.HasSuffix(r.URL.Path, "/questions"):
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"comment": map[string]interface{}{"id": commentID},
			})
		case strings.HasSuffix(r.URL.Path, "/claim"):
			w.WriteHeader(200)
			w.Write([]byte(`{"success":true}`))
		case r.URL.Path == "/api/agent/batch":
			w.WriteHeader(200)
			w.Write([]byte(`{"completed":1,"results":[{"status":200}]}`))
		default:
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		}
	})
	return httptest.NewServer(mux)
}

// TestRealActor_Act_NeedsInfo_JournalsStateQuestioned is the MAJOR's red-first proof: driving a
// real needs_info decision through RealActor.Act must append a StateQuestioned record carrying the
// commentId PostQuestion returned. Before the fix, Act never appended anything beyond what the
// runner itself journals (advised/acting/acted/...), so Journal.HasOpenQuestion could never see a
// StateQuestioned record for a job Act actually ran -- only for one a test hand-injected.
func TestRealActor_Act_NeedsInfo_JournalsStateQuestioned(t *testing.T) {
	commentID := "comment-123"
	srv := newActorTestServer(t, commentID)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "k")

	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	// The record loop.Runner would already have appended (at least StateClaimed/StateAdvised)
	// before ever calling Act for this job -- resolveJobRecord depends on it being present.
	if err := j.Append(state.Record{JobID: "job-1", IssueID: "issue-1", Kind: "triage", TriggerSeq: 7, State: state.StateAdvised}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	actor := RealActor{Client: client, Journal: j}
	raw, _ := json.Marshal(TriageDecision{
		Disposition: string(DispositionNeedsInfo),
		Summary:     "need more info",
		Question:    strp("what version?"),
	})
	if err := actor.Act(context.Background(), "job-1", Decision{Kind: "triage", Raw: raw}); err != nil {
		t.Fatalf("Act: %v", err)
	}

	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	rec, ok := latest["job-1"]
	if !ok {
		t.Fatal("no journal record for job-1 after Act")
	}
	if rec.State != state.StateQuestioned {
		t.Fatalf("latest journal state = %v, want StateQuestioned", rec.State)
	}
	if rec.TriggerSeq != 7 {
		t.Errorf("TriggerSeq = %d, want 7 (carried from the job's existing journal trail)", rec.TriggerSeq)
	}
	var payload questionedPayload
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatalf("decoding StateQuestioned payload: %v", err)
	}
	if payload.CommentID != commentID {
		t.Errorf("payload.commentId = %q, want %q", payload.CommentID, commentID)
	}
}

// TestRealActor_Act_NeedsInfo_HasOpenQuestion_SurvivesRunnerTail is finding-2's red-first proof on
// the REAL pipeline path, not a hand-seeded journal: it drives a real needs_info decision through
// RealActor.Act (as above) and then appends the SAME StateActed/StateDone tail loop.Runner always
// appends onto that identical jobID immediately after Act returns (see runner.go's post-Act
// journaling). Before the finding-2 fix, Journal.HasOpenQuestion checked "latest record per
// jobId", so this terminal tail always overwrote the StateQuestioned record and HasOpenQuestion
// returned false forever -- deleting the fix (reverting to latest-per-jobId) makes this test fail.
func TestRealActor_Act_NeedsInfo_HasOpenQuestion_SurvivesRunnerTail(t *testing.T) {
	commentID := "comment-456"
	srv := newActorTestServer(t, commentID)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "k")

	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	if err := j.Append(state.Record{JobID: "job-2", IssueID: "issue-2", Kind: "triage", TriggerSeq: 9, State: state.StateAdvised}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	actor := RealActor{Client: client, Journal: j}
	raw, _ := json.Marshal(TriageDecision{
		Disposition: string(DispositionNeedsInfo),
		Summary:     "need more info",
		Question:    strp("what version?"),
	})
	if err := actor.Act(context.Background(), "job-2", Decision{Kind: "triage", Raw: raw}); err != nil {
		t.Fatalf("Act: %v", err)
	}

	// Mirror exactly what loop.Runner does right after Act returns for the SAME jobID (runner.go):
	// journals StateActed then StateDone, driving the triage job to terminal.
	if err := j.Append(state.Record{JobID: "job-2", IssueID: "issue-2", Kind: "triage", TriggerSeq: 9, State: state.StateActed}); err != nil {
		t.Fatalf("journal append StateActed: %v", err)
	}
	if err := j.Append(state.Record{JobID: "job-2", IssueID: "issue-2", Kind: "triage", TriggerSeq: 9, State: state.StateDone}); err != nil {
		t.Fatalf("journal append StateDone: %v", err)
	}

	open, err := j.HasOpenQuestion("issue-2")
	if err != nil {
		t.Fatalf("HasOpenQuestion: %v", err)
	}
	if !open {
		t.Fatal("HasOpenQuestion(issue-2) = false, want true: the triage job's own post-Act Acted/Done tail must not close the question it just asked")
	}
}

// fakeFixer records the FixJobInput Dispatch was called with, standing in for jobs.FixRunner in
// tests that only care about what RealActor.Act hands the FIX engine, not what the FIX engine
// itself does with it.
type fakeFixer struct {
	dispatched []FixJobInput
}

func (f *fakeFixer) Dispatch(in FixJobInput) {
	f.dispatched = append(f.dispatched, in)
}

// fixReadySettings makes every project report FIX enabled -- the narrow ProjectFixSettings RealActor
// needs to let CompileFollowup's attemptFix branch set compiled.EnqueueFix.
type fixReadySettings struct{}

func (fixReadySettings) FixEnabled(string) (bool, bool) { return true, true }

// newActorTestServerFailingQuestion is newActorTestServer but with the questions route always
// returning questionStatus instead of 201 -- finding 3's fixture for driving a failed blocking
// question through the REAL RealActor.Act path (not just the act.go-level Act unit test).
func newActorTestServerFailingQuestion(t *testing.T, questionStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/agent/issues/"):
			w.WriteHeader(200)
			w.Write([]byte(`{"issue":{"projectId":"project-1","issueType":"user_report"}}`))
		case strings.HasSuffix(r.URL.Path, "/questions"):
			w.WriteHeader(questionStatus)
			w.Write([]byte(`{"error":"nope"}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			w.WriteHeader(200)
			w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		}
	})
	return httptest.NewServer(mux)
}

// TestRealActor_Act_NeedsInfo_QuestionFails_DoesNotJournalQuestioned is finding 3's full-wiring
// red-first proof: driving a needs_info decision through RealActor.Act against a server whose
// questions route 500s must (a) return a non-nil error from Act, and (b) NOT journal
// StateQuestioned -- before the fix, Act only checked PostQuestion's transport err (nil here), so
// it proceeded to journalQuestioned with an empty commentId, stranding the claim with no way to
// ever ask the reporter.
func TestRealActor_Act_NeedsInfo_QuestionFails_DoesNotJournalQuestioned(t *testing.T) {
	srv := newActorTestServerFailingQuestion(t, 500)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "k")

	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	if err := j.Append(state.Record{JobID: "job-qfail", IssueID: "issue-qfail", Kind: "triage", TriggerSeq: 5, State: state.StateAdvised}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	actor := RealActor{Client: client, Journal: j}
	raw, _ := json.Marshal(TriageDecision{
		Disposition: string(DispositionNeedsInfo),
		Summary:     "need more info",
		Question:    strp("what version?"),
	})
	err := actor.Act(context.Background(), "job-qfail", Decision{Kind: "triage", Raw: raw})
	if err == nil {
		t.Fatal("Act: got nil error for a 500 question response, want an error")
	}

	latest, lerr := j.LatestByJobID()
	if lerr != nil {
		t.Fatalf("LatestByJobID: %v", lerr)
	}
	rec, ok := latest["job-qfail"]
	if !ok {
		t.Fatal("no journal record for job-qfail after Act")
	}
	if rec.State == state.StateQuestioned {
		t.Fatalf("FINDING 3: a failed question POST must not journal StateQuestioned, got state %v", rec.State)
	}
}

// TestRealActor_Act_Replay_ResendsJournaledBatchVerbatim is finding 4's red-first proof: the
// FIRST Act call fails mid-batch (500, transient) after already journaling the compiled batch body
// into the StateActing record; a SECOND Act call for the SAME jobID (exactly what a runner-level
// in-lane retry, or a crash-restart Resume, drives) must replay that journaled batch VERBATIM
// rather than re-fetching the issue and recompiling. To prove recompiling would have produced a
// DIFFERENT body, the fake server flips the issue's issueType from "user_report" (first GET) to
// "system_error" (any later GET) between the two Act calls -- CompileTriage's severity op is only
// built when actx.isUserReport() (C8), so a fresh recompile against the second GET would DROP the
// severity op from the batch. The test asserts (a) GetIssue is called exactly once (never
// refetched on replay) and (b) the second PostBatch request still carries the severity op.
func TestRealActor_Act_Replay_ResendsJournaledBatchVerbatim(t *testing.T) {
	var getIssueCalls int32
	var batchCalls int32
	var batchRequests []sentinel.BatchRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/agent/issues/"):
			n := atomic.AddInt32(&getIssueCalls, 1)
			issueType := "user_report"
			if n > 1 {
				// If Act ever refetches on replay, this is what it would (wrongly) see.
				issueType = "system_error"
			}
			w.WriteHeader(200)
			w.Write([]byte(`{"issue":{"projectId":"project-1","issueType":"` + issueType + `"}}`))
		case r.URL.Path == "/api/agent/batch":
			n := atomic.AddInt32(&batchCalls, 1)
			var req sentinel.BatchRequest
			_ = json.Unmarshal(body, &req)
			batchRequests = append(batchRequests, req)
			if n == 1 {
				// First attempt: batch envelope itself fails transiently (finding 1's own 5xx path).
				w.WriteHeader(500)
				w.Write([]byte(`{"error":"boom"}`))
				return
			}
			w.WriteHeader(200)
			w.Write([]byte(`{"completed":2,"results":[{"status":201},{"status":200},{"status":200}]}`))
		default:
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "k")

	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	if err := j.Append(state.Record{JobID: "job-replay", IssueID: "issue-replay", Kind: "triage", TriggerSeq: 11, State: state.StateAdvised}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	actor := RealActor{Client: client, Journal: j}
	raw, _ := json.Marshal(TriageDecision{
		Disposition: string(DispositionCommentOnly),
		Summary:     "looking into it",
		Severity:    strp("high"),
	})
	decision := Decision{Kind: "triage", Raw: raw}

	// First attempt: fetches + compiles + journals the batch, then the batch POST itself fails.
	if err := actor.Act(context.Background(), "job-replay", decision); err == nil {
		t.Fatal("first Act: expected an error (transient 500 batch envelope), got nil")
	}

	// Second attempt (the retry/replay): must reuse the journaled compiled batch, NOT refetch.
	if err := actor.Act(context.Background(), "job-replay", decision); err != nil {
		t.Fatalf("second (replay) Act: %v", err)
	}

	if got := atomic.LoadInt32(&getIssueCalls); got != 1 {
		t.Fatalf("FINDING 4: GetIssue was called %d times, want exactly 1 (replay must not refetch the issue)", got)
	}
	if got := atomic.LoadInt32(&batchCalls); got != 2 {
		t.Fatalf("expected exactly 2 PostBatch calls (fail then retry), got %d", got)
	}
	if len(batchRequests) != 2 {
		t.Fatalf("expected 2 recorded batch requests, got %d", len(batchRequests))
	}
	first, _ := json.Marshal(batchRequests[0])
	second, _ := json.Marshal(batchRequests[1])
	if string(first) != string(second) {
		t.Fatalf("FINDING 4: replayed batch body differs from the original:\n first:  %s\n second: %s", first, second)
	}
	foundSeverity := false
	for _, op := range batchRequests[1].Operations {
		if op.Op == "issues.report.severity" {
			foundSeverity = true
		}
	}
	if !foundSeverity {
		t.Fatal("FINDING 4: replayed batch dropped the severity op -- it must have recompiled against the (wrong) second GET's issueType instead of replaying the journaled batch verbatim")
	}
}

// TestRealActor_Act_EnqueueFix_PopulatesFixJobInputIssueURL is finding 3's proof: before this fix,
// FixJobInput.IssueURL was never populated by RealActor.Act -- every dispatched FIX job carried an
// empty IssueURL, so BuildFixPRSpec's PR body/TASK.md fell back to the bare issueID instead of a
// clickable Sentinel issue link (plan §4.4 step 5). Driving Act through a real attempt_fix FOLLOW-UP
// decision (high confidence, FIX enabled) with SentinelURL configured must produce a non-empty
// IssueURL on the dispatched FixJobInput, and that URL must actually render into the PR body
// BuildFixPRSpec produces.
func TestRealActor_Act_EnqueueFix_PopulatesFixJobInputIssueURL(t *testing.T) {
	srv := newActorTestServer(t, "")
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "k")

	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	if err := j.Append(state.Record{JobID: "job-fix-1", IssueID: "issue-fix-1", Kind: "followup", TriggerSeq: 3, State: state.StateAdvised}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	fixer := &fakeFixer{}
	actor := RealActor{
		Client:      client,
		Journal:     j,
		Fix:         fixReadySettings{},
		Fixer:       fixer,
		SentinelURL: "https://sentinel.example.com/",
	}
	raw, _ := json.Marshal(FollowupDecision{
		Action:     string(ActionAttemptFix),
		Body:       "trying a fix",
		FixBrief:   strp("dereference guarded now"),
		Confidence: f64p(0.9),
	})
	if err := actor.Act(context.Background(), "job-fix-1", Decision{Kind: "followup", Raw: raw}); err != nil {
		t.Fatalf("Act: %v", err)
	}

	if len(fixer.dispatched) != 1 {
		t.Fatalf("expected exactly one Dispatch call, got %d", len(fixer.dispatched))
	}
	in := fixer.dispatched[0]
	if in.IssueID != "issue-fix-1" {
		t.Fatalf("IssueID = %q, want issue-fix-1", in.IssueID)
	}
	wantURL := "https://sentinel.example.com/issues/issue-fix-1"
	if in.IssueURL != wantURL {
		t.Fatalf("IssueURL = %q, want %q (finding 3: FixJobInput.IssueURL was never populated)", in.IssueURL, wantURL)
	}

	spec, err := BuildFixPRSpec(in.IssueID, in.IssueURL, "SomeError", in.FixBrief, "sentinel-fix/deadbeef", "main", nil, nil, 0)
	if err != nil {
		t.Fatalf("BuildFixPRSpec: %v", err)
	}
	if !strings.Contains(spec.Body, wantURL) {
		t.Fatalf("PR body does not render the issue URL: %q", spec.Body)
	}
}

// TestRealActor_Act_EnqueueFix_UsesDistinctJobID is finding 1's (BLOCKER) red-first proof, driven
// entirely through the real path: RealActor.Act (real Journal, real Dispatch call) -> the runner's
// own post-Act terminal write (state.StateDone under the PARENT jobID, exactly as loop.Runner
// drives every job after Act returns) -> the FIX engine's own journalFixRunning call (exactly what
// FixRunner.RunFix does at attempt start) using whatever jobID Act handed Fixer.Dispatch.
//
// Before the fix, Act dispatched FixJobInput{JobID: jobID} (the PARENT's own jobID). Once the
// runner then appends the parent's StateDone record, Journal.Append's terminal-guard (state/
// journal.go) silently drops ANY further non-terminal record for that same jobId -- so
// journalFixRunning's StateFixRunning write for the FIX attempt never persists. This test proves
// (a) the dispatched FixJobInput.JobID differs from the parent jobID, and (b) the FIX engine's
// journalFixRunning record survives being appended AFTER the parent's terminal StateDone record.
//
// Mutation check: revert the fixJobID derivation in actor.go back to `JobID: jobID` and this test
// goes red on both assertions -- the JobID-differs check fails immediately, and (if that were
// somehow bypassed) the FixRunning-record-survives check fails because Append's terminal-guard
// silently drops it.
func TestRealActor_Act_EnqueueFix_UsesDistinctJobID(t *testing.T) {
	srv := newActorTestServer(t, "")
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "k")

	journalPath := filepath.Join(t.TempDir(), "jobs.journal")
	j := state.OpenJournal(journalPath)
	const parentJobID = "parent-job-1"
	if err := j.Append(state.Record{JobID: parentJobID, IssueID: "issue-fix-1", Kind: "followup", TriggerSeq: 3, State: state.StateAdvised}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	fixer := &fakeFixer{}
	actor := RealActor{
		Client:      client,
		Journal:     j,
		Fix:         fixReadySettings{},
		Fixer:       fixer,
		SentinelURL: "https://sentinel.example.com/",
	}
	raw, _ := json.Marshal(FollowupDecision{
		Action:     string(ActionAttemptFix),
		Body:       "trying a fix",
		FixBrief:   strp("dereference guarded now"),
		Confidence: f64p(0.9),
	})
	if err := actor.Act(context.Background(), parentJobID, Decision{Kind: "followup", Raw: raw}); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if len(fixer.dispatched) != 1 {
		t.Fatalf("expected exactly one Dispatch call, got %d", len(fixer.dispatched))
	}
	in := fixer.dispatched[0]

	// (a) the FIX job's own jobID must differ from the parent's.
	if in.JobID == parentJobID {
		t.Fatalf("finding 1: FixJobInput.JobID (%q) reuses the parent jobID (%q) -- the runner will drive it terminal and drop every FIX journal record", in.JobID, parentJobID)
	}
	if in.JobID == "" {
		t.Fatalf("FixJobInput.JobID is empty")
	}

	// Exactly what loop.Runner does immediately after Act returns for the parent job.
	if err := j.Append(state.Record{JobID: parentJobID, IssueID: "issue-fix-1", Kind: "followup", TriggerSeq: 3, State: state.StateDone}); err != nil {
		t.Fatalf("journal append parent StateDone: %v", err)
	}

	// Exactly what FixRunner.RunFix/executeValidatePublish does at FIX attempt start.
	if err := journalFixRunning(j, in, "deadbeef", false); err != nil {
		t.Fatalf("journalFixRunning: %v", err)
	}

	latest, err := j.LatestByJobID()
	if err != nil {
		t.Fatalf("LatestByJobID: %v", err)
	}
	rec, ok := latest[in.JobID]
	if !ok {
		t.Fatalf("finding 1: no journal record at all for FIX jobID %q -- terminal-guard dropped it", in.JobID)
	}
	if rec.State != state.StateFixRunning {
		t.Fatalf("finding 1: latest record for FIX jobID %q is %v, want StateFixRunning (Journal.Append's terminal-guard for the parent's jobID silently dropped the FIX record)", in.JobID, rec.State)
	}

	// (b) FIX batch idempotency keys must not collide with the parent's own ops (0/1).
	fixKey0 := sentinel.IdempotencyKey(in.JobID, 0)
	parentKey0 := sentinel.IdempotencyKey(parentJobID, 0)
	if fixKey0 == parentKey0 {
		t.Fatalf("finding 1: FIX batch idempotency key %q collides with the parent job's op-0 key", fixKey0)
	}
}

// TestSweep_ReconcileReaped_UsesRealJournalPopulatedByAct is the MAJOR's live-reconcile proof: it
// drives Act through a needs_info decision (as above) so the journal's StateQuestioned record is
// produced by the SAME code path a real deployment runs -- not hand-injected -- and then proves
// ReconcileReaped re-claims off that real record. Mutation check: comment out the
// `a.journalQuestioned` call in RealActor.Act (or its call site) and this test goes red, because
// Journal.HasOpenQuestion then finds nothing for issue-1 and ReconcileReaped correctly declines to
// re-claim -- proving this test is not hand-fed a StateQuestioned record.
func TestSweep_ReconcileReaped_UsesRealJournalPopulatedByAct(t *testing.T) {
	commentID := "comment-456"
	srv := newActorTestServer(t, commentID)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "k")

	j := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	if err := j.Append(state.Record{JobID: "job-1", IssueID: "issue-1", Kind: "triage", TriggerSeq: 7, State: state.StateAdvised}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	actor := RealActor{Client: client, Journal: j}
	raw, _ := json.Marshal(TriageDecision{
		Disposition: string(DispositionNeedsInfo),
		Summary:     "need more info",
		Question:    strp("what version?"),
	})
	if err := actor.Act(context.Background(), "job-1", Decision{Kind: "triage", Raw: raw}); err != nil {
		t.Fatalf("Act: %v", err)
	}

	s := &Sweep{Client: client, Journal: j, Execute: true}
	reclaimed, err := s.ReconcileReaped(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("ReconcileReaped: %v", err)
	}
	if !reclaimed {
		t.Fatal("reclaimed = false, want true: ReconcileReaped should see the real StateQuestioned record Act just journaled")
	}
}
