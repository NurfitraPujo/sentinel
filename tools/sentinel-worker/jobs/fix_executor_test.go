package jobs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBuildExecutorEnv_NoForbiddenKeys proves the trust boundary (plan §4.4): the sanitized env
// never carries SENTINEL_AGENT_KEY, any LLM_*_API_KEY, or any git-provider token, even when the
// caller's own process environment has them set (buildExecutorEnv builds from an explicit
// allowlist, never os.Environ()).
//
// MUTATION-TEST NOTE (per task brief): to prove this is load-bearing, temporarily change
// buildExecutorEnv to `env = append(env, os.Environ()...)` (or otherwise fold in the ambient
// process environment) — this test must go red.
func TestBuildExecutorEnv_NoForbiddenKeys(t *testing.T) {
	for _, kv := range []string{
		"SENTINEL_AGENT_KEY=super-secret-agent-key",
		"LLM_API_KEY=super-secret-llm-key",
		"GIT_GITHUB_TOKEN=super-secret-git-token",
	} {
		parts := strings.SplitN(kv, "=", 2)
		t.Setenv(parts[0], parts[1])
	}

	env, err := buildExecutorEnv(t.TempDir(), FixExecutorInput{
		Cmd:          "true",
		RepoDir:      t.TempDir(),
		TaskPath:     "/x/TASK.md",
		ProgressPath: "/x/PROGRESS.md",
		JobID:        "job-1",
		ExtraEnv:     map[string]string{"DEEPSEEK_API_KEY": "the-executors-own-key"},
	})
	if err != nil {
		t.Fatalf("buildExecutorEnv: %v", err)
	}

	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"SENTINEL_AGENT_KEY", "LLM_API_KEY", "GIT_GITHUB_TOKEN", "super-secret"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("sanitized env leaked forbidden material %q:\n%s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "DEEPSEEK_API_KEY=the-executors-own-key") {
		t.Fatalf("sanitized env dropped the executor's own legitimate credential:\n%s", joined)
	}
	if !strings.Contains(joined, "TASK_MD=/x/TASK.md") || !strings.Contains(joined, "PROGRESS_MD=/x/PROGRESS.md") {
		t.Fatalf("sanitized env missing TASK_MD/PROGRESS_MD wiring:\n%s", joined)
	}
}

// TestBuildExecutorEnv_RefusesForbiddenExtraEnvKey proves a caller cannot smuggle a forbidden key
// in through ExtraEnv either — RunFixExecutor must refuse to start rather than silently pass it
// through or silently drop it (a silent drop could mask a real caller bug).
func TestBuildExecutorEnv_RefusesForbiddenExtraEnvKey(t *testing.T) {
	_, err := buildExecutorEnv(t.TempDir(), FixExecutorInput{
		Cmd:      "true",
		RepoDir:  t.TempDir(),
		ExtraEnv: map[string]string{"SENTINEL_AGENT_KEY": "leaked"},
	})
	if err == nil {
		t.Fatalf("expected an error for a forbidden ExtraEnv key")
	}
	if !strings.Contains(err.Error(), "SENTINEL_AGENT_KEY") {
		t.Fatalf("error does not name the offending key: %v", err)
	}
}

func TestIsForbiddenExecutorEnvKey(t *testing.T) {
	cases := map[string]bool{
		"SENTINEL_AGENT_KEY":    true,
		"LLM_API_KEY":           true,
		"LLM_FALLBACK_API_KEY":  true,
		"GIT_GITHUB_TOKEN":      true,
		"SENTINEL_ASKPASS_HOST": true,
		"DEEPSEEK_API_KEY":      false,
		"PATH":                  false,
		"HOME":                  false,
	}
	for key, want := range cases {
		if got := isForbiddenExecutorEnvKey(key); got != want {
			t.Errorf("isForbiddenExecutorEnvKey(%q) = %v, want %v", key, got, want)
		}
	}
}

// stubExecutorScript is a shell script standing in for a real coding-agent CLI (plan §8: "STUB-
// based" e2e). It appends two lines to PROGRESS_MD, writes a known change into the repo, and exits
// 0 -- exercising the exact PROGRESS.md tailing + log-capture path RunFixExecutor drives.
const stubExecutorScript = `#!/bin/sh
set -e
echo "read TASK.md" >> "$PROGRESS_MD"
sleep 0.05
echo "applied fix" >> "$PROGRESS_MD"
printf 'package main\n\nfunc fixed() {}\n' > fixed.go
echo "stub stdout line"
`

func writeStubExecutor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "stub.sh")
	if err := os.WriteFile(path, []byte(stubExecutorScript), 0o700); err != nil {
		t.Fatalf("write stub executor: %v", err)
	}
	return path
}

