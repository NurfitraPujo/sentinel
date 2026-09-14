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

### Rolling back sentinel-releases-snooze to the pre-feature tag — TWO steps, in this order

`1724900000_add_releases_and_issue_snooze.sql` keeps its CHECK
`check_snooze_only_when_ignored` on `issues` (contract decision 37): a row may
carry `snoozed_until` / `snooze_occurrence_target` only while `status = 'ignored'`.
A pre-feature dashboard's `updateIssueStatus` and `batchUpdateIssues` change
`status` without nulling columns they do not know exist, so **an old-tag resolve
or reopen of a still-snoozed issue is a class-23 constraint error — a `500` to
that caller, no data change — until the snooze columns are nulled.** That is why
the image roll-back is step 2, never step 1.

**Step 1 — against the shared database, with the new-tag pods still running:**

```sql
UPDATE issues SET snoozed_until = NULL, snooze_occurrence_target = NULL, snooze_count_at = NULL
 WHERE snoozed_until IS NOT NULL OR snooze_occurrence_target IS NOT NULL;
```

New-tag pods are correct with the nulls: every running snooze degrades to a
plain indefinite ignore — the ignore survives, the condition does not.

**Step 2 — the image roll-back** (the `argocd app set … image.tag=<previous-tag>`
+ `sync` above, for `processor` and `dashboard` alike).

After both steps the old processor stores occurrences without writing `releases`
or `first_release_version` and evaluates regressions with the old compare; the
old dashboard resolves and reopens without error; the old service still
dispatches alerts for ignored issues. Releases first seen during the rollback are
never recorded: `1725000000`'s backfill is a goose version like every other and
does not re-run on the next roll-forward (contract decision 22). The dashboard's
`RollingUpdate` minutes apply to the roll-back too (see the rollout note below).

No `migrate down` is needed for this rollback and none should be run while
new-tag pods are attached (§3 above): `1724900000`'s Down drops `releases` and the
four `issues` columns the running new-tag processor and dashboard read.

### Rolling out sentinel-releases-snooze (release note)

Rollout order is the chart's existing one and it is load-bearing here (contract
§8): **`db-migrate` runs before `processor` and `dashboard` come up on the new
tag.** A new-tag processor against a database without `1724900000` fails every
event (`42703` / `42P01`, retried then dead-lettered); a new-tag dashboard's
`issues` select fails on the four missing columns. The reverse — a migrated
database under old-tag pods — is correct: they simply write no releases. There is
no feature flag: an empty `releases` table and all-`NULL` columns render today's
page and decide today's regressions.

- **File 2's `UPDATE` takes a row lock on every issue it rewrites** while the
  old-tag pods are still ingesting at wave 1. Under its `lock_timeout = '5s'` a
  busy row fails the migrate Job, the sync stalls at wave 1 and the old pods keep
  running — safe. Re-sync: the file re-runs and its `WHERE NOT EXISTS` guards skip
  what the first attempt finished (contract decision 42).
- **Backfilled `first_seen` is exact only inside the retention window**: a release
  whose earliest occurrences were swept before the backfill ran reads the earliest
  *retained* one, later than the truth, and every ordering by `first_seen`
  inherits that for pre-feature releases (contract decision 31).
- **The `RollingUpdate` minutes — processor.** The processor Deployment has no
  `strategy:` and rolls over `replicaCount: 2`; for the minutes the old-tag pod is
  still draining, its occurrences are stored **without** a `releases` row or a
  `first_release_version`, after the backfill has already looked. They never
  count on `releases`, and an issue *created* in that window reads `—` for good —
  unlike the trend's bucket gaps, these do NOT age out with retention. Accepted
  (contract decision 34); `strategy: Recreate` on the processor Deployment for
  this one release is the operator's alternative if the permanent gap matters.
- **The `RollingUpdate` minutes — dashboard.** The dashboard Deployment has no
  `strategy:` either. For its minutes an old-tag dashboard pod serves
  `/issues/{id}` with no control block and no facts (nothing 500s on read), and a
  status write it makes on a row a new-tag pod has already snoozed is refused by
  `check_snooze_only_when_ignored` — a `500` to that one caller, no data change.
  The window holds only rows snoozed in those same minutes and closes when the
  last old pod is gone. Accepted (contract decision 37); `strategy: Recreate` on
  the dashboard Deployment for this one release is the named alternative.
- **Rollback is the two-step recipe above** — the nulling `UPDATE` first, the
  image roll-back second.
- **`next` becomes a version by a post-commit rewrite, with an hourly catch-up.**
  For a window of milliseconds a new release exists and a `next` issue still
  reads `next`; a crash inside that window leaves it `next` until the retention
  cron's catch-up rewrites it — so the window is **at most one hour** (contract
  §8, decision 15).
- **`next` is project-level and its rewrite crosses environments**: a release
  first seen in any environment of the project resolves every `next` issue of
  that project, whichever environment the issue is in (contract decisions 32, 41).
- **Every plain-ignored issue stops paging, and the alert frequency window is not
  fed during a snooze**: the processor skips alert dispatch for an `ignored`
  outcome (contract decision 16, §6), so the occurrence that ends a snooze is
  dispatched normally and starts its window cold (decision 17). **A snooze ending
  sends no notification** — the issue returns to `unresolved` with a
  `status_changed` activity row; subscribers see it on `/`, in the activity feed
  and on `GET /api/agent/events` (contract decision 45).
- **A release's *Issues introduced* decays with retention while its
  *Occurrences* is lifetime** (contract §4.4): the count reads
  `issues.first_release_version`, which retention can delete with the issue;
  `releases.occurrence_count` is never decremented.

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
