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
# Example per key (repeat for all 11 keys in secrets.sealedData, plus TASK-07's two MAXMIND_* keys):
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
| `MAXMIND_ACCOUNT_ID` | the numeric MaxMind account id (sentinel-event-enrichment TASK-07); read only by the geoip-refresh CronJob |
| `MAXMIND_LICENSE_KEY` | a GeoLite licence key from that MaxMind account; never paste it anywhere but the encryptor. Read only by the geoip-refresh CronJob |

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

### Pre-deploy check before a processor tag (sentinel-sourcemaps)

The processor reads source maps from object storage (contract §4.4) and, with
`SYMBOLICATE_PLATFORMS` non-empty (the chart default), **refuses to start without
`S3_ACCESS_KEY` / `S3_SECRET_KEY`** — `log.Fatalf` before it subscribes, so the
pod CrashLoopBackOffs, never consumes an event, and the rollout stalls on the new
ReplicaSet while the old pods keep processing (contract §8, decision 32). No
Prometheus consumes Sentinel's rule group yet (decision 18), so **this render is
the check** — run it against the env's values before the tag is cut and confirm
BOTH `secretKeyRef`s are on the processor Deployment:

```bash
helm template sentinel deploy/helm/sentinel \
  -f deploy/helm/sentinel/values-huawei-<env>.yml \
  --show-only templates/processor.yaml \
  | grep -E -A1 'name: (S3_ACCESS_KEY|S3_SECRET_KEY|GOMEMLIMIT|SYMBOLICATE_|SOURCEMAP_)'
# Expected (the processor Deployment is the only Deployment in that template):
#   - name: S3_ACCESS_KEY
#     valueFrom: { secretKeyRef: { name: sentinel-sentinel-secrets, key: S3_ACCESS_KEY } }
#   - name: S3_SECRET_KEY
#     valueFrom: { secretKeyRef: { name: sentinel-sentinel-secrets, key: S3_SECRET_KEY } }
#   the six knobs SYMBOLICATE_PLATFORMS, SOURCEMAP_CACHE_BYTES, SOURCEMAP_NEGATIVE_TTL,
#   SOURCEMAP_POSITIVE_TTL, SYMBOLICATE_DEADLINE, SOURCEMAP_MAX_PARSE_BYTES (values.yaml,
#   config.processor.*), and GOMEMLIMIT "800MiB" — below the 1Gi memory limit (decision 35;
#   the three numbers are derived together, arithmetic beside processor.gomemlimit in values.yaml).
# Both secret keys exist in every env's sealedData (they are the dashboard's too). A render
# missing either secretKeyRef is a processor that will CrashLoopBackOff on this tag.
```

**Is symbolication working?** No alert says so (the `sentinel-sourcemaps` rule
group in `templates/prometheusrule.yaml` is dormant until a Prometheus loads it).
Read it by hand: on a processor pod's `/metrics`,
`sentinel_symbolication_events_total{outcome="none"}` climbing **while
`sourcemaps` has rows for that release** (`SELECT count(*) FROM sourcemaps WHERE
release_version = '<version>'`) means events for a mapped release are not being
symbolicated — check `sentinel_symbolication_frames_total` by `outcome`
(`fetch_error` / `map_error` name the cause; `no_column` means the producer sends
no column). `outcome="none"` with **no** rows is the expected state for a release
whose maps were never uploaded.

**`RESTARTS` climbing on a processor pod is now two signals, not one.** Read
`lastState.terminated.reason` on the pod before assuming memory:

- `Error`, last log line naming `S3_ACCESS_KEY` / `S3_SECRET_KEY` → the startup
  fatal above: the chart release is missing the secret refs (or the Secret the
  keys). Fix the chart/secret; nothing was consumed, nothing to replay.
- `OOMKilled` → the parsed maps outgrew `1Gi`. `GOMEMLIMIT` (`800MiB`) makes the
  runtime collect first, but the cache accounts **raw** bytes and holds parsed maps
  that are ~7.7× raw (§13 M2), which is why `SOURCEMAP_CACHE_BYTES` is 64 MiB in the
  chart, not the code's 128 MiB — the arithmetic is beside `processor.gomemlimit` in
  values.yaml. Levers, in order: `SOURCEMAP_MAX_PARSE_BYTES` (keep the biggest
  maps out of RSS), `SOURCEMAP_CACHE_BYTES` down, or the memory limit up with
  `GOMEMLIMIT` following it (contract §12, decisions 11, 35, 38). In-flight events
  are redelivered by NATS and a redelivery can land on a second issue (Known limit 6).

### Pre-deploy check before this tag (sentinel-occurrence-dedup-project)

`1725200000_add_error_occurrences_project_id_dedup.sql` adds
`error_occurrences.project_id`, backfills it from `issues.project_id` in batches,
and builds `uq_error_occurrences_project_event` — `UNIQUE (project_id, event_id)
WHERE project_id IS NOT NULL AND event_id IS NOT NULL` — **`CONCURRENTLY`**, the
first concurrent build in this repository, on the table every ingest writes to.
A unique build fails if the key is already violated, and a failed concurrent
build leaves an `INVALID` index behind, so the file's gate (c) `RAISE`s with the
group count before any index is touched (contract §4.2, §4.4). That gate must
never be the first place a duplicate is discovered: **run O1 by hand before the
tag is cut, and a non-zero count blocks the tag.** The mechanism that creates
groups (a JetStream redelivery or DLQ replay whose frames symbolicate differently,
`sentinel-sourcemaps` Known limit 6) is live on `v0.2.17` / `v0.2.18` and is fed
by the 72 h redelivery window and every DLQ drain, so the count decays the moment
it is taken: **O1 is re-run within the hour before each `argocd app sync`** — the
staging sync and the production sync each get their own run, not the one taken
before the tag (contract decision 18).

No registered rig reaches Sentinel's database. The venue is the in-cluster `psql`
pod, as for `sentinel-ingest-timestamp-clamp` (contract decision 10), staging
then production. Every number below is recorded in the feature's `status/be.md`
with its date.

**O1 — the duplicate count and the sizes.** Four reads in one `psql` window.
The first is contract §4.4's query, verbatim (the join form — `project_id` does
not exist on the table before this tag, so the project is reached through the
issue; after the tag O4 groups on the column instead):

```sql
SELECT i.project_id, o.event_id, count(*) AS rows, count(DISTINCT o.issue_id) AS issues,
       min(o.created_at) AS first, max(o.created_at) AS last
  FROM error_occurrences o JOIN issues i ON i.id = o.issue_id
 WHERE o.event_id IS NOT NULL
 GROUP BY 1, 2 HAVING count(*) > 1
 ORDER BY last DESC;
```

**Zero rows back is the only result that lets the sync proceed.** Any row is a
`(project_id, event_id)` group the gate will refuse: run O2, then O1 again until
it returns nothing.

```sql
-- The table's size, for the bloat headroom (contract §12): the backfill rewrites every
-- pre-release row once and the concurrent build scans the table twice, so the heap roughly
-- doubles until autovacuum reclaims it. Read the same number after the deploy to see it drain.
SELECT pg_total_relation_size('error_occurrences'),
       pg_size_pretty(pg_total_relation_size('error_occurrences'));

-- The server major: the backfill is one DO block with a COMMIT per batch, which is legal
-- outside a transaction block only on PostgreSQL >= 11 (contract decision 18). The RDS major
-- is recorded nowhere in this repo; below 11 the block fails on its first COMMIT and the
-- migrate Job never advances — stop and raise it before the tag.
SELECT version();

-- The database size, beside the free disk on the instance, so "the heap roughly doubles" is a
-- number against a number: the instance needs headroom of at least one more
-- pg_total_relation_size('error_occurrences') on top of pg_database_size for the minutes of the
-- backfill plus the build.
SELECT pg_database_size(current_database()),
       pg_size_pretty(pg_database_size(current_database()));
-- Free disk: the RDS instance's storage metric in the Huawei console (or `df -h` on the
-- volume for a self-hosted Postgres) — psql cannot read it. Record both together.
```

