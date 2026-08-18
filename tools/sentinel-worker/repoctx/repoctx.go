// Package repoctx implements the worker's confined, read-only repository access layer (plan
// §4.5): a shallow-clone cache keyed by provider/owner/repo, lazily populated per job and
// refreshed on a throttled cadence, plus the read_file/search_code Advisor tools that read out of
// it under strict path confinement.
//
// SECURITY (plan §4.5): clones live under WORKER_REPO_CACHE_DIR, never WORKER_STATE_DIR (the
// caller — main.go's startup validation — is responsible for rejecting a nested configuration;
// this package does not re-derive that check, it just never touches WORKER_STATE_DIR itself).
// Every git invocation this package makes goes through gitprovider.RunGit, so the same
// askpass/argv/config hygiene gitprovider documents applies here too — this package never talks
// to git via a shell, never embeds a credential in a URL, and never writes one into .git/config.
package repoctx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
)

// componentPattern is the charset a single provider/owner/repo path segment must match before it
// is ever used to build a cache directory path or a forge clone URL. Mirrors gitprovider's own
// RepoRef validation (kept independent rather than exported/shared, since this package must not
// trust that a caller already validated — the cache is keyed straight off these values).
var componentPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// refPattern is the plan §4.5-mandated charset for anything (branch name, release ref) passed to
// git as a revision: "the proto constrains only length — git argument injection is otherwise
// live". A ref must additionally not begin with '-' (else it could be parsed as a flag) and is
// always passed to git after a literal "--".
var refPattern = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,100}$`)

// isDotsOnly reports whether s is entirely '.' characters (".", "..", ...) — component charsets
// above admit these, and interpolated into a filesystem path or forge URL they are a traversal
// vector (same finding as gitprovider.isDotsOnly).
func isDotsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c != '.' {
			return false
		}
	}
	return true
}

func validateComponent(name, value string) error {
	if !componentPattern.MatchString(value) || isDotsOnly(value) {
		return fmt.Errorf("repoctx: invalid %s %q", name, value)
	}
	return nil
}

// validateRef checks a git revision value (branch, tag, SHA) against plan §4.5's charset and
// leading-dash rule.
func validateRef(ref string) error {
	if !refPattern.MatchString(ref) {
		return fmt.Errorf("repoctx: invalid ref %q", ref)
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("repoctx: ref %q must not begin with '-'", ref)
	}
	// refPattern's charset admits ".", "..", "..." etc, which git itself rejects downstream as a
	// revision — but confinement here should not depend on git's own behavior (same rationale as
	// validateComponent's isDotsOnly check). A ref of ".." is otherwise indistinguishable from a
	// legitimate short charset match until it reaches git.
	if isDotsOnly(ref) {
		return fmt.Errorf("repoctx: ref %q must not be dots-only", ref)
	}
	return nil
}

// RepoKey identifies one mapped repository for cache purposes.
type RepoKey struct {
	Provider      string
	Owner         string
	Repo          string
	DefaultBranch string // may be empty; empty means "clone/refresh the remote's default HEAD"
}

func (k RepoKey) validate() error {
	if err := validateComponent("provider", k.Provider); err != nil {
		return err
	}
	if err := validateComponent("owner", k.Owner); err != nil {
		return err
	}
	if err := validateComponent("repo", k.Repo); err != nil {
		return err
	}
	if k.DefaultBranch != "" {
		if err := validateRef(k.DefaultBranch); err != nil {
			return err
		}
	}
	return nil
}

func (k RepoKey) cacheKey() string {
	return k.Provider + "/" + k.Owner + "/" + k.Repo
}

// CloneURL builds the tokenless HTTPS clone URL for a provider/owner/repo — the URL RunGit's
// callers pass as a plain argument (per gitprovider's own doc: "remotes are always the tokenless
// URL"). Supported providers per plan §4.5 v1: "github", "bitbucket".
func CloneURL(provider, owner, repo string) (string, error) {
	if err := validateComponent("provider", provider); err != nil {
		return "", err
	}
	if err := validateComponent("owner", owner); err != nil {
		return "", err
	}
	if err := validateComponent("repo", repo); err != nil {
		return "", err
	}
	switch provider {
	case "github":
		return fmt.Sprintf("https://github.com/%s/%s.git", owner, repo), nil
	case "bitbucket":
		return fmt.Sprintf("https://bitbucket.org/%s/%s.git", owner, repo), nil
	default:
		return "", fmt.Errorf("repoctx: unsupported provider %q", provider)
	}
}

// Repo is a confined handle onto one cached clone's working tree.
type Repo struct {
	Key  RepoKey
	Root string // absolute path to the clone's working tree root
}

// entry is the cache's per-repo bookkeeping. Its own mutex serializes clone/fetch operations for
// one repo so two concurrent jobs referencing the same repo don't race two `git fetch`es into the
// same working tree; the Cache-level mutex only ever guards the `entries` map itself, so different
// repos proceed concurrently.
type entry struct {
	mu        sync.Mutex
	dir       string
	lastFetch time.Time
}

// Cache is the confined, shallow-clone read cache described in plan §4.5. All clones live under
// dir (WORKER_REPO_CACHE_DIR); Get is lazy (clones on first use per repo) and throttles refresh to
// at most once per `refresh` (WORKER_REPO_REFRESH), driven by an injected clock so tests don't
// need to sleep.
type Cache struct {
	dir     string
	depth   int
	refresh time.Duration
	now     func() time.Time

	mu      sync.Mutex
	entries map[string]*entry
}

// defaultCloneDepth is used when a RepoConn's CloneDepth is unset/non-positive.
const defaultCloneDepth = 1

// worktreesDirName is the reserved top-level entry under a Cache's dir that CheckoutRelease uses
// for release-ref worktrees (release.go), as opposed to the provider/owner/repo tree of clones.
// Evict must never treat it as a provider directory.
const worktreesDirName = "_worktrees"

// NewCache builds a Cache rooted at dir (created if missing). refresh is the minimum interval
// between `git fetch`es for an already-cloned repo (plan default 15m); now is the injected clock
// (time.Now in production).
func NewCache(dir string, refresh time.Duration, now func() time.Time) (*Cache, error) {
	if dir == "" {
		return nil, fmt.Errorf("repoctx: cache dir must not be empty")
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("repoctx: cache dir must be absolute, got %q", dir)
	}
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("repoctx: create cache dir: %w", err)
	}
	return &Cache{dir: dir, depth: defaultCloneDepth, refresh: refresh, now: now, entries: map[string]*entry{}}, nil
}

func (c *Cache) entryFor(key RepoKey) *entry {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := key.cacheKey()
	e, ok := c.entries[k]
	if !ok {
		e = &entry{dir: filepath.Join(c.dir, key.Provider, key.Owner, key.Repo)}
		c.entries[k] = e
	}
	return e
}

func (c *Cache) cloneDepth(depth int) int {
	if depth > 0 {
		return depth
	}
	return c.depth
}

// Get returns a confined handle onto key's clone, cloning it on first use and refreshing it (a
// shallow `git fetch` + hard reset to the fetched tip) when more than `refresh` has elapsed since
// the last fetch. cloneURL must be the tokenless URL (see CloneURL); cred is consumed only by
// gitprovider.RunGit's askpass plumbing and never touches this package's own memory beyond that
// call.
func (c *Cache) Get(ctx context.Context, key RepoKey, cloneURL string, cred gitprovider.GitCredential, cloneDepth int) (*Repo, error) {
	if err := key.validate(); err != nil {
		return nil, err
	}
	e := c.entryFor(key)
	e.mu.Lock()
	defer e.mu.Unlock()

	depth := c.cloneDepth(cloneDepth)
	branchArgs := []string{}
	if key.DefaultBranch != "" {
		branchArgs = []string{"--branch", key.DefaultBranch, "--single-branch"}
	}

	if _, err := os.Stat(filepath.Join(e.dir, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("repoctx: stat clone dir: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(e.dir), 0o700); err != nil {
			return nil, fmt.Errorf("repoctx: create clone parent dir: %w", err)
		}
		_ = os.RemoveAll(e.dir) // clear any partial leftover from a prior failed clone
		args := append([]string{"clone", "--depth", fmt.Sprint(depth)}, branchArgs...)
		args = append(args, "--", cloneURL, e.dir)
		if err := gitprovider.RunGit(ctx, "", cred, nil, args...); err != nil {
			_ = os.RemoveAll(e.dir)
			return nil, fmt.Errorf("repoctx: clone %s/%s: %w", key.Owner, key.Repo, err)
		}
		e.lastFetch = c.now()
		return &Repo{Key: key, Root: e.dir}, nil
	}

	if c.now().Sub(e.lastFetch) >= c.refresh {
		if err := c.refreshLocked(ctx, e, key, cred, depth); err != nil {
			return nil, err
		}
	}
	return &Repo{Key: key, Root: e.dir}, nil
}

// refreshLocked runs the throttled `git fetch --depth 1` + hard reset. Caller must hold e.mu.
func (c *Cache) refreshLocked(ctx context.Context, e *entry, key RepoKey, cred gitprovider.GitCredential, depth int) error {
	fetchArgs := []string{"fetch", "--depth", fmt.Sprint(depth), "origin"}
	if key.DefaultBranch != "" {
		fetchArgs = append(fetchArgs, "--", key.DefaultBranch)
	}
	if err := gitprovider.RunGit(ctx, e.dir, cred, nil, fetchArgs...); err != nil {
		return fmt.Errorf("repoctx: refresh fetch %s/%s: %w", key.Owner, key.Repo, err)
	}
	if err := gitprovider.RunGit(ctx, e.dir, cred, nil, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("repoctx: refresh reset %s/%s: %w", key.Owner, key.Repo, err)
	}
	if err := gitprovider.RunGit(ctx, e.dir, cred, nil, "clean", "-fd"); err != nil {
		return fmt.Errorf("repoctx: refresh clean %s/%s: %w", key.Owner, key.Repo, err)
	}
	e.lastFetch = c.now()
	return nil
}

// Evict removes every cached clone whose RepoKey is not present in mapped — plan §4.5's "cache
// eviction for repos no longer mapped". It walks the on-disk provider/owner/repo tree rather than
// trusting only the in-memory `entries` map, so a clone left over from a previous process
// lifetime (entries map empty on restart) is still cleaned up.
//
// Release worktrees (CheckoutRelease, release.go) live in a separate reserved worktreesDirName
// tree, not the provider/owner/repo tree, and are handled per-repo below: a still-mapped repo's
// worktrees are left alone (only `git worktree prune` is run against its parent clone, to clear
// any dangling `.git/worktrees/<name>` admin entries), while an unmapped repo has its worktrees
// removed alongside its clone.
func (c *Cache) Evict(mapped []RepoKey) error {
	keep := make(map[string]bool, len(mapped))
	for _, k := range mapped {
		if err := k.validate(); err == nil {
			keep[filepath.Join(c.dir, k.Provider, k.Owner, k.Repo)] = true
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	providers, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("repoctx: read cache dir: %w", err)
	}
	for _, p := range providers {
		if !p.IsDir() {
			continue
		}
		if p.Name() == worktreesDirName {
			// Not a provider directory — CheckoutRelease's reserved worktree tree, handled
			// per-repo below instead of being swept as if it were provider/owner/repo.
			continue
		}
		providerDir := filepath.Join(c.dir, p.Name())
		owners, err := os.ReadDir(providerDir)
		if err != nil {
			continue
		}
		for _, o := range owners {
			if !o.IsDir() {
				continue
			}
			ownerDir := filepath.Join(providerDir, o.Name())
			repos, err := os.ReadDir(ownerDir)
			if err != nil {
				continue
			}
			for _, r := range repos {
				if !r.IsDir() {
					continue
				}
				repoDir := filepath.Join(ownerDir, r.Name())
				repoWorktreesDir := filepath.Join(c.dir, worktreesDirName, p.Name(), o.Name(), r.Name())
				if keep[repoDir] {
					// Still mapped: clear dangling worktree admin entries but leave the release
					// worktrees themselves alone.
					_ = gitprovider.RunGit(context.Background(), repoDir, gitprovider.GitCredential{}, nil, "worktree", "prune")
					continue
				}
				// Unmapped: remove this repo's release worktrees first, while the parent clone's
				// .git still exists, then the clone itself.
				if err := os.RemoveAll(repoWorktreesDir); err != nil {
					return fmt.Errorf("repoctx: evict worktrees %s: %w", repoWorktreesDir, err)
				}
				if err := os.RemoveAll(repoDir); err != nil {
					return fmt.Errorf("repoctx: evict %s: %w", repoDir, err)
				}
				delete(c.entries, p.Name()+"/"+o.Name()+"/"+r.Name())
			}
		}
	}
	return nil
}
