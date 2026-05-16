# Implementation Plan: Architecture Alignment & Service Completion

**Branch**: `003-architecture-alignment` | **Date**: 2024-05-20 | **Spec**: [spec.md](spec.md)
**Input**: Architecture Review Report (V1-V6), Missing Tasks (T008, T009, T027, T028)

## Summary
Resolve critical architectural drift in the ingestion and processing pipelines while completing pending dashboard and infrastructure tasks.

## Technical Context
- **Validation**: `protovalidate-go` (replaces manual logic in `validator.go`)
- **Security**: Regex-based masking for `Message` field
- **Structure**: Service-layer extraction for `processor-go`
- **Infrastructure**: Redis for distributed rate limiting
- **UI**: SvelteKit for SMTP settings

## Constitution Check

| Rule | Status | Notes |
|------|--------|-------|
| Explicit Contracts (Protos) | NEUTRAL | Currently drift exists; this plan fixes it (V2). |
| Layer Boundaries | NEUTRAL | Currently eroded in Processor; this plan restores them (V3). |
| Logic Leakage | FAIL | Business logic currently in handlers/main; this plan extracts it. |
| Validation Boundaries | FAIL | Manual validation bypasses Proto CEL; this plan implements `protovalidate` (V1). |

## Architectural Standards

### 1. Proto-First Validation (R001)
- The Ingestor MUST use the `protovalidate` library to enforce rules defined in `error_event.proto`.
- Manual checks in `validator.go` will be deprecated in favor of CEL expressions.

### 2. Sanitization & Fingerprinting (R002, R005)
- Masking MUST happen before storage and MUST cover the `Message` field.
- Normalization and Masking MUST occur BEFORE fingerprinting to ensure consistent grouping of variants.

### 3. Service Extraction (R004)
- Processor orchestration logic will move from `package main` to `package service`.
- Persistence logic remains in `package store`.

### 4. Shared State (T028)
- In-memory rate limiting will be replaced by a Redis-backed implementation to support horizontal scaling of the Ingestor.

## Implementation Steps

### Phase 1: Contract & Validation (R001, R003)
1. Update `error_event.proto` with `fingerprint` and `fingerprint_override` fields.
2. Add missing CEL rules to `error_event.proto` (e.g., regex for platform).
3. Integrate `protovalidate-go` into `apps/ingestor-go/service/service.go`.

### Phase 2: Security & Processor Refactor (R002, R004, R005)
1. Update `apps/processor-go/event/event.go` to mask the `Message` field.
2. Re-order `Deserialize` logic: Normalize -> Mask -> Fingerprint.
3. Extract `processEventInternal` logic to `apps/processor-go/service/processor_service.go`.

### Phase 3: Infrastructure & Persistence (T027, T028)
1. Implement Redis client in `packages/shared-go`.
2. Migrate `apps/ingestor-go/middleware/ratelimit.go` to use Redis.
3. Implement `apps/processor-go/store/audit_store.go` to persist audit events.

### Phase 4: Dashboard & SMTP (T008, T009)
1. Create SvelteKit route `apps/dashboard-web/src/routes/settings/auth`.
2. Implement SMTP settings form and persistence in PostgreSQL `settings` table.

## Security Review

### Validation Risk
Transitioning to `protovalidate` ensures that the Ingestor cannot accept malformed data that skips manual checks.

### Masking Effectiveness
Masking the primary `Message` field closes a major PII leak. We must ensure regex patterns are exhaustive.

### Token Security
SMTP credentials in the database MUST be encrypted at rest if the environment provides a KMS or similar mechanism.

## Complexity Tracking
- **Risk**: Updating the fingerprinting order will change hashes for existing errors. This is an accepted deviation for long-term de-duplication health.
- **Risk**: Redis dependency adds infrastructure overhead for local development.

---
**Next Step**: Generate detailed tasks in `tasks.md`.
