package sentinel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPMiddlewarePanicRecovery(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	Init(Config{
		ProjectKey: "pk_test",
		Endpoint:   ts.URL,
	})

	panicHandler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something went terribly wrong")
	}))

	req := httptest.NewRequest("GET", "/panic", nil)
	w := httptest.NewRecorder()

	panicHandler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", w.Code)
	}

	Flush(1 * time.Second)
}
