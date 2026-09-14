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
	"google.golang.org/protobuf/types/known/timestamppb"
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

// TestStoreEvent_RedeliveryWritesNoBucketRowNoIncrement is sentinel-issue-trend flow I5, beside
// TestStoreEvent_RedeliveryInOneEnvironmentAdvancesNeitherCounter as contract §11 places it: a
// redelivered (issue_id, event_id) is rolled back at step 3 of StoreEvent, BEFORE step 4b's bucket
// upsert runs — so the bucket table is byte-for-byte what the first delivery left: one row, count 1,
// even when the redelivery carries a LATER timestamp in a DIFFERENT hour (a leaked upsert would
// show as a second row, a leaked increment as count 2).
func TestStoreEvent_RedeliveryWritesNoBucketRowNoIncrement(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "bucketredeliver")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "bucketredeliver")
	eventID := "evt-bucketredeliver-" + uuid.New().String()
	hour := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)

	first := eidOccurrenceIn("production", eventID)
	first.CreatedAt = hour.Add(10 * time.Minute)
	issue := eidIssueIn(projectID, "production", fp)
	_, stored1, err := s.StoreEvent(ctx, issue, first, "")
	require.NoError(t, err)
	require.True(t, stored1)

	before := trendBuckets(t, ctx, pool, issue.ID)
	require.Equal(t, []trendBucket{{Start: hour, Count: 1}}, before)

	redelivered := eidOccurrenceIn("production", eventID)
	redelivered.CreatedAt = hour.Add(2 * time.Hour) // a different hour: a leaked upsert would INSERT
	_, stored2, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), redelivered, "")
	require.NoError(t, err, "a redelivery ACKs as a no-op, it does not error")
	require.False(t, stored2)

	assert.Equal(t, before, trendBuckets(t, ctx, pool, issue.ID),
		"I5: a redelivered (issue_id, event_id) is rolled back before step 4b — no row, no increment")
}

// TestStoreEvent_RedeliveryMovesNeitherReleaseNorSnooze is sentinel-releases-snooze flow I10 and
// §11 property (3), beside TestStoreEvent_RedeliveryInOneEnvironmentAdvancesNeitherCounter as the
// contract places it: a redelivered (issue_id, event_id) is rolled back at step 3 of StoreEvent,
// BEFORE step 4c's release upsert runs and UNDOING step 1's snooze bookkeeping — so the `releases`
// row is byte-for-byte what the first delivery left (count 1, last_seen unmoved even though the
// redelivery carries a LATER timestamp), the snoozed issue's count and its three snooze columns are
// untouched, it is still 'ignored', and occ.ReleaseInserted is false on the redelivery.
func TestStoreEvent_RedeliveryMovesNeitherReleaseNorSnooze(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "relredeliver")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "relredeliver")
	const release = "v7.0.0"
	t1 := time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

	// The issue exists and is snoozed one occurrence away from its target — the most sensitive
	// snooze state a leaked step-1 write could disturb.
	seed := eidOccurrenceIn("production", "evt-relredeliver-seed-"+uuid.New().String())
	seed.CreatedAt = t1
	seed.ReleaseVersion = release
	issue := eidIssueIn(projectID, "production", fp)
	_, stored, err := s.StoreEvent(ctx, issue, seed, release)
	require.NoError(t, err)
	require.True(t, stored)
	require.True(t, seed.ReleaseInserted, "the seed is the project's first sighting of the release")
	_, err = pool.Exec(ctx,
		`UPDATE issues SET status = 'ignored', snooze_occurrence_target = 3, snooze_count_at = 0 WHERE id = $1`, issue.ID)
	require.NoError(t, err) // count is 1: one more occurrence ends it (1 + 1 - 0 < 3 is false only at count 3)

	eventID := "evt-relredeliver-" + uuid.New().String()
	first := eidOccurrenceIn("production", eventID)
	first.CreatedAt = t1.Add(time.Minute)
	first.ReleaseVersion = release
	outcome1, stored1, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), first, release)
	require.NoError(t, err)
	require.True(t, stored1)
	require.Equal(t, store.IssueOutcomeIgnored, outcome1, "count 2 - 0 < 3: the snooze is still running")
	require.False(t, first.ReleaseInserted)

	before := releaseRowOf(t, ctx, pool, projectID, release)
	require.True(t, before.found)
	require.EqualValues(t, 2, before.occurrenceCount)
	snoozeBefore := issueSnoozeStateOf(t, ctx, pool, issue.ID)
	require.Equal(t, "ignored", snoozeBefore.status)
	require.EqualValues(t, 2, snoozeBefore.count)

	// The redelivery: same event_id, a LATER timestamp (a leaked upsert would move last_seen), the
	// same release (a leaked upsert would bump occurrence_count), and a count that would END the
	// snooze if step 1's arm were not rolled back (2 + 1 - 0 >= 3).
	redelivered := eidOccurrenceIn("production", eventID)
	redelivered.CreatedAt = t1.Add(2 * time.Hour)
	redelivered.ReleaseVersion = release
	outcome2, stored2, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), redelivered, release)
	require.NoError(t, err, "a redelivery ACKs as a no-op, it does not error")
	require.False(t, stored2)
	assert.Equal(t, store.IssueOutcomeExisting, outcome2, "the duplicate path reports Existing, whatever the original was")
	assert.False(t, redelivered.ReleaseInserted, "I10: a redelivery never reports a first sighting")

	assert.Equal(t, before, releaseRowOf(t, ctx, pool, projectID, release),
		"I10: a redelivered (issue_id, event_id) is rolled back before step 4c — no release bump, no last_seen move")
	assert.Equal(t, snoozeBefore, issueSnoozeStateOf(t, ctx, pool, issue.ID),
		"I10: the step-3 rollback undoes step 1 — no snooze progress, the columns and the status as they were")
	var snoozeRows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issue_activity WHERE issue_id = $1 AND actor_id = 'sentinel-snooze'`, issue.ID).Scan(&snoozeRows))
	assert.Equal(t, 0, snoozeRows, "no status_changed row survives the duplicate rollback")
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

// ---------------------------------------------------------------------------------------------------
// sentinel-releases-snooze §4.3 through StoreEvent — contract §11's first row, properties (1)–(6)
// at the store level (flows I1–I9, I12–I16, I20). One function per numbered property, on a real
// Postgres via the suite's TestMain (FORCE_TESTCONTAINERS=1). Property (3) is above, beside the
// other redelivery cases. The service-level half of (6) (the dispatchAlert gate) and (7) (the
// post-commit rewrite) are be.md task 4's and live with their ProcessorService cases.
// ---------------------------------------------------------------------------------------------------

// releaseRowState is one `releases` row as the readers see it, or found = false.
type releaseRowState struct {
	found           bool
	firstSeen       time.Time
	lastSeen        time.Time
	occurrenceCount int64
}

func releaseRowOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, version string) releaseRowState {
	t.Helper()
	var r releaseRowState
	err := pool.QueryRow(ctx,
		`SELECT first_seen, last_seen, occurrence_count FROM releases WHERE project_id = $1 AND version = $2`,
		projectID, version).Scan(&r.firstSeen, &r.lastSeen, &r.occurrenceCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return releaseRowState{}
	}
	require.NoError(t, err)
	r.found = true
	r.firstSeen, r.lastSeen = r.firstSeen.UTC(), r.lastSeen.UTC()
	return r
}

// seedReleaseRow inserts a `releases` row directly — the decision-table cases need a version with
// a chosen first_seen, which only the store's own step 4c would otherwise write.
func seedReleaseRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, version string, firstSeen time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO releases (project_id, version, first_seen, last_seen, occurrence_count) VALUES ($1, $2, $3, $3, 1)`,
		projectID, version, firstSeen)
	require.NoError(t, err)
}

