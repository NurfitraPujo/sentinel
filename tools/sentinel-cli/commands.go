package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// env is everything a command needs to run: the resolved client, output format, and the streams
// to write to. Kept as a struct rather than package globals so tests can point stdout/stderr at
// buffers and swap in an httptest.Server's client.
type env struct {
	client *Client
	format string
	stdout io.Writer
	stderr io.Writer
}

// cmdFunc is one subcommand. args are everything after the subcommand name (global -url/-key/
// -format have already been consumed by main). Returns the process exit code.
type cmdFunc func(ctx context.Context, e *env, args []string) int

// commands is the dispatch table used by both main() and tests.
var commands = map[string]cmdFunc{
	"issues":   cmdIssues,
	"claim":    cmdClaim,
	"release":  cmdRelease,
	"status":   cmdStatus,
	"comment":  cmdComment,
	"comments": cmdComments,
	"question": cmdQuestion,
	"progress": cmdProgress,
	"severity": cmdSeverity,
	"link":     cmdLink,
	"unlink":   cmdUnlink,
	"projects": cmdProjects,
	"whoami":   cmdWhoami,
	"events":   cmdEvents,
	"batch":    cmdBatch,
	"upload":   cmdUpload,
	"key":      cmdKey,
}

// reorderArgs moves every "-flag value" (or "-flag=value", or a bare boolean "-flag") pair to the
// front of args, preserving the relative order of positional arguments and of flags. Go's flag
// package stops parsing at the first non-flag token, but every subcommand in this program is
// documented (and tested) as `<positional...> [--flag value...]` — flags trailing the positional
// arguments — so callers must reorder before calling fs.Parse. boolFlags lists flag names (without
// leading dashes) that take no value.
func reorderArgs(args []string, boolFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if boolFlags[name] {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positional...)
}

// respond prints a request Result in the configured format and returns the exit code its status
// maps to. On a non-2xx status it prints the server's error message to stderr (per the contract:
// "non-zero exit prints the server error message to stderr").
func (e *env) respond(res *Result, listKey string) int {
	code := exitCodeForStatus(res.Status)
	if code == ExitOK {
		if err := output(e.stdout, e.format, res.Body, listKey); err != nil {
			fmt.Fprintln(e.stderr, "error rendering response:", err)
			return ExitNetwork
		}
		return ExitOK
	}
	fmt.Fprintf(e.stderr, "error: server returned %d: %s\n", res.Status, errorMessage(res.Body))
	return code
}

func (e *env) networkErr(err error) int {
	fmt.Fprintln(e.stderr, "error:", err)
	return ExitNetwork
}

func usageErr(e *env, format string, args ...interface{}) int {
	fmt.Fprintf(e.stderr, format+"\n", args...)
	return ExitUsage
}

// --- issues -----------------------------------------------------------------------------------

