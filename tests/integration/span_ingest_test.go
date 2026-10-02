package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/sentrycompat"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/spanpull"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/spans"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/spanwire"
)

// sentinel-span-ingest D9: real-Postgres proof of the two span sources. Run with FORCE_TESTCONTAINERS=1 like every
// file here (CLAUDE.md "Working conventions"); each test seeds its own organization and removes its rows.
//
//   - a compat envelope with a `transaction` item stores its root and child spans (D2), masked (D7), deduplicated;
//   - an error occurrence with a canonical trace id in an allow-listed org enqueues exactly one pull (D3); the worker
//     stores the trace's spans from a stub Tempo (OTLP JSON) and a stub Jaeger API, adds late spans at the second
//     fetch without duplicating, refuses other organizations, and never fails error ingest on store errors.

const spanFixtureEnvelope = "../fixtures/sentry/envelopes/flutter-9.6.0-transaction.envelope"

func spanPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := newPostgresPool(t, TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
	})
	t.Cleanup(pool.Close)
	return pool
}

type spanTenant struct{ OrgID, ProjectID, ProjectName string }

func seedSpanTenant(t *testing.T, pool *pgxpool.Pool, tag string) spanTenant {
	t.Helper()
	name := uniqueProjectName("span-" + tag)
	projectID := createTestProject(t, pool, name, uniqueAPIKey("span-"+tag))
	var org string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT organization_id::text FROM projects WHERE id = $1`, projectID).Scan(&org))
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM spans WHERE org_id = $1`, org)
		_, _ = pool.Exec(ctx, `DELETE FROM span_pull_queue WHERE org_id = $1`, org)
		cleanupServiceRows(t, pool, projectID)
	})
	return spanTenant{OrgID: org, ProjectID: projectID, ProjectName: name}
}

func countRows(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

// shiftedFixture returns the fixture's transaction item with every timestamp moved so the root starts at base.
func shiftedFixture(t *testing.T, base time.Time) []byte {
	t.Helper()
	raw, err := os.ReadFile(spanFixtureEnvelope)
	require.NoError(t, err)
	env, err := sentrycompat.ReadEnvelope(strings.NewReader(string(raw)))
	require.NoError(t, err)
	var tx map[string]any
	require.NoError(t, json.Unmarshal(env.Items[0].Payload, &tx))
	delta := float64(base.UnixMicro())/1e6 - tx["start_timestamp"].(float64)
	tx["start_timestamp"] = tx["start_timestamp"].(float64) + delta
	tx["timestamp"] = tx["timestamp"].(float64) + delta
	for _, s := range tx["spans"].([]any) {
		m := s.(map[string]any)
		m["start_timestamp"] = m["start_timestamp"].(float64) + delta
		m["timestamp"] = m["timestamp"].(float64) + delta
	}
	out, err := json.Marshal(tx)
	require.NoError(t, err)
	return out
}

// publishTransaction drives the real compat handler and returns what it published to SPANS.
func publishTransaction(t *testing.T, tenant spanTenant, item []byte) [][]byte {
	t.Helper()
	var msgs [][]byte
	h := sentrycompat.NewEnvelopeHandler(sentrycompat.Config{
		Spans:    func(_ context.Context, m []byte) error { msgs = append(msgs, m); return nil },
		Identity: func(context.Context) (string, string, bool) { return tenant.OrgID, tenant.ProjectID, true },
	})
	var body strings.Builder
	body.WriteString(`{"sdk":{"name":"sentry.dart.flutter","version":"9.6.0"}}` + "\n")
	fmt.Fprintf(&body, `{"type":"transaction","length":%d}`+"\n", len(item))
	body.Write(item)
	body.WriteString("\n")
	r := httptest.NewRequest(http.MethodPost, "/api/1/envelope/", strings.NewReader(body.String()))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	return msgs
}

func TestSpanIngest_TransactionItemStoresRootAndChildren(t *testing.T) {
	pool := spanPool(t)
	tenant := seedSpanTenant(t, pool, "txn")
	ctx := context.Background()
	_, err := spans.NewStore(pool).EnsurePartitions(ctx)
	require.NoError(t, err)

	msgs := publishTransaction(t, tenant, shiftedFixture(t, time.Now().Add(-time.Minute)))
	require.Len(t, msgs, 1)
	consumer := &spans.Consumer{Store: spans.NewStore(pool)}
	require.NoError(t, consumer.Handle(ctx, msgs[0]))

	assert.Equal(t, 4, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id = $1`, tenant.OrgID), "the root and its three children")
	var parent, svc, source, env, op string
	var proj string
	require.NoError(t, pool.QueryRow(ctx, `SELECT parent_span_id, service_name, source, environment, op, project_id::text FROM spans
		WHERE org_id = $1 AND span_id = 'b2b2b2b2b2b2b2b2'`, tenant.OrgID).Scan(&parent, &svc, &source, &env, &op, &proj))
	assert.Equal(t, "a1a1a1a1a1a1a1a1", parent, "grandchild parentage survives the round trip")
	assert.Equal(t, tenant.ProjectName, svc, "a mobile span's service is its project")
	assert.Equal(t, "mobile", source)
	assert.Equal(t, "production", env)
	assert.Equal(t, tenant.ProjectID, proj)

	// D7: the masker ran on the way in.
	var authz, name string
	require.NoError(t, pool.QueryRow(ctx, `SELECT attributes->>'authorization', name FROM spans WHERE org_id = $1 AND span_id = 'a1a1a1a1a1a1a1a1'`,
		tenant.OrgID).Scan(&authz, &name))
	assert.Equal(t, "***REDACTED***", authz)
	assert.NotContains(t, name, "abcdefghijklmnopqrstuvwxyz0123")

	// A redelivery or an outbox retry stores nothing new: ON CONFLICT (trace_id, span_id, start_ts) DO NOTHING.
	require.NoError(t, consumer.Handle(ctx, msgs[0]))
	assert.Equal(t, 4, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id = $1`, tenant.OrgID))
}

