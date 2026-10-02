package spanpull

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

// tempoStore reads Tempo's `GET /api/traces/{traceId}` with Accept: application/json: OTLP JSON,
// `{"batches":[{"resource":…,"scopeSpans":[{"spans":[…]}]}]}`. Older builds spell the middle level
// `instrumentationLibrarySpans` and the v2 endpoint wraps the list as `resourceSpans`; all are read.
type tempoStore struct {
	base   string
	client *http.Client
}

type otlpValue struct {
	StringValue *string         `json:"stringValue"`
	IntValue    json.RawMessage `json:"intValue"` // a JSON string ("12") per the protobuf-JSON mapping of int64, or a number
	DoubleValue *float64        `json:"doubleValue"`
	BoolValue   *bool           `json:"boolValue"`
	BytesValue  *string         `json:"bytesValue"`
	ArrayValue  *struct {
		Values []otlpValue `json:"values"`
	} `json:"arrayValue"`
	KvlistValue *struct {
		Values []otlpKV `json:"values"`
	} `json:"kvlistValue"`
}

type otlpKV struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId"`
	Name              string          `json:"name"`
	Kind              json.RawMessage `json:"kind"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano"`
	Attributes        []otlpKV        `json:"attributes"`
	Status            struct {
		Code json.RawMessage `json:"code"`
	} `json:"status"`
}

type otlpScope struct {
	Spans []otlpSpan `json:"spans"`
}

type otlpResourceSpans struct {
	Resource struct {
		Attributes []otlpKV `json:"attributes"`
	} `json:"resource"`
	ScopeSpans []otlpScope `json:"scopeSpans"`
	ILS        []otlpScope `json:"instrumentationLibrarySpans"`
}

type otlpTrace struct {
	Batches       []otlpResourceSpans `json:"batches"`
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
	Trace         *struct {
		ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
	} `json:"trace"`
}

func (t *tempoStore) Fetch(ctx context.Context, traceID string) ([]spanwire.Span, error) {
	body, err := getJSON(ctx, t.client, t.base, "/api/traces/"+url.PathEscape(traceID))
	if err != nil {
		return nil, err
	}
	return parseOTLP(body, traceID)
}

// parseOTLP converts an OTLP JSON trace into spans of wantTrace. A span of any other trace id is skipped, so a
// store that returned the wrong trace can never put another trace's spans under this id.
func parseOTLP(body []byte, wantTrace string) ([]spanwire.Span, error) {
	var tr otlpTrace
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("tempo response: %w", err)
	}
	groups := tr.Batches
	groups = append(groups, tr.ResourceSpans...)
	if tr.Trace != nil {
		groups = append(groups, tr.Trace.ResourceSpans...)
	}
	var out []spanwire.Span
	for _, g := range groups {
		res := otlpAttrs(g.Resource.Attributes)
		service, _ := res["service.name"].(string)
		env, _ := res["environment"].(string)
		if env == "" {
			env, _ = res["deployment.environment"].(string)
		}
		scopes := append(append([]otlpScope{}, g.ScopeSpans...), g.ILS...)
		for _, sc := range scopes {
			for _, s := range sc.Spans {
				if normID(s.TraceID, 32) != wantTrace {
					continue
				}
				id := normID(s.SpanID, 16)
				if id == "" {
					continue
				}
				start, ok := unixNano(s.StartTimeUnixNano)
				if !ok {
					continue
				}
				end, endOK := unixNano(s.EndTimeUnixNano)
				var dur int64
				if endOK && end.After(start) {
					dur = end.Sub(start).Microseconds()
				}
				out = append(out, spanwire.Span{
					TraceID:      wantTrace,
					SpanID:       id,
					ParentSpanID: normID(s.ParentSpanID, 16),
					Name:         s.Name,
					Kind:         otlpKind(s.Kind),
					ServiceName:  service,
					Environment:  env,
					StartTS:      start,
					DurationUS:   dur,
					Status:       otlpStatus(s.Status.Code),
					Attributes:   otlpAttrs(s.Attributes),
				})
			}
		}
	}
	return out, nil
}

func unixNano(raw json.RawMessage) (time.Time, bool) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}, false
	}
	return time.Unix(0, n).UTC(), true
}

var otlpKinds = []string{"unspecified", "internal", "server", "client", "producer", "consumer"}

// otlpKind accepts the enum as a name ("SPAN_KIND_SERVER") or its number.
func otlpKind(raw json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if n, err := strconv.Atoi(s); err == nil {
		if n >= 0 && n < len(otlpKinds) {
			return otlpKinds[n]
		}
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(s, "SPAN_KIND_"))
}

// otlpStatus maps the status code to ok | error | unset.
func otlpStatus(raw json.RawMessage) string {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch strings.TrimPrefix(s, "STATUS_CODE_") {
	case "1", "OK":
		return "ok"
	case "2", "ERROR":
		return "error"
	default:
		return "unset"
	}
}

func otlpAttrs(kvs []otlpKV) map[string]any {
	if len(kvs) == 0 {
		return nil
	}
	m := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		if kv.Key != "" {
			m[kv.Key] = otlpAny(kv.Value)
		}
	}
	return m
}

func otlpAny(v otlpValue) any {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case len(v.IntValue) > 0:
		s := strings.Trim(string(v.IntValue), `"`)
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
		return s
	case v.DoubleValue != nil:
		return *v.DoubleValue
	case v.BoolValue != nil:
		return *v.BoolValue
	case v.BytesValue != nil:
		return *v.BytesValue
	case v.ArrayValue != nil:
		out := make([]any, 0, len(v.ArrayValue.Values))
		for _, e := range v.ArrayValue.Values {
			out = append(out, otlpAny(e))
		}
		return out
	case v.KvlistValue != nil:
		return otlpAttrs(v.KvlistValue.Values)
	}
	return nil
}
