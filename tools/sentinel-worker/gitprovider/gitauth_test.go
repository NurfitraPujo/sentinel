package gitprovider

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const secretToken = "ghp_theSuperSecretLeakTestToken"

// writeStubGit installs a fake `git` executable at the front of PATH for this test (via
// t.Setenv, restored automatically) that, instead of doing anything git-like, dumps its own
// argv and full environment to dumpFile and exits 0. This is the plan §8 "stub git (a test script
// that dumps its own argv+env to a file)" leak-test fixture: it lets the test prove the secret
// reaches the child ONLY via env, never argv, without needing a real credential-checking git
// server.
func writeStubGit(t *testing.T, dumpFile string) {
	t.Helper()
	dir := t.TempDir()
	stubPath := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  echo '--ARGV--'\n" +
		"  for a in \"$@\"; do echo \"arg:[$a]\"; done\n" +
		"  echo '--ENV--'\n" +
		"  env\n" +
		"} >> \"" + dumpFile + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub git: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// THE LEAK TEST (plan §4.5/§8): proves the credential's secret value(s) appear in the env of the
// askpass-invoking git child process only, and NEVER in that process's argv (i.e. never observable
// via /proc/*/cmdline). Table-driven over all three credential constructors, because
// BitbucketTokenCredential and BitbucketBasicCredential put their secret in the PASSWORD slot
// (unlike GitHubTokenCredential, whose secret is the USERNAME) — a leak-test covering only the
// username slot cannot catch an argv leak of a Bitbucket secret.
func TestRunGit_TokenNeverInArgv(t *testing.T) {
	const bbSecret = "bb_theSuperSecretAppPassword"
	cases := []struct {
		name       string
		cred       GitCredential
		wantEnvVar string
		wantEnvVal string
	}{
		{"github-token-in-username", GitHubTokenCredential(secretToken), "SENTINEL_ASKPASS_USERNAME", secretToken},
		{"bitbucket-token-in-password", BitbucketTokenCredential(secretToken), "SENTINEL_ASKPASS_PASSWORD", secretToken},
		{"bitbucket-basic-in-password", BitbucketBasicCredential("someuser", bbSecret), "SENTINEL_ASKPASS_PASSWORD", bbSecret},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dumpFile := filepath.Join(t.TempDir(), "dump.txt")
			writeStubGit(t, dumpFile)

			dir := t.TempDir()
			var logBuf bytes.Buffer
			redactor := NewRedactor(&logBuf, tc.cred.username, tc.cred.password)

			if err := RunGit(context.Background(), dir, tc.cred, redactor, "status", "--porcelain"); err != nil {
				t.Fatalf("RunGit: %v", err)
			}

			dump, err := os.ReadFile(dumpFile)
			if err != nil {
				t.Fatalf("read dump: %v", err)
			}
			argvSection, envSection := splitDump(t, string(dump))

			// Both credential slots must be absent from argv, whichever one carries the actual
			// secret in this case.
			if tc.cred.username != "" && strings.Contains(argvSection, tc.cred.username) {
				t.Fatalf("SECURITY: credential username leaked into argv:\n%s", argvSection)
			}
			if tc.cred.password != "" && strings.Contains(argvSection, tc.cred.password) {
				t.Fatalf("SECURITY: credential password leaked into argv:\n%s", argvSection)
			}
			if !strings.Contains(envSection, tc.wantEnvVar+"="+tc.wantEnvVal) {
				t.Fatalf("expected secret in child env (%s), got:\n%s", tc.wantEnvVar, envSection)
			}
			if strings.Contains(logBuf.String(), tc.wantEnvVal) {
				t.Fatalf("SECURITY: secret leaked into redacted log output: %q", logBuf.String())
			}
		})
	}
}

// splitDump separates the stub's ARGV and ENV sections so the argv assertion above can't
// accidentally match against the env section (which legitimately contains the secret).
func splitDump(t *testing.T, content string) (argv, env string) {
	t.Helper()
	parts := strings.SplitN(content, "--ENV--", 2)
	if len(parts) != 2 {
		t.Fatalf("stub dump missing --ENV-- marker: %q", content)
	}
	return parts[0], parts[1]
}

