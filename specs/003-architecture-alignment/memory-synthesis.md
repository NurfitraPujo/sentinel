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
- [D1] Status Active Why this is durable Protects Sentinel ingestion hot path (&lt; 1ms overhead) from authentication latency and denial-of-service memory amplification while maintaining immediate (&lt; 100ms) cache invalidation upon key revocation across distributed ingestor nodes. Decision Store only SHA256 hashed API key digests ( key_hash ) in project_api_keys with raw secret tokens ( sent_live_... or sent_org_... (Source: `docs/memory/DECISIONS.md`)
- [D2] Status SUPERSEDED by D10 (2026-07-29) — the mechanism was DELETED, not repaired. What this decision specified It required (past tense — none of this exists in code any more) that when PostgreSQL was unavailable, the processor buffer incoming events in memory (MaxBufferSize = 10,000) and flush them once the connection was restored, so a temporary outage could not lose events. Why it is gone The mechanism never delivered its guarantee, and could not be made to. (Source: `docs/memory/DECISIONS.md`)
- [D3] Status : active · Recorded : 2026-07-29 · Tags : nats,jetstream,reliability,delivery,dlq Context JetStream is an at-least-once transport: it redelivers until the consumer acks or terminates. The processor previously did neither correctly — on any handler error it issued a bare msg.Nak() with no delivery cap and no dead-letter path, and scripts/nats-init.sh created the consumer with --defaults (unlimited redeliveries). (Source: `docs/memory/DECISIONS.md`)
- [D4] Status Active Why this is durable Establishes the real-time regression reopening architecture inside the high-throughput Go ingestion worker while decoupling asynchronous issue linkage to protect ingestion throughput. Decision Perform automated version-aware regression reopening ( detectAndHandleRegression ) directly inside apps/processor-go/store/store.go during event ingestion. Maintain 0% read/lock overhead on issue_relations on the high-throughput ingestion path. (Source: `docs/memory/DECISIONS.md`)

## Active Architecture Constraints
- [A1] Status Active Why this is durable ErrorEvent (the protobuf message in packages/proto/sentinel/v1/error_event.proto ) is the only place the field names, types, and size limits of an ingested error event are declared once. (Source: `docs/memory/ARCHITECTURE.md`)
- [A2] Status Active — superseded in part by P0-3 ( docs/plans/E2E_RECOVERY_PLAN.md , constraint C1, now resolved 2026-07-28). Why this is durable Module boundaries determine what can ever be tested together. Until P0-3, this layout silently forbade an entire class of test, which is why the SDK↔ingestor contract broke undetected (S4/B5). (Source: `docs/memory/ARCHITECTURE.md`)
- [A3] The component diagram above describes the intended design. Several of its arrows do not exist in the running binaries. Before reasoning about data flow, read VERIFIED_STATE.md . (Source: `docs/memory/ARCHITECTURE.md`)

## Accepted Deviations
- [V1] Status Active — commit cd84d17 (P0+P1) is on main ; P2/P2b are staged, uncommitted as of this entry (36 files, +2501/-254). This entry is written from that staged state; re-verify after it merges. Why this is durable Every milestone above this one in this file recorded a merge event, not verified behavior. (Source: `docs/memory/WORKLOG.md`)

## Relevant Security Constraints
- [S1] Tradeoffs Gained : a cross-tenant write requires forging or stealing a credential, not just guessing or knowing another tenant's project name. Made harder : an org-wide key's caller must get the header/body precedence right — a client that sets both, expecting the body to be a fallback confirmation, will find the header silently wins and the body is ignored. This is a minor footgun for SDK authors, not a security issue. (Source: `docs/memory/ARCHITECTURE.md`)

## Related Historical Lessons
- [B1] Status Resolved for the SDK↔ingestor seam — a contract test now exists and runs in CI. The dashboard↔ingestor NATS seam (symptom (b) below) is untouched by this work and remains open; do not mark it resolved. Symptoms Producer and consumer both work, both are tested, and the integration is 100% broken. (Source: `docs/memory/BUGS.md`)

## Conflict Warnings
- [c] potentially stale memory surfaced from architecture / architecture / never store here / 2026-07-26 - three-module go layout, workspace mode for local/contract testing only (updated 2026-07-28) (source: `docs/memory/architecture.md`)
- [c] potentially stale memory surfaced from decisions / technical decisions (`docs/memory/`) / entry lifecycle / 2024-05-20 - graceful degradation via in-memory buffering (source: `docs/memory/decisions.md`)

## Retrieval Notes
- Index entries considered: 10
- Source sections read: 10
- Budget status: within limit
