package jobs

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// recordingSweepServer captures every mutating request's method+path+body so tests can assert on
// exact wire calls (heartbeat text, nag/release ops) without re-parsing httptest internals per
// test.
type recordingSweepServer struct {
	mu       sync.Mutex
	requests []recordedReq
	// waitingIssues is served by GET /api/agent/issues.
	waitingIssues []map[string]interface{}
	// claimStatus lets a test force ClaimIssue's response (200 default, or 409 conflict).
	claimStatus int
}

type recordedReq struct {
	Method string
	Path   string
	Body   map[string]interface{}
}

func newRecordingSweepServer() *recordingSweepServer {
	return &recordingSweepServer{claimStatus: 200}
}

func (s *recordingSweepServer) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/issues", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]interface{}{"issues": s.waitingIssues})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		switch {
		case strings.HasSuffix(r.URL.Path, "/progress"):
			w.WriteHeader(200)
			w.Write([]byte(`{"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(200)
			w.Write([]byte(`{"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			w.WriteHeader(s.claimStatus)
			if s.claimStatus == http.StatusConflict {
				json.NewEncoder(w).Encode(map[string]interface{}{"claimedBy": "someone-else", "claimedAt": "2026-01-01T00:00:00Z"})
			} else {
				json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "alreadyClaimed": true})
			}
		case r.URL.Path == "/api/agent/batch":
			w.WriteHeader(200)
			w.Write([]byte(`{"completed":2,"results":[{"status":200},{"status":200}]}`))
		default:
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
		}
	})
	return httptest.NewServer(mux)
}

func (s *recordingSweepServer) record(r *http.Request) {
	var body map[string]interface{}
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		if len(data) > 0 {
			json.Unmarshal(data, &body)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, recordedReq{Method: r.Method, Path: r.URL.Path, Body: body})
}

func (s *recordingSweepServer) byPath(suffix string) []recordedReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedReq
	for _, req := range s.requests {
		if strings.HasSuffix(req.Path, suffix) {
			out = append(out, req)
		}
	}
	return out
}

type fixedHeldClaims struct{ claims []heldClaim }

func (f fixedHeldClaims) HeldClaims(_ context.Context) ([]heldClaim, error) { return f.claims, nil }

func newTestJournal(t *testing.T) *state.Journal {
	t.Helper()
	dir := t.TempDir()
	return state.OpenJournal(filepath.Join(dir, "jobs.journal"))
}

// --- heartbeat: text varies (dedup non-collision) -----------------------------------------------

func TestSweep_Heartbeat_TextVariesByTimestamp(t *testing.T) {
	srv := newRecordingSweepServer()
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	t1 := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 8, 19, 11, 5, 0, 0, time.UTC) // different minute -> different key & text

	s := &Sweep{Client: client, Heartbeat: time.Hour}
	claim := heldClaim{IssueID: "issue-1", LastActivity: t1.Add(-2 * time.Hour)}

	s.Now = func() time.Time { return t1 }
	if err := s.heartbeatOne(context.Background(), claim); err != nil {
		t.Fatalf("heartbeatOne #1: %v", err)
	}
	s.Now = func() time.Time { return t2 }
	if err := s.heartbeatOne(context.Background(), claim); err != nil {
		t.Fatalf("heartbeatOne #2: %v", err)
	}

	reqs := srv.byPath("/progress")
	if len(reqs) != 2 {
		t.Fatalf("got %d progress posts, want 2", len(reqs))
	}
	body1, _ := reqs[0].Body["message_md"].(string)
	body2, _ := reqs[1].Body["message_md"].(string)
	if body1 == body2 {
		t.Errorf("heartbeat text did not vary across timestamps: %q == %q", body1, body2)
	}
	key1, _ := reqs[0].Body["idempotency_key"].(string)
	key2, _ := reqs[1].Body["idempotency_key"].(string)
	if key1 == "" || key2 == "" || key1 == key2 {
		t.Errorf("idempotency keys did not vary: %q vs %q", key1, key2)
	}
}

