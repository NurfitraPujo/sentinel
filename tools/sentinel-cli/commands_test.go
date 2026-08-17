package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestEnv builds an env pointed at srv with the given key, capturing stdout/stderr into
// buffers the test can inspect.
func newTestEnv(srv *httptest.Server, key string) (*env, *bytes.Buffer, *bytes.Buffer) {
	var out, errBuf bytes.Buffer
	e := &env{
		client: NewClient(srv.URL, key),
		format: "json",
		stdout: &out,
		stderr: &errBuf,
	}
	return e, &out, &errBuf
}

// recordedRequest captures what the fake server actually received, for assertions.
type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   map[string]interface{}
}

func newRecordingServer(t *testing.T, status int, respBody string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.Method = r.Method
		rec.Path = r.URL.Path
		rec.Query = r.URL.RawQuery
		rec.Auth = r.Header.Get("Authorization")
		if r.Body != nil {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.Body = body
		}
		w.WriteHeader(status)
		fmt.Fprint(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestCommands_MethodPathQueryBodyAndAuth(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantMethod string
		wantPath   string
		wantQuery  map[string]string // key -> expected value (checked via url.ParseQuery)
		wantBody   map[string]interface{}
	}{
		{
			name:       "issues list with filters",
			args:       []string{"issues", "list", "--type", "system_error", "--claimed", "true", "--project", "p1", "--waiting", "true"},
			wantMethod: "GET",
			wantPath:   "/api/agent/issues",
			wantQuery:  map[string]string{"type": "system_error", "claimed": "true", "project": "p1", "waiting": "true"},
		},
		{
			name:       "issues list with pagination flags",
			args:       []string{"issues", "list", "--since", "2026-01-01T00:00:00Z", "--sort", "firstSeen", "--limit", "25", "--cursor", "abc123"},
			wantMethod: "GET",
			wantPath:   "/api/agent/issues",
			wantQuery:  map[string]string{"since": "2026-01-01T00:00:00Z", "sort": "firstSeen", "limit": "25", "cursor": "abc123"},
		},
		{
			name:       "issues get",
			args:       []string{"issues", "get", "iss-1"},
			wantMethod: "GET",
			wantPath:   "/api/agent/issues/iss-1",
		},
		{
			name:       "issues occurrences",
			args:       []string{"issues", "occurrences", "iss-1", "--limit", "5", "--before", "2026-01-01T00:00:00Z"},
			wantMethod: "GET",
			wantPath:   "/api/agent/issues/iss-1/occurrences",
			wantQuery:  map[string]string{"limit": "5", "before": "2026-01-01T00:00:00Z"},
		},
		{
			name:       "claim",
			args:       []string{"claim", "iss-1"},
			wantMethod: "POST",
			wantPath:   "/api/agent/issues/iss-1/claim",
		},
		{
			name:       "release",
			args:       []string{"release", "iss-1"},
			wantMethod: "DELETE",
			wantPath:   "/api/agent/issues/iss-1/claim",
		},
		{
			name:       "status with resolved-in",
			args:       []string{"status", "iss-1", "resolved", "--resolved-in", "v2.3.0"},
			wantMethod: "PATCH",
			wantPath:   "/api/agent/issues/iss-1/status",
			wantBody:   map[string]interface{}{"status": "resolved", "resolved_in_version": "v2.3.0"},
		},
		{
			name:       "comment",
			args:       []string{"comment", "iss-1", "--body", "hello **world**"},
			wantMethod: "POST",
			wantPath:   "/api/agent/issues/iss-1/comments",
			wantBody:   map[string]interface{}{"body_md": "hello **world**"},
		},
		{
			name:       "comment edit",
			args:       []string{"comment", "edit", "iss-1", "c-1", "--body", "revised"},
			wantMethod: "PATCH",
			wantPath:   "/api/agent/issues/iss-1/comments/c-1",
			wantBody:   map[string]interface{}{"body_md": "revised"},
		},
		{
			name:       "comment delete",
			args:       []string{"comment", "delete", "iss-1", "c-1"},
			wantMethod: "DELETE",
			wantPath:   "/api/agent/issues/iss-1/comments/c-1",
		},
		{
			name:       "severity",
			args:       []string{"severity", "iss-1", "high"},
			wantMethod: "PATCH",
			wantPath:   "/api/agent/issues/iss-1/report/severity",
			wantBody:   map[string]interface{}{"severity": "high"},
		},
		{
			name:       "comments list with after",
			args:       []string{"comments", "iss-1", "--after", "2026-01-01T00:00:00Z"},
			wantMethod: "GET",
			wantPath:   "/api/agent/issues/iss-1/comments",
			wantQuery:  map[string]string{"after": "2026-01-01T00:00:00Z"},
		},
		{
			name:       "question",
			args:       []string{"question", "iss-1", "--body", "why?", "--waiting-on", "reporter"},
			wantMethod: "POST",
			wantPath:   "/api/agent/issues/iss-1/questions",
			wantBody:   map[string]interface{}{"body_md": "why?", "audience": "reporter"},
		},
		{
			name:       "progress",
			args:       []string{"progress", "iss-1", "--body", "working on it"},
			wantMethod: "POST",
			wantPath:   "/api/agent/issues/iss-1/progress",
			wantBody:   map[string]interface{}{"message_md": "working on it"},
		},
		{
			name:       "link",
			args:       []string{"link", "iss-1", "iss-2", "--type", "caused_by"},
			wantMethod: "POST",
			wantPath:   "/api/agent/issues/iss-1/relations",
			wantBody:   map[string]interface{}{"target_issue_id": "iss-2", "relation_type": "caused_by"},
		},
		{
			name:       "unlink",
			args:       []string{"unlink", "iss-1", "iss-2", "--type", "linked_to"},
			wantMethod: "DELETE",
			wantPath:   "/api/agent/issues/iss-1/relations",
			wantBody:   map[string]interface{}{"target_issue_id": "iss-2", "relation_type": "linked_to"},
		},
		{
			name:       "projects",
			args:       []string{"projects"},
			wantMethod: "GET",
			wantPath:   "/api/agent/projects",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := newRecordingServer(t, 200, `{"ok":true}`)
			e, _, errBuf := newTestEnv(srv, "sk_test_key")

			code := commands[tc.args[0]](context.Background(), e, tc.args[1:])
			if code != ExitOK {
				t.Fatalf("exit code = %d, want 0; stderr=%s", code, errBuf.String())
			}
			if rec.Method != tc.wantMethod {
				t.Errorf("method = %s, want %s", rec.Method, tc.wantMethod)
			}
			if rec.Path != tc.wantPath {
				t.Errorf("path = %s, want %s", rec.Path, tc.wantPath)
			}
			if rec.Auth != "Bearer sk_test_key" {
				t.Errorf("Authorization = %q, want Bearer sk_test_key", rec.Auth)
			}
			for k, v := range tc.wantQuery {
				q := parseQuery(t, rec.Query)
				if q.Get(k) != v {
					t.Errorf("query[%s] = %q, want %q (full query: %s)", k, q.Get(k), v, rec.Query)
				}
			}
			for k, v := range tc.wantBody {
				if got := rec.Body[k]; got != v {
					t.Errorf("body[%s] = %v, want %v (full body: %v)", k, got, v, rec.Body)
				}
			}
		})
	}
}

func parseQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	vals, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parsing query %q: %v", raw, err)
	}
	return vals
}

func TestExitCodeMapping(t *testing.T) {
	cases := []struct {
		status int
		want   int
	}{
		{200, ExitOK},
		{201, ExitOK},
		{400, ExitValidation},
		{401, ExitAuth},
		{403, ExitAuth},
		{404, ExitNotFound},
		{409, ExitConflict},
		{422, ExitValidation},
		{500, ExitNetwork},
	}
	for _, tc := range cases {
		srv, _ := newRecordingServer(t, tc.status, `{"error":"boom"}`)
		e, _, errBuf := newTestEnv(srv, "k")

		code := cmdClaim(context.Background(), e, []string{"iss-1"})
		if code != tc.want {
			t.Errorf("status %d: exit code = %d, want %d", tc.status, code, tc.want)
		}
		if tc.want != ExitOK && !strings.Contains(errBuf.String(), "boom") {
			t.Errorf("status %d: stderr %q does not contain server error message", tc.status, errBuf.String())
		}
	}
}

func TestBatch_Stdin(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"results":[{"ok":true,"status":200,"result":{}}],"completed":1}`)
	e, out, _ := newTestEnv(srv, "k")

	oldStdin := os.Stdin
	r, w, _ := os.Pipe()
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()
	go func() {
		_, _ = w.WriteString(`{"operations":[{"op":"issues.claim","issueId":"iss-1"}]}`)
		w.Close()
	}()

	code := cmdBatch(context.Background(), e, []string{"-f", "-"})
	if code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if rec.Method != "POST" || rec.Path != "/api/agent/batch" {
		t.Fatalf("unexpected request: %s %s", rec.Method, rec.Path)
	}
	ops, ok := rec.Body["operations"].([]interface{})
	if !ok || len(ops) != 1 {
		t.Fatalf("operations = %v", rec.Body["operations"])
	}
	if out.Len() == 0 {
		t.Errorf("expected output, got none")
	}
}

func TestBatch_StopOnErrorFlag(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"results":[],"completed":0}`)
	e, _, _ := newTestEnv(srv, "k")

	path := filepath.Join(t.TempDir(), "ops.json")
	if err := os.WriteFile(path, []byte(`{"operations":[{"op":"issues.claim","issueId":"iss-1"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	code := cmdBatch(context.Background(), e, []string{"-f", path, "--stop-on-error=false"})
	if code != ExitOK {
		t.Fatalf("exit code = %d", code)
	}
	if rec.Body["stopOnError"] != false {
		t.Errorf("stopOnError = %v, want false", rec.Body["stopOnError"])
	}
}

func TestConfigPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "sentinel"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "sentinel", "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"url":"https://file.example","agent_key":"file_key"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// File alone.
	t.Setenv("SENTINEL_URL", "")
	t.Setenv("SENTINEL_AGENT_KEY", "")
	cfg, err := resolveConfig("", "", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://file.example" || cfg.Key != "file_key" {
		t.Errorf("file-only config = %+v", cfg)
	}

	// Env overrides file.
	t.Setenv("SENTINEL_URL", "https://env.example")
	t.Setenv("SENTINEL_AGENT_KEY", "env_key")
	cfg, err = resolveConfig("", "", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://env.example" || cfg.Key != "env_key" {
		t.Errorf("env-over-file config = %+v", cfg)
	}

	// Flags override everything.
	cfg, err = resolveConfig("https://flag.example", "flag_key", func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.URL != "https://flag.example" || cfg.Key != "flag_key" {
		t.Errorf("flag-over-all config = %+v", cfg)
	}
}

func TestConfigFile_WorldReadableWarns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("SENTINEL_URL", "")
	t.Setenv("SENTINEL_AGENT_KEY", "")
	if err := os.MkdirAll(filepath.Join(dir, "sentinel"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "sentinel", "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"url":"https://file.example","agent_key":"file_key"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var warned string
	_, err := resolveConfig("", "", func(msg string) { warned = msg })
	if err != nil {
		t.Fatal(err)
	}
	if warned == "" {
		t.Errorf("expected a permissions warning for a 0644 config file, got none")
	}
	if !strings.Contains(warned, "600") {
		t.Errorf("warning %q does not mention chmod 600", warned)
	}
}

// N7b (A02): `issues list` with no pagination flags must send NO since/sort/limit/cursor query
// params at all -- byte-identical to pre-N7b request shape, matching the server's "params absent
// ⇒ byte-identical current behavior" contract. Deleting the `if *since != ""` (etc.) guards in
// cmdIssuesList and always calling q.Set would fail this test.
func TestCmdIssuesList_NoFlagsOmitsPaginationParams(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"issues":[]}`)
	e, _, errBuf := newTestEnv(srv, "sk_test_key")

	code := cmdIssues(context.Background(), e, []string{"list"})
	if code != ExitOK {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, errBuf.String())
	}
	if rec.Query != "" {
		t.Errorf("query = %q, want empty (no pagination params sent when no flags given)", rec.Query)
	}
}

