package sentrycompat

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

// ErrBadTransaction is a transaction item that cannot yield even its root span: not a JSON object, or no usable
// contexts.trace (trace_id / span_id) or start/end timestamps.
var ErrBadTransaction = errors.New("invalid transaction item")

const (
	maxSpanNameBytes = 1024
	maxSpanOpBytes   = 128
	maxStatusBytes   = 64
	maxEnvBytes      = 64
)

// TransactionResult is what one transaction item yields.
type TransactionResult struct {
	Environment string
	Spans       []spanwire.Span // the root first, then children in item order; at most spanwire.MaxSpansPerTransaction
	Truncated   int             // child spans past the cap, counted and not stored (D2)
	Invalid     int             // child spans skipped for an unusable id or timestamp
}

type sentryTrace struct {
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id"`
	Op           string         `json:"op"`
	Status       string         `json:"status"`
	Description  string         `json:"description"`
	Data         map[string]any `json:"data"`
}

type sentrySpan struct {
	TraceID      string            `json:"trace_id"`
	SpanID       string            `json:"span_id"`
	ParentSpanID string            `json:"parent_span_id"`
	Op           string            `json:"op"`
	Description  string            `json:"description"`
	Status       string            `json:"status"`
	Start        json.RawMessage   `json:"start_timestamp"`
	End          json.RawMessage   `json:"timestamp"`
	Data         map[string]any    `json:"data"`
	Tags         map[string]string `json:"tags"`
}

type sentryTransaction struct {
	Transaction string            `json:"transaction"`
	Start       json.RawMessage   `json:"start_timestamp"`
	End         json.RawMessage   `json:"timestamp"`
	Environment string            `json:"environment"`
	Release     string            `json:"release"`
	Platform    string            `json:"platform"`
	Tags        map[string]string `json:"tags"`
	Contexts    struct {
		Trace sentryTrace `json:"trace"`
	} `json:"contexts"`
	Spans []json.RawMessage `json:"spans"`
}

// ParseTransaction turns one Sentry `transaction` item into span rows (D2): one for the transaction's root
// (contexts.trace + start_timestamp/timestamp) and one per spans[] entry, at most spanwire.MaxSpansPerTransaction
// in all. Attributes are bounded here to spanwire.MaxAttributeBytes so a message stays under the NATS budget; the
// processor masks them and caps them again (D7).
func ParseTransaction(raw []byte) (*TransactionResult, error) {
	var tx sentryTransaction
	if err := json.Unmarshal(raw, &tx); err != nil {
		return nil, ErrBadTransaction
	}
	tr := tx.Contexts.Trace
	if !spanwire.ValidTraceID(tr.TraceID) || !spanwire.ValidSpanID(tr.SpanID) {
		return nil, ErrBadTransaction
	}
	start, okS := sentryTime(tx.Start)
	end, okE := sentryTime(tx.End)
	if !okS || !okE {
		return nil, ErrBadTransaction
	}
	env := spanwire.CleanString(tx.Environment, maxEnvBytes)

	rootAttrs := map[string]any{}
	for k, v := range tr.Data {
		rootAttrs[k] = v
	}
	if len(tx.Tags) > 0 {
		rootAttrs["tags"] = tagsAttr(tx.Tags)
	}
	if tx.Release != "" {
		rootAttrs["release"] = tx.Release
	}
	if tx.Platform != "" {
		rootAttrs["platform"] = tx.Platform
	}
	name := firstNonEmpty(tx.Transaction, tr.Description, tr.Op)

	res := &TransactionResult{Environment: env}
	res.Spans = append(res.Spans, spanwire.Span{
		TraceID:      tr.TraceID,
		SpanID:       tr.SpanID,
		ParentSpanID: validParent(tr.ParentSpanID),
		Name:         spanwire.CleanString(name, maxSpanNameBytes),
		Op:           spanwire.CleanString(tr.Op, maxSpanOpBytes),
		Environment:  env,
		StartTS:      start,
		DurationUS:   durationUS(start, end),
		Status:       spanwire.CleanString(strings.ToLower(tr.Status), maxStatusBytes),
		Attributes:   boundAttrs(rootAttrs),
	})

	seen := map[string]struct{}{tr.SpanID: {}}
	for i, rawSpan := range tx.Spans {
		if len(res.Spans) >= spanwire.MaxSpansPerTransaction {
			res.Truncated = len(tx.Spans) - i
			break
		}
		var sp sentrySpan
		if json.Unmarshal(rawSpan, &sp) != nil {
			res.Invalid++
			continue
		}
		// A span names the trace it is in; one that names a different trace than its transaction is not part of this
		// transaction and is skipped rather than re-parented.
		if sp.TraceID != "" && sp.TraceID != tr.TraceID {
			res.Invalid++
			continue
		}
		if !spanwire.ValidSpanID(sp.SpanID) {
			res.Invalid++
			continue
		}
		if _, dup := seen[sp.SpanID]; dup {
			res.Invalid++
			continue
		}
		s0, ok0 := sentryTime(sp.Start)
		s1, ok1 := sentryTime(sp.End)
		if !ok0 || !ok1 {
			res.Invalid++
			continue
		}
		seen[sp.SpanID] = struct{}{}
		attrs := map[string]any{}
		for k, v := range sp.Data {
			attrs[k] = v
		}
		if len(sp.Tags) > 0 {
			attrs["tags"] = tagsAttr(sp.Tags)
		}
		res.Spans = append(res.Spans, spanwire.Span{
			TraceID:      tr.TraceID,
			SpanID:       sp.SpanID,
			ParentSpanID: validParent(sp.ParentSpanID),
			Name:         spanwire.CleanString(firstNonEmpty(sp.Description, sp.Op), maxSpanNameBytes),
			Op:           spanwire.CleanString(sp.Op, maxSpanOpBytes),
			Environment:  env,
			StartTS:      s0,
			DurationUS:   durationUS(s0, s1),
			Status:       spanwire.CleanString(strings.ToLower(sp.Status), maxStatusBytes),
			Attributes:   boundAttrs(attrs),
		})
	}
	return res, nil
}

// tagsAttr is a decoded-JSON-shaped copy of the tags (map[string]any), so CleanValue reaches every string in it.
func tagsAttr(tags map[string]string) map[string]any {
	m := make(map[string]any, len(tags))
	for k, v := range tags {
		m[k] = v
	}
	return m
}

func boundAttrs(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	clean, _ := spanwire.CleanValue(m).(map[string]any)
	return spanwire.CapAttributes(clean, spanwire.MaxAttributeBytes)
}

func validParent(s string) string {
	if spanwire.ValidSpanID(s) {
		return s
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func durationUS(start, end time.Time) int64 {
	d := end.Sub(start).Microseconds()
	if d < 0 {
		return 0
	}
	return d
}

// sentryTime reads a Sentry timestamp with the mapper's parser (Unix seconds or RFC 3339); the zero time is "none".
func sentryTime(r json.RawMessage) (time.Time, bool) {
	t := parseSentryTime(raw(r))
	return t, !t.IsZero()
}
