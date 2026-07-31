package unit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
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
