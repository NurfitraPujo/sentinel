package loop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// TestHTTPIssuesLister_ListUnresolvedUnclaimed_RequestShape is the red-first proof for the major
// coverage gap the validator's mutation test found: dropping every query parameter
// (since/sort=firstSeen/limit=200/claimed=false) from ListUnresolvedUnclaimed's request left the
// whole module green. Without an exact limit=200, GET /api/agent/issues returns NO .limit() clause
// and NO nextCursor (agent-work.ts:199-219), so a fresh install would backfill a synthetic TRIAGE
// job for EVERY unresolved issue in the org's entire history -- exactly the re-triage storm plan
// §2.1 exists to prevent.
func TestHTTPIssuesLister_ListUnresolvedUnclaimed_RequestShape(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"issues":[]}`)
	}))
	defer srv.Close()

	lister := NewIssuesLister(sentinel.NewClient(srv.URL, "test-key"))
	since := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	if _, err := lister.ListUnresolvedUnclaimed(context.Background(), since); err != nil {
		t.Fatalf("ListUnresolvedUnclaimed: %v", err)
	}

	q, err := parseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parsing recorded query %q: %v", gotQuery, err)
	}
	if got := q.Get("since"); got != "2026-08-10T00:00:00Z" {
		t.Errorf("since = %q, want RFC3339 2026-08-10T00:00:00Z", got)
	}
	if got := q.Get("sort"); got != "firstSeen" {
		t.Errorf("sort = %q, want firstSeen", got)
	}
	if got := q.Get("limit"); got != "200" {
		t.Errorf("limit = %q, want 200 -- without it the server omits nextCursor entirely and this backfills the ENTIRE unresolved-issue history, not just the backfill window", got)
	}
	if got := q.Get("claimed"); got != "false" {
		t.Errorf("claimed = %q, want false", got)
	}
}

// TestHTTPIssuesLister_ListUnresolvedUnclaimed_KeysetPagesUntilNextCursorAbsent proves the lister
// follows nextCursor across pages and stops the moment the server omits the key entirely (the real
// contract per agent-work.ts:212-221 -- there is no explicit hasMore flag on this route, unlike the
// events feed).
func TestHTTPIssuesLister_ListUnresolvedUnclaimed_KeysetPagesUntilNextCursorAbsent(t *testing.T) {
	pages := map[string]string{
		"":      `{"issues":[{"id":"iss-1","status":"unresolved"}],"nextCursor":"cur-2"}`,
		"cur-2": `{"issues":[{"id":"iss-2","status":"unresolved"}],"nextCursor":"cur-3"}`,
		"cur-3": `{"issues":[{"id":"iss-3","status":"unresolved"}]}`, // nextCursor absent -> stop
	}
	var seenCursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		seenCursors = append(seenCursors, cursor)
		body, ok := pages[cursor]
		if !ok {
			t.Fatalf("unexpected cursor %q requested", cursor)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	lister := NewIssuesLister(sentinel.NewClient(srv.URL, "test-key"))
	ids, err := lister.ListUnresolvedUnclaimed(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("ListUnresolvedUnclaimed: %v", err)
	}
	want := []string{"iss-1", "iss-2", "iss-3"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i].ID != id {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i].ID, id)
		}
	}
	if len(seenCursors) != 3 {
		t.Fatalf("expected exactly 3 page requests (stopping when nextCursor is absent), got %d: %v", len(seenCursors), seenCursors)
	}
}

// TestHTTPIssuesLister_ListUnresolvedUnclaimed_FiltersNonUnresolved proves rows the server returns
// that are NOT status=unresolved are dropped client-side rather than backfilled as synthetic TRIAGE
// jobs (Bootstrap only ever wants unresolved, unclaimed issues, plan §2.1 step 1).
func TestHTTPIssuesLister_ListUnresolvedUnclaimed_FiltersNonUnresolved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"issues":[
			{"id":"iss-unresolved","status":"unresolved"},
			{"id":"iss-resolved","status":"resolved"},
			{"id":"iss-ignored","status":"ignored"}
		]}`)
	}))
	defer srv.Close()

	lister := NewIssuesLister(sentinel.NewClient(srv.URL, "test-key"))
	ids, err := lister.ListUnresolvedUnclaimed(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("ListUnresolvedUnclaimed: %v", err)
	}
	if len(ids) != 1 || ids[0].ID != "iss-unresolved" {
		t.Fatalf("ids = %v, want only [iss-unresolved]", ids)
	}
}

// TestHTTPIssuesLister_ListUnresolvedUnclaimed_NonOKSurfacesError proves a non-2xx response is
// surfaced as an error rather than silently treated as an empty page.
func TestHTTPIssuesLister_ListUnresolvedUnclaimed_NonOKSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	lister := NewIssuesLister(sentinel.NewClient(srv.URL, "test-key"))
	if _, err := lister.ListUnresolvedUnclaimed(context.Background(), time.Now()); err == nil {
		t.Fatalf("expected a non-2xx response to surface as an error")
	}
}

// TestHTTPIssuesLister_ListClaimedByMe_RequestShape proves the held-claims seed pass (Bootstrap
// step 2) sends claimed=me with no status filter, and keyset-pages the same way.
func TestHTTPIssuesLister_ListClaimedByMe_RequestShape(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"issues":[{"id":"iss-held","status":"resolved"}]}`)
	}))
	defer srv.Close()

	lister := NewIssuesLister(sentinel.NewClient(srv.URL, "test-key"))
	ids, err := lister.ListClaimedByMe(context.Background())
	if err != nil {
		t.Fatalf("ListClaimedByMe: %v", err)
	}
	// A resolved issue we still hold a claim on must NOT be filtered out (unlike
	// ListUnresolvedUnclaimed) -- a held claim survives resolution until released.
	if len(ids) != 1 || ids[0] != "iss-held" {
		t.Fatalf("ids = %v, want [iss-held] (claimed=me applies no status filter)", ids)
	}

	q, err := parseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parsing recorded query %q: %v", gotQuery, err)
	}
	if got := q.Get("claimed"); got != "me" {
		t.Errorf("claimed = %q, want me", got)
	}
	if got := q.Get("limit"); got != "200" {
		t.Errorf("limit = %q, want 200", got)
	}
	if q.Get("since") != "" || q.Get("sort") != "" {
		t.Errorf("expected no since/sort params on the claimed=me pass, got since=%q sort=%q", q.Get("since"), q.Get("sort"))
	}
}

// TestHTTPIssuesLister_ListClaimedByMe_NonOKSurfacesError mirrors the unresolved-list case for the
// claimed=me pass.
func TestHTTPIssuesLister_ListClaimedByMe_NonOKSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":"boom"}`)
	}))
	defer srv.Close()

	lister := NewIssuesLister(sentinel.NewClient(srv.URL, "test-key"))
	if _, err := lister.ListClaimedByMe(context.Background()); err == nil {
		t.Fatalf("expected a non-2xx response to surface as an error")
	}
}

func parseQuery(raw string) (url.Values, error) {
	return url.ParseQuery(raw)
}