// issueSnoozeState is the issue row's status, count, the three snooze columns and the regression
// columns — everything the snooze arm may or may not move.
type issueSnoozeState struct {
	status              string
	count               int64
	snoozedUntil        *time.Time
	snoozeTarget        *int64
	snoozeCountAt       *int64
	regressionStatus    *string
	regressionCount     int64
	firstReleaseVersion *string
}

func issueSnoozeStateOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID string) issueSnoozeState {
	t.Helper()
	var st issueSnoozeState
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, count, snoozed_until, snooze_occurrence_target, snooze_count_at,
		        regression_status, regression_count, first_release_version
		   FROM issues WHERE id = $1`, issueID).
		Scan(&st.status, &st.count, &st.snoozedUntil, &st.snoozeTarget, &st.snoozeCountAt,
			&st.regressionStatus, &st.regressionCount, &st.firstReleaseVersion))
	if st.snoozedUntil != nil {
		u := st.snoozedUntil.UTC()
		st.snoozedUntil = &u
	}
	return st
}

// snoozeEndedActivity is the one 'status_changed' row the snooze arm writes (decision 18), decoded.
type snoozeEndedActivity struct {
	oldValue map[string]interface{}
	newValue map[string]interface{}
}

// snoozeEndedActivityRows reads every sentinel-snooze activity row of one issue, oldest first.
func snoozeEndedActivityRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID string) []snoozeEndedActivity {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT old_value, new_value FROM issue_activity
		  WHERE issue_id = $1 AND event_type = 'status_changed' AND actor_type = 'system' AND actor_id = 'sentinel-snooze'
		  ORDER BY created_at`, issueID)
	require.NoError(t, err)
	defer rows.Close()
	var out []snoozeEndedActivity
	for rows.Next() {
		var oldRaw, newRaw []byte
		require.NoError(t, rows.Scan(&oldRaw, &newRaw))
		var a snoozeEndedActivity
		require.NoError(t, json.Unmarshal(oldRaw, &a.oldValue))
		require.NoError(t, json.Unmarshal(newRaw, &a.newValue))
		out = append(out, a)
	}
	require.NoError(t, rows.Err())
	return out
}

// assertSnoozedUntilPayload is decision 40's three-part check on new_value.snoozedUntil: it parses
// as RFC 3339, ends in `Z`, and equals the instant the row held before the arm nulled it, to the
// second (the processor writes whole seconds; the dashboard writes milliseconds — readers parse).
func assertSnoozedUntilPayload(t *testing.T, newValue map[string]interface{}, stored time.Time) {
	t.Helper()
	raw, ok := newValue["snoozedUntil"].(string)
	require.True(t, ok, "new_value.snoozedUntil must be a string when the row had a date, got %v", newValue["snoozedUntil"])
	assert.True(t, strings.HasSuffix(raw, "Z"), "decision 40: UTC with the letter, never the offset form: %q", raw)
	parsed, err := time.Parse(time.RFC3339, raw)
	require.NoError(t, err, "new_value.snoozedUntil must parse as RFC 3339: %q", raw)
	assert.True(t, parsed.Equal(stored.Truncate(time.Second)),
		"new_value.snoozedUntil %s must equal the stored snoozed_until %s to the second", parsed, stored)
}

// releaseOccurrence is eidOccurrenceIn with the release and the timestamp set — the two fields
// this feature reads (§4.3: R = occ.ReleaseVersion, the clock = occ.CreatedAt).
func releaseOccurrence(release string, at time.Time) *store.ErrorOccurrence {
	occ := eidOccurrenceIn("production", "evt-rel-"+uuid.New().String())
	occ.ReleaseVersion = release
	occ.CreatedAt = at
	return occ
}

// (1) a first occurrence with R inserts the `releases` row with first_seen = last_seen =
// occ.CreatedAt, occurrence_count = 1, and stamps first_release_version = R on the fresh issue
// (flow I1); a second in the same release makes count = 2, last_seen = GREATEST, and leaves
// first_release_version alone (flow I2). occ.ReleaseInserted reads true exactly once.
func TestStoreEvent_ReleaseFirstOccurrenceInsertsRowAndStampsIssue(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "rel1")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "rel1")
	const release = "v1.2.3"
	t1 := time.Date(2026, 9, 14, 9, 30, 15, 0, time.UTC)

	require.False(t, releaseRowOf(t, ctx, pool, projectID, release).found, "no releases row before the first sighting")

	first := releaseOccurrence(release, t1)
	issue := eidIssueIn(projectID, "production", fp)
	outcome, stored, err := s.StoreEvent(ctx, issue, first, release)
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeNew, outcome)
	assert.True(t, first.ReleaseInserted, "I1: the first occurrence carrying R is the project's first sighting")

	row := releaseRowOf(t, ctx, pool, projectID, release)
	require.True(t, row.found, "I1: one releases row for (project, R)")
	assert.True(t, row.firstSeen.Equal(t1), "first_seen = occ.CreatedAt (§3, decision 7), got %s", row.firstSeen)
	assert.True(t, row.lastSeen.Equal(t1), "last_seen = occ.CreatedAt, got %s", row.lastSeen)
	assert.EqualValues(t, 1, row.occurrenceCount)

	st := issueSnoozeStateOf(t, ctx, pool, issue.ID)
	require.NotNil(t, st.firstReleaseVersion, "I1: the fresh issue is stamped with its first release")
	assert.Equal(t, release, *st.firstReleaseVersion)

	// I2: the same release, a later timestamp, and a different (later) release on the SAME issue —
	// first_release_version is the first release, whatever the DO UPDATE arm sees.
	second := releaseOccurrence(release, t1.Add(time.Hour))
	outcome, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), second, release)
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeExisting, outcome)
	assert.False(t, second.ReleaseInserted, "I2: the DO UPDATE arm is not a first sighting")

	row = releaseRowOf(t, ctx, pool, projectID, release)
	assert.EqualValues(t, 2, row.occurrenceCount, "I2: occurrence_count = 2")
	assert.True(t, row.firstSeen.Equal(t1), "I2: first_seen does not move")
	assert.True(t, row.lastSeen.Equal(t1.Add(time.Hour)), "I2: last_seen = GREATEST")

	// An out-of-order (earlier) timestamp: GREATEST keeps last_seen where it is.
	earlier := releaseOccurrence(release, t1.Add(-time.Hour))
	_, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), earlier, release)
	require.NoError(t, err)
	require.True(t, stored)
	row = releaseRowOf(t, ctx, pool, projectID, release)
	assert.EqualValues(t, 3, row.occurrenceCount)
	assert.True(t, row.lastSeen.Equal(t1.Add(time.Hour)), "GREATEST: an earlier client timestamp never moves last_seen back")

	later := releaseOccurrence("v1.2.4", t1.Add(2*time.Hour))
	_, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), later, "v1.2.4")
	require.NoError(t, err)
	require.True(t, stored)
	assert.True(t, later.ReleaseInserted, "v1.2.4 is a first sighting of its own")
	st = issueSnoozeStateOf(t, ctx, pool, issue.ID)
	require.NotNil(t, st.firstReleaseVersion)
	assert.Equal(t, release, *st.firstReleaseVersion, "I2: the DO UPDATE arm never touches first_release_version")
	assert.EqualValues(t, 1, releaseRowOf(t, ctx, pool, projectID, "v1.2.4").occurrenceCount)
}

