// Package e2e — this file covers matrix rows U11, U12, U13 (docs/plans/E2E_RECOVERY_PLAN.md, "P7 — The
// E2E proof harness") and sentinel-releases-snooze's U12b (resolve in `next`, contract §11's last row):
// issue-lifecycle regression detection and issue relations, driven against the real compose stack via
// the harness in main_test.go/harness_test.go.
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
	"time"
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
// point first: wait for the occurrence count to reach 3 (proving the third event was actually
// processed), THEN assert the regression fields are still unset — never just sleep-and-hope.
//
// RE-SEQUENCED for sentinel-releases-snooze (contract §6, decisions 1 and 14). As first written this
// row ingested 2.0.0, resolved in 2.0.0, and then let 1.0.0 arrive — which encoded SEMVER ordering:
// "1.0.0 is older than 2.0.0 because the numbers say so". Under the releases entity "older" is decided
// by `releases.first_seen`, never by parsing the version (decision 1), and a version this project has
// never seen before is the NEWEST by definition (decision 14: a first sighting is newest). So the old
// sequence now regresses the issue — 1.0.0 had never been seen — and the row would fail for a reason
// that is not the bug it guards. The sequence below makes BOTH orderings agree: 1.0.0 is ingested
// FIRST (first seen earliest), then 2.0.0 (first seen later), the issue is resolved in 2.0.0, and
// 1.0.0 recurs — older by first_seen AND older by semver — so a regression here would be wrong under
// either rule, which is exactly what the row exists to catch.
func TestU12_OlderReleaseRecurrenceDoesNotRegress(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	// 1.0.0 first: this project's earliest-seen release.
	first := f.newEvent().with(map[string]any{"release_version": "1.0.0"})
	if res := f.ingest(first); res.Status != http.StatusAccepted {
		t.Fatalf("U12: first ingest (1.0.0): got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(1)
	f.waitForIssues(1)

	// 2.0.0 second: first seen AFTER 1.0.0, so it is the newer release by first_seen as well as by number.
	second := f.newEvent().with(map[string]any{"release_version": "2.0.0"})
	if res := f.ingest(second); res.Status != http.StatusAccepted {
		t.Fatalf("U12: second ingest (2.0.0): got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(2)
	f.waitForIssues(1)

	fresh := f.onlyIssue()
	if fresh.RegressionStatus != "none" || fresh.RegressionCount != 0 {
		t.Fatalf("U12: issue already looks regressed before it was ever resolved: status=%q count=%d",
			fresh.RegressionStatus, fresh.RegressionCount)
	}
	f.resolve(fresh.ID, "2.0.0")

	// 1.0.0 recurs: older than the resolving release under both orderings.
	third := f.newEvent().with(map[string]any{"release_version": "1.0.0"})
	if res := f.ingest(third); res.Status != http.StatusAccepted {
		t.Fatalf("U12: third (older-release recurrence) ingest: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	// The positive synchronization point: prove the third event was actually processed (occurrence
	// count reaches 3, and remains 3 — waitForOccurrences also guards against a duplicate arriving late)
	// before asserting on the negative (no regression fields changed).
	f.waitForOccurrences(3)
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
	if got.Status != "resolved" {
		t.Fatalf("U12: status = %q, want %q — a non-regressing recurrence must leave the resolution in place", got.Status, "resolved")
	}
	if got.ResolvedInVersion == nil || *got.ResolvedInVersion != "2.0.0" {
		t.Fatalf("U12: resolved_in_version = %v, want %q — untouched by the older recurrence", got.ResolvedInVersion, "2.0.0")
	}
	if got.Count != 3 {
		t.Fatalf("U12: issue count = %d, want 3 — the older-release occurrence must still be recorded against the issue", got.Count)
	}

	activity := f.activity("regressed")
	if len(activity) != 0 {
		t.Fatalf("U12: found %d issue_activity 'regressed' row(s), want 0 — an older release must not be logged as a regression", len(activity))
	}
}

// ---------------------------------------------------------------------------------------------------
// U12b — resolve in `next`, then an UNRELATED issue's first occurrence on a new SHA names the release.
// ---------------------------------------------------------------------------------------------------

// TestU12b_ResolvedInNextIsRewrittenByTheNextRelease is sentinel-releases-snooze's e2e row (contract
// §11's last row; flows A7 and I11; decisions 15, 21): an issue resolved with the reserved value `next`
// reads the concrete version of the FIRST release seen after its resolution — a release carried by an
// UNRELATED fingerprint, since the resolved issue itself never recurred — and it is NOT a regression.
//
// The resolution goes through the real agent route (`PATCH /api/agent/issues/{id}/status` with
// `resolved_in_version: "next"` — what `sentinel status <id> resolved --resolved-in next` sends, A7),
// so the row also proves the literal is accepted and stored as-is.
//
// Two synchronization points, and the second is deliberately a POLL rather than a read: the
// occurrence landing (waitForOccurrences) proves the STORE committed the unrelated event, but the
// `next` rewrite is a separate post-commit statement — best-effort, its own short transaction, after
// StoreEvent returns — so the occurrence's arrival proves the store, not the rewrite. The row polls
// `GET /api/agent/issues/{id}` until `resolvedInVersion` equals the SHA or 10 s elapse, and a loaded
// runner must not read as a bug.
func TestU12b_ResolvedInNextIsRewrittenByTheNextRelease(t *testing.T) {
	requireStack(t)
	f := newFixture(t)
	admin := f.newDashboardUser("admin")
	_, bearer := f.seedAgent(t, admin, "next-release-agent")

	first := f.newEvent().with(map[string]any{"release_version": "1.0.0"})
	if res := f.ingest(first); res.Status != http.StatusAccepted {
		t.Fatalf("U12b: first ingest: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(1)
	f.waitForIssues(1)
	fresh := f.onlyIssue()

	// A7: resolve in `next` through the agent route.
	resolveRes := agentRequest(t, http.MethodPatch, "/api/agent/issues/"+fresh.ID+"/status", bearer, map[string]any{
		"status":              "resolved",
		"resolved_in_version": "next",
	})
	if resolveRes.Status != http.StatusOK {
		t.Fatalf("U12b: PATCH resolve in next: got %d, want 200\n  body: %s", resolveRes.Status, resolveRes.Body)
	}
	resolved := f.onlyIssue()
	if resolved.Status != "resolved" || resolved.ResolvedInVersion == nil || *resolved.ResolvedInVersion != "next" {
		t.Fatalf("U12b: after the PATCH the row must read status=resolved resolved_in_version=next, got status=%q resolved_in_version=%v",
			resolved.Status, resolved.ResolvedInVersion)
	}
	if resolved.ResolvedAt == nil {
		t.Fatalf("U12b: resolved_at is NULL after the PATCH — the rewrite compares against it (decision 27), so a `next` with no resolved_at could never be rewritten")
	}

	// An UNRELATED fingerprint's first occurrence on a never-seen SHA: the release's first sighting.
	const sha = "9f8e7d6"
	unrelated := f.newEvent().with(map[string]any{
		"error_class":     "LifecycleNextReleaseUnrelatedError",
		"message":         "e2e unrelated error carrying the next release",
		"release_version": sha,
	})
	if res := f.ingest(unrelated); res.Status != http.StatusAccepted {
		t.Fatalf("U12b: unrelated ingest on %s: got %d, want 202\n  body: %s", sha, res.Status, res.Body)
	}
	f.waitForOccurrences(2)
	f.waitForIssues(2)

	// The rewrite is post-commit and best-effort: poll the agent read for up to 10 s.
	path := "/api/agent/issues/" + fresh.ID
	var last string
	waitFor(t, 10*time.Second, fmt.Sprintf("GET %s to report resolvedInVersion = %q (the post-commit next rewrite)", path, sha), func() (bool, string) {
		res := agentRequest(t, http.MethodGet, path, bearer, nil)
		if res.Status != http.StatusOK {
			return false, fmt.Sprintf("GET %s: %d %s", path, res.Status, res.Body)
		}
		var body struct {
			Issue struct {
				Status            string  `json:"status"`
				ResolvedInVersion *string `json:"resolvedInVersion"`
			} `json:"issue"`
		}
		if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
			return false, fmt.Sprintf("GET %s: body is not JSON: %v\n  raw: %s", path, err, res.Body)
		}
		if body.Issue.ResolvedInVersion == nil {
			last = "<nil>"
		} else {
			last = *body.Issue.ResolvedInVersion
		}
		return body.Issue.Status == "resolved" && last == sha, fmt.Sprintf("status=%q resolvedInVersion=%q", body.Issue.Status, last)
	})

	got := f.issues()[0] // oldest first: the resolved issue
	if got.ID != fresh.ID {
		t.Fatalf("U12b: expected the resolved issue %s first (oldest), got %s", fresh.ID, got.ID)
	}
	if got.Status != "resolved" || got.ResolvedInVersion == nil || *got.ResolvedInVersion != sha {
		t.Fatalf("U12b: the row must read status=resolved resolved_in_version=%q, got status=%q resolved_in_version=%v", sha, got.Status, got.ResolvedInVersion)
	}
	if got.RegressionStatus != "none" || got.RegressionCount != 0 || got.LastRegressedAt != nil {
		t.Fatalf("U12b: the rewrite must not regress the issue: regression_status=%q regression_count=%d last_regressed_at=%v",
			got.RegressionStatus, got.RegressionCount, got.LastRegressedAt)
	}
	if got.Count != 1 {
		t.Fatalf("U12b: issue count = %d, want 1 — the unrelated fingerprint's occurrence must not be counted against this issue", got.Count)
	}
	if activity := f.activity("regressed"); len(activity) != 0 {
		t.Fatalf("U12b: found %d issue_activity 'regressed' row(s), want 0 — naming the release is not a regression (decision 8)", len(activity))
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
