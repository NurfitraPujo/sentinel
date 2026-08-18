package gitprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

func TestGitHubProvider_Auth(t *testing.T) {
	p := NewGitHubProvider("ghp_supersecrettoken")
	cred := p.Auth()
	if cred.username != "ghp_supersecrettoken" || cred.password != "x-oauth-basic" {
		t.Fatalf("unexpected credential shape: %+v", cred)
	}
}

func TestGitHubProvider_CreatePR_RequestGolden(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotAccept string
	var gotBody githubCreatePRRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/acme/widgets/pull/42","state":"open","merged":false}`))
	}))
	defer srv.Close()

	p := &GitHubProvider{Token: "ghp_thetoken", APIBase: srv.URL, HTTP: srv.Client()}
	pr, err := p.CreatePR(context.Background(), RepoRef{Provider: "github", Owner: "acme", Repo: "widgets"}, PRSpec{
		Title: "fix: null pointer in handler",
		Body:  "Fixes issue #123.",
		Head:  "sentinel/fix-123",
		Base:  "main",
	})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/repos/acme/widgets/pulls" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer ghp_thetoken" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	wantBody := githubCreatePRRequest{
		Title: "fix: null pointer in handler",
		Body:  "Fixes issue #123.",
		Head:  "sentinel/fix-123",
		Base:  "main",
	}
	if gotBody != wantBody {
		t.Errorf("request body = %+v, want %+v", gotBody, wantBody)
	}

	if pr.Number != 42 || pr.ID != "42" || pr.URL != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("pr = %+v", pr)
	}
}

func TestGitHubProvider_CreatePR_ErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		want   sentinel.FailureClass
	}{
		{401, sentinel.ClassAuthFailure},
		{403, sentinel.ClassAuthFailure},
		{404, sentinel.ClassGone},
		{429, sentinel.ClassRateLimited},
		{500, sentinel.ClassTransient},
		{422, sentinel.ClassPermanent},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		}))
		p := &GitHubProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}
		_, err := p.CreatePR(context.Background(), RepoRef{Owner: "a", Repo: "b"}, PRSpec{Head: "h", Base: "m"})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		gerr, ok := err.(*Error)
		if !ok {
			t.Fatalf("status %d: error is not *gitprovider.Error: %T", tc.status, err)
		}
		if gerr.Status != tc.status {
			t.Errorf("status %d: Error.Status = %d", tc.status, gerr.Status)
		}
		if gerr.Class != tc.want {
			t.Errorf("status %d: class = %v, want %v", tc.status, gerr.Class, tc.want)
		}
	}
}

// TestGitHubProvider_RejectsInjectionInRepoRefAndID proves that owner/repo/id values are validated
// and escaped rather than interpolated raw into the request URL: neither a traversal payload in
// Owner nor a query-string payload in the PR id can reach the actual HTTP request.
func TestGitHubProvider_RejectsInjectionInRepoRefAndID(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p := &GitHubProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}

	if _, err := p.CreatePR(context.Background(), RepoRef{Owner: "acme/../../../widgets", Repo: "widgets"}, PRSpec{Head: "h", Base: "m"}); err == nil {
		t.Fatal("expected error for traversal in Owner, got nil")
	}
	if _, err := p.PRStatus(context.Background(), RepoRef{Owner: "acme", Repo: "widgets"}, "42?secret=1"); err == nil {
		t.Fatal("expected error for query-injection PR id, got nil")
	}
	if called {
		t.Fatal("SECURITY: request reached the server despite invalid RepoRef/id")
	}
}

