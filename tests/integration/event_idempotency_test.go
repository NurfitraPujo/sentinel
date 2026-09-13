package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
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
	"google.golang.org/protobuf/types/known/structpb"
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
	return eidIssueIn(projectID, "test", fingerprint)
}

// eidIssueIn is eidIssue with the environment named explicitly — sentinel-project-environments
// §4.9 makes (project_id, environment, fingerprint) the identity, so the environment-split tests
// need to vary the one field eidIssue pins.
func eidIssueIn(projectID, environment, fingerprint string) *store.Issue {
	now := time.Now().UTC()
	return &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Environment: environment,
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
	return eidOccurrenceIn("test", eventID)
}

func eidOccurrenceIn(environment, eventID string) *store.ErrorOccurrence {
	return &store.ErrorOccurrence{
		ID:          uuid.New().String(),
		Environment: environment,
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

// TestAffectedRollup_WireIDsLandVerbatimInIndexAndSideTables is sentinel-affected-users contract
// §11's second boundary — ProcessEvent with only a pgx pool, exactly as the test above — and the
// decision-19 / decision-3 proof on the WIRE: a UUID `user_id`, a 7-digit `tenant_id` and an email
// `user` go through Deserialize -> Normalize -> StoreEvent -> IndexOccurrence and land VERBATIM in
// error_search_index.user_id / tenant_id (flow I8) and in issue_affected_users /
// issue_affected_tenants (flow I1/I2). It sits beside the search-index test above because that is
// the one place this suite already reads the index after commit.
//
// It fails at the branch point by construction: there, ExtractSearchFields reads the
// POST-normalisation map and error_search_index.user_id holds the literal "<UUID>" (contract §1's
// second fact), and nothing writes the side tables at all. The 300-byte id is the decision-5 half of
// I8: it lands as NULL in the index — absent, not a failed VARCHAR(255) insert — and the event is
// still stored.
func TestAffectedRollup_WireIDsLandVerbatimInIndexAndSideTables(t *testing.T) {
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
	}
	pool := newPostgresPool(t, cfg)
	t.Cleanup(func() { pool.Close() })

	projectName := uniqueProjectName("aff-wire")
	projectID := createTestProject(t, pool, projectName, uniqueAPIKey("aff-wire"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc := service.NewProcessorService(pool)

	const (
		uuidUser     = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
		digitsTenant = "1234567"
		emailUser    = "alice@example.com"
	)
	tooLongUser := strings.Repeat("x", 300)

	// One issue (one fingerprint), three deliveries: UUID user + 7-digit tenant, then an email under
	// the `user` key, then a 300-byte user_id.
	fp := "fp-aff-wire-" + uuid.New().String()
	deliver := func(eventID string, metadata map[string]interface{}) {
		t.Helper()
		data, err := proto.Marshal(affectedProtoEvent(t, projectName, fp, eventID, metadata))
		require.NoError(t, err)
		require.NoError(t, svc.ProcessEvent(ctx, data))
	}
	deliver("evt-aff-wire-uuid-"+uuid.New().String(), map[string]interface{}{"user_id": uuidUser, "tenant_id": digitsTenant})
	deliver("evt-aff-wire-email-"+uuid.New().String(), map[string]interface{}{"user": emailUser})
	deliver("evt-aff-wire-long-"+uuid.New().String(), map[string]interface{}{"user_id": tooLongUser})

	var issueID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1 AND fingerprint = $2`, projectID, fp).Scan(&issueID))

	// --- The index (decision 19, flow I8): the raw ids, and NULL for the unusable one.
	var indexed []struct{ user, tenant *string }
	rows, err := pool.Query(ctx,
		`SELECT s.user_id, s.tenant_id FROM error_search_index s
		   JOIN error_occurrences eo ON eo.id = s.occurrence_id
		  WHERE eo.issue_id = $1 ORDER BY eo.created_at, eo.id`, issueID)
	require.NoError(t, err)
	for rows.Next() {
		var u, tn *string
		require.NoError(t, rows.Scan(&u, &tn))
		indexed = append(indexed, struct{ user, tenant *string }{u, tn})
	}
	rows.Close()
	// assert, not require: at the branch point this is 2 (the 300-byte id fails the after-commit
	// VARCHAR(255) insert), and the verbatim-value assertions below are the ones criterion 1 wants
	// to see fail with "<UUID>".
	assert.Len(t, indexed, 3, "all three occurrences are stored AND indexed — the 300-byte id must not fail the after-commit insert")

	var userIDs, tenantIDs []string
	for _, r := range indexed {
		if r.user != nil {
			userIDs = append(userIDs, *r.user)
		}
		if r.tenant != nil {
			tenantIDs = append(tenantIDs, *r.tenant)
		}
	}
	assert.ElementsMatch(t, []string{uuidUser, emailUser}, userIDs,
		"error_search_index.user_id must hold the UUID and the email VERBATIM — \"<UUID>\" / \"<EMAIL>\" means the "+
			"index was fed from the post-normalisation map (decision 19 regression); the 300-byte id must be NULL, not present")
	assert.Equal(t, []string{digitsTenant}, tenantIDs,
		"error_search_index.tenant_id must hold the 7-digit id VERBATIM — \"<NUMERIC_ID>\" means it was normalised")

	// --- The side tables (decision 3, flows I1/I2): the same verbatim values, the 300-byte id absent.
	var sideUsers []string
	rows, err = pool.Query(ctx, `SELECT user_id FROM issue_affected_users WHERE issue_id = $1 ORDER BY user_id`, issueID)
	require.NoError(t, err)
	for rows.Next() {
		var u string
		require.NoError(t, rows.Scan(&u))
		sideUsers = append(sideUsers, u)
	}
	rows.Close()
	assert.Equal(t, []string{uuidUser, emailUser}, sideUsers,
		"issue_affected_users must hold the UUID and the email as those EXACT strings, and no row for the 300-byte id")

	var sideTenants []string
	rows, err = pool.Query(ctx, `SELECT tenant_id FROM issue_affected_tenants WHERE issue_id = $1`, issueID)
	require.NoError(t, err)
	for rows.Next() {
		var tn string
		require.NoError(t, rows.Scan(&tn))
		sideTenants = append(sideTenants, tn)
	}
	rows.Close()
	assert.Equal(t, []string{digitsTenant}, sideTenants)

	var userCount, tenantCount, occCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT affected_user_count, affected_tenant_count, count FROM issues WHERE id = $1`, issueID).
		Scan(&userCount, &tenantCount, &occCount))
	assert.Equal(t, 2, userCount, "two distinct usable users (UUID, email); the 300-byte one is skipped")
	assert.Equal(t, 1, tenantCount)
	assert.Equal(t, 3, occCount, "all three occurrences counted — an unusable id never fails the event")
}

// affectedProtoEvent builds a valid proto event for projectKey carrying a fixed client fingerprint
// (so several deliveries share one issue), an explicit event_id and the given metadata. It is
// buildValidProtoEvent's shape with the three things the rollup tests need to control.
func affectedProtoEvent(t *testing.T, projectKey, fp, eventID string, metadata map[string]interface{}) *sentinelv1.ErrorEvent {
	t.Helper()
	protoEvent := buildValidProtoEvent(projectKey)
	protoEvent.Fingerprint = fp
	protoEvent.EventId = eventID
	md, err := structpb.NewStruct(metadata)
	require.NoError(t, err)
	protoEvent.Metadata = md
	return protoEvent
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

// ---------------------------------------------------------------------------------------------------
// sentinel-project-environments §4.9 — the identity split, and §4.8's registry touch
// ---------------------------------------------------------------------------------------------------

// TestStoreEvent_EnvironmentSplitsIssueIdentity is criterion 2's StoreEvent half. The SAME error
// class and the SAME frames delivered to `production` and to `staging` must land as TWO `issues`
// rows with independent `status` — resolving one must leave the other open.
//
// The fingerprints differ here because §4.9 also folds the environment into the hash, which is what
// a real delivery does; the test that proves the COLUMN rather than the hash is
// TestStoreEvent_CustomFingerprintStillSplitsOnEnvironment below, where the fingerprint is
// deliberately identical in both environments.
func TestStoreEvent_EnvironmentSplitsIssueIdentity(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "envsplit")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	frames := []fingerprint.StackFrame{
		{File: "checkout.go", Line: 88, Function: "checkout.Charge", InApp: true},
	}
	fpProd := fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production", ErrorClass: "PaymentError", Stacktrace: frames,
	})
	fpStag := fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "staging", ErrorClass: "PaymentError", Stacktrace: frames,
	})
	require.NotEqual(t, fpProd, fpStag, "§4.9 folds the environment into the hash input")

	prodIssue := eidIssueIn(projectID, "production", fpProd)
	_, storedProd, err := s.StoreEvent(ctx, prodIssue, eidOccurrenceIn("production", "evt-envsplit-prod"), "")
	require.NoError(t, err)
	require.True(t, storedProd)

	stagIssue := eidIssueIn(projectID, "staging", fpStag)
	_, storedStag, err := s.StoreEvent(ctx, stagIssue, eidOccurrenceIn("staging", "evt-envsplit-stag"), "")
	require.NoError(t, err)
	require.True(t, storedStag)

	// Counted by the two fingerprints the SAME class and frames produced, which is the whole claim:
	// eidIssueIn writes its own fixed error_class, so counting on that would measure the fixture.
	var rowCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issues WHERE project_id = $1 AND fingerprint = ANY($2)`,
		projectID, []string{fpProd, fpStag}).Scan(&rowCount))
	assert.Equal(t, 2, rowCount, "one error class in two environments must be two issue rows")
	assert.NotEqual(t, prodIssue.ID, stagIssue.ID)

	// Independent status is the payoff of the whole decision: resolving in staging must leave
	// production open, with no extra column and no aggregate anywhere.
	_, err = pool.Exec(ctx, `UPDATE issues SET status = 'resolved' WHERE id = $1`, stagIssue.ID)
	require.NoError(t, err)

	var prodStatus, stagStatus string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM issues WHERE id = $1`, prodIssue.ID).Scan(&prodStatus))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status FROM issues WHERE id = $1`, stagIssue.ID).Scan(&stagStatus))
	assert.Equal(t, "unresolved", prodStatus, "resolving in staging must not resolve production")
	assert.Equal(t, "resolved", stagStatus)
}

// TestStoreEvent_CustomFingerprintStillSplitsOnEnvironment is the assertion that fails if the
// migration's UNIQUE (project_id, environment, fingerprint) is missing or if either ON CONFLICT
// clause still names (project_id, fingerprint). A client-chosen fingerprint is returned verbatim by
// fingerprint.Compute in EVERY environment (§4.9 leaves that early return alone), so the identical
// string arrives from two environments — and the column, not the hash, is what makes them two rows.
func TestStoreEvent_CustomFingerprintStillSplitsOnEnvironment(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "customfp")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The one fingerprint a client chose, sent from both environments.
	shared := eidFingerprint(t, "customfp")

	prodIssue := eidIssueIn(projectID, "production", shared)
	_, storedProd, err := s.StoreEvent(ctx, prodIssue, eidOccurrenceIn("production", "evt-customfp-prod"), "")
	require.NoError(t, err)
	require.True(t, storedProd)

	stagIssue := eidIssueIn(projectID, "staging", shared)
	_, storedStag, err := s.StoreEvent(ctx, stagIssue, eidOccurrenceIn("staging", "evt-customfp-stag"), "")
	require.NoError(t, err,
		"a second environment on the same fingerprint must INSERT, not fail and not update the first row")
	require.True(t, storedStag)

	var rowCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, shared).Scan(&rowCount))
	assert.Equal(t, 2, rowCount, "one custom fingerprint in two environments must still be two issue rows")

	var prodCount, stagCount int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count FROM issues WHERE project_id = $1 AND environment = 'production' AND fingerprint = $2`,
		projectID, shared).Scan(&prodCount))
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count FROM issues WHERE project_id = $1 AND environment = 'staging' AND fingerprint = $2`,
		projectID, shared).Scan(&stagCount))
	assert.Equal(t, int64(1), prodCount, "the staging delivery must not have bumped production's count")
	assert.Equal(t, int64(1), stagCount)
}

// TestStoreEvent_RedeliveryInOneEnvironmentAdvancesNeitherCounter is criterion 2's third clause,
// asserted against the widened conflict target: a redelivered event (same issue, same event_id)
// must advance neither `last_seen` nor `count`, and must not touch the OTHER environment's row
// either.
func TestStoreEvent_RedeliveryInOneEnvironmentAdvancesNeitherCounter(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "envredeliver")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	shared := eidFingerprint(t, "envredeliver")
	eventID := "evt-envredeliver-" + uuid.New().String()

	_, stored1, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", shared),
		eidOccurrenceIn("production", eventID), "")
	require.NoError(t, err)
	require.True(t, stored1)

	var lastSeenBefore time.Time
	var countBefore int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen, count FROM issues WHERE project_id = $1 AND environment = 'production' AND fingerprint = $2`,
		projectID, shared).Scan(&lastSeenBefore, &countBefore))

	_, stored2, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", shared),
		eidOccurrenceIn("production", eventID), "")
	require.NoError(t, err, "a redelivery ACKs as a no-op, it does not error")
	assert.False(t, stored2)

	var lastSeenAfter time.Time
	var countAfter int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen, count FROM issues WHERE project_id = $1 AND environment = 'production' AND fingerprint = $2`,
		projectID, shared).Scan(&lastSeenAfter, &countAfter))

	assert.Equal(t, countBefore, countAfter, "a redelivery must not advance count")
	assert.WithinDuration(t, lastSeenBefore, lastSeenAfter, 0, "a redelivery must not advance last_seen")
}

// TestStoreEvent_RedeliveryWritesNoAffectedRowNoBumpNoMetric is sentinel-affected-users flow I5,
// beside the redelivery test above: a redelivered (issue_id, event_id) is rolled back at step 3 of
// StoreEvent, BEFORE the rollup of step 4 runs — so it writes no side row, bumps neither count,
// moves neither last_seen, and (decision 8's "never reached" property, the same one the registry
// touch holds) records nothing on sentinel_affected_rollup_skipped_total even when the redelivery
// carries an unusable id, because no rollback can un-increment a counter.
func TestStoreEvent_RedeliveryWritesNoAffectedRowNoBumpNoMetric(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "affredeliver")
	reader := eidManualReader()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "affredeliver")
	eventID := "evt-affredeliver-" + uuid.New().String()
	t1 := time.Now().UTC().Truncate(time.Second)

	first := eidOccurrenceIn("production", eventID)
	first.CreatedAt = t1
	first.AffectedUserID = "user-affredeliver"
	first.AffectedTenantID = "tenant-affredeliver"
	issue := eidIssueIn(projectID, "production", fp)
	_, stored1, err := s.StoreEvent(ctx, issue, first, "")
	require.NoError(t, err)
	require.True(t, stored1)

	skippedBefore := collectAffectedSkipped(t, ctx, reader)

	// The redelivery: same event_id, a LATER timestamp (so a leaked write would be visible on
	// last_seen), the same user, and a tenant that decision 5 would drop — a leaked metric would be
	// visible on {tenant, empty}.
	redelivered := eidOccurrenceIn("production", eventID)
	redelivered.CreatedAt = t1.Add(time.Hour)
	redelivered.AffectedUserID = "user-affredeliver"
	redelivered.AffectedTenantID = ""
	redelivered.AffectedTenantSkipReason = obs.AffectedRollupReasonEmpty
	_, stored2, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), redelivered, "")
	require.NoError(t, err, "a redelivery ACKs as a no-op, it does not error")
	require.False(t, stored2)

	var userCount, tenantCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT affected_user_count, affected_tenant_count FROM issues WHERE id = $1`, issue.ID).
		Scan(&userCount, &tenantCount))
	assert.Equal(t, 1, userCount, "a redelivery must not bump affected_user_count")
	assert.Equal(t, 1, tenantCount, "a redelivery must not bump affected_tenant_count")

	var userRows, tenantRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_affected_users WHERE issue_id = $1`, issue.ID).Scan(&userRows))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_affected_tenants WHERE issue_id = $1`, issue.ID).Scan(&tenantRows))
	assert.Equal(t, 1, userRows)
	assert.Equal(t, 1, tenantRows)

	var lastSeen time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen FROM issue_affected_users WHERE issue_id = $1 AND user_id = 'user-affredeliver'`, issue.ID).Scan(&lastSeen))
	assert.WithinDuration(t, t1, lastSeen, 0, "a redelivery must not move the side row's last_seen — the rollup is never reached")

	assert.Equal(t, skippedBefore, collectAffectedSkipped(t, ctx, reader),
		"%s must not move on a redelivery: the counter is recorded after commit and a duplicate never commits", obs.MetricAffectedRollupSkipped)
}

// TestStoreEvent_RegistryTouchUpdatesDeclaredEnvironmentOnly covers contract §4.8's registry touch,
// all four of its properties in one place because each of them fails SILENTLY:
//
//	declared    -> first_seen_at and last_seen_at are set, inside the same transaction
//	undeclared  -> zero rows updated, and the event is STILL stored (decision 9)
//	redelivery  -> last_seen_at does not move, because the UPDATE is never reached
//	no INSERT   -> ingest creates no registry row for an environment nobody declared (decision 8)
func TestStoreEvent_RegistryTouchUpdatesDeclaredEnvironmentOnly(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "registry")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Only `production` is declared. `shadow` deliberately is not.
	_, err := pool.Exec(ctx,
		`INSERT INTO project_environments (project_id, name, display_name) VALUES ($1, 'production', 'Production')`,
		projectID)
	require.NoError(t, err)

	// --- declared: both timestamps are set by the first delivery.
	_, stored, err := s.StoreEvent(ctx,
		eidIssueIn(projectID, "production", eidFingerprint(t, "registry-prod")),
		eidOccurrenceIn("production", "evt-registry-prod-1"), "")
	require.NoError(t, err)
	require.True(t, stored)

	var firstSeenAt, lastSeenAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT first_seen_at, last_seen_at FROM project_environments WHERE project_id = $1 AND name = 'production'`,
		projectID).Scan(&firstSeenAt, &lastSeenAt))
	require.NotNil(t, firstSeenAt, "first_seen_at must be set by the first touch")
	require.NotNil(t, lastSeenAt, "last_seen_at must be set by the first touch")
	firstTouch := *firstSeenAt

	// --- redelivery: the UPDATE is never reached, so last_seen_at does not move.
	redeliveredEventID := "evt-registry-prod-dup"
	fpDup := eidFingerprint(t, "registry-dup")
	_, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fpDup),
		eidOccurrenceIn("production", redeliveredEventID), "")
	require.NoError(t, err)
	require.True(t, stored)

	var beforeRedelivery time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen_at FROM project_environments WHERE project_id = $1 AND name = 'production'`,
		projectID).Scan(&beforeRedelivery))

	_, storedDup, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fpDup),
		eidOccurrenceIn("production", redeliveredEventID), "")
	require.NoError(t, err)
	require.False(t, storedDup, "the second delivery of the same (issue, event_id) is a duplicate")

	var afterRedelivery time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen_at FROM project_environments WHERE project_id = $1 AND name = 'production'`,
		projectID).Scan(&afterRedelivery))
	assert.WithinDuration(t, beforeRedelivery, afterRedelivery, 0,
		"a deduplicated redelivery must not advance last_seen_at (flow F2)")

	// first_seen_at is COALESCEd, so it never moves after the first touch.
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT first_seen_at FROM project_environments WHERE project_id = $1 AND name = 'production'`,
		projectID).Scan(&firstSeenAt))
	assert.WithinDuration(t, firstTouch, *firstSeenAt, 0, "first_seen_at is COALESCEd and never moves")

	// --- undeclared: zero rows updated is not an error, the event is stored, and NO row is created.
	_, storedShadow, err := s.StoreEvent(ctx,
		eidIssueIn(projectID, "shadow", eidFingerprint(t, "registry-shadow")),
		eidOccurrenceIn("shadow", "evt-registry-shadow-1"), "")
	require.NoError(t, err, "an unregistered environment must never fail ingest (decision 9)")
	assert.True(t, storedShadow, "the occurrence is stored regardless of the registry")

	var registryRows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM project_environments WHERE project_id = $1`, projectID).Scan(&registryRows))
	assert.Equal(t, 1, registryRows,
		"ingest must TOUCH the registry and never create a row (decision 8) — only §4.3 creates rows")
}

