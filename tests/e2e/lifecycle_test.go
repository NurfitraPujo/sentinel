// Package e2e — this file covers matrix rows U11, U12, U13 (docs/plans/E2E_RECOVERY_PLAN.md, "P7 — The
// E2E proof harness"): issue-lifecycle regression detection and issue relations, driven against the real
// compose stack via the harness in main_test.go/harness_test.go.
//
// # History
//
// An earlier version of this file duplicated its own read helpers (lifecycleIssues, lifecycleOnlyIssue,
// lifecycleActivity, lifecycleResolve) because harness_test.go's f.issues()/f.onlyIssue()/f.activity()
// referenced columns that do not exist on the real schema (`title`, `first_release`, `last_release` on
// `issues`; `ia.action` on `issue_activity` instead of the real `event_type`), and f.setIssueStatus never
// set resolved_in_version, which the processor's regression comparison requires to tell "resolved at an
// OLDER release" (U12) apart from "no prior release recorded" (always a regression). Those were bugs in
// the harness itself — confirmed against `\d issues` / `\d issue_activity` on the live compose Postgres —
// and have since been fixed in harness_test.go directly (issueRow now carries the real columns, f.activity
// filters on event_type, and f.resolve(issueID, resolvedInVersion) sets resolved_in_version). This file now
// uses those shared readers instead of re-deriving them, so a future column rename only needs fixing in one
// place.
//
// issue_relations has no harness reader (the shared foundation doesn't need one for any other matrix row),
// so this file keeps its own small helpers for that table.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// ---------------------------------------------------------------------------------------------------
// issue_relations helpers (lifecycle-specific: no shared harness reader exists for this table).
// ---------------------------------------------------------------------------------------------------

// lifecycleRelationCount counts issue_relations rows matching the given (source, target, type) triple.
func lifecycleRelationCount(t *testing.T, sourceID, targetID, relationType string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_relations
		  WHERE source_issue_id = $1 AND target_issue_id = $2 AND relation_type = $3`,
		sourceID, targetID, relationType).Scan(&n); err != nil {
		t.Fatalf("lifecycle: counting issue_relations: %v", err)
	}
	return n
}

// lifecycleRelationRow is the subset of the real `issue_relations` columns this file asserts on.
type lifecycleRelationRow struct {
	ID            string
	CreatedByType string
	CreatedBy     string
}

// lifecycleOnlyRelation returns the single issue_relations row for (source, target, type), failing if
// there is not exactly one.
func lifecycleOnlyRelation(t *testing.T, sourceID, targetID, relationType string) lifecycleRelationRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, created_by_type, created_by FROM issue_relations
		  WHERE source_issue_id = $1 AND target_issue_id = $2 AND relation_type = $3`,
		sourceID, targetID, relationType)
	if err != nil {
		t.Fatalf("lifecycle: querying issue_relations: %v", err)
	}
	defer rows.Close()

	var out []lifecycleRelationRow
	for rows.Next() {
		var r lifecycleRelationRow
		if err := rows.Scan(&r.ID, &r.CreatedByType, &r.CreatedBy); err != nil {
			t.Fatalf("lifecycle: scanning issue_relations: %v", err)
		}
		out = append(out, r)
	}
	if len(out) != 1 {
		t.Fatalf("lifecycle: expected exactly 1 issue_relations row for (%s -> %s, %s), found %d",
			sourceID, targetID, relationType, len(out))
	}
	return out[0]
}

// ---------------------------------------------------------------------------------------------------
// U11 — resolve, then a NEWER release recurs: regression must fire.
// ---------------------------------------------------------------------------------------------------

