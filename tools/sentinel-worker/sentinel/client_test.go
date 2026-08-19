package sentinel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// capturedRequest records everything a golden test needs to assert about one incoming request.
type capturedRequest struct {
	Method string
	Path   string
	Query  string
	Body   map[string]interface{}
}

// captureServer returns an httptest.Server that decodes each request's JSON body (if any) and
// hands the result to onRequest, then replies with the given status/body.
func captureServer(t *testing.T, status int, respBody string, onRequest func(capturedRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body) // empty/absent body is fine for GETs
		onRequest(capturedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: body})
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
}

func TestGetEvents_RequestShape(t *testing.T) {
	var got capturedRequest
	srv := captureServer(t, 200, `{"events":[],"cursor":0,"hasMore":false}`, func(r capturedRequest) { got = r })
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	res, err := c.GetEvents(context.Background(), 42, 50, "issue_created,commented", "proj-1")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if res.Status != 200 {
		t.Fatalf("status = %d", res.Status)
	}
	if got.Method != "GET" || got.Path != "/api/agent/events" {
		t.Fatalf("request = %s %s", got.Method, got.Path)
	}
	q := got.Query
	for _, want := range []string{"after=42", "limit=50", "project=proj-1"} {
		if !contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
}

// TestGetEvents_429RetryAfterReachableThroughResultHeader proves the client's own endpoint surface
// (not just Do()'s raw *http.Response) carries enough to drive WaitRateLimit: Result.Header must
// expose Retry-After so a caller of GetEvents (not just Do) can honor it.
func TestGetEvents_429RetryAfterReachableThroughResultHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	res, err := c.GetEvents(context.Background(), 0, 0, "", "")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if res.Status != 429 {
		t.Fatalf("status = %d", res.Status)
	}

	var slept time.Duration
	fake := func(d time.Duration) { slept = d }
	got := WaitRateLimit(res.Header, fake)
	if got != 17*time.Second || slept != 17*time.Second {
		t.Fatalf("WaitRateLimit through GetEvents' Result.Header = %v (slept %v), want 17s", got, slept)
	}
}

func TestGetIssue_RequestShape(t *testing.T) {
	var got capturedRequest
	srv := captureServer(t, 200, `{}`, func(r capturedRequest) { got = r })
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	if _, err := c.GetIssue(context.Background(), "iss-1"); err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Method != "GET" || got.Path != "/api/agent/issues/iss-1" {
		t.Fatalf("request = %s %s", got.Method, got.Path)
	}
}

func TestListIssues_RequestShape(t *testing.T) {
	var got capturedRequest
	srv := captureServer(t, 200, `{}`, func(r capturedRequest) { got = r })
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	opts := IssuesListOptions{Since: "2026-08-01T00:00:00Z", Sort: "firstSeen", Limit: 200, Cursor: "cur-1", Claimed: "me", Waiting: true}
	if _, err := c.ListIssues(context.Background(), opts); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if got.Path != "/api/agent/issues" {
		t.Fatalf("path = %s", got.Path)
	}
	for _, want := range []string{"since=", "sort=firstSeen", "limit=200", "cursor=cur-1", "claimed=me", "waiting=true"} {
		if !contains(got.Query, want) {
			t.Errorf("query %q missing %q", got.Query, want)
		}
	}
}

