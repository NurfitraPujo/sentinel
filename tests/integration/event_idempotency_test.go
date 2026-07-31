package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// This file is W3 of docs/plans/IDEMPOTENCY_PLAN.md (P9-3): the (a)-(g) proof suite for the
// event_id-keyed atomic write path (D-c/D-b/D-e), against a REAL Postgres via testcontainers — no
// mocks. Every test in this file MUST be run with FORCE_TESTCONTAINERS=1 (see CLAUDE.md's "Working
// conventions" — the compose Postgres is the SHARED dev database, and this file both drives
// concurrency and mutates issue/occurrence state directly).
//
// (a) same event twice sequentially: exactly one occurrence, count=1, second call reports stored=false.
// (b) N goroutines racing the same event: exactly one occurrence, count=1 — proves the issues-ROW LOCK
//     ordering (F-TX-2), not the unique index (that is what (a) and the sequential-redelivery case prove).
// (c) different event_id, same fingerprint: two distinct occurrences, count=2 (dedup is scoped, not global).
// (d) two DISTINCT events with an EMPTY event_id, same fingerprint: both must still store (F-TX-1's guard
//     — a dropped NULLIF silently destroys the pre-W0/legacy population), and both rows have event_id
//     IS NULL, never ''.
// (e) a resolved issue regressed by the SAME event_id delivered twice: regression_count increments
//     exactly once and exactly one issue_activity 'regressed' row exists — the COMMIT-instead-of-ROLLBACK
//     mutation is visible ONLY here (F-TP-8c).
// (f) the alert frequency counter and error_search_index are both gated on stored==true (D-d): a
//     duplicate must not feed either.
// (g) the outcome-sum invariant (D-e: stored+duplicate == deliveries) and the READ COMMITTED isolation
//     level D-c requires explicitly (F-TX-3).

// ---------------------------------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------------------------------

// newEIDStoreAndPoolFromEnv mirrors newServiceAndPoolFromEnv (processor_service_test.go) but returns
// the bare store.IssueStore, for the (a)-(e)/(g) tests that drive StoreEvent directly rather than
// through the full ProcessorService.
func newEIDStoreAndPoolFromEnv(t *testing.T) (store.IssueStore, *pgxpool.Pool) {
	t.Helper()
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
	}
	pool := newPostgresPool(t, cfg)
	t.Cleanup(func() { pool.Close() })
	return store.NewStore(pool), pool
}

// eidIssue returns a fresh Issue seed for projectID/fingerprint. A fresh Go-side ID is used on every
// call deliberately: StoreEvent resolves the real row by (project_id, fingerprint) and RETURNS its own
// id, mirroring exactly how a real redelivery — a NEW proto message, no shared Go state — behaves.
func eidIssue(projectID, fingerprint string) *store.Issue {
	now := time.Now().UTC()
	return &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Fingerprint: fingerprint,
		Message:     "event idempotency test error",
		ErrorClass:  "EventIdempotencyError",
		Status:      "unresolved",
		FirstSeen:   now,
		LastSeen:    now,
	}
}

// eidOccurrence returns a fresh ErrorOccurrence seed carrying eventID as its idempotency key ("" means
// "no key", D-b's NULLIF mapping).
func eidOccurrence(eventID string) *store.ErrorOccurrence {
	return &store.ErrorOccurrence{
		ID:          uuid.New().String(),
		Environment: "test",
		Platform:    "go",
		Stacktrace:  json.RawMessage(`[]`),
		Metadata:    json.RawMessage(`{}`),
		TraceID:     "trace-eid",
		SpanID:      "span-eid",
		CreatedAt:   time.Now().UTC(),
		EventID:     eventID,
	}
}

func eidFingerprint(t *testing.T, tag string) string {
	t.Helper()
	return fmt.Sprintf("fp-eid-%s-%d", tag, time.Now().UnixNano())
}

func eidSeedProject(t *testing.T, pool *pgxpool.Pool, tag string) string {
	t.Helper()
	projectID := createTestProject(t, pool, uniqueProjectName("eid-"+tag), uniqueAPIKey("eid-"+tag))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })
	return projectID
}

// ---------------------------------------------------------------------------------------------------
// (a) same event twice sequentially
// ---------------------------------------------------------------------------------------------------

