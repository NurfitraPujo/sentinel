package spanpull

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/spans"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

const (
	pollInterval = 15 * time.Second
	claimBatch   = 10
)

// Worker drains the pull queue: for each due trace it fetches the spans from the trace store and stores them with
// source 'pulled' under the triggering organization (D3). Nothing here can fail error ingest: it is a separate loop,
// and every failure is logged, counted and retried a bounded number of times.
type Worker struct {
	Queue  *Queue
	Spans  *spans.Store
	Traces TraceStore
	Cfg    Config
	Now    func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run polls until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		for {
			n, err := w.RunOnce(ctx)
			if err != nil {
				slog.WarnContext(ctx, "span pull: claim failed", slog.String("error", err.Error()))
				break
			}
			if n < claimBatch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RunOnce claims one batch of due jobs and processes them in order. It returns how many it claimed.
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	// Before the scopes are validated every organization looks "not allowed"; claiming now would burn attempts and
	// the delisted branch would finish valid jobs (a restart window). Leave the queue untouched until the gate is up.
	if !w.Cfg.Validated() {
		return 0, nil
	}
	jobs, err := w.Queue.Claim(ctx, claimBatch)
	if err != nil {
		return 0, err
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		w.process(ctx, j)
	}
	return len(jobs), nil
}

func (w *Worker) process(ctx context.Context, j Job) {
	// The allow-list (published, so an org absent from it is truly delisted) is checked again here: the row may predate a configuration change, and trace ids are
	// client-asserted, so the guard must hold at the moment of the copy, not only at the enqueue.
	if !w.Cfg.Allowed(j.OrgID) {
		slog.WarnContext(ctx, "span pull: organization no longer allow-listed; dropping the job")
		if err := w.Queue.Finish(ctx, j); err != nil {
			slog.WarnContext(ctx, "span pull: finish failed", slog.String("error", err.Error()))
		}
		return
	}

	// The attempt was counted when the job was claimed. Past the bound this stage is given up on, so a job that keeps
	// failing or keeps crashing its worker moves on instead of retrying every lease forever.
	if j.Attempts > MaxAttemptsPerStage {
		count(ctx, fetchesCtr, "gave_up")
		slog.WarnContext(ctx, "span pull: giving up on a stage after repeated attempts", slog.Int("stage", j.Stage), slog.Int("attempts", j.Attempts-1))
		w.advance(ctx, j)
		return
	}

	fetchCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
	fetched, err := w.Traces.Fetch(fetchCtx, j.TraceID)
	cancel()
	outcome := "ok"
	switch {
	case errors.Is(err, ErrNotFound):
		// Not flushed yet is normal at the first stage (the second one tries again), so this is not a warning.
		fetched, err, outcome = nil, nil, "not_found"
		slog.DebugContext(ctx, "span pull: trace not found", slog.Int("stage", j.Stage))
	case errors.Is(err, ErrTooLarge):
		// Over the read bound or refused by the store as too big: a trace only grows, so neither a retry nor the
		// second stage can succeed; the job is finished.
		count(ctx, fetchesCtr, "too_large")
		slog.InfoContext(ctx, "span pull: trace too large for the trace store; finishing the job", slog.Int("stage", j.Stage))
		if err := w.Queue.Finish(ctx, j); err != nil {
			slog.WarnContext(ctx, "span pull: finish failed", slog.String("error", err.Error()))
		}
		return
	case err != nil:
		w.fail(ctx, j, err)
		return
	}

	if len(fetched) > MaxSpansPerTrace {
		spans.RecordDropped(ctx, spanwire.SourcePulled, spans.DropOverTraceCap, len(fetched)-MaxSpansPerTrace)
		fetched = fetched[:MaxSpansPerTrace]
	}
	now := w.now()
	rows := make([]spans.Row, 0, len(fetched))
	for _, sp := range fetched {
		prepared, reason := spans.Prepare(sp, now)
		if reason != "" {
			spans.RecordDropped(ctx, spanwire.SourcePulled, reason, 1)
			continue
		}
		rows = append(rows, spans.Row{OrgID: j.OrgID, ScopeProjectID: w.Cfg.ScopeProject(j.OrgID), Source: spanwire.SourcePulled, Span: prepared})
	}
	n, err := w.Spans.Insert(ctx, rows)
	if err != nil {
		w.fail(ctx, j, err)
		return
	}
	spans.RecordStored(ctx, spanwire.SourcePulled, n)
	count(ctx, fetchesCtr, outcome)
	w.advance(ctx, j)
}

func (w *Worker) advance(ctx context.Context, j Job) {
	if _, err := w.Queue.Advance(ctx, j); err != nil {
		slog.WarnContext(ctx, "span pull: advance failed", slog.String("error", err.Error()))
	}
}

func (w *Worker) fail(ctx context.Context, j Job, cause error) {
	if err := w.Queue.Fail(ctx, j, cause); err != nil {
		slog.WarnContext(ctx, "span pull: could not record a failed fetch", slog.String("error", err.Error()))
	}
	count(ctx, fetchesCtr, "error")
	slog.WarnContext(ctx, "span pull: fetch failed", slog.Int("stage", j.Stage), slog.Int("attempt", j.Attempts),
		slog.String("error", cause.Error()))
}
