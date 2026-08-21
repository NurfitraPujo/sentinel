package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// EventsMaxLimit is the server's documented ceiling for GET /api/agent/events' `limit` query param
// (apps/dashboard-web/src/lib/server/agent-events.ts: EVENTS_MAX_LIMIT=200, vs.
// EVENTS_DEFAULT_LIMIT=50 applied when the param is omitted). httpEventsClient always requests this
// ceiling explicitly -- 4x fewer round trips than relying on the server default -- which matters
// most for Bootstrap's step 3 head-seek (loop/bootstrap.go), an O(history/limit) paging sweep with
// no cheaper "give me the head" affordance.
const EventsMaxLimit = 200

// EventsPage is the shape of a GET /api/agent/events response page.
type EventsPage struct {
	Events  []Event `json:"events"`
	HasMore bool    `json:"hasMore"`
	Cursor  int64   `json:"cursor"`
}

// EventsClient is the subset of sentinel.Client the poll loop needs, kept as an interface so unit
// tests can drive it with an httptest server or a fake without a real HTTP round trip.
type EventsClient interface {
	// GetEvents fetches one page of the events feed after the given seq (0 for "from the start" —
	// callers should not do this without bootstrapping first, per plan §2.1).
	GetEvents(ctx context.Context, after int64) (EventsPage, error)
}

// httpEventsClient adapts a *sentinel.Client to EventsClient. eventTypes/projects, when non-empty,
// are wired into every request as the query params GET /api/agent/events itself documents
// (apps/dashboard-web/src/routes/api/agent/events/+server.ts): `type=` is a comma-joined list of
// event types, `project=` is a SINGLE project id (the server reads only one value, not a list).
type httpEventsClient struct {
	c          *sentinel.Client
	eventTypes []string
	projects   []string
}

// NewEventsClient wraps a sentinel.Client for use by the poll loop. eventTypes and projects are
// WORKER_EVENT_TYPES / WORKER_PROJECTS (plan §5) -- pass nil for either to apply no filter.
func NewEventsClient(c *sentinel.Client, eventTypes, projects []string) EventsClient {
	return httpEventsClient{c: c, eventTypes: eventTypes, projects: projects}
}

func (h httpEventsClient) GetEvents(ctx context.Context, after int64) (EventsPage, error) {
	typ := ""
	if len(h.eventTypes) > 0 {
		typ = strings.Join(h.eventTypes, ",")
	}
	project := ""
	if len(h.projects) > 0 {
		// The server-side route (agent/events/+server.ts) only reads a single `project` value --
		// there is no comma-joined multi-project filter server-side. WORKER_PROJECTS is a list for
		// forward-compatibility/documentation symmetry with WORKER_EVENT_TYPES, but only the first
		// configured project is actually applied as a feed filter today.
		project = h.projects[0]
	}
	// Routed through sentinel.Client.GetEvents (not a second hand-rolled h.c.Do call) so the wire
	// shape this package actually sends is the SAME one client_test.go's request-shape goldens
	// cover. limit is always EventsMaxLimit (200): the server would otherwise apply its own
	// EVENTS_DEFAULT_LIMIT (50), quadrupling the round trips any O(history/limit) sweep pays --
	// most visibly Bootstrap's step 3 head-seek (loop/bootstrap.go).
	res, err := h.c.GetEvents(ctx, after, EventsMaxLimit, typ, project)
	if err != nil {
		return EventsPage{}, err
	}
	if res.Status < 200 || res.Status >= 300 {
		// Wrapped as *sentinel.StatusError (not a bare fmt.Errorf) so PollLoop.Run can classify the
		// failure via sentinel.ClassifyEnvelope/WaitRateLimit (plan §2.4) instead of hand-rolling a
		// fixed-interval sleep regardless of what the server said.
		return EventsPage{}, &sentinel.StatusError{Status: res.Status, Header: res.Header, Body: res.Body}
	}
	var page EventsPage
	if err := json.Unmarshal(res.Body, &page); err != nil {
		return EventsPage{}, fmt.Errorf("parsing events page: %w", err)
	}
	return page, nil
}

