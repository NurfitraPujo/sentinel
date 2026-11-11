package integration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sentinel-issue-trend contract §11 boundary 1, flows I1–I4, I6, I7 and I9: the per-(issue, UTC
// hour) occurrence bucket StoreEvent's step 4b upserts inside its own transaction (§4.3, decision
// 1), proven through store.StoreEvent against the suite's REAL Postgres with a pgx pool only —
// event_idempotency_test.go's harness, because the feature's ingest properties are transaction
// properties and only this boundary sees a rollback. I5 (a redelivery moves nothing) sits beside
// TestStoreEvent_RedeliveryInOneEnvironmentAdvancesNeitherCounter in event_idempotency_test.go, as
// the contract places it; I8 (the storm) is trend_m2_timing_test.go's correctness gate.
//
// Every test here MUST be run with FORCE_TESTCONTAINERS=1 (CLAUDE.md "Working conventions"): the
// compose Postgres is the SHARED dev database and these tests mutate issue/occurrence state
// directly.

// trendBucket is one issue_occurrence_buckets row as the readers see it.
type trendBucket struct {
	Start time.Time
	Count int
}

// trendBuckets reads every bucket row of one issue, oldest first, in UTC.
func trendBuckets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID string) []trendBucket {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT bucket_start, count FROM issue_occurrence_buckets WHERE issue_id = $1 ORDER BY bucket_start`, issueID)
	require.NoError(t, err)
	defer rows.Close()
	var out []trendBucket
	for rows.Next() {
		var b trendBucket
		require.NoError(t, rows.Scan(&b.Start, &b.Count))
		b.Start = b.Start.UTC()
		out = append(out, b)
	}
	require.NoError(t, rows.Err())
	return out
}

// trendIssueID resolves the issue StoreEvent created for (project, production, fingerprint).
func trendIssueID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, fp string) string {
	t.Helper()
	var issueID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1 AND environment = 'production' AND fingerprint = $2`,
		projectID, fp).Scan(&issueID))
	return issueID
}

// trendOccurrenceAt is eidOccurrenceIn("production", …) with the event timestamp pinned — the
// bucket clock is occ.CreatedAt (§3, decision 2), so every test here sets it explicitly.
func trendOccurrenceAt(eventID string, at time.Time) *store.ErrorOccurrence {
	occ := eidOccurrenceIn("production", eventID)
	occ.CreatedAt = at
	return occ
}

// I1 + I2: the first occurrence in an hour INSERTs the (issue, hour) row at 1; a second in the same
// UTC hour makes it 2 — one row, and the bucket agrees with issues.count and error_occurrences.
func TestIssueTrend_TwoOccurrencesInOneHourMakeOneBucketOfTwo(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "trendsamehour")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "trendsamehour")
	hour := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)

	_, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
		trendOccurrenceAt("evt-trendsamehour-1-"+uuid.New().String(), hour.Add(5*time.Minute)), "")
	require.NoError(t, err)
	require.True(t, stored)

	issueID := trendIssueID(t, ctx, pool, projectID, fp)
	require.Equal(t, []trendBucket{{Start: hour, Count: 1}}, trendBuckets(t, ctx, pool, issueID),
		"I1: the first occurrence inserts the (issue, UTC hour) row at count 1")

	_, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
		trendOccurrenceAt("evt-trendsamehour-2-"+uuid.New().String(), hour.Add(59*time.Minute+59*time.Second)), "")
	require.NoError(t, err)
	require.True(t, stored)

	assert.Equal(t, []trendBucket{{Start: hour, Count: 2}}, trendBuckets(t, ctx, pool, issueID),
		"I2: a second occurrence in the same UTC hour increments the one row to 2")

	var issueCount, occRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count FROM issues WHERE id = $1`, issueID).Scan(&issueCount))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1`, issueID).Scan(&occRows))
	assert.Equal(t, 2, issueCount, "the bucket and issues.count move together")
	assert.Equal(t, 2, occRows, "the bucket and error_occurrences agree")
}

// I3: adjacent hours are two rows of 1 — the hour boundary is exact (:59:59.999 stays in its hour,
// :00:00 opens the next).
func TestIssueTrend_AdjacentHoursMakeTwoBucketsOfOne(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "trendadjacent")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "trendadjacent")
	h14 := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	h15 := h14.Add(time.Hour)

	for i, at := range []time.Time{h14.Add(59*time.Minute + 59*time.Second + 999*time.Millisecond), h15} {
		_, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
			trendOccurrenceAt(fmt.Sprintf("evt-trendadjacent-%d-%s", i, uuid.New().String()), at), "")
		require.NoError(t, err)
		require.True(t, stored)
	}

	issueID := trendIssueID(t, ctx, pool, projectID, fp)
	assert.Equal(t, []trendBucket{{Start: h14, Count: 1}, {Start: h15, Count: 1}}, trendBuckets(t, ctx, pool, issueID),
		"I3: two occurrences in adjacent hours are two rows of count 1")
}

