package loop

import (
	"encoding/json"
	"testing"
)

const me = "agent-me"

func strp(s string) *string { return &s }

func TestClassify_EchoSuppression(t *testing.T) {
	e := Event{Type: "created", ActorID: me}
	if got := Classify(e, me); got != KindNone {
		t.Fatalf("expected echo-suppressed event to classify KindNone, got %q", got)
	}
}

func TestClassify_CreatedAndReportCreatedAlwaysTriage(t *testing.T) {
	for _, typ := range []string{"created", "report_created"} {
		e := Event{Type: typ, ActorID: "someone-else"}
		if got := Classify(e, me); got != KindTriage {
			t.Errorf("%s: got %q, want KindTriage", typ, got)
		}
	}
}

func TestClassify_OccurrenceBurstAndRegressed(t *testing.T) {
	cases := []struct {
		name  string
		issue *EventIssue
		want  Kind
	}{
		{"unclaimed", &EventIssue{AssigneeType: nil}, KindTriage},
		{"claimed by me", &EventIssue{AssigneeType: strp("agent"), AssignedTo: strp(me)}, KindTriage},
		{"claimed by another agent", &EventIssue{AssigneeType: strp("agent"), AssignedTo: strp("agent-other")}, KindNone},
		{"assigned to a user", &EventIssue{AssigneeType: strp("user"), AssignedTo: strp("user-1")}, KindNone},
	}
	for _, typ := range []string{"occurrence_burst", "regressed"} {
		for _, c := range cases {
			e := Event{Type: typ, ActorID: "someone-else", Issue: c.issue}
			if got := Classify(e, me); got != c.want {
				t.Errorf("%s/%s: got %q, want %q", typ, c.name, got, c.want)
			}
		}
	}
}

func TestClassify_QuestionAnsweredAndCommented(t *testing.T) {
	mine := &EventIssue{AssigneeType: strp("agent"), AssignedTo: strp(me)}
	other := &EventIssue{AssigneeType: strp("agent"), AssignedTo: strp("agent-other")}

	if got := Classify(Event{Type: "question_answered", ActorID: "user-1", Issue: mine}, me); got != KindFollowUp {
		t.Errorf("question_answered assigned to me: got %q, want KindFollowUp", got)
	}
	if got := Classify(Event{Type: "question_answered", ActorID: "user-1", Issue: other}, me); got != KindNone {
		t.Errorf("question_answered assigned to another agent: got %q, want KindNone", got)
	}
	if got := Classify(Event{Type: "commented", ActorID: "user-1", Issue: mine}, me); got != KindFollowUp {
		t.Errorf("commented, assigned to me, foreign actor: got %q, want KindFollowUp", got)
	}
	if got := Classify(Event{Type: "commented", ActorID: "user-1", Issue: other}, me); got != KindNone {
		t.Errorf("commented, assigned to another agent: got %q, want KindNone", got)
	}
}

// TestClassify_QuestionAnswered_OpenQuestionOrArm proves the second half of plan §3's
// question_answered rule: "FOLLOW-UP if assignedTo == me OR journal shows my open question". A
// reaped claim (server reaper, C11) means assignedTo no longer reads "me" by the time the answer
// lands, so without the OR-arm the answer to our own blocking question is silently dropped
// forever -- the "dead question loop" defect class. Deleting the OR-arm (reverting to the plain
// `if len(hasOpenQuestion) > 0 ...` block) must turn this red.
func TestClassify_QuestionAnswered_OpenQuestionOrArm(t *testing.T) {
	unassigned := &EventIssue{ID: "i1", AssigneeType: strp(""), AssignedTo: nil}
	e := Event{Type: "question_answered", ActorID: "user-1", Issue: unassigned}

	if got := Classify(e, me); got != KindNone {
		t.Fatalf("with no open-question hook: got %q, want KindNone", got)
	}
	if got := Classify(e, me, func(issueID string) bool { return issueID == "i1" }); got != KindFollowUp {
		t.Fatalf("with an open question recorded for i1: got %q, want KindFollowUp", got)
	}
	if got := Classify(e, me, func(issueID string) bool { return false }); got != KindNone {
		t.Fatalf("with no open question recorded: got %q, want KindNone", got)
	}
}

