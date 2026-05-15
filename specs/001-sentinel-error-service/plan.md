# Implementation Plan: Sentinel Error Service - Magic Link Auth

**Branch**: `001-sentinel-error-service` | **Date**: 2026-05-15 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/001-sentinel-error-service/spec.md` + user requirement for magic link authentication

## Summary

Add magic link authentication as a fallback for local development environments where Google Workspace OIDC is unavailable. Uses Auth.js built-in Email provider for magic link support rather than custom implementation.

## Technical Context

**Language/Version**: Go 1.22+, TypeScript 5.0+, Node.js 20+  
**Primary Dependencies**: SvelteKit, Drizzle ORM, pgx/v5, NATS JetStream, Auth.js (SvelteKit), Google Auth Library  
**Storage**: PostgreSQL 15+  
**Testing**: Go `testing` package (Workers), Vitest/Playwright (Dashboard)  
**Target Platform**: Kubernetes / Dockerized Environment + Local Development  
**Project Type**: Web Service + Background Workers  
**Performance Goals**: Ingestion < 50ms, Processing < 200ms, Search < 1s  
**Constraints**: Magic link tokens must expire within 15 minutes; tokens are single-use; local auth must not bypass project RBAC  
**Scale/Scope**: 1k+ events per second (throttled via NATS)

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Rule | Status | Notes |
|------|--------|-------|
| Modular Monolith Structure | PASS | Using `apps/` and `packages/` as required. |
| Explicit Contracts (Protos) | PASS | Defining error event schema in `packages/proto`. |
| CQRS Lite Pattern | PASS | Separating ingestion (write) from dashboard (read). |
| Async Processing (NATS) | PASS | Using NATS JetStream between Ingestor and Processor. |
| Google Workspace Auth | PASS | Planned for production dashboard authentication. |
| Domain-Driven Design | PASS | Logic for authentication resides in domain layer. |
| Auth.js Provider Pattern | PASS | Using Auth.js Email provider instead of custom auth abstraction |

## Security Standards

### Authorization & Multi-tenancy
All dashboard data access must strictly enforce project ownership. Every server-side load function and action (SvelteKit) that takes a resource ID (Issue, Occurrence, Project) must verify that the authenticated user's organization has a valid role for that specific project.

### Magic Link Authentication (via Auth.js Email Provider)
- Tokens are managed by Auth.js (not stored in our database directly)
- Tokens are cryptographically random (min 32 bytes) - handled by Auth.js
- Tokens expire within 15 minutes of generation - configurable in Auth.js
- Tokens are single-use - handled by Auth.js token invalidation
- Magic link auth MUST NOT bypass RBAC - authenticated user still requires project membership
- Email sender MUST be configurable (SMTP)

## Architectural Standards

### Auth.js Integration
The dashboard uses Auth.js for authentication. Adding magic link support means:
- Adding Auth.js Email provider to existing providers array
- Configuring SMTP adapter for email delivery
- Using existing session storage and RBAC integration

No custom `AuthProvider` interface needed - Auth.js handles provider abstraction internally.

## Project Structure

### Documentation (this feature)

```text
specs/001-sentinel-error-service/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Proto definitions
└── tasks.md             # Phase 2 output
```

### Source Code (repository root)

```text
apps/
├── ingestor-go/         # HTTP API for error ingestion
│   └── service/         # Ingestion orchestration logic
├── processor-go/        # NATS consumer for grouping/masking
└── dashboard-web/       # SvelteKit interface
    └── src/lib/
        ├── auth.ts     # Auth.js config (Google + Email providers)
        ├── email/      # Email/SMTP utilities
        └── server/     # Server-side logic (RBAC, audit)

packages/
├── proto/               # Shared protobuf definitions
├── shared-go/           # Shared Go utilities (DB, NATS)
└── tailwind-config/     # Shared styles
```

**Structure Decision**: Adhering to the Modular Monolith pattern. Magic link auth is implemented within existing Auth.js framework, adding only SMTP configuration and email templates.

## Complexity Tracking

*No violations detected. Plan uses Auth.js built-in Email provider for magic link, avoiding custom implementation.*

---

**Generated Artifacts**:
- Plan: `/home/fitrapujo/oss/sentinel/specs/001-sentinel-error-service/plan.md`
- Branch: `001-sentinel-error-service`