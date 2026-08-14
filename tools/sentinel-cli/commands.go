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
	"link":     cmdLink,
	"unlink":   cmdUnlink,
	"projects": cmdProjects,
	"whoami":   cmdWhoami,
	"events":   cmdEvents,
	"batch":    cmdBatch,
	"upload":   cmdUpload,
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

// cmdComment posts a non-blocking comment. NOTE (API-contract mismatch, server wins): the agent
// comment op (apps/dashboard-web/src/lib/server/agent-ops.ts's issuesComment) reads only body_md
// and attachment_ids from the request body — it does not read or honor a parent/thread id, even
// though the underlying createComment() query does support one. -parent is still accepted here
// (sent as "parent_id" in the body) so this stays forward-compatible if the server starts reading
// it, but as of this writing the server silently ignores it. See README.md.
func cmdComment(ctx context.Context, e *env, args []string) int {
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

// cmdWhoami. NOTE (API-contract mismatch, documented per the task): there is no identity-returning
// route under /api/agent/* — authenticateAgentRequest() (agent-auth.ts) never echoes agentId/
// agentName/organizationId back to the caller anywhere. So whoami is implemented as an auth probe:
// it calls GET /api/agent/issues (the cheapest read route) and reports reachability + the
// configured URL/key prefix, rather than any real identity. A 401/403 here means the key is
// wrong/revoked/expired; any 2xx means the key is valid, but this command cannot tell the caller
// WHICH agent or org it authenticated as.
func cmdWhoami(ctx context.Context, e *env, args []string) int {
	fmt.Fprintf(e.stdout, "url: %s\n", e.client.BaseURL)
	fmt.Fprintf(e.stdout, "key: %s\n", keyPrefix(e.client.Key))

	res, err := e.client.Do(ctx, "GET", "/api/agent/issues", url.Values{"waiting": []string{"true"}}, nil)
	if err != nil {
		fmt.Fprintf(e.stdout, "reachable: no (%v)\n", err)
		return ExitNetwork
	}

	code := exitCodeForStatus(res.Status)
	if code == ExitOK {
		fmt.Fprintln(e.stdout, "auth: ok (no dedicated identity endpoint exists; this only confirms the key authenticates)")
		return ExitOK
	}
	fmt.Fprintf(e.stdout, "auth: failed (%d %s)\n", res.Status, errorMessage(res.Body))
	return code
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

// cmdUpload. NOTE (API-contract mismatch, server wins): POST /api/agent/uploads
// (apps/dashboard-web/src/routes/api/agent/uploads/+server.ts, via upload-core.ts's
// handleAttachmentUpload) does NOT take an issueId at all — the row is inserted with
// `issueId: null` and only later associated with an issue by passing the returned attachment id
// to `sentinel comment --attachment <id>`. The <issueId> positional here is accepted for
// consistency with every other issue-scoped command and printed in the response context, but it
// is never sent to the server. See README.md.
func cmdUpload(ctx context.Context, e *env, args []string) int {
	if len(args) != 2 {
		return usageErr(e, "usage: sentinel upload <issueId> <file>")
	}
	issueID, filePath := args[0], args[1]
	_ = issueID // accepted for CLI symmetry only; the server route has no issueId param — see doc comment.

	f, err := os.Open(filePath)
	if err != nil {
		return e.networkErr(fmt.Errorf("opening %s: %w", filePath, err))
	}
	defer f.Close()

	base := filePath
	if idx := strings.LastIndexAny(filePath, "/\\"); idx >= 0 {
		base = filePath[idx+1:]
	}

	res, err := e.client.UploadFile(ctx, "/api/agent/uploads", filePath, f, base)
	if err != nil {
		return e.networkErr(err)
	}
	return e.respond(res, "")
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
