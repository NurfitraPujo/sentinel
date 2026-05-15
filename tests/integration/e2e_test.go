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
	if err != nil {
		t.Skipf("Skipping: cannot connect to postgres: %v", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("Skipping: cannot ping postgres: %v", err)
	}

	return pool
}

func createTestProject(t *testing.T, pool *pgxpool.Pool, projectName, apiKey string) string {
	var projectID string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO projects (name, api_key, api_key_hash)
		 VALUES ($1, $2, encode(digest($3::bytea, 'sha256'), 'hex'))
		 RETURNING id::text`,
		projectName, apiKey, apiKey,
	).Scan(&projectID)
	if err != nil {
		t.Fatalf("Failed to create test project: %v", err)
	}
	return projectID
}

func cleanupProject(t *testing.T, pool *pgxpool.Pool, projectID string) {
	_, err := pool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, projectID)
	if err != nil {
		t.Logf("Failed to cleanup project %s: %v", projectID, err)
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
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
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
