package nats

import (
	"context"
	"fmt"
	"github.com/nats-io/nats.go"
	"testing"
)

func TestDeliveryCountContext(t *testing.T) {
	if _, ok := DeliveryCountFromContext(context.Background()); ok {
		t.Fatal("a bare context carries no delivery count")
	}
	n, ok := DeliveryCountFromContext(WithDeliveryCount(context.Background(), 3))
	if !ok || n != 3 {
		t.Fatalf("got (%d, %v), want (3, true)", n, ok)
	}
}

// The subscriber hands every handler a context carrying the delivery count (sentinel-grouping-v2
// §5.2). A message with no JetStream metadata has an UNKNOWN count: the context carries none, so
// the retry is skipped rather than enabled by a default of 1.
func TestHandleMessage_UnknownDeliveryCountWhenMetadataFails(t *testing.T) {
	s := &Subscriber{}
	var got uint64
	var ok bool
	s.handleMessage(context.Background(), &nats.Msg{Data: []byte("x")}, func(ctx context.Context, _ []byte, _ Header) error {
		got, ok = DeliveryCountFromContext(ctx)
		return nil
	})
	if ok {
		t.Fatalf("handler saw a delivery count (%d); want none when Metadata() errors", got)
	}
}

// A JetStream message carries its delivery count in the ACK reply subject. The handler must see that
// count, not a default: the processor's bounded symbolication retry (sentinel-grouping-v2 §5.2)
// reads it from the context, and a handler handed a context without it never retries.
func TestHandleMessage_HandlerSeesMessageNumDelivered(t *testing.T) {
	for _, delivered := range []uint64{1, 2, 3, 6} {
		// $JS.ACK.<stream>.<consumer>.<delivered>.<stream seq>.<consumer seq>.<timestamp ns>.<pending>
		reply := fmt.Sprintf("$JS.ACK.EVENTS.processor.%d.10.10.1700000000000000000.0", delivered)
		msg := &nats.Msg{Subject: "events", Reply: reply, Data: []byte("x"), Sub: &nats.Subscription{}}

		var got uint64
		var ok bool
		func() {
			// The handler's nil return makes handleMessage Ack, which needs a live connection this
			// unit test deliberately does not have; the count is recorded before that panics.
			defer func() { _ = recover() }()
			(&Subscriber{}).handleMessage(context.Background(), msg, func(ctx context.Context, _ []byte, _ Header) error {
				got, ok = DeliveryCountFromContext(ctx)
				return nil
			})
		}()
		if !ok || got != delivered {
			t.Errorf("message delivered %d times: handler saw (%d, %v)", delivered, got, ok)
		}
	}
}
