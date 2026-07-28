package unit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	libredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newMiniRedisClient starts an in-process miniredis server and returns a
// *redis.Client wired to it, so the real ZSET sliding-window logic in
// middleware.RateLimiter is actually exercised rather than mocked.
func newMiniRedisClient(t *testing.T) *libredis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := libredis.NewClient(&libredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// okHandler is a trivial downstream handler that records how many times it
// was invoked and always responds 200.
func okHandler(calls *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			*calls++
		}
		w.WriteHeader(http.StatusOK)
	})
}

func doRequest(t *testing.T, handler http.Handler, apiKeyHash string, rateLimitRPM *int) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	ctx := req.Context()
	if apiKeyHash != "" {
		ctx = context.WithValue(ctx, "api_key_hash", apiKeyHash)
	}
	if rateLimitRPM != nil {
		ctx = context.WithValue(ctx, "rate_limit_rpm", *rateLimitRPM)
	}
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// TODO(P3-3): S10 (fail-open on nil/unreachable Redis) is a DEFECT, not a spec.
// P3-3 will make a nil/unreachable Redis fatal at startup/request time, at
// which point this expectation must INVERT (the request should be rejected,
// not passed through). Do not weaken P3-3's fix to keep this test green —
// update this test's expected behavior instead.
func TestRateLimiter_NilClientFallsOpen(t *testing.T) {
	rl := middleware.NewRateLimiter(nil)
	require.NotNil(t, rl)

	var calls int
	handler := rl.Middleware(okHandler(&calls))

	// With no Redis client, every request must pass through regardless of
	// how many are sent or what key is used (fail-open behavior, S10).
	for i := 0; i < 5; i++ {
		rr := doRequest(t, handler, "some-api-key-hash", nil)
		assert.Equal(t, http.StatusOK, rr.Code)
	}
	assert.Equal(t, 5, calls)
}

func TestRateLimiter_NoAPIKeyHashBypassesLimit(t *testing.T) {
	client := newMiniRedisClient(t)
	rl := middleware.NewRateLimiter(client)

	var calls int
	handler := rl.Middleware(okHandler(&calls))

	// No api_key_hash in context: the middleware has no identity to key the
	// sliding window on, so it must let the request through untouched.
	for i := 0; i < 10; i++ {
		rr := doRequest(t, handler, "", nil)
		assert.Equal(t, http.StatusOK, rr.Code)
	}
	assert.Equal(t, 10, calls)
}

func TestRateLimiter_AllowsUpToLimitThenRejects(t *testing.T) {
	client := newMiniRedisClient(t)
	rl := middleware.NewRateLimiter(client)

	var calls int
	handler := rl.Middleware(okHandler(&calls))

	limit := 3
	// First `limit` requests within the window must succeed.
	for i := 0; i < limit; i++ {
		rr := doRequest(t, handler, "key-a", &limit)
		require.Equal(t, http.StatusOK, rr.Code, "request %d should be allowed", i+1)
		assert.Equal(t, strconv.Itoa(limit), rr.Header().Get("X-RateLimit-Limit"))
	}
	assert.Equal(t, limit, calls)

	// The next request in the same window must be rejected with 429 and a
	// Retry-After header, and must NOT reach the downstream handler.
	rr := doRequest(t, handler, "key-a", &limit)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
	assert.Equal(t, "60", rr.Header().Get("Retry-After"))
	assert.Equal(t, limit, calls, "downstream handler must not be invoked once rejected")
}

func TestRateLimiter_DifferentKeysHaveIndependentBudgets(t *testing.T) {
	client := newMiniRedisClient(t)
	rl := middleware.NewRateLimiter(client)

	var calls int
	handler := rl.Middleware(okHandler(&calls))

	limit := 2
	// Exhaust the budget for key-a.
	for i := 0; i < limit; i++ {
		rr := doRequest(t, handler, "key-a", &limit)
		require.Equal(t, http.StatusOK, rr.Code)
	}
	rejected := doRequest(t, handler, "key-a", &limit)
	require.Equal(t, http.StatusTooManyRequests, rejected.Code)

	// key-b must be unaffected by key-a's exhausted budget.
	rr := doRequest(t, handler, "key-b", &limit)
	assert.Equal(t, http.StatusOK, rr.Code)
}

func TestRateLimiter_RemainingHeaderDecreases(t *testing.T) {
	client := newMiniRedisClient(t)
	rl := middleware.NewRateLimiter(client)

	handler := rl.Middleware(okHandler(nil))

	limit := 5
	rr1 := doRequest(t, handler, "key-remaining", &limit)
	require.Equal(t, http.StatusOK, rr1.Code)
	remaining1, err := strconv.Atoi(rr1.Header().Get("X-RateLimit-Remaining"))
	require.NoError(t, err)

	rr2 := doRequest(t, handler, "key-remaining", &limit)
	require.Equal(t, http.StatusOK, rr2.Code)
	remaining2, err := strconv.Atoi(rr2.Header().Get("X-RateLimit-Remaining"))
	require.NoError(t, err)

	assert.Equal(t, remaining1-1, remaining2, "X-RateLimit-Remaining should decrease by one per request")
}

func TestRateLimiter_DefaultRPMWhenContextMissingOrInvalid(t *testing.T) {
	client := newMiniRedisClient(t)
	rl := middleware.NewRateLimiter(client)

	handler := rl.Middleware(okHandler(nil))

	// No rate_limit_rpm in context at all: the middleware must fall back to
	// its documented default of 5000 rather than rejecting or panicking.
	rr := doRequest(t, handler, "key-default", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, "5000", rr.Header().Get("X-RateLimit-Limit"))
}

// TODO(P3-3): S10 (fail-open on nil/unreachable Redis) is a DEFECT, not a spec.
// P3-3 will make a nil/unreachable Redis fatal, at which point fail-open
// becomes impossible and this expectation must INVERT. Do not weaken P3-3's
// fix to keep this test green — update this test's expected behavior instead.
func TestRateLimiter_RedisUnreachableFailsOpenWhenNotStrict(t *testing.T) {
	// This process is not started with RATELIMIT_STRICT_MODE=true, so the
	// package-level strictMode flag is false for the whole test binary.
	mr := miniredis.NewMiniRedis()
	require.NoError(t, mr.Start())
	client := libredis.NewClient(&libredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	// Kill the server so ZCard returns an error, exercising the non-strict
	// fail-open branch (S10).
	mr.Close()

	rl := middleware.NewRateLimiter(client)
	var calls int
	handler := rl.Middleware(okHandler(&calls))

	rr := doRequest(t, handler, "key-unreachable", nil)
	assert.Equal(t, http.StatusOK, rr.Code, "non-strict mode must fail open when Redis is unreachable")
	assert.Equal(t, 1, calls)
}
