# TODO 03: Production Deployment Infrastructure & Helm Specs

## Priority: Critical (Blocker for Production Integration)
## Status: Pending

### Overview
Sentinel local development relies on Podman and Testcontainers. A production deployment requires infrastructure-as-code manifests for Kubernetes and production Docker Compose with storage persistence and connection pooling.

### Requirements
1. **Production Docker Compose (`docker-compose.prod.yml`)**:
   - NATS JetStream container configured with persistent volume storage on SSD.
   - PostgreSQL container with PgBouncer connection pooling.
   - Resource limits (CPU/Memory) for `ingestor-go` and `processor-go`.

2. **Kubernetes Helm Chart (`deploy/helm/sentinel`)**:
   - StatefulSets for NATS JetStream and PostgreSQL with PVC templates.
   - Deployments and HPA (Horizontal Pod Autoscaler) for `ingestor-go` and `processor-go`.
   - Ingress configurations with TLS termination.
   - Secrets management for DB credentials, NATS auth tokens, and session secrets.

### Acceptance Criteria
- NATS JetStream stream data persists across pod/container restarts.
- `ingestor-go` scale out to multiple replicas behind a load balancer without state conflicts.
