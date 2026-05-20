# Recurring Bug Patterns (`docs/memory/`)

This file stores durable implementation bug patterns and their mitigations. For systemic, high-risk, or governance-level patterns, see `.specify/memory/BUGS.md`.

### 2024-05-20 - Data Loss on Database Outage
**Status**
Active

**Symptoms**
Events are lost or dropped when the Processor cannot reach the PostgreSQL database.

**Root Cause**
Processor service traditionally assumed the database is always available during event processing.

**Future mistake prevented**
Failing to handle transient database connection failures in processing workers.

**Evidence**
Historical analysis of ingestion gaps during database maintenance windows.

**Prevention / Detection**
Use the `GracefulDegradation` buffer in `apps/processor-go/degradation`. Monitor `WARNING: Database unavailable` logs and buffer size metrics.

**Where to look next**
`apps/processor-go/degradation/buffer.go`
