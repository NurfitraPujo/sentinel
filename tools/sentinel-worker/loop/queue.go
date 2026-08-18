package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// Dispatcher owns the plan §3 "per-issue serial queues" layer: jobs for one issue run in order,
// single-flight, while different issues run concurrently (one goroutine per issue, lazily
// started, no other locking). It sits between PollLoop/Classify and Runner.Run, and implements
// the three behaviors §3 assigns to this layer that Runner.Run itself does not:
//
//   - Coalescing (ALL kinds): queued-but-not-yet-running jobs of the SAME kind for one issue
//     collapse into one — the latest triggerSeq wins, the loser(s) are journaled `superseded` so
//     crash recovery never resurrects them (plan §2.2).
//   - FOLLOW-UP debounce: a FOLLOW-UP job waits one poll interval before running, so C10's two
//     adjacent events (`commented` + `question_answered`) for the same reply land in one job
//     instead of two.
//   - Panic recovery per job: a panic inside Runner.Run is caught, journaled `failed`, and logged
//     — it must never take down the dispatcher goroutine, let alone the process.
//
// Non-job kinds (KindCancelQueued, KindSkippedDeleted, KindSweepReconcile) are handled by
// Dispatch directly rather than queued: cancel/delete drop this issue's pending queued job (if
// any) and journal it `skipped` with the matching reason (plan §3's "cancel queued jobs" /
// "journal skipped(deleted)"); sweep-reconcile has no per-issue job to run against and is handed
// to OnSweepReconcile for the sweep component (jobs/sweep.go, a later phase) to pick up.
type Dispatcher struct {
	Runner  *Runner
	Journal *state.Journal
	Log     *slog.Logger

	// Debounce is how long a queued FOLLOW-UP job waits before running (plan §3: "one poll
	// interval"). Zero means run immediately (used by tests that don't care about debounce).
	Debounce time.Duration

	// Sleep is used for the debounce wait; overridable in tests. Defaults to a context-aware
	// real-time sleep.
	Sleep func(ctx context.Context, d time.Duration)

	// Ctx is the process's shutdown-aware context (main sets it to the same context
	// signal.NotifyContext cancels on SIGTERM/SIGINT). It is the base context every queued job's
	// own context is derived from -- the debounce wait and Runner.Run both observe its
	// cancellation -- so shutdown actually reaches in-flight work instead of Enqueue/runOne
	// severing it behind their own context.Background() (finding 1). Nil is treated as
	// context.Background() (tests, and any caller that hasn't wired shutdown, keep working
	// unchanged).
	Ctx context.Context

	wg sync.WaitGroup // tracks every currently-running per-issue queue goroutine, for Drain

	// OnSweepReconcile, when non-nil, is called for every KindSweepReconcile dispatch (a
	// claim_released event whose newValue.previousAssignee is us, plan §2.7/§3). The sweep
	// component that consumes it (jobs/sweep.go) is out of N8a's scope; this hook is the seam it
	// plugs into later.
	OnSweepReconcile func(Event)

	// OnOutcome, when non-nil, is called once for every terminal outcome the DISPATCHER itself
	// decides (as opposed to Runner.Run's own outcomes, wired via loop.Runner.OnOutcome): a
	// coalesced loser journaled `superseded`, a cancelled queued job journaled `skipped(<reason>)`,
	// or a panicking job journaled `failed`. Kept as its own hook (not shared state with Runner)
	// so this package stays free of a health import, matching Runner.OnOutcome's own reasoning --
	// main.go wires both to the same health.Status.Inc closure so plan §7's "jobs by kind×outcome"
	// counts every journaled outcome, not just the ones Runner.Run reaches (validator finding:
	// superseded/cancelled outcomes were journaled but never counted).
	OnOutcome func(kind, outcome string)

	mu     sync.Mutex
	queues map[string]*issueQueue
}

// issueQueue is one issue's serial single-flight lane: at most one job of each kind waits to run
// (coalesced), and a worker goroutine drains them one at a time, in the order Dispatch delivered
// distinct kinds, blocking on new work rather than polling.
type issueQueue struct {
	mu      sync.Mutex
	pending map[Kind]*pendingJob // kind -> latest not-yet-started job of that kind
	order   []Kind               // insertion order of kinds currently pending (FIFO across kinds)
	started bool                 // a worker goroutine is currently draining this queue
}

