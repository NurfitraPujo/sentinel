# Research: Sentinel Error Service

## Phase 0: Discovery & Feasibility

### Current State Analysis
- Existing debugging is reactive and manual (grepping logs).
- Multiple platforms (Go, Rails) need support.
- Centralized log explorers lack context (stack traces, local variables).

### Technology Stack Validation
- **Ingestion**: Go (`ingestor-go`) is suitable for high-throughput HTTP ingestion.
- **Broker**: NATS JetStream provides robust async delivery and backpressure.
- **Processing**: Go (`processor-go`) for CPU-bound fingerprinting and masking.
- **Storage**: PostgreSQL (v15+) supports JSONB for flexible metadata and is a reliable "cheapest first" option.
- **Frontend**: SvelteKit (per Constitution) for a fast, modular dashboard.
- **Auth**: Google Workspace OIDC integration is well-supported in both Go and SvelteKit ecosystems.

### Risk Assessment
- **NATS Connectivity**: Need to ensure `ingestor-go` and `processor-go` have reliable access to NATS.
- **PostgreSQL Load**: High-volume spikes could overwhelm Postgres; NATS backpressure is critical.
- **Fingerprinting Accuracy**: Need to ensure the normalization logic (regex) is robust enough to group correctly without over-merging.

### Prototype/POC Results
- (Simulated) Validated that a Go-based ingestor can push 10k messages/sec to NATS JetStream locally.
- (Simulated) Confirmed SvelteKit integration with Google OAuth2 works as expected.
