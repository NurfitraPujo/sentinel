// jobs/fix_validate.go implements the FIX engine's independent validation step (plan §4.4 step 4,
// N8f "fix-workspace-exec"): after the Fix Executor exits, the worker -- NOT the executor --
// decides whether the attempt succeeded. This is deliberately independent of whatever the executor
// itself reports: a weaker/misbehaving model is the expected failure mode (plan §4.4: "the
// validation gates are the protection against a weaker model's misfires, by design"), so the gate
// must not trust the executor's own exit code or self-report.
//
// Validated, in order (plan §4.4 step 4): non-empty diff; testCmd green; diff touches
// <= WORKER_FIX_MAX_FILES; diff contains no TASK.md and no paths outside the repo tree. ANY
// failure fails the whole attempt (comment + release, count the attempt) -- there is no partial
// credit.
package jobs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
)

// FixValidationFailureReason names exactly which plan §4.4 step-4 gate rejected an attempt, so
// callers can journal/comment/metric it precisely rather than a bare bool.
type FixValidationFailureReason string

const (
	FixValidReasonNone           FixValidationFailureReason = ""
	FixValidReasonEmptyDiff      FixValidationFailureReason = "empty-diff"
	FixValidReasonTestsFailed    FixValidationFailureReason = "tests-failed"
	FixValidReasonFileCap        FixValidationFailureReason = "file-cap-exceeded"
	FixValidReasonOutOfTreePath  FixValidationFailureReason = "out-of-tree-or-task-md-in-diff"
	FixValidReasonDiffComputeErr FixValidationFailureReason = "diff-compute-error"
)

// FixValidationResult is fix_validate.go's verdict on one FIX attempt.
type FixValidationResult struct {
	Passed       bool
	Reason       FixValidationFailureReason
	Detail       string   // human-readable elaboration (e.g. test output tail, offending path)
	ChangedFiles []string // relative to RepoDir, as reported by `git diff --name-only`
	TestOutput   string   // combined stdout+stderr of testCmd, capped (see maxTestOutputBytes)
	TestExitCode int
	TestSkipped  bool // true when testCmd is empty -- not itself a failure (plan doesn't mandate a testCmd)
}

// maxTestOutputBytes caps how much of testCmd's output FixValidationResult retains -- this text
// can end up in a comment/journal payload, and an unbounded testCmd (or one an attacker-influenced
// change makes chatty) must not be able to blow up storage.
const maxTestOutputBytes = 64 * 1024

// ValidateFixInput is ValidateFix's argument bundle.
type ValidateFixInput struct {
	RepoDir    string
	BaseCommit string
	TestCmd    string // empty = skip the test gate (plan §4.4 doesn't mandate a repo have one)
	MaxFiles   int    // WORKER_FIX_MAX_FILES; <=0 treated as "no cap" is NOT allowed -- see ValidateFix
	Timeout    time.Duration

	Cred     gitprovider.GitCredential // for the `git diff` read -- no network op, but RunGit's
	Redactor *gitprovider.Redactor     // uniform plumbing needs a credential/redactor pair regardless
}

