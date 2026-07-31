package obs

import natsgo "github.com/nats-io/nats.go"

// NATSHeaderCarrier adapts nats.Header to propagation.TextMapCarrier (go.opentelemetry.io/otel/
// propagation), so a W3C traceparent (and baggage) can ride across a NATS publish/consume hop the same
// way it rides HTTP headers — see OBSERVABILITY_PLAN.md D-e: "the wire contract is `traceparent` in
// NATS headers... the propagator carrier is the only bespoke code, and it lives in
// packages/shared-go/obs next to its constants." This is that carrier, and the only one.
//
// nats.Header is a plain map[string][]string with case-SENSITIVE keys (unlike net/http.Header, which
// canonicalizes). That is harmless here: both Inject and Extract go through the same
// otel.GetTextMapPropagator(), which always spells its own keys ("traceparent", "tracestate",
// "baggage") consistently, so there is nothing for case sensitivity to trip over.
//
// Get (used by Extract) is nil-safe: extracting from a message published with no headers at all (or by
// a pre-W0 publisher) degrades cleanly to a new root trace, per D-e. Set (used by Inject) is NOT
// nil-safe — it requires a non-nil, already-allocated map, mirroring
// go.opentelemetry.io/otel/propagation.HeaderCarrier's identical contract for http.Header. Construct
// with NATSHeaderCarrier(natsgo.Header{}) (or wrap an existing populated nats.Msg.Header) before
// injecting.
type NATSHeaderCarrier natsgo.Header

// Get returns the first value for key, or "" if key is absent or the carrier wraps a nil map.
func (c NATSHeaderCarrier) Get(key string) string {
	return natsgo.Header(c).Get(key)
}

// Set replaces the values for key with a single value. Panics if the underlying map is nil — see the
// type doc comment.
func (c NATSHeaderCarrier) Set(key, value string) {
	natsgo.Header(c).Set(key, value)
}

// Keys returns all keys currently stored in the carrier.
func (c NATSHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	return keys
}
