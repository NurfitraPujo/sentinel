# TODO 01: Client Integration SDKs

## Priority: Critical (Blocker for Production Integration)
## Status: Pending

### Overview
Sentinel currently accepts error events via raw `POST /ingest` HTTP requests with manual JSON payloads. To integrate Sentinel cleanly into production applications, language-specific client SDKs are required.

### Requirements
1. **Go SDK (`packages/sentinel-go` or standalone repo)**:
   - Async non-blocking event submission (worker pool/channel buffer).
   - HTTP transport retry with exponential backoff.
   - Panic recovery middleware for `net/http`, `gin`, and `chi`.
   - `sentinel.CaptureError(err, tags, metadata)` API.

2. **JavaScript / TypeScript SDK (`packages/sentinel-js`)**:
   - Universal browser and Node.js error capture.
   - Global `uncaughtException` and `unhandledRejection` hooks.
   - Breadcrumb tracking (console logs, network requests).

3. **Python SDK (`packages/sentinel-python`)**:
   - WSGI / ASGI middleware (FastAPI, Django, Flask).
   - Unhandled exception hook integration.

### Acceptance Criteria
- SDKs submit valid Protobuf/JSON payloads matching `packages/proto/error_event.proto`.
- Event submission does not block client application request threads.
- Documentation and code examples provided for each supported language.
