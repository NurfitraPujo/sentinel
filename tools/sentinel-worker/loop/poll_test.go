package loop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// fakeEventsClient serves fixed pages keyed by "after" cursor, letting tests simulate multi-page
// draining without HTTP.
type fakeEventsClient struct {
	pages map[int64]EventsPage
	calls []int64
}

func (f *fakeEventsClient) GetEvents(_ context.Context, after int64) (EventsPage, error) {
	f.calls = append(f.calls, after)
	p, ok := f.pages[after]
	if !ok {
		return EventsPage{}, nil
	}
	return p, nil
}

type recordingEnqueuer struct {
	got    []Event
	err    error // when set, Enqueue fails starting from the failAt'th call (1-indexed); 0 = never
	failAt int
	calls  int
}

func (r *recordingEnqueuer) Enqueue(e Event, kind Kind) error {
	r.calls++
	if r.err != nil && r.failAt != 0 && r.calls >= r.failAt {
		return r.err
	}
	r.got = append(r.got, e)
	return nil
}

// TestPollLoop_DrainsHasMoreBeforeSleeping proves plan §2.1: "the poll loop drains hasMore in a
// tight loop before sleeping" — a single PollOnce call must consume every page, not just the
// first.
func TestPollLoop_DrainsHasMoreBeforeSleeping(t *testing.T) {
	client := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0: {Events: []Event{{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}}, HasMore: true, Cursor: 1},
			1: {Events: []Event{{Seq: 2, Type: "created", Issue: &EventIssue{ID: "i2"}}}, HasMore: true, Cursor: 2},
			2: {Events: []Event{{Seq: 3, Type: "created", Issue: &EventIssue{ID: "i3"}}}, HasMore: false, Cursor: 3},
		},
	}
	enq := &recordingEnqueuer{}
	p := &PollLoop{Client: client, MyAgentID: "agent-me", Enqueue: enq}

	n, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 events drained across 3 pages in one PollOnce, got %d", n)
	}
	if len(client.calls) != 3 {
		t.Fatalf("expected 3 fetch calls (one per page), got %d: %v", len(client.calls), client.calls)
	}
	if p.Cursor() != 3 {
		t.Fatalf("expected cursor advanced to 3, got %d", p.Cursor())
	}
}

// flakyThenOKEventsClient fails the first failCount calls with a 500 StatusError, then serves a
// single empty page (hasMore: false) forever after — used to prove Run's "never exits on error"
// rule (plan §2.1/§2.4) survives a streak of consecutive 5xx failures.
type flakyThenOKEventsClient struct {
	failCount int
	calls     int
}

func (f *flakyThenOKEventsClient) GetEvents(_ context.Context, _ int64) (EventsPage, error) {
	f.calls++
	if f.calls <= f.failCount {
		return EventsPage{}, &sentinel.StatusError{Status: http.StatusInternalServerError, Body: []byte(`{"error":"boom"}`)}
	}
	return EventsPage{Events: nil, HasMore: false, Cursor: 0}, nil
}

// alwaysFailEventsClient always returns a transient 500, used to keep PollLoop.Run parked in its
// retry-backoff sleep so TestPollLoop_RunReturnsPromptlyOnCtxCancelDuringRetryBackoff can prove
// the wait is interruptible.
type alwaysFailEventsClient struct{}

func (alwaysFailEventsClient) GetEvents(_ context.Context, _ int64) (EventsPage, error) {
	return EventsPage{}, &sentinel.StatusError{Status: http.StatusInternalServerError, Body: []byte(`{"error":"boom"}`)}
}

// TestPollLoop_RunReturnsPromptlyOnCtxCancelDuringRetryBackoff proves the validator's major
// finding's fix: a ctx cancellation (e.g. SIGTERM) DURING a retry/backoff wait must make Run
// return quickly, not block for up to the longest step on sentinel.BackoffSchedule (5m) the way an
// uncancellable time.Sleep-based RetrySleep used to. RetrySleep is left unset here so Run uses the
// REAL production default (sentinel.SleepCtx), proving the default itself is ctx-aware -- not just
// a test double.
func TestPollLoop_RunReturnsPromptlyOnCtxCancelDuringRetryBackoff(t *testing.T) {
	p := &PollLoop{
		Client:    alwaysFailEventsClient{},
		MyAgentID: "agent-me",
		Enqueue:   &recordingEnqueuer{},
		Interval:  time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	// Give Run time to fail once and enter its (real, 1s-first-step) backoff sleep.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancellation during retry backoff -- RetrySleep default is not ctx-aware")
	}
}

