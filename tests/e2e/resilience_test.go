package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

// This file covers matrix rows U28-U30 from docs/plans/E2E_RECOVERY_PLAN.md's P7 section: the resilience
// and migration rows. They are the destructive ones — U28 takes the database away from every service in
// the stack — so they are written to be run with the stack to themselves and to restore it unconditionally.

// ---------------------------------------------------------------------------------------------------
// Outage simulation
// ---------------------------------------------------------------------------------------------------

// resilienceAdminPool opens a pool to the `postgres` maintenance database, so it stays usable while the
// `sentinel` database is refusing connections. Connecting the admin pool to the database it is about to
// close would be self-defeating: it could take the outage down but never end it.
func resilienceAdminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	adminDSN, dbName := resilienceSplitDSN(t, cfg.DatabaseURL)
	p, err := pgxpool.New(context.Background(), adminDSN)
	if err != nil {
		t.Fatalf("opening the admin pool at %s: %v", redactDSN(adminDSN), err)
	}
	if err := p.Ping(context.Background()); err != nil {
		p.Close()
		t.Fatalf("pinging the admin pool: %v", err)
	}
	t.Logf("U28: admin pool on the maintenance database; target database is %q", dbName)
	return p
}

// resilienceSplitDSN returns (DSN pointing at the `postgres` maintenance database, target db name).
func resilienceSplitDSN(t *testing.T, dsn string) (string, string) {
	t.Helper()

	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		t.Fatalf("cannot find the database name in %s", redactDSN(dsn))
	}
	rest := dsn[slash+1:]
	name := rest
	query := ""
	if q := strings.Index(rest, "?"); q >= 0 {
		name = rest[:q]
		query = rest[q:]
	}
	if name == "" {
		t.Fatalf("empty database name in %s", redactDSN(dsn))
	}
	return dsn[:slash+1] + "postgres" + query, name
}

// resilienceSetDBReachable simulates a database outage WITHOUT touching the container.
//
// Neither obvious approach works, and both were tried on this project. Stopping the container makes
// docker assign a new random host port on restart, so every already-open DSN — this test's pool and the
// processor's own — keeps dialing an address nothing listens on and can never recover; podman preserves
// the mapping, which is exactly why Stop/Start passed locally and failed in CI. And `pg_ctl stop` inside
// the container kills PID 1, taking the container down with it (observed: exit 137).
//
// CONNECTION LIMIT 0 plus terminating existing backends makes new connections fail while the container,
// the port mapping and the data all survive. That is what an application actually experiences during a
// database outage: connections refused, then accepted again.
func resilienceSetDBReachable(t *testing.T, admin *pgxpool.Pool, dbName string, reachable bool) {
	t.Helper()

	limit := "0"
	if reachable {
		limit = "-1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := admin.Exec(ctx, fmt.Sprintf("ALTER DATABASE %q CONNECTION LIMIT %s", dbName, limit)); err != nil {
		t.Fatalf("setting CONNECTION LIMIT %s on %q: %v", limit, dbName, err)
	}
	if !reachable {
		// Existing sessions keep working regardless of the limit, so evict them or the outage never
		// actually starts for anything already connected — including the processor.
		if _, err := admin.Exec(ctx,
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
			  WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName); err != nil {
			t.Fatalf("terminating backends on %q: %v", dbName, err)
		}
	}
}

// ---------------------------------------------------------------------------------------------------
// U28
// ---------------------------------------------------------------------------------------------------

