package repoctx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
)

// TestCheckoutRelease_FetchInsteadOfRedirect_NoAuthLeaksToAttackerHost is finding 9's RED-FIRST
// proof: CheckoutRelease's release-ref fetch previously ran through a plain gitprovider.RunGit
// call with the askpass host pin DISABLED (unlike refreshLocked, fixed in N8i for the identical
// shape). A repo-local `url.<attacker>.insteadOf` rewrite of the cached clone's "origin" remote
// would otherwise silently redirect the release-ref fetch's authenticated request to an
// attacker-controlled host. This mirrors
// TestCache_RefreshFetch_InsteadOfRedirect_NoAuthLeaksToAttackerHost exactly, but drives
// c.CheckoutRelease instead of refreshLocked.
//
// MUTATION-TEST NOTE: to prove this is load-bearing, temporarily revert CheckoutRelease's fetch
// back to a plain gitprovider.RunGit(ctx, repo.Root, cred, nil, fetchArgs...) call (dropping
// expectedRefreshHost/RunGitWithHost) and re-run this test — it must go red (the attacker server
// starts receiving the Authorization header) — then restore the RunGitWithHost call.
func TestCheckoutRelease_FetchInsteadOfRedirect_NoAuthLeaksToAttackerHost(t *testing.T) {
	const wantPass = "ghp_repoctxReleaseInsteadOfLeakTestSecret"

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

	bareDir, url := newFixtureRepo(t)
	_ = bareDir

	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "widgets", DefaultBranch: "main"}
	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Repoint "origin" to the legit stub, then a repo-local insteadOf redirects it to the attacker
	// -- the exact exploit shape -- and the Repo handle's CloneURL (the entry's known-good clone
	// URL) is what CheckoutRelease must pin against, never origin's rewritten .git/config value.
	repoctxRunReal(t, repo.Root, "git", "remote", "set-url", "origin", legit.URL+"/o/r.git")
	repoctxRunReal(t, repo.Root, "git", "config", "url."+attacker.URL+"/.insteadOf", legit.URL+"/")
	repo.CloneURL = legit.URL + "/o/r.git"

	cred := gitprovider.GitHubTokenCredential(wantPass)
	_, _ = c.CheckoutRelease(context.Background(), repo, cred, "main") // fetch against stub servers cannot fully succeed; irrelevant to the assertion

	if attackerSawRequest && attackerGotAuth != "" {
		t.Fatalf("SECURITY: credential leaked to attacker host via url.insteadOf redirect on CheckoutRelease's fetch: Authorization=%q", attackerGotAuth)
	}
	if legitSawRequest && legitGotAuth == "" {
		t.Fatal("expected the legitimate host to receive an authenticated request once redirected traffic is refused")
	}
}
