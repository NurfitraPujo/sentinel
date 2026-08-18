// Package gitprovider is the worker's Git-forge abstraction (plan §4.5): a Provider interface
// implemented for GitHub and Bitbucket Cloud, plus the askpass-helper mechanism (gitauth.go) that
// lets git authenticate without ever putting a secret in argv, .git/config, git remotes, logs, or
// the journal.
//
// Token hygiene is the point of this package. A Provider never returns a URL carrying a secret —
// Auth() returns an opaque GitCredential consumed only by RunGit's askpass plumbing. Every field
// that leaves this package as PR title/body text is expected to already be harness-templated
// (plan §4.4) by the time it reaches PRSpec; this package does not interpret or sanitize that
// text, it only carries it to the forge's REST API.
package gitprovider

import (
	"context"
	"fmt"
	"regexp"
)

// ownerRepoPattern and prIDPattern constrain the values a Provider will interpolate into a forge
// REST URL. The upstream source (dashboard agent-settings validation) only checks non-empty, so
// this package cannot trust RepoRef.Owner/Repo or a PR id to be free of path-traversal or query
// injection characters — validate here, at the point those values are turned into a URL.
var (
	ownerRepoPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	prIDPattern      = regexp.MustCompile(`^[0-9]{1,19}$`)
)

// validateRepoRef rejects an owner/repo pair that doesn't match the strict charset forges use,
// returning a permanent Error rather than letting fmt.Sprintf interpolate it raw into a URL.
func validateRepoRef(provider, op string, repo RepoRef) error {
	if !ownerRepoPattern.MatchString(repo.Owner) || isDotsOnly(repo.Owner) {
		return newError(provider, op, 0, fmt.Sprintf("invalid RepoRef.Owner %q", repo.Owner))
	}
	if !ownerRepoPattern.MatchString(repo.Repo) || isDotsOnly(repo.Repo) {
		return newError(provider, op, 0, fmt.Sprintf("invalid RepoRef.Repo %q", repo.Repo))
	}
	return nil
}

// isDotsOnly reports whether s consists entirely of '.' characters (".", "..", "...", ...). The
// charset ownerRepoPattern allows (needed for real owner/repo names like "my.org") also admits
// these path-traversal segments; a value of "." or ".." interpolated into a forge REST URL like
// /repos/{owner}/{repo}/pulls collapses the path (e.g. Owner=Repo=".." -> /repos/../../pulls),
// letting a normalizing intermediary or origin route an authenticated request to an unintended
// endpoint with the forge token attached.
func isDotsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c != '.' {
			return false
		}
	}
	return true
}

// validatePRID rejects a PR id that isn't a plain decimal number, the only shape GitHub/Bitbucket
// PR identifiers take.
func validatePRID(provider, op, id string) error {
	if !prIDPattern.MatchString(id) {
		return newError(provider, op, 0, fmt.Sprintf("invalid PR id %q", id))
	}
	return nil
}

// RepoRef identifies a repository on a specific provider. Owner is the GitHub owner/org or the
// Bitbucket workspace; Repo is the repository name or Bitbucket repo slug.
type RepoRef struct {
	Provider string // "github" | "bitbucket"
	Owner    string
	Repo     string
}

// PRSpec is the harness-templated pull-request payload (plan §4.4/§4.5): Title and Body are
// produced by the N8f harness template, never raw model prose, by the time they reach this
// package. Head/Base are branch names, not refs (no "refs/heads/" prefix).
type PRSpec struct {
	Title string
	Body  string
	Head  string // source branch (the fix branch)
	Base  string // target branch (project.agentSettings.repo.defaultBranch)
}

// PR is the minimal result of a successful CreatePR call.
type PR struct {
	ID     string // provider-native PR identifier, as a string (GitHub PR number, Bitbucket PR id)
	Number int
	URL    string
}

// PRState is the provider-agnostic pull-request lifecycle state (plan §4.5).
type PRState string

const (
	PRStateOpen     PRState = "open"
	PRStateMerged   PRState = "merged"
	PRStateDeclined PRState = "declined"
)

// GitCredential is opaque secret material consumed only by gitauth.go's askpass helper. It is
// deliberately not a URL and exposes no accessor outside this package — callers pass it straight
// into RunGit, which is the only place it is read.
type GitCredential struct {
	username string
	password string
	// usernameIsSecret marks whether username itself is secret material that must be guarded
	// against appearing in git argv (GitHub's token-as-username and Bitbucket's constant
	// "x-token-auth" are not secrets in the argv-leak sense for the latter, but GitHub's IS the
	// token). Bitbucket basic auth's username is a plain, non-secret account name and must NOT be
	// guarded, or a tokenless clone/remote-add against a URL containing that username is wrongly
	// refused.
	usernameIsSecret bool
}

// secrets returns the credential's own secret material — the set of values that must never
// appear in a git argument (see checkArgsForCredential). It intentionally excludes non-secret
// identifiers such as Bitbucket's plain account username or its "x-token-auth" constant.
func (c GitCredential) secrets() []string {
	var out []string
	if c.usernameIsSecret && c.username != "" {
		out = append(out, c.username)
	}
	if c.password != "" {
		out = append(out, c.password)
	}
	return out
}

// Provider is the per-forge implementation the worker's FIX flow (N8f) drives: authenticate git
// operations via Auth(), then open and poll a pull request via CreatePR/PRStatus.
type Provider interface {
	// Auth returns the opaque credential material for this provider, consumed by RunGit's
	// askpass helper — never a URL, never suitable for logging.
	Auth() GitCredential
	CreatePR(ctx context.Context, repo RepoRef, pr PRSpec) (PR, error)
	PRStatus(ctx context.Context, repo RepoRef, id string) (PRState, error)
}