func TestSpanIngest_ForeignProjectAndOutOfWindowSpansAreNotStored(t *testing.T) {
	pool := spanPool(t)
	a, b := seedSpanTenant(t, pool, "tenant-a"), seedSpanTenant(t, pool, "tenant-b")
	ctx := context.Background()
	store := spans.NewStore(pool)
	_, err := store.EnsurePartitions(ctx)
	require.NoError(t, err)
	consumer := &spans.Consumer{Store: store}

	// B's project under A's organization: the stated org does not own the project, so nothing is written (B7).
	forged := publishTransaction(t, spanTenant{OrgID: a.OrgID, ProjectID: b.ProjectID}, shiftedFixture(t, time.Now().Add(-time.Minute)))
	require.NoError(t, consumer.Handle(ctx, forged[0]), "a message that can never succeed is acked, not redelivered")
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id IN ($1, $2)`, a.OrgID, b.OrgID))

	// D1: a span 20 days old has no partition and is dropped and counted, never defaulted.
	old := publishTransaction(t, a, shiftedFixture(t, time.Now().AddDate(0, 0, -20)))
	require.NoError(t, consumer.Handle(ctx, old[0]))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id = $1`, a.OrgID))
}

// ---- D3: pull ---------------------------------------------------------------------------------------------

const pullTrace = "5b8efff798038103d269b633813fc60c"

type stubTraceStore struct {
	srv      *httptest.Server
	requests atomic.Int64
	status   atomic.Int64
	body     atomic.Value // string
}

