// actor.go is the real loop.Actor implementation (plan §2.3/§9's N8d row), replacing
// NotImplementedActor once wired into main.go. It bridges loop.Runner's narrow
// Act(ctx, jobID, Decision) seam -- which carries no issueId/issueType/project context of its own
// -- back to what CompileTriage/CompileFollowup need: it resolves jobID -> issueId from the
// journal (the same record Runner itself just appended for this job), fetches the issue once to
// learn issueType/projectId, consults the settings snapshot for that project's FIX-enablement, and
// hands the result to the existing CompileTriage/CompileFollowup + Act (act.go, already shipped).
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// ProjectFixSettings is the narrow view of settings.Store.Project the Actor needs -- kept as an
// interface (rather than importing the settings package directly) so jobs stays free of a
// dependency on the settings package's wire-decoding concerns; main.go's wiring supplies a small
// adapter over *settings.Store.
type ProjectFixSettings interface {
	// FixEnabled reports whether projectID has FIX enabled, per the current settings snapshot.
	// The second return is false when the project is unknown to the snapshot (treated as
	// fix-disabled -- never fail a job open on a settings-freshness gap).
	FixEnabled(projectID string) (enabled bool, known bool)
}

// RealActor implements loop.Actor for real (plan §2.3): decode the journaled Decision by kind,
// compile it via CompileTriage/CompileFollowup (guard.Check runs inside those, plan §4.6), and
// send it via Act. loop.Runner has already gated DryRun before ever calling Act, so Act is always
// invoked with execute=true here.
type RealActor struct {
	Client        *sentinel.Client
	Journal       *state.Journal
	Fix           ProjectFixSettings // nil is treated as "no project ever has FIX enabled"
	Fixer         Fixer              // nil is treated as "FIX engine not wired -- EnqueueFix is a no-op"
	Secrets       []string           // configured secret values to redact (guard.Check's verbatim gate)
	FixConfidence float64            // WORKER_FIX_CONFIDENCE; <=0 uses DefaultFixConfidence
	MaxVerbatim   float64            // WORKER_GATE_MAX_VERBATIM; <=0 uses guard.DefaultMaxVerbatim

	// SentinelURL is $SENTINEL_URL (the dashboard's own base URL, the same value the worker's own
	// API client is built from) -- the only thing Act needs to turn an issueId into the human-facing
	// Sentinel issue URL a FIX PR body/TASK.md links back to (plan §4.4 step 5: the PR body must
	// carry the actual issue link, not just the raw id). Empty is a valid, if degraded, config
	// (FixJobInput.IssueURL is then left empty, exactly as it was before this field existed) --
	// never fail a FIX dispatch over a missing display URL.
	SentinelURL string
}

// actIssueEnvelope decodes the fields Act needs from GET /api/agent/issues/:id -- issueType (C8's
// severity gate), projectId (the FIX-enablement lookup), and errorClass/message/occurrence
// (fix.go's TaskBrief/PR-title seeding once compiled.EnqueueFix fires). Deliberately narrower than
// triageIssueEnvelope only in that it doesn't need the report body -- FIX's TASK.md uses
// errorClass, not the raw report.
type actIssueEnvelope struct {
	Issue struct {
		ProjectID  string `json:"projectId"`
		IssueType  string `json:"issueType"`
		ErrorClass string `json:"errorClass"`
		Message    string `json:"message"`
	} `json:"issue"`
	LatestOccurrence *struct {
		Stacktrace json.RawMessage `json:"stacktrace"`
	} `json:"latestOccurrence"`
}

// actingPayload is what RealActor.Act journals into the StateActing record the FIRST time it
// compiles a job's decision (finding 4, plan §2.2): the compiled batch body verbatim (Compiled),
// plus the handful of GET-issue-derived fields (ProjectID/ErrorClass/Occurrences) a FIX dispatch
// needs, so a REPLAY (resumeFromAdvised driving the SAME jobID back through Act -- whether via an
// in-lane retry after a transient batch/question failure, or a crash-restart Resume) can re-send
// the ORIGINAL compiled ops/question byte-for-byte instead of recompiling from a fresh issue fetch.
// Recompiling is risky because a compile can be issue-state-dependent (e.g. the severity op is
// only built when actx.isUserReport()) -- if the issue's state visibly changed between the first
// attempt and the replay, a fresh compile could silently diverge from what was actually already
// (partially) sent, and the idempotency-key dedupe on the server only fires when the replayed body
// matches the original.
type actingPayload struct {
	Compiled
	ProjectID   string `json:"projectId,omitempty"`
	ErrorClass  string `json:"errorClass,omitempty"`
	Occurrences string `json:"occurrences,omitempty"`
}

