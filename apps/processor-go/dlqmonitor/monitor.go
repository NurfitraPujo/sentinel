package dlqmonitor

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/alerts"
)

// DefaultCheckInterval is how often Monitor polls DLQ stats when Interval is unset. One minute is
// frequent enough to catch a backlog well within the CriticalAge window (an hour, by default) without
// adding meaningful load to JetStream — each check is at most a StreamInfo plus a GetMsg, both O(1).
const DefaultCheckInterval = time.Minute

// CheckIntervalFromEnv reads PROCESSOR_DLQ_ALERT_INTERVAL (a Go duration string) or falls back to
// DefaultCheckInterval, mirroring ThresholdsFromEnv's parsing pattern.
func CheckIntervalFromEnv() time.Duration {
	if v := os.Getenv("PROCESSOR_DLQ_ALERT_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("dlqmonitor: ignoring invalid PROCESSOR_DLQ_ALERT_INTERVAL=%q; using default %s", v, DefaultCheckInterval)
	}
	return DefaultCheckInterval
}

// OperationalDispatcher is the subset of *alerts.Dispatcher the monitor needs. Declaring it as an
// interface keeps Monitor unit testable with a fake that records calls instead of a live
// email/telegram sender.
type OperationalDispatcher interface {
	DispatchOperational(ctx context.Context, cfg *alerts.AlertConfig, message string)
}

// Monitor periodically classifies the DLQ backlog and dispatches an operational alert on a transition
// into or out of Critical severity — never on Attention, and never repeatedly while a condition
// persists. See CheckOnce for the exact edge-trigger rule and why.
type Monitor struct {
	Stats       StatsSource           // required
	Oldest      OldestMessageSource   // optional; nil disables age/class enrichment
	Dispatcher  OperationalDispatcher // optional; nil disables dispatch (still logs)
	AlertConfig *alerts.AlertConfig   // optional; nil disables dispatch (still logs) — see alerts.OperationalAlertConfigFromEnv
	Thresholds  Thresholds
	Interval    time.Duration

	wasCritical bool
	initialized bool
}

// Run blocks, checking on Interval (DefaultCheckInterval if unset) until ctx is done. Intended to be
// started with `go monitor.Run(ctx)`.
func (m *Monitor) Run(ctx context.Context) {
	interval := m.Interval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkOnceRecoverable(ctx)
		}
	}
}

// checkOnceRecoverable wraps CheckOnce with a panic recovery. This runs on its own goroutine outside
// any request path: an unrecovered panic here would take down the entire process (Go does not isolate
// goroutine panics), which would be strictly worse than the DLQ backlog this monitor exists to report
// on. A dispatch failure — or any other failure in this loop — must never crash or wedge the processor.
func (m *Monitor) checkOnceRecoverable(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("dlqmonitor: recovered from panic during check: %v", r)
		}
	}()
	m.CheckOnce(ctx)
}

// CheckOnce fetches the current DLQ detail, classifies it, and dispatches exactly when severity crosses
// the Critical boundary in either direction since the last check:
//
//   - entering Critical (from Healthy or Attention): dispatches a "backlog critical" alert.
//   - leaving Critical (to Healthy or Attention): dispatches a "backlog recovered" alert.
//   - any other transition, including Healthy<->Attention, or no transition at all: does nothing.
//
// This is edge-triggered, not rate-limited on a timer: a persistent backlog produces exactly one alert
// on the way in and exactly one on the way out, however many checks run while it persists. Re-notifying
// every poll interval is exactly how alerting gets muted, and Attention (a handful of dead-lettered
// messages — normal operation, see package doc) never pages at all, only /health surfaces it.
//
// The very first observation after Monitor is constructed never dispatches, regardless of severity: it
// seeds wasCritical from whatever state the DLQ happens to be in at that moment rather than treating
// process startup itself as a transition.
//
// Exported so tests can drive it synchronously without waiting on Run's ticker.
func (m *Monitor) CheckOnce(ctx context.Context) {
	if m.Stats == nil {
		return
	}

	detail, err := GetDetail(ctx, m.Stats, m.Oldest)
	if err != nil {
		log.Printf("dlqmonitor: failed to read DLQ stats: %v", err)
		return
	}

	severity, statusMsg := Classify(detail, m.Thresholds)
	nowCritical := severity == Critical

	if !m.initialized {
		m.initialized = true
		m.wasCritical = nowCritical
		return
	}

	if nowCritical == m.wasCritical {
		return
	}
	m.wasCritical = nowCritical

	text := alertText(detail, m.Thresholds, nowCritical, statusMsg)

	if m.Dispatcher == nil || m.AlertConfig == nil {
		log.Printf("dlqmonitor: %s (no PROCESSOR_DLQ_ALERT_CHANNEL configured; see /health)", text)
		return
	}
	m.Dispatcher.DispatchOperational(ctx, m.AlertConfig, text)
}

func alertText(d Detail, th Thresholds, nowCritical bool, statusMsg string) string {
	// "unavailable" here means the detailer never fetched this — a different state from the class
	// vocabulary's "unclassified", which means it DID fetch and the message carries no class header.
	// Using one word for both would make an operator reading this alert unable to tell "we could not
	// look" from "we looked and there is nothing to see".
	age := "unavailable"
	if d.HasOldestAge {
		age = d.OldestAge.Round(time.Second).String()
	}
	class := d.OldestClass
	if class == "" {
		class = "unavailable"
	}

	if nowCritical {
		return fmt.Sprintf(
			"DLQ backlog CRITICAL: depth=%d (threshold=%d) publish_failures=%d oldest_age=%s oldest_class=%s — %s",
			d.Stats.Depth, th.Depth, d.Stats.PublishFailures, age, class, statusMsg,
		)
	}
	return fmt.Sprintf(
		"DLQ backlog recovered: depth=%d publish_failures=%d oldest_age=%s — no longer above threshold=%d/critical_age=%s",
		d.Stats.Depth, d.Stats.PublishFailures, age, th.Depth, th.CriticalAge,
	)
}