func newStubTraceStore(t *testing.T) *stubTraceStore {
	s := &stubTraceStore{}
	s.status.Store(200)
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		w.WriteHeader(int(s.status.Load()))
		_, _ = w.Write([]byte(s.body.Load().(string)))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// otlpBody renders a Tempo response with n spans of pullTrace starting at start, span i = 0x1000+i, each a child of
// the first.
func otlpBody(start time.Time, n int) string {
	var sp []string
	for i := 0; i < n; i++ {
		parent := ""
		if i > 0 {
			parent = fmt.Sprintf("%016x", 0x1000)
		}
		sp = append(sp, fmt.Sprintf(`{"traceId":%q,"spanId":"%016x","parentSpanId":%q,"name":"span-%d","kind":"SPAN_KIND_SERVER",
			"startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":[{"key":"password","value":{"stringValue":"hunter2hunter2"}}],"status":{"code":"STATUS_CODE_OK"}}`,
			pullTrace, 0x1000+i, parent, i, start.UnixNano()+int64(i)*1000, start.UnixNano()+int64(i)*1000+500_000))
	}
	return `{"batches":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"core-api"}},{"key":"environment","value":{"stringValue":"production"}}]},"scopeSpans":[{"spans":[` +
		strings.Join(sp, ",") + `]}]}]}`
}

func jaegerBody(start time.Time, n int) string {
	var sp []string
	for i := 0; i < n; i++ {
		refs := `[]`
		if i > 0 {
			refs = fmt.Sprintf(`[{"refType":"CHILD_OF","traceID":%q,"spanID":"%016x"}]`, pullTrace, 0x1000)
		}
		sp = append(sp, fmt.Sprintf(`{"traceID":%q,"spanID":"%016x","operationName":"span-%d","references":%s,"startTime":%d,"duration":500000,"processID":"p1",
			"tags":[{"key":"span.kind","type":"string","value":"server"},{"key":"password","type":"string","value":"hunter2hunter2"}]}`,
			pullTrace, 0x1000+i, i, refs, start.UnixMicro()+int64(i)))
	}
	return fmt.Sprintf(`{"data":[{"traceID":%q,"spans":[%s],"processes":{"p1":{"serviceName":"core-api","tags":[{"key":"environment","type":"string","value":"production"}]}}}]}`,
		pullTrace, strings.Join(sp, ","))
}

// seedProjectIn adds a project to an existing organization (no api key row: only the processor path uses it).
func seedProjectIn(t *testing.T, pool *pgxpool.Pool, orgID, tag string) spanTenant {
	t.Helper()
	name := uniqueProjectName(tag)
	var id string
	require.NoError(t, pool.QueryRow(context.Background(), `INSERT INTO projects (name, api_key, api_key_hash, organization_id)
		VALUES ($1, $2, $2, $3) RETURNING id::text`, name, uniqueAPIKey(tag), orgID).Scan(&id))
	t.Cleanup(func() { cleanupServiceRows(t, pool, id) })
	return spanTenant{OrgID: orgID, ProjectID: id, ProjectName: name}
}

type pullHarness struct {
	mobile spanTenant // a second project of the SAME organization: the mobile app, whose errors can queue a core trace
	pool   *pgxpool.Pool
	svc    *service.ProcessorService
	queue  *spanpull.Queue
	worker *spanpull.Worker
	stub   *stubTraceStore
	tenant spanTenant
	other  spanTenant
	clock  *time.Time
}

func newPullHarness(t *testing.T, kind string) *pullHarness {
	t.Helper()
	pool := spanPool(t)
	h := &pullHarness{pool: pool, stub: newStubTraceStore(t)}
	// h.tenant is the org and its CORE project (the one SPAN_PULL_SCOPES maps the org to); h.mobile is another project
	// of the same org; h.other is a different organization entirely.
	h.tenant, h.other = seedSpanTenant(t, pool, "pull-"+kind), seedSpanTenant(t, pool, "pull-other-"+kind)
	h.mobile = seedProjectIn(t, pool, h.tenant.OrgID, "pull-mobile-"+kind)
	now := time.Now()
	h.clock = &now
	clock := func() time.Time { return *h.clock }

	cfg := spanpull.Config{Scopes: map[string]string{h.tenant.OrgID: h.tenant.ProjectID}, Kind: kind, URL: h.stub.srv.URL}
	store, err := spanpull.NewTraceStore(cfg, h.stub.srv.Client())
	require.NoError(t, err)
	h.queue = spanpull.NewQueue(pool, cfg).WithClock(clock)
	h.svc = service.NewProcessorService(pool)
	h.svc.SetSpanPull(h.queue)
	h.worker = &spanpull.Worker{Queue: h.queue, Spans: spans.NewStore(pool).WithClock(clock), Traces: store, Cfg: cfg, Now: clock}
	_, err = spans.NewStore(pool).EnsurePartitions(context.Background())
	require.NoError(t, err)
	return h
}

func (h *pullHarness) errorIn(t *testing.T, tenant spanTenant, traceID string) {
	t.Helper()
	ev := buildValidProtoEvent(tenant.ProjectName)
	ev.TraceId = traceID
	ev.EventId = uuid.New().String()
	ev.Fingerprint = "fp-" + uuid.New().String()
	data, err := proto.Marshal(ev)
	require.NoError(t, err)
	require.NoError(t, h.svc.ProcessEvent(context.Background(), data), "error ingest must succeed")
}

func (h *pullHarness) queueRows(t *testing.T, org string) int {
	return countRows(t, h.pool, `SELECT count(*) FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, org, pullTrace)
}

func (h *pullHarness) spanRows(t *testing.T) int {
	return countRows(t, h.pool, `SELECT count(*) FROM spans WHERE org_id = $1 AND source = 'pulled'`, h.tenant.OrgID)
}

func TestSpanPull_EnqueueFetchLateSpansAndAllowList(t *testing.T) {
	for _, kind := range []string{spanpull.KindTempo, spanpull.KindJaeger} {
		t.Run(kind, func(t *testing.T) {
			h := newPullHarness(t, kind)
			ctx := context.Background()
			// A span's start time is part of its identity (the dedupe key), and a store returns the same one on
			// every fetch, so the two responses share one start.
			start := time.Now().Add(-time.Minute)
			body := func(n int) string {
				if kind == spanpull.KindTempo {
					return otlpBody(start, n)
				}
				return jaegerBody(start, n)
			}

			// An error with a canonical trace id in the allow-listed org enqueues ONE pull; a second error of the
			// same trace (different event) does not enqueue another (D3: one row per trace, deduplicated).
			h.errorIn(t, h.tenant, pullTrace)
			h.errorIn(t, h.tenant, pullTrace)
			assert.Equal(t, 1, h.queueRows(t, h.tenant.OrgID))

			// The allow-list is the guard: the same trace id asserted by another organization enqueues nothing.
			h.errorIn(t, h.other, pullTrace)
			assert.Zero(t, h.queueRows(t, h.other.OrgID), "another org's error must never cause core's trace to be copied")

			// A non-canonical trace id is not a trace to pull.
			h.errorIn(t, h.tenant, "trace-not-canonical")
			assert.Equal(t, 1, countRows(t, h.pool, `SELECT count(*) FROM span_pull_queue WHERE org_id = $1`, h.tenant.OrgID))

			// Nothing is due before 2 minutes.
			h.stub.body.Store(body(2))
			n, err := h.worker.RunOnce(ctx)
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.Zero(t, h.stub.requests.Load())

			// 3 minutes after the first error: first fetch, 2 spans, source pulled, org = the triggering org.
			*h.clock = time.Now().Add(3 * time.Minute)
			n, err = h.worker.RunOnce(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			assert.Equal(t, 2, h.spanRows(t))
			var svc, env, pwd, parent, kindCol string
			var proj *string
			require.NoError(t, h.pool.QueryRow(ctx, `SELECT service_name, environment, attributes->>'password', parent_span_id, kind, project_id::text
				FROM spans WHERE org_id = $1 AND span_id = $2`, h.tenant.OrgID, fmt.Sprintf("%016x", 0x1001)).Scan(&svc, &env, &pwd, &parent, &kindCol, &proj))
			assert.Equal(t, "core-api", svc, "service_name from service.name")
			assert.Equal(t, "production", env, "environment from the resource attribute")
			assert.Equal(t, "***REDACTED***", pwd, "D7: pulled attributes are masked too")
			assert.Equal(t, fmt.Sprintf("%016x", 0x1000), parent)
			assert.Equal(t, "server", kindCol)
			assert.Nil(t, proj, "a pulled span has no Sentinel project")

			// Between the fetches nothing is due.
			n, _ = h.worker.RunOnce(ctx)
			assert.Zero(t, n)

			// 11 minutes: the second fetch returns a late span; the two stored spans are not duplicated.
			h.stub.body.Store(body(3))
			*h.clock = time.Now().Add(11 * time.Minute)
			n, err = h.worker.RunOnce(ctx)
			require.NoError(t, err)
			assert.Equal(t, 1, n)
			assert.Equal(t, 3, h.spanRows(t), "upsert: the late span is added, the first two are kept once")
			assert.EqualValues(t, 2, h.stub.requests.Load(), "exactly two fetches per trace")

			// Done: nothing further is ever due.
			*h.clock = time.Now().Add(48 * time.Hour)
			n, _ = h.worker.RunOnce(ctx)
			assert.Zero(t, n)
			assert.Equal(t, 3, h.spanRows(t))
			assert.Zero(t, countRows(t, h.pool, `SELECT count(*) FROM spans WHERE org_id = $1`, h.other.OrgID))
		})
	}
}

func TestSpanPull_FailuresNeverFailErrorIngest(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()

	t.Run("an unreachable queue database does not fail ProcessEvent", func(t *testing.T) {
		dead := spanPool(t)
		dead.Close()
		h.svc.SetSpanPull(spanpull.NewQueue(dead, spanpull.Config{Scopes: map[string]string{h.tenant.OrgID: h.tenant.ProjectID}, Kind: spanpull.KindTempo, URL: h.stub.srv.URL}))
		h.errorIn(t, h.tenant, pullTrace) // require.NoError inside
		assert.Equal(t, 1, countRows(t, h.pool, `SELECT count(*) FROM error_occurrences eo JOIN issues i ON i.id = eo.issue_id WHERE i.project_id = $1`, h.tenant.ProjectID),
			"the occurrence was stored regardless")
		assert.Zero(t, h.queueRows(t, h.tenant.OrgID))
		h.svc.SetSpanPull(h.queue)
	})

	t.Run("a failing trace store retries a bounded number of times, then gives the stage up", func(t *testing.T) {
		h.errorIn(t, h.tenant, pullTrace)
		require.Equal(t, 1, h.queueRows(t, h.tenant.OrgID))
		h.stub.status.Store(503)
		h.stub.body.Store("unavailable")
		for attempt := 1; attempt <= spanpull.MaxAttemptsPerStage; attempt++ {
			*h.clock = h.clock.Add(15 * time.Minute)
			n, err := h.worker.RunOnce(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, n, "attempt %d", attempt)
		}
		assert.EqualValues(t, spanpull.MaxAttemptsPerStage, h.stub.requests.Load())
		assert.Zero(t, h.spanRows(t))
		var stage, attempts int
		var lastErr string
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT stage, attempts, last_error FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, h.tenant.OrgID, pullTrace).Scan(&stage, &attempts, &lastErr))
		assert.Equal(t, 0, stage, "still at stage 0 after the last allowed attempt")
		assert.Equal(t, spanpull.MaxAttemptsPerStage, attempts, "m2: the attempt is counted at claim")
		assert.Contains(t, lastErr, "503")

		// The next claim finds the budget spent: the stage is given up on WITHOUT another fetch.
		*h.clock = h.clock.Add(15 * time.Minute)
		n, err := h.worker.RunOnce(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		assert.EqualValues(t, spanpull.MaxAttemptsPerStage, h.stub.requests.Load(), "no fetch on the give-up claim")
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT stage, attempts FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, h.tenant.OrgID, pullTrace).Scan(&stage, &attempts))
		assert.Equal(t, 1, stage, "stage 0 given up on; the second fetch is still to come")
		assert.Zero(t, attempts, "the budget resets for the next stage")
	})

	t.Run("a trace the store does not hold is not a failure", func(t *testing.T) {
		h.stub.status.Store(404)
		*h.clock = h.clock.Add(15 * time.Minute)
		n, err := h.worker.RunOnce(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		var stage int
		require.NoError(t, h.pool.QueryRow(ctx, `SELECT stage FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, h.tenant.OrgID, pullTrace).Scan(&stage))
		assert.Equal(t, 2, stage)
	})
}

