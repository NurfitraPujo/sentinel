package jobs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newValidatableRepo creates a real (non-bare) git repo with one commit, returning its path and
// the baseCommit SHA -- ValidateFix operates on a working tree, not a clone URL, so tests build
// the fixture directly rather than through PrepareFixWorkspace.
func newValidatableRepo(t *testing.T) (repoDir, baseCommit string) {
	t.Helper()
	dir := t.TempDir()
	runReal(t, dir, "git", "init", "-b", "main")
	runReal(t, dir, "git", "config", "user.email", "test@example.com")
	runReal(t, dir, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runReal(t, dir, "git", "add", ".")
	runReal(t, dir, "git", "commit", "-m", "seed")
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v: %s", err, out)
	}
	sha := string(out)
	for len(sha) > 0 && (sha[len(sha)-1] == '\n' || sha[len(sha)-1] == '\r') {
		sha = sha[:len(sha)-1]
	}
	return dir, sha
}

func TestValidateFix_EmptyDiffFails(t *testing.T) {
	repoDir, base := newValidatableRepo(t)
	res, err := ValidateFix(context.Background(), ValidateFixInput{
		RepoDir:    repoDir,
		BaseCommit: base,
		Cred:       testCred(),
	})
	if err != nil {
		t.Fatalf("ValidateFix: %v", err)
	}
	if res.Passed {
		t.Fatalf("expected empty-diff failure, got Passed=true")
	}
	if res.Reason != FixValidReasonEmptyDiff {
		t.Fatalf("Reason = %q, want %q", res.Reason, FixValidReasonEmptyDiff)
	}
}

func TestValidateFix_RedTestsFail(t *testing.T) {
	repoDir, base := newValidatableRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "fixed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ValidateFix(context.Background(), ValidateFixInput{
		RepoDir:    repoDir,
		BaseCommit: base,
		TestCmd:    "exit 1",
		Cred:       testCred(),
	})
	if err != nil {
		t.Fatalf("ValidateFix: %v", err)
	}
	if res.Passed {
		t.Fatalf("expected red-tests failure, got Passed=true")
	}
	if res.Reason != FixValidReasonTestsFailed {
		t.Fatalf("Reason = %q, want %q", res.Reason, FixValidReasonTestsFailed)
	}
	if res.TestExitCode != 1 {
		t.Fatalf("TestExitCode = %d, want 1", res.TestExitCode)
	}
}