// TestU28_DatabaseOutageLosesNothingAndDuplicatesNothing asserts that every event the ingestor ACCEPTED
// is stored exactly once after a database outage ends — no loss, and no duplicates.
//
// This is defect S9's regression test, and the history is the point. The original "graceful degradation"
// buffer ACKed events into an in-memory queue and lost them on restart: 3 events in, 0 rows after a
// SIGKILL. It was DELETED rather than repaired (DECISIONS.md D10). The current contract is that
// ProcessEvent returns an error when the database is unreachable, so the message is NAKed and NATS
// redelivers it — nothing is ever ACKed unstored.
//
// "Exactly once" is the whole assertion. Asserting "at least N" would pass while redelivery silently
// doubled every event, which is the other half of what this row is for.
//
// The outage is kept short on purpose. The subscriber's retry budget is 1/5/15/30/60/120/300s with
// MaxDeliver 7, so a brief outage recovers on an early attempt; an outage long enough to exhaust the
// budget would legitimately dead-letter the events, which is correct behaviour but a different test.
func TestU28_DatabaseOutageLosesNothingAndDuplicatesNothing(t *testing.T) {
	f := newFixture(t)

	admin := resilienceAdminPool(t)
	t.Cleanup(admin.Close)
	_, dbName := resilienceSplitDSN(t, cfg.DatabaseURL)

	// Restoring the limit is registered BEFORE the outage starts and is unconditional. Leaving a
	// database at CONNECTION LIMIT 0 breaks every later test on the machine, and it looks like their bug.
	t.Cleanup(func() {
		resilienceSetDBReachable(t, admin, dbName, true)
	})

	// One event with the database up. This proves the path works before the outage, and it warms the
	// ingestor's API-key cache — without that, authentication itself would need the database and the
	// outage would be indistinguishable from a bad key.
	if res := f.ingest(f.newEvent()); res.Status != http.StatusAccepted {
		t.Fatalf("pre-outage event: got %d, want 202 (body: %s)", res.Status, res.Body)
	}
	f.waitForOccurrences(1)

	// --- outage begins ---
	resilienceSetDBReachable(t, admin, dbName, false)
	waitFor(t, 60*time.Second, "the database to start refusing connections", func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			return true, "refusing: " + err.Error()
		}
		return false, "still accepting connections"
	})

	const n = 5
	accepted := 0
	statuses := make([]int, 0, n)
	for i := 0; i < n; i++ {
		res := f.ingest(f.newEvent().with(map[string]any{
			"message":     fmt.Sprintf("e2e outage event %d", i),
			"error_class": "E2EOutageError",
		}))
		statuses = append(statuses, res.Status)
		if res.Status == http.StatusAccepted {
			accepted++
		}
	}
	t.Logf("U28: during the outage, %d/%d events were accepted (202). statuses=%v", accepted, n, statuses)

	// --- outage ends ---
	resilienceSetDBReachable(t, admin, dbName, true)
	waitFor(t, 60*time.Second, "the database to accept connections again", func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			return false, "still refusing: " + err.Error()
		}
		return true, "accepting"
	})

	// Whatever the ingestor answered 202 to, it took responsibility for. Those events must all be
	// stored, exactly once, once the database is back. Anything the ingestor REJECTED never entered the
	// pipeline: a caller would retry it, so it is not loss — but it is worth seeing, hence the log above.
	want := 1 + accepted

	// The retry budget's second and third attempts are at 5s and 15s, and redelivery has to wait for the
	// NAK backoff, so allow well past that before declaring loss.
	waitFor(t, 4*time.Minute, fmt.Sprintf("%d occurrences after the outage ended", want), func() (bool, string) {
		got := f.occurrenceCount()
		return got == want, fmt.Sprintf("%d occurrences", got)
	})

	// Now the part that catches duplication rather than loss: hold, and require the count to stay put.
	// A redelivery that arrives after the target count is reached is invisible to the poll above.
	time.Sleep(10 * time.Second)
	if got := f.occurrenceCount(); got != want {
		t.Fatalf("occurrence count moved after reaching %d: now %d — the outage produced duplicates", want, got)
	}

	// issues.count is incremented by a different statement than the occurrence insert, in a separate
	// transaction with no event_id idempotency (the known open gap S16). If redelivery inflated it while
	// the occurrence rows stayed correct, that is worth reporting precisely rather than hiding.
	issues := f.issues()
	var total int64
	for _, i := range issues {
		total += i.Count
	}
	if total != int64(want) {
		t.Errorf("occurrences are exactly %d but issues.count totals %d — redelivery inflated the counter "+
			"without duplicating rows. This is S16: the issue upsert and the occurrence insert are separate "+
			"transactions with no event_id idempotency", want, total)
	}
}

// ---------------------------------------------------------------------------------------------------
// U29
// ---------------------------------------------------------------------------------------------------

