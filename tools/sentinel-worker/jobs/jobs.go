// Package jobs holds the job Kind implementations that plug into loop/runner.go: TRIAGE,
// FOLLOW-UP, FIX, and the periodic sweep (plan §1). N8a defines only the seam later phases plug
// into — the Advisor interface and a canned Stub implementation — so the runner has something to
// call in dry-run mode before N8d ships the real TRIAGE/FOLLOW-UP Advisors (per CONTEXT.md's
// "Advisor" term).
package jobs

import (
	"context"
	"encoding/json"
)

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
