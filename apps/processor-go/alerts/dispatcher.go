package alerts

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/procmetrics"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AlertConfig is one row of alert_configs. Since 1722100000_add_alert_config_org_layer.sql, a row is
// either PROJECT-SCOPED (ProjectID set, OrganizationID also set) or ORGANIZATION-WIDE (ProjectID empty,
// OrganizationID set) — the two-layer shape alert_configs now shares with project_api_keys. ID is the
// row's primary key; it is used to key per-config rate-limit counters (see Dispatcher.counters) so that,
// when both layers apply to one project, each config gets its own independent frequency
// threshold/window instead of sharing a single counter.
type AlertConfig struct {
	ID                 string
	ProjectID          string
	OrganizationID     string
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

	// The three caches below are always read/written together under configMu, and always replaced
	// wholesale together by refreshConfigs — never partially — so Dispatch never observes configs
	// from one refresh paired with a projectOrg mapping from another.
	//
	// configs holds PROJECT-SCOPED rows (project_id = P), keyed by project id. This is the same name
	// and shape SetConfigsForTest has always had, kept unchanged so existing callers (including
	// tests/integration, which this change does not touch) keep compiling.
	configs map[string]*AlertConfig
	// orgConfigs holds ORGANIZATION-WIDE rows (project_id IS NULL), keyed by organization id.
	orgConfigs map[string]*AlertConfig
	// projectOrg maps a project id to its organization id. Dispatch is only ever called with a
	// projectID (see the Dispatch signature below) — it has no organizationID to look up orgConfigs
	// with. Rather than querying the projects table per event (a regression on the hot path),
	// refreshConfigs loads this mapping alongside the configs themselves, so resolving "which
	// organization-wide configs apply to this project" at Dispatch time is a pure in-memory lookup,
	// never a DB round trip.
	projectOrg map[string]string
	configMu   sync.RWMutex
	// senderForTest is the dispatcher's outbound send hook. Despite the
	// name (kept for compatibility with existing callers, including
	// SetSenderForTest below), it is no longer test-only: NewProcessorService
	// wires it to a real sender (see alerts.BuildSender) via SetSender, which
	// is the production, non-test entry point. It is left nil until a setter
	// is called, in which case sendAlert falls back to a slog line — this is
	// what made alerting a no-op in production before S8 was fixed (see
	// docs/plans/E2E_RECOVERY_PLAN.md P5-1 / VERIFIED_STATE.md S8).
	senderMu      sync.RWMutex
	senderForTest func(ctx context.Context, cfg *AlertConfig, alert *Alert)
}

type alertCounter struct {
	count       int
	windowStart time.Time
	window      time.Duration
	threshold   int
}

func NewDispatcher(db *pgxpool.Pool) *Dispatcher {
	d := &Dispatcher{
		db:         db,
		counters:   make(map[string]*alertCounter),
		configs:    make(map[string]*AlertConfig),
		orgConfigs: make(map[string]*AlertConfig),
		projectOrg: make(map[string]string),
	}

	// loadConfigs only refreshes on a periodic ticker with no initial load
	// (VERIFIED_STATE.md S8 item 4) — without this, a Dispatcher built right
	// before an event arrives would ignore every alert config until the
	// ticker's first tick, which defeats the "within one event" delivery
	// this dispatcher exists for. Load synchronously, once, here, bounded by
	// a short timeout so a slow/unreachable DB at startup does not hang
	// process startup indefinitely — a failed initial load just means the
	// ticker's next refresh (or a later RefreshConfigsForTest-equivalent
	// call) picks it up instead.
	initCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	d.refreshConfigs(initCtx)
	cancel()

	go d.loadConfigs(context.Background())
	return d
}

// NewDispatcherForTest builds a Dispatcher without spawning the loadConfigs
// background goroutine. It is intended for tests that populate the configs
// map directly via SetConfigsForTest and therefore do not need the periodic
// refresh ticker. The returned Dispatcher otherwise behaves like one built
// by NewDispatcher.
func NewDispatcherForTest(db *pgxpool.Pool) *Dispatcher {
	return &Dispatcher{
		db:         db,
		counters:   make(map[string]*alertCounter),
		configs:    make(map[string]*AlertConfig),
		orgConfigs: make(map[string]*AlertConfig),
		projectOrg: make(map[string]string),
	}
}