// TestU29_MalformedMessageDeadLettersInsteadOfRedeliveringForever asserts that a permanently
// undecodable message reaches the dead-letter stream and stops being redelivered.
//
// Defect S13 was a poison-message livelock: with no MaxDeliver and no DLQ, one malformed message starved
// the entire pipeline forever. The mechanism now exists (D10) but nothing asserted "N attempts, then
// dead-letter" as a repeatable check — and a retry budget is exactly the kind of thing that gets
// regressed by a well-meaning config change.
//
// This publishes straight to the ingest subject rather than through /ingest, because the ingestor
// validates and would (correctly) reject the payload at the front door. The target here is the
// subscriber's delivery behaviour, which only a message already on the stream can exercise.
func TestU29_MalformedMessageDeadLettersInsteadOfRedeliveringForever(t *testing.T) {
	requireStack(t)

	before := readProcessorHealth(t)
	t.Logf("U29: DLQ depth before = %d (publish failures = %d)", before.DLQDepth, before.DLQPublishFailures)

	nc, err := nats.Connect(cfg.NATSURL, nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to NATS at %s: %v", cfg.NATSURL, err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("obtaining a JetStream context: %v", err)
	}

	// Not valid protobuf, and not valid JSON either, so no decoder anywhere can make sense of it. A
	// marker makes it findable in the DLQ and in the processor log.
	marker := "e2e-poison-" + uniqueSuffix()
	if _, err := js.Publish("error_events", []byte("\x00\x01\x02 not a valid ErrorEvent "+marker)); err != nil {
		t.Fatalf("publishing the malformed message: %v", err)
	}
	t.Logf("U29: published a malformed message with marker %s", marker)

	// The dead-letter is the observable outcome. Redelivery attempts happen on the retry backoff
	// (1/5/15/30/60/120/300s, MaxDeliver 7) unless the payload is classified as a permanent error, in
	// which case it dead-letters immediately — either path must END, and end here.
	waitFor(t, 5*time.Minute, "the malformed message to reach the dead-letter stream", func() (bool, string) {
		now := readProcessorHealth(t)
		if now.DLQDepth > before.DLQDepth {
			return true, fmt.Sprintf("DLQ depth %d -> %d", before.DLQDepth, now.DLQDepth)
		}
		return false, fmt.Sprintf("DLQ depth still %d", now.DLQDepth)
	})

	after := readProcessorHealth(t)
	t.Logf("U29: DLQ depth after = %d (delta %d), publish failures = %d",
		after.DLQDepth, after.DLQDepth-before.DLQDepth, after.DLQPublishFailures)

	if after.DLQPublishFailures > before.DLQPublishFailures {
		t.Errorf("dlq_publish_failures rose from %d to %d — the DLQ publish itself is failing, which means "+
			"messages are being NAKed indefinitely rather than captured",
			before.DLQPublishFailures, after.DLQPublishFailures)
	}

	// And it must STOP. If redelivery were unbounded the depth would keep climbing as each new attempt
	// re-dead-letters; a stable depth is the evidence that delivery terminated.
	settled := readProcessorHealth(t).DLQDepth
	time.Sleep(20 * time.Second)
	if again := readProcessorHealth(t).DLQDepth; again != settled {
		t.Errorf("DLQ depth kept moving after the message was captured (%d -> %d) — delivery has not "+
			"terminated, which is the S13 livelock this row exists to rule out", settled, again)
	}

	// The status string is NOT the assertion. A non-empty DLQ legitimately reports
	// "attention: dead-lettered events awaiting replay" — that is the endpoint doing its job, and
	// demanding "healthy" here would have asserted that the processor must stay quiet about a backlog.
	//
	// What S13 was actually about is starvation: one malformed message blocked every good one behind it
	// forever. So assert the thing that matters — a valid event still flows after the poison message.
	// This is strictly stronger than any status check, because it exercises the pipeline rather than
	// asking it how it feels.
	if after.Status == "" {
		t.Error("processor /health returned no status field")
	}

	f := newFixture(t)
	if res := f.ingest(f.newEvent()); res.Status != http.StatusAccepted {
		t.Fatalf("post-poison event: got %d, want 202 (body: %s)", res.Status, res.Body)
	}
	f.waitForOccurrences(1)
	t.Logf("U29: a valid event still flows after the poison message — no starvation (S13 stays closed)")
}

