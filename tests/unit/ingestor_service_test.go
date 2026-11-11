package unit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/obs"
)

// fakeEventPublisher captures every message service.IngestService would have handed to
// *nats.Publisher.PublishWithHeaders, without a real NATS connection. It satisfies the unexported
// eventPublisher interface service.NewIngestService accepts purely structurally - Go interface
// satisfaction does not require naming the interface, so this file needs no access to it.
type fakeEventPublisher struct {
	published [][]byte
}

func (f *fakeEventPublisher) PublishWithHeaders(_ context.Context, data []byte, _ nats.Header) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	f.published = append(f.published, cp)
	return nil
}

// validIngestPayload returns an ErrorPayload with every proto-required field populated (project_key,
// platform, environment, error_class - see requiredWireKeys in tests/contract/sdk_ingestor_test.go),
// so the only thing under test in each case below is the event_id policy, not an unrelated validation
// rejection. Callers set EventID themselves.
func validIngestPayload() *validation.ErrorPayload {
	return &validation.ErrorPayload{
		ProjectKey:  "proj-svc-test",
		Platform:    "go",
		Environment: "test",
		ErrorClass:  "RuntimeError",
		Timestamp:   time.Now().UTC(),
	}
}

func newTestIngestService(t *testing.T) (*service.IngestService, *fakeEventPublisher) {
	t.Helper()
	pub := &fakeEventPublisher{}
	svc, err := service.NewIngestService(pub)
	require.NoError(t, err)
	return svc, pub
}

// publishedEvent unmarshals the single message fakeEventPublisher captured back into the real proto
// type, so assertions run against what would actually have gone out on the wire.
func publishedEvent(t *testing.T, pub *fakeEventPublisher) *sentinelv1.ErrorEvent {
	t.Helper()
	require.Len(t, pub.published, 1, "expected exactly one message published")
	var event sentinelv1.ErrorEvent
	require.NoError(t, proto.Unmarshal(pub.published[0], &event))
	return &event
}

// TestIngest_ClientEventIDTooLongIsReplacedNotRejected proves D-a's invalid-id policy
// (docs/plans/IDEMPOTENCY_PLAN.md): an oversized event_id (>64 chars, the proto's
// "error_event.event_id" CEL bound and error_occurrences.event_id VARCHAR(64)) must NOT 400-reject the
// request - it must be replaced with a freshly minted UUIDv4 and the request must still succeed, with
// the published proto carrying the fresh id, not the client's oversized one.
func TestIngest_ClientEventIDTooLongIsReplacedNotRejected(t *testing.T) {
	svc, pub := newTestIngestService(t)

	tooLong := strings.Repeat("a", 65)
	payload := validIngestPayload()
	payload.EventID = tooLong

	err := svc.Ingest(context.Background(), payload)
	require.NoError(t, err, "an oversized event_id must be replaced, never rejected (D-a)")

	event := publishedEvent(t, pub)
	assert.NotEqual(t, tooLong, event.GetEventId(), "the published proto must carry a fresh minted id, not the client's oversized one")
	assert.NotEmpty(t, event.GetEventId())
	assert.LessOrEqual(t, len(event.GetEventId()), 64, "the minted id itself must respect the 64-char bound")

	// D-a/D-f: the effective id is stamped back onto payload.EventID in place, which is what
	// main.go's handleIngest/handleBatchIngest read to echo the effective id in the HTTP response.
	assert.Equal(t, event.GetEventId(), payload.EventID, "payload.EventID must be mutated to the effective (minted) id")
}

// TestIngest_ClientEventIDEmptyIsMinted proves the absent/empty half of D-a: no event_id in the
// request is not an error (pre-W0 and non-Go-SDK clients may never send one) - the ingestor mints a
// UUIDv4 and the request succeeds normally.
func TestIngest_ClientEventIDEmptyIsMinted(t *testing.T) {
	svc, pub := newTestIngestService(t)

	payload := validIngestPayload()
	payload.EventID = ""

	err := svc.Ingest(context.Background(), payload)
	require.NoError(t, err)

	event := publishedEvent(t, pub)
	assert.NotEmpty(t, event.GetEventId(), "an absent event_id must be minted, never published as empty")
	assert.Equal(t, event.GetEventId(), payload.EventID, "payload.EventID must be mutated to the minted id")
}

