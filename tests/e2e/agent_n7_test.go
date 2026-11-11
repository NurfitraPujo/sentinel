//go:build e2e

// Package e2e — N7g: e2e proof for the N7a-f remediations (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md).
//
// U38 covers discovery (N7b: since=/sort=firstSeen pagination on GET /api/agent/issues, and the
// 'created'/'occurrence_burst' events feed rows). U39 covers the agent lifecycle remediations: N7f's
// GET /api/agent/self, N7d's retry-safe PATCH status (changed:false, no duplicate status_changed
// event) and idempotent double-release (A13), and N7e's own-comment edit/delete route.
//
// Both reuse agent_work_loop_test.go's seedAgent/agentRequest helpers and
// agent_events_batch_test.go's itoa, and provision their own org/agent/key/project via
// fixture/newDashboardUser exactly like every other file in this package.
package e2e

import (
	"testing"
	"time"
)

// TestU38_DiscoveryViaEventsFeedAndIssuesSincePagination proves N7b's discovery remediation: an
// agent ingesting a brand-new error can find it through the events feed as a 'created' row, and
// through GET /api/agent/issues?since=&sort=firstSeen&limit= pagination -- without ever having to
// list every issue in the project.
func TestU38_DiscoveryViaEventsFeedAndIssuesSincePagination(t *testing.T) {
	requireM5AgentIntegration(t)
	requireStack(t)

	f := newFixture(t)
	admin := f.newDashboardUser("admin")
	_, bearer := f.seedAgent(t, admin, "discovery-worker")

	t0 := time.Now().UTC()

	// The processor's normalizer masks digit runs in error_class to <NUMERIC_ID> (fingerprint
	// stability), so the unique suffix must be alphabetic or the ErrorClass round-trip assertion
	// below fails against the masked value.
	uniqueClass := "U38DiscoveryError-" + alphaSuffix()
	uniqueMsg := "u38 discovery target " + uniqueSuffix()

	ev := f.newEvent().with(map[string]any{
		"error_class": uniqueClass,
		"message":     uniqueMsg,
	})

	ingestRes := f.ingest(ev)
	if ingestRes.Status != 202 {
		t.Fatalf("POST /ingest: got %d, want 202, body=%s", ingestRes.Status, ingestRes.Body)
	}

	f.waitForIssues(1)
	issue := ingestOnlyIssueSummary(t, f)
	if issue.ErrorClass != uniqueClass {
		t.Fatalf("issue.ErrorClass = %q, want %q", issue.ErrorClass, uniqueClass)
	}

	// ---- events feed: a 'created' row for this issue must appear, respecting the feed's 2s lag ----
	type feedEvent struct {
		Seq       int64  `json:"seq"`
		EventType string `json:"eventType"`
		Issue     struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	type feedResp struct {
		Events []feedEvent `json:"events"`
	}

	sawCreated := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !sawCreated {
		pollRes := agentRequest(t, "GET", "/api/agent/events?after=0&limit=200", bearer, nil)
		if pollRes.Status != 200 {
			t.Fatalf("GET /api/agent/events: got %d, want 200, body=%s", pollRes.Status, pollRes.Body)
		}
		var resp feedResp
		pollRes.JSON(t, &resp)
		for _, ev := range resp.Events {
			if ev.Issue.ID == issue.ID && ev.EventType == "created" {
				sawCreated = true
				break
			}
		}
		if !sawCreated {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !sawCreated {
		t.Fatalf("events feed never surfaced a 'created' event for newly discovered issue %s", issue.ID)
	}

	// ---- re-ingest the SAME error: within OCCURRENCE_EVENT_MIN_INTERVAL_SECONDS's default 1h
	// throttle window no second 'occurrence_burst' (or 'created') row should appear for this issue.
	// This asserts the throttle's absence-of-noise property, not the burst path itself. ----
	reingestRes := f.ingest(ev.with(map[string]any{
		"trace_id": "trace-" + uniqueSuffix(),
		"span_id":  "span-" + uniqueSuffix(),
	}))
	if reingestRes.Status != 202 {
		t.Fatalf("POST /ingest (re-ingest): got %d, want 202, body=%s", reingestRes.Status, reingestRes.Body)
	}
	f.waitForOccurrences(2)

	// Give the async pipeline a moment to have written anything it was going to write, then assert
	// exactly one 'created' row and zero 'occurrence_burst' rows for this issue.
	time.Sleep(3 * time.Second)
	finalRes := agentRequest(t, "GET", "/api/agent/events?after=0&limit=500", bearer, nil)
	if finalRes.Status != 200 {
		t.Fatalf("GET /api/agent/events (final): got %d, want 200, body=%s", finalRes.Status, finalRes.Body)
	}
	var finalResp feedResp
	finalRes.JSON(t, &finalResp)
	createdCount, burstCount := 0, 0
	for _, ev := range finalResp.Events {
		if ev.Issue.ID != issue.ID {
			continue
		}
		switch ev.EventType {
		case "created":
			createdCount++
		case "occurrence_burst":
			burstCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("expected exactly 1 'created' event for issue %s, got %d", issue.ID, createdCount)
	}
	if burstCount != 0 {
		t.Fatalf("expected 0 'occurrence_burst' events for issue %s within the throttle window, got %d", issue.ID, burstCount)
	}

	// ---- GET /api/agent/issues?since=<t0>&sort=firstSeen&limit=10: finds the issue, with nextCursor
	// semantics (a small limit forces pagination). ----
	type agentIssueRow struct {
		ID string `json:"id"`
	}
	type agentIssuesResp struct {
		Issues     []agentIssueRow `json:"issues"`
		NextCursor string          `json:"nextCursor"`
	}

	sinceParam := t0.Format(time.RFC3339Nano)
	listRes := agentRequest(t, "GET", "/api/agent/issues?since="+sinceParam+"&sort=firstSeen&limit=10", bearer, nil)
	if listRes.Status != 200 {
		t.Fatalf("GET /api/agent/issues?since=&sort=firstSeen&limit=10: got %d, want 200, body=%s", listRes.Status, listRes.Body)
	}
	var listResp agentIssuesResp
	listRes.JSON(t, &listResp)

	foundViaSince := false
	for _, row := range listResp.Issues {
		if row.ID == issue.ID {
			foundViaSince = true
		}
	}
	if !foundViaSince {
		t.Fatalf("GET /api/agent/issues?since=%s&sort=firstSeen did not include newly discovered issue %s: %+v",
			sinceParam, issue.ID, listResp.Issues)
	}

	// nextCursor round-trip: if present, using it as cursor must not error and must not repeat the
	// same page's rows.
	if listResp.NextCursor != "" {
		cursorRes := agentRequest(t, "GET",
			"/api/agent/issues?since="+sinceParam+"&sort=firstSeen&limit=10&cursor="+listResp.NextCursor, bearer, nil)
		if cursorRes.Status != 200 {
			t.Fatalf("GET /api/agent/issues with nextCursor: got %d, want 200, body=%s", cursorRes.Status, cursorRes.Body)
		}
		var cursorResp agentIssuesResp
		cursorRes.JSON(t, &cursorResp)
		for _, row := range cursorResp.Issues {
			if row.ID == issue.ID {
				t.Fatalf("nextCursor page repeated issue %s already seen on the first page", issue.ID)
			}
		}
	}
}

// TestU39_LifecycleSelfRetryReleaseAndOwnCommentEditDelete proves N7f/N7d/N7e's lifecycle
// remediations: GET /api/agent/self identity, retry-idempotent PATCH status (changed:false, no
// duplicate status_changed event), idempotent double-release, and own-comment edit/delete.
func TestU39_LifecycleSelfRetryReleaseAndOwnCommentEditDelete(t *testing.T) {
	requireM5AgentIntegration(t)
	requireStack(t)

	f := newFixture(t)
	admin := f.newDashboardUser("admin")
	reporter := f.newDashboardUser("viewer")
	agentID, bearer := f.seedAgent(t, admin, "lifecycle-worker")

	reportRes := dashboardRequest(t, "POST", "/api/organizations/"+f.OrgID+"/reports", reporter, map[string]any{
		"title":     "u39 lifecycle target " + uniqueSuffix(),
		"bodyMd":    "please investigate",
		"severity":  "high",
		"projectId": f.ProjectID,
	})
	if reportRes.Status != 201 {
		t.Fatalf("seeding report: got %d, want 201, body=%s", reportRes.Status, reportRes.Body)
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

	// ---- GET /self returns the agent's own identity ----
	selfRes := agentRequest(t, "GET", "/api/agent/self", bearer, nil)
	if selfRes.Status != 200 {
		t.Fatalf("GET /api/agent/self: got %d, want 200, body=%s", selfRes.Status, selfRes.Body)
	}
	var selfResp struct {
		AgentID        string `json:"agentId"`
		OrganizationID string `json:"organizationId"`
	}
	selfRes.JSON(t, &selfResp)
	if selfResp.AgentID != agentID {
		t.Fatalf("GET /api/agent/self returned agentId %q, want %q: %s", selfResp.AgentID, agentID, selfRes.Body)
	}
	if selfResp.OrganizationID != f.OrgID {
		t.Fatalf("GET /api/agent/self returned organizationId %q, want %q: %s", selfResp.OrganizationID, f.OrgID, selfRes.Body)
	}

	// ---- claim ----
	claimRes := agentRequest(t, "POST", "/api/agent/issues/"+issueID+"/claim", bearer, nil)
	if claimRes.Status != 200 {
		t.Fatalf("POST claim: got %d, want 200, body=%s", claimRes.Status, claimRes.Body)
	}

	// ---- status retry idempotence: two identical PATCHes; second returns changed:false, and the
	// events feed shows exactly ONE status_changed event for this issue ----
	statusBody := map[string]any{"status": "resolved"}

	firstStatusRes := agentRequest(t, "PATCH", "/api/agent/issues/"+issueID+"/status", bearer, statusBody)
	if firstStatusRes.Status != 200 {
		t.Fatalf("PATCH status (1st): got %d, want 200, body=%s", firstStatusRes.Status, firstStatusRes.Body)
	}
	var firstStatusResp struct {
		Success bool `json:"success"`
		Changed bool `json:"changed"`
	}
	firstStatusRes.JSON(t, &firstStatusResp)
	if !firstStatusResp.Changed {
		t.Fatalf("PATCH status (1st): expected changed:true, got %+v: %s", firstStatusResp, firstStatusRes.Body)
	}

	secondStatusRes := agentRequest(t, "PATCH", "/api/agent/issues/"+issueID+"/status", bearer, statusBody)
	if secondStatusRes.Status != 200 {
		t.Fatalf("PATCH status (2nd, retry): got %d, want 200, body=%s", secondStatusRes.Status, secondStatusRes.Body)
	}
	var secondStatusResp struct {
		Success bool `json:"success"`
		Changed bool `json:"changed"`
	}
	secondStatusRes.JSON(t, &secondStatusResp)
	if secondStatusResp.Changed {
		t.Fatalf("PATCH status (2nd, retry): expected changed:false, got %+v: %s", secondStatusResp, secondStatusRes.Body)
	}

	type feedEvent struct {
		Seq       int64  `json:"seq"`
		EventType string `json:"eventType"`
		Issue     struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	type feedResp struct {
		Events []feedEvent `json:"events"`
	}

	// Give the retry a moment to have written anything it was going to write before counting.
	time.Sleep(2 * time.Second)
	eventsRes := agentRequest(t, "GET", "/api/agent/events?after=0&limit=200", bearer, nil)
	if eventsRes.Status != 200 {
		t.Fatalf("GET /api/agent/events: got %d, want 200, body=%s", eventsRes.Status, eventsRes.Body)
	}
	var eventsResp feedResp
	eventsRes.JSON(t, &eventsResp)
	statusChangedCount := 0
	for _, ev := range eventsResp.Events {
		if ev.Issue.ID == issueID && ev.EventType == "status_changed" {
			statusChangedCount++
		}
	}
	if statusChangedCount != 1 {
		t.Fatalf("expected exactly 1 status_changed event for issue %s after retry, got %d: %+v",
			issueID, statusChangedCount, eventsResp.Events)
	}

	// ---- release twice; both 200 (A13: idempotent release) ----
	firstReleaseRes := agentRequest(t, "DELETE", "/api/agent/issues/"+issueID+"/claim", bearer, nil)
	if firstReleaseRes.Status != 200 {
		t.Fatalf("DELETE claim (1st release): got %d, want 200, body=%s", firstReleaseRes.Status, firstReleaseRes.Body)
	}
	secondReleaseRes := agentRequest(t, "DELETE", "/api/agent/issues/"+issueID+"/claim", bearer, nil)
	if secondReleaseRes.Status != 200 {
		t.Fatalf("DELETE claim (2nd release, retry): got %d, want 200, body=%s", secondReleaseRes.Status, secondReleaseRes.Body)
	}

	// ---- own-comment edit/delete via the new route ----
	commentRes := agentRequest(t, "POST", "/api/agent/issues/"+issueID+"/comments", bearer, map[string]any{
		"body_md": "u39 original comment",
	})
	if commentRes.Status != 201 {
		t.Fatalf("POST comment: got %d, want 201, body=%s", commentRes.Status, commentRes.Body)
	}
	var commentResp struct {
		Comment struct {
			ID     string `json:"id"`
			BodyMd string `json:"bodyMd"`
		} `json:"comment"`
	}
	commentRes.JSON(t, &commentResp)
	commentID := commentResp.Comment.ID
	if commentID == "" {
		t.Fatalf("comment creation response had no id: %s", commentRes.Body)
	}

	editRes := agentRequest(t, "PATCH", "/api/agent/issues/"+issueID+"/comments/"+commentID, bearer, map[string]any{
		"body_md": "u39 edited comment",
	})
	if editRes.Status != 200 {
		t.Fatalf("PATCH comment: got %d, want 200, body=%s", editRes.Status, editRes.Body)
	}
	var editResp struct {
		Comment struct {
			ID     string `json:"id"`
			BodyMd string `json:"bodyMd"`
		} `json:"comment"`
	}
	editRes.JSON(t, &editResp)
	if editResp.Comment.BodyMd != "u39 edited comment" {
		t.Fatalf("PATCH comment: bodyMd = %q, want %q: %s", editResp.Comment.BodyMd, "u39 edited comment", editRes.Body)
	}

	deleteRes := agentRequest(t, "DELETE", "/api/agent/issues/"+issueID+"/comments/"+commentID, bearer, nil)
	if deleteRes.Status != 200 {
		t.Fatalf("DELETE comment: got %d, want 200, body=%s", deleteRes.Status, deleteRes.Body)
	}
	var deleteResp struct {
		Success bool   `json:"success"`
		IssueID string `json:"issueId"`
	}
	deleteRes.JSON(t, &deleteResp)
	if !deleteResp.Success {
		t.Fatalf("DELETE comment: expected success:true, got %+v: %s", deleteResp, deleteRes.Body)
	}
	if deleteResp.IssueID != issueID {
		t.Fatalf("DELETE comment: issueId = %q, want %q", deleteResp.IssueID, issueID)
	}
}

// alphaSuffix is uniqueSuffix with every digit mapped to a letter, for identifiers that flow
// through the processor's normalizer (which masks digit runs to <NUMERIC_ID>, e.g. error_class).
func alphaSuffix() string {
	digits := "abcdefghij"
	out := make([]byte, 0, 16)
	for _, c := range uniqueSuffix() {
		if c >= '0' && c <= '9' {
			out = append(out, digits[c-'0'])
		} else {
			out = append(out, 'x')
		}
	}
	return string(out)
}
