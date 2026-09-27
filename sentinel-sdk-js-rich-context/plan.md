# Sentinel sdk-js rich context: implementation plan

**Feature:** `sentinel-sdk-js-rich-context` · **Repositories:** `sentinel` (`tuju-ai/sentinel`, side `be`, `packages/sdk-js`), then
`web` (`tuju-ai/web`, side `fe`), one run, `--order sentinel:web`, tier **full** (the tunnel is the `credentials` surface).
**Bases:** sentinel `main` @ `dde0ea2` (v0.2.39, still `origin/main` on 2026-09-26); web `main` @ `69f8d865e` (2.45.6, still
`origin/main` on 2026-09-26, no commit after it).
**Built from:** `sentinel-sdk-js-rich-context/spec.md` **revision 13** (`6effd15`, branch `NurfitraPujo/sentinel-sdk-js-rich-context-specifying`),
which passed spec review round 12 with 0 findings. Contract `prd/sentinel-sdk-js-rich-context/contract.md`, `be.md`, `fe.md` @
`e030211` (decisions 1-7). **If this plan and the spec disagree, the spec wins**, and the seat reports the conflict instead of
resolving it silently. A spec defect is reported (`NEEDS_DECISION`), never patched in the spec.
**Cards:** `TASK-01` … `TASK-05` → the sentinel seat (one PR). `TASK-06` → the web seat (one PR). Section numbers below (`§3.7`,
`§11.2`) are the spec's unless prefixed `plan`.

**Invariant this feature holds.** Every event any sdk-js entry sends is accepted whole by the ingestor's strict decode,
`ValidatePayload` and protovalidate, is publishable to NATS, and carries no URL secret the SDK recorded — before and after this
change. What changes is *where* identity, tags and context travel (typed fields instead of `metadata`), not that invariant.

**Primitives reused, not re-implemented** (recorded so review does not re-propose them): the Go-ruler byte count
(`src/core/serialize.ts: metadataWireBytes`) for every new size rule; `sanitize` for every object the SDK stores; `shrinkMetadata`
for every marker re-run and for §4.6 step 4 (extended by two optional parameters, not forked); `src/core/pii.ts: PII_KEYS` as the
substring half of the URL key test; the tunnel suite's `commit()` (extracted to `tests/helpers/commit.ts`, not copied) for the
golden corpus and the estimate sidecars; the existing Go replays (`tests/unit/sdk_js_fixture_test.go`,
`sdk_js_tunnel_fixture_test.go`) as the cross-language proof, extended rather than joined by a new harness. Rejected: a second
metadata shrink for the proto measure (drifts from the JSON one the first time either is fixed); a byte reservation for markers
(spec §4.5 rule 3 says why); Playwright (contract decision 4).

---

## 1. Seats and waves

| | Wave 1 — sentinel | Wave 2 — web |
|---|---|---|
| **Seat** | `sentinel-sdk-js-rich-context-implementing.sentinel` | `sentinel-sdk-js-rich-context-implementing.web` |
| **Repository** | `sentinel` | `web` |
| **Branch** | the one the plane gives the seat, cut from `main` @ `dde0ea2`, **carrying `sentinel-sdk-js-rich-context/spec.md` and this `plan.md`** (merge or cherry-pick this planning branch first, so the PR carries what it implements) | the one the plane gives the seat, cut from web `main` @ `69f8d865e` |
| **Cards** | TASK-01, TASK-02, TASK-03, TASK-04, TASK-05 | TASK-06 |
| **Depends on** | nothing in this run | the rc tag `packages/sdk-js/v0.2.0-rc.N`, which the **coordinator** pushes on the sentinel seat's branch head once wave 1's gates (plan §3) pass (contract decision 3). The web brief names `N`. |
| **Pull request** | one, against `main`: `feat(sdk-js): typed fields, breadcrumbs, contexts and the widened tunnel — 0.2.0 (sentinel-sdk-js-rich-context)` | one, against `main`: `feat: adopt @sentinel/sdk-js 0.2.0 — user, tenant, breadcrumbs, URL stripping (sentinel-sdk-js-rich-context)` |
| **Commits** | C1 … C11 (plan §2) | W1 … W4, then W5 after the final tag (plan §4) |
| **Not the seat's** | pushing any tag (`v0.2.0-rc.N` and `v0.2.0` are the coordinator's, decision 3; TASK-05's tag is cut on the **merge** commit after merge) | the staging observation (§11.4, coordinator evidence); merging on an rc (never) |

**Why one sentinel seat, not five.** Every card writes `src/core/capture.ts`, `src/core/event.ts`, `src/core/types.ts` and the
fixture corpus that every other card regenerates (spec §10). Five seats would five-way-conflict on those files; one seat runs the
cards as an ordered commit sequence instead.

**Work in flight in the same repository.** `sentinel-sdk-go-rich-context` (sibling run, `packages/sdk-go`) is specified to edit
three files this plan also edits: `Taskfile.yml` `test-unit` (it **adds** a `packages/sdk-go` line; C2 here edits the two
tunnel-fixture drift lines), `.github/workflows/ci.yml` (its `go-sdk` job; C2 here edits only `sdk-js-node`'s drift step) and
`docs/sdk-specification.md` (its sdk-go rows; C11 here edits only sdk-js rows). Both add files to the flat `tests/unit` package.
The hunks do not overlap. Whichever PR merges second rebases, re-runs `go vet ./tests/unit/...` and plan §3's G1, and reports
the rebase. Neither seat edits the other's rows.

---

## 2. Sentinel seat — commit sequence C1 … C11

### 2.0 Rules for every commit

- **Base measurement first (B0).** Before C1, on the untouched base, run plan §3's environment block and record:
  `cd packages/sdk-js && npx -y pnpm@10.34.5 install --frozen-lockfile && npx -y pnpm@10.34.5 test` → **27 files / 491 tests
  passed** (measured by this planning seat on `6effd15`, whose tree equals `dde0ea2` outside `sentinel-sdk-js-rich-context/`), and
  `go test ./tests/unit/... -count=1` → record the pass/fail/skip count. That is the baseline "no worse than" is judged against:
  a test red at B0 is pre-existing and not this feature's to fix; quote it in the report.
- **Each commit builds and passes the unit gate on its own**, and each card's proofs land in the same commit as the code they
  prove. Red first: write the commit's tests, see them fail on **behaviour** (add signatures as stubs first so they compile), then
  write the code. For every proof the commit lands, perform the spec's *"shown red by …"* demonstration named in §11.2 (delete or
  revert the named production line, watch the named test go red, restore) and record `mutation → test → red` in the commit body.
  A proof that stays green under its mutation is decoration: fix it before committing (CLAUDE.md working conventions).
- **Fixtures are generated, never hand-written, and always committed with the change that altered them.** `tests/fixtures/*.json`,
  `tests/tunnel-fixtures/*.json` and (from C2) `tests/fixture-estimates/**` are written by the vitest suites locally and are
  compared, never written, under `CI`. Any commit that changes what the pipeline emits (C1, C4, C6, C7, C8, C9, C11 at least)
  regenerates them in its own local run and commits them; the drift checks (C2 widens them to all three directories) fail a
  commit that forgot.
- **Per-commit check (P)** — light-to-medium, sequential, never two at once:

  ```bash
  cd packages/sdk-js
  npx -y pnpm@10.34.5 typecheck && npx -y pnpm@10.34.5 build     # build BEFORE test: tests/package.test.ts and tests/inertness read dist/
  npx -y pnpm@10.34.5 test                                         # local run: regenerates fixtures → git add them
  CI=1 npx -y pnpm@10.34.5 test                                    # never writes: the committed fixtures equal what the suite produces
  cd ../..
  go vet ./tests/unit/... && go test ./tests/unit/... -count=1     # tests/unit is ONE flat package: one bad file silences all of it (B4)
  git status --porcelain                                           # empty after the commit
  ```

  The two dashboard lines and `go test ./apps/ingestor-go/...` of `task test-unit` cannot change result between these commits,
  because no file under `apps/` changes (`git diff --stat dde0ea2 -- apps/` is empty at every commit, and the seat checks it).
  The **full** `task test-unit` (plan §3 G1) runs at **C2** (the commit that edits `Taskfile.yml`) and at the **head**.
