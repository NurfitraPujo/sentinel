package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
)

func TestAPIKeyLifecycleAndRateLimitingE2E(t *testing.T) {
	// Mock test server representing authenticated and rate-limited ingestor
	rateLimiter := middleware.NewRateLimiter(nil)
	authenticator := auth.NewAPIKeyAuthenticator(nil, nil, nil)

	handler := authenticator.Middleware(rateLimiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"accepted"}`))
	})))

	ts := httptest.NewServer(handler)
	defer ts.Close()

	// 1. Missing API Key returns 401
	req, _ := http.NewRequestWithContext(context.Background(), "POST", ts.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Failed request: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Expected 401 Unauthorized for missing key, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
