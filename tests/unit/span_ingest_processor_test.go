package unit

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/spanpull"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/spans"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

// sentinel-span-ingest D1/D3/D7/D9: the processor half's pure logic — the masker and the 8 KiB cap, the partition
// window, the configuration guard, and the two trace-store adapters against stub servers. The database halves are
// in tests/integration/span_ingest_test.go.

func TestSpanPrepareMasksCapsAndWindows(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	base := spanwire.Span{TraceID: spanTrace, SpanID: spanRoot, StartTS: now, DurationUS: 5}

	t.Run("D7: sensitive keys and secrets in values and the name are masked", func(t *testing.T) {
		sp := base
		sp.Name = "GET https://x.invalid/a?token=abcdefghijklmnopqrstuvwxyz0123"
		sp.Attributes = map[string]any{"authorization": "Bearer abc.def", "nested": map[string]any{"password": "hunter2hunter2"},
			"url": "https://x.invalid/?api_key=abcdefghijklmnopqrstuvwxyz", "status": 200}
		out, reason := spans.Prepare(sp, now)
		require.Empty(t, reason)
		assert.Equal(t, "***REDACTED***", out.Attributes["authorization"])
		assert.Equal(t, "***REDACTED***", out.Attributes["nested"].(map[string]any)["password"])
		assert.NotContains(t, out.Attributes["url"], "abcdefghijklmnopqrstuvwxyz")
		assert.NotContains(t, out.Name, "abcdefghijklmnopqrstuvwxyz0123")
		assert.EqualValues(t, 200, out.Attributes["status"])
	})
	t.Run("D7: attributes are capped at 8 KiB, largest values first, marked", func(t *testing.T) {
		sp := base
		sp.Attributes = map[string]any{"keep": "me", "blob": strings.Repeat("z", 50_000)}
		out, reason := spans.Prepare(sp, now)
		require.Empty(t, reason)
		b, _ := json.Marshal(out.Attributes)
		assert.LessOrEqual(t, len(b), spanwire.MaxAttributeBytes)
		assert.Equal(t, "me", out.Attributes["keep"])
		assert.Contains(t, out.Attributes[spanwire.TruncatedKey], "blob")
	})
	t.Run("D1: a span outside the created partition window is dropped, never defaulted", func(t *testing.T) {
		for name, ts := range map[string]time.Time{
			"two days back":  now.AddDate(0, 0, -2),
			"fifteen days":   now.AddDate(0, 0, -15),
			"three days out": now.AddDate(0, 0, 3),
			"zero":           {},
		} {
			sp := base
			sp.StartTS = ts
			_, reason := spans.Prepare(sp, now)
			assert.Equal(t, spans.DropOutOfWindow, reason, name)
		}
		for name, ts := range map[string]time.Time{
			"start of yesterday": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
			"end of day+2":       time.Date(2026, 10, 4, 23, 59, 59, 0, time.UTC),
		} {
			sp := base
			sp.StartTS = ts
			_, reason := spans.Prepare(sp, now)
			assert.Empty(t, reason, name)
		}
	})
	t.Run("a malformed id is dropped", func(t *testing.T) {
		sp := base
		sp.SpanID = "xyz"
		_, reason := spans.Prepare(sp, now)
		assert.Equal(t, spans.DropInvalid, reason)
	})
}