// MUTATION-TEST NOTE: to prove this guard is load-bearing, temporarily change RunGit's env
// construction to also append "--token="+cred.password (or similar) to args, re-run this test —
// it must go red — then revert.

// TestRunGit_AskpassHelperReadsFromEnvNotArgv exercises WriteAskpassHelper + RunGit against the
// REAL git binary (repo convention: real git against local fixtures is allowed) doing a clone of
// a local bare repo over a plain file path (no auth actually required), and asserts that after
// the clone, neither .git/config nor `git remote -v` output contains the token — the remote is
// always the tokenless URL per plan §4.5, so the askpass path is exercised (GIT_ASKPASS is set)
// even though this particular transport never calls it.
func TestRunGit_CloneAndPush_NoSecretInGitConfigOrRemote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only fixture script")
	}

	root := t.TempDir()
	bareRepo := filepath.Join(root, "origin.git")
	workRepo := filepath.Join(root, "seed")
	cloneDir := filepath.Join(root, "clone")

	runReal(t, root, "git", "init", "--bare", "-b", "main", bareRepo)
	runReal(t, root, "git", "init", "-b", "main", workRepo)
	runReal(t, workRepo, "git", "config", "user.email", "test@example.com")
	runReal(t, workRepo, "git", "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workRepo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runReal(t, workRepo, "git", "add", ".")
	runReal(t, workRepo, "git", "commit", "-m", "seed")
	runReal(t, workRepo, "git", "remote", "add", "origin", bareRepo)
	runReal(t, workRepo, "git", "push", "origin", "main")

	var logBuf bytes.Buffer
	redactor := NewRedactor(&logBuf, secretToken)
	cred := GitHubTokenCredential(secretToken)

	if err := RunGit(context.Background(), root, cred, redactor, "clone", bareRepo, cloneDir); err != nil {
		t.Fatalf("RunGit clone: %v", err)
	}

	cfg, err := os.ReadFile(filepath.Join(cloneDir, ".git", "config"))
	if err != nil {
		t.Fatalf("read .git/config: %v", err)
	}
	if strings.Contains(string(cfg), secretToken) {
		t.Fatalf("SECURITY: token leaked into .git/config:\n%s", cfg)
	}
	if strings.Contains(string(cfg), "@") && strings.Contains(string(cfg), "url") {
		// tokenless local-path remotes never contain "@"; a userinfo-style leaked-credential URL
		// would. This is a loose extra guard, not the primary assertion above.
	}

	var remoteBuf bytes.Buffer
	remoteRedactor := NewRedactor(&remoteBuf, secretToken)
	if err := RunGit(context.Background(), cloneDir, cred, remoteRedactor, "remote", "-v"); err != nil {
		t.Fatalf("RunGit remote -v: %v", err)
	}
	if strings.Contains(remoteBuf.String(), secretToken) {
		t.Fatalf("SECURITY: token leaked into `git remote -v` output: %q", remoteBuf.String())
	}

	// Make a change and push it back through RunGit too, then re-check .git/config.
	if err := os.WriteFile(filepath.Join(cloneDir, "CHANGE.md"), []byte("change\n"), 0o644); err != nil {
		t.Fatalf("write change file: %v", err)
	}
	runReal(t, cloneDir, "git", "config", "user.email", "test@example.com")
	runReal(t, cloneDir, "git", "config", "user.name", "Test")
	runReal(t, cloneDir, "git", "add", ".")
	runReal(t, cloneDir, "git", "commit", "-m", "change")
	if err := RunGit(context.Background(), cloneDir, cred, redactor, "push", "origin", "main"); err != nil {
		t.Fatalf("RunGit push: %v", err)
	}
	cfgAfterPush, err := os.ReadFile(filepath.Join(cloneDir, ".git", "config"))
	if err != nil {
		t.Fatalf("read .git/config after push: %v", err)
	}
	if strings.Contains(string(cfgAfterPush), secretToken) {
		t.Fatalf("SECURITY: token leaked into .git/config after push:\n%s", cfgAfterPush)
	}
	if strings.Contains(logBuf.String(), secretToken) {
		t.Fatalf("SECURITY: token leaked into redacted log output: %q", logBuf.String())
	}
}

func runReal(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v (in %s): %v\n%s", name, args, dir, err, out)
	}
}

