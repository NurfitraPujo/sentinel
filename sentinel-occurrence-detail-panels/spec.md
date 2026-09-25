# Sentinel occurrence detail panels: implementation specification

**Feature:** `sentinel-occurrence-detail-panels` · **Repository:** `sentinel` (`tuju-ai/sentinel`) · **Base:** `main` @ `f49721f` (v0.2.37)
**Built against:** `prd/sentinel-occurrence-detail-panels/contract.md` and `fe.md`, both @ **`bc275e6`** (decisions 1-7). If
this document and the contract disagree, the contract wins. The exception is where §1.3 records that the contract's description of
the code or of Sentry is wrong, and each such case says what this spec does about it.
**Revision:** repair round 3 of the spec review, plus the round-4 note (`odp-sr4-pr-ci-skips-check-test`) folded in at
planning. §15 maps every review finding, round 0 (`odp-sr-*`), round 2 (`odp-sr2-*`), round 3 (`odp-sr3-*`) and round 4
(`odp-sr4-*`), to the section and the named test that fixes it.
**Committed with this spec:** `payload-fixture.mjs` and `measure-payload.mjs`, in this folder. `node
sentinel-occurrence-detail-panels/measure-payload.mjs` reproduces every number in §3.3 from a bare checkout, and the `devalue`
figures too once `pnpm install` has run in `apps/dashboard-web` (§12.4).
**Side:** `fe` only (`apps/dashboard-web`). There is no processor, ingestor, migration, chart or agent API change.
**Work items:** `sentinel-occurrence-detail-panels/TASK-01` … `TASK-05`. §12.3 maps every acceptance criterion to the test that
proves it. Every proof is a vitest test run by `task test-unit` (`Taskfile.yml:191-201`, line `:197`). None of them needs Postgres,
a browser or the compose stack, so none of them can skip there (§12.1).
**Outcome of the open questions:** none are left open, and there is no `NEEDS_DECISION`. The contract leaves seven things for "the
spec" to decide. Each one is decided below with its reasoning, and §13.1 lists them with their sources: §3.3 payload, §4.3
truncation, §7.1 context list, §8.3 breadcrumb bound, §8.2 relative time, §9.1 chain order, §4.4 level colours. The tag href is no
longer this spec's to decide: contract decisions 6 and 7 fix it, and §6.3 adopts them verbatim.

Every "today" claim cites `path:line` at `f49721f`. `apps/dashboard-web/` is abbreviated to `dw/`. **Every diff and every
baseline this spec runs is against the merge base, `$(git merge-base origin/main HEAD)`, never a pinned commit** (review finding
`odp-sr2-g5-base-pinned`): `sentinel-issue-tag-facets` may merge first (decision 3) and change files this run does not touch.
`f49721f` is only the commit the citations were read at. Sentry facts were read from
`getsentry/sentry`, `getsentry/relay` and `getsentry/sentry-docs` `master` on 2026-09-25. §13.2 lists the file each one comes from.

---

## 1. The problem

### 1.1 In the words of whoever has it

> Whoever triages an issue in Sentinel sees almost nothing of what schema v2 now stores. — contract §1

The contract's hypothesis: if the occurrence view renders the v2 fields the way Sentry does, a triager can answer "who, on what
device and browser, doing what, from which request, in which release" from the page. They would not need the raw JSON or the agent
API.

The measure has two halves:
- An occurrence with Flutter-shaped contexts, 100 breadcrumbs, a request, a user with geo, tags and a 3-level exception chain renders
  every panel.
- An occurrence with none of them renders exactly what it renders today.

### 1.2 What is true today (verified)

