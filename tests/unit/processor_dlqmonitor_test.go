package unit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/dlqmonitor"
	sharedNats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// dlqmonitor.ThresholdsFromEnv
// ---------------------------------------------------------------------------

// TestDLQMonitorThresholdsFromEnv_Default asserts the un-tuned thresholds match the documented
// defaults (dlqmonitor.DefaultDepthThreshold / DefaultCriticalAge) so a regression here — e.g. an
// accidental change that makes the DLQ monitor page on every single poison message again — fails a
// test rather than surfacing as alert fatigue in production.
func TestDLQMonitorThresholdsFromEnv_Default(t *testing.T) {
	t.Setenv("PROCESSOR_DLQ_DEPTH_THRESHOLD", "")
	t.Setenv("PROCESSOR_DLQ_CRITICAL_AGE", "")

	got := dlqmonitor.ThresholdsFromEnv()
	assert.Equal(t, uint64(dlqmonitor.DefaultDepthThreshold), got.Depth)
	assert.Equal(t, dlqmonitor.DefaultCriticalAge, got.CriticalAge)
}

// TestDLQMonitorThresholdsFromEnv_EnvOverride mirrors the ALERT_CONFIG_REFRESH_INTERVAL env-tuning
// pattern already used by alerts.ConfigRefreshInterval: a valid override wins, an invalid one falls back
// to the default instead of zeroing out the threshold (which would make everything "critical").
func TestDLQMonitorThresholdsFromEnv_EnvOverride(t *testing.T) {
	t.Run("valid override wins", func(t *testing.T) {
		t.Setenv("PROCESSOR_DLQ_DEPTH_THRESHOLD", "10")
		t.Setenv("PROCESSOR_DLQ_CRITICAL_AGE", "5m")
		got := dlqmonitor.ThresholdsFromEnv()
		assert.Equal(t, uint64(10), got.Depth)
		assert.Equal(t, 5*time.Minute, got.CriticalAge)
	})

	t.Run("unparseable values fall back to defaults", func(t *testing.T) {
		t.Setenv("PROCESSOR_DLQ_DEPTH_THRESHOLD", "not-a-number")
		t.Setenv("PROCESSOR_DLQ_CRITICAL_AGE", "not-a-duration")
		got := dlqmonitor.ThresholdsFromEnv()
		assert.Equal(t, uint64(dlqmonitor.DefaultDepthThreshold), got.Depth)
		assert.Equal(t, dlqmonitor.DefaultCriticalAge, got.CriticalAge)
	})

	t.Run("zero and negative depth fall back to default", func(t *testing.T) {
		t.Setenv("PROCESSOR_DLQ_DEPTH_THRESHOLD", "0")
		got := dlqmonitor.ThresholdsFromEnv()
		assert.Equal(t, uint64(dlqmonitor.DefaultDepthThreshold), got.Depth)
	})
}

// ---------------------------------------------------------------------------
// dlqmonitor.Classify
// ---------------------------------------------------------------------------

// TestDLQMonitorClassify covers the healthy/attention/critical boundary — the central behavior this
// feature exists to get right: a single poison message must land in Attention, not Critical, and each
// of the three independent Critical triggers (depth, staleness, publish failures) must fire on its own.
func TestDLQMonitorClassify(t *testing.T) {
	th := dlqmonitor.Thresholds{Depth: 25, CriticalAge: time.Hour}

	cases := []struct {
		name     string
		detail   dlqmonitor.Detail
		wantSev  dlqmonitor.Severity
		wantText string
	}{
		{
			name:     "no backlog is healthy",
			detail:   dlqmonitor.Detail{Stats: sharedNats.DLQStats{Depth: 0, PublishFailures: 0}},
			wantSev:  dlqmonitor.Healthy,
			wantText: dlqmonitor.StatusHealthy,
		},
		{
			name:     "a single poison message is attention, not critical",
			detail:   dlqmonitor.Detail{Stats: sharedNats.DLQStats{Depth: 1, PublishFailures: 0}},
			wantSev:  dlqmonitor.Attention,
			wantText: dlqmonitor.StatusAttention,
		},
		{
			name:     "depth just below threshold is attention",
			detail:   dlqmonitor.Detail{Stats: sharedNats.DLQStats{Depth: 24, PublishFailures: 0}},
			wantSev:  dlqmonitor.Attention,
			wantText: dlqmonitor.StatusAttention,
		},
		{
			name:     "depth at threshold is critical",
			detail:   dlqmonitor.Detail{Stats: sharedNats.DLQStats{Depth: 25, PublishFailures: 0}},
			wantSev:  dlqmonitor.Critical,
			wantText: dlqmonitor.StatusCritical,
		},
		{
			name: "small but stale backlog is critical on age alone",
			detail: dlqmonitor.Detail{
				Stats:        sharedNats.DLQStats{Depth: 1, PublishFailures: 0},
				HasOldestAge: true,
				OldestAge:    2 * time.Hour,
			},
			wantSev:  dlqmonitor.Critical,
			wantText: dlqmonitor.StatusCritical,
		},
		{
			name: "small fresh backlog stays attention",
			detail: dlqmonitor.Detail{
				Stats:        sharedNats.DLQStats{Depth: 1, PublishFailures: 0},
				HasOldestAge: true,
				OldestAge:    time.Minute,
			},
			wantSev:  dlqmonitor.Attention,
			wantText: dlqmonitor.StatusAttention,
		},
		{
			name:     "publish failures are always critical regardless of depth",
			detail:   dlqmonitor.Detail{Stats: sharedNats.DLQStats{Depth: 0, PublishFailures: 1}},
			wantSev:  dlqmonitor.Critical,
			wantText: dlqmonitor.StatusCritical,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sev, text := dlqmonitor.Classify(tc.detail, th)
			assert.Equal(t, tc.wantSev, sev)
			assert.Equal(t, tc.wantText, text)
		})
	}
}

