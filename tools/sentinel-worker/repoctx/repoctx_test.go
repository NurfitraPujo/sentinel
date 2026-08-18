package repoctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
)

func mustRunGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newFixtureRepo creates a bare repo at <tmp>/origin.git seeded with one commit (a file at
// "hello.txt" and a "src/main.go" file), and returns its file:// clone URL.
func newFixtureRepo(t *testing.T) (bareDir string, cloneURL string) {
	t.Helper()
	tmp := t.TempDir()
	bareDir = filepath.Join(tmp, "origin.git")
	mustRunGit(t, tmp, "init", "--bare", "-b", "main", bareDir)

	seed := filepath.Join(tmp, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, seed, "init", "-b", "main")
	mustRunGit(t, seed, "remote", "add", "origin", bareDir)
	if err := os.WriteFile(filepath.Join(seed, "hello.txt"), []byte("line1\nline2\nline3\nsecret-token-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "src", "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(\"needle\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, seed, "add", ".")
	mustRunGit(t, seed, "commit", "-m", "seed")
	mustRunGit(t, seed, "push", "origin", "main")

	return bareDir, "file://" + bareDir
}

func fixedClock(t time.Time) func() time.Time {
	cur := t
	return func() time.Time { return cur }
}

func testCache(t *testing.T, refresh time.Duration, now func() time.Time) *Cache {
	t.Helper()
	c, err := NewCache(t.TempDir(), refresh, now)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	return c
}

func noCred() gitprovider.GitCredential { return gitprovider.GitCredential{} }

func TestGet_LazyClone(t *testing.T) {
	_, url := newFixtureRepo(t)
	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "widgets", DefaultBranch: "main"}

	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, "hello.txt")); err != nil {
		t.Fatalf("expected clone to contain hello.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, ".git")); err != nil {
		t.Fatalf("expected .git dir in clone: %v", err)
	}
}

func TestGet_RefreshThrottled(t *testing.T) {
	bareDir, url := newFixtureRepo(t)
	cur := time.Unix(1000, 0)
	clock := func() time.Time { return cur }
	c := testCache(t, 15*time.Minute, clock)
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "widgets", DefaultBranch: "main"}

	if _, err := c.Get(context.Background(), key, url, noCred(), 1); err != nil {
		t.Fatalf("initial Get: %v", err)
	}

	// Push a new commit upstream.
	pushNewCommit(t, bareDir, "new.txt", "fresh\n")

	// Not enough time has elapsed: refresh should be a no-op, new.txt absent.
	cur = cur.Add(5 * time.Minute)
	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get within throttle window: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, "new.txt")); err == nil {
		t.Fatal("expected new.txt to be absent before refresh interval elapses")
	}

	// Past the refresh interval: should fetch and pick up new.txt.
	cur = cur.Add(11 * time.Minute)
	repo, err = c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get after throttle window: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, "new.txt")); err != nil {
		t.Fatalf("expected new.txt after refresh: %v", err)
	}
}

