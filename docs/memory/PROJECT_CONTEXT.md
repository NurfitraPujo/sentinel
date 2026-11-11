# Project Context

Last reviewed: 2024-05-20

## Product / Service
Sentinel is a high-performance data ingestion and processing pipeline designed for observability and event monitoring. It provides a robust architecture for receiving, validating, normalizing, and storing events from various sources.

## Key Constraints
- **Low Latency**: Ingestion must be fast to handle high volumes of incoming events.
- **Scalability**: The system uses NATS as a message broker to decouple ingestion from processing, allowing independent scaling of workers.
- **Data Integrity**: Protobuf is used for strict schema enforcement across all components.
- **Auditability**: Sensitive data must be masked (via the processor) before long-term storage.

## Important Domains
- **Ingestion**: Authentication, validation, and mapping of raw events.
- **Processing**: Normalization, fingerprinting, masking, and enrichment of events.
- **Observability**: Alerts, degradation detection, and notifiers.
- **Persistence**: Long-term storage in PostgreSQL.

## Current Priorities
- Standardizing event schemas via Protobuf.
- Implementing robust masking and normalization in the processor.
- Ensuring end-to-end testing via Testcontainers.

## Keep Here
- Durable product constraints.
- Domain language (Ingestor, Processor, Fingerprinting, Masking).
- Project-wide priorities that shape feature tradeoffs.

## Never Store Here
- Feature-specific acceptance criteria.
- Task lists.
- Transient implementation notes.
- Changelog entries.