// ---------------------------------------------------------------------------------------------------
// BUG-12: the registry touch never fails ingest, and it is counted either way
// ---------------------------------------------------------------------------------------------------

// collectUnlabelledCounter snapshots the cumulative value of an UNLABELLED int64 counter by name. A
// family the binary has not recorded to yet is 0, not an error — the §12 instruments are created at
// procmetrics init but emit nothing until their first Add.
func collectUnlabelledCounter(t *testing.T, ctx context.Context, reader *sdkmetric.ManualReader, name string) int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))

	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "expected %s to be an int64 Sum, got %T", name, m.Data)
			for _, dp := range sum.DataPoints {
				total += dp.Value
			}
		}
	}
	return total
}

// breakRegistryTouch installs a BEFORE UPDATE trigger on project_environments that raises for
// projectID and nothing else, so the §4.8 UPDATE — and only that statement, for only this test's
// project — errors against a REAL Postgres. It is dropped again by t.Cleanup, and returned as a
// function so a test can drop it early and prove the failure was not sticky.
func breakRegistryTouch(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) (restore func()) {
	t.Helper()

	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION eid_registry_touch_boom() RETURNS trigger AS $fn$
		BEGIN
			IF NEW.project_id::text = %s THEN
				RAISE EXCEPTION 'simulated project_environments touch failure (BUG-12)';
			END IF;
			RETURN NEW;
		END;
		$fn$ LANGUAGE plpgsql;
	`, quoteSQLLiteral(projectID)))
	require.NoError(t, err, "creating the touch-failure trigger function")

	_, err = pool.Exec(ctx, `
		CREATE TRIGGER eid_registry_touch_boom
		BEFORE UPDATE ON project_environments
		FOR EACH ROW EXECUTE FUNCTION eid_registry_touch_boom()
	`)
	require.NoError(t, err, "creating the touch-failure trigger")

	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS eid_registry_touch_boom ON project_environments`)
		_, _ = pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS eid_registry_touch_boom()`)
	}
	t.Cleanup(drop)
	return drop
}

// quoteSQLLiteral single-quotes a value for interpolation into DDL, which takes no bind parameters.
func quoteSQLLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// TestStoreEvent_RegistryTouchFailureNeverDropsTheEvent is BUG-12, and the regression fence for
// BUG-19 (procmetrics.RecordEnvironmentRegistryTouchFailure "has zero callers": the failure-counter
// assertions below go red when the store's call is removed — verified by mutation, not by
// inspection). §4.8 point 1 says "ingest never
// fails because of the registry" without qualification, and §12 describes a failing touch as
// first_seen_at/last_seen_at drifting from reality — a counted, logged degradation, NOT a lost
// event. Returning the UPDATE's error from StoreEvent aborted the transaction and dropped the
// occurrence AND the issue with it, which is the exact inversion those two sections forbid.
//
// The touch stays inside the transaction (property 2); a SAVEPOINT is what makes an erroring
// statement survivable there, since Postgres otherwise poisons the whole transaction.
func TestStoreEvent_RegistryTouchFailureNeverDropsTheEvent(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "touchfail")
	reader := eidManualReader()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// `production` IS declared, so the UPDATE matches a row and the trigger below actually fires.
	_, err := pool.Exec(ctx,
		`INSERT INTO project_environments (project_id, name, display_name) VALUES ($1, 'production', 'Production')`,
		projectID)
	require.NoError(t, err)

	dropTrigger := breakRegistryTouch(t, ctx, pool, projectID)

	failuresBefore := collectUnlabelledCounter(t, ctx, reader, obs.MetricEnvironmentRegistryTouchFailures)

	fp := eidFingerprint(t, "touchfail")
	eventID := "evt-touchfail-" + uuid.New().String()

	issue := eidIssueIn(projectID, "production", fp)
	outcome, stored, err := s.StoreEvent(ctx, issue, eidOccurrenceIn("production", eventID), "")
	require.NoError(t, err, "a failing registry touch must never surface as an ingest error (§4.8 point 1)")
	assert.True(t, stored, "the occurrence must still be stored when the registry touch fails")
	assert.Equal(t, store.IssueOutcomeNew, outcome, "the issue is still a new issue")

	// The occurrence committed.
	var occCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1 AND i.environment = 'production' AND i.fingerprint = $2 AND eo.event_id = $3`,
		projectID, fp, eventID).Scan(&occCount))
	assert.Equal(t, 1, occCount, "the occurrence row must be committed, not rolled back with the touch")

	// The issue committed.
	var issueCount int64
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count FROM issues WHERE project_id = $1 AND environment = 'production' AND fingerprint = $2`,
		projectID, fp).Scan(&issueCount))
	assert.Equal(t, int64(1), issueCount, "the issue row must be committed with count = 1")

	// The failure is counted — this is the signal §12 alerts on.
	failuresAfter := collectUnlabelledCounter(t, ctx, reader, obs.MetricEnvironmentRegistryTouchFailures)
	assert.Equal(t, failuresBefore+1, failuresAfter,
		"%s must increment once per failed touch", obs.MetricEnvironmentRegistryTouchFailures)

	// And the drift §12 names is exactly what happened: the timestamps did not move.
	var lastSeenAt *time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen_at FROM project_environments WHERE project_id = $1 AND name = 'production'`,
		projectID).Scan(&lastSeenAt))
	assert.Nil(t, lastSeenAt, "a failed touch leaves last_seen_at behind reality — that is the accepted cost")

	// The transaction was not left poisoned for the NEXT delivery: with the trigger gone, the very
	// same path touches the registry normally again.
	dropTrigger()

	fp2 := eidFingerprint(t, "touchfail-recovered")
	_, stored2, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp2),
		eidOccurrenceIn("production", "evt-touchfail-ok-"+uuid.New().String()), "")
	require.NoError(t, err)
	require.True(t, stored2)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen_at FROM project_environments WHERE project_id = $1 AND name = 'production'`,
		projectID).Scan(&lastSeenAt))
	require.NotNil(t, lastSeenAt, "once the touch succeeds again last_seen_at moves")

	assert.Equal(t, failuresBefore+1, collectUnlabelledCounter(t, ctx, reader, obs.MetricEnvironmentRegistryTouchFailures),
		"a successful touch must not increment the failure counter")
}

// TestStoreEvent_UnregisteredEnvironmentIsCounted is the other half of BUG-12: decision 9 pairs
// "accept the event anyway" with a counter, and the UPDATE's tag was being discarded with `_`, so
// RowsAffected was never inspected and sentinel_unregistered_environment_events_total had no caller
// anywhere in the tree.
func TestStoreEvent_UnregisteredEnvironmentIsCounted(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "unregistered")
	reader := eidManualReader()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx,
		`INSERT INTO project_environments (project_id, name, display_name) VALUES ($1, 'production', 'Production')`,
		projectID)
	require.NoError(t, err)

	// A REGISTERED environment does not move the counter.
	before := collectUnlabelledCounter(t, ctx, reader, obs.MetricUnregisteredEnvironmentEvents)
	_, stored, err := s.StoreEvent(ctx,
		eidIssueIn(projectID, "production", eidFingerprint(t, "unreg-declared")),
		eidOccurrenceIn("production", "evt-unreg-declared-"+uuid.New().String()), "")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, before, collectUnlabelledCounter(t, ctx, reader, obs.MetricUnregisteredEnvironmentEvents),
		"a declared environment is registered — %s must not move", obs.MetricUnregisteredEnvironmentEvents)

	// An UNREGISTERED one is stored (decision 9) AND counted.
	_, storedShadow, err := s.StoreEvent(ctx,
		eidIssueIn(projectID, "shadow", eidFingerprint(t, "unreg-shadow")),
		eidOccurrenceIn("shadow", "evt-unreg-shadow-"+uuid.New().String()), "")
	require.NoError(t, err, "an unregistered environment must never fail ingest (decision 9)")
	require.True(t, storedShadow, "the occurrence is stored regardless of the registry")
	assert.Equal(t, before+1, collectUnlabelledCounter(t, ctx, reader, obs.MetricUnregisteredEnvironmentEvents),
		"%s must increment once per stored occurrence in an undeclared environment", obs.MetricUnregisteredEnvironmentEvents)
}

// ---------------------------------------------------------------------------------------------------
// sentinel-affected-users §4.3 — the affected users / tenants rollup (flows I1–I8)
// ---------------------------------------------------------------------------------------------------
//
// Contract §11, boundary 1: "store.StoreEvent through this file (a real Postgres via the suite's
// TestMain)" — the feature's ingest properties are TRANSACTION properties and this is the highest
// boundary that still sees a rollback. I5 sits beside the redelivery test above and I8 beside the
// search-index test; the rest are here.

// collectAffectedSkipped snapshots sentinel_affected_rollup_skipped_total as a map keyed
// "<kind>/<reason>". A family the binary has not recorded to yet is an empty map, not an error.
func collectAffectedSkipped(t *testing.T, ctx context.Context, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))

	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != obs.MetricAffectedRollupSkipped {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "expected %s to be an int64 Sum, got %T", obs.MetricAffectedRollupSkipped, m.Data)
			for _, dp := range sum.DataPoints {
				kind, _ := dp.Attributes.Value(attribute.Key(obs.LabelKind))
				reason, _ := dp.Attributes.Value(attribute.Key(obs.LabelReason))
				out[kind.AsString()+"/"+reason.AsString()] += dp.Value
			}
		}
	}
	return out
}

// affectedCounts reads the two projection columns for issueID.
func affectedCounts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID string) (users, tenants int) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT affected_user_count, affected_tenant_count FROM issues WHERE id = $1`, issueID).Scan(&users, &tenants))
	return users, tenants
}