Recorded 2026-09-16 (contract decision 10; the baseline every later O1 is read
against):

| Environment | §4.4 groups | `error_occurrences` rows | `pg_total_relation_size` |
| --- | --- | --- | --- |
| staging | **0** | 3 (all with `event_id`) | 128 kB |
| production | **0** | 769,590 (all with `event_id`) | 1421 MB |

Indexes present on both: `error_occurrences_pkey`, `idx_error_occurrences_created_at`,
`idx_error_occurrences_issue_id`, `idx_error_occurrences_trace_id`,
`idx_occurrences_issue_created`, `idx_occurrences_issue_release`,
`uq_error_occurrences_issue_event`. `SELECT version()`, `pg_database_size` and the
free disk were not part of that window (decision 18 added them afterwards) and are
taken on the first O1 re-run before the staging sync.

**O2 — if O1 is non-zero: delete the later row of each group** (contract
decision 13, the OQ-4 (a) recipe). No migration in this feature deletes a row
(decision 4); this is the operator's statement, run with O1's number in front of
them. Keep the row with the earliest `created_at`, tie on `id`; delete every
other row of the group. Inside a transaction, so the row count can be checked
against O1 before it is committed — the expected count is the sum over O1's rows
of `rows - 1`:

```sql
BEGIN;
WITH ranked AS (
  SELECT o.id,
         row_number() OVER (PARTITION BY i.project_id, o.event_id
                            ORDER BY o.created_at, o.id) AS rn
    FROM error_occurrences o JOIN issues i ON i.id = o.issue_id
   WHERE o.event_id IS NOT NULL
)
DELETE FROM error_occurrences WHERE id IN (SELECT id FROM ranked WHERE rn > 1);
-- DELETE <n> — n must equal the sum of (rows - 1) over O1's output; ROLLBACK if it does not.
COMMIT;
```

What it leaves behind, and why that is accepted:

- **The orphaned issue is left to retention.** If the deleted row's issue now has
  no occurrence, do nothing to it: the hourly retention sweep's orphan rule
  (`apps/dashboard-web/src/lib/server/retention.ts`, `deletedOrphanedIssues`)
  removes it and cascades its side tables. It stays visible on `/` until the
  sweep runs — at most one hour.
- **Stated counter drift, bounded and not repaired.** The lifetime counters that
  the second delivery incremented are never decremented by retention: the
  *surviving* second issue's `issues.count`, its `issue_occurrence_buckets` hour,
  its affected-user / affected-tenant rollups and the release's
  `releases.occurrence_count` stay one too high per deleted row. Contract
  decision 13 accepts that drift (the numbers were already wrong for as long as
  the group existed) rather than the per-group five-table recompute it would take
  to remove it.

Then O1 again. The sync does not start until O1 returns zero rows.

**O3 — after the production roll completes: the post-roll sweep, in two steps
(contract decisions 12 and 15).** For the minutes of the processor roll, old-tag
pods keep inserting rows **with `project_id = NULL`** — they do not know the
column — and those rows are outside the new key until this sweep joins them to
it. The one-statement sweep is not safe on its own: a window row `(I₁, k, NULL)`
whose event was later redelivered cross-issue on a new-tag pod and stored as
`(I₂, k, P)` makes it raise `23505` on `uq_error_occurrences_project_event`, and a
`23505` mid-`UPDATE` rolls back every row it had set. Hence two steps, in this
order, once `kubectl -n sentinel-<env> rollout status deploy/sentinel-sentinel-processor`
reports the roll complete:

**Step 1 — the NULL-tolerant group query, then O2 on every group.** Contract
§4.4's query with the key read as `COALESCE(o.project_id, i.project_id)`, so a
window row (NULL column, project via the issue) and its cross-issue redelivery
(column set) land in the same group:

```sql
SELECT COALESCE(o.project_id, i.project_id) AS project_id, o.event_id, count(*) AS rows,
       count(DISTINCT o.issue_id) AS issues,
       min(o.created_at) AS first, max(o.created_at) AS last
  FROM error_occurrences o JOIN issues i ON i.id = o.issue_id
 WHERE o.event_id IS NOT NULL
 GROUP BY 1, 2 HAVING count(*) > 1
 ORDER BY last DESC;
```

Apply O2 to every group it returns — keep `min(created_at)`, tie on `id`, which
is the earlier NULL row; delete the later row; leave its issue to retention —
with O2's `DELETE` partitioned the same way:

```sql
BEGIN;
WITH ranked AS (
  SELECT o.id,
         row_number() OVER (PARTITION BY COALESCE(o.project_id, i.project_id), o.event_id
                            ORDER BY o.created_at, o.id) AS rn
    FROM error_occurrences o JOIN issues i ON i.id = o.issue_id
   WHERE o.event_id IS NOT NULL
)
DELETE FROM error_occurrences WHERE id IN (SELECT id FROM ranked WHERE rn > 1);
-- DELETE <n> — n must equal the sum of (rows - 1) over step 1's output; ROLLBACK if it does not.
COMMIT;
```

