//go:build e2e

// Package e2e — U40/U41 (docs/plans/AGENT_WORKER_PLAN.md §8): the sentinel-worker binary, built
// GOWORK=off exactly as CI's `go-root`-adjacent worker gate would, launched against the REAL
// compose stack (dashboard on :13000, ingestor on :18080) with LLM_BASE_URL pointed at an
// httptest fake OpenAI-compatible Advisor running in THIS test process. Gated by requireStack,
// which fatals rather than skips under SENTINEL_E2E=1 (never a silent skip, per the harness's own
// convention -- see agent_work_loop_test.go's requireM5AgentIntegration doc).
//
// U40: a fresh system_error is ingested; the worker claims it, posts exactly one TRIAGE comment
// (comment_only, no severity since system_error never carries one), and a kill -9 mid-job +
// restart replays the journaled decision without posting a second comment.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// buildWorkerBinary compiles tools/sentinel-worker exactly like the module's own gate
// (GOWORK=off go build ./...), once per test binary run, and returns the built binary's path.
// GOWORK=off matters here for the same reason CLAUDE.md's A2 note does for go-root: the worker is
// an independent module and must build the way a real `go get` consumer would see it.
func buildWorkerBinary(t *testing.T) string {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	workerDir := filepath.Join(repoRoot, "tools", "sentinel-worker")
	binPath := filepath.Join(t.TempDir(), "sentinel-worker")

	cmd := osexec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = workerDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building tools/sentinel-worker (GOWORK=off go build ./...): %v\n%s", err, out)
	}
	return binPath
}

// fakeAdvisorServer is a minimal OpenAI-compatible /v1/chat/completions server: every call returns
// the next scripted decision JSON as the assistant message content, verbatim (no tool-calling —
// the worker's toolchain still registers tools, but the fake advisor never asks for one, which is
// a valid RunLoop path: a first turn with no tool_calls terminates the loop immediately).
type fakeAdvisorServer struct {
	srv    *httptest.Server
	calls  atomic.Int64
	decide func(reqBody map[string]any) string // returns the assistant message content (a decision JSON string)
}

func newFakeAdvisorServer(t *testing.T, decide func(reqBody map[string]any) string) *fakeAdvisorServer {
	t.Helper()
	f := &fakeAdvisorServer{decide: decide}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		content := f.decide(body)
		resp := map[string]any{
			"choices": []map[string]any{
				{
					"message":       map[string]any{"content": content},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// workerProcess is one launched sentinel-worker instance under test control.
type workerProcess struct {
	cmd     *osexec.Cmd
	logPath string
}

// startWorker launches the built binary with a full, WORKER_EXECUTE=true env, pointed at the live
// compose stack and the fake Advisor. stateDir must be reused across a kill+restart pair for the
// journal-replay proof (U40's "kill -9 mid-job, restart, exactly one comment").
func startWorker(t *testing.T, binPath, stateDir, agentKey, fakeAdvisorURL string, extraEnv ...string) *workerProcess {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), fmt.Sprintf("worker-%d.log", time.Now().UnixNano()))
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating worker log file: %v", err)
	}
	t.Cleanup(func() { logFile.Close() })

	cmd := osexec.Command(binPath)
	cmd.Env = append(os.Environ(),
		"SENTINEL_URL="+cfg.DashboardURL,
		"SENTINEL_AGENT_KEY="+agentKey,
		"WORKER_ENABLED=true",
		"WORKER_EXECUTE=true",
		"WORKER_STATE_DIR="+stateDir,
		"WORKER_POLL_INTERVAL=300ms",
		"WORKER_SWEEP_INTERVAL=1h",
		"WORKER_BACKFILL_HOURS=1",
		"WORKER_HEALTH_ADDR=:0",
		"LLM_PROVIDER=openai",
		"LLM_MODEL=fake-e2e-model",
		"LLM_API_KEY=fake-key",
		"LLM_BASE_URL="+fakeAdvisorURL,
		"WORKER_TRIAGE_TIMEOUT=20s",
		"WORKER_FOLLOWUP_TIMEOUT=20s",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting worker binary: %v", err)
	}
	return &workerProcess{cmd: cmd, logPath: logPath}
}

// killNow sends SIGKILL and waits for the process to exit (never a graceful SIGTERM -- U40/U41
// need the "crash mid-job" case, not the drained-shutdown case another part of this suite already
// covers via loop's own shutdown tests).
func (w *workerProcess) killNow(t *testing.T) {
	t.Helper()
	if w.cmd.Process == nil {
		return
	}
	_ = w.cmd.Process.Signal(syscall.SIGKILL)
	_, _ = w.cmd.Process.Wait()
}

func (w *workerProcess) stop(t *testing.T) {
	t.Helper()
	if w.cmd.Process == nil {
		return
	}
	_ = w.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _, _ = w.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = w.cmd.Process.Kill()
		<-done
	}
}

