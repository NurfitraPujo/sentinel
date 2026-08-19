// toolchain.go builds the read-only tool set and the fenced system prompt an Advisor (TRIAGE or
// FOLLOW-UP, plan §4.2/§4.3) runs its llm.RunLoop against (plan §4.1's "Read-tools for
// TRIAGE/FOLLOW-UP"). It is the seam that finally wires N8b's llm package and N8c's repoctx tools
// + guard delimiting into a live decision path (plan §9's N8d row) — nothing before this phase
// constructs any of this against a real job.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/guard"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/repoctx"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// Tool names exposed to the Advisor beyond repoctx's read_file/search_code (plan §4.1).
const (
	ToolGetIssue       = "get_issue"
	ToolGetOccurrences = "get_occurrences"
	ToolListSimilar    = "list_similar"
	ToolGetProjects    = "get_projects"
)

// ToolOutputRecorder tracks every tool result's raw output produced while answering one job, so
// guard.Check can measure verbatim overlap across ALL of a job's tool outputs (plan §4.6(c): "coverage
// measured across ALL of toolOutputs combined"). Safe for concurrent use — RunLoop's tool
// execution is sequential today, but nothing about this type assumes that stays true.
type ToolOutputRecorder struct {
	mu      sync.Mutex
	outputs []string
}

// NewToolOutputRecorder returns an empty recorder ready to hand to BuildToolchain.
func NewToolOutputRecorder() *ToolOutputRecorder {
	return &ToolOutputRecorder{}
}

// Record appends one raw tool result to the recorder. Called once per tool invocation, regardless
// of success/error — an error string returned to the model is still text the model saw and could
// try to launder back out, so it counts toward coverage too.
func (r *ToolOutputRecorder) Record(output string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outputs = append(r.outputs, output)
}

// All returns a snapshot of every output recorded so far, in call order. The returned slice is
// owned by the caller — safe to range over without holding the recorder's lock.
func (r *ToolOutputRecorder) All() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.outputs))
	copy(out, r.outputs)
	return out
}

// recording wraps a llm.ToolFunc so its raw string result (success OR error text — runTool in
// llm/toolloop.go feeds err.Error() to the model on failure, so that text is exactly as visible to
// the model as a success result and must be tracked identically) is captured into rec before it is
// returned. This is the ONE place every tool result — repoctx's and the sentinel-backed ones alike
// — funnels through, so BuildToolchain's caller never has to remember to record anywhere else.
func recording(rec *ToolOutputRecorder, fn llm.ToolFunc) llm.ToolFunc {
	return func(ctx context.Context, arguments string) (string, error) {
		out, err := fn(ctx, arguments)
		if err != nil {
			rec.Record(err.Error())
			return out, err
		}
		rec.Record(out)
		return out, nil
	}
}

// Toolchain is the assembled per-job toolset: the llm.ToolDef advertisements to hand the model and
// the llm.ToolFunc map RunLoop executes against. Both are pre-bound to one job's issue/repo
// context.
type Toolchain struct {
	Defs  []llm.ToolDef
	Funcs map[string]llm.ToolFunc
}

// occurrencesArgs is the tool-call argument shape for get_occurrences: a newest-first page,
// optionally paged further back via an RFC3339 "before" cursor (mirrors the real paginated
// endpoint, GET /api/agent/issues/:id/occurrences?limit=&before=).
type occurrencesArgs struct {
	Before string `json:"before"`
	Limit  int    `json:"limit"`
}

