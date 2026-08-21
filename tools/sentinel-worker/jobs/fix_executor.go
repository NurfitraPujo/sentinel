// jobs/fix_executor.go implements the FIX engine's Fix Executor invocation (plan §4.4 step 3,
// N8f "fix-workspace-exec"): running $FIX_EXECUTOR_CMD with cwd=<jobId>/repo/, a hard timeout, a
// SANITIZED child environment, output routed through the token redactor to agent-logs/<jobId>.log,
// and a PROGRESS.md tail that forwards new lines to a caller-supplied callback (the worker journals
// them and posts a gated, throttled issues.progress update -- that plumbing lives in the runner
// that wires this package, not here).
//
// Trust boundary (plan §4.4 "Trust boundary", CLAUDE.md's hard rule): the Fix Executor gets the
// workspace, TASK.md, and its own configured credentials -- NOTHING else. The worker's own process
// environment carries SENTINEL_AGENT_KEY, every LLM_* key, and every git provider token; none of it
// is relevant to the executor and all of it becomes reachable to anything the executor's own child
// processes run on the repo's behalf if inherited. buildExecutorEnv below is therefore built from
// an explicit allowlist -- NEVER os.Environ() -- so a forbidden key can only reach the child if a
// caller explicitly puts it in ExtraEnv, which every caller in this codebase must not do.
package jobs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/gitprovider"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// ForbiddenExecutorEnvKeys are the exact keys (or prefixes, see isForbiddenExecutorEnvKey) that
// must NEVER reach the Fix Executor child process. Kept as a named, testable list so a mutation
// test can assert removing an entry from this list -- or the check that consults it -- turns a
// leak-detection test red (CLAUDE.md: "mutation: leak SENTINEL_AGENT_KEY into the child env -> a
// test asserting its absence goes red").
var forbiddenExecutorEnvPrefixes = []string{
	"SENTINEL_AGENT_KEY",
	"LLM_API_KEY",
	"LLM_FALLBACK_API_KEY",
	"GIT_GITHUB_TOKEN",
	"GIT_BITBUCKET_TOKEN",
	"GIT_BITBUCKET_APP_PASSWORD",
	"SENTINEL_ASKPASS_", // the git askpass wiring's own secret-carrying env vars (gitauth.go)
}

// isForbiddenExecutorEnvKey reports whether key is one this package must never let into the Fix
// Executor's child environment, matched by exact name or by a "PREFIX_" style prefix from the list
// above (covers e.g. a future LLM_FALLBACK_API_KEY_2 without needing a new literal entry).
// IsForbiddenExecutorEnvKey is the exported form of isForbiddenExecutorEnvKey (finding 4,
// fix-lifecycle remediation round 2): main.go's LoadConfig uses it to reject a WORKER_FIX_EXECUTOR_ENV
// entry naming a forbidden key at config-validation time, in addition to (not instead of)
// buildExecutorEnv's own runtime guard below -- an operator gets a startup validation error
// (plan §6: invalid config keeps the process up with /readyz failing) rather than discovering the
// misconfiguration only when the first FIX attempt's executor invocation fails.
func IsForbiddenExecutorEnvKey(key string) bool { return isForbiddenExecutorEnvKey(key) }