func TestSpanPullConfig(t *testing.T) {
	look := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	org := "11111111-1111-4111-8111-111111111111"
	core := "33333333-3333-4333-8333-333333333333"

	cfg, err := spanpull.ParseConfig(look(nil))
	require.NoError(t, err)
	assert.False(t, cfg.Enabled(), "unset = off")

	cfg, err = spanpull.ParseConfig(look(map[string]string{"TRACE_STORE_KIND": "tempo", "TRACE_STORE_URL": "http://t:3100"}))
	require.NoError(t, err)
	assert.False(t, cfg.Enabled(), "no scope map = off")

	cfg, err = spanpull.ParseConfig(look(map[string]string{"SPAN_PULL_SCOPES": strings.ToUpper(org) + ":" + core, "TRACE_STORE_KIND": "Tempo", "TRACE_STORE_URL": "http://t:3100/"}))
	require.NoError(t, err)
	assert.True(t, cfg.Enabled())
	assert.Equal(t, "http://t:3100", cfg.URL)
	assert.False(t, cfg.Allowed(org), "BUG-01: nothing is allowed until the scopes have been validated against the database")
	assert.Empty(t, cfg.ScopeProject(org))
	_, refused, err := cfg.ValidateAgainstDB(context.Background(), fakeProjects{core: org})
	require.NoError(t, err)
	assert.Zero(t, refused)
	assert.True(t, cfg.Allowed(org))
	assert.True(t, cfg.Allowed(strings.ToUpper(org)))
	assert.Equal(t, core, cfg.ScopeProject(org), "the mapped core project governs the org's pulled spans")
	assert.False(t, cfg.Allowed("22222222-2222-4222-8222-222222222222"), "D3: another org is refused")
	assert.Empty(t, cfg.ScopeProject("22222222-2222-4222-8222-222222222222"))

	two, err := spanpull.ParseConfig(look(map[string]string{"SPAN_PULL_SCOPES": org + ":" + core + ", 22222222-2222-4222-8222-222222222222:" + core, "TRACE_STORE_KIND": "jaeger", "TRACE_STORE_URL": "http://t"}))
	require.NoError(t, err)
	assert.Len(t, two.Scopes, 2)

	for name, bad := range map[string]string{
		"an org slug":      "example:" + core,
		"no project":       org,
		"a project slug":   org + ":core",
		"the old org list": org,
		"a trailing colon": org + ":",
	} {
		_, err = spanpull.ParseConfig(look(map[string]string{"SPAN_PULL_SCOPES": bad, "TRACE_STORE_KIND": "tempo", "TRACE_STORE_URL": "http://t"}))
		assert.Error(t, err, name+": a typo stops the process rather than pulling nothing")
	}
	_, err = spanpull.ParseConfig(look(map[string]string{"SPAN_PULL_SCOPES": org + ":" + core, "TRACE_STORE_KIND": "zipkin", "TRACE_STORE_URL": "http://t"}))
	assert.Error(t, err)
}

const tempoOTLP = `{"batches":[
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"core-api"}},{"key":"environment","value":{"stringValue":"production"}}]},
  "scopeSpans":[{"spans":[
   {"traceId":"5b8efff798038103d269b633813fc60c","spanId":"00f067aa0ba902b7","parentSpanId":"","name":"POST /orders","kind":"SPAN_KIND_SERVER",
    "startTimeUnixNano":"1790329310000000000","endTimeUnixNano":"1790329310500000000",
    "attributes":[{"key":"http.status_code","value":{"intValue":"500"}},{"key":"http.route","value":{"stringValue":"/orders"}},{"key":"authorization","value":{"stringValue":"Bearer s3cr3t-token-value"}}],
    "status":{"code":"STATUS_CODE_ERROR"}},
   {"traceId":"5b8efff798038103d269b633813fc60c","spanId":"b7ad6b7169203331","parentSpanId":"00f067aa0ba902b7","name":"SELECT orders","kind":2,
    "startTimeUnixNano":"1790329310100000000","endTimeUnixNano":"1790329310200000000","attributes":[],"status":{"code":1}},
   {"traceId":"ffffffffffffffffffffffffffffffff","spanId":"1111111111111111","name":"another trace","startTimeUnixNano":"1790329310100000000"}]}]},
 {"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"core-worker"}}]},
  "instrumentationLibrarySpans":[{"spans":[
   {"traceId":"` + "W47/95gDgQPSabYzgT/GDA==" + `","spanId":"` + "Gis8TV5vcIE=" + `","parentSpanId":"` + "ALhn6nXQ1Sg=" + `","name":"consume","kind":5,
    "startTimeUnixNano":1790329310300000000,"endTimeUnixNano":1790329310400000000}]}]}]}`

