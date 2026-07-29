package integration

// This file proves, by execution, the two fixes owned by the "procgo" work
// item (docs/plans/E2E_RECOVERY_PLAN.md P5-1 / S8 "wire alerting" and P4-2 /
// S9 "fix the degradation buffer"):
//
//   - TestProcgoAlertingDispatchesWithinOneEvent: configure an alert,
//     ingest a NEW error class, and show the notifier being invoked
//     (a captured sender) within one event.
//   - TestProcgoDegradationBufferSurvivesOutageNoLossNoDuplicates: stop
//     Postgres mid-stream, send N events, restart it, and assert exactly N
//     occurrences land — no loss, no duplicates.
//
// IMPORTANT (re-verified 2026-07-29, full SENTINEL_E2E run): the second test's
// mechanism changed underneath it since it was first written. S9's fix went
// through two shapes: an in-process buffer-with-async-flush (what this test
// originally drove directly via ProcessEvent, expecting a nil "buffered"
// return while the DB was down), and — later, same body of work — the
// buffer's removal entirely in favor of D10's NATS bounded-retry/backoff/DLQ
// (see apps/processor-go/degradation/buffer.go's package doc comment:
// "That mechanism is gone, deliberately, not accidentally"). Calling
// svc.ProcessEvent directly during an outage now always returns a non-nil
// error (there is nothing left in-process to buffer into), so the
// no-loss-no-duplicates guarantee can only be observed the way production
// actually delivers it: through a real NATS subscriber (D10's
// packages/shared-go/nats.Subscriber) retrying with backoff until Postgres
// comes back. The test below now drives a real Publisher/Subscriber pair
// against a dedicated, uniquely-named JetStream stream (never the shared
// ERROR_EVENTS stream the running processor container consumes in
// docker-compose/SENTINEL_E2E mode) instead of calling ProcessEvent inline.
//
// Every top-level identifier in this file is prefixed with Procgo/procgo so
// it cannot collide with identifiers other agents add to this same flat
// `integration` package.

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	dbmigrations "github.com/NurfitraPujo/sentinel/packages/db-migrations"
	sharednats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/golang/protobuf/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	gonats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// procgoSetupPostgres provisions its own isolated, killable Postgres
// testcontainer and applies the real goose migration chain to it, rather
// than using tc.Setup/tc.WithMigrations. Two reasons:
//
//  1. TestProcgoDegradationBufferSurvivesOutageNoLossNoDuplicates needs to
//     Stop/Start the container itself mid-test, so it needs the
//     *tc.PostgreSQLContainer handle regardless.
//  2. tc.Setup's migration step (unexported runInitSQLMigrations) has no
//     retry around its first connection, and the official postgres:15-alpine
//     image logs "database system is ready to accept connections" twice —
//     once for the initdb-triggered bootstrap instance, which then shuts
//     back down, and once for the real server start-up. A wait strategy
//     keyed on the first occurrence of that line (what
//     tests/integration/testcontainers/postgres.go uses) can return control
//     while the container is mid-restart, which surfaced here as
//     "FATAL: the database system is starting up (SQLSTATE 57P03)" on the
//     very first connection attempt. Retrying the initial connection (not
//     the container creation) below absorbs that race without needing to
//     touch tests/integration/testcontainers, which is out of this file's
//     scope.
func procgoSetupPostgres(t *testing.T) (*tc.PostgreSQLContainer, *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := tc.StartPostgreSQL(ctx)
	require.NoError(t, err, "failed to start an isolated postgres testcontainer")
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = pgContainer.Terminate(cleanupCtx)
	})

	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable",
		tc.DefaultUsername, tc.DefaultPassword, pgContainer.HostIP, pgContainer.HostPort, tc.DefaultDatabaseName)

	var pool *pgxpool.Pool
	require.Eventually(t, func() bool {
		p, poolErr := pgxpool.New(ctx, dsn)
		if poolErr != nil {
			return false
		}
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if pingErr := p.Ping(pingCtx); pingErr != nil {
			p.Close()
			return false
		}
		pool = p
		return true
	}, 60*time.Second, 500*time.Millisecond, "postgres testcontainer never became reachable")
	require.NotNil(t, pool)
	t.Cleanup(pool.Close)

	sqlDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer sqlDB.Close()
	require.NoError(t, sqlDB.PingContext(ctx))

	migDir, err := filepath.Abs("../../packages/db-migrations/migrations")
	require.NoError(t, err)
	// A dedicated table name keeps this isolated container's migration
	// ledger separate from every other ledger name already in use elsewhere
	// in this package (schema_migrations, processor_migrations,
	// seq_migrations, baseline_test_migrations, status_test_migrations —
	// see docs/memory/VERIFIED_STATE.md's shared-dev-DB corruption hazard,
	// which this isolated container is not exposed to, but the naming
	// collision risk is worth avoiding anyway).
	opts := dbmigrations.MigrationOptions{
		TableName: "procgo_test_migrations",
		Directory: migDir,
	}
	require.NoError(t, dbmigrations.RunMigrations(ctx, sqlDB, "up", opts))

	return pgContainer, pool
}