// ---------------------------------------------------------------------------------------------------
// U30
// ---------------------------------------------------------------------------------------------------

// TestU30_LiveSchemaAgreesWithItsMigrationLedger checks that the running stack's schema and its goose
// ledger tell the same story.
//
// On the up/down/up round-trip half of U30: that is deliberately NOT reimplemented here. It lives in
// tests/integration/db_migrations_test.go, which clones a throwaway database per test from a template
// via CREATE DATABASE ... TEMPLATE under a pg_advisory_lock, precisely because it runs DDL. Running
// migrations against the shared `sentinel` database corrupted the dev environment three times on this
// project: a goose ledger table records which versions THAT ledger applied and does not scope the DDL,
// so a `down` under one ledger drops tables another ledger still believes exist. Duplicating that
// machinery in a package whose entire isolation model is "by data, not by database" (see the comment in
// main_test.go) would be a strictly worse copy of a test that already passes.
//
// What this row can add from here is the check that catches the aftermath of that corruption, which no
// migration test can see because each runs against its own clean clone: does the LIVE database, the one
// every other row in this suite asserts against, actually match its ledger? When it did not, the symptom
// was `max(version_id) = 1722000000` recorded as applied while `project_api_keys` did not exist — and
// the failure surfaced as a wave of unrelated e2e failures that read like a code regression.
func TestU30_LiveSchemaAgreesWithItsMigrationLedger(t *testing.T) {
	requireStack(t)
	ctx := context.Background()

	var applied int64
	queryRow(t, &applied,
		`SELECT count(*) FROM schema_migrations WHERE is_applied`)
	if applied == 0 {
		t.Fatal("schema_migrations reports zero applied migrations — the migrate service has not run " +
			"against this database")
	}

	var maxVersion int64
	queryRow(t, &maxVersion, `SELECT max(version_id) FROM schema_migrations WHERE is_applied`)
	t.Logf("U30: schema_migrations reports %d applied migrations, max version_id = %d", applied, maxVersion)

	// Every table the ledger's migrations create must exist. This is the exact assertion that was false
	// during the corruption: the ledger said applied, the table was gone.
	expected := []string{
		"organizations", "organization_members", "organization_invitations",
		"projects", "project_members", "project_api_keys",
		"issues", "issue_activity", "issue_relations",
		"error_occurrences", "error_search_index",
		"alert_configs", "audit_logs", "settings",
		"user", "session", "account", "verification_token",
	}
	var missing []string
	for _, table := range expected {
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists); err != nil {
			t.Fatalf("probing for %q: %v", table, err)
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("schema_migrations claims %d migrations applied (max version_id %d) but these tables do "+
			"not exist: %v.\nThat combination means a migration test ran `down` against this shared "+
			"database under a different goose ledger. See tests/integration/db_migrations_test.go, which "+
			"clones a database per test to prevent exactly this.", applied, maxVersion, missing)
	}

	// A goose ledger with duplicate version rows means two runners raced, which makes "what is applied"
	// unanswerable and every later up/down unpredictable.
	var dupes int64
	queryRow(t, &dupes, `SELECT count(*) FROM (
	    SELECT version_id FROM schema_migrations GROUP BY version_id HAVING count(*) > 1
	  ) d`)
	if dupes != 0 {
		t.Errorf("schema_migrations has %d version_id values recorded more than once — concurrent goose "+
			"runs against one database", dupes)
	}

	// The other ledgers (processor_migrations, dashboard_migrations, ...) point at this SAME physical
	// database. That is legal, but if one of them is ahead of schema_migrations it is the signature of
	// the multi-ledger hazard, so report it rather than let it sit.
	rows, err := pool.Query(ctx,
		`SELECT table_name FROM information_schema.tables
		  WHERE table_schema = 'public' AND table_name LIKE '%migrations%' AND table_name <> 'schema_migrations'`)
	if err != nil {
		t.Fatalf("listing migration ledgers: %v", err)
	}
	defer rows.Close()
	var others []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning ledger name: %v", err)
		}
		others = append(others, name)
	}
	if len(others) > 0 {
		t.Logf("U30: additional goose ledgers present on this database: %v — each tracks versions "+
			"independently while sharing one physical schema (the multi-ledger hazard)", others)
	}
}