// SetConfigsForTest replaces the dispatcher's loaded PROJECT-SCOPED configurations (keyed by project
// id). It is intended for tests that populate the configs map directly without going through the
// loadConfigs background goroutine. Signature and behavior are unchanged from before the two-layer
// (organization-wide) resolution was added, so existing callers keep compiling; see
// SetOrgConfigsForTest and SetProjectOrgForTest for the new organization-wide layer.
func (d *Dispatcher) SetConfigsForTest(configs map[string]*AlertConfig) {
	d.configMu.Lock()
	defer d.configMu.Unlock()
	d.configs = configs
}

// SetOrgConfigsForTest replaces the dispatcher's loaded ORGANIZATION-WIDE configurations (project_id
// IS NULL rows), keyed by organization id. Pair with SetProjectOrgForTest so Dispatch (which only
// receives a projectID) can find them.
func (d *Dispatcher) SetOrgConfigsForTest(configs map[string]*AlertConfig) {
	d.configMu.Lock()
	defer d.configMu.Unlock()
	d.orgConfigs = configs
}

// SetProjectOrgForTest replaces the dispatcher's project id -> organization id mapping, used to resolve
// which organization-wide configs (see SetOrgConfigsForTest) apply to a project passed to Dispatch.
func (d *Dispatcher) SetProjectOrgForTest(projectOrg map[string]string) {
	d.configMu.Lock()
	defer d.configMu.Unlock()
	d.projectOrg = projectOrg
}

// RefreshConfigsForTest exposes the package-private refreshConfigs query so
// tests can drive the loader synchronously without relying on the periodic
// ticker started by NewDispatcher.
func (d *Dispatcher) RefreshConfigsForTest(ctx context.Context) {
	d.refreshConfigs(ctx)
}

// LoadConfigsForTest exposes the package-private loadConfigs loop so tests
// can drive the ticker behavior synchronously with a caller-supplied
// context. The loop returns when the context is cancelled.
func (d *Dispatcher) LoadConfigsForTest(ctx context.Context) {
	d.loadConfigs(ctx)
}

// defaultRefreshInterval is loadConfigs' periodic backstop tick.
//
// Before StartInvalidationSubscriber existed, this ticker was the ONLY path by which a config change
// ever became visible, so it was set conservatively (5 minutes) to keep the DB query cheap. Now that a
// config change made through the dashboard propagates via alert_config.changed in well under a second,
// this ticker's job is narrower: catch a missed/unavailable NATS invalidation, and catch config rows
// written by anything that does NOT go through the dashboard's publish path (a migration, a direct SQL
// edit, an operator running psql, or tests/e2e's alertsSeedConfig helper, which INSERTs directly and
// never publishes alert_config.changed at all). It no longer needs to be the sole correctness mechanism,
// so it no longer needs to be as conservative — see defaultRefreshInterval's value and refreshInterval's
// env override below.
const defaultRefreshInterval = 30 * time.Second

// refreshInterval returns the configured backstop tick period for loadConfigs: ALERT_CONFIG_REFRESH_INTERVAL
// (a duration string, e.g. "30s") when set and parseable, otherwise defaultRefreshInterval. Mirrors the
// pattern of apps/ingestor-go/auth/apikey.go's cacheTTL().
func refreshInterval() time.Duration {
	if v := os.Getenv("ALERT_CONFIG_REFRESH_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		slog.Warn("alerts: ignoring unparseable ALERT_CONFIG_REFRESH_INTERVAL; using default",
			slog.String("value", v), slog.Duration("default", defaultRefreshInterval))
	}
	return defaultRefreshInterval
}

// ConfigRefreshInterval exposes the effective backstop tick period so callers (main.go's startup/warning
// logs, tests) can report the actual worst-case staleness bound a missing or unavailable invalidation
// subscriber falls back to.
func ConfigRefreshInterval() time.Duration { return refreshInterval() }

