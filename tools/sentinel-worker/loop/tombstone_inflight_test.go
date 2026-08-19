// tombstone_inflight_test.go proves plan §9 N8e's tombstone hardening: "A tombstone for an issue
// with an in-flight job must cancel it" -- an issue_deleted event landing while a TRIAGE/FOLLOW-UP
// job is already running (not merely queued) must cancel that job's context and the dispatcher
// must journal it skipped(deleted), not leave it stranded or journal a misleading failed.
package loop

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// TestDispatcher_TombstoneCancelsInFlightJob proves the in-flight cancellation path end to end:
// dispatch a TRIAGE job whose Advisor blocks (simulating real work in flight), confirm it started,
// then dispatch KindSkippedDeleted for the SAME issue and prove (a) the advisor's ctx is
// cancelled promptly and (b) the job's own jobId lands on a terminal skipped(deleted) journal
// record -- not failed, and not left stranded non-terminal.
func TestDispatcher_TombstoneCancelsInFlightJob(t *testing.T) {
	adv := &ctxAwareAdvisor{release: make(chan struct{}), started: make(chan struct{})}
	d, j := newShutdownTestDispatcher(t, adv)
	ctx := context.Background()
	d.Ctx = ctx

	triggerEvent := ev(1, "issue-1")
	d.Dispatch(ctx, triggerEvent, KindTriage)

	select {
	case <-adv.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the TRIAGE job to start")
	}

	// The tombstone: issue_deleted for the SAME issue while the job above is still in flight
	// (blocked in ctxAwareAdvisor.Decide until released or cancelled).
	d.Dispatch(ctx, ev(2, "issue-1"), KindSkippedDeleted)

	// The advisor's ctx must be cancelled promptly by the tombstone (not by the never-closed
	// release channel, and not by d.Ctx, which is still context.Background() here).
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&adv.sawCancelled) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("in-flight job's context was never cancelled by the tombstone")
		}
		time.Sleep(time.Millisecond)
	}

	// Drain so runOne has fully returned and journaled its terminal record before we assert on it.
	drainCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Drain(drainCtx)

	jobID := state.JobID(string(KindTriage), "issue-1", triggerEvent.Seq)
	records, _, err := j.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var last *state.Record
	for i := range records {
		if records[i].JobID == jobID {
			rec := records[i]
			last = &rec
		}
	}
	if last == nil {
		t.Fatalf("expected a journal record for job %s", jobID)
	}
	if last.State != state.StateSkipped {
		t.Fatalf("expected the tombstoned in-flight job to land on skipped (not %s) -- a stray failed or a stranded non-terminal record both violate plan §9 N8e's tombstone hardening", last.State)
	}
	if !strings.Contains(string(last.Payload), string(SkipDeleted)) {
		t.Fatalf("skip payload = %s, want reason=%s", last.Payload, SkipDeleted)
	}
}
