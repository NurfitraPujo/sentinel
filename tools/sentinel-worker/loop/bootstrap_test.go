package loop

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// fakeIssuesLister is a scripted IssuesLister for Bootstrap tests.
type fakeIssuesLister struct {
	unresolvedUnclaimed []string
	// secondPassExtra, when non-nil, is what ListUnresolvedUnclaimed returns on its SECOND call
	// (Bootstrap's post-head-seek sweep-window delta re-list) instead of unresolvedUnclaimed again --
	// simulating an issue created while the head-seek was in flight. nil means "return
	// unresolvedUnclaimed unchanged both times" (no new issue arrived mid-sweep).
	secondPassExtra []string
	// firstSeen, when set for an id, is the IssueRef.FirstSeen returned for it. An id absent from
	// this map defaults to well outside the events-lag-guard window (an hour ago) -- safely "old"
	// so tests unconcerned with the sweep-window-delta cutoff aren't accidentally affected by it.
	firstSeen       map[string]time.Time
	claimedByMe     []string
	sinceGot        time.Time
	unresolvedCalls int
	unresolvedErr   error
	claimedErr      error
}

func (f *fakeIssuesLister) refsFor(ids []string) []IssueRef {
	refs := make([]IssueRef, len(ids))
	for i, id := range ids {
		fs, ok := f.firstSeen[id]
		if !ok {
			fs = time.Now().Add(-1 * time.Hour)
		}
		refs[i] = IssueRef{ID: id, FirstSeen: fs}
	}
	return refs
}

func (f *fakeIssuesLister) ListUnresolvedUnclaimed(_ context.Context, since time.Time) ([]IssueRef, error) {
	f.sinceGot = since
	f.unresolvedCalls++
	if f.unresolvedErr != nil {
		return nil, f.unresolvedErr
	}
	if f.unresolvedCalls >= 2 && f.secondPassExtra != nil {
		return f.refsFor(f.secondPassExtra), nil
	}
	return f.refsFor(f.unresolvedUnclaimed), nil
}

func (f *fakeIssuesLister) ListClaimedByMe(_ context.Context) ([]string, error) {
	if f.claimedErr != nil {
		return nil, f.claimedErr
	}
	return f.claimedByMe, nil
}

// TestBootstrap_BackfillsSyntheticTriageAndSeeksHeadWithoutReplaying proves plan §2.1's fresh-start
// contract: (1) a synthetic TRIAGE job per unresolved/unclaimed issue with a stable bootstrap
// jobId, (2) the held-claims view is seeded via ListClaimedByMe, (3) the events feed is drained to
// discover its head WITHOUT any of its events being enqueued as jobs.
func TestBootstrap_BackfillsSyntheticTriageAndSeeksHeadWithoutReplaying(t *testing.T) {
	lister := &fakeIssuesLister{
		unresolvedUnclaimed: []string{"iss-1", "iss-2"},
		claimedByMe:         []string{"iss-held-1"},
	}
	events := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0:  {Events: []Event{{Seq: 10, Type: "created", Issue: &EventIssue{ID: "iss-old-1"}}}, HasMore: true, Cursor: 10},
			10: {Events: []Event{{Seq: 42, Type: "commented", Issue: &EventIssue{ID: "iss-old-2"}}}, HasMore: false, Cursor: 42},
		},
	}
	enq := &recordingEnqueuer{}

	res, err := Bootstrap(context.Background(), lister, events, enq, 24, slog.Default())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if res.HeadSeq != 42 {
		t.Fatalf("expected head seq 42 (feed's current head), got %d", res.HeadSeq)
	}
	if res.BootstrapJobCount != 2 {
		t.Fatalf("expected BootstrapJobCount=2, got %d", res.BootstrapJobCount)
	}
	if len(res.HeldClaimIssueIDs) != 1 || res.HeldClaimIssueIDs[0] != "iss-held-1" {
		t.Fatalf("expected held claims [iss-held-1], got %v", res.HeldClaimIssueIDs)
	}

	// Only the 2 backfilled issues were enqueued -- NEVER the feed's events (iss-old-1/iss-old-2).
	if enq.calls != 2 {
		t.Fatalf("expected exactly 2 Enqueue calls (the backfill only, never feed replay), got %d", enq.calls)
	}
	for _, e := range enq.got {
		if e.Seq != BootstrapSeq {
			t.Errorf("expected synthetic events to carry BootstrapSeq, got %d", e.Seq)
		}
		if e.Issue == nil || (e.Issue.ID != "iss-1" && e.Issue.ID != "iss-2") {
			t.Errorf("unexpected enqueued issue %+v", e.Issue)
		}
	}

	// since must be backfillHours in the past, not zero/now-exact.
	if time.Since(lister.sinceGot) < 23*time.Hour {
		t.Errorf("expected since ~24h in the past, got %v ago", time.Since(lister.sinceGot))
	}

	// The head-seek pages the feed from after=0, following the returned cursor page by page, until
	// it exhausts HasMore -- an intentional consequence of the API exposing no head-only query
	// (documented on Bootstrap's step 3), not an accidental full replay. Assert the exact request
	// sequence so a future "optimization" that silently starts skipping pages (or accidentally
	// pages twice) is caught here rather than discovered as a correctness regression.
	wantCalls := []int64{0, 10}
	if len(events.calls) != len(wantCalls) {
		t.Fatalf("expected head-seek request sequence %v, got %v", wantCalls, events.calls)
	}
	for i, want := range wantCalls {
		if events.calls[i] != want {
			t.Errorf("head-seek call %d: after=%d, want %d (full sequence %v)", i, events.calls[i], want, events.calls)
		}
	}
}

