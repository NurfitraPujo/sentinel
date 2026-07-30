package e2e

// This file drives U14-U17 of the P7 use-case matrix (docs/plans/E2E_RECOVERY_PLAN.md, "## P7 — The E2E
// proof harness") against the real dashboard API-key routes and the real ingestor.
//
// U15-U17 are defect S7's regression tests (docs/memory/VERIFIED_STATE.md): the ingestor's key lookup
// used to filter on `status` only, never on `expires_at`, and the dashboard's revoke/rotate mutations
// never flipped `status` at all — so a revoked or rotated-away key kept authenticating until its Redis
// cache entry aged out (or forever, for expiry). apps/ingestor-go/auth/apikey.go now enforces
// `expires_at` in the lookup query itself and caps any cached entry's TTL at the key's remaining
// lifetime; apps/dashboard-web/src/lib/db/queries/apikeys.ts now sets status='revoked' immediately on
// both revoke and rotate, and publishes `api_key.invalidated` (keyHash) over NATS so the ingestor's
// Redis cache entry is deleted right away instead of waiting out the TTL.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
)

// apikeysKeyResponse mirrors the `{ key, token }` shape returned by
// POST /api/organizations/[orgId]/keys and .../rotate.
type apikeysKeyResponse struct {
	Key struct {
		ID             string  `json:"id"`
		OrganizationID string  `json:"organizationId"`
		ProjectID      *string `json:"projectId"`
		Name           string  `json:"name"`
		KeyPrefix      string  `json:"keyPrefix"`
		Scope          string  `json:"scope"`
		Status         string  `json:"status"`
	} `json:"key"`
	Token string `json:"token"`
}

// apikeysRevokeResponse mirrors DELETE .../keys/[keyId]'s body.
type apikeysRevokeResponse struct {
	Success bool   `json:"success"`
	KeyID   string `json:"keyId"`
}

// apikeysCreateViaAPI calls the real dashboard route as an authorized user and returns the decoded
// response. It fails the test outright on a non-201 so every row's happy path starts from a known-good
// key rather than silently limping on with a zero-value response.
func apikeysCreateViaAPI(t *testing.T, f *fixture, user *dashboardUser, body map[string]any) apikeysKeyResponse {
	t.Helper()
	res := dashboardRequest(t, http.MethodPost, "/api/organizations/"+f.OrgID+"/keys", user, body)
	if res.Status != http.StatusCreated {
		t.Fatalf("POST .../keys: want 201, got %d\n  body: %s", res.Status, res.Body)
	}
	var out apikeysKeyResponse
	res.JSON(t, &out)
	return out
}

