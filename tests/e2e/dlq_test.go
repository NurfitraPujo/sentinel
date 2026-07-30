package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// This file covers matrix rows U33-U34 (docs/plans/E2E_RECOVERY_PLAN.md, "## P7"): the JetStream streams
// are bounded, and the DLQ backlog is reported in a way somebody could act on.
//
// Both rows exist because of a real incident. The dead-letter queue silently reached 6,148 messages,
// exhausted JetStream storage, and started failing unrelated operations with "nats: insufficient storage
// resources available". Nothing alerted, because nothing was watching, and nothing was bounded.

// ---------------------------------------------------------------------------------------------------
// U33 — the streams are bounded
// ---------------------------------------------------------------------------------------------------

// TestU33_StreamsAreBounded asserts that every JetStream stream has both an age and a size limit, and
// that each one's discard policy is the one its role requires.
//
// The failure this prevents is not subtle but it is silent: with retention=Limits and no limits, nothing
// is ever removed. ERROR_EVENTS accumulated 18,654 fully-acked messages that way. Combined with
// discard=new, a full store means the server REJECTS NEW PUBLISHES — ingestion stops at the front door,
// and the first symptom is unrelated things failing.
//
// The assertions are deliberately about the PROPERTY (bounded at all) rather than the exact tuning, which
// is a judgement call that may legitimately change. The discard policies are asserted exactly, because
// those are correctness rather than tuning — see each case.
func TestU33_StreamsAreBounded(t *testing.T) {
	requireStack(t)

	nc, err := nats.Connect(cfg.NATSURL, nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to NATS at %s: %v", cfg.NATSURL, err)
	}
	defer nc.Close()

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("obtaining a JetStream context: %v", err)
	}

	cases := []struct {
		stream      string
		wantDiscard nats.DiscardPolicy
		why         string
	}{
		{
			stream:      "ERROR_EVENTS",
			wantDiscard: nats.DiscardNew,
			why: "an ingest stream must reject new publishes when full rather than silently drop " +
				"unprocessed events — rejection is backpressure the ingestor can surface, whereas " +
				"DiscardOld would destroy events nobody has processed yet",
		},
		{
			stream:      "ERROR_EVENTS_DLQ",
			wantDiscard: nats.DiscardOld,
			why: "a full DLQ must never refuse new dead letters. If it did, the subscriber could not " +
				"park a poison message and would NAK it forever — recreating exactly the S13 livelock " +
				"the DLQ exists to prevent",
		},
	}

	for _, c := range cases {
		t.Run(c.stream, func(t *testing.T) {
			info, err := js.StreamInfo(c.stream)
			if err != nil {
				t.Fatalf("StreamInfo(%s): %v", c.stream, err)
			}
			t.Logf("U33 %s: msgs=%d maxAge=%v maxBytes=%d discard=%v",
				c.stream, info.State.Msgs, info.Config.MaxAge, info.Config.MaxBytes, info.Config.Discard)

			if info.Config.MaxAge <= 0 {
				t.Errorf("%s has no MaxAge — messages are retained forever, so the stream grows without "+
					"bound until the store is exhausted", c.stream)
			}
			if info.Config.MaxBytes <= 0 {
				t.Errorf("%s has no MaxBytes — a single burst of large events can exhaust the store even "+
					"inside the age window", c.stream)
			}
			if info.Config.Discard != c.wantDiscard {
				t.Errorf("%s discard policy is %v, want %v: %s", c.stream, info.Config.Discard, c.wantDiscard, c.why)
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------
// U34 — the backlog is reported in a way somebody could act on
// ---------------------------------------------------------------------------------------------------

// dlqHealth is the full processor /health body, including the operational fields. It is a superset of
// harness_test.go's processorHealth, which stays minimal because most rows only need depth and status.
type dlqHealth struct {
	Status             string   `json:"status"`
	Database           string   `json:"database"`
	DLQStream          string   `json:"dlq_stream"`
	DLQDepth           int64    `json:"dlq_depth"`
	DLQPublishFailures int64    `json:"dlq_publish_failures"`
	DLQThreshold       *int64   `json:"dlq_threshold"`
	DLQStaleAfterSecs  *float64 `json:"dlq_stale_after_seconds"`
	DLQOldestAgeSecs   *float64 `json:"dlq_oldest_age_seconds"`
	DLQOldestClass     *string  `json:"dlq_oldest_class"`
}

func readDLQHealth(t *testing.T) (dlqHealth, string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(cfg.ProcessorHealth + "/health")
	if err != nil {
		t.Fatalf("GET processor /health: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	var out dlqHealth
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("processor /health was not JSON: %v\n  body: %s", err, raw)
	}
	return out, string(raw)
}

// TestU34_DLQBacklogIsReportedActionably asserts the health endpoint carries enough to decide whether a
// backlog needs attention — not merely that one exists.
//
// "dlq_depth > 0" was the old signal and it is not actionable: a single poison message is normal
// operation, so a status that flips on the first one gets ignored, and then 6,148 accumulate. What makes
// it actionable is a threshold to compare against and the age of the OLDEST message — a stale backlog
// means nobody is triaging, which is worse than a large fresh one.
func TestU34_DLQBacklogIsReportedActionably(t *testing.T) {
	requireStack(t)

	h, raw := readDLQHealth(t)
	t.Logf("U34 processor /health: %s", raw)

	if h.DLQStream == "" {
		t.Error("dlq_stream is empty — the endpoint does not say which stream it is reporting on")
	}
	if h.Database == "" {
		t.Error("database field is missing from /health")
	}

	// A bare depth is a number, not a signal. Without a threshold to compare it against, every consumer
	// of this endpoint has to hardcode its own idea of "too many".
	if h.DLQThreshold == nil {
		t.Error("dlq_threshold is absent — /health reports a depth with nothing to compare it against, " +
			"so no external monitor can decide whether the backlog is normal or an incident")
	} else if *h.DLQThreshold <= 0 {
		t.Errorf("dlq_threshold is %d, which can never be a meaningful trigger", *h.DLQThreshold)
	}

	if h.DLQStaleAfterSecs == nil {
		t.Error("dlq_stale_after_seconds is absent — age is half the signal: a small backlog nobody has " +
			"touched for hours is worse than a large one that is actively draining")
	}

	// Depth and the oldest-message fields have to agree with each other. An empty queue reporting an age,
	// or a non-empty one reporting none, means the two are being computed from different places.
	switch {
	case h.DLQDepth == 0 && h.DLQOldestAgeSecs != nil:
		t.Errorf("dlq_depth is 0 but dlq_oldest_age_seconds is %v — the queue cannot be simultaneously "+
			"empty and hold an oldest message", *h.DLQOldestAgeSecs)
	case h.DLQDepth > 0 && h.DLQOldestAgeSecs == nil:
		t.Logf("note: dlq_depth=%d but no dlq_oldest_age_seconds — the detailer is optional and may be "+
			"unavailable; this is a degraded signal, not a contradiction", h.DLQDepth)
	}

	// The point of this assertion is drift, not tidiness: these three strings are the entire vocabulary
	// defined in packages/shared-go/nats, and a fourth value means a reader and the writer disagree.
	// "unclassified" is deliberately included — it is the honest answer for a message parked before the
	// class header existed, and for one whose header value is not recognized. Reporting those as
	// "transient" would be worse than useless, because transient is the replayable class.
	//
	// Two readers of this header did briefly invent different words for that state ("unknown" in the
	// processor's /health, "unclassified" in tools/dlq), which is why the name now lives in one place.
	if h.DLQOldestClass != nil {
		got := *h.DLQOldestClass
		switch got {
		case "permanent", "transient", "unclassified":
		default:
			t.Errorf("dlq_oldest_class is %q, want \"permanent\", \"transient\" or \"unclassified\" — that "+
				"is the whole vocabulary packages/shared-go/nats defines for X-Sentinel-Dlq-Class, so "+
				"anything else means a reader and the writer have drifted", got)
		}
	}
}

// TestU34_DLQDepthTracksARealDeadLetter proves the reported depth is live rather than a constant, by
// parking a real message and watching the number move.
//
// A health field that never changes is indistinguishable from a hardcoded zero, and this endpoint is now
// the thing an operator is expected to trust.
func TestU34_DLQDepthTracksARealDeadLetter(t *testing.T) {
	requireStack(t)

	before, _ := readDLQHealth(t)

	nc, err := nats.Connect(cfg.NATSURL, nats.Timeout(10*time.Second))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()
	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}

	marker := "e2e-u34-" + uniqueSuffix()
	if _, err := js.Publish("error_events", []byte("\x00\x01\x02 undecodable "+marker)); err != nil {
		t.Fatalf("publishing a malformed event: %v", err)
	}

	waitFor(t, 3*time.Minute, "the processor's reported DLQ depth to rise", func() (bool, string) {
		now, _ := readDLQHealth(t)
		return now.DLQDepth > before.DLQDepth,
			fmt.Sprintf("depth %d (was %d)", now.DLQDepth, before.DLQDepth)
	})

	after, raw := readDLQHealth(t)
	t.Logf("U34 depth %d -> %d after parking one malformed event. /health: %s",
		before.DLQDepth, after.DLQDepth, raw)

	// With a message parked, the oldest-message detail should now be populated — that is the field an
	// operator reads to decide whether anyone is triaging.
	if after.DLQOldestAgeSecs == nil {
		t.Error("dlq_oldest_age_seconds is still absent with a message parked — the age signal is not " +
			"being computed, so a stale backlog would look identical to a fresh one")
	}
	if after.DLQOldestClass == nil {
		t.Error("dlq_oldest_class is absent with a message parked — without it, a backlog of permanently " +
			"dead messages (which will never drain on their own) looks the same as a transient one that " +
			"will clear when the database recovers")
	}
}