func TestClassify_ClaimReleased(t *testing.T) {
	mineVal, _ := json.Marshal(claimReleasedValue{Reason: "stale", PreviousAssignee: me})
	otherVal, _ := json.Marshal(claimReleasedValue{Reason: "stale", PreviousAssignee: "agent-other"})

	if got := Classify(Event{Type: "claim_released", ActorID: "system", NewValue: mineVal}, me); got != KindSweepReconcile {
		t.Errorf("claim_released (mine): got %q, want KindSweepReconcile", got)
	}
	if got := Classify(Event{Type: "claim_released", ActorID: "system", NewValue: otherVal}, me); got != KindNone {
		t.Errorf("claim_released (not mine): got %q, want KindNone", got)
	}
}

func TestClassify_StatusChanged(t *testing.T) {
	resolvedVal, _ := json.Marshal(statusChangedValue{Status: "resolved"})
	ignoredVal, _ := json.Marshal(statusChangedValue{Status: "ignored"})

	if got := Classify(Event{Type: "status_changed", ActorID: "user-1", NewValue: resolvedVal}, me); got != KindCancelQueued {
		t.Errorf("status_changed resolved: got %q, want KindCancelQueued", got)
	}
	if got := Classify(Event{Type: "status_changed", ActorID: "user-1", NewValue: ignoredVal}, me); got != KindNone {
		t.Errorf("status_changed ignored: got %q, want KindNone", got)
	}
}

// TestClassify_StatusChanged_BulkDashboardShape is the red-first proof for finding #9:
// apps/dashboard-web/src/lib/db/queries/issues.ts's batchUpdateIssues (the dashboard bulk-resolve
// action) writes status_changed's newValue as `{action, ...options}` -- there is NO `status` key
// at all, unlike the single-issue PATCH path's `{status, resolvedInVersion}`. Before the fix,
// Classify's status_changed case read only newValue.status, so a bulk-resolved issue's queued job
// was never cancelled (KindNone instead of KindCancelQueued) -- reverting statusChangedValue.
// isResolved to `v.Status == "resolved"` alone makes this go red.
func TestClassify_StatusChanged_BulkDashboardShape(t *testing.T) {
	bulkResolvedVal, _ := json.Marshal(map[string]any{
		"action":            "resolve",
		"resolvedInVersion": "1.2.3",
		"actorType":         "user",
		"actorId":           "user-1",
	})
	if got := Classify(Event{Type: "status_changed", ActorID: "user-1", NewValue: bulkResolvedVal}, me); got != KindCancelQueued {
		t.Errorf("bulk status_changed resolve: got %q, want KindCancelQueued", got)
	}

	bulkIgnoreVal, _ := json.Marshal(map[string]any{"action": "ignore"})
	if got := Classify(Event{Type: "status_changed", ActorID: "user-1", NewValue: bulkIgnoreVal}, me); got != KindNone {
		t.Errorf("bulk status_changed ignore: got %q, want KindNone", got)
	}
}

// TestClassify_ClaimReleased_DashboardUnassignShape is the red-first proof for finding #10:
// apps/dashboard-web/src/lib/db/queries/issues.ts's assignIssue (the D24 dashboard-unassign path)
// is fixed to write claim_released's newValue with a `previousAssignee` field, matching the
// reaper's shape (apps/dashboard-web/src/lib/server/retention.ts), so Classify's existing
// PreviousAssignee read covers it -- this test pins that contract from the worker side. Before
// the dashboard fix, this shape carried only `{assigneeType, assignedTo}` and PreviousAssignee
// decoded to "", permanently losing the reconcile.
func TestClassify_ClaimReleased_DashboardUnassignShape(t *testing.T) {
	val, _ := json.Marshal(map[string]any{
		"assigneeType":     nil,
		"assignedTo":       nil,
		"previousAssignee": me,
	})
	if got := Classify(Event{Type: "claim_released", ActorID: "dashboard-user-1", NewValue: val}, me); got != KindSweepReconcile {
		t.Errorf("dashboard-unassign claim_released (mine): got %q, want KindSweepReconcile", got)
	}
}

func TestClassify_IssueDeleted(t *testing.T) {
	if got := Classify(Event{Type: "issue_deleted", ActorID: "system"}, me); got != KindSkippedDeleted {
		t.Errorf("issue_deleted: got %q, want KindSkippedDeleted", got)
	}
}

