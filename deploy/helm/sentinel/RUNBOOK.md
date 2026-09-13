# Sentinel Huawei CCE Runbook

Deploy path: SWR images → SealedSecrets → ArgoCD sync → CCE (`hwc-daya-cce-production`).

## 0. One-time setup (per env)

```bash
# 1. Create the 7 SWR repos (private, app_server) under daya-engineering:
for repo in sentinel-ingestor sentinel-processor sentinel-dashboard \
    sentinel-db-migrate sentinel-dlq-drainer sentinel-nats-init sentinel-minio-init; do
  hcloud SWR CreateRepo --cli-region=ap-southeast-3 \
    --namespace=daya-engineering --repository="$repo" \
    --is_public=false --category=app_server \
    --description="Repository for Sentinel $repo image"
done

# 2. Managed infra must exist first: RDS Postgres, DCS Redis, OBS bucket.
#    Record their private IPs / endpoints into values-huawei-<env>.yml
#    (externalServices + config.*). NATS may stay bundled (default) or managed.

# 3. Confirm the image pull secret exists in the target namespace:
kubectl --context hwc-daya-cce-production -n sentinel-staging get secret default-secret
```

Namespaces (`sentinel-staging`, `sentinel-production`) are created by ArgoCD
(`CreateNamespace=true` on staging; create manually for production since its
sync policy is manual).

## 1. Seal secrets (you do this — ciphertext is never committed as plaintext)

SealedSecrets are namespace-scoped. Seal each value with kubeseal-encryptor
scoped to the target namespace, then paste the ciphertext into
`values-huawei-<env>.yml` under `secrets.sealedData`:

```bash
# Example per key (repeat for all 11 keys in secrets.sealedData):
curl -s -X POST https://kubeseal-encryptor-daya-cce-production.daya.ai/encrypt \
  -H 'Content-Type: application/json' \
  -d '{"namespace":"sentinel-staging","secretName":"sentinel-secrets","key":"POSTGRES_PASSWORD","value":"<plaintext>"}'
```

Required keys and generation (same contract as DEPLOYMENT.md §1):

| Key | Generation |
|---|---|
| `POSTGRES_PASSWORD` | `openssl rand -base64 24` (must match the RDS password) |
| `REDIS_PASSWORD` | `openssl rand -base64 24` |
| `S3_ACCESS_KEY` / `S3_SECRET_KEY` | OBS credentials for the env bucket |
| `AUTH_SECRET` | `openssl rand -hex 32` (≥32 chars; rotation invalidates sessions) |
| `SETTINGS_ENCRYPTION_KEY` | **exactly 32 chars**: `openssl rand -hex 16` |
| `SENTINEL_ENCRYPTION_KEY` | base64 of exactly 32 bytes: `openssl rand -base64 32` |
| `CRON_SECRET` | `openssl rand -hex 32` |
| `EMAIL_SERVER` | SMTP DSN, e.g. `smtp://user:pass@smtp.host:587` |
| `GOOGLE_CLIENT_ID` / `GOOGLE_CLIENT_SECRET` | empty string sealed if SSO unused |

## 2. Deploy order

```bash
# Render + lint (no cluster needed):
helm lint deploy/helm/sentinel
helm template sentinel deploy/helm/sentinel \
  -f deploy/helm/sentinel/values-huawei-staging.yml > /tmp/sentinel-stg.yaml
kube-linter lint --config deploy/helm/sentinel/.kube-linter.yaml /tmp/sentinel-stg.yaml

# First install (ArgoCD owns the release afterwards):
#   1. Apply argocd-app/staging.yml to ArgoCD (or let the app-of-apps pick it up).
#   2. ArgoCD syncs: migrate hook Job runs first (pre-install), then app pods roll.
#   3. Verify:
kubectl --context hwc-daya-cce-production -n sentinel-staging rollout status deploy/sentinel-sentinel-dashboard
kubectl --context hwc-daya-cce-production -n sentinel-staging get pods
curl -k https://api-sentinel-stg.daya.ai/health
curl -k https://sentinel-stg.daya.ai/
```

Releases afterwards: push tag `vX.Y.Z-staging` → CI builds + pushes 7 SWR
images (`:vX.Y.Z-staging`) → ArgoCD `app set --parameter *.image.tag` + sync.
Production: tag `vX.Y.Z` (no `-staging` suffix) → same flow against
`sentinel-production` after the manual environment gate.

## 3. Rollback

```bash
# Via ArgoCD: History and Rollback in the UI, or:
argocd app history sentinel-staging
argocd app rollback sentinel-staging <id>
# Image-only rollback (fastest for a bad image):
argocd app set sentinel-staging --parameter dashboard.image.tag=<previous-tag>
argocd app sync sentinel-staging
```

Migrations are forward-only (goose, one shared DB): rolling back app images
does not roll back schema. If a migration itself is bad, fix forward with a
new migration. If a `migrate down` is ever hand-run anyway, roll the images
back to the previous tag FIRST — `down` is never run against a database with
new-tag pods attached: dropping a table the running processor and dashboard
read turns into an ingest outage (every `StoreEvent` fails, is retried, then
dead-lettered), not a broken chart.

### Re-running the issue_occurrence_buckets backfill (sentinel-issue-trend)

`1724800000_backfill_issue_occurrence_buckets.sql` fills `issue_occurrence_buckets`
from `error_occurrences` once, and goose never replays an applied version — so
occurrences stored by the OLD processor in the rollout's last minutes, or during
a rollback to the pre-feature tag, are never bucketed. To fill the missing hours
by hand, run the same statement WITHOUT the migration's `WHERE NOT EXISTS` guard
against the shared database (outside any transaction; it is safe with the new
processor running — the inserts take only `FOR KEY SHARE` on `issues` rows):

```sql
INSERT INTO issue_occurrence_buckets (issue_id, bucket_start, count) SELECT issue_id, date_trunc('hour', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', count(*) FROM error_occurrences GROUP BY 1, 2 ON CONFLICT (issue_id, bucket_start) DO NOTHING;
```

An hour that already has a row keeps its live count — the gap inside an existing
hour is not recoverable by re-running, only hours with no row at all are filled.
Images are rolled back to the previous tag before any `migrate down` (§3 above):
the table this statement writes is dropped by `1724700000`'s Down, under a
running new-tag processor that is an ingest outage.

## 4. Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| Pods `ImagePullBackOff` | `default-secret` missing/wrong in namespace | `kubectl -n sentinel-<env> get secret default-secret` |
| `POSTGRES_HOST` unresolvable | external-services Endpoints IP wrong | `kubectl -n sentinel-<env> get endpoints rds-postgres-sentinel` |
| SealedSecret never decrypts | sealed for wrong namespace/scope | re-seal scoped to `sentinel-<env>`; controller logs |
| Ingress has no address | ELB id / cert ids wrong | `hcloud ELB ShowLoadBalancer` for the id in values |
| Large uploads fail, small work | `S3_PUBLIC_ENDPOINT` not browser-reachable | must resolve from end-user browsers (DEPLOYMENT.md §2) |
| Dashboard 503 on repo credentials | `SENTINEL_ENCRYPTION_KEY` unset/invalid | must be base64 of exactly 32 bytes |
| Cron 401s | `CRON_SECRET` mismatch | retention CronJob reads the same chart Secret |