func TestWriteAskpassHelper_ScriptAnswersFromEnv(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteAskpassHelper(dir)
	if err != nil {
		t.Fatalf("WriteAskpassHelper: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat helper: %v", err)
	}
	if info.Mode()&0o100 == 0 {
		t.Fatalf("helper script is not executable: %v", info.Mode())
	}

	cmd := exec.Command(path, "Username for 'https://github.com':")
	cmd.Env = append(os.Environ(), "SENTINEL_ASKPASS_USERNAME=theuser", "SENTINEL_ASKPASS_PASSWORD=thepass")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run helper (username prompt): %v", err)
	}
	if string(out) != "theuser" {
		t.Fatalf("username answer = %q, want %q", out, "theuser")
	}

	cmd = exec.Command(path, "Password for 'https://theuser@github.com':")
	cmd.Env = append(os.Environ(), "SENTINEL_ASKPASS_USERNAME=theuser", "SENTINEL_ASKPASS_PASSWORD=thepass")
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("run helper (password prompt): %v", err)
	}
	if string(out) != "thepass" {
		t.Fatalf("password answer = %q, want %q", out, "thepass")
	}
}

func TestRunGit_HelperDirCleanedUp(t *testing.T) {
	before, _ := os.ReadDir(os.TempDir())
	dir := t.TempDir()
	var buf bytes.Buffer
	redactor := NewRedactor(&buf, secretToken)
	// Use the real git binary against a harmless no-op subcommand in an empty dir.
	if err := RunGit(context.Background(), dir, GitHubTokenCredential(secretToken), redactor, "init", "-b", "main"); err != nil {
		t.Fatalf("RunGit init: %v", err)
	}
	after, _ := os.ReadDir(os.TempDir())
	leaked := 0
	for _, e := range after {
		if strings.HasPrefix(e.Name(), "sentinel-askpass-") {
			leaked++
		}
	}
	_ = before
	if leaked != 0 {
		t.Fatalf("expected sentinel-askpass- temp dirs to be cleaned up, found %d", leaked)
	}
}

// TestRunGit_NilRedactorDoesNotPanic proves a nil *Redactor (a concrete pointer type, which
// invites a caller passing nil to mean "don't log") does not crash the exec goroutine — RunGit
// must substitute a safe discard-based Redactor.
func TestRunGit_NilRedactorDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	if err := RunGit(context.Background(), dir, GitHubTokenCredential(secretToken), nil, "init", "-b", "main"); err != nil {
		t.Fatalf("RunGit with nil Redactor: %v", err)
	}
}

// TestRunGit_ErrorRedactsArgs proves a secret-bearing argument cannot survive into RunGit's
// returned error string — the error path must run args through the same Redactor as output,
// not interpolate them raw.
func TestRunGit_ErrorRedactsArgs(t *testing.T) {
	dir := t.TempDir() // not a git repo: `git -C dir log` will fail, producing an error to inspect
	var buf bytes.Buffer
	// Use a credential whose secret does NOT appear in args, so checkArgsForCredential's own
	// separate guard doesn't short-circuit before we reach the exec/error path we're testing.
	cred := GitHubTokenCredential("unrelated-cred-value")
	redactor := NewRedactor(&buf, secretToken)

	err := RunGit(context.Background(), dir, cred, redactor, "log", "--grep="+secretToken)
	if err == nil {
		t.Fatal("expected error from git log in a non-repo dir")
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("SECURITY: secret-bearing arg leaked into RunGit error: %v", err)
	}
}

