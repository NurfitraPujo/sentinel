package unit

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

// ---- harness ----

const (
	scKey      = "sent_live_" + "0000000000000000000000000000000000000000000000000000000000000001"
	scOrgKey   = "sent_org_" + "0000000000000000000000000000000000000000000000000000000000000002"
	scAdminKey = "sent_live_" + "0000000000000000000000000000000000000000000000000000000000000003"
	scReadKey  = "sent_live_" + "0000000000000000000000000000000000000000000000000000000000000004"
	scAgentKey = "sent_agent_" + "000000000000000000000000000000000000000000000000000000000000005"
	scRepKey   = "sent_live_" + "0000000000000000000000000000000000000000000000000000000000000006"
	scRelayKey = "sent_live_" + "0000000000000000000000000000000000000000000000000000000000000007"
	scPathID   = "27690631804612" // sentryProjectId(scProjectUUID)
)

func scSum(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

func scAuthenticator() *auth.APIKeyAuthenticator {
	pid := scProjectUUID
	proj := func(scope string) *auth.APIKeyData {
		return &auth.APIKeyData{ProjectID: &pid, OrganizationID: "org-1", Scope: scope, ProjectName: "proj-sc", RateLimitRPM: 100000}
	}
	return auth.NewAPIKeyAuthenticatorWithLookup(scLookup(map[string]*auth.APIKeyData{
		scSum(scKey):      proj("ingest"),
		scSum(scAdminKey): proj("admin"),
		scSum(scReadKey):  proj("read"),
		scSum(scAgentKey): proj("agent"),
		scSum(scRepKey):   proj("report"),
		scSum(scRelayKey): proj("relay"),
		scSum(scOrgKey):   {OrganizationID: "org-1", Scope: "ingest", RateLimitRPM: 100000},
	}))
}

// scScope is what applyAuthenticatedScope does for a project-scoped key.
func scScope(ctx context.Context, p *validation.ErrorPayload) error {
	k, _ := auth.ProjectKeyFromContext(ctx)
	id, _ := auth.ProjectIDFromContext(ctx)
	if k == "" {
		k, id = "proj-sc", scProjectUUID
	}
	p.ProjectKey, p.ProjectID = k, id
	return nil
}

type scRecorder struct {
	mu    sync.Mutex
	items []string
}

func (r *scRecorder) record(_ context.Context, typ, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items = append(r.items, typ+"/"+outcome)
}

func (r *scRecorder) count(typ, outcome string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, it := range r.items {
		if it == typ+"/"+outcome {
			n++
		}
	}
	return n
}

func (r *scRecorder) total() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.items) }

// scFakeIngester stands in for the service where a proof is about the handler, not the pipeline.
type scFakeIngester struct {
	calls  int
	err    error
	ids    []string
	stamp  string
	budget int
}

func (f *scFakeIngester) IngestAtWithin(_ context.Context, p *validation.ErrorPayload, _ time.Time, budget int, _ service.Shedder) error {
	f.calls++
	f.budget = budget
	f.ids = append(f.ids, p.EventID)
	if f.err != nil {
		return f.err
	}
	if f.stamp != "" {
		p.EventID = f.stamp
	}
	return nil
}

type scItem struct {
	header   map[string]any
	payload  []byte
	noLength bool
}

func scEnvelope(header any, items ...scItem) []byte {
	var b bytes.Buffer
	h, _ := json.Marshal(header)
	b.Write(h)
	for _, it := range items {
		ih := map[string]any{}
		for k, v := range it.header {
			ih[k] = v
		}
		if !it.noLength {
			ih["length"] = len(it.payload)
		}
		j, _ := json.Marshal(ih)
		b.WriteString("\n")
		b.Write(j)
		b.WriteString("\n")
		b.Write(it.payload)
	}
	return b.Bytes()
}

func scEvent(payload []byte) scItem {
	return scItem{header: map[string]any{"type": "event"}, payload: payload}
}

func scGzip(b []byte) []byte {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	_, _ = w.Write(b)
	_ = w.Close()
	return out.Bytes()
}

func scPost(h http.Handler, path string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func scHandlers(cfg sentrycompat.Config) (envelope, store http.Handler) {
	return sentrycompat.NewEnvelopeHandler(cfg), sentrycompat.NewStoreHandler(cfg)
}

func scBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m), w.Body.String())
	return m
}

// ---- P2.1, P2.4, P2.5 ----

