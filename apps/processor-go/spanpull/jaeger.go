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

// jaegerStore reads VictoriaTraces' Jaeger query API: `GET /select/jaeger/api/traces/{traceId}`, answering
// `{"data":[{"traceID":…,"spans":[…],"processes":{"p1":{"serviceName":…,"tags":[…]}}}]}`.
type jaegerStore struct {
	base   string
	client *http.Client
}

type jaegerTag struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type jaegerSpan struct {
	TraceID       string `json:"traceID"`
	SpanID        string `json:"spanID"`
	ParentSpanID  string `json:"parentSpanID"`
	OperationName string `json:"operationName"`
	References    []struct {
		RefType string `json:"refType"`
		TraceID string `json:"traceID"`
		SpanID  string `json:"spanID"`
	} `json:"references"`
	StartTime int64       `json:"startTime"` // microseconds since the epoch
	Duration  int64       `json:"duration"`  // microseconds
	Tags      []jaegerTag `json:"tags"`
	ProcessID string      `json:"processID"`
}

type jaegerResponse struct {
	Data []struct {
		TraceID   string       `json:"traceID"`
		Spans     []jaegerSpan `json:"spans"`
		Processes map[string]struct {
			ServiceName string      `json:"serviceName"`
			Tags        []jaegerTag `json:"tags"`
		} `json:"processes"`
	} `json:"data"`
}

func (j *jaegerStore) Fetch(ctx context.Context, traceID string) ([]spanwire.Span, error) {
	body, err := getJSON(ctx, j.client, j.base, "/select/jaeger/api/traces/"+url.PathEscape(traceID))
	if err != nil {
		return nil, err
	}
	return parseJaeger(body, traceID)
}

// jaegerInternalTags are mapped onto span fields rather than copied into attributes.
var jaegerInternalTags = map[string]bool{
	"span.kind": true, "otel.status_code": true, "otel.status_description": true,
	"otel.library.name": true, "otel.library.version": true,
}

func parseJaeger(body []byte, wantTrace string) ([]spanwire.Span, error) {
	var resp jaegerResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("jaeger response: %w", err)
	}
	var out []spanwire.Span
	for _, tr := range resp.Data {
		for _, s := range tr.Spans {
			if normID(s.TraceID, 32) != wantTrace {
				continue
			}
			id := normID(s.SpanID, 16)
			if id == "" || s.StartTime <= 0 {
				continue
			}
			parent := normID(s.ParentSpanID, 16)
			for _, ref := range s.References {
				if ref.RefType == "CHILD_OF" && normID(ref.TraceID, 32) == wantTrace {
					if p := normID(ref.SpanID, 16); p != "" {
						parent = p
						break
					}
				}
			}
			var service, env string
			if p, ok := tr.Processes[s.ProcessID]; ok {
				service = p.ServiceName
				for _, t := range p.Tags {
					if v, _ := t.Value.(string); v != "" && (t.Key == "environment" || (t.Key == "deployment.environment" && env == "")) {
						env = v
					}
				}
			}
			attrs := map[string]any{}
			kind, status := "", ""
			isError := false
			for _, t := range s.Tags {
				switch {
				case t.Key == "span.kind":
					kind, _ = t.Value.(string)
				case t.Key == "otel.status_code":
					status = strings.ToLower(fmt.Sprint(t.Value))
				case t.Key == "error":
					if b, ok := t.Value.(bool); ok && b {
						isError = true
					} else if sv, ok := t.Value.(string); ok && sv == "true" {
						isError = true
					}
					attrs[t.Key] = t.Value
				case jaegerInternalTags[t.Key]:
				default:
					attrs[t.Key] = jaegerValue(t)
				}
			}
			switch {
			case status == "error" || isError:
				status = "error"
			case status == "ok":
			default:
				status = "unset"
			}
			dur := s.Duration
			if dur < 0 {
				dur = 0
			}
			out = append(out, spanwire.Span{
				TraceID:      wantTrace,
				SpanID:       id,
				ParentSpanID: parent,
				Name:         s.OperationName,
				Kind:         strings.ToLower(kind),
				ServiceName:  service,
				Environment:  env,
				StartTS:      time.UnixMicro(s.StartTime).UTC(),
				DurationUS:   dur,
				Status:       status,
				Attributes:   attrs,
			})
		}
	}
	return out, nil
}

// jaegerValue keeps a tag's JSON value; int64 tags may arrive as strings and are converted back to numbers.
func jaegerValue(t jaegerTag) any {
	if s, ok := t.Value.(string); ok && t.Type == "int64" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	}
	return t.Value
}
