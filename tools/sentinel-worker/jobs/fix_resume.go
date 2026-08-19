// jobs/fix_resume.go implements the FIX engine's live resume state and restart-time resume flow
// (plan §4.4 step 3b, N8f "fix-pr-resume-caps"): {TASK.md, PROGRESS.md, diff.patch (git diff vs
// baseCommit), baseCommit} persisted to fix-artifacts/<jobId>/ whenever PROGRESS.md grows
// (debounced) and on SIGTERM, and, on restart, fresh clone -> checkout baseCommit -> git apply
// diff.patch -> re-invoke the Fix Executor with a continuation prompt. A resume consumes the SAME
// attempt (jobs/fix_caps.go counts per jobID, not per run) — callers must not call
// FixCaps.RecordAttempt for a resume, only for a fresh start.
//
// Artifact sink: no S3 client exists anywhere in this module as of N8f (grepped; only
// state.Snapshotter's WORKER_SNAPSHOT_BACKEND=none no-op is implemented). Per CLAUDE.md's
// documented-seam allowance, this file ships the local-dir fallback (LocalDirArtifactSink) as the
// one working implementation and leaves the s3 backend as a documented, unimplemented seam behind
// the same ArtifactSink interface — mirroring state.Snapshotter/NoneSnapshotter's own
// interface-now-one-impl-later shape exactly, so a later phase adds an S3ArtifactSink without
// touching any caller of ArtifactSink.
package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
)

// ArtifactSink is the plan §2.8/§4.4-step-3b/3c persistence abstraction for FIX resume state and
// end-of-job artifact bundles. Put must be idempotent (the SAME key overwritten with the latest
// content is the expected steady-state usage — a debounced resume-state save, or a final bundle
// upload); Get returns ErrArtifactNotFound (not a bare nil,nil) when key does not exist, so a
// caller checking for prior resume state on restart can distinguish "nothing to resume" from a
// transport error.
type ArtifactSink interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
}

// ErrArtifactNotFound is returned by ArtifactSink.Get for a key that was never Put (or has
// expired under the sink's own lifecycle policy — plan §4.4 step 3c: "~30 days").
var ErrArtifactNotFound = errors.New("jobs: fix resume: artifact not found")

// LocalDirArtifactSink implements ArtifactSink under a local directory root — the
// WORKER_SNAPSHOT_BACKEND-less fallback this phase ships (see package doc). Keys are treated as
// slash-separated relative paths under Root; Put creates any missing parent directories.
type LocalDirArtifactSink struct {
	Root string
}

func (s LocalDirArtifactSink) path(key string) string {
	return filepath.Join(s.Root, filepath.FromSlash(key))
}

// Put writes data to Root/key, creating parent directories as needed.
func (s LocalDirArtifactSink) Put(_ context.Context, key string, data []byte) error {
	p := s.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("jobs: fix resume: local artifact sink: mkdir for %s: %w", key, err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return fmt.Errorf("jobs: fix resume: local artifact sink: write %s: %w", key, err)
	}
	return nil
}

// Get reads Root/key, returning ErrArtifactNotFound if it does not exist.
func (s LocalDirArtifactSink) Get(_ context.Context, key string) ([]byte, error) {
	data, err := os.ReadFile(s.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrArtifactNotFound
		}
		return nil, fmt.Errorf("jobs: fix resume: local artifact sink: read %s: %w", key, err)
	}
	return data, nil
}

var _ ArtifactSink = LocalDirArtifactSink{}

// resumeArtifactKeys are the four fixed keys plan §4.4 step 3b persists under
// fix-artifacts/<jobId>/. Kept as named functions (not string literals scattered across this
// file) so SaveResumeState/LoadResumeState can never drift out of sync with each other on the
// key shape.
func resumeArtifactPrefix(jobID string) string { return "fix-artifacts/" + jobID + "/" }
func resumeTaskKey(jobID string) string        { return resumeArtifactPrefix(jobID) + "TASK.md" }
func resumeProgressKey(jobID string) string    { return resumeArtifactPrefix(jobID) + "PROGRESS.md" }
func resumeDiffKey(jobID string) string        { return resumeArtifactPrefix(jobID) + "diff.patch" }
func resumeBaseCommitKey(jobID string) string  { return resumeArtifactPrefix(jobID) + "baseCommit" }

