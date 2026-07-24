# TODO 06: Prometheus Metrics & OpenTelemetry Tracing

## Priority: Important
## Status: Pending

### Overview
To operate Sentinel in production, operations teams need deep visibility into pipeline latency, message queue depth, DB buffer status, and service health metrics.

### Requirements
1. **Prometheus Metrics Endpoint (`/metrics`)**:
   - `ingestor-go`: Request counts by status code, ingestion latency histograms, rate-limited request counts.
   - `processor-go`: Processed event throughput, fingerprinting latency, NATS JetStream pending consumer lag, `GracefulDegradation` buffer size metrics.

2. **OpenTelemetry Tracing**:
   - Trace context propagation from `ingestor-go` HTTP headers -> NATS JetStream message headers -> `processor-go` handler -> PostgreSQL query execution.

### Acceptance Criteria
- `/metrics` endpoints are exposed for Prometheus scraping.
- Grafana dashboard template provided in `deploy/dashboards/sentinel-pipeline.json`.
