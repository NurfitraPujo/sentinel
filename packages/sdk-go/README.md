# Sentinel Go Client SDK (`sdk-go`)

[![Go Reference](https://pkg.go.dev/badge/github.com/tuju-ai/sentinel/packages/sdk-go.svg)](https://pkg.go.dev/github.com/tuju-ai/sentinel/packages/sdk-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/tuju-ai/sentinel/packages/sdk-go)](https://goreportcard.com/report/github.com/tuju-ai/sentinel/packages/sdk-go)

The official **Go Client SDK** for [Sentinel](https://github.com/tuju-ai/sentinel) — a high-performance, real-time error monitoring platform. `packages/sdk-go` provides non-blocking asynchronous error capture (< 50 µs latency), high-throughput batching, automatic OpenTelemetry trace extraction, PII scrubbing, panic recovery middleware, and logging adapters for `slog`, `zerolog`, and `log.Logger`.

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
- **Context Wiring Helpers**: Set `user_id`/full user info, `tenant_id`, low-cardinality tags, arbitrary
  per-request metadata, a level and breadcrumbs, all read from `context.Context` via `sentinel.WithUser()`,
  `sentinel.WithUserInfo()`, `sentinel.WithTenant()`, `sentinel.WithTag()`, `sentinel.WithContextMap()`,
  `sentinel.WithLevel()` and `sentinel.AddBreadcrumb()`. `sentinel.ApplyContext(ctx, event)` is the single call
  that copies all of it onto an `*Event`, for callers that build events without `Client` (see "Call order without
  `Client`" below).
- **Client-Side PII Scrubbing**: Filters sensitive keys (`password`, `authorization`, `token`, `secret`, `credit_card`, `ssn`, `api_key`, `cookie`, `credential`, `private_key`) automatically, recursively through metadata maps and lists.
- **`BeforeSend` hook**: `Config.BeforeSend func(*Event) *Event` runs after every automatic field is set, to edit or drop an event before it is sent.
- **HTTP request context**: `NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: bool})` fills `request` (method, URL, filtered query string and headers) and `transaction` (the route pattern on Go 1.23+ `ServeMux`, else the request path), and, on opt-in, `user.ip_address`.
- **In-App Frame Detection**: Stack frames are automatically classified as in-app (your code) vs. not (standard library, module cache, `vendor/`), which the server uses to fingerprint and group issues correctly.
- **Logging Adapters**: Plug into existing logging setups with zero code refactoring via `sentinelslog` (`slog.Handler`), `sentinelzerolog` (`zerolog.Hook`), and `sentinellog` (`io.Writer`).
- **Retry with Backoff + Failure Visibility**: Ingest failures are no longer silent. 4xx responses are dropped immediately (not retried); 5xx/network errors are retried with capped exponential backoff before being dropped. Set `Config.OnError func(error)` to be notified whenever a batch is ultimately dropped, and `Config.Debug: true` to log the status/response body.

---

## Installation

```bash
go get github.com/tuju-ai/sentinel/packages/sdk-go@latest
```

---

## Quickstart

### 1. Initialize Sentinel

```go
package main

import (
	"errors"
	"time"

	sentinel "github.com/tuju-ai/sentinel/packages/sdk-go"
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

Pass request `context.Context` to capture OpenTelemetry `trace_id` / `span_id` and attach typed fields. `WithTag`
is for low-cardinality values only (region, plan, feature flag) — every `tags` entry is rolled up per issue into
a tag facet, so a value that differs per request (an order id, a timestamp) belongs in `WithContextMap`
(metadata) instead. `request_id`/`requestId`/`request-id` passed to `WithTag` stay in metadata for this reason:

```go
func handleRequest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ctx = sentinel.WithUserInfo(ctx, sentinel.UserInfo{ID: "usr_99", Email: "user@example.test"})
	ctx = sentinel.WithTenant(ctx, "org_acme")
	ctx = sentinel.WithTag(ctx, "region", "eu")
	ctx = sentinel.WithContextMap(ctx, map[string]interface{}{"order_ref": "ord_12345"})
	sentinel.AddBreadcrumb(ctx, sentinel.Breadcrumb{Message: "order validated", Category: "order"})

	if err := processOrder(ctx); err != nil {
		// Automatically extracts OpenTelemetry trace_id/span_id, the typed user, tags, metadata and breadcrumbs.
		sentinel.CaptureErrorContext(ctx, err)
	}
}
```

### Call order without `Client`

A caller that never uses `Client` (for example a package that builds and pushes its own `*Event`) reproduces
what `CaptureErrorContext` does by calling these in order:

```go
event := sentinel.NewEvent(cfg, err, ctxTags)   // level, sdk, server_name, runtime/os/device contexts
sentinel.ApplyContext(ctx, event)               // metadata, level, user, tags, breadcrumbs, trace, request, transaction
if event = cfg.ApplyBeforeSend(event); event == nil {
	return
}
transport.Push(event)
```

`ApplyContext` only adds what the context carries and never clears a field the caller already set; it must run
before your own assignments so that anything you set afterwards wins.

---

## Logging Framework Integration

### `log/slog` Handler

```go
import (
	"log/slog"
	"os"

	"github.com/tuju-ai/sentinel/packages/sdk-go/sentinelslog"
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
	"github.com/tuju-ai/sentinel/packages/sdk-go/sentinelzerolog"
)

logger := zerolog.New(os.Stdout).Hook(sentinelzerolog.NewHook())
logger.Error().Msg("cache cluster unreachable")
```

---

## HTTP Middleware & Panic Recovery

`HTTPMiddleware` is `NewHTTPMiddleware(MiddlewareOptions{})`. Use `NewHTTPMiddleware` directly to opt into
recording the client IP, or to size the per-request breadcrumb buffer:

```go
import (
	"net/http"

	sentinel "github.com/tuju-ai/sentinel/packages/sdk-go"
)

mux := http.NewServeMux()
mux.HandleFunc("/api/data", dataHandler)

// Automatically recovers panics and attaches request/transaction to every event
// captured on this request's context; the recovered-panic event is sent through
// the default client, so it reaches your project only if sentinel.Init was called.
handler := sentinel.NewHTTPMiddleware(sentinel.MiddlewareOptions{
	CaptureClientIP: true, // fills user.ip_address from r.RemoteAddr; off by default
})(mux)
http.ListenAndServe(":8080", handler)
```

Client-IP headers (`X-Forwarded-For`, `Forwarded`, and every header the ingestor's documented header patterns
match) are never read and never recorded in `request.headers`, whatever `CaptureClientIP` says; only the
connection's own `r.RemoteAddr` is used, and only on opt-in. `transaction` is the `ServeMux` route pattern only
when the program is built with Go 1.23+ **and** its main module's `go.mod` declares `go 1.22` or newer; otherwise
it is the request path.

---

## License

GNU General Public License v3.0 (GPL-3.0). See [LICENSE](LICENSE) for details.