// TestBootstrap_StableJobIdMakesRerunADedupeNoOp proves the synthetic jobId is deterministic
// (kind+issueId+BootstrapSeq), so re-running Bootstrap after a second lost state volume produces
// the identical jobId rather than a fresh one -- the journal's normal dedupe absorbs it.
func TestBootstrap_StableJobIdMakesRerunADedupeNoOp(t *testing.T) {
	lister := &fakeIssuesLister{unresolvedUnclaimed: []string{"iss-1"}}
	events := &fakeEventsClient{pages: map[int64]EventsPage{0: {HasMore: false}}}

	enq1 := &recordingEnqueuer{}
	if _, err := Bootstrap(context.Background(), lister, events, enq1, 24, nil); err != nil {
		t.Fatalf("Bootstrap run 1: %v", err)
	}
	enq2 := &recordingEnqueuer{}
	if _, err := Bootstrap(context.Background(), lister, events, enq2, 24, nil); err != nil {
		t.Fatalf("Bootstrap run 2: %v", err)
	}

	id1, err := enq1.got[0].IssueID()
	if err != nil {
		t.Fatalf("IssueID: %v", err)
	}
	id2, err := enq2.got[0].IssueID()
	if err != nil {
		t.Fatalf("IssueID: %v", err)
	}
	jobID1 := state.JobID("triage", id1, enq1.got[0].Seq)
	jobID2 := state.JobID("triage", id2, enq2.got[0].Seq)
	if jobID1 != jobID2 {
		t.Fatalf("expected identical bootstrap jobId across runs, got %q vs %q", jobID1, jobID2)
	}
}

// TestBootstrap_SweepWindowDeltaCatchesIssueCreatedDuringHeadSeek proves the fix for "bootstrap
// silently drops every issue created while the sweep runs": an issue that first-listed AFTER
// step 1's ListUnresolvedUnclaimed call (so absent from that pass) but still unresolved/unclaimed
// when the re-list runs after the head-seek is enqueued as a synthetic TRIAGE job too, not
// silently lost to a feed cursor that starts past its `created` event.
func TestBootstrap_SweepWindowDeltaCatchesIssueCreatedDuringHeadSeek(t *testing.T) {
	lister := &fakeIssuesLister{
		unresolvedUnclaimed: []string{"iss-1"},
		secondPassExtra:     []string{"iss-1", "iss-mid-sweep"}, // iss-1 still present + the new arrival
	}
	events := &fakeEventsClient{pages: map[int64]EventsPage{0: {HasMore: false}}}
	enq := &recordingEnqueuer{}

	res, err := Bootstrap(context.Background(), lister, events, enq, 24, nil)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if lister.unresolvedCalls != 2 {
		t.Fatalf("expected ListUnresolvedUnclaimed to be called twice (initial + sweep-window delta), got %d", lister.unresolvedCalls)
	}
	if res.BootstrapJobCount != 2 {
		t.Fatalf("expected BootstrapJobCount=2 (iss-1 + iss-mid-sweep), got %d", res.BootstrapJobCount)
	}
	if enq.calls != 2 {
		t.Fatalf("expected exactly 2 Enqueue calls (iss-1 once, iss-mid-sweep once — no double-enqueue of iss-1), got %d", enq.calls)
	}
	var gotMidSweep bool
	for _, e := range enq.got {
		if e.Issue != nil && e.Issue.ID == "iss-mid-sweep" {
			gotMidSweep = true
		}
		if e.Seq != BootstrapSeq {
			t.Errorf("expected synthetic events to carry BootstrapSeq, got %d", e.Seq)
		}
	}
	if !gotMidSweep {
		t.Fatalf("expected iss-mid-sweep to be enqueued via the sweep-window delta pass, got %+v", enq.got)
	}
}