// TestU11_NewerReleaseRecurrenceRegresses is matrix row U11: resolve an issue, then the same error
// recurs in a newer release. Asserted end state: regression_status set, regression_count=1,
// last_regressed_at set, and an issue_activity row with event_type 'regressed'. This is the regression
// test for S5 (release_version was destroyed by normalization before the store ever read it — B6 in
// docs/memory/BUGS.md), so this row proves the fix stays fixed rather than merely that the code compiles.
func TestU11_NewerReleaseRecurrenceRegresses(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	first := f.newEvent().with(map[string]any{"release_version": "1.0.0"})
	if res := f.ingest(first); res.Status != http.StatusAccepted {
		t.Fatalf("U11: first ingest: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(1)
	f.waitForIssues(1)

	fresh := f.onlyIssue()
	if fresh.RegressionStatus != "none" || fresh.RegressionCount != 0 {
		t.Fatalf("U11: freshly-created issue already looks regressed: status=%q count=%d",
			fresh.RegressionStatus, fresh.RegressionCount)
	}

	f.resolve(fresh.ID, "1.0.0")

	second := f.newEvent().with(map[string]any{"release_version": "2.0.0"})
	if res := f.ingest(second); res.Status != http.StatusAccepted {
		t.Fatalf("U11: second (recurrence) ingest: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	// Positive synchronization point: the second event has been fully processed once its occurrence
	// lands. Only then is it meaningful to inspect the issue's regression fields.
	f.waitForOccurrences(2)

	got := f.onlyIssue()
	if got.RegressionStatus != "regressed" {
		t.Fatalf("U11: regression_status = %q, want %q (issue %s)", got.RegressionStatus, "regressed", got.ID)
	}
	if got.RegressionCount != 1 {
		t.Fatalf("U11: regression_count = %d, want 1 (issue %s)", got.RegressionCount, got.ID)
	}
	if got.LastRegressedAt == nil {
		t.Fatalf("U11: last_regressed_at is NULL, want set (issue %s)", got.ID)
	}
	if got.Status != "unresolved" {
		t.Fatalf("U11: status = %q after regression, want %q (a regression must reopen the issue)", got.Status, "unresolved")
	}
	if got.ResolvedInVersion != nil {
		t.Fatalf("U11: resolved_in_version = %q after regression, want NULL (issue %s)", *got.ResolvedInVersion, got.ID)
	}

	activity := f.activity("regressed")
	if len(activity) != 1 {
		t.Fatalf("U11: found %d issue_activity 'regressed' rows for project %s, want 1", len(activity), f.ProjectName)
	}
	row := activity[0]
	if row.ActorType != "system" {
		t.Fatalf("U11: 'regressed' activity actor_type = %q, want %q", row.ActorType, "system")
	}
	// D7 (docs/memory/DECISIONS.md) records a residual, known gap: the regression-activity insert in
	// apps/processor-go/store/store.go only ever writes new_value, never old_value. A non-nil old_value
	// here would be new (and welcome) behavior, not something this test currently requires; a NULL
	// old_value is the documented, expected state, not a bug this test should flag.
	if row.NewValue == nil {
		t.Fatalf("U11: 'regressed' activity new_value is NULL, want the release-version detail")
	}
	var detail map[string]string
	if err := json.Unmarshal([]byte(*row.NewValue), &detail); err != nil {
		t.Fatalf("U11: 'regressed' activity new_value is not JSON: %v\n  raw: %s", err, *row.NewValue)
	}
	if detail["releaseVersion"] != "2.0.0" {
		t.Fatalf("U11: 'regressed' activity new_value.releaseVersion = %q, want %q", detail["releaseVersion"], "2.0.0")
	}
	if detail["previousResolvedVersion"] != "1.0.0" {
		t.Fatalf("U11: 'regressed' activity new_value.previousResolvedVersion = %q, want %q",
			detail["previousResolvedVersion"], "1.0.0")
	}
}

// ---------------------------------------------------------------------------------------------------
// U12 — resolve, then an OLDER release recurs: no regression must be recorded.
// ---------------------------------------------------------------------------------------------------

// TestU12_OlderReleaseRecurrenceDoesNotRegress is matrix row U12: the same error recurs in an OLDER
// release than the one the issue was resolved at. Asserted end state: NO regression recorded —
// regression_status stays unset ('none'), regression_count does not increment, and no 'regressed'
// issue_activity row appears. Per the plan, this is the half of the feature "nothing has ever tested" and
// "easy to get wrong by comparing versions with a plain string comparison" (a naive lexical/string compare
// of "1.9.0" vs "1.10.0" gets this backwards). Proving a negative requires a positive synchronization
// point first: wait for the occurrence count to reach 2 (proving the second event was actually
// processed), THEN assert the regression fields are still unset — never just sleep-and-hope.
func TestU12_OlderReleaseRecurrenceDoesNotRegress(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	first := f.newEvent().with(map[string]any{"release_version": "2.0.0"})
	if res := f.ingest(first); res.Status != http.StatusAccepted {
		t.Fatalf("U12: first ingest: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(1)
	f.waitForIssues(1)

	fresh := f.onlyIssue()
	f.resolve(fresh.ID, "2.0.0")

	second := f.newEvent().with(map[string]any{"release_version": "1.0.0"})
	if res := f.ingest(second); res.Status != http.StatusAccepted {
		t.Fatalf("U12: second (older-release recurrence) ingest: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	// The positive synchronization point: prove the second event was actually processed (occurrence
	// count reaches 2, and remains 2 — waitForOccurrences also guards against a duplicate arriving late)
	// before asserting on the negative (no regression fields changed).
	f.waitForOccurrences(2)
	f.waitForIssues(1) // still the same issue by fingerprint — no second issue row was created

	got := f.onlyIssue()
	if got.RegressionStatus != "none" {
		t.Fatalf("U12: regression_status = %q, want %q — an OLDER release must never regress an issue (issue %s)",
			got.RegressionStatus, "none", got.ID)
	}
	if got.RegressionCount != 0 {
		t.Fatalf("U12: regression_count = %d, want 0 (issue %s)", got.RegressionCount, got.ID)
	}
	if got.LastRegressedAt != nil {
		t.Fatalf("U12: last_regressed_at = %v, want NULL (issue %s)", *got.LastRegressedAt, got.ID)
	}
	if got.Count != 2 {
		t.Fatalf("U12: issue count = %d, want 2 — the older-release occurrence must still be recorded against the issue", got.Count)
	}

	activity := f.activity("regressed")
	if len(activity) != 0 {
		t.Fatalf("U12: found %d issue_activity 'regressed' row(s), want 0 — an older release must not be logged as a regression", len(activity))
	}
}

// ---------------------------------------------------------------------------------------------------
// U13 — issue relations link/unlink through the dashboard JSON API.
// ---------------------------------------------------------------------------------------------------

// TestU13_IssueRelationsLinkAndUnlink is matrix row U13: issue relations link/unlink through the
// dashboard JSON API. Route: apps/dashboard-web/src/routes/api/issues/[issueId]/relations/+server.ts.
// This route used to 500 on every request because the Drizzle `issueRelations` table definition omitted
// the DB's NOT NULL created_by_type/created_by columns; the schema (apps/dashboard-web/src/lib/db/schema.ts)
// has since been fixed, so the link half of this row should now be reachable — this test verifies that,
// and separately verifies whether unlink is.
func TestU13_IssueRelationsLinkAndUnlink(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	evtA := f.newEvent().with(map[string]any{"error_class": "LifecycleRelationErrorA"})
	evtB := f.newEvent().with(map[string]any{"error_class": "LifecycleRelationErrorB"})
	if res := f.ingest(evtA); res.Status != http.StatusAccepted {
		t.Fatalf("U13: ingesting source issue's error: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	if res := f.ingest(evtB); res.Status != http.StatusAccepted {
		t.Fatalf("U13: ingesting target issue's error: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(2)
	f.waitForIssues(2)

	issues := f.issues()
	if len(issues) != 2 {
		t.Fatalf("U13: expected exactly 2 issues, found %d", len(issues))
	}
	sourceID, targetID := issues[0].ID, issues[1].ID

	user := f.newDashboardUser("admin")
	path := fmt.Sprintf("/api/issues/%s/relations", sourceID)

	t.Run("link", func(t *testing.T) {
		linkRes := dashboardRequest(t, http.MethodPost, path, user, map[string]any{
			"targetIssueId": targetID,
			"relationType":  "linked_to",
		})
		if linkRes.Status != http.StatusCreated {
			t.Fatalf("U13: POST %s: got %d, want 201\n  body: %s", path, linkRes.Status, linkRes.Body)
		}

		var body map[string]any
		linkRes.JSON(t, &body)
		if body["sourceIssueId"] != sourceID {
			t.Fatalf("U13: response sourceIssueId = %v, want %s\n  body: %s", body["sourceIssueId"], sourceID, linkRes.Body)
		}
		if body["targetIssueId"] != targetID {
			t.Fatalf("U13: response targetIssueId = %v, want %s\n  body: %s", body["targetIssueId"], targetID, linkRes.Body)
		}

		rel := lifecycleOnlyRelation(t, sourceID, targetID, "linked_to")
		if rel.CreatedByType != "user" {
			t.Fatalf("U13: issue_relations.created_by_type = %q, want %q", rel.CreatedByType, "user")
		}
		if rel.CreatedBy != user.ID {
			t.Fatalf("U13: issue_relations.created_by = %q, want %q (the acting user)", rel.CreatedBy, user.ID)
		}

		linked := f.activity("linked")
		if len(linked) != 1 {
			t.Fatalf("U13: found %d issue_activity 'linked' row(s) for project %s, want 1", len(linked), f.ProjectName)
		}
		if linked[0].ActorType != "user" || linked[0].ActorID != user.ID {
			t.Fatalf("U13: 'linked' activity actor = (%q, %q), want (\"user\", %q)",
				linked[0].ActorType, linked[0].ActorID, user.ID)
		}
	})

	t.Run("unlink", func(t *testing.T) {
		if n := lifecycleRelationCount(t, sourceID, targetID, "linked_to"); n != 1 {
			t.Fatalf("U13: precondition failed — expected the link subtest's relation to still exist, found %d rows", n)
		}

		delRes := dashboardRequest(t, http.MethodDelete, path, user, map[string]any{
			"targetIssueId": targetID,
			"relationType":  "linked_to",
		})

		remaining := lifecycleRelationCount(t, sourceID, targetID, "linked_to")
		if remaining != 0 {
			t.Fatalf("U13: unlink did not remove the issue_relations row (still %d present) after DELETE %s "+
				"(status %d, body %q). apps/dashboard-web/src/routes/api/issues/[issueId]/relations/+server.ts "+
				"exports only POST — there is no DELETE handler anywhere in the dashboard routes for this "+
				"resource (confirmed: grep for 'export const DELETE' and 'relationId' across "+
				"apps/dashboard-web/src/routes and src/lib finds no issue-relations counterpart), so a relation "+
				"created through this API can never be removed through it.",
				remaining, path, delRes.Status, delRes.Body)
		}
	})
}
