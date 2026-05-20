package notifiers

import (
	"fmt"
	"log"
	"net/smtp"
	"time"
)

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
	var lastErr error

	for attempt := 0; attempt < w.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := w.backoffs[attempt-1]
			log.Printf("Email retry %d/%d after %v", attempt+1, w.maxRetries, backoff)
			time.Sleep(backoff)
		}

		err := w.sendEmail(notification)
		if err == nil {
			log.Printf("Email sent successfully to %s", notification.ToAddress)
			return
		}

		lastErr = err
		log.Printf("Email attempt %d failed: %v", attempt+1, err)
	}

	log.Printf("Email failed after %d attempts: %v", w.maxRetries, lastErr)
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