// TestIngest_ClientEventIDValidIsUsedVerbatim proves the common case of D-a: a usable client id
// (1-64 chars) rides through unchanged - the id in the published proto and in the mutated payload must
// be byte-identical to what the client sent, or dedup keyed on it would silently break.
func TestIngest_ClientEventIDValidIsUsedVerbatim(t *testing.T) {
	svc, pub := newTestIngestService(t)

	const clientID = "client-supplied-event-id-0001"
	payload := validIngestPayload()
	payload.EventID = clientID

	err := svc.Ingest(context.Background(), payload)
	require.NoError(t, err)

	event := publishedEvent(t, pub)
	assert.Equal(t, clientID, event.GetEventId(), "a valid client event_id must be used verbatim, not replaced")
	assert.Equal(t, clientID, payload.EventID)
}

// TestIngest_NegativeStackFrameColumnIsRejectedBeforePublish pins sentinel-sourcemaps §4.3 on the
// LIVE ingest path: validation.ValidatePayload carries the same rule but no handler calls it
// (docs/memory/BUGS.md B3), so the check that actually answers a request lives in Ingest. A frame
// with column < 0 must be a "validation failed: stacktrace[i].column must be >= 0" error -- the
// prefix handleIngest maps to 400 -- and nothing may be published; 0 (unknown) and a positive column
// ride through to the proto's field 5 verbatim.
func TestIngest_NegativeStackFrameColumnIsRejectedBeforePublish(t *testing.T) {
	t.Run("column=-1 on the second frame is rejected with that frame's index and nothing is published", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		payload := validIngestPayload()
		payload.Platform = "javascript"
		payload.Stacktrace = []validation.StackFrame{
			{File: "/assets/index-3f2a1c.js", Line: 1, Column: 5, Function: "f", InApp: true},
			{File: "/assets/vendor-9a0d4e.js", Line: 3, Column: -1, Function: "g", InApp: true},
		}

		err := svc.Ingest(context.Background(), payload)
		require.EqualError(t, err, "validation failed: stacktrace[1].column must be >= 0")
		assert.Empty(t, pub.published, "a rejected event must never reach the publisher")
	})

	t.Run("column=1234 and an absent column both publish, carried verbatim on proto field 5", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		payload := validIngestPayload()
		payload.Platform = "javascript"
		payload.Stacktrace = []validation.StackFrame{
			{File: "/assets/index-3f2a1c.js", Line: 1, Column: 1234, Function: "f", InApp: true},
			{File: "/assets/vendor-9a0d4e.js", Line: 3, Function: "g", InApp: false},
		}

		require.NoError(t, svc.Ingest(context.Background(), payload))
		event := publishedEvent(t, pub)
		require.Len(t, event.GetStacktrace(), 2)
		assert.Equal(t, int32(1234), event.GetStacktrace()[0].GetColumn())
		assert.Equal(t, int32(0), event.GetStacktrace()[1].GetColumn())
	})
}

// TestIngest_TwoMintedEventIDsAreDistinct is a sanity check that resolveEventID's minting is not
// accidentally deterministic (e.g. derived from a fixed input) - two separate empty-id requests must
// not collide, or D-b's per-issue dedup would silently merge unrelated events.
func TestIngest_TwoMintedEventIDsAreDistinct(t *testing.T) {
	svc, pub := newTestIngestService(t)

	payloadA := validIngestPayload()
	payloadA.EventID = ""
	require.NoError(t, svc.Ingest(context.Background(), payloadA))

	payloadB := validIngestPayload()
	payloadB.EventID = ""
	require.NoError(t, svc.Ingest(context.Background(), payloadB))

	require.Len(t, pub.published, 2)
	var eventA, eventB sentinelv1.ErrorEvent
	require.NoError(t, proto.Unmarshal(pub.published[0], &eventA))
	require.NoError(t, proto.Unmarshal(pub.published[1], &eventB))
	assert.NotEqual(t, eventA.GetEventId(), eventB.GetEventId())
}