// TestPollLoop_Run_SurvivesConsecutive5xxStreakWithoutExiting proves plan §2.4: the poll loop
// NEVER exits on error (unlike the CLI's `events --follow`) -- a streak of consecutive 5xx
// responses is backed off via sentinel.BackoffForAttempt, not treated as fatal, and Run keeps
// calling PollOnce until it eventually succeeds.
func TestPollLoop_Run_SurvivesConsecutive5xxStreakWithoutExiting(t *testing.T) {
	// failCount is deliberately BELOW the 5-consecutive-failure circuit-open threshold
	// (sentinel.CircuitBreaker) so this test asserts Run's own "never exits on error" backoff
	// behavior, not the circuit breaker's much longer (2m, real-time) half-open recovery -- that
	// is sentinel/retry_test.go's job.
	client := &flakyThenOKEventsClient{failCount: 4}
	enq := &recordingEnqueuer{}

	var mu sync.Mutex
	successes := 0
	p := &PollLoop{
		Client:    client,
		MyAgentID: "agent-me",
		Enqueue:   enq,
		Interval:  time.Millisecond,
		Jitter:    0,
		// Sleep/RetrySleep are both no-ops (context-respecting) so the test runs fast regardless
		// of the backoff ladder's real durations -- this test asserts survival/eventual success,
		// not the ladder's timing (that's sentinel/retry_test.go's job).
		Sleep: func(ctx context.Context, _ time.Duration) {
			select {
			case <-ctx.Done():
			default:
			}
		},
		RetrySleep: func(context.Context, time.Duration) {},
		OnPageDrained: func(_ int, _ bool) {
			mu.Lock()
			successes++
			mu.Unlock()
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		s := successes
		mu.Unlock()
		if s > 0 {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("Run gave up (or never recovered) after a 5xx streak of %d; expected it to keep retrying and eventually succeed", client.failCount)
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return promptly after ctx cancellation")
	}

	if client.calls <= client.failCount {
		t.Fatalf("expected Run to call GetEvents past the failure streak (>%d), got %d calls", client.failCount, client.calls)
	}
}

// TestPollLoop_PersistsCursorAfterEachPage proves the cursor file reflects progress after
// PollOnce completes (plan §2.1: advance only after events are enqueued).
func TestPollLoop_PersistsCursorAfterEachPage(t *testing.T) {
	client := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0: {Events: []Event{{Seq: 5, Type: "created", Issue: &EventIssue{ID: "i1"}}}, HasMore: false},
		},
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	p := &PollLoop{Client: client, MyAgentID: "agent-me", Enqueue: &recordingEnqueuer{}, CursorPath: cursorPath}

	if _, err := p.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	c, err := state.LoadCursor(cursorPath)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if c == nil || c.Seq != 5 {
		t.Fatalf("expected persisted cursor seq 5, got %+v", c)
	}
}

// TestPollLoop_EmptyPageIsANoOp proves an empty (no new events) page doesn't error and doesn't
// move the cursor.
func TestPollLoop_EmptyPageIsANoOp(t *testing.T) {
	client := &fakeEventsClient{pages: map[int64]EventsPage{0: {Events: nil, HasMore: false}}}
	p := &PollLoop{Client: client, MyAgentID: "agent-me", Enqueue: &recordingEnqueuer{}}
	n, err := p.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if n != 0 || p.Cursor() != 0 {
		t.Fatalf("expected no events and cursor 0, got n=%d cursor=%d", n, p.Cursor())
	}
}

// TestPollLoop_EnqueuesWithClassifiedKind proves each drained event is handed to the Enqueuer
// paired with its dispatcher classification (echo suppression included).
func TestPollLoop_EnqueuesWithClassifiedKind(t *testing.T) {
	client := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0: {
				Events: []Event{
					{Seq: 1, Type: "created", ActorID: "user-1", Issue: &EventIssue{ID: "i1"}},
					{Seq: 2, Type: "created", ActorID: "agent-me", Issue: &EventIssue{ID: "i2"}}, // echo, must suppress
				},
				HasMore: false,
			},
		},
	}
	var kinds []Kind
	enq := enqueueFunc(func(e Event, k Kind) { kinds = append(kinds, k) })
	p := &PollLoop{Client: client, MyAgentID: "agent-me", Enqueue: enq}

	if _, err := p.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(kinds) != 2 {
		t.Fatalf("expected 2 enqueue calls, got %d", len(kinds))
	}
	if kinds[0] != KindTriage {
		t.Errorf("event 1: got %q, want KindTriage", kinds[0])
	}
	if kinds[1] != KindNone {
		t.Errorf("event 2 (echo): got %q, want KindNone", kinds[1])
	}
}

