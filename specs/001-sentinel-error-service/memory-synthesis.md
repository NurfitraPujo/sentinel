# Memory Synthesis

## Current Scope
- Feature: 001-sentinel-error-service
- Spec: Feature Specification: Sentinel Error Service
- Feature folder: specs/001-sentinel-error-service
- Active notes: # Feature Memory - Sentinel Error Service ## Scope Notes - This feature implements the core ingestion and processing for error events . - Durability is guaranteed by NATS JetStream stream bounds and bounded retries . ## Relevant Durable Memory...
- Spec context: # Feature Specification : Sentinel Error Service **Feature Branch **: `001-sentinel-error-service` **Created**: 2026-05-09 **Updated**: 2026-05-10 **Status**: Draft — this spec was never formally closed out , but the code implementing FR-001 /FR-002...

## Relevant Project Context
- [none]

## Relevant Decisions
- [D1] Status Active Why this is durable Protects Sentinel ingestion hot path (&lt; 1ms overhead) from authentication latency and denial-of-service memory amplification while maintaining immediate (&lt; 100ms) cache invalidation upon key revocation across distributed ingestor nodes. Decision Store only SHA256 hashed API key digests ( key_hash ) in project_api_keys with raw secret tokens ( sent_live_... or sent_org_... (Source: `docs/memory/DECISIONS.md`)
- [D2] Status : active · Recorded : 2026-07-29 · Tags : nats,jetstream,reliability,delivery,dlq Context JetStream is an at-least-once transport: it redelivers until the consumer acks or terminates. The processor previously did neither correctly — on any handler error it issued a bare msg.Nak() with no delivery cap and no dead-letter path, and scripts/nats-init.sh created the consumer with --defaults (unlimited redeliveries). (Source: `docs/memory/DECISIONS.md`)
- [D3] Status SUPERSEDED by D10 (2026-07-29) — the mechanism was DELETED, not repaired. What this decision specified It required (past tense — none of this exists in code any more) that when PostgreSQL was unavailable, the processor buffer incoming events in memory (MaxBufferSize = 10,000) and flush them once the connection was restored, so a temporary outage could not lose events. Why it is gone The mechanism never delivered its guarantee, and could not be made to. (Source: `docs/memory/DECISIONS.md`)
- [D4] Status Active Why this is durable Establishes the real-time regression reopening architecture inside the high-throughput Go ingestion worker while decoupling asynchronous issue linkage to protect ingestion throughput. Decision Perform automated version-aware regression reopening ( detectAndHandleRegression ) directly inside apps/processor-go/store/store.go during event ingestion. Maintain 0% read/lock overhead on issue_relations on the high-throughput ingestion path. (Source: `docs/memory/DECISIONS.md`)
- [D5] Status Active Why this is durable Silent or partially-applied schema changes corrupt production data and are expensive to detect. This policy is enforced by goose itself (transactional DDL + advisory lock) and is the contract every migration author must honor. Decision Migrations MUST follow these non-negotiable rules: Single-run only : Concurrent migration runs against the same target are blocked by goose 's advisory lock. (Source: `docs/memory/DECISIONS.md`)

## Active Architecture Constraints
- [A1] Status Active Why this is durable ErrorEvent (the protobuf message in packages/proto/sentinel/v1/error_event.proto ) is the only place the field names, types, and size limits of an ingested error event are declared once. (Source: `docs/memory/ARCHITECTURE.md`)

## Accepted Deviations
- [V1] Status Active — commit cd84d17 (P0+P1) is on main ; P2/P2b are staged, uncommitted as of this entry (36 files, +2501/-254). This entry is written from that staged state; re-verify after it merges. Why this is durable Every milestone above this one in this file recorded a merge event, not verified behavior. (Source: `docs/memory/WORKLOG.md`)
- [V2] Status Resolved (both halves) — recorded for the general lesson, which will recur. Symptoms The ingest pipeline's front door ( /ingest , S3) rejected 100% of well-formed events for the life of the project. (Source: `docs/memory/BUGS.md`)

## Relevant Security Constraints
- [none]

## Related Historical Lessons
- [B1] Status Resolved for the SDK↔ingestor seam — a contract test now exists and runs in CI. The dashboard↔ingestor NATS seam (symptom (b) below) is untouched by this work and remains open; do not mark it resolved. Symptoms Producer and consumer both work, both are tested, and the integration is 100% broken. (Source: `docs/memory/BUGS.md`)
- [B2] Status Resolved. Kept here — do not delete — because the original defect and the near-miss on its first fix attempt are both worth knowing about; see &quot;Fix, and how the fix could have failed&quot; below. Symptoms Any holder of an active ingest -scope API key can write error events into any other tenant's project by naming that project in the JSON request body. (Source: `docs/memory/BUGS.md`)

## Conflict Warnings
- [c] potentially stale memory surfaced from bugs / recurring bug patterns (`docs/memory/`) / 2026-07-29 - a component with zero throughput proves nothing about what's behind it (source: `docs/memory/bugs.md`)
- [c] potentially stale memory surfaced from decisions / technical decisions (`docs/memory/`) / entry lifecycle / 2024-05-20 - graceful degradation via in-memory buffering (source: `docs/memory/decisions.md`)

## Retrieval Notes
- Index entries considered: 10
- Source sections read: 10
- Budget status: within limit
