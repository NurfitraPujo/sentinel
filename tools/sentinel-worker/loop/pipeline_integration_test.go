package loop

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/jobs"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// selectiveFailIssues fails GetIssue for one poisoned issue id and succeeds for everything else --
// it models a GET /api/agent/issues/:id 404 for an issue deleted between the event landing and the
// job running (C14), the routine occurrence that used to wedge the whole feed.
type selectiveFailIssues struct {
	poisonID string
}

func (s selectiveFailIssues) GetIssue(_ context.Context, issueID string) (IssueSnapshot, error) {
	if issueID == s.poisonID {
		return IssueSnapshot{}, &sentinel.StatusError{Status: 404, Body: []byte(`{"error":"not found"}`)}
	}
	return IssueSnapshot{ID: issueID, Status: "unresolved"}, nil
}

// TestDispatcherAsEnqueuer_PoisonedEventNeverWedgesTheFeed is the red-first proof for the
// head-of-line-blocking blocker: driving the REAL production path (PollLoop + Dispatcher +
// Runner + state.Journal, wired exactly as main.go wires them) with a feed page carrying one
// poisoned event (its issue 404s) followed by one healthy event for a DIFFERENT issue must
// (a) advance and persist the cursor past BOTH events after a single PollOnce, and (b) let the
// healthy event's job actually run and reach a terminal "done" record -- neither may be true if
// the poisoned job blocks synchronously in front of the healthy one.
func TestDispatcherAsEnqueuer_PoisonedEventNeverWedgesTheFeed(t *testing.T) {
	const me = "agt_me"
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))

	runner := &Runner{
		Journal:   journal,
		Issues:    selectiveFailIssues{poisonID: "iss-poison"},
		Claims:    &fakeClaimer{held: true},
		Advisor:   &countingAdvisor{},
		Act:       &countingActor{},
		DryRun:    true,
		MyAgentID: me,
	}
	dispatcher := &Dispatcher{Runner: runner, Journal: journal}

	client := &fakeEventsClient{
		pages: map[int64]EventsPage{
			0: {
				Events: []Event{
					{Seq: 1, Type: "created", Issue: &EventIssue{ID: "iss-poison"}},
					{Seq: 2, Type: "created", Issue: &EventIssue{ID: "iss-healthy"}},
				},
				HasMore: false,
				Cursor:  2,
			},
		},
	}

	cursorPath := filepath.Join(t.TempDir(), "cursor.json")
	poll := &PollLoop{
		Client:     client,
		Journal:    journal,
		CursorPath: cursorPath,
		MyAgentID:  me,
		Enqueue:    dispatcher,
		Sleep:      func(context.Context, time.Duration) {},
	}

	n, err := poll.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("PollOnce: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 events enqueued in the single page, got %d", n)
	}
	if poll.Cursor() != 2 {
		t.Fatalf("expected cursor to advance to 2 (past BOTH events) after one PollOnce, got %d -- a poisoned job must not prevent later events in the same page from being enqueued and the cursor from advancing past them", poll.Cursor())
	}
	cursor, err := state.LoadCursor(cursorPath)
	if err != nil || cursor == nil || cursor.Seq != 2 {
		t.Fatalf("expected the persisted cursor to be 2, got %+v (err=%v)", cursor, err)
	}

	// Give the async per-issue worker goroutines a chance to drain (both queues run concurrently,
	// single-flight per issue, no explicit join point exposed by Dispatcher -- polling the journal
	// with a short timeout is the seam integration tests have).
	deadline := time.Now().Add(2 * time.Second)
	var records map[string]state.Record
	for time.Now().Before(deadline) {
		var loadErr error
		records, loadErr = journal.LatestByJobID()
		if loadErr == nil && len(records) == 2 {
			allTerminal := true
			for _, r := range records {
				if !r.State.IsTerminal() {
					allTerminal = false
				}
			}
			if allTerminal {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	var sawHealthyDone, sawPoisonSkipped bool
	for jobID, rec := range records {
		switch rec.IssueID {
		case "iss-healthy":
			if rec.State == state.StateDone {
				sawHealthyDone = true
			}
		case "iss-poison":
			if rec.State == state.StateSkipped {
				sawPoisonSkipped = true
			}
		default:
			t.Errorf("unexpected job record for issue %q (jobId %s)", rec.IssueID, jobID)
		}
	}
	if !sawHealthyDone {
		t.Fatalf("expected iss-healthy's job to reach a terminal 'done' record, got: %+v", records)
	}
	if !sawPoisonSkipped {
		t.Fatalf("expected iss-poison's job to reach a terminal 'skipped(deleted)' record, got: %+v", records)
	}
}

var _ jobs.Advisor = (*countingAdvisor)(nil)

// TestDryRun_FullDispatchRunCycle_SendsZeroMutatingRequests is the required brief proof (finding
// #12) that WORKER_EXECUTE=false never sends a single mutating HTTP request across a FULL
// dispatch->run cycle (Dispatcher -> Runner -> HTTPClaimer/HTTPIssueReader against a real
// httptest server), not merely with fake seams. GET /api/agent/issues/:id (the precondition read)
// is allowed; anything else -- especially POST .../claim -- must never be observed.
func TestDryRun_FullDispatchRunCycle_SendsZeroMutatingRequests(t *testing.T) {
	var mu sync.Mutex
	var nonGET []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			nonGET = append(nonGET, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/agent/issues/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"issue":{"id":"iss-1","status":"unresolved","assigneeType":"agent","assignedTo":"agt_me"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := sentinel.NewClient(srv.URL, "test-key")
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	runner := &Runner{
		Journal:   journal,
		Issues:    HTTPIssueReader{Client: client},
		Claims:    HTTPClaimer{Client: client}, // must never be called in dry-run
		Advisor:   jobs.StubAdvisor{},
		Act:       jobs.NotImplementedActor{}, // must never be called in dry-run
		DryRun:    true,
		MyAgentID: "agt_me",
	}
	dispatcher := &Dispatcher{Runner: runner, Journal: journal}

	e := Event{Seq: 1, Type: "created", Issue: &EventIssue{ID: "iss-1"}}
	if err := dispatcher.Enqueue(e, KindTriage); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	jobID := state.JobID(string(KindTriage), "iss-1", 1)
	deadline := time.Now().Add(2 * time.Second)
	for {
		latest, err := journal.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		if rec, ok := latest[jobID]; ok && rec.State == state.StateDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for dry-run job to reach done")
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(nonGET) != 0 {
		t.Fatalf("dry-run sent %d non-GET (mutating) request(s), want 0: %v", len(nonGET), nonGET)
	}
}

// panicOnFirstAdvisor panics on the Decide call for the designated issue, then behaves like
// StubAdvisor -- proving a panic inside one job never kills the process/dispatcher and the other
// issue's job still runs (finding #12's second required test). Selection is keyed on IssueID, not
// call order: the two issues run on independent per-issue queue goroutines, so "first call" is a
// scheduling race that made this test flaky (~1/3 of runs picked the wrong victim).
type panicOnFirstAdvisor struct{ calls int32 }

func (a *panicOnFirstAdvisor) Decide(ctx context.Context, in jobs.Input) (jobs.Decision, error) {
	if in.IssueID == "issue-panic" {
		atomic.AddInt32(&a.calls, 1)
		panic("boom: advisor panic on designated job")
	}
	return jobs.StubAdvisor{}.Decide(ctx, in)
}

func TestDispatcher_PanicOnOneJob_ProcessAliveNextJobRuns(t *testing.T) {
	journal := state.OpenJournal(filepath.Join(t.TempDir(), "jobs.journal"))
	advisor := &panicOnFirstAdvisor{}
	runner := &Runner{
		Journal:   journal,
		Issues:    fakeIssues{snap: IssueSnapshot{Status: "unresolved", AssigneeType: "agent", AssignedTo: "me"}},
		Claims:    &fakeClaimer{held: true},
		Advisor:   advisor,
		Act:       &countingActor{},
		MyAgentID: "me",
	}
	dispatcher := &Dispatcher{Runner: runner, Journal: journal}

	// Two DIFFERENT issues so the second is not blocked behind the first's queue -- both should
	// reach a terminal state: the first `failed` (panic recovered), the second `done`.
	e1 := Event{Seq: 1, Type: "created", ActorID: "other", Issue: &EventIssue{ID: "issue-panic"}}
	e2 := Event{Seq: 2, Type: "created", ActorID: "other", Issue: &EventIssue{ID: "issue-ok"}}
	dispatcher.Dispatch(context.Background(), e1, KindTriage)
	dispatcher.Dispatch(context.Background(), e2, KindTriage)

	job1ID := state.JobID(string(KindTriage), "issue-panic", 1)
	job2ID := state.JobID(string(KindTriage), "issue-ok", 2)
	deadline := time.Now().Add(2 * time.Second)
	for {
		latest, err := journal.LatestByJobID()
		if err != nil {
			t.Fatal(err)
		}
		r1, ok1 := latest[job1ID]
		r2, ok2 := latest[job2ID]
		if ok1 && ok2 && r1.State.IsTerminal() && r2.State.IsTerminal() {
			if r1.State != state.StateFailed {
				t.Fatalf("expected panicking job to journal failed, got %s", r1.State)
			}
			if r2.State != state.StateDone {
				t.Fatalf("expected the OTHER issue's job to still complete normally (process alive), got %s", r2.State)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: job1=%v(%v) job2=%v(%v) -- a panic must never take down the dispatcher/process", ok1, r1.State, ok2, r2.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