// affectedUserRow reads one issue_affected_users row; found=false when there is none.
func affectedUserRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID, userID string) (firstSeen, lastSeen time.Time, found bool) {
	t.Helper()
	err := pool.QueryRow(ctx,
		`SELECT first_seen, last_seen FROM issue_affected_users WHERE issue_id = $1 AND user_id = $2`,
		issueID, userID).Scan(&firstSeen, &lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return firstSeen, lastSeen, false
	}
	require.NoError(t, err)
	return firstSeen, lastSeen, true
}

// eidOccurrenceAffected is eidOccurrenceIn carrying the rollup's two identifiers and an explicit
// created_at (decision 9: the side-table timestamps come from it, never from now()).
func eidOccurrenceAffected(environment, eventID, userID, tenantID string, at time.Time) *store.ErrorOccurrence {
	occ := eidOccurrenceIn(environment, eventID)
	occ.CreatedAt = at
	occ.AffectedUserID = userID
	occ.AffectedTenantID = tenantID
	return occ
}

// TestAffectedRollup_VerbatimIDsLandInSideTablesThroughStoreEvent is the decision-3 test at the
// StoreEvent boundary (flows I1, I2): a UUID user_id, a 7-digit tenant_id and an email user land in
// issue_affected_users / issue_affected_tenants as those EXACT strings; the count is bumped once per
// NEW pair and never again; the second event from a known user moves last_seen (GREATEST) and
// leaves first_seen alone (LEAST); an out-of-order older event moves first_seen back. Timestamps are
// the occurrence's created_at, never now().
//
// (This test cannot compile at the branch point — ErrorOccurrence has no Affected* fields there —
// which is why the wire-driven twin, TestAffectedRollup_WireIDsLandVerbatimInIndexAndSideTables,
// is the one run against the branch point for criterion 1.)
func TestAffectedRollup_VerbatimIDsLandInSideTablesThroughStoreEvent(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "affverbatim")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const (
		uuidUser     = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
		digitsTenant = "1234567"
		emailUser    = "alice@example.com"
	)
	fp := eidFingerprint(t, "affverbatim")
	t1 := time.Now().UTC().Truncate(time.Second)
	t2 := t1.Add(10 * time.Minute)
	t0 := t1.Add(-10 * time.Minute)

	// Event 1: the UUID user and the 7-digit tenant, at t1.
	issue := eidIssueIn(projectID, "production", fp)
	_, stored, err := s.StoreEvent(ctx, issue,
		eidOccurrenceAffected("production", "evt-affverbatim-1-"+uuid.New().String(), uuidUser, digitsTenant, t1), "")
	require.NoError(t, err)
	require.True(t, stored)

	users, tenants := affectedCounts(t, ctx, pool, issue.ID)
	assert.Equal(t, 1, users, "a new user bumps affected_user_count once")
	assert.Equal(t, 1, tenants, "a new tenant bumps affected_tenant_count once")

	firstSeen, lastSeen, found := affectedUserRow(t, ctx, pool, issue.ID, uuidUser)
	require.True(t, found, "the UUID must land in issue_affected_users as that EXACT string, not \"<UUID>\"")
	assert.WithinDuration(t, t1, firstSeen, 0, "first_seen is the occurrence's created_at (decision 9)")
	assert.WithinDuration(t, t1, lastSeen, 0)

	var tenantID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT tenant_id FROM issue_affected_tenants WHERE issue_id = $1`, issue.ID).Scan(&tenantID))
	assert.Equal(t, digitsTenant, tenantID, "the 7-digit tenant must land verbatim, not \"<NUMERIC_ID>\"")

	// Event 2: the SAME user and tenant, later (t2): no bump, last_seen moves, first_seen stays.
	_, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
		eidOccurrenceAffected("production", "evt-affverbatim-2-"+uuid.New().String(), uuidUser, digitsTenant, t2), "")
	require.NoError(t, err)
	require.True(t, stored)

	users, tenants = affectedCounts(t, ctx, pool, issue.ID)
	assert.Equal(t, 1, users, "a known user must NOT bump the count again — bumped only on a new pair")
	assert.Equal(t, 1, tenants)

	firstSeen, lastSeen, found = affectedUserRow(t, ctx, pool, issue.ID, uuidUser)
	require.True(t, found)
	assert.WithinDuration(t, t1, firstSeen, 0, "first_seen must not move forward")
	assert.WithinDuration(t, t2, lastSeen, 0, "last_seen must move to the second event's created_at")

	var userRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_affected_users WHERE issue_id = $1`, issue.ID).Scan(&userRows))
	assert.Equal(t, 1, userRows, "one row per DISTINCT (issue, user) pair")

	// Event 3: an email under `user`, at t1: a second distinct user, so one more bump.
	_, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
		eidOccurrenceAffected("production", "evt-affverbatim-3-"+uuid.New().String(), emailUser, "", t1), "")
	require.NoError(t, err)
	require.True(t, stored)

	users, tenants = affectedCounts(t, ctx, pool, issue.ID)
	assert.Equal(t, 2, users, "a second distinct user bumps once more")
	assert.Equal(t, 1, tenants, "an event with no tenant id leaves affected_tenant_count alone")
	_, _, found = affectedUserRow(t, ctx, pool, issue.ID, emailUser)
	assert.True(t, found, "the email must land in issue_affected_users as that EXACT string, not \"<EMAIL>\"")

	// Event 4: the UUID user again with an OLDER timestamp (t0) — client clocks: first_seen moves
	// back (LEAST), last_seen stays at t2 (GREATEST).
	_, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp),
		eidOccurrenceAffected("production", "evt-affverbatim-4-"+uuid.New().String(), uuidUser, "", t0), "")
	require.NoError(t, err)
	require.True(t, stored)

	firstSeen, lastSeen, _ = affectedUserRow(t, ctx, pool, issue.ID, uuidUser)
	assert.WithinDuration(t, t0, firstSeen, 0, "an out-of-order older event moves first_seen back (LEAST)")
	assert.WithinDuration(t, t2, lastSeen, 0, "and leaves last_seen at the latest (GREATEST)")

	users, _ = affectedCounts(t, ctx, pool, issue.ID)
	assert.Equal(t, 2, users)

	// §4.6's invariant, stated once: the count IS the side table's cardinality.
	var mismatched int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM issues i
		  WHERE i.id = $1
		    AND (affected_user_count   <> (SELECT count(*) FROM issue_affected_users   a WHERE a.issue_id = i.id)
		      OR affected_tenant_count <> (SELECT count(*) FROM issue_affected_tenants a WHERE a.issue_id = i.id))`,
		issue.ID).Scan(&mismatched))
	assert.Equal(t, 0, mismatched, "affected_*_count must equal the side table's cardinality after every commit")
}

// TestAffectedRollup_NoIDWritesNothing is flow I3: an event with neither identifier runs no
// side-table statement, moves no count, records no metric, and is stored exactly as today.
func TestAffectedRollup_NoIDWritesNothing(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "affnoid")
	reader := eidManualReader()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	skippedBefore := collectAffectedSkipped(t, ctx, reader)

	issue := eidIssueIn(projectID, "production", eidFingerprint(t, "affnoid"))
	_, stored, err := s.StoreEvent(ctx, issue, eidOccurrenceIn("production", "evt-affnoid-"+uuid.New().String()), "")
	require.NoError(t, err)
	require.True(t, stored, "an event with no ids is stored as today")

	users, tenants := affectedCounts(t, ctx, pool, issue.ID)
	assert.Equal(t, 0, users)
	assert.Equal(t, 0, tenants)

	var userRows, tenantRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_affected_users WHERE issue_id = $1`, issue.ID).Scan(&userRows))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_affected_tenants WHERE issue_id = $1`, issue.ID).Scan(&tenantRows))
	assert.Equal(t, 0, userRows)
	assert.Equal(t, 0, tenantRows)

	assert.Equal(t, skippedBefore, collectAffectedSkipped(t, ctx, reader),
		"no key at all increments nothing — only a PRESENT unusable value is counted (decision 24)")
}

// TestAffectedRollup_SameUserTwoEnvironmentsTwoRows is flow I6: identity is per environment by
// construction (decision 7 — an issue IS one (project, environment, fingerprint)), so one user seen
// in two environments is two issues, two side rows and two counts of 1. No cross-environment total.
func TestAffectedRollup_SameUserTwoEnvironmentsTwoRows(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "affenv")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	shared := eidFingerprint(t, "affenv") // a client-chosen fingerprint, identical in both environments
	const user = "usr_7712"
	now := time.Now().UTC().Truncate(time.Second)

	prod := eidIssueIn(projectID, "production", shared)
	_, stored, err := s.StoreEvent(ctx, prod, eidOccurrenceAffected("production", "evt-affenv-prod-"+uuid.New().String(), user, "", now), "")
	require.NoError(t, err)
	require.True(t, stored)

	stag := eidIssueIn(projectID, "staging", shared)
	_, stored, err = s.StoreEvent(ctx, stag, eidOccurrenceAffected("staging", "evt-affenv-stag-"+uuid.New().String(), user, "", now), "")
	require.NoError(t, err)
	require.True(t, stored)
	require.NotEqual(t, prod.ID, stag.ID, "two environments are two issues (§4.9)")

	var rowsForUser int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issue_affected_users a JOIN issues i ON i.id = a.issue_id
		  WHERE i.project_id = $1 AND a.user_id = $2`, projectID, user).Scan(&rowsForUser))
	assert.Equal(t, 2, rowsForUser, "one user in two environments is two side rows, one per issue")

	prodUsers, _ := affectedCounts(t, ctx, pool, prod.ID)
	stagUsers, _ := affectedCounts(t, ctx, pool, stag.ID)
	assert.Equal(t, 1, prodUsers)
	assert.Equal(t, 1, stagUsers)
}

