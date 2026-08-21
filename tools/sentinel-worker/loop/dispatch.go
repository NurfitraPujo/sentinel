// Package loop implements sentinel-worker's poll → dispatch → run pipeline (plan §0/§3):
// loop/poll.go polls the events feed and feeds the journal's enqueue step, loop/dispatch.go
// classifies each Issue Activity Event into a job kind using ONLY the event type and the payload's
// current-state fields (C2), and loop/runner.go carries a dispatched job through
// resolve → preconditions → ensure-claimed → Advisor → journal → act.
package loop

import (
	"encoding/json"
	"fmt"
	"time"
)

// EventIssue is the per-event issue snapshot the feed carries (C2): CURRENT state at read time,
// not state at event time (events.ts:63-66) — fine for dispatch, not for history reconstruction.
type EventIssue struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	IssueType    string  `json:"issueType"`
	ProjectID    string  `json:"projectId"`
	AssigneeType *string `json:"assigneeType"`
	AssignedTo   *string `json:"assignedTo"`
	ClaimedAt    *string `json:"claimedAt"`
	WaitingOn    *string `json:"waitingOn"`
}

// Event is one row from GET /api/agent/events (Issue Activity Event, per CONTEXT.md).
//
// Wire tags match the server verbatim (SENTINEL_AGENT_GUIDE.md §3 / agent-events.ts): the JSON
// key is `eventType`, not `type`; there is NO top-level issue id field — it is nested at
// `issue.id`; the timestamp key is `createdAt`, not `at`. Getting this wrong makes Classify
// return KindNone for every real event and IssueID() return "" (B5 cross-boundary failure).
type Event struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"eventType"`
	ActorID   string          `json:"actorId"`
	ActorType string          `json:"actorType"`
	Issue     *EventIssue     `json:"issue"`
	NewValue  json.RawMessage `json:"newValue,omitempty"`
	At        time.Time       `json:"createdAt"`
}

// IssueID returns the id of the issue this event belongs to, read from the nested `issue` object
// (the feed carries no top-level issue id). Returns an error when Issue is nil so callers cannot
// silently build a job for issueID "".
func (e Event) IssueID() (string, error) {
	if e.Issue == nil || e.Issue.ID == "" {
		return "", fmt.Errorf("event seq %d: missing issue payload, cannot derive issue id", e.Seq)
	}
	return e.Issue.ID, nil
}

// Kind is the dispatcher's classification of one event into a job kind (plan §3's table). Kinds
// that are jobs get queued (and coalesced, §3's "Coalescing"); the others act directly.
type Kind string

const (
	KindNone           Kind = "" // no dispatch: cursor-advance only
	KindTriage         Kind = "triage"
	KindFollowUp       Kind = "followup"
	KindSweepReconcile Kind = "sweep-reconcile" // claim_released reconcile, §2.7
	KindCancelQueued   Kind = "cancel-queued"   // status_changed -> resolved
	KindSkippedDeleted Kind = "skipped-deleted" // issue_deleted tombstone, C14
)

// statusChangedValue is the newValue shape for a status_changed event, enough to read the new
// status (plan §3's runner-precondition column: "newValue.status == resolved").
//
// Two different writers produce a status_changed row with two different newValue shapes:
//   - apps/dashboard-web/src/lib/db/queries/issues.ts's updateIssueStatus (single-issue PATCH)
//     writes `newValue: { status, resolvedInVersion }` -- Status above reads this directly.
//   - the SAME file's batchUpdateIssues (bulk dashboard action) writes
//     `newValue: { action, ...options }` for action in {resolve, ignore, unresolve} -- there is NO
//     `status` key at all, so Status decodes to "" and the resolved check below would never fire
//     for a bulk-resolved issue (validator finding: "the dashboard BULK path writes a different
//     newValue shape"). Action captures that second shape's discriminator.
type statusChangedValue struct {
	Status string `json:"status"`
	Action string `json:"action"`
}

// isResolved reports whether this status_changed newValue represents a transition to "resolved",
// across BOTH writer shapes (see statusChangedValue's doc comment).
func (v statusChangedValue) isResolved() bool {
	return v.Status == "resolved" || v.Action == "resolve"
}

// claimReleasedValue is the newValue shape for a claim_released event (plan §2.7/§3).
//
// Three writers produce a claim_released row:
//   - apps/dashboard-web/src/lib/server/retention.ts (the stale-claim reaper) writes
//     `newValue: { previousAssignee, reason: 'stale' }` -- PreviousAssignee reads this directly.
//   - apps/dashboard-web/src/lib/db/queries/reports.ts's releaseClaim (an agent releasing its OWN
//     claim) writes `newValue: { force }` with no previousAssignee at all, but its actorId is the
//     releasing agent itself, so Classify's echo-suppression check above (e.ActorID == myAgentID)
//     already drops it before this case is ever reached -- no handling needed here.
//   - apps/dashboard-web/src/lib/db/queries/issues.ts's assignIssue (D24 dashboard-unassign of an
//     agent-claimed issue) is actor-attributed to the DASHBOARD USER, not the agent, so it is NOT
//     echo-suppressed, and previously wrote `newValue: { assigneeType, assignedTo }` with no
//     previousAssignee either -- PreviousAssignee decoded to "" and this claim_released could never
//     match myAgentID, silently losing the D24 dashboard-unassign reconcile path (validator
//     finding). Fixed at the source: assignIssue now also writes previousAssignee for this case, so
//     PreviousAssignee below reads it the same way it reads the reaper's.
type claimReleasedValue struct {
	Reason           string `json:"reason"`
	PreviousAssignee string `json:"previousAssignee"`
}

// Classify implements the plan §3 dispatch table using only the event's type and the payload's
// current-claim-state fields (no read per event) — myAgentID is the Agent identity this worker
// runs under, used both for echo suppression and for "assignedTo == me" checks.
//
// hooks is Classify's optional-callback seam: hasOpenQuestion and hasOpenFix. Kept as a variadic
// []func(string) bool (rather than two named parameters) so every existing 2-arg call site
// (Classify(e, myAgentID)) and the pre-finding-3 3-arg call site (Classify(e, myAgentID,
// hasOpenQuestion)) both keep compiling unchanged — position 0 is always hasOpenQuestion, position
// 1 is always hasOpenFix (finding 3, fix-lifecycle remediation round 2); a caller that only wants
// hasOpenFix must still pass a (possibly nil) hasOpenQuestion in position 0.
//
// hasOpenQuestion implements the "OR the journal shows my open question" arm of the
// question_answered rule (plan §3): PollLoop wires it to Journal.HasOpenQuestion.
//
// hasOpenFix implements the plan-mandated "skip if FIX in flight per journal" rule for
// occurrence_burst/regressed (finding 3): without it, an issue that keeps erroring while its FIX
// PR is out for review gets re-triaged on every burst/regression, and a fixable re-decision
// dispatches a SECOND, duplicate FIX PR (a fresh trigger seq mints a distinct jobID, so the
// existing per-jobID dedup never catches it). PollLoop wires this to Journal.HasOpenKind(issueID,
// jobs.FixKind) — the SAME non-terminal-fix-record scan jobs/sweep.go's hasOpenFix already
// performs for ReconcileReaped, reused rather than re-implemented (CLAUDE.md B3: a second,
// diverging copy of "is there an open fix" is exactly how these dedup checks rot).
func Classify(e Event, myAgentID string, hooks ...func(issueID string) bool) Kind {
	var hasOpenQuestion, hasOpenFix func(issueID string) bool
	if len(hooks) > 0 {
		hasOpenQuestion = hooks[0]
	}
	if len(hooks) > 1 {
		hasOpenFix = hooks[1]
	}

	// Echo suppression: our own writes echo on the feed and must never re-dispatch (plan §3).
	if e.ActorID != "" && e.ActorID == myAgentID {
		return KindNone
	}

	switch e.Type {
	case "created", "report_created":
		return KindTriage
	case "occurrence_burst", "regressed":
		if !unclaimedOrMine(e.Issue, myAgentID) {
			return KindNone
		}
		if hasOpenFix != nil {
			if issueID, err := e.IssueID(); err == nil && hasOpenFix(issueID) {
				return KindNone
			}
		}
		return KindTriage
	case "question_answered":
		if assignedToMe(e.Issue, myAgentID) {
			return KindFollowUp
		}
		// The claim may have been reaped since we asked (server reaper, C11) — assignedTo no
		// longer reads "me", but the journal still shows an unresolved question we asked on this
		// issue. Without this arm, a reaped claim permanently orphans that question thread: the
		// answer arrives and nobody ever follows up on it.
		if hasOpenQuestion != nil {
			if issueID, err := e.IssueID(); err == nil && hasOpenQuestion(issueID) {
				return KindFollowUp
			}
		}
		return KindNone
	case "commented":
		if assignedToMe(e.Issue, myAgentID) && e.ActorID != myAgentID {
			return KindFollowUp
		}
		return KindNone
	case "claim_released":
		var v claimReleasedValue
		_ = json.Unmarshal(e.NewValue, &v)
		if v.PreviousAssignee == myAgentID {
			return KindSweepReconcile
		}
		return KindNone
	case "status_changed":
		var v statusChangedValue
		_ = json.Unmarshal(e.NewValue, &v)
		if v.isResolved() {
			return KindCancelQueued
		}
		return KindNone
	case "issue_deleted":
		return KindSkippedDeleted
	default:
		return KindNone
	}
}

func unclaimedOrMine(issue *EventIssue, myAgentID string) bool {
	if issue == nil {
		return true
	}
	if issue.AssigneeType == nil || *issue.AssigneeType == "" {
		return true
	}
	return *issue.AssigneeType == "agent" && issue.AssignedTo != nil && *issue.AssignedTo == myAgentID
}

func assignedToMe(issue *EventIssue, myAgentID string) bool {
	if issue == nil || issue.AssigneeType == nil || issue.AssignedTo == nil {
		return false
	}
	return *issue.AssigneeType == "agent" && *issue.AssignedTo == myAgentID
}

// IsJob reports whether k represents a queueable job (subject to per-issue serial ordering and
// same-kind coalescing, plan §3), as opposed to an immediate cursor-only or cancel action.
func (k Kind) IsJob() bool {
	return k == KindTriage || k == KindFollowUp
}