func isForbiddenExecutorEnvKey(key string) bool {
	for _, p := range forbiddenExecutorEnvPrefixes {
		if key == p || strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// FixExecutorInput configures one Fix Executor invocation.
type FixExecutorInput struct {
	// Cmd is $FIX_EXECUTOR_CMD verbatim -- a shell command line, run via `sh -c`, so a configured
	// value like `droid exec --non-interactive --task-file "$TASK_MD"` can use the env vars this
	// package sets below.
	Cmd     string
	RepoDir string // cwd for the child process (plan §4.4: "run ... with cwd=<jobId>/repo/")
	Timeout time.Duration

	TaskPath     string // $TASK_MD -- OUTSIDE RepoDir
	ProgressPath string // $PROGRESS_MD -- OUTSIDE RepoDir, tailed while the process runs
	JobID        string

	// ExtraEnv is the ONLY channel through which the executor's own credentials (its own API key
	// for whatever model it's configured against) reach the child -- explicit, caller-supplied,
	// never derived from the worker's own process environment. Every key in it is validated against
	// isForbiddenExecutorEnvKey before being applied; a forbidden key here is a caller bug and
	// RunFixExecutor refuses to start rather than silently dropping or silently leaking it.
	ExtraEnv map[string]string

	// LogWriter receives the child's combined stdout+stderr, redacted (plan §4.4: "stdout/stderr ->
	// agent-logs/<jobId>.log through the gitprovider Redactor"). Callers wrap the log file (or any
	// io.Writer) in a *gitprovider.Redactor built from every secret in play before passing it here;
	// this package does not know what those secrets are.
	LogWriter io.Writer

	// OnProgressLine is called, in order, once per newly-appended complete line of ProgressPath
	// while the process runs (plan §4.4: "tail PROGRESS.md -> journal + gated throttled
	// issues.progress update"). May be nil (no tailing). Called from the tailing goroutine, not the
	// caller's goroutine -- must not block indefinitely.
	OnProgressLine func(line string)

	// PollInterval controls how often ProgressPath is re-read for new lines. Defaults to 2s.
	PollInterval time.Duration
}

// FixExecutorResult is what RunFixExecutor returns once the child exits or is killed on timeout.
type FixExecutorResult struct {
	ExitCode   int
	TimedOut   bool
	NoProgress bool // true if the executor never appended a single PROGRESS.md line (plan §4.4:
	// "journaled no-progress-reported (metric-visible, not a failure)")
	Err error // non-nil for a launch failure (e.g. Cmd empty); a non-zero exit is NOT an error here
	// -- validation (fix_validate.go) is what decides pass/fail, matching plan §4.4 step 4's
	// "independent validation" framing (a red testCmd is a validation outcome, not an exec error).
}

// buildExecutorEnv constructs the Fix Executor child's environment from scratch (never
// os.Environ()): PATH (needed to resolve the executor binary and anything it shells out to),
// HOME pointed at a private scratch dir (never the worker's own $HOME -- see gitauth.go's
// scratchHome for the identical rationale: no shared on-disk credential store), the TASK_MD/
// PROGRESS_MD/SENTINEL_FIX_JOB_ID vars the executor's own prompt template can reference, and
// finally ExtraEnv -- validated key by key against isForbiddenExecutorEnvKey. A caller that
// supplies a forbidden key gets an error, not a silently-dropped or silently-leaked variable.
func buildExecutorEnv(scratchHome string, in FixExecutorInput) ([]string, error) {
	env := []string{
		"HOME=" + scratchHome,
		"TASK_MD=" + in.TaskPath,
		"PROGRESS_MD=" + in.ProgressPath,
		"SENTINEL_FIX_JOB_ID=" + in.JobID,
	}
	for _, name := range []string{"PATH", "TMPDIR", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	keys := make([]string, 0, len(in.ExtraEnv))
	for k := range in.ExtraEnv {
		keys = append(keys, k)
	}
	sortStringsLocal(keys)
	for _, k := range keys {
		if isForbiddenExecutorEnvKey(k) {
			return nil, fmt.Errorf("jobs: RunFixExecutor: ExtraEnv key %q is forbidden -- the Fix Executor must never receive Sentinel/LLM/git-token secrets (plan §4.4 trust boundary)", k)
		}
		env = append(env, k+"="+in.ExtraEnv[k])
	}
	return env, nil
}

// sortStringsLocal avoids pulling in "sort" just for one call site's determinism need (stable
// env-var ordering makes RunFixExecutor's env reproducible/testable).
func sortStringsLocal(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// RunFixExecutor runs cfg.Cmd (via `sh -c`) with cwd=cfg.RepoDir under cfg.Timeout, a sanitized
// child environment (see buildExecutorEnv), stdout/stderr streamed to cfg.LogWriter, and
// PROGRESS.md tailed to cfg.OnProgressLine. It always returns once the child exits, is killed on
// timeout, or ctx is cancelled -- it never blocks past cfg.Timeout.
func RunFixExecutor(ctx context.Context, cfg FixExecutorInput) FixExecutorResult {
	if strings.TrimSpace(cfg.Cmd) == "" {
		return FixExecutorResult{Err: fmt.Errorf("jobs: RunFixExecutor: Cmd must not be empty")}
	}
	if cfg.RepoDir == "" {
		return FixExecutorResult{Err: fmt.Errorf("jobs: RunFixExecutor: RepoDir must not be empty")}
	}

	scratchHome, err := os.MkdirTemp("", "sentinel-fix-home-")
	if err != nil {
		return FixExecutorResult{Err: fmt.Errorf("jobs: RunFixExecutor: create scratch HOME: %w", err)}
	}
	defer os.RemoveAll(scratchHome)

	env, err := buildExecutorEnv(scratchHome, cfg)
	if err != nil {
		return FixExecutorResult{Err: err}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", cfg.Cmd)
	cmd.Dir = cfg.RepoDir
	cmd.Env = env
	// Setpgid puts the `sh -c` child (and everything IT forks -- a compound $FIX_EXECUTOR_CMD using
	// pipes/&&/a wrapper script routinely does) into its own new process group, so on timeout/cancel
	// the kill below can reach the whole tree, not just `sh` itself. Without this, exec.CommandContext's
	// default cancel behaviour (Process.Kill) only ever SIGKILLs the direct child: `sh` dies, but any
	// grandchild it spawned is orphaned and keeps running in RepoDir -- writing to the workspace and
	// burning whatever API budget it holds, unbounded, past WORKER_FIX_TIMEOUT (finding 1).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// cmd.Cancel overrides the default "kill just this process" cancellation exec.CommandContext
	// would otherwise install: on ctx (runCtx) cancellation -- timeout or an outer shutdown -- send
	// SIGKILL to the NEGATIVE pid, i.e. the whole process group Setpgid created above, reaping the
	// grandchildren along with `sh`.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	// WaitDelay bounds how long cmd.Wait() will wait after Cancel returns before forcibly closing
	// the child's I/O pipes and returning -- a safety net so a pathological grandchild holding a
	// pipe open (even though it is already SIGKILLed above) cannot wedge RunFixExecutor past its
	// configured timeout.
	cmd.WaitDelay = 5 * time.Second
	var logW io.Writer = io.Discard
	if cfg.LogWriter != nil {
		logW = cfg.LogWriter
	}
	cmd.Stdout = logW
	cmd.Stderr = logW

	stopTail := make(chan struct{})
	tailDone := make(chan struct{})
	sawProgress := false
	var tailMu sync.Mutex
	if cfg.OnProgressLine != nil && cfg.ProgressPath != "" {
		go func() {
			defer close(tailDone)
			tailProgress(cfg.ProgressPath, cfg.pollInterval(), stopTail, func(line string) {
				tailMu.Lock()
				sawProgress = true
				tailMu.Unlock()
				cfg.OnProgressLine(line)
			})
		}()
	} else {
		close(tailDone)
	}

	runErr := cmd.Run()
	close(stopTail)
	<-tailDone

	if flusher, ok := cfg.LogWriter.(*gitprovider.Redactor); ok {
		flusher.Flush()
	}

	res := FixExecutorResult{}
	tailMu.Lock()
	res.NoProgress = !sawProgress
	tailMu.Unlock()

	if runCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}
	if runErr == nil {
		res.ExitCode = 0
		return res
	}
	var exitErr *exec.ExitError
	if ok := isExitError(runErr, &exitErr); ok {
		res.ExitCode = exitErr.ExitCode()
		return res
	}
	res.Err = fmt.Errorf("jobs: RunFixExecutor: %w", runErr)
	res.ExitCode = -1
	return res
}

func isExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func (cfg FixExecutorInput) pollInterval() time.Duration {
	if cfg.PollInterval > 0 {
		return cfg.PollInterval
	}
	return 2 * time.Second
}

// tailProgress polls path every interval and calls onLine once per newly-appended, complete
// (newline-terminated) line since the last poll, until stop is closed. It performs one final read
// after stop closes so a line the process wrote just before exiting is not lost to a race between
// the last poll tick and process exit (cmd.Run() returning does not imply the last poll already
// observed the final write).
func tailProgress(path string, interval time.Duration, stop <-chan struct{}, onLine func(string)) {
	var offset int64
	read := func() {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return
		}
		var buf bytes.Buffer
		n, _ := io.Copy(&buf, f)
		if n == 0 {
			return
		}
		data := buf.Bytes()
		// Only advance offset past complete lines -- a torn trailing partial line (the process is
		// mid-write) is left for the next poll to pick up whole.
		lastNL := bytes.LastIndexByte(data, '\n')
		if lastNL < 0 {
			return
		}
		complete := data[:lastNL]
		offset += int64(lastNL) + 1
		scanner := bufio.NewScanner(bytes.NewReader(complete))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			onLine(line)
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			read() // final catch-up read
			return
		case <-ticker.C:
			read()
		}
	}
}

// AgentLogPath is a thin re-export convenience so callers of this file don't need to import
// state.AgentLogPath separately just to build FixExecutorInput.LogWriter's target path; it defers
// entirely to state's own definition (plan §2.2) rather than duplicating the layout decision.
func AgentLogPath(stateDir, jobID string) string {
	return state.AgentLogPath(stateDir, jobID)
}