func tempoServer(t *testing.T, status int, body string, gotPath *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotPath != nil {
			*gotPath = r.URL.Path + "|" + r.Header.Get("Accept")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestSpanPullTempoAdapter(t *testing.T) {
	var got string
	srv := tempoServer(t, 200, tempoOTLP, &got)
	defer srv.Close()
	store, err := spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindTempo, URL: srv.URL}, srv.Client())
	require.NoError(t, err)

	out, err := store.Fetch(context.Background(), spanTrace)
	require.NoError(t, err)
	assert.Equal(t, "/api/traces/"+spanTrace+"|application/json", got, "D3: Tempo GET /api/traces/{id} with Accept: application/json")
	require.Len(t, out, 3, "the span of another trace id is skipped")

	by := map[string]spanwire.Span{}
	for _, s := range out {
		assert.Equal(t, spanTrace, s.TraceID)
		by[s.SpanID] = s
	}
	root := by["00f067aa0ba902b7"]
	assert.Equal(t, "core-api", root.ServiceName, "service_name from resource service.name")
	assert.Equal(t, "production", root.Environment, "environment from the resource attribute")
	assert.Equal(t, "server", root.Kind)
	assert.Equal(t, "error", root.Status)
	assert.Equal(t, int64(500_000), root.DurationUS)
	assert.Equal(t, "", root.ParentSpanID)
	assert.EqualValues(t, 500, root.Attributes["http.status_code"])
	child := by["b7ad6b7169203331"]
	assert.Equal(t, "00f067aa0ba902b7", child.ParentSpanID, "parentage")
	assert.Equal(t, "ok", child.Status)
	assert.Equal(t, "server", child.Kind, "numeric enum 2 = server")
	// The base64-spelled ids (protobuf-JSON bytes) of the older instrumentationLibrarySpans shape normalise to hex.
	raw, _ := base64.StdEncoding.DecodeString("Gis8TV5vcIE=")
	w := by[hex.EncodeToString(raw)] // 1a2b3c4d5e6f7081
	assert.Equal(t, "core-worker", w.ServiceName)
	assert.Equal(t, "consumer", w.Kind)
	assert.Equal(t, "00b867ea75d0d528", w.ParentSpanID)

	srv404 := tempoServer(t, 404, `trace not found`, nil)
	defer srv404.Close()
	nf, _ := spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindTempo, URL: srv404.URL}, srv404.Client())
	_, err = nf.Fetch(context.Background(), spanTrace)
	assert.ErrorIs(t, err, spanpull.ErrNotFound)

	// m3: a response over the read bound is ErrTooLarge (not retried), never an unbounded read.
	srvBig := tempoServer(t, 200, `{"batches":[],"pad":"`+strings.Repeat("x", 33<<20)+`"}`, nil)
	defer srvBig.Close()
	big, _ := spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindTempo, URL: srvBig.URL}, srvBig.Client())
	_, err = big.Fetch(context.Background(), spanTrace)
	assert.ErrorIs(t, err, spanpull.ErrTooLarge)

	srv500 := tempoServer(t, 503, `x`, nil)
	defer srv500.Close()
	bad, _ := spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindTempo, URL: srv500.URL}, srv500.Client())
	_, err = bad.Fetch(context.Background(), spanTrace)
	require.Error(t, err)
	assert.NotErrorIs(t, err, spanpull.ErrNotFound)
}

