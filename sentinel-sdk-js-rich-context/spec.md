# Sentinel sdk-js rich context (typed fields, breadcrumbs, contexts, web adoption): implementation specification

**Feature:** `sentinel-sdk-js-rich-context` · **Repositories:** `sentinel` (`NurfitraPujo/sentinel`, side `be`, `packages/sdk-js`) and
`web` (`example-org/web`, side `fe`), one run, `--order sentinel:web` · **Base:** sentinel `main` @ `dde0ea2` (v0.2.39), web `main` @
`69f8d865e` (2.45.6).
**Built against:** `prd/sentinel-sdk-js-rich-context/contract.md`, `be.md` and `fe.md` @ `e030211` (decisions 1-7). If this document
and the contract disagree, the contract wins, except where §1.3 records that the contract's description of the code is wrong or its
proposal is unworkable as written. Each such case says what this spec does instead, and why.
**Work items:** `sentinel-sdk-js-rich-context/TASK-01` … `TASK-05` (side `be`) and `TASK-06` (side `fe`). §11.2 maps every card
criterion to a proof that runs under sentinel's `task test-unit` or web's `pnpm test` (vitest) and cannot skip there, except the
two post-merge facts (TASK-05's tag, TASK-06's staging observation) that §11.2 *Proofs no gate can hold* assigns to the coordinator. TASK-03's card
still says "(Playwright)"; contract decision 4 replaces that with a jsdom proof, and §11.2 uses it.
**Outcome of the open questions:** none are left open, and there is no `NEEDS_DECISION`. Every rule the contract left to the spec
(§4.1-§4.6) is fixed below; §12.1 names the source each one was derived from. §1.3 lists seven places where the contract is wrong
about today's code, proposes something that would make the data worse, or asks for something the runtime cannot supply, and what
this spec does about each. §12.2 lists the privacy and trust-boundary consequences the spec reviewer should see before merge.
**Revision 2 (2026-09-26):** repairs the 11 findings and 1 note of an internal adversarial review of revision 1 (`131eb9b`):
the tunnel no longer forwards an empty `user` (§7.1); every committed fixture is machine-independent (§7.3); URLs inside console
messages and bare query strings are filtered (§5.1, §5.2); every URL-bearing server header is filtered (§6.4) and web strips them
all (§9.4); web strips before anything that can throw (§9.3); per-call `ctx.user` / `ctx.tags` replace the per-call metadata
identity keys so metadata never carries them (§3.7, decision 7); the Next client and edge builds name themselves `nextjs` (§3.3);
retained breadcrumbs pass a newly installed `beforeBreadcrumb` (§4.1); the size bound of §4.5 is recomputed. §12.4 maps each
finding to its fix.
**Revision 3 (2026-09-26):** repairs the six findings of spec review round 1: every object the SDK puts where `ErrorPayload`
declares an object is checked **after** `sanitize` (a `toJSON` can turn it into a string or array, §3.6, §3.8, §4.2); no regex
the spec prescribes uses lookbehind or any other syntax newer than Safari 12, and a test pins that (§5.1, §9.4, §11.2); web's
real-SDK tunnel test is listed with its new expected forward (§9.5, §10); stack-frame files are URL-filtered by the SDK and
stripped by web (§5.1, §9.4); `afterHook` rebuilds the event from an allow-list and type-checks every v1 field, so the §3.8
guarantee holds (§3.8); §11.2 states who records the two proofs no gate can hold. §12.5 maps each finding to its fix.
**Revision 4 (2026-09-26):** repairs the two findings of spec review round 2. The fetch wrapper returns a promise chained on
the original's instead of the original promise object, so an uncaught failed `fetch` still fires `unhandledrejection` exactly
once and 0.1.x's capture still reports it; no other wrapper reacts to a page promise (§5, §5.3, §5.4, §11.2). A URL nested in a
query value (`next=/reset-password?reset_token=X`) is filtered recursively (§5.1 rule 5), and `URL_IN_TEXT`'s query ends at a
comma followed by a scheme, still without lookbehind (§5.1, §9.4, §9.5, §11.2). §12.6 maps each finding to its fix.
**Revision 5 (2026-09-26):** repairs the three findings of spec review round 3 (0 majors). Revision 4's comma rule is **dropped**
from `URL_IN_TEXT`, on both sides: it ended a query at any `,http(s)://`, so `see /p?next=x,https://h/q&reset_token=S` lost its
`reset_token` to no match, and rule 5 already filters the case the comma rule was written for (§5.1, §9.4). The SDK now runs
`filterUrlsInText` over every event's `message`, which is where an error's own message travels (§5.1 *Where the filters run*),
and web's `message` strip is pinned by a test that goes red without it (§9.5). The URL-filter residuals are closed rather than
recorded: keys and nested values are decoded until stable (at most 3 decodes), a nested value whose fragment names a sensitive
key is treated as a URL, and `filterUrlsInText` / `stripQueriesInText` drop fragments (§5.1 rules 2 and 5, `URL_IN_TEXT` group 4).
The new pattern has no lookaround at all and runs in linear time. What stays open is listed in §12.2(3) and the README (§8).
§12.7 maps each finding to its fix.
**Revision 6 (2026-09-26):** repairs the one note of spec review round 4 (0 majors, 0 minors). §5.1 rule 2 splits a query on
`;` as well as `&`, so `filterUrl('/p?a=1;reset_token=X')` filters `reset_token`; a keyless query part (`/verify?<token>`) is
kept and now listed as a residual, with the reason, in §12.2(3) and the README (§8); §12.2(3) no longer says the SDK never
records URLs in crumb messages (console crumbs do, filtered, §5.2). §12.8 maps the finding to its fix.
**Revision 7 (2026-09-26):** repairs the four notes of spec review round 5 (0 majors, 0 minors). Userinfo (`user:pass@`) is
removed by `filterUrl` (new rule 0, and rule 5's nested values), by `filterUrlsInText` and by web's `stripUrl` /
`stripQueriesInText` (§5.1, §9.4); a query nested without `?` and a bare fragment in text are listed as residuals (§12.2(3), §8,
§11.2 TASK-05). A `beforeSend` crumb with an unusable timestamp is stamped now, not removed (§3.8). A marker `enqueue` adds to
`metadata.truncated` is followed by a `shrinkMetadata` re-run (§4.5). web's command list gains `pnpm check` (§11.1). §12.9 maps
each note to its fix.
**Revision 8 (2026-09-26):** repairs spec review round 6 (0 majors, 1 minor, 1 note). §4.5's rules 2 and 3 become one loop whose
measure includes the marker: the first crumb removed adds `'breadcrumbs'` to `metadata.truncated` and runs the `shrinkMetadata`
re-run, and the loop continues against the re-measured bytes, so the marker can no longer push a trimmed event with crumbs left
back over 61 440. Rule 3's claim about `keepalive` fit is corrected, and the reviewer's boundary case and the near-cap case are
pinned (§11.2 TASK-02). §5.1's decode is lenient (a `%E9` or a trailing `%` no longer hides a nested URL). Every URL-bearing
string is filtered whole before it is cut (§5.1 *Filter, then cut*, §5.2, §6.1, §6.4). §12.10 maps both findings to their fixes.
**Revision 9 (2026-09-26):** repairs spec review round 7 (1 major, 1 minor, 1 note). The binding size limit is NATS's
`max_payload` (1 048 576 bytes) on the protobuf `ErrorEvent` the ingestor publishes per item, not a JSON cap (S11). New §4.6: on
every runtime, `enqueue` computes an upper-bound proto-size estimate that counts every `Struct` value at its proto cost. Over
900 000 bytes, it sheds breadcrumbs oldest first, then contexts (non-well-known first), then metadata through the existing
shrink, with §4.5's markers. §3.8 drops a `request.data` over the server's own 16 384-byte bound. Proven with the reviewer's
numbers (§11.2 TASK-02), whose event measures 1 053 681 proto bytes. That the ingestor accepts over HTTP what it cannot publish is
reported upward (§13 F4). When the effective tags exceed 50, the SDK keeps `tenant_id` and drops the lexically largest other keys
(§3.5 rule 6). A URL concatenated into text with an unencoded space, quote, `<` or `>` in its query is listed as a residual
(§12.2(3), §8, §11.2). §12.11 maps each finding to its fix.
**Revision 10 (2026-09-26):** repairs spec review round 8 (1 major, 2 minors, 1 note). The SDK now applies the server's own
bounds on `contexts` (32 names, 65 536 Go-ruler bytes, custom names before the six well-known ones, largest first) and on
`request.headers` / `request.env` (64 entries each, keys of 1-256 code points, values cut to 4096 / 1024 code points, the
lexically largest keys dropped), at `build` and again in `afterHook`, exactly as it already applies the metadata shrink, the
50-tag cap, the breadcrumb total and the `request.data` bound (new §3.10). §4.6's estimate therefore counts only what the server
keeps, and revision 9's "over-count, deliberately" defence is removed: an app's 1.2 MB context no longer costs it its breadcrumbs
(§11.2 TASK-02 case (8)). §11.2 TASK-02 case (7) now states what the browser actually does with case (1): every crumb removed,
sent by §4.5 rule 4 without `keepalive`. Every golden and tunnel fixture's `protoSizeEstimate` is committed beside it
(`tests/fixture-estimates/`), and the Go replay asserts `proto.Size` is within it on every fixture, including a new number-heavy
and a new string/header-heavy one, drift-gated like the tunnel fixtures (§7.3). §4.5 rule 5 removes every tag except
`tenant_id`, and a test pins it. §12.12 maps each finding to its fix.
**Revision 11 (2026-09-26):** repairs spec review round 9 (0 majors, 2 minors). A `beforeSend` can no longer put a typed field
on the wire past the server's bound. `afterHook` cuts `request.method` / `url` / `query_string` to 16 / 2048 / 4096 code points
after removing U+0000, as the server's `boundRequest` does, through a new `boundRequestStrings` that `build` runs too (§3.10), and
it keeps the last 100 of a hook's `breadcrumbs` (§3.8). §3.8 now lists every field a hook can set with its bound, so §3.10's
claim that the estimate counts only what the server keeps holds whatever set the field. A hook's 1 000 001-character URL no
longer costs a node event its 100 crumbs, or a browser event its delivery (§11.2 TASK-02 case (9)). A marker whose
`shrinkMetadata` re-run returns `false` is **withdrawn**: `metadata` goes back to its copy from before the marker, which is within
65 536, and the step continues. `enqueue` never drops an event for its metadata (§4.5 rule 3, §4.6 steps 2-3), and `build` /
`afterHook` withdraw this feature's `'contexts'` / `'tags'` markers the same way before falling back to 0.1.x's own drop, now
recorded as §13 F5 (§11.2 TASK-02 case (10), `setExtra('route', 'x'.repeat(65_480))`). A crumb is copied at capture as
`{ ...crumb }` with `data` run through `sanitize` again, never `structuredClone` (§4.4). §5.1 states the browser built-in floor
beside the regex list: Safari 12, and SDK code does not count on Next 15.5's `@next/polyfill-module`. `tests/core/es-compat.test.ts`
and web's scan fail on every API past that floor that the code could reach for (`structuredClone`, `toWellFormed`,
`isWellFormed`, `.at`, `findLast`, `Object.hasOwn`, `Object.fromEntries`, …). The audit found three more places where the spec's
code invited such an API, and each is now written out. 0.1.x's own post-Safari-12 calls are recorded as §13 F6. §12.13 maps each
finding to its fix.
**Revision 12 (2026-09-26):** repairs spec review round 10 (0 majors, 1 minor) under the coordinator's two rulings. (a) §4.6's
budget is **1 000 000** (`NATS_PUBLISH_BUDGET`), as in the sibling sdk-go spec, leaving 48 576 bytes below NATS's 1 048 576 for
what the ingestor adds after the SDK, at most ~21 KB. At 900 000 the SDK shed crumbs from events the ingestor would publish: the
reviewer's node event with 28 crumbs (estimate 988 224, Go 986 550) lost 4. It is now sent whole, and a test pins it (§11.2
TASK-02). The case figures, the `v2-publish-bound` window and case (5), which no longer reached step 5, are redone. (b) The
metadata shrink **never drops an event**. `SHRINK_EXEMPT_KEYS` gives way to the server's own 18 exempt names
(`bounds.go: metadataExemptKeys`), which `route` is not in. Once every other key is gone, the shrink removes the exempt keys too,
last and in the server's order, recording each in `truncated_keys`, and if only the markers are left it caps `truncated_keys` in
the server's own shape. `runtime` is never removed (S5). `metadata_too_large` is no longer emitted, and
`setExtra('route', 'x'.repeat(70_000))` is sent with `route` removed (§3.7). Revision 11's marker withdrawal goes with it, because
the re-run can no longer fail (§4.5 rule 3). §13 F5 is closed, not recorded. §12.14 maps the finding to its fix.

**Revision 13 (2026-09-26):** repairs spec review round 11 (0 majors, 1 minor, 1 note). The metadata shrink writes and measures
`truncated_keys` in the server's capped shape (`bounds.go: removedKeys`: the first 50 names, each cut to 128 bytes, then
`"+N more"`) before every measurement of steps 1 and 2, as the server's `shrinkMetadata` does per removal, so the marker can no
longer force an exempt key out, and revision 12's step 3 is folded into that rule (§3.7). §11.2 TASK-02 case (10)'s *The cap*
now pins the server's outcome for `setExtra('request_id', 'r1')` plus 3 000 extras with 60-character keys: 958 extras kept,
`request_id` kept, 50 names and `'+1992 more'`, checked against `validation.ValidatePayload` on `dde0ea2`. The error-code union is
named by its real symbol, `SentinelSdkErrorCode` (`src/core/types.ts`). §12.15 maps each finding to its fix.
Every "today" claim cites a path and a symbol, verified against sentinel `origin/main` @ `dde0ea2` and web `origin/main` @
`69f8d865ec3b780b3b19c4378c3671bd0e97a3c4` on 2026-09-26. Line numbers are approximate (`~L`) and never the only identifier.

---

## 1. The problem

### 1.1 In the words of whoever has it

> Sentinel's issue page now has panels for user, request, contexts, breadcrumbs and tags […]. For browser and Next.js errors they
> are empty, because sdk-js sends none of the typed fields. — contract §1

Whoever triages a web error in Sentinel cannot see who hit it, on which page, or what the user did just before, although the
ingestor, the processor and the dashboard all store and show those fields since schema v2. The one route a browser event takes
to Sentinel, the Next.js tunnel, also deletes those fields.

### 1.2 What is true today (verified)

**sentinel (`packages/sdk-js` unless stated)**

| # | Fact | Evidence |
|---|---|---|
| T1 | The wire type has only the v1 keys: `event_id platform environment release_version error_class message stacktrace metadata timestamp fingerprint trace_id span_id trace_flags`. | `src/core/types.ts: SentinelEvent` (~L18-32) |
| T2 | `Level` is `'error' \| 'warning' \| 'info'`. Level reaches the wire only as `metadata.level`, only from `captureMessage`'s `ctx.level`; `captureException` ignores `ctx.level` with a debug line. `captureMessage(message, ctx?)` takes the level inside `ctx`, not as a positional argument. | `types.ts: Level` (~L34); `src/core/event.ts: readLevel`, `assembleMetadata` (~L182); `src/core/capture.ts: build` (~L252 of the file), `captureMessage` |
| T3 | The scope is `{ tags: Map<string, unknown>, user_id?, tenant_id? }` on the `globalThis[Symbol.for('@sentinel/sdk-js')]` slot. `setUser({ id })` writes `user_id`, `setTenant` writes `tenant_id`, `setTag(key, value)` writes a tag that `assembleMetadata` spreads as a **top-level metadata key**. `RESERVED_TAG_KEYS` blocks ten names. | `src/core/state.ts: Scope`, `getSlot`; `src/core/scope.ts: setUser`, `setTenant`, `setTag`, `RESERVED_TAG_KEYS`; `event.ts: assembleMetadata` |
| T4 | Metadata is assembled as `{ ...tags, user_id?, tenant_id?, ...ctx.metadata, runtime, source?, react_component_stack?, level? }`, sanitised, bounded to 65 536 bytes **by the Go validator's ruler** (`metadataWireBytes`: `<` `>` `&` count 6 bytes, U+2028/U+2029 count 6), shrunk largest-key-first except `SHRINK_EXEMPT_KEYS`, then PII-scrubbed (`[FILTERED]`, substring match on 15 keys, objects only). | `event.ts: assembleMetadata`, `shrinkMetadata`, `SHRINK_EXEMPT_KEYS`; `src/core/serialize.ts: metadataWireBytes`; `src/core/pii.ts: PII_KEYS`, `scrubPII` |
| T5 | `CaptureContext` is `{ metadata?, contexts?: { react?: { componentStack? } }, fingerprint?, traceparent?, level? }`. `contexts.react.componentStack` becomes `metadata.react_component_stack`. | `types.ts: CaptureContext`; `event.ts: assembleMetadata` |
| T6 | `beforeSend`'s result is normalised by `afterHook`: `event_id`, `platform`, `timestamp` and `metadata.runtime` are re-stamped, metadata is re-sanitised, bounded, shrunk and scrubbed; "everything else the hook set stands". | `capture.ts: afterHook`, `send` |
| T7 | Every runtime's worker packs batches of at most `MAX_BATCH_BYTES` = 61 440 (60 KiB) serialised bytes, body `[` + events joined by `,` + `]`. One event over that is sent **alone and without `keepalive`**; only the browser uses `keepalive`, and only for a batch that is not oversize. | `src/core/queue.ts: MAX_BATCH_BYTES`, `batchBytes`, `RingBuffer.takeBatch`; `src/core/transport/transport.ts` (~L401 `oversize`, ~L497 `keepalive`) |
| T8 | There are no breadcrumbs, no browser integrations other than the `error` / `unhandledrejection` handlers, and no automatic contexts. The README's *What is not implemented* lists breadcrumbs ("the wire has no field"). | `src/core/handlers/browser.ts`; `README.md` § *What is not implemented* (~L531-541) |
| T9 | The SDK sends no `sdk` field and no version constant exists in `src/`. `package.json` version is `0.1.2`. A public constant `SDK_NAME = '@sentinel/sdk-js'` exists (the slot key and an export); it is the **package** name and stays as it is — it is not the wire `sdk.name` of §3.3. | `grep -rn version packages/sdk-js/src` (only `release_version` and unrelated hits); `package.json`; `src/core/state.ts: SDK_NAME` |
| T10 | `captureRequestError` captures with `metadata.http_method`, `http_url` (Next's `errorRequest.path`, **query string included**), `route`, `router_kind`, `route_type`, `render_source`, `revalidate_reason`, and `ctx.traceparent` from the request headers. No other header is read. | `src/nextjs/request-error.ts: captureRequestErrorEvent` |
| T11 | The entries: `./browser`, `./nextjs/client` (re-exports browser) → runtime `browser`; `./node`, `./nextjs/server` (calls node's `init`) → `node`; `./edge`, `./nextjs/edge` (re-exports edge) → `edge`. `initCore(options, runtime, plan)` knows the runtime, not the entry. `./nextjs`'s types are the hand-written union `src/nextjs/index.d.ts`, which re-exports `setTag setTenant setUser …` by name. | `src/browser.ts`, `src/node.ts`, `src/edge.ts`, `src/nextjs/{client,server,edge}.ts: init`; `src/core/init.ts: initCore`; `src/nextjs/index.d.ts` |
| T12 | The tunnel's `ITEM_KEYS` is the v1 key set plus `fingerprint_override`; `normaliseBody` copies only those keys (exact `Set.has`), forces `platform`, and checks `metadata` is an object. `applyClientIp` runs on the normalised items after that, rebuilding `user` from `USER_KEYS = ['id','username','email']` (string values only) and writing `ip_address` last. Default body cap 262 144 bytes (`maxBodyBytes`), 50 items. | `src/nextjs/route.ts: ITEM_KEYS`, `normaliseBody`, `applyClientIp`, `USER_KEYS`, `DEFAULT_MAX_BODY_BYTES`, `DEFAULT_MAX_ITEMS` |
| T13 | `tests/unit/sdk_js_fixture_test.go` decodes every `packages/sdk-js/tests/fixtures/*.json` with `DisallowUnknownFields` into `validation.ErrorPayload`, then runs `ValidatePayload`, `MapPayloadToEvent` and protovalidate; `TestSDKJSFixtures_PinnedValues` pins five fixtures by file. `tests/unit/sdk_js_tunnel_fixture_test.go` replays `tests/tunnel-fixtures/*.json` through `clientip.Middleware` and `enrich.Apply` with a **plain** `json.NewDecoder(...).Decode` — **no** `DisallowUnknownFields` (see §1.3 D4). | `tests/unit/sdk_js_fixture_test.go: TestSDKJSFixtures_AcceptedByIngestor`, `TestSDKJSFixtures_PinnedValues`; `tests/unit/sdk_js_tunnel_fixture_test.go: tunnelReplay` (~L74) |
| T14 | `task test-unit` runs `go test ./tests/unit/...`, the ingestor's packages, the dashboard vitest, then `npx -y pnpm@10.34.5 --dir packages/sdk-js install --frozen-lockfile` and `… test`, and fails on tunnel-fixture drift (tracked or untracked). The sdk-js vitest config is `environment: 'node'`; browser suites opt in with a `// @vitest-environment jsdom` docblock (`jsdom` 28.1.0 is already an exact-pinned devDependency). No sdk-js test uses `skip`, `skipIf`, `runIf` or `todo`. | `Taskfile.yml: test-unit` (~L191-201); `packages/sdk-js/vitest.config.ts`; `tests/handlers/browser.test.ts` L1; `package.json: devDependencies`; `grep -rn "skipIf\|\.skip\|runIf\|todo(" packages/sdk-js/tests` → nothing |
| T15 | `pnpm size` is report-only (prints `sdk_js_client_entry_gzip_bytes` of `dist/nextjs/client.js`, exits 0); README records 11 198. | `scripts/size.mjs`; `README.md` § *Bundle size* |
| T16 | `sanitize` honours `toJSON` exactly as `JSON.stringify` does: an object with a `toJSON` method (a `Date`, a `URL`, a `dayjs` value, any class that defines one) is **replaced by that method's result**, so an object argument can come out as a string, an array or a number. A throwing `toJSON`, a throwing `Object.keys` (a hostile `Proxy`) or a top-level symbol comes out as the string `[unserializable]`. `afterHook`'s `isPlainRecord` is only `typeof v === 'object' && v !== null && !Array.isArray(v)` — a `Date` passes it. | `src/core/serialize.ts: sanitize`, `walk` (the `toJSON` branch, ~L59-76); `src/core/capture.ts: isPlainRecord` |
| T17 | `afterHook` today re-stamps `event_id`, `platform`, `timestamp`, `metadata.runtime`, rebuilds `metadata` and `stacktrace` (a frame's `line` / `column` kept when finite and > 0, with **no upper bound**), and runs `messageText` over `message` / `error_class`. It does not touch `environment`, `release_version`, `fingerprint`, `trace_id`, `span_id`, `trace_flags` or any other key, and it writes onto the object the hook returned (so a `toJSON` on that object is honoured by `enqueue`'s `sanitize`). | `src/core/capture.ts: afterHook`, `enqueue` |
| T18 | The sdk-js sources contain no regex lookbehind, no `\p{…}`, no `new RegExp` and no `replaceAll` today; `tsc` (target `ES2022`) does not transform regex literals. | `grep -rnE "\(\?<[=!]\|\\\\p\{\|new RegExp\|replaceAll" packages/sdk-js/src` → nothing; `packages/sdk-js/tsconfig.json` |

**sentinel (server side, read only — this feature changes none of it)**

| # | Fact | Evidence |
|---|---|---|
| S1 | `ErrorPayload`'s v2 JSON types: `level string`, `logger`, `server_name`, `transaction`, `dist`, `source` strings; `sdk {name, version}` strings; `user {id, username, email, ip_address, geo}` strings (+ `geo` object); `tags map[string]string`; `contexts map[string]map[string]interface{}`; `request {method, url, query_string string; headers map[string]string; data interface{}; env map[string]string}`; `breadcrumbs []{timestamp time.Time (RFC 3339 string), type, category, level, message string; data map[string]interface{}}`; `exception {values[]}`. A value of the wrong JSON type fails the decode, which 400s the whole request. Unknown keys are ignored in production. | `apps/ingestor-go/validation/validator.go: ErrorPayload`, `SDKInfo`, `UserInfo`, `RequestInfo`, `Breadcrumb` (~L87-140); `docs/sdk-specification.md` §3 |
| S2 | The bounds step (never a 400): `level` ∉ {`""`,`fatal`,`error`,`warning`,`info`,`debug`} → `""`; `sdk.name` ≤ 128, `sdk.version` ≤ 64 code points; `user.id` > 255 **bytes** → dropped, `email` > 254 → dropped, `ip_address` > 45 → dropped, `username` > 128 → truncated; tags: key `^[a-zA-Z0-9_.:-]{1,32}$`, value 1-200 code points without `\n`, else the entry is dropped, then the 50 lexically smallest keys kept; contexts ≤ 32 names of 1-64 code points, ≤ 65 536 bytes (each name's `jsonSize(name) + 1 + jsonSize(value)`, summed with the object's braces and commas, `objectSize`), other names removed before `device os app runtime browser culture`, largest first, a tie to the lexically smallest name; `request.url` ≤ 2048, `query_string` ≤ 4096, `headers` and `env` ≤ 64 entries each (key 1-256 code points, else the entry is dropped; a value over 4096 / 1024 code points is cut; past 64, the lexically largest keys are dropped, `boundStringMap`); U+0000 is stripped from context names and values and from header / env keys and values before they are measured; `request.data` over 16 384 bytes (`MaxRequestDataSize`) → dropped; breadcrumbs: last 100 kept, `type` ≤ 64, `category` ≤ 128, `message` ≤ 8192 code points (truncated), invalid `level` → `""`, `data` > 4096 bytes → that crumb's `data` dropped, total > 131 072 bytes → oldest dropped. Sizes are Go `json.Marshal` bytes of the **Go struct** (every `Breadcrumb` field is marshalled, empty or not — no `omitempty`). | `apps/ingestor-go/validation/bounds.go`: constants (~L47-91), `applyBounds`, `boundUser`, `boundTags` (~L359), `boundContexts`, `boundRequest`, `boundBreadcrumbs` (~L522), `jsonSize`, `arraySize` |
| S3 | Request body caps: 1 MiB single, 5 MiB batch. These bound the **JSON**; the binding size limit is the NATS publish (S11). | `apps/ingestor-go/main.go: maxEventBodyBytes`, `maxBatchBodyBytes` |
| S4 | `{{auto}}` in `user.ip_address` is replaced with the caller's IP when the resolver has one, else cleared (I1/I2). A `javascript` item whose `metadata.runtime` is `"browser"` never gets a connection-derived IP otherwise (I3t). The ingestor's UA parse fills `contexts.browser` and `contexts.os` (`contexts.client_os` instead of `os` for a server-platform event) **only for a name the event does not already carry**, reading the User-Agent from `request.headers` first (case-insensitive `User-Agent`), then the ingest request's own for client platforms. Its names are uap-core's (`Chrome`, `Mac OS X`, …) with Relay's frozen-version rule. | `apps/ingestor-go/enrich/enrich.go: applyIP`, `isTunnelOnly`, `applyUA`, `userAgent`, `insertContext`, `frozenOSVersion` |
| S5 | `metadata.runtime` decides client vs server platform for enrichment. | `enrich.go: IsClientPlatform`, `isTunnelOnly` (~L136-152) |
| S6 | The processor's affected-user id is `user.id` when non-empty, else `metadata.user_id` (by `indexer.UserIDFromMetadata`); the affected tenant is `tags["tenant_id"]` when the key is present, else the metadata fallback. It masks `request.url`, `query_string`, headers, breadcrumb messages and data, contexts and tag values; `user` and `sdk` are stored as sent. | `apps/processor-go/event/event.go: readAffectedIDs` (~L258-271), `maskV2`; `apps/processor-go/service/processor_service.go` (~L516-528) |
| S7 | Nothing in `apps/` reads `metadata.level`, `metadata.http_method` or `metadata.http_url`. | `grep -rnE "metadata\.level\|Metadata\[\"level\"\]\|http_url\|http_method" apps/` → no hit outside test files |
| S8 | The dashboard's breadcrumb timeline renders `category`, `type`, `level`, `message` and `data` generically, oldest first. | `apps/dashboard-web/src/lib/components/issues/BreadcrumbTimeline.svelte` |
| S9 | The processor drops a frame path's `?…` and `#…` suffix before the sourcemap lookup, the stored `minified_path` join and the fingerprint hash input, so a frame `file`'s query string changes neither symbolication nor grouping. | `apps/processor-go/symbolicate/normalize.go: NormalizePath` (rule (b)) |
| S10 | Besides the JSON types of S1, protovalidate rejects (400) an `environment` not matching `^[a-z0-9][a-z0-9._-]{0,49}$` (and it is `required`), `release_version` > 100, `fingerprint` > 64, `trace_id` / `span_id` > 64, `logger` / `dist` / `source` > 64 code points; `trace_flags` is `uint32` and a frame's `line` / `column` `int32`, so a negative, fractional or too-large number fails the decode. Go's decoder matches keys **case-insensitively**, so `LEVEL` or `Logger` is decoded (and type-checked) as `level` / `logger`. | `packages/proto/sentinel/v1/error_event.proto` (the `error_event.*` CEL rules); `validator.go: ErrorPayload`, `StackFrame` |
| S11 | The ingestor publishes each accepted item to NATS as `proto.Marshal` of the mapped `ErrorEvent` (`IngestAt` → `publish` → `PublishWithHeaders`, with the trace headers the global propagator injects). A publish error fails that item: the batch handler counts it in `failed`, and a batch in which no item was ingested answers 400. The SDK's transport treats a 400 whose errors do not start with `validation failed` as transient, so a lone oversize item is resent unchanged until its attempts run out and then reported `batch_dropped`. In a batch where another item landed (202), it is reported `item_rejected` and not retried. Either way the event is lost. Neither compose file sets `max_payload`, so the NATS server default, **1 048 576 bytes** (headers + payload), applies. `metadata`, each `contexts` value and `Breadcrumb.data` are `google.protobuf.Struct`, and `Request.data` is a `google.protobuf.Value`. `MapPayloadToEvent` builds them with `structpb.NewStruct` / `NewValue`, so every JSON number becomes a `double` (9 bytes plus framing), and a `null` or `false` is still written (a `Value` always writes its oneof). `0,` is 2 JSON bytes and 11 proto bytes, so S2's JSON bounds leave up to ~5.5× their figure in proto. | `apps/ingestor-go/service/service.go: IngestAt`, `publish`; `apps/ingestor-go/main.go` (the batch handler: `result.Failed`, `status = http.StatusBadRequest` when `result.Ingested == 0`); `packages/sdk-js/src/core/transport/transport.ts` (the `status === 400` branch, `item_rejected`); `docker-compose.yml`, `docker-compose.prod.yml` (`nats` service, no `max_payload`); `packages/proto/sentinel/v1/error_event.proto: ErrorEvent.metadata`, `contexts`, `Request.data`, `Breadcrumb.data`; `apps/ingestor-go/mapping/mapper.go: MapPayloadToEvent`, `mapContexts` |

**web (`origin/main` @ `69f8d865e`)**

| # | Fact | Evidence |
|---|---|---|
| W1 | `"@sentinel/sdk-js": "git+https://github.com/NurfitraPujo/sentinel.git#packages/sdk-js/v0.1.2&path:packages/sdk-js"`; `pnpm-lock.yaml` records the tag in `specifier` and the commit in `version` / `resolution.commit` (`096157a…`). `packageManager` is `pnpm@10.13.1`. `@sentry/nextjs` `^10.69.0` stays for replay. | `package.json` (~L81); `pnpm-lock.yaml` importers (~L82-84), packages (~L2193-2205) |
| W2 | Four init files at the repo root: `instrumentation-client.ts` (`@sentinel/sdk-js/nextjs/client`, `tunnel: '/api/sentinel'`, `beforeSend` = the 401/403 filter on `hint.originalException` then the `metadata.sentry_replay_id` copy), `sentinel.server.config.ts` (`/nextjs/server`) and `sentinel.edge.config.ts` (`/nextjs/edge`), each with its own copy of the 401/403 filter, and `instrumentation.ts` (`onRequestError = Sentinel.captureRequestError`). **No `beforeBreadcrumb` exists anywhere.** `Sentinel.init` runs before `Sentry.init` in `instrumentation-client.ts`. | the four files; `git grep -n beforeBreadcrumb origin/main` → nothing |
| W3 | `src/layouts/dashboard/shell/index.tsx` (`import * as Sentinel from '@sentinel/sdk-js/nextjs'`), in a `useDidMount` on `[me]`: `if (!impersonation) { Sentinel.setUser({ id: me.id }); Sentinel.setTenant(me.companyId); }`. | `shell/index.tsx` (~L281-296) |
| W4 | `me` is `UserProfile` = `User & { company: { name, timezone, notification }, scope, … }`. `User` has `id`, `companyId`, `email`, `name`, `phoneNumber`, `isEmailPrivate?`. **`company` has no `id`.** | `src/features/auths/interfaces.ts: UserProfile`; `src/features/users/interfaces.ts: User` |
| W5 | Logout: `endSession` calls `Sentinel.setUser(null)` and `Sentinel.setTenant(null)` (browser only), then navigates to `/sign-in`. | `src/common/utils/credential.ts: endSession` (~L19-29) |
| W6 | The tunnel is `src/app/api/sentinel/route.ts`: `createSentinelRouteHandler({ apiKey: SENTINEL_TUNNEL_API_KEY, isCrossSite: isCrossSiteRequest, clientKey: getClientIp })`, `runtime = 'nodejs'`, default `maxBodyBytes`. | that file |
| W7 | web's vitest: one `vitest.config.ts`, `environment: 'node'`, `*.test.tsx` on happy-dom, `restoreMocks`/`clearMocks`. `src/__tests__/instrumentation.test.ts` runs on node and replaces every `@sentinel/sdk-js/*` subpath with a `vi.mock` factory, reading each init's options from `init.mock.calls[0][0]`. The SDK is also mocked in `shell/__tests__/index.test.tsx` (`setUser`, `setTenant`), `src/common/utils/__tests__/credential.test.ts` and `credential.server.test.ts`, `src/app/__tests__/layout-shells.test.tsx` (`captureException`, `setUser`, `setTenant`), `report-boundary-error.test.ts`, `stale-deploy.test.ts`. | `vitest.config.ts`; those tests |
| W8 | Known URL carriers (all query parameters; no token rides in a path): `/reset-password?reset_token=` and `/accept-invitation?invite_token=` (read by `searchParams.get`, **never removed from the address bar**); six OAuth callback pages `/email-gmail-callback`, `/email-outlook-callback`, `/google-callback`, `/lark-callback`, `/microsoft-callback`, `/zoom-callback` reading `code` / `state` (and `scope`, `error` on google; `error` on lark), four of which never navigate away; `/api/proxy?u=<encodeURIComponent(file.url)>` fetched by `downloadFilesAsZip`; emails' `search=` (free text) in the page URL via `useSyncUrlFilter`; databases' `q=` (free text) and `f=` (base64url JSON filter values); timelines' `cf=` and `f=` (base64url JSON carrying internal users' ids **and names**). Request-only carriers: `search=` on axios `params` and the `/api/sse/record-ids` EventSource. | `src/features/reset-password/form/index.tsx` (~L52); `src/features/accept-invitation/form/index.tsx` (~L23); `src/features/<callback>/index.tsx`; `src/features/media-and-files/utils.ts` (~L24); `src/common/hooks/use-sync-url-filter.ts`, `src/features/emails/list/index.tsx` (~L335); `src/features/databases/database/constants.ts`, `use-database-url-sync.ts`; `src/features/timelines/conversations/index.tsx`, `timelines/list/index.tsx`, `timelines/utils.ts` |
| W9 | web is App Router only (no `pages/`); navigation is `next/navigation` `router.push` / `router.replace` (which call `history.pushState` / `replaceState`), plus direct `window.history.pushState` (same URL, `use-back-guard.ts`) and `replaceState` ×4 (`integrations/index.tsx`). | `src/app/`; `src/features/databases/database/draft-exit-guard/use-back-guard.ts`; `src/features/integrations/index.tsx` |
| W10 | web calls no `captureMessage`, `setTag`, `setTags`, `setContext`, `setExtra` or `addBreadcrumb`. | `git grep` over `origin/main` |
| W11 | web is on `next` `15.5.26` and declares no `browserslist` (no key in `package.json`, no `.browserslistrc`), so its client bundle targets Next's default list, which includes **Safari 12**. Safari supports regex lookbehind only from 16.4, so a lookbehind **literal** anywhere in the client bundle is a parse-time `SyntaxError` for the whole chunk on iOS ≤ 16.3, and a lookbehind built with `new RegExp` throws when it runs. No transpiler lowers regex syntax. | `package.json` (`"next": "15.5.26"`, no `browserslist`); `git ls-tree origin/main` (no `.browserslistrc`); `next/dist/shared/lib/modern-browserslist-target.js` in web's installed `next@15.5.26` (`'safari 12'` in the list) |
| W12 | `src/app/api/sentinel/__tests__/route.test.ts` is the **only** web test that runs the real `@sentinel/sdk-js/nextjs/route` (no `vi.mock` of the SDK; every other SDK-importing test mocks it). Its "forwards to {endpoint}/batch with the tunnel key only …" case pins v0.1.2's forward: `init.headers` `toEqual({ 'content-type': 'application/json', 'x-api-key': TUNNEL_KEY })` and the body `toEqual([{ message: 'boom', error_class: 'Error', platform: 'javascript' }])`, for a request carrying `x-forwarded-for: 203.0.113.10, 10.0.0.1` with `TRUSTED_PROXY_HOPS=1` stubbed. `packages/sdk-js/v0.1.2` predates sentinel `f49721f` (sentinel-event-enrichment), which added the `x-sentinel-derive-ip: off` forward header and `applyClientIp`; 0.2.0 ships both. `getClientIp` (`src/app/api/client-ip.ts`) returns the entry `hops` from the right of `X-Forwarded-For` when `isIP` accepts it. | that test; `src/app/api/client-ip.ts: getClientIp`; sentinel `packages/sdk-js/src/nextjs/route.ts: DERIVE_IP_HEADER`, `applyClientIp`; `git merge-base --is-ancestor packages/sdk-js/v0.1.2 f49721f` |

### 1.3 Where the contract does not match the code, or would make the data worse (Deviations)

| # | Contract says | What is true | What this spec does |
|---|---|---|---|
| D1 | The brief: write the spec "the same layout as `sentinel-issue-tag-facets/spec.md` on main". | That file does not exist on `main`; `sentinel-issue-tag-facets/` holds only `plan.md`. | Uses the layout of `sentinel-event-enrichment/spec.md` and `sentinel-occurrence-detail-panels/spec.md`, which do exist. |
| D2 | §4.3: browser `contexts.browser` / `contexts.os` "from `navigator.userAgentData` where available, else left for the ingestor's UA parse". | (a) Low-entropy UA-CH (`brands`, `platform`, `mobile`) carries only the **major** version, and names the families differently from the ingestor's uap-core parse (`Google Chrome` vs `Chrome`, `macOS` vs `Mac OS X`). (b) The ingestor fills a context only when the event lacks it (S4), so an SDK-sent `contexts.browser` **blocks** the server's parse. Following the contract would store Chromium browsers under a different name and a poorer version than Firefox and Safari, splitting one browser into two names across events. The high-entropy values are async, which a synchronous capture cannot wait for. | The browser SDK sends **no** `contexts.browser` / `contexts.os`. It sends `request.headers["User-Agent"] = navigator.userAgent`, which the ingestor's `applyUA` reads first (S4), so every browser event — Chromium or not — gets both contexts from one parser. The contract's measure ("shows `contexts.browser` / `contexts.os`") holds; TASK-04's proof asserts the header on the wire and the tunnel fixture replay asserts the ingestor-filled contexts (§11.2). |
| D3 | §4.6: `setContext('tenant', { id, name })` "from `me.company`". | `me.company` has no `id` (W4). | `setContext('tenant', { id: me.companyId, name: me.company.name })`. |
| D4 | §4.5: the tunnel fixtures are "replayed by `tests/unit/sdk_js_tunnel_fixture_test.go` with `DisallowUnknownFields` against `ErrorPayload`". | The tunnel replay decodes with a plain decoder (T13). Only `sdk_js_fixture_test.go` is strict. | A new strict test, `TestTunnelFixtureV2StrictDecode`, decodes every forwarded body with `DisallowUnknownFields` (§7.3). |
| D5 | §4.1: "`captureMessage` takes its level argument". | Today the level travels in `ctx.level` (T2); there is no positional argument. | `captureMessage(message, levelOrContext?)` accepts both: a `Level` string, or a `CaptureContext` whose `level` is read (§3.2). Existing `{ level }` callers keep working. |
| D6 | §4.3: `captureRequestError` headers "minus `authorization`, `cookie`, `set-cookie`, `proxy-authorization`, `x-api-key`". §4.4 lists the URLs the SDK filters; §4.6 web strips `request.url` and `request.query_string`. | Next's request headers include `referer`, a full URL whose query string can carry web's tokens (a request made from `/reset-password?reset_token=…`). Neither the contract's header list nor its URL list covers it. | The SDK runs `referer` — and every other header value containing `?` or `#`, since proxies add URL-bearing headers the SDK cannot enumerate (Envoy's `x-envoy-original-path`; web's ingress is emissary) — through the URL filter (§5.1), and web's `beforeSend` strips the query from every header value (§9.4). The browser SDK sends **no** `Referer` at all (`document.referrer` is the previous page's URL, e.g. the reset link). |

| D7 | §4.3: node / edge `contexts.runtime { name, version }`. | The edge runtime exposes no version (no `process.versions`; Next sets only the global `EdgeRuntime` string). | Edge sends `contexts.runtime { name }` without `version` (§6.3); node sends both. |

None of these changes the contract's intent, its risk surfaces or any decision; D2 and D6 are on the conservative side of decision 5.

---

## 2. What this change does, and does not do

**Does (side `be`, `packages/sdk-js`):**

1. Sends the typed v2 fields `level`, `sdk`, `user`, `tags`, `contexts`, `request`, `breadcrumbs`, and on server runtimes
   `transaction` and `server_name`, with the exact JSON types of S1 and client-side bounds equal to S2's where the contract asks
   (§3), and S2's own contexts and headers / env bounds applied before sending (§3.10).
2. Moves identity and tags out of `metadata`: `setUser` → `user`, `setTenant` → `tags.tenant_id`, `setTag` / `setTags` → `tags`;
   adds `setContext`, `setExtra` / `setExtras`. `metadata.runtime` stays; `metadata.level` goes (§3.7). The metadata shrink
   removes the server's exempt keys last, as the server does, instead of dropping the event with `metadata_too_large` (§3.7).
3. Keeps a bounded breadcrumb buffer with `addBreadcrumb`, `beforeBreadcrumb` and `maxBreadcrumbs`, attached to every event, and
   trims it so a browser event fits the 60 KiB keepalive body whenever the event without breadcrumbs does (§4). On every
   runtime, sheds breadcrumbs, then contexts, then metadata from an event whose estimated protobuf size would not fit the
   ingestor's NATS publish (§4.6).
4. Records console, fetch, XHR, navigation and click breadcrumbs in the browser, each switchable, with no bodies, no fetch/XHR
   headers, no input values, and sensitive query values `[Filtered]` (§5).
5. Fills runtime contexts per entry, and `request` / `transaction` for Next's `captureRequestError` (§6).
6. Widens the tunnel's `ITEM_KEYS` by exactly the seven keys a browser event now carries; the filter stays exact-key and
   `applyClientIp` still runs after it, now deleting a rebuilt `user` that has no key (§7).
7. Releases 0.2.0 (§8).

**Does (side `fe`, `web`):** pins the rc tag, then `v0.2.0`; sends `user { id, email, username }`, `tags.tenant_id` and
`contexts.tenant`; strips the whole query string from every recorded URL in `beforeBreadcrumb` and `beforeSend` in all three init
files; keeps the 401/403 filter and the replay-id copy (§9).

**Does not:**

- change the ingestor, processor, dashboard, proto or any migration (contract §2). That the ingestor accepts over HTTP an event
  it cannot publish to NATS is **reported upward** (§13 F4), and §4.6 keeps this SDK's events clear of it;
- add tracing, spans, session replay, per-request server scope (`AsyncLocalStorage`), mobile, `withSentinelErrorBoundary` or
  index-map `url` sections (contract §2);
- send `exception` (the chained-exception interface), `logger`, `dist` or top-level `source`; the SDK has no producer for them in
  this feature, the tunnel keeps dropping them (§7.1), and `afterHook` deletes them when a `beforeSend` adds them (§3.8);
- add per-call `contexts` or `extra` on `CaptureContext`; the per-call typed keys are exactly `level`, `user` and `tags` (§3.7).
  The README's per-request workaround moves from `metadata: { user_id }` to `user: { id }` on the capture call;
- record automatic breadcrumbs on node or edge;
- change the tunnel's body limit, item limit, forward headers, rate limit or IP derivation (§7.2);
- migrate stored events; a 0.1.x app keeps sending metadata-only events, which the server still accepts (contract *Compatibility*).

**Measurement:** no measurement acceptance criterion is owed for a read/write path — the feature changes what one client puts in
an event, not any server path, index or query. The one metric this package already defines, `sdk_js_client_entry_gzip_bytes`, is
re-measured and recorded in the README by TASK-05 and stays report-only (README *Bundle size*, T15); no threshold is introduced.
**Infrastructure:** no infra/alerting changes owed — no new service, queue, bucket, job or endpoint; the tunnel is an existing
route whose limits do not change.

---

## 3. Typed fields (TASK-01)

### 3.1 The wire, field by field

Every value below is emitted with exactly S1's JSON type. A key with no usable value is **omitted**, never sent as `null`, `""` or
`{}`.

| Field | JSON type | Source | SDK-side rule (this spec) | Server rule it matches (S2) |
|---|---|---|---|---|
| `level` | string | §3.2 | always one of `fatal error warning info debug` | invalid → `""` |
| `sdk` | `{name: string, version: string}` | §3.3 | always sent, re-stamped after `beforeSend` | name ≤ 128, version ≤ 64 |
| `user` | object of strings | `setUser`, per-call `ctx.user`, §3.4, §3.7 | only `id username email ip_address`; bounds as S2 | same |
| `tags` | object, string → string | `setTag`, `setTags`, `setTenant`, per-call `ctx.tags`, §3.5, §3.7 | invalid entries dropped at write time; over 50 per event, `tenant_id` kept and the lexically largest others dropped (§3.5 rule 6) | same key/value rule; the 50 lexically smallest kept |
| `contexts` | object, string → object | `setContext` + automatic (§6), §3.6 | names 1-64 code points, values JSON objects **after** `sanitize` (§3.6) | same |
| `request` | object | automatic (§6), or a `beforeSend` (§3.8) | `method`, `url`, `query_string` strings, U+0000 removed, cut to 16 / 2048 / 4096 code points (§3.10); `headers` / `env` string → string, bounded by §3.10; `data` ≤ 16 384 bytes (§3.8) | same |
| `breadcrumbs` | array of objects | §4, §5 | §4.2 | §4.4 |
| `transaction` | string | Next `captureRequestError` only (§6.4) | ≤ 200 code points, cut | truncated at 200 |
| `server_name` | string | node only (§6.2) | ≤ 256 code points, cut | truncated at 256 |
| `metadata` | object | `setExtra`, `setExtras`, `ctx.metadata`, SDK keys, §3.7 | today's rules (T4) | today's rules |

`SentinelEvent` (`types.ts`) gains the optional keys `level`, `sdk`, `user`, `tags`, `contexts`, `request`, `breadcrumbs`,
`transaction`, `server_name`, typed as above (`level` required once built; the others optional). New exported types:
`User`, `Breadcrumb`, `BreadcrumbHint`, `BeforeBreadcrumb`, `RequestInfo`, `SdkInfo`. They are exported from `.` and every
runtime entry, and re-exported by name from `src/nextjs/index.d.ts` (T11) — with the new functions `setTags`, `setContext`,
`setExtra`, `setExtras`, `addBreadcrumb` — so `import * as Sentinel from '@sentinel/sdk-js/nextjs'` (W3) type-checks.

### 3.2 `level`

- `Level` becomes `'fatal' | 'error' | 'warning' | 'info' | 'debug'`.
- `captureException(err, ctx?)`: `ctx.level` when it is one of the five, else `'error'`. The 0.1.x debug line "ctx.level is
  ignored by captureException" is removed (the default is what the contract fixes; an explicit level is honoured, as in Sentry).
- `captureMessage(message, levelOrContext?)`: a string argument is the level; an object argument is a `CaptureContext` whose
  `level` is read. An absent or invalid level is `'info'` (Sentry's default). (D5)
- Global handlers: node's `uncaughtException` capture is `'fatal'` (Sentry's `onUncaughtExceptionIntegration`); node's
  `unhandledRejection`, the browser's `error` and `unhandledrejection` captures are `'error'`. The browser handler's
  `captureMessage`-shaped capture (`handlers/browser.ts`, a resource/`ErrorEvent` without `.error`) keeps `ctx.level: 'error'`.
- The level goes to top-level `level` only; `metadata.level` is no longer written (S7: nothing reads it). `level` stays in
  `SDK_OWNED_METADATA_KEYS`, so a per-call `ctx.metadata.level` is still dropped with a debug line.
- After `beforeSend`: an invalid `level` is replaced by the pre-hook level with a debug line.

### 3.3 `sdk`

`sdk = { name: 'sentinel.javascript.<entry>', version: SDK_VERSION }`, re-stamped after `beforeSend` like `event_id` (T6).

| The app called `init` from | `<entry>` |
|---|---|
| `@sentinel/sdk-js/browser` | `browser` |
| `@sentinel/sdk-js/node` | `node` |
| `@sentinel/sdk-js/edge` | `edge` |
| `@sentinel/sdk-js/nextjs/client`, `/nextjs/server`, `/nextjs/edge` | `nextjs` (Sentry names all three Next runtimes `sentry.javascript.nextjs`; `metadata.runtime` still says which) |
| no entry (`initCore` directly, the test helpers) | the runtime: `browser`, `node` or `edge` |

The entry is passed to `initCore` as a new fourth parameter and kept on `InternalClient.sdkName`. Each runtime entry's public
`init` becomes a thin call to an internal `initBrowser(options, entry)` / `initNode(options, entry)` / `initEdge(options, entry)`.
`nextjs/server.ts` already has its own `init` and calls `initNode(options, 'nextjs')`. `nextjs/client.ts` and `nextjs/edge.ts`
today re-export `init` through `export * from '../browser.js'` / `'../edge.js'` (T11); each gains its **own** exported `init`,
which shadows the star export (an explicit export wins over `export *`), calling `initBrowser(options, 'nextjs')` — which also
installs the handlers and the §5 integrations — and `initEdge(options, 'nextjs')`. `SDK_VERSION` is a string literal in
a new `src/core/version.ts`; `tests/package.test.ts` asserts it equals `package.json`'s `version` (a literal, not a build-time
define, so the vitest suite sees the same value as `dist`).

### 3.4 `user` and the `{{auto}}` IP (`autoIpAddress`)

`setUser(user: User | null)`, `User = { id?: string | number; username?: string; email?: string; ip_address?: string }`.

- The call **replaces** the scope's user (Sentry semantics). `null`, `undefined`, or an object with no usable key after the rules
  below, clears it.
- `id`: `coerceIdentity` (today's rule: non-empty string stands; a finite number or boolean is `String(v)`; anything else is
  unusable). Over **255 UTF-8 bytes** → **omitted** (not truncated: a truncated id is a different user; the server drops it by the
  same rule, S2), with a debug line.
- `email`: non-empty string, ≤ 254 code points, else omitted (debug). `ip_address`: non-empty string, ≤ 45 code points, else
  omitted (debug). `username`: non-empty string, cut to 128 code points (debug when cut). Any other key is ignored with one debug
  line naming the keys (the ingestor has no field for them; a strict decode would fail).
- The event's `user` is a copy of the scope's user.

**`autoIpAddress`** (new option; `boolean`; **default `true`**; on `BrowserInitOptions` only — `./browser` and
`./nextjs/client`; a non-boolean degrades to the default with one `invalid_option`): when true, a browser event whose `user` has no
`ip_address` is sent with `user.ip_address = "{{auto}}"`, creating `user = { ip_address: "{{auto}}" }` when no user is set. When
false, the SDK never adds it.

What the option does and does not decide, stated in the README:

- Behind the Sentinel tunnel the SDK's value never reaches the ingestor: the tunnel rebuilds `user` without any `ip_address` and
  writes the browser's derived IP itself (T12, contract §4.5). The **app owner's** switch there is the tunnel's
  `trustedProxyHops` (`0` → no IP under Next). A browser cannot choose, and cannot suppress, its stored IP.
- A browser event that reaches the ingestor through **another** same-origin proxy is filled by the ingestor from that proxy's
  connection only because of `{{auto}}` (I1 runs before the I3t browser guard, S4) — which stores **the proxy's egress IP**, not
  the browser's, as `user.ip_address`. Without `{{auto}}`, I3t keeps it empty. An app behind its own proxy should set
  `autoIpAddress: false` unless that proxy is its users' address. The README says this in these words. (The default stays `true`:
  contract §4.1.)
