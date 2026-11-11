package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// V2b (sentinel-event-schema-v2 spec §9.2, TASK-02): IngestAt with the full v2 fixture, published through the
// real NATS container; the consumed message carries every v2 field by value.
func TestIngestServicePublishesEventV2Fields(t *testing.T) {
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

	raw, err := os.ReadFile("../fixtures/events/event-v2-full.json")
	require.NoError(t, err)
	var payload validation.ErrorPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&payload))
	payload.ProjectKey = fmt.Sprintf("ingest-service-v2-%d", time.Now().UnixNano())

	receivedAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, svc.IngestAt(ctx, &payload, receivedAt))

	var msg *gonats.Msg
	select {
	case msg = <-messages:
	case <-ctx.Done():
		require.FailNow(t, "timed out waiting for published error event", ctx.Err().Error())
	}

	var event sentinelv1.ErrorEvent
	require.NoError(t, proto.Unmarshal(msg.Data, &event))
	assert.True(t, proto.Equal(mapping.MapPayloadToEvent(&payload), &event), "published event should equal the payload after IngestAt's step and mapping")

	assert.Equal(t, "onerror", event.GetSource())
	assert.Equal(t, "warning", event.GetLevel())
	assert.Equal(t, "checkout", event.GetLogger())
	assert.Equal(t, "web-7f9c-pod-2", event.GetServerName())
	assert.Equal(t, "/checkout/pay", event.GetTransaction())
	assert.Equal(t, "4127", event.GetDist())
	assert.Equal(t, "1.8.0", event.GetSdk().GetVersion())
	assert.Equal(t, "user-typed-42", event.GetUser().GetId())
	assert.Equal(t, "DE", event.GetUser().GetGeo().GetCountryCode())
	assert.Equal(t, "tenant-acme", event.GetTags()["tenant_id"])
	assert.Equal(t, "Pixel 8", event.GetContexts()["device"].AsMap()["model"])
	assert.Equal(t, "sid=abc123; theme=dark", event.GetRequest().GetHeaders()["Cookie"])
	assert.Equal(t, "EUR", event.GetRequest().GetData().GetStructValue().AsMap()["currency"])
	assert.Equal(t, "10.1.2.3", event.GetRequest().GetEnv()["REMOTE_ADDR"])
	require.Len(t, event.GetBreadcrumbs(), 2)
	assert.Equal(t, "POST /api/pay 402", event.GetBreadcrumbs()[1].GetMessage())
	require.Len(t, event.GetException().GetValues(), 2)
	assert.Equal(t, "TypeError", event.GetException().GetValues()[1].GetType())
	assert.False(t, event.GetException().GetValues()[1].GetMechanism().GetHandled())
	assert.Equal(t, "pay.checkout", event.GetStacktrace()[0].GetModule())
	assert.Equal(t, []string{"  return charge(id);", "}"}, event.GetStacktrace()[0].GetPostContext())
	assert.Equal(t, receivedAt, event.GetReceivedAt().AsTime())
	_, clientMarker := event.GetMetadata().AsMap()["server_truncated"]
	assert.False(t, clientMarker, "a fixture within every bound gets no server_truncated marker (I3)")
}