// I4 (contract §3's hour-truncation rule): an event stamped 2026-09-13T20:29:59+05:30 lands in
// bucket_start 2026-09-13T14:00:00Z — the UTC hour, not the wall-clock hour. The truncation is
// done in Go (occ.CreatedAt.UTC().Truncate(time.Hour)); date_trunc('hour', …) on a timestamptz in
// a +05:30 session would have produced 20:00+05:30 = 14:30Z, a bucket_start no reader's window
// arithmetic could ever hit.
func TestIssueTrend_BucketStartIsTheUTCHourNotTheWallClockHour(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "trendutc")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "trendutc")
	kolkata := time.FixedZone("IST", 5*60*60+30*60)
	at := time.Date(2026, 9, 13, 20, 29, 59, 0, kolkata) // 14:59:59Z

	_, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
		trendOccurrenceAt("evt-trendutc-"+uuid.New().String(), at), "")
	require.NoError(t, err)
	require.True(t, stored)

	issueID := trendIssueID(t, ctx, pool, projectID, fp)
	got := trendBuckets(t, ctx, pool, issueID)
	require.Len(t, got, 1)
	assert.True(t, got[0].Start.Equal(time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)),
		"I4: bucket_start must be 2026-09-13T14:00:00Z, got %s", got[0].Start.Format(time.RFC3339))
	assert.Equal(t, 1, got[0].Count)
}

// I6: an event with NO event_id (proto3's "absent", NULLIF'd to NULL — D-b) is never deduplicated,
// so delivered twice it is stored twice, and the bucket says 2 — it agrees with error_occurrences,
// which is the whole point of writing it inside the same transaction.
func TestIssueTrend_NoEventIDDeliveredTwiceCountsTwo(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "trendnoid")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "trendnoid")
	hour := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 2; i++ {
		_, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
			trendOccurrenceAt("", hour.Add(time.Duration(i)*time.Minute)), "")
		require.NoError(t, err)
		require.True(t, stored, "delivery %d: an event with no event_id always stores", i)
	}

	issueID := trendIssueID(t, ctx, pool, projectID, fp)
	assert.Equal(t, []trendBucket{{Start: hour, Count: 2}}, trendBuckets(t, ctx, pool, issueID),
		"I6: two stored occurrences, two counted")
	var occRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1 AND event_id IS NULL`, issueID).Scan(&occRows))
	assert.Equal(t, 2, occRows)
}

// I7: two concurrent deliveries for the same issue in the same hour both land and the bucket says
// 2 — the issues row lock (step 1) serialises them before either reaches step 4b, so the two
// upserts never race on the bucket row and neither can deadlock (§4.3's lock-order argument). Eight
// goroutines, not two, in the idempotency suite's style: a lost increment or a 40P01 is more
// likely to show with more contenders, and the assertion is the same.
func TestIssueTrend_ConcurrentSameIssueSameHourCountsEveryDelivery(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "trendrace")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "trendrace")
	const n = 8
	hour := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	var storedCount atomic.Int32
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
				trendOccurrenceAt(fmt.Sprintf("evt-trendrace-%d-%s", i, uuid.New().String()), hour.Add(time.Duration(i)*time.Minute)), "")
			if err != nil {
				errs <- err
				return
			}
			if stored {
				storedCount.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err, "no delivery may fail — a 40P01 here is a lock-order regression")
	}
	require.Equal(t, int32(n), storedCount.Load(), "every delivery has a distinct event_id, so all %d store", n)

	issueID := trendIssueID(t, ctx, pool, projectID, fp)
	assert.Equal(t, []trendBucket{{Start: hour, Count: n}}, trendBuckets(t, ctx, pool, issueID),
		"I7: %d concurrent same-hour deliveries are %d increments on one row", n, n)
	var occRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1`, issueID).Scan(&occRows))
	assert.Equal(t, n, occRows)
}

// I9 (decision 18): a timestamp older than the retention cutoff and one in the future each land in
// their own hour, exactly as their occurrence rows do — StoreEvent clamps nothing. The readers are
// bounded at both ends so neither row can reach a series (that is the readers' test); the sweep
// removes the old one on its next run (the retention test's). Here: both rows exist, at the hours
// the timestamps say.
func TestIssueTrend_PreCutoffAndFutureTimestampsLandInTheirOwnHours(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "trendedges")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "trendedges")
	now := time.Now().UTC()
	old := now.Add(-400 * 24 * time.Hour).Truncate(time.Hour).Add(17 * time.Minute) // far beyond any DATA_RETENTION_DAYS
	future := now.Add(3 * 24 * time.Hour).Truncate(time.Hour).Add(42 * time.Minute)

	for i, at := range []time.Time{old, future} {
		_, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
			trendOccurrenceAt(fmt.Sprintf("evt-trendedges-%d-%s", i, uuid.New().String()), at), "")
		require.NoError(t, err)
		require.True(t, stored)
	}

	issueID := trendIssueID(t, ctx, pool, projectID, fp)
	assert.Equal(t, []trendBucket{
		{Start: old.Truncate(time.Hour), Count: 1},
		{Start: future.Truncate(time.Hour), Count: 1},
	}, trendBuckets(t, ctx, pool, issueID), "I9: a pre-cutoff and a future occurrence each bucket at their own hour, nothing is clamped to now()")
}
