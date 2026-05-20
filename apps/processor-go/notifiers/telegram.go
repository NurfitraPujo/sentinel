package notifiers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

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
		config: cfg,
		client: &http.Client{Timeout: 10 * time.Second},
		queue:  make(chan *TelegramNotification, 1000),
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
	var lastErr error

	for attempt := 0; attempt < w.maxRetries; attempt++ {
		if attempt > 0 {
			backoff := w.backoffs[attempt-1]
			log.Printf("Telegram retry %d/%d after %v", attempt+1, w.maxRetries, backoff)
			time.Sleep(backoff)
		}

		err := w.sendTelegram(notification)
		if err == nil {
			log.Printf("Telegram message sent successfully")
			return
		}

		lastErr = err
		log.Printf("Telegram attempt %d failed: %v", attempt+1, err)
	}

	log.Printf("Telegram failed after %d attempts: %v", w.maxRetries, lastErr)
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