// procgoBuildEvent builds a minimal, valid ErrorEvent for project
// `projectName` with the given error class. Fingerprint is derived from the
// error class so distinct calls (with distinct error classes) create
// distinct issues.
func procgoBuildEvent(projectName, errorClass string) *sentinelv1.ErrorEvent {
	return &sentinelv1.ErrorEvent{
		ProjectKey:  projectName,
		Platform:    "go",
		Environment: "test",
		Message:     "procgo test error: " + errorClass,
		ErrorClass:  errorClass,
		Fingerprint: "procgo-fp-" + errorClass,
		Timestamp:   timestamppb.Now(),
	}
}

func procgoMarshal(t *testing.T, evt *sentinelv1.ErrorEvent) []byte {
	t.Helper()
	data, err := proto.Marshal(evt)
	require.NoError(t, err)
	return data
}

// procgoAdminPool opens a pool to the maintenance database, so it stays usable while the TEST
// database is refusing connections.
func procgoAdminPool(ctx context.Context, c *tc.PostgreSQLContainer) (*pgxpool.Pool, error) {
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%s/postgres?sslmode=disable",
		tc.DefaultUsername, tc.DefaultPassword, c.HostIP, c.HostPort)
	return pgxpool.New(ctx, dsn)
}

// procgoSetDBReachable simulates a database outage WITHOUT touching the container.
//
// Neither obvious approach works here. Stopping the container makes Docker assign a new random host
// port on restart, so every already-open DSN — this test's pool and the processor's own — keeps
// dialing an address nothing listens on and can never recover (podman preserves the mapping, which is
// exactly why Stop/Start passed locally and failed on every CI run). And `pg_ctl stop` inside the
// container kills PID 1, which takes the container down with it (observed: exit 137).
//
// Setting CONNECTION LIMIT 0 and terminating existing backends makes new connections fail while the
// container, the port mapping and the data all survive. That is what an application actually
// experiences during a database outage: connections refused, then accepted again.
func procgoSetDBReachable(ctx context.Context, admin *pgxpool.Pool, dbName string, reachable bool) error {
	limit := "0"
	if reachable {
		limit = "-1"
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf("ALTER DATABASE %q CONNECTION LIMIT %s", dbName, limit)); err != nil {
		return err
	}
	if !reachable {
		if _, err := admin.Exec(ctx,
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
			dbName); err != nil {
			return err
		}
	}
	return nil
}

// procgoWaitPostgresReady polls pool.Ping until it succeeds or timeout
// elapses. pgxpool reconnects lazily, so this is what actually detects that
// a restarted Postgres container is accepting connections again.
func procgoWaitPostgresReady(t *testing.T, pool *pgxpool.Pool, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return pool.Ping(ctx) == nil
	}, timeout, 250*time.Millisecond, "postgres did not become ready again in time")
}

