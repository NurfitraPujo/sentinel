# Sentinel occurrence detail panels: implementation plan

**Feature:** `sentinel-occurrence-detail-panels` · **Repository:** `sentinel` (`tuju-ai/sentinel`) · **Base:** `origin/main`, at
planning time `f49721f` (v0.2.37)
**Built from:** `sentinel-occurrence-detail-panels/spec.md` @ `5bbf266` on `NurfitraPujo/sentinel-occurrence-detail-panels-planning`.
That is the reviewed spec (`010ad64`, four review rounds, 0 blocking findings) plus the round-4 note `odp-sr4-pr-ci-skips-check-test`,
folded into §12.1 and §15 by this planning seat and changing nothing else. Contract `prd/sentinel-occurrence-detail-panels/contract.md`
and `fe.md` @ `bc275e6` (decisions 1-7). If this plan and the spec disagree, **the spec wins**, and the seat reports the conflict
upward. It does not resolve it silently.
**Cards:** `sentinel-occurrence-detail-panels/TASK-01` … `TASK-05`. **One seat, one pull request.**
**Tier:** standard, side `fe`. Every path below is under `apps/dashboard-web/` or this spec folder (§1.1). There is no migration,
processor, ingestor, chart or agent API change (spec §2, §10.3).

Citations below use `dw/` for `apps/dashboard-web/`, as the spec does. "Red before green" means the spec's CLAUDE.md convention: every
test is seen to fail, either because the production code does not exist yet or because the spec's §12.3 "Mutation" is applied, and
the commit body records which.

---

## 1. Seat

There is **one** implementing seat. The five cards cannot be split between seats without sharing paths:
- `dw/src/routes/issues/[id]/+page.svelte` is edited by every card: each panel is placed inside the same `occurrenceCard` snippet (spec
  §4.5 order), and TASK-04 creates that snippet.
- `dw/src/lib/components/issues/occurrence-display.ts`, `occurrence-fixtures.ts` and `ExpandableText.svelte` serve all five cards
  (spec §4.5, §12.2).
