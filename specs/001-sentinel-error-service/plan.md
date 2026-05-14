# Implementation Plan: Sentinel Error Service

**Branch**: `001-sentinel-error-service` | **Date**: 2026-05-09 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/001-sentinel-error-service/spec.md`

## Summary
Implement a high-performance error tracking system consisting of a Go-based ingestion and processing pipeline, NATS JetStream for async messaging, and a SvelteKit dashboard. The system will de-duplicate errors into issues, perform server-side PII masking, and provide a searchable interface for root-cause analysis.

## Technical Context

**Language/Version**: Go 1.22+, TypeScript 5.0+, Node.js 20+  
**Primary Dependencies**: NATS JetStream, PostgreSQL 15+, SvelteKit, Google Auth Library  
**Storage**: PostgreSQL (Occurrences, Issues, Projects)  
**Testing**: Go `testing` package (Workers), Vitest/Playwright (Dashboard)  
**Target Platform**: Kubernetes / Dockerized Environment  
**Project Type**: Web Service + Background Workers  
**Performance Goals**: Ingestion < 50ms, Processing < 200ms, Search < 1s  
**Constraints**: < 1% error drop rate during ingestion, PII masking mandatory  
**Scale/Scope**: 1k+ events per second (throttled via NATS)

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Rule | Status | Notes |
|------|--------|-------|
| Modular Monolith Structure | PASS | Using `apps/` and `packages/` as required. |
| Explicit Contracts (Protos) | PASS | Defining error event schema in `packages/proto`. |
| CQRS Lite Pattern | PASS | Separating ingestion (write) from dashboard (read). |
| Async Processing (NATS) | PASS | Using NATS JetStream between Ingestor and Processor. |
| Google Workspace Auth | PASS | Planned for Dashboard authentication. |
| Domain-Driven Design | PASS | Logic for fingerprinting and masking resides in domain layer. |

## Architecture

### CQRS Lite Enforcement
The **Processor** layer is strictly forbidden from performing direct database queries. All data access must go through the `Store` abstraction, which must explicitly separate Command (write) and Query (read) interfaces. This ensures that processing logic remains decoupled from persistence and maintains the integrity of the "write" side of the system.

The **Dashboard** (UI layer) is strictly forbidden from direct database knowledge. All data retrieval must be encapsulated within a dedicated `QueryService`. SvelteKit loaders and actions must only interact with this service, ensuring that the "read" side of the system is isolated from the underlying schema and Drizzle ORM implementation.

## Architectural Standards

### Fingerprinting
Error fingerprinting must use the `file:function` format for stack trace analysis. Custom fingerprints provided by clients must be mandatory hashed before storage to ensure consistent indexing and prevent collision with system-generated keys.

### Contract Layer
Protobuf definitions in `packages/proto` must enforce validation of metadata size. The aggregate size of the `metadata` field in error events must not exceed 64KB to ensure performance and prevent resource exhaustion during ingestion and processing.

## Security Standards

### Authorization & Multi-tenancy
All dashboard data access must strictly enforce project ownership. Every server-side load function and action (SvelteKit) that takes a resource ID (Issue, Occurrence, Project) must verify that the authenticated user's organization has a valid role for that specific project.

### Data Protection (PII Masking)
The PII masking engine in the processing layer must use high-precision matching. Sensitive keys (e.g., `password`, `token`) must be matched exactly or via scoped regex to prevent the "over-redaction" of non-sensitive metadata that happens with broad substring matching.

## Project Structure

### Documentation (this feature)

```text
specs/001-sentinel-error-service/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Proto definitions
└── checklists/
    └── requirements.md  # Spec quality checklist
```

### Source Code (repository root)

```text
apps/
├── ingestor-go/         # HTTP API for error ingestion
│   └── service/         # Ingestion orchestration logic (prevents handler leakage)
├── processor-go/        # NATS consumer for grouping/masking
└── dashboard-web/       # SvelteKit interface

packages/
├── proto/               # Shared protobuf definitions
├── shared-go/           # Shared Go utilities (DB, NATS)
└── tailwind-config/     # Shared styles
```

**Structure Decision**: Adhering to the Modular Monolith pattern defined in the Architecture Constitution.

## Complexity Tracking

*No violations detected. Plan strictly follows the Constitution.*
