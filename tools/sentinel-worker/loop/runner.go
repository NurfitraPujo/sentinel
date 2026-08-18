package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// SkipReason names why a runner declined to act on a job, for journal payloads and /metrics
// (plan §8: "runner preconditions (each `skipped` reason)").
type SkipReason string

const (
	SkipForeignClaim SkipReason = "foreign-claim"
	SkipDeleted      SkipReason = "deleted"
	SkipResolved     SkipReason = "resolved"
	// SkipIgnored is the KindTriage precondition-skip reason for an issue whose status is
	// "ignored" (C7) rather than "resolved" -- previously folded into SkipResolved, coarsening
	// /metrics' skip-reason breakdown (plan §8: "each `skipped` reason" is meant to be enumerable
	// on its own, not merged into an unrelated status).
	SkipIgnored           SkipReason = "ignored"
	SkipNotPreconditioned SkipReason = "precondition-failed"
)

// IssueSnapshot is the runner's one-GET-per-job precondition read (plan §3: "the runner still
// re-checks preconditions with one GET /api/agent/issues/:id at job start").
type IssueSnapshot struct {
	ID           string
	Status       string // unresolved | resolved | ignored (C7)
	AssigneeType string
	AssignedTo   string
}

// IssueReader is the seam the runner uses for its precondition read; kept as an interface so
// tests can fake it without HTTP.
type IssueReader interface {
	GetIssue(ctx context.Context, issueID string) (IssueSnapshot, error)
}

// Claimer is the seam for the plan §2.2/C1 "ensure-claimed" step: claim is idempotent for
// self-reclaim (200 + alreadyClaimed:true), foreign claimant is 409.
type Claimer interface {
	// EnsureClaimed attempts to claim issueID. ok=true means the claim is held by us (whether
	// freshly acquired or already ours); ok=false means a foreign claimant holds it (409, C1), in
	// which case claimedBy carries the foreign claimant's agent id parsed from the 409 body
	// ({claimedBy, claimedAt}) when the implementation can supply one -- empty if not (e.g. a fake
	// in tests, or a body that failed to parse).
	EnsureClaimed(ctx context.Context, issueID string) (ok bool, claimedBy string, err error)
}

// Actor is the seam for §2.3's act(): compiles a journaled Decision into the actual batch/question
// calls. N8a's runner only needs to invoke it; the real compiler ships in N8d (act() per
// disposition) — until then dry-run mode never calls it for real (see Runner.dryRun below).
type Actor interface {
	Act(ctx context.Context, jobID string, d jobs.Decision) error
}

// Runner carries one dispatched job through resolve → preconditions → ensure-claimed → advisor →
// journal → act (plan §0's per-job pipeline). It is deliberately small: durability lives in the
// Journal, not in the Runner's own state.
type Runner struct {
	Journal   *state.Journal
	Issues    IssueReader
	Claims    Claimer
	Advisor   jobs.Advisor
	Act       Actor
	DryRun    bool // WORKER_EXECUTE=false: journal decisions, never call Act for real (plan §5)
	MyAgentID string
	Log       *slog.Logger
	// OnOutcome, when non-nil, is called once per job with its kind and terminal outcome ("done",
	// "failed", or "skipped" -- see SkipReason for the skip sub-reasons folded into the outcome
	// string) immediately after the corresponding journal record is appended. The seam
	// health.JobsTotalMetricName(kind, outcome) plugs into via main.go so plan §7's "jobs by
	// kind×outcome" counter reflects reality. Kept as a plain func hook, not a *health.Status
	// field, so this package stays free of a health import (same reasoning as PollLoop.OnCursorSaved).
	OnOutcome func(kind, outcome string)
}

