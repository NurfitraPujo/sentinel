package nats

import (
	"context"
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