func TestSweep_Heartbeat_SkippedWhenRecentlyActive(t *testing.T) {
	srv := newRecordingSweepServer()
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	s := &Sweep{Client: client, Heartbeat: 12 * time.Hour, Now: func() time.Time { return now }}
	claim := heldClaim{IssueID: "issue-1", LastActivity: now.Add(-time.Hour)} // well within heartbeat window

	if err := s.heartbeatOne(context.Background(), claim); err != nil {
		t.Fatalf("heartbeatOne: %v", err)
	}
	if reqs := srv.byPath("/progress"); len(reqs) != 0 {
		t.Errorf("got %d progress posts, want 0 (claim recently active)", len(reqs))
	}
}

// --- nag thresholds (injected clock) --------------------------------------------------------

func TestSweep_Nag_Thresholds(t *testing.T) {
	base := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	nagAfter := 3 * 24 * time.Hour
	nagRelease := 6 * 24 * time.Hour

	cases := []struct {
		name         string
		waitingSince time.Time
		wantNag      bool
		wantRelease  bool
	}{
		{"fresh", base.Add(-1 * time.Hour), false, false},
		{"past nagAfter", base.Add(-4 * 24 * time.Hour), true, false},
		{"past nagRelease", base.Add(-7 * 24 * time.Hour), false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRecordingSweepServer()
			srv.waitingIssues = []map[string]interface{}{
				{"id": "issue-1", "waitingOn": "reporter", "waitingSince": tc.waitingSince.Format(time.RFC3339)},
			}
			httpSrv := srv.server()
			defer httpSrv.Close()
			client := sentinel.NewClient(httpSrv.URL, "k")

			s := &Sweep{
				Client:     client,
				Execute:    true,
				NagAfter:   nagAfter,
				NagRelease: nagRelease,
				Now:        func() time.Time { return base },
			}
			res := s.Run(context.Background(), fixedHeldClaims{})

			if tc.wantNag && res.Nags != 1 {
				t.Errorf("Nags = %d, want 1", res.Nags)
			}
			if !tc.wantNag && res.Nags != 0 {
				t.Errorf("Nags = %d, want 0", res.Nags)
			}
			if tc.wantRelease && res.Releases != 1 {
				t.Errorf("Releases = %d, want 1", res.Releases)
			}
			if !tc.wantRelease && res.Releases != 0 {
				t.Errorf("Releases = %d, want 0", res.Releases)
			}
			if len(res.Errors) != 0 {
				t.Errorf("unexpected errors: %v", res.Errors)
			}
		})
	}
}

func TestSweep_ReleaseWithHandback_CommentBeforeRelease(t *testing.T) {
	srv := newRecordingSweepServer()
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	s := &Sweep{Client: client}
	if err := s.releaseWithHandback(context.Background(), "issue-1"); err != nil {
		t.Fatalf("releaseWithHandback: %v", err)
	}
	reqs := srv.byPath("/batch")
	if len(reqs) != 1 {
		t.Fatalf("got %d batch posts, want 1", len(reqs))
	}
	ops, _ := reqs[0].Body["operations"].([]interface{})
	if len(ops) != 2 {
		t.Fatalf("got %d ops, want 2 (comment, release)", len(ops))
	}
	first := ops[0].(map[string]interface{})
	second := ops[1].(map[string]interface{})
	if first["op"] != "issues.comment" {
		t.Errorf("ops[0].op = %v, want issues.comment", first["op"])
	}
	if second["op"] != "issues.claim.release" {
		t.Errorf("ops[1].op = %v, want issues.claim.release", second["op"])
	}
}

// --- reconcile: re-claims vs re-triages -----------------------------------------------------