// Run executes exactly one job for one event, implementing the pipeline. It is idempotent per the
// journal's dedupe rule: a jobId whose latest record is terminal is a no-op here, because the
// caller (the dispatcher's per-issue queue) is expected to have already dropped it — Run still
// double-checks via the journal as defense in depth.
func (r *Runner) Run(ctx context.Context, e Event, kind Kind) error {
	if !kind.IsJob() {
		return fmt.Errorf("loop: Run called with non-job kind %q", kind)
	}
	issueID, err := e.IssueID()
	if err != nil {
		return fmt.Errorf("loop: %w", err)
	}

	jobKind := string(kind)
	jobID := state.JobID(jobKind, issueID, e.Seq)

	latest, err := r.Journal.LatestByJobID()
	if err != nil {
		return fmt.Errorf("reading journal: %w", err)
	}
	if rec, ok := latest[jobID]; ok {
		if rec.State.IsTerminal() {
			// Already terminal (done/failed/skipped/superseded from a prior delivery) — dedupe drop.
			return nil
		}
		if rec.State == state.StateAdvised || rec.State == state.StateActing {
			// A crash/restart between "advised" (or "acting") and the terminal record must NOT
			// re-invoke the Advisor (plan §2.2: "the LLM is NEVER re-invoked for a job that
			// already produced a decision", plan §8's required proof). Replay the journaled
			// decision straight into the act step instead of falling through to resolve/
			// precondition/ensure-claimed/Advisor.Decide below.
			return r.resumeFromAdvised(ctx, jobID, issueID, e, jobKind, rec)
		}
		// Any other non-terminal state (queued/claimed/questioned) has not yet reached the
		// Advisor, so falling through to the normal pipeline is correct: ensure-claimed is
		// idempotent (C1) and ends up ensuring rather than double-claiming.
	}

	if err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateQueued}); err != nil {
		return err
	}

	// resolve issue state (one GET per job, per plan §3)
	snap, err := r.Issues.GetIssue(ctx, issueID)
	if err != nil {
		var statusErr *sentinel.StatusError
		if errors.As(err, &statusErr) && sentinel.ClassifyEnvelope(statusErr.Status, false, false) == sentinel.ClassGone {
			// The issue was deleted between the event landing and this job running (C14) -- a
			// ROUTINE occurrence, not exceptional. This MUST journal a terminal record and return
			// nil rather than propagate the error: with the poll loop's old synchronous Enqueuer, an
			// error return here re-fetched and re-ran the identical poisoned job forever, permanently
			// wedging the entire feed behind one deleted issue. Even now that Dispatcher.Enqueue
			// decouples durable enqueue from job execution (so a stuck job can no longer block other
			// issues), leaving this jobId stuck at "queued" forever would still be wrong -- it must
			// resolve to a real terminal outcome.
			if r.Log != nil {
				r.Log.Info("skipping job: issue no longer resolvable (deleted)", "jobId", jobID, "issueId", issueID, "kind", jobKind, "error", err)
			}
			return r.journalSkip(jobID, issueID, e, jobKind, SkipDeleted)
		}
		// Any other error resolving the issue (5xx, 429, network failure) is transient, not gone
		// -- but leaving the journal record at "queued" (its state as of the Append a few lines
		// above) would strand this job forever: invisible to /metrics (no terminal outcome ever
		// fires) and, worse, invisible to crash recovery, since RecoveryScan's in-flight set is
		// keyed off the SAME non-terminal states this job never advances out of. Full in-lane
		// retry + circuit wiring is N8e (see plan §9's "N8a unwired seams" note); the N8a-minimum
		// bar is to resolve to a real terminal outcome here too, just failed rather than skipped,
		// so nothing is silently stranded or uncounted.
		return r.journalTransientFailure(jobID, issueID, e, jobKind, fmt.Errorf("resolving issue %s: %w", issueID, err))
	}

	// preconditions
	if reason, skip := r.precondition(kind, e.Type, snap); skip {
		return r.journalSkip(jobID, issueID, e, jobKind, reason)
	}

	// ensure-claimed (C1). Dry-run must not perform this mutating call either (plan §5: "every
	// mutating call ... is logged with its exact body and not sent") — a real Claim writes an
	// assignment and an issue_activity row, so gate it behind DryRun the same as Act below, and
	// treat the claim as held for the purpose of exercising the rest of the pipeline.
	var held bool
	var claimedBy string
	if r.DryRun {
		held = true
		if r.Log != nil {
			r.Log.Info("dry-run: skipping claim, not sent", "jobId", jobID, "issueId", issueID, "kind", jobKind)
		}
	} else {
		held, claimedBy, err = r.Claims.EnsureClaimed(ctx, issueID)
		if err != nil {
			return r.journalTransientFailure(jobID, issueID, e, jobKind, fmt.Errorf("claiming issue %s: %w", issueID, err))
		}
	}
	if !held {
		if r.Log != nil {
			r.Log.Info("skipping job: issue claimed by another agent", "jobId", jobID, "issueId", issueID, "kind", jobKind, "claimedBy", claimedBy)
		}
		return r.journalSkipForeignClaim(jobID, issueID, e, jobKind, claimedBy)
	}
	if err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateClaimed}); err != nil {
		return err
	}

	// advisor — never re-invoked once "advised" is journaled; recovery replays that payload
	// instead. Run() is only reached for a fresh job here, so we always decide once.
	decision, err := r.Advisor.Decide(ctx, jobs.Input{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq})
	if err != nil {
		return r.journalTransientFailure(jobID, issueID, e, jobKind, fmt.Errorf("advisor decision for job %s: %w", jobID, err))
	}
	payload, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("marshaling decision: %w", err)
	}
	if err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateAdvised, Payload: payload}); err != nil {
		return err
	}

	if r.DryRun {
		// WORKER_EXECUTE=false: the decision is journaled and nothing mutating is sent (plan §5).
		if r.Log != nil {
			r.Log.Info("dry-run: decision journaled, not acting", "jobId", jobID, "issueId", issueID, "kind", jobKind)
		}
		return r.journalDone(jobID, issueID, e, jobKind)
	}

	if err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateActing}); err != nil {
		return err
	}
	if err := r.Act.Act(ctx, jobID, decision); err != nil {
		_ = r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateFailed})
		if r.OnOutcome != nil {
			r.OnOutcome(jobKind, "failed")
		}
		return fmt.Errorf("acting on job %s: %w", jobID, err)
	}
	if err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateActed}); err != nil {
		return err
	}
	return r.journalDone(jobID, issueID, e, jobKind)
}