// Enqueuer is what the poll loop hands each drained event to. It MUST durably record the event
// (append it into the job journal) before returning nil, because the poll loop advances and
// persists its cursor only after Enqueue succeeds for every event in a page (plan §2.1: "advance
// the cursor only after the batch of events has been fully enqueued into the journal"). Returning
// an error aborts the page: the cursor is neither advanced nor persisted past it, so a crash or a
// journal-write failure is recovered by re-polling — at-least-once delivery plus the journal's
// jobId dedupe make the re-enqueue a safe no-op. *Dispatcher (loop/queue.go) is the production
// implementation; unit tests substitute a recording fake.
type Enqueuer interface {
	Enqueue(e Event, kind Kind) error
}

// The production Enqueuer is *Dispatcher (loop/queue.go): it durably journals the StateQueued
// record synchronously (the guarantee PollLoop relies on to advance its cursor) and then runs the
// job asynchronously on the issue's own per-issue serial queue, so one poisoned job can never wedge
// the feed the way a synchronous "run inline, abort the page on any error" Enqueuer would. See
// Dispatcher.Enqueue's doc comment for the full rationale.

// PollLoop is the plan §2.1/§0 "pollLoop" component: ticks WORKER_POLL_INTERVAL ±jitter, drains
// hasMore before sleeping, and advances the persisted cursor ONLY after a page's events have been
// fully enqueued into the journal — a crash between receipt and enqueue is absorbed by re-poll +
// journal dedupe (plan §2.1's "effectively-once enqueue on top of at-least-once delivery").
type PollLoop struct {
	Client     EventsClient
	Journal    *state.Journal
	CursorPath string
	MyAgentID  string
	Enqueue    Enqueuer
	Interval   time.Duration
	Jitter     float64 // fraction, e.g. 0.2 for ±20%
	Log        *slog.Logger
	Sleep      func(context.Context, time.Duration) // overridable in tests
	// OnCursorSaved, when non-nil, is called with the current time immediately after every
	// successful state.SaveCursor -- the seam health.Status.NoteCursorSaved plugs into so
	// /readyz's "cursor persisted recently" leg (plan §7) reflects reality instead of being
	// permanently true from process start. Kept as a plain func hook (not a *health.Status field)
	// so this package stays free of a health import.
	OnCursorSaved func(time.Time)
	// OnEvent, when non-nil, is called once for every event drained from the feed (after
	// Classify, before Enqueue) -- the seam health.Status.Inc(health.MetricEventsConsumed, 1)
	// plugs into so plan §7's "events consumed" counter reflects reality instead of never being
	// wired to anything. Kept as a plain func hook (not a *health.Status field) for the same
	// reason as OnCursorSaved: this package stays free of a health import.
	OnEvent func(Event, Kind)
	// OnPageDrained, when non-nil, is called once per fetched page immediately after its cursor
	// has been persisted, with the number of events that page carried and whether the feed
	// reported hasMore. health.Status.SetGauge(health.MetricCursorLag, ...) plugs into this: 0
	// once a poll cycle is fully caught up (hasMore=false), otherwise the pending page's size as a
	// backlog signal (see health.MetricCursorLag's doc for why this is an approximation, not an
	// exact head-minus-cursor count -- GET /api/agent/events never reports the feed's true head
	// seq).
	OnPageDrained func(pageEvents int, hasMore bool)
	cursor        int64

	// Breaker is the plan §2.4 "sentinel-api" circuit: 5 consecutive PollOnce failures open it,
	// gating further attempts until a half-open probe succeeds every 2m. Lazily built with
	// sentinel.ScopeSentinelAPI on first use if nil, so existing callers/tests that don't set it
	// keep working unchanged.
	Breaker *sentinel.CircuitBreaker
	// RetrySleep is the ctx-aware sleep used for backoff/rate-limit waits (distinct from Sleep,
	// which governs the steady-state poll interval) -- overridable in tests to avoid real sleeps.
	// Defaults to sentinel.SleepCtx, which returns as soon as ctx is Done instead of blocking for
	// the full backoff/Retry-After duration (validator finding: an uncancellable wait here turns a
	// graceful SIGTERM drain into a SIGKILL, up to the longest BackoffSchedule step).
	RetrySleep sentinel.CtxSleepFunc
	attempt    int
}

