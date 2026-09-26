# Sentinel sdk-go rich context: implementation plan

**Feature:** `sentinel-sdk-go-rich-context` · **Repository:** `sentinel`, side `be`, module `packages/sdk-go`
**Spec:** [`spec.md`](./spec.md) @ **`fd69055`** (passed spec review round 14: 0 majors, 0 minors). This plan does not
change the spec's substance. Where it adds something, it is a test, or it is the one open note below, which is an
implementation step inside the spec's own size model.
**Contract:** `prd/sentinel-sdk-go-rich-context/contract.md` and `be.md` @ **`e030211`**. Pin this SHA in the
implementing seat's packet.
**Base:** `main` @ `dde0ea2` (v0.2.39). The spec's facts T1-T41 were transcribed there. Nothing in `packages/sdk-go`
has changed since `packages/sdk-go/v0.3.2` (T22).
**Work items:** `TASK-01` … `TASK-04`. One implementing seat builds TASK-01, TASK-02 and TASK-03 as **one PR**. The
coordinator does TASK-04 after merge (§6).

Section numbers written `spec §n` point into `spec.md`. Proof ids (`P1.6`, …) are spec §10.2's. Commit ids (`C1`, …)
are this plan's.

---

## 1. Invariant, primitives, and where it is tested

**The invariant.** Before this change and after it, every event `packages/sdk-go` sends is accepted and published by
the unchanged ingestor. No value it carries makes a body answer 400 or 413, and no event is lost at the NATS
publish for its size. The change adds one more obligation: both producer paths, `Client.CaptureErrorContext` and
`NewEvent` + `ApplyContext` + `Transport.Push`, emit the same typed fields for the same context. The SDK never
writes into a value it did not allocate for the event.

**Primitives considered, in order (closest first).**

| Need | Existing primitive | Decision |
|---|---|---|
| Per-request state on `context.Context` | copy-on-write `WithTag`/`getTagsMap` under one context key (`packages/sdk-go/context.go:59-89`) | **Bent, not replaced.** Each piece of state gets its own unexported key with the same copy-on-write discipline (spec §4.1). A second, mutex-guarded object is added only for the breadcrumb ring and the request scope, because only those are appended to after the context is derived. |
| Client-side bounds | none in sdk-go. The server's own `apps/ingestor-go/validation/bounds.go` (`applyBounds`, `boundContexts`, `shrinkMetadata`, `nulMap`, `metadataExemptKeys`, `wellKnownContexts`) | **Copied deliberately (B5), not imported.** sdk-go is a separate module with no dependency on the ingestor (spec §2; `go.mod` requires zerolog and otel/trace only, T16). The copy's drift is caught loudly: a `server_truncated` marker shows up on a golden in P1.8's root half (spec §11.3). Rejected: importing `validation`, which would add the root module and protobuf to every consumer's graph and break the contract's "no new dependencies". |
| Per-event marshal and batching | `Transport.sendBatch` (`packages/sdk-go/transport.go:151-255`): one `json.Marshal` of the whole batch, a lone event sent to `{endpoint}`, and `dropBatch` on a marshal error (`:163-171`) | **Extended in place.** The retry, 429, 4xx and partial-failure logic (`doSend` `:256`, `handlePartialFailure` `:344`, `dropBatch` `:374`) is kept as is. Only the body construction in front of it changes (spec §3.5). |
| Wire proof across the two Go modules | `tests/unit/sdk_js_fixture_test.go:54-110` (`TestSDKJSFixtures_AcceptedByIngestor`) decodes committed SDK output strictly into `validation.ErrorPayload`, then runs `ValidatePayload`, `MapPayloadToEvent` and protovalidate. `:172` does the same for a committed sdk-go copy. | **Reused as the pattern** for `tests/unit/sdk_go_wire_test.go` (D6): committed goldens under `packages/sdk-go/testdata/`, read by relative path, under `GOWORK=off`. Rejected: extending `tests/contract` only. That suite runs only in workspace mode and only on `workflow_dispatch` (T18), so it gates nothing. |
| PII key matching | `pii.go: defaultPIIKeys`, `isSensitiveKey` (`packages/sdk-go/pii.go:12,52`) | **Reused as the single list** for metadata, tags, header names and query parameter names. It is widened to the documented list (spec §6.2 item 3). |
| Proto size of what is sent | none in sdk-go. The module may not depend on protobuf (§2). | **New:** `publishcost.go`. This is the one new mechanism, and the reason is that no existing primitive can see a protobuf size (spec §3.5, §11.2 item 26). |

**Test boundaries.** The feature is judged at two boundaries that already exist, plus two supplementary ones:

- **B1**, `packages/sdk-go` unit tests under `GOWORK=off` and `-race`: every P-row except P1.7/P1.8's root half and P4.2.
- **B2**, `tests/unit` (root module, `GOWORK=off`): the golden bridge, which is P1.7 and P1.8's root half.
- **Supplementary:** `tests/contract` (workspace mode, `-tags=contract`) and `tests/e2e` U8. Both run in the
  coordinator's `workflow_dispatch`. Neither gates a commit.

Every commit below names the boundary its proofs sit at.

---

## 2. Shape of the work: one seat, one PR, nine commits

One implementing seat, on one branch, opens **one PR** for TASK-01, TASK-02 and TASK-03. Nothing is split across
seats. The three tasks share `context.go` (`ApplyContext` gains a row in C4, C5 and C7), `transport.go` and
`event.go`, so separate PRs would conflict with each other at every step.

**Ordering principle.** The riskiest work comes first:

1. **C1**, the one external risk. It is whether the self-hosted runner can run `-race` (T21, T32). Landing it first
   means the coordinator *may* dispatch CI on the first pushed commit and retire that risk before any feature code
   exists.
2. **C2 and C3**, the size model. This is where fourteen spec review rounds went (spec §13), and it is the only
   place an error loses events silently.
3. **C4**, the copy-on-write and concurrency rules of the context API.

Everything after C4 is additive surface on a proven core.

**Every commit builds and passes its own gate (§4.2) before the next commit starts.** No commit leaves a red test
for a later one to fix. A proof lands in the commit that makes it green, and §5 names that commit.

### C1 — gate plumbing (D5)

- **Files:**
  - `Taskfile.yml`: `test-unit` gains one `cmds` line, `cd packages/sdk-go && GOWORK=off go test -race -count=1 ./...`,
    **directly after `- go test ./apps/ingestor-go/...`** (`Taskfile.yml:195`, spec §9). No other line of the file changes.
  - `.github/workflows/ci.yml`: the `go-sdk` job's `go test` step (`ci.yml:148-150`) becomes `run: go test -race ./... -count=1`
    with `env: CGO_ENABLED: "1"`. The `if:` guard is unchanged. No `continue-on-error` and no fallback step.
- **Proves:** the existing 29 sdk-go tests pass under `-race` on both toolchains (T21 says they do today). The
  plumbing is D5's.
- **Why first:** see the ordering principle.

### C2 — v2 wire types, automatic fields, level mapper, bounds steps 1-3, every body to `/batch` (TASK-01)

