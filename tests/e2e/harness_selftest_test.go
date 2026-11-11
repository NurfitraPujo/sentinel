package e2e

import "testing"

// TestHarnessReadersMatchSchema runs every harness read helper once against the live database.
//
// It exists because the first draft of this harness was written from
// apps/dashboard-web/src/lib/db/schema.ts rather than from the migrated tables, and selected four
// columns that do not exist: issues.title, issues.first_release, issues.last_release, and
// error_occurrences.message — plus issue_activity.action, whose real name is event_type. None of that
// is caught by `go build`, `go vet`, or a type checker. It surfaced as "column \"title\" does not
// exist" inside an unrelated use-case test, where it looked like that test's bug.
//
// A column rename or drop in packages/db-migrations/ will now fail HERE, once, naming the helper —
// instead of failing in whichever matrix row happens to run first and sending the next person to
// debug the wrong file. The Drizzle schema is what the dashboard believes; the migrations are what
// exists. This asserts the harness agrees with the latter.
//
// It deliberately asserts nothing about the CONTENTS. An empty result is a pass: the point is that
// every query is well-formed and every scan destination matches its column type.
func TestHarnessReadersMatchSchema(t *testing.T) {
	f := newFixture(t)

	// Reads over an empty project: each of these fails loudly on a bad column or a bad scan type.
	f.issues()
	f.occurrences()
	f.activity("regressed")
	f.issueCount()
	f.occurrenceCount()

	// Writers, on a real row. The processor owns issue creation, so insert the minimum directly rather
	// than driving the pipeline — this test must stay fast and must not depend on the async hop.
	var issueID string
	queryRow(t, &issueID,
		`INSERT INTO issues (project_id, fingerprint, message, error_class)
		 VALUES ($1, 'e2e-selftest-fp', 'harness self-test', 'SelfTestError')
		 RETURNING id::text`, f.ProjectID)

	f.setIssueStatus(issueID, "ignored")
	f.resolve(issueID, "1.2.3")

	got := f.issues()
	if len(got) != 1 {
		t.Fatalf("expected the 1 issue just inserted, got %d", len(got))
	}
	if got[0].Status != "resolved" {
		t.Errorf("resolve() left status = %q, want \"resolved\"", got[0].Status)
	}
	if got[0].ResolvedInVersion == nil || *got[0].ResolvedInVersion != "1.2.3" {
		t.Errorf("resolve() left resolved_in_version = %v, want \"1.2.3\" — regression detection compares "+
			"against this column, so a test that skips it proves nothing", got[0].ResolvedInVersion)
	}
	if got[0].Message != "harness self-test" {
		t.Errorf("issues().Message = %q, want the inserted message", got[0].Message)
	}

	// occurrences() was already exercised above against an EMPTY project (a query that returns zero
	// rows still proves every column NAME and type resolves, but never actually runs Scan on the new
	// EventID column — a broken scan destination for a NULLABLE column would sail through that). Insert
	// one occurrence with a real event_id and one with none, and confirm both round-trip through the
	// reader (docs/plans/IDEMPOTENCY_PLAN.md W3/F-TP-2).
	queryRow(t, new(string),
		`INSERT INTO error_occurrences (issue_id, environment, platform, event_id)
		 VALUES ($1, 'test', 'go', 'e2e-selftest-event-id') RETURNING id::text`, issueID)
	queryRow(t, new(string),
		`INSERT INTO error_occurrences (issue_id, environment, platform, event_id)
		 VALUES ($1, 'test', 'go', NULL) RETURNING id::text`, issueID)

	occs := f.occurrences()
	if len(occs) != 2 {
		t.Fatalf("expected the 2 occurrences just inserted, got %d", len(occs))
	}
	var sawWithID, sawWithoutID bool
	for _, o := range occs {
		switch {
		case o.EventID != nil && *o.EventID == "e2e-selftest-event-id":
			sawWithID = true
		case o.EventID == nil:
			sawWithoutID = true
		default:
			t.Errorf("occurrences().EventID = %v, want either nil or \"e2e-selftest-event-id\"", o.EventID)
		}
	}
	if !sawWithID {
		t.Error("occurrences() did not surface the inserted non-NULL event_id")
	}
	if !sawWithoutID {
		t.Error("occurrences() did not surface the inserted NULL event_id as a nil pointer")
	}
}

// TestHarnessDashboardSessionIsAcceptedAsSignedIn proves the seeded Auth.js session is a real one.
//
// The harness signs in by inserting a `session` row and presenting its token as the
// `authjs.session-token` cookie. That is only a genuine sign-in while the adapter is configured and the
// session strategy is therefore "database" — if it ever switches to JWT, the cookie stops meaning
// anything, every authenticated request silently becomes anonymous, and every authorization test
// downgrades to "unauthenticated is also denied" while still passing.
//
// So: assert an authenticated request and an unauthenticated one get DIFFERENT answers. That is the
// weakest claim that still catches the failure, and it does not depend on any particular route's
// permission model.
func TestHarnessDashboardSessionIsAcceptedAsSignedIn(t *testing.T) {
	f := newFixture(t)
	owner := f.newDashboardUser("owner")

	path := "/api/organizations/" + f.OrgID + "/keys"
	anon := dashboardRequest(t, "GET", path, nil, nil)
	auth := dashboardRequest(t, "GET", path, owner, nil)

	if anon.Status == auth.Status {
		body := auth.Body
		if len(body) > 400 {
			body = body[:400] + "…"
		}
		t.Fatalf("GET %s returned %d both with and without a session cookie — the seeded session is not "+
			"being recognized, so every authorization assertion in this suite is vacuous.\n"+
			"  Check that the Auth.js session strategy is still \"database\" and the cookie name still "+
			"matches (authjs.session-token over plain HTTP).\n  authenticated body: %s",
			path, anon.Status, body)
	}
}