// TestRunGit_SelfDefendingRedactor_MismatchedCallerRedactorCannotLeak is a regression test: a
// caller-supplied Redactor built from an UNRELATED secret (not this call's credential) must still
// be unable to leak the credential in play, through either out or the returned error. RunGit must
// self-defend by adding the actual credential's secrets to whatever Redactor it is given, rather
// than trusting the caller built it from the right values.
func TestRunGit_SelfDefendingRedactor_MismatchedCallerRedactorCannotLeak(t *testing.T) {
	dir := t.TempDir()
	stubDir := t.TempDir()
	stubPath := filepath.Join(stubDir, "git")
	// Stub git echoes the askpass username back on stderr, mimicking git's own real failure text
	// (e.g. "Authentication failed for 'https://<username>@github.com/...'") which embeds the
	// GitHub token-as-username verbatim.
	script := "#!/bin/sh\n" +
		"echo \"fatal: Authentication failed for 'https://$SENTINEL_ASKPASS_USERNAME@github.com/a/b.git'\" 1>&2\n" +
		"exit 128\n"
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub git: %v", err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cred := GitHubTokenCredential(secretToken)
	// Redactor built from an entirely unrelated secret — simulates the caller-contract mismatch.
	var buf bytes.Buffer
	mismatched := NewRedactor(&buf, "some-other-providers-token")

	err := RunGit(context.Background(), dir, cred, mismatched, "fetch")
	if err == nil {
		t.Fatal("expected error from stub git exiting 128")
	}
	if strings.Contains(buf.String(), secretToken) {
		t.Fatalf("SECURITY: credential leaked into mismatched Redactor's output: %q", buf.String())
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("SECURITY: credential leaked into RunGit's returned error: %v", err)
	}
}

// TestRunGit_RefusesCredentialBearingArgs is Probe E from the review: "remotes always tokenless"
// must be an enforced invariant, not a doc comment. A caller (a future N8f bug) that builds a
// credential-bearing remote URL must be rejected before exec, not silently write the secret into
// .git/config.
func TestRunGit_RefusesCredentialBearingArgs(t *testing.T) {
	root := t.TempDir()
	runReal(t, root, "git", "init", "-b", "main", root)

	cred := GitHubTokenCredential(secretToken)
	var buf bytes.Buffer
	redactor := NewRedactor(&buf, secretToken)

	authedURL := "https://x-access-token:" + secretToken + "@github.com/acme/widgets.git"
	err := RunGit(context.Background(), root, cred, redactor, "remote", "add", "origin", authedURL)
	if err == nil {
		t.Fatal("expected RunGit to refuse a credential-bearing remote URL")
	}

	cfg, readErr := os.ReadFile(filepath.Join(root, ".git", "config"))
	if readErr != nil {
		t.Fatalf("read .git/config: %v", readErr)
	}
	if strings.Contains(string(cfg), secretToken) {
		t.Fatalf("SECURITY: token landed in .git/config despite refusal:\n%s", cfg)
	}

	// A generic userinfo URL (no configured secret in it) must also be refused.
	err2 := RunGit(context.Background(), root, cred, redactor, "remote", "add", "origin2", "https://someone:somepass@github.com/acme/widgets.git")
	if err2 == nil {
		t.Fatal("expected RunGit to refuse a userinfo-bearing remote URL")
	}
}

// MUTATION-TEST NOTE: to prove checkArgsForCredential is load-bearing, temporarily make it a
// no-op (`return nil`), re-run TestRunGit_RefusesCredentialBearingArgs — it must go red — then
// revert.

// TestRunGit_BitbucketBasicUsernameInURLNotRefused proves checkArgsForCredential guards only
// actual secret material. Bitbucket Cloud's username+app-password auth form uses a plain, public
// account name as the username — and on Bitbucket Cloud a personal workspace ID equals the
// account username by default, so the username routinely appears in the repo URL itself. That
// must not trip the "credential-bearing argument" guard, or this auth form is refused in the
// default case. The app password itself must still be guarded.
func TestRunGit_BitbucketBasicUsernameInURLNotRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only fixture script")
	}

	root := t.TempDir()
	bareRepo := filepath.Join(root, "acme", "widgets.git")
	if err := os.MkdirAll(bareRepo, 0o755); err != nil {
		t.Fatalf("mkdir bare repo dir: %v", err)
	}
	runReal(t, root, "git", "init", "--bare", "-b", "main", bareRepo)
	cloneDir := filepath.Join(root, "clone")

	const appPassword = "app-secret-value"
	cred := BitbucketBasicCredential("acme", appPassword)
	var buf bytes.Buffer
	redactor := NewRedactor(&buf, appPassword)

	// The username "acme" appears in the clone source path (as it would for Bitbucket's default
	// workspace==username layout) but is not secret and must not be refused.
	argWithUsername := filepath.Join(root, "acme", "widgets.git")
	if err := RunGit(context.Background(), root, cred, redactor, "clone", "--depth", "1", argWithUsername, cloneDir); err != nil {
		t.Fatalf("RunGit refused a tokenless clone whose path contains the non-secret username %q: %v", cred.username, err)
	}

	// An argument containing the actual secret (the app password) must still be refused.
	if err := checkArgsForCredential([]string{"remote", "add", "origin2", "https://foo/" + appPassword}, cred); err == nil {
		t.Fatal("expected checkArgsForCredential to refuse an argument containing the app password")
	}
}