// TestAffectedRollup_ConcurrentSameNewUserBumpsOnce is flow I7 and the lock-order claim of §4.3
// under a real race: N deliveries for one issue, each a DISTINCT event_id (so none is a duplicate),
// all carrying the same NEW user, in parallel. The issues row lock is taken first by every
// transaction and held to commit, so they serialise before touching the side table; the first
// upsert INSERTs (xmax = 0, bump), every later one hits DO UPDATE (inserted = false, no bump).
// Exactly one row, exactly one bump, N occurrences — and no 40P01, which would be a lock-order
// regression.
func TestAffectedRollup_ConcurrentSameNewUserBumpsOnce(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "affrace")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "affrace")
	const (
		user = "usr-race-" + "7c9e6679"
		n    = 8
	)
	now := time.Now().UTC().Truncate(time.Second)

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
				eidOccurrenceAffected("production", fmt.Sprintf("evt-affrace-%d-%s", i, uuid.New().String()), user, "", now), "")
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

	var issueID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1 AND environment = 'production' AND fingerprint = $2`,
		projectID, fp).Scan(&issueID))

	users, _ := affectedCounts(t, ctx, pool, issueID)
	assert.Equal(t, 1, users, "the same new user delivered %d times concurrently must bump exactly once", n)

	var userRows, occRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_affected_users WHERE issue_id = $1`, issueID).Scan(&userRows))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1`, issueID).Scan(&occRows))
	assert.Equal(t, 1, userRows, "exactly one side row")
	assert.Equal(t, n, occRows)
}

// TestAffectedRollup_UnusableIDsStoreTheEventAndCountTheSkip is flow I4 / be.md AC 6 at the wire
// boundary (ProcessEvent with a pgx pool only): a 300-byte id (too_long), an empty id (empty) and an
// id with a control character (control_char) each STORE the event, write no side row, leave the
// counts at 0, land as NULL in error_search_index, and increment
// sentinel_affected_rollup_skipped_total{kind,reason} with exactly that reason — read through the
// same ManualReader the D-e invariant test uses, as before/after deltas. An empty string at a
// higher-precedence key beside a good lower one is `empty`, not the good value (decision 24).
//
// The control character is \x01, not NUL: the occurrence's METADATA is written to a jsonb column,
// and jsonb rejects \u0000 outright ("unsupported Unicode escape sequence") — so an event carrying
// NUL anywhere in its metadata fails at the occurrence insert today, before the rollup is a factor.
// That is a pre-existing property of storing metadata as jsonb and not this feature's; NUL is
// covered by the unit test of event.BoundAffectedID, which is the function that classifies it.
func TestAffectedRollup_UnusableIDsStoreTheEventAndCountTheSkip(t *testing.T) {
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
	}
	pool := newPostgresPool(t, cfg)
	t.Cleanup(func() { pool.Close() })

	projectName := uniqueProjectName("aff-skip")
	projectID := createTestProject(t, pool, projectName, uniqueAPIKey("aff-skip"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })
	reader := eidManualReader()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc := service.NewProcessorService(pool)
	fp := "fp-aff-skip-" + uuid.New().String()

	before := collectAffectedSkipped(t, ctx, reader)

	deliver := func(metadata map[string]interface{}) {
		t.Helper()
		data, err := proto.Marshal(affectedProtoEvent(t, projectName, fp, "evt-aff-skip-"+uuid.New().String(), metadata))
		require.NoError(t, err)
		require.NoError(t, svc.ProcessEvent(ctx, data), "an unusable id must never fail the event")
	}
	deliver(map[string]interface{}{"user_id": strings.Repeat("x", 300), "tenant_id": ""})              // user/too_long, tenant/empty
	deliver(map[string]interface{}{"user_id": "", "userId": "u1", "tenant_id": "acme\x01"})            // user/empty (decision 24), tenant/control_char
	deliver(map[string]interface{}{"user_id": "usr\x01", "organization_id": strings.Repeat("y", 256)}) // user/control_char, tenant/too_long

	after := collectAffectedSkipped(t, ctx, reader)
	delta := func(kind, reason string) int64 { return after[kind+"/"+reason] - before[kind+"/"+reason] }
	assert.Equal(t, int64(1), delta(obs.AffectedKindUser, obs.AffectedRollupReasonTooLong), "user/too_long")
	assert.Equal(t, int64(1), delta(obs.AffectedKindUser, obs.AffectedRollupReasonEmpty), "user/empty — the empty user_id wins the precedence over userId")
	assert.Equal(t, int64(1), delta(obs.AffectedKindUser, obs.AffectedRollupReasonControlChar), "user/control_char")
	assert.Equal(t, int64(1), delta(obs.AffectedKindTenant, obs.AffectedRollupReasonEmpty), "tenant/empty")
	assert.Equal(t, int64(1), delta(obs.AffectedKindTenant, obs.AffectedRollupReasonControlChar), "tenant/control_char")
	assert.Equal(t, int64(1), delta(obs.AffectedKindTenant, obs.AffectedRollupReasonTooLong), "tenant/too_long")

	var issueID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1 AND fingerprint = $2`, projectID, fp).Scan(&issueID))

	var occRows, userRows, tenantRows, indexedNonNull int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1`, issueID).Scan(&occRows))
	assert.Equal(t, 3, occRows, "all three events are STORED — dropped id, kept event")

	users, tenants := affectedCounts(t, ctx, pool, issueID)
	assert.Equal(t, 0, users, "no usable user id, so the count stays 0 (\"0\" means no usable id, not no user)")
	assert.Equal(t, 0, tenants)
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_affected_users WHERE issue_id = $1`, issueID).Scan(&userRows))
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_affected_tenants WHERE issue_id = $1`, issueID).Scan(&tenantRows))
	assert.Equal(t, 0, userRows, "an unusable value writes no side row — never truncated into a phantom user")
	assert.Equal(t, 0, tenantRows)

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_search_index s JOIN error_occurrences eo ON eo.id = s.occurrence_id
		  WHERE eo.issue_id = $1 AND (s.user_id IS NOT NULL OR s.tenant_id IS NOT NULL)`, issueID).Scan(&indexedNonNull))
	assert.Equal(t, 0, indexedNonNull, "an unusable id is NULL in error_search_index (flow I8), never the value and never \"u1\"")
}
