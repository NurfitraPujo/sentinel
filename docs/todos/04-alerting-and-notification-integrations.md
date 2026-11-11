# TODO 04: Alerting & Notification Integrations

## Priority: Important
## Status: Pending

### Overview
Sentinel processor has a basic Telegram notification helper (`apps/processor-go/notifiers/telegram.go`), but lacks multi-channel integrations and configurable alert rules.

### Requirements
1. **Multi-Channel Notifiers**:
   - Webhook notifier (generic JSON HTTP POST).
   - Slack incoming webhook notifier with formatted markdown cards.
   - PagerDuty integration for critical severity incidents.
   - Email notification support via SMTP / Cloudflare Email.

2. **Alert Rules Engine & UI**:
   - UI in `apps/dashboard-web` to define alert rules per project.
   - Rule conditions: Thresholds (e.g. `> 50 events in 5 minutes`), New Issue detected, or Spike in Error Class rate.
   - Quiet hours / rate suppression per alert rule to prevent notification fatigue.

### Acceptance Criteria
- Triggering an alert rule dispatches formatted notifications across all configured channels.
- Rate suppression prevents spamming channels during large outage spikes.