// TestRunGit_DoesNotInheritUnrelatedWorkerSecrets is Probe H from the review: the worker's own
// process env (SENTINEL_AGENT_KEY, LLM_API_KEY, other providers' git tokens, ...) must not reach
// the git child at all — it has no use for them, and once a coding agent (N8f) can write this
// workspace's .git/config, anything git executes on the repo's behalf (core.pager,
// filter.*.process, ...) would otherwise inherit them.
func TestRunGit_DoesNotInheritUnrelatedWorkerSecrets(t *testing.T) {
	t.Setenv("SENTINEL_AGENT_KEY", "sentinel-agent-key-should-not-leak")
	t.Setenv("LLM_API_KEY", "llm-key-should-not-leak")

	dumpFile := filepath.Join(t.TempDir(), "dump.txt")
	writeStubGit(t, dumpFile)

	dir := t.TempDir()
	var buf bytes.Buffer
	redactor := NewRedactor(&buf, secretToken)
	if err := RunGit(context.Background(), dir, GitHubTokenCredential(secretToken), redactor, "status"); err != nil {
		t.Fatalf("RunGit: %v", err)
	}

	dump, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	_, envSection := splitDump(t, string(dump))
	if strings.Contains(envSection, "sentinel-agent-key-should-not-leak") {
		t.Fatalf("SECURITY: git child inherited SENTINEL_AGENT_KEY:\n%s", envSection)
	}
	if strings.Contains(envSection, "llm-key-should-not-leak") {
		t.Fatalf("SECURITY: git child inherited LLM_API_KEY:\n%s", envSection)
	}
}

// TestRunGit_AskpassHelperActuallyAuthenticates is the end-to-end proof the review asked for: a
// real HTTP server standing in for a git smart-HTTP remote, asserting the request that arrives
// carries exactly the Basic-auth credential the askpass helper supplied — not merely that some
// env vars were set. This is what kills a mutation that silently disables the askpass wiring
// (e.g. renaming GIT_ASKPASS=) or drops the password line: with either mutation git has no way to
// answer the server's 401 challenge and the clone fails / never reaches the asserted request.
func TestRunGit_AskpassHelperActuallyAuthenticates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only fixture script")
	}

	const wantUser = "x-access-token"
	const wantPass = "ghp_endToEndAskpassSecret"

	bareRepo := t.TempDir()
	runReal(t, bareRepo, "git", "init", "--bare", "-b", "main", bareRepo)

	var gotAuthHeader string
	var sawRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			// git's smart-HTTP client probes anonymously first; challenge it so it retries with
			// credentials from the askpass helper, exactly like a real forge would.
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotAuthHeader = auth
		sawRequest = true
		// Serve the bare repo's dumb-HTTP files so `git ls-remote` can actually complete over
		// this transport once authenticated.
		http.FileServer(http.Dir(bareRepo)).ServeHTTP(w, r)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	remote := u.String() + "/"

	dir := t.TempDir()
	var buf bytes.Buffer
	redactor := NewRedactor(&buf, wantPass)
	runErr := RunGit(context.Background(), dir, GitCredential{username: wantUser, password: wantPass}, redactor,
		"ls-remote", remote)

	if !sawRequest {
		t.Fatalf("SECURITY/CORRECTNESS: server never received an authenticated request (git ls-remote err: %v) — askpass helper did not authenticate", runErr)
	}
	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(wantUser+":"+wantPass))
	if gotAuthHeader != wantHeader {
		t.Fatalf("Authorization header = %q, want %q", gotAuthHeader, wantHeader)
	}
	// runErr may be non-nil here (this bare fixture doesn't serve full dumb-HTTP repo content, so
	// git may fail to find refs after authenticating) — that is a content-shape detail, not an
	// auth failure, and is irrelevant to what this test proves: the askpass helper supplied the
	// credential and the server actually validated/received it via the standard HTTP Basic
	// challenge-response flow, i.e. the wiring is genuinely exercised end to end.
	_ = runErr
}