// Act implements loop.Actor.
func (a RealActor) Act(ctx context.Context, jobID string, d Decision) error {
	if a.Client == nil {
		return fmt.Errorf("jobs: RealActor.Client is nil")
	}
	if a.Journal == nil {
		return fmt.Errorf("jobs: RealActor.Journal is nil")
	}

	jobRec, err := a.resolveJobRecord(jobID)
	if err != nil {
		return err
	}
	issueID := jobRec.IssueID

	var ap actingPayload
	if jobRec.State == state.StateActing && len(jobRec.Payload) > 0 {
		// Replay (§2.2, finding 4): a prior attempt for this SAME jobID already compiled and
		// journaled the batch body -- re-send it verbatim rather than re-fetching the issue and
		// recompiling, which the LLM/Advisor is never re-consulted for and must not silently
		// diverge from what was already (partially) sent.
		if err := json.Unmarshal(jobRec.Payload, &ap); err != nil {
			return fmt.Errorf("jobs: RealActor: unmarshaling journaled compiled batch for job %s: %w", jobID, err)
		}
	} else {
		res, err := a.Client.GetIssue(ctx, issueID)
		if err != nil {
			return fmt.Errorf("jobs: RealActor: fetching issue %s for job %s: %w", issueID, jobID, err)
		}
		if res.Status < 200 || res.Status >= 300 {
			// Wrapped as *sentinel.StatusError (not a bare fmt.Errorf), matching
			// loop.HTTPIssueReader.GetIssue's own reasoning: this is the runner's Act-time re-read of
			// the issue (mid-job, after the precondition GET already succeeded once), so a 404 here is
			// the same "issue deleted between the event landing and this job running" race (C14) --
			// hardening N8e's tombstone/404 handling means the runner must be able to classify THIS
			// 404 as ClassGone too, not just the earlier precondition-read 404.
			return fmt.Errorf("jobs: RealActor: GET issue %s: %w", issueID, &sentinel.StatusError{Status: res.Status, Header: res.Header, Body: res.Body})
		}
		var env actIssueEnvelope
		if err := json.Unmarshal(res.Body, &env); err != nil {
			return fmt.Errorf("jobs: RealActor: decoding issue %s: %w", issueID, err)
		}

		fixEnabled := false
		if a.Fix != nil {
			fixEnabled, _ = a.Fix.FixEnabled(env.Issue.ProjectID)
		}

		actx := ActContext{
			JobID:         jobID,
			IssueID:       issueID,
			IssueType:     env.Issue.IssueType,
			ToolOutputs:   d.ToolOutputs,
			Secrets:       a.Secrets,
			FixEnabled:    fixEnabled,
			FixConfidence: a.FixConfidence,
			MaxVerbatim:   a.MaxVerbatim,
		}

		var compiled Compiled
		switch d.Kind {
		case "triage":
			td, err := DecodeTriage(d)
			if err != nil {
				return fmt.Errorf("jobs: RealActor: decoding triage decision for job %s: %w", jobID, err)
			}
			compiled, err = CompileTriage(actx, td)
			if err != nil {
				return err
			}
		case "followup":
			fd, err := DecodeFollowup(d)
			if err != nil {
				return fmt.Errorf("jobs: RealActor: decoding followup decision for job %s: %w", jobID, err)
			}
			compiled, err = CompileFollowup(actx, fd)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("jobs: RealActor: unknown decision kind %q for job %s", d.Kind, jobID)
		}

		ap = actingPayload{Compiled: compiled, ProjectID: env.Issue.ProjectID, ErrorClass: env.Issue.ErrorClass}
		if env.LatestOccurrence != nil && len(env.LatestOccurrence.Stacktrace) > 0 {
			ap.Occurrences = string(env.LatestOccurrence.Stacktrace)
		}

		payload, err := json.Marshal(ap)
		if err != nil {
			return fmt.Errorf("jobs: RealActor: marshaling compiled batch for job %s: %w", jobID, err)
		}
		if err := a.Journal.Append(state.Record{
			JobID:      jobID,
			IssueID:    issueID,
			Kind:       jobRec.Kind,
			TriggerSeq: jobRec.TriggerSeq,
			State:      state.StateActing,
			Payload:    payload,
		}); err != nil {
			return fmt.Errorf("jobs: RealActor: journaling compiled batch for job %s: %w", jobID, err)
		}
	}
	compiled := ap.Compiled

	_, qRes, bRes, err := Act(ctx, a.Client, compiled, true)
	if err != nil {
		return err
	}
	// §2.2: when Act posts a blocking question, journal a StateQuestioned record recording the
	// returned commentId BEFORE anything else observes success. This is what makes
	// Journal.HasOpenQuestion satisfiable -- without it, sweep.go's ReconcileReaped can never
	// re-claim a reaped question-waiting issue (it always sees no open question), and
	// resolveWaitingSince's journal fallback for pre-N9 rows has nothing to fall back to. Done
	// right after the question POST returns, matching "records the returned commentId ... survives"
	// (plan §2.2) -- not deferred to the caller's own journaling of StateActed/StateDone.
	if compiled.Question != nil {
		if journalErr := a.journalQuestioned(jobRec, qRes); journalErr != nil {
			return fmt.Errorf("jobs: RealActor: job %s: %w", jobID, journalErr)
		}
	}
	if bRes != nil {
		if walkErr := checkBatchResults(compiled, bRes); walkErr != nil {
			return fmt.Errorf("jobs: RealActor: job %s: %w", jobID, walkErr)
		}
	}
	// plan §4.4/§9 N8f: compiled.EnqueueFix was, until this wiring, a seam that only journaled the
	// decision to enqueue -- nothing ever ran the FIX engine. Everything above this point (the
	// batch send, the claim-kept-vs-released decision) already succeeded, so dispatching here is
	// the correct point: a FIX attempt should only ever start once the compiled decision has
	// actually landed on the issue. a.Fixer nil means the FIX engine isn't wired for this
	// deployment (e.g. FIX_EXECUTOR_CMD unset) -- EnqueueFix is then a no-op, matching "no repo
	// connection => propose-only" in spirit: FIX readiness gates whether anything runs, not
	// whether Act itself succeeds.
	if compiled.EnqueueFix && a.Fixer != nil {
		fixBrief := decisionFixBrief(d)
		// finding 1 (BLOCKER): the FIX job MUST NOT reuse the parent triage/followup jobID. The
		// runner drives that jobID to a terminal state (StateDone) immediately after Act returns,
		// and Journal.Append's terminal-guard (state/journal.go) silently drops any further
		// non-terminal record for an already-terminal jobId -- so journalFixRunning/
		// JournalFixPROpen would never persist, and every FIX batch idempotency key
		// (jobID:opIndex) would collide with the parent Act's own ops 0/1. Derive a distinct,
		// stable-per-trigger jobId the same way every other job kind does (state.JobID), so the
		// FIX job has its own independent lifecycle and idempotency-key namespace.
		fixJobID := state.JobID(FixKind, issueID, jobRec.TriggerSeq)
		a.Fixer.Dispatch(FixJobInput{
			JobID:       fixJobID,
			IssueID:     issueID,
			IssueURL:    a.issueURL(issueID),
			ProjectID:   ap.ProjectID,
			ErrorClass:  ap.ErrorClass,
			FixBrief:    fixBrief,
			Occurrences: ap.Occurrences,
			ToolOutputs: d.ToolOutputs,
			TriggerSeq:  jobRec.TriggerSeq,
		})
	}
	return nil
}