// P2.1
func TestSentryCompatRoutes(t *testing.T) {
	mux := http.NewServeMux()
	var hits []string
	mark := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits = append(hits, name+":"+r.PathValue(sentrycompat.PathValueProjectID))
		})
	}
	mux.Handle("/ingest", mark("ingest"))
	mux.Handle("/ingest/batch", mark("batch"))
	mux.Handle("/health", mark("health"))
	mux.Handle("/metrics", mark("metrics"))
	sentrycompat.Mount(mux, mark("envelope"), mark("store")) // must not panic on a conflict (8 registrations)

	serve := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	for _, c := range []struct{ path, want string }{
		{"/api/27690631804612/envelope/", "envelope:27690631804612"},
		{"/api/27690631804612/envelope", "envelope:27690631804612"},
		{"/api/27690631804612/store/", "store:27690631804612"},
		{"/api/27690631804612/store", "store:27690631804612"},
	} {
		hits = nil
		w := serve(http.MethodPost, c.path)
		assert.Equal(t, http.StatusOK, w.Code, c.path)
		assert.Equal(t, []string{c.want}, hits, c.path)
		for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodOptions, http.MethodDelete} {
			hits = nil
			w := serve(m, c.path)
			assert.Equal(t, http.StatusMethodNotAllowed, w.Code, m+" "+c.path)
			assert.Contains(t, w.Header().Get("Allow"), "POST")
			assert.Empty(t, hits, "the chain must not run for "+m)
		}
	}
	for _, path := range []string{"/api/1/envelope/x", "/api/1/envelopes/", "/api/envelope/", "/api/1/store/x"} {
		hits = nil
		assert.Equal(t, http.StatusNotFound, serve(http.MethodPost, path).Code, path)
		assert.Empty(t, hits, path)
	}
	hits = nil
	assert.Equal(t, http.StatusOK, serve(http.MethodPost, "/ingest").Code)
	assert.Equal(t, []string{"ingest:"}, hits)
}

// scMountedAuth mounts SentryMiddleware over a probe through the real Mount, so PathValue is real.
func scMountedAuth(t *testing.T, a *auth.APIKeyAuthenticator, probe func(ctx context.Context)) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	h := a.SentryMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	sentrycompat.Mount(mux, h, h)
	return mux
}

// P2.4
func TestSentryCompatAuthenticatesDSNKey(t *testing.T) {
	var gotProject string
	var gotOrgWide bool
	ran := 0
	mux := scMountedAuth(t, scAuthenticator(), func(ctx context.Context) {
		ran++
		gotProject, _ = auth.ProjectIDFromContext(ctx)
		gotOrgWide = auth.IsOrgWideKey(ctx)
	})
	for _, route := range []string{"envelope", "store"} {
		for name, mk := range map[string]func(*http.Request){
			"header": func(r *http.Request) {
				r.Header.Set("X-Sentry-Auth", "Sentry sentry_version=7, sentry_client=sentry.dart.flutter/9.6.0, sentry_key="+scKey)
			},
			"query": func(r *http.Request) { r.URL.RawQuery = "sentry_key=" + scKey + "&sentry_version=7" },
			"xapi":  func(r *http.Request) { r.Header.Set("X-API-Key", scKey) },
		} {
			ran, gotProject, gotOrgWide = 0, "", true
			r := httptest.NewRequest(http.MethodPost, "/api/"+scPathID+"/"+route+"/", nil)
			mk(r)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			assert.Equal(t, http.StatusNoContent, w.Code, route+" "+name)
			assert.Equal(t, 1, ran, route+" "+name)
			assert.Equal(t, scProjectUUID, gotProject)
			assert.False(t, gotOrgWide)
		}
	}
}

// P2.5
func TestSentryCompatRejectsWrongKeys(t *testing.T) {
	ran := 0
	mux := scMountedAuth(t, scAuthenticator(), func(context.Context) { ran++ })
	do := func(path, key string, hdr map[string]string) int {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.Header.Set("X-Sentry-Auth", "Sentry sentry_key="+key)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	good := "/api/" + scPathID + "/envelope/"
	assert.Equal(t, http.StatusNoContent, do(good, scKey, nil), "control")
	ran = 0
	assert.Equal(t, http.StatusForbidden, do("/api/1/envelope/", scKey, nil), "another project's id")
	assert.Equal(t, http.StatusForbidden, do("/api/0027690631804612/envelope/", scKey, nil), "leading zeros are a different spelling")
	assert.Equal(t, http.StatusForbidden, do("/api/abc/envelope/", scKey, nil), "not numeric")
	for name, key := range map[string]string{"admin": scAdminKey, "read": scReadKey, "agent": scAgentKey, "report": scRepKey} {
		assert.Equal(t, http.StatusForbidden, do(good, key, nil), name+" key")
	}
	assert.Equal(t, http.StatusForbidden, do(good, scOrgKey, nil), "org-wide ingest key")
	assert.Equal(t, http.StatusForbidden, do(good, scOrgKey, map[string]string{"X-Project-Key": "proj-sc"}), "org-wide key naming a project")
	assert.Equal(t, http.StatusUnauthorized, do(good, "sent_live_unknown", nil))
	assert.Zero(t, ran, "the next handler never runs")
}

// ---- P2.11 chain half ----

func scLimiter(t *testing.T) *middleware.RateLimiter {
	return middleware.NewRateLimiter(newMiniRedisClient(t))
}

// P2.11 (chain half)
func TestSentryCompatChainBelievesRelay(t *testing.T) {
	_, compat, err := clientip.Resolvers("", "10.20.0.0/16")
	require.NoError(t, err)
	svc, pub := newTestIngestService(t)
	chain := sentrycompat.Chain(compat, scAuthenticator(), scLimiter(t), sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: svc, Scope: scScope}))

	env := scReadFixture(t, "envelopes/flutter-9.6.0.envelope")
	r := httptest.NewRequest(http.MethodPost, "/api/"+scPathID+"/envelope/", bytes.NewReader(env))
	r.SetPathValue("project_id", scPathID)
	r.RemoteAddr = "10.20.3.4:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	r.Header.Set("X-Sentry-Auth", "Sentry sentry_key="+scKey)
	w := httptest.NewRecorder()
	chain.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	e := publishedEvent(t, pub)
	assert.Equal(t, "203.0.113.9", e.User.IpAddress, "the phone's address, through the relay's X-Forwarded-For")

	// SentryMiddleware is in the chain: no key is a 401 and nothing is published.
	before := len(pub.published)
	r = httptest.NewRequest(http.MethodPost, "/api/"+scPathID+"/envelope/", bytes.NewReader(env))
	r.SetPathValue("project_id", scPathID)
	r.RemoteAddr = "10.20.3.4:5555"
	w = httptest.NewRecorder()
	chain.ServeHTTP(w, r)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Len(t, pub.published, before)
}

