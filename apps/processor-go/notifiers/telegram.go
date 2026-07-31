package notifiers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/procmetrics"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// channelTelegram is the LabelChannel value this worker records on procmetrics.RecordAlertDispatch —
// the same literal "telegram" alerts.AlertConfig.Channel and notify.go's BuildSender switch on.
const channelTelegram = "telegram"

type TelegramConfig struct {
	BotToken   string
	ChatID     string
	APIBaseURL string
}

type TelegramNotification struct {
	Message string
}

type TelegramWorker struct {
	config     TelegramConfig
	client     *http.Client
	queue      chan *TelegramNotification
	maxRetries int
	backoffs   []time.Duration
}

func NewTelegramWorker(cfg TelegramConfig) *TelegramWorker {
	w := &TelegramWorker{
		config:     cfg,
		client:     &http.Client{Timeout: 10 * time.Second},
		queue:      make(chan *TelegramNotification, 1000),
		maxRetries: 3,
		backoffs:   []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second},
	}
	go w.processQueue()
	return w
}

func (w *TelegramWorker) Send(notification *TelegramNotification) error {
	select {
	case w.queue <- notification:
		return nil
	default:
		return fmt.Errorf("telegram queue is full")
	}
}

func (w *TelegramWorker) processQueue() {
	for notification := range w.queue {
		w.sendWithRetry(notification)
	}
}

func (w *TelegramWorker) sendWithRetry(notification *TelegramNotification) {
	// No request context survives the enqueue — see EmailWorker.sendWithRetry's identical comment;
	// the queue is the decoupling point for both notifiers.
	ctx := context.Background()
	var lastErr error

	for attempt := 0; attempt < w.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := w.backoffs[attempt-1]
			slog.InfoContext(ctx, "Telegram retry after backoff",
				slog.Int("attempt", attempt+1), slog.Int("max_retries", w.maxRetries), slog.Duration("backoff", backoff))
			time.Sleep(backoff)
		}

		err := w.sendTelegram(notification)
		if err == nil {
			slog.InfoContext(ctx, "Telegram message sent successfully",
				slog.String(obs.LogKeyEvent, "alert.telegram.sent"))
			procmetrics.RecordAlertDispatch(ctx, channelTelegram, obs.OutcomeDispatchSent)
			return
		}

		lastErr = err
		slog.WarnContext(ctx, "Telegram attempt failed",
			slog.Int("attempt", attempt+1), slog.String("error", err.Error()))
	}

	slog.ErrorContext(ctx, "Telegram failed after max attempts",
		slog.Int("max_retries", w.maxRetries), slog.String("error", lastErr.Error()),
		slog.String(obs.LogKeyEvent, "alert.telegram.failed"))
	procmetrics.RecordAlertDispatch(ctx, channelTelegram, obs.OutcomeDispatchError)
}

func (w *TelegramWorker) sendTelegram(notification *TelegramNotification) error {
	apiURL := fmt.Sprintf("%s/bot%s/sendMessage", w.config.APIBaseURL, w.config.BotToken)

	payload := map[string]interface{}{
		"chat_id":    w.config.ChatID,
		"text":       notification.Message,
		"parse_mode": "HTML",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned status %d", resp.StatusCode)
	}

	return nil
}

func (w *TelegramWorker) Close() {
	close(w.queue)
}

// QueueLen returns the current number of buffered notifications awaiting
// delivery. Intended for tests and operational diagnostics; not part of the
// public notifier contract.
func (w *TelegramWorker) QueueLen() int {
	return len(w.queue)
}
