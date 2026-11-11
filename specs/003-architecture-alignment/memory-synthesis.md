# Memory Synthesis

## Current Scope
- Feature: 003-architecture-alignment
- Spec: Spec: Architecture Alignment & Completion
- Feature folder: specs/003-architecture-alignment
- Active notes: # Feature Memory - Architecture Alignment ## Scope Notes - Refactor Ingestor validation to use Proto CEL rules (R001). - Apply masking to Error Message field (R002). - Align Proto contract with fingerprint and overrides (R003). - Decouple Processor logic...
- Spec context: # Spec : Architecture Alignment & Completion ## Goal Bring the Sentinel codebase into full alignment with its Constitution and complete the pending Error Service implementations . ## Requirements 1 . **Validation**:...

## Relevant Project Context
- [none]

## Relevant Decisions
- [D1] Status Active Why this is durable Sentinel is an observability platform. Losing events during a temporary database outage defeats the purpose of the platform. This decision ensures that short-term infrastructure issues don't lead to permanent data loss. (Source: `docs/memory/DECISIONS.md`)

## Active Architecture Constraints
- [A1] Entry Layer : apps/ingestor-go , apps/processor-go , apps/dashboard-web . Contract Layer : packages/proto . Defines the &quot;source of truth&quot; for cross-module communication. (Source: `.specify/memory/architecture_constitution.md`)
- [A2] Ingestor-go : Handles incoming traffic, authentication, and initial validation. Acts as a producer for NATS. Processor-go : Consumes events from NATS, performs heavy lifting (masking, normalization, fingerprinting), and stores results in the database. (Source: `docs/memory/ARCHITECTURE.md`)
- [A3] Source of Truth : All cross-module and API contracts must be defined in packages/proto . Validation Strategy : Go : Use Proto-based validation (e.g., proto-gen-validate ) or manual checks within domain models. SvelteKit : Use Zod for runtime validation of inputs (Actions/Loaders). (Source: `.specify/memory/architecture_constitution.md`)
- [A4] Automated tools (Architecture Guard) will scan for violations during the planning and implementation phases. PRs containing P0 violations will be blocked until remediated or the Constitution is updated. (Source: `.specify/memory/architecture_constitution.md`)
- [A5] Contract-Based : Imports between apps/ and packages/ must be restricted to shared libraries and proto definitions. No Direct Imports : Apps must not directly import internals from other apps. (Source: `.specify/memory/architecture_constitution.md`)

## Accepted Deviations
- [V1] Boundary Violation : Direct cross-module imports skipping contracts or shared packages. Logic Leakage : Business logic implemented in handlers, controllers, or transport layers. Unvalidated Input : Missing or bypassed input validation (Proto/Zod). (Source: `.specify/memory/architecture_constitution.md`)

## Relevant Security Constraints
- [S1] Data Integrity : Ensure that ingested error data is sanitized and stored securely. Least Privilege : Workers and apps should only have access to the resources they strictly need. Contract Enforcement : Use Protos to ensure that data entering and moving through the system is valid and safe. (Source: `.specify/memory/constitution.md`)

## Related Historical Lessons
- [B1] Status Active Symptoms Events are lost or dropped when the Processor cannot reach the PostgreSQL database. Root Cause Processor service traditionally assumed the database is always available during event processing. Future mistake prevented Failing to handle transient database connection failures in processing workers. (Source: `docs/memory/BUGS.md`)

## Conflict Warnings
- [none]

## Retrieval Notes
- Index entries considered: 10
- Source sections read: 10
- Budget status: within limit
