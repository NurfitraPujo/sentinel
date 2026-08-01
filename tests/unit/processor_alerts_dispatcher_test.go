package unit

import (
	"bytes"
	"context"
	"log"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// alerts.ConfigRefreshInterval / ALERT_CONFIG_REFRESH_INTERVAL
// ---------------------------------------------------------------------------

// TestAlertsConfigRefreshInterval_Default asserts the un-tuned backstop
// interval is short enough to matter now that alert_config.changed exists as
// the fast path: the ticker is no longer the ONLY way a config becomes
// visible, so it no longer needs to be as conservative as the old
// hardcoded 5 minutes (dispatcher.go's defaultRefreshInterval doc comment).
// This also guards against an accidental regression back to a
// multi-minute default.
func TestAlertsConfigRefreshInterval_Default(t *testing.T) {
	got := alerts.ConfigRefreshInterval()
	assert.Less(t, got, 5*time.Minute, "default backstop interval should be well under the old 5-minute-only cadence")
	assert.Greater(t, got, time.Duration(0))
}

// TestAlertsConfigRefreshInterval_EnvOverride mirrors
// apps/ingestor-go/auth/apikey.go's APIKEY_CACHE_TTL env-tuning pattern:
// a valid duration string overrides the default, an empty/unparseable one
// falls back to it instead of panicking or zeroing out the ticker.
func TestAlertsConfigRefreshInterval_EnvOverride(t *testing.T) {
	t.Run("valid override wins", func(t *testing.T) {
		t.Setenv("ALERT_CONFIG_REFRESH_INTERVAL", "3s")
		assert.Equal(t, 3*time.Second, alerts.ConfigRefreshInterval())
	})

	t.Run("unparseable value falls back to default", func(t *testing.T) {
		t.Setenv("ALERT_CONFIG_REFRESH_INTERVAL", "not-a-duration")
		got := alerts.ConfigRefreshInterval()
		assert.Less(t, got, 5*time.Minute)
		assert.Greater(t, got, time.Duration(0))
	})

	t.Run("unset uses default", func(t *testing.T) {
		t.Setenv("ALERT_CONFIG_REFRESH_INTERVAL", "")
		got := alerts.ConfigRefreshInterval()
		assert.Less(t, got, 5*time.Minute)
		assert.Greater(t, got, time.Duration(0))
	})
}

// ---------------------------------------------------------------------------
// Dispatcher.StartInvalidationSubscriber
// ---------------------------------------------------------------------------

// TestAlertsStartInvalidationSubscriber_NilIsSafe proves the "optimization,
// never the sole path to correctness" requirement at the API-contract level:
// a nil subscriber (NATS unavailable at boot, mirroring
// apps/ingestor-go/main.go's api_key.invalidated handling when
// APIKEY_INVALIDATION_REQUIRED=false) must not panic and must log plainly
// that alert configs will take up to the backstop interval, rather than
// failing silently.
func TestAlertsStartInvalidationSubscriber_NilIsSafe(t *testing.T) {
	d := alerts.NewDispatcherForTest(nil)

	var buf bytes.Buffer
	origOutput := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOutput)
		log.SetFlags(origFlags)
	}()

	assert.NotPanics(t, func() {
		d.StartInvalidationSubscriber(nil)
	})

	assert.Contains(t, buf.String(), "alert_config.changed subscriber unavailable")
	assert.Contains(t, buf.String(), "periodic backstop")
}

// ---------------------------------------------------------------------------
// Concurrency: Dispatch reads the config map while a reload writes it
// ---------------------------------------------------------------------------

// TestAlertsDispatcher_ConfigReloadRaceFree exercises the exact lock pattern
// that guards Dispatcher.Dispatch's read of the config map against a
// concurrent reload's write: SetConfigsForTest takes the same d.configMu
// write lock refreshConfigs takes (dispatcher.go), so hammering it
// concurrently with Dispatch under `go test -race` proves that guard is
// actually sufficient rather than merely present. This does not require a
// live DB (refreshConfigs itself is exercised against a real Postgres by
// tests/integration/processor_alerts_test.go), only the locking discipline
// around the shared map, which is what a reload triggered by
// StartInvalidationSubscriber's message handler additionally contends on.
func TestAlertsDispatcher_ConfigReloadRaceFree(t *testing.T) {
	d := alerts.NewDispatcherForTest(nil)
	d.SetSenderForTest(func(ctx context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {})

	const projectID = "race-project"
	cfg := &alerts.AlertConfig{
		ProjectID:          projectID,
		Channel:            "email",
		ChannelConfig:      map[string]interface{}{"to": "oncall@example.test"},
		FrequencyThreshold: 1,
		FrequencyWindow:    time.Minute,
		Enabled:            true,
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: repeatedly swaps the whole config map, standing in for both
	// the periodic ticker's refreshConfigs and StartInvalidationSubscriber's
	// message-triggered refreshConfigs. Stops once the reader below signals
	// it is done, via `stop`.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				d.SetConfigsForTest(map[string]*alerts.AlertConfig{projectID: cfg})
			}
		}
	}()

	// Reader: Dispatch, exactly as ProcessorService.dispatchAlert calls it
	// per-occurrence.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			d.Dispatch(context.Background(), "issue-1", projectID, "TestError", "msg")
		}
		close(stop)
	}()

	wg.Wait()
	require.True(t, true, "reaching here under -race with no report proves the configMu guard holds")
}

