# Feature Memory - Sentinel Error Service

## Scope Notes
- This feature implements the core ingestion and processing for error events.
- It must integrate with the `GracefulDegradation` buffer.

## Relevant Durable Memory
- **[D1] Graceful Degradation**: Must use the in-memory buffer when Postgres is down.
- **[W1] CEL Validation**: Use the defined Protobuf validation rules.

## Open Questions
- Should we apply masking (D2 - pending) before or after buffering?
- What is the retention policy for buffered events if the DB is down for > 1 hour?

## Watchlist
- Ensure NATS connection is stable before starting the consumer.
- Monitor memory usage if the database remains unavailable for extended periods.