| # | Fact | Evidence |
|---|---|---|
| T1 | The issue page renders each occurrence as one `.occurrence-card`. The card holds: a header line (`formatDate(createdAt)`, the `environment` badge, the `platform` badge); a trace line when `traceId` is set; the stack as one `<pre><code>` of `formatStackFrame` lines under `<h4>Stack Trace</h4>`, with the symbolication badge and raw toggle; and a "Show/Hide Metadata" toggle that dumps `JSON.stringify(metadata, null, 2)`. Every card is always expanded. No occurrence is "the one being viewed". | `dw/src/routes/issues/[id]/+page.svelte:583-655`; `formatStackFrame` `:48-57`; badge/toggle `:603-629`; metadata `:632-651` |
| T2 | The page's `StackFrame` interface knows `file, line, function, column?, symbolicated?, raw?`. Its `Occurrence` interface knows `id, issueId, environment, platform, stacktrace, metadata, traceId, spanId, createdAt`. | `+page.svelte:135-154` |
| T3 | The header message is the **issue's** (`data.issue.message`). | `+page.svelte:488` |
| T4 | `getOccurrencesByIssueId` selects `issuePageOccurrenceColumns`: the 12 pre-v2 columns, 100 rows, `created_at DESC`. A comment says leaving out the v2 bodies is deliberate. `releaseVersion`, `eventId` and `projectId` are selected and never rendered. | `dw/src/lib/server/queries/issue-queries.ts:236-249`, `:261-272` |
| T5 | A unit test (V7e) pins that projection to exactly those 12 keys and asserts that **no** v2 column is selected. | `dw/src/lib/server/queries/issue-queries.occurrences.test.ts:63-89` |
| T6 | The page load runs `requireProjectAccess(…, 'read')`. Then it runs the environment-scope check, which throws `404 Issue not found`, never `403`. Only then does it read occurrences. It uses `params` and `locals` and does not read `url`. | `dw/src/routes/issues/[id]/+page.server.ts:15-52`, `:86-90` |
| T7 | The Drizzle mirror declares the 17 v2 columns. The user column is `eventUser` (`event_user`). | `dw/src/lib/db/schema.ts:475-500` |
| T8 | **Every v2 row has a non-NULL `level`.** `fillOccurrenceV2` sets `occ.Level = "error"` when the event has none, and the SDK specification says "Absent is stored as `error`". It is called on the only live write path (`processor_service.go:342` → `StoreEvent`), which writes all 17 columns in one `INSERT`. The `level` column has **no `DEFAULT`**, so a row written before the migration stays NULL, and an older processor binary during a rolling deploy writes NULL for `level` and every other v2 column together. **One residual writer exists and is unused:** `pgStore.InsertOccurrence` is still in the `Store` interface and writes none of the v2 columns. Its only caller is `tests/integration/processor_store_test.go:292`; nothing under `apps/` calls it. If it were ever revived, it would write v2-shaped events as pre-v2 rows, which then render as pre-v2 (safe, not wrong-shaped). The dev seed scripts (`dw/scripts/*-seed.sql`) set no `level` either, so their rows are pre-v2 and render as today. So a row whose `level` is NULL has all 17 v2 columns NULL: it is a pre-v2 row. | `apps/processor-go/service/processor_service.go:342`, `:647-651`; `apps/processor-go/store/store.go:74-77`, `:206-208`, `:1062-1103`, `:1311`; `tests/integration/processor_store_test.go:292`; `packages/db-migrations/migrations/1725300000_add_error_occurrences_event_v2_fields.sql:63`; `docs/sdk-specification.md:250-251`; `grep -c level dw/scripts/*-seed.sql` → 0 each |
| T9 | A v2 JSONB column is NULL when the event carried nothing for it. `tags`, `contexts` and `breadcrumbs` are NULL when empty. `sdk`, `event_user`, `request` and `exception` are NULL when absent, but **can be `{}`** when the SDK sent an empty object. | `processor_service.go:667-686` |
| T10 | **`in_app` is not a v2 field.** Proto `StackFrame.in_app` is field 4 and existed from the first commit (`313bbfd`). The processor's frame has `InApp bool \`json:"in_app"\`` with **no** `omitempty`, so every stored frame carries `in_app`, pre-v2 rows included. `module`, `abs_path`, `context_line`, `pre_context` and `post_context` are the v2 additions (proto fields 6-10). They use `omitempty`, so a pre-v2 frame has none of them. | `packages/proto/sentinel/v1/error_event.proto:382`; `apps/processor-go/event/event.go:183-209` |
| T11 | Sentinel's frame order is **crashing frame first**, in both `stacktrace` and `exception.values[].stacktrace`. Sentry's is caller first, crashing frame last. | `docs/sdk-specification.md:255-257`; `event.go:168-169` |
| T12 | `exception.values` is stored oldest first, so the exception raised last is the last value. Chain frames are never symbolicated: `ErrorEvent.Symbolicate` walks `e.Stacktrace` only. Symbolication is applied to platforms `javascript,browser` by default. | `event.go:162-181`, `:510-529`, `:624-633`; `apps/processor-go/main.go:95` |
| T13 | Masking before storage. The processor masks `request.url`, `request.query_string`, `request.headers` and `request.env` (key-based and pattern-based), `request.data`, every breadcrumb's `message` and `data`, `contexts`, and every `exception.values[].value`. Tags are masked too. A sensitive **key's** value becomes the string `***REDACTED***`. That holds even when the value was an object, so a context can be a string. `user` is stored unmasked (contract decision 1). | `event.go:242-249`, `:275-305`; `apps/processor-go/masker/masker.go:18-47`, `:76-94` |
| T14 | The stored geo object is `{city, region, country_code, subdivision}`. It has **no `country` field**. The ingestor's GeoLite2 lookup writes the **country name into `region`** (Relay's mapping) and the first subdivision's name into `subdivision`. | `event.go:134-140`; `apps/ingestor-go/geoip/loader.go:415-443`; `dw/src/lib/server/agent-api-spec/schemas.ts:182-195` |
| T15 | The enrichment writes `contexts.os` for a client-platform event and `contexts.client_os` for any other. Each holds `{name, version?}` and is inserted only when absent. | `apps/ingestor-go/enrich/enrich.go:203-230`, `:280-295` |
| T16 | Ingest bounds that decide the payload, and thus a worst-case occurrence: breadcrumbs ≤ 100 items and ≤ 128 KiB; one crumb's `message` ≤ 8192 and `data` ≤ 4096 B; contexts ≤ 32 keys and ≤ 64 KiB; headers ≤ 64 × (256 + 4096); `request.data` ≤ **16 KiB**; tags ≤ 50, key ≤ 32, value ≤ 200; `exception.values` ≤ 20, each with ≤ 100 frames; context lines ≤ 1024 each, ≤ 5 before and after; `message` ≤ 10000. The whole single-event body is ≤ 1 MiB and a batch body ≤ 5 MiB. | `apps/ingestor-go/validation/bounds.go:20-92`; `apps/ingestor-go/main.go:43-47` |
| T17 | A breadcrumb `level` is one of `fatal error warning info debug`, or it is dropped (the key is absent). The stored `timestamp` is `RFC3339Nano` in UTC, and the key is absent when the crumb had no timestamp. | `bounds.go:535-537`; `event.go:152-160`, `:620-625` |
| T18 | A tag key matches `^[a-zA-Z0-9_.:-]{1,32}$`, so a key **can contain `:`**. A value is non-empty, ≤ 200 code points and has no newline. | `bounds.go:127`, `:359-380` |
| T19 | `/search` reads `user_id tenant_id trace_id span_id request_id project environment platform search page` and nothing else, so it ignores `tag`. | `dw/src/routes/search/+page.server.ts:15-25` |
| T20 | `src/app.css` defines one severity family, `--severity-critical-{bg,text,border}`. Its other families are `--status-open-*` (hue 75), `--status-resolved-*`, `--status-ignored-*` and `--color-primary{,-subtle}`. `DESIGN.md` documents a `--severity-warning-*` family that `app.css` **does not define**. | `dw/src/app.css:3-51`; `DESIGN.md:53-60` |
| T21 | The only `{@html}` in the dashboard is `Markdown.svelte`, which renders DOMPurify-sanitised Markdown. | `dw/src/lib/components/Markdown.svelte:34`; `grep -rn '{@html' dw/src` |
| T22 | Component tests render under jsdom with `@testing-library/svelte`. `vitest-setup.ts` unmounts between tests. The `include` is `src/**/*.test.ts` and `tests/**/*.test.ts`. The page tests render `+page.svelte` with a hand-written `data` literal and stub `fetch`. The fixtures in `page.svelte.test.ts` and `page.sourcemaps.test.ts` carry **no v2 keys at all** (not even `null`). | `dw/vite.config.js:4-26`; `dw/vitest-setup.ts`; `dw/src/routes/issues/[id]/page.svelte.test.ts:5-55`; `page.sourcemaps.test.ts:10-60` |
| T23 | `task test-unit` runs `pnpm --dir apps/dashboard-web test`, which is `vitest run`. The only dashboard suites that can skip there are the real-Postgres ones (`schema-drift`, `retention.*.integration`). | `Taskfile.yml:178-201`; `dw/package.json` `scripts.test` |
| T24 | V8 (Node 24.15) parses `2026-09-25T02:00:00.123456789Z` with `Date.parse`. ECMAScript only guarantees three fractional digits, so another engine may return `NaN`. | measured 2026-09-25; ECMA-262 §21.4.3.2 "Date Time String Format" |
| T25 | **SvelteKit's page-data serialiser throws on a `__proto__` key.** SvelteKit serialises load data with `devalue` (5.8.0 in `dw/pnpm-lock.yaml`, a dependency of `@sveltejs/kit` and `svelte`). `devalue.stringify` and `devalue.uneval` of `JSON.parse('{"tags":{"__proto__":"x"}}')` both throw `Cannot stringify objects with __proto__ keys`. `JSON.parse`, which the `postgres` driver uses for JSONB, makes `__proto__` an ordinary own key. `__proto__` matches the tag-key pattern (T18). Context names, header names, breadcrumb `data` keys, request `data` keys and `metadata` keys are not pattern-checked. So one stored event with such a key makes the page load throw during serialisation, a 500, **today** for `metadata` on any of the 100 listed rows. `devalue` is **not** a direct dependency of `dw` today. | `measure-payload.mjs` §3 output, run against an installed `f49721f` checkout, which resolves `devalue` through `@sveltejs/kit` to `node_modules/.pnpm/devalue@5.8.0`; `dw/pnpm-lock.yaml:1222`, `:2903` (kit's dependency), `:3672`; `bounds.go:127` |
| T26 | **Context names are only length-checked** (1..64 code points after a NUL strip), so a context can be named `constructor`, `toString`, `hasOwnProperty` or `__proto__`. The same holds for context field names, header names and breadcrumb `data` keys. A plain-object table lookup `TABLE[name]` on such a name returns an `Object.prototype` member (a function), not `undefined`. | `bounds.go:412-440`; ECMA-262 `Object.prototype` |
| T27 | **Top-level frame `file` and `function` have no server-side bound.** They are SDK-side MUST-truncate rules only (512 and 255), because cutting them would split issues. Chain frames are truncated server-side. `context_line` and each `pre_context`/`post_context` line are ≤ 1024, `module` ≤ 255. | `bounds.go:38-43`, `:88-91` |
| T28 | Today's page **ignores unknown query parameters**: its load reads `params` and `locals` only (T6), so `/issues/<id>?anything=…` renders the page. `error_occurrences.id` is a `UUID PRIMARY KEY`, so Postgres rejects a malformed id with `22P02` before any row is read. | `+page.server.ts:15`; `packages/db-migrations/migrations/1716508800_init.sql:33-34` |
| T29 | **On the real page, an occurrence's `createdAt` is a `Date`, never a string.** The Drizzle column is `timestamp('created_at').defaultNow()` with no `mode`, so Drizzle returns a `Date` (or `null`: the column is nullable in the mirror), and SvelteKit's `devalue` carries a `Date` to the client as a `Date`. Only hand-written test data holds a string: every existing page fixture passes an ISO string (T22). The page's two existing consumers already accept both: `formatDate(date: string \| Date \| null)` and `getDateGroup(dateStr: string \| Date \| null)` each call `new Date(v)`. `formatDate` is `toLocaleString('en-US', …)` with **no `timeZone`**, so its text depends on the process's time zone: `2026-09-25T02:00:00.000Z` reads `Sep 25, 2026, 02:00:00 AM` under `TZ=UTC` and `09:00:00 AM` under `TZ=Asia/Jakarta` (measured, Node 24). | `dw/src/lib/db/schema.ts:501`; `+page.svelte:25-34`, `:82-92`, `:153`, `:162`, `:589`; review finding `odp-sr3-crumb-anchor-date` |
| T30 | **The merge base fails `pnpm check`.** At `f49721f`, `svelte-check` reports `5 errors and 1 warning in 2 files` (reproduced locally with the lockfile installed, and CI run 36043616975's dashboard job fails at that step, so its `pnpm test` step never runs). The 5 errors: `src/lib/db/queries/organizations.ts:627-633` uses `users` in `listOrganizationMembers` without importing it (the import block `:2-10` lists seven tables, not `users`), from `e91b07e` (#52). This is a runtime defect too: `GET /api/agent/members` calls it (`routes/api/agent/members/+server.ts:26`) and would throw `ReferenceError: users is not defined`; its test mocks the function (`members.test.ts:9`), so nothing caught it. The 1 warning: `src/routes/+page.svelte:162:27`, `let searchQuery = $state(data.filters.q ?? '')`, Svelte's `state_referenced_locally` (the `$effect` at `:163-165` already re-syncs it). Adding the `users` import and wrapping that initialiser in `untrack(() => …)` gives `0 errors, 0 warnings` (verified on `f49721f`), and the issue-list page's four test files still pass (69 tests). | `svelte-check` output at `f49721f`; `git log -1 e91b07e`; review finding `odp-sr3-g3-base-red` |
| T31 | **`jsdom` ships no type declarations, and DefinitelyTyped published no `@types/jsdom` 22-26.** `dw` has `jsdom` `^25.0.1` (resolved `25.0.1`, published 2024-09-22). `import { JSDOM } from 'jsdom'` in a file under `src/` fails `pnpm check` with TS7016 `Could not find a declaration file for module 'jsdom'` (verified). `@types/jsdom` goes from `21.1.7` (2024-05-30) straight to `27.0.0` (2025-09-29), so `21.1.7` is the typings line that was current for every jsdom from 21 through 26, 25.0.1 included. `pnpm add -D -E @types/jsdom@21.1.7` adds two packages to the lockfile (`@types/jsdom@21.1.7`, `@types/tough-cookie@4.0.5`; its `parse5` `^7` resolves to the `7.3.0` already locked for jsdom), and `pnpm check` then passes with that import (verified). | `npm view @types/jsdom versions`, `npm view @types/jsdom time`, `npm view jsdom time`; `dw/package.json:21`; review finding `odp-sr3-jsdom-types` |

### 1.3 Where the contract does not match the code or Sentry

Each correction goes upward. The last column says what this spec does about it.

| Contract says | What is true | Consequence for this spec |
|---|---|---|
| §1: the processor "stores `in_app` … too", listed among what frames lose. §4.2: "Mark `in_app` frames. Collapse runs of non-app frames." Compatibility: a pre-v2 occurrence renders "byte-for-byte the same … stack". | `in_app` is stored on **every** frame of every row, pre-v2 included (T10). Marking and collapsing by `in_app` on every row would change every pre-v2 stack, which breaks the binding compatibility rule. | The new frame renderer runs for **v2 occurrences only** (§4.1, the `level` test of T8). A pre-v2 stack keeps today's `<pre>` exactly, and its `in_app` stays unrendered. |
| §4.5: level "coloured with the existing `--severity-*` variables". | Only `--severity-critical-*` exists (T20). | §4.4 maps the five levels onto variables that exist, without adding any. The `DESIGN.md` drift is recorded in §14. |
| §4.7: "a 64 KB `request.data`". | The ingest bound is 16 KiB (T16). Header values are ≤ 4096. Top-level frame `file`/`function` have no server bound at all (T27). | The truncation rule (§4.3) does not depend on any bound: **every** event string this feature renders goes through `ExpandableText`. The 64 KiB case is still tested, because a hand-written or pre-bound row could be larger. |
| §4.3: user `geo (city, region, country / country code)`. | There is no `country` field. `region` holds the country **name** for enriched events (T14). | §6.1 renders all four stored fields. It invents no `country` key and derives nothing from the code. |
| §4.3 before `aab1cc5`: the tag link `/search?tag=<key>:<value>`, the whole string encoded once. | A tag key can contain `:` (T18), so the string `a:b:c` does not say where the key ends. | **Resolved upstream.** This spec's round-0 finding O1 raised it, and contract decision 6 (`aab1cc5`, amended together with `sentinel-issue-tag-facets` decision 8) answers it: each part is encoded before the join. Decision 7 (`bc275e6`, with tag-facets decision 10) makes that `encodeURIComponent` form the one normative href, built only by the shared `tagSearchHref`, never with `URLSearchParams`. §6.3 adopts both verbatim, and TG1/TG3 prove them. O1 and O5 are closed (§14). |
| §4.5: a timeline "in stored order" with time "relative to the occurrence". | Sentry's breadcrumbs default to **absolute** time and **newest first**. Relative time is an option, and its zero point is in practice the event. | The contract's choice stands. It is a product decision, and it matches Sentry's relative mode. §8.2 anchors the relative time on the occurrence's `createdAt`. |
| §4.1: "the heavy columns only for the occurrence being viewed". | No occurrence is "being viewed" today. Every card is expanded (T1). | §3.2 adds the notion: a selected occurrence, `?occurrence=<id>`, defaulting to the newest. An occurrence of this issue older than the newest 100 can still be selected (§3.2 S3a). |
| §4.1: "It must not bloat the page", and the page returns every JSONB body as page data. | A `__proto__` key in any JSONB body makes SvelteKit's serialiser throw (T25). `metadata` already has this exposure today. | §3.4 adds a load step that drops `__proto__` keys at every depth from every JSONB value the page returns, `metadata` and `stacktrace` included. L11 proves it with `devalue`. |
| §4.1 and §5: "pre-v2 fixture unchanged" for the query. | V7e (T5) asserts that the page selects **no** v2 column. This feature exists to break that. | §3.1 replaces V7e with a new exact-projection test. |

---

## 2. What this change does, and does not do

**Does:**
1. The list read gains six light v2 columns. A new read fetches the whole row, heavy columns included, for **one** occurrence,
   the selected one (§3). The newest occurrence is selected by default. `?occurrence=<id>` selects another occurrence **of this
   issue**, listed or older than the newest 100. An occurrence of another issue answers 404, and a malformed or unknown value is
   ignored, as today's page ignores unknown parameters. Access control is the page load's own, unchanged.
2. The load drops `__proto__` keys at every depth from every JSONB value it returns, so no stored key can make the page's
   serialisation throw (§3.4). This also closes the exposure `metadata` has today.
3. Every **v2** occurrence card shows its own message, level, release, dist, event id, server name, transaction and SDK (TASK-04).
   Its stack renders through a frame component. That component marks `in_app` frames, collapses runs of system frames the way
   Sentry does, and shows `module` and numbered source context (TASK-04).
4. The **selected** v2 occurrence also shows these panels: tags (TASK-03), the exception chain (TASK-05), breadcrumbs (TASK-02),
   request and user (TASK-03), and contexts (TASK-01).
5. Every event string is rendered as text, through `ExpandableText`, so a long one is truncated with a full-text expand (§4.3).
   Every table keyed by event data is own-property only (§4.2 rule 5).

**Does not:**
- It does not change anything for a **pre-v2** occurrence. The card is identical to the merge base's once Svelte's comment anchors,
  scoped class hashes and runs of whitespace-only text are normalised away (§10, proved against a golden captured from the merge
  base).
- It adds no column, migration, processor, ingestor or chart change. It makes no agent API, openapi or drift-test change.
- It implements no `/search?tag=` (tag facets does that) and no symbolication of chain frames (decision 4).
- It adds no Playwright (decision 2) and no new CSS variable. It does not redesign the issue header, trend, relations or occurrence
  list. The page still lists the newest 100 occurrences.
- It adds exactly **two devDependencies** and no runtime dependency, both pinned exactly:
  - `devalue` **`5.8.0`**, the exact version `@sveltejs/kit` resolves in `dw/pnpm-lock.yaml` (T25, `:2903`). `pnpm install` then
    adds an importer entry to the lockfile and no new package, and links `dw/node_modules/devalue`. L11 imports it to serialise the
    load result exactly as SvelteKit does, and `measure-payload.mjs` finds it there. Neither needs an environment variable (review
    finding `odp-sr2-devalue-path-note`). If `@sveltejs/kit` is ever bumped to a different `devalue`, this pin moves with it in the
    same change.
  - `@types/jsdom` **`21.1.7`**, the typings for `html-structure.ts`'s `import { JSDOM } from 'jsdom'` (§12.3), without which
    `pnpm check` fails with TS7016 (T31, review finding `odp-sr3-jsdom-types`). The coordinator's ruling pins it to the installed
    jsdom major; no `@types/jsdom` 25.x exists, so the pin is the typings line that was current for jsdom 25 (T31: `21.1.7`,
    published before jsdom 25.0.1 and not superseded until `27.0.0`). It adds `@types/jsdom` and `@types/tough-cookie` to the
    lockfile. If `jsdom` is ever bumped to 27 or later, this pin moves to the matching `@types/jsdom` major in the same change.
- It fixes **no** pre-existing defect beyond the two its own gate needs. G3 (`pnpm check`) is red at the merge base (T30), so this
  run adds the missing `users` import to `dw/src/lib/db/queries/organizations.ts` and wraps the list page's `searchQuery`
  initialiser in `untrack` (`dw/src/routes/+page.svelte:162`), in **a separate commit** (§11.1), and records both as §14 O8. The
  import also stops `GET /api/agent/members` from throwing at runtime; no agent API file changes (`organizations.ts` is not one of
  §10.3's paths, and the endpoint's response shape is what its code already declares).
- It does not render `request.env`, `logger`, `source`, `error_class`, `trace_flags` or `received_at`. The contract lists none of
  them. They stay readable through the agent API.
- It does no unit formatting of context values (bytes, MHz, percent). Values render as stored (§7.3).
- It does not derive a country name from `country_code`.

---

## 3. Reading the data (§4.1, all tasks)

### 3.1 Two projections

In `dw/src/lib/server/queries/issue-queries.ts`:

**`issuePageOccurrenceColumns`** (the list, ≤ 100 rows) is the 12 pre-v2 columns of T4 **plus** exactly these light columns:

| Key | Column | Bound (T16) |
|---|---|---|
| `level` | `level` | 16 |
| `message` | `message` | 10000 code points |
| `serverName` | `server_name` | 256 |
| `transaction` | `transaction` | 200 |
| `dist` | `dist` | 64 |
| `sdk` | `sdk` | name 128 + version 64 |

That makes 18 keys. The comment at `:261-264` is rewritten: it now says the list carries the light header fields of every listed
occurrence, and the heavy JSONB bodies go only to the selected one (this spec §3.1).

**`issuePageOccurrenceDetailColumns`** (new, one row) is the whole row the page can render: `...issuePageOccurrenceColumns` (the
18 keys above) plus the six heavy keys `user` (bound to `errorOccurrences.eventUser`, the same aliasing as
`agentOccurrenceColumns`, `agent-reads.ts:197`), `tags`, `contexts`, `request`, `breadcrumbs`, `exception`. That makes 24 keys.
The list keys are included because a selected occurrence older than the newest 100 has no listed row to borrow them from (§3.2
S3a). For a listed selection they repeat one row's list columns, about 5 KB (§3.3).

**`issueQueries.getOccurrenceDetail(issueId: string, occurrenceId: string)`** (new) runs
`db.select(issuePageOccurrenceDetailColumns).from(errorOccurrences).where(and(eq(errorOccurrences.id, occurrenceId),
eq(errorOccurrences.issueId, issueId))).limit(1)`. It returns the row or `null`. **Both** predicates are required. The `issueId`
predicate is what stops an occurrence id from being read through another issue's page.

**`issueQueries.occurrenceExistsOutsideIssue(issueId: string, occurrenceId: string): Promise<boolean>`** (new) runs
`db.select({ id: errorOccurrences.id }).from(errorOccurrences).where(and(eq(errorOccurrences.id, occurrenceId),
ne(errorOccurrences.issueId, issueId))).limit(1)` and returns whether a row came back. It selects the id only, so no column of
another issue's occurrence is ever read into the load. It is a primary-key lookup (T28).

`logger`, `errorClass`, `source`, `traceFlags` and `receivedAt` are not selected by either projection (§2).

### 3.2 The selected occurrence

The load gains `url` (`load: async ({ params, locals, url })`). After `occurrences` is read (`+page.server.ts:86`), and so after
both access checks (T6), it reads **`raw = url?.searchParams.get('occurrence') ?? null`** and resolves the selection. The optional
chain is binding (review finding `odp-sr3-facets-loader-url`): a loader test that builds its event without a `url` (today's
`page.server.test.ts` `loadAs`, and `sentinel-issue-tag-facets`' `page.tag-facets.test.ts` if tag facets merges first) must reach
S1, not throw `Cannot read properties of undefined (reading 'searchParams')`. SvelteKit always passes a `url`, so on the real
page this changes nothing. No existing loader test is edited (§10.7); L12 proves an absent `url` is S1. `UUID_RE` is
`/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/`, tested against `raw.toLowerCase()`, and `id` below is that
lower-cased value. Postgres writes UUIDs lower-case, so a listed id compares equal whatever case the link used.

| # | `raw` | Reads | Result |
|---|---|---|---|
| S1 | `null` or `''`, including no `url` at all | none | **default:** `selectedOccurrenceId` = `occurrences[0]?.id ?? null`, the newest listed occurrence |
| S2 | `id` equals the `id` of a listed occurrence | none | `selectedOccurrenceId` = that id |
| S3a | `id` matches `UUID_RE`, is not listed, and `getOccurrenceDetail(issueId, id)` returns a row | the scoped detail read | `selectedOccurrenceId` = `id`, `occurrenceDetail` = that row. It is an occurrence of **this** issue older than the newest 100, and it renders selected (below). |
| S3b | `id` matches `UUID_RE`, is not listed, the scoped read returns `null`, and `occurrenceExistsOutsideIssue(issueId, id)` is `true` | both | `throw error(404, 'Occurrence not found')`. The id belongs to another issue, whether in this project or another one: out of scope. |
| S3c | `id` matches `UUID_RE`, and both reads come back empty | both | **default**, as S1. No error. The id is unknown: never stored, or deleted by the retention sweep. |
| S4 | anything else: not UUID-shaped (`not-a-uuid`, `{…}`, a 32-hex id without hyphens, an SQL fragment) | **none** | **default**, as S1. No error. The value never reaches a query, so a malformed id cannot cause `22P02` → 500 (T28). |

Then:
- For S1, S2 and the S3c/S4 default, `occurrenceDetail` = `await issueQueries.getOccurrenceDetail(issueId,
  selectedOccurrenceId)`. It is called only when a selected id exists **and** that listed row's `level` is non-null (a v2 row, T8).
  Otherwise it is `null`, and no detail query runs. S3a already holds its row, whatever its `level`, and does not read it again.
- A `null` result for a listed selection (the row was deleted between the two reads, for example by the retention sweep) is not an
  error. The page then renders no panels.
- The page treats an **absent** `selectedOccurrenceId` or `occurrenceDetail` as `null`, as it already does for `canWrite` and
  `releases` (`+page.svelte:244-249`). The existing page fixtures omit both (T22), and so does a stale loader build.
- The load returns `selectedOccurrenceId` and `occurrenceDetail` beside `occurrences`. Both JSONB-bearing values pass through the
  §3.4 step first.

Why these answers (coordinator ruling on review finding `odp-sr-occurrence-param-404-regression`):
- **404 only outside scope.** The contract requires "404, never 403, outside scope". The issue itself being inaccessible is
  already a 404 or the project helper's error, thrown before the parameter is read. An id that belongs to another issue or
  project is the other out-of-scope case, S3b. Its message, `Occurrence not found`, says nothing about where the id lives.
- **An in-scope id always renders.** A "Show event details" link to an occurrence of this issue keeps working after 100 newer
  events push it out of the list (S3a). Before this rule, it would have 404'd the whole issue page.
- **Anything else is ignored, as today.** Today's page renders for any query string (T28). A malformed or unknown value (S3c, S4)
  therefore keeps rendering the page, with the default selection, and shows no error. That is not a regression for any URL that
  works today.
- The order is fixed: the list check first (no query), then the **scoped** read, then the existence probe. The probe runs only
  after the scoped read found nothing, and selects nothing but the id (§3.1).
- The residual: S3b and S3c answer differently, so a caller who holds a well-formed occurrence id can learn whether it exists
  under some other issue. It reveals no column of that occurrence, the ids are random v4 UUIDs, and it is what the ruling
  prescribes. It is recorded in §14 (O4).

The environment-scope `404 Issue not found` and every `requireProjectAccess` error are thrown **before** the occurrence parameter
is read. Their order and messages are unchanged.

**Selecting from the page.** On a v2 card that is not the selected one, the header line (T1) ends with a link
`<a href="?occurrence={id}" data-sveltekit-noscroll data-testid="select-occurrence">Show event details</a>`. The selected v2
card shows `<span class="selected-marker" data-testid="selected-occurrence">Event details below</span>` in the same place, and
the card gets `aria-current="true"`. A pre-v2 card gets neither (§10). The `href` is built from the row's `id`, a UUID the
database generated, not from any event field.

**A selected occurrence outside the list (S3a).** The card markup inside `{#each group.occurrences as occurrence}` moves into a
snippet, `{#snippet occurrenceCard(occurrence)}`, which the `each` calls. When `occurrenceDetail` is non-null and its `id` is not
among `data.occurrences`, the Occurrence History section renders, directly after its heading and the `occurrence-window` line
and before the first date group:

```svelte
<div class="selected-outside-window" data-testid="selected-outside-window">
  <p class="occurrence-window">This event is older than the {data.occurrences.length} most recent shown below.</p>
  {@render occurrenceCard(data.occurrenceDetail)}
</div>
```

The card is rendered by the same snippet, so it is the selected card: marker, `aria-current`, header facts and panels when it is
v2, and today's pre-v2 card when it is not. No such block exists in any other case, so the default page is unchanged (§10).

### 3.3 Why only the selected occurrence carries the heavy columns (measured)

Measured with the committed script (§12.4): `node sentinel-occurrence-detail-panels/measure-payload.mjs`, `JSON.stringify` UTF-8
bytes. Run with `devalue` available, the same script also prints what SvelteKit actually sends. `devalue` deduplicates repeated
values, so it came out **smaller** on the fixture: −27 % for a listed row, −19 % for the heavy columns, +9 % for the light ones. The
JSON figures below are therefore the conservative ones.

**The largest fixture**, the contract §1 hypothesis occurrence (`FLUTTER_OCCURRENCE` / `FLUTTER_DETAIL` in
`payload-fixture.mjs`, §12.2): 20 frames (3 with source context), 7 contexts, 100 breadcrumbs, a request, a user with geo, 10 tags
and a 3-value chain.

| Payload | Bytes |
|---|---|
| one listed row today (12 columns) | 4 574 |
| the six light columns | 212 |
| the six heavy columns | 27 504 (`breadcrumbs` 19 645, `exception` 5 787, `contexts` 1 174, `request` 438, `tags` 211, `user` 181) |
| the detail row (18 list keys + 6 heavy, §3.1) | 32 288 |
| **100 rows today** | **457 400** |
| 100 rows × (today + light + heavy) | 3 229 000 (7.1 × today) |
| **100 rows × (today + light) + 1 detail row** (this spec) | **510 888** (+11.7 %) |

**The ingest-bounds maxima** (T16), per occurrence, ASCII: `breadcrumbs` 131 001, `contexts` 65 505, `request` 383 828 (`env`
included: the column is read whole, though §2 does not render it), `user` 1 189, `tags` 11 901. That is 593 424 bytes without
`exception`, which is bounded only by the event body (≤ 1 MiB single-event, ≤ 5 MiB batch body). So "all columns for all 100" is
59 342 400 bytes before any `exception`, and up to about 100 MB with one. The selected-only design carries one occurrence's heavy
columns: ≤ 1 MiB single-event (≤ 5 MiB batch body).

**The worst-case light payload** (review finding `odp-sr-measurement-not-reproducible`). The light columns go to all 100 rows,
and `message` (≤ 10 000 code points) dominates them. For 100 rows at their ingest maxima:

| Encoding of every code point | 100 rows of light columns |
|---|---|
| ASCII, 1 byte | 1 081 800 |
| outside the BMP, 4 bytes of UTF-8 | 4 295 400 |
| a control character JSON must escape, 6 bytes (`\u0001`) | 6 437 800 |
| for comparison, today: `metadata`'s own bound alone, 64 KiB × 100, before its markers | 6 553 600 |

**It does not change decision 1.** The light worst case is at most 6.4 MB, the same order as what the page already accepts
today from `metadata` alone. It is also far below today's real worst case: a listed row's `stacktrace` has no server bound on
top-level `file`/`function` (T27), so each of the 100 listed rows is bounded only by the 1 MiB event body. The heavy columns are
the ones that would multiply the page by 100 × 0.6-1 MiB, and they stay selected-only. Moving `message` off the list as well
was considered and rejected: §5.1 shows it on every v2 card, which is the header fact the triager scans the list by.

**Decision: the heavy columns go only to the selected occurrence**, through the same page load (§3.2). A separate endpoint was
rejected, because it would need its own copy of `requireProjectAccess` plus the environment check, and the contract requires the
same checks. The load already has them, in a tested order. The light columns go to all 100 rows.

M1 (§12.3) pins the fixture sizes the implementer copies into `occurrence-fixtures.ts` to the table above, ±10 %, so the numbers
and the fixture cannot drift apart unnoticed.

### 3.4 No `__proto__` key reaches page data (binding)

T25: one stored `__proto__` key makes SvelteKit's serialiser throw, a 500 for the whole issue page. So the load cleans every
JSONB value it returns.

**`dropProtoKeys(value: unknown): unknown`**, new, in `dw/src/lib/server/json-safe.ts` (a `$lib/server` module, so no route file
gains an export, B12):
- An array returns a new array of `dropProtoKeys(element)`.
- A plain object (prototype `Object.prototype` or `null`) returns a new plain object with every own enumerable key **except**
  `__proto__`, each value passed through `dropProtoKeys`. The copy is made with `Object.defineProperty` or by skipping the key
  before assignment, never with `out['__proto__'] = …`, which would set the copy's prototype.
- Anything else (string, number, boolean, `null`, a `Date`) is returned as is.
- It drops the key rather than renaming it. A renamed key would show a value under a name the SDK never sent, and the page's
  own `__proto__` data is not worth rendering.
- It recurses. `devalue` recurses over the same structure when it serialises, so the step adds no depth limit the page does not
  already have.

**Where it runs:** in the load, on exactly these values, before they are returned:
- every listed row's `stacktrace`, `metadata` and `sdk` (`occurrences.map(cleanOccurrenceRow)`);
- the detail row's `stacktrace`, `metadata`, `sdk`, `user`, `tags`, `contexts`, `request`, `breadcrumbs` and `exception`.

Every other key of a row (`createdAt` a `Date`, the text columns) is copied unchanged. `cleanOccurrenceRow` is a function in
`json-safe.ts` that returns `{ ...row, [k]: dropProtoKeys(row[k]) }` for each of those JSONB keys present on the row.

**Compatibility.** A row without a `__proto__` key comes back deep-equal to its input, so every page that renders today renders the
same. A row with one fails today (500). After the change it renders without that key, and its metadata dump omits it. This is the
coordinator's ruling on review finding `odp-sr-devalue-proto-key`: it closes the pre-existing `metadata` exposure rather than
recording it.

---

## 4. Rules shared by every panel

### 4.1 Which occurrence is v2

`hasV2Fields(occ)` is `occ.level !== null && occ.level !== undefined`. By T8, this is equivalent to "not every v2 column is NULL"
for any row the processor wrote. `undefined` counts as `null` because the existing page fixtures omit the keys (T22). A load
built before this change (a stale build) also omits them.

- A **pre-v2** occurrence renders exactly as at `f49721f` (§10).
- A **v2** occurrence gets the header facts (§5.1) and the frame renderer (§5.2). If it is selected, it also gets the panels
  (§6-§9).

### 4.2 Untrusted content (contract §4.7, binding)

1. **No `{@html}`** in any file this feature adds or edits. No event field reaches `Markdown.svelte` or any other sanitising
   renderer. Every event string is a Svelte text interpolation (`{value}`), which Svelte escapes, and reaches it through
   `ExpandableText` (§4.3). An event string may appear in an attribute value in exactly four places, all escaped by Svelte and
   none of them a URL, style or handler: `data-context-key` (§7.3), an `<option value>` of the breadcrumb filters (§8.3), and
   `title`/`datetime` of `.crumb-time`, which hold the stored timestamp only after it parsed (§8.2). The tag link's `href` holds
   event strings only percent-encoded behind the constant `/search?tag=` (§6.3).
2. **No event field becomes an `href`, `src` or event-handler attribute.** `request.url`, a breadcrumb's `data.url`, and any URL
   inside contexts, tags or headers are text. The only anchors the feature renders are the tag links (§6.3), whose `href` is the
   constant `/search?tag=` plus `encodeURIComponent(…)`, and the select links (§3.2), built from the row's database id. Neither can
   produce a `javascript:` URL.
3. **No event field reaches `style` or `class`.** The level tone (§4.4) is chosen from a fixed map. An unknown level falls back to
   the neutral class and is never interpolated into a class name.
4. **Non-string values** are shown through `formatValue(v)`:
   - a string, verbatim;
   - a number or boolean, `String(v)`;
   - `null`, `null`;
   - an object or array, `JSON.stringify(v)` (compact) inside table cells, or `JSON.stringify(v, null, 2)` inside block views
     (request body, breadcrumb data, a non-object context).

   Nothing is parsed as HTML, Markdown or a URL.
5. **Every lookup keyed by event data is own-property only** (T26, review finding `odp-sr-prototype-lookup`). A context name,
   field name, level, category, header name or tag key can be `constructor`, `toString`, `hasOwnProperty` or `__proto__`, and a
   plain-object lookup on it returns an `Object.prototype` function.
   - Every fixed table this feature defines is a `Map`: the context titles and the well-known context order (§7.1), the
     per-card field lists and labels (§7.2), the headline rules (§7.2), `levelTone`'s map (§4.4) and the breadcrumb level order
     (§8.3). It is read with `.get()`/`.has()` only.
   - Every collection built from event values is a `Map` or `Set`: the distinct categories and levels of the breadcrumb filters,
     and any count or group.
   - Iteration over an event object is `Object.keys`/`Object.entries` (own enumerable keys), never `for…in`.
   - A plain object may still be used for a table only if every read is guarded by `Object.hasOwn(table, key)`. `key in table`,
     `table[key] ?? fallback` and `table[key] || fallback` are not allowed on event-supplied keys.
   - The effect: a context named `constructor` is an unknown context rendered generically under its raw key (§7.3), a device field
     named `constructor` is an unlabelled row, and a level `toString` is `tone-neutral`. Nothing renders a function's source and
     nothing throws. C6 and U4 prove it (§12.3).

### 4.3 Truncation and expand (`ExpandableText`)

One component, `ExpandableText.svelte`, props `{ text: string; limit: number; block?: boolean }`:
- Length is counted in **code points** (`Array.from(text).length`), so a surrogate pair is never split.
- If `length ≤ limit`, it renders `text`.
- Otherwise it renders the first `limit` code points followed by `…`, then a button `Show all ({length} characters)`. The button has
  `aria-expanded="false"` and `data-testid="expand-text"`. Clicking renders the full text and relabels the button `Show less`
  (`aria-expanded="true"`). The state is per instance.
- **Markup, exactly** (§4.6 says where each form may go):
  - inline (no `block`): `<span class="expandable-text">{shown}{button}</span>`. Phrasing content only: a text node and, when
    over the limit, the `<button>`.
  - `block`: `<div class="expandable-text block"><pre>{shown}</pre>{button}</div>`. Whitespace is kept inside the `<pre>`; the
    button is the `<pre>`'s sibling, never inside it. This is flow content, so it never goes inside a `<p>` or any other
    phrasing-only element (§4.6).
- `limit` and `block` are independent. `BLOCK_LIMIT` says how much text shows; `block` says whether it is a `<pre>`. Of the fields
  below, exactly four are `block`: E1, E22, E28 and E38. A frame line (E9) and a source line (E11) use `BLOCK_LIMIT` **inline**,
  because they sit inside a `<code>`.

The two limits:

| Constant | Value | Used for |
|---|---|---|
| `INLINE_LIMIT` | 256 | every inline value: table cells, facts, headings, crumb fields (the `Limit` column below says which) |
| `BLOCK_LIMIT` | 2048 | the occurrence message, a frame line, a source-context line, `request.data`, a crumb's `data`, a non-object context value (only the message, `request.data`, a crumb's `data` and a non-object context are also `block`) |

Why these numbers:
- The contract's example header (1 KiB) must truncate, so the cell limit has to be below 1024. 256 keeps one row to at most about
  three lines at the card's width.
- 2048 is about 40 lines of pretty-printed JSON. That shows a typical body whole, while the 16 KiB bound and the contract's 64 KiB
  case collapse. A frame line (`at fn (file:line:col)`, an SDK-truncated file of 512 plus a function of 255) and a source line
  (≤ 1024) take `BLOCK_LIMIT` so that a normal frame never shows a button, while an unbounded one (T27) still collapses.
- Neither number is an ingest bound, so the view does not change if a bound moves.

**The rule (binding, review finding `odp-sr-truncation-untested-in-panels`): every event string this feature renders as text goes
through `ExpandableText`, whatever its ingest bound.** The review asked for every string with no ingest bound tighter than
`INLINE_LIMIT`. This rule contains that one and drops the exception, for three reasons: a hand-written or pre-bound row can
exceed any bound (§1.3); a bound can move; and `ExpandableText` renders a string under its limit as the bare text, so routing a
short field costs nothing. A bare `{value}` for an event string is a defect in review, whatever the field.

Where a button cannot go, the text goes through `clip(text, limit)` in `occurrence-display.ts` instead: the first `limit` code
points plus `…` when longer, else the text. There are exactly two such places: an `<option>` label of the breadcrumb filters
(the full value is in the crumb itself, E19) and the tag link's text (an `<a>` may not contain a button, §6.3, E13).

**The inventory.** Every event string the feature renders, its limit, and the ingest bound that would be at risk with a bare
`{value}` (T16, T27). The `E` ids are what the `HOSTILE_*` and `LONG_*` fixtures (§12.2) fill, one by one:

| # | Field | Rendered by | Limit | Ingest bound |
|---|---|---|---|---|
| E1 | `message` (or the issue's fallback) | `OccurrenceHeader`, in `<div class="occurrence-message">` (§5.1) | `BLOCK_LIMIT`, block | 10 000 |
| E2 | `level` (badge text) | `OccurrenceHeader` | `INLINE_LIMIT` | one of 5 values |
| E3 | `releaseVersion` | `OccurrenceHeader` | `INLINE_LIMIT` | 100 |
| E4 | `dist` | `OccurrenceHeader` | `INLINE_LIMIT` | 64 |
| E5 | `eventId` | `OccurrenceHeader` | `INLINE_LIMIT` | 64 |
| E6 | `serverName` | `OccurrenceHeader` | `INLINE_LIMIT` | 256 |
| E7 | `transaction` | `OccurrenceHeader` | `INLINE_LIMIT` | 200 |
| E8 | the SDK line (`sdk.name`, `sdk.version`) | `OccurrenceHeader` | `INLINE_LIMIT` | 128 + 64 |
| E9 | the frame line, `formatStackFrame(displayedFrame(frame, showRaw))`: `function`, `file` | `StackFrames` | `BLOCK_LIMIT`, inline | **none** for top-level frames (T27) |
| E10 | frame `module` | `StackFrames` | `INLINE_LIMIT` | 255 |
| E11 | each `pre_context` line, `context_line`, each `post_context` line | `StackFrames` | `BLOCK_LIMIT`, inline | 1 024 |
| E12 | tag key | `TagsTable` | `INLINE_LIMIT` | 32 |
| E13 | tag value, the link text | `TagsTable` | `clip(…, INLINE_LIMIT)`: an `<a>` cannot hold a button (§6.3) | 200 |
| E14 | chain value `type` | `ExceptionChain` | `INLINE_LIMIT` | 255 |
| E15 | chain value `value` | `ExceptionChain` | `INLINE_LIMIT` | 10 000 |
| E16 | `mechanism.type` | `ExceptionChain` | `INLINE_LIMIT` | 64 |
| E17 | a crumb `timestamp` that does not parse (shown raw, §8.2) | `BreadcrumbTimeline` | `INLINE_LIMIT` | processor-written RFC 3339, so short; unbounded on a hand-written row |
| E18 | crumb `level` | `BreadcrumbTimeline` | `INLINE_LIMIT` | one of 5 values |
| E19 | crumb `category` | `BreadcrumbTimeline` | `INLINE_LIMIT`; `clip` in its `<option>` | 128 |
| E20 | crumb `type` | `BreadcrumbTimeline` | `INLINE_LIMIT` | 64 |
| E21 | crumb `message` | `BreadcrumbTimeline` | `INLINE_LIMIT` | 8 192 |
| E22 | crumb `data`, pretty JSON, after its toggle | `BreadcrumbTimeline` | `BLOCK_LIMIT`, block | 4 096 |
| E23 | `request.method` | `RequestPanel` | `INLINE_LIMIT` | 16 |
| E24 | `request.url` | `RequestPanel` | `INLINE_LIMIT` | 2 048 |
| E25 | `request.query_string` | `RequestPanel` | `INLINE_LIMIT` | 4 096 |
| E26 | header name | `RequestPanel` | `INLINE_LIMIT` | 256 |
| E27 | header value | `RequestPanel` | `INLINE_LIMIT` | 4 096 |
| E28 | `request.data` | `RequestPanel` | `BLOCK_LIMIT`, block | 16 KiB |
| E29 | `user.id` | `UserPanel` | `INLINE_LIMIT` | 255 |
| E30 | `user.username` | `UserPanel` | `INLINE_LIMIT` | 128 |
| E31 | `user.email` | `UserPanel` | `INLINE_LIMIT` | 254 |
| E32 | `user.ip_address` | `UserPanel` | `INLINE_LIMIT` | 45 |
| E33 | the geo line (`city`, `subdivision`, `region`, `country_code`) | `UserPanel` | `INLINE_LIMIT` | 3 × 128 + 8 |
| E34 | an unknown context's title (its key) | `ContextCards` | `INLINE_LIMIT` | 64 |
| E35 | a context headline (§7.2) | `ContextCards` | `INLINE_LIMIT` | contexts ≤ 64 KiB in total |
| E36 | an unlabelled context field's `dt` (its key) | `ContextCards` | `INLINE_LIMIT` | contexts ≤ 64 KiB in total |
| E37 | a context field's `dd`, `formatValue(v)` | `ContextCards` | `INLINE_LIMIT` | contexts ≤ 64 KiB in total |
| E38 | a non-object context value | `ContextCards`, in `<div class="context-scalar">` (§7.3) | `BLOCK_LIMIT`, block | contexts ≤ 64 KiB in total |

Labels, titles and headings this feature writes itself (`Release`, `Device`, `Exception 2 of 3`, `Not symbolicated`, …) are
constants, not event strings. The pre-v2 card's existing text (environment and platform badges, trace line, the `<pre>` stack and
the metadata dump) is today's markup, unchanged (§10), and is outside this inventory.

### 4.4 Level tones

`levelTone(level: unknown)` returns a class name from this fixed map, a `Map<string, string>` read with `.get()` (§4.2 rule 5):
`levelTone('constructor')` is `tone-neutral`, not a function. The class sets `color`, `background` and `border-color`
from variables that exist in `app.css` (T20):

| Level | Class | Variables |
|---|---|---|
| `fatal`, `error` | `tone-critical` | `--severity-critical-{text,bg,border}` |
| `warning` | `tone-warning` | `--status-open-{text,bg,border}`: hue 75, the same colour `DESIGN.md` documents as `--severity-warning-*` |
| `info` | `tone-info` | `--color-primary` text, `--color-primary-subtle` background, `--border-color` border |
| `debug` | `tone-debug` | `--status-ignored-{text,bg,border}` |
| anything else, or absent | `tone-neutral` | `--text-muted` text, no background, `--border-color` border |

The badge's text is the level string itself, through `ExpandableText(INLINE_LIMIT)` (E2, E18). Colour is never the only carrier. Ingest restricts both occurrence and crumb
levels to the five values (T17, `docs/sdk-specification.md:250`), so `tone-neutral` is reached only by an absent crumb level or a
hand-written row.

### 4.5 Components and placement

New files under `dw/src/lib/components/issues/`. Each is a Svelte 5 runes component with its own `<style>`, using only `app.css`
variables. No Tailwind, no UI kit (contract §5).

| File | Task | Props |
|---|---|---|
| `occurrence-display.ts` | all | the pure helpers named in this spec: `hasV2Fields`, `formatStackFrame` (moved verbatim from `+page.svelte:48-57`), `displayedFrame` (moved verbatim from `:78-80`), `frameVisibility`, `sourceContextLines`, `formatValue`, `levelTone`, `parseCrumbTime`, `anchorMs`, `formatRelative`, `contextCards`, `geoLine`, `chainDisplayOrder`, `clip`, and the constants `INLINE_LIMIT`, `BLOCK_LIMIT`. Every table among them is a `Map` (§4.2 rule 5). |
| `dw/src/lib/server/json-safe.ts` | §3.4 | `dropProtoKeys`, `cleanOccurrenceRow` (server-only) |
| `dw/src/lib/tag-filter.ts` | §6.3 | **shared with `sentinel-issue-tag-facets`**: `tagSearchHref` is imported from here, never redefined |
| `ExpandableText.svelte` | §4.3 | `{ text, limit, block? }` |
| `OccurrenceHeader.svelte` | TASK-04 | `{ occurrence, issueMessage }` |
| `StackFrames.svelte` | TASK-04, TASK-05 | `{ frames: unknown[]; showRaw: boolean }` |
| `TagsTable.svelte` | TASK-03 | `{ tags: unknown }` |
| `ExceptionChain.svelte` | TASK-05 | `{ exception: unknown; platform: string }` |
| `BreadcrumbTimeline.svelte` | TASK-02 | `{ breadcrumbs: unknown; occurredAt: string \| Date \| null }` |
| `RequestPanel.svelte` | TASK-03 | `{ request: unknown }` |
| `UserPanel.svelte` | TASK-03 | `{ user: unknown }` |
| `ContextCards.svelte` | TASK-01 | `{ contexts: unknown }` |

Every prop that comes from JSONB is typed `unknown` and narrowed inside the component. A wrong shape never throws, on the client
or under SSR: a body that is not the expected type renders nothing, or renders through `formatValue`. The rule for each panel is
in its section; the frame rule, which has to adapt a helper moved verbatim, is §5.2 "Frame shape".

**Order inside a v2 card** (`+page.svelte`'s `.occurrence-card`). ✦ marks what exists today, unchanged. † marks what renders only
on the selected occurrence and only when non-empty:

1. ✦ the header line, plus the select link or marker (§3.2)
2. `OccurrenceHeader`: the message and facts (§5.1)
3. ✦ the trace line
4. † `TagsTable` (§6.3)
5. ✦ `Stack Trace`: heading, badge and raw toggle as today, frames through `StackFrames` (§5.2)
6. † `ExceptionChain` (§9)
7. † `BreadcrumbTimeline` (§8)
8. † `RequestPanel` (§6.2)
9. † `UserPanel` (§6.1)
10. † `ContextCards` (§7)
11. ✦ the metadata toggle, last as today

Tags sit above the stack and contexts below breadcrumbs, as on Sentry's event page. Each panel is a `<section>` with an `<h4>`
title (`Tags`, `Exception Chain`, `Breadcrumbs`, `Request`, `User`, `Contexts`), matching `<h4>Stack Trace</h4>`. **A panel
whose data is empty renders nothing at all: no section, no heading** (contract §4.1, "empty sections are hidden"). Each section
defines what "empty" means for it.

Page edits are confined to: the script's `Occurrence` interface (it gains the six light keys as optional), the imports,
`formatStackFrame`/`displayedFrame` moving out, the markup **inside** `{#each group.occurrences as occurrence}` (moved into the
`occurrenceCard` snippet, §3.2), and the `selected-outside-window` block inside the Occurrence History section (§3.2 S3a). The
issue header, trend, relations and page heading are not touched, so the rebase against `sentinel-issue-tag-facets` (decision 3)
is confined to the occurrence section, and the tag link's encoder is one shared module (§6.3), not two.

### 4.6 Markup nesting (binding)

SvelteKit server-renders this page, and the browser's HTML parser **repairs** invalid nesting before hydration: `<p><pre>…</pre></p>`
becomes `<p></p><pre>…</pre><p></p>` (verified with `svelte` 5.55.5's `svelte/server` `render()` and jsdom 25's parser). Hydration
then walks a DOM that is not the one Svelte rendered. Svelte's compiler catches this only **inside one component** (it rejects
`<p><pre>` there with `element_invalid_closing_tag_autoclosed`); across a component boundary, such as `<p><ExpandableText block/></p>`,
it compiles, and prints only a dev-time `node_invalid_placement_ssr` warning. Every §12.3 component test mounts on the client,
where no parser runs, and `pnpm check` does not look across components. So the rule is written here and gated by SSR1-SSR3
(§12.3, review finding `odp-sr2-pre-in-p-ssr`).

**The rule: no block element inside a `<p>` anywhere,** and more generally no element where the HTML parser would move it:
1. A `block` `ExpandableText` (a `<div>` holding a `<pre>`, §4.3) is placed only in an element whose content model is flow
   content: `<div>`, `<section>`, `<article>`, `<li>`, `<dd>`, `<td>`. Never in `<p>`, `<span>`, `<code>`, `<a>`, `<button>`,
   `<time>`, `<dt>`, `<th>`, `<option>` or `<h1>`-`<h6>`.
2. An inline `ExpandableText` (a `<span>` holding text and at most a `<button>`) may go anywhere phrasing content may, except inside
   `<a>`, `<button>` or `<option>` (interactive content, and option text is text only). Those three hold `clip(…)` text or a
   constant.
3. Every `<table>` this feature renders has an explicit `<tbody>`; a `<tr>` is never a direct child of `<table>` (the parser would
   insert one).
4. `<dt>`/`<dd>` appear only inside a `<dl>`; `<li>` only inside an `<ol>`/`<ul>`.

**The audit**, every container this feature renders an event string in (the E ids of §4.3):

| Container | Element | Holds | Allowed by |
|---|---|---|---|
| message (E1) | `<div class="occurrence-message">` | block | rule 1 |
| fact values (E2-E8) | `<dd>` in `<dl class="occurrence-facts">`; the level badge is a `<span>` | inline | rule 2 |
| frame line (E9) | `<code class="frame-line">` in `<li class="frame">` | inline | rule 2 |
| frame module (E10) | `<span class="frame-module">` | inline | rule 2 |
| source line (E11) | `<code>` in `<li>` of `<ol class="frame-context">` | inline | rule 2 |
| tag key (E12) | `<th scope="row">` in `<tbody>` | inline | rules 2, 3 |
| tag value (E13) | `<a>` in `<td>` | `clip` text | rule 2 |
| chain heading (E14, E15) | `<h5>` | inline | rule 2 |
| mechanism (E16) | `<p class="mechanism">` | inline | rule 2 |
| raw crumb time (E17) | `<time class="crumb-time">` | inline | rule 2 |
| crumb level, category, type, message (E18-E21) | `<span class="crumb-level|crumb-category|crumb-type|crumb-message">` in `<li class="crumb">` | inline | rule 2 |
| crumb data (E22) | `<div data-testid="crumb-data">` in `<li class="crumb">` | block | rule 1 |
| method, URL (E23, E24) | `<span class="method">`, `<span class="request-url">` in `<p class="request-line">` | inline | rule 2 |
| query string (E25) | `<dd>` in `<dl class="request-query">` | inline | rules 2, 4 |
| header name, value (E26, E27) | `<th scope="row">`, `<td>` in `<tbody>` | inline | rules 2, 3 |
| request body (E28) | `<div class="request-body">` | block | rule 1 |
| user fields (E29-E33) | `<dd>` in `<dl data-testid="user-panel">` | inline | rules 2, 4 |
| unknown context title (E34) | `<h5>` | inline | rule 2 |
| context headline (E35) | `<p class="context-headline">` | inline | rule 2 |
| context field key, value (E36, E37) | `<dt>`, `<dd>` in `<dl>` | inline | rules 2, 4 |
| non-object context (E38) | `<div class="context-scalar">` | block | rule 1 |
| filter options (E18, E19) | `<option>` | `clip` text | rule 2 |

The other containers hold no event string, or only one of the rows above: the header line `<div class="occurrence-header">` (the
select `<a>` or marker `<span>`), `<div class="selected-outside-window">` (a text-only `<p class="occurrence-window">` and the card
`<div>` as siblings), `<p data-testid="crumb-count">` and `<p data-testid="crumb-guard">` (text only), each panel's `<section>`
and `<h4>`, `<article class="chain-value">`, and the `<li class="frames-toggle">` holding its `<button>`. The two `<p>` elements that
held a block in round 1, `occurrence-message` and `context-scalar`, are `<div>` now.

---

## 5. Header and frames (TASK-04)

### 5.1 Occurrence header (`OccurrenceHeader.svelte`)

Rendered on **every v2** card, selected or not.

- **Message:** `<div class="occurrence-message" data-testid="occurrence-message">`, a `<div>` and never a `<p>`, because it holds
  a block (§4.6). The text is `occurrence.message` when it is a non-empty string, otherwise `issueMessage` (the page passes
  `data.issue.message`), shown through `ExpandableText(limit BLOCK_LIMIT, block)`: a message can span lines, and the `<pre>` keeps
  them (E1). The fallback applies to v2 rows only, whose `message` is NULL when the SDK sent `""` (T8, `NULLIF`). A pre-v2 card gets
  no message line at all (§10).
- **Facts:** `<dl class="occurrence-facts" data-testid="occurrence-facts">`, in this order. A fact renders only when its value is
  a non-empty string. Every `dd` value goes through `ExpandableText(INLINE_LIMIT)` (E2-E8, §4.3):

| `dt` | `dd` | Source |
|---|---|---|
| `Level` | a badge with `levelTone(level)` and the level text | `level` |
| `Release` | the value | `releaseVersion` (pre-v2 column, rendered for v2 rows only) |
| `Dist` | the value | `dist` |
| `Event ID` | the value, in `var(--font-mono)` | `eventId` (pre-v2 column, rendered for v2 rows only) |
| `Server` | the value | `serverName` |
| `Transaction` | the value | `transaction` |
| `SDK` | `name version`, or whichever one is a non-empty string | `sdk.name`, `sdk.version`. It renders nothing when `sdk` is not an object or both are empty. |

The issue header above (`+page.svelte:368-528`) is unchanged: it stays the issue's.

### 5.2 Frames (`StackFrames.svelte`)

Used by a v2 card's `Stack Trace` block, which keeps today's `.stacktrace` wrapper, heading, `symbolicationBadge` and raw toggle
(`+page.svelte:603-626`) and swaps only the `<pre><code>` for `<StackFrames frames={occurrence.stacktrace} showRaw={showRaw} />`.
`ExceptionChain` also uses it, with `showRaw={false}`. A pre-v2 card keeps the `<pre><code>` (§10).

Markup: `<ol class="frames" data-testid="frames">`. One `<li class="frame" data-testid="frame" data-in-app="true|false">` per
visible frame, in stored order (crashing frame first, T11).

**Frame shape (binding, review finding `odp-sr2-frame-shape-throw`).** `formatStackFrame` moves verbatim (§4.5) and reads
`frame.file.length`, so handing it a `null` element, or a frame without a string `file`, throws while rendering, and under SSR
that is a 500 for the whole page. The processor never writes such a frame (`StackFrame.File` has no `omitempty`), but a
hand-written row can. So `StackFrames` never passes a stored frame to it directly:
- `frameObjects(frames)` in `occurrence-display.ts` keeps only the elements that are non-null, non-array objects. Every other
  element (`null`, a number, a string, an array) is **skipped**: not rendered, not counted, not given to `frameVisibility`. An
  empty result renders nothing, like an empty `frames`.
- `frameLine(frame, showRaw)` in `occurrence-display.ts` builds the text: `shown = displayedFrame(frame, showRaw)`, except that
  `raw` is used only when it is a non-null, non-array object (a string or `null` `raw` falls back to the frame itself). It then
  calls `formatStackFrame({ file: text(shown.file), function: text(shown.function), line, column })`, where `text(v)` is `v` for a
  string, `<unknown>` for `undefined` or `null`, and `formatValue(v)` otherwise (§4.2); `line` is `shown.line` when it is a finite
  number, else `0`, the wire's "unknown"; `column` is `shown.column` when it is a finite number, else absent. A well-formed frame
  (string `file`/`function`, numeric `line`) gives exactly `formatStackFrame(displayedFrame(frame, showRaw))`, so F1 and F5 hold.
- `sourceContextLines` reads `raw.line` only when `raw` is such an object (the numbering base below).
- On a v2 card, the page computes the symbolication badge from `frameObjects(occurrence.stacktrace)`, not from the raw array,
  since today's `countSymbolicated` reads `frame.symbolicated` and throws on a `null` element too. A pre-v2 card keeps today's
  code exactly (§10); its exposure to a hand-written `null` frame is pre-existing and recorded as O7 (§14).
- F7 (§12.3) proves all of this on the client, and SSR2 under `svelte/server`.

Inside each frame:

1. **The frame line:** `<code class="frame-line">` holding `frameLine(frame, showRaw)` (above) through
   `ExpandableText(BLOCK_LIMIT)`, inline (E9). This is the same string today's `<pre>` holds for that frame (`:627`), and a
   frame line under 2 048 code points renders as exactly that text. A frame with none of the v2 keys therefore reads exactly as today, and
   the raw toggle swaps it exactly as today.
2. **In-app mark:** when `frame.in_app === true`, the text `<span class="in-app" data-testid="in-app">in app</span>`.
   `data-in-app` is `"true"` only then. Any other value, missing included, counts as not in app. System frames are drawn in
   `--text-muted`.
3. **Module:** when `frame.module` is a non-empty string, `<span class="frame-module" data-testid="frame-module">` shown through
   `ExpandableText(INLINE_LIMIT)`.
4. **Source context:** when `frame.context_line` is a string, render
   `<ol class="frame-context" data-testid="frame-context">` from `sourceContextLines(frame)`:
   - `pre_context` lines, then `context_line`, then `post_context` lines. Each context array is kept only if it is an array, and
     only its string elements.
   - Each line is `<li>` with a number gutter `<span class="lineno">` and the code `<code>`.
   - The `context_line` item has `class="current"` and `aria-current="true"`, drawn with `--color-primary-subtle` background.
   - Numbering anchors on `base = frame.raw?.line ?? frame.line` (with `raw` read only when it is an object, above). `raw` holds what the SDK sent (`event.go:194-201`), and the SDK
     captured the context at **its** line. Symbolication rewrites `line` and never the context (`event.go:510-529`). So a
     symbolicated frame's context is numbered from the line it was read at, in both raw and symbolicated mode.
   - `pre_context[i]` is `base − len(pre) + i`, `context_line` is `base`, and `post_context[i]` is `base + 1 + i`.
   - If `base` is not an integer ≥ 1, or `base − len(pre) < 1`, the gutter is omitted for **all** lines. That is better than
     printing `0` or a negative number. `0` is the wire's "unknown" (`+page.svelte:45-47`).
   - `pre_context`/`post_context` without a `context_line` are not rendered: there is no line to anchor or highlight. Sentry's
     definition anchors both on `context_line` (`stacktrace.mdx`).
   - Each line's code goes through `ExpandableText(BLOCK_LIMIT)`, inline (E11). Context lines are ≤ 1024 each at ingest (T16), so
     a processor-written line never shows a button; a longer hand-written one collapses. The block scrolls horizontally, like
     today's `pre`.

**Collapsing system frames (`frameVisibility(frames)`)** is Sentry's `frameIsVisible` (`crashContent/stackTrace/content.tsx`),
translated to Sentinel's frame order:
- Let `app[i] = frames[i].in_app === true`.
- **Collapse is active only when some frames are in app and some are not.** With no in-app frame, or all in-app, every frame is
  visible. This matches Sentry's forced full view (`interfaces/exception.tsx`, `forceFullStackTrace`, and `hasSystemFrames`,
  which in `sentry/interfaces/stacktrace.py` means "has any in-app frame").