type enqueueFunc func(e Event, k Kind)

func (f enqueueFunc) Enqueue(e Event, k Kind) error { f(e, k); return nil }

// TestPollLoop_NilEnqueueIsAConstructionErrorNotASilentDrop proves a nil Enqueuer fails loudly
// instead of silently dropping every event while still advancing the cursor.
func TestPollLoop_NilEnqueueIsAConstructionErrorNotASilentDrop(t *testing.T) {
	client := &fakeEventsClient{
		pages: map[int64]EventsPage{0: {Events: []Event{{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}}, HasMore: false}},
	}
	p := &PollLoop{Client: client, MyAgentID: "agent-me"} // Enqueue left nil
	n, err := p.PollOnce(context.Background())
	if err == nil {
		t.Fatalf("expected an error for a nil Enqueuer, got nil (n=%d)", n)
	}
	if p.Cursor() != 0 {
		t.Fatalf("a nil Enqueuer must not advance the cursor, got %d", p.Cursor())
	}
}

// TestPollLoop_EnqueueFailureAbortsPageWithoutAdvancingCursor proves plan §2.1's durability rule:
// when Enqueue fails partway through a page, the cursor must not advance or persist past events
// that were not durably enqueued — re-polling must re-deliver them.
func TestPollLoop_EnqueueFailureAbortsPageWithoutAdvancingCursor(t *testing.T) {
	client := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0: {Events: []Event{
				{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}},
				{Seq: 2, Type: "created", Issue: &EventIssue{ID: "i2"}},
			}, HasMore: false},
		},
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	enq := &recordingEnqueuer{err: fmt.Errorf("journal write failed"), failAt: 2}
	p := &PollLoop{Client: client, MyAgentID: "agent-me", Enqueue: enq, CursorPath: cursorPath}

	n, err := p.PollOnce(context.Background())
	if err == nil {
		t.Fatalf("expected PollOnce to surface the enqueue failure")
	}
	if n != 1 {
		t.Fatalf("expected 1 successfully-enqueued event before the failure, got %d", n)
	}
	if p.Cursor() != 0 {
		t.Fatalf("cursor must not advance past an enqueue failure, got %d", p.Cursor())
	}
	if c, _ := state.LoadCursor(cursorPath); c != nil {
		t.Fatalf("cursor must not be persisted when the page failed, got %+v", c)
	}
}

// TestPollLoop_OnCursorSavedCalledAfterPersist proves the health.Status.NoteCursorSaved seam
// (plan §7's "cursor persisted recently" leg of /readyz) actually fires after every successful
// persist -- the hook existed on PollLoop but PollOnce never called it, so /readyz's freshness
// check could never observe a real poll loop's progress.
func TestPollLoop_OnCursorSavedCalledAfterPersist(t *testing.T) {
	client := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0: {Events: []Event{{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}}, HasMore: false},
		},
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	var calls int
	var lastAt time.Time
	p := &PollLoop{
		Client: client, MyAgentID: "agent-me", Enqueue: &recordingEnqueuer{}, CursorPath: cursorPath,
		OnCursorSaved: func(at time.Time) { calls++; lastAt = at },
	}
	if _, err := p.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected OnCursorSaved called exactly once after a successful persist, got %d", calls)
	}
	if lastAt.IsZero() || time.Since(lastAt) > time.Second {
		t.Fatalf("expected OnCursorSaved called with a recent timestamp, got %v", lastAt)
	}
}