// The compat variable is fatal at start-up and named in the error, before anything connects.
func TestSentryCompatStartupExitsOnBadCompatList(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ingestor")
	build := exec.Command("go", "build", "-o", bin, "./apps/ingestor-go")
	build.Dir = filepath.Join("..", "..")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "SENTRY_COMPAT_TRUSTED_PROXY_CIDRS=10.20.0.0/33",
		"POSTGRES_HOST=127.0.0.1", "POSTGRES_PORT=1", "OTEL_SDK_DISABLED=true")
	out, err = cmd.CombinedOutput()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr, "the ingestor must exit non-zero: %s", out)
	assert.Equal(t, 1, exitErr.ExitCode(), string(out))
	assert.Contains(t, string(out), `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS: \"10.20.0.0/33\" is not a CIDR or IP address`)
	assert.NotContains(t, string(out), "database")
}

// ---- P3.5-P3.8 ----

// P3.5
func TestSentryEnvelopeHandlerStatuses(t *testing.T) {
	newH := func() (http.Handler, *scFakeIngester, *scRecorder) {
		f, rec := &scFakeIngester{stamp: "stamped"}, &scRecorder{}
		return sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: f, Scope: scScope, RecordItem: rec.record}), f, rec
	}
	ev := scEvent([]byte(`{"message":"m"}`))
	att := func(n int) scItem {
		return scItem{header: map[string]any{"type": "attachment"}, payload: make([]byte, n)}
	}
	hdr := map[string]any{"event_id": "e"}

	t.Run("unsupported encoding", func(t *testing.T) {
		h, f, _ := newH()
		w := scPost(h, "/api/1/envelope/", scEnvelope(hdr, ev), map[string]string{"Content-Encoding": "br"})
		assert.Equal(t, http.StatusUnsupportedMediaType, w.Code)
		assert.Zero(t, f.calls)
	})
	t.Run("5 MiB plus one as sent", func(t *testing.T) {
		h, f, _ := newH()
		w := scPost(h, "/api/1/envelope/", scEnvelope(hdr, att(sentrycompat.MaxCompressedBodyBytes+1)), nil)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Zero(t, f.calls)
	})
	t.Run("21 MiB decoded", func(t *testing.T) {
		h, f, _ := newH()
		body := scGzip(scEnvelope(hdr, att(11<<20), att(11<<20)))
		require.Less(t, len(body), sentrycompat.MaxCompressedBodyBytes)
		w := scPost(h, "/api/1/envelope/", body, map[string]string{"Content-Encoding": "gzip"})
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Zero(t, f.calls)
	})
	t.Run("101 items is 413 and ingests nothing; 100 is 200", func(t *testing.T) {
		mk := func(n int) []byte {
			items := []scItem{ev}
			for i := 1; i < n; i++ {
				items = append(items, scItem{header: map[string]any{"type": "session"}, payload: []byte("{}")})
			}
			return scEnvelope(hdr, items...)
		}
		h, f, _ := newH()
		assert.Equal(t, http.StatusRequestEntityTooLarge, scPost(h, "/api/1/envelope/", mk(101), nil).Code)
		assert.Zero(t, f.calls, "the whole envelope is read before any event is ingested")
		assert.Equal(t, http.StatusOK, scPost(h, "/api/1/envelope/", mk(100), nil).Code)
		assert.Equal(t, 1, f.calls)
	})
	t.Run("corrupt gzip", func(t *testing.T) {
		h, f, _ := newH()
		w := scPost(h, "/api/1/envelope/", []byte("definitely not gzip"), map[string]string{"Content-Encoding": "gzip"})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "invalid compressed request body")
		assert.Zero(t, f.calls)
	})
	t.Run("malformed item header", func(t *testing.T) {
		h, f, _ := newH()
		body := append(scEnvelope(hdr, ev), []byte("\nnot json\nx")...)
		w := scPost(h, "/api/1/envelope/", body, nil)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "malformed envelope")
		assert.Zero(t, f.calls, "a malformed envelope ingests nothing, even its valid first event")
	})
	huge := bytes.Repeat([]byte("x"), sentrycompat.MaxEventItemBytes+1)
	for name, big := range map[string]scItem{
		"length-framed":  {header: map[string]any{"type": "event"}, payload: huge},
		"newline-framed": {header: map[string]any{"type": "event"}, payload: append(append([]byte{}, huge...), '\n'), noLength: true},
	} {
		t.Run("oversized event, "+name, func(t *testing.T) {
			h, f, rec := newH()
			body := scEnvelope(hdr, big)
			if name == "newline-framed" {
				body = append(body, []byte(`{"type":"event"}`+"\n"+`{"message":"m"}`)...)
			} else {
				body = append(body, scEnvelope(nil, ev)[len("null"):]...)
			}
			w := scPost(h, "/api/1/envelope/", body, nil)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			assert.Equal(t, map[string]any{"id": "stamped"}, scBody(t, w))
			assert.Equal(t, 1, f.calls)
			assert.Equal(t, 1, rec.count("event", "too_large"))
			assert.Equal(t, 1, rec.count("event", "accepted"))
		})
	}
	t.Run("only a transaction and a client report", func(t *testing.T) {
		h, f, rec := newH()
		w := scPost(h, "/api/1/envelope/", scEnvelope(hdr,
			scItem{header: map[string]any{"type": "transaction"}, payload: []byte("{}")},
			scItem{header: map[string]any{"type": "client_report"}, payload: []byte("{}")}), nil)
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, map[string]any{}, scBody(t, w))
		assert.Zero(t, f.calls)
		assert.Equal(t, 1, rec.count("transaction", "discarded"))
		assert.Equal(t, 1, rec.count("client_report", "discarded"))
	})
}

