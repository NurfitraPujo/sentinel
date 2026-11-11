package e2e

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/nats-io/nats.go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This file covers matrix row U36 (docs/plans/IDEMPOTENCY_PLAN.md, P9-3): event_id-keyed idempotency on
// the processor's write path, proven against the deployed stack over three legs plus the batch echo
// contract (F-VW0-3):
//
//   - Leg 1: a client-retry-shaped whole-body re-POST (the SDK's actual duplication window, per D-a's
//     table) dedups to one stored occurrence, and the 202 body echoes the effective event_id both times.
//   - Leg 2: literal NATS redelivery (the P9-3 defect's own window — a NAK/backoff or DLQ replay
//     re-publishing the identical bytes) dedups the same way via a direct publish to ERROR_EVENTS.
//   - Leg 3: a THIRD, distinct event on the same issue still counts — dedup is scoped to (issue_id,
//     event_id), not to the issue, so this rules out a "gate everything after the first" implementation
//     that would pass legs 1-2 for the wrong reason.
//
// THE WAIT CONDITION IS THE POINT (IDEMPOTENCY_PLAN.md §4 W3 / F-TP-3): a duplicate's DB non-effect is
// indistinguishable from "hasn't been processed yet" without a positive signal that processing actually
// happened. waitForOccurrences alone is satisfied by the FIRST delivery — it would pass identically
// whether or not the second delivery had been handled at all. Every leg below polls
// sentinel_process_events_total{outcome="duplicate"} (via processOutcomeCounts, tracing_test.go) for a
// duplicate delta BEFORE trusting any DB assertion.

// ---------------------------------------------------------------------------------------------------
// U36 — event idempotency
// ---------------------------------------------------------------------------------------------------

