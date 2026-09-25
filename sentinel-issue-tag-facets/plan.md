# Sentinel issue tag facets: implementation plan

**Feature:** `sentinel-issue-tag-facets` · **Repository:** `sentinel` (`tuju-ai/sentinel`) · **Base:** `origin/main`, at planning
time `f49721f` (v0.2.37; #51 schema v2 and #53 enrichment merged, no open PR)
**Built from:** `sentinel-issue-tag-facets/spec.md` @ `48a9e5e` (branch `NurfitraPujo/sentinel-issue-tag-facets-specifying`, clean
after its 9th review round) and contract `prd/sentinel-issue-tag-facets/{contract,be,fe}.md` @ `bc275e6` (decisions 1-10). The
operator's rulings are binding here: **backfill every retained occurrence** (decision 2), **tag filters ANDed** (decision 6), **the
rollup reflects retained occurrences** (decision 3), **proofs never skip under the gate** (decision 5). If this plan and the spec
disagree, **the spec wins** and the seat reports the conflict upward. §7 lists the four places where this plan adds to the spec,
because the spec as written would leave a gate red or a proof skippable.
**Cards:** `sentinel-issue-tag-facets/TASK-01` (rollup, triggers, backfill, retention), `TASK-02` (issue-page tag distribution panel),
`TASK-03` (search by `key:value`). **One seat, one pull request.**
**Tier:** full, declared `migration`. Sides `be` and `fe`. §1.1 lists every path the change touches. That list is the surface the
plane re-derives the tier from.

"Red before green" follows CLAUDE.md: every test is seen to fail first. It fails either because the production code does not exist
yet, or because the spec's §10.2 "Mutation" is applied and then reverted. Each commit body records which, and what failed. `dw/`
means `apps/dashboard-web/`.

---

## 1. Seat

There is **one** implementing seat. The three cards cannot be given to separate seats without sharing paths:
- TASK-02 and TASK-03 both read the migration's objects (`issue_tag_values`, `issue_tag_derived`, the indexes), both depend on the
  Drizzle mirror (`dw/src/lib/db/schema.ts`), and both depend on `dw/src/lib/tag-filter.ts`: the panel links through `tagSearchHref`,
  and search parses with `parseTagParam`.
- TASK-01's retention step and TASK-03's search both write `dw/src/lib/server/**` and share the check-query module
  `dw/src/lib/server/tag-facets-check.sql.ts`. They also share `scripts/test-dashboard-pg.sh`, which is the one gate step that runs
  both real-Postgres suites.
- The gate guard `tests/unit/tag_facets_gate_test.go` reads files that belong to all three cards.

So the cards run as a **sequence of commits inside one seat** (§3).

| | |
|---|---|
| **Seat** | `sentinel-issue-tag-facets-implementing` |
| **Repository** | `sentinel` |
| **Branch** | `NurfitraPujo/sentinel-issue-tag-facets-implementing`, cut from `origin/main` after `git fetch origin`. The seat's first action is to merge `origin/NurfitraPujo/sentinel-issue-tag-facets-planning` (docs only: `spec.md`, `plan.md`), so the PR carries the spec it implements. |
| **Cards** | TASK-01, TASK-02, TASK-03 |
| **Depends on** | nothing inside this run. It runs **in parallel** with `sentinel-occurrence-detail-panels` (contract decision 4). §5 is the interface between the two runs. |
| **Pull request** | one, against `main`: `feat: issue tag facets — rollup triggers, backfill, retention, tag distribution panel, key:value search (sentinel-issue-tag-facets)` |
| **Gate** | §4, run on the final pushed commit in that order. The output goes into the PR body and the report. |

### 1.1 Paths the seat may write (the whole change's surface)

```
# migration (TASK-01)
packages/db-migrations/migrations/1725400000_add_issue_tag_values.sql   (new; the id is re-checked at PR time, §6)
# processor (TASK-01)
apps/processor-go/store/store.go                         doc comment only: the fifth in-transaction exception (spec §3.5)
apps/processor-go/tagfacets/backfill.go                  (new) Run, Options, Result, ErrAlreadyRunning (spec §4.1-§4.2)
apps/processor-go/main.go                                `backfill-tag-facets` dispatch on os.Args[1], before NATS; exit 2 on ErrAlreadyRunning
# Go proofs (TASK-01; all never-skip, spec §10.1)
tests/integration/tag_facets_db_test.go                  (new) tagFacetsDB helper, tagFacetsCheckSQL, migration-file locator
tests/integration/tag_facets_pairs_test.go               (new) K1-K3, V1-V3
tests/integration/tag_facets_rollup_test.go              (new) StoreEvent counts, duplicates, cap/overflow, admission, subtraction cost
tests/integration/tag_facets_locks_test.go               (new) deterministic lock-order proofs
tests/integration/tag_facets_drift_test.go               (new) W11
tests/integration/tag_facets_backfill_test.go            (new) backfill boundary, resume, rerun, second runner, index bound
tests/integration/tag_facets_retention_test.go           (new) the sweep twin
tests/integration/tag_facets_migration_test.go           (new) TestTagFacetsMigration_* (plan addition A2, §7: the spec puts these in db_migrations_test.go)
tests/unit/tag_facets_gate_test.go                       (new) the gate guard (spec §10.1)
tests/unit/helm_tag_facets_backfill_test.go              (new) the chart shape (spec §10.2 "Chart")
# chart, compose, runbook (TASK-01)
deploy/helm/sentinel/templates/tag-facets-backfill-cronjob.yaml          (new) the suspended CronJob
deploy/helm/sentinel/templates/prometheusrule.yaml                       the `sentinel-retention` group
deploy/helm/sentinel/values.yaml                         tagFacetsBackfill.*, retentionCron.timeoutSeconds 30→180 and activeDeadlineSeconds 120→600, prometheusRule.retention*
deploy/helm/sentinel/RUNBOOK.md                          the new "Tag facets backfill (sentinel-issue-tag-facets)" section, retention keep-up and route outcomes (spec §9 row)
docker-compose.yml, scripts/Dockerfile.cron, scripts/cron-entrypoint.sh   RETENTION_CRON_TIMEOUT_SECONDS 30→180
# dashboard, TASK-01 retention
dw/src/lib/db/schema.ts                                  mirror: issueTagValues, issueTagValuesBackfill, issueTagValuesDrift, errorOccurrences.tagRollupApplied
dw/tests/tag-facets-mirror.test.ts                       (new)
dw/src/lib/server/retention.ts                           deleteAgedOccurrences, reportTagRollupDrift, rebuildPendingTagRollups, run plumbing
dw/src/lib/server/retention-batch.ts (+ .test.ts)        (new) constants and pure functions (spec §5)
dw/src/lib/server/retention-run.ts (+ .test.ts)          (new) openRetentionRun, RetentionDeadlineError, RetentionConnectionLost
dw/src/lib/server/db.ts                                  hoisted connectionString/types; openRetentionRunClient(onclose)
dw/src/lib/server/storage.ts, dw/src/lib/server/attachment-reaper.ts, dw/src/lib/db/queries/organizations.ts   exec/signal parameters only
dw/src/lib/server/metrics.ts, dw/src/lib/server/metrics.test.ts          sentinel_retention_runs_total{outcome}
dw/src/lib/server/tag-facets-check.sql.ts                (new) the check query, shared by both dashboard suites
dw/src/lib/server/retention.tag-facets.integration.test.ts               (new) real Postgres, TAG_FACETS_INTEGRATION_REQUIRED
dw/src/lib/server/retention.attachments.test.ts          harness only (plan addition A1, §7): passes a `run` built on its mock; R6 assertions unchanged
dw/src/routes/api/cron/retention/+server.ts, dw/src/routes/api/cron/retention/server.test.ts
# dashboard, TASK-02 panel
dw/src/lib/server/queries/issue-queries.ts               getTagFacets(issueId)
dw/src/lib/server/queries/issue-queries.tag-facets.test.ts   (new)
dw/src/lib/tag-facets.ts (+ .test.ts)                    (new) buildTagFacets, largest remainder
dw/src/lib/components/issues/IssueTagFacets.svelte (+ .test.ts)          (new)
dw/src/routes/issues/[id]/+page.server.ts, dw/src/routes/issues/[id]/+page.svelte
dw/src/routes/issues/[id]/page.tag-facets.test.ts        (new)
dw/src/routes/issues/[id]/page.server.test.ts            one line (plan addition A1): `getTagFacets` added to its issue-queries mock
# dashboard, TASK-03 search
dw/src/lib/tag-filter.ts                                 (new) the byte-identical seed (C1), then TASK-03 appends to it (C9)
dw/src/lib/tag-filter.test.ts                            (new)
dw/src/lib/server/search.ts
dw/src/lib/server/search.tag-facets.integration.test.ts  (new) real Postgres, TAG_FACETS_INTEGRATION_REQUIRED
dw/src/lib/server/search.tag-forms.test.ts, search.tag-hints.test.ts, search.no-tag-snapshot.test.ts (+ its __snapshots__/)   (new)
dw/src/routes/search/+page.server.ts, dw/src/routes/search/+page.svelte
# gate
scripts/test-dashboard-pg.sh                             (new)
Taskfile.yml                                             test-integration gains `- ./scripts/test-dashboard-pg.sh`
.github/workflows/ci.yml                                 dashboard job env: TAG_FACETS_INTEGRATION_REQUIRED: "1"
docs/memory/VERIFIED_STATE.md                            one entry: what runs, and the commands that proved it (CLAUDE.md)
# C0 only, when needed (§3 C0): the sibling run's base-debt commit, byte-identical
dw/src/lib/db/queries/organizations.members.test.ts (new), dw/src/routes/+page.svelte, dw/src/lib/db/queries/organizations.ts
# carried from planning, never edited
sentinel-issue-tag-facets/spec.md, sentinel-issue-tag-facets/plan.md
```

**Paths the seat may not write:** everything else. In particular: `apps/ingestor-go/**`, `packages/proto/**`, `gen/**`,
`packages/sdk-*/**` and `packages/shared-go/**` (spec §9: none of them changes). The agent API (`dw/src/routes/api/agent/**`,
`dw/src/lib/server/agent-api-spec/**`, `dw/src/lib/db/queries/agent-reads.ts`; contract "nothing in the agent API changes"; its
occurrence reads project columns explicitly, so the new `tagRollupApplied` column does not leak into them). Every existing
`tests/integration/*.go` file, `tests/integration/setup_*.go` included. `dw/src/app.css`. The issues list's `q`
(`dw/src/lib/server/queries/issue-queries.ts` changes only by adding `getTagFacets`).

Rules that hold in every commit:
- No route file gains an export other than `load`, `actions` and the route handlers (B12). Constants go in `$lib/server/…`.
- Tag keys and values are text interpolation, never `{@html}`.
- No `db.transaction` callback `return`s early to signal failure (D18).
- No tag-facets Go test calls `t.Parallel()`: goose's table name is process-global (`goose.SetTableName`), and `tagFacetsDB`
  migrates under its own ledger.

---

## 2. Before the first commit

1. `git fetch origin`. Cut the branch from `origin/main`, merge the planning branch (§1), and push with `-u origin HEAD`.
2. **Private caches, always.** The seat does this in bash; `env …` prefixes work in fish too:
   ```bash
   export GOCACHE=/home/fitrapujo/.cache/go-build-sentinel-tfs-impl
   export TMPDIR=/home/fitrapujo/.tmp/sentinel-tfs-impl GOTMPDIR=/home/fitrapujo/.tmp/sentinel-tfs-impl
   mkdir -p "$GOCACHE" "$TMPDIR"
   ```
   Every Go, task, pnpm and docker command in this plan runs with these set. The shared default cache is never used.
3. **The migration id.** `git ls-tree --name-only origin/main packages/db-migrations/migrations/ | sort | tail -1` must print
   `…/1725300000_add_error_occurrences_event_v2_fields.sql`. At planning time it does: #51 added `1725300000` and #53 added no
   migration. No other remote branch adds a version above `1725300000`
   (`git diff --name-only --diff-filter=A origin/main...<branch> -- packages/db-migrations/migrations/`, run over every remote
   branch). So the id is **`1725400000`**. If `main` has moved past it, use the next free id and follow §6.
4. **What is already on `main` from the sibling run** (§5). Each of these decides whether a conditional commit runs:
   - `git ls-tree origin/main -- dw/src/lib/tag-filter.ts` prints a line → **C1 is skipped**.
   - `cd dw && pnpm install --frozen-lockfile && pnpm check`. At `f49721f` it reports **`5 errors and 1 warning in 2 files`**
     (sibling spec T30: `users` is not imported in `organizations.ts:627-633`, plus `state_referenced_locally` at
     `routes/+page.svelte:162`). If it reports `0 errors and 0 warnings`, **C0 is skipped**.
5. **The scratch database, and the container rule.** This laptop has kernel-panicked while containers were starting. So:
   - The **container suite** is `task test-integration`. It starts about five containers per `go test` run, and the script starts
     one more. It runs **once, alone, at the end** (§4 step 9). It runs only when no container has been created in the last 5
     minutes:
     `for e in docker podman; do $e ps -aq | xargs -r $e inspect -f '{{.Created}} {{.Name}}'; done | sort | tail -1`,
     compared with `date -u`.
   - For the inner loop, the seat creates **one** scratch Postgres, once, after the same 5-minute check. It is a single container,
     reused for every inner-loop run and never re-created:
     ```bash
     docker run -d --name sentinel-tfs-scratch -e POSTGRES_USER=sentinel -e POSTGRES_PASSWORD=changeme \
       -e POSTGRES_DB=sentinel -p 127.0.0.1::5432 postgres:15-alpine
     PORT=$(docker port sentinel-tfs-scratch 5432 | sed 's/.*://')
     export SCRATCH_URL="postgres://sentinel:changeme@127.0.0.1:$PORT/sentinel?sslmode=disable"
     (cd packages/db-migrations && env GOWORK=off DB_URL_DASHBOARD="$SCRATCH_URL" go run ./cmd/migrate up -target dashboard)
     ```
     The port is random and on loopback only, so nothing can reach the shared dev database on 5432. **Never set `DATABASE_URL` to
     `localhost:5432`.** It is removed (`docker rm -f sentinel-tfs-scratch`) before §4's container step, which must then wait out
     the 5 minutes again.
   - What the scratch database proves in the inner loop: the migration (up, down, up, then every Up block replayed through `psql`
     under a second ledger), each SQL mutation of §10.2 (apply it, see the check query go non-zero, revert), both dashboard
     real-Postgres suites
     (`env DATABASE_URL="$SCRATCH_URL" TAG_FACETS_INTEGRATION_REQUIRED=1 pnpm --dir apps/dashboard-web exec vitest run <file>`),
     the existing `retention.*.integration.test.ts` suites (C7), and the real `go run ./apps/processor-go backfill-tag-facets`
     against seeded rows (C4).
   - **The Go integration tests cannot run in the inner loop.** `tests/integration`'s `TestMain` always starts Redis and, under
     `FORCE_TESTCONTAINERS=1`, Postgres, NATS, the ingestor and the processor. Before §4 they are proven two ways, and the commit
     bodies say which applied. (a) Compile-red: `go vet ./tests/integration/` fails while the symbol they call does not exist.
     (b) SQL-red on the scratch database: the test's own statements and the check query, run through `psql` with the §10.2
     mutation applied, give the wrong count. They run green, for real, in §4 step 9. If that run is red, the seat fixes it, waits
     for the 5-minute rule again, and runs the suite once more. It never loops.
6. Leave the compose stack **down** for G2/G4 (`docker ps` shows no `sentinel-postgres`). The dashboard's real-Postgres suites then
   skip there, as they do on the base, and their proof is §4 step 9. If compose were up they would run against the shared dev
   database. The scoped check query makes that safe, but it is not the gate.

---

## 3. Ordered steps and commits

**Push after every commit** (`git push`; the laptop has kernel-panicked). Before each push the seat runs the fast checks for what the
commit touched:
- Go: `go build ./... && go vet ./...`, plus `go test ./tests/unit/... -count=1` when `tests/unit` changed.
- Dashboard: `pnpm check` in `dw` (0 errors and 0 warnings from C0 on), `pnpm build` when a route file changed, and `pnpm vitest run
  <files>`.

Each commit is TDD. The named tests are written first and seen red; the production code turns them green. The commit body lists
each red, and each §10.2 mutation applied and reverted with the failure it produced.

### C0 — base `pnpm check` debt. **Only if §2 step 4 found it red.** On its own.

The sibling run's `308bfd0` (`fix(dashboard): import users in organizations.ts; untrack list-page searchQuery`), taken byte for
byte with `git cherry-pick 308bfd0`. It is three files (`organizations.ts`: the `users` import; `routes/+page.svelte`: `untrack`; a
new `organizations.members.test.ts`). Check: `git show HEAD | git patch-id --stable` prints
`f9c3318e47b24971af66192f871e8aa964cf25e2` first.
- **Why here:** G4 needs `pnpm check` clean, and at `f49721f` it is red with pre-existing debt from `e91b07e` (#52). That debt also
  breaks `GET /api/agent/members` at runtime. The seat does not weaken the gate to "no new errors". Instead it carries the identical
  fix. Whichever PR merges second finds the same content on `main`, so its copy of the commit applies as empty and is dropped.
- **Red:** `pnpm check` at the base, 5 errors and 1 warning; the new test fails with `ReferenceError: users is not defined`.
  **Green:** `0 errors and 0 warnings`.
- If `308bfd0` has changed on the sibling branch by then, take the sibling's current commit (`git log
  origin/NurfitraPujo/sentinel-occurrence-detail-panels-implementing.sentinel -- dw/src/routes/+page.svelte`) instead, and record its
  patch-id.

### C1 — the shared tag-link encoder seed. **Skipped if `tag-filter.ts` is on `origin/main`.** Alone.

`feat(dashboard): shared tag-link encoder (tag-filter.ts) for occurrence panels and tag facets`

Exactly one file, `dw/src/lib/tag-filter.ts`, byte-identical to the sibling's `5e2fa3e`:
`git checkout 5e2fa3e -- apps/dashboard-web/src/lib/tag-filter.ts && git commit`. §5.1 names the checks that prove it.
Nothing else goes in this commit. It must come before C8, because the panel imports `tagSearchHref`.

### C2 — TASK-01: the migration. **Alone.**

`feat(db): issue_tag_values rollup, triggers and search indexes (sentinel-issue-tag-facets TASK-01)`

| Files | Exactly one: `packages/db-migrations/migrations/1725400000_add_issue_tag_values.sql`, spec §8.2's blocks in order: S, C, T1, T2 + singleton, T3, F (13 functions), I1, `statement_timeout 60min`, I2-I5, `statement_timeout 15min`, TR, R. Down as spec §8.4. `-- +goose NO TRANSACTION` on line 1 and nowhere else. One statement per `StatementBegin`/`StatementEnd`. A header naming the fifth exception, the "every writer and deleter locks `issues` first" rule, the ~70 µs-per-row update-trigger cost, and "batch, never disable the triggers" (decision 9; spec §8.3, §11.2). |
|---|---|
| Red | On the scratch database at `1725300000`: the C3 proofs' SQL (the check query, `issue_tag_pairs` rows, the `StoreEvent`-shaped insert) errors with `relation "issue_tag_values" does not exist` / `function issue_tag_pairs does not exist`. Write that SQL in C3's files first, run it red, then write this file. |
| Green | `cmd/migrate up` on the scratch database; `down` leaves 0 triggers, 0 functions, 0 of the relations and no column; `up` again gives 5 triggers, 13 functions, 11 relations and 1 column (spec §8.1's counts); the 55 Up statements replayed through `psql` under a second ledger with no error, **while a second session holds `ACCESS SHARE` on `error_occurrences`**; `cd packages/db-migrations && GOWORK=off go test ./... -count=1` green (its checks of every migration file run over the new one). |
| Rule | No application code, no test and no other file in this commit. That keeps it reviewable on its own and lets §6 renumber it with one `git mv`. |

### C3 — TASK-01: the rollup's proofs, the processor doc comment, the Drizzle mirror

`feat: tag rollup proofs, StoreEvent fifth exception, Drizzle mirror (TASK-01)`

| Files | `store.go` (the fifth bullet, spec §3.5, verbatim in meaning); `tests/integration/tag_facets_db_test.go` (`tagFacetsDB(t, opts)` exactly as spec §10.1 steps 1-4, `t.Fatalf` on every error; `tagFacetsCheckSQL`; and `tagFacetsMigrationFile(t)`, which finds the one `*_add_issue_tag_values.sql` in the directory and derives `tagFacetsVersion` from its name and `tagFacetsPrevVersion` as the greatest version below it. No test hard-codes `1725400000` or `1725300000`, so §6's renumbering touches no test); `tag_facets_pairs_test.go`, `tag_facets_rollup_test.go`, `tag_facets_locks_test.go`, `tag_facets_drift_test.go`, `tag_facets_migration_test.go` (§7 A2); `dw/src/lib/db/schema.ts` (spec §8.5, with the comment that the tables are trigger-written only); `dw/tests/tag-facets-mirror.test.ts` (locates the migration file by the same suffix glob). |
|---|---|
| Red | `go vet ./tests/integration/` while `tagFacetsDB` does not exist; each test's SQL red on the scratch database before C2 (§2 step 5b); the mirror test red before `schema.ts` changes; `schema-drift.test.ts`, run against the scratch database, red until the mirror has all three tables. |
| Green (inner loop) | Mirror and schema-drift green; every §10.2 SQL mutation for these rows seen red on the scratch database, then reverted. The Go tests run green in §4 step 9. |
| §10.2 rows | rollup exact counts; duplicate counts once; cap and overflow; K1-K3/V1-V3; old-pod insert and redelivery; lock order (insert, delete, sweep twin); update/truncate/cascade; admission cost; subtraction cost; drift is repaired (the Go half); Compatibility (migration idempotent, `DownThenUp`, `OldInsertWorks`, `BoundedLockWait`); Mirror. |

### C4 — TASK-01: the backfill runner and its Job

`feat(processor): backfill-tag-facets subcommand and suspended CronJob (TASK-01)`

| Files | `apps/processor-go/tagfacets/backfill.go` (the session advisory lock `hashtext('sentinel.tag_facets.backfill')`, the batch loop over `issue_tag_values_backfill_batch`, `SET LOCAL lock_timeout = '5s'`, `sleep max(pause, elapsed × (1 − duty) / duty)`, retry of `40001`/`40P01`/`55P03` up to 10 times, and the `tag_facets.backfill.{start,progress,done,already_done,already_running,retry}` lines, spec §4.1-§4.2); `apps/processor-go/main.go` (dispatch before NATS; exit 2 on `ErrAlreadyRunning`; exit 1 otherwise); `tests/integration/tag_facets_backfill_test.go`; `deploy/helm/sentinel/templates/tag-facets-backfill-cronjob.yaml` (literal `suspend: true`, `schedule: "0 0 31 2 *"`, `concurrencyPolicy: Forbid`, the processor image, `sentinel.postgresEnv`, args from `tagFacetsBackfill.{batch,pause,duty}`, `backoffLimit: 3`, `activeDeadlineSeconds: 21600`, `ttlSecondsAfterFinished: 604800`, `restartPolicy: Never`, the chart's pod security context and a `resources` block (kube-linter), and **no** hook, sync-wave or `helm.sh/hook` annotation; rendered iff `tagFacetsBackfill.enabled`); `values.yaml` `tagFacetsBackfill: {enabled: true, batch: 200, pause: 200ms, duty: 0.25}`; `tests/unit/helm_tag_facets_backfill_test.go` (T26 approach: no helm exec); `RUNBOOK.md` "Tag facets backfill" (the running-Job check, the `create job --from` command, the log lines, P2's lag check and its remedy, P4's query, `issue_tag_values_rebuild`, and a line for the operator to record the deploy date). |
|---|---|
| Red | `go vet ./tests/integration/` (no `tagfacets` package); the chart unit test (no template); `go run ./apps/processor-go backfill-tag-facets` against the scratch database with seeded pre-migration rows starts the service instead of backfilling. |
| Green (inner loop) | The same `go run`, with the processor's DB env pointed at `$SCRATCH_URL`, logs `tag_facets.backfill.done`, and the check query over the seeded issues is 0. A second run logs `already_done`. A run started while `psql` holds the advisory key exits 2 with `already_running`. `go test ./tests/unit/... -run HelmTagFacets` green. `helm template sentinel deploy/helm/sentinel --show-only templates/tag-facets-backfill-cronjob.yaml` renders one `CronJob`. |
| §10.2 rows | backfill (matches GROUP BY, locks the issue before counting, rerun, resume, refuses without triggers, second runner exits, index-bounded candidate); Chart. |

### C5 — TASK-01: retention's pure pacing module

`feat(dashboard): retention batch constants and pacing functions (TASK-01)`

`dw/src/lib/server/retention-batch.ts` holds every constant and pure function spec §5 and §9 name: `retentionPauseMs`,
`heldPercentile`, `occurrenceStopReason`, `nextOccurrenceBatch`, `OccurrenceBudgetSpent`, `retentionRunOutcome`,
`retentionRunCapacity`, and the `RETENTION_*` constants. It imports nothing from `$lib/server/db`. `retention-batch.test.ts` holds
every §10.2 "retention pacing" unit case, including the `retentionRunOutcome` table with both drained cases.
- **Red:** module missing. Then each listed mutation (flat pause, `heldPercentile` returning the max, budget 20,000 or ceiling 250,
  the backlog bound applied to a drained run, no bound at all).
- **Green:** `pnpm vitest run src/lib/server/retention-batch.test.ts`.

### C6 — TASK-01: one retention run, one connection, one deadline

`feat(dashboard): retention route runs on one per-run connection under a deadline (TASK-01)`

| Files | `db.ts` (`openRetentionRunClient(onclose)`: a `max: 1` client with `application_name: 'sentinel.retention.run'` and the keepalive startup parameters; `connectionString` and `types` hoisted); `retention-run.ts` + `retention-run.test.ts` (cases 1-8 of spec §10.2 "retention route"); `retention.ts` (`cleanupRetainedData(…, run?)` sends every step through `run.db`, and opens its own run when `run` is omitted; the occurrence delete is still today's one statement in this commit, reported as `occurrenceStopped: 'drained'`, and C7 replaces it); `storage.ts` (`deleteObject(key, signal?)`), `attachment-reaper.ts` (`exec`, `signal`), `organizations.ts` (`reapAllExpiredInvitations(exec = db)`) and the other reapers' `exec = db`; `routes/api/cron/retention/+server.ts` (try-lock, holder read aged by `backend_start`, 200 skip / 503 stale / 500 deadline / 500 lost, `recordRetentionRun` once per authorised `POST`, unlock unless lost, `end()` in `finally`); `server.test.ts` (the `$lib/server/db` and `$lib/server/metrics` mocks exactly as spec §10.2, plus the seven new cases); `retention.attachments.test.ts` (§7 A1); `metrics.ts` + `metrics.test.ts`; `prometheusrule.yaml` (`sentinel-retention`, three alerts, `or vector(0)`); `values.yaml` (`retentionCron.timeoutSeconds: 180`, `activeDeadlineSeconds: 600`, the `prometheusRule` retention keys); `docker-compose.yml`, `scripts/Dockerfile.cron`, `scripts/cron-entrypoint.sh` (180). |
|---|---|
| Red | `retention-run.test.ts` (module missing); `server.test.ts`'s new cases; `metrics.test.ts`. Then each mutation of the "retention route" row. |
| Green | Those files green; `pnpm check` and `pnpm build` green (the route exports only `POST`); `pnpm test --sequence.shuffle`. |

### C7 — TASK-01: batched, paced occurrence delete, drift report, deferred rebuilds

`feat(dashboard): retention deletes in paced issue-locked batches and repairs tag rollup drift (TASK-01)`

| Files | `retention.ts` (`deleteAgedOccurrences` exactly as spec §5's listing: keyset cursor, `issues FOR UPDATE` in id order before the `DELETE`, caps through `run.setCaps`, `OccurrenceBudgetSpent` guards, halving retry with its pause, the backlog read; `reportTagRollupDrift`; `rebuildPendingTagRollups` with the probe budget and probe timeout; the new `RetentionResult` fields; every `retention.*` event); `tag-facets-check.sql.ts`; `retention.tag-facets.integration.test.ts` (every dashboard case of §10.2 "retention", "retention pacing" and "drift is repaired", under `TAG_FACETS_INTEGRATION_REQUIRED`, with `pg_advisory_lock(hashtext('sentinel.retention.integration'))` held in `beforeAll`); `tests/integration/tag_facets_retention_test.go` (the twin, running the **exact** statements of `deleteAgedOccurrences`); `RUNBOOK.md` (retention keep-up, route outcomes, alerts, operator rebuilds: the rest of spec §9's RUNBOOK row). |
|---|---|
| Red | The new suite on the scratch database against C6's single `DELETE` (no batches, no drift event); then each mutation of the rows it proves, on the scratch database. |
| Green (inner loop) | `env DATABASE_URL="$SCRATCH_URL" TAG_FACETS_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/retention.tag-facets.integration.test.ts`. The existing `src/lib/server/retention.*.integration.test.ts` suites, run once against the scratch database, stay green: this commit rewrites the step they exercise. The twin compiles (`go vet`). |
| §10.2 rows | retention (sweep equals GROUP BY; zero rows deleted; lock order); retention pacing (suite half); drift (dashboard half: reported, rebuilt, operator-only, head of line, probe budget and timeout). |

### C8 — TASK-02: the issue-page tag distribution panel

`feat(dashboard): issue tag distribution panel (TASK-02)`

| Files | `issue-queries.ts` (`getTagFacets`: spec §7.2's one query) + `issue-queries.tag-facets.test.ts` (exactly one statement); `dw/src/lib/tag-facets.ts` + `.test.ts` (F1-F6, largest remainder, 500 random fixtures sum to 100.0); `IssueTagFacets.svelte` + `.test.ts` (test ids `issue-tag-facets`, `tag-facet-block`, `tag-facet-value`, `tag-facet-other`; hrefs from `tagSearchHref`, the `a:b` key included; a `<script>` value renders as text; empty renders nothing); `routes/issues/[id]/+page.server.ts` (`getTagFacets` called once, after both access checks; `tagFacets` returned beside `trend`); `+page.svelte` (placed after the trend section and before `IssueRelations`; its import beside `IssueRelations`'s); `page.tag-facets.test.ts` (calls `load` **without** a `url`, which the sibling run's loader tolerates: its `url?.`); `page.server.test.ts` (one line, §7 A1). |
|---|---|
| Red | All new files (missing modules and component); `page.server.test.ts` goes red with `issueQueries.getTagFacets is not a function` the moment the loader calls it, and that red is why its one-line mock addition belongs in this commit. Then the mutations: sort by value, round each percent on its own, link "Other", call per key. |
| Green | Those files; `pnpm check`, `pnpm build`, `pnpm test --sequence.shuffle`. Svelte 5 runes, a per-component `<style>`, `app.css` variables, no Tailwind. |

### C9 — TASK-03: search by tag `key:value`

`feat(dashboard): /search tag=<key>:<value> and key:value tokens, ANDed, rollup-backed (TASK-03)`

**Order inside the step matters.** The §6.6 no-tag snapshots must come from `f49721f`'s `search.ts`. So the seat writes
`search.no-tag-snapshot.test.ts` first and generates its snapshots (`pnpm vitest run -u src/lib/server/search.no-tag-snapshot.test.ts`)
**before** editing `search.ts`. It confirms `git diff origin/main -- dw/src/lib/server/search.ts` is empty at that moment, and only
then changes the code.

| Files | `tag-filter.ts`: **appends** `TAG_KEY_PATTERN`, `MAX_TAG_VALUE_BYTES`, `MAX_TAG_FILTERS`, `parseTagParam`, `parseSearchText` and `formatSearchToken` (spec §6.1-§6.2) **below** the seed's last line. The seed's ten lines are not edited, so `diff <(git show 5e2fa3e:apps/dashboard-web/src/lib/tag-filter.ts) <(head -n 10 apps/dashboard-web/src/lib/tag-filter.ts)` stays empty (§5). Also `tag-filter.test.ts` (P1-P6; the shared href table's exact strings, the `it's (x)!~` row and its `URLSearchParams` spelling included; the §6.2 grammar; the 1,000-case property round trip); `search.ts` (`SearchFilters.tags`; the per-filter condition with branches (a) and (b); the hint map; form J when an id filter is present, otherwise form L with `issues.environment = $env` in the outer `WHERE`, no environment condition inside the `LATERAL`, and `o.platform` inside it; the inner join kept); `search.tag-forms.test.ts` (and the rows-read metric helper on a canned plan: 20,250); `search.tag-hints.test.ts` (reads the migration file by suffix glob); `search.no-tag-snapshot.test.ts`; `search.tag-facets.integration.test.ts` (cases 1-8, including (5b) equivalence and (5c) the bound, under `TAG_FACETS_INTEGRATION_REQUIRED`); `routes/search/+page.server.ts` (`getAll('tag')`, box tokens, union, de-duplicate, first 10, `ignoredTagTerms`, never throws); `+page.svelte` (chips with remove links, hidden `tag` inputs, `buildSearchUrl` appending every tag, results shown when a tag is active, the ignored-terms hint, the subtitle). |
|---|---|
| Red | New modules missing; the snapshot test is green on the unmodified `search.ts` by construction and guards §6.6 from here on; the integration suite red on the scratch database before `search.ts` changes. Then the TASK-03 mutations (split on the first `:`, `URLSearchParams` href, OR with the id group, LEFT join, drop branch (b), drop the issue-level environment test, keep `o.environment` in the `LATERAL`, form L with an id filter, emitted-rows-only metric, no hint for `device.model`). |
| Green (inner loop) | `env DATABASE_URL="$SCRATCH_URL" TAG_FACETS_INTEGRATION_REQUIRED=1 pnpm vitest run src/lib/server/search.tag-facets.integration.test.ts`; the unit files; `pnpm check`, `pnpm build`, `pnpm test --sequence.shuffle`. |

### C10 — the gate that cannot skip

`test: tag-facets proofs run under task test-integration and cannot skip (decision 5)`

| Files | `scripts/test-dashboard-pg.sh` (spec §10.1 steps 1-5: random loopback port, `trap … EXIT`, `pg_isready` up to 60 s, `cmd/migrate up -target dashboard` with `GOWORK=off`, frozen install, both suite paths with `TAG_FACETS_INTEGRATION_REQUIRED=1`); `Taskfile.yml` (`- ./scripts/test-dashboard-pg.sh` after the `go test` line); `.github/workflows/ci.yml` (`dashboard` job env); `tests/unit/tag_facets_gate_test.go` (every bullet of spec §10.1 "The guard", `tag_facets_migration_test.go` included by its glob); `docs/memory/VERIFIED_STATE.md` (dated entry: what runs, with §4's commands). |
|---|---|
| Red | The gate test before the script, the Taskfile line and the CI env exist; then its four named mutations (`requireInfra` in `tagFacetsDB`, `migTestDB` from a `tag_facets_*` test, `pingOrSkip` moved into `setup_helper_test.go`, `or vector(0)` dropped). |
| Green | `go test ./tests/unit/... -run TagFacets -count=1`, then §4 in full. |

---

## 4. Gates the seat runs before reporting (final commit, in this order)

All with §2 step 2's `GOCACHE`/`TMPDIR`. Each command's output tail goes into the PR body; steps 2, 5 and 9 go in verbatim.

| # | Command | Pass |
|---|---|---|
| 1 | `task build` (G1) | exit 0 |
| 2 | `env GOWORK=off go build ./... && env GOWORK=off go vet ./... && env GOWORK=off go test ./tests/unit/... -count=1` (CI `go-root`) | exit 0. This compiles `tests/integration` against `db-migrations`' **published** pseudo-version (A2); the helpers it uses, `RunMigrations`, `BaselineVersion` and `MigrationOptions`, all exist there |
| 3 | `cd packages/db-migrations && env GOWORK=off go build ./... && env GOWORK=off go test ./... -count=1` (CI `go-migrations`) | exit 0 |
| 4 | `task test-unit` (G2) | exit 0. It includes `tag_facets_gate_test.go`, `helm_tag_facets_backfill_test.go` and every new dashboard unit file |
| 5 | `cd apps/dashboard-web && pnpm build && pnpm check && pnpm test --sequence.shuffle` (G4) | build green (the route-export allowlist, B12); `svelte-check found 0 errors and 0 warnings`; tests green in shuffled order (B13) |
| 6 | **Chart (G5).** `helm lint deploy/helm/sentinel`; then for each of `""`, `-f deploy/helm/sentinel/values-huawei-staging.yml`, `-f deploy/helm/sentinel/values-huawei-production.yml`: `helm template sentinel deploy/helm/sentinel $V --show-only templates/tag-facets-backfill-cronjob.yaml > "$TMPDIR/tfb.yaml"`, then `grep -c '^kind: CronJob$'` = 1, `grep -c '^kind: Job$'` = 0, and `grep -q 'suspend: true'`, `schedule: "0 0 31 2 \*"`, `concurrencyPolicy: Forbid`, `activeDeadlineSeconds: 21600` and the args `backfill-tag-facets`, `-batch`, `"200"`, `-pause`, `200ms`, `-duty`, `"0.25"` all present, while `grep -E 'argocd.argoproj.io/(hook\|hook-delete-policy\|sync-wave)\|helm.sh/hook'` finds nothing; `helm template … --set tagFacetsBackfill.enabled=false --show-only templates/tag-facets-backfill-cronjob.yaml` **fails** ("could not find template": nothing rendered); and CI's `lint-helm-huawei` loop: `helm template sentinel ./deploy/helm/sentinel --values ./deploy/helm/sentinel/<overlay> \| kube-linter lint --config ./deploy/helm/sentinel/.kube-linter.yaml -` for both overlays | every assertion holds; kube-linter clean |
| 7 | **Identity checks** (§5.1, §5.2, §6): the seed's blob and patch-id checks; C0's patch-id when C0 ran; the migration-id re-check | all hold |
| 8 | `git diff --name-only origin/main...HEAD` | every path is in §1.1 |
| 9 | **The container suite, once, alone** (G3). `docker rm -f sentinel-tfs-scratch`; wait until §2 step 5's check shows no container created in the last 5 minutes and `docker ps`/`podman ps` show no other lane's testcontainers; then `task test-integration 2>&1 \| tee "$TMPDIR/g3.log"` | exit 0, and in the log: **no skipped proof.** `grep -E -- '--- SKIP: Test(TagFacets\|TagFacetsMigration)' "$TMPDIR/g3.log"` prints nothing. The number of `--- PASS: TestTagFacets…` lines equals `grep -rhoE '^func (TestTagFacets[A-Za-z0-9_]*)' tests/integration \| sort -u \| wc -l`. The script's vitest summary shows `Test Files  2 passed (2)` with no `skipped`. `SENTINEL_E2E` is **not** set (the proofs must not depend on it). The suite's 15 m timeout is not raised without a recorded measurement (Taskfile comment) |

Not run by the seat, and said so in the PR: CI's `integration`, `contract` and `e2e` jobs run only on `workflow_dispatch`, and the
`dashboard` job's `pnpm check`/`pnpm test` with `TAG_FACETS_INTEGRATION_REQUIRED=1` runs only on the push to `main`. The PR's CI
proves `pnpm build`, the Go jobs and `lint-helm-huawei`. The post-merge `main` run is recorded as a follow-up check.

### 4.1 Criterion → step

| Criterion (spec §10.2 row, contract §5) | Made to pass in | Gate |
|---|---|---|
| TASK-01 "N fixture events → exact counts per key/value" | C2 (triggers), proven by C3 | G3 |
| TASK-01 "a duplicate event counts once"; rolled-back insert counts nothing | C2, C3 | G3 |
| TASK-01 "the cap and overflow row" | C2, C3 | G3 |
| TASK-01 derivation rules K1-K3, V1-V3 | C2 (`issue_tag_pairs`), C3 | G3 |
| TASK-01 retried message and old-pod inserts counted once | C2 (mark trigger), C3 | G3 |
| TASK-01 concurrency and lock order (insert, delete) | C2, C3; the sweep-twin lock case with C7's statements | G3 |
| TASK-01 update / truncate / cascade | C2, C3 | G3 |
| TASK-01 admission cost; subtraction cost | C2 (`MATERIALIZED` state, `RETURNING`-fed zero-row delete), C3 | G3 |
| TASK-01 **backfill** (GROUP BY with concurrent processing, rerun, resume, locks, second runner, index bound) | C2 (`issue_tag_values_backfill_batch`), C4 | G3 |
| TASK-01 **retention**: sweep equals GROUP BY over survivors; 0-rows deleted; lock order | C7 (with C6's run) | G3 (script), CI `dashboard` on push |
| TASK-01 retention pacing (units; suite) | C5 (units), C7 (suite) | G2, G3 |
| TASK-01 retention route (one run, one connection, one deadline) | C6 | G2, G4 |
| TASK-01 drift is repaired (W11) | C2 (triggers' inline rebuild), C3 (Go half), C7 (route half) | G3 |
| TASK-02 panel: top-5 + percentages; "Other"; no panel when empty; one query per load | C8 (after C1's seed) | G2, G4 |
| TASK-03 `device.model:SM-A115F` only matching; AND; `:` inside the key round-trips; overflow found; the bound; equivalence | C9 (after C2's indexes) | G3 (script), CI `dashboard` on push |
| TASK-03 encoder table, parser P1-P6, grammar, round trip; hint drift; forms | C1 + C9 | G2, G4 |
| Compatibility: the five id filters unchanged without a tag | C9 (snapshots taken before the edit) | G2, G3 |
| Compatibility: migration idempotent across ledgers; no `ACCESS EXCLUSIVE` on replay; bounded lock wait; Down/Up; old insert works | C2, proven by C3's `tag_facets_migration_test.go` | G3 |
| Mirror | C3 | G2 |
| Gate cannot skip | C10 | G2 |
| Chart | C4 | G5, G2 |

---

## 5. The parallel run: `sentinel-occurrence-detail-panels` (contract decisions 4, 8, 10)

### 5.1 The shared encoder: which bytes, and the check that proves they match

The sibling run committed `dw/src/lib/tag-filter.ts` alone in **`5e2fa3e`** (on
`NurfitraPujo/sentinel-occurrence-detail-panels-implementing.sentinel`). Those are its plan §5.1 seed bytes: ten lines, tabs, a
trailing newline, `formatTagParam` and `tagSearchHref`. C1 carries the same file in a commit of its own. It is byte-identical when
all three of these hold, and §4 step 7 runs them on the final branch:

```bash
C1=$(git log --format=%H --diff-filter=A -- apps/dashboard-web/src/lib/tag-filter.ts | tail -1)   # the commit that added it
test "$(git rev-parse "$C1:apps/dashboard-web/src/lib/tag-filter.ts")" = 8229c71c708705c401eef02f6e240b3ef29de4b3   # blob = content hash
test "$(git show "$C1" | git patch-id --stable | cut -d' ' -f1)" = 291c89db6968ded46cc4d38045c0aa25589cf838      # same patch as 5e2fa3e
test "$(git diff-tree --no-commit-id --name-only -r "$C1")" = apps/dashboard-web/src/lib/tag-filter.ts             # nothing else in it
```

The **blob id** is the proof of byte identity: git names a blob by the SHA-1 of its exact bytes, so equal ids mean equal bytes. The
**patch-id** proves the commit's whole diff is the same (the same path added with the same content and mode). That is what lets a
rebase recognise the commit. The third line proves the commit carries nothing else, so dropping it drops nothing of ours.

Right before the PR the seat re-reads the sibling's current bytes, in case a repair round changed them:
`git fetch origin && git rev-parse origin/NurfitraPujo/sentinel-occurrence-detail-panels-implementing.sentinel:apps/dashboard-web/src/lib/tag-filter.ts`
(and `origin/main:…` if the file is on `main`). Each must print `8229c71c…`. If one prints another id, the sibling changed its
seed. The seat then re-copies that version into C1 (an interactive-free rewrite: reset C1's file, `git commit --fixup`, and
`GIT_SEQUENCE_EDITOR=true git rebase --autosquash`), re-runs §4, and reports it.

C9 **appends** to the file and never edits the seed's ten lines:
`diff <(git show 5e2fa3e:apps/dashboard-web/src/lib/tag-filter.ts) <(head -n 10 apps/dashboard-web/src/lib/tag-filter.ts)` is empty.
TASK-03's `tag-filter.test.ts` pins spec §6.1's href table, and the sibling's TG1 pins the same table. So the two runs cannot
disagree on an href without a red test.

### 5.2 The rebase, whichever PR merges second

- **This PR second** (the likelier order: the sibling is already implemented). `git fetch origin && git rebase origin/main`.
  - C1 adds a file that `main` already has with identical bytes. A squash merge of the sibling leaves the same content on `main`, so
    C1 applies as empty and is dropped (git's default `--empty=drop`). C0 goes the same way if it ran.
  - C9's append then applies on top of the identical seed.
  - `routes/issues/[id]/+page.svelte`: our panel sits between the trend section and `IssueRelations`, with its import beside
    `IssueRelations`'s. The sibling edits the script's `Occurrence` interface, imports under `SvelteSet`, and markup inside
    Occurrence History. The hunks do not overlap; an adjacent-context conflict keeps both sides.
  - `+page.server.ts`: our `getTagFacets` call after the access checks and a `tagFacets` return key; the sibling's block after
    `getOccurrencesByIssueId` and two return keys after `occurrences,`. Keep both.
  - **After the rebase, every sibling test file under `routes/issues/[id]/` that mocks `$lib/server/queries/issue-queries` gets the
    same one-line `getTagFacets: vi.fn(async () => [])`** as §7 A1. Otherwise the rebased loader throws in the sibling's tests.
    Find them with `grep -l "queries/issue-queries" "apps/dashboard-web/src/routes/issues/[id]/"*.test.ts`.
  - Then the whole §4 again, and a `git push --force-with-lease`.
- **This PR first.** The sibling drops its seed commit on its rebase (its plan §5.3), and its later commits only import from
  `$lib/tag-filter`. The sibling's plan §5.3 then adds `getTagFacets` to its own loader-test mocks. The seat's report says so
  upward, so that seat does it.

---

## 6. The migration id at PR time

Ids go stale between authoring and merge. Immediately before opening the PR, and again before any re-push after a rebase:

```bash
git fetch origin
LATEST=$(git ls-tree --name-only origin/main packages/db-migrations/migrations/ | sed 's|.*/||' | sort | tail -1)
OURS=$(ls packages/db-migrations/migrations/ | grep '_add_issue_tag_values.sql$')
echo "main newest: $LATEST   ours: $OURS"
# pass: OURS's numeric prefix is greater than LATEST's, and no file on main shares OURS's prefix
```

Two remote checks also run: open PRs (`gh pr list --state open --json number,headRefName`, then each PR's
`gh pr diff <n> --name-only | grep migrations/`), and the §2 step 3 loop over remote branches. Their ids cannot block the merge, but a
collision there is reported in the PR body, so the second author knows.

If `main`'s newest is ≥ ours: rebase first (§5.2), then `git mv` the file to the next free id (the next multiple of 100000 above
`main`'s newest) in a **new** commit, `chore(db): renumber tag-facets migration to <id>`. The tests find the file by its suffix and
derive both versions from the directory (C3), so the move is the whole change. `git grep -n 1725400000` then shows only prose
(`plan.md`, `spec.md` and the RUNBOOK's mention, which the commit updates). §4 runs in full again, because the migration now runs
after a different predecessor.

---

## 7. Where this plan adds to the spec (reported upward)

None of these changes behaviour. Each keeps a gate the spec requires from going red, or a proof from skipping.

- **A1: two existing test files break under the spec's own changes.** The spec's §9 does not list either.
  - `dw/src/routes/issues/[id]/page.server.test.ts` mocks `issueQueries` with five functions. The loader's new `getTagFacets` call
    makes every case throw. Remedy: one line added to its mock, in C8.
  - `dw/src/lib/server/retention.attachments.test.ts` mocks `$lib/server/db` as `{ db }` and calls `cleanupRetainedData(30)`. Once
    that opens its own run (C6), the call reaches the unmocked `openRetentionRunClient`. Remedy: pass a `run` built from the mock
    as the fourth argument; its R6 assertions stay unchanged. Done in C6.
- **A2: the migration proofs would skip under the gate.** Spec §10.2's Compatibility row adds `TestTagFacetsMigration_*` to
  `db_migrations_test.go`, whose `migTestDB` reaches `requireInfra` and skips without `SENTINEL_E2E=1`, which G3 does not set.
  Contract decision 5 forbids a proof that can skip. The four tests keep their names and assertions but live in
  `tests/integration/tag_facets_migration_test.go` on `tagFacetsDB`. `tag_facets_gate_test.go`'s glob then guards them too.
- **A3: G4 is red on the base.** C0 carries the sibling's identical debt commit, rather than weakening `pnpm check` to "no new
  errors".
- **A4: the inner loop.** The machine rule allows the container suite once, at the end. So Go integration tests are proven
  compile-red and SQL-red in the inner loop, and green in one final run (§2 step 5). The dashboard suites and every SQL mutation
  run red→green in the inner loop against one scratch Postgres.

Findings outside scope stay as spec §12 records them. The seat adds any new one to its report and does not fix it.

---

## 8. Production rollout: order, who enables what, post-deploy steps

**Order (spec §8.4).** One ArgoCD sync of the release does steps 1-2. Step 3 is a human action.

1. **The migration** runs in the migrate Job at sync **wave 1** (`migrate up` for processor, ingestor and dashboard, one ledger). It
   creates the table, the functions and the **triggers**. From the commit of its `TR` block, every insert from every pod version is
   counted and every delete is subtracted. Nobody enables this: it is live the moment the file commits. Hot-table DDL waits at most
   1.5 s per attempt (spec §8.1). The four `CONCURRENTLY` builds take several minutes on production's heap and block no reads or
   writes.
2. **Wave 2 rolls the processor and the dashboard.** Nothing to enable here either:
   - The processor's only change is the subcommand and a comment.
   - The dashboard's panel and search go live, showing a partial rollup until step 3 finishes.
   - **Retention's new behaviour** (paced issue-locked batches, the per-run connection, the drift report, deferred rebuilds, the
     `sentinel_retention_runs_total` counter) is live with the new dashboard pods. No flag gates it.
   - The same sync updates the retention CronJob's curl timeout to 180 s and its `activeDeadlineSeconds` to 600. In production it
     deletes nothing before **~2026-10-14 17:00 UTC** (30-day retention; oldest row 2026-09-14). Until then only its connection,
     lock and reaper paths run.
3. **The backfill.** The chart ships `sentinel-sentinel-tag-facets-backfill` as a **CronJob that never runs by itself**: `suspend: true`
   is a template literal (no overlay can switch it on), the schedule is 31 February, and it has no hook or sync-wave annotation.
   **Nobody unsuspends it.** The **operator** (the person deploying, with `kubectl` on the cluster) starts one run per environment
   with the P2 command: `kubectl -n sentinel-<env> create job --from=cronjob/sentinel-sentinel-tag-facets-backfill
   tag-facets-backfill-$(date +%s)`, after checking no such Job is `Running`. After P2 passes, an overlay **may** set
   `tagFacetsBackfill.enabled: false` in a later values change, which removes the CronJob and its Jobs. That is optional and not
   part of this PR.
4. **Alerts.** The `sentinel-retention` rule group renders only under `prometheusRule.enabled`, which production keeps `false`. No one
   enables it in this change. Until the "Load Sentinel rule groups into the live Prometheus" card lands, P5's hand-read of
   `sentinel_retention_runs_total` and the Job list stands in for it.

**Post-deploy steps, in order** (spec §10.3). The operator runs them on **staging first**, and on production only after staging passes
P1-P4, P6 and P7:

| Order | Step | When | Pass (spec §10.3) |
|---|---|---|---|
| 1 | **P1** | right after the sync | five `trg_error_occurrences_tag_rollup_*` triggers; a new occurrence has `tag_rollup_applied = true` and rollup rows; the five indexes `indisvalid`; build times and bounded-wait retries recorded; the Application `Healthy`, the CronJob with no `health` field (ArgoCD v2.9.6) or `Healthy` |
| 2 | **P2** | right after P1 | the backfill Job ends `tag_facets.backfill.done`; `num_pending` ≤ 100 in every 30 s sample (else delete the Job, re-create it with a larger `-pause`, smaller `-duty` or smaller `-batch`; the cursor is committed); `batch_ms_*` and the `StoreEvent` p99 recorded. ~1.4 h at today's 4.4 M rows |
| 3 | **P3** | after P2's `done` | the tag-search result and count plans as spec §6.4 "P3 passes when", and the four filter cases; each query < 100 ms |
| 4 | **P4** | after P2's `done` | the check query over the 20 largest issues in `REPEATABLE READ`, all zeros; otherwise `issue_tag_values_rebuild(<id>)` and a finding |
| 5 | **P6** | a 24 h window starting after `done` | `sentinel_process_duration_seconds` p99 up < 5 ms against the 24 h before the deploy |
| 6 | **P7** | 24 h after `done`, staging | `n_tup_hot_upd / n_tup_upd ≥ 0.9`, autovacuum within the day, size as spec §3.1 |
| 7 | **P5** (then **P4 again**) | from the **first retention run with `deletedOccurrences > 0`**, for 24 hourly runs: staging when its oldest occurrence passes 30 days; production from ~2026-10-14 18:00 UTC, a window that includes the 2026-10-15 02:00 UTC run deleting the peak hour (34,481) | the keep-up inequality with *A* re-read before and after; `occurrenceStopped = 'drained'` and backlog ≤ 7,200 s every run (the one DLQ-replay exemption); `occurrenceHeldP95Ms` ≤ 150; `num_pending` ≤ 100; route ≤ 159 s; no lock-timeout, batch-reduced, delete-timeout, deadline or stale lines; `completed` counter rising, others flat; no drift. **Never suspend retention on a P5 failure**: apply the schedule lever (spec §5) |

The operator writes the deploy date into the RUNBOOK's tag-facets section at the deploy, so P5's start is known.

**Rollback** (spec §8.4): roll the dashboard and processor back first, then run the migration's Down (bounded-wait trigger drop,
functions, the five indexes `CONCURRENTLY`, the three tables, the column). Leaving the migration under the older application is also
safe: the triggers keep the rollup exact, and nothing else reads it.

---

## 9. Reporting

- **PR body:** the cards; the commit list, each with its red→green or mutation evidence; §4's outputs (steps 2, 5 and 9 verbatim);
  the §5.1 identity checks and the §6 id check as run; the list of CI jobs not run on a PR (§4); §7's additions; §8's rollout order
  and P-steps.
- **Upward, not as cards:** §7 A1-A4. Tell the sibling planning seat that its rebase must add `getTagFacets` to its loader-test mocks
  when this PR merges first (§5.2). Spec §12's findings stay recorded, not fixed.
- **Outcome:** `COMPLETED` only with every §4 step passed on the pushed final commit, step 9 with no skipped proof. A red gate the
  seat cannot fix inside §1.1 is `FAILED`, naming the gate. A needed path outside §1.1 is escalated, not written.