// cmdIssues dispatches `issues list|get|occurrences`.
func cmdIssues(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 {
		return usageErr(e, "usage: sentinel issues <list|get|occurrences> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return cmdIssuesList(ctx, e, rest)
	case "get":
		return cmdIssuesGet(ctx, e, rest)
	case "occurrences":
		return cmdIssuesOccurrences(ctx, e, rest)
	default:
		return usageErr(e, "unknown issues subcommand %q: want list, get, or occurrences", sub)
	}
}

func cmdIssuesList(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("issues list", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	typ := fs.String("type", "", `filter: "user_report" or "system_error" (omit for any)`)
	claimed := fs.String("claimed", "", `filter: "true" or "false"`)
	project := fs.String("project", "", "filter: project id")
	waiting := fs.String("waiting", "", `filter: "true" to show only issues waiting on a reply`)
	since := fs.String("since", "", "ISO timestamp: only issues first seen at or after this time")
	sort := fs.String("sort", "", `sort column: "firstSeen" or "lastSeen" (default lastSeen)`)
	limit := fs.String("limit", "", "max rows per page (server clamps to [1,200]; omit for unbounded legacy list)")
	cursor := fs.String("cursor", "", "opaque keyset cursor from a prior response's nextCursor")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	q := url.Values{}
	if *typ != "" {
		q.Set("type", *typ)
	}
	if *claimed != "" {
		q.Set("claimed", *claimed)
	}
	if *project != "" {
		q.Set("project", *project)
	}
	if *waiting != "" {
		q.Set("waiting", *waiting)
	}
	if *since != "" {
		q.Set("since", *since)
	}
	if *sort != "" {
		q.Set("sort", *sort)
	}
	if *limit != "" {
		q.Set("limit", *limit)
	}
	if *cursor != "" {
		q.Set("cursor", *cursor)
	}

	res, err := e.client.Do(ctx, "GET", "/api/agent/issues", q, nil)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "issues")
}

func cmdIssuesGet(ctx context.Context, e *env, args []string) int {
	if len(args) != 1 {
		return usageErr(e, "usage: sentinel issues get <issueId>")
	}
	res, err := e.client.Do(ctx, "GET", "/api/agent/issues/"+url.PathEscape(args[0]), nil, nil)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

func cmdIssuesOccurrences(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("issues occurrences", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	limit := fs.String("limit", "", "max rows (server clamps to [1,50], default 20)")
	before := fs.String("before", "", "ISO timestamp cursor: only occurrences before this time")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageErr(e, "usage: sentinel issues occurrences <issueId> [--limit N] [--before TS]")
	}

	q := url.Values{}
	if *limit != "" {
		q.Set("limit", *limit)
	}
	if *before != "" {
		q.Set("before", *before)
	}

	res, err := e.client.Do(ctx, "GET", "/api/agent/issues/"+url.PathEscape(rest[0])+"/occurrences", q, nil)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "occurrences")
}

// --- claim / release ----------------------------------------------------------------------------

func cmdClaim(ctx context.Context, e *env, args []string) int {
	if len(args) != 1 {
		return usageErr(e, "usage: sentinel claim <issueId>")
	}
	res, err := e.client.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(args[0])+"/claim", nil, map[string]interface{}{})
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

func cmdRelease(ctx context.Context, e *env, args []string) int {
	if len(args) != 1 {
		return usageErr(e, "usage: sentinel release <issueId>")
	}
	res, err := e.client.Do(ctx, "DELETE", "/api/agent/issues/"+url.PathEscape(args[0])+"/claim", nil, map[string]interface{}{})
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

// --- status --------------------------------------------------------------------------------------

var validStatuses = map[string]bool{"unresolved": true, "resolved": true, "ignored": true}

func cmdStatus(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	resolvedIn := fs.String("resolved-in", "", "version string to record when transitioning to resolved")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return usageErr(e, "usage: sentinel status <issueId> <unresolved|resolved|ignored> [--resolved-in VERSION]")
	}
	issueID, status := rest[0], rest[1]
	if !validStatuses[status] {
		return usageErr(e, "status must be one of: unresolved, resolved, ignored")
	}

	body := map[string]interface{}{"status": status}
	if *resolvedIn != "" {
		body["resolved_in_version"] = *resolvedIn
	}

	res, err := e.client.Do(ctx, "PATCH", "/api/agent/issues/"+url.PathEscape(issueID)+"/status", nil, body)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

// --- comment / comments --------------------------------------------------------------------------

// cmdComment dispatches `comment edit|delete <issueId> <commentId> ...` (A08, N7e) to their own
// subcommand handlers; anything else falls through to the original bare
// `comment <issueId> --body <md>` post-a-comment behavior below, preserved byte-for-byte for
// backward compatibility (an issueId happening to literally be "edit" or "delete" is not a real
// concern — the server's ids are never bare English words).
func cmdComment(ctx context.Context, e *env, args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "edit":
			return cmdCommentEdit(ctx, e, args[1:])
		case "delete":
			return cmdCommentDelete(ctx, e, args[1:])
		}
	}
	return cmdCommentPost(ctx, e, args)
}

