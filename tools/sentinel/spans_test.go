package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testTrace = "5b8efff798038103d269b633813fc60c"
	testOccID = "11111111-2222-4333-8444-555555555555"
)

// routeServer answers by "METHOD path"; unknown routes are 500. It records the requests it saw.
func routeServer(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		if h, ok := routes[r.Method+" "+r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.Error(w, `{"message":"unexpected"}`, 500)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func reply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func fastPoll(t *testing.T) {
	prev := spansPollInterval
	spansPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { spansPollInterval = prev })
}

func TestIssuesOccurrences_WithSpansSendsFilterAndShowsStatusColumn(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"occurrences":[{"id":"o1","spansStatus":"available","hasSpans":true},{"id":"o2","spansStatus":"pending","hasSpans":false}],"spansScan":{"scanned":2000,"exhausted":false},"nextBefore":"2026-10-01T00:00:00.000Z"}`)
	e, out, errBuf := newTestEnv(srv, "k")
	e.format = "table"
	if code := cmdIssues(context.Background(), e, []string{"occurrences", "11111111-1111-4111-8111-111111111111", "--with-spans", "--limit", "5"}); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errBuf)
	}
	if !strings.Contains(rec.Query, "hasSpans=true") || !strings.Contains(rec.Query, "limit=5") {
		t.Fatalf("query = %q", rec.Query)
	}
	if !strings.Contains(out.String(), "spansStatus") || !strings.Contains(out.String(), "pending") {
		t.Fatalf("table lacks the spans status column:\n%s", out)
	}
	if !strings.Contains(errBuf.String(), "--before 2026-10-01T00:00:00.000Z") {
		t.Fatalf("no continuation hint on stderr: %q", errBuf)
	}
}

func TestIssuesOccurrences_WithoutFlagSendsNoFilter(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"occurrences":[]}`)
	e, _, _ := newTestEnv(srv, "k")
	cmdIssues(context.Background(), e, []string{"occurrences", "11111111-1111-4111-8111-111111111111"})
	if strings.Contains(rec.Query, "hasSpans") {
		t.Fatalf("query = %q", rec.Query)
	}
}

func TestSpansPull_ByTraceIdPostsAndPrintsState(t *testing.T) {
	srv, seen := routeServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/agent/traces/" + testTrace + "/pull": reply(200, `{"status":"pending","reason":null,"action":"queued","requestId":"r1"}`),
	})
	e, out, errBuf := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", testTrace}); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errBuf)
	}
	if !strings.Contains(out.String(), `"action": "queued"`) || len(*seen) != 1 {
		t.Fatalf("out=%s seen=%v", out, *seen)
	}
}

func TestSpansPull_ByOccurrenceIdResolvesTheTrace(t *testing.T) {
	srv, seen := routeServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/agent/occurrences/" + testOccID:        reply(200, `{"occurrence":{"id":"x","traceId":"`+testTrace+`"}}`),
		"POST /api/agent/traces/" + testTrace + "/pull": reply(200, `{"status":"pending","reason":null,"action":"queued","requestId":"r1"}`),
	})
	e, _, errBuf := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", strings.ToUpper(testOccID)}); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errBuf)
	}
	if len(*seen) != 2 {
		t.Fatalf("seen=%v", *seen)
	}
}

func TestSpansPull_OccurrenceWithoutTraceIsRefusedBeforePulling(t *testing.T) {
	srv, seen := routeServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/agent/occurrences/" + testOccID: reply(200, `{"occurrence":{"id":"x","traceId":null}}`),
	})
	e, _, errBuf := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", testOccID}); code != ExitValidation {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errBuf.String(), "no canonical trace id") || len(*seen) != 1 {
		t.Fatalf("err=%q seen=%v", errBuf, *seen)
	}
}

func TestSpansPull_BadArgumentIsUsage(t *testing.T) {
	srv, _ := newRecordingServer(t, 200, `{}`)
	e, _, _ := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", "nope"}); code != ExitUsage {
		t.Fatalf("exit %d", code)
	}
}

func TestSpansPull_WaitUntilAvailable(t *testing.T) {
	fastPoll(t)
	polls := 0
	srv, _ := routeServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/agent/traces/" + testTrace + "/pull": reply(200, `{"status":"pending","reason":null,"action":"queued","requestId":"r1"}`),
		"GET /api/agent/traces/" + testTrace: func(w http.ResponseWriter, r *http.Request) {
			polls++
			st := "pending"
			if polls >= 3 {
				st = "available"
			}
			fmt.Fprintf(w, `{"occurrences":[],"spans":{"status":%q,"reason":null}}`, st)
		},
	})
	e, out, errBuf := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", testTrace, "--wait", "--timeout", "10s"}); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errBuf)
	}
	if polls != 3 || !strings.Contains(out.String(), "spans available") {
		t.Fatalf("polls=%d out=%q", polls, out)
	}
}

func TestSpansPull_WaitTerminalNoneExitsNotFoundWithReason(t *testing.T) {
	fastPoll(t)
	srv, _ := routeServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/agent/traces/" + testTrace + "/pull": reply(200, `{"status":"pending","reason":null,"action":"queued","requestId":"r1"}`),
		"GET /api/agent/traces/" + testTrace:            reply(200, `{"occurrences":[],"spans":{"status":"none","reason":"not_found"}}`),
	})
	e, _, errBuf := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", testTrace, "--wait"}); code != ExitNotFound {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errBuf.String(), "not_found") {
		t.Fatalf("err=%q", errBuf)
	}
}

func TestSpansPull_WaitTimesOut(t *testing.T) {
	fastPoll(t)
	srv, _ := routeServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/agent/traces/" + testTrace + "/pull": reply(200, `{"status":"pending","reason":null,"action":"queued","requestId":"r1"}`),
		"GET /api/agent/traces/" + testTrace:            reply(200, `{"occurrences":[],"spans":{"status":"pending","reason":null}}`),
	})
	e, _, errBuf := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", testTrace, "--wait", "--timeout", "60ms"}); code != ExitNetwork {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errBuf.String(), "timed out") {
		t.Fatalf("err=%q", errBuf)
	}
}

func TestSpansPull_RateLimitedShowsRetryAfter(t *testing.T) {
	srv, _ := routeServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/agent/traces/" + testTrace + "/pull": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"message":"used its 30 on-demand span pulls"}`)
		},
	})
	e, _, errBuf := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", testTrace}); code != ExitNetwork {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(errBuf.String(), "429") || !strings.Contains(errBuf.String(), "retry after 120s") {
		t.Fatalf("err=%q", errBuf)
	}
}

func TestSpansPull_NotEnabledMapsToAuthExit(t *testing.T) {
	srv, _ := routeServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/agent/traces/" + testTrace + "/pull": reply(403, `{"message":"Span pull is not enabled for this organization"}`),
	})
	e, _, errBuf := newTestEnv(srv, "k")
	if code := cmdSpans(context.Background(), e, []string{"pull", testTrace}); code != ExitAuth {
		t.Fatalf("exit %d: %s", code, errBuf)
	}
}