func TestSpanPull_JobOfDelistedOrgIsDroppedWithoutFetching(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()
	// A queue row for an organization that is not (or no longer) allow-listed, e.g. left by an older configuration.
	_, err := h.pool.Exec(ctx, `INSERT INTO span_pull_queue (org_id, trace_id, next_fetch_at) VALUES ($1, $2, now() - interval '1 minute')`, h.other.OrgID, pullTrace)
	require.NoError(t, err)
	h.stub.body.Store(otlpBody(time.Now(), 2))
	n, err := h.worker.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Zero(t, h.stub.requests.Load(), "the guard holds at the moment of the copy, not only at the enqueue")
	assert.Zero(t, countRows(t, h.pool, `SELECT count(*) FROM spans WHERE org_id = $1`, h.other.OrgID))
}

func TestSpanPull_CapsATraceAt5000Spans(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()
	h.errorIn(t, h.tenant, pullTrace)
	h.stub.body.Store(otlpBody(time.Now().Add(-time.Minute), spanpull.MaxSpansPerTrace+200))
	*h.clock = time.Now().Add(3 * time.Minute)
	_, err := h.worker.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, spanpull.MaxSpansPerTrace, h.spanRows(t))
}

var _ = spanwire.SourcePulled

// ---- repair round 1 ---------------------------------------------------------------------------------------

