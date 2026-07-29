package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type AlertConfig struct {
	ProjectID          string
	Channel            string
	ChannelConfig      map[string]interface{}
	FrequencyThreshold int
	FrequencyWindow    time.Duration
	Enabled            bool
}

type Alert struct {
	IssueID    string
	ProjectID  string
	Channel    string
	Message    string
	OccurredAt time.Time
}

type Dispatcher struct {
	db       *pgxpool.Pool
	counters map[string]*alertCounter
	mu       sync.RWMutex
	configs  map[string]*AlertConfig
	configMu sync.RWMutex
	// senderForTest is the dispatcher's outbound send hook. Despite the
	// name (kept for compatibility with existing callers, including
	// SetSenderForTest below), it is no longer test-only: NewProcessorService
	// wires it to a real sender (see alerts.BuildSender) via SetSender, which
	// is the production, non-test entry point. It is left nil until a setter
	// is called, in which case sendAlert falls back to log.Printf — this is
	// what made alerting a no-op in production before S8 was fixed (see
	// docs/plans/E2E_RECOVERY_PLAN.md P5-1 / VERIFIED_STATE.md S8).
	senderMu      sync.RWMutex
	senderForTest func(ctx context.Context, cfg *AlertConfig, alert *Alert)
}

type alertCounter struct {
	count       int
	windowStart time.Time
	window      time.Duration
	threshold   int
}

func NewDispatcher(db *pgxpool.Pool) *Dispatcher {
	d := &Dispatcher{
		db:       db,
		counters: make(map[string]*alertCounter),
		configs:  make(map[string]*AlertConfig),
	}

	// loadConfigs only refreshes on a 5-minute ticker with no initial load
	// (VERIFIED_STATE.md S8 item 4) — without this, a Dispatcher built right
	// before an event arrives would ignore every alert config for the first
	// five minutes after boot, which defeats the "within one event" delivery
	// this dispatcher exists for. Load synchronously, once, here, bounded by
	// a short timeout so a slow/unreachable DB at startup does not hang
	// process startup indefinitely — a failed initial load just means the
	// ticker's next refresh (or a later RefreshConfigsForTest-equivalent
	// call) picks it up instead.
	initCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	d.refreshConfigs(initCtx)
	cancel()

	go d.loadConfigs(context.Background())
	return d
}

// NewDispatcherForTest builds a Dispatcher without spawning the loadConfigs
// background goroutine. It is intended for tests that populate the configs
// map directly via SetConfigsForTest and therefore do not need the periodic
// refresh ticker. The returned Dispatcher otherwise behaves like one built
// by NewDispatcher.
func NewDispatcherForTest(db *pgxpool.Pool) *Dispatcher {
	return &Dispatcher{
		db:       db,
		counters: make(map[string]*alertCounter),
		configs:  make(map[string]*AlertConfig),
	}
}

// SetConfigsForTest replaces the dispatcher's loaded configurations. It is
// intended for tests that populate the configs map directly without going
// through the loadConfigs background goroutine.
func (d *Dispatcher) SetConfigsForTest(configs map[string]*AlertConfig) {
	d.configMu.Lock()
	defer d.configMu.Unlock()
	d.configs = configs
}

// RefreshConfigsForTest exposes the package-private refreshConfigs query so
// tests can drive the loader synchronously without relying on the periodic
// ticker started by NewDispatcher.
func (d *Dispatcher) RefreshConfigsForTest(ctx context.Context) {
	d.refreshConfigs(ctx)
}

// LoadConfigsForTest exposes the package-private loadConfigs loop so tests
// can drive the ticker behavior synchronously with a caller-supplied
// context. The loop returns when the context is cancelled.
func (d *Dispatcher) LoadConfigsForTest(ctx context.Context) {
	d.loadConfigs(ctx)
}

func (d *Dispatcher) loadConfigs(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshConfigs(ctx)
		}
	}
}

