# Worklog

Use concise high-value entries only.
This is not a changelog. Do not record routine releases, version bumps, or implementation summaries.

### 2024-05-20 - Adopted CEL for Protobuf Validation

- **Why durable**: Validation logic traditionally drifted between the Go Ingestor and any potential future clients. CEL allows embedding validation rules directly in the schema.
- **Future mistake prevented**: Mismatched validation logic between producers and consumers.
- **Evidence**: `packages/proto/error_event.proto` uses `buf.validate.message` with CEL expressions.
- **Where to look**: `packages/proto/error_event.proto`

## Template

### YYYY-MM-DD - Summary

- why this is durable
- what future mistake it prevents
- evidence
- where future contributors should look

## Example

### 2026-03-15 - Pagination cursor must be opaque to clients

- **Why durable**: three features so far have tried to expose raw database offsets as pagination cursors, each time creating breaking changes when the underlying query changes
- **Future mistake prevented**: next time a feature adds pagination, the implementer will know to use opaque cursors from the start
- **Evidence**: specs 018, 024, and 031 all required pagination rework; see DECISIONS.md entry on API pagination
- **Where to look**: `src/api/pagination.ts`, `docs/memory/DECISIONS.md`

## Counter-Example (do not write entries like this)

> ### 2026-03-15 - Updated pagination
>
> - Changed pagination to use cursors
> - Deployed to staging

This is a changelog entry, not a durable lesson. It records what happened, not what was learned.
