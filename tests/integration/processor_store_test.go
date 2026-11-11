package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	ingestservice "github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	processorservice "github.com/NurfitraPujo/sentinel/apps/processor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	sharednats "github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// uniqueProjectName returns a project name that is unique per test invocation
// so concurrent runs of the test suite do not collide on the same project row.
func uniqueProjectNameU13(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-proj-u13-%d", time.Now().UnixNano())
}

// uniqueAPIKey returns a sufficiently long API key value guaranteed to be
// unique for the duration of a single test run.
func uniqueAPIKeyU13(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-api-key-u13-%d", time.Now().UnixNano())
}

// seedProject inserts a project row and returns its generated ID. It also
// registers a cleanup that removes the project (and via ON DELETE CASCADE,
// every row that references it).
func seedProjectU13(t *testing.T, pool *pgxpool.Pool) (projectID string, projectName string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	projectName = uniqueProjectNameU13(t)
	apiKey := uniqueAPIKeyU13(t)

	err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'))
		 RETURNING id::text`,
		projectName, apiKey, apiKey,
	).Scan(&projectID)
	require.NoError(t, err, "seed project")

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM projects WHERE id = $1`, projectID); err != nil {
			t.Logf("cleanup project %s failed: %v", projectID, err)
		}
	})

	return projectID, projectName
}

// postgresConfigFromTest resolves the Postgres connection settings via testcontainers.Setup.
func postgresConfigFromTest(t *testing.T) (host, port, user, password, db string) {
	t.Helper()
	env := tc.Setup(t, tc.WithResources(tc.PostgresResource), tc.WithMigrations(true))
	var hostErr error
	if env.PGConfig.Host == "" {
		hostErr = fmt.Errorf("postgres host unavailable")
	}
	requireInfra(t, hostErr, "postgres host")
	return env.PGConfig.Host, env.PGConfig.Port, env.PGConfig.User, env.PGConfig.Password, env.PGConfig.DB
}

// storeFromTest builds a fresh pgxpool.Pool and constructs an IssueStore backed
// by that pool. The pool is closed automatically when the test ends.
func storeFromTest(t *testing.T) (store.IssueStore, *pgxpool.Pool) {
	t.Helper()

	host, port, user, password, db := postgresConfigFromTest(t)

	dsn := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		user, password, host, port, db,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	require.NoError(t, pool.Ping(ctx))

	t.Cleanup(pool.Close)

	return store.NewStore(pool), pool
}

