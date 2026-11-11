# TODO 07: Pre-Production Readiness Checklist

## Priority: Recommended
## Status: In Progress (deployment artifacts landed 2026-08-13; operator items remain)

### Overview
Action items to execute immediately before connecting live production systems to Sentinel.
Reconciled 2026-08-13 against `docs/memory/VERIFIED_STATE.md` and the new deployment artifacts.

Deployment artifacts now exist (previously the blocker in TODO 03):
- **`DEPLOYMENT.md`** — operator runbook (both paths).
- **`docker-compose.prod.yml`** — hardened single-host stack (required secrets, PgBouncer,
  resource limits, no insecure defaults, loopback-only app ports).
- **`deploy/helm/sentinel/`** — Kubernetes Helm chart (Deployments + HPA + PDB for the three app
  services, StatefulSets/PVCs for optional bundled deps, migration hook Job, Ingress + TLS,
  Secret/ConfigMap). Validated with `helm lint`, `helm template`, and server-side `kubectl` dry-run.

### Checklist

Legend: ✅ done in code/artifacts · ☐ operator action required per environment

- [x] ✅ **Ingestor Rate Limiting**: per-project rate limiting is implemented and wired (R1 in
  VERIFIED_STATE.md — it had been silently disabled and was fixed). Enable strict rejection with
  `RATELIMIT_STRICT_MODE=true` (the prod compose file and prod Helm values default it on).
- [x] ✅ **Connection Pooling**: `docker-compose.prod.yml` runs **PgBouncer** (transaction mode) in
  front of Postgres; apps connect through it, migrations connect directly. On Kubernetes, use your
  managed pooler or add one — the chart points `config.postgres.host` at whatever you supply.
- [x] ✅ **Observability**: `slog` structured logs everywhere, Prometheus `/metrics` on both Go
  services, and one distributed trace ingestor → NATS → processor (P9-2, DONE). Operator: point
  `OTEL_EXPORTER_OTLP_ENDPOINT` at a collector and scrape `/metrics`.
- [ ] ☐ **SSL / TLS Termination**: enforce TLS in front of `ingestor-go` and the dashboard. Helm
  Ingress ships TLS-enabled (bring a cert / cert-manager issuer); Compose publishes app ports on
  loopback only — front them with a TLS reverse proxy.
- [ ] ☐ **NATS Storage Volume**: persistent SSD for JetStream `ERROR_EVENTS`. Helm gives NATS a PVC
  (durability boundary); Compose uses a named volume — put it on SSD-backed storage.
- [ ] ☐ **Database Backup Strategy**: automated PostgreSQL WAL archiving + daily snapshots. Not
  automated by either artifact — configure on your managed instance / host.
- [ ] ☐ **Secrets**: generate strong values for every secret (see DEPLOYMENT.md §1); no `change-me`
  or dev defaults. Compose refuses to start if a required secret is unset; Helm `NOTES.txt` warns on
  leftover `change-me` placeholders.
- [ ] ☐ **`S3_PUBLIC_ENDPOINT`**: set to a **browser-reachable** object-store URL (DEPLOYMENT.md §2).
  Required for M6 large uploads; a wrong value fails only for large browser uploads, which localhost
  testing cannot catch.
- [ ] ☐ **Email delivery (`EMAIL_SERVER`)**: invitations depend entirely on working SMTP (a
  deliberate consequence of D06 — the raw invitation token is no longer surfaced in the UI).
- [ ] ☐ **Retention cron**: schedule a job to POST `/api/cron/retention` with the `CRON_SECRET`
  bearer token (honors `DATA_RETENTION_DAYS`). Not scheduled by either artifact.
- [ ] ☐ **Object-store lifecycle/backup**: enable bucket versioning / lifecycle for attachments.
- [ ] ☐ **SDK Retry & Async Buffer**: ensure client apps submit errors asynchronously off the
  request thread with retry backoff (client-side, outside this repo's deployment).

### Related
- `docs/todos/03-production-deployment-infra-and-helm-specs.md` — the infra spec this now satisfies.
- `DEPLOYMENT.md` — the runbook.
