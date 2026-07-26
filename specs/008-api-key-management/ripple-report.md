# Ripple Report: 008-api-key-management

**Branch**: `008-api-key-management` | **Scanned**: 2026-07-26T11:31:15+07:00
**Baseline**: `ccf6984` (branch point from main)
**Change Set**: 14 files changed | **Blast Radius**: 6 dependents checked
**Findings**: 1 critical, 2 warning, 1 info

## Summary

Implementation of multi-tenant auth and API key management introduced high performance and security protections. However, ripple analysis revealed one **CRITICAL** middleware ordering issue in `apps/ingestor-go/main.go` where rate limiting occurs prior to authentication, exposing the rate-limiting ZSET cache to unauthenticated Denial-of-Service (DoS) memory amplification.

## Findings

### CRITICAL

#### R-001: Rate Limiter Middleware Outer Nesting Enables Unauthenticated Cache Amplification

- **Category**: Security & Concurrency / Resource
- **Cause**: In `apps/ingestor-go/main.go` (lines 108-109), `rateLimiter.Middleware` wraps the entire `authenticator.Middleware` handler pipeline:
  ```go
  http.Handle("/ingest", rateLimiter.Middleware(ingestHandler))
  ```
- **Affected**: `apps/ingestor-go/middleware/ratelimit.go` (line 47) and `apps/ingestor-go/main.go` (line 108)
- **Blast Radius**: Redis memory footprint and Ingestor throughput under high load.
- **Before**: Previously, rate limiting ran on project keys directly extracted after authentication or bypassed unauthenticated requests.
- **After**: Now, `middleware/ratelimit.go` checks `r.Context().Value("api_key_hash")`. Because `rateLimiter` runs *before* `authenticator`, `api_key_hash` is ALWAYS empty on the initial outer entry. This causes rate limiting to completely bypass unauthenticated requests, OR if wrapped inside, `authenticator` should run first so `rateLimiter` operates on authenticated key hashes only.
- **Why Tests Miss It**: Unit tests test `rateLimiter.Middleware` with a pre-populated context, missing the HTTP router wrapping order in `main.go`.
- **Recommendation**: Swap handler order in `apps/ingestor-go/main.go` so `authenticator.Middleware` wraps `rateLimiter.Middleware`:
  ```go
  http.Handle("/ingest", authenticator.Middleware(rateLimiter.Middleware(handleIngest(ingestService))))
  ```
- **Status**: RESOLVED
- **Resolution**: Re-ordered HTTP middleware handlers in `apps/ingestor-go/main.go` so `authenticator.Middleware` wraps `rateLimiter.Middleware`. Requests now extract and validate `api_key_hash` into request context prior to rate limiter execution (2026-07-26).

---

### WARNING

#### R-002: Missing Request Body Buffering for Org-Wide `project_key` Resolution in JSON Payload

- **Category**: Data Flow
- **Cause**: In `apps/ingestor-go/auth/apikey.go` (lines 73-82), when an Org-Wide key is used (`data.ProjectID == null`), `projectKey` is read from `r.Header.Get("X-Project-Key")`. If a client sends `project_key` in the JSON payload body without `X-Project-Key` header, the body cannot be decoded in middleware without consuming `r.Body`.
- **Affected**: `apps/ingestor-go/service/service.go` and `apps/ingestor-go/auth/apikey.go`
- **Blast Radius**: Client SDKs attempting Org-Wide ingestion using JSON body `project_key`.
- **Before**: Ingest payload contained `project_key` and was parsed inside `handleIngest`.
- **After**: Org-Wide key authentication requires `X-Project-Key` header unless `r.Body` is buffered and replaced (`io.NopCloser`).
- **Why Tests Miss It**: Integration test supplied `X-Project-Key` header instead of body `project_key`.
- **Recommendation**: Document `X-Project-Key` header requirement clearly for Org-Wide API keys in specification and client SDK documentation.
- **Status**: ACCEPTED_RISK
- **Resolution Strategy**: Option A: Enforce X-Project-Key header requirement for Org-Wide API keys as standard contract — chosen on 2026-07-26

