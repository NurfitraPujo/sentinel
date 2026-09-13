// Package obs is the shared observability foundation for the Go services (ingestor, processor):
// structured logging via log/slog, an OpenTelemetry provider bootstrap for traces and metrics, and the
// NATS header carrier that lets a W3C trace context cross the publish/consume hop. See
// docs/plans/OBSERVABILITY_PLAN.md — this package implements W0, the contracts every other work
// package (W1 ingestor, W2 processor, W4 the proving row) is briefed from.
//
// Nothing in this package may be renamed casually. The constants below exist as constants — not
// ad hoc string literals at each call site — because on 2026-07-30 two readers of a day-old contract
// independently invented different words for one state within an hour (see BUGS.md's B5 addendum).
// One name, defined here, used everywhere.
package obs

// HTTPRequestIDHeader is the response header the ingestor sets to the current trace's hex id (D-d):
// human-friendly name, standard value. Also honoured as an *inbound* correlation hint by callers that
// want to log it, though the authoritative propagation mechanism is always the W3C `traceparent`
// header, never this one.
const HTTPRequestIDHeader = "X-Request-Id"

// LogKeyTraceID is the slog attribute key a recording span's trace id is logged under. Injected
// automatically by Handler (below) — call sites must never set this key by hand.
const LogKeyTraceID = "trace_id"

// LogKeySpanID is the slog attribute key a recording span's span id is logged under. Injected
// automatically by Handler (below) — call sites must never set this key by hand.
const LogKeySpanID = "span_id"

// LogKeyService is the slog attribute key every log line carries, set once by Setup.
const LogKeyService = "service"

// LogKeyEvent is the slog attribute key for fixed, machine-greppable event names (D-f's
// api_key.invalidated publish-failure line is the first consumer of this convention). A value under
// this key is an identifier, not prose: stable across a reword of the surrounding message.
const LogKeyEvent = "event"

// Metric names, and the label keys and values that go with them.
//
// These are constants for the same reason the log/header keys above are. The plan listed them as a table
// in prose, which would have meant W1 (ingestor), W2 (processor) and W4 (the e2e assertion) each
// hand-typing the same seven strings — one contract, three independent transcriptions. That is precisely
// the configuration that produced both B5 recurrences on 2026-07-30, the second of which took an hour to
// appear: two readers of a day-old contract invented different words for one state.
//
// Naming follows Prometheus convention (<system>_<subsystem>_<name>_<unit>) because the metrics are
// exposed through the OTel Prometheus exporter.
const (
	MetricIngestRequests        = "sentinel_ingest_requests_total"
	MetricIngestPublishFailures = "sentinel_ingest_publish_failures_total"
	MetricProcessDuration       = "sentinel_process_duration_seconds"
	MetricProcessEvents         = "sentinel_process_events_total"
	MetricDLQDepth              = "sentinel_dlq_depth"
	MetricDLQPublishFailures    = "sentinel_dlq_publish_failures_total"
	MetricAlertDispatch         = "sentinel_alert_dispatch_total"
	// MetricIngestEventIDReplaced counts requests whose event_id the ingestor replaced with a minted
	// UUIDv4 instead of using the client's value verbatim (docs/plans/IDEMPOTENCY_PLAN.md D-a). Never
	// records the id itself as a label (D15 cardinality rule) - only the fixed EventIDReason* below.
	MetricIngestEventIDReplaced = "sentinel_ingest_event_id_replaced_total"

	// The four sentinel-project-environments §12 instruments. They are declared here, beside the
	// eight above, for the same B5 reason: infra's Prometheus rules (a THIRD repo — see decision 17)
	// name these strings in their `expr`s, and the only defence against a rule that silently matches
	// nothing is that exactly one place in this repo spells each name.
	//
	// None of them carries a label. `environment` is client-controlled and must never become one
	// (obs/provider.go's unboundedMetricAttributes deny-filter exists for exactly this class), and
	// project_id is unbounded for the reason the LabelOutcome block below already records.

	// MetricUnregisteredEnvironmentEvents counts stored occurrences whose environment has no
	// project_environments row (decision 9: the event is ACCEPTED and stored regardless — ingest
	// never fails because of the registry). Sustained above zero means errors are landing in an
	// environment nobody declared, so they are unfilterable, unlabelled and ungrantable.
	MetricUnregisteredEnvironmentEvents = "sentinel_unregistered_environment_events_total"
	// MetricEnvironmentRegistryTouchFailures counts failures of §4.8's
	// `UPDATE project_environments SET last_seen_at = now() ...` touch. Above zero means
	// first_seen_at/last_seen_at are drifting from reality and an admin cannot tell a live
	// environment from a dead one.
	MetricEnvironmentRegistryTouchFailures = "sentinel_environment_registry_touch_failures_total"
	// MetricAlertsSuppressedByEnvironment counts occurrences that an alert config did NOT count
	// toward its threshold because the occurrence's environment is outside the config's
	// `environments` scope (§4.7). Its alert rule is "sustained zero WHILE
	// MetricAlertConfigsEnvironmentScoped > 0" — i.e. scoping is configured and suppressing nothing,
	// which is the only signal in the feature that catches it silently not being applied.
	MetricAlertsSuppressedByEnvironment = "sentinel_alerts_suppressed_by_environment_total"
	// MetricAlertConfigsEnvironmentScoped is a GAUGE, not a counter: the number of enabled
	// alert_configs rows whose `environments` is non-NULL, set on every refreshConfigs pass
	// (apps/processor-go/alerts/dispatcher.go). It alerts on nothing by itself — it exists solely as
	// the predicate the rule above needs, because a flat-zero suppression counter is also what a
	// correctly installed feature with nothing to do looks like.
	MetricAlertConfigsEnvironmentScoped = "sentinel_alert_configs_environment_scoped"

	// MetricAffectedRollupSkipped counts user/tenant identifiers that the processor read out of event
	// metadata but did NOT write to issue_affected_users / issue_affected_tenants because the value
	// was unusable (sentinel-affected-users §4.3 decision 5: empty, longer than 255 bytes, or carrying
	// a control character). The event itself is still stored; only the rollup skips it. Incremented
	// AFTER commit, beside MetricUnregisteredEnvironmentEvents, so a rolled-back event never counts.
	// Two labels, both closed sets declared below: LabelKind (AffectedKindUser / AffectedKindTenant)
	// and LabelReason (AffectedRollupReason*). Never the value itself — it is a client-supplied string
	// (D15's cardinality rule), and a 300-byte id as a label is the exact failure the deny-filter in
	// provider.go exists to prevent. It is the only way to diagnose a permanent `0` on the dashboard
	// (§12): a producer sending `user_id: ""` shows up here as reason="empty", which is
	// distinguishable from "no key at all", which increments nothing.
	MetricAffectedRollupSkipped = "sentinel_affected_rollup_skipped_total"
)