// (2) R = ” writes no `releases` row and stamps NULL (flow I3): step 0 and step 4c are skipped
// entirely, first_release_version is NULL on the fresh issue, and ReleaseInserted is false.
func TestStoreEvent_ReleaseEmptyVersionWritesNothing(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "rel2")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "rel2")
	occ := releaseOccurrence("", time.Now().UTC())
	issue := eidIssueIn(projectID, "production", fp)
	outcome, stored, err := s.StoreEvent(ctx, issue, occ, "")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeNew, outcome)
	assert.False(t, occ.ReleaseInserted)

	var releaseRows int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM releases WHERE project_id = $1`, projectID).Scan(&releaseRows))
	assert.Equal(t, 0, releaseRows, "I3: an empty version writes no releases row — and the table's CHECK (version <> '') would refuse one")

	st := issueSnoozeStateOf(t, ctx, pool, issue.ID)
	assert.Nil(t, st.firstReleaseVersion, "I3: NULLIF('', '') — a version-less first occurrence stamps NULL, never ''")

	// A later occurrence WITH a release on the same issue: the DO UPDATE arm still leaves the
	// NULL alone (the first release is the first release, and this issue had none).
	withRelease := releaseOccurrence("v2.0.0", time.Now().UTC())
	_, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), withRelease, "v2.0.0")
	require.NoError(t, err)
	require.True(t, stored)
	st = issueSnoozeStateOf(t, ctx, pool, issue.ID)
	assert.Nil(t, st.firstReleaseVersion, "the DO UPDATE arm never touches first_release_version, NULL included")
}

// (4) the §4.3 decision table, one case per row, through StoreEvent against a real Postgres: a
// resolved issue, the `releases` rows the row needs seeded with chosen first_seen values, one
// occurrence carrying R — and the issue either regressed (status 'unresolved', regression_count 1,
// resolved_in_version NULL, one 'regressed' activity row) or not (still 'resolved', count + 1).
// SHA versions (flows I4/I5, where today's compare reads 0 == 0), R == X (decision 13), the tie
// (decision 3) and `next` with resolved_at IS NULL (decision 27, flow I20) included. Each case owns
// a project so its release rows cannot collide.
func TestStoreEvent_ReleaseDecisionTable(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)

	monday := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	wednesday := monday.Add(48 * time.Hour)
	previousFriday := monday.Add(-72 * time.Hour)
	resolvedAt := monday.Add(12 * time.Hour)
	occurrenceAt := wednesday.Add(6 * time.Hour) // the incoming occurrence's own clock, after everything

	type seeded struct {
		version   string
		firstSeen time.Time
	}
	cases := []struct {
		name          string
		resolvedIn    *string // nil = NULL
		resolvedAt    *time.Time
		release       string // R
		releases      []seeded
		wantRegressed bool
	}{
		{name: "X NULL: any R regresses (unchanged today)", resolvedIn: nil, resolvedAt: &resolvedAt, release: "9f8e7d6", wantRegressed: true},
		{name: "R empty: regresses (unchanged today)", resolvedIn: ptrStr("a1b2c3d"), resolvedAt: &resolvedAt, release: "", releases: []seeded{{"a1b2c3d", monday}}, wantRegressed: true},
		{name: "next, rowR absent: a release first seen now is newer (decision 6; flow I12)", resolvedIn: ptrStr("next"), resolvedAt: &resolvedAt, release: "9f8e7d6", wantRegressed: true},
		{name: "next, rowR.first_seen > resolved_at (decision 6; flow I12)", resolvedIn: ptrStr("next"), resolvedAt: &resolvedAt, release: "9f8e7d6", releases: []seeded{{"9f8e7d6", wednesday}}, wantRegressed: true},
		{name: "next, rowR.first_seen <= resolved_at: R existed when the resolution was made (decisions 6, 32; flows I13, I21)", resolvedIn: ptrStr("next"), resolvedAt: &resolvedAt, release: "a1b2c3d", releases: []seeded{{"a1b2c3d", monday}}, wantRegressed: false},
		{name: "next, resolved_at NULL: a pre-feature batch resolve regresses (decision 27; flow I20)", resolvedIn: ptrStr("next"), resolvedAt: nil, release: "a1b2c3d", releases: []seeded{{"a1b2c3d", monday}}, wantRegressed: true},
		{name: "rowX absent, fallback: 1.2.3 against a typed 9.9.9 is not a regression (decision 3; flow I6)", resolvedIn: ptrStr("9.9.9"), resolvedAt: &resolvedAt, release: "1.2.3", releases: []seeded{{"1.2.3", monday}}, wantRegressed: false},
		{name: "rowX absent, fallback: 10.0.0 against a typed 9.9.9 regresses (decision 3; flow I6)", resolvedIn: ptrStr("9.9.9"), resolvedAt: &resolvedAt, release: "10.0.0", releases: []seeded{{"10.0.0", monday}}, wantRegressed: true},
		{name: "rowX present, rowR absent: a first sighting is newest, v1.2.1 after v1.3.0 (decision 14; flow I7)", resolvedIn: ptrStr("v1.3.0"), resolvedAt: &resolvedAt, release: "v1.2.1", releases: []seeded{{"v1.3.0", monday}}, wantRegressed: true},
		{name: "R == X: the fix did not land (decision 13)", resolvedIn: ptrStr("a1b2c3d"), resolvedAt: &resolvedAt, release: "a1b2c3d", releases: []seeded{{"a1b2c3d", monday}}, wantRegressed: true},
		{name: "SHA pair I4: 9f8e7d6 first seen Wednesday after a1b2c3d Monday regresses (decision 1)", resolvedIn: ptrStr("a1b2c3d"), resolvedAt: &resolvedAt, release: "9f8e7d6", releases: []seeded{{"a1b2c3d", monday}, {"9f8e7d6", wednesday}}, wantRegressed: true},
		{name: "SHA pair I5: 0000aaa first seen the previous Friday does not regress a1b2c3d (decision 1)", resolvedIn: ptrStr("a1b2c3d"), resolvedAt: &resolvedAt, release: "0000aaa", releases: []seeded{{"a1b2c3d", monday}, {"0000aaa", previousFriday}}, wantRegressed: false},
		{name: "tie on first_seen, R != X: today's compare decides, v1.2.0 over v1.1.0 (decision 3)", resolvedIn: ptrStr("v1.1.0"), resolvedAt: &resolvedAt, release: "v1.2.0", releases: []seeded{{"v1.1.0", monday}, {"v1.2.0", monday}}, wantRegressed: true},
		{name: "tie on first_seen, R != X: today's compare decides, v1.0.9 under v1.1.0 (decision 3)", resolvedIn: ptrStr("v1.1.0"), resolvedAt: &resolvedAt, release: "v1.0.9", releases: []seeded{{"v1.1.0", monday}, {"v1.0.9", monday}}, wantRegressed: false},
	}

	for i, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			projectID := eidSeedProject(t, pool, fmt.Sprintf("reltable%d", i))
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			fp := eidFingerprint(t, fmt.Sprintf("reltable%d", i))
			issue := eidIssueIn(projectID, "production", fp)
			_, stored, err := s.StoreEvent(ctx, issue, releaseOccurrence("", previousFriday.Add(-24*time.Hour)), "")
			require.NoError(t, err)
			require.True(t, stored)

			for _, r := range tc.releases {
				seedReleaseRow(t, ctx, pool, projectID, r.version, r.firstSeen)
			}
			_, err = pool.Exec(ctx,
				`UPDATE issues SET status = 'resolved', resolved_in_version = $2, resolved_at = $3,
				        resolved_by_type = 'user', resolved_by = 'integration-test'
				  WHERE id = $1`, issue.ID, tc.resolvedIn, tc.resolvedAt)
			require.NoError(t, err)

			occ := releaseOccurrence(tc.release, occurrenceAt)
			outcome, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), occ, tc.release)
			require.NoError(t, err)
			require.True(t, stored)

			var status string
			var resolvedIn *string
			var regressionCount int64
			var regressedRows int
			require.NoError(t, pool.QueryRow(ctx,
				`SELECT status, resolved_in_version, regression_count,
				        (SELECT COUNT(*) FROM issue_activity WHERE issue_id = issues.id AND event_type = 'regressed')
				   FROM issues WHERE id = $1`, issue.ID).Scan(&status, &resolvedIn, &regressionCount, &regressedRows))
			if tc.wantRegressed {
				assert.Equal(t, store.IssueOutcomeRegressed, outcome)
				assert.Equal(t, "unresolved", status)
				assert.Nil(t, resolvedIn, "the regression UPDATE nulls resolved_in_version")
				assert.EqualValues(t, 1, regressionCount)
				assert.Equal(t, 1, regressedRows, "one 'regressed' activity row")
			} else {
				assert.Equal(t, store.IssueOutcomeExisting, outcome, "a resolved, non-regressing recurrence is Existing (decision 29)")
				assert.Equal(t, "resolved", status)
				require.NotNil(t, resolvedIn)
				assert.Equal(t, *tc.resolvedIn, *resolvedIn, "not a regression: the resolution stands")
				assert.EqualValues(t, 0, regressionCount)
				assert.Equal(t, 0, regressedRows)
			}
			if tc.release != "" {
				row := releaseRowOf(t, ctx, pool, projectID, tc.release)
				require.True(t, row.found, "step 4c runs whatever step 1 decided")
				assert.True(t, row.lastSeen.Equal(occurrenceAt), "last_seen = GREATEST(seeded, occ.CreatedAt)")
			}
		})
	}
}

func ptrStr(s string) *string { return &s }

// (5) the snooze arm on an occurrence target: `ignored` with target 100 and count_at 500 flips on
// exactly 599 → 600 (flows I8, I9). 598 → 599 stays 'ignored' with the columns untouched and the
// outcome Ignored; 599 → 600 is 'unresolved', the three columns NULL, regression columns untouched
// (decision 17), the outcome Unsnoozed, and ONE 'status_changed' row by sentinel-snooze whose
// new_value carries reason, the three PRE-null values (snoozedUntil null — only a target was set)
// and the committed count 600 — not the snapshot's 599 (decision 18).
func TestStoreEvent_SnoozeOccurrenceTargetEndsOnExactCount(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "snooze5a")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "snooze5a")
	issue := eidIssueIn(projectID, "production", fp)
	_, stored, err := s.StoreEvent(ctx, issue, releaseOccurrence("v3.0.0", time.Now().UTC()), "v3.0.0")
	require.NoError(t, err)
	require.True(t, stored)
	_, err = pool.Exec(ctx,
		`UPDATE issues SET status = 'ignored', snooze_occurrence_target = 100, snooze_count_at = 500, count = 598 WHERE id = $1`, issue.ID)
	require.NoError(t, err)

	// 598 → 599: (599 - 500) = 99 < 100. Still parked.
	outcome, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), releaseOccurrence("v3.0.0", time.Now().UTC()), "v3.0.0")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeIgnored, outcome, "I8: RETURNING status says 'ignored' — the snooze still runs")
	st := issueSnoozeStateOf(t, ctx, pool, issue.ID)
	assert.Equal(t, "ignored", st.status)
	assert.EqualValues(t, 599, st.count)
	require.NotNil(t, st.snoozeTarget)
	require.NotNil(t, st.snoozeCountAt)
	assert.EqualValues(t, 100, *st.snoozeTarget, "I8: the columns are untouched")
	assert.EqualValues(t, 500, *st.snoozeCountAt)
	assert.Empty(t, snoozeEndedActivityRows(t, ctx, pool, issue.ID), "I8: no status_changed row while the snooze runs")

	// 599 → 600: (600 - 500) = 100 >= 100. The snooze ends on THIS occurrence.
	outcome, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), releaseOccurrence("v3.0.0", time.Now().UTC()), "v3.0.0")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeUnsnoozed, outcome, "I9: the snooze arm ran")
	st = issueSnoozeStateOf(t, ctx, pool, issue.ID)
	assert.Equal(t, "unresolved", st.status)
	assert.EqualValues(t, 600, st.count)
	assert.Nil(t, st.snoozedUntil, "I9: the three columns are NULL")
	assert.Nil(t, st.snoozeTarget)
	assert.Nil(t, st.snoozeCountAt)
	assert.EqualValues(t, 0, st.regressionCount, "decision 17: no regression column moves — a person parked it, nothing regressed")
	require.NotNil(t, st.regressionStatus)
	assert.Equal(t, "none", *st.regressionStatus, "decision 17: regression_status keeps its DEFAULT 'none'")

	rows := snoozeEndedActivityRows(t, ctx, pool, issue.ID)
	require.Len(t, rows, 1, "I9: exactly one status_changed row")
	assert.Equal(t, map[string]interface{}{"status": "ignored"}, rows[0].oldValue)
	assert.Equal(t, map[string]interface{}{
		"status":                 "unresolved",
		"reason":                 "snooze_occurrences_reached",
		"snoozedUntil":           nil,
		"snoozeOccurrenceTarget": float64(100),
		"snoozeCountAt":          float64(500),
		"count":                  float64(600),
	}, rows[0].newValue, "the pre-null values and the COMMITTED count (600, not 599)")

	// One more: an unresolved issue takes the plain path — Existing, no second snooze row.
	outcome, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), releaseOccurrence("v3.0.0", time.Now().UTC()), "v3.0.0")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeExisting, outcome)
	assert.Len(t, snoozeEndedActivityRows(t, ctx, pool, issue.ID), 1)
}

// (5) the snooze arm on a date: `ignored` with snoozed_until in the past ends on the next occurrence
// (flow I14) with reason 'snoozed_until_passed' and new_value.snoozedUntil the UTC `Z` string equal
// to the stored instant to the second (decision 40); a future snoozed_until keeps the issue
// 'ignored' with the outcome Ignored (flow I15).
func TestStoreEvent_SnoozeUntilPastEndsOnNextOccurrence(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "snooze5b")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "snooze5b")
	issue := eidIssueIn(projectID, "production", fp)
	_, stored, err := s.StoreEvent(ctx, issue, releaseOccurrence("", time.Now().UTC()), "")
	require.NoError(t, err)
	require.True(t, stored)

	// A future date, stored with an offset the way a +07:00 request would land it — the column is
	// TIMESTAMPTZ, so the instant is what is stored. Still ignored on the next occurrence.
	future := time.Now().Add(2 * time.Hour).In(time.FixedZone("WIB", 7*3600)).Truncate(time.Second)
	_, err = pool.Exec(ctx, `UPDATE issues SET status = 'ignored', snoozed_until = $2 WHERE id = $1`, issue.ID, future)
	require.NoError(t, err)
	outcome, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), releaseOccurrence("", time.Now().UTC()), "")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeIgnored, outcome, "I15: a future snoozed_until keeps the snooze running")
	st := issueSnoozeStateOf(t, ctx, pool, issue.ID)
	assert.Equal(t, "ignored", st.status)
	require.NotNil(t, st.snoozedUntil)
	assert.True(t, st.snoozedUntil.Equal(future), "the column is untouched")
	assert.Empty(t, snoozeEndedActivityRows(t, ctx, pool, issue.ID))

	// A past date: the server's clock (now() in the transaction) says it lapsed — the occurrence
	// ends it whatever the client's CreatedAt says (decision 7: a skewed client cannot end a snooze
	// early, and cannot keep a lapsed one running either).
	past := time.Now().Add(-90 * time.Minute).In(time.FixedZone("WIB", 7*3600)).Truncate(time.Second)
	_, err = pool.Exec(ctx, `UPDATE issues SET snoozed_until = $2 WHERE id = $1`, issue.ID, past)
	require.NoError(t, err)
	countBefore := issueSnoozeStateOf(t, ctx, pool, issue.ID).count
	skewed := releaseOccurrence("", time.Now().Add(-3*time.Hour).UTC()) // a client clock BEFORE the snooze date
	outcome, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), skewed, "")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeUnsnoozed, outcome, "I14: a lapsed date ends on the next occurrence")
	st = issueSnoozeStateOf(t, ctx, pool, issue.ID)
	assert.Equal(t, "unresolved", st.status)
	assert.EqualValues(t, countBefore+1, st.count)
	assert.Nil(t, st.snoozedUntil)
	assert.Nil(t, st.snoozeTarget)
	assert.Nil(t, st.snoozeCountAt)

	rows := snoozeEndedActivityRows(t, ctx, pool, issue.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "snoozed_until_passed", rows[0].newValue["reason"])
	assert.Equal(t, "unresolved", rows[0].newValue["status"])
	assert.Nil(t, rows[0].newValue["snoozeOccurrenceTarget"], "only a date was set: the target reads null")
	assert.Nil(t, rows[0].newValue["snoozeCountAt"])
	assert.Equal(t, float64(countBefore+1), rows[0].newValue["count"])
	assertSnoozedUntilPayload(t, rows[0].newValue, past)
}

// (5) both conditions set (flow I16): the snooze ends when EITHER is met; when both hold on the same
// occurrence reason is 'snoozed_until_passed' (the date is checked first), and the payload carries
// all three pre-null values. The mirror: a future date with the target reached ends on the count,
// and the payload still carries the (future) date as the UTC `Z` string.
func TestStoreEvent_SnoozeBothConditionsDateCheckedFirst(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "snooze5c")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Case A: both hold at once → snoozed_until_passed.
	fpA := eidFingerprint(t, "snooze5c-both")
	issueA := eidIssueIn(projectID, "production", fpA)
	_, stored, err := s.StoreEvent(ctx, issueA, releaseOccurrence("", time.Now().UTC()), "")
	require.NoError(t, err)
	require.True(t, stored)
	pastA := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	_, err = pool.Exec(ctx,
		`UPDATE issues SET status = 'ignored', snoozed_until = $2, snooze_occurrence_target = 1, snooze_count_at = 1 WHERE id = $1`,
		issueA.ID, pastA) // count is 1: the next occurrence makes 2 - 1 >= 1 AND the date has passed
	require.NoError(t, err)
	outcome, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fpA), releaseOccurrence("", time.Now().UTC()), "")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeUnsnoozed, outcome)
	rows := snoozeEndedActivityRows(t, ctx, pool, issueA.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "snoozed_until_passed", rows[0].newValue["reason"], "I16: both hold — the date is checked first")
	assert.Equal(t, float64(1), rows[0].newValue["snoozeOccurrenceTarget"], "all three pre-null values travel")
	assert.Equal(t, float64(1), rows[0].newValue["snoozeCountAt"])
	assert.Equal(t, float64(2), rows[0].newValue["count"])
	assertSnoozedUntilPayload(t, rows[0].newValue, pastA)
	stA := issueSnoozeStateOf(t, ctx, pool, issueA.ID)
	assert.Equal(t, "unresolved", stA.status)
	assert.Nil(t, stA.snoozedUntil)
	assert.Nil(t, stA.snoozeTarget)
	assert.Nil(t, stA.snoozeCountAt)

	// Case B: the date is still in the future, the target is reached → snooze_occurrences_reached,
	// and the payload's snoozedUntil is the future date, UTC, `Z`.
	fpB := eidFingerprint(t, "snooze5c-count")
	issueB := eidIssueIn(projectID, "production", fpB)
	_, stored, err = s.StoreEvent(ctx, issueB, releaseOccurrence("", time.Now().UTC()), "")
	require.NoError(t, err)
	require.True(t, stored)
	futureB := time.Now().Add(3 * time.Hour).In(time.FixedZone("WIB", 7*3600)).Truncate(time.Second)
	_, err = pool.Exec(ctx,
		`UPDATE issues SET status = 'ignored', snoozed_until = $2, snooze_occurrence_target = 2, snooze_count_at = 1 WHERE id = $1`,
		issueB.ID, futureB)
	require.NoError(t, err)
	outcome, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fpB), releaseOccurrence("", time.Now().UTC()), "")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeIgnored, outcome, "count 2 - 1 = 1 < 2 and the date is in the future: still parked")
	outcome, stored, err = s.StoreEvent(ctx, eidIssueIn(projectID, "production", fpB), releaseOccurrence("", time.Now().UTC()), "")
	require.NoError(t, err)
	require.True(t, stored)
	assert.Equal(t, store.IssueOutcomeUnsnoozed, outcome, "count 3 - 1 = 2 >= 2: the count ends it")
	rows = snoozeEndedActivityRows(t, ctx, pool, issueB.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "snooze_occurrences_reached", rows[0].newValue["reason"], "I16: only the count held")
	assert.Equal(t, float64(3), rows[0].newValue["count"])
	assertSnoozedUntilPayload(t, rows[0].newValue, futureB)
}

// (6) the outcome, at the store level (decision 29): `ignored` + target 3 (snoozed at count 1) is
// Ignored on the 2nd post-snooze occurrence and Unsnoozed on the 3rd; a plain indefinite ignore is
// Ignored on every occurrence; a resolved, non-regressing recurrence is Existing; New and
// Regressed as today. The service-level half — dispatchAlert skipped iff Ignored — is be.md task
// 4's case through ProcessorService.
func TestStoreEvent_SnoozeOutcomes(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "snooze6")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	deliver := func(fp, release string) store.IssueOutcome {
		t.Helper()
		outcome, stored, err := s.StoreEvent(ctx, eidIssueIn(projectID, "production", fp), releaseOccurrence(release, time.Now().UTC()), release)
		require.NoError(t, err)
		require.True(t, stored)
		return outcome
	}
	issueIDOf := func(fp string) string {
		t.Helper()
		var id string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT id::text FROM issues WHERE project_id = $1 AND environment = 'production' AND fingerprint = $2`,
			projectID, fp).Scan(&id))
		return id
	}

	// New, then Existing (an unresolved recurrence).
	fpNew := eidFingerprint(t, "snooze6-new")
	assert.Equal(t, store.IssueOutcomeNew, deliver(fpNew, "v1.0.0"))
	assert.Equal(t, store.IssueOutcomeExisting, deliver(fpNew, "v1.0.0"))

	// Ignored on the 2nd, Ignored on the 3rd... : target 3 snoozed at count 1 — the 1st and 2nd
	// post-snooze occurrences leave (count - 1) at 1 and 2, the 3rd makes it 3.
	fpTarget := eidFingerprint(t, "snooze6-target")
	assert.Equal(t, store.IssueOutcomeNew, deliver(fpTarget, ""))
	_, err := pool.Exec(ctx,
		`UPDATE issues SET status = 'ignored', snooze_occurrence_target = 3, snooze_count_at = count WHERE id = $1`, issueIDOf(fpTarget))
	require.NoError(t, err)
	assert.Equal(t, store.IssueOutcomeIgnored, deliver(fpTarget, ""), "1st post-snooze occurrence: 2 - 1 < 3")
	assert.Equal(t, store.IssueOutcomeIgnored, deliver(fpTarget, ""), "2nd post-snooze occurrence: 3 - 1 < 3")
	assert.Equal(t, store.IssueOutcomeUnsnoozed, deliver(fpTarget, ""), "3rd post-snooze occurrence: 4 - 1 >= 3")
	assert.Equal(t, store.IssueOutcomeExisting, deliver(fpTarget, ""), "unresolved now: the plain path")

	// A plain indefinite ignore: Ignored, every time (flow I15 — decision 16's estate-wide flip).
	fpPlain := eidFingerprint(t, "snooze6-plain")
	assert.Equal(t, store.IssueOutcomeNew, deliver(fpPlain, ""))
	_, err = pool.Exec(ctx, `UPDATE issues SET status = 'ignored' WHERE id = $1`, issueIDOf(fpPlain))
	require.NoError(t, err)
	assert.Equal(t, store.IssueOutcomeIgnored, deliver(fpPlain, ""))
	assert.Equal(t, store.IssueOutcomeIgnored, deliver(fpPlain, ""))
	st := issueSnoozeStateOf(t, ctx, pool, issueIDOf(fpPlain))
	assert.Equal(t, "ignored", st.status)
	assert.EqualValues(t, 3, st.count, "counted and stored, never dispatched — that is what ignore means")

	// A resolved, non-regressing recurrence: Existing — never Ignored, never Regressed.
	fpResolved := eidFingerprint(t, "snooze6-resolved")
	assert.Equal(t, store.IssueOutcomeNew, deliver(fpResolved, "v2.0.0"))
	assert.Equal(t, store.IssueOutcomeExisting, deliver(fpResolved, "v2.1.0"))
	_, err = pool.Exec(ctx,
		`UPDATE issues SET status = 'resolved', resolved_in_version = 'v2.1.0', resolved_at = now(),
		        resolved_by_type = 'user', resolved_by = 'integration-test' WHERE id = $1`, issueIDOf(fpResolved))
	require.NoError(t, err)
	assert.Equal(t, store.IssueOutcomeExisting, deliver(fpResolved, "v2.0.0"), "v2.0.0 was first seen before v2.1.0: not a regression (decision 1)")
	st = issueSnoozeStateOf(t, ctx, pool, issueIDOf(fpResolved))
	assert.Equal(t, "resolved", st.status)

	// Regressed, as today: a first sighting against an explicit version is newest (decision 14).
	assert.Equal(t, store.IssueOutcomeRegressed, deliver(fpResolved, "v2.2.0"))
}

