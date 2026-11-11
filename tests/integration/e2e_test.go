package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type TestConfig struct {
	PostgresHost     string
	PostgresUser     string
	PostgresPassword string
	PostgresDB       string
	NATSURL          string
	IngesterURL      string
}

func newPostgresPool(t *testing.T, cfg TestConfig) *pgxpool.Pool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		cfg.PostgresUser, cfg.PostgresPassword, cfg.PostgresHost,
		os.Getenv("POSTGRES_PORT"), cfg.PostgresDB,
	))
	requireInfra(t, err, "postgres connection")

	pingErr := pool.Ping(ctx)
	if pingErr != nil {
		pool.Close()
	}
	requireInfra(t, pingErr, "postgres ping")

	return pool
}

// createTestProject seeds a project AND the project_api_keys row the ingestor actually
// authenticates against.
//
// Feature 008 moved authentication onto project_api_keys; apps/ingestor-go/auth/apikey.go
// getAPIKeyData looks the presented key up by pak.key_hash. Seeding only projects.api_key_hash — as
// this helper used to — makes every request 401 no matter what the rest of the pipeline does, which
// is exactly how TestIngestAndProcess and TestSearchIndexing failed while asserting 202.
func createTestProject(t *testing.T, pool *pgxpool.Pool, projectName, apiKey string) string {
	ctx := context.Background()

	var orgID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (name, slug) VALUES ($1, $1) RETURNING id::text`,
		projectName+"-org",
	).Scan(&orgID); err != nil {
		t.Fatalf("Failed to create test organization: %v", err)
	}

	var projectID string
	err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash, organization_id)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'), $4)
		 RETURNING id::text`,
		projectName, apiKey, apiKey, orgID,
	).Scan(&projectID)
	if err != nil {
		t.Fatalf("Failed to create test project: %v", err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO project_api_keys
		   (project_id, organization_id, key_hash, key_prefix, name, scope, status, rate_limit_rpm, created_by)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'), 'test', $4, 'ingest', 'active', 100000, gen_random_uuid())`,
		projectID, orgID, apiKey, projectName+"-key",
	); err != nil {
		t.Fatalf("Failed to create test API key: %v", err)
	}

	return projectID
}

// cleanupProject removes the project AND the organization createTestProject made for it.
//
// It used to leave the organization behind. createTestProject derives the org slug from a FIXED project
// name (no unique suffix), and organizations.slug is UNIQUE — so the leaked row made every one of these
// tests fail on its SECOND run against the same database with
// "duplicate key value violates unique constraint organizations_slug_key". They passed in CI only because
// CI tears the volume down between runs; on any reused database they were single-use. Deleting the
// organization is what makes them re-runnable.
func cleanupProject(t *testing.T, pool *pgxpool.Pool, projectID string) {
	ctx := context.Background()

	// Capture the org before the project row disappears — it is the only link to it.
	var orgID string
	if err := pool.QueryRow(ctx,
		`SELECT organization_id::text FROM projects WHERE id = $1`, projectID).Scan(&orgID); err != nil {
		t.Logf("Could not resolve the organization for project %s (already gone?): %v", projectID, err)
	}

	// Occurrence and issue rows reference the project, and FKs are not all ON DELETE CASCADE.
	_, _ = pool.Exec(ctx, `DELETE FROM error_search_index WHERE occurrence_id IN (
	                         SELECT eo.id FROM error_occurrences eo
	                         JOIN issues i ON i.id = eo.issue_id WHERE i.project_id = $1)`, projectID)
	_, _ = pool.Exec(ctx, `DELETE FROM error_occurrences WHERE issue_id IN (
	                         SELECT id FROM issues WHERE project_id = $1)`, projectID)
	_, _ = pool.Exec(ctx, `DELETE FROM issues WHERE project_id = $1`, projectID)

	// project_api_keys FKs to projects, so it must go first.
	_, _ = pool.Exec(ctx, `DELETE FROM project_api_keys WHERE project_id = $1`, projectID)
	if _, err := pool.Exec(ctx, `DELETE FROM projects WHERE id = $1`, projectID); err != nil {
		t.Logf("Failed to cleanup project %s: %v", projectID, err)
	}

	if orgID != "" {
		_, _ = pool.Exec(ctx, `DELETE FROM project_api_keys WHERE organization_id = $1`, orgID)
		if _, err := pool.Exec(ctx, `DELETE FROM organizations WHERE id = $1`, orgID); err != nil {
			t.Logf("Failed to cleanup organization %s: %v", orgID, err)
		}
	}
}

