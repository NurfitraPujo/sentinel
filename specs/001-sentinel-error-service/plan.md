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