func TestEventIdempotency_SameEventTwiceSequentially(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "a")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "a")
	eventID := "evt-eid-a-" + uuid.New().String()

	issue1 := eidIssue(projectID, fingerprint)
	_, stored1, err1 := s.StoreEvent(ctx, issue1, eidOccurrence(eventID), "")
	require.NoError(t, err1, "first delivery must not error")
	assert.True(t, stored1, "first delivery must store")

	issue2 := eidIssue(projectID, fingerprint)
	_, stored2, err2 := s.StoreEvent(ctx, issue2, eidOccurrence(eventID), "")
	require.NoError(t, err2, "a duplicate delivery must return a nil error, not fail (it ACKs as a no-op)")
	assert.False(t, stored2, "second delivery of the same (issue,event_id) must report stored=false")

	var occCount int
	var issueCount int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1`, issue1.ID).Scan(&occCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count FROM issues WHERE project_id = $1 AND fingerprint = $2`, projectID, fingerprint).Scan(&issueCount))

	assert.Equal(t, 1, occCount, "exactly one occurrence after two identical deliveries")
	assert.Equal(t, int64(1), issueCount, "issues.count must not double-increment on a duplicate (S18)")
}

// ---------------------------------------------------------------------------------------------------
// (b) N goroutines racing the same event
// ---------------------------------------------------------------------------------------------------

// TestEventIdempotency_ConcurrentSameEventOnlyOneStores proves the issues-ROW-LOCK serialization
// (F-TX-2), not the unique index: 8 deliveries of the SAME event race each other, all bottleneck on the
// `issues` row's ON CONFLICT DO UPDATE lock, and only the one that commits first ever reaches the
// occurrence insert with RowsAffected() > 0.
func TestEventIdempotency_ConcurrentSameEventOnlyOneStores(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "b")

	fingerprint := eidFingerprint(t, "b")
	eventID := "evt-eid-b-" + uuid.New().String()

	const n = 8
	var wg sync.WaitGroup
	var storedCount, errCount int64

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, stored, err := s.StoreEvent(ctx, eidIssue(projectID, fingerprint), eidOccurrence(eventID), "")
			if err != nil {
				atomic.AddInt64(&errCount, 1)
				t.Errorf("concurrent StoreEvent returned an unexpected error: %v", err)
				return
			}
			if stored {
				atomic.AddInt64(&storedCount, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(0), atomic.LoadInt64(&errCount), "no goroutine should ever see an error")
	assert.Equal(t, int64(1), atomic.LoadInt64(&storedCount), "exactly one of the 8 racers must store")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var occCount int
	var issueCount int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1 AND i.fingerprint = $2`, projectID, fingerprint).Scan(&occCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count FROM issues WHERE project_id = $1 AND fingerprint = $2`, projectID, fingerprint).Scan(&issueCount))

	assert.Equal(t, 1, occCount, "exactly one occurrence after 8 racing identical deliveries")
	assert.Equal(t, int64(1), issueCount, "issues.count must stay 1 — a split tx boundary lets every racer's issue bump commit independently (mutation 5)")
}

// ---------------------------------------------------------------------------------------------------
// (c) different event_id, same fingerprint
// ---------------------------------------------------------------------------------------------------

func TestEventIdempotency_DifferentEventIDSameFingerprintBothStore(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "c")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "c")

	_, stored1, err1 := s.StoreEvent(ctx, eidIssue(projectID, fingerprint), eidOccurrence("evt-eid-c-1"), "")
	require.NoError(t, err1)
	assert.True(t, stored1)

	_, stored2, err2 := s.StoreEvent(ctx, eidIssue(projectID, fingerprint), eidOccurrence("evt-eid-c-2"), "")
	require.NoError(t, err2)
	assert.True(t, stored2, "a DIFFERENT event_id on the same issue must store — dedup is scoped to (issue_id, event_id), not to the issue alone")

	var occCount int
	var issueCount int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1 AND i.fingerprint = $2`, projectID, fingerprint).Scan(&occCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count FROM issues WHERE project_id = $1 AND fingerprint = $2`, projectID, fingerprint).Scan(&issueCount))

	assert.Equal(t, 2, occCount)
	assert.Equal(t, int64(2), issueCount)
}

// ---------------------------------------------------------------------------------------------------
// (d) two DISTINCT events with an EMPTY event_id, same fingerprint — the F-TX-1 guard
// ---------------------------------------------------------------------------------------------------