// ---------------------------------------------------------------------------
// Two-layer alert resolution (packages/db-migrations/migrations/
// 1722100000_add_alert_config_org_layer.sql): an alert config is either
// PROJECT-SCOPED (organization_id + project_id) or ORGANIZATION-WIDE
// (organization_id, project_id NULL). The resolution rule for an event in
// project P belonging to organization O is:
//
//	WHERE enabled AND (project_id = P OR (project_id IS NULL AND organization_id = O))
//
// implemented by Dispatcher.resolveConfigs over the caches SetConfigsForTest /
// SetOrgConfigsForTest / SetProjectOrgForTest populate directly (dispatcher.go).
// Both layers are a UNION, never one overriding the other, except that two
// configs resolving to the identical destination (channel + "to"/"chat_id")
// are deduplicated to a single send.
// ---------------------------------------------------------------------------

const (
	resolutionProjectID = "resolution-project"
	resolutionOrgID     = "resolution-org"
)

// captureDispatch wires a Dispatcher with a capturing sender and returns it
// along with a snapshot function, so resolution tests can assert exactly
// which configs (by ChannelConfig target) actually sent.
func captureDispatch(t *testing.T) (*alerts.Dispatcher, func() []*alerts.AlertConfig) {
	t.Helper()
	d := alerts.NewDispatcherForTest(nil)

	var mu sync.Mutex
	var sent []*alerts.AlertConfig
	d.SetSenderForTest(func(ctx context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, cfg)
	})

	return d, func() []*alerts.AlertConfig {
		mu.Lock()
		defer mu.Unlock()
		out := make([]*alerts.AlertConfig, len(sent))
		copy(out, sent)
		return out
	}
}

// projectScopedConfig and orgWideConfig build threshold=1 configs (so a single Dispatch call always
// fires immediately) that differ only in which layer they belong to and, via chatOrEmailTo, which
// destination they route to.
func projectScopedConfig(id, to string) *alerts.AlertConfig {
	return &alerts.AlertConfig{
		ID:                 id,
		ProjectID:          resolutionProjectID,
		OrganizationID:     resolutionOrgID,
		Channel:            "email",
		ChannelConfig:      map[string]interface{}{"to": to},
		FrequencyThreshold: 1,
		FrequencyWindow:    time.Minute,
		Enabled:            true,
	}
}

func orgWideConfig(id, to string) *alerts.AlertConfig {
	return &alerts.AlertConfig{
		ID:                 id,
		OrganizationID:     resolutionOrgID,
		Channel:            "email",
		ChannelConfig:      map[string]interface{}{"to": to},
		FrequencyThreshold: 1,
		FrequencyWindow:    time.Minute,
		Enabled:            true,
	}
}

// TestAlertsDispatcher_ResolveOrgWideOnly proves an organization-wide config (no project-scoped
// config exists for this project at all) still fires — the whole point of the org layer being a
// safety net that applies even to a project nobody has configured individually.
func TestAlertsDispatcher_ResolveOrgWideOnly(t *testing.T) {
	d, sent := captureDispatch(t)
	d.SetConfigsForTest(map[string]*alerts.AlertConfig{})
	d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{
		resolutionOrgID: {orgWideConfig("org-cfg", "org-oncall@example.test")},
	})
	d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

	d.Dispatch(context.Background(), "issue-1", resolutionProjectID, "TestError", "msg")

	got := sent()
	require.Len(t, got, 1)
	assert.Equal(t, "org-oncall@example.test", got[0].ChannelConfig["to"])
}