// cmdCommentPost posts a non-blocking comment. NOTE (API-contract mismatch, server wins): the
// agent comment op (apps/dashboard-web/src/lib/server/agent-ops.ts's issuesComment) reads only
// body_md and attachment_ids from the request body — it does not read or honor a parent/thread
// id, even though the underlying createComment() query does support one. -parent is still
// accepted here (sent as "parent_id" in the body) so this stays forward-compatible if the server
// starts reading it, but as of this writing the server silently ignores it. See README.md.
func cmdCommentPost(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("comment", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	body := fs.String("body", "", "comment body (Markdown), required")
	parent := fs.String("parent", "", "parent comment id (NOT currently honored by the server — see README)")
	var attachments stringList
	fs.Var(&attachments, "attachment", "attachment id to include; repeatable")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageErr(e, "usage: sentinel comment <issueId> --body <md> [--parent <commentId>] [--attachment <id> ...]")
	}
	if *body == "" {
		return usageErr(e, "--body is required")
	}

	reqBody := map[string]interface{}{"body_md": *body}
	if *parent != "" {
		reqBody["parent_id"] = *parent
	}
	if len(attachments) > 0 {
		reqBody["attachment_ids"] = []string(attachments)
	}

	res, err := e.client.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(rest[0])+"/comments", nil, reqBody)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

func cmdComments(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("comments", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	after := fs.String("after", "", "ISO timestamp: only comments after this time")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageErr(e, "usage: sentinel comments <issueId> [--after <ts>]")
	}

	q := url.Values{}
	if *after != "" {
		q.Set("after", *after)
	}
	res, err := e.client.Do(ctx, "GET", "/api/agent/issues/"+url.PathEscape(rest[0])+"/comments", q, nil)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "comments")
}

// A08 (N7e): edit/delete your OWN comment. PATCH/DELETE
// /api/agent/issues/{issueId}/comments/{commentId} — 403 if the calling agent didn't author it
// (no moderator carve-out for agents, see the server route's doc comment), 404 if it belongs to a
// different issue or no longer exists.

