package integration

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// alertCapture is a thread-safe recorder for alerts sent by the dispatcher
// via the senderForTest test seam.
type alertCapture struct {
	mu    sync.Mutex
	items []capturedAlert
}

type capturedAlert struct {
	cfg   alerts.AlertConfig
	alert alerts.Alert
}

func (a *alertCapture) sender() func(ctx context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {
	return func(ctx context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {
		a.mu.Lock()
		defer a.mu.Unlock()
		var cfgCopy alerts.AlertConfig
		var alertCopy alerts.Alert
		if cfg != nil {
			cfgCopy = *cfg
		}
		if alert != nil {
			alertCopy = *alert
		}
		a.items = append(a.items, capturedAlert{cfg: cfgCopy, alert: alertCopy})
	}
}

func (a *alertCapture) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.items)
}

func (a *alertCapture) snapshot() []capturedAlert {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]capturedAlert, len(a.items))
	copy(out, a.items)
	return out
}

// dispatchPool returns a dedicated pgxpool for the dispatcher under test.
// The pool is created and closed outside the synctest bubble so that the
// pool's background health check goroutine does not leak into the bubble
// (which would deadlock when the bubble tears down).
func dispatchPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	env := tc.Setup(t, tc.WithResources(tc.PostgresResource), tc.WithMigrations(true))
	require.NotNil(t, env.PGPool, "PGPool must be initialized")
	ensureAlertsSchema(t, env.PGPool)
	return env.PGPool
}

// ensureAlertsSchema bootstraps the projects and alert_configs tables when
// they are missing. In docker-compose mode TestMain assumes the database is
// pre-initialized, but the sentinel-pg-test container may be empty, so the
// alerts tests apply the init.sql bootstrap themselves.
func ensureAlertsSchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'projects')`,
	).Scan(&exists)
	if err != nil {
		return
	}
	if exists {
		return
	}

	sqlBytes, err := os.ReadFile(alertsFindProjectRoot(t) + "/scripts/db/init.sql")
	if err != nil {
		t.Logf("could not read init.sql (%v); alerts tests will likely fail", err)
		return
	}
	if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
		t.Logf("could not apply init.sql (%v); alerts tests will likely fail", err)
	}
}

// alertsFindProjectRoot walks up from the working directory until it finds
// a go.mod file. It mirrors the helper in setup_test.go but is kept local so
// the alerts tests do not depend on internal test utilities.
func alertsFindProjectRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(dir + "/go.mod"); err == nil {
			return dir
		}
		parent := dir + "/.."
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// postgresConnectionParams resolves the PostgreSQL connection parameters in
// either docker-compose mode (where setup_test.go sets env vars and leaves
// testConfig empty) or testcontainers mode (where GetTestConfig() returns the
// ephemeral container's address). Returns ("", "", ...) when PostgreSQL is
// not available so the caller can skip.
func postgresConnectionParams() (host, port, user, password, db string) {
	if cfg, _ := GetTestConfig(); cfg.Host != "" {
		return cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.DB
	}
	host = os.Getenv("POSTGRES_HOST")
	if host == "" {
		return "", "", "", "", ""
	}
	return host,
		os.Getenv("POSTGRES_PORT"),
		os.Getenv("POSTGRES_USER"),
		os.Getenv("POSTGRES_PASSWORD"),
		os.Getenv("POSTGRES_DB")
}

// seedProject creates a projects row and returns its UUID. The row is cleaned
// up in t.Cleanup; cascading deletes remove alert_configs associated with it.
func seedProject(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	nonce := time.Now().UnixNano()

	// The project needs an organization. projects.organization_id is nullable, so this used to omit it
	// and produce an orphan project — which nothing else in the system expects, and which broke the
	// moment alert_configs gained a NOT NULL organization_id derived from the project (migration
	// 1722100000): the derivation returned NULL and the insert failed with 23502.
	var orgID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id::text`,
		fmt.Sprintf("u14-org-%d", nonce),
	).Scan(&orgID))
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM organizations WHERE id = $1`, orgID)
	})

	var projectID string
	err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash, organization_id)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'), $4)
		 RETURNING id::text`,
		fmt.Sprintf("u14-project-%d", nonce),
		fmt.Sprintf("u14-api-key-%d", nonce),
		fmt.Sprintf("u14-api-key-%d", nonce),
		orgID,
	).Scan(&projectID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID)
	})
	return projectID
}

