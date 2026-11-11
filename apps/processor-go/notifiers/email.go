package notifiers

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/procmetrics"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// channelEmail is the LabelChannel value this worker records on procmetrics.RecordAlertDispatch —
// the same literal "email" alerts.AlertConfig.Channel and notify.go's BuildSender switch on.
const channelEmail = "email"

type EmailConfig struct {
	SMTPHost    string
	SMTPPort    int
	Username    string
	Password    string
	FromAddress string
}

type EmailNotification struct {
	ToAddress string
	Subject   string
	Body      string
}

type EmailWorker struct {
	config     EmailConfig
	queue      chan *EmailNotification
	retries    int
	maxRetries int
	backoffs   []time.Duration
}

func NewEmailWorker(cfg EmailConfig) *EmailWorker {
	w := &EmailWorker{
		config:     cfg,
		queue:      make(chan *EmailNotification, 1000),
		maxRetries: 3,
		backoffs:   []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second},
	}
	go w.processQueue()
	return w
}

func (w *EmailWorker) Send(notification *EmailNotification) error {
	select {
	case w.queue <- notification:
		return nil
	default:
		return fmt.Errorf("email queue is full")
	}
}

func (w *EmailWorker) processQueue() {
	for notification := range w.queue {
		w.sendWithRetry(notification)
	}
}

func (w *EmailWorker) sendWithRetry(notification *EmailNotification) {
	// No request context survives the enqueue (the queue itself is the decoupling point — see
	// EmailWorker's doc comment on NewEmailWorker), so these log with context.Background() rather than
	// a per-request ctx: there is no span in scope here to correlate against, which is not an error,
	// it is simply "no trace in scope right now" (see packages/shared-go/obs.Handler's doc comment).
	ctx := context.Background()
	var lastErr error

	for attempt := 0; attempt < w.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := w.backoffs[attempt-1]
			// Message text unchanged (OBSERVABILITY_PLAN.md §4): structure added via attrs only.
			slog.InfoContext(ctx, fmt.Sprintf("Email retry %d/%d after %v", attempt+1, w.maxRetries, backoff),
				slog.Int("attempt", attempt+1), slog.Int("max_retries", w.maxRetries), slog.Duration("backoff", backoff))
			time.Sleep(backoff)
		}

		err := w.sendEmail(notification)
		if err == nil {
			// tests/e2e/alerting_test.go (U26/U27) greps this exact substring — do not reword.
			slog.InfoContext(ctx, fmt.Sprintf("Email sent successfully to %s", notification.ToAddress),
				slog.String(obs.LogKeyEvent, "alert.email.sent"), slog.String("to", notification.ToAddress))
			procmetrics.RecordAlertDispatch(ctx, channelEmail, obs.OutcomeDispatchSent)
			return
		}

		lastErr = err
		// tests/e2e/alerting_test.go (U26/U27) greps this exact substring — do not reword.
		slog.WarnContext(ctx, fmt.Sprintf("Email attempt %d failed: %v", attempt+1, err),
			slog.Int("attempt", attempt+1), slog.String("error", err.Error()))
	}

	// tests/e2e/alerting_test.go (U26/U27) greps this exact substring — do not reword.
	slog.ErrorContext(ctx, fmt.Sprintf("Email failed after %d attempts: %v", w.maxRetries, lastErr),
		slog.Int("max_retries", w.maxRetries), slog.String("error", lastErr.Error()),
		slog.String(obs.LogKeyEvent, "alert.email.failed"))
	procmetrics.RecordAlertDispatch(ctx, channelEmail, obs.OutcomeDispatchError)
}

func (w *EmailWorker) sendEmail(notification *EmailNotification) error {
	addr := fmt.Sprintf("%s:%d", w.config.SMTPHost, w.config.SMTPPort)

	auth := smtp.PlainAuth("", w.config.Username, w.config.Password, w.config.SMTPHost)

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s",
		w.config.FromAddress,
		notification.ToAddress,
		notification.Subject,
		notification.Body,
	)

	err := smtp.SendMail(addr, auth, w.config.FromAddress, []string{notification.ToAddress}, []byte(msg))
	return err
}

func (w *EmailWorker) Close() {
	close(w.queue)
}