func TestValidateFix_FileCapExceeded(t *testing.T) {
	repoDir, base := newValidatableRepo(t)
	for i := 0; i < 3; i++ {
		name := filepath.Join(repoDir, "f"+string(rune('a'+i))+".go")
		if err := os.WriteFile(name, []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := ValidateFix(context.Background(), ValidateFixInput{
		RepoDir:    repoDir,
		BaseCommit: base,
		MaxFiles:   2,
		Cred:       testCred(),
	})
	if err != nil {
		t.Fatalf("ValidateFix: %v", err)
	}
	if res.Passed {
		t.Fatalf("expected file-cap failure, got Passed=true")
	}
	if res.Reason != FixValidReasonFileCap {
		t.Fatalf("Reason = %q, want %q", res.Reason, FixValidReasonFileCap)
	}
}

func TestValidateFix_TaskMdInDiffRejected(t *testing.T) {
	repoDir, base := newValidatableRepo(t)
	// Simulate a non-complying executor that wrote (and staged/committed) a TASK.md INSIDE the
	// repo tree, defeating the "outside the clone" convention some other way (e.g. explicit `git
	// add TASK.md` after copying it in) -- this gate must catch it even though PrepareFixWorkspace
	// itself keeps TASK.md out of RepoDir by construction (defense in depth).
	if err := os.WriteFile(filepath.Join(repoDir, "TASK.md"), []byte("leaked brief"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ValidateFix(context.Background(), ValidateFixInput{
		RepoDir:    repoDir,
		BaseCommit: base,
		Cred:       testCred(),
	})
	if err != nil {
		t.Fatalf("ValidateFix: %v", err)
	}
	if res.Passed {
		t.Fatalf("expected TASK.md-in-diff failure, got Passed=true")
	}
	if res.Reason != FixValidReasonOutOfTreePath {
		t.Fatalf("Reason = %q, want %q", res.Reason, FixValidReasonOutOfTreePath)
	}
}

func TestValidateFix_OutOfTreePathRejected(t *testing.T) {
	changed := []string{"../escaped.go"}
	if _, bad := firstDisallowedPath(changed); !bad {
		t.Fatalf("expected ../escaped.go to be flagged as out-of-tree")
	}
	if _, bad := firstDisallowedPath([]string{"/etc/passwd"}); !bad {
		t.Fatalf("expected an absolute path to be flagged")
	}
	if _, bad := firstDisallowedPath([]string{"pkg/handler.go"}); bad {
		t.Fatalf("a normal in-tree path must not be flagged")
	}
}

func TestValidateFix_HappyPath(t *testing.T) {
	repoDir, base := newValidatableRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "fixed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ValidateFix(context.Background(), ValidateFixInput{
		RepoDir:    repoDir,
		BaseCommit: base,
		TestCmd:    "true",
		MaxFiles:   20,
		Cred:       testCred(),
	})
	if err != nil {
		t.Fatalf("ValidateFix: %v", err)
	}
	if !res.Passed {
		t.Fatalf("expected Passed=true, got Reason=%q Detail=%q", res.Reason, res.Detail)
	}
	if len(res.ChangedFiles) != 1 || res.ChangedFiles[0] != "fixed.go" {
		t.Fatalf("ChangedFiles = %v, want [fixed.go]", res.ChangedFiles)
	}
}

func TestValidateFix_NoTestCmdSkipsGateButStillPasses(t *testing.T) {
	repoDir, base := newValidatableRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "fixed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ValidateFix(context.Background(), ValidateFixInput{
		RepoDir:    repoDir,
		BaseCommit: base,
		Cred:       testCred(),
	})
	if err != nil {
		t.Fatalf("ValidateFix: %v", err)
	}
	if !res.Passed || !res.TestSkipped {
		t.Fatalf("Passed=%v TestSkipped=%v, want both true", res.Passed, res.TestSkipped)
	}
}

// TestValidateFix_TestCmdDoesNotLeakWorkerSecrets is the trust-boundary regression test: testCmd
// runs attacker-influenceable repo code (including whatever test the Fix Executor itself wrote),
// so it must NEVER see the worker's own secrets. Mutation check: fold cmd.Env back to nil (which
// makes Go inherit os.Environ()) in fix_validate.go's ValidateFix and this test goes red.
func TestValidateFix_TestCmdDoesNotLeakWorkerSecrets(t *testing.T) {
	const sentinelKey = "sentinel-agent-key-should-never-leak"
	const gitToken = "git-token-should-never-leak"
	t.Setenv("SENTINEL_AGENT_KEY", sentinelKey)
	t.Setenv("GIT_GITHUB_TOKEN", gitToken)

	repoDir, base := newValidatableRepo(t)
	if err := os.WriteFile(filepath.Join(repoDir, "fixed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dumpPath := filepath.Join(t.TempDir(), "env-dump.txt")
	res, err := ValidateFix(context.Background(), ValidateFixInput{
		RepoDir:    repoDir,
		BaseCommit: base,
		TestCmd:    "env > " + dumpPath + " && exit 0",
		Cred:       testCred(),
	})
	if err != nil {
		t.Fatalf("ValidateFix: %v", err)
	}
	if !res.Passed {
		t.Fatalf("expected Passed=true, got Reason=%q Detail=%q", res.Reason, res.Detail)
	}

	dump, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read env dump: %v", err)
	}
	got := string(dump)
	if strings.Contains(got, sentinelKey) {
		t.Fatalf("testCmd child env leaked SENTINEL_AGENT_KEY value into the process: %s", got)
	}
	if strings.Contains(got, gitToken) {
		t.Fatalf("testCmd child env leaked GIT_GITHUB_TOKEN value into the process: %s", got)
	}
	if strings.Contains(got, "SENTINEL_AGENT_KEY=") {
		t.Fatalf("testCmd child env contains SENTINEL_AGENT_KEY key at all: %s", got)
	}
	if strings.Contains(got, "GIT_GITHUB_TOKEN=") {
		t.Fatalf("testCmd child env contains GIT_GITHUB_TOKEN key at all: %s", got)
	}
}