// TestAlertsDispatcher_ResolveProjectScopedOnly proves a project-scoped config fires when there is no
// organization-wide config at all (the pre-existing, single-layer behavior must still work).
func TestAlertsDispatcher_ResolveProjectScopedOnly(t *testing.T) {
	d, sent := captureDispatch(t)
	d.SetConfigsForTest(map[string]*alerts.AlertConfig{
		resolutionProjectID: projectScopedConfig("proj-cfg", "project-oncall@example.test"),
	})
	d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{})
	d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

	d.Dispatch(context.Background(), "issue-1", resolutionProjectID, "TestError", "msg")

	got := sent()
	require.Len(t, got, 1)
	assert.Equal(t, "project-oncall@example.test", got[0].ChannelConfig["to"])
}

// TestAlertsDispatcher_ResolveBothLayersUnion proves the contract's core rule: both layers fire. An
// org-wide config and a project-scoped config routing to DIFFERENT destinations must both be sent —
// this is a union, not "project overrides org".
func TestAlertsDispatcher_ResolveBothLayersUnion(t *testing.T) {
	d, sent := captureDispatch(t)
	d.SetConfigsForTest(map[string]*alerts.AlertConfig{
		resolutionProjectID: projectScopedConfig("proj-cfg", "project-oncall@example.test"),
	})
	d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{
		resolutionOrgID: {orgWideConfig("org-cfg", "org-oncall@example.test")},
	})
	d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

	d.Dispatch(context.Background(), "issue-1", resolutionProjectID, "TestError", "msg")

	got := sent()
	require.Len(t, got, 2, "both an org-wide and a project-scoped config apply and must both fire")
	targets := []string{got[0].ChannelConfig["to"].(string), got[1].ChannelConfig["to"].(string)}
	assert.ElementsMatch(t, []string{"project-oncall@example.test", "org-oncall@example.test"}, targets)
}

// TestAlertsDispatcher_ResolveSameDestinationDeduplicated proves the other half of the contract: when
// the org-wide config and the project-scoped config resolve to the SAME destination (same channel,
// same "to"), the event is sent exactly once, not twice. Being paged twice for one event is how
// alerting gets muted.
func TestAlertsDispatcher_ResolveSameDestinationDeduplicated(t *testing.T) {
	d, sent := captureDispatch(t)
	d.SetConfigsForTest(map[string]*alerts.AlertConfig{
		resolutionProjectID: projectScopedConfig("proj-cfg", "shared-oncall@example.test"),
	})
	d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{
		resolutionOrgID: {orgWideConfig("org-cfg", "shared-oncall@example.test")},
	})
	d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

	d.Dispatch(context.Background(), "issue-1", resolutionProjectID, "TestError", "msg")

	got := sent()
	require.Len(t, got, 1, "two configs resolving to the same channel+target must be deduplicated to one send")
	assert.Equal(t, "shared-oncall@example.test", got[0].ChannelConfig["to"])
}

// TestAlertsDispatcher_ResolveDisabledConfigIgnored proves a disabled config in either layer is
// ignored while an enabled config in the other layer still fires. refreshConfigs' real query already
// filters WHERE enabled = true, so this exercises Dispatch's own defensive cfg.Enabled check, reached
// via configs injected directly (bypassing that DB-level filter), the same way a stale cache entry or
// a test seam would.
func TestAlertsDispatcher_ResolveDisabledConfigIgnored(t *testing.T) {
	t.Run("disabled org-wide config is ignored, enabled project-scoped config still fires", func(t *testing.T) {
		d, sent := captureDispatch(t)
		projCfg := projectScopedConfig("proj-cfg", "project-oncall@example.test")
		orgCfg := orgWideConfig("org-cfg", "org-oncall@example.test")
		orgCfg.Enabled = false

		d.SetConfigsForTest(map[string]*alerts.AlertConfig{resolutionProjectID: projCfg})
		d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{resolutionOrgID: {orgCfg}})
		d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

		d.Dispatch(context.Background(), "issue-1", resolutionProjectID, "TestError", "msg")

		got := sent()
		require.Len(t, got, 1)
		assert.Equal(t, "project-oncall@example.test", got[0].ChannelConfig["to"])
	})

	t.Run("disabled project-scoped config is ignored, enabled org-wide config still fires", func(t *testing.T) {
		d, sent := captureDispatch(t)
		projCfg := projectScopedConfig("proj-cfg", "project-oncall@example.test")
		projCfg.Enabled = false
		orgCfg := orgWideConfig("org-cfg", "org-oncall@example.test")

		d.SetConfigsForTest(map[string]*alerts.AlertConfig{resolutionProjectID: projCfg})
		d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{resolutionOrgID: {orgCfg}})
		d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

		d.Dispatch(context.Background(), "issue-2", resolutionProjectID, "TestError", "msg")

		got := sent()
		require.Len(t, got, 1)
		assert.Equal(t, "org-oncall@example.test", got[0].ChannelConfig["to"])
	})
}