// TestIngest_ClientEventID64MultibyteRunesIsUsedVerbatim pins the unit the length guard counts in
// (F-VW0-1). All three enforcement points count CHARACTERS: the proto CEL rule (`.size()` counts code
// points — proven by execution during W0 review: 64 copies of 'ä' = 128 bytes passes, 65 runes fails), Postgres
// VARCHAR(64), and — because of this test — the ingestor's resolveEventID. The original implementation
// used len() (bytes), which re-minted any multibyte id over 64 BYTES with a false "too_long", silently
// stripping those clients of dedup while every gate stayed green: the mismatch direction can never
// produce a 400 or a 22001, only a quietly different id, which is exactly why it needs its own pin.
func TestIngest_ClientEventID64MultibyteRunesIsUsedVerbatim(t *testing.T) {
	svc, pub := newTestIngestService(t)

	clientID := strings.Repeat("ä", 64) // 64 runes, 128 bytes: over a byte bound, within the real one
	payload := validIngestPayload()
	payload.EventID = clientID

	err := svc.Ingest(context.Background(), payload)
	require.NoError(t, err)

	event := publishedEvent(t, pub)
	assert.Equal(t, clientID, event.GetEventId(),
		"a 64-RUNE multibyte id is within every real bound (CEL counts code points, VARCHAR(64) counts "+
			"characters) and must ride through verbatim — re-minting it means the guard is counting bytes")
	assert.Equal(t, clientID, payload.EventID)
}