// TestBootstrap_SweepWindowDeltaLeavesRecentIssuesToTheFeed is the red-first proof for the
// validator's major finding: an issue whose firstSeen falls INSIDE the events-lag-guard window
// around headCapturedAt must NOT be enqueued by the sweep-window delta pass, because it is not
// provably behind `head` yet -- its `created` event may still arrive on the feed with seq > head,
// and enqueuing it here too would mint a second, un-dedupeable jobId (kind+issueId+<realSeq> vs.
// kind+issueId+BootstrapSeq), producing a genuine double-triage once a real Advisor lands.
func TestBootstrap_SweepWindowDeltaLeavesRecentIssuesToTheFeed(t *testing.T) {
	lister := &fakeIssuesLister{
		unresolvedUnclaimed: []string{"iss-1"},
		secondPassExtra:     []string{"iss-1", "iss-just-created"},
		// iss-just-created's firstSeen is "now" -- well inside the 2s lag guard around whenever
		// Bootstrap captures headCapturedAt a moment later in this same test run.
		firstSeen: map[string]time.Time{"iss-just-created": time.Now()},
	}
	events := &fakeEventsClient{pages: map[int64]EventsPage{0: {HasMore: false}}}
	enq := &recordingEnqueuer{}

	res, err := Bootstrap(context.Background(), lister, events, enq, 24, nil)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if res.BootstrapJobCount != 1 {
		t.Fatalf("expected BootstrapJobCount=1 (iss-1 only -- iss-just-created left to the feed), got %d", res.BootstrapJobCount)
	}
	// Finding #6 (red-first): SkippedCount must count iss-just-created as a deliberate skip
	// (left to the feed), separate from BootstrapJobCount's enqueued total -- before the fix this
	// field did not exist and callers folded skipped counts into the enqueued metric.
	if res.SkippedCount != 1 {
		t.Fatalf("expected SkippedCount=1 (iss-just-created left to the feed), got %d", res.SkippedCount)
	}
	for _, e := range enq.got {
		if e.Issue != nil && e.Issue.ID == "iss-just-created" {
			t.Fatalf("iss-just-created must NOT be enqueued by the sweep-window delta pass -- it is inside the lag guard and the feed is guaranteed to deliver it, got %+v", enq.got)
		}
	}
}

// TestBootstrap_PropagatesListUnresolvedUnclaimedError proves a listing failure aborts Bootstrap
// (no partial cursor advance / no jobs silently dropped).
func TestBootstrap_PropagatesListUnresolvedUnclaimedError(t *testing.T) {
	lister := &fakeIssuesLister{unresolvedErr: errors.New("boom")}
	events := &fakeEventsClient{}
	_, err := Bootstrap(context.Background(), lister, events, &recordingEnqueuer{}, 24, nil)
	if err == nil {
		t.Fatalf("expected an error when ListUnresolvedUnclaimed fails")
	}
}

// TestBootstrap_PropagatesListClaimedByMeError proves the held-claims seed step's failure also
// aborts Bootstrap rather than silently skipping it.
func TestBootstrap_PropagatesListClaimedByMeError(t *testing.T) {
	lister := &fakeIssuesLister{claimedErr: errors.New("boom")}
	events := &fakeEventsClient{}
	_, err := Bootstrap(context.Background(), lister, events, &recordingEnqueuer{}, 24, nil)
	if err == nil {
		t.Fatalf("expected an error when ListClaimedByMe fails")
	}
}

// flakyHeadSeekEvents fails the head-seek's first N calls with a transient *sentinel.StatusError,
// then serves the real page -- it never advances `after` on a failed call the way a real feed
// wouldn't either, so a correct retry resumes cleanly.
type flakyHeadSeekEvents struct {
	failsRemaining int
	failStatus     int
	page           EventsPage
	calls          int
}

func (f *flakyHeadSeekEvents) GetEvents(_ context.Context, _ int64) (EventsPage, error) {
	f.calls++
	if f.failsRemaining > 0 {
		f.failsRemaining--
		return EventsPage{}, &sentinel.StatusError{Status: f.failStatus, Body: []byte(`{"error":"boom"}`)}
	}
	return f.page, nil
}

