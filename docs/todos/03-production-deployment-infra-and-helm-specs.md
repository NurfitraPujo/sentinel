# TODO 03: Production Deployment Infrastructure & Helm Specs

## Priority: Critical (Blocker for Production Integration)
## Status: Satisfied 2026-08-13 — artifacts landed; see below and DEPLOYMENT.md

> **Delivered 2026-08-13.** Both requirements below now have artifacts:
> - `docker-compose.prod.yml` — persistent volumes, PgBouncer connection pooling, per-service
>   resource limits, required-secret guards, loopback-only app ports.
> - `deploy/helm/sentinel/` — StatefulSets + PVCs for NATS/Postgres/Redis/MinIO (bundled deps,
>   disabled for prod in favor of managed services), Deployments + HPA + PDB for `ingestor-go`,
>   `processor-go`, `dashboard-web`, Ingress with TLS, and Secret-based credential management.
>   Validated with `helm lint`, `helm template`, and server-side `kubectl apply --dry-run`.
>
> Acceptance criteria are met by construction: NATS/Postgres use PVCs (data persists across pod
> restarts), and `ingestor-go` runs ≥2 replicas behind a Service with HPA (stateless — no conflicts).
> Operator-side items (real managed infra, backups, TLS certs) are tracked in TODO 07.

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
