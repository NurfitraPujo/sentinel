//go:build e2e

// Package e2e — M5 agent work-loop integration proof.
//
// Gated by stack availability through requireStack, which under SENTINEL_E2E=1 fatals rather than
// skips (see main_test.go's requireStack/skipNotPermitted): so the CI e2e job — which sets
// SENTINEL_E2E=1 and brings the compose stack up — runs this proof unconditionally. It previously
// gated on a separate M5_AGENT_INTEGRATION_REQUIRED=1 env that NO workflow ever set, so the proof
// skipped on every CI run while the job stayed green (the "recorded green, never executed" trap).
//
// Drives the REAL /api/agent/* surface over real HTTP against the compose dashboard (Bearer key
// auth needs no browser session, so this goes straight over HTTP — no server-module shortcut).
// Provisioning (org/project/agent/agent-key/dashboard users) uses direct DB seeding exactly like
// every other fixture in this package (harness_test.go's newFixture/newDashboardUser) — that is
// the harness's established pattern, not a shortcut invented for this test, and creating an
// org/agent through a live browser session is genuinely infeasible from a Go test.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

func requireM5AgentIntegration(t *testing.T) {
	t.Helper()
	// Under SENTINEL_E2E=1 requireStack fatals when the stack is down (never skips), so the agent
	// proof is mandatory in CI. There is no separate opt-in env: gating on one that no workflow set
	// is exactly how this coverage went dead. Locally without SENTINEL_E2E, an unavailable stack
	// still skips.
	requireStack(t)
}

// seedAgent creates an org agent + an active 'agent'-scope key via the SAME session-authenticated
// HTTP routes a real admin would use (POST /api/organizations/{orgId}/agents, then
// POST /api/organizations/{orgId}/keys with scope=agent), driven by an admin dashboardUser. Returns
// the agent id and the raw bearer secret.
func (f *fixture) seedAgent(t *testing.T, admin *dashboardUser, name string) (agentID, rawKey string) {
	t.Helper()

	res := dashboardRequest(t, "POST", "/api/organizations/"+f.OrgID+"/agents", admin, map[string]any{
		"name": name,
		"kind": "ai",
	})
	if res.Status != 201 {
		t.Fatalf("creating agent %q: got %d, want 201, body=%s", name, res.Status, res.Body)
	}
	var agentResp struct {
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
	}
	res.JSON(t, &agentResp)
	if agentResp.Agent.ID == "" {
		t.Fatalf("agent creation response had no id: %s", res.Body)
	}

	keyRes := dashboardRequest(t, "POST", "/api/organizations/"+f.OrgID+"/keys", admin, map[string]any{
		"name":    "agent-key-" + name,
		"scope":   "agent",
		"agentId": agentResp.Agent.ID,
	})
	if keyRes.Status != 201 {
		t.Fatalf("creating agent key for %q: got %d, want 201, body=%s", name, keyRes.Status, keyRes.Body)
	}
	var keyResp struct {
		Token string `json:"token"`
	}
	keyRes.JSON(t, &keyResp)
	if keyResp.Token == "" {
		t.Fatalf("agent key creation response had no token: %s", keyRes.Body)
	}

	return agentResp.Agent.ID, keyResp.Token
}

// agentRequest calls a dashboard /api/agent/* route with Bearer authentication over real HTTP.
func agentRequest(t *testing.T, method, path, bearer string, body any) dashboardResult {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshalling %s %s body: %v", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, cfg.DashboardURL+path, reader)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return dashboardResult{Status: resp.StatusCode, Body: string(raw)}
}

