# Sentinel Deployment Runbook

This is the operator's guide to deploying Sentinel. Two supported paths:

| Path | Artifact | Use when |
|------|----------|----------|
| **Kubernetes** | `deploy/helm/sentinel` (Helm chart) | Multi-node, autoscaling, managed cloud infra |
| **Single host** | `docker-compose.prod.yml` | One VM / small footprint / on-prem box |

> The root `docker-compose.yml` is the **dev/CI** stack (ephemeral volumes, insecure defaults,
> bundled Jaeger). Never run it in production — use one of the two paths above.

## Architecture recap

```
SDK ──HTTP─▶ ingestor-go ──▶ NATS JetStream ──▶ processor-go ──▶ PostgreSQL
                                                                     ▲
                                          dashboard-web ────────────┘ (reads same DB)
```

Stateful dependencies: **PostgreSQL** (system of record), **NATS JetStream** (durable event
queue — the pipeline's durability boundary), **Redis** (API-key cache + rate-limit state),
**S3/MinIO** (attachment object store). Only **ingestor-go** (ingest) and **dashboard-web** (UI/API)
should be externally reachable, both behind TLS.

---

## 0. Build & publish images

Five images back a deployment. Compose can build them in place (`up -d --build`); for Kubernetes you
build and push to a registry, then set the tags in your Helm values.

| Image | Dockerfile | Build context | Notes |
|-------|-----------|---------------|-------|
| ingestor-go | `apps/ingestor-go/Dockerfile` | repo root (`.`) | static Go binary |
| processor-go | `apps/processor-go/Dockerfile` | repo root (`.`) | static Go binary |
| dashboard-web | `apps/dashboard-web/Dockerfile` | repo root (`.`) | SvelteKit node build |
| **db-migrate** | `packages/db-migrations/Dockerfile` | `packages/db-migrations` | binary **+ on-disk .sql files** — see nuance below |
| dlq-drainer | `scripts/Dockerfile.dlq-drainer` | repo root (`.`) | DLQ maintenance tool |

The one-shot init images (`scripts/Dockerfile.nats-init`, `scripts/Dockerfile.minio-init`) create the
JetStream streams and the attachments bucket; Compose builds them automatically, Helm references them
as `init.nats.image` / `init.minio.image`.

```bash
# Example: build and push the migration image (context is the MODULE dir, not the repo root)
docker build -t registry.example.com/sentinel/db-migrate:1.4.0 packages/db-migrations
docker push registry.example.com/sentinel/db-migrate:1.4.0
```

> **db-migrate nuance — migrations are read from disk, not embedded.** The `migrate` CLI resolves
> `packages/db-migrations/migrations/*.sql` relative to its working directory at runtime. The image
> therefore ships both the binary (`/usr/local/bin/migrate`, its ENTRYPOINT) and that directory under
> `WORKDIR /app`, so the default path resolution finds them. Its build context is the module directory
> (`packages/db-migrations`) because that module is self-contained (no `replace` directives). Invoke it
> as `migrate <command> -target=<processor|ingestor|dashboard>` with the DSN in `DB_URL_<TARGET>`; the
> CLI redacts the password in its log output. Adding a `.sql` file means rebuilding this image.

## 1. Secrets — generate these first

Never reuse the dev defaults. Generate strong values:

```bash
openssl rand -hex 32   # AUTH_SECRET  (Auth.js session signing; >= 32 chars)
openssl rand -hex 16   # SETTINGS_ENCRYPTION_KEY must be EXACTLY 32 chars — use: openssl rand -hex 16
openssl rand -hex 32   # CRON_SECRET  (guards /api/cron/* retention endpoints)
openssl rand -base64 24 # POSTGRES_PASSWORD, S3_SECRET_KEY, REDIS_PASSWORD
```

| Secret | Consumed by | Notes |
|--------|-------------|-------|
| `POSTGRES_PASSWORD` | all services + migrate | |
| `REDIS_PASSWORD` | ingestor | optional but recommended |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | dashboard, minio | real S3/R2 credentials in prod |
| `AUTH_SECRET` | dashboard | Auth.js; rotating it invalidates all sessions |
| `SETTINGS_ENCRYPTION_KEY` | dashboard | **exactly 32 chars**; encrypts stored org settings — rotating it makes existing encrypted settings unreadable |
| `CRON_SECRET` | dashboard | bearer token for retention cron |
| `EMAIL_SERVER` | dashboard | SMTP DSN; **without it, invitations cannot be delivered** (by design, D06) |
| `GOOGLE_CLIENT_ID/SECRET` | dashboard | only if using Google SSO |

---

## 2. The one deployment gotcha that isn't obvious: `S3_PUBLIC_ENDPOINT`

Large uploads (>25 MB, the M6 presigned path) hand a **presigned PUT URL to the user's browser**.
SigV4 signs the hostname into that URL, so it must be signed against a host the **browser** can
reach — never the in-cluster endpoint the server uses.

- `S3_ENDPOINT` — server-side endpoint (e.g. `http://minio:9000`, `https://s3.us-east-1.amazonaws.com`). Used for all server S3 calls.
- `S3_PUBLIC_ENDPOINT` — **browser-reachable** endpoint (e.g. `https://uploads.example.com`). Used **only** to sign presigned PUT URLs.

If `S3_PUBLIC_ENDPOINT` points at an in-cluster host, small uploads (proxied) keep working while
large uploads silently fail from the browser — a failure mode a localhost test cannot catch.

---

## 3. Database migrations

All three goose targets (`processor`, `ingestor`, `dashboard`) share **one physical database**;
migrations are idempotent and replayed per target. Migrations run automatically:

- **Compose:** the `migrate` service (built from `packages/db-migrations/Dockerfile`) runs on `up`
  before the apps start, connecting **direct-to-Postgres**, not through PgBouncer — DDL must not go
  through a transaction pooler.
- **Helm:** a `pre-install`/`pre-upgrade` hook Job runs every target before the app pods roll, using
  the `images.migrate` image. It overrides the image ENTRYPOINT to loop
  `migrate up -target=<t>` over `migrate.targets`. Point it at Postgres **directly** (bypass any
  pooler) via `config.postgres.host`.

Both invoke the same `db-migrate` image (§0). Because migrations are read from disk, **a new `.sql`
file requires rebuilding and republishing that image** before the upgrade.

Never point migrations at a shared database you can't afford to lose during testing (see the repo
convention on `tests/integration`).

