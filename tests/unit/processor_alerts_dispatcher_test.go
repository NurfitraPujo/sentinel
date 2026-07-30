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