func TestStorePackage_UpsertIssue_InsertsNewIssue(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	issue := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Environment: "test",
		Fingerprint: fmt.Sprintf("fp-insert-%d", time.Now().UnixNano()),
		Message:     "first occurrence",
		ErrorClass:  "UpsertInsertError",
		Status:      "unresolved",
		FirstSeen:   time.Now().UTC(),
		LastSeen:    time.Now().UTC(),
	}

	require.NoError(t, s.UpsertIssue(ctx, issue, ""))

	// Verify the row was inserted with count = 1.
	var (
		gotID     string
		gotCount  int64
		gotStatus string
	)
	err := pool.QueryRow(ctx,
		`SELECT id::text, count, status FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, issue.Fingerprint,
	).Scan(&gotID, &gotCount, &gotStatus)
	require.NoError(t, err)
	assert.Equal(t, issue.ID, gotID, "inserted issue ID should match")
	assert.Equal(t, int64(1), gotCount, "first insert should set count to 1")
	assert.Equal(t, "unresolved", gotStatus)
}

func TestStorePackage_UpsertIssue_DuplicateIncrementsCountAndUpdatesLastSeen(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fingerprint := fmt.Sprintf("fp-upsert-%d", time.Now().UnixNano())

	first := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Environment: "test",
		Fingerprint: fingerprint,
		Message:     "first error",
		ErrorClass:  "UpsertDuplicateError",
		Status:      "unresolved",
		FirstSeen:   time.Now().UTC().Add(-1 * time.Hour),
		LastSeen:    time.Now().UTC().Add(-1 * time.Hour),
	}
	require.NoError(t, s.UpsertIssue(ctx, first, ""))

	var (
		firstID    string
		firstCount int64
		firstSeen  time.Time
		lastSeen1  time.Time
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text, count, first_seen, last_seen FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, fingerprint,
	).Scan(&firstID, &firstCount, &firstSeen, &lastSeen1))
	require.Equal(t, int64(1), firstCount)

	// A second call must increment count and update last_seen (using GREATEST).
	second := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Environment: "test",
		Fingerprint: fingerprint,
		Message:     "second error",
		ErrorClass:  "UpsertDuplicateError",
		Status:      "unresolved",
		FirstSeen:   time.Now().UTC(),
		LastSeen:    time.Now().UTC(),
	}
	require.NoError(t, s.UpsertIssue(ctx, second, ""))

	var (
		secondID    string
		secondCount int64
		secondSeen  time.Time
		lastSeen2   time.Time
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text, count, first_seen, last_seen FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, fingerprint,
	).Scan(&secondID, &secondCount, &secondSeen, &lastSeen2))

	assert.Equal(t, firstID, secondID, "upsert must keep the original issue ID")
	assert.Equal(t, int64(2), secondCount, "count should increment by 1 on duplicate")
	assert.Equal(t, firstSeen, secondSeen, "first_seen must be preserved on conflict")
	assert.True(t, !lastSeen2.Before(lastSeen1), "last_seen should advance (or stay equal) after duplicate")
}

func TestStorePackage_UpsertIssue_DuplicateWithOlderLastSeenKeepsGreatest(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fingerprint := fmt.Sprintf("fp-greatest-%d", time.Now().UnixNano())
	now := time.Now().UTC()

	first := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Environment: "test",
		Fingerprint: fingerprint,
		Message:     "first",
		ErrorClass:  "GreatestError",
		Status:      "unresolved",
		FirstSeen:   now,
		LastSeen:    now,
	}
	require.NoError(t, s.UpsertIssue(ctx, first, ""))

	// A duplicate call with an older last_seen must NOT regress last_seen.
	older := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Environment: "test",
		Fingerprint: fingerprint,
		Message:     "stale duplicate",
		ErrorClass:  "GreatestError",
		Status:      "unresolved",
		FirstSeen:   now.Add(-2 * time.Hour),
		LastSeen:    now.Add(-2 * time.Hour),
	}
	require.NoError(t, s.UpsertIssue(ctx, older, ""))

	var lastSeen time.Time
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT last_seen FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, fingerprint,
	).Scan(&lastSeen))

	// Compare against the recorded first-seen timestamp, which equals the
	// last_seen we initially wrote.
	assert.WithinDuration(t, now, lastSeen, time.Second,
		"last_seen should not regress on duplicate insert (uses GREATEST)")
}

func TestStorePackage_InsertOccurrence_InsertsRow(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Seed an issue so the foreign key reference is valid.
	issue := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Environment: "test",
		Fingerprint: fmt.Sprintf("fp-occ-%d", time.Now().UnixNano()),
		Message:     "occ insert",
		ErrorClass:  "OccurrenceInsertError",
		Status:      "unresolved",
		FirstSeen:   time.Now().UTC(),
		LastSeen:    time.Now().UTC(),
	}
	require.NoError(t, s.UpsertIssue(ctx, issue, ""))

	stacktrace := json.RawMessage(`[{"file":"main.go","line":1,"function":"main"}]`)
	metadata := json.RawMessage(`{"k":"v"}`)

	occ := &store.ErrorOccurrence{
		ID:          uuid.New().String(),
		IssueID:     issue.ID,
		Environment: "test",
		Platform:    "go",
		Stacktrace:  stacktrace,
		Metadata:    metadata,
		TraceID:     "trace-occ-u13",
		SpanID:      "span-occ-u13",
		CreatedAt:   time.Now().UTC(),
	}

	require.NoError(t, s.InsertOccurrence(ctx, occ))

	var (
		gotID       string
		gotEnv      string
		gotPlatform string
		gotTraceID  string
		gotSpanID   string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id::text, environment, platform, trace_id, span_id FROM error_occurrences WHERE id = $1`,
		occ.ID,
	).Scan(&gotID, &gotEnv, &gotPlatform, &gotTraceID, &gotSpanID))

	assert.Equal(t, occ.ID, gotID)
	assert.Equal(t, "test", gotEnv)
	assert.Equal(t, "go", gotPlatform)
	assert.Equal(t, "trace-occ-u13", gotTraceID)
	assert.Equal(t, "span-occ-u13", gotSpanID)
}