type pendingJob struct {
	event    Event
	deadline time.Time       // when Debounce applies; zero = run as soon as picked up
	ctx      context.Context // base context this job's debounce wait and Runner.Run derive from
}

func (d *Dispatcher) queueFor(issueID string) *issueQueue {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.queues == nil {
		d.queues = make(map[string]*issueQueue)
	}
	q, ok := d.queues[issueID]
	if !ok {
		q = &issueQueue{pending: make(map[Kind]*pendingJob)}
		d.queues[issueID] = q
	}
	return q
}

// baseCtx returns the ctx a Dispatch call should root its queued job in: the caller-supplied ctx
// when non-nil, falling back to d.Ctx (the process shutdown ctx), falling back to
// context.Background() when neither is set (tests, and any caller that hasn't wired shutdown).
func (d *Dispatcher) baseCtx(ctx context.Context) context.Context {
	if ctx != nil {
		return ctx
	}
	if d.Ctx != nil {
		return d.Ctx
	}
	return context.Background()
}

// Drain blocks until every currently-running per-issue queue goroutine has finished — including
// any job it was mid-execution on, and any FOLLOW-UP still parked in its debounce wait — or until
// ctx is done (WORKER_SHUTDOWN_TIMEOUT), whichever comes first. main.go calls this after
// srv.Shutdown so SIGTERM actually waits for in-flight work up to a bounded timeout instead of
// the process exiting out from under it (finding 1: "no WaitGroup/drain exists").
func (d *Dispatcher) Drain(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		d.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (d *Dispatcher) sleep() func(context.Context, time.Duration) {
	if d.Sleep != nil {
		return d.Sleep
	}
	return func(ctx context.Context, dur time.Duration) {
		if dur <= 0 {
			return
		}
		t := time.NewTimer(dur)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
}

// Enqueue implements the Enqueuer seam (loop/poll.go) so PollLoop -- and Bootstrap -- can hand
// events straight to the Dispatcher instead of the degenerate JournalEnqueuer, which drove
// Runner.Run synchronously inline in the poll loop and let one poisoned job (e.g. a 404 on the
// precondition GET for a deleted issue, C14 -- routine, not exceptional) wedge the ENTIRE feed
// forever: PollOnce aborts the page without advancing the cursor on any Enqueue error, so the same
// event is re-fetched on every poll cycle and every later event never gets a chance to enqueue.
//
// For job kinds (Kind.IsJob(), plan §2.1's queueable triage/followup), Enqueue durably appends the
// StateQueued journal record SYNCHRONOUSLY before returning -- this is the durability guarantee
// PollLoop.PollOnce relies on to advance/persist its cursor ("advance the cursor only after the
// batch of events has been fully enqueued into the journal", plan §2.1) -- and only THEN hands the
// event to Dispatch, which queues/coalesces/runs it ASYNCHRONOUSLY on the issue's own goroutine. A
// later failure inside Runner.Run therefore surfaces long after Enqueue has already returned and
// the cursor has already moved past it: it can no longer block any other issue's queue, let alone
// the whole feed. Non-job kinds (KindSweepReconcile/KindCancelQueued/KindSkippedDeleted, and
// KindNone) go straight to Dispatch, which already handles them synchronously and cheaply (no
// queueing, no journal write here).
func (d *Dispatcher) Enqueue(e Event, kind Kind) error {
	if kind.IsJob() {
		issueID, err := e.IssueID()
		if err != nil {
			return err
		}
		if d.Journal != nil {
			jobID := state.JobID(string(kind), issueID, e.Seq)
			// Dedupe BEFORE appending the queued record (plan §2.2: "a re-delivered event whose
			// jobId has ANY terminal record is dropped"). Without this check, Enqueue's own
			// StateQueued append would overwrite the journal's latest-per-jobId index entry with a
			// non-terminal record, blinding Runner.Run's own IsTerminal() dedupe check to the fact
			// that this exact job already ran to completion — re-invoking the Advisor and Act a
			// second time for a re-delivered event (finding: redelivery re-ran an already-terminal
			// job). A duplicate is dropped silently here: it already has a terminal journal record
			// from its first delivery, so there is nothing further to journal.
			dup, dupErr := d.Journal.IsDuplicate(jobID)
			if dupErr != nil {
				return fmt.Errorf("dispatcher: checking duplicate for job %s: %w", jobID, dupErr)
			}
			if dup {
				if d.Log != nil {
					d.Log.Info("dispatcher: dropping re-delivered event, job already terminal", "jobId", jobID, "issueId", issueID, "kind", kind)
				}
				return nil
			}
			if err := d.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: string(kind), TriggerSeq: e.Seq, State: state.StateQueued}); err != nil {
				return fmt.Errorf("dispatcher: journaling queued record for job %s: %w", jobID, err)
			}
		}
	}
	d.Dispatch(d.baseCtx(nil), e, kind)
	return nil
}