// TestIngest_ClientEventIDWithControlCharsIsReplaced pins the invalid_chars arm of D-a (F-VW0-2). A
// NUL byte inside a JSON string survives decoding (`a\x00b` is a legal 3-char Go string) and passes
// protovalidate (size()==3), but Postgres cannot store 0x00 in a varchar — so once the processor
// writes event_id, a NUL-carrying id would fail the INSERT with a class-22 error that
// classifyStoreError marks Permanent: the client could dead-letter its own events at will. The
// ingestor therefore mints over any id carrying a control character, preserving the event at the cost
// of that client's dedup.
func TestIngest_ClientEventIDWithControlCharsIsReplaced(t *testing.T) {
	svc, pub := newTestIngestService(t)

	payload := validIngestPayload()
	payload.EventID = "a\x00b"

	err := svc.Ingest(context.Background(), payload)
	require.NoError(t, err, "a control-char id must be replaced, never rejected — rejection would drop the event")

	event := publishedEvent(t, pub)
	assert.NotEqual(t, "a\x00b", event.GetEventId(), "the NUL-carrying id must not reach the wire")
	assert.NotContains(t, event.GetEventId(), "\x00")
	assert.Regexp(t, `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
		event.GetEventId(), "the replacement must be a freshly minted UUIDv4")
}

// ---------------------------------------------------------------------------------------------------
// sentinel-ingest-timestamp-clamp — contract §4.1 R1–R4, §4.2 (one T_recv per request), §4.3 (the
// counter). This is §11 row 1: the rule runs in IngestService and is observed exactly where its
// sibling resolveEventID is — post a payload, proto.Unmarshal what was published, read
// Timestamp.AsTime(). The receipt time is injected through IngestAt so every boundary is exact.
// ---------------------------------------------------------------------------------------------------

// tsRecv is the fixed T_recv every case below is measured against. Deliberately NOT time.Now(): the
// ±1 s boundary cases only mean something against an instant the test controls.
var tsRecv = time.Date(2026, 9, 16, 10, 0, 0, 500_000_000, time.UTC)

// decodeIngestPayload drives a raw JSON body through encoding/json into validation.ErrorPayload, the
// exact decode path handleIngest uses (json.NewDecoder(...).Decode(&payload)), so what the R1 cases
// assert is what the decoder actually produces for each wire shape — decision 1 proven, not assumed.
func decodeIngestPayload(t *testing.T, body string) *validation.ErrorPayload {
	t.Helper()
	var payload validation.ErrorPayload
	require.NoError(t, json.Unmarshal([]byte(body), &payload))
	return &payload
}

// publishedTimestamp is publishedEvent narrowed to proto field 10, as the instant it carries.
func publishedTimestamp(t *testing.T, pub *fakeEventPublisher) time.Time {
	t.Helper()
	event := publishedEvent(t, pub)
	require.NotNil(t, event.GetTimestamp(), "proto field 10 must never be nil")
	return event.GetTimestamp().AsTime()
}

// installTimestampManualReader installs a fresh sdkmetric.ManualReader as the GLOBAL meter provider
// and returns it. It must run BEFORE service.NewIngestService, which creates its counters through
// otel.Meter at construction time; the eidManualReader pattern (tests/integration/
// event_idempotency_test.go) with one difference — each test installs its own, so a count asserted
// here is a count this test produced and never another test's global. Not restored in cleanup on
// purpose: OTel resolves delegation to the FIRST real provider an instrument saw, so a restore does
// not undo anything and a later test that installs its own reader is unaffected either way.
func installTimestampManualReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	return reader
}

// collectTimestampReplaced snapshots sentinel_ingest_timestamp_replaced_total by reason label. A
// missing family is an empty map, not an error: before the first replacement the counter has never
// recorded and emits nothing, which is exactly the R4 assertion.
func collectTimestampReplaced(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != obs.MetricIngestTimestampReplaced {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok, "expected %s to be an int64 Sum, got %T", obs.MetricIngestTimestampReplaced, m.Data)
			for _, dp := range sum.DataPoints {
				v, ok := dp.Attributes.Value(attribute.Key(obs.LabelReason))
				require.True(t, ok, "%s must carry a %s label on every data point", obs.MetricIngestTimestampReplaced, obs.LabelReason)
				require.Equal(t, 1, dp.Attributes.Len(), "%s must carry NO label but %s (never the value, delta or project)", obs.MetricIngestTimestampReplaced, obs.LabelReason)
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

// TestIngest_TimestampBoundsAreTheContractsValues pins decisions 8 and 9 as literals: the two
// constants are what docs/sdk-specification.md tells clients, and a drifted value would be a
// semantic wire change nothing else would catch.
func TestIngest_TimestampBoundsAreTheContractsValues(t *testing.T) {
	assert.Equal(t, 60*time.Second, service.MaxFutureSkew, "decision 8: MaxFutureSkew = 60 s")
	assert.Equal(t, 30*24*time.Hour, service.MaxEventAge, "decision 9: MaxEventAge = 30 days")
}

// TestIngest_TimestampEmptyIsReceiptTime is R1 (flows W1), driven through encoding/json for each of
// the three wire shapes decision 1 says are one case: no key, an explicit null, and the literal zero
// string a Go client with an unset time.Time marshals to. Each must decode to time.Time{} (asserted,
// not assumed — the whole rule hinges on it) and publish proto field 10 = T_recv exactly.
func TestIngest_TimestampEmptyIsReceiptTime(t *testing.T) {
	const bodyPrefix = `{"project_key":"proj-svc-test","platform":"python","environment":"test","error_class":"ValueError"`
	cases := []struct {
		name string
		body string
	}{
		{"absent", bodyPrefix + `}`},
		{"null", bodyPrefix + `,"timestamp":null}`},
		{"explicit zero string", bodyPrefix + `,"timestamp":"0001-01-01T00:00:00Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, pub := newTestIngestService(t)
			payload := decodeIngestPayload(t, tc.body)
			require.True(t, payload.Timestamp.IsZero(),
				"decision 1: encoding/json must leave time.Time at its zero value for this shape, got %v", payload.Timestamp)

			require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv))

			got := publishedTimestamp(t, pub)
			assert.True(t, tsRecv.Equal(got), "R1: an empty timestamp must publish T_recv, got %v want %v", got, tsRecv)
			assert.Equal(t, tsRecv.UnixNano(), got.UnixNano(), "T_recv must reach the wire at nanosecond precision")
			// Decision 12: the effective value is stamped back onto the payload, in UTC, which is what
			// handleIngest/handleBatchIngest read to echo it.
			assert.True(t, tsRecv.Equal(payload.Timestamp), "payload.Timestamp must be mutated to the effective value")
			assert.Equal(t, time.UTC, payload.Timestamp.Location(), "the stamped-back value must be UTC")
		})
	}
}

