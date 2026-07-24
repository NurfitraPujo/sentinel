package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	sharednats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	gonats "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

const ingestServiceSubject = "error_events"

func ensureErrorEventsStream(t *testing.T, conn *gonats.Conn) {
	t.Helper()

	js, err := conn.JetStream()
	require.NoError(t, err)
	if _, err = js.StreamInfo("ERROR_EVENTS"); err == nil {
		return
	}
	require.ErrorIs(t, err, gonats.ErrStreamNotFound)
	_, err = js.AddStream(&gonats.StreamConfig{
		Name:     "ERROR_EVENTS",
		Subjects: []string{ingestServiceSubject},
	})
	require.NoError(t, err)
}

func TestIngestServicePublishesValidPayload(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	publisher, err := sharednats.NewPublisher(ctx, sharednats.PublisherConfig{
		URL:     natsConfig.URL,
		Subject: ingestServiceSubject,
		Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, publisher.Close()) })

	svc, err := service.NewIngestService(publisher)
	require.NoError(t, err)

	conn, err := gonats.Connect(natsConfig.URL)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	ensureErrorEventsStream(t, conn)

	messages := make(chan *gonats.Msg, 1)
	sub, err := conn.ChanSubscribe(ingestServiceSubject, messages)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
	require.NoError(t, conn.Flush())

	now := time.Now().UTC().Truncate(time.Microsecond)
	unique := fmt.Sprintf("ingest-service-%d", now.UnixNano())
	payload := &validation.ErrorPayload{
		ProjectKey:  unique,
		Platform:    "go",
		Environment: "test",
		Message:     strings.Repeat("x", validation.MaxMessageLength),
		ErrorClass:  "IntegrationError",
		TraceID:     unique + "-trace",
		SpanID:      unique + "-span",
		Stacktrace: []validation.StackFrame{
			{File: "service_test.go", Line: 42, Function: "TestIngestServicePublishesValidPayload", InApp: true},
		},
		Metadata:   map[string]interface{}{"test_id": unique},
		Timestamp:  now,
		TraceFlags: 1,
	}

	require.NoError(t, svc.Ingest(ctx, payload))

	var msg *gonats.Msg
	select {
	case msg = <-messages:
	case <-ctx.Done():
		require.FailNow(t, "timed out waiting for published error event", ctx.Err().Error())
	}

	var event sentinelv1.ErrorEvent
	require.NoError(t, proto.Unmarshal(msg.Data, &event))
	assert.True(t, proto.Equal(mapping.MapPayloadToEvent(payload), &event), "published event should equal the input payload after mapping")
}

func TestIngestServiceRejectsInvalidPayloadWithoutPublishing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	publisher, err := sharednats.NewPublisher(ctx, sharednats.PublisherConfig{
		URL:     natsConfig.URL,
		Subject: ingestServiceSubject,
		Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, publisher.Close()) })

	svc, err := service.NewIngestService(publisher)
	require.NoError(t, err)

	conn, err := gonats.Connect(natsConfig.URL)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	ensureErrorEventsStream(t, conn)

	messages := make(chan *gonats.Msg, 1)
	sub, err := conn.ChanSubscribe(ingestServiceSubject, messages)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sub.Unsubscribe()) })
	require.NoError(t, conn.Flush())

	payload := &validation.ErrorPayload{
		ProjectKey:  "",
		Platform:    "go",
		Environment: "test",
		ErrorClass:  "InvalidPayloadError",
		Timestamp:   time.Now().UTC(),
	}

	err = svc.Ingest(ctx, payload)
	require.Error(t, err)
	assert.ErrorContains(t, err, "validation failed")

	select {
	case msg := <-messages:
		assert.Fail(t, "invalid payload was published", "received %d bytes", len(msg.Data))
	case <-time.After(500 * time.Millisecond):
		// No message is the expected result.
	}
}