func TestU36_EventIdempotency(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	// -----------------------------------------------------------------------------------------------
	// Leg 1 — client-retry over HTTP: the same event_id, POSTed twice.
	// -----------------------------------------------------------------------------------------------

	id1 := "e2e-u36-" + uniqueSuffix()
	ev1 := f.newEvent().with(map[string]any{"event_id": id1})

	// before1 is taken BEFORE either POST, not between them: the first POST's 202 only proves the
	// ingestor accepted it, not that the processor has already recorded its outcome metric (that is
	// exactly the async race the wait condition below exists to close). Measuring deltas across the
	// WHOLE two-POST sequence from one snapshot avoids racing that metric.
	before1 := processOutcomeCounts(t)

	res1a := f.ingest(ev1)
	if res1a.Status != http.StatusAccepted {
		t.Fatalf("leg 1 first POST: got %d, want 202 (body: %s)", res1a.Status, res1a.Body)
	}
	body1a := res1a.decodeAccepted(t)
	if body1a.EventID != id1 {
		t.Errorf("leg 1 first POST: 202 body event_id = %q, want the client literal %q (F-VW0-3's echo "+
			"contract — a client-supplied id under the 64-char/no-control-char bounds must be used verbatim)",
			body1a.EventID, id1)
	}

	res1b := f.ingest(ev1)
	if res1b.Status != http.StatusAccepted {
		t.Fatalf("leg 1 second (duplicate) POST: got %d, want 202 (body: %s)", res1b.Status, res1b.Body)
	}
	body1b := res1b.decodeAccepted(t)
	if body1b.EventID != id1 {
		t.Errorf("leg 1 second POST: 202 body event_id = %q, want %q — a deterministic-minting ingestor "+
			"that broke D-a's retry-safety contract would still echo SOMETHING here, so this is asserted "+
			"against the exact client literal, not merely \"non-empty\"", body1b.EventID, id1)
	}

	waitFor(t, asyncTimeout, "leg 1: duplicate recorded on sentinel_process_events_total", func() (bool, string) {
		now := processOutcomeCounts(t)
		delta := now["duplicate"] - before1["duplicate"]
		return delta >= 1, fmt.Sprintf("duplicate delta %v (now=%v, before=%v)", delta, now["duplicate"], before1["duplicate"])
	})

	// ONLY NOW: the duplicate has actually been processed, so a DB read means something.
	if got := f.occurrenceCount(); got != 1 {
		t.Fatalf("leg 1: occurrenceCount() = %d, want 1 — the duplicate must not create a second row", got)
	}
	issue1 := f.onlyIssue()
	if issue1.Count != 1 {
		t.Errorf("leg 1: onlyIssue().Count = %d, want 1 — a duplicate must not bump issues.count (S18)", issue1.Count)
	}
	occs1 := f.occurrences()
	if len(occs1) != 1 {
		t.Fatalf("leg 1: len(occurrences()) = %d, want 1", len(occs1))
	}
	if occs1[0].EventID == nil || *occs1[0].EventID != id1 {
		t.Errorf("leg 1: stored occurrence.event_id = %v, want %q — the stored value must be the id that "+
			"actually deduped, not merely present", occs1[0].EventID, id1)
	}

	after1 := processOutcomeCounts(t)
	// Latent-flake caveat, accepted (F-VW3-5): this counter is process-global, so residual async
	// traffic from a PRECEDING e2e test could in principle land inside this window and break the
	// exact ==1. The suite runs sequentially (no t.Parallel) and every earlier test waits out its own
	// deliveries, so it has not been observed — and the double-count defect this guards is also
	// caught fixture-free by integration (g)'s delta invariant. If this line ever flakes in CI,
	// demote it to a diagnostic and let (g) carry the proof; do not widen it to >=.
	if delta := after1["stored"] - before1["stored"]; delta != 1 {
		t.Errorf("leg 1: stored delta = %v, want exactly 1 across the two POSTs (one store + one "+
			"duplicate) — catches the double-count implementation D-e forbids", delta)
	}

	// -----------------------------------------------------------------------------------------------
	// Leg 2 — literal redelivery via NATS: the identical proto bytes, published twice.
	// -----------------------------------------------------------------------------------------------

	natsErrorClass := "E2EU36NatsRedeliveryError"
	natsID := "e2e-u36-nats-" + uniqueSuffix()

	metadata, err := structpb.NewStruct(map[string]any{})
	if err != nil {
		t.Fatalf("building metadata struct: %v", err)
	}
	evt := &sentinelv1.ErrorEvent{
		// ProjectId is SET deliberately: production stamps it (the ingestor resolves it from the
		// authenticated API key before publish). Omitting it here would exercise the legacy
		// GetProjectByKey fallback path production no longer takes for real traffic.
		ProjectId:   f.ProjectID,
		ProjectKey:  f.ProjectName,
		Platform:    "go",
		Environment: "e2e",
		Message:     "e2e u36 literal NATS redelivery",
		ErrorClass:  natsErrorClass,
		TraceId:     "trace-" + uniqueSuffix(),
		SpanId:      "span-" + uniqueSuffix(),
		Stacktrace: []*sentinelv1.StackFrame{
			{File: "main.go", Line: 1, Function: "main", InApp: true},
		},
		Metadata:  metadata,
		Timestamp: timestamppb.Now(),
		EventId:   natsID,
	}
	raw, err := proto.Marshal(evt)
	if err != nil {
		t.Fatalf("marshalling the NATS redelivery proto event: %v", err)
	}

	nc, err := nats.Connect(cfg.NATSURL, nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to NATS at %s: %v", cfg.NATSURL, err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("obtaining a JetStream context: %v", err)
	}

	before2 := processOutcomeCounts(t)

	ack1, err := js.Publish("error_events", raw)
	if err != nil {
		t.Fatalf("leg 2: first publish: %v", err)
	}
	if ack1.Stream != "ERROR_EVENTS" {
		t.Errorf("leg 2: first publish ack.Stream = %q, want \"ERROR_EVENTS\" — this test believes it is "+
			"publishing straight onto the stream the processor actually consumes", ack1.Stream)
	}

	ack2, err := js.Publish("error_events", raw)
	if err != nil {
		t.Fatalf("leg 2: second (redelivery) publish: %v", err)
	}
	if ack2.Stream != "ERROR_EVENTS" {
		t.Errorf("leg 2: second publish ack.Stream = %q, want \"ERROR_EVENTS\"", ack2.Stream)
	}

	waitFor(t, asyncTimeout, "leg 2: duplicate recorded on sentinel_process_events_total", func() (bool, string) {
		now := processOutcomeCounts(t)
		delta := now["duplicate"] - before2["duplicate"]
		return delta >= 1, fmt.Sprintf("duplicate delta %v (now=%v, before=%v)", delta, now["duplicate"], before2["duplicate"])
	})

	after2 := processOutcomeCounts(t)
	if delta := after2["stored"] - before2["stored"]; delta != 1 {
		t.Errorf("leg 2: stored delta = %v, want exactly 1 — the literal redelivery must record stored "+
			"exactly once, not once per publish", delta)
	}

	var natsIssue *issueRow
	for _, iss := range f.issues() {
		if iss.ErrorClass == natsErrorClass {
			ic := iss
			natsIssue = &ic
			break
		}
	}
	if natsIssue == nil {
		t.Fatalf("leg 2: no issue found with error_class %q", natsErrorClass)
	}
	if natsIssue.Count != 1 {
		t.Errorf("leg 2: issue.Count = %d, want 1 — the literal redelivery must not double the counter", natsIssue.Count)
	}
	var natsOccCount int
	for _, o := range f.occurrences() {
		if o.IssueID == natsIssue.ID {
			natsOccCount++
		}
	}
	if natsOccCount != 1 {
		t.Errorf("leg 2: found %d occurrences for the NATS-redelivered issue, want exactly 1", natsOccCount)
	}

	// -----------------------------------------------------------------------------------------------
	// Leg 3 — a third, DISTINCT event on leg 1's issue must still count.
	// -----------------------------------------------------------------------------------------------

	id3 := "e2e-u36-3-" + uniqueSuffix()
	ev3 := f.newEvent().with(map[string]any{"event_id": id3})
	res3 := f.ingest(ev3)
	if res3.Status != http.StatusAccepted {
		t.Fatalf("leg 3 POST: got %d, want 202 (body: %s)", res3.Status, res3.Body)
	}
	body3 := res3.decodeAccepted(t)
	if body3.EventID != id3 {
		t.Errorf("leg 3: 202 body event_id = %q, want %q", body3.EventID, id3)
	}

	waitFor(t, asyncTimeout, "leg 3: a distinct event_id bumps the SAME issue to count=2", func() (bool, string) {
		for _, iss := range f.issues() {
			if iss.ID == issue1.ID {
				return iss.Count == 2, fmt.Sprintf("issue1.Count = %d", iss.Count)
			}
		}
		return false, "leg 1's issue is no longer present"
	})

	if got := f.occurrenceCount(); got != 3 {
		t.Errorf("after all 3 legs: occurrenceCount() = %d, want 3 (leg1 + leg2 + leg3, each stored once)", got)
	}
}