// ---------------------------------------------------------------------------------------------------
// sentinel-releases-snooze §4.3 through ProcessorService — the outcome gate (6) and the post-commit
// `next` rewrite (7); flows I11, I17, I18, I20
// ---------------------------------------------------------------------------------------------------
//
// Contract §11, boundary 1's service-level half: the gate on dispatchAlert and the rewrite are
// properties of processEventInternal, not of StoreEvent, so they are driven through
// service.ProcessorService with a captured sender — the SetSender pattern of
// TestEventIdempotency_DuplicateDoesNotFeedAlertsOrSearchIndex above, at frequency_threshold = 1 so
// every dispatched occurrence is exactly one captured alert and a withheld one is exactly zero.

// releaseProtoEvent is buildValidProtoEvent with the three fields these cases vary: the fingerprint
// (the issue identity), the release and the event clock (the release's first_seen on a first sighting,
// and the rewrite's `$3`). Environment stays buildValidProtoEvent's "test"; the metadata carries no
// affected ids so the rollup writes nothing.
func releaseProtoEvent(projectKey, fp, release string, at time.Time) *sentinelv1.ErrorEvent {
	ev := buildValidProtoEvent(projectKey)
	ev.Fingerprint = fp
	ev.ReleaseVersion = release
	ev.EventId = "evt-rel-svc-" + uuid.New().String()
	ev.Timestamp = timestamppb.New(at)
	ev.Metadata = nil
	return ev
}