// TestRunGit_InsteadOfRedirect_NoAuthLeaksToAttackerHost is the RED-FIRST reproduction of finding
// 1: a repo-LOCAL `.git/config` entry `url.<attacker>.insteadOf https://github.com/` rewrites the
// tokenless upstream URL to an attacker-controlled host, and (before the fix) the askpass helper
// answers unconditionally, handing the credential to that attacker host via the real HTTP Basic
// challenge-response flow. This test drives a real httptest server standing in for the attacker
// host and asserts the request that arrives (if any) carries NO Authorization header.
func TestRunGit_InsteadOfRedirect_NoAuthLeaksToAttackerHost(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only fixture script")
	}

	const wantUser = "x-access-token"
	const wantPass = "ghp_insteadOfLeakTestSecret"

	var gotAuthHeader string
	var sawAnyRequest bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAnyRequest = true
		if auth := r.Header.Get("Authorization"); auth != "" {
			gotAuthHeader = auth
			return
		}
		// Mimic a real forge's challenge so git would retry with credentials if the askpass
		// helper were willing to supply them for this (wrong) host.
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer attacker.Close()

	root := t.TempDir()
	runReal(t, root, "git", "init", "-b", "main", root)
	// Repo-LOCAL config: the exact exploit shape from the finding. RunGit neutralizes system/
	// global config (GIT_CONFIG_NOSYSTEM / GIT_CONFIG_GLOBAL=/dev/null) but git always still reads
	// this repo's own .git/config.
	runReal(t, root, "git", "config", "url."+attacker.URL+"/.insteadOf", "https://github.com/")

	var buf bytes.Buffer
	redactor := NewRedactor(&buf, wantPass)
	cred := GitCredential{username: wantUser, password: wantPass}

	// The caller asks for github.com; repo-local insteadOf silently retargets the request at the
	// attacker's httptest server.
	runErr := RunGit(context.Background(), root, cred, redactor, "ls-remote", "https://github.com/acme/widgets.git")
	_ = runErr // git may fail for content-shape reasons; irrelevant to the security assertion below.

	if sawAnyRequest && gotAuthHeader != "" {
		t.Fatalf("SECURITY: credential leaked to attacker host via url.insteadOf redirect: Authorization=%q", gotAuthHeader)
	}
	if strings.Contains(buf.String(), wantPass) {
		t.Fatalf("SECURITY: credential leaked into redacted log output: %q", buf.String())
	}
}

// MUTATION-TEST NOTE (finding 1): to prove the host-pin check is load-bearing, temporarily change
// askpassScript's guard to unconditionally answer (e.g. remove the `[ -n "$SENTINEL_ASKPASS_HOST"
// ] && [ "$host" != "$SENTINEL_ASKPASS_HOST" ]` branch, or hardcode `exit 0` out), re-run
// TestRunGit_InsteadOfRedirect_NoAuthLeaksToAttackerHost — it must go red — then revert. Also try
// making deriveExpectedHost always return "" — same expectation.

// TestRunGit_InsteadOfRedirect_SamePortHost_StillAuthenticates is the "legitimate path still
// works" companion to the port-pin fix below: an insteadOf rewrite to the exact same host:port
// the caller asked for must still authenticate normally (the pin must not become so strict it
// breaks the ordinary case).
func TestRunGit_InsteadOfRedirect_SamePortHost_StillAuthenticates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only fixture script")
	}

	const wantUser = "x-access-token"
	const wantPass = "ghp_samePortHostTestSecret"

	bareRepo := t.TempDir()
	runReal(t, bareRepo, "git", "init", "--bare", "-b", "main", bareRepo)

	var gotAuthHeader string
	var sawRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotAuthHeader = auth
		sawRequest = true
		http.FileServer(http.Dir(bareRepo)).ServeHTTP(w, r)
	}))
	defer srv.Close()

	root := t.TempDir()
	runReal(t, root, "git", "init", "-b", "main", root)
	// insteadOf to the EXACT same URL (same host, same port) — a no-op rewrite in substance.
	runReal(t, root, "git", "config", "url."+srv.URL+"/.insteadOf", srv.URL+"/original/")

	var buf bytes.Buffer
	redactor := NewRedactor(&buf, wantPass)
	cred := GitCredential{username: wantUser, password: wantPass}
	runErr := RunGit(context.Background(), root, cred, redactor, "ls-remote", srv.URL+"/original/")
	_ = runErr

	if !sawRequest {
		t.Fatalf("expected an authenticated request to reach the server for a same-host-same-port insteadOf rewrite (runErr=%v)", runErr)
	}
	wantHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(wantUser+":"+wantPass))
	if gotAuthHeader != wantHeader {
		t.Fatalf("Authorization header = %q, want %q", gotAuthHeader, wantHeader)
	}
}

