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
	Secrets       []string           // configured secret values to redact (guard.Check's verbatim gate)
	FixConfidence float64            // WORKER_FIX_CONFIDENCE; <=0 uses DefaultFixConfidence
}

// actIssueEnvelope decodes just the fields Act needs from GET /api/agent/issues/:id -- issueType
// (C8's severity gate) and projectId (the FIX-enablement lookup). Deliberately narrower than
// triageIssueEnvelope, which also needs message/errorClass/report/occurrence for prompt-seeding
// that Act itself never touches.
type actIssueEnvelope struct {
	Issue struct {
		ProjectID string `json:"projectId"`
		IssueType string `json:"issueType"`
	} `json:"issue"`
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
	return nil
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