func (p *PollLoop) breaker() *sentinel.CircuitBreaker {
	if p.Breaker == nil {
		p.Breaker = sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
	}
	return p.Breaker
}

func (p *PollLoop) retrySleep() sentinel.CtxSleepFunc {
	if p.RetrySleep != nil {
		return p.RetrySleep
	}
	return sentinel.SleepCtx
}

// jitteredSleep is the default Sleep implementation: sleeps Interval ± Jitter, respecting ctx
// cancellation.
func (p *PollLoop) jitteredSleep(ctx context.Context, base time.Duration) {
	if base <= 0 {
		return
	}
	delta := time.Duration(float64(base) * p.Jitter * (rand.Float64()*2 - 1))
	d := base + delta
	if d < 0 {
		d = 0
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// hasOpenQuestion adapts p.Journal to Classify's hasOpenQuestion seam (plan §3's question_answered
// OR-arm). A nil Journal (some tests construct a bare PollLoop) reports no open question rather
// than panicking; a journal read error is treated the same way — this is a best-effort dispatch
// hint, not a durability-bearing read, so it fails open to "no open question" rather than blocking
// classification.
func (p *PollLoop) hasOpenQuestion(issueID string) bool {
	if p.Journal == nil {
		return false
	}
	ok, err := p.Journal.HasOpenQuestion(issueID)
	if err != nil {
		return false
	}
	return ok
}

// hasOpenFix adapts p.Journal to Classify's hasOpenFix seam (finding 3, fix-lifecycle remediation
// round 2: the plan-mandated "skip if FIX in flight per journal" rule for occurrence_burst/
// regressed). Same fail-open-to-false posture as hasOpenQuestion above, for the same reason: this
// is a best-effort dispatch hint, not a durability-bearing read, and a nil Journal/read error must
// never block classification -- worst case a duplicate FIX PR gets opened, not a stuck pipeline.
func (p *PollLoop) hasOpenFix(issueID string) bool {
	if p.Journal == nil {
		return false
	}
	ok, err := p.Journal.HasOpenKind(issueID, state.FixKind)
	if err != nil {
		return false
	}
	return ok
}

// SetCursor seeds the in-memory cursor (used after LoadCursor or a bootstrap sweep at startup).
func (p *PollLoop) SetCursor(seq int64) { p.cursor = seq }

// Cursor returns the current in-memory cursor value.
func (p *PollLoop) Cursor() int64 { return p.cursor }

// PollOnce fetches and drains ALL available pages (hasMore) once, enqueuing every event and
// advancing + persisting the cursor only after each page's events are journaled. Returns the
// number of events processed. Exposed separately from Run so unit tests can assert draining
// behavior deterministically without sleeping.
func (p *PollLoop) PollOnce(ctx context.Context) (int, error) {
	if p.Enqueue == nil {
		// A nil Enqueuer must never silently drop events: fail loudly instead of advancing the
		// cursor past events nobody durably recorded.
		return 0, fmt.Errorf("loop: PollLoop.Enqueue must not be nil")
	}
	total := 0
	for {
		page, err := p.Client.GetEvents(ctx, p.cursor)
		if err != nil {
			return total, err
		}
		pageCursor := p.cursor
		for _, e := range page.Events {
			kind := Classify(e, p.MyAgentID, p.hasOpenQuestion, p.hasOpenFix)
			if err := p.Enqueue.Enqueue(e, kind); err != nil {
				// Abort the page WITHOUT advancing or persisting the cursor: re-polling from the
				// last persisted cursor will re-deliver this event (and any already-enqueued
				// events earlier in this page), which the journal's jobId dedupe absorbs safely.
				return total, fmt.Errorf("enqueueing event seq %d: %w", e.Seq, err)
			}
			total++
			if p.OnEvent != nil {
				// Fired only once Enqueue has durably recorded the event (mirrors the "at-least-
				// once delivery, durably journaled" guarantee Enqueuer documents) so events_consumed
				// never counts an event that a later crash could still cause to be re-delivered
				// and re-counted as if it were new work.
				p.OnEvent(e, kind)
			}
			if e.Seq > pageCursor {
				pageCursor = e.Seq
			}
		}
		p.cursor = pageCursor
		if p.CursorPath != "" {
			if err := state.SaveCursor(p.CursorPath, p.cursor); err != nil {
				return total, fmt.Errorf("persisting cursor: %w", err)
			}
			if p.OnCursorSaved != nil {
				p.OnCursorSaved(time.Now())
			}
		}
		if p.OnPageDrained != nil {
			p.OnPageDrained(len(page.Events), page.HasMore)
		}
		if !page.HasMore {
			return total, nil
		}
	}
}

// Run loops PollOnce forever until ctx is cancelled. Per plan §2.4: "the poll loop never exits on
// error" (unlike the CLI's `events --follow`) — a fetch/persist error is classified through
// sentinel.ClassifyEnvelope and handled per the plan §2.4 table instead of always sleeping one
// fixed interval:
//   - ClassRateLimited (429): sleeps exactly Retry-After via sentinel.WaitRateLimit and does NOT
//     count against the circuit breaker or the backoff attempt counter (plan §2.4: "never counts
//     as a failure").
//   - Anything else that's a failure: records it against the sentinel-api CircuitBreaker and sleeps
//     sentinel.BackoffForAttempt(attempt) on the 1s->5s->30s->2m->5m ladder, with attempt
//     incrementing per consecutive failure and resetting to 0 on the next success.
//   - When the circuit is open, PollOnce is skipped entirely (Allow()==false) and the loop just
//     waits out the normal poll interval before checking again.
func (p *PollLoop) Run(ctx context.Context) {
	sleep := p.Sleep
	if sleep == nil {
		sleep = p.jitteredSleep
	}
	b := p.breaker()
	for {
		if ctx.Err() != nil {
			return
		}
		if !b.Allow() {
			if p.Log != nil {
				p.Log.Warn("poll loop: sentinel-api circuit open, skipping poll", "state", b.State().String())
			}
			sleep(ctx, p.Interval)
			continue
		}

		_, err := p.PollOnce(ctx)
		if err == nil {
			b.RecordSuccess()
			p.attempt = 0
			sleep(ctx, p.Interval)
			continue
		}

		var statusErr *sentinel.StatusError
		if errors.As(err, &statusErr) {
			// The poll loop's GET /api/agent/events is neither the claim route nor a relation
			// route, so neither disambiguating flag applies here.
			class := sentinel.ClassifyEnvelope(statusErr.Status, false, false)
			if class == sentinel.ClassRateLimited {
				if p.Log != nil {
					p.Log.Warn("poll loop: rate limited, honoring Retry-After", "error", err)
				}
				sentinel.WaitRateLimitCtx(ctx, statusErr.Header, p.retrySleep())
				// A 429 never counts as a failure (plan §2.4) -- no RecordFailure, no attempt bump.
				continue
			}
		}

		b.RecordFailure()
		p.attempt++
		if p.Log != nil {
			p.Log.Error("poll loop: fetch/persist failed, backing off", "error", err, "attempt", p.attempt, "circuit_state", b.State().String())
		}
		p.retrySleep()(ctx, sentinel.BackoffForAttempt(p.attempt))
	}
}