func sendErrorEvent(t *testing.T, ingesterURL, apiKey string, payload map[string]interface{}) *http.Response {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Failed to marshal payload: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, ingesterURL+"/ingest", bytes.NewReader(payloadJSON))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	requireInfra(t, err, fmt.Sprintf("ingester HTTP service at %s", ingesterURL))
	return resp
}

func getIssueCount(t *testing.T, pool *pgxpool.Pool, projectID, fingerprint string) int64 {
	var count int64
	err := pool.QueryRow(context.Background(),
		`SELECT i.count FROM issues i WHERE i.project_id = $1 AND i.fingerprint = $2`,
		projectID, fingerprint,
	).Scan(&count)
	if err == pgx.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatalf("Failed to query issue count: %v", err)
	}
	return count
}

func getIssueByFingerprint(t *testing.T, pool *pgxpool.Pool, projectID, fingerprint string) (string, int64) {
	var id string
	var count int64
	err := pool.QueryRow(context.Background(),
		`SELECT id::text, count FROM issues WHERE project_id = $1 AND fingerprint = $2`,
		projectID, fingerprint,
	).Scan(&id, &count)
	if err == pgx.ErrNoRows {
		return "", 0
	}
	if err != nil {
		t.Fatalf("Failed to query issue: %v", err)
	}
	return id, count
}

func computeFingerprint(errorClass string, stacktrace []map[string]interface{}) string {
	const maxAppFrames = 3
	var appFrames []string
	for _, frame := range stacktrace {
		if inApp, ok := frame["in_app"].(bool); ok && inApp {
			file := frame["file"].(string)
			function := frame["function"].(string)
			appFrames = append(appFrames, fmt.Sprintf("%s:%s", file, function))
			if len(appFrames) >= maxAppFrames {
				break
			}
		}
	}

	input := errorClass
	if len(appFrames) > 0 {
		input += "|" + strings.Join(appFrames, "|")
	}

	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])[:16]
}

