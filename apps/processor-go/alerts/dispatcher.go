package alerts

import (
	"context"
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
	// senderForTest is a non-exported test seam. When set, sendAlert calls
	// this function instead of logging. It is left nil in production; tests
	// can install a fake via the setter exposed in export_test.go.
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
		var channelConfigJSON []byte
		var windowSeconds int

		if err := rows.Scan(&cfg.ProjectID, &cfg.ProjectID, &cfg.Channel, &channelConfigJSON, &cfg.FrequencyThreshold, &windowSeconds, &cfg.Enabled); err != nil {
			continue
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
	if d.senderForTest != nil {
		d.senderForTest(ctx, cfg, alert)
		return
	}
	log.Printf("ALERT: %s via %s - %s", alert.IssueID, cfg.Channel, alert.Message)
}

// SetSenderForTest replaces the default sendAlert implementation with a
// caller-supplied function. It is intended for tests that need to observe
// the alerts the dispatcher produces. When s is nil, the default logging
// behavior is restored. Production code should not call this method.
func (d *Dispatcher) SetSenderForTest(s func(ctx context.Context, cfg *AlertConfig, alert *Alert)) {
	d.senderForTest = s
}

func formatAlertMessage(errorClass, message string, count int) string {
	if len(message) > 100 {
		message = message[:100] + "..."
	}
	return fmt.Sprintf("[%dx] %s: %s", count, errorClass, message)
}
