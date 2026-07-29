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
	dbmigrations "github.com/NurfitraPujo/sentinel/packages/db-migrations"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/golang/protobuf/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
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

// TestProcgoDegradationBufferSurvivesOutageNoLossNoDuplicates is the P4-2
// (S9) execution proof. It exercises the real
// service.ProcessorService.ProcessEvent path — not GracefulDegradation in
// isolation, which tests/unit already covers — against a real, killable
// testcontainers Postgres, so the tri-state fix in
// degradation.GracefulDegradation.Evaluate and its wiring through
// ProcessorService.ProcessEvent/processEventInternal are proven together,
// the way the running binary actually uses them.
func TestProcgoDegradationBufferSurvivesOutageNoLossNoDuplicates(t *testing.T) {
	pgContainer, pool := procgoSetupPostgres(t)

	projectName := fmt.Sprintf("procgo-degrade-%d", time.Now().UnixNano())
	apiKey := projectName + "-key"
	projectID := createTestProject(t, pool, projectName, apiKey)

	svc := service.NewProcessorService(pool)

	const n = 5
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// 1. Stop Postgres mid-stream.
	require.NoError(t, pgContainer.Stop(ctx, nil))

	// 2. Send N events while it is down. Each must be safely buffered (nil
	// error, i.e. degradation.StatusBuffered) — never surfaced as a failure,
	// and processEventInternal must never run for them (there is nothing
	// for it to talk to; if it ran, ProcessEvent would return a connection
	// error instead of nil here).
	for i := 0; i < n; i++ {
		evt := procgoBuildEvent(projectName, fmt.Sprintf("ProcgoDegradedError%d", i))
		err := svc.ProcessEvent(ctx, procgoMarshal(t, evt))
		require.NoError(t, err, "event %d should be buffered (nil error) while the database is down", i)
	}

	// 3. Restart Postgres and wait for it to accept connections again.
	require.NoError(t, pgContainer.Start(ctx))
	procgoWaitPostgresReady(t, pool, 90*time.Second)

	// 4. Send one more "trigger" event. Its Evaluate() call is what observes
	// the down->up transition and kicks off the async buffer flush (see
	// degradation.GracefulDegradation.triggerAsyncFlush). Production traffic
	// provides this naturally via the next message NATS delivers; the test
	// provides it explicitly.
	triggerEvt := procgoBuildEvent(projectName, "ProcgoTriggerEvent")
	require.NoError(t, svc.ProcessEvent(ctx, procgoMarshal(t, triggerEvt)),
		"the trigger event itself must process live now that the database is back")

	// 5. Wait for the async flush to land all N buffered events, then assert
	// EXACTLY N occurrences exist for the N distinct degraded-error issues:
	// fewer than N would mean loss, more than N would mean duplicates.
	require.Eventually(t, func() bool {
		var count int
		qErr := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM error_occurrences eo
			   JOIN issues i ON i.id = eo.issue_id
			  WHERE i.project_id = $1 AND i.error_class LIKE 'ProcgoDegradedError%'`,
			projectID,
		).Scan(&count)
		return qErr == nil && count == n
	}, 60*time.Second, 250*time.Millisecond, "expected exactly %d occurrences to land after recovery", n)

	var finalOccCount, finalIssueCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_occurrences eo
		   JOIN issues i ON i.id = eo.issue_id
		  WHERE i.project_id = $1 AND i.error_class LIKE 'ProcgoDegradedError%'`,
		projectID,
	).Scan(&finalOccCount))
	assert.Equal(t, n, finalOccCount, "no loss, no duplicates: exactly N buffered events must land exactly once each")

	require.NoError(t, pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM issues WHERE project_id = $1 AND error_class LIKE 'ProcgoDegradedError%'`,
		projectID,
	).Scan(&finalIssueCount))
	assert.Equal(t, n, finalIssueCount, "each buffered event should have created its own distinct issue")
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