// Label keys. Kept few and low-cardinality on purpose: project_id is unbounded and would blow up the
// registry, so it is deliberately absent (OBSERVABILITY_PLAN.md §5).
const (
	LabelOutcome = "outcome"
	LabelChannel = "channel"
	LabelReason  = "reason"
	// LabelKind names WHICH identifier MetricAffectedRollupSkipped skipped: AffectedKindUser or
	// AffectedKindTenant. Nothing else records it.
	LabelKind = "kind"
)

// Permitted values for LabelOutcome. Fixed sets, so a typo at one call site cannot invent an eighth
// time series that looks almost like a real one.
const (
	OutcomeAccepted     = "accepted"     // ingest: 202
	OutcomeRejected     = "rejected"     // ingest: 4xx validation failure
	OutcomeRateLimited  = "ratelimited"  // ingest: 429
	OutcomeUnauthorized = "unauthorized" // ingest: 401
	OutcomeStored       = "stored"       // process: persisted
	OutcomeRetried      = "retried"      // process: returned to NATS for redelivery
	OutcomeDeadLettered = "deadlettered" // process: parked in the DLQ
	// OutcomeDuplicate marks a message whose (issue_id, event_id) pair was already stored
	// (docs/plans/IDEMPOTENCY_PLAN.md D-e): store.StoreEvent returned stored=false, nil — a healthy
	// no-op ACK, not a failure. Recorded EXACTLY ONCE per message, by ProcessEvent's deferred
	// classifier alone (never inside processEventInternal too — recording it in both places would
	// double-count one message across two outcome labels, breaking the
	// stored+duplicate+retried+deadlettered == deliveries invariant W3 asserts).
	OutcomeDuplicate       = "duplicate" // process: (issue_id, event_id) already stored; no-op ACK
	OutcomeDispatchSent    = "sent"      // alert: notifier accepted it
	OutcomeDispatchError   = "error"     // alert: notifier failed
	OutcomeDispatchDropped = "dropped"   // alert: never reached a notifier worker (missing/unroutable
	// channel_config, unknown channel, a full notifier queue, or no sender wired at all — see
	// apps/processor-go/alerts/notify.go and dispatcher.go's sendAlert). Without this outcome,
	// "alerting configured and quiet" and "alerting silently broken" both read as a flat zero on
	// sentinel_alert_dispatch_total — the exact blind spot that let alerting ship as a no-op before
	// S8 was caught (docs/memory/VERIFIED_STATE.md).
)

// Permitted values for LabelReason on MetricIngestEventIDReplaced (docs/plans/IDEMPOTENCY_PLAN.md
// D-a). Fixed set for the same reason as the Outcome* block above - a typo'd literal at a call site
// must not be able to invent a new time series.
const (
	EventIDReasonEmpty   = "empty"    // client sent no event_id (or "") - the common absent case
	EventIDReasonTooLong = "too_long" // client's event_id exceeded 64 characters (counted in runes)
	// EventIDReasonInvalidChars: the id carried a control character (rune < 0x20 or 0x7f). NUL in
	// particular cannot be stored in a Postgres varchar at all, so passing it through would let a
	// client dead-letter its own events once the processor writes the column (F-VW0-2).
	EventIDReasonInvalidChars = "invalid_chars"
)

// Permitted values for LabelKind and LabelReason on MetricAffectedRollupSkipped
// (sentinel-affected-users §4.3 / §12, decision 24). Both sets are CLOSED: the reason set is exactly
// these three and the contract says so in those words. AffectedRollupReasonControlChar is a NEW
// constant, deliberately not EventIDReasonInvalidChars above — the two metrics are different
// contracts read by different rules, and the contract names this reason `control_char`. `empty` is
// a member because a `0` caused by producers sending `user_id: ""` is exactly what the metric exists
// to diagnose; a key that is absent altogether increments nothing.
const (
	AffectedKindUser   = "user"
	AffectedKindTenant = "tenant"

	AffectedRollupReasonEmpty       = "empty"        // the key was present and its value was ""
	AffectedRollupReasonTooLong     = "too_long"     // the value exceeded 255 BYTES (VARCHAR(255) on both side tables)
	AffectedRollupReasonControlChar = "control_char" // the value carried a rune < 0x20 or 0x7f (NUL cannot be stored in a varchar at all)
)
