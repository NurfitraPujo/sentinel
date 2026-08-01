// Package e2e — this file covers matrix rows U22, U23, U24, U25, U31, U32 (docs/plans/E2E_RECOVERY_PLAN.md
// "P7 — The E2E proof harness"): dashboard invitations/RBAC, magic-link and Google sign-in, issue
// list/search tenant scoping, and the cron retention endpoint.
//
// Note on the read helpers used below: this file predates the fix to harness_test.go's f.issues() /
// f.onlyIssue() / f.occurrences(), which originally selected columns that do not exist in the migrated
// schema (issues.title/first_release/last_release, error_occurrences.message/timestamp) and failed with
// Postgres 42703. Those helpers are correct now, but this file keeps its own scoped queries
// (dashQueryIssues, dashOccurrenceIDsForIssue, ...) because they select exactly the fields these rows
// assert on. New tests should prefer the shared harness readers.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------------------------------
// Shared helpers (area-prefixed "dash" so they cannot collide with another agent's file)
// ---------------------------------------------------------------------------------------------------

// dashHTTPResult is a generic (status, body) pair for the raw HTTP calls in this file that
// dashboardRequest cannot make (custom headers, form encoding, redirect inspection).
type dashHTTPResult struct {
	Status int
	Body   string
}

// dashIssueRow mirrors the REAL `issues` columns (see the package comment: harness_test.go's issueRow
// does not).
type dashIssueRow struct {
	ID          string
	ProjectID   string
	Fingerprint string
	Message     string
	ErrorClass  string
	Status      string
	Count       int
}