func TestIngestAndProcess(t *testing.T) {
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
		NATSURL:          os.Getenv("NATS_URL"),
		IngesterURL:      os.Getenv("INGESTOR_URL"),
	}
	pool := newPostgresPool(t, cfg)
	defer pool.Close()

	projectName := "test-project-ingest"
	apiKey := "test-api-key-ingest-123"

	projectID := createTestProject(t, pool, projectName, apiKey)
	defer cleanupProject(t, pool, projectID)

	// Debug: verify project was created and show count
	var projCount int64
	pool.QueryRow(context.Background(), "SELECT count(*) FROM projects").Scan(&projCount)
	t.Logf("Created project: name=%s, id=%s, api_key=%s, total projects in DB=%d", projectName, projectID, apiKey, projCount)

	// Debug: verify project exists by fetching it back
	var verifyName string
	err := pool.QueryRow(context.Background(), "SELECT name FROM projects WHERE id = $1", projectID).Scan(&verifyName)
	if err != nil {
		t.Fatalf("Failed to verify project exists after creation: %v", err)
	}
	t.Logf("Verified project exists: id=%s, name=%s", projectID, verifyName)

	payload := map[string]interface{}{
		"project_key": projectName,
		"platform":    "go",
		"environment": "test",
		"message":     "Test error message",
		"error_class": "TestError",
		"trace_id":    "trace-001",
		"span_id":     "span-001",
		"stacktrace": []map[string]interface{}{
			{"file": "main.go", "line": 42, "function": "main", "in_app": true},
		},
		"metadata":    map[string]interface{}{},
		"timestamp":   time.Now(),
		"trace_flags": 0,
	}

	resp := sendErrorEvent(t, cfg.IngesterURL, apiKey, payload)
	if resp == nil {
		return
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("Expected status 202, got %d", resp.StatusCode)
	}

	time.Sleep(5 * time.Second)

	fingerprint := computeFingerprint("TestError", payload["stacktrace"].([]map[string]interface{}))
	count := getIssueCount(t, pool, projectID, fingerprint)

	// Debug: log issue count
	var dbCount int64
	pool.QueryRow(context.Background(), "SELECT count(*) FROM issues WHERE project_id = $1", projectID).Scan(&dbCount)
	t.Logf("Issue count check: fingerprint=%s, countFromFn=%d, countDirect=%d", fingerprint, count, dbCount)

	assert.Equal(t, int64(1), count, "Expected issue count to be 1 after first event")

	for i := 0; i < 9; i++ {
		resp := sendErrorEvent(t, cfg.IngesterURL, apiKey, payload)
		resp.Body.Close()
	}

	time.Sleep(5 * time.Second)

	count = getIssueCount(t, pool, projectID, fingerprint)
	assert.Equal(t, int64(10), count, "Expected issue count to be 10 after 10 identical events")
}

func TestSearchIndexing(t *testing.T) {
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
		NATSURL:          os.Getenv("NATS_URL"),
		IngesterURL:      os.Getenv("INGESTOR_URL"),
	}
	pool := newPostgresPool(t, cfg)
	defer pool.Close()

	projectName := "test-project-search"
	apiKey := "test-api-key-search-456"

	projectID := createTestProject(t, pool, projectName, apiKey)
	defer cleanupProject(t, pool, projectID)

	payload := map[string]interface{}{
		"project_key": projectName,
		"platform":    "go",
		"environment": "test",
		"message":     "Search indexing test error",
		"error_class": "SearchError",
		"trace_id":    "abc123",
		"span_id":     "span-002",
		"stacktrace": []map[string]interface{}{
			{"file": "main.go", "line": 100, "function": "handleRequest", "in_app": true},
		},
		"metadata": map[string]interface{}{
			"user_id":   "user123",
			"tenant_id": "tenant456",
		},
		"timestamp":   time.Now(),
		"trace_flags": 0,
	}

	resp := sendErrorEvent(t, cfg.IngesterURL, apiKey, payload)
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("Expected status 202, got %d", resp.StatusCode)
	}

	time.Sleep(5 * time.Second)

	var occurrenceID, userID, tenantID, traceID string
	err := pool.QueryRow(context.Background(),
		`SELECT esi.occurrence_id::text, esi.user_id, esi.tenant_id, esi.trace_id
		 FROM error_search_index esi
		 JOIN error_occurrences eo ON eo.id = esi.occurrence_id
		 JOIN issues i ON i.id = eo.issue_id
		 WHERE i.project_id = $1 AND esi.trace_id = $2
		 LIMIT 1`,
		projectID, "abc123",
	).Scan(&occurrenceID, &userID, &tenantID, &traceID)

	if err != nil {
		t.Fatalf("Failed to query error_search_index: %v", err)
	}

	assert.Equal(t, "user123", userID, "Expected user_id to be user123")
	assert.Equal(t, "tenant456", tenantID, "Expected tenant_id to be tenant456")
	assert.Equal(t, "abc123", traceID, "Expected trace_id to be abc123")
}