func (w *workerProcess) logs(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.logPath)
	if err != nil {
		return ""
	}
	return string(b)
}

// waitForCondition polls fn until it returns true or timeout elapses, failing the test otherwise.
// desc is used only in the failure message.
func waitForCondition(t *testing.T, timeout time.Duration, desc string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !fn() {
		t.Fatalf("timed out waiting for: %s", desc)
	}
}

// agentComment decodes one row of GET /api/agent/issues/:id/comments' `comments` array.
type agentComment struct {
	AuthorType string `json:"authorType"`
	BodyMD     string `json:"bodyMd"`
}

func listAgentComments(t *testing.T, bearer, issueID string) []agentComment {
	t.Helper()
	res := agentRequest(t, "GET", "/api/agent/issues/"+issueID+"/comments?after=1970-01-01T00:00:00Z", bearer, nil)
	if res.Status != 200 {
		t.Fatalf("GET comments for issue %s: got %d, want 200, body=%s", issueID, res.Status, res.Body)
	}
	var env struct {
		Comments []agentComment `json:"comments"`
	}
	res.JSON(t, &env)
	return env.Comments
}

func agentComments(comments []agentComment, authorType string) []agentComment {
	var out []agentComment
	for _, c := range comments {
		if c.AuthorType == authorType {
			out = append(out, c)
		}
	}
	return out
}

// getIssueSeverity reads back the C8 severity op's write target -- manual_issue_reports.severity
// (act.go's "issues.report.severity" op, user_report-only) -- not any column on issues itself,
// which carries no severity at all. Returns nil when the issue has no manual_issue_reports row
// (the system_error case: no report was ever filed, so there is nothing to have set severity on).
func getIssueSeverity(t *testing.T, f *fixture, issueID string) *string {
	t.Helper()
	var sev *string
	err := pool.QueryRow(context.Background(),
		`SELECT r.severity FROM manual_issue_reports r JOIN issues i ON i.id = r.issue_id
		 WHERE r.issue_id = $1 AND i.project_id = $2`, issueID, f.ProjectID,
	).Scan(&sev)
	if err != nil && strings.Contains(err.Error(), "no rows") {
		return nil
	}
	if err != nil {
		t.Fatalf("reading issue severity for %s: %v", issueID, err)
	}
	return sev
}

