// Package webhooks implements N3b: the outbound webhook delivery dispatcher. It polls
// agent_webhooks on a ticker (mirroring apps/processor-go/dlqmonitor's Run/checkOnceRecoverable
// pattern) and, for each active webhook, fetches any issue_activity events past its cursor and
// POSTs them, HMAC-signed, to the subscriber's URL. See store/webhooks.go's package doc for the
// compare-and-swap cursor design and why this is optimistic rather than FOR UPDATE SKIP LOCKED.
package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// DefaultDispatchInterval is how often the dispatcher scans for active webhooks with due events,
// used when WEBHOOK_DISPATCH_INTERVAL is unset or invalid.
const DefaultDispatchInterval = 5 * time.Second

// DefaultFailureThreshold is the consecutive-failure count at which a webhook is auto-disabled
// (status set to 'failed'), used when WEBHOOK_FAILURE_THRESHOLD is unset or invalid.
const DefaultFailureThreshold = 20

// eventsPerTick caps how many events a single delivery POSTs, matching the task spec's <=100.
const eventsPerTick = 100

// DispatchIntervalFromEnv reads WEBHOOK_DISPATCH_INTERVAL (a Go duration string, e.g. "5s") or
// falls back to DefaultDispatchInterval, mirroring dlqmonitor.CheckIntervalFromEnv's parsing
// pattern.
func DispatchIntervalFromEnv() time.Duration {
	if v := os.Getenv("WEBHOOK_DISPATCH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		slog.Warn("webhooks: ignoring invalid WEBHOOK_DISPATCH_INTERVAL; using default",
			slog.String("value", v), slog.Duration("default", DefaultDispatchInterval))
	}
	return DefaultDispatchInterval
}

// FailureThresholdFromEnv reads WEBHOOK_FAILURE_THRESHOLD or falls back to DefaultFailureThreshold.
func FailureThresholdFromEnv() int {
	if v := os.Getenv("WEBHOOK_FAILURE_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		slog.Warn("webhooks: ignoring invalid WEBHOOK_FAILURE_THRESHOLD; using default",
			slog.String("value", v), slog.Int("default", DefaultFailureThreshold))
	}
	return DefaultFailureThreshold
}

// EnabledFromEnv reports whether WEBHOOK_DISPATCH_ENABLED is exactly "true". Off by default —
// main.go only starts the dispatcher goroutine when this is true.
func EnabledFromEnv() bool {
	return os.Getenv("WEBHOOK_DISPATCH_ENABLED") == "true"
}

// eventPayload and issuePayload mirror OrgActivityEvent / its nested issue object from
// src/lib/db/queries/events.ts EXACTLY (field names and nesting) — GET /api/agent/events (poll) and
// this dispatcher (push) must never present two different shapes for the same underlying event.
type eventPayload struct {
	Seq       int64           `json:"seq"`
	EventType string          `json:"eventType"`
	ActorType string          `json:"actorType"`
	ActorID   string          `json:"actorId"`
	OldValue  json.RawMessage `json:"oldValue"`
	NewValue  json.RawMessage `json:"newValue"`
	CreatedAt time.Time       `json:"createdAt"`
	Issue     issuePayload    `json:"issue"`
}

type issuePayload struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	IssueType string `json:"issueType"`
	ProjectID string `json:"projectId"`
}

// deliveryBody is the top-level webhook POST body.
type deliveryBody struct {
	WebhookID string         `json:"webhookId"`
	AgentID   string         `json:"agentId"`
	Events    []eventPayload `json:"events"`
	Cursor    int64          `json:"cursor"`
}

// Dispatcher polls store for active webhooks and delivers due events to each. Store is declared as
// store.WebhookStore (an interface) so tests can substitute a fake without a live database — see
// dispatcher_test.go.
type Dispatcher struct {
	Store            store.WebhookStore
	Client           *http.Client
	Interval         time.Duration
	FailureThreshold int
	MaxRetries       int
	Backoffs         []time.Duration
}

// NewDispatcher returns a Dispatcher with the same client timeout and retry/backoff shape as
// notifiers/telegram.go's TelegramWorker (10s client timeout, 3 attempts, 1s/5s/30s backoff).
func NewDispatcher(st store.WebhookStore) *Dispatcher {
	return &Dispatcher{
		Store:            st,
		Client:           &http.Client{Timeout: 10 * time.Second},
		Interval:         DispatchIntervalFromEnv(),
		FailureThreshold: FailureThresholdFromEnv(),
		MaxRetries:       3,
		Backoffs:         []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second},
	}
}

// Run blocks, ticking on d.Interval (DefaultDispatchInterval if unset) until ctx is done. Intended
// to be started with `go dispatcher.Run(ctx)`, mirroring dlqmonitor.Monitor.Run.
func (d *Dispatcher) Run(ctx context.Context) {
	interval := d.Interval
	if interval <= 0 {
		interval = DefaultDispatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.tickRecoverable(ctx)
		}
	}
}

// tickRecoverable wraps Tick with a panic recovery, for the same reason dlqmonitor's
// checkOnceRecoverable does: this runs on its own goroutine outside any request path, and an
// unrecovered panic here would take down the entire process. A delivery failure — network error,
// bad webhook config, anything — must never crash or wedge the processor.
func (d *Dispatcher) tickRecoverable(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "webhooks: recovered from panic during dispatch tick", slog.Any("panic", r))
		}
	}()
	d.Tick(ctx)
}

