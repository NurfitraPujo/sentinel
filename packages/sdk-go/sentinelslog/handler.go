package sentinelslog

import (
	"context"
	"errors"
	"log/slog"

	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

type Handler struct {
	handler slog.Handler
}

func NewHandler(h slog.Handler) *Handler {
	return &Handler{handler: h}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		var err error
		attrs := make(map[string]interface{})
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "err" || a.Key == "error" {
				if e, ok := a.Value.Any().(error); ok {
					err = e
				}
			}
			attrs[a.Key] = a.Value.Any()
			return true
		})

		if err == nil {
			err = errors.New(r.Message)
		}

		sentinel.CaptureErrorContext(ctx, err, attrs)
	}

	return h.handler.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{handler: h.handler.WithAttrs(attrs)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{handler: h.handler.WithGroup(name)}
}