- When active, frame `i` is **visible** iff `app[i]`, **or** `i > 0 && app[i-1]`, **or** `i === 0`.
  - In Sentry's order, `nextFrame` is the callee, which is Sentinel's `i-1`, so `i > 0 && app[i-1]` is "the frame that called
    into app code".
  - Sentry's "the last non-app frame" (no `nextFrame`) is Sentinel's crashing frame, `i === 0`.
- A maximal run of consecutive hidden frames is replaced by one
  `<li class="frames-toggle"><button type="button" aria-expanded="false" data-testid="show-frames">Show {n} more frame{n === 1 ? '' : 's'}</button></li>`,
  Sentry's `Show %s more frames`. Clicking expands **that run only** and relabels the button `Hide {n} more frame(s)`
  (`aria-expanded="true"`). The state is per stack and per run, and it resets when the component remounts.
- The raw toggle does not change `in_app` or visibility: `in_app` is read from the frame itself in both modes.

An empty or non-array `frames` renders nothing. The page already renders the `Stack Trace` block only when
`stacktrace.length > 0` (`:603`).

---

## 6. User, request and tags (TASK-03)

Each panel renders on the selected v2 occurrence only, from `occurrenceDetail`.

### 6.1 User (`UserPanel.svelte`)

A `<dl data-testid="user-panel">`. Each row renders only when its value is a non-empty string, and each `dd` goes through
`ExpandableText(INLINE_LIMIT)` (E29-E33):