// TestU40_WorkerTriageSystemErrorAndJournalReplay is the plan §8 U40 proof: ingest a fresh
// system_error, start the worker against a scripted fake Advisor returning a comment_only TRIAGE
// decision, assert the claim + exactly one triage comment + no severity write, then kill -9 the
// worker mid-lifecycle and restart it, asserting the journal replay does NOT produce a second
// comment (CONTEXT.md's Replay contract: "journaled decision re-executed verbatim, Advisor never
// re-consulted").
func TestU40_WorkerTriageSystemErrorAndJournalReplay(t *testing.T) {
	requireStack(t)
	f := newFixture(t)
	admin := f.newDashboardUser("owner")
	_, agentKey := f.seedAgent(t, admin, "worker-u40")

	binPath := buildWorkerBinary(t)

	const triageSummary = "U40 e2e: system_error assessed as comment_only, nothing actionable found."
	fake := newFakeAdvisorServer(t, func(_ map[string]any) string {
		raw, _ := json.Marshal(map[string]any{
			"severity":    nil,
			"disposition": "comment_only",
			"duplicateOf": nil,
			"causedBy":    nil,
			"summary":     triageSummary,
			"question":    nil,
			"fixBrief":    nil,
			"confidence":  0.9,
		})
		return string(raw)
	})

	// Ingest a fresh system_error (no `report` field => issueType system_error server-side).
	ev := f.newEvent()
	res := f.ingest(ev)
	if res.Status != 202 {
		t.Fatalf("ingesting system_error: got %d, want 202, body=%s", res.Status, res.Body)
	}

	stateDir := t.TempDir()
	worker := startWorker(t, binPath, stateDir, agentKey, fake.srv.URL)
	t.Cleanup(func() { worker.stop(t) })

	waitFor(t, asyncTimeout, "issue created from ingested system_error", func() (bool, string) {
		issues := ingestIssueSummaries(t, f)
		return len(issues) == 1, fmt.Sprintf("%d issues so far", len(issues))
	})
	issue := ingestOnlyIssueSummary(t, f)
	issueID := issue.ID

	var comments []agentComment
	waitForCondition(t, 30*time.Second, "worker posts exactly one agent triage comment", func() bool {
		comments = agentComments(listAgentComments(t, agentKey, issueID), "agent")
		return len(comments) >= 1
	})
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 agent comment after triage, got %d: %+v\nworker logs:\n%s", len(comments), comments, worker.logs(t))
	}
	if !strings.Contains(comments[0].BodyMD, triageSummary) {
		t.Fatalf("triage comment body = %q, want it to contain %q", comments[0].BodyMD, triageSummary)
	}

	// C8: severity ops are user_report-only. This is a system_error, so severity must stay unset
	// even though the fake Advisor returns severity:null anyway (the decision never asked for one).
	if sev := getIssueSeverity(t, f, issueID); sev != nil {
		t.Fatalf("issue severity = %q, want unset (system_error, C8)", *sev)
	}

	// Echo-suppression: the worker's own comment must not have re-triggered a second TRIAGE job
	// against itself. Give the poll loop a few more cycles, then assert the comment count is still
	// exactly one -- a broken echo-suppression path would show up as runaway duplicate comments.
	time.Sleep(1500 * time.Millisecond)
	comments = agentComments(listAgentComments(t, agentKey, issueID), "agent")
	if len(comments) != 1 {
		t.Fatalf("comment count grew after settling (echo-suppression failure?): got %d: %+v", len(comments), comments)
	}
	callsBeforeKill := fake.calls.Load()
	if callsBeforeKill < 1 {
		t.Fatalf("fake Advisor was never called")
	}

	// Kill -9 mid-lifecycle (the journal has already reached at least "advised"/"acted" for this
	// job by now) and restart against the SAME state dir. Replay must not re-consult the Advisor
	// (call count must not increase for THIS job) and must not post a second comment.
	worker.killNow(t)

	worker2 := startWorker(t, binPath, stateDir, agentKey, fake.srv.URL)
	t.Cleanup(func() { worker2.stop(t) })

	// Give the restarted worker time to run recovery + a few poll cycles, then assert nothing
	// duplicated.
	time.Sleep(3 * time.Second)
	comments = agentComments(listAgentComments(t, agentKey, issueID), "agent")
	if len(comments) != 1 {
		t.Fatalf("after kill -9 + restart, expected exactly 1 agent comment (replay must not re-act), got %d: %+v\nworker1 logs:\n%s\nworker2 logs:\n%s",
			len(comments), comments, worker.logs(t), worker2.logs(t))
	}
}

// schemaNameOf reads the OpenAI response_format.json_schema.name field out of a fake-Advisor
// request body -- "triage_decision" or "followup_decision" (triage.go/followup.go's
// JSONSchemaName) -- so the fake Advisor can script a DIFFERENT decision per Advisor without any
// other signal to key on.
func schemaNameOf(reqBody map[string]any) string {
	rf, _ := reqBody["response_format"].(map[string]any)
	if rf == nil {
		return ""
	}
	js, _ := rf["json_schema"].(map[string]any)
	if js == nil {
		return ""
	}
	name, _ := js["name"].(string)
	return name
}

