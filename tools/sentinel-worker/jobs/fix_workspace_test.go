package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
)

// runReal runs a real git command directly (not through RunGit) to set up test fixtures --
// mirroring gitprovider/gitauth_test.go's own runReal helper.
func runReal(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

// newBareFixtureRepo creates a bare "origin" repo plus a work tree pushed to it with one commit
// on branch main, and returns the bare repo's path (a plain filesystem path, which git treats as
// a valid clone URL) for use as PrepareFixWorkspaceInput.CloneURL.
func newBareFixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bareRepo := filepath.Join(root, "origin.git")
	workRepo := filepath.Join(root, "work")

	runReal(t, root, "git", "init", "--bare", "-b", "main", bareRepo)
	runReal(t, root, "git", "init", "-b", "main", workRepo)
	runReal(t, workRepo, "git", "config", "user.email", "test@example.com")
	runReal(t, workRepo, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workRepo, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runReal(t, workRepo, "git", "add", ".")
	runReal(t, workRepo, "git", "commit", "-m", "seed")
	runReal(t, workRepo, "git", "remote", "add", "origin", bareRepo)
	runReal(t, workRepo, "git", "push", "origin", "main")
	return bareRepo
}

func testCred() gitprovider.GitCredential {
	// A local filesystem clone URL needs no real auth; an empty-secret credential exercises the
	// askpass plumbing without it ever being consulted (git doesn't prompt for a local path).
	return gitprovider.GitHubTokenCredential("")
}

func TestPrepareFixWorkspace_ClonesRecordsBaseCommitAndBranch(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	workspaceRoot := t.TempDir()

	ws, err := PrepareFixWorkspace(context.Background(), PrepareFixWorkspaceInput{
		WorkspaceRoot: workspaceRoot,
		JobID:         "job-abc123",
		IssueID:       "deadbeef-1234-5678-9abc-def012345678",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		CloneDepth:    1,
		Cred:          testCred(),
		Brief: TaskBrief{
			IssueID:    "deadbeef-1234-5678-9abc-def012345678",
			IssueTitle: "nil pointer in handler",
			TestCmd:    "go test ./...",
		},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}

	if ws.RepoDir != filepath.Join(workspaceRoot, "job-abc123", "repo") {
		t.Fatalf("unexpected RepoDir: %s", ws.RepoDir)
	}
	if _, err := os.Stat(filepath.Join(ws.RepoDir, "main.go")); err != nil {
		t.Fatalf("cloned repo missing seed file: %v", err)
	}

	wantBranch := "sentinel-fix/deadbeef"
	if ws.Branch != wantBranch {
		t.Fatalf("Branch = %q, want %q", ws.Branch, wantBranch)
	}

	// The workspace must actually be checked out onto the fix branch.
	out, err := exec.Command("git", "-C", ws.RepoDir, "rev-parse", "--abbrev-ref", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse --abbrev-ref HEAD: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != wantBranch {
		t.Fatalf("checked-out branch = %q, want %q", got, wantBranch)
	}

	if ws.BaseCommit == "" {
		t.Fatalf("BaseCommit not recorded")
	}
	headOut, err := exec.Command("git", "-C", ws.RepoDir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v: %s", err, headOut)
	}
	if got := strings.TrimSpace(string(headOut)); got != ws.BaseCommit {
		t.Fatalf("BaseCommit = %q, want %q (actual HEAD)", ws.BaseCommit, got)
	}
}

// TestPrepareFixWorkspace_TaskAndProgressOutsideClone proves the plan §4.4 rev-5 fix: TASK.md and
// PROGRESS.md live OUTSIDE repo/, so a stub executor's `git add -A` inside repo/ (the natural thing
// a coding agent does before committing) cannot stage either of them.
func TestPrepareFixWorkspace_TaskAndProgressOutsideClone(t *testing.T) {
	bareRepo := newBareFixtureRepo(t)
	workspaceRoot := t.TempDir()

	ws, err := PrepareFixWorkspace(context.Background(), PrepareFixWorkspaceInput{
		WorkspaceRoot: workspaceRoot,
		JobID:         "job-xyz",
		IssueID:       "cafebabe0000",
		CloneURL:      bareRepo,
		DefaultBranch: "main",
		Cred:          testCred(),
		Brief:         TaskBrief{IssueID: "cafebabe0000", IssueTitle: "boom"},
	})
	if err != nil {
		t.Fatalf("PrepareFixWorkspace: %v", err)
	}

	if filepath.Dir(ws.TaskPath) != ws.Dir {
		t.Fatalf("TaskPath %q is not directly under workspace Dir %q", ws.TaskPath, ws.Dir)
	}
	if filepath.Dir(ws.ProgressPath) != ws.Dir {
		t.Fatalf("ProgressPath %q is not directly under workspace Dir %q", ws.ProgressPath, ws.Dir)
	}
	if strings.HasPrefix(ws.TaskPath, ws.RepoDir) {
		t.Fatalf("TaskPath %q is inside RepoDir %q", ws.TaskPath, ws.RepoDir)
	}

	// Simulate exactly what a non-complying coding agent does: cd repo && touch a file && git add -A.
	if err := os.WriteFile(filepath.Join(ws.RepoDir, "fix.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write fix file: %v", err)
	}
	runReal(t, ws.RepoDir, "git", "add", "-A")
	out, err := exec.Command("git", "-C", ws.RepoDir, "diff", "--cached", "--name-only").CombinedOutput()
	if err != nil {
		t.Fatalf("diff --cached: %v: %s", err, out)
	}
	staged := strings.TrimSpace(string(out))
	if strings.Contains(staged, "TASK.md") || strings.Contains(staged, "PROGRESS.md") {
		t.Fatalf("git add -A staged TASK.md/PROGRESS.md into the repo: %q", staged)
	}
	if staged != "fix.go" {
		t.Fatalf("unexpected staged set: %q", staged)
	}

	taskContent, err := os.ReadFile(ws.TaskPath)
	if err != nil {
		t.Fatalf("read TASK.md: %v", err)
	}
	if !strings.Contains(string(taskContent), "boom") {
		t.Fatalf("TASK.md does not contain the wrapped issue title: %s", taskContent)
	}
	if !strings.Contains(string(taskContent), "TASK.md is immutable") {
		t.Fatalf("TASK.md missing immutability rule")
	}
}

func TestFixBranchName_NonHexIssueIDFallsBackToHash(t *testing.T) {
	name := FixBranchName("not-hex-at-all!!")
	if !strings.HasPrefix(name, "sentinel-fix/") {
		t.Fatalf("branch name missing prefix: %q", name)
	}
	suffix := strings.TrimPrefix(name, "sentinel-fix/")
	if len(suffix) != 8 {
		t.Fatalf("branch suffix length = %d, want 8: %q", len(suffix), suffix)
	}
	for _, r := range suffix {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("branch suffix %q is not hex", suffix)
		}
	}
	// Deterministic: same input always yields the same branch.
	if again := FixBranchName("not-hex-at-all!!"); again != name {
		t.Fatalf("FixBranchName not deterministic: %q vs %q", name, again)
	}
}

func TestCloneURL_UnknownProviderErrors(t *testing.T) {
	if _, err := CloneURL("gitlab", "o", "r"); err == nil {
		t.Fatalf("expected error for unknown provider")
	}
	url, err := CloneURL("github", "acme", "widgets")
	if err != nil || url != "https://github.com/acme/widgets.git" {
		t.Fatalf("CloneURL(github) = %q, %v", url, err)
	}
	url, err = CloneURL("bitbucket", "acme", "widgets")
	if err != nil || url != "https://bitbucket.org/acme/widgets.git" {
		t.Fatalf("CloneURL(bitbucket) = %q, %v", url, err)
	}
}