// seedAlertConfig inserts an alert_configs row referencing the given project.
// threshold / windowSeconds encode the matching frequency rule. enabled
// controls whether the dispatcher loads the config in the first place.
func seedAlertConfig(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string, threshold int, windowSeconds int, enabled bool) {
	t.Helper()
	_, err := pool.Exec(ctx,
		// organization_id is NOT NULL as of migration 1722100000; derive it from the project so this seed
		// stays correct without the caller having to know the organization.
		`INSERT INTO alert_configs (project_id, organization_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled)
		 VALUES ($1, (SELECT organization_id FROM projects WHERE id = $1), 'email', '{}'::jsonb, $2, $3, $4)`,
		projectID, threshold, windowSeconds, enabled,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM alert_configs WHERE project_id = $1`, projectID)
	})
}

// seedOrgAlertConfig inserts an ORGANIZATION-WIDE alert_configs row (project_id IS NULL,
// organization_id = orgID) — the shape 1722100000_add_alert_config_org_layer.sql introduced and that
// seedAlertConfig above cannot produce (it always derives organization_id from a non-null project_id).
// channel and channelConfigJSON let a caller seed two rules for the same organization that route to
// different destinations (e.g. one "email" row and one "telegram" row), which is exactly the shape
// D04/D21 found nothing in the schema constrains to one row.
func seedOrgAlertConfig(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID string, channel string, channelConfigJSON string, threshold int, windowSeconds int, enabled bool) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO alert_configs (project_id, organization_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled)
		 VALUES (NULL, $1, $2, $3::jsonb, $4, $5, $6)`,
		orgID, channel, channelConfigJSON, threshold, windowSeconds, enabled,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM alert_configs WHERE organization_id = $1 AND project_id IS NULL`, orgID)
	})
}

// seedOrganizationForOrgConfig creates a bare organizations row (no project) so org-wide alert configs
// can be seeded against it directly, and seeds a project belonging to that same organization so
// Dispatch (which is only ever called with a projectID) can resolve back to orgID via
// Dispatcher.projectOrg, exactly as refreshConfigs' real "SELECT id, organization_id FROM projects"
// query populates it in production.
func seedOrganizationForOrgConfig(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (orgID string, projectID string) {
	t.Helper()
	projectID = seedProject(t, ctx, pool)
	err := pool.QueryRow(ctx, `SELECT organization_id::text FROM projects WHERE id = $1`, projectID).Scan(&orgID)
	require.NoError(t, err)
	return orgID, projectID
}

// configFromRow builds an AlertConfig from the package's known shape so that
// tests can mirror what refreshConfigs populates from the alert_configs row.
func configFromRow(projectID string, threshold int, windowSeconds int, enabled bool) *alerts.AlertConfig {
	return &alerts.AlertConfig{
		ProjectID:          projectID,
		Channel:            "email",
		ChannelConfig:      map[string]interface{}{},
		FrequencyThreshold: threshold,
		FrequencyWindow:    time.Duration(windowSeconds) * time.Second,
		Enabled:            enabled,
	}
}

// runAlertsTest creates a dispatch pool outside the synctest bubble and then
// runs the inner test function inside the bubble. The pool is closed when
// the outer test ends, after the bubble has torn down. This avoids the
// deadlock that occurs when the pool's background health check goroutine
// outlives the bubble.
func runAlertsTest(t *testing.T, inner func(t *testing.T, pool *pgxpool.Pool)) {
	t.Helper()
	pool := dispatchPool(t)
	synctest.Test(t, func(t *testing.T) {
		inner(t, pool)
	})
}

func TestAlertsDispatcher_FormatAlertMessageShortMessage(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 1, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())
		d.SetConfigsForTest(map[string]*alerts.AlertConfig{
			projectID: configFromRow(projectID, 1, 60, true),
		})

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "short message")

		got := cap.snapshot()
		require.Len(t, got, 1, "expected exactly one alert for threshold=1")
		assert.Equal(t, "[1x] TestError: short message", got[0].alert.Message)
	})
}

func TestAlertsDispatcher_FormatAlertMessageTruncates(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 1, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())
		d.SetConfigsForTest(map[string]*alerts.AlertConfig{
			projectID: configFromRow(projectID, 1, 60, true),
		})

		long := make([]byte, 250)
		for i := range long {
			long[i] = 'a'
		}
		d.Dispatch(ctx, "issue-1", projectID, "TestError", string(long))

		got := cap.snapshot()
		require.Len(t, got, 1)
		assert.False(t, len(got[0].alert.Message) > 130,
			"formatted message should be truncated, got %q", got[0].alert.Message)
		assert.Contains(t, got[0].alert.Message, "...", "truncated message should end with ...")
		assert.Contains(t, got[0].alert.Message, "TestError")
	})
}

func TestAlertsDispatcher_DispatchNoConfigDoesNothing(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())
		d.SetConfigsForTest(map[string]*alerts.AlertConfig{})

		d.Dispatch(ctx, "issue-1", "nonexistent-project", "TestError", "msg")

		assert.Equal(t, 0, cap.count(), "no alert should be sent when no config is loaded")
	})
}

func TestAlertsDispatcher_DispatchDisabledConfigDoesNothing(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 1, 60, false)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())
		d.SetConfigsForTest(map[string]*alerts.AlertConfig{
			projectID: configFromRow(projectID, 1, 60, false),
		})

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")

		assert.Equal(t, 0, cap.count(),
			"disabled configs should be skipped by Dispatch's enabled check")
	})
}

func TestAlertsDispatcher_DispatchBelowThresholdDoesNotSend(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 3, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())
		d.SetConfigsForTest(map[string]*alerts.AlertConfig{
			projectID: configFromRow(projectID, 3, 60, true),
		})

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")

		assert.Equal(t, 0, cap.count(), "below threshold should not send an alert")
	})
}

func TestAlertsDispatcher_DispatchAtThresholdSendsAlert(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 2, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())
		d.SetConfigsForTest(map[string]*alerts.AlertConfig{
			projectID: configFromRow(projectID, 2, 60, true),
		})

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "first")
		d.Dispatch(ctx, "issue-1", projectID, "TestError", "second")

		got := cap.snapshot()
		require.Len(t, got, 1, "expected exactly one alert when threshold is reached")
		assert.Equal(t, "issue-1", got[0].alert.IssueID)
		assert.Equal(t, projectID, got[0].alert.ProjectID)
		assert.Equal(t, "email", got[0].alert.Channel)
		assert.Contains(t, got[0].alert.Message, "[2x]")
		assert.Contains(t, got[0].alert.Message, "TestError")

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "third")
		assert.Equal(t, 1, cap.count(), "counter should reset after an alert is sent")
	})
}

func TestAlertsDispatcher_CounterResetsAfterWindow(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 3, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())
		d.SetConfigsForTest(map[string]*alerts.AlertConfig{
			projectID: configFromRow(projectID, 3, 60, true),
		})

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		require.Equal(t, 0, cap.count())

		// Advance past the 60s window. The counter resets, so reaching the
		// threshold takes another 3 calls from this point in synthetic time.
		time.Sleep(61 * time.Second)

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		require.Equal(t, 0, cap.count(), "first two dispatches after window reset should not trigger")

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		got := cap.snapshot()
		require.Len(t, got, 1, "counter reset should require a fresh threshold-worth of dispatches")
		assert.Contains(t, got[0].alert.Message, "[3x]", "alert should reflect the new count after reset")
	})
}

func TestAlertsDispatcher_RefreshConfigsLoadsSeededRow(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 1, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())

		d.RefreshConfigsForTest(ctx)

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		got := cap.snapshot()
		require.Len(t, got, 1, "refreshConfigs should populate the configs map from the seeded row")
		assert.Equal(t, projectID, got[0].alert.ProjectID)
	})
}

func TestAlertsDispatcher_RefreshConfigsSkipsDisabledRows(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 1, 60, false)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())

		d.RefreshConfigsForTest(ctx)

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		assert.Equal(t, 0, cap.count(),
			"refreshConfigs should filter out rows where enabled = false")
	})
}

func TestAlertsDispatcher_RefreshConfigsPicksUpRowInsertedAfterStart(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())

		d.RefreshConfigsForTest(ctx)
		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		require.Equal(t, 0, cap.count())

		seedAlertConfig(t, ctx, pool, projectID, 1, 60, true)
		d.RefreshConfigsForTest(ctx)

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		got := cap.snapshot()
		require.Len(t, got, 1,
			"a row inserted after the first refresh should be visible after the next refresh")
		assert.Equal(t, projectID, got[0].alert.ProjectID)
	})
}

func TestAlertsDispatcher_RefreshConfigsHandlesQueryError(t *testing.T) {
	// refreshConfigs logs and returns when the query fails. Drive it with a
	// canceled context so the underlying pgxpool.Query aborts immediately.
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 1, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())

		// Cancel the context before calling refreshConfigs so the query
		// fails. The dispatcher should log the error and leave the configs
		// map empty, leaving Dispatch as a no-op.
		canceledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		d.RefreshConfigsForTest(canceledCtx)

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		assert.Equal(t, 0, cap.count(),
			"a failed refresh should leave the configs map empty so Dispatch does nothing")
	})
}

func TestAlertsDispatcher_SendAlertDefaultBranch(t *testing.T) {
	// When senderForTest is nil, sendAlert should fall back to its
	// log.Printf implementation. The test only verifies that no panics
	// occur and that the call returns normally.
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 1, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		// Intentionally do not call SetSenderForTest.
		d.SetConfigsForTest(map[string]*alerts.AlertConfig{
			projectID: configFromRow(projectID, 1, 60, true),
		})

		assert.NotPanics(t, func() {
			d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		}, "Dispatch should fall back to the log.Printf sendAlert path without panicking")
	})
}

// TestAlertsDispatcher_RefreshConfigsMultipleOrgWideRulesBothFire is the acceptance test for D04/D21:
// TWO organization-wide alert_configs rows, seeded through REAL SQL (not SetOrgConfigsForTest, which
// every prior org-wide test used and which bypasses both the SQL and the map keying the defect lives
// in), for the SAME organization with DIFFERENT destinations (one email, one telegram). refreshConfigs
// must load both, and Dispatch must fire both — not silently keep only whichever row Postgres happened
// to return last, which is what map[string]*AlertConfig keying did before this fix.
func TestAlertsDispatcher_RefreshConfigsMultipleOrgWideRulesBothFire(t *testing.T) {
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		orgID, projectID := seedOrganizationForOrgConfig(t, ctx, pool)

		seedOrgAlertConfig(t, ctx, pool, orgID, "email", `{"to":"org-oncall@example.test"}`, 1, 60, true)
		seedOrgAlertConfig(t, ctx, pool, orgID, "telegram", `{"chat_id":"987654"}`, 1, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())

		d.RefreshConfigsForTest(ctx)

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")

		got := cap.snapshot()
		require.Len(t, got, 2, "both org-wide rules (email and telegram, different destinations) must fire, not just one")
		channels := []string{got[0].cfg.Channel, got[1].cfg.Channel}
		assert.ElementsMatch(t, []string{"email", "telegram"}, channels)
	})
}

func TestAlertsDispatcher_NewDispatcherStarts(t *testing.T) {
	// NewDispatcher is the production constructor: it builds a dispatcher
	// and spawns the loadConfigs background loop. Exercise it outside the
	// synctest bubble so the bg loop's ticker is associated with real time.
	// The test only asserts that the constructor returns a usable dispatcher.
	pool := dispatchPool(t)
	d := alerts.NewDispatcher(pool)
	require.NotNil(t, d)
}

func TestAlertsDispatcher_LoadConfigsRespectsContextCancel(t *testing.T) {
	// loadConfigs returns when the supplied context is cancelled. Run the
	// loop inside the synctest bubble, cancel the context, and let the
	// bubble tear down to confirm the loop's ctx.Done branch fires.
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		d := alerts.NewDispatcherForTest(pool)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			d.LoadConfigsForTest(ctx)
			close(done)
		}()

		// Let the goroutine park on the ticker, then cancel.
		synctest.Wait()
		cancel()
		synctest.Wait()

		select {
		case <-done:
		default:
			t.Fatal("loadConfigs did not return after the context was cancelled")
		}
	})
}

func TestAlertsDispatcher_LoadConfigsTickerFires(t *testing.T) {
	// The ticker's <-ticker.C branch drives refreshConfigs. Inside the
	// synctest bubble, time.Sleep advances the synthetic clock so the
	// backstop ticker (alerts.ConfigRefreshInterval(); a periodic backstop
	// now that StartInvalidationSubscriber gives config changes a
	// sub-second path via NATS — see dispatcher.go's defaultRefreshInterval
	// doc comment) fires immediately, exercising the otherwise unreachable
	// branch.
	runAlertsTest(t, func(t *testing.T, pool *pgxpool.Pool) {
		cap := &alertCapture{}
		ctx := context.Background()

		projectID := seedProject(t, ctx, pool)
		seedAlertConfig(t, ctx, pool, projectID, 1, 60, true)

		d := alerts.NewDispatcherForTest(pool)
		d.SetSenderForTest(cap.sender())

		ctx2, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			d.LoadConfigsForTest(ctx2)
			close(done)
		}()

		// Advance synthetic time past one ticker period so refreshConfigs
		// fires and the seeded configs become visible to Dispatch.
		synctest.Wait()
		time.Sleep(alerts.ConfigRefreshInterval() + time.Second)
		synctest.Wait()

		d.Dispatch(ctx, "issue-1", projectID, "TestError", "msg")
		assert.Equal(t, 1, cap.count(),
			"the backstop ticker should have fired refreshConfigs which loaded the seeded row")

		cancel()
		<-done
	})
}
