package unit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

// sentinel-span-ingest D2/D9: the compat endpoint turns `transaction` items into span rows and publishes them to the
// SPANS stream. The fixture is a sentry_flutter 9.6.0 Dio request transaction (a root and three child spans).

const (
	spanFixture = "envelopes/flutter-9.6.0-transaction.envelope"
	spanTrace   = "5b8efff798038103d269b633813fc60c"
	spanRoot    = "00f067aa0ba902b7"
	spanOrg     = "11111111-1111-4111-8111-111111111111"
	spanProject = "22222222-2222-4222-8222-222222222222"
)

func spanFixtureItem(t *testing.T) []byte {
	t.Helper()
	env := scReadEnv(t, scReadFixture(t, spanFixture))
	require.Len(t, env.Items, 1)
	require.Equal(t, "transaction", env.Items[0].Type)
	require.NotEmpty(t, env.Items[0].Payload)
	return env.Items[0].Payload
}

// TASK-02 criterion: a sentry_flutter transaction envelope yields its spans with correct parentage.
func TestSpanTransactionYieldsSpansWithParentage(t *testing.T) {
	res, err := sentrycompat.ParseTransaction(spanFixtureItem(t))
	require.NoError(t, err)
	require.Len(t, res.Spans, 4)
	assert.Zero(t, res.Truncated)
	assert.Zero(t, res.Invalid)
	assert.Equal(t, "production", res.Environment)

	by := map[string]spanwire.Span{}
	for _, s := range res.Spans {
		assert.Equal(t, spanTrace, s.TraceID, "every span carries the transaction's trace id")
		by[s.SpanID] = s
	}
	root := res.Spans[0]
	assert.Equal(t, spanRoot, root.SpanID, "the root comes first")
	assert.Equal(t, "1111111111111111", root.ParentSpanID, "the root keeps the upstream parent (the caller's span)")
	assert.Equal(t, "GET /v1/orders", root.Name)
	assert.Equal(t, "http.client", root.Op)
	assert.Equal(t, "ok", root.Status)
	assert.Equal(t, int64(750_000), root.DurationUS, "timestamp - start_timestamp, in microseconds")
	assert.Equal(t, int64(1790329310), root.StartTS.Unix())
	assert.Equal(t, 250_000, root.StartTS.Nanosecond()/1000)
	assert.Equal(t, "production", root.Environment)

	assert.Equal(t, spanRoot, by["a1a1a1a1a1a1a1a1"].ParentSpanID, "a child of the root")
	assert.Equal(t, "a1a1a1a1a1a1a1a1", by["b2b2b2b2b2b2b2b2"].ParentSpanID, "a grandchild")
	assert.Equal(t, spanRoot, by["c3c3c3c3c3c3c3c3"].ParentSpanID)
	assert.Equal(t, "internal_error", by["c3c3c3c3c3c3c3c3"].Status)
	assert.Equal(t, int64(600_000), by["a1a1a1a1a1a1a1a1"].DurationUS)
	assert.Equal(t, "serialize", by["b2b2b2b2b2b2b2b2"].Op)
	assert.Equal(t, "decode body", by["b2b2b2b2b2b2b2b2"].Name)
	assert.EqualValues(t, 200, by["a1a1a1a1a1a1a1a1"].Attributes["http.response.status_code"])
	assert.Equal(t, map[string]any{"http.method": "GET", "http.status_code": "200"}, root.Attributes["tags"])
}

