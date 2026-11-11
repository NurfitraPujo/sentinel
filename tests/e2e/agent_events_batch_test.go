//go:build e2e

// Package e2e — N5b: e2e coverage for the agent-native events feed and batch endpoint (N1-N4).
//
// Drives the real /api/agent/events and /api/agent/batch HTTP routes over Bearer auth, exactly
// like agent_work_loop_test.go's M5 proof, reusing its fixture.seedAgent / agentRequest helpers.
package e2e

import (
	"context"
	"testing"
	"time"
)

// TestU37_AgentEventsFeedOrderingAndOrgIsolation drives one flow through the agent-native layer:
// claim -> comment -> status change, then polls GET /api/agent/events?after=0 until all three
// resulting events (claimed, commented, status_changed) are visible in strictly ascending seq
// order, respecting the events feed's 2-second lag guard (agent-events.ts's
// EVENTS_LAG_GUARD_INTERVAL). It also proves org isolation: a second org's agent key must never
// see org1's events.
func TestU37_AgentEventsFeedOrderingAndOrgIsolation(t *testing.T) {
	requireM5AgentIntegration(t)

	f1 := newFixture(t)
	f2 := newFixture(t)

	admin1 := f1.newDashboardUser("admin")
	reporter1 := f1.newDashboardUser("viewer")
	admin2 := f2.newDashboardUser("admin")

	_, bearer1 := f1.seedAgent(t, admin1, "events-worker-1")
	_, bearer2 := f2.seedAgent(t, admin2, "events-worker-2")

	reportRes := dashboardRequest(t, "POST", "/api/organizations/"+f1.OrgID+"/reports", reporter1, map[string]any{
		"title":     "events feed target " + uniqueSuffix(),
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

	// ---- drive claim -> comment -> status change through the agent-native single routes ----
	claimRes := agentRequest(t, "POST", "/api/agent/issues/"+issueID+"/claim", bearer1, nil)
	if claimRes.Status != 200 {
		t.Fatalf("POST claim: got %d, want 200, body=%s", claimRes.Status, claimRes.Body)
	}

	commentRes := agentRequest(t, "POST", "/api/agent/issues/"+issueID+"/comments", bearer1, map[string]any{
		"body_md": "leaving a note for the events feed test",
	})
	if commentRes.Status != 201 {
		t.Fatalf("POST comment: got %d, want 201, body=%s", commentRes.Status, commentRes.Body)
	}

	statusRes := agentRequest(t, "PATCH", "/api/agent/issues/"+issueID+"/status", bearer1, map[string]any{
		"status": "resolved",
	})
	if statusRes.Status != 200 {
		t.Fatalf("PATCH status: got %d, want 200, body=%s", statusRes.Status, statusRes.Body)
	}

	type feedEvent struct {
		Seq       int64  `json:"seq"`
		EventType string `json:"eventType"`
		Issue     struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	type feedResp struct {
		Events  []feedEvent `json:"events"`
		Cursor  int64       `json:"cursor"`
		HasMore bool        `json:"hasMore"`
	}

	wantTypes := map[string]bool{"claimed": false, "commented": false, "status_changed": false}
	var seenForIssue []feedEvent

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		pollRes := agentRequest(t, "GET", "/api/agent/events?after=0&limit=200", bearer1, nil)
		if pollRes.Status != 200 {
			t.Fatalf("GET /api/agent/events: got %d, want 200, body=%s", pollRes.Status, pollRes.Body)
		}
		var resp feedResp
		pollRes.JSON(t, &resp)

		seenForIssue = nil
		for k := range wantTypes {
			wantTypes[k] = false
		}
		for _, ev := range resp.Events {
			if ev.Issue.ID == issueID {
				seenForIssue = append(seenForIssue, ev)
				if _, ok := wantTypes[ev.EventType]; ok {
					wantTypes[ev.EventType] = true
				}
			}
		}

		allSeen := true
		for _, seen := range wantTypes {
			if !seen {
				allSeen = false
			}
		}
		if allSeen {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	for evType, seen := range wantTypes {
		if !seen {
			t.Fatalf("events feed never surfaced a %q event for issue %s within timeout; got %+v", evType, issueID, seenForIssue)
		}
	}

	// ---- ordering: seq must be strictly ascending across the events we saw for this issue ----
	for i := 1; i < len(seenForIssue); i++ {
		if seenForIssue[i].Seq <= seenForIssue[i-1].Seq {
			t.Fatalf("events feed not in strictly ascending seq order: %+v", seenForIssue)
		}
	}

	// ---- claimed must precede commented must precede status_changed, by seq ----
	var claimedSeq, commentedSeq, statusSeq int64 = -1, -1, -1
	for _, ev := range seenForIssue {
		switch ev.EventType {
		case "claimed":
			claimedSeq = ev.Seq
		case "commented":
			commentedSeq = ev.Seq
		case "status_changed":
			statusSeq = ev.Seq
		}
	}
	if !(claimedSeq < commentedSeq && commentedSeq < statusSeq) {
		t.Fatalf("expected claimed < commented < status_changed by seq, got claimed=%d commented=%d status_changed=%d",
			claimedSeq, commentedSeq, statusSeq)
	}

	// ---- cursor advances: after= the highest seq we saw returns nothing new for this issue ----
	maxSeq := seenForIssue[len(seenForIssue)-1].Seq
	for _, ev := range seenForIssue {
		if ev.Seq > maxSeq {
			maxSeq = ev.Seq
		}
	}
	cursorRes := agentRequest(t, "GET", "/api/agent/events?after="+itoa(maxSeq)+"&limit=200", bearer1, nil)
	if cursorRes.Status != 200 {
		t.Fatalf("GET /api/agent/events?after=<cursor>: got %d, want 200, body=%s", cursorRes.Status, cursorRes.Body)
	}
	var cursorResp feedResp
	cursorRes.JSON(t, &cursorResp)
	for _, ev := range cursorResp.Events {
		if ev.Issue.ID == issueID {
			t.Fatalf("cursor did not advance: after=%d still returned an event for issue %s: %+v", maxSeq, issueID, ev)
		}
	}

	// ---- org isolation: org2's agent key must NEVER see org1's events, ever ----
	crossRes := agentRequest(t, "GET", "/api/agent/events?after=0&limit=200", bearer2, nil)
	if crossRes.Status != 200 {
		t.Fatalf("GET /api/agent/events (org2 key): got %d, want 200, body=%s", crossRes.Status, crossRes.Body)
	}
	var crossResp feedResp
	crossRes.JSON(t, &crossResp)
	for _, ev := range crossResp.Events {
		if ev.Issue.ID == issueID {
			t.Fatalf("org isolation violated: org2's agent key saw an org1 event for issue %s: %+v", issueID, ev)
		}
	}
}

// TestU37_AgentBatchAppliesMultipleOpsWithPerOpResults exercises POST /api/agent/batch with two
// ops (a comment followed by a status change) against one issue and asserts both applied, with
// per-op results reported in the batch envelope (agent-ops.ts / batch/+server.ts's contract).
func TestU37_AgentBatchAppliesMultipleOpsWithPerOpResults(t *testing.T) {
	requireM5AgentIntegration(t)

	f1 := newFixture(t)
	admin1 := f1.newDashboardUser("admin")
	reporter1 := f1.newDashboardUser("viewer")
	_, bearer1 := f1.seedAgent(t, admin1, "batch-worker-1")

	reportRes := dashboardRequest(t, "POST", "/api/organizations/"+f1.OrgID+"/reports", reporter1, map[string]any{
		"title":     "batch target " + uniqueSuffix(),
		"bodyMd":    "please investigate via batch",
		"severity":  "medium",
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

	batchRes := agentRequest(t, "POST", "/api/agent/batch", bearer1, map[string]any{
		"operations": []map[string]any{
			{
				"op":      "issues.comment",
				"issueId": issueID,
				"params":  map[string]any{"body_md": "batch comment op"},
			},
			{
				"op":      "issues.status",
				"issueId": issueID,
				"params":  map[string]any{"status": "resolved"},
			},
		},
	})
	if batchRes.Status != 200 {
		t.Fatalf("POST /api/agent/batch: got %d, want 200, body=%s", batchRes.Status, batchRes.Body)
	}

	var batchResp struct {
		Completed int `json:"completed"`
		Results   []struct {
			Ok     bool   `json:"ok"`
			Status int    `json:"status"`
			Error  string `json:"error"`
		} `json:"results"`
	}
	batchRes.JSON(t, &batchResp)

	if batchResp.Completed != 2 {
		t.Fatalf("expected batch to complete 2 ops, got %d: %+v", batchResp.Completed, batchResp.Results)
	}
	if len(batchResp.Results) != 2 {
		t.Fatalf("expected 2 per-op results, got %d: %+v", len(batchResp.Results), batchResp.Results)
	}
	if !batchResp.Results[0].Ok || batchResp.Results[0].Status != 201 {
		t.Fatalf("expected op 0 (comment) to succeed with status 201, got %+v", batchResp.Results[0])
	}
	if !batchResp.Results[1].Ok || batchResp.Results[1].Status != 200 {
		t.Fatalf("expected op 1 (status) to succeed with status 200, got %+v", batchResp.Results[1])
	}

	// ---- assert both actually applied ----
	commentsRes := agentRequest(t, "GET", "/api/agent/issues/"+issueID+"/comments?after=1970-01-01T00:00:00Z", bearer1, nil)
	if commentsRes.Status != 200 {
		t.Fatalf("GET agent comments: got %d, want 200, body=%s", commentsRes.Status, commentsRes.Body)
	}
	var commentsResp struct {
		Comments []struct {
			BodyMd string `json:"bodyMd"`
		} `json:"comments"`
	}
	commentsRes.JSON(t, &commentsResp)
	sawComment := false
	for _, c := range commentsResp.Comments {
		if c.BodyMd == "batch comment op" {
			sawComment = true
		}
	}
	if !sawComment {
		t.Fatalf("batch comment op did not apply: %+v", commentsResp.Comments)
	}

	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM issues WHERE id = $1`, issueID).Scan(&status); err != nil {
		t.Fatalf("reading issue status after batch: %v", err)
	}
	if status != "resolved" {
		t.Fatalf("batch status op did not apply: issue status = %q, want resolved", status)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
