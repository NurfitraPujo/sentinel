package event

import "strings"

// sentinel-reporting-and-dashboard D18/D19: where an error came from. core tags every event with `service` (the
// subcommand) and `source` (the capture site); the processor promotes both, plus whether the event happened inside an
// API request, to columns on error_occurrences so the dashboard can tell an API error from a worker error.
//
// The wire schema v2 has NO top-level `service` (apps/processor-go/event.ErrorEvent has none; transcribed 2026-10-07),
// so `service` is read from tags.service. This is NOT error_occurrences.source, the wire's top-level capture origin
// (window.onerror, sentry-compat ...): that column keeps its meaning and is written by the caller exactly as before.

// maxOriginLength is D18's cap on service and tag_source.
const maxOriginLength = 64

// The tags.source values core emits that D18 and D19 read (core pkg/sentinelx/bounds.go).
const (
	TagSourceHTTPHandler = "http_handler"
	TagSourceHTTPPanic   = "http_panic"
	TagSourceLogger      = "logger"
	TagSourceJob         = "job"
	TagSourceWorker      = "worker_handler"
	TagSourceNATSClient  = "nats_client"
)

// Origin is the D18 triple stored on an occurrence. Service and TagSource are "" when absent (stored NULL).
type Origin struct {
	Service       string
	TagSource     string
	RequestScoped bool
}

// DeriveOrigin is D18's rule, a pure function of the event's tags, transaction and request URL. The SQL backfill
// (migration 1726000100) is its twin and must stay identical.
//
//   - service and tag_source: the tag, trimmed, at most 64 characters, "" when absent or empty.
//   - request_scoped: tag_source is http_handler or http_panic; or tag_source is logger AND the event carries the
//     request context core's HTTP layer attaches (transaction set and request.url set).
func DeriveOrigin(tags map[string]string, transaction, requestURL string) Origin {
	o := Origin{
		Service:   cleanOrigin(tags["service"]),
		TagSource: cleanOrigin(tags["source"]),
	}
	switch o.TagSource {
	case TagSourceHTTPHandler, TagSourceHTTPPanic:
		o.RequestScoped = true
	case TagSourceLogger:
		o.RequestScoped = transaction != "" && requestURL != ""
	}
	return o
}

// cleanOrigin trims and truncates to maxOriginLength characters (runes), matching left(btrim(x), 64) in the backfill.
func cleanOrigin(v string) string {
	v = strings.Trim(v, " ")
	if r := []rune(v); len(r) > maxOriginLength {
		v = string(r[:maxOriginLength])
	}
	return v
}

// The D19 groups, in decision order. The strings are the `group` label values of D8a and D20.
const (
	GroupAPI          = "api"
	GroupJobs         = "jobs"
	GroupNATS         = "nats"
	GroupOther        = "other"
	GroupUnattributed = "unattributed"
)

// Group is D19's five-way split, decided in this order: API (request_scoped), Jobs (tag_source job), NATS
// (worker_handler / nats_client), Other (any other non-null service), Unattributed (no service).
func (o Origin) Group() string {
	switch {
	case o.RequestScoped:
		return GroupAPI
	case o.TagSource == TagSourceJob:
		return GroupJobs
	case o.TagSource == TagSourceWorker || o.TagSource == TagSourceNATSClient:
		return GroupNATS
	case o.Service != "":
		return GroupOther
	default:
		return GroupUnattributed
	}
}
