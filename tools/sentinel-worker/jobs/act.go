// act.go is the decision→batch compiler shared by the TRIAGE and FOLLOW-UP Advisors (plan §4.2/
// §4.3, §2.3): it takes a validated Decision plus job context and returns the ordered batch op
// list, gating every published field through guard.Check first (plan §4.6) and enforcing C7/C8 by
// construction. It is the second half of the "advisor-toolchain-act" seam — toolchain.go builds
// what an Advisor reads, this file compiles what it decided into what the harness sends.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/guard"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// --- TRIAGE decision schema (plan §4.2) -------------------------------------------------------

// TriageDisposition is the plan §4.2 disposition enum — an assessment, never an action verb (root
// CONTEXT.md), and deliberately NOT the old "escalate" name.
type TriageDisposition string

const (
	DispositionCommentOnly TriageDisposition = "comment_only"
	DispositionNeedsInfo   TriageDisposition = "needs_info"
	DispositionDuplicate   TriageDisposition = "duplicate"
	DispositionLinkedCause TriageDisposition = "linked_cause"
	DispositionFixable     TriageDisposition = "fixable"
	DispositionNeedsHuman  TriageDisposition = "needs_human"
)

// TriageDecision is the plan §4.2 JSON schema, decoded from a Decision{Kind:"triage"}.Raw.
type TriageDecision struct {
	Severity    *string `json:"severity"`
	Disposition string  `json:"disposition"`
	DuplicateOf *string `json:"duplicateOf"`
	CausedBy    *string `json:"causedBy"`
	Summary     string  `json:"summary"`
	Question    *string `json:"question"`
	FixBrief    *string `json:"fixBrief"`
	Confidence  float64 `json:"confidence"`
}

// --- FOLLOW-UP decision schema (plan §4.3) ----------------------------------------------------

// FollowupAction is the plan §4.3 action enum — NOT the old "escalate_to_fix" name.
type FollowupAction string

const (
	ActionReply      FollowupAction = "reply"
	ActionResolve    FollowupAction = "resolve"
	ActionAttemptFix FollowupAction = "attempt_fix"
	ActionRelease    FollowupAction = "release"
)

// FollowupDecision is the plan §4.3 JSON schema, decoded from a Decision{Kind:"followup"}.Raw.
type FollowupDecision struct {
	Action            string   `json:"action"`
	Body              string   `json:"body"`
	ResolvedInVersion *string  `json:"resolvedInVersion"`
	FixBrief          *string  `json:"fixBrief"`
	Confidence        *float64 `json:"confidence"`
}

// --- shared plumbing ----------------------------------------------------------------------------

// Valid issue statuses (C7: "no in_progress status — the valid set is unresolved|resolved|ignored").
const (
	StatusUnresolved = "unresolved"
	StatusResolved   = "resolved"
	StatusIgnored    = "ignored"
)

// issueTypeUserReport is the only issueType severity ops are valid against (C8).
const issueTypeUserReport = "user_report"

// DefaultFixConfidence is the plan §4.2/§5 default for WORKER_FIX_CONFIDENCE.
const DefaultFixConfidence = 0.7

// ActContext is everything CompileTriage/CompileFollowup need beyond the Decision itself: job
// identity for idempotency-key derivation (C4), the issue's type for the C8 severity gate, this
// job's full tool-output corpus for the guard.Check verbatim gate (§4.6(c) — ALL of a job's tool
// outputs, gathered via toolchain.go's ToolOutputRecorder), configured secret values, and the FIX
// gate inputs (plan §4.2: "fixable + confidence >= WORKER_FIX_CONFIDENCE + FIX enabled").
type ActContext struct {
	JobID       string
	IssueID     string
	IssueType   string // "user_report" | "system_error" | ...
	ToolOutputs []string
	Secrets     []string
	FixEnabled  bool
	// FixConfidence is the WORKER_FIX_CONFIDENCE threshold; <=0 uses DefaultFixConfidence.
	FixConfidence float64
	// MaxVerbatim is WORKER_GATE_MAX_VERBATIM, threaded through to guard.CheckWithConfig (plan
	// §4.6/§5 finding 3); <=0 uses guard.DefaultMaxVerbatim. main.go's cfg.WorkerGateMaxVerbatim
	// already defaults to 0.25 and is validated to [0,1], so in production this is always a real
	// configured value — the <=0 fallback only matters for callers (tests, or a zero-valued
	// ActContext) that never set it, and preserves this gate's pre-existing default behavior for
	// them exactly as DefaultFixConfidence does for FixConfidence above.
	MaxVerbatim float64
}

