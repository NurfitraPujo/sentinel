package gitprovider

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// maxErrorBodyBytes bounds how much of a forge's error response body ends up in an Error, per
// this file's own invariant that Body is "already truncated/redacted by the caller before
// wrapping" — an unbounded io.ReadAll of a hostile/misbehaving forge response must not end up
// verbatim in a string the worker's failure taxonomy logs and journals.
const maxErrorBodyBytes = 8 << 10

// maxResponseBytes bounds a SUCCESS response body read (CreatePR/PRStatus decode paths). Forge PR
// objects routinely embed full repository/user sub-objects and url templates and are commonly
// 20-30KB, far larger than an error body — reusing maxErrorBodyBytes there truncated valid JSON
// mid-object and made every real CreatePR/PRStatus call fail with "unexpected end of JSON input".
const maxResponseBytes = 1 << 20

// readErrorBody reads at most maxErrorBodyBytes from body and redacts every secret out of it
// before returning it as a string, satisfying Error.Body's documented invariant. Callers pass the
// provider credential's own secret values (token / app password) as secrets.
func readErrorBody(body io.Reader, secrets ...string) string {
	limited, _ := io.ReadAll(io.LimitReader(body, maxErrorBodyBytes))
	red := NewRedactor(io.Discard, secrets...)
	return string(red.Redact(limited))
}

// Error wraps a forge REST API failure with the plan's failure taxonomy (reusing
// sentinel.FailureClass — plan says "reuse the module's error classes" rather than inventing a
// second parallel one): 401/403 -> ClassAuthFailure (triggers credential re-fetch upstream, C16),
// 404 -> ClassGone (not-found), 429/5xx -> ClassRateLimited/ClassTransient, everything else ->
// ClassPermanent.
type Error struct {
	Provider string // "github" | "bitbucket"
	Op       string // "CreatePR" | "PRStatus"
	Status   int
	Body     string // response body, already truncated/redacted by the caller before wrapping
	Class    sentinel.FailureClass
}

func (e *Error) Error() string {
	return fmt.Sprintf("gitprovider(%s): %s: http %d: %s", e.Provider, e.Op, e.Status, e.Body)
}

// classifyStatus maps an HTTP status from a forge REST API onto the shared taxonomy.
func classifyStatus(status int) sentinel.FailureClass {
	switch {
	case status >= 200 && status < 300:
		return sentinel.ClassSuccess
	case status == 401 || status == 403:
		return sentinel.ClassAuthFailure
	case status == 404:
		return sentinel.ClassGone
	case status == 429:
		return sentinel.ClassRateLimited
	case status >= 500:
		return sentinel.ClassTransient
	default:
		return sentinel.ClassPermanent
	}
}

// newError builds a classified *Error for a non-2xx forge response.
func newError(provider, op string, status int, body string) *Error {
	return &Error{Provider: provider, Op: op, Status: status, Body: body, Class: classifyStatus(status)}
}

// IsAuthFailure reports whether err represents a git-auth failure (C16, finding 5): either a
// classified *Error from CreatePR/PRStatus whose Class is sentinel.ClassAuthFailure (a REST
// 401/403), or a plain error from RunGit (clone/push have no HTTP status to classify — the git CLI
// only ever returns exit-status + stderr text) whose message contains git's own
// "Authentication failed" / "could not read Username"/"invalid credentials" phrasing. The text
// heuristic is deliberately narrow (git's own wording is stable across versions/providers) rather
// than matching on any non-zero git exit, so a transient network failure or a merge conflict is
// never misclassified as an auth failure.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var gerr *Error
	if errors.As(err, &gerr) {
		return gerr.Class == sentinel.ClassAuthFailure
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "could not read username") ||
		strings.Contains(msg, "invalid credentials") ||
		strings.Contains(msg, "http basic: access denied") ||
		strings.Contains(msg, "403") && strings.Contains(msg, "forbidden")
}
