package obs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// Setup builds the process-wide *slog.Logger for serviceName.
//
//   - Handler: JSON by default (containers); LOG_FORMAT=text switches to a human-readable handler for
//     local dev.
//   - Level: LOG_LEVEL (debug/info/warn/error, case-insensitive), default info. An unrecognized value
//     falls back to info rather than erroring — a typo'd env var must not crash the service.
//   - Every line carries LogKeyService=serviceName.
//   - Every line logged through a context that carries a span automatically carries LogKeyTraceID/
//     LogKeySpanID (see Handler) — call sites log with *Context methods (InfoContext, ErrorContext,
//     ...) and a context; they must never thread trace/span ids by hand.
func Setup(serviceName string) *slog.Logger {
	return SetupTo(os.Stdout, serviceName)
}

// SetupTo is Setup with an explicit destination. It exists so the "every line carries LogKeyService"
// promise is testable: with Setup writing only to os.Stdout, a mutation that dropped the service attribute
// entirely left the whole unit suite green (found by W0's review). A seam that makes a documented
// guarantee assertable is worth more than one fewer exported function.
func SetupTo(w io.Writer, serviceName string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(os.Getenv("LOG_LEVEL"))}

	var base slog.Handler
	if strings.EqualFold(os.Getenv("LOG_FORMAT"), "text") {
		base = slog.NewTextHandler(w, opts)
	} else {
		base = slog.NewJSONHandler(w, opts)
	}

	handler := NewHandler(base).WithAttrs([]slog.Attr{slog.String(LogKeyService, serviceName)})
	return slog.New(handler)
}

func parseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Handler wraps an slog.Handler and injects LogKeyTraceID/LogKeySpanID into every record whose context
// carries a span with a valid span context (trace.SpanContextFromContext(ctx).IsValid()) — this covers
// both a locally-started recording span and a span context extracted from an inbound traceparent, which
// is deliberately broader than "only a locally recording span": the trace id is worth logging for
// correlation either way, sampling decision notwithstanding. A context with no span in it (the common
// case for anything logged outside a request/message handler) is passed through unchanged — this is not
// an error, it is simply "no trace in scope right now," and call sites never need to know the
// difference: they just log with a context.
type Handler struct {
	next slog.Handler
}

// NewHandler wraps next. Setup is the common entry point; NewHandler is exported for callers assembling
// a custom handler chain (e.g. a test that wants to inspect records without going through Setup's
// env-driven format/level selection).
func NewHandler(next slog.Handler) *Handler {
	return &Handler{next: next}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, record slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		record.AddAttrs(
			slog.String(LogKeyTraceID, sc.TraceID().String()),
			slog.String(LogKeySpanID, sc.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, record)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name)}
}
