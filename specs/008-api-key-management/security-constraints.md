# Security Constraints: 008-api-key-management

**Feature**: `008-api-key-management`  
**Reviewed**: 2026-07-26  
**Status**: Security Review Complete

---

## Security Boundaries & Risk Analysis

### 1. Plaintext Key Exposure Risk
- **Risk**: Storing raw API key secret tokens in plaintext allows database compromise or server log leakage to gain full ingestion/management access.
- **Constraint**: Plaintext secret tokens MUST be generated using `crypto/rand` (32 bytes entropy), prefixed with `sent_live_` or `sent_org_`, displayed ONCE to the user upon creation, and NEVER stored in PostgreSQL or output to server logs. PostgreSQL MUST store only SHA256 digests (`key_hash`).

### 2. Authorization & Scope Bypass
- **Risk**: An `Ingest-Only` API key used to query Dashboard APIs or an unauthorized user managing org API keys.
- **Constraint**:
  - `Ingest-Only` keys MUST be restricted to error ingestion endpoints (`POST /ingest`, `POST /ingest/batch`). Any attempt to access management or query APIs MUST return `HTTP 403 Forbidden`.
  - User API key management endpoints (`/api/organizations/[orgId]/keys`) MUST enforce Organization RBAC, requiring `owner`, `admin`, or `engineer` roles.

### 3. Immediate Revocation Propagation
- **Risk**: Delayed cache invalidation permits revoked API keys to continue ingesting malicious data.
- **Constraint**: Revoking an API key in the Dashboard MUST publish a NATS JetStream invalidation event (`api_key.invalidated`). Ingestor workers MUST purge local/Redis cache entries within < 100 ms.

### 4. Denial of Service & Noisy-Neighbor Protection
- **Risk**: Misconfigured or compromised client SDKs flooding ingestion workers.
- **Constraint**: Rate-limiting middleware MUST evaluate request volume per project/API-key, return `HTTP 429` with RFC rate-limit headers before reaching NATS JetStream or PostgreSQL, and fall back to local in-memory counters if Redis is unavailable.