// D8 (round 2): pulled spans are CORE's data, governed by the org's configured core project — whichever project's
// error queued the trace. The order of the errors must make no difference.
func TestSpanPull_PulledSpansAreScopedToTheCoreProjectWhicheverErrorCameFirst(t *testing.T) {
	for _, mobileFirst := range []bool{true, false} {
		name := "core error first"
		if mobileFirst {
			name = "mobile error first"
		}
		t.Run(name, func(t *testing.T) {
			h := newPullHarness(t, spanpull.KindTempo)
			ctx := context.Background()
			first, second := h.tenant, h.mobile
			if mobileFirst {
				first, second = h.mobile, h.tenant
			}
			h.errorIn(t, first, pullTrace)
			h.errorIn(t, second, pullTrace) // same trace, other project: no new row, nothing rewritten
			assert.Equal(t, 1, h.queueRows(t, h.tenant.OrgID))
			var trig string
			require.NoError(t, h.pool.QueryRow(ctx, `SELECT trigger_project_id::text FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, h.tenant.OrgID, pullTrace).Scan(&trig))
			assert.Equal(t, first.ProjectID, trig, "the first error's project is kept, for debugging only")

			h.stub.body.Store(otlpBody(time.Now().Add(-time.Minute), 2))
			*h.clock = time.Now().Add(3 * time.Minute)
			_, err := h.worker.RunOnce(ctx)
			require.NoError(t, err)
			assert.Equal(t, 2, countRows(t, h.pool, `SELECT count(*) FROM spans WHERE org_id = $1 AND source = 'pulled' AND scope_project_id = $2`, h.tenant.OrgID, h.tenant.ProjectID),
				"every pulled span is stamped with the CORE project, never the triggering error's")
			assert.Zero(t, countRows(t, h.pool, `SELECT count(*) FROM spans WHERE org_id = $1 AND source = 'pulled' AND scope_project_id IS DISTINCT FROM $2`, h.tenant.OrgID, h.tenant.ProjectID))
		})
	}
}

func TestSpanPull_AnOrgOutsideTheScopeMapEnqueuesNothing(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	otherProject := seedProjectIn(t, h.pool, h.other.OrgID, "pull-notmapped")
	h.errorIn(t, otherProject, pullTrace)
	h.errorIn(t, h.other, pullTrace)
	assert.Zero(t, countRows(t, h.pool, `SELECT count(*) FROM span_pull_queue WHERE org_id = $1`, h.other.OrgID), "(d) not a key of SPAN_PULL_SCOPES")
}

func TestSpanPull_AdvanceIsConditionalOnTheClaimedStage(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()
	h.errorIn(t, h.tenant, pullTrace)
	*h.clock = time.Now().Add(3 * time.Minute)
	first, err := h.queue.Claim(ctx, 5)
	require.NoError(t, err)
	require.Len(t, first, 1)
	// The lease expires while the first worker is still fetching; a second worker re-claims the same stage-0 row.
	*h.clock = h.clock.Add(spanpull.ClaimLease + time.Second)
	second, err := h.queue.Claim(ctx, 5)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, 2, second[0].Attempts)

	advanced1, err := h.queue.Advance(ctx, first[0])
	require.NoError(t, err)
	advanced2, err := h.queue.Advance(ctx, second[0])
	require.NoError(t, err)
	assert.True(t, advanced1)
	assert.False(t, advanced2, "m2: the stale claim must not advance the row a second time")
	var stage int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT stage FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, h.tenant.OrgID, pullTrace).Scan(&stage))
	assert.Equal(t, 1, stage, "the 10-minute fetch is not skipped")
}

func TestSpanPull_AWorkerThatKeepsCrashingDoesNotRetryForever(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()
	h.errorIn(t, h.tenant, pullTrace)
	// Claim and never finish (the worker dies mid-fetch), past every lease: the attempts still accumulate.
	for i := 1; i <= spanpull.MaxAttemptsPerStage; i++ {
		*h.clock = h.clock.Add(spanpull.ClaimLease + 3*time.Minute)
		jobs, err := h.queue.Claim(ctx, 5)
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, i, jobs[0].Attempts)
	}
	h.stub.body.Store(otlpBody(time.Now().Add(-time.Minute), 2))
	*h.clock = h.clock.Add(spanpull.ClaimLease + 3*time.Minute)
	_, err := h.worker.RunOnce(ctx) // the next claim finds the budget spent
	require.NoError(t, err)
	assert.Zero(t, h.stub.requests.Load(), "the poison stage is given up on without fetching")
	var stage int
	require.NoError(t, h.pool.QueryRow(ctx, `SELECT stage FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, h.tenant.OrgID, pullTrace).Scan(&stage))
	assert.Equal(t, 1, stage)
}

func TestSpanPull_AnOversizedTraceResponseIsNotRetried(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	ctx := context.Background()
	h.errorIn(t, h.tenant, pullTrace)
	h.stub.body.Store(`{"batches":[],"pad":"` + strings.Repeat("x", 33<<20) + `"}`)
	*h.clock = time.Now().Add(3 * time.Minute)
	n, err := h.worker.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	assert.Equal(t, 2, jobStage(t, h), "m3: finished (a trace only grows, so no second stage), not retried")
	assert.EqualValues(t, 1, h.stub.requests.Load())
}

func spanRow(org, project, source, trace, spanID string, start time.Time) spans.Row {
	return spans.Row{OrgID: org, ProjectID: project, Source: source, Span: spanwire.Span{
		TraceID: trace, SpanID: spanID, StartTS: start, DurationUS: 5, Name: "n", Status: "ok"}}
}

func TestSpanStore_DedupeIsPerOrganization(t *testing.T) {
	pool := spanPool(t)
	a, b := seedSpanTenant(t, pool, "dedupe-a"), seedSpanTenant(t, pool, "dedupe-b")
	store := spans.NewStore(pool)
	ctx := context.Background()
	_, err := store.EnsurePartitions(ctx)
	require.NoError(t, err)
	start := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)

	// Org B writes a span with the ids org A's trace will use, FIRST.
	n, err := store.Insert(ctx, []spans.Row{spanRow(b.OrgID, b.ProjectID, "mobile", pullTrace, "00000000000000a1", start)})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	// m1: A's row with the same trace/span/start is NOT blocked by B's, and each org keeps one.
	n, err = store.Insert(ctx, []spans.Row{spanRow(a.OrgID, a.ProjectID, "mobile", pullTrace, "00000000000000a1", start)})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	// A re-insert within an org is still a no-op.
	n, err = store.Insert(ctx, []spans.Row{spanRow(a.OrgID, a.ProjectID, "mobile", pullTrace, "00000000000000a1", start)})
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id = $1`, a.OrgID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id = $1`, b.OrgID))
}