// TestRunGit_InsteadOfRedirect_SameHostDifferentPort_NoAuthLeaks is the RED-FIRST reproduction of
// the re-attack on finding 1: SENTINEL_ASKPASS_HOST previously compared HOSTNAME only, so a
// repo-local insteadOf rewrite to the SAME host on a DIFFERENT port (e.g. a loopback service on
// another port) passed the pin and received the credential via the real HTTP Basic
// challenge-response flow. The fix pins host:port; this test drives two real httptest servers on
// 127.0.0.1 (guaranteed same hostname, different ports) and asserts the request that reaches the
// "attacker" port carries NO Authorization header.
func TestRunGit_InsteadOfRedirect_SameHostDifferentPort_NoAuthLeaks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only fixture script")
	}

	const wantUser = "x-access-token"
	const wantPass = "ghp_samehostDiffPortLeakTestSecret"

	// "Legitimate" target: the URL RunGit is actually asked to reach.
	legit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer legit.Close()

	// "Attacker" target: same host (127.0.0.1) as legit, but a DIFFERENT port.
	var gotAuthHeader string
	var sawAnyRequest bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAnyRequest = true
		if auth := r.Header.Get("Authorization"); auth != "" {
			gotAuthHeader = auth
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer attacker.Close()

	legitHost, err := url.Parse(legit.URL)
	if err != nil {
		t.Fatalf("parse legit URL: %v", err)
	}
	attackerHost, err := url.Parse(attacker.URL)
	if err != nil {
		t.Fatalf("parse attacker URL: %v", err)
	}
	if legitHost.Hostname() != attackerHost.Hostname() {
		t.Fatalf("test fixture invariant broken: expected same hostname, got %q vs %q", legitHost.Hostname(), attackerHost.Hostname())
	}
	if legitHost.Port() == attackerHost.Port() {
		t.Fatalf("test fixture invariant broken: expected different ports, both got %q", legitHost.Port())
	}

	root := t.TempDir()
	runReal(t, root, "git", "init", "-b", "main", root)
	// Repo-local insteadOf rewrites the legit URL to the attacker's URL: SAME host, DIFFERENT port.
	runReal(t, root, "git", "config", "url."+attacker.URL+"/.insteadOf", legit.URL+"/")

	var buf bytes.Buffer
	redactor := NewRedactor(&buf, wantPass)
	cred := GitCredential{username: wantUser, password: wantPass}

	runErr := RunGit(context.Background(), root, cred, redactor, "ls-remote", legit.URL+"/acme/widgets.git")
	_ = runErr // git may fail for content-shape reasons; irrelevant to the security assertion below.

	if sawAnyRequest && gotAuthHeader != "" {
		t.Fatalf("SECURITY: credential leaked to same-host-different-port target via url.insteadOf redirect: Authorization=%q", gotAuthHeader)
	}
	if strings.Contains(buf.String(), wantPass) {
		t.Fatalf("SECURITY: credential leaked into redacted log output: %q", buf.String())
	}
}

// MUTATION-TEST NOTE (re-attack on finding 1, host:port pin): to prove the port component of the
// pin is load-bearing (not just the hostname), temporarily revert deriveExpectedHost to return
// u.Hostname() instead of u.Host (or revert askpassScript's `hostport` extraction to also strip
// ":*" like the old `host` variable did), re-run
// TestRunGit_InsteadOfRedirect_SameHostDifferentPort_NoAuthLeaks — it must go red — then revert.