func (a ActContext) fixConfidence() float64 {
	if a.FixConfidence > 0 {
		return a.FixConfidence
	}
	return DefaultFixConfidence
}

func (a ActContext) maxVerbatim() float64 {
	if a.MaxVerbatim > 0 {
		return a.MaxVerbatim
	}
	return guard.DefaultMaxVerbatim
}

func (a ActContext) isUserReport() bool { return a.IssueType == issueTypeUserReport }

// GateRejectedError reports that a published field failed guard.Check (plan §4.6: "Gate rejection
// ⇒ one structured re-ask citing the violation, then Permanent"). Callers (the Advisor loop) use
// this to build exactly one re-ask prompt naming Field/Reason, then treat a second rejection as
// Permanent (plan §2.4).
type GateRejectedError struct {
	Field guard.PublishedField
	Err   error // always a *guard.Violation
}

func (e *GateRejectedError) Error() string {
	return fmt.Sprintf("jobs: act: published field rejected: %v", e.Err)
}
func (e *GateRejectedError) Unwrap() error { return e.Err }

// UnknownDecisionValueError reports that a Decision's disposition/action field carried a value
// outside its schema's enum — an Advisor bug (a schema-invalid model output is normally caught by
// llm/toolloop.go's finalizeDecision before it ever reaches Act, but the compiler boundary must not
// silently trust that upstream gate). Before this type existed, an unrecognized value fell through
// every disposition/action check unmatched and was compiled as if it meant "comment_only"/"release"
// — a silent default the caller never chose, on the security-sensitive execute path.
type UnknownDecisionValueError struct {
	Kind  string // "disposition" | "action"
	Value string
}

func (e *UnknownDecisionValueError) Error() string {
	return fmt.Sprintf("jobs: act: unknown %s %q", e.Kind, e.Value)
}

// gate runs guard.Check for one published field and wraps a rejection as *GateRejectedError.
func gate(actx ActContext, field guard.PublishedField, text string) error {
	cfg := guard.Config{SecretValues: actx.Secrets, MaxVerbatim: actx.maxVerbatim()}
	if err := guard.CheckWithConfig(field, text, actx.ToolOutputs, cfg); err != nil {
		return &GateRejectedError{Field: field, Err: err}
	}
	return nil
}

// opBuilder accumulates BatchOperations for one job's compiled decision, assigning each keyable
// op the next `<jobId>:<opIndex>` key (C4) in a single, shared counter — the question call (when
// present) consumes index 0 before any batch op is built, so the SAME counter that numbers batch
// ops also numbers the out-of-band question, keeping the derivation uniform across everything one
// job's decision publishes.
type opBuilder struct {
	jobID   string
	next    int
	ops     []sentinel.BatchOperation
	release bool // whether a claim.release op has been appended (must stay last)
}

func newOpBuilder(jobID string) *opBuilder { return &opBuilder{jobID: jobID} }

// nextKey reserves and returns the next idempotency key in this job's sequence.
func (b *opBuilder) nextKey() string {
	key := sentinel.IdempotencyKey(b.jobID, b.next)
	b.next++
	return key
}

// add appends a batch op with a freshly reserved idempotency key. Must not be called after
// addRelease.
func (b *opBuilder) add(op, issueID string, params map[string]interface{}) {
	b.ops = append(b.ops, sentinel.NewBatchOperation(op, issueID, params, b.nextKey()))
}