func TestSweep_ReconcileReaped_ReclaimsWhenOpenQuestion(t *testing.T) {
	srv := newRecordingSweepServer()
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	j := newTestJournal(t)
	if err := j.Append(state.Record{JobID: "job-1", IssueID: "issue-1", Kind: "followup", State: state.StateQuestioned}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	s := &Sweep{Client: client, Journal: j, Execute: true}
	reclaimed, err := s.ReconcileReaped(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("ReconcileReaped: %v", err)
	}
	if !reclaimed {
		t.Errorf("reclaimed = false, want true (open question)")
	}
	if reqs := srv.byPath("/claim"); len(reqs) != 1 {
		t.Errorf("got %d claim posts, want 1", len(reqs))
	}
}

func TestSweep_ReconcileReaped_DoesNotReclaimWithoutOpenState(t *testing.T) {
	srv := newRecordingSweepServer()
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	j := newTestJournal(t)
	// A healthy, intentional release (e.g. needs_human's terminal comment_only path) leaves no
	// open question and no open fix -- reconcile must NOT re-claim it.
	if err := j.Append(state.Record{JobID: "job-1", IssueID: "issue-1", Kind: "triage", State: state.StateDone}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	s := &Sweep{Client: client, Journal: j, Execute: true}
	reclaimed, err := s.ReconcileReaped(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("ReconcileReaped: %v", err)
	}
	if reclaimed {
		t.Errorf("reclaimed = true, want false (no open question/fix)")
	}
	if reqs := srv.byPath("/claim"); len(reqs) != 0 {
		t.Errorf("got %d claim posts, want 0", len(reqs))
	}
}

func TestSweep_ReconcileReaped_ForeignClaimBeatsUs(t *testing.T) {
	srv := newRecordingSweepServer()
	srv.claimStatus = http.StatusConflict
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	j := newTestJournal(t)
	if err := j.Append(state.Record{JobID: "job-1", IssueID: "issue-1", Kind: "followup", State: state.StateQuestioned}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	s := &Sweep{Client: client, Journal: j, Execute: true}
	reclaimed, err := s.ReconcileReaped(context.Background(), "issue-1")
	if err != nil {
		t.Fatalf("ReconcileReaped: %v", err)
	}
	if reclaimed {
		t.Errorf("reclaimed = true, want false (foreign claimant)")
	}
}

// --- startup rejects heartbeat >= stale ------------------------------------------------------

func TestValidateHeartbeatBelowStale(t *testing.T) {
	if err := ValidateHeartbeatBelowStale(12*time.Hour, 24); err != nil {
		t.Errorf("12h < 24h stale: got error %v, want nil", err)
	}
	if err := ValidateHeartbeatBelowStale(24*time.Hour, 24); err == nil {
		t.Errorf("24h heartbeat == 24h stale: want error, got nil")
	}
	if err := ValidateHeartbeatBelowStale(30*time.Hour, 24); err == nil {
		t.Errorf("30h heartbeat > 24h stale: want error, got nil")
	}
	if err := ValidateHeartbeatBelowStale(12*time.Hour, 0); err != nil {
		t.Errorf("stale unknown (0): want nil, got %v", err)
	}
}

// --- FixPRStatusHook seam is invoked per held claim -------------------------------------------

func TestSweep_Run_InvokesFixPRStatusHookPerHeldClaim(t *testing.T) {
	srv := newRecordingSweepServer()
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	var hooked []string
	s := &Sweep{
		Client:          client,
		Execute:         true,
		Now:             func() time.Time { return now },
		FixPRStatusHook: func(_ context.Context, issueID string) { hooked = append(hooked, issueID) },
	}
	claims := fixedHeldClaims{claims: []heldClaim{
		{IssueID: "issue-1", LastActivity: now},
		{IssueID: "issue-2", LastActivity: now},
	}}
	s.Run(context.Background(), claims)
	if len(hooked) != 2 {
		t.Fatalf("hooked = %v, want 2 entries", hooked)
	}
}

// --- SWEEP MINOR 1: Execute gates Run/ReconcileReaped, matching runner.DryRun's contract --------

// TestSweep_DryRun_SendsNothing proves a dry-run sweep (Execute: false, the zero value) posts
// zero requests even with held claims, waiting issues, and a re-claimable open question all
// present -- mirroring plan §5's "dry-run must send NOTHING" for the sweep loop, not just the
// per-job claim/act path.
func TestSweep_DryRun_SendsNothing(t *testing.T) {
	srv := newRecordingSweepServer()
	srv.waitingIssues = []map[string]interface{}{
		{"id": "issue-2", "waitingOn": "reporter", "waitingSince": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)},
	}
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	j := newTestJournal(t)
	if err := j.Append(state.Record{JobID: "job-1", IssueID: "issue-3", Kind: "followup", State: state.StateQuestioned}); err != nil {
		t.Fatalf("journal append: %v", err)
	}

	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	s := &Sweep{
		Client:  client,
		Journal: j,
		Execute: false, // dry-run
		Now:     func() time.Time { return now },
	}
	claims := fixedHeldClaims{claims: []heldClaim{{IssueID: "issue-1", LastActivity: now.Add(-48 * time.Hour)}}}

	res := s.Run(context.Background(), claims)
	if res.Heartbeats != 0 || res.Nags != 0 || res.Releases != 0 || res.Reconciled != 0 {
		t.Fatalf("dry-run Run() result must be all-zero, got %+v", res)
	}

	reclaimed, err := s.ReconcileReaped(context.Background(), "issue-3")
	if err != nil {
		t.Fatalf("ReconcileReaped: %v", err)
	}
	if reclaimed {
		t.Error("dry-run ReconcileReaped must never report reclaimed=true")
	}

	if len(srv.requests) != 0 {
		t.Fatalf("dry-run sweep must issue zero requests, got %d: %+v", len(srv.requests), srv.requests)
	}
}

// --- SWEEP MINOR 2: releaseWithHandback classifies per-op results (C3), not just the envelope ----

// TestSweep_ReleaseWithHandback_FailedReleaseOpSurfacesAsError proves a batch envelope that is
// HTTP 200 overall but whose issues.claim.release op failed (C3: per-op outcomes live in
// results[]) is reported as an error, not a clean release -- the bug being fixed is
// releaseWithHandback previously checked only res.Status, so a lost release op was silently
// reported as success.
func TestSweep_ReleaseWithHandback_FailedReleaseOpSurfacesAsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/batch", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		// op0 (issues.comment) succeeds; op1 (issues.claim.release) fails permanently.
		w.Write([]byte(`{"completed":2,"results":[{"status":201},{"status":400}]}`))
	})
	httpSrv := httptest.NewServer(mux)
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	s := &Sweep{Client: client}
	err := s.releaseWithHandback(context.Background(), "issue-1")
	if err == nil {
		t.Fatal("releaseWithHandback: got nil error, want an error for a failed release op inside a 200 envelope")
	}
	if !strings.Contains(err.Error(), "issues.claim.release") {
		t.Errorf("error %q should name the failing op (issues.claim.release)", err.Error())
	}
}

