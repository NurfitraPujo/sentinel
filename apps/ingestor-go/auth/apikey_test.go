package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIKeyAuthenticator_MissingKey(t *testing.T) {
	auth := NewAPIKeyAuthenticator(nil, nil, nil)

	handler := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rr.Code)
	}
}