// dashQueryIssues reads every issue for a project directly, using only columns confirmed live
// (packages/db-migrations/migrations/1716508800_init.sql + 1721900000_...sql).
func dashQueryIssues(t *testing.T, projectID string) []dashIssueRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, project_id::text, fingerprint, message, error_class, status, count
		   FROM issues WHERE project_id = $1 ORDER BY first_seen`, projectID)
	if err != nil {
		t.Fatalf("querying issues for project %s: %v", projectID, err)
	}
	defer rows.Close()
	var out []dashIssueRow
	for rows.Next() {
		var r dashIssueRow
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Fingerprint, &r.Message, &r.ErrorClass, &r.Status, &r.Count); err != nil {
			t.Fatalf("scanning issue row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating issues: %v", err)
	}
	return out
}

// dashOccurrenceIDsForIssue returns this issue's occurrence ids, oldest first.
func dashOccurrenceIDsForIssue(t *testing.T, issueID string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text FROM error_occurrences WHERE issue_id = $1 ORDER BY created_at`, issueID)
	if err != nil {
		t.Fatalf("querying occurrences for issue %s: %v", issueID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning occurrence id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// dashBackdateOccurrence rewrites one occurrence's created_at to `age` in the past. Scoped to a single
// id this test's own fixture owns — never an unscoped UPDATE.
func dashBackdateOccurrence(t *testing.T, occurrenceID string, age time.Duration) {
	t.Helper()
	tag, err := pool.Exec(context.Background(),
		`UPDATE error_occurrences SET created_at = now() - make_interval(secs => $2) WHERE id = $1`,
		occurrenceID, age.Seconds())
	if err != nil {
		t.Fatalf("backdating occurrence %s: %v", occurrenceID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("backdating occurrence %s affected %d rows, want 1", occurrenceID, tag.RowsAffected())
	}
}

func dashOccurrenceExists(t *testing.T, occurrenceID string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM error_occurrences WHERE id = $1)`, occurrenceID).Scan(&exists); err != nil {
		t.Fatalf("checking occurrence %s existence: %v", occurrenceID, err)
	}
	return exists
}

func dashIssueExists(t *testing.T, issueID string) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM issues WHERE id = $1)`, issueID).Scan(&exists); err != nil {
		t.Fatalf("checking issue %s existence: %v", issueID, err)
	}
	return exists
}

// dashSeedProjectMember inserts a project_members row directly — newDashboardUser only seeds
// organization_members, and U23's project-scoped half needs the other table.
func dashSeedProjectMember(t *testing.T, projectID, userID, role string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO project_members (project_id, user_id, role) VALUES ($1, $2, $3)`,
		projectID, userID, role); err != nil {
		t.Fatalf("seeding project_members role %q: %v", role, err)
	}
	t.Cleanup(func() {
		exec(context.Background(), `DELETE FROM project_members WHERE project_id = $1 AND user_id = $2`, projectID, userID)
	})
}

// dashCronRequest calls the retention cron route directly — dashboardRequest has no way to set an
// arbitrary header. present=false omits the header entirely (as opposed to sending it empty).
func dashCronRequest(t *testing.T, secret string, present bool) dashHTTPResult {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, cfg.DashboardURL+"/api/cron/retention", nil)
	if err != nil {
		t.Fatalf("building cron retention request: %v", err)
	}
	if present {
		req.Header.Set("x-cron-secret", secret)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /api/cron/retention: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return dashHTTPResult{Status: resp.StatusCode, Body: string(raw)}
}

// dashRandomToken returns a fresh random hex token, standing in for the raw token Auth.js would put in
// a magic-link URL.
func dashRandomToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("generating a random token: %v", err)
	}
	return hex.EncodeToString(buf)
}

// dashCallbackEmail drives Auth.js's built-in email callback (GET /auth/callback/email) exactly as a
// clicked email link would, without following the redirect it issues on success. Returns the redirect
// Location, every Set-Cookie value by name, and the status.
func dashCallbackEmail(t *testing.T, email, rawToken, callbackURL string) (location string, cookies map[string]string, status int) {
	t.Helper()
	q := url.Values{}
	q.Set("token", rawToken)
	q.Set("email", email)
	q.Set("callbackUrl", callbackURL)
	req, err := http.NewRequest(http.MethodGet, cfg.DashboardURL+"/auth/callback/email?"+q.Encode(), nil)
	if err != nil {
		t.Fatalf("building callback request: %v", err)
	}
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /auth/callback/email: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	cookies = map[string]string{}
	for _, c := range resp.Cookies() {
		cookies[c.Name] = c.Value
	}
	return resp.Header.Get("Location"), cookies, resp.StatusCode
}

// dashRepoPath resolves a path relative to the repository root. Tests in this package always run with
// tests/e2e as the working directory.
func dashRepoPath(rel string) string {
	return filepath.Join("..", "..", rel)
}

// dashIssueJSON mirrors the JSON shape GET /api/projects/[projectId]/issues returns: Drizzle's
// `.select()` serializes using the TS property names in schema.ts (camelCase).
type dashIssueJSON struct {
	ID         string `json:"id"`
	ProjectID  string `json:"projectId"`
	ErrorClass string `json:"errorClass"`
	Message    string `json:"message"`
	Status     string `json:"status"`
	Count      int    `json:"count"`
}

// ---------------------------------------------------------------------------------------------------
// U22 — invite -> accept -> role applies
// ---------------------------------------------------------------------------------------------------

// TestU22_InviteCreationRBACAndAcceptanceWall proves U22's reachable half — creating an invitation
// through POST /api/organizations/[orgId]/invitations, gated to owner/admin — end to end against the
// real route and a real row in organization_invitations. It also nails down exactly where the row's
// second half (accept -> membership at that role) stops being reachable: there is no HTTP route
// anywhere in the app that consumes an invitation token. This was confirmed by source inspection
// (`grep -rn "accept" apps/dashboard-web/src` matches nothing but an unrelated comment) and is reconfirmed
// live below by probing every plausible accept URL and observing 404 — not 401/403/500, which would mean
// a route exists but rejects the request; 404 means SvelteKit found no route at all. This is stronger
// than "needs a browser": a browser has nothing to click through to either, because the acceptance
// endpoint itself was never built.
func TestU22_InviteCreationRBACAndAcceptanceWall(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	owner := f.newDashboardUser("owner")
	viewer := f.newDashboardUser("viewer")
	outsider := f.newDashboardUser("") // authenticated, but zero membership rows anywhere

	path := fmt.Sprintf("/api/organizations/%s/invitations", f.OrgID)

	rbacCases := []struct {
		name string
		user *dashboardUser
		want int
	}{
		{"unauthenticated", nil, http.StatusUnauthorized},
		{"authenticated with no org membership", outsider, http.StatusForbidden},
		{"viewer (not owner/admin)", viewer, http.StatusForbidden},
	}
	for _, c := range rbacCases {
		t.Run(c.name, func(t *testing.T) {
			res := dashboardRequest(t, http.MethodPost, path, c.user, map[string]any{
				"email": "invitee-" + uniqueSuffix() + "@example.test",
				"role":  "engineer",
			})
			if res.Status >= 500 {
				t.Fatalf("invitation creation as %q: got %d (500-class!) body=%s", c.name, res.Status, res.Body)
			}
			if res.Status != c.want {
				t.Fatalf("invitation creation as %q: got %d, want %d, body=%s", c.name, res.Status, c.want, res.Body)
			}
		})
	}

	// The reachable half: an owner creates a real invitation.
	inviteEmail := fmt.Sprintf("invitee-%s@example.test", uniqueSuffix())
	res := dashboardRequest(t, http.MethodPost, path, owner, map[string]any{
		"email": inviteEmail,
		"role":  "engineer",
	})
	if res.Status != http.StatusCreated {
		t.Fatalf("owner creating an invitation: got %d, want 201, body=%s", res.Status, res.Body)
	}
	var invite struct {
		ID        string `json:"id"`
		Token     string `json:"token"`
		Status    string `json:"status"`
		Role      string `json:"role"`
		Email     string `json:"email"`
		Delivered bool   `json:"delivered"`
	}
	res.JSON(t, &invite)
	if invite.Status != "pending" || invite.Role != "engineer" || invite.Email != inviteEmail {
		t.Fatalf("invite response shape mismatch: %+v", invite)
	}
	if invite.ID == "" {
		t.Fatalf("invite response missing id: %+v", invite)
	}

	// D06: the raw token must NOT come back to any client. It used to, and this test used to require
	// it. The token now exists only in the emailed URL; the DB stores nothing but its sha256 hash,
	// and InviteMemberModal was updated to stop offering a copy-paste link built from it. Asserting
	// its ABSENCE is the regression fence for that change.
	if invite.Token != "" {
		t.Errorf("invite response leaked the raw token (%q) — it must appear only in the emailed URL", invite.Token)
	}
	// `organizationId` is likewise no longer echoed back; it is verified against the DB row below.

	// D41: a 201 no longer implies delivery. With no EMAIL_SERVER configured in the compose stack
	// this is expected to be false — the point is that the outcome is REPORTED rather than swallowed
	// by a `.catch(() => {})`, so an operator can tell "created and emailed" from "created only".
	t.Logf("U22 invitation delivered=%v (false is expected when EMAIL_SERVER is unset)", invite.Delivered)
	t.Cleanup(func() { exec(context.Background(), `DELETE FROM organization_invitations WHERE id = $1`, invite.ID) })

	// Confirm it is a real row, not just an API-shaped response.
	var dbStatus, dbRole, dbEmail string
	queryRow(t, &dbStatus, `SELECT status FROM organization_invitations WHERE id = $1`, invite.ID)
	queryRow(t, &dbRole, `SELECT role FROM organization_invitations WHERE id = $1`, invite.ID)
	queryRow(t, &dbEmail, `SELECT email FROM organization_invitations WHERE id = $1`, invite.ID)
	if dbStatus != "pending" || dbRole != "engineer" || dbEmail != inviteEmail {
		t.Fatalf("organization_invitations row mismatch: status=%q role=%q email=%q", dbStatus, dbRole, dbEmail)
	}
	var dbOrgID string
	queryRow(t, &dbOrgID, `SELECT organization_id FROM organization_invitations WHERE id = $1`, invite.ID)
	if dbOrgID != f.OrgID {
		t.Fatalf("invitation written to the wrong organization: got %q, want %q", dbOrgID, f.OrgID)
	}
	// D06: what is persisted is the HASH, never the raw token. A 64-char hex digest, and never a
	// value that appeared in any response.
	var dbTokenHash string
	queryRow(t, &dbTokenHash, `SELECT token_hash FROM organization_invitations WHERE id = $1`, invite.ID)
	if len(dbTokenHash) != 64 {
		t.Errorf("token_hash is %d chars (%q); expected a 64-char sha256 hex digest", len(dbTokenHash), dbTokenHash)
	}

	// The "wall" this test was written to document is GONE: acceptance was implemented in P1/P2
	// (routes/invitations/[token] sets a short-lived HttpOnly cookie and redirects to
	// routes/auth/accept-invite, which claims the invitation atomically). What remains true is that
	// no JSON *API* endpoint consumes a token — acceptance is a browser page flow, and these
	// candidate API shapes must still 404 rather than 401/403/500, which would mean a half-built
	// endpoint exists. The token-bearing candidates are dropped: the raw token is no longer returned
	// to any client (D06), so this test cannot construct them, which is itself the point.
	candidates := []string{
		fmt.Sprintf("/api/organizations/%s/invitations/accept", f.OrgID),
		fmt.Sprintf("/api/organizations/%s/invitations/%s/accept", f.OrgID, invite.ID),
	}
	for _, cand := range candidates {
		r := dashboardRequest(t, http.MethodPost, cand, owner, map[string]any{"token": invite.Token})
		// 404 = no such route. 405 = the path matches an existing route's dynamic segment (P1 added
		// DELETE /api/organizations/[orgId]/invitations/[id] for revocation, so ".../invitations/accept"
		// binds [id]="accept") but that route exposes no POST handler. Both mean the same thing here:
		// there is no JSON acceptance endpoint. Anything else — 200/401/403/500 — would mean a
		// half-built one exists and needs review.
		if r.Status != http.StatusNotFound && r.Status != http.StatusMethodNotAllowed {
			t.Errorf("candidate accept route %s answered %d (want 404 or 405 = no POST endpoint there); "+
				"a JSON acceptance endpoint may have been added — review it before this test is trusted. body=%s",
				cand, r.Status, r.Body)
		}
	}
	t.Log("U22: invitation CREATION is verified end to end above (RBAC, real row, hashed token, no " +
		"token echoed back). Acceptance now exists as a browser page flow (routes/invitations/[token] -> " +
		"routes/auth/accept-invite), which this HTTP-level test does not drive: it needs a signed-in " +
		"session and a cookie round trip. Its logic is covered by unit tests instead " +
		"(routes/auth/accept-invite/page.server.test.ts).")
}

// ---------------------------------------------------------------------------------------------------
// U23 — RBAC: every DB-permitted role decides, never 500s, never silently denies as "unknown"
// ---------------------------------------------------------------------------------------------------

// TestU23_RBACDecidesEveryDBPermittedRole is the important row: it drives EVERY role value the database
// itself permits (not just the ones src/lib/rbac.ts historically knew about) through real, hasPermission
// (src/lib/rbac.ts)-gated routes, and asserts each produces a decided 200/201/403 — never a 500, and
// never an accidental deny caused by the role being unrecognized.
//
//   - organization_members.role CHECK (owner|admin|engineer|support|viewer) —
//     packages/db-migrations/migrations/1721800000_add_organization_layer.sql — tested against
//     GET/POST /api/organizations/[orgId]/keys, which calls hasPermission(membership.role, ...) directly.
//   - project_members.role CHECK (admin|developer|viewer|support) —
//     packages/db-migrations/migrations/1716550000_add_project_members.sql — tested against
//     POST /api/alerts, which also calls hasPermission(role, 'write') directly.
func TestU23_RBACDecidesEveryDBPermittedRole(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	type roleResult struct {
		scope        string
		role         string
		routeAndVerb string
		status       int
		want         int
	}
	var results []roleResult

	t.Run("organization_roles_against_keys_route", func(t *testing.T) {
		orgRoles := []string{"owner", "admin", "engineer", "support", "viewer"}
		keysPath := fmt.Sprintf("/api/organizations/%s/keys", f.OrgID)

		for _, role := range orgRoles {
			role := role
			t.Run(role, func(t *testing.T) {
				u := f.newDashboardUser(role)

				getRes := dashboardRequest(t, http.MethodGet, keysPath, u, nil)
				if getRes.Status >= 500 {
					t.Fatalf("GET %s as org role %q: got %d (500-class!) body=%s", keysPath, role, getRes.Status, getRes.Body)
				}
				results = append(results, roleResult{"organization_members", role, "GET /keys", getRes.Status, http.StatusOK})
				if getRes.Status != http.StatusOK {
					t.Errorf("GET %s as org role %q: got %d, want 200 ('read' is granted to every org role), body=%s",
						keysPath, role, getRes.Status, getRes.Body)
				}

				postRes := dashboardRequest(t, http.MethodPost, keysPath, u, map[string]any{
					"name":  "u23-" + role + "-" + uniqueSuffix(),
					"scope": "ingest",
				})
				if postRes.Status >= 500 {
					t.Fatalf("POST %s as org role %q: got %d (500-class!) body=%s", keysPath, role, postRes.Status, postRes.Body)
				}
				wantPost := http.StatusForbidden
				switch role {
				case "owner", "admin", "engineer":
					wantPost = http.StatusCreated
				}
				results = append(results, roleResult{"organization_members", role, "POST /keys", postRes.Status, wantPost})
				if postRes.Status != wantPost {
					t.Errorf("POST %s as org role %q: got %d, want %d, body=%s", keysPath, role, postRes.Status, wantPost, postRes.Body)
				}
			})
		}
	})

	t.Run("no_membership_and_unauthenticated_are_also_decided", func(t *testing.T) {
		keysPath := fmt.Sprintf("/api/organizations/%s/keys", f.OrgID)
		outsider := f.newDashboardUser("")

		res := dashboardRequest(t, http.MethodGet, keysPath, outsider, nil)
		if res.Status != http.StatusForbidden {
			t.Errorf("GET %s as an authenticated user with no membership: got %d, want 403, body=%s", keysPath, res.Status, res.Body)
		}
		res2 := dashboardRequest(t, http.MethodGet, keysPath, nil, nil)
		if res2.Status != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated: got %d, want 401, body=%s", keysPath, res2.Status, res2.Body)
		}
	})

	t.Run("project_roles_against_alerts_route", func(t *testing.T) {
		projectRoles := []string{"admin", "developer", "viewer", "support"}

		for _, role := range projectRoles {
			role := role
			t.Run(role, func(t *testing.T) {
				u := f.newDashboardUser("") // no org membership at all; only a project membership below
				dashSeedProjectMember(t, f.ProjectID, u.ID, role)

				res := dashboardRequest(t, http.MethodPost, "/api/alerts", u, map[string]any{
					"projectId":     f.ProjectID,
					"channel":       "email",
					"channelTarget": "u23-" + role + "@example.test",
				})
				if res.Status >= 500 {
					t.Fatalf("POST /api/alerts as project role %q: got %d (500-class!) body=%s", role, res.Status, res.Body)
				}
				want := http.StatusForbidden
				if role == "admin" || role == "developer" {
					want = http.StatusCreated
				}
				results = append(results, roleResult{"project_members", role, "POST /api/alerts", res.Status, want})
				if res.Status != want {
					t.Errorf("POST /api/alerts as project role %q: got %d, want %d, body=%s", role, res.Status, want, res.Body)
				}
			})
		}
	})

	t.Cleanup(func() {
		t.Log("U23 role -> route -> status table:")
		t.Logf("%-20s %-9s %-16s %-6s %-6s", "scope", "role", "route", "status", "want")
		for _, r := range results {
			t.Logf("%-20s %-9s %-16s %-6d %-6d", r.scope, r.role, r.routeAndVerb, r.status, r.want)
		}
	})
}

// ---------------------------------------------------------------------------------------------------
// U24 — magic-link sign-in
// ---------------------------------------------------------------------------------------------------

// TestU24_MagicLinkSignIn proves U24 over plain HTTP, no browser needed to reach a verdict, and finds a
// defect far more serious than the plan anticipated ("needs a live browser session").
//
// GET /auth/signin — the ONLY page that hosts the magic-link email form, custom or otherwise — is an
// INFINITE REDIRECT LOOP for every visitor, browser or not. Root cause, pinned down by reading the
// installed @auth/core/@auth/sveltekit source directly:
//
//   - apps/dashboard-web/src/lib/server/auth-config.ts:106 sets `pages: { signIn: '/auth/signin' }`.
//   - @auth/sveltekit's `handle` hook (node_modules/@auth/sveltekit/dist/index.js:335-346) intercepts
//     EVERY request under the Auth.js basePath ("/auth") whose first path segment is one of its own
//     reserved action names — including "signin" — and routes it straight to Auth.js's core handler,
//     BEFORE SvelteKit's own router ever sees it. This happens unconditionally: it does not matter that a
//     custom page exists at that exact route.
//   - For a GET with a configured `pages.signIn`, @auth/core's core signin renderer
//     (node_modules/@auth/core/lib/pages/index.js:53-60) unconditionns returns
//     `{redirect: `${pages.signIn}?callbackUrl=...`}` — i.e. it redirects back to `pages.signIn` itself.
//
// Because `pages.signIn` was set to the exact path Auth.js already reserves for its own "signin" action,
// every GET /auth/signin redirects to /auth/signin?callbackUrl=..., which redirects to itself again,
// forever. This is reproduced below and confirmed independent of cookies/query state. The custom
// `+page.svelte`/`+page.server.ts` at that route — including its "magiclink" form action — is therefore
// unreachable not because this harness lacks a browser, but because NOTHING (browser, curl, or otherwise)
// can ever reach it: SvelteKit's router never gets the request.
//
// What IS still checked: that the OTHER half of magic-link sign-in — GET /auth/callback/email exchanging
// a token for a session — genuinely works when a valid token exists, proving the mechanism itself is
// sound and the defect above is precisely what's blocking the row, not something deeper. AUTH_SECRET is
// a plaintext dev value in docker-compose.yml, and Auth.js hashes the URL token with it before comparing
// (node_modules/@auth/core/lib/actions/callback/index.js:144, `createHash(paramToken+secret)`, SHA-256
// per lib/utils/web.js:75-82), so this test seeds its own verification_token row hashed the same way and
// drives the real callback code path — not a bypass of it.
func TestU24_MagicLinkSignIn(t *testing.T) {
	requireStack(t)

	// --- A visitor must be able to REACH a sign-in page. ---
	//
	// This assertion is deliberately URL-agnostic. Its first version hardcoded /auth/signin and
	// asserted that it LOOPED — a defect-confirmation test that reported the bug by failing and would
	// have started failing for the opposite reason once the bug was fixed. A test that can never pass
	// cannot be part of a green suite, and a test that asserts current behaviour obstructs the fix.
	//
	// What U24 actually requires is the user-visible property: an unauthenticated visitor is sent to a
	// sign-in page, and that page RENDERS. Following the redirect chain from the app root proves it
	// without this test needing to know whether the page lives at /auth/signin, /signin, or anywhere
	// else — which is exactly the kind of coupling that made the first version brittle.
	status, finalURL, body, hops := dashFollowRedirects(t, "/", 8)
	t.Logf("U24: GET / as an anonymous visitor -> %d at %s after %d hop(s): %v", status, finalURL, len(hops), hops)

	if seen := dashFirstRepeat(hops); seen != "" {
		t.Errorf("the sign-in redirect chain from / revisits %s — it does not terminate.\n"+
			"  chain: %v\n"+
			"  A configured custom sign-in page at a path Auth.js reserves for its own built-in signin "+
			"action makes @auth/core redirect back to pages.signIn forever, which makes the entire human "+
			"sign-in surface (magic-link form and any Google button) unreachable by anyone.", seen, hops)
	} else if status != http.StatusOK {
		t.Errorf("the sign-in chain from / ended at %s with status %d, want 200 — an anonymous visitor "+
			"never reaches a rendered sign-in page.\n  chain: %v", finalURL, status, hops)
	} else if !dashLooksLikeSignInPage(body) {
		snippet := body
		if len(snippet) > 400 {
			snippet = snippet[:400] + "…"
		}
		t.Errorf("the chain from / ended at %s with 200, but the page does not look like a sign-in page "+
			"(no form, no email field, no sign-in text).\n  body: %s", finalURL, snippet)
	}

	// --- The mechanism, proven sound independent of the page defect above: a real callback session. ---
	f := newFixture(t)
	u := f.newDashboardUser("viewer") // real user + org membership; its pre-seeded session is unused here

	rawToken := dashRandomToken(t)
	authSecret := env("AUTH_SECRET", "dev-only-insecure-secret-change-me-please-32chars")
	hashed := sha256Hex(rawToken + authSecret)
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO verification_token (identifier, token, expires) VALUES ($1, $2, now() + interval '15 minutes')`,
		u.Email, hashed); err != nil {
		t.Fatalf("seeding verification_token: %v", err)
	}
	t.Cleanup(func() { exec(context.Background(), `DELETE FROM verification_token WHERE identifier = $1`, u.Email) })

	loc, cookies, status := dashCallbackEmail(t, u.Email, rawToken, "/")
	if status < 300 || status >= 400 {
		t.Fatalf("callback with a valid token: got %d (want a redirect), location=%q", status, loc)
	}
	if strings.Contains(loc, "/auth/error") {
		t.Fatalf("callback with a valid, unexpired token redirected to an error page: %s", loc)
	}
	// The cookie Auth.js actually sets here is __Secure-authjs.session-token (confirmed live: this
	// specific action computes useSecureCookies=true), NOT the unprefixed name — but the harness's own
	// dashboardRequest always presents the unprefixed "authjs.session-token" and that IS accepted by
	// locals.auth() (confirmed live: a manually-seeded session row authenticates via the unprefixed cookie
	// name regardless of which name the app used when it originally set it). So only the TOKEN VALUE
	// matters for re-presenting the session below; extract it from whichever cookie name carries it.
	sessionToken := cookies["authjs.session-token"]
	if sessionToken == "" {
		sessionToken = cookies["__Secure-authjs.session-token"]
	}
	if sessionToken == "" {
		t.Fatalf("callback succeeded (redirect to %s) but set no session-token cookie under either name; cookies seen: %v", loc, cookies)
	}
	t.Cleanup(func() { exec(context.Background(), `DELETE FROM session WHERE session_token = $1`, sessionToken) })

	// Prove it is a real, usable session by presenting it to a real authenticated route.
	authedUser := &dashboardUser{ID: u.ID, Email: u.Email, SessionToken: sessionToken}
	orgsRes := dashboardRequest(t, http.MethodGet, "/api/organizations", authedUser, nil)
	if orgsRes.Status != http.StatusOK {
		t.Fatalf("using the magic-link session against GET /api/organizations: got %d, want 200, body=%s", orgsRes.Status, orgsRes.Body)
	}
	t.Logf("U24 mechanism PROVEN sound: GET /auth/callback/email, given a valid token, establishes a real, usable session (GET /api/organizations returned 200 using its cookie). The row is blocked purely by the /auth/signin redirect-loop defect above, not by anything wrong with the token-exchange mechanism itself.")

	// Single-use: useVerificationToken deletes the row on success, so a replay must NOT re-authenticate.
	loc2, cookies2, status2 := dashCallbackEmail(t, u.Email, rawToken, "/")
	if !strings.Contains(loc2, "/auth/error") {
		t.Errorf("REPLAYING a consumed magic-link token: got %d -> %q (want a redirect to /auth/error; the token should be single-use)", status2, loc2)
	}
	if cookies2["authjs.session-token"] != "" || cookies2["__Secure-authjs.session-token"] != "" {
		t.Errorf("replaying a consumed magic-link token still set a session cookie — token reuse is not being rejected")
	}
}

