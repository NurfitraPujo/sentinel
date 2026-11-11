package unit

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	"github.com/alicebob/miniredis/v2"
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
		// Built via the production helper, never by string literal: a hand-injected context can
		// silently diverge from what the middleware actually reads (that is how the typed-key
		// switch bypassed rate limiting entirely without a single test failing).
		rpm := 0
		if rateLimitRPM != nil {
			rpm = *rateLimitRPM
		}
		ctx = auth.WithIdentity(ctx, "test-project", "", "test-org", apiKeyHash, rpm, false)
	}
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

// P3-3 (S10): a nil Redis client is what main.go now produces only when an operator has explicitly
// opted into RATELIMIT_ALLOW_NO_REDIS=true after a boot-time outage (main.go otherwise refuses to
// start). The middleware itself must not treat that as an unconditional bypass — fail-open must be a
// decision, read every time via RATELIMIT_STRICT_MODE, never an accident of a short-circuit before that
// decision is consulted. Default (no override) is now fail-CLOSED. This inverts the previous version of
// this test, which asserted the defect (unconditional fail-open) as correct behavior — see
// TestRateLimiter_NilClientExplicitOptOutFallsOpen below for the opt-out path.
func TestRateLimiter_NilClientFailsClosedByDefault(t *testing.T) {
	rl := middleware.NewRateLimiter(nil)
	require.NotNil(t, rl)

	var calls int
	handler := rl.Middleware(okHandler(&calls))

	// With no Redis client and no explicit RATELIMIT_STRICT_MODE=false opt-out, every request must be
	// refused (fail-closed), not silently accepted.
	for i := 0; i < 5; i++ {
		rr := doRequest(t, handler, "some-api-key-hash", nil)
		assert.Equal(t, http.StatusInternalServerError, rr.Code, "request %d should fail closed", i+1)
	}
	assert.Equal(t, 0, calls, "downstream handler must never be invoked when Redis is unavailable and not explicitly opted out of strict mode")
}

// TestRateLimiter_NilClientExplicitOptOutFallsOpen proves the other half of P3-3's contract: fail-open
// on a nil client is still reachable, but only when an operator explicitly sets
// RATELIMIT_STRICT_MODE=false. That is a decision, not a default.
func TestRateLimiter_NilClientExplicitOptOutFallsOpen(t *testing.T) {
	t.Setenv("RATELIMIT_STRICT_MODE", "false")

	rl := middleware.NewRateLimiter(nil)
	require.NotNil(t, rl)

	var calls int
	handler := rl.Middleware(okHandler(&calls))

	for i := 0; i < 5; i++ {
		rr := doRequest(t, handler, "some-api-key-hash", nil)
		assert.Equal(t, http.StatusOK, rr.Code, "request %d should pass through once RATELIMIT_STRICT_MODE=false is set explicitly", i+1)
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

// TestRateLimiter_RedisUnreachableFailsClosedByDefault inverts the previous version of this test, which
// asserted fail-open-when-Redis-is-unreachable as correct default behavior — that was S10, a defect, not
// a spec (docs/plans/E2E_RECOVERY_PLAN.md P3-3). RATELIMIT_STRICT_MODE now defaults to strict (fail
// closed); an operator must set it to the literal string "false" to get fail-open, which is exercised by
// TestRateLimiter_RedisUnreachableExplicitOptOutFallsOpen below.
func TestRateLimiter_RedisUnreachableFailsClosedByDefault(t *testing.T) {
	mr := miniredis.NewMiniRedis()
	require.NoError(t, mr.Start())
	client := libredis.NewClient(&libredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	// Kill the server so the Lua EVAL errors out, exercising the Redis-error branch (S10).
	mr.Close()

	rl := middleware.NewRateLimiter(client)
	var calls int
	handler := rl.Middleware(okHandler(&calls))

	rr := doRequest(t, handler, "key-unreachable", nil)
	assert.Equal(t, http.StatusInternalServerError, rr.Code, "default (strict) mode must fail closed when Redis is unreachable")
	assert.Equal(t, 0, calls)
}

// TestRateLimiter_RedisUnreachableExplicitOptOutFallsOpen proves fail-open is still reachable when an
// unreachable Redis is hit mid-request, but only given the explicit RATELIMIT_STRICT_MODE=false
// opt-out — the same decision point the nil-client path now shares (P3-3).
func TestRateLimiter_RedisUnreachableExplicitOptOutFallsOpen(t *testing.T) {
	t.Setenv("RATELIMIT_STRICT_MODE", "false")

	mr := miniredis.NewMiniRedis()
	require.NoError(t, mr.Start())
	client := libredis.NewClient(&libredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	mr.Close()

	rl := middleware.NewRateLimiter(client)
	var calls int
	handler := rl.Middleware(okHandler(&calls))

	rr := doRequest(t, handler, "key-unreachable", nil)
	assert.Equal(t, http.StatusOK, rr.Code, "explicit RATELIMIT_STRICT_MODE=false must fail open when Redis is unreachable")
	assert.Equal(t, 1, calls)
}

// TestRateLimiter_ConcurrentRequestsDoNotExceedLimit is a local, fast proxy for U19
// (tests/e2e/ratelimit_test.go's TestU19_ConcurrentRequestsExceedLimit) using miniredis instead of the
// real stack: it fires genuinely concurrent requests (every goroutine built and parked on a shared gate
// before any of them runs) against a single key and asserts admitted requests never exceed the
// configured limit. This cannot substitute for the e2e proof against a real Redis and a real HTTP
// server — miniredis is single-process and network latency is not exercised — but it does prove the Lua
// script's atomicity holds even when many goroutines call Eval on the same key at once, which is the
// exact defect this change targets (S10: the old four-round-trip form let concurrent requests all read
// the same ZCARD count before any of their ZADDs landed).
func TestRateLimiter_ConcurrentRequestsDoNotExceedLimit(t *testing.T) {
	client := newMiniRedisClient(t)
	rl := middleware.NewRateLimiter(client)

	var calls int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := rl.Middleware(inner)

	const limit = 20
	const fire = 60

	var ready sync.WaitGroup
	var wg sync.WaitGroup
	start := make(chan struct{})
	ready.Add(fire)
	wg.Add(fire)

	codes := make([]int, fire)
	var mu sync.Mutex

	for i := 0; i < fire; i++ {
		go func(i int) {
			defer wg.Done()
			l := limit
			ready.Done()
			<-start
			rr := doRequest(t, handler, "concurrent-key", &l)
			mu.Lock()
			codes[i] = rr.Code
			if rr.Code == http.StatusOK {
				calls++
			}
			mu.Unlock()
		}(i)
	}

	ready.Wait()
	close(start)
	wg.Wait()

	var accepted, limited, other int
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			accepted++
		case http.StatusTooManyRequests:
			limited++
		default:
			other++
		}
	}

	t.Logf("concurrent unit probe: limit=%d fired=%d accepted=%d limited=%d other=%d", limit, fire, accepted, limited, other)
	assert.Equal(t, 0, other, "every response should be either 200 or 429")
	assert.LessOrEqual(t, accepted, limit, "atomic Lua script must not admit more than the configured limit under concurrency")
	assert.Equal(t, accepted+limited, fire)
}