// TestIngest_TimestampFutureBoundary is R2 (flows W3) at both sides of T_recv + MaxFutureSkew:
// one second past the bound is replaced with T_recv; one second inside it is stored verbatim — a
// client two seconds ahead is not "wrong" (contract §4.1).
func TestIngest_TimestampFutureBoundary(t *testing.T) {
	t.Run("T_recv+MaxFutureSkew+1s is replaced with T_recv", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		payload := validIngestPayload()
		payload.Timestamp = tsRecv.Add(service.MaxFutureSkew + time.Second)

		require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv), "R2 replaces, never rejects (decision 5)")

		got := publishedTimestamp(t, pub)
		assert.True(t, tsRecv.Equal(got), "R2: a too-far-future timestamp must publish T_recv, got %v", got)
		assert.True(t, tsRecv.Equal(payload.Timestamp), "payload.Timestamp must be mutated to T_recv")
	})

	t.Run("T_recv+MaxFutureSkew-1s is stored verbatim", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		sent := tsRecv.Add(service.MaxFutureSkew - time.Second)
		payload := validIngestPayload()
		payload.Timestamp = sent

		require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv))

		got := publishedTimestamp(t, pub)
		assert.True(t, sent.Equal(got), "R4: a value inside the window must be stored verbatim, got %v want %v", got, sent)
		assert.Equal(t, sent.UnixNano(), got.UnixNano())
		assert.True(t, sent.Equal(payload.Timestamp))
	})
}

// TestIngest_TimestampAncientBoundary is R3 (flows W4, decision 10) at both sides of
// T_recv − MaxEventAge: one second past the bound is disbelieved entirely and becomes T_recv — NOT
// the bound's edge, which would land the row one second inside the retention window to be swept at
// the next run (OQ-3 (b), rejected) — and one second inside it is stored verbatim.
func TestIngest_TimestampAncientBoundary(t *testing.T) {
	t.Run("T_recv-MaxEventAge-1s is replaced with T_recv", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		payload := validIngestPayload()
		payload.Timestamp = tsRecv.Add(-(service.MaxEventAge + time.Second))

		require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv), "R3 replaces, never rejects (decision 5)")

		got := publishedTimestamp(t, pub)
		assert.True(t, tsRecv.Equal(got), "R3 (decision 10): an ancient timestamp must publish T_recv itself, got %v", got)
		assert.False(t, tsRecv.Add(-service.MaxEventAge).Equal(got), "R3 must NOT clamp to the window's edge (OQ-3 (b) was rejected)")
		assert.True(t, tsRecv.Equal(payload.Timestamp), "payload.Timestamp must be mutated to T_recv")
	})

	t.Run("T_recv-MaxEventAge+1s is stored verbatim", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		sent := tsRecv.Add(-(service.MaxEventAge - time.Second))
		payload := validIngestPayload()
		payload.Timestamp = sent

		require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv))

		got := publishedTimestamp(t, pub)
		assert.True(t, sent.Equal(got), "R4: a value inside the window must be stored verbatim, got %v want %v", got, sent)
		assert.Equal(t, sent.UnixNano(), got.UnixNano())
	})
}