// ---------------------------------------------------------------------------------------------------
// U25 — Google sign-in with the domain restriction unset
// ---------------------------------------------------------------------------------------------------

// TestU25_GoogleSignInDomainRestrictionUnset proves U25: with the domain restriction unset, any Google
// account should be permitted. This CANNOT be driven end to end through the real OAuth flow in this
// stack: docker-compose.yml sets GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET to "" (confirmed live below via
// Auth.js's own /auth/providers listing), so auth-config.ts's `if (GOOGLE_WORKSPACE_CLIENT_ID && ...)`
// guard is false and the google provider is never registered — there is no OAuth consent screen for a
// browser (or anything else) to complete. A live proof of this row needs a real Google test app (or a
// mock IdP standing in for one) plus a browser; neither exists here.
//
// What IS checked without either: whether "unset" is even an expressible configuration. It is not.
// auth-config.ts:10 hardcodes `const ALLOWED_EMAIL_DOMAIN = 'company.com'` with no environment read at
// all — plan item P3-4 ("make it env-driven, empty = allow all") is confirmed NOT done. This test asserts
// the intended behavior and is EXPECTED TO FAIL against the current source; that failure is the correct,
// confirmed-open-defect outcome for this row, not a bug in the test.
func TestU25_GoogleSignInDomainRestrictionUnset(t *testing.T) {
	requireStack(t)

	res := dashboardRequest(t, http.MethodGet, "/auth/providers", nil, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("GET /auth/providers: got %d, body=%s", res.Status, res.Body)
	}
	if strings.Contains(res.Body, `"google"`) {
		t.Logf("google IS registered as a provider in this stack (GOOGLE_CLIENT_ID/SECRET must no longer be empty) "+
			"— a live OAuth-driven proof of U25 may now be possible instead of the static probe below. body=%s", res.Body)
	} else {
		t.Logf("confirmed live: google is NOT a registered provider (GOOGLE_CLIENT_ID/SECRET are empty in docker-compose.yml) "+
			"— there is no OAuth consent flow to drive, browser or otherwise. /auth/providers body=%s", res.Body)
	}

	src, err := os.ReadFile(dashRepoPath("apps/dashboard-web/src/lib/server/auth-config.ts"))
	if err != nil {
		t.Fatalf("reading auth-config.ts: %v", err)
	}
	content := string(src)

	// This assertion is STATIC, and that is a real limitation stated plainly rather than hidden: Google
	// is not a registered provider in this stack, so there is no consent flow to drive and no way to
	// observe the callback's decision over HTTP. The source is the only available evidence.
	//
	// What it must NOT be is a skip. The previous version skipped as soon as the fix appeared to have
	// landed, which under SENTINEL_E2E=1 is a hard failure by design (P0-4) — and, worse, meant the row
	// could never report success. So the checks below are written as the DESIRED end state: red while
	// the domain is hardcoded, green once it is env-driven with empty meaning "allow all".
	if !strings.Contains(content, "env.ALLOWED_EMAIL_DOMAIN") &&
		!strings.Contains(content, "process.env.ALLOWED_EMAIL_DOMAIN") {
		t.Errorf("auth-config.ts does not read ALLOWED_EMAIL_DOMAIN from the environment at all. P3-4 " +
			"requires it env-driven with empty meaning \"allow all\", so that \"unset\" is an expressible, " +
			"permitted configuration. While it is a literal, every Google account outside that one domain is " +
			"permanently rejected in any real deployment.")
	}
	// Comments are stripped before this check. The first version grepped the whole file and failed on a
	// comment that documented the old hardcoded value — which is exactly the sort of comment worth
	// keeping. What must not survive is the literal in CODE, as a default or a fallback.
	if line := dashFirstCodeLineContaining(content, "company.com"); line != "" {
		t.Errorf("auth-config.ts still hardcodes the placeholder domain in code: %s", line)
	}

	// Env-driven is not sufficient on its own: reading the variable and then comparing unconditionally
	// would reject everything when it is unset, which is the opposite of "allow all". The comparison has
	// to be guarded by the value being non-empty.
	guarded := strings.Contains(content, "ALLOWED_EMAIL_DOMAIN && domain !== ALLOWED_EMAIL_DOMAIN") ||
		strings.Contains(content, "ALLOWED_EMAIL_DOMAIN !== '' && domain !== ALLOWED_EMAIL_DOMAIN")
	if !guarded {
		t.Errorf("the domain comparison in auth-config.ts is not guarded by ALLOWED_EMAIL_DOMAIN being " +
			"non-empty. Unset must permit every domain; an unguarded `domain !== ALLOWED_EMAIL_DOMAIN` " +
			"rejects every sign-in when the variable is absent.")
	}

}

