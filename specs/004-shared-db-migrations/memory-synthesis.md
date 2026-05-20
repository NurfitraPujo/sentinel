# Memory Synthesis

## Current Scope
- Feature: 004-shared-db-migrations
- Spec: Feature Specification: Shared Database Migrations
- Feature folder: specs/004-shared-db-migrations
- Spec context: # Feature Specification : Shared Database Migrations **Feature Branch **: `004-shared-db-migrations` **Created**: 2024-05-24 **Status**: Draft **Input**: User description : "create database migrations that supports incremental migration setup . The database migrations is...

## Relevant Project Context
- [C1] Low Latency : Ingestion must be fast to handle high volumes of incoming events. Scalability : The system uses NATS as a message broker to decouple ingestion from processing, allowing independent scaling of workers. Data Integrity : Protobuf is used for strict schema enforcement across all components. (Source: `docs/memory/PROJECT_CONTEXT.md`)

## Relevant Decisions
- [D1] Status Active Why this is durable Local development environments may not have access to Google Workspace OIDC. Magic link authentication provides a fallback that doesn't bypass project RBAC. (Source: `docs/memory/DECISIONS.md`)
- [D2] Status Active Why this is durable Sentinel is an observability platform. Losing events during a temporary database outage defeats the purpose of the platform. This decision ensures that short-term infrastructure issues don't lead to permanent data loss. (Source: `docs/memory/DECISIONS.md`)

## Active Architecture Constraints
- [A1] Ingestor-go : Handles incoming traffic, authentication, and initial validation. Acts as a producer for NATS. Processor-go : Consumes events from NATS, performs heavy lifting (masking, normalization, fingerprinting), and stores results in the database. (Source: `docs/memory/ARCHITECTURE.md`)
- [A2] Contract-Based : Imports between apps/ and packages/ must be restricted to shared libraries and proto definitions. No Direct Imports : Apps must not directly import internals from other apps. (Source: `.specify/memory/architecture_constitution.md`)
- [A3] Automated tools (Architecture Guard) will scan for violations during the planning and implementation phases. PRs containing P0 violations will be blocked until remediated or the Constitution is updated. (Source: `.specify/memory/architecture_constitution.md`)

## Accepted Deviations
- [V1] Boundary Violation : Direct cross-module imports skipping contracts or shared packages. Logic Leakage : Business logic implemented in handlers, controllers, or transport layers. Unvalidated Input : Missing or bypassed input validation (Proto/Zod). (Source: `.specify/memory/architecture_constitution.md`)

## Relevant Security Constraints
- [S1] Data Integrity : Ensure that ingested error data is sanitized and stored securely. Least Privilege : Workers and apps should only have access to the resources they strictly need. Contract Enforcement : Use Protos to ensure that data entering and moving through the system is valid and safe. (Source: `.specify/memory/constitution.md`)

## Related Historical Lessons
- [B1] Status Active Symptoms Events are lost or dropped when the Processor cannot reach the PostgreSQL database. Root Cause Processor service traditionally assumed the database is always available during event processing. Future mistake prevented Failing to handle transient database connection failures in processing workers. (Source: `docs/memory/BUGS.md`)

## Conflict Warnings
- [none]

## Specific Migration Decisions
- **PostgreSQL Focus**: All migrations must be optimized for PostgreSQL features.
- **Shared Packages Pattern**: Migrations live in `packages/db-migrations` to be accessible by all Go apps.
- **Unified Schema Structure**: Migrations are unified into a single root directory (`packages/db-migrations/migrations/`) rather than partitioned by app target.
- **Unix Timestamp Versioning**: Use a Unix timestamp (to the seconds) as the version prefix (e.g., `1716541200_init.sql`) to support parallel development and ensure global uniqueness.

## Retrieval Notes
- Index entries considered: 10
- Source sections read: 10
- Budget status: within limit
