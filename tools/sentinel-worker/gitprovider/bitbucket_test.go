package gitprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBitbucketProvider_RejectsInjectionInRepoRefAndID mirrors the GitHub coverage: owner/repo/id
// must be validated, not interpolated raw into the request URL.
func TestBitbucketProvider_RejectsInjectionInRepoRefAndID(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	p := &BitbucketProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}

	if _, err := p.CreatePR(context.Background(), RepoRef{Owner: "acme/../../../widgets", Repo: "widgets"}, PRSpec{Head: "h", Base: "m"}); err == nil {
		t.Fatal("expected error for traversal in Owner, got nil")
	}
	if _, err := p.PRStatus(context.Background(), RepoRef{Owner: "acme", Repo: "widgets"}, "7?secret=1"); err == nil {
		t.Fatal("expected error for query-injection PR id, got nil")
	}
	if called {
		t.Fatal("SECURITY: request reached the server despite invalid RepoRef/id")
	}
}

func TestBitbucketProvider_Auth_TokenTakesPriority(t *testing.T) {
	p := &BitbucketProvider{Token: "bbtoken", Username: "user", AppPassword: "app-pw"}
	cred := p.Auth()
	if cred.username != "x-token-auth" || cred.password != "bbtoken" {
		t.Fatalf("expected token credential, got %+v", cred)
	}
}

func TestBitbucketProvider_Auth_BasicFallback(t *testing.T) {
	p := &BitbucketProvider{Username: "user", AppPassword: "app-pw"}
	cred := p.Auth()
	if cred.username != "user" || cred.password != "app-pw" {
		t.Fatalf("expected basic credential, got %+v", cred)
	}
}

func TestBitbucketProvider_CreatePR_RequestGolden_Token(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody bitbucketCreatePRRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":7,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/acme/widgets/pull-requests/7"}}}`))
	}))
	defer srv.Close()

	p := &BitbucketProvider{Token: "bb_thetoken", APIBase: srv.URL, HTTP: srv.Client()}
	pr, err := p.CreatePR(context.Background(), RepoRef{Provider: "bitbucket", Owner: "acme", Repo: "widgets"}, PRSpec{
		Title: "fix: nil deref",
		Body:  "Fixes issue #9.",
		Head:  "sentinel/fix-9",
		Base:  "main",
	})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if gotPath != "/2.0/repositories/acme/widgets/pullrequests" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer bb_thetoken" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	want := bitbucketCreatePRRequest{
		Title:       "fix: nil deref",
		Description: "Fixes issue #9.",
		Source:      bitbucketPRBranch{Branch: bitbucketBranchRef{Name: "sentinel/fix-9"}},
		Destination: bitbucketPRBranch{Branch: bitbucketBranchRef{Name: "main"}},
	}
	if gotBody != want {
		t.Errorf("body = %+v, want %+v", gotBody, want)
	}
	if pr.ID != "7" || pr.Number != 7 || pr.URL != "https://bitbucket.org/acme/widgets/pull-requests/7" {
		t.Errorf("pr = %+v", pr)
	}
}

func TestBitbucketProvider_CreatePR_RequestGolden_BasicAuth(t *testing.T) {
	var gotUser, gotPass string
	var gotOK bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, gotOK = r.BasicAuth()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/x/y/pull-requests/1"}}}`))
	}))
	defer srv.Close()

	p := &BitbucketProvider{Username: "svc-user", AppPassword: "app-secret", APIBase: srv.URL, HTTP: srv.Client()}
	_, err := p.CreatePR(context.Background(), RepoRef{Owner: "x", Repo: "y"}, PRSpec{Head: "h", Base: "m"})
	if err != nil {
		t.Fatalf("CreatePR: %v", err)
	}
	if !gotOK || gotUser != "svc-user" || gotPass != "app-secret" {
		t.Errorf("basic auth = (%q,%q,%v), want (svc-user, app-secret, true)", gotUser, gotPass, gotOK)
	}
}

func TestBitbucketProvider_PRStatus_Mapping(t *testing.T) {
	cases := []struct {
		state string
		want  PRState
	}{
		{"OPEN", PRStateOpen},
		{"MERGED", PRStateMerged},
		{"DECLINED", PRStateDeclined},
		{"SUPERSEDED", PRStateDeclined},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/2.0/repositories/acme/widgets/pullrequests/7" {
				t.Errorf("path = %s", r.URL.Path)
			}
			body, _ := json.Marshal(bitbucketPRResponse{ID: 7, State: tc.state})
			_, _ = w.Write(body)
		}))
		p := &BitbucketProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}
		got, err := p.PRStatus(context.Background(), RepoRef{Owner: "acme", Repo: "widgets"}, "7")
		srv.Close()
		if err != nil {
			t.Fatalf("state=%s: %v", tc.state, err)
		}
		if got != tc.want {
			t.Errorf("state=%s: got %s, want %s", tc.state, got, tc.want)
		}
	}
}

// TestBitbucketProvider_LargeSuccessResponse_NotTruncated is the Bitbucket sibling of the GitHub
// regression: a PR response echoing a long templated description or rendered HTML can also exceed
// the old 8KiB error-body cap that was incorrectly reused for success bodies.
func TestBitbucketProvider_LargeSuccessResponse_NotTruncated(t *testing.T) {
	pad := strings.Repeat("y", 20000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":55,"state":"OPEN","links":{"html":{"href":"https://bitbucket.org/acme/widgets/pull-requests/55"}},"_pad":"` + pad + `"}`))
	}))
	defer srv.Close()

	p := &BitbucketProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}
	pr, err := p.CreatePR(context.Background(), RepoRef{Owner: "acme", Repo: "widgets"}, PRSpec{Head: "fix", Base: "main"})
	if err != nil {
		t.Fatalf("CreatePR with large response: %v", err)
	}
	if pr.Number != 55 || pr.URL != "https://bitbucket.org/acme/widgets/pull-requests/55" {
		t.Fatalf("unexpected PR: %+v", pr)
	}
}

func TestBitbucketProvider_RejectsDotsOnlyOwnerRepo(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	for _, bad := range []string{".", "..", "...."} {
		called = false
		p := &BitbucketProvider{Token: "t", APIBase: srv.URL, HTTP: srv.Client()}
		_, err := p.CreatePR(context.Background(), RepoRef{Owner: bad, Repo: bad}, PRSpec{Head: "h", Base: "b"})
		if err == nil {
			t.Errorf("owner/repo %q: expected error, got nil", bad)
		}
		if called {
			t.Errorf("owner/repo %q: request reached server, want rejected before dispatch", bad)
		}
	}
}