func TestListProjects_and_GetSelf_RequestShape(t *testing.T) {
	var paths []string
	srv := captureServer(t, 200, `{}`, func(r capturedRequest) { paths = append(paths, r.Path) })
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	if _, err := c.ListProjects(context.Background()); err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if _, err := c.GetSelf(context.Background()); err != nil {
		t.Fatalf("GetSelf: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/api/agent/projects" || paths[1] != "/api/agent/self" {
		t.Fatalf("paths = %v", paths)
	}
}

// TestClaimIssue_Success200_AlreadyClaimedFlag proves C1: both a fresh claim and a self-reclaim
// return 200, distinguished only by alreadyClaimed — no conflict is returned.
func TestClaimIssue_Success200_AlreadyClaimedFlag(t *testing.T) {
	var got capturedRequest
	srv := captureServer(t, 200, `{"success":true,"issue":{},"alreadyClaimed":true}`, func(r capturedRequest) { got = r })
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	res, conflict, err := c.ClaimIssue(context.Background(), "iss-1")
	if err != nil {
		t.Fatalf("ClaimIssue: %v", err)
	}
	if got.Method != "POST" || got.Path != "/api/agent/issues/iss-1/claim" {
		t.Fatalf("request = %s %s", got.Method, got.Path)
	}
	if res.Status != 200 {
		t.Fatalf("status = %d", res.Status)
	}
	if conflict != nil {
		t.Fatalf("expected no conflict on 200, got %+v", conflict)
	}
	var parsed ClaimResult
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !parsed.AlreadyClaimed {
		t.Fatalf("expected AlreadyClaimed=true")
	}
}

// TestClaimIssue_409ParsesForeignClaimant proves C1's "409 with {claimedBy, claimedAt}" body shape
// is parsed into ClaimConflict — the only case that is genuinely foreign.
func TestClaimIssue_409ParsesForeignClaimant(t *testing.T) {
	srv := captureServer(t, 409, `{"claimedBy":"agent-other","claimedAt":"2026-08-18T00:00:00Z"}`, func(capturedRequest) {})
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	res, conflict, err := c.ClaimIssue(context.Background(), "iss-1")
	if err != nil {
		t.Fatalf("ClaimIssue: %v", err)
	}
	if res.Status != 409 {
		t.Fatalf("status = %d", res.Status)
	}
	if conflict == nil || conflict.ClaimedBy != "agent-other" {
		t.Fatalf("expected foreign claimant conflict, got %+v", conflict)
	}
}

func TestPostBatch_RequestShape_StopOnErrorFalse(t *testing.T) {
	var got capturedRequest
	srv := captureServer(t, 200, `{"results":[]}`, func(r capturedRequest) { got = r })
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	req := BatchRequest{
		Operations: []BatchOperation{
			NewBatchOperation("issues.comment", "iss-1", map[string]interface{}{"body_md": "hi"}, "job-1:0"),
		},
		StopOnError: false,
	}
	if _, err := c.PostBatch(context.Background(), req); err != nil {
		t.Fatalf("PostBatch: %v", err)
	}
	if got.Method != "POST" || got.Path != "/api/agent/batch" {
		t.Fatalf("request = %s %s", got.Method, got.Path)
	}
	if v, ok := got.Body["stopOnError"].(bool); !ok || v {
		t.Fatalf("expected stopOnError:false, got %v", got.Body["stopOnError"])
	}
	ops, ok := got.Body["operations"].([]interface{})
	if !ok || len(ops) != 1 {
		t.Fatalf("operations = %v", got.Body["operations"])
	}
	op := ops[0].(map[string]interface{})
	// idempotency_key must live INSIDE params (matching the server's single-route body shape),
	// never as a sibling of params at the op's top level -- the server never reads it from there.
	if _, present := op["idempotency_key"]; present {
		t.Fatalf("idempotency_key must not be a top-level op field, got %v", op)
	}
	params, ok := op["params"].(map[string]interface{})
	if !ok {
		t.Fatalf("params = %v", op["params"])
	}
	if params["idempotency_key"] != "job-1:0" {
		t.Fatalf("params.idempotency_key = %v", params["idempotency_key"])
	}
	if params["body_md"] != "hi" {
		t.Fatalf("params.body_md = %v", params["body_md"])
	}
}

// TestPostComment_Progress_Question_CarryIdempotencyKey proves C4's opt-in body field plumbing:
// present when a key is given, absent entirely when it isn't.
func TestPostComment_Progress_Question_CarryIdempotencyKey(t *testing.T) {
	var got capturedRequest
	srv := captureServer(t, 201, `{}`, func(r capturedRequest) { got = r })
	defer srv.Close()

	c := NewClient(srv.URL, "key")

	if _, err := c.PostComment(context.Background(), "iss-1", "hello", "job-1:0"); err != nil {
		t.Fatalf("PostComment: %v", err)
	}
	if got.Path != "/api/agent/issues/iss-1/comments" || got.Body["idempotency_key"] != "job-1:0" {
		t.Fatalf("comment request = %+v", got)
	}
	if got.Body["body_md"] != "hello" {
		t.Fatalf("comment body_md = %v, want %q", got.Body["body_md"], "hello")
	}
	if _, present := got.Body["body"]; present {
		t.Fatalf("comment must not send legacy 'body' field, got %+v", got.Body)
	}

	if _, err := c.PostProgress(context.Background(), "iss-1", "still working", ""); err != nil {
		t.Fatalf("PostProgress: %v", err)
	}
	if got.Path != "/api/agent/issues/iss-1/progress" {
		t.Fatalf("progress path = %s", got.Path)
	}
	if _, present := got.Body["idempotency_key"]; present {
		t.Fatalf("expected idempotency_key omitted when empty, got %+v", got.Body)
	}
	if got.Body["message_md"] != "still working" {
		t.Fatalf("progress message_md = %v, want %q", got.Body["message_md"], "still working")
	}
	if _, present := got.Body["note"]; present {
		t.Fatalf("progress must not send legacy 'note' field, got %+v", got.Body)
	}

	if _, err := c.PostQuestion(context.Background(), "iss-1", map[string]interface{}{"body_md": "why?", "audience": "reporter"}, "job-2:1"); err != nil {
		t.Fatalf("PostQuestion: %v", err)
	}
	if got.Path != "/api/agent/issues/iss-1/questions" || got.Body["idempotency_key"] != "job-2:1" || got.Body["audience"] != "reporter" || got.Body["body_md"] != "why?" {
		t.Fatalf("question request = %+v", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func TestClient_Do_SetsBearerAndReturnsBody(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "secret-key")
	res, _, err := c.Do(context.Background(), "GET", "/api/agent/issues", nil, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if res.Status != 200 {
		t.Fatalf("expected 200, got %d", res.Status)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("expected Bearer header, got %q", gotAuth)
	}
}

// TestClient_KeyIsReadFreshOnEachCall proves keyguard's rotation-swap contract: changing c.Key
// between calls changes what the NEXT request sends, without rebuilding the Client.
func TestClient_KeyIsReadFreshOnEachCall(t *testing.T) {
	var lastAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key-1")
	if _, _, err := c.Do(context.Background(), "GET", "/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if lastAuth != "Bearer key-1" {
		t.Fatalf("expected key-1, got %q", lastAuth)
	}

	c.SetKey("key-2") // simulate keyguard rotation swap
	if _, _, err := c.Do(context.Background(), "GET", "/x", nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if lastAuth != "Bearer key-2" {
		t.Fatalf("expected key-2 after rotation, got %q", lastAuth)
	}
}

// TestClient_Do_InvokesOnAuthStatus proves the health.Status.SetAuthValid wiring seam (plan §7):
// a 401 response reports auth-invalid; a 2xx response reports auth-valid (clearing a prior
// failure); other statuses (500, 404) are not evidence about the credential and must not call the
// hook at all.
func TestClient_Do_InvokesOnAuthStatus(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantCalled bool
		wantOK     bool
	}{
		{"401 reports auth invalid", http.StatusUnauthorized, true, false},
		{"200 reports auth valid", http.StatusOK, true, true},
		{"201 reports auth valid", http.StatusCreated, true, true},
		{"500 is not auth evidence", http.StatusInternalServerError, false, false},
		{"404 is not auth evidence", http.StatusNotFound, false, false},
		{"403 is not auth evidence (only 401 is wired)", http.StatusForbidden, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "key")
			var called bool
			var gotOK bool
			c.OnAuthStatus = func(ok bool) { called = true; gotOK = ok }

			if _, _, err := c.Do(context.Background(), "GET", "/api/agent/self", nil, nil); err != nil {
				t.Fatalf("Do: %v", err)
			}
			if called != tc.wantCalled {
				t.Fatalf("OnAuthStatus called=%v, want %v", called, tc.wantCalled)
			}
			if called && gotOK != tc.wantOK {
				t.Fatalf("OnAuthStatus(ok=%v), want %v", gotOK, tc.wantOK)
			}
		})
	}
}

// TestClient_Do_AuthRecoversAfterSuccessFollowing401 proves the "until a subsequent success
// clears it" half of the contract end to end against a stateful server.
func TestClient_Do_AuthRecoversAfterSuccessFollowing401(t *testing.T) {
	first := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first {
			first = false
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key")
	var states []bool
	c.OnAuthStatus = func(ok bool) { states = append(states, ok) }

	if _, _, err := c.Do(context.Background(), "GET", "/x", nil, nil); err != nil {
		t.Fatalf("Do 1: %v", err)
	}
	if _, _, err := c.Do(context.Background(), "GET", "/x", nil, nil); err != nil {
		t.Fatalf("Do 2: %v", err)
	}
	if len(states) != 2 || states[0] != false || states[1] != true {
		t.Fatalf("expected [false true], got %v", states)
	}
}

// TestClient_ConcurrentSetKeyAndDo_NoRace proves the keyguard rotation-swap contract is race-free:
// keyguard's Guard sidecar calls SetKey from its own goroutine while the poll loop, dispatcher,
// sweep, and settings-refresh goroutines all call Do concurrently on the SAME shared *Client. This
// gate is meant to fail under `-race` if Client.key is ever read/written outside the keyMu lock
// again.
func TestClient_ConcurrentSetKeyAndDo_NoRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "key-0")

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: simulates keyguard's rotation swap firing repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
				i++
				c.SetKey(fmt.Sprintf("key-%d", i))
			}
		}
	}()

	// Readers: simulate the poll loop / dispatcher / sweep / settings-refresh goroutines all
	// sharing this one Client.
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if _, _, err := c.Do(context.Background(), "GET", "/x", nil, nil); err != nil {
					t.Errorf("Do: %v", err)
					return
				}
			}
		}()
	}

	// Let the readers run their fixed iteration count, then stop the writer.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func TestErrorMessage_PrefersErrorFieldThenMessageThenBody(t *testing.T) {
	if got := ErrorMessage([]byte(`{"error":"bad thing"}`)); got != "bad thing" {
		t.Errorf("got %q", got)
	}
	if got := ErrorMessage([]byte(`{"message":"nope"}`)); got != "nope" {
		t.Errorf("got %q", got)
	}
	if got := ErrorMessage([]byte(`plain text`)); got != "plain text" {
		t.Errorf("got %q", got)
	}
	if got := ErrorMessage([]byte(``)); got != "(no error body)" {
		t.Errorf("got %q", got)
	}
}