// newCapturingService builds a ProcessorService over pool with ONE enabled alert config for projectID
// at frequency_threshold = 1 (inserted BEFORE construction — NewProcessorService loads configs
// synchronously, VERIFIED_STATE.md S8 item 4) and a captured sender. The returned func reads how
// many alerts have been sent so far.
func newCapturingService(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) (*service.ProcessorService, func() int) {
	t.Helper()
	var orgID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT organization_id FROM projects WHERE id = $1`, projectID).Scan(&orgID))
	_, err := pool.Exec(ctx,
		`INSERT INTO alert_configs (project_id, organization_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled)
		 VALUES ($1, $2, 'email', '{"to": "oncall@example.com"}'::jsonb, 1, 60, true)`,
		projectID, orgID)
	require.NoError(t, err)

	svc := service.NewProcessorService(pool)
	var mu sync.Mutex
	sent := 0
	svc.Alerts().SetSender(func(_ context.Context, _ *alerts.AlertConfig, _ *alerts.Alert) {
		mu.Lock()
		defer mu.Unlock()
		sent++
	})
	return svc, func() int {
		mu.Lock()
		defer mu.Unlock()
		return sent
	}
}

// processOK marshals ev and drives it through svc.ProcessEvent, requiring an ACK (nil).
func processOK(t *testing.T, ctx context.Context, svc *service.ProcessorService, ev *sentinelv1.ErrorEvent) {
	t.Helper()
	data, err := proto.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, svc.ProcessEvent(ctx, data))
}

// issueIDByFingerprint reads the id of the (project, "test", fp) issue the service wrote.
func issueIDByFingerprint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, fp string) string {
	t.Helper()
	var id string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1 AND environment = 'test' AND fingerprint = $2`,
		projectID, fp).Scan(&id))
	return id
}