func TestFingerprinting(t *testing.T) {
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
		NATSURL:          os.Getenv("NATS_URL"),
		IngesterURL:      os.Getenv("INGESTOR_URL"),
	}
	pool := newPostgresPool(t, cfg)
	defer pool.Close()

	projectName := "test-project-fingerprint"
	apiKey := "test-api-key-fingerprint-789"

	projectID := createTestProject(t, pool, projectName, apiKey)
	defer cleanupProject(t, pool, projectID)

	payload1 := map[string]interface{}{
		"project_key": projectName,
		"platform":    "go",
		"environment": "test",
		"message":     "Error message one",
		"error_class": "FingerprintError",
		"trace_id":    "trace-fp-001",
		"span_id":     "span-fp-001",
		"stacktrace": []map[string]interface{}{
			{"file": "main.go", "line": 10, "function": "funcA", "in_app": true},
			{"file": "main.go", "line": 20, "function": "funcB", "in_app": true},
			{"file": "main.go", "line": 30, "function": "funcC", "in_app": true},
		},
		"metadata":    map[string]interface{}{},
		"timestamp":   time.Now(),
		"trace_flags": 0,
	}

	resp := sendErrorEvent(t, cfg.IngesterURL, apiKey, payload1)
	resp.Body.Close()
	time.Sleep(5 * time.Second)

	payload2 := map[string]interface{}{
		"project_key": projectName,
		"platform":    "go",
		"environment": "test",
		"message":     "Error message two - different",
		"error_class": "FingerprintError",
		"trace_id":    "trace-fp-002",
		"span_id":     "span-fp-002",
		"stacktrace": []map[string]interface{}{
			{"file": "main.go", "line": 10, "function": "funcA", "in_app": true},
			{"file": "main.go", "line": 20, "function": "funcB", "in_app": true},
			{"file": "main.go", "line": 30, "function": "funcC", "in_app": true},
		},
		"metadata":    map[string]interface{}{},
		"timestamp":   time.Now(),
		"trace_flags": 0,
	}

	resp = sendErrorEvent(t, cfg.IngesterURL, apiKey, payload2)
	resp.Body.Close()
	time.Sleep(5 * time.Second)

	_, count1 := getIssueByFingerprint(t, pool, projectID, computeFingerprint("FingerprintError", payload1["stacktrace"].([]map[string]interface{})))
	_, count2 := getIssueByFingerprint(t, pool, projectID, computeFingerprint("FingerprintError", payload2["stacktrace"].([]map[string]interface{})))

	assert.Equal(t, count1, count2, "Same fingerprint expected for same error class and stack frames")

	payload3 := map[string]interface{}{
		"project_key": projectName,
		"platform":    "go",
		"environment": "test",
		"message":     "Error with different stack frame position",
		"error_class": "FingerprintError",
		"trace_id":    "trace-fp-003",
		"span_id":     "span-fp-003",
		"stacktrace": []map[string]interface{}{
			{"file": "main.go", "line": 10, "function": "funcA", "in_app": true},
			{"file": "utils.go", "line": 20, "function": "funcB", "in_app": true},
			{"file": "main.go", "line": 50, "function": "funcC", "in_app": true},
		},
		"metadata":    map[string]interface{}{},
		"timestamp":   time.Now(),
		"trace_flags": 0,
	}

	resp = sendErrorEvent(t, cfg.IngesterURL, apiKey, payload3)
	resp.Body.Close()
	time.Sleep(5 * time.Second)

	fp1 := computeFingerprint("FingerprintError", payload1["stacktrace"].([]map[string]interface{}))
	fp3 := computeFingerprint("FingerprintError", payload3["stacktrace"].([]map[string]interface{}))

	assert.NotEqual(t, fp1, fp3, "Different fingerprints expected when first 3 in_app frames differ")
}

