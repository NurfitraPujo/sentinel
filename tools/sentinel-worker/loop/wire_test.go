package loop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// realEventsPageJSON is copied verbatim from docs/agents/SENTINEL_AGENT_GUIDE.md §3's documented
// GET /api/agent/events response body — the server's actual wire shape: `eventType` (not `type`),
// no top-level issue id (nested at `issue.id`), and `createdAt` (not `at`). This is the golden
// fixture the B5 cross-boundary bug (Event's JSON tags not matching the real wire contract) is
// tested against, instead of the module's own Go-struct-literal test fakes, which cannot catch a
// wire-tag mismatch because they bypass JSON entirely.
const realEventsPageJSON = `{
  "events": [
    {
      "seq": 42,
      "eventType": "status_changed",
      "actorType": "agent",
      "actorId": "agt_other",
      "oldValue": { "status": "unresolved" },
      "newValue": { "status": "resolved" },
      "createdAt": "2026-08-14T12:00:00.000Z",
      "issue": {
        "id": "iss_example",
        "title": "NPE in checkout handler",
        "status": "resolved",
        "issueType": "system_error",
        "projectId": "prj_example",
        "assigneeType": "agent",
        "assignedTo": "agt_other",
        "claimedAt": "2026-08-14T11:59:00.000Z",
        "waitingOn": null
      }
    }
  ],
  "cursor": 42,
  "hasMore": false
}`

// TestEvent_RealWirePayloadDecodesAndDispatches proves Event's JSON tags match the server's real
// wire contract end to end: unmarshal the guide's exact payload, then assert Type/IssueID/At/
// Classify all come out correct. Before the B5 fix this failed with Type="" IssueID error
// Classify=KindNone for every real event.
func TestEvent_RealWirePayloadDecodesAndDispatches(t *testing.T) {
	var page EventsPage
	if err := json.Unmarshal([]byte(realEventsPageJSON), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(page.Events))
	}
	e := page.Events[0]

	if e.Type != "status_changed" {
		t.Errorf("Type = %q, want %q", e.Type, "status_changed")
	}
	issueID, err := e.IssueID()
	if err != nil {
		t.Fatalf("IssueID(): %v", err)
	}
	if issueID != "iss_example" {
		t.Errorf("IssueID() = %q, want %q", issueID, "iss_example")
	}
	if e.At.IsZero() {
		t.Errorf("At is zero, want a parsed createdAt timestamp")
	}
	if e.Seq != 42 {
		t.Errorf("Seq = %d, want 42", e.Seq)
	}

	// status_changed -> resolved -> KindCancelQueued (plan §3 dispatch table).
	if got := Classify(e, "agent-me"); got != KindCancelQueued {
		t.Errorf("Classify = %q, want KindCancelQueued", got)
	}
}

// TestHTTPEventsClient_RealWirePayload drives the SAME payload through httpEventsClient (the
// concrete HTTP path the poll loop actually uses in production) against an httptest server, so the
// coverage isn't limited to json.Unmarshal called directly by the test.
func TestHTTPEventsClient_RealWirePayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(realEventsPageJSON))
	}))
	defer srv.Close()

	client := sentinel.NewClient(srv.URL, "test-key")
	ec := NewEventsClient(client, nil, nil)

	page, err := ec.GetEvents(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("expected 1 event via the HTTP path, got %d", len(page.Events))
	}
	e := page.Events[0]
	issueID, err := e.IssueID()
	if err != nil || issueID != "iss_example" {
		t.Fatalf("HTTP path decoded IssueID=%q err=%v — dispatcher would classify wrongly for every real event", issueID, err)
	}
	if e.Type != "status_changed" {
		t.Fatalf("HTTP path decoded Type=%q, want %q", e.Type, "status_changed")
	}
}