// issueURL builds the human-facing Sentinel issue URL from a.SentinelURL + the canonical
// (org-independent) issue route dashboard-web exposes at /issues/:id -- unlike the
// /:orgSlug/projects/:projectId/issues/:issueId route, this one needs nothing this envelope
// doesn't already have (just the bare issueID), so Act never needs a second fetch just to learn an
// org slug purely to build a link. Returns "" when SentinelURL is unconfigured (finding 3: better
// to omit the link than to fail or fabricate one).
func (a RealActor) issueURL(issueID string) string {
	base := strings.TrimRight(strings.TrimSpace(a.SentinelURL), "/")
	if base == "" || issueID == "" {
		return ""
	}
	return base + "/issues/" + issueID
}

// decisionFixBrief extracts the fixBrief string from whichever decision schema d.Kind names --
// CompileTriage/CompileFollowup already validated its presence whenever EnqueueFix is true (both
// require a non-empty fixBrief before setting it), so this only ever runs after that check passed.
func decisionFixBrief(d Decision) string {
	switch d.Kind {
	case "triage":
		if td, err := DecodeTriage(d); err == nil && td.FixBrief != nil {
			return *td.FixBrief
		}
	case "followup":
		if fd, err := DecodeFollowup(d); err == nil && fd.FixBrief != nil {
			return *fd.FixBrief
		}
	}
	return ""
}

