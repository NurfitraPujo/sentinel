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
		occurrences := ""
		if env.LatestOccurrence != nil && len(env.LatestOccurrence.Stacktrace) > 0 {
			occurrences = string(env.LatestOccurrence.Stacktrace)
		}
		a.Fixer.Dispatch(FixJobInput{
			JobID:       jobID,
			IssueID:     issueID,
			IssueURL:    a.issueURL(issueID),
			ProjectID:   env.Issue.ProjectID,
			ErrorClass:  env.Issue.ErrorClass,
			FixBrief:    fixBrief,
			Occurrences: occurrences,
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
	_, perOp, _ := sentinel.ClassifyBatch(bRes.Status, bRes.Body, meta)
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
			return fmt.Errorf("batch op %d (%s) did not succeed: class=%v", i, opName, c)
		}
	}
	return nil
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