// P3.6
func TestSentryEnvelopeHandlerResponses(t *testing.T) {
	ev := scEvent([]byte(`{"message":"m"}`))
	cases := []struct {
		name   string
		f      *scFakeIngester
		body   []byte
		code   int
		substr string
	}{
		{"accepted echoes the effective id", &scFakeIngester{stamp: "effective-id"}, scEnvelope(map[string]any{}, ev), 200, `"id":"effective-id"`},
		{"validation failure is 400 with its text", &scFakeIngester{err: errors.New("validation failed: x")}, scEnvelope(map[string]any{}, ev), 400, "validation failed: x"},
		{"publish failure is 500", &scFakeIngester{err: errors.New("nats: timeout")}, scEnvelope(map[string]any{}, ev), 500, "Failed to ingest event"},
		{"payload that is not an object is 400", &scFakeIngester{}, scEnvelope(map[string]any{}, scEvent([]byte(`[1,2]`))), 400, "invalid event payload"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := scPost(sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: c.f, Scope: scScope}), "/api/1/envelope/", c.body, nil)
			assert.Equal(t, c.code, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), c.substr)
		})
	}
	// the envelope header's event_id reaches IngestAt when the payload has none, and loses when it has one
	f := &scFakeIngester{}
	h := sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: f, Scope: scScope})
	scPost(h, "/api/1/envelope/", scEnvelope(map[string]any{"event_id": "from-header"}, ev), nil)
	scPost(h, "/api/1/envelope/", scEnvelope(map[string]any{"event_id": "from-header"}, scEvent([]byte(`{"event_id":"from-payload"}`))), nil)
	assert.Equal(t, []string{"from-header", "from-payload"}, f.ids)

	// a scope refusal is 403 and counted rejected
	rec := &scRecorder{}
	h = sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: &scFakeIngester{}, RecordItem: rec.record,
		Scope: func(context.Context, *validation.ErrorPayload) error { return errors.New("nope") }})
	w := scPost(h, "/api/1/envelope/", scEnvelope(map[string]any{}, ev), nil)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 1, rec.count("event", "rejected"))
}

// P3.7
func TestSentryStoreHandler(t *testing.T) {
	f, rec := &scFakeIngester{stamp: "stored"}, &scRecorder{}
	h := sentrycompat.NewStoreHandler(sentrycompat.Config{Ingest: f, Scope: scScope, RecordItem: rec.record})
	w := scPost(h, "/api/1/store/", []byte(`{"message":"m"}`), nil)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, map[string]any{"id": "stored"}, scBody(t, w))
	w = scPost(h, "/api/1/store/", scGzip([]byte(`{"message":"m"}`)), map[string]string{"Content-Encoding": "gzip"})
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, f.calls)
	assert.Equal(t, 2, rec.count("event", "accepted"))

	big := []byte(`{"message":"` + strings.Repeat("x", sentrycompat.MaxEventItemBytes) + `"}`)
	w = scPost(h, "/api/1/store/", big, nil)
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, "1 MiB + 1 decoded is a 413 on /store/ (V4)")
	assert.Equal(t, 1, rec.count("event", "too_large"))
	assert.Equal(t, 2, f.calls)
	assert.Equal(t, http.StatusUnsupportedMediaType, scPost(h, "/api/1/store/", []byte("{}"), map[string]string{"Content-Encoding": "zstd"}).Code)
	assert.Equal(t, http.StatusBadRequest, scPost(h, "/api/1/store/", []byte("[1]"), nil).Code)
}