// questionedPayload is state.Record.Payload's shape at a StateQuestioned record: the id of the
// blocking-question comment PostQuestion just created, per plan §2.2 ("records the returned
// commentId ... survives").
type questionedPayload struct {
	CommentID string `json:"commentId"`
}

// journalQuestioned decodes qRes's `{comment:{id:...}}` body (the questions endpoint's response
// shape, apps/dashboard-web's questions +server.ts) and appends the plan §2.2 StateQuestioned
// record for jobRec's job. qRes is never nil here -- Act only returns a nil error with
// compiled.Question set after PostQuestion itself returned a non-nil *sentinel.Result and no
// error -- but a defensive nil check keeps this from panicking if that contract is ever loosened.
func (a RealActor) journalQuestioned(jobRec state.Record, qRes *sentinel.Result) error {
	var env struct {
		Comment struct {
			ID string `json:"id"`
		} `json:"comment"`
	}
	if qRes != nil {
		if err := json.Unmarshal(qRes.Body, &env); err != nil {
			return fmt.Errorf("jobs: RealActor: decoding question response for job %s: %w", jobRec.JobID, err)
		}
	}
	payload, err := json.Marshal(questionedPayload{CommentID: env.Comment.ID})
	if err != nil {
		return fmt.Errorf("jobs: RealActor: marshaling questioned payload for job %s: %w", jobRec.JobID, err)
	}
	return a.Journal.Append(state.Record{
		JobID:      jobRec.JobID,
		IssueID:    jobRec.IssueID,
		Kind:       jobRec.Kind,
		TriggerSeq: jobRec.TriggerSeq,
		State:      state.StateQuestioned,
		Payload:    payload,
	})
}