- **Files:**
  - `event.go`: spec §3.1 types and constants (`SDKName`, `SDKVersion = "0.4.0"`, `Level*`), the new `Event`
    fields, and the extended B5 doc comment. `NewEvent` fills fields per spec §3.2: level, sdk, `server_name`
    (`sync.Once` hostname), per-event `runtime`/`os`/`device` maps, and non-nil `Tags`/`Metadata`. Adds
    `NormalizeLevel` (spec §3.3).
  - `config.go`: `ServerName`. `withDefaults` clamps `MaxBreadcrumbs` to 100.
  - `breadcrumbs.go` (new): only the constants `DefaultMaxBreadcrumbs` and `MaxBreadcrumbsLimit`. The breadcrumbs
    row needs them.
  - `pii.go`: the widened `defaultPIIKeys` only. The tags row filters by it. `ScrubPII` changes in C4.
  - `bounds.go` (new): `validUTF8` (one U+FFFD per invalid byte), every typed row of spec §3.4, both timestamp rules,
    the `event_id` re-mint and `validTag`. It also holds the **decoded** rows (U+0000 with the server's collision
    rule, `contexts` and `metadata` at 61,440 bytes in the server's order, copies of `metadataExemptKeys` and
    `wellKnownContexts`, the SDK's `truncated`/`truncated_keys` marker, `request.data`/crumb `data` sizes, and the
    crumb list's ingestor-shape byte bound), and the decode mirror type.
  - `transport.go`: `sendBatch` runs spec §3.5 steps 1-3 per event (bound a shallow copy, marshal alone, decode
    `[raw]` as the ingestor does, remove a failing entry alone, bound the decoded fields and send the decoded form).
    Every body is a JSON array posted to `{endpoint}/batch`, built from per-event bytes. `Push` gains the ownership
    doc comment.
- **Tests (B1):**
  - `testutil_test.go` (new): `newTestTransport`, which calls `t.Fatal` on a zero `MaxBufferSize`, `BatchSize` or
    `BatchWait` (spec §10.1). Also a recording server, and a decoding server that answers 400 as `handleBatchIngest`
    does.
  - `bounds_test.go`: **P1.6**, every case except the `WithTenant`/`WithTag` half of `TestBoundTagsKeepsTenantID`.
    Its direct-`Event.Tags` half (`tenant_id` plus `k00`…`k49` assigned on the event) lands here.
  - `level_test.go`: the **P1.4** `TestNormalizeLevel` case.
  - `event_rich_test.go`: the **P1.5** tests `TestServerNamePrecedence`, `TestAutomaticContexts` and
    `TestAutomaticContextsInnerMapsPerEvent`.
  - `pii_test.go`: **P3.9** `TestPIIKeysCoverSpecList`.
  - `client_test.go: TestCaptureErrorContextCarriesFingerprintAndErrorClass` now decodes a one-element array, and
    `transport_test.go: TestTransportBatchFlushing` drops its bare-object branch (`transport_test.go:18`). Both are
    listed in spec §9.
- **Existing tests that must stay green unchanged:** `integration_test.go`, `sentinelslog`, `sentinellog` and the
  `sentinelzerolog` servers. They answer 202 with an empty body, which `handlePartialFailure` already accepts
  (`transport_test.go:146`).

### C3 — publish cost, step 4, body cutting, the rc14 note, and the goldens (TASK-01)

- **Files:**
  - `publishcost.go` (new): `publishCost(b, m)` with parts 1-5 of spec §3.5, **part 1 as amended by the open note
    (§3 of this plan)**, the `struct.proto` size functions (`vl`, `ld`, `V`, `S`) over the decode mirror, and
    `maxPublishBytes = 1_000_000` and the 4,096 margin.
  - `transport.go`: step 4. Two measurements: the one-element array against 5,242,880, and `publishCost` against
    1,000,000, both compared with `>`. The shed order is (a) headers/env, (b) `request.data`, (c) breadcrumbs,
    (d) drop with `OnError`. Shedding happens on `b`, never on the pushed event. Bodies are cut at 5,242,880 bytes
    (commas and brackets counted) and at 500 events.
  - `testdata/wire_v2_bounded.golden.json`, `wire_v2_bounded_crumbs.golden.json`, `wire_v2_bounded_numbers.golden.json`,
    `wire_v2_shrunk.golden.json`, `wire_v2_nul.golden.json`, **`wire_v2_escapes.golden.json` (plan addition, §3)**,
    `wire_v2_publish_cost.json`.
  - `tests/unit/sdk_go_wire_test.go` (new, D6).
- **Tests:**
  - `publishcost_test.go` (B1, new): table tests of `vl`/`ld`/`V`/`S` against the spec §3.5 formulas, and of
    `jsonStringExcess` (§3).
  - `bodycap_test.go` (B1): **P1.11** in full, plus the two rc14 cases of §3.
  - `wire_golden_test.go` (B1): **P1.8**'s five tests, plus `TestWireGoldenEscapes` (§3).
  - `tests/unit/sdk_go_wire_test.go` (B2): `TestSDKGoBoundedWireNeedsNoServerCut` and
    `TestSDKGoPublishCostBoundsProto`, which is **P1.8**'s root half, over every entry of `wire_v2_publish_cost.json`.
- **The request in the goldens.** P1.8 uses "the same fixed `Config` and request as P1.7", and P1.7 builds its
  request through `NewHTTPMiddleware`, which does not exist until C7. So C3's `wire_golden_test.go` builds that
  request with one helper, `goldenScope(t, ev)`. The helper assigns exactly what `ApplyContext` will add from P1.7's
  scope: `Request` (`GET`, `http://api.example.test/items/42`, `id=7&token=[FILTERED]`, the fixed `Accept`,
  `User-Agent` and `Host` headers), `Transaction` `/items/42`, and `user.ip_address` `192.0.2.1` where the user has
  none. C7 replaces the helper's body with the real middleware drive, and **the goldens must not change**. A diff
  there is a snapshot defect or a helper error. It is investigated, never `-update`d away. This lets the
  estimate-against-`proto.Marshal` proof land in the third commit instead of the eighth.
- **Golden rule:** every golden and every P1.11 case assigns its volatile values before `Push` (spec §10.2), so
  `wire_v2_publish_cost.json` holds on both toolchains.

### C4 — the context API, copy-on-write, `ApplyContext`, the `Client` path, the zerolog hook (TASK-01)

- **Files:**
  - `context.go`: per-state unexported keys. `WithUser` and `WithUserInfo`. The `WithTenant`/`WithTag` routing
    (rules 1-5, nil removes, one key one store). `WithContextMap` removes the key from tags. Adds the readers
    (`UserFromContext`, `TagsFromContext`, `MetadataFromContext`), `WithLevel`, and the **metadata-routed table** with
    each row's reader in a comment. Adds `ApplyContext` with its metadata, level, user, tags, trace and
    `WithFingerprint`/`WithErrorClass` rows (the breadcrumb row comes in C5, the request-scope and client-IP rows in
    C7). It never writes through.
  - `event.go`: the unexported pre-options record of `ErrorClass`/`Fingerprint`, and the flags set by
    `WithEventErrorClass`/`WithEventFingerprint` (spec §3.2).
  - `pii.go`: `ScrubPII` becomes a deep copy through `map[string]interface{}` and `[]interface{}`, and cycle-safe
    (spec §6.2 item 3).
  - `client.go`: `CaptureErrorContext` becomes nil check → sampling → merge → `NewEvent` → `ApplyContext` → `Push`
    (`client.go:47-86`; the `BeforeSend` step is added in C6).
  - `sentinelzerolog/hook.go`: `LevelFromZerolog`. The hook fires on Error/Fatal/Panic only, through
    `CaptureErrorContext(WithLevel(e.GetCtx(), …))` (spec §3.6; today's code is `hook.go:16-22`).
- **Tests (B1):**
  - `context_test.go`: `TestContextHelpers` is rewritten for the new routing (T20), and `TestWithTagRouting` is
    **P1.3 (b)**.
  - `metadata_path_test.go`: **P1.3 (c)**.
  - `event_rich_test.go`: `TestCallerSetContextWins`, `TestApplyContextMergesUserFieldByField` and
    `TestApplyContextOptionWinsOverMetadataKeys` (**P1.5**).
  - `bounds_test.go`: the context half of `TestBoundTagsKeepsTenantID` (**P1.6**).
  - `pii_test.go`: `TestScrubPII` updated, plus the cases for a list inside a map and for a cycle.
  - `sentinelzerolog/hook_test.go`: `TestLevelFromZerolog`, `TestLevelFromZerologIgnoresLevelValueGlobals` and
    `TestHookSendsTypedLevel` (**P1.4**).
  - `event_refs_test.go`: `TestSDKNeverWritesCallerValues`, with path (i) and the third and fourth cases (**P1.10**,
    the parts that need neither `BeforeSend` nor the middleware).
- **Transient, stated so nobody "fixes" it:** until C7, `HTTPMiddleware` (`middleware.go:9-37`) still calls
  `WithTag(ctx, "http_method", …)`, so between C4 and C7 those keys route to `tags`. No test asserts either location
  in between, and C7 removes them.

### C5 — breadcrumbs on `context.Context` (TASK-02)

- **Files:**
  - `breadcrumbs.go`: the buffer header, a lazily allocated mutex-guarded ring, and `WithBreadcrumbBuffer`.
    `AddBreadcrumb` normalizes the timestamp, level and empty `Data` on the way in, and makes a shallow copy of
    `Data`. `BreadcrumbsFromContext` returns a new slice with a top-level `Data` copy per crumb.
  - `context.go`: the `ApplyContext` breadcrumb row.
- **Tests (B1):** `breadcrumbs_test.go`, which holds these proofs:
  - **P2.1**: `TestBreadcrumbsConcurrentSameContext`.
  - **P2.3**: `TestBreadcrumbBufferCap` and `TestTransportTrimsToConfigMaxBreadcrumbs`.
  - **P2.4**: `TestAddBreadcrumbWithoutBufferIsNoop`.
  - **P2.7**: `TestAddBreadcrumbNormalizes`.

### C6 — `BeforeSend` (TASK-03)

- **Files:**
  - `config.go`: `BeforeSend func(*Event) *Event` tagged `json:"-"`, and `(*Config).ApplyBeforeSend`, which
    recovers a panic, logs it always, and drops the event (spec §6.1).
  - `client.go`: calls `ApplyBeforeSend` after `ApplyContext`, and returns on nil.
- **Tests (B1):**
  - `beforesend_test.go`: **P3.1**, **P3.2** and **P3.4**. P3.3 asserts `Request`, so it waits for C7.
  - `event_rich_test.go`: `TestAutomaticContextsRaceBeforeSend` (**P1.5**).
  - `event_refs_test.go`: `TestSDKNeverWritesCallerValues` path (ii), `TestBreadcrumbDataRaceBeforeSend` and
    `TestMetadataListRaceBeforeSend` (**P1.10**, `-race`).

### C7 — `HTTPMiddleware` fills `request` and `transaction` (TASK-03)

- **Files:**
  - `middleware.go`: `MiddlewareOptions` and `NewHTTPMiddleware`. `HTTPMiddleware` becomes a wrapper around them.
  - The eager request scope plus buffer header. A nested middleware reuses the outer scope.
  - A lazy `sync.Once` snapshot of `method`, `url` and `query_string`. The query is split on `&` and `;`, and a name
    that does not unescape is matched raw.
  - The header filters: the credential names, patterns and `-key` suffix; the client-IP names, patterns and value
    rule; and the PII list. URL-valued headers get the query rule, with fragment and userinfo stripped and an
    unparsable value dropped. `Link` is dropped, and `Host` is added.
  - The client IP on opt-in, and the recovered panic with `WithLevel(…, LevelFatal)`. The `http_*` metadata writes
    are removed.
  - `middleware_pattern_go123.go` (`//go:build go1.23`) and `middleware_pattern_pre123.go` (`//go:build !go1.23`).
  - `context.go`: the `ApplyContext` request-scope, transaction and client-IP rows.
  - `wire_golden_test.go`: `goldenScope` now drives the real middleware. **No golden may change** (C3).
- **Tests (B1):**
  - `middleware_test.go`: **P3.5**, **P3.7** and **P3.8**. It is rewritten (T20).
  - `middleware_pattern_go123_test.go`, which opens with `//go:build go1.23` and `//go:debug httpmuxgo121=0`, and
    `middleware_pattern_pre123_test.go`, which registers `"/items/"`: **P3.6**.
  - `middleware_lazy_test.go`: **P3.10**.
  - `breadcrumbs_test.go`: `TestBreadcrumbsSeparateRequestsThroughMiddleware` (**P2.2**).
  - `beforesend_test.go`: `TestBeforeSendSeesAutomaticFields` (**P3.3**).

### C8 — cross-path proofs, the full golden, the surface, and the supplementary suites (TASK-01)

- **Files and tests:**
  - `client_rich_test.go`: **P1.1** (B1).
  - `newevent_path_test.go`: **P1.2** (B1).
  - **P1.3 (a)** lives inside P1.1 and P1.2, and so does **P2.5**.
  - `wire_golden_test.go: TestWireGoldenFull`, `testdata/wire_v2_full.golden.json` and its entry in
    `wire_v2_publish_cost.json` form the **P1.7** SDK half.
  - `tests/unit/sdk_go_wire_test.go: TestSDKGoWireGoldenMatchesIngestor` is the **P1.7** root half (B2). The two
    loops of C3 now also cover `wire_v2_full`.
  - `surface_test.go` (package `sentinel_test`): **P1.9**, one `var _` line per identifier in the §8 list at the
    end of this plan.
  - `event_refs_test.go: TestEventReferencesArePerEvent`: **P1.10**'s full table, which needs the middleware scope.
  - `tests/contract/sdk_ingestor_test.go`: `buildFullyPopulatedEvent` (`:52`) also calls `ApplyContext`, and
    `TestSDKToIngestorContract_SingleEvent` (`:257`) decodes a one-element array. This is supplementary and runs
    in workspace mode.
  - `tests/e2e/sdk_test.go`: in U8, `user_id` moves from metadata (`:159-166`) to `event_user.id`, and the test adds
    `tags.case`. This is supplementary and needs the compose stack.

### C9 — release documents (TASK-04's in-PR part)

- **Files and tests:**
  - `CHANGELOG.md`: entries for v0.3.1, v0.3.2 and v0.4.0 per spec §7.1. The v0.4.0 publish-cost sentence also
    says `"`, `\` and control characters count their UTF-8 bytes (§3).
  - `README.md`: spec §4-§6 and §8's call order.
  - `changelog_test.go`: **P4.1**.
  - `docs/sdk-specification.md`: the keys in the §3 Go example. §5 gets **one new sub-heading,
    `### sdk-go: headers and query parameters`, inserted directly after the last bullet of
    `### What the server does NOT mask`** (`docs/sdk-specification.md:354-357`, before the `---` at `:359`). No
    existing line changes.

**Paths the PR touches** (for the risk-surface match):
- `packages/sdk-go/**`
- `tests/unit/sdk_go_wire_test.go`
- `tests/contract/sdk_ingestor_test.go`
- `tests/e2e/sdk_test.go`
- `Taskfile.yml`
- `.github/workflows/ci.yml`
- `docs/sdk-specification.md`

The declared risk surface is privacy, through `request.headers`, `query_string` and `user.ip_address` (C7).

---

## 3. The open note, carried as an implementation step (`sdkgo-rc14-size-model-escape-overcount-sheds-publishable`)

**The defect.** As written, spec §3.5 publish-cost part 1 encodes the typed fields with `SetEscapeHTML(false)` and
counts that length. That encoder still escapes `"` and `\` (2 bytes each), control characters (`\n`, `\r`, `\t` at
2 bytes, the rest as `\u00XX` at 6 bytes) and U+2028/U+2029 (6 bytes where UTF-8 is 3). Protobuf writes every one
of those as its UTF-8 bytes. So an event with 64 headers of 4,096 backslashes, plus modest `metadata` and
breadcrumbs, is counted over 1,000,000 and sheds all its headers, although the ingestor would publish about 771 KB.
The count is an over-count: safe, but it sheds data NATS would carry.

**The fix, option (b), lands in C3 as part of `publishCost` (it is not a later patch).** Part 1 counts each typed
string at **its UTF-8 length plus its key and length-prefix overhead**:

```
part1 = len(escOffJSON(projection)) − Σ jsonStringExcess(s)  +  6 × (entries of Tags, Request.Headers, Request.Env)
```

- `jsonStringExcess(s) = len(escOffJSON(s)) − 2 − len(s)` is the bytes the escaping-off encoder adds to `s`
  beyond its quotes.
- It is computed with **the same encoder settings** on `s` alone, behind a fast path that returns 0 when `s` has
  no byte below `0x20`, no `"`, no `\` and no U+2028/U+2029. It is therefore exact by construction and never
  mirrors `encoding/json`'s `appendString` by hand.
- The sum runs over **every typed string the projection carries**:
  - every `string` of `Event` and of its typed parts: `sdk.*`, `user.*`, `request.method`/`url`/`query_string`,
    each frame's `file`/`function`, and each crumb's `type`/`category`/`level`/`message`;
  - **every key and value** of `Tags`, `Request.Headers` and `Request.Env`.
- Step 1 has already made every such string valid UTF-8, so there is no invalid-byte case.

**Why it stays an upper bound (the proof obligation).** After the subtraction, a string field costs its UTF-8 bytes
plus its JSON framing (`"key":"` + `",`). That framing is at least 7 bytes for the shortest key (`id`), and it covers
protobuf's tag (≤ 2 bytes) plus length varint (≤ 3 bytes below 2 MiB, ≤ 5 at any size). A map entry keeps its
6-byte allowance (spec §3.5 part 1: at most 11 bytes of protobuf against JSON's `k+v+5`). Parts 2-5 are unchanged.
So the estimate is still ≥ the protobuf of what is sent, and `TestSDKGoPublishCostBoundsProto`'s `n <= cost` still
holds with zero tolerance.

**Tests it adds** (all in C3):

1. **P1.11 `TestLoneEventBackslashHeadersSentWhole`** (B1). One event built with `NewEvent` + `Push` under the
   golden rule, carrying:
   - a `request.method`/`url`;
   - **64 headers of 4,096 `\`**;
   - `metadata` {`a`: 30,000 zeros};
   - a small number of crumbs whose `data` is number-dense ({`rows`: 2,000 zeros}), which the test sizes so that
     `publishCost` lands in (737,856, 1,000,000].

   The test asserts both sides before `Push`: `publishCost ≤ 1,000,000`, **and** `publishCost + Σ jsonStringExcess`
   over the headers (the escaped count, 262,144 more) is `> 1,000,000`, so the case discriminates. Pushed alone:
   exactly one `/batch` body, all 64 headers intact, nothing shed, `OnError` never called, `droppedCount == 0`.
2. **P1.11 `TestNewEventPathControlCharHeadersSentWhole`** (B1). Built as §8's call order (`NewEvent` →
   `ApplyContext` → `ApplyBeforeSend` → `Push`, through `newTestTransport`). It carries **64 headers of 4,096
   `\x01`**, which `net/http` would never let the middleware record, so only a `NewEvent` caller such as core can
   send them. The test asserts:
   - the one-element array is over 1,048,576 and under 5,242,880 (each `\x01` is 6 bytes marshalled);
   - `publishCost` is ≤ 1,000,000 (about 264 KB).

   Sent whole: one body, all 64 headers, `droppedCount == 0`.

   `ApplyContext` and `ApplyBeforeSend` do not exist until C4/C6. So C3 builds this case with `NewEvent` + `Push`,
   and **C6 inserts the two calls** (a context with no scope and a nil `BeforeSend`). The expected output is
   unchanged.
3. **`TestWireGoldenEscapes`** (B1) + **`testdata/wire_v2_escapes.golden.json`** and its entry in
   `wire_v2_publish_cost.json`. This is a plan addition so that the root half *gates* the fix against the real
   `proto.Marshal`. The event carries:
   - 64 headers of 300 characters mixing `\`, `"`, `\x01` and U+2028;
   - a `message` and a tag value holding each of `"`, `\`, `\n`, `\x01` and U+2028;
   - one crumb `message` and one frame `function` holding the same.

   Its escape excess is over 19,200 bytes. So the root half's existing `cost − n <= 16384` check fails if part 1
   ever counts escaped length again, and `n <= cost` fails if the subtraction ever over-reaches. It also proves
   that protovalidate accepts control characters in `request.headers` values. If it does not, that is a finding
   to escalate, not a test to weaken.

**Mutations, recorded like every §10.2 row:**

- Drop the `− Σ jsonStringExcess` term. The backslash case sheds its headers, and
  `TestSDKGoPublishCostBoundsProto` on `wire_v2_escapes` fails `cost − n <= 16384`.
- Subtract `len(escOffJSON(s)) − len(s)`, forgetting the quotes. `n <= cost` fails on the escapes golden.
- Skip the map keys in the sum. `n <= cost` still holds (an over-count), so this one is not a red mutation. Record
  it as such.

**Nothing else in the size model changes.** The 5 MiB check still uses `json.Marshal`'s real length, and parts 2-5
are exact as specified.

---

## 4. Gates

### 4.1 Machine safety (binding on the seat)

The machine froze several times on 2026-09-26. Every gate below runs with this environment. It is bash syntax; in
the fish login shell, wrap each line in `bash -c '…'` or use `set -x`.

```bash
export GOFLAGS=-p=3
export TMPDIR=$HOME/.cache/sentinel-sdkgo/tmp GOCACHE=$HOME/.cache/sentinel-sdkgo/gocache   # never /tmp (tmpfs)
mkdir -p "$TMPDIR" "$GOCACHE"
```

- **One heavy suite at a time.** Never run two of the lines marked HEAVY concurrently, and never start one while
  another agent's heavy suite is visibly running.
- **`-race` runs only on `packages/sdk-go`.** This deviates from spec §10.1 line 3, which runs
  `go test -race ./tests/unit/...`. The seat runs that line **without `-race`**, exactly as CI's `go-root` does
  (`ci.yml`, unchanged). Nothing is lost: every concurrency proof is in `packages/sdk-go` (P1.5, P1.10, P2.1, P2.2),
  and `tests/unit`'s P1.7/P1.8 root half is single-goroutine.
- **`task test-integration` is NOT run.** The spec does not require it: spec §10.1 does not list it, and nothing
  under `tests/integration` changes. The coordinator's `workflow_dispatch` runs `integration` anyway (T32).
- **`tests/e2e` U8 is not run locally**, because it needs the compose stack. The coordinator's dispatch runs the
  `e2e` job.
- The first `GOTOOLCHAIN=go1.21.13` run downloads that toolchain (`golang.org/toolchain`, about 70 MB) into the
  module cache. That is a network step and runs once.

### 4.2 Per commit (before the next commit is started)

Run in order, from the worktree root:

1. `cd packages/sdk-go && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test -race -count=1 ./...`
2. `cd packages/sdk-go && GOWORK=off GOTOOLCHAIN=go1.21.13 go vet ./...`

   This is a compile-only check of the 1.21 leg, cheap enough for every commit. It catches post-1.21 language and
   API use (T30).
3. **Only in C3 and C8, which touch `tests/unit`:** `GOWORK=off go build ./... && GOWORK=off go vet ./... &&
   GOWORK=off go test -count=1 ./tests/unit/...`. **HEAVY**: it compiles the ingestor, processor and proto.
4. **Only in C1, C3 and C7, the toolchain-sensitive commits (race plumbing, `encoding/json` sizes, build-tagged
   pattern files):** `cd packages/sdk-go && GOWORK=off GOTOOLCHAIN=go1.21.13 go test -race -count=1 ./...`. **HEAVY**.

### 4.3 Before reporting (spec §10.1), in this order, one at a time

| # | Command | Weight |
|---|---|---|
| 1 | `cd packages/sdk-go && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test -race -count=1 ./...` | medium |
| 2 | `cd packages/sdk-go && GOWORK=off GOTOOLCHAIN=go1.21.13 go test -race -count=1 ./...` | **HEAVY** |
| 3 | `GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test -count=1 ./tests/unit/...` (no `-race`, §4.1) | **HEAVY** |
| 4 | `task test-unit`: the plane's unit gate. It now includes the sdk-go `-race` line, plus the dashboard and sdk-js vitest suites (pnpm installs). | **HEAVY** (the heaviest; run last among the required) |
| 5 | `go test -tags=contract ./tests/contract/... -count=1` (workspace mode, supplementary, T18) | **HEAVY** |

- **No skip passes the gate.** The seat checks `go test -json` output for `"Action":"skip"` in `packages/sdk-go`
  and `tests/unit` and treats any skip as a failure.
- **Baseline, not "all green".** Before C1, at the base commit, the seat records the pass/fail counts of lines 1, 3
  and 4. Today's `packages/sdk-go` has 29 `Test` functions and `tests/unit` has 305. The criterion is: no test that
  passed at the base fails at the head, and every P-row test is present and passing. A pre-existing failure in the
  dashboard or sdk-js suites inside `task test-unit` is recorded against the baseline, not fixed.
- **Mutations** (spec §10.2 "Mutation", and §3 above). Each one runs once, on a committed tree, with a targeted
  `-run` and no `-race`:
  1. Apply the mutation.
  2. Run the named test(s).
  3. Capture the red line.
  4. `git checkout -- <file>`.

  Do not use `git stash`: the stash stack is shared. The mutation log (mutation → test → red output) goes in the
  delivery report.
- **The pre-merge CI proof is the coordinator's.** After the seat pushes its last commit, the coordinator runs
  `gh workflow run ci.yml --ref <branch>`, watches it, and records the evidence spec §10.1 asks for: `go-sdk (go
  1.21)` and `go-sdk (go stable)` `success` with `-race` in the step log, `go-root` `success`, and the conclusions of
  `integration`, `contract` and `e2e`.
  - The seat does not dispatch.
  - If the dispatch fails on something in this change, the coordinator re-engages the seat to fix it, and any new
    commit invalidates the proof.
  - If a leg fails for want of cgo or a C compiler, that is `BLOCKED_EXTERNAL` for the runner. It is never answered
    by removing `-race`, adding `continue-on-error` or adding a fallback step.
  - The coordinator may also dispatch on C1 alone as soon as it is pushed (§2), to retire that risk early.

---

## 5. Criterion → proof → commit

The card criteria are the contract's §5 "Proven by" column, made exact by spec §10.2.

**TASK-01: typed fields, contexts, level mapper, bounds**

| Criterion | Proof | Commit | Boundary |
|---|---|---|---|
| Every typed field populated via `Client` | P1.1 `TestClientPathPopulatesEveryTypedField` | C8 | B1 |
| Every typed field populated via `NewEvent` + `Transport.Push` | P1.2 `TestNewEventPushPathPopulatesEveryTypedField` | C8 | B1 |
| Metadata carries no tags / `user_id`; metadata-routed keys reach the wire on both paths | P1.3 (a) in P1.1/P1.2; (b) `TestWithTagRouting`; (c) `TestContextMetadataReachesWireOnBothPaths` | (a) C8; (b), (c) C4 | B1 |
| Level mapping (`warn`→`warning`, `panic`→`fatal`, `trace`→`debug`); the zerolog hook sends the typed level | P1.4 `TestNormalizeLevel`; `TestLevelFromZerolog`, `…IgnoresLevelValueGlobals`, `TestHookSendsTypedLevel` | C2; C4 | B1 |
| `server_name` precedence, automatic contexts, caller wins, field-by-field user merge | P1.5: three tests; three tests; `TestAutomaticContextsRaceBeforeSend` | C2; C4; C6 | B1 |
| Client-side bounds (spec §3.4), incl. `tenant_id` surviving the 50-tag cap | P1.6 `bounds_test.go` | C2 (context half of `TestBoundTagsKeepsTenantID`: C4) | B1 |
| The contract test against the ingestor's `ErrorPayload` (as D6's golden bridge) | P1.7 SDK half + `TestSDKGoWireGoldenMatchesIngestor`; `tests/contract` supplementary | C8 | B1 + B2 |
| The server cuts nothing; the publish-cost estimate is ≥ the published protobuf | P1.8 five goldens + `wire_v2_escapes` (§3); `TestSDKGoBoundedWireNeedsNoServerCut`, `TestSDKGoPublishCostBoundsProto` | C3 | B1 + B2 |
| Old signatures compile; §8 surface exists | P1.9 `surface_test.go` | C8 | B1 |
| §8 guarantee 1 (per-event references; the SDK never writes caller values) | P1.10 `event_refs_test.go` | C4 → C6 → C8 | B1 (`-race`) |
| No body over the ingestor's caps, no event over the publish limit, nothing shed that fits | P1.11 `bodycap_test.go` + the two rc14 cases | C3 | B1 |

**TASK-02: breadcrumbs**

| Criterion | Proof | Commit |
|---|---|---|
| Concurrent goroutines on one request are safe (`-race`) | P2.1 | C5 |
| Separate requests keep separate buffers (`-race`) | P2.2 | C7 |
| Cap enforced, oldest dropped | P2.3 | C5 |
| No-op without a buffer | P2.4 | C5 |
| Attached by `CaptureErrorContext`; readable without `Client` | P2.5 (inside P1.1/P1.2) | C8 |
| Size limits 100 / 128 KiB / 4 KiB / 8192 | P2.6 (P1.6's crumb rows, P1.8) | C2, C3 |
| Normalized on the way in | P2.7 | C5 |

**TASK-03: `BeforeSend` and the middleware**

| Criterion | Proof | Commit |
|---|---|---|
| `BeforeSend` modifies / drops | P3.1, P3.2 | C6 |
| Runs after every automatic field | P3.3 | C7 |
| A panicking hook does not crash; dropped; logged | P3.4 | C6 |
| Middleware events carry `request` without sensitive, identity-assertion or client-IP headers; never the body | P3.5 | C7 |
| `transaction` = the pattern (1.23+, `httpmuxgo121=0`) or the path | P3.6 | C7 |
| IP only on opt-in | P3.7 | C7 |
| Panic event: 500, stack, `level: fatal`, `request` | P3.8 | C7 |
| PII list matches the doc | P3.9 | C2 |
| Scope eager, ring and snapshot lazy | P3.10 | C7 |

**TASK-04: release (coordinator evidence, after merge)**

| Criterion | Proof | Who / when |
|---|---|---|
| CHANGELOG has v0.3.1, v0.3.2, v0.4.0; newest heading = `SDKVersion` | P4.1 `TestChangelogMatchesSDKVersion` | seat, C9 |
| `packages/sdk-go/v0.4.0` tagged on the merge commit; `go get …@v0.4.0` resolves with `GOPRIVATE`; `go list -m` prints it with `GOPROXY=direct GONOSUMDB=…`; a probe that calls `ApplyContext` and reads `SDKVersion` builds | P4.2 = spec §7.2's three commands | **coordinator**, after merge |
| The `-race` CI step runs green on both `go-sdk` legs before merge | spec §10.1 "Pre-merge proof" | **coordinator**, `workflow_dispatch` on the PR head |

TASK-04 is not a build item. It needs the merge commit, so it cannot deadlock the build set. The seat's part is C9's
documents and P4.1.

---

## 6. Work in flight: the sibling `sentinel-sdk-js-rich-context`

The sibling run is IMPLEMENTING on this repository, on branch
`NurfitraPujo/sentinel-sdk-js-rich-context-implementing.sentinel`, whose head was `70fa957` when this plan was
written. Its diff against `main` touches three shared files:

- **`Taskfile.yml`:** comment lines inserted above `test-unit:` (`Taskfile.yml:188-190`), and the two `git …`
  tunnel-fixture drift lines at the end of `test-unit` (`:200-201`). Ours is one line inserted **directly after
  `go test ./apps/ingestor-go/...` (`:195`)**. The changed regions are separated by unchanged lines (`:196-199`), so
  a three-way rebase applies cleanly in either order.
- **`.github/workflows/ci.yml`:** its hunk is in the `sdk-js-node` job (about `ci.yml:415`). Ours is in `go-sdk`
  (`:148-150`). No overlap.
- **`docs/sdk-specification.md` §5:** its plan adds a URL-filter note (its §8). At `70fa957` it has not landed yet.
  Ours is its own sub-heading, `### sdk-go: headers and query parameters`, inserted after the last bullet of
  `### What the server does NOT mask`. If both insert at that point, git reports an adjacent-insertion conflict.

**The rebase rule:** whichever PR merges **second** rebases onto `main`. In Taskfile and ci.yml it keeps both
changes as they are. In `docs/sdk-specification.md` it keeps both sub-headings, the first-merged one above, and
edits neither. After rebasing, it re-runs §4.3 lines 1, 3 and 4 before re-requesting merge, and the coordinator
re-dispatches CI (a new head invalidates the proof). No other file is shared. The sibling works in
`packages/sdk-js/**`, and this plan touches nothing under it.

---

## 7. Risks

| Risk | Where | Mitigation |
|---|---|---|
| The self-hosted runner lacks cgo or a C compiler for `-race` | C1 / CI | C1 lands first, and the coordinator may dispatch on it early. Failure is `BLOCKED_EXTERNAL`, never a weakened step. |
| The decoded-field bounds drift from `bounds.go` | C2 | P1.8 root half: a server marker on any golden fails. |
| The publish-cost estimate is under the real protobuf (events lost at NATS) | C3 | `TestSDKGoPublishCostBoundsProto`, zero tolerance, now also on escape-heavy strings (§3). |
| A data race between `BeforeSend` and the worker's marshal | C4-C6 | P1.5 / P1.10 race tests; `-race` on every per-commit gate. |
| `go 1.21` GODEBUG and API traps (T30) | C7, all | The per-commit 1.21 `vet` compile; P3.6's `//go:debug`; bare handlers everywhere else. |
| The eviction trap (T36) makes a count test pass by losing events | all | `newTestTransport` refuses zero sizes; every delivery assertion checks `droppedCount == 0` first. |
| The sibling PR's §5 doc edit collides | C9 | §6's rebase rule. |

---

## 8. Findings outside this scope (recorded, not fixed)

- **Stale label.** The subtest at `tests/unit/sdk_js_fixture_test.go:154` is named "the array not descended
  (packages/sdk-go/pii.go parity)". From C4, sdk-go's `ScrubPII` does descend into `[]interface{}` (spec §6.2 item 3).
  The test asserts the JS SDK's own fixture, so it stays green, but its parity label becomes false. That file is the
  sibling's and not in D6's sanction, so it is not edited here. Recorded for the sibling or a follow-up.
- **The committed copy stays v1.** `tests/fixtures/events/sdk-go-contract-event.json` is a committed copy of the
  sdk-go contract event. It feeds `tests/unit/fingerprint_golden_test.go:44` and `sdk_js_fixture_test.go:172`, and
  it stays v1-shaped. Spec §9 does not list it, and regenerating it would move the fingerprint golden. Left as is,
  deliberately.
- **U+2028/U+2029.** `encoding/json` always escapes these, even with `SetEscapeHTML(false)`, which spec §3.5 part 1's
  prose does not mention. The note's fix (§3) covers them, and `wire_v2_escapes` carries one.

---

## 9. Surface for core (spec §8, copied verbatim)

The section below is spec §8 at `fd69055`, byte for byte, so that `core-sentinel-typed-metadata` can pin it from
this plan.

## 8. Surface for core (binding on `core-sentinel-typed-metadata`)

core builds with `NewEvent(cfg, err, ctxTags, opts...)` and pushes with `Transport.Push`, and never uses `Client` (T24).
Everything below is exported by v0.4.0, works without `Client`, and is frozen for the 0.4.x line: renaming or changing
any of it fails `surface_test.go` (§10.2 P1.9), which spells each identifier with its type.

**On core's path, `request` and `transaction` come only from core's own assignments.** core serves HTTP with gin,
never wraps a handler in `HTTPMiddleware` or `NewHTTPMiddleware`, and never calls `Init` (T34). So no request scope is
ever on a core context: `ApplyContext`'s request-scope and client-IP rows (§4.3) add nothing there, and neither does
§6.5's recovered-panic path, which needs the middleware **and** an `Init`'d default client. core builds `RequestInfo`
itself, from the request data it already collects and redacts into metadata (`pkg/sentinelx/httpx/metadata.go`,
T34), and assigns `wire.Request` and `wire.Transaction` after `ApplyContext` — which is how the core contract's decision
4 ("nothing redacted today is sent after this change") is kept: core's redaction, not the SDK's, decides what
`request.headers` carries. The SDK's snapshot builder (§6.2 step 2) is **not exported**: it takes a `*http.Request`,
which core would have to rebuild from a `gin.Context` only to apply a second, different filter to data core has
already filtered. What the transport's bounds step does to `Request` (§3.4) applies to core's value as to any other.

**Functions and methods core will call**

| Identifier | Signature |
|---|---|
| `NewEvent` | `func(cfg Config, err error, ctxTags map[string]interface{}, opts ...EventOption) *Event` (unchanged) |
| `ApplyContext` | `func(ctx context.Context, ev *Event)` |
| `BreadcrumbsFromContext` | `func(ctx context.Context) []Breadcrumb` |
| `AddBreadcrumb` | `func(ctx context.Context, b Breadcrumb)` |
| `WithBreadcrumbBuffer` | `func(ctx context.Context, max int) context.Context` |
| `UserFromContext` | `func(ctx context.Context) (UserInfo, bool)` |
| `TagsFromContext` | `func(ctx context.Context) map[string]string` |
| `MetadataFromContext` | `func(ctx context.Context) map[string]interface{}` |
| `WithContextMap` | `func(ctx context.Context, additionalTags map[string]interface{}) context.Context` (unchanged) |
| `WithUser` | `func(ctx context.Context, userID string) context.Context` (unchanged) |
| `WithUserInfo` | `func(ctx context.Context, u UserInfo) context.Context` |
| `WithTenant` | `func(ctx context.Context, tenantID string) context.Context` (unchanged) |
| `WithTag` | `func(ctx context.Context, key string, value interface{}) context.Context` (unchanged) |
| `WithLevel` | `func(ctx context.Context, level string) context.Context` |
| `NormalizeLevel` | `func(level string) string` |
| `sentinelzerolog.LevelFromZerolog` (package `github.com/tuju-ai/sentinel/packages/sdk-go/sentinelzerolog`) | `func(l zerolog.Level) string` — core's level mapper (see below) |
| `(*Config).ApplyBeforeSend` | `func(ev *Event) *Event` |
| `(*Config).Validate` | `func() error` (unchanged) |
| `NewTransport` | `func(cfg Config) *Transport` (unchanged) |
| `(*Transport).Push` | `func(event *Event)` (unchanged; now bounds and marshals per event) |
| `(*Transport).Flush` | `func(timeout time.Duration) bool` (unchanged) |
| `ExtractTraceContext` | `func(ctx context.Context) (traceID, spanID string, traceFlags uint32)` (unchanged) |

**Fields and constants core will set or read**

- `Event.Level string`, `Event.SDK *SDKInfo`, `Event.ServerName string`, `Event.Transaction string`,
  `Event.User *UserInfo`, `Event.Tags map[string]string`, `Event.Contexts map[string]map[string]interface{}`,
  `Event.Request *RequestInfo`, `Event.Breadcrumbs []Breadcrumb`, plus every existing `Event` field.
- `SDKInfo{Name, Version}`, `UserInfo{ID, Username, Email, IPAddress}`,
  `RequestInfo{Method, URL, QueryString, Headers, Data, Env}`,
  `Breadcrumb{Timestamp, Type, Category, Level, Message, Data}`.
- `Config.ServerName string`, `Config.BeforeSend func(*Event) *Event`, `Config.MaxBreadcrumbs int` (now read by the
  transport; core's 50 stays valid).
- `LevelFatal`, `LevelError`, `LevelWarning`, `LevelInfo`, `LevelDebug`, `SDKName`, `SDKVersion`,
  `DefaultMaxBreadcrumbs`, `MaxBreadcrumbsLimit`.

**Guarantees core may rely on**

1. `NewEvent` returns non-nil, per-event `Tags` and `Contexts` maps, `Level == "error"`, `SDK` set, `ServerName`
   resolved per §3.2.

   **The rule, for `ApplyContext` and the transport alike: the SDK never writes into a map, slice or pointer it did not
   allocate for this event.** `ApplyContext` assigns fields of the `*Event` it is given, and every map or struct it
   adds to is a new one it built for that event (§4.3, "It assigns; it never writes through"); the transport does not
   even assign — it bounds, trims and marshals a shallow copy, copying every pointer and rebuilding every map or slice
   it changes (§3.5). So a value **the caller** puts on an event — an option's or `BeforeSend`'s `Tags` map or
   `*UserInfo` shared by many events, over-bound or not, core's own `RequestInfo`, a `Request.Data` — is never written
   by the SDK, and whether to share it is the caller's choice alone. P1.10 proves it with a shared `Tags` map and a
   shared over-bound `*UserInfo` under `-race`.

   **Every map, slice and pointer an event can hold, and whether it is the event's own** — the
   complete list, on both paths, for events built by `NewEvent` + `ApplyContext` (and so by `Client`). "Per event"
   means no context, `Config`, cache or other event holds it, so writing it in place (a caller's assignment, an
   `EventOption`, `BeforeSend`) affects that event only and cannot race another event's marshal. "Shared" means it
   may be held by the context and by other events of the same context; it must be **replaced** (assign a new value to
   its key or field), never modified in place.

   | Reference on `Event` | Per event? | Why |
   |---|---|---|
   | `Tags` | **yes** | `NewEvent` allocates it; `ApplyContext`, when it adds a context entry, replaces it with a new map (a copy plus the entries, §4.3); keys and values are strings. `TagsFromContext` returns a copy |
   | `Contexts` and its `runtime`, `os`, `device` inner maps | **yes** | allocated by `NewEvent` for each event; only the scalar values are cached (§3.2). `ApplyContext` does not touch `Contexts` |
   | `Metadata` (top level) | **yes** | `NewEvent` builds it with `ScrubPII(ctxTags)`, a new map (T37), or, for nil `ctxTags`, a new empty map — never nil, so it can be written in place (§3.2); `ApplyContext`, when it adds a context key, replaces it with a new map (a copy plus the scrubbed keys, §4.3) |
   | `Metadata` values that are `map[string]interface{}` or `[]interface{}`, at any depth — a map inside a list included | **yes** | `ScrubPII` rebuilds every such map and list, through both (T37, §6.2 item 3). The one exception is a cycle: a map or list met again on its own path is kept by reference (shared), and §3.5 step 2 removes the entry holding it from what is sent |
   | `Metadata` values of any other reference kind (a slice of any other element type — `[]string`, `[]map[string]interface{}` — a `map[string]string`, a pointer, a struct holding one) | **shared** | `ScrubPII` copies them by reference (T37): they are the objects in the `ctxTags` the caller passed and in the context's metadata map, and so in every event captured from that context (T37: on the `Client` path `ctxTags` is the context's map) |
   | `User` (`*UserInfo`) | **yes** | `ApplyContext` installs a new `*UserInfo` whenever it adds a field (user row, client-IP row), never writing an existing one in place (§4.3); `UserInfo` holds strings only. `NewEvent` sets none |
   | `SDK` (`*SDKInfo`) | **yes** | allocated by `NewEvent`; strings only |
   | `Request` (`*RequestInfo`) and `Request.Headers` | **yes** | `ApplyContext` deep-copies the request scope's snapshot for each event (§4.3, §6.2); the bounds step builds new `Headers` / `Env` maps (§3.5) |
   | `Request.Data`, `Request.Env` | never set by the SDK | the middleware sets neither (§6.2) |
   | `Breadcrumbs` (slice) | **yes** | `BreadcrumbsFromContext` builds a new slice per call (§5.2) |
   | `Breadcrumbs[i].Data` (top level) | **yes** | `BreadcrumbsFromContext` gives each returned crumb its own copy (§5.1, §5.2) |
   | values nested inside `Breadcrumbs[i].Data`, maps and lists at any depth — a map inside a list included | **shared** | `AddBreadcrumb` and `BreadcrumbsFromContext` copy the top level only: nested maps and slices are the buffer's, and so every event's of that request (§5.2) |
   | `Stacktrace` | **yes** | `ExtractStacktrace` builds a new slice per call; `Frame` holds scalars only |

   **A map under a list, field by field** (round 12): in `Metadata` it is per event (the row above); `Contexts` holds
   no list the SDK put there — `NewEvent`'s `runtime`, `os` and `device` hold scalars, and any other context is one the
   caller assigned, the caller's by the rule; `Request.Data` is never set by the SDK, so whatever it holds is the
   caller's; below a crumb's top-level `Data`, a map inside a list is the buffer's and shared, exactly as a map inside
   a map is (§5.2) — replace it, never edit it. Whatever the caller owns, the transport only reads: steps 2-3 send
   new values built from the decode (§3.5).

   Nothing else: `Config` contributes strings only, and the SDK caches only scalars (the hostname, the runtime version,
   `GOOS`, `GOARCH`) and the request scope's snapshot, which is deep-copied per event. A value **the caller assigns**
   (a map it put on `ev.Tags` or into `ev.Contexts`, an option's `*UserInfo`, core's own `RequestInfo`, a
   `Request.Data`) is the caller's to share or not; by the rule above, neither `ApplyContext` nor the transport ever
   writes into it. P1.10 proves every row, and the rule.
2. `ApplyContext` only adds what the context carries and never clears a field; the event's existing value wins except
   for `Level`, and for `Fingerprint` / `ErrorClass` set with `WithFingerprint` / `WithErrorClass` (§4.3's table). A
   context **metadata** `error_class` or `fingerprint` replaces only `NewEvent`'s own value, never one an
   `EventOption` set (`WithEventErrorClass` / `WithEventFingerprint` exactly; a custom option that changed the value,
   §3.2). It carries the context's metadata (scrubbed) onto
   `Metadata` for keys the event lacks, so `request_id`, `WithContextMap` keys and the other metadata-routed keys reach
   the wire on this path exactly as on the `Client` path.
3. The transport bounds every pushed event per §3.4 — the pre-v2 fields included, so no value core assigns can make
   the ingestor answer 400 (§3.4's rule table), both timestamps, so none can fail the marshal, and `metadata` and
   `contexts`, 4,096 bytes under the server's 64 KiB with U+0000 and key collisions resolved as the server would, so
   the server's bounds step cuts nothing and writes no marker (what the transport removes from those two it records
   in `metadata.truncated` / `truncated_keys`) — posts every body to `{endpoint}/batch`, a lone event as a
   one-element array, and fits every event under the ingestor's two per-event limits: its one-element array under the
   5 MiB batch body cap, and its **publish cost** under 1,000,000 bytes, below NATS's 1,048,576-byte `max_payload` by
   the fixed allowance §3.5 sizes. The publish cost is the protobuf size of exactly what is sent, estimated from
   above: the typed fields as JSON with HTML escaping off, the four `Struct`-typed fields (`metadata`, `contexts`,
   `request.data`, breadcrumb `data`) at their protobuf size — a number costs 11 bytes there (§3.5, T40). It fits
   them by shedding `request.headers`/`env`, then `request.data`, then `breadcrumbs`, so an event within both limits
   is never shed, and cuts every batch under 5 MiB and 500 items, so none is answered 413 and none is accepted and
   then lost at the publish for its size (§3.5, T38, T39, T40). Every event is decoded as the ingestor's
   `json.Decode` will decode it before it is sent, so no body is answered 400 because one event holds a number beyond
   `float64` or nesting over 10,000 levels (§3.5 step 2, T41). A value that cannot be marshalled, or that the
   ingestor cannot decode, costs only its own entry: `request.data` or a crumb's `data` is set to nil, a `metadata`
   key or a context is removed and recorded, and the event is sent (§3.5 step 2). It drops an event only when it is
   still over either limit after those three steps — possible only through frame `file`/`function` text (without it
   an event, once shed, is at most ~860 KB of estimate and far under 5 MiB of body) — alone, with `OnError`.
4. `ApplyBeforeSend` never panics and returns nil to mean "drop".

**The call order core's `Push` uses** (illustrative; core owns its code):

```go
wire := sentinel.NewEvent(s.cfg, nil, ev.Metadata) // level, sdk, server_name, runtime/os/device contexts
sentinel.ApplyContext(ctx, wire)                    // metadata, level, user, tags, breadcrumbs, trace, fingerprint
                                                    // (request, transaction: never on core's path — no middleware scope)
// … core's own assignments: Message, Stacktrace, Timestamp, trace ids,
//   if class != "" { wire.ErrorClass = class }  // only a non-empty core value: it then wins over the context's
//   if fp != ""    { wire.Fingerprint = fp }    //   (ApplyContext ran first); an empty one would blank it
//   wire.Request = &sentinel.RequestInfo{…} (from core's own redacted httpx data), wire.Transaction,
//   wire.Level = sentinelzerolog.LevelFromZerolog(zl) (zl is the zerolog.Level value, never zl.String()),
//   wire.User, wire.Tags[k] = v, wire.Contexts[name] = m …
if wire = s.cfg.ApplyBeforeSend(wire); wire == nil {
	return
}
s.transport.Push(wire)
```

**core assigns `ErrorClass` and `Fingerprint` only when it has a non-empty value, and a non-empty core value wins over
the context.** `ApplyContext` has already put the context's `WithErrorClass` / `WithFingerprint` value (or a
metadata `error_class` / `fingerprint`, §4.3) on `wire`; core's assignment comes after it, so a non-empty core value
replaces it — core's classification is the more specific one. An unconditional assignment of an empty value would
instead blank what the context set: a blank `Fingerprint` silently falls back to the server's grouping, and a blank
`ErrorClass` reaches the wire as `"Error"` (§3.4). Today's `sink.go` assigns both unconditionally (T24), which is
harmless only while nothing on core's contexts sets either.

**core maps its level from the `zerolog.Level` enum, not from `Level.String()`.** `String()` returns the mutable
globals `zerolog.LevelTraceValue` … `LevelPanicValue` (T31), so a mapper fed it gives a different answer the day any
code in the process renames a level; the enum does not move. The enum mapper is `LevelFromZerolog(zerolog.Level)` in
**`sentinelzerolog`**, which core imports for it (importing the package installs no hook: the hook is a value core would
have to attach). It stays there rather than in the root `sentinel` package because the root package imports no zerolog
today (T31) and every `sentinel` importer would otherwise compile zerolog in; sdk-go's module already requires zerolog
and core already depends on it (v1.35.1, T24; MVS selects core's version and the `Level` constants are unchanged), so
nothing is added to either side's dependency graph. `NormalizeLevel(string)` remains for callers whose level is a
string to begin with (a config value, another logger).
