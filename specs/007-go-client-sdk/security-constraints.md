# Security Constraints: Go Client SDK

**Feature**: `007-go-client-sdk`  
**Reviewed**: 2026-07-25  

---

## Validated Trust Boundaries & Constraints

1. **Client-Side PII Masking**: SDK MUST scrub sensitive metadata fields (`password`, `authorization`, `secret`, `token`, `credit_card`, `ssn`) to `"[FILTERED]"` before sending over the wire.
2. **API Key Authentication**: All single and batch requests to `ingestor-go` MUST include `X-API-Key` header matching a valid project key.
3. **Bounded Memory & Rate Limiting**: Max buffer size (100 events) and client-side sampling prevent Denial-of-Service (DoS) memory exhaustion on client applications.
4. **Log Recursion Protection**: Internal SDK errors MUST NOT be sent back through the SDK's own logger adapter to prevent infinite logging loops.