func TestSpanStore_OneBadRowIsDroppedNotTheBatch(t *testing.T) {
	pool := spanPool(t)
	a := seedSpanTenant(t, pool, "badrow")
	store := spans.NewStore(pool)
	ctx := context.Background()
	_, err := store.EnsurePartitions(ctx)
	require.NoError(t, err)
	start := time.Now().UTC().Add(-time.Minute)

	good1 := spanRow(a.OrgID, a.ProjectID, "mobile", pullTrace, "00000000000000b1", start)
	evil := spanRow(a.OrgID, a.ProjectID, "evil", pullTrace, "00000000000000b2", start)  // fails valid()
	badIDs := spanRow(a.OrgID, a.ProjectID, "mobile", pullTrace, "nothex", start)        // fails valid()
	nul := spanRow(a.OrgID, a.ProjectID, "mobile", pullTrace, "00000000000000b3", start) // passes valid(), refused by Postgres
	nul.Name = "a\x00b"
	old := spanRow(a.OrgID, a.ProjectID, "mobile", pullTrace, "00000000000000b4", start.AddDate(0, 0, -30)) // no partition
	good2 := spanRow(a.OrgID, a.ProjectID, "mobile", pullTrace, "00000000000000b5", start)

	n, err := store.Insert(ctx, []spans.Row{good1, evil, badIDs, nul, old, good2})
	require.NoError(t, err, "m4: bad rows must not fail the message (3 deliveries then the DLQ)")
	assert.Equal(t, 2, n)
	assert.Equal(t, 2, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id = $1`, a.OrgID))
}

func TestSpanIngest_AProjectlessMobileBatchIsNotStored(t *testing.T) {
	pool := spanPool(t)
	tenant := seedSpanTenant(t, pool, "noproj")
	store := spans.NewStore(pool)
	_, err := store.EnsurePartitions(context.Background())
	require.NoError(t, err)
	msgs := publishTransaction(t, tenant, shiftedFixture(t, time.Now().Add(-time.Minute)))
	b, err := spanwire.Decode(msgs[0])
	require.NoError(t, err)
	b.ProjectID = "" // an organization-wide key that named no project
	raw, _ := json.Marshal(b)
	require.NoError(t, (&spans.Consumer{Store: store}).Handle(context.Background(), raw))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id = $1`, tenant.OrgID), "m5: invisible forever, so not stored")
}