func TestM5AgentWorkLoopIntegration(t *testing.T) {
	requireM5AgentIntegration(t)

	ctx := context.Background()

	// ---- provision org1 (the tenant under test) + org2 (the isolation control) ----
	f1 := newFixture(t)
	f2 := newFixture(t)

	admin1 := f1.newDashboardUser("admin")
	reporter1 := f1.newDashboardUser("viewer")
	admin2 := f2.newDashboardUser("admin")

	agentID, bearer := f1.seedAgent(t, admin1, "worker-1")
	if agentID == "" || bearer == "" {
		t.Fatalf("agent provisioning did not return usable id/key")
	}

	// ---- an issue that exists only in org2, to prove the agent's org1 key cannot see it ----
	otherOrgRes := dashboardRequest(t, "POST", "/api/organizations/"+f2.OrgID+"/reports", admin2, map[string]any{
		"title":    "org2-only report " + uniqueSuffix(),
		"bodyMd":   "must never be visible to an org1 agent key",
		"severity": "low",
	})
	if otherOrgRes.Status != 201 {
		t.Fatalf("seeding org2 report: got %d, want 201, body=%s", otherOrgRes.Status, otherOrgRes.Body)
	}

	// ---- the org1 report the agent will actually work ----
	reportRes := dashboardRequest(t, "POST", "/api/organizations/"+f1.OrgID+"/reports", reporter1, map[string]any{
		"title":     "agent work-loop target " + uniqueSuffix(),
		"bodyMd":    "please investigate",
		"severity":  "high",
		"projectId": f1.ProjectID,
	})
	if reportRes.Status != 201 {
		t.Fatalf("seeding org1 report: got %d, want 201, body=%s", reportRes.Status, reportRes.Body)
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

	// ---- 1. Pull: agent lists open issues, sees only its own org ----
	listRes := agentRequest(t, "GET", "/api/agent/issues?status=unresolved&claimed=false", bearer, nil)
	if listRes.Status != 200 {
		t.Fatalf("GET /api/agent/issues: got %d, want 200, body=%s", listRes.Status, listRes.Body)
	}
	var listResp struct {
		Issues []struct {
			ID string `json:"id"`
		} `json:"issues"`
	}
	listRes.JSON(t, &listResp)

	sawTarget := false
	for _, row := range listResp.Issues {
		if row.ID == issueID {
			sawTarget = true
		}
	}
	if !sawTarget {
		t.Fatalf("agent issue list did not include the seeded org1 target issue %s: %s", issueID, listRes.Body)
	}
	for _, row := range listResp.Issues {
		var owner string
		if err := pool.QueryRow(ctx,
			`SELECT p.organization_id::text FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.id = $1`,
			row.ID,
		).Scan(&owner); err != nil {
			t.Fatalf("resolving org for listed issue %s: %v", row.ID, err)
		}
		if owner != f1.OrgID {
			t.Fatalf("agent issue list leaked an issue (%s) belonging to org %s, not the key's own org %s", row.ID, owner, f1.OrgID)
		}
	}

	// ---- 2. Claim ----
	claimRes := agentRequest(t, "POST", "/api/agent/issues/"+issueID+"/claim", bearer, nil)
	if claimRes.Status != 200 {
		t.Fatalf("POST claim: got %d, want 200, body=%s", claimRes.Status, claimRes.Body)
	}

	// ---- 3. Concurrency: two simultaneous claim attempts on a FRESH issue by the SAME agent ----
	// N9 (contract correction C1, idempotent self-reclaim): both requests use the same agent key, so
	// the loser of the conditional-UPDATE race (`WHERE assigned_to IS NULL`) re-reads, finds ITSELF
	// the current claimant, and returns 200 with `alreadyClaimed: true` rather than a 409 -- a 409 is
	// reserved for a DIFFERENT holder. The race therefore resolves to exactly one fresh claim + one
	// idempotent self-reclaim (both 200), which is what this asserts.
	concRes := dashboardRequest(t, "POST", "/api/organizations/"+f1.OrgID+"/reports", reporter1, map[string]any{
		"title":     "concurrency target " + uniqueSuffix(),
		"bodyMd":    "race the claim",
		"severity":  "medium",
		"projectId": f1.ProjectID,
	})
	if concRes.Status != 201 {
		t.Fatalf("seeding concurrency-target report: got %d, want 201, body=%s", concRes.Status, concRes.Body)
	}
	var concResp struct {
		Issue struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	concRes.JSON(t, &concResp)
	concurrencyIssueID := concResp.Issue.ID

	var wg sync.WaitGroup
	statuses := make([]int, 2)
	bodies := make([]string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			r := agentRequest(t, "POST", "/api/agent/issues/"+concurrencyIssueID+"/claim", bearer, nil)
			statuses[idx] = r.Status
			bodies[idx] = r.Body
		}(i)
	}
	wg.Wait()

	// Both must be 200 (C1); exactly one carries alreadyClaimed:true (the self-reclaim), the other
	// is the fresh claim. A 409 here would mean the self-reclaim guard regressed.
	var freshClaims, selfReclaims int
	for i, s := range statuses {
		if s != 200 {
			t.Fatalf("concurrent claim: got statuses %v, want both 200 (one fresh, one self-reclaim); body[%d]=%s", statuses, i, bodies[i])
		}
		var parsed struct {
			AlreadyClaimed bool `json:"alreadyClaimed"`
		}
		if err := json.Unmarshal([]byte(bodies[i]), &parsed); err != nil {
			t.Fatalf("concurrent claim: response %d not JSON: %v (body=%s)", i, err, bodies[i])
		}
		if parsed.AlreadyClaimed {
			selfReclaims++
		} else {
			freshClaims++
		}
	}
	if freshClaims != 1 || selfReclaims != 1 {
		t.Fatalf("concurrent claim: want exactly one fresh claim and one self-reclaim, got fresh=%d reclaim=%d statuses=%v", freshClaims, selfReclaims, statuses)
	}

	// ---- 4. Progress update: activity row, in-app only (no email kind on 'progress_update') ----
	progressRes := agentRequest(t, "POST", "/api/agent/issues/"+issueID+"/progress", bearer, map[string]any{
		"message_md": "looking into it",
	})
	if progressRes.Status != 201 {
		t.Fatalf("POST progress: got %d, want 201, body=%s", progressRes.Status, progressRes.Body)
	}
	var progressActivityCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM issue_activity WHERE issue_id = $1 AND event_type = 'progress_update' AND actor_type = 'agent' AND actor_id = $2`,
		issueID, agentID,
	).Scan(&progressActivityCount); err != nil {
		t.Fatalf("counting progress_update activity: %v", err)
	}
	if progressActivityCount < 1 {
		t.Fatalf("expected at least one progress_update issue_activity row, got %d", progressActivityCount)
	}

	// ---- 5. Blocking question: comment blocking=true, waiting_on set, reporter notification 'question_asked' ----
	questionRes := agentRequest(t, "POST", "/api/agent/issues/"+issueID+"/questions", bearer, map[string]any{
		"body_md":  "can you confirm the affected version?",
		"audience": "reporter",
	})
	if questionRes.Status != 201 {
		t.Fatalf("POST questions: got %d, want 201, body=%s", questionRes.Status, questionRes.Body)
	}

	var waitingOn *string
	if err := pool.QueryRow(ctx, `SELECT waiting_on FROM issues WHERE id = $1`, issueID).Scan(&waitingOn); err != nil {
		t.Fatalf("reading waiting_on after blocking question: %v", err)
	}
	if waitingOn == nil || *waitingOn != "reporter" {
		t.Fatalf("expected issues.waiting_on = 'reporter' after blocking question, got %v", waitingOn)
	}

	var questionAskedCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE issue_id = $1 AND kind = 'question_asked' AND user_id = $2`,
		issueID, reporter1.ID,
	).Scan(&questionAskedCount); err != nil {
		t.Fatalf("counting question_asked notifications: %v", err)
	}
	if questionAskedCount < 1 {
		t.Fatalf("expected a question_asked notification for the reporter, got %d", questionAskedCount)
	}

	// ---- 6. Human reply (M3 path, real session over HTTP) clears waiting_on ----
	replyTimestamp := time.Now().UTC().Add(-time.Second) // 'after' cursor for the agent's poll, below
	replyRes := dashboardRequest(t, "POST", "/api/issues/"+issueID+"/comments", reporter1, map[string]any{
		"bodyMd": "confirmed: v2.3.1",
	})
	if replyRes.Status != 201 {
		t.Fatalf("human reply POST /api/issues/%s/comments: got %d, want 201, body=%s", issueID, replyRes.Status, replyRes.Body)
	}

	var waitingOnAfterReply *string
	if err := pool.QueryRow(ctx, `SELECT waiting_on FROM issues WHERE id = $1`, issueID).Scan(&waitingOnAfterReply); err != nil {
		t.Fatalf("reading waiting_on after human reply: %v", err)
	}
	if waitingOnAfterReply != nil {
		t.Fatalf("expected issues.waiting_on to be cleared after a human reply, still %v", *waitingOnAfterReply)
	}

	// ---- agent reads the reply via GET comments?after= (the pull-model poll) ----
	pollRes := agentRequest(t, "GET",
		"/api/agent/issues/"+issueID+"/comments?after="+replyTimestamp.Format(time.RFC3339Nano), bearer, nil)
	if pollRes.Status != 200 {
		t.Fatalf("GET agent comments?after=: got %d, want 200, body=%s", pollRes.Status, pollRes.Body)
	}
	var pollResp struct {
		Comments []struct {
			BodyMd     string `json:"bodyMd"`
			AuthorType string `json:"authorType"`
		} `json:"comments"`
	}
	pollRes.JSON(t, &pollResp)
	sawReply := false
	for _, c := range pollResp.Comments {
		if c.AuthorType == "user" && c.BodyMd == "confirmed: v2.3.1" {
			sawReply = true
		}
	}
	if !sawReply {
		t.Fatalf("agent poll of comments?after= did not surface the human reply: %s", pollRes.Body)
	}

	// ---- 7. Agent resolves ----
	resolveRes := agentRequest(t, "PATCH", "/api/agent/issues/"+issueID+"/status", bearer, map[string]any{
		"status": "resolved",
	})
	if resolveRes.Status != 200 {
		t.Fatalf("PATCH resolve: got %d, want 200, body=%s", resolveRes.Status, resolveRes.Body)
	}
	var resolvedByType *string
	if err := pool.QueryRow(ctx, `SELECT resolved_by_type FROM issues WHERE id = $1`, issueID).Scan(&resolvedByType); err != nil {
		t.Fatalf("reading resolved_by_type: %v", err)
	}
	if resolvedByType == nil || *resolvedByType != "agent" {
		t.Fatalf("expected issues.resolved_by_type = 'agent', got %v", resolvedByType)
	}

	// ---- 8. Auditability (Q5): every mutation above has a matching audit_logs row ----
	var auditCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE resource_id = $1 AND actor_id = $2`,
		issueID, agentID,
	).Scan(&auditCount); err != nil {
		t.Fatalf("counting audit_logs for issue %s / agent %s: %v", issueID, agentID, err)
	}
	// claim + progress + question_asked + status_changed = 4 mutations against this issueID.
	const wantAuditRows = 4
	if auditCount < wantAuditRows {
		t.Fatalf("expected at least %d audit_logs rows for issue %s / agent %s, got %d", wantAuditRows, issueID, agentID, auditCount)
	}
	var auditActions []string
	rows, err := pool.Query(ctx, `SELECT action FROM audit_logs WHERE resource_id = $1 AND actor_id = $2 ORDER BY created_at`, issueID, agentID)
	if err != nil {
		t.Fatalf("listing audit_logs actions: %v", err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatalf("scanning audit_logs action: %v", err)
		}
		auditActions = append(auditActions, a)
	}
	rows.Close()
	for _, want := range []string{"agent.issue.claimed", "agent.issue.progress_update", "agent.issue.question_asked", "agent.issue.status_changed"} {
		found := false
		for _, a := range auditActions {
			if a == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected an audit_logs row with action %q for issue %s, got actions %v", want, issueID, auditActions)
		}
	}

	// ---- 9. An ingest attempt with the agent key against the ingestor is REJECTED ----
	ingestBody, err := json.Marshal(map[string]any{
		"project_key":     f1.ProjectName,
		"platform":        "go",
		"environment":     "e2e",
		"message":         "should never be accepted",
		"error_class":     "ShouldNeverIngest",
		"trace_id":        "trace-" + uniqueSuffix(),
		"span_id":         "span-" + uniqueSuffix(),
		"stacktrace":      []map[string]any{{"file": "x.go", "line": 1, "function": "f", "in_app": true}},
		"metadata":        map[string]any{},
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
		"trace_flags":     0,
		"release_version": "1.0.0",
	})
	if err != nil {
		t.Fatalf("marshalling ingest body: %v", err)
	}
	ingestReq, err := http.NewRequest("POST", cfg.IngestorURL+"/ingest", bytes.NewReader(ingestBody))
	if err != nil {
		t.Fatalf("building ingest request: %v", err)
	}
	ingestReq.Header.Set("Content-Type", "application/json")
	ingestReq.Header.Set("X-API-Key", bearer)
	ingestResp, err := (&http.Client{Timeout: 10 * time.Second}).Do(ingestReq)
	if err != nil {
		t.Fatalf("POST %s/ingest with agent key: %v", cfg.IngestorURL, err)
	}
	defer ingestResp.Body.Close()
	if ingestResp.StatusCode != http.StatusForbidden && ingestResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the ingestor to REJECT an agent-scoped key (401/403), got %d", ingestResp.StatusCode)
	}
	t.Logf("ingestor correctly rejected the agent key with status %d", ingestResp.StatusCode)
}