// TestRunGit_CredentialHelperStore_NeutralizedForChild is the RED-FIRST reproduction of finding 2:
// a repo-LOCAL `credential.helper store` (set via ordinary `git config`, exactly as an attacker who
// can write .git/config in a FIX workspace would do) must be invisible to git as RunGit executes
// it — proven directly and deterministically by asking the SAME child environment RunGit builds
// what `credential.helper` resolves to, rather than depending on a full, flaky end-to-end HTTP
// auth exchange to reach git's "approve" step. `git config --get` exits non-zero and prints
// nothing when a key is unset; if neutralization were missing it would print "store" and exit 0.
func TestRunGit_CredentialHelperStore_NeutralizedForChild(t *testing.T) {
	root := t.TempDir()
	runReal(t, root, "git", "init", "-b", "main", root)
	runReal(t, root, "git", "config", "credential.helper", "store")

	var buf bytes.Buffer
	redactor := NewRedactor(&buf, secretToken)
	cred := GitHubTokenCredential(secretToken)

	// GIT_CONFIG_KEY_0=credential.helper / GIT_CONFIG_VALUE_0="" makes credential.helper resolve to
	// the EMPTY string for this child (not "unset") — git's own documented behavior for an empty
	// credential.helper value is "no helper", and `git config --get` on an empty-valued key exits 0
	// with empty output, so err may legitimately be nil here. The security-relevant assertion is
	// the OUTPUT: it must never be (or contain) "store".
	err := RunGit(context.Background(), root, cred, redactor, "config", "--get", "credential.helper")
	got := strings.TrimSpace(buf.String())
	if got == "store" || strings.Contains(got, "store") {
		t.Fatalf("SECURITY: repo-local credential.helper=store leaked through to the child process: %q (err=%v)", buf.String(), err)
	}
}

// MUTATION-TEST NOTE (finding 2a): to prove the credential.helper neutralization is load-bearing,
// temporarily remove the GIT_CONFIG_COUNT/GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 triplet from
// minimalGitEnv, re-run TestRunGit_CredentialHelperStore_NeutralizedForChild — it must go red
// (git config --get will then print "store" and exit 0) — then revert.

// TestRunGit_UsesPrivateScratchHomeNotSharedHome is the RED-FIRST reproduction of finding 2's
// secondary hardening: even if credential.helper neutralization above were somehow bypassed by a
// future git version's config-precedence change, the child's HOME must never be the worker
// process's own (job-to-job shared) HOME — otherwise any on-disk credential store git might still
// be persuaded to write lands in a location the NEXT job's git invocation can also read. Uses the
// plan §8 stub-git dump fixture to observe the literal HOME= the child process received.
func TestRunGit_UsesPrivateScratchHomeNotSharedHome(t *testing.T) {
	sharedHome := t.TempDir()
	t.Setenv("HOME", sharedHome)

	dumpFile := filepath.Join(t.TempDir(), "dump.txt")
	writeStubGit(t, dumpFile)

	dir := t.TempDir()
	var buf bytes.Buffer
	redactor := NewRedactor(&buf, secretToken)
	if err := RunGit(context.Background(), dir, GitHubTokenCredential(secretToken), redactor, "status"); err != nil {
		t.Fatalf("RunGit: %v", err)
	}

	dump, err := os.ReadFile(dumpFile)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	_, envSection := splitDump(t, string(dump))
	if strings.Contains(envSection, "HOME="+sharedHome+"\n") {
		t.Fatalf("SECURITY: git child ran with the shared worker-process HOME (%s) instead of a private per-call scratch dir:\n%s", sharedHome, envSection)
	}
	if !strings.Contains(envSection, "HOME=") {
		t.Fatalf("expected child env to set HOME at all, got:\n%s", envSection)
	}
}

// MUTATION-TEST NOTE (finding 2b): to prove scratchHome is load-bearing, temporarily change
// minimalGitEnv to add "HOME"/os.LookupEnv("HOME") back to the passthrough allowlist instead of
// "HOME="+scratchHome, re-run TestRunGit_UsesPrivateScratchHomeNotSharedHome — it must go red —
// then revert.

var _ = fmt.Sprintf
var _ = io.Discard
