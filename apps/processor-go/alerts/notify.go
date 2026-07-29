package alerts

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/notifiers"
)

// NotifierConfig holds the operational, environment-provided settings for
// each outbound alert channel. Per-alert routing (which email address, which
// chat) comes from AlertConfig.ChannelConfig, populated from the
// alert_configs table a project owner controls; channel-wide credentials
// (SMTP auth, the bot token) come from here instead, because they are
// deployment secrets, not something a project should be able to set for
// itself through the alert-config API.
type NotifierConfig struct {
	Email    notifiers.EmailConfig
	Telegram notifiers.TelegramConfig
}

// NotifierConfigFromEnv builds a NotifierConfig from the process
// environment. Every field has a safe zero-value fallback: if SMTP or
// Telegram credentials are not configured, BuildSender's returned function
// logs and drops the alert for that channel rather than failing — alerting
// must never cost the event that triggered it (see BuildSender).
func NotifierConfigFromEnv() NotifierConfig {
	return NotifierConfig{
		Email: notifiers.EmailConfig{
			SMTPHost:    getEnv("ALERT_SMTP_HOST", "localhost"),
			SMTPPort:    getEnvInt("ALERT_SMTP_PORT", 587),
			Username:    getEnv("ALERT_SMTP_USERNAME", ""),
			Password:    getEnv("ALERT_SMTP_PASSWORD", ""),
			FromAddress: getEnv("ALERT_EMAIL_FROM", "alerts@sentinel.local"),
		},
		Telegram: notifiers.TelegramConfig{
			BotToken:   getEnv("ALERT_TELEGRAM_BOT_TOKEN", ""),
			ChatID:     getEnv("ALERT_TELEGRAM_CHAT_ID", ""),
			APIBaseURL: getEnv("ALERT_TELEGRAM_API_BASE_URL", "https://api.telegram.org"),
		},
	}
}

// BuildSender wires cfg into a Dispatcher-compatible sender function: given
// an AlertConfig (which channel, which per-project routing target) and an
// Alert (the message), it hands the notification to the matching notifier
// worker and returns immediately.
//
// This is safe to call synchronously from Dispatch (and therefore from
// processEventInternal) because notifiers.EmailWorker.Send /
// notifiers.TelegramWorker.Send only enqueue onto an in-process buffered
// channel (a non-blocking select/default) — the actual SMTP dial / Telegram
// HTTP POST, and any retry/backoff around it, happens later on the worker's
// own goroutine. Nothing here does network I/O, so there is no timeout for
// event processing to be blocked on (docs/plans/E2E_RECOVERY_PLAN.md P5-1
// item 5).
//
// Per-project routing target: for "email" it is AlertConfig.ChannelConfig["to"];
// for "telegram" it is AlertConfig.ChannelConfig["chat_id"], falling back to
// cfg.Telegram.ChatID (a single shared default chat) when the project has not
// set one. A missing/unknown channel or an unroutable config logs and drops
// the alert rather than panicking — an alert that cannot be delivered must
// never take the ingest pipeline down with it.
func BuildSender(cfg NotifierConfig) func(ctx context.Context, alertCfg *AlertConfig, alert *Alert) {
	emailWorker := notifiers.NewEmailWorker(cfg.Email)
	telegramWorker := notifiers.NewTelegramWorker(cfg.Telegram)

	return func(ctx context.Context, alertCfg *AlertConfig, alert *Alert) {
		if alertCfg == nil || alert == nil {
			return
		}

		switch alertCfg.Channel {
		case "email":
			to, _ := alertCfg.ChannelConfig["to"].(string)
			if to == "" {
				log.Printf("alerts: email channel_config missing \"to\" for project=%s issue=%s, dropping alert",
					alert.ProjectID, alert.IssueID)
				return
			}
			err := emailWorker.Send(&notifiers.EmailNotification{
				ToAddress: to,
				Subject:   fmt.Sprintf("Sentinel alert: issue %s", alert.IssueID),
				Body:      alert.Message,
			})
			if err != nil {
				log.Printf("alerts: failed to queue email for project=%s issue=%s: %v",
					alert.ProjectID, alert.IssueID, err)
			}
		case "telegram":
			// Per-alert chat override is not yet supported end-to-end: the
			// telegram worker is bound to a single chat ID at construction
			// (notifiers.NewTelegramWorker), so a per-project chat_id in
			// ChannelConfig can only be honored if it matches the shared
			// default. This is a known simplification, not a silent gap —
			// see the comment on notifiers.TelegramWorker for the follow-up.
			chatID, _ := alertCfg.ChannelConfig["chat_id"].(string)
			if chatID != "" && chatID != cfg.Telegram.ChatID {
				log.Printf("alerts: telegram channel_config chat_id=%q for project=%s does not match the configured default chat, dropping alert (per-project chat routing not yet implemented)",
					chatID, alert.ProjectID)
				return
			}
			if cfg.Telegram.ChatID == "" && chatID == "" {
				log.Printf("alerts: no telegram chat_id configured for project=%s issue=%s, dropping alert",
					alert.ProjectID, alert.IssueID)
				return
			}
			err := telegramWorker.Send(&notifiers.TelegramNotification{Message: alert.Message})
			if err != nil {
				log.Printf("alerts: failed to queue telegram message for project=%s issue=%s: %v",
					alert.ProjectID, alert.IssueID, err)
			}
		default:
			log.Printf("alerts: unknown channel %q for project=%s issue=%s, dropping alert",
				alertCfg.Channel, alert.ProjectID, alert.IssueID)
		}
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if s := os.Getenv(key); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return defaultValue
}