// TestEventIdempotency_EmptyEventIDNeverDedupsAndStoresAsNull is D-b's tripwire: without the NULLIF
// mapping at the insert site, proto3's "" (its only representation of "absent") would enter the
// partial unique index as a real value, and every empty-event_id delivery after the first on one issue
// would be silently discarded as a false duplicate — the exact loss F-TX-1 found against the 72h
// JetStream window and the 30-day DLQ population of pre-W0 messages.
func TestEventIdempotency_EmptyEventIDNeverDedupsAndStoresAsNull(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "d")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "d")

	_, stored1, err1 := s.StoreEvent(ctx, eidIssue(projectID, fingerprint), eidOccurrence(""), "")
	require.NoError(t, err1)
	assert.True(t, stored1, "first empty-event_id delivery must store")

	_, stored2, err2 := s.StoreEvent(ctx, eidIssue(projectID, fingerprint), eidOccurrence(""), "")
	require.NoError(t, err2)
	assert.True(t, stored2, "a SECOND distinct empty-event_id delivery on the same issue must ALSO store — "+
		"'' is not a key, it is the absence of one, and must never be treated as a dedup match")

	var occCount int
	var issueCount int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1 AND i.fingerprint = $2`, projectID, fingerprint).Scan(&occCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count FROM issues WHERE project_id = $1 AND fingerprint = $2`, projectID, fingerprint).Scan(&issueCount))
	assert.Equal(t, 2, occCount)
	assert.Equal(t, int64(2), issueCount)

	rows, err := pool.Query(ctx,
		`SELECT eo.event_id FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1 AND i.fingerprint = $2`, projectID, fingerprint)
	require.NoError(t, err)
	defer rows.Close()

	var seen int
	for rows.Next() {
		var eventID *string
		require.NoError(t, rows.Scan(&eventID))
		assert.Nil(t, eventID, "an absent event_id must be stored as SQL NULL, never the empty string — "+
			"the CHECK constraint (23514) is the tripwire if a future edit binds '' directly")
		seen++
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, 2, seen)
}

// ---------------------------------------------------------------------------------------------------
// (e) regression exactly-once
// ---------------------------------------------------------------------------------------------------

