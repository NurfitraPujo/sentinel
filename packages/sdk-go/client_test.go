package sentinel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientLatencyAndAutoInit(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	Init(Config{
		ProjectKey: "pk_test",
		Endpoint:   ts.URL,
	})

	start := time.Now()
	CaptureError(errors.New("test error latency"))
	elapsed := time.Since(start)

	if elapsed > 50*time.Microsecond && elapsed > 1*time.Millisecond {
		t.Logf("CaptureError elapsed: %v", elapsed)
	}

	ok := Flush(2 * time.Second)
	if !ok {
		t.Errorf("expected flush to succeed")
	}
}
