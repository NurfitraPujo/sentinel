package sentrycompat

import (
	"log/slog"
	"net/http"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

// processTransactions is the transaction half of an envelope (sentinel-span-ingest D2): each kept `transaction` item
// becomes span rows, all of the envelope's spans are encoded into SPANS messages and published, and each item is
// counted `stored` or `rejected` on sentinel_compat_items_total. It reports false after writing a 5xx — only when
// the publish itself fails, so the client's outbox retries; a malformed item is counted and skipped and never fails
// the request (a bad span must not block the errors batched beside it).
func processTransactions(w http.ResponseWriter, r *http.Request, cfg Config, items [][]byte) bool {
	if len(items) == 0 {
		return true
	}
	ctx := r.Context()
	logger := cfg.logger()

	var orgID, projectID string
	var ok bool
	if cfg.Identity != nil {
		orgID, projectID, ok = cfg.Identity(ctx)
	}
	// A mobile span is shown only through its project (D8): an organization-wide key that named no project would
	// store spans nobody can ever see, so its transaction items are rejected and counted.
	if !ok || orgID == "" || projectID == "" {
		for range items {
			cfg.record(ctx, "transaction", OutcomeRejected)
		}
		return true
	}

	batch := spanwire.Batch{OrgID: orgID, ProjectID: projectID, Source: spanwire.SourceMobile}
	parsed := 0
	var truncated, invalid int
	for _, raw := range items {
		res, err := ParseTransaction(raw)
		if err != nil {
			cfg.record(ctx, "transaction", OutcomeRejected)
			continue
		}
		parsed++
		batch.Spans = append(batch.Spans, res.Spans...)
		truncated += res.Truncated
		invalid += res.Invalid
	}
	if parsed == 0 {
		return true
	}

	msgs, err := spanwire.Encode(batch)
	if err != nil {
		// A single span too big for a message: the rest of the envelope is not worth a retry loop. Counted, not 5xx.
		logger.WarnContext(ctx, "span encode failed; transaction items dropped", slog.Any("error", err))
		for i := 0; i < parsed; i++ {
			cfg.record(ctx, "transaction", OutcomeRejected)
		}
		return true
	}
	for _, m := range msgs {
		if err := cfg.Spans(ctx, m); err != nil {
			logger.ErrorContext(ctx, "Failed to publish spans", slog.Any("error", err))
			for i := 0; i < parsed; i++ {
				cfg.record(ctx, "transaction", OutcomeFailed)
			}
			http.Error(w, "Failed to ingest spans", http.StatusInternalServerError)
			return false
		}
	}
	for i := 0; i < parsed; i++ {
		cfg.record(ctx, "transaction", OutcomeStored)
	}
	cfg.recordSpans(ctx, "stored", len(batch.Spans))
	cfg.recordSpans(ctx, "truncated", truncated)
	cfg.recordSpans(ctx, "invalid", invalid)
	return true
}