func (d *Dispatcher) loadConfigs(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshConfigs(ctx)
		}
	}
}

// alertConfigInvalidation is the wire payload the dashboard publishes on the alert_config.changed
// subject after a successful create, update, or delete of an alert_configs row. Both fields are used
// for logging/correlation only: refreshConfigs always reloads the ENTIRE alert_configs table (it has no
// per-project or per-config query path), so there is nothing to filter on here — any message on this
// subject is treated as "reload everything now."
type alertConfigInvalidation struct {
	ProjectID string `json:"projectId"`
	ConfigID  string `json:"configId"`
}

// StartInvalidationSubscriber wires sub, when non-nil, to trigger an immediate refreshConfigs on every
// alert_config.changed message, so a config created/updated/deleted through the dashboard's real API
// becomes visible to Dispatch in well under a second instead of waiting for loadConfigs' periodic
// backstop tick (docs/plans/E2E_RECOVERY_PLAN.md U27; VERIFIED_STATE.md's staleness gap).
//
// sub may be nil, or Subscribe may fail: this mirrors apps/ingestor-go/auth's
// NewAPIKeyAuthenticator/api_key.invalidated pattern of tolerating an unavailable subscriber rather than
// treating it as fatal. Unlike API-key invalidation, correctness here never depended solely on this
// subscriber — loadConfigs' ticker (started unconditionally by NewDispatcher) is the backstop whether or
// not this call ever runs, whether or not sub is nil, and whether or not the dashboard's own publish
// ever succeeds (the assignment is explicit that a publish failure on that side must not fail the
// request). So an unavailable subscriber here only degrades latency — down to ConfigRefreshInterval() —
// never correctness, and is logged loudly rather than silently swallowed.
func (d *Dispatcher) StartInvalidationSubscriber(sub *nats.Subscriber) {
	if sub == nil {
		slog.Warn("alerts: alert_config.changed subscriber unavailable; alert config changes will take up to the periodic backstop refresh interval to take effect",
			slog.Duration("backstop_interval", refreshInterval()))
		return
	}

	err := sub.Subscribe(context.Background(), func(ctx context.Context, data []byte, headers nats.Header) error {
		var msg alertConfigInvalidation
		if jsonErr := json.Unmarshal(data, &msg); jsonErr != nil {
			slog.WarnContext(ctx, "alerts: ignoring unreadable alert_config.changed message",
				slog.String("error", jsonErr.Error()))
			return nil
		}

		refreshCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		d.refreshConfigs(refreshCtx)
		cancel()

		// tests/e2e/alerting_test.go (U27) greps this exact substring — do not reword.
		slog.InfoContext(ctx, fmt.Sprintf("alerts: reloaded alert configs after alert_config.changed (project=%s config=%s)", msg.ProjectID, msg.ConfigID),
			slog.String("project_id", msg.ProjectID), slog.String("config_id", msg.ConfigID),
			slog.String(obs.LogKeyEvent, "alert_config.reloaded"))
		return nil
	})
	if err != nil {
		slog.Warn("alerts: failed to subscribe to alert_config.changed; alert config changes will take up to the periodic backstop refresh interval to take effect",
			slog.String("error", err.Error()), slog.Duration("backstop_interval", refreshInterval()))
		return
	}

	// Errors() MUST be drained by every Subscribe caller (packages/shared-go/nats/subscriber.go's
	// DroppedErrors doc comment) — an unread, capacity-1 channel does not deadlock the fetch loop
	// itself, but it does silently lose error visibility for this subscriber.
	go func() {
		for subErr := range sub.Errors() {
			slog.Error("alerts: alert_config.changed subscriber error", slog.String("error", subErr.Error()))
		}
	}()
}