// ResumeState is the plan §4.4 step-3b live resume payload for one FIX job.
type ResumeState struct {
	JobID      string
	TaskMD     string
	ProgressMD string
	DiffPatch  []byte // `git diff` of the workspace vs BaseCommit, empty if nothing changed yet
	BaseCommit string
}

// ComputeDiffPatch runs `git add -A` (so new, never-staged files are captured too — matching
// fix_validate.go's diffNameOnly reasoning exactly, duplicated rather than shared because the two
// call sites want different `git diff` output shapes: --name-only there, the full patch here) then
// `git diff --cached <baseCommit>`, returning the full patch text. Safe to call on a workspace with
// no changes yet (returns an empty patch, not an error).
func ComputeDiffPatch(ctx context.Context, repoDir, baseCommit string, cred gitprovider.GitCredential, redactor *gitprovider.Redactor) ([]byte, error) {
	if err := gitprovider.RunGit(ctx, repoDir, cred, redactor, "add", "-A"); err != nil {
		return nil, fmt.Errorf("jobs: fix resume: git add -A: %w", err)
	}
	var buf bytes.Buffer
	out := gitprovider.NewRedactor(&buf)
	if err := gitprovider.RunGit(ctx, repoDir, cred, out, "diff", "--cached", baseCommit); err != nil {
		return nil, fmt.Errorf("jobs: fix resume: git diff --cached %s: %w", baseCommit, err)
	}
	return buf.Bytes(), nil
}

// gitDiffPatchIsolated computes the equivalent of ComputeDiffPatch's `git add -A` + `git diff
// --cached <baseCommit>` WITHOUT ever staging into (or even opening for write) repoDir/.git/index
// -- see SaveResumeState's doc comment for why that matters (finding 5: the live-save path runs
// concurrently with the Fix Executor's own subprocess in the SAME repoDir). GIT_INDEX_FILE, pointed
// at a private temp file created outside the repo, redirects `git add`/`git diff --cached` onto an
// isolated, throwaway index that this call alone ever touches -- the real index is never opened.
//
// Deliberately bypasses gitprovider.RunGit (which has no way to inject an extra env var like
// GIT_INDEX_FILE into the child): this is a purely LOCAL read (no clone/fetch/push, no network,
// no credential needed), so it does not need RunGit's askpass/credential-neutralization plumbing --
// only the same "never trust ambient host env" discipline buildTestCmdEnv/buildExecutorEnv already
// apply elsewhere in this package, reused here via isolatedGitEnv.
func gitDiffPatchIsolated(ctx context.Context, repoDir, baseCommit string, redactor *gitprovider.Redactor) ([]byte, error) {
	idxFile, err := os.CreateTemp("", "sentinel-fix-resume-index-")
	if err != nil {
		return nil, fmt.Errorf("jobs: fix resume: create isolated index file: %w", err)
	}
	idxPath := idxFile.Name()
	if err := idxFile.Close(); err != nil {
		os.Remove(idxPath)
		return nil, fmt.Errorf("jobs: fix resume: close isolated index file: %w", err)
	}
	// git treats an EMPTY file at GIT_INDEX_FILE as a corrupt index ("index file smaller than
	// expected"), not as "no index yet" -- unlike a genuinely missing path, which it happily
	// initializes fresh. Remove the just-created empty file so only its unique NAME is reused; `git
	// add` then creates a brand-new, valid index there itself.
	if err := os.Remove(idxPath); err != nil {
		return nil, fmt.Errorf("jobs: fix resume: prime isolated index path: %w", err)
	}
	defer os.Remove(idxPath)

	env := isolatedGitEnv(idxPath)
	if _, err := runIsolatedGit(ctx, repoDir, env, "add", "-A"); err != nil {
		return nil, fmt.Errorf("jobs: fix resume: isolated git add -A: %w", err)
	}
	out, err := runIsolatedGit(ctx, repoDir, env, "diff", "--cached", baseCommit)
	if err != nil {
		return nil, fmt.Errorf("jobs: fix resume: isolated git diff --cached %s: %w", baseCommit, err)
	}
	if redactor != nil {
		return redactor.Redact(out), nil
	}
	return out, nil
}