// TestBootstrap_HeadSeekSurvivesTransientErrorsInsteadOfAborting proves the major finding's fix:
// a transient (5xx) failure mid-seek is retried with backoff instead of aborting the ENTIRE
// Bootstrap sweep (which, on a busy org with a persistently flaky feed, made main.go's outer retry
// re-list every issue AND re-page the feed from after=0 on every restart -- a livelock risk).
func TestBootstrap_HeadSeekSurvivesTransientErrorsInsteadOfAborting(t *testing.T) {
	lister := &fakeIssuesLister{}
	events := &flakyHeadSeekEvents{
		failsRemaining: 2,
		failStatus:     503,
		page:           EventsPage{Events: []Event{{Seq: 7}}, HasMore: false, Cursor: 7},
	}
	var slept []time.Duration
	fakeSleep := func(_ context.Context, d time.Duration) { slept = append(slept, d) }

	res, err := Bootstrap(context.Background(), lister, events, &recordingEnqueuer{}, 24, nil, fakeSleep)
	if err != nil {
		t.Fatalf("Bootstrap: expected transient errors to be survived via retry, got: %v", err)
	}
	if res.HeadSeq != 7 {
		t.Fatalf("HeadSeq = %d, want 7", res.HeadSeq)
	}
	if events.calls != 3 {
		t.Fatalf("expected 3 GetEvents calls (2 failures + 1 success), got %d", events.calls)
	}
	if len(slept) != 2 {
		t.Fatalf("expected exactly 2 backoff sleeps (one per failed attempt), got %d: %v", len(slept), slept)
	}
}

// TestBootstrap_ReturnsPromptlyOnCtxCancelDuringHeadSeekBackoff proves the validator's major
// finding's fix: Bootstrap's head-seek loop now checks ctx.Err() (it checked nothing before) and
// its sleep is ctx-aware, so a cancelled ctx during a persistent-failure retry returns promptly
// instead of walking the ENTIRE backoff ladder (~7m36s) uncancellably. sleep is left unset so this
// exercises the REAL production default (sentinel.SleepCtx).
func TestBootstrap_ReturnsPromptlyOnCtxCancelDuringHeadSeekBackoff(t *testing.T) {
	lister := &fakeIssuesLister{}
	events := &flakyHeadSeekEvents{failsRemaining: 1000, failStatus: 503}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := Bootstrap(ctx, lister, events, &recordingEnqueuer{}, 24, nil)
		done <- err
	}()

	// Give Bootstrap time to fail once and enter its (real, 1s-first-step) backoff sleep.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected Bootstrap to return an error (ctx cancelled) rather than succeed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Bootstrap did not return promptly after ctx cancellation during head-seek backoff")
	}
}

// TestBootstrap_HeadSeekGivesUpAfterExhaustingRetries proves a PERSISTENTLY failing feed still
// eventually surfaces an error (so main.go's outer retry loop, not an infinite inner loop, is what
// paces the retry) rather than blocking forever.
func TestBootstrap_HeadSeekGivesUpAfterExhaustingRetries(t *testing.T) {
	lister := &fakeIssuesLister{}
	events := &flakyHeadSeekEvents{failsRemaining: 1000, failStatus: 503}
	fakeSleep := func(context.Context, time.Duration) {}

	_, err := Bootstrap(context.Background(), lister, events, &recordingEnqueuer{}, 24, nil, fakeSleep)
	if err == nil {
		t.Fatalf("expected Bootstrap to eventually give up on a persistently failing feed")
	}
}

// TestBootstrap_HeadSeekHonorsRateLimitWithoutCountingAsFailure proves a 429 sleeps Retry-After and
// is retried indefinitely without being counted against the bounded-retry budget (plan §2.4: "never
// counts as a failure").
func TestBootstrap_HeadSeekHonorsRateLimitWithoutCountingAsFailure(t *testing.T) {
	lister := &fakeIssuesLister{}
	events := &flakyHeadSeekEvents{
		failsRemaining: len(sentinel.BackoffSchedule) + 2, // would exhaust the transient budget if miscounted
		failStatus:     429,
		page:           EventsPage{Events: []Event{{Seq: 9}}, HasMore: false, Cursor: 9},
	}
	var slept []time.Duration
	fakeSleep := func(_ context.Context, d time.Duration) { slept = append(slept, d) }

	res, err := Bootstrap(context.Background(), lister, events, &recordingEnqueuer{}, 24, nil, fakeSleep)
	if err != nil {
		t.Fatalf("Bootstrap: expected repeated 429s to never exhaust retries, got: %v", err)
	}
	if res.HeadSeq != 9 {
		t.Fatalf("HeadSeq = %d, want 9", res.HeadSeq)
	}
}
