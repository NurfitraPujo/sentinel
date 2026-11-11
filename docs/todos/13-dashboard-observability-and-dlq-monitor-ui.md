# TODO 13: System Observability & DLQ Monitoring Dashboard UI

## Priority: Low
## Status: Completed

### Overview
`ingestor-go` and `processor-go` export Prometheus metrics via `/metrics` and send OpenTelemetry traces. Additionally, `processor-go` exposes DLQ detail endpoints (`/dlq`). However, the frontend dashboard lacks a dedicated operational health and DLQ monitoring view for system administrators.

### Requirements
1. **System Health & Metrics View**:
   - Create a dashboard view (`/settings/observability` or `/[orgSlug]/system`) displaying ingestor throughput, event processing latency, and processor queue metrics.
2. **Dead-Letter Queue (DLQ) Inspector**:
   - Build a UI table listing dead-lettered events with machine-readable error classes (`sentinel.error_permanent`, etc.).
   - Allow administrators to inspect raw DLQ payloads and failure causes.

### Affected Files
- `apps/dashboard-web/src/routes/settings/observability/+page.svelte` (New)
- `apps/dashboard-web/src/routes/settings/observability/+page.server.ts` (New)

### Acceptance Criteria
- Administrators can inspect real-time ingestor & processor operational health.
- Dead-lettered events can be viewed with error classifications and stack context.
