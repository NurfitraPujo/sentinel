package loop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// TestHTTPClaimer_FreshClaimReturnsHeld drives HTTPClaimer through an httptest server standing in
// for POST /api/agent/issues/:id/claim, proving the live path routes through sentinel.Client's
// endpoint wrapper (not a second hand-rolled request) and a 200 reports held with no claimedBy.
func TestHTTPClaimer_FreshClaimReturnsHeld(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"success":true,"issue":{},"alreadyClaimed":false}`))
	}))
	defer srv.Close()

	c := HTTPClaimer{Client: sentinel.NewClient(srv.URL, "test-key")}
	held, claimedBy, err := c.EnsureClaimed(context.Background(), "iss_1")
	if err != nil {
		t.Fatalf("EnsureClaimed: %v", err)
	}
	if !held {
		t.Fatalf("held = false, want true on 200")
	}
	if claimedBy != "" {
		t.Fatalf("claimedBy = %q, want empty on a fresh claim", claimedBy)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/agent/issues/iss_1/claim" {
		t.Fatalf("path = %q, want /api/agent/issues/iss_1/claim", gotPath)
	}
}

// TestHTTPClaimer_ForeignConflictSurfacesClaimedBy is the regression this package's B3 finding
// exists to prevent: the live claim path must actually parse and surface C1's 409
// {claimedBy, claimedAt} body to the runner, not just to an unreachable client_test.go golden.
func TestHTTPClaimer_ForeignConflictSurfacesClaimedBy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		w.Write([]byte(`{"claimedBy":"agt_other","claimedAt":"2026-08-14T11:59:00.000Z"}`))
	}))
	defer srv.Close()

	c := HTTPClaimer{Client: sentinel.NewClient(srv.URL, "test-key")}
	held, claimedBy, err := c.EnsureClaimed(context.Background(), "iss_1")
	if err != nil {
		t.Fatalf("EnsureClaimed: %v", err)
	}
	if held {
		t.Fatalf("held = true, want false on 409")
	}
	if claimedBy != "agt_other" {
		t.Fatalf("claimedBy = %q, want %q (parsed from the 409 body)", claimedBy, "agt_other")
	}
}

// TestHTTPIssueReader_ParsesLiveResponse drives HTTPIssueReader through an httptest server,
// proving it routes through sentinel.Client.GetIssue rather than a second hand-rolled request.
func TestHTTPIssueReader_ParsesLiveResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/issues/iss_1" {
			t.Errorf("path = %q, want /api/agent/issues/iss_1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"issue":{"id":"iss_1","status":"unresolved","assigneeType":"agent","assignedTo":"agt_me"}}`))
	}))
	defer srv.Close()

	r := HTTPIssueReader{Client: sentinel.NewClient(srv.URL, "test-key")}
	snap, err := r.GetIssue(context.Background(), "iss_1")
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if snap.ID != "iss_1" || snap.Status != "unresolved" || snap.AssigneeType != "agent" || snap.AssignedTo != "agt_me" {
		t.Fatalf("snap = %+v, unexpected", snap)
	}
}
