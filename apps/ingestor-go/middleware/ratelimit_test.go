package middleware

import (
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiterMiddleware(t *testing.T) {
	// Without Redis client (fallback mode)
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

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", rr.Code)
	}
}
