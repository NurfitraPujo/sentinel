# Implementation Plan: Organization Management Layer

**Branch**: `feature/organization-layer` | **Date**: 2026-07-24 | **Spec**: [specs/005-organization-layer/spec.md](file:///home/fitrapujo/oss/sentinel/specs/005-organization-layer/spec.md)

**Input**: Feature specification from `specs/005-organization-layer/spec.md`

## Summary

Introduce an Organization hierarchy layer above Projects in Sentinel. This enables users to belong to multiple organizations with explicit role-based access control (`owner`, `admin`, `engineer`, `support`, `viewer`), inherit project access within an organization, apply per-project access overrides via `project_members`, seamlessly switch active organization context post-login (with automatic last-active persistence), and effortlessly navigate between organization projects.

## Technical Context

**Language/Version**: Go 1.22+ (Ingestor/Processor API services), TypeScript / Node.js 20+ (SvelteKit / Next.js Web Dashboard), SQL (PostgreSQL)  
**Primary Dependencies**: Drizzle ORM (Dashboard DB queries & schema definition), Goose (Go database migrations), SvelteKit / Tailwind CSS (Dashboard UI), Protobuf / gRPC (Ingestion contracts)  
**Storage**: PostgreSQL (main storage for organizations, organization_members, organization_invitations, project_members, projects, user_session_preferences)  
**Testing**: Go unit/integration tests (`go test`), Vitest / Playwright (Dashboard UI components and end-to-end multi-tenant isolation tests)  
**Target Platform**: Linux server containerized deployment (Docker / Kubernetes)  
**Project Type**: Web Service & Dashboard (Modular Monolith)  
**Performance Goals**: Context switching and org-scoped project data loading < 500ms (SC-001); Project-to-project navigation within an org < 2 clicks (SC-002).  
**Constraints**: 100% multi-tenant isolation with zero cross-organization data leaks (SC-003); Backward compatibility with single-project setups via default personal organization migration.  
**Scale/Scope**: Support users with access to unlimited organizations; scalable DB queries indexed by `organization_id` and `user_id`.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

- **Domain-Driven**: Pass. Organization entity sits naturally above Projects in the core domain hierarchy.
- **Explicit over Implicit**: Pass. Organization membership roles (`owner`, `admin`, `engineer`, `support`, `viewer`) and project access overrides are explicitly stored and evaluated.
- **Reliability by Default**: Pass. Multi-tenant scoping logic enforced at database query and middleware levels to prevent data cross-contamination.
- **Security & Least Privilege**: Pass. Access control strictly enforced; revoking org access immediately invalidates child project access.

## Project Structure

### Documentation (this feature)

```text
specs/005-organization-layer/
├── spec.md              # Feature specification
├── plan.md              # Implementation plan
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 validation guide
├── contracts/           # Phase 1 interface contracts
│   └── organization-api.md # REST/API contract for org management & switching
└── checklists/
    └── requirements.md  # Spec quality checklist
```

### Source Code (repository root)

```text
apps/dashboard-web/
├── src/
│   ├── lib/
│   │   ├── db/
│   │   │   ├── schema.ts            # Drizzle schema (organizations, members, invitations, prefs)
│   │   │   └── queries/             # Org & project DB queries
│   │   ├── server/
│   │   │   └── auth/                # Org context & RBAC middleware
│   │   └── components/
│   │       ├── orgs/                # Org Switcher, Org Settings, Member Management UI
│   │       └── projects/            # Org-scoped Project Directory & Quick Switcher
│   └── routes/                      # Org & Project routes ([orgSlug]/...)

packages/db-migrations/
└── migrations/                      # Goose migrations (1721800000_add_organizations.sql)
```

**Structure Decision**: Web application layout reflecting Sentinel's monorepo architecture (`apps/dashboard-web` for frontend & API routes, `packages/db-migrations` for database schema updates).

## Complexity Tracking

*No constitution violations present.*