// TestAlertsDispatcher_ResolveMultipleOrgWideRulesUnion proves D04/D21: multiple org-wide rules for the
// SAME organization (e.g. one email + one telegram rule, both created through the dashboard's "create
// org-wide alert" UI, which has no duplicate check) must ALL fire, not just whichever refreshConfigs'
// query happened to return last for that organization id. Before this fix, orgConfigs was keyed
// map[string]*AlertConfig — a single-value map — so the second write for the same key silently
// overwrote the first and only one config's destination ever fired.
func TestAlertsDispatcher_ResolveMultipleOrgWideRulesUnion(t *testing.T) {
	d, sent := captureDispatch(t)
	d.SetConfigsForTest(map[string]*alerts.AlertConfig{})

	emailCfg := orgWideConfig("org-email-cfg", "org-oncall@example.test")
	telegramCfg := &alerts.AlertConfig{
		ID:                 "org-telegram-cfg",
		OrganizationID:     resolutionOrgID,
		Channel:            "telegram",
		ChannelConfig:      map[string]interface{}{"chat_id": "123456"},
		FrequencyThreshold: 1,
		FrequencyWindow:    time.Minute,
		Enabled:            true,
	}

	d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{
		resolutionOrgID: {emailCfg, telegramCfg},
	})
	d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

	d.Dispatch(context.Background(), "issue-1", resolutionProjectID, "TestError", "msg")

	got := sent()
	require.Len(t, got, 2, "both org-wide rules (email and telegram) must fire, not just the last one loaded")
	channels := []string{got[0].Channel, got[1].Channel}
	assert.ElementsMatch(t, []string{"email", "telegram"}, channels)
}

// TestAlertsDispatcher_ResolveMultipleProjectScopedConfigs proves that TWO project-scoped rules on
// the SAME project both fire (D04, project half).
//
// `Dispatcher.configs` was map[string]*AlertConfig — one config per project id — while nothing in
// the schema constrains alert_configs to one row per project and the dashboard UI lets a user create
// N rules (e.g. one email destination plus one telegram). Every rule but the last one refreshConfigs
// happened to scan was silently discarded: no error, no log, the alert simply never arrived. The
// organization-wide half of this bug was fixed first and left the project half in place.
//
// This test uses SetProjectConfigsForTest, which — like SetOrgConfigsForTest — bypasses the SQL. See
// TestProcessorAlerts_MultipleProjectScopedConfigsLoadFromSQL in tests/integration for the load-path
// coverage that this injection cannot provide.
func TestAlertsDispatcher_ResolveMultipleProjectScopedConfigs(t *testing.T) {
	d, sent := captureDispatch(t)
	d.SetProjectConfigsForTest(map[string][]*alerts.AlertConfig{
		resolutionProjectID: {
			projectScopedConfig("proj-cfg-a", "team-a@example.test"),
			projectScopedConfig("proj-cfg-b", "team-b@example.test"),
		},
	})
	d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{})
	d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

	d.Dispatch(context.Background(), "issue-1", resolutionProjectID, "TestError", "msg")

	got := sent()
	require.Len(t, got, 2, "both project-scoped rules must fire; a single-value map drops one silently")

	destinations := []string{}
	for _, cfg := range got {
		destinations = append(destinations, cfg.ChannelConfig["to"].(string))
	}
	assert.ElementsMatch(t, []string{"team-a@example.test", "team-b@example.test"}, destinations)
}

// TestAlertsDispatcher_MultipleProjectConfigsSameDestinationDedup proves the union above still
// deduplicates by destination, so two project rules pointing at the same inbox send once — the
// dedup guarantee resolveConfigs already made for the org layer must not have been lost when the
// project layer became a slice.
func TestAlertsDispatcher_MultipleProjectConfigsSameDestinationDedup(t *testing.T) {
	d, sent := captureDispatch(t)
	d.SetProjectConfigsForTest(map[string][]*alerts.AlertConfig{
		resolutionProjectID: {
			projectScopedConfig("proj-cfg-a", "same@example.test"),
			projectScopedConfig("proj-cfg-b", "same@example.test"),
		},
	})
	d.SetOrgConfigsForTest(map[string][]*alerts.AlertConfig{})
	d.SetProjectOrgForTest(map[string]string{resolutionProjectID: resolutionOrgID})

	d.Dispatch(context.Background(), "issue-1", resolutionProjectID, "TestError", "msg")

	require.Len(t, sent(), 1, "same channel+destination must dedup to a single send")
}
