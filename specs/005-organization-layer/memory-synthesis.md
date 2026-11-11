# Memory Synthesis: Organization Management Layer

## Historical Context & Architecture Constraints

1. **Modular Monolith Boundaries**:
   - Sentinel is structured as a modular monolith (`apps/dashboard-web` for web/API routes, `packages/db-migrations` for database schema migrations, and Go services for ingestion/processing).
   - Domain logic and database access MUST remain explicit, type-safe (TypeScript/Drizzle & Go SQL), and scoped to multi-tenant organization identifiers (`organization_id`).

2. **Least Privilege & Role-Based Access Control**:
   - Authentication is global (user identity level).
   - Authorization is scoped per organization via `organization_members` (`owner`, `admin`, `engineer`, `support`, `viewer`).
   - Project-level overrides (`project_members`) take precedence over inherited organization roles for fine-grained project access control.

3. **Data Integrity & Isolation**:
   - Database migrations MUST gracefully handle pre-existing unassigned projects by provisioning a default personal organization and populating foreign keys without data loss.
   - All dashboard API routes and database queries MUST enforce `organization_id` scoping to prevent cross-tenant data leaks.
