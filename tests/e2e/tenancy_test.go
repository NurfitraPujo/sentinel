package e2e

// This file drives U18 and U21 of the P7 use-case matrix (docs/plans/E2E_RECOVERY_PLAN.md, "## P7 — The
// E2E proof harness").
//
// U18 is defect S6's regression test (docs/memory/VERIFIED_STATE.md): the ingestor used to resolve a
// request's target project from the body's project_key with no organization scoping at all, so any
// valid ingest key could write into ANY tenant's project simply by naming it. The fix lives in
// apps/ingestor-go/main.go's applyAuthenticatedScope and apps/ingestor-go/auth/apikey.go's
// ResolveProjectInOrg: every resolution of a client-supplied project name is scoped to the
// authenticated credential's own organization_id, never global. The plan explicitly calls out that this
// behavior was "believed correct" but had no automated test — that is what this file fixes.
//
// applyAuthenticatedScope (apps/ingestor-go/main.go, ~line 328) documents three shapes:
//   - project-scoped key + body naming a DIFFERENT project -> 403 (never silently rewritten)
//   - org-wide key + X-Project-Key header resolved -> header wins; body's project_key is ignored
//     entirely, even if it names something nonsensical or another tenant's project
//   - org-wide key + no header -> body's project_key is resolved, scoped to the key's own org; a name
//     belonging to (or absent from) another organization is indistinguishable from "not found" -> 403
//
// U21 exercises packages/db-migrations' `UNIQUE (organization_id, name)` index on `projects`: project
// names are unique per-organization, not globally, so two organizations may each legitimately have a
// project of the same name with no cross-visibility between them.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------------------------------
// U18 — cross-tenant write rejection
// ---------------------------------------------------------------------------------------------------

// TestU18_ProjectScopedKeyCannotWriteAcrossTenant proves the core S6 regression: a project-scoped key
// for tenant A, presented with a body naming tenant B's project, must 403 — and must land zero rows in
// B. "Zero rows" is a negative, so it is proven by synchronizing on a POSITIVE control event landing in
// B afterward (via B's own key) rather than by sleeping and hoping: if the malicious write had gotten
// through, B's occurrence count would be 2 (the attack + the control) instead of 1, and
// waitForOccurrences's own overshoot check (a 1s hold after reaching the target count) would catch a
// late-arriving duplicate too.
func TestU18_ProjectScopedKeyCannotWriteAcrossTenant(t *testing.T) {
	requireStack(t)
	a := newFixture(t)
	b := newFixture(t)

	// Sanity: A's own key legitimately writes to A first, proving the pipeline itself is live before we
	// draw any conclusion from what does NOT land in B.
	okRes := a.ingest(a.newEvent())
	if okRes.Status != http.StatusAccepted {
		t.Fatalf("control ingest into A with A's own key: want 202, got %d\n  body: %s", okRes.Status, okRes.Body)
	}
	a.waitForOccurrences(1)

	// The attack: A's project-scoped key, body names B's project.
	attack := a.ingest(a.newEvent().with(map[string]any{"project_key": b.ProjectName}))
	if attack.Status != http.StatusForbidden {
		t.Fatalf("SECURITY REGRESSION (S6): project-scoped key for tenant A, body naming tenant B's "+
			"project %q: want 403, got %d\n  body: %s", b.ProjectName, attack.Status, attack.Body)
	}

	// Positive control in B, sent AFTER the attack attempt: if the attack had silently written into B,
	// B would already be at 1 occurrence and this control would push it to 2. waitForOccurrences(1)
	// demands EXACTLY 1 and holds briefly to catch a late-arriving second row.
	controlRes := b.ingest(b.newEvent())
	if controlRes.Status != http.StatusAccepted {
		t.Fatalf("control ingest into B with B's own key: want 202, got %d\n  body: %s", controlRes.Status, controlRes.Body)
	}
	b.waitForOccurrences(1)
}

