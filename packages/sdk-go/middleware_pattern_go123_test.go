//go:build go1.23

// This module declares go 1.21 (T16), so without the directive below its
// test binaries run with httpmuxgo121=1 and ServeMux would treat
// "GET /items/{id}" as a literal path, never reaching the handler (T30).
//go:debug httpmuxgo121=0

package sentinel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMiddlewareTransactionFromPattern(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})

	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		reached = true
		pushCaptured(t, transport, r)
		w.WriteHeader(http.StatusAccepted)
	})

	handler := NewHTTPMiddleware(MiddlewareOptions{})(mux)

	req := httptest.NewRequest("GET", "/items/42", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	transport.Flush(1 * time.Second)

	if !reached {
		t.Fatal("handler was not reached (httpmuxgo121 not disabled?)")
	}
	var ev map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &ev)
	if got := ev["transaction"]; got != "GET /items/{id}" {
		t.Errorf("transaction = %v, want GET /items/{id}", got)
	}
}

func TestMiddlewareTransactionFromPatternOnPanic(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	Init(Config{ProjectKey: "pk_test", Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})

	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
		reached = true
		panic("boom")
	})

	handler := HTTPMiddleware(mux)

	req := httptest.NewRequest("GET", "/items/42", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	Flush(1 * time.Second)

	if !reached {
		t.Fatal("handler was not reached")
	}
	var ev map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &ev)
	if got := ev["transaction"]; got != "GET /items/{id}" {
		t.Errorf("panic transaction = %v, want GET /items/{id}", got)
	}
}

func TestMiddlewareTransactionBareHandlerIsPath(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})

	handler := NewHTTPMiddleware(MiddlewareOptions{})(captureHandler(transport, func(r *http.Request) { pushCaptured(t, transport, r) }))

	req := httptest.NewRequest("GET", "/items/42", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	transport.Flush(1 * time.Second)

	var ev map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &ev)
	if got := ev["transaction"]; got != "/items/42" {
		t.Errorf("transaction = %v, want /items/42 (bare handler: path)", got)
	}
}