| `dt` | `dd` |
|---|---|
| `ID` | `user.id` |
| `Username` | `user.username` |
| `Email` | `user.email` |
| `IP Address` | `user.ip_address` |
| `Geo` | `geoLine(user.geo)` |

`geoLine(geo)` builds the line in three steps:
1. `parts` = the non-empty strings among `geo.city`, `geo.subdivision`, `geo.region`, in that order, joined with `, `.
2. If `geo.country_code` is a non-empty string, `parts` becomes `${parts} (${country_code})`, or just `country_code` when `parts`
   is empty.
3. `''` when `geo` is not an object.

Every stored field is shown verbatim. `region` is not relabelled "country", because an SDK-sent geo may use it differently
(§1.3). With enrichment the line reads `Jakarta, Jakarta, Indonesia (ID)`.

**Empty:** `user` is not an object, or no row has a value. `{}` is empty (T9).

Email, IP and geo are shown unmasked: contract decision 1 ("storing and showing … is OK for now").

### 6.2 Request (`RequestPanel.svelte`)

A `<section data-testid="request-panel">`:

1. **Request line:** `<p class="request-line">`. It contains `request.method` (a `<span class="method">`, when non-empty) and
   `request.url` (a `<span class="request-url">`), each through inline `ExpandableText(INLINE_LIMIT)` (E23, E24), so the `<p>`
   holds phrasing content only (§4.6). Both are text. **No `<a>`** (§4.2).
2. **Query string:** `<dl class="request-query"><dt>Query</dt><dd>…</dd></dl>`, the `dd` holding `query_string` through
   `ExpandableText(INLINE_LIMIT)`, when it is non-empty.
3. **Headers:** `<table class="headers" data-testid="request-headers"><tbody>…</tbody></table>`, when `headers` is an object with
   at least one entry.
   - One row per entry with a string value: `<th scope="row">` name, `<td>` value, each through `ExpandableText(INLINE_LIMIT)`.
   - Rows are sorted by `name.toLowerCase()`, then by `name`, comparing code units. Postgres `jsonb` does not keep the order the
     client sent (it orders keys by length, then bytes), so no stored order exists to keep, and a stable one is chosen.
4. **Body:** `<div class="request-body" data-testid="request-body">`, when `data` is neither `undefined` nor `null`:
   - a string is shown verbatim;
   - anything else is `JSON.stringify(data, null, 2)`;
   - either way through `ExpandableText(BLOCK_LIMIT, block)`.

   This is the contract's "pretty-printed and collapsible". Over `BLOCK_LIMIT` the body starts collapsed, and its full text is one
   click away.

Values are rendered as stored. A masked value reads `***REDACTED***` (T13). Nothing is unmasked, parsed or re-derived: no cookie
splitting, no query parsing, no URL construction.

**Empty:** `request` is not an object, or it has none of items 1-4. `request.env` is never rendered (§2), so a request carrying
only `env` counts as empty.

### 6.3 Tags (`TagsTable.svelte`)

`<table data-testid="tags-table"><tbody>…</tbody></table>` with one row per entry of `tags` whose value is a string:
- `<th scope="row">` holding the key through `ExpandableText(INLINE_LIMIT)` (E12), and `<td>` holding
  `<a href={tagSearchHref(key, value)} data-testid="tag-link">{clip(value, INLINE_LIMIT)}</a>` (E13).
  - The value is clipped rather than expandable because an `<a>` may not contain a button (HTML's content model forbids
    interactive content inside `<a>`). Ingest bounds a tag value at 200 code points (T18), under `INLINE_LIMIT`, so a
    processor-written value is never clipped. The full value of a longer hand-written one is what the link searches for.
- Rows are sorted by key, comparing code units. Keys are ASCII by the ingest pattern (T18), so this is also byte order, the
  order the ingestor's `sortedKeys` uses.

**The href, exactly (contract decisions 6 and 7):**

```ts
tagSearchHref(key, value) = '/search?tag=' + encodeURIComponent(encodeURIComponent(key) + ':' + encodeURIComponent(value))
```

**One encoder, shared (coordinator ruling, binding across both runs).** The implementation lives in exactly one module,
**`apps/dashboard-web/src/lib/tag-filter.ts`**, as **`tagSearchHref(key: string, value: string): string`**, the name and signature
`sentinel-issue-tag-facets` fixes in its spec (branch `NurfitraPujo/sentinel-issue-tag-facets-specifying` @ `153f6db`, §6.1 and
§11.3). `TagsTable.svelte` imports it from `$lib/tag-filter`. No other file in either run defines a tag-link encoder, and
`occurrence-display.ts` does not export one.
- **If tag facets merges first,** this run imports what it added and adds nothing to `tag-filter.ts`.
- **If this run merges first,** it creates `tag-filter.ts` exporting `formatTagParam(key, value)` =
  `encodeURIComponent(key) + ':' + encodeURIComponent(value)` and `tagSearchHref(key, value)` = `'/search?tag=' +
  encodeURIComponent(formatTagParam(key, value))`, and nothing else. Tag facets then adds its parser and grammar to the same
  module when it rebases.
- **`URLSearchParams` is never used to build the href** (contract decision 7, tag-facets decision 10, `bc275e6`). It
  serialises `!`, `'`, `(`, `)` and `~` differently: `encodeURIComponent` leaves those five bare, `URLSearchParams` escapes them
  (`(` → `%28`). The `encodeURIComponent` form above is the one normative href, whichever run creates the module. If tag facets
  created it with the `URLSearchParams` body of its spec §6.1 @ `153f6db`, TG1's `it's (x)!~` row goes red, and this run's PR
  changes that one line to the normative form (the file is inside `apps/dashboard-web`, so G5 allows it). Both spellings decode to
  the same `tag` value, so the search side's parser accepts either (tag-facets decision 10); only the href is pinned.

- Each part is encoded first, so the only literal `:` left after **one** decode is the separator. A key or value containing `:`
  stays unambiguous (T18: a key can contain `:`).
- The joined string is then encoded again as one query value. So `:` becomes `%3A`, and every `%` of the inner encoding becomes
  `%25`: a space is `%2520`, a `+` is `%252B`, a `:` inside a part is `%253A`. Nothing in a tag can end the parameter, add a second
  one or start a fragment, and no literal `+` survives, so `+`-as-space decoding cannot alter it.
- **How the search side reads it** (tag facets, contract decision 6 / tag-facets decision 8): `URLSearchParams.get('tag')` (one
  decode), split at the single literal `:`, then `decodeURIComponent` on each part. TG3 runs exactly that and gets the pair back.
- It is one `tag` parameter per link. "Repeatable" (contract §4.3) means the search side accepts several; this panel emits one per
  link.
- Until tag facets ships, `/search` ignores the parameter (T19, decision 3). That is accepted.

Pinned examples (§12.3 TG1, TASK-03), computed with Node 24 from the normative form:

| key | value | `href` |
|---|---|---|
| `browser` | `Chrome 120` | `/search?tag=browser%3AChrome%2520120` |
| `os.name` | `Android` | `/search?tag=os.name%3AAndroid` |
| `release` | `2.14.0+2140` | `/search?tag=release%3A2.14.0%252B2140` |
| `url` | `/a?b=1&c=2#x` | `/search?tag=url%3A%252Fa%253Fb%253D1%2526c%253D2%2523x` |
| `k:v` | `x` | `/search?tag=k%253Av%3Ax` (a key containing `:`) |
| `a:b` | `c:d e/%` | `/search?tag=a%253Ab%3Ac%253Ad%2520e%252F%2525` (TG3's round trip) |
| `k` | `it's (x)!~` | `/search?tag=k%3Ait's%2520(x)!~` (decision 7: `'`, `(`, `)`, `!`, `~` stay bare in both layers; `URLSearchParams` would give `/search?tag=k%3Ait%27s%2520%28x%29%21%7E`) |

**Empty:** `tags` is not an object, or no entry has a string value.

---

## 7. Context cards (TASK-01)

### 7.1 Which cards, in which order

`contextCards(contexts)` returns one card per key of `contexts` (an object; anything else gives no cards):
- **The well-known keys come first, in this order:** `device`, `os`, `client_os`, `app`, `runtime`, `browser`, `culture`,
  `trace`. This is the contract's list and order. The first six are also the ingestor's own well-known set (`bounds.go:122-125`).
- **Every other key follows**, sorted by key in code-unit order, rendered generically (§7.3).

**Titles:** `device` → `Device`, `os` → `Operating System`, `client_os` → `Client Operating System`, `app` → `App`, `runtime` →
`Runtime`, `browser` → `Browser`, `culture` → `Culture`, `trace` → `Trace`. Any other key's title is the key verbatim. The order,
the titles, the field lists and the labels are `Map`s read with `.get()`/`.has()` (§4.2 rule 5), so `constructor`, `toString`,
`hasOwnProperty` and `__proto__` are unknown keys like any other.
- `os` and `client_os` are **both** rendered when both exist, under those two distinct titles (T15).
- Relay has no client-OS type: `client_os` is a key holding an OS context (`normalize/user_agent.rs`), so it takes `os`'s field
  list.

### 7.2 Well-known fields and labels

From Relay's `protocol/contexts/*.rs` and Sentry's `contexts.mdx`. Fields render in this order when present, then the object's
other keys (§7.3):

| Card | Headline (`<p class="context-headline">`, inline only, §4.6) | Fields, in order: `key` → label |
|---|---|---|
| `device` | `model`, else `name` | `name` Name, `family` Family, `model` Model, `model_id` Model ID, `manufacturer` Manufacturer, `brand` Brand, `arch` Architecture, `simulator` Simulator, `device_type` Device Type, `orientation` Orientation, `screen_resolution` Screen Resolution, `screen_width_pixels` Screen Width, `screen_height_pixels` Screen Height, `screen_density` Screen Density, `screen_dpi` Screen DPI, `battery_level` Battery Level, `battery_status` Battery Status, `charging` Charging, `online` Online, `low_memory` Low Memory, `memory_size` Memory Size, `free_memory` Free Memory, `usable_memory` Usable Memory, `storage_size` Storage Size, `free_storage` Free Storage, `external_storage_size` External Storage Size, `external_free_storage` External Free Storage, `processor_count` Processor Count, `processor_frequency` Processor Frequency, `cpu_description` CPU, `boot_time` Boot Time, `timezone` Timezone |
| `os`, `client_os` | `name version` | `name` Name, `version` Version, `build` Build, `kernel_version` Kernel Version, `rooted` Rooted, `distribution_name` Distribution, `distribution_version` Distribution Version, `distribution_pretty_name` Distribution Name, `raw_description` Raw Description |
| `app` | `app_name app_version` | `app_name` Name, `app_identifier` Identifier, `app_version` Version, `app_build` Build, `build_type` Build Type, `app_start_time` Start Time, `in_foreground` In Foreground, `app_memory` Memory, `device_app_hash` Device App Hash, `view_names` View Names |
| `runtime` | `name version` | `name` Name, `version` Version, `build` Build, `raw_description` Raw Description |
| `browser` | `name version` | `name` Name, `version` Version |
| `culture` | `locale` | `locale` Locale, `display_name` Display Name, `calendar` Calendar, `timezone` Timezone, `is_24_hour_format` 24-Hour Format |
| `trace` | none | `trace_id` Trace ID, `span_id` Span ID, `parent_span_id` Parent Span ID, `op` Operation, `status` Status, `origin` Origin, `data` Data |

Headline rules:
- A headline part is used only when it is a non-empty string. Parts are joined with one space.
- If no part qualifies, there is no headline.
- The headline goes through `ExpandableText(INLINE_LIMIT)` (E35): a 64 KiB `device.model` shows 256 code points and a button.
- The headline repeats values that also appear as rows. The rows are the complete record, and the headline is the scan line
  (`Pixel 7`, `Android 14`, `Chrome 120.0`).

### 7.3 Rendering a card

`<section class="context-card" data-testid="context-card" data-context-key={key}>` containing an `<h5>` title. A well-known
title is a constant. An unknown card's title is its key, through `ExpandableText(INLINE_LIMIT)` (E34). Then:

- **Object value:** the optional headline, then `<dl>`:
  - First, the card's well-known fields in the §7.2 order, each with its label as `dt`.
  - Then every remaining key, sorted in code-unit order, with the key verbatim as `dt`, through `ExpandableText(INLINE_LIMIT)`
    (E36). On an unknown card, every key is
    "remaining".
  - `dd` is `formatValue(v)` (§4.2) through `ExpandableText(INLINE_LIMIT)`. `true`/`false`/numbers are shown as stored: no units,
    no yes/no, no byte formatting.
  - **The `type` key is skipped when it equals the card's key**, the Sentry convention ("You can omit `type` if the key name is
    the type"). Any other `type`, such as `client_os`'s `"os"`, renders as an ordinary row.
  - A key whose value is `undefined` is skipped. JSON has no `undefined`, so this matters only for hand-built props.
- **Non-object value**, for example the string `***REDACTED***` (T13, `schemas.ts:251`), a number or an array: `<div
  class="context-scalar">`, a `<div>` and never a `<p>` (§4.6), holding `formatValue(v, pretty)` through
  `ExpandableText(BLOCK_LIMIT, block)`. The redacted string is shown exactly as stored.
- **An object with no renderable row** (`{}`, or `{type: <key>}` only) produces **no card**.

**Empty:** no card is produced. The whole `Contexts` section, heading included, is then absent.

---

## 8. Breadcrumbs (TASK-02)

### 8.1 The timeline

`<section data-testid="breadcrumbs">` renders when `breadcrumbs` is an array with at least one object element. Non-object
elements are skipped. Crumbs render as `<ol class="crumbs">` in **stored order** (oldest first, the SDK's order,
`breadcrumbs.mdx` "ordered from oldest to newest"), one `<li class="crumb" data-testid="crumb">` each, holding in order:

1. **Time:** `<time class="crumb-time" data-testid="crumb-time">` (§8.2).
2. **Level:** `<span class="crumb-level {levelTone(level)}">` showing `level` through `ExpandableText(INLINE_LIMIT)` (E18), when it
   is a non-empty string.
3. **Category:** `<span class="crumb-category">` holding `category` through `ExpandableText(INLINE_LIMIT)` (E19), when it is a
   non-empty string.
4. **Type:** `<span class="crumb-type">` holding `type` through `ExpandableText(INLINE_LIMIT)` (E20), when it is a non-empty string. No default is shown: an absent type or level is shown as
   absent, not as Sentry's implicit `default`/`info`, because the values are shown as stored.
5. **Message:** `<span class="crumb-message">` holding `message` through `ExpandableText(INLINE_LIMIT)` (E21), when it is a
   non-empty string.
6. **Data:** when `data` is an object with at least one key, a button `Show data` (`aria-expanded="false"`,
   `data-testid="crumb-data-toggle"`).
   - Clicking it renders `<div data-testid="crumb-data">` with `JSON.stringify(data, null, 2)` through
     `ExpandableText(BLOCK_LIMIT, block)`, and relabels the button `Hide data`.
   - **`data` is not rendered, stringified or measured until it is expanded.**
   - `data.url` is part of that text. It is never an anchor (§4.2).

### 8.2 Relative time

`parseCrumbTime(s)` returns epoch milliseconds or `null`:
- `s` must be a string matching `^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(\.\d+)?(Z|[+-]\d{2}:\d{2})$`.
- The fraction is cut or padded to exactly 3 digits (`.123456789` → `.123`, `.1` → `.100`, none → `.000`) before `Date.parse`. That
  keeps the result the same on every engine (T24). The processor writes nanoseconds.
- A non-match or `NaN` gives `null`.

The anchor is `occurredAt`, the occurrence's `createdAt`. That is the SDK's capture time, `CreatedAt: evt.Timestamp`
(`processor_service.go:331`), taken on the same client clock as the crumbs. The page passes the selected detail row's `createdAt`
**as the load returned it**, never converted: on the real page that is a `Date` (or `null`), and only hand-written test data holds
an ISO string (T29). So the anchor is defined for **both shapes** (review finding `odp-sr3-crumb-anchor-date`):

`anchorMs(v: string | Date | null | undefined): number | null`:
- `v instanceof Date`: `v.getTime()` when it is finite, otherwise `null` (an Invalid Date);
- a string: `parseCrumbTime(v)`;
- anything else (`null`, `undefined`, a number, an object): `null`.

A `Date` is **not** stringified and re-parsed: `String(date)` is not ISO and does not match the regex, and `toISOString()` throws on
an Invalid Date. Reading the anchor with `parseCrumbTime` alone would put every crumb on the real page into the "unanchored" row
below and never show a relative time, while every string-fixture test still passed. B4b and P1 are the tests that see the real
shape.

`formatRelative(deltaMs)`, where `deltaMs = crumb − occurrence`, and `a = |deltaMs|`:
- The sign is `-` when `deltaMs < 0`, `+` when `deltaMs > 0`, and nothing when it is `0`.
- `a < 60 000`: `{sign}{(a / 1000).toFixed(1)}s`.
- `a < 3 600 000`: `{sign}{⌊a / 60 000⌋}m {⌊(a mod 60 000) / 1000⌋}s`.
- otherwise: `{sign}{⌊a / 3 600 000⌋}h {⌊(a mod 3 600 000) / 60 000⌋}m`.

Pinned: `-4200` → `-4.2s`; `1500` → `+1.5s`; `0` → `0.0s`; `-125000` → `-2m 5s`; `-7380000` → `-2h 3m`.

What the `<time>` element shows:

| Crumb `timestamp` | `anchorMs(occurredAt)` | Text | `title` (hover) | `datetime` |
|---|---|---|---|---|
| parseable | a number (a valid `Date`, or a parseable string) | `formatRelative(…)` | the stored `timestamp` string, verbatim (absolute, UTC) | the stored string |
| parseable | `null` (`null`, an Invalid `Date`, an unparseable string) | the stored `timestamp` string | none | the stored string |
| a non-empty string that does not parse | any | **the raw string, verbatim**, through `ExpandableText(INLINE_LIMIT)` (E17), never `NaN` or `Invalid Date` | none | none |
| absent, `""` or not a string | any | `—` | none | none |

**Every other timestamp this feature reads from page data** (the audit the coordinator's ruling on `odp-sr3-crumb-anchor-date`
asked for), and what each does with a `Date`:

| Consumer | Value | Shape handled | Proof |
|---|---|---|---|
| crumb anchor (above) | `occurrenceDetail.createdAt` | `Date` and string, through `anchorMs` | B4, B4b, B5, P1 |
| crumb `timestamp` (above) | a JSONB string | always a string: JSONB has no date type, so it never arrives as a `Date` | B4 |
| the card header line (T1, unchanged) | each card's `createdAt`, the S3a card's included | the existing `formatDate(date: string \| Date \| null)`, which calls `new Date(v)` on either shape (T29); not edited | P1 (a `Date`), P4 |
| the date groups (unchanged) | `data.occurrences[].createdAt` | the existing `getDateGroup(string \| Date \| null)` (T29); not edited; the S3a card is rendered outside the groups (§3.2) | P7 |
| `cleanOccurrenceRow` / `dropProtoKeys` (§3.4) | every row's `createdAt` | a `Date` is returned by identity and never walked | J1, L11 (`createdAt` is still a `Date`) |
| §3.3 measurement, M1 | the fixture's `createdAt` | the fixture holds the ISO string. `JSON.stringify` writes a `Date` as that same string, so every JSON figure is identical for either shape. `devalue` is not: it writes a `Date` as `["Date","…"]`, **9 bytes more per row** than the string (measured, devalue 5.8.0), so the real page's devalue payload is 909 bytes above the fixture's for 100 rows plus the detail row, under 0.3 % of the page. Decision 1 is unaffected | G7 |
| the P4 golden | pre-v2 cards' header `formatDate` text | `formatDate` has no `timeZone` (T29), so its text depends on the process's time zone and a golden captured under one `TZ` would fail under another. The golden capture and `page.occurrence-compat.test.ts` therefore set `process.env.TZ = 'UTC'` as the file's first statement (vitest 2's default `forks` pool gives each file its own process, and Node re-reads `TZ` on assignment), and P4 first asserts `new Date(0).getTimezoneOffset() === 0`, so a pin that did not take fails loudly rather than as a golden mismatch. The golden's header lines read in UTC | P4 |

