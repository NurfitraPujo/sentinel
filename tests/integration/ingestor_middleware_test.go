package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	sharedredis "github.com/NurfitraPujo/sentinel/packages/shared-go/redis"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// integrationRedisClient connects to the Redis testcontainer provisioned via
// testcontainers.Setup (or existing test config).
func integrationRedisClient(t *testing.T) *redis.Client {
	t.Helper()

	env := tc.Setup(t, tc.WithResources(tc.RedisResource))
	require.NotEmpty(t, env.RedisConfig.Addr, "redis testcontainer must be initialized")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := sharedredis.NewClient(ctx, sharedredis.Config{
		Addr:     env.RedisConfig.Addr,
		Password: env.RedisConfig.Password,
		DB:       env.RedisConfig.DB,
	})
	require.NoError(t, err, "connect to redis testcontainer")
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// uniqueKey returns a redis key that is unique to the calling test so
// concurrent tests and reruns cannot collide on the same bucket.
func uniqueKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("test-%s-%d", t.Name(), time.Now().UnixNano())
}

// TestRateLimiterAllow_UnderThreshold verifies that the first Allow call
// for a fresh key returns true and triggers the Expire path (count == 1).
func TestRateLimiterAllow_UnderThreshold(t *testing.T) {
	client := integrationRedisClient(t)

	rl := middleware.NewRateLimiter(client, 5, time.Minute)
	require.NotNil(t, rl)

	key := uniqueKey(t)
	got := rl.Allow(context.Background(), key)
	assert.True(t, got, "first call under threshold should be allowed")
}

// TestRateLimiterAllow_OverThreshold verifies that Allow returns false
// once the per-window count exceeds the configured rate.
func TestRateLimiterAllow_OverThreshold(t *testing.T) {
	client := integrationRedisClient(t)

	const rate = 2
	rl := middleware.NewRateLimiter(client, rate, time.Minute)
	key := uniqueKey(t)

	// First `rate` calls must succeed.
	for i := 0; i < rate; i++ {
		require.True(t, rl.Allow(context.Background(), key),
			"call %d under threshold should be allowed", i+1)
	}

	// The next call must be rejected.
	got := rl.Allow(context.Background(), key)
	assert.False(t, got, "call beyond rate should be rejected")
}

// TestRateLimiterAllow_ClientErrorFailOpen verifies that an error returned
// by the redis client is treated as fail-open when strictMode is false.
// The unit test at tests/unit/ingestor_middleware_test.go already covers
// the nil-client branch; here we trigger the error path with a closed
// client to exercise the Incr error branch that wraps the strictMode check.
func TestRateLimiterAllow_ClientErrorFailOpen(t *testing.T) {
	// Build a client, then close it so subsequent operations return errors.
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer func() { _ = client.Close() }()
	_ = client.Close()

	rl := middleware.NewRateLimiter(client, 1, time.Minute)
	got := rl.Allow(context.Background(), uniqueKey(t))
	assert.True(t, got, "fail-open: client error without strict mode must allow the request")
}

// okHandler is a tiny http.Handler that always returns 200 OK. It is used
// to detect whether the middleware passed the request through.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

// TestRateLimiterMiddleware_AllowsUnderThreshold verifies that the
// middleware returns 200 when the caller is under the rate limit.
func TestRateLimiterMiddleware_AllowsUnderThreshold(t *testing.T) {
	client := integrationRedisClient(t)
	rl := middleware.NewRateLimiter(client, 5, time.Minute)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	req.Header.Set("X-API-Key", uniqueKey(t))

	rl.Middleware(okHandler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

// TestRateLimiterMiddleware_ReturnsTooManyRequests verifies that the
// middleware returns 429 with the Retry-After: 60 header once the rate
// limit is exceeded for the same API key.
func TestRateLimiterMiddleware_ReturnsTooManyRequests(t *testing.T) {
	client := integrationRedisClient(t)

	const rate = 1
	rl := middleware.NewRateLimiter(client, rate, time.Minute)
	handler := rl.Middleware(okHandler)

	apiKey := uniqueKey(t)

	// First request must succeed.
	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	firstReq.Header.Set("X-API-Key", apiKey)
	handler.ServeHTTP(first, firstReq)
	require.Equal(t, http.StatusOK, first.Code)

	// Second request must be rate-limited.
	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	secondReq.Header.Set("X-API-Key", apiKey)
	handler.ServeHTTP(second, secondReq)

	assert.Equal(t, http.StatusTooManyRequests, second.Code)
	assert.Equal(t, "60", second.Header().Get("Retry-After"))
}

// TestRateLimiterMiddleware_AllowsUnauthenticatedTraffic verifies that the
// middleware does not rate-limit requests that omit the X-API-Key header,
// even when the call would otherwise be over the limit.
func TestRateLimiterMiddleware_AllowsUnauthenticatedTraffic(t *testing.T) {
	client := integrationRedisClient(t)
	rl := middleware.NewRateLimiter(client, 1, time.Minute)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	// Intentionally no X-API-Key header.

	rl.Middleware(okHandler).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "unauthenticated traffic must bypass rate limiting")
	assert.Equal(t, "ok", rec.Body.String())
}