// TestIngestAndProcess_NoTimestampIsStoredAtReceipt is the end-to-end proof of
// sentinel-ingest-timestamp-clamp (contract §11 row 5, flows W1 → D1–D3 / R1): a client that omits
// `timestamp` — every non-Go client following docs/sdk-specification.md §3 — used to get an
// occurrence stored at year 1 and swept within the hour. After the rule, the ingestor's receipt time
// is what reaches error_occurrences.created_at, and the 202 body echoes exactly that value
// (decision 12), so the row is read back BY the echoed event_id and compared at microsecond
// precision — TIMESTAMP WITH TIME ZONE keeps microseconds, the wire keeps nanoseconds (decision 15).
func TestIngestAndProcess_NoTimestampIsStoredAtReceipt(t *testing.T) {
	cfg := TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
		NATSURL:          os.Getenv("NATS_URL"),
		IngesterURL:      os.Getenv("INGESTOR_URL"),
	}
	pool := newPostgresPool(t, cfg)
	defer pool.Close()

	projectName := "test-project-ingest-nots"
	apiKey := "test-api-key-ingest-nots-789"

	projectID := createTestProject(t, pool, projectName, apiKey)
	defer cleanupProject(t, pool, projectID)

	// No "timestamp" key at all — the R1 shape a curl client produces.
	payload := map[string]interface{}{
		"project_key": projectName,
		"platform":    "python",
		"environment": "test",
		"message":     "posted without a timestamp",
		"error_class": "NoTimestampError",
		"stacktrace": []map[string]interface{}{
			{"file": "app.py", "line": 7, "function": "handler", "in_app": true},
		},
	}

	before := time.Now().UTC()
	resp := sendErrorEvent(t, cfg.IngesterURL, apiKey, payload)
	if resp == nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("Expected status 202, got %d", resp.StatusCode)
	}

	var body struct {
		Status    string `json:"status"`
		EventID   string `json:"event_id"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("Failed to decode 202 body: %v", err)
	}
	if body.EventID == "" {
		t.Fatalf("202 body carries no event_id: %+v", body)
	}
	echoed, err := time.Parse(time.RFC3339Nano, body.Timestamp)
	if err != nil {
		t.Fatalf("202 body timestamp %q is not RFC3339Nano: %v", body.Timestamp, err)
	}
	t.Logf("202 body: status=%s event_id=%s timestamp=%s", body.Status, body.EventID, body.Timestamp)

	// The processor consumes asynchronously; poll for the row by the echoed event_id rather than
	// sleeping a fixed interval.
	var createdAt time.Time
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := pool.QueryRow(context.Background(),
			`SELECT o.created_at FROM error_occurrences o
			   JOIN issues i ON i.id = o.issue_id
			  WHERE i.project_id = $1 AND o.event_id = $2`,
			projectID, body.EventID,
		).Scan(&createdAt)
		if err == nil {
			break
		}
		if err != pgx.ErrNoRows {
			t.Fatalf("Failed to query occurrence by event_id: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("occurrence with event_id %s never appeared in error_occurrences", body.EventID)
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("error_occurrences.created_at=%s for event_id=%s", createdAt.UTC().Format(time.RFC3339Nano), body.EventID)

	// Not year 1 (the pre-feature behaviour), within 10 s of the test's own clock, and equal to the
	// echoed value once both are truncated to the microsecond (decision 15).
	assert.NotEqual(t, 1, createdAt.Year(), "created_at must not be the zero time's year 1")
	assert.False(t, createdAt.Before(before.Add(-10*time.Second)) || createdAt.After(before.Add(10*time.Second)),
		"created_at %v is not within 10 s of the request time %v", createdAt, before)
	assert.True(t, echoed.Truncate(time.Microsecond).Equal(createdAt.Truncate(time.Microsecond)),
		"the 202 body's timestamp (%s) must equal the stored created_at (%s) at microsecond precision",
		echoed.Format(time.RFC3339Nano), createdAt.UTC().Format(time.RFC3339Nano))
}

// ---- sentinel-event-schema-v2 (TASK-02, TASK-05): v2 events through the testcontainers ingestor and
// processor into error_occurrences.

func eventV2E2EConfig() TestConfig {
	return TestConfig{
		PostgresHost:     os.Getenv("POSTGRES_HOST"),
		PostgresUser:     os.Getenv("POSTGRES_USER"),
		PostgresPassword: os.Getenv("POSTGRES_PASSWORD"),
		PostgresDB:       os.Getenv("POSTGRES_DB"),
		NATSURL:          os.Getenv("NATS_URL"),
		IngesterURL:      os.Getenv("INGESTOR_URL"),
	}
}

// loadEventV2FullBody returns the shared fixture as a JSON object with project_key and event_id set.
func loadEventV2FullBody(t *testing.T, projectKey, eventID string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile("../fixtures/events/event-v2-full.json")
	require.NoError(t, err)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &body))
	body["project_key"] = projectKey
	body["event_id"] = eventID
	delete(body, "timestamp") // stored at receipt, inside the window
	return body
}

func postJSON(t *testing.T, url, apiKey string, body interface{}) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	requireInfra(t, err, fmt.Sprintf("ingester HTTP service at %s", url))
	return resp
}

// waitForOccurrence polls until the processor has stored the occurrence of eventID, and returns its id.
func waitForOccurrence(t *testing.T, pool *pgxpool.Pool, projectID, eventID string) string {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for {
		var id string
		err := pool.QueryRow(context.Background(),
			`SELECT id::text FROM error_occurrences WHERE project_id = $1 AND event_id = $2`, projectID, eventID).Scan(&id)
		if err == nil {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("occurrence of event %s was not stored within 45s: %v", eventID, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestEventV2_JSONFingerprintAndSourceStored is V2d: two events with different error_class, the same
// JSON fingerprint and source "onerror" are grouped into exactly one issue whose fingerprint is the value
// sent, and both occurrence rows store source.
func TestEventV2_JSONFingerprintAndSourceStored(t *testing.T) {
	cfg := eventV2E2EConfig()
	pool := newPostgresPool(t, cfg)
	defer pool.Close()
	projectName := fmt.Sprintf("esv2-v2d-%d", time.Now().UnixNano())
	apiKey := "esv2-v2d-key-" + projectName
	projectID := createTestProject(t, pool, projectName, apiKey)
	defer cleanupProject(t, pool, projectID)

	fp := fmt.Sprintf("fp-json-%d", time.Now().UnixNano())
	var eventIDs []string
	for i, class := range []string{"FirstClass", "SecondClass"} {
		eventID := fmt.Sprintf("evt-v2d-%d-%d", i, time.Now().UnixNano())
		eventIDs = append(eventIDs, eventID)
		resp := postJSON(t, cfg.IngesterURL+"/ingest", apiKey, map[string]interface{}{
			"project_key": projectName, "platform": "javascript", "environment": "test",
			"error_class": class, "message": "grouped by the client", "fingerprint": fp,
			"source": "onerror", "event_id": eventID,
		})
		resp.Body.Close()
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
	}
	for _, eventID := range eventIDs {
		waitForOccurrence(t, pool, projectID, eventID)
	}

	var issues int
	var gotFP string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*), max(fingerprint) FROM issues WHERE project_id = $1`, projectID).Scan(&issues, &gotFP))
	assert.Equal(t, 1, issues, "one issue: the client fingerprint groups both classes")
	assert.Equal(t, fp, gotFP, "issues.fingerprint is the value sent, unhashed")

	var sources []string
	rows, err := pool.Query(context.Background(),
		`SELECT coalesce(source, '<null>') FROM error_occurrences WHERE project_id = $1`, projectID)
	require.NoError(t, err)
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		sources = append(sources, s)
	}
	rows.Close()
	assert.Equal(t, []string{"onerror", "onerror"}, sources)
}