func cmdCommentEdit(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("comment edit", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	body := fs.String("body", "", "new comment body (Markdown), required")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return usageErr(e, "usage: sentinel comment edit <issueId> <commentId> --body <md>")
	}
	if *body == "" {
		return usageErr(e, "--body is required")
	}

	issueID, commentID := rest[0], rest[1]
	reqBody := map[string]interface{}{"body_md": *body}
	res, err := e.client.Do(
		ctx, "PATCH",
		"/api/agent/issues/"+url.PathEscape(issueID)+"/comments/"+url.PathEscape(commentID),
		nil, reqBody,
	)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

func cmdCommentDelete(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("comment delete", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return usageErr(e, "usage: sentinel comment delete <issueId> <commentId>")
	}

	issueID, commentID := rest[0], rest[1]
	res, err := e.client.Do(
		ctx, "DELETE",
		"/api/agent/issues/"+url.PathEscape(issueID)+"/comments/"+url.PathEscape(commentID),
		nil, nil,
	)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

// --- severity --------------------------------------------------------------------------------------

var validSeverities = map[string]bool{"low": true, "medium": true, "high": true, "critical": true}

// cmdSeverity sets the severity of a user_report issue (A09, N7e). PATCH
// /api/agent/issues/{issueId}/report/severity — the server 400s if the issue isn't a user_report.
func cmdSeverity(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("severity", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return usageErr(e, "usage: sentinel severity <issueId> <low|medium|high|critical>")
	}
	issueID, severity := rest[0], rest[1]
	if !validSeverities[severity] {
		return usageErr(e, "severity must be one of: low, medium, high, critical")
	}

	res, err := e.client.Do(
		ctx, "PATCH",
		"/api/agent/issues/"+url.PathEscape(issueID)+"/report/severity",
		nil, map[string]interface{}{"severity": severity},
	)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

// --- question --------------------------------------------------------------------------------------

func cmdQuestion(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("question", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	body := fs.String("body", "", "question body (Markdown), required")
	waitingOn := fs.String("waiting-on", "", `who this blocks on: "reporter" or "team", required`)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageErr(e, "usage: sentinel question <issueId> --body <md> --waiting-on <reporter|team>")
	}
	if *body == "" {
		return usageErr(e, "--body is required")
	}
	if *waitingOn != "reporter" && *waitingOn != "team" {
		return usageErr(e, "--waiting-on must be \"reporter\" or \"team\"")
	}

	reqBody := map[string]interface{}{"body_md": *body, "audience": *waitingOn}
	res, err := e.client.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(rest[0])+"/questions", nil, reqBody)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

// --- progress --------------------------------------------------------------------------------------

func cmdProgress(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("progress", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	body := fs.String("body", "", "progress note (Markdown), required")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageErr(e, "usage: sentinel progress <issueId> --body <md>")
	}
	if *body == "" {
		return usageErr(e, "--body is required")
	}

	// The server op (issuesProgress in agent-ops.ts) reads "message_md", not "body_md" — matched
	// exactly here even though every other write command uses body_md, because that is what the
	// route actually reads.
	reqBody := map[string]interface{}{"message_md": *body}
	res, err := e.client.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(rest[0])+"/progress", nil, reqBody)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

// --- link / unlink --------------------------------------------------------------------------------

var validRelationTypes = map[string]bool{"linked_to": true, "caused_by": true, "duplicate_of": true}

func cmdLink(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	relType := fs.String("type", "", "linked_to | caused_by | duplicate_of, required")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return usageErr(e, "usage: sentinel link <issueId> <targetIssueId> --type <linked_to|caused_by|duplicate_of>")
	}
	if !validRelationTypes[*relType] {
		return usageErr(e, "--type must be one of: linked_to, caused_by, duplicate_of")
	}

	body := map[string]interface{}{"target_issue_id": rest[1], "relation_type": *relType}
	res, err := e.client.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(rest[0])+"/relations", nil, body)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

// cmdUnlink. NOTE (API-contract mismatch, server wins): the server's DELETE
// /api/agent/issues/[issueId]/relations op (issuesRelationsRemove in agent-ops.ts) identifies the
// relation to remove by {target_issue_id, relation_type} in the JSON body — there is no
// relation-id-based delete endpoint, even though relation rows do have their own id. So this
// command takes the same (targetIssueId, --type) shape as `link`, not a bare relation id.
func cmdUnlink(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("unlink", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	args = reorderArgs(args, nil)
	relType := fs.String("type", "", "linked_to | caused_by | duplicate_of, required")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return usageErr(e, "usage: sentinel unlink <issueId> <targetIssueId> --type <linked_to|caused_by|duplicate_of>")
	}
	if !validRelationTypes[*relType] {
		return usageErr(e, "--type must be one of: linked_to, caused_by, duplicate_of")
	}

	body := map[string]interface{}{"target_issue_id": rest[1], "relation_type": *relType}
	res, err := e.client.Do(ctx, "DELETE", "/api/agent/issues/"+url.PathEscape(rest[0])+"/relations", nil, body)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
}

// --- projects / whoami --------------------------------------------------------------------------------

func cmdProjects(ctx context.Context, e *env, args []string) int {
	res, err := e.client.Do(ctx, "GET", "/api/agent/projects", nil, nil)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "projects")
}

// cmdWhoami. R1a (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): upgraded to call the
// real identity route, GET /api/agent/self, instead of the old reachability-only probe against
// GET /api/agent/issues. Exit codes are unchanged (0 = ok, network/auth codes as before) so
// existing scripts checking `$?` keep working; only the printed output gained real identity
// fields.
func cmdWhoami(ctx context.Context, e *env, args []string) int {
	fmt.Fprintf(e.stdout, "url: %s\n", e.client.BaseURL)
	fmt.Fprintf(e.stdout, "key: %s\n", keyPrefix(e.client.Key))

	res, err := e.client.Do(ctx, "GET", "/api/agent/self", nil, nil)
	if err != nil {
		fmt.Fprintf(e.stdout, "reachable: no (%v)\n", err)
		return ExitNetwork
	}

	code := exitCodeForStatus(res.Status)
	if code != ExitOK {
		fmt.Fprintf(e.stdout, "auth: failed (%d %s)\n", res.Status, errorMessage(res.Body))
		return code
	}

	fmt.Fprintln(e.stdout, "auth: ok")
	if err := output(e.stdout, e.format, res.Body, ""); err != nil {
		fmt.Fprintln(e.stderr, "error rendering response:", err)
		return ExitNetwork
	}
	return ExitOK
}

// --- key -------------------------------------------------------------------------------------

// cmdKey dispatches `key rotate`. R1b (N7f): the only subcommand today.
func cmdKey(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 || args[0] != "rotate" {
		return usageErr(e, "usage: sentinel key rotate")
	}
	return cmdKeyRotate(ctx, e, args[1:])
}

// cmdKeyRotate calls POST /api/agent/key/rotate: mints a new key for the SAME agent this key
// already authenticates as, and leaves the calling key valid for its grace window
// (AGENT_KEY_ROTATION_GRACE_HOURS server-side). The new secret is printed to STDOUT exactly
// once, with a loud warning to STDERR — same one-time-reveal contract as the dashboard's own key
// creation UI. Reconfigure SENTINEL_AGENT_KEY (env, flag, or config file) with the new secret
// before the old key's grace window elapses.
func cmdKeyRotate(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("key rotate", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if len(fs.Args()) != 0 {
		return usageErr(e, "usage: sentinel key rotate")
	}

	res, err := e.client.Do(ctx, "POST", "/api/agent/key/rotate", nil, nil)
	if err != nil {
		return e.networkErr(err)
	}
	code := exitCodeForStatus(res.Status)
	if code != ExitOK {
		fmt.Fprintf(e.stderr, "error: server returned %d: %s\n", res.Status, errorMessage(res.Body))
		return code
	}

	var parsed struct {
		NewKey struct {
			ID     string `json:"id"`
			Prefix string `json:"prefix"`
			Secret string `json:"secret"`
		} `json:"newKey"`
	}
	if err := json.Unmarshal(res.Body, &parsed); err != nil {
		fmt.Fprintln(e.stderr, "error decoding rotate response:", err)
		return ExitNetwork
	}

	fmt.Fprintln(e.stderr, "WARNING: this secret is shown ONCE and never again. Store it now (e.g. update SENTINEL_AGENT_KEY) before it scrolls off your terminal.")
	fmt.Fprintln(e.stdout, parsed.NewKey.Secret)
	return ExitOK
}

// --- events --------------------------------------------------------------------------------------

func cmdEvents(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("events", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	after := fs.Int64("after", 0, "only events with seq greater than this")
	limit := fs.Int("limit", 0, "max events per page (server clamps/defaults)")
	typ := fs.String("type", "", "comma-separated event type filter")
	project := fs.String("project", "", "project id filter")
	claimedMe := fs.Bool("claimed-me", false, "only events for issues claimed by this agent")
	follow := fs.Bool("follow", false, "poll continuously, printing new events as NDJSON, persisting the cursor across restarts")
	interval := fs.Int("interval", 10, "poll interval in seconds for --follow")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	if *follow {
		return runEventsFollow(ctx, e, *limit, *typ, *project, *claimedMe, *interval)
	}
	return runEventsOnce(ctx, e, *after, *limit, *typ, *project, *claimedMe)
}

func eventsQuery(after int64, limit int, typ, project string, claimedMe bool) url.Values {
	q := url.Values{}
	if after > 0 {
		q.Set("after", fmt.Sprintf("%d", after))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	if typ != "" {
		q.Set("type", typ)
	}
	if project != "" {
		q.Set("project", project)
	}
	if claimedMe {
		q.Set("claimed", "me")
	}
	return q
}

func runEventsOnce(ctx context.Context, e *env, after int64, limit int, typ, project string, claimedMe bool) int {
	res, err := e.client.Do(ctx, "GET", "/api/agent/events", eventsQuery(after, limit, typ, project, claimedMe), nil)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "events")
}

// eventsFeed mirrors the shape documented in the task: {events:[{seq,...}],cursor,hasMore}.
type eventsFeed struct {
	Events  []json.RawMessage `json:"events"`
	Cursor  int64             `json:"cursor"`
	HasMore bool              `json:"hasMore"`
}

type eventSeq struct {
	Seq int64 `json:"seq"`
}

// cursorState is the on-disk shape persisted at $XDG_STATE_HOME/sentinel/events-cursor.json,
// keyed by "<baseURL>|<keyPrefix>" so multiple configured servers/keys don't clobber each other's
// cursor on the same machine.
type cursorState struct {
	Cursors map[string]int64 `json:"cursors"`
}

func cursorStatePath() (string, error) {
	dir, err := stateDir()
	if err != nil {
		return "", err
	}
	return dir + "/events-cursor.json", nil
}

func cursorKey(baseURL, key string) string {
	return baseURL + "|" + keyPrefix(key)
}

func loadCursorState(path string) (*cursorState, error) {
	st := &cursorState{Cursors: map[string]int64{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if st.Cursors == nil {
		st.Cursors = map[string]int64{}
	}
	return st, nil
}

func saveCursorState(path string, st *cursorState) error {
	dir := path[:strings.LastIndex(path, "/")]
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// runEventsFollow polls GET /api/agent/events in a loop, printing each new event as one NDJSON
// line to stdout and persisting the max seq seen to the cursor file after every successful poll,
// so a restarted process resumes where it left off. Stops cleanly when ctx is canceled (SIGINT).
func runEventsFollow(ctx context.Context, e *env, limit int, typ, project string, claimedMe bool, intervalSeconds int) int {
	path, err := cursorStatePath()
	if err != nil {
		return e.networkErr(err)
	}
	st, err := loadCursorState(path)
	if err != nil {
		return e.networkErr(err)
	}
	key := cursorKey(e.client.BaseURL, e.client.Key)
	cursor := st.Cursors[key]

	poll := func() (bool, int) {
		res, err := e.client.Do(ctx, "GET", "/api/agent/events", eventsQuery(cursor, limit, typ, project, claimedMe), nil)
		if err != nil {
			fmt.Fprintln(e.stderr, "poll error:", err)
			return false, ExitNetwork
		}
		if code := exitCodeForStatus(res.Status); code != ExitOK {
			fmt.Fprintf(e.stderr, "error: server returned %d: %s\n", res.Status, errorMessage(res.Body))
			return false, code
		}

		var feed eventsFeed
		if err := json.Unmarshal(res.Body, &feed); err != nil {
			fmt.Fprintln(e.stderr, "error decoding events feed:", err)
			return false, ExitNetwork
		}

		advanced := false
		for _, raw := range feed.Events {
			fmt.Fprintln(e.stdout, string(raw))
			var s eventSeq
			if err := json.Unmarshal(raw, &s); err == nil && s.Seq > cursor {
				cursor = s.Seq
				advanced = true
			}
		}
		if feed.Cursor > cursor {
			cursor = feed.Cursor
			advanced = true
		}
		if advanced {
			st.Cursors[key] = cursor
			if err := saveCursorState(path, st); err != nil {
				fmt.Fprintln(e.stderr, "warning: failed to persist cursor:", err)
			}
		}
		return true, ExitOK
	}

	for {
		ok, code := poll()
		if !ok {
			return code
		}
		select {
		case <-ctx.Done():
			return ExitOK
		case <-tick(intervalSeconds):
		}
	}
}

// --- batch --------------------------------------------------------------------------------------

type batchOperation struct {
	Op      string      `json:"op"`
	IssueID string      `json:"issueId"`
	Params  interface{} `json:"params,omitempty"`
}

type batchRequest struct {
	Operations []batchOperation `json:"operations"`
	// Pointer so "absent from the ops file" is distinguishable from an explicit false: absent is
	// omitted from the request and the server default (true) applies; a value in the file is
	// honored; --stop-on-error, when EXPLICITLY passed, wins over both (see cmdBatch).
	StopOnError *bool `json:"stopOnError,omitempty"`
}

func cmdBatch(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	file := fs.String("f", "", `path to a JSON file with {"operations":[...]}, or "-" for stdin`)
	stopOnError := fs.Bool("stop-on-error", true, "stop the batch after the first failing op (server default is also true)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if *file == "" {
		return usageErr(e, "usage: sentinel batch -f <ops.json|-> [--stop-on-error=false]")
	}

	var data []byte
	var err error
	if *file == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(*file)
	}
	if err != nil {
		return e.networkErr(fmt.Errorf("reading %s: %w", *file, err))
	}

	var req batchRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return usageErr(e, "invalid batch JSON: %v", err)
	}
	// Only an EXPLICIT --stop-on-error overrides a stopOnError set inside the ops file; the flag's
	// default must not silently clobber the file's value.
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "stop-on-error" {
			req.StopOnError = stopOnError
		}
	})

	res, err := e.client.Do(ctx, "POST", "/api/agent/batch", nil, req)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "results")
}

// --- upload --------------------------------------------------------------------------------------

// baseName returns the final path segment of filePath (handles both '/' and '\' separators).
func baseName(filePath string) string {
	base := filePath
	if idx := strings.LastIndexAny(filePath, "/\\"); idx >= 0 {
		base = filePath[idx+1:]
	}
	return base
}

// cmdUpload. A15 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7f): one-shot upload +
// comment. `sentinel upload <file> --issue <id> --comment "text"` uploads the file (POST
// /api/agent/uploads, which itself has no issueId param — it returns an unassociated attachment
// id) and then, when --issue is given, immediately posts a comment on that issue with
// `attachment_ids: [<id>]` (optionally with --comment's text as the body; empty body defaults to
// a fixed placeholder since postComment requires non-empty body_md). This replaces the old
// documented no-op: previously `<issueId>` was a bare positional the server never received, so
// "uploading to an issue" always took a second manual `sentinel comment --attachment <id>` call.
//
// Backward compatibility: the OLD two-positional form `sentinel upload <issueId> <file>` (no
// flags) is still accepted — it performs a plain upload with no follow-up comment, exactly like
// before, but now prints a deprecation warning to stderr pointing at the new flag form, since that
// positional issueId was always silently discarded.
func cmdUpload(ctx context.Context, e *env, args []string) int {
	fs := flag.NewFlagSet("upload", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	issueFlag := fs.String("issue", "", "issue id to comment on with the uploaded attachment")
	commentFlag := fs.String("comment", "", `comment body (Markdown) for the follow-up comment; defaults to "Uploaded attachment." if --issue is set and this is empty`)
	reordered := reorderArgs(args, nil)
	if err := fs.Parse(reordered); err != nil {
		return ExitUsage
	}
	rest := fs.Args()

	var filePath, issueID string
	switch {
	case *issueFlag != "":
		// New one-shot form: exactly one positional, the file.
		if len(rest) != 1 {
			return usageErr(e, `usage: sentinel upload <file> --issue <id> [--comment "text"]`)
		}
		filePath = rest[0]
		issueID = *issueFlag
	case len(rest) == 2:
		// Old two-positional form, kept for backward compatibility. The first positional used to
		// be silently discarded (the server route has no issueId param); it still is here — this
		// branch is a plain upload, same behavior as before this phase.
		fmt.Fprintln(e.stderr, `warning: "sentinel upload <issueId> <file>" is deprecated and the issueId is ignored; use "sentinel upload <file> --issue <id> --comment \"text\"" to upload AND comment in one call`)
		issueID, filePath = rest[0], rest[1]
		_ = issueID
	case len(rest) == 1:
		filePath = rest[0]
	default:
		return usageErr(e, `usage: sentinel upload <file> --issue <id> [--comment "text"]`)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return e.networkErr(fmt.Errorf("opening %s: %w", filePath, err))
	}
	defer f.Close()

	uploadRes, err := e.client.UploadFile(ctx, "/api/agent/uploads", filePath, f, baseName(filePath))
	if err != nil {
		return e.networkErr(err)
	}
	if code := exitCodeForStatus(uploadRes.Status); code != ExitOK {
		fmt.Fprintf(e.stderr, "error: server returned %d: %s\n", uploadRes.Status, errorMessage(uploadRes.Body))
		return code
	}

	if *issueFlag == "" {
		// No --issue: same as before, just the upload response.
		return e.respond(uploadRes, "")
	}

	var uploaded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(uploadRes.Body, &uploaded); err != nil || uploaded.ID == "" {
		fmt.Fprintln(e.stderr, "error: upload succeeded but response had no attachment id:", err)
		return ExitNetwork
	}

	commentBody := *commentFlag
	if commentBody == "" {
		commentBody = "Uploaded attachment."
	}
	reqBody := map[string]interface{}{
		"body_md":        commentBody,
		"attachment_ids": []string{uploaded.ID},
	}
	commentRes, err := e.client.Do(ctx, "POST", "/api/agent/issues/"+url.PathEscape(*issueFlag)+"/comments", nil, reqBody)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(commentRes, "")
}

// stringList implements flag.Value for a repeatable string flag (e.g. -attachment).
type stringList []string

func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(*s, ",")
}

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
