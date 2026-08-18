package repoctx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
)

// CheckoutRelease implements plan §4.5's best-effort release-ref checkout: when an occurrence's
// release resolves to a tag/SHA, read tools should read from a worktree at that ref instead of
// the cached default-branch clone. ref is validated against refPattern (^[A-Za-z0-9._/+-]{1,100}$,
// must not start with '-') before it is used at all; on any failure — invalid ref, fetch failure,
// worktree failure — the caller is expected to fall back to repo (the default-branch clone), per
// the plan's "best-effort ... fallback to default branch".
//
// The ref is fetched (always passed to git after a literal "--") and the worktree is then added
// at FETCH_HEAD rather than the raw ref string a second time — FETCH_HEAD is a fixed, non-secret,
// non-attacker-controlled literal, so the one place ref touches an argv is the validated fetch
// call, and nothing downstream re-interpolates attacker-influenced text into a git invocation.
func (c *Cache) CheckoutRelease(ctx context.Context, repo *Repo, cred gitprovider.GitCredential, ref string) (*Repo, error) {
	if repo == nil {
		return nil, fmt.Errorf("repoctx: nil repo")
	}
	if err := validateRef(ref); err != nil {
		return nil, err
	}

	fetchArgs := []string{"fetch", "--depth", "1", "origin", "--", ref}
	if err := gitprovider.RunGit(ctx, repo.Root, cred, nil, fetchArgs...); err != nil {
		return nil, fmt.Errorf("repoctx: fetch release ref %q: %w", ref, err)
	}

	worktreesDir := filepath.Join(c.dir, worktreesDirName, repo.Key.Provider, repo.Key.Owner, repo.Key.Repo)
	if err := os.MkdirAll(worktreesDir, 0o700); err != nil {
		return nil, fmt.Errorf("repoctx: create worktrees dir: %w", err)
	}
	worktreeDir := filepath.Join(worktreesDir, sanitizeRefForPath(ref))

	// A prior worktree at this path (stale from an earlier job) must be removed first — `git
	// worktree add` refuses to reuse a non-empty target directory, and this path is derived from
	// attacker-influenced (but charset-validated) text, so this package removes it itself rather
	// than assuming it is empty.
	_ = gitprovider.RunGit(ctx, repo.Root, cred, nil, "worktree", "remove", "--force", worktreeDir)
	_ = os.RemoveAll(worktreeDir)

	if err := gitprovider.RunGit(ctx, repo.Root, cred, nil, "worktree", "add", "--detach", "--force", worktreeDir, "--", "FETCH_HEAD"); err != nil {
		_ = os.RemoveAll(worktreeDir)
		return nil, fmt.Errorf("repoctx: worktree add for release ref %q: %w", ref, err)
	}

	return &Repo{Key: repo.Key, Root: worktreeDir}, nil
}

// sanitizeRefForPath turns a validated ref (already refPattern-constrained, so only
// [A-Za-z0-9._/+-]) into a single filesystem path component by replacing '/' — refPattern's
// charset otherwise maps 1:1 onto safe path characters.
func sanitizeRefForPath(ref string) string {
	out := make([]byte, len(ref))
	for i := 0; i < len(ref); i++ {
		if ref[i] == '/' {
			out[i] = '_'
		} else {
			out[i] = ref[i]
		}
	}
	return string(out)
}