// TestPollLoop_OnCursorSavedNotCalledOnEnqueueFailure proves the hook only fires when a cursor was
// actually persisted -- an aborted page (enqueue failure) must not report a fresh save.
func TestPollLoop_OnCursorSavedNotCalledOnEnqueueFailure(t *testing.T) {
	client := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0: {Events: []Event{{Seq: 1, Type: "created", Issue: &EventIssue{ID: "i1"}}}, HasMore: false},
		},
	}
	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	var calls int
	enq := &recordingEnqueuer{err: fmt.Errorf("boom"), failAt: 1}
	p := &PollLoop{
		Client: client, MyAgentID: "agent-me", Enqueue: enq, CursorPath: cursorPath,
		OnCursorSaved: func(time.Time) { calls++ },
	}
	if _, err := p.PollOnce(context.Background()); err == nil {
		t.Fatalf("expected PollOnce to fail")
	}
	if calls != 0 {
		t.Fatalf("expected OnCursorSaved NOT called when the page's enqueue failed, got %d calls", calls)
	}
}

// TestHTTPEventsClient_WiresEventTypesAndProjectsIntoQuery proves WORKER_EVENT_TYPES /
// WORKER_PROJECTS actually reach the wire as `type=` (comma-joined) and `project=` (first value —
// the server route only supports one), matching apps/dashboard-web's
// /api/agent/events/+server.ts contract.
func TestHTTPEventsClient_WiresEventTypesAndProjectsIntoQuery(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"events":[],"hasMore":false,"cursor":0}`))
	}))
	defer srv.Close()

	client := sentinel.NewClient(srv.URL, "test-key")
	ec := NewEventsClient(client, []string{"created", "commented"}, []string{"proj-a", "proj-b"})
	if _, err := ec.GetEvents(context.Background(), 5); err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	q := gotQuery
	if !containsParam(q, "type=created%2Ccommented") {
		t.Fatalf("expected type=created,commented (URL-encoded) in query, got %q", q)
	}
	if !containsParam(q, "project=proj-a") {
		t.Fatalf("expected project=proj-a (first configured project only) in query, got %q", q)
	}
	if containsParam(q, "proj-b") {
		t.Fatalf("did not expect the second project in the query (server supports only one), got %q", q)
	}
	if !containsParam(q, "after=5") {
		t.Fatalf("expected after=5 in query, got %q", q)
	}
}

// TestHTTPEventsClient_NoFiltersOmitsParams proves nil eventTypes/projects don't add empty
// type=/project= params (which would 400 the server route).
func TestHTTPEventsClient_NoFiltersOmitsParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"events":[],"hasMore":false,"cursor":0}`))
	}))
	defer srv.Close()

	client := sentinel.NewClient(srv.URL, "test-key")
	ec := NewEventsClient(client, nil, nil)
	if _, err := ec.GetEvents(context.Background(), 0); err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if containsParam(gotQuery, "type=") || containsParam(gotQuery, "project=") {
		t.Fatalf("expected no type=/project= params with nil filters, got %q", gotQuery)
	}
}

func containsParam(query, substr string) bool {
	for _, part := range splitAmp(query) {
		if part == substr {
			return true
		}
	}
	return false
}