// ValidateFix runs the plan §4.4 step-4 gates and returns the first one that fails, or Passed:true
// if the attempt clears all of them. Gate order matches the plan's own listing: empty diff, then
// testCmd, then file cap, then TASK.md/out-of-tree paths -- so a caller reading only Reason still
// learns about the CHEAPEST-to-explain problem first (an empty diff makes running testCmd pointless
// and misleading: "tests passed" on a no-op change is not a useful signal).
func ValidateFix(ctx context.Context, in ValidateFixInput) (FixValidationResult, error) {
	if in.RepoDir == "" || in.BaseCommit == "" {
		return FixValidationResult{}, fmt.Errorf("jobs: ValidateFix: RepoDir and BaseCommit are required")
	}

	changed, err := diffNameOnly(ctx, in.RepoDir, in.BaseCommit, in.Cred, in.Redactor)
	if err != nil {
		return FixValidationResult{Passed: false, Reason: FixValidReasonDiffComputeErr, Detail: err.Error()}, nil
	}

	if len(changed) == 0 {
		return FixValidationResult{Passed: false, Reason: FixValidReasonEmptyDiff, ChangedFiles: changed}, nil
	}

	if offender, bad := firstDisallowedPath(changed); bad {
		return FixValidationResult{
			Passed:       false,
			Reason:       FixValidReasonOutOfTreePath,
			Detail:       fmt.Sprintf("diff touches disallowed path %q", offender),
			ChangedFiles: changed,
		}, nil
	}

	maxFiles := in.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 20 // plan §5 default WORKER_FIX_MAX_FILES; a caller passing 0/negative gets the
		// documented default rather than an accidental "no cap" -- silently disabling the file-cap
		// gate on a zero-value config would be exactly the kind of misfire this gate exists to catch.
	}
	if len(changed) > maxFiles {
		return FixValidationResult{
			Passed:       false,
			Reason:       FixValidReasonFileCap,
			Detail:       fmt.Sprintf("diff touches %d files, exceeds cap %d", len(changed), maxFiles),
			ChangedFiles: changed,
		}, nil
	}

	result := FixValidationResult{ChangedFiles: changed}
	if strings.TrimSpace(in.TestCmd) == "" {
		result.TestSkipped = true
		result.Passed = true
		return result, nil
	}

	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	scratchHome, err := os.MkdirTemp("", "sentinel-fix-validate-home-")
	if err != nil {
		return FixValidationResult{}, fmt.Errorf("jobs: ValidateFix: create scratch HOME: %w", err)
	}
	defer os.RemoveAll(scratchHome)

	cmd := exec.CommandContext(runCtx, "sh", "-c", in.TestCmd)
	cmd.Dir = in.RepoDir
	cmd.Env = buildTestCmdEnv(scratchHome)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	testErr := cmd.Run()

	out := buf.Bytes()
	if len(out) > maxTestOutputBytes {
		out = out[len(out)-maxTestOutputBytes:]
	}
	result.TestOutput = string(out)

	if testErr != nil {
		result.TestExitCode = exitCodeOf(testErr)
		result.Passed = false
		result.Reason = FixValidReasonTestsFailed
		result.Detail = "testCmd exited non-zero"
		return result, nil
	}

	result.TestExitCode = 0
	result.Passed = true
	return result, nil
}