// refreshConfigs reloads three caches together: the project-scoped alert configs, the
// organization-wide alert configs, and the project->organization mapping needed to resolve the
// latter against a bare projectID at Dispatch time. All three are built into local maps first and
// swapped into the Dispatcher as one atomic step under configMu.Lock — never partially — so Dispatch
// can never observe, say, a freshly reloaded orgConfigs paired with a stale/empty projectOrg that
// would make every organization-wide config unreachable. If either underlying query fails, the whole
// refresh is abandoned and the previous (still consistent) caches are left in place; the next tick or
// invalidation message will retry.
func (d *Dispatcher) refreshConfigs(ctx context.Context) {
	rows, err := d.db.Query(ctx,
		"SELECT id, project_id, organization_id, channel, channel_config, frequency_threshold, frequency_window_seconds, enabled FROM alert_configs WHERE enabled = true",
	)
	if err != nil {
		slog.ErrorContext(ctx, "alerts: failed to load alert configs", slog.String("error", err.Error()))
		return
	}

	projectConfigs := make(map[string]*AlertConfig)
	orgConfigs := make(map[string]*AlertConfig)

	for rows.Next() {
		var cfg AlertConfig
		var configID string
		var projectID sql.NullString
		var channelConfigJSON []byte
		var windowSeconds int

		// project_id is nullable as of 1722100000_add_alert_config_org_layer.sql: NULL means this row
		// is organization-wide. organization_id is NOT NULL on every row (project-scoped rows carry
		// it too, backfilled by that migration), so it scans directly into cfg.OrganizationID.
		if err := rows.Scan(&configID, &projectID, &cfg.OrganizationID, &cfg.Channel, &channelConfigJSON, &cfg.FrequencyThreshold, &windowSeconds, &cfg.Enabled); err != nil {
			slog.WarnContext(ctx, "alerts: skipping unreadable alert_configs row", slog.String("error", err.Error()))
			continue
		}
		cfg.ID = configID

		// channel_config used to be scanned and then discarded, leaving ChannelConfig nil for every
		// config. Both permitted channels read their destination out of it (email's "to", telegram's
		// "chat_id"), so every alert was dropped at the notifier with a "missing" message — alerting
		// could not deliver for ANY configuration, however correct the row.
		if len(channelConfigJSON) > 0 {
			if err := json.Unmarshal(channelConfigJSON, &cfg.ChannelConfig); err != nil {
				// The dashboard has written this column double-encoded (a JSON *string* containing
				// JSON) — see P6. Unwrap one level rather than dropping the config on the floor.
				var nested string
				if json.Unmarshal(channelConfigJSON, &nested) == nil {
					if err2 := json.Unmarshal([]byte(nested), &cfg.ChannelConfig); err2 != nil {
						slog.WarnContext(ctx, "alerts: channel_config is not a JSON object; alerts for this config will drop",
							slog.String("config_id", configID), slog.String("error", err2.Error()))
						continue
					}
				} else {
					slog.WarnContext(ctx, "alerts: channel_config unreadable; alerts for this config will drop",
						slog.String("config_id", configID), slog.String("error", err.Error()))
					continue
				}
			}
		}

		cfg.FrequencyWindow = time.Duration(windowSeconds) * time.Second

		if projectID.Valid && projectID.String != "" {
			cfg.ProjectID = projectID.String
			projectConfigs[cfg.ProjectID] = &cfg
		} else {
			orgConfigs[cfg.OrganizationID] = &cfg
		}
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		slog.ErrorContext(ctx, "alerts: error iterating alert_configs rows", slog.String("error", rowsErr.Error()))
		return
	}

	// Second query: every project's organization, not just projects that happen to have a
	// project-scoped config. Dispatch is called with only a projectID (see below), so this is the
	// only way it can find an applicable organization-wide config without querying the DB per event.
	projectOrg := make(map[string]string)
	orgRows, err := d.db.Query(ctx, "SELECT id, organization_id FROM projects")
	if err != nil {
		slog.ErrorContext(ctx, "alerts: failed to load project->organization mapping", slog.String("error", err.Error()))
		return
	}
	for orgRows.Next() {
		var pid, oid string
		if err := orgRows.Scan(&pid, &oid); err != nil {
			slog.WarnContext(ctx, "alerts: skipping unreadable projects row", slog.String("error", err.Error()))
			continue
		}
		projectOrg[pid] = oid
	}
	orgRowsErr := orgRows.Err()
	orgRows.Close()
	if orgRowsErr != nil {
		slog.ErrorContext(ctx, "alerts: error iterating projects rows", slog.String("error", orgRowsErr.Error()))
		return
	}

	d.configMu.Lock()
	d.configs = projectConfigs
	d.orgConfigs = orgConfigs
	d.projectOrg = projectOrg
	d.configMu.Unlock()
}

