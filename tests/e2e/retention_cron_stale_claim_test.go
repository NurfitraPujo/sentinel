//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------------------------------
// N9 — the retention scheduler makes the claim_released(reason=stale) path reachable
// ---------------------------------------------------------------------------------------------------
//
// Before N9 nothing in the reference stack invoked POST /api/cron/retention: docker-compose.yml set
// CRON_SECRET but shipped no cron service, and the Helm chart had no CronJob (DEPLOYMENT.md listed it as
// an unfinished operator checklist item). Consequence: reapStaleClaims (src/lib/server/retention.ts)
// never ran, so a claim held by a crashed agent was never force-released and the
// claim_released(reason=stale) issue_activity path was untestable in the reference stack.
//
// N9 adds two schedulers — a `sentinel-cron` compose service (scripts/cron-entrypoint.sh) and a Helm
// CronJob (deploy/helm/sentinel/templates/retention-cronjob.yaml) — each of which does exactly one
// thing: POST /api/cron/retention with the `x-cron-secret` header. This test drives that endpoint with
// the *identical* request the containerized caller makes (POST + x-cron-secret; see dashCronRequest and
// scripts/cron-entrypoint.sh's curl), and proves the previously-unreachable stale-claim release actually
// fires end to end.
//
// State is created by the test, per repo convention: it seeds an issue, hand-claims it as an agent with a
// deliberately ancient claimed_at (far past any CLAIM_STALE_HOURS setting), then asserts the claim is
// cleared AND the claim_released system event with reason=stale is written naming the prior claimant.
func TestN9_RetentionReleasesStaleAgentClaim(t *testing.T) {
	requireStack(t)

	cronSecret := env("CRON_SECRET", "dev-only-cron-secret-change-me")

	f := newFixture(t)

	// Seed one issue via the real ingest path.
	res := f.ingest(f.newEvent().with(map[string]any{
		"error_class": "StaleClaimError",
		"message":     "held by an agent that then crashed",
	}))
	if res.Status != http.StatusAccepted {
		t.Fatalf("seeding issue: got %d: %s", res.Status, res.Body)
	}
	f.waitForIssues(1)

	issues := dashQueryIssues(t, f.ProjectID)
	if len(issues) != 1 {
		t.Fatalf("want exactly 1 seeded issue, got %d: %+v", len(issues), issues)
	}
	issueID := issues[0].ID

	// The agent whose claim we are about to strand. assigned_to is a free varchar (not an FK), so a
	// stable synthetic id is enough to exercise reapStaleClaims and to assert it back out of the
	// claim_released event's previousAssignee.
	const agentID = "n9-stale-agent"

	// Hand-claim the issue as that agent, with a claimed_at far older than any CLAIM_STALE_HOURS window
	// (the endpoint defaults to 24h). No issue_activity is written for this actor, so reapStaleClaims'
	// "recent progress" protection does not apply — the claim is unambiguously stale.
	exec(context.Background(),
		`UPDATE issues
		    SET assignee_type = 'agent', assigned_to = $2, claimed_at = now() - interval '1000 hours'
		  WHERE id = $1`,
		issueID, agentID)

	// The one real invocation — byte-for-byte the request scripts/cron-entrypoint.sh's curl issues.
	cron := dashCronRequest(t, cronSecret, true)
	if cron.Status != http.StatusOK {
		t.Fatalf("retention cron POST: got %d, want 200, body=%s", cron.Status, cron.Body)
	}

	// The claim must be gone: reapStaleClaims clears all three columns in one update.
	var assigneeType, assignedTo *string
	var claimedAt *string
	if err := pool.QueryRow(context.Background(),
		`SELECT assignee_type, assigned_to, claimed_at::text FROM issues WHERE id = $1`, issueID,
	).Scan(&assigneeType, &assignedTo, &claimedAt); err != nil {
		t.Fatalf("re-reading issue %s after retention: %v", issueID, err)
	}
	if assigneeType != nil || assignedTo != nil || claimedAt != nil {
		t.Fatalf("stale agent claim not released: assignee_type=%v assigned_to=%v claimed_at=%v (all should be NULL)",
			deref(assigneeType), deref(assignedTo), deref(claimedAt))
	}

	// The audit trail must record it: a system-actor claim_released event naming the prior claimant and
	// reason=stale. This is the exact path that had no scheduler to trigger it before N9.
	var eventCount int
	var previousAssignee, reason string
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*),
		        coalesce(max(new_value->>'previousAssignee'), ''),
		        coalesce(max(new_value->>'reason'), '')
		   FROM issue_activity
		  WHERE issue_id = $1
		    AND event_type = 'claim_released'
		    AND actor_type = 'system'
		    AND actor_id   = 'sentinel-claim-reaper'`,
		issueID,
	).Scan(&eventCount, &previousAssignee, &reason); err != nil {
		if err == pgx.ErrNoRows {
			t.Fatalf("no claim_released system event was written for issue %s", issueID)
		}
		t.Fatalf("querying claim_released activity for issue %s: %v", issueID, err)
	}
	if eventCount != 1 {
		t.Fatalf("want exactly 1 claim_released system event for issue %s, got %d", issueID, eventCount)
	}
	if previousAssignee != agentID {
		t.Fatalf("claim_released event previousAssignee = %q, want %q", previousAssignee, agentID)
	}
	if reason != "stale" {
		t.Fatalf("claim_released event reason = %q, want %q", reason, "stale")
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
