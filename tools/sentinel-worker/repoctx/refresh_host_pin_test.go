package repoctx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
)

func repoctxRunReal(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// TestExpectedRefreshHost covers expectedRefreshHost's contract directly: an http(s) clone URL
// yields its host:port, a non-http(s)/schemeless clone URL (the local filesystem paths test
// fixtures use elsewhere in this package) yields "" (no credential-bearing request to protect, so
// no pin needed), and an empty or malformed clone URL is refused outright rather than silently
// disabling the pin.
func TestExpectedRefreshHost(t *testing.T) {
	cases := []struct {
		name     string
		cloneURL string
		want     string
		wantErr  bool
	}{
		{"https with default port", "https://github.com/o/r.git", "github.com", false},
		{"https with explicit port", "https://git.example.com:8443/o/r.git", "git.example.com:8443", false},
		{"local filesystem path (no scheme)", "/tmp/some/bare/repo.git", "", false},
		{"file scheme", "file:///tmp/some/bare/repo.git", "", false},
		{"empty clone URL refused", "", "", true},
		{"http(s) scheme with no host refused", "https:///o/r.git", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := expectedRefreshHost(tc.cloneURL)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expectedRefreshHost(%q): expected an error, got host %q", tc.cloneURL, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("expectedRefreshHost(%q): unexpected error: %v", tc.cloneURL, err)
			}
			if got != tc.want {
				t.Fatalf("expectedRefreshHost(%q) = %q, want %q", tc.cloneURL, got, tc.want)
			}
		})
	}
}

// TestCache_RefreshFetch_InsteadOfRedirect_NoAuthLeaksToAttackerHost is circuit-config-sec finding
// 5's RED-FIRST proof: `git fetch origin` (bare remote name, no URL in argv) previously ran through
// a plain gitprovider.RunGit call, whose own deriveExpectedHost(args) finds no http(s) URL and
// therefore returns "", disabling the askpass host pin entirely -- the exact same attack shape
// jobs.PushFixBranch's finding 1 already fixed on the write path. A repo-local
// `url.<attacker>.insteadOf` rewrite of the cached clone's "origin" remote (plausible: this cache
// directory is shared state something with local write access could tamper with between refreshes)
// would silently redirect the periodic refresh fetch's authenticated request to an
// attacker-controlled server.
//
// This test builds a real local clone (so refreshLocked has a genuine git repo + "origin" remote to
// operate on), rewrites "origin" to an httptest attacker server via insteadOf, sets the entry's
// cloneURL to the KNOWN-GOOD legit server's URL (never re-read from the tampered .git/config,
// mirroring PushFixBranch's own CloneURL-derived pin), and drives refreshLocked directly. It asserts
// the attacker server never receives an Authorization header while the legit server does.
func TestCache_RefreshFetch_InsteadOfRedirect_NoAuthLeaksToAttackerHost(t *testing.T) {
	const wantPass = "ghp_repoctxRefreshInsteadOfLeakTestSecret"

	var attackerGotAuth string
	var attackerSawRequest bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerSawRequest = true
		if auth := r.Header.Get("Authorization"); auth != "" {
			attackerGotAuth = auth
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer attacker.Close()

	var legitGotAuth string
	var legitSawRequest bool
	legit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		legitSawRequest = true
		if auth := r.Header.Get("Authorization"); auth != "" {
			legitGotAuth = auth
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer legit.Close()

	// A real local clone (from a real bare fixture repo, unrelated to legit/attacker) so
	// refreshLocked has a genuine .git to operate on. "origin" is repointed at the legit URL, then
	// a repo-local insteadOf redirects it to the attacker -- the exact exploit shape.
	bareDir, _ := newFixtureRepo(t)
	workDir := t.TempDir() + "/work"
	repoctxRunReal(t, t.TempDir(), "git", "clone", bareDir, workDir)
	repoctxRunReal(t, workDir, "git", "remote", "set-url", "origin", legit.URL+"/o/r.git")
	repoctxRunReal(t, workDir, "git", "config", "url."+attacker.URL+"/.insteadOf", legit.URL+"/")

	cache, err := NewCache(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	e := &entry{dir: workDir, cloneURL: legit.URL + "/o/r.git"} // the KNOWN-GOOD clone URL, never origin's rewritten one
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "widgets"}
	cred := gitprovider.GitHubTokenCredential(wantPass)

	_ = cache.refreshLocked(context.Background(), e, key, cred, 1) // git fetch against these stub servers cannot fully succeed; irrelevant to the assertion

	if attackerSawRequest && attackerGotAuth != "" {
		t.Fatalf("SECURITY: credential leaked to attacker host via url.insteadOf redirect on refresh fetch: Authorization=%q", attackerGotAuth)
	}
	if legitSawRequest && legitGotAuth == "" {
		t.Fatal("expected the legitimate host to receive an authenticated request once redirected traffic is refused")
	}
}

// MUTATION-TEST NOTE (finding 5): to prove refreshLocked's host pin is load-bearing, temporarily
// revert its fetch call back to a plain gitprovider.RunGit(ctx, e.dir, cred, nil, fetchArgs...)
// call (dropping expectedRefreshHost/RunGitWithHost entirely) and re-run
// TestCache_RefreshFetch_InsteadOfRedirect_NoAuthLeaksToAttackerHost — it must go red (the attacker
// server starts receiving the Authorization header) — then restore the RunGitWithHost call.