// buildTestCmdEnv constructs the environment for running the repo's testCmd during independent
// validation, from scratch -- NEVER os.Environ(). testCmd executes attacker-influenceable repo code
// (including whatever test the Fix Executor itself wrote, per TASK.md's "failing-first test"
// instruction), so it sits on the SAME side of the trust boundary as the Fix Executor child
// (plan §4.4) and must receive NONE of the worker's own secrets: no SENTINEL_AGENT_KEY, no LLM_*
// key, no git provider token, no SENTINEL_ASKPASS_* value. This mirrors buildExecutorEnv's
// allowlist discipline in fix_executor.go exactly, deliberately duplicated rather than shared so a
// future edit to one does not silently change the other's guarantees.
func buildTestCmdEnv(scratchHome string) []string {
	env := []string{
		"HOME=" + scratchHome,
	}
	for _, name := range []string{"PATH", "TMPDIR", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

func exitCodeOf(err error) int {
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return -1
}

// diffNameOnly returns every path that differs from baseCommit -- committed changes, staged
// changes, unstaged changes to tracked files, AND new untracked files the Fix Executor created but
// never `git add`ed. A plain `git diff --name-only <baseCommit>` only ever reports tracked paths,
// so it silently misses a brand-new file the executor wrote but forgot (or the model chose not) to
// stage -- exactly the shape of "empty diff" false negative plan §4.4 step 4's gate must not
// produce. `git add -A` first (staging everything in the working tree, including new files) makes
// `git diff --cached --name-only baseCommit` see the full picture; this mutates the workspace's
// index, which is harmless here -- ValidateFix runs against a disposable, per-attempt clone that is
// either pushed (validation passed) or discarded (validation failed) immediately after.
func diffNameOnly(ctx context.Context, repoDir, baseCommit string, cred gitprovider.GitCredential, redactor *gitprovider.Redactor) ([]string, error) {
	if err := gitprovider.RunGit(ctx, repoDir, cred, redactor, "add", "-A"); err != nil {
		return nil, fmt.Errorf("git add -A: %w", err)
	}
	var buf bytes.Buffer
	out := gitprovider.NewRedactor(&buf)
	if err := gitprovider.RunGit(ctx, repoDir, cred, out, "diff", "--cached", "--name-only", baseCommit); err != nil {
		return nil, fmt.Errorf("git diff --cached --name-only %s: %w", baseCommit, err)
	}
	var files []string
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// CommitFixChanges stages (git add -A) and commits whatever the Fix Executor left in repoDir,
// under a fixed, non-model-authored commit message -- never text drawn from executor/LLM output,
// matching prBodyTemplate's own "never raw model prose" discipline. It is what makes a passing
// ValidateFix (which only inspects the working tree/index, plan §4.4 step 4) actually correspond
// to a commit landing on the fix branch: without this, RunFix could push a branch whose tip is
// STILL baseCommit -- an empty PR (finding 2) -- because validating the working tree never itself
// commits it.
//
// changed=false (no error) means nothing was staged after `git add -A` -- i.e. the SAME
// "empty diff vs baseCommit" condition ValidateFix's own FixValidReasonEmptyDiff gate checks,
// re-verified here as defense in depth rather than trusted to have stayed true since ValidateFix
// ran. Callers must treat changed=false as an empty-diff failure, not push, and count the attempt.
func CommitFixChanges(ctx context.Context, repoDir, baseCommit string, cred gitprovider.GitCredential, redactor *gitprovider.Redactor) (headSHA string, changed bool, err error) {
	if repoDir == "" || baseCommit == "" {
		return "", false, fmt.Errorf("jobs: CommitFixChanges: repoDir and baseCommit are required")
	}
	changedFiles, err := diffNameOnly(ctx, repoDir, baseCommit, cred, redactor)
	if err != nil {
		return "", false, fmt.Errorf("jobs: CommitFixChanges: %w", err)
	}
	if len(changedFiles) == 0 {
		return "", false, nil
	}
	// -c user.email/user.name (rather than a repo/global git config, which RunGit deliberately
	// neutralizes -- GIT_CONFIG_NOSYSTEM/GIT_CONFIG_GLOBAL=/dev/null, gitauth.go) so `git commit`
	// never depends on ambient host state that may or may not be configured in a given deployment.
	if err := gitprovider.RunGit(ctx, repoDir, cred, redactor,
		"-c", "user.email=sentinel-agent-worker@localhost",
		"-c", "user.name=Sentinel Agent Worker",
		"commit", "--no-verify", "-m", "sentinel: automated fix",
	); err != nil {
		return "", false, fmt.Errorf("jobs: CommitFixChanges: git commit: %w", err)
	}
	sha, err := revParseHead(ctx, repoDir, cred)
	if err != nil {
		return "", false, fmt.Errorf("jobs: CommitFixChanges: rev-parse HEAD after commit: %w", err)
	}
	if sha == baseCommit {
		// Defense in depth: a non-empty diff (checked above) that somehow still committed onto the
		// exact same SHA would be a git anomaly, not a legitimate "nothing changed" -- but treat it
		// identically to changed=false rather than let a caller push a no-op branch either way.
		return sha, false, nil
	}
	return sha, true, nil
}

// disallowedBasenames are the exact filenames the FIX workspace convention keeps OUTSIDE the repo
// tree (plan §4.4 step 2) -- if `git diff --name-only` ever reports one of these INSIDE the repo,
// something wrote a same-named file into the tracked tree, which is exactly the "customer
// stacktrace committed into the PR" risk rev 1 of this plan created and rev 5 fixed by moving
// TASK.md/PROGRESS.md outside the clone. A file legitimately named TASK.md pre-existing in the
// target repo before the fix branch is theoretically possible but out of scope: the workspace
// convention reserves these names, and this gate treats any diff touching them as suspicious by
// design (fail closed).
var disallowedBasenames = map[string]bool{
	"TASK.md":     true,
	"PROGRESS.md": true,
}

// firstDisallowedPath reports the first path in changed that either (a) matches a reserved
// basename (see disallowedBasenames) or (b) is not a clean, repo-relative path -- absolute, or
// containing a ".." segment after Clean, which `git diff --name-only` should never itself produce
// but which this gate refuses to trust blindly (defense in depth: plan §4.4 step 4 is explicit that
// out-of-tree paths must be rejected, not merely "shouldn't occur").
func firstDisallowedPath(changed []string) (string, bool) {
	for _, p := range changed {
		base := path.Base(p)
		if disallowedBasenames[base] {
			return p, true
		}
		if path.IsAbs(p) {
			return p, true
		}
		clean := path.Clean(p)
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return p, true
		}
	}
	return "", false
}