// Resume feeds one journal-recovered in-flight job back through the pipeline, implementing the
// production side of CONTEXT.md's Recovery contract ("scan the journal, then replay or resume
// each in-flight job") — without this, RecoveryScan's findings were only logged (main.go's
// runJournalMaintenance) and never actually re-run, silently losing every job that was in flight
// at crash/SIGTERM (validator finding: "resumeFromAdvised... is unreachable from main()").
//
// Resume reconstructs a minimal Event from the journaled record (IssueID + TriggerSeq are all
// Run/resumeFromAdvised need; the original event's Type is not journaled and is not needed except
// for the question_answered precondition arm, so a resumed FOLLOW-UP is conservatively treated as
// NOT a question_answered redelivery -- it still recovers via the "still claimed by me" precondition,
// just without that arm's extra reaped-claim tolerance) and dispatches by state:
//
//   - Advised/Acting: replay the journaled decision straight into Act, verbatim, via
//     resumeFromAdvised -- the Advisor is NEVER re-consulted for these (plan §2.2, §8's required
//     proof).
//   - Queued/Claimed/Questioned: the Advisor was never reached, so re-running the normal pipeline
//     from the top via Run is correct and matches Run's own fallthrough comment for these states;
//     ensure-claimed is idempotent (C1) so this cannot double-claim.
func (r *Runner) Resume(ctx context.Context, job state.InFlightJob) error {
	kind := Kind(job.Kind)
	if !kind.IsJob() {
		return fmt.Errorf("loop: Resume called with non-job kind %q for job %s", job.Kind, job.JobID)
	}
	e := Event{
		Seq:   job.TriggerSeq,
		Issue: &EventIssue{ID: job.IssueID},
	}

	switch job.State {
	case state.StateAdvised, state.StateActing:
		rec := state.Record{
			JobID:      job.JobID,
			IssueID:    job.IssueID,
			Kind:       job.Kind,
			TriggerSeq: job.TriggerSeq,
			State:      job.State,
			Payload:    job.Payload,
		}
		return r.resumeFromAdvised(ctx, job.JobID, job.IssueID, e, job.Kind, rec)
	default:
		// queued / claimed / questioned: Run's own dedupe check will see this job's latest
		// journal record is non-terminal and not advised/acting, and fall through to the normal
		// resolve -> preconditions -> ensure-claimed -> Advisor pipeline, exactly as if this were a
		// fresh delivery.
		return r.Run(ctx, e, kind)
	}
}

