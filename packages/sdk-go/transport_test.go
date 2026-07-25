package sentinel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTransportBatchFlushing(t *testing.T) {
	var requestCount int32
	var totalIngested int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		if r.URL.Path == "/batch" {
			var events []Event
			json.NewDecoder(r.Body).Decode(&events)
			atomic.AddInt32(&totalIngested, int32(len(events)))
		} else {
			atomic.AddInt32(&totalIngested, 1)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	tr := NewTransport(Config{
		ProjectKey:    "pk_test",
		Endpoint:      ts.URL,
		MaxBufferSize: 50,
		BatchSize:     5,
		BatchWait:     100 * time.Millisecond,
	})

	for i := 0; i < 10; i++ {
		tr.Push(&Event{EventID: "e1", ErrorMessage: "err"})
	}

	time.Sleep(300 * time.Millisecond)
	tr.Flush(1 * time.Second)

	if atomic.LoadInt32(&totalIngested) != 10 {
		t.Fatalf("expected 10 total ingested, got %d", atomic.LoadInt32(&totalIngested))
	}
	if atomic.LoadInt32(&requestCount) > 3 {
		t.Fatalf("expected <= 3 batched requests, got %d", atomic.LoadInt32(&requestCount))
	}
}