func TestStorePackage_GetProjectByKey_ReturnsID(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, projectName := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := s.GetProjectByKey(ctx, projectName)
	require.NoError(t, err)
	assert.Equal(t, projectID, got)
}

func TestStorePackage_GetProjectByKey_UnknownReturnsError(t *testing.T) {
	s, _ := storeFromTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	unknown := fmt.Sprintf("missing-proj-%d", time.Now().UnixNano())
	got, err := s.GetProjectByKey(ctx, unknown)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "project not found")
}

func TestStorePackage_GetIssueIDByFingerprint_ReturnsID(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fingerprint := fmt.Sprintf("fp-get-%d", time.Now().UnixNano())
	issue := &store.Issue{
		ID:          uuid.New().String(),
		ProjectID:   projectID,
		Environment: "test",
		Fingerprint: fingerprint,
		Message:     "get by fingerprint",
		ErrorClass:  "GetByFingerprintError",
		Status:      "unresolved",
		FirstSeen:   time.Now().UTC(),
		LastSeen:    time.Now().UTC(),
	}
	require.NoError(t, s.UpsertIssue(ctx, issue, ""))

	got, err := s.GetIssueIDByFingerprint(ctx, projectID, "test", fingerprint)
	require.NoError(t, err)
	assert.Equal(t, issue.ID, got)
}

func TestStorePackage_GetIssueIDByFingerprint_UnknownReturnsError(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := s.GetIssueIDByFingerprint(ctx, projectID, "test", "fp-does-not-exist")
	require.Error(t, err)
	assert.Empty(t, got)
}

func TestStorePackage_PersistAuditLog_InsertsRow(t *testing.T) {
	s, pool := storeFromTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	auditID := uuid.New().String()
	resourceID := uuid.New().String()
	metadata := json.RawMessage(`{"src":"u13","ok":true}`)

	entry := &store.AuditLog{
		ID:           auditID,
		Action:       "u13.test.insert",
		ResourceType: "test",
		ResourceID:   &resourceID,
		ActorID:      "actor-u13",
		Metadata:     metadata,
	}

	require.NoError(t, s.PersistAuditLog(ctx, entry))

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM audit_logs WHERE id = $1`, auditID); err != nil {
			t.Logf("cleanup audit log %s failed: %v", auditID, err)
		}
	})

	var (
		gotAction       string
		gotResourceType string
		gotResourceID   *string
		gotActorID      string
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT action, resource_type, resource_id::text, actor_id FROM audit_logs WHERE id = $1`,
		auditID,
	).Scan(&gotAction, &gotResourceType, &gotResourceID, &gotActorID))

	assert.Equal(t, "u13.test.insert", gotAction)
	assert.Equal(t, "test", gotResourceType)
	require.NotNil(t, gotResourceID)
	assert.Equal(t, resourceID, *gotResourceID)
	assert.Equal(t, "actor-u13", gotActorID)
}