// dashFollowRedirects walks a redirect chain by hand and reports every hop, so a caller can tell a
// terminating chain from a cycle. Go's http.Client would either follow silently (hiding the shape) or
// give up after 10 hops with an error that does not say what repeated.
//
// Returns the final status, the final URL, the final body, and the ordered list of locations visited.
func dashFollowRedirects(t *testing.T, path string, maxHops int) (int, string, string, []string) {
	t.Helper()

	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	current := path
	var hops []string
	for i := 0; i < maxHops; i++ {
		url := current
		if strings.HasPrefix(url, "/") {
			url = cfg.DashboardURL + url
		}

		resp, err := client.Get(url)
		if err != nil {
			t.Fatalf("GET %s (hop %d): %v", url, i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return resp.StatusCode, current, string(body), hops
		}
		loc := resp.Header.Get("Location")
		if loc == "" {
			return resp.StatusCode, current, string(body), hops
		}
		hops = append(hops, loc)
		current = loc
	}
	return 0, current, "", hops
}

// dashFirstRepeat returns the first location that appears twice in a redirect chain, or "" if every hop
// is distinct. A repeat is the definition of a loop, and naming the repeated target is most of the
// diagnosis.
func dashFirstRepeat(hops []string) string {
	seen := make(map[string]bool, len(hops))
	for _, h := range hops {
		if seen[h] {
			return h
		}
		seen[h] = true
	}
	return ""
}

// dashLooksLikeSignInPage decides whether a rendered page is plausibly a sign-in page, without pinning
// the assertion to this app's exact markup — the point is that a human arriving here could sign in, not
// that the HTML matches a snapshot.
func dashLooksLikeSignInPage(body string) bool {
	lower := strings.ToLower(body)
	hasForm := strings.Contains(lower, "<form") || strings.Contains(lower, `type="email"`) ||
		strings.Contains(lower, "type='email'")
	mentionsSignIn := strings.Contains(lower, "sign in") || strings.Contains(lower, "signin") ||
		strings.Contains(lower, "sign-in") || strings.Contains(lower, "magic link")
	return hasForm && mentionsSignIn
}

// ---------------------------------------------------------------------------------------------------
// U31 — issue list / detail / search, correct counts, correct tenant scoping
// ---------------------------------------------------------------------------------------------------

// TestU31_IssueListSearchAndTenantScoping proves U31 via the JSON API
// (GET /api/projects/[projectId]/issues) rather than scraping HTML: correct counts for a seeded org,
// correct detail fields per issue, filtering ("search") by status, and — the assertion that matters
// most — that a second organization's issues are never visible to the first, and that naming another
// org's project outright fails closed (403) rather than silently returning an empty list.
func TestU31_IssueListSearchAndTenantScoping(t *testing.T) {
	requireStack(t)

	fa := newFixture(t)
	fb := newFixture(t) // a second, unrelated tenant — proves no cross-visibility, never queried as "self"

	userA := fa.newDashboardUser("admin")

	classes := []string{"AlphaError", "BravoError", "CharlieError"}
	for _, class := range classes {
		res := fa.ingest(fa.newEvent().with(map[string]any{
			"error_class": class,
			"message":     "u31 " + class,
		}))
		if res.Status != http.StatusAccepted {
			t.Fatalf("seeding issue for class %s: ingest returned %d: %s", class, res.Status, res.Body)
		}
	}
	fa.waitForOccurrences(3)
	fa.waitForIssues(3)
	seededA := dashQueryIssues(t, fa.ProjectID)
	if len(seededA) != 3 {
		t.Fatalf("fixture A: want 3 issues seeded, got %d: %+v", len(seededA), seededA)
	}

	resB := fb.ingest(fb.newEvent().with(map[string]any{
		"error_class": "OtherOrgError",
		"message":     "should never be visible to org A",
	}))
	if resB.Status != http.StatusAccepted {
		t.Fatalf("seeding fixture B's issue: ingest returned %d: %s", resB.Status, resB.Body)
	}
	fb.waitForIssues(1)

	// --- List + detail fields, correct counts, own-tenant-only ---
	var listed struct {
		Issues []dashIssueJSON `json:"issues"`
	}
	res := dashboardRequest(t, http.MethodGet, fmt.Sprintf("/api/projects/%s/issues", fa.ProjectID), userA, nil)
	if res.Status != http.StatusOK {
		t.Fatalf("GET issues for org A's own project: got %d, body=%s", res.Status, res.Body)
	}
	res.JSON(t, &listed)
	if len(listed.Issues) != 3 {
		t.Fatalf("expected exactly 3 issues for fixture A's project, got %d: %+v", len(listed.Issues), listed.Issues)
	}
	gotClasses := map[string]bool{}
	for _, is := range listed.Issues {
		if is.ProjectID != fa.ProjectID {
			t.Fatalf("issue %s has projectId=%s, want %s — tenant leak inside a query that is supposed to be scoped", is.ID, is.ProjectID, fa.ProjectID)
		}
		if is.ErrorClass == "OtherOrgError" {
			t.Fatalf("tenant scoping violated: org B's issue (%s) appeared in org A's issue list", is.ID)
		}
		gotClasses[is.ErrorClass] = true
		if is.Message == "" || is.Status == "" || is.Count < 1 {
			t.Errorf("issue %s missing expected detail fields: %+v", is.ID, is)
		}
	}
	for _, class := range classes {
		if !gotClasses[class] {
			t.Errorf("expected an issue for class %s in the list, not present: %+v", class, listed.Issues)
		}
	}

	// --- Search/filter by status ---
	resolvedID := seededA[0].ID
	fa.setIssueStatus(resolvedID, "resolved")
	var resolvedList struct {
		Issues []dashIssueJSON `json:"issues"`
	}
	res2 := dashboardRequest(t, http.MethodGet, fmt.Sprintf("/api/projects/%s/issues?status=resolved", fa.ProjectID), userA, nil)
	if res2.Status != http.StatusOK {
		t.Fatalf("GET issues?status=resolved: got %d, body=%s", res2.Status, res2.Body)
	}
	res2.JSON(t, &resolvedList)
	if len(resolvedList.Issues) != 1 || resolvedList.Issues[0].ID != resolvedID {
		t.Fatalf("status=resolved filter: want exactly issue %s, got %+v", resolvedID, resolvedList.Issues)
	}

	// --- Tenant scoping: org A's user must be refused org B's project outright (403), not shown an
	// empty (or worse, populated) list. ---
	resCross := dashboardRequest(t, http.MethodGet, fmt.Sprintf("/api/projects/%s/issues", fb.ProjectID), userA, nil)
	if resCross.Status != http.StatusForbidden {
		t.Fatalf("org A's user requesting org B's project issues: got %d, want 403, body=%s", resCross.Status, resCross.Body)
	}

	resAnon := dashboardRequest(t, http.MethodGet, fmt.Sprintf("/api/projects/%s/issues", fa.ProjectID), nil, nil)
	if resAnon.Status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated issue list: got %d, want 401, body=%s", resAnon.Status, resAnon.Body)
	}
}