// ---- sentinel-span-ingest BUG-01: SPAN_PULL_SCOPES is checked against the database ---------------------------

func gatedConfig(t *testing.T, url string, pairs ...string) spanpull.Config {
	t.Helper()
	env := map[string]string{"SPAN_PULL_SCOPES": strings.Join(pairs, ","), "TRACE_STORE_KIND": "tempo", "TRACE_STORE_URL": url}
	cfg, err := spanpull.ParseConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	require.NoError(t, err)
	return cfg
}

func TestSpanPull_ScopesAreValidatedAgainstTheProjectsTable(t *testing.T) {
	pool := spanPool(t)
	ctx := context.Background()
	a, b, c := seedSpanTenant(t, pool, "scope-a"), seedSpanTenant(t, pool, "scope-b"), seedSpanTenant(t, pool, "scope-c")
	ghost := uuid.New().String()
	stub := newStubTraceStore(t)
	stub.body.Store("")
	cfg := gatedConfig(t, stub.srv.URL,
		a.OrgID+":"+a.ProjectID, // valid
		b.OrgID+":"+a.ProjectID, // b mapped to a's core project: refused
		c.OrgID+":"+ghost,       // unknown project: refused
	)
	queue := spanpull.NewQueue(pool, cfg)
	svc := service.NewProcessorService(pool)
	svc.SetSpanPull(queue)
	h := &pullHarness{pool: pool, svc: svc}

	// Before validation nothing is enqueued, not even for the valid org.
	h.errorIn(t, a, pullTrace)
	assert.Zero(t, h.queueRows(t, a.OrgID), "no enqueue before validation has succeeded")

	accepted, refused, err := cfg.ValidateAgainstDB(ctx, pool)
	require.NoError(t, err)
	assert.Equal(t, 1, accepted)
	assert.Equal(t, 2, refused)

	h.errorIn(t, a, pullTrace)
	h.errorIn(t, b, pullTrace)
	h.errorIn(t, c, pullTrace)
	assert.Equal(t, 1, h.queueRows(t, a.OrgID), "the valid scope is kept")
	assert.Zero(t, h.queueRows(t, b.OrgID), "a project of another organization is refused")
	assert.Zero(t, h.queueRows(t, c.OrgID), "an unknown project is refused")
	assert.Equal(t, a.ProjectID, cfg.ScopeProject(a.OrgID))
	assert.False(t, cfg.Allowed(b.OrgID))
}

// ---- sentinel-span-ingest BUG-02: Tempo's "trace too large" is final; 404 is not a warning -------------------

const tooLargeBody = "trace exceeds max size (max bytes: 5000000)"

func jobStage(t *testing.T, h *pullHarness) int {
	return countRows(t, h.pool, `SELECT stage FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`, h.tenant.OrgID, pullTrace)
}

func captureLogs(t *testing.T) *strings.Builder {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestSpanPull_TooLargeAtStageOneFinishesTheJobWithoutRetryOrWarning(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	logs := captureLogs(t)
	h.stub.status.Store(500)
	h.stub.body.Store(tooLargeBody)
	h.errorIn(t, h.tenant, pullTrace)
	*h.clock = time.Now().Add(3 * time.Minute)

	_, err := h.worker.RunOnce(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 1, h.stub.requests.Load(), "one fetch, no retry")
	assert.Equal(t, 2, jobStage(t, h), "finished: the second stage is skipped, the trace only grows")
	assert.NotContains(t, logs.String(), "level=WARN")
	assert.Equal(t, 1, strings.Count(logs.String(), "too large"), "one line for the stage")

	*h.clock = h.clock.Add(time.Hour)
	_, err = h.worker.RunOnce(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 1, h.stub.requests.Load(), "nothing more is fetched")
}

func TestSpanPull_NotFoundAdvancesAndFinishesWithoutWarning(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	logs := captureLogs(t)
	h.stub.status.Store(404)
	h.stub.body.Store("trace not found")
	h.errorIn(t, h.tenant, pullTrace)
	*h.clock = time.Now().Add(3 * time.Minute)

	_, err := h.worker.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, jobStage(t, h), "404 at stage 0 advances to the second fetch")
	assert.EqualValues(t, 1, h.stub.requests.Load())

	*h.clock = time.Now().Add(11 * time.Minute)
	_, err = h.worker.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, jobStage(t, h), "404 at the last stage finishes the job")
	assert.EqualValues(t, 2, h.stub.requests.Load())
	assert.NotContains(t, logs.String(), "level=WARN")
}