// BuildToolchain assembles the Advisor read-tool set for one job (plan §4.1): get_issue,
// get_occurrences(page), list_similar, get_projects always; search_code/read_file additionally
// when repo is non-nil (the project has a repo mapping, plan §4.5). issueID scopes get_issue/
// get_occurrences to this job's issue; list_similar is scoped to projectID (plan §4.1: "issues
// list, same project, sort=lastSeen") so a duplicate_of/caused_by relation can never be formed
// against an issue the model wrongly believes is same-project. Every tool result — success or error — is
// funneled through rec (ToolOutputRecorder) so guard.Check can measure verbatim coverage against
// this job's ENTIRE tool corpus, not just the ones a particular Advisor happens to remember citing.
//
// No mutation tools are ever registered here (plan §4.1: "mutations exist only as the structured
// decision the harness executes") — this is a read-only surface by construction.
//
// projectID must be non-empty: list_similar scopes its ListIssues call by Project, and an empty
// projectID makes sentinel.IssuesListOptions.Project a no-op filter, silently widening list_similar
// to an ORG-WIDE issue listing — exactly the cross-project sight plan §4.1's "same project" scoping
// exists to prevent (a duplicate_of/caused_by relation could then be formed against an issue in a
// different project entirely). Reject rather than silently build a toolchain with that hole.
func BuildToolchain(client *sentinel.Client, repo *repoctx.Repo, issueID string, projectID string, rec *ToolOutputRecorder) (Toolchain, error) {
	if projectID == "" {
		return Toolchain{}, fmt.Errorf("jobs: BuildToolchain: projectID must not be empty (list_similar would scope org-wide)")
	}
	defs := []llm.ToolDef{
		{
			Name:        ToolGetIssue,
			Description: "Get the full detail of the issue this job is about.",
			Params:      llm.Schema{Type: "object", Properties: map[string]llm.Schema{}},
		},
		{
			Name:        ToolGetOccurrences,
			Description: "Get a page of occurrences (individual error events) for this job's issue, newest first, including stacktraces. Up to 50 per page (default 20). To page further back, pass 'before' as the oldest occurrence timestamp seen so far.",
			Params: llm.Schema{
				Type: "object",
				Properties: map[string]llm.Schema{
					"before": {Type: "string", Description: "RFC3339 timestamp cursor: return occurrences strictly older than this. Omit for the newest page.", Nullable: true},
					"limit":  {Type: "number", Description: "Max occurrences to return, 1-50. Omit (or 0) for the default (20).", Nullable: true},
				},
			},
		},
		{
			Name:        ToolListSimilar,
			Description: "List other issues in the same project, most recently seen first — the limited cross-issue sight available (no multi-issue incident reasoning).",
			Params:      llm.Schema{Type: "object", Properties: map[string]llm.Schema{}},
		},
		{
			Name:        ToolGetProjects,
			Description: "List the projects this agent can see, with their settings.",
			Params:      llm.Schema{Type: "object", Properties: map[string]llm.Schema{}},
		},
	}

	funcs := map[string]llm.ToolFunc{
		ToolGetIssue: recording(rec, func(ctx context.Context, _ string) (string, error) {
			res, err := client.GetIssue(ctx, issueID)
			return resultToToolOutput(res, err)
		}),
		ToolGetOccurrences: recording(rec, func(ctx context.Context, arguments string) (string, error) {
			var args occurrencesArgs
			if arguments != "" {
				if err := json.Unmarshal([]byte(arguments), &args); err != nil {
					return "", fmt.Errorf("jobs: invalid %s arguments: %w", ToolGetOccurrences, err)
				}
			}
			res, err := client.GetOccurrences(ctx, issueID, args.Limit, args.Before)
			return resultToToolOutput(res, err)
		}),
		ToolListSimilar: recording(rec, func(ctx context.Context, _ string) (string, error) {
			res, err := client.ListIssues(ctx, sentinel.IssuesListOptions{Sort: "lastSeen", Limit: 20, Project: projectID})
			return resultToToolOutput(res, err)
		}),
		ToolGetProjects: recording(rec, func(ctx context.Context, _ string) (string, error) {
			res, err := client.ListProjects(ctx)
			return resultToToolOutput(res, err)
		}),
	}

	if repo != nil {
		defs = append(defs, repoctx.ToolDefs()...)
		for name, fn := range repoctx.Tools(repo) {
			funcs[name] = recording(rec, fn)
		}
	}

	return Toolchain{Defs: defs, Funcs: funcs}, nil
}

// resultToToolOutput turns a *sentinel.Result/error pair into the plain-string tool result RunLoop
// expects: the response body as text on success, or the error's message. A non-2xx status is
// surfaced as an error string (not a Go error) — the model can be told an issue lookup failed
// without the harness itself treating it as a fatal Advisor failure; the Advisor is free to
// note the failure in its final decision.
func resultToToolOutput(res *sentinel.Result, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if res.Status < 200 || res.Status >= 300 {
		return "", fmt.Errorf("sentinel: request failed: status %d: %s", res.Status, sentinel.ErrorMessage(res.Body))
	}
	return string(res.Body), nil
}

// UntrustedIssueContext is the set of attacker-influenced job fields a TRIAGE/FOLLOW-UP prompt
// builder feeds into guard.ComposeUntrustedSection (plan §4.6: "issue title/message, occurrence
// stacktraces, comment bodies"). Every field here MUST be wrapped — never interpolated bare into a
// prompt — which is exactly what BuildSystemPrompt enforces.
type UntrustedIssueContext struct {
	Title       string
	Message     string
	Stacktraces []string
	Comments    []string
}

// BuildSystemPrompt composes the Advisor's system prompt: basePrompt (the Advisor's own
// instructions — trusted, worker-authored) followed by the guard.ComposeUntrustedSection-fenced
// untrusted issue context (plan §4.6's delimiting control). Every field of ctx passes through
// guard.WrapUntrusted (via ComposeUntrustedSection) — omitting any one of them here is exactly the
// mutation §8 calls out ("unwrap one → a test asserting the fenced markers are present goes red").
func BuildSystemPrompt(basePrompt string, ctx UntrustedIssueContext) string {
	fields := []guard.LabelledContent{
		{Label: "issue_title", Content: ctx.Title},
		{Label: "issue_message", Content: ctx.Message},
	}
	for i, st := range ctx.Stacktraces {
		fields = append(fields, guard.LabelledContent{Label: fmt.Sprintf("stacktrace_%d", i), Content: st})
	}
	for i, c := range ctx.Comments {
		fields = append(fields, guard.LabelledContent{Label: fmt.Sprintf("comment_%d", i), Content: c})
	}
	return basePrompt + "\n\n" + guard.ComposeUntrustedSection(fields...)
}