// ---------------------------------------------------------------------------------------------------
// U32 — cron retention endpoint: requires auth, deletes only beyond the window
// ---------------------------------------------------------------------------------------------------

// TestU32_RetentionRequiresAuthAndWindow proves both halves of U32.
//
// Auth half: missing/wrong x-cron-secret is 401 (already true per the plan; reconfirmed here).
//
// Window half (never previously verified): seeds one issue with two occurrences, backdates one of them
// well outside DATA_RETENTION_DAYS (30, per docker-compose.yml), and asserts retention removes exactly
// the backdated one, keeping the fresh occurrence and the issue itself.
//
// Along the way this surfaces a real defect in the SAME code path: retention.ts's orphaned-issue delete
// (src/lib/server/retention.ts:40-48) has no age check of its own at all — it deletes ANY issue with zero
// occurrences immediately, regardless of how recently that issue (or its now-gone occurrence) was
// created. That is demonstrated directly: a second, freshly-seeded issue whose only occurrence is deleted
// moments before the retention call is gone after it, even though nothing about it is 30 days old. See
// the "orphaned issue deletion respects the window" subtest, which is expected to fail.
//
// Blast radius note: cleanupRetainedData is global — it has no project/org scoping at all, by the
// product's own design (a retention cron is inherently cross-tenant). This test's own writes stay scoped
// to rows it created (backdating only its own occurrence, deleting only its own occurrence), but the
// actual retention call this test makes is real and, like any real cron run, can affect stale data left by
// any other fixture in this shared database if that data is already 30+ days old. Given the DB is a fresh
// dev/test instance with tests cleaning up their own rows via t.Cleanup, that risk is expected to be
// negligible, but it is inherent to the endpoint, not to this test.
func TestU32_RetentionRequiresAuthAndWindow(t *testing.T) {
	requireStack(t)

	cronSecret := env("CRON_SECRET", "dev-only-cron-secret-change-me")

	t.Run("missing_secret_is_401", func(t *testing.T) {
		res := dashCronRequest(t, "", false)
		if res.Status != http.StatusUnauthorized {
			t.Fatalf("missing x-cron-secret: got %d, want 401, body=%s", res.Status, res.Body)
		}
	})
	t.Run("wrong_secret_is_401", func(t *testing.T) {
		res := dashCronRequest(t, cronSecret+"-wrong", true)
		if res.Status != http.StatusUnauthorized {
			t.Fatalf("wrong x-cron-secret: got %d, want 401, body=%s", res.Status, res.Body)
		}
	})

	f := newFixture(t)

	// One issue, two occurrences: one will age out, one will not.
	old := f.ingest(f.newEvent().with(map[string]any{"error_class": "RetentionWindowError", "message": "outside the window"}))
	if old.Status != http.StatusAccepted {
		t.Fatalf("seeding first occurrence: got %d: %s", old.Status, old.Body)
	}
	f.waitForOccurrences(1)
	fresh := f.ingest(f.newEvent().with(map[string]any{"error_class": "RetentionWindowError", "message": "outside the window"}))
	if fresh.Status != http.StatusAccepted {
		t.Fatalf("seeding second occurrence: got %d: %s", fresh.Status, fresh.Body)
	}
	f.waitForOccurrences(2)

	issues := dashQueryIssues(t, f.ProjectID)
	var windowIssueID string
	for _, is := range issues {
		if is.ErrorClass == "RetentionWindowError" {
			windowIssueID = is.ID
		}
	}
	if windowIssueID == "" {
		t.Fatalf("could not find the seeded RetentionWindowError issue among %+v", issues)
	}
	occIDs := dashOccurrenceIDsForIssue(t, windowIssueID)
	if len(occIDs) != 2 {
		t.Fatalf("want 2 occurrences for the window-test issue, got %d", len(occIDs))
	}
	oldOccID, freshOccID := occIDs[0], occIDs[1]
	dashBackdateOccurrence(t, oldOccID, 45*24*time.Hour) // well outside DATA_RETENTION_DAYS=30

	// A second, separate issue whose only occurrence we delete ourselves, scoped to our own fixture,
	// moments before calling retention — manufacturing a FRESH zero-occurrence issue to test whether the
	// orphan-delete path respects any age window at all.
	orphanIngest := f.ingest(f.newEvent().with(map[string]any{"error_class": "RetentionOrphanError", "message": "fresh but about to be orphaned"}))
	if orphanIngest.Status != http.StatusAccepted {
		t.Fatalf("seeding orphan-test issue: got %d: %s", orphanIngest.Status, orphanIngest.Body)
	}
	f.waitForIssues(2)
	var orphanIssueID string
	for _, is := range dashQueryIssues(t, f.ProjectID) {
		if is.ErrorClass == "RetentionOrphanError" {
			orphanIssueID = is.ID
		}
	}
	if orphanIssueID == "" {
		t.Fatalf("could not find the seeded RetentionOrphanError issue")
	}
	orphanOccIDs := dashOccurrenceIDsForIssue(t, orphanIssueID)
	if len(orphanOccIDs) != 1 {
		t.Fatalf("want 1 occurrence for the orphan-test issue, got %d", len(orphanOccIDs))
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM error_occurrences WHERE id = $1`, orphanOccIDs[0]); err != nil {
		t.Fatalf("deleting the orphan-test issue's only occurrence: %v", err)
	}

	// The one real, global invocation this test makes.
	res := dashCronRequest(t, cronSecret, true)
	if res.Status != http.StatusOK {
		t.Fatalf("retention with the correct secret: got %d, want 200, body=%s", res.Status, res.Body)
	}
	var body struct {
		Success bool `json:"success"`
		Result  struct {
			DeletedOccurrences    int `json:"deletedOccurrences"`
			MarkedStaleIssues     int `json:"markedStaleIssues"`
			DeletedOrphanedIssues int `json:"deletedOrphanedIssues"`
			RetentionDays         int `json:"retentionDays"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
		t.Fatalf("retention response was not the documented JSON shape: %v, body=%s", err, res.Body)
	}
	if !body.Success || body.Result.DeletedOccurrences < 1 {
		t.Fatalf("retention reported an unexpected result: %+v (raw body: %s)", body, res.Body)
	}

	t.Run("occurrences_outside_window_deleted_inside_window_kept", func(t *testing.T) {
		if dashOccurrenceExists(t, oldOccID) {
			t.Errorf("occurrence %s (backdated 45 days, window is %d days) should have been deleted, still present", oldOccID, body.Result.RetentionDays)
		}
		if !dashOccurrenceExists(t, freshOccID) {
			t.Errorf("occurrence %s (created moments ago) should NOT have been deleted, but is gone", freshOccID)
		}
		if !dashIssueExists(t, windowIssueID) {
			t.Errorf("issue %s should still exist (occurrence %s is still within the window) but retention removed it", windowIssueID, freshOccID)
		}
	})

	t.Run("orphaned issue deletion respects the window", func(t *testing.T) {
		if dashIssueExists(t, orphanIssueID) {
			t.Logf("issue %s survived retention — contradicts the source read of retention.ts:40-48; re-verify before trusting this defect report", orphanIssueID)
			return
		}
		t.Errorf("CONFIRMED DEFECT (apps/dashboard-web/src/lib/server/retention.ts:40-48): the orphaned-issue "+
			"delete query has NO retention-window check at all — `DELETE FROM issues WHERE id NOT IN (SELECT "+
			"DISTINCT issue_id FROM error_occurrences)` with no created_at/age condition. Issue %s, created "+
			"moments ago (nowhere near the %d-day window), was deleted the instant its last occurrence was "+
			"removed. \"deletes only beyond the window\" does not hold for this code path — it deletes any "+
			"zero-occurrence issue immediately, however fresh.", orphanIssueID, body.Result.RetentionDays)
	})
}

// dashFirstCodeLineContaining returns the first non-comment line containing needle, trimmed, or "" if
// none does. Line comments (`//`) and block-comment continuations (`*`) are ignored, so an assertion can
// distinguish a value that is still hardcoded in code from a comment that merely mentions it — a
// distinction a plain strings.Contains over the whole file cannot make.
func dashFirstCodeLineContaining(content, needle string) string {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "*") ||
			strings.HasPrefix(line, "/*") {
			continue
		}
		if strings.Contains(line, needle) {
			return line
		}
	}
	return ""
}
