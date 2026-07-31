package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/notifiers"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/procmetrics"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// channelUnknown is the LabelChannel value recorded for an alertCfg.Channel this switch does not
// recognize. Deliberately NOT the raw, project-controlled alertCfg.Channel string: unlike "email"/
// "telegram" (fixed literals every call site already agrees on), an unrecognized channel value comes
// straight from the alert_configs table with no enum/CHECK constraint behind it, so recording it
// verbatim would let a project owner mint arbitrary Prometheus label values — the same unbounded-
// cardinality mistake LabelOutcome's fixed constants exist to prevent (see obs.go's doc comment).
const channelUnknown = "unknown"

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
				slog.WarnContext(ctx, "alerts: email channel_config missing \"to\", dropping alert",
					slog.String("project_id", alert.ProjectID), slog.String("issue_id", alert.IssueID))
				procmetrics.RecordAlertDispatch(ctx, "email", obs.OutcomeDispatchDropped)
				return
			}
			err := emailWorker.Send(&notifiers.EmailNotification{
				ToAddress: to,
				Subject:   fmt.Sprintf("Sentinel alert: issue %s", alert.IssueID),
				Body:      alert.Message,
			})
			if err != nil {
				slog.ErrorContext(ctx, "alerts: failed to queue email",
					slog.String("project_id", alert.ProjectID), slog.String("issue_id", alert.IssueID),
					slog.String("error", err.Error()))
				procmetrics.RecordAlertDispatch(ctx, "email", obs.OutcomeDispatchDropped)
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
				slog.WarnContext(ctx, "alerts: telegram channel_config chat_id does not match the configured default chat, dropping alert (per-project chat routing not yet implemented)",
					slog.String("chat_id", chatID), slog.String("project_id", alert.ProjectID))
				procmetrics.RecordAlertDispatch(ctx, "telegram", obs.OutcomeDispatchDropped)
				return
			}
			if cfg.Telegram.ChatID == "" && chatID == "" {
				slog.WarnContext(ctx, "alerts: no telegram chat_id configured, dropping alert",
					slog.String("project_id", alert.ProjectID), slog.String("issue_id", alert.IssueID))
				procmetrics.RecordAlertDispatch(ctx, "telegram", obs.OutcomeDispatchDropped)
				return
			}
			err := telegramWorker.Send(&notifiers.TelegramNotification{Message: alert.Message})
			if err != nil {
				slog.ErrorContext(ctx, "alerts: failed to queue telegram message",
					slog.String("project_id", alert.ProjectID), slog.String("issue_id", alert.IssueID),
					slog.String("error", err.Error()))
				procmetrics.RecordAlertDispatch(ctx, "telegram", obs.OutcomeDispatchDropped)
			}
		default:
			slog.WarnContext(ctx, "alerts: unknown channel, dropping alert",
				slog.String("channel", alertCfg.Channel), slog.String("project_id", alert.ProjectID),
				slog.String("issue_id", alert.IssueID))
			procmetrics.RecordAlertDispatch(ctx, channelUnknown, obs.OutcomeDispatchDropped)
		}
	}
}

// OperationalAlertConfigFromEnv builds the AlertConfig used for operational, non-project alerts — today
// only the DLQ backlog monitor (apps/processor-go/dlqmonitor). alert_configs rows are per-project
// (ProjectID, owned through the dashboard's alert-config API); a DLQ backlog belongs to the platform,
// not any one project, and there is no ProjectID to key a lookup on. Rather than force this into that
// shape — inventing a magic sentinel ProjectID, or a DB row nothing else in the schema expects — this
// mirrors NotifierConfigFromEnv's existing split of "channel-wide, deployment-controlled setting" from
// "per-alert routing target set through the product": PROCESSOR_DLQ_ALERT_CHANNEL/_TO/_CHAT_ID play the
// same role for the operational alert that alert_configs.channel/channel_config play per-project.
//
// If a proper "operational alerts" concept is ever added to the product (e.g. a non-project-scoped
// alert_configs row, or a dedicated table), this function should be replaced by that lookup — this is
// documented as the interim shape, not a permanent design decision.
//
// Returns nil when PROCESSOR_DLQ_ALERT_CHANNEL is unset, which DispatchOperational and
// dlqmonitor.Monitor both treat as "operational alerting is disabled" (the /health endpoint still
// reports DLQ state regardless — this only controls the push side).
func OperationalAlertConfigFromEnv() *AlertConfig {
	channel := getEnv("PROCESSOR_DLQ_ALERT_CHANNEL", "")
	if channel == "" {
		return nil
	}

	channelConfig := map[string]interface{}{}
	switch channel {
	case "email":
		if to := getEnv("PROCESSOR_DLQ_ALERT_TO", ""); to != "" {
			channelConfig["to"] = to
		} else {
			slog.Warn("alerts: PROCESSOR_DLQ_ALERT_CHANNEL=email but PROCESSOR_DLQ_ALERT_TO is unset; operational DLQ alerts will drop at BuildSender")
		}
	case "telegram":
		// chat_id may be left unset here: BuildSender falls back to the shared
		// ALERT_TELEGRAM_CHAT_ID default when ChannelConfig has none.
		if chatID := getEnv("PROCESSOR_DLQ_ALERT_CHAT_ID", ""); chatID != "" {
			channelConfig["chat_id"] = chatID
		}
	default:
		slog.Warn("alerts: unknown PROCESSOR_DLQ_ALERT_CHANNEL (want \"email\" or \"telegram\"); operational DLQ alerts will drop at BuildSender",
			slog.String("channel", channel))
	}

	return &AlertConfig{Channel: channel, ChannelConfig: channelConfig, Enabled: true}
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
