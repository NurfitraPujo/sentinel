// Package spanwire is the message the ingestor publishes on the SPANS stream and the processor consumes
// (sentinel-span-ingest D2). Like the ErrorEvent contract, nothing compiles the two sides against each other (B5):
// a field renamed here must be renamed in both in the same change. It is JSON rather than proto because a span has
// no protovalidate rules and `attributes` is free-form.
package spanwire

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

const (
	// Stream and Subject are the JetStream names scripts/nats-init.sh creates (D2: 2 GiB, 24 h, discard-new).
	Stream  = "SPANS"
	Subject = "spans"

	SourceMobile = "mobile"
	SourcePulled = "pulled"

	// MaxMessageBytes keeps a SPANS message under NATS's 1 MiB max_payload with room for headers (D2: "messages
	// < 1 MiB"), the same 1,000,000 the error path's budget uses.
	MaxMessageBytes = 900_000

	// MaxSpansPerTransaction is D2's cap on spans stored from one transaction item: the root plus up to 999 children.
	MaxSpansPerTransaction = 1000
)

// Span is one span as the processor stores it, before the masker and the 8 KiB attribute cap (D7).
type Span struct {
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id,omitempty"`
	Name         string         `json:"name,omitempty"`
	Op           string         `json:"op,omitempty"`
	Kind         string         `json:"kind,omitempty"`
	ServiceName  string         `json:"service_name,omitempty"`
	Environment  string         `json:"environment,omitempty"`
	StartTS      time.Time      `json:"start_ts"`
	DurationUS   int64          `json:"duration_us"`
	Status       string         `json:"status,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
}

// Batch is one SPANS message. OrgID and ProjectID come from the authenticated credential, never from the body
// (B7); the processor trusts them as it trusts an ErrorEvent's resolved project.
type Batch struct {
	OrgID     string `json:"org_id"`
	ProjectID string `json:"project_id,omitempty"`
	Source    string `json:"source"`
	Spans     []Span `json:"spans"`
}

var (
	traceIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDRe  = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ValidTraceID reports whether s is a canonical W3C trace id: 32 lower-case hex digits, not all zero.
func ValidTraceID(s string) bool {
	return traceIDRe.MatchString(s) && s != "00000000000000000000000000000000"
}

// ValidSpanID reports whether s is a canonical span id: 16 lower-case hex digits, not all zero.
func ValidSpanID(s string) bool { return spanIDRe.MatchString(s) && s != "0000000000000000" }

// Encode marshals b into one or more messages, each at most MaxMessageBytes, splitting the span list. A single span
// that does not fit on its own is an error: the caller counts it, it is never silently truncated.
func Encode(b Batch) ([][]byte, error) {
	var out [][]byte
	var cur []Span
	flush := func() error {
		if len(cur) == 0 {
			return nil
		}
		m, err := json.Marshal(Batch{OrgID: b.OrgID, ProjectID: b.ProjectID, Source: b.Source, Spans: cur})
		if err != nil {
			return err
		}
		out = append(out, m)
		cur = nil
		return nil
	}
	size := 0
	for _, s := range b.Spans {
		raw, err := json.Marshal(s)
		if err != nil {
			return nil, fmt.Errorf("spanwire: marshal span: %w", err)
		}
		if len(raw) > MaxMessageBytes-1024 {
			return nil, fmt.Errorf("spanwire: span %s is %d bytes, over the message budget", s.SpanID, len(raw))
		}
		if size+len(raw) > MaxMessageBytes-1024 {
			if err := flush(); err != nil {
				return nil, err
			}
			size = 0
		}
		cur = append(cur, s)
		size += len(raw) + 1
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return out, nil
}

// Decode parses one SPANS message.
func Decode(data []byte) (*Batch, error) {
	var b Batch
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("spanwire: %w", err)
	}
	return &b, nil
}