const jaegerJSON = `{"data":[{"traceID":"5b8efff798038103d269b633813fc60c",
 "spans":[
  {"traceID":"5b8efff798038103d269b633813fc60c","spanID":"00f067aa0ba902b7","operationName":"POST /orders","references":[],
   "startTime":1790329310000000,"duration":500000,"processID":"p1",
   "tags":[{"key":"span.kind","type":"string","value":"server"},{"key":"http.status_code","type":"int64","value":500},{"key":"otel.status_code","type":"string","value":"ERROR"},{"key":"otel.library.name","type":"string","value":"otelgin"}]},
  {"traceID":"5b8efff798038103d269b633813fc60c","spanID":"b7ad6b7169203331","operationName":"SELECT","references":[{"refType":"CHILD_OF","traceID":"5b8efff798038103d269b633813fc60c","spanID":"00f067aa0ba902b7"}],
   "startTime":1790329310100000,"duration":100000,"processID":"p2","tags":[{"key":"error","type":"bool","value":true}]},
  {"traceID":"5b8efff798038103d269b633813fc60c","spanID":"1212121212121212","operationName":"no status","references":[],"startTime":1790329310200000,"duration":1,"processID":"p2","tags":[]}],
 "processes":{"p1":{"serviceName":"core-api","tags":[{"key":"environment","type":"string","value":"production"}]},"p2":{"serviceName":"core-db","tags":[]}}}]}`

func TestSpanPullJaegerAdapter(t *testing.T) {
	var got string
	srv := tempoServer(t, 200, jaegerJSON, &got)
	defer srv.Close()
	store, err := spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindJaeger, URL: srv.URL}, srv.Client())
	require.NoError(t, err)
	out, err := store.Fetch(context.Background(), spanTrace)
	require.NoError(t, err)
	assert.Equal(t, "/select/jaeger/api/traces/"+spanTrace+"|application/json", got, "D3: VictoriaTraces Jaeger API")
	require.Len(t, out, 3)
	by := map[string]spanwire.Span{}
	for _, s := range out {
		by[s.SpanID] = s
	}
	root := by["00f067aa0ba902b7"]
	assert.Equal(t, "core-api", root.ServiceName)
	assert.Equal(t, "production", root.Environment)
	assert.Equal(t, "server", root.Kind)
	assert.Equal(t, "error", root.Status)
	assert.Equal(t, int64(500_000), root.DurationUS)
	assert.EqualValues(t, 500, root.Attributes["http.status_code"])
	assert.NotContains(t, root.Attributes, "otel.library.name", "internal tags are mapped, not copied")
	assert.Equal(t, int64(1790329310), root.StartTS.Unix())
	child := by["b7ad6b7169203331"]
	assert.Equal(t, "00f067aa0ba902b7", child.ParentSpanID, "CHILD_OF reference = parent")
	assert.Equal(t, "error", child.Status, "an error=true tag is an error")
	assert.Equal(t, "core-db", child.ServiceName)
	assert.Equal(t, "unset", by["1212121212121212"].Status)
}

// fakeProjects answers the validation query from a project id -> organization id map; a missing id is no row.
type fakeProjects map[string]string

type fakeRow struct {
	org string
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(**string)) = &r.org
	return nil
}

func (f fakeProjects) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	if org, ok := f[args[0].(string)]; ok {
		return fakeRow{org: org}
	}
	return fakeRow{err: pgx.ErrNoRows}
}

type brokenDB struct{}

func (brokenDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeRow{err: errors.New("connection refused")}
}

func TestSpanPullConfig_DuplicateOrgIsRefusedAndOthersKept(t *testing.T) {
	a, b := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	p1, p2, p3 := "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"
	env := map[string]string{"SPAN_PULL_SCOPES": a + ":" + p1 + "," + b + ":" + p3 + "," + a + ":" + p2, "TRACE_STORE_KIND": "tempo", "TRACE_STORE_URL": "http://t"}
	cfg, err := spanpull.ParseConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err, "a duplicate must not stop the process: it also ingests errors")
	assert.NotContains(t, cfg.Scopes, a, "the duplicated org gets no entry, not the last value")
	assert.Equal(t, p3, cfg.Scopes[b])
	assert.Equal(t, 1, cfg.Duplicated)

	_, _, err = cfg.ValidateAgainstDB(context.Background(), fakeProjects{p1: a, p2: a, p3: b})
	require.NoError(t, err)
	assert.False(t, cfg.Allowed(a))
	assert.True(t, cfg.Allowed(b))
}