func scCollect(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != sentrycompat.MetricItems {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, dp := range sum.DataPoints {
				require.Equal(t, 2, dp.Attributes.Len(), "no attribute beyond type and outcome")
				typ, _ := dp.Attributes.Value(attribute.Key(sentrycompat.LabelType))
				outc, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out[typ.AsString()+"/"+outc.AsString()] += dp.Value
			}
		}
	}
	return out
}

// P3.8
func TestSentryCompatItemMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	record, err := sentrycompat.NewItemRecorder(provider.Meter("test"))
	require.NoError(t, err)

	f := &scFakeIngester{}
	h := sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: f, Scope: scScope, RecordItem: record})
	huge := scItem{header: map[string]any{"type": "event"}, payload: bytes.Repeat([]byte("x"), sentrycompat.MaxEventItemBytes+1)}
	w := scPost(h, "/api/1/envelope/", scEnvelope(map[string]any{},
		scEvent([]byte(`{"message":"m"}`)),
		scItem{header: map[string]any{"type": "transaction"}, payload: []byte("{}")},
		scItem{header: map[string]any{"type": "Sneaky-User-Chosen-Type-123"}, payload: []byte("{}")},
		scItem{header: map[string]any{"type": "security"}, payload: []byte("{}")},
		huge), nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, map[string]int64{
		"event/accepted": 1, "transaction/discarded": 1, "unknown/discarded": 2, "event/too_large": 1,
	}, scCollect(t, reader))

	// A second envelope of two events whose first fails validation: rejected 1, skipped 1.
	f.err = errors.New("validation failed: boom")
	scPost(h, "/api/1/envelope/", scEnvelope(map[string]any{}, scEvent([]byte(`{"message":"a"}`)), scEvent([]byte(`{"message":"b"}`))), nil)
	got := scCollect(t, reader)
	assert.EqualValues(t, 1, got["event/rejected"])
	assert.EqualValues(t, 1, got["event/skipped"])
	assert.EqualValues(t, 1, got["event/accepted"])

	assert.Equal(t, "unknown", sentrycompat.ItemTypeLabel("whatever"))
	assert.Equal(t, "client_report", sentrycompat.ItemTypeLabel("client_report"))
	assert.Len(t, sentrycompat.ItemTypes, 17)
}

// ---- P4.6 ----

// P4.6
func TestSentryCompatDuplicateDeliveryPublishesSameIdentity(t *testing.T) {
	svc, pub := newTestIngestService(t)
	h := sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: svc, Scope: scScope})
	env := scReadFixture(t, "envelopes/flutter-9.6.0.envelope")
	env = bytes.Replace(env, []byte(`"event_id":"11111111111141118111111111111111","sdk"`), []byte(`"event_id":"55555555555545558555555555555555","sdk"`), 1)
	require.Contains(t, string(env[:200]), "55555555555545558555555555555555", "the header id was changed")
	for i := 0; i < 2; i++ {
		w := scPost(h, "/api/"+scPathID+"/envelope/", env, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, map[string]any{"id": "11111111111141118111111111111111"}, scBody(t, w))
	}
	require.Len(t, pub.published, 2)
	var a, b sentinelv1.ErrorEvent
	require.NoError(t, proto.Unmarshal(pub.published[0], &a))
	require.NoError(t, proto.Unmarshal(pub.published[1], &b))
	assert.Equal(t, "11111111111141118111111111111111", a.EventId, "the payload's id, not the header's")
	assert.Equal(t, a.EventId, b.EventId)
	assert.Equal(t, scProjectUUID, a.ProjectId)
	assert.Equal(t, a.ProjectId, b.ProjectId)

	// The payload's id absent: the header's is used, both times.
	var ev map[string]any
	require.NoError(t, json.Unmarshal(scFixtureEvent(t, "flutter-typeerror-unhandled.json"), &ev))
	delete(ev, "event_id")
	body := scEnvelope(map[string]any{"event_id": "55555555555545558555555555555555"}, scEvent(scJSON(t, ev)))
	pub.published = nil
	for i := 0; i < 2; i++ {
		require.Equal(t, http.StatusOK, scPost(h, "/api/"+scPathID+"/envelope/", body, nil).Code)
	}
	require.Len(t, pub.published, 2)
	require.NoError(t, proto.Unmarshal(pub.published[0], &a))
	require.NoError(t, proto.Unmarshal(pub.published[1], &b))
	assert.Equal(t, "55555555555545558555555555555555", a.EventId)
	assert.Equal(t, a.EventId, b.EventId)
}

// ---- P4.8, P4.9 (handler cases) ----

func scMustFit(t *testing.T, payload []byte, budget int) {
	t.Helper()
	p := scMapRaw(t, payload, sentrycompat.EnvelopeHeader{})
	p.ProjectKey = "proj-sc"
	require.Greater(t, scMarshalSize(t, p), budget, "guard: the case must be over its budget unshed")
}