// addRelease appends `issues.claim.release` — the plan §2.3 fixed op order's terminal op. Calling
// it more than once, or calling add() afterward, is a compiler bug (callers here only ever call it
// once, at the very end of a compile function) — guarded defensively rather than silently
// reordering.
func (b *opBuilder) addRelease(issueID string) error {
	if b.release {
		return fmt.Errorf("jobs: act: internal error: claim.release compiled twice for job %s", b.jobID)
	}
	// issues.claim.release takes no body per agent-ops.ts's issuesClaimRelease handler.
	b.ops = append(b.ops, sentinel.NewBatchOperation("issues.claim.release", issueID, nil, b.nextKey()))
	b.release = true
	return nil
}

// Compiled is what CompileTriage/CompileFollowup return: the out-of-band question (if any, sent
// BEFORE the batch per plan §2.3: "the question call ... is ordered before the batch so the
// batch's release/severity ops can't strand a question-less waiting_on state"), the ordered batch
// op list, and whether the caller should enqueue a FIX job.
type Compiled struct {
	// Question, when non-nil, must be sent via sentinel.Client.PostQuestion (with its own
	// idempotency key) BEFORE Ops is sent as a batch.
	Question *QuestionOp
	// Ops is the plan §2.3 fixed-order batch op list: load-bearing ops first (issues.comment,
	// issues.progress, issues.status), then droppable ops (issues.report.severity,
	// issues.relations.add), then issues.claim.release last.
	Ops []sentinel.BatchOperation
	// EnqueueFix reports whether the caller should enqueue a FIX job for this issue (fixable/
	// attempt_fix above the confidence threshold, with FIX enabled) — FIX itself is N8f's engine;
	// this seam only surfaces the decision.
	EnqueueFix bool
}

// QuestionOp is the plan §2.2/§2.3 out-of-band blocking question: sent via PostQuestion with its
// own idempotency key, never folded into the batch. Audience defaults to "reporter" (the
// overwhelmingly common case — needs_info almost always asks the person who filed the report);
// nothing in plan §4.2's TRIAGE schema surfaces a per-decision audience choice.
type QuestionOp struct {
	IssueID        string
	Body           string
	Audience       string
	IdempotencyKey string
}

// defaultQuestionAudience is QuestionOp.Audience's fallback when unset.
const defaultQuestionAudience = "reporter"

// --- TRIAGE compile (plan §4.2) -----------------------------------------------------------------

