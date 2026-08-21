package gitprovider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// GitHubTokenCredential builds the GitCredential for a GitHub fine-grained PAT. GitHub's HTTPS
// auth accepts the token as the username with any non-empty password.
func GitHubTokenCredential(token string) GitCredential {
	return GitCredential{username: token, password: "x-oauth-basic", usernameIsSecret: true}
}

// BitbucketTokenCredential builds the GitCredential for a Bitbucket Cloud access token.
// Bitbucket's HTTPS auth accepts "x-token-auth" as the username with the token as the password.
func BitbucketTokenCredential(token string) GitCredential {
	return GitCredential{username: "x-token-auth", password: token}
}

// BitbucketBasicCredential builds the GitCredential for Bitbucket Cloud username + app-password
// auth.
func BitbucketBasicCredential(username, appPassword string) GitCredential {
	return GitCredential{username: username, password: appPassword}
}

// askpassScript is the helper git invokes via GIT_ASKPASS. It never receives the secret as an
// argument: git calls it as `<script> "Username for '...'"` / `<script> "Password for '...'"` on
// its OWN argv (which this package does not control), and the script answers by reading two env
// vars that RunGit sets ONLY in this one child process's environment — never persisted to disk,
// never passed on this package's own command line. POSIX sh so it runs on any git installation
// without a further interpreter dependency.
//
// Host pinning (finding 1, and re-attack finding: the pin must include the PORT too): a
// repo-LOCAL `.git/config` entry such as `url.<attacker>.insteadOf https://github.com/` is
// honored by git even though RunGit neutralizes system/global config — it rewrites the target of
// the very request this helper is about to authenticate. Git invokes the askpass helper with a
// prompt that names the URL/host it is ACTUALLY about to contact (i.e. the rewritten,
// attacker-controlled host when insteadOf fired), so the helper parses that host:port out of its
// own $1 and refuses to answer (prints nothing, exit 0 — never hangs, never errors) unless it
// matches SENTINEL_ASKPASS_HOST, the host:port RunGit derived from the caller-supplied args
// BEFORE any git-side rewriting could occur. Comparing hostname alone let an insteadOf rewrite to
// the SAME host on a DIFFERENT port (e.g. a loopback service listening on another port) pass the
// pin and receive the credential; the "host" extracted below deliberately keeps the port (only
// scheme/userinfo/path are stripped) so it must match host AND port exactly. An empty
// SENTINEL_ASKPASS_HOST (no URL found in args to pin to) disables the check rather than refusing
// every prompt.
const askpassScript = `#!/bin/sh
prompt="$1"
url=$(printf '%s\n' "$prompt" | sed -n "s/.*for '\(.*\)':.*/\1/p")
hostport="${url#*://}"
hostport="${hostport#*@}"
hostport="${hostport%%/*}"
if [ -n "$SENTINEL_ASKPASS_HOST" ] && [ "$hostport" != "$SENTINEL_ASKPASS_HOST" ]; then
	exit 0
fi
case "$prompt" in
	Username*) printf '%s' "$SENTINEL_ASKPASS_USERNAME" ;;
	*) printf '%s' "$SENTINEL_ASKPASS_PASSWORD" ;;
esac
`

// WriteAskpassHelper writes the askpass helper script into dir (which must already exist) and
// returns its path. The caller is responsible for the directory's lifetime/permissions — RunGit
// uses a private per-call temp dir so no two concurrent git invocations share (or leak into) one
// another's helper.
func WriteAskpassHelper(dir string) (string, error) {
	path := filepath.Join(dir, "askpass.sh")
	if err := os.WriteFile(path, []byte(askpassScript), 0o700); err != nil {
		return "", fmt.Errorf("gitprovider: write askpass helper: %w", err)
	}
	return path, nil
}

// RunGit executes git in dir with the given credential wired through the askpass mechanism: the
// secret reaches the child process ONLY via the inherited environment of that single `git`
// invocation (SENTINEL_ASKPASS_USERNAME / SENTINEL_ASKPASS_PASSWORD, read back out by the helper
// script above), never as a command-line argument, never written into dir/.git/config, and never
// embedded in a remote URL — callers MUST configure remotes with a tokenless URL. Combined
// stdout+stderr is written to out (callers should wrap out in a Redactor so any secret that a
// misbehaving git subcommand might echo is stripped before it reaches a log or the journal).
//
// GIT_TERMINAL_PROMPT=0 ensures a misconfigured helper fails fast instead of hanging on an
// interactive prompt (which would also risk the secret going to a real terminal instead of this
// controlled path).
func RunGit(ctx context.Context, dir string, cred GitCredential, out *Redactor, args ...string) error {
	return runGit(ctx, dir, cred, out, deriveExpectedHost(args), args...)
}

