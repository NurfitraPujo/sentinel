# Architecture Refactor & Migration Tasks: Organization Layer

## Data Model & Schema Migrations
- **AR-001: Data Migration Script for Unassigned Legacy Projects**
  - **Type**: Migration
  - **Module**: `packages/db-migrations`
  - **Description**: Ensure legacy projects created prior to the Organization layer are safely migrated into default personal organizations (`Personal Org - <user_id>`) during migration execution (`1721800000_add_organization_layer.sql`).

- **AR-002: Modularization of Project Query Scoping**
  - **Type**: Refactor
  - **Module**: `apps/dashboard-web/src/lib/db/queries/projects.ts`
  - **Description**: Refactor pre-existing project queries to require `organization_id` parameters, guaranteeing that direct project lookups validate organization ownership.