// Dispatch routes one classified event into the per-issue queue layer. It never blocks on job
// execution — it only records the pending job (coalescing as needed) and wakes the issue's
// worker, starting one if this is the issue's first job.
func (d *Dispatcher) Dispatch(ctx context.Context, e Event, kind Kind) {
	issueID, err := e.IssueID()
	if err != nil {
		if d.Log != nil {
			d.Log.Error("dispatcher: dropping event with no issue id", "error", err)
		}
		return
	}

	switch kind {
	case KindNone:
		return
	case KindSweepReconcile:
		if d.OnSweepReconcile != nil {
			d.OnSweepReconcile(e)
		}
		return
	case KindCancelQueued:
		d.cancelPending(issueID, SkipResolved)
		return
	case KindSkippedDeleted:
		d.cancelPending(issueID, SkipDeleted)
		return
	}

	// Only KindTriage/KindFollowUp reach here (Kind.IsJob()) — queue + coalesce + start the
	// issue's worker if needed.
	jobCtx := d.baseCtx(ctx)
	q := d.queueFor(issueID)
	q.mu.Lock()
	deadline := time.Time{}
	if kind == KindFollowUp && d.Debounce > 0 {
		deadline = time.Now().Add(d.Debounce)
	}
	if prev, ok := q.pending[kind]; ok {
		// Coalesce: the loser is the one already queued (it never got a job appended to the
		// journal yet — only queued/running jobs are journaled — so "superseded" here means
		// journaling straight to that terminal state for the superseded trigger's own jobId,
		// per plan §2.2 "the losers get a terminal superseded record").
		//
		// EXCEPTION: a same-seq re-delivery (prev.event.Seq == e.Seq) is NOT a distinct loser —
		// it is the identical event arriving again (e.g. PollOnce re-fetching a page after an
		// Enqueue/SaveCursor error, plan §2.1) while this issue's lane is still busy with an
		// earlier job. Its jobId (state.JobID(kind, issueID, seq)) is IDENTICAL to the pending
		// job's jobId, so journaling `superseded` here would write a terminal record for the very
		// job we are about to run — Runner.Run's own terminal-dedupe (loop/runner.go) would then
		// see that jobId as already-terminal and drop it when it finally executes, permanently
		// losing the event and violating plan §2.1's "absorbed by journal dedupe" guarantee.
		// Treat it as a no-op replacement instead: keep the pending slot's kind position, just
		// refresh the stored event/deadline below.
		if prev.event.Seq != e.Seq {
			d.journalSuperseded(issueID, kind, prev.event)
		}
	} else {
		q.order = append(q.order, kind)
	}
	q.pending[kind] = &pendingJob{event: e, deadline: deadline, ctx: jobCtx}
	started := q.started
	q.started = true
	q.mu.Unlock()

	if !started {
		d.wg.Add(1)
		go d.runIssueWorker(issueID, q)
	}
}

// cancelPending drops any not-yet-started queued job for issueID and journals it `skipped` with
// reason (plan §3: status_changed->resolved / issue_deleted both "cancel queued jobs"). A job
// that is already RUNNING is not interrupted here — Runner.Run's own precondition re-check (which
// re-reads issue state) is what stops it from acting, per plan §3's "runner precondition
// (re-checked)" column.
func (d *Dispatcher) cancelPending(issueID string, reason SkipReason) {
	d.mu.Lock()
	q, ok := d.queues[issueID]
	d.mu.Unlock()
	if !ok {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for kind, job := range q.pending {
		d.journalCancelled(issueID, kind, job.event, reason)
		delete(q.pending, kind)
	}
	q.order = q.order[:0]
}

func (d *Dispatcher) journalSuperseded(issueID string, kind Kind, e Event) {
	if d.Journal == nil {
		return
	}
	jobID := state.JobID(string(kind), issueID, e.Seq)
	_ = d.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: string(kind), TriggerSeq: e.Seq, State: state.StateSuperseded})
	if d.OnOutcome != nil {
		d.OnOutcome(string(kind), "superseded")
	}
}