// CompileTriage gates every published field of d through guard.Check, then compiles it into the
// plan §4.2 batch per disposition. A gate rejection returns *GateRejectedError before any op is
// built (the caller re-asks once, then treats the job as Permanent — plan §4.6/§2.4); nothing
// partially compiles.
func CompileTriage(actx ActContext, d TriageDecision) (Compiled, error) {
	disposition := TriageDisposition(d.Disposition)
	switch disposition {
	case DispositionCommentOnly, DispositionNeedsInfo, DispositionDuplicate, DispositionLinkedCause, DispositionFixable, DispositionNeedsHuman:
	default:
		return Compiled{}, &UnknownDecisionValueError{Kind: "disposition", Value: d.Disposition}
	}

	if err := gate(actx, guard.FieldSummary, d.Summary); err != nil {
		return Compiled{}, err
	}
	if disposition == DispositionNeedsInfo {
		if d.Question == nil || *d.Question == "" {
			return Compiled{}, fmt.Errorf("jobs: act: needs_info disposition requires a non-empty question")
		}
		if err := gate(actx, guard.FieldQuestion, *d.Question); err != nil {
			return Compiled{}, err
		}
	}
	fixable := disposition == DispositionFixable && d.Confidence >= actx.fixConfidence() && actx.FixEnabled
	if fixable {
		if d.FixBrief == nil || *d.FixBrief == "" {
			return Compiled{}, fmt.Errorf("jobs: act: fixable disposition at/above confidence threshold requires a non-empty fixBrief")
		}
		if err := gate(actx, guard.FieldFixBrief, *d.FixBrief); err != nil {
			return Compiled{}, err
		}
	}

	b := newOpBuilder(actx.JobID)
	var compiled Compiled

	// Reserve the question's key first (opIndex 0) so it precedes every batch op's key, matching
	// "the question call is ordered before the batch" (plan §2.3) at the key-derivation level too.
	if disposition == DispositionNeedsInfo {
		compiled.Question = &QuestionOp{
			IssueID:        actx.IssueID,
			Body:           *d.Question,
			Audience:       defaultQuestionAudience,
			IdempotencyKey: b.nextKey(),
		}
	}

	// Load-bearing ops first: the triage-summary comment, always (plan §4.2: "always: triage-
	// summary comment").
	prefix := "🤖 Triage: "
	if disposition == DispositionNeedsHuman {
		prefix = "🤖 Escalation: "
	}
	b.add("issues.comment", actx.IssueID, map[string]interface{}{"body_md": prefix + d.Summary})

	// Droppable ops next: severity (C8: user_report only), then relations.
	if disposition == DispositionNeedsHuman {
		if actx.isUserReport() {
			b.add("issues.report.severity", actx.IssueID, map[string]interface{}{"severity": "critical"})
		}
	} else if d.Severity != nil && *d.Severity != "" && actx.isUserReport() {
		b.add("issues.report.severity", actx.IssueID, map[string]interface{}{"severity": *d.Severity})
	}

	switch disposition {
	case DispositionDuplicate:
		if d.DuplicateOf == nil || *d.DuplicateOf == "" {
			return Compiled{}, fmt.Errorf("jobs: act: duplicate disposition requires duplicateOf")
		}
		b.add("issues.relations.add", actx.IssueID, map[string]interface{}{
			"target_issue_id": *d.DuplicateOf,
			"relation_type":   "duplicate_of",
		})
	case DispositionLinkedCause:
		if d.CausedBy == nil || *d.CausedBy == "" {
			return Compiled{}, fmt.Errorf("jobs: act: linked_cause disposition requires causedBy")
		}
		b.add("issues.relations.add", actx.IssueID, map[string]interface{}{
			"target_issue_id": *d.CausedBy,
			"relation_type":   "caused_by",
		})
	}

	// Claim disposition: needs_info and fixable(-enqueued) KEEP the claim (plan §4.2: "claim KEPT
	// ... releasing here is what killed the question loop in rev 1"; "keep claim, enqueue FIX").
	// needs_human and everything else release.
	keepClaim := disposition == DispositionNeedsInfo || fixable
	if !keepClaim {
		if err := b.addRelease(actx.IssueID); err != nil {
			return Compiled{}, err
		}
	}

	compiled.Ops = b.ops
	compiled.EnqueueFix = fixable
	return compiled, nil
}

// --- FOLLOW-UP compile (plan §4.3) --------------------------------------------------------------

// resolvedInVersionPattern is the conservative allowlist resolvedInVersionForPublish restricts
// resolved_in_version to (finding 6, core-robustness round 3): letters, digits, and the
// punctuation real version strings use (dots/dashes/underscores/plus/tilde -- semver's own
// pre-release/build-metadata charset, plus common "v1.2.3", "2026.08.20-rc1" style variants).
// Everything else is dropped, same posture as fix_pr.go's titleCharsetPattern for errorClass.
var resolvedInVersionPattern = regexp.MustCompile(`[^A-Za-z0-9.+_~-]`)

// resolvedInVersionForPublish sanitizes a model-authored resolved_in_version value before it is
// ever published (finding 6, core-robustness round 3): unlike every other model-authored field
// CompileFollowup/CompileTriage publish, resolved_in_version was passed through to the
// issues.status batch op completely unconstrained -- no guard.Check, no charset/length limit --
// despite being just as attacker-influenced (it flows from the Advisor's JSON decision, which in
// turn is shaped by event/tool-output data an external error report can influence). This collapses
// whitespace, restricts the result to resolvedInVersionPattern's charset, and caps length so a
// pathological or injection-y value (script tags, markdown, control characters, a multi-KB blob)
// cannot ride along in a field that is not sized or gated like a prose field. A result that
// contains a configured secret value verbatim (guard.Check's SecretValues gate, same protection
// every other published field gets) is rejected outright via *GateRejectedError rather than
// silently stripped, matching gate's own all-or-nothing compile contract.
func resolvedInVersionForPublish(actx ActContext, raw string) (string, error) {
	s := strings.Join(strings.Fields(raw), " ")
	s = resolvedInVersionPattern.ReplaceAllString(s, "")
	const maxLen = 64
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	if s == "" {
		return "", nil
	}
	if err := gate(actx, guard.FieldReplyBody, s); err != nil {
		return "", err
	}
	return s, nil
}