**Step 2 — the sweep, guarded so it can never violate the new index.** The
`NOT EXISTS` skips any window row whose `(project_id, event_id)` is already
stored under the column — a group step 1 did not see because it formed in
between. The guard reads the column only: two window rows of one event that are
*both* NULL (an old-tag pod's own cross-issue redelivery during the roll) pass
the guard together and would collide inside this one statement — they are step
1's group, which is why step 1 is never skipped and always runs first:

```sql
UPDATE error_occurrences o
   SET project_id = i.project_id
  FROM issues i
 WHERE i.id = o.issue_id
   AND o.project_id IS NULL
   AND NOT EXISTS (SELECT 1 FROM error_occurrences x
                    WHERE x.project_id = i.project_id AND x.event_id = o.event_id);
-- UPDATE <rows updated> — record it.
```

Then report the skipped count — the window rows the guard left NULL because
their key is already taken. It is O2's input: non-zero means step 1 has groups
to handle again (they formed between step 1 and step 2), so run step 1, then
step 2, until the skipped count is zero:

```sql
SELECT count(*) AS skipped
  FROM error_occurrences o JOIN issues i ON i.id = o.issue_id
 WHERE o.project_id IS NULL AND o.event_id IS NOT NULL
   AND EXISTS (SELECT 1 FROM error_occurrences x
                WHERE x.project_id = i.project_id AND x.event_id = o.event_id);
```

Rows with `event_id IS NULL` may also be left with `project_id = NULL` by the
guard's join; they are outside both keys (contract decision 7) and the sweep
sets them regardless, since `NOT EXISTS` is trivially true for them. Record
`rows updated` and the final skipped count (zero) in `status/be.md`.

The sweep is hygiene, not the guarantee: a same-issue redelivery of a window row
is already the duplicate path on the new processor (`StoreEvent` treats a
`23505` named `uq_error_occurrences_issue_event` as `stored = false`, decision
12's OQ-3 (a)), and this sweep only extends the new key to those rows'
*cross-issue* redeliveries. Between the roll and the sweep, a cross-issue
redelivery of a window row stores a second row, exactly as every redelivery did
before this tag — the minutes of exposure are why step 1 exists.

**O4 — ≥ 72 h after the production sync: the re-check.** The JetStream window
has turned over by then, so any group with `last` after the deploy is one the
new key failed to catch — contract §13 M3's "zero groups created after the
deploy", the hypothesis' clause (iii). Same query as O1, now on the **column**,
because after the backfill and O3 every row with an `event_id` carries its
`project_id` and the column, not the issue join, is what the index enforces
(contract decision 27: `moveIssueToProject` rewrites `issues.project_id` and
touches no occurrence, so the join form can differ from the key after this tag):

```sql
SELECT o.project_id, o.event_id, count(*) AS rows, count(DISTINCT o.issue_id) AS issues,
       min(o.created_at) AS first, max(o.created_at) AS last
  FROM error_occurrences o
 WHERE o.project_id IS NOT NULL AND o.event_id IS NOT NULL
 GROUP BY o.project_id, o.event_id HAVING count(*) > 1
 ORDER BY last DESC;
```

Expected: zero rows, and in particular **no row with `last` later than the
production sync**. Record the number and the date in `status/be.md`. Do not run
it earlier than 72 h and read a zero as the proof: before the window has turned
over, a zero only says nothing was redelivered yet. A row with `last` after the
deploy is a `23505`-shaped hole in the key — read the processor's duplicate log
lines for that `event_id` (`matched_key`, below) before touching the row.

### GeoIP refresh (sentinel-event-enrichment)

The ingestor fills `user.geo` from a GeoLite2-City database it loads from this env's bucket,
`geoip/GeoLite2-City.mmdb` (`geoip.storage.*`). Ingestor pods only READ that object — they never
contact MaxMind, so pod starts, rollouts, autoscaling and crash loops cost no MaxMind download.

- **What the job does.** `sentinel-sentinel-geoip-refresh` (the ingestor image, `args: ["geoip-refresh"]`)
  runs Wed + Sat 06:00 UTC. It reads the stored object's `maxmind-md5` metadata, asks MaxMind whether
  GeoLite2-City changed (a metadata check that does not count against the download limit) and, only
  when it did, downloads it once, checks its MD5, opens and verifies it, checks it is `GeoLite2-City`,
  and uploads it (one atomic object replacement). `backoffLimit: 0` + `concurrencyPolicy: Forbid`:
  at most 2 downloads a week per env against the free account's 30 per 24 h.
- **Manual run** (costs at most one download):

  ```bash
  kubectl -n sentinel-<env> create job --from=cronjob/sentinel-sentinel-geoip-refresh geoip-refresh-manual-$(date +%s)
  ```

- **Reading the job log** (`kubectl -n sentinel-<env> logs job/<name>`):
  - `geoip-refresh: uploaded` (md5, build_time, bytes): a newer database is in the bucket.
  - `geoip-refresh: up to date` (md5): nothing to do; no download happened.
  - `geoip-refresh: MAXMIND_ACCOUNT_ID / MAXMIND_LICENSE_KEY not set; MaxMind not contacted`: the
    keys are not sealed yet (TASK-07, §1).
  - any other `geoip-refresh:` error: nothing was uploaded; the stored object stays the last good one.
- **Confirming the pods loaded it:** every ingestor replica logs `geoip.state … to=ready` with
  `build_time` at start, and `geoip.reloaded` (with `build_time`, `etag`) when a newer object is
  picked up, within `geoip.pollInterval` (1h) and with no restart:

  ```bash
  kubectl -n sentinel-<env> logs deploy/sentinel-sentinel-ingestor | grep -E 'geoip\.(state|reloaded)'
  ```

- **Moving from OBS to AWS S3** is a values-and-secret change, no code: set `geoip.storage.endpoint`
  (`https://s3.<region>.amazonaws.com`), `region`, `bucket`, `forcePathStyle: ""`; seal a read-only key
  scoped to `geoip/*` as `GEOIP_S3_ACCESS_KEY` / `GEOIP_S3_SECRET_KEY` and point
  `geoip.storage.accessKeySecretKey` / `secretKeySecretKey` at it; point
  `geoip.refreshCron.accessKeySecretKey` / `secretKeySecretKey` at a key that can write that prefix.
  Until then both workloads use the existing read-write `S3_ACCESS_KEY` / `S3_SECRET_KEY`, an accepted
  risk (contract decision 8).

### Tag facets backfill (sentinel-issue-tag-facets)

The issue page's tag distribution panel and `/search`'s `key:value` filter read `issue_tag_values`, a
per-issue rollup that triggers on `error_occurrences` keep exact from the moment the migration
(`*_add_issue_tag_values.sql`) commits: every occurrence stored since is counted, by every pod version.
Occurrences stored **before** it are counted once, by the backfill below. Until it finishes, the panel
and tag search see a partial rollup.

**Deploy date (fill in at the deploy, per env):** staging `____-__-__`, production `____-__-__`.
(P5's retention window starts from the first retention run that deletes, spec §10.3.)

- **What it is.** `sentinel-sentinel-tag-facets-backfill`, a CronJob that **never runs by itself**:
  `suspend: true` is a literal in the template, the schedule is 31 February, and it carries no ArgoCD or
  Helm hook annotation, so ArgoCD syncs it and never waits on it. The processor image,
  `args: ["backfill-tag-facets", "-batch", "200", "-pause", "200ms", "-duty", "0.25"]`
  (`tagFacetsBackfill.*`). It is resumable: a run cut short (a deleted Job, the 6 h
  `activeDeadlineSeconds`) continues from its committed cursor on the next Job, and a finished run is a
  no-op (`already_done`).
- **Run it once per environment, staging first**, right after the sync that brings the migration (P1
  passed). First check no run is live — never create a Job while one shows `Running`:

  ```bash
  kubectl -n sentinel-<env> get jobs | grep tag-facets-backfill
  kubectl -n sentinel-<env> create job --from=cronjob/sentinel-sentinel-tag-facets-backfill tag-facets-backfill-$(date +%s)
  ```

  A second Job created by mistake exits **2** with `tag_facets.backfill.already_running` (the holder's
  `pid`) and ends `Failed`; it changes nothing.
- **Reading its log** (`kubectl -n sentinel-<env> logs -f job/tag-facets-backfill-<epoch>`), JSON lines
  with `event`:
  - `tag_facets.backfill.start` — `batch`, `pause`, `duty`, and the state row (`cursor_id`,
    `rows_counted`, `done`).
  - `tag_facets.backfill.progress` — at most every 10 s: `batches`, `rows_counted`, `cursor_id`,
    `progress_pct` (the cursor's position in the uuid space: an unbiased estimate of the fraction done),
    `rows_per_sec`, `batch_ms_p50`, `batch_ms_max`. Record `batch_ms_*` for P2.
  - `tag_facets.backfill.retry` — a batch rolled back on `40001` / `40P01` / `55P03` (a lock wait over
    5 s); it retries after 1 s, up to 10 times in a row, then the Job fails (re-create it: it resumes).
  - `tag_facets.backfill.done` — `rows_counted` (final total), `batches`, `elapsed`. ~1.4 h at 4.4 M rows,
    `N / 900` s for `N` rows.
  - `tag_facets.backfill.already_done` — nothing to do.
  - `tag_facets.backfill.failed` — the error; nothing half-done is left (each batch is one transaction).
- **P2, the ingest check while it runs.** Every batch locks the `issues` rows it touches (every hot
  issue) for its duration, capped at 25 % of the time. Sample the processor consumer's lag every 30 s for
  the length of the run; **pass: `num_pending` ≤ 100 in every sample** (`num_ack_pending` ≤ 20):

  Both Huawei overlays run `dependencies.nats.enabled: false` and reach a shared cluster NATS by DSN
  (`nats.url`, sealed as `NATS_URL`) — no `svc/*-nats` renders in this chart to port-forward. Every app
  pod already has `NATS_URL` and network reachability to it (production traffic depends on it), so sample
  from inside one instead of assuming a Service name: the monitoring port (8222) is the same host as the
  client port in `NATS_URL`.

  This assumes the shared cluster NATS actually serves monitoring on 8222 at that host, and that the
  processor pod's egress policy allows reaching it — unverified by review. Confirm it once per cluster
  before relying on it during a real backfill: `kubectl -n sentinel-<env> exec deploy/<release>-sentinel-processor
  -- sh -c 'host=$(echo "$NATS_URL" | sed -E "s#nats://([^@]*@)?([^:/]+).*#\2#"); wget -qO- "http://$host:8222/varz" | head -c 200'`
  should return NATS server JSON (a `server_id` field), not a timeout or connection refused. There is no
  in-repo fallback today (the processor exports no NATS consumer-lag metric of its own): if the monitoring
  port is unreachable, P2 cannot be sampled as written and the QA seat should escalate rather than
  approximate it.

  ```bash
  kubectl -n sentinel-<env> exec deploy/<release>-sentinel-processor -- sh -c '
    host=$(echo "$NATS_URL" | sed -E "s#nats://([^@]*@)?([^:/]+).*#\2#")
    while sleep 30; do wget -qO- "http://$host:8222/jsz?consumers=true"; echo; done' \
    | jq '.account_details[].stream_detail[] | select(.name=="ERROR_EVENTS") | .consumer_detail[]
          | select(.name=="processor-consumer") | {num_pending, num_ack_pending}'
  ```

  If it is exceeded, **delete the Job** (the cursor is committed; nothing is lost) and create a new one
  with a larger `-pause`, a smaller `-duty` or a smaller `-batch`, by editing the args of a dry-run copy:

  ```bash
  kubectl -n sentinel-<env> delete job tag-facets-backfill-<epoch>
  kubectl -n sentinel-<env> create job --from=cronjob/sentinel-sentinel-tag-facets-backfill \
    tag-facets-backfill-$(date +%s) --dry-run=client -o json \
    | jq '.spec.template.spec.containers[0].args = ["backfill-tag-facets","-batch","200","-pause","500ms","-duty","0.1"]' \
    | kubectl -n sentinel-<env> apply -f -
  ```

  Record the `sentinel_process_duration_seconds` p99 over the
  run window against the day before.
- **P4, the reconciliation, after `done`.** For the 20 largest issues, in one `REPEATABLE READ`
  transaction, spec §10.2's check query (also `tests/integration/tag_facets_db_test.go`
  `tagFacetsCheckSQL`) with `$1` = those ids must return five zeros:

  ```sql
  BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY;
  -- $1 := ARRAY(SELECT id FROM issues ORDER BY count DESC LIMIT 20)
  -- then the check query; columns named_mismatch, key_total_mismatch, value_nowhere, zero_rows, over_cap
  COMMIT;
  ```

  Any non-zero: rebuild that issue (below) and file a finding with the query's output.
- **`issue_tag_values_rebuild('<issue id>')`** recomputes one issue's rollup exactly from its counted
  occurrences. It holds that issue's `issues` row lock for ~3-6 µs per occurrence (seconds for a
  million-occurrence issue), so that issue's deliveries wait: run it **off-peak**, one issue at a time,
  only for the issues P4 names and for every `operatorOnlyIssueIds` entry of
  `retention.tag_rollup_rebuild_pending` or `retention.tag_rollup_rebuild_operator_only` (the retention
  route will not retry those). Start it right after a `retention_cron.completed` line (the next run is an
  hour away).

  ```sql
  SELECT issue_tag_values_rebuild('<issue id>');   -- returns the pairs counted
  ```

- **After P2 passed** an overlay may set `tagFacetsBackfill.enabled: false` in a later values change,
  which removes the CronJob and its Jobs. Optional.
- **Never disable the `trg_error_occurrences_tag_rollup_*` triggers** (not for a migration either: batch
  an `UPDATE` of `error_occurrences` instead, it costs ~70 µs a row in the update trigger). A disabled
  trigger lets the rollup drift silently, and the backfill refuses to run while one is missing.

### Retention keep-up and route outcomes (sentinel-issue-tag-facets)

`POST /api/cron/retention` (the hourly `sentinel-sentinel-retention` CronJob) now deletes aged occurrences in
**paced, issue-locked batches**: 200 rows at a time, each batch locking its issues' rows first (in id order),
bounded lock waits (250 ms per lock, 1 s for the lock phase, 1 s for the `DELETE`), a 50 % duty cycle, and a
120 s budget per run. The delete trigger keeps `issue_tag_values` exact in the same transaction. One run at a
time: a run holds the advisory lock `sentinel.retention.route` on its own connection
(`application_name = 'sentinel.retention.run'`), runs every statement on it, and cannot outlive its 170 s
deadline. curl's `--max-time` is 180 s (`retentionCron.timeoutSeconds`). Every
`retention_cron.completed` line carries `outcome`, `routeMs`, `deletedOccurrences`, `occurrenceStopped`,
`occurrenceBatches`, `occurrenceStepMs`, `occurrenceBatchSize`, `occurrenceHeldP95Ms`, `occurrenceHeldMaxMs`,
`occurrenceBacklogSeconds`, `oldestOccurrenceAt`, `tagRollupDriftEvents`, `tagRollupRebuildsPending` and
`rebuildStepMs`.

**Keep-up.** The step keeps up with a margin of 2 when

> floor(119,000 / c̄) × 200 ≥ 2 × A

where c̄ = `occurrenceStepMs / occurrenceBatches` of a run with ≥ 20 batches, and A is the peak hourly aging
rate (34,481 measured 2026-09-25; re-read it before judging a run):

```sql
BEGIN READ ONLY;
SET LOCAL statement_timeout = '120s';
SELECT date_trunc('hour', created_at AT TIME ZONE 'UTC') AS hour, count(*)
  FROM error_occurrences WHERE created_at >= now() - interval '12 days' GROUP BY 1 ORDER BY 2 DESC LIMIT 5;
COMMIT;
```

At the 150 ms p95 hold ceiling, c̄ = 325 ms → 73,200 rows a run = 2.12 × A; measured warm, c̄ ≈ 100 ms →
~237,000. What each warning means:

| Log line | Meaning | Act |
|---|---|---|
| `retention.occurrence_backlog` (warn) | the oldest surviving occurrence is > 2 h behind the cutoff | once, right after a DLQ replay of old messages: expected (the next run deletes them). Two runs in a row: a keep-up failure, apply the levers below |
| `retention.occurrence_hold_p95_over_ceiling` (warn) | a run with ≥ 20 batches held its batches > 150 ms at p95: production's per-row cost is above what the margin was derived for | the levers below |
| `retention.occurrence_hold_over_ceiling` (warn) | one or more batches held > 150 ms | informational alone (one slow batch among hundreds); act only on the p95 line or a failing inequality |
| `retention.occurrence_batch_reduced` (warn) | a batch's `DELETE` ran past 1 s; the run went on at half the batch (`to` is the size reached) | the levers below |
| `retention.occurrence_delete_timeout` (warn) | even a 25-row `DELETE` ran past 1 s and the step stopped | the levers below, and look for what made the rows that slow: an inline drift rebuild, a lock in the FK cascade |
| `retention.occurrence_lock_timeout` (warn) | a batch waited > 250 ms for a lock (`phase`: `lock` = an `issues` row, `delete` = a row the `DELETE` or its cascade needs) and the step stopped; the next run continues from the oldest row | if it recurs, find the blocker (query below) |
| `retention.occurrence_budget_exhausted` (warn) | the 120 s budget ran out with rows left | a backlog is draining; if it persists, the levers below |

**The levers, in order — never suspend the retention CronJob for any of these** (suspending stops every
deletion AND the invitation, attachment, stale-claim and idempotency reapers on the route, which makes the
backlog strictly worse): (1) add runs: `retentionCron.schedule: "*/30 * * * *"` (a values change, no
release; each halving of the interval doubles the capacity); (2) a smaller `RETENTION_OCCURRENCE_BATCH` in a
release, **only together with** (1), because a smaller batch alone shortens each wait but lowers capacity.

**Route outcomes** (`outcome` on `retention_cron.completed`, and `sentinel_retention_runs_total{outcome}`):

- `completed` — the normal case.
- `behind` (the Job is `Complete`, the counter's `behind` rises; three in a row fire
  `SentinelRetentionNotCompleting`) — occurrence deletion stopped early (`occurrenceStopped` not `drained`)
  with nothing deleted, or stopped early with the backlog over 2 h. A `drained` run is never `behind`; its
  `retention.occurrence_backlog` line alone means an event was stored already aged, which the next run
  deletes. Read the run's `retention.occurrence_lock_timeout`, `retention.occurrence_delete_timeout` or
  `retention.occurrence_backlog` line; for a lock timeout find the blocker (below), otherwise apply the levers.
- `retention_cron.skipped_concurrent` (200, `skipped`) — a second POST met a live run (a `backoffLimit`
  re-POST or a manual Job) and did nothing.
- **`retention_cron.deadline_exceeded`** (a Failed Job, 500) — a run reached its 170 s deadline and stopped.
  It holds nothing afterwards, so there is nothing of it to kill. The usual cause is a session holding rows a
  step needs; find it:

  ```sql
  SELECT pid, application_name, client_addr, state, xact_start, left(query, 200)
    FROM pg_stat_activity
   WHERE state LIKE 'idle in transaction%' AND xact_start < now() - interval '1 minute';
  ```

  End it with `SELECT pg_terminate_backend(<pid>)`, **unless it belongs to a dashboard pod** (`client_addr`
  is a dashboard pod's): then restart that pod instead, because terminating a dashboard backend
  mid-transaction crashes that process anyway (a postgres-js 3.4.9 defect, spec §12). The next hourly run
  then completes.
- **`retention_cron.lock_stale`** (a Failed Job, 503) — the route lock has been held past 4 min, longer than
  any live run can hold it. The holder is a dead or wedged dashboard process, or a session that is not a run
  (`holderApp` other than `sentinel.retention.run`). Record it, then end it:

  ```sql
  SELECT pid, application_name, client_addr, backend_start, state, wait_event_type, left(query, 200)
    FROM pg_stat_activity WHERE pid = <holderPid>;
  SELECT pg_terminate_backend(<holderPid>);
  ```

  That ends the lock and every statement of that run together (the run uses no other session). If the pod
  at that `client_addr` is still running but unresponsive, restart it as well. The next hourly run proceeds
  from the oldest row.
- `retention_cron.connection_lost` (500) — a run's session ended under it (a terminate, a database
  restart). The run stopped without reconnecting; the next run continues.

**Operator rebuilds and retention.** Start an `issue_tag_values_rebuild` right after a
`retention_cron.completed` line (the next run is an hour away). A run that meets it anyway ends its
occurrence step within 1 s (`retention.occurrence_lock_timeout`) and the next run continues, so the CronJob is
not suspended for it. `retention.tag_rollup_drift` (warn) reports drift the triggers repaired or deferred
since the previous run (`events`, `issues`, `contributions`, `sources`, `issueIds`): a recurring cause is a
bug to fix. `retention.tag_rollup_rebuild_pending` names the issues the route will still try (`issueIds`)
and those handed to the operator (`operatorOnlyIssueIds`: too large for its 5 s budget, or timed out once);
rebuild each of the latter with `SELECT issue_tag_values_rebuild('<id>')`, off-peak (see "Tag facets
backfill" above). The route never retries them.

**Alerts.** The `sentinel-retention` rule group (`SentinelRetentionLockStale`, `SentinelRetentionRunFailing`,
`SentinelRetentionNotCompleting`) is dormant until Sentinel's rule groups are loaded into a Prometheus
(`prometheusRule.enabled`, false in production). Until then, check by hand after a deploy and whenever
retention is in doubt: `sentinel_retention_runs_total` on each dashboard replica's `/api/metrics`
(`completed` rising hourly, the failure outcomes flat) and

```bash
kubectl -n sentinel-<env> get jobs -l app.kubernetes.io/component=retention-cron   # no Failed
```

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

### Rolling back sentinel-occurrence-dedup-project — processor tag FIRST, `Down` second and only if the column must go

`1725200000_add_error_occurrences_project_id_dedup.sql` is one-way safe in the
chart's own order: **an old-tag processor against the migrated schema is fine.**
It writes no `project_id` (the column is nullable with no default) and uses
`uq_error_occurrences_issue_event` as its `ON CONFLICT` arbiter — which this
release deliberately keeps (contract decision 5) for exactly this rollback.
Nothing fails; the rolled-back pods simply write rows outside the new key, and
O3's sweep joins them to it on the next roll-forward.

**Step 1 — the processor image roll-back, to `v0.2.18`** (the previous tag,
contract decision 26). The ingestor did not change on this tag; the dashboard's
only change is the Drizzle mirror's declaration of the column. **In this step —
an image roll-back with the migrated schema left in place — neither the ingestor
nor the dashboard needs a roll-back**: a dashboard on either tag is correct
against the *migrated* schema (the old tag never names the column, the new tag
reads a column that exists):

```bash
argocd app set sentinel-<env> --parameter images.processor.tag=v0.2.18
argocd app sync sentinel-<env>
kubectl --context hwc-daya-cce-production -n sentinel-<env> rollout status deploy/sentinel-sentinel-processor
```

That is the whole rollback. Stop here.

**Step 2 — `Down`, only if the column itself must go, and never before BOTH
`images.processor.tag=v0.2.18` AND `images.dashboard.tag=v0.2.18` have rolled
out.** `Down` drops `uq_error_occurrences_project_event` (concurrently) and then
`error_occurrences.project_id`, and *both* new-tag services read that column:

- A processor on this feature's tag against a database that ran `Down` gets
  **`42703` (undefined column `project_id`) on every `INSERT`** — class 42, which
  `classifyStoreError` leaves *retryable*, so every event burns `MaxDeliver` (7)
  and is dead-lettered until the tag is rolled: an ingest outage, not a broken
  chart (contract §7, §8).
- A dashboard on this feature's tag is **not** correct against the Down'd schema,
  because step 1's "either tag" only held while the column existed.
  `getOccurrencesByIssueId` (`apps/dashboard-web/src/lib/server/queries/issue-queries.ts:200`)
  is a bare `db.select().from(errorOccurrences)`, which Drizzle expands to every
  column the mirror declares — including `project_id` — so after `Down` every
  `/issues/[id]` page load (`routes/issues/[id]/+page.server.ts:86`) fails with
  **`42703` `column "project_id" does not exist`** until the dashboard image is
  rolled back or the file is re-applied.

That is why `Down` is not part of a rollback and why, if it is ever run, the
running pods of BOTH Deployments must already be on `v0.2.18` — the general rule
at the top of §3 (`down` is never run with new-tag pods attached) applies to the
dashboard exactly as it does to the processor:

```bash
# Precondition: BOTH rollouts complete on v0.2.18 — step 1's processor roll, plus the dashboard:
argocd app set sentinel-<env> --parameter images.dashboard.tag=v0.2.18
argocd app sync sentinel-<env>
kubectl --context hwc-daya-cce-production -n sentinel-<env> rollout status deploy/sentinel-sentinel-processor
kubectl --context hwc-daya-cce-production -n sentinel-<env> rollout status deploy/sentinel-sentinel-dashboard
# Only after BOTH rollout status commands report the roll complete, from the migrate image's shell:
migrate status -target=processor     # `down` rolls the ledger's LATEST version back: confirm it is 1725200000
migrate down -target=processor
# then the same for the other two ledgers, which replayed the file (hazard sentinel_goose_target):
migrate down -target=ingestor
migrate down -target=dashboard
```

`Down` also discards the backfill: rolling forward again re-runs the whole file
(the column, the 769,590-row backfill, the gate, the build) — the release note's
durations apply again in full. A rolled-back processor's rows carry NULL
`project_id`, which the roll-forward's backfill fills from the issue (the block
is `WHERE project_id IS NULL`, idempotent and resumable).

### Rolling out sentinel-occurrence-dedup-project (release note)

The occurrence dedup key widens from `(issue_id, event_id)` to
`(project_id, event_id)`: a redelivery of an already-stored event is now a no-op
whatever issue its frames resolve to, which closes `sentinel-sourcemaps` Known
limit 6 (a redelivery whose symbolication differs from the first delivery's
"lands on a second issue"). No HTTP shape, no queue subject, no env var and no
chart value changes; the only externally visible changes are one nullable
column, one unique index, and the meaning of one metric label (below).

Rollout order is the chart's existing one and it is load-bearing (contract §8):
**`db-migrate` runs in wave 1, before `processor` rolls in wave 2**, so the column
and the index exist before the first new-tag processor pod starts — a new-tag
processor without them fails every insert (`42703` / `42P10`, retried then
dead-lettered). The reverse — the migrated database under old-tag pods — is
correct (the rollback entry above). There is no feature flag: the key is a schema
fact.

**O1 within the hour before this sync** (§2 above): the gate refuses a non-zero
duplicate count by name, after the backfill has already run — the migrate Job
fails, no pod rolls, and the operator is looking at a number O1 would have
shown them before the tag.

- **What the migrate log shows.** `>> migrating target=processor`, then goose's
  one line for the file — `OK   1725200000_add_error_occurrences_project_id_dedup.sql (<duration>)`
  — then `>> migrating target=ingestor` and `>> migrating target=dashboard`, each
  replaying the same file in seconds and changing nothing (the column exists,
  the backfill block exits by name because the index exists, the gate finds no
  group on the column, the index is valid and `IF NOT EXISTS` skips the build).
  It is **not** true that the backfill "finds no NULL row" on those replays:
  old-tag pods insert rows with `project_id = NULL` for the whole of wave 1
  (§13 M2 counted 4,124 of them below ledger 1's cursor at 1,010,000 rows), and
  the second and third ledgers meet them minutes after the first ledger built
  the index. Those rows are the rollout window's and O3's (contract decisions
  12, 15); the backfill deliberately does not touch them (block 3 below).
  Goose prints per file, not per block; the blocks are visible by name only
  when one **fails**. In order, they are:
  1. the session guards — `SET statement_timeout = '15min'`, `SET lock_timeout = '5s'`
     (decision 16: the SETs are session-scoped under `NO TRANSACTION`);
  2. **(a)** `ALTER TABLE error_occurrences ADD COLUMN IF NOT EXISTS project_id UUID` —
     catalog-only, milliseconds under `ACCESS EXCLUSIVE`; an ingest holding the
     table makes it fail with `canceling statement due to lock timeout` — re-sync;
  3. **(b)** the backfill `DO` block — `project_id := issues.project_id` via
     `issue_id`, `WHERE project_id IS NULL`, 10,000 rows per batch by `id` range,
     one `COMMIT` per batch (decision 20). **It runs only while no index of the
     key exists** (`to_regclass` on `uq_error_occurrences_project_event` and on
     block (d)'s `…_invalid` name both NULL) and otherwise exits before its first
     batch with the notice `1725200000 backfill (b): uq_error_occurrences_project_event
     exists -- the backfill is skipped …` — the second and third ledgers, and any
     re-run of the first after its build. Its purpose is the pre-release rows
     before the build; a row still NULL once the index exists was inserted by an
     old-tag pod during the window, and the unguarded `UPDATE` over such a row
     is decision 15's hazard (a cross-issue redelivery of that event already
     stored under the key → `23505` on `uq_error_occurrences_project_event` from
     inside the `DO` block, on every retry, no pod rolled). O3's two-step sweep
     is the guarded form and owns those rows. A failure here on the first ledger
     is `canceling statement due to statement timeout` naming the `DO $$` —
     every committed batch stays and the re-run continues from the remaining
     NULLs;
  4. **(c)** the duplicate gate `DO` block — fails as
     `1725200000 gate (c): N (project_id, event_id) group(s) in error_occurrences have more than one row …`
     (run O2, re-sync), or as
     `1725200000 gate (c): statement_timeout is … in this session` if the session
     lost block 1's `SET` (decision 16 — raise it, do not re-sync blindly);
  5. **(d)** drop-if-invalid — `DROP INDEX CONCURRENTLY IF EXISTS uq_error_occurrences_project_event_invalid`,
     the rename `DO` block (renames the index only when `pg_index.indisvalid = false`),
     the same `DROP` again;
  6. `SET lock_timeout = 0` — so the build is never failed for merely waiting on a
     slow `StoreEvent` (decision 16);
  7. `CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_error_occurrences_project_event` —
     `SHARE UPDATE EXCLUSIVE`; inserts continue. A violation the gate could not
     see (a group formed between (c) and the scan) fails it with `23505` and
     leaves an `INVALID` index behind;
  8. **(e)** the `indisvalid` check `DO` block — fails as
     `1725200000 check (e): uq_error_occurrences_project_event exists but pg_index.indisvalid = false …`
     (the next run's (d) drops it and rebuilds; O2 first if (c) then names a group);
  9. `RESET lock_timeout; RESET statement_timeout`.

  Every block before a failure stays applied and the ledger stays unadvanced;
  the next sync re-runs the file top to bottom (contract §4.2). The failing Job's
  log: `kubectl -n sentinel-<env> logs job/sentinel-sentinel-migrate`.
- **Durations at 769,590 rows, scaled from contract §13 M2** (taken on the local
  compose stack as the labelled fallback, 2026-09-16, postgres:15, at 1,010,000
  rows with the `v0.2.18` processor consuming `tests/load` at its default rate
  throughout; `status/be.md` has the run). Measured: the backfill `DO` block
  **281.3 s** (102 batches; the per-batch UPDATE 64 / 549 / 2,508 ms min / mean /
  max, 56.0 s in total — the rest is the batch-bound `SELECT`, which grew from
  1.2 s to 4.0 s per batch); the concurrent build **3.2 s**; the gate 1.7 s; the
  `ALTER` 13 ms; the whole `cmd/migrate up` **286.3 s**; `ingest_naks_during_build`
  **0** (no retry, no dead letter; inserts continued at reduced throughput, 32.5 →
  16.9 events/s during the backfill); `indisvalid = true` after the run. At
  769,590 rows (×0.76): **the backfill ≈ 170–215 s** (77 batches; the lower
  figure if the `SELECT`'s growth scales with the square of the row count, the
  higher if linearly), **the build ≈ 2.5 s**, the file ≈ 3–4 min, the three
  ledgers together ≈ 4–5 min. Against the three caps: the backfill block runs
  well inside its **`statement_timeout = '15min'`** (≈ 4× headroom); the migrate
  Job's **`backoffLimit: 3`** gives four attempts and each attempt resumes the
  backfill from its committed batches, so even one timed-out attempt does not
  restart the rewrite; the release workflow's **deploy job is capped at
  30 minutes** with `argocd app sync` itself un-timed (decision 20). An RDS
  instance is not the compose stack's disk: read the number as an order of
  magnitude, and if the production `OK` line reports more than ~10 min, the
  `SELECT`-growth note in `status/be.md` is where to look before the next tag.
- **The bloat window is the backfill plus the build.** The backfill rewrites
  every pre-release row once, so the heap roughly doubles (production: one more
  `pg_total_relation_size('error_occurrences')`, 1421 MB on 2026-09-16) until
  autovacuum reclaims it; `pg_stat_user_tables.n_dead_tup` for `error_occurrences`
  spikes then drains. O1's `pg_database_size` and free disk are the headroom
  check (contract §12).
- **A cancelled deploy job is checked with `argocd app get`, never re-synced.**
  The release workflow's deploy job can be cancelled or time out (its own 30-minute
  cap; the `wait_timeout: 600` it hands the reusable deploy workflow) while the
  sync it started is still running wave 1 in ArgoCD — the migrate Job does not
  stop because the GitHub job did. Read the operation with
  `argocd app get sentinel-<env>` (operation state, the `sentinel-sentinel-migrate`
  hook's status, then `kubectl -n sentinel-<env> get job sentinel-sentinel-migrate`)
  and let it finish; a second `argocd app sync` on top of a running one is at
  best refused and at worst — the hook's `BeforeHookCreation` delete policy —
  kills the running migrate Job mid-backfill and starts a fresh one (resumable,
  but the batches restart). Re-sync only once the operation has ended in failure.
- **The `RollingUpdate` minutes — processor.** The processor Deployment has no
  `strategy:` and rolls over `replicaCount: 2`; for the minutes the old-tag pod is
  still draining it writes rows with `project_id = NULL`, outside the new key.
  A same-issue redelivery of one of those rows on a new pod is still a clean
  duplicate (`StoreEvent` tolerates a `23505` named
  `uq_error_occurrences_issue_event`, decision 12); only their cross-issue
  redeliveries are unprotected until **O3's two-step sweep** (§2 above) joins them
  to the key. Run O3 as soon as `rollout status` reports the roll complete.
- **`outcome="duplicate"` on `sentinel_process_events_total` now also counts
  cross-issue redeliveries.** Its value is unchanged and the invariant
  `stored + duplicate + retried + deadlettered == deliveries` still holds
  (contract decision 8); its meaning widens from "same-issue redelivery" to
  "already stored under `(project_id, event_id)`". A Grafana panel that read it as
  the former reads the latter now. Expect it to rise after this tag: the
  redeliveries that used to open phantom issues are counted here instead of as
  `stored`.
- **The duplicate log line is key-aware.** `Duplicate event skipped` now carries
  `event_id`, `project_id`, the `issue_id` this delivery resolved to, and
  `matched_key`, which is *the constraint that refused the row* and has exactly
  two values: **`project`** — `uq_error_occurrences_project_event`, the arbiter,
  the row was already stored under this project on this issue or any other; and
  **`issue`** — `uq_error_occurrences_issue_event`, tolerated by name, the stored
  row predates the column (`project_id IS NULL`, a pre-backfill or rollout-window
  row) and this delivery resolved to the same issue. Same-issue vs cross-issue is
  **not** what `matched_key` says (decision 19); the M1 and O4 counts are the
  proofs of the cross-issue case.
- **`uq_error_occurrences_issue_event` is kept this release** (decision 5) —
  two unique indexes are maintained per insert until a later release's migration
  drops the old one, once no pod on a tag that names it as arbiter can be
  running. Do not drop it by hand: an old-tag pod (the rollback above) uses it as
  its `ON CONFLICT` arbiter and gets `42P10` on every insert without it.
- **O4 at ≥ 72 h** (§2 above) is the number that says the hypothesis held: zero
  `(project_id, event_id)` groups with `last` after the production sync.

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
| geoip-refresh Job fails at start | MaxMind keys not sealed (TASK-07) | the step-1 log line (`MAXMIND_ACCOUNT_ID / MAXMIND_LICENSE_KEY not set`) |
| Ingestor logs `geoip.state to=unavailable reason=not_found` | the refresh job has not run yet | run the job manually (§2 "GeoIP refresh") |
| Ingestor logs `geoip.state to=stale` | the refresh job has been failing for ~30 days | the job history: `kubectl -n sentinel-<env> get jobs \| grep geoip-refresh` |
| `{{auto}}` events posted straight to the ingestor (not through the tunnel) are stored with an empty IP | Expected on Huawei: no forwarding header is believed and the ingestor's peer is infrastructure, which is never stored (contract decision 11, spec §3.6) | Not a fault. Send the real IP in `user.ip_address`, or send browser events through the sdk-js ≥ 0.2.0 tunnel. Do **not** add ranges to `trustedProxyCidrs` to "fix" it. |
| Browser events from web carry no IP | web still runs sdk-js < 0.2.0, whose tunnel writes no IP (contract decision 5) | web's `@sentinel/sdk-js` version |
| A stored IP is a private, `100.64.0.0/10` or `100.125.x.x` address | It came from a body, which is stored as sent. The ingestor never derives such an address (spec §3.5) | The sender: an SDK that set it, or a tunnel whose `trustedProxyHops` does not match its proxy count |
| Tunnelled browser events (web on sdk-js ≥ 0.2.0) are stored with an IP the browser chose, e.g. P5(b)'s `6.6.6.6` | web's public ELB listener (master Ingress `argocd/argocd-public-production`, owned by another team) stopped inserting `X-Forwarded-For`, or a proxy was added in front of web without changing web's `TRUSTED_PROXY_HOPS` | Re-run P5(b) (sentinel-event-enrichment spec §10.3). Until it passes, roll web back to the sdk-js version before 0.2.0, whose tunnel writes no IP (`sentinel-sdk-js-rich-context/TASK-06` owns web's pin). Then get the listener's owner to restore the insertion, or set the hop count to the real proxy count. |
| Processor `CrashLoopBackOff`, `RESTARTS` climbing, `lastState.terminated.reason: Error` | processor rendered without `S3_ACCESS_KEY`/`S3_SECRET_KEY` while `SYMBOLICATE_PLATFORMS` is non-empty (startup fatal, contract decision 32) | last log line names the two variables; the §2 pre-deploy render above |
| Processor `RESTARTS` climbing, `lastState.terminated.reason: OOMKilled` | parsed source maps exceed `1Gi` despite `GOMEMLIMIT` | §2's RESTARTS note: `SOURCEMAP_MAX_PARSE_BYTES`, `SOURCEMAP_CACHE_BYTES`, or the limit + `GOMEMLIMIT` |
| Browser events for a mapped release stay unsymbolicated | maps uploaded after the events (Known limit 1), no `column` from the producer, or storage unreachable from the processor | `sentinel_symbolication_events_total{outcome="none"}` vs `sourcemaps` rows; `frames_total` by `outcome` |
| `POST /api/sourcemaps` 413 before the route runs | `BODY_SIZE_LIMIT` on the dashboard below the request (chart default `110M`) | `config.dashboard.bodySizeLimit`; the route's own caps are `SOURCEMAP_MAX_*` |
| Migrate Job fails, log names `1725200000 gate (c): N (project_id, event_id) group(s)` | duplicate groups on the table — O1 was skipped or is stale (the count decays within the hour) | §2's pre-deploy check: O1, O2 until zero, re-sync; no index was touched |
| Migrate Job fails, log names `1725200000 check (e): … indisvalid = false` | the concurrent build found a violation the gate could not see, or was killed mid-scan | O1 (a group formed between the gate and the scan → O2), then re-sync: block (d) drops the invalid index and rebuilds |
| Migrate Job fails, log shows `duplicate key value violates unique constraint "uq_error_occurrences_project_event" (SQLSTATE 23505)` under the backfill `DO $$` (block (b): `SET project_id = i.project_id … AND o.project_id IS NULL`), same failure on every retry, under the `ingestor` or `dashboard` target | block (b)'s sweep ran with the index already present — a rollout-window row (`project_id IS NULL`, an old-tag pod's insert) whose event is already stored under the key on another issue (contract decision 15's hazard). The block's `to_regclass` guard exits before the first batch when the index exists; seeing this means the migrate image predates that guard — check the tag | §2's **O3 step 1**: the `COALESCE(o.project_id, i.project_id)` group query, then O2 on every group it returns; re-sync, and run O3 in full once the roll completes |
| Processor dead-letters every event with `42703` on `project_id` after a rollback | `migrate down` ran while new-tag processor pods were attached | §3's rollback entry: processor tag `v0.2.18` FIRST; `Down` is not part of a rollback |
| Dashboard `/issues/[id]` returns 500 on every load after a rollback; dashboard log shows `42703` `column "project_id" does not exist` | `migrate down` ran while new-tag dashboard pods were attached — `getOccurrencesByIssueId` (`issue-queries.ts:200`) is a bare `select()` that names every mirror column, `project_id` included | §3's rollback entry, step 2's precondition: roll the dashboard tag back to `v0.2.18` FIRST (`images.dashboard.tag=v0.2.18`, `rollout status deploy/sentinel-sentinel-dashboard`); re-applying the file also clears it |


### Sentry-compatible ingest (sentinel-sentry-compat-ingest)

The ingestor serves `POST /api/{project_id}/envelope/` and `/store/` for core's mobile relay (docs/sdk-specification.md §10).

- **`sentinel_compat_items_total{type,outcome}`** counts every envelope item once. `rejected` or `failed` above zero means lost
  Sentry events: read the ingestor's `Failed to ingest` lines. `too_large` means an event over 1 MiB as sent **or** one still over
  the 1,000,000-byte publish budget after shedding; it is answered `413`, which core's relay must not retry.
- **`event=ingest.event_shed`** Warn lines mean events were published with parts removed to fit the budget. The occurrence's
  `metadata.server_truncated_keys` says which (`breadcrumbs:<n>`, bare `extra` keys, `contexts.<name>`, `exception.values:<n>`,
  `stacktrace.context`).
- A **`403`** on a compat route is a key that is not a project `ingest` key, or a path project id that does not match the key's
  project. A `401` is a missing or unknown key. The derived id is shown on the dashboard's project settings (Sentry SDK section).
- Trace of an envelope: it is one `sentinel_ingest_requests_total` request, then one `sentinel_compat_items_total` series per item.

### Sentry-compatible ingest: trusting core's relay (sentinel-sentry-compat-ingest)

`config.ingestor.sentryCompatTrustedProxyCidrs` (env `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS`) ships **empty** in `values.yaml` and in
every overlay: with it empty every relay header is ignored, whatever `config.ingestor.trustedProxyCidrs` holds, and the compat routes' `X-Forwarded-For` handling is exactly `/ingest`'s. This list **alone** grants relay identity; `trustedProxyCidrs` never does (it only widens the `X-Forwarded-For` walk on these routes). Setting
it belongs to `core-sentinel-mobile-relay`'s rollout, by the steps below. This entry names no value.

**Order of the rollout:** `core-sentinel-mobile-relay` sets this list (step 4) and passes the check (step 5) **before** it enables
core's anonymous relay route. While the list is empty every relayed request is untrusted, so D14's anonymous stripping does not
apply yet.

1. **What the value is.** The CIDR of the **immediate peer** the ingestor pod sees on the relay's connections: the address in the
   connection's `RemoteAddr`, never an address inside `X-Forwarded-For`. That one address decides whether `X-Forwarded-For` is read
   (here or in `trustedProxyCidrs`) and, when it is inside **this list**, whether the `X-Sentinel-Relay-*` identity headers are honoured, so **every host inside the CIDR can assert any app
   user and tenant**. Use the narrowest CIDR that covers the relay's peer addresses (a single-address prefix per address where
   the addresses are stable), never a VPC, a subnet "to be safe", or the ingestor's own range.
2. **How to observe it.** With the list still empty, send one relayed event. Every compat request that carries
   `X-Sentinel-Relay-Auth` from an untrusted peer is logged by the ingestor at Info as
   `event=sentrycompat.relay_peer_untrusted peer=<ip>` (the peer address only, never a header value). The `peer` values seen for
   the relay's traffic are the candidates.
3. **Which topology produced it**, and what that means:
   - **Direct to the ingestor's Service** (core's consumer in the same cluster calls the chart's `<fullname>-ingestor` ClusterIP
     Service, `templates/ingestor.yaml`): the peer is the consumer **pod's** address. Pod addresses move on every reschedule, so
     the narrowest stable value is the range core's consumer pods are scheduled from (its node group's pod range); prefer a
     dedicated one. This is the topology to choose: only core's pods are trusted.
   - **Through an internal load balancer** (for example `api-sentinel.staging.tuju.ai`): the peer is the load balancer's private
     address, and **every caller of that load balancer becomes a trusted relay**: it can set `X-Sentinel-Relay-*` and
     `X-Forwarded-For` at will. Do not trust a shared load balancer; route the consumer to the Service directly instead. If there
     is no alternative, the load balancer must append the caller's address to `X-Forwarded-For`, core's range must be listed as
     well (the walk skips only trusted hops), and the operator records that every caller of that load balancer is trusted.
   - **Across clusters or VPCs through NAT**: the peer is the NAT gateway's egress address; list exactly those addresses, and note
     that everything else egressing through the same NAT is trusted too.
4. **Set it** in that environment's overlay (`values.aws.staging.yml`, `values.aws.production.yml`), which already carries an
   explicit empty `sentryCompatTrustedProxyCidrs: []` to edit, in the same change as the relay's rollout, and roll the ingestor.
5. **Check it** (two-sided): one relayed event whose occurrence has `event_user.ip_address` equal to the test phone's public
   address (not empty, not core's) and `tags.relay_auth` set; and one request to the same route from another in-network host
   carrying `X-Sentinel-Relay-Auth: user` whose occurrence has **no** `relay_auth` tag.

A wrong value fails one of two ways: too narrow drops the phone's address and the relay's identity (the fail-closed state this
change ships); too wide lets another in-network caller forge both.