// TestEventIdempotency_RegressionRecordedExactlyOnceForADuplicateID is the ONLY place the
// COMMIT-instead-of-ROLLBACK mutation is visible (F-TP-8c): a resolved issue is regressed by TWO
// deliveries of the identical event_id, and regression_count/issue_activity must show the regression
// exactly once, not twice.
//
// This MUST race, and the interleaving is DELIBERATELY forced rather than left to goroutine scheduling
// luck. A purely sequential pair (deliver, then redeliver) never re-enters the regression branch on the
// second call: the first delivery's commit already flips status to 'unresolved', so the second
// delivery's own unlocked SELECT sees "not resolved" and takes the ordinary ON CONFLICT DO UPDATE arm
// instead — which never touches regression_count at all, making regression_count insensitive to the
// COMMIT/ROLLBACK choice regardless of which one the code takes. An UNSYNCHRONIZED two-goroutine race
// (mirroring (b)'s mechanism) was tried first and does not reliably hit the window either: against a
// local container, StoreEvent's whole 4-5-statement transaction completes in well under a millisecond,
// so an unsynchronized second goroutine's first statement usually does not even start before the first
// has already committed — three consecutive attempts against the actual COMMIT mutation stayed green.
//
// So: the FIRST regressing transaction's exact SQL (copied from store.go's regression arm) is driven
// manually on a held-open, uncommitted raw connection. The REAL, second s.StoreEvent call for the
// identical event_id is then launched concurrently — its regression UPDATE targets the same row
// (`WHERE id = $1`) and blocks on the lock the manual transaction holds. Only once that call is
// confirmed to still be in flight (a full 500ms after starting it — several orders of magnitude past
// what even a heavily loaded local container needs to reach and block on the same row) is the manual
// transaction committed, unblocking it: Postgres re-evaluates the (id-keyed, therefore still-matching)
// UPDATE against the now-committed row and reapplies `regression_count = regression_count + 1` on top
// of the manual transaction's own +1 — precisely the double-application D-c's rollback exists to undo,
// and precisely what the COMMIT-instead-of-ROLLBACK mutation leaves standing.
func TestEventIdempotency_RegressionRecordedExactlyOnceForADuplicateID(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "e")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "e")

	// Seed at 1.0.0 via a first, ordinary delivery, then resolve it there — mirrors
	// tests/e2e/harness_test.go's resolve() semantics: resolved_in_version must be non-NULL or every
	// later occurrence looks like a regression regardless of version comparison.
	issue0 := eidIssue(projectID, fingerprint)
	_, stored0, err0 := s.StoreEvent(ctx, issue0, eidOccurrence("evt-eid-e-seed"), "1.0.0")
	require.NoError(t, err0)
	require.True(t, stored0)

	tag, err := pool.Exec(ctx,
		`UPDATE issues SET status = 'resolved', resolved_at = now(), resolved_in_version = '1.0.0',
		        resolved_by_type = 'user', resolved_by = 'integration-test'
		  WHERE id = $1`, issue0.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	regressEventID := "evt-eid-e-regress-" + uuid.New().String()

	// --- Manual "first" regressing transaction, held open ---
	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()

	tx1, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	require.NoError(t, err)
	committed1 := false
	defer func() {
		if !committed1 {
			_ = tx1.Rollback(context.Background())
		}
	}()

	_, err = tx1.Exec(ctx,
		`UPDATE issues SET
			status = 'unresolved', regression_status = 'regressed',
			regression_count = regression_count + 1, last_regressed_at = now(),
			resolved_in_version = NULL, resolved_at = NULL, resolved_by_type = NULL, resolved_by = NULL,
			last_seen = GREATEST(issues.last_seen, now()), count = issues.count + 1
		  WHERE id = $1`, issue0.ID)
	require.NoError(t, err, "manual first regression UPDATE")

	_, err = tx1.Exec(ctx,
		`INSERT INTO issue_activity (id, issue_id, actor_type, actor_id, event_type, new_value, created_at)
		 VALUES (gen_random_uuid(), $1, 'system', 'sentinel-regression-detector', 'regressed', '{}'::jsonb, now())`,
		issue0.ID)
	require.NoError(t, err, "manual first activity insert")

	_, err = tx1.Exec(ctx,
		`INSERT INTO error_occurrences (issue_id, environment, platform, created_at, event_id)
		 VALUES ($1, 'test', 'go', now(), $2)`, issue0.ID, regressEventID)
	require.NoError(t, err, "manual first occurrence insert")

	// --- The REAL, concurrent second delivery of the identical event_id ---
	type result struct {
		stored bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		gctx, gcancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer gcancel()
		_, stored, err := s.StoreEvent(gctx, eidIssue(projectID, fingerprint), eidOccurrence(regressEventID), "2.0.0")
		done <- result{stored, err}
	}()

	// POSITIVE observation that the concurrent StoreEvent has reached the row lock and is blocking on
	// tx1, before tx1 commits. A fixed sleep here is a silent-green flake seed (F-VW3-2): if the
	// goroutine's unlocked SELECT lands after the commit on a loaded runner, it reads
	// status='unresolved', never enters the regression arm, and every assertion below passes
	// REGARDLESS of the COMMIT-vs-ROLLBACK behavior under test — this exact vacuity was observed in a
	// weaker draft of this test. Polling pg_locks turns "assume it blocked" into "proved it blocked";
	// only then is committing tx1 guaranteed to release a waiter that has already read
	// status='resolved' and taken the regression branch.
	blockDeadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting))
		if waiting >= 1 {
			break
		}
		if time.Now().After(blockDeadline) {
			t.Fatal("the concurrent StoreEvent never blocked on tx1's row lock — the interleaving this " +
				"test exists to force did not occur, and its assertions would be vacuous")
		}
		time.Sleep(20 * time.Millisecond)
	}

	require.NoError(t, tx1.Commit(ctx))
	committed1 = true

	var got result
	select {
	case got = <-done:
	case <-time.After(25 * time.Second):
		t.Fatal("the concurrent StoreEvent call never returned after the blocking transaction committed")
	}
	require.NoError(t, got.err)
	assert.False(t, got.stored, "the concurrent delivery must detect (issue_id, event_id) as already "+
		"stored (by the manual transaction) and report stored=false")

	var regressionCount int
	var status string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT regression_count, status FROM issues WHERE id = $1`, issue0.ID).Scan(&regressionCount, &status))
	assert.Equal(t, 1, regressionCount, "regression_count must record the regression exactly once — a "+
		"COMMIT on the duplicate's rollback path would double it")
	assert.Equal(t, "unresolved", status)

	var activityCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issue_activity WHERE issue_id = $1 AND event_type = 'regressed'`,
		issue0.ID).Scan(&activityCount))
	assert.Equal(t, 1, activityCount, "exactly one 'regressed' issue_activity row, not one per delivery")

	var occCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1`, issue0.ID).Scan(&occCount))
	assert.Equal(t, 2, occCount, "the seed occurrence plus the ONE stored regressing occurrence — the duplicate must not add a second row")
}

// ---------------------------------------------------------------------------------------------------
// (f) alert gate + search index stay correct across a duplicate
// ---------------------------------------------------------------------------------------------------

// TestEventIdempotency_DuplicateDoesNotFeedAlertsOrSearchIndex is F-TP-4's execution proof: D-d gates
// dispatchAlert and IndexOccurrence on stored==true, so a duplicate delivery must not move the alert
// frequency counter or add a second error_search_index row. The capturing-sender pattern mirrors
// procgo_alerting_degradation_test.go:389 (TestProcgoAlertingDispatchesWithinOneEvent).
func TestEventIdempotency_DuplicateDoesNotFeedAlertsOrSearchIndex(t *testing.T) {
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
	}
	pool := newPostgresPool(t, cfg)
	t.Cleanup(func() { pool.Close() })

	projectName := uniqueProjectName("eid-f")
	apiKey := uniqueAPIKey("eid-f")
	projectID := createTestProject(t, pool, projectName, apiKey)
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var orgID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT organization_id FROM projects WHERE id = $1`, projectID).Scan(&orgID))

	_, err := pool.Exec(ctx,
		`INSERT INTO alert_configs (project_id, organization_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled)
		 VALUES ($1, $2, 'email', '{"to": "oncall@example.com"}'::jsonb, 2, 60, true)`,
		projectID, orgID)
	require.NoError(t, err)

	// service.NewProcessorService performs alerts.NewDispatcher's synchronous initial config load AT
	// CONSTRUCTION (VERIFIED_STATE.md S8 item 4, and procgo_alerting_degradation_test.go's identical
	// ordering requirement) — so the row inserted above must exist BEFORE this call, not after, or the
	// dispatcher starts with an empty config set and no alert ever fires regardless of what StoreEvent
	// does.
	svc := service.NewProcessorService(pool)

	type captured struct {
		cfg   alerts.AlertConfig
		alert alerts.Alert
	}
	var mu sync.Mutex
	var got []captured
	svc.Alerts().SetSender(func(_ context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, captured{*cfg, *alert})
	})

	sharedEventID := "evt-eid-f-" + uuid.New().String()

	first := buildValidProtoEvent(projectName)
	first.EventId = sharedEventID
	data1, mErr := proto.Marshal(first)
	require.NoError(t, mErr)
	require.NoError(t, svc.ProcessEvent(ctx, data1))

	mu.Lock()
	n0 := len(got)
	mu.Unlock()
	assert.Equal(t, 0, n0, "one occurrence at frequency_threshold=2 must not dispatch yet")

	// Redeliver the SAME event_id: a duplicate.
	dup := buildValidProtoEvent(projectName)
	dup.EventId = sharedEventID
	data2, mErr := proto.Marshal(dup)
	require.NoError(t, mErr)
	require.NoError(t, svc.ProcessEvent(ctx, data2))

	mu.Lock()
	n1 := len(got)
	mu.Unlock()
	assert.Equal(t, 0, n1, "the duplicate must NOT feed the alert frequency counter — it stays at 0, not 1")

	var issueID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1`, projectID).Scan(&issueID))

	var searchCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_search_index
		  WHERE occurrence_id IN (SELECT id FROM error_occurrences WHERE issue_id = $1)`,
		issueID).Scan(&searchCount))
	// Honesty note (F-VW3-3): this assertion is enforced by the SCHEMA, not by D-d's gate — under a
	// neutered stored-gate the duplicate's IndexOccurrence targets an occurrence id whose row was
	// rolled back, so error_search_index's FK on occurrence_id rejects it and the count stays 1 either
	// way (proven under mutation: row 6 left this line green while the alert assertion above went
	// red). The alert-counter assertion above is what actually forces D-d; this line documents that
	// the search index cannot drift even if the gate regresses, courtesy of the FK.
	assert.Equal(t, 1, searchCount, "the duplicate must not add a second error_search_index row "+
		"(enforced by error_search_index's occurrence_id FK; see the comment above)")

	// A third, DISTINCT occurrence reaches the threshold.
	third := buildValidProtoEvent(projectName)
	third.EventId = "evt-eid-f-fresh-" + uuid.New().String()
	data3, mErr := proto.Marshal(third)
	require.NoError(t, mErr)
	require.NoError(t, svc.ProcessEvent(ctx, data3))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1, "the third DISTINCT occurrence must reach frequency_threshold=2 and dispatch exactly one alert")
	assert.Equal(t, projectID, got[0].alert.ProjectID)
}