No other timestamp from page data is read by this feature: `receivedAt` is not selected (§3.1), and a context field that holds a
time (`app.app_start_time`, …) is a JSONB string rendered as stored (§7.3).

### 8.3 Filters and the rendering bound

**Filters** sit above the list, and both start at "all":
- `<select aria-label="Filter by category" data-testid="crumb-category-filter">`: `All categories`, then each distinct non-empty
  `category` in code-unit order.
- `<select aria-label="Filter by level" data-testid="crumb-level-filter">`: `All levels`, then each distinct non-empty `level`
  present, ordered `fatal error warning info debug` and any other value after those in code-unit order.
- Each `<option>` has `value` = the category or level string and label `clip(value, INLINE_LIMIT)` (§4.3). The distinct values
  are collected in a `Set`, and the level order is a `Map` from level to rank, so a category `constructor` or a level `toString`
  is an ordinary option (§4.2 rule 5).
- A crumb is shown when it matches both filters. A crumb with no category is shown only under `All categories`, and likewise for
  level.
- A line `<p data-testid="crumb-count">Showing {shown} of {total}</p>` renders whenever a filter is not "all".
- The filter state is local to the component, one per occurrence.

**The bound: all crumbs render, and `data` is deferred.**
- Ingest caps an event at 100 crumbs and 128 KiB (T16). Each crumb renders about 5 short text nodes, so 100 crumbs cost about
  500 nodes, less than the frames of one 100-frame stack (§3.3 fixture).
- The expensive part, `data` (up to 4 KiB each, 100 × 4 KiB of pretty JSON), is exactly what §8.1 item 6 defers. Only a crumb the
  triager opens is stringified.
- Rendering "at most N initially" was rejected. It would hide crumbs from the filters' "Showing n of N" count, and the ingest
  bound already caps N at 100.
- For a row written outside the ingest bound (more than 100 crumbs), the component renders the **last 100**, the ones nearest the
  error, and a line `<p data-testid="crumb-guard">Showing the 100 most recent of {n} breadcrumbs.</p>`. This is a guard, not a
  feature. §12.3 tests it with 150.

---

## 9. Exception chain (TASK-05)

### 9.1 Order and placement

`ExceptionChain` renders on the selected v2 occurrence **only when `exception.values` is an array with more than one object
element**. With 0 or 1 values it renders nothing. A single value is already the event's top-level stack, and the contract forbids a
duplicate block.

**Order: outermost first.**
- `chainDisplayOrder(values)` is `values` reversed: the last stored value, the exception raised last, comes first.
- The SDK specification stores values oldest first (`errors.mdx`: "**MUST** be sorted oldest to newest", T12).
- Sentry displays them newest first by default: `crashContent/exception/content.tsx`, `if (newestFirst) children.reverse()`, and
  `interfaces/utils.tsx` `isStacktraceNewestFirst()` returns `true` by default.

**The top-level `Stack Trace` block stays** above the chain, unchanged (§4.5 item 5). The chain is added below it; it does not
replace it. Three reasons:
- The top-level `stacktrace` is the only symbolicated stack, and the only one with the badge and raw toggle (T12). Replacing it
  would regress TASK-04's "the symbolication badge and raw-frames toggle keep working" for every chained JS event.
- It is the fingerprint input (`docs/sdk-specification.md:259-261`).
- The chain's outermost value usually repeats those frames unsymbolicated, and it is labelled so (§9.2).

A 3-value chain therefore renders 3 chain stacks in the chain section, which is the contract §5 criterion, plus the page's own
top-level block.

### 9.2 One value

`<article class="chain-value" data-testid="chain-value">`, in display order:
- **Heading:** `<h5>`: `type` through `ExpandableText(INLINE_LIMIT)` (E14, when non-empty), then `: `, then `value` through
  `ExpandableText(INLINE_LIMIT)` (E15, when non-empty).
  If both are empty, the heading is `Exception {k} of {n}`.
- **Mechanism:** when `mechanism` is an object, `<p class="mechanism" data-testid="mechanism">`:
  - `mechanism: {type}` when `type` is non-empty, `type` through `ExpandableText(INLINE_LIMIT)` (E16);
  - `handled: true` or `handled: false` when `handled` is a boolean. The field is tri-state (`event.go:177-181`), and an absent
    `handled` prints nothing. The two parts are separated by ` · `.
  - The paragraph is omitted if both parts are absent.
- **Stack:** `<StackFrames frames={value.stacktrace} showRaw={false} />`, the same renderer as §5.2 (contract §4.6). There is no
  raw toggle, because chain frames have no `raw`. A value with no or empty `stacktrace` renders no stack.
- **Unsymbolicated label (decision 4):** when the occurrence's `platform` is `javascript` or `browser`, and the value's stack has
  at least one frame, and none of its frames has `symbolicated === true` (always the case today, T12), the stack is preceded by
  `<span class="badge unsymbolicated" data-testid="chain-unsymbolicated">Not symbolicated</span>` with
  `title="Chained exception frames are stored as received; only the top-level stack trace is symbolicated."`.
  - One label per stack labels every frame in it. That is the contract's "an unsymbolicated chained JS frame is labelled".
  - The platform set is the processor's default `SYMBOLICATE_PLATFORMS` (`main.go:95`). A Go or Dart chain gets no label,
    because nothing would ever symbolicate it.
  - If the processor follow-up ever symbolicates chain frames, the "none of its frames" condition removes the label without a
    dashboard change.

**Collapse beyond three:** Sentry's `initialCollapse={excIdx < values.length - 3}`.
- Values at display index ≥ 3 (the 4th outermost and older) start collapsed. They show only the heading and a button
  `Show exception` (`aria-expanded="false"`, `data-testid="chain-expand"`), and clicking it renders the mechanism and stack.
- A 3-value chain has nothing collapsed.

---

## 10. Compatibility (binding)

1. **A pre-v2 occurrence renders the same content as at the merge base** (`$(git merge-base origin/main HEAD)`, which was
   `f49721f` when this spec was written). This is proved by comparing the rendered card against a golden captured from the merge
   base's page (§12.2 `pre-v2-cards`):
   - Comparison is **after** normalising away Svelte's comment anchors (`<!---->`, which new `{#if}` blocks add even when false),
     scoped class hashes (`svelte-[a-z0-9]+`, which change whenever a `<style>` changes), and the extra whitespace a false
     `{#if}` leaves behind (review finding `odp-sr2-p4-whitespace`). Svelte keeps the template's whitespace on **both** sides of
     each block's anchor, so inserting `{#if}` between two existing children turns `<div>h</div> <div>t</div>` into
     `<div>h</div> <!----> <div>t</div>`, which is two whitespace text nodes once the comment is gone (verified, svelte 5.55.5).
     The normaliser therefore **collapses every run of adjacent whitespace-only text nodes outside `<pre>` into one space**.
     Those nodes carry no content: outside `<pre>`, HTML renders a run of whitespace as one space whatever its length. Inside a
     `<pre>`, whitespace is content and is kept byte for byte. The exact steps are in §12.2.
   - None of the three is content. Every element, attribute, class name the page wrote and every text node with content must
     match. A separator that disappears (`<span>a</span> <span>b</span>` becoming `<span>a</span><span>b</span>`) is a visible
     change, and the normaliser does not hide it (N3).
   - That covers: no select link or marker, no message or facts, the `<pre><code>` stack with badge and raw toggle, the metadata
     toggle and dump, and no panels, even when that occurrence is the selected one.
2. The **metadata toggle** and dump stay on every card, v2 included, unchanged.
3. **Nothing in the agent API changes.** `git diff $(git merge-base origin/main HEAD) -- dw/src/lib/server/agent-api-spec
   dw/src/lib/db/queries/agent-reads.ts dw/src/routes/api/agent` is empty, and `openapi-drift.test.ts` and `contract.test.ts` pass
   unedited.
4. **Moving the card markup into a snippet** (§3.2) adds no element, attribute or text node with content, only comment anchors and
   whitespace-only text, which the golden's normaliser removes and collapses. P4 runs after the move.
5. **`dropProtoKeys`** returns a deep-equal copy of any value without a `__proto__` key (§3.4), so no page that renders today
   changes. J1 pins the deep equality.
