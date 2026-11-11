package sentinel_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
	"github.com/NurfitraPujo/sentinel/packages/sdk-go/sentinelslog"
)

func TestGoSDKEndToEndPipeline(t *testing.T) {
	var ingestedCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "proj_integration_key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&ingestedCount, 1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	// 1. Initialize Go SDK
	sentinel.Init(sentinel.Config{
		APIKey:     "proj_integration_key", // secret -> X-API-Key header
		ProjectKey: "integration-project",  // project unique name -> body project_key
		Endpoint:   server.URL,
		BatchSize:  5,
		BatchWait:  100 * time.Millisecond,
	})

	// 2. Test Direct Context Capture
	ctx := context.Background()
	ctx = sentinel.WithUser(ctx, "usr_integration_1")
	ctx = sentinel.WithTenant(ctx, "tenant_acme")
	ctx = sentinel.WithTag(ctx, "service", "payment_processor")

	sentinel.CaptureErrorContext(ctx, errors.New("insufficient funds"))

	// 3. Test Slog Logger Integration
	slogHandler := sentinelslog.NewHandler(slog.NewTextHandler(os.Stdout, nil))
	logger := slog.New(slogHandler)
	logger.ErrorContext(ctx, "gateway timeout", "gateway_id", "gw_88")

	// 4. Flush SDK events
	flushed := sentinel.Flush(2 * time.Second)
	if !flushed {
		t.Fatalf("expected SDK flush to succeed")
	}

	if atomic.LoadInt32(&ingestedCount) == 0 {
		t.Fatalf("expected ingested events to reach server, got 0")
	}
}