func TestSpanPull_AnotherServerErrorIsStillRetried(t *testing.T) {
	h := newPullHarness(t, spanpull.KindTempo)
	logs := captureLogs(t)
	h.stub.status.Store(500)
	h.stub.body.Store("ingester unavailable")
	h.errorIn(t, h.tenant, pullTrace)
	*h.clock = time.Now().Add(3 * time.Minute)

	_, err := h.worker.RunOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, jobStage(t, h), "still stage 0: it will be retried")
	assert.Contains(t, logs.String(), "level=WARN")
	assert.Contains(t, logs.String(), "HTTP 500: ingester unavailable")
}

// ---- repair round 1 ----------------------------------------------------------------------------------------

// A job that comes due while the pod restarts (scopes not validated yet) is left alone, then fetched once the
// validation has succeeded: it must not be finished as "delisted".
func TestSpanPull_JobsDueBeforeValidationAreUntouchedThenFetched(t *testing.T) {
	pool := spanPool(t)
	ctx := context.Background()
	tenant := seedSpanTenant(t, pool, "restart")
	stub := newStubTraceStore(t)
	start := time.Now().Add(-time.Minute)
	stub.body.Store(otlpBody(start, 2))
	now := time.Now()
	clock := func() time.Time { return now }
	cfg := gatedConfig(t, stub.srv.URL, tenant.OrgID+":"+tenant.ProjectID)
	queue := spanpull.NewQueue(pool, cfg).WithClock(clock)
	store, err := spanpull.NewTraceStore(cfg, stub.srv.Client())
	require.NoError(t, err)
	worker := &spanpull.Worker{Queue: queue, Spans: spans.NewStore(pool).WithClock(clock), Traces: store, Cfg: cfg, Now: clock}
	_, err = spans.NewStore(pool).EnsurePartitions(ctx)
	require.NoError(t, err)

	// The job was queued by an earlier process that had validated.
	_, err = pool.Exec(ctx, `INSERT INTO span_pull_queue (org_id, trace_id, stage, next_fetch_at, trigger_project_id)
		VALUES ($1::uuid, $2, 0, $3, $4::uuid)`, tenant.OrgID, pullTrace, now.Add(2*time.Minute), tenant.ProjectID)
	require.NoError(t, err)
	now = now.Add(3 * time.Minute)
	snapshot := func() string {
		var s string
		require.NoError(t, pool.QueryRow(ctx, `SELECT stage||'|'||attempts||'|'||next_fetch_at::text FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`,
			tenant.OrgID, pullTrace).Scan(&s))
		return s
	}
	before := snapshot()

	n, err := worker.RunOnce(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing is claimed before validation")
	assert.Equal(t, before, snapshot(), "stage, attempts and next_fetch_at are unchanged")
	assert.Zero(t, stub.requests.Load())

	_, _, err = cfg.ValidateAgainstDB(ctx, pool)
	require.NoError(t, err)
	n, err = worker.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.EqualValues(t, 1, stub.requests.Load())
	assert.Equal(t, 2, countRows(t, pool, `SELECT count(*) FROM spans WHERE org_id = $1 AND source = 'pulled'`, tenant.OrgID))
}

func TestSpanPull_AMultibyteErrorBodyStillRecordsTheFailure(t *testing.T) {
	for name, body := range map[string]string{
		"multibyte": strings.Repeat("错", 300),
		"invalid":   "bad \xff\xfe bytes",
		"nul":       "proxy error \x00\x01 binary",
	} {
		t.Run(name, func(t *testing.T) {
			h := newPullHarness(t, spanpull.KindTempo)
			h.stub.status.Store(502)
			h.stub.body.Store(body)
			h.errorIn(t, h.tenant, pullTrace)
			*h.clock = time.Now().Add(3 * time.Minute)
			_, err := h.worker.RunOnce(context.Background())
			require.NoError(t, err)
			var last string
			var retryAt time.Time
			require.NoError(t, h.pool.QueryRow(context.Background(), `SELECT last_error, next_fetch_at FROM span_pull_queue WHERE org_id = $1 AND trace_id = $2`,
				h.tenant.OrgID, pullTrace).Scan(&last, &retryAt), "last_error was recorded")
			assert.True(t, utf8.ValidString(last))
			assert.LessOrEqual(t, len(last), 500)
			assert.Contains(t, last, "HTTP 502")
			assert.True(t, retryAt.After(h.clock.Add(30*time.Second)), "the retry delay was applied")
		})
	}
}
