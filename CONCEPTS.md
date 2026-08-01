# Concepts & Vocabulary

Key architectural terms and patterns across the Sentinel repository.

## Testing & Infrastructure

### Testcontainers Setup (`testcontainers.Setup`)
Granular test setup helper in `tests/integration/testcontainers/setup.go` supporting bitmask resource selection (`PostgresResource`, `NATSResource`, `RedisResource`, `IngestorResource`, `ProcessorResource`), Podman auto-detection via `TESTCONTAINER_PROVIDER=podman` / `TESTCONTAINERS_PODMAN=true`, and automated teardown via `t.Cleanup()`.

### Idempotent Goose Migrations
Database schema migrations executed via `goose.UpToContext` in `packages/db-migrations/goose.go` using `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` DDL statements to ensure test suite repeatability on shared or reused connection pools.

## Client SDK Architecture

### Sentinel SDK Protocol Specification
Formal specification documented in [`docs/sdk-specification.md`](file:///home/fitrapujo/oss/sentinel/docs/sdk-specification.md) defining client SDK requirements for multi-language implementations. Guarantees non-blocking asynchronous event capture, client-side PII masking (`[FILTERED]`), standardized JSON payload serialization matching `packages/proto/error_event.proto`, and bounded ring-buffer retry loops during network outages.

### SDK Package Directory Convention
All official client SDK implementations reside in the `packages/` directory (e.g. `packages/sdk-go`, `packages/sdk-js`, `packages/sdk-python`) adhering to the monorepo layer boundaries defined in `.specify/memory/architecture_constitution.md`.

## API & Security Architecture

### Ephemeral Secret Token Display
Security pattern for single-exposure API secrets (`sent_org_...` / `sk_proj_...`) generated on creation or rotation. The raw secret is returned unhashed in the server response payload exactly once and rendered in a single-exposure alert banner. Raw secret tokens are never cached in persistent client state or localStorage; only SHA-256 hashes are stored in the database.

### Email Mismatch Security Guard
Privacy and security pattern for single-use token redemption routes. Compares the authenticated user's email against the target email bound to the invitation token before returning page data. On mismatch, server load handlers return a clean error state while stripping all target organization titles, roles, and invitation details to prevent metadata enumeration attacks.

### Sole Owner Protection Guard
System authorization rule enforcing that the sole remaining owner of an organization cannot be demoted to a non-owner role or revoked from membership. Prevents organizational lockout by rejecting demotion or revocation requests with a 400 Bad Request status whenever active owner count drops to 1.


## Issue Triage & Relations

### Bi-Directional Issue Relations
Semantic linking between error issues within an organization supporting three relationship types (`linked_to`, `caused_by`, `duplicate_of`). Outgoing and incoming relation queries are unified in the database query layer so links remain visible across both source and target issue detail pages.

### Organization-Wide Alert Rule
An alert configuration that applies globally across all current and future projects within an organization rather than being scoped to a single project. Represented in the database with a `NULL` project identifier and an explicit organization identifier. Creation and mutation require the `manage_keys` organization-level capability.
