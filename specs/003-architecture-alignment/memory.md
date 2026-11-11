# Feature Memory - Architecture Alignment

## Scope Notes
- Refactor Ingestor validation to use Proto CEL rules (R001).
- Apply masking to Error Message field (R002).
- Align Proto contract with fingerprint and overrides (R003).
- Decouple Processor logic from main package (R004).
- Complete missing Error Service tasks (T008, T009, T027, T028).

## Relevant Durable Memory
- [D1] Graceful Degradation: Must be preserved during refactor.
- [B1] Data Loss Prevention: Ensure buffer logic remains intact.
- [W1] CEL Validation: The core reason for R001.

## Open Questions
- Should we use Redis for the Ingestor rate limiter now, or move it to a shared Go package first?
- Where should the SMTP configuration be stored? Global settings table or project-specific?
