package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAuthTestPool provisions a Postgres container (or uses existing test config)
// via testcontainers.Setup and returns a pgxpool.
func newAuthTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	env := tc.Setup(t, tc.WithResources(tc.PostgresResource), tc.WithMigrations(true))
	require.NotNil(t, env.PGPool, "PGPool must be initialized")
	return env.PGPool
}

// hashAPIKey mirrors the SHA-256 hashing performed by
// APIKeyAuthenticator.validateAPIKey so test seed data lines up with the
// lookup the production code performs at request time.
func hashAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:])
}

// seedAuthProject inserts a projects row whose api_key_hash matches the SHA-256
// of the supplied API key. The row is removed in t.Cleanup to keep tests
// isolated. The project name is returned so callers can assert on the context
// value the authenticator should set.
func seedAuthProject(t *testing.T, pool *pgxpool.Pool, name, apiKey string) string {
	t.Helper()
	hash := hashAPIKey(apiKey)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO projects (name, api_key, api_key_hash)
		 VALUES ($1, $2, $3)
		 RETURNING id::text`,
		name, apiKey, hash,
	).Scan(&id)
	require.NoError(t, err)

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM projects WHERE id = $1`, id)
	})

	return id
}

// TestAPIKeyAuthenticator_MissingKey covers the first branch of
// APIKeyAuthenticator.Middleware: when no X-API-Key header is supplied the
// request is rejected with 401 before validateAPIKey is ever invoked and the
// downstream handler is not called.
func TestAPIKeyAuthenticator_MissingKey(t *testing.T) {
	pool := newAuthTestPool(t)
	authn := auth.NewAPIKeyAuthenticator(pool, nil, nil)

	called := false
	handler := authn.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.False(t, called, "downstream handler must not run when API key is missing")
}

// TestAPIKeyAuthenticator_InvalidKey covers the validateAPIKey error branch
// indirectly: a request with an X-API-Key that does not match any seeded
// api_key_hash causes validateAPIKey to return an error (pgx.ErrNoRows), and
// the middleware translates that into a 401 without invoking the downstream
// handler.
func TestAPIKeyAuthenticator_InvalidKey(t *testing.T) {
	pool := newAuthTestPool(t)

	// Seed a project so the "no row matches" path is taken (rather than the
	// table simply being empty for unrelated reasons).
	seedName := fmt.Sprintf("test-project-invalid-%d", time.Now().UnixNano())
	seedAuthProject(t, pool, seedName, "the-correct-api-key")

	authn := auth.NewAPIKeyAuthenticator(pool, nil, nil)

	called := false
	handler := authn.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "definitely-not-the-seeded-key")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusUnauthorized, rr.Code)
	assert.False(t, called, "downstream handler must not run for an invalid API key")
}

// TestAPIKeyAuthenticator_ValidKey covers the happy path: when the supplied
// X-API-Key hashes to a row in projects, the middleware sets the project_key
// context value to the seeded name and forwards the request to the downstream
// handler which then writes 200.
func TestAPIKeyAuthenticator_ValidKey(t *testing.T) {
	pool := newAuthTestPool(t)

	projectName := fmt.Sprintf("test-project-valid-%d", time.Now().UnixNano())
	apiKey := fmt.Sprintf("valid-api-key-%d", time.Now().UnixNano())
	seedAuthProject(t, pool, projectName, apiKey)

	authn := auth.NewAPIKeyAuthenticator(pool, nil, nil)

	var (
		observedKey any
		wasCalled   bool
	)
	handler := authn.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observedKey = r.Context().Value("project_key")
		wasCalled = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", apiKey)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	assert.True(t, wasCalled, "downstream handler must run for a valid API key")
	assert.Equal(t, http.StatusOK, rr.Code)
	require.NotNil(t, observedKey, "project_key must be set on the context")
	assert.Equal(t, projectName, observedKey, "project_key must equal the seeded project name")
}