// --- SWEEP MINOR 3: nag idempotency key is per-episode (issueID + waitingSince) ------------------

// TestSweep_Nag_KeyVariesByWaitingEpisode proves two distinct waiting episodes on the SAME issue
// (a first question, answered, then a second question asked later with a new waitingSince)
// produce distinct nag idempotency keys -- a fixed 'sweep-nag:<issueID>:0' key would make the
// second episode's nag dedupe away as a "retry" of the first, so the reporter never gets reminded
// about the second question.
func TestSweep_Nag_KeyVariesByWaitingEpisode(t *testing.T) {
	srv := newRecordingSweepServer()
	httpSrv := srv.server()
	defer httpSrv.Close()
	client := sentinel.NewClient(httpSrv.URL, "k")

	s := &Sweep{Client: client}
	episode1 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	episode2 := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)

	if err := s.nagOne(context.Background(), "issue-1", episode1); err != nil {
		t.Fatalf("nagOne #1: %v", err)
	}
	if err := s.nagOne(context.Background(), "issue-1", episode2); err != nil {
		t.Fatalf("nagOne #2: %v", err)
	}

	reqs := srv.byPath("/comments")
	if len(reqs) != 2 {
		t.Fatalf("got %d comment posts, want 2", len(reqs))
	}
	key1, _ := reqs[0].Body["idempotency_key"].(string)
	key2, _ := reqs[1].Body["idempotency_key"].(string)
	if key1 == "" || key2 == "" || key1 == key2 {
		t.Errorf("nag idempotency keys did not vary across waiting episodes: %q vs %q", key1, key2)
	}
}