#### R-003: Soft Delete Cascade Absence on API Key Revocation

- **Category**: State & Lifecycle
- **Cause**: In `apps/dashboard-web/src/lib/db/queries/apikeys.ts`, `revokeApiKey` updates status to `'revoked'` and sets `revoked_at = NOW()`. Redis cache key `apikey:<hash>` is purged via NATS, but in-memory LRU cache entries in running `ingestor-go` instances without Redis rely on 60-second TTL.
- **Affected**: `apps/ingestor-go/auth/apikey.go` (line 93)
- **Blast Radius**: Standalone `ingestor-go` instances running without Redis or NATS.
- **Before**: No API key caching existed.
- **After**: 60-second in-memory LRU cache TTL window exists when Redis/NATS is absent.
- **Why Tests Miss It**: Integration tests tested Redis-enabled environments.
- **Recommendation**: Accept 60-second TTL fallback for non-Redis environments as documented in D8 decision.
- **Status**: ACCEPTED_RISK
- **Resolution Strategy**: Option A: Accept 60-second TTL fallback window for non-Redis environments as documented in Decision D8 — chosen on 2026-07-26

---

### INFO

#### R-004: Default 5,000 RPM Allocation on Unset `rate_limit_rpm`

- **Category**: Resource & Performance
- **Cause**: In `apps/ingestor-go/middleware/ratelimit.go` (lines 33-36), missing or `<= 0` `rateLimitRPM` context values default to `5000`.
- **Affected**: `apps/ingestor-go/middleware/ratelimit.go`
- **Blast Radius**: Low-tier or free organization API keys.
- **Before**: No per-key rate limiting existed.
- **After**: Every active API key receives 5,000 RPM default.
- **Why Tests Miss It**: Expected default behavior.
- **Recommendation**: Retain 5,000 RPM default; support Organization tier overrides in future features.
- **Status**: ACCEPTED_RISK
- **Resolution Strategy**: Option A: Retain 5,000 RPM default quota — chosen on 2026-07-26

---

## Coverage Gap Matrix

| Category | Critical | Warning | Info | Not Applicable |
|----------|----------|---------|------|----------------|
| Data Flow | 0 | 1 | 0 | |
| State & Lifecycle | 0 | 1 | 0 | |
| Interface Contract | 0 | 0 | 0 | |
| Resource & Performance | 0 | 0 | 1 | |
| Concurrency | 0 (Resolved) | 0 | 0 | |
| Distributed Coordination | 0 | 0 | 0 | N/A — NATS JetStream invalidation verified |
| Configuration & Environment | 0 | 0 | 0 | |
| Error Propagation | 0 | 0 | 0 | |
| Observability | 0 | 0 | 0 | |

## Resolution History

| Date | Scope | Resolved | Accepted Risk | Skipped | Still Open |
|------|-------|----------|---------------|---------|------------|
| 2026-07-26T12:30:46+07:00 | all | 1 | 3 | 0 | 0 |

### Session detail (2026-07-26)
- **R-001** [CRITICAL]: Option A (Nest RateLimiter inside Authenticator) — RESOLVED
- **R-002** [WARNING]: Option A (Enforce `X-Project-Key` Header Requirement) — ACCEPTED_RISK
- **R-003** [WARNING]: Option A (Accept 60-second TTL Fallback) — ACCEPTED_RISK
- **R-004** [INFO]: Option A (Retain 5,000 RPM Default) — ACCEPTED_RISK

## Check History

| Date | Scope | Resolved | Mitigated | Worsened | Stale | New | Still Open |
|------|-------|----------|-----------|----------|-------|-----|------------|
| 2026-07-26T12:16:57+07:00 | all | 1 | 0 | 0 | 0 | 0 | 3 |

## Next Steps

- [x] Address CRITICAL finding R-001 (Resolved)
- [x] Resolve open findings and accept documented risk strategies