func TestStorePackage_PersistAuditLog_FailureIncrementsCounter(t *testing.T) {
	s, _ := storeFromTest(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	before := store.GetAuditPersistFailureCount()

	// Force a failure: a malformed UUID violates the column type. pgx will
	// return an error and the implementation must increment the counter.
	entry := &store.AuditLog{
		ID:           "not-a-uuid",
		Action:       "u13.test.failure",
		ResourceType: "test",
		ActorID:      "actor-u13",
		Metadata:     json.RawMessage(`{}`),
	}

	err := s.PersistAuditLog(ctx, entry)
	require.Error(t, err, "malformed UUID must surface as an error")

	after := store.GetAuditPersistFailureCount()
	assert.Equal(t, before+1, after,
		"PersistAuditLog must increment GetAuditPersistFailureCount on failure")
}

// ---- sentinel-event-schema-v2 §6.1 (TASK-05): the v2 occurrence columns, through StoreEvent.

// TestStoreEvent_V2FieldsPersisted is V5a: every v2 column is written by the occurrence INSERT and
// read back; the six NULLIF strings are NULL for "", trace_flags is NULL without a trace id, and every
// JSONB column is NULL when the occurrence carried none.
func TestStoreEvent_V2FieldsPersisted(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	receivedAt := time.Date(2026, 9, 23, 12, 0, 1, 0, time.UTC)
	flags := int64(1)
	full := eidOccurrence("evt-v5a-full-" + uuid.New().String())
	full.Level = "warning"
	full.Message = "charge [REDACTED_PAN] failed"
	full.ErrorClass = "TypeError"
	full.Logger = "checkout"
	full.ServerName = "web-7f9c-pod-2"
	full.Transaction = "/checkout/pay"
	full.Dist = "4127"
	full.Source = "onerror"
	full.TraceFlags = &flags
	full.ReceivedAt = &receivedAt
	full.SDK = json.RawMessage(`{"name":"sentinel.javascript.browser","version":"1.8.0"}`)
	full.User = json.RawMessage(`{"id":"u1","email":"alice@example.com","geo":{"country_code":"DE"}}`)
	full.Tags = json.RawMessage(`{"tenant_id":"tenant-acme"}`)
	full.Contexts = json.RawMessage(`{"device":{"model":"Pixel 8"}}`)
	full.Request = json.RawMessage(`{"method":"POST","headers":{"Authorization":"***REDACTED***"}}`)
	full.Breadcrumbs = json.RawMessage(`[{"type":"http","message":"POST /api/pay 402"}]`)
	full.Exception = json.RawMessage(`{"values":[{"type":"TypeError","mechanism":{"type":"onerror","handled":false}}]}`)
	_, stored, err := s.StoreEvent(ctx, eidIssue(projectID, "fp-v5a-"+uuid.New().String()), full, "")
	require.NoError(t, err)
	require.True(t, stored)

	var (
		level, errorClass                                   string
		message, logger, serverName, transaction, dist, src *string
		traceFlags                                          *int64
		gotReceivedAt                                       *time.Time
		sdk, user, tags, contexts, request, crumbs, exc     *string
	)
	readBack := func(id string) {
		t.Helper()
		require.NoError(t, pool.QueryRow(ctx, `
			SELECT level, message, error_class, logger, server_name, transaction, dist, source, trace_flags, received_at,
			       sdk::text, event_user::text, tags::text, contexts::text, request::text, breadcrumbs::text, exception::text
			  FROM error_occurrences WHERE id = $1`, id).Scan(
			&level, &message, &errorClass, &logger, &serverName, &transaction, &dist, &src, &traceFlags, &gotReceivedAt,
			&sdk, &user, &tags, &contexts, &request, &crumbs, &exc))
	}
	readBack(full.ID)
	assert.Equal(t, "warning", level)
	assert.Equal(t, "TypeError", errorClass)
	for name, got := range map[string]*string{"message": message, "logger": logger, "server_name": serverName, "transaction": transaction, "dist": dist, "source": src} {
		require.NotNil(t, got, "%s must be stored", name)
	}
	assert.Equal(t, "charge [REDACTED_PAN] failed", *message)
	assert.Equal(t, "checkout", *logger)
	assert.Equal(t, "web-7f9c-pod-2", *serverName)
	assert.Equal(t, "/checkout/pay", *transaction)
	assert.Equal(t, "4127", *dist)
	assert.Equal(t, "onerror", *src)
	require.NotNil(t, traceFlags)
	assert.Equal(t, int64(1), *traceFlags)
	require.NotNil(t, gotReceivedAt)
	assert.True(t, gotReceivedAt.Equal(receivedAt))
	for name, pair := range map[string][2]interface{}{
		"sdk": {sdk, full.SDK}, "event_user": {user, full.User}, "tags": {tags, full.Tags}, "contexts": {contexts, full.Contexts},
		"request": {request, full.Request}, "breadcrumbs": {crumbs, full.Breadcrumbs}, "exception": {exc, full.Exception},
	} {
		got := pair[0].(*string)
		require.NotNil(t, got, "%s must be stored", name)
		assert.JSONEq(t, string(pair[1].(json.RawMessage)), *got, name)
	}

	// The empty occurrence: "" strings are NULL through NULLIF, no trace flags, every JSONB NULL.
	empty := eidOccurrence("evt-v5a-empty-" + uuid.New().String())
	empty.Level = "error"
	empty.ErrorClass = "E"
	empty.TraceID = ""
	_, stored, err = s.StoreEvent(ctx, eidIssue(projectID, "fp-v5a-empty-"+uuid.New().String()), empty, "")
	require.NoError(t, err)
	require.True(t, stored)
	readBack(empty.ID)
	assert.Equal(t, "error", level)
	for name, got := range map[string]*string{"message": message, "logger": logger, "server_name": serverName, "transaction": transaction, "dist": dist, "source": src,
		"sdk": sdk, "event_user": user, "tags": tags, "contexts": contexts, "request": request, "breadcrumbs": crumbs, "exception": exc} {
		assert.Nil(t, got, "%s must be NULL for an occurrence that carried none", name)
	}
	assert.Nil(t, traceFlags, "trace_flags is NULL without a trace id")
	assert.Nil(t, gotReceivedAt)
}

// TestStoreEvent_PerOccurrenceMessage is V5a's second half (spec P8): two occurrences of one issue with
// different messages each keep their own message and class; issues.message keeps the first.
func TestStoreEvent_PerOccurrenceMessage(t *testing.T) {
	s, pool := storeFromTest(t)
	projectID, _ := seedProjectU13(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := "fp-v5a-per-occ-" + uuid.New().String()
	var ids []string
	for i, msg := range []string{"first failure for order 1", "second failure for order 2"} {
		issue := eidIssue(projectID, fp)
		issue.Message = msg
		occ := eidOccurrence(fmt.Sprintf("evt-v5a-per-%d-%s", i, uuid.New().String()))
		occ.Level = "error"
		occ.Message = msg
		occ.ErrorClass = fmt.Sprintf("Class%d", i)
		_, stored, err := s.StoreEvent(ctx, issue, occ, "")
		require.NoError(t, err)
		require.True(t, stored)
		ids = append(ids, occ.ID)
	}
	for i, want := range []string{"first failure for order 1", "second failure for order 2"} {
		var msg, class string
		require.NoError(t, pool.QueryRow(ctx, `SELECT message, error_class FROM error_occurrences WHERE id = $1`, ids[i]).Scan(&msg, &class))
		assert.Equal(t, want, msg)
		assert.Equal(t, fmt.Sprintf("Class%d", i), class)
	}
	var issueMessage string
	require.NoError(t, pool.QueryRow(ctx, `SELECT message FROM issues WHERE project_id = $1 AND fingerprint = $2`, projectID, fp).Scan(&issueMessage))
	assert.Equal(t, "first failure for order 1", issueMessage, "issues.message is written on the INSERT arm only (T6)")
}

// TestStoreEvent_NULInOccurrenceMessage is V5e (spec P15, §6.1): an occurrence of an EXISTING issue
// whose message carries U+0000 commits, with the NUL removed from the stored occurrence message. It
// goes through ProcessorService, because the strip is the service's fill (fillOccurrenceV2): the
// ingestor leaves top-level fields as sent, and Postgres would reject the NUL with 22021 (class 22,
// Permanent), dead-lettering an occurrence that stored fine before this change.
func TestStoreEvent_NULInOccurrenceMessage(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)
	projectName := uniqueProjectName("v5e-nul")
	projectID := createTestProject(t, pool, projectName, uniqueAPIKey("v5e-nul-key"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	first := buildValidProtoEvent(projectName)
	first.EventId = "evt-v5e-first-" + uuid.New().String()
	data, err := proto.Marshal(first)
	require.NoError(t, err)
	require.NoError(t, svc.ProcessEvent(ctx, data), "the first occurrence creates the issue")

	second := buildValidProtoEvent(projectName) // same fingerprint: the same, existing issue
	second.EventId = "evt-v5e-second-" + uuid.New().String()
	second.Message = "a\x00b"
	data, err = proto.Marshal(second)
	require.NoError(t, err)
	require.NoError(t, svc.ProcessEvent(ctx, data), "a NUL in the message must not dead-letter the occurrence")

	var msg string
	require.NoError(t, pool.QueryRow(ctx, `SELECT message FROM error_occurrences WHERE event_id = $1`, second.EventId).Scan(&msg))
	assert.Equal(t, "ab", msg)
	var count int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT count FROM issues WHERE project_id = $1`, projectID).Scan(&count))
	assert.Equal(t, int64(2), count)
}

// TestStoreEvent_NULInTagKeyAndContextName is the integration case for review finding
// esv2-rev-nul-tag-key-dlq (spec §4.2 NUL row, I7c): a U+0000 in a tag key, and in a context name that
// collides with one the client sent as-is, used to reach metadata.server_truncated_keys raw. The
// processor stores metadata as JSONB, Postgres rejects \u0000 there with 22P05 (class 22, Permanent),
// and the event was dead-lettered. It runs the real ingestor bounds step and mapper, then the processor,
// and requires the occurrence to be stored with the tag key stripped to its valid form.
func TestStoreEvent_NULInTagKeyAndContextName(t *testing.T) {
	svc, pool, _ := newServiceAndPoolFromEnv(t)
	projectName := uniqueProjectName("v2-nul-key")
	projectID := createTestProject(t, pool, projectName, uniqueAPIKey("v2-nul-key"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	payload := &validation.ErrorPayload{
		EventID:     "evt-v2-nul-key-" + uuid.New().String(),
		ProjectKey:  projectName,
		Platform:    "go",
		Environment: "test",
		Message:     "integration test error",
		ErrorClass:  "IntegrationRuntimeError",
		Fingerprint: "fp-nul-" + uuid.New().String(),
		Timestamp:   time.Now().UTC(),
		Metadata:    map[string]interface{}{"user_id": "u-nul"},
		Tags:        map[string]string{"env\x00x": "v"},
		Contexts: map[string]map[string]interface{}{
			"dev\x00ice": {"model": "stripped"},
			"device":     {"model": "sent"},
		},
	}
	res := validation.ValidatePayload(payload)
	require.True(t, res.Valid, "%+v", res.Errors)
	data, err := proto.Marshal(mapping.MapPayloadToEvent(payload))
	require.NoError(t, err)
	require.NoError(t, svc.ProcessEvent(ctx, data), "a NUL in a tag key or context name must not dead-letter the event")

	var metadata, tags, contexts string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT metadata::text, tags::text, contexts::text FROM error_occurrences WHERE event_id = $1`, payload.EventID).
		Scan(&metadata, &tags, &contexts))
	assert.JSONEq(t, `{"envx":"v"}`, tags, "the tag key is stripped to its valid form, not dropped")
	assert.JSONEq(t, `{"device":{"model":"sent"}}`, contexts)
	var md map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(metadata), &md))
	assert.NotContains(t, md, "server_truncated_keys", "contexts.device is stored, so nothing is recorded as removed (esv2-rev2-nul-collision-markers)")
	assert.Equal(t, []interface{}{"tags", "contexts"}, md["server_truncated"])
	assert.Equal(t, "u-nul", md["user_id"])
}