// TestU18_OrgWideKeyCannotResolveOutsideOwnOrg proves the organization-wide key shape of U18: a key
// with NULL project_id (organization-wide) naming a project that belongs to a DIFFERENT organization,
// via the body's project_key with no X-Project-Key header, must also 403 — never a cross-tenant write.
// It also proves the key is not simply broken: the same credential naming a project inside its OWN
// organization must succeed.
func TestU18_OrgWideKeyCannotResolveOutsideOwnOrg(t *testing.T) {
	requireStack(t)
	a := newFixture(t)
	b := newFixture(t)

	orgWideSecret := newSecret(t)
	a.addKey(keySpec{Name: "u18-org-wide", Secret: orgWideSecret, ProjectScoped: false})

	// Attack: A's org-wide key, no X-Project-Key header, body names B's project (a different org).
	attack := a.ingest(a.newEvent().with(map[string]any{"project_key": b.ProjectName}), ingestOpts{APIKey: orgWideSecret})
	if attack.Status != http.StatusForbidden {
		t.Fatalf("SECURITY REGRESSION (S6): org-wide key for tenant A's organization, body naming tenant "+
			"B's project %q (a different organization): want 403, got %d\n  body: %s",
			b.ProjectName, attack.Status, attack.Body)
	}

	// Positive control, sent AFTER the attack: B must still show zero rows from it.
	controlB := b.ingest(b.newEvent())
	if controlB.Status != http.StatusAccepted {
		t.Fatalf("control ingest into B with B's own key: want 202, got %d\n  body: %s", controlB.Status, controlB.Body)
	}
	b.waitForOccurrences(1)

	// The org-wide key is not broken outright: naming a project WITHIN its own organization must work.
	ownOrg := a.ingest(a.newEvent(), ingestOpts{APIKey: orgWideSecret})
	if ownOrg.Status != http.StatusAccepted {
		t.Fatalf("org-wide key naming its OWN organization's project: want 202, got %d\n  body: %s", ownOrg.Status, ownOrg.Body)
	}
	a.waitForOccurrences(1)
}