// ---------------------------------------------------------------------------------------------------
// F-VW0-3 — batch echo contract: event_ids has one entry per SUCCESSFUL item, correctly indexed
// ---------------------------------------------------------------------------------------------------

// TestU36_BatchEchoesEventIDsForSuccessfulItemsOnly asserts that a partially-failing batch's
// `event_ids` array names only the items that actually ingested, by their ORIGINAL index — not a
// compacted 0..N-1 sequence, which would misattribute an id to the wrong item the moment anything before
// it fails.
func TestU36_BatchEchoesEventIDsForSuccessfulItemsOnly(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	id0 := "e2e-u36-batch-0-" + uniqueSuffix()
	id2 := "e2e-u36-batch-2-" + uniqueSuffix()

	valid0 := f.newEvent().with(map[string]any{"event_id": id0})
	// Empty error_class fails the ingestor's live validation path (protovalidate's
	// (buf.validate.field).required on error_event.error_class) exactly as a missing key would — the
	// wire-level JSON distinction between "absent" and "present but empty" collapses to the same
	// server-side outcome here, and using an explicit override keeps this test's use of .with() uniform
	// with the rest of the harness.
	invalid1 := f.newEvent().with(map[string]any{"event_id": "e2e-u36-batch-1-" + uniqueSuffix(), "error_class": ""})
	valid2 := f.newEvent().with(map[string]any{"event_id": id2})

	res := f.ingestBatch([]event{valid0, invalid1, valid2})
	if res.Status != http.StatusAccepted {
		t.Fatalf("batch POST: got %d, want 202 (2 of 3 items are valid) (body: %s)", res.Status, res.Body)
	}
	decoded := f.decodeBatch(res)

	if decoded.Ingested != 2 {
		t.Errorf("batch: ingested = %d, want 2", decoded.Ingested)
	}
	if decoded.Failed != 1 {
		t.Errorf("batch: failed = %d, want 1", decoded.Failed)
	}
	if len(decoded.EventIDs) != 2 {
		t.Fatalf("batch: len(event_ids) = %d, want exactly 2 (one per successfully ingested item)\n  body: %s",
			len(decoded.EventIDs), res.Body)
	}

	byIndex := map[int]string{}
	for _, e := range decoded.EventIDs {
		byIndex[e.Index] = e.EventID
	}
	if _, ok := byIndex[0]; !ok {
		t.Errorf("batch: event_ids is missing index 0 (the first valid item): %+v", decoded.EventIDs)
	} else if byIndex[0] != id0 {
		t.Errorf("batch: event_ids[index=0] = %q, want the client literal %q", byIndex[0], id0)
	}
	if _, ok := byIndex[1]; ok {
		t.Errorf("batch: event_ids names index 1, but index 1 is the FAILED item — it must never appear here")
	}
	if _, ok := byIndex[2]; !ok {
		t.Errorf("batch: event_ids is missing index 2 (the second valid item): %+v", decoded.EventIDs)
	} else if byIndex[2] != id2 {
		t.Errorf("batch: event_ids[index=2] = %q, want the client literal %q", byIndex[2], id2)
	}
}
