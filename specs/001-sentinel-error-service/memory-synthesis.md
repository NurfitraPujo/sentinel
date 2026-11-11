# Memory Synthesis

## Current Scope
- Feature: 001-sentinel-error-service
- Spec: Feature Specification: Sentinel Error Service
- Feature folder: specs/001-sentinel-error-service
- Active notes: # Feature Memory - Sentinel Error Service ## Scope Notes - This feature implements the core ingestion and processing for error events . - It must integrate with the `GracefulDegradation` buffer . ## Relevant Durable Memory - **[D1] Graceful Degradation...
- Spec context: # Feature Specification : Sentinel Error Service **Feature Branch **: `001-sentinel-error-service` **Created**: 2026-05-09 **Updated**: 2026-05-10 **Status**: Draft Input : User description : "Currently, debugging errors across our Go APIs , Go Workers...

## Relevant Project Context
- [C1] Low Latency : Ingestion must be fast to handle high volumes of incoming events. Scalability : The system uses NATS as a message broker to decouple ingestion from processing, allowing independent scaling of workers. Data Integrity : Protobuf is used for strict schema enforcement across all components. (Source: `docs/memory/PROJECT_CONTEXT.md`)
- [C2] Ingestion : Authentication, validation, and mapping of raw events. Processing : Normalization, fingerprinting, masking, and enrichment of events. Observability : Alerts, degradation detection, and notifiers. (Source: `docs/memory/PROJECT_CONTEXT.md`)

## Relevant Decisions
- [D1] Status Active Why this is durable Sentinel is an observability platform. Losing events during a temporary database outage defeats the purpose of the platform. This decision ensures that short-term infrastructure issues don't lead to permanent data loss. (Source: `docs/memory/DECISIONS.md`)

## Active Architecture Constraints
- [A1] Ingestor-go : Handles incoming traffic, authentication, and initial validation. Acts as a producer for NATS. Processor-go : Consumes events from NATS, performs heavy lifting (masking, normalization, fingerprinting), and stores results in the database. (Source: `docs/memory/ARCHITECTURE.md`)
- [A2] Queue-First : All non-read operations should be handled asynchronously via background workers ( ingestor-go , processor-go ). Consistency : Use the queue system to ensure eventual consistency and system resilience. (Source: `.specify/memory/architecture_constitution.md`)
- [A3] Entry Layer : apps/ingestor-go , apps/processor-go , apps/dashboard-web . Contract Layer : packages/proto . Defines the &quot;source of truth&quot; for cross-module communication. (Source: `.specify/memory/architecture_constitution.md`)

## Accepted Deviations
- [none]

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