// journalTransientFailure implements the N8a-minimum fix for a non-Gone error surfacing anywhere
// in Run's pipeline before a decision has been journaled (resolving the issue, ensure-claimed,
// the Advisor call): it journals a terminal failed(transient: <class>) record, fires OnOutcome
// exactly like every other terminal path, and returns cause so the caller (Dispatch's runOne,
// main's Resume loop) still sees and logs the underlying error. Without this, these three error
// returns left the job's journal record at whatever non-terminal state Run had already appended
// (queued/claimed) -- invisible to /metrics (no OnOutcome ever fires) and, worse, invisible to
// crash recovery too, since RecoveryScan's in-flight set is keyed off the SAME non-terminal
// states. Full in-lane retry + circuit-breaker wiring (re-driving the job through
// sentinel.BackoffForAttempt without leaving the per-issue queue) is N8e -- see plan §9's "N8a
// unwired seams" note; this only guarantees the job resolves to a real, counted terminal outcome.
func (r *Runner) journalTransientFailure(jobID, issueID string, e Event, jobKind string, cause error) error {
	payload, _ := json.Marshal(map[string]string{"reason": "transient", "class": classifyRunnerError(cause)})
	if appendErr := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateFailed, Payload: payload}); appendErr != nil {
		// The journal write itself failed -- nothing further we can do to make this terminal;
		// surface both errors so the operator sees the append failure was not silently swallowed.
		return fmt.Errorf("%w (also failed to journal transient failure: %v)", cause, appendErr)
	}
	if r.OnOutcome != nil {
		r.OnOutcome(jobKind, "failed")
	}
	if r.Log != nil {
		r.Log.Error("job failed with a transient error, journaled terminal failed", "jobId", jobID, "issueId", issueID, "kind", jobKind, "error", cause)
	}
	return cause
}

// classifyRunnerError labels cause for journalTransientFailure's payload, per plan §2.4's failure
// taxonomy when cause carries a *sentinel.StatusError (an HTTP call failed with a known status);
// a bare network error (no StatusError -- e.g. connection refused, DNS failure, timeout) is
// labeled "network".
func classifyRunnerError(cause error) string {
	var statusErr *sentinel.StatusError
	if errors.As(cause, &statusErr) {
		switch sentinel.ClassifyEnvelope(statusErr.Status, false, false) {
		case sentinel.ClassRateLimited:
			return "rate-limited"
		case sentinel.ClassTransient:
			return "server-error"
		case sentinel.ClassAuthFailure:
			return "auth-failure"
		case sentinel.ClassPermanent:
			return "permanent"
		case sentinel.ClassConflictForeign, sentinel.ClassConflictDroppable, sentinel.ClassConflictKeyMismatch:
			return "conflict"
		default:
			return "unknown"
		}
	}
	return "network"
}

func (r *Runner) journalDone(jobID, issueID string, e Event, jobKind string) error {
	err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateDone})
	if err == nil && r.OnOutcome != nil {
		r.OnOutcome(jobKind, "done")
	}
	return err
}

func (r *Runner) journalSkip(jobID, issueID string, e Event, jobKind string, reason SkipReason) error {
	payload, _ := json.Marshal(map[string]string{"reason": string(reason)})
	err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateSkipped, Payload: payload})
	if err == nil && r.OnOutcome != nil {
		r.OnOutcome(jobKind, "skipped_"+string(reason))
	}
	return err
}

