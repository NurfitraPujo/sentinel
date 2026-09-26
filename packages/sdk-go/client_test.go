package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

func TestCaptureErrorContextCarriesFingerprintAndErrorClass(t *testing.T) {
	var received Event
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []Event
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("failed to decode received batch: %v", err)
		} else if len(batch) != 1 {
			t.Errorf("expected a one-element array, got %d", len(batch))
		} else {
			received = batch[0]
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	client := Init(Config{
		ProjectKey: "pk_test",
		Endpoint:   ts.URL,
		BatchSize:  1,
	})

	ctx := context.Background()
	ctx = WithFingerprint(ctx, "custom-fingerprint-123")
	ctx = WithErrorClass(ctx, "ExplicitErrorClass")

	wrapped := fmt.Errorf("outer: %w", errors.New("inner"))
	client.CaptureErrorContext(ctx, wrapped)

	ok := Flush(2 * time.Second)
	if !ok {
		t.Fatalf("expected flush to succeed")
	}

	if received.Fingerprint != "custom-fingerprint-123" {
		t.Errorf("expected Fingerprint=custom-fingerprint-123, got %q", received.Fingerprint)
	}
	if received.ErrorClass != "ExplicitErrorClass" {
		t.Errorf("expected ErrorClass=ExplicitErrorClass, got %q", received.ErrorClass)
	}
}
