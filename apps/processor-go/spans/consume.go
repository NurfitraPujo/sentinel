package spans

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/masker"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

const (
	MetricStored  = "sentinel_spans_stored_total"  // rows newly written, by source
	MetricDropped = "sentinel_spans_dropped_total" // spans not written, by source and reason
)

// Drop reasons (a closed set, so the metric cannot grow a label per client string).
const (
	DropInvalid      = "invalid"        // a malformed id, an empty trace, an over-long field after cleaning
	DropOutOfWindow  = "out_of_window"  // start_ts outside the created partitions (D1)
	DropBadTenant    = "bad_tenant"     // no organization, or a project outside the stated organization
	DropOverTraceCap = "over_trace_cap" // pulled: past the 5,000-span cap (D3)
)

var (
	meter       = otel.Meter("processor-go")
	storedCtr   metric.Int64Counter
	droppedCtr  metric.Int64Counter
	spanMasker  = masker.NewMasker()
	maxNameByte = 1024
)

func init() {
	// Creation errors leave the nil-safe no-op instrument the OTel API hands back; recording is guarded below.
	storedCtr, _ = meter.Int64Counter(MetricStored, metric.WithDescription("Span rows newly stored, by source"), metric.WithUnit("{span}"))
	droppedCtr, _ = meter.Int64Counter(MetricDropped, metric.WithDescription("Spans not stored, by source and reason"), metric.WithUnit("{span}"))
}

// RecordStored and RecordDropped are exported for the pull worker, which counts through the same instruments.
func RecordStored(ctx context.Context, source string, n int) {
	if storedCtr != nil && n > 0 {
		storedCtr.Add(ctx, int64(n), metric.WithAttributes(attribute.String("source", source)))
	}
}

func RecordDropped(ctx context.Context, source, reason string, n int) {
	if droppedCtr != nil && n > 0 {
		droppedCtr.Add(ctx, int64(n), metric.WithAttributes(attribute.String("source", source), attribute.String("reason", reason)))
	}
}

// Prepare turns one wire span into a stored row, or reports why it is dropped. It applies D7: the name and the
// attributes go through the masker, the attributes are capped at spanwire.MaxAttributeBytes (largest values cut
// first, marked), and a span whose start_ts is outside the partition window is refused (D1).
func Prepare(sp spanwire.Span, now time.Time) (spanwire.Span, string) {
	if !spanwire.ValidTraceID(sp.TraceID) || !spanwire.ValidSpanID(sp.SpanID) {
		return sp, DropInvalid
	}
	if sp.ParentSpanID != "" && !spanwire.ValidSpanID(sp.ParentSpanID) {
		sp.ParentSpanID = ""
	}
	if sp.StartTS.IsZero() || !InWindow(sp.StartTS, now) {
		return sp, DropOutOfWindow
	}
	if sp.DurationUS < 0 {
		sp.DurationUS = 0
	}
	sp.StartTS = sp.StartTS.UTC()
	sp.Name = spanMask(spanwire.CleanString(sp.Name, maxNameByte))
	sp.Op = spanwire.CleanString(sp.Op, 128)
	sp.Kind = spanwire.CleanString(sp.Kind, 32)
	sp.Status = spanwire.CleanString(strings.ToLower(sp.Status), 64)
	sp.ServiceName = spanwire.CleanString(sp.ServiceName, 128)
	sp.Environment = spanwire.CleanString(sp.Environment, 64)
	if len(sp.Attributes) > 0 {
		clean, _ := spanwire.CleanValue(sp.Attributes).(map[string]any)
		// Mask first, then cap: masking only ever shortens a value, but the marker it writes can lengthen a short one,
		// so the cap is the last step and its result is what is stored.
		sp.Attributes = spanwire.CapAttributes(spanMasker.MaskMap(clean), spanwire.MaxAttributeBytes)
	}
	return sp, ""
}

func spanMask(s string) string { return spanMasker.MaskString(s) }

// Consumer handles one SPANS message.
type Consumer struct {
	Store *Store
	Now   func() time.Time
}

func (c *Consumer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Handle decodes one message and writes it. A message that can never succeed (undecodable, no organization, a
// project that is not in the stated organization) is counted and ACKED — returning nil — because redelivering the
// same bytes cannot help and spans must never be dead-lettered into the error DLQ's operators' queue. A database
// failure is returned so the subscriber redelivers (bounded by its MaxDeliver).
func (c *Consumer) Handle(ctx context.Context, data []byte) error {
	b, err := spanwire.Decode(data)
	if err != nil {
		slog.WarnContext(ctx, "spans: undecodable message dropped", slog.String("error", err.Error()))
		RecordDropped(ctx, "unknown", DropInvalid, 1)
		return nil
	}
	source := b.Source
	if source != spanwire.SourceMobile && source != spanwire.SourcePulled {
		RecordDropped(ctx, "unknown", DropInvalid, len(b.Spans))
		return nil
	}
	if b.OrgID == "" {
		RecordDropped(ctx, source, DropBadTenant, len(b.Spans))
		return nil
	}

	// A mobile span belongs to a project and is shown only through it (D8): one with no project (an organization-wide
	// key that named none) could never be seen, so it is not stored.
	if source == spanwire.SourceMobile && b.ProjectID == "" {
		RecordDropped(ctx, source, DropBadTenant, len(b.Spans))
		return nil
	}

	serviceFallback := ""
	if b.ProjectID != "" {
		// The credential already bound this project to this organization at the ingestor; re-checking here costs one
		// primary-key read per message and means a stale or forged message cannot write spans into an organization the
		// project is not in (B7).
		org, name, found, err := c.Store.ProjectInfo(ctx, b.ProjectID)
		if err != nil {
			return fmt.Errorf("spans: resolve project: %w", err)
		}
		if !found || org != b.OrgID {
			RecordDropped(ctx, source, DropBadTenant, len(b.Spans))
			return nil
		}
		serviceFallback = name
	}

	now := c.now()
	rows := make([]Row, 0, len(b.Spans))
	for _, sp := range b.Spans {
		prepared, reason := Prepare(sp, now)
		if reason != "" {
			RecordDropped(ctx, source, reason, 1)
			continue
		}
		if prepared.ServiceName == "" {
			prepared.ServiceName = serviceFallback
		}
		rows = append(rows, Row{OrgID: b.OrgID, ProjectID: b.ProjectID, Source: source, Span: prepared})
	}
	n, err := c.Store.Insert(ctx, rows)
	if err != nil {
		return fmt.Errorf("spans: insert: %w", err)
	}
	RecordStored(ctx, source, n)
	return nil
}