// journalSkipForeignClaim is journalSkip's SkipForeignClaim variant, additionally carrying the
// foreign claimant's agent id (parsed from the claim route's 409 {claimedBy, claimedAt} body per
// C1) into the journal payload when the Claimer implementation supplied one -- empty is omitted
// rather than journaled as a misleading empty string.
func (r *Runner) journalSkipForeignClaim(jobID, issueID string, e Event, jobKind, claimedBy string) error {
	fields := map[string]string{"reason": string(SkipForeignClaim)}
	if claimedBy != "" {
		fields["claimedBy"] = claimedBy
	}
	payload, _ := json.Marshal(fields)
	err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateSkipped, Payload: payload})
	if err == nil && r.OnOutcome != nil {
		r.OnOutcome(jobKind, "skipped_"+string(SkipForeignClaim))
	}
	return err
}

// precondition implements the plan §3 "Runner precondition (re-checked)" column for job kinds.
// eventType distinguishes the two events that both dispatch to KindFollowUp (plan §3):
// question_answered's precondition is "re-claim if reaped (200/alreadyClaimed either way)" — i.e.
// no skip here, ensure-claimed below re-acquires idempotently — while commented's precondition is
// "still claimed by me", which DOES skip if the claim moved on.
func (r *Runner) precondition(kind Kind, eventType string, snap IssueSnapshot) (SkipReason, bool) {
	switch kind {
	case KindTriage:
		// Enumerate ignored separately from resolved (finding: an ignored issue journaled
		// skipped(resolved), coarsening the /metrics skip-reason breakdown -- plan §8's "each
		// `skipped` reason" is meant to distinguish them, per C7's three-way issue status).
		if snap.Status == "ignored" {
			return SkipIgnored, true
		}
		if snap.Status != "unresolved" {
			return SkipResolved, true
		}
		if snap.AssigneeType == "agent" && snap.AssignedTo != r.MyAgentID {
			return SkipForeignClaim, true
		}
		return "", false
	case KindFollowUp:
		if eventType == "question_answered" {
			// The claim may have been reaped since we asked (C11) — that is exactly the case
			// this arm exists to recover, so don't skip; ensure-claimed re-acquires it below.
			return "", false
		}
		if snap.AssigneeType != "agent" || snap.AssignedTo != r.MyAgentID {
			return SkipNotPreconditioned, true
		}
		return "", false
	default:
		return "", false
	}
}

// resumeFromAdvised replays a journaled decision straight into the act step, without ever
// re-invoking the Advisor (plan §2.2, §8). rec is the job's latest record, in state Advised or
// Acting:
//   - Advised: rec.Payload IS the decision — journaled right there.
//   - Acting: rec's own payload is a batchBodyHash (§2.2), not the decision, so the decision must
//     be fetched from the job's earlier Advised record via Journal.DecisionForJob.
func (r *Runner) resumeFromAdvised(ctx context.Context, jobID, issueID string, e Event, jobKind string, rec state.Record) error {
	decisionPayload := rec.Payload
	if rec.State == state.StateActing {
		var err error
		decisionPayload, err = r.Journal.DecisionForJob(jobID)
		if err != nil {
			return fmt.Errorf("loading journaled decision for job %s: %w", jobID, err)
		}
	}
	var decision jobs.Decision
	if len(decisionPayload) > 0 {
		if err := json.Unmarshal(decisionPayload, &decision); err != nil {
			return fmt.Errorf("unmarshaling journaled decision for job %s: %w", jobID, err)
		}
	}

	if r.DryRun {
		if r.Log != nil {
			r.Log.Info("dry-run: resuming from journaled decision, not acting", "jobId", jobID, "issueId", issueID, "kind", jobKind, "fromState", rec.State)
		}
		return r.journalDone(jobID, issueID, e, jobKind)
	}

	if rec.State != state.StateActing {
		if err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateActing}); err != nil {
			return err
		}
	}
	if err := r.Act.Act(ctx, jobID, decision); err != nil {
		_ = r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateFailed})
		if r.OnOutcome != nil {
			r.OnOutcome(jobKind, "failed")
		}
		return fmt.Errorf("acting on job %s: %w", jobID, err)
	}
	if err := r.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: jobKind, TriggerSeq: e.Seq, State: state.StateActed}); err != nil {
		return err
	}
	return r.journalDone(jobID, issueID, e, jobKind)
}
