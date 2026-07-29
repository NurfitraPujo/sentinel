# Sentinel Go Client SDK (`sdk-go`)

[![Go Reference](https://pkg.go.dev/badge/github.com/NurfitraPujo/sentinel/packages/sdk-go.svg)](https://pkg.go.dev/github.com/NurfitraPujo/sentinel/packages/sdk-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/NurfitraPujo/sentinel/packages/sdk-go)](https://goreportcard.com/report/github.com/NurfitraPujo/sentinel/packages/sdk-go)

The official **Go Client SDK** for [Sentinel](https://github.com/NurfitraPujo/sentinel) — a high-performance, real-time error monitoring platform. `packages/sdk-go` provides non-blocking asynchronous error capture (< 50 µs latency), high-throughput batching, automatic OpenTelemetry trace extraction, PII scrubbing, panic recovery middleware, and logging adapters for `slog`, `zerolog`, and `log.Logger`.

> [!WARNING]
> **v0.2.0 is a breaking wire-format change.** Every prior version had its payload rejected by the ingestor
> on **every single event**, silently (no version before this one ever successfully delivered an event — see
> [`CHANGELOG.md`](CHANGELOG.md) and `docs/memory/VERIFIED_STATE.md` S4/S11). Upgrade to `v0.2.0` or later;
> there is no working payload format on any earlier release.

---

## Features

- **Non-Blocking Lock-Free Capture**: Enqueues errors via a buffered Go channel (`chan *Event`) with ring-buffer FIFO eviction under heavy load.
- **High-Throughput Batch Ingestion**: Sends event batches (`POST /ingest/batch`) with HTTP connection pooling (`http.Transport`).
- **Telemetry & Tracing Correlation**: Automatically extracts W3C `trace_id` and `span_id` from OpenTelemetry active spans (`context.Context`).
- **Context Wiring Helpers**: Set `user_id`, `tenant_id`, and custom service tags effortlessly with `sentinel.WithUser()`, `sentinel.WithTenant()`, `sentinel.WithTag()`.
- **Client-Side PII Scrubbing**: Filters sensitive keys (`password`, `authorization`, `token`, `secret`, `credit_card`, `ssn`, `api_key`) automatically.
- **In-App Frame Detection**: Stack frames are automatically classified as in-app (your code) vs. not (standard library, module cache, `vendor/`), which the server uses to fingerprint and group issues correctly.
- **Logging Adapters**: Plug into existing logging setups with zero code refactoring via `sentinelslog` (`slog.Handler`), `sentinelzerolog` (`zerolog.Hook`), and `sentinellog` (`io.Writer`).
- **Retry with Backoff + Failure Visibility**: Ingest failures are no longer silent. 4xx responses are dropped immediately (not retried); 5xx/network errors are retried with capped exponential backoff before being dropped. Set `Config.OnError func(error)` to be notified whenever a batch is ultimately dropped, and `Config.Debug: true` to log the status/response body.

---

## Installation

```bash
go get github.com/NurfitraPujo/sentinel/packages/sdk-go@latest
```

---

## Quickstart

### 1. Initialize Sentinel

```go
package main

import (
	"errors"
	"time"

	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

func main() {
	sentinel.Init(sentinel.Config{
		ProjectKey: "proj_your_project_key",
		Endpoint:   "http://localhost:8080/ingest",
		Environment: "production",
		ReleaseVersion: "v1.0.0",
		BatchSize:  10,
		BatchWait:  1 * time.Second,
	})
	defer sentinel.Flush(5 * time.Second)

	// Explicit error capture
	if err := doWork(); err != nil {
		sentinel.CaptureError(err, map[string]interface{}{
			"component": "worker",
		})
	}
}
```

---

## API Keys & Authentication

Sentinel supports two types of API keys for authentication (`X-API-Key`):

1. **Project API Keys** (`sent_live_...`): Bound to a single project. The SDK sends `ProjectKey` in the `X-API-Key` header.
2. **Organization-Wide API Keys** (`sent_org_...`): Bound to an organization. When ingesting with an Org-Wide key, specify the target project via the `X-Project-Key` HTTP header (or set `ProjectKey` to your target project's key).

> [!NOTE]
> **API Key Revocation**: When an API key is revoked via the Sentinel Dashboard, active cluster nodes purge key caches in **< 100ms** via NATS JetStream. In standalone single-instance deployments without Redis/NATS, cache eviction completes within a **60-second TTL** window.

---

## Context Wiring & OpenTelemetry Tracing

Pass request `context.Context` to capture OpenTelemetry `trace_id` / `span_id` and attach service tags:

```go
func handleRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctx = sentinel.WithUser(ctx, "usr_99")
	ctx = sentinel.WithTenant(ctx, "org_acme")
	ctx = sentinel.WithTag(ctx, "request_id", "req_12345")

	if err := processOrder(ctx); err != nil {
		// Automatically extracts OpenTelemetry trace_id, span_id, user_id, tenant_id, and tags
		sentinel.CaptureErrorContext(ctx, err)
	}
}
```

---

## Logging Framework Integration

### `log/slog` Handler

```go
import (
	"log/slog"
	"os"

	"github.com/NurfitraPujo/sentinel/packages/sdk-go/sentinelslog"
)

slogHandler := sentinelslog.NewHandler(slog.NewJSONHandler(os.Stdout, nil))
logger := slog.New(slogHandler)

// Logged errors are automatically pushed to Sentinel
logger.ErrorContext(ctx, "database connection failed", "db_host", "db.internal", "err", err)
```

### `zerolog` Hook

```go
import (
	"os"

	"github.com/rs/zerolog"
	"github.com/NurfitraPujo/sentinel/packages/sdk-go/sentinelzerolog"
)

logger := zerolog.New(os.Stdout).Hook(sentinelzerolog.NewHook())
logger.Error().Msg("cache cluster unreachable")
```

---

## HTTP Middleware & Panic Recovery

```go
import (
	"net/http"

	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

mux := http.NewServeMux()
mux.HandleFunc("/api/data", dataHandler)

// Automatically recovers panics and attaches request metadata
handler := sentinel.HTTPMiddleware(mux)
http.ListenAndServe(":8080", handler)
```

---

## License

GNU General Public License v3.0 (GPL-3.0). See [LICENSE](LICENSE) for details.