- node and edge never send `{{auto}}`: a server platform's connection is a pod (S5), and the option does not exist on their types.

### 3.5 `tags`, `setTag`, `setTags`, `setTenant`

`setTag(key, value: string | number | boolean | null)`, `setTags(tags: Record<string, string | number | boolean | null>)` (each
entry as `setTag`), `setTenant(id: string | number | null)`.

At write time, into `Scope.tags: Map<string, string>`:

1. `value === null` (or `undefined`) deletes the key.
2. A number (finite) or boolean is `String(value)`; any other non-string (object, array, `NaN`, `±Infinity`, symbol, bigint,
   function) is dropped with a debug line.
3. The key must match `^[a-zA-Z0-9_.:-]{1,32}$` (the ingestor's `tagKeyPattern`, S2); the value must be non-empty, ≤ 200 code
   points, and contain no `\n`. Otherwise the entry is **dropped client-side with a debug line** naming the key and the rule, and a
   previous value under that key is deleted too (so the event never carries a stale value the app tried to replace). Values are
   not truncated (the server drops, it does not cut).
4. `tenant_id` is reserved for `setTenant`: `setTag('tenant_id', …)` / `setTags({ tenant_id })` is dropped with a debug line
   ("use setTenant"). `RESERVED_TAG_KEYS`' other nine names are **freed** — they reserved metadata keys, and tags no longer live in
   metadata.
5. `setTenant(id)`: `coerceIdentity(id)`, then rule 3 → `tags.tenant_id`; unusable or `null` deletes it (contract decision 7).
6. **The 50-tag cap, `tenant_id` kept.** The server keeps the 50 lexically smallest keys (S2). `tenant_id` sorts after every key
   that starts with a digit, an upper-case letter, `-`, `.`, `:`, `_` or `a`-`s`, so the server's cut can drop it. It is then the only thing lost that
   matters: the processor's affected tenant is `tags["tenant_id"]` (S6), and metadata no longer carries a fallback (§3.7). So
   when an event's **effective** tags (the scope's, merged with `ctx.tags` by §3.7, and again after `beforeSend` by §3.8) number
   more than 50, the SDK keeps 50: `tenant_id` when present, plus the lexically smallest other keys (JS `<` on these ASCII keys
   orders them as Go's `sort.Strings` does). It drops the lexically largest others, writes one debug line naming the dropped
   keys, and adds `'tags'` to `metadata.truncated` before that event's metadata shrink runs, so the shrink measures it. Without
   `tenant_id`, the SDK keeps exactly the 50 keys the server would, and the event then reaches the server with nothing left for
   it to cut. The scope's map is not changed; the cap applies per event.

The event's `tags` is a copy of the scope's map as a fresh object, filled by `forEach` (not `Object.fromEntries`, §5.1
*Built-in API floor*), omitted when empty. Debug lines need a client; before the first
`init` a dropped entry is dropped silently (today's `debug` is a no-op without a client, `src/core/report.ts: debug`).

### 3.6 `setContext`

`setContext(name: string, context: Record<string, unknown> | null)`, into `Scope.contexts: Map<string, object>`:

- `null` / `undefined` deletes the name.
- The name must be 1-64 code points, else the call is dropped with a debug line. The value must be a non-null, non-array object,
  else dropped with a debug line.
- The value is copied through the safe serialiser (`sanitize`, `serialize.ts`) at write time, so a later mutation of the caller's
  object does not change the scope, and a cycle / `bigint` / function becomes today's `[unserializable]` marker instead of a throw.
- **The object check is applied to `sanitize`'s output, not only to its input.** `sanitize` replaces an object that has a
  `toJSON` method by that method's result (T16), so `setContext('x', new Date())`, `setContext('x', new URL(u))`, a `dayjs()`
  value, or `{ toJSON: () => [1, 2] }` come out as a string or an array — where `ErrorPayload.contexts` is
  `map[string]map[string]interface{}` and a non-object value 400s the whole batch (S1). The rule, used everywhere this spec
  says "a JSON object": `isJsonObject(v) = typeof v === 'object' && v !== null && !Array.isArray(v)`, evaluated on the
  **sanitised** value. When it fails, the call is dropped with a debug line naming the name and what the value serialised to
  (`"string"`, `"array"`), and the scope is unchanged (as for any other rejected `setContext` call). A `toJSON` that returns a
  plain object is accepted — the stored context is that object.
- Contexts are **not** PII-scrubbed by the SDK (they are values the app chose to send; the processor masks them key by key, S6).
- Merge with automatic contexts (§6): for each name, `{ ...automatic[name], ...setContext[name] }` — key by key, **user-set keys
  win**; a name only one side has is taken as is. A context the event already carries is never overwritten by an automatic one
  (contract §4.3).
- The SDK applies the server's own contexts bound (32 names, 65 536 Go-ruler bytes, S2) to every event, at `build` and again in
  `afterHook` (§3.10), so the server's cut is a no-op on what the SDK sends. §4.5's last-resort rule and §4.6's publish bound
  can remove more.

### 3.7 `metadata`: extras, and what leaves it

`setExtra(key: string, value: unknown)` and `setExtras(extras: Record<string, unknown>)` (Sentry's names, contract §4.1) write
`Scope.extras: Map<string, unknown>`; `null`/`undefined` deletes. The keys `runtime source level truncated truncated_keys`
(`SDK_OWNED_METADATA_KEYS`) and `user_id tenant_id` are dropped with a debug line (use `setUser` / `setTenant`).

Metadata is assembled as
`{ ...extras, ...ctx.metadata, runtime, source?, react_component_stack? }` — per-call `ctx.metadata` wins over scope extras; the
per-call rules for `ctx.metadata` are unchanged except that `user_id` and `tenant_id` join the dropped keys (debug line: "use
ctx.user / ctx.tags"), then sanitised, bounded, shrunk and scrubbed as today, except for the shrink's exempt set and its end
(below).

**The shrink never drops an event** (spec review round 10, coordinator ruling). 0.1.x's `shrinkMetadata` never removes
`SHRINK_EXEMPT_KEYS` (`user_id tenant_id runtime route truncated truncated_keys`). It returns `false` when only those are left
and `metadata` is still over 65 536, and `boundAndShrink` (`src/core/capture.ts`) then drops the event with
`metadata_too_large` (T4). The ingestor never drops for this. `bounds.go: shrinkMetadata` removes every key outside
`metadataExemptKeys` first, then the exempt ones, the largest first, with a tie going to the lexically smallest key
(`sortVictims`). So an event carrying `setExtra('route', 'x'.repeat(70_000))` was one the server would publish, and the SDK
lost it. `SHRINK_EXEMPT_KEYS` is replaced by `METADATA_EXEMPT_KEYS` (`src/core/event.ts`), the server's 18 names copied from
`bounds.go: metadataExemptKeys`: `user_id userId user tenant_id tenantId organization_id trace_id traceId trace-id span_id
spanId span-id request_id requestId request-id release_version truncated truncated_keys`. `route` is not among them, because
the server never protected it. `user_id` / `tenant_id` are protected only because the server protects them.

**`truncated_keys` is written, and measured, in the server's capped shape** (`bounds.go: removedKeys.add`, `shrinkMetadata`;
spec review round 11). The shrink keeps the full list internally, for 0.1.x's replacement of a removed key's sub-paths by its bare name, and `writeMarkers` writes
it as the server's `removedKeys.value()` does: its first 50 entries, each with U+0000 removed and cut to 128 UTF-8 bytes on a
code-point boundary, then one `"+N more"` entry counting the rest, where an earlier `"+K more"` entry counts as K. The shrink
writes the marker this way **before every measurement, including those of steps 1 and 2**, exactly as the server's `size()`
measures `removedKeys.jsonSize()` per removal. A full list is never what is measured, so the names of removed keys can never
force another key out, and an exempt key is never removed to make room for the marker. The capped list is under 40 KB even
written in 6-byte escapes. While `metadata` is over, the shrink removes:

1. **every other key except `runtime`**, the largest first, the earlier-inserted key winning a tie (today's loop, over the new
   set);
2. **then the exempt keys** other than `truncated` / `truncated_keys`, the largest first, a tie to the lexically smallest key
   (the server's order for its removed-last group).

Every key removed in 1 and 2 is recorded in `truncated_keys`, as today. Once 1 and 2 have removed everything else, what is left is
`runtime` and the two markers, the capped `truncated_keys` among them, and that fits, so the loop always ends. `runtime` is never
removed. The ingestor's enrichment reads it before its own shrink runs (S5), so an SDK that removed it would change what
enrichment does, where the server's removal comes after enrichment. It is one short string, so keeping it can never leave
`metadata` over, and the server's shrink stays a no-op on what the SDK sends. Measured this way, the SDK's
marker costs no more than what the server would add for the same removals (`server_truncated: ['metadata', …]` and a capped
`server_truncated_keys`, beside the SDK's own markers, which the server measures as exempt keys), and both remove the largest
first, so the SDK never removes more than the server's shrink would. For `setExtra('request_id', 'r1')` and 3 000 extras with
60-character keys, both keep `request_id` and the same 958 extras (§11.2 TASK-02 case (10), *The cap*). The shrink no longer returns a result, and
`boundAndShrink` never drops. Nothing reports `metadata_too_large`, but the code stays in the `SentinelSdkErrorCode` union
(`src/core/types.ts`), so an app's exhaustive `switch` still compiles. §8's CHANGELOG says it is no longer emitted.
`setExtra('route', 'x'.repeat(70_000))` is sent with `route` removed and `truncated_keys: ['route']` (§11.2 TASK-02 case (10)).
With a stop test supplied (§4.6 step 4), the shrink runs step 1 only.

**Per-call identity.** `CaptureContext` gains `user?: User` and `tags?: Record<string, string | number | boolean | null>`. For that
one event, `ctx.user` replaces the scope user (§3.4's rules; `null` sends no user) and `ctx.tags` is merged over the scope's tags
by §3.5's rules (`null` removes a scope tag from this event; `tenant_id` **is** allowed here, so a server can attribute one
request's event to a tenant). This is the per-request path on the server, where the scope is global (contract §2), and it is what
the README's workaround now says.

**After this change metadata never carries tags, `user_id` or `tenant_id`** (contract §4.1, decision 7) — neither from the scope
nor from a per-call `ctx.metadata`. `metadata.runtime` stays (S5). `metadata.level` goes (§3.2).

### 3.8 After `beforeSend`

`afterHook` (T6, T17) re-normalises the hook's result so that **no `beforeSend` can make the ingestor 400 the event** — not by
a JSON type (S1), not by a protovalidate rule (S10), not by a case-folded key, and not by a `toJSON`. It does so in three steps:

1. **A fresh object from an allow-list.** The result must be a non-null, non-array object (else the event is dropped, as today).
   `afterHook` builds a **new** plain object and copies into it only the exact keys of `SentinelEvent`: `event_id platform
   environment release_version error_class message stacktrace metadata timestamp fingerprint trace_id span_id trace_flags level
   sdk user tags contexts request breadcrumbs transaction server_name`. Every other key the hook left is not copied, with one
   debug line naming them. That removes the `ErrorPayload` keys the SDK never sends — `logger`, `dist`, `source`, `exception`,
   `project_key`, `project_id`, `fingerprint_override` (this spec sends none of them, §2) — and every case variant (`LEVEL`,
   `Logger`, `Tags`) that Go's case-insensitive decoder would otherwise type-check as a real field (S10). Because the result is a
   new object, a `toJSON` on the hook's returned object or its prototype is never consulted by `enqueue`'s `sanitize` (T16, T17).
2. **Each copied key re-checked**, by the table below. Every row reads the hook's value directly and writes new plain data, so
   no value on the wire is an object the hook built (and a nested `toJSON` is either rejected or run through `sanitize` and then
   checked with `isJsonObject`, §3.6).
3. Then today's steps: the bounds, §3.10's contexts, headers / env and request-string bounds (before the shrink, so a
   `'contexts'` marker is measured), the metadata shrink and the scrub (T6), the breadcrumb total of §4.4, in the browser §4.5, and on every runtime §4.6.

| Field | After the hook |
|---|---|
| `event_id`, `platform`, `timestamp` | re-stamped from the pre-hook event (today) |
| `sdk` | re-stamped (SDK-owned) |
| `environment` | kept when a string matching `^[a-z0-9][a-z0-9._-]{0,49}$` (the proto rule, S10), else **the pre-hook value** with a debug line — not deleted, because `environment` is `required` and an absent one is itself a 400 |
| `release_version` | kept when a string of 1-100 UTF-8 bytes (`MAX_RELEASE_BYTES`, `src/core/init.ts`), else removed with a debug line |
| `fingerprint` | `readFingerprint` re-applied (a non-empty string of ≤ 64 bytes, else removed; over 64 → `fingerprint_too_long`, as at build) |
| `trace_id`, `span_id` | each kept when a string of ≤ 64 code points, else removed with a debug line |
| `trace_flags` | kept when an integer 0-255 (a W3C trace-flags byte; inside `uint32`), else removed with a debug line |
| `error_class`, `message` | today's `messageText` and cuts |
| `stacktrace` | today's frame rebuild, except that `line` / `column` are kept only when an integer **1-2 147 483 647** (Go `int32`, S10; today has no upper bound, T17); the same rule applies at build |
| `metadata` | today's treatment (T6) |
| `level` | kept if valid, else the pre-hook level |
| `user` | §3.4's rules re-applied to whatever object the hook left (no `{{auto}}` re-added if the hook removed `user`); not a non-null, non-array object → removed; a `geo` or any other key → not copied |
| `tags` | §3.5 rule 2-3 re-applied per entry, then rule 6's 50-tag cap (`tenant_id` kept); not a non-null, non-array object → removed |
| `contexts` | not a non-null, non-array object → removed; otherwise each value is run through `sanitize` and kept only when `isJsonObject` holds on the **sanitised** value (§3.6) — so a hook's `contexts.x = new Date()` / `dayjs()` / `new URL(u)` / `{ toJSON: () => [] }` removes the name `x`, with a debug line, instead of 400ing the batch; then §3.10's contexts bound (a name the hook added counts like any other); an empty result → the key removed |
| `request` | not a non-null, non-array object → removed; `method`, `url`, `query_string` kept only as strings, then cut by §3.10's `boundRequestStrings` (U+0000 removed first, then 16 / 2048 / 4096 code points: the server's `boundRequest`, S2); `headers` / `env` kept only as non-null, non-array objects, each non-string value removed, then bounded by §3.10 (64 entries, key 1-256 code points, value cut to 4096 / 1024 code points); `data` sanitised, then removed when over 16 384 bytes by the Go ruler (`metadataWireBytes(JSON.stringify(data))`), the server's own `MaxRequestDataSize` (S2), so §4.6 never needs to shed it; anything else removed; an empty result → the key removed |
| `breadcrumbs` | not an array → removed; each element re-normalised by §4.2 (an element that is not a non-null, non-array object is removed; an element whose `timestamp` is absent or unusable is **kept and stamped now**, by §4.2's `timestamp` rule, exactly as `addBreadcrumb` stamps one; its `data` is kept only when `isJsonObject` holds after `sanitize`, §4.2); then, when more than 100 are left, only the **last 100** are kept (the newest, which are the ones the server's `boundBreadcrumbs` keeps, S2); then §4.4's sizes re-applied |
| `transaction`, `server_name` | kept only as strings, cut as §3.1 |

Every removal is one debug line naming the field. The proof is a golden fixture built through a deliberately hostile
`beforeSend` and replayed through the ingestor's strict decode, `ValidatePayload` and protovalidate (§7.3, §11.2).

**Every typed field a hook can set, and its bound (spec review round 9).** §3.10 and §4.6 rely on the SDK sending nothing that
the server's bounds step (`apps/ingestor-go/validation/bounds.go: applyBounds`) would cut or drop, because the estimate counts
what the SDK sends. A `beforeSend` can set any field below, so each one ends at the server's bound or tighter, whatever set it.
Revision 10 had two gaps. It kept a hook's `request.method` / `url` / `query_string` at any length, where the server cuts them
to 16 / 2048 / 4096 code points, so a hook's 1 000 001-character URL made §4.6 shed crumbs for bytes the server never publishes.
It also let a hook's `breadcrumbs` array of 150 crumbs reach `enqueue` whole, where the server keeps the last 100. The audit:

| Field a hook can set | The SDK's bound after the hook | The server's (S2) |
|---|---|---|
| `environment`, `release_version`, `fingerprint`, `trace_id`, `span_id`, `trace_flags`, `level` | the rows above (the proto rules, S10) | protovalidate, or `level` → `""` |
| `event_id`, `platform`, `timestamp`, `sdk` | re-stamped | — |
| `error_class`, `message` | 255 / 10 000 UTF-8 bytes (`applyBounds`, T6) | protovalidate, 255 / 10 000 |
| `stacktrace` | 100 frames; `file` 512 and `function` 255 bytes; only `file function line column in_app` are copied, so no `module`, `abs_path` or context lines reach the wire | top-level `file` / `function` are not cut (fingerprint inputs); the three v2 frame fields are cut |
| `metadata` | the 65 536 shrink (T4) | the same shrink |
| `user` | §3.4: `id` ≤ 255 bytes, `email` ≤ 254 and `ip_address` ≤ 45 code points (else omitted), `username` cut to 128; `geo` not copied | the same |
| `tags` | §3.5 rules 2, 3 and 6 | the same rules, and the 50 smallest keys |
| `contexts` | §3.10 `boundContexts` | `boundContexts` |
| `request.method`, `url`, `query_string` | §3.10 `boundRequestStrings` (**new in revision 11**) | `c.str`: U+0000 stripped, then 16 / 2048 / 4096 |
| `request.headers`, `env` | §3.10 `boundStringMap` | `boundStringMap` |
| `request.data` | removed over 16 384 Go-ruler bytes | the same |
| `breadcrumbs` | the last 100 (**new in revision 11**), §4.2 per crumb, §4.4's total | the last 100, the same per-crumb cuts, the same total |
| `transaction`, `server_name` | cut to 200 / 256 code points | the same |
| `logger`, `dist`, `source`, `exception`, any other key | not copied (step 1) | — |

Where the SDK cuts a string that still holds a U+0000 (`username`, `transaction`, the crumb strings), the string it sends is
never shorter than the one the server keeps. The server strips U+0000 and then cuts to the same count, so the SDK over-counts by
at most the NULs, which is the safe direction for §4.6. An identifier the SDK measures with its NULs (`user.id`, `email`) can be
dropped where the server would keep it, which is stricter than the server, as §3.10's names are.

### 3.9 Public API added (all entries, and `./nextjs`'s union types)

`setTags`, `setContext`, `setExtra`, `setExtras`, `addBreadcrumb`; `CaptureContext.user` / `.tags` (§3.7); option `maxBreadcrumbs`, `beforeBreadcrumb` (all runtimes);
`breadcrumbs` and `autoIpAddress` (browser entries only); `serverName` (node entries only). Changed signatures: `setUser`,
`setTenant` (now `string | number | null`), `captureMessage` (D5), `Level`. `getClient().options` shows the resolved new options
(the key-redaction rule of decision 36 is unchanged).

### 3.10 The server's contexts and request-map bounds, mirrored (TASK-01, for §4.6)

The SDK already applies four of the ingestor's bounds itself, so that the server's own step removes nothing from what the SDK
sends: the metadata shrink (T4), the 50-tag cap (§3.5 rule 6), the breadcrumb total (§4.4) and the `request.data` bound
(§3.8). This section adds the rest: `contexts`, `request.headers` / `request.env`, and, since revision 11, `request.method` /
`url` / `query_string` (`apps/ingestor-go/validation/bounds.go: boundContexts`, `boundStringMap`, `boundRequest`, S2). All three
run in `src/core/server-bounds.ts` (new), on
every runtime, at `build` (after the automatic contexts of §6 are merged and §6.4's request is built) and again in `afterHook`
(after the `contexts` and `request` rows of §3.8), in both places **before** the metadata shrink.

**Contexts** (`boundContexts(contexts)`), after each value has passed §3.6's `sanitize` and `isJsonObject`:

1. A name that contains U+0000 or is not 1-64 code points is removed. (`setContext` already refuses a bad name, §3.6. A
   `beforeSend` can still add one. The server renames a U+0000 name rather than removing it, so this is stricter than the
   server, never looser.)
2. Each remaining name's size is `pairBytes(name, value)` (`src/core/event.ts`: `metadataWireBytes(JSON.stringify(name) + ':' +
   JSON.stringify(value))`), which is the server's `entrySize` by the Go ruler the SDK already carries (T4). The object's size is
   the server's `objectSize(body, n)`: `body + 2`, plus `n − 1` commas when `n > 1`.
3. Victims are sorted once (a `slice()` then `sort()`, not `toSorted`, §5.1 *Built-in API floor*), in the server's order
   (`bounds.go: sortVictims`): names other than `device os app runtime browser
   culture` first, then those six. Within each group the larger size goes first, and a tie goes to the name that is smaller by
   code point (Go's byte order; `compareCodePoints`, below).
4. While `n > 32` or `objectSize(body, n) > 65 536`, the next victim is removed and its size subtracted. There is no
   re-serialising per removal, which is the server's own fix for `esv2-rev-bounds-quadratic-dos`.
5. If any name was removed, `'contexts'` joins `metadata.truncated` (the §4.5 vocabulary entry) before the event's metadata
   shrink runs, so the shrink measures it, and one debug line names the removed names. An emptied map deletes the key.

**Headers and env** (`boundStringMap(map, maxValue)`, `maxValue` 4096 for `headers` and 1024 for `env`), after §3.8's
string-only check:

1. Every key and value is made well-formed first: each lone surrogate becomes U+FFFD, which is what Go's JSON decoder stores for
   it. Code-point counts and key order are then the ones the server sees. The repair is a loop over `charCodeAt`, not
   `String.prototype.toWellFormed`, which Safari has only from 16.4 (§5.1 *Built-in API floor*).
2. A key that contains U+0000 or is not 1-256 code points is dropped. Like the contexts rule, this is stricter than the server,
   which strips U+0000 and keeps the key.
3. U+0000 is removed from each value, and the value is cut to `maxValue` code points.
4. Past 64 entries, only the 64 keys that are smallest by code point are kept, and the rest are dropped. That is the server's
   `sortedKeys(out)[64:]` rule.
5. One debug line names the dropped keys and the cut values. There is no `metadata.truncated` marker, as for `request.data`
   (SP34). A map left empty deletes its key, and a `request` left empty is removed (§3.8).

**Request strings** (`boundRequestStrings(request)`, revision 11), after §3.8's string-only check, and at `build` after §6.1 or
§6.4 has filled `request`:

1. U+0000 is removed from `method`, `url` and `query_string`, as the server's `c.str` removes it before it measures.
2. Each is cut to the server's bound in code points: `method` 16, `url` 2048, `query_string` 4096 (`MaxRequestMethodLength`,
   `MaxRequestURLLength`, `MaxRequestQueryStringLength`). A code point above U+FFFF counts once, and the cut never splits a
   surrogate pair.
3. A value left empty deletes its key. One debug line names each field changed. There is no `metadata.truncated` marker, as
   for headers / env.

At `build` this changes nothing that §6.1 and §6.4 produce, since both already cut to the same numbers after filtering (§5.1
*Filter, then cut*). It exists for the hook. It runs at `build` as well so that one function holds the rule wherever `request`
was set.

`compareCodePoints(a, b)` compares two strings by code point, not by UTF-16 unit, so a key in U+E000-U+FFFF and a key above
U+FFFF sort as Go's `sort.Strings` sorts them. The tags cap keeps JS `<`, because tag keys are ASCII (§3.5 rule 3).

**Why the SDK's measure is never below the server's.** Where the two rulers differ, the SDK counts more. A lone surrogate
serialises to a 6-byte `\udXXX` in JS and to a 3-byte U+FFFD in Go, and a U+0000 inside a context value counts 6 bytes in the
SDK but is stripped before the server measures. So every context the SDK keeps is one the server keeps too. At a boundary the
SDK can remove a context the server would have kept, which is the same direction the metadata shrink already errs in (T4).
§6.4's header bound is this function, called by `captureRequestError`'s build.

**What it changes for §4.6.** After this section, every field the estimate counts is one the server keeps. The metadata, tags,
breadcrumbs and `request.data` rules were already mirrored, and every other typed field, whatever set it, is cut to the server's
bound or tighter (§3.8 *Every typed field a hook can set*, which also lists the two gaps revision 11 closed). The estimate is therefore of the event the ingestor publishes, less what the ingestor adds itself (§4.6
*The budget*).

---

## 4. Breadcrumbs core (TASK-02)

### 4.1 The buffer

- `Scope.breadcrumbs`: a ring of at most `maxBreadcrumbs` crumbs, oldest first, on the `globalThis` slot — global in the browser,
  per process on node/edge, kept across a second `init`, recorded before the first `init` with the default capacity.
- `maxBreadcrumbs` (all runtimes): an integer `0`-`100`; default `100` (the ingestor keeps the last 100, S2). `undefined` → 100.
  A non-integer, negative or non-number → 100 with one `invalid_option` (option `maxBreadcrumbs`). Over 100 → 100 with one
  `invalid_option` (the message says the ingestor keeps 100). `0` disables breadcrumbs: `addBreadcrumb` is a no-op and no
  automatic integration is installed.
- On `init`, the existing buffer is trimmed to the new capacity (oldest dropped), and then — when the new client has a
  `beforeBreadcrumb` — every retained crumb (recorded before the first `init`, or under a previous client) is passed through it by
  §4.3's rules, in order, and replaced or dropped, so no crumb in the buffer has skipped the hook the current client installed.

### 4.2 `addBreadcrumb(crumb, hint?)` and normalisation

`Breadcrumb = { type?: string; category?: string; message?: string; level?: Level; data?: Record<string, unknown>; timestamp?:
string | number | Date }`. Normalised at add time, before `beforeBreadcrumb` sees it, into the wire shape:

| Key | Rule |
|---|---|
| `timestamp` | a `Date`, a number (Unix **seconds**, Sentry's convention) or a string that `Date.parse` accepts is converted with `toISOString()`; absent or unusable → now. A result outside years 0000-9999 (where `toISOString` emits `+010000-…`, which Go's RFC 3339 decoder rejects) → now. Always an RFC 3339 string on the wire (S1: a number 400s the event). |
| `type` | string, cut to 64 code points; absent → `'default'` |
| `category` | string, cut to 128 code points; absent → omitted |
| `message` | string, cut to 8192 code points; a non-string is `String(v)` through `stringOrMarker`; absent → omitted |
| `level` | one of the five, else `'info'` |
| `data` | a non-null, non-array object, sanitised (`sanitize`), and kept only when `isJsonObject` (§3.6) holds on the **sanitised** value — `addBreadcrumb({ data: new URL(u) })`, `data: new Date()` or a `data` whose `toJSON` returns an array serialise to a string / array, which `Breadcrumb.data` (`map[string]interface{}`, S1) would reject with a 400, so `data` is omitted with a debug line and the crumb kept; then PII-scrubbed with `scrubPII` (the metadata scrub, T4 — crumbs are recorded automatically, so the same client-side scrub applies); over **4096 bytes by the Go ruler** (`metadataWireBytes(JSON.stringify(data))`) → `data` removed, debug line; absent → omitted |
| other keys | dropped (a strict decode rejects them) |

`hint` is passed to `beforeBreadcrumb` unchanged (default `{}`); the automatic integrations pass §5's hints. A `crumb` that is not a
non-null, non-array object makes `addBreadcrumb` a no-op with a debug line. The same normalisation runs on a `beforeBreadcrumb`
result (§4.3) and on every element of a `beforeSend` result's `breadcrumbs` (§3.8), so the post-`sanitize` check on `data`
holds on all three paths.

### 4.3 `beforeBreadcrumb(crumb, hint) => crumb | null`

- Called synchronously for every crumb (manual and automatic) after normalisation, before the crumb enters the buffer.
- Returns: `null` → dropped. A non-null, non-array, non-thenable object → re-normalised by §4.2 (so a hook cannot put a wrong type on the wire —
  including a `data` whose `toJSON` returns a non-object, §4.2) and stored.
  Anything else (a primitive, an array, a promise) → **dropped**.
- A hook that **throws** → the crumb is **dropped** with one debug line (no `onError`): this hook is where apps remove private data
  (web does, §9), so failing open would record exactly what the app tried to remove. This differs deliberately from `beforeSend`,
  whose throw sends the event unmodified (T6); losing a crumb costs little, losing a privacy filter costs a leak.
- A non-function option value is ignored, as `beforeSend`'s is (`src/core/init.ts`).

### 4.4 Attaching, and the size rules

At capture, the event's `breadcrumbs` is a **copy** of the buffer, omitted when empty. Later crumbs and later mutation do not
change a captured event. Each crumb is copied as `{ ...crumb, data: sanitize(crumb.data).value }`, with the `data` key left out
when the stored crumb has none. After §4.2 a stored crumb is plain data: strings, a `level`, an ISO timestamp string, and a
`data` that `sanitize` already produced. So the spread copies every top-level value, and `sanitize` makes a fresh `data` that
shares nothing with the buffer. Nothing deeper is needed, and `structuredClone` is not used, because Safari has it only from
15.4 (§5.1 *Built-in API floor*). Then:

| Limit | Value | Enforced where | Rule |
|---|---|---|---|
| Count | 100 | the buffer (§4.1), and `afterHook` for a hook's array (§3.8) | ring capacity `maxBreadcrumbs` ≤ 100; a hook's array keeps its last 100 |
| Per-crumb strings | 64 / 128 / 8192 code points | `addBreadcrumb` (§4.2), again after the hooks | cut |
| Per-crumb `data` | 4096 bytes, Go ruler | `addBreadcrumb`, again after the hooks | `data` removed |
| Total | 131 072 bytes, Go ruler | `build` and again in `afterHook`, all runtimes | drop the **oldest** while over |
| Contexts (not a breadcrumb limit; listed because §4.5 and §4.6 rely on it) | 32 names, 65 536 bytes, Go ruler | `build` and again in `afterHook`, all runtimes | §3.10: custom names before the six well-known ones, largest first |
| `request.headers` / `env` (likewise) | 64 entries; key 256, value 4096 / 1024 code points | `build` and again in `afterHook`, all runtimes | §3.10: value cut, lexically largest keys dropped |
| `request.method` / `url` / `query_string` (likewise) | 16 / 2048 / 4096 code points, U+0000 removed first | `build` and again in `afterHook`, all runtimes | §3.10: cut |
| Browser transport | 61 440 bytes per request | `enqueue` (after `beforeSend`), browser runtime only | §4.5 |
| Tunnel body | 262 144 bytes (`maxBodyBytes`) | the tunnel, unchanged | §4.5's last resort keeps this feature from pushing an event over it |
| Ingestor body | 1 MiB single / 5 MiB batch (JSON) | the ingestor, unchanged | not the binding limit: the proto below binds first |
| NATS publish | 1 048 576 bytes of `ErrorEvent` proto + headers (S11) | `enqueue`, all runtimes: estimate ≤ 1 000 000 | §4.6 |

**The Go ruler for the total.** The ingestor measures `json.Marshal` of its `Breadcrumb` struct, which always has all six keys
(S2). The SDK therefore measures each crumb as `metadataWireBytes(JSON.stringify(full))`, where `full` is the crumb with every
missing key filled (`""` for the strings, `null` for `data`, the timestamp as sent). A JS ISO timestamp (`…T12:00:00.123Z`) is never
shorter than Go's RFC3339Nano re-marshal of it (Go drops trailing zeros), so the SDK's count is never below the server's, and a
list the SDK keeps is never trimmed again by the server. The array size is `sum + 2 + (n − 1)` (the server's `arraySize`). Each
crumb is measured once and the total kept arithmetically while dropping (the server's review finding
`esv2-rev2-breadcrumbs-byte-loop-quadratic` applies equally here).

### 4.5 The browser keepalive rule (and the tunnel)

Only for `runtime === 'browser'`, in `enqueue` (`capture.ts`), on the final event (after `beforeSend` and `afterHook`), before it
is serialised for the queue:

1. `bytes = utf8Bytes(JSON.stringify(event))` — the queue's own count (T7). If `bytes + 2 ≤ 61 440` (the event alone in a batch
   body), stop.
2. Otherwise remove breadcrumbs **oldest first**, one at a time, in **one loop whose measure always includes the marker**:
   - Removing a crumb subtracts its serialised bytes and its comma from `bytes`. Each crumb is serialised once for this. If the
     list empties, the `breadcrumbs` key is deleted and `bytes` re-measured.
   - **Immediately after the first removal**, before the next comparison, `metadata.truncated` gains the new vocabulary entry
     `'breadcrumbs'` (appended to `TRUNCATED_VOCABULARY` after `stacktrace.function`) and rule 3's `shrinkMetadata` re-run
     follows; then `bytes` is re-measured once, as in rule 1, so it counts the marker and whatever the re-run removed.
   - The loop continues against that updated `bytes` until `bytes + 2 ≤ 61 440` or no crumb is left. One debug line names the
     count removed.

   The marker is inside the loop because it can decide the outcome. It adds 28 bytes (`,"truncated":["breadcrumbs"]`), or 14
   (`,"breadcrumbs"`) when `metadata.truncated` already exists, to an event the loop had just brought within 61 440. Revision 7
   added it **after** the loop, so a removal that left the event 1-28 bytes under the cap was pushed back over by the marker.
   Such an event still had crumbs, so rule 4 (no breadcrumbs left) did not describe it, and it went without `keepalive`
   although removing one more crumb would have let it fit. Spec review round 6's case was a 60 973-character string and three
   console crumbs of ~141 bytes: one removal fitted, and the marker tipped the event over. §11.2 TASK-02 pins it. With the
   marker in the measure, the loop removes that next crumb.
3. **The `shrinkMetadata` re-run** (`src/core/event.ts`), run once by rule 2 after the `'breadcrumbs'` marker is added, and by
   rule 5 after each of its markers, and by §4.6 steps 2 and 3 after theirs. It runs over `event.metadata`. The `truncated` set is rebuilt from `metadata.truncated` (as
   `afterHook` rebuilds it) plus the new entry, and `truncated_keys` is taken from `metadata.truncated_keys`. The re-run writes
   both markers itself and measures them inside the 65 536 bound (T4). Without it, a `metadata` that `afterHook` left within a
   few bytes of 65 536 goes over by the marker. The ingestor's own shrink
   (`apps/ingestor-go/validation/bounds.go: shrinkMetadata`) then removes a key the SDK did not choose and records it under
   `server_truncated_keys`, where the SDK's shrink exists to keep the server's a no-op (T4). **Re-running, not reserving the
   marker's bytes, and why:** a reservation would have to be taken in `afterHook`'s shrink on every event, for a marker only a
   trimmed browser event carries. It would be up to 46 bytes (`,"truncated":["breadcrumbs","contexts","tags"]`) of arithmetic
   kept beside the one ruler and changed with the vocabulary. The re-run reuses that ruler, costs nothing unless a marker was
   added, and never raises `bytes` above the marker-added figure: it removes keys only when `metadata` is over 65 536, and
   leaves it within 65 536. **What decides `keepalive` fit, corrected:** the re-run acts only when `metadata` alone is over
   65 536 − 28 bytes, so the event is already past 61 440 before the marker. It can therefore let an event fit where it would
   not have (the near-cap case: the re-run removes the large metadata key, and the loop stops with crumbs kept, §11.2 TASK-02),
   but it never makes an event miss the cap. The **marker** can make an event miss it, by the 28 bytes above, and that is why
   the loop measures it.

   **The re-run cannot fail, so the marker is always kept** (revision 12). Revision 11 withdrew a marker whose re-run returned
   `false`, which happened when the exempt keys fit 65 536 alone and not with the marker. §3.7's shrink now removes the exempt
   keys last instead of giving up, so the re-run always leaves `metadata` within 65 536 with the marker in it. In the browser,
   `setExtra('route', 'x'.repeat(65_480))` leaves `metadata` at 65 512 bytes, and `'breadcrumbs'` takes it to 65 540. The re-run
   now removes `route` (§3.7 step 1). `bytes` falls by about 65 KB, and the loop stops with two of three crumbs kept, sent with
   `keepalive` (§11.2 TASK-02 case (10)). The same holds for rule 5's markers and §4.6's, and for this feature's `'contexts'` /
   `'tags'` markers at `build` / `afterHook` (§3.5 rule 6, §3.10), whose shrink now always passes. No marker is withdrawn, and no
   event is dropped for its metadata anywhere in the SDK.
4. If the event is still over 61 440 bytes with no breadcrumbs left, it is what 0.1.x would have sent plus this feature's other
   fields, and it goes as today: alone, without `keepalive` (T7).
5. **Last resort, for the tunnel's 262 144.** If the event is still over 262 142 bytes, the SDK removes `contexts`, then
   **every tag except `tenant_id`** (the only other fields this feature adds that an app can make large; `user`, `sdk`, `level`
   and the browser `request` are bounded to under 67 KB together, §6.1), recording `'contexts'` / `'tags'` in
   `metadata.truncated`. `tenant_id` stays because it is the processor's affected tenant (S6) and metadata no longer carries a
   fallback (§3.7), the reason §3.5 rule 6 keeps it past 50 tags. It is one value of at most 200 code points, under 1 250 JSON
   bytes. An event with no other tag has nothing removed by this step and gets no `'tags'` marker, and a `tags` left empty
   (no `tenant_id`) deletes the key. As in rule 2, each
   removal is followed at once by its marker and rule 3's `shrinkMetadata` re-run, and `bytes` is re-measured before the next
   comparison, so the test against 262 142 always counts the markers; the tags are removed only if the event is still over
   after `contexts` and its marker. What remains is
   an event 0.1.x could already produce, except that `tenant_id` rides in `tags` where 0.1.x put it in `metadata` (T4), about
   10 bytes more for the `tags` key. Its fate is today's (the tunnel's `413` → `batch_dropped`). Apart from those 10 bytes, this
   feature never makes a browser event exceed a transport it previously fitted.

So: a browser event whose non-breadcrumb part, **with the `'breadcrumbs'` marker added** (at most 28 bytes more), fits
61 440 − 2 bytes is **always** sent with `keepalive` on page hide; the ingestor's 128 KiB breadcrumb cap is never the binding
one in the browser. An event whose non-breadcrumb part fits without the marker but not with it (a window 28 bytes wide) has
every crumb removed and goes by rule 4, alone and without `keepalive`. The marker is kept even so: without it, a reader
cannot tell an event that had no breadcrumbs from one whose breadcrumbs the SDK removed.

**The tunnel's `maxBodyBytes` default does not change** (contract §4.5): the browser posts batches of at most 61 440 bytes except for an oversize single event, and rule 5 keeps this feature from pushing
one of those past 262 144.

On node and edge there is no keepalive cap (T7's packing still applies), and only the 131 072 total rule and §4.6 apply. S3's
1 MiB JSON cap is not what binds a server event. The ingestor's NATS publish of the event as protobuf binds first (S11), because
`metadata`, contexts and breadcrumb `data` can cost up to ~5.5 times their JSON size there. §4.6 is the rule for that, on every
runtime.

### 4.6 The NATS publish bound (all runtimes)

**The limit.** The ingestor bounds an event by its JSON (S2, S3), then publishes it to NATS as a protobuf `ErrorEvent`. NATS
refuses a message over `max_payload`, 1 048 576 bytes, and the item is lost (S11). `metadata`, each context and each crumb's `data`
are `google.protobuf.Struct`, where a JSON number costs 11 bytes against its JSON 2 (`0,`). So an event can pass every JSON bound
and still fail to publish. Spec review round 7's event is `setExtra('a', <32 700 zeros>)` plus 32 crumbs with
`data: { a: <2 030 zeros> }`. §4.4's total keeps 31 of the crumbs. Every part is within its bound and the whole is 194 490 JSON
bytes, but it is **1 053 681** bytes as `ErrorEvent`. That figure was measured for this revision with Go's
`proto.Size(mapping.MapPayloadToEvent(p))` after `validation.ValidatePayload(p)`. Adding `setContext('c', { a: <32 700 zeros> })`
makes it 1 413 408. The server's JSON bounds allow up to ~360 KB of metadata, ~360 KB of contexts and ~720 KB of breadcrumbs in
proto, so this is reachable on node and edge with nothing hostile.

**The rule.** In `enqueue` (`capture.ts`), on **every** runtime, on the final event: after `afterHook`, in the browser after
§4.5, before the event is serialised for the queue. By then `build` and `afterHook` have applied every server bound an app can
make large: the metadata shrink, the 50-tag cap, the breadcrumb total, the `request.data` bound, and §3.10's contexts and
headers / env bounds. So step 1 measures only what the server keeps.

1. `est = protoSizeEstimate(event)` (below). If `est ≤ NATS_PUBLISH_BUDGET` (1 000 000), stop. Every ordinary event stops here,
   after one walk.
2. **Breadcrumbs**, oldest first, in one loop with the marker inside the measure, as in §4.5 rule 2. Each removal subtracts that
   crumb's contribution from `est`, computed once: `utf8Bytes(JSON.stringify(crumb with data → {}))`, plus 1 for its comma, plus
   `structBytes(crumb.data)`. Immediately after the first removal, `'breadcrumbs'` joins `metadata.truncated` if it is absent,
   §4.5 rule 3's `shrinkMetadata` re-run follows (it cannot fail, §3.7), and `est` is recomputed. A list that empties deletes
   the `breadcrumbs` key and `est` is recomputed. The loop stops once `est ≤ 1 000 000`.
3. **Contexts**, one name at a time: names other than `device os app runtime browser culture` first, then those six (the server's
   own victim order, `bounds.go: boundContexts`, S2). Within each group the name with the largest contribution goes first, and a
   tie goes to the lexically smallest name. A name's contribution is `utf8Bytes(JSON.stringify(name)) + 4` (`:{}` and a comma)
   `+ structBytes(value) + 8`. The first removal adds `'contexts'` to `metadata.truncated`, followed by the re-run and a recompute,
   as in step 2. An emptied map deletes the key and `est` is recomputed.
4. **Metadata**, through the existing `shrinkMetadata` (`src/core/event.ts`), which gains two optional parameters whose defaults
   are today's behaviour. The first is the per-key measure: here `fld(fld(utf8Bytes(key)) + fld(val(value)))` instead of
   `pairBytes`. The second is the stop test: here `rest + structBytes(metadata) ≤ 1 000 000`, where
   `rest = est − structBytes(metadata)` is taken once at the start of this step. The shrink evaluates the test after writing its
   markers, as it does today, so the test counts them. With a stop test supplied, the shrink runs §3.7's step 1 only:
   `runtime` and the `METADATA_EXEMPT_KEYS` are never removed here (they are what enrichment and the processor route on, and
   step 5 sends what is left), an earlier-inserted key wins a tie, and each removed key is recorded in `truncated_keys`.
5. If the estimate is still over 1 000 000 once step 4 has no victim left, the event is **sent as it stands**, with one debug line
   giving the estimate. The estimate is an upper bound, so dropping the event here could drop one the ingestor would publish. What
   is left is either bounded by the SDK or its runtime, or it is something 0.1.x could already send.

Each step writes one debug line naming what it removed. No new report code and no new `metadata.truncated` value is added:
`breadcrumbs` and `contexts` are §4.5's.

**Why this order, and what is not shed.** The order is the browser's (§4.5): breadcrumbs first, then contexts, which this feature
adds, then metadata, which 0.1.x already sent. `tags` need no step, because §3.5 rule 6 caps them at 50, which is at most ~42 KB.
`request.data` needs none either: §3.8 removes it over the server's 16 384-byte bound (S2), which leaves at most ~90 KB. The other
typed fields are cut to the server's own bounds, whatever set them (§3.8 *Every typed field a hook can set*). `request.headers` / `env` are bounded by §3.10 to the server's own bound (64
entries each, values of at most 4096 / 1024 code points), but no step sheds them further. With the other bounded strings, that
bound still admits more than the budget. 64 headers of 4096 three-byte characters are ~787 KB, 64 env values of 1024 add ~197 KB,
and a 10 000-byte `message` and a 4096-code-point `query_string` of the same characters take the estimate past 1 000 000. Step 5
sends such an event as it stands (§11.2 TASK-02 case (5)). By then the crumbs, the contexts and every metadata key but `runtime`
and the exempt ones are gone, and what is left is mostly strings, whose proto size is close to their JSON size. The ingestor
refuses a JSON body over 1 MiB at the door (S3). So such an event is published, as case (5)'s is at 1 006 977 proto bytes,
unless it is within a few KB of that cap. In
that case the ingestor refuses it, or fails to publish it, as it would the same maps from any client (§13 F4). Dropping it in
the SDK would save nothing in either case.

**The estimate.** `protoSizeEstimate(event)` in `src/core/proto-size.ts` (new) is an upper bound on the `ErrorEvent` the ingestor
would marshal from the event as sent:

```js
vl(n)          = n < 2 ** 7 ? 1 : n < 2 ** 14 ? 2 : n < 2 ** 21 ? 3 : 4   // a varint's length
fld(n)         = 1 + vl(n) + n                        // one length-delimited field with a 1-byte tag
val(v)         = v === null || typeof v === 'boolean' ? 2
               : typeof v === 'number'                ? 9
               : typeof v === 'string'                ? fld(utf8Bytes(v))
               : Array.isArray(v)                     ? fld(sum of fld(val(e)) over the elements)
               :                                        fld(structBytes(v))
structBytes(o) = sum over o's own keys k of fld(fld(utf8Bytes(k)) + fld(val(o[k])))

protoSizeEstimate(e) =
    utf8Bytes(JSON.stringify(skeleton))   // e with metadata, every context value and every crumb's data → {}, request.data → null
  + structBytes(e.metadata) + (sum of structBytes(value) over e.contexts)
  + (sum of structBytes(crumb.data) over the crumbs that have data)
  + (e.request?.data === undefined ? 0 : val(e.request.data))
  + 8 × (the number of entries in tags, contexts, request.headers and request.env)
```

`val` and `structBytes` are the exact proto3 wire sizes of a `google.protobuf.Value` and a `Struct`'s content. A `Value` always
writes its oneof, so `null`, `false` and `0` cost their tag plus 1 or 8 bytes. A map entry always writes its key and its value.
`utf8Bytes` counts raw UTF-8 (`TextEncoder`), so a lone surrogate costs 3 bytes, as Go's U+FFFD replacement does. Everything
else is bounded by the skeleton's JSON:

- A proto string field costs at most 2 bytes of tag, 3 of length and its bytes. Its JSON costs at least two quotes, a quoted key
  and a colon, and JSON escaping only adds to that.
- A `Timestamp` costs at most 14 bytes, where JSON spends at least 30.
- Frame lines and columns are varints.
- The skeleton's `"metadata":{}`, `"data":{}` and `"data":null` cover their fields' tag and length.
- The one place proto framing can exceed JSON's punctuation is a map entry (`tags`, `contexts`, `headers`, `env`): a 2-byte
  field tag and up to three length varints, at most 7 bytes more than `"k":…,`. The 8 per entry covers it.

Before revision 9 the formula was checked against Go's `proto.Size(mapping.MapPayloadToEvent(p))` of the same JSON (before
the bounds, which only remove) on 56 events. They were §11.2's four size cases before and after the shed, the reviewer's event
with 31 and with 32 crumbs, and 40 events with random nested values (numbers, booleans, `null`, empty, non-ASCII and U+2028
strings, nested lists and objects). The estimate was never below the Go size, and was above it by 229 to 1 986 bytes. For the
reviewer's event it was 1 055 508 against 1 053 681. Revision 10 checked eight more, after `ValidatePayload` and with the
ingestor-stamped `project_key` / `project_id` cleared: case (8)'s two events, the revised case (5) before and after its shed,
the browser event of case (7), and the two new fixtures of §7.3. The estimate was again never below Go. A string/header-heavy event (50 tags, 64 headers and 64 env values, and 30 contexts, each of 100 `é`, sent by
`captureMessage`) estimates 46 203 against Go's 44 803. Without the 8 per entry it would be 44 523, **below** Go, and with the
skeleton counted in UTF-16 units instead of UTF-8 bytes it would be 28 303, also below. So both terms are load-bearing, and
the fixture that carries this event proves it (§7.3). A number-heavy event estimates 101 305 against 100 404, and its JSON is
only 24 141 bytes.

The implementation is shown correct the same way. Every golden and tunnel fixture's estimate is committed beside it, and the Go
replay asserts `proto.Size` is within it (§7.3).

**The budget, 1 000 000, and why** (spec review round 10, coordinator ruling). It is the sibling sdk-go spec's figure for
the same model (`sentinel-sdk-go-rich-context/spec.md`, *Why 1,000,000 still*). It leaves 48 576 bytes below NATS's 1 048 576
for what is added after the SDK, which is at most ~21 KB and under 1 KB typically:

- the headers the ingestor's publish injects (W3C `traceparent`, `tracestate` and baggage), under 9 KB even at the W3C limits;
- the ingestor's enrichment before publish: `user.ip_address`, `user.geo`, the UA-derived `contexts.browser` / `os` /
  `client_os`, `received_at`, `project_id`, the project name, a re-minted `event_id`, and the `server_truncated` /
  `server_truncated_keys` markers (at most ~7.4 KB of protobuf, `bounds.go: MaxServerTruncatedKeys`,
  `MaxServerTruncatedKeyBytes`).

That is more than twice the additions, before the estimate's own over-count of 229 to 1 986 bytes on the measured events, which
adds to it. Revision 11's 900 000 kept 148 576 bytes, about 100 KB more than those additions need, and shed events the ingestor
would publish. The reviewer's node event with 28 crumbs of `data: { a: Array(2_030).fill(0) }` and
`setExtra('a', Array(32_700).fill(0))` estimates 988 224 and is 986 550 bytes in Go. That is 62 026 bytes below 1 048 576, and
still ~41 KB below it after the worst-case additions, yet 900 000 shed 4 of its crumbs. At 1 000 000 it is sent whole (§11.2 TASK-02 case (1b)). The cost is the margin for
a field a later ingestor adds before publishing: 48 576 bytes where there were 148 576. An ingestor that adds more than ~27 KB
beyond today's additions must lower the budget, which §8's CHANGELOG entry names as a coupling. The budget is never near an ordinary event: a
1 000 000 estimate needs at least ~182 000 JSON bytes, since 5.5 (`0,` → 11) is the largest proto-to-JSON ratio of any value.

**What the estimate counts: only what the server keeps.** Revision 9 estimated contexts as the SDK sent them, over-counting
whenever an app's contexts were over the server's 64 KiB bound, which the server cuts before publishing. That cost real data:
with 100 crumbs of `'m'.repeat(1_190)` (130 201 Go-ruler bytes) and `setContext('big', { s: 'x'.repeat(1_200_000) })`, the
estimate was 1 328 187. Step 2 then removed every crumb, and step 3 removed `big`, which the server would have removed anyway.
With §3.10 the SDK removes `big` at `build`, with the `'contexts'` marker, and the estimate is 128 184, so step 1 stops and all
100 crumbs are sent (§11.2 TASK-02 case (8)). The Go size of that event after `ValidatePayload` is 122 324. Revision 9's argument against
modelling the cut was that a disagreement between the two rulers could make the estimate **lower** than the published size.
§3.10 settles it: the SDK's ruler is never below the server's, so where the two disagree the SDK removes more, never less.
The server's cut is then a no-op on what the SDK sends, and an upper bound on what the SDK sends is an upper bound on what the
ingestor publishes, apart from what the ingestor adds itself, which the budget covers.

**The browser.** An event that §4.5 leaves with crumbs is at most 61 438 JSON bytes, so at most ~338 KB of proto: far below the
budget, and step 1 stops. A crumb-less event can reach the tunnel's 262 144-byte body, which spec review round 7 bounds at
~895 KB published. That is under the budget, so step 1 normally stops there. The rule still runs in the browser, after §4.5,
so that one ruler covers every runtime.

**Cost.** Step 1 is one walk of the event, of the same order as the `JSON.stringify` that `enqueue` already runs. Steps 2 and 3
compute each item's contribution once and subtract it. The whole estimate is recomputed only after a marker and when a list
empties, which is at most four times, so the loops stay linear (`esv2-rev2-breadcrumbs-byte-loop-quadratic`, §4.4). Step 4
re-measures `metadata` alone after each removal, as today's shrink re-serialises it.

---

## 5. Automatic browser breadcrumbs (TASK-03)

Installed by the browser entries' `init` (`./browser`, `./nextjs/client`) after the client exists, unless the client is no-op or
`maxBreadcrumbs` is 0. Never on node or edge. Each installed patch is uninstalled by the client's `teardown` (`src/core/state.ts: InternalClient.teardown`;
`close` and a second `init` run it).

**Option `breadcrumbs`** (browser entries): `false` (none), or `{ console?, fetch?, xhr?, navigation?, click? }`, each `boolean`,
each default `true`. A non-boolean member degrades to `true` with one `invalid_option` (option `breadcrumbs.<name>`); any other
non-object, non-`false` value degrades to all-`true` with one `invalid_option`. `addBreadcrumb` works whatever this says.

**Patching rules (all five):** wrap, never replace behaviour: the wrapper calls the original with the same `this` and arguments,
and rethrows, synchronously, whatever it threw. It returns what the original returned, **except `fetch`**, whose wrapper returns
a new promise chained on the original's (see *No wrapper marks a page promise handled* below). Recording happens in a
`try/catch` that swallows, so a recording failure never reaches the page. A module-level re-entrancy flag is set while the SDK
records a crumb or writes a debug line, and nothing is recorded while it is set. Uninstall restores the original only when the
global still holds the SDK's own wrapper; otherwise (someone wrapped after us — web's `@sentry/nextjs`, W2) the wrapper is left in
place and turned into a pass-through by an `active` flag. A second `init` therefore never stacks two SDK wrappers.

**No wrapper marks a page promise handled.** Attaching a reaction (`then`, `catch`, `finally`) to a promise marks that promise
handled. A fetch wrapper that recorded on settle and then returned the **original** promise would therefore stop the browser
from firing `unhandledrejection` for an uncaught failed `fetch`. That is the page's own error signal, and 0.1.x's `unhandledrejection`
capture (`src/core/handlers/browser.ts`) would stop reporting it. Spec review round 2 measured it: 0 events with the same-object
wrapper, 1 with the chained one. The fetch wrapper therefore returns the **chained** promise, the same shape as Sentry's fetch
instrumentation (`@sentry/core`, `instrument/fetch.ts: instrumentFetch`):

```js
return original.apply(this, arguments).then(
  (response) => { record(response); return response; },
  (error) => { record(undefined, error); throw error; },
);
```

`record` swallows its own errors (the rule above), so each reaction either returns the `Response` or rethrows the original reason
object, and nothing else. The returned promise is a different object from the original but resolves to the same `Response` and
rejects with the same reason. No caller can tell the difference except by identity, and nothing in this spec or in web compares
fetch promises by identity. The consequences, pinned by §11.2 TASK-03:

- An **uncaught** rejected fetch leaves the chained promise with no handler. The browser fires exactly **one** `unhandledrejection`
  for it, and the SDK's handler captures it once. The original promise fires none, because the wrapper's reaction handles it.
- A **caught** rejected fetch fires none.
- When the original throws synchronously, the wrapper rethrows and records nothing. When it returns a value with no callable
  `then` (a broken polyfill), the wrapper returns that value unchanged and records nothing.

No other wrapper touches a promise. `XMLHttpRequest.prototype.open` / `send`, `history.pushState` / `replaceState` and the
console methods are synchronous, and each wrapper returns the original's return value (normally `undefined`) unchanged. The XHR
`loadend`, `popstate` and click listeners are passive, added with `addEventListener`, and never call `preventDefault`,
`stopPropagation` or `stopImmediatePropagation`. Apart from the fetch reaction above, no wrapper or listener calls `then`,
`catch` or `finally` on any value the page owns.

### 5.1 URL privacy (decision 5): the SDK's filter

One function, `filterUrl(url: string, base?: string): string`, in `src/core/url.ts`, applied to **every** URL the SDK records:
breadcrumb `data.url`, `data.from`, `data.to`, `request.url`, `request.query_string`, the `referer` header in §6.4 (D6), and
**every `stacktrace[].file`** (all runtimes, when the event is built, before `applyBounds` cuts the file to `MAX_FILE_BYTES`). A
browser reports an inline script's or an `eval`'s frame under the **page** URL, query included — an error thrown on
`https://app/reset-password?reset_token=X` can carry that string as a frame's `file`. Filtering it costs nothing: the processor
drops a frame path's query and fragment before the sourcemap lookup and the fingerprint hash (S9), so symbolication and grouping
see the same path either way. (Chunk URLs such as `/_next/static/chunks/x.js?dpl=…` keep their non-sensitive parameters byte for
byte, rule 4. Rule 0 changes a frame's file only for a script loaded from a URL carrying userinfo, which no bundler emits.)

0. **Userinfo is removed**, as Sentry's `getSanitizedUrlString` filters it. When the string starts with `//`, or with a scheme
   followed by `//` (`/^[A-Za-z][A-Za-z0-9+.-]*:\/\//`), its **authority** is the text after that `//` up to the first `/`, `\`,
   `?` or `#`, or the end. If the authority contains `@`, everything from the authority's start through its **last** `@` is
   removed (the WHATWG URL parser takes the last `@` as the delimiter, so `https://a@b:p@h/` has host `h`):
   `https://user:pass@h/p?a=1` → `https://h/p?a=1`, `//u@h/x` → `//h/x`. An `@` after the authority (`https://h/users/@me`) and a
   string without `//` (`mailto:a@b.test`) are untouched. Sentry writes `[filtered]:[filtered]@` in its place; this filter
   removes it, because a credential's presence tells a reader nothing and the result is the URL the browser requested minus its
   secret. The helper is exported as `stripUserinfo(url: string): string` from `src/core/url.ts`; it uses `indexOf` /
   `lastIndexOf` and the one anchored scheme test above, and runs in linear time.
1. The fragment (`#…`) is removed.
2. The query string is split on `&` **and on `;`** (the W3C-legacy separator, which some servers still accept, so
   `/p?a=1;reset_token=X` is two parts and rule 3 reaches the second); each part keeps the separator that followed it, so the
   rejoined query differs from the original only where rule 4 or 5 changes a value. Each part is split on its first `=`. The
   **key** is decoded for matching and compared
   case-insensitively. **Decoding**, here and in rule 5, is one **lenient** step **repeated until the string stops changing, at
   most 3 times**. The step is `+` → space, then every maximal run of `%XX` triplets (`/(?:%[0-9A-Fa-f]{2})+/g`) is replaced by
   its decoding:
   - the run through `decodeURIComponent` when that does not throw;
   - otherwise triplet by triplet from the run's start: the longest sequence of 4, 3, 2 or 1 triplets that
     `decodeURIComponent` accepts is decoded and passed; when none is accepted, that one triplet is kept as written and the scan
     moves on by one triplet.

   A `%` not followed by two hex digits (`%zz`, a trailing `%`, `%4`) is in no run and stays as written. The step never throws,
   so one malformed or non-UTF-8 escape never stops the valid escapes around it from being read:
   `%E9%2Freset%3Freset_token%3DX` decodes to `%E9/reset?reset_token=X`, and rule 5 sees its `?`. (Revision 7 decoded the whole
   string with `decodeURIComponent` and kept it raw when that threw. A value with one `%E9` (Latin-1 `é`) or a trailing `%`
   therefore hid its nested URL, and its token was kept.) So a doubly encoded `reset%255Ftoken` or `%252F…` is read as what it
   spells. The count `n` of steps that changed the string is kept for rule 5's write-back. The work is linear: each triplet is
   tried at most four times.
3. A key is **sensitive** when either
   - its lower-cased form **contains** any of the 15 substrings of `src/core/pii.ts: PII_KEYS` (`password passwd pass token bearer
     secret authorization auth credit_card card_number cvv ssn social_security api_key apikey`) — this covers the contract's
     `token`, `secret`, `password`, `auth` and "any key containing `token`" — or
   - one of its **segments**, splitting the lower-cased key on `-`, `_`, `.` and `[`/`]`, **equals** one of `code state key
     signature sig jwt otp credential`.
   The new words match whole segments, not substrings, because `code`, `key` and `state` are substrings of ordinary keys
   (`zipcode`, `monkey`, `statement`); segments still catch `X-Amz-Signature`, `X-Goog-Credential`, `Key-Pair-Id`, `auth_code`,
   `session_state`, `invite_token`, `reset_token`.
4. A sensitive part's value is replaced by the literal `[Filtered]` (the contract's spelling; not percent-encoded, so it reads
   as written; distinct from metadata's `[FILTERED]`, which is Go parity, T4). A part with no `=` is kept as is: it has no key
   for rule 3 to test, and filtering every keyless part would rewrite ordinary URLs (`?print`, `?amp`, `?v2`), so a token sent
   as a bare query (`/verify?<token>`) is a recorded residual (§12.2(3)). Every other part
   is kept **byte for byte** — no re-encoding, no reordering — except as rule 5 says.
5. **A URL nested in a value.** A part kept by rule 4 that has an `=` has its value decoded as rule 2 decodes a key (the same
   lenient step, until stable, at most 3 steps, `n` of them changing it), so a malformed or non-UTF-8 escape anywhere in the
   value does not stop the tests below. The decoded value is **URL-shaped**, a URL carried in a parameter such as
   a `next`, `redirect`, `return_to` or `callbackUrl` target, when either
   - it contains `?`, or
   - it contains `#` and its fragment (the text after the first `#`), split as rule 2 splits a query, has a part with an `=`
     whose key is sensitive by rule 3 — an OAuth-style `/cb#access_token=X` or `/cb#state=s&code=c`. A fragment with no such
     part does not qualify, so `color=%23fff` (`#fff`) and `next=%2Fp%23page%3D2` (`#page=2`) keep their values; or
   - it starts with `//` or a scheme and `//`, and its authority holds `@` (rule 0), so `next=https%3A%2F%2Fu%3Ap%40h%2Fx`
     becomes `next=https%3A%2F%2Fh%2Fx`.

   The parameter's name does not matter here. `filterUrl` is applied to the **decoded** value, recursively, at most **3** levels
   deep; below that, a value is kept as is (rules 3-4 still run at the third level, so a sensitive key there is filtered).
   - If that leaves the decoded value unchanged, the part stays byte for byte.
   - Otherwise the value is replaced by the filtered string passed through `encodeURIComponent` **`n` times** — written as is
     when `n` is 0 (raw equals decoded), so the outer query still parses at every level of encoding it arrived with:
     `next=%2Freset%3Freset_token%3DX` → `next=%2Freset%3Freset_token%3D%5BFiltered%5D` (`n` = 1), and the doubly encoded
     `next=%252Freset%253Freset_token%253DX` → `next=%252Freset%253Freset_token%253D%255BFiltered%255D` (`n` = 2). A
     triplet the lenient decode kept as written is re-encoded with the rest: `next=%E9%2Freset%3Freset_token%3DX` →
     `next=%25E9%2Freset%3Freset_token%3D%5BFiltered%5D`. So in a value that was filtered, that one byte now reads as the text
     `%E9`. The value is recorded for a reader, not requested again, and a value that was not filtered keeps its bytes.

   Examples: `/login?next=/reset-password?reset_token=X` → `/login?next=/reset-password?reset_token=[Filtered]`; the bare
   query `a=1,https://h/q?reset_token=S` → `a=1,https://h/q?reset_token=[Filtered]` (the value `1,https://h/q?reset_token=S`
   is filtered from its first `?`); `/login?next=%2Fcb%23access_token%3DX` → `/login?next=%2Fcb` (the nested URL's fragment is
   dropped by rule 1, as every fragment is). This rule exists for the **first** parameter after a nested `?`, which the outer
   split on `&` and `;` leaves inside the value, and for a nested fragment, which rule 1 never sees because it is encoded. A nested
   URL's later parameters (`/login?next=/r?page=2&reset_token=X`) are already separate parts of the outer split, and rule 3
   catches them there.
6. A string with no `?` and no `#` comes back unchanged except for rule 0 (`https://u:p@h/x` → `https://h/x`). The function
   never throws.

Two companions in the same module:

- `filterQuery(qs: string): string` — the same rules 2-5 applied to a **bare** query string (no leading `?`), e.g.
  `reset_token=abc&page=2` → `reset_token=[Filtered]&page=2`. Rule 5 applies here too, so a value holding a URL is filtered
  (`next=/reset-password?reset_token=X` → `next=/reset-password?reset_token=[Filtered]`). Every `request.query_string` the SDK
  builds goes through it (§6.1, §6.4). `filterUrl` is never called on a bare query, where its rule 6 would return it unfiltered.
- `filterUrlsInText(text: string): string` — every URL-shaped substring of free text has its base passed through
  `stripUserinfo` (rule 0), its query passed through `filterQuery` and its fragment **dropped** (rule 1, and contract §4.4's
  "Fragment (`#…`) is dropped").
  The pattern is the exported constant `URL_IN_TEXT` in `src/core/url.ts`:

  ```js
  /(^|[^\w/])(https?:\/\/[^\s"'<>?#]+|\/[^\s"'<>?#]*)(?:\?([^\s"'<>#]*))?(#[^\s"'<>]*)?/g
  ```

  Group 1 is the **captured prefix** — the start of the text, or one character that is neither a word character nor `/` — and is
  put back on replace. Group 2 is the base: an absolute http(s) URL up to its `?` or `#`, or a path starting with a single `/`.
  Group 3 is the query without its `?` (absent when there is no `?`); it ends at whitespace, a quote, `<`, `>` or `#`, and a
  comma is part of it like any other character. Group 4 is the fragment, `#` included, up to whitespace, a quote, `<` or `>`.
  The replacement is

  ```js
  text.replace(URL_IN_TEXT, (_m, prefix, base, query) =>
    prefix + stripUserinfo(base) + (query === undefined ? '' : '?' + filterQuery(query)))
  ```

  which writes group 4 nowhere. A match with neither a query nor a fragment is rebuilt as `prefix + stripUserinfo(base)`, which
  is the match itself unless its base carries userinfo, so a plain path or URL comes back unchanged and
  `fetch https://u:p@h/x failed` → `fetch https://h/x failed`. A protocol-relative `//u:p@h/x` matches as a path whose base
  starts with `//`, and rule 0 reaches it the same way. A comma-separated list of URLs is one query: `see /p?a=1,https://h/q?reset_token=S`
  is one match whose query is `a=1,https://h/q?reset_token=S`, and rule 5 filters the second URL's `reset_token` inside the value
  of `a`; `see /p?next=x,https://h/q&reset_token=S` is one match whose second part `reset_token=S` is filtered by rule 3.
  (Revision 4 ended the query at a comma followed by `http(s)://`; that left the second of these two with `reset_token=S`
  belonging to no match, unfiltered. The rule is gone, §12.7.) `go https://h/cb#access_token=S` → `go https://h/cb`.

  The captured prefix does what a lookbehind `(?<![\w/])` would (a `/` preceded by a word character or another `/` does not
  start a path) **without lookbehind**, which Safari only parses from 16.4 (W11): this function ships in the browser bundle,
  where a lookbehind literal would stop the whole chunk parsing on iOS ≤ 16.3. The prefix character is consumed but is never
  part of a base, query or fragment (each stops before whitespace, quotes, `<` and `>`), so two adjacent URLs separated by a
  space both match. **Linear time:** groups 3 and 4 are optional and every repetition is greedy over a class that cannot match
  the character that ends it, so a position either fails on its first one or two characters or matches without backtracking.
  The revision-3/4 shape required a `?` after the base, so text such as `-/-/-/…` with no `?` re-scanned to the end from every
  `/`: in node, 40 KB of it took ~300 ms, while the pattern above takes under 1 ms on 1 MB. That matters because the message
  rule below runs this over error messages of any length.

  Used for console messages (§5.2) and every event's `message` (below). Next 15 itself logs such a URL in production
  (`console.error("Failed to fetch RSC payload for " + url + ". Falling back to browser navigation.")`,
  `next/dist/client/components/router-reducer/fetch-server-response.js`), and `url` is the soft-navigation target with its query.

**Where the filters run on an event's `message`.** `build` (`src/core/capture.ts: build`) passes the event's `message` through
`filterUrlsInText` on **every** capture path — `captureException` (the message is the error's own `message`, from
`src/core/event.ts: classify`), `captureMessage`, the global `error` / `unhandledrejection` / `uncaughtException` handlers,
and Next's `captureRequestError` (§6.4) — before `boundAndShrink`, so `applyBounds` cuts the filtered string (a filter that
lengthens a value, `X` → `[Filtered]`, is still bounded by `MAX_MESSAGE_BYTES`). An error message is a token path the brief
names: `new Error('failed at ' + location.href)` on `/reset-password?reset_token=X` is sent as
`failed at https://app/reset-password?reset_token=[Filtered]`. The SDK sends no `exception` (§2), so the error's value has no
other place to travel; `error_class` is a class name and is not filtered. It costs no grouping: the processor's fingerprint
hashes environment, `error_class` and frames, never the message (`apps/processor-go/fingerprint/fingerprint.go: Compute`), so
an issue's identity is the same whether its message was filtered or not. A message that a `beforeSend` returns is not filtered
again (§3.8 keeps today's `messageText` and cuts); the app's hook owns what it writes.

The filters run before `beforeBreadcrumb` / `beforeSend`, so an app's stricter rule (web's, §9) sees already-filtered values.

**Filter, then cut.** Wherever this spec bounds a URL-bearing string, the filter runs on the **whole** string first and the cut
is applied to its output. This covers:
- each console argument (§5.2), and the event `message` (above, before `applyBounds`);
- `stacktrace[].file` (above, before the `MAX_FILE_BYTES` cut);
- `request.url` and `request.query_string` (§6.1, §6.4);
- the server `request.headers` values (§6.4).

A cut taken first can end inside a `%XX` escape or a key. Filtering that fragment then has two costs. It reads a shortened
value, which revision 7's strict decode kept raw whenever it ended in a partial escape. And a filter that lengthens a value
(`X` → `[Filtered]`, a re-encoded `%2` → `%252`) can push the result past the bound it was cut to. Filtered first, a cut can
only shorten text in which every sensitive value is already `[Filtered]`. web's strips (§9.4) run after the SDK's cuts and
need no such rule: they drop everything from the first `?` or `#`, and a cut cannot move that position.

**Regex syntax (every pattern this spec prescribes, SDK and web).** Everything the browser entries ship must parse and run on
Safari 12, the oldest browser in web's target list (W11); no transpiler lowers regex syntax. So: **no lookbehind** (`(?<=`,
`(?<!`), **no `v` flag**, **no `d` flag**, and no `\p{…}` / `\P{…}` (Safari 11.1 accepts them with `u`, but nothing here needs
them, and banning them keeps the check below a plain text scan). Named groups and the `s` / `u` flags would be fine (Safari
11.1) and are not used. **Lookahead** (`(?=`, `(?!`) is ES3 syntax and allowed, though no pattern here uses one; non-capturing
groups `(?:…)` are ES3 too. The audit of every pattern this spec prescribes:

| Pattern | Where | Safari 12 |
|---|---|---|
| `URL_IN_TEXT` above | §5.1 `filterUrlsInText`, §9.4 `stripQueriesInText` | yes — captured prefix, one ES3 non-capturing group, no lookaround, flags `g` only |
| the nested-value tests "contains `?`" and "its fragment has a sensitive key" | §5.1 rule 5 | no regex: `indexOf`, then rule 2's split and rule 3's key test |
| the repeated lenient decode (until stable, ≤ 3 steps) | §5.1 rules 2 and 5 | the `/\+/g` below, `/(?:%[0-9A-Fa-f]{2})+/g` for the escape runs, and `decodeURIComponent` in a `try` over each run or triplet sequence, in bounded loops — yes |
| the tag key `^[a-zA-Z0-9_.:-]{1,32}$` | §3.5 | yes |
| the environment `^[a-z0-9][a-z0-9._-]{0,49}$` | §3.8 (already in `src/core/init.ts`) | yes |
| the scheme test `^[A-Za-z][A-Za-z0-9+.-]*:\/\/` | §5.1 rule 0 `stripUserinfo`, §9.4 web's copy | yes |
| the query split on `&` and `;`, separators kept, e.g. `/([&;])/` | §5.1 rule 2 | yes |
| the key-segment split on `-` `_` `.` `[` `]`, e.g. `/[-_.[\]]/` | §5.1 rule 3 | yes |
| `+` → space before decoding, `/\+/g` | §5.1 rule 2 | yes |
| everything else ("up to the first `?` or `#`", "starts with `[@sentinel/sdk-js]`") | §5.2, §9.4 `stripUrl` | no regex: `indexOf` / `startsWith` |

Replacement is `String.prototype.replace` with a `g` pattern, never `String.prototype.replaceAll` (Safari 13.1), in the SDK and in
web's module alike — a `TypeError` inside web's `beforeSend` would send the event unstripped (§9.3). The sdk-js suite pins this
with `tests/core/es-compat.test.ts` and web's with `sentinel-url-privacy.test.ts` (§9.5, §11.2).

**Built-in API floor (every browser code path this spec prescribes, SDK and web; spec review round 9).** The regex rule above has
a runtime counterpart. A built-in the browser lacks is a `TypeError` when the line runs. Inside a §5 wrapper, §5's rule swallows
it and the crumb is silently lost. Inside web's `beforeSend`, the SDK answers it by sending the event unstripped (T6, §9.3). So
the browser code this spec prescribes calls nothing that Safari 12 lacks. Next 15.5's `@next/polyfill-module`
(`next/dist/build/polyfills/polyfill-module.js`, read from web's installed `next@15.5.26`) supplies exactly nine APIs:
`String.prototype.trimStart` / `trimEnd`, `Symbol.prototype.description`, `Array.prototype.flat` / `flatMap`,
`Promise.prototype.finally`, `Object.fromEntries`, `Array.prototype.at`, `Object.hasOwn` and `URL.canParse`. SDK code does
**not** count on them, because `./browser` is not used only under Next. web's module could count on them, and needs none of
them. Everything past Safari 12 that the prescribed code could reach for is banned on both sides:

| API | Safari from | Where the spec's code might reach for it, and what it does instead |
|---|---|---|
| `structuredClone` | 15.4 | the crumb copy at capture (§4.4): `{ ...crumb }`, with `data` through `sanitize` |
| `String.prototype.toWellFormed` / `isWellFormed` | 16.4 | §3.10's lone-surrogate repair: a loop over `charCodeAt` |
| `Array.prototype.at`, `String.prototype.at` | 15.4 (Next polyfills the `Array` one only) | index with `[length - 1]` |
| `Array.prototype.findLast` / `findLastIndex` | 15.4 | a reverse `for` loop |
| `Object.hasOwn` | 15.4 (Next polyfills it) | `Object.prototype.hasOwnProperty.call` |
| `Object.fromEntries` | 12.1 (Next polyfills it) | the event's `tags` and `contexts`, built from the scope's `Map`s (§3.5, §3.6) by `forEach` into a fresh object |
| `String.prototype.replaceAll` / `matchAll` | 13.1 / 13 | `replace` with a `g` pattern (above); `exec` in a loop |
| `Promise.allSettled` / `Promise.any`, `AggregateError` | 13 / 14 / 14 | nothing here needs them |
| `WeakRef`, `FinalizationRegistry` | 14.1 | nothing here needs them (§5.4's `WeakMap` is in Safari 8) |
| `URL.canParse` | 17 (Next polyfills it) | `new URL` inside a `try` |
| `Array.prototype.toSorted` / `toReversed` / `toSpliced`, `Object.groupBy` | 16 / 17.4 | `slice()` then `sort()` (§3.10's victim order) |

`Symbol.prototype.description` (12.1) is not used either. The scan leaves it out because `.description` is too common a name to
scan for. Everything else the prescribed code uses is in Safari 12: `Map`, `Set`, `WeakMap`, `Symbol.for`, `TextEncoder` (§4.6's
`utf8Bytes`), `String.prototype.codePointAt` (§3.10's `compareCodePoints`), `Array.from`, `Object.entries`,
`Array.prototype.includes`, `Intl.DateTimeFormat().resolvedOptions().timeZone` (§6.1), `URL`, `history.pushState`,
`XMLHttpRequest`, `Element.classList` and `performance.now` (§5). 0.1.x's own browser code already goes past Safari 12 in four
places, which this change neither fixes nor extends (§13 F6). `tests/core/es-compat.test.ts` pins the table. Its scan (§11.2
TASK-03) fails on `structuredClone(`, `.toWellFormed(`, `.isWellFormed(`, `.at(`, `.findLast(`, `Object.hasOwn(`, `Object.fromEntries(`, `.matchAll(`, `Promise.allSettled(`, `Promise.any(`, `AggregateError`, `WeakRef`, `FinalizationRegistry`, `URL.canParse(`, `Object.groupBy(`, `.toSorted(`, `.toReversed(` and `.toSpliced(`, as it already fails on `replaceAll(`. web's scan of its module does the same (§9.5).

### 5.2 Console

- Wraps `console.debug`, `info`, `log`, `warn`, `error`, `assert`, `trace` present at install time.
- Crumb: `{ type: 'default', category: 'console', level, message, data: { logger: 'console' } }`; level map `debug → debug`,
  `info → info`, `log → info`, `warn → warning`, `error → error`, `trace → debug`, `assert → error`. `console.assert` records only
  when its first argument is falsy, with message `Assertion failed: ` + the remaining arguments rendered (or `Assertion failed`).
- **Argument bounds:** at most the first **10** arguments; each rendered as: a string as is; a number, boolean, `null`,
  `undefined`, `bigint` as `String(v)`; an `Error` as `name: message`; any other object by `sanitize` → `scrubPII` (objects) →
  `JSON.stringify`, `[unserializable]` on failure; each rendered argument passed **whole** through `filterUrlsInText` (§5.1)
  and **then** cut to **512 UTF-8 bytes** on a code-point boundary (§5.1 *Filter, then cut*); joined with a single space; the
  whole message cut to **2048 UTF-8 bytes**. The join needs no second filter: `URL_IN_TEXT` stops at whitespace, so no URL spans
  two arguments. Revision 7 cut each argument first and filtered the joined message. An argument cut inside a `%XX` after a
  nested token was then read raw by the strict decode, and the token was kept. Arguments beyond the tenth are counted in
  `data.arguments_omitted` (a number) when there are any. Raw arguments are **not** copied into `data`.
- Never recorded: a call whose first argument is a string starting with `[@sentinel/sdk-js]` (the SDK's own `debug` / route lines,
  `src/core/report.ts`), and anything logged while the re-entrancy flag is set.
- Hint: `{ input: <the arguments array>, level: <console method name> }`.

### 5.3 fetch

- Wraps `globalThis.fetch` when it is a function. It returns the **chained** promise described in §5 *No wrapper marks a page
  promise handled*, never the original promise object. The settle-time recording below happens inside that promise's two
  reactions, so an uncaught failed `fetch` still fires exactly one `unhandledrejection`.
- Method: `init.method`, else the `Request`'s `method`, else `GET`; upper-cased, cut to 16 code points. URL: the `Request`'s `url`,
  else `String(input)`; resolved against `location.href`; then `filterUrl`. **No header, no body, no body size** is read — the
  wrapper never touches `init.body`, `init.headers`, `Request.body`, `Response.body` or `Response.headers`, and never clones.
- On settle: `{ type: 'http', category: 'fetch', level, data: { method, url, status_code?, duration_ms } }`. `status_code` is
  `response.status` (a number); `duration_ms` is an integer (`Math.round(performance.now() delta)`, `Date.now()` when
  `performance` is absent). Level: status ≥ 500 → `error`, ≥ 400 → `warning`, else `info`; a rejected fetch → `error`, no
  `status_code`. The timestamp is the settle time. No `message`, so no URL is ever duplicated outside `data`.
- **Not recorded:** a request whose resolved origin + pathname equals the client's transport target URL's origin + pathname (the
  tunnel in the browser, `InitPlan.target.url`). This covers the SDK's own sends and any app fetch to the tunnel.
- Hint: `{ input: <fetch arguments> }` on the request side; `{ response }` or `{ error }` added on settle. The SDK itself never reads
  the response; an app hook may.

### 5.4 XMLHttpRequest

- Patches `XMLHttpRequest.prototype.open` (records method and URL on a `WeakMap` keyed by the instance, with §5.3's method and URL
  rules) and `send` (records the start time and adds one `loadend` listener). **Never** patches `setRequestHeader`; never reads
  the `send` argument, `response`, `responseText` or response headers.
- `open` and `send` return the original's return value unchanged, and anything either throws reaches the caller unchanged (for
  example `send`'s `InvalidStateError` on an unopened request, in which case nothing is recorded). No promise is created or
  reacted to. The `loadend` listener is passive and never cancels or stops the event (§5 *No wrapper marks a page promise
  handled*).
- On `loadend`: `{ type: 'http', category: 'xhr', level, data: { method, url, status_code?, duration_ms } }`; `status === 0`
  (network error / abort) → no `status_code`, level `error`; otherwise the fetch level map. Same self-exclusion as fetch.
- Hint: `{ xhr }`.

### 5.5 Navigation

- Wraps `history.pushState` and `history.replaceState`, and listens to `window` `popstate`. Next's App Router navigates through
  these (W9), so `router.push` / `router.replace` are covered without a Next hook (`onRouterTransitionStart` stays a no-op, T11).
- `from` is the last recorded href (initialised to `location.href` at install); `to` is: for push/replace, the URL argument resolved
  against `location.href` when it is a string or `URL`, else `location.href` after the call; for `popstate`, `location.href`. The
  wrapper calls the original **first**, then records, then sets the last href to `to`.
- Both are passed through `filterUrl`; a same-origin URL is recorded relative (path + query), a cross-origin one absolute
  (Sentry's rule). A transition where the filtered `from` equals the filtered `to` is **not** recorded (web's back guard pushes
  the same URL, W9).
- Crumb: `{ type: 'navigation', category: 'navigation', level: 'info', data: { from, to } }`. No initial page-load crumb.
- Hint: `{ from, to, kind: 'pushState' | 'replaceState' | 'popstate' }` (unfiltered — an app hook may want them; the stored crumb is
  filtered).

### 5.6 Click

- One `click` listener on `document`, capture phase, passive.
- Crumb: `{ type: 'default', category: 'ui.click', level: 'info', message: <selector> }`, no `data`.
- **Selector shape:** walk from `event.target` (an `Element`; a text node's parent) up through `parentElement`, at most **5**
  elements, stopping before `html`/`body`. Each element renders as the lower-case tag name, then `#` + its `id` when non-empty,
  then `.` + each class of `classList` in order; ids and class names are emitted as they are (no escaping, since the string is
  display text, not a query). Elements are joined root-most first with ` > `. When adding the next ancestor would make the whole
  string exceed **80 characters**, the walk stops (the target itself is always included, cut to 80). No attribute other than `id`
  and `class`, no text content, and **never** `value`, `placeholder`, `name`, `aria-*` or `data-*`.
- The same target clicked again within **1000 ms** is recorded once (Sentry's debounce).
- Hint: `{ event }`.
- No input breadcrumbs (`keypress` / `input`) are recorded at all, so an input's value cannot reach Sentinel through this
  integration.

---

## 6. Automatic contexts per runtime (TASK-04)

Built at capture time and merged under the app's `setContext` values (§3.6). Each reader is guarded (`try/catch`, a missing API is
skipped), and runs only in the entry for its runtime, never at import (import-time inertness, contract §4.7 of `sentinel-sdk-js`).

### 6.1 Browser (`./browser`, `./nextjs/client`)

| Field | Value |
|---|---|
| `request.url` | `location.origin + location.pathname` (no query, no fragment), through `filterUrl`, then cut to 2048 code points (§5.1 *Filter, then cut*) |
| `request.query_string` | `location.search` without the leading `?`, through `filterQuery` (§5.1) **whole**, then cut to 4096 code points; omitted when empty |
| `request.headers` | `{ "User-Agent": navigator.userAgent }` when non-empty, cut to 4096 code points. **No `Referer`** (D6). |
| `contexts.culture` | `{ locale: navigator.language, timezone: Intl.DateTimeFormat().resolvedOptions().timeZone }`, each key only when a non-empty string |
| `contexts.device` | `{ screen_width_pixels, screen_height_pixels, screen_density, memory_size, processor_count }` where exposed: `screen.width` / `screen.height` (integers), `devicePixelRatio` (number), `navigator.deviceMemory × 1 073 741 824` (bytes, integer), `navigator.hardwareConcurrency` (integer); omitted when none is exposed |
| `contexts.browser`, `contexts.os` | **not sent** (D2); the ingestor fills both from `request.headers["User-Agent"]` |

Worst case of `user` + `sdk` + `level` + this `request`, serialised (a control character escapes to 6 bytes, the worst case per
code point): `user.id` ≤ 255 bytes (escapes count: ≤ 1 530), `username` ≤ 128 × 6, `email` ≤ 254 × 6, `ip_address` ≤ 45 × 6,
`request.url` ≤ 2048 × 6, `query_string` ≤ 4096 × 6, the UA ≤ 4096 × 6 = 65 952 bytes, plus under 400 bytes of keys, `sdk` and
`level` — under **67 KB**, far from 262 142, which is why §4.5 rule 5 does not shed them. (The SDK cuts `request.url` and
`query_string` to 2048 / 4096 code points, the server's bounds, after filtering them; `location` values are percent-encoded, so the realistic figure is a
sixth of this.)

### 6.2 Node (`./node`, `./nextjs/server`)

| Field | Value |
|---|---|
| `contexts.runtime` | `{ name: 'node', version: process.version }` |
| `contexts.os` | `{ name, kernel_version }`: `name` from `os.platform()` mapped `linux → Linux`, `darwin → macOS`, `win32 → Windows`, else `os.type()`; `kernel_version = os.release()` |
| `server_name` | option `serverName` (`NodeInitOptions` only) when a non-empty string, else `os.hostname()`; cut to 256 code points; a non-string option degrades to the hostname with one `invalid_option` |

`node:os` is imported by the node entry only (`src/node.ts`), which no browser or edge bundle resolves (`package.json` exports
conditions, T11); reading it happens inside `init` / capture.

### 6.3 Edge (`./edge`, `./nextjs/edge`)

`contexts.runtime = { name: typeof EdgeRuntime === 'string' ? EdgeRuntime : 'edge' }` (Next defines the global `EdgeRuntime`),
no `version` (the edge exposes none). No `os`, no `server_name` (there is no host to name).

### 6.4 Next `captureRequestError` (server and edge builds)

Replaces T10's `metadata.http_method` and `metadata.http_url`:

| Field | Value |
|---|---|
| `request.method` | `errorRequest.method` when a string, cut to 16 code points |
| `request.url` | `errorRequest.path` up to its first `?` or `#` (a relative URL; Next gives no scheme/host, and the spec does not guess them), through `filterUrl`, then cut to 2048 code points (§5.1 *Filter, then cut*) |
| `request.query_string` | the part of `path` after `?` (and before `#`), through `filterQuery` (§5.1) **whole**, then cut to 4096 code points; omitted when empty |
| `request.headers` | `errorRequest.headers` as `string → string`: an array value joined with `, `; `undefined` skipped; names **dropped** when, lower-cased, they are `authorization`, `cookie`, `set-cookie`, `proxy-authorization` or `x-api-key` (contract §4.3); a name that `isSensitiveKey` (T4) matches keeps its key with the value `[Filtered]`; **every other value containing `?` or `#`** — `referer`, `next-url`, `x-envoy-original-path`, `x-original-url`, whatever the app's proxies add — through `filterUrl` (D6) on the whole value; then, after the filter (§5.1 *Filter, then cut*), bounded by §3.10's `boundStringMap`, the server's rule: the first 64 names by code point, name 1-256 code points (else dropped), value cut to 4096 code points |
| `transaction` | `errorContext.routePath` when a string, cut to 200 code points |
| `metadata` | `route`, `router_kind`, `route_type`, `render_source`, `revalidate_reason` **stay** as today (the contract replaces only the `http_*` keys; `route` is an ordinary key to the metadata shrink, as it is to the server's, §3.7) |
| `ctx.traceparent` | unchanged |

The runtime contexts of §6.2 / §6.3 are added as for any capture.

---

## 7. The tunnel forwards the typed fields (TASK-01, with TASK-04)

### 7.1 `ITEM_KEYS`

`ITEM_KEYS` (`src/nextjs/route.ts`) gains **exactly** these seven keys — the ones a browser event now carries (§3, §4, §6.1):

```
level  sdk  user  tags  contexts  request  breadcrumbs
```

giving `event_id platform environment message error_class trace_id span_id stacktrace metadata timestamp trace_flags
release_version fingerprint fingerprint_override level sdk user tags contexts request breadcrumbs`.

Not admitted, with the reason: `transaction`, `server_name` (the browser never sends them; server runtimes post directly);
`exception`, `logger`, `dist`, `source` (no producer in this feature, §2); `project_key`, `project_id` (deleted, tenant scope comes
from the key — unchanged); `received_at` and anything else (not `ErrorPayload` keys, or server-owned).

Unchanged, and asserted by tests (§11.2):

- The top-level filter stays **exact-key** (`Set.has` over `Object.keys`), not case-insensitive: `USER`, `uſer`, `Tags` beside
  their exact spellings are dropped, so Go's case-folding decoder never sees a second candidate (enrichment spec T28).
- `applyClientIp` still runs **after** `normaliseBody`, on its output: `user` is rebuilt from the exact `USER_KEYS` string values
  and `ip_address` is written last; a browser-sent `ip_address` (including `{{auto}}`, §3.4) or `geo` never survives.
- **One change to `applyClientIp`, forced by admitting `user`:** its case B4 (no derived IP, `user` a plain object) now deletes
  `user` when the rebuilt object has no key, instead of forwarding `user: {}`. Before this feature `ITEM_KEYS` removed `user`, so
  B4 was reachable only through the exported function; now every browser event with `{{auto}}` and no identity reaches it through
  the route. The invariant the enrichment tests state — "no IP derived: no user at all" — therefore stays true for such items, and
  an item with identity keeps `{ id?, username?, email? }` without an IP.
- `normaliseBody` still checks only the shape (`metadata` an object); a wrong type inside a new key is the ingestor's to reject,
  and it rejects only that browser's own batch.
- The body limit (262 144), `maxItems` (50), the forward headers (`x-sentinel-derive-ip: off`, the browser's `user-agent`), the rate
  limit and the IP derivation do not change (§4.5 says why the limit need not).

One consequence the spec reviewer should see (§12.2): the ingestor's UA source for a tunnelled event becomes the body's
`request.headers["User-Agent"]` (S4 reads the body first), which the browser writes; before, it was the tunnel's forwarded
`user-agent` header, which the browser also writes. Same trust, same value.

### 7.2 Tests (sdk-js vitest)

In `tests/route/route.test.ts`: each of the seven keys other than `user` survives a forward unchanged (deep-equal), `user` is
rebuilt as above (with an IP; without one, deleted when empty and kept as `{ id, username, email }` otherwise), and each of the not-admitted
keys above is absent; case-folded spellings (`USER`, `Tags`, `Breadcrumbs`, `uſer`) are dropped; a v2 item's `user` with
`ip_address: '{{auto}}'`, `IP_ADDRESS`, `geo` and an extra key forwards as `{ id, username, email, ip_address: <derived> }`
exactly; the order is proven by an item whose `user` would only be rebuilt if `applyClientIp` saw `normaliseBody`'s output.

### 7.3 Fixtures and the Go replay

- **Every committed fixture is machine-independent.** CI writes nothing and compares byte-for-byte on node 20 and node 22
  (`tunnel-fixtures.test.ts: commit()`), and node ≥ 21 has a global `navigator` that node 20 lacks. So the automatic contexts are
  read through an injectable provider: the entry sets `InternalClient.autoContexts: () => AutoContexts` (the readers of §6), and
  the test helpers accept a fixed provider. Both fixture suites install fixed values — User-Agent the existing
  `CHROME_WINDOWS_UA`, locale `en-US`, timezone `Asia/Jakarta`, screen 1920 × 1080 @ 1, memory 8 GiB, 8 processors, page
  `https://app.example/dashboard?tab=1`; node `v20.0.0`, `Linux` / `6.0.0`, host `fixture-host` — and `vi.useFakeTimers` for every
  timestamp, as the existing fixtures do. A fixture that differs between node 20 and 22 is a defect of this spec's proof.
- `tests/tunnel-fixtures.test.ts` adds route fixture **`tunnel-v2-event`**: a browser-runtime event built by the real pipeline
  with `setUser({ id, username, email })`, `setTag`, `setTenant`, `setContext`, `autoIpAddress` on, two manual breadcrumbs and one
  of each automatic kind's shape, `level`, `sdk` and the §6.1 `request`, forwarded by the real route handler with a public
  `X-Forwarded-For`. Written and checked under the existing rules (never written when `CI` is set; the unit gate's `git diff` /
  untracked checks catch drift, T14).
- `tests/unit/sdk_js_tunnel_fixture_test.go`: `tunnel-v2-event` joins `tunnelRouteFixtures` (so the existing shape and replay
  tests cover it). `TestTunnelFixtureShape`'s `user` assertion changes from "exactly `{ip_address}`" to "exactly the inbound
  item's `USER_KEYS` string values plus `ip_address` = `expect.client_ip`" (and "no `user`" when no IP was derived and the inbound
  item carried no identity), since `user` now passes the filter. Then a new **`TestTunnelFixtureV2StrictDecode`** decodes the `forwarded.body` of **every** route fixture into
  `[]validation.ErrorPayload` with `DisallowUnknownFields` (D4), then asserts on `tunnel-v2-event`: `level`, `sdk.name`
  (`sentinel.javascript.browser`), `user.id` / `username` / `email`, `user.ip_address` = the fixture's derived IP, `tags.tenant_id`
  and the custom tag, the `setContext` context, `request.url` / `request.headers["User-Agent"]`, every breadcrumb's `timestamp`
  decoded as a `time.Time`, and — after `enrich.Apply` — `contexts.browser` / `contexts.os` filled from the body's User-Agent
  (D2's proof on the server side).
- `tests/fixtures.test.ts` (the golden corpus) adds **`v2-rich-context.json`** (the same scope calls, node runtime so no
  `{{auto}}`), which `TestSDKJSFixtures_AcceptedByIngestor` picks up by glob, and `TestSDKJSFixtures_PinnedValues` gains a subtest
  pinning: every typed field in its typed place, and `metadata` having no `user_id`, no `tenant_id` and no tag key. Every existing
  fixture regenerates (each now carries `level` and `sdk`). `metadata-near-the-cap` passed its exempt key as per-call
  `ctx.metadata.user_id`, which §3.7 now drops; it passes `request_id` instead, which is in `METADATA_EXEMPT_KEYS` because the
  server exempts it (`route` no longer is, §3.7), and its Go subtest pins `payload.Metadata["request_id"]` in place of
  `["user_id"]`. The other pinned subtests are unchanged.
- The golden corpus also adds **`v2-hostile-before-send.json`** (node runtime): an event built through the real pipeline with
  `setContext('ok', { a: 1 })`, one manual breadcrumb, and a `beforeSend` that returns the event with `logger: 5`, `dist: {}`,
  `source: []`, `exception: 'x'`, `project_key: 7`, `fingerprint_override: 'yes'`, `LEVEL: 7`, `Tags: 'x'`, `environment: 42`,
  `release_version: {}`, `fingerprint: 'f'.repeat(65)`, `trace_id: 5`, `trace_flags: -1`, `stacktrace[0].line = 1e12`,
  `contexts.when = new Date()`, `contexts.url = new URL('https://h/p')`, `contexts.arr = { toJSON: () => [1] }`,
  `breadcrumbs[0].data = new URL('https://h/p')`, `request = { headers: { a: 1 } }` and a `toJSON` on the returned object itself.
  `TestSDKJSFixtures_AcceptedByIngestor` picks it up by glob, so it is decoded with `DisallowUnknownFields`, run through
  `ValidatePayload`, `MapPayloadToEvent` and protovalidate — the proof of §3.8's guarantee, on the server's own code. A pinned
  subtest asserts: `Logger`, `Dist`, `Source` empty and `Exception` nil; `Environment` the pre-hook value; `ReleaseVersion`,
  `Fingerprint`, `TraceID` empty and `TraceFlags` 0; `Stacktrace[0].Line` 0; `Contexts` has `ok` and none of `when`, `url`,
  `arr`; `Breadcrumbs[0].Data` nil; `Request` nil (its only member was removed, so the key is omitted, §3.1).
- The golden corpus also adds **`v2-publish-bound.json`** (node runtime): the event `enqueue` sends for spec review round 7's
  case (§11.2 TASK-02's first size case), after §4.6 has shed its crumbs. It is about 182 KB of JSON, most of it zeros.
  `TestSDKJSFixtures_AcceptedByIngestor` picks it up by glob. A pinned subtest runs `ValidatePayload` and `MapPayloadToEvent`,
  then asserts `proto.Size(event)` ≤ 1 000 000 and > 950 000, so the fixture exercises the bound rather than passing under it
  (this revision's model: 28 crumbs, 986 582 bytes). It also asserts fewer than 31 breadcrumbs and `metadata.truncated` containing `breadcrumbs`. That is §4.6's guarantee on the
  server's own encoder. It is shown red by deleting §4.6: the fixture then carries 31 crumbs and measures over 1 048 576 bytes
  (1 053 681 in this revision's model, whose stack has one frame).
- **Every fixture's estimate is committed and checked against Go's `proto.Size`** (spec review round 8). §4.6's guarantee rests
  on `protoSizeEstimate` being an upper bound, and until now only one fixture compared it with the Go size. So:
  - The two JS suites write each fixture's `protoSizeEstimate` to a committed sidecar, one file per fixture, under
    `packages/sdk-js/tests/fixture-estimates/`. `golden/<name>.json` is `{ "protoSizeEstimate": <n> }` for the event in
    `tests/fixtures/<name>.json`, computed by `src/core/proto-size.ts` over the event `emit` wrote. `tunnel/<name>.json` is
    `{ "protoSizeEstimate": [<n>, …] }`, one per item of the route fixture's `forwarded.body`, computed over the forwarded
    items, which are what the ingestor receives, after the route's `applyClientIp`. It covers every route fixture
    (`tunnelRouteFixtures`, including `tunnel-v2-event`). `tunnel-user-rebuild.json` is excluded: it is a table of
    `applyClientIp` inputs and outputs, not a forward, and it stays on its own test (§13 F2). The directory is outside both Go
    globs, so no sidecar is ever decoded as an `ErrorPayload` under `DisallowUnknownFields`. One file per fixture means a
    filtered or shuffled run never writes a partial map.
  - The sidecars are written under `tunnel-fixtures.test.ts`'s `commit()` rules, which both suites now share
    (`tests/helpers/commit.ts`). Under `CI` a sidecar is never written: it must exist and equal what the suite computed, or the
    test fails. Otherwise it is written only when it changed.
  - The drift gate covers them. `Taskfile.yml: test-unit`'s `git diff --exit-code` and its untracked-file check gain
    `packages/sdk-js/tests/fixture-estimates` and `packages/sdk-js/tests/fixtures`, and so does `ci.yml`'s `sdk-js-node` step
    *tunnel fixtures committed and unchanged*, with the job-relative pathspecs `tests/fixture-estimates` and `tests/fixtures`.
    The golden corpus was not under that gate before. Its `emit` rewrites a changed fixture even under `CI`, and `go test`
    runs before the vitest suite in `test-unit`, so a pipeline change could leave Go replaying a stale corpus.
  - `TestSDKJSFixtures_AcceptedByIngestor` gains one assertion per golden fixture, after protovalidate. The sidecar must exist.
    The test clones the mapped event, clears `ProjectKey` and `ProjectId`, which `sdkJSStampAuthenticatedScope` stamped (the SDK
    never sends them, and §4.6's budget covers what the ingestor adds), and asserts `proto.Size(clone) ≤ protoSizeEstimate`.
    `TestTunnelFixtureV2StrictDecode` does the same for each item of each route fixture's forwarded body: stamp as the golden
    test does, `ValidatePayload`, `MapPayloadToEvent`, clear the two fields, then compare with `protoSizeEstimate[i]`. It also
    requires as many estimates as items. A new `TestFixtureEstimatesHaveFixtures` fails on a sidecar whose fixture no longer
    exists.
  - Two golden fixtures are added so the comparison has teeth, both on the node runtime, and both modelled for this revision.
    **`v2-estimate-numbers.json`** is number-heavy: `setExtra('n', Array(5_000).fill(0))`,
    `setExtra('f', [0.1, -0, 1e21, 123456789.125])`, `setContext('m', { a: Array(2_000).fill(1.5), nested: { x: [[0, 1],
    { y: false }] } })`, and 10 crumbs with `data: { a: Array(200).fill(0), b: -1.5e-7, c: true, d: null, e: 2 ** 53 }`.
    The model estimates 101 305, Go measures 100 404, and the JSON is 24 141 bytes, so a JSON-byte measure goes red.
    **`v2-estimate-strings.json`** is string- and header-heavy, sent by `captureMessage('é'.repeat(100))` so there is no stack
    to add slack. It has `setTags` `t00`…`t49`, `setContext` `c00`…`c29` of `{ s: … }`, and a `beforeSend` adding
    `request.headers` `h00`…`h63` and `request.env` `e00`…`e63`, every value `'é'.repeat(100)`. The model estimates 46 203
    against Go's 44 803. **Shown red two ways:** without the `8 ×` map-entry term the estimate is 44 523, and with the skeleton
    counted in UTF-16 units instead of UTF-8 bytes it is 28 303. Both are below Go, so the Go assertion fails. Its pinned
    subtest asserts 50 tags, 32 contexts, 64 headers and 64 env entries, and that
    `protoSizeEstimate − 8 × (the number of map entries) < proto.Size(clone)`. So the fixture keeps proving the `8 ×` term is
    needed after it regenerates. The golden replay also covers `v2-rich-context`, `v2-hostile-before-send` and
    `v2-publish-bound`, and the tunnel replay covers `tunnel-v2-event`.

---

## 8. Release (TASK-05)

- `package.json` `version` `0.2.0`; `src/core/version.ts` `SDK_VERSION = '0.2.0'` (§3.3); the lockfile is regenerated with pnpm
  **10.34.5** only if a dependency changes (none is expected: zero runtime dependencies, and `jsdom` is already a devDependency).
- `CHANGELOG.md`: the heading *Unreleased — the tunnel writes the browser's IP (ships with the next tag, planned v0.2.0)* becomes
  part of a `## v0.2.0 — …` entry (first `## ` entry, which `tests/docs.test.ts` requires to name the package version), with a
  **Behaviour change** section first: tags, `user_id`, `tenant_id` and `level` leave `metadata`; what replaces each; that 0.1.x
  apps keep working because the server accepts both shapes; the new options (`maxBreadcrumbs`, `beforeBreadcrumb`, `breadcrumbs`,
  `autoIpAddress`, `serverName`) and functions; the `ITEM_KEYS` widening; the URL filter and its key list; the `[Filtered]` vs
  `[FILTERED]` spellings; the new `metadata.truncated` values `breadcrumbs`, `contexts`, `tags` (§4.5, §4.6, §3.5 rule 6); the
  NATS publish bound (§4.6: what is shed, in what order, and the 1 000 000-byte budget, which assumes the ingestor adds at most
  ~48 KB after the SDK); the metadata shrink's new end (§3.7: the server's 18 exempt keys replace `SHRINK_EXEMPT_KEYS`, `route`
  is no longer exempt, the exempt keys are removed last instead of the event being dropped, `truncated_keys` is written in the
  server's capped shape, the first 50 names cut to 128 bytes and then `+N more`, and `metadata_too_large` is no longer emitted); the SDK's own 50-tag cap, which keeps
  `tenant_id` (§3.5 rule 6); the SDK's own contexts bound (32 names, 64 KiB, custom names before well-known ones, largest first)
  and headers / env bound (64 entries, values cut to 4096 / 1024 code points), the server's rules applied before sending
  (§3.10); D2's reason for not sending `contexts.browser` / `os`.
- `README.md`: *What is not implemented* loses **Breadcrumbs**; keeps tracing, per-request server scope (its workaround becomes `user: { id }` / `tags: { tenant_id }` on the capture call,
  §3.7), `withSentinelErrorBoundary`, index-map `url` sections; gains sections for scope APIs, breadcrumbs and each automatic
  integration's switch, URL privacy (the key list, the `&` and `;` separators, nested URLs and fragments, the `message` filter,
  userinfo removal, and a *Not filtered* list naming each residual of §12.2(3) with its reason and example, including the keyless
  query part, the nested query without `?` (`/login?next=reset_token%3DX`), the bare fragment in text
  (`hash is #access_token=X`), and a URL written into text unencoded (`GET /api/search?q=John Doe&token=abc`)), contexts per
  runtime (with the rule that the SDK removes contexts over the server's 32 names / 64 KiB itself, largest custom context
  first, and marks the event, §3.10), the size rules of §4.5 and §4.6, and `autoIpAddress` with §3.4's three bullets verbatim. *Bundle size*
  records the new `sdk_js_client_entry_gzip_bytes`, still report-only.
- `docs/sdk-specification.md`, sdk-js rows only: the `max_breadcrumbs` row (~L37) says sdk-js implements it as `maxBreadcrumbs`,
  default 100; the `platform` row keeps "`metadata.runtime`"; §5 gains the URL filter.
- The tag `packages/sdk-js/v0.2.0` is pushed on the sentinel **merge** commit, after merge (contract decision 3, be.md). The
  `v0.2.0-rc.N` tags are the coordinator's. The coordinator records the tag's SHA from `git ls-remote` on TASK-05 (§11.2
  *Proofs no gate can hold*).

---

## 9. web adopts it (TASK-06, side `fe`)

### 9.1 The pin (contract decision 3)

1. While building: `package.json` →
   `"@sentinel/sdk-js": "git+https://github.com/NurfitraPujo/sentinel.git#packages/sdk-js/v0.2.0-rc.N&path:packages/sdk-js"` (the `N`
   the coordinator names), `pnpm install` with the repo's pnpm **10.13.1** (`packageManager`), commit `package.json` and
   `pnpm-lock.yaml` (its `specifier` and resolved `commit` change, W1).
2. After the sentinel PR merges and `packages/sdk-js/v0.2.0` is tagged on its merge commit: the same two files move to
   `#packages/sdk-js/v0.2.0&path:packages/sdk-js`, nothing else in that commit. **web never merges on an rc tag**; the web PR's
   description states the pinned tag, and review checks `package.json` names `v0.2.0` before merge.

### 9.2 User, tenant, context (`src/layouts/dashboard/shell/index.tsx`)

In the existing `useDidMount` on `[me]`, inside the existing `if (!impersonation)` (W3), replacing the two calls:

```ts
Sentinel.setUser({ id: me.id, email: me.email, username: me.name });
Sentinel.setTenant(me.companyId);
Sentinel.setContext('tenant', { id: me.companyId, name: me.company?.name });
```

`username` is `me.name` and `email` is `me.email` (decisions 1, 6). `User.isEmailPrivate` exists (W4) and is not consulted:
decision 6 sends the email, and decision 1 covers storing it (§12.2 records this for the reviewer). The context's `name` key is
omitted by the SDK's serialiser when `me.company` is absent (`undefined` values do not serialise). While impersonating nothing is
called, as today.

`src/common/utils/credential.ts: endSession` gains `Sentinel.setContext('tenant', null)` beside `setUser(null)` and
`setTenant(null)` (which now clears `tags.tenant_id`).

### 9.3 Init files

`instrumentation-client.ts`, `sentinel.server.config.ts`, `sentinel.edge.config.ts` each pass
`beforeBreadcrumb: stripBreadcrumbUrls` and wrap their existing `beforeSend`: the 401/403 filter (on `hint.originalException`)
runs first and is unchanged in all three; then `stripEventUrls(event)` runs **before anything that can throw**; then, in the client
init only, the `sentry_replay_id` copy, now inside `try { … } catch { /* keep the event */ }`. The SDK sends the **pre-hook** event
when `beforeSend` throws (T6), so a throw after the strip would otherwise send the unstripped event; with the strip first and the
only foreign call guarded, no path of web's hook throws. No other option changes (automatic breadcrumbs stay on at their defaults;
`autoIpAddress` stays at its default, which the tunnel overrides anyway, §3.4). `withSentryConfig(withSentinelConfig(...))` in
`next.config.mjs` is untouched (`scripts/__tests__/next-config-security.test.mjs`).

### 9.4 URL stripping, stricter than the SDK (decision 5)

A new module `src/common/utils/sentinel-url-privacy.ts` (no SDK import beyond types):

- `stripUrl(url: string): string` — returns the string up to its first `?` or `#`, with its userinfo removed by the module's own
  `stripUserinfo`, a copy of §5.1 rule 0 (`https://u:p@h/x?y=1` → `https://h/x`). A value with neither and no userinfo is
  returned unchanged.
- `stripQueriesInText(text: string): string` — every URL-shaped substring loses its `?…` part and its `#…` part entirely. The
  module exports its own copy of the SDK's pattern (§5.1) as `URL_IN_TEXT` —
  `/(^|[^\w/])(https?:\/\/[^\s"'<>?#]+|\/[^\s"'<>?#]*)(?:\?([^\s"'<>#]*))?(#[^\s"'<>]*)?/g` — and replaces with the
  **captured prefix put back**, the base's userinfo removed, and nothing after the base:
  `text.replace(URL_IN_TEXT, (_m, prefix, base) => prefix + stripUserinfo(base))`.
  A comma-separated list after a query is part of that query and goes with it: `see /p?a=1,https://h/q?reset_token=S` →
  `see /p`. Dropping the second URL's base with it is harmless (web keeps no query anyway), and it is what keeps
  `see /p?next=x,https://h/q&reset_token=S` → `see /p`; revision 4's comma rule turned that into `see /p,https://h/q&reset_token=S`
  and is gone (§12.7). A URL nested in a query value needs no rule of its own here, because the whole query goes:
  `stripUrl('/login?next=/reset-password?reset_token=X')` is `/login`. A fragment goes too: `go https://h/cb#access_token=S` →
  `go https://h/cb`. The pattern has no lookaround. **No
  lookbehind**, no `v` / `d` flag, no `\p{…}`, and `replace`, not `replaceAll` (§5.1 *Regex syntax*), and no built-in newer than
  Safari 12 (§5.1 *Built-in API floor*): this module is imported by
  `instrumentation-client.ts`, so it is in web's client bundle, which targets Safari 12 (W11) — a lookbehind literal would be a
  parse-time `SyntaxError` that stops web hydrating on iOS ≤ 16.3, and a lookbehind built with `new RegExp` would throw inside
  `beforeSend`, which the SDK answers by sending the **unstripped** event (T6).
- `stripBreadcrumbUrls(crumb)` — returns the crumb with `data.url`, `data.from` and `data.to` passed through `stripUrl` when they are
  strings, and `message` through `stripQueriesInText` (console crumbs carry Next's own "Failed to fetch RSC payload for <url>"
  line, §5.1); everything else unchanged. Never returns `null` (web drops nothing, it only strips).
- `stripEventUrls(event)` — `request.url` through `stripUrl`; `request.query_string` **deleted**; **every** `request.headers` value
  through `stripUrl` when it contains `?` or `#` (`referer`, `next-url`, emissary's `x-envoy-original-path`, D6); `message` through
  `stripQueriesInText` (an error's own message travels there; the SDK already filtered its sensitive keys, §5.1, and this drops
  the rest of the query); every `event.breadcrumbs[]` through `stripBreadcrumbUrls`; **every `event.stacktrace[].file`** through
  `stripUrl` when it is a string (a frame of an inline script carries the page URL, §5.1), and, when present, every
  `event.exception.values[].stacktrace[].file` likewise (the SDK sends no `exception`, §2, and `afterHook` would delete one, §3.8;
  the strip is defensive and costs nothing). The processor drops a frame path's query before symbolication and fingerprinting
  (S9), so stripping it changes neither. Returns the same event.

That removes the **whole** query string — so every carrier of W8 (`reset_token`, `invite_token`, `code`/`state`/`scope`/`error`,
`u`, `search`, `q`, `f`, `cf`) — from every URL the SDK records, on top of the SDK's own key filter.

### 9.5 Tests (web vitest, `pnpm test`)

- `src/common/utils/__tests__/sentinel-url-privacy.test.ts` (node environment, no SDK mock needed): **one test per known
  carrier** — `/reset-password?reset_token=`, `/accept-invitation?invite_token=`, each of the six callbacks
  (`/email-gmail-callback`, `/email-outlook-callback`, `/google-callback`, `/lark-callback`, `/microsoft-callback`,
  `/zoom-callback`) with `code`, `state`, `scope`, `/api/proxy?u=<encoded signed URL>`, emails `?search=`, databases `?q=` and
  `?f=`, timelines `?cf=` and `?f=` — each asserting, for a navigation crumb (`from` and `to`), a fetch crumb (`data.url`), and an
  event's `request.url`, `request.query_string` and `request.headers.referer`, that the stripped output contains neither the
  query parameter's name nor its value, and that `JSON.stringify` of the result does not contain the value. Plus: a URL without a
  query is unchanged; a fragment is removed; a non-URL string is unchanged. Also: a console crumb whose message is Next's exact
  `Failed to fetch RSC payload for https://app.example/databases/x?q=alice%40example.com. Falling back to browser navigation.` keeps
  no `q=` value; an `x-envoy-original-path: /reset-password?reset_token=…` header is stripped. Frame files: an event with
  `stacktrace: [{ file: 'https://app/reset-password?reset_token=X', function: 'f', in_app: true }]` and an
  `exception.values[0].stacktrace[0].file` of the same value comes out with both files `https://app/reset-password`, and
  `JSON.stringify` of the event does not contain `reset_token` or `X`. Regex syntax: the test reads the module's source with
  `readFileSync` and asserts it contains neither `(?<=` nor `(?<!` nor `replaceAll`, nor any call on §5.1 *Built-in API floor*'s scan
  list (`structuredClone(`, `.toWellFormed(`, `.isWellFormed(`, `.at(`, `.findLast(`, `Object.hasOwn(`, `Object.fromEntries(`, `.matchAll(`, `Promise.allSettled(`, `Promise.any(`, `AggregateError`, `WeakRef`, `FinalizationRegistry`, `URL.canParse(`, `Object.groupBy(`, `.toSorted(`, `.toReversed(` and `.toSpliced(`), and asserts the exported `URL_IN_TEXT.flags`
  is exactly `'g'`; `stripQueriesInText('see /a?q=1 and https://h/p?token=z.')` is `'see /a and https://h/p'` (the prefix
  characters — the space — are put back). Nested and comma-separated URLs:
  `stripQueriesInText('see /p?a=1,https://h/q?reset_token=S')` is `'see /p'`;
  `stripQueriesInText('see /p?next=x,https://h/q&reset_token=S')` is `'see /p'`;
  `stripQueriesInText('see /login?next=/reset-password?reset_token=X done')` is `'see /login done'`;
  `stripUrl('/login?next=/reset-password?reset_token=X')` is `'/login'`; `stripQueriesInText('/p?ids=1,2,3 ok')` is
  `'/p ok'`; Next's RSC line for `https://app/t?search=a,https://x.io&cf=Alice%20Tan` keeps neither `search=` nor `cf=` nor
  `Alice`. None of these outputs contains `reset_token`. The `next=x` case is shown red by revision 4's pattern (the comma rule
  `(?:[^\s"'<>#,]|,(?!https?:\/\/))*` in the query group), which gives `'see /p,https://h/q&reset_token=S'`. Fragments:
  `stripQueriesInText('go https://h/cb#access_token=S')` is `'go https://h/cb'`, shown red by deleting group 4
  (`(#[^\s"'<>]*)?`) from the pattern. **Userinfo** (§9.4): `stripUrl('https://u:p@h/x?y=1')` is `'https://h/x'` and
  `stripUrl('https://u:p@h/x')` is `'https://h/x'`, shown red by deleting `stripUrl`'s `stripUserinfo` call;
  `stripQueriesInText('fetch https://u:p@h/x failed')` is `'fetch https://h/x failed'`, shown red by deleting the one in the
  replacement; `stripUrl('https://h/users/@me')` is unchanged. The no-lookbehind scan is shown red by writing the pattern with `(?<![\w/])`. (The
  module's comments must not spell those tokens either; the scan is plain text.) **Message:** an event
  `{ message: 'failed at /reset-password?reset_token=X', stacktrace: [], request: { url: '/p' } }` (the fixture holds no other
  `X`) passed through `stripEventUrls` has `message` `'failed at /reset-password'`, and `JSON.stringify` of it contains neither
  `reset_token` nor `X`. Shown red by deleting the `message` line of `stripEventUrls`. Without this case and its twin in
  `instrumentation.test.ts` below, that deletion leaves every web test and `test:coverage` green (the function still runs).
- `src/__tests__/instrumentation.test.ts`: for each of the three Sentinel inits, `beforeBreadcrumb` is the stripping function and
  strips a `reset_token` crumb; `beforeSend` strips `request.url` / `query_string` on a kept event and still drops the 401/403
  cases (the existing table); the client `beforeSend` still stamps `sentry_replay_id` and returns the stripped event; with
  `Sentry.getClient` mocked to throw, the client `beforeSend` returns the stripped event (no `query_string`) and does not throw.
  **Message:** the client `beforeSend`, given the event `{ message: 'failed at /reset-password?reset_token=X' }` with a kept
  (non-401/403) hint, returns an event whose `JSON.stringify` contains neither `reset_token` nor `X` (neither the fixture nor
  the mocked replay id holds another `X`) — the same fixture and the
  same red (deleting `stripEventUrls`' `message` line) as in `sentinel-url-privacy.test.ts`, proving the wiring and not only the
  module.
- `src/layouts/dashboard/shell/__tests__/index.test.tsx`: the mock gains `setContext`; asserts
  `setUser({ id, email, username })`, `setTenant(companyId)`, `setContext('tenant', { id: companyId, name })`, and none of the three
  while impersonating.
- `src/common/utils/__tests__/credential.test.ts` / `credential.server.test.ts`: `endSession` calls `setContext('tenant', null)`
  in the browser; the server variant still calls none.
- `src/app/__tests__/layout-shells.test.tsx`: its `@sentinel/sdk-js/nextjs` mock gains `setContext` (the shell now calls it).
- `src/app/api/sentinel/__tests__/route.test.ts` — **the one web test that runs the real `@sentinel/sdk-js/nextjs/route`** (W12),
  so moving the pin to the rc and then to `v0.2.0` changes what it observes; it is updated in the same commit as the pin:
  - `beforeEach` also stubs `SENTINEL_PROJECT_KEY` to `undefined`, so the forward's header set does not depend on the machine's
    environment (the route adds `x-project-key` when that variable is set).
  - The case "forwards to {endpoint}/batch with the tunnel key only, project_key and project_id stripped" (request headers
    `cookie: session=abc`, `x-forwarded-for: 203.0.113.10, 10.0.0.1`; `TRUSTED_PROXY_HOPS=1`) now expects exactly:
    - `init.headers` `toEqual({ 'content-type': 'application/json', 'x-api-key': TUNNEL_KEY, 'x-sentinel-derive-ip': 'off' })`
      — no `user-agent` (the test's `Request` sets none; undici's `Request` adds none), no `cookie`, no `x-forwarded-for`;
    - the body `toEqual([{ message: 'boom', error_class: 'Error', platform: 'javascript', user: { ip_address: '10.0.0.1' } }])`
      — `getClientIp` with one hop takes the rightmost `X-Forwarded-For` entry, `10.0.0.1`, and `applyClientIp` writes it into a
      new `user` (case B1, the item has no `user`); `project_key` / `project_id` still absent.
  - A new case, "forwards a 0.2.0 item's typed fields through the widened ITEM_KEYS": the same request with one item
    `{ message: 'boom', error_class: 'Error', level: 'error', tags: { tenant_id: 'c1' }, user: { id: 'u1', email: 'a@b.test',
    ip_address: '{{auto}}', geo: { city: 'X' } }, LEVEL: 'fatal' }` forwards exactly `[{ message: 'boom', error_class: 'Error',
    platform: 'javascript', level: 'error', tags: { tenant_id: 'c1' }, user: { id: 'u1', email: 'a@b.test', ip_address:
    '10.0.0.1' } }]` — the browser's `{{auto}}` and `geo` replaced by the derived IP, the case-folded `LEVEL` dropped (§7.1).
  - The 413, 400, 429, 503 and rate-limit cases are unchanged (none reaches the forward, or none depends on its shape).

---

## 10. Files touched

**sentinel** (side `be`)

| Path | Change |
|---|---|
| `packages/sdk-js/src/core/types.ts` | `SentinelEvent` v2 keys, `Level`, `User`, `Breadcrumb`, `BreadcrumbHint`, `BeforeBreadcrumb`, `RequestInfo`, `SdkInfo`, new options on `CoreOptions` / browser / node option types |
| `src/core/state.ts` | `Scope` = `{ tags, user?, contexts, extras, breadcrumbs }`; `InternalClient.sdkName` |
| `src/core/scope.ts` | `setUser`, `setTenant`, `setTag`, `setTags`, `setContext`, `setExtra`, `setExtras`, `addBreadcrumb` |
| `src/core/breadcrumbs.ts` (new) | ring buffer, normalisation (§4.2), `beforeBreadcrumb`, the Go-ruler sizes (§4.4) |
| `src/core/url.ts` (new) | `filterUrl`, `filterQuery`, `filterUrlsInText`, `URL_IN_TEXT`, `stripUserinfo` (§5.1, including rule 0's userinfo removal, rule 2's repeated lenient decode, rule 5's nested URLs and fragments, and group 4's dropped fragment) |
| `src/core/version.ts` (new) | `SDK_VERSION` |
| `src/core/event.ts`, `src/core/capture.ts` | typed fields, level rules, the 50-tag cap keeping `tenant_id` at build and in `afterHook` (§3.5 rule 6), `message` through `filterUrlsInText` in `build` (§5.1), metadata assembly (§3.7), `afterHook` (§3.8, including the 16 384-byte `request.data` bound and the last 100 of a hook's `breadcrumbs`), browser trimming in `enqueue` (§4.5, one loop with the marker and the `shrinkMetadata` re-run inside it), `METADATA_EXEMPT_KEYS` in place of `SHRINK_EXEMPT_KEYS` and the shrink's never-drop end, with `boundAndShrink` no longer dropping (§3.7), the crumb copy at capture (§4.4), the publish bound in `enqueue` on every runtime (§4.6), `shrinkMetadata`'s two optional parameters (§4.6 step 4), `TRUNCATED_VOCABULARY` + `breadcrumbs contexts tags` |
| `src/core/proto-size.ts` (new) | `protoSizeEstimate`, `structBytes`, `NATS_PUBLISH_BUDGET` = 1 000 000 (§4.6) |
| `src/core/server-bounds.ts` (new) | `boundContexts`, `boundStringMap`, `boundRequestStrings`, `compareCodePoints` (§3.10), called from `build` and `afterHook` before the metadata shrink, and by `src/nextjs/request-error.ts` for §6.4's headers |
| `src/core/init.ts`, `src/core/options.ts` | new options and their degradation; `initCore(…, sdkName)` |
| `src/core/index.ts`, `src/nextjs/index.d.ts` | exports (§3.9) |
| `src/browser/integrations/{console,fetch,xhr,navigation,click}.ts` (new), `src/browser/contexts.ts` (new) | §5 (the console integration filters each argument before cutting it, §5.2), §6.1 |
| `src/browser.ts`, `src/nextjs/client.ts` | install integrations, entry names |
| `src/node.ts`, `src/nextjs/server.ts`, `src/node/contexts.ts` (new) | §6.2, `serverName`, entry names |
| `src/edge.ts`, `src/nextjs/edge.ts` | §6.3, entry names |
| `src/nextjs/request-error.ts` | §6.4 |
| `src/nextjs/route.ts` | `ITEM_KEYS`; `applyClientIp` deletes an empty rebuilt `user` (§7.1) |
| `packages/sdk-js/tests/**` | §11.2's suites; regenerated `tests/fixtures/*.json`; new `tests/fixtures/v2-rich-context.json`, `v2-hostile-before-send.json`, `v2-publish-bound.json`, `v2-estimate-numbers.json`, `v2-estimate-strings.json`, `tests/tunnel-fixtures/tunnel-v2-event.json`; `tests/fixture-estimates/{golden,tunnel}/*.json` (new, §7.3); `tests/helpers/commit.ts` (new, the shared `commit()`) |
| `tests/unit/sdk_js_fixture_test.go`, `tests/unit/sdk_js_tunnel_fixture_test.go` | §7.3, including the `proto.Size ≤ protoSizeEstimate` assertion on every fixture and `TestFixtureEstimatesHaveFixtures` |
| `Taskfile.yml` (`test-unit`), `.github/workflows/ci.yml` (`sdk-js-node`'s fixture step) | the drift checks gain `tests/fixture-estimates` and `tests/fixtures` (§7.3) |
| `packages/sdk-js/package.json`, `CHANGELOG.md`, `README.md`; `docs/sdk-specification.md` (sdk-js rows) | §8 |

**web** (side `fe`): `package.json`, `pnpm-lock.yaml`, `src/layouts/dashboard/shell/index.tsx`, `src/common/utils/credential.ts`,
`instrumentation-client.ts`, `sentinel.server.config.ts`, `sentinel.edge.config.ts`, `src/common/utils/sentinel-url-privacy.ts`
(new), and the tests of §9.5: `src/common/utils/__tests__/sentinel-url-privacy.test.ts` (new), `src/__tests__/instrumentation.test.ts`,
`src/layouts/dashboard/shell/__tests__/index.test.tsx`, `src/common/utils/__tests__/credential.test.ts`,
`src/common/utils/__tests__/credential.server.test.ts`, `src/app/__tests__/layout-shells.test.tsx`, and
**`src/app/api/sentinel/__tests__/route.test.ts`** (the real-SDK tunnel test, W12 — it changes with the pin).

---

## 11. Verification

### 11.1 Commands

sentinel, from the repository root (a private worktree, never a shared checkout):

```sh
task test-unit        # go test ./tests/unit/... (both fixture replays), ingestor packages, dashboard vitest,
                      # sdk-js install (pnpm 10.34.5, --frozen-lockfile) + vitest, fixture / estimate / tunnel-fixture drift checks
cd packages/sdk-js && npx -y pnpm@10.34.5 test -- --sequence.shuffle   # the suite is written shuffle-safe (vitest.config.ts)
npx -y pnpm@10.34.5 typecheck && npx -y pnpm@10.34.5 build && npx -y pnpm@10.34.5 check:no-dependencies && npx -y pnpm@10.34.5 size
```

CI runs the same through `sdk-js-node` (node 20 and 22: typecheck, build, no-dependencies, consumer-path install; `pnpm test` on
push) and `go-root` (`go test ./tests/unit/...`). `task` is not used by CI (repo hazard `sentinel_task_absent_in_ci`); the check
names are the truth.

web, with pnpm 10.13.1: `pnpm install --frozen-lockfile && pnpm check && pnpm typecheck && pnpm lint && pnpm test &&
pnpm check:lockfile && pnpm check:stale-mocks && pnpm test:coverage` (the last enforces per-file coverage, which the new
`sentinel-url-privacy.ts` must meet). `pnpm check` (`scripts/check.mjs`) is the step `.github/workflows/pr-checks.yml` runs
first, and it holds gates nothing else in this list runs: format, knip (`check:dead`), ast-grep (`sg`, whose `no-prose-comments`
rules the new module's comments must pass), structure, boundaries, secrets, docs and agents checks, beside lint and typecheck.
The individual commands after it stay listed because they are what a failing lane names.

"Cannot skip": no new test may use `skip`, `skipIf`, `runIf`, `todo` or an early `return` on a missing API. The jsdom suites
**install** what jsdom lacks (a stub `fetch`, a fake `XMLHttpRequest` class, `navigator.deviceMemory`) before `init`, so the code
path runs rather than being skipped; each proof below is shown red by deleting the production line it guards (repo convention).

### 11.2 Criterion → proof

| Card criterion | Proof (all under `task test-unit` for sentinel, `pnpm test` for web) |
|---|---|
| **TASK-01** "Unit tests show each API landing in its typed field; metadata no longer carries tags/user_id" | `tests/core/scope.test.ts` (new): `setUser` → `user` (each key, replace semantics, `null` clears, a 256-byte id omitted and a 255-byte id kept, unknown key ignored); per-call `ctx.user` / `ctx.tags` for one event only, and per-call `ctx.metadata.user_id` / `tenant_id` dropped; `setTag` / `setTags` → `tags` (each rule of §3.5 with its debug line, number/boolean coercion, `null` deletes, a rejected value deletes the old one); `setTenant` → `tags.tenant_id`, `setTag('tenant_id')` refused; **the 50-tag cap keeps `tenant_id`** (§3.5 rule 6, spec review round 7): `setTags` of `a00`…`a49` (50 keys) plus `setTenant('c1')` sends 50 tags, `tenant_id: 'c1'` and `a00`…`a48`, without `a49`, with a debug line naming `a49` and `metadata.truncated` containing `tags`. It is shown red by deleting the rule: 51 tags are sent, of which the server's cut (S2) keeps `a00`…`a49` and drops `tenant_id`. The same 50 keys without `setTenant` are sent whole, with no marker; `a00`…`a50` without a tenant sends `a00`…`a49`, the server's own choice; a per-call `ctx.tags` that takes the effective count past 50 is capped the same way; and a `beforeSend` that adds `z00`…`z60` to an event carrying `tenant_id` sends `tenant_id` and the 49 lexically smallest others (§3.8 `tags` row); `setContext` → `contexts` (object only, name length, `null` deletes, snapshot at write; **after `sanitize`**: `setContext('x', new Date())`, `setContext('x', new URL('https://h/p'))` and `setContext('x', { toJSON: () => [1] })` each store nothing under `x` and write a debug line, and `setContext('x', { toJSON: () => ({ a: 1 }) })` stores `{ a: 1 }`, §3.6); **§3.10's mirrored bounds** (spec review round 8), in `tests/core/server-bounds.test.ts` (new) and through `captureException`: 33 small custom contexts send 32, without the largest, with `'contexts'` in `metadata.truncated` and measured by the shrink (a metadata sized so the marker makes the shrink remove a key); contexts over 65 536 Go-ruler bytes but under by `JSON.stringify` (values full of `<`) lose their largest custom name; `runtime` / `os` go only after every custom name, largest first, and a size tie goes to the name smaller by code point; a `beforeSend` that adds a name of 65 code points, a name with U+0000 or a context that takes the map over 65 536 has it removed; `request.headers` of 80 entries keeps the 64 keys smallest by code point, and a key in U+E000-U+FFFF sorts before one above U+FFFF (JS `<` gets that wrong); a key of 257 code points and a key with U+0000 are dropped; a header value of 5 000 code points is cut to 4 096 and an env value to 1 024, counting a character above U+FFFF once; U+0000 is removed from values and a lone surrogate becomes U+FFFD; §6.4's headers go through the same function; **`boundRequestStrings` and the hook's crumb count** (§3.8, §3.10, spec review round 9): a `beforeSend` that sets `request.method` to 17 code points, `url` to 2 049 and `query_string` to 4 097, each holding one U+0000, sends them with the U+0000 removed and cut to 16 / 2 048 / 4 096, and a `url` whose 2 048th code point is above U+FFFF keeps it whole, counted once; a `beforeSend` that returns 150 breadcrumbs sends the last 100, in order. Each is shown red by deleting the rule it pins; `setExtra(s)` → `metadata`, reserved keys refused; `level` for each capture path of §3.2; `sdk.name` per entry (all six entries + `initCore`) and `sdk.version === package.json version`; `autoIpAddress` on/off/browser-only; `afterHook` re-normalisation, one case per row of §3.8, and per step: the allow-list removes each of `logger`, `dist`, `source`, `exception`, `project_key`, `project_id`, `fingerprint_override` and the case variants `LEVEL` / `Logger` / `Tags`; a `toJSON` on the returned object is not honoured; `environment: 42` and `environment: 'Prod!'` → the pre-hook value; `release_version: {}` and a 101-byte one → removed; `fingerprint` of 65 bytes → removed with `fingerprint_too_long`; `trace_id: 5`, `trace_flags: -1` / `1.5` / `256` → removed; a frame `line: 1e12` → removed; `contexts.x = new Date()` / `new URL(u)` / `{ toJSON: () => [] }` → `x` removed and the other names kept; `breadcrumbs[0].data = new URL(u)` → that crumb's `data` removed, the crumb kept; `breadcrumbs` of `[{ message: 'a', timestamp: 'not a date' }, { message: 'b', timestamp: 253402300800 }]` (the second is 10000-01-01 in Unix seconds) → **both crumbs kept**, each with `timestamp` equal to the faked clock's `toISOString()` (§4.2's "unusable → now", §3.8), shown red by removing such a crumb instead; **and on every event built in the file: `metadata` has no `user_id`, `tenant_id`, `level`, or any key set through `setTag`.** Cross-language: `TestSDKJSFixtures_AcceptedByIngestor` + the new pinned subtests over `v2-rich-context.json` and `v2-hostile-before-send.json` (strict decode, `ValidatePayload`, protovalidate — the latter is §3.8's "no hook can make the ingestor 400 the event" proven on the server's code, §7.3), `TestTunnelFixtureV2StrictDecode` over `tunnel-v2-event.json` (§7.3); `tests/route/route.test.ts` for `ITEM_KEYS` (§7.2). |
| **TASK-02** "Buffer bounded; beforeBreadcrumb can drop or modify entries; events carry breadcrumbs (unit tests)" | `tests/core/breadcrumbs.test.ts` (new): 150 adds keep the last `maxBreadcrumbs` (default 100; 5; 0 records none; 101 → 100 + `invalid_option`; a second `init` trims); `beforeBreadcrumb` returning a modified crumb stores it (re-normalised), `null` drops, a throw drops (and does not call `onError`), a non-object drops; an event carries a copy (a crumb added after capture and a mutation of the stored crumb do not change it); timestamp normalisation for Date / seconds / string / garbage / year 10000; `data` over 4096 Go-ruler bytes removed (a case under by `JSON.stringify` but over by the Go ruler); `addBreadcrumb({ message: 'm', data: new Date() })`, `data: new URL('https://h/p')` and `data: { toJSON: () => [1] }` each store the crumb **without** `data` (debug line), and a `beforeBreadcrumb` that returns `{ ...crumb, data: new URL(u) }` likewise (§4.2); `addBreadcrumb('x')` is a no-op; the 131 072 total drops oldest first with the Go ruler. `tests/core/size.test.ts` (new): an event with 100 × 2 KiB crumbs in the browser runtime is trimmed oldest-first to ≤ 61 440 − 2 bytes with `metadata.truncated` containing `breadcrumbs` and the transport sending it with `keepalive` on hide; the same event on node is not trimmed below the 131 072 rule; the rule-5 shed for an event over 262 142 (§4.5 rule 5, spec review round 8): a browser event with `setTenant('c1')`, tags `a00`…`a48`, a 60 000-byte context and a `beforeSend` that sets 100 frames whose `function` is 255 × U+0001 and whose `file` is 500 × U+0001 (each escapes to 6 bytes, so about 460 KB; since §3.10 bounds contexts to 64 KiB, contexts alone can no longer carry an event past 262 142). Every crumb, then `contexts`, then every tag but `tenant_id` is removed: the sent `tags` is exactly `{ tenant_id: 'c1' }`, and `metadata.truncated` contains `breadcrumbs`, `contexts` and `tags`. Shown red by revision 9's rule, which removes the whole `tags` key, so `tags` is absent. The same event with `setTenant('c1')` and no other tag is sent with `tags: { tenant_id: 'c1' }` and no `'tags'` marker. **The marker inside the loop** (§4.5 rule 2, spec review round 6's boundary case): a browser event with three console crumbs and a non-exempt metadata string `pad`. The reviewer's case used a 60 973-character string with ~141-byte crumbs. The test carries the string in metadata because `message` is cut to `MAX_MESSAGE_BYTES` = 10 000 (`src/core/event.ts`). It sizes `pad` from the measured event, so that removing the oldest crumb leaves `bytes + 2` within 61 440 by fewer than 28 bytes. This revision's node model, with 172-byte crumbs, found 60 718 characters: 61 584 bytes with three crumbs, 61 411 after one removal, 61 439 once the marker is added. Expected: two crumbs are removed and one kept; `metadata.truncated` is `['breadcrumbs']`; the sent event's `utf8Bytes(JSON.stringify(event)) + 2` is ≤ 61 440; the transport sends it with `keepalive` on hide. Shown red by revision 7's order (marker added after the loop), which sends two crumbs at 61 439 bytes (+ 2 = 61 441) without `keepalive`. **Near the metadata cap** (§4.5 rule 3's re-run): a browser event whose `metadata` leaves `afterHook` at 65 530 Go-ruler bytes (a non-exempt `blob` string sized to that, no `truncated` key yet), with three crumbs. The first removal adds the marker, the re-run removes `blob`, and the loop stops at once. **Two crumbs are kept**; `metadata.truncated` is `['breadcrumbs']`; `metadataWireBytes(JSON.stringify(sent.metadata))` is ≤ 65 536; `metadata.truncated_keys` contains `blob`; the event is sent with `keepalive` (no `metadata_too_large`). Shown red two ways. Deleting the re-run removes every crumb and sends 65 558 bytes of metadata without `keepalive`. Revision 7's order (marker and re-run after the loop) removes every crumb, and the test's two-crumb assertion fails. Both expected outcomes were produced by a node model of rules 1-3 before this revision. The same event, pushed over 262 142 by that 100-frame `beforeSend` and carrying a 60 000-byte context, gives `truncated` containing `contexts` and the same ≤ 65 536 bound. **The NATS publish bound** (§4.6, spec review round 7). `tests/core/proto-size.test.ts` (new) pins the estimator on exact values: `val(null)`, `val(false)` 2, `val(0)` 9, `val('')` 2, `val('é')` 4, `val([0])` 13, `structBytes({ a: 0 })` 16, `structBytes({ a: { b: [null, true, 'x'] } })` 31, `structBytes({ a: Array(2_030).fill(0) })` 22 345 and `structBytes({ a: Array(32_700).fill(0) })` 359 715. Each equals Go's `proto.Size` of `structpb.NewValue` / `NewStruct` of the same value, measured before this revision. `tests/core/size.test.ts` then runs these cases on the node runtime. The unshed sizes below are Go `proto.Size(MapPayloadToEvent(p))` of this revision's model of each event, and the shed figures are that model's (its stack has one frame). The tests assert the outcomes, not those byte counts: **(1) the reviewer's case.** `setExtra('a', Array(32_700).fill(0))`, then 32 × `addBreadcrumb({ data: { a: Array(2_030).fill(0) } })`, then `captureException`. §4.4's total keeps 31 crumbs (32 are 133 601 Go-ruler bytes). Unshed, it is 1 053 681 proto bytes, over 1 048 576. Expected: `protoSizeEstimate(sent) ≤ 1 000 000`. The kept crumbs are the newest, in order. Putting back the last crumb removed takes the estimate over 1 000 000, so the loop removed no more than it needed. `metadata.a` and every context are kept, and `metadata.truncated` contains `breadcrumbs`. The model keeps 28 crumbs, at an estimate of 988 256 (Go: 986 582); with one more it is 1 010 684. **(1b) A publishable event is sent whole** (spec review round 10's case): the same `setExtra` and 28 crumbs. The estimate is 988 224 (Go: 986 550, 62 026 bytes below 1 048 576, more than the ingestor adds), so step 1 stops. Expected: all 28 crumbs are sent, and `metadata` has no `truncated` key. Shown red by revision 11's 900 000 budget, which removes 4 crumbs. **(2)** The same, plus `setContext('c', { a: Array(32_700).fill(0) })`, is 1 413 408 bytes unshed. Breadcrumbs are shed first, with the same minimality assertion, and `contexts.c` is kept. The model keeps 12 crumbs, at 989 138 (Go: 988 277). **(3) Contexts.** No crumbs, `setExtra('a', Array(32_700).fill(0))`, `setContext('c', { a: Array(32_700).fill(0) })`, and a `beforeSend` that adds `request.headers` `h00`…`h63`, each `'€'.repeat(2000)`: 1 104 439 bytes unshed. Expected: `c` is removed, `runtime` and `os` are kept (non-well-known first), `metadata.truncated` contains `contexts`, and `metadata.a` is kept. The model gives 745 391 (Go: 744 741). **(4) Metadata.** No crumbs, `setExtra('a', Array(32_700).fill(0))`, and the `beforeSend` adding 64 headers of `'€'.repeat(4096)`: 1 147 144 bytes unshed. Expected: both well-known contexts are removed, largest first (`os`, then `runtime`, with the fixed provider of §7.3), because there are no crumbs and no other names to shed before them, then the shrink removes `a`. `metadata.truncated_keys` contains `a`, `metadata.runtime` is kept (never removed, §3.7), and the estimate is ≤ 1 000 000. The model gives 788 003 (Go: 787 378). **(5) Step 5.** No crumbs, `setExtra('a', Array(32_700).fill(0))`, `captureException(new Error('€'.repeat(3_333)))` (a 9 999-byte message, within `MAX_MESSAGE_BYTES`), and a `beforeSend` adding 80 headers `h00`…`h79` of `'€'.repeat(4096)`, 80 env entries `e00`…`e79` of `'€'.repeat(1024)`, and `request.query_string` `'€'.repeat(4096)`. §3.10 keeps `h00`…`h63` and `e00`…`e63`, which is the server's own bound, and the query string is within its 4096 code points, so nothing sheds them further. §4.6 removes both contexts and then `a`, and the estimate is still over 1 000 000 (the model: 1 008 008; Go: 1 006 977; unshed, 1 366 743). Expected: the event is sent with its 64 headers, 64 env entries, message and query string, one debug line giving the estimate, `metadata.truncated_keys` exactly `['a']` (`runtime` is kept), and `metadata.truncated` containing `contexts`; it is not dropped. It is 41 599 bytes under NATS's 1 048 576, more than the ingestor's ~21 KB of additions, and its JSON (1 006 973 bytes) is under the ingestor's 1 MiB, so the server would publish it. That is why step 5 sends rather than drops. Revision 11's case (5), without the message and the query string, now stops in step 4 at 985 707 once `a` is removed, and the test pins that too. Revision 9's case (80 headers only) no longer reaches step 5 either: §3.10 keeps 64 of them, and case (4) shows that 64 fit. **(6) Under the budget:** an event with 100 small crumbs and a 10 KB context is sent exactly as built, with no marker. **(7) The browser** (corrected by spec review round 8): case (1) under `./browser`. `metadata.a` alone is about 65 400 JSON bytes (65 455 Go-ruler bytes with the `'breadcrumbs'` marker, within 65 536), so no event carrying it fits 61 440. Expected: §4.5 removes every crumb; `metadata.truncated` contains `breadcrumbs`; the event (about 66 200 bytes) is sent by rule 4, alone and without `keepalive`; and §4.6 step 1 stops (the model's estimate is 360 606, Go 360 329), so there is no `contexts` marker, the contexts are kept, and `metadata.a` is kept. Revision 9 said §4.5 trimmed this event to within 61 440, which it cannot. **(8) Contexts over the server's bound do not cost breadcrumbs** (§3.10, spec review round 8). Node: 100 × `addBreadcrumb({ message: 'm'.repeat(1_190) })` (130 201 Go-ruler bytes, within §4.4's 131 072, and the test asserts that all 100 are attached), then `setContext('big', { s: 'x'.repeat(1_200_000) })`, then `captureException`. Expected: all 100 crumbs are sent; `contexts` has `runtime` and `os` and not `big`; `metadata.truncated` contains `contexts` and not `breadcrumbs`; and `protoSizeEstimate(sent)` is under 1 000 000 (the model: 128 184; Go 122 324). The same holds with `setContext('big', { a: Array(100_000).fill(0) })`, which is 200 007 Go-ruler bytes and about 1.1 MB of proto. Both are shown red by deleting §3.10's contexts bound: the estimates are 1 328 187 and 1 228 187, §4.6 step 2 removes every crumb, and step 3 removes `big`. **(9) A hook's request strings are cut before they are counted** (§3.8, §3.10, spec review round 9). Node: 100 × `addBreadcrumb({ message: 'm'.repeat(1_190) })` as in case (8), and a `beforeSend` that sets `request.url` to a 1 000 001-character string (`'https://app.example/' + 'a'.repeat(999_981)`). Expected: `request.url` is sent as its first 2 048 code points; all 100 crumbs are sent; `metadata.truncated` does not contain `breadcrumbs`; and `protoSizeEstimate(sent)` is under 1 000 000. Shown red by deleting `boundRequestStrings`: the estimate counts the whole URL, so §4.6 step 2 removes every crumb and step 3 both contexts, and step 5 sends an event whose URL the server then cuts to 2 048 code points anyway. The crumbs are lost for bytes the server never publishes. The browser: the same crumbs and hook under `./browser`. Expected: the URL is cut to 2 048, §4.5 keeps the newest crumbs that fit (this revision's model keeps 45, at 60 294 bytes), and the event is sent with `keepalive` on hide. Shown red the same way. Every crumb is removed, rule 5 removes `contexts`, and the event is still about 1 MB, over the tunnel's 262 144, so it is lost (`413` → `batch_dropped`). **(10) The metadata shrink never drops, and a marker always fits** (§3.7, §4.5 rule 3, spec review rounds 9 and 10). Browser: `setExtra('route', 'x'.repeat(65_480))` (`metadata` with `runtime` is 65 512 bytes), three small crumbs, then `captureException`. The event cannot fit 61 440, so §4.5 removes the first crumb, and `'breadcrumbs'` takes `metadata` to 65 540. The re-run removes `route`. Expected: two crumbs are kept and the event is sent with `keepalive` on hide; `metadata` is exactly `{ runtime, truncated: ['breadcrumbs'], truncated_keys: ['route'] }`; nothing reports `metadata_too_large`. Shown red by revision 11's rule (the marker withdrawn, every crumb removed, sent without `keepalive`) and by revision 10's (dropped). Node, through §4.6 step 2: the same `setExtra` (65 509 bytes with `runtime: 'node'`, 65 537 with the marker), case (2)'s context and 32 of case (1)'s crumbs (§4.4 keeps 31; unshed, the estimate is 1 121 022 and Go 1 119 192). The re-run after the first removal removes `route`. Expected: the event is sent with the newest crumbs, with case (1)'s minimality assertion, `contexts.c` kept, `metadata.truncated` `['breadcrumbs']`, `truncated_keys` `['route']` and `protoSizeEstimate(sent) ≤ 1 000 000` (the model keeps 28 crumbs, at 988 302; Go 986 623). At `build`: `./browser`, the same `setExtra` and 40 small custom contexts `c00`…`c39`. §3.10 keeps 32 names and adds `'contexts'`, which takes `metadata` to 65 537, and the shrink removes `route`. Expected: the event is sent with 32 contexts, `truncated: ['contexts']` and `truncated_keys: ['route']`. **The coordinator's pin:** `setExtra('route', 'x'.repeat(70_000))` alone is sent from `build` with `route` removed, `truncated_keys: ['route']`, `runtime` kept, and no `onError` call. Shown red by 0.1.x's `boundAndShrink`, which drops it with `metadata_too_large`. **The exempt keys go last, in the server's order:** `setExtra('request_id', 'x'.repeat(70_000))` is sent with `request_id` removed (§3.7 step 2), shown red by deleting step 2, which leaves `metadata` over 65 536. `setExtras({ request_id: 'x'.repeat(40_000), 'request-id': 'x'.repeat(40_000), small: 1 })` removes `small` first (step 1), then `request-id`, which is lexically smaller at the same size, and keeps `request_id`. That is shown red by 0.1.x's order, where both are ordinary keys and the earlier-inserted `request_id` goes first. **The cap** (spec review round 11): `setExtra('request_id', 'r1')`, then 3 000 extras whose keys are 60 characters (`'k' + String(i).padStart(59, '0')`) with value `0`, through the real `./browser` `init`. Expected, the server's own outcome: `metadata` keeps `runtime`, `request_id: 'r1'` and the **958** extras `k…02042` to `k…02999`, and the 2 042 removed ones, the first 2 042 in insertion order, are recorded in `truncated_keys` as the first 50 keys (`k…00000` to `k…00049`) and `'+1992 more'`, 51 entries. Its `metadataWireBytes` is 65 491 (≤ 65 536), and nothing reports `onError`. The figure is the server's: `validation.ValidatePayload` on `dde0ea2`, given the same unshrunk metadata (`runtime: 'browser'`), keeps the same 958 extras, `request_id` and `runtime`, writes `server_truncated_keys` as 50 keys and `'+1992 more'`, and leaves 65 530 bytes. With `runtime: 'node'` both keep 958 too (SDK 65 488, server 65 527). The test asserts the kept key set, `request_id` and the marker, not the byte counts. Shown red by measuring the full list in steps 1 and 2 (revision 12's rule, where the cap applied only once the markers were all that was left): the ~189 KB of names never fit, so every extra and then `request_id` are removed, and the event leaves as `{ runtime, truncated_keys: [50 keys, '+2951 more'] }`. Cases (1)-(4) are shown red by deleting §4.6: each is sent unshed. Case (5) is shown red by deleting §3.10's headers / env bound: 80 of each are sent, and the server then drops 16 of each. Case (1) is also shown red by measuring with JSON bytes instead of `structBytes`, because its JSON is 194 490 bytes. The cross-language proof is `v2-publish-bound.json` (§7.3): case (1)'s sent event, whose `proto.Size` Go asserts is ≤ 1 000 000. **The estimate against Go on every fixture** (§7.3, spec review round 8): `TestSDKJSFixtures_AcceptedByIngestor` and `TestTunnelFixtureV2StrictDecode` assert `proto.Size` ≤ the committed `protoSizeEstimate` for every golden fixture and every item of every route fixture, including `v2-rich-context`, `v2-hostile-before-send`, `v2-publish-bound`, `v2-estimate-numbers`, `v2-estimate-strings` and `tunnel-v2-event`. They are shown red on `v2-estimate-strings` by removing the `8 ×` map-entry term (model: 44 523 against Go 44 803) and by counting the skeleton in UTF-16 units (28 303), and on `v2-estimate-numbers` by counting its values as JSON bytes. Sidecar drift fails the vitest suite under `CI` and the `git diff` gate of `task test-unit` / `sdk-js-node` locally and in CI. |
| **TASK-03** "A browser test page produces all four breadcrumb types on a captured error" (Playwright → jsdom, decision 4) | `tests/browser/integrations.test.ts` (new, `// @vitest-environment jsdom`): a page (`document.body.innerHTML` with a `form`, an `input` holding `secret-input-value`, a `button#save.btn.primary`) initialised through the real `./browser` `init` with a stub `fetch` and a fake `XMLHttpRequest` installed first; it logs to the console, `fetch`es `/api/x?token=t0k&page=2` with a body `secret-body`, sends an XHR, `history.pushState`s to `/reset?code=c0de`, clicks the button, then `captureException`s. The captured event carries crumbs of categories `console`, `fetch`, `xhr`, `navigation`, `ui.click`; the serialised event contains neither `secret-input-value`, `secret-body`, `t0k` nor `c0de`, and contains `token=[Filtered]` and `code=[Filtered]`; the click message is `form > button#save.btn.primary`; no crumb records the tunnel URL. Per integration: `breadcrumbs: { <name>: false }` records none of that kind while the other four still record; `breadcrumbs: false` records none; `close()` restores `fetch`, `XMLHttpRequest.prototype.open/send`, `history.pushState/replaceState` and the console methods when still the SDK's, and leaves a later wrapper (a stand-in for Sentry) working and pass-through otherwise. `tests/core/url.test.ts` (new): `filterUrl` for every key of §5.1 (substring and segment), the non-matches `zipcode`, `monkey`, `statement`, byte-for-byte preservation of other parts, fragments, a key without `=`, malformed `%`; `filterQuery` on a bare `reset_token=abc&page=2`; `filterUrlsInText` on Next's RSC message, on an absolute and a relative URL, on two URLs separated by one space (both filtered — the captured prefix is put back), on `a/b?token=x` and `x//c?token=x` (not a path start, unchanged, as the lookbehind it replaces would have left them), and on text with a `?` that is not a URL (unchanged). Frame files: `captureException` of an error whose `stack` is `'Error: boom\n    at f (https://app/reset-password?reset_token=X:1:2)'` sends `stacktrace[0].file` `https://app/reset-password?reset_token=[Filtered]` (§5.1). `tests/core/es-compat.test.ts` (new, node): reads every file under `src/` and asserts none contains `(?<=`, `(?<!`, `\p{`, `\P{` or `replaceAll(`, and that the exported `URL_IN_TEXT.flags` is exactly `'g'` — shown red by writing `URL_IN_TEXT` with `(?<![\w/])`. It also asserts that no file under `src/` except `src/cli/**` and `src/upload/**` (the sourcemap-upload CLI, node only, whose `walk.ts` uses `matchAll` today) contains any entry on §5.1 *Built-in API floor*'s scan list: `structuredClone(`, `.toWellFormed(`, `.isWellFormed(`, `.at(`, `.findLast(`, `Object.hasOwn(`, `Object.fromEntries(`, `.matchAll(`, `Promise.allSettled(`, `Promise.any(`, `AggregateError`, `WeakRef`, `FinalizationRegistry`, `URL.canParse(`, `Object.groupBy(`, `.toSorted(`, `.toReversed(` and `.toSpliced(`. A node-only server file is held to the same list; nothing in one needs more. The scan passes on today's `src/`, which was checked for revision 11 against `origin/main`. It is shown red by writing §4.4's crumb copy as `structuredClone(crumb)`, and §3.10's surrogate repair as `s.toWellFormed()`. A retained pre-`init` crumb is dropped by a `beforeBreadcrumb` installed at `init`. Console bounds: 11 arguments, a 1000-byte argument, a 3000-byte message, the SDK's own `[@sentinel/sdk-js]` line not recorded. Selector: depth 5, the 80-character stop, the 1000 ms debounce. **Nested URLs** (`tests/core/url.test.ts`, §5.1 rule 5): `filterUrl('/login?next=/reset-password?reset_token=X')` is `/login?next=/reset-password?reset_token=[Filtered]`; `filterUrl('/login?next=%2Freset%3Freset_token%3DX')` is `/login?next=%2Freset%3Freset_token%3D%5BFiltered%5D`; `filterQuery('a=1,https://h/q?reset_token=S')` is `a=1,https://h/q?reset_token=[Filtered]`; `filterQuery('next=/reset-password?reset_token=X')` is `next=/reset-password?reset_token=[Filtered]`; `/login?next=/r?page=2&reset_token=X` has its outer `reset_token` filtered; `/p?color=%23fff` and `/p?ids=1,2,3` come back unchanged; a value nested 3 levels deep is filtered at every level; and a value nested 10 000 levels deep returns without throwing. Each nested case above is shown red by deleting rule 5. `filterUrlsInText('see /login?next=/reset-password?reset_token=X done')` is `see /login?next=/reset-password?reset_token=[Filtered] done`, `filterUrlsInText('see /p?a=1,https://h/q?reset_token=S')` is `see /p?a=1,https://h/q?reset_token=[Filtered]`, and `filterUrlsInText('see /p?next=x,https://h/q&reset_token=S')` is `see /p?next=x,https://h/q&reset_token=[Filtered]` — shown red by revision 4's pattern (the query group `(?:[^\s"'<>#,]\|,(?!https?:\/\/))*`), which returns that input unchanged. The pattern itself: the matches of `URL_IN_TEXT` over `see /p?a=1,https://h/q?reset_token=S` are **one** match with group 2 `/p` and group 3 `a=1,https://h/q?reset_token=S`, and over `/p?ids=1,2,3 ok` one match with group 3 `ids=1,2,3`. **Round 3's three residuals, closed** (§5.1 rules 2 and 5, group 4): `filterUrl('/login?next=%252Freset%253Freset_token%253DX')` (doubly encoded) is `/login?next=%252Freset%253Freset_token%253D%255BFiltered%255D`, shown red by decoding once; `filterUrl('/login?next=%2Fcb%23access_token%3DX')` (a nested fragment-only URL) is `/login?next=%2Fcb`, shown red by deleting rule 5's fragment test, while `/p?color=%23fff` and `/p?next=%2Fp%23page%3D2` stay unchanged; `filterUrlsInText('go https://h/cb#access_token=S')` is `go https://h/cb`, and `filterUrlsInText('see /p?a=1#frag and more')` is `see /p?a=1 and more`, both shown red by deleting group 4 from the pattern. `/p?co%2564e=X&page=2` gives `/p?co%2564e=[Filtered]&page=2` (the key spells `code` only after rule 2's second decode; shown red by decoding once); `/p?%zz=1&token=%zz` gives `/p?%zz=1&token=[Filtered]` (a malformed escape stays as written). **Lenient decode** (§5.1 rules 2 and 5, spec review round 6): `filterUrl('/login?next=%E9%2Freset%3Freset_token%3DX')` (a non-UTF-8 `%E9`) is `/login?next=%25E9%2Freset%3Freset_token%3D%5BFiltered%5D`; `filterUrl('/login?next=%2Freset%3Freset_token%3DX%')` (a trailing `%`) is `/login?next=%2Freset%3Freset_token%3D%5BFiltered%5D`; `filterUrlsInText('see /login?next=%2Freset%3Freset_token%3DX%E9 ok')` is `see /login?next=%2Freset%3Freset_token%3D%5BFiltered%5D ok` — each shown red by revision 7's strict decode (`decodeURIComponent` of the whole string, raw on a throw), which returns each input unchanged, token included. `filterUrl('/login?next=%2Fcaf%C3%A9%3Ftoken%3DX')` is `/login?next=%2Fcaf%C3%A9%3Ftoken%3D%5BFiltered%5D` (a valid multi-byte escape decodes and re-encodes to the same bytes). **Console: filter, then cut** (`tests/browser/integrations.test.ts`, §5.2): `console.log(arg)` with `arg = '/login?next=%2Freset%3Freset_token%3DS3CR3T%26pad%3D' + 'a'.repeat(458) + '%26b%3D1'` (518 bytes; its 512-byte cut ends inside the nested `%26`, as `…a%2`) records a crumb whose `message` equals `filterUrlsInText(arg)` cut to 512 bytes, is 512 bytes, and contains neither `S3CR3T` nor `%252`. Shown red by cutting before filtering: with revision 7's strict decode the message contains `S3CR3T`; with the lenient decode it is 522 bytes and ends in `%252`, past the 512 bound. **The `;` separator** (§5.1 rule 2): `filterUrl('/p?a=1;reset_token=X')` is `/p?a=1;reset_token=[Filtered]` and `filterQuery('a=1;token=t&b=2')` is `a=1;token=[Filtered]&b=2`, both shown red by splitting on `&` only; the keyless residual is pinned as kept, `filterUrl('/verify?t0k')` is `/verify?t0k` (§12.2(3)). **Userinfo** (§5.1 rule 0): `filterUrl('https://user:pass@h/p?a=1')` is `https://h/p?a=1`, `filterUrl('https://u:p@h/x')` (no `?`, no `#`) is `https://h/x`, `filterUrl('//u@h/x')` is `//h/x`, `filterUrl('https://a@b:p@h/')` is `https://h/`, and `filterUrl('https://h/users/@me')` and `filterUrl('mailto:a@b.test')` are unchanged — shown red by deleting rule 0; `filterUrl('/login?next=https%3A%2F%2Fu%3Ap%40h%2Fx')` is `/login?next=https%3A%2F%2Fh%2Fx`, shown red by deleting rule 5's userinfo condition; `filterUrlsInText('fetch https://u:p@h/x failed')` is `fetch https://h/x failed` and `filterUrlsInText('see //u:p@h/x ok')` is `see //h/x ok`, shown red by deleting the `stripUserinfo` call in the replacement. **Round 5's two residuals, pinned as kept** (§12.2(3)): `filterUrl('/login?next=reset_token%3DX')` is `/login?next=reset_token%3DX` and `filterUrlsInText('hash is #access_token=X')` is `hash is #access_token=X`. **Round 7's residual, pinned as kept** (§12.2(3)): `filterUrlsInText('GET /api/search?q=John Doe&token=abc')` is returned unchanged. The match ends at the space, so its query is `q=John` and ` Doe&token=abc` is outside every match. **Linear time:** `filterUrlsInText('-/'.repeat(500_000))` returns within 1 s — shown red by the revision-3/4 shape, which needs minutes on that input (§5.1). Each expected output above was produced by a node model of these rules before this revision. **Error messages** (`tests/core/capture.test.ts`, extended, §5.1 *Where the filters run on an event's `message`*): `captureException(new Error('failed at /reset-password?reset_token=X'))` sends `message` `failed at /reset-password?reset_token=[Filtered]`; `captureMessage('see https://h/q?token=t#frag')` sends `see https://h/q?token=[Filtered]`; the browser `unhandledrejection` handler with `new Error('at /r?code=c0de')` and node's `uncaughtException` path each send `code=[Filtered]`; a message of exactly `MAX_MESSAGE_BYTES` ending in `/r?token=X` is filtered and then cut to `MAX_MESSAGE_BYTES` with `metadata.truncated` containing `message`; a `beforeSend` that sets `message` to `'/r?token=t'` is sent with it as written. Each is shown red by deleting the `filterUrlsInText` call in `build` (the `beforeSend` case by adding one to `afterHook`). **fetch keeps `unhandledrejection` working** (`tests/browser/fetch-rejection.test.ts`, new, jsdom; §5 *No wrapper marks a page promise handled*): jsdom never fires `unhandledrejection` by itself, so the test does what a browser host does. It temporarily removes vitest's own `process` `unhandledRejection` listeners (the `withSoleListener` pattern of `tests/handlers/node.test.ts`, restored in `finally`) and adds a bridge that, for each Node `unhandledRejection(reason, promise)`, counts it and dispatches `new PromiseRejectionEvent('unhandledrejection', { promise, reason, cancelable: true })` on `window`. `fetch` is stubbed to `() => Promise.reject(new TypeError('Failed to fetch'))` before `init` from `./browser`, with the default `installGlobalHandlers` and `fetch` breadcrumbs on. *Uncaught:* `const ret = fetch('/api/x')` with no handler, then two `setTimeout(0)` turns. The bridge counts exactly **1**, its `promise` is `ret`, and exactly one event is captured, with `error_class` `TypeError`, `metadata.source` `unhandledrejection` and one `fetch` crumb at level `error`. *Caught:* `await fetch('/api/x').catch(() => {})`. The bridge counts **0**, no event is captured, and the `fetch` crumb is still recorded. *Resolved:* with a stub that resolves to a `Response`, the awaited value is that same `Response` (`toBe`). Shown red two ways: returning the original promise after attaching record-only reactions (`p.then(ok, fail); return p`) counts 0 in the uncaught case, and attaching a rethrowing reaction while still returning the original counts 1 in the caught case. No test asserts that `fetch` returns the original promise object. XHR, in the same file: the wrapped `open` and `send` return the fake's return value, and a `send` that throws rethrows the same error object (`toBe`) and records no crumb. |
| **TASK-04** "Each runtime's events carry its contexts (unit + Next fixture tests)" | `tests/contexts/browser.test.ts` (jsdom): `request.url` without query/fragment, `query_string` filtered, `request.headers["User-Agent"]`, no `Referer`, `contexts.culture`, `contexts.device` (with jsdom's `screen` and a stubbed `deviceMemory` / `hardwareConcurrency`), **no** `contexts.browser` / `os` (D2), and an app `setContext('culture', { locale: 'x' })` winning key-wise over the automatic one. `tests/contexts/node.test.ts`: `contexts.runtime`, `contexts.os`, `server_name` from `os.hostname()` and from `serverName`. `tests/contexts/edge.test.ts`: `contexts.runtime` with and without `EdgeRuntime`, no `os` / `server_name`. `tests/nextjs/adapter.test.ts` (extended): `captureRequestError` on the server and edge builds fills `request.method`, `request.url` (no query), `query_string` filtered by `filterQuery`, the five headers removed, `referer` and an `x-envoy-original-path` filtered, `sdk.name` `sentinel.javascript.nextjs` from all three Next builds, `transaction` = `routePath`, and **no** `metadata.http_method` / `http_url`. Server side of D2: `TestTunnelFixtureV2StrictDecode`'s `enrich.Apply` fills `contexts.browser` / `os` from the body UA. |
| **TASK-05** "Tag v0.2.0 (sdk-js) pushed; README 'not implemented' list updated" | `tests/package.test.ts` (version 0.2.0, `SDK_VERSION` equal); `tests/docs.test.ts` (extended): the CHANGELOG's first `## ` entry is `v0.2.0`, the README's *What is not implemented* does not mention breadcrumbs and still lists tracing and per-request server scope, and its *URL privacy* section's *Not filtered* list names the residuals of §12.2(3) (it contains `scheme-less`, `location.search`, `location.hash`, `more than 3`, `keyless`, `next=reset_token%3DX`, `#access_token=X`, `q=John Doe&token=abc` and `addBreadcrumb`). The tag: `git ls-remote --tags origin packages/sdk-js/v0.2.0` resolves to the sentinel merge commit — **recorded by the coordinator as card-criterion evidence** (see *Proofs no gate can hold* below). |
| **TASK-06** "A staging browser error in Sentinel shows user, browser/os context and breadcrumbs" | web vitest of §9.5 (per-carrier stripping, frame files, nested and comma-separated URLs, fragments in text, the event `message` in the module and through the client `beforeSend`, the no-lookbehind scan, the three inits' wiring, shell and logout calls, and the real-SDK tunnel forward of `route.test.ts`). The staging observation of §11.4 — **recorded by the coordinator as card-criterion evidence** (see below). |

**Proofs no gate can hold.** Two card criteria are facts about the world after merge, not about a tree, so neither `task test-unit`
nor web's `pnpm test` can hold them, and no CI job re-checks them:

- **TASK-05's tag.** Pushed on the sentinel merge commit after merge (§8). The coordinator records, on TASK-05, the output of
  `git ls-remote --tags origin packages/sdk-js/v0.2.0` — the tag's SHA — beside the merge commit's SHA, and the criterion is met
  when the two are equal (for an annotated tag, the peeled `^{}` line). The unit gate's `tests/package.test.ts` / `tests/docs.test.ts`
  prove the version and the README; they cannot prove the tag exists.
- **TASK-06's staging observation.** Done by a person after web deploys with `v0.2.0` (§11.4). The coordinator records, on
  TASK-06, the Sentinel **occurrence id** read in Sentinel staging for the check error, and the one for the `reset_token` check,
  with the result of each item of §11.4 against them. web's vitest proves the wiring; it cannot prove what staging stored.

Both are recorded by the coordinator as **card-criterion evidence**; neither is claimed by an implementer's green gate, and a card
whose evidence is missing is not done, whatever its tests say.

### 11.3 Existing tests this change rewrites (their old assertions are the 0.1.x behaviour this spec replaces)

| Test | Old assertion | New assertion |
|---|---|---|
| `tests/core/capture.test.ts` B12 group (~L130-260): "the spread order — tags, then user_id / tenant_id, then ctx.metadata, then the SDK keys"; "every reserved tag name is dropped; setTag(key, "") keeps the empty string"; "scope calls made before the first init are kept" | tags and identity spread into `metadata`; `RESERVED_TAG_KEYS`; empty tag kept | tags in `tags`, identity in `user` / `tags.tenant_id`; only `tenant_id` reserved; empty value dropped (§3.5); pre-`init` scope calls still stamp the first event, now in the typed fields |
| `tests/core/capture.test.ts` B11 (~L83) "… with setTenant — … exempt keys never" | `setTenant` puts `tenant_id` into metadata as an exempt key, and `user_id` / `route` are exempt | the same shrink case with `request_id` as its exempt key in place of `user_id` / `tenant_id`, which §3.7 drops from `ctx.metadata`; `a` is still removed first (the tie rule is unchanged), and `route: '/x'` is still kept, now because it is small rather than exempt (§3.7) |
| `tests/core/capture.test.ts` B11 (~L104) "{ route: 70 KiB } alone → the event is dropped with onError metadata_too_large" | dropped, `metadata_too_large` reported | sent: `route` removed, `small` kept, `truncated_keys: ['route']`, no `onError` call (§3.7, §11.2 TASK-02 case (10)) |
| `tests/core/capture.test.ts` N12 (~L328-345): "metadata.level warning"; "captureMessage('plain') has no metadata.level"; "captureException ignores level (debug notes it)" | level in metadata, absent by default, ignored on exceptions | top-level `level` `warning`; `captureMessage('plain')` → `info`; `captureException(e, { level: 'fatal' })` → `fatal`, default `error` (§3.2) |
| `tests/handlers/browser.test.ts` B26 (a) (~L36) | `metadata.level error` | top-level `level: 'error'`, no `metadata.level` |
| `tests/init/browser.test.ts` B22 (~L127-240) | `metadata.user_id` from `setUser`, the tag in metadata | `user.id` and `tags.feature`; the one-slot / two-module-instances property it proves is unchanged |
| `tests/nextjs/adapter.test.ts` N3 and the edge case (~L50-150) | `metadata.http_method`, `metadata.http_url` | `request.method`, `request.url`, `transaction`; `metadata.route` / `router_kind` / `route_type` still present (§6.4) |
| `tests/core/classify.test.ts` B3 (~L178), `tests/transport/lifecycle.test.ts` (~L260), `tests/fixtures.test.ts` (~L175-202) | per-call `ctx.metadata.user_id` in metadata | the exempt key they rely on becomes `request_id` (B3, near-the-cap), which the server exempts, or the identity moves to `ctx.user` (lifecycle), since §3.7 drops per-call `user_id`; the property each proves is unchanged |
| `tests/unit/sdk_js_fixture_test.go` `TestSDKJSFixtures_PinnedValues` "near-the-cap" | `payload.Metadata["user_id"] == "usr_1"` exempt | `payload.Metadata["request_id"]` exempt (§7.3) |
| `tests/tunnel-fixtures.test.ts` (`tunnel-no-ip`: "no user"), `tests/route/route.test.ts` ("with none, no user at all" ~L803, the T10 key-set test ~L319, the exported `applyClientIp` table's `user: {}` rows ~L795-796) | `ITEM_KEYS` removes `user`; B4 yields `{}` | `user` passes the filter; with no IP the rebuilt-and-empty `user` is deleted (§7.1), so "no user" still holds for identity-less items and the `{}` rows become "no `user`"; the key-set test lists the seven new keys |
| `tests/unit/sdk_js_tunnel_fixture_test.go` `TestTunnelFixtureShape`, and `tunnel-user-rebuild.json` (regenerated from the changed `applyClientIp`) with `TestTunnelFixtureRebuildDecode` | `user` exactly `{ip_address}` | `USER_KEYS` strings + `ip_address` (§7.3); the rebuild fixture's no-IP rows lose `user: {}` |
| every `tests/fixtures/*.json` | v1 events | regenerated with `level` and `sdk` (and the browser ones with `user.ip_address: "{{auto}}"`); `TestSDKJSFixtures_PinnedValues`' existing subtests unchanged |

### 11.4 Post-merge observation (staging, then production), for TASK-06 and the contract's measure

After web deploys with `v0.2.0` to staging, a person signed in (not impersonating) on staging web: opens a page with a query
string (e.g. a databases view with `?q=alice`), clicks a button, triggers a network request, then raises a browser error (any
existing test hook, or `setTimeout(() => { throw new Error('sdk-js 0.2.0 staging check') })` in the devtools console). In Sentinel
staging, the occurrence must show: `user` with id, email and name (the username field), a public `user.ip_address` and geo;
`contexts.browser` and `contexts.os` (ingestor-filled), `contexts.culture`, `contexts.device`, `contexts.tenant`; `request.url`
with **no** query string and no `query_string`; breadcrumbs of all four automatic kinds (`console`, `fetch` or `xhr`, `navigation`,
`ui.click`), none with a `?` in `data.url` / `from` / `to`; `level` `error`; `sdk.name` `sentinel.javascript.nextjs`. The issue's tag
facets must list `tenant_id` with the company id (the `setTenant` path is `setTag`'s path, §3.5 — web calls no other `setTag`,
W10). Then `/reset-password?reset_token=<a throwaway value>` is opened and an error raised there: the token must appear nowhere in
the stored occurrence (search the occurrence JSON for it). The coordinator records the result, with both occurrence ids, on
TASK-06 as card-criterion evidence (§11.2 *Proofs no gate can hold*).

---

## 12. Decisions this spec made, and what each was derived from

### 12.1 Decisions

| # | Decision | Derived from |
|---|---|---|
| SP1 | SDK names `sentinel.javascript.{browser,node,edge,nextjs}`; one `nextjs` name for all three Next builds, each with its own `init` | contract §4.1 (asks the spec); Sentry's `sentry.javascript.nextjs` convention; `metadata.runtime` already carries the runtime |
| SP2 | `autoIpAddress`, default `true`, browser entries only; no effect behind the Sentinel tunnel | contract §4.1 (default on); enrichment decision 11 and S4 (I1, I3t, the tunnel's rebuild) |
| SP3 | `setExtra` / `setExtras` → metadata; per-call name stays `ctx.metadata`; per-call identity is `ctx.user` / `ctx.tags`, never metadata | contract §4.1 ("Sentry's `extra`"), decision 7; T5 |
| SP4 | Level defaults: exception `error`, message `info`, node uncaught `fatal`; `ctx.level` honoured by both; `metadata.level` dropped | contract §4.1; Sentry defaults; S7 (no reader) |
| SP5 | Tag rules applied at write time, invalid entries dropped with a debug line, `tenant_id` reserved to `setTenant`; the 50-tag cap applied per event by the SDK, keeping `tenant_id` and dropping the lexically largest others (`'tags'` in `metadata.truncated`) | contract §4.1, decision 7; S2 (the server keeps the 50 lexically smallest); S6 (the affected tenant is `tags["tenant_id"]`); spec review round 7 |
| SP6 | `user.id` > 255 bytes **omitted**, not truncated; same drop/cut split as the server for the other three keys | contract §4.1 ("says which"); S2 (`ident` vs `str`) |
| SP7 | `maxBreadcrumbs` 0-100, default 100; `beforeBreadcrumb` fails closed | contract §4.2; S2; decision 5 (privacy side) |
| SP8 | Sizes: Go ruler for per-crumb data and the 128 KiB total, all runtimes; browser-only oldest-first trim to 61 440 in `enqueue`; last-resort shed of contexts, then every tag but `tenant_id`, over 262 142 (SP38); no tunnel limit change; on every runtime the NATS publish bound of §4.6 (SP33) | contract §4.2, §4.5; T7, T12, S2, S3, S11 |
| SP9 | URL filter: PII substrings + segment-exact `code state key signature sig jwt otp credential`; `[Filtered]`; fragment dropped; other parts byte-for-byte; `filterQuery` for bare queries; keys and values decoded until stable (≤ 3 decodes); a URL nested in a value (it holds `?`, or its fragment names a sensitive key) filtered recursively, 3 levels; `filterUrlsInText` for console text and every event `message`, fragments dropped; every server header value with `?`/`#`; every stack-frame `file` | contract §4.4, decision 5; `src/core/pii.ts`; Next's RSC log line |
| SP10 | Click selector: tag + `#id` + `.classes`, ≤ 5 elements, ≤ 80 characters, ` > `, 1000 ms debounce, nothing else | contract §4.4; Sentry's `htmlTreeAsString` limits |
| SP11 | Console: ≤ 10 arguments, ≤ 512 bytes each, ≤ 2048 bytes total, objects scrubbed, the SDK's own lines excluded | contract §4.4 ("stringified and bounded") |
| SP12 | fetch/XHR: method, filtered URL, status, `duration_ms`; no message; self-exclusion by target origin + path; fetch returns the chained promise (SP26) | contract §4.4; T12 (the tunnel URL) |
| SP13 | Navigation via `history` wrappers and `popstate`, same-URL transitions skipped, relative for same origin | contract §4.4; W9 |
| SP14 | No SDK-side `contexts.browser` / `os`; UA sent in `request.headers` | D2 |
| SP15 | `captureRequestError`: relative `request.url`, filtered `query_string`, headers with the contract's five removed, sensitive names `[Filtered]`, `referer` filtered, `transaction` = `routePath`; other route metadata kept | contract §4.3; D6; T10 |
| SP16 | `ITEM_KEYS` + exactly `level sdk user tags contexts request breadcrumbs` | contract §4.5; §7.1 |
| SP17 | web: one privacy module, whole-query strip in both hooks of all three inits, `referer` included | contract §4.6, decision 5; D6 |
| SP18 | Tenant context id from `me.companyId` | D3 |
| SP19 | `applyClientIp` deletes a rebuilt `user` with no key | §7.1; the enrichment tests' "no IP → no user" invariant |
| SP20 | Automatic contexts through an injectable provider; fixtures on fixed values | §7.3; `tunnel-fixtures.test.ts: commit()`'s CI byte comparison on node 20 and 22 |
| SP21 | Every "must be an object" check on a value that went through `sanitize` is made on the **sanitised** value (`isJsonObject`); a failing context name / crumb `data` is dropped, never the event | T16 (`toJSON` replaces the object); S1 (a non-object where `ErrorPayload` declares one 400s the batch) |
| SP22 | No regex lookbehind, `v` / `d` flag or `\p{…}`, and no `replaceAll`, in anything the spec prescribes; `URL_IN_TEXT` uses a captured prefix put back on replace and no lookaround; lookahead (ES3) would be allowed; pinned by source-scanning tests on both sides | W11 (web targets Safari 12; lookbehind is Safari 16.4+; no transpiler lowers regex syntax); T6 (a throw in `beforeSend` sends the unstripped event) |
| SP23 | `afterHook` builds a new object from `SentinelEvent`'s exact key set and re-checks every v1 field against S10's rules; `environment` falls back to the pre-hook value rather than being deleted | S1, S10 (types, CEL rules, `required` environment, case-insensitive keys); T17 |
| SP24 | Stack-frame files are URL-filtered by the SDK and query-stripped by web | S9 (the processor ignores the query for symbolication and grouping); a page URL appears as the file of an inline-script frame |
| SP25 | TASK-05's tag and TASK-06's staging observation are card-criterion evidence recorded by the coordinator | §11.2 *Proofs no gate can hold* |
| SP26 | The fetch wrapper returns `original(...).then(record-and-return, record-and-rethrow)`, never the original promise object; no other wrapper reacts to a page promise, and no listener cancels or stops an event | Promise semantics (a reaction marks a promise handled, so the same-object form suppresses `unhandledrejection`, measured in spec review round 2); `src/core/handlers/browser.ts` (0.1.x's `unhandledrejection` capture); `@sentry/core` `instrument/fetch.ts: instrumentFetch` |
| SP27 | Rule 5: a query value whose decoded form (until stable, ≤ 3 decodes) holds `?`, or holds `#` with a sensitive key in its fragment, is passed through `filterUrl`, recursively to 3 levels, re-encoded as many times as it was decoded; `URL_IN_TEXT`'s query group runs across commas (revision 4's comma rule is withdrawn) and its fragment group is dropped on replace | Spec review round 2 (`/login?next=/reset-password?reset_token=X`, `a=1,https://h/q?reset_token=S`) and round 3 (`see /p?next=x,https://h/q&reset_token=S`; the double-encoded, nested-fragment and text-fragment inputs); decision 5; contract §4.4 ("Fragment (`#…`) is dropped"); W8 (web's reset and invite tokens); W11 (no lookbehind) |
| SP28 | The SDK runs `filterUrlsInText` over every event's `message` in `build`, before the bounds, on every capture path; not again after `beforeSend` | The brief names error messages as a token path (spec review round 3); §2 (no `exception` is sent, so `message` carries the error's value); `apps/processor-go/fingerprint/fingerprint.go: Compute` (the message is not hashed, so grouping is unchanged); T6 (the hook owns what it returns) |
| SP29 | Userinfo (`user:pass@`) is removed from every URL `filterUrl` sees (rule 0, and rule 5's nested values), from every URL `filterUrlsInText` matches, and by web's `stripUrl` / `stripQueriesInText`; a query nested without `?` and a bare fragment in text stay residuals | Sentry's `getSanitizedUrlString` (authority filtered); spec review round 5; the WHATWG parser's last-`@` rule |
| SP30 | A `metadata.truncated` marker added in `enqueue` (§4.5 rules 2 and 5) is followed by a `shrinkMetadata` re-run (rule 3), not covered by a byte reservation | T4 (the SDK's shrink keeps the server's a no-op); spec review round 5 |
| SP31 | §4.5's trim is one loop whose measure includes the marker: the first removal adds `'breadcrumbs'` and runs the re-run, and the loop continues against the re-measured bytes; rule 5 does the same per shed field. The marker is kept even when it alone keeps an event out of `keepalive` | Spec review round 6 (the marker tipped a trimmed event with crumbs left back over 61 440); T7 (the queue's own count); a reader must be able to tell removed crumbs from none |
| SP32 | URL decoding is lenient (each valid `%XX` run decoded, each invalid triplet kept as written), and every URL-bearing string is filtered whole before it is cut | Spec review round 6 (a `%E9` or trailing `%` hid a nested URL; a console argument cut inside a `%XX` kept its token) |
| SP33 | `enqueue` bounds every event, on every runtime, by an upper-bound estimate of its protobuf size that counts each `Struct` / `Value` at its exact proto3 wire size and everything else by its JSON. Over 1 000 000 (revision 12; 900 000 before) it sheds breadcrumbs oldest first, then contexts (non-well-known first, largest first), then metadata through `shrinkMetadata` with a proto measure. Each marker is inside the measure, and an event with nothing left to shed is sent, not dropped. Since revision 10 the estimate is taken after the SDK has applied the server's contexts and headers / env bounds (SP36), so it counts only what the server keeps; revision 9's deliberate contexts over-count is gone | S11 (NATS `max_payload` 1 048 576 on the published proto; a failed publish loses the item); spec review round 7 (the reviewer's 1 053 681-byte event); §4.5 (order and markers); `bounds.go: boundContexts` (victim order) |
| SP34 | `afterHook` drops a `request.data` over 16 384 Go-ruler bytes | S2 (the server's `MaxRequestDataSize` drops it whole anyway); §4.6 (so the publish bound never needs to shed it) |
| SP35 | A URL written unencoded into text, whose query holds a space, quote, `<` or `>`, is a residual: listed, pinned as kept, no new rule | Spec review round 7; `URL_IN_TEXT` ends a query at those characters because in text they end the URL far more often than they belong to it |
| SP36 | The SDK applies the server's `boundContexts` (32 names, 65 536 Go-ruler bytes, custom names before the six well-known ones, largest first, tie to the smaller name) and `boundStringMap` (headers / env: 64 entries, keys 1-256 code points, values cut to 4096 / 1024 code points, lexically largest keys dropped) at `build` and again in `afterHook`, before the metadata shrink. A removed context adds `'contexts'` to `metadata.truncated`; a header / env cut is a debug line only. Names and keys holding U+0000 are removed, which is stricter than the server | Spec review round 8 (the estimate over-counted contexts the server cuts, and cost an app its crumbs); S2 (`bounds.go: boundContexts`, `boundStringMap`, `sortVictims`, `objectSize`); T4 (the SDK's ruler, never below the server's); the precedent of the tags cap (SP5), the crumb total (SP8) and `request.data` (SP34) |
| SP37 | Every golden and route-tunnel fixture's `protoSizeEstimate` is committed as a sidecar under `tests/fixture-estimates/`, one file per fixture. The Go replays assert `proto.Size` (with the ingestor-stamped project fields cleared) is within it, and the drift gates cover the sidecars and the golden corpus | Spec review round 8 (the estimate's upper-bound claim was gated on one fixture); T13, T14 (the replays and the drift gate); the tunnel suite's `commit()` (never written under `CI`) |
| SP38 | §4.5 rule 5's last resort removes every tag except `tenant_id` | Spec review round 8; S6 (the affected tenant is `tags["tenant_id"]`); SP5 (the same reason the 50-tag cap keeps it) |
| SP39 | `build` and `afterHook` cut `request.method` / `url` / `query_string` to the server's 16 / 2048 / 4096 code points after removing U+0000 (`boundRequestStrings`), and `afterHook` keeps the last 100 of a hook's `breadcrumbs`. §3.8 lists every typed field a hook can set, with its SDK and server bound | Spec review round 9; S2 (`bounds.go: boundRequest`, `boundBreadcrumbs`); §3.10 (the estimate counts only what the server keeps) |
| SP40 | The metadata shrink never drops an event. `METADATA_EXEMPT_KEYS` (the server's 18 names) replaces `SHRINK_EXEMPT_KEYS`, so `route` is an ordinary key; the other keys go first, then the exempt ones (largest first, lexical tie), with `truncated_keys` written and measured in the server's capped `removedKeys` shape at every step, so the marker never forces an exempt key out; `runtime` is never removed. The `shrinkMetadata` re-run therefore cannot fail, and revision 11's marker withdrawal is gone. `metadata_too_large` is no longer emitted | Spec review rounds 9, 10 and 11; the coordinator's ruling that F5 is not an exception; `bounds.go: shrinkMetadata`, `metadataExemptKeys`, `sortVictims`, `removedKeys`; S5 (enrichment reads `runtime` before the server's shrink); SP30 (the re-run itself) |
| SP41 | Browser code this spec prescribes calls no built-in newer than Safari 12, and SDK code does not count on Next's `@next/polyfill-module`. A crumb is copied at capture by spread, with `data` through `sanitize`. Both sides' source scans pin the list | Spec review round 9; W11 (Safari 12 in web's target list); web's installed `next@15.5.26`, `dist/build/polyfills/polyfill-module.js` (the nine APIs it supplies); SP22 (the regex counterpart) |
| SP42 | `NATS_PUBLISH_BUDGET` is 1 000 000, leaving 48 576 bytes below NATS's `max_payload` for what the ingestor adds after the SDK (at most ~21 KB) | Spec review round 10 and the coordinator's ruling; the sibling sdk-go spec's `maxPublishBytes` for the same model; S11 |

### 12.2 Trust-boundary and privacy notes for the spec reviewer

1. **The tunnel admits seven more browser-written keys.** All of them were already writable by the browser in 0.1.x through
   `metadata` (tags, `user_id`, `tenant_id`), or describe the browser itself (UA, page URL, contexts, breadcrumbs). What a hostile
   browser can do with them is what it could do before: label its own events. It **cannot** choose its stored IP (the rebuild of
   §7.1 is unchanged) or its project (key-derived). `tags.tenant_id` from a browser is the browser's claim, exactly as
   `metadata.tenant_id` was.
2. **The UA source for tunnelled events moves** from the forwarded header to the body's `request.headers` (§7.1). Both are written
   by the same browser; no new spoofing capability.
3. **URLs.** The SDK filters sensitive query values by default and drops fragments (§5.1); it never records bodies, fetch/XHR
   headers, input values or `document.referrer`. web additionally drops every query string (§9.4). The
   processor still masks URLs, messages, data and contexts server-side (S6). URL-shaped text in console messages **and in every
   event's `message`** is filtered by the SDK (`filterUrlsInText`, §5.1 *Where the filters run on an event's `message`*) and
   stripped by web (`stripQueriesInText`), fragments included. Userinfo (`https://user:pass@h/…`) is removed from every URL
   both filters see, in text too (§5.1 rule 0, §9.4). A URL carried inside a query value (`next=/reset-password?reset_token=X`),
   however many times (up to 3) it was percent-encoded and whatever malformed or non-UTF-8 escape it also carries (the decode is lenient), and a nested fragment that names a sensitive key
   (`next=%2Fcb%23access_token%3DX`), are filtered by the SDK (§5.1 rules 2 and 5); web drops the whole query anyway. Stack-frame
   files — which carry the page URL for an inline script's frame — are filtered by the SDK and stripped by web (§5.1, §9.4).
   **Residuals, recorded rather than fixed** (each is also listed in the README's *URL privacy* section, §8, and pinned there by
   `tests/docs.test.ts`, §11.2 TASK-05):
   - a token that appears in text **not** as a URL query value (`console.log('token is', t)`, `new Error('bad token ' + t)`) is
     recorded as text (a console crumb bounded to 2048 bytes, a message to `MAX_MESSAGE_BYTES`); no filter can recognise it;
   - URL-shaped text `URL_IN_TEXT` does not recognise: a scheme-less host (`app.example.com/reset?reset_token=X`, whose `/`
     follows a word character), a bare query with no base (`?reset_token=X`, such as a logged `location.search`), and a bare
     fragment with no base (`hash is #access_token=X`, such as a logged `location.hash`). Neither the SDK nor web's
     `stripQueriesInText` touches them. The pattern needs a base because a `?` or `#` in prose (`why?`, `issue #12`,
     `#general`) is not a URL, and a pattern that started at either would rewrite ordinary text;
   - a query nested in a value **without a `?`** (`filterUrl('/login?next=reset_token%3DX')`, whose `next` decodes to
     `reset_token=X`) is kept. Rule 5 treats a value as a URL only when it holds `?`, a sensitive fragment or userinfo; reading
     every value that holds `=` as a query would rewrite ordinary values (`filter=status%3Dopen`, `q=a%3Db`). web is
     unaffected: it drops every query whole (§9.4);
   - a nested value encoded more than 3 times, or nested more than 3 levels deep, is kept as is below that depth (rules 2 and 5 are
     bounded so a hostile URL cannot make the filter loop); a nested fragment that holds a secret under no key, or under a key rule 3
     does not list (`#t0k`, `#x=t0k`), is kept, as the same fragment at the top level would not be (rule 1 drops every top-level
     fragment);
   - a **keyless** query part — one with no `=`, such as a token sent as the whole query (`/verify?<token>`) or as a bare part
     (`/p?a=1&<token>`) — is kept as is (§5.1 rule 4). Rule 3 has no key to test, and filtering every keyless part would rewrite
     ordinary URLs (`?print`, `?amp`, `?v2`). web is unaffected: it drops every query whole (§9.4);
   - a URL written into text **before it was encoded**, whose query holds an unencoded space, quote, `<` or `>`
     (`'GET /api/search?q=John Doe&token=abc'`, from `'GET ' + path` with a raw user-typed `q`). `URL_IN_TEXT`'s query ends at
     those characters, so the match is `/api/search?q=John` and ` Doe&token=abc` lies outside every match; `filterUrlsInText`
     keeps the token. web's `stripQueriesInText` uses the same pattern, drops only `?q=John` and keeps `Doe&token=abc`, so web
     is affected too. No filter rule is added: a query that ran on past a space would swallow the prose after every URL. The
     SDK's own URL fields (`request.url` / `query_string`, crumb `data.url` / `from` / `to`, frame files) are not affected,
     because they go through `filterUrl` / `filterQuery`, which split the whole string on `?`, `&` and `;`. Only URL-shaped
     **text** is affected (console arguments, an event's `message`), and only when an app builds it by hand;
   - text the app writes itself — an `addBreadcrumb` message, a `beforeSend`'s returned `message` — is the app's and is not filtered.

   For each, the processor's `MaskString` still runs server-side over the event `message` and crumb messages
   (`apps/processor-go/event/event.go`: the masking step and `maskV2`; S6) — it masks patterns (`apps/processor-go/masker/masker.go: MaskString`:
   card numbers, SSNs, `token=` / `password=` / API-key-style pairs, `Bearer …`), so it may catch the second residual's
   `reset_token=X`, but it does not recognise a bare token either. web's known carriers are all query parameters of a URL (W8), which both filters reach.
4. **Email.** web sends `me.email` although `User.isEmailPrivate` exists (W4). Decision 6 and decision 1 cover this; the reviewer
   should see that the flag is not consulted.
5. **`beforeBreadcrumb` fails closed, `beforeSend` fails open** (§4.3). The asymmetry is deliberate and is in the README. Because
   `beforeSend` fails open, web's hook strips first and guards its only foreign call (§9.3).
6. **`autoIpAddress` defaults to `true`** (contract §4.1). Behind the Sentinel tunnel it has no effect; behind any other proxy it
   stores that proxy's egress IP as the user's (§3.4). The README says so and names the switch.
7. **Server `request.headers`** now reach Sentinel from `captureRequestError` (previously none but `traceparent`). The contract's
   five credential headers are removed, sensitive-named headers are `[Filtered]`, and every header value containing `?` or `#` is URL-filtered (D6); `x-forwarded-for`
   and the like are stored, which decision 1 (IP storage is OK) covers.

### 12.3 NEEDS_DECISION

None.

### 12.4 Revision 2: internal review findings and where each was fixed

| # | Finding (severity) | Fixed in |
|---|---|---|
| R1 | Admitting `user` makes `applyClientIp` B4 forward `user: {}` and breaks the tunnel "no IP → no user" tests and `TestTunnelFixtureShape` (major) | §7.1 (empty `user` deleted), §7.2, §7.3, §11.3 rows |
| R2 | New fixtures depend on node version and host (`navigator` exists on node ≥ 21; `os.hostname()`) while CI compares byte-for-byte (major) | §7.3 (injectable provider, fixed values), SP20 |
| R3 | Next logs soft-navigation URLs with their query via `console.error`, reaching console crumbs (major) | §5.1 `filterUrlsInText`, §5.2, §9.4 `stripQueriesInText`, §9.5 test, §12.2(3) |
| R4 | `filterUrl` returns a bare query string unfiltered (its rule 5, rule 6 since revision 4), so `request.query_string` was unfiltered (major) | §5.1 `filterQuery`, §6.1, §6.4, §11.2 |
| R5 | Only `referer` was filtered among server headers; proxies add URL-bearing headers (major) | §6.4, D6, §9.4 |
| R6 | web's replay-id copy ran before the strip; a throw sends the pre-hook event (minor) | §9.3, §9.5 |
| R7 | Per-call `ctx.metadata.user_id` / `tenant_id` contradicted contract §4.1 and decision 7; edge runtime `version` unlisted deviation (minor) | §3.7 (per-call `ctx.user` / `ctx.tags`), §2, §11.3, D7 |
| R8 | `nextjs/client` and `nextjs/edge` have no own `init`, so they would name themselves `browser` / `edge` (minor) | §3.3 |
| R9 | §11.2's id-length wording contradicted §3.4 (minor) | §11.2 |
| R10 | §4.5 / §6.1 size arithmetic understated (minor) | §6.1, §4.5 |
| R11 | Crumbs retained from before `init` never passed the hook (minor) | §4.1 |
| R12 | `{{auto}}` behind a non-Sentinel proxy stores the proxy's IP (note) | §3.4, §12.2(6) |
| — | S4 omitted `contexts.client_os`; T9 omitted `SDK_NAME` (notes) | S4, T9 |

### 12.5 Revision 3: spec review round 1 findings and where each was fixed

Every finding is fixed; none is dismissed.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-tojson-object-400` (major) | `sanitize` replaces an object that has `toJSON` by its result (T16), so `setContext('x', new Date())`, `addBreadcrumb({ data: new URL(u) })` or a `beforeSend` setting `contexts.x = dayjs()` put a string where `ErrorPayload` declares an object, and the ingestor 400s the whole batch. | §3.6 (`isJsonObject` on the **sanitised** value; the call dropped, the scope unchanged); §3.1 contexts row; §4.2 `data` row and the paragraph after it (the same check on `addBreadcrumb`, `beforeBreadcrumb` results and `beforeSend` crumbs); §3.8 `contexts` / `breadcrumbs` rows and step 1 (a `toJSON` on the returned event itself); T16; SP21. Tests: §11.2 TASK-01 (`setContext` with `Date`, `URL`, `toJSON` → array, and `toJSON` → object accepted; `afterHook` `contexts.x` of each), TASK-02 (`addBreadcrumb` `data` of each, and via `beforeBreadcrumb`); §7.3 `v2-hostile-before-send.json` through the ingestor's strict decode. |
| `sdkjs-rc-spec-lookbehind-regex` (major) | `filterUrlsInText` (§5.1) and, by reference, web's `stripQueriesInText` (§9.4) used `(?<![\w/])`; web targets Safari 12 (W11), where a lookbehind literal is a parse-time `SyntaxError` that stops the chunk (and web hydrating) on iOS ≤ 16.3, and a `new RegExp` lookbehind throws inside `beforeSend`, which sends the event unstripped. | §5.1 (`URL_IN_TEXT` rewritten with the captured prefix `(^\|[^\w/])` put back on replace, and the *Regex syntax* rule with the audit table of every prescribed pattern — none needs syntax newer than Safari 12; `replace`, never `replaceAll`); §9.4 (web's own copy, the same rule, and why); W11, T18; SP22. Tests: `tests/core/es-compat.test.ts` (sdk-js, source scan + `URL_IN_TEXT.flags`), the same scan in web's `sentinel-url-privacy.test.ts`, each shown red with a lookbehind; `filterUrlsInText` cases proving the prefix behaves as the lookbehind did (§11.2 TASK-03, §9.5). |
| `sdkjs-rc-spec-web-route-test-omitted` (minor) | web's `src/app/api/sentinel/__tests__/route.test.ts` runs the real `@sentinel/sdk-js/nextjs/route` and pins v0.1.2's forward; 0.2.0 ships `f49721f` (`x-sentinel-derive-ip: off`, `applyClientIp`) and the widened `ITEM_KEYS`, so the test changes with the pin and was listed nowhere. | W12; §9.5 (the exact new headers `{ content-type, x-api-key, x-sentinel-derive-ip: 'off' }` and body `[{ message, error_class, platform, user: { ip_address: '10.0.0.1' } }]`, `SENTINEL_PROJECT_KEY` stubbed unset, and a new v2-item forward case); §10 web files; §11.2 TASK-06. |
| `sdkjs-rc-spec-stack-file-url` (note) | web's strip and the SDK's filter covered request, header and crumb URLs but not stack-frame files, which carry the page URL (query included) for an inline script's frame. | §5.1 (the SDK filters every `stacktrace[].file`, with why symbolication and grouping are unaffected, S9); §9.4 (`stripEventUrls` strips `stacktrace[].file` and, if present, `exception.values[].stacktrace[].file`); §12.2(3); SP24. Tests: §11.2 TASK-03 (a frame file `https://app/reset-password?reset_token=X` → `reset_token=[Filtered]`), §9.5 (the same file stripped to `https://app/reset-password`, top-level and exception frames). |
| `sdkjs-rc-spec-afterhook-overclaim` (note) | §3.8 claimed "no hook can make the ingestor 400 the event", but `afterHook` left `logger`, `dist`, `source`, `exception` and case-variant keys, and never type-checked `environment`, `release_version`, `fingerprint`, the trace fields or a frame's `int32` line (T17, S10). | §3.8 rewritten: a new object from `SentinelEvent`'s exact key set (the never-sent keys and case variants removed, a returned `toJSON` ignored); a row for every v1 field (`environment` falls back to the pre-hook value — deleting it would itself 400, since it is `required`; `release_version` / `fingerprint` / `trace_id` / `span_id` / `trace_flags` removed when invalid; frame `line` / `column` bounded to `int32`); §2; S10, T17; SP23. Tests: §11.2 TASK-01 (one case per removed key, per case variant and per new row) and the golden fixture `v2-hostile-before-send.json`, accepted by the ingestor's strict decode, `ValidatePayload` and protovalidate (§7.3). |
| `sdkjs-rc-spec-manual-proofs` (note) | §11.2 said every criterion's proof runs under a gate, but TASK-05's tag and TASK-06's staging observation are post-merge facts no gate can hold. | §11.2 *Proofs no gate can hold* (the coordinator records the tag SHA from `git ls-remote` and the occurrence ids read in Sentinel as card-criterion evidence); the TASK-05 / TASK-06 rows; §8; §11.4; the header; SP25. |

### 12.6 Revision 4: spec review round 2 findings and where each was fixed

Round 2 verified both round-1 majors fixed and found no new major. Both remaining findings are fixed; neither is dismissed.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-fetch-same-promise-unhandled` (minor) | §5 had the fetch wrapper record on settle and return "the same promise object". Attaching the recording reactions marks that promise handled, so an uncaught rejected `fetch` no longer fired `unhandledrejection`, and 0.1.x's own browser `unhandledrejection` capture stopped reporting it. The reviewer measured 0 events with the same-object wrapper and 1 with a chained rethrow. | §5 *Patching rules* (fetch is the one wrapper that returns something other than the original's value) and the new *No wrapper marks a page promise handled* (the chained `.then(record-and-return, record-and-rethrow)`, Sentry's `instrumentFetch` shape; a synchronous throw or a non-thenable return recorded nothing; the same reasoning applied to every other wrapper and listener: XHR `open` / `send`, `history`, console and click never react to a page promise and never cancel or stop an event); §5.3; §5.4; SP12, SP26. "The same promise object" is gone from the spec, and no test asserts identity with the original. Tests: §11.2 TASK-03 `tests/browser/fetch-rejection.test.ts` (an uncaught rejected fetch reaches the `unhandledrejection` capture exactly once, with the bridged event's `promise` being the returned one; a caught one produces none; a resolved one yields the same `Response`; shown red by both same-object variants), plus the XHR return and rethrow cases. The claim itself was re-checked in node before this revision: same-object 0 events, chained 1, caught 0. |
| `sdkjs-rc-spec-nested-url-in-query-value` (note) | `filterQuery` split the outer query on `&` and matched keys only, so the first parameter of a URL carried in a value (`/login?next=/reset-password?reset_token=X`) stayed inside the value and was never matched. `URL_IN_TEXT`'s query group also ran across a comma into the next URL (`a=1,https://h/q?reset_token=S`). | §5.1 rule 5 (a value whose decoded form holds `?` is passed through `filterUrl`, recursively to 3 levels, written back as is or through `encodeURIComponent` to match how it arrived; a value with `#` but no `?` is left alone), rule 4's cross-reference, `filterQuery` ("rules 2-5"), and rule 5 → 6 where cited; `URL_IN_TEXT` group 3 `((?:[^\s"'<>#,]\|,(?!https?:\/\/))*)`, an ES3 lookahead, still no lookbehind, with the *Regex syntax* rule and audit table saying lookahead is allowed; §9.4 (web's copy of the pattern, and why nesting needs nothing more there); SP9, SP22, SP27; §10. Tests: §11.2 TASK-03 (both brief examples, the encoded form, the outer-split case, `color=%23fff` and `ids=1,2,3` unchanged, the depth bound, the two-match pattern case, each shown red), §9.5 (web's `stripQueriesInText` / `stripUrl` on both examples). |


### 12.7 Revision 5: spec review round 3 findings and where each was fixed

Round 3 verified both round-2 fixes and found no major. All three remaining findings are fixed; none is dismissed, and the note
is closed rather than recorded. Every expected output below was checked against a node model of the revised §5.1 rules and
web's §9.4 strip before this revision was committed.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-url-comma-rule-regression` (minor) | Revision 4's comma rule in `URL_IN_TEXT` group 3, `(?:[^\s"'<>#,]\|,(?!https?:\/\/))*`, ended a query at **any** `,http(s)://`, including one whose URL has no `?` of its own. The rest of the outer query then belonged to no match: `filterUrlsInText('see /p?next=x,https://h/q&reset_token=S')` returned its input unchanged (revision 3 filtered it), and web's `stripQueriesInText` gave `see /p,https://h/q&reset_token=S`. | The comma rule is **dropped** on both sides: §5.1 `URL_IN_TEXT` (the query group runs across commas; rule 5 already filters `a=1,https://h/q?reset_token=S` inside the value), §9.4 (web's copy; over-stripping a second URL's base is harmless), the *Regex syntax* paragraph and audit table (no lookaround left), the header, §10, SP9, SP22, SP27. Tests: §11.2 TASK-03 (`filterUrlsInText('see /p?next=x,https://h/q&reset_token=S')` → `…&reset_token=[Filtered]`; the pattern test now expects **one** match over `see /p?a=1,https://h/q?reset_token=S`) and §9.5 (`stripQueriesInText` of the same → `'see /p'`; `'see /p?a=1,https://h/q?reset_token=S'` → `'see /p'`; the reviewer's RSC line `https://app/t?search=a,https://x.io&cf=Alice%20Tan` keeps no `cf=`), each shown red by revision 4's pattern. |
| `sdkjs-rc-spec-web-message-strip-unpinned` (minor) | `stripEventUrls` passed `event.message` through `stripQueriesInText`, but no §9.5 test asserted it, so deleting that line left every web test and `test:coverage` green. It was also the only guard on a URL inside an error message, since the SDK filtered URL text in console crumbs only. | §9.5: `sentinel-url-privacy.test.ts` and `instrumentation.test.ts` (through the client `beforeSend`) each run an event whose `message` is `'failed at /reset-password?reset_token=X'` and assert `JSON.stringify` contains neither `reset_token` nor `X`, shown red by deleting the `message` line; §9.4 says why the line is there. **Decided: yes, the SDK filters the message too.** §5.1 *Where the filters run on an event's `message`*: `build` runs `filterUrlsInText` over `message` on every capture path, before the bounds (the SDK sends no `exception`, §2, so `message` is where an error's value travels; `error_class` is not filtered; the processor's fingerprint does not hash the message, so grouping is unchanged; a `beforeSend`'s returned message is not re-filtered). SP28; §10; §12.2(3). Tests: §11.2 TASK-03 *Error messages* (`captureException`, `captureMessage`, the browser and node global handlers, the bound after filtering, the `beforeSend` exemption), shown red by deleting the call. |
| `sdkjs-rc-spec-url-filter-residuals-unrecorded` (note) | Rule 5 decoded a value once and treated it as a URL only when it held `?`, and `filterUrlsInText` kept fragments. So `filterUrl('/login?next=%252Freset%253Freset_token%253DX')`, `filterUrl('/login?next=%2Fcb%23access_token%3DX')` and `filterUrlsInText('go https://h/cb#access_token=S')` all came back unchanged, and §12.2(3) recorded none of them. | **Closed.** §5.1 rule 2: keys and values are decoded until stable, at most 3 steps, the count kept; rule 5: a decoded value is URL-shaped when it holds `?`, or `#` with a sensitive key in its fragment (`color=%23fff` and `#page=2` untouched), and a changed value is re-encoded as many times as it was decoded; `URL_IN_TEXT` gains group 4, the fragment, which neither `filterUrlsInText` nor web's `stripQueriesInText` writes back (contract §4.4, "Fragment (`#…`) is dropped"). The new pattern is also linear-time, where the revision-3/4 shape was quadratic on text with many `/` and no `?` (§5.1). Tests: §11.2 TASK-03 — the reviewer's three inputs give `/login?next=%252Freset%253Freset_token%253D%255BFiltered%255D`, `/login?next=%2Fcb` and `go https://h/cb`, shown red by decoding once, by deleting rule 5's fragment test, and by deleting group 4; plus `co%2564e`, `/p?color=%23fff`, `/p?next=%2Fp%23page%3D2`, a malformed `%zz`, and the linear-time case; §9.5 (web's `stripQueriesInText('go https://h/cb#access_token=S')`). What stays open — tokens not in a URL query, scheme-less and base-less URL text, nesting or encoding beyond 3, keyless nested fragments, app-written text — is listed in §12.2(3), in the README's *URL privacy* section (§8), and pinned there by `tests/docs.test.ts` (§11.2 TASK-05). SP9, SP27. |
### 12.8 Revision 6: spec review round 4 findings and where each was fixed

Round 4 found no major and no minor. Its one note is fixed in all three parts; nothing is dismissed. The two new expected
outputs were checked against a node model of the revised rule 2 before this revision was committed.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-url-residuals-list-incomplete-r4` (note) | §12.2(3)'s residual list was incomplete in two ways, and carried one stale clause. Rule 2 split a query on `&` only, so `filterUrl('/p?a=1;reset_token=X')` read one part with key `a` and value `1;reset_token=X`, and the token was kept. A keyless part (`/verify?<token>`) was kept by rule 4 but listed nowhere. And §12.2(3) said the SDK never records "URLs in crumb messages", while console crumbs do record them, filtered (§5.2). | **`;`:** §5.1 rule 2 splits on `&` and `;`, each part keeping its separator, so the rejoined query changes only where a value is filtered; rule 5's "outer split" wording and the *Regex syntax* audit table (`/([&;])/`) follow. Tests: §11.2 TASK-03 — `filterUrl('/p?a=1;reset_token=X')` is `/p?a=1;reset_token=[Filtered]` and `filterQuery('a=1;token=t&b=2')` is `a=1;token=[Filtered]&b=2`, shown red by splitting on `&` only. **Keyless part:** kept, not filtered — filtering every keyless part would rewrite ordinary URLs (`?print`, `?amp`, `?v2`); §5.1 rule 4 says so, §12.2(3) lists it as a residual with that reason and notes web is unaffected because it drops whole queries (§9.4), the README's *Not filtered* list names it (§8), `tests/docs.test.ts` pins the word `keyless` there (§11.2 TASK-05), and §11.2 TASK-03 pins `filterUrl('/verify?t0k')` as unchanged. **Stale clause:** dropped from §12.2(3). |

### 12.9 Revision 7: spec review round 5 findings and where each was fixed

Round 5 found no major and no minor. All four notes are fixed; nothing is dismissed. Every new expected URL output was checked
against a node model of rule 0, rule 5's userinfo condition and both `URL_IN_TEXT` replacements before this revision was
committed, and the marker byte counts against `Buffer.byteLength`.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-url-residuals-unlisted-r5` (note) | No filter removed userinfo, so `https://user:pass@h/p` was recorded with its credentials by the SDK and by web. Two inputs were kept by the filters and listed nowhere: a query nested in a value without `?` (`filterUrl('/login?next=reset_token%3DX')`) and a bare fragment in text with no base (`hash is #access_token=X`). | **Userinfo stripped:** §5.1 rule 0 (`stripUserinfo`: the authority after `//`, through its last `@`, WHATWG's rule), rule 5's third URL-shaped condition (a nested value with userinfo), rule 6's exception, `filterUrlsInText`'s replacement (`prefix + stripUserinfo(base) + …`), the *Regex syntax* audit table (the anchored scheme test); §9.4 web's `stripUrl` and `stripQueriesInText`; §12.2(3); SP29; §10. Tests: §11.2 TASK-03 (`https://user:pass@h/p?a=1`, a userinfo URL with no `?`, `//u@h/x`, `https://a@b:p@h/`, the untouched `https://h/users/@me` and `mailto:`, the nested encoded case, two text cases, each shown red), §9.5 (`stripUrl` twice and `stripQueriesInText` once, each shown red). **Residuals listed, no rule added:** §12.2(3) names both with the reason (a value holding `=` is not a query; a `?` or `#` in prose is not a URL), §8's README *Not filtered* list names both with their examples, §11.2 TASK-05's `tests/docs.test.ts` pins `location.hash`, `next=reset_token%3DX` and `#access_token=X`, and §11.2 TASK-03 pins both inputs as unchanged. |
| `sdkjs-rc-spec-afterhook-crumb-timestamp-contradiction` (note) | §3.8's `breadcrumbs` row removed a `beforeSend` crumb "whose `timestamp` cannot be normalised", while §4.2, which §3.8 applies, stamps an absent or unusable timestamp with now. | §3.8's row: such a crumb is kept and stamped now by §4.2's rule. Test: §11.2 TASK-01 (a `beforeSend` returning crumbs with `'not a date'` and year-10000 timestamps keeps both, each stamped with the faked clock, shown red by removing them). |
| `sdkjs-rc-spec-truncated-marker-after-metadata-shrink` (note) | §4.5 appended `breadcrumbs` (rule 3) or `contexts` / `tags` (rule 5) to `metadata.truncated` in `enqueue`, after `afterHook`'s shrink, so a `metadata` left near 65 536 went over, and the ingestor's shrink removed a key the SDK did not choose. | §4.5 rule 3: `shrinkMetadata` re-runs after the marker is added (rule 5 follows it), with the reason for re-running over reserving (a reservation taxes every event for a marker only trimmed browser events carry, and duplicates the ruler's arithmetic; the re-run costs nothing otherwise and only lowers `bytes`), and the `metadata_too_large` outcome; SP30. Test: §11.2 TASK-02 `tests/core/size.test.ts` near-cap case (65 530-byte metadata, crumbs trimmed, sent metadata ≤ 65 536 with `blob` in `truncated_keys`, shown red by deleting the re-run), and the rule-5 `contexts` variant. |
| `sdkjs-rc-spec-web-commands-omit-pnpm-check` (note) | §11.1's web command list omitted `pnpm check`, which `pr-checks.yml` runs first and which alone runs format, knip, ast-grep (`no-prose-comments`), structure, boundaries, secrets, docs and agents checks. | §11.1: `pnpm check` added after install, with what it covers and why the individual commands stay listed. |

### 12.10 Revision 8: spec review round 6 findings and where each was fixed

Round 6 found no major, one minor and one note. Both are fixed; nothing is dismissed. The boundary and near-cap outcomes were
produced by a node model of §4.5 rules 1-3 under both orders, and every new URL output by a node model of the lenient decode
and rules 0-5 under both decodes, before this revision was committed.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-keepalive-marker-after-trim` (minor) | §4.5 rule 2 removed crumbs until the event fitted 61 440, and rule 3 added the `'breadcrumbs'` marker afterwards. The marker (28 bytes, 14 when `truncated` exists) could push the event back over with crumbs still left. Rule 4 (no crumbs left) did not describe that state, so the event went without `keepalive`, although one more removal would have fitted. Rule 3 also claimed the re-run "never changes whether an event fits `keepalive`", which read as if the marker could not either. | §4.5: rules 2 and 3 are one loop whose measure includes the marker (added, with the re-run, immediately after the first removal, then `bytes` re-measured). Rule 3 now says the re-run can only help fit and the marker can hurt it. Rule 5 re-measures after each marker. The guarantee paragraph counts the marker and states the 28-byte window that goes by rule 4. SP31; §10. Tests: §11.2 TASK-02. The reviewer's boundary case is sized by measurement (one crumb kept, `keepalive`), shown red by revision 7's order. The near-cap case (two crumbs kept once the re-run removes `blob`) is shown red by deleting the re-run and by revision 7's order. |
| `sdkjs-rc-spec-url-nested-decode-failure-r6` (note) | Rules 2 and 5 decoded with `decodeURIComponent` over the whole string and read it raw when that threw. So one `%E9` (non-UTF-8) or a trailing `%` in a value hid its nested `?`, and the nested token was kept. §5.2 cut each console argument to 512 bytes **before** `filterUrlsInText`. An argument cut inside a `%XX` after a nested token therefore failed the decode the same way. | §5.1 rule 2: a lenient step decodes each maximal `%XX` run, falls back triplet by triplet, and keeps an invalid triplet as written. Rule 5 uses it, and the write-back of a raw triplet is stated. The *Regex syntax* audit gains the run pattern. §5.1 *Filter, then cut* states the order for every bounded URL-bearing string: console arguments, `message`, frame files, `request.url` / `query_string`, server headers. §5.2 filters each argument whole, then cuts. §6.1 / §6.4 state filter-then-cut for `request.url` and `query_string`. §12.2(3); SP32; §10. Tests: §11.2 TASK-03 — `%E9`, trailing `%` and a `%E9` in text (each red under the strict decode), a valid `%C3%A9` round trip, and the console argument cut inside `%26` after a nested `S3CR3T` (red by cutting first, under either decode). |


### 12.11 Revision 9: spec review round 7 findings and where each was fixed

Round 7 verified both round-6 fixes and found one major, one minor and one note. All three are fixed, and nothing is dismissed.
Every size in this table was measured before this revision was committed. Proto sizes are Go's
`proto.Size(mapping.MapPayloadToEvent(p))` on this repository's ingestor code, and estimates and shed outcomes come from a node
model of §4.6. The model was compared with the Go size on 56 events and was never below it.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-node-event-over-nats-max-payload` (major) | The spec bounded events by JSON only: §4.5 in the browser, S3's 1 MiB and the server's JSON bounds elsewhere. §4.5 claimed S3 was "not approached" on node. But the ingestor publishes each item as a protobuf `ErrorEvent`, and NATS refuses it over `max_payload`, 1 048 576 bytes. `metadata`, contexts and crumb `data` are `google.protobuf.Struct`, where `0,` costs 11 bytes against its JSON 2. So `setExtra('a', <32 700 zeros>)` plus 32 crumbs of `{ a: <2 030 zeros> }`, every part within its JSON bound, is 1 053 681 proto bytes. The item fails to publish and is lost. | **§4.6 (new)**: on every runtime, `enqueue` computes `protoSizeEstimate`. It counts every `Struct` / `Value` at its exact proto3 wire size and everything else by the skeleton's JSON plus 8 bytes per map entry. It is an upper bound, and the reasons are stated and measured. Over 900 000 bytes, the event sheds breadcrumbs oldest first, then contexts (non-well-known first), then metadata through `shrinkMetadata` with a proto measure. Each step keeps §4.5's markers inside the measure and runs its `shrinkMetadata` re-run. An event with nothing left to shed is sent. The budget is stated with its headroom (NATS headers, the ingestor's enrichment), and so is the deliberate contexts over-count. The browser figures are given (~338 KB with crumbs, ~895 KB crumb-less), and the rule runs there too. §3.8 drops a `request.data` over the server's 16 384 bytes (SP34). S3, S11 (new) and S2 (`request.data`); §2; §3.6; §3.8 step 3; §4.4's table; §4.5's closing paragraph; §7.3 (`v2-publish-bound.json`); §8; §10 (`src/core/proto-size.ts`); SP8, SP33. Tests: §11.2 TASK-02. `proto-size.test.ts` pins exact values, each equal to Go's `structpb` size. `size.test.ts` runs the reviewer's case, its `setContext('c', …)` variant, a contexts case, a metadata case, step 5, an under-budget case and the browser case. Each shed case is shown red by deleting §4.6, and the reviewer's case also by measuring with JSON bytes. The Go replay of `v2-publish-bound.json` asserts `proto.Size` ≤ 900 000. **Reported upward:** the ingestor accepts over HTTP what it cannot publish (§13 F4, §2). |
| `sdkjs-rc-spec-tenant-tag-lost-past-50-tags` (minor) | §3.5 rule 6 left the 50-tag cap to the server, which keeps the 50 lexically smallest keys. `tenant_id` sorts after most keys, so with more than 50 tags the server drops it. The processor's affected tenant is `tags["tenant_id"]`, and metadata no longer carries a fallback (§3.7), so the event lost its tenant. | §3.5 rule 6: over 50 effective tags, the SDK keeps `tenant_id` and the lexically smallest others, dropping the largest down to 49 others. It writes a debug line and adds `'tags'` to `metadata.truncated`, and the rule runs at build and again in `afterHook` (§3.8 `tags` row). Without `tenant_id`, the SDK's choice is the server's own. SP5; §8; §10. Tests: §11.2 TASK-01. `setTags` `a00`…`a49` + `setTenant('c1')` keeps `tenant_id` and drops `a49`, shown red by deleting the rule; there are also the no-tenant, per-call and `beforeSend` cases. |
| `sdkjs-rc-spec-url-text-unencoded-delimiter-r7` (note) | A URL written into text before encoding, whose query holds an unencoded space, quote, `<` or `>` (`'GET /api/search?q=John Doe&token=abc'`), ends its `URL_IN_TEXT` match at that character. The token after it is kept by `filterUrlsInText`, and by web's `stripQueriesInText` too. §12.2(3) did not list this. | **Listed, no rule added.** §12.2(3) names it with the example, why no rule is added, and why the SDK's own URL fields (through `filterUrl`) and web's field strips are unaffected. §8's README *Not filtered* list names it with the example. §11.2 TASK-05's `tests/docs.test.ts` pins `q=John Doe&token=abc`, and §11.2 TASK-03 pins the input as unchanged. SP35. |

### 12.12 Revision 10: spec review round 8 findings and where each was fixed

Round 8 verified the round-7 fixes and found one major, two minors and one note. All four are fixed, and nothing is dismissed.
Every size below was measured for this revision the way §12.11's were. Proto sizes are Go's
`proto.Size(mapping.MapPayloadToEvent(p))` on this repository's ingestor code, after `validation.ValidatePayload` and with
`project_key` / `project_id` cleared. Estimates come from a node model of `protoSizeEstimate` and §3.10. On eight more events
the model was again never below Go.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-publish-estimate-overcounts-contexts-r8` (major) | §4.6 estimated contexts as sent and defended the over-count as deliberate. But the server cuts contexts to 32 names and 65 536 bytes before it publishes, so an app whose contexts exceeded that lost its breadcrumbs to room the server's cut would have made anyway. With 100 crumbs (~130 KB) and a 1.2 MB context, the estimate was 1 328 187: every crumb was shed, and then the context too. Revision 9's reason for not modelling the cut, that a ruler mismatch could under-count, does not hold: the SDK already mirrors the metadata shrink, the tags cap, the crumb total and `request.data` with a ruler that is never below the server's. `request.headers` / `env` were likewise left to the server's bound. | **§3.10 (new)**: `boundContexts` and `boundStringMap`, the server's rules on the Go ruler (`metadataWireBytes`, `pairBytes`, `objectSize`), in the server's victim order, at `build` and again in `afterHook`, before the metadata shrink. A removed context adds `'contexts'` to `metadata.truncated`. The section argues why the SDK's measure is never below the server's (lone surrogates, U+0000), so the server's cut is a no-op on what the SDK sends. §4.6: step 1 now measures only what the server keeps; the "over-count, deliberately" paragraph is replaced by *What the estimate counts*, with the measured before and after; the headers / env paragraph states their bound and why step 5 still sends what is left. S2 (the rules, exactly); §2; §3.6; §3.8 (step 3, the `contexts` and `request` rows); §4.4's table; §6.4 (its header bound is §3.10's function); §8; §10 (`src/core/server-bounds.ts`); SP33, SP36. Tests: §11.2 TASK-02 case (8) pins the reviewer's case: node, 100 × `'m'.repeat(1_190)` crumbs (130 201 Go-ruler bytes), and `setContext('big', { s: 'x'.repeat(1_200_000) })`, which keeps all 100 crumbs and drops `big` with the marker (estimate 128 184, Go 122 324). The same holds with `{ a: Array(100_000).fill(0) }`. Both are shown red by deleting §3.10. Case (5) is rebuilt, because §3.10 now keeps 64 of its 80 headers: 64 headers plus 64 env entries still reach step 5 at 985 711, and the server would publish them. §11.2 TASK-01 pins every rule of §3.10. |
| `sdkjs-rc-spec-publish-case7-browser-unreachable-r8` (minor) | §11.2 TASK-02 case (7) said case (1) under `./browser` is trimmed by §4.5 to within 61 440 bytes. Its `metadata.a` alone is about 65 400 JSON bytes, so that cannot happen. | Case (7) now expects what §4.5 does: every crumb is removed, `metadata.truncated` contains `breadcrumbs`, and the event (about 66 200 bytes) is sent by rule 4, without `keepalive`. §4.6 step 1 stops (estimate 360 606, Go 360 329), so there is no `contexts` marker, and `metadata.a` (65 455 Go-ruler bytes with the marker) is kept. |
| `sdkjs-rc-spec-estimate-vs-proto-not-gated-r8` (minor) | The estimate's upper-bound claim was checked in Go on one fixture (`v2-publish-bound`). The model's 56-event comparison was not a gate, so an implementation that dropped a term could pass. | §7.3: every golden fixture and every route-tunnel fixture's `protoSizeEstimate` is committed as a sidecar, one per fixture, under `packages/sdk-js/tests/fixture-estimates/` (outside both Go globs, so never strict-decoded). It is written under the tunnel suite's `commit()` rules, now shared, and never written under `CI`. `TestSDKJSFixtures_AcceptedByIngestor` and `TestTunnelFixtureV2StrictDecode` assert `proto.Size` ≤ the estimate on every one, and `TestFixtureEstimatesHaveFixtures` catches orphans. `Taskfile.yml: test-unit` and `ci.yml`'s `sdk-js-node` drift checks gain the sidecars, and the golden corpus, which that gate did not cover. There are two new fixtures. `v2-estimate-numbers` (estimate 101 305, Go 100 404, JSON 24 141) goes red under a JSON measure. `v2-estimate-strings` (estimate 46 203, Go 44 803) goes red without the `8 ×` map-entry term (44 523) and with a UTF-16 skeleton count (28 303), and its pinned subtest keeps asserting the `8 ×` term is needed. §4.6 (the measurement paragraph); §10; §11.1; §11.2 TASK-02; SP37. |
| `sdkjs-rc-spec-rule5-drops-tenant-tag-r8` (note) | §4.5 rule 5 removed all of `tags`, so a browser event over 262 142 lost its `tenant_id`, which §3.5 rule 6 exists to keep. | §4.5 rule 5 removes every tag except `tenant_id`. The text gives the reason (S6) and the cost (one value under 1 250 bytes), and an event with no other tag gets no `'tags'` marker. §11.2 TASK-02 pins it: `setTenant('c1')` plus `a00`…`a48` on an event held over 262 142 by a 100-frame `beforeSend` is sent with `tags` exactly `{ tenant_id: 'c1' }`. That is shown red by revision 9's rule. SP8, SP38. |

### 12.13 Revision 11: spec review round 9 findings and where each was fixed

Round 9 verified the round-8 fixes and found two minors. Both are fixed as the reviewer proposed, and nothing is dismissed. The
byte counts below come from this revision's node model. The tests assert outcomes, not those counts.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-size-model-hook-request-and-rerun-drop-r9` (minor) | (A) §3.10 claimed every field the estimate counts is one the server keeps, but §3.8 kept a hook's `request.method` / `url` / `query_string` only as strings, at any length. The server cuts them to 16 / 2048 / 4096 code points, so a hook's 1 000 001-character URL made §4.6 shed all 100 crumbs of a node event for bytes the server never publishes, and made a browser event too large for the tunnel. (B) §4.5 rule 3 dropped an event with `metadata_too_large` in `enqueue` when a marker's `shrinkMetadata` re-run returned `false`. That happens when the exempt keys fit 65 536 alone and not with the marker, as with `setExtra('route', 'x'.repeat(65_480))`, so an event `afterHook` had passed was lost to a marker. | (A) §3.10 *Request strings*: `boundRequestStrings` removes U+0000 and then applies the server's cut, at `build` and in `afterHook`. §3.8's `request` row uses it, and its `breadcrumbs` row now keeps the last 100 of a hook's array. §3.8 *Every typed field a hook can set* audits every field against its SDK and server bound, and those two were the only gaps. Also §3.1, §3.10 *What it changes for §4.6*, §4.4's table, §4.6 and SP39. Tests: §11.2 TASK-01 (each cut, the U+0000 strip, a code point above U+FFFF at the boundary, 150 hook crumbs → the last 100), and TASK-02 case (9). On node, 100 × 1 190-character crumbs and a 1 000 001-character hook URL keep all 100 crumbs. In the browser, the event fits 61 440 with the newest crumbs and is sent with `keepalive`. Each is shown red by deleting the cut. (B) §4.5 rule 3: on `false`, `metadata` goes back to its copy from before the marker, which withdraws it; the step continues, and `enqueue` never drops. §4.6 steps 2-3 follow it. `build` and `afterHook` withdraw this feature's `'contexts'` / `'tags'` markers the same way before 0.1.x's drop. SP40; §13 F5 records 0.1.x's own drop. Tests: §11.2 TASK-02 case (10), browser, node (§4.6 step 2) and `build`, each with `setExtra('route', 'x'.repeat(65_480))` and each shown red by revision 10's rule, and the 70 000-character `route` still dropped at `build`. |
| `sdkjs-rc-spec-browser-api-floor-structuredclone-r9` (minor) | §4.4 said each crumb is "cloned" at capture without saying how, which invites `structuredClone` (Safari 15.4). The spec stated a Safari 12 floor for regex syntax only, not for built-ins. | §4.4: the copy is `{ ...crumb }` with `data` through `sanitize`, since crumbs are plain data after §4.2. §5.1 *Built-in API floor* sits beside the regex list. It gives the floor (Safari 12) and the nine APIs Next 15.5's `@next/polyfill-module` supplies, read from web's `next@15.5.26`, and says why SDK code does not count on them. A table lists every API past Safari 12 that the spec's code could reach for, and what it does instead. The audit of the prescribed browser code found three more places that invited one, and each is now written out: the scope `Map` → event object (§3.5, §3.6; `Object.fromEntries`), §3.10's lone-surrogate repair (`toWellFormed`) and §3.10's victim sort (`toSorted`). Nothing else prescribed goes past Safari 12. §13 F6 records 0.1.x's own post-Safari-12 calls. `tests/core/es-compat.test.ts` (§11.2 TASK-03) and web's scan (§9.5) fail on every listed API. The sdk-js scan covers `src/` except the node-only upload CLI, and passes on today's `src/`. It is shown red by `structuredClone(crumb)` and by `s.toWellFormed()`. §10; SP41. |

### 12.14 Revision 12: spec review round 10 findings and where each was fixed

Round 10 verified both round-9 fixes and found one minor. It is fixed under the coordinator's two rulings, and nothing is
dismissed. The byte counts below come from this revision's node model, checked with Go's `proto.Size(MapPayloadToEvent(p))`
after `ValidatePayload` on `dde0ea2`. The tests assert outcomes, not those counts.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-size-model-budget-sheds-publishable-r10` (minor) | Two failing cases of the rule that the SDK never sheds or drops an event the ingestor would publish. (a) §4.6's budget of 900 000 kept 148 576 bytes of headroom, although what the ingestor adds after the SDK is at most ~21 KB, and the sibling sdk-go spec uses 1 000 000 for the same model. The reviewer's node event with 28 crumbs estimates 988 224 and is 986 550 bytes in Go, so the ingestor would publish it, yet sdk-js shed 4 crumbs. (b) §13 F5: `setExtra('route', 'x'.repeat(70_000))` was dropped at `build` with `metadata_too_large`, though `bounds.go: shrinkMetadata` removes exempt keys last instead of dropping, so the ingestor would publish it. | (a) Ruling: 1 000 000, as in sdk-go. §4.6 *The budget, 1 000 000, and why* itemises the additions and the 48 576 bytes left for them; the steps, §4.4's table, `NATS_PUBLISH_BUDGET` (§10), §8's CHANGELOG line and SP33 / SP42 use it. §11.2 TASK-02 recomputes cases (1) (28 crumbs kept, estimate 988 256, Go 986 582) and (2) (12 kept, 989 138, Go 988 277), and adds case (1b), the reviewer's 28-crumb event, sent whole and shown red by 900 000. Case (5) no longer reached step 5 (its event stops in step 4 at 985 707, now pinned), so it gains a 9 999-byte message and a 4096-code-point query string: 1 008 008 estimated, 1 006 977 in Go, JSON 1 006 973, still sent. `v2-publish-bound`'s window becomes ≤ 1 000 000 and > 950 000 (§7.3). (b) Ruling: F5 is not an exception. §3.7 *The shrink never drops an event*: the server's 18 exempt names replace `SHRINK_EXEMPT_KEYS` (`route` is not among them), the exempt keys are removed last in the server's order, and `truncated_keys` takes the server's capped shape when only the markers are left. `runtime` is never removed, because enrichment reads it before the server's shrink (S5). `user_id` / `tenant_id` / `request_id` are protected only because the server protects them. `metadata_too_large` is no longer emitted. §4.5 rule 3's marker withdrawal is removed, because the re-run can no longer fail, and §4.6 steps 2 and 4 say so. §6.4, §7.3's near-the-cap fixture (`request_id` in place of `route`), §8, §10, §11.3 (the two B11 rows, B3 and near-the-cap) and SP40 follow. Tests: §11.2 TASK-02 case (10), rewritten: `setExtra('route', 'x'.repeat(70_000))` is sent with `route` removed and the marker written, and the browser, node and `build` cases each keep their marker. There are pins for the exempt keys going last in the server's order and for the `truncated_keys` cap. F5 leaves §13. |

### 12.15 Revision 13: spec review round 11 findings and where each was fixed

Round 11 verified round 10's fix and found one minor and one note. Both are fixed, and nothing is dismissed. The figures below come
from a replay of `bounds.go: shrinkMetadata` and were checked by running `validation.ValidatePayload` on `dde0ea2` over the same
metadata. The tests assert the kept key set and the marker, not the byte counts.

| Finding (severity) | What was wrong | Fixed in |
|---|---|---|
| `sdkjs-rc-spec-shrink-uncapped-marker-sheds-server-kept-keys-r11` (minor) | §3.7 capped `truncated_keys` only in step 3, once `runtime` and the markers were all that was left, so steps 1 and 2 measured the full list of removed names. The server's `shrinkMetadata` measures its capped `removedKeys` on every removal. For `setExtra('request_id', 'r1')` plus 3 000 extras with 60-character keys, the full list (~189 KB) never fit, so the SDK removed every extra and then `request_id`, an exempt key, while the server keeps `request_id` and 958 extras. §11.2 TASK-02 case (10)'s *The cap* expected `{ runtime, truncated_keys }`, which pinned the defect. | §3.7: `truncated_keys` is written and measured in the server's capped shape (first 50 names, U+0000 removed, each cut to 128 UTF-8 bytes, then `"+N more"`) before every measurement of steps 1 and 2, and step 3 is folded into that rule; the SDK never removes more than the server's shrink would. §11.2 TASK-02 case (10), *The cap*: the input gains `setExtra('request_id', 'r1')`, and the expected outcome is the server's (958 extras `k…02042`-`k…02999` kept, `request_id` and `runtime` kept, `truncated_keys` of 50 names and `'+1992 more'`; SDK 65 491 bytes, `ValidatePayload` 65 530 with its own markers), shown red by revision 12's full-list measure, which sends `{ runtime, truncated_keys: [50 keys, '+2951 more'] }`. §8's CHANGELOG line and SP40 follow. |
| `sdkjs-rc-spec-reportcode-symbol-drift-r11` (note) | §3.7 named the error-code union `ReportCode`; the symbol is `SentinelSdkErrorCode` (`src/core/types.ts`). | §3.7, the only occurrence. |

---

## 13. Findings outside this scope (recorded, not fixed)

| # | Finding | Why not here |
|---|---|---|
| F1 | `docs/sdk-specification.md`'s generic `max_breadcrumbs` row says the default is `50`, while the ingestor keeps 100 and sdk-js (this spec) defaults to 100. | Only the sdk-js wording is in scope (be.md); the generic default belongs to the SDK spec's owner. |
| F2 | The tunnel's Go replay never strict-decoded its fixtures (D4); `TestTunnelFixtureV2StrictDecode` covers the route fixtures from now on, but `tunnel-user-rebuild.json` (the exported `applyClientIp` output, not a forward) stays on its own test. | Adding the strict decode is in scope; restructuring the older test is not. |
| F3 | web's reset-password, accept-invitation and four OAuth callback pages leave their tokens in the address bar (W8), where the browser's history, `document.referrer` of the next page and any third-party script can read them. | A web product/security change, not an SDK adoption; this spec only keeps them out of Sentinel. Reported for the web owners. |
| F4 | **REPORTED UPWARD.** The ingestor accepts over HTTP an event it cannot publish to NATS. Its JSON bounds (S2, S3) admit an event whose `ErrorEvent` proto exceeds NATS's `max_payload` of 1 048 576 bytes. Spec review round 7's event is 194 490 JSON bytes and 1 053 681 proto bytes. The publish fails, and the item is counted `failed`; a lone item gets 400, which the SDK retries as transient until its attempts run out (S11). Any client can produce such an event: another SDK, an older sdk-js, or `curl`. The ingestor has no proto-size check before publishing, and nothing tells the sender why. | Contract §2 forbids changing the ingestor in this feature. §4.6 keeps this SDK's events under the limit, but only the ingestor can close it for every client. Candidates for its owner: measure `proto.Size` in `IngestAt` and answer `validation failed` (400, not retried) or shed server-side; raise `max_payload`; or bound Struct fields by proto size instead of JSON. Recorded here for the coordinator to route. |
| F6 | 0.1.x's browser code already calls four built-ins that Safari 12 lacks: `globalThis` (12.1; `src/core/state.ts: getSlot`), `queueMicrotask` and `AbortController` (12.1; `src/core/transport/transport.ts`), and `crypto.randomUUID` (15.4; `src/core/capture.ts: newEventId`). `capture` calls `newEventId` before its `try`, so on Safari 12.1-15.3 `captureException` throws a `TypeError` into its caller. | This change neither introduces nor extends them: it adds no call past Safari 12 (§5.1 *Built-in API floor*), and the scan's list leaves these four out so that it passes on 0.1.x's code. A fix (a `getRandomValues` fallback for the id, a `self` / `window` fallback for the slot) changes 0.1.x's code and belongs to its owner. Reported for the coordinator to route. |

F5 (0.1.x's `metadata_too_large` drop when its exempt keys alone were over 65 536) was listed here until revision 12. The
coordinator ruled it is not an exception to the never-drop rule, so §3.7 now fixes it. The number is not reused.