// resolvedInVersionOf reads (status, resolved_in_version) — the two columns the rewrite reads and
// the one it writes. resolved_in_version is returned as "" for NULL.
func resolvedInVersionOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID string) (status, resolvedIn string) {
	t.Helper()
	var riv *string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, resolved_in_version FROM issues WHERE id = $1`, issueID).Scan(&status, &riv))
	if riv != nil {
		resolvedIn = *riv
	}
	return status, resolvedIn
}

func activityRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issueID string) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM issue_activity WHERE issue_id = $1`, issueID).Scan(&n))
	return n
}

// TestProcessorService_AlertGateSkipsOnlyIgnoredOutcome is §11 (6)'s service-level case and decision
// 16's execution proof: dispatchAlert is NOT called for an occurrence whose outcome is Ignored — a
// running snooze and a plain indefinite ignore — and IS called for New, Existing, Regressed and
// Unsnoozed. frequency_threshold = 1 makes "called" observable as exactly one captured alert per
// dispatched occurrence. Remove the `if outcome != store.IssueOutcomeIgnored` gate in
// processEventInternal and the two Ignored steps below go red (the count moves to 4 and 6).
func TestProcessorService_AlertGateSkipsOnlyIgnoredOutcome(t *testing.T) {
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
	}
	pool := newPostgresPool(t, cfg)
	t.Cleanup(func() { pool.Close() })

	projectName := uniqueProjectName("rel-gate")
	projectID := createTestProject(t, pool, projectName, uniqueAPIKey("rel-gate"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	svc, sentCount := newCapturingService(t, ctx, pool, projectID)
	fp := "fp-rel-gate-" + uuid.New().String()

	// New → dispatched.
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fp, "v1.0.0", time.Now().UTC()))
	assert.Equal(t, 1, sentCount(), "New: dispatched (threshold 1 → one alert)")
	issueID := issueIDByFingerprint(t, ctx, pool, projectID, fp)

	// Existing (an unresolved recurrence) → dispatched.
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fp, "v1.0.0", time.Now().UTC()))
	assert.Equal(t, 2, sentCount(), "Existing: dispatched")

	// Regressed: resolved in v1.0.0, then a first sighting of v1.1.0 is newest (decision 14) → dispatched.
	_, err := pool.Exec(ctx,
		`UPDATE issues SET status = 'resolved', resolved_in_version = 'v1.0.0', resolved_at = now(),
		        resolved_by_type = 'user', resolved_by = 'integration-test' WHERE id = $1`, issueID)
	require.NoError(t, err)
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fp, "v1.1.0", time.Now().UTC()))
	status, _ := resolvedInVersionOf(t, ctx, pool, issueID)
	require.Equal(t, "unresolved", status, "precondition: the regression arm ran")
	assert.Equal(t, 3, sentCount(), "Regressed: dispatched")

	// Ignored (a running snooze): target 2 set at the current count — the next occurrence leaves
	// (count + 1) - count_at = 1 < 2, so the row stays ignored → NOT dispatched.
	_, err = pool.Exec(ctx,
		`UPDATE issues SET status = 'ignored', snooze_occurrence_target = 2, snooze_count_at = count WHERE id = $1`, issueID)
	require.NoError(t, err)
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fp, "v1.1.0", time.Now().UTC()))
	status, _ = resolvedInVersionOf(t, ctx, pool, issueID)
	require.Equal(t, "ignored", status, "precondition: the snooze is still running after this occurrence")
	assert.Equal(t, 3, sentCount(), "Ignored (snooze running): NOT dispatched — decision 16")

	// Unsnoozed: the 2nd post-snooze occurrence reaches the target; the row is unresolved → dispatched
	// normally through the threshold (decision 17/29: neither bypassed nor withheld).
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fp, "v1.1.0", time.Now().UTC()))
	status, _ = resolvedInVersionOf(t, ctx, pool, issueID)
	require.Equal(t, "unresolved", status, "precondition: the snooze arm ended the snooze")
	assert.Equal(t, 4, sentCount(), "Unsnoozed: dispatched")

	// Ignored (a plain indefinite ignore) → NOT dispatched, on every occurrence (flow I15).
	_, err = pool.Exec(ctx, `UPDATE issues SET status = 'ignored' WHERE id = $1`, issueID)
	require.NoError(t, err)
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fp, "v1.1.0", time.Now().UTC()))
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fp, "v1.1.0", time.Now().UTC()))
	assert.Equal(t, 4, sentCount(), "Ignored (plain ignore): NOT dispatched, twice")

	// The withheld occurrences were still stored, counted and bucketed (decision 16: muted, not dropped).
	st := issueSnoozeStateOf(t, ctx, pool, issueID)
	assert.Equal(t, "ignored", st.status)
	assert.EqualValues(t, 7, st.count, "seven deliveries, seven counted — the gate withholds dispatch only")
	var occ int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1`, issueID).Scan(&occ))
	assert.Equal(t, 7, occ)
}

// TestProcessorService_NextRewriteAfterFirstSighting is §11 (7): the post-commit `next` rewrite through
// ProcessorService (flows I11, I18, I20; decisions 8, 15, 27, 41).
//
//   - a `next` issue reads the new version after an UNRELATED issue's first occurrence in it (I11), in
//     every environment of the project (decision 41), and NOT in another project;
//   - it is untouched by an occurrence in a release whose first_seen ≤ its resolved_at — a first
//     sighting carrying a client clock BEFORE the resolution inserts the release with that first_seen,
//     the rewrite runs, and `resolved_at < first_seen` matches nothing (the STRICT comparison);
//   - a `next` with resolved_at IS NULL is untouched (I20, decision 27);
//   - it is rewritten ONCE: a later first sighting leaves the concrete version alone, and a second
//     occurrence in the same release (not inserted) runs no rewrite at all (I18);
//   - no issue_activity row is written for the rewritten issue (decision 8).
func TestProcessorService_NextRewriteAfterFirstSighting(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)

	projectName := uniqueProjectName("rel-next")
	projectID := createTestProject(t, pool, projectName, uniqueAPIKey("rel-next"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })
	otherProjectName := uniqueProjectName("rel-next-other")
	otherProjectID := createTestProject(t, pool, otherProjectName, uniqueAPIKey("rel-next-other"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, otherProjectID) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// X (environment "test") and X2 (environment "staging") are the project's `next` issues; Z is
	// `next` with resolved_at IS NULL; O is another project's `next` issue. All created through the
	// service so they are real rows, then resolved by SQL exactly as the dashboard's PATCH would.
	t0 := time.Now().UTC().Add(-2 * time.Hour)
	fpX, fpX2, fpZ, fpO := "fp-x-"+uuid.New().String(), "fp-x2-"+uuid.New().String(), "fp-z-"+uuid.New().String(), "fp-o-"+uuid.New().String()
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fpX, "v1.0.0", t0))
	x2 := releaseProtoEvent(projectName, fpX2, "v1.0.0", t0)
	x2.Environment = "staging"
	processOK(t, ctx, svc, x2)
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fpZ, "v1.0.0", t0))
	processOK(t, ctx, svc, releaseProtoEvent(otherProjectName, fpO, "v1.0.0", t0))

	xID := issueIDByFingerprint(t, ctx, pool, projectID, fpX)
	var x2ID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text FROM issues WHERE project_id = $1 AND environment = 'staging' AND fingerprint = $2`,
		projectID, fpX2).Scan(&x2ID))
	zID := issueIDByFingerprint(t, ctx, pool, projectID, fpZ)
	oID := issueIDByFingerprint(t, ctx, pool, otherProjectID, fpO)

	resolvedAt := time.Now().UTC().Add(-1 * time.Hour) // T
	for _, id := range []string{xID, x2ID, oID} {
		_, err := pool.Exec(ctx,
			`UPDATE issues SET status = 'resolved', resolved_in_version = 'next', resolved_at = $2,
			        resolved_by_type = 'user', resolved_by = 'integration-test' WHERE id = $1`, id, resolvedAt)
		require.NoError(t, err)
	}
	_, err := pool.Exec(ctx,
		`UPDATE issues SET status = 'resolved', resolved_in_version = 'next', resolved_at = NULL,
		        resolved_by_type = 'user', resolved_by = 'integration-test' WHERE id = $1`, zID)
	require.NoError(t, err)
	xActivityBefore := activityRowCount(t, ctx, pool, xID)

	// Y is the unrelated issue whose occurrences carry the releases.
	fpY := "fp-y-" + uuid.New().String()

	// (b) A first sighting of v0.9.0 with a client clock BEFORE T: the release row is inserted with
	// first_seen = T - 30m ≤ resolved_at, the rewrite runs and matches nothing.
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fpY, "v0.9.0", resolvedAt.Add(-30*time.Minute)))
	require.True(t, releaseRowOf(t, ctx, pool, projectID, "v0.9.0").found, "precondition: v0.9.0 was a first sighting")
	_, riv := resolvedInVersionOf(t, ctx, pool, xID)
	assert.Equal(t, "next", riv, "a release first seen at or before resolved_at must not rewrite `next` (strict >)")

	// (a) A first sighting of v1.1.0 after T: X and X2 read it; Z (NULL resolved_at) and O (another
	// project) do not.
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fpY, "v1.1.0", time.Now().UTC()))
	status, riv := resolvedInVersionOf(t, ctx, pool, xID)
	assert.Equal(t, "resolved", status, "the resolution did not change — only its version became known")
	assert.Equal(t, "v1.1.0", riv, "I11: X reads the new release after an unrelated issue's first occurrence in it")
	_, riv = resolvedInVersionOf(t, ctx, pool, x2ID)
	assert.Equal(t, "v1.1.0", riv, "decision 41: the rewrite is project-wide across environments")
	_, riv = resolvedInVersionOf(t, ctx, pool, zID)
	assert.Equal(t, "next", riv, "I20 / decision 27: a `next` with resolved_at IS NULL is never rewritten")
	_, riv = resolvedInVersionOf(t, ctx, pool, oID)
	assert.Equal(t, "next", riv, "another project's `next` is not this project's release's to rewrite")
	assert.Equal(t, xActivityBefore, activityRowCount(t, ctx, pool, xID), "decision 8: no activity row per rewritten issue")

	// (c) Rewritten once: a later first sighting (v1.2.0) leaves the concrete version alone, and a
	// second occurrence in v1.1.0 (the DO UPDATE arm, not inserted) changes nothing either.
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fpY, "v1.1.0", time.Now().UTC()))
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fpY, "v1.2.0", time.Now().UTC()))
	_, riv = resolvedInVersionOf(t, ctx, pool, xID)
	assert.Equal(t, "v1.1.0", riv, "I18: the version is written once; later releases do not move it")
	assert.EqualValues(t, 2, releaseRowOf(t, ctx, pool, projectID, "v1.1.0").occurrenceCount)
	_, riv = resolvedInVersionOf(t, ctx, pool, zID)
	assert.Equal(t, "next", riv)
}

