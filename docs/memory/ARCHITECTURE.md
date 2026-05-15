# Architecture

Last reviewed: 2024-05-20

## System Overview
Sentinel follows a decoupled, event-driven architecture using NATS as the central message broker. Data flows from source to the Ingestor, through NATS, into the Processor, and finally to PostgreSQL.

## Major Components
- **Ingestor-go**: Handles incoming traffic, authentication, and initial validation. Acts as a producer for NATS.
- **Processor-go**: Consumes events from NATS, performs heavy lifting (masking, normalization, fingerprinting), and stores results in the database.
- **Dashboard-web**: Frontend for visualization and management of ingested events.
- **NATS**: Distributed message broker for service decoupling.
- **PostgreSQL**: Primary data store for processed events and metadata.

## Boundaries
- **Ingestor vs Processor**: Communication is purely asynchronous via NATS. The Ingestor should never wait for processing to complete.
- **Internal vs External**: The Ingestor is the only component exposed to external data sources. The Processor and Database remain in internal network layers.

## Integrations
- **Protobuf**: Shared contracts between all services.
- **Testcontainers**: Used for integration testing with live NATS and Postgres instances.
- **Tailwind CSS**: Standardized styling for the dashboard.

## Risks / Complexity Hotspots
- **NATS Backpressure**: High ingestion rates could lead to NATS buffer overflows if the Processor lags.
- **Database Indexing**: Large event volumes require careful indexing strategies in PostgreSQL to maintain query performance.

## Keep Here
- Stable system boundaries.
- Ownership lines between modules or services (e.g., Ingestor owns auth, Processor owns masking).
- Integration constraints that affect many features.

## Never Store Here
- Step-by-step implementation plans.
- One-off feature details.
- Stale diagrams without current boundaries.