- **Browser floor from C2 on:** no code under `src/` may use regex lookbehind, `\p{}`, `replaceAll`, `structuredClone`,
  `toWellFormed`, `.at(`, `Object.fromEntries`, … (§5.1 *Regex syntax*, *Built-in API floor*); C2 lands the scan that fails on them.
- Commit messages name the plan step and the card: `feat(sdk-js): … (sentinel-sdk-js-rich-context/TASK-0x, C<n>)`.

### 2.1 Order, and why the riskiest pieces go first

```
C1  tunnel ITEM_KEYS + empty-user delete ........ TASK-01 (§7)          ← credentials surface; decides every later tunnel fixture
C2  proto-size estimator + sidecars + drift gate  TASK-02 (§4.6, §7.3)  ← the size model's upper-bound proof, on today's corpus
C3  setExtra + never-drop metadata shrink ........ TASK-01 (§3.7)        ← server-parity shrink every marker re-run depends on
C4  typed level/sdk/user/tags/contexts ........... TASK-01 (§3.1-§3.7)
C5  breadcrumb buffer core ....................... TASK-02 (§4.1-§4.4)
C6  afterHook allow-list + server bounds ......... TASK-01 (§3.8, §3.10)
C7  URL privacy filter (message, frames) ......... TASK-03 (§5.1)
C8  automatic contexts + captureRequestError ..... TASK-04 (§6, §7.3 tunnel-v2-event)
C9  browser keepalive trim + NATS publish shed ... TASK-02 (§4.5, §4.6)
C10 automatic browser breadcrumbs ................ TASK-03 (§5, §5.2-§5.6)
C11 release 0.2.0 ................................ TASK-05 (§8)
```

- **C1 first**: the tunnel holds web's API key (contract tier) and is the one change here on a trust boundary. It is also a
  precondition for C4: once the SDK sends `user: { ip_address: "{{auto}}" }` (C4), a tunnel without C1's empty-user delete would
  forward `user: {}` for every identity-less browser event, and the `tunnel-no-ip` fixture would record it.
- **C2 second**: the size model is what §4.5, §4.6 and every fixture's Go check rest on. C2 proves `protoSizeEstimate` against Go's
  `proto.Size` on the whole existing corpus and pins the estimator's exact values, before any code sheds by it. Every fixture a
  later commit adds is checked against Go the moment it lands.
- **C3 third**: the server-parity shrink (never drops; exempt keys last; capped `truncated_keys`) is what C4's `'tags'` marker,
  C6's `'contexts'` marker and C9's `'breadcrumbs'` re-run all call. It is also where server parity is most delicate (the 958-extras
  case), so it lands alone.
- **The shed loops (C9) cannot go earlier than C8**: §11.2 TASK-02 cases (3), (4) and (8) assert on the node runtime's automatic
  `runtime` / `os` contexts (C8), and case (8)/(9) need §3.10's bounds (C6). C9 still lands **before** C10, the commit that makes
  long breadcrumb trails ordinary in the browser.

### 2.2 The commits

Paths are repo-relative; `sdk/` abbreviates `packages/sdk-js/`. "Rewrites" are §11.3's rows, each rewritten in the commit whose
change turns its old assertion red.

#### C1 — the tunnel forwards the typed fields (TASK-01, §7.1, §7.2)

| | |
|---|---|
| **Files** | `sdk/src/nextjs/route.ts`: `ITEM_KEYS` (`~L93`) gains exactly `level sdk user tags contexts request breadcrumbs`; `applyClientIp` (`~L327`) case B4 deletes a rebuilt `user` with no key; the filter stays `Set.has` over `Object.keys` (`~L420`), `applyClientIp` still after `normaliseBody`; body/item limits, forward headers unchanged. `sdk/tests/route/route.test.ts` (§7.2; rewrites the key-set test `~L319`, the `user: {}` table rows `~L795-796`, "with none, no user at all" `~L803`). `sdk/tests/tunnel-fixtures.test.ts` (its header comment about `ITEM_KEYS` removing `user`). `sdk/tests/tunnel-fixtures/*.json` regenerated (the rebuild table's no-IP rows lose `user: {}`). `tests/unit/sdk_js_tunnel_fixture_test.go`: `TestTunnelFixtureShape`'s `user` assertion (§7.3 bullet 3), `TestTunnelFixtureRebuildDecode`'s no-IP rows, and the new `TestTunnelFixtureV2StrictDecode` decoding **every** route fixture's `forwarded.body` into `[]validation.ErrorPayload` with `DisallowUnknownFields` (D4). Its `tunnel-v2-event` assertions arrive in C8. |
| **Proofs** | §7.2's cases: each of the six non-`user` keys survives deep-equal; `user` rebuilt with an IP, deleted when empty without one, `{ id, username, email }` kept otherwise; the not-admitted keys absent; `USER` / `Tags` / `Breadcrumbs` / `uſer` dropped; `{{auto}}` + `IP_ADDRESS` + `geo` + extra key → `{ id, username, email, ip_address: <derived> }`; the order proof. |
| **Shown red by** | removing one key from `ITEM_KEYS`; a case-insensitive filter; `applyClientIp` before `normaliseBody`; reverting the B4 delete (the "no user" cases). For the strict decode: a scratch unknown key in one fixture's forwarded body (revert). |
| **Criteria** | TASK-01's tunnel half (§11.2 TASK-01, last clause: `tests/route/route.test.ts` for `ITEM_KEYS`). |

#### C2 — the size model's estimator, its sidecars, and the widened drift gate (TASK-02 §4.6 estimator, §7.3; TASK-03 scan)

| | |
|---|---|
| **Files** | `sdk/src/core/proto-size.ts` (new): `vl`, `fld`, `val`, `structBytes`, `protoSizeEstimate` exactly as §4.6's formula, `NATS_PUBLISH_BUDGET = 1_000_000`, `utf8Bytes` by `TextEncoder`. `sdk/tests/core/proto-size.test.ts` (new). `sdk/tests/helpers/commit.ts` (new; the tunnel suite's `commit()` moved here, `sdk/tests/tunnel-fixtures.test.ts:67` then imports it). `sdk/tests/fixtures.test.ts`: `emit` (`~L36`) writes through `commit()` (never writes under `CI`) and writes `tests/fixture-estimates/golden/<name>.json`; the tunnel suite writes `tests/fixture-estimates/tunnel/<name>.json` for every route fixture (not `tunnel-user-rebuild`). `sdk/tests/fixture-estimates/**` (new, generated). `tests/unit/sdk_js_fixture_test.go`: after protovalidate, clone the mapped event, clear `ProjectKey` / `ProjectId`, assert `proto.Size ≤ protoSizeEstimate`; new `TestFixtureEstimatesHaveFixtures`. `tests/unit/sdk_js_tunnel_fixture_test.go`: the same per item inside `TestTunnelFixtureV2StrictDecode`, and as many estimates as items. `Taskfile.yml` `test-unit` (`~L200-201` only): the `git diff` and untracked checks gain `packages/sdk-js/tests/fixture-estimates` and `packages/sdk-js/tests/fixtures`. `.github/workflows/ci.yml`, `sdk-js-node`'s step *tunnel fixtures committed and unchanged* (`~L415-419`) only: the job-relative pathspecs `tests/fixture-estimates` and `tests/fixtures` (never the repo-root spelling, `enr-rev2-ci-drift-pathspec`). `sdk/tests/core/es-compat.test.ts` (new): the §11.2 TASK-03 scan of `src/` (lookbehind, `\p{`, `replaceAll(`, the built-in list, `src/cli/**` and `src/upload/**` exempt from the built-in list); it passes on today's `src/`. The `URL_IN_TEXT.flags === 'g'` assertion joins it in C7. |
| **Proofs** | §11.2 TASK-02 *The NATS publish bound*: `val(null)`, `val(false)` 2, `val(0)` 9, `val('')` 2, `val('é')` 4, `val([0])` 13, `structBytes({ a: 0 })` 16, `structBytes({ a: { b: [null, true, 'x'] } })` 31, `structBytes({ a: Array(2_030).fill(0) })` 22 345, `structBytes({ a: Array(32_700).fill(0) })` 359 715. *The estimate against Go on every fixture* for the existing 18 golden and 5 route fixtures (`tunnel-user-rebuild.json` is a table, not a forward, and has no sidecar, §13 F2). |
| **Shown red by** | `val(number)` → 8; `structBytes` measured in JSON bytes (the `Array(32_700)` pin); a sidecar edited to 1 (Go red); a sidecar whose fixture was deleted (`TestFixtureEstimatesHaveFixtures` red); an edited sidecar under `CI=1` (vitest red) and locally (the `git diff` gate red). Scan: a scratch `replaceAll(` in `src/` (revert). |
| **Also** | Run the **full** `task test-unit` (plan §3 G1) at this commit: it is the one that edits the gate. `tests/unit/pnpm_pin_test.go` must stay green (it parses the same `test-unit` block). |
| **Criteria** | TASK-02 (estimator half of §4.6; the per-fixture Go bound), TASK-03 (the es-compat scan, first half). |

