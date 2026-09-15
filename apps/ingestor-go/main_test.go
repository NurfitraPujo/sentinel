package main

// The echo boundary of sentinel-ingest-timestamp-clamp (contract §11 row 3, decision 14): the REAL
// handleIngest / handleBatchIngest driven over httptest, so what is asserted is the 202 body a client
// actually receives, not a mock of the handler (service/batch_test.go builds its own inline handler
// and never reaches these; tests/e2e is an opt-in job).
//
// db is nil throughout. applyAuthenticatedScope only consults the pool for an ORGANIZATION-WIDE key
// that named no project; a project-scoped identity from auth.WithIdentity is resolved entirely from
// the request context, so the handlers run end to end without Postgres. The publisher is a capturing
// fake, exactly as tests/unit does it, so the echoed value can be compared with what was published.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

const (
	testProjectKey = "proj-handler-test"
	testProjectID  = "11111111-1111-1111-1111-111111111111"
	testOrgID      = "22222222-2222-2222-2222-222222222222"
)

// capturingPublisher satisfies service's unexported eventPublisher interface structurally and keeps
// every published message in order, so batch item i can be matched to published message i.
type capturingPublisher struct {
	published [][]byte
}

func (c *capturingPublisher) PublishWithHeaders(_ context.Context, data []byte, _ nats.Header) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	c.published = append(c.published, cp)
	return nil
}

func newHandlerTestService(t *testing.T) (*service.IngestService, *capturingPublisher) {
	t.Helper()
	pub := &capturingPublisher{}
	svc, err := service.NewIngestService(pub)
	require.NoError(t, err)
	return svc, pub
}

// authedRequest builds a POST carrying a PROJECT-SCOPED identity, the shape the auth middleware
// installs for a sent_live_ key — which is what lets applyAuthenticatedScope pass with db == nil.
func authedRequest(t *testing.T, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithIdentity(req.Context(), testProjectKey, testProjectID, testOrgID, "hash", 1000, false)
	return req.WithContext(ctx)
}

func decodePublished(t *testing.T, raw []byte) *sentinelv1.ErrorEvent {
	t.Helper()
	var event sentinelv1.ErrorEvent
	require.NoError(t, proto.Unmarshal(raw, &event))
	require.NotNil(t, event.GetTimestamp(), "proto field 10 must never be nil")
	return &event
}

// assertEchoMatchesPublished is decision 15: the wire value is time.RFC3339Nano with a "Z" suffix and
// parses back to the published proto's Timestamp.AsTime() exactly — both carry nanoseconds, so the
// comparison here is exact; the microsecond truncation belongs to the created_at comparison in
// tests/integration, where TIMESTAMP WITH TIME ZONE is the coarser side.
func assertEchoMatchesPublished(t *testing.T, echoed string, event *sentinelv1.ErrorEvent) time.Time {
	t.Helper()
	require.NotEmpty(t, echoed, "the 202 body must carry timestamp")
	assert.True(t, strings.HasSuffix(echoed, "Z"), "the echoed timestamp must be UTC with a Z suffix, got %q", echoed)
	parsed, err := time.Parse(time.RFC3339Nano, echoed)
	require.NoError(t, err, "the echoed timestamp must be RFC3339Nano, got %q", echoed)
	published := event.GetTimestamp().AsTime()
	assert.Equal(t, published.UnixNano(), parsed.UnixNano(),
		"the echoed timestamp must equal the published proto's timestamp: echoed %v, published %v", parsed, published)
	return parsed
}

const singleBodyPrefix = `{"project_key":"` + testProjectKey + `","platform":"python","environment":"test","error_class":"ValueError","message":"boom"`

func TestHandleIngest_202EchoesEffectiveTimestampEqualToPublished(t *testing.T) {
	svc, pub := newHandlerTestService(t)
	handler := handleIngest(svc, nil, slog.Default())

	// A value inside the window is stored verbatim (R4); a fixed instant would drift out of
	// MaxEventAge as the calendar moves, so post one relative to the wall clock.
	sent := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Microsecond)
	body := singleBodyPrefix + `,"timestamp":"` + sent.Format(time.RFC3339Nano) + `"}`

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest(t, "/ingest", body))

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "accepted", resp["status"])
	assert.NotEmpty(t, resp["event_id"], "event_id must be echoed (minted when absent)")

	require.Len(t, pub.published, 1)
	event := decodePublished(t, pub.published[0])
	assert.Equal(t, event.GetEventId(), resp["event_id"], "the echoed event_id must be the published one")
	echoed := assertEchoMatchesPublished(t, resp["timestamp"], event)
	// R4: a value inside the window reaches the wire, the proto and the echo untouched.
	assert.True(t, sent.Equal(echoed), "an in-window timestamp must be echoed verbatim: sent %v, echoed %v", sent, echoed)
}

