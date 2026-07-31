# Feature Memory - Sentinel Error Service

## Scope Notes
- This feature implements the core ingestion and processing for error events.
- Durability is guaranteed by NATS JetStream stream bounds and bounded retries.

## Relevant Durable Memory
- **[D10] Bounded-Retry NATS Delivery**: Uses NATS JetStream bounded retry with dead-letter capture (`ERROR_EVENTS_DLQ`).
- **[W1] CEL Validation**: Use the defined Protobuf validation rules.

## Open Questions
- What is the retention policy for DLQ events? (Resolved: 30 days / 1 GiB per D13).

## Watchlist
- Ensure NATS connection is stable before starting the consumer.
- Monitor DLQ depth on `/health` during processing failures.