func splitAmp(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '&' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// TestPollLoop_Run_SharedBreakerOpensOnRepeatedFailure is finding 5's fast, backoff-independent
// proof (durability-startup remediation): main.go constructs ONE sentinelAPIBreaker and must pass
// it into both loop.Runner (whose activity publishCircuitStateGauge reads) and
// loop.PollLoop.Breaker -- before the fix, PollLoop.Breaker was left nil and poll.go's breaker()
// helper lazily built a SEPARATE CircuitBreaker instance, so a poll-only outage never showed up on
// the shared /metrics gauge. This drives Run() with a real externally-owned CircuitBreaker (the
// same object a caller like main.go would hand to both PollLoop and the gauge publisher) through 5
// real consecutive failures and asserts THAT SAME instance opened. Sleep/RetrySleep are no-ops so
// the test doesn't pay the real backoff ladder's cost -- this is loop.PollLoop's own mechanism
// under test, not sentinel.BackoffForAttempt's timing. Passing a fresh, un-shared breaker to
// PollLoop.Breaker (i.e. reverting to "nil => own breaker") would still pass THIS test in
// isolation, which is exactly why finding 5's real defect (main.go never wiring the shared
// instance in) is additionally proven by inspection of runPipeline's poller construction, not by a
// test in this package alone -- see the `Breaker: sentinelAPIBreaker` line in main.go.
func TestPollLoop_Run_SharedBreakerOpensOnRepeatedFailure(t *testing.T) {
	shared := sentinel.NewCircuitBreaker(sentinel.ScopeSentinelAPI)
	if shared.State() != sentinel.CircuitClosed {
		t.Fatalf("fresh breaker state = %v, want Closed", shared.State())
	}

	p := &PollLoop{
		Client:     alwaysFailEventsClient{},
		MyAgentID:  "agent-me",
		Enqueue:    &recordingEnqueuer{},
		Interval:   time.Millisecond,
		Breaker:    shared,
		Sleep:      func(ctx context.Context, _ time.Duration) {},
		RetrySleep: func(context.Context, time.Duration) {},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for shared.State() == sentinel.CircuitClosed {
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("shared breaker never opened after repeated poll failures -- PollLoop is not driving the caller-supplied Breaker")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done

	if shared.State() != sentinel.CircuitOpen {
		t.Fatalf("shared breaker state = %v, want Open", shared.State())
	}
}

// TestPollLoop_HasOpenFixSuppressesOccurrenceBurst is the wired end-to-end proof for finding 3
// (fix-lifecycle remediation round 2): PollLoop.PollOnce, against a REAL *state.Journal carrying a
// non-terminal Kind=state.FixKind record for an issue (exactly what jobs.journalFixRunning/
// JournalFixPROpen append), must classify that issue's occurrence_burst event as KindNone -- not
// merely Classify in isolation (dispatch_test.go covers that), but PollLoop's own hasOpenFix hook
// actually consulting the journal via p.hasOpenFix -> Journal.HasOpenKind.
//
// MUTATION-TEST NOTE: drop `p.hasOpenFix` from PollOnce's `Classify(e, p.MyAgentID,
// p.hasOpenQuestion, p.hasOpenFix)` call (its pre-fix shape) and this test goes red -- the
// occurrence_burst event classifies KindTriage despite the open FIX record.
func TestPollLoop_HasOpenFixSuppressesOccurrenceBurst(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	if err := journal.Append(state.Record{
		JobID:   "fix-job-1",
		IssueID: "issue-with-open-fix",
		Kind:    state.FixKind,
		State:   state.StateActed, // JournalFixPROpen's own State -- non-terminal, PR out for review
	}); err != nil {
		t.Fatalf("journal.Append: %v", err)
	}

	client := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0: {
				Events: []Event{
					{Seq: 1, Type: "occurrence_burst", ActorID: "someone-else", Issue: &EventIssue{ID: "issue-with-open-fix"}},
					{Seq: 2, Type: "occurrence_burst", ActorID: "someone-else", Issue: &EventIssue{ID: "issue-no-fix"}},
				},
				HasMore: false,
			},
		},
	}
	var kinds []Kind
	enq := enqueueFunc(func(e Event, k Kind) { kinds = append(kinds, k) })
	p := &PollLoop{Client: client, MyAgentID: "agent-me", Enqueue: enq, Journal: journal}

	if _, err := p.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if len(kinds) != 2 {
		t.Fatalf("expected 2 enqueue calls, got %d", len(kinds))
	}
	if kinds[0] != KindNone {
		t.Errorf("issue with an open FIX: got %q, want KindNone", kinds[0])
	}
	if kinds[1] != KindTriage {
		t.Errorf("issue with no open FIX: got %q, want KindTriage", kinds[1])
	}
}