func (d *Dispatcher) refreshConfigs(ctx context.Context) {
	rows, err := d.db.Query(ctx,
		"SELECT id, project_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled FROM alert_configs WHERE enabled = true",
	)
	if err != nil {
		log.Printf("Failed to load alert configs: %v", err)
		return
	}
	defer rows.Close()

	d.configMu.Lock()
	defer d.configMu.Unlock()

	d.configs = make(map[string]*AlertConfig)
	for rows.Next() {
		var cfg AlertConfig
		var configID string
		var channelConfigJSON []byte
		var windowSeconds int

		// configID is a real destination, not a throwaway: passing &cfg.ProjectID for BOTH `id` and
		// `project_id` (as this did) only happened to work because the second write overwrote the
		// first — any reorder of the SELECT list would silently corrupt the tenant routing key.
		if err := rows.Scan(&configID, &cfg.ProjectID, &cfg.Channel, &channelConfigJSON, &cfg.FrequencyThreshold, &windowSeconds, &cfg.Enabled); err != nil {
			log.Printf("alerts: skipping unreadable alert_configs row: %v", err)
			continue
		}

		// channel_config used to be scanned and then discarded, leaving ChannelConfig nil for every
		// config. Both permitted channels read their destination out of it (email's "to", telegram's
		// "chat_id"), so every alert was dropped at the notifier with a "missing" message — alerting
		// could not deliver for ANY configuration, however correct the row.
		if len(channelConfigJSON) > 0 {
			if err := json.Unmarshal(channelConfigJSON, &cfg.ChannelConfig); err != nil {
				// The dashboard has written this column double-encoded (a JSON *string* containing
				// JSON) — see P6. Unwrap one level rather than dropping the config on the floor.
				var nested string
				if json.Unmarshal(channelConfigJSON, &nested) == nil {
					if err2 := json.Unmarshal([]byte(nested), &cfg.ChannelConfig); err2 != nil {
						log.Printf("alerts: project=%s channel_config is not a JSON object (%v); alerts for this project will drop", cfg.ProjectID, err2)
						continue
					}
				} else {
					log.Printf("alerts: project=%s channel_config unreadable (%v); alerts for this project will drop", cfg.ProjectID, err)
					continue
				}
			}
		}

		cfg.FrequencyWindow = time.Duration(windowSeconds) * time.Second
		d.configs[cfg.ProjectID] = &cfg
	}
}

func (d *Dispatcher) Dispatch(ctx context.Context, issueID, projectID, errorClass, message string) {
	d.configMu.RLock()
	cfg, exists := d.configs[projectID]
	d.configMu.RUnlock()

	if !exists || !cfg.Enabled {
		return
	}

	key := projectID + ":" + issueID

	d.mu.Lock()
	counter, exists := d.counters[key]
	if !exists {
		counter = &alertCounter{
			window:    cfg.FrequencyWindow,
			threshold: cfg.FrequencyThreshold,
		}
		d.counters[key] = counter
	}

	now := time.Now()
	if now.Sub(counter.windowStart) >= counter.window {
		counter.count = 0
		counter.windowStart = now
	}

	counter.count++
	count := counter.count
	d.mu.Unlock()

	if count >= counter.threshold {
		alert := &Alert{
			IssueID:    issueID,
			ProjectID:  projectID,
			Channel:    cfg.Channel,
			Message:    formatAlertMessage(errorClass, message, count),
			OccurredAt: now,
		}

		d.sendAlert(ctx, cfg, alert)

		d.mu.Lock()
		delete(d.counters, key)
		d.mu.Unlock()
	}
}

func (d *Dispatcher) sendAlert(ctx context.Context, cfg *AlertConfig, alert *Alert) {
	d.senderMu.RLock()
	sender := d.senderForTest
	d.senderMu.RUnlock()

	if sender != nil {
		sender(ctx, cfg, alert)
		return
	}
	log.Printf("ALERT: %s via %s - %s", alert.IssueID, cfg.Channel, alert.Message)
}

// SetSender wires the dispatcher's outbound alert channel to a real sender.
// This is the production entry point: service.NewProcessorService calls it
// with alerts.BuildSender(...) so sendAlert actually reaches the email/
// telegram notifiers instead of only logging "ALERT: ..." (VERIFIED_STATE.md
// S8, docs/plans/E2E_RECOVERY_PLAN.md P5-1 item 3). When s is nil, the
// default logging behavior is restored.
func (d *Dispatcher) SetSender(s func(ctx context.Context, cfg *AlertConfig, alert *Alert)) {
	d.senderMu.Lock()
	defer d.senderMu.Unlock()
	d.senderForTest = s
}

// SetSenderForTest is retained for existing tests that install a capturing
// sender; it is now a thin alias for SetSender, which is the method
// production code (and any new caller) should use.
func (d *Dispatcher) SetSenderForTest(s func(ctx context.Context, cfg *AlertConfig, alert *Alert)) {
	d.SetSender(s)
}

func formatAlertMessage(errorClass, message string, count int) string {
	if len(message) > 100 {
		message = message[:100] + "..."
	}
	return fmt.Sprintf("[%dx] %s: %s", count, errorClass, message)
}