// ---------------------------------------------------------------------------
// dlqmonitor.GetDetail
// ---------------------------------------------------------------------------

type fakeStatsSource struct {
	stats sharedNats.DLQStats
	err   error
}

func (f *fakeStatsSource) DLQStats(ctx context.Context) (sharedNats.DLQStats, error) {
	return f.stats, f.err
}

type fakeOldestSource struct {
	called bool
	hasAge bool
	age    time.Duration
	class  string
	err    error
}

func (f *fakeOldestSource) OldestMessage(ctx context.Context, stream string) (bool, time.Duration, string, error) {
	f.called = true
	return f.hasAge, f.age, f.class, f.err
}

// TestDLQMonitorGetDetail_SkipsOldestLookupWhenDepthZero asserts the cheap-path guarantee: when there is
// nothing dead-lettered, GetDetail must not call the (potentially network-bound) OldestMessageSource at
// all.
func TestDLQMonitorGetDetail_SkipsOldestLookupWhenDepthZero(t *testing.T) {
	stats := &fakeStatsSource{stats: sharedNats.DLQStats{Stream: "ERROR_EVENTS_DLQ", Depth: 0}}
	oldest := &fakeOldestSource{hasAge: true, age: time.Hour, class: sharedNats.DLQClassPermanent}

	detail, err := dlqmonitor.GetDetail(context.Background(), stats, oldest)
	require.NoError(t, err)
	assert.False(t, oldest.called, "OldestMessage must not be called when depth is 0")
	assert.False(t, detail.HasOldestAge)
}

// TestDLQMonitorGetDetail_PopulatesAgeAndClass asserts a non-empty backlog does enrich Detail from the
// OldestMessageSource.
func TestDLQMonitorGetDetail_PopulatesAgeAndClass(t *testing.T) {
	stats := &fakeStatsSource{stats: sharedNats.DLQStats{Stream: "ERROR_EVENTS_DLQ", Depth: 3}}
	oldest := &fakeOldestSource{hasAge: true, age: 5 * time.Minute, class: sharedNats.DLQClassTransient}

	detail, err := dlqmonitor.GetDetail(context.Background(), stats, oldest)
	require.NoError(t, err)
	assert.True(t, oldest.called)
	assert.True(t, detail.HasOldestAge)
	assert.Equal(t, 5*time.Minute, detail.OldestAge)
	assert.Equal(t, sharedNats.DLQClassTransient, detail.OldestClass)
}

// TestDLQMonitorGetDetail_NilOldestSourceDegradesGracefully asserts a nil OldestMessageSource (the
// detailer connection failed at startup) never causes an error, only a Detail without age/class — this
// is the fallback behavior main.go relies on.
func TestDLQMonitorGetDetail_NilOldestSourceDegradesGracefully(t *testing.T) {
	stats := &fakeStatsSource{stats: sharedNats.DLQStats{Stream: "ERROR_EVENTS_DLQ", Depth: 3}}

	detail, err := dlqmonitor.GetDetail(context.Background(), stats, nil)
	require.NoError(t, err)
	assert.False(t, detail.HasOldestAge)
	assert.Equal(t, "", detail.OldestClass)
	assert.Equal(t, uint64(3), detail.Stats.Depth)
}