- `StackFrames.svelte` (TASK-04) is TASK-05's frame renderer (spec §9.2).
- The page-level proofs (TR1's 44 buttons, U1, SSR2, P1) need every panel at once.

So the cards run as a **sequence of commits inside one seat** (§3).

| | |
|---|---|
| **Seat** | `sentinel-occurrence-detail-panels-implementing` |
| **Repository** | `sentinel` (`tuju-ai/sentinel`) |
| **Branch** | `NurfitraPujo/sentinel-occurrence-detail-panels-implementing`, cut from `origin/main` after `git fetch origin`. Its first action is to merge `origin/NurfitraPujo/sentinel-occurrence-detail-panels-planning` (docs only: `spec.md`, `plan.md`, `payload-fixture.mjs`, `measure-payload.mjs`), so the PR carries the spec it implements. |
| **Cards** | TASK-01, TASK-02, TASK-03, TASK-04, TASK-05 |
| **Depends on** | nothing inside this run. It runs **in parallel** with `sentinel-issue-tag-facets` (contract decision 3); §5 is the interface. |
| **Pull request** | one, against `main`, titled `feat(dashboard): occurrence detail panels — contexts, breadcrumbs, user/request/tags, header and frames, exception chain (sentinel-occurrence-detail-panels)` |
| **Gate** | §4, run in that order on the final commit, with the output quoted in the PR body and the report |

### 1.1 Paths the seat may write (the whole change's surface)

```
dw/src/lib/db/queries/organizations.ts                        C1 only: the `users` import (spec §11.1, T30, O8)
dw/src/lib/db/queries/organizations.members.test.ts   (new)   C1: G3a
dw/src/routes/+page.svelte                                    C1 only: `untrack` at :162 (the issue LIST page)
dw/src/lib/tag-filter.ts                              (new*)  C2: the shared encoder seed (§5); *only if absent from origin/main
dw/package.json, dw/pnpm-lock.yaml                            C3: devDependencies devalue 5.8.0, @types/jsdom 21.1.7 (exact)
dw/src/lib/server/queries/issue-queries.ts                    C3: light columns, detail projection, two reads, :261-264 comment
dw/src/lib/server/queries/issue-queries.occurrences.test.ts   C3: rewritten (Q1-Q4, supersedes V7e)
dw/src/lib/server/json-safe.ts                        (new)   C3
dw/src/lib/server/json-safe.test.ts                   (new)   C3: J1
dw/src/routes/issues/[id]/+page.server.ts                     C3: `url`, S1-S4, cleanOccurrenceRow
dw/src/routes/issues/[id]/page.server.occurrence-detail.test.ts (new)  C3: L1-L12
dw/src/lib/components/issues/occurrence-display.ts    (new)   C3, grown by C4-C8
dw/src/lib/components/issues/occurrence-display.test.ts (new) C3, grown by C4-C8
dw/src/lib/components/issues/occurrence-fixtures.ts   (new)   C3 (test-only module)
dw/src/lib/components/issues/occurrence-fixtures.test.ts (new) C3: M1
dw/src/lib/components/issues/ExpandableText.svelte    (new)   C3
dw/src/lib/components/issues/ExpandableText.test.ts   (new)   C3: U3
dw/src/lib/components/issues/html-structure.ts        (new)   C3 (test-only module)
dw/src/lib/components/issues/html-structure.test.ts   (new)   C3: N1-N4
dw/tests/fixtures/issue-page-pre-v2-cards.golden.html (new)   C3: the P4 golden, captured from the merge base
dw/src/routes/issues/[id]/page.occurrence-compat.test.ts (new) C3: P4
dw/src/routes/issues/[id]/+page.svelte                        C4-C8 (spec §4.5 "Page edits are confined to")
dw/src/lib/components/issues/OccurrenceHeader.svelte, StackFrames.svelte (+ .test.ts)          (new)  C4
dw/src/routes/issues/[id]/page.occurrence-panels.test.ts (new)  C4, grown by C5-C9
dw/src/routes/issues/[id]/page.occurrence-ssr.test.ts    (new)  C4, grown by C5-C9
dw/src/lib/components/issues/TagsTable.svelte, UserPanel.svelte, RequestPanel.svelte (+ .test.ts)  (new)  C5
dw/src/lib/components/issues/ExceptionChain.svelte (+ .test.ts)                                (new)  C6
dw/src/lib/components/issues/BreadcrumbTimeline.svelte (+ .test.ts)                            (new)  C7
dw/src/lib/components/issues/ContextCards.svelte (+ .test.ts)                                  (new)  C8
dw/src/routes/issues/[id]/page.occurrence-truncation.test.ts (new)  C9: TR1
dw/src/lib/components/issues/occurrence-untrusted.test.ts    (new)  C9: U1, U2
sentinel-occurrence-detail-panels/{spec.md,plan.md,payload-fixture.mjs,measure-payload.mjs}  carried from planning, NOT edited
```

**Paths the seat may not write:** everything else. In particular `dw/src/lib/server/agent-api-spec/**`,
`dw/src/lib/db/queries/agent-reads.ts`, `dw/src/routes/api/agent/**` (spec §10.3, G5), every existing
`dw/src/routes/issues/[id]/*.test.ts` (spec §10.7: no loader or page test is edited; P5), `dw/src/app.css` (no new CSS variable,
spec §2), `dw/src/routes/search/**` (tag facets owns it), and anything outside `apps/dashboard-web/` and this folder (G5). The
**one** exception, spelled out in spec §6.3: if tag facets merged first and its `tag-filter.ts` builds `tagSearchHref` with
`URLSearchParams`, the seat changes that one line to the normative form, in C5 (§5.2).

No route file gains an export other than `load` (B12). `html-structure.ts` and `occurrence-fixtures.ts` are imported only by tests.

---

## 2. Before the first commit

1. `git fetch origin`, cut the branch from `origin/main`, merge the planning branch (§1). Record `BASE=$(git merge-base origin/main
   HEAD)`; every diff and the golden use it, never a pinned commit (spec header, decision 21).
2. Check whether `sentinel-issue-tag-facets` has merged: `git ls-tree origin/main -- apps/dashboard-web/src/lib/tag-filter.ts`. If it
   prints a line, **C2 is skipped** (§5.2).
3. Check whether the §11.1 debt is still there: `grep -n "users" dw/src/lib/db/queries/organizations.ts | head -3` and
   `sed -n 162p dw/src/routes/+page.svelte`. If someone fixed either part, C1 drops that part and its body says so (spec §11.1).
4. `cd apps/dashboard-web && pnpm install --frozen-lockfile`, then `pnpm check` and record the base's result. At `f49721f` it is
   `5 errors and 1 warning in 2 files` (T30). This is the "red" of C1.
5. Use a **private Go build cache** for every command that runs Go (`task build`, `task test-unit`):
   `env GOCACHE=$HOME/.cache/go-build-sentinel-odp-impl …`. Never the shared default cache.
6. **No container suite runs in this seat.** Nothing in this plan needs Postgres, NATS, the compose stack or testcontainers
   (spec §12.1: every proof mocks the database; `task test-integration` is not needed). Do not set `DATABASE_URL`, do not start
   `docker compose`/`podman`. The dashboard's real-Postgres suites (`schema-drift`, `retention.*.integration`,
   `issue-queries.*.integration`) skip under `task test-unit` without a database, exactly as at the base (T23). This laptop has
   kernel-panicked while podman started many containers; if the seat ever finds it must run a container suite, that is outside
   this plan: it escalates instead, and any container run happens **once, alone, at the very end**, only after `podman ps` shows no
   other lane's testcontainers running.

