// Transcribed (not imported — the dashboard is a separate npm module, and the Go side is a
// separate module graph entirely) from packages/shared-go/obs/obs.go. Keep these values byte-for-byte
// identical to that file; drifting either side silently breaks cross-service trace correlation
// (docs/plans/OBSERVABILITY_PLAN.md §2, D-d/D-e).
//
//   HTTPRequestIDHeader = "X-Request-Id"
//   LogKeyTraceID       = "trace_id"
//   LogKeySpanID        = "span_id"
//   LogKeyService       = "service"
//   LogKeyEvent         = "event"

/** Response header carrying the request's trace id (hex), same name the Go services use. */
export const HTTP_REQUEST_ID_HEADER = 'X-Request-Id';

/** slog-equivalent attribute keys — kept identical to packages/shared-go/obs so a log aggregator
 * can query `trace_id`/`service`/`event` the same way across all three services. */
export const LOG_KEY_TRACE_ID = 'trace_id';
export const LOG_KEY_SPAN_ID = 'span_id';
export const LOG_KEY_SERVICE = 'service';
export const LOG_KEY_EVENT = 'event';

/** service.name — the dashboard's identity in every log line and (if OTel is active) every span. */
export const SERVICE_NAME = 'dashboard-web';