func TestHandleIngest_NoTimestampEchoesReceiptTimeWithinClockWindow(t *testing.T) {
	svc, pub := newHandlerTestService(t)
	handler := handleIngest(svc, nil, slog.Default())

	before := time.Now().UTC()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest(t, "/ingest", singleBodyPrefix+`}`))
	after := time.Now().UTC()

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	require.Len(t, pub.published, 1)
	event := decodePublished(t, pub.published[0])
	echoed := assertEchoMatchesPublished(t, resp["timestamp"], event)

	// R1 (flows W1): the effective value is T_recv, read between the test's two clock readings —
	// never year 1.
	assert.NotEqual(t, 1, echoed.Year(), "an absent timestamp must not be echoed as year 1")
	assert.False(t, echoed.Before(before), "echoed %v is before the request was made (%v)", echoed, before)
	assert.False(t, echoed.After(after), "echoed %v is after the response was received (%v)", echoed, after)
}

func TestHandleIngest_FutureTimestampEchoesReceiptTime(t *testing.T) {
	svc, pub := newHandlerTestService(t)
	handler := handleIngest(svc, nil, slog.Default())

	before := time.Now().UTC()
	ahead := before.Add(365 * 24 * time.Hour)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest(t, "/ingest", singleBodyPrefix+`,"timestamp":"`+ahead.Format(time.RFC3339Nano)+`"}`))
	after := time.Now().UTC()

	require.Equal(t, http.StatusAccepted, rec.Code, "R2 is never a 400; body: %s", rec.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, pub.published, 1)
	echoed := assertEchoMatchesPublished(t, resp["timestamp"], decodePublished(t, pub.published[0]))
	assert.False(t, echoed.Before(before) || echoed.After(after),
		"R2: a timestamp a year ahead must be replaced with T_recv and echoed as such, got %v", echoed)
}

func TestHandleBatchIngest_202EchoesTimestampPerItemAndKeepsErrorsShape(t *testing.T) {
	svc, pub := newHandlerTestService(t)
	handler := handleBatchIngest(svc, nil, slog.Default())

	good := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Microsecond)
	future := time.Now().UTC().Add(48 * time.Hour)
	items := []string{
		singleBodyPrefix + `}`, // absent → R1
		singleBodyPrefix + `,"timestamp":"` + future.Format(time.RFC3339Nano) + `"}`, // R2
		singleBodyPrefix + `,"timestamp":"` + good.Format(time.RFC3339Nano) + `"}`,   // R4
		// A project-scoped key with a body naming ANOTHER project is refused by scope (403 per item):
		// it never reaches the rule and lands in errors[], which keeps its pre-existing shape.
		`{"project_key":"someone-elses-project","platform":"python","environment":"test","error_class":"ValueError"}`,
	}
	body := "[" + strings.Join(items, ",") + "]"

	before := time.Now().UTC()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, authedRequest(t, "/ingest/batch", body))
	after := time.Now().UTC()

	require.Equal(t, http.StatusAccepted, rec.Code, "body: %s", rec.Body.String())
	var resp struct {
		Ingested int `json:"ingested"`
		Failed   int `json:"failed"`
		Errors   []struct {
			Index   int    `json:"index"`
			Message string `json:"message"`
		} `json:"errors"`
		EventIDs []struct {
			Index     int    `json:"index"`
			EventID   string `json:"event_id"`
			Timestamp string `json:"timestamp"`
		} `json:"event_ids"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, 3, resp.Ingested)
	assert.Equal(t, 1, resp.Failed)
	// errors[] keeps its shape: index + message only, no timestamp on a refused item.
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, 3, resp.Errors[0].Index)
	assert.Contains(t, resp.Errors[0].Message, "does not match the authenticated project")
	var rawResp struct {
		Errors []map[string]json.RawMessage `json:"errors"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rawResp))
	assert.Equal(t, []string{"index", "message"}, sortedKeys(rawResp.Errors[0]), "errors[] items must carry exactly index and message")

	// event_ids[i].timestamp is present per ingested item and equals what was published for it —
	// the publisher captured messages in item order, and the refused item published nothing.
	require.Len(t, pub.published, 3, "three items reached the publisher")
	require.Len(t, resp.EventIDs, 3)
	var echoed []time.Time
	for i, item := range resp.EventIDs {
		assert.Equal(t, i, item.Index)
		event := decodePublished(t, pub.published[i])
		assert.Equal(t, event.GetEventId(), item.EventID, "item %d: event_id must be the published one", i)
		echoed = append(echoed, assertEchoMatchesPublished(t, item.Timestamp, event))
	}

	// Decision 3: the absent and the future item share ONE T_recv, inside the test's clock window;
	// the good item is verbatim.
	assert.True(t, echoed[0].Equal(echoed[1]), "R1 and R2 items of one batch must share one T_recv: %v vs %v", echoed[0], echoed[1])
	assert.False(t, echoed[0].Before(before) || echoed[0].After(after), "T_recv %v must lie within [%v, %v]", echoed[0], before, after)
	assert.True(t, good.Equal(echoed[2]), "the in-window item must be echoed verbatim: sent %v, echoed %v", good, echoed[2])
}

// sortedKeys returns a map's keys in lexical order, for shape assertions on decoded JSON objects.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