// destinationKey identifies where an AlertConfig actually sends to, for deduplication purposes: two
// configs that would deliver to the same channel and the same target must be treated as one
// destination, even if they come from different layers (one organization-wide, one project-scoped) or
// different rows. Mirrors exactly what notify.go's BuildSender reads to route each channel (email's
// "to", telegram's "chat_id"). A missing/empty target, or a channel this dispatcher does not know how
// to extract a target from, falls back to the config's own ID so it is never accidentally deduped
// against an unrelated config that also happens to have no target set.
func destinationKey(cfg *AlertConfig) string {
	var target string
	switch cfg.Channel {
	case "email":
		target, _ = cfg.ChannelConfig["to"].(string)
	case "telegram":
		target, _ = cfg.ChannelConfig["chat_id"].(string)
	}
	if target == "" {
		return cfg.Channel + "|id:" + cfg.ID
	}
	return cfg.Channel + "|" + target
}

// resolveConfigs returns the deduplicated union of the alert configs that apply to projectID,
// implementing the two-layer resolution rule (packages/db-migrations/migrations/
// 1722100000_add_alert_config_org_layer.sql):
//
//	WHERE enabled AND (project_id = P OR (project_id IS NULL AND organization_id = O))
//
// where O is P's organization, found via d.projectOrg. Both layers are UNIONED — an organization-wide
// config is a safety net that must still fire even when a project-scoped config also exists for the
// same project; this deliberately does not implement "project overrides org". The only thing that
// collapses two applicable configs into one is both resolving to the exact same destinationKey (same
// channel, same target) — sending the same event to the same inbox/chat twice per occurrence is worse
// than the redundancy of two configs existing. When a collision happens the project-scoped config is
// kept (it is appended first below, and destinationKey dedup only skips a LATER duplicate) — it is the
// more specific rule, so its frequency threshold/window is the one a caller configuring both would
// reasonably expect to apply.
//
// The enabled filter itself is normally already applied by refreshConfigs' query (WHERE enabled =
// true), so in production every config reaching this function is enabled; Dispatch still checks
// cfg.Enabled defensively (and tests exercise that path directly via SetConfigsForTest /
// SetOrgConfigsForTest with Enabled: false, bypassing the DB-level filter).
//
// Must be called with d.configMu held for reading (or writing).
func (d *Dispatcher) resolveConfigs(projectID string) []*AlertConfig {
	var result []*AlertConfig
	seen := make(map[string]bool)

	if cfg, ok := d.configs[projectID]; ok {
		result = append(result, cfg)
		seen[destinationKey(cfg)] = true
	}

	if orgID, ok := d.projectOrg[projectID]; ok {
		if cfg, ok := d.orgConfigs[orgID]; ok && !seen[destinationKey(cfg)] {
			result = append(result, cfg)
		}
	}

	return result
}

// Dispatch resolves every alert config applicable to projectID (see resolveConfigs — both the
// project-scoped and organization-wide layers, unioned and deduplicated by destination) and evaluates
// each one's frequency threshold/window independently, so an organization-wide config and a
// project-scoped config for the same project rate-limit separately rather than sharing one counter.
func (d *Dispatcher) Dispatch(ctx context.Context, issueID, projectID, errorClass, message string) {
	d.configMu.RLock()
	cfgs := d.resolveConfigs(projectID)
	d.configMu.RUnlock()

	for _, cfg := range cfgs {
		if !cfg.Enabled {
			continue
		}
		d.dispatchToConfig(ctx, cfg, issueID, projectID, errorClass, message)
	}
}