// ---------------------------------------------------------------------------------------------------
// (g) outcome-sum invariant + isolation level
// ---------------------------------------------------------------------------------------------------

// TestEventIdempotency_OutcomeSumInvariantAndIsolationLevel proves D-e's metric invariant
// (stored+duplicate deltas sum to the number of deliveries) using a real OTel ManualReader, and
// separately documents what this test can and cannot prove about D-c's explicit READ COMMITTED
// isolation level (F-TX-3).
//
// The ManualReader works because procmetrics' instruments are obtained from otel's GLOBAL meter
// (otel.Meter("processor-go")) at package init, before this test ever runs — and the OTel Go API's
// documented global-delegation mechanism means an instrument created before otel.SetMeterProvider is
// called is transparently upgraded in place once it IS called (see procmetrics.go's own doc comment,
// and packages/shared-go/obs/provider.go's Bootstrap, which does exactly this in production).
//
// CRITICAL constraint on that mechanism, learned the hard way (F-VW3-1): the delegation resolves
// EXACTLY ONCE, to the first real MeterProvider ever installed in the process, and can never be
// re-pointed — restoring the previous global in a t.Cleanup restores the POINTER but not the
// already-resolved instruments, so a second reader installed later collects nothing and the test
// fails with a message blaming the metric. Hence eidManualReader below: ONE reader, installed once
// per test binary via sync.Once, never restored, and this test asserts BEFORE/AFTER DELTAS rather
// than absolutes — which is also what D-e literally specifies ("deltas sum to the number of
// deliveries"). This makes the test -count=N-safe and immune to ordering against any earlier
// metric-recording test. The corollary every future author must respect: NO OTHER TEST in this
// package may call otel.SetMeterProvider — if one does and runs first, the instruments delegate to
// ITS provider and this test's `found` requirement fails naming the delegation, not idempotency.
func TestEventIdempotency_OutcomeSumInvariantAndIsolationLevel(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	projectName := uniqueProjectName("eid-g")
	apiKey := uniqueAPIKey("eid-g")
	projectID := createTestProject(t, pool, projectName, apiKey)
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reader := eidManualReader()
	before := collectProcessOutcomes(t, ctx, reader)

	// 2 fresh + 1 duplicate = 3 deliveries.
	fresh1 := buildValidProtoEvent(projectName)
	fresh1.EventId = "evt-eid-g-1"
	data1, err := proto.Marshal(fresh1)
	require.NoError(t, err)
	require.NoError(t, svc.ProcessEvent(ctx, data1))

	dup := buildValidProtoEvent(projectName)
	dup.EventId = "evt-eid-g-1" // duplicate of fresh1
	data2, err := proto.Marshal(dup)
	require.NoError(t, err)
	require.NoError(t, svc.ProcessEvent(ctx, data2))

	fresh2 := buildValidProtoEvent(projectName)
	fresh2.EventId = "evt-eid-g-2"
	data3, err := proto.Marshal(fresh2)
	require.NoError(t, err)
	require.NoError(t, svc.ProcessEvent(ctx, data3))

	const deliveries = 3

	after := collectProcessOutcomes(t, ctx, reader)
	stored := after[obs.OutcomeStored] - before[obs.OutcomeStored]
	duplicate := after[obs.OutcomeDuplicate] - before[obs.OutcomeDuplicate]

	assert.Equal(t, int64(2), stored, "2 fresh deliveries must record OutcomeStored (delta)")
	assert.Equal(t, int64(1), duplicate, "1 duplicate delivery must record OutcomeDuplicate (delta)")
	assert.Equal(t, int64(deliveries), stored+duplicate,
		"D-e's invariant: stored+duplicate deltas must sum to the number of deliveries — a message "+
			"recorded in both places (or neither) breaks this")

	// --- Isolation level (F-TX-3) ---
	//
	// pgx does not expose an in-flight transaction's negotiated isolation level to a caller outside the
	// package that opened it, and StoreEvent's tx is opened and committed/rolled back entirely inside
	// store.StoreEvent — there is no seam to run `SHOW transaction_isolation` from inside THAT exact
	// transaction without adding a debug-only leak to production code, which is out of this change's
	// scope. Two things ARE honestly observable from here, and both are asserted:
	//
	//  1. A source-level check that the code actually requests pgx.ReadCommitted (not merely "whatever
	//     the default happens to be" — D-c is explicit that the isolation level must never be left
	//     implicit, because REPEATABLE READ/SERIALIZABLE throws 40001 on the exact interleaving this
	//     plan depends on, per F-TX-3's probe).
	//  2. This pool's own session-level default, which is what any BeginTx without an explicit
	//     TxOptions override would also see — context for why pgx.ReadCommitted is a no-op change on an
	//     unmodified Postgres install, not proof of what StoreEvent's own transaction negotiated.
	storeSrc, err := os.ReadFile(findEIDStoreGoPath(t))
	require.NoError(t, err, "reading apps/processor-go/store/store.go to verify the isolation level literal")
	assertStoreEventUsesReadCommitted(t, string(storeSrc))

	var isolation string
	require.NoError(t, pool.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation))
	assert.Equal(t, "read committed", isolation,
		"this pool's session-level default isolation must be read committed (context for #1 above)")
}