func pushNewCommit(t *testing.T, bareDir, name, content string) {
	t.Helper()
	tmp := t.TempDir()
	mustRunGit(t, tmp, "clone", bareDir, tmp)
	if err := os.WriteFile(filepath.Join(tmp, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, tmp, "add", ".")
	mustRunGit(t, tmp, "commit", "-m", "more")
	mustRunGit(t, tmp, "push", "origin", "HEAD:main")
}

func TestEvict_RemovesUnmapped(t *testing.T) {
	_, url := newFixtureRepo(t)
	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	kept := RepoKey{Provider: "github", Owner: "acme", Repo: "kept", DefaultBranch: "main"}
	gone := RepoKey{Provider: "github", Owner: "acme", Repo: "gone", DefaultBranch: "main"}

	if _, err := c.Get(context.Background(), kept, url, noCred(), 1); err != nil {
		t.Fatalf("Get kept: %v", err)
	}
	if _, err := c.Get(context.Background(), gone, url, noCred(), 1); err != nil {
		t.Fatalf("Get gone: %v", err)
	}

	if err := c.Evict([]RepoKey{kept}); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if _, err := os.Stat(filepath.Join(c.dir, "github", "acme", "kept")); err != nil {
		t.Fatalf("expected kept repo to survive eviction: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.dir, "github", "acme", "gone")); !os.IsNotExist(err) {
		t.Fatalf("expected gone repo to be evicted, stat err = %v", err)
	}
}

// TestEvict_PreservesWorktreesOfMappedRepo is finding 3: before the fix, Evict treated the
// reserved "_worktrees" top-level entry as if it were a provider directory and wiped every
// release worktree for every repo (mapped or not) on every call. A kept repo's release worktree
// must survive Evict.
func TestEvict_PreservesWorktreesOfMappedRepo(t *testing.T) {
	bareDir, url := newFixtureRepo(t)
	mustRunGit(t, bareDir, "tag", "v1.0.0")
	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "kept", DefaultBranch: "main"}

	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rel, err := c.CheckoutRelease(context.Background(), repo, noCred(), "v1.0.0")
	if err != nil {
		t.Fatalf("CheckoutRelease: %v", err)
	}
	if _, err := os.Stat(rel.Root); err != nil {
		t.Fatalf("expected release worktree to exist before Evict: %v", err)
	}

	if err := c.Evict([]RepoKey{key}); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if _, err := os.Stat(rel.Root); err != nil {
		t.Fatalf("expected release worktree of a still-mapped repo to survive Evict, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo.Root, ".git")); err != nil {
		t.Fatalf("expected mapped repo's clone to survive Evict: %v", err)
	}
}

// TestEvict_UnmappingPrunesOnlyThatReposWorktrees is finding 3's converse: unmapping a repo must
// remove only ITS release worktrees, leaving a different, still-mapped repo's worktrees intact.
func TestEvict_UnmappingPrunesOnlyThatReposWorktrees(t *testing.T) {
	bareDir, url := newFixtureRepo(t)
	mustRunGit(t, bareDir, "tag", "v1.0.0")
	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	keptKey := RepoKey{Provider: "github", Owner: "acme", Repo: "kept", DefaultBranch: "main"}
	goneKey := RepoKey{Provider: "github", Owner: "acme", Repo: "gone", DefaultBranch: "main"}

	keptRepo, err := c.Get(context.Background(), keptKey, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get kept: %v", err)
	}
	goneRepo, err := c.Get(context.Background(), goneKey, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get gone: %v", err)
	}
	keptRel, err := c.CheckoutRelease(context.Background(), keptRepo, noCred(), "v1.0.0")
	if err != nil {
		t.Fatalf("CheckoutRelease kept: %v", err)
	}
	goneRel, err := c.CheckoutRelease(context.Background(), goneRepo, noCred(), "v1.0.0")
	if err != nil {
		t.Fatalf("CheckoutRelease gone: %v", err)
	}

	if err := c.Evict([]RepoKey{keptKey}); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if _, err := os.Stat(keptRel.Root); err != nil {
		t.Fatalf("expected kept repo's worktree to survive: %v", err)
	}
	if _, err := os.Stat(goneRel.Root); !os.IsNotExist(err) {
		t.Fatalf("expected gone repo's worktree to be removed, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.dir, worktreesDirName, "github", "acme", "gone")); !os.IsNotExist(err) {
		t.Fatal("expected gone repo's entire worktrees dir to be removed")
	}
}

func getFixtureRepo(t *testing.T) *Repo {
	t.Helper()
	_, url := newFixtureRepo(t)
	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "widgets", DefaultBranch: "main"}
	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return repo
}

// --- read_file confinement battery ---

func TestReadFile_Happy(t *testing.T) {
	repo := getFixtureRepo(t)
	out, err := ReadFile(repo, "hello.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(out, "line1") {
		t.Fatalf("expected content, got %q", out)
	}
}

func TestReadFile_LineRange(t *testing.T) {
	repo := getFixtureRepo(t)
	out, err := ReadFile(repo, "hello.txt", 2, 3)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if out != "line2\nline3\n" {
		t.Fatalf("got %q", out)
	}
}

func TestReadFile_RejectsTraversal(t *testing.T) {
	repo := getFixtureRepo(t)
	if _, err := ReadFile(repo, "../origin.git/HEAD", 0, 0); err == nil {
		t.Fatal("expected traversal to be rejected")
	}
	if _, err := ReadFile(repo, "src/../../origin.git/HEAD", 0, 0); err == nil {
		t.Fatal("expected traversal via subdir to be rejected")
	}
}

func TestReadFile_RejectsAbsolute(t *testing.T) {
	repo := getFixtureRepo(t)
	if _, err := ReadFile(repo, "/etc/passwd", 0, 0); err == nil {
		t.Fatal("expected absolute path to be rejected")
	}
	// filepath.Join drops a leading "/" from its second argument, so without the explicit
	// IsAbs guard "/hello.txt" would silently resolve to repo.Root/hello.txt (which exists) and
	// succeed instead of being rejected — this is the case that actually exercises the guard.
	if _, err := ReadFile(repo, "/hello.txt", 0, 0); err == nil {
		t.Fatal("expected absolute path to be rejected even when it would resolve inside the root")
	}
}

func TestReadFile_RejectsGitDir(t *testing.T) {
	repo := getFixtureRepo(t)
	if _, err := ReadFile(repo, ".git/config", 0, 0); err == nil {
		t.Fatal("expected .git path to be rejected")
	}
	if _, err := ReadFile(repo, ".git/HEAD", 0, 0); err == nil {
		t.Fatal("expected .git/HEAD to be rejected")
	}
}

// newFixtureRepoWithSymlinks builds a fixture like newFixtureRepo but COMMITS three malicious
// symlinks into the seed history before push — the realistic attacker path (finding 4): a hostile
// commit/PR lands the symlinks in the repo itself, so no local filesystem access to the clone is
// needed to plant them, unlike a symlink materialized by the test after cloning.
//
//   - "escape-abs"  — an absolute symlink straight to a file outside the future clone root.
//   - "sub/.git"    — a symlink literally named ".git", nested under a subdirectory (the toplevel
//     name is unavailable at commit time — it collides with the real repository metadata
//     directory every clone has), relatively targeting the clone's own real .git dir.
//   - "escape-dir"  — a directory symlink escaping the future clone root, exercising path-join
//     resolution through a committed symlink for an intermediate (non-leaf) component.
func newFixtureRepoWithSymlinks(t *testing.T) (cloneURL string) {
	t.Helper()
	tmp := t.TempDir()
	bareDir := filepath.Join(tmp, "origin.git")
	mustRunGit(t, tmp, "init", "--bare", "-b", "main", bareDir)

	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	seed := filepath.Join(tmp, "seed")
	if err := os.MkdirAll(filepath.Join(seed, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, seed, "init", "-b", "main")
	mustRunGit(t, seed, "remote", "add", "origin", bareDir)
	if err := os.WriteFile(filepath.Join(seed, "hello.txt"), []byte("line1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outsideDir, "secret.txt"), filepath.Join(seed, "escape-abs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".git"), filepath.Join(seed, "sub", ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(seed, "escape-dir")); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, seed, "add", ".")
	mustRunGit(t, seed, "commit", "-m", "seed with malicious symlinks")
	mustRunGit(t, seed, "push", "origin", "main")

	return "file://" + bareDir
}

func getFixtureRepoWithSymlinks(t *testing.T) *Repo {
	t.Helper()
	url := newFixtureRepoWithSymlinks(t)
	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "evil", DefaultBranch: "main"}
	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return repo
}

// TestReadFile_RejectsCommittedSymlinkEscapes is finding 4: the confinement battery must cover the
// realistic attacker path (symlinks committed into the repo) rather than only a symlink the test
// harness materializes on disk after cloning.
func TestReadFile_RejectsCommittedSymlinkEscapes(t *testing.T) {
	repo := getFixtureRepoWithSymlinks(t)

	if _, err := ReadFile(repo, "escape-abs", 0, 0); err == nil {
		t.Fatal("expected committed absolute symlink-out to be rejected")
	}
	if _, err := ReadFile(repo, "sub/.git/config", 0, 0); err == nil {
		t.Fatal("expected committed .git symlink to be rejected")
	}
	if _, err := ReadFile(repo, "escape-dir/secret.txt", 0, 0); err == nil {
		t.Fatal("expected committed directory symlink escape to be rejected")
	}
}

// TestReadFile_RejectsSymlinkToGitViaAlias is finding 1: it pins the resolved-path .git re-check
// at read.go:70-74 as the SOLE guard against reading the clone's own .git metadata through a
// symlink that is NOT itself named ".git" but resolves under it. Neither of the other two guards
// fires here: the root-escape check (read.go:60-62) doesn't fire because .git IS under root, and
// the literal cleaned-path pre-check (read.go:45-49) doesn't fire because the requested path is
// "peek/config", not ".git/config".
//
// MUTATION-TEST NOTE: temporarily changing the loop at read.go:70-74 from `if seg == ".git"` to
// `if seg == ".git" && false` turns this test RED — "peek/config" is then read successfully,
// leaking .git internals through the alias.
func TestReadFile_RejectsSymlinkToGitViaAlias(t *testing.T) {
	repo := getFixtureRepo(t)
	linkPath := filepath.Join(repo.Root, "peek")
	if err := os.Symlink(filepath.Join(repo.Root, ".git"), linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(repo, "peek/config", 0, 0); err == nil {
		t.Fatal("expected symlink-aliased .git read to be rejected")
	}
}

// TestReadFile_RejectsIntermediateComponentSymlinkEscape is finding 1's companion: an
// intermediate (non-leaf) path component, rather than the leaf, is a symlink pointing outside
// root.
func TestReadFile_RejectsIntermediateComponentSymlinkEscape(t *testing.T) {
	repo := getFixtureRepo(t)
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(repo.Root, "jail")
	if err := os.Symlink(outsideDir, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(repo, "jail/secret.txt", 0, 0); err == nil {
		t.Fatal("expected intermediate-component symlink escape to be rejected")
	}
}

func TestReadFile_ByteCap(t *testing.T) {
	repo := getFixtureRepo(t)
	big := strings.Repeat("x", maxReadBytes*2)
	if err := os.WriteFile(filepath.Join(repo.Root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFile(repo, "big.txt", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasSuffix(out, truncationMarker) {
		t.Fatalf("expected truncation marker, got tail %q", out[max(0, len(out)-40):])
	}
	if len(out)-len(truncationMarker) > maxReadBytes {
		t.Fatalf("output exceeds cap: %d bytes", len(out))
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// --- search_code battery ---

func TestSearchCode_Happy(t *testing.T) {
	repo := getFixtureRepo(t)
	out, err := SearchCode(context.Background(), repo, "needle", "")
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if !strings.Contains(out, "src/main.go") || !strings.Contains(out, "needle") {
		t.Fatalf("expected a match, got %q", out)
	}
}

func TestSearchCode_Glob(t *testing.T) {
	repo := getFixtureRepo(t)
	out, err := SearchCode(context.Background(), repo, "needle", "*.txt")
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if strings.Contains(out, "src/main.go") {
		t.Fatalf("glob restriction was not applied: %q", out)
	}
}

func TestSearchCode_NoMatchIsNotAnError(t *testing.T) {
	repo := getFixtureRepo(t)
	out, err := SearchCode(context.Background(), repo, "definitely-not-present-xyz", "")
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if out != "" {
		t.Fatalf("expected empty result, got %q", out)
	}
}

func TestSearchCode_RejectsInjectionGlob(t *testing.T) {
	repo := getFixtureRepo(t)
	if _, err := SearchCode(context.Background(), repo, "needle", "-x"); err == nil {
		t.Fatal("expected glob starting with '-' to be rejected")
	}
	if _, err := SearchCode(context.Background(), repo, "needle", "/etc/passwd"); err == nil {
		t.Fatal("expected absolute glob to be rejected")
	}
	if _, err := SearchCode(context.Background(), repo, "needle", "../etc"); err == nil {
		t.Fatal("expected traversal glob to be rejected")
	}
}

func TestSearchCode_PatternStartingWithDashIsNotAFlag(t *testing.T) {
	repo := getFixtureRepo(t)
	// A pattern that looks like a flag must be treated as a literal search pattern (passed via
	// -e), not parsed as a git-grep option.
	if _, err := SearchCode(context.Background(), repo, "-x", ""); err != nil {
		t.Fatalf("SearchCode with dash-leading pattern: %v", err)
	}
}

func TestSearchCode_ResultCap(t *testing.T) {
	repo := getFixtureRepo(t)
	var b strings.Builder
	for i := 0; i < maxSearchResults+50; i++ {
		b.WriteString("needle-repeat\n")
	}
	if err := os.WriteFile(filepath.Join(repo.Root, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, repo.Root, "add", "many.txt")
	mustRunGit(t, repo.Root, "commit", "-m", "many")

	out, err := SearchCode(context.Background(), repo, "needle-repeat", "")
	if err != nil {
		t.Fatalf("SearchCode: %v", err)
	}
	if !strings.HasSuffix(out, truncationMarker) {
		t.Fatal("expected result-cap truncation marker")
	}
	lines := strings.Count(out, "\n")
	if lines > maxSearchResults+1 {
		t.Fatalf("expected at most ~%d lines, got %d", maxSearchResults, lines)
	}
}

// --- release-ref checkout ---

func TestCheckoutRelease_Happy(t *testing.T) {
	bareDir, url := newFixtureRepo(t)
	mustRunGit(t, bareDir, "tag", "v1.0.0")
	// Add a commit AFTER the tag so default-branch content diverges from the tagged content —
	// proves CheckoutRelease actually reads at the tag, not just the cache's HEAD.
	pushNewCommit(t, bareDir, "after-tag.txt", "after\n")

	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "widgets", DefaultBranch: "main"}
	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	rel, err := c.CheckoutRelease(context.Background(), repo, noCred(), "v1.0.0")
	if err != nil {
		t.Fatalf("CheckoutRelease: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rel.Root, "hello.txt")); err != nil {
		t.Fatalf("expected tagged content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rel.Root, "after-tag.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected after-tag.txt to be absent at the tag, err=%v", err)
	}
}

func TestCheckoutRelease_RejectsInjectionRef(t *testing.T) {
	_, url := newFixtureRepo(t)
	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "widgets", DefaultBranch: "main"}
	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	for _, bad := range []string{"-x", "--upload-pack=/bin/sh", "refs/heads/main; rm -rf /", "a b"} {
		if _, err := c.CheckoutRelease(context.Background(), repo, noCred(), bad); err == nil {
			t.Fatalf("expected ref %q to be rejected", bad)
		}
	}
}

func TestCheckoutRelease_FallbackOnUnknownRef(t *testing.T) {
	_, url := newFixtureRepo(t)
	c := testCache(t, time.Hour, fixedClock(time.Unix(0, 0)))
	key := RepoKey{Provider: "github", Owner: "acme", Repo: "widgets", DefaultBranch: "main"}
	repo, err := c.Get(context.Background(), key, url, noCred(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.CheckoutRelease(context.Background(), repo, noCred(), "does-not-exist-xyz"); err == nil {
		t.Fatal("expected checkout of a nonexistent ref to fail (caller falls back to default branch)")
	}
}

// --- CloneURL ---

func TestCloneURL(t *testing.T) {
	got, err := CloneURL("github", "acme", "widgets")
	if err != nil || got != "https://github.com/acme/widgets.git" {
		t.Fatalf("github: %q, %v", got, err)
	}
	got, err = CloneURL("bitbucket", "acme", "widgets")
	if err != nil || got != "https://bitbucket.org/acme/widgets.git" {
		t.Fatalf("bitbucket: %q, %v", got, err)
	}
	if _, err := CloneURL("gitlab", "acme", "widgets"); err == nil {
		t.Fatal("expected unsupported provider to error")
	}
	if _, err := CloneURL("github", "..", "widgets"); err == nil {
		t.Fatal("expected dots-only owner to be rejected")
	}
}
