package dlqmonitor

import (
	"context"
	"fmt"
	"log"
	"time"

	sharedNats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	natsgo "github.com/nats-io/nats.go"
)

// JetStreamDetailer implements OldestMessageSource against a second, read-only NATS/JetStream
// connection, kept deliberately separate from the Subscriber's own connection.
//
// packages/shared-go/nats.Subscriber does not expose its JetStream context (js and conn are private),
// and this change is scoped to apps/processor-go only — packages/shared-go/nats is owned by a parallel
// change in flight and is off limits here. So rather than extend DLQStats, this opens its own
// connection to do exactly two admin calls, both O(1) regardless of DLQ depth:
//
//  1. StreamInfo, for State.FirstTime — the same data DLQStats already computes internally for Depth,
//     just not returned today.
//  2. GetMsg for that single oldest message's sequence, to read its X-Sentinel-Dlq-Class header.
//
// It deliberately does NOT attempt a full permanent/transient breakdown across the whole backlog — that
// would be one JetStream request per message, which is not cheap for a queue sized like the
// 6,148-message incident this feature responds to. The oldest message's class is the highest-value
// single data point available for O(1) cost: it is the longest-neglected message, so its class answers
// "can this backlog resolve itself, or does someone have to look" — exactly the distinction
// docs/plans/E2E_RECOVERY_PLAN.md and DLQClassPermanent/DLQClassTransient's doc comments call out.
type JetStreamDetailer struct {
	conn *natsgo.Conn
	js   natsgo.JetStreamContext
}

// NewJetStreamDetailer opens the secondary connection. It intentionally mirrors only the NATS URL that
// main.go already passes to the primary Subscriber (natCfg.URL) — NKey/TLS options are not wired here
// because main.go does not currently wire them for the primary subscriber either, so this connection
// regresses no auth capability relative to what already runs today. If a future change adds NKey/TLS to
// the primary connection, this constructor should gain the matching options at the same time.
func NewJetStreamDetailer(url string) (*JetStreamDetailer, error) {
	conn, err := natsgo.Connect(url)
	if err != nil {
		return nil, fmt.Errorf("dlqmonitor: connect: %w", err)
	}
	js, err := conn.JetStream()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("dlqmonitor: jetstream context: %w", err)
	}
	return &JetStreamDetailer{conn: conn, js: js}, nil
}

// Close releases the underlying connection. Safe to call on a nil *JetStreamDetailer.
func (d *JetStreamDetailer) Close() {
	if d == nil || d.conn == nil {
		return
	}
	d.conn.Close()
}

// OldestMessage implements OldestMessageSource. Safe to call on a nil *JetStreamDetailer (returns
// hasAge=false, err=nil), so callers do not need to nil-check before every use.
func (d *JetStreamDetailer) OldestMessage(ctx context.Context, stream string) (bool, time.Duration, string, error) {
	if d == nil {
		return false, 0, "", nil
	}

	info, err := d.js.StreamInfo(stream, natsgo.Context(ctx))
	if err != nil {
		return false, 0, "", fmt.Errorf("StreamInfo(%s): %w", stream, err)
	}
	if info.State.Msgs == 0 || info.State.FirstTime.IsZero() {
		return false, 0, "", nil
	}
	age := time.Since(info.State.FirstTime)

	raw, err := d.js.GetMsg(stream, info.State.FirstSeq, natsgo.Context(ctx))
	if err != nil {
		// The age is still real and worth reporting even if the class lookup failed independently
		// (e.g. the message was replayed/deleted between the two calls) — degrade the class only.
		log.Printf("dlqmonitor: GetMsg(%s, %d) failed, oldest-message class unavailable: %v", stream, info.State.FirstSeq, err)
		return true, age, sharedNats.DLQClassUnclassified, nil
	}

	switch raw.Header.Get(sharedNats.DLQClassHeader) {
	case sharedNats.DLQClassPermanent:
		return true, age, sharedNats.DLQClassPermanent, nil
	case sharedNats.DLQClassTransient:
		return true, age, sharedNats.DLQClassTransient, nil
	default:
		return true, age, sharedNats.DLQClassUnclassified, nil
	}
}