// isolatedGitEnv builds a minimal, from-scratch child environment for gitDiffPatchIsolated's two
// local git invocations: GIT_INDEX_FILE (the whole point -- redirects staging away from the real
// index), a scratch HOME (no shared on-disk git state, matching buildTestCmdEnv/buildExecutorEnv's
// own reasoning), and GIT_CONFIG_NOSYSTEM/GIT_CONFIG_GLOBAL=/dev/null so only the target repo's own
// local config applies. Never os.Environ() -- these git invocations run inside a workspace whose
// tree the Fix Executor (attacker-influenceable, plan §4.4 trust boundary) controls, so they sit on
// the same "minimal allowlisted env" side of the line as the executor's own child process.
func isolatedGitEnv(indexFile string) []string {
	env := []string{
		"GIT_INDEX_FILE=" + indexFile,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}
	for _, name := range []string{"PATH", "TMPDIR", "LANG", "HOME", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// runIsolatedGit runs `git args...` in repoDir with env and returns its combined stdout+stderr,
// raw and unredacted -- callers that hand the result to an untrusted sink must redact it themselves
// (see gitDiffPatchIsolated).
func runIsolatedGit(ctx context.Context, repoDir string, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoDir
	cmd.Env = env
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, buf.String())
	}
	return buf.Bytes(), nil
}

// SaveResumeState reads ws's TASK.md/PROGRESS.md, computes the current diff vs ws.BaseCommit, and
// Puts all four plan §4.4 step-3b artifacts to sink under fix-artifacts/<jobId>/. Called by a
// caller-owned debounce (see ResumeDebouncer) on PROGRESS.md growth, and once more unconditionally
// on SIGTERM — this function itself has no opinion on when it's called, only what it persists.
//
// Unlike SaveJobArtifacts' end-of-job bundle (which runs only after the Fix Executor has already
// exited), THIS is called live, from watchLiveResumeSave's debounced goroutine, WHILE the Fix
// Executor subprocess may still be running in the very same ws.RepoDir (fix.go). Computing the diff
// via ComputeDiffPatch's `git add -A` would stage into -- and therefore race -- the executor's own
// real .git/index: two processes writing index.lock concurrently, or this goroutine silently
// mutating what the executor itself is mid-way through staging/committing (finding 5). Instead this
// uses gitDiffPatchIsolated, which points GIT_INDEX_FILE at a private, throwaway index outside the
// repo -- `git add`/`git diff --cached` then never touch repoDir/.git/index at all.
func SaveResumeState(ctx context.Context, sink ArtifactSink, ws *FixWorkspace, cred gitprovider.GitCredential, redactor *gitprovider.Redactor) error {
	if ws == nil {
		return fmt.Errorf("jobs: fix resume: SaveResumeState: nil workspace")
	}
	taskMD, err := os.ReadFile(ws.TaskPath)
	if err != nil {
		return fmt.Errorf("jobs: fix resume: reading TASK.md: %w", err)
	}
	progressMD, err := os.ReadFile(ws.ProgressPath)
	if err != nil {
		return fmt.Errorf("jobs: fix resume: reading PROGRESS.md: %w", err)
	}
	diff, err := gitDiffPatchIsolated(ctx, ws.RepoDir, ws.BaseCommit, redactor)
	if err != nil {
		return err
	}
	for key, data := range map[string][]byte{
		resumeTaskKey(ws.JobID):       taskMD,
		resumeProgressKey(ws.JobID):   progressMD,
		resumeDiffKey(ws.JobID):       diff,
		resumeBaseCommitKey(ws.JobID): []byte(ws.BaseCommit),
	} {
		if err := sink.Put(ctx, key, data); err != nil {
			return fmt.Errorf("jobs: fix resume: saving %s: %w", key, err)
		}
	}
	return nil
}