// CompileFollowup gates every published field of d through guard.Check, then compiles it into the
// plan §4.3 batch. Same compile pattern as CompileTriage: gate first, build ops after, nothing
// partially compiles on rejection.
func CompileFollowup(actx ActContext, d FollowupDecision) (Compiled, error) {
	action := FollowupAction(d.Action)
	switch action {
	case ActionReply, ActionResolve, ActionAttemptFix, ActionRelease:
	default:
		return Compiled{}, &UnknownDecisionValueError{Kind: "action", Value: d.Action}
	}

	if err := gate(actx, guard.FieldReplyBody, d.Body); err != nil {
		return Compiled{}, err
	}
	attemptFix := action == ActionAttemptFix
	if attemptFix {
		if d.FixBrief == nil || *d.FixBrief == "" || d.Confidence == nil {
			return Compiled{}, fmt.Errorf("jobs: act: attempt_fix action requires fixBrief and confidence")
		}
		if err := gate(actx, guard.FieldFixBrief, *d.FixBrief); err != nil {
			return Compiled{}, err
		}
	}
	attemptFix = attemptFix && d.Confidence != nil && *d.Confidence >= actx.fixConfidence() && actx.FixEnabled

	b := newOpBuilder(actx.JobID)
	var compiled Compiled

	if d.Body != "" {
		b.add("issues.comment", actx.IssueID, map[string]interface{}{"body_md": d.Body})
	}

	if action == ActionResolve {
		params := map[string]interface{}{"status": StatusResolved}
		if d.ResolvedInVersion != nil && *d.ResolvedInVersion != "" {
			// Finding 6 (core-robustness round 3): resolved_in_version used to be published
			// verbatim, the one model-authored field in this file that never went through any
			// guard -- constrain it to a version charset + length cap and run it through
			// guard.Check's secret-leak gate, like every other published field.
			sanitized, err := resolvedInVersionForPublish(actx, *d.ResolvedInVersion)
			if err != nil {
				return Compiled{}, err
			}
			if sanitized != "" {
				params["resolved_in_version"] = sanitized
			}
		}
		b.add("issues.status", actx.IssueID, params)
	}

	// Claim disposition (plan §4.3: "attempt_fix uses the same FIX gate as §4.2"): attempt_fix
	// AT/above the confidence threshold with FIX enabled keeps the claim (FIX job takes over);
	// resolve/release always release. A below-threshold or FIX-disabled attempt_fix must NOT
	// silently keep the claim — that strands it forever, since it has no question (waiting_on
	// unset) so the §4.3 nag sweep never picks it up and only the §2.7 heartbeat keeps it alive
	// indefinitely. Match CompileTriage's below-threshold `fixable` branch: comment only + release.
	keepClaim := attemptFix
	if action == ActionReply {
		// reply: keep the claim, the thread continues.
		keepClaim = true
	}
	if !keepClaim {
		if err := b.addRelease(actx.IssueID); err != nil {
			return Compiled{}, err
		}
	}

	compiled.Ops = b.ops
	compiled.EnqueueFix = attemptFix
	return compiled, nil
}

// --- decoding + top-level Act --------------------------------------------------------------------

// DecodeTriage decodes Decision.Raw as a TriageDecision. The caller (the TRIAGE Advisor) is
// responsible for having already validated Raw against the plan §4.2 llm.Schema via
// llm.RunLoop/finalizeDecision before this is ever called — this is a plain json.Unmarshal, not a
// second validation pass.
func DecodeTriage(dec Decision) (TriageDecision, error) {
	var t TriageDecision
	if err := json.Unmarshal(dec.Raw, &t); err != nil {
		return TriageDecision{}, fmt.Errorf("jobs: act: decoding triage decision: %w", err)
	}
	return t, nil
}