func TestGoldenOutput_IssuesList(t *testing.T) {
	body := `{"issues":[{"id":"iss-1","title":"NPE in checkout","status":"unresolved"},{"id":"iss-2","title":"Timeout","status":"resolved"}]}`

	t.Run("json", func(t *testing.T) {
		srv, _ := newRecordingServer(t, 200, body)
		e, out, _ := newTestEnv(srv, "k")
		e.format = "json"
		code := cmdIssues(context.Background(), e, []string{"list"})
		if code != ExitOK {
			t.Fatalf("exit = %d", code)
		}
		want := "{\n  \"issues\": [\n    {\n      \"id\": \"iss-1\",\n      \"title\": \"NPE in checkout\",\n      \"status\": \"unresolved\"\n    },\n    {\n      \"id\": \"iss-2\",\n      \"title\": \"Timeout\",\n      \"status\": \"resolved\"\n    }\n  ]\n}\n"
		if out.String() != want {
			t.Errorf("json output mismatch:\ngot:  %q\nwant: %q", out.String(), want)
		}
	})

	t.Run("table", func(t *testing.T) {
		srv, _ := newRecordingServer(t, 200, body)
		e, out, _ := newTestEnv(srv, "k")
		e.format = "table"
		code := cmdIssues(context.Background(), e, []string{"list"})
		if code != ExitOK {
			t.Fatalf("exit = %d", code)
		}
		got := out.String()
		if !strings.Contains(got, "id") || !strings.Contains(got, "title") || !strings.Contains(got, "status") {
			t.Errorf("table header missing columns: %q", got)
		}
		if !strings.Contains(got, "iss-1") || !strings.Contains(got, "NPE in checkout") {
			t.Errorf("table missing row data: %q", got)
		}
	})
}