func TestRunFixExecutor_TailsProgressAndCapturesLog(t *testing.T) {
	repoDir := t.TempDir()
	scratchDir := t.TempDir()
	taskPath := filepath.Join(scratchDir, "TASK.md")
	progressPath := filepath.Join(scratchDir, "PROGRESS.md")
	if err := os.WriteFile(taskPath, []byte("do the thing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(progressPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	stub := writeStubExecutor(t)
	var logBuf bytes.Buffer
	var mu sync.Mutex
	var lines []string

	res := RunFixExecutor(context.Background(), FixExecutorInput{
		Cmd:          stub,
		RepoDir:      repoDir,
		Timeout:      10 * time.Second,
		TaskPath:     taskPath,
		ProgressPath: progressPath,
		JobID:        "job-tail",
		LogWriter:    &logBuf,
		PollInterval: 10 * time.Millisecond,
		OnProgressLine: func(line string) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, line)
		},
	})
	if res.Err != nil {
		t.Fatalf("RunFixExecutor: %v", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (log: %s)", res.ExitCode, logBuf.String())
	}
	if res.TimedOut {
		t.Fatalf("unexpected timeout")
	}
	if res.NoProgress {
		t.Fatalf("NoProgress = true, but the stub wrote progress lines")
	}

	mu.Lock()
	got := append([]string(nil), lines...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "read TASK.md" || got[1] != "applied fix" {
		t.Fatalf("progress lines = %v, want [read TASK.md, applied fix]", got)
	}

	if !strings.Contains(logBuf.String(), "stub stdout line") {
		t.Fatalf("log writer missing stdout: %q", logBuf.String())
	}

	if _, err := os.Stat(filepath.Join(repoDir, "fixed.go")); err != nil {
		t.Fatalf("stub's file change missing: %v", err)
	}
}

func TestRunFixExecutor_NoProgressWhenExecutorWritesNone(t *testing.T) {
	repoDir := t.TempDir()
	scratchDir := t.TempDir()
	progressPath := filepath.Join(scratchDir, "PROGRESS.md")
	if err := os.WriteFile(progressPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	res := RunFixExecutor(context.Background(), FixExecutorInput{
		Cmd:            "true",
		RepoDir:        repoDir,
		Timeout:        5 * time.Second,
		ProgressPath:   progressPath,
		PollInterval:   5 * time.Millisecond,
		OnProgressLine: func(string) {},
	})
	if res.Err != nil {
		t.Fatalf("RunFixExecutor: %v", res.Err)
	}
	if !res.NoProgress {
		t.Fatalf("expected NoProgress = true for a non-complying executor")
	}
}

func TestRunFixExecutor_TimesOut(t *testing.T) {
	res := RunFixExecutor(context.Background(), FixExecutorInput{
		Cmd:     "sleep 5",
		RepoDir: t.TempDir(),
		Timeout: 50 * time.Millisecond,
	})
	if !res.TimedOut {
		t.Fatalf("expected TimedOut = true")
	}
}

func TestRunFixExecutor_NonZeroExitIsNotAnError(t *testing.T) {
	res := RunFixExecutor(context.Background(), FixExecutorInput{
		Cmd:     "exit 3",
		RepoDir: t.TempDir(),
		Timeout: 5 * time.Second,
	})
	if res.Err != nil {
		t.Fatalf("a non-zero exit must not populate Err (validation decides pass/fail): %v", res.Err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", res.ExitCode)
	}
}

// TestRunFixExecutor_TimeoutKillsWholeProcessGroup proves finding 1: a compound $FIX_EXECUTOR_CMD
// that forks a grandchild (via `sh -c ... &`, exactly the shape a wrapper script produces) must not
// leave that grandchild running after RunFixExecutor returns TimedOut -- the whole process GROUP
// must be reaped, not just the direct `sh` child exec.CommandContext's default cancellation would
// otherwise only ever kill.
//
// MUTATION-TEST NOTE (per task brief): comment out `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid:
// true}` (or the `cmd.Cancel` override) in RunFixExecutor -- this test must go red, because the
// grandchild (backgrounded via `&`, in its own inherited process group under the default behaviour)
// survives the SIGKILL sent to `sh` alone and keeps appending to the marker file.
func TestRunFixExecutor_TimeoutKillsWholeProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// The grandchild is backgrounded (&) so `sh -c` itself exits almost immediately -- the parent
	// (the direct child RunFixExecutor's cmd.Process points at) is gone well before the timeout, but
	// the grandchild it forked keeps looping and appending to marker forever unless the whole group
	// is killed.
	script := fmt.Sprintf(`sh -c 'while true; do printf x >> %q; sleep 0.02; done' & disown; sleep 5`, marker)

	res := RunFixExecutor(context.Background(), FixExecutorInput{
		Cmd:     script,
		RepoDir: dir,
		Timeout: 200 * time.Millisecond,
	})
	if !res.TimedOut {
		t.Fatalf("expected TimedOut = true, got %+v", res)
	}

	// Give the grandchild a moment to have written at least once, so a size of 0 here would itself
	// be suspicious (script never ran) rather than a false pass.
	time.Sleep(100 * time.Millisecond)
	sizeAfterTimeout, err := markerSize(marker)
	if err != nil {
		t.Fatal(err)
	}
	if sizeAfterTimeout == 0 {
		t.Fatalf("marker file never grew -- test setup is broken, not proving anything")
	}

	// The grandchild must be dead: if it is still running, the marker keeps growing across this
	// window. A reaped grandchild leaves the file size unchanged.
	time.Sleep(300 * time.Millisecond)
	sizeLater, err := markerSize(marker)
	if err != nil {
		t.Fatal(err)
	}
	if sizeLater != sizeAfterTimeout {
		t.Fatalf("grandchild still writing after timeout: marker grew from %d to %d bytes -- process group was not fully reaped", sizeAfterTimeout, sizeLater)
	}
}

func markerSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func TestRunFixExecutor_EmptyCmdErrors(t *testing.T) {
	res := RunFixExecutor(context.Background(), FixExecutorInput{RepoDir: t.TempDir()})
	if res.Err == nil {
		t.Fatalf("expected an error for an empty Cmd")
	}
}
