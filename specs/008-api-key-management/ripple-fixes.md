# Ripple Fixes — Session 2026-07-26

## R-001: Rate Limiter Middleware Outer Nesting Enables Unauthenticated Cache Amplification [CRITICAL]

**Strategy**: Option A — Nest `rateLimiter.Middleware` inside `authenticator.Middleware`

**Files to modify**:
- `apps/ingestor-go/main.go` — Nest rateLimiter handler inside authenticator handler so request context contains `api_key_hash`.

**Key steps**:
1. Wrap `rateLimiter.Middleware` inside `authenticator.Middleware` for both `/ingest` and `/ingest/batch` endpoints.
2. Route HTTP handlers to `ingestHandler` and `batchIngestHandler`.

**Verification**: Run `go test ./apps/ingestor-go/...` and integration test suite `go test -run TestAPIKeyLifecycleAndRateLimitingE2E ./tests/integration/...`.