// ---------------------------------------------------------------------------
// dlqmonitor.Monitor.CheckOnce — edge-triggered dispatch
// ---------------------------------------------------------------------------

type stubStatsSource struct {
	mu    sync.Mutex
	stats sharedNats.DLQStats
}

func (s *stubStatsSource) set(d sharedNats.DLQStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = d
}

func (s *stubStatsSource) DLQStats(ctx context.Context) (sharedNats.DLQStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats, nil
}

type fakeOperationalDispatcher struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeOperationalDispatcher) DispatchOperational(ctx context.Context, cfg *alerts.AlertConfig, message string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, message)
}

func (f *fakeOperationalDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// TestDLQMonitorCheckOnce_EdgeTriggeredNotEveryPoll is the core requirement from the task: do not alert
// on every check, alert on entering Critical, and alert again on recovery — with no repeat alerts while
// the state persists in between.
func TestDLQMonitorCheckOnce_EdgeTriggeredNotEveryPoll(t *testing.T) {
	stats := &stubStatsSource{stats: sharedNats.DLQStats{Depth: 0}}
	dispatcher := &fakeOperationalDispatcher{}
	monitor := &dlqmonitor.Monitor{
		Stats:       stats,
		Dispatcher:  dispatcher,
		AlertConfig: &alerts.AlertConfig{Channel: "email", ChannelConfig: map[string]interface{}{"to": "oncall@example.test"}},
		Thresholds:  dlqmonitor.Thresholds{Depth: 25, CriticalAge: time.Hour},
	}
	ctx := context.Background()

	// First observation ever: must not dispatch, whatever the state.
	monitor.CheckOnce(ctx)
	assert.Equal(t, 0, dispatcher.count(), "must not alert on the very first observation")

	// A handful of dead-lettered messages (Attention, below threshold): still no alert.
	stats.set(sharedNats.DLQStats{Depth: 3})
	monitor.CheckOnce(ctx)
	assert.Equal(t, 0, dispatcher.count(), "attention-level backlog must not page")

	// Crosses into Critical: exactly one alert.
	stats.set(sharedNats.DLQStats{Depth: 30})
	monitor.CheckOnce(ctx)
	require.Equal(t, 1, dispatcher.count(), "entering critical should dispatch exactly once")
	assert.Contains(t, dispatcher.calls[0], "CRITICAL")

	// Stays Critical (even grows) across repeated checks: no additional alert — this is the
	// "do not alert on every check" requirement.
	stats.set(sharedNats.DLQStats{Depth: 40})
	monitor.CheckOnce(ctx)
	stats.set(sharedNats.DLQStats{Depth: 45})
	monitor.CheckOnce(ctx)
	assert.Equal(t, 1, dispatcher.count(), "must not re-alert while critical persists")

	// Recovers: exactly one recovery alert.
	stats.set(sharedNats.DLQStats{Depth: 0})
	monitor.CheckOnce(ctx)
	require.Equal(t, 2, dispatcher.count(), "recovery should dispatch exactly once")
	assert.Contains(t, dispatcher.calls[1], "recovered")

	// Stays healthy: no additional alert.
	monitor.CheckOnce(ctx)
	assert.Equal(t, 2, dispatcher.count(), "must not re-alert while healthy persists")
}

// TestDLQMonitorCheckOnce_NoConfigLogsInsteadOfDispatching asserts a monitor with no operational
// AlertConfig (PROCESSOR_DLQ_ALERT_CHANNEL unset) never calls the dispatcher — /health remains the only
// signal — and, critically, never panics.
func TestDLQMonitorCheckOnce_NoConfigLogsInsteadOfDispatching(t *testing.T) {
	stats := &stubStatsSource{stats: sharedNats.DLQStats{Depth: 0}}
	dispatcher := &fakeOperationalDispatcher{}
	monitor := &dlqmonitor.Monitor{
		Stats:       stats,
		Dispatcher:  dispatcher,
		AlertConfig: nil,
		Thresholds:  dlqmonitor.Thresholds{Depth: 25, CriticalAge: time.Hour},
	}
	ctx := context.Background()

	monitor.CheckOnce(ctx) // seed
	stats.set(sharedNats.DLQStats{Depth: 100})
	assert.NotPanics(t, func() { monitor.CheckOnce(ctx) })
	assert.Equal(t, 0, dispatcher.count(), "no AlertConfig means no dispatch, only logging")
}

// TestDLQMonitorCheckOnce_NilStatsIsSafe asserts a zero-value Monitor.Stats (misconfiguration) is a
// no-op rather than a nil-pointer panic — CheckOnce runs on a background goroutine where an unrecovered
// panic would take the whole process down.
func TestDLQMonitorCheckOnce_NilStatsIsSafe(t *testing.T) {
	monitor := &dlqmonitor.Monitor{}
	assert.NotPanics(t, func() { monitor.CheckOnce(context.Background()) })
}

// ---------------------------------------------------------------------------
// alerts.Dispatcher.DispatchOperational / alerts.OperationalAlertConfigFromEnv
// ---------------------------------------------------------------------------

// TestAlertsDispatchOperational_NilOrEmptyChannelIsNoop asserts DispatchOperational treats "no
// operational alert channel configured" as a deliberate no-op, not something to log-spam or error on.
func TestAlertsDispatchOperational_NilOrEmptyChannelIsNoop(t *testing.T) {
	d := alerts.NewDispatcherForTest(nil)
	var sent int
	d.SetSenderForTest(func(ctx context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {
		sent++
	})

	d.DispatchOperational(context.Background(), nil, "should not send")
	d.DispatchOperational(context.Background(), &alerts.AlertConfig{Channel: ""}, "should not send")
	assert.Equal(t, 0, sent)
}

// TestAlertsDispatchOperational_SendsThroughExistingSenderPipeline asserts DispatchOperational reuses
// the same sender plumbing as the per-project Dispatch path (SetSender/BuildSender), not a second
// notification mechanism.
func TestAlertsDispatchOperational_SendsThroughExistingSenderPipeline(t *testing.T) {
	d := alerts.NewDispatcherForTest(nil)
	var gotChannel, gotMessage string
	d.SetSenderForTest(func(ctx context.Context, cfg *alerts.AlertConfig, alert *alerts.Alert) {
		gotChannel = cfg.Channel
		gotMessage = alert.Message
	})

	cfg := &alerts.AlertConfig{Channel: "email", ChannelConfig: map[string]interface{}{"to": "ops@example.test"}}
	d.DispatchOperational(context.Background(), cfg, "DLQ backlog critical")

	assert.Equal(t, "email", gotChannel)
	assert.Equal(t, "DLQ backlog critical", gotMessage)
}

// TestOperationalAlertConfigFromEnv_UnsetDisables asserts the default (no PROCESSOR_DLQ_ALERT_CHANNEL)
// is "operational alerting disabled" — /health still reports DLQ state regardless; this only controls
// the push side, and it must not silently invent a destination.
func TestOperationalAlertConfigFromEnv_UnsetDisables(t *testing.T) {
	t.Setenv("PROCESSOR_DLQ_ALERT_CHANNEL", "")
	assert.Nil(t, alerts.OperationalAlertConfigFromEnv())
}

// TestOperationalAlertConfigFromEnv_Email asserts the email destination round-trips into ChannelConfig
// under the same "to" key BuildSender already reads for per-project email alerts.
func TestOperationalAlertConfigFromEnv_Email(t *testing.T) {
	t.Setenv("PROCESSOR_DLQ_ALERT_CHANNEL", "email")
	t.Setenv("PROCESSOR_DLQ_ALERT_TO", "ops@example.test")
	t.Setenv("PROCESSOR_DLQ_ALERT_CHAT_ID", "")

	cfg := alerts.OperationalAlertConfigFromEnv()
	require.NotNil(t, cfg)
	assert.Equal(t, "email", cfg.Channel)
	assert.Equal(t, "ops@example.test", cfg.ChannelConfig["to"])
	assert.True(t, cfg.Enabled)
}

// TestOperationalAlertConfigFromEnv_Telegram asserts the telegram destination round-trips into
// ChannelConfig under the same "chat_id" key BuildSender already reads.
func TestOperationalAlertConfigFromEnv_Telegram(t *testing.T) {
	t.Setenv("PROCESSOR_DLQ_ALERT_CHANNEL", "telegram")
	t.Setenv("PROCESSOR_DLQ_ALERT_TO", "")
	t.Setenv("PROCESSOR_DLQ_ALERT_CHAT_ID", "-100123")

	cfg := alerts.OperationalAlertConfigFromEnv()
	require.NotNil(t, cfg)
	assert.Equal(t, "telegram", cfg.Channel)
	assert.Equal(t, "-100123", cfg.ChannelConfig["chat_id"])
}