func TestSpanTransactionCapAndInvalidSpans(t *testing.T) {
	mk := func(n int, mut func(i int, sp map[string]any)) []byte {
		var spans []map[string]any
		for i := 0; i < n; i++ {
			sp := map[string]any{"span_id": fmt.Sprintf("%016x", i+1), "parent_span_id": spanRoot, "op": "x",
				"start_timestamp": 1790329310.5, "timestamp": 1790329310.6}
			if mut != nil {
				mut(i, sp)
			}
			spans = append(spans, sp)
		}
		b, _ := json.Marshal(map[string]any{"transaction": "t", "start_timestamp": 1790329310.0, "timestamp": 1790329311.0,
			"contexts": map[string]any{"trace": map[string]any{"trace_id": spanTrace, "span_id": spanRoot}}, "spans": spans})
		return b
	}

	t.Run("D2: at most 1,000 spans per transaction, the excess counted", func(t *testing.T) {
		res, err := sentrycompat.ParseTransaction(mk(1500, nil))
		require.NoError(t, err)
		assert.Len(t, res.Spans, spanwire.MaxSpansPerTransaction)
		assert.Equal(t, 1500-(spanwire.MaxSpansPerTransaction-1), res.Truncated)
		assert.Equal(t, spanRoot, res.Spans[0].SpanID)
	})
	t.Run("a child naming another trace, a duplicate id, a bad id and a bad timestamp are skipped and counted", func(t *testing.T) {
		res, err := sentrycompat.ParseTransaction(mk(5, func(i int, sp map[string]any) {
			switch i {
			case 0:
				sp["trace_id"] = strings.Repeat("a", 32)
			case 1:
				sp["span_id"] = spanRoot // duplicates the root
			case 2:
				sp["span_id"] = "not-hex"
			case 3:
				sp["timestamp"] = nil
			}
		}))
		require.NoError(t, err)
		assert.Len(t, res.Spans, 2, "the root and the one good child")
		assert.Equal(t, 4, res.Invalid)
	})
	t.Run("RFC 3339 timestamps are read", func(t *testing.T) {
		res, err := sentrycompat.ParseTransaction(mk(1, func(_ int, sp map[string]any) {
			sp["start_timestamp"], sp["timestamp"] = "2026-09-25T09:41:53.500Z", "2026-09-25T09:41:53.750Z"
		}))
		require.NoError(t, err)
		assert.Equal(t, int64(250_000), res.Spans[1].DurationUS)
	})
	t.Run("an unusable root is an error, never a partial transaction", func(t *testing.T) {
		for name, raw := range map[string]string{
			"not json":       `[1,2`,
			"no trace":       `{"start_timestamp":1790329310,"timestamp":1790329311,"contexts":{"trace":{"span_id":"00f067aa0ba902b7"}}}`,
			"zero trace":     `{"start_timestamp":1790329310,"timestamp":1790329311,"contexts":{"trace":{"trace_id":"00000000000000000000000000000000","span_id":"00f067aa0ba902b7"}}}`,
			"no timestamps":  `{"contexts":{"trace":{"trace_id":"5b8efff798038103d269b633813fc60c","span_id":"00f067aa0ba902b7"}}}`,
			"uppercase hex":  `{"start_timestamp":1790329310,"timestamp":1790329311,"contexts":{"trace":{"trace_id":"5B8EFFF798038103D269B633813FC60C","span_id":"00f067aa0ba902b7"}}}`,
			"negative times": `{"start_timestamp":-5,"timestamp":-1,"contexts":{"trace":{"trace_id":"5b8efff798038103d269b633813fc60c","span_id":"00f067aa0ba902b7"}}}`,
		} {
			_, err := sentrycompat.ParseTransaction([]byte(raw))
			assert.ErrorIs(t, err, sentrycompat.ErrBadTransaction, name)
		}
	})
	t.Run("NUL bytes never reach Postgres text or jsonb", func(t *testing.T) {
		res, err := sentrycompat.ParseTransaction(mk(1, func(_ int, sp map[string]any) {
			sp["description"] = "a\x00b"
			sp["data"] = map[string]any{"k\x00": "v\x00"}
			sp["tags"] = map[string]any{"t": "x\x00y"}
		}))
		require.NoError(t, err)
		b, _ := json.Marshal(res.Spans[1])
		assert.NotContains(t, string(b), `\u0000`)
		assert.Equal(t, "ab", res.Spans[1].Name)
	})
}

type spanPub struct {
	msgs [][]byte
	err  error
}

func (p *spanPub) publish(_ context.Context, m []byte) error {
	if p.err != nil {
		return p.err
	}
	p.msgs = append(p.msgs, m)
	return nil
}

type spanCounts struct{ got map[string]int }

func (c *spanCounts) record(_ context.Context, outcome string, n int) {
	if c.got == nil {
		c.got = map[string]int{}
	}
	c.got[outcome] += n
}

func spanHandler(pub *spanPub, rec *scRecorder, sc *spanCounts, f *scFakeIngester, identity func(context.Context) (string, string, bool)) http.Handler {
	if identity == nil {
		identity = func(context.Context) (string, string, bool) { return spanOrg, spanProject, true }
	}
	cfg := sentrycompat.Config{Ingest: f, Scope: scScope, RecordItem: rec.record, Identity: identity, RecordSpans: sc.record}
	if pub != nil {
		cfg.Spans = pub.publish
	}
	return sentrycompat.NewEnvelopeHandler(cfg)
}

