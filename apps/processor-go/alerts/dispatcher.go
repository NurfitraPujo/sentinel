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
	log.Printf("ALERT: %s via %s - %s", alert.IssueID, cfg.Channel, alert.Message)
}

func formatAlertMessage(errorClass, message string, count int) string {
	if len(message) > 100 {
		message = message[:100] + "..."
	}
	return fmt.Sprintf("[%dx] %s: %s", count, errorClass, message)
}