// dispatchToConfig applies cfg's frequency threshold/window to one occurrence and sends when the
// threshold is reached, exactly as Dispatch did when it only ever resolved a single config. The
// counter key includes cfg.ID (in addition to projectID and issueID) so that when Dispatch resolves
// multiple configs for the same project/issue (one org-wide, one project-scoped), each gets its own
// independent counter instead of colliding on one.
func (d *Dispatcher) dispatchToConfig(ctx context.Context, cfg *AlertConfig, issueID, projectID, errorClass, message string) {
	key := projectID + ":" + issueID + ":" + cfg.ID

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

// DispatchOperational sends message immediately through the same sender pipeline as Dispatch
// (email/telegram via SetSender/BuildSender), bypassing the per-project alert_configs lookup and the
// per-issue frequency counters entirely.
//
// Dispatch's design is per-project: it keys AlertConfig lookups and rate-limit counters by ProjectID,
// because an issue always belongs to a project. A DLQ backlog is operational, not per-project — there
// is no ProjectID to key off of, and alert_configs has no row shape for "platform-wide" — so cfg here
// is expected to come from an out-of-band source (see alerts.OperationalAlertConfigFromEnv), not the
// DB-backed configs map. Edge-triggering/rate-limiting for the DLQ case belongs to the caller (see
// apps/processor-go/dlqmonitor.Monitor), which alerts on a Critical-severity transition rather than on
// every call, so this method itself applies none.
//
// A nil cfg, or a cfg with an empty Channel, is a no-op: callers use this to mean "operational alerting
// is not configured," not an error.
func (d *Dispatcher) DispatchOperational(ctx context.Context, cfg *AlertConfig, message string) {
	if cfg == nil || cfg.Channel == "" {
		return
	}
	alert := &Alert{
		IssueID:    "dlq-backlog",
		ProjectID:  "operational",
		Channel:    cfg.Channel,
		Message:    message,
		OccurredAt: time.Now(),
	}
	d.sendAlert(ctx, cfg, alert)
}

func (d *Dispatcher) sendAlert(ctx context.Context, cfg *AlertConfig, alert *Alert) {
	d.senderMu.RLock()
	sender := d.senderForTest
	d.senderMu.RUnlock()

	if sender != nil {
		sender(ctx, cfg, alert)
		return
	}
	// Reached only when no sender has been wired via SetSender/SetSenderForTest — production always
	// wires one (see NewProcessorService). This is the pre-S8 no-op path, kept as a safety net rather
	// than a panic. Recording OutcomeDispatchDropped here is what makes this path distinguishable from
	// a healthy-but-quiet system on sentinel_alert_dispatch_total — see obs.OutcomeDispatchDropped.
	slog.InfoContext(ctx, "ALERT (no sender configured)",
		slog.String("issue_id", alert.IssueID), slog.String("channel", cfg.Channel), slog.String("message", alert.Message))
	procmetrics.RecordAlertDispatch(ctx, cfg.Channel, obs.OutcomeDispatchDropped)
}

// SetSender wires the dispatcher's outbound alert channel to a real sender.
// This is the production entry point: service.NewProcessorService calls it
// with alerts.BuildSender(...) so sendAlert actually reaches the email/
// telegram notifiers instead of only logging "ALERT: ..." (VERIFIED_STATE.md
// S8, docs/plans/E2E_RECOVERY_PLAN.md P5-1 item 3). When s is nil, the
// default logging behavior is restored.
func (d *Dispatcher) SetSender(s func(ctx context.Context, cfg *AlertConfig, alert *Alert)) {
	d.senderMu.Lock()
	defer d.senderMu.Unlock()
	d.senderForTest = s
}

// SetSenderForTest is retained for existing tests that install a capturing
// sender; it is now a thin alias for SetSender, which is the method
// production code (and any new caller) should use.
func (d *Dispatcher) SetSenderForTest(s func(ctx context.Context, cfg *AlertConfig, alert *Alert)) {
	d.SetSender(s)
}

func formatAlertMessage(errorClass, message string, count int) string {
	if len(message) > 100 {
		message = message[:100] + "..."
	}
	return fmt.Sprintf("[%dx] %s: %s", count, errorClass, message)
}