// allAgentEventTypes mirrors apps/dashboard-web/src/lib/server/agent-events.ts's AGENT_EVENT_TYPES
// verbatim (also documented at docs/agents/SENTINEL_AGENT_GUIDE.md §3) — the full, real
// `issue_activity.event_type` vocabulary the feed can emit. Copied by hand rather than parsed from
// the TS source so a future vocabulary change fails this test instead of silently going uncovered
// (CLAUDE.md: "a test must create the state it asserts on").
var allAgentEventTypes = []string{
	"status_changed", "assigned", "unassigned", "regressed", "ai_analysis", "linked",
	"commented", "claimed", "claim_released", "progress_update", "question_asked",
	"question_answered", "moved", "attachment_added", "report_edited", "report_created",
	"created", "occurrence_burst", "issue_deleted",
}

// handledKindsByType is the expected Classify outcome for every real event type, using a neutral
// event (foreign actor, unclaimed issue, no NewValue) so only the type-driven default arms are
// exercised. Types not listed here are expected to classify KindNone.
var handledKindsByType = map[string]Kind{
	"created":          KindTriage,
	"report_created":   KindTriage,
	"occurrence_burst": KindTriage, // unclaimed issue -> triage
	"regressed":        KindTriage, // unclaimed issue -> triage
	"issue_deleted":    KindSkippedDeleted,
}

// TestClassify_AllRealEventTypes walks the FULL real event vocabulary (all 18 types the server can
// emit, mirrored from AGENT_EVENT_TYPES) and asserts Classify's outcome for each — the 5 types with
// dedicated dispatch-table entries classify as expected, and the remaining 13 (including
// claim_released and question_answered/commented, which need issue/newValue state the neutral
// fixture here doesn't provide, so they fall through to KindNone) classify KindNone. This replaces
// a prior test that walked 9 invented type names, 5 of which do not exist in the real vocabulary.
func TestClassify_AllRealEventTypes(t *testing.T) {
	if len(allAgentEventTypes) != 19 {
		t.Fatalf("expected 19 documented event types (18 emitted by dashboard-web + issue_deleted), got %d", len(allAgentEventTypes))
	}
	for _, typ := range allAgentEventTypes {
		want, handled := handledKindsByType[typ]
		if !handled {
			want = KindNone
		}
		e := Event{Type: typ, ActorID: "someone-else"}
		if got := Classify(e, me); got != want {
			t.Errorf("%s: got %q, want %q", typ, got, want)
		}
	}
}

func TestKind_IsJob(t *testing.T) {
	if !KindTriage.IsJob() || !KindFollowUp.IsJob() {
		t.Errorf("triage/followup must be jobs")
	}
	if KindSweepReconcile.IsJob() || KindCancelQueued.IsJob() || KindSkippedDeleted.IsJob() || KindNone.IsJob() {
		t.Errorf("non-triage/followup kinds must not be jobs")
	}
}

// TestClassify_OccurrenceBurstSuppressedByOpenFix is the RED-FIRST proof for finding 3
// (fix-lifecycle remediation round 2): an occurrence_burst/regressed event on an issue that
// already has an open FIX must classify KindNone, not KindTriage -- re-triaging a fixable issue
// while its FIX PR is out for review would re-decide fixable and dispatch a SECOND, duplicate FIX
// PR (a fresh trigger seq mints a distinct jobID, so the per-jobID journal dedup never catches it).
//
// MUTATION-TEST NOTE: delete the `if hasOpenFix != nil { ... }` arm from Classify's
// occurrence_burst/regressed case (its pre-fix shape) and this test goes red -- both cases would
// classify KindTriage regardless of hasOpenFix's answer.
func TestClassify_OccurrenceBurstSuppressedByOpenFix(t *testing.T) {
	mine := &EventIssue{ID: "issue-1", AssigneeType: strp("agent"), AssignedTo: strp(me)}
	for _, typ := range []string{"occurrence_burst", "regressed"} {
		e := Event{Type: typ, ActorID: "someone-else", Issue: mine, Seq: 1}

		hasOpenFixTrue := func(issueID string) bool { return true }
		if got := Classify(e, me, nil, hasOpenFixTrue); got != KindNone {
			t.Errorf("%s: with an open FIX, got %q, want KindNone", typ, got)
		}

		hasOpenFixFalse := func(issueID string) bool { return false }
		if got := Classify(e, me, nil, hasOpenFixFalse); got != KindTriage {
			t.Errorf("%s: with no open FIX, got %q, want KindTriage", typ, got)
		}

		// Omitted hasOpenFix hook (2-arg call, every pre-finding-3 call site) must keep behaving
		// exactly as before -- KindTriage, never blocked by a hook that was never wired.
		if got := Classify(e, me); got != KindTriage {
			t.Errorf("%s: with no hasOpenFix hook at all, got %q, want KindTriage", typ, got)
		}
	}
}