6. **Unknown or malformed `?occurrence=` values** keep rendering the page (§3.2 S3c, S4), as today (T28).
7. The page's other data (`issue`, `trend`, `relations`, `releases`, `canWrite`, …) and every existing
   `dw/src/routes/issues/[id]/*.test.ts` keep passing. The one existing test file that changes on purpose is
   `issue-queries.occurrences.test.ts` (§3.1, supersedes V7e). **No loader test is edited**: the load reads
   `url?.searchParams.get('occurrence') ?? null` (§3.2), so a test that calls `load` without a `url` (today's
   `page.server.test.ts` `loadAs`, and tag facets' `page.tag-facets.test.ts` if that PR merges first) gets S1, not a `TypeError`.
   L12 proves it (review finding `odp-sr3-facets-loader-url`).

---

## 11. Files touched

| File | Change |
|---|---|
| `dw/src/lib/server/queries/issue-queries.ts` | §3.1: the six light columns, the detail projection, `getOccurrenceDetail`, `occurrenceExistsOutsideIssue`, and the `:261-264` comment |
| `dw/src/lib/server/json-safe.ts` | new: §3.4 `dropProtoKeys`, `cleanOccurrenceRow` |
| `dw/src/lib/server/json-safe.test.ts` | new: §12.3 J1 |
| `dw/src/routes/issues/[id]/+page.server.ts` | §3.2: `url`, S1-S4, `selectedOccurrenceId`, `occurrenceDetail`; §3.4: `cleanOccurrenceRow` on every returned row |
| `dw/src/routes/issues/[id]/+page.svelte` | §4.5: interface, imports, the card markup (now the `occurrenceCard` snippet), the select link or marker, the S3a `selected-outside-window` block, and `formatStackFrame`/`displayedFrame` moved out |
| `dw/src/lib/components/issues/occurrence-display.ts` | new: the pure helpers |
| `dw/src/lib/components/issues/{ExpandableText,OccurrenceHeader,StackFrames,TagsTable,ExceptionChain,BreadcrumbTimeline,RequestPanel,UserPanel,ContextCards}.svelte` | new |
| `dw/src/lib/components/issues/occurrence-fixtures.ts` | new: shared test fixtures (§12.2). Imported only by tests, never by app code. |
| `dw/src/lib/components/issues/html-structure.ts` | new, test support only (imported only by tests): `normaliseCardHtml` (§12.2, the P4 golden normaliser) and `tagSequence`/`reparse` (the SSR1-SSR3 nesting check, §12.3) |
| `dw/src/lib/components/issues/html-structure.test.ts` | new: §12.3 N1-N4, the normaliser and the nesting check tested on their own |
| `dw/src/routes/issues/[id]/page.occurrence-ssr.test.ts` | new: §12.3 SSR1-SSR3, `// @vitest-environment node`, `svelte/server` `render()` |
| `dw/src/lib/tag-filter.ts` | **shared with `sentinel-issue-tag-facets`** (§6.3): imported for `tagSearchHref`. Created by this run, with `formatTagParam` and `tagSearchHref` only, **only if** this run's PR merges first; otherwise untouched. |
| `dw/src/lib/components/issues/*.test.ts`, `dw/src/routes/issues/[id]/page.occurrence-*.test.ts` | new: §12.3 |
| `dw/src/lib/server/queries/issue-queries.occurrences.test.ts` | rewritten: §12.3 Q1-Q3 |
| `dw/tests/fixtures/issue-page-pre-v2-cards.golden.html` | new: the golden (§12.2), captured from the merge base; its first line is an HTML comment naming that commit |
| `dw/package.json`, `dw/pnpm-lock.yaml` | the run's **two devDependencies**, both exact (§2): `"devalue": "5.8.0"`, the version `@sveltejs/kit` resolves in the lockfile (`:2903`, T25), which gains an importer entry and no package; it is what L11 imports and what `measure-payload.mjs` finds, with no environment variable (§12.4). And `"@types/jsdom": "21.1.7"`, the typings line current for the installed `jsdom` 25 (T31), which gains `@types/jsdom@21.1.7` and `@types/tough-cookie@4.0.5`; without it `html-structure.ts`'s `import { JSDOM } from 'jsdom'` fails G3 with TS7016 (review finding `odp-sr3-jsdom-types`). Added with `pnpm add -D -E devalue@5.8.0 @types/jsdom@21.1.7` |
| `dw/src/lib/db/queries/organizations.ts` | **§11.1 commit 1 only** (T30, O8): `users` added to the `$lib/db/schema` import block (`:2-10`). Nothing else in the file changes |
| `dw/src/lib/db/queries/organizations.members.test.ts` | new, **§11.1 commit 1**: §12.3 G3a |
| `dw/src/routes/+page.svelte` (the issue **list** page) | **§11.1 commit 1 only** (T30, O8): `import { untrack } from 'svelte'` and `let searchQuery = $state(untrack(() => data.filters.q ?? ''))` at `:162`. The `$effect` at `:163-165` is unchanged |
| `sentinel-occurrence-detail-panels/{spec.md,payload-fixture.mjs,measure-payload.mjs}` | this spec and its measurement (§12.4), committed with it |

No file outside `apps/dashboard-web` and this spec folder changes. No route file gains an export other than `load` (B12).

### 11.1 Commit plan

The pre-existing `pnpm check` debt (T30) is cleared in **its own commit, first**, so it is reviewable on its own and a revert of the
feature does not bring the red gate back (coordinator ruling on review finding `odp-sr3-g3-base-red`):

1. `fix(dashboard): import users in organizations.ts; untrack list-page searchQuery`. Exactly three files:
   `dw/src/lib/db/queries/organizations.ts` (the import), `dw/src/routes/+page.svelte` (the list page's `untrack`) and the new
   `dw/src/lib/db/queries/organizations.members.test.ts` (G3a). After it, at the merge base plus this commit alone, `pnpm check`
   reports `0 errors and 0 warnings` and `pnpm test` passes. No issue-page file is in it.
2. The feature, in whatever commits the implementer chooses (the TASK-01 to TASK-05 split is natural), each keeping G1-G4 green.
   The two devDependencies (§2) land with the first commit that imports them.

If the base already carries the `users` import or the `untrack` when the implementation starts (someone else fixed it), commit 1
drops that part, and its body says so; G3a is still added if `listOrganizationMembers` has no test calling it for real.

---

## 12. Verification

### 12.1 Gate and commands

The gate is **`task test-unit`**. Its dashboard line runs `pnpm --dir apps/dashboard-web test` (T23). Every test in §12.3:
- is a vitest file matched by `src/**/*.test.ts` or `tests/**/*.test.ts`;
- renders with jsdom, server-renders with `svelte/server` in a `// @vitest-environment node` file (SSR1-SSR3 only), or calls a
  function with mocked `$lib/server/db` / query modules, like the files it sits beside;
- needs no Postgres, network, browser or compose stack;
- uses no `it.skip`, `it.todo`, `skipIf` or `describe.skip`.

None of them can skip under `task test-unit`. The reviewer greps the new files for `skip`/`todo`.

Required before claiming green (CLAUDE.md working conventions, B12/B13):

| Id | Command |
|---|---|
| G1 | `task test-unit` |
| G2 | `cd apps/dashboard-web && pnpm build` (the route-export allowlist, B12) |
| G3 | `cd apps/dashboard-web && pnpm check`: **`0 errors and 0 warnings`**. The merge base itself fails it with `5 errors and 1 warning in 2 files` (T30), so this is reachable only because §11.1 commit 1 clears that debt (review finding `odp-sr3-g3-base-red`). On `main`'s **push** run it is also what lets the dashboard job reach its `pnpm test` step, which follows `pnpm check` and so never ran at the base. It does **not** do so on this run's pull request: see "What PR CI proves" below (review finding `odp-sr4-pr-ci-skips-check-test`) |
| G4 | `cd apps/dashboard-web && pnpm test --sequence.shuffle` (order independence, B13) |
| G5 | with `BASE=$(git merge-base origin/main HEAD)` (after `git fetch origin`): `git diff --stat "$BASE" -- ':!apps/dashboard-web' ':!sentinel-occurrence-detail-panels'` is empty, and so is `git diff "$BASE" -- apps/dashboard-web/src/lib/server/agent-api-spec apps/dashboard-web/src/lib/db/queries/agent-reads.ts apps/dashboard-web/src/routes/api/agent`. Never a pinned commit: if `sentinel-issue-tag-facets` merges first, its `packages/db-migrations`, `apps/processor-go`, `deploy/helm`, `scripts/`, `Taskfile.yml` and `ci.yml` changes are in the base and out of this diff |
| G6 | `grep -rn '{@html' apps/dashboard-web/src/lib/components/issues apps/dashboard-web/src/routes/issues` prints nothing. It is also asserted as test U2 (§12.3). |
| G7 | `node sentinel-occurrence-detail-panels/measure-payload.mjs` from the repository root prints the §3.3 tables (it needs no install), and its `devalue` section once `pnpm install` has run in `apps/dashboard-web`. M1 is the gate that the implemented fixture still matches them. |

`task test-integration` is not needed. Nothing in this feature crosses a process boundary.

**What PR CI proves, and what it does not** (review finding `odp-sr4-pr-ci-skips-check-test`). The dashboard job of
`.github/workflows/ci.yml` puts `if: github.event_name != 'pull_request'` on three of its four steps: the migrations step (`:308`),
`pnpm check` (`:323`) and `pnpm test` (`:330`), line numbers at the merge base `f49721f` (the review cited `:307`, `:318`, `:326`;
the steps are the same). On this run's pull request, CI therefore runs **only `pnpm build`** (G2) for the dashboard. It proves
none of G1, G3 or G4.
- **G1, G3 and G4 are proven by the local gates**, run by the implementing seat on its final commit. Their output goes in the PR
  body verbatim: `task test-unit`'s dashboard `Test Files … Tests …` summary line, `svelte-check`'s `0 errors and 0 warnings`
  line, and the `--sequence.shuffle` run's summary line with its seed. A green PR check is not evidence for any of the three.
- **The first CI run of `pnpm check` and `pnpm test` on this code is the `push` run on `main` after the merge.** That is a
  **post-merge check**: whoever merges watches `main`'s push run (`gh run list --branch main --event push --limit 1`) and records
  its dashboard job's outcome against this run.
- That push run is also the first since `e91b07e` (#52) to run `pnpm check`, `pnpm test`, `schema-drift.test.ts` and the other
  Postgres-backed dashboard suites (`retention.*.integration`) against a freshly migrated database: `pnpm check` has failed at
  every base since #52 (T30), so `pnpm test` never ran after #51 (`1e5c6e2`) or #53 (`f49721f`), which added the v2 columns and
  the enrichment. A red push run in a **Postgres-backed** suite is therefore triaged against #51 and #53 first, and is not by itself
  evidence against this run. Nothing in this feature touches a migration or a real-database read path (§12.1: every test here
  mocks the database).

G5's first command excludes this spec folder, `apps/dashboard-web` and nothing else. The same merge base is the baseline of the
§10.1 golden and of §10.3. `tag-filter.ts` lies inside `apps/dashboard-web`,
so it needs no exclusion.

Every test must be seen to **fail** with its production line removed (CLAUDE.md working convention). The "Mutation" column names
that line.

### 12.2 Fixtures

`occurrence-fixtures.ts` exports literal rows in the shape the load returns: the list row, the light columns and the detail row.
The JSONB bodies are in the stored snake_case (T9-T17):

- **`PRE_V2_OCCURRENCE`**: the 12 pre-v2 keys only, with 3 frames each carrying `in_app` (true, false, false), one with
  `symbolicated: true` and `raw`. **`PRE_V2_OCCURRENCE_NULLS`** is the same with all 17 v2 keys present as `null`.
- **`FLUTTER_OCCURRENCE`** / **`FLUTTER_DETAIL`**: the §3.3 measurement fixture, **copied verbatim** from
  `sentinel-occurrence-detail-panels/payload-fixture.mjs` (same keys, same values, `createdAt` as the ISO string the existing page
  fixtures use). M1 pins its size to §3.3. What the tests rely on:
  - `platform: 'dart'`, `level: 'error'`, its own `message`, `releaseVersion: '2.14.0+2140'`, `dist: '2140'`, `eventId`,
    `serverName: null`, `transaction: 'CheckoutScreen'`, `sdk: {name: 'sentinel.dart.flutter', version: '0.3.0'}`.
  - 20 frames. Frames 0-2 are in app and carry `module` plus source context (5 `pre_context` lines, a `context_line`, 5
    `post_context` lines); frame 0 has `line: 102`. Frames 5, 10 and 15 are in app. The rest are system frames.
  - contexts `device` (with the unlisted field `custom_field` for C1), `os`, `app`, `runtime`, `culture`, `trace`, and an unknown
    `flutter`.
  - 100 crumbs, index 0-99 in stored order. Crumb `i` has `timestamp` = `createdAt − (99 − i) × 1.2 s`, so crumb 99 is at the
    occurrence (`0.0s`), crumb 98 at `-1.2s`, crumb 97 at `-2.4s` and **crumb 96 at `-3.6s`**. Categories cycle `navigation`,
    `http` (with `data.url`), `ui.click`, `console` by `i mod 4`. Levels: `debug` for console, `error` for the 13 http crumbs with
    `i mod 8 = 1`, `info` otherwise.
  - a POST request with 5 headers, one of them `***REDACTED***`, a query string and an object body.
  - a user with `id`, `username`, `email`, `ip_address` and the geo `{city: 'Jakarta', subdivision: 'Jakarta', region:
    'Indonesia', country_code: 'ID'}`.
  - 10 tags.
  - a 3-value chain `SocketException` → `ApiException` → `CheckoutException`, oldest first, with mechanisms `chained/true`,
    `chained/true` and `generic/false`.
- **`OCCURRED_AT`** = `'2026-09-25T02:00:00.000Z'`, exported (it is `payload-fixture.mjs`'s constant, the `FLUTTER` rows'
  `createdAt`), and **`loaded(row)`**, which returns `{ ...row, createdAt: row.createdAt == null ? null : new Date(row.createdAt) }`:
  the shape the real load returns, a `Date` (T29, review finding `odp-sr3-crumb-anchor-date`). Fixtures stay literal ISO strings so
  M1 and `payload-fixture.mjs` are unchanged; a test that must see the production shape wraps its rows in `loaded`.
- **`V2_BARE_OCCURRENCE`**: `level: 'error'`, every other v2 column `null`, detail row all `null`. This is a v2 event whose SDK
  sent nothing else.
- **`inventoryOccurrence(fill)`** returns `{ occurrence, detail }`: a v2 row and its detail row in which **every** §4.3
  inventory field E1-E38 holds `fill(id)`. Exactly these paths (the id is the argument):
  - E1 `message`, E2 `level`, E3 `releaseVersion`, E4 `dist`, E5 `eventId`, E6 `serverName`, E7 `transaction`, E8 `sdk.name` and
    `sdk.version`.
  - `stacktrace`: one frame, `in_app: true`, `line: 10`, with E9 `function` and `file`, E10 `module`, E11 `pre_context: [fill]`,
    `context_line` and `post_context: [fill]`.
  - `tags`: one entry, key E12, value E13.
  - `exception.values`: two values (so the chain renders), each with E14 `type`, E15 `value`, `mechanism: {type: E16, handled:
    true}` and one frame `{function: E9, file: E9, line: 1, in_app: true}`.
  - `breadcrumbs`: one crumb with E17 `timestamp`, E18 `level`, E19 `category`, E20 `type`, E21 `message` and `data: {url: E22,
    note: E22}`.
  - `request`: E23 `method`, E24 `url`, E25 `query_string`, one header E26 → E27, E28 `data` (a string).
  - `user`: E29 `id`, E30 `username`, E31 `email`, E32 `ip_address`, `geo` with E33 in `city`, `subdivision`, `region` and
    `country_code`.
  - `contexts`: `device: {model: E35}` (the headline, and the `Model` row's value, an E37), an unknown context keyed E34 holding
    `{[E36]: E37}`, and `os: E38` (a string context).
- **`HOSTILE_OCCURRENCE`** / **`HOSTILE_DETAIL`** = `inventoryOccurrence(id => (URL_LIKE.has(id) ? URL_PAYLOAD : PAYLOAD) + '#' +
  id)`, with:
  - `PAYLOAD = '<script>alert(1)</script><img src=x onerror=alert(1)>'`;
  - `URL_PAYLOAD = 'javascript:alert(1)//' + PAYLOAD`, a string that is at once a clickable `javascript:` URL and markup;
  - `URL_LIKE` = the fields that look like a URL: E9 (a frame's `file`), E13 (a tag value), E22 (a crumb's `data.url`), E24
    (`request.url`), E27 (a header value) and E37 (a context field value).
  - The `#E…` suffix makes every field's text unique, so U1 can prove each one was rendered, not merely that nothing executed.
    This covers every field the review listed as missing: `dist`, `serverName`, `releaseVersion`, `eventId`, `sdk.version`, the
    user's id, username, IP and geo parts, `request.method`, `query_string`, the crumb type, level and timestamp, the mechanism
    type, the frame function and file, and a context headline.
  - Two more hostile keys are added beside the inventory: a header **named** `PAYLOAD` and a context field **named** `PAYLOAD`
    inside `device`, so an attribute or key sink is exercised too.
- **`LONG_OCCURRENCE`** / **`LONG_DETAIL`** = `inventoryOccurrence(id => id + ':' + 'x'.repeat(3000))`, over both limits,
  except the contract's three examples: E27 is a **1 KiB** header value (`'h'.repeat(1024)`), E28 a **64 KiB** string
  `request.data` (`'d'.repeat(65536)`) and E35 a **64 KiB** `device.model` (`'m'.repeat(65536)`). `LONG_EXPANDERS` exports how
  many `expand-text` buttons the selected card must show **before any click**, one per rendered inventory instance:

  | Rendered by | Instances | Buttons |
  |---|---|---|
  | `OccurrenceHeader` | E1-E8 | 8 |
  | `StackFrames` (top-level) | E9, E10, E11 × 3 | 5 |
  | `TagsTable` | E12 (E13 is clipped, no button) | 1 |
  | `ExceptionChain` | 2 × (E14, E15, E16, one frame line E9) | 8 |
  | `BreadcrumbTimeline` | E17, E18, E19, E20, E21 (E22 is deferred) | 5 |
  | `RequestPanel` | E23-E28 | 6 |
  | `UserPanel` | E29-E33 | 5 |
  | `ContextCards` | E34, E35, E36, E37 × 2 (the `Model` row and the unknown field), E38 | 6 |
  | **total** | | **44** |

- **`PROTO_KEY_ROWS`**: listed rows and a detail row built with `JSON.parse` (an object literal's `__proto__` sets the prototype
  instead of making a key), with a `__proto__` key in `metadata`, `stacktrace[0]` and `sdk` of a listed **pre-v2** row and of a
  listed v2 row, and in the detail row's `tags`, `contexts` (top level and inside `device`), `request.headers`,
  `breadcrumbs[0].data`, `user`, `exception.values[0].mechanism` and `sdk`. Each `__proto__` sits beside an ordinary sibling key.
- **`JS_CHAIN_DETAIL`**: `platform: 'javascript'`, a 4-value chain with no `symbolicated` frame.
- **`V2_JS_OCCURRENCE`** / **`V2_JS_DETAIL`** (review finding `odp-sr2-f6-fixture`; F6's fixture): a v2 JavaScript row whose
  top-level stack mixes symbolicated and raw frames, the shape `sentinel-sourcemaps` writes. `platform: 'javascript'`,
  `level: 'error'`, `message: 'TypeError: Cannot read properties of undefined (reading \'total\')'`, `sdk: {name:
  'sentinel.javascript.browser', version: '1.2.0'}`, the other light columns `null`. `stacktrace`, crashing frame first:
  0. `{function: 'submitOrder', file: 'src/cart/checkout.ts', line: 42, column: 7, in_app: true, symbolicated: true, raw:
     {function: 'n', file: 'https://cdn.example.com/assets/app.3f9a1c.min.js', line: 1, column: 18234}}`;
  1. `{function: 'postJson', file: 'src/cart/api.ts', line: 17, column: 3, in_app: true, symbolicated: true, raw: {function: 't',
     file: 'https://cdn.example.com/assets/app.3f9a1c.min.js', line: 1, column: 9021}}`;
  2. `{function: 'r', file: 'https://cdn.example.com/assets/vendor.min.js', line: 2, column: 1044, in_app: false}`: never
     symbolicated, no `raw`.

  So the badge reads `Partially symbolicated (2 of 3)`. Symbolicated, frame 0 reads `at submitOrder (src/cart/checkout.ts:42:7)`;
  raw, `at n (https://cdn.example.com/assets/app.3f9a1c.min.js:1:18234)` (48 characters of file, under today's 50-character cut);
  frame 2 reads `at r (https://cdn.example.com/assets/vendor.min.js:2:1044)` in both modes. `V2_JS_DETAIL` is the same row with
  all six heavy columns `null`, so a selected `V2_JS_OCCURRENCE` renders no panel and the stack block is the only thing under test.
- **`MALFORMED_FRAMES`** (F7, SSR2): `[null, 7, 'x', [], {line: 3, in_app: true}, {file: null, function: {a: 1}, line: 'x', raw:
  'str', in_app: false}]`, and **`V2_MALFORMED_OCCURRENCE`**, a v2 row (`level: 'error'`) with `stacktrace: MALFORMED_FRAMES`.

**The golden `issue-page-pre-v2-cards.golden.html`** is captured once from the **merge base's** page (review finding
`odp-sr2-g5-base-pinned`), never from a pinned commit. The procedure goes in the test file's header comment, and a reviewer can
re-run it:
1. `BASE=$(git merge-base origin/main HEAD)` (after `git fetch origin`), then `git show "$BASE":'apps/dashboard-web/src/routes/issues/[id]/+page.svelte' > 'src/routes/issues/[id]/Baseline.tmp.svelte'`.
2. With `process.env.TZ = 'UTC'` set first (§8.2's audit: the card header's `formatDate` depends on the time zone), render it with
   `data` = the page fixture holding `PRE_V2_OCCURRENCE` and `PRE_V2_OCCURRENCE_NULLS`, and apply `normaliseCardHtml` to each
   `.occurrence-card`.
3. Write the golden: its first line is `<!-- captured from <the full $BASE sha> -->`, then the concatenated normalised cards,
   captured once with metadata collapsed and once after clicking the first card's metadata toggle and the raw-frames toggle. P4
   drops that first line before comparing.
4. Delete the `.tmp.svelte` file.

If the base moves and changes the pre-v2 card (a rebase onto a `main` that edited it), the golden is re-captured by the same
steps in the same PR, and the diff of the golden is reviewed as the change it is.

**`normaliseCardHtml(card: Element): string`** (in `html-structure.ts`, §11) returns the `outerHTML` of a deep clone of `card`
after exactly these steps, in this order, and nothing else:
1. Remove every comment node.
2. Remove every `class` token matching `^svelte-[a-z0-9]+$`; remove a `class` attribute that this leaves empty.
3. Remove every empty text node (length 0).
4. For every **maximal run of adjacent sibling text nodes that are all whitespace-only** (`/^[ \t\n\f\r]+$/`, the HTML
   whitespace characters; a no-break space is content), **unless** their parent is a `<pre>` or has a `<pre>` ancestor, replace the
   run with one text node holding a single space `' '`. A text node with any other character is never touched, and neither is a
   whitespace node beside it that is not part of a whitespace-only run.

Steps 3-4 are review finding `odp-sr2-p4-whitespace`'s fix; N1-N3 (§12.3) test the normaliser itself.

### 12.3 Criterion → proof

"Mutation" names the production change that must turn the test red. A row with `—` is a regression guard whose failure mode is
covered by a mutated row beside it.

**§4.1 query and load**

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| Q1 | `issue-queries.occurrences.test.ts` (rewritten) | the list projection's keys are exactly the 12 pre-v2 plus `level message serverName transaction dist sdk`, each bound to its own `errorOccurrences` column; none of `eventUser tags contexts request breadcrumbs exception logger errorClass source traceFlags receivedAt` is selected | add `breadcrumbs` to the list projection → red |
| Q2 | same | `getOccurrenceDetail('i','o')`: the projection keys are exactly the 18 list keys plus `user tags contexts request breadcrumbs exception`, with `user` bound to `eventUser`; `where` receives `and(eq(id,'o'), eq(issueId,'i'))` (asserted on the argument, B10); `limit(1)`; `[]` → `null` | drop the `issueId` predicate → red |
| Q3 | same | the table declares all 17 v2 columns (kept from V7e) | — |
| Q4 | same | `occurrenceExistsOutsideIssue('i','o')`: selects `{ id }` only; `where` receives `and(eq(id,'o'), ne(issueId,'i'))`; `limit(1)`; a row → `true`, `[]` → `false` | `eq` for `ne` → red |
| L1 | `page.server.occurrence-detail.test.ts` | S1: no param → `selectedOccurrenceId` = the first listed id; `getOccurrenceDetail` called with `(issueId, thatId)`; its row returned as `occurrenceDetail` | select `occurrences.at(-1)` → red |
| L2 | same | S1 with `?occurrence=` (empty) behaves as absent | — |
| L3 | same | S2: `?occurrence=<second listed id>` selects it; the same id upper-cased also selects it | ignore the param → red |
| L4 | same | S3b: `?occurrence=<a UUID the scoped read does not find and the probe does>` (another issue's, and another project's) throws `{status: 404, body.message: 'Occurrence not found'}`; the probe ran only after the scoped read returned `null` | treat the probe's `true` as unknown (default) → red |
| L5 | same | the environment-scoped caller of `page.server.test.ts`'s existing case gets `404 Issue not found` **with** an `?occurrence=` param, and neither occurrence read runs; a project-access `403`/`404` from `requireProjectAccess` propagates unchanged, and no occurrence read runs | move the occurrence read above the scope check → red |
| L6 | same | a selected **listed pre-v2** row (`level: null`): `occurrenceDetail` is `null` and `getOccurrenceDetail` is not called | query unconditionally → red |
| L7 | same | `getOccurrenceDetail` resolving `null` for a listed v2 selection (row vanished) → `occurrenceDetail: null`, no throw | — |
| L8 | same | S3a, **aged out**: `?occurrence=<UUID of this issue, not listed>`, the scoped read returns its row → no throw, `selectedOccurrenceId` is that id, `occurrenceDetail` is that row, the probe is **not** called; also with that row's `level: null` (an aged-out pre-v2 row) | 404 every unlisted id (round 0's S3) → red |
| L9 | same | S4, **garbage**: `not-a-uuid`, `'; drop table x;--`, `{0b7e3f2a-5c1d-4e8f-9a6b-3c2d1e0f9a8b}` and the listed id with its hyphens removed each render the default (`selectedOccurrenceId` = the first listed id) with no throw, and neither `getOccurrenceDetail` nor the probe is ever called with the raw value | pass the raw value to `getOccurrenceDetail` without the `UUID_RE` check → red |
| L10 | same | S3c, **unknown**: a well-formed UUID both reads miss → the default selection, no throw | throw 404 when the scoped read misses → red |
| L11 | same | `PROTO_KEY_ROWS`: the load result passes `devalue.stringify(result)` **and** `devalue.uneval(result)` without throwing; no `__proto__` own key remains at any depth of any returned row (walked with `Object.hasOwn`), including the pre-v2 row's `metadata` (the pre-existing exposure); every sibling key is kept; `createdAt` is still a `Date` | return rows without `cleanOccurrenceRow` → red (`Cannot stringify objects with __proto__ keys`) |
| L12 | same | **no `url` at all** (review finding `odp-sr3-facets-loader-url`): the load called with `{ params, locals }` only, the event shape of today's `loadAs` and of tag facets' `page.tag-facets.test.ts`, does not throw and returns the S1 default (`selectedOccurrenceId` = the first listed id, `getOccurrenceDetail` called with it); likewise with `url: undefined` | read `url.searchParams.get('occurrence')` without the optional chain → red (`TypeError: Cannot read properties of undefined (reading 'searchParams')`) |
| J1 | `dw/src/lib/server/json-safe.test.ts` | `dropProtoKeys(JSON.parse('{"__proto__":"t","a":{"__proto__":{"x":1},"b":[{"__proto__":1,"c":2}]}}'))` deep-equals `{a:{b:[{c:2}]}}`, and `Object.getPrototypeOf` of every object in it is `Object.prototype` (the key was not assigned); a value with no `__proto__` key comes back deep-equal; a `Date` is returned by identity | stop recursing into arrays → red |

**TASK-04: header and frames**

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| H1 | `OccurrenceHeader.test.ts` | `FLUTTER_OCCURRENCE`: `occurrence-message` is the occurrence's own message (≠ `issueMessage`); facts in order Level/Release/Dist/Event ID/Transaction/SDK with those values; no `Server` row (null); `SDK` reads `sentinel.dart.flutter 0.3.0` | render `issueMessage` always → red |
| H2 | same | `message: null` on a v2 row → `issueMessage` shown | — |
| H3 | same | the level badge has class `tone-critical` for `error` and `fatal`, `tone-warning` for `warning`, `tone-info`, `tone-debug`, and `tone-neutral` for an unknown value | — |
| F1 | `StackFrames.test.ts` | `FLUTTER` frames: each `frame-line` text equals `formatStackFrame(frame)`; in-app frames carry `data-in-app="true"` and an `in app` mark; `frame-module` is shown for frames with `module` | drop the in-app mark → red |
| F2 | same | source context: frame 0 (`line: 102`, 5 pre, 5 post) renders 11 lines numbered 97-107, with the `aria-current="true"` line's text equal to `context_line` and numbered 102; a symbolicated frame with `raw.line: 1` and `line: 88` numbers from 1; a frame with `line: 2` and 5 pre lines has no gutter; `pre_context` without `context_line` renders no context | number from `frame.line` → red (symbolicated case) |
| F3 | `occurrence-display.test.ts` | `frameVisibility` table: `[S,S,A,S,S,S,A,S]` (S = system, A = app, index 0 crashing) → visible indices `0,2,3,6,7`; hidden runs `{1}` and `{4,5}`; all-S → all visible; all-A → all visible | invert `i-1` to `i+1` → red |
| F4 | `StackFrames.test.ts` | rendering `[S,S,A,S,S,S,A,S]`: two `show-frames` buttons, `Show 1 more frame` and `Show 2 more frames`; clicking the second reveals frames 4-5 only and relabels it `Hide 2 more frames` | — |
| F5 | same | `showRaw` swaps each frame line to `formatStackFrame(frame.raw)` and keeps visibility | — |
| F6 | `page.occurrence-panels.test.ts` | the page with `V2_JS_OCCURRENCE` listed and selected and `V2_JS_DETAIL` (§12.2): the v2 card's `Stack Trace` block renders through `frames` (not the `<pre>`), keeps `symbolication-badge` reading `Partially symbolicated (2 of 3)` and `raw-frames-toggle`; before the click, frame 0's `frame-line` is `at submitOrder (src/cart/checkout.ts:42:7)`; after it, frames 0 and 1 read their raw forms (`at n (https://cdn.example.com/assets/app.3f9a1c.min.js:1:18234)`, …) and frame 2 is unchanged; the `in app` marks are the same in both modes; a second click restores the symbolicated lines (`page.sourcemaps.test.ts` semantics on the new renderer) | pass `showRaw={false}` to the card's `StackFrames` → red |
| F7 | `StackFrames.test.ts` | frame shape (§5.2): `MALFORMED_FRAMES` mounts without throwing with `showRaw` `false` and `true`; exactly 2 `frame` items render (the four non-objects are skipped); their lines read `at <unknown> (<unknown>:3)` and `at {"a":1} (<unknown>:0)` in both modes (the string `raw` is ignored); `frameVisibility` saw 2 frames (one app, one system, so both visible); SSR2 repeats it under `svelte/server` | call `formatStackFrame(displayedFrame(frame, showRaw))` on the stored element → red (`TypeError: Cannot read properties of null`) |

**TASK-03: user, request and tags**

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| US1 | `UserPanel.test.ts` | `FLUTTER_DETAIL.user` renders ID/Username/Email/IP Address/Geo, with Geo `Jakarta, Jakarta, Indonesia (ID)` for the fixture's `{city: 'Jakarta', subdivision: 'Jakarta', region: 'Indonesia', country_code: 'ID'}` (the enrichment's shape, T14); `user: null`, `{}` and `{geo: {}}` each render nothing (`queryByTestId('user-panel')` is null, and so is any `h4` `User`) | render the section unconditionally → red |
| R1 | `RequestPanel.test.ts` | the fixture renders `POST`, the URL as text, the query row, 5 header rows sorted case-insensitively with `***REDACTED***` shown verbatim, and the body as pretty JSON; `container.querySelectorAll('a').length === 0` | — |
| R2 | same | `request: null`, `{}` and `{env: {A: 'b'}}` each render nothing | — |
| R3 | same | a string body is shown verbatim, not JSON-quoted; `data: 0` and `data: false` are shown (`0`, `false`), and `data: null` is not | — |
| TG1 | `TagsTable.test.ts` | rows sorted by key; each `tag-link`'s `getAttribute('href')` equals the §6.3 pinned table exactly, row by row, including `k` / `it's (x)!~` → `/search?tag=k%3Ait's%2520(x)!~`; `tagSearchHref` called directly returns the same seven strings; `TagsTable` imports `tagSearchHref` from `$lib/tag-filter` (the module is read with `fs` and the import line asserted) | encode the joined string once (the pre-decision-6 form) → red on every row but `os.name`; build the href with `URLSearchParams` (contract decision 7) → red on the `it's (x)!~` row (`%27`, `%28`, `%29`, `%21`, `%7E`) |
| TG2 | same | `tags: null` and `{}` render nothing | — |
| TG3 | `TagsTable.test.ts` | the round trip of contract decision 6: for key `a:b` and value `c:d e/%`, `raw = new URL(href, 'http://x').searchParams.get('tag')`; `raw` has **exactly one** literal `:`; splitting there and applying `decodeURIComponent` to each part gives exactly `['a:b', 'c:d e/%']` | build the href as `encodeURIComponent(key + ':' + value)` → red (three literal `:` after one decode) |

**TASK-01: context cards**

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| C1 | `ContextCards.test.ts` | `FLUTTER_DETAIL.contexts`: cards in order `device, os, app, runtime, culture, trace, flutter` (`data-context-key`); titles `Device`, `Operating System`, …, `flutter`; headlines `Pixel 7`, `Android 14`, `Daya 2.14.0`, `Dart 3.3.0 (stable)`, `id_ID`; the device card's first rows are `Name`, `Family`, `Model` in §7.2 order; the fixture's device field `custom_field`, which §7.2 does not list, renders after every labelled row under its raw key | reorder the well-known list → red |
| C2 | same | the unknown `flutter` card renders its keys generically (`has_render_view` → `true`, `renderer` → `Impeller`) with keys as labels | — |
| C3 | same | `os` and `client_os` both present → two cards titled `Operating System` and `Client Operating System` | — |
| C4 | same | a context that is the string `***REDACTED***` renders a card whose body text is exactly `***REDACTED***`; `type` equal to the key is not rendered, and `type: 'os'` under `client_os` is | — |
| C5 | same | `contexts: null`, `{}`, and `{device: {}}` / `{os: {type: 'os'}}` each render nothing: no card, no `Contexts` heading | — |
| C6 | same | **prototype keys** (§4.2 rule 5): `contexts = {device: {model: 'P7', constructor: 'Pixel'}, constructor: {name: 'c'}, toString: {v: 1}, hasOwnProperty: 'x'}`, plus `JSON.parse('{"__proto__": {"a": 1}}')` merged in with `Object.defineProperty`: no throw; cards in order `device`, `__proto__`, `constructor`, `hasOwnProperty`, `toString` (well-known first, then code-unit order); each unknown card's title is its raw key; `constructor`'s body has the row `name` → `c`; `hasOwnProperty` is a scalar card `x`; the device card has a `constructor` row reading `Pixel` after its labelled `Model` row; the container's `textContent` contains neither `function` nor `[native code]` | replace the titles `Map` with a plain object read as `TITLES[key] ?? key` → red (the `constructor` title renders `function Object() { [native code] }`) |

**TASK-02: breadcrumbs**

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| B1 | `BreadcrumbTimeline.test.ts` | 100 `crumb` items in stored order (first and last messages checked); **zero** `crumb-data` elements before any expand | stringify data eagerly (render the block hidden) → red |
| B2 | same | the level filter set to `error` leaves exactly the fixture's 13 error crumbs, and `crumb-count` reads `Showing 13 of 100`; the category filter `http` AND level `error` narrows to their intersection (the same 13); back to `All` restores 100. **Level tone** (contract §4.5 "coloured with the existing `--severity-*` variables", review finding `odp-sr3-crumb-tone-untested`): with the filters at "all", crumb 1's `.crumb-level` (`http`, `error`) has class `tone-critical`, crumb 3's (`console`, `debug`) has `tone-debug`, and crumb 0's (`navigation`, `info`) has `tone-info`; mounted with `[{category: 'x', message: 'no level', timestamp: OCCURRED_AT}]`, that crumb renders **no** `.crumb-level` element | drop `{levelTone(level)}` from the crumb-level span's `class` → red (no `tone-*` class on any crumb) |
| B3 | same | clicking a crumb's `crumb-data-toggle` renders its `crumb-data` with the pretty JSON, and the button reads `Hide data` | — |
| B4 | same | `crumb-time` of **crumb 96**, the fixture's crumb 3.6 s before the occurrence (§12.2), reads `-3.6s`, with `title` equal to its stored timestamp; crumb 99 reads `0.0s`; a crumb with `timestamp: 'yesterday-ish'` reads exactly `yesterday-ish` (no `NaN`, no `Invalid Date` anywhere in the list's text); an absent timestamp reads `—` | anchor on `Date.now()` instead of `occurredAt` → red |
| B4b | same | **the production shape** (review finding `odp-sr3-crumb-anchor-date`): the `FLUTTER` crumbs with `occurredAt = new Date('2026-09-25T02:00:00.000Z')` (`new Date(OCCURRED_AT)`): crumb 96's `crumb-time` reads `-3.6s` with `title` its stored timestamp, and crumb 99 reads `0.0s`, exactly as B4 with the string; with `occurredAt = new Date('nope')` (an Invalid Date) and with `null`, crumb 96 reads its stored timestamp string, with no `title` and no `NaN` or `Invalid Date` anywhere in the list's text | read the anchor with `parseCrumbTime(occurredAt)` only → red (crumb 96 shows the absolute timestamp, not `-3.6s`) |
| B5 | `occurrence-display.test.ts` | `formatRelative` pinned values (§8.2); `anchorMs(new Date(OCCURRED_AT))` and `anchorMs(OCCURRED_AT)` both equal `Date.parse(OCCURRED_AT)`; `anchorMs` of `new Date('nope')`, `null`, `undefined`, `0` and `'nope'` is `null`; `parseCrumbTime` of `…T02:00:00.123456789Z` = `…T02:00:00.123Z`'s epoch, and of `2026-13-45T99:00:00Z` = `null` | — |
| B6 | `BreadcrumbTimeline.test.ts` | 150 crumbs → 100 rendered (the last 100) plus the guard line | render all 150 → red |
| B7 | `BreadcrumbTimeline.test.ts` | `breadcrumbs: null`, `[]` and `[1, 'x']` render nothing | render the section for any array → red on `[1, 'x']` |

**TASK-05: chain**

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| X1 | `ExceptionChain.test.ts` | `FLUTTER_DETAIL.exception` (3 values): 3 `chain-value` articles whose headings read `CheckoutException: …`, `ApiException: …`, `SocketException: …`, in that order (outermost first); each has its own `frames` list; mechanisms read `mechanism: generic · handled: false`, then `mechanism: chained · handled: true` twice | drop the reverse → red |
| X2 | same | a 1-value and a 0-value `exception` render nothing | render for `length ≥ 1` → red |
| X3 | same | `JS_CHAIN_DETAIL` with `platform: 'javascript'`: every stack carries `chain-unsymbolicated`; the same chain with `platform: 'dart'` carries none; the 4th displayed value is collapsed until `chain-expand` is clicked | — |
| X4 | `page.occurrence-panels.test.ts` | on the page, the selected Flutter card shows the top-level `Stack Trace` block **and** the chain section below it | — |

**§4.3 truncation, in the panels, before any expand** (review finding `odp-sr-truncation-untested-in-panels`)

Every row here mounts its component or page and asserts **before clicking anything**. A text node's length is counted in code
points.

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| TR1 | `page.occurrence-truncation.test.ts` | the page with `LONG_OCCURRENCE` listed and selected and `LONG_DETAIL`: the selected card holds exactly `LONG_EXPANDERS` (44) `expand-text` buttons, and each component's own count matches the §12.2 table (so a miss names its panel); no text node in the card is longer than `BLOCK_LIMIT + 1` code points; every `<option>` label of the two crumb filters is at most `INLINE_LIMIT + 1`; the tag link's text is 256 code points plus `…` | render any one inventory field as a bare `{value}` → red (the count drops by one; for a `BLOCK_LIMIT` field, the text-node bound fails too) |
| TR2 | `RequestPanel.test.ts` | `LONG_DETAIL.request`: the 1 KiB header's value cell shows 256 code points, `…` and `Show all (1024 characters)`; the header-name cell shows 256 + `…`; `request-body` shows 2048 code points, `…` and `Show all (65536 characters)`; `request.url` and `query_string` each show 256 + `…` | `<td>{value}</td>` for header values → red; the body as bare `{body}` → red |
| TR3 | `ContextCards.test.ts` | `LONG_DETAIL.contexts`: the device card's `context-headline` shows 256 code points, `…` and `Show all (65536 characters)`; the unknown card's title, its field's `dt` and `dd`, and the `Model` row each show 256 + `…`; the `os` scalar shows 2048 + `…` | a bare `{headline}` → red |
| TR4 | `StackFrames.test.ts` | a frame whose `function` and `file` are 3 000 code points: its `frame-line` shows 2048 + `…` and a button; a 3 000-code-point `context_line` likewise; a 3 000-code-point `module` shows 256 + `…` | a bare `{formatStackFrame(…)}` → red |
| TR5 | `BreadcrumbTimeline.test.ts` | the `LONG_DETAIL` crumb: its level, category, type, message and unparseable timestamp each show 256 + `…` and a button; after its data toggle, `crumb-data` shows 2048 + `…` | a bare `{category}` → red |
| TR6 | `ExceptionChain.test.ts` | `LONG_DETAIL.exception`: each value's `type`, `value` and mechanism type show 256 + `…`, and each chain frame line 2048 + `…` | a bare `{type}` in the heading → red |
| TR7 | `OccurrenceHeader.test.ts` | `LONG_OCCURRENCE`: `occurrence-message` shows 2048 + `…`; each of Level, Release, Dist, Event ID, Server, Transaction and SDK shows 256 + `…` | a bare `{dist}` → red |
| TR8 | `UserPanel.test.ts`, `TagsTable.test.ts` | `LONG_DETAIL.user`: each of the five rows shows 256 + `…`; `LONG_DETAIL.tags`: the key cell shows 256 + `…` and the link text is `clip(value, 256)` | a bare `{user.email}` → red; the link text unclipped → red |

**§4.6 markup nesting under SSR, and the normaliser** (review findings `odp-sr2-pre-in-p-ssr`, `odp-sr2-p4-whitespace`)

`tagSequence(html)` (in `html-structure.ts`) is the list of start and end tag names in `html`, lower-cased, in document order:
comments are skipped, void elements (`area base br col embed hr img input link meta source track wbr`) are skipped, and a start
tag is matched with a pattern that skips quoted attribute values, so an attribute holding `<pre>` is not a tag. `reparse(html)` is
`new JSDOM('<!doctype html><html><head></head><body>' + html + '</body></html>').window.document.body.innerHTML`: jsdom is already a
devDependency of `dw` (`package.json:21`) and parses with parse5, the HTML standard's tree builder. jsdom ships no type
declarations, and `html-structure.ts` sits under `src/`, which `pnpm check` type-checks under `strict: true` (and so are the
`.test.ts` files, so moving the import there would not help). The import is typed by the `@types/jsdom` `21.1.7` devDependency
(§2, §11, T31); **G3 is the proof**: without it, `pnpm check` reports TS7016 `Could not find a declaration file for module
'jsdom'` for this file (verified on `f49721f`), and with it the file checks clean. `html-structure.ts` never uses `any` to route
around a missing type. **"The parse changed no element
nesting" means `tagSequence(reparse(html))` equals `tagSequence(html)`.** The strings themselves are not compared, because
re-serialising legitimately re-escapes text (`>` becomes `&gt;`); only the tags say where an element sits. Svelte escapes `<` in
text and in attribute values (`&lt;`), so an event string cannot forge a tag in either sequence (verified with the hostile payload,
svelte 5.55.5).

The SSR rows run in `page.occurrence-ssr.test.ts`, whose first line is `// @vitest-environment node`. In a `node`-environment file,
vitest compiles `.svelte` for the server, and `render` from `svelte/server` works under `dw/vite.config.js` as it is, including its
test-mode `resolve.conditions: ['browser']`: verified on `f49721f`, where `render(Page, { props: { data } })` returns the whole
issue page (5 289 bytes, `.occurrence-card` included). It is matched by `src/**/*.test.ts`, so `task test-unit` runs it (T23), and
it needs no browser. Each SSR row also spies on `console.warn` and `console.error` and asserts that no call mentions
`node_invalid_placement`, Svelte's own dev-time warning for the same defect.

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| SSR1 | `page.occurrence-ssr.test.ts` | each panel server-rendered alone with `render()` from `svelte/server`: `OccurrenceHeader`, `StackFrames`, `TagsTable`, `ExceptionChain`, `BreadcrumbTimeline`, `RequestPanel`, `UserPanel`, `ContextCards`, and `ExpandableText` in all four forms (inline and `block`, under and over its limit), each with the `HOSTILE_*`, `FLUTTER_*` and `LONG_*` fixtures (§12.2): no throw; `tagSequence(reparse(body))` equals `tagSequence(body)` | make `OccurrenceHeader`'s message container a `<p>` again (round 1's markup, `<p class="occurrence-message">` around the block `ExpandableText`) → red: the reparse closes the `<p>` before the `<pre>` and opens an empty `<p>` after it; likewise `context-scalar` as a `<p>`, and a `<tr>` without its `<tbody>` |
| SSR2 | same | the whole occurrence card, server-rendered as the page: `render(Page, { props: { data } })` for (a) `HOSTILE_OCCURRENCE` listed and selected with `HOSTILE_DETAIL`, (b) `FLUTTER_OCCURRENCE` likewise, (c) `FLUTTER_DETAIL` as an S3a selection outside the list (the `selected-outside-window` block, §3.2), (d) `V2_MALFORMED_OCCURRENCE` selected: no throw; `tagSequence(reparse(body))` equals `tagSequence(body)`; in (d) the body holds exactly 2 `data-testid="frame"` items and the text `at <unknown> (<unknown>:3)` | as SSR1, on the page; and for (d), F7's mutation → red (the render throws) |
| SSR3 | same | the pre-v2 page (`PRE_V2_OCCURRENCE`, `PRE_V2_OCCURRENCE_NULLS`) server-renders with an unchanged nesting too, so the check covers every card shape the page has | — |
| N1 | `html-structure.test.ts` | `normaliseCardHtml` of `<div><div>h</div> <!----> <div>t</div></div>` equals that of `<div><div>h</div> <div>t</div></div>` (the false `{#if}` case, verified to arise on a client mount, svelte 5.55.5); `class="h svelte-oaiw42"` normalises to `class="h"`, and `class="svelte-oaiw42"` to no `class` attribute | drop step 4 (collapsing) → red |
| N2 | same | inside a `<pre>`, whitespace is kept: `<pre>a <!----> b</pre>` does **not** normalise equal to `<pre>a b</pre>`, and neither does the same inside `<pre><code>` | collapse runs inside `<pre>` too → red |
| N3 | same | content is never normalised away: `<div>h</div> x <div>t</div>` differs from `<div>h</div> <div>t</div>`; `<span>a</span><span>b</span>` differs from `<span>a</span> <span>b</span>` (a separator is content); a no-break space is kept; a class `badge` is kept | remove whitespace-only text nodes instead of collapsing them → red (the separator case) |
| N4 | same | the nesting check itself: `tagSequence(reparse('<p class="occurrence-message"><pre>x</pre></p>'))` differs from its `tagSequence` (a `<pre>` inside a `<p>` goes red); `'<div class="occurrence-message"><pre>x</pre></div>'` does not differ; neither does `'<p title="<pre>x</pre>">t</p>'` (a tag inside a quoted attribute is not a tag); `'<table><tr><td>x</td></tr></table>'` differs (the parser inserts `<tbody>`) | make `reparse` return its input unparsed → red (the `<p><pre>` and `<table><tr>` cases stop differing) |

**§4.7 untrusted content**

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| U1 | `occurrence-untrusted.test.ts` | the page with `HOSTILE_OCCURRENCE` listed and selected and `HOSTILE_DETAIL`, every `expand-text`, `show-frames`, `chain-expand` and `crumb-data-toggle` clicked: (1) `document.querySelectorAll('script, img, iframe, object, embed').length` equals the count **before** mounting; (2) no element has an attribute whose name starts with `on`; (3) no `a[href]` starts with `javascript:` (case-insensitive, trimmed), and every `a` in the card is a `tag-link` or `select-occurrence`; (4) no element has a `src`, `style`, `srcdoc`, `formaction` or `xlink:href` attribute containing `alert`; (5) every attribute value containing `alert` is a `data-context-key`, an `<option value>`, or the `href` of a `tag-link` beginning `/search?tag=`; (6) **for every inventory id E1-E38**, the card's `textContent` contains that field's literal fill (`PAYLOAD#E7`, `javascript:alert(1)//…#E24`, …), which proves each field is rendered, and rendered as text | swap one `{value}` for `{@html value}` → red (1, 6); render `request.url` as `<a href={url}>` → red (3); drop a field from a panel → red (6) |
| U2 | same | static: no `{@html` in any `.svelte` file under `dw/src/lib/components/issues/` or in `dw/src/routes/issues/[id]/+page.svelte` (read with `fs`). U1 is the proof; U2 is a cheap second fence, not the one relied on | — |
| U3 | `ExpandableText.test.ts` | a 1 KiB header value shows 256 code points plus `…` and `Show all (1024 characters)`, and after the click the full value; the 64 KiB body shows 2048 plus `…`, and after the click all 65 536; a string of 300 emoji (surrogate pairs) truncates at 256 code points without a lone surrogate | count UTF-16 units → red (emoji case) |
| U4 | `occurrence-display.test.ts`, `BreadcrumbTimeline.test.ts` | **prototype keys** (§4.2 rule 5): `levelTone` of `constructor`, `toString`, `hasOwnProperty`, `valueOf` and `__proto__` is `tone-neutral`; `contextCards` of C6's contexts returns the same cards C6 renders, and never a card whose title or field list is a function; crumbs with category `constructor` and level `toString` render, both appear as filter options, and selecting each narrows the list to exactly those crumbs, with `crumb-count` correct | read the level map as a plain object `TONES[level] ?? 'tone-neutral'` → red (`constructor` yields a function, not a class name) |

**Compatibility, the fixture and the hypothesis**

| Id | Test (file) | Asserts | Mutation |
|---|---|---|---|
| P1 | `page.occurrence-panels.test.ts` | the contract §1 hypothesis, **in the shape the real load returns**: `loaded(FLUTTER_OCCURRENCE)` selected with `loaded(FLUTTER_DETAIL)` (`createdAt` a `Date`, §12.2, T29), and `occurrence-message`, `occurrence-facts`, `tags-table`, `frames`, the chain, `breadcrumbs`, `request-panel`, `user-panel` and `context-card` all present, in §4.5's document order; the card header's `.timestamp` text equals `new Date(OCCURRED_AT).toLocaleString('en-US', {year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit'})` (the page-local `formatDate`'s options, `+page.svelte:25-34`, computed in the same process so no `TZ` pin is needed) and is neither `Unknown` nor `Invalid Date`; inside `breadcrumbs`, crumb 96's `crumb-time` reads `-3.6s` and crumb 99's `0.0s` (review finding `odp-sr3-crumb-anchor-date`) | anchor on `parseCrumbTime(occurredAt)` only → red (crumb 96 reads its absolute timestamp); pass `occurredAt={String(createdAt)}` from the page → red |
| P2 | same | a second, non-selected v2 card has `select-occurrence` with `href="?occurrence=<its id>"`, and no panel; the selected card has `selected-occurrence` and `aria-current="true"` | — |
| P3 | same | `V2_BARE_OCCURRENCE` selected: message and facts render; no panel section and no panel `h4` render | render headings unconditionally → red |
| P4 | `page.occurrence-compat.test.ts` (first statement `process.env.TZ = 'UTC'`; the test first asserts `new Date(0).getTimezoneOffset() === 0`, §8.2 audit) | `normaliseCardHtml` (§12.2) of each `.occurrence-card` for `PRE_V2_OCCURRENCE` and `PRE_V2_OCCURRENCE_NULLS`, with each selected in turn, equals the golden captured from the merge base, both before and after the metadata and raw toggles are clicked | add the facts block without the v2 gate → red |
| P5 | existing `page.svelte.test.ts`, `page.sourcemaps.test.ts`, `page.affected-users.test.ts`, `page.issue-trend.test.ts`, `page.releases-snooze.test.ts`, `page.server.test.ts` (and `page.tag-facets.test.ts` when tag facets is in the merge base) | pass unedited, `page.server.test.ts`'s `loadAs` included: it passes no `url` (§3.2, §10.7) | — |
| P6 | `openapi-drift.test.ts`, `contract.test.ts` | pass unedited (G5 shows the files unchanged) | — |
| P7 | `page.occurrence-panels.test.ts` | S3a on the page: `occurrences` = two listed v2 rows, `occurrenceDetail` = a third, unlisted `FLUTTER`-shaped row: `selected-outside-window` renders after the section heading and before the first date group, with the text `This event is older than the 2 most recent shown below.`; its card has `selected-occurrence`, `aria-current="true"` and the panels; no listed card has `aria-current` | never render the block → red |
| P8 | same | a listed selection (`occurrenceDetail.id` among `occurrences`) renders **no** `selected-outside-window`, and neither does a page with `occurrenceDetail: null` | render the block whenever `occurrenceDetail` is set → red |
| G3a | `dw/src/lib/db/queries/organizations.members.test.ts` (§11.1 commit 1) | T30's runtime half: with `$lib/server/db` mocked by a chain that records its calls (`select` → `from` → `innerJoin` → `where` → resolves `[{id: 'm1', role: 'admin', user: {id: 'u1', name: null, email: 'a@x'}}]`) and `$lib/db/schema` mocked with `organizationMembers` **and** `users` tables, `listOrganizationMembers('org-1')` resolves `[{id: 'm1', userId: 'u1', user: {name: 'a@x', email: 'a@x'}, role: 'admin'}]`, and `innerJoin`'s first argument is the mocked `users` table (asserted on the call, B10). G3 is its compile-time half | remove the `users` import → red (`ReferenceError: users is not defined`), and G3 red (TS2304 at `:627-633`) |
| M1 | `occurrence-fixtures.test.ts` | the fixture still matches §3.3: `Buffer.byteLength(JSON.stringify(…))` of `FLUTTER_OCCURRENCE`'s 12 pre-v2 keys, its six light keys and `FLUTTER_DETAIL`'s six heavy keys are each within ±10 % of 4 574, 212 and 27 504 | drop the breadcrumbs from `FLUTTER_DETAIL` → red |

### 12.4 The measurement script

§3.3's numbers come from two files committed **with this spec**, in this folder (review finding
`odp-sr-measurement-not-reproducible`):
- `payload-fixture.mjs`: `FLUTTER_OCCURRENCE` and `FLUTTER_DETAIL`, plain ESM with no dependency. It is the source TASK-04 copies
  into `occurrence-fixtures.ts`.
- `measure-payload.mjs`: prints the fixture table, the ingest-bounds maxima of every heavy column (built from the
  `bounds.go:20-92` constants), the worst-case light payload of 100 rows in three encodings, and, when `devalue` can be found, the
  fixture sizes through `devalue.stringify` plus a live check that a `__proto__` key makes it throw (T25).

**Where the script finds `devalue`** (review finding `odp-sr2-devalue-path-note`). pnpm links only direct dependencies into
`apps/dashboard-web/node_modules`, and at the merge base `devalue` is not one: it is `@sveltejs/kit`'s. The script tries, in order:
1. `DEVALUE_PATH`, if set (an explicit override, never required);
2. `devalue` resolved from `apps/dashboard-web/package.json`: the devDependency this feature adds (§2, §11), pinned to `5.8.0`, so
   after the implementation PR and `pnpm install` the script and L11 find the same copy with no environment variable;
3. `devalue` resolved from `@sveltejs/kit`'s own location, the exact copy SvelteKit serialises with. This makes section 3 work on
   any installed checkout **before** the devDependency lands, including the merge base. Verified: on an installed `f49721f`
   checkout, where `apps/dashboard-web/node_modules/devalue` does not exist, step 3 resolves
   `node_modules/.pnpm/devalue@5.8.0/…` and prints the figures §3.3 quotes.

Re-measure from the repository root:

```bash
node sentinel-occurrence-detail-panels/measure-payload.mjs            # sections 1-2 need nothing installed
(cd apps/dashboard-web && pnpm install) && node sentinel-occurrence-detail-panels/measure-payload.mjs   # section 3 too
```

The implemented fixture is held to these numbers by M1, a gate under `task test-unit`, not by a note in the PR description. The
decision does not rest on the fixture: it rests on the ingest-bounds maxima, which the same script prints.

---

## 13. Decisions this spec made, and what each was derived from

### 13.1 Decisions

| # | Decision | Derived from |
|---|---|---|
| 1 | Heavy columns (`user tags contexts request breadcrumbs exception`) only for the selected occurrence, through the page load. Light header columns for all 100; the worst-case light payload (≤ 6.4 MB for 100 rows) does not change this. (§3.3) | the committed §3.3 measurement; contract §4.1 (same checks, 404); T6, T27 |
| 2 | Selection by `?occurrence=`, defaulting to the newest listed occurrence. An unlisted occurrence of this issue renders selected; another issue's answers 404; a malformed or unknown value is ignored. (§3.2) | contract §4.1 "404, never 403, outside scope"; T1, T28; coordinator ruling on `odp-sr-occurrence-param-404-regression` |
| 3 | The v2 gate is `level != null`. (§4.1) | T8 (including `InsertOccurrence`'s non-use), T10; the compatibility section |
| 4 | New frame renderer on v2 rows only; `in_app` unrendered on pre-v2. (§5.2, §1.3) | T10 against the compatibility section |
| 5 | Sentry's `frameIsVisible`, translated to crashing-first order; collapse only with mixed in-app. (§5.2) | `crashContent/stackTrace/content.tsx`; `interfaces/exception.tsx`; `sentry/interfaces/stacktrace.py`; T11 |
| 6 | Source context numbered from `raw.line ?? line`. (§5.2) | `event.go:194-201`, `:510-529` |
| 7 | Level tones mapped onto existing variables; no new variable. (§4.4) | T20; `DESIGN.md:58-60` (hue 75) |
| 8 | `INLINE_LIMIT` 256, `BLOCK_LIMIT` 2048, in code points; **every** event string through `ExpandableText`, or `clip` where a button cannot go. (§4.3) | contract §4.7 examples; T16, T27; review finding `odp-sr-truncation-untested-in-panels` |
| 9 | Tag href: contract decision 6's `encodeURIComponent` form, the one normative href (decision 7), built only by the shared `tagSearchHref` in `dw/src/lib/tag-filter.ts`, never with `URLSearchParams`. (§6.3) | contract decisions 6 (`aab1cc5`) and 7 (`bc275e6`); tag-facets decision 10; tag-facets spec §6.1, §11.3 @ `153f6db` |
| 10 | Context order `device os client_os app runtime browser culture trace`, then others sorted; the field lists of §7.2. | contract §4.4; Relay `protocol/contexts/*.rs`; `contexts.mdx`; `bounds.go:122-125` |
| 11 | Breadcrumbs: all rendered, `data` deferred until expanded; guard at 100. (§8.3) | contract §4.5 (the spec sets the bound); T16 |
| 12 | Relative time anchored on `createdAt`, with the §8.2 format; raw string for unparseable. The anchor is read by `anchorMs`, which accepts the `Date` the real load returns as well as a test's ISO string. | contract §4.5; `processor_service.go:331`; T24; T29; coordinator ruling on `odp-sr3-crumb-anchor-date` |
| 13 | Chain: outermost first (reverse), added below the kept top-level stack; Sentry's collapse beyond 3; a per-stack "Not symbolicated" label for `javascript`/`browser`. (§9) | `errors.mdx`; `crashContent/exception/content.tsx`; `interfaces/utils.tsx`; decision 4; T12 |
| 14 | Geo shown as all four stored fields; no country-name derivation. (§6.1) | T14 |
| 15 | `request.env` and the other unlisted v2 fields are not rendered. (§2) | the contract's field lists in §4.2 and §4.3 |
| 16 | Headers sorted case-insensitively. (§6.2) | Postgres `jsonb` key order (no stored order exists) |
| 17 | The load drops `__proto__` keys at every depth from every returned JSONB value, `metadata` and `stacktrace` included. (§3.4) | T25; coordinator ruling on `odp-sr-devalue-proto-key` |
| 18 | Every lookup keyed by event data is a `Map`/`Set` or `Object.hasOwn`-guarded. (§4.2 rule 5) | T26; review finding `odp-sr-prototype-lookup` |
| 19 | No block element inside a `<p>` anywhere; the message and the non-object context are `<div>`s; `ExpandableText` has exact inline/block markup; tables carry `<tbody>`; an SSR-plus-reparse check gates it. (§4.3, §4.6) | the HTML parser's tree-building rules, verified with `svelte/server` and jsdom; coordinator ruling on `odp-sr2-pre-in-p-ssr` |
| 20 | The P4 normaliser collapses runs of whitespace-only text nodes outside `<pre>`. (§10.1, §12.2) | verified Svelte 5.55.5 whitespace around block anchors; coordinator ruling on `odp-sr2-p4-whitespace` |
| 21 | Every diff and baseline is against `$(git merge-base origin/main HEAD)`. (header, §10, §12.1, §12.2) | decision 3 (tag facets may merge first); coordinator ruling on `odp-sr2-g5-base-pinned` |
| 22 | `StackFrames` skips non-object frames and coerces `file`/`function` before formatting. (§5.2) | `formatStackFrame` moved verbatim (§4.5); §4.5's never-throws rule; coordinator ruling on `odp-sr2-frame-shape-throw` |
| 23 | The loader reads `url?.searchParams.get('occurrence') ?? null`, so a loader test with no `url` gets S1 and no existing loader test is edited. (§3.2, §10.7) | tag facets' `page.tag-facets.test.ts` (decision 3); coordinator ruling on `odp-sr3-facets-loader-url` |
| 24 | Two exact devDependencies: `devalue` `5.8.0` and `@types/jsdom` `21.1.7`, the typings line current for the installed jsdom 25, since no `@types/jsdom` 22-26 exists. (§2, §11) | T25, T31; coordinator ruling on `odp-sr3-jsdom-types` |
| 25 | The base's `pnpm check` debt (5 errors, 1 warning) is cleared by this run in its own first commit, not accepted into G3. (§2, §11.1, §12.1, O8) | T30; coordinator ruling on `odp-sr3-g3-base-red` (option a) |

### 13.2 Sentry sources (read 2026-09-25, `master`)

- `getsentry/sentry` `static/app/components/events/interfaces/crashContent/stackTrace/content.tsx` (`frameIsVisible`, "the last
  non-app frame");
- `…/crashContent/exception/content.tsx` (`if (newestFirst) children.reverse()`,
  `initialCollapse={excIdx < values.length - 3}`);
- `…/interfaces/utils.tsx` (`isStacktraceNewestFirst`: default `true`);
- `…/interfaces/exception.tsx` (`forceFullStackTrace`);
- `…/frame/deprecatedLine.tsx` (`Show %s more frames`);
- `src/sentry/interfaces/stacktrace.py` (`get_has_system_frames` = any in-app);
- `…/breadcrumbs/breadcrumbsDataSection.tsx` (default absolute time, newest first);
- `getsentry/relay` `relay-event-schema/src/protocol/contexts/{device,os,app,runtime,browser,culture,trace,mod}.rs`;
  `relay-event-normalization/src/normalize/user_agent.rs` (`client_os` key, `OsContext`);
- `getsentry/sentry-docs` `develop-docs/sdk/foundations/envelopes/event-payloads/{contexts,breadcrumbs,stacktrace}.mdx`,
  `develop-docs/sdk/telemetry/errors.mdx`.

### 13.3 NEEDS_DECISION

None. Every choice above is derived from the contract, the code at `f49721f`, Sentry's source, or a coordinator ruling on this
run's review findings (rounds 0, 2 and 3). Where the contract leaves a choice to this spec, §13.1 says which, and why. The items in §14 go upward;
none blocks this run.

---

## 14. Findings outside this scope (recorded; only O8 is fixed here, because this run's own gate needs it)

| # | Finding | Class | Owner |
|---|---|---|---|
| O1 | ~~`/search?tag=key:value` is ambiguous for a tag key containing `:`~~ **Closed** by contract decision 6 (`aab1cc5`), which §6.3 adopts through the shared `tag-filter.ts`, and proved by TG3. | resolved upstream | — |
| O2 | `DESIGN.md` documents `--severity-warning-*` and `--border-subtle`/`--border-muted`/`--border-focus`, which `src/app.css` does not define (it has `--border-color*`, `--status-open-*`). | doc drift, pre-existing | unassigned |
| O3 | Chain frames are not symbolicated (decision 4, contract §2). | known, deferred | its own follow-up card |
| O4 | §3.2 S3b answers 404 and S3c renders the default, so a caller holding a well-formed occurrence id can learn whether it exists under some other issue. No column of it is revealed, and ids are random v4 UUIDs. This is the coordinator's ruling as prescribed. | residual of a ruling, low | coordinator |
| O5 | ~~Contract decision 6's `encodeURIComponent` form and tag facets' `URLSearchParams` form differ on `!'()~`~~ **Closed** by contract decision 7 and tag-facets decision 10 (`bc275e6`): the `encodeURIComponent` form is the one normative href, built only by the shared `tagSearchHref`, and `URLSearchParams` is not used to build it. §6.3 adopts it; TG1's `it's (x)!~` row pins it. | resolved upstream | — |
| O7 | A **pre-v2** card still hands every stored frame to `formatStackFrame` and `countSymbolicated` unguarded, as at the merge base, so a hand-written pre-v2 row with a `null` frame or a frame without a string `file` throws while rendering (a 500 under SSR). The processor never writes such a frame, and §10 forbids changing the pre-v2 card. The v2 path is guarded (§5.2, F7, SSR2). | pre-existing, hand-written rows only | unassigned |
| O8 | **The merge base fails `pnpm check`** (T30): `organizations.ts:627-633` uses `users` without importing it (5 errors, from `e91b07e` / #52), which also makes `GET /api/agent/members` throw `ReferenceError` at runtime, and the list page's `searchQuery` initialiser raises `state_referenced_locally` (1 warning). Pre-existing and unrelated to this feature, but it blocks G3, this run's own gate, so this run **clears it** in §11.1 commit 1 (coordinator ruling on `odp-sr3-g3-base-red`), proved by G3 and G3a. | pre-existing debt, **cleared by this run** | this run (§11.1 commit 1) |
| O6 | `pgStore.InsertOccurrence` (`store.go:1311`) is still in the `Store` interface, writes no v2 column, and has one test-only caller. It is harmless to this feature (T8), but reviving it would silently write v2 events as pre-v2. | dead-code hazard, pre-existing | processor owners |

---

## 15. Review findings resolved in this revision

Each finding from the round-0 spec review (`./wayctl finding list sentinel-occurrence-detail-panels`), the section that fixes it,
and the named test that fails without the fix.

| Finding | Severity | Fixed in | Proved by |
|---|---|---|---|
| `odp-sr-tag-href-decision6` | major | header re-pinned to contract @ `aab1cc5`; §6.3 adopts decision 6 and the shared `tag-filter.ts` encoder; the pinned table rewritten (`k:v` → `/search?tag=k%253Av%3Ax`); §1.3 row; §13.1 decision 9; O1 closed (§14) | TG1 (mutation: the single-encode form), TG3 (the `a:b` / `c:d e/%` round trip) |
| `odp-sr-devalue-proto-key` | major | T25; §3.4 `dropProtoKeys` on every returned JSONB value, `metadata` of all listed rows included (coordinator ruling); §2 item 2; §13.1 decision 17 | L11 (`devalue.stringify` and `uneval` of the load result), J1 |
| `odp-sr-prototype-lookup` | major | T26; §4.2 rule 5 (`Map`/`Set`/`Object.hasOwn` for every table keyed by event data); §4.4, §7.1, §8.3; §13.1 decision 18 | C6 (contexts `constructor`, `toString`, `hasOwnProperty`, `__proto__`, and a device field `constructor`), U4 (`levelTone`, crumb filters) |
| `odp-sr-truncation-untested-in-panels` | major | §4.3: every event string through `ExpandableText` (or `clip`), the E1-E38 inventory, the headline, heading type, frame function and file, crumb category and type, and `context_line` included; T27; §5.2, §7.2, §7.3, §8.1, §9.2 | TR1 (44 buttons before any expand, per panel) and TR2-TR8, each with a bare-`{value}` mutation |
| `odp-sr-hostile-coverage` | minor | §12.2 `HOSTILE_*` fills every inventory field E1-E38, `javascript:alert(1)` in every URL-like one, plus hostile key names | U1 (6): every field's literal fill is present as text; U1 (3)-(5) attribute sinks; U2 is now only a second fence |
| `odp-sr-occurrence-param-404-regression` | minor | §3.2 S1-S4 (coordinator ruling): 404 only for another issue's id; an aged-out id of this issue renders selected; malformed or unknown ignored; §3.1 `occurrenceExistsOutsideIssue`; the S3a page block; O4 | L4 (other issue / project), L8 (aged out), L9 (garbage), L10 (unknown), Q4, P7, P8 |
| `odp-sr-measurement-not-reproducible` | minor | `payload-fixture.mjs` and `measure-payload.mjs` committed in this folder; §3.3 re-measured from them, with the worst-case light payload (100 × max message: 1.1 / 4.3 / 6.4 MB) and why it does not change decision 1; §12.4 | M1 (fixture sizes ±10 %), G7 |
| `odp-sr-level-gate-verified` | note | T8 now cites `fillOccurrenceV2`'s call site, the no-`DEFAULT` column, the rolling-deploy case, the seed scripts and `InsertOccurrence`'s test-only caller; O6 | — (verification note; L6 and P4 already cover the gate) |
| `odp-sr-b4-fixture-mismatch` | note | §12.2 names the crumb offsets (crumb 96 = `-3.6s`); B4 uses crumb 96 and crumb 99; B6 and B7 moved to `BreadcrumbTimeline.test.ts` | B4, B6, B7 |

**Round 2** (`odp-sr2-*`), fixed in repair round 2 as the coordinator ruled:

| Finding | Severity | Fixed in | Proved by |
|---|---|---|---|
| `odp-sr2-pre-in-p-ssr` | major | §4.6 (new): the nesting rule, and an audit of every container E1-E38 plus the non-event ones; §4.3 `ExpandableText`'s exact inline (`<span>`) and block (`<div><pre>`) markup, and which four fields are block; §5.1 `occurrence-message` and §7.3 `context-scalar` are `<div>`; the E1 contradiction resolved (E1 is block, in a `<div>`); §6.2 `request-query` `<dl>` and `<tbody>`; §6.3 `<tbody>`; §8.1/§8.3 containers named; §11 `page.occurrence-ssr.test.ts`, `html-structure.ts`; decision 19 | SSR1-SSR3 (`svelte/server` `render()` of each panel and of the whole card on the hostile, Flutter and long fixtures, reparsed with jsdom's parse5; mutation: the message container back to `<p>` → red), N4 (a `<pre>` inside a `<p>` goes red) |
| `odp-sr2-p4-whitespace` | minor | §10.1: why the whitespace appears and why collapsing it hides no content; §12.2 `normaliseCardHtml`'s exact four steps (collapse runs of whitespace-only text nodes outside `<pre>`); decision 20 | N1 (the false-`{#if}` case), N2 (`<pre>` kept), N3 (content and separators kept), P4 |
| `odp-sr2-g5-base-pinned` | minor | header; §2; §10.1 and §10.3; §12.1 G5 and its note; §12.2 golden captured from the merge base (`issue-page-pre-v2-cards.golden.html`, sha recorded in its first line); §11; decision 21 | G5 (`git diff "$(git merge-base origin/main HEAD)"`), P4 |
| `odp-sr2-devalue-path-note` | note | §2 and §11: `devalue` `5.8.0` as an exact devDependency of `apps/dashboard-web`, the version `@sveltejs/kit` resolves (`pnpm-lock.yaml:2903`), so the script and L11 need no environment variable; §12.4 the resolution order; `measure-payload.mjs` gains the kit-relative fallback so section 3 also runs at the merge base; T25 evidence corrected to 5.8.0 | G7 (run on an installed `f49721f` checkout: section 3 prints with no `DEVALUE_PATH`), L11 |
| `odp-sr2-f6-fixture` | note | §12.2 `V2_JS_OCCURRENCE` / `V2_JS_DETAIL`: a v2 JavaScript row with two symbolicated frames carrying `raw` and one raw-only frame, with every expected line spelled out | F6 (names the fixture; mutation: `showRaw={false}` → red) |
| `odp-sr2-frame-shape-throw` | note | §5.2 "Frame shape": `frameObjects` skips non-object frames, `frameLine` coerces `file`/`function`/`line`/`column` before `formatStackFrame`, `raw` used only when an object, the v2 badge counted over `frameObjects`; §4.5; §12.2 `MALFORMED_FRAMES`; decision 22; the pre-v2 residual recorded as O7 | F7 (client), SSR2 (d) (under SSR); mutation: format the stored element → red |

The coordinator's binding note on `bc275e6` (contract decision 7) is applied in the same revision: the header re-pinned to
`bc275e6`, §6.3's encoder text, the `it's (x)!~` row added to TG1's pinned table, decision 9, and O5 closed.

**Round 3** (`odp-sr3-*`), fixed in repair round 3 as the coordinator ruled:

| Finding | Severity | Fixed in | Proved by |
|---|---|---|---|
| `odp-sr3-crumb-anchor-date` | major | T29 (the real `createdAt` is a `Date`); §8.2 `anchorMs` defines the anchor for a `Date`, an ISO string and `null`, and the table keyed on it; §8.2's audit of every timestamp consumer (crumb anchor, crumb `timestamp`, the header's `formatDate`, the date groups, `cleanOccurrenceRow`, the §3.3 measurement's devalue delta of 9 bytes per row, and the P4 golden's time-zone dependence, now pinned to UTC); §4.5 helper list; §12.2 `OCCURRED_AT` and `loaded(row)`; decision 12 | B4b (`occurredAt` a `Date` reads `-3.6s`; mutation: `parseCrumbTime`-only anchor → red), P1 (the page with `createdAt` a `Date`, same mutation), B5 (`anchorMs` values), P4 (the `TZ` pin asserted) |
| `odp-sr3-jsdom-types` | minor | T31; §2 and §11 list the run's two devDependencies, `devalue` `5.8.0` and `@types/jsdom` `21.1.7`, with the lockfile's two added packages; §12.3 `reparse`'s typing; decision 24 | G3 (`pnpm check` fails with TS7016 on `html-structure.ts` without it) |
| `odp-sr3-g3-base-red` | minor | T30 (the base's 5 errors and 1 warning, and the `/api/agent/members` runtime `ReferenceError`); §2; §11 rows and §11.1 commit 1 (the `users` import and the list page's `untrack`, a separate commit); §12.1 G3 = `0 errors and 0 warnings`; §14 O8 (pre-existing debt this run clears because its own gate needs it); decision 25 | G3; G3a (`listOrganizationMembers` called for real; mutation: remove the import → `ReferenceError`) |
| `odp-sr3-facets-loader-url` | note | §3.2 `url?.searchParams.get('occurrence') ?? null` and S1 "including no `url`"; §10.7 and §11 (no loader test edited); P5; decision 23 | L12 (no `url` → S1; mutation: drop the optional chain → `TypeError`) |
| `odp-sr3-crumb-tone-untested` | note | §12.3 B2 | B2 (crumb `tone-critical`, `tone-debug`, `tone-info`, and no `crumb-level` for a crumb without a level; mutation: drop `{levelTone(level)}` → red) |

**Round 4** (`odp-sr4-*`), folded in by the planning seat (the round-4 review raised no major or minor finding):

| Finding | Severity | Fixed in | Proved by |
|---|---|---|---|
| `odp-sr4-pr-ci-skips-check-test` | note | §12.1: the G3 row no longer says clearing the debt lets PR CI reach `pnpm test`; "What PR CI proves": PR CI runs only `pnpm build` (`ci.yml:308`, `:323`, `:330` skip migrations, `pnpm check` and `pnpm test` on `pull_request`), so G1/G3/G4 are proven by the local gates with their output in the PR body, `main`'s push run is a named post-merge check, and a red Postgres-backed suite there is triaged against #51/#53 first | G1, G3, G4 run locally and quoted in the PR body; the post-merge push run on `main` |