// TestIngest_TimestampNonUTCOffsetIsSameInstant is R4 (flows W2) with a non-UTC offset: a +07:00
// value decoded from the wire is published as the same INSTANT — the offset is normalised, the
// moment is not moved — and stamped back onto the payload in UTC so the echo renders it with "Z".
func TestIngest_TimestampNonUTCOffsetIsSameInstant(t *testing.T) {
	svc, pub := newTestIngestService(t)
	// 2026-09-16T16:59:30.25+07:00 == 2026-09-16T09:59:30.25Z, thirty seconds before tsRecv: inside
	// the window on both sides, so only R4 can apply.
	payload := decodeIngestPayload(t, `{"project_key":"proj-svc-test","platform":"python","environment":"test",`+
		`"error_class":"ValueError","timestamp":"2026-09-16T16:59:30.25+07:00"}`)
	want := time.Date(2026, 9, 16, 9, 59, 30, 250_000_000, time.UTC)
	require.True(t, want.Equal(payload.Timestamp), "precondition: the decoder kept the instant")
	_, offset := payload.Timestamp.Zone()
	require.Equal(t, 7*3600, offset, "precondition: the decoder kept the +07:00 offset, so this test exercises normalisation")

	require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv))

	got := publishedTimestamp(t, pub)
	assert.True(t, want.Equal(got), "R4: a +07:00 value must be stored as the same instant, got %v want %v", got, want)
	assert.Equal(t, want.UnixNano(), got.UnixNano())
	assert.Equal(t, time.UTC, payload.Timestamp.Location(), "the stamped-back value must be UTC (decision 12: the echo ends in Z)")
	assert.Equal(t, "2026-09-16T09:59:30.25Z", payload.Timestamp.Format(time.RFC3339Nano))
}

// TestIngest_BatchSharesOneReceiptTime is §4.2 / flows W6 (decision 3): three items — absent (R1),
// a year ahead (R2), and a good one (R4) — ingested against ONE fixed receivedAt, the way
// handleBatchIngest reads the clock once before its loop. Both replaced items must carry the SAME
// instant, and the good one must ride through untouched.
func TestIngest_BatchSharesOneReceiptTime(t *testing.T) {
	svc, pub := newTestIngestService(t)

	good := tsRecv.Add(-5 * time.Minute)
	batch := []*validation.ErrorPayload{
		decodeIngestPayload(t, `{"project_key":"proj-svc-test","platform":"python","environment":"test","error_class":"ValueError"}`),
		validIngestPayload(),
		validIngestPayload(),
	}
	batch[1].Timestamp = tsRecv.Add(365 * 24 * time.Hour)
	batch[2].Timestamp = good
	require.True(t, batch[0].Timestamp.IsZero())

	for i, payload := range batch {
		require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv), "item %d", i)
	}

	require.Len(t, pub.published, 3)
	published := make([]time.Time, 3)
	for i := range pub.published {
		var event sentinelv1.ErrorEvent
		require.NoError(t, proto.Unmarshal(pub.published[i], &event))
		require.NotNil(t, event.GetTimestamp())
		published[i] = event.GetTimestamp().AsTime()
	}

	assert.True(t, tsRecv.Equal(published[0]), "item 0 (absent) must be T_recv, got %v", published[0])
	assert.True(t, tsRecv.Equal(published[1]), "item 1 (future) must be T_recv, got %v", published[1])
	assert.Equal(t, published[0].UnixNano(), published[1].UnixNano(), "every replaced item of one batch shares ONE instant (decision 3)")
	assert.True(t, good.Equal(published[2]), "item 2 (good) must be stored verbatim, got %v want %v", published[2], good)
	assert.Equal(t, good.UnixNano(), published[2].UnixNano())
	// The per-item echo reads these back (decision 12).
	assert.True(t, tsRecv.Equal(batch[0].Timestamp))
	assert.True(t, tsRecv.Equal(batch[1].Timestamp))
	assert.True(t, good.Equal(batch[2].Timestamp))
}

