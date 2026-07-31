# Feature Memory - Architecture Alignment

## Scope Notes
- Refactor Ingestor validation to use Proto CEL rules (R001).
- Apply masking to Error Message field (R002).
- Align Proto contract with fingerprint and overrides (R003).
- Decouple Processor logic from main package (R004).
- Complete missing Error Service tasks (T008, T009, T027, T028).

## Relevant Durable Memory
- [D10] Bounded-Retry NATS Delivery: Recovery handled via bounded retries and DLQ.
- [D16] Exactly-Once Event Writes: `event_id` end-to-end idempotency and atomic writes.
- [W1] CEL Validation: The core reason for R001.

## Open Questions
- Should we use Redis for the Ingestor rate limiter now, or move it to a shared Go package first? (Resolved: Redis sliding window in ingestor-go).
- Where should the SMTP configuration be stored? Global settings table or project-specific?

