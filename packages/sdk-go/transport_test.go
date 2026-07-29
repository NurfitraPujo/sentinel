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
		tr.Push(&Event{EventID: "e1", Message: "err"})
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

// TestSendBatchDropsOn4xxWithoutRetry verifies the previously-silent 100%
// rejection scenario (VERIFIED_STATE.md S4) is now observable: a 4xx
// response is not retried, is counted as dropped, and invokes OnError.
func TestSendBatchDropsOn4xxWithoutRetry(t *testing.T) {
	var requestCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"platform is required"}`))
	}))
	defer ts.Close()

	var onErrorCalls int32
	var lastErr error
	tr := NewTransport(Config{
		ProjectKey:    "pk_test",
		Endpoint:      ts.URL,
		MaxBufferSize: 10,
		BatchSize:     10,
		BatchWait:     50 * time.Millisecond,
		OnError: func(err error) {
			atomic.AddInt32(&onErrorCalls, 1)
			lastErr = err
		},
	})

	tr.Push(&Event{EventID: "e1", Message: "err"})
	tr.Flush(2 * time.Second)

	if atomic.LoadInt32(&requestCount) != 1 {
		t.Fatalf("expected exactly 1 request (no retry on 4xx), got %d", atomic.LoadInt32(&requestCount))
	}
	if atomic.LoadInt32(&onErrorCalls) != 1 {
		t.Fatalf("expected OnError called once, got %d", atomic.LoadInt32(&onErrorCalls))
	}
	if atomic.LoadUint64(&tr.droppedCount) != 1 {
		t.Fatalf("expected droppedCount=1, got %d", tr.droppedCount)
	}
	if lastErr == nil {
		t.Fatalf("expected OnError to receive a non-nil error")
	}
}

// TestSendBatchSurfacesPartialFailureOn2xx verifies VERIFIED_STATE.md S15 is
// fixed: apps/ingestor-go's /ingest/batch endpoint returns 202 as long as at
// least one event was ingested, with a body reporting which items failed
// (see main.go's batchResult). Previously the SDK returned on any 2xx
// without reading that body, so a batch of 3 valid + 2 invalid events was
// recorded as a complete success - nothing logged, OnError never called,
// droppedCount never incremented for the 2 that failed.
func TestSendBatchSurfacesPartialFailureOn2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Exact shape of apps/ingestor-go/main.go's handleBatchIngest response
		// for a batch that partially succeeded: HTTP 202 (Ingested > 0), body
		// reports the failures.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"ingested":3,"failed":2,"errors":[{"index":1,"message":"validation failed: platform is required"},{"index":4,"message":"validation failed: message: must be 10000 characters"}]}`))
	}))
	defer ts.Close()

	var onErrorCalls int32
	var lastErr error
	tr := NewTransport(Config{
		ProjectKey:    "pk_test",
		Endpoint:      ts.URL,
		MaxBufferSize: 10,
		BatchSize:     5,
		BatchWait:     50 * time.Millisecond,
		Debug:         true,
		OnError: func(err error) {
			atomic.AddInt32(&onErrorCalls, 1)
			lastErr = err
		},
	})

	for i := 0; i < 5; i++ {
		tr.Push(&Event{EventID: "e1", Message: "err"})
	}
	tr.Flush(2 * time.Second)

	if atomic.LoadInt32(&onErrorCalls) != 1 {
		t.Fatalf("expected OnError called once for the partially-failed batch, got %d", atomic.LoadInt32(&onErrorCalls))
	}
	if lastErr == nil {
		t.Fatalf("expected OnError to receive a non-nil error describing the partial failure")
	}
	if atomic.LoadUint64(&tr.droppedCount) != 2 {
		t.Fatalf("expected droppedCount=2 (only the failed items), got %d", tr.droppedCount)
	}
}

// TestSendBatchIgnoresUnparsableBodyOn2xx verifies handlePartialFailure
// never panics or misreports a drop when a 2xx response body isn't the
// batchIngestResult shape at all - an older ingestor, a proxy that strips
// the body, or (as exercised here) the single-event endpoint's bare
// {"status":"accepted"}, which has no "failed" field.
func TestSendBatchIgnoresUnparsableBodyOn2xx(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		w.Write([]byte(`{"status":"accepted"}`))
	}))
	defer ts.Close()

	var onErrorCalls int32
	tr := NewTransport(Config{
		ProjectKey:    "pk_test",
		Endpoint:      ts.URL,
		MaxBufferSize: 10,
		BatchSize:     10,
		BatchWait:     50 * time.Millisecond,
		OnError:       func(error) { atomic.AddInt32(&onErrorCalls, 1) },
	})

	tr.Push(&Event{EventID: "e1", Message: "err"})
	tr.Flush(2 * time.Second)

	if atomic.LoadInt32(&onErrorCalls) != 0 {
		t.Fatalf("expected OnError not called for a body with no failures, got %d", atomic.LoadInt32(&onErrorCalls))
	}
	if atomic.LoadUint64(&tr.droppedCount) != 0 {
		t.Fatalf("expected droppedCount=0, got %d", tr.droppedCount)
	}
}

// TestSendBatchRetriesOn5xxThenSucceeds verifies transient server errors are
// retried rather than silently dropped on the first attempt.
func TestSendBatchRetriesOn5xxThenSucceeds(t *testing.T) {
	var requestCount int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&requestCount, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	var onErrorCalls int32
	tr := NewTransport(Config{
		ProjectKey:    "pk_test",
		Endpoint:      ts.URL,
		MaxBufferSize: 10,
		BatchSize:     10,
		BatchWait:     50 * time.Millisecond,
		OnError:       func(error) { atomic.AddInt32(&onErrorCalls, 1) },
	})

	tr.Push(&Event{EventID: "e1", Message: "err"})
	tr.Flush(5 * time.Second)

	if atomic.LoadInt32(&requestCount) != 3 {
		t.Fatalf("expected exactly 3 requests (2 failures + 1 success), got %d", atomic.LoadInt32(&requestCount))
	}
	if atomic.LoadInt32(&onErrorCalls) != 0 {
		t.Fatalf("expected OnError not called on eventual success, got %d calls", atomic.LoadInt32(&onErrorCalls))
	}
}

// TestSendBatchDropsAfterExhaustingRetriesOnNetworkError verifies a
// persistent network error (server unreachable) is eventually dropped rather
// than retried forever, and reported via OnError.
func TestSendBatchDropsAfterExhaustingRetriesOnNetworkError(t *testing.T) {
	var onErrorCalls int32
	var lastErr error
	tr := NewTransport(Config{
		ProjectKey: "pk_test",
		// Nothing listens on this address, so every attempt is a network error.
		Endpoint:      "http://127.0.0.1:1",
		MaxBufferSize: 10,
		BatchSize:     10,
		BatchWait:     50 * time.Millisecond,
		OnError: func(err error) {
			atomic.AddInt32(&onErrorCalls, 1)
			lastErr = err
		},
	})

	tr.Push(&Event{EventID: "e1", Message: "err"})
	ok := tr.Flush(10 * time.Second)
	if !ok {
		t.Fatalf("expected Flush to complete within timeout")
	}

	if atomic.LoadInt32(&onErrorCalls) != 1 {
		t.Fatalf("expected OnError called once after retries exhausted, got %d", atomic.LoadInt32(&onErrorCalls))
	}
	if atomic.LoadUint64(&tr.droppedCount) != 1 {
		t.Fatalf("expected droppedCount=1, got %d", tr.droppedCount)
	}
	if lastErr == nil {
		t.Fatalf("expected a non-nil error describing the failure")
	}
}