// P4.8
func TestSentryCompatPublishBudgetReviewerCases(t *testing.T) {
	t.Run("A numeric arrays", func(t *testing.T) {
		raw := scCaseA(t)
		scMustFit(t, raw, service.MaxPublishBytes)
		svc, pub := newTestIngestService(t)
		rec := &scRecorder{}
		h := sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: svc, Scope: scScope, RecordItem: rec.record})
		w := scPost(h, "/api/"+scPathID+"/envelope/", scEnvelope(map[string]any{}, scEvent(raw)), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, pub.published, 1)
		assert.LessOrEqual(t, len(pub.published[0]), service.MaxPublishBytes)
		e := publishedEvent(t, pub)
		require.NotEmpty(t, e.Breadcrumbs)
		removed := 55 - len(e.Breadcrumbs)
		require.Positive(t, removed)
		assert.Equal(t, "m54", e.Breadcrumbs[len(e.Breadcrumbs)-1].Message, "the newest crumb is present")
		for i, c := range e.Breadcrumbs {
			assert.Equal(t, "m"+strconvI(removed+i), c.Message, "the crumbs are a suffix of the original order: the oldest were removed")
		}
		md := e.Metadata.AsMap()
		assert.Contains(t, md, "samples")
		assert.Contains(t, e.Contexts, "samples")
		assert.Contains(t, md[validation.MetadataKeyServerTruncated], "breadcrumbs")
		assert.Contains(t, md[validation.MetadataKeyServerTruncatedKeys], "breadcrumbs:"+strconvI(removed))
		assert.Equal(t, 1, rec.count("event", "accepted"))
	})
	t.Run("B 70 frames", func(t *testing.T) {
		raw := scCaseB(t)
		scMustFit(t, raw, service.MaxPublishBytes)
		mapped := scMapRaw(t, raw, sentrycompat.EnvelopeHeader{})
		require.Len(t, mapped.Stacktrace, 70)
		svc, pub := newTestIngestService(t)
		rec := &scRecorder{}
		h := sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: svc, Scope: scScope, RecordItem: rec.record})
		w := scPost(h, "/api/"+scPathID+"/envelope/", scEnvelope(map[string]any{}, scEvent(raw)), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, pub.published, 1)
		assert.LessOrEqual(t, len(pub.published[0]), service.MaxPublishBytes)
		e := publishedEvent(t, pub)
		require.Len(t, e.Stacktrace, 70)
		for i, f := range e.Stacktrace {
			want := mapped.Stacktrace[i]
			assert.Equal(t, want.File, f.File)
			assert.Equal(t, want.Function, f.Function)
			assert.EqualValues(t, want.Line, f.Line)
			assert.Equal(t, want.InApp, f.InApp)
			assert.Empty(t, f.ContextLine)
			assert.Empty(t, f.PreContext)
			assert.Empty(t, f.PostContext)
		}
		assert.Equal(t, "f69", e.Stacktrace[0].Function, "crash-first")
		assert.Empty(t, e.Breadcrumbs)
		for name := range e.Contexts {
			assert.Contains(t, []string{"device", "os", "app", "runtime", "browser", "culture"}, name, "no non-well-known context is left")
		}
		assert.Contains(t, e.Contexts, "device")
		assert.Contains(t, e.Contexts, "os")
		assert.Contains(t, e.Metadata.AsMap()[validation.MetadataKeyServerTruncated], "stacktrace.context")
		assert.Equal(t, 1, rec.count("event", "accepted"))
	})
}

// scShapeEvent is P4.9's event: every sheddable element is far larger than ShedSlack.
func scShapeEvent(t *testing.T) []byte {
	t.Helper()
	crumbs := make([]any, 10)
	for i := range crumbs {
		crumbs[i] = map[string]any{"timestamp": 1790320841.0 + float64(i), "type": "default", "category": "c", "level": "info",
			"message": "c" + strconvI(i) + strings.Repeat("m", 8000-2)}
	}
	frames := make([]any, 20)
	line := strings.Repeat("l", 1000)
	for i := range frames {
		frames[i] = map[string]any{"abs_path": "package:app/f" + strconvI(i) + ".dart", "function": "fn" + strconvI(i), "lineno": i + 1, "in_app": true,
			"context_line": line, "pre_context": []any{line, line, line, line, line}, "post_context": []any{line, line, line, line, line}}
	}
	values := []any{
		map[string]any{"type": "First", "value": strings.Repeat("a", 10000)},
		map[string]any{"type": "Second", "value": strings.Repeat("b", 10000)},
		map[string]any{"type": "Titled", "value": strings.Repeat("c", 10000), "mechanism": map[string]any{"type": "m", "handled": false}, "stacktrace": map[string]any{"frames": frames}},
	}
	return scJSON(t, map[string]any{
		"platform": "other", "sdk": map[string]any{"name": "sentry.dart.flutter", "version": "9.6.0"}, "level": "error",
		"breadcrumbs": map[string]any{"values": crumbs},
		"extra":       map[string]any{"big": strings.Repeat("B", 40000), "small": strings.Repeat("S", 15000), "user_id": "u1"},
		"contexts":    map[string]any{"device": map[string]any{"family": "F"}, "custom": map[string]any{"blob": strings.Repeat("x", 30000)}},
		"exception":   map[string]any{"values": values},
	})
}

