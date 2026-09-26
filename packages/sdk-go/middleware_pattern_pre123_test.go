//go:build !go1.23

package sentinel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMiddlewareTransactionIsPathPre123(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})

	reached := false
	mux := http.NewServeMux()
	mux.HandleFunc("/items/", func(w http.ResponseWriter, r *http.Request) {
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
		t.Fatal("handler was not reached")
	}
	var ev map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &ev)
	if got := ev["transaction"]; got != "/items/42" {
		t.Errorf("transaction = %v, want /items/42 (no pattern before go1.23)", got)
	}
}