// TestU18_OrgWideKeyHeaderWinsOverBody documents the third shape applyAuthenticatedScope permits: for
// an org-wide key, X-Project-Key (resolved by the auth middleware, scoped to the key's own
// organization) is authoritative and the body's project_key is not consulted at all — even when the
// body names a different tenant's project. This is not a tenancy hole (the header was itself resolved
// within the authenticated organization); it is the documented precedence rule, verified here so a
// future change to that precedence does not go unnoticed.
func TestU18_OrgWideKeyHeaderWinsOverBody(t *testing.T) {
	requireStack(t)
	a := newFixture(t)
	b := newFixture(t)

	orgWideSecret := newSecret(t)
	a.addKey(keySpec{Name: "u18-header-wins", Secret: orgWideSecret, ProjectScoped: false})

	// Header names A's own project (legitimate); body names B's project (should be ignored outright).
	ev := a.newEvent().with(map[string]any{"project_key": b.ProjectName})
	res := a.ingest(ev, ingestOpts{APIKey: orgWideSecret, ProjectKeyHeader: a.ProjectName})
	if res.Status != http.StatusAccepted {
		t.Fatalf("org-wide key, X-Project-Key naming own org's project, body naming a foreign project: "+
			"want 202 (header wins, body ignored), got %d\n  body: %s", res.Status, res.Body)
	}
	a.waitForOccurrences(1)

	// B must never see it: the body's mention of B's project must have been ignored, not resolved.
	if got := b.occurrenceCount(); got != 0 {
		t.Fatalf("body's project_key (naming B) was not ignored: %d occurrence(s) landed in B", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// U21 — same project name, two organizations, no cross-visibility
// ---------------------------------------------------------------------------------------------------

// tenancyTenant is a hand-seeded org+project+org-wide-key tuple for U21, distinct from `fixture`
// because newFixture always derives a unique-per-test project name; U21 specifically needs two
// different organizations sharing the SAME project name, which `projects`' `UNIQUE (organization_id,
// name)` index (not a global unique) is what makes legal.
type tenancyTenant struct {
	OrgID       string
	ProjectID   string
	ProjectName string
	// APIKey is an organization-wide ingest key's plaintext secret (project_id IS NULL), so the target
	// project is selected by name via X-Project-Key — the same mechanism U18 exercises.
	APIKey string
}

// tenancySeedTenant seeds one organization and one project named projectName within it, plus an active
// organization-wide ingest key, and registers cleanup scoped to exactly the rows it created. Two calls
// with the same projectName produce two organizations that legitimately share a project name.
func tenancySeedTenant(t *testing.T, projectName string) *tenancyTenant {
	t.Helper()
	requireStack(t)

	suffix := uniqueSuffix()
	orgName := "e2e-u21-org-" + suffix
	orgSlug := "e2e-u21-org-" + suffix

	ctx := context.Background()
	tt := &tenancyTenant{ProjectName: projectName, APIKey: newSecret(t)}

	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ($1, $2) RETURNING id::text`,
		orgName, orgSlug,
	).Scan(&tt.OrgID); err != nil {
		t.Fatalf("seeding organization %q: %v", orgName, err)
	}

	// projects.api_key/api_key_hash are legacy NOT NULL UNIQUE columns predating project_api_keys; they
	// are not read by the auth path under test here (apps/ingestor-go/auth uses project_api_keys), so a
	// throwaway unique value satisfies the schema without meaning anything.
	legacyKey := newSecret(t)
	if err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash, organization_id)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'), $4)
		 RETURNING id::text`,
		projectName, legacyKey, legacyKey, tt.OrgID,
	).Scan(&tt.ProjectID); err != nil {
		t.Fatalf("seeding project %q in org %s: %v", projectName, tt.OrgID, err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO project_api_keys
		   (project_id, organization_id, key_hash, key_prefix, name, scope, status, rate_limit_rpm, created_by)
		 VALUES (NULL, $1, encode(digest($2::bytea, 'sha256'), 'hex'), $3, $4, 'ingest', 'active', 100000, 'e2e-harness')`,
		tt.OrgID, tt.APIKey, keyPrefix(tt.APIKey), "u21-org-wide-"+suffix,
	); err != nil {
		t.Fatalf("seeding org-wide API key for org %s: %v", tt.OrgID, err)
	}

	t.Cleanup(func() {
		exec(ctx, `DELETE FROM error_search_index WHERE occurrence_id IN (
		             SELECT eo.id FROM error_occurrences eo
		             JOIN issues i ON i.id = eo.issue_id WHERE i.project_id = $1)`, tt.ProjectID)
		exec(ctx, `DELETE FROM error_occurrences WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`, tt.ProjectID)
		exec(ctx, `DELETE FROM issue_activity WHERE issue_id IN (SELECT id FROM issues WHERE project_id = $1)`, tt.ProjectID)
		exec(ctx, `DELETE FROM issue_relations WHERE source_issue_id IN (SELECT id FROM issues WHERE project_id = $1)
		             OR target_issue_id IN (SELECT id FROM issues WHERE project_id = $1)`, tt.ProjectID)
		exec(ctx, `DELETE FROM issues WHERE project_id = $1`, tt.ProjectID)
		exec(ctx, `DELETE FROM alert_configs WHERE project_id = $1`, tt.ProjectID)
		exec(ctx, `DELETE FROM project_members WHERE project_id = $1`, tt.ProjectID)
		exec(ctx, `DELETE FROM project_api_keys WHERE organization_id = $1`, tt.OrgID)
		exec(ctx, `DELETE FROM projects WHERE organization_id = $1`, tt.OrgID)
		exec(ctx, `DELETE FROM organization_invitations WHERE organization_id = $1`, tt.OrgID)
		exec(ctx, `DELETE FROM organization_members WHERE organization_id = $1`, tt.OrgID)
		exec(ctx, `DELETE FROM organizations WHERE id = $1`, tt.OrgID)
	})

	return tt
}

// tenancyNewEvent builds a canonical valid event naming projectKey, independent of any *fixture.
func tenancyNewEvent(projectKey string) event {
	return event{
		"project_key": projectKey,
		"platform":    "go",
		"environment": "e2e",
		"message":     "e2e u21 canonical error",
		"error_class": "E2EU21CanonicalError",
		"trace_id":    "trace-" + uniqueSuffix(),
		"span_id":     "span-" + uniqueSuffix(),
		"stacktrace": []map[string]any{
			{"file": "main.go", "line": 42, "function": "main", "in_app": true},
		},
		"metadata":        map[string]any{},
		"timestamp":       time.Now().UTC().Format(time.RFC3339Nano),
		"trace_flags":     0,
		"release_version": "1.0.0",
	}
}

// tenancyIngest posts an event to the real /ingest endpoint using an arbitrary API key and
// X-Project-Key header, independent of any *fixture (tenancyTenant is not a *fixture).
func tenancyIngest(t *testing.T, apiKey, projectKeyHeader string, ev event) ingestResult {
	t.Helper()

	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshalling tenancy ingest body: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.IngestorURL+"/ingest", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("building tenancy ingest request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	if projectKeyHeader != "" {
		req.Header.Set("X-Project-Key", projectKeyHeader)
	}

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /ingest: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return ingestResult{Status: resp.StatusCode, Body: string(raw), Header: resp.Header}
}

// tenancyOccurrenceCount counts error_occurrences scoped to a raw project id, mirroring
// fixture.occurrenceCount but usable for a tenancyTenant.
func tenancyOccurrenceCount(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM error_occurrences eo
		 JOIN issues i ON i.id = eo.issue_id
		 WHERE i.project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatalf("counting occurrences for project %s: %v", projectID, err)
	}
	return n
}

// tenancyWaitForOccurrences waits until projectID has exactly want occurrences, mirroring
// fixture.waitForOccurrences's overshoot guard against a late duplicate.
func tenancyWaitForOccurrences(t *testing.T, projectID string, want int) {
	t.Helper()
	waitFor(t, asyncTimeout, fmt.Sprintf("%d occurrences in project %s", want, projectID), func() (bool, string) {
		got := tenancyOccurrenceCount(t, projectID)
		return got == want, fmt.Sprintf("%d occurrences", got)
	})
	time.Sleep(1 * time.Second)
	if got := tenancyOccurrenceCount(t, projectID); got != want {
		t.Fatalf("occurrence count for project %s moved after reaching %d: now %d — duplicate delivery", projectID, want, got)
	}
}

// TestU21_SameProjectNameAcrossOrgsNoCrossVisibility proves row U21: two organizations each name a
// project identically. `projects`' UNIQUE (organization_id, name) index (not a global unique) permits
// this; each organization's own org-wide key must resolve to its OWN project when naming it, and an
// event ingested via one organization's key must never be visible under the other organization's
// project of the same name.
func TestU21_SameProjectNameAcrossOrgsNoCrossVisibility(t *testing.T) {
	requireStack(t)

	sharedName := "e2e-u21-shared-" + uniqueSuffix()
	orgA := tenancySeedTenant(t, sharedName)
	orgB := tenancySeedTenant(t, sharedName)

	if orgA.ProjectID == orgB.ProjectID {
		t.Fatalf("test setup bug: orgA and orgB resolved to the same project id %s", orgA.ProjectID)
	}

	resA := tenancyIngest(t, orgA.APIKey, sharedName, tenancyNewEvent(sharedName))
	if resA.Status != http.StatusAccepted {
		t.Fatalf("ingest into org A (via its own org-wide key naming %q): want 202, got %d\n  body: %s",
			sharedName, resA.Status, resA.Body)
	}
	tenancyWaitForOccurrences(t, orgA.ProjectID, 1)

	resB := tenancyIngest(t, orgB.APIKey, sharedName, tenancyNewEvent(sharedName))
	if resB.Status != http.StatusAccepted {
		t.Fatalf("ingest into org B (via its own org-wide key naming %q): want 202, got %d\n  body: %s",
			sharedName, resB.Status, resB.Body)
	}
	tenancyWaitForOccurrences(t, orgB.ProjectID, 1)

	// Each org's project must show EXACTLY its own event — not 2 (which would mean the two orgs'
	// same-named projects were being conflated into one), and not the other org's data.
	if got := tenancyOccurrenceCount(t, orgA.ProjectID); got != 1 {
		t.Fatalf("org A's project %q: want exactly 1 occurrence, got %d — cross-tenant visibility", sharedName, got)
	}
	if got := tenancyOccurrenceCount(t, orgB.ProjectID); got != 1 {
		t.Fatalf("org B's project %q: want exactly 1 occurrence, got %d — cross-tenant visibility", sharedName, got)
	}

	var issueOrgA, issueOrgB int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM issues WHERE project_id = $1`, orgA.ProjectID).Scan(&issueOrgA); err != nil {
		t.Fatalf("counting issues for org A's project: %v", err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM issues WHERE project_id = $1`, orgB.ProjectID).Scan(&issueOrgB); err != nil {
		t.Fatalf("counting issues for org B's project: %v", err)
	}
	if issueOrgA != 1 || issueOrgB != 1 {
		t.Fatalf("expected exactly 1 issue in each org's same-named project, got orgA=%d orgB=%d", issueOrgA, issueOrgB)
	}
}