// RunGitWithHost is RunGit but pins SENTINEL_ASKPASS_HOST to the caller-supplied expectedHost
// instead of deriving it from args (finding 1: `git push -u origin <branch>` carries no URL in
// args at all, so deriveExpectedHost(args) returns "" and the askpass pin is disabled --
// answering a credential prompt for ANY host, including one a repo-local
// `url.<attacker>.insteadOf` rewrite of origin substitutes in). Callers performing any
// authenticated git operation whose target host is NOT necessarily present in args (chiefly
// `push` to a bare remote name) MUST derive expectedHost themselves from the operation's own
// known-good clone URL (never from the repo's current, attacker-writable remote config) and pass
// it here so the pin is always active for a credentialed request.
func RunGitWithHost(ctx context.Context, dir string, cred GitCredential, out *Redactor, expectedHost string, args ...string) error {
	return runGit(ctx, dir, cred, out, expectedHost, args...)
}

func runGit(ctx context.Context, dir string, cred GitCredential, out *Redactor, expectedHost string, args ...string) error {
	if out == nil {
		out = NewRedactor(io.Discard, cred.username, cred.password)
	}
	// Self-defending: never trust that a caller-supplied Redactor was built from exactly this
	// credential's secrets. A mismatch here would let git's own failure text (which can embed the
	// credential verbatim, e.g. GitHub's token-as-username in "Authentication failed for
	// 'https://<token>@github.com/...'") reach out unredacted, and from there the log/journal sink.
	out.AddSecrets(cred.username, cred.password)

	if err := checkArgsForCredential(args, cred); err != nil {
		return err
	}

	helperDir, err := os.MkdirTemp("", "sentinel-askpass-")
	if err != nil {
		return fmt.Errorf("gitprovider: create askpass dir: %w", err)
	}
	defer os.RemoveAll(helperDir)

	helperPath, err := WriteAskpassHelper(helperDir)
	if err != nil {
		return err
	}

	// scratchHome (finding 2, secondary hardening) gives this one invocation a private, empty HOME
	// so that even if credential.helper neutralization below somehow failed to take effect, there
	// is no shared $HOME/.git-credentials for a `credential.helper store` (set via repo-local
	// config) to write the token into, and no cross-job leakage through a shared HOME either.
	scratchHome := filepath.Join(helperDir, "home")
	if err := os.MkdirAll(scratchHome, 0o700); err != nil {
		return fmt.Errorf("gitprovider: create scratch HOME dir: %w", err)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	// Build the child's environment explicitly rather than inheriting os.Environ(): the worker's
	// own process env carries SENTINEL_AGENT_KEY, LLM keys, and every OTHER provider's git token,
	// none of which git needs and all of which become reachable to anything git executes on the
	// repo's behalf (core.sshCommand, core.pager, filter.*.process, credential.helper, ... — all
	// attacker-controlled once a coding agent can write .git/config in this workspace, N8f). Only
	// the minimal set git itself needs, plus the askpass wiring, is passed through.
	cmd.Env = minimalGitEnv(helperPath, cred, expectedHost, scratchHome)

	// buf captures raw (unredacted) output ONLY in memory, to build an error message — it is never
	// written to a log or the journal directly. It is redacted via out.Redact before being placed
	// into the returned error, so any secret git might echo (e.g. into a diagnostic message)
	// cannot leak through the error path either.
	var buf bytes.Buffer
	w := io.MultiWriter(out, &buf)
	cmd.Stdout = w
	cmd.Stderr = w

	runErr := cmd.Run()
	// Flush must run before out is used for anything else: the streaming path can hold back up
	// to maxLen-1 bytes of legitimate output between Write calls to catch a secret straddling a
	// chunk boundary (redactor.go), and that tail must be emitted before the caller observes out
	// as "done".
	out.Flush()
	if err := runErr; err != nil {
		// args is redacted too: a caller-supplied argument (a credential-bearing URL, a
		// `-c http.extraheader=...`) must not reach this error string raw, since this string is
		// what the worker's failure taxonomy logs/journals.
		redactedArgs := out.Redact([]byte(strings.Join(args, " ")))
		return fmt.Errorf("gitprovider: git %s: %w: %s", redactedArgs, err, out.Redact(buf.Bytes()))
	}
	return nil
}

// minimalGitEnv builds the child git process's environment from scratch: a small allowlist of
// vars git/the OS resolver needs, plus the askpass wiring. GIT_CONFIG_NOSYSTEM and
// GIT_CONFIG_GLOBAL=/dev/null ensure only the target repo's own local config applies, so a
// system/global config on the host cannot inject a credential.helper or similar — but a
// repo-LOCAL config (which this package cannot avoid honoring; git has no "ignore local config"
// switch) still can, which is what the two findings below defend against:
//
//   - finding 1: expectedHost, derived by the caller from the operation's own target URL BEFORE
//     any git-side rewriting, is exported as SENTINEL_ASKPASS_HOST so the askpass script (see
//     askpassScript) can refuse to answer a prompt for any OTHER host — defeating a repo-local
//     `url.<attacker>.insteadOf https://github.com/` that would otherwise silently redirect the
//     authenticated request (and the token with it) to an attacker-controlled host.
//   - finding 2: credential.helper is neutralized for this child via the GIT_CONFIG_COUNT /
//     GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n environment mechanism, which git treats as
//     higher-priority than any file-based config (including repo-local) — a repo-local
//     `credential.helper store` would otherwise cause git to run `credential approve` after a
//     successful askpass auth and write the token in plaintext to $HOME/.git-credentials. Setting
//     the value to empty clears any credential.helper list accumulated from file config, and the
//     argv git actually runs is untouched. scratchHome additionally points HOME at a private,
//     per-call scratch directory so no on-disk credential store (this or any other) can persist
//     across calls or jobs even if a future git version changes this precedence.
func minimalGitEnv(askpassHelperPath string, cred GitCredential, expectedHost string, scratchHome string) []string {
	env := []string{
		"GIT_ASKPASS=" + askpassHelperPath,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"SENTINEL_ASKPASS_USERNAME=" + cred.username,
		"SENTINEL_ASKPASS_PASSWORD=" + cred.password,
		"SENTINEL_ASKPASS_HOST=" + expectedHost,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
		"HOME=" + scratchHome,
	}
	for _, name := range []string{"PATH", "TMPDIR", "LANG", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// deriveExpectedHost scans args for the first http(s) URL argument and returns its host:port
// (u.Host, which includes an explicit port when present and is just the hostname otherwise), for
// SENTINEL_ASKPASS_HOST (finding 1; re-attack: the pin must cover the port, not just the
// hostname). Callers of RunGit pass the operation's target URL as a plain argument (see this
// package's own doc comment: "remotes always tokenless" — the URL, not a pre-configured remote
// name, is how RunGit's own callers and tests invoke clone/fetch/push/remote-add), so the FIRST
// such URL in args is the host:port this call is actually meant to reach, before any repo-local
// url.*.insteadOf rewriting can substitute a different one (including a same-host, different-port
// rewrite). Returns "" when no URL argument is present (e.g. a plain `git status`), in which case
// host pinning is a no-op — there is nothing to pin to and no credential-bearing request is in
// play for that call shape.
func deriveExpectedHost(args []string) string {
	for _, a := range args {
		u, err := url.Parse(a)
		if err != nil {
			continue
		}
		if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return u.Host
		}
	}
	return ""
}

// userinfoURLPattern matches a URL containing embedded userinfo (scheme://user[:pass]@host), the
// classic shape of a credential-bearing remote — the exact leak plan §4.5 mandates remotes must
// never carry.
var userinfoURLPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://[^/@\s]+@`)

// checkArgsForCredential refuses to exec git if any argument would write the credential's own
// secret values into the repo permanently (e.g. `git remote add origin https://user:pass@...`),
// or otherwise embeds userinfo in a URL — "remotes always tokenless" must be an enforced
// invariant, not just documentation. Returns a permanent *Error-shaped failure via fmt.Errorf
// (not a network Error — there is no HTTP status here) so callers can distinguish it from git's
// own exit-status failures if needed.
func checkArgsForCredential(args []string, cred GitCredential) error {
	for _, a := range args {
		for _, secret := range cred.secrets() {
			if strings.Contains(a, secret) {
				return fmt.Errorf("gitprovider: refusing git argument containing credential secret material")
			}
		}
		if userinfoURLPattern.MatchString(a) {
			return fmt.Errorf("gitprovider: refusing git argument with userinfo embedded in a URL: remotes must be tokenless")
		}
	}
	return nil
}