// capturePublisher records what IngestService.IngestAt publishes, so a test can hand the exact wire
// bytes to the processor without a NATS round trip.
type capturePublisher struct{ published [][]byte }

func (c *capturePublisher) PublishWithHeaders(_ context.Context, data []byte, _ sharednats.Header) error {
	c.published = append(c.published, append([]byte(nil), data...))
	return nil
}

// TestStoreEvent_LevelDefaultsToError is spec P1/I6 through ProcessorService: an event with no level,
// or an invalid one the ingestor replaced with "", is stored with level 'error' (fillOccurrenceV2).
// Every other integration test sets occ.Level by hand at the store layer, so without this one a
// regression would store NULL for every event from today's SDKs, none of which sends a level (review
// finding esv2-rev2-level-default-untested).
//
// storeFromTest, not newServiceAndPoolFromEnv: the latter skips when its env-configured Postgres is
// gone, which is how every test on it skipped in a full `task test-integration` run on this change.
func TestStoreEvent_LevelDefaultsToError(t *testing.T) {
	_, pool := storeFromTest(t)
	svc := processorservice.NewProcessorService(pool)
	projectName := uniqueProjectName("v2-level")
	projectID := createTestProject(t, pool, projectName, uniqueAPIKey("v2-level-key"))
	t.Cleanup(func() { cleanupServiceRows(t, pool, projectID) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	storedLevel := func(t *testing.T, eventID string) *string {
		t.Helper()
		var level *string
		require.NoError(t, pool.QueryRow(ctx, `SELECT level FROM error_occurrences WHERE event_id = $1`, eventID).Scan(&level))
		return level
	}

	t.Run("a pre-v2 proto with no level", func(t *testing.T) {
		evt := buildValidProtoEvent(projectName)
		evt.EventId = "evt-v2-level-none-" + uuid.New().String()
		require.Empty(t, evt.GetLevel())
		data, err := proto.Marshal(evt)
		require.NoError(t, err)
		require.NoError(t, svc.ProcessEvent(ctx, data))
		level := storedLevel(t, evt.EventId)
		require.NotNil(t, level, "level must never be stored NULL")
		assert.Equal(t, "error", *level)
	})

	t.Run("level WARN through IngestAt", func(t *testing.T) {
		pub := &capturePublisher{}
		ingest, err := ingestservice.NewIngestService(pub)
		require.NoError(t, err)
		payload := &validation.ErrorPayload{
			EventID:     "evt-v2-level-warn-" + uuid.New().String(),
			ProjectKey:  projectName,
			Platform:    "go",
			Environment: "test",
			Message:     "integration test error",
			ErrorClass:  "IntegrationRuntimeError",
			Fingerprint: "fp-level-" + uuid.New().String(),
			Timestamp:   time.Now().UTC(),
			Level:       "WARN",
		}
		require.NoError(t, ingest.IngestAt(ctx, payload, time.Now().UTC()))
		require.Len(t, pub.published, 1)
		require.NoError(t, svc.ProcessEvent(ctx, pub.published[0]))
		level := storedLevel(t, payload.EventID)
		require.NotNil(t, level, "level must never be stored NULL")
		assert.Equal(t, "error", *level, "the ingestor replaces an invalid level with \"\", which is stored as error")
	})
}

// sentinel-retention-resolved-ignored-kept contract §5.3: retention keeps a resolved issue's row after
// every one of its occurrences has aged out. This pins that the regression path reads only the issue
// row and `releases`, never `error_occurrences`: a resolved issue with ZERO occurrence rows, receiving
// one event in a later release, comes back unresolved/regressed on the SAME issue id.
func TestStoreEvent_ResolvedIssueWithZeroOccurrencesRegresses(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "zero-occ-regress")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fingerprint := eidFingerprint(t, "zero-occ-regress")

	// Seed through an ordinary delivery in 1.0.0 (this also writes the releases row), resolve it
	// there, then play the retention sweep: every occurrence row is gone, the issue row stays.
	issue0 := eidIssue(projectID, fingerprint)
	_, stored0, err := s.StoreEvent(ctx, issue0, eidOccurrence("evt-zero-occ-seed"), "1.0.0")
	require.NoError(t, err)
	require.True(t, stored0)

	_, err = pool.Exec(ctx,
		`UPDATE issues SET status = 'resolved', resolved_at = now(), resolved_in_version = '1.0.0',
		        resolved_by_type = 'user', resolved_by = 'integration-test'
		  WHERE id = $1`, issue0.ID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DELETE FROM error_occurrences WHERE issue_id = $1`, issue0.ID)
	require.NoError(t, err)

	var occurrences int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM error_occurrences WHERE issue_id = $1`, issue0.ID).Scan(&occurrences))
	require.Zero(t, occurrences, "precondition: the issue has no occurrence rows left")

	outcome, stored, err := s.StoreEvent(ctx, eidIssue(projectID, fingerprint),
		eidOccurrence("evt-zero-occ-regress-"+uuid.New().String()), "2.0.0")
	require.NoError(t, err)
	require.True(t, stored)
	require.Equal(t, store.IssueOutcomeRegressed, outcome)

	var status, regressionStatus string
	var regressionCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT status, COALESCE(regression_status, ''), regression_count FROM issues WHERE id = $1`,
		issue0.ID).Scan(&status, &regressionStatus, &regressionCount))
	require.Equal(t, "unresolved", status)
	require.Equal(t, "regressed", regressionStatus)
	require.Equal(t, 1, regressionCount)

	var issueRows int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, fingerprint).Scan(&issueRows))
	require.Equal(t, 1, issueRows, "the regression must land on the existing issue, not a new one")
}