// P4.9
func TestSentryCompatShedOrderAnd413(t *testing.T) {
	raw := scShapeEvent(t)

	// Estimates, taken from the bounded proto exactly as Shed takes them.
	p := scMapRaw(t, raw, sentrycompat.EnvelopeHeader{})
	p.ProjectKey, p.ProjectID = "proj-sc", scProjectUUID
	svc0, _ := newTestIngestService(t)
	require.NoError(t, svc0.IngestAt(context.Background(), p, time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)))
	ev := mapping.MapPayloadToEvent(p)
	total := proto.Size(ev)
	var t1, big, small, custom, v0, v1 int
	for _, c := range ev.Breadcrumbs {
		t1 += proto.Size(c)
	}
	big = len("big") + proto.Size(ev.Metadata.Fields["big"])
	small = len("small") + proto.Size(ev.Metadata.Fields["small"])
	custom = len("custom") + proto.Size(ev.Contexts["custom"])
	v0, v1 = proto.Size(ev.Exception.Values[0]), proto.Size(ev.Exception.Values[1])
	require.Greater(t, big, sentrycompat.ShedSlack)
	require.Greater(t, custom, sentrycompat.ShedSlack)
	cum1, cum2, cum3 := t1, t1+big+small, t1+big+small+custom

	run := func(t *testing.T, target int) (*sentinelv1.ErrorEvent, *scRecorder, int) {
		t.Helper()
		budget := total - (target - sentrycompat.ShedSlack)
		require.Positive(t, budget)
		svc, pub := newTestIngestService(t)
		rec := &scRecorder{}
		h := sentrycompat.NewEnvelopeHandler(sentrycompat.Config{Ingest: svc, Scope: scScope, RecordItem: rec.record, PublishBudget: budget})
		w := scPost(h, "/api/"+scPathID+"/envelope/", scEnvelope(map[string]any{}, scEvent(raw)), nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Len(t, pub.published, 1)
		assert.LessOrEqual(t, len(pub.published[0]), budget)
		return publishedEvent(t, pub), rec, budget
	}
	keys := func(e *sentinelv1.ErrorEvent) []any {
		return e.Metadata.AsMap()[validation.MetadataKeyServerTruncatedKeys].([]any)
	}
	require.Greater(t, t1/2, sentrycompat.ShedSlack, "k=1's target must be positive over the slack")

	t.Run("k=1 breadcrumbs only, oldest first", func(t *testing.T) {
		e, _, _ := run(t, cum1/2)
		require.NotEmpty(t, e.Breadcrumbs)
		require.Less(t, len(e.Breadcrumbs), 10)
		removed := 10 - len(e.Breadcrumbs)
		assert.True(t, strings.HasPrefix(e.Breadcrumbs[0].Message, "c"+strconvI(removed)), "the oldest went first")
		md := e.Metadata.AsMap()
		assert.Contains(t, md, "big")
		assert.Contains(t, md, "small")
		assert.Contains(t, e.Contexts, "custom")
		assert.Len(t, e.Exception.Values, 3)
		assert.NotEmpty(t, e.Stacktrace[0].ContextLine)
		assert.Equal(t, []any{"breadcrumbs:" + strconvI(removed)}, keys(e))
	})
	t.Run("k=2 metadata, biggest first, exempt key kept", func(t *testing.T) {
		e, _, _ := run(t, cum1+big/2)
		assert.Empty(t, e.Breadcrumbs, "every crumb went first")
		md := e.Metadata.AsMap()
		assert.NotContains(t, md, "big")
		assert.Contains(t, md, "small", "big before small")
		assert.Equal(t, "u1", md["user_id"])
		e, _, _ = run(t, cum2-1000) // margin: the run's timestamp and id may differ from the measurement's by a few bytes
		md = e.Metadata.AsMap()
		assert.NotContains(t, md, "big")
		assert.NotContains(t, md, "small")
		assert.Equal(t, "u1", md["user_id"], "the processor routes on user_id: never shed")
		assert.Contains(t, e.Contexts, "custom")
	})
	t.Run("k=3 contexts, well-known kept", func(t *testing.T) {
		e, _, _ := run(t, cum2+custom/2)
		assert.NotContains(t, e.Contexts, "custom")
		assert.Contains(t, e.Contexts, "device")
		assert.Len(t, e.Exception.Values, 3)
	})
	t.Run("k=4 exception values, the titled one kept", func(t *testing.T) {
		e, _, _ := run(t, cum3+v0+v1/2)
		require.Len(t, e.Exception.Values, 1)
		last := e.Exception.Values[0]
		assert.Equal(t, "Titled", last.Type)
		assert.Equal(t, strings.Repeat("c", 10000), last.Value)
		assert.Equal(t, "m", last.Mechanism.Type)
		assert.NotEmpty(t, e.Stacktrace[0].ContextLine, "context lines are B5's")
	})
	t.Run("k=5 context lines, grouping inputs intact", func(t *testing.T) {
		mapped := scMapRaw(t, raw, sentrycompat.EnvelopeHeader{})
		e, _, _ := run(t, cum3+v0+v1+1000)
		require.Len(t, e.Stacktrace, 20)
		for i, f := range e.Stacktrace {
			assert.Empty(t, f.ContextLine)
			assert.Empty(t, f.PreContext)
			assert.Empty(t, f.PostContext)
			assert.Equal(t, mapped.Stacktrace[i].File, f.File)
			assert.Equal(t, mapped.Stacktrace[i].Function, f.Function)
			assert.EqualValues(t, mapped.Stacktrace[i].Line, f.Line)
		}
		assert.Equal(t, []any{"breadcrumbs:10", "big", "small", "contexts.custom", "exception.values:2", "stacktrace.context"}, keys(e))
	})

	t.Run("a budget below what B5 reaches is 413, nothing published", func(t *testing.T) {
		svc, pub := newTestIngestService(t)
		rec := &scRecorder{}
		cfg := sentrycompat.Config{Ingest: svc, Scope: scScope, RecordItem: rec.record, PublishBudget: 1000}
		env, store := scHandlers(cfg)
		w := scPost(env, "/api/"+scPathID+"/envelope/", scEnvelope(map[string]any{}, scEvent(raw)), nil)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Equal(t, "Event too large\n", w.Body.String())
		assert.Equal(t, 1, rec.count("event", "too_large"))
		w = scPost(store, "/api/"+scPathID+"/store/", raw, nil)
		assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		assert.Equal(t, 2, rec.count("event", "too_large"))
		assert.Empty(t, pub.published)
	})
	t.Run("what NATS refuses, and ErrEventTooLarge from any Ingester, is 413 too_large, never 500", func(t *testing.T) {
		for name, err := range map[string]error{
			"nats max payload": fmt.Errorf("failed to publish message: %w", natsgo.ErrMaxPayload),
			"too large":        fmt.Errorf("wrapped: %w", service.ErrEventTooLarge),
		} {
			rec := &scRecorder{}
			f := &scFakeIngester{err: err}
			env, store := scHandlers(sentrycompat.Config{Ingest: f, Scope: scScope, RecordItem: rec.record})
			for route, h := range map[string]http.Handler{"envelope": env, "store": store} {
				var body []byte = []byte(`{"message":"m"}`)
				if route == "envelope" {
					body = scEnvelope(map[string]any{}, scEvent(body))
				}
				w := scPost(h, "/api/1/"+route+"/", body, nil)
				assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code, name+" "+route)
			}
			assert.Equal(t, 2, rec.count("event", "too_large"), name)
			assert.Zero(t, rec.count("event", "failed"), name)
		}
	})
	t.Run("the handler passes the budget it was configured with", func(t *testing.T) {
		f := &scFakeIngester{}
		scPost(sentrycompat.NewStoreHandler(sentrycompat.Config{Ingest: f, Scope: scScope}), "/api/1/store/", []byte("{}"), nil)
		assert.Equal(t, service.MaxPublishBytes, f.budget)
		scPost(sentrycompat.NewStoreHandler(sentrycompat.Config{Ingest: f, Scope: scScope, PublishBudget: 123}), "/api/1/store/", []byte("{}"), nil)
		assert.Equal(t, 123, f.budget)
	})
}

