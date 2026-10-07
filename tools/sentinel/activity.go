package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// `sentinel issue activity <issueId> [--limit N] [--before CURSOR] [--all] [--json]` -- one issue's history from
// GET /api/agent/issues/{issueId}/activity, newest first. The server collapses a run of consecutive occurrence_burst
// rows into one entry (with `burst`), pages by an opaque cursor (`before=<nextCursor>`), and authorizes exactly like
// `issues get` (404 outside the key's scope). Unlike the JSON-first commands, the default output is a table for a
// person; --json prints the API response (with --all, every page merged into one response).

// maxActivityPages bounds --all: 1,000 pages of up to 200 entries is far beyond any issue's history.
const maxActivityPages = 1000

type activityActor struct {
	Type     string  `json:"type"`
	ID       string  `json:"id"`
	Name     *string `json:"name"`
	Personal bool    `json:"personal"`
}

type activityEntry struct {
	ID        string          `json:"id"`
	Seq       int64           `json:"seq"`
	EventType string          `json:"eventType"`
	ActorType string          `json:"actorType"`
	ActorID   string          `json:"actorId"`
	Actor     activityActor   `json:"actor"`
	OldValue  json.RawMessage `json:"oldValue"`
	NewValue  json.RawMessage `json:"newValue"`
	CreatedAt string          `json:"createdAt"`
	Comment   *struct {
		ID      string  `json:"id"`
		Excerpt *string `json:"excerpt"`
		Deleted bool    `json:"deleted"`
	} `json:"comment,omitempty"`
	Assignee *activityActor `json:"assignee,omitempty"`
	Burst    *struct {
		Rows            int64    `json:"rows"`
		FirstAt         string   `json:"firstAt"`
		LastAt          string   `json:"lastAt"`
		OccurrencesFrom *float64 `json:"occurrencesFrom"`
		OccurrencesTo   *float64 `json:"occurrencesTo"`
	} `json:"burst,omitempty"`
}

type activityPage struct {
	Entries    []json.RawMessage `json:"entries"`
	NextCursor *string           `json:"nextCursor"`
	HasMore    bool              `json:"hasMore"`
}

func cmdIssueActivity(ctx context.Context, e *env, args []string) int {
	const usage = "usage: sentinel issue activity <issueId> [--limit N] [--before CURSOR] [--all] [--json]"
	fs := flag.NewFlagSet("issue activity", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	limit := fs.Int("limit", 0, "entries per page (server: default 50, clamps to [1,200])")
	before := fs.String("before", "", "cursor from a previous page (printed after the table, or nextCursor in --json)")
	all := fs.Bool("all", false, "follow every page to the start of the history")
	asJSON := fs.Bool("json", false, "print the API response (JSON) instead of a table")
	if err := fs.Parse(reorderArgs(args, map[string]bool{"all": true, "json": true})); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageErr(e, usage)
	}
	if code, ok := e.checkIDs("issue id", rest[0]); !ok {
		return code
	}
	if *limit < 0 {
		return usageErr(e, "--limit must be a positive integer")
	}

	path := "/api/agent/issues/" + url.PathEscape(rest[0]) + "/activity"
	cursor := *before
	var entries []json.RawMessage
	var last activityPage
	for n := 0; ; n++ {
		q := url.Values{}
		if *limit > 0 {
			q.Set("limit", strconv.Itoa(*limit))
		}
		if cursor != "" {
			q.Set("before", cursor)
		}
		res, err := e.client.Do(ctx, "GET", path, q, nil)
		if err != nil {
			return e.networkErr(err)
		}
		if exitCodeForStatus(res.Status) != ExitOK {
			return e.httpFailure(res)
		}
		// A fresh value per page: decoding into the previous page's slice would reuse the RawMessage buffers already
		// appended to `entries` and overwrite them.
		var page activityPage
		if err := json.Unmarshal(res.Body, &page); err != nil {
			fmt.Fprintln(e.stderr, "error decoding response:", err)
			return ExitNetwork
		}
		last = page
		entries = append(entries, page.Entries...)
		if !*all || !last.HasMore || last.NextCursor == nil || n+1 >= maxActivityPages {
			break
		}
		cursor = *last.NextCursor
	}

	if *asJSON {
		merged, err := json.Marshal(map[string]interface{}{
			"entries":    entries,
			"nextCursor": nullIfAll(last.NextCursor, *all && !last.HasMore),
			"hasMore":    last.HasMore,
		})
		if err != nil {
			return exitFromWrite(e, err)
		}
		return exitFromWrite(e, printJSON(e.stdout, merged))
	}

	if err := printActivityTable(e.stdout, entries); err != nil {
		fmt.Fprintln(e.stderr, "error rendering response:", err)
		return ExitNetwork
	}
	if last.HasMore && last.NextCursor != nil {
		fmt.Fprintf(e.stderr, "older activity exists: rerun with --before %s (or --all)\n", *last.NextCursor)
	}
	return ExitOK
}

func nullIfAll(c *string, done bool) *string {
	if done {
		return nil
	}
	return c
}

func exitFromWrite(e *env, err error) int {
	if err != nil {
		fmt.Fprintln(e.stderr, "error rendering response:", err)
		return ExitNetwork
	}
	return ExitOK
}