#### C3 — `setExtra` / `setExtras` and the metadata shrink that never drops (TASK-01 §3.7; TASK-02 case (10), shrink parts)

| | |
|---|---|
| **Files** | `sdk/src/core/event.ts`: `METADATA_EXEMPT_KEYS` (the server's 18 names, `bounds.go: metadataExemptKeys`) replaces `SHRINK_EXEMPT_KEYS` (`~L35`); `shrinkMetadata` (`~L277`) runs §3.7 step 1 (every other key but `runtime`, largest first, earlier-inserted wins a tie) then step 2 (exempt keys, largest first, lexical tie), writes and **measures** `truncated_keys` in the server's capped `removedKeys` shape before every measurement, never removes `runtime`, returns nothing. `sdk/src/core/capture.ts`: `boundAndShrink` (`~L61`) never drops; `metadata_too_large` stays in `SentinelSdkErrorCode` (`src/core/types.ts`) but is no longer emitted. `sdk/src/core/state.ts` (`Scope.extras`), `sdk/src/core/scope.ts` (`setExtra`, `setExtras`: `null` deletes; `runtime source level truncated truncated_keys user_id tenant_id` dropped with a debug line), `assembleMetadata` (`~L139`) spreads extras **before** `ctx.metadata` (in C3 the 0.1.x tag / identity keys still come first; C4 removes them), exports from `src/core/index.ts`, every runtime entry and `src/nextjs/index.d.ts`. |
| **Proofs** | `sdk/tests/core/capture.test.ts`: rewrites B11 `~L83` (`route: '/x'` now kept because small, not exempt) and `~L104` (`{ route: 70 KiB }` → sent, `route` removed, `small` kept, `truncated_keys: ['route']`, no `onError`); §11.2 TASK-02 case (10)'s *coordinator's pin* (`setExtra('route', 'x'.repeat(70_000))` from `build`), *the exempt keys go last, in the server's order* (`request_id` alone; `request_id` / `request-id` / `small`), and *the cap* (`setExtra('request_id', 'r1')` + 3 000 60-char extras through the real `./browser` `init`: 958 kept, `request_id` kept, 50 names + `'+1992 more'`); `setExtra(s)` → `metadata`, reserved keys refused (§11.2 TASK-01). |
| **Shown red by** | 0.1.x's `boundAndShrink` (dropped with `metadata_too_large`); deleting step 2 (metadata over 65 536); 0.1.x's order (`request_id` removed first); measuring the full `truncated_keys` list in steps 1-2 (every extra and `request_id` removed). |
| **Criteria** | TASK-01 (`setExtra(s)`), TASK-02 case (10) shrink sub-cases. The browser/node halves of case (10) land in C9, its *at build* half in C6. |

#### C4 — typed `level`, `sdk`, `user`, `tags`, `contexts`; identity and tags leave `metadata` (TASK-01 §3.1-§3.7, §3.9)

| | |
|---|---|
| **Files** | `sdk/src/core/types.ts` (`SentinelEvent` v2 keys, `Level` five values, `User`, `RequestInfo`, `SdkInfo`, `CaptureContext.user` / `.tags`, `BrowserInitOptions.autoIpAddress`), `sdk/src/core/version.ts` (new: `SDK_VERSION = '0.1.2'`, equal to `package.json` until C11), `state.ts` (`Scope = { tags: Map<string,string>, user?, contexts: Map, extras }`, `InternalClient.sdkName`), `scope.ts` (`setUser` replace semantics and §3.4 bounds; `setTag` / `setTags` / `setTenant` by §3.5 rules 1-5, `RESERVED_TAG_KEYS` reduced to `tenant_id`; `setContext` with `sanitize` then `isJsonObject` on the **sanitised** value), `event.ts` (`assembleMetadata` → `{ ...extras, ...ctx.metadata, runtime, source?, react_component_stack? }`; no `metadata.level`; `TRUNCATED_VOCABULARY` gains `breadcrumbs contexts tags` in that final order after `stacktrace.function`), `capture.ts` (`build`: level defaults §3.2, `captureMessage(message, levelOrContext?)` (D5), `ctx.user` / `ctx.tags` for one event, `ctx.metadata.user_id` / `tenant_id` dropped, the 50-tag cap keeping `tenant_id` with the `'tags'` marker before the shrink (§3.5 rule 6), `tags` / `contexts` built by `forEach` into fresh objects, `sdk` stamped, `{{auto}}` by `autoIpAddress` on browser only), `handlers/node.ts` (`uncaughtException` → `fatal`), `handlers/browser.ts` (top-level `level: 'error'`), `init.ts` / `options.ts` (`initCore(options, runtime, plan, sdkName)`, `autoIpAddress` degradation), `sdk/src/browser.ts` / `node.ts` / `edge.ts` (`initBrowser` / `initNode` / `initEdge(options, entry)`), `sdk/src/nextjs/client.ts` and `nextjs/edge.ts` (own `init` shadowing `export *`, entry `nextjs`), `nextjs/server.ts` (`initNode(options, 'nextjs')`), `src/core/index.ts`, `src/nextjs/index.d.ts` (`setTags`, `setContext`, the new types). Regenerated: every golden fixture (`level`, `sdk`; browser ones `user.ip_address: "{{auto}}"`), every route fixture and every sidecar. `sdk/tests/fixtures.test.ts`: `metadata-near-the-cap` passes `request_id` instead of per-call `user_id`; `tests/unit/sdk_js_fixture_test.go` `TestSDKJSFixtures_PinnedValues` near-the-cap subtest pins `payload.Metadata["request_id"]`. |
| **Proofs** | `sdk/tests/core/scope.test.ts` (new): every §11.2 TASK-01 clause for `setUser`, per-call `ctx.user` / `ctx.tags`, `setTag` / `setTags`, `setTenant`, **the 50-tag cap keeps `tenant_id`** (all five sub-cases; the `beforeSend` sub-case lands in C6), `setContext` including the `new Date()` / `new URL()` / `toJSON` cases, `level` per capture path, `sdk.name` for all six entries + `initCore`, `sdk.version === package.json version`, `autoIpAddress` on / off / browser-only, and on **every event the file builds** `metadata` has no `user_id`, `tenant_id`, `level` or `setTag` key. `sdk/tests/package.test.ts`: `SDK_VERSION` equals `package.json`'s `version`. Rewrites: `capture.test.ts` B12 group (`~L130-260`), B11 `~L83` (`request_id` as the exempt key), N12 (`~L328-345`); `handlers/browser.test.ts` B26 (`~L36`); `init/browser.test.ts` B22 (`~L127-240`); `core/classify.test.ts` B3 (`~L178`, `request_id`); `transport/lifecycle.test.ts` (`~L260`, identity via `ctx.user`); `fixtures.test.ts` (`~L175-202`). |
| **Shown red by** | deleting §3.5 rule 6 (51 tags sent, the server's cut then drops `tenant_id`); leaving `user_id` in `assembleMetadata` (the every-event assertion); removing the `isJsonObject`-after-`sanitize` check (`setContext('x', new Date())` stored as a string); the 0.1.x `captureException` level (`fatal` ignored). |
| **Criteria** | TASK-01 "each API landing in its typed field; metadata no longer carries tags / `user_id`" (JS half). |

#### C5 — the breadcrumb buffer (TASK-02 §4.1-§4.4)

| | |
|---|---|
| **Files** | `sdk/src/core/breadcrumbs.ts` (new: the ring, §4.2 normalisation, §4.3 `beforeBreadcrumb` failing closed, the Go-ruler per-crumb and total sizes of §4.4), `types.ts` (`Breadcrumb`, `BreadcrumbHint`, `BeforeBreadcrumb`, `CoreOptions.maxBreadcrumbs` / `beforeBreadcrumb`), `state.ts` (`Scope.breadcrumbs`), `scope.ts` (`addBreadcrumb`), `init.ts` / `options.ts` (`maxBreadcrumbs` 0-100 with `invalid_option`; trim on a second `init`; retained crumbs passed through a newly installed hook), `capture.ts` `build` (attach a copy — `{ ...crumb, data: sanitize(crumb.data).value }`, never `structuredClone` — and the 131 072 total, oldest first, measured once per crumb), exports + `src/nextjs/index.d.ts` (`addBreadcrumb`). No fixture changes (no golden fixture records a crumb yet). |
| **Proofs** | `sdk/tests/core/breadcrumbs.test.ts` (new): every clause of §11.2 TASK-02's first sentence (150 adds; `maxBreadcrumbs` 5 / 0 / 101 / second `init`; hook modify / `null` / throw / non-object; copy isolation; timestamp forms incl. year 10000; `data` over 4096 by the Go ruler; `Date` / `URL` / `toJSON` data; `addBreadcrumb('x')` no-op; the 131 072 total). §11.2 TASK-03's *"a retained pre-`init` crumb is dropped by a `beforeBreadcrumb` installed at `init`"* (§4.1). |
| **Shown red by** | the §4.4 copy written as `structuredClone(crumb)` → `es-compat.test.ts` red (the scan's own §11.2 demonstration); a JSON-byte `data` measure (the Go-ruler case); a throwing hook failing open. |
| **Criteria** | TASK-02 "buffer bounded; `beforeBreadcrumb` can drop or modify; events carry breadcrumbs". |

#### C6 — `afterHook` from an allow-list, and the server's contexts / headers / request-string bounds (TASK-01 §3.8, §3.10)

| | |
|---|---|
| **Files** | `sdk/src/core/server-bounds.ts` (new: `boundContexts`, `boundStringMap`, `boundRequestStrings`, `compareCodePoints`; the lone-surrogate repair a `charCodeAt` loop), `capture.ts` `afterHook` (`~L81`: step 1 fresh object from `SentinelEvent`'s exact keys; step 2 every row of §3.8's table, including `breadcrumbs` re-normalised with "unusable timestamp → stamped now" and the last 100 kept; step 3 the bounds **before** the shrink) and `build` (§3.10 at build; frame `line` / `column` kept only as integers 1-2 147 483 647 at build and after the hook). |
| **Proofs** | `sdk/tests/core/server-bounds.test.ts` (new) and `capture.test.ts`, every clause of §11.2 TASK-01 from *§3.10's mirrored bounds* through *the hook's crumb count* and *`afterHook` re-normalisation, one case per row of §3.8* (allow-list; case variants; returned-object `toJSON`; `environment` fallback; `release_version`; `fingerprint_too_long`; `trace_*`; `line: 1e12`; contexts `Date` / `URL` / `toJSON`; crumb `data: URL`; both unusable timestamps stamped now; the `beforeSend` 50-tag sub-case). §11.2 TASK-02 case (10)'s *at build* sub-case (40 contexts + 65 480-char `route` → 32 contexts, `['contexts']`, `truncated_keys: ['route']`). **Cross-language:** golden `v2-hostile-before-send.json` and `v2-rich-context.json` (both node runtime, §7.3) + their sidecars; `TestSDKJSFixtures_AcceptedByIngestor` picks both up by glob; new pinned subtests in `TestSDKJSFixtures_PinnedValues` (§7.3 bullets 4-5: `Logger`/`Dist`/`Source` empty, `Exception` nil, `Environment` pre-hook, `Stacktrace[0].Line` 0, `Contexts` has only `ok`, `Breadcrumbs[0].Data` nil, `Request` nil; every typed field of `v2-rich-context` in place and no `user_id` / `tenant_id` / tag key in its metadata). |
| **Shown red by** | each rule the case pins (§11.2 names them); the surrogate repair written as `s.toWellFormed()` → `es-compat.test.ts` red; removing a field from the allow-list rebuild (the hostile fixture fails the Go strict decode or protovalidate). |
| **Criteria** | TASK-01 (typed-field guarantees after `beforeSend`; the Go replay "accepts a v2 event with `DisallowUnknownFields`", contract §5). |

#### C7 — the URL privacy filter; event `message` and frame files filtered (TASK-03 §5.1)

| | |
|---|---|
| **Files** | `sdk/src/core/url.ts` (new: `stripUserinfo`, `filterUrl`, `filterQuery`, `filterUrlsInText`, exported `URL_IN_TEXT` with flags `g` only, the lenient repeated decode, rule 3's key test = `PII_KEYS` substrings + segment-exact `code state key signature sig jwt otp credential`, rule 5's recursion to 3 levels), `capture.ts` `build` (`message` through `filterUrlsInText` on every capture path, **before** `boundAndShrink`; every `stacktrace[].file` through `filterUrl` before the `MAX_FILE_BYTES` cut), `sdk/src/core/pii.ts` only if the key list must be exported. `es-compat.test.ts` gains `URL_IN_TEXT.flags === 'g'`. |
| **Proofs** | `sdk/tests/core/url.test.ts` (new): every `filterUrl` / `filterQuery` / `filterUrlsInText` clause of §11.2 TASK-03 — keys (substring and segment), non-matches, byte-for-byte preservation, fragments, `;` separator, userinfo, nested URLs, round 3's closed residuals, lenient decode, the residuals pinned as **kept** (`/verify?t0k`, `next=reset_token%3DX`, `hash is #access_token=X`, `q=John Doe&token=abc`), the `URL_IN_TEXT` match shape, and linear time (`'-/'.repeat(500_000)` within 1 s). `capture.test.ts` *Error messages* (five cases) and *Frame files*. |
| **Shown red by** | as §11.2 lists per case: deleting rule 5 / rule 0 / group 4 / the `stripUserinfo` call; decoding once; revision 7's strict decode; revision 4's comma pattern; `URL_IN_TEXT` written with `(?<![\w/])` (es-compat red); deleting the `filterUrlsInText` call in `build`. |
| **Criteria** | TASK-03 URL privacy half (contract §5 "a sensitive query value is `[Filtered]`"). |

#### C8 — automatic contexts per runtime; `captureRequestError` fills `request` and `transaction` (TASK-04 §6; tunnel `tunnel-v2-event`)

| | |
|---|---|
| **Files** | `sdk/src/browser/contexts.ts` (new, §6.1: `request.url` / `query_string` filtered **then** cut, `User-Agent` header, `culture`, `device`; **no** `browser` / `os`, D2; no `Referer`), `sdk/src/node/contexts.ts` (new, §6.2; `node:os` imported by the node entry only), `sdk/src/edge.ts` (§6.3, `runtime` without `version`), `sdk/src/node.ts` / `nextjs/server.ts` (`serverName` on `NodeInitOptions`), `state.ts` (`InternalClient.autoContexts` provider), `capture.ts` `build` (merge automatic under app contexts key by key, app keys win; then §3.10), `sdk/src/nextjs/request-error.ts` (§6.4: `request.method` / `url` / `query_string` / headers through the drop list, `[Filtered]` names, `filterUrl` on every value holding `?`/`#`, then `boundStringMap`; `transaction = routePath`; no `metadata.http_method` / `http_url`; `route` etc. stay). `sdk/tests/helpers/pipeline.ts` (accepts the fixed provider of §7.3). Both fixture suites install the fixed values (Chrome/Windows UA, `en-US`, `Asia/Jakarta`, 1920 × 1080 @ 1, 8 GiB, 8 CPUs, page `https://app.example/dashboard?tab=1`; node `v20.0.0`, `Linux` / `6.0.0`, `fixture-host`). New route fixture **`tunnel-v2-event`** (§7.3 bullet 2; its "one of each automatic kind's shape" crumbs are added through `addBreadcrumb`, since the integrations arrive in C10) + sidecar; every other fixture and sidecar regenerated. `tests/unit/sdk_js_tunnel_fixture_test.go`: `tunnel-v2-event` joins `tunnelRouteFixtures` (so the shape, Huawei and public-peer replays cover it), and `TestTunnelFixtureV2StrictDecode`'s `tunnel-v2-event` assertions (§7.3 bullet 3: `level`, `sdk.name` `sentinel.javascript.browser`, user fields + derived IP, `tags.tenant_id` + custom tag, the `setContext` context, `request.url` / UA, every crumb `timestamp` a `time.Time`, and after `enrich.Apply` `contexts.browser` / `os` from the body UA). |
| **Proofs** | `sdk/tests/contexts/browser.test.ts` (jsdom), `node.test.ts`, `edge.test.ts` (new), and `sdk/tests/nextjs/adapter.test.ts` extended — every clause of §11.2 TASK-04. Rewrites `adapter.test.ts` N3 and the edge case (`~L50-150`). |
| **Shown red by** | an SDK-sent `contexts.browser` (the ingestor's UA parse is then blocked: the Go D2 assertion red); automatic context overwriting an app key (the `culture` case); leaving `metadata.http_url`; reading `navigator` outside the provider (the fixture differs between node 20 and 22 — see plan §5 R6). |
| **Criteria** | TASK-04 "each runtime's events carry its contexts (unit + Next fixture tests)"; TASK-01's tunnel cross-language proof (`TestTunnelFixtureV2StrictDecode` over `tunnel-v2-event`). |

#### C9 — the browser keepalive trim and the NATS publish shed, in `enqueue` (TASK-02 §4.5, §4.6)

| | |
|---|---|
| **Files** | `capture.ts` `enqueue` (`~L136`): §4.5 rules 1-5 for `runtime === 'browser'` (one loop, marker inside the measure, the re-run after each marker, rule 5 removing `contexts` then every tag but `tenant_id`), then §4.6 steps 1-5 on every runtime (crumbs oldest first, contexts in the server's victim order, metadata by `shrinkMetadata` with the proto measure and stop test, step 5 sends). `event.ts`: `shrinkMetadata`'s two optional parameters (per-key measure, stop test; defaults are C3's behaviour; with a stop test only step 1 runs). Golden fixtures **`v2-publish-bound.json`**, **`v2-estimate-numbers.json`**, **`v2-estimate-strings.json`** (node runtime, §7.3) + sidecars; their pinned Go subtests (`proto.Size` ≤ 1 000 000 and > 950 000, fewer than 31 crumbs, `truncated` ∋ `breadcrumbs`; 50 tags / 32 contexts / 64 headers / 64 env and `estimate − 8 × entries < proto.Size`). |
| **Proofs** | `sdk/tests/core/size.test.ts` (new): the 100 × 2 KiB browser trim with `keepalive` on hide (the `tests/transport/hide.test.ts` pattern), the same event on node untrimmed, the rule-5 shed, *the marker inside the loop*, *near the metadata cap* (and its 262 142 variant), cases **(1), (1b), (2)-(9)**, and case **(10)**'s browser and node halves — each asserting outcomes, not byte counts (§11.2). |
| **Shown red by** | revision 7's marker-after-loop order; deleting the re-run; revision 9's whole-`tags` removal; revision 11's 900 000 budget (case (1b)); deleting §4.6 (cases (1)-(4); `v2-publish-bound` then carries 31 crumbs and measures over 1 048 576 in Go); a JSON-byte measure (case (1)); deleting §3.10's headers / env bound (case (5)) or contexts bound (case (8)) or `boundRequestStrings` (case (9)); the `8 ×` term removed or the skeleton counted in UTF-16 units (`v2-estimate-strings` Go red); JSON-byte values (`v2-estimate-numbers` Go red). |
| **Criteria** | TASK-02 "an oversized trail is trimmed to fit the transport" (contract §5) and the §4.6 publish bound. |

#### C10 — automatic browser breadcrumbs (TASK-03 §5, §5.2-§5.6)

| | |
|---|---|
| **Files** | `sdk/src/browser/integrations/{console,fetch,xhr,navigation,click}.ts` (new), `sdk/src/browser.ts` (install after the client exists, unless no-op or `maxBreadcrumbs` 0; the `breadcrumbs` option and its degradation), `types.ts` / `options.ts` (`BrowserInitOptions.breadcrumbs`), `state.ts` (`teardown` uninstalls; restore only when the global still holds the SDK's wrapper, else pass-through). The fetch wrapper returns the **chained** promise (§5 code block); no other wrapper reacts to a page promise. |
| **Proofs** | `sdk/tests/browser/integrations.test.ts` (new, jsdom): the page test (console, `fetch('/api/x?token=t0k&page=2')` with body, XHR, `pushState('/reset?code=c0de')`, click on `button#save.btn.primary`, then `captureException`: categories `console` `fetch` `xhr` `navigation` `ui.click`; none of `secret-input-value` / `secret-body` / `t0k` / `c0de`; `token=[Filtered]`, `code=[Filtered]`; selector `form > button#save.btn.primary`; no tunnel URL), each integration switched off alone and `breadcrumbs: false`, `close()` restore and pass-through behind a later wrapper, console bounds, *console: filter, then cut*, selector depth / 80 chars / 1000 ms debounce. `sdk/tests/browser/fetch-rejection.test.ts` (new, jsdom, the `withSoleListener` bridge): uncaught → exactly 1 `unhandledrejection` and 1 event; caught → 0; resolved → same `Response`; XHR `open` / `send` return values and rethrow. jsdom lacks `fetch` / XHR / `deviceMemory`: the tests **install** stubs before `init`, never skip (§11.1). |
| **Shown red by** | returning the original promise with record-only reactions (uncaught counts 0); rethrowing reaction on the original (caught counts 1); cutting before filtering (console `S3CR3T` / `%252`); recording `input.value`; not excluding the transport URL. |
| **Criteria** | TASK-03 "a browser test page produces all four breadcrumb types on a captured error" (jsdom, decision 4). |

#### C11 — release 0.2.0 (TASK-05 §8)

| | |
|---|---|
| **Files** | `sdk/package.json` `version` `0.2.0`; `sdk/src/core/version.ts` `'0.2.0'`; lockfile untouched (no dependency change; if one changes, regenerate with pnpm 10.34.5 only). `sdk/CHANGELOG.md`: the *Unreleased — the tunnel writes the browser's IP* heading becomes part of the first `## v0.2.0 — …` entry, **Behaviour change** first, every item §8 lists (incl. the 1 000 000 budget's coupling to what the ingestor adds, `metadata_too_large` no longer emitted, D2's reason). `sdk/README.md`: *What is not implemented* loses Breadcrumbs, keeps tracing / per-request server scope (workaround now `user: { id }` / `tags: { tenant_id }` per call) / `withSentinelErrorBoundary` / index-map `url`; new sections for scope APIs, breadcrumbs and each integration switch, *URL privacy* with its *Not filtered* list, contexts per runtime, the §4.5 / §4.6 size rules, `autoIpAddress` with §3.4's three bullets verbatim; *Bundle size* updated from `pnpm size`. `docs/sdk-specification.md`: the sdk-js `max_breadcrumbs` row (`~L37`), §5's URL filter — sdk-js rows only (§13 F1's generic default is not ours). Every fixture and sidecar regenerated (`sdk.version`). |
| **Proofs** | `sdk/tests/package.test.ts` (version `0.2.0`, `SDK_VERSION` equal); `sdk/tests/docs.test.ts` extended (first `## ` entry is `v0.2.0`; *What is not implemented* has no breadcrumbs and still lists tracing and per-request server scope; the *Not filtered* list contains `scheme-less`, `location.search`, `location.hash`, `more than 3`, `keyless`, `next=reset_token%3DX`, `#access_token=X`, `q=John Doe&token=abc`, `addBreadcrumb`). The existing *"has every §4.10 section, in that order"* test (`docs.test.ts:43`) must stay green: add sections without reordering the existing ones. |
| **Shown red by** | leaving `SDK_VERSION` at `0.1.2`; removing one residual from the README list. |
| **Criteria** | TASK-05 "README 'not implemented' list updated" (the gate half). The tag is post-merge (plan §6). |

### 2.3 Paths the sentinel seat may write

```
packages/sdk-js/src/**                    C1-C11 (new: core/{breadcrumbs,url,version,proto-size,server-bounds}.ts,
                                                  browser/contexts.ts, browser/integrations/*.ts, node/contexts.ts)
packages/sdk-js/tests/**                  C1-C11 (incl. fixtures/, tunnel-fixtures/, fixture-estimates/, helpers/commit.ts)
packages/sdk-js/package.json              C11 (version only)
packages/sdk-js/README.md, CHANGELOG.md   C11
tests/unit/sdk_js_fixture_test.go         C2, C4, C6, C9
tests/unit/sdk_js_tunnel_fixture_test.go  C1, C2, C8
Taskfile.yml                              C2 — test-unit's two drift lines only
.github/workflows/ci.yml                  C2 — sdk-js-node's "tunnel fixtures committed and unchanged" step only
docs/sdk-specification.md                 C11 — sdk-js rows only
sentinel-sdk-js-rich-context/{spec,plan}.md   carried from the planning branch, NOT edited
```

**Must not write:** `apps/**` (contract §2: no ingestor, processor or dashboard change), `packages/proto/**`, `gen/**`,
`packages/db-migrations/**`, `packages/sdk-go/**`, `packages/shared-go/**`, `tests/integration/**` (including
`enrichment_test.go`, which reads five tunnel fixtures by name and is not extended), `tests/e2e/**`, `deploy/**`, every other
`Taskfile.yml` task and `ci.yml` job, `packages/sdk-js/pnpm-lock.yaml` (unless a dependency changes, which none should). A needed
change outside this list is reported to the coordinator, not made.

---

## 3. Sentinel gates (before reporting), in order

**Environment, set once per shell** (this machine has crashed repeatedly from RAM exhaustion; fish syntax, then bash):

```fish
set -gx GOFLAGS -p=3
set -gx GOCACHE /home/fitrapujo/.cache/go-build-sentinel-sdk-js-rich-context
set -gx TMPDIR /home/fitrapujo/.cache/tmp-sentinel-sdk-js-rich-context; mkdir -p $TMPDIR
```
```bash
export GOFLAGS=-p=3 GOCACHE=/home/fitrapujo/.cache/go-build-sentinel-sdk-js-rich-context
export TMPDIR=/home/fitrapujo/.cache/tmp-sentinel-sdk-js-rich-context && mkdir -p "$TMPDIR"
```

`TMPDIR` and `GOCACHE` live on `/home`, never on `/tmp` (tmpfs, i.e. RAM). **One heavy suite at a time**: no gate runs beside
another gate, a compose stack, a container, or another seat's suite in this terminal. Run in a private worktree, never a shared
checkout under `~/works/`.

| # | Command (from the repository root unless stated) | Weight | What it holds |
|---|---|---|---|
| G1 | `rtk task test-unit` | **HEAVY** (`go test ./tests/unit/... -v`, `go test ./apps/ingestor-go/...`, dashboard install + vitest, sdk-js install + vitest under pnpm 10.34.5, the three drift checks) | every sdk-js proof and both Go replays, never skipping (spec §11.1, contract §5) |
| G2 | `cd packages/sdk-js && npx -y pnpm@10.34.5 test -- --sequence.shuffle` | light (≈ 4 s at B0) | the suite is order-independent (`vitest.config.ts`) |
| G3 | `npx -y pnpm@10.34.5 typecheck && npx -y pnpm@10.34.5 build && npx -y pnpm@10.34.5 check:no-dependencies && npx -y pnpm@10.34.5 size` (in `packages/sdk-js`) | light | what CI's `sdk-js-node` runs on a PR; zero runtime dependencies; the size figure C11 records (report-only) |
| G4 | `CI=1 npx -y pnpm@10.34.5 test` (in `packages/sdk-js`), then `git status --porcelain` from the root | light | CI's own mode: the suites compare, never write; the tree is clean |

G1 must end **no worse than B0**: every B0-green test still green, plus the new ones. A red test that was red at B0 is quoted and
classified pre-existing; anything else is this change's to repair, then the sequence restarts from G1. If node 20 and 22 are both
available (`fnm list`), run G4 under each (`fnm exec --using=20 …`, `--using=22 …`): CI compares fixtures byte for byte on both.
Otherwise say which node ran and that CI's `sdk-js-node` matrix is the check.

**Deliberately not run:**
- **`task test-integration` — never.** The spec agrees: §11.1 lists only the commands above, and contract §5 puts every sdk-js
  proof under `task test-unit`. This change touches no server code (`apps/**`, proto, migrations are out of paths), and the Go
  replays already run the ingestor's own strict decode, `ValidatePayload`, `MapPayloadToEvent`, protovalidate and `enrich.Apply`
  in-process on every fixture. `tests/integration/enrichment_test.go` reads five route fixtures by name and the rebuild table; the
  properties it asserts there (the stored client IP, `tunnel-clean`'s UA-derived contexts, six rebuild cases) are preserved by
  C1/C8 and asserted in-process by `TestTunnelFixtureShape` / `HuaweiReplay` / `PublicPeerReplay` / `RebuildDecode`. CI's
  `integration` job runs only on `workflow_dispatch`.
- No compose stack, no e2e suite, no container of any kind.
- No second `GOWORK=off go test ./tests/unit/...`: the Go edits are test files of the root module only, and nothing under
  `packages/db-migrations` or `packages/sdk-go` changes, so workspace and `GOWORK=off` resolve the same code.

**Report (on the return channel):** outcome, PR URL, head SHA, B0's counts, each gate's command and result (G1 counts per
section, G2/G4 file and test counts, G3's `sdk_js_client_entry_gzip_bytes`), the per-commit mutation log, the node versions G4 ran
on, and any pre-existing failure quoted verbatim. Then the coordinator pushes `packages/sdk-js/v0.2.0-rc.N` on that head.

---

## 4. Web seat — commit sequence W1 … W5

**Starts only when** the coordinator has pushed `packages/sdk-js/v0.2.0-rc.N` on the sentinel seat's head and named `N` in the
brief. **Base measurement (WB0)** on `69f8d865e`, before W1: `npx -y pnpm@10.13.1 install --frozen-lockfile` and every gate of
plan §4.2; record the counts and any red gate verbatim. That is web's "no worse than" baseline.

Use the repo's pnpm, **10.13.1** (`packageManager`), for every command: `npx -y pnpm@10.13.1 …` (never pnpm 10.34.5, which is
sentinel's sdk-js pin, and never 11). Installing the rc fetches `github.com/tuju-ai/sentinel` at the tag and runs the package's
`prepare` build through npm (W1's consumer path); if the tag cannot be fetched, report `BLOCKED_EXTERNAL` naming the URL.

### 4.1 The commits

| | Files | Proofs (spec §9.5) | Shown red by |
|---|---|---|---|
| **W1** pin the rc | `package.json` → `git+https://github.com/tuju-ai/sentinel.git#packages/sdk-js/v0.2.0-rc.N&path:packages/sdk-js`; `pnpm-lock.yaml` (`npx -y pnpm@10.13.1 install`, then commit; `specifier` and resolved `commit` change); `src/app/api/sentinel/__tests__/route.test.ts` in the **same** commit (W12: the one test that runs the real SDK): `beforeEach` stubs `SENTINEL_PROJECT_KEY` to `undefined`; the forward case expects headers `{ content-type, x-api-key, 'x-sentinel-derive-ip': 'off' }` and body `[{ message, error_class, platform, user: { ip_address: '10.0.0.1' } }]`; new case "forwards a 0.2.0 item's typed fields through the widened ITEM_KEYS" (§9.5). | the two `route.test.ts` cases | the old v0.1.2 expectations fail against the rc (that is the red) |
| **W2** the privacy module | `src/common/utils/sentinel-url-privacy.ts` (new: `stripUserinfo`, `stripUrl`, `URL_IN_TEXT` copy, `stripQueriesInText` with `replace` putting the prefix back, `stripBreadcrumbUrls` (never `null`), `stripEventUrls` incl. `message`, every header value holding `?`/`#`, `query_string` deleted, frames and `exception.values[].stacktrace[].file`); `src/common/utils/__tests__/sentinel-url-privacy.test.ts` (new, node env). Comments must not spell the scanned tokens, and must pass `sg`'s `no-prose-comments`. | one test per W8 carrier (reset, invite, six callbacks × `code`/`state`/`scope`, `/api/proxy?u=`, `search=`, `q=`, `f=`, `cf=`) over nav/fetch crumbs and `request.url` / `query_string` / `referer`; RSC console line; `x-envoy-original-path`; frame files; the source scan (no lookbehind, no `replaceAll`, no §5.1 built-in, `URL_IN_TEXT.flags === 'g'`); nested / comma / fragment / userinfo cases; the **message** case | the pattern with `(?<![\w/])`; revision 4's comma rule; deleting group 4; deleting either `stripUserinfo` call; deleting `stripEventUrls`' `message` line |
| **W3** the three inits | `instrumentation-client.ts`, `sentinel.server.config.ts`, `sentinel.edge.config.ts`: `beforeBreadcrumb: stripBreadcrumbUrls`; `beforeSend` = 401/403 filter first (unchanged), then `stripEventUrls(event)` before anything that can throw, then (client only) the `sentry_replay_id` copy inside `try { … } catch {}`; no other option changes; `next.config.mjs` untouched. `src/__tests__/instrumentation.test.ts`. | per init: `beforeBreadcrumb` strips a `reset_token` crumb; `beforeSend` strips and still drops 401/403; client stamps the replay id; `Sentry.getClient` throwing → stripped event returned, no throw; the **message** twin | deleting `stripEventUrls`' `message` line (the twin goes red, proving the wiring) |
| **W4** identity and tenant | `src/layouts/dashboard/shell/index.tsx` (inside `if (!impersonation)`: `setUser({ id: me.id, email: me.email, username: me.name })`, `setTenant(me.companyId)`, `setContext('tenant', { id: me.companyId, name: me.company?.name })`, D3); `src/common/utils/credential.ts` (`endSession` adds `setContext('tenant', null)`); tests `shell/__tests__/index.test.tsx`, `credential.test.ts`, `credential.server.test.ts`, `src/app/__tests__/layout-shells.test.tsx` (mocks gain `setContext`). | the three calls and none while impersonating; logout clears the context in the browser, the server variant calls none | reverting to `setUser({ id })`; dropping the `setContext(null)` |
| **W5** final pin (a **separate dispatch**, after the sentinel PR merges and the coordinator tags `packages/sdk-js/v0.2.0` on its merge commit) | `package.json` + `pnpm-lock.yaml` **only**, `#packages/sdk-js/v0.2.0&path:packages/sdk-js`. If `route.ts` changed between the rc and the tag, `route.test.ts` changes in this commit too, and the report says why. | gates of plan §4.2 re-run | — |

W1 goes first because W2-W4 type-check against 0.2.0's API (`setContext`, `beforeBreadcrumb`, the widened `Breadcrumb` /
`SentinelEvent` types). W2 precedes W3, which imports it. If the coordinator later names a newer rc (the sentinel PR changed
after review), a commit that changes only the pin, the lockfile and, if needed, `route.test.ts` follows, and the gates re-run.
**web never merges on an rc**: the PR description names the pinned tag, and review checks `package.json` says `v0.2.0`.

**Per-commit check:** `npx -y pnpm@10.13.1 typecheck` then `npx -y pnpm@10.13.1 vitest run <the commit's test files>`
(sequential). The full gates run at W4 (and again at W5).

### 4.2 Web gates, in order (spec §11.1)

| # | Command | Weight |
|---|---|---|
| WG0 | `npx -y pnpm@10.13.1 install --frozen-lockfile` | medium (network: fetches the sentinel tag and runs its `prepare` build) |
| WG1 | `npx -y pnpm@10.13.1 check` (`scripts/check.mjs`: format, knip `check:dead`, `sg`, structure, boundaries, secrets, docs, agents, lint, typecheck — the step `pr-checks.yml` runs first) | **HEAVY** |
| WG2 | `npx -y pnpm@10.13.1 typecheck` | heavy (tsc) |
| WG3 | `npx -y pnpm@10.13.1 lint` | heavy |
| WG4 | `npx -y pnpm@10.13.1 test` | **HEAVY** (full vitest) |
| WG5 | `npx -y pnpm@10.13.1 check:lockfile` | light |
| WG6 | `npx -y pnpm@10.13.1 check:stale-mocks` | light (the mocks that gained `setContext` must match the real module) |
| WG7 | `npx -y pnpm@10.13.1 test:coverage` | **HEAVY** (per-file coverage; the new `sentinel-url-privacy.ts` must meet it) |

One at a time, in this order; WG2 and WG3 repeat parts of WG1 and stay because they are what a failing lane names. Every gate
ends no worse than WB0. Report: outcome, PR URL, head SHA, the pinned tag, WB0 and final counts, each gate's result, the
mutation log.

### 4.3 Paths the web seat may write

`package.json`, `pnpm-lock.yaml`, `instrumentation-client.ts`, `sentinel.server.config.ts`, `sentinel.edge.config.ts`,
`src/layouts/dashboard/shell/index.tsx`, `src/common/utils/credential.ts`, `src/common/utils/sentinel-url-privacy.ts` (new), and the
tests `src/common/utils/__tests__/sentinel-url-privacy.test.ts` (new), `src/__tests__/instrumentation.test.ts`,
`src/layouts/dashboard/shell/__tests__/index.test.tsx`, `src/common/utils/__tests__/credential.test.ts`,
`src/common/utils/__tests__/credential.server.test.ts`, `src/app/__tests__/layout-shells.test.tsx`,
`src/app/api/sentinel/__tests__/route.test.ts`. **Not:** `next.config.mjs`, `src/app/api/sentinel/route.ts` (the tunnel's options
do not change), anything `@sentry/nextjs`, `instrumentation.ts`.

---

## 5. Risks, in the order the plan retires them

| # | Risk | Retired at | How |
|---|---|---|---|
| R1 | **The tunnel** (credentials surface) forwards a browser-chosen IP, or a case-folded key Go would decode as a real field, or `user: {}` | C1 | exact-key filter kept; `applyClientIp` after it; empty rebuilt `user` deleted; route tests + strict Go decode of every forward from C1 on |
| R2 | **The size model** under-counts, so §4.6 lets an unpublishable event through, or over-counts and sheds data the server would keep | C2 (estimator), C9 (shed) | exact-value pins measured against Go; `proto.Size ≤ estimate` on every fixture at every later commit; the two teeth fixtures in C9 go red on both known wrong measures |
| R3 | **Server parity of the metadata shrink** (never drop; exempt order; capped `truncated_keys`) | C3 | the 958-extras case is the server's own outcome on `dde0ea2` |
| R4 | **Fixture churn**: ten commits regenerate the corpus, and a forgotten file passes locally | C2 onward | drift gate covers all three fixture directories; `CI=1` run per commit; `git status --porcelain` clean |
| R5 | **Safari 12 floor**: a lookbehind literal or post-12 built-in breaks web's client bundle on old iOS, or throws inside a wrapper / `beforeSend` | C2 (scan), C5 / C6 / C7 (its reds), W2 (web's scan) | plain-text source scans on both sides |
| R6 | **Node 20 vs 22 fixture drift** (node ≥ 21 has a global `navigator`) | C8 | automatic contexts only through the injectable provider; fixed values in both fixture suites; G4 on both nodes where available |
| R7 | **fetch wrapper swallows `unhandledrejection`** — 0.1.x's capture of uncaught failed fetches silently stops | C10 | chained promise; `fetch-rejection.test.ts` counts 1 / 0 with a real listener bridge |
| R8 | **Sibling sdk-go run edits the same three files** | merge time | disjoint hunks (plan §1); the later PR rebases and re-runs G1 |
| R9 | **RAM**: the machine crashes under parallel heavy suites | every step | `GOFLAGS=-p=3`, caches on `/home`, one heavy suite at a time, no containers |
| R10 | **web: rc unreachable, or lockfile written by the wrong pnpm** | W1 | `npx -y pnpm@10.13.1`; `check:lockfile`; `BLOCKED_EXTERNAL` if the tag cannot be fetched |
| R11 | **web merges on an rc** | W5 | W5 is a separate dispatch after the tag; review checks `v0.2.0` |
| R12 | A green gate claimed for a post-merge fact (tag, staging) | never | plan §6: those are coordinator evidence, not seat claims |

---

## 6. Criterion → proof → commit

| Card criterion | Proof (spec §11.2) | Lands in | Gate |
|---|---|---|---|
| **TASK-01** each API lands in its typed field | `tests/core/scope.test.ts` (user, tags, tenant, 50-tag cap, `setContext`, level per path, `sdk.name` per entry, `sdk.version`, `autoIpAddress`) | C4 | G1, G2 |
| | `setExtra(s)` → `metadata`, reserved keys refused | C3 | G1, G2 |
| | §3.10 mirrored bounds, `boundRequestStrings`, the hook's crumb count, `afterHook` one case per §3.8 row | C6 | G1, G2 |
| TASK-01 metadata no longer carries tags / `user_id` | "on every event built in the file" assertion (`tenant_id`, `level`, tag keys too) | C4 | G1, G2 |
| TASK-01 Go replay accepts a v2 event, `DisallowUnknownFields` (contract §5) | `TestSDKJSFixtures_AcceptedByIngestor` + pinned `v2-rich-context`, `v2-hostile-before-send` | C6 | G1 |
| | `TestTunnelFixtureV2StrictDecode` over every route fixture | C1 (all forwards), C8 (`tunnel-v2-event` assertions) | G1 |
| TASK-01 tunnel `ITEM_KEYS` (contract §4.5) | `tests/route/route.test.ts` (§7.2) | C1 | G1, G2 |
| **TASK-02** buffer bounded | `tests/core/breadcrumbs.test.ts` (150 adds, `maxBreadcrumbs`, 131 072 total) | C5 | G1, G2 |
| TASK-02 `beforeBreadcrumb` can drop or modify | `breadcrumbs.test.ts` hook cases; the retained-crumb case | C5 | G1, G2 |
| TASK-02 events carry breadcrumbs | `breadcrumbs.test.ts` copy-at-capture cases | C5 | G1, G2 |
| TASK-02 an oversized trail is trimmed to fit the transport (contract §5) | `tests/core/size.test.ts` browser trim, marker in loop, near cap, rule 5, cases (1)-(10) | C9 (case (10): C3 shrink parts, C6 at-build part) | G1, G2 |
| | estimator pins `tests/core/proto-size.test.ts`; `proto.Size ≤ estimate` on every fixture; `TestFixtureEstimatesHaveFixtures` | C2 (grows with each fixture) | G1 |
| | `v2-publish-bound`, `v2-estimate-numbers`, `v2-estimate-strings` pinned Go subtests | C9 | G1 |
| **TASK-03** a browser test page produces all four breadcrumb types on a captured error (jsdom, decision 4) | `tests/browser/integrations.test.ts` page test | C10 | G1, G2 |
| TASK-03 each integration can be switched off; no body, no input value | same file: per-integration off, `breadcrumbs: false`, `close()`, the absent-secret assertions | C10 | G1, G2 |
| TASK-03 a sensitive query value is `[Filtered]` | `tests/core/url.test.ts`; `capture.test.ts` error messages and frame files; console filter-then-cut | C7 (console part C10) | G1, G2 |
| TASK-03 Safari-12-safe code | `tests/core/es-compat.test.ts` | C2 (reds at C5, C6, C7) | G1, G2 |
| TASK-03 page `unhandledrejection` still reported | `tests/browser/fetch-rejection.test.ts` | C10 | G1, G2 |
| **TASK-04** each runtime's events carry its contexts (unit) | `tests/contexts/{browser,node,edge}.test.ts` | C8 | G1, G2 |
| TASK-04 … (Next fixture tests) | `tests/nextjs/adapter.test.ts` extended (request, transaction, headers, `sdk.name`, no `http_*`) | C8 | G1, G2 |
| TASK-04 browser/OS contexts reach Sentinel (D2, server side) | `TestTunnelFixtureV2StrictDecode`'s `enrich.Apply` assertion on `tunnel-v2-event` | C8 | G1 |
| **TASK-05** README "not implemented" list updated | `tests/docs.test.ts` extended; `tests/package.test.ts` 0.2.0 | C11 | G1, G2 |
| TASK-05 tag `v0.2.0` pushed | `git ls-remote --tags origin packages/sdk-js/v0.2.0` equals the sentinel **merge** commit (peeled `^{}` for an annotated tag) | **post-merge, coordinator evidence** (spec §11.2 *Proofs no gate can hold*) | — |
| **TASK-06** (web wiring) user, tenant, URL stripping | web vitest of §9.5: `sentinel-url-privacy.test.ts`, `instrumentation.test.ts`, shell, credential, layout-shells, `route.test.ts` | W1-W4 | WG4, WG7 |
| TASK-06 a staging browser error shows user, browser/OS context and breadcrumbs | §11.4's observation, both occurrence ids | **post-deploy, coordinator evidence** | — |

**Post-merge sequence (coordinator, not a seat):** sentinel wave-1 gates pass → coordinator pushes `v0.2.0-rc.N` on the sentinel
head → web wave 2 (W1-W4) → sentinel PR merges → coordinator tags `packages/sdk-js/v0.2.0` on the merge commit and records the
`ls-remote` SHA on TASK-05 → web W5 (pin to `v0.2.0`) and its gates → web PR merges → web deploys to staging → §11.4's
observation recorded on TASK-06.

## 7. Stop conditions

- `NEEDS_DECISION`: a spec rule cannot be implemented as written (for example, a Go-measured outcome the spec pins — the 958
  extras, a case's shed count — does not reproduce on `dde0ea2`). Quote the rule, the measured value and the command. Do not
  adjust the expectation to match the code.
- `BLOCKED_EXTERNAL`: the rc or final tag cannot be fetched by web's install; GitHub is unreachable for the push or PR.
- Everything else inside the declared paths is repaired, not asked about.
