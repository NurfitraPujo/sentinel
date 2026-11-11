# TODO 02: Multi-Tenant Project Auth & API Key Management

## Priority: Critical (Blocker for Production Integration)
## Status: Pending

### Overview
While database migrations (`1716550000_add_project_members.sql`) established tables for projects and members, Sentinel lacks complete UI/API lifecycle management for API keys and multi-tenant rate limiting.

### Requirements
1. **API Key Lifecycle in Dashboard (`apps/dashboard-web`)**:
   - UI for creating, listing, revoking, and rotating project API keys.
   - Key scoping: Ingest-Only vs. Read/Query API keys.
   - SHA256 hashed storage verification for all API keys.

2. **Ingestor Rate Limiting & Quotas (`apps/ingestor-go`)**:
   - Per-project / per-API key rate limiting middleware using Redis/in-memory token bucket.
   - Return `HTTP 429 Too Many Requests` when project event quotas are exceeded.
   - Protect NATS JetStream and PostgreSQL from noisy-neighbor spikes.

### Acceptance Criteria
- Revoked API keys are immediately rejected by `apps/ingestor-go`.
- Ingestor rate limits prevent high-volume floods from single client keys.
- Project members can manage keys directly from dashboard settings.