func TestEventsFollow_TwoPollsAdvanceCursorAndPersist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	var call int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		switch call {
		case 1:
			fmt.Fprint(w, `{"events":[{"seq":1,"eventType":"issue.created"},{"seq":2,"eventType":"issue.commented"}],"cursor":2,"hasMore":false}`)
		default:
			fmt.Fprint(w, `{"events":[{"seq":3,"eventType":"issue.claimed"}],"cursor":3,"hasMore":false}`)
		}
	}))
	defer srv.Close()

	e, out, _ := newTestEnv(srv, "sk_follow_key")

	oldTick := tick
	defer func() { tick = oldTick }()
	tickCh := make(chan time.Time, 1)
	tick = func(int) <-chan time.Time { return tickCh }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runEventsFollow(ctx, e, 0, "", "", false, 10)
	}()

	// let the first poll happen
	waitFor(t, func() bool { return call >= 1 })
	// trigger the second poll
	tickCh <- time.Now()
	waitFor(t, func() bool { return call >= 2 })
	cancel()

	code := <-done
	if code != ExitOK {
		t.Fatalf("runEventsFollow exit = %d", code)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 NDJSON lines (2 + 1), got %d: %q", len(lines), out.String())
	}

	path, err := cursorStatePath()
	if err != nil {
		t.Fatal(err)
	}
	st, err := loadCursorState(path)
	if err != nil {
		t.Fatal(err)
	}
	key := cursorKey(e.client.BaseURL, e.client.Key)
	if st.Cursors[key] != 3 {
		t.Errorf("persisted cursor = %d, want 3", st.Cursors[key])
	}

	// Restart: a fresh load (simulating a new process) must pick up the persisted cursor.
	st2, err := loadCursorState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Cursors[key] != 3 {
		t.Errorf("cursor did not survive restart: got %d, want 3", st2.Cursors[key])
	}
}