// apikeysHashOf returns the hex sha256 of a plaintext secret, matching how both the ingestor and the
// dashboard hash keys for storage/lookup.
func apikeysHashOf(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// apikeysAssertRowMatches confirms the row the dashboard route claims to have created is a REAL,
// independently-queryable project_api_keys row with a hash matching the returned plaintext token — the
// exact thing a hardcoded-fixture mock could never produce. Row U14's point is precisely that this used
// to be a mock; a passing ingest alone wouldn't distinguish "real key" from "route also happens to
// accept anything".
func apikeysAssertRowMatches(t *testing.T, keyID, orgID, projectID, token string) {
	t.Helper()
	var gotOrg, gotHash, gotStatus string
	var gotProject *string
	if err := pool.QueryRow(context.Background(),
		`SELECT organization_id::text, project_id::text, key_hash, status FROM project_api_keys WHERE id = $1`,
		keyID,
	).Scan(&gotOrg, &gotProject, &gotHash, &gotStatus); err != nil {
		t.Fatalf("querying created key %s back from project_api_keys: %v", keyID, err)
	}
	if gotOrg != orgID {
		t.Fatalf("created key's organization_id = %q, want %q", gotOrg, orgID)
	}
	if gotProject == nil || *gotProject != projectID {
		t.Fatalf("created key's project_id = %v, want %q", gotProject, projectID)
	}
	if gotHash != apikeysHashOf(token) {
		t.Fatalf("created key's stored key_hash does not match sha256 of the returned token — route is not persisting the real secret")
	}
	if gotStatus != "active" {
		t.Fatalf("created key's status = %q, want active", gotStatus)
	}
}

// apikeysPollUntilStatus polls /ingest with secret every 20ms until the response matches want or
// maxWait elapses, and returns the elapsed time at the moment it first matched (or timed out). This is
// deliberately NOT waitFor: waitFor doesn't report back the elapsed duration, and U15's whole point is
// measuring that duration, not just eventually observing the end state.
func apikeysPollUntilStatus(t *testing.T, f *fixture, secret string, want int, maxWait time.Duration) (time.Duration, ingestResult) {
	t.Helper()
	start := time.Now()
	var last ingestResult
	for {
		last = f.ingest(f.newEvent(), ingestOpts{APIKey: secret})
		elapsed := time.Since(start)
		if last.Status == want {
			return elapsed, last
		}
		if elapsed >= maxWait {
			return elapsed, last
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestU14_DashboardCreatedKeyIngestsForReal proves row U14: creating a key through the real dashboard
// API as an authorized user produces a genuinely usable ingest credential — not a hardcoded fixture
// (VERIFIED_STATE.md's prior finding on this exact route).
func TestU14_DashboardCreatedKeyIngestsForReal(t *testing.T) {
	requireStack(t)
	f := newFixture(t)
	// FR-007 (spec 008): only owner/admin/engineer may manage keys. 'owner' is unambiguously permitted.
	owner := f.newDashboardUser("owner")

	created := apikeysCreateViaAPI(t, f, owner, map[string]any{
		"name":      "u14 project-scoped ingest key",
		"scope":     "ingest",
		"projectId": f.ProjectID,
	})
	if created.Token == "" {
		t.Fatalf("create-key response carried no plaintext token: %+v", created)
	}
	if created.Key.ID == "" {
		t.Fatalf("create-key response carried no key id: %+v", created)
	}
	apikeysAssertRowMatches(t, created.Key.ID, f.OrgID, f.ProjectID, created.Token)

	// The actual bar: ingest with the returned secret must be accepted and the event must land.
	res := f.ingest(f.newEvent(), ingestOpts{APIKey: created.Token})
	if res.Status != http.StatusAccepted {
		t.Fatalf("ingest with dashboard-issued key: want 202, got %d\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(1)
}

// TestU15_RevokedKeyFailsFastOverNATS proves row U15: revoking a key through the dashboard makes it
// fail ingest, and does so within 1 second — the bound only meaningful because revocation propagates by
// NATS invalidation of the ingestor's Redis cache entry, not by waiting out the cache TTL (S7).
func TestU15_RevokedKeyFailsFastOverNATS(t *testing.T) {
	requireStack(t)
	f := newFixture(t)
	owner := f.newDashboardUser("owner")

	created := apikeysCreateViaAPI(t, f, owner, map[string]any{
		"name":      "u15 revoke-me",
		"scope":     "ingest",
		"projectId": f.ProjectID,
	})

	// Populate the ingestor's Redis cache for this key BEFORE revoking, so the propagation measured
	// below is the real "revoke while cached" path (S7's exact failure mode), not a cold lookup that
	// would 401 anyway on the next DB read.
	warm := f.ingest(f.newEvent(), ingestOpts{APIKey: created.Token})
	if warm.Status != http.StatusAccepted {
		t.Fatalf("warm-up ingest before revoke: want 202, got %d\n  body: %s", warm.Status, warm.Body)
	}
	f.waitForOccurrences(1)

	revokeRes := dashboardRequest(t, http.MethodDelete, "/api/organizations/"+f.OrgID+"/keys/"+created.Key.ID, owner, nil)
	if revokeRes.Status != http.StatusOK {
		t.Fatalf("DELETE .../keys/%s: want 200, got %d\n  body: %s", created.Key.ID, revokeRes.Status, revokeRes.Body)
	}
	var revoked apikeysRevokeResponse
	revokeRes.JSON(t, &revoked)
	if !revoked.Success {
		t.Fatalf("revoke response reported success=false: %s", revokeRes.Body)
	}

	elapsed, last := apikeysPollUntilStatus(t, f, created.Token, http.StatusUnauthorized, 10*time.Second)
	t.Logf("U15: revoked key returned status %d after %s (cache TTL is %s)", last.Status, elapsed, auth.CacheTTLForLogging())
	if last.Status != http.StatusUnauthorized {
		t.Fatalf("revoked key never returned 401 within 10s — still returning %d after %s\n  body: %s",
			last.Status, elapsed, last.Body)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("revoked key returned 401, but only after %s — exceeds the 1s bound (S7 regression). "+
			"Mechanism: dashboard publishes api_key.invalidated{keyHash} over NATS; ingestor deletes the "+
			"Redis cache entry on receipt (apps/ingestor-go/auth/apikey.go). A miss here means either the "+
			"NATS message was not delivered/consumed in time, or the invalidation payload/key did not match.",
			elapsed)
	}

	// The DB row itself must also be correct, independent of caching.
	var status string
	queryRow(t, &status, `SELECT status FROM project_api_keys WHERE id = $1`, created.Key.ID)
	if status != "revoked" {
		t.Fatalf("key row status = %q after revoke, want revoked", status)
	}
}

// TestU16_RotatedKeyInvalidatesOldSecretImmediately proves row U16: rotating a key makes the OLD
// secret 401, and the NEW secret returned by the route works.
func TestU16_RotatedKeyInvalidatesOldSecretImmediately(t *testing.T) {
	requireStack(t)
	f := newFixture(t)
	owner := f.newDashboardUser("owner")

	created := apikeysCreateViaAPI(t, f, owner, map[string]any{
		"name":      "u16 rotate-me",
		"scope":     "ingest",
		"projectId": f.ProjectID,
	})
	oldToken := created.Token

	warm := f.ingest(f.newEvent(), ingestOpts{APIKey: oldToken})
	if warm.Status != http.StatusAccepted {
		t.Fatalf("warm-up ingest before rotate: want 202, got %d\n  body: %s", warm.Status, warm.Body)
	}
	f.waitForOccurrences(1)

	rotateRes := dashboardRequest(t, http.MethodPost, "/api/organizations/"+f.OrgID+"/keys/"+created.Key.ID+"/rotate", owner, nil)
	if rotateRes.Status != http.StatusOK {
		t.Fatalf("POST .../keys/%s/rotate: want 200, got %d\n  body: %s", created.Key.ID, rotateRes.Status, rotateRes.Body)
	}
	var rotated apikeysKeyResponse
	rotateRes.JSON(t, &rotated)
	if rotated.Token == "" || rotated.Token == oldToken {
		t.Fatalf("rotate response did not carry a distinct new token: %+v", rotated)
	}

	elapsed, last := apikeysPollUntilStatus(t, f, oldToken, http.StatusUnauthorized, asyncTimeout)
	if last.Status != http.StatusUnauthorized {
		t.Fatalf("old (pre-rotation) key never returned 401 within %s — still returning %d\n  body: %s",
			asyncTimeout, last.Status, last.Body)
	}
	t.Logf("U16: old key invalidated after rotate in %s", elapsed)

	// New secret must actually work, and land its own occurrence (project unchanged: 1 already there
	// from warm-up, +1 from the new key).
	res := f.ingest(f.newEvent(), ingestOpts{APIKey: rotated.Token})
	if res.Status != http.StatusAccepted {
		t.Fatalf("ingest with rotated (new) key: want 202, got %d\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(2)

	var oldStatus string
	queryRow(t, &oldStatus, `SELECT status FROM project_api_keys WHERE id = $1`, created.Key.ID)
	if oldStatus != "revoked" {
		t.Fatalf("old key row status after rotate = %q, want revoked", oldStatus)
	}
}

// TestU17_ExpiredKeyRejected proves row U17: a key whose expires_at is already in the past is rejected
// with 401, even on a cold (never-cached) lookup — apikey.go's SELECT filters `expires_at` in the WHERE
// clause itself, so an expired row can never be scanned into a cache entry in the first place.
func TestU17_ExpiredKeyRejected(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	secret := newSecret(t)
	past := time.Now().Add(-1 * time.Hour)
	f.addKey(keySpec{
		Name:          "u17 expired",
		Secret:        secret,
		ProjectScoped: true,
		ExpiresAt:     &past,
	})

	res := f.ingest(f.newEvent(), ingestOpts{APIKey: secret})
	if res.Status != http.StatusUnauthorized {
		t.Fatalf("ingest with an already-expired key: want 401, got %d\n  body: %s", res.Status, res.Body)
	}
	if f.occurrenceCount() != 0 {
		t.Fatalf("expired key's ingest was rejected but an occurrence still landed: %d", f.occurrenceCount())
	}
}