// procgoSetupDedicatedNATS provisions (or, in docker-compose/SENTINEL_E2E
// mode, connects to) a NATS instance via tc.Setup, then creates a stream,
// subject and durable consumer name that are unique to this test run. This
// deliberately does NOT reuse the production ERROR_EVENTS stream/subject:
// docker-compose mode already has a real sentinel-processor container
// pull-consuming ERROR_EVENTS with its own durable consumer, and publishing
// or binding a second consumer there would race the real pipeline and/or
// double-process production traffic. tests/integration/shared_nats_test.go's
// newNATSPackageFixture established this exact isolation pattern; this
// mirrors it rather than reusing the shared setup_test.go stream.
func procgoSetupDedicatedNATS(t *testing.T) (natsURL string, js gonats.JetStreamContext, stream, subject, consumer string) {
	t.Helper()

	env := tc.Setup(t, tc.WithResources(tc.NATSResource))
	natsURL = env.NATSConfig.URL
	require.NotEmpty(t, natsURL, "NATS URL must be configured")

	nc, err := gonats.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err = nc.JetStream()
	require.NoError(t, err)

	id := fmt.Sprintf("%d", time.Now().UnixNano())
	stream = "PROCGO_DEGRADE_" + id
	subject = "procgo.degrade." + id
	consumer = "procgo-degrade-consumer-" + id

	_, err = js.AddStream(&gonats.StreamConfig{
		Name:     stream,
		Subjects: []string{subject},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = js.DeleteStream(stream) })

	return natsURL, js, stream, subject, consumer
}