// A08/A09 (N7e): usage-level guards must fail fast (ExitUsage) without ever hitting the network.
func TestCommentEditAndSeverity_UsageGuards(t *testing.T) {
	cases := []struct {
		name string
		cmd  cmdFunc
		args []string
	}{
		{"comment edit missing --body", cmdComment, []string{"edit", "iss-1", "c-1"}},
		{"comment edit wrong arity", cmdComment, []string{"edit", "iss-1", "--body", "x"}},
		{"comment delete wrong arity", cmdComment, []string{"delete", "iss-1"}},
		{"severity invalid level", cmdSeverity, []string{"iss-1", "urgent"}},
		{"severity wrong arity", cmdSeverity, []string{"iss-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("unexpected network call for a usage error: %s %s", r.Method, r.URL.Path)
			}))
			t.Cleanup(srv.Close)
			e, _, _ := newTestEnv(srv, "k")

			code := tc.cmd(context.Background(), e, tc.args)
			if code != ExitUsage {
				t.Errorf("exit code = %d, want ExitUsage (%d)", code, ExitUsage)
			}
		})
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// R1a (N7f): whoami now calls GET /api/agent/self and prints the real identity, not a reachability
// probe against /api/agent/issues.
func TestWhoami_CallsSelfAndPrintsIdentity(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"agentId":"ag-1","name":"Triage Bot","organizationId":"org-1","key":{"id":"k-1","prefix":"sent_agent_","expiresAt":null,"lastUsedAt":null}}`)
	e, out, _ := newTestEnv(srv, "sent_agent_secret")

	code := cmdWhoami(context.Background(), e, nil)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want ExitOK", code)
	}
	if rec.Method != "GET" || rec.Path != "/api/agent/self" {
		t.Errorf("request = %s %s, want GET /api/agent/self", rec.Method, rec.Path)
	}
	if rec.Auth != "Bearer sent_agent_secret" {
		t.Errorf("Authorization = %q", rec.Auth)
	}
	if !strings.Contains(out.String(), "ag-1") || !strings.Contains(out.String(), "Triage Bot") {
		t.Errorf("stdout %q does not contain the identity fields from /self", out.String())
	}
}

func TestWhoami_AuthFailureReportsStatus(t *testing.T) {
	srv, _ := newRecordingServer(t, 401, `{"message":"Invalid API key"}`)
	e, out, _ := newTestEnv(srv, "bad-key")

	code := cmdWhoami(context.Background(), e, nil)
	if code != ExitAuth {
		t.Fatalf("exit code = %d, want ExitAuth", code)
	}
	if !strings.Contains(out.String(), "auth: failed") {
		t.Errorf("stdout %q does not report auth failure", out.String())
	}
}

// R1b (N7f): `sentinel key rotate` prints the new secret to stdout exactly once and a warning to
// stderr, and never echoes the old key.
func TestKeyRotate_PrintsNewSecretOnceWithWarning(t *testing.T) {
	srv, rec := newRecordingServer(t, 200, `{"success":true,"oldKey":{"id":"k-1","expiresAt":"2026-08-15T00:00:00Z"},"newKey":{"id":"k-2","prefix":"sent_agent_","secret":"sent_agent_brandnew"}}`)
	e, out, errBuf := newTestEnv(srv, "sent_agent_old")

	code := cmdKey(context.Background(), e, []string{"rotate"})
	if code != ExitOK {
		t.Fatalf("exit code = %d, want ExitOK; stderr=%s", code, errBuf.String())
	}
	if rec.Method != "POST" || rec.Path != "/api/agent/key/rotate" {
		t.Errorf("request = %s %s, want POST /api/agent/key/rotate", rec.Method, rec.Path)
	}
	stdout := strings.TrimSpace(out.String())
	if stdout != "sent_agent_brandnew" {
		t.Errorf("stdout = %q, want exactly the new secret", stdout)
	}
	if strings.Count(out.String(), "sent_agent_brandnew") != 1 {
		t.Errorf("new secret must appear exactly once in stdout, got: %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "WARNING") {
		t.Errorf("stderr %q does not carry the one-time-reveal warning", errBuf.String())
	}
	if strings.Contains(out.String(), "sent_agent_old") || strings.Contains(errBuf.String(), "sent_agent_old") {
		t.Error("the OLD key must never be echoed by rotate")
	}
}

func TestKey_UnknownSubcommandIsUsageError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected network call: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	e, _, _ := newTestEnv(srv, "k")

	if code := cmdKey(context.Background(), e, []string{"revoke"}); code != ExitUsage {
		t.Errorf("exit code = %d, want ExitUsage", code)
	}
	if code := cmdKey(context.Background(), e, nil); code != ExitUsage {
		t.Errorf("exit code = %d, want ExitUsage", code)
	}
}

// A15 (N7f): `sentinel upload <file> --issue <id> --comment "text"` uploads, then posts exactly
// one comment on that issue with the returned attachment id.
func TestUpload_OneShotWithIssueAndComment(t *testing.T) {
	var requests []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		if r.URL.Path == "/api/agent/uploads" {
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":"att-1","url":"https://x/att-1","filename":"f.txt","contentType":"text/plain","sizeBytes":3}`)
			requests = append(requests, rec)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.Body = body
		requests = append(requests, rec)
		w.WriteHeader(201)
		fmt.Fprint(w, `{"comment":{"id":"c-1"}}`)
	}))
	t.Cleanup(srv.Close)
	e, _, errBuf := newTestEnv(srv, "k")

	tmp := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(tmp, []byte("hey"), 0o600); err != nil {
		t.Fatal(err)
	}

	code := cmdUpload(context.Background(), e, []string{tmp, "--issue", "iss-1", "--comment", "see attached"})
	if code != ExitOK {
		t.Fatalf("exit code = %d, want ExitOK; stderr=%s", code, errBuf.String())
	}
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want 2 (upload then comment)", len(requests))
	}
	upload, comment := requests[0], requests[1]
	if upload.Method != "POST" || upload.Path != "/api/agent/uploads" {
		t.Errorf("first request = %s %s, want POST /api/agent/uploads", upload.Method, upload.Path)
	}
	if comment.Method != "POST" || comment.Path != "/api/agent/issues/iss-1/comments" {
		t.Errorf("second request = %s %s, want POST /api/agent/issues/iss-1/comments", comment.Method, comment.Path)
	}
	if comment.Body["body_md"] != "see attached" {
		t.Errorf("comment body_md = %v, want %q", comment.Body["body_md"], "see attached")
	}
	ids, ok := comment.Body["attachment_ids"].([]interface{})
	if !ok || len(ids) != 1 || ids[0] != "att-1" {
		t.Errorf("comment attachment_ids = %v, want [\"att-1\"]", comment.Body["attachment_ids"])
	}
}