---

## 4a. Deploy on Kubernetes (Helm)

```bash
# 1. Provision managed Postgres, NATS, Redis, and S3 (or a bucket on R2/GCS).
# 2. Create the app Secret (or use External Secrets Operator / Vault):
kubectl create namespace sentinel
kubectl -n sentinel create secret generic sentinel-app-secrets \
  --from-literal=POSTGRES_PASSWORD=... \
  --from-literal=REDIS_PASSWORD=... \
  --from-literal=S3_ACCESS_KEY=... --from-literal=S3_SECRET_KEY=... \
  --from-literal=AUTH_SECRET=... --from-literal=SETTINGS_ENCRYPTION_KEY=... \
  --from-literal=CRON_SECRET=... --from-literal=EMAIL_SERVER=smtp://... \
  --from-literal=GOOGLE_CLIENT_ID= --from-literal=GOOGLE_CLIENT_SECRET=

# 3. Copy and edit the prod values (disables bundled deps, points at managed infra):
cp deploy/helm/sentinel/values-prod-example.yaml my-prod-values.yaml
$EDITOR my-prod-values.yaml   # set image tags, hosts, S3_PUBLIC_ENDPOINT, ingress hosts

# 4. Install / upgrade:
helm upgrade --install sentinel deploy/helm/sentinel \
  -n sentinel -f my-prod-values.yaml

# 5. Verify:
kubectl -n sentinel rollout status deploy/sentinel-sentinel-dashboard
kubectl -n sentinel get pods
```

