package sentrycompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/enrich"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// Ingester is the slice of *service.IngestService the handlers call: IngestAt with a publish budget (§4.7).
type Ingester interface {
	IngestAtWithin(ctx context.Context, payload *validation.ErrorPayload, receivedAt time.Time,
		budget int, shed service.Shedder) error
}

// Config is what the handlers need from main.go.
type Config struct {
	Ingest        Ingester
	Scope         func(ctx context.Context, p *validation.ErrorPayload) error // main.go: applyAuthenticatedScope
	Logger        *slog.Logger                                                // nil → slog.Default()
	RecordItem    func(ctx context.Context, itemType, outcome string)         // nil → no-op (§4.6)
	PublishBudget int                                                         // 0 → service.MaxPublishBytes (§4.7); tests pass a smaller one
}

func (c Config) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c Config) record(ctx context.Context, itemType, outcome string) {
	if c.RecordItem != nil {
		c.RecordItem(ctx, itemType, outcome)
	}
}

func (c Config) budget() int {
	if c.PublishBudget > 0 {
		return c.PublishBudget
	}
	return service.MaxPublishBytes
}

// statusFor is the one error → status function both routes use (spec §4.1).
func statusFor(err error) (int, string) {
	var mbe *http.MaxBytesError
	switch {
	case errors.Is(err, ErrUnsupportedEncoding):
		return http.StatusUnsupportedMediaType, "Unsupported Content-Encoding"
	case errors.Is(err, ErrTooManyItems):
		return http.StatusRequestEntityTooLarge, "Too many envelope items"
	case errors.As(err, &mbe), errors.Is(err, ErrBodyTooLarge):
		return http.StatusRequestEntityTooLarge, "Request body too large"
	case errors.Is(err, ErrCorruptBody):
		return http.StatusBadRequest, "invalid compressed request body"
	case errors.Is(err, ErrMalformedEnvelope):
		return http.StatusBadRequest, err.Error()
	default:
		return http.StatusBadRequest, "Bad request"
	}
}

// NewEnvelopeHandler serves POST /api/{project_id}/envelope/ (spec §4.2). The whole envelope is read before any
// event is ingested, so every 4xx of §4.1 and §4.3 is decided before anything is published.
func NewEnvelopeHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := http.MaxBytesReader(w, r.Body, MaxCompressedBodyBytes)
		rd, err := DecodeBody(body, r.Header.Get("Content-Encoding"), MaxDecompressedBodyBytes)
		if err != nil {
			writeStatus(w, err)
			return
		}
		env, err := ReadEnvelope(rd)
		if err != nil {
			writeStatus(w, err)
			return
		}
		var events [][]byte
		for _, it := range env.Items {
			switch {
			case it.Type != "event":
				cfg.record(r.Context(), it.Type, OutcomeDiscarded)
			case it.Oversized:
				cfg.record(r.Context(), "event", OutcomeTooLarge)
			default:
				events = append(events, it.Payload)
			}
		}
		ingestEvents(w, r, cfg, env.Header, events)
	})
}

// NewStoreHandler serves POST /api/{project_id}/store/: one event JSON, the same encodings, a 1 MiB cap that is a
// 413 here because a store body has no rest to process (V4).
func NewStoreHandler(cfg Config) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := http.MaxBytesReader(w, r.Body, MaxCompressedBodyBytes)
		rd, err := DecodeBody(body, r.Header.Get("Content-Encoding"), MaxEventItemBytes)
		if err != nil {
			writeStatus(w, err)
			return
		}
		payload, err := io.ReadAll(rd)
		if err != nil {
			if errors.Is(err, ErrBodyTooLarge) {
				cfg.record(r.Context(), "event", OutcomeTooLarge)
			}
			writeStatus(w, err)
			return
		}
		ingestEvents(w, r, cfg, EnvelopeHeader{}, [][]byte{payload})
	})
}

func writeStatus(w http.ResponseWriter, err error) {
	code, msg := statusFor(err)
	http.Error(w, msg, code)
}

// ingestEvents is steps 5-7 of §4.2, shared by both handlers: map, scope and ingest each kept event in order; the
// first failure answers and stops the envelope, and every kept event after it is counted skipped.
func ingestEvents(w http.ResponseWriter, r *http.Request, cfg Config, header EnvelopeHeader, payloads [][]byte) {
	ctx := enrich.WithBudget(r.Context(), enrich.NewBudget())
	receivedAt := time.Now().UTC()
	logger := cfg.logger()
	// Resolved once per request (spec §3.7.3); the untrusted-peer log line, when it applies, is written here once.
	relay := RelayIdentityFromRequest(r, logger)

	firstID := ""
	for i, raw := range payloads {
		fail := func(outcome string, code int, msg string) {
			cfg.record(ctx, "event", outcome)
			for range payloads[i+1:] {
				cfg.record(ctx, "event", OutcomeSkipped)
			}
			http.Error(w, msg, code)
		}

		p, err := MapEvent(raw, header)
		if err != nil {
			fail(OutcomeRejected, http.StatusBadRequest, "invalid event payload")
			return
		}
		relay.Apply(p)
		if cfg.Scope != nil {
			if err := cfg.Scope(ctx, p); err != nil {
				fail(OutcomeRejected, http.StatusForbidden, err.Error())
				return
			}
		}
		if err := cfg.Ingest.IngestAtWithin(ctx, p, receivedAt, cfg.budget(), Shed); err != nil {
			switch {
			case errors.Is(err, service.ErrEventTooLarge) || errors.Is(err, natsgo.ErrMaxPayload):
				logger.WarnContext(ctx, fmt.Sprintf("Sentry-compatible event too large: %v", err),
					slog.String("project_key", p.ProjectKey), slog.Any("error", err))
				fail(OutcomeTooLarge, http.StatusRequestEntityTooLarge, "Event too large")
			case strings.Contains(err.Error(), "validation failed"):
				logger.ErrorContext(ctx, fmt.Sprintf("Failed to ingest error: %v", err), slog.Any("error", err), slog.String("project_key", p.ProjectKey))
				fail(OutcomeRejected, http.StatusBadRequest, err.Error())
			default:
				logger.ErrorContext(ctx, fmt.Sprintf("Failed to ingest error: %v", err), slog.Any("error", err), slog.String("project_key", p.ProjectKey))
				fail(OutcomeFailed, http.StatusInternalServerError, "Failed to ingest event")
			}
			return
		}
		cfg.record(ctx, "event", OutcomeAccepted)
		if firstID == "" {
			firstID = p.EventID // the EFFECTIVE id IngestAt stamped
		}
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]string{}
	if firstID != "" {
		resp["id"] = firstID
	}
	_ = json.NewEncoder(w).Encode(resp)
}