func (d *Dispatcher) journalCancelled(issueID string, kind Kind, e Event, reason SkipReason) {
	if d.Journal == nil {
		return
	}
	jobID := state.JobID(string(kind), issueID, e.Seq)
	payload, _ := json.Marshal(map[string]string{"reason": string(reason)})
	_ = d.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: string(kind), TriggerSeq: e.Seq, State: state.StateSkipped, Payload: payload})
	if d.OnOutcome != nil {
		d.OnOutcome(string(kind), "skipped_"+string(reason))
	}
}

// runIssueWorker is the single goroutine that serially drains one issue's pending jobs, one kind
// at a time, until there is nothing left pending — at which point it exits (queueFor starts a
// fresh one the next time Dispatch delivers work for this issue, so idle issues hold no
// goroutine). It always reports to d.wg (Add in Dispatch, Done here) so Drain can wait for it.
func (d *Dispatcher) runIssueWorker(issueID string, q *issueQueue) {
	defer d.wg.Done()
	for {
		q.mu.Lock()
		if len(q.order) == 0 {
			q.started = false
			q.mu.Unlock()
			return
		}
		kind := q.order[0]
		q.order = q.order[1:]
		job := q.pending[kind]
		delete(q.pending, kind)
		q.mu.Unlock()

		if job == nil {
			continue
		}

		if kind == KindFollowUp && !job.deadline.IsZero() {
			if wait := time.Until(job.deadline); wait > 0 {
				// job.ctx is the shutdown-aware base ctx (d.Ctx by default) -- cancelling it (SIGTERM)
				// cuts this wait short instead of blocking for the full debounce window (finding 1).
				d.sleep()(job.ctx, wait)
			}
			// A newer FOLLOW-UP may have coalesced in while we waited (C10's second event
			// landing during the debounce window) — re-check before running so we execute the
			// latest triggerSeq, not the one that started the wait.
			q.mu.Lock()
			if newer, ok := q.pending[KindFollowUp]; ok {
				delete(q.pending, KindFollowUp)
				for i, k := range q.order {
					if k == KindFollowUp {
						q.order = append(q.order[:i], q.order[i+1:]...)
						break
					}
				}
				d.journalSuperseded(issueID, KindFollowUp, job.event)
				job = newer
			}
			q.mu.Unlock()
		}

		d.runOne(job.ctx, job.event, kind)
	}
}

// runOne executes exactly one job with panic recovery: a panic inside Runner.Run is caught,
// journaled `failed`, and logged, so one broken job can never kill the dispatcher goroutine (plan
// N8a brief: "Panic recovery per job (journal failed, never kill the process)"). ctx is the job's
// own base context (job.ctx, derived from d.Ctx) -- cancelling it (SIGTERM) reaches Runner.Run
// directly instead of the disconnected context.Background() this used to hand it (finding 1).
func (d *Dispatcher) runOne(ctx context.Context, e Event, kind Kind) {
	defer func() {
		if r := recover(); r != nil {
			if d.Log != nil {
				d.Log.Error("dispatcher: recovered panic running job", "kind", kind, "panic", r)
			}
			if d.Journal != nil {
				issueID, err := e.IssueID()
				if err == nil {
					jobID := state.JobID(string(kind), issueID, e.Seq)
					_ = d.Journal.Append(state.Record{JobID: jobID, IssueID: issueID, Kind: string(kind), TriggerSeq: e.Seq, State: state.StateFailed})
					if d.OnOutcome != nil {
						d.OnOutcome(string(kind), "failed")
					}
				}
			}
		}
	}()
	if d.Runner == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := d.Runner.Run(ctx, e, kind); err != nil && d.Log != nil {
		d.Log.Error("dispatcher: job run failed", "kind", kind, "error", err)
	}
}
