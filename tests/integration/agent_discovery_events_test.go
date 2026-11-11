package integration

import (
	"context"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// N7a (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md, A01/A06/R2): StoreEvent must write a
// 'created' issue_activity row exactly once for a genuinely-new issue, and a throttled
// 'occurrence_burst' row on repeat occurrences (never on the delivery that already wrote
// 'created', and never on the delivery that wrote 'regressed'). Uses the same
// FORCE_TESTCONTAINERS=1 harness as event_idempotency_test.go — see that file's header comment.

func adeStoreAndPool(t *testing.T) (store.IssueStore, *pgxpool.Pool) {
	t.Helper()
	return newEIDStoreAndPoolFromEnv(t)
}

func adeCountActivity(t *testing.T, pool *pgxpool.Pool, issueID, eventType string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM issue_activity WHERE issue_id = $1 AND event_type = $2`,
		issueID, eventType).Scan(&n))
	return n
}

// (1) a brand-new issue writes exactly one 'created' row, in the same tx as the store.
func TestAgentDiscoveryEvents_NewIssueWritesExactlyOneCreatedRow(t *testing.T) {
	s, pool := adeStoreAndPool(t)
	projectID := eidSeedProject(t, pool, "ade1")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "ade1")
	issue := eidIssue(projectID, fingerprint)

	_, stored, err := s.StoreEvent(ctx, issue, eidOccurrence("evt-ade1-"+uuid.New().String()), "")
	require.NoError(t, err)
	require.True(t, stored)

	assert.Equal(t, 1, adeCountActivity(t, pool, issue.ID, "created"),
		"a genuinely-new issue must produce exactly one 'created' issue_activity row")
	assert.Equal(t, 0, adeCountActivity(t, pool, issue.ID, "occurrence_burst"),
		"the very first delivery must not also emit a burst event")
}

// (2) a duplicate delivery of the SAME event_id (rolled back) writes no activity row at all —
// proves the 'created' insert lives inside the same rollback boundary as the rest of the tx.
func TestAgentDiscoveryEvents_DuplicateDeliveryWritesNoCreatedRow(t *testing.T) {
	s, pool := adeStoreAndPool(t)
	projectID := eidSeedProject(t, pool, "ade2")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "ade2")
	eventID := "evt-ade2-" + uuid.New().String()

	issue1 := eidIssue(projectID, fingerprint)
	_, stored1, err1 := s.StoreEvent(ctx, issue1, eidOccurrence(eventID), "")
	require.NoError(t, err1)
	require.True(t, stored1)
	require.Equal(t, 1, adeCountActivity(t, pool, issue1.ID, "created"))

	// Same event_id again: StoreEvent rolls back the whole tx (including the 'created' insert it
	// would otherwise have attempted on a fresh issue.ID) and reports stored=false.
	issue2 := eidIssue(projectID, fingerprint)
	_, stored2, err2 := s.StoreEvent(ctx, issue2, eidOccurrence(eventID), "")
	require.NoError(t, err2)
	require.False(t, stored2)

	assert.Equal(t, 1, adeCountActivity(t, pool, issue1.ID, "created"),
		"a duplicate delivery must not add a second 'created' row")
}

// (3) a resolved issue that regresses writes 'regressed', never 'created' — the isRegressed
// branch is untouched by N7a and must stay that way.
func TestAgentDiscoveryEvents_RegressionWritesRegressedNotCreated(t *testing.T) {
	s, pool := adeStoreAndPool(t)
	projectID := eidSeedProject(t, pool, "ade3")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "ade3")

	issue1 := eidIssue(projectID, fingerprint)
	_, stored1, err1 := s.StoreEvent(ctx, issue1, eidOccurrence("evt-ade3-a-"+uuid.New().String()), "")
	require.NoError(t, err1)
	require.True(t, stored1)
	require.Equal(t, 1, adeCountActivity(t, pool, issue1.ID, "created"))

	_, err := pool.Exec(ctx,
		`UPDATE issues SET status = 'resolved', resolved_in_version = '1.0.0' WHERE id = $1`, issue1.ID)
	require.NoError(t, err)

	issue2 := eidIssue(projectID, fingerprint)
	// The occurrence's release must be >= resolved_in_version: isRegressionVersion (store.go:143)
	// deliberately does NOT count an occurrence from an OLDER release than the fix as a regression.
	// "0.9.0" here (vs resolved_in 1.0.0) made this test fail on its first CI run — the product was
	// right, the fixture was wrong.
	_, stored2, err2 := s.StoreEvent(ctx, issue2, eidOccurrence("evt-ade3-b-"+uuid.New().String()), "1.1.0")
	require.NoError(t, err2)
	require.True(t, stored2)

	assert.Equal(t, 1, adeCountActivity(t, pool, issue1.ID, "regressed"),
		"a regression delivery must write exactly one 'regressed' row")
	assert.Equal(t, 1, adeCountActivity(t, pool, issue1.ID, "created"),
		"a regression delivery must not add a second 'created' row")
}

// (4) repeat occurrences beyond the throttle window each earn at most one 'occurrence_burst' row
// per window; occurrences delivered inside the same window after the first burst add none.
func TestAgentDiscoveryEvents_BurstThrottleHonoured(t *testing.T) {
	s, pool := adeStoreAndPool(t)
	projectID := eidSeedProject(t, pool, "ade4")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "ade4")

	issue1 := eidIssue(projectID, fingerprint)
	_, stored1, err1 := s.StoreEvent(ctx, issue1, eidOccurrence("evt-ade4-a-"+uuid.New().String()), "")
	require.NoError(t, err1)
	require.True(t, stored1)

	// Second occurrence, same issue: 'created' is still "recent" (default 1h throttle window in
	// this test's env), so no burst row yet.
	issue2 := eidIssue(projectID, fingerprint)
	_, stored2, err2 := s.StoreEvent(ctx, issue2, eidOccurrence("evt-ade4-b-"+uuid.New().String()), "")
	require.NoError(t, err2)
	require.True(t, stored2)

	assert.Equal(t, 0, adeCountActivity(t, pool, issue1.ID, "occurrence_burst"),
		"no burst row while a 'created'/'occurrence_burst' row is still within the throttle window")

	// Force the 'created' row to look old enough to fall outside the (default) throttle window,
	// then deliver another occurrence: exactly one burst row should appear.
	_, err := pool.Exec(ctx,
		`UPDATE issue_activity SET created_at = NOW() - INTERVAL '2 hours' WHERE issue_id = $1 AND event_type = 'created'`,
		issue1.ID)
	require.NoError(t, err)

	issue3 := eidIssue(projectID, fingerprint)
	_, stored3, err3 := s.StoreEvent(ctx, issue3, eidOccurrence("evt-ade4-c-"+uuid.New().String()), "")
	require.NoError(t, err3)
	require.True(t, stored3)

	assert.Equal(t, 1, adeCountActivity(t, pool, issue1.ID, "occurrence_burst"),
		"exactly one burst row once the throttle window has elapsed")

	// Immediately deliver a fourth occurrence: the fresh burst row is now the recent one, so no
	// second burst row should appear.
	issue4 := eidIssue(projectID, fingerprint)
	_, stored4, err4 := s.StoreEvent(ctx, issue4, eidOccurrence("evt-ade4-d-"+uuid.New().String()), "")
	require.NoError(t, err4)
	require.True(t, stored4)

	assert.Equal(t, 1, adeCountActivity(t, pool, issue1.ID, "occurrence_burst"),
		"a second burst row must not appear inside the same throttle window as the first")
}
