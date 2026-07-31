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
)

// Label keys. Kept few and low-cardinality on purpose: project_id is unbounded and would blow up the
// registry, so it is deliberately absent (OBSERVABILITY_PLAN.md §5).
const (
	LabelOutcome = "outcome"
	LabelChannel = "channel"
)

// Permitted values for LabelOutcome. Fixed sets, so a typo at one call site cannot invent an eighth
// time series that looks almost like a real one.
const (
	OutcomeAccepted        = "accepted"     // ingest: 202
	OutcomeRejected        = "rejected"     // ingest: 4xx validation failure
	OutcomeRateLimited     = "ratelimited"  // ingest: 429
	OutcomeUnauthorized    = "unauthorized" // ingest: 401
	OutcomeStored          = "stored"       // process: persisted
	OutcomeRetried         = "retried"      // process: returned to NATS for redelivery
	OutcomeDeadLettered    = "deadlettered" // process: parked in the DLQ
	OutcomeDispatchSent    = "sent"         // alert: notifier accepted it
	OutcomeDispatchError   = "error"        // alert: notifier failed
	OutcomeDispatchDropped = "dropped"      // alert: never reached a notifier worker (missing/unroutable
	// channel_config, unknown channel, a full notifier queue, or no sender wired at all — see
	// apps/processor-go/alerts/notify.go and dispatcher.go's sendAlert). Without this outcome,
	// "alerting configured and quiet" and "alerting silently broken" both read as a flat zero on
	// sentinel_alert_dispatch_total — the exact blind spot that let alerting ship as a no-op before
	// S8 was caught (docs/memory/VERIFIED_STATE.md).
)
