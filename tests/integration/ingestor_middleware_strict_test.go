//go:build strictmode

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRateLimiterAllow_NilClient_Strict covers the nil-client branch when
// the strict-mode package variable is true. This file is intended to be
// run with RATELIMIT_STRICT_MODE=true set in the environment so that the
// strictMode variable is initialized to true before the tests run.
func TestRateLimiterAllow_NilClient_Strict(t *testing.T) {
	require.Equal(t, "true", os.Getenv("RATELIMIT_STRICT_MODE"),
		"this test must run with RATELIMIT_STRICT_MODE=true")

	rl := middleware.NewRateLimiter(nil, 1, time.Minute)
	got := rl.Allow(context.Background(), "api-key")
	assert.False(t, got, "nil client with strict mode must reject")
}

// TestRateLimiterAllow_ClientError_Strict covers the redis-client error
// branch when strictMode is true. A closed client is used to force
// Incr(...).Result() to return an error.
func TestRateLimiterAllow_ClientError_Strict(t *testing.T) {
	require.Equal(t, "true", os.Getenv("RATELIMIT_STRICT_MODE"),
		"this test must run with RATELIMIT_STRICT_MODE=true")

	// Build a client and immediately close it so subsequent operations fail.
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = client.Close()

	rl := middleware.NewRateLimiter(client, 1, time.Minute)
	got := rl.Allow(context.Background(), uniqueKey(t))
	assert.False(t, got, "client error with strict mode must reject")
}

// TestRateLimiterMiddleware_ReturnsTooManyRequests_Strict verifies that
// the middleware still returns 429 when the rate limit is exceeded while
// running in strict mode (the strict-mode branch is hit on the error
// path, but the happy/over-limit paths operate identically).
func TestRateLimiterMiddleware_ReturnsTooManyRequests_Strict(t *testing.T) {
	require.Equal(t, "true", os.Getenv("RATELIMIT_STRICT_MODE"),
		"this test must run with RATELIMIT_STRICT_MODE=true")

	client := integrationRedisClient(t)

	const rate = 1
	rl := middleware.NewRateLimiter(client, rate, time.Minute)
	handler := rl.Middleware(okHandler)

	apiKey := uniqueKey(t)

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	firstReq.Header.Set("X-API-Key", apiKey)
	handler.ServeHTTP(first, firstReq)

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	secondReq.Header.Set("X-API-Key", apiKey)
	handler.ServeHTTP(second, secondReq)

	assert.Equal(t, http.StatusTooManyRequests, second.Code)
	assert.Equal(t, "60", second.Header().Get("Retry-After"))
}