// DecodeFollowup decodes Decision.Raw as a FollowupDecision. Same caveat as DecodeTriage.
func DecodeFollowup(dec Decision) (FollowupDecision, error) {
	var f FollowupDecision
	if err := json.Unmarshal(dec.Raw, &f); err != nil {
		return FollowupDecision{}, fmt.Errorf("jobs: act: decoding followup decision: %w", err)
	}
	return f, nil
}

// Sender is the subset of *sentinel.Client Act needs to execute a compiled decision — narrowed to
// an interface so tests can substitute a recording fake without a real HTTP server.
type Sender interface {
	PostQuestion(ctx context.Context, issueID string, params map[string]interface{}, idempotencyKey string) (*sentinel.Result, error)
	PostBatch(ctx context.Context, req sentinel.BatchRequest) (*sentinel.Result, error)
}

// ActOutcome is what Act returns: whether anything was actually sent (false in dry-run — plan §5:
// "dry-run (WORKER_EXECUTE=false) must still journal decisions and send NOTHING"), and the
// compiled payload that was (or, in dry-run, would have been) sent, for journaling verbatim.
type ActOutcome struct {
	Sent     bool
	Compiled Compiled
}

// Act executes a Compiled decision against client: PostQuestion first (when Compiled.Question is
// set), then PostBatch with stopOnError:false (C3) for Compiled.Ops. When execute is false
// (WORKER_EXECUTE=false, dry-run), Act sends NOTHING — it only returns the compiled payload for
// the caller to journal, matching plan §5's dry-run contract exactly. The per-op result walk
// (dedup=success, relation-409 drop, claim-family per C1, op-mismatch-409 permanent — plan §2.3)
// is the caller's responsibility (loop/runner.go, a different phase's wiring); Act's job stops at
// "did the wire call happen and what did it return", which the caller classifies via
// sentinel.ClassifyBatch/ClassifyOp.
func Act(ctx context.Context, client Sender, compiled Compiled, execute bool) (ActOutcome, *sentinel.Result, *sentinel.Result, error) {
	if !execute {
		return ActOutcome{Sent: false, Compiled: compiled}, nil, nil, nil
	}
	var qRes *sentinel.Result
	if compiled.Question != nil {
		params := map[string]interface{}{
			"body_md":  compiled.Question.Body,
			"audience": compiled.Question.Audience,
		}
		res, err := client.PostQuestion(ctx, compiled.Question.IssueID, params, compiled.Question.IdempotencyKey)
		if err != nil {
			return ActOutcome{Sent: false, Compiled: compiled}, nil, nil, err
		}
		if res.Status < 200 || res.Status >= 300 {
			// MAJOR (finding 3): PostQuestion returns a non-2xx status in Result.Status with
			// err == nil (a transport-layer success carrying an application-level failure) -- a
			// 400/403/409/5xx question response must not be treated as "asked". Before this check,
			// the caller (RealActor.Act) journaled StateQuestioned with an empty commentId
			// regardless, stranding the claim (never actually asking the reporter) with no way to
			// ever close the question. Wrapped as *sentinel.StatusError so the runner classifies
			// 429/5xx as transient (retry) and 401/403 as auth, matching checkBatchResults' own
			// envelope-failure wrapping above.
			return ActOutcome{Sent: false, Compiled: compiled}, res, nil, fmt.Errorf("posting question for issue %s: status=%d: %w", compiled.Question.IssueID, res.Status, &sentinel.StatusError{Status: res.Status, Header: res.Header, Body: res.Body})
		}
		qRes = res
	}
	var bRes *sentinel.Result
	if len(compiled.Ops) > 0 {
		res, err := client.PostBatch(ctx, sentinel.BatchRequest{Operations: compiled.Ops, StopOnError: false})
		if err != nil {
			return ActOutcome{Sent: false, Compiled: compiled}, qRes, nil, err
		}
		bRes = res
	}
	return ActOutcome{Sent: true, Compiled: compiled}, qRes, bRes, nil
}