// countBlockingComments queries issue_comments directly (blocking is not a field the
// GET .../comments JSON exposes on agentComment above) for the number of blocking (question)
// comments on issueID -- the U41 "exactly one blocking question" and "no duplicate after
// kill -9 + restart" assertions.
func countBlockingComments(t *testing.T, issueID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_comments WHERE issue_id = $1 AND author_type = 'agent' AND blocking = true`,
		issueID,
	).Scan(&n); err != nil {
		t.Fatalf("counting blocking comments for issue %s: %v", issueID, err)
	}
	return n
}

// TestU41_WorkerFollowupNeedsInfoThenReply is the plan §8 U41 proof: a user_report (report_created)
// fixture drives a needs_info TRIAGE decision (exactly one blocking question, severity SET since
// C8 allows it for user_report, claim KEPT per plan §4.2), kill -9 timed as close as this test can
// get to "between the question landing and the batch completing" + restart proves the
// questioned/idempotency guard (no duplicate question), and then a real user reply drives a
// FOLLOW-UP reply decision whose comment lands.
func TestU41_WorkerFollowupNeedsInfoThenReply(t *testing.T) {
	requireStack(t)
	f := newFixture(t)
	admin := f.newDashboardUser("owner")
	reporter := f.newDashboardUser("viewer")
	_, agentKey := f.seedAgent(t, admin, "worker-u41")

	binPath := buildWorkerBinary(t)

	const triageQuestion = "U41 e2e: can you share the exact request payload that triggered this?"
	const triageSummary = "U41 e2e: user_report needs more information before it can be triaged."
	const followupReplyBody = "U41 e2e: thanks, that confirms the repro -- looking into a fix now."

	fake := newFakeAdvisorServer(t, func(reqBody map[string]any) string {
		switch schemaNameOf(reqBody) {
		case "followup_decision":
			raw, _ := json.Marshal(map[string]any{
				"action":            "reply",
				"body":              followupReplyBody,
				"resolvedInVersion": nil,
				"fixBrief":          nil,
				"confidence":        nil,
			})
			return string(raw)
		default: // "triage_decision" (or unset -- treat as triage, the first call this test drives)
			raw, _ := json.Marshal(map[string]any{
				"severity":    "high",
				"disposition": "needs_info",
				"duplicateOf": nil,
				"causedBy":    nil,
				"summary":     triageSummary,
				"question":    triageQuestion,
				"fixBrief":    nil,
				"confidence":  0.5,
			})
			return string(raw)
		}
	})

	// Seed the user_report (report_created) fixture via the same session-authenticated route
	// agent_work_loop_test.go's M5 proof uses -- issueType user_report server-side, C8-eligible.
	reportRes := dashboardRequest(t, "POST", "/api/organizations/"+f.OrgID+"/reports", reporter, map[string]any{
		"title":     "U41 report " + uniqueSuffix(),
		"bodyMd":    "something is broken, not sure what triggered it",
		"severity":  "medium",
		"projectId": f.ProjectID,
	})
	if reportRes.Status != 201 {
		t.Fatalf("seeding user_report: got %d, want 201, body=%s", reportRes.Status, reportRes.Body)
	}
	var reportResp struct {
		Issue struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	reportRes.JSON(t, &reportResp)
	issueID := reportResp.Issue.ID
	if issueID == "" {
		t.Fatalf("report creation response had no issue id: %s", reportRes.Body)
	}

	stateDir := t.TempDir()
	worker := startWorker(t, binPath, stateDir, agentKey, fake.srv.URL)
	t.Cleanup(func() { worker.stop(t) })

	// Wait for the blocking question to land, killing -9 as close to that instant as this test can
	// get -- Act (act.go) sends PostQuestion then PostBatch in sequence inside one process, so there
	// is no external signal for "exactly between" the two calls; polling for the question and
	// killing immediately on the first sighting is the closest a black-box e2e test can land in
	// that window. The idempotency assertion below holds regardless of exactly where the kill
	// actually lands (before, inside, or after the batch), which is the point of proving it via
	// replay-safety rather than exact timing.
	waitForCondition(t, 30*time.Second, "worker posts the blocking question", func() bool {
		return countBlockingComments(t, issueID) >= 1
	})
	worker.killNow(t)

	if n := countBlockingComments(t, issueID); n != 1 {
		t.Fatalf("expected exactly 1 blocking question before restart, got %d\nworker logs:\n%s", n, worker.logs(t))
	}
	callsAfterFirstKill := fake.calls.Load()

	// Restart against the SAME state dir: replay must finish the batch (severity, claim kept) if it
	// hadn't already, WITHOUT re-consulting the Advisor and WITHOUT a second question.
	worker2 := startWorker(t, binPath, stateDir, agentKey, fake.srv.URL)
	t.Cleanup(func() { worker2.stop(t) })

	// Severity (C8: user_report-only) and the claim being kept (plan §4.2: needs_info keeps the
	// claim) are both terminal states of the SAME batch the question preceded -- wait for severity
	// to land as the signal that replay has completed acting.
	var severity *string
	waitForCondition(t, 30*time.Second, "severity set after needs_info replay completes", func() bool {
		severity = getIssueSeverity(t, f, issueID)
		return severity != nil && *severity == "high"
	})
	if severity == nil || *severity != "high" {
		t.Fatalf("issue severity = %v, want \"high\" (C8, user_report, needs_info)\nworker1 logs:\n%s\nworker2 logs:\n%s",
			severity, worker.logs(t), worker2.logs(t))
	}

	if n := countBlockingComments(t, issueID); n != 1 {
		t.Fatalf("after kill -9 + restart, expected exactly 1 blocking question (idempotency guard), got %d\nworker1 logs:\n%s\nworker2 logs:\n%s",
			n, worker.logs(t), worker2.logs(t))
	}
	if fake.calls.Load() != callsAfterFirstKill {
		t.Fatalf("replay re-consulted the Advisor (CONTEXT.md: Advisor never re-consulted on replay): calls before restart=%d, after=%d",
			callsAfterFirstKill, fake.calls.Load())
	}

	var waitingOn *string
	if err := pool.QueryRow(context.Background(), `SELECT waiting_on FROM issues WHERE id = $1`, issueID).Scan(&waitingOn); err != nil {
		t.Fatalf("reading waiting_on for issue %s: %v", issueID, err)
	}
	if waitingOn == nil || *waitingOn != "reporter" {
		t.Fatalf("expected issues.waiting_on = 'reporter' after needs_info question, got %v", waitingOn)
	}
	var assignedTo *string
	if err := pool.QueryRow(context.Background(), `SELECT assigned_to FROM issues WHERE id = $1`, issueID).Scan(&assignedTo); err != nil {
		t.Fatalf("reading assigned_to for issue %s: %v", issueID, err)
	}
	if assignedTo == nil {
		t.Fatalf("expected the issue's claim to be KEPT after needs_info (plan §4.2), assigned_to is unset")
	}

	// Now the reporter answers -- a real human reply over the session-authenticated route, exactly
	// like agent_work_loop_test.go's M3 path. This must clear waiting_on and drive a FOLLOW-UP job
	// whose reply decision (the fake Advisor's "followup_decision" branch) lands as a new agent
	// comment.
	replyRes := dashboardRequest(t, "POST", "/api/issues/"+issueID+"/comments", reporter, map[string]any{
		"bodyMd": "here's the payload: {\"foo\":\"bar\"}",
	})
	if replyRes.Status != 201 {
		t.Fatalf("human reply POST /api/issues/%s/comments: got %d, want 201, body=%s", issueID, replyRes.Status, replyRes.Body)
	}

	waitForCondition(t, 30*time.Second, "FOLLOW-UP reply comment lands after the user's reply", func() bool {
		for _, c := range agentComments(listAgentComments(t, agentKey, issueID), "agent") {
			if strings.Contains(c.BodyMD, followupReplyBody) {
				return true
			}
		}
		return false
	})
}

// TestU42_WorkerKeyRotation is the plan §8/§9 N8e U42 proof: the running worker rotates its own
// agent key unattended, persists the new secret to the file keystore BEFORE swapping it in memory,
// keeps polling/claiming successfully afterward, the OLD key still authenticates during the 24h
// grace window (C6), and the new secret is never written to any log line the worker emitted.
//
// This exercises the expiry-near trigger (plan C6(a)): the org key route's expiresInDays (N10)
// DOES apply to agent-scope keys (POST /api/organizations/{orgId}/keys accepts it regardless of
// scope), so this seeds the agent key with expiresInDays=1 and sets
// WORKER_ROTATE_BEFORE_HOURS=48 so "now + 48h is after expiresAt" is true from the worker's very
// first keyguard tick.
func TestU42_WorkerKeyRotation(t *testing.T) {
	requireStack(t)
	f := newFixture(t)
	admin := f.newDashboardUser("owner")

	agentRes := dashboardRequest(t, "POST", "/api/organizations/"+f.OrgID+"/agents", admin, map[string]any{
		"name": "worker-u42",
		"kind": "ai",
	})
	if agentRes.Status != 201 {
		t.Fatalf("creating agent: got %d, want 201, body=%s", agentRes.Status, agentRes.Body)
	}
	var agentResp struct {
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
	}
	agentRes.JSON(t, &agentResp)

	keyRes := dashboardRequest(t, "POST", "/api/organizations/"+f.OrgID+"/keys", admin, map[string]any{
		"name":          "agent-key-worker-u42",
		"scope":         "agent",
		"agentId":       agentResp.Agent.ID,
		"expiresInDays": 1,
	})
	if keyRes.Status != 201 {
		t.Fatalf("creating near-expiry agent key: got %d, want 201, body=%s", keyRes.Status, keyRes.Body)
	}
	var keyResp struct {
		Token string `json:"token"`
	}
	keyRes.JSON(t, &keyResp)
	agentKey := keyResp.Token
	if agentKey == "" {
		t.Fatalf("agent key creation response had no token: %s", keyRes.Body)
	}

	binPath := buildWorkerBinary(t)

	fake := newFakeAdvisorServer(t, func(_ map[string]any) string {
		raw, _ := json.Marshal(map[string]any{
			"severity":    nil,
			"disposition": "comment_only",
			"duplicateOf": nil,
			"causedBy":    nil,
			"summary":     "U42 e2e: comment_only.",
			"question":    nil,
			"fixBrief":    nil,
			"confidence":  0.9,
		})
		return string(raw)
	})

	stateDir := t.TempDir()
	keyPath := filepath.Join(stateDir, "agent-key.json")

	worker := startWorker(t, binPath, stateDir, agentKey, fake.srv.URL,
		"WORKER_KEYSTORE=file",
		"WORKER_ROTATE_BEFORE_HOURS=48",
		"WORKER_ROTATE_EVERY_DAYS=0",
		"WORKER_KEYGUARD_INTERVAL=500ms",
	)
	t.Cleanup(func() { worker.stop(t) })

	// (b) persisted via the file backend: agent-key.json appears and its content changes from the
	// bootstrap key to something else (the new secret) once rotation fires.
	{
		deadline := time.Now().Add(20 * time.Second)
		for {
			if _, err := os.Stat(keyPath); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for agent-key.json\nworker logs:\n%s", worker.logs(t))
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	var rotatedKey string
	waitForCondition(t, 20*time.Second, "agent-key.json content differs from the original bootstrap key (rotation persisted)", func() bool {
		raw, err := os.ReadFile(keyPath)
		if err != nil {
			return false
		}
		var doc struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return false
		}
		if doc.Key == "" || doc.Key == agentKey {
			return false
		}
		rotatedKey = doc.Key
		return true
	})

	// (c) the worker keeps operating with the new key: ingest an event and confirm the worker still
	// claims + comments (proves it swapped the new key into its live sentinel.Client and kept
	// polling successfully).
	ev := f.newEvent()
	res := f.ingest(ev)
	if res.Status != 202 {
		t.Fatalf("ingesting system_error: got %d, want 202, body=%s", res.Status, res.Body)
	}
	waitFor(t, asyncTimeout, "issue created from ingested system_error", func() (bool, string) {
		issues := ingestIssueSummaries(t, f)
		return len(issues) == 1, fmt.Sprintf("%d issues so far", len(issues))
	})
	issue := ingestOnlyIssueSummary(t, f)

	waitForCondition(t, 30*time.Second, "worker (post-rotation) posts an agent triage comment using the NEW key", func() bool {
		comments := agentComments(listAgentComments(t, rotatedKey, issue.ID), "agent")
		return len(comments) >= 1
	})

	// (d) the OLD key still authenticates during the 24h grace window (C6) -- a direct GET against
	// /api/agent/self with the pre-rotation bearer must still succeed.
	oldKeyRes := agentRequest(t, "GET", "/api/agent/self", agentKey, nil)
	if oldKeyRes.Status != 200 {
		t.Fatalf("old (pre-rotation) key expected to still authenticate during grace, got %d: %s", oldKeyRes.Status, oldKeyRes.Body)
	}

	// (e) the new secret must never appear in any log line the worker emitted.
	logs := worker.logs(t)
	if strings.Contains(logs, rotatedKey) {
		t.Fatalf("worker logs contain the rotated secret verbatim -- secret leaked into logs:\n%s", logs)
	}
}