func TestSpanEnvelopeHandlerPublishesTransactions(t *testing.T) {
	body := scReadFixture(t, spanFixture)

	t.Run("a transaction envelope is published to SPANS with the credential's tenant and counted stored", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{}, &scRecorder{}, &spanCounts{}, &scFakeIngester{}
		w := scPost(spanHandler(pub, rec, sc, f, nil), "/api/1/envelope/", body, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, pub.msgs, 1)
		got, err := spanwire.Decode(pub.msgs[0])
		require.NoError(t, err)
		assert.Equal(t, spanOrg, got.OrgID)
		assert.Equal(t, spanProject, got.ProjectID)
		assert.Equal(t, spanwire.SourceMobile, got.Source)
		assert.Len(t, got.Spans, 4)
		assert.Equal(t, 1, rec.count("transaction", "stored"))
		assert.Equal(t, 4, sc.got["stored"])
		assert.Zero(t, f.calls, "no event in the envelope")
		assert.Less(t, len(pub.msgs[0]), spanwire.MaxMessageBytes)
	})

	t.Run("tenancy comes from the credential, never the body", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{}, &scRecorder{}, &spanCounts{}, &scFakeIngester{}
		w := scPost(spanHandler(pub, rec, sc, f, func(context.Context) (string, string, bool) {
			return "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", true
		}), "/api/1/envelope/", body, nil)
		require.Equal(t, http.StatusOK, w.Code)
		got, _ := spanwire.Decode(pub.msgs[0])
		assert.Equal(t, "33333333-3333-4333-8333-333333333333", got.OrgID)
		assert.Equal(t, "44444444-4444-4444-8444-444444444444", got.ProjectID)
	})

	t.Run("m5: an organization-wide key that named no project is rejected and counted, nothing published", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{}, &scRecorder{}, &spanCounts{}, &scFakeIngester{}
		w := scPost(spanHandler(pub, rec, sc, f, func(context.Context) (string, string, bool) {
			return "33333333-3333-4333-8333-333333333333", "", true
		}), "/api/1/envelope/", body, nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, pub.msgs, "its spans would be invisible forever")
		assert.Equal(t, 1, rec.count("transaction", "rejected"))
	})

	t.Run("span ingest off: the item is discarded and counted exactly as before", func(t *testing.T) {
		rec, sc, f := &scRecorder{}, &spanCounts{}, &scFakeIngester{}
		w := scPost(spanHandler(nil, rec, sc, f, nil), "/api/1/envelope/", body, nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, 1, rec.count("transaction", "discarded"))
		assert.Zero(t, rec.count("transaction", "stored"))
	})

	t.Run("no authenticated organization: rejected, nothing published", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{}, &scRecorder{}, &spanCounts{}, &scFakeIngester{}
		w := scPost(spanHandler(pub, rec, sc, f, func(context.Context) (string, string, bool) { return "", "", false }), "/api/1/envelope/", body, nil)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Empty(t, pub.msgs)
		assert.Equal(t, 1, rec.count("transaction", "rejected"))
	})

	t.Run("a malformed transaction is rejected and does not fail the request or the events beside it", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{}, &scRecorder{}, &spanCounts{}, &scFakeIngester{stamp: "id1"}
		bad := scItem{header: map[string]any{"type": "transaction"}, payload: []byte(`{"nope":1}`)}
		w := scPost(spanHandler(pub, rec, sc, f, nil), "/api/1/envelope/", scEnvelope(map[string]any{}, scEvent([]byte(`{"message":"m"}`)), bad), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, map[string]any{"id": "id1"}, scBody(t, w))
		assert.Equal(t, 1, rec.count("transaction", "rejected"))
		assert.Equal(t, 1, rec.count("event", "accepted"))
		assert.Empty(t, pub.msgs)
	})

	t.Run("a publish failure is a 500 so the client retries, and is counted failed", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{err: errors.New("nats down")}, &scRecorder{}, &spanCounts{}, &scFakeIngester{}
		w := scPost(spanHandler(pub, rec, sc, f, nil), "/api/1/envelope/", body, nil)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
		assert.Equal(t, 1, rec.count("transaction", "failed"))
		assert.Zero(t, sc.got["stored"])
	})

	t.Run("errors first: a failing event stops the envelope and its transactions are skipped, unpublished", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{}, &scRecorder{}, &spanCounts{}, &scFakeIngester{err: errors.New("validation failed: boom")}
		tx := scItem{header: map[string]any{"type": "transaction"}, payload: spanFixtureItem(t)}
		w := scPost(spanHandler(pub, rec, sc, f, nil), "/api/1/envelope/", scEnvelope(map[string]any{}, tx, scEvent([]byte(`{"message":"m"}`))), nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Empty(t, pub.msgs)
		assert.Equal(t, 1, rec.count("transaction", "skipped"))
	})

	t.Run("an oversized transaction item is read past and counted too_large", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{}, &scRecorder{}, &spanCounts{}, &scFakeIngester{}
		huge := scItem{header: map[string]any{"type": "transaction"}, payload: bytes.Repeat([]byte("x"), sentrycompat.MaxEventItemBytes+1)}
		w := scPost(spanHandler(pub, rec, sc, f, nil), "/api/1/envelope/", scEnvelope(map[string]any{}, huge), nil)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, 1, rec.count("transaction", "too_large"))
		assert.Empty(t, pub.msgs)
	})

	t.Run("up to 50 transactions in one envelope share one message budget", func(t *testing.T) {
		pub, rec, sc, f := &spanPub{}, &scRecorder{}, &spanCounts{}, &scFakeIngester{}
		var items []scItem
		for i := 0; i < 50; i++ {
			items = append(items, scItem{header: map[string]any{"type": "transaction"}, payload: spanFixtureItem(t)})
		}
		w := scPost(spanHandler(pub, rec, sc, f, nil), "/api/1/envelope/", scEnvelope(map[string]any{}, items...), nil)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, 50, rec.count("transaction", "stored"))
		for _, m := range pub.msgs {
			assert.Less(t, len(m), spanwire.MaxMessageBytes, "D2: messages < 1 MiB")
		}
	})
}