// breakNextRewrite installs a BEFORE UPDATE trigger on issues that raises with SQLSTATE 40P01 for
// exactly the write the post-commit rewrite performs on projectID's rows — resolved_in_version
// leaving 'next' while the row stays resolved — so the fan-out's UPDATE, and only it, errors against
// a REAL Postgres. The regression arm (status → unresolved) and every other writer are untouched.
// 40P01 is decision 42's accepted deadlock, the one SQLSTATE the log line exists to make legible.
//
// Returned as a function so a test can drop it early and prove the failure was not sticky; t.Cleanup
// drops it regardless.
func breakNextRewrite(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) (restore func()) {
	t.Helper()
	name := "eid_next_rewrite_boom_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	_, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION %s() RETURNS trigger AS $fn$
		BEGIN
			IF OLD.project_id::text = %s AND OLD.resolved_in_version = 'next'
			   AND NEW.resolved_in_version IS DISTINCT FROM 'next' AND NEW.status = 'resolved' THEN
				RAISE EXCEPTION 'simulated next rewrite deadlock (decision 42)' USING ERRCODE = '40P01';
			END IF;
			RETURN NEW;
		END;
		$fn$ LANGUAGE plpgsql;
	`, name, quoteSQLLiteral(projectID)))
	require.NoError(t, err, "creating the rewrite-failure trigger function")
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON issues FOR EACH ROW EXECUTE FUNCTION %s()`, name, name))
	require.NoError(t, err, "creating the rewrite-failure trigger")
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON issues`, name))
		_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	}
	t.Cleanup(drop)
	return drop
}

// TestProcessorService_NextRewriteFailureIsCountedAndNeverFailsTheEvent is flow I17 and the
// registration proof for sentinel_release_next_rewrite_failures_total (§12): when the post-commit
// rewrite's UPDATE fails, the event is STORED and ACKed, the release row exists, the `next` issue keeps
// `next` (for the hourly catch-up), and the UNLABELLED counter moves by exactly one — read through the
// same ManualReader the other §12 counters are, so it does not rely on obs.Bootstrap having run.
// Remove procmetrics.RecordReleaseNextRewriteFailure from rewriteNextIssues and this goes red.
func TestProcessorService_NextRewriteFailureIsCountedAndNeverFailsTheEvent(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)
	reader := eidManualReader()

	projectName := uniqueProjectName("rel-fail")
	projectID := createTestProject(t, pool, projectName, uniqueAPIKey("rel-fail"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fpX, fpY := "fp-x-"+uuid.New().String(), "fp-y-"+uuid.New().String()
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fpX, "v1.0.0", time.Now().UTC().Add(-2*time.Hour)))
	xID := issueIDByFingerprint(t, ctx, pool, projectID, fpX)
	_, err := pool.Exec(ctx,
		`UPDATE issues SET status = 'resolved', resolved_in_version = 'next', resolved_at = $2,
		        resolved_by_type = 'user', resolved_by = 'integration-test' WHERE id = $1`,
		xID, time.Now().UTC().Add(-1*time.Hour))
	require.NoError(t, err)

	restore := breakNextRewrite(t, ctx, pool, projectID)
	before := collectUnlabelledCounter(t, ctx, reader, obs.MetricReleaseNextRewriteFailures)

	ev := releaseProtoEvent(projectName, fpY, "v1.1.0", time.Now().UTC())
	processOK(t, ctx, svc, ev) // nil: the event is ACKed regardless of the rewrite

	// Stored: the occurrence and the release row are both there (the rewrite is outside the ingest tx).
	var occ int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences WHERE event_id = $1`, ev.EventId).Scan(&occ))
	assert.Equal(t, 1, occ, "I17: the event is stored even though the rewrite failed")
	assert.True(t, releaseRowOf(t, ctx, pool, projectID, "v1.1.0").found, "the release row committed before the rewrite ran")

	// Untouched: X keeps `next` for the hourly catch-up.
	_, riv := resolvedInVersionOf(t, ctx, pool, xID)
	assert.Equal(t, "next", riv, "a failed rewrite leaves `next` in place — the catch-up's input")

	// Counted, once, under the contract's name, with no label.
	assert.Equal(t, before+1, collectUnlabelledCounter(t, ctx, reader, obs.MetricReleaseNextRewriteFailures),
		"%s must increment exactly once per failed rewrite", obs.MetricReleaseNextRewriteFailures)
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	found := false
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != obs.MetricReleaseNextRewriteFailures {
				continue
			}
			found = true
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				assert.Equal(t, 0, dp.Attributes.Len(), "decision 30: no label — the dashboard's same-named counter sums with this one by name")
			}
		}
	}
	assert.True(t, found, "%s must be registered under exactly that name", obs.MetricReleaseNextRewriteFailures)
	assert.Equal(t, "sentinel_release_next_rewrite_failures_total", obs.MetricReleaseNextRewriteFailures)

	// A second first sighting after the trigger is gone rewrites normally — the failure was not sticky.
	restore()
	processOK(t, ctx, svc, releaseProtoEvent(projectName, fpY, "v1.2.0", time.Now().UTC()))
	_, riv = resolvedInVersionOf(t, ctx, pool, xID)
	assert.Equal(t, "v1.2.0", riv, "with the trigger gone the next first sighting rewrites X")
	assert.Equal(t, before+1, collectUnlabelledCounter(t, ctx, reader, obs.MetricReleaseNextRewriteFailures),
		"a successful rewrite must not move the failure counter")
}
