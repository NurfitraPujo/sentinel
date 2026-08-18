package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// discardLogger is a slog.Logger that writes nowhere, so WARN/ERROR assertions below use their
// own capturing handler instead of polluting test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 100}))
}

// capturingLogger returns a logger whose records are appended (as rendered text) to *buf, so a
// test can assert a WARN/ERROR fired with the expected content.
func capturingLogger(buf *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// fakeServer builds an httptest.Server serving GET /api/agent/projects and
// GET /api/agent/repo-credentials from the given handlers, and a sentinel.Client (satisfying this
// package's Client interface) pointed at it -- the real wire client, real JSON encoding, per repo
// convention (loop package's httptest-based tests).
func fakeServer(t *testing.T, projects http.HandlerFunc, credentials http.HandlerFunc) (*sentinel.Client, func()) {
	t.Helper()
	mux := http.NewServeMux()
	if projects != nil {
		mux.HandleFunc("/api/agent/projects", projects)
	}
	if credentials != nil {
		mux.HandleFunc("/api/agent/repo-credentials", credentials)
	}
	srv := httptest.NewServer(mux)
	c := sentinel.NewClient(srv.URL, "test-key")
	return c, srv.Close
}

func jsonHandler(t *testing.T, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const projectsWithRepoJSON = `{"projects":[
  {"id":"p1","name":"Proj One","agentSettings":{"fixEnabled":true,"maxPrsPerDay":5,
    "repo":{"provider":"github","owner":"acme","repo":"widgets","defaultBranch":"main","testCmd":"go test ./...","agentCmd":"claude","cloneDepth":1}}},
  {"id":"p2","name":"Proj Two","agentSettings":{"fixEnabled":false,"maxPrsPerDay":null,"repo":null}}
]}`

const projectsNoRepoJSON = `{"projects":[
  {"id":"p1","name":"Proj One","agentSettings":{"fixEnabled":false,"maxPrsPerDay":null,"repo":null}}
]}`

func TestRefresh_ProjectsOnly_NoRepoConnection_SkipsCredentialsFetch(t *testing.T) {
	credentialsCalled := false
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsNoRepoJSON),
		func(w http.ResponseWriter, r *http.Request) { credentialsCalled = true; w.WriteHeader(200) },
	)
	defer closeFn()

	s := NewStore()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if credentialsCalled {
		t.Fatal("GET /api/agent/repo-credentials must not be called when no project has a repo connection")
	}
	p, ok := s.Project("p1")
	if !ok || p.FixEnabled {
		t.Fatalf("expected p1 fixEnabled=false, got %+v ok=%v", p, ok)
	}
}

func TestRefresh_SingleCredentialPerProvider_Resolved(t *testing.T) {
	credsJSON := `{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"ghp_abc123"}}]}`
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, credsJSON),
	)
	defer closeFn()

	s := NewStore()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	cred, ok := s.CredentialFor("github")
	if !ok {
		t.Fatal("expected a github credential to be resolved")
	}
	if cred.Secret.Token != "ghp_abc123" {
		t.Fatalf("expected token ghp_abc123, got %q", cred.Secret.Token)
	}

	p, ok := s.Project("p1")
	if !ok {
		t.Fatal("expected p1")
	}
	if !p.FixReady() {
		t.Fatalf("expected p1 to be FixReady (fixEnabled+repo), got %+v", p)
	}
	if p.MaxPRsPerDay == nil || *p.MaxPRsPerDay != 5 {
		t.Fatalf("expected maxPrsPerDay=5, got %v", p.MaxPRsPerDay)
	}
}

