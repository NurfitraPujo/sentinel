//go:build e2e

// Package e2e — sentinel-event-schema-v2 (TASK-07, supplementary evidence, not a plane gate): contract
// §1's hypothesis verbatim. An event carrying every v2 field goes through /ingest and comes back from the
// agent occurrence endpoint with every field intact, stored per occurrence, masked wherever a sensitive
// key or a card number requires it, and never normalized.
//
// Needs the compose stack REBUILT (`docker compose up -d --build --force-recreate`), because a stale
// processor image never stores the v2 columns.
package e2e

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestEventV2_AgentOccurrenceReadBack(t *testing.T) {
	f := newFixture(t)
	admin := f.newDashboardUser("admin")
	_, bearer := f.seedAgent(t, admin, "event-v2-reader")

	raw, err := os.ReadFile("../fixtures/events/event-v2-full.json")
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
	body["project_key"] = f.ProjectName
	body["event_id"] = "evt-e2e-v2-" + uniqueSuffix()
	body["environment"] = "e2e"
	delete(body, "timestamp")

	if res := f.ingest(body); res.Status != 202 {
		t.Fatalf("POST /ingest: want 202, got %d: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(1)
	issue := f.onlyIssue()

	res := agentRequest(t, "GET", "/api/agent/issues/"+issue.ID+"/occurrences", bearer, nil)
	if res.Status != 200 {
		t.Fatalf("GET occurrences: want 200, got %d: %s", res.Status, res.Body)
	}
	var out struct {
		Occurrences []map[string]any `json:"occurrences"`
	}
	res.JSON(t, &out)
	if len(out.Occurrences) != 1 {
		t.Fatalf("want 1 occurrence, got %d", len(out.Occurrences))
	}
	occ := out.Occurrences[0]

	want := map[string]any{
		"level": "warning", "logger": "checkout", "serverName": "web-7f9c-pod-2", "transaction": "/checkout/pay",
		"dist": "4127", "source": "onerror", "errorClass": "TypeError", "traceFlags": 1.0,
		"message": "charge [REDACTED_PAN] failed for order 7c9e6679-7425-40de-944b-e07fc1f90ae7",
	}
	for k, v := range want {
		if occ[k] != v {
			t.Errorf("%s: want %v, got %v", k, v, occ[k])
		}
	}
	if occ["receivedAt"] == nil {
		t.Error("receivedAt must be set by the ingestor")
	} else if at, err := time.Parse(time.RFC3339Nano, occ["receivedAt"].(string)); err != nil || time.Since(at) > 10*time.Minute {
		t.Errorf("receivedAt %v is not a recent timestamp (%v)", occ["receivedAt"], err)
	}

	user := occ["user"].(map[string]any)
	if user["email"] != "alice@example.com" || user["ip_address"] != "2001:db8::1" {
		t.Errorf("user must be stored as sent: %v", user)
	}
	headers := occ["request"].(map[string]any)["headers"].(map[string]any)
	for _, h := range []string{"Authorization", "Cookie", "X-API-Key"} {
		if headers[h] != "***REDACTED***" {
			t.Errorf("request.headers.%s: want ***REDACTED***, got %v", h, headers[h])
		}
	}
	metadata := occ["metadata"].(map[string]any)
	if metadata["email"] != "alice@example.com" || metadata["request_id"] != "7c9e6679-7425-40de-944b-e07fc1f90ae7" {
		t.Errorf("metadata must be verbatim, not normalized: %v", metadata)
	}
	crumbs := occ["breadcrumbs"].([]any)
	if len(crumbs) != 2 || crumbs[1].(map[string]any)["data"].(map[string]any)["password"] != "***REDACTED***" {
		t.Errorf("breadcrumbs: want two, the password redacted: %v", crumbs)
	}
	if values := occ["exception"].(map[string]any)["values"].([]any); len(values) != 2 {
		t.Errorf("exception.values: want 2, got %d", len(values))
	}
	if tags := occ["tags"].(map[string]any); tags["tenant_id"] != "tenant-acme" {
		t.Errorf("tags: %v", tags)
	}
	if device := occ["contexts"].(map[string]any)["device"].(map[string]any); device["model"] != "Pixel 8" {
		t.Errorf("contexts.device: %v", device)
	}
}
