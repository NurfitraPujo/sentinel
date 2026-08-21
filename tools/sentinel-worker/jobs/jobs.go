// Package jobs holds the job Kind implementations that plug into loop/runner.go: TRIAGE,
// FOLLOW-UP, FIX, and the periodic sweep (plan §1). N8a defines only the seam later phases plug
// into — the Advisor interface and a canned Stub implementation — so the runner has something to
// call in dry-run mode before N8d ships the real TRIAGE/FOLLOW-UP Advisors (per CONTEXT.md's
// "Advisor" term).
package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
)

// PromptHash computes a hex sha256 over the fully rendered system prompt plus every user message
// (in order), for Decision.PromptSHA256 (plan §7 finding 4). A NUL byte separates system from each
// user message so no crafted content can make two different (system, messages) pairs hash
// identically by shifting a boundary.
func PromptHash(system string, userMessages ...string) string {
	h := sha256.New()
	h.Write([]byte(system))
	for _, m := range userMessages {
		h.Write([]byte{0})
		h.Write([]byte(m))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Decision is the structured output an Advisor produces for one job — the harness executes it,
// the Advisor never mutates anything itself (per CONTEXT.md's Advisor definition). N8a keeps this
// as an opaque envelope; jobs/triage.go and jobs/followup.go (N8d) will define the real
// disposition schemas from plan §4.2/§4.3. Kept here so loop/runner.go has a concrete type to
// journal as the "advised" payload (plan §2.2).
type Decision struct {
	// Kind identifies which schema Raw should be interpreted as ("triage" | "followup"), matching
	// the job kind that produced it.
	Kind string          `json:"kind"`
	Raw  json.RawMessage `json:"raw"`
	// ToolOutputs is the FULL corpus of raw read-tool results (read_file/search_code/get_issue/
	// etc.) gathered while producing this Decision, via toolchain.go's ToolOutputRecorder (plan
	// §4.6(c)). It is journaled alongside the decision (part of the Advised record's Payload) so
	// that Act — whether invoked immediately or replayed later from the journal without ever
	// re-consulting the Advisor (CONTEXT.md's Replay contract) — can hand the SAME corpus to
	// ActContext.ToolOutputs for guard.Check's verbatim-exfiltration gate. Without this field the
	// corpus is ephemeral to Decide and the gate downstream runs against an empty slice, making it
	// a no-op.
	ToolOutputs []string `json:"toolOutputs,omitempty"`

	// Usage is the adapter-reported token accounting for the RunLoop that produced this Decision
	// (plan §2.6 finding 1). Journaled as part of the "advised" record so main.go's boot-time
	// llm.DailyBudget.SeedSpent reconstruction (SumAdvisedTokenUsage) can recover today's spend
	// from the journal alone, exactly like FixCaps.SeedToday reconstructs its own counters —
	// without this field the running total is invisible to a restart and WORKER_DAILY_TOKEN_BUDGET
	// silently resets on every crash/redeploy.
	Usage llm.Usage `json:"usage,omitempty"`

	// Provider is the llm.Result.Provider that actually produced this Decision ("primary" or
	// "fallback" per llm.RunLoop's own labeling) — plan §7's "llm_tokens by provider" needs to
	// know which provider's spend to attribute Usage to. Not journaled as part of any
	// budget-affecting decision (Usage already carries the number that matters for the budget
	// itself); this is purely an observability label.
	Provider string `json:"provider,omitempty"`

	// PromptSHA256/PromptVersion identify exactly which rendered prompt produced this Decision
	// (plan §7 finding 4): a hex sha256 of the fully rendered system+user prompt, and the base
	// prompt's version tag (triagePromptVersion/followupPromptVersion). Journaled alongside the
	// decision so an operator (or an incident review) can tell, after the fact, precisely what
	// text the model saw for this job without re-deriving it from live code.
	PromptSHA256  string `json:"promptSha256,omitempty"`
	PromptVersion string `json:"promptVersion,omitempty"`
}

// Input is what an Advisor needs to produce a Decision: enough job identity for prompt construction
// and journaling. Later phases (N8b llm package) extend the actual prompt context; N8a's runner
// only needs to pass identity through to the stub.
type Input struct {
	JobID      string
	IssueID    string
	Kind       string // "triage" | "followup"
	TriggerSeq int64
}

// Advisor is the seam llm/toolloop.go (N8b) will implement for real: given a job Input, produce a
// structured Decision. The runner calls this exactly once per job — a job already past "advised"
// in the journal is replayed from its stored payload and never re-invokes Advisor (plan §2.2).
type Advisor interface {
	Decide(ctx context.Context, in Input) (Decision, error)
}

// StubAdvisor is a canned Advisor used by N8a's dry-run mode (WORKER_EXECUTE=false, plan §5) and by
// unit tests, before N8d ships real TRIAGE/FOLLOW-UP Advisors. It always returns a fixed,
// harmless "comment_only" style decision so the runner's journal→act plumbing is exercisable
// end-to-end without an LLM call.
type StubAdvisor struct{}

// Decide implements Advisor by returning a fixed decision naming the job it was asked about, never
// calling out to any network or LLM.
func (StubAdvisor) Decide(_ context.Context, in Input) (Decision, error) {
	raw, _ := json.Marshal(map[string]any{
		"disposition": "comment_only",
		"summary":     "stub decision (N8a dry-run placeholder) for job " + in.JobID,
		"stub":        true,
	})
	return Decision{Kind: in.Kind, Raw: raw}, nil
}

// NotImplementedActor is the placeholder loop.Runner.Act seam for N8a: act()'s real batch
// compiler (plan §2.3) ships in N8d. It errors loudly rather than silently no-oping so a
// misconfigured WORKER_EXECUTE=true deployment fails fast instead of appearing to work while
// dropping every decision on the floor. WORKER_EXECUTE=false (dry-run, N8a's supported mode)
// never reaches this — loop.Runner short-circuits before calling Act.
type NotImplementedActor struct{}

// Act implements Actor.
func (NotImplementedActor) Act(_ context.Context, jobID string, _ Decision) error {
	return errNotImplemented(jobID)
}

func errNotImplemented(jobID string) error {
	return &notImplementedError{jobID: jobID}
}

type notImplementedError struct{ jobID string }

func (e *notImplementedError) Error() string {
	return "jobs: Act is not implemented until N8d (act() batch compiler, plan §2.3); WORKER_EXECUTE=true is not yet supported for job " + e.jobID
}
