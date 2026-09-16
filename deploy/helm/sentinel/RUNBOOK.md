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
only change is the Drizzle mirror's declaration of the column, and a dashboard
on either tag is correct against either schema — neither needs a roll-back:

```bash
argocd app set sentinel-<env> --parameter images.processor.tag=v0.2.18
argocd app sync sentinel-<env>
kubectl --context hwc-daya-cce-production -n sentinel-<env> rollout status deploy/sentinel-sentinel-processor
```

That is the whole rollback. Stop here.

**Step 2 — `Down`, only if the column itself must go, and never before step 1
has completed.** `Down` drops `uq_error_occurrences_project_event` (concurrently)
and then `error_occurrences.project_id`. A processor on this feature's tag
against a database that ran `Down` gets **`42703` (undefined column
`project_id`) on every `INSERT`** — class 42, which `classifyStoreError` leaves
*retryable*, so every event burns `MaxDeliver` (7) and is dead-lettered until the
tag is rolled: an ingest outage, not a broken chart (contract §7, §8). That is why
`Down` is not part of a rollback and why, if it is ever run, the running pods
must already be on `v0.2.18`:

```bash
# Only after step 1's rollout status reports the roll complete, from the migrate image's shell:
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
  replaying the same file as a no-op in seconds (the column exists, the backfill
  finds no NULL row, the gate finds no group, the index is valid and
  `IF NOT EXISTS` skips the build). Goose prints per file, not per block; the
  blocks are visible by name only when one **fails**. In order, they are:
  1. the session guards — `SET statement_timeout = '15min'`, `SET lock_timeout = '5s'`
     (decision 16: the SETs are session-scoped under `NO TRANSACTION`);
  2. **(a)** `ALTER TABLE error_occurrences ADD COLUMN IF NOT EXISTS project_id UUID` —
     catalog-only, milliseconds under `ACCESS EXCLUSIVE`; an ingest holding the
     table makes it fail with `canceling statement due to lock timeout` — re-sync;
  3. **(b)** the backfill `DO` block — `project_id := issues.project_id` via
     `issue_id`, `WHERE project_id IS NULL`, 10,000 rows per batch by `id` range,
     one `COMMIT` per batch (decision 20). A failure here is
     `canceling statement due to statement timeout` naming the `DO $$` — every
     committed batch stays and the re-run continues from the remaining NULLs;
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
| Processor `CrashLoopBackOff`, `RESTARTS` climbing, `lastState.terminated.reason: Error` | processor rendered without `S3_ACCESS_KEY`/`S3_SECRET_KEY` while `SYMBOLICATE_PLATFORMS` is non-empty (startup fatal, contract decision 32) | last log line names the two variables; the §2 pre-deploy render above |
| Processor `RESTARTS` climbing, `lastState.terminated.reason: OOMKilled` | parsed source maps exceed `1Gi` despite `GOMEMLIMIT` | §2's RESTARTS note: `SOURCEMAP_MAX_PARSE_BYTES`, `SOURCEMAP_CACHE_BYTES`, or the limit + `GOMEMLIMIT` |
| Browser events for a mapped release stay unsymbolicated | maps uploaded after the events (Known limit 1), no `column` from the producer, or storage unreachable from the processor | `sentinel_symbolication_events_total{outcome="none"}` vs `sourcemaps` rows; `frames_total` by `outcome` |
| `POST /api/sourcemaps` 413 before the route runs | `BODY_SIZE_LIMIT` on the dashboard below the request (chart default `110M`) | `config.dashboard.bodySizeLimit`; the route's own caps are `SOURCEMAP_MAX_*` |
| Migrate Job fails, log names `1725200000 gate (c): N (project_id, event_id) group(s)` | duplicate groups on the table — O1 was skipped or is stale (the count decays within the hour) | §2's pre-deploy check: O1, O2 until zero, re-sync; no index was touched |
| Migrate Job fails, log names `1725200000 check (e): … indisvalid = false` | the concurrent build found a violation the gate could not see, or was killed mid-scan | O1 (a group formed between the gate and the scan → O2), then re-sync: block (d) drops the invalid index and rebuilds |
| Processor dead-letters every event with `42703` on `project_id` after a rollback | `migrate down` ran while new-tag processor pods were attached | §3's rollback entry: processor tag `v0.2.18` FIRST; `Down` is not part of a rollback |