// printActivityTable renders WHEN (UTC), WHO, WHAT, DETAIL -- one line per entry, a burst run as one line with its window.
func printActivityTable(w io.Writer, raw []json.RawMessage) error {
	if len(raw) == 0 {
		_, err := fmt.Fprintln(w, "(no activity)")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "WHEN (UTC)\tWHO\tWHAT\tDETAIL")
	for _, r := range raw {
		var a activityEntry
		if err := json.Unmarshal(r, &a); err != nil {
			return fmt.Errorf("decoding an entry: %w", err)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortTime(a.CreatedAt), actorName(a.Actor), activityVerb(a), oneLine(activityDetail(a), 100))
	}
	return tw.Flush()
}

func shortTime(iso string) string {
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		return iso
	}
	return t.UTC().Format("2006-01-02 15:04")
}

// actorName mirrors the dashboard: the resolved name (a personal CLI agent is "<owner> (CLI)"), never a raw id when a
// name exists; a vanished user/agent says so.
func actorName(a activityActor) string {
	if a.Name != nil && *a.Name != "" {
		return *a.Name
	}
	switch a.Type {
	case "agent":
		return "(deleted agent)"
	case "user":
		return "(deleted user)"
	}
	return "System"
}

var activityVerbs = map[string]string{
	"created":           "first seen",
	"occurrence_burst":  "new occurrences",
	"status_changed":    "changed status",
	"assigned":          "assigned",
	"unassigned":        "unassigned",
	"regressed":         "regressed",
	"ai_analysis":       "ran AI analysis",
	"linked":            "linked an issue",
	"commented":         "commented",
	"claimed":           "claimed",
	"claim_released":    "released the claim",
	"progress_update":   "posted progress",
	"question_asked":    "asked a question",
	"question_answered": "answered a question",
	"moved":             "moved the issue",
	"attachment_added":  "added an attachment",
	"report_created":    "created the report",
	"report_edited":     "edited the report",
}

func activityVerb(a activityEntry) string {
	if a.EventType == "linked" && str(obj(a.NewValue), "action") == "unlink" {
		return "unlinked an issue"
	}
	if v, ok := activityVerbs[a.EventType]; ok {
		return v
	}
	return strings.ReplaceAll(a.EventType, "_", " ")
}

func obj(raw json.RawMessage) map[string]interface{} {
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func str(m map[string]interface{}, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

func count(f float64) string {
	s := strconv.FormatInt(int64(f), 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func plural(n int64, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return count(float64(n)) + " " + one + "s"
}

// activityDetail is the second half of a line: what changed. Mirrors the dashboard's activityDetail
// (apps/dashboard-web/src/lib/issue-activity.ts) in fewer words.
func activityDetail(a activityEntry) string {
	nv, ov := obj(a.NewValue), obj(a.OldValue)
	switch a.EventType {
	case "status_changed":
		from, to := str(ov, "status"), str(nv, "status")
		out := to
		if from != "" && from != to {
			out = from + " -> " + to
		}
		if v := str(nv, "resolvedInVersion"); to == "resolved" && v != "" {
			out += " in " + v
		}
		if r := str(nv, "reason"); r != "" {
			out += " (" + strings.ReplaceAll(r, "_", " ") + ")"
		}
		return out
	case "regressed":
		if rel, prev := str(nv, "releaseVersion"), str(nv, "previousResolvedVersion"); rel != "" && prev != "" {
			return "seen again in " + rel + " (resolved in " + prev + ")"
		} else if rel != "" {
			return "seen again in " + rel
		}
		return "seen again after it was resolved"
	case "commented", "question_asked":
		if a.Comment == nil {
			return ""
		}
		if a.Comment.Deleted || a.Comment.Excerpt == nil {
			return "(comment deleted)"
		}
		return *a.Comment.Excerpt
	case "progress_update":
		return str(nv, "messageMd")
	case "claim_released":
		if str(nv, "reason") == "stale" {
			return "automatic: the claim went stale"
		}
		if f, _ := nv["force"].(bool); f {
			return "force-released"
		}
	case "assigned":
		if a.Assignee != nil {
			return "to " + actorName(*a.Assignee)
		}
	case "linked":
		t := strings.ReplaceAll(str(nv, "relationType"), "_", " ")
		if redacted, _ := nv["targetRedacted"].(bool); redacted {
			return t + " (an issue this key cannot see)"
		}
		if id := str(nv, "targetIssueId"); id != "" {
			return t + " " + id
		}
		return t
	case "occurrence_burst":
		b := a.Burst
		if b == nil {
			return ""
		}
		out := plural(b.Rows, "burst")
		if b.OccurrencesFrom != nil && b.OccurrencesTo != nil && *b.OccurrencesTo > *b.OccurrencesFrom {
			out += ", " + count(*b.OccurrencesFrom) + " -> " + count(*b.OccurrencesTo) + " occurrences"
		} else if b.OccurrencesTo != nil {
			out += ", " + count(*b.OccurrencesTo) + " occurrences in total"
		}
		if b.Rows > 1 {
			out += ", " + shortTime(b.FirstAt) + " .. " + shortTime(b.LastAt)
		}
		return out
	case "created":
		return str(nv, "errorClass")
	}
	return ""
}

// oneLine flattens a Markdown excerpt for a table cell and caps it.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
