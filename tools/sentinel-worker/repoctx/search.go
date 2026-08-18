package repoctx

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
)

// maxSearchPatternLen bounds the pattern's own length (resource-exhaustion guard, not a
// correctness requirement — git grep -e takes the pattern as one literal argv value regardless of
// its content, so there is no injection surface here beyond size).
const maxSearchPatternLen = 500

// maxSearchResults / maxSearchBytes are plan §4.5's "result-capped" bound on search_code.
const (
	maxSearchResults = 200
	maxSearchBytes   = 32 << 10
)

// globPattern is the charset an optional search_code glob/pathspec may use: word characters,
// path separators, and the shell-glob metacharacters git pathspecs understand. It must not begin
// with '-' (flag injection) and must not be an absolute path or contain "..".
var globPattern = regexp.MustCompile(`^[A-Za-z0-9._*?/\[\]-]{1,200}$`)

func validateGlob(glob string) error {
	if glob == "" {
		return nil
	}
	if !globPattern.MatchString(glob) {
		return fmt.Errorf("repoctx: invalid glob %q", glob)
	}
	if glob[0] == '-' {
		return fmt.Errorf("repoctx: glob %q must not begin with '-'", glob)
	}
	if glob[0] == '/' {
		return fmt.Errorf("repoctx: glob %q must not be absolute", glob)
	}
	for i := 0; i+1 < len(glob); i++ {
		if glob[i] == '.' && glob[i+1] == '.' {
			return fmt.Errorf("repoctx: glob %q must not contain '..'", glob)
		}
	}
	return nil
}

// SearchCode runs `git grep -n` confined to repo.Root (plan §4.5). pattern is passed to git via
// `-e`, never interpolated into a shell string (exec.Command with an explicit argv — there is no
// shell in this path at all), so pattern content cannot inject additional git flags regardless of
// its own leading characters. glob, when non-empty, is validated and passed as a trailing
// pathspec after a literal "--".
func SearchCode(ctx context.Context, repo *Repo, pattern, glob string) (string, error) {
	if repo == nil {
		return "", fmt.Errorf("repoctx: nil repo")
	}
	if pattern == "" {
		return "", fmt.Errorf("repoctx: empty pattern")
	}
	if len(pattern) > maxSearchPatternLen {
		return "", fmt.Errorf("repoctx: pattern exceeds %d bytes", maxSearchPatternLen)
	}
	if err := validateGlob(glob); err != nil {
		return "", err
	}

	args := []string{"grep", "-n", "--no-color", "-I", "-e", pattern, "--"}
	if glob != "" {
		args = append(args, glob)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo.Root
	cmd.Env = minimalReadEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// git grep exits 1 for "no matches" — that's a valid empty result, not an error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && stderr.Len() == 0 {
			return "", nil
		}
		return "", fmt.Errorf("repoctx: git grep: %w: %s", err, stderr.String())
	}

	return capSearchOutput(stdout.Bytes()), nil
}

// capSearchOutput enforces plan §4.5's "result-capped" bound: at most maxSearchResults lines and
// maxSearchBytes total, whichever comes first.
func capSearchOutput(out []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var buf bytes.Buffer
	lines := 0
	truncated := false
	for scanner.Scan() {
		lines++
		if lines > maxSearchResults {
			truncated = true
			break
		}
		if buf.Len() >= maxSearchBytes {
			truncated = true
			break
		}
		buf.Write(scanner.Bytes())
		buf.WriteByte('\n')
	}
	res := buf.Bytes()
	if len(res) > maxSearchBytes {
		res = res[:maxSearchBytes]
		truncated = true
	}
	if truncated {
		return string(res) + truncationMarker
	}
	return string(res)
}

// minimalReadEnv builds a small, deliberate environment for the read-only git subprocesses this
// file runs (git grep needs no credential — it never talks to a remote), consistent with
// gitprovider.RunGit's own "don't inherit the whole process env" posture (repoctx never wants a
// stray GIT_ASKPASS/GIT_CONFIG_* from the ambient environment influencing a plain local read).
func minimalReadEnv() []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	}
	for _, name := range []string{"PATH", "TMPDIR", "LANG", "HOME"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}