// checkBatchResults walks bRes's per-op results[] (C3: a batch is HTTP 200 with per-op outcomes)
// via sentinel.ClassifyBatch, using compiled.Ops[i].Op to build each op's BatchOpMeta. A dedup
// or droppable-conflict result is not a failure; any other non-success op classification (a
// permanent rejection, an auth failure, a gone issue, an unresolved transient/rate-limited op,
// or an unreachable-by-construction key mismatch) means part of what the Advisor decided to
// publish was NOT actually sent -- Act returning err == nil in that case would journal a partial
// failure as full success, which is exactly what the validator flagged. This treats any such op
// as a job failure rather than silently recording it as acted.
func checkBatchResults(compiled Compiled, bRes *sentinel.Result) error {
	meta := make([]sentinel.BatchOpMeta, len(compiled.Ops))
	for i, op := range compiled.Ops {
		meta[i] = sentinel.BatchOpMeta{
			IsClaimOp:    op.Op == "issues.claim" || op.Op == "issues.claim.release",
			IsRelationOp: op.Op == "issues.relations.add",
			IsSeverityOp: op.Op == "issues.report.severity",
		}
	}
	overall, perOp, _ := sentinel.ClassifyBatch(bRes.Status, bRes.Body, meta)
	if perOp == nil {
		// BLOCKER (finding 1): a non-2xx batch ENVELOPE (401/429/5xx) short-circuits
		// ClassifyBatch to (envelope's own class, nil, nil) before it ever walks results[] --
		// perOp being nil here means the failure is at the envelope level (or, for a 2xx envelope
		// with an unparsable body, ClassPermanent), NOT "every op succeeded". Discarding `overall`
		// and only looping over `perOp` (as this used to) made a 401/429/5xx batch response look
		// like an empty-but-successful per-op walk -- the whole decision was lost and never
		// retried/journaled failed. Wrap overall's status as a *sentinel.StatusError so the runner
		// (loop.classifyRunnerFailureClass) classifies 429/5xx as transient (retry) and 401 as auth
		// (terminal), exactly like every other envelope-level failure this package surfaces.
		return fmt.Errorf("batch envelope failed: status=%d class=%v: %w", bRes.Status, overall, &sentinel.StatusError{Status: bRes.Status, Header: bRes.Header, Body: bRes.Body})
	}
	for i, c := range perOp {
		switch c {
		case sentinel.ClassSuccess, sentinel.ClassConflictDroppable:
			continue
		case sentinel.ClassGone:
			// A 404 on an individual batch op (race: the issue was deleted between the precondition
			// read and this batch landing, C14) must be distinguishable from every other batch
			// failure so the runner can journal skipped(deleted) instead of failed (N8e hardening,
			// matching the mid-job GET's own *sentinel.StatusError wrapping above). Status 404 here
			// stands for "this op's own result was Gone", not a literal envelope status.
			opName := "?"
			if i < len(compiled.Ops) {
				opName = compiled.Ops[i].Op
			}
			return fmt.Errorf("batch op %d (%s) is gone: %w", i, opName, &sentinel.StatusError{Status: 404})
		default:
			opName := "?"
			if i < len(compiled.Ops) {
				opName = compiled.Ops[i].Op
			}
			// Wrap a *sentinel.StatusError carrying a status representative of this op's class, so
			// the runner (classifyRunnerFailureClass) journals a PERMANENT per-op rejection
			// (400/401/409) as failed_permanent — NOT retried MaxInlaneRetries times — while a
			// genuinely transient/rate-limited op stays retryable. A bare error defaults to
			// ClassTransient, which would spin a permanent 400/422 through 5 pointless resends.
			return fmt.Errorf("batch op %d (%s) did not succeed: class=%v: %w",
				i, opName, c, &sentinel.StatusError{Status: statusForOpClass(c)})
		}
	}
	return nil
}

// statusForOpClass maps a per-op FailureClass to a representative HTTP status whose
// ClassifyEnvelope round-trips to the same retry disposition, so a per-op result classified in
// checkBatchResults' default branch is retried (transient/rate-limited) or terminal
// (permanent/auth/conflict) consistently with envelope-level failures.
func statusForOpClass(c sentinel.FailureClass) int {
	switch c {
	case sentinel.ClassRateLimited:
		return 429
	case sentinel.ClassTransient:
		return 503
	case sentinel.ClassAuthFailure:
		return 401
	case sentinel.ClassConflictForeign, sentinel.ClassConflictKeyMismatch:
		return 409
	default: // ClassPermanent and anything else terminal
		return 400
	}
}

// resolveJobRecord looks up jobID's latest journal record -- the same record loop.Runner just
// appended (state.StateClaimed, at the latest) before ever calling Advisor/Act for this job -- so
// it is always present by the time Act runs. Returns the full record (not just IssueID) so
// journalQuestioned can carry the SAME Kind/TriggerSeq the rest of this job's journal trail uses,
// rather than re-deriving or guessing them.
func (a RealActor) resolveJobRecord(jobID string) (state.Record, error) {
	latest, err := a.Journal.LatestByJobID()
	if err != nil {
		return state.Record{}, fmt.Errorf("jobs: RealActor: reading journal for job %s: %w", jobID, err)
	}
	rec, ok := latest[jobID]
	if !ok || rec.IssueID == "" {
		return state.Record{}, fmt.Errorf("jobs: RealActor: no journal record with an issueId for job %s", jobID)
	}
	return rec, nil
}