**Render before you apply** (catches template/value errors without touching the cluster):

```bash
helm template sentinel deploy/helm/sentinel -f my-prod-values.yaml | kubectl apply --dry-run=client -f -
```

Chart specifics:
- **Bundled deps (postgres/nats/redis/minio) are single-replica StatefulSets with PVCs — demo only.**
  In `values-prod-example.yaml` they are all disabled; you point `config.*` at managed services.
- **HPA** is on by default for the three app services; **PodDisruptionBudgets** keep ≥1 pod during
  drains. **NATS JetStream storage MUST be persistent** — that PVC is the pipeline's durability boundary.
- The **dlq-drainer** is deployed but inert (`drainerEnabled`/`drainerExecute` both `false`, per D14).

## 4b. Deploy on a single host (Compose)

```bash
# 1. Create a root-owned .env with every required secret (see the header of docker-compose.prod.yml):
sudo install -m600 /dev/null /opt/sentinel/.env
sudo $EDITOR /opt/sentinel/.env

# 2. Bring the stack up (the migrate service runs first):
docker compose -f docker-compose.prod.yml --env-file /opt/sentinel/.env up -d --build

# 3. Wait for health, then check:
docker compose -f docker-compose.prod.yml ps
```

The stack **refuses to start** if a required secret is unset (`:?` expansion) — this is intentional.
Only `127.0.0.1:3000` (dashboard) and `127.0.0.1:8080` (ingestor) are published; put a TLS reverse
proxy (nginx/Caddy/Traefik) in front of both. `processor` scales with
`docker compose -f docker-compose.prod.yml up -d --scale processor=3`.

---

## 5. Pre-production readiness checklist

Tracked, with current status, in
[docs/todos/07-pre-production-readiness-checklist.md](docs/todos/07-pre-production-readiness-checklist.md).
The short version:

- [ ] TLS termination in front of ingestor **and** dashboard
- [ ] Managed/persistent PostgreSQL with **WAL archiving + daily snapshots**
- [ ] Persistent SSD volume for NATS JetStream (`ERROR_EVENTS` stream)
- [ ] PgBouncer (Compose ships it; on K8s use your managed pooler / add one)
- [ ] Real secrets generated; no `change-me` / dev defaults anywhere
- [ ] `S3_PUBLIC_ENDPOINT` set to a browser-reachable URL (see §2)
- [ ] `EMAIL_SERVER` configured (invitations depend on it, D06)
- [ ] `OTEL_EXPORTER_OTLP_ENDPOINT` pointed at your collector (optional; absent = no traces, still functional)
- [ ] Retention cron scheduled to POST `/api/cron/retention` with the `CRON_SECRET` bearer token
- [ ] Object-store lifecycle/backup policy for attachments

---

## 6. Observability

- **Metrics:** `processor` exposes Prometheus `/metrics` on `:8081`; the Helm pod carries
  `prometheus.io/scrape` annotations. Scrape both Go services.
- **Traces:** one distributed trace spans ingestor → NATS → processor when
  `OTEL_EXPORTER_OTLP_ENDPOINT` is set. If the collector is down, services degrade to a single
  warning log and stay fully functional (never a hard dependency).
- **Logs:** structured JSON with `trace_id`/`span_id` correlation across all services.

## 7. Backup & recovery

- **PostgreSQL** is the system of record — WAL archiving + periodic base backups (or managed
  automated backups). This is the primary recovery target.
- **NATS JetStream** holds in-flight events not yet persisted to Postgres; its PVC/volume protects
  against event loss on restart. Streams are bounded (D13).
- **Object store** holds attachments — enable versioning/lifecycle on the bucket.
- The **DLQ** (`ERROR_EVENTS_DLQ`) collects events the processor could not handle. Inspect/replay
  with `tools/dlq`; the drainer stays OFF until an operator enables both gates (D14).