func TestSpanPullConfig_ValidationDropsForeignAndUnknownProjects(t *testing.T) {
	a, b, c := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "66666666-6666-4666-8666-666666666666"
	pa, pb, ghost := "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555"
	env := map[string]string{"SPAN_PULL_SCOPES": a + ":" + pa + "," + b + ":" + pa + "," + c + ":" + ghost, "TRACE_STORE_KIND": "tempo", "TRACE_STORE_URL": "http://t"}
	cfg, err := spanpull.ParseConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)

	_, _, err = cfg.ValidateAgainstDB(context.Background(), brokenDB{})
	require.Error(t, err)
	assert.False(t, cfg.Allowed(a), "a failed validation leaves everything refused")

	accepted, refused, err := cfg.ValidateAgainstDB(context.Background(), fakeProjects{pa: a, pb: b})
	require.NoError(t, err)
	assert.Equal(t, 1, accepted)
	assert.Equal(t, 2, refused)
	assert.True(t, cfg.Allowed(a))
	assert.False(t, cfg.Allowed(b), "b is mapped to a's project")
	assert.False(t, cfg.Allowed(c), "unknown project")
}

func TestSpanPullConfig_ValidatedOnlyAfterTheGateIsPublished(t *testing.T) {
	a, pa := "11111111-1111-4111-8111-111111111111", "33333333-3333-4333-8333-333333333333"
	env := map[string]string{"SPAN_PULL_SCOPES": a + ":" + pa, "TRACE_STORE_KIND": "tempo", "TRACE_STORE_URL": "http://t"}
	cfg, err := spanpull.ParseConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	assert.False(t, cfg.Validated())
	_, _, _ = cfg.ValidateAgainstDB(context.Background(), brokenDB{})
	assert.False(t, cfg.Validated(), "a failed validation publishes nothing")
	_, _, err = cfg.ValidateAgainstDB(context.Background(), fakeProjects{})
	require.NoError(t, err)
	assert.True(t, cfg.Validated(), "published, even if every entry was refused")
	assert.False(t, cfg.Allowed(a))
}

func TestSpanPullErrorExcerptIsValidUTF8(t *testing.T) {
	srv := tempoServer(t, 502, "bad \xff\xfe bytes", nil)
	defer srv.Close()
	st, _ := spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindTempo, URL: srv.URL}, srv.Client())
	_, err := st.Fetch(context.Background(), spanTrace)
	require.Error(t, err)
	assert.True(t, utf8.ValidString(err.Error()))

	nul := tempoServer(t, 502, "proxy error \x00\x01 binary\nline2", nil)
	defer nul.Close()
	st, _ = spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindTempo, URL: nul.URL}, nul.Client())
	_, err = st.Fetch(context.Background(), spanTrace)
	require.Error(t, err)
	assert.Equal(t, "trace store answered HTTP 502: proxy error binary line2", err.Error()[:len("trace store answered HTTP 502: proxy error binary line2")])
	assert.NotContains(t, err.Error(), "\x00")
	assert.NotContains(t, err.Error(), "\n")
}

func TestSpanPullTempoTooLargeIsFinalAndOtherErrorsCarryTheBody(t *testing.T) {
	big := tempoServer(t, 500, "trace exceeds max size (max bytes: 5000000)\n", nil)
	defer big.Close()
	st, _ := spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindTempo, URL: big.URL}, big.Client())
	_, err := st.Fetch(context.Background(), spanTrace)
	assert.ErrorIs(t, err, spanpull.ErrTooLarge)

	other := tempoServer(t, 500, "boom\n"+strings.Repeat("y", 500), nil)
	defer other.Close()
	st, _ = spanpull.NewTraceStore(spanpull.Config{Kind: spanpull.KindTempo, URL: other.URL}, other.Client())
	_, err = st.Fetch(context.Background(), spanTrace)
	require.Error(t, err)
	assert.NotErrorIs(t, err, spanpull.ErrTooLarge)
	assert.Contains(t, err.Error(), "HTTP 500: boom yyy")
	assert.NotContains(t, err.Error(), "\n")
	assert.Less(t, len(err.Error()), 260, "the body excerpt is at most 200 characters")
}