// finding rev1-compat-chain-ratelimit-unproven: the limit is proved THROUGH Chain, not the middleware alone.
func TestSentryCompatChainRateLimitsWithSentryHeaders(t *testing.T) {
	pid := scProjectUUID
	limited := auth.NewAPIKeyAuthenticatorWithLookup(scLookup(map[string]*auth.APIKeyData{
		scSum(scKey): {ProjectID: &pid, OrganizationID: "org-1", Scope: "ingest", ProjectName: "proj-sc", RateLimitRPM: 1},
	}))
	_, compat, err := clientip.Resolvers("", "")
	require.NoError(t, err)
	var ran int
	chain := sentrycompat.Chain(compat, limited, scLimiter(t), okHandler(&ran))
	do := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/"+scPathID+"/envelope/", nil)
		r.SetPathValue("project_id", scPathID)
		r.Header.Set("X-Sentry-Auth", "Sentry sentry_key="+scKey)
		w := httptest.NewRecorder()
		chain.ServeHTTP(w, r)
		return w
	}
	first := do()
	require.Equal(t, http.StatusOK, first.Code)
	assert.Empty(t, first.Header().Get("X-Sentry-Rate-Limits"))
	second := do()
	require.Equal(t, http.StatusTooManyRequests, second.Code)
	assert.Equal(t, "60", second.Header().Get("Retry-After"))
	assert.Equal(t, "60::organization", second.Header().Get("X-Sentry-Rate-Limits"))
	assert.Equal(t, 1, ran, "the limited request never reaches the handler")
}