func TestSpanwireEncodeChunksAndDecodes(t *testing.T) {
	big := strings.Repeat("v", 6000)
	var spans []spanwire.Span
	for i := 0; i < 400; i++ {
		spans = append(spans, spanwire.Span{TraceID: spanTrace, SpanID: fmt.Sprintf("%016x", i+1), Name: big})
	}
	msgs, err := spanwire.Encode(spanwire.Batch{OrgID: spanOrg, Source: spanwire.SourceMobile, Spans: spans})
	require.NoError(t, err)
	require.Greater(t, len(msgs), 1, "400 x 6 KB cannot fit one 900 KB message")
	total := 0
	for _, m := range msgs {
		assert.LessOrEqual(t, len(m), spanwire.MaxMessageBytes)
		b, err := spanwire.Decode(m)
		require.NoError(t, err)
		assert.Equal(t, spanOrg, b.OrgID)
		total += len(b.Spans)
	}
	assert.Equal(t, 400, total, "no span lost across chunks")

	_, err = spanwire.Encode(spanwire.Batch{Spans: []spanwire.Span{{TraceID: spanTrace, SpanID: "0000000000000001", Name: strings.Repeat("x", spanwire.MaxMessageBytes)}}})
	assert.Error(t, err, "a single span over the budget is an error, never truncated silently")
}

func TestSpanwireCapAttributes(t *testing.T) {
	attrs := map[string]any{"small": "ok", "huge": strings.Repeat("x", 20_000), "n": 7, "nested": map[string]any{"a": strings.Repeat("y", 9_000)}}
	got := spanwire.CapAttributes(attrs, spanwire.MaxAttributeBytes)
	b, _ := json.Marshal(got)
	assert.LessOrEqual(t, len(b), spanwire.MaxAttributeBytes)
	assert.Equal(t, "ok", got["small"], "small values survive; the largest are cut first")
	assert.EqualValues(t, 7, got["n"])
	assert.Contains(t, got[spanwire.TruncatedKey], "huge", "cut keys are marked")
	assert.Len(t, attrs["huge"], 20_000, "the input is never mutated")

	small := map[string]any{"a": "b"}
	assert.Equal(t, small, spanwire.CapAttributes(small, spanwire.MaxAttributeBytes))
	assert.NotContains(t, spanwire.CapAttributes(small, spanwire.MaxAttributeBytes), spanwire.TruncatedKey)

	many := map[string]any{}
	for i := 0; i < 2000; i++ {
		many[fmt.Sprintf("key-%04d", i)] = "v"
	}
	b, _ = json.Marshal(spanwire.CapAttributes(many, spanwire.MaxAttributeBytes))
	assert.LessOrEqual(t, len(b), spanwire.MaxAttributeBytes, "many tiny keys are dropped until the map fits")
}