// TestRefresh_EmptyOrMalformedSecret_NotUsable_FallsBackToEnv is the validator-finding regression:
// a server credential row whose `secret` is `{}`, `null`, or a half-populated username/appPassword
// pair must never resolve into Store.credentials, must never suppress a working env fallback, and
// must never report as available on /readyz. Mutation test: delete the `!c.Secret.Usable()` guard
// in Refresh (settings.go) and every one of these subtests goes red (resolved=true, env
// suppressed).
func TestRefresh_EmptyOrMalformedSecret_NotUsable_FallsBackToEnv(t *testing.T) {
	cases := []struct {
		name   string
		secret string
	}{
		{"EmptyObject", `{}`},
		{"ExplicitNull", `null`},
		{"EmptyToken", `{"token":""}`},
		{"HalfUserPass", `{"username":"u","appPassword":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			credsJSON := `{"credentials":[{"id":"c1","provider":"github","label":"default","secret":` + tc.secret + `}]}`
			client, closeFn := fakeServer(t,
				jsonHandler(t, 200, projectsWithRepoJSON),
				jsonHandler(t, 200, credsJSON),
			)
			defer closeFn()

			s := NewStore()
			env := EnvFallback{GitHubToken: "WORKING-ENV-TOKEN"}
			if err := s.Refresh(context.Background(), client, nil, env, discardLogger()); err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			cred, ok := s.CredentialFor("github")
			if !ok {
				t.Fatal("expected env fallback to resolve a github credential")
			}
			if cred.Secret.Token != "WORKING-ENV-TOKEN" {
				t.Fatalf("expected env fallback token, got %+v (server credential with unusable secret must not win)", cred.Secret)
			}

			rd := s.Readiness()
			for _, p := range rd.Providers {
				if p.Provider != "github" {
					continue
				}
				if !p.Available {
					t.Fatalf("expected github to report available via env fallback, got %+v", p)
				}
			}
			if rd.RepoConnectionsReady != rd.RepoConnectionsTotal {
				t.Fatalf("expected all connections ready via env fallback, got %d/%d", rd.RepoConnectionsReady, rd.RepoConnectionsTotal)
			}
		})
	}
}

// TestCredentialSecret_Usable directly exercises the Usable() predicate the finding required.
func TestCredentialSecret_Usable(t *testing.T) {
	cases := []struct {
		name   string
		secret CredentialSecret
		want   bool
	}{
		{"Token", CredentialSecret{Token: "t"}, true},
		{"UserPass", CredentialSecret{Username: "u", AppPassword: "p"}, true},
		{"Empty", CredentialSecret{}, false},
		{"UserOnly", CredentialSecret{Username: "u"}, false},
		{"AppPasswordOnly", CredentialSecret{AppPassword: "p"}, false},
	}
	for _, tc := range cases {
		if got := tc.secret.Usable(); got != tc.want {
			t.Errorf("%s: Usable() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRefresh_AmbiguousCredentials_WarnsAndMarksUnavailable(t *testing.T) {
	credsJSON := `{"credentials":[
	  {"id":"c1","provider":"github","label":"one","secret":{"token":"tok-one"}},
	  {"id":"c2","provider":"github","label":"two","secret":{"token":"tok-two"}}
	]}`
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, credsJSON),
	)
	defer closeFn()

	var logbuf strings.Builder
	s := NewStore()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, capturingLogger(&logbuf)); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if _, ok := s.CredentialFor("github"); ok {
		t.Fatal("ambiguous provider must not resolve a credential")
	}
	if reason := s.ProviderUnavailableReason("github"); reason == "" {
		t.Fatal("expected a recorded unavailable reason for the ambiguous provider")
	}
	if !strings.Contains(logbuf.String(), "level=WARN") || !strings.Contains(logbuf.String(), "ambiguous") {
		t.Fatalf("expected a WARN log about ambiguous credentials, got: %s", logbuf.String())
	}
}

func TestRefresh_LabelDisambiguation_Resolves(t *testing.T) {
	credsJSON := `{"credentials":[
	  {"id":"c1","provider":"github","label":"one","secret":{"token":"tok-one"}},
	  {"id":"c2","provider":"github","label":"two","secret":{"token":"tok-two"}}
	]}`
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, credsJSON),
	)
	defer closeFn()

	s := NewStore()
	if err := s.Refresh(context.Background(), client, []string{"github=two"}, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	cred, ok := s.CredentialFor("github")
	if !ok {
		t.Fatal("expected label-disambiguated credential to resolve")
	}
	if cred.Secret.Token != "tok-two" {
		t.Fatalf("expected tok-two (label=two), got %q", cred.Secret.Token)
	}
}

func TestRefresh_UnknownLabel_MarksUnavailable(t *testing.T) {
	credsJSON := `{"credentials":[
	  {"id":"c1","provider":"github","label":"one","secret":{"token":"tok-one"}},
	  {"id":"c2","provider":"github","label":"two","secret":{"token":"tok-two"}}
	]}`
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, credsJSON),
	)
	defer closeFn()

	s := NewStore()
	if err := s.Refresh(context.Background(), client, []string{"github=three"}, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if _, ok := s.CredentialFor("github"); ok {
		t.Fatal("a WORKER_CREDENTIAL_LABELS value naming a nonexistent label must not resolve a credential")
	}
}

func TestRefresh_403_MarksForbidden_AndFallsBackToEnv(t *testing.T) {
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 403, `{"error":"forbidden"}`),
	)
	defer closeFn()

	var logbuf strings.Builder
	s := NewStore()
	env := EnvFallback{GitHubToken: "env-token"}
	if err := s.Refresh(context.Background(), client, nil, env, capturingLogger(&logbuf)); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if !strings.Contains(logbuf.String(), "canAccessRepoCredentials") {
		t.Fatalf("expected a log line naming the canAccessRepoCredentials provisioning hint, got: %s", logbuf.String())
	}
	if !s.Readiness().CredentialsForbidden {
		t.Fatal("expected Readiness().CredentialsForbidden to be true after a 403")
	}
	cred, ok := s.CredentialFor("github")
	if !ok || cred.Secret.Token != "env-token" {
		t.Fatalf("expected env fallback token after 403, got %+v ok=%v", cred, ok)
	}
	// Worker still triages: p1's settings must still be loaded despite the credentials failure.
	if _, ok := s.Project("p1"); !ok {
		t.Fatal("expected project settings to still be present after a repo-credentials 403")
	}
}

func TestRefresh_EnvFallback_OnlyFillsUnresolvedProviders(t *testing.T) {
	credsJSON := `{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"server-token"}}]}`
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, credsJSON),
	)
	defer closeFn()

	s := NewStore()
	env := EnvFallback{GitHubToken: "env-token", BitbucketUser: "bb-user", BitbucketAppPassword: "bb-pass"}
	if err := s.Refresh(context.Background(), client, nil, env, discardLogger()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	ghCred, _ := s.CredentialFor("github")
	if ghCred.Secret.Token != "server-token" {
		t.Fatalf("server-resolved credential must win over env fallback, got %q", ghCred.Secret.Token)
	}
	bbCred, ok := s.CredentialFor("bitbucket")
	if !ok || bbCred.Secret.Username != "bb-user" || bbCred.Secret.AppPassword != "bb-pass" {
		t.Fatalf("expected bitbucket env fallback (no server credential for it), got %+v ok=%v", bbCred, ok)
	}
}

func TestEnvCredentials_TokenPreferredOverUserPass(t *testing.T) {
	env := EnvFallback{BitbucketToken: "bb-tok", BitbucketUser: "u", BitbucketAppPassword: "p"}
	creds := envCredentials(env)
	c, ok := creds["bitbucket"]
	if !ok {
		t.Fatal("expected a bitbucket env credential")
	}
	if c.Secret.Token != "bb-tok" || c.Secret.Username != "" {
		t.Fatalf("expected token form to win, got %+v", c.Secret)
	}
}

func TestReadiness_CountsRepoConnectionsAndAvailability(t *testing.T) {
	credsJSON := `{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"tok"}}]}`
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, credsJSON),
	)
	defer closeFn()

	s := NewStore()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	detail := s.Readiness()
	if detail.RepoConnectionsTotal != 1 {
		t.Fatalf("expected 1 repo connection (p1 only), got %d", detail.RepoConnectionsTotal)
	}
	if detail.RepoConnectionsReady != 1 {
		t.Fatalf("expected 1 ready connection, got %d", detail.RepoConnectionsReady)
	}
	found := false
	for _, p := range detail.Providers {
		if p.Provider == "github" {
			found = true
			if !p.Available {
				t.Fatal("expected github marked available")
			}
		}
	}
	if !found {
		t.Fatal("expected github in the providers list")
	}
}

func TestReadiness_UnavailableProvider_NeverCrashes(t *testing.T) {
	// A connection whose provider has no usable credential at all (no server credential, no env
	// fallback) must report per-project via Readiness, never panic or error.
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, `{"credentials":[]}`),
	)
	defer closeFn()

	s := NewStore()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	detail := s.Readiness()
	if detail.RepoConnectionsReady != 0 {
		t.Fatalf("expected 0 ready connections (no credential at all), got %d", detail.RepoConnectionsReady)
	}
	if detail.RepoConnectionsTotal != 1 {
		t.Fatalf("expected 1 total connection, got %d", detail.RepoConnectionsTotal)
	}
	// p2 (fixEnabled=false, no repo) is unaffected/still present.
	if _, ok := s.Project("p2"); !ok {
		t.Fatal("expected p2 to still be present")
	}

	// Per-project row: p1 must appear, unavailable, with a non-empty diagnostic reason -- an
	// operator reading /readyz must be able to tell WHICH project is unready and why, not just an
	// aggregate count (plan §4.5).
	if len(detail.Projects) != 1 {
		t.Fatalf("expected exactly 1 project row (p1 has a repo, p2 does not), got %d: %+v", len(detail.Projects), detail.Projects)
	}
	row := detail.Projects[0]
	if row.ProjectID != "p1" || row.Provider != "github" {
		t.Fatalf("expected p1/github row, got %+v", row)
	}
	if row.CredentialAvailable {
		t.Fatal("expected p1's row to report CredentialAvailable=false (no credential at all)")
	}
	if row.Reason == "" {
		t.Fatal("expected a non-empty reason for the 'no credential at all' case, got empty string")
	}
}

// TestRefresh_TransientCredentialsFailure_RetainsPreviousCredential proves a single transient
// failure of GET /api/agent/repo-credentials (network error, 5xx, unparseable body) does not
// discard a previously-good server-resolved credential -- a network blip is not evidence of
// revocation (plan §4.5). Table-driven over the three non-authoritative failure shapes.
func TestRefresh_TransientCredentialsFailure_RetainsPreviousCredential(t *testing.T) {
	// "connection error" hijacks and abruptly closes the TCP connection on the second
	// repo-credentials request only (projects keeps answering normally on the same server), which
	// makes sentinel.Client's GetRepoCredentials return a non-nil error -- the credErr != nil
	// branch in Refresh -- without tearing down the whole server.
	hijackClose := func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("test server ResponseWriter does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			panic(err)
		}
		_ = conn.Close()
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"5xx", jsonHandler(t, 500, `{"error":"boom"}`)},
		{"unparseable body", jsonHandler(t, 200, `not json`)},
		{"connection error", hijackClose},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			goodCredsJSON := `{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"good-token"}}]}`
			var callCount int
			mux := http.NewServeMux()
			mux.HandleFunc("/api/agent/projects", jsonHandler(t, 200, projectsWithRepoJSON))
			mux.HandleFunc("/api/agent/repo-credentials", func(w http.ResponseWriter, r *http.Request) {
				callCount++
				if callCount == 1 {
					jsonHandler(t, 200, goodCredsJSON)(w, r)
					return
				}
				tc.handler(w, r)
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			client := sentinel.NewClient(srv.URL, "test-key")

			s := NewStore()
			if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, discardLogger()); err != nil {
				t.Fatalf("first Refresh: %v", err)
			}
			if cred, ok := s.CredentialFor("github"); !ok || cred.Secret.Token != "good-token" {
				t.Fatalf("sanity check: expected good-token after first refresh, got %+v ok=%v", cred, ok)
			}

			var logbuf strings.Builder
			if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, capturingLogger(&logbuf)); err != nil {
				t.Fatalf("second Refresh: %v", err)
			}
			cred, ok := s.CredentialFor("github")
			if !ok || cred.Secret.Token != "good-token" {
				t.Fatalf("expected previous good-token to be RETAINED after a transient failure (%s), got %+v ok=%v; log=%s", tc.name, cred, ok, logbuf.String())
			}
			if _, ok := s.Project("p1"); !ok {
				t.Fatal("expected p1 project settings to still be present after the transient credentials failure")
			}
		})
	}
}

// TestRefresh_403AfterGoodCredential_ClearsIt proves the OPPOSITE side of the transient-retention
// fix: a 403 is an authoritative denial (C16 -- "an org admin must grant canAccessRepoCredentials"),
// not a blip, so it correctly DOES clear a previously-good server credential rather than retaining
// it indefinitely.
func TestRefresh_403AfterGoodCredential_ClearsIt(t *testing.T) {
	goodCredsJSON := `{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"good-token"}}]}`
	var callCount int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/projects", jsonHandler(t, 200, projectsWithRepoJSON))
	mux.HandleFunc("/api/agent/repo-credentials", func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			jsonHandler(t, 200, goodCredsJSON)(w, r)
			return
		}
		jsonHandler(t, 403, `{"error":"forbidden"}`)(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "test-key")

	s := NewStore()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("first Refresh: %v", err)
	}
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if _, ok := s.CredentialFor("github"); ok {
		t.Fatal("expected the 403 to clear the previously-good credential (authoritative denial, not a blip)")
	}
	if !s.Readiness().CredentialsForbidden {
		t.Fatal("expected Readiness().CredentialsForbidden to be true after the 403")
	}
}

// TestRefreshLoop_RunsOnIntervalAndExitsOnCancel drives RefreshLoop itself (not just Refresh) --
// the brief's "refresh loop against httptest" requirement. Uses a short real interval; asserts at
// least two refreshes happen and the goroutine exits promptly once ctx is cancelled.
func TestRefreshLoop_RunsOnIntervalAndExitsOnCancel(t *testing.T) {
	var callCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/projects", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		jsonHandler(t, 200, projectsNoRepoJSON)(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "test-key")

	s := NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RefreshLoop(ctx, s, client, 5*time.Millisecond, nil, EnvFallback{}, discardLogger())
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&callCount) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("expected at least 3 refreshes within 2s, got %d", atomic.LoadInt32(&callCount))
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshLoop did not exit within 2s of ctx cancellation")
	}
}

// TestRefreshLoop_ZeroInterval_NeverPolls_ExitsOnCancel covers RefreshLoop's interval<=0
// early-return branch: it must not call time.NewTicker(0) (which panics) and must still exit
// promptly on ctx cancellation without ever calling Refresh.
func TestRefreshLoop_ZeroInterval_NeverPolls_ExitsOnCancel(t *testing.T) {
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/projects", func(w http.ResponseWriter, r *http.Request) {
		called = true
		jsonHandler(t, 200, projectsNoRepoJSON)(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "test-key")

	s := NewStore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RefreshLoop(ctx, s, client, 0, nil, EnvFallback{}, discardLogger())
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshLoop(interval<=0) did not exit within 2s of ctx cancellation")
	}
	if called {
		t.Fatal("RefreshLoop(interval<=0) must never call Refresh")
	}
}

// TestRefreshLoop_LogsErrorAndKeepsPreviousSnapshot covers RefreshLoop's error-logging branch: a
// ListProjects failure on a later tick logs and leaves the previous snapshot in place rather than
// crashing the loop.
func TestRefreshLoop_LogsErrorAndKeepsPreviousSnapshot(t *testing.T) {
	var callCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/projects", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCount, 1)
		if n == 1 {
			jsonHandler(t, 200, projectsNoRepoJSON)(w, r)
			return
		}
		w.WriteHeader(500)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := sentinel.NewClient(srv.URL, "test-key")

	s := NewStore()
	var logbuf strings.Builder
	var logmu sync.Mutex
	logger := slog.New(slog.NewTextHandler(syncWriter{&logbuf, &logmu}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RefreshLoop(ctx, s, client, 5*time.Millisecond, nil, EnvFallback{}, logger)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&callCount) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("expected at least 2 refresh attempts within 2s, got %d", atomic.LoadInt32(&callCount))
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let the failing tick's log line land
	cancel()
	<-done

	logmu.Lock()
	out := logbuf.String()
	logmu.Unlock()
	if !strings.Contains(out, "settings refresh failed") {
		t.Fatalf("expected RefreshLoop's error-logging branch to fire, got log: %s", out)
	}
	if _, ok := s.Project("p1"); !ok {
		t.Fatal("expected the previous snapshot (p1) to survive the failing refresh")
	}
}

// syncWriter serializes writes to an underlying *strings.Builder so the concurrent slog handler
// used by TestRefreshLoop_LogsErrorAndKeepsPreviousSnapshot is race-safe.
type syncWriter struct {
	b  *strings.Builder
	mu *sync.Mutex
}

func (w syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

// TestMemoryOnly_CredentialNeverReachesLogs is the C16 memory-only proof (task brief), driven
// through a channel this package can actually leak into: the logger Refresh is handed. Every
// error/warn/info call Refresh makes goes through this *slog.Logger, so a mutation that logs the
// resolved secret (e.g. `logger.Info("resolved credential", "token", c.Secret.Token)` right before
// the store commit) is caught here -- unlike a test that walks a state-dir Refresh never writes to
// and therefore cannot fail. Mutation-tested: inserting such a log call into Refresh turns this red.
func TestMemoryOnly_CredentialNeverReachesLogs(t *testing.T) {
	const secretMarker = "SUPER-SECRET-TOKEN-VALUE-DO-NOT-PERSIST-3f9a7c"
	credsJSON := `{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"` + secretMarker + `"}}]}`
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, credsJSON),
	)
	defer closeFn()

	var logbuf strings.Builder
	s := NewStore()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, capturingLogger(&logbuf)); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	cred, ok := s.CredentialFor("github")
	if !ok || cred.Secret.Token != secretMarker {
		t.Fatalf("sanity check: expected the secret to actually be resolved in memory, got %+v ok=%v", cred, ok)
	}
	if strings.Contains(logbuf.String(), secretMarker) {
		t.Fatalf("secret marker leaked into logger output: %s", logbuf.String())
	}

	// Readiness() is the only value this package exposes to callers outside Store.CredentialFor
	// (feeding /readyz JSON and /metrics); it must never carry the secret either.
	detail := s.Readiness()
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal Readiness: %v", err)
	}
	if strings.Contains(string(encoded), secretMarker) {
		t.Fatalf("secret marker leaked into Readiness() JSON: %s", encoded)
	}

	// A second refresh pass (e.g. RefreshLoop's periodic tick) must not leak it either -- proves
	// the guarantee holds across repeated resolution, not just a first pass.
	logbuf.Reset()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, capturingLogger(&logbuf)); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if strings.Contains(logbuf.String(), secretMarker) {
		t.Fatalf("secret marker leaked into logger output on second refresh: %s", logbuf.String())
	}
}

// TestMemoryOnly_CredentialNeverReachesJournalOrStateDir drives the real main.go-style wiring --
// a settings.Store refresh alongside a state.Journal/cursor write cycle sharing the same state
// dir -- and proves the marker never crosses from the Store (memory-only) into any file the
// worker durably writes. Unlike a bare state-dir walk with nothing under test, this asserts the
// two subsystems that DO write to disk (journal, cursor) do so without ever having been handed
// the credential in the first place -- exactly the wiring N8f's askpass helper will extend.
func TestMemoryOnly_CredentialNeverReachesJournalOrStateDir(t *testing.T) {
	const secretMarker = "SUPER-SECRET-TOKEN-VALUE-DO-NOT-PERSIST-3f9a7c"
	credsJSON := `{"credentials":[{"id":"c1","provider":"github","label":"default","secret":{"token":"` + secretMarker + `"}}]}`
	client, closeFn := fakeServer(t,
		jsonHandler(t, 200, projectsWithRepoJSON),
		jsonHandler(t, 200, credsJSON),
	)
	defer closeFn()

	stateDir := t.TempDir()
	s := NewStore()
	if err := s.Refresh(context.Background(), client, nil, EnvFallback{}, discardLogger()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	cred, ok := s.CredentialFor("github")
	if !ok || cred.Secret.Token != secretMarker {
		t.Fatalf("sanity check: expected the secret to actually be resolved in memory, got %+v ok=%v", cred, ok)
	}

	// A full journal cycle: open, append a representative record (built entirely from the record's
	// own fields, never touching cred), compact -- exactly as main.go's
	// runJournalMaintenance/runPipeline do -- this is the "journal cycle" the marker must survive
	// untouched by.
	journalPath := filepath.Join(stateDir, "jobs.journal")
	j := state.OpenJournal(journalPath)
	if err := j.Append(state.Record{
		JobID:   "job-1",
		IssueID: "issue-1",
		Kind:    "TRIAGE",
		State:   state.StateDone,
	}); err != nil {
		t.Fatalf("journal append: %v", err)
	}
	if _, err := j.Compact(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatalf("journal compact: %v", err)
	}

	// Also exercise cursor.json, the other durable file main.go writes to the same state dir --
	// with an unrelated, non-secret value (the point is to prove the marker is absent even after a
	// full write cycle over every file in stateDir, not to inject the marker into it).
	cursorPath := filepath.Join(stateDir, "cursor.json")
	if err := state.SaveCursor(cursorPath, 42); err != nil {
		t.Fatalf("save cursor: %v", err)
	}

	// Walk every file under stateDir and assert the marker is absent.
	err := filepath.Walk(stateDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), secretMarker) {
			t.Errorf("secret marker leaked into state-dir file %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking state dir: %v", err)
	}
}

// TestRedaction_SecretNeverSerializes proves CredentialSecret and Credential resist every common
// Go serialization path -- fmt's %v/%+v/%s, encoding/json, and slog -- per the validator finding
// that the package doc's redaction claim previously had no code backing it.
func TestRedaction_SecretNeverSerializes(t *testing.T) {
	const marker = "SUPER-SECRET-TOKEN-VALUE-DO-NOT-PERSIST-3f9a7c"
	cred := Credential{
		ID:       "c1",
		Provider: "github",
		Label:    "default",
		Secret:   CredentialSecret{Token: marker},
	}

	checks := map[string]string{
		"fmt %v":     fmt.Sprintf("%v", cred),
		"fmt %+v":    fmt.Sprintf("%+v", cred),
		"fmt %s":     fmt.Sprintf("%s", cred),
		"secret %+v": fmt.Sprintf("%+v", cred.Secret),
	}

	jsonBytes, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("json.Marshal(Credential): %v", err)
	}
	checks["json.Marshal(Credential)"] = string(jsonBytes)

	secretJSON, err := json.Marshal(cred.Secret)
	if err != nil {
		t.Fatalf("json.Marshal(CredentialSecret): %v", err)
	}
	checks["json.Marshal(CredentialSecret)"] = string(secretJSON)

	var logBuf strings.Builder
	logger := capturingLogger(&logBuf)
	logger.Info("test credential", "cred", cred, "secret", cred.Secret)
	checks["slog attribute"] = logBuf.String()

	for name, out := range checks {
		if strings.Contains(out, marker) {
			t.Errorf("%s leaked the secret marker: %s", name, out)
		}
	}
	// Sanity: prove the checks above actually exercise real output, not empty strings.
	if !strings.Contains(checks["json.Marshal(Credential)"], "REDACTED") {
		t.Errorf("expected redacted marker in json.Marshal(Credential) output, got %s", checks["json.Marshal(Credential)"])
	}
}

// TestStore_LoggingNeverLeaksCredentialSecrets is the RED-FIRST reproduction of finding 6: fmt/slog
// cannot call String()/LogValue() on a value reached only through an UNEXPORTED field (Store's
// `credentials`/`serverCreds` maps), so slogging or fmt-printing the whole *Store -- or a struct
// embedding one -- bypassed Credential/CredentialSecret's own redaction entirely and printed the
// raw fields, including the plaintext secret, before Store grew its own String()/LogValue().
func TestStore_LoggingNeverLeaksCredentialSecrets(t *testing.T) {
	const marker = "SUPER-SECRET-STORE-LEAK-MARKER-0xC0FFEE"

	s := NewStore()
	s.mu.Lock()
	s.projects["p1"] = ProjectSettings{ProjectID: "p1", Name: "proj", FixEnabled: true}
	s.credentials["github"] = Credential{ID: "c1", Provider: "github", Label: "default", Secret: CredentialSecret{Token: marker}}
	s.serverCreds["github"] = s.credentials["github"]
	s.mu.Unlock()

	// embeddingStruct mirrors the realistic failure mode: a struct that merely EMBEDS *Store (e.g.
	// a future request-context or job type) also picks up Store's own String()/LogValue() once
	// defined, per Go's method-promotion rules -- fmt/slog need not reach Store directly for the
	// leak (or the fix) to apply.
	type embeddingStruct struct {
		*Store
		Other string
	}
	embedded := embeddingStruct{Store: s, Other: "context"}

	checks := map[string]string{
		"fmt %v store":         fmt.Sprintf("%v", s),
		"fmt %+v store":        fmt.Sprintf("%+v", s),
		"fmt %s store":         fmt.Sprintf("%s", s),
		"fmt %+v embedding":    fmt.Sprintf("%+v", embedded),
		"fmt %+v cred (again)": fmt.Sprintf("%+v", s.credentials["github"]),
	}

	var logBuf strings.Builder
	logger := capturingLogger(&logBuf)
	logger.Info("test store", "store", s, "embedding", embedded)
	checks["slog attribute"] = logBuf.String()

	for name, out := range checks {
		if strings.Contains(out, marker) {
			t.Errorf("SECURITY: %s leaked the secret marker: %s", name, out)
		}
	}
}

// MUTATION-TEST NOTE (finding 6): to prove Store.String/Store.LogValue are load-bearing,
// temporarily delete both methods (or their bodies, replacing with the zero value), re-run
// TestStore_LoggingNeverLeaksCredentialSecrets — it must go red (fmt falls back to reflecting into
// the unexported credentials map) — then revert.

// TestRedaction_GoStringNeverLeaksSecret is the RED-FIRST reproduction of the re-attack finding
// (N8c finding 3): fmt's "%#v" verb (GoStringer) dumps a struct's fields verbatim via reflection
// and is NOT intercepted by String(), LogValue(), or MarshalJSON -- only by a type's own
// GoString() method. Before CredentialSecret/Credential/*Store grew GoString(), "%#v" on any of
// the three printed the raw Token/Username/AppPassword straight past every other redaction path
// this package has (TestRedaction_SecretNeverSerializes and TestStore_LoggingNeverLeaksCredentialSecrets
// above only exercise %v/%+v/%s/json/slog, none of which cover %#v).
func TestRedaction_GoStringNeverLeaksSecret(t *testing.T) {
	const marker = "SUPER-SECRET-GOSTRING-LEAK-MARKER-7e2f91"

	secret := CredentialSecret{Token: marker}
	cred := Credential{ID: "c1", Provider: "github", Label: "default", Secret: secret}

	s := NewStore()
	s.mu.Lock()
	s.projects["p1"] = ProjectSettings{ProjectID: "p1", Name: "proj", FixEnabled: true}
	s.credentials["github"] = cred
	s.serverCreds["github"] = cred
	s.mu.Unlock()

	checks := map[string]string{
		"CredentialSecret %#v": fmt.Sprintf("%#v", secret),
		"Credential %#v":       fmt.Sprintf("%#v", cred),
		"*Store %#v":           fmt.Sprintf("%#v", s),
	}

	for name, out := range checks {
		if strings.Contains(out, marker) {
			t.Errorf("SECURITY: %s leaked the secret marker via fmt's %%#v (GoStringer) verb: %s", name, out)
		}
	}
}

// MUTATION-TEST NOTE (re-attack finding 3, %#v/GoStringer) -- verified by actually deleting each
// method in turn and re-running TestRedaction_GoStringNeverLeaksSecret:
//   - Deleting CredentialSecret.GoString: goes RED on "CredentialSecret %#v" (Credential %#v and
//     *Store %#v stayed green in this isolated case -- Credential.GoString hardcodes
//     "Secret:[REDACTED]" without recursing into Secret's own formatting, so it doesn't depend on
//     CredentialSecret.GoString existing).
//   - Deleting Credential.GoString alone: stays GREEN -- %#v's struct recursion still calls the
//     now-remaining CredentialSecret.GoString on the (exported) Secret field, so the secret stays
//     hidden even without Credential's own override. (Confirms Credential.GoString is redundant
//     defense-in-depth here, not the sole guard -- unlike the other two.)
//   - Deleting (*Store).GoString alone: goes RED on "*Store %#v", dumping the FULL unexported
//     `credentials`/`serverCreds` map structure with plaintext tokens -- reflecting through an
//     unexported field disables method dispatch for %#v exactly as it does for String()/LogValue()
//     (see finding 6 above), so nested CredentialSecret.GoString does NOT save it once Store's own
//     GoString is gone.
// Net: CredentialSecret.GoString and (*Store).GoString are each independently load-bearing;
// Credential.GoString is redundant given the other two but kept for defense-in-depth per the
// finding's instruction to add it on all three types.