// TestGitHubProvider_ErrorBody_TruncatedAndRedacted proves errors.go's own documented invariant
// ("Body ... already truncated/redacted by the caller before wrapping") actually holds: a huge,
// token-echoing error response must not appear verbatim in Error.Body.
func TestGitHubProvider_ErrorBody_TruncatedAndRedacted(t *testing.T) {
	const secret = "ghp_leakyToken"
	huge := secret + strings.Repeat("x", 200000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	p := &GitHubProvider{Token: secret, APIBase: srv.URL, HTTP: srv.Client()}
	_, err := p.CreatePR(context.Background(), RepoRef{Owner: "a", Repo: "b"}, PRSpec{Head: "h", Base: "m"})
	if err == nil {
		t.Fatal("expected error")
	}
	gerr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is not *gitprovider.Error: %T", err)
	}
	if len(gerr.Body) > maxErrorBodyBytes+len(redactedPlaceholder) {
		t.Fatalf("Error.Body not truncated: len=%d", len(gerr.Body))
	}
	if strings.Contains(gerr.Body, secret) {
		t.Fatalf("SECURITY: token leaked into Error.Body: %q", gerr.Body)
	}
}

func TestGitHubProvider_PRStatus_Mapping(t *testing.T) {
	cases := []struct {
		state  string
		merged bool
		want   PRState
	}{
		{"open", false, PRStateOpen},
		{"closed", true, PRStateMerged},
		{"closed", false, PRStateDeclined},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			if r.URL.Path != "/repos/acme/widgets/pulls/42" {
				t.Errorf("path = %s", r.URL.Path)
			}
			body, _ := json.Marshal(githubPullResponse{State: tc.state, Merged: tc.merged})
			_, _ = w.Write(body)
		}))
		p := &GitHubProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}
		got, err := p.PRStatus(context.Background(), RepoRef{Owner: "acme", Repo: "widgets"}, "42")
		srv.Close()
		if err != nil {
			t.Fatalf("state=%s merged=%v: %v", tc.state, tc.merged, err)
		}
		if got != tc.want {
			t.Errorf("state=%s merged=%v: got %s, want %s", tc.state, tc.merged, got, tc.want)
		}
	}
}

// TestGitHubProvider_LargeSuccessResponse_NotTruncated is a regression test for a bug where
// success bodies were capped at maxErrorBodyBytes (8KiB) — a real GitHub pull object (embedding
// two full repository objects plus ~30 url templates) is routinely 20-30KB, and truncating it
// mid-JSON made CreatePR/PRStatus always fail with "unexpected end of JSON input".
func TestGitHubProvider_LargeSuccessResponse_NotTruncated(t *testing.T) {
	pad := strings.Repeat("x", 20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":99,"html_url":"https://github.com/acme/widgets/pull/99","state":"open","merged":false,"_pad":"` + pad + `"}`))
	}))
	defer srv.Close()

	p := &GitHubProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}
	pr, err := p.CreatePR(context.Background(), RepoRef{Owner: "acme", Repo: "widgets"}, PRSpec{Head: "fix", Base: "main"})
	if err != nil {
		t.Fatalf("CreatePR with large response: %v", err)
	}
	if pr.Number != 99 || pr.URL != "https://github.com/acme/widgets/pull/99" {
		t.Fatalf("unexpected PR: %+v", pr)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"open","merged":false,"_pad":"` + pad + `"}`))
	}))
	defer srv2.Close()
	p2 := &GitHubProvider{Token: "t", APIBase: srv2.URL, HTTP: srv2.Client()}
	state, err := p2.PRStatus(context.Background(), RepoRef{Owner: "acme", Repo: "widgets"}, "99")
	if err != nil {
		t.Fatalf("PRStatus with large response: %v", err)
	}
	if state != PRStateOpen {
		t.Fatalf("state = %s, want open", state)
	}
}

func TestGitHubProvider_RejectsDotsOnlyOwnerRepo(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, bad := range []string{".", "..", "..."} {
		called = false
		p := &GitHubProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}
		_, err := p.CreatePR(context.Background(), RepoRef{Owner: bad, Repo: bad}, PRSpec{Head: "h", Base: "b"})
		if err == nil {
			t.Errorf("owner/repo %q: expected error, got nil", bad)
		}
		if called {
			t.Errorf("owner/repo %q: request reached server, want rejected before dispatch", bad)
		}
	}
}
