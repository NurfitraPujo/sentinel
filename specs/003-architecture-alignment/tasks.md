# Tasks: Architecture Alignment & Service Completion

**Input**: Design documents from `/specs/003-architecture-alignment/`
**Prerequisites**: plan.md (required), spec.md (required)

## Phase 1: Contract & Validation (R001, R003)
- [x] T001 Update `packages/proto/error_event.proto` with `fingerprint` and `fingerprint_override` fields.
- [x] T002 Add CEL regex rules for `platform` and `environment` to `error_event.proto`.
- [x] T003 Install `github.com/bufbuild/protovalidate-go` in `apps/ingestor-go`.
- [x] T004 Replace manual validation in `apps/ingestor-go/service/service.go` with `protovalidate`.
- [x] T005 Deprecate `apps/ingestor-go/validation/validator.go` manual checks.

## Phase 2: Security & Processor Refactor (R002, R004, R005)
- [x] T006 Update `apps/processor-go/event/event.go` to mask the `Message` field.
- [x] T007 Re-order `Deserialize` in `apps/processor-go/event/event.go`: Normalize -> Mask -> Fingerprint.
- [x] T008 Create `apps/processor-go/service/processor_service.go` and extract orchestration from `processor.go`.
- [x] T009 Update `apps/processor-go/main.go` to use the new `ProcessorService`.

## Phase 3: Infrastructure & Persistence (T027, T028)
- [x] T010 Implement Redis client in `packages/shared-go/redis`.
- [x] T011 Refactor `apps/ingestor-go/middleware/ratelimit.go` to use Redis-backed window counter.
- [x] T012 Create `apps/processor-go/store/audit_store.go` and implement `PersistAuditLog`.
- [x] T013 Update audit logging in `processor-go` to use the new store.

## Phase 4: Dashboard & SMTP (T008, T009)
- [x] T014 Create database migration for `settings` table with encrypted `value` column.
- [x] T015 Create SvelteKit route `apps/dashboard-web/src/routes/settings/auth/+page.svelte`.
- [x] T016 Implement SMTP settings save/test functionality in the dashboard.

## Phase 5: Verification
- [x] T017 Verify that duplicate errors with different dynamic IDs are correctly grouped (V5 fix).
- [x] T018 Verify that PII in the error message is masked in the database (V4 fix).
- [x] T019 Verify that invalid Protobuf payloads are rejected by `protovalidate` (V1 fix).
- [x] T020 Run full architecture guard scan to confirm resolution of V1-V6.