// eidReaderOnce/eidReaderInst back eidManualReader: ONE ManualReader for the whole test binary,
// installed as the global meter provider exactly once and never restored. See the long comment on
// TestEventIdempotency_OutcomeSumInvariantAndIsolationLevel for why restore-in-cleanup is not merely
// unnecessary but actively breaks every subsequent collection (OTel resolves instrument delegation to
// the FIRST real provider, permanently).
var (
	eidReaderOnce sync.Once
	eidReaderInst *sdkmetric.ManualReader
)

func eidManualReader() *sdkmetric.ManualReader {
	eidReaderOnce.Do(func() {
		eidReaderInst = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(eidReaderInst)))
	})
	return eidReaderInst
}

// collectProcessOutcomes snapshots sentinel_process_events_total by outcome label. A missing family is
// returned as an empty map, NOT an error: before the first ProcessEvent in the binary the counter has
// never recorded and legitimately emits nothing — while after at least one delivery the caller's
// delta assertions fail loudly if the family still is not there, which subsumes the old `found`
// requirement without being order-fragile.
func collectProcessOutcomes(t *testing.T, ctx context.Context, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))

	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != obs.MetricProcessEvents {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "expected %s to be an int64 Sum, got %T", obs.MetricProcessEvents, m.Data)
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(obs.LabelOutcome)); ok {
					out[v.AsString()] += dp.Value
				}
			}
		}
	}
	return out
}