// Tick lists every active webhook and attempts one delivery cycle for each. Exported so tests can
// drive it synchronously without waiting on Run's ticker.
func (d *Dispatcher) Tick(ctx context.Context) {
	webhooks, err := d.Store.ListActiveWebhooks(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "webhooks: failed to list active webhooks", slog.String("error", err.Error()))
		return
	}

	for _, wh := range webhooks {
		d.deliverOne(ctx, wh)
	}
}

func (d *Dispatcher) deliverOne(ctx context.Context, wh store.WebhookRow) {
	events, err := d.Store.FetchEventsForWebhook(ctx, wh.OrganizationID, wh.LastDeliveredSeq, wh.EventTypes, eventsPerTick)
	if err != nil {
		slog.ErrorContext(ctx, "webhooks: failed to fetch events for webhook",
			slog.String("webhook_id", wh.ID), slog.String("error", err.Error()))
		return
	}
	if len(events) == 0 {
		return
	}

	newSeq := events[len(events)-1].Seq
	payload := buildPayload(wh, events, newSeq)

	body, err := json.Marshal(payload)
	if err != nil {
		slog.ErrorContext(ctx, "webhooks: failed to marshal delivery payload",
			slog.String("webhook_id", wh.ID), slog.String("error", err.Error()))
		return
	}

	if err := d.sendWithRetry(ctx, wh, body); err != nil {
		slog.WarnContext(ctx, "webhooks: delivery failed after max attempts",
			slog.String("webhook_id", wh.ID), slog.String("error", err.Error()),
			slog.String(obs.LogKeyEvent, "webhook.delivery.failed"))
		if recErr := d.Store.RecordDeliveryFailure(ctx, wh.ID, err.Error(), d.FailureThreshold); recErr != nil {
			slog.ErrorContext(ctx, "webhooks: failed to record delivery failure",
				slog.String("webhook_id", wh.ID), slog.String("error", recErr.Error()))
		}
		return
	}

	advanced, err := d.Store.RecordDeliverySuccess(ctx, wh.ID, wh.LastDeliveredSeq, newSeq)
	if err != nil {
		slog.ErrorContext(ctx, "webhooks: failed to record delivery success",
			slog.String("webhook_id", wh.ID), slog.String("error", err.Error()))
		return
	}
	if !advanced {
		// CAS lost the race to another processor instance — see store/webhooks.go's package doc.
		// Not an error: the events this instance just delivered were (at least) also delivered by
		// whichever instance won the CAS, so the cursor is already at or past newSeq.
		slog.InfoContext(ctx, "webhooks: cursor CAS did not advance; another instance already delivered",
			slog.String("webhook_id", wh.ID))
		return
	}
	slog.InfoContext(ctx, "webhooks: delivered events",
		slog.String("webhook_id", wh.ID), slog.Int("event_count", len(events)), slog.Int64("cursor", newSeq),
		slog.String(obs.LogKeyEvent, "webhook.delivery.sent"))
}

func buildPayload(wh store.WebhookRow, events []store.WebhookEvent, cursor int64) deliveryBody {
	out := make([]eventPayload, 0, len(events))
	for _, e := range events {
		out = append(out, eventPayload{
			Seq:       e.Seq,
			EventType: e.EventType,
			ActorType: e.ActorType,
			ActorID:   e.ActorID,
			OldValue:  e.OldValue,
			NewValue:  e.NewValue,
			CreatedAt: e.CreatedAt,
			Issue: issuePayload{
				ID:        e.IssueID,
				Title:     e.IssueTitle,
				Status:    e.IssueStatus,
				IssueType: e.IssueType,
				ProjectID: e.ProjectID,
			},
		})
	}
	return deliveryBody{
		WebhookID: wh.ID,
		AgentID:   wh.AgentID,
		Events:    out,
		Cursor:    cursor,
	}
}

// sendWithRetry POSTs body to wh.URL up to d.MaxRetries times with d.Backoffs between attempts,
// mirroring notifiers/telegram.go's sendWithRetry shape. Returns nil the first time the receiver
// answers 2xx; otherwise the last error observed.
func (d *Dispatcher) sendWithRetry(ctx context.Context, wh store.WebhookRow, body []byte) error {
	maxRetries := d.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}
	backoffs := d.Backoffs
	if len(backoffs) == 0 {
		backoffs = []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second}
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			idx := attempt - 1
			if idx >= len(backoffs) {
				idx = len(backoffs) - 1
			}
			backoff := backoffs[idx]
			slog.InfoContext(ctx, "webhooks: retrying delivery after backoff",
				slog.String("webhook_id", wh.ID), slog.Int("attempt", attempt+1),
				slog.Int("max_retries", maxRetries), slog.Duration("backoff", backoff))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		err := d.send(ctx, wh, body)
		if err == nil {
			return nil
		}
		lastErr = err
		slog.WarnContext(ctx, "webhooks: delivery attempt failed",
			slog.String("webhook_id", wh.ID), slog.Int("attempt", attempt+1), slog.String("error", err.Error()))
	}
	return lastErr
}

func (d *Dispatcher) send(ctx context.Context, wh store.WebhookRow, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, wh.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	ts := time.Now().Unix()
	sig := Sign(wh.Secret, ts, body)
	req.Header.Set("X-Sentinel-Signature", sig)
	req.Header.Set("X-Sentinel-Delivery-Id", uuid.NewString())

	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook receiver returned status %d", resp.StatusCode)
	}
	return nil
}

// Sign returns the X-Sentinel-Signature header value: "t=<unix>,v1=<hex hmac-sha256(secret,
// "<t>.<body>")>". Exported so tests (and, if ever needed, a CLI verification helper) can compute
// the same value the dispatcher sends.
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	sum := mac.Sum(nil)
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(sum))
}
