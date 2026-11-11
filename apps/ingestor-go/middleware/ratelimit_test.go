package middleware

import (
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRateLimiterMiddleware_NilClientFailsClosedByDefault used to assert a nil Redis client falls open
// (every request accepted) as correct behavior. That was S10 (docs/plans/E2E_RECOVERY_PLAN.md P3-3): a
// nil client is what main.go now produces only after an operator explicitly opts in via
// RATELIMIT_ALLOW_NO_REDIS=true post-outage, and even then the middleware itself must not treat "no
// Redis" as an unconditional bypass. Fail-open is now only reachable via the explicit
// RATELIMIT_STRICT_MODE=false opt-out (see TestRateLimiterMiddleware_NilClientExplicitOptOutFallsOpen).
func TestRateLimiterMiddleware_NilClientFailsClosedByDefault(t *testing.T) {
	rl := NewRateLimiter(nil)

	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	// Add api_key_hash to context
	ctx := auth.WithIdentity(req.Context(), "test-project", "", "test-org", "some-hash", 5000, false)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500 (fail closed, no opt-out set), got %d", rr.Code)
	}
}

// TestRateLimiterMiddleware_NilClientExplicitOptOutFallsOpen proves fail-open is still reachable given
// the explicit RATELIMIT_STRICT_MODE=false opt-out.
func TestRateLimiterMiddleware_NilClientExplicitOptOutFallsOpen(t *testing.T) {
	t.Setenv("RATELIMIT_STRICT_MODE", "false")

	rl := NewRateLimiter(nil)

	handler := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	ctx := auth.WithIdentity(req.Context(), "test-project", "", "test-org", "some-hash", 5000, false)
	req = req.WithContext(ctx)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200 (RATELIMIT_STRICT_MODE=false explicitly set), got %d", rr.Code)
	}
}