// A15: the old two-positional form still works (plain upload, no comment), but now warns.
func TestUpload_DeprecatedTwoPositionalFormStillWorksAndWarns(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/agent/uploads" {
			t.Fatalf("unexpected request %s %s (old form must never comment)", r.Method, r.URL.Path)
		}
		w.WriteHeader(201)
		fmt.Fprint(w, `{"id":"att-1","url":"https://x/att-1","filename":"f.txt","contentType":"text/plain","sizeBytes":3}`)
	}))
	t.Cleanup(srv.Close)
	e, _, errBuf := newTestEnv(srv, "k")

	tmp := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(tmp, []byte("hey"), 0o600); err != nil {
		t.Fatal(err)
	}

	code := cmdUpload(context.Background(), e, []string{"iss-1", tmp})
	if code != ExitOK {
		t.Fatalf("exit code = %d, want ExitOK; stderr=%s", code, errBuf.String())
	}
	if calls != 1 {
		t.Errorf("got %d requests, want 1 (upload only, no comment)", calls)
	}
	if !strings.Contains(errBuf.String(), "deprecated") {
		t.Errorf("stderr %q does not warn about the deprecated form", errBuf.String())
	}
}

func TestUpload_UsageErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected network call for a usage error: %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	e, _, _ := newTestEnv(srv, "k")

	if code := cmdUpload(context.Background(), e, nil); code != ExitUsage {
		t.Errorf("no args: exit code = %d, want ExitUsage", code)
	}
	if code := cmdUpload(context.Background(), e, []string{"a", "b", "c"}); code != ExitUsage {
		t.Errorf("three positionals: exit code = %d, want ExitUsage", code)
	}
}