// LoadResumeState is SaveResumeState's read side: it fetches all four artifacts for jobID and
// decodes them into a ResumeState. found=false (nil error) means no resume state exists for
// jobID — the caller should do a fresh PrepareFixWorkspace instead of resuming. A partial write
// (some but not all four keys present — should not happen since SaveResumeState writes all four,
// but a crash mid-save is exactly the scenario resume exists to survive) is treated the same as
// "not found": a partial resume state is not safely resumable, so this falls back to fresh start
// rather than resuming from an inconsistent {TASK.md, PROGRESS.md, diff.patch, baseCommit} set.
func LoadResumeState(ctx context.Context, sink ArtifactSink, jobID string) (state *ResumeState, found bool, err error) {
	baseCommit, err := sink.Get(ctx, resumeBaseCommitKey(jobID))
	if errors.Is(err, ErrArtifactNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	taskMD, err := sink.Get(ctx, resumeTaskKey(jobID))
	if errors.Is(err, ErrArtifactNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	progressMD, err := sink.Get(ctx, resumeProgressKey(jobID))
	if errors.Is(err, ErrArtifactNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	diff, err := sink.Get(ctx, resumeDiffKey(jobID))
	if errors.Is(err, ErrArtifactNotFound) {
		diff = nil // a save before any change existed is legitimate: empty diff, not "not found"
	} else if err != nil {
		return nil, false, err
	}
	return &ResumeState{
		JobID:      jobID,
		TaskMD:     string(taskMD),
		ProgressMD: string(progressMD),
		DiffPatch:  diff,
		BaseCommit: string(baseCommit),
	}, true, nil
}

// ResumeDebouncer decides whether a PROGRESS.md-growth event should trigger a new SaveResumeState
// call, so a chatty Fix Executor cannot cause a resume-state upload per line. Not itself
// goroutine-driving anything — a caller's PROGRESS.md tail callback (fix_executor.go's
// OnProgressLine) calls ShouldSave() and, if true, calls SaveResumeState.
type ResumeDebouncer struct {
	Interval time.Duration
	Now      func() time.Time // defaults to time.Now when nil

	mu   sync.Mutex
	last time.Time
}

func (d *ResumeDebouncer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// ShouldSave reports whether enough time has passed since the last true result to save again, and
// if so, atomically marks "now" as the new last-saved time (so concurrent callers can't both
// observe "yes, save" for the same debounce window).
func (d *ResumeDebouncer) ShouldSave() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if d.last.IsZero() || now.Sub(d.last) >= d.Interval {
		d.last = now
		return true
	}
	return false
}

// ResumeFixWorkspaceInput is ResumeFixWorkspace's argument bundle.
type ResumeFixWorkspaceInput struct {
	WorkspaceRoot string // $WORKER_WORKSPACE_DIR
	JobID         string
	IssueID       string
	CloneURL      string

	Cred     gitprovider.GitCredential
	Redactor *gitprovider.Redactor

	State *ResumeState // as loaded by LoadResumeState; required
}

// ResumeFixWorkspaceResult is ResumeFixWorkspace's outcome.
type ResumeFixWorkspaceResult struct {
	Workspace *FixWorkspace
	// PatchApplied is false when `git apply diff.patch` failed at BaseCommit (upstream drift,
	// a corrupted/truncated saved patch, etc.) — plan §4.4 step 3b: "Patch-apply failure => clean
	// restart of that attempt." This is NOT reported as an error: it is an expected outcome the
	// caller handles by discarding this workspace (CleanupFixWorkspace) and calling
	// PrepareFixWorkspace fresh for the SAME attempt (no FixCaps.RecordAttempt call — a clean
	// restart of an attempt is not a new one).
	PatchApplied bool
}

// ResumeFixWorkspace implements plan §4.4 step 3b's resume flow: fresh clone -> checkout
// in.State.BaseCommit -> create the fix branch from there -> restore TASK.md/PROGRESS.md OUTSIDE
// the clone -> `git apply` in.State.DiffPatch. Unlike PrepareFixWorkspace's shallow, single-branch
// clone (which only ever fetches DefaultBranch's tip and is not guaranteed to contain an older
// BaseCommit once upstream has moved on), this performs a full clone specifically so BaseCommit —
// recorded at the ORIGINAL attempt's clone time — is guaranteed reachable, matching plan §4.4's
// "guaranteed patch base" framing for baseCommit.
func ResumeFixWorkspace(ctx context.Context, in ResumeFixWorkspaceInput) (ResumeFixWorkspaceResult, error) {
	if in.State == nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: ResumeFixWorkspace: nil State")
	}
	if in.WorkspaceRoot == "" || in.JobID == "" {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: ResumeFixWorkspace: WorkspaceRoot and JobID are required")
	}
	dir := filepath.Join(in.WorkspaceRoot, in.JobID)
	// A resume always starts from a clean scratch dir -- any stale prior-attempt files must not
	// bleed into what diff.patch is applied against (PrepareFixWorkspace's own doc makes the same
	// "fresh clone every attempt" point).
	if err := os.RemoveAll(dir); err != nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: clearing stale workspace dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: create workspace dir: %w", err)
	}
	repoDir := filepath.Join(dir, "repo")

	if err := gitprovider.RunGit(ctx, dir, in.Cred, in.Redactor, "clone", in.CloneURL, repoDir); err != nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: clone: %w", err)
	}
	if err := gitprovider.RunGit(ctx, repoDir, in.Cred, in.Redactor, "checkout", in.State.BaseCommit); err != nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: checkout baseCommit %s: %w", in.State.BaseCommit, err)
	}
	branch := FixBranchName(in.IssueID)
	if err := gitprovider.RunGit(ctx, repoDir, in.Cred, in.Redactor, "checkout", "-b", branch); err != nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: create fix branch %s: %w", branch, err)
	}

	taskPath := filepath.Join(dir, "TASK.md")
	progressPath := filepath.Join(dir, "PROGRESS.md")
	if err := os.WriteFile(taskPath, []byte(in.State.TaskMD), 0o600); err != nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: restoring TASK.md: %w", err)
	}
	if err := os.WriteFile(progressPath, []byte(in.State.ProgressMD), 0o600); err != nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: restoring PROGRESS.md: %w", err)
	}

	ws := &FixWorkspace{
		JobID:        in.JobID,
		Dir:          dir,
		RepoDir:      repoDir,
		Branch:       branch,
		BaseCommit:   in.State.BaseCommit,
		TaskPath:     taskPath,
		ProgressPath: progressPath,
	}

	if len(in.State.DiffPatch) == 0 {
		return ResumeFixWorkspaceResult{Workspace: ws, PatchApplied: true}, nil
	}

	patchPath := filepath.Join(dir, "resume.patch") // OUTSIDE repoDir -- same "never in the tracked tree" discipline as TASK.md/PROGRESS.md
	if err := os.WriteFile(patchPath, in.State.DiffPatch, 0o600); err != nil {
		return ResumeFixWorkspaceResult{}, fmt.Errorf("jobs: fix resume: writing resume.patch: %w", err)
	}
	if err := gitprovider.RunGit(ctx, repoDir, in.Cred, in.Redactor, "apply", "--whitespace=nowarn", patchPath); err != nil {
		// Patch-apply failure is an EXPECTED outcome (plan §4.4 step 3b), not an error this
		// function surfaces as such -- the caller does a clean restart of the same attempt.
		return ResumeFixWorkspaceResult{Workspace: ws, PatchApplied: false}, nil
	}
	return ResumeFixWorkspaceResult{Workspace: ws, PatchApplied: true}, nil
}

// ContinuationBrief renders the plan §4.4 step-3b continuation prompt for a resumed attempt: b's
// normal TASK.md content plus prior.ProgressMD wrapped as untrusted input (guard.WrapUntrusted,
// via TaskBrief.ContinuationPrompt — this is a thin, resume-flow-scoped wrapper so callers driving
// resume don't need to import guard directly just to reach ContinuationPrompt).
func ContinuationBrief(b TaskBrief, prior *ResumeState) string {
	priorProgress := ""
	if prior != nil {
		priorProgress = prior.ProgressMD
	}
	return b.ContinuationPrompt(priorProgress)
}