---

## 3. Ordered steps and commits

Push after **every** commit (`git push`, the first one with `-u origin HEAD`). Each commit keeps the dashboard green: before each
push the seat runs, in `apps/dashboard-web`, `pnpm check` (must be `0 errors and 0 warnings` from C1 on) and `pnpm test`. The full
§4 gate runs on the final commit. Fast inner loop: `pnpm vitest run <files>`.

Each card commit is TDD: the named test files are written first and run red (component or function missing, or the named
assertion failing), then the production code turns them green. A test that cannot be red first (a regression guard, or a
cross-panel test written after its panels exist) is proven by applying the spec's §12.3 "Mutation" to the production code, seeing
red, and reverting; the commit body lists each mutation run and the failure it produced.

### C1 — base `pnpm check` debt (spec §11.1 commit 1, T30, O8, G3a). **First, on its own.**

`fix(dashboard): import users in organizations.ts; untrack list-page searchQuery`

| Files | Exactly three: `dw/src/lib/db/queries/organizations.ts` (add `users` to the `$lib/db/schema` import block `:2-10`, nothing else), `dw/src/routes/+page.svelte` (`import { untrack } from 'svelte'`; `let searchQuery = $state(untrack(() => data.filters.q ?? ''))` at `:162`; the `$effect` at `:163-165` unchanged), new `dw/src/lib/db/queries/organizations.members.test.ts` |
|---|---|
| Red | G3a written first against the unfixed file: `ReferenceError: users is not defined`. `pnpm check` at the base: 5 errors, 1 warning. |
| Green | G3a passes; `pnpm check` → `0 errors and 0 warnings`; `pnpm test` passes (the list page's 4 test files, 69 tests, included). |
| Rule | No issue-page file in this commit. Its body names finding `odp-sr3-g3-base-red` and states it is pre-existing debt from `e91b07e` (#52) cleared because G3 needs it. |

### C2 — the shared tag-link encoder seed (spec §6.3, contract decisions 6-7). **Skipped if `tag-filter.ts` is on `origin/main`.**

`feat(dashboard): shared tag-link encoder (tag-filter.ts) for occurrence panels and tag facets`

| Files | Exactly one: new `dw/src/lib/tag-filter.ts`, **byte-identical to §5.1's seed**. Nothing else in the commit, so the rebase in §5.3 can drop it cleanly. |
|---|---|
| Proof | TG1 and TG3 (C5) test it: TG1 calls `tagSearchHref` directly for the seven pinned strings. In C5 the seat applies both TG1 mutations to this file (encode the joined string once; build with `URLSearchParams`) and sees TG1 red on the rows the spec names, then reverts. |

### C3 — foundation: data read, `__proto__` safety, shared primitives, the golden (spec §3, §4.1-§4.4, §10, §12.2)

`feat(dashboard): occurrence detail read — light columns, selected-occurrence detail, proto-key-safe page data`

| Step | Files | Tests (red first) | Criteria |
|---|---|---|---|
| a | `pnpm add -D -E devalue@5.8.0 @types/jsdom@21.1.7` → `dw/package.json`, `dw/pnpm-lock.yaml` (spec §2: devalue adds an importer entry and no package; @types/jsdom adds `@types/jsdom@21.1.7` and `@types/tough-cookie@4.0.5`) | — | lets L11 import `devalue`, G3 type-check `html-structure.ts` |
| b | `issue-queries.ts`: the six light columns, `issuePageOccurrenceDetailColumns`, `getOccurrenceDetail`, `occurrenceExistsOutsideIssue`, the `:261-264` comment (spec §3.1) | `issue-queries.occurrences.test.ts` rewritten: Q1-Q4 | §3.1 |
| c | `json-safe.ts`: `dropProtoKeys`, `cleanOccurrenceRow` (spec §3.4) | `json-safe.test.ts`: J1 | §3.4, §10.5 |
| d | `+page.server.ts`: `load: async ({ params, locals, url })`, `url?.searchParams.get('occurrence') ?? null`, `UUID_RE`, S1-S4 in the spec's order **after** both access checks and after `occurrences` is read, `selectedOccurrenceId`, `occurrenceDetail`, `cleanOccurrenceRow` on every returned row (spec §3.2, §3.4) | `page.server.occurrence-detail.test.ts`: L1-L12 | §3.2, §10.6, §10.7 |
| e | `occurrence-display.ts` first slice: `INLINE_LIMIT`, `BLOCK_LIMIT`, `hasV2Fields`, `formatValue`, `clip`, `levelTone` (a `Map`) | `occurrence-display.test.ts`: `formatValue`/`clip` table, U4's `levelTone` half | §4.1, §4.2 rules 4-5, §4.4 |
| f | `ExpandableText.svelte` (exact inline/block markup, spec §4.3) | `ExpandableText.test.ts`: U3 | §4.3 |
| g | `occurrence-fixtures.ts`: every §12.2 fixture (`FLUTTER_*` copied **verbatim** from `payload-fixture.mjs`, `OCCURRED_AT`, `loaded`, `PRE_V2_*`, `V2_BARE_*`, `inventoryOccurrence`, `HOSTILE_*`, `LONG_*` + `LONG_EXPANDERS`, `PROTO_KEY_ROWS`, `JS_CHAIN_DETAIL`, `V2_JS_*`, `MALFORMED_FRAMES`, `V2_MALFORMED_OCCURRENCE`) | `occurrence-fixtures.test.ts`: M1 | §3.3 (G7 matches M1) |
| h | `html-structure.ts`: `normaliseCardHtml` (the four steps, spec §12.2), `tagSequence`, `reparse` (typed `import { JSDOM } from 'jsdom'`, no `any`) | `html-structure.test.ts`: N1-N4 | §4.6, §10.1 |
| i | the golden, captured **before any `+page.svelte` edit**, by spec §12.2's four steps from `git show "$BASE":…/+page.svelte`, under `process.env.TZ = 'UTC'`, first line `<!-- captured from <full $BASE sha> -->`; the capture harness and `Baseline.tmp.svelte` are deleted and never committed (`git status` clean) | `page.occurrence-compat.test.ts`: P4 (first statement `process.env.TZ = 'UTC'`, asserts `new Date(0).getTimezoneOffset() === 0`) | §10.1, §10.2 |

P4 is green here by construction (the page is untouched); it is the guard every later page edit runs under. Its mutation (the
facts block without the v2 gate) is run in C4. P5 (the six existing `issues/[id]` test files, `page.server.test.ts`'s `url`-less
`loadAs` included) must pass unedited from this commit on.

### C4 — TASK-04: per-occurrence header fields and richer frames (spec §5, §3.2 page side)

`feat(dashboard): occurrence header facts and frame renderer (TASK-04)`

| Step | Files | Tests (red first) |
|---|---|---|
| a | `occurrence-display.ts`: `formatStackFrame` and `displayedFrame` **moved verbatim** from `+page.svelte:48-57`, `:78-80`; `frameObjects`, `frameLine`, `frameVisibility`, `sourceContextLines` | `occurrence-display.test.ts`: F3 |
| b | `OccurrenceHeader.svelte` | `OccurrenceHeader.test.ts`: H1-H3, TR7 |
| c | `StackFrames.svelte` | `StackFrames.test.ts`: F1, F2, F4, F5, F7, TR4 |
| d | `+page.svelte`: the six light keys on `Occurrence` (optional); the new imports **as one block directly under `import { SvelteSet } from 'svelte/reactivity';`** (not beside `IssueRelations`, where tag facets adds its import, §5.4); `formatStackFrame`/`displayedFrame` removed; the card markup moved into `{#snippet occurrenceCard(occurrence)}`; on a v2 card: select link or marker + `aria-current` (spec §3.2), `OccurrenceHeader` (item 2), `StackFrames` in the existing `Stack Trace` block with the badge from `frameObjects(…)`; the S3a `selected-outside-window` block; absent `selectedOccurrenceId`/`occurrenceDetail` treated as `null` | `page.occurrence-panels.test.ts`: F6, P2, P3, P7, P8; `page.occurrence-ssr.test.ts` created: SSR1 for `ExpandableText` (4 forms), `OccurrenceHeader`, `StackFrames`; SSR2 (b) and (d); SSR3 |

After step d: P4 still green (spec §10.4); P4's mutation (facts without the v2 gate) run and seen red. F7's and SSR2 (d)'s
mutation (format the stored element) seen red.

### C5 — TASK-03: user, request and tags panels (spec §6)

`feat(dashboard): user, request and tags panels (TASK-03)`

| Step | Files | Tests (red first) |
|---|---|---|
| a | `occurrence-display.ts`: `geoLine` | `occurrence-display.test.ts`: `geoLine` cases |
| b | `UserPanel.svelte` | `UserPanel.test.ts`: US1, TR8 (user half) |
| c | `RequestPanel.svelte` (no `<a>`; headers sorted; `<tbody>`) | `RequestPanel.test.ts`: R1-R3, TR2 |
| d | `TagsTable.svelte`, importing `tagSearchHref` from `$lib/tag-filter` (never redefined) | `TagsTable.test.ts`: TG1 (incl. the `fs` read of the import line and the seven direct calls), TG2, TG3, TR8 (tags half); plus C2's two mutations on `tag-filter.ts` |
| e | `+page.svelte`: `TagsTable` at item 4, `RequestPanel` at item 8, `UserPanel` at item 9, selected v2 card only, from `occurrenceDetail` | `page.occurrence-ssr.test.ts`: SSR1 rows for the three panels |

### C6 — TASK-05: exception chain (spec §9)

`feat(dashboard): exception chain, outermost first (TASK-05)`

| Step | Files | Tests (red first) |
|---|---|---|
| a | `occurrence-display.ts`: `chainDisplayOrder` | `occurrence-display.test.ts`: order case |
| b | `ExceptionChain.svelte` (uses `StackFrames` with `showRaw={false}`; `Not symbolicated` for `javascript`/`browser`; collapse at display index ≥ 3) | `ExceptionChain.test.ts`: X1-X3, TR6 |
| c | `+page.svelte`: `ExceptionChain` at item 6, below the kept top-level `Stack Trace` | `page.occurrence-panels.test.ts`: X4; SSR1 row for `ExceptionChain` |

### C7 — TASK-02: breadcrumbs timeline (spec §8)

`feat(dashboard): breadcrumbs timeline with filters (TASK-02)`

| Step | Files | Tests (red first) |
|---|---|---|
| a | `occurrence-display.ts`: `parseCrumbTime`, `anchorMs`, `formatRelative` | `occurrence-display.test.ts`: B5 |
| b | `BreadcrumbTimeline.svelte` (`Map`/`Set` filters; `data` deferred until expanded; last-100 guard) | `BreadcrumbTimeline.test.ts`: B1-B4, B4b, B6, B7, TR5, U4's crumb half |
| c | `+page.svelte`: `BreadcrumbTimeline` at item 7 with `occurredAt={occurrenceDetail.createdAt}` passed **as loaded, never converted** | SSR1 row for `BreadcrumbTimeline` |

### C8 — TASK-01: context cards (spec §7)

`feat(dashboard): context cards (TASK-01)`

| Step | Files | Tests (red first) |
|---|---|---|
| a | `occurrence-display.ts`: `contextCards` (titles, order, field lists, headline rules all `Map`s) | `occurrence-display.test.ts`: U4's `contextCards` part |
| b | `ContextCards.svelte` (`context-scalar` a `<div>`) | `ContextCards.test.ts`: C1-C6, TR3 |
| c | `+page.svelte`: `ContextCards` at item 10, before the metadata toggle | SSR1 row for `ContextCards` |

### C9 — cross-panel proofs (spec §4.3, §4.7, §4.6, the §1 hypothesis)

`test(dashboard): cross-panel proofs — truncation inventory, untrusted content, SSR nesting, hypothesis (TASK-01..05)`

Tests only, unless one goes red and exposes a defect, which the seat fixes in the same commit and names in its body.

| Tests | Proven red by (spec §12.3 Mutation) |
|---|---|
| `page.occurrence-truncation.test.ts`: TR1 (44 buttons, per-panel counts) | one inventory field rendered as a bare `{value}` |
| `occurrence-untrusted.test.ts`: U1 (1)-(6), U2 | `{@html value}` for one field; `request.url` as `<a href>`; a field dropped from a panel |
| `page.occurrence-ssr.test.ts`: SSR2 (a) hostile and (c) S3a | the message container back to `<p>` |
| `page.occurrence-panels.test.ts`: P1 (`loaded(FLUTTER_*)`, document order, crumb 96 `-3.6s`) | anchor with `parseCrumbTime(occurredAt)` only; `occurredAt={String(createdAt)}` |

---

## 4. Gates the seat runs before reporting (final commit, in this order)

From the repository root unless stated. `GOCACHE` private as in §2.5. Every command's exit status and summary line go into the
report, and G1, G3 and G4's output into the PR body (spec §12.1, round-4 note).

| # | Command | Must show |
|---|---|---|
| G0 | `env GOCACHE=$HOME/.cache/go-build-sentinel-odp-impl task build` | exit 0 (Go binaries + `build-dashboard`: `pnpm install` + `pnpm build`); then `git status --porcelain` is empty (the install must not rewrite the lockfile) |
| G1 | `env GOCACHE=$HOME/.cache/go-build-sentinel-odp-impl task test-unit` | exit 0. Its dashboard line is `pnpm --dir apps/dashboard-web test` (vitest run): every component, page, loader and helper test of §3 runs here, and none skips; only the pre-existing real-Postgres suites skip (T23) |
| G2 | `cd apps/dashboard-web && pnpm build` | exit 0 (route-export allowlist, B12; also inside G0) |
| G3 | `cd apps/dashboard-web && pnpm check` | `svelte-check found 0 errors and 0 warnings`. **`task test-unit` does not run `pnpm check`**; this is a separate gate |
| G4 | `cd apps/dashboard-web && pnpm test --sequence.shuffle` | exit 0; record the seed printed. Run it twice (two seeds) since order dependence is probabilistic (B13) |
| — | `cd apps/dashboard-web && pnpm vitest run src/lib/components/issues src/lib/server/json-safe.test.ts src/lib/server/queries/issue-queries.occurrences.test.ts 'src/routes/issues/[id]' src/lib/db/queries/organizations.members.test.ts` | the component, loader and page tests of this feature in isolation, with the six existing `issues/[id]` files (P5): all pass, none skipped |
| G5 | `git fetch origin && BASE=$(git merge-base origin/main HEAD)`; `git diff --stat "$BASE" -- ':!apps/dashboard-web' ':!sentinel-occurrence-detail-panels'` and `git diff "$BASE" -- apps/dashboard-web/src/lib/server/agent-api-spec apps/dashboard-web/src/lib/db/queries/agent-reads.ts apps/dashboard-web/src/routes/api/agent` | both empty |
| G6 | `grep -rn '{@html' apps/dashboard-web/src/lib/components/issues apps/dashboard-web/src/routes/issues` | prints nothing (U2 asserts it too) |
| G7 | `node sentinel-occurrence-detail-panels/measure-payload.mjs` | prints the spec §3.3 tables, section 3 (`devalue`) included now that `devalue` is a devDependency, with no `DEVALUE_PATH` |
| — | `git diff --name-only "$BASE" -- 'apps/dashboard-web/**/*.test.ts' \| xargs grep -nE '\b(it\|test\|describe)\.(skip\|todo)\b\|skipIf'` | prints nothing (spec §12.1: no new test can skip) |
| — | `git diff --name-only "$BASE" -- 'apps/dashboard-web/src/routes/issues/[id]/*.test.ts'` | lists only the new `page.*occurrence*` files: no existing loader or page test edited (spec §10.7) |

`task test-integration` is **not** run: the spec needs it for nothing (§12.1, "Nothing in this feature crosses a process boundary"),
and §2.6 forbids container suites in this seat.

**What PR CI proves** (spec §12.1, round-4 note `odp-sr4-pr-ci-skips-check-test`). On `pull_request`, the dashboard job runs only
`pnpm build`: `ci.yml:308`, `:323` and `:330` skip the migrations step, `pnpm check` and `pnpm test`. So the PR's checks prove G2
only. G1, G3 and G4 are proven by the runs above, quoted in the PR body. The PR body also names a **post-merge check**: after
merge, `gh run list --branch main --event push --limit 1` and the dashboard job's result on that push run, which is the first CI
run of `pnpm check`/`pnpm test` since `e91b07e` (#52). A failure there in a Postgres-backed suite (`schema-drift`,
`retention.*.integration`, `issue-queries.*.integration`) is triaged against #51/#53 first.

### 4.1 Criterion → step

| Criterion | Proof | Step that makes it pass |
|---|---|---|
| §3.1 projections / §4.1 contract query | Q1-Q4 | C3b |
| §3.2 selection S1-S4, access order, no `url` | L1-L10, L12 | C3d |
| §3.4 no `__proto__` in page data / §10.5 | J1, L11 | C3c, C3d |
| §3.3 payload decision | M1, G7 | C3g |
| §4.3 `ExpandableText` | U3 | C3f |
| §4.3 truncation in every panel | TR7, TR4 (C4); TR2, TR8 (C5); TR6 (C6); TR5 (C7); TR3 (C8); TR1 (C9) | C4-C9 |
| §4.2 rule 5 prototype keys | U4 (C3e `levelTone`, C7 crumbs, C8 `contextCards`), C6 | C3, C7, C8 |
| §4.6 nesting under SSR | N1-N4 (C3h); SSR1 grown per card (C4-C8); SSR2 (b)(d), SSR3 (C4); SSR2 (a)(c) (C9) | C3-C9 |
| §4.7 untrusted content | U1, U2, G6 | C9 (every panel escapes by construction from C4 on) |
| TASK-04 header | H1-H3 | C4b |
| TASK-04 frames, badge and raw toggle | F1-F7 | C4a, C4c, C4d (F6 on the page) |
| TASK-03 user / request / tags, exact href | US1, R1-R3, TG1-TG3 | C5 (C2's encoder) |
| TASK-05 chain, outermost first | X1-X4 | C6 |
| TASK-02 breadcrumbs, filter, expand, relative time | B1-B7, B4b, B5 | C7 |
| TASK-01 context cards | C1-C6 | C8 |
| §1 hypothesis (every panel, production `Date` shape) | P1 | C9 (panels from C4-C8) |
| selection UI, S3a block | P2, P3, P7, P8 | C4d |
| §10.1 / §10.2 / §10.4 pre-v2 card unchanged | P4 | C3i (golden), held through C4-C8 |
| §10.3 agent API unchanged | G5, P6 | every commit (never touched) |
| §10.6 unknown / malformed param | L9, L10 | C3d |
| §10.7 existing tests unedited | P5 | C3d onward |
| §11.1 / T30 base debt | G3a, G3 | C1 |
| §12.1 G1-G7 | §4 | final commit |

---

## 5. The parallel run: `sentinel-issue-tag-facets` (contract decision 3)

### 5.1 The shared encoder

- **File:** `apps/dashboard-web/src/lib/tag-filter.ts`, exporting `tagSearchHref(key: string, value: string): string` (contract
  decision 7; tag-facets spec §6.1 and §11.3). `TagsTable.svelte` imports it from `$lib/tag-filter`. No other file in either run
  defines a tag-link encoder.
- **Which run creates it:** whichever run's PR merges **first** (decision 7). Tag facets is a larger, multi-surface change (migration,
  processor, backfill job, search) still in spec review at planning time, so this run is expected to merge first and create it in
  C2. If tag facets merges first, C2 is skipped and C5 imports its module.
- **The seed.** C2's file is exactly this, byte for byte (tabs, a trailing newline), so that both runs can carry the identical seed:

```ts
// The one tag-link encoder, shared by sentinel-occurrence-detail-panels (spec §6.3) and sentinel-issue-tag-facets
// (spec §6.1). Contract decisions 6 and 7: each part is encoded before the join, the joined string is encoded again
// as one query value, and URLSearchParams is never used to build the href.
export function formatTagParam(key: string, value: string): string {
	return encodeURIComponent(key) + ':' + encodeURIComponent(value);
}

export function tagSearchHref(key: string, value: string): string {
	return '/search?tag=' + encodeURIComponent(formatTagParam(key, value));
}
```

### 5.2 If tag facets merges first

C2 is skipped; this branch never adds `tag-filter.ts`, so rebasing onto `main` has no conflict on it. C5's TG1 runs against tag
facets' module. If TG1's `it's (x)!~` row is red because that module builds the href with `URLSearchParams`, C5 changes that one
line to the normative form (spec §6.3; the file is inside `apps/dashboard-web`, so G5 allows it) and says so in its body.

### 5.3 How the second PR rebases without a conflict

- **On `tag-filter.ts`.** The seed lives in a commit that contains only that file.
  - *This run second:* `git fetch origin && git rebase origin/main`. Git sees C2 add a file that `main` already has. Whether `main`'s
    copy is still the bare seed or already carries tag facets' appended parser, C2 applies as empty and rebase drops it (simulated
    at planning with git 2.55.0 in a scratch repository: both cases rebase clean, and the file ends as `main`'s). If a git version
    instead stops on an add/add conflict in that one file, the seat runs `git checkout --ours --
    apps/dashboard-web/src/lib/tag-filter.ts` (during a rebase `--ours` is `main`), `git add` it, and `git rebase --skip`, since
    the commit is then empty. Nothing else in the branch touches the file (C5 only imports it), so no later commit conflicts on it.
  - *Tag facets second:* conflict-free on this file **if** its plan also adds the §5.1 seed byte for byte in a commit of its own
    and appends `parseTagParam`, `TAG_KEY_PATTERN` and the rest in later commits. The identical add then applies as empty, and its
    appends apply as ordinary edits (simulated at planning: clean). Otherwise the conflict is confined to that one new file, and it is resolved by keeping
    `main`'s two functions unchanged and adding its own exports. The seat reports this upward so the tag-facets planning seat can
    adopt the seed (§6).
- **On `issues/[id]/+page.svelte`.** Tag facets inserts its panel between the trend section and `IssueRelations` (its spec T14, §7.1),
  and its import beside `IssueRelations`. This run edits only the script's `Occurrence` interface, removes `formatStackFrame` and
  `displayedFrame`, adds its imports directly under `SvelteSet` (C4d), and edits the markup inside the Occurrence History section
  (spec §4.5). The hunks do not overlap. If git still reports one (adjacent context), both sides are kept: each run's lines are
  additive.
- **On `issues/[id]/+page.server.ts`.** This run adds its block directly after `const occurrences = await
  issueQueries.getOccurrencesByIssueId(issueId);` and its two return keys directly after `occurrences,`. Tag facets adds
  `getTagFacets` after the access checks and its own return key. Any conflict is adjacent-context only and resolved by keeping
  both. Tag facets' `page.tag-facets.test.ts` calls `load` without a `url`; `url?.searchParams` makes that S1 (spec §3.2, L12),
  so it passes unedited (P5).
- **After a rebase onto a `main` that moved,** the seat recomputes `BASE`, re-runs P4, and if the pre-v2 card changed on `main`,
  re-captures the golden by spec §12.2's steps in the same PR (spec §12.2). Tag facets' panel sits outside the occurrence card, so
  the golden should not change. Then the whole §4 gate runs again.

---

## 6. Reporting

- The PR body carries: the card list, the commit list with each commit's red→green or mutation evidence, G0-G7's output (G1, G3, G4
  verbatim), the post-merge check (§4), and the §5.3 note on `tag-filter.ts`.
- The seat reports **upward** (not as a card): the §5.1 seed and §5.3 protocol for the `sentinel-issue-tag-facets` planning seat; the
  spec's §14 findings O2, O3, O4, O6 and O7 (recorded, not fixed); O8 fixed by C1.
- Outcome `COMPLETED` only with every §4 gate passed on the pushed final commit. A red gate that the seat cannot fix inside §1.1's
  paths is `FAILED` with the gate named; a needed path outside §1.1 is escalated, not written.