// findEIDStoreGoPath locates apps/processor-go/store/store.go relative to the test binary's working
// directory (tests/integration), without hardcoding an absolute path.
func findEIDStoreGoPath(t *testing.T) string {
	t.Helper()
	return "../../apps/processor-go/store/store.go"
}

// assertStoreEventUsesReadCommitted greedily locates the StoreEvent function body and asserts it opens
// its transaction with the exact literal D-c specifies. A regex-free substring check on the isolated
// function body (not the whole file) so an unrelated ReadCommitted reference elsewhere in the package
// cannot produce a false pass.
func assertStoreEventUsesReadCommitted(t *testing.T, src string) {
	t.Helper()
	const marker = "func (s *pgStore) StoreEvent("
	start := indexOf(src, marker)
	require.GreaterOrEqual(t, start, 0, "could not locate StoreEvent's function body in store.go — has it been renamed?")
	body := src[start:]
	if end := indexOf(body[1:], "\nfunc "); end >= 0 {
		body = body[:end+1]
	}
	require.Contains(t, body, "pgx.TxOptions{IsoLevel: pgx.ReadCommitted}",
		"StoreEvent must open its transaction with an EXPLICIT pgx.TxOptions{IsoLevel: pgx.ReadCommitted} "+
			"(D-c/F-TX-3) — relying on the driver's default is not the same guarantee")
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Reference kept so `pgx` stays imported even if a future edit removes SHOW transaction_isolation
// above without noticing it was the only other use — pgx.ReadCommitted is D-c's literal, and this
// keeps the compiler enforcing that the identifier still resolves to the constant this test's
// source-text check is verifying against.
var _ = pgx.ReadCommitted