// eventV2StoredRow is the error_occurrences row V5d reads back, JSONB columns decoded.
type eventV2StoredRow struct {
	Level, Message, ErrorClass, Logger, ServerName, Transaction, Dist, Source string
	TraceID                                                                   string
	TraceFlags                                                                *int64
	ReceivedAt                                                                *time.Time
	Metadata, SDK, User, Tags, Contexts, Request                              map[string]interface{}
	Breadcrumbs                                                               []map[string]interface{}
	Exception                                                                 map[string]interface{}
}

func readEventV2Row(t *testing.T, pool *pgxpool.Pool, occurrenceID string) eventV2StoredRow {
	t.Helper()
	var r eventV2StoredRow
	var md, sdk, user, tags, contexts, request, crumbs, exc []byte
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT level, coalesce(message, ''), error_class, coalesce(logger, ''), coalesce(server_name, ''),
		       coalesce(transaction, ''), coalesce(dist, ''), coalesce(source, ''), coalesce(trace_id, ''),
		       trace_flags, received_at, metadata, sdk, event_user, tags, contexts, request, breadcrumbs, exception
		  FROM error_occurrences WHERE id = $1`, occurrenceID).Scan(
		&r.Level, &r.Message, &r.ErrorClass, &r.Logger, &r.ServerName, &r.Transaction, &r.Dist, &r.Source, &r.TraceID,
		&r.TraceFlags, &r.ReceivedAt, &md, &sdk, &user, &tags, &contexts, &request, &crumbs, &exc))
	for _, pair := range []struct {
		raw []byte
		out interface{}
	}{{md, &r.Metadata}, {sdk, &r.SDK}, {user, &r.User}, {tags, &r.Tags}, {contexts, &r.Contexts},
		{request, &r.Request}, {crumbs, &r.Breadcrumbs}, {exc, &r.Exception}} {
		require.NotNil(t, pair.raw, "every v2 JSONB column is populated for the full fixture")
		require.NoError(t, json.Unmarshal(pair.raw, pair.out))
	}
	return r
}

// TestEventV2_IngestToStoredOccurrence is V5d: event-v2-full.json posted to /ingest and, with a fresh
// event_id, to /ingest/batch; both occurrence rows carry every v2 column, masked where spec §6.2 says
// and verbatim otherwise, with received_at set.
func TestEventV2_IngestToStoredOccurrence(t *testing.T) {
	cfg := eventV2E2EConfig()
	pool := newPostgresPool(t, cfg)
	defer pool.Close()
	projectName := fmt.Sprintf("esv2-v5d-%d", time.Now().UnixNano())
	apiKey := "esv2-v5d-key-" + projectName
	projectID := createTestProject(t, pool, projectName, apiKey)
	defer cleanupProject(t, pool, projectID)

	single := fmt.Sprintf("evt-v5d-single-%d", time.Now().UnixNano())
	batch := fmt.Sprintf("evt-v5d-batch-%d", time.Now().UnixNano())
	resp := postJSON(t, cfg.IngesterURL+"/ingest", apiKey, loadEventV2FullBody(t, projectName, single))
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp = postJSON(t, cfg.IngesterURL+"/ingest/batch", apiKey, []interface{}{loadEventV2FullBody(t, projectName, batch)})
	resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	for _, eventID := range []string{single, batch} {
		t.Run(eventID, func(t *testing.T) {
			row := readEventV2Row(t, pool, waitForOccurrence(t, pool, projectID, eventID))
			assertEventV2StoredRow(t, pool, projectID, row)
		})
	}
}

// assertEventV2StoredRow is V5d's per-row assertion over the event-v2-full.json fixture.
func assertEventV2StoredRow(t *testing.T, pool *pgxpool.Pool, projectID string, row eventV2StoredRow) {
	t.Helper()
	assert.Equal(t, "warning", row.Level)
	assert.Equal(t, "TypeError", row.ErrorClass)
	assert.Equal(t, "checkout", row.Logger)
	assert.Equal(t, "web-7f9c-pod-2", row.ServerName)
	assert.Equal(t, "/checkout/pay", row.Transaction)
	assert.Equal(t, "4127", row.Dist)
	assert.Equal(t, "onerror", row.Source)
	require.NotNil(t, row.TraceFlags)
	assert.Equal(t, int64(1), *row.TraceFlags)
	require.NotNil(t, row.ReceivedAt, "received_at is stamped by the ingestor")
	assert.WithinDuration(t, time.Now(), *row.ReceivedAt, 5*time.Minute)

	assert.Equal(t, map[string]interface{}{"name": "sentinel.javascript.browser", "version": "1.8.0"}, row.SDK)
	assert.Equal(t, "alice@example.com", row.User["email"], "user is stored as sent (decision 1)")
	assert.Equal(t, "2001:db8::1", row.User["ip_address"])
	assert.Equal(t, "Pixel 8", row.Contexts["device"].(map[string]interface{})["model"], "P5: contexts verbatim")

	headers := row.Request["headers"].(map[string]interface{})
	assert.Equal(t, "***REDACTED***", headers["Authorization"], "P3")
	assert.Equal(t, "***REDACTED***", headers["Cookie"], "P10 (TASK-08)")
	assert.Equal(t, "***REDACTED***", headers["X-API-Key"], "P10 (TASK-08)")
	assert.Equal(t, "charge [REDACTED_PAN] failed for order 7c9e6679-7425-40de-944b-e07fc1f90ae7", row.Message,
		"P11: the PAN is redacted and the UUID is stored verbatim, not normalised")
	assert.Equal(t, map[string]interface{}{"tenant_id": "tenant-acme", "region": "eu-central-1", "feature.flag": "new-checkout"}, row.Tags)
	assert.Equal(t, "alice@example.com", row.Metadata["email"], "P6: verbatim, no longer <EMAIL>")
	assert.Equal(t, "7c9e6679-7425-40de-944b-e07fc1f90ae7", row.Metadata["request_id"], "P9")
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", row.TraceID, "P9: error_occurrences.trace_id verbatim")
	assert.Equal(t, "***REDACTED***", row.Request["data"].(map[string]interface{})["password"])
	assert.Equal(t, "***REDACTED***", row.Breadcrumbs[1]["data"].(map[string]interface{})["password"], "P4")
	assert.Equal(t, "POST /api/pay 402", row.Breadcrumbs[1]["message"])

	values := row.Exception["values"].([]interface{})
	require.Len(t, values, 2)
	assert.Equal(t, "TypeError", values[1].(map[string]interface{})["type"])

	// P9: the search index holds the request and trace ids verbatim (it used to hold "<UUID>"-style
	// placeholders); P7: its user id is the typed one.
	var idxUser, idxTenant, idxTrace, idxRequest string
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT coalesce(s.user_id, ''), coalesce(s.tenant_id, ''), coalesce(s.trace_id, ''), coalesce(s.request_id, '')
		  FROM error_search_index s JOIN error_occurrences o ON o.id = s.occurrence_id
		 WHERE o.project_id = $1 LIMIT 1`, projectID).Scan(&idxUser, &idxTenant, &idxTrace, &idxRequest))
	assert.Equal(t, "user-typed-42", idxUser)
	assert.Equal(t, "tenant-acme", idxTenant)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", idxTrace)
	assert.Equal(t, "7c9e6679-7425-40de-944b-e07fc1f90ae7", idxRequest)

	// OQ-3 (P7): the typed user.id feeds the rollup and the search index, not metadata.user_id.
	var affected int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM issue_affected_users u JOIN issues i ON i.id = u.issue_id
		  WHERE i.project_id = $1 AND u.user_id = 'user-typed-42'`, projectID).Scan(&affected))
	assert.Equal(t, 1, affected)
}
