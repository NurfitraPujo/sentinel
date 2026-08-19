package jobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
			w.Write([]byte(`{"completed":true,"results":[{"status":200}]}`))
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