// TestProcgoDegradationBufferSurvivesOutageNoLossNoDuplicates is the P4-2
// (S9) execution proof, updated for the buffer's removal (see the file-level
// comment above). It drives events through a real
// packages/shared-go/nats Publisher/Subscriber pair — not a direct
// ProcessEvent call — so the thing actually being proven is what production
// relies on for durability: D10's bounded-retry/backoff NATS redelivery
// picking a failed event back up once Postgres is reachable again, with the
// real service.ProcessorService.ProcessEvent as the subscriber's handler.
func TestProcgoDegradationBufferSurvivesOutageNoLossNoDuplicates(t *testing.T) {
	pgContainer, pool := procgoSetupPostgres(t)
	natsURL, _, stream, subject, consumer := procgoSetupDedicatedNATS(t)

	projectName := fmt.Sprintf("procgo-degrade-%d", time.Now().UnixNano())
	apiKey := projectName + "-key"
	projectID := createTestProject(t, pool, projectName, apiKey)

	svc := service.NewProcessorService(pool)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	publisher, err := sharednats.NewPublisher(ctx, sharednats.PublisherConfig{
		URL:     natsURL,
		Subject: subject,
		Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = publisher.Close() })

	subscriber, err := sharednats.NewSubscriber(ctx, sharednats.SubscriberConfig{
		URL:       natsURL,
		Stream:    stream,
		Subject:   subject,
		Consumer:  consumer,
		BatchSize: 1,
		BatchWait: 200 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscriber.Close() })

	// The handler mirrors exactly what apps/processor-go/main.go wires: a
	// non-nil, non-Permanent error from ProcessEvent (the "database
	// unavailable" sentinel below) is retried by the Subscriber with D10's
	// backoff (1s/5s/15s/30s/60s across MaxDeliver=5); nil Acks.
	require.NoError(t, subscriber.Subscribe(ctx, func(data []byte) error {
		return svc.ProcessEvent(ctx, data)
	}))

	const n = 5

	// 1. Take the DATABASE down — not the container. See procgoSetDBReachable for why.
	admin, adminErr := procgoAdminPool(ctx, pgContainer)
	require.NoError(t, adminErr)
	t.Cleanup(admin.Close)
	require.NoError(t, procgoSetDBReachable(ctx, admin, tc.DefaultDatabaseName, false))
	require.Eventually(t, func() bool {
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return pool.Ping(pingCtx) != nil
	}, 60*time.Second, 250*time.Millisecond, "database never became unreachable — the outage never started")

	// 2. Publish N events while the database is down. Each delivery attempt fails with "database
	// unavailable: event returned to NATS for bounded retry" and the Subscriber NAKs it with backoff;
	// there is no in-process buffer for ProcessEvent to succeed into anymore.
	for i := 0; i < n; i++ {
		evt := procgoBuildEvent(projectName, fmt.Sprintf("ProcgoDegradedError%d", i))
		require.NoError(t, publisher.Publish(ctx, procgoMarshal(t, evt)),
			"publish must succeed even though the database is down — NATS durability is independent of Postgres")
	}

	// 3. Bring it back. The port never changed, so both pools reconnect on their own.
	require.NoError(t, procgoSetDBReachable(ctx, admin, tc.DefaultDatabaseName, true))
	procgoWaitPostgresReady(t, pool, 2*time.Minute)

	// 4. Wait for D10's bounded-retry redelivery to land all N events, then
	// assert EXACTLY N occurrences exist for the N distinct degraded-error
	// issues: fewer than N would mean loss, more than N would mean
	// duplicates. The backoff schedule sums to ~51s across 5 attempts, well
	// inside this Eventually's window and Postgres's typical restart time.
	require.Eventually(t, func() bool {
		var count int
		qErr := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM error_occurrences eo
			   JOIN issues i ON i.id = eo.issue_id
			  WHERE i.project_id = $1 AND i.error_class LIKE 'ProcgoDegradedError%'`,
			projectID,
		).Scan(&count)
		return qErr == nil && count == n
	}, 90*time.Second, 250*time.Millisecond, "expected exactly %d occurrences to land after recovery", n)

	var finalOccCount, finalIssueCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences eo
		   JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1 AND i.error_class LIKE 'ProcgoDegradedError%'`,
		projectID,
	).Scan(&finalOccCount))
	assert.Equal(t, n, finalOccCount, "no loss, no duplicates: exactly N redelivered events must land exactly once each")

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issues WHERE project_id = $1 AND error_class LIKE 'ProcgoDegradedError%'`,
		projectID,
	).Scan(&finalIssueCount))
	assert.Equal(t, n, finalIssueCount, "each redelivered event should have created its own distinct issue")
}

// TestProcgoAlertingDispatchesWithinOneEvent is the P5-1 (S8) execution
// proof: configure an alert, ingest a NEW error class, and show the notifier
// being invoked (a captured sender) within one event, not five minutes.
func TestProcgoAlertingDispatchesWithinOneEvent(t *testing.T) {
	_, pool := procgoSetupPostgres(t)

	projectName := fmt.Sprintf("procgo-alert-%d", time.Now().UnixNano())
	apiKey := projectName + "-key"
	projectID := createTestProject(t, pool, projectName, apiKey)

	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO alert_configs (project_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled)
		 VALUES ($1, 'email', '{"to": "oncall@example.com"}'::jsonb, 1, 60, true)`,
		projectID,
	)
	require.NoError(t, err)

	// NewProcessorService performs alerts.NewDispatcher's synchronous initial
	// config load at construction (VERIFIED_STATE.md S8 item 4), so the row
	// inserted above is already loaded by the time this returns — no ticker
	// wait needed.
	svc := service.NewProcessorService(pool)

	type procgoCapturedAlert struct {
		cfg   alerts.AlertConfig
		alert alerts.Alert
	}
	var mu sync.Mutex
	var got []procgoCapturedAlert

	// Override the production sender (wired to real email/telegram notifiers
	// at construction, see ProcessorService.Alerts) with a capturing one, so
	// this test observes dispatch without needing real SMTP/Telegram
	// infrastructure.
	svc.Alerts().SetSender(func(_ context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, procgoCapturedAlert{cfg: *cfg, alert: *alert})
	})

	evt := procgoBuildEvent(projectName, "ProcgoNewAlertableError")
	require.NoError(t, svc.ProcessEvent(ctx, procgoMarshal(t, evt)))

	// Dispatch runs synchronously inside ProcessEvent -> processEventInternal
	// -> dispatchAlert -> alerts.Dispatcher.Dispatch -> sendAlert -> the
	// sender above (see ProcessorService.dispatchAlert and
	// alerts.Dispatcher.sendAlert): no goroutines, no polling needed. The
	// capture must already be populated the instant ProcessEvent returns,
	// which is itself part of the "within one event" proof.
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1,
		"a NEW issue with an enabled, threshold=1 alert config must dispatch exactly one alert within the same ProcessEvent call")
	assert.Equal(t, projectID, got[0].alert.ProjectID)
	assert.Contains(t, got[0].alert.Message, "ProcgoNewAlertableError")
	assert.Equal(t, "email", got[0].alert.Channel)
}