// TestIngest_TimestampReplacedCounter is §4.3 / flows W5 at the unit boundary: a ManualReader
// installed as the global meter provider BEFORE NewIngestService (so the service's counter is created
// against it) sees sentinel_ingest_timestamp_replaced_total with data points for exactly
// reason=empty|future|ancient, +1 each after one event of each kind, and NOTHING after an R4 event.
// The label set is asserted closed on every data point in collectTimestampReplaced.
func TestIngest_TimestampReplacedCounter(t *testing.T) {
	t.Run("nothing is recorded for an R4 event", func(t *testing.T) {
		reader := installTimestampManualReader(t)
		svc, pub := newTestIngestService(t)

		payload := validIngestPayload()
		payload.Timestamp = tsRecv.Add(-time.Minute)
		require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv))
		require.Len(t, pub.published, 1)

		assert.Empty(t, collectTimestampReplaced(t, reader),
			"a verbatim (R4) timestamp must not touch %s at all", obs.MetricIngestTimestampReplaced)
	})

	t.Run("one event of each replaced kind is +1 under its own reason", func(t *testing.T) {
		reader := installTimestampManualReader(t)
		svc, pub := newTestIngestService(t)
		require.Empty(t, collectTimestampReplaced(t, reader), "the reader is this test's own: it must start empty")

		empty := validIngestPayload()
		empty.Timestamp = time.Time{}
		future := validIngestPayload()
		future.Timestamp = tsRecv.Add(service.MaxFutureSkew + time.Second)
		ancient := validIngestPayload()
		ancient.Timestamp = tsRecv.Add(-(service.MaxEventAge + time.Second))
		for _, payload := range []*validation.ErrorPayload{empty, future, ancient} {
			require.NoError(t, svc.IngestAt(context.Background(), payload, tsRecv))
		}
		require.Len(t, pub.published, 3)

		got := collectTimestampReplaced(t, reader)
		assert.Equal(t, map[string]int64{
			obs.TimestampReasonEmpty:   1,
			obs.TimestampReasonFuture:  1,
			obs.TimestampReasonAncient: 1,
		}, got, "exactly the three closed reasons, +1 each")
		t.Logf("ManualReader dump of %s after one event of each kind: %v", obs.MetricIngestTimestampReplaced, got)

		// And a second R4 event afterwards moves nothing.
		verbatim := validIngestPayload()
		verbatim.Timestamp = tsRecv
		require.NoError(t, svc.IngestAt(context.Background(), verbatim, tsRecv))
		assert.Equal(t, got, collectTimestampReplaced(t, reader), "an R4 event must not increment any reason")
	})
}

// TestIngest_WrapperUsesWallClock pins Ingest as the time.Now().UTC() wrapper over IngestAt: an
// empty timestamp through the no-clock entry point lands within the test's own window, so the
// callers that keep using Ingest (tests/integration, and this file's event_id cases) get the rule
// too rather than a silent year-1 passthrough.
func TestIngest_WrapperUsesWallClock(t *testing.T) {
	svc, pub := newTestIngestService(t)
	before := time.Now().UTC()
	payload := validIngestPayload()
	payload.Timestamp = time.Time{}

	require.NoError(t, svc.Ingest(context.Background(), payload))
	after := time.Now().UTC()

	got := publishedTimestamp(t, pub)
	assert.False(t, got.Before(before), "Ingest must resolve an empty timestamp against the wall clock, got %v before %v", got, before)
	assert.False(t, got.After(after), "Ingest must resolve an empty timestamp against the wall clock, got %v after %v", got, after)
	assert.NotEqual(t, 1, got.Year(), "year 1 must never reach the wire")
}
