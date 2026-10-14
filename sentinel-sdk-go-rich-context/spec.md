# Sentinel sdk-go rich context (typed fields, contexts, breadcrumbs, BeforeSend, request): implementation specification

**Feature:** `sentinel-sdk-go-rich-context` · **Repository:** `sentinel` (`tuju-ai/sentinel`), side `be`, module
`packages/sdk-go` · **Base:** `main` @ `dde0ea2` (v0.2.39)
**Built against:** `prd/sentinel-sdk-go-rich-context/contract.md` and `be.md` @ **`e030211`** (tuju-ai/prd#798). If this
document and the contract disagree, the contract wins, except where §1.3 records that the contract's description of the
code or of the repository's gates is wrong. Each such row says what this spec does instead and why.
**Revision:** repair round 13 (2026-09-26): two minors and one note, aligned with the sibling sdk-js spec — over 50
tags the SDK keeps `tenant_id` when present plus the lexically smallest others up to 50 (§3.4, §4.2, §7.1, P1.6);
`Taskfile.yml`'s sdk-go line is pinned directly after `go test ./apps/ingestor-go/...` and the
`docs/sdk-specification.md` §5 note to a new sub-heading after *What the server does NOT mask* (§9, D5); and
`query_string` splits on `&` and `;` and matches a name that does not unescape on its raw form (§6.2, P3.5); the
size model is unchanged. Repair round 12 (2026-09-26): spec review round 12's two minors are fixed — every invalid UTF-8 byte of
a typed string, tag, header or env entry becomes one U+FFFD (one per byte, as `json.Marshal` writes it) before any
bound runs (§3.5 step 1), so every byte the SDK measures is the server's; and `ScrubPII` becomes a deep copy through
maps and `[]interface{}`, cycle-safe (§6.2 item 3), so §8 guarantee 1's metadata row holds for a map inside a list;
the size model is unchanged. Repair round 11 (2026-09-26): spec review round 11's one minor is fixed in §10.2 alone — every golden
and every P1.11 case now assigns its volatile values (`event_id`, `timestamp`, crumb timestamps, `stacktrace`, the
`runtime`/`os`/`device` contexts) before `Push` and replaces nothing afterwards (**the golden rule**), so the committed
publish-cost integers hold on both `go-sdk` legs and any machine, and `TestBoundDataNul` counts U+0000 as the 6-byte
`\u0000`; the size model is unchanged. Repair round 10 (2026-09-26): the two minors of spec review round 10 are fixed, the first by the
coordinator's binding direction — the size model is now **"apply with headroom, then measure what is sent"**: the SDK
bounds `metadata` and `contexts` itself, 4,096 bytes under the server's 64 KiB, strips U+0000 and resolves key
collisions where the server would, so the server's bounds step cuts nothing, and the publish cost is the protobuf
size of exactly what is sent; the replay of the server's cut, and every marker-prediction rule, is gone (§3.4,
§3.5). On top of round 9's three minors, round 8's one and the `Struct` inflation the parallel sdk-js review proved,
round 1's eleven, round 2's eight, round 3's six, round 4's five, round 5's four, round 6's three and round 7's one;
§13 maps each finding of every round to the sections it changed.
**Work items:** `sentinel-sdk-go-rich-context/TASK-01` (typed fields, automatic contexts, level mapper, client-side
bounds), `TASK-02` (breadcrumbs on `context.Context`), `TASK-03` (`BeforeSend`, `HTTPMiddleware` fills `request`),
`TASK-04` (release `packages/sdk-go/v0.4.0`).
§10 maps every acceptance criterion to the command that proves it and to the mutation that must turn that proof red.
**§8 "Surface for core" is binding on `core-sentinel-typed-metadata`.**
**Outcome of the open questions:** no `NEEDS_DECISION`. Every "the spec fixes" of the contract is fixed in §3-§7 and
listed in §11.1 with what it was derived from. Three rows of §1.3 (D5, D6, D7) touch files outside `be.md`'s listed
scope (`Taskfile.yml`, `.github/workflows/ci.yml`, `tests/unit/`) or replace a check the contract names; the
coordinator ruled on all three (D5 **sanctioned and tightened**, D6 **sanctioned**, D7 **sanctioned**), and §1.3 records
each ruling with what it binds.

Every "today" claim cites a path and a symbol at `dde0ea2` (line numbers are approximate, `~L`, and never the only
identifier). Commands marked **run** were executed by this seat on 2026-09-26 in a clean worktree of `dde0ea2`.

**Risk surfaces:** privacy — `request.headers` and `request.query_string` recorded by `HTTPMiddleware` (§6.2), and
`user.ip_address` on opt-in (§6.4). No tenancy, credential, migration or payment surface: the change is inside a client
library and adds no endpoint, table or secret. Unchanged from the contract's declaration.
**No measurement owed:** no read or write path over stored data changes, and no performance outcome is claimed. **No
infra/alerting changes owed:** a library release; nothing is deployed, no service, queue, bucket or job is added.
The NATS `max_payload` the transport now sizes each event to (T39, T40) is read, not changed; raising it, or checking
the size before the publish, is an ingestor/infra change recorded as §12 F10.

---

## 1. The problem

### 1.1 Who has it

Every Go producer reaches Sentinel as a v1 event. Whoever triages a Go error in the dashboard sees empty typed panels
(user, request, contexts, breadcrumbs) and no tag facets for it, although the server has stored and rendered those
fields since `sentinel-event-schema-v2` (v0.2.36) and `sentinel-occurrence-detail-panels`. The largest producer, core,
builds events with `sentinel.NewEvent` and pushes them with `Transport.Push` and never uses `Client`, so a feature that
lives only in `Client.CaptureErrorContext` never reaches it (contract §1).

The measure the contract sets: unit tests show every typed field populated through both paths (`Client`, and `NewEvent`
+ `Transport.Push`), metadata no longer carries tags or `user_id`, and the tag resolves with `go get`.

### 1.2 What is true today (verified)

| # | Fact | Evidence |
|---|---|---|
| T1 | `Event` has exactly `event_id`, `project_key`, `platform`, `error_class`, `message`, `stacktrace`, `timestamp`, `environment`, `release_version` (omitempty), `trace_id` (omitempty), `span_id` (omitempty), `trace_flags` (not omitempty), `fingerprint` (omitempty), `metadata` (omitempty). No v2 field exists. | `packages/sdk-go/event.go: type Event` (~L32-97) |
| T2 | `NewEvent(cfg Config, err error, ctxTags map[string]interface{}, opts ...EventOption) *Event` sets id, project key, `platform: "go"`, class, message, fingerprint (from `ctxTags["fingerprint"]`), class override (from `ctxTags["error_class"]`), `ExtractStacktrace(3)`, `time.Now().UTC()`, environment, release, and `Metadata: ScrubPII(ctxTags)`; then applies `opts`. No host, runtime or level. | `event.go: NewEvent` (~L298-337) |
| T3 | `WithUser(ctx, userID string)` and `WithTenant(ctx, tenantID string)` are `WithTag(ctx, "user_id"/"tenant_id", …)`. `WithTag(ctx, key string, value interface{})` and `WithContextMap(ctx, map)` copy-on-write one `map[string]interface{}` under the context key `sentinel_tags`; that map is what becomes `metadata`. | `context.go: WithUser, WithTenant, WithTag, WithContextMap, getTagsMap` (~L51-89) |
| T4 | `Config.MaxBreadcrumbs` is defaulted to 50 in `withDefaults` and read nowhere else. There is no breadcrumb type, no `BeforeSend`, no `ServerName`, no hostname call. | `config.go: withDefaults` (~L92-93); `grep -rn 'MaxBreadcrumbs\|Hostname\|BeforeSend' packages/sdk-go` (**run**: only `config.go`) |
| T5 | `Client.CaptureErrorContext`: returns on nil err; samples; merges `getTagsMap(ctx)` with `additionalContext[0]`; `NewEvent(c.cfg, err, ctxTags)`; sets trace id/span id/flags from `ExtractTraceContext(ctx)` unconditionally; overrides fingerprint and error class from `ExtractFingerprint(ctx)` / `ExtractErrorClass(ctx)` when non-empty; `c.transport.Push(event)`. | `client.go: (*Client).CaptureErrorContext` (~L27-66) |
| T6 | `HTTPMiddleware(next http.Handler) http.Handler` puts `http_method`, `http_url` (`r.URL.String()`, query included), `http_user_agent`, `http_remote_addr` into the metadata map; on panic it calls the **package-level** `CaptureErrorContext` (the default client) with metadata `panic: true`, `panic_stacktrace: debug.Stack()`, and answers 500. It has no options. | `middleware.go: HTTPMiddleware` (~L9-37) |
| T7 | `sentinelzerolog.Hook.Run` fires for `level >= zerolog.ErrorLevel`, which in zerolog v1.31 is Error(3), Fatal(4), Panic(5), **NoLevel(6) and Disabled(7)**; it calls the package-level `CaptureError` (no context) with metadata `zerolog_level: level.String()`. Warn(2) is never sent. | `sentinelzerolog/hook.go: Run` (~L16-22); `zerolog@v1.31.0/log.go` level constants (~L130-150) |
| T8 | zerolog v1.31.0 (the version `go.mod` requires) has `(*zerolog.Event).GetCtx() context.Context`, which returns `context.Background()` when no context was attached. | `zerolog@v1.31.0/event.go: GetCtx` (~L437) |
| T9 | The ingestor accepts `level` ∈ {`fatal`,`error`,`warning`,`info`,`debug`} and blanks anything else (marking `server_truncated`); it drops tag entries whose key fails `^[a-zA-Z0-9_.:-]{1,32}$`, whose value is empty, over 200 code points or contains `\n`, keeps the 50 lexically smallest keys; drops `user.id` over 255 **bytes** and `email`/`ip_address` over 254/45 code points; truncates every other v2 string; keeps the last 100 breadcrumbs, truncates type/category/message to 64/128/8192, blanks an invalid crumb level, nils a crumb `data` over 4096 bytes, then drops oldest while the list is over 128 KiB. None of that rejects the event. | `apps/ingestor-go/validation/bounds.go: consts` (~L47-91), `validLevels`, `tagKeyPattern`, `applyBounds`, `boundUser`, `boundTags`, `boundRequest`, `boundStringMap`, `boundBreadcrumbs` |
| T10 | What does reject an event: a JSON value of the wrong **type** for a decoded field (the whole body fails `json.Decode` → 400), a body over 1 MiB (single) or 5 MiB (batch) (`http.MaxBytesReader` → 413), or a protovalidate rule on a pre-v2 field. The v2 types: `level`, `server_name`, `transaction` strings; `sdk {name, version}`; `user {id, username, email, ip_address, geo}`; `tags map[string]string`; `contexts map[string]map[string]interface{}`; `request {method, url, query_string, headers map[string]string, data any, env map[string]string}`; `breadcrumbs [{timestamp RFC 3339, type, category, level, message, data map}]`. | `apps/ingestor-go/validation/validator.go: ErrorPayload, SDKInfo, UserInfo, RequestInfo, Breadcrumb` (~L25-147); `apps/ingestor-go/main.go: maxEventBodyBytes, maxBatchBodyBytes` (~L39-52) |
| T11 | The processor takes the affected user from `user.id` when non-empty, else from metadata `user_id`/`userId`/`user`; the affected tenant from `tags["tenant_id"]` when present, else from metadata `tenant_id`/`tenantId`/`organization_id`. `error_search_index.user_id`/`tenant_id` are overwritten with those two values. `request_id` (`requestId`, `request-id`), `trace_id` (`traceId`, `trace-id`) and `span_id` (`spanId`, `span-id`) are read **only** from metadata (trace and span fall back to the typed fields). | `apps/processor-go/event/event.go: readAffectedIDs` (~L253-271); `apps/processor-go/indexer/indexer.go: UserIDFromMetadata, TenantIDFromMetadata, ExtractSearchFields` (~L60-121); `apps/processor-go/service/processor_service.go` (`searchEntry.UserID = evt.AffectedUserID`, ~L516-532) |
| T12 | For a server platform such as `go`, the ingestor never fills `user.ip_address` from the ingest connection (that would be the Go server's own address), and it parses `request.headers["User-Agent"]` into `contexts.browser` and `contexts.client_os` — **not** `contexts.os` — when those are absent. A context the body carries always wins. | `apps/ingestor-go/enrich/enrich.go: applyIP` (rows I4/I6), `applyUA` (`osKey := "client_os"` unless `IsClientPlatform`) |
| T13 | The server masks `tags` values by key, `request.url`/`query_string`/`headers`/`env`/`data`, breadcrumb `message`/`data` and `contexts`; it stores `user` (email and IP included) as sent. | `apps/processor-go/event/event.go: maskV2`; `docs/sdk-specification.md` §5 "What the server masks" |
| T14 | `docs/sdk-specification.md` §5 requires SDKs to scrub metadata keys that **contain** any of `access_token api-key api_key apikey auth authorization bearer card_number client_secret cookie credential credentials credit_card cvv passwd password private_key pwd refresh_token secret social_security ssn token`, and says `Set-Cookie` and `X-API-Key` are covered. The SDK's `defaultPIIKeys` is `password passwd pass token bearer secret authorization auth credit_card card_number cvv ssn social_security api_key apikey`: it lacks `api-key`, `cookie`, `credential`, `private_key` and `pwd`, so `Set-Cookie`, `X-Api-Key`, `db_credential` and `pwd` are **not** scrubbed by the SDK today. | `docs/sdk-specification.md` §5 "Mandatory Masking Keys"; `packages/sdk-go/pii.go: defaultPIIKeys, isSensitiveKey` |
| T15 | `net/http.Request.Pattern` exists from **Go 1.23**; Go 1.22 added pattern routing and `Request.PathValue` but not `Pattern`. `ServeMux.ServeHTTP` writes `r.Pattern` **in place** on the request it was given before calling the handler. `Request.WithContext` returns a shallow copy. | `$GOROOT/api/go1.23.txt` (`pkg net/http, type Request struct, Pattern string #66405`); `$GOROOT/api/go1.22.txt` (`PathValue`); `net/http/server.go: (*ServeMux).ServeHTTP` (`h, r.Pattern, r.pat, r.matches = mux.findHandler(r)`); `net/http/request.go: WithContext` (go1.26.4, **run**) |
| T16 | `go.mod` says `go 1.21` and requires only `github.com/rs/zerolog v1.31.0` and `go.opentelemetry.io/otel/trace v1.21.0`. | `packages/sdk-go/go.mod` |
| T17 | CI's `go-sdk` job runs `go build`, `go vet` and, **only when the event is not `pull_request`**, `go test ./... -count=1` — **without `-race`** — in `packages/sdk-go` with `GOWORK: "off"` on Go `1.21` and `stable`. `task test-unit` does **not** run `packages/sdk-go` at all. The `go-root` job runs `go test ./tests/unit/... -count=1` under `GOWORK=off`, also only off `pull_request`. Operator policy since 2026-09-06: a PR proves the tree builds; unit suites run on push to `main`. | `.github/workflows/ci.yml: jobs.go-sdk` (~L117-150), `jobs.go-root` (~L78-106), header comment "PR scope (2026-09-06, operator)"; `Taskfile.yml: test-unit` (~L191-201) |
| T18 | `tests/contract/sdk_ingestor_test.go` (`//go:build contract`) marshals a real `sentinel.NewEvent` and decodes it into the real `validation.ErrorPayload` with `DisallowUnknownFields`, maps it with `mapping.MapPayloadToEvent` and validates with protovalidate. It imports `packages/sdk-go`, so it runs **only in workspace mode** (it cannot build under `GOWORK=off`), and CI's `contract` job runs only on `workflow_dispatch`. `go test -tags=contract ./tests/contract/... -count=1` is green today (**run**). | `tests/contract/sdk_ingestor_test.go` package comment; `.github/workflows/ci.yml: jobs.contract` (`if: github.event_name == 'workflow_dispatch'`) |
| T19 | `tests/e2e/sdk_test.go` (`//go:build e2e`) `TestU8_RealSDKEndToEndPipeline` sends through the real SDK with `sentinel.WithUser(ctx, "u8-user")` and asserts the stored occurrence's **metadata** has `user_id`. It needs the compose stack; CI's `e2e` job is `workflow_dispatch` only. | `tests/e2e/sdk_test.go: TestU8_RealSDKEndToEndPipeline` (~L113-170) |
| T20 | Unit tests that assert today's metadata routing: `context_test.go: TestContextHelpers` (`user_id`, `tenant_id` in the tags map), `event_test.go: TestNewEventFieldsMatchWireContract` (`Metadata["user_id"]` from `ctxTags`), `pii_test.go: TestScrubPII`. | the named tests |
| T21 | `GOWORK=off go test -race ./... -count=1` in `packages/sdk-go` is green today on go1.26.4 and on go1.21.13 (`GOTOOLCHAIN=go1.21.13`) (**run**). `-race` requires cgo, and it fails, never degrades, without it, on both toolchains: `CGO_ENABLED=0 go test -race` exits non-zero with `go: -race requires cgo; enable cgo by setting CGO_ENABLED=1`, and with no C compiler (`CC=/nonexistent/gcc`, `CGO_ENABLED` unset or `1`) it exits non-zero with `[build failed]` (**run**, go1.21.13 and go1.26.4). | this seat, 2026-09-26 |
| T22 | Tags: `packages/sdk-go/v0.3.0` → `925530d` (merge of #26), `v0.3.1` → `ac7d18c` (error-class unwrapping, `WithFingerprint`/`WithErrorClass`/`Extract*`), `v0.3.2` → `78218f3` (merge of #38, whose change is `4c8763c`: `ClassifiedError`, `Classify`). `git log packages/sdk-go/v0.3.2..origin/main -- packages/sdk-go` is empty. `CHANGELOG.md`'s newest entry is `v0.3.0`. | `git rev-list -n1 packages/sdk-go/v0.3.{0,1,2}` (**run**); `packages/sdk-go/CHANGELOG.md` |
| T23 | `tuju-ai/sentinel` is a **private** repository. Consumers fetch the module with `GOPRIVATE=github.com/tuju-ai/*` (the v0.3.0 CHANGELOG entry says so); `proxy.golang.org` and `sum.golang.org` cannot see it. | `gh repo view tuju-ai/sentinel --json visibility` → `PRIVATE` (**run**); `CHANGELOG.md` v0.3.0 |
| T24 | core `origin/production` (`73be60827`) requires sdk-go **v0.3.1**; core `main` (`dc653f34a`) requires v0.3.2 (`git show <sha>:go.mod`, **run**). The core release that ships v0.4.0 to production therefore crosses the v0.3.1, v0.3.2 and v0.4.0 CHANGELOG entries, none of which exists today (T22). core `main`: `pkg/sentinelx/sdk/sink.go` imports sdk-go, builds `sentinel.Config` by hand (`MaxBreadcrumbs: 50`, `BatchWait: time.Second`, …) because `NewTransport` does not apply `withDefaults`, and in `(*Sink).Push(ctx, ev)` calls `sentinel.NewEvent(s.cfg, nil, ev.Metadata)`, overwrites `ErrorClass`, `Message`, `Stacktrace`, `Fingerprint`, `Timestamp`, trace ids (from `ev` or `sentinel.ExtractTraceContext(ctx)`), then `s.transport.Push(wire)`. core's `go.mod` is `go 1.26.3` and requires zerolog v1.35.1. | core `pkg/sentinelx/sdk/sink.go: Open, Push` (~L56-114); core `go.mod` at `origin/production` and `origin/main` |
| T25 | `ExtractStacktrace` drops frames whose function name contains `"runtime."` or `"sentinel."`. Since the v0.3.0 module rename the SDK's own functions are named `github.com/tuju-ai/sentinel/packages/sdk-go.…`, which contains neither, so SDK frames are no longer filtered (they are marked `InApp: false` when compiled from the module cache, so grouping is unaffected). | `event.go: ExtractStacktrace` (~L99-123) |
| T26 | The dashboard labels a context key `arch` only under `device` ("Architecture"), and renders `runtime` as `name version`. | `apps/dashboard-web/src/lib/components/issues/occurrence-display.ts` (context label maps, ~L278-336) |
| T27 | Metadata keys read by name, outside T11: `NewEvent` reads `ctxTags["error_class"]` and `ctxTags["fingerprint"]` (non-empty strings) into `ErrorClass` / `Fingerprint` (T2); the processor falls back to metadata `release_version` when the typed `release_version` is empty, reading the raw map before masking; the ingestor reads metadata `runtime` to classify a `javascript` event (never consulted for `go`), and owns `server_truncated` / `server_truncated_keys` (it deletes or rewrites a client value under either); and the ingestor's 64 KiB metadata shrink removes the keys of `metadataExemptKeys` only after every other key — T11's routing keys, `release_version`, and the JS SDK's markers `truncated` / `truncated_keys` (the last two are read by name nowhere else; the dashboard does not read either, `grep -rn truncated_keys apps/dashboard-web/src`, **run**: no hit). Nothing else in the SDK, ingestor or processor reads a metadata key by name. The processor copies the metadata `release_version` fallback **unbounded** (no length check) into `ReleaseVersion`, which is stored in `error_occurrences.release_version VARCHAR(100)` (§12 F8). | `packages/sdk-go/event.go: NewEvent`; `apps/processor-go/event/event.go: (*ErrorEvent).Prepare` (~L229-233); `apps/ingestor-go/enrich/enrich.go: IsClientPlatform, isTunnelOnly`; `apps/ingestor-go/validation/bounds.go: writeMarkers, metadataExemptKeys, shrinkMetadata`; `packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql` (`error_occurrences.release_version`); `grep -rn -E 'Metadata\[' apps/processor-go apps/ingestor-go` excluding tests (**run**) |
| T28 | The ingestor's `Breadcrumb` has **no** `omitempty`: re-marshalled, every crumb carries `"type"`, `"category"`, `"level"`, `"message"` (empty strings when absent) and `"data":null` when it has none. `boundBreadcrumbs` measures the 128 KiB list bound on that shape: `jsonSize` of each crumb plus separators (`arraySize`). A crumb of an 8,100-character ASCII message with only a whole-second UTC timestamp is 8,196 bytes on that shape and 8,149 on an all-`omitempty` shape, so 200 such crumbs keep 15 on the server's shape and 16 on the other (**run**, scratch program mirroring both structs). An **empty** `data` map is the same as none on the wire: the SDK's `omitempty` drops `{}`, the ingestor decodes the absent key as a nil map and re-marshals it as `null` (4 bytes, where `{}` would be 2). The list is cut while it is **over** 128 KiB (`arraySize(…) > MaxBreadcrumbsSize`), so exactly 131,072 bytes is kept. | `apps/ingestor-go/validation/validator.go: Breadcrumb`; `apps/ingestor-go/validation/bounds.go: boundBreadcrumbs, arraySize, jsonSize` (~L522-562) |
| T29 | Since `sentinel-issue-tag-facets` (v0.2.39), every `tags` entry of an occurrence is rolled up per issue into `issue_tag_values` (one row per `(issue_id, key, value)`, with a count and first/last seen), which feeds the tag panel and search by tag. A tag with a per-request value (a request id, an order number) creates one rollup row per request. | `apps/processor-go/store/store.go: StoreEvent` (tag rollup comment, ~L734-745); `packages/db-migrations/migrations/1725400000_add_issue_tag_values.sql` |
| T30 | **GODEBUG defaults come from the main module's `go` line, not from the toolchain.** A test binary's main module is the module under test, so every `packages/sdk-go` test binary (`go 1.21`, T16) runs with `httpmuxgo121=1` on every toolchain: `ServeMux` then uses the Go 1.21 matcher, which treats `"GET /items/{id}"` as a literal path (the handler is never reached, 404) and never sets `r.Pattern`. A `//go:debug httpmuxgo121=0` line in a `_test.go` file (allowed since Go 1.21) switches it for that test binary. Proven in a `go 1.21` scratch module: a `//go:build go1.23` test with `//go:debug httpmuxgo121=0` and `mux.HandleFunc("GET /items/{id}", …)` reads `r.Pattern == "GET /items/{id}"` on go1.26.4, and **fails** (handler not reached) with the directive removed; a `//go:build !go1.23` test with `mux.HandleFunc("/items/", …)` reaches the handler with `r.URL.Path == "/items/42"` on go1.21.13 and go1.22.12; a `go 1.21` **program** built with go1.26.4 gets `r.Pattern == ""` for `/items/` and 404 for `GET /x/{id}`, and the same program with `go 1.22` gets `"/items/"` and 200 (**run**, all). The directive may follow `//go:build`; `go vet` accepts it. Other behaviours keyed to the `go` line: loop variables are per-loop (Go 1.21 semantics) on every toolchain; `asynctimerchan=1` (pre-1.23 timer channels) — the SDK uses only `time.NewTicker` + `Stop` and `time.After` (`transport.go: worker, Flush`), where the two semantics do not differ; language features newer than 1.21 (range over an integer or a function) and standard-library APIs newer than 1.21 (`testing.T.Context`, `httptest.NewRequestWithContext`, `reflect.TypeFor`) do not compile on the 1.21 leg — a loud failure, not a silent one. | `packages/sdk-go/go.mod`; `net/http/server.go: (*ServeMux).ServeHTTP` (`use121` branch); Go `doc/godebug.md`; scratch modules (**run**) |
| T31 | zerolog's `Level.String()` returns the **mutable package globals** `LevelTraceValue` … `LevelPanicValue` (a program may set `zerolog.LevelWarnValue = "WARN"` or anything else), `"disabled"` for Disabled, `""` for NoLevel and the decimal number for any other value. A mapper fed `Level.String()` therefore depends on process-wide state it cannot see; one fed the `zerolog.Level` value does not. The root `sentinel` package imports no zerolog today; only `sentinelzerolog` does. core sets none of those globals today (`git grep 'Level[A-Za-z]*Value *='` on core `origin/main`, **run**: none). | `zerolog@v1.35.1/log.go: (Level).String`, `zerolog@v1.31.0/globals.go`; `packages/sdk-go/*.go` imports |
| T32 | CI's `workflow_dispatch` trigger runs **every** job of `ci.yml` on the dispatched ref: the step guards `github.event_name != 'pull_request'` let the unit suites run (`go-sdk`'s `go test` included), and `integration`, `contract` and `e2e`, guarded `== 'workflow_dispatch'`, run too. That is the only way to run the unit suites on a branch before merge (`push` is `main` only; `pull_request` skips tests, T17). All jobs run on the org's self-hosted pool (`runs-on: … tuju-ai-runner`), whose cgo/C-compiler provisioning this seat cannot see. | `.github/workflows/ci.yml: on`, job and step guards, header comment "PR scope" and "Runners" |
| T33 | `message` over **10,000 code points** rejects the whole event with 400: it is a pre-v2 field, the ingestor's bounds step does not touch it (`MaxMessageLength = 10000` is listed there only as documentation, "enforced by protovalidate (reject 400)"), and the `error_event.message` CEL rule is `this.message.size() <= 10000` — CEL `size()` of a string counts code points. `message` is **not** a fingerprint input: `fingerprint.Compute` hashes `CustomFingerprint` (returned verbatim when set), else `Environment`, `ErrorClass` (normalized), the stack frames and `Platform` — `FingerprintConfig` has no message field, and the processor's only call builds it from exactly those; the SDK's `NewEvent` sets `Message = err.Error()` and takes `Fingerprint` only from `ctxTags["fingerprint"]` and options, never from the message. No stored occurrence has a message over 10,000 code points (such an event was rejected), so truncating to 10,000 changes no stored row and no grouping. `ExtractStacktrace` captures at most 32 frames, under the `stacktrace` `max_items = 100` rule. | `packages/proto/sentinel/v1/error_event.proto` (`error_event.message` rule; the `message` field comment; `stacktrace` `repeated.max_items`); `apps/ingestor-go/validation/bounds.go` (const block comment, `MaxMessageLength`); `apps/processor-go/fingerprint/fingerprint.go: FingerprintConfig, Compute`; `apps/processor-go/event/event.go` (the `fingerprint.Compute(fingerprint.FingerprintConfig{…})` call); `packages/sdk-go/event.go: NewEvent, ExtractStacktrace` (`pcs := make([]uintptr, 32)`) |
| T34 | core serves HTTP with **gin** (`go.mod` requires `github.com/gin-gonic/gin v1.10.0`); nothing in core calls `sentinel.Init`, `sentinel.HTTPMiddleware` or any sdk-go middleware (`git grep` on core `origin/main` `dc653f34a`, **run**: the only `sentinel.Init` hit is a comment in `sink.go`). core already collects request data itself, redacted, into metadata (`pkg/sentinelx/httpx/metadata.go: Metadata, build, headerMap, headerRemoved, redactMap`), and the core contract's decision 4 is *"Nothing redacted today is sent after this change … it never widens what leaves core"*. | core `go.mod`, `pkg/sentinelx/sdk/sink.go`, `pkg/sentinelx/httpx/metadata.go` @ `dc653f34a`; `prd/core-sentinel-typed-metadata/contract.md` "Decisions taken (do not re-open)" item 4 @ `4bc309357` |
| T35 | **What rejects a pre-v2 field.** `service.IngestAt` resolves `event_id` (empty, over 64 code points or holding a control character → a minted UUIDv4) and `timestamp` (clamped to receipt time), runs the enrichment and `ValidatePayload` — which bounds every **v2** value in place, `contexts` included (invalid names dropped, then ≤ 32 entries and ≤ 64 KiB), and rejects only a negative top-level `stacktrace[i].column` — then maps and runs protovalidate. `ValidatePayload` does **not** touch `project_key`, `platform`, `environment`, `message`, `error_class`, `trace_id`, `span_id`, `release_version`, `fingerprint` or the top-level `stacktrace`, so each is judged by protovalidate alone, and a violation is a **400 for the whole event** (`"validation failed"` → `http.StatusBadRequest`): `platform` not matching `^[a-z0-9]+$` or over 50; `environment` not matching `^[a-z0-9][a-z0-9._-]{0,49}$` (so an empty or upper-case one); `error_class` empty (`required`) or over 255; `fingerprint`, `trace_id`, `span_id` over 64; `release_version` over 100; over 100 `stacktrace` frames (`repeated.max_items`); a frame with `in_app` true and an empty `file` (`stack_frame.file_required`). A frame `line` outside `int32` fails the JSON decode (`StackFrame.Line int32`; the SDK's `Frame.Line` is `int`), also 400. `project_key` never reaches protovalidate as the client wrote it unless it named a real project: `applyAuthenticatedScope` overwrites it with the authenticated project's name, or (organization-wide key, no `X-Project-Key`) resolves it against `projects.name` and fails the request before validation. None of these events is stored, so none has an issue, a fingerprint or a release row. `fingerprint.Compute` hashes at most the first 5 usable frames (`MaxFrames`, algorithm `v2`), as `file:function`. The SDK never produces an in-app frame without a file (`isInAppFrame("")` is false) nor more than 32 frames (T33). The JS SDK drops an over-long `fingerprint` (field omitted, event sent), and truncates `error_class` and cuts `stacktrace` to 100 frames. | `packages/proto/sentinel/v1/error_event.proto` (every `cel` rule and field rule); `apps/ingestor-go/service/service.go: IngestAt, resolveEventID, resolveTimestamp`; `apps/ingestor-go/validation/validator.go: ValidatePayload, StackFrame`; `apps/ingestor-go/validation/bounds.go: applyBounds, boundContexts`, const block ("existing fields: enforced by protovalidate (reject 400)", `MaxFingerprintLength` comment); `apps/ingestor-go/main.go: applyAuthenticatedScope`, `handleIngest` (the `"validation failed"` branch); `apps/processor-go/fingerprint/fingerprint.go: Compute`; `apps/processor-go/fingerprint/grouping.go: MaxFrames`; `packages/sdk-go/event.go: ExtractStacktrace, isInAppFrame`; `packages/sdk-js/src/core/event.ts: applyBounds, readFingerprint` |
| T36 | `Transport.Push` never blocks: when the channel (capacity `Config.MaxBufferSize`) is full it evicts the **oldest** queued event and `droppedCount` +1, **without** calling `OnError` (which is called only from `dropBatch` / `handlePartialFailure` on the worker). A test that pushes more events than the buffer holds, faster than the worker drains them, therefore loses events silently. **Only `Init` applies `withDefaults`** (`MaxBufferSize` 100, `BatchSize` 10, `BatchWait` 1 s); `NewTransport(cfg)` uses `cfg` as given (§12 F6). So through `NewTransport`, `MaxBufferSize` 0 is an **unbuffered** channel: `Push` succeeds only while the worker is parked in its receive, and otherwise drops the event (**run**, go1.26.4: 20 events pushed to `NewTransport(Config{Endpoint, BatchSize: 1, BatchWait: time.Second})`, 0 received, `droppedCount` 20); `BatchSize` 0 sends every event alone (`len(batch) >= 0`); `BatchWait` 0 panics the worker (`time.NewTicker`). core's `sink.go: Open` sets all three explicitly (T24). `droppedCount` is unexported, readable from a test in package `sentinel` with `atomic.LoadUint64`. | `packages/sdk-go/transport.go: NewTransport, Push, workerLoop, dropBatch, handlePartialFailure`; `config.go: withDefaults`; `client.go: Init`; core `pkg/sentinelx/sdk/sink.go: Open`; scratch test (**run**) |
| T37 | `ScrubPII` returns a **new** top-level map and recurses into every nested `map[string]interface{}` value, returning new maps at every depth; every other value — a slice, a `map[string]string`, a pointer, a struct — is copied by reference. On the `Client` path without `additionalContext`, `NewEvent`'s `ctxTags` **is** the context's own stored map (`getTagsMap`), which every capture from that context passes. So today a `[]interface{}` value, and every map inside it, is shared by every event of the context, and the keys of a map inside a list are not scrubbed (`{"items": [{"password": "x"}]}` is sent as is); a map that holds itself recurses without end. This spec changes `ScrubPII` to a deep copy through maps **and** `[]interface{}` (§6.2 item 3, round 12). | `packages/sdk-go/pii.go: ScrubPII`; `client.go: CaptureErrorContext` |
| T38 | **A bounded event can exceed the ingestor's body cap.** The single-event endpoint reads at most 1,048,576 bytes (`http.MaxBytesReader(w, r.Body, maxEventBodyBytes)`; a body of exactly that size is read, one byte more is a 413); the batch endpoint at most 5,242,880 bytes and 500 items (`maxBatchBodyBytes`, `maxBatchSize`; each a 413). The transport treats a 413 like every non-429 4xx: not retried, the whole body's events dropped through `dropBatch`. `json.Marshal` escapes `<`, `>` and `&` as `<`-style sequences (6 bytes each) and control characters likewise, so a code point costs up to 6 bytes on the wire: 64 headers of 4,096 `'<'` — within §3.4's header bounds — marshal to **1,573,569 bytes** (**run**, go1.26.4), and four such events in one batch to over 6 MiB. **Where a body goes:** v0.3.x sends a batch of two or more events to `{endpoint}/batch` as a JSON array and a lone event to `{endpoint}` as a bare object (`sendBatch`, `len(batch) > 1`); the JS SDK sends every batch, a lone event included, to `{endpoint}/batch`. The batch handler accepts a one-element array (it rejects only an empty one, `len(payloads) == 0` → 400), so the 1 MiB single-event cap binds only a client that posts to `{endpoint}`. It accounts every item alike: an item that fails for any reason — scope, validation, or an internal error such as a failed publish — is counted in `failed` and the response is 202 when at least one item was ingested, **400** when none was; the single handler answers an internal error with **500**, which the transport retries. | `apps/ingestor-go/main.go: maxEventBodyBytes, maxBatchBodyBytes, maxBatchSize`, `handleIngest` and `handleBatchIngest` (`http.MaxBytesReader`, `isBodyTooLarge`, the `len(payloads) == 0` and `len(payloads) > maxBatchSize` branches, the `result.Ingested == 0` → `StatusBadRequest` branch); `packages/sdk-go/transport.go: sendBatch` (the `len(batch) > 1` endpoint choice, the "Every other 4xx" branch); `packages/sdk-js/src/core/options.ts` (the batch URL `endpoint.replace(/\/$/, '') + '/batch'`); scratch test (**run**) |
| T39 | **An accepted event can still be lost at the NATS publish.** `service.IngestAt` publishes every accepted item **alone**: enrichment, `ValidatePayload`, `mapping.MapPayloadToEvent`, protovalidate, `proto.Marshal(event)`, then `publish` → `Publisher.PublishWithHeaders` (a JetStream `PublishMsg` carrying the injected trace headers). nats.go refuses a message whose `len(data)` plus header block exceeds the server's `max_payload` with `ErrMaxPayload`, and no NATS server this repo runs sets `max_payload` (`git grep -i max_payload` finds nothing; no stream sets a `MaxMsgSize` either), so it is nats-server's default, **1,048,576 bytes**, in compose, helm and both config files. A failed publish is an internal item error: the batch handler counts it in `failed` (T38) and the event is gone. **Proto size tracks JSON with HTML escaping off, not `json.Marshal`:** proto writes a string as its UTF-8 bytes, which is what an encoder with `SetEscapeHTML(false)` writes for every code point except `"`, `\` and control characters (2-6 bytes in JSON), while `json.Marshal` also writes `<`, `>` and `&` as 6 bytes each. Measured on a minimal event through the real `ValidatePayload`, mapper and `proto.Marshal` (**run**, go1.26.4, `dde0ea2`): 64 headers of 4,096 `'<'` — 1,573,968 bytes marshalled, 263,248 with escaping off, **263,155** proto; 64 headers of 4,096 U+1F600 (4 bytes in every encoding; within §3.4's bounds) — 1,049,680 both ways, **1,049,715** proto: under every HTTP cap, accepted, and lost at the publish. The exception is **numbers inside a `google.protobuf.Struct` field** (`metadata`, `contexts`, `request.data`, breadcrumb `data`): each is an 8-byte double inside a nested `Value`, ~11 bytes per list element in proto against 2 in JSON — a `metadata` list of 30,000 zeros is 60,298 bytes of JSON and **330,113** of proto, and an event with such lists in all four Struct fields, which the server's bounds leave whole, 257,499 and **1,399,384** (**run**). T40 gives the `Struct` encoding exactly, and what the server keeps of `metadata` and `contexts`. **What grows an event between the SDK and NATS** (§3.5 sizes its fixed allowance from this): the NATS header block (`traceparent`, ~85 bytes framed; `tracestate` and `baggage` only when the incoming request carried them, otel-go capping baggage at 8,192 bytes); `enrich.ApplyWithBudget` — `user.ip_address` is never filled for `go` (T12), `user.geo` only when the SDK sent an address (§6.4; three texts ≤ 128 code points and a country code ≤ 8), and `contexts.browser` / `client_os` parsed from a `User-Agent` of at most 1,024 bytes (`MaxUserAgentBytes`), each a name and a version; `received_at` (field 30); `project_id` (field 16, a 36-character UUID: 39 bytes), which the SDK never sends; `project_key` overwritten with the project's name (T35, ≤ 64 code points); and the `server_truncated` / `server_truncated_keys` markers, written only when the server cut something — which for an event §3.4 bounds is nothing (§3.5, "The margin") — and even should a rule drift, at most **599** bytes of protobuf: `server_truncated` naming all 35 fields of `obs.TruncatedFieldVocabulary`, since `server_truncated_keys` is written only by a cut that removes a key, and every such cut is one §3.4 makes first (the full pair, keys included, can reach 7,347, T40). Together about 15 KB at worst and under 1 KB typically. Proto's per-entry map overhead over JSON is a few bytes an entry (+35 bytes over the 64-header event above), at most 6 for a §3.4-bounded entry (§3.5). | `apps/ingestor-go/service/service.go: IngestAt, publish`; `packages/shared-go/nats/nats.go: PublishWithHeaders`; `nats.go@v1.52.0/nats.go: publish` (the `msgSize > nc.info.MaxPayload` → `ErrMaxPayload` check); nats-server `server/const.go: MAX_PAYLOAD_SIZE`; `docker-compose.yml` (`nats` `command`), `deploy/helm/sentinel/templates/dependencies.yaml` (nats `args`), `apps/nats/nats-server.conf`, `scripts/nats-server.conf`; `packages/shared-go/obs/provider.go` (TraceContext + Baggage propagator); `apps/ingestor-go/enrich/enrich.go: ApplyWithBudget, applyGeo, applyUA, MaxUserAgentBytes`; `apps/ingestor-go/validation/bounds.go: writeMarkers, shrinkMetadata, MaxServerTruncatedKeys, MaxMetadataSize, MaxContextsSize`; `packages/proto/sentinel/v1/error_event.proto` (`google.protobuf.Struct metadata`, `contexts`, `data`; `project_id = 16`); scratch test (**run**) |
| T40 | **What the ingestor publishes of the four `Struct`-typed fields, and what that costs in protobuf.** `service.IngestAt` runs `ValidatePayload` — whose `applyBounds` runs `boundContexts`, then `writeMarkers` and `shrinkMetadata` — before `mapping.MapPayloadToEvent` and `proto.Marshal`, so `metadata` and `contexts` reach the protobuf only as that bound leaves them. **`shrinkMetadata`:** when `jsonSize(metadata)`, markers included, is over `MaxMetadataSize` (65,536), it removes whole top-level keys — every key outside `metadataExemptKeys` before any key in it, the largest `entrySize` (`len(json.Marshal(key)) + 1 + len(json.Marshal(value))`) first, a tie to the lexically smallest key (`sortVictims`) — while the object with both markers is over 65,536; the markers are never removed, and a client's value under either marker key is deleted or overwritten (`writeMarkers`). The exempt set is 18 keys: `user_id`, `userId`, `user`, `tenant_id`, `tenantId`, `organization_id`, `trace_id`, `traceId`, `trace-id`, `span_id`, `spanId`, `span-id`, `request_id`, `requestId`, `request-id`, `release_version`, `truncated`, `truncated_keys`. **`boundContexts`:** U+0000 is stripped by `nulMap` (keys and strings at every depth; renamed keys re-inserted in sorted order, a key already present wins, an empty key is dropped); a name that collides after stripping (names visited in sorted order) or has 0 or over 64 code points is dropped; then contexts are removed — every name outside `wellKnownContexts` (`device`, `os`, `app`, `runtime`, `browser`, `culture`) first, the largest `entrySize` first, a tie to the smallest name — while there are over `MaxContexts` (32) or the object is over `MaxContextsSize` (65,536). Enrichment inserts a `User-Agent` context only when the result still fits (`validation.ContextFits`), so it never changes which of the client's contexts are kept. `request.data` and a breadcrumb's `data` are stripped of U+0000 and dropped whole over 16 KiB / 4 KiB. **Encoding** (`structpb.NewStruct` / `NewValue` in `MapPayloadToEvent`, `mapContexts`, `mapRequest`, `mapBreadcrumbs`; `google/protobuf/struct.proto`): a `Value` is a oneof — `null_value` and `bool_value` 2 bytes, `number_value` an 8-byte double, 9 bytes; `string_value`, `list_value` and `struct_value` length-delimited — and a `Struct` a `map<string, Value>`, each entry a length-delimited message of its key and its value, so a number is **11** bytes in a list where JSON spends 2, and a string entry `k + v + 8` where JSON spends `k + v + 6`. **Measured** (**run**, go1.26.4, `dde0ea2`: a scratch module driving the real `ValidatePayload`, `MapPayloadToEvent` and `proto.Marshal`; the base event has one frame, `sdk`, `level`, `server_name` and the SDK's three small contexts), as JSON with escaping off / published protobuf: the sdk-js review's case, `metadata` {`a`: 32,710 zeros} (65,427 bytes of JSON, which the server keeps whole) and 32 crumbs of `data` {`a`: 2,030 zeros} (§3.4's 128 KiB keeps 31) — 193,436 / **1,053,347**; plus a context of 32,600 zeros — 258,649 / **1,411,975**; a number-dense event within every server bound (`metadata` 20,000 zeros, a context of 10,000, `request.data` 8,000, 20 crumbs of 2,000) — 158,359 / 859,271; 64 headers × 4,096 U+1F600 — 1,049,902 / 1,049,909; × `'<'` — 263,470 / 263,349. **Round 10** (**run**, the same scratch approach, 2026-09-26), on events **as §3.4 and §3.5 now send them** — `metadata` and `contexts` at most 61,440 bytes, U+0000 and collisions already resolved — the server's bounds step writes no marker and leaves the payload deep-equal on every one, and §3.5's `publishCost` (parts 1-5 as specified) against the published protobuf is: spec review round 10's run (1), `contexts` {`a`, `a\x00`} and `metadata` {`m`: 32,748 zeros}, 32 headers × 4,096 U+1F600 and 100 frames of a 3,000-byte `function`, sent with `a\x00` dropped and `m` removed client-side — 830,747 / **826,463** (with `m` kept, the size the server would have kept at 65,503 bytes, it publishes 1,186,672: over `max_payload`); run (2), the duplicate empty names dropped and `metadata` {`m`: 30,650 zeros} kept at 61,363 bytes — 337,637 / 337,450; run (3), `request.data` {`q`: `"a\x00"`} sent as {`q`: `"a"`}, `metadata` {`m`: 32,764 zeros} removed client-side and 100 frames of a 6,500-byte `function` — 655,919 / 651,709; `metadata` (61,330 bytes) and a context (61,410) of number lists at the margin, `request.data` 8,000 zeros and 20 crumbs of 2,000 — 1,204,648 / 1,203,481 (so §3.5 sheds its crumbs); and 2,000 random events (nested lists and objects of numbers, bools, nulls and strings of `<`, `"`, `\`, newline, U+1F600, U+0000 inside `metadata`): the estimate is **never under**, and over by 230 to 4,942 bytes on the 521 the server leaves uncut. The markers, when the server writes them, are at most **7,347** bytes of protobuf: `server_truncated` with all 35 fields of `obs.TruncatedFieldVocabulary` (599 bytes alone; 558 bytes of JSON as a `metadata` entry with its comma), `server_truncated_keys` with 50 keys of 128 bytes and a `+N more`. | `apps/ingestor-go/service/service.go: IngestAt`; `apps/ingestor-go/validation/bounds.go: applyBounds, shrinkMetadata, writeMarkers, boundContexts, boundRequest, boundBreadcrumbs, nulMap, nulDeep, sortVictims, entrySize, objectSize, ContextFits, metadataExemptKeys, wellKnownContexts, MaxMetadataSize, MaxContexts, MaxContextsSize, MaxServerTruncatedKeys, MaxServerTruncatedKeyBytes`; `apps/ingestor-go/validation/validator.go: ErrorPayload, RequestInfo, Breadcrumb` (the `Struct`-typed fields' Go types); `apps/ingestor-go/enrich/enrich.go` (the `ContextFits` guard); `apps/ingestor-go/mapping/mapper.go: MapPayloadToEvent, mapContexts, mapRequest, mapBreadcrumbs`; `packages/shared-go/obs/obs.go: TruncatedFieldVocabulary`; `packages/proto/sentinel/v1/error_event.proto` (`metadata = 9`, `contexts = 26`, `Request.data = 5`, `Breadcrumb.data = 6`); scratch test (**run**) |
| T41 | **What the server's identifier replacement, its timestamp clamp and its decoder do to an SDK event.** (Round 9's **Markers** paragraph, which measured `shrinkMetadata`'s `size()` for a replay of the server's cut, is removed in round 10 with the replay it served; the server's shrink order is T40's.) **Identifiers.** `resolveEventID` re-mints an `event_id` that is empty, over 64 code points or holds a control character (`r < 0x20 || r == 0x7f`) as a 36-character UUIDv4, before the mapping; `applyAuthenticatedScope` overwrites `project_key` with the authenticated project's name, or fails the item when a project-scoped key's body names another project, or when an organization-wide key's body names none it can resolve — so a sent `project_key` over 64 code points is never published. **Timestamps.** `resolveTimestamp` replaces a zero timestamp, one more than `MaxFutureSkew` after the receipt time or more than `MaxEventAge` before it, with the receipt time in UTC (R1-R3), and writes no marker; `time.Time.MarshalJSON` fails for a year outside [0, 9999] (`Time.MarshalJSON: year outside of range [0,9999]`, spec review round 10: `time.Unix(<unix millis>, 0)` is year 58,706) and for a zone offset of 24 hours or more, on go1.21.13 and go1.26.4, so such a value never reaches the server: `json.Marshal` of the event fails. No other field of `Event` or `Breadcrumb` has a type whose `MarshalJSON` can fail on a valid Go value (strings, `int`, `uint32`, `bool`, `map[string]string`, `[]Frame`); the `interface{}`-typed `metadata`, `contexts`, `request.data` and crumb `data` can hold any value, a `time.Time` among them. **Decoder.** `handleBatchIngest` decodes the whole body with one `json.NewDecoder(r.Body).Decode(&payloads)` and answers `400 Invalid JSON payload array` on any error, so one undecodable event costs every event of its body. `encoding/json` refuses two things `json.Marshal` writes: a `*big.Int` of 10^400 in one event's `metadata` marshals (a 3-event body of 1,269 bytes) and fails the decode with `json: cannot unmarshal number 10…0 into Go struct field ErrorPayload.metadata of type float64` (an `UnmarshalTypeError`); `metadata` {`a`: k nested lists}, in a one-element array, decodes at k = 9,997 and fails at k = 9,998 with `exceeded max depth` (`encoding/json: maxNestingDepth` = 10,000 counts the array, the event object and `metadata` too), while `json.Marshal` writes both. | `apps/ingestor-go/service/service.go: resolveEventID, resolveTimestamp` (R1-R3); `go doc time.Time.MarshalJSON`; `apps/ingestor-go/main.go: applyAuthenticatedScope, handleBatchIngest`; `encoding/json` (go1.26.4): `scanner.go: maxNestingDepth`, `decode.go: convertNumber`; scratch test (**run**) |

### 1.3 Deviations from the contract

Each row: what the contract says, what is true, and what this spec does. D1-D4 and D8-D9 need no contract change; D5-D7
touch a file outside `be.md`'s scope or replace a check the contract names, and each carries the coordinator's ruling
(spec review round 1).

| # | Contract says | What is true | This spec |
|---|---|---|---|
| D1 | §4.4 "`transaction` = the route pattern where `net/http` exposes it (**Go 1.22+** `Request.Pattern`, else the path)". | `Request.Pattern` is Go **1.23** (T15). A file reading it under a `go1.22` build constraint does not compile on a 1.22 toolchain. And the toolchain is not the only gate: `ServeMux` sets `r.Pattern` only when `httpmuxgo121=0`, and that GODEBUG defaults from the **main module's `go` line** (T30) — so a consumer whose main module declares `go 1.21` gets the path by default even when built with Go 1.26, and so does this module's own test binary. | The pattern is read under `//go:build go1.23`. A program gets the pattern only when it is built with Go 1.23+, uses `ServeMux`, and runs with `httpmuxgo121=0` — the default when its main module declares `go 1.22` or newer (core: `go 1.26.3`, T24), or set with `//go:debug httpmuxgo121=0`. Every other case — an older toolchain, a main module declaring `go` < 1.22 (by default), an explicit `GODEBUG=httpmuxgo121=1`, a non-`ServeMux` router — gets the path (§6.3, CHANGELOG §7.1). The go1.23 test file carries `//go:debug httpmuxgo121=0`; the pre-1.23 test uses a pattern Go 1.21's matcher understands (P3.6). |
| D2 | §1 item 6: "the ingestor … blanks anything else, so `warn` and `panic` would be lost". | True for a typed level, but today the hook never sends `warn` at all (it fires at ≥ Error), and it **does** fire for `NoLevel` and `Disabled` (T7), so `logger.Log().Msg(…)` is captured as an error today. | The hook keeps firing for Error, Fatal and Panic only and sends the typed level; `NoLevel`/`Disabled` stop being captured (§3.6, a behaviour change listed in the CHANGELOG). The mapper still maps `warn` for callers that send warnings through `NewEvent`. |
| D3 | §4.2 "The SDK trims to the same limits **so an event is not dropped server-side**." | The server never drops an event for an over-bound v2 value; it truncates or drops the value and writes `server_truncated` markers (T9). What does drop an event is a wrong JSON type, an oversized body (T10), or a pre-v2 field over its protovalidate rule — `message` over 10,000 code points among them (T33). And an event the ingestor **accepted** is lost when its proto, with its NATS headers, is over NATS's 1,048,576-byte `max_payload`: the publish fails and the item is counted `failed` (T39) — which a bounded event under the 5 MiB body cap can reach. | The SDK trims to the server's limits anyway — `metadata` and `contexts` included, with a 4,096-byte margin (so the server cuts nothing and writes no marker, and the SDK can size each event as sent, §3.5) — posts every body to `{endpoint}/batch` and fits every body — a lone event's one-element array included — under 5 MiB and 500 items (§3.5; a bounded event alone can exceed 1 MiB, the single endpoint's cap, which the SDK therefore never uses, T38), fits every event's **publish cost** — an estimate, never below it, of the protobuf the ingestor publishes for exactly what the SDK sends: the typed fields from their JSON with HTML escaping off, the `Struct`-typed fields at their protobuf size (a number 11 bytes in a list), `metadata` and `contexts` whole, since the SDK now bounds both itself, 4,096 bytes under the server's 64 KiB, and the server cuts nothing — under 1,000,000 bytes, so no event fails the publish for its size (§3.5, T39, T40), the types are fixed so no v2 value can 400 an event (§3.5), and **every pre-v2 field protovalidate judges alone is bounded too** (T35): `message`, `error_class`, `fingerprint`, `release_version`, `trace_id`, `span_id`, `platform`, `environment` and the top-level `stacktrace` (§3.4). §3.4's closing table lists every rule of `error_event.proto` and what satisfies it, so no value an SDK event can carry is rejected with 400. The proof is that the server's own bounds step changes nothing in an SDK-bounded worst case, and protovalidate accepts it (§10.2 P1.8). |
| D4 | §4.1 "`contexts.os { name: runtime.GOOS }` (plus arch)". | The dashboard labels `arch` only under `device` (T26); sentry-go puts it in `device.arch`. | `contexts.os = {name: GOOS}`, `contexts.device = {arch: GOARCH}` (§3.2). |
| D5 | §5 / be.md: proofs "run under `task test-unit` / the repo's Go test gate (CI job at `ci.yml:108-145` runs `packages/sdk-go` with `GOWORK=off` …)"; §4.2 "-race tests". | `task test-unit` does not run `packages/sdk-go`; the `go-sdk` job's `go test` has no `-race` and runs only on push to `main` (T17). A `-race` proof is therefore not gated anywhere today. | **Ruling: SANCTIONED AND TIGHTENED.** The gate for this feature is §10.1's command list, all under `GOWORK=off` with `-race`, and two files outside `be.md`'s scope change so that it is gated, not optional: (a) `Taskfile.yml`'s `test-unit` gains the command `cd packages/sdk-go && GOWORK=off go test -race -count=1 ./...`, directly after `go test ./apps/ingestor-go/...` (§9 pins the position) — `test-unit` is the plane's unit gate, so the SDK's race proofs run wherever that gate runs; (b) `.github/workflows/ci.yml`'s `go-sdk` job's `go test` step becomes `go test -race ./... -count=1` with `env: CGO_ENABLED: "1"` on both matrix legs (the runner then needs cgo and a C compiler, T21, and the step fails loudly — never skips, never falls back to a run without `-race` — without them). Because a push to `main` is the first automatic run of that step (T17), the implementing seat proves it **before merge** by dispatching `ci.yml` on its branch (§10.1, "Pre-merge proof", T32). (a) and (b) are listed in §9, and all three in §10.1. |
| D6 | §5 TASK-01 "the contract test against the ingestor's `ErrorPayload`", with every proof under `GOWORK=off`. | That test imports sdk-go from the root module, so it builds only in workspace mode, and its CI job is dispatch-only (T18). It cannot run under `GOWORK=off`. | **Ruling: SANCTIONED.** The contract test is extended (§9) and stays a supplementary proof. The gated proof is a **committed wire fixture** bridging the two modules: an sdk-go test (GOWORK=off) proves the SDK emits exactly `packages/sdk-go/testdata/wire_v2_*.golden.json`, and a `tests/unit` test (root module, GOWORK=off) decodes those files strictly into the real `ErrorPayload`, runs the real bounds step, mapper and protovalidate (§10.2 P1.7-P1.8). `tests/unit/` is in this feature's scope by that ruling. |
| D7 | §4.5 "`go get …@v0.4.0` resolves (checked with `GOPROXY=direct` **and the default proxy**)". | The repository is private (T23): `proxy.golang.org` can never serve it, and `GOPROXY=direct` alone still consults `sum.golang.org`, which cannot see it either. | **Ruling: SANCTIONED.** The check replaces the default-proxy check and is run the way consumers (core) fetch it: `GOPRIVATE=github.com/tuju-ai/*` (which implies direct fetch and no checksum-DB lookup) in a scratch module, plus `go list -m` of the tag (§7.2), **after merge**. It is a post-merge command, not a Go test: the tag does not exist before merge. |
| D8 | Header "The last tag is `packages/sdk-go/v0.3.2` (`4c8763c`)". | The tag points at the merge commit `78218f3`; `4c8763c` is the change it merged (T22). | Citation only; nothing depends on it. |
| D9 | §4.1 "`WithTag` writes `tags`". | Callers pass `WithTag(ctx, "request_id", …)` (the SDK's own test does, T20), and `request_id`, `trace_id`, `span_id` and their aliases are read **only** from metadata by the search index (T11); `NewEvent` reads `fingerprint` and `error_class`, and the processor `release_version`, from metadata by name (T27); `WithTag` takes `interface{}` values, but `tags` is `map[string]string` (T10). Routing every key to `tags` would silently remove request-id search, context fingerprints and error classes, and the release fallback for existing callers. | `WithTag` writes `tags`, except every key the SDK, ingestor or processor reads from metadata by name (which keep going to metadata, as decision 4 requires for `request_id`; the complete audited set, with the reader of each, is §4.2's table), `user_id` (→ typed `user.id`), non-scalar values (→ metadata), and a scalar that could not be a valid tag — a key failing `^[a-zA-Z0-9_.:-]{1,32}$`, or a string form that is empty, over 200 code points or contains `\n` (→ metadata, where v0.3.x put it; §4.2, §11.2 item 12). `ApplyContext` carries the context's metadata onto the event on both paths (§4.3). The contract's "tag keys that fail … and bad values are dropped" (§4.1) is kept for `tags` itself: the bounds step drops any such entry that reaches `Event.Tags` (§3.4). |

---

## 2. What this change does, and does not do

**Does:**
1. Adds the v2 fields to `Event` as ordinary exported fields with the ingestor's JSON names and types, all `omitempty`
   (§3.1), and makes `NewEvent` fill `level`, `sdk`, `server_name` and the `runtime`/`os`/`device` contexts (§3.2).
2. Moves `WithUser`/`WithTenant`/`WithTag` off metadata onto `user`/`tags` (§4.2), adds `WithUserInfo` (struct form),
   `WithLevel`, context readers (including `MetadataFromContext`), and one function, `ApplyContext`, that copies
   everything a context carries onto an event — metadata included — so the `NewEvent` path gets exactly what `Client`
   gets (§4.3).
3. Adds a per-request breadcrumb buffer on `context.Context` (§5).
4. Adds `Config.BeforeSend` and `(*Config).ApplyBeforeSend` (§6.1).
5. Makes `HTTPMiddleware` fill `request` and `transaction`, with an opt-in for the client IP (§6.2-§6.5).
6. Bounds every event client-side in the transport, on a copy (the SDK never writes into a map, slice or pointer it did
   not allocate for the event, §8 guarantee 1) — `metadata` and `contexts` included, 4,096 bytes under the server's
   64 KiB, with U+0000 stripped and key collisions resolved where the server would, so the server's bounds step cuts
   nothing and writes no marker (§3.4) — marshals events one by one and decodes each as the ingestor will, so a value
   that cannot be marshalled, or that the ingestor cannot decode, costs only its own entry and never its event or its
   batch, posts every body — a lone event as a one-element array — to `{endpoint}/batch`, trims an event whose
   one-element array is over the 5 MiB batch body cap or whose publish cost — the estimated protobuf of exactly what
   it sends — is over 1,000,000 bytes (the ingestor publishes each event alone to NATS, whose 1 MiB `max_payload` it
   would otherwise fail, T39, T40), and cuts batches under 5 MiB and 500 items (§3.5).
7. Exports a level mapper, `NormalizeLevel`, and `sentinelzerolog.LevelFromZerolog`; the zerolog hook sends the typed
   level and the logger's context (§3.6).
8. Aligns the SDK's PII key list with `docs/sdk-specification.md` §5 (§6.2, T14).
9. Releases `packages/sdk-go/v0.4.0` with CHANGELOG entries for v0.3.1, v0.3.2 and v0.4.0 (§7).

**Does not** (contract §2, plus what this spec declines):
- change core, the ingestor, the processor, the dashboard or the proto; add tracing or spans; raise `go 1.21`; add a
  dependency (the standard library, zerolog and otel/trace only);
- set `logger`, `dist`, `source`, `exception` or `user.geo` (the contract does not ask; the server derives geo);
- trust `X-Forwarded-For`, `Forwarded`, `X-Real-IP` or any other client-IP header, or offer an option to (contract
  §4.4: "not trusted by default"; a caller behind a proxy sets `user.ip_address` itself with `WithUserInfo`, §6.4) —
  and those headers are never **recorded** either, so a client address cannot reach the wire through
  `request.headers` (§6.2);
- record a request body or `request.env` (§6.2);
- shed `metadata` or `contexts` to fit a size limit, or write any marker but its own: the transport applies the
  server's bounds to both (item 6) and otherwise sends them whole, recording what those bounds removed in the JS
  SDK's `truncated` / `truncated_keys` keys (§3.4). Only when an event's one-element array would exceed the 5 MiB batch
  body cap, or its publish cost 1,000,000 bytes (T39, T40), does the transport shed whole fields — request maps,
  request data, breadcrumbs — in a fixed order, and drop the event with `OnError` if that is not enough (§3.5); §12
  F2 records what remains;
- change `sentinellog` or `sentinelslog` (they set no level today and capture only at ≥ Error, where `error` is the
  default; they gain breadcrumbs, user and tags through the context they already pass — `sentinelslog` passes `ctx`,
  `sentinellog` has none).

---

## 3. Typed fields and automatic contexts (TASK-01)

### 3.1 The wire struct

New exported types in `event.go`. JSON names and Go types equal `apps/ingestor-go/validation/validator.go`'s
(`SDKInfo`, `UserInfo`, `RequestInfo`, `Breadcrumb`); `UserInfo` omits `Geo` (server-derived). Every field is
`omitempty` so an empty value is absent, never `null` — with one exception, `Breadcrumb.Timestamp`, which the SDK always
sets (§5.2).

```go
const (
	SDKName    = "sentinel.go"
	SDKVersion = "0.4.0" // equals the CHANGELOG's newest heading and the tag (§7, P4.1)
)

// Levels the ingestor accepts (apps/ingestor-go/validation/bounds.go: validLevels).
const (
	LevelFatal   = "fatal"
	LevelError   = "error"
	LevelWarning = "warning"
	LevelInfo    = "info"
	LevelDebug   = "debug"
)

type SDKInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type UserInfo struct {
	ID        string `json:"id,omitempty"`
	Username  string `json:"username,omitempty"`
	Email     string `json:"email,omitempty"`
	IPAddress string `json:"ip_address,omitempty"`
}

type RequestInfo struct {
	Method      string            `json:"method,omitempty"`
	URL         string            `json:"url,omitempty"`
	QueryString string            `json:"query_string,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Data        interface{}       `json:"data,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

type Breadcrumb struct {
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type,omitempty"`
	Category  string                 `json:"category,omitempty"`
	Level     string                 `json:"level,omitempty"`
	Message   string                 `json:"message,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
}
```

New `Event` fields, appended after `Metadata` (existing fields and their tags are unchanged):

```go
	Level       string                            `json:"level,omitempty"`
	SDK         *SDKInfo                          `json:"sdk,omitempty"`
	ServerName  string                            `json:"server_name,omitempty"`
	Transaction string                            `json:"transaction,omitempty"`
	User        *UserInfo                         `json:"user,omitempty"`
	Tags        map[string]string                 `json:"tags,omitempty"`
	Contexts    map[string]map[string]interface{} `json:"contexts,omitempty"`
	Request     *RequestInfo                      `json:"request,omitempty"`
	Breadcrumbs []Breadcrumb                      `json:"breadcrumbs,omitempty"`
```

Pointers for `SDK`, `User` and `Request` match the ingestor's pointers, so "absent" and "present but empty" stay
distinct on both sides. The `Event` doc comment's B5 warning (T1) is extended to name these fields and the golden files
of §10.2 P1.7.

### 3.2 What `NewEvent` fills

`NewEvent`'s signature is unchanged. After the existing fields and **before** `opts` run (so an option wins), it sets:

| Field | Value |
|---|---|
| `Level` | `LevelError` |
| `SDK` | `&SDKInfo{Name: SDKName, Version: SDKVersion}` |
| `ServerName` | `cfg.ServerName` if non-empty; else `os.Getenv("SENTINEL_SERVER_NAME")` if non-empty; else `os.Hostname()` (the pod name on Kubernetes), resolved once per process with `sync.Once`; `""` if that fails. The environment variable is read per call, so a test can set it with `t.Setenv`. (Contract decision 6.) |
| `Contexts` | a **new** outer map per event holding three **new** inner maps per event: `runtime: {name: "go", version: v}`, `os: {name: runtime.GOOS}`, `device: {arch: runtime.GOARCH}` (D4). `v` is `runtime.Version()` with a leading `go` removed when a digit follows (`go1.26.4` → `1.26.4`, as `docs/sdk-specification.md` §3's example shows); any other form (`devel …`) is sent as is. Only the **scalar** values (`v`, GOOS, GOARCH) may be computed once and cached; no map — outer or inner — is ever cached or shared between events, because a caller, an option or `BeforeSend` may write `ev.Contexts["os"][k]` on one event while the transport marshals another (P1.5). |
| `Tags` | a new, empty, non-nil `map[string]string` (so a `NewEvent` caller can write `ev.Tags[k] = v`; an empty map is omitted on the wire). |
| `Metadata` | `ScrubPII(ctxTags)` as today when `ctxTags` is non-nil; when it is nil — core's call for an event with no metadata, any `NewEvent(cfg, err, nil)` — a new, empty, non-nil `map[string]interface{}` instead of the nil `ScrubPII` returns for nil (`pii.go: ScrubPII`, `metadata == nil` → `nil`), so an option, a caller or `BeforeSend` can write `ev.Metadata[k] = v` on every event `NewEvent` builds, as with `Tags`. The wire is unchanged: `metadata` is `omitempty`, so an empty map is omitted exactly as nil was. |

`NewEvent` also records, in two **unexported** `Event` fields (never marshalled), the `ErrorClass` and `Fingerprint` it
computed **before** `opts` run (from `err` and `ctxTags`, exactly as today), and the SDK's own options
`WithEventErrorClass` / `WithEventFingerprint` additionally set an unexported flag each. An option is taken to have set
the class (fingerprint) when its flag is set **or** the value after `opts` differs from the recorded one. `ApplyContext`'s
metadata row reads this so that an option wins over the context's metadata on both keys (§4.3). The one case it cannot
see — a caller's own `EventOption` that assigns exactly the value `NewEvent` had already computed — is indistinguishable
from no option by construction; the SDK's options and any assignment after `ApplyContext` (§8) are exact. On an `Event`
not built by `NewEvent` the recorded values are empty, so a non-empty class or fingerprint on it counts as set.

"A context or field the caller already set wins" (contract §4.1) is a matter of order and is the same on both paths:
`NewEvent` sets defaults → `opts` → `ApplyContext` (§4.3) adds what the context carries (its table says, row by row,
whether the context or the event's existing value wins) → the caller's own assignments → `BeforeSend` (§6.1) →
client-side bounds in the transport (§3.5). A caller that wants its own `os` context assigns `ev.Contexts["os"]` after
`NewEvent`; replacing `ev.Contexts` wholesale also works and drops the automatic ones.

`Metadata` stays `ScrubPII(ctxTags)` (a new empty map for nil `ctxTags`, the row above): `NewEvent` does **not** move keys out of `ctxTags` into typed fields. Only the
context helpers change where they write (§4.2). This keeps core's `NewEvent(cfg, nil, ev.Metadata)` byte-identical in
metadata until core itself moves fields (T24); `ApplyContext` then adds the context's metadata keys the event does not
already carry (§4.3).

### 3.3 Level mapper

```go
// NormalizeLevel maps a level name to one the ingestor accepts, or "" when there is none.
func NormalizeLevel(level string) string
```

Case-insensitive, surrounding whitespace ignored: `fatal`, `error`, `warning`, `info`, `debug` map to themselves;
`warn` → `warning`; `panic` → `fatal`; `trace` → `debug`; anything else → `""`. Nothing else is guessed (no `critical`,
`err`, `notice`): the contract names these three, and an unknown level becomes absent exactly as the server would make
it (T9).

### 3.4 Client-side bounds: the fields the SDK owns

The SDK applies the server's rules (T9) to every field it emits — `metadata` and `contexts` included, 4,096 bytes
under the server's 65,536 (§3.5, "The margin") — so the server's bounds step changes nothing and writes no marker, and
protovalidate's rules to the pre-v2 fields the bounds step does not look at (T35), so nothing it emits is rejected
with 400. They are applied **once, in the transport** (§3.5), so the `Client` path and the `NewEvent` + `Push` path get
exactly the same treatment and a `BeforeSend` result is bounded too. String lengths are in code points on a UTF-8
boundary (the server's `truncateRunes`); sizes are `len(json.Marshal(v))` (the server's `jsonSize`); both are taken
after every invalid UTF-8 byte of a typed string, tag, header or env entry has become one U+FFFD (§3.5 step 1), so
they are taken on the text the server decodes. The rows marked
**decoded** — `contexts`, `metadata`, `request.data` and breadcrumb `data`, and the breadcrumb list's byte size, which
counts each crumb's `data` — run in §3.5 step 3 on the value the ingestor will decode (numbers `float64`, a Go struct
an object), not on the Go value a caller put there, because that is what the server strips and measures (a
`json.Number` `1e20` is 4 bytes as the caller wrote it and 21 as the server re-marshals it); the SDK then **sends**
that decoded value, so what it measured is what the server receives. A value inside one of those four fields that
cannot be marshalled or decoded is removed by §3.5 step 2 (the entry, never the event). U+0000 is removed from every
**v2** string and map key this table bounds, and from every key and string of `contexts`, `request.data` and
breadcrumb `data` at every depth — exactly where the server strips it (`cuts.str`, `cuts.nul`, `nulDeep`, `nulMap`),
and where the server would otherwise mark it. A map key that **collides** with another once stripped is resolved the
server's way: the key sent without a U+0000 wins, else the lexically smallest original key, and a key left empty is
dropped (`boundTags`, `boundStringMap`, `nulMap`) — so the server sees neither a U+0000 nor a collision in those
fields. `metadata`, which the server does not strip, is not stripped. The pre-v2 rows (`message`, `error_class`,
`fingerprint`, `release_version`, `trace_id`, `span_id`, `platform`, `environment`, `stacktrace`) apply only their
own rule and do not strip it (a U+0000 makes `platform` or `environment` fail its pattern, so those two rows'
replacement removes it).

| Field | Rule |
|---|---|
| `level` | `NormalizeLevel`; `""` means absent. |
| `server_name`, `transaction` | truncate to 256 / 200. |
| `message` | truncate to **10,000** code points (T33: over that, protovalidate rejects the **whole event** with 400). `message` is not a fingerprint input — `fingerprint.Compute` never reads it and the SDK derives no fingerprint from it (T33) — so cutting it changes no issue's grouping, and no stored message is longer (such events were rejected). Code points are counted with `utf8.RuneCountInString` and cut on a rune boundary; an invalid UTF-8 byte is by then one U+FFFD (§3.5 step 1), one code point, exactly as `json.Marshal` sends it and CEL counts it. U+0000 is **not** removed from `message` (a pre-v2 field whose handling is unchanged; only its length is new). |
| `error_class` | **truncate** to 255 code points; empty → `"Error"` (`NewEvent`'s own class for a nil error, T2). Not dropped: the field is `required`, so an empty class is itself a 400 (T35). Truncated rather than replaced because it is the issue's title and a fingerprint input read as a label: the first 255 code points keep what a reader recognises, and no stored issue has a longer class (such events were rejected), so no existing issue splits; two classes sharing their first 255 code points now group together, where before both were rejected. |
| `fingerprint` | over 64 code points → `""` (**dropped**: the server then computes the default grouping from class, frames, environment and platform). Not truncated: a custom fingerprint is stored verbatim as the grouping key (`bounds.go: MaxFingerprintLength`, "a shortened grouping key would merge issues"), so its first 64 code points could equal another issue's key and merge two unrelated groups; the JS SDK drops it for the same reason (T35). |
| `release_version` | **truncate** to 100 code points. Not dropped: the release is per-process configuration (`Config.ReleaseVersion`), so every event of the process gets the same cut value, and equality — all that regression detection and "resolved in release" compare — survives; dropping would switch both off for the whole service. |
| `trace_id`, `span_id` | each over 64 code points → `""` (**dropped**), independently; `trace_flags` untouched. Not truncated: they are per-event identifiers that point into a trace backend, and a cut id points at no trace (or a wrong one). |
| `platform` | not matching `^[a-z0-9]+$` or over 50 → `"go"`. Only an assignment after `NewEvent` (which sets `"go"`) can produce it, and every event this SDK builds is a Go event. It is a fingerprint input, but no stored event carries an invalid platform (T35). |
| `environment` | kept when it matches `^[a-z0-9][a-z0-9._-]{0,49}$`; otherwise **normalized**: lower-cased (`strings.ToLower`), every maximal run of characters outside `[a-z0-9._-]` replaced by one `-`, leading characters outside `[a-z0-9]` removed, cut to 50 (ASCII by then, so bytes = code points); an empty result → `"development"` (the environment `withDefaults` already gives an unset `Config.Environment`, `config.go: withDefaults`). So `Production` → `production`, `Prod EU` → `prod-eu`, `" staging"` → `staging`, `""` (a hand-built `Config`, as core's is, T24) → `development`. Replaced rather than dropped: the field is required and the regex rejects `""`, so there is nothing to drop to. It is configuration, so every event of the process lands in the same, recognisable environment; today every one of them is rejected (T35). |
| `stacktrace` | over 100 frames → the first 100 (Sentinel's order: crashing frame first); a frame with `InApp` true and an empty `File` → `InApp` false (`stack_frame.file_required`); a `Line` outside `int32` → `0` (the ingestor decodes `int32`, T35). A new slice is built when any frame changes. Frames are fingerprint inputs, but `Compute` reads only the first 3 in-app frames, and every event these rules change was rejected before (T35), so no stored issue splits. `File` and `Function` are **not** cut (§12 F7). |
| `event_id` | empty, over 64 code points, or holding a control character (below U+0020, or U+007F) → a **new UUIDv4**: exactly the replacement the server makes before it maps the event (`service.go: resolveEventID`, T41), so the id sent is the id published and the publish cost counts it as sent (§3.5). `NewEvent` mints a valid id, so only an assignment (core's, a `BeforeSend`'s) reaches this row; the server would have stored the event under a fresh UUID either way. |
| `timestamp` | `.UTC()` (removes a zone offset of 24 hours or more, which `time.Time.MarshalJSON` refuses); then a year outside [0, 9999] → `time.Now().UTC()`. `MarshalJSON` fails for such a year (T41), and a failed marshal would cost the event; a timestamp that far from now is one the ingestor clamps to its receipt time anyway (`resolveTimestamp` R2/R3, T41), so the stored value is "now" on either side. Any other timestamp is sent as that instant in UTC, and the server clamps it as today. |
| `sdk.name`, `sdk.version` | truncate to 128 / 64. `sdk` with both empty → `nil`. |
| `user.id` | over 255 **bytes** → `""` (dropped, never truncated: a cut id is a different user). Measured after §3.5 step 1's U+FFFD replacement, as the server measures the id it decoded: a 255-byte id holding one `0xff` is 257 bytes and is **dropped**, as the server drops it; a 253-byte id holding one is sent as its 255-byte U+FFFD form. |
| `user.username` | truncate to 128. |
| `user.email`, `user.ip_address` | over 254 / 45 code points → `""`. `user` with every field empty → `nil`. |
| `tags` | U+0000 removed from every key and value, a collision resolved as above; then a key matching the SDK PII list (§6.2) keeps its entry with value `"[FILTERED]"`; then an entry is **dropped** if its key fails `^[a-zA-Z0-9_.:-]{1,32}$`, or its value is empty, over 200 code points or contains `\n`; then, when over 50 keys remain, **50 are kept: `tenant_id` when present, plus the lexically smallest other keys** (49 of them beside `tenant_id`, else 50; byte order, Go's `sort.Strings`). Without `tenant_id` that is exactly the server's cut (the 50 lexically smallest, T9); with it, the server receives 50 keys and cuts nothing. `tenant_id` is kept because it is the processor's affected tenant (T11) and metadata no longer carries it (§4.2); it sorts after every key starting with a digit, an upper-case letter, `-`, `.`, `:`, `_` or `a`-`s`, so a plain lexical cut drops it (P1.6). The sibling sdk-js spec keeps it the same way (its §3.5 rule 6). A new map is built; the caller's map is not modified. The key and value checks are one unexported predicate, `validTag(key, value string) bool`, which `WithTag` also uses to route a value that could not be a tag to metadata instead (§4.2); so what this row drops is only what reached `Event.Tags` directly (a `NewEvent` caller's or `BeforeSend`'s assignment) or a key over the 50-key cap (never `tenant_id`). |
| `request.method`, `.url`, `.query_string` | truncate to 16 / 2048 / 4096. |
| `request.headers`, `request.env` | U+0000 removed from every key and value, a collision resolved as above; a key of 0 or over 256 code points is dropped; values truncated to 4096 (headers) / 1024 (env); then only the 64 lexically smallest keys are kept. New maps. |
| `request.data` | **decoded**: U+0000 removed at every depth (collisions as above); then `jsonSize` over 16 KiB → `nil`, and the event is sent without it (never dropped for it). A value that cannot be marshalled, or that the ingestor cannot decode (a number beyond `float64`, such as a `*big.Int` of 10^400), is set to nil by §3.5 step 2 and the event sent. (The middleware never sets it; a `NewEvent` caller may.) |
| `breadcrumbs` | keep the last `n`, where `n` = the transport's `Config.MaxBreadcrumbs` (≤ 0 → `DefaultMaxBreadcrumbs` = 50; over 100 → 100); per crumb truncate `type`/`category`/`message` to 64/128/8192, `level` through `NormalizeLevel`, `timestamp` `.UTC()`, and a zero one or one whose year is outside [0, 9999] → the event's `timestamp` (after the `timestamp` row, so already valid); **decoded**: `data` with U+0000 removed at every depth (collisions as above), then with `jsonSize` over 4096 → `nil` (the crumb and the event are kept; `data` that cannot be marshalled or decoded is set to nil the same way by §3.5 step 2); then drop the oldest while the list is over 128 KiB **measured as the ingestor re-marshals it** (T28): each crumb's size is `jsonSize` of an unexported mirror of the ingestor's `Breadcrumb` — the same six fields and JSON names with **no** `omitempty`, so empty strings are counted as `""` and a nil **or empty** `data` as `null` (the SDK's `omitempty` drops an empty map, and the ingestor re-marshals the absent key as `null`, T28; the mirror's `data` is set to nil when the crumb's map has length 0) — and the list's size is the sum plus `n-1` commas plus 2 brackets (the ingestor's `arraySize`); crumbs are dropped while that size is **over** 131,072 bytes, so a list of exactly 131,072 is kept, as on the server. Measuring the SDK's own `omitempty` shape instead under-counts every sparse crumb and lets the server cut one (T28's 200 × 8,100 case). A new slice is built. |
| `contexts` | **decoded**, in `boundContexts`' order (T40): U+0000 removed from every name, key and string at every depth (collisions as above; among names, visited in sorted order, a cleaned name already present loses); a name of 0 or over 64 code points dropped; then, while there are over **32** contexts or their object is over **61,440** bytes (65,536 − 4,096; measured as `boundContexts` measures it, `objectSize` over each `entrySize(name, value)`), one is removed: every name outside the six well-known ones (`device`, `os`, `app`, `runtime`, `browser`, `culture`: `bounds.go: wellKnownContexts`) first, the largest `"name":value` first, a tie to the smallest name (`sortVictims`). A new map. Each context removed is recorded in the SDK's marker (below). |
| `metadata` | **decoded**, last (the server shrinks it last, `applyBounds`): the keys `server_truncated` and `server_truncated_keys` removed (the server deletes or overwrites a client's value under either, `writeMarkers`, T27); **not** stripped of U+0000 (the server does not strip `metadata`); then, when the object is over **61,440** bytes, the shrink runs: `"metadata"` joins the SDK's marker (below), which replaces any value a caller put under its two keys, and keys are removed — every key outside `bounds.go: metadataExemptKeys` (18 keys, T40, copied) before any key in it, the largest `entrySize(key, value)` first, a tie to the lexically smallest key — one at a time while the object **with the SDK's marker** (below) is over 61,440: `shrinkMetadata`'s order and its `size()`, with the SDK's marker where the server has its own. The marker's two keys are never removed. A new map. |

**The SDK's marker.** When the `contexts` or `metadata` row, or §3.5 step 2, removes a context or a `metadata` key, the
transport writes two `metadata` keys — the JS SDK's own markers (T27), which are in `metadataExemptKeys`, so a server
shrink would remove them last — in `server_truncated`'s shape (`bounds.go: writeMarkers, removedKeys`):
- `truncated`: `["contexts"]`, `["metadata"]` or `["contexts", "metadata"]` — each field an entry of which was removed,
  and `"metadata"` also whenever the `metadata` shrink ran;
- `truncated_keys`, when a key is recorded: `"contexts.<name>"` for each context removed, in name order and without
  duplicates (a name that lost a collision is recorded only when no context of that name was kept: `appendLost`'s
  rule, so `{"": …, "\x00": …}` records `"contexts."` once), then each `metadata` key removed, in removal order —
  each with U+0000 removed and cut to 128 bytes on a UTF-8 boundary (`truncateBytes`), the first 50 listed and the
  rest as one `"+N more"` entry.

It is written — replacing a caller's value under either key, as the server's `writeMarkers` replaces a client's
`server_truncated` — only when the transport removed something or the shrink ran; otherwise a caller's `truncated` /
`truncated_keys` is sent as it is (and counts toward the 61,440 like any key). The pair is at most **38,622** bytes of JSON (50 keys of 128 bytes that each marshal to 6-byte escapes, and
the `"+N more"`), so the `metadata` shrink, which never removes it, always ends at or under 61,440. The `contexts`
row runs before the `metadata` row, so the shrink measures the marker with the contexts' entries already in it. Only
removals are recorded: a U+0000 stripped, or a `request.data` / crumb `data` set to nil, is not (§12 F7).

**Not bounded client-side:** `project_key`, which the server replaces or resolves from the credential (a cut name
would name a different project; §3.5 counts it as sent, and the project name the server may publish in its place is
in the allowance); and the `file` / `function` of a frame (§12 F7). Up to round 9 `contexts` and `metadata` were here
too, left to the server's shrink, and §3.5 replayed that shrink to count what the server would keep; the replay had
to predict the server's cut byte for byte — its markers, its collisions, its de-duplication — and needed a fix in
four successive rounds. Applying the server's own rules client-side, with a margin, leaves the server nothing to cut,
so the publish cost measures the event as sent (§3.5). The cost is the margin: the SDK keeps up to 4,096 bytes less
of `metadata` and of `contexts` than the server would (§7.1). Earlier revisions also listed `message` and
`stacktrace` here, and left `error_class`, `fingerprint`, `release_version`, `trace_id`, `span_id`, `platform`,
`environment`, `event_id` and `timestamp` unmentioned; each could 400 its event, fail its marshal or be counted at a
size the server never publishes (T35, T41), so each now has a row above.

**Every rule in `packages/proto/sentinel/v1/error_event.proto`, and what satisfies it for an SDK event.** "SDK" means
the row of the table above, applied by the transport to every pushed event on both paths; "server" means a step the
ingestor runs **before** protovalidate (T35), so the rule can never fail on its value; "absent" means the SDK's `Event`
has no such field, so the key is never on the wire.

| Rule (message: id or field rule) | Satisfied by |
|---|---|
| `ErrorEvent`: `error_event.project_key` (1-64), `project_key` `required` | server: replaced by the authenticated project's name, or resolved against `projects.name` before validation (T35); a misconfigured key is a scope error, which no bound could fix |
| `error_event.platform` (`^[a-z0-9]+$`, ≤ 50), `platform` `required` | SDK: `platform` row |
| `error_event.environment` (`^[a-z0-9][a-z0-9._-]{0,49}$`), `environment` `required` | SDK: `environment` row |
| `error_event.message` (≤ 10,000) | SDK: `message` row |
| `error_event.error_class` (≤ 255), `error_class` `required` | SDK: `error_class` row |
| `error_event.trace_id`, `error_event.span_id` (≤ 64) | SDK: `trace_id`, `span_id` row |
| `error_event.release_version` (≤ 100) | SDK: `release_version` row |
| `error_event.fingerprint` (≤ 64) | SDK: `fingerprint` row |
| `error_event.event_id` (≤ 64) | SDK: `event_id` row (server: `resolveEventID` replaces an empty, over-long or control-character id with a UUIDv4) |
| `error_event.level` | SDK: `level` row (server: blanks an invalid one) |
| `error_event.server_name` (≤ 256), `error_event.transaction` (≤ 200) | SDK: their row (server: truncates) |
| `error_event.logger`, `error_event.dist`, `error_event.source` (≤ 64) | absent (server: truncates) |
| `error_event.tags` | SDK: `tags` row (server: drops, keeps 50) |
| `error_event.contexts` (≤ 32 entries, names 1-64) | SDK: `contexts` row (server: `boundContexts`, which then has nothing to drop or evict) |
| `stacktrace` `repeated.max_items = 100` | SDK: `stacktrace` row |
| `breadcrumbs` `repeated.max_items = 100` | SDK: `breadcrumbs` row (≤ `MaxBreadcrumbsLimit`); server: keeps the last 100 |
| `Sdk`: `sdk.bounds` | SDK: `sdk.*` row (server: truncates) |
| `User`: `user.bounds` | SDK: `user.*` rows (server: drops or truncates) |
| `Geo`: `geo.bounds` | absent from the SDK's `UserInfo`; geo is server-derived and server-bounded |
| `Request`: `request.bounds`, `request.headers`, `request.env` | SDK: `request.*` rows (server: truncates, drops) |
| `Breadcrumb`: `breadcrumb.bounds` | SDK: `breadcrumbs` row (server: truncates, blanks the level) |
| `ExceptionInterface` `values` `max_items = 20`; `ExceptionValue`: `exception_value.bounds`, `stacktrace` `max_items = 100`; `Mechanism`: `mechanism.bounds` | absent: the SDK sends no `exception` (server: `boundException`) |
| `StackFrame`: `stack_frame.file_required` | SDK: `stacktrace` row (and `ExtractStacktrace` never makes such a frame, T35) |
| `StackFrame`: `stack_frame.v2_bounds`, `pre_context` / `post_context` `max_items = 5` | absent: the SDK's `Frame` has only `function`, `file`, `line`, `in_app` (server: `boundFrameV2`) |

Outside the proto, the one other 400 on a value is `ValidatePayload`'s negative top-level `stacktrace[i].column`: absent
(the SDK's `Frame` has no `column`). A wrong JSON **type** is excluded by the struct types (§3.1), the frame `line` range
by the `stacktrace` row, and a `time.Time` `MarshalJSON` refuses by the two timestamp rules. A value inside
`metadata`, `contexts`, `request.data` or a breadcrumb's `data` that cannot be marshalled, or that marshals but that
the ingestor's `json.Decode` refuses — a number beyond `float64` (a `*big.Int` or `json.Number` of 10^400), or
nesting deeper than `encoding/json`'s 10,000 levels, which would fail the decode of the **whole body**, a 400 for
every event in it (T10, T41) — is found by §3.5 step 2, which decodes each event as the ingestor will: the `metadata`
key or the context holding it is removed and recorded in the SDK's marker, `request.data` or the crumb's `data` is set
to nil, and the event is sent. A body over 5 MiB / 500 items — a 413, not a 400 (T38) — is excluded by §3.5's
body-cap step, which posts every body to `{endpoint}/batch` (so the single endpoint's 1 MiB cap never applies), trims
an event whose one-element array is over 5 MiB or whose publish cost — the estimated protobuf of exactly what it
sends — is over 1,000,000 bytes (T39, T40: past NATS's 1 MiB the ingestor answers 202 and then loses the event at the
publish) and cuts batches; only an event still over either limit with its request maps, request data and breadcrumbs
removed is dropped, alone and through `OnError` (§12 F2). `P1.8`'s root half runs the ingestor's bounds step and
protovalidate on the bounded goldens, which carry a value over every SDK row above, and requires the bounds step to
change nothing.

### 3.5 Where the bounds run, and one event per marshal

The model is **apply with headroom, then measure what is sent**. The transport applies every bound the server's bounds
step applies — `metadata` and `contexts` included, 4,096 bytes under the server's 65,536 — so that step has nothing
left to cut and writes no marker; it then measures the event it is about to send, exactly as sent. Nothing in this
section predicts a cut the server makes (up to round 9 it did: the publish cost replayed the server's `metadata` and
`contexts` shrink, its markers included, and the replay needed a fix in rounds 7, 8, 9 and 10 because it had to
match the server's cut byte for byte, §11.2 item 26).

`Transport.sendBatch` (worker goroutine) gains a first step, per event, in four parts:

1. **Bound a copy.** `b := boundEvent(ev, max)` applies every §3.4 row not marked **decoded** — every typed field,
   both timestamp rules, the crumb count and each crumb's text and level — and returns a **bounded shallow copy** of
   the pushed event; the pushed `*Event` itself is never written (below).

   **Before any row runs, every typed string is made valid UTF-8 exactly as `json.Marshal` makes it** (round 12):
   each invalid byte becomes **one U+FFFD per byte** — `validUTF8(s)` (unexported) walks `s` with
   `utf8.DecodeRuneInString` and writes `"\uFFFD"` for every `(utf8.RuneError, 1)` result, keeping a valid U+FFFD in
   the input (`(RuneError, 3)`) as it is. **Not** `strings.ToValidUTF8`, which writes one U+FFFD for a whole run of
   invalid bytes where `json.Marshal` writes one per byte (`encoding/json: appendString`), so `"a\xfe\xff"` would
   measure 4 bytes against the server's 7. The typed strings are every `string` of `Event` and of its typed parts —
   `level`, `server_name`, `transaction`, `message`, `error_class`, `fingerprint`, `release_version`, `trace_id`,
   `span_id`, `trace_flags`, `platform`, `environment`, `event_id`, `project_key`, `sdk.*`, `user.*`,
   `request.method` / `url` / `query_string`, each frame's `file` and `function`, each crumb's `type`, `category`,
   `message` and `level` — and **every key and value of `tags`, `request.headers` and `request.env`**, the three maps
   the SDK bounds as strings. The ingestor decodes each such byte as that same U+FFFD (`json.Marshal` writes it as the
   escape `\ufffd`, which decodes to the valid 3-byte rune), so after this rule every byte and code point the SDK
   measures is the one the server measures: `user.id`'s 255 bytes, each code-point bound, the crumb list's 131,072
   (`jsonSize` of a string holding a raw invalid byte counts the 6-byte escape; the server re-marshals its U+FFFD as
   3 bytes), and step 4's two sizes. Before it, a 255-byte `user.id` holding one `0xff` was kept by the SDK and
   dropped by the server (257 bytes decoded), and a crumb list the server measures at exactly 131,072 was measured 3
   bytes over per invalid byte and lost a crumb the server keeps (P1.6). A map key the replacement makes equal to
   another — `"X-A\xfe"` and `"X-A\xff"` both become `"X-A\uFFFD"` — is resolved as a U+0000 collision is (§3.4):
   the key that needed no replacement wins, else the lexically smallest original key; so the server receives one key
   and never picks between two (its decode keeps the last). The replacement runs first; U+0000 removal and every row
   then run on its result. Each changed string is assigned on `b` or on the copies below, never on the pushed event.
   The four **decoded** fields need no such rule, at any depth, keys included: step 2's marshal and decode makes the
   same replacement (a key collision resolved as the server's decode resolves it — `json.Marshal` writes map keys in
   the sorted order of their original bytes, and the decode keeps the last), and step 3 measures and sends that
   decoded form.
2. **Marshal it alone, and decode it as the ingestor will.** `raw := json.Marshal(b)`; then the one-element array
   `[` `raw` `]` is decoded with `json.Unmarshal` into `m`, a one-element slice of the unexported **decode mirror**:
   the four `Struct`-typed fields with the ingestor's Go types — `map[string]interface{}` for `metadata` and a crumb's
   `data`, `map[string]map[string]interface{}` for `contexts`, `interface{}` for `request.data` — and nothing else.
   The enclosing array puts every value at the depth it has in any body (the array and the event object are the two
   levels above every event), because `handleBatchIngest` decodes the **whole body** with one `Decode` and answers 400
   to all of it on any error (T10, T41), and `encoding/json` refuses two things `json.Marshal` writes: a number beyond
   `float64` (a `*big.Int` or `json.Number` of 10^400 marshals as its digits) and nesting over its 10,000-level limit.

   After step 1 no typed field can fail either: every typed field has the ingestor's Go type (§3.1, T10), the frame
   `line` is in `int32`, and both `time.Time` fields are in UTC with a year in [0, 9999] — the one typed value whose
   `MarshalJSON` fails on a valid Go value (T41; round 10's audit found no other). So a failure comes from a value a
   caller put into one of the four `Struct`-typed fields: to the marshal, a `func`, `chan`, complex number, `NaN` or
   ±Inf, a cyclic value, a `time.Time` outside year [0, 9999] or with an offset of 24 hours or more, an invalid
   `json.Number` or `json.RawMessage`, or a type whose own `MarshalJSON` / `MarshalText` fails; to the decode, a number
   beyond `float64` or nesting too deep. Then every entry of those fields is marshalled and decoded **alone at its
   body depth** — each `metadata` key as `[{"metadata":{"k":V}}]`, each context as `[{"contexts":{"name":V}}]`,
   `request.data` as `[{"request":{"data":V}}]`, each crumb's `data` as `[{"breadcrumbs":[{"data":V}]}]`, `V` the
   entry's own `json.Marshal` — which names every entry that fails, and each is removed from `b` (from a copy of the
   map, of `b.Request` or of the crumb slice; the pushed event is never written): a `metadata` key or a context is
   **removed and recorded in the SDK's marker** (§3.4), `request.data` or a crumb's `data` is set to nil. `raw` is
   then marshalled and decoded again, which cannot fail by the above; should it, the event is dropped alone
   (`droppedCount` +1, `Config.OnError` naming its `event_id` and the error), so step 3 only ever sees an event whose
   `m` decoded. **No event is dropped for a value inside it**: v0.3.x lost every event of the batch to one marshal
   failure (`transport.go: sendBatch`, "marshal batch"), and a body holding an undecodable value is answered 400 for
   all of its events (T41, P1.6).
3. **Bound the decoded fields, and send them.** §3.4's **decoded** rows run on `m`, in the server's order
   (`applyBounds`: `contexts`, `request.data`, the breadcrumbs, then `metadata` last, so its shrink measures the SDK's
   marker with whatever the `contexts` row recorded). Each measures what the server measures — the decoded value,
   re-marshalled with `json.Marshal` (`jsonSize`, `entrySize`, `objectSize`) — and the four results replace the fields
   on `b` (new maps and slices, never the caller's): **the SDK sends the decoded form** of `metadata`, `contexts`,
   `request.data` and every crumb's `data`, and `raw` is marshalled from `b` once more. The server decodes that `raw`
   back to exactly `m` (`encoding/json` round-trips its own output: a `float64` is written in the shortest form that
   parses back to it), so every size this step measured is the size the server measures, and its bounds step finds
   nothing to cut (P1.8). The wire changes only where the server changes the value on decode anyway: a number is sent
   as the `float64` the server holds (`int64` 2^53+1 as 9007199254740992), a Go struct as an object with sorted keys,
   invalid UTF-8 as U+FFFD (in the typed fields step 1 already made it), and U+0000 is gone from `contexts` and both `data` fields.
4. **Fit the ingestor's two per-event limits.** Every body goes to `{endpoint}/batch` (below), so the HTTP cap an
   event must fit is the batch endpoint's, not the single endpoint's 1 MiB, which the SDK never posts to (T38). And the
   ingestor publishes every accepted event **alone** to NATS, whose `max_payload` is 1,048,576 bytes (T39), so an event
   must fit that too. Two sizes are measured on `b`:
   - its **one-element array** — `len(raw)` plus the two brackets, what it costs in a body — against **5,242,880
     bytes** (the ingestor's `maxBatchBodyBytes`; exactly 5,242,880 is kept, as `http.MaxBytesReader` keeps it, T38);
   - its **publish cost** — `publishCost(b, m)`, defined below: the protobuf size of exactly what is sent, estimated
     from above — against **1,000,000 bytes** (`maxPublishBytes`: `max_payload` less the fixed allowance below;
     exactly 1,000,000 is kept).

   While **either** is over, shed one step and measure both again, in this order: (a) `request.headers` and
   `request.env`; (b) `request.data`; (c) `breadcrumbs`; (d) the event itself — dropped alone, `droppedCount` +1 and
   `Config.OnError` with an error naming its `event_id`, the limit it is over and both sizes after (c). Each step sets
   the field to nil on `b` (and on a copy of `b.Request`), never on the pushed event, sets the same field to nil in
   `m`, and re-marshals `b` for the next measurement. Under `Debug` each step taken is logged with the event id and the
   limit that forced it. No SDK-bounded field can alone pass 5 MiB — the request maps, the largest, reach ~1.5 MiB (64
   keys × 4,096 code points of headers, up to 6 bytes each on the wire, T38; `env` 64 × 1,024 is up to ~0.5 MiB, and
   the SDK never sets it), `metadata` and `contexts` are at most 61,440 bytes each as sent, `data` ≤ 16 KiB and the
   crumb list ≤ 128 KiB (§3.4) — so the 5 MiB cap is reached only by what §3.4 leaves unbounded: frame `file` /
   `function` (and `project_key`, which is configuration). The **publish** limit is reached by more: headers alone (64
   × 4,096 code points of 4 UTF-8 bytes each are 1,048,576 bytes of values, T39), number-dense `Struct` fields — a
   number costs 11 bytes of protobuf in a list where JSON spends 2 (T40), so breadcrumb `data` reaches ~0.7 MB within
   its 128 KiB, `request.data` ~90 KB within its 16 KiB, and `metadata` and `contexts` ~338 KB each within their
   61,440 bytes — and frame `file` / `function`. Every other field §3.4 bounds is under ~180 KB even written in 6-byte
   escapes (`message` 60,000, `tags` ~61,000, `url` and `query_string` ~37,000), and `timestamp`, `trace_flags` and
   `event_id` (≤ 64 code points, ≤ 256 bytes) are small. So an event without frame text is at most ~860 KB of
   estimate once (a)-(c) are shed: after (c), only frame `file` / `function` can hold an event over either limit (§12
   F2, F7). An event whose one-element array is at most 5,242,880 bytes and whose publish cost is at most 1,000,000 is
   never shed. The order sheds first what is largest and least diagnostic, while the error itself — class, message,
   stack, tags, user, trace — and the caller's `metadata` and `contexts` are never shed. No marker is written into the
   event for a shed field (§12 F7); `request.method`, `url` and `query_string` stay, so the event still says which
   request it was.

**The publish cost.** `publishCost(b, m)` (unexported, `publishcost.go`) is the protobuf size of the event exactly as
the transport sends it — `b`'s typed fields and `m`'s four `Struct`-typed fields, which step 3 made equal to what `b`
sends — estimated from above. It replays nothing: the server's bounds step changes nothing in an event the transport
bounded (step 3, "The margin" below), so what the ingestor maps and marshals is what was sent, plus the few things
the ingestor adds, which the allowance carries. It reads `m` and decodes nothing, so it has no error to return. It is
the sum of five parts:

1. **The typed fields.** A copy of `b` with `Metadata`, `Contexts`, `Request.Data` and every breadcrumb's `Data` set to
   nil (on a copied `Request` and a copied `Breadcrumbs` slice; `b` is not changed), encoded by a `json.Encoder` with
   `SetEscapeHTML(false)`, its length less the trailing newline `Encode` writes; plus **6 bytes for each entry** of
   `Tags`, `Request.Headers` and `Request.Env`. `event_id` and `project_key` are counted as sent: `event_id` because
   §3.4 already made it one the server keeps, and `project_key` because the server publishes either the key sent or
   the authenticated project's name in its place (T41) — a sent key longer than the name only over-counts, and the
   name's own length is in the allowance. For these fields JSON with escaping off is never below protobuf: a string
   is written as its UTF-8 bytes (`"`, `\` and a control character take more in JSON, never less; step 1 left no
   invalid UTF-8 byte), each JSON key with its quotes, colon and comma is longer than the tag and length prefix it stands for — a
   frame, a breadcrumb or a `Timestamp` has tens of bytes of keys to protobuf's few, which also covers the one or two
   bytes a `Request`'s or a breadcrumb's length prefix grows by with its `data` (a negative `line`, a 10-byte varint,
   sits beside a frame's ~35 bytes of other keys) — and a `map<string, string>` entry is the one exception: a tag,
   the entry's length and two inner tags and lengths, at most 11 bytes for a §3.4-bounded header (2-byte key length,
   3-byte value and entry lengths) and 9 for a tag (2-byte field tag), where JSON has 5 (`"":""`) and a comma between
   entries — hence 6 an entry. Escaping is off because `json.Marshal` writes `<`, `>` and `&` as 6 bytes each, which
   protobuf writes as one: the 64-header `'<'` event is 1,573,968 bytes marshalled and 263,155 in protobuf (T39), and
   must not be shed.
2. **`metadata`**: `ld(S(metadata))` of `m`'s `metadata`, as sent — the SDK's marker included; 0 when absent.
3. **`contexts`**: over `m`'s contexts, as sent, the sum of `2 + vl(e) + e` with `e = ld(len(name)) + ld(S(context))`
   (field 26 has a 2-byte tag).
4. **`request.data`**: `ld(V(data))` of `m`'s `request.data`, when present.
5. **breadcrumb `data`**: `ld(S(data))` of `m`'s `data`, for each crumb whose `data` is present.

The protobuf sizes are those of `google/protobuf/struct.proto`, every tag inside it one byte: `vl(n)` is the length of
`n` as a varint (1 below 128, 2 below 16,384, 3 below 2,097,152, 4 below 268,435,456); `ld(n) = 1 + vl(n) + n`, a
length-delimited field of `n` bytes; for a decoded value `v`, `V(v)` is 2 for `null` or a bool, **9 for a number** (an
8-byte double), `ld(len(s))` for a string of `len(s)` UTF-8 bytes, `ld(Σ ld(V(x)))` over a list's elements, and
`ld(S(m))` for an object, where `S(m) = Σ ld(ld(len(k)) + ld(V(v)))` over its entries (a map entry: the key, then the
`Value`). So a number is 11 bytes in a list against JSON's 2 (`0,`), and a string entry `k + v + 8` against JSON's
`k + v + 6`, for short `k` and `v` (T40).

The estimate is exact for the four `Struct`-typed fields — counted as sent, with nothing left for the server to cut —
and over by the typed fields' JSON keys, so it is never under the protobuf of what was sent. Measured against the real
`ValidatePayload`, `mapping.MapPayloadToEvent` and `proto.Marshal` (T40, **run**): over by 187 to 4,284 bytes on spec
review round 10's three events as the SDK now sends them, 1,167 on `metadata` and a context of number lists at the
margin, and never under on 2,000 random events (over by 230 to 4,942 on the 521 the server leaves uncut). P1.8's root
half gates it on goldens that carry U+0000, key collisions, `metadata` and `contexts` at the margin and number-dense
data.

**The allowance: why 1,000,000.** `maxPublishBytes` is `max_payload` less a fixed allowance, 1,048,576 − 48,576. The
allowance carries what the ingestor adds that is not in the event as sent (T39): the NATS header block
(`traceparent`, and `tracestate` and `baggage` only when the incoming request carried them, otel-go capping baggage
at 8,192 bytes), enrichment (`user.geo` when the SDK sent an address, §6.4; `contexts.browser` / `client_os` parsed
from a `User-Agent` of at most 1,024 bytes, inserted only when `ContextFits`), `received_at`, `project_id` (39 bytes),
the project name in place of the sent key (≤ 64 code points), and the server's markers — none for an event the
transport bounded, and at most 599 bytes of protobuf should a rule drift (`server_truncated` naming all 35 fields,
below). About 15 KB at worst and under 1 KB typically, so the allowance is more than three times the growth. The
cost: an event whose estimate is over 1,000,000 while NATS would carry it — within the typed fields' few KB of keys
or the allowance — is shed when it need not be, and dropped if only frame text holds it there.

**The margin: why 4,096.** The server's `metadata` shrink runs only when `metadata`, **with the markers the server
writes**, is over 65,536 (`shrinkMetadata`), and `boundContexts` evicts only over 32 contexts or 65,536 bytes. The
transport sends `metadata` at most 61,440 bytes and `contexts` at most 32 names and 61,440 bytes, both measured as the
server measures them (step 3). What the server can still add before it measures: to `contexts`, only enrichment's
`User-Agent` contexts, which it inserts only when `validation.ContextFits` — measured exactly as `boundContexts`
measures — says the result still fits, so they never evict one (T40); to `metadata`, only its two markers. Those are
empty for a bounded event, and bounded even if they were not: `server_truncated_keys` is written only by a cut that
removes a key — `boundTags` (`tags.<key>`), `boundContexts` (`contexts.<name>`) and `shrinkMetadata` — and §3.4 makes
the first two with the server's own rule (the tags) or with more room (the contexts), and the third cannot run, so
it stays empty; `server_truncated` is at most the list of all 35 fields of `obs.TruncatedFieldVocabulary`, **558
bytes** of JSON as a `metadata` entry with its comma (T40). 61,440 + 558 = 61,998 ≤ 65,536, so the server's shrink
cannot run on an SDK event whatever the server marks, and no entry the SDK sends is removed. The margin is 4,096
rather than 558 so that it also covers a server bound tightened by a few KB, or a vocabulary grown past 35, without
the server cutting an SDK event before this SDK is released again; a larger margin would cost more of the caller's
data for no case this spec can name. **The trade:** the SDK keeps up to 4,096 bytes less of `metadata` and of
`contexts` than the server would (the server keeps up to 65,536 of each), stated in the CHANGELOG (§7.1). In return
the server never cuts an SDK event, so nothing the SDK measures can diverge from what the server publishes: spec
review round 10's three cases — a context name that collides once U+0000 is stripped, two names that both clean to
the empty name, a U+0000 in `request.data` — are resolved by the transport, and the server sees none of them (P1.11).

Then the survivors, in push order, are **cut into bodies**: a body holds at most **500** events (`maxBatchSize`) and
its size — the sum of its events' marshalled lengths plus one comma between each two plus the two brackets — is at most
**5,242,880 bytes** (`maxBatchBodyBytes`); an event that would push the body over either limit starts the next one.
Every body, a body of one event included, is sent to `{endpoint}/batch` as a JSON array built from the per-event
bytes (a lone event as a one-element array, ≤ 5 MiB by step 4), as v0.3.x already does for every batch of two or more
and the JS SDK does for every batch (T38); none → nothing is sent. The transport never posts to `{endpoint}` itself. Each body is sent, retried and accounted independently with
today's logic (`doSend`, the 5xx / 429 / 4xx branches, `handlePartialFailure`, `dropBatch`), so a failure costs only
that body's events; the response of every body is read as the batch result it now always is (`handlePartialFailure`).
A lone event thereby gets the batch handler's accounting (T38): an internal ingestor error on it is answered 400 and
not retried, where v0.3.x's `{endpoint}` answered 500 and the transport retried — as is already true today of every
event in a batch of two or more (§12 F9). Today one batch of `BatchSize` events is one body whatever its size, and a 413 drops all of it
(T38); `BatchSize` is the caller's, and nothing stopped it being over 500 either.

`Push` keeps its signature and behaviour. Its doc comment gains the ownership rule the transport relies on: *after
`Push` the event, and every map, slice and pointer it holds, belongs to the transport; the caller must not modify
them.* (core already complies: `sink.go` builds a fresh event per push, T24.) The transport, for its part, only reads
them.

**The transport never writes into what it was pushed.** `boundEvent` starts from `b := *ev` and, before it changes any
field, shallow-copies every pointer whose target it changes — `User`, `SDK`, `Request` — and builds a new map or slice
for every map or slice it changes — `Tags`, `Request.Headers`, `Request.Env`, `Breadcrumbs` and `Stacktrace` — then
assigns the copies to `b`. `Metadata`, `Contexts`, `Request.Data` and each crumb's `Data` it only reads: steps 2 and 3
replace them on `b` (on the copied `Request` and crumb slice) with new values built from the decode, and never edit
the caller's. Step 4 likewise changes only `b` and `b.Request`'s copy. So a pointer or map a caller shares between
events — an `EventOption` or `BeforeSend` assigning one `*UserInfo`, one `Tags` map or one `Metadata` map to many
events, over-bound or not — is never written, whatever the bounds cut, and the pushed `*Event` still holds exactly
what it held (§8 guarantee 1, P1.6, P1.10).

### 3.6 `sentinelzerolog`

```go
// LevelFromZerolog maps a zerolog level to a Sentinel level: Trace and Debug (and any numeric level below
// Trace) → debug, Info → info, Warn → warning, Error → error, Fatal and Panic → fatal, NoLevel and Disabled → "".
func LevelFromZerolog(l zerolog.Level) string
```

`Hook.Run` fires for `ErrorLevel`, `FatalLevel` and `PanicLevel` only (D2), and captures with the logger's context:
`sentinel.CaptureErrorContext(sentinel.WithLevel(e.GetCtx(), LevelFromZerolog(level)), errors.New(msg),
map[string]interface{}{"zerolog_level": level.String()})`. `zerolog_level` stays in metadata (nothing asks to remove it,
and a caller may filter on it). Through `e.GetCtx()` a log line written with `.Ctx(ctx)` now carries that context's user,
tags and breadcrumbs.

---

## 4. The context API (TASK-01)

### 4.1 Storage and concurrency

Each piece of Sentinel state on a context is an **immutable value** under its own unexported key, replaced
copy-on-write by the `With*` helpers (as `WithTag` does today): the typed user (`UserInfo`), the tags
(`map[string]string`, never mutated after it is stored), the level (`string`), the metadata map (unchanged), plus the
two mutable, lock-guarded objects of §5 (breadcrumb buffer) and §6.2 (request scope). A helper that writes a key to
the tags or to the metadata removes that key from the other one in the same call (§4.2, "One key, one store"), also
copy-on-write: it stores new maps and never edits the ones a parent context holds. A reader returns a copy. So any
number of goroutines may read one context, and `With*` never races with a reader. A nil `ctx` is accepted everywhere:
readers return zero values, `With*` helpers treat it as `context.Background()`, `ApplyContext` and `AddBreadcrumb` do
nothing.

### 4.2 Helpers

Existing signatures are unchanged (Compatibility, contract); what changes is where they write.

| Function | Behaviour from v0.4.0 |
|---|---|
| `WithUser(ctx, userID string) context.Context` | sets the typed user's `ID`, **keeping** any `Username`/`Email`/`IPAddress` the context's user already has. No longer writes metadata `user_id`. |
| `WithUserInfo(ctx, u UserInfo) context.Context` | **new**; replaces the context's typed user with `u` (the struct form). |
| `UserFromContext(ctx) (UserInfo, bool)` | **new**; the context's typed user, `false` if none. |
| `WithTenant(ctx, tenantID string) context.Context` | writes `tags["tenant_id"]` and removes `tenant_id` from the context's metadata (one key, one store, below). No longer writes metadata `tenant_id`. The processor reads the tag first (T11). A valid `tenant_id` tag survives the 50-key cap however many other tags the event has (§3.4 `tags` row). |
| `WithTag(ctx, key string, value interface{}) context.Context` | routed by key and value, in this order: (1) `key == "user_id"` with a scalar value → as `WithUser(ctx, str(value))`, and `key == "tenant_id"` with a scalar value → as `WithTenant(ctx, str(value))` (so `tenant_id` is never routed to metadata by rule 4, see below; a non-scalar value under either key falls to rule 3, as today); (2) `key` is in the **metadata-routed set** below (every key the SDK, ingestor or processor reads from metadata by name, except `user_id`/`tenant_id`, which the contract moves) → metadata, as today (D9, decision 4); (3) `value` is not a scalar → metadata, as today; (4) `validTag(key, str(value))` is false (§3.4's predicate: the key fails `^[a-zA-Z0-9_.:-]{1,32}$`, or the string form, with U+0000 removed, is empty, over 200 code points or contains `\n`) → metadata, the raw `value`, as v0.3.x stored it; (5) otherwise → `tags[key] = str(value)`. **Whichever store a rule writes, the key is removed from the other** (one key, one store, below): rule 1's `user_id` removes metadata `user_id`, rule 1's `tenant_id` and rule 5 remove the key from metadata, rules 2-4 remove it from the tags. Keys are compared exactly (case-sensitive), as their readers compare them. **Scalar** is defined on the dynamic value, so named types are covered: `reflect.ValueOf(value).Kind()` is `String`, `Bool`, `Int`, `Int8`, `Int16`, `Int32`, `Int64`, `Uint`, `Uint8`, `Uint16`, `Uint32`, `Uint64`, `Float32` or `Float64`, **or** the value implements `fmt.Stringer`. So `type Region string` (`Region("eu")`), `time.Duration` and a named `int` are scalars; `Uintptr`, `Complex*`, and every pointer, slice, map, struct, func and chan that is not a Stringer are not. **`str(value)` is `fmt.Sprint(value)`** for every scalar — which calls `String()` on a Stringer (and `Error()` on a value that is also an `error`, fmt's own precedence), prints a named string's underlying string (`Region("eu")` → `eu`) and a number's decimal form (a `rune` is its number). **A `nil` value, and a nil pointer** (`Kind() == Ptr && IsNil()`, which `fmt.Sprint` would render `<nil>`), **remove the key from both stores** (checked before rule 1): the key is deleted from the tags and from the metadata, copy-on-write, and under `user_id` the typed user's `ID` is cleared as well (its other fields kept; a user left with no field is removed). That is the last write winning, as on v0.3.x's one map: a nil carries no value worth a metadata `null`, and leaving the key alone would keep the stale earlier value on the wire. A key in neither store returns `ctx` unchanged. |
| `TagsFromContext(ctx) map[string]string` | **new**; a copy of the context's tags, `nil` if none. |
| `WithContextMap(ctx, map[string]interface{}) context.Context` | unchanged: metadata (`extra`), except that every key it writes is removed from the context's tags (one key, one store, below). A `user_id` key passed here stays in metadata and does not touch the typed user; the processor reads `user.id` first and falls back to it (T11). The place for per-request identifiers (T29, §7.1). |
| `MetadataFromContext(ctx) map[string]interface{}` | **new**; a shallow copy of the context's metadata map (what `WithContextMap` and the metadata-routed `WithTag` keys wrote), **unscrubbed**, `nil` if none. `ApplyContext` scrubs what it merges (§4.3). |
| `WithLevel(ctx, level string) context.Context` | **new**; stores `NormalizeLevel(level)`; an unknown level leaves `ctx` unchanged. Used by the middleware (panic → `fatal`) and the zerolog hook. |
| `WithFingerprint`, `WithErrorClass`, `ExtractFingerprint`, `ExtractErrorClass`, `ExtractTraceIDs`, `ExtractTraceContext` | unchanged. |

**The metadata-routed set**, audited at `dde0ea2` (T11, T27): every key any SDK or server code reads from metadata by
name. The SDK keeps this list as one unexported table in `context.go`, with each row's reader in a comment.

| Key(s) | Read by | Effect of reading it from metadata |
|---|---|---|
| `fingerprint` | SDK `event.go: NewEvent` (`ctxTags["fingerprint"]`) | sets `Event.Fingerprint` |
| `error_class` | SDK `event.go: NewEvent` (`ctxTags["error_class"]`) | overrides `Event.ErrorClass` |
| `release_version` | processor `event/event.go: (*ErrorEvent).Prepare` | the release, when the typed `release_version` is empty (regression detection) |
| `request_id`, `requestId`, `request-id` | processor `indexer/indexer.go: ExtractSearchFields` | `error_search_index.request_id` (read **only** from metadata) |
| `trace_id`, `traceId`, `trace-id`, `span_id`, `spanId`, `span-id` | processor `indexer/indexer.go: ExtractSearchFields` | the search index's trace/span ids (metadata first, typed fields as fallback) |
| `userId`, `user` | processor `indexer/indexer.go: UserIDFromMetadata` | the affected user when `user.id` is empty |
| `tenantId`, `organization_id` | processor `indexer/indexer.go: TenantIDFromMetadata` | the affected tenant when `tags.tenant_id` is absent |
| `runtime` | ingestor `enrich/enrich.go: IsClientPlatform, isTunnelOnly` | client/server classification for `javascript` events only; kept in metadata so the rule "read by name → stays in metadata" has no exception |
| `server_truncated`, `server_truncated_keys` | ingestor `validation/bounds.go: writeMarkers` | server-owned; a client value is removed or rewritten — from v0.4.0 by the transport itself, before the server sees it (§3.4 `metadata` row). Kept in metadata, as today, so it is removed rather than becoming a tag |
| `truncated`, `truncated_keys` | ingestor `validation/bounds.go: metadataExemptKeys` (`shrinkMetadata`) | removed only after every other key by the 64 KiB metadata shrink (the JS SDK's cut markers, T27); from v0.4.0 also **written by this SDK's transport** when it removes a context or `metadata` key (§3.4, "The SDK's marker"), which then replaces a caller's value. Kept in metadata so a caller's marker keeps that protection instead of becoming a tag |

Not in the set: `user_id` (→ typed `user.id`) and `tenant_id` (→ `tags.tenant_id`), which the contract moves and whose
new homes the processor reads first (T11). If any reader above gains a key, or a new reader appears, this table and the
SDK's copy must change in the same PR (B5; §11.3).

**One key, one store (last write wins).** In v0.3.x `WithTag` and `WithContextMap` wrote one map, so a second write to
a key replaced the first. v0.4.0 has two stores, tags and metadata, and a key's store depends on its value (rules 3-4)
as well as on the helper — so without a rule, `WithTag(ctx, "plan", "pro")` followed by `WithTag(ctx, "plan", <300
characters>)` would leave `tags.plan = "pro"` **and** `metadata.plan = <300 characters>`, and the event would carry a
stale value beside the new one. So every write that lands a key in one store removes that key from the other, in the
same copy-on-write step, and the **last write wins across tags and metadata** exactly as it won on v0.3.x's one map.
It applies to `WithTag` (every rule), `WithTenant` and `WithContextMap`, in both directions: `WithTag` of a valid tag
after `WithContextMap` of the same key moves it to the tags, and `WithContextMap` after `WithTag` moves it back to
metadata. A value becoming empty, over 200 code points or non-scalar is such a write (rules 3-4 route it to metadata
and the old tag is removed), and so is a `nil` (or nil pointer) value, which removes the key from both stores (and,
under `user_id`, the typed user's `ID`): `WithTag(ctx, "plan", "pro")` then `WithTag(ctx, "plan", nil)` leaves `plan`
in neither. The typed user is a third store only for `user_id`: `WithTag(ctx, "user_id", …)` removes
metadata `user_id` (the typed user is now the last write), while `WithContextMap({"user_id": …})` leaves the typed user
as it is — two different helpers, and the processor prefers `user.id` (T11); this one divergence from v0.3.x's
last-write-wins is stated in the CHANGELOG (§7.1). A parent context is never modified: a context derived before the
second write still reads the first. P1.3 (b) proves every direction.

**Why an invalid tag goes to metadata rather than being dropped** (§11.2 item 12): in v0.3.x every `WithTag` value
reached the wire in metadata. Dropping the ones that cannot be a tag would make an upgrade silently lose data a caller
already sends (`WithTag(ctx, "customer email", …)`, a 300-character reason, a multi-line value), with no signal but its
absence. Routing it to metadata keeps it, keeps it out of the tag facets it could never have joined (T29), and puts it
where the CHANGELOG already sends high-cardinality values. `tenant_id` (through `WithTenant`, or `WithTag` rule 1) is the one key that does not route:
an invalid tenant id is written to `tags` and dropped by the bounds step, because moving it to metadata `tenant_id` would
revive the key the contract removes. The 50-key cap is not routed either: which 50 survive depends on every tag the event
ends up with (options, `ctxTags`, `BeforeSend`), which `WithTag` cannot see; the CHANGELOG states it (§7.1). The cap
never removes a `tenant_id` tag: over 50 keys, the bounds step keeps `tenant_id` plus the 49 lexically smallest others
(§3.4), so a tenant set with `WithTenant` reaches the processor even when 50 other keys sort before it.

### 4.3 `ApplyContext`: the one call that makes the two paths equal

```go
// ApplyContext copies onto ev what ctx carries for Sentinel. Call it right after NewEvent and before your own
// assignments, so that what you set afterwards wins. Client.CaptureErrorContext calls exactly this.
func ApplyContext(ctx context.Context, ev *Event)
```

It acts **only when `ctx` carries a value**, so it never erases anything. In the order of the table, and with the
precedence each row states — the general rule is that **the event's existing value wins** (ApplyContext fills what is
missing), and only `Level`, and `ErrorClass` / `Fingerprint` through `WithErrorClass` / `WithFingerprint`, let the
context replace a value, each for the reason given. The metadata row's `error_class` and `fingerprint` effects never
override an option (§3.2's pre-options record).

**It assigns; it never writes through.** `ApplyContext` sets fields of the `*Event` it is given — that is its job — but
never writes into a map, slice or pointer that event holds: every row that adds to a map or a struct builds a new one
(a copy of the event's plus what the row adds) and assigns it, and a row that adds nothing leaves the field as it was.
The event's maps may be a caller's, shared with other events (a custom `EventOption` assigning one package-level
default `Tags` map, or one `*UserInfo` for a service account, to every event); writing into them would race another
capture and leak one context's values into every event holding the same map (P1.10). The cost is one copy of the
event's top-level tags and metadata per capture that adds to them.

| From `ctx` | Onto `ev` | Who wins |
|---|---|---|
| metadata (`WithContextMap`, metadata-routed `WithTag` keys; `MetadataFromContext`) | `ScrubPII` of the context's map; each of its keys `Metadata` does **not** already carry is added to a **new** map — a copy of `Metadata`'s top level (empty if nil) — which is then assigned to `Metadata`, only when at least one key is added; the event's existing map is never written. For each key it adds, the same effect `NewEvent` gives that key in `ctxTags` (T27), read from the unscrubbed value as `NewEvent` does: a non-empty string `fingerprint` sets `Fingerprint`, and a non-empty string `error_class` sets `ErrorClass`, **each only when no option set that field** (§3.2: the SDK option's flag is unset and the value still equals what `NewEvent` computed before options). The context's value replaces `NewEvent`'s default class (from `err`) — that is what the key means, and what it did through `ctxTags` in v0.3.x — but never an option's value, so an option wins on both keys exactly as it wins over `ctxTags` inside `NewEvent`. | the event, key by key; an option's `ErrorClass` / `Fingerprint` over the key's effect. On the `Client` path the context's keys are already in `Metadata` (they reached `NewEvent` through `ctxTags`), so the row adds nothing and `additionalContext` keeps winning over the context exactly as today (`client.go: CaptureErrorContext`, the merge after `getTagsMap`, ~L60-64). On the `NewEvent` path the caller's `ctxTags` play `additionalContext`'s part and win the same way. |
| level (`WithLevel`) | `Level` | **the context.** `NewEvent` always sets `error`, so an existing value cannot be told from the default; a caller that must force a level assigns it after `ApplyContext` (§8). |
| typed user (`WithUser`, `WithUserInfo`) | `User`: `nil` → a new `*UserInfo` copy; otherwise merged **field by field** — each of `ID`, `Username`, `Email`, `IPAddress` is filled from the context only where the event's field is empty (the existing `*UserInfo` is replaced by a merged copy, never written in place) | the event, field by field: a field an option set survives, and the context fills the rest. |
| client IP of the request scope (§6.4) | `User.IPAddress`, only if it is still empty after the row above (a `User` is created if needed; an existing `*UserInfo` is replaced by a copy carrying the IP, never written in place) | the event (and the context's typed user) |
| tags | each context tag whose key `Tags` does not already carry is added to a **new** map — a copy of `Tags` (empty if nil) — which is then assigned to `Tags`, only when at least one key is added; the event's existing map is never written | the event, key by key |
| breadcrumb buffer (§5) | `Breadcrumbs` = `BreadcrumbsFromContext(ctx)` — a new slice whose every crumb has its own top-level copy of `Data` (§5.1) — when `Breadcrumbs` is empty | the event |
| request scope (§6.2) | `Request` = a deep copy of the scope's snapshot (built on first use, §6.2), when `Request` is nil; `Transaction` per §6.3, when `Transaction` is empty | the event |
| valid span context | `TraceID`, `SpanID`, `TraceFlags`, when `TraceID` is empty | the event |
| `WithFingerprint` / `WithErrorClass` | `Fingerprint` / `ErrorClass` | **the context**, over `ctxTags`, options and the metadata row — what `CaptureErrorContext` does today after `NewEvent` (T5). These typed context values are the explicit, per-request override; the metadata keys of the first row are not. |

The metadata row closes the gap by which, on the `NewEvent` + `Push` path, everything the context routes to metadata
(`request_id`, `WithContextMap` keys, `fingerprint`/`error_class`/`release_version` written with `WithTag`) would never
reach the wire; P1.2 and P1.3 prove it on both paths. The trace row differs from today only in that it no longer writes
empty ids when the context has no span; on the `Client` path `NewEvent` never sets them, so the wire result is identical.
On the `Client` path `NewEvent` is called without options and sets no user, tag entry, breadcrumb, request or trace, so
"the event wins" and "the context wins" give the same result there for every row except metadata (whose precedence is
today's `Client` behaviour, stated in its row); the rule matters only to a `NewEvent` caller's options and `ctxTags`.
P1.5 (`TestApplyContextOptionWinsOverMetadataKeys`) proves option-wins on `error_class` and `fingerprint`.

`Client.CaptureErrorContext` becomes: nil check → sampling → metadata merge (unchanged) → `NewEvent(c.cfg, err,
ctxTags)` → `ApplyContext(ctx, event)` → `event = c.cfg.ApplyBeforeSend(event)`; nil → return → `c.transport.Push(event)`.
The `NewEvent` call site does not move, so `ExtractStacktrace(3)`'s skip count is unaffected.

---

## 5. Breadcrumbs on `context.Context` (TASK-02)

New file `packages/sdk-go/breadcrumbs.go`.

### 5.1 API

```go
const (
	DefaultMaxBreadcrumbs = 50  // Config.MaxBreadcrumbs' existing default (T4), and core's (T24)
	MaxBreadcrumbsLimit   = 100 // the ingestor's cap (bounds.go: MaxBreadcrumbs)
)

// WithBreadcrumbBuffer returns a context carrying a new, empty buffer holding at most max breadcrumbs
// (max <= 0 → DefaultMaxBreadcrumbs, max > MaxBreadcrumbsLimit → MaxBreadcrumbsLimit). It shadows any buffer
// ctx already carries.
func WithBreadcrumbBuffer(ctx context.Context, max int) context.Context

// AddBreadcrumb appends b to ctx's buffer, dropping the oldest when full. Without a buffer (or with a nil
// ctx) it does nothing.
func AddBreadcrumb(ctx context.Context, b Breadcrumb)

// BreadcrumbsFromContext returns a copy of ctx's buffer, oldest first, or nil when ctx has none. Each returned
// crumb has its own top-level copy of Data, so the caller (and BeforeSend) may add, replace or delete Data keys
// without touching the buffer or another event's crumbs; values nested inside Data are shared and read-only.
func BreadcrumbsFromContext(ctx context.Context) []Breadcrumb
```

`MaxBreadcrumbs` default: **50** — the value `Config` already documents and core already pins, clamped at 100.
`withDefaults` additionally clamps `Config.MaxBreadcrumbs` over 100 to 100. The field is now read by the transport
(§3.4), which trims every event's breadcrumbs to it on both paths.

### 5.2 Concurrency model

The buffer is one heap object shared by every context derived from the one `WithBreadcrumbBuffer` returned: a ring of
`max` slots behind a `sync.Mutex`. `WithBreadcrumbBuffer` allocates only the buffer's header (the mutex, `max`, a nil
ring); the ring's `max` slots are allocated **lazily, by the first `AddBreadcrumb`**, under the lock — so a request
that never adds a crumb pays nothing per slot. `AddBreadcrumb` locks, allocates the ring if it is nil, writes one slot,
unlocks; `BreadcrumbsFromContext` locks, copies the slots in order into a new slice, giving every crumb whose `Data`
is non-nil a **new top-level copy** of that map (nil ring → nil), unlocks. Without that copy every event captured from
one request would carry the ring's own `Data` maps: a `BeforeSend` deleting a key on one event while the worker
marshals another is a data race, and the deletion would reach every later event of the request (P1.10). The cost is
paid per capture only: at most `max` (≤ 100) small map copies of top-level keys; the 4 KiB `Data` bound is applied at
send time (§3.4), so an over-sized `Data` is copied and then dropped. So:
- goroutines working for the same request share one buffer and are race-free (P2.1);
- a separate `WithBreadcrumbBuffer` call — the middleware makes one per request (§6.2) — is a separate object, so
  concurrent requests never see each other's crumbs (P2.2).

`AddBreadcrumb` normalizes on the way in: a zero `Timestamp` becomes `time.Now().UTC()`, `Level` goes through
`NormalizeLevel`, an **empty** `Data` map (length 0) becomes `nil` (it is absent on the wire either way, and `nil` is
what the ingestor sees, T28), and a non-empty `Data` is **shallow-copied** so a caller reusing its top-level map cannot
race the transport. The
copy is not deep: nested maps or slices in `Data` are shared by the buffer and by every event that snapshots it, and
must not be modified after the call — by the caller or by `BeforeSend` (doc comments of `AddBreadcrumb` and
`Config.BeforeSend`; §8 guarantee 1). The size rules are
applied at send time (§3.4), not here, so a crumb is bounded identically whichever path attaches it.

### 5.3 Attaching

`ApplyContext` attaches the buffer (§4.3), so `Client.CaptureErrorContext` does, and so does a `NewEvent` caller that
calls `ApplyContext` — or it can call `BreadcrumbsFromContext(ctx)` itself (core, §8). Attaching takes a snapshot;
crumbs added after the capture belong to later events only.

---

## 6. `BeforeSend` and `HTTPMiddleware` (TASK-03)

### 6.1 `BeforeSend`

```go
// Config
	// BeforeSend, if set, runs on every event just before it is queued: return nil to drop it, or the event
	// (modified, or a different one) to send. It runs on the capturing goroutine and must be safe for concurrent use.
	BeforeSend func(*Event) *Event `json:"-"`

// ApplyBeforeSend runs c.BeforeSend on ev and returns what should be pushed, or nil to drop.
func (c *Config) ApplyBeforeSend(ev *Event) *Event
```

- nil `c.BeforeSend` → `ev`; nil `ev` → nil.
- **A panicking hook drops the event**, and the SDK logs, always (not only under `Debug`):
  `log.Printf("sentinel: Config.BeforeSend panicked; event %s dropped: %v", ev.EventID, r)`. The host does not crash
  (the panic is recovered inside `ApplyBeforeSend`). Dropped, not sent unmodified: a `BeforeSend` hook is how a caller
  scrubs data it does not want sent, and a hook that failed half-way may be the only thing standing between that data
  and the wire. `OnError` is not called: it is documented as a transport-worker callback for dropped batches.
- **Order:** on the `Client` path it runs after `NewEvent` and `ApplyContext`, i.e. after every automatic field, and
  before the transport's bounds. A `NewEvent` caller calls it after its own assignments, as §8 shows. The hook sees the
  event before client-side bounds; whatever it returns is bounded (§3.4).
- It is not called for an event the sampler discarded.
- **What it may write in place** is §8 guarantee 1's list: every map and slice marked per-event there (tags, the
  contexts and their three inner maps, metadata and its nested `map[string]interface{}` and `[]interface{}` values — a map inside a list included — the user, the request
  and its headers, the breadcrumb slice and each crumb's top-level `Data`). A value marked shared there is replaced
  (assign a new value to its key), never modified in place. The `Config.BeforeSend` doc comment says so.

### 6.2 `HTTPMiddleware`: the request scope

```go
type MiddlewareOptions struct {
	// CaptureClientIP sets user.ip_address from the connection's remote address (r.RemoteAddr). Client-IP
	// headers (X-Forwarded-For, Forwarded, X-Real-Ip, any name containing forwarded, client-ip, real-ip,
	// connection-ip, originating-ip, remote-ip, remote-addr, …, and any header whose value is only IP
	// addresses) are never read, and never recorded in request.headers whatever this is set to. Off by default.
	CaptureClientIP bool
	// MaxBreadcrumbs is the per-request buffer size; 0 → DefaultMaxBreadcrumbs.
	MaxBreadcrumbs int
}

func NewHTTPMiddleware(opts MiddlewareOptions) func(http.Handler) http.Handler

// HTTPMiddleware is NewHTTPMiddleware(MiddlewareOptions{})(next). Signature unchanged.
func HTTPMiddleware(next http.Handler) http.Handler
```

Per request, unless the request's context already carries a request scope (a nested middleware reuses the outer
scope and buffer and only adds its own `recover`), the middleware does only what every request must pay for, and
defers the rest to the first capture:

1. **Eagerly** (every request): allocate the scope — `{req *http.Request, opts, once sync.Once, snapshot
   *RequestInfo, clientIP string}` — and a breadcrumb buffer header (`WithBreadcrumbBuffer(ctx, opts.MaxBreadcrumbs)`,
   whose ring is allocated by the first `AddBreadcrumb`, §5.2); store both on the context; `r2 :=
   r.WithContext(ctx)`; `scope.req = r2`; `next.ServeHTTP(w, r2)`. No header, query or URL is read or copied here.
2. **Lazily, once, on the first capture** that reads the scope (`ApplyContext`, under `scope.once`), from `scope.req`:
   build the snapshot `RequestInfo`, and the client IP if opted in (§6.4). Every later capture of the same request
   reuses that snapshot (each event gets a deep copy of it, §4.3). A request that captures nothing never builds it.
   The snapshot therefore reflects the request as the handler chain holds it at the first capture (a header an inner
   middleware set before that capture is included); `net/http` forbids a handler to modify the `*http.Request` it was
   given, and a capture on a goroutine racing an inner middleware's own header writes is the caller's race, which the
   doc comment names alongside §6.3's.
   The snapshot:
   - `method` = `r.Method`;
   - `url` = scheme `://` `r.Host` `r.URL.EscapedPath()`, scheme `https` when `r.TLS != nil`, else `http`
     (`X-Forwarded-Proto` is not trusted); when `r.Host` is empty, the escaped path alone. No query, no fragment;
   - `query_string` = `r.URL.RawQuery` with every parameter whose unescaped name matches the SDK PII list keeping its
     name and getting the value `[FILTERED]`; order and every other byte preserved. The query is split on `&` **and on
     `;`** (the legacy separator some servers still accept, so `a=1;reset_token=X` is two parameters and the second is
     filtered; parity with the sibling sdk-js spec's §5.1 rule 2); each part keeps the separator that followed it, so the
     rejoined query differs from `RawQuery` only in the filtered values. A part's name is everything up to its first
     `=` (the whole part when it has none). The name is unescaped with `url.QueryUnescape` for matching; a name that
     does not unescape (`%zz`, a trailing `%`) is **matched on its raw form**, never skipped and never an error, so
     `api_key%zz=s` is still filtered by `api_key`. The name itself is written back as it was sent;
   - `headers` = every header except, compared case-insensitively:
     - the credential headers, always, recognised three ways on the lower-cased name: (a) the five names
       `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`; (b) **any name containing** one
       of `jwt`, `assertion`, `oidc`, `principal`, `session` — the families by which an authenticating proxy or
       platform hands the application the signed-in user's identity token or claims: Cloudflare Access
       `Cf-Access-Jwt-Assertion`, Google IAP `X-Goog-Iap-Jwt-Assertion`, AWS ALB `X-Amzn-Oidc-Data` /
       `X-Amzn-Oidc-Identity`, Azure App Service `X-Ms-Client-Principal` / `-Name` / `-Id`, Pomerium
       `X-Pomerium-Jwt-Assertion`, and session identifiers (`X-Session-Id`). A signed assertion is a
       replayable bearer credential until it expires, and its claims carry the user's email and id, so recording it would
       put on the wire what `Authorization` is withheld for. None of these names contains a string of the SDK PII list
       (`X-Amzn-Oidc-Accesstoken` does — `token` — and so did not need a pattern), and the list of such headers is open, as
       the client-IP list below is, so the families are matched by pattern, as client-IP headers are by (b) below; and (c) **any name ending in**
       `-key` — the family by which API gateways and platforms take the caller's key: Azure API Management
       `Ocp-Apim-Subscription-Key`, Azure Functions `X-Functions-Key` (neither contains a string of the SDK PII list;
       `X-Api-Key`, which (a) names, is one of the family). The cost, accepted: a header whose name merely mentions a
       session (`X-Session-Region`) is dropped too, and so are `Idempotency-Key` and `Sec-Websocket-Key`, which name no
       credential (a client's retry token, the WebSocket handshake nonce) but end in `-key`;
     - **every client-IP header, always, whatever `CaptureClientIP` says**, recognised three ways, the first two on
       the lower-cased name: (a) the seven names `X-Forwarded-For`, `Forwarded`, `X-Real-Ip`, `True-Client-Ip`,
       `Cf-Connecting-Ip`, `X-Client-Ip`, `X-Cluster-Client-Ip`; (b) **any name containing** one of `forwarded`,
       `client-ip`, `clientip`, `real-ip`, `connecting-ip`, `connection-ip`, `originating-ip`, `remote-ip`,
       `remote-addr`, `external-address`, `user-ip`, `socketip`; and (c) **by value**: a header whose value — all its
       values joined with `", "` — split on `,` gives tokens that, each trimmed of spaces, with a port removed
       (`net.SplitHostPort`, when it succeeds, keeps the host) and surrounding `[` `]` removed, are **all** non-empty
       and **all** parse with `net.ParseIP`. So `X-Custom-Peer: 198.51.100.20, [2001:db8::7]:443` is dropped
       whatever its name, while `X-Build: 1.2.3` or `X-Request-Id: 12345` (not addresses to `net.ParseIP`) is kept.
       The list of vendor headers that carry an end-user address is open (`X-Original-Forwarded-For` from
       ingress-nginx, `X-Envoy-External-Address`, `Fastly-Client-Ip`, `X-Appengine-User-Ip`, `Wl-Proxy-Client-Ip`,
       Netlify's `X-Nf-Client-Connection-Ip`, `X-Originating-Ip`, `X-Remote-Ip`, `X-Remote-Addr`, …), so a closed
       name list leaks whichever one a deployment adds; the patterns catch the families and the value rule catches a
       name no pattern foresaw. What still passes, stated rather than hidden: a header under an unforeseen name whose
       value **mixes** an address with non-address tokens (`peer=198.51.100.1;proto=h2`), and an address `net.ParseIP`
       does not accept (an IPv6 literal with a `%zone`). They carry the end user's
       address, and recording them would put on the wire exactly what the opt-in of §6.4 withholds (and, with the
       opt-in on, an untrusted second address beside the one §6.4 chose). The patterns also drop
       `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Forwarded-Port` and `X-Forwarded-Prefix`, which carry no address:
       accepted, since `url` is built from `r.Host` and the connection (below) and never trusts them;
     - every header whose name matches the SDK PII list;

     multiple values joined with `", "`, keyed by Go's canonical name; plus `Host` = `r.Host` when non-empty (Go
     removes it from `r.Header`). The value rule (c) applies to `r.Header` entries only, not to `Host`: `Host` is the
     **server's** address as the client addressed it, the one `url` already carries, never the end user's.

     **A kept header whose value is a URL gets the `query_string` rule.** These are the canonical names `Referer`,
     `Location`, `Content-Location`, `X-Original-Url`, `X-Original-Uri` and `X-Rewrite-Url`. Each of the header's
     values is parsed with `url.Parse` (absolute or relative: `X-Original-Url: /reset?token=abc` is a path and query);
     its `RawQuery` gets exactly the `query_string` rule above — a parameter whose unescaped name matches the SDK PII
     list keeps its name and gets the value `[FILTERED]`, order and every other byte preserved — its fragment is removed
     (`Fragment` and `RawFragment` cleared), and so is its userinfo (`User` nil: `user:password@` is a credential); the
     value is then written back with `(*url.URL).String()`, which writes `RawQuery` as set. A value that does not parse
     drops the **whole header**, since its query cannot be filtered. So
     `Referer: https://app.example.test/reset?token=abc&x=1#top` is recorded as
     `https://app.example.test/reset?token=[FILTERED]&x=1`. Why: a browser's `Referer` carries the previous page's full
     URL, and a password-reset or magic-link page puts its token in that query — the value `url` and `query_string`
     withhold for the current request would reach the wire through the next one's headers; `X-Original-Url` and its
     siblings (set by IIS URL Rewrite, nginx `auth_request` setups and ingress rewrites) carry the pre-rewrite path and
     query of the request itself. The others considered, and why they get nothing: `Origin` is scheme, host and port
     only — no path, query or fragment to filter (kept as is); `X-Forwarded-Uri` is already dropped by name (it contains
     `forwarded`, client-IP rule (b)); `Link` is a list of `<URI>; param` entries rather than one URL, and a request
     rarely carries it, so it is **dropped** whole rather than parsed. The list is closed, like the credential names
     (a): a URL under a name it does not foresee is recorded as sent, stated as residue;
   - **never** `data` (the body is not read) and no `env`.
3. The SDK PII list (`pii.go: defaultPIIKeys`) becomes the union of today's list and `docs/sdk-specification.md` §5's:
   it gains `api-key`, `cookie`, `credential`, `private_key`, `pwd` (T14). This also widens `ScrubPII` for metadata
   (listed in the CHANGELOG). `credentials`, `access_token`, `refresh_token` and `client_secret` are already covered by
   substrings in the list. **`ScrubPII` also becomes a deep copy through maps and lists** (round 12, T37): it returns
   a new `map[string]interface{}` for every such map and a new `[]interface{}` for every such list, at any depth and
   through either — a map inside a list, a list inside a map or a list — scrubbing the keys of each map it rebuilds.
   So a `WithContextMap({"items": []interface{}{map[string]interface{}{"k": "v"}}})` value is the event's own (§8
   guarantee 1), and `{"items": [{"password": "x"}]}` is sent with `[FILTERED]` where v0.3.x sent `x` (CHANGELOG).
   Every other value — a slice of any other element type (`[]string`, `[]map[string]interface{}`), a
   `map[string]string`, a pointer, a struct — is still copied by reference (§8's shared row): copying those needs
   reflection over caller types (§11.2 item 21). A cycle ends the copy rather than the process: `ScrubPII` keeps the
   maps and lists on its current path (a map by `reflect.ValueOf(m).UnsafePointer()`, a list by its data pointer and
   length, as `encoding/json` identifies them) and copies one it meets again **by reference**, so a self-holding
   value is returned, shared, instead of overflowing the stack (today a self-holding map already does); §3.5 step 2's
   marshal then reports the cycle and removes the entry holding it from what is sent.
4. The `http_method`, `http_url`, `http_user_agent`, `http_remote_addr` metadata keys are **no longer written**
   (contract §4.4). `User-Agent` now travels in `request.headers`, where the ingestor derives `contexts.browser` and
   `contexts.client_os` from it (T12).

Why lazy rather than eager: the middleware wraps every request of a service, and most requests never capture. Eager
capture would copy every header value and filter the query on each of them, and allocate `max` breadcrumb slots,
for nothing. The only thing eagerness would buy is a snapshot that ignores what inner middleware does to the headers,
which nothing here needs. P3.10 proves the laziness.

### 6.3 `transaction`

Resolved when `ApplyContext` runs, from the scope's `*http.Request` (`r2`):

- built with Go **1.23 or newer** (`middleware_pattern_go123.go`, `//go:build go1.23`): `r2.Pattern` when non-empty;
- built with Go 1.21 or 1.22 (`middleware_pattern_pre123.go`, `//go:build !go1.23`): no pattern;
- no pattern → `r2.URL.Path`.

`r2.Pattern` is non-empty only when `ServeMux` routed the request **with the Go 1.22+ matcher**, i.e. with
`httpmuxgo121=0`. That GODEBUG defaults from the **main module's `go` line**, not from the toolchain (T30): it is `0`
when the program's main module declares `go 1.22` or newer (core does, T24) and `1` when it declares `go 1.21` or older,
unless the program overrides it (`//go:debug httpmuxgo121=0` in its main package, or, from Go 1.23, a `godebug` line in
its `go.mod`). So a consumer whose main module declares `go` < 1.22 gets the **path** by default even when built with Go
1.26 — and its `ServeMux` does not understand method or wildcard patterns either (T30), so there is no pattern to report.

So: `NewHTTPMiddleware(…)(mux)` in a program built with Go 1.23+ whose main module declares `go 1.22`+, with
`mux.HandleFunc("GET /items/{id}", h)`, gives `transaction: "GET /items/{id}"` for an event captured in `h` or in the
middleware's `recover` (the mux writes `r2.Pattern` in place before calling `h`, T15). A per-route middleware inside the
mux sees the pattern already on `r`, which `WithContext` copies. A non-`ServeMux` router, `httpmuxgo121=1` (set, or
defaulted by a `go` < 1.22 main module), a wrapper that re-copies the request before the mux (for example
`http.TimeoutHandler`), or an older toolchain gives the path. The pattern is used verbatim (with its method when the
pattern has one); `request.method` carries the method in every case.

This module's own tests are such a consumer: `packages/sdk-go` declares `go 1.21` (T16), so its test binaries run with
`httpmuxgo121=1` unless a test file says otherwise. P3.6's go1.23 test file therefore carries
`//go:debug httpmuxgo121=0`, and every other test that goes through a middleware uses a bare handler (P1.1, P1.2, P1.7,
P2.2, P3.5, P3.7, P3.8, P3.10), whose `transaction` is the path under any toolchain and any GODEBUG.

Concurrency: the mux writes `r2.Pattern` before it calls the handler, so a capture made in the handler, or in any
goroutine the handler starts, reads it after the write. A capture on a goroutine started **before** the mux routes
(only possible from code between this middleware and the mux) would race that write; the doc comment says so. No SDK
code reads `r2` outside a capture.

### 6.4 `user.ip_address`: the opt-in

Only with `MiddlewareOptions.CaptureClientIP`: the host part of `r.RemoteAddr` (`net.SplitHostPort`; if that fails, the
value is not used), taken with the snapshot on first capture (§6.2). No client-IP header — neither the seven names, nor
any name matching §6.2's patterns, nor any header §6.2's value rule drops — is ever read, and none is ever recorded in `request.headers`, with or without the
opt-in. `ApplyContext` fills it only when the
event's user has no IP yet, so `WithUserInfo(ctx, UserInfo{IPAddress: …})` — the way a service behind a trusted proxy
sets the real client address — wins. The server does not fill it for a `go` event (T12), so without the opt-in the
event has no IP. Storing it is allowed (contract decision 1).

### 6.5 The recovered panic

As today: `recover`, convert a non-error to `fmt.Errorf("%v", rec)`, capture through the package-level
`CaptureErrorContext` (the default client), metadata `panic: true` and `panic_stacktrace: string(debug.Stack())`, answer
500. New: the capture context is `WithLevel(r2.Context(), LevelFatal)`, so the event carries `level: fatal`, `request`,
`transaction` and the request's breadcrumbs.

**This path needs an `Init`'d default client.** The package-level `CaptureErrorContext` sends through the client `Init`
installed. Without `Init`, the first package-level capture makes one from an empty `Config` (`client.go:
getDefaultClient` → `Init(Config{})`, whose `withDefaults` gives the endpoint `http://localhost:8080/ingest` and no API
key), so the recovered panic is answered 500 and **never reaches the program's project** — as today. A
program that builds its own `Client` or `Transport` and never calls `Init` still gets the scope, the breadcrumb buffer
and `request`/`transaction` on every capture **it** makes with `r.Context()`, but not the panic event; it must recover
itself and capture through its own client. The doc comments of `HTTPMiddleware` and `NewHTTPMiddleware` say so, and so
does the README. (core never calls `Init`, and uses neither middleware, T34; §8.)

---

## 7. Release (TASK-04)

### 7.1 CHANGELOG and version

`packages/sdk-go/CHANGELOG.md` gains, above `v0.3.0`, entries for the three versions core's upgrade crosses: core
production runs v0.3.1 and core `main` v0.3.2 (T24), and none of the three has an entry today (T22):
- **v0.3.1** — error class resolved by unwrapping wrapped/joined errors; `WithFingerprint`, `WithErrorClass`,
  `ExtractFingerprint`, `ExtractErrorClass` (`ac7d18c`);
- **v0.3.2** — `ClassifiedError`, `Classify`, `ErrorClass()` interface honoured by class extraction (`4c8763c`);
- **v0.4.0** — everything in §2, with:
  - a **"Moved: metadata → typed fields"** subsection listing each moved key (`user_id` → `user.id`, `tenant_id` →
    `tags.tenant_id`, other `WithTag` scalars → `tags`, `http_*` → `request`) and what stays in metadata (§4.2's
    metadata-routed set — `fingerprint`, `error_class`, `release_version`, `request_id` and the other aliases —
    `WithContextMap`, non-scalar `WithTag` values, `panic`, `panic_stacktrace`, `zerolog_level`). It carries this
    **warning**, verbatim in substance: *tags are no longer free-form metadata. Every `tags` entry is rolled up per
    issue into a tag facet (`issue_tag_values`: one row per `(issue, key, value)`, T29), shown in the tag panel and used
    by search by tag. A tag whose value differs per request — a request id, an order or session id, a timestamp —
    creates one facet row per request and makes the panel useless. Put per-request identifiers in
    `WithContextMap` (metadata); keep `WithTag` for low-cardinality values (region, plan, feature flag).* It also
    notes that `request_id`/`requestId`/`request-id` passed to `WithTag` stay in metadata for this reason and for
    search by request id. And it states what `WithTag` does with a value that cannot be a tag, and what is now
    **dropped** where v0.3.x kept it: *a `WithTag` scalar whose key fails `^[a-zA-Z0-9_.:-]{1,32}$`, or whose value is
    empty, over 200 characters or multi-line, still goes to metadata, as in v0.3.x (§4.2); a scalar is judged by its
    kind, so a named type such as `type Region string` is a tag. What is dropped client-side: an invalid entry assigned
    directly to `Event.Tags` (or by `BeforeSend`), an invalid `WithTenant` id, and, over 50 tags, every tag
    beyond the 50 kept (`tenant_id` when present, plus the lexically smallest others) — v0.3.x kept all of those in metadata. A `WithTenant`
    id is never cut by the 50-tag cap: over 50 tags the SDK keeps `tenant_id` plus the 49 lexically smallest others, so
    the affected tenant survives however many tags sort before it. Put a value you need to keep, and anything per-request, in
    `WithContextMap`;* And it states that **the last write to a key still wins**, across tags and metadata as on
    v0.3.x's one map: *a `WithTag` or `WithContextMap` write removes the key from the other store, so a tag later
    overwritten with a value that cannot be a tag (empty, over 200 characters, non-scalar), or a `WithContextMap` key
    later set with `WithTag`, reaches the wire once, with the last value. `WithTag(ctx, key, nil)` (or a nil pointer)
    removes the key from both — v0.3.x sent it as `null` in metadata; under `user_id` it also clears the user id set
    with `WithUser`. One exception: `WithContextMap({"user_id": …})` does not clear a user set with
    `WithUser`/`WithTag("user_id", …)`, and Sentinel shows `user.id` first;*
  - a **"Compatibility"** subsection: every existing function signature is unchanged, but `Event` and `Config` gain
    fields, so **unkeyed composite literals** of either (`sentinel.Event{"id", "key", …}`) stop compiling and must be
    rewritten with field names (`go vet`'s `composites` check already flags them); keyed literals and every other use
    compile unchanged;
  - a **"`transaction` and your `go` line"** note: *`transaction` is the `ServeMux` route pattern only when your
    program is built with Go 1.23+ **and** its main module's `go.mod` declares `go 1.22` or newer (or sets
    `httpmuxgo121=0` with `//go:debug`); a main module declaring `go 1.21` or older gets the request path by default,
    whatever toolchain builds it, because Go derives `httpmuxgo121` from that line (§6.3, T30);*
  - the other behaviour changes: the widened PII list, which `ScrubPII` now also applies inside lists
    (`[]interface{}`) in metadata — v0.3.x sent `{"items": [{"password": "x"}]}` unfiltered — and a metadata list is
    copied per event with every map inside it (§6.2 item 3); that invalid UTF-8 in any string is sent as U+FFFD, one
    per invalid byte, as `encoding/json` already wrote it, and every bound measures that form (a 255-byte `user.id`
    holding an invalid byte is dropped, as the server already dropped it, §3.5 step 1); per-event marshalling; that every body, a lone event
    included, is now posted to `{endpoint}/batch` as a JSON array (v0.3.x posted a lone event to `{endpoint}` as an
    object, where the ingestor caps a body at 1 MiB and retried an internal error with 500; the batch endpoint answers
    a lone event's internal error with 400, which is not retried, as it already did for every batch of two or more);
    that an event whose one-element array is over the batch endpoint's 5 MiB cap, or whose estimated size as
    protobuf is over 1,000,000 bytes (the ingestor publishes each event to NATS as protobuf, and NATS refuses a message
    over 1 MiB; the SDK estimates it from the event's JSON **without HTML escaping** — so `<`, `>` and `&` count one
    byte, as in protobuf — except that `metadata`, `contexts`, `request.data` and breadcrumb `data` are counted as
    protobuf encodes them, where a number takes 11 bytes), now has `request.headers`/`env`, then `request.data`, then `breadcrumbs` removed until
    it fits, and is dropped with `OnError` only if it still does not (v0.3.x sent it, and the server either rejected it
    with 413, taking the rest of its batch along, or accepted it and lost it at the publish; headers of multi-byte
    text alone, or breadcrumb data dense with numbers, can pass 1 MB of protobuf); that a value `encoding/json` cannot marshal (a `func`, a channel,
    `NaN`, a `time.Time` outside years 0-9999) costs only its own entry — a `request.data` or breadcrumb `data` holding
    it is removed, a `metadata` key or context holding it is removed and listed in `truncated_keys` — and the event
    is sent (v0.3.x lost the whole batch); that the same holds for a value that marshals but that the ingestor cannot
    decode — a number too large for a `float64` (a `*big.Int` of
    10^400) or nesting deeper than 10,000 levels — which v0.3.x sent and the ingestor answered with 400 for the **whole
    request**, every event batched with it lost; and that a batch is sent
    as several requests when one would pass 5 MiB or 500 events (§3.5); that the SDK no longer writes into a `Tags` map, `*UserInfo` or other value the caller put on an event —
    `ApplyContext` and the transport build new ones (§8 guarantee 1); client-side bounds, including that a
    `message` over 10,000 characters is now truncated to 10,000 where v0.3.x sent it whole and the server rejected
    the event with 400 (§3.4; grouping is unaffected, the message is not part of the fingerprint); that every other
    value the server rejected with 400 is now fixed client-side and the event sent (§3.4): an `error_class` over 255
    characters is truncated and an empty one becomes `Error`, a `fingerprint` over 64 is dropped (the server groups the
    event itself), a `release_version` over 100 is truncated, a `trace_id` or `span_id` over 64 is dropped, an invalid
    `platform` becomes `go`, an invalid `environment` is normalized (`Production` → `production`, `Prod EU` →
    `prod-eu`, empty → `development`), and a stack trace over 100 frames keeps the first 100; that `BeforeSend` may
    edit an event's own maps in place but must replace, not edit, the values §8 lists as shared; that the zerolog
    hook sends a typed level and **no longer sends `logger.Log()` (`NoLevel`) lines or `Disabled`-level lines, which
    v0.3.x captured as errors** — only `Error`, `Fatal` and `Panic` lines are captured (§3.6, D2); and that
    `request.headers` never carries a credential header — the five named ones, any header whose name matches the PII
  list, any header whose name contains `jwt`, `assertion`, `oidc`, `principal` or `session` (identity assertions
  from Cloudflare Access, Google IAP, AWS ALB, Azure App Service, Pomerium; session ids), which also removes a header
  that merely mentions a session, and any header whose name ends in `-key` (`Ocp-Apim-Subscription-Key`,
  `X-Functions-Key`), which also removes `Idempotency-Key` and `Sec-Websocket-Key` — nor a client-IP header — the seven named ones, any header whose name contains
    `forwarded`, `client-ip`, `clientip`, `real-ip`, `connecting-ip`, `connection-ip`, `originating-ip`,
    `remote-ip`, `remote-addr`, `external-address`, `user-ip` or `socketip`, which also removes
    `X-Forwarded-Host`/`-Proto`/`-Port`/`-Prefix`, and any header whose value is nothing but IP addresses (§6.2); and
    that a URL in `Referer`, `Location`, `Content-Location`, `X-Original-Url`, `X-Original-Uri` or `X-Rewrite-Url` has
    its query filtered as `query_string` is — `Referer: …/reset?token=abc&x=1` is recorded as
    `…/reset?token=[FILTERED]&x=1` — and its fragment and any `user:password@` removed, a value that is not a URL
    drops the header, and `Link` is no longer recorded (§6.2);
  - a **"`metadata` and `contexts` are bounded client-side"** note, with **the trade** stated: *the SDK now applies
    the server's own bounds to `metadata` and `contexts` before it sends them, with a 4 KiB margin: over 32 contexts,
    or over 61,440 bytes of contexts, it removes contexts in the server's order (names other than `device`, `os`,
    `app`, `runtime`, `browser`, `culture` first, largest first); over 61,440 bytes of `metadata` it removes keys in
    the server's order (largest first, the keys Sentinel routes on — `request_id`, `user_id`, `trace_id`, … — last),
    and records what it removed in `metadata.truncated` / `truncated_keys`, as the JS SDK does, replacing any value you
    put under those two keys. **The SDK keeps up to 4 KiB less of each than the server would** (the server keeps up to
    64 KiB): in exchange the server never cuts an event this SDK sent, so the SDK can size each event exactly as it
    sends it and never loses one at the NATS publish for a cut it mispredicted. It also removes U+0000 from
    `contexts`, `request.data` and breadcrumb `data` (keys and strings, at every depth) and from tag, header and env
    keys and values, resolving a key that collides once it is removed as the server would, and removes a
    `server_truncated` / `server_truncated_keys` you put in `metadata` (the server always did). `metadata`,
    `contexts`, `request.data` and breadcrumb `data` are sent as the ingestor decodes them: numbers as `float64`
    (an `int64` beyond 2^53 loses precision, as it did on the server), a struct as an object;*
  - a **"Timestamps and ids"** note: *the event and breadcrumb timestamps are sent in UTC; an event timestamp outside
    years 0-9999, which `encoding/json` cannot write (a Unix time in milliseconds passed as seconds lands in year
    58,706), is replaced with the send time — the ingestor would have clamped it to its receipt time — and such a
    breadcrumb timestamp with the event's (v0.3.x could not marshal a batch holding such an event timestamp, and lost all of it); an `event_id` that is empty,
    over 64 characters or holds a control character is replaced with a new UUID, as the ingestor would replace it;*
  - a **"`HTTPMiddleware` and `Init`"** note: the recovered-panic event is sent through the default client, so it
    reaches your project only if the program called `sentinel.Init` (§6.5).

  It gives the upgrade command in the form v0.3.0's entry uses.

`SDKVersion` is `"0.4.0"` (§3.1); P4.1 ties it to the newest heading.

### 7.2 Tag and resolution (after merge)

1. On the **merge commit** of this feature's PR on `main`: `git tag packages/sdk-go/v0.4.0 <merge sha> && git push
   origin packages/sdk-go/v0.4.0`.
2. In an empty scratch module (`go mod init example.com/probe`):
   `GOPRIVATE=github.com/tuju-ai/* GOFLAGS=-mod=mod go get github.com/tuju-ai/sentinel/packages/sdk-go@v0.4.0` must
   succeed and `go list -m github.com/tuju-ai/sentinel/packages/sdk-go` must print `… v0.4.0`; and
   `GOPROXY=direct GONOSUMDB=github.com/tuju-ai/* go list -m github.com/tuju-ai/sentinel/packages/sdk-go@v0.4.0` must
   print the same (D7).
3. A program in that module that calls `sentinel.ApplyContext` and reads `sentinel.SDKVersion` must `go build`.

---

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

---

## 9. Files touched

| File | Change |
|---|---|
| `packages/sdk-go/event.go` | §3.1 types and constants; new `Event` fields; `NewEvent` §3.2; `NormalizeLevel` §3.3 |
| `packages/sdk-go/context.go` | §4.1-§4.3: typed user, tags, level keys; `WithUserInfo`, `UserFromContext`, `TagsFromContext`, `MetadataFromContext`, `WithLevel`, `ApplyContext`; `WithUser`/`WithTenant`/`WithTag` routing and the metadata-routed key table |
| `packages/sdk-go/breadcrumbs.go` (new) | §5 |
| `packages/sdk-go/bounds.go` (new) | §3.4 `boundEvent` and its helpers (unexported): the typed rows, the two timestamp rules and the `event_id` re-mint; the **decoded** rows — U+0000 removal with the server's collision rule, the `contexts` bound and the `metadata` shrink to 61,440 bytes in the server's order, with copies of `metadataExemptKeys` (18 keys) and `wellKnownContexts` (6 names), the SDK's `truncated` / `truncated_keys` marker, the `request.data` / crumb `data` sizes and the crumb list's byte bound |
| `packages/sdk-go/config.go` | `ServerName`, `BeforeSend`, `ApplyBeforeSend`; `withDefaults` clamps `MaxBreadcrumbs` |
| `packages/sdk-go/client.go` | `CaptureErrorContext` per §4.3 |
| `packages/sdk-go/publishcost.go` (new) | §3.5 `publishCost` and its helpers (unexported): the typed-field projection encoded with `SetEscapeHTML(false)` plus 6 bytes a string-map entry, the decode mirror of the four `Struct`-typed fields (the type step 2 of the transport decodes into; `publishCost` reads it and never decodes) and the `Struct` size functions over it, as sent; the `maxPublishBytes` (1,000,000) and margin (4,096) constants. No replay of the server's bounds |
| `packages/sdk-go/transport.go` | §3.5: bound a shallow copy and marshal per event, decode each as the ingestor will (on a failure, find and remove the entry that fails — a `metadata` key or a context, recorded in the SDK's marker; `request.data` or a crumb's `data`, set to nil), run the decoded bounds and send the decoded `Struct`-typed fields, shed fields to fit a one-element array in 5 MiB and a publish cost (`publishCost`) in 1,000,000 bytes, cut bodies at 5 MiB / 500 events, post every body to `{endpoint}/batch` and account each alone; `Push` doc comment |
| `packages/sdk-go/middleware.go` | §6.2-§6.5: `MiddlewareOptions`, `NewHTTPMiddleware`, request scope; the `query_string` rule as one unexported function applied to `r.URL.RawQuery` and to the query of every URL-valued header (`Referer`, `Location`, `Content-Location`, `X-Original-Url`, `X-Original-Uri`, `X-Rewrite-Url`), `Link` dropped |
| `packages/sdk-go/middleware_pattern_go123.go`, `middleware_pattern_pre123.go` (new) | §6.3, build-constrained |
| `packages/sdk-go/pii.go` | §6.2 item 3: the widened list; `ScrubPII` as a deep copy through maps and `[]interface{}`, cycle-safe (T37) |
| `packages/sdk-go/sentinelzerolog/hook.go` | §3.6 |
| `packages/sdk-go/*_test.go`, `sentinelzerolog/hook_test.go` | §10.2 (`middleware_pattern_go123_test.go` opens with `//go:build go1.23` and `//go:debug httpmuxgo121=0`, T30); `context_test.go: TestContextHelpers` and `middleware_test.go` rewritten for the new routing (T20); `client_test.go: TestCaptureErrorContextCarriesFingerprintAndErrorClass` decodes a one-element array and `transport_test.go: TestTransportBatchFlushing` drops its bare-object branch, both because every body is now an array at `/batch` (§3.5) |
| `packages/sdk-go/testdata/wire_v2_full.golden.json`, `wire_v2_bounded.golden.json`, `wire_v2_bounded_crumbs.golden.json`, `wire_v2_bounded_numbers.golden.json`, `wire_v2_shrunk.golden.json`, `wire_v2_nul.golden.json`, `wire_v2_publish_cost.json` (new) | §10.2 P1.7-P1.8 |
| `packages/sdk-go/CHANGELOG.md`, `README.md` | §7.1; README documents §4-§6 and §8's call order |
| `tests/unit/sdk_go_wire_test.go` (new) | §10.2 P1.7-P1.8, root half, including `TestSDKGoPublishCostBoundsProto` — outside `be.md`'s listed scope, **sanctioned (D6)** |
| `Taskfile.yml` | `test-unit` gains the command `cd packages/sdk-go && GOWORK=off go test -race -count=1 ./...`, as **one new `cmds` line directly after `go test ./apps/ingestor-go/...`** (the second Go line) and before the dashboard lines — not last: the sibling sdk-js run edits the two `git …` tunnel-fixture drift lines at the end of the list (its §7.3), so a line appended there collides with its hunk; no other line of `test-unit` changes — outside `be.md`'s scope, **sanctioned (D5)** |
| `.github/workflows/ci.yml` | `go-sdk` job's `go test` step: `go test -race ./... -count=1` with `env: CGO_ENABLED: "1"` on both matrix legs; no `continue-on-error`, no fallback command — outside `be.md`'s scope, **sanctioned (D5)** |
| `tests/contract/sdk_ingestor_test.go` | `buildFullyPopulatedEvent` also calls `ApplyContext` with a context carrying a user, tags, level and breadcrumbs and a middleware scope, and `assertPayloadMatchesEvent` compares every v2 field the SDK sets (P1.7's list); `TestSDKToIngestorContract_SingleEvent` marshals the event as a one-element array, as §3.5's transport now sends it, and decodes it into `[]validation.ErrorPayload`, as `handleBatchIngest` does, instead of a bare object (supplementary, workspace mode) |
| `tests/e2e/sdk_test.go` | U8 asserts the stored occurrence's typed user (`event_user.id == "u8-user"`) and `tags.case`, and that metadata has **no** `user_id` (supplementary, needs the stack) |
| `docs/sdk-specification.md` | sdk-go behaviour only: §3's Go example keys; in §5, **one new sub-heading, `### sdk-go: headers and query parameters`, inserted directly after the last bullet of `### What the server does NOT mask`** (before §5's closing `---`), stating that sdk-go filters request headers, `query_string` and URL-valued headers by the same list (§6.2). No existing line of §5 is edited. The sibling sdk-js spec's §5 URL-filter text (its §8, "§5 gains the URL filter") is its own addition and is not written under this sub-heading; if both land at §5's end, each is a pure insertion of its own sub-heading and the second to merge rebases without editing the first |

---

## 10. Verification

### 10.1 The gate

All from the repository root of the implementing worktree; every command must pass with no test skipped (a skip in
these packages is a failure of the gate; none of the tests below calls `t.Skip` or reads an opt-in environment
variable). Every test that calls `NewTransport` sets `MaxBufferSize` (at least the events it pushes), `BatchSize` and
`BatchWait` explicitly, as core's sink does — `NewTransport` applies no defaults, and an unset `MaxBufferSize` drops
events silently (T36; "The eviction trap" below):

```bash
cd packages/sdk-go && GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test -race -count=1 ./...
cd packages/sdk-go && GOWORK=off GOTOOLCHAIN=go1.21.13 go test -race -count=1 ./...      # the 1.21 leg (T21)
GOWORK=off go build ./... && GOWORK=off go vet ./... && GOWORK=off go test -race -count=1 ./tests/unit/...
task test-unit                                                                           # the plane's unit gate; now includes the sdk-go -race run (D5)
go test -tags=contract ./tests/contract/... -count=1                                     # supplementary, workspace mode (T18)
```

Where each line is gated (D5, sanctioned and tightened):
- **`Taskfile.yml` `test-unit`** — the plane's unit gate — runs `cd packages/sdk-go && GOWORK=off go test -race
  -count=1 ./...` (added by this feature as the line directly after `go test ./apps/ingestor-go/...`, §9) alongside its
  existing `go test ./tests/unit/...`; so the SDK's race
  proofs, P1.* to P4.1, run wherever that gate runs, on the toolchain of the machine running it.
- **CI `go-sdk`** runs the first line, with `-race` added to its `go test` step by this feature, on Go 1.21 and stable,
  on push to `main` (T17; unit suites do not run on `pull_request`, operator policy).
- **CI `go-root`** runs the third line's test (without `-race`, unchanged) on push to `main`: the root half of P1.7-P1.8.

**Pre-merge proof of the CI change (required before the PR merges).** The `-race` step of `go-sdk` first runs
automatically on the push to `main` that merges it (T17), which is too late to learn that the self-hosted runner cannot
run it. So the implementing seat, after its last commit and before asking for merge:

1. dispatches the workflow on its own branch: `gh workflow run ci.yml --ref <branch>` (the only trigger that runs the
   unit suites on a branch, T32);
2. waits for it: `gh run list --workflow ci.yml --branch <branch> --event workflow_dispatch -L 1` for the run id, then
   `gh run watch <run-id> --exit-status`;
3. records in its delivery report the run id and URL, the head sha it ran on (must equal the PR head), and that the
   check runs **`go-sdk (go 1.21)`** and **`go-sdk (go stable)`** concluded `success` with their `go test` step's log
   showing `go test -race ./... -count=1` (`gh run view <run-id> --log --job <job-id> | grep -- '-race'`), and
   `go-root` `success` (the root half of P1.7-P1.8).

The dispatch also runs `integration`, `contract` and `e2e` (T32), which boot the compose stack on the shared pool; the
seat records their conclusions too (`contract` is this feature's supplementary proof) and does not re-dispatch for a
failure in those three that it can show is unrelated to this change. A later commit to the branch invalidates the
proof; the seat dispatches again.

**If the runner lacks cgo or a C compiler**, the step fails — it never passes without `-race`: the step sets
`CGO_ENABLED: "1"`, so a missing compiler is a `[build failed]` error rather than Go quietly defaulting cgo off, and
`CGO_ENABLED=0` would fail with `-race requires cgo` anyway (T21). The implementer must not answer that failure by
removing `-race`, adding `continue-on-error`, or adding a fallback `go test` without it (each is a mutation this proof
exists to catch). The missing C toolchain is a runner-provisioning dependency outside this repository: the seat reports
it to the coordinator as `BLOCKED_EXTERNAL`, naming the runner (`runner_name` from the job) and the failing leg, and the
PR does not merge until a green dispatch is recorded.

**The `go`-line trap (T30), re-checked across every test this spec prescribes.** `packages/sdk-go` declares `go 1.21`,
so every test binary runs with Go 1.21's GODEBUG defaults on every toolchain, and the 1.21 leg compiles only Go 1.21
language and API. What each prescribed test does about it:
- `ServeMux` patterns (`httpmuxgo121`): only P3.6 routes through a `ServeMux`; its go1.23 file sets
  `//go:debug httpmuxgo121=0`, its pre-1.23 file registers `"/items/"`. Every other middleware test uses a bare handler
  (§6.3's list), so `transaction` is the path whatever the GODEBUG.
- Timers (`asynctimerchan`): the transport uses `NewTicker`+`Stop` and `time.After` only (T30); no test relies on
  `Timer.Stop`/`Reset` draining a channel. Tests wait on `Flush` or on the recording server, never on timer semantics.
- `panicnil`: tests that panic (P3.4, P3.8) panic with a non-nil value.
- Loop variables: per-loop under `go 1.21`; closures and goroutines copy them (below).
- Language/API: no range over an integer or function, no `t.Context()`, `httptest.NewRequestWithContext`,
  `reflect.TypeFor` or other post-1.21 API — a compile error on the 1.21 leg of §10.1's second line, so it cannot pass
  silently.
- TLS/x509 GODEBUGs: no prescribed test uses TLS (`httptest.NewServer`, not `NewTLSServer`; `url`'s `https` case is
  unit-tested by setting `r.TLS` to a non-nil `&tls.ConnectionState{}` on a request, P3.5).

`tests/e2e` U8 is supplementary: `SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -run TestU8 -count=1` with the stack
up.

**The eviction trap (T36), re-checked across every test this spec prescribes.** `Push` silently evicts the oldest
queued event once `MaxBufferSize` events wait, so a test that counts what reached the recording server must not let
the queue fill. **Round 4's audit of this trap was wrong about the default:** it said the buffer is 100 by default,
which is true only through `Init` (`withDefaults`). `NewTransport` applies no defaults (T36, §12 F6): with
`MaxBufferSize` unset its channel is **unbuffered** and `Push` drops every event the worker is not already waiting for
(**run**: 20 of 20 dropped), `BatchSize` unset sends every event alone, and `BatchWait` unset panics the worker. So the
rules are:
- **Every test that calls `NewTransport` sets `MaxBufferSize`, `BatchSize` and `BatchWait` explicitly**, as core's
  `sink.go: Open` does (T24, T36): `MaxBufferSize` at least the number of events the test pushes (**512** in every
  such test, which pushes at most 501), `BatchSize` to what the test's body count assumes (1 where it counts one body
  per event; P1.6's batch-of-3 case **3**; P1.11 as its cases state), and `BatchWait` to **1 minute** where a case
  needs its events in one batch (so the ticker cannot split it; `Flush` drains the rest), else 1 second. These are
  P1.2, P1.3 (c)'s `NewEvent` path, P1.6, P1.7, P1.8, P1.10's `TestSDKNeverWritesCallerValues` (path (i) and its third case), P1.11 and P2.3's
  `TestTransportTrimsToConfigMaxBreadcrumbs`; every one of them builds its transport with one
  helper, `newTestTransport(t, cfg)`, which calls `t.Fatal` when `cfg.MaxBufferSize`, `BatchSize` or `BatchWait` is
  zero, and no test calls `NewTransport` directly.
- **A test through `Init` / `Client`** gets `withDefaults` (buffer 100, batch 10, wait 1 s); one that pushes **more
  than 100 events** sets `Config.MaxBufferSize` to at least the number it pushes (**512**).
- **Every** test that asserts what was delivered first calls `Flush`, then asserts
  `atomic.LoadUint64(&transport.droppedCount) == 0` (T36; every such test in `packages/sdk-go` is in package
  `sentinel`, except `sentinelzerolog/hook_test.go`, which cannot read it, pushes at most four events through `Init`
  and asserts the `OnError` half only) and that its `OnError` hook was never called, and only then counts or compares
  bodies.
- **Every body is a JSON array at `{endpoint}/batch`** (§3.5), a lone event's included: a recording server decodes
  each body as an array (`[]Event`, or `[]json.RawMessage` where the test compares bytes), and "the body" a §10.2 row
  names for one event means that event's element of the array it arrived in.

The prescribed tests that push more than 100 events are exactly four: P1.5's `TestAutomaticContextsRaceBeforeSend`
(400, `Client`), P1.10's `TestBreadcrumbDataRaceBeforeSend` (200, `Client`) and `TestSDKNeverWritesCallerValues` (200
per path, both paths), and P1.11's 501-event case (`NewTransport`); all four set `MaxBufferSize` **512**. Every other
one pushes at most 20 (P2.2). One test expects a drop, and asserts exactly it rather than zero: P1.11's
`TestShedOrder`, whose two last-resort cases, (d) of each family, each run on their own transport (`droppedCount == 1`,
`OnError` once). (P1.6's `TestUnmarshalableEntryRemovedAlone` and `TestUndecodableEntryRemovedAlone`, which expected a
drop up to round 9, now expect every event delivered, `droppedCount == 0`, §3.5 step 2.) P2.1 and P2.3's 800 and 40 are breadcrumbs on
one buffer, not events on the queue. The recording server answers `202`, so no send drops a batch — except P1.11's,
which enforces the ingestor's body caps and answers `413` over them, as the ingestor does (T38), and so must never see
such a body, and which fails the test on any element whose publish cost is over the publish limit (T39, T40); and
P1.6's decode tests' (`TestUndecodableEntryRemovedAlone`, `TestBoundEventUndecodableData`), which decode every body
as `handleBatchIngest` does and answer `400` on an error (T41), and so must never see an undecodable one.

Every test that needs an ingestor uses an `httptest.Server` that records request bodies; none needs the network.
Tests that call `Init` (the package-level default client) do not run in parallel with each other, as today.
Loop variables captured by closures are copied (`go 1.21` semantics).

### 10.2 Criterion → proof

"Mutation" is the production change that must turn the proof red; the implementer runs each once and records it.

**The golden rule — every golden, and every case of P1.11.** A test that compares a body with a committed file, or
with a committed integer, fixes every machine- or run-dependent value **itself, before `Push`**, and replaces nothing
in the body it receives. It uses P1.7's fixed `Config` (so `server_name` is `golden-host`, never `os.Hostname` or
`SENTINEL_SERVER_NAME`) and, after `NewEvent` (and `ApplyContext`, where the test calls it) and before `Push`,
assigns: `EventID` `"00000000-0000-4000-8000-000000000001"` (the events of one test take `…0001`, `…0002`, …, all
the same length); `Timestamp` `time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)`, and that same instant on every
breadcrumb it adds (a non-zero crumb timestamp is kept, §5.1); `Stacktrace`, a fixed frame list the test writes
(the bounded golden's 120 frames, P1.11's padded frames, otherwise one frame `{file: "golden.go", function:
"golden", line: 1, in_app: true}`); and, into the event's own per-event inner maps (§3.2), `contexts.runtime.version`
`"0.0.0-golden"`, `contexts.os.name` `"golden-os"`, `contexts.device.arch` `"golden-arch"`. The values are synthetic
on purpose: a golden regenerated without the assignment shows the machine's real ones, and the byte comparison
fails on the other leg. Nothing volatile is then replaced after bounding, so the body the SDK bounded and measured
is the body compared, the root half measures what the SDK measured, and every committed integer —
`wire_v2_publish_cost.json` above all — is the same on both `go-sdk` matrix legs (go1.21.13 and the current
toolchain, whose `runtime.Version()` differ in length) and on any machine (GOOS, GOARCH and hostname differ in
length too). The rule is a construction rule for the tests, not a production mutation; P1.7, P1.8 and P1.11 each
cite it.

**TASK-01**

| # | Criterion | Proof (`packages/sdk-go` unless named) | Mutation |
|---|---|---|---|
| P1.1 | Every typed field populated via `Client` | `client_rich_test.go: TestClientPathPopulatesEveryTypedField` — `Init` against a recording server; a **bare handler** (no `ServeMux`, so `transaction` is the request path on every toolchain and GODEBUG, §6.3) behind `NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: true})` adds a breadcrumb, sets `WithUserInfo` (id, username, email), `WithTenant`, `WithTag("region","eu")`, `WithTag("request_id","req-1")`, `WithContextMap({"order_ref":"o-1"})`, `WithLevel("warn")`, and calls `c.CaptureErrorContext`; the decoded body has `level: "warning"`, `sdk {sentinel.go, 0.4.0}`, `server_name`, `transaction` (the request path), `user {id, username, email, ip_address}`, `tags {tenant_id, region}`, `contexts {runtime, os, device}`, `request {method, url, query_string, headers}`, one breadcrumb | delete `ApplyContext(ctx, event)` from `CaptureErrorContext`; or delete any one row of §3.2 |
| P1.2 | Every typed field populated via `NewEvent` + `Transport.Push` | `newevent_path_test.go: TestNewEventPushPathPopulatesEveryTypedField` — the same context and assertions as P1.1 (P1.3's metadata assertions included), built exactly as §8's call order with its own `NewTransport` (no `Init`, no `Client`; `MaxBufferSize`, `BatchSize` and `BatchWait` set explicitly, §10.1); and a second event on the same path built with **nil `ctxTags`** — `NewEvent(cfg, err, nil)`, then `ApplyContext(context.Background(), ev)` (which adds no metadata key, so the map written is `NewEvent`'s own), then `cfg.ApplyBeforeSend(ev)` with a `BeforeSend` that writes `ev.Metadata["core_hook"] = "1"` — whose body carries `metadata.core_hook == "1"`; and `NewEvent(cfg, err, nil)` marshalled alone has no `metadata` key (§3.2) | delete any row of `ApplyContext` — the metadata row included; or make `NewEvent` return a nil `Tags` (the test writes `ev.Tags["core"]`); or leave `Metadata` nil for nil `ctxTags` (the hook panics, `ApplyBeforeSend` drops the second event, its body never arrives) |
| P1.3 | Metadata has no tags / `user_id`; every metadata-routed key stays in metadata **and reaches the wire on both paths** | (a) P1.1 and P1.2 both assert the body's `metadata` has none of `user_id`, `tenant_id`, `region`, `http_method`, `http_url`, `http_user_agent`, `http_remote_addr`, and has `request_id == "req-1"` and `order_ref == "o-1"`. (b) `context_test.go: TestWithTagRouting` — one case per key of §4.2's metadata-routed set, every key spelled out in the test (`fingerprint`, `error_class`, `release_version`, the nine request/trace/span ids, `userId`, `user`, `tenantId`, `organization_id`, `runtime`, `server_truncated`, `server_truncated_keys`, `truncated`, `truncated_keys`): `MetadataFromContext` has it and `TagsFromContext` does not; plus `user_id` → `UserFromContext`, `tenant_id` and a scalar → tags, a non-scalar → metadata, and `nil` or a nil `*T` Stringer on a key in neither store → `ctx` unchanged (no `<nil>` tag, no metadata entry). **Scalar kinds and string form** (§4.2): `type Region string` with `Region("eu")` → `tags["region"] == "eu"`; a named `type Shard int` → `"3"`; `time.Duration(2*time.Second)` → `"2s"` (Stringer); a struct implementing `fmt.Stringer` → its `String()`; a struct that is not a Stringer, a `[]string` and a `uintptr` → metadata. **Invalid tags route to metadata** (§4.2 rule 4), each with its boundary: a 32-character key → tags, a 33-character key → metadata; a key with a space → metadata; `""` → metadata; a 200-code-point value (multi-byte runes, so bytes ≠ code points) → tags, 201 → metadata; `"a\nb"` → metadata; `WithTag(ctx, "tenant_id", "")` → `TagsFromContext` has it and `MetadataFromContext` does not (rule 1, not rule 4; the bounds step then drops the empty tag). **One key, one store** (§4.2), each a sequence on one key starting from `WithTag(ctx1, "plan", "pro")` (in `TagsFromContext`), where `ctx2` is the second write and the assertion is that the key is in **exactly one** store with the **second** value: the value becoming `""` → metadata `""`, tags lack `plan`; becoming 201 code points → metadata, tags lack it; becoming non-scalar (`[]string{"a"}`) → metadata, tags lack it; the reverse, `WithTag(ctx1, "plan", <201 code points>)` then `WithTag(ctx2, "plan", "pro")` → tags `pro`, metadata lacks `plan`; `WithContextMap(ctx1, {"plan": "x"})` then `WithTag(ctx2, "plan", "pro")` → tags only; `WithTag(ctx1, "plan", "pro")` then `WithContextMap(ctx2, {"plan": "x"})` → metadata only; `WithTenant(ctx1, "t1")` then `WithContextMap(ctx2, {"tenant_id": "t2"})` → metadata `t2`, tags lack `tenant_id`, and the reverse → tags `t1`, metadata lacks it; `WithContextMap(ctx1, {"user_id": "m"})` then `WithTag(ctx2, "user_id", "u")` → `UserFromContext` `u`, metadata lacks `user_id`. **A nil write removes** (§4.2): `WithTag(ctx1, "plan", "pro")` then `WithTag(ctx2, "plan", nil)` → `plan` in **neither** store; the same with a nil `*T` Stringer as the second value; `WithContextMap(ctx1, {"plan": "x"})` then `WithTag(ctx2, "plan", nil)` → neither; `WithUserInfo(ctx1, UserInfo{ID: "u", Email: "e@example.test"})` then `WithTag(ctx2, "user_id", nil)` → `UserFromContext` is `{Email: "e@example.test"}` with an empty `ID`, and `WithUser(ctx1, "u")` then `WithTag(ctx2, "user_id", nil)` → `UserFromContext` reports none. In every sequence `ctx1` still reads its own first value (copy-on-write), and `NewEvent` + `ApplyContext(ctx2, ev)` puts the key in exactly one of `ev.Tags` / `ev.Metadata` (in neither after a nil write). (c) `metadata_path_test.go: TestContextMetadataReachesWireOnBothPaths`, a table over both paths (`Client.CaptureErrorContext`; `NewEvent` + `ApplyContext` + `Push`): ctx with `WithTag` of `request_id: "r1"`, `fingerprint: "fp-ctx"`, `error_class: "CtxClass"`, `release_version: "9.9.9"`, and `WithContextMap({"order_ref": "o-1", "k": "ctx", "db_password": "x"})`; call-site metadata `{"k": "call"}` (`additionalContext` on the `Client` path, `ctxTags` on the other). Both bodies must have `metadata` `request_id == "r1"`, `release_version == "9.9.9"`, `order_ref == "o-1"`, `k == "call"`, `db_password == "[FILTERED]"`, no `tags.fingerprint`/`tags.error_class`/`tags.release_version`, and top-level `fingerprint == "fp-ctx"`, `error_class == "CtxClass"`; a second case adds `WithFingerprint(ctx, "fp-typed")` and requires `fingerprint == "fp-typed"` on both paths | restore `WithUser` → `WithTag(ctx,"user_id",…)`; recognise only the built-in `string` type (the `Region` case lands in metadata); drop the nil-pointer check (the nil `*T` Stringer becomes the tag `<nil>`); ignore a nil write instead of removing (the stale `pro` stays in tags), or remove it from one store only; drop rule 4 (the 33-character key and the 201-code-point value vanish from both maps instead of reaching metadata); count bytes instead of code points (the 200-rune value goes to metadata); route `request_id` (or `fingerprint`, or `release_version`) to tags; delete `ApplyContext`'s metadata row (the `NewEvent` path loses `r1`, `o-1`, `9.9.9`); let the context win the metadata merge (`k == "ctx"`); drop the row's `fingerprint`/`error_class` effect (the `NewEvent` path's top-level values go wrong); skip `ScrubPII` in the row; drop the cross-store removal from `WithTag` (the stale `pro` stays in tags beside the new metadata value), or from `WithContextMap` (the `WithTag`-then-`WithContextMap` sequence keeps both), or edit the parent's map in place (`ctx1` reads the second value) |
| P1.4 | Level mapping | `level_test.go: TestNormalizeLevel` (table: the five, `warn`, `panic`, `trace`, case/space, unknown → `""`); `sentinelzerolog/hook_test.go: TestLevelFromZerolog` (every zerolog level incl. NoLevel, Disabled, −2); `TestLevelFromZerologIgnoresLevelValueGlobals` — sets `zerolog.LevelWarnValue = "WRN"` and `zerolog.LevelPanicValue = "boom"` (restored with `t.Cleanup`; the test does not run in parallel) and still requires `warning` and `fatal` (T31); `TestHookSendsTypedLevel` — `logger.WithLevel(zerolog.PanicLevel).Msg` → body `level: "fatal"`, `Error()` → `"error"`, `Log()` and `Warn()` → no request | map `panic` → `panic`; widen the hook back to `>= ErrorLevel`; implement `LevelFromZerolog(l)` as `NormalizeLevel(l.String())` (the globals test fails) |
| P1.5 | `server_name` precedence; automatic contexts; caller wins; context user merges field by field | `event_rich_test.go: TestServerNamePrecedence` (Config > `t.Setenv("SENTINEL_SERVER_NAME")` > `os.Hostname()`); `TestAutomaticContexts` (`runtime.name == "go"`, version without `go` prefix, `os.name == runtime.GOOS`, `device.arch == runtime.GOARCH`); `TestAutomaticContextsInnerMapsPerEvent` — `ev1 := NewEvent(…)`, then `ev1.Contexts["os"]["name"] = "mutated"`, `ev1.Contexts["os"]["extra"] = 1`, and the same kind of write into `ev1.Contexts["runtime"]` and `ev1.Contexts["device"]`; a second `NewEvent` must have `os.name == runtime.GOOS`, no `extra`, and untouched `runtime`/`device` maps; `TestAutomaticContextsRaceBeforeSend` (run under §10.1's `-race`) — a `Client` against a recording server answering `202`, with `BatchSize` 1, **`MaxBufferSize` 512** (400 events must never meet `Push`'s silent eviction at the default 100, T36) and an `OnError` that records every call, whose `BeforeSend` writes `ev.Contexts["os"]["seq"] = n` and `ev.Contexts["runtime"]["name"] = "go"` on every event, while 8 goroutines capture 50 events each, so the worker marshals one event while `BeforeSend` writes another's inner maps; the race detector must report nothing; after `Flush`, **first** `droppedCount == 0` and `OnError` never called, **then** the 400 received bodies must carry the 400 distinct `seq` values `BeforeSend` wrote (one event's write never shows in another's body); `TestCallerSetContextWins` (an option and a later assignment override `os`, `level`, `server_name`); `TestApplyContextMergesUserFieldByField` — an `EventOption` sets `ev.User = &UserInfo{Email: "opt@example.test"}`, ctx has `WithUserInfo(UserInfo{ID: "u1", Username: "n1", Email: "ctx@example.test"})`: after `ApplyContext` the event's user is `{ID: "u1", Username: "n1", Email: "opt@example.test"}`, and the option's `*UserInfo` still holds only its email (not written in place); likewise an option-set tag `region: "opt"` survives a context `region: "ctx"` while a context-only tag is added; `TestApplyContextOptionWinsOverMetadataKeys` — ctx `WithTag(ctx, "error_class", "CtxClass")` and `WithTag(ctx, "fingerprint", "fp-ctx")` (metadata-routed), `ApplyContext` after `NewEvent(cfg, errors.New("x"), nil, opts...)`, table: (i) `WithEventErrorClass("OptClass")`, `WithEventFingerprint("fp-opt")` → `OptClass`, `fp-opt`; (ii) a custom option `func(e *Event){ e.ErrorClass = "Custom"; e.Fingerprint = "fp-custom" }` → `Custom`, `fp-custom`; (iii) `WithEventErrorClass(<the class NewEvent computes for err>)` → that class, not `CtxClass` (the flag, not the value, decides); (iv) no option → `CtxClass`, `fp-ctx`; (v) no option plus `WithErrorClass(ctx, "Typed")` / `WithFingerprint(ctx, "fp-typed")` → `Typed`, `fp-typed` (the typed context values still win over options, §4.3) | swap the env/Config order; drop the prefix strip; replace `ev.User` wholesale with the context's user; merge into the existing pointer in place; let the context win the tag merge; drop the pre-options check from the metadata row (cases i-iii get `CtxClass`/`fp-ctx`); drop the SDK option's flag and compare values only (case iii gets `CtxClass`); cache the inner `runtime`/`os`/`device` maps in a package variable built once (the second `NewEvent` sees `mutated`, and the race test reports a data race) |
| P1.6 | Client-side bounds (§3.4) | `bounds_test.go: TestBoundEvent*` — one case per table row, through `Transport` and a recording server: invalid tag keys, empty/200+/newline values dropped, 51 keys → 50 smallest; `TestBoundTagsKeepsTenantID` — a context from `WithTenant(ctx, "acme")` plus 50 `WithTag` keys that all sort before `tenant_id` (`k00` … `k49`) → the wire's `tags` has exactly 50 keys, `tenant_id: "acme"` and the 49 smallest others (`k00` … `k48`; `k49` absent), and the same 51 keys without `WithTenant` → `k00` … `k49`; sensitive tag key → `[FILTERED]`, 256-byte `user.id` → absent, 255-byte kept, email/IP over bound absent, all-empty user → no `user` key, header/env caps, breadcrumb rules; `TestBoundEventMessage` — a `message` of exactly 10,000 ASCII characters is sent whole; 10,001 ASCII → the first 10,000; 10,001 code points of a 3-byte rune (`"€"`, so bytes ≠ code points) → exactly 10,000 code points, valid UTF-8, never cut inside a rune; 10,000 `"€"` → unchanged (a byte-counting bound would cut it to 3,333); the test also asserts `Fingerprint` and `ErrorClass` are the same before and after bounding; `TestBoundBreadcrumbsMeasuresIngestorShape` — `Config.MaxBreadcrumbs` 100, 200 crumbs each with only a timestamp of exactly `2026-09-26T12:00:00Z` and an 8,100-character ASCII message: exactly the **last 15** reach the wire (T28). The test also computes the count an all-`omitempty` mirror would keep and fails if it equals 15, so the case stays discriminating if a constant moves; `TestBoundBreadcrumbsEmptyDataCountsAsNull` — crumbs set **directly** on `ev.Breadcrumbs` (bypassing `AddBreadcrumb`), each with `Data: map[string]interface{}{}` and a fixed timestamp, message lengths computed by the test from the ingestor-shape mirror so that the list measures exactly **131,073** bytes with empty data counted as `null` (and at most 131,072 counted as `{}`): the oldest crumb is dropped; the same list with one message one byte shorter measures exactly **131,072**: every crumb is kept; the test asserts both sizes with the ingestor-shape formula before pushing, so it fails if its own construction drifts; `TestBoundEventPreV2Fields` — one case per pre-v2 row of §3.4, each with its boundary, through `Transport` and a recording server: `error_class` of 255 `"€"` kept, 256 → the first 255 code points, `""` → `"Error"`; `fingerprint` of 64 code points kept, 65 → no `fingerprint` key (and `error_class` unchanged); `release_version` of 100 kept, 101 → the first 100; `trace_id` of 64 kept, 65 → absent while a valid `span_id` beside it is kept, and the same with the roles swapped; `platform` `"go"` kept, `"Go"` and `""` → `"go"`; `environment` `"prod.eu-1_a"` kept, `"Production"` → `"production"`, `"Prod EU"` → `"prod-eu"`, `" staging"` → `"staging"`, `"-x"` → `"x"`, `""` → `"development"`, 60 valid characters → the first 50; `stacktrace` of 100 frames kept whole, 120 → the first 100 in order, an in-app frame with an empty `File` → `in_app: false` (a frame with a file keeps its `in_app`), `Line: 1 << 40` → `0`; each case also asserts that no other field changed and that the event pushed is unmodified (the bounds step builds new values); `TestBoundEventUnmarshalableData` — one event whose `Request.Data` is `math.NaN()` and whose one breadcrumb has `Data` `{"f": func() {}}`, pushed alone: one body, the event with its `request.method`/`url` and **no** `request.data`, the crumb with its message and **no** `data`; `OnError` never called, `droppedCount == 0`; the pushed event unchanged; `TestUnmarshalableEntryRemovedAlone` (`newTestTransport` with `MaxBufferSize` 512, **`BatchSize` 3** and `BatchWait` 1 minute, so the three events are one batch whatever the scheduling — with `BatchSize` unset each would be sent alone and the case would prove nothing, T36): a batch of 3, the second with a `func` in a `Contexts` entry `bad` beside a valid `ok` context, and a `time.Time` of year 10,000 under `Metadata["when"]` beside a valid `Metadata["k"]`: all 3 delivered in **one** `/batch` body, the second without `bad` and `when` and with `ok`, `k`, `truncated: ["contexts", "metadata"]` and `truncated_keys: ["contexts.bad", "when"]`; `OnError` never called, `droppedCount == 0`; `TestUndecodableEntryRemovedAlone` — the same transport settings, and a recording server that decodes every body exactly as `handleBatchIngest` does (`json.NewDecoder(r.Body).Decode` into a slice of the package's decode mirror, §3.5) and answers **400** on an error, as the ingestor does (T41); each case on its own transport, 3 events, the second of which carries the undecodable value beside a valid entry of the same field: (i) `Metadata` {`n`: a `*big.Int` of 10^400}; (ii) `Metadata` {`a`: 10,001 nested `[]interface{}`}, over 10,000 levels deep; (iii) `Metadata` {`a`: **9,998** nested lists} — which decodes on its own and as a bare event object, and fails only in a body, where the array and the event add two levels (T41: 9,997 is the deepest a body carries); (iv) a `Contexts` entry {`n`: the 10^400 `*big.Int`}. In each: all 3 delivered in **one** `/batch` body, no 400 answered, the second without the undecodable key or context, with the valid one, and with the SDK's marker naming it (`truncated_keys: ["n"]`, `["a"]`, `["a"]`, `["contexts.n"]`); `OnError` never called, `droppedCount == 0`. The test first asserts that `json.Marshal` of each case's event succeeds and that the decode of its one-element array fails, and that 9,997 nested lists decode in one (so the construction cannot drift into a marshal failure or a pass). `TestBoundEventUndecodableData` — one event whose `Request.Data` is {`n`: the 10^400 `*big.Int`} and whose one crumb's `Data` is the same, pushed alone to the decoding server: one body, the event with its `request.method`/`url` and **no** `request.data`, the crumb with its message and **no** `data`; `OnError` never called, `droppedCount == 0`; the pushed event unchanged; `TestBoundEventTimestamps` — spec review round 10's case: one event whose `Timestamp` is `time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)` and whose one crumb's is year 58,706 (`time.Unix(<unix millis>, 0)`, milliseconds passed as seconds), pushed alone: the event is sent, its `timestamp` between the `time.Now().UTC()` read before `Push` and after `Flush`, the crumb's equal to it; `OnError` never called, `droppedCount == 0`; and, each alone: an event timestamp in `time.FixedZone("x", 25*3600)` (an offset `MarshalJSON` refuses) → sent as the same instant in UTC; a crumb timestamp in `time.FixedZone("y", 3600)` → sent in UTC, the same instant; a zero crumb timestamp → the event's; year 9,999 kept, year −1 replaced; `TestBoundEventID` — each alone: `EventID` `""`, 65 code points, `"a\x00b"`, `"a\x7fb"` → sent as a valid UUIDv4 (version and variant nibbles checked), 64 code points and a UUID kept byte for byte; `EventID` of 1,000,000 × `e` → sent with a UUIDv4, nothing shed (the test asserts that event's publish cost, computed on its own copy with the id unreplaced, is over 1,000,000); `droppedCount == 0`; `TestBoundContexts` — decoded contexts, through `Transport`: a name of 65 code points and an empty one dropped, 64 kept; `a` {`v`: `"x"`} with `a\x00` {`v`: `"y"`} → `a` {`v`: `"x"`}, and with only `b\x00` → `b`; `""` with `"\x00"` → neither, recorded as `"contexts."` **once**; `n` {`k\x00`: `"v\x00w"`, `k`: `"clean"`, `l`: [0, `"\x00"`, {`"\x00"`: 1}]} → {`k`: `"clean"`, `l`: [0, `""`, {}]}; 33 contexts besides the SDK's three → the 4 non-well-known largest removed (ties to the smallest name), the six well-known names removed only after every other; a set the test pads to measure **exactly 61,440** bytes as sent → kept whole, and **61,441** → the largest non-well-known one removed; each removal in `truncated_keys` in name order, `truncated` holding `"contexts"`; `TestBoundMetadata` — decoded metadata: `server_truncated` / `server_truncated_keys` a caller set → absent; keys `request_id` (exempt) of 30,000 zeros, `a` of 10,000 × `s`, `b` of 10,000 zeros → `b` then `a` removed, `request_id` kept, `truncated_keys: ["b", "a"]`; two non-exempt keys of equal size → the lexically smaller removed; metadata the test pads to measure **exactly 61,440** bytes with no marker → sent whole, the caller's own `truncated: "mine"` kept, and one byte more → the shrink runs, `truncated` becomes `["metadata"]`; 200 keys of 800 bytes → `truncated_keys` lists 50 keys then one `"+N more"`, and the object as sent is at most 61,440; entries holding `json.Number("1e20")` (4 bytes as written, 21 as decoded) that the test sizes to be under 61,440 as the caller's value and over it decoded → shrunk; a string with U+0000 in `metadata` → sent with it (the server does not strip `metadata`); `TestBoundDataNul` — `request.data` {`q`: `"a\x00"`, `m\x00`: 1, `m`: 2} → {`q`: `"a"`, `m`: 2}; a crumb `data` {`k\x00`: `"x"`} → {`k`: `"x"`}; a `request.data` that measures **16,390** bytes with one U+0000 in it — `json.Marshal` writes U+0000 as the 6-byte `\u0000`, so stripping it removes 6 bytes — and **16,384** once stripped → kept, as the server keeps it (the test asserts both sizes before pushing); tag key `env\x00x` beside `envx` → `envx`'s value; a header `X-A\x00` → `X-A`; `TestBoundEventInvalidUTF8` (round 12; a recording server that decodes each body as `handleBatchIngest` does and re-measures with the ingestor's own `jsonSize` / `arraySize` rules): (i) a `user.id` of 254 ASCII bytes plus `0xff` (255 bytes) → no `user.id` key — **dropped**, because its U+FFFD form is 257 bytes, as the server measures it; a `user.id` of 252 ASCII bytes plus `0xff` (253) → sent as those 252 bytes plus U+FFFD, exactly 255 bytes, and the server keeps it; (ii) a tag value `"a\xfe\xff"` → `"a\uFFFD\uFFFD"` (two, one per byte), and header keys `X-A\xfe` (value `1`) and `X-A\xff` (value `2`) → one key `X-A\uFFFD` with value `1`; (iii) crumbs set directly on `ev.Breadcrumbs`, each `message` holding a run of two invalid bytes, lengths computed by the test so that the list measures exactly **131,072** bytes by the ingestor-shape mirror **after** replacing each invalid byte with one U+FFFD (what the server measures; the raw list would measure 6 bytes more per crumb): **every crumb is kept**, and the server's re-measure of the received list is 131,072; the same list with one more ASCII byte in the newest crumb (131,073) loses its oldest crumb. The test asserts both sizes by the formula before pushing, and that the pushed event still holds the raw bytes | truncate `fingerprint` instead of dropping it (the 65 case sends 64 code points); cap tags as the plain 50 lexically smallest (`TestBoundTagsKeepsTenantID`: `tenant_id` absent); drop an over-long `error_class` instead of truncating it, or leave `""` (the root half of P1.8 then fails protovalidate's `required`); truncate `trace_id` instead of dropping; drop `release_version` instead of truncating; count bytes in any of them (the `"€"` case); skip the `platform` or `environment` rule, or normalize a valid environment (`prod.eu-1_a` changes); keep the last 100 frames instead of the first; edit the pushed event's `Stacktrace` in place; remove the key regex check; use `>= 255`; measure crumbs on the SDK's own `Breadcrumb` (16 kept); count an empty `data` as `{}` (the 131,073 case keeps every crumb); drop while `>=` 131,072 instead of `>` (the exact case loses one); marshal the batch as a whole; drop the event for an entry that cannot be marshalled or decoded, as round 9 did, instead of removing the entry (`TestUnmarshalableEntryRemovedAlone`, `TestUndecodableEntryRemovedAlone`, `TestBoundEventUnmarshalableData`, `TestBoundEventUndecodableData`: `droppedCount` 1); **skip the decode check** (the server answers the whole 3-event body 400: all three events are lost, `droppedCount` 3, in every case of `TestUndecodableEntryRemovedAlone`; `TestBoundEventUndecodableData` loses its event); decode `raw` without its enclosing array (case (iii) is sent and its body answered 400); remove the whole `metadata` or `contexts` field instead of the failing entry (the valid `k` / `ok` is missing); **no timestamp rule** (`TestBoundEventTimestamps`: the marshal fails and the event is dropped, `droppedCount` 1); normalise the timestamps without `.UTC()` (the 25-hour offset still fails the marshal); set an out-of-range crumb timestamp to `time.Now()` instead of the event's (the crumb's differs); no `event_id` rule (the 1,000,000-byte id is counted as sent: the event is shed to (d) and dropped, `droppedCount` 1; the `"a\x00b"` id is sent as is); resolve a collision toward the stripped key (`a` gets `"y"`); record a collided or duplicate name for every removal instead of `appendLost`'s once (`"contexts."` twice); bound `contexts` or `metadata` at 65,536 instead of 61,440 (the 61,441 cases are sent whole); compare with `>=` (the exactly-61,440 cases lose an entry); measure `metadata` on the caller's value instead of the decoded one (the `1e20` case is sent unshrunk); remove an exempt key before a non-exempt one (`request_id` goes); keep a caller's `server_truncated`; strip U+0000 from `metadata`, or leave it in `contexts`, `request.data`, a crumb's `data`, a tag or a header key (each case's expected value differs; P1.8's root half then sees the server mark the field); measure `request.data` before stripping U+0000 (the 16,390-byte case is dropped); remove the `message` bound (10,001 is sent whole), or count bytes (the 10,000 `"€"` case is cut); **no UTF-8 replacement** (case (i)'s 255-byte id is sent and the server drops it; case (iii)'s 131,072 list is measured over and loses a crumb); `strings.ToValidUTF8` instead of one U+FFFD per byte (case (ii)'s value carries one U+FFFD; case (iii)'s 131,073 list is measured under and sent whole, and the server cuts it); replace after the rows instead of before (case (i)'s id is kept); resolve the header collision toward the largest key (value `2`) |
| P1.7 | The wire validates against the ingestor's `ErrorPayload` | **SDK half:** `wire_golden_test.go: TestWireGoldenFull` builds an event through §8's order and pushes it through a `Transport` to a recording server. Everything machine-dependent is fixed by the test, not scrubbed afterwards: `Config{ProjectKey: "golden-key", Environment: "golden", ReleaseVersion: "1.2.3", ServerName: "golden-host"}` (so neither `os.Hostname` nor `SENTINEL_SERVER_NAME` is read); the request is `httptest.NewRequest("GET", "http://api.example.test/items/42?id=7&token=t", nil)` with `RemoteAddr` `"192.0.2.1:1234"` and fixed `Accept`/`User-Agent`, driven through `NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: true})` around a **bare handler** (no `ServeMux`, so `transaction` is `/items/42` on every toolchain) into an `httptest.ResponseRecorder`, so no host or port of the test machine or of the recording server enters the body (the test asserts the recording server's `host:port` is absent). The remaining volatile values — `event_id`, `timestamp`, the breadcrumb's timestamp, `stacktrace`, `contexts.runtime.version`, `contexts.os.name`, `contexts.device.arch` — the test assigns itself by the golden rule above, after `NewEvent` and `ApplyContext` and before `Push`; nothing in the received body is replaced. The body must equal `testdata/wire_v2_full.golden.json` byte for byte after canonical re-marshal (`-update` rewrites it). **The fields the SDK sets, and so the golden carries, are exactly:** `level`, `sdk.name`, `sdk.version`, `server_name`, `transaction`, `user.id`, `user.username`, `user.email`, `user.ip_address`, `tags` (`tenant_id` and one more), `contexts.runtime.{name, version}`, `contexts.os.name`, `contexts.device.arch`, `request.{method, url, query_string, headers}`, `breadcrumbs[]` with `{timestamp, type, category, level, message, data}`, and the pre-v2 fields `NewEvent` sets; the golden must **not** carry `exception`, `logger`, `dist`, `source`, `user.geo`, `request.data`, `request.env`, which the SDK never sets. **Root half:** `tests/unit/sdk_go_wire_test.go: TestSDKGoWireGoldenMatchesIngestor` reads that file, decodes it with `DisallowUnknownFields` into `validation.ErrorPayload`, requires `ValidatePayload` valid, maps with `mapping.MapPayloadToEvent`, requires protovalidate to accept it, and requires each field of the list above to be non-empty on the mapped proto and each field of the never-set list to be empty | rename a JSON tag (`ip_address` → `ipAddress`): the SDK half fails until the golden is regenerated, then the root half fails; stop setting any listed field |
| P1.8 | The SDK trims to the server's limits, so the server cuts nothing, and its publish cost is never below the protobuf the ingestor publishes | `wire_golden_test.go` (same fixed `Config` and request as P1.7, `Config.MaxBreadcrumbs` 100), five events, five goldens: `TestWireGoldenBounded` pushes an event over every typed §3.4 bound — a `message` of 12,000 code points mixing ASCII and `"€"`, an `error_class` of 300 code points, a 65-character `fingerprint`, a 101-character `release_version`, a 65-character `trace_id` and `span_id`, `platform` `"Go"`, `environment` `"Golden Env"` (assigned after `NewEvent`, over the fixed `Config`'s), a fixed 120-frame `stacktrace` assigned by the test (one in-app frame with an empty `file`), 60 tags incl. invalid ones, 300-byte `user.id`, 70 headers of 5,000 runes mixing ASCII and `"€"`, 150 crumbs with 9,000-rune messages and 5,000-byte data — asserts that the bounded event's publish cost is under 1,000,000 bytes and its one-element array under 5,242,880, so §3.5 sheds nothing from it (4-byte runes in every header would shed them, T39), and compares with `testdata/wire_v2_bounded.golden.json`; `TestWireGoldenBoundedCrumbShape` pushes **200 breadcrumbs of an 8,100-character message each, timestamped exactly `2026-09-26T12:00:00Z`, with no type, category, level or data** (T28: the case where a crumb measured on the SDK's `omitempty` shape lets a 16th crumb through that the server then cuts) and compares with `testdata/wire_v2_bounded_crumbs.golden.json` (15 crumbs); `TestWireGoldenBoundedNumbers` pushes a **number-dense** event within every server bound — `metadata` {`request_id`: `"req-1"`, `samples`: 20,000 zeros}, a context `metrics` {`window`: 10,000 zeros}, `request.data` {`ids`: 8,000 zeros}, 20 crumbs of category `db`, message `q` and `data` {`rows`: 2,000 zeros} — asserts its publish cost is under 1,000,000 (~860,000, T40) while its JSON with escaping off is under 200,000 (so a JSON measure would be ~5.4 times under), and compares with `testdata/wire_v2_bounded_numbers.golden.json`; `TestWireGoldenShrunk` pushes an event the **SDK** shrinks — `metadata` {`request_id`: 30,000 zeros, `a`: 10,000 × `s`, `b`: 10,000 zeros} and, besides the SDK's three, contexts `c00`…`c32` of {`v`: 900 zeros} each and `blob` {`v`: 40,000 zeros} — and compares with `testdata/wire_v2_shrunk.golden.json`, which carries `metadata` {`request_id`, `truncated`: `["contexts", "metadata"]`, `truncated_keys`: `["contexts.blob", "contexts.c00", "contexts.c01", "contexts.c02", "contexts.c03", "b", "a"]`} (the largest context, then four equal ones broken by name to get to 32, then `metadata`'s non-exempt keys largest first: the server's order, §3.4) and 32 contexts; `TestWireGoldenNulAndMargin` pushes spec review round 10's shapes together, at the margin — contexts `a` {`v`: `"x"`} and `a\x00` {`v`: `"y"`}, `""` {`v`: 1} and `"\x00"` {`v`: 2}, `n\x00b` {`k\x00`: `"v\x00w"`, `k`: `"clean"`, `l`: [0, `"\x00"`, {`"\x00"`: 1}]} and `pad` {`w`: a list of zeros}, `metadata` {`request_id`: `"req-1"`, `samples`: a list of zeros}, `request.data` {`q`: `"a\x00"`, `m\x00`: 1, `m`: 2, `ids`: 8,000 zeros}, 5 crumbs of `data` {`rows`: 2,000 zeros, `k\x00`: `"x"`} — the two zero lists sized by the test so that `contexts` and `metadata` as sent each measure between 61,000 and 61,440 bytes (asserted), the publish cost under 1,000,000 (asserted: nothing shed) — and compares with `testdata/wire_v2_nul.golden.json`, which holds no U+0000 in `contexts`, `request.data` or crumb `data`, `a` {`v`: `"x"`}, `nb` {`k`: `"clean"`, `l`: [0, `""`, {}]}, no empty name, and `truncated: ["contexts"]`, `truncated_keys: ["contexts."]`. Each of these five tests and P1.7's `TestWireGoldenFull` also computes `publishCost` of the element it received (decoded into `Event`, re-marshalled, and its mirror decoded as §3.5 step 2 decodes it) and requires it to equal that golden's entry in `testdata/wire_v2_publish_cost.json`, a JSON object from golden file name to integer (`-update` rewrites the golden and its entry). All six tests follow the golden rule above: `event_id`, `timestamp`, every breadcrumb timestamp, the whole `stacktrace` and the `runtime`/`os`/`device` contexts are assigned by the test before `Push` (the bounded test's 120 frames included), so nothing is replaced after bounding and each committed integer holds on both `go-sdk` legs and on any machine — a value replaced afterwards, or left to `NewEvent`, of a different length (`runtime.version` `1.21.13` against `1.26.4`, `darwin` against `linux`, another hostname) would move the publish cost and the size the root half measures. **Root half:** `tests/unit: TestSDKGoBoundedWireNeedsNoServerCut` runs `ValidatePayload` (which applies the ingestor's bounds step) on **each** of the six goldens and requires `metadata` to have **no** `server_truncated` / `server_truncated_keys` and the payload to be unchanged by that step (deep-equal before/after) — the proof that the server cuts nothing in an SDK event, U+0000, collisions and `metadata` / `contexts` at the margin included — then maps each with `mapping.MapPayloadToEvent` and requires protovalidate to accept it — which is what proves the 10,000-code-point `message` bound against the real `error_event.message` rule (T33), and every other pre-v2 row of §3.4 against its rule (`error_class` incl. `required`, `fingerprint`, `release_version`, `trace_id`, `span_id`, `platform`, `environment`, `stacktrace` `max_items`, `stack_frame.file_required`; T35), since the bounds step looks at none of them. `tests/unit: TestSDKGoPublishCostBoundsProto` gates the estimate: for each of the six entries of `wire_v2_publish_cost.json` it decodes the golden into `validation.ErrorPayload`, runs `ValidatePayload`, maps with `mapping.MapPayloadToEvent`, and requires `n := len(proto.Marshal(mapped))` to satisfy **`n <= cost`** — a tolerance of **0**: the estimate is an upper bound by construction, and this test adds nothing `IngestAt` adds (no enrichment, `received_at` or `project_id`), and nothing needs deleting, since no golden draws a marker — and **`cost - n <= 16384`**, so an estimate that double-counts a `Struct` fails too (the typed fields' keys put it 187 to 5,112 bytes over on these shapes, T40) | raise any SDK bound above the server's (breadcrumb message 8192 → 8193; event `message` 10,000 → 10,001, `error_class` 255 → 256, `release_version` 100 → 101, frames 100 → 101: protovalidate rejects the bounded golden; skip the `platform`, `environment`, `fingerprint`, `trace_id`/`span_id` or in-app-file rule: likewise); measure crumbs on the SDK's `omitempty` shape (the crumb golden then has 16 crumbs, and the root half sees the server drop one and mark `breadcrumbs`); leave U+0000 in `contexts`, `request.data` or a crumb's `data`, or keep an empty context name (the nul golden: the server strips or drops it and marks `contexts`, `request.data` or `breadcrumbs.data`); bound `contexts` at 33 names, or use `bounds.go`'s order wrongly (well-known first, smallest first: the shrunk golden keeps other contexts, and its `truncated_keys` differ); in `publishCost`, cost a list element as `V(x)` instead of `ld(V(x))`, or a number as 8 bytes (the number and nul goldens: `n > cost`); count `metadata` or `contexts` at their JSON size (the nul golden: `n > cost`) |
| P1.9 | Old signatures compile; §8 surface exists | `surface_test.go` (package `sentinel_test`): `var _ func(context.Context, string) context.Context = sentinel.WithUser`, one such line per §8 row and per field (`var _ *sentinel.UserInfo = ev.User`, …), `var _ func(zerolog.Level) string = sentinelzerolog.LevelFromZerolog` (an external test package may import `sentinelzerolog` without a cycle), plus today's exported functions | rename or re-type any §8 identifier (compile error) |
| P1.10 | §8 guarantee 1: every reference listed per-event is per-event, on both paths; the SDK never writes into a value the caller owns | `event_refs_test.go` (package `sentinel`): `TestEventReferencesArePerEvent` — a table over both paths (`Client.CaptureErrorContext` with a `BeforeSend` that records the event; `NewEvent` + `ApplyContext`), two events captured from **one** context carrying `WithTag("region","eu")`, `WithUserInfo(UserInfo{ID: "u", Email: "e@example.test"})`, `WithContextMap({"order": "o", "nested": map[string]interface{}{"k": "v"}})`, a breadcrumb buffer holding one crumb with `Data {"k": 1, "drop": "x"}`, and a middleware request scope (bare handler, `CaptureClientIP: true`). On event 1 the test writes every per-event row of guarantee 1's table in place — `Tags["region"]`, `Contexts["os"]["name"]`, `Metadata["order"]`, `Metadata["nested"].(map[string]interface{})["k"]`, `User.Email`, `User.IPAddress`, `Request.Headers["Accept"]`, `Breadcrumbs[0].Message`, `delete(Breadcrumbs[0].Data, "drop")`, `Stacktrace[0].Line` (on the `Client` path inside the first capture's `BeforeSend`, so the test never touches an event after `Push`; on the `NewEvent` path on `ev1` before `ev2` is built) — then requires event 2 (on the `Client` path, the body the recording server received for it) and every reader of the context (`TagsFromContext`, `UserFromContext`, `MetadataFromContext`, `BreadcrumbsFromContext`) to show the original values, and a third event captured afterwards to carry them too. `TestBreadcrumbDataRaceBeforeSend` (run under §10.1's `-race`) — the finding's case: one request context whose buffer holds 50 crumbs, each `Data {"k": i, "drop": "x"}`; a `Client` against a recording server answering `202`, `BatchSize` 1, **`MaxBufferSize` 512**, an `OnError` that records every call, and a `BeforeSend` that takes `n` from an atomic counter, sets `ev.Tags["seq"]`, and on every crumb does `delete(c.Data, "drop")` and `c.Data["seq"] = n`; 8 goroutines capture 25 events each from that **same** context, so the worker marshals one event's crumbs while `BeforeSend` deletes keys from another's. The race detector must report nothing; after `Flush`, first `droppedCount == 0` and `OnError` never called (§10.1, T36), then the 200 bodies must each carry 50 crumbs with no `drop` and `seq` equal to that body's `tags.seq`, and `BreadcrumbsFromContext(ctx)` must still show every crumb with `drop` and without `seq`. `TestSDKNeverWritesCallerValues` (run under §10.1's `-race`) — §8 guarantee 1's rule, with caller-owned values shared by every event: a package-level `sharedTags := map[string]string{"opt": "v", "password": "p", "bad key": "x"}` (a PII key and an invalid key, so the bounds step must change it) and `sharedUser := &UserInfo{ID: <300 bytes>, Username: <200 code points>, Email: <300 characters>}` (over every `user` bound), each with a deep copy kept by the test; one context carrying `WithTag("region","eu")` and `WithUserInfo(UserInfo{IPAddress: "203.0.113.9"})` (so `ApplyContext` has a tag and a user field to add). A table over both paths, each 8 goroutines × 25 captures from that context: (i) `NewEvent(cfg, err, nil, opt)` + `ApplyContext` + `Push` through `newTestTransport` (`MaxBufferSize` 512, `BatchSize` 1), where `opt` assigns `e.Tags = sharedTags` and `e.User = sharedUser`; (ii) `Client.CaptureErrorContext` (`MaxBufferSize` 512, `BatchSize` 1) whose `BeforeSend` assigns the same two values. The race detector must report nothing; after `Flush`, first `droppedCount == 0` and `OnError` never called, then `sharedTags` and `*sharedUser` must deep-equal the test's copies (no `region`, no `[FILTERED]`, the invalid key still there, `ID` still 300 bytes), on path (i), recorded after `ApplyContext` and before `Push`, each event's `Tags` is a different map from `sharedTags` (`reflect.ValueOf(m).Pointer()`) and its `User` a different pointer from `sharedUser`, carrying the context's IP (`ApplyContext` added `region` and the IP, so it built new values); and every body must carry the bounded values — path (i): `tags` `{opt: v, password: [FILTERED], region: eu}`, `user` `{username: <128 code points>, ip_address: 203.0.113.9}`; path (ii), where `BeforeSend` replaced both after `ApplyContext`: `tags` `{opt: v, password: [FILTERED]}`, `user` `{username: <128 code points>}`; on neither an `id`, an `email` or the key `bad key`. A third case pushes one event holding `sharedUser` directly (no `ApplyContext`) and asserts, after `Flush`, that the pushed `*Event` still points at `sharedUser` and that `*sharedUser` is unchanged: the transport bounded a copy. A fourth (round 10), on path (i) with the same 8 × 25 captures, has `opt` also assign `e.Metadata = sharedMeta` (`{"big": <70,000 × "x">, "server_truncated": "c", "k": "v"}`) and `e.Contexts = sharedCtx` (`{"shared": {"k\x00": "v"}}`), each with a deep copy kept by the test: after `Flush` both deep-equal their copies, while every body carries `metadata` {`k`, `truncated`, `truncated_keys`: `["big"]`} without `big` or `server_truncated`, and `contexts.shared` {`k`: `"v"`}. Round 12: the context of `TestEventReferencesArePerEvent` also carries `WithContextMap({"items": []interface{}{map[string]interface{}{"k": "v"}}})`, and event 1's writes include `Metadata["items"].([]interface{})[0].(map[string]interface{})["k"]` and `Metadata["items"].([]interface{})[0] = "x"`, which event 2, `MetadataFromContext` and the third event must not show; `TestMetadataListRaceBeforeSend` (run under §10.1's `-race`) — one context carrying that `items` value, a `Client` against a recording server answering `202`, `BatchSize` 1, **`MaxBufferSize` 512**, an `OnError` that records every call, and a `BeforeSend` that takes `n` from an atomic counter, sets `ev.Tags["seq"]` and writes `ev.Metadata["items"].([]interface{})[0].(map[string]interface{})["k"] = n`; 8 goroutines capture 25 events each from that context. The race detector must report nothing; after `Flush`, first `droppedCount == 0` and `OnError` never called, then each of the 200 bodies carries `metadata.items[0].k` equal to its `tags.seq`, and `MetadataFromContext(ctx)` still shows `k: "v"`; `TestScrubPIIThroughLists` — `ScrubPII({"items": []interface{}{map[string]interface{}{"password": "x", "k": []interface{}{map[string]interface{}{"token": "t"}}}}, "tags": []string{"a"}})` returns `items[0].password` and `items[0].k[0].token` as `[FILTERED]`, a different `[]interface{}` and map at every level (`reflect.ValueOf(v).Pointer()`), and the same `[]string` (a typed slice is shared, §8); and a cycle — `l := []interface{}{nil}; l[0] = l` and `m := map[string]interface{}{}; m["self"] = m` — returns (no stack overflow), and an event captured with `WithContextMap({"loop": l, "self": m, "k": "v"})` is delivered with `k`, without `loop` and `self`, and with both in `truncated_keys` (§3.5 step 2) | return the ring's own `Data` maps from `BreadcrumbsFromContext` (the race detector reports; the buffer loses `drop`); copy the crumb slice but not `Data`; let `ScrubPII` stop recursing (event 2's `nested.k` changes); point `ev.User` at the context's stored user instead of a copy (event 1's `User.Email` write reaches `UserFromContext` and event 2); share the scope snapshot's `Headers` map instead of deep-copying it; build `Tags` or a `Contexts` inner map once and reuse it; let `ApplyContext` add the context's tags (or metadata keys) into the event's existing map (`sharedTags` gains `region`, and `-race` reports concurrent map writes); let the bounds step write `ev.User.ID = ""` or `ev.Tags[k] = "[FILTERED]"` through the pushed event's pointer instead of a copy (`sharedUser.ID` becomes empty, `sharedTags.password` becomes `[FILTERED]`, and `-race` reports); let it assign the bounded copies to the pushed `*Event` instead of to its own shallow copy (the third case's event no longer points at `sharedUser`); let the `metadata` shrink or the U+0000 strip edit the caller's map in place instead of the decoded copy (`sharedMeta` loses `big`, `sharedCtx`'s key is renamed, and `-race` reports); let `ScrubPII` copy a `[]interface{}` by reference, or rebuild the list but not the maps in it (event 2 and `MetadataFromContext` show event 1's `items` write; `TestMetadataListRaceBeforeSend` reports a race and a body carries another event's `seq`); stop scrubbing inside lists (`items[0].password` is `x`); drop the on-path set (the cycle case overflows the stack) |
| P1.11 | No body exceeds the ingestor's caps and no event its publish limit; an event within both is never shed (§3.5, T38, T39, T40) | `bodycap_test.go` (package `sentinel`): the recording server enforces the batch endpoint's caps exactly — `http.MaxBytesReader` of 5,242,880 bytes on `{endpoint}/batch` and more than 500 items → `413`, as `main.go: handleBatchIngest` does — records every body with its length, fails the test (`t.Errorf`) on any element whose **publish cost** — the element decoded into `Event` and measured with the package's own `publishCost`, its mirror decoded as §3.5 step 2 decodes it — is over 1,000,000 bytes (standing in for the NATS publish, which a test in this module cannot reach without a proto dependency, §2; P1.8's root half proves the estimate against `proto.Marshal`; T39, T40), and fails the test (`t.Errorf`, answering 404) on a request to any other path, `{endpoint}` included; every transport is `newTestTransport` (§10.1), and every case first computes the sizes it relies on — the one-element array with `json.Marshal`, the publish cost with `publishCost` — and fails if its own construction drifts. Every event follows the golden rule above — P1.7's fixed `Config`, and `event_id`, `timestamp`, crumb timestamps, `stacktrace` (the case's own frames) and the three SDK contexts assigned before the case sizes it — so the sizes a case computes before `Push` are the sizes sent, the figures below reproduce on both `go-sdk` legs, and the distinct fixed `event_id`s of one case give the push order the batching cases check. Every `'<'` is 6 bytes wherever a size is JSON (`json.Marshal` writes `\u003c`, and §3.4's `jsonSize` is `json.Marshal`) and 1 in the publish cost, so a `'<'` pad moves a JSON size in steps of 6: a case that must land on an **exact** JSON size pads with `'<'` and then with 0 to 5 ASCII `a` for the remainder. A case padded to reach the 5 MiB cap pads frame `function` text with `'<'` (6 bytes marshalled, 1 in the estimate; the one field neither side bounds, `apps/ingestor-go/validation/bounds.go: boundFrameV2`), so that only the 5 MiB cap binds — the test asserts each such event's publish cost is at most 1,000,000 at every step; `metadata` and `contexts` can no longer reach it, at 61,440 bytes each. A case padded to reach the publish limit pads with headers of U+1F600, frame `function` text and number-dense `request.data`, breadcrumb `data`, `metadata` or `contexts`: every one of them the ingestor publishes as sent, since the server cuts nothing in an SDK event (P1.8). `TestLoneEventOverOneMiBSentWhole` — one event whose request has **64 headers of 4,096 `'<'`** (the values alone 64 × 4,096 × 6 = 1,572,864 bytes marshalled; 1,573,569 for T38's whole event; the test asserts it is over 1,048,576, the single endpoint's cap, and that its publish cost, ~264 KB, is at most 1,000,000: each `'<'` is one byte in proto, T39), pushed alone: exactly one body reaches `{endpoint}/batch`, a one-element array holding the event with all 64 headers, nothing shed; `OnError` never called, `droppedCount == 0`. `TestLoneEventOverPublishLimitShedsHeaders` — beside it, the same event with **64 headers of 4,096 U+1F600** instead (the test asserts its publish cost is over 1,048,576 — NATS's `max_payload` itself — and its one-element array under 5,242,880: the batch endpoint would accept it and the publish would lose it, T39) plus a `request.method`/`url`, pushed alone: exactly one body reaches `{endpoint}/batch`, a one-element array holding the event **without** `request.headers` and with `method` and `url`; `OnError` never called, `droppedCount == 0`; the pushed `*Event` still holds its 64 headers. `TestFourHeaderHeavyEventsCutNotShed` — **four such events**, `BatchSize` 4, `BatchWait` 1 minute (the test asserts three in one array fit 5,242,880 bytes and four do not): a `/batch` body of the first 3 and one of the 4th, every event with its 64 headers; `droppedCount == 0`. `TestOversizedEventShedsHeaders` — one such event plus a `request.method`/`url`, a small `request.data`, one breadcrumb, a small `metadata` and 100 frames whose `function` the test pads with `'<'` so that its one-element array is over 5,242,880 bytes with the headers and at most 5,242,880 without (its publish cost at most 1,000,000 either way: ~870,000 with the headers): exactly one body reaches `{endpoint}/batch`, at most 5,242,880 bytes, with no `request.headers`, and with `request.method`, `url`, `data`, the breadcrumb, `metadata` and every frame intact; `OnError` never called, `droppedCount == 0`; the pushed `*Event` still holds its 64 headers. `TestShedOrder` — events padded by the test to land each case on one step, each case on its own transport. **5 MiB family** (frame `function` padded with `'<'`; every size the event's one-element array, and every publish cost at most 1,000,000, asserted): (a) 25 headers of 4,000 `'<'` and ~800,000 `'<'` of frame text — over 5,242,880 with the headers, at most without → only headers shed; (b) a `request.data` string of `'<'` whose JSON is near its 16,384 bytes (about 2,700 characters, 6 bytes each) and frame text such that it is still over without headers, at most without `request.data` → headers and data shed, breadcrumbs kept; (c) crumbs of `'<'` messages near the 128 KiB list bound, measured as §3.4 measures it (6 bytes a `'<'`, so a crumb of 8,192 of them is ~49 KB: two full crumbs and a shorter third fill it), and frame text such that it is still over without data, at most without breadcrumbs → all three shed; (d) frame text alone over 5,242,880 (~900,000 `'<'`, a publish cost of ~900,000) → nothing sent for it, `OnError` once with an error naming its `event_id` and the 5 MiB limit, `droppedCount == 1`; and an event with headers whose one-element array is **exactly** 5,242,880 bytes (frame text padded with `'<'`, then 0 to 5 ASCII `a`) → sent whole, nothing shed. **Publish family** (every size the event's publish cost, every one-element array under 5,242,880; no `metadata`, and no context beyond the SDK's three): fixed-size headers, `request.data` and breadcrumbs at every step that uses them — 64 headers of 1,024 U+1F600, `request.data` {`d`: 1,500 zeros}, 5 crumbs of `data` {`d`: 400 zeros} — beside a 100-frame stacktrace whose frames each carry a 2,000-byte `function` base (200,000 bytes untouched); the test pads only `frames[0]`'s `function` further, by however many bytes `solveCountForTarget` finds, to land the specific combination under test exactly over its threshold, because a request header value is capped at `sdkMaxRequestHeaderValueLen` (4,096 runes) and so cannot supply the fine, unbounded per-byte control frame text can. (a) headers alone, no `request.data`/breadcrumbs — over 1,000,000 with the headers, at most without → only headers shed; (b) headers, `request.data` and breadcrumbs together — still over 1,000,000 without headers (data and crumbs kept), at most without headers and data too → headers and data shed, breadcrumbs kept; (c) the same three together, padded instead so it is still over 1,000,000 without headers and data (breadcrumbs alone) and at most with nothing left → all three shed; (d) a separate 100-frame stacktrace of an 65,000-byte `function` each, nothing else, over 1,000,000 by itself → nothing sent for it, `OnError` once with an error naming its `event_id` and the publish limit, `droppedCount == 1`; and an event with one header and one padded frame whose publish cost is **exactly** 1,000,000 → sent whole, and one of **1,000,001** → the header shed. `TestNumberDenseCrumbsShed` — the sdk-js review's case (T40), at the SDK's margin: `metadata` {`a`: 30,000 zeros} (60,007 bytes of JSON, under 61,440, so the SDK keeps it whole; ~330,000 of protobuf) and 32 crumbs of `data` {`a`: 2,030 zeros} (§3.4 keeps 31, ~22,350 bytes of protobuf each); the test asserts its publish cost is over 1,000,000 with the crumbs (~1,023,000) and at most without (~331,000), and its JSON with escaping off under 200,000 (round 7's measure sent it whole); pushed alone: one body, the event with `metadata` whole and **no** `breadcrumbs`; `OnError` never called, `droppedCount == 0`. The same event plus a context `zz` {`a`: 30,000 zeros}: likewise, with `contexts` whole (~661,000 once the crumbs are shed). The sdk-js reviewer's own sizes, `metadata` {`a`: 32,710 zeros} (65,427 bytes) and a context of 32,600 zeros, are over 61,440 each: `TestMetadataAndContextsShrunkNotShed` covers them. `TestMetadataAndContextsShrunkNotShed` — spec review round 8's cases, which the server shrank and round 8 replayed: `metadata` {`request_id`: `"req-1"`, `debug_dump`: 1,500,000 × `x`} plus a `request.method`/`url`, 64 headers of 16 ASCII characters and one crumb; pushed alone: one body, the event with `request_id`, the headers and the crumb, **without** `debug_dump`, and with `truncated: ["metadata"]`, `truncated_keys: ["debug_dump"]`; its publish cost a few thousand bytes (asserted under 5,000); nothing shed; `OnError` never called, `droppedCount == 0`. Likewise `contexts.app` {`state`: 1,200,000 × `x`} (a well-known name, removed because nothing else is left to remove: `truncated_keys: ["contexts.app"]`), and the sdk-js reviewer's `metadata` {`a`: 32,710 zeros} beside 32 crumbs of `data` {`a`: 2,030 zeros}: `a` removed client-side, then its publish cost at most 1,000,000 with 31 crumbs (~694,000), so the crumbs are **kept**. `TestServerCutsNothing` — spec review round 10's three runs (T40), each pushed alone to the recording server, which here also runs, on the element it received, the real bound the SDK mirrors — decode into the package's decode mirror and require no U+0000 in `contexts`, `request.data` or crumb `data`, no context name empty, over 64 code points or beyond 32, and `contexts` and `metadata` each at most 61,440 bytes (`json.Marshal` of the decoded value) — standing in for P1.8's root half, which runs the ingestor's own `ValidatePayload` on the goldens and requires it to write no marker: (1) contexts `a` {`v`: `"x"`} and `a\x00` {`v`: `"y"`}, `metadata` {`m`: 32,748 zeros} (65,503 bytes), 32 headers of 4,096 U+1F600 and 100 frames of a 3,000-byte `function` → one body, `contexts.a` {`v`: `"x"`} and no `a\x00`, `m` removed (`truncated: ["contexts", "metadata"]`, `truncated_keys: ["m"]`), **every header kept**, publish cost ~830,700 at most 1,000,000 (asserted; the ingestor publishes 826,463 bytes, T40 — where the server, keeping `m`, would have published 1,186,672, over `max_payload`); (2) contexts `""` {`v`: 1} and `"\x00"` {`v`: 2} with `metadata` {`m`: a string of `x` the test sizes so that `metadata` with the SDK's marker measures exactly 61,440 bytes when `"contexts."` is recorded once} → both names absent, `truncated_keys: ["contexts."]`, and `m` **kept** (recording the name twice would push `metadata` over 61,440 and remove it); (3) `request.data` {`q`: `"a\x00"`}, `metadata` {`m`: 32,764 zeros} (65,535 bytes) and 100 frames of a 6,500-byte `function` → one body, `request.data` {`q`: `"a"`} kept, `m` removed, publish cost ~656,000 (the ingestor publishes 651,709, T40), nothing shed. In each: `OnError` never called, `droppedCount == 0`. `TestBatchCutBySize` — **5 events of exactly 1,048,576 bytes** each (marshalled; one frame's `function` padded with `'<'`, then 0 to 5 ASCII `a` for the remainder, since `'<'` alone moves the size in steps of 6; each publish cost is under 200,000), `BatchSize` 5, `BatchWait` 1 minute: a `/batch` body of the first 4 (4,194,309 bytes) and the 5th alone as a one-element array (1,048,578 bytes), also at `/batch` — five in one body would be 5,242,886 bytes, over by the separators and brackets; all 5 delivered in push order. `TestBatchCutByCount` — **501** small events, `BatchSize` 501, `MaxBufferSize` 512, `BatchWait` 1 minute: a `/batch` body of 500 and one of 1; all 501 delivered. No case reaches the server's `413` branch. | drop the 5 MiB check (`TestOversizedEventShedsHeaders` and `TestShedOrder`'s 5 MiB (a) get `413`: `OnError`, `droppedCount` 1); compare with `>=` (the exact-5 MiB event loses its headers); drop the publish-cost check (`TestLoneEventOverPublishLimitShedsHeaders` and the publish family arrive over 1,000,000: the server fails the test); encode the typed fields with `json.Marshal` or `SetEscapeHTML(true)` (`TestLoneEventOverOneMiBSentWhole` and the four-event case lose their headers: 1.5 MB marshalled, ~264 KB published); count the `Struct`-typed fields by their JSON, as round 7 did, or a list element without its `Value` wrapper (`TestNumberDenseCrumbsShed` keeps its crumbs, and the server fails the test); leave `metadata` or `contexts` unbounded, as round 9 did (`TestMetadataAndContextsShrunkNotShed`: `debug_dump` and `app` are sent and counted whole, and the event is dropped, `OnError`; `TestServerCutsNothing` (1) and (3): `m` is sent, and (1) sheds its headers — the recording server's bound check also fails); bound them at 65,536 instead of 61,440 (`TestServerCutsNothing` (1): `m`, 65,503 bytes, is kept and the headers shed); resolve no collision, or strip no U+0000, client-side (`TestServerCutsNothing` (1), (3): the recording server's check fails); record `"contexts."` twice (`TestServerCutsNothing` (2): `m` is removed); shed `metadata` or `contexts` before the crumbs (`TestNumberDenseCrumbsShed` loses `metadata` instead); compare the publish cost with `>=` (the exactly-1,000,000 event loses its headers); use 1,048,576 as the publish limit (the 1,000,001 event keeps its headers); shed at the single endpoint's 1,048,576 instead (`TestLoneEventOverOneMiBSentWhole` and the four-event case lose their headers); shed on the pushed event instead of a copy (its 64 headers are gone after `Flush`); swap steps (b) and (c) (case (b) loses its breadcrumbs); skip step (d)'s `OnError`; count a body without its commas and brackets (5 exact-MiB events in one 5,242,886-byte body: `413`); skip the 500-item cut (`413`); send a lone event to `{endpoint}` as a bare object, as v0.3.x did (the server fails the test on every one-event case) |

**TASK-02**

| # | Criterion | Proof | Mutation |
|---|---|---|---|
| P2.1 | Concurrent goroutines on one request are safe | `breadcrumbs_test.go: TestBreadcrumbsConcurrentSameContext` — 16 goroutines × 50 `AddBreadcrumb` plus 4 goroutines calling `BreadcrumbsFromContext` in a loop on one buffer of 100; `-race` clean; final length 100 | remove the mutex |
| P2.2 | Separate requests keep separate buffers | `TestBreadcrumbsSeparateRequestsThroughMiddleware` — 20 concurrent requests through one `NewHTTPMiddleware` around a bare handler, each adding crumbs tagged with its request number and capturing; every captured event carries only its own crumbs | store the buffer in a package-level variable, or reuse the incoming scope when none exists |
| P2.3 | Cap enforced, oldest dropped first | `TestBreadcrumbBufferCap` — max 3, add 5 → messages 3,4,5 in order; max 0 → 50; max 500 → 100; `TestTransportTrimsToConfigMaxBreadcrumbs` (Config 10, buffer 100, 40 crumbs → the last 10 on the wire) | drop newest instead; skip the clamp |
| P2.4 | No-op without a buffer, never a panic | `TestAddBreadcrumbWithoutBufferIsNoop` — `context.Background()`, a nil context (passed through a variable, so vet's nil-context check does not reject the test), a cancelled context: no panic, reader returns nil | delete the nil check |
| P2.5 | Attached by `CaptureErrorContext`; readable without `Client` | P1.1 and P1.2 (one crumb each on the wire) | as P1.1/P1.2 |
| P2.6 | Size limits 100 / 128 KiB / 4 KiB / 8192 | P1.6's breadcrumb rows (including the empty-`data` boundary) and P1.8 | as P1.6 and P1.8 |
| P2.7 | `AddBreadcrumb` normalises on the way in | `TestAddBreadcrumbNormalizes` — `Data: map[string]interface{}{}` → the stored crumb's `Data == nil` (not merely empty); `Data: {"k": 1}` then the caller writes `k = 2` → the stored crumb still has `1`; zero `Timestamp` → within the test's before/after `time.Now().UTC()`; `Level: "warn"` → `"warning"`, `"bogus"` → `""` | keep an empty map as is; store the caller's map instead of a copy; skip `NormalizeLevel` |

**TASK-03**

| # | Criterion | Proof | Mutation |
|---|---|---|---|
| P3.1 | `BeforeSend` modifies | `beforesend_test.go: TestBeforeSendModifies` (Client path: sets a tag; on the wire) and `TestApplyBeforeSendWithoutClient` (`NewEvent` path) | skip the hook call |
| P3.2 | `BeforeSend` drops | `TestBeforeSendNilDrops` — recording server receives nothing after `Flush` | push the original on nil |
| P3.3 | Runs after every automatic field | `TestBeforeSendSeesAutomaticFields` — the hook asserts `Level`, `SDK`, `ServerName`, `Contexts["runtime"]`, `Breadcrumbs`, `Request`, `User` are set | call the hook before `ApplyContext` |
| P3.4 | A panicking hook does not crash the host; dropped; logged | `TestBeforeSendPanicDropsAndLogs` — hook panics; the test's goroutine survives, nothing is received, `log.SetOutput` buffer contains `BeforeSend panicked` and the event id | remove the `recover`; send the original on panic |
| P3.5 | Middleware events carry `request` without sensitive, identity-assertion or client-IP headers; never the body | `middleware_test.go: TestMiddlewareRequestHeaders` — table over `CaptureClientIP` **false and true**; request with `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Auth-Token`, `X-Db-Credential`, the identity headers only the **credential name patterns** of §6.2 catch — `Cf-Access-Jwt-Assertion` and `X-Amzn-Oidc-Data` (each carrying a fake three-part JWT whose payload holds a marker email), plus one header per pattern that matches that pattern alone, `X-Jwt-Payload` (`jwt`), `X-Saml-Assertion` (`assertion`), `X-Ms-Client-Principal` (`principal`) and `X-Session-Id` (`session`), and the keys only the `-key` suffix of §6.2 (c) catches — `Ocp-Apim-Subscription-Key` and `X-Functions-Key` (each carrying a marker key value) — the seven named client-IP headers `X-Forwarded-For`, `Forwarded`, `X-Real-Ip`, `True-Client-Ip`, `Cf-Connecting-Ip`, `X-Client-Ip`, `X-Cluster-Client-Ip`, and headers that only the **name patterns** of §6.2 catch — `X-Original-Forwarded-For` (`forwarded`), `X-Envoy-External-Address` (`external-address`), `X-Appengine-User-Ip` (`user-ip`), `X-ClientIP` (`clientip`), `X-Nf-Client-Connection-Ip` (`connection-ip`), `X-Originating-Ip` (`originating-ip`), `X-Remote-Ip` (`remote-ip`), `X-Remote-Addr` (`remote-addr`) and `X-Forwarded-Host` (`forwarded`, carrying no address), headers that only the **value rule** catches — `X-Custom-Peer: 198.51.100.20, [2001:db8::7]:443` and `X-Via-Addr: 198.51.100.21:8080` — and, to be kept, `X-Build: 1.2.3` — each client-IP one sent in a non-canonical case too (e.g. `x-real-ip`, `x-original-forwarded-for`, set with a raw map write), `Accept`, `User-Agent`, and a POST body containing a marker: in both cases the wire's `request.headers` has exactly `Accept`, `User-Agent`, `X-Build`, `Host`; none of the others; neither the body marker, nor the JWTs or their marker email, nor either marker key value, nor any client-IP header's value (`198.51.100.1`, …) appears anywhere in the body; `TestMiddlewareQueryStringFiltered` (`?id=7&api_key=s&token=t` → `id=7&api_key=[FILTERED]&token=[FILTERED]`; `?a=1;reset_token=X&b=2` → exactly `a=1;reset_token=[FILTERED]&b=2`, `;` and `&` kept where they were; `?api_key%zz=s&id=7`, whose name `url.QueryUnescape` rejects → exactly `api_key%zz=[FILTERED]&id=7`); `TestMiddlewareURLHeadersFiltered` (§6.2's URL-valued headers, each value carrying its own marker): `Referer: https://app.example.test/reset?token=abc&x=1` → exactly `https://app.example.test/reset?token=[FILTERED]&x=1`; `Referer: https://u:pw-marker@app.example.test/p?x=1#frag-marker` → `https://app.example.test/p?x=1`; `X-Original-Url: /reset?password=p-marker&id=7` → `/reset?password=[FILTERED]&id=7`, and `X-Rewrite-Url`, `X-Original-Uri`, `Location` and `Content-Location` likewise, each set in a non-canonical case too; `Referer: https://app.example.test/%zz?token=bad-marker` (which `url.Parse` rejects) → no `Referer` key; `Origin: https://app.example.test` → kept unchanged; `Link: <https://cdn.example.test/a.js?token=link-marker>; rel=preload` → no `Link` key; `X-Forwarded-Uri: /reset?token=fwd-marker` → no key (client-IP rule (b)); none of `abc`, `pw-marker`, `frag-marker`, `p-marker`, `bad-marker`, `link-marker`, `fwd-marker` appears anywhere in the body; `TestMiddlewareRequestURL` — `r.TLS == nil` → `http://api.example.test/items/42`, `r.TLS = &tls.ConnectionState{}` → `https://…`, `r.Host = ""` → `/items/42`; query and fragment never in `url`; `X-Forwarded-Proto: https` on a plain request → still `http` | drop an entry from either deny list; drop the credential name-pattern check (`Cf-Access-Jwt-Assertion` and `X-Amzn-Oidc-Data` survive), or one credential pattern (`oidc`: `X-Amzn-Oidc-Data` survives; `session`: `X-Session-Id` survives); drop the `-key` suffix check (`Ocp-Apim-Subscription-Key` and `X-Functions-Key` survive); drop the client-IP name-pattern check (`X-Original-Forwarded-For` and `X-Envoy-External-Address` survive); drop one pattern (`connection-ip`: `X-Nf-Client-Connection-Ip` survives); drop the value rule (`X-Custom-Peer` survives); apply it to "any token" instead of "all tokens" or skip the port/bracket stripping (`X-Via-Addr` or the bracketed IPv6 survives); apply it without `net.ParseIP` (`X-Build` is dropped); drop them only when `CaptureClientIP` is false; compare case-sensitively; remove the PII-list check; trust `X-Forwarded-Proto`; skip the URL-header rule (`token=abc` reaches the wire), or apply it to `Referer` only (`X-Original-Url`'s `p-marker` survives); keep the fragment or the userinfo; keep a value `url.Parse` rejects (`bad-marker` survives); record `Link`; split the query on `&` only (`reset_token=X` survives); skip a parameter whose name does not unescape (`api_key%zz=s` survives) |
| P3.6 | `transaction` from the pattern (1.23+) or the path | `middleware_pattern_go123_test.go` opens with `//go:build go1.23` **and `//go:debug httpmuxgo121=0`** — without the directive the module's `go 1.21` line makes the test binary run with `httpmuxgo121=1`, `ServeMux` treats `"GET /items/{id}"` as a literal path and the handler is never reached (T30). It asserts: `ServeMux` `GET /items/{id}`, request `GET /items/42` → the handler **was reached** (a flag it sets) and `transaction: "GET /items/{id}"`, also for the panic event; a bare handler → path. `middleware_pattern_pre123_test.go` (`//go:build !go1.23`) registers **`mux.HandleFunc("/items/", h)`**, a pattern Go 1.21's matcher (the only one a 1.21/1.22 toolchain uses for a `go 1.21` module) understands, asserts the handler was reached, and requires `transaction == "/items/42"` — the path, since no pre-1.23 build reads a pattern. Exactly one file compiles on each toolchain, so neither can be skipped; the handler-reached flag makes a 404 fail the test rather than pass it vacuously. | return `""` from the go1.23 reader; return the pattern from neither; delete the `//go:debug` line (the go1.23 test fails: handler not reached); register `"GET /items/{id}"` in the pre-1.23 file (fails on the 1.21 leg: 404) |
| P3.7 | IP only on opt-in; client-IP headers neither read nor recorded | `TestMiddlewareClientIPOptIn` — default: no `user.ip_address`; opt-in: `RemoteAddr "203.0.113.7:5555"` → `203.0.113.7` with the seven named client-IP headers and the pattern-matched `X-Original-Forwarded-For: 198.51.100.9`, `X-Envoy-External-Address: 198.51.100.10`, `X-Nf-Client-Connection-Ip: 198.51.100.11` and `X-Originating-Ip: 198.51.100.12` present (`X-Forwarded-For: 198.51.100.1`, `X-Real-Ip: 198.51.100.2`, …), none of whose addresses appears as `user.ip_address` or in `request.headers`, in either mode; `WithUserInfo` IP wins | read `X-Forwarded-For`; ignore the option; record client-IP headers when the option is on |
| P3.8 | Panic event: 500, stack, `level: fatal`, `request` | `TestHTTPMiddlewarePanicRecovery` (extended): status 500; body `level: "fatal"`, `request.method`, `metadata.panic == true`, non-empty `metadata.panic_stacktrace` | drop `WithLevel(…, LevelFatal)` |
| P3.9 | PII list matches the doc | `pii_test.go: TestPIIKeysCoverSpecList` — every key of `docs/sdk-specification.md` §5's list (hard-coded in the test with a comment naming the doc) is sensitive | remove `cookie` |
| P3.10 | The middleware allocates per request only the scope; the ring and the snapshot are lazy | `middleware_lazy_test.go` (package `sentinel`, so it may read the unexported scope): `TestMiddlewareScopeIsLazy` — inside the handler, before anything else, the scope's snapshot is nil and its buffer's ring is nil; after one `AddBreadcrumb` the ring has `max` slots and the snapshot is still nil; after a capture the snapshot is built, and a second capture reuses the same snapshot (`scope.once` ran once: pointer equality). A request that neither adds a crumb nor captures leaves both nil when the handler returns | build the snapshot or the ring in the middleware before `next.ServeHTTP` |

**TASK-04**

| # | Criterion | Proof | Mutation |
|---|---|---|---|
| P4.1 | CHANGELOG entries for v0.3.1, v0.3.2, v0.4.0; version matches | `changelog_test.go: TestChangelogMatchesSDKVersion` — the first `## v` heading of `CHANGELOG.md` is `## v` + `SDKVersion`, and `## v0.3.1` and `## v0.3.2` exist | bump `SDKVersion` alone |
| P4.2 | Tag on the merge commit; `go get` resolves | §7.2's commands, run after merge and recorded in the delivery report. Not a Go test: the tag cannot exist before merge (D7). | — |

---

## 11. Decisions this spec made

### 11.1 Every "the spec fixes" of the contract

| Contract asks the spec to fix | Fixed as | Derived from |
|---|---|---|
| Exact new `Event` fields and JSON tags | §3.1 | `validator.go` types (T10); omitempty (contract §4) |
| `WithUser` struct form keeping the old signature | `WithUserInfo(ctx, UserInfo)`; `WithUser(ctx, id)` unchanged, keeps the other user fields | Compatibility (contract) |
| `MaxBreadcrumbs` default | 50, clamped to 100 | T4 and T24 (existing default and core's value); contract §4.2 "≤ 100" |
| Breadcrumb buffer API and concurrency model | §5.1-§5.2: `WithBreadcrumbBuffer` / `AddBreadcrumb` / `BreadcrumbsFromContext`; one mutex-guarded ring per buffer | contract §4.2 |
| `BeforeSend` panic handling | dropped, always logged | privacy: a scrubbing hook that failed must not leak (§6.1) |
| Transaction source on 1.21 vs 1.22+ | pattern on 1.23+ via build tag **and** only under `httpmuxgo121=0` (the default for a main module declaring `go 1.22`+), else path; the SDK's own go1.23 test sets `//go:debug httpmuxgo121=0` | T15, T30, D1 |
| IP opt-in | `MiddlewareOptions.CaptureClientIP`, remote address only | contract §4.4; T12 |
| Level mapper | `NormalizeLevel` for strings; `sentinelzerolog.LevelFromZerolog` for the `zerolog.Level` enum, which is what core uses (§8) | contract §4.1; T31 |
| Client-side bounds | §3.4, in the transport | T9; contract §4.1-§4.2 |
| Surface for core | §8 | T24; contract §4.6 |

### 11.2 Decisions the design needed

1. **`ApplyContext` is exported** rather than a set of `EventOption`s: one call gives a `NewEvent` caller everything the
   `Client` path gets, and the `Client` path calls the same function, so the two cannot drift (P1.1/P1.2 prove both).
2. **Bounds live in the transport**, the one place both paths pass through, and after `BeforeSend`.
3. **`WithTag` keeps metadata for the processor's metadata-only keys and for non-scalar values** (D9).
4. **The zerolog hook stops firing for `NoLevel`/`Disabled`** (D2): mapping a level-less line to a Sentinel level would
   be invented, and `logger.Log()` is not an error by any reading.
5. **Query parameters are filtered by the PII list** in addition to headers: today `http_url` carries the raw query into
   metadata, where key-based scrubbing never looks at it; the contract's privacy surface is the middleware's request.
6. **The PII list is widened to the documented one** (T14): the doc says SDKs must scrub those keys and names
   `Set-Cookie` and `X-API-Key` as covered.
7. **`Host` is recorded** as a header (Go strips it from `r.Header`); it is needed to read `url` and is not sensitive.
8. **Per-event marshalling** (§3.5): the v2 fields add `interface{}` places, and one bad value must not cost nine other
   events.
9. **`ApplyContext` fills; it replaces only `Level`, `Fingerprint` and `ErrorClass`** (§4.3): the event's existing
   value wins field by field (user), key by key (tags, metadata) or whole (breadcrumbs, request, transaction, trace),
   so an option or `ctxTags` value is never overwritten by the context — the same precedence `Client` gives
   `additionalContext` over the context today. The exceptions: `Level` (cannot be told apart from `NewEvent`'s
   default), and `Fingerprint` / `ErrorClass` set with `WithFingerprint` / `WithErrorClass` (today's `Client`
   behaviour, T5). A context metadata `error_class` / `fingerprint` replaces only `NewEvent`'s own value, never an
   option's (item 14).
10. **Client-IP headers are never recorded** (§6.2), whatever `CaptureClientIP` says: the opt-in is the one door by
    which an end-user address reaches the wire.
11. **The middleware is lazy** (§6.2, §5.2): the ring and the request snapshot are built on first use, so a request
    that never captures pays for a scope, a buffer header and two context values only.
12. **A `WithTag` value that cannot be a tag goes to metadata, not nowhere** (§4.2 rule 4). The alternative, dropping
    it client-side, satisfies the contract's "bad values are dropped" literally but turns an upgrade into silent data
    loss for callers whose v0.3.x `WithTag` values reached the wire in metadata. Routing keeps the value, keeps it out
    of the facets, and uses the same predicate as the bounds step, so the two cannot disagree about what a valid tag
    is. The contract's drop rule still governs `Event.Tags` itself (bounds step). Exceptions, both stated in the
    CHANGELOG: `tenant_id` (routing it would revive metadata `tenant_id`) and the 50-key cap (not decidable at
    `WithTag` time), which always keeps `tenant_id` when present (§3.4).
13. **Client-IP headers are recognised by name pattern as well as by name** (§6.2): the family of vendor headers that
    carry an end-user address is open-ended, and a closed list leaks the first one it misses. The patterns cost the
    address-free `X-Forwarded-Host`/`-Proto`/`-Port`/`-Prefix`, which the SDK never trusts anyway.
14. **An option wins over the context's metadata `error_class` / `fingerprint`** (§3.2, §4.3): detected by an SDK
    option flag or a changed value, because `NewEvent` always sets a class and an `EventOption` is opaque. The
    residue (a custom option that re-assigns the computed value) is documented, not guessed at.
15. **core maps zerolog levels from the enum, through `sentinelzerolog`** (§8): `Level.String()` is process-mutable
    (T31), and moving the enum mapper into the root package would make every `sentinel` importer compile zerolog.
16. **One key, one store: the last write wins across tags and metadata** (§4.2). v0.3.x had one map and last-write-wins
    semantics; splitting it in two by value would otherwise let a stale tag ride beside its replacement. The typed user
    is deliberately left out of `WithContextMap`'s reach (two helpers, and `user.id` is what the processor reads
    first); the CHANGELOG names that one divergence.
17. **`message` is bounded to 10,000 code points** (§3.4): over that, protovalidate rejects the whole event (T33), and
    `message` is no fingerprint input, so the cut costs no grouping; it is not NUL-stripped, since its handling is
    otherwise unchanged. (`stacktrace` was left unbounded here because its frames are fingerprint inputs; round 4 bounds it only beyond protovalidate's 100-frame cap, where no event was ever stored, item 22.)
18. **The automatic contexts' inner maps are per event; only their scalar values may be cached** (§3.2): a shared inner
    map would be written by one event's `BeforeSend` while the transport marshals another.
19. **Client-IP headers are also recognised by value** (§6.2 (c)): a header whose every token is an IP address is
    dropped whatever its name. The residue (an address mixed with other tokens under an unforeseen name) is stated in
    §6.2, not claimed away. `Host` is exempt: it is the server's address.
20. **core's `request` is core's own** (§8): the SDK's snapshot builder stays unexported, because core (gin, T34) has
    no request scope and already holds the same data redacted to its own rules, which the core contract's decision 4
    binds.
21. **Every event owns its references down to the level the SDK builds** (§8 guarantee 1): `BreadcrumbsFromContext`
    copies each crumb's `Data`, `ApplyContext` copies users and the request snapshot, `NewEvent` allocates tags and
    contexts, `ScrubPII` rebuilds metadata maps and `[]interface{}` lists, through both (round 12). What stays shared is
    named in one table instead of being copied partially and claimed whole: other reference values in metadata (typed
    slices, `map[string]string`, pointers), which the SDK cannot copy without a generic deep copy (reflection over
    caller types it does not know), and values nested in crumb `Data`, maps and lists alike, which §5.2 copies at the
    top level only so that a capture pays for at most `max` small map copies.
22. **Every pre-v2 rule protovalidate enforces is met client-side** (§3.4): a field is truncated when it is a label or
    per-process configuration (`error_class`, `release_version`), dropped when it is an identifier that must match
    something outside the event (`fingerprint`, `trace_id`, `span_id`), and replaced when the field is required and
    nothing valid can be had by cutting (`platform` → `go`, `environment` normalized, an empty class → `Error`). No
    stored event carries a value these rules change (T35), so none of them costs an existing issue its grouping.
23. **An invalid `Config.Environment` is normalized per event rather than reported by `Config.Validate`.** Adding the
    regex to `Validate` would make a service that validates its config at startup refuse to start on upgrade, where
    today it runs (and loses every event); normalizing keeps it running and makes its events arrive, under a name
    derived from the one configured, and the CHANGELOG states the rule.
24. **A nil `WithTag` value removes the key** (§4.2) rather than being ignored: ignoring it left the earlier value on
    the wire, against last-write-wins, and v0.3.x's metadata `null` carried nothing a reader can use.
25. **The SDK never writes into a map, slice or pointer it did not allocate for this event** (§8 guarantee 1, §3.5,
    §4.3). `ApplyContext` builds a new `Tags` / `Metadata` map whenever it adds a key, as its user row already built a
    new `*UserInfo`; the transport bounds a shallow copy of the event, copying `User`, `SDK` and `Request` before
    changing a field. The alternative — declaring caller-assigned values the transport's after `Push` — would make a
    shared default map, legal Go and natural in an `EventOption`, a data race the caller cannot see. The cost is one
    top-level map copy per capture that adds tags or metadata, and a struct copy per bounded pointer.
26. **Every body goes to `{endpoint}/batch`; an event whose one-element array is over 5 MiB, or whose publish cost is
    over 1,000,000 bytes, sheds whole fields,
    request first, and batches are cut by size and count** (§3.5). Posting a lone event to `{endpoint}` would make it
    fit the single endpoint's 1 MiB cap, so the SDK would shed from a lone event what the batch endpoint accepts whole
    beside others; one endpoint gives every event one cap, whatever it is batched with, as the JS SDK already does
    (T38). The cost is the batch handler's accounting for a lone event — an internal error is a 400, not a retried 500
    (§12 F9). The transport already marshals per event, so it knows each length for free. Shedding whole fields, rather than cutting
    more finely, keeps every kept value exactly as §3.4 bounded it; the order drops the least diagnostic data first and
    never the error itself. Dropping the event is the last resort, and it goes through `OnError`, unlike v0.3.x's
    413, which dropped the event's whole batch. **The second limit, the publish cost (rounds 7 and 8):** the body cap is not
    the last size an event meets — the ingestor publishes each accepted event alone to NATS, whose 1 MiB `max_payload`
    no server here raises (T39), and a bounded event of multi-byte header text passes it while fitting 5 MiB, so a
    body-only check would deliver it to a 202 and a lost publish. The SDK has no proto dependency (§2), so it **estimates** the protobuf (round 8): the typed
    fields from their JSON with HTML escaping off, which writes every string as proto does except `"`, `\` and
    control characters, where it is larger (`json.Marshal`'s 6-byte `<`, `>`, `&` would shed events NATS carries),
    plus 6 bytes a map entry; and the four `Struct`-typed fields by `struct.proto`'s encoding, because JSON is up to 5.5
    times under there (a number: 2 bytes of JSON, 11 of protobuf, T40). **What it estimates changed in round 10.**
    Rounds 8 and 9 counted `metadata` and `contexts` through a **replay** of the server's 64 KiB shrink, because the
    SDK sent both unbounded and counting them whole shed events the server shrinks and publishes; the replay had to
    predict the server's cut byte for byte — its exempt keys, its markers and the keys they record, its U+0000
    collisions and their de-duplication — and was wrong in a new way in rounds 7, 8, 9 and 10 (round 10: it recorded
    collided and duplicate context names the server never records, an under-count proven at 1,186,570 bytes
    published against ~840 KB estimated). By the coordinator's binding direction (round 10) the model is now **apply
    with headroom, then measure what is sent**: the transport applies the server's own bounds to `metadata` and
    `contexts` — its order, its exempt and well-known sets, its U+0000 and collision rules — 4,096 bytes under its
    65,536, so the server has nothing left to cut, and the publish cost is the protobuf of the event exactly as sent,
    plus a fixed allowance for what the ingestor adds. Nothing is predicted, so nothing can diverge: a rule the SDK
    mirrors imperfectly makes the server cut, which P1.8's root half catches as a marker on a golden, instead of making
    the estimate silently wrong. The margin is sized in §3.5 ("The margin"): it exceeds the most the server can still
    write into `metadata` for a bounded event, 558 bytes. **The trade:** the SDK keeps up to 4,096 bytes less of
    `metadata` and of `contexts` than the server would, and now removes entries of both itself, recording them in the
    JS SDK's `truncated` / `truncated_keys` (§3.4, §7.1). Rejected: keeping the replay with round 10's fixes (it would
    stay a byte-for-byte copy of a server algorithm, one drift from an under-count); a flat worst-case cap per field
    (~360 KB each, which drops events the server publishes); and no margin, bounding at 65,536 exactly (any marker the
    server wrote would push a full `metadata` over and start its shrink). 1,000,000 rather than 1,048,576 leaves more
    than three times the ~15 KB the ingestor adds (T39). The cost is one encode of the typed fields per event, step
    2's decode, which the decode check (decision 28) pays anyway, a second marshal of each event after its decoded
    fields are bounded, and events between 1,000,000 bytes and the true limit shed needlessly.
27. **Identity-assertion headers are dropped by name pattern** (§6.2): `jwt`, `assertion`, `oidc`, `principal`,
    `session`, beside the five named credential headers, for the reason client-IP headers are matched by pattern — the
    family is open (every authenticating proxy names its own), and a signed assertion is a bearer credential carrying
    the user's identity. The cost is a header that merely mentions a session. For the same reason a name **ending in**
    `-key` is dropped (§6.2 (c)): gateway and platform keys (`Ocp-Apim-Subscription-Key`, `X-Functions-Key`) are an
    open family the PII list does not reach; the cost is `Idempotency-Key` and `Sec-Websocket-Key`, which carry no
    credential. "Ends in", not "contains": every member of the family ends so, and a bare `key` anywhere in a name
    would reach names that carry no credential at all.
28. **Every event is decoded as the ingestor will decode it, before it is sent, and a value that fails costs only its
    entry** (§3.5 step 2). `json.Marshal` succeeding does not make a body the ingestor reads: a number beyond
    `float64` or nesting over 10,000 levels fails its one `Decode` of the whole body, and all of the body's events are
    answered 400 (T41). The check decodes each event as the one-element array, so every value sits at its body depth,
    and step 3 then bounds and sends that decoded form. On a marshal or decode error, each entry of the four
    `Struct`-typed fields is tried alone at its body depth and the failing ones are removed: `request.data` or a
    crumb's `data` set to nil, a `metadata` key or a context removed and recorded in the SDK's marker. Round 9 dropped
    the whole event for a failing `metadata` or `contexts`, because those were the caller's context, which the SDK did
    not edit; from round 10 the SDK removes entries of both anyway (§3.4), so removing a failing one is the same act,
    and the event — class, message, stack — is kept, as the server keeps an event whatever a v2 field holds (spec
    review round 10 found the same trap in `time.Time`: a timestamp outside year [0, 9999] fails `MarshalJSON`, so
    §3.4 now normalises both timestamps). Rejected: decoding with `UseNumber` (the ingestor does not), and clamping or
    stringifying the number (it would change a value the caller set rather than remove it visibly). The cost is
    nothing on the common path beyond the decode, and one small marshal and decode per entry on the rare error.
29. **A URL-valued header gets the `query_string` rule; the list is closed** (§6.2). `Referer` carries the previous
    page's URL, `X-Original-Url` and its siblings the current request's pre-rewrite one, and either can hold the
    token the `query_string` rule exists to filter. They reuse that rule rather than a new one, so a parameter is
    filtered in a header exactly when it would be in `query_string`; the fragment and userinfo, which `url` never
    carries, are removed; a value that does not parse is dropped, since its query cannot be filtered. `Origin` has no
    query; `X-Forwarded-Uri` is already dropped by name; `Link` is a list, not a URL, and is dropped rather than
    parsed. A closed list, as for credential names, because "any header whose value parses as a URL" would rewrite
    arbitrary values (`url.Parse` accepts almost any string as a relative reference); the residue — a URL under an
    unforeseen name — is stated in §6.2.

### 11.3 Notes for the spec reviewer

- §4.2's metadata-routed set is every key the SDK (`NewEvent`), the ingestor (`enrich.go`, `bounds.go: writeMarkers`) and
  the processor (`indexer.go`, `event.go: Prepare, readAffectedIDs`) read from metadata by name, audited at `dde0ea2`
  (T11, T27). If any of them ever reads a new metadata key, the list does not follow automatically. That is the same
  cross-boundary coupling B5 describes, and it is why the list is spelled out, with each key's reader, rather than
  derived; P1.3 (b) spells it out again so a key removed from the SDK's table fails a test.
- `WithTag` on an existing `user_id` or `tenant_id` key changes where the value lands but not what the processor
  computes (T11); U8 (§9) is updated because it asserted the old storage location, not the old behaviour.
- §3.4's `contexts` and `metadata` rows copy two lists and one order from `apps/ingestor-go/validation/bounds.go` —
  `metadataExemptKeys` (18 keys), `wellKnownContexts` (6 names) and `sortVictims`' order, plus `nulMap`'s U+0000 and
  collision rules and the 32-entry limit — to apply the server's bounds client-side, 4,096 bytes under its 64 KiB.
  This is B5's coupling again, taken deliberately (decision 26), but its failure mode is now loud and bounded: a
  change to that file that makes the server cut an SDK event shows up as a `server_truncated` on a golden in P1.8's
  root half, and costs at most the entries the server removes — never an estimate that is silently under, since the
  publish cost measures the event as sent (§3.5). §12 F10 records the ingestor-side fix for any event that still
  reaches NATS over `max_payload`.

---

## 12. Findings outside this scope (recorded, not fixed)

| # | Finding | Why not here |
|---|---|---|
| F1 | `ExtractStacktrace`'s `"sentinel."` filter matches no SDK function since the v0.3.0 rename (T25), so SDK frames appear in stack traces (as not-in-app when built from the module cache). | Changing frame filtering changes fingerprints for in-repo builds; not asked. |
| F2 | **Corrected in rounds 5, 6, 8 and 10.** This row used to say the bounded v2 fields "stay far below" the 1 MiB cap; they do not — bounded `request.headers` alone can marshal to 1.5 MiB (T38) — and the batch was one body whatever its size, so one such event cost its whole batch a 413. Round 5 shed fields to fit the single endpoint's 1 MiB, which removed from a lone event what the batch endpoint accepts; round 6 posts every body to `{endpoint}/batch` and sheds `request.headers`/`env`, `request.data` and `breadcrumbs` only when an event's one-element array is over 5 MiB, and cuts bodies at 5 MiB / 500 events; round 7 added the publish limit (T39); rounds 8 and 9 counted `metadata` and `contexts` through a replay of the server's shrink; round 10 bounds both client-side at 61,440 bytes and measures the event as sent (§3.5). What remains is only this. An event is dropped by the SDK, alone and through `OnError` (§3.5 step 4 (d)), when after (a)-(c) its one-element array is still over 5 MiB, or its publish cost is still over 1,000,000 — both possible only through frame `file`/`function` text (or a megabyte `project_key`), since everything else is bounded: `metadata` and `contexts` are at most ~676 KB of estimate together, and an event without frame text at most ~860 KB once shed (§3.5); v0.3.x sent such an event and the server rejected it with 413, taking the rest of its batch along, or accepted it and lost it at the NATS publish once its protobuf passed 1,048,576, and the SDK now also drops one between 1,000,000 and that, which NATS would have carried (the allowance's cost, §3.5). | Frame fields are F7's. Not asked by the contract. |
| F3 | A zerolog `Fatal` line is captured, then zerolog calls `os.Exit`, so the queued event is never sent. | Pre-existing; a flush on fatal would stop the transport for good (`Flush` cancels it). |
| F4 | `HTTPMiddleware` recovers `http.ErrAbortHandler`, captures it and answers 500, where `net/http` expects it re-panicked. | Pre-existing; "keeps its current behaviour" (contract §4.4). |
| F5 | `Init` assigns the package-level `defaultClient` without synchronization while `getDefaultClient` may read it. | Pre-existing; the race tests of §10 do not call `Init` concurrently. |
| F6 | `NewTransport` does not apply `withDefaults`, so a zero `BatchWait` panics the worker (core works around it, T24). | Pre-existing and not asked. |
| F7 | `docs/sdk-specification.md` §5 names `stacktrace[].file` ≤ 512 bytes and `function` ≤ 255 bytes as **SDK-side MUST-truncate** rules the server does not enforce (`bounds.go: MaxFilePathLength, MaxFunctionNameLength`), and says a pre-v2 cut must be **recorded** "as the JS SDK does in `metadata.truncated`". sdk-go truncates neither field and records none of §3.4's pre-v2 cuts; the only cuts it records (round 10) are the `contexts` and `metadata` entries it removes, in the JS SDK's `truncated` / `truncated_keys` (§3.4). | Both frame fields are fingerprint inputs (`Compute` hashes `file:function` of the first in-app frames, T35) and, unlike every value §3.4 changes, an over-long one **is** stored today; cutting it in v0.4.0 would split every existing issue whose frames carry one (a Go function name with generic type parameters can pass 255 bytes). Recording every cut needs a vocabulary for each field; the contract asks for none, and round 10's marker covers only the two fields whose entries the SDK removes whole. |
| F8 | The processor copies the metadata `release_version` fallback into `ReleaseVersion` with no length check (`event.go: (*ErrorEvent).Prepare`), and it is stored in `error_occurrences.release_version VARCHAR(100)` (`packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql`). A metadata value over 100 therefore fails at storage, after the event was accepted. The SDK's §3.4 bounds only the typed field. | Server-side (a bound in `Prepare`); this spec changes no server code. |
| F9 | `handleBatchIngest` counts an item's internal error (a failed publish) as `failed` and answers 400 when no item was ingested, so the transport does not retry it; `handleIngest` answers the same error with 500, which it retries (T38). Every event of a batch of two or more has had this in v0.3.x; §3.5 now sends a lone event to `/batch` too, so it reaches lone events. | An ingestor change (a retryable status, or a per-item retryable flag, for internal item errors); this spec changes no ingestor code (§2). |
| F10 | **Rewritten in rounds 8 and 10.** The ingestor never checks an event's size before the publish, so an event it accepts can still be lost at NATS for its size (T39): from a client that does not estimate the protobuf (a raw HTTP client; sdk-js, whose own review found the same `Struct` inflation), and from this SDK only if what the ingestor adds ever passes §3.5's 48,576-byte allowance (~15 KB today, T39) or its bounds step ever cuts an SDK event in a way that makes it **larger** (it only removes and truncates today). Round 10 removed the other way: the SDK no longer predicts the server's shrink, so a server rule that drifts from §3.4's copy can make the server cut an SDK event — costing the entries it removes, visible as a marker, and caught by P1.8's root half on the goldens — but not make the estimate wrong. The batch response counts a failed publish `failed`, which the transport reports through `OnError` as any other failed item (T36, T38). | Only the ingestor knows the published size. The fix belongs there — check `len(data)` plus headers against the connection's `MaxPayload` before the publish and answer that item as a permanent, named failure, or raise `max_payload` in compose, helm and both config files — and this spec changes no ingestor or infra code (§2). |
| F11 | **New in round 10.** The ingestor strips U+0000 from every v2 field but not from `metadata` (`bounds.go` has no `nul` call on it), and its own comments say `jsonb` rejects U+0000 with 22P05, a Permanent error that dead-letters the event (`bounds.go: cuts.addKey`, `cuts.nul`); a `metadata` string holding U+0000, from any client, v0.3.x included, may therefore be dead-lettered at the processor. Not run here. §3.4 does not strip `metadata` client-side, because the server does not, and the round-10 model is to apply exactly the server's bounds. | A server-side rule (strip it in `applyBounds`, marking `metadata`), after which §3.4 would copy it; this spec changes no server code (§2). |

---

## 13. Review findings, and where each is fixed

Spec review round 1 (against `7018fba`). Every finding is fixed; none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc-applycontext-drops-context-metadata` | major | `ApplyContext` gains a metadata row: `ScrubPII` of the context's metadata merged into `ev.Metadata` for keys the event does not already carry, which keeps `additionalContext` (Client) / `ctxTags` (`NewEvent`) winning over the context exactly as `client.go: CaptureErrorContext` does today; the row also gives `fingerprint`/`error_class` the effect `NewEvent` gives them. New exported reader `MetadataFromContext`. P1.2 and P1.3 now assert metadata on both paths, and P1.3 (c) tests the precedence. | §2 item 2; §3.2; §4.2 (`MetadataFromContext` row); §4.3 (table, closing paragraph); §8 (functions table, guarantee 2, call-order comment); §9 (`context.go`); §10.2 P1.1-P1.3 |
| `sdkgo-rc-withtag-reroutes-fingerprint-errorclass-release` | major | Audited every key the SDK, ingestor and processor read from metadata by name (new T27); `WithTag`'s metadata-routed set is now that complete list — `fingerprint`, `error_class`, `release_version` added, plus `runtime`, `server_truncated`, `server_truncated_keys` — as a table giving each key's reader and effect. `TestWithTagRouting` covers every key; P1.3 (c) proves `fingerprint`, `error_class` and `release_version` on both paths. | §1.2 T27; §1.3 D9; §4.2 (`WithTag` row, metadata-routed set table); §7.1; §10.2 P1.3; §11.3 |
| `sdkgo-rc-forwarded-headers-leak-client-ip` | major | `X-Forwarded-For`, `Forwarded`, `X-Real-Ip`, `True-Client-Ip`, `Cf-Connecting-Ip`, `X-Client-Ip`, `X-Cluster-Client-Ip` are always dropped from `request.headers`, whatever `CaptureClientIP` says, compared case-insensitively. P3.5 runs with the option off and on and asserts all seven (and their values) absent; P3.7 asserts none appears in either mode. | §2 "Does not"; §6.2 (option comment, headers rule); §6.4; §7.1; §10.2 P3.5, P3.7; §11.2 item 10 |
| `sdkgo-rc-breadcrumb-size-measured-on-wrong-shape` | minor | The 128 KiB list bound is measured on an unexported mirror of the ingestor's non-`omitempty` `Breadcrumb` (`data: null` when absent) with the ingestor's `arraySize`. T28 records the shapes and the measured 200 × 8,100 case (15 kept vs 16). P1.6 adds the case with a self-check that it still discriminates; P1.8 adds a golden for it. | §1.2 T28; §3.4 (breadcrumbs row); §9 (goldens); §10.2 P1.6, P1.8 |
| `sdkgo-rc-d5-ruling-not-stated-taskfile-gate` | minor | D5 records **SANCTIONED AND TIGHTENED**: `Taskfile.yml` `test-unit` gains `cd packages/sdk-go && GOWORK=off go test -race -count=1 ./...`; `ci.yml` `go-sdk`'s `go test` gains `-race`. "Locally only" deleted. | header; §1.3 D5; §9 (`Taskfile.yml`, `ci.yml` rows); §10.1 |
| `sdkgo-rc-d6-d7-rulings-not-stated` | minor | D6 records **SANCTIONED** (the golden wire fixture bridged into `tests/unit` is the gated proof; `tests/unit` in scope); D7 records **SANCTIONED** (`GOPRIVATE=github.com/tuju-ai/*` scratch-module resolution plus `go list -m`, after merge). No "ruling requested" remains. | header; §1.3 intro, D6, D7; §9 (`tests/unit` row) |
| `sdkgo-rc-p17-every-v2-field-unsatisfiable` | minor | P1.7 lists exactly the fields the SDK sets (and the v2 fields it never sets, which must be absent) instead of "every v2 field"; the SDK half fixes `Config.ServerName`, the request's host, remote address and headers, and uses a bare handler so `transaction` is toolchain-independent, so no host or port of the machine enters the golden. | §9 (`tests/contract` row); §10.2 P1.7, P1.8 |
| `sdkgo-rc-tags-feed-tag-facet-rollup` | note | T29 records that tags feed the `issue_tag_values` rollup; the CHANGELOG's "Moved" subsection carries the warning and points per-request identifiers at `WithContextMap`. | §1.2 T29; §4.2 (`WithContextMap` row); §7.1 |
| `sdkgo-rc-applycontext-user-replaces-option` | note | The context user merges field by field into an existing `ev.User` (an option-set field wins; the existing pointer is copied, never written in place). The same fill rule now applies to tags; §4.3 states the precedence of every row. P1.5 asserts both. | §3.2; §4.3; §8 guarantee 2; §10.2 P1.5; §11.2 item 9 |
| `sdkgo-rc-compat-unkeyed-literals` | note | The CHANGELOG's v0.4.0 entry has a Compatibility subsection: unkeyed composite literals of `Event` and `Config` stop compiling. | §7.1 |
| `sdkgo-rc-middleware-eager-per-request-alloc` | note | The ring is allocated by the first `AddBreadcrumb`; the header/query snapshot (and the client IP) is built once, under `sync.Once`, by the first capture; the middleware eagerly allocates only the scope, a buffer header and the context values. P3.10 proves it. | §5.2; §6.2 (steps 1-2, "Why lazy"); §6.4; §10.2 P3.10; §11.2 item 11 |

Spec review round 2 (against `88987ef`). Every finding is fixed, minors and notes included; none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc2-p36-mux-pattern-unprovable-go121-module` | major | Verified as the reviewer did (T30, **run** on go1.21.13, go1.22.12, go1.26.4): the module's `go 1.21` line makes every test binary run with `httpmuxgo121=1`. The go1.23 test file carries `//go:debug httpmuxgo121=0` and asserts the handler was reached; the pre-1.23 test registers `"/items/"` and asserts the path. §6.3, D1 and the CHANGELOG say a consumer whose main module declares `go` < 1.22 gets the path by default. Every other prescribed test re-checked for the same `go`-line trap (§10.1's list): all other middleware tests use a bare handler; timers, `panicnil`, loop variables and post-1.21 APIs covered. | §1.2 T30; §1.3 D1; §6.3; §7.1; §9 (test files row); §10.1 ("The `go`-line trap"); §10.2 P1.1, P2.2, P3.6; §11.1 |
| `sdkgo-rc2-client-ip-header-list-closed` | minor | Client-IP headers are dropped by the seven names **and** by case-insensitive name pattern (`forwarded`, `client-ip`, `clientip`, `real-ip`, `connecting-ip`, `external-address`, `user-ip`, `socketip`); the cost (`X-Forwarded-Host`/`-Proto`/`-Port`/`-Prefix`) is stated. P3.5 adds `X-Original-Forwarded-For`, `X-Envoy-External-Address`, `X-Appengine-User-Ip`, `X-ClientIP` and `X-Forwarded-Host`; P3.7 adds the first two. | §6.2 (option comment, headers rule); §6.4; §7.1; §10.2 P3.5, P3.7; §11.2 item 13 |
| `sdkgo-rc2-withtag-scalar-named-types-ambiguous` | minor | Scalar = `reflect.Kind` `String`, `Bool`, `Int*`, `Uint*` (not `Uintptr`), `Float*`, or implements `fmt.Stringer`; string form `fmt.Sprint` for all of them; nil pointers ignored (superseded in round 4: a nil value or nil pointer removes the key from both stores). `TestWithTagRouting` adds `type Region string`, a named int, `time.Duration`, a Stringer struct, and non-scalars. | §4.2 (`WithTag` row); §10.2 P1.3 |
| `sdkgo-rc2-applycontext-errorclass-beats-option` | note | `NewEvent` records its pre-options class and fingerprint (unexported) and the SDK's two options set a flag; the metadata row sets `ErrorClass`/`Fingerprint` only when no option set them, so option-wins holds on both keys. `TestApplyContextOptionWinsOverMetadataKeys` covers the SDK options, a custom option, an option equal to the default, no option, and the typed `WithErrorClass`/`WithFingerprint`. | §3.2; §4.3 (intro, metadata row, `With*` row, closing paragraph); §8 guarantee 2; §10.2 P1.5; §11.2 items 9, 14 |
| `sdkgo-rc2-crumb-mirror-empty-data` | note | The size mirror counts nil **or empty** `data` as `null` (T28 records why); `AddBreadcrumb` turns an empty map into nil. `TestBoundBreadcrumbsEmptyDataCountsAsNull` tests the 131,073 / 131,072 boundary with empty maps set directly on the event; P2.7 tests the normalisation. | §1.2 T28; §3.4 (breadcrumbs row); §5.2; §10.2 P1.6, P2.6, P2.7 |
| `sdkgo-rc2-race-ci-step-unproven-premerge` | note | §10.1 gains a required pre-merge proof: `gh workflow run ci.yml --ref <branch>`, both `go-sdk` legs and `go-root` green, with the `-race` command visible in the log, recorded in the delivery report. The step sets `CGO_ENABLED: "1"` and fails loudly without cgo or a C compiler (T21, **run** on both toolchains); removing `-race` or adding leniency is forbidden, and a runner without a C toolchain is reported as `BLOCKED_EXTERNAL`. | §1.2 T21, T32; §1.3 D5; §9 (`ci.yml` row); §10.1 |
| `sdkgo-rc2-normalizelevel-zerolog-string` | note | §8 tells core to map from the `zerolog.Level` enum with `sentinelzerolog.LevelFromZerolog` (added to §8's surface table and P1.9), which core imports; it stays out of the root package, which imports no zerolog (T31). P1.4 adds a test that renamed level globals do not change the mapping. T24 now cites core `origin/production` `73be60827` (v0.3.1) and `main` `dc653f34a` (v0.3.2), so the upgrade crosses the v0.3.1, v0.3.2 and v0.4.0 entries. | §1.2 T24, T31; §7.1; §8 (surface table, call order, level paragraph); §10.2 P1.4, P1.9; §11.1; §11.2 item 15 |
| `sdkgo-rc2-changelog-withtag-shapes-now-dropped` | note | **Chose to route, not drop:** a `WithTag` scalar that cannot be a valid tag (bad key; value empty, over 200 code points, or multi-line) goes to metadata as in v0.3.x, using the bounds step's own predicate. The reason is in §4.2 and §11.2 item 12. The CHANGELOG names what *is* now dropped (a direct `Event.Tags` entry, an invalid `tenant_id`, tags past the 50th key) and points at `WithContextMap`. | §1.3 D9; §3.4 (tags row); §4.2 (`WithTag` rule 1 and rule 4, "Why an invalid tag…"); §7.1; §10.2 P1.3; §11.2 item 12 |

Spec review round 3 (against `bef0d49`): 0 majors; the three minors and three notes are all fixed (clean-pass rule);
none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc3-withtag-reroute-leaves-stale-value` | minor | A write that lands a key in tags or in metadata removes it from the other store, copy-on-write, so the last write wins across both as it did on v0.3.x's one map — `WithTag` (every rule), `WithTenant`, `WithContextMap`, in both directions; `WithTag("user_id")` also removes metadata `user_id`; `WithContextMap({"user_id"})` leaves the typed user alone (stated divergence). `TestWithTagRouting` adds the value becoming empty, over 200 code points and non-scalar, the reverse, `WithContextMap` then `WithTag` and the reverse, `tenant_id` both ways and `user_id`, each asserting exactly one store, the second value, and an untouched parent context. | §4.1; §4.2 (`WithTenant`, `WithTag`, `WithContextMap` rows, "One key, one store"); §7.1; §10.2 P1.3; §11.2 item 16 |
| `sdkgo-rc3-message-over-10000-still-400s` | minor | `boundEvent` truncates `message` to 10,000 code points on a rune boundary. T33 records the `error_event.message` CEL rule (`size()` = code points, 400 over it) and proves `message` is no fingerprint input (`fingerprint.go: FingerprintConfig, Compute`, the processor's one call; the SDK's `NewEvent` never derives a fingerprint from it). §3.4's "not bounded" list no longer names `message`, and says why `stacktrace` stays. P1.6 adds 10,000 / 10,001 ASCII and 10,001 / 10,000 three-byte-rune cases; P1.8's bounded golden carries a 12,000-code-point message and its root half now runs protovalidate. | §1.2 T33; §1.3 D3; §3.4 (`message` row, "Not bounded client-side"); §7.1; §10.2 P1.6, P1.8; §11.2 item 17 |
| `sdkgo-rc3-contexts-inner-maps-shared` | minor | `NewEvent` allocates the `runtime`/`os`/`device` inner maps per event; only the scalar values may be cached. P1.5 adds `TestAutomaticContextsInnerMapsPerEvent` (mutate one event's inner maps, a second `NewEvent` is unaffected) and `TestAutomaticContextsRaceBeforeSend` (under `-race`, `BeforeSend` writes inner maps while the worker marshals other events). | §3.2 (`Contexts` row); §8 guarantee 1; §10.2 P1.5; §11.2 item 18 |
| `sdkgo-rc3-clientip-patterns-residue` | note | Name patterns gain `connection-ip`, `originating-ip`, `remote-ip`, `remote-addr`; a value rule drops any `r.Header` entry whose comma-separated tokens, trimmed and with port and brackets stripped, all parse with `net.ParseIP` (`Host` exempt: the server's address). The remaining residue is stated. P3.5 adds `X-Nf-Client-Connection-Ip`, `X-Originating-Ip`, `X-Remote-Ip`, `X-Remote-Addr`, two value-rule-only headers and a kept `X-Build: 1.2.3`; P3.7 adds the first two. | §6.2 (option comment, headers rule, `Host`); §6.4; §7.1; §10.2 P3.5, P3.7; §11.2 item 19 |
| `sdkgo-rc3-core-request-scope-unreachable` | note | §8 states that on core's path `request`/`transaction` come only from core's own assignments: core is gin, uses neither middleware and never calls `Init` (T34, verified on core `dc653f34a`); core builds `RequestInfo` from its already-redacted `httpx` metadata, which keeps the core contract's decision 4. The snapshot builder stays unexported. §6.5 now states that the recovered-panic path needs an `Init`'d default client (without it, `getDefaultClient` builds one from an empty `Config`, and the event never reaches the project). | §1.2 T34; §6.5; §7.1; §8 (core-path paragraph, call-order comment); §11.2 item 20 |
| `sdkgo-rc3-changelog-zerolog-nolevel-vague` | note | The v0.4.0 entry says in a sentence that `logger.Log()` (`NoLevel`) lines and `Disabled`-level lines, which v0.3.x captured as errors, are no longer sent; only `Error`, `Fatal` and `Panic` lines are. | §7.1 |

Spec review round 4 (against `82873cd`): the round-3 fixes were verified; the one major, two minors and two notes are all
fixed (clean-pass rule); none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc4-breadcrumb-data-shared-across-events` | major | `BreadcrumbsFromContext` (so `ApplyContext` and `CaptureErrorContext`) gives every returned crumb its own top-level copy of `Data` (≤ 100 small copies per capture). Then every map, slice and pointer an `Event` can hold was audited against the context, `Config`, the caches and other events (new T37 on `ScrubPII` and the `Client` path's `ctxTags`): §8 guarantee 1 is now the complete table — `Tags`, `Contexts` and its inner maps, `Metadata` and its nested `map[string]interface{}` values, `User`, `SDK`, `Request` and `Headers`, the crumb slice and each crumb's top-level `Data`, `Stacktrace` are per event; values nested in crumb `Data` and non-map reference values in metadata are **shared** and must be replaced, not edited; `Request.Data`/`Env` are never set by the SDK. The client-IP row of `ApplyContext` now also copies rather than writes in place. `BeforeSend`'s doc comment points at the table. New P1.10: `TestEventReferencesArePerEvent` (every per-event row, both paths) and the `-race` `TestBreadcrumbDataRaceBeforeSend` (many captures of one request, a `BeforeSend` deleting a `Data` key while the worker marshals). | §3.5 (last paragraph); §4.3 (client-IP and breadcrumb rows); §5.1; §5.2; §6.1; §7.1; §8 guarantee 1; §10.1; §10.2 P1.10; §11.2 item 21; §1.2 T37 |
| `sdkgo-rc4-race-test-evicted-by-default-buffer` | minor | `Push` evicts silently at `MaxBufferSize` (default 100 — through `Init` only; corrected in round 5: `NewTransport` applies no default) without `OnError` (new T36). P1.5's race test sets `MaxBufferSize` 512 and, after `Flush`, asserts `droppedCount == 0` and `OnError` never called before counting `seq` values. §10.1 gains "the eviction trap": every prescribed test re-checked — exactly two push more than 100 events (P1.5's 400, P1.10's 200; both set 512), every delivery-counting test asserts no drop first, and the rest push at most 20. | §1.2 T36; §10.1; §10.2 P1.5, P1.10 |
| `sdkgo-rc4-withtag-nil-write-keeps-stale-value` | minor | A `nil` value or nil pointer passed to `WithTag` removes the key from both stores, copy-on-write (under `user_id`, it clears the typed user's `ID`); a key in neither store leaves `ctx` unchanged. `TestWithTagRouting` adds `WithTag(plan,"pro")` then `WithTag(plan,nil)`, the nil-pointer form, `WithContextMap` then nil, and the two `user_id` cases. The CHANGELOG's last-write-wins sentence names it. | §4.2 (`WithTag` row, "One key, one store"); §7.1; §10.2 P1.3; §11.2 item 24 |
| `sdkgo-rc4-prev2-protovalidate-residue-unlisted` | note | Bounded client-side, per field with its reason: `error_class` truncated to 255 (empty → `Error`: it is `required`), `fingerprint` over 64 dropped (a cut grouping key could merge issues), `release_version` truncated to 100 (per-process, so equality survives), `trace_id`/`span_id` over 64 dropped (identifiers into a trace backend). Auditing **every** rule of `error_event.proto` (new T35) found four more that could 400 an SDK event, now bounded too: `platform` (→ `go`), `environment` (normalized; empty → `development`), `stacktrace` over 100 frames (first 100), an in-app frame without a file (→ not in-app), plus a frame `line` outside `int32` (a decode 400). §3.4 ends with a table of every rule in the file and what satisfies it (SDK row, server step before protovalidate, or absent field). P1.6 adds `TestBoundEventPreV2Fields`; P1.8's bounded golden carries a value over every pre-v2 row and its root half runs protovalidate. Found alongside and fixed: T27's audit missed `bounds.go: metadataExemptKeys`, so `truncated`/`truncated_keys` join the metadata-routed set. Recorded, not fixed: F7 (frame `file`/`function` MUST-truncate and cut recording in `docs/sdk-specification.md` §5), F8 (the processor's unbounded metadata `release_version` fallback). | §1.2 T27, T35; §1.3 D3; §3.4 (rows, "Not bounded", rule table); §3.5; §4.2 (metadata-routed set); §7.1; §8 guarantee 3; §10.2 P1.3, P1.6, P1.8; §11.2 items 17, 22, 23; §12 F7, F8 |
| `sdkgo-rc4-core-call-order-blanks-context-fingerprint` | note | §8's call order assigns `ErrorClass` / `Fingerprint` only when core has a non-empty value, and says a non-empty core value wins over the context (`ApplyContext` ran first), while an unconditional empty assignment would blank the context's `WithFingerprint` / `WithErrorClass` value. | §8 (call order, the paragraph after it) |

Spec review round 5 (against `e3fef55`): every round-4 fix was verified; 0 majors. The two minors and two notes are all
fixed, each by changing the design (clean-pass rule); none is dismissed or recorded as residue.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc5-sharing-sdk-writes-caller-owned-values` | minor | Copy-on-write everywhere the SDK writes. `ApplyContext` builds a new `Tags` / `Metadata` map whenever it adds a key (as its user row already built a new `*UserInfo`) and never writes through a reference the event holds; the transport's bounds step works on `b := *ev` and shallow-copies `User`, `SDK` and `Request` (and rebuilds every map or slice) before changing any field, so the pushed `*Event` is never written either. §8 guarantee 1 states the rule once — *the SDK never writes into a map, slice or pointer it did not allocate for this event* — naming `ApplyContext` and the transport. P1.10 adds `TestSDKNeverWritesCallerValues` under `-race`: an option-supplied shared `Tags` map and a shared over-bound `*UserInfo` (and the same two assigned by `BeforeSend` on the `Client` path, and pushed directly) are deep-equal to their originals after 200 concurrent captures per path. | §2 item 6; §3.5 (step 1, "The transport never writes into what it was pushed", `Push` doc comment); §4.3 ("It assigns; it never writes through", metadata and tags rows); §7.1; §8 guarantee 1 (the rule, `Tags` and `Metadata` rows, closing paragraph); §10.1; §10.2 P1.10; §11.2 item 25 |
| `sdkgo-rc5-newtransport-tests-unbuffered-queue` | minor | Verified (**run**): `NewTransport` applies no defaults, so an unset `MaxBufferSize` is an unbuffered channel on which `Push` dropped 20 of 20 events; an unset `BatchSize` sends every event alone; an unset `BatchWait` panics the worker. T36 now says so. §10.1 states that every test calling `NewTransport` sets `MaxBufferSize` (≥ events pushed; 512), `BatchSize` and `BatchWait` explicitly, as core's `sink.go: Open` does, through one helper, `newTestTransport(t, cfg)`, that fails the test when any of the three is zero, and lists those tests. P1.6's batch-of-3 case fixes `BatchSize` 3 and `BatchWait` 1 minute and asserts one `/batch` body. The round-4 audit's claim that the buffer is 100 "by default" is corrected in §10.1 (true through `Init` only), and its count of tests pushing over 100 events is redone (four, all at 512). | §1.2 T36; §10.1 (gate preamble, "The eviction trap"); §10.2 P1.2, P1.6 |
| `sdkgo-rc5-bounded-event-exceeds-body-cap` | note | New T38: the caps are 1 MiB single, 5 MiB and 500 items batch (each a 413, not retried, whole body dropped), and `json.Marshal`'s 6-byte escapes put 64 bounded headers of 4,096 `'<'` at 1,573,569 bytes (**run**). §3.5 step 3: an event over 1,048,576 bytes sheds `request.headers`/`env`, then `request.data`, then `breadcrumbs` (on the copy), and only then is dropped alone with `OnError`; the survivors are cut into bodies of at most 500 events and 5,242,880 bytes counting separators, each sent and accounted alone. F2 is corrected (its "far below" claim was wrong) and now records only what §3.4 leaves unbounded. New P1.11: 64 × 4,096 `'<'`; four such events in one batch; the shed order step by step; the exact-1 MiB boundary; 5 exact-1 MiB events (cut 4 + 1 by the separators); 501 events (cut 500 + 1) — against a recording server that enforces the ingestor's caps. | §1.2 T38; §1.3 D3; §2 (item 6, "Does not"); §3.4 (closing paragraph); §3.5 (step 3, body cutting); §7.1; §8 guarantee 3; §9 (`transport.go`); §10.1; §10.2 P1.11; §11.2 item 26; §12 F2 |
| `sdkgo-rc5-sso-assertion-headers-recorded` | note | The always-dropped credential headers gain name patterns `jwt`, `assertion`, `oidc`, `principal`, `session` (lower-cased, contains), beside the five names, as §6.2 (b) does for client IPs; the families they catch (Cloudflare Access, Google IAP, AWS ALB, Azure App Service, Pomerium, session ids) and the cost (a header merely mentioning a session) are stated. P3.5 adds `Cf-Access-Jwt-Assertion` and `X-Amzn-Oidc-Data` carrying fake JWTs, plus one header per pattern that matches only it (`X-Jwt-Payload`, `X-Saml-Assertion`, `X-Ms-Client-Principal`, `X-Session-Id`); none of them, nor the JWTs' marker email, may reach the body. | §6.2 (credential headers rule); §7.1; §10.2 P3.5; §11.2 item 27 |

Spec review round 6 (against `9420ec9`): 0 majors, 0 minors, three notes, all fixed; none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc6-body-cap-sheds-what-batch-accepts` | note | The transport posts **every** body to `{endpoint}/batch`, a lone event as a one-element array, as v0.3.x already did for every batch of two or more and the JS SDK does for every batch (T38: `handleBatchIngest` accepts a one-element array; verified in `main.go` and `packages/sdk-js/src/core/options.ts`). It sheds `request.headers`/`env`, then `request.data`, then `breadcrumbs`, and drops with `OnError`, only an event whose one-element array is over 5,242,880 bytes, and cuts bodies so none exceeds that; the single endpoint's 1 MiB cap no longer applies to anything the SDK sends. No SDK-bounded field reaches 5 MiB alone, so shedding is reached only through unbounded `metadata`/`contexts`/frame fields. The cost — a lone event's internal ingestor error is now the batch handler's 400, not the single handler's retried 500 — is recorded as §12 F9. P1.11's shed and drop cases move to the 5 MiB threshold, gain `TestLoneEventOverOneMiBSentWhole` and `TestFourHeaderHeavyEventsCutNotShed` (header-heavy events over 1 MiB are sent whole), and its server fails the test on any request outside `/batch`. Existing tests that decode a bare object are listed for update. | §1.2 T38; §1.3 D3; §2 (item 6, "Does not"); §3.4 (closing paragraph); §3.5 (step 3, body cutting, accounting); §7.1 (behaviour-change line); §8 guarantee 3; §9 (`transport.go`, `*_test.go`, `tests/contract` rows); §10.1 (body-shape rule); §10.2 P1.11; §11.2 decision 26; §12 F2, F9 |
| `sdkgo-rc6-newevent-nil-metadata-beforesend-panics` | note | `NewEvent` sets `Metadata` to a new, empty, non-nil map when `ctxTags` is nil (`ScrubPII(nil)` returns nil, `pii.go: ScrubPII`), so a `BeforeSend` or option writing `ev.Metadata[k]` on core's path cannot panic and drop the event; the wire is unchanged (`metadata` is `omitempty`). §8 guarantee 1's `Metadata` row states it is never nil. P1.2 adds a nil-`ctxTags` event whose `BeforeSend` writes `ev.Metadata["core_hook"]` and must arrive. | §3.2 (new `Metadata` row, the paragraph after it); §8 guarantee 1 (`Metadata` row); §10.2 P1.2 |
| `sdkgo-rc6-key-named-credential-headers-recorded` | note | The always-dropped credential headers gain a third family, (c): any lower-cased name **ending in** `-key` (Azure API Management `Ocp-Apim-Subscription-Key`, Azure Functions `X-Functions-Key`, which no PII-list string catches). `Idempotency-Key` and `Sec-Websocket-Key` are dropped too and stated as the accepted cost, as `session`'s is. P3.5 sends both keys with marker values and asserts neither header nor value reaches the wire; its mutation list gains "drop the `-key` suffix check". | §6.2 (credential headers (c)); §7.1 (PII/headers line); §10.2 P3.5; §11.2 decision 27 |

Spec review round 7 (against `21884bf`): 0 majors, one minor, 0 notes, fixed; none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc7-nats-max-payload-below-shed-threshold` | minor | Verified (**run**, new T39): `service.IngestAt` publishes every accepted item alone (`proto.Marshal` → `publish` → `Publisher.PublishWithHeaders`), no NATS server here sets `max_payload`, so nats.go's `ErrMaxPayload` check refuses any message over the 1,048,576-byte default, and a bounded event of 64 headers × 4,096 U+1F600 — 1,049,680 bytes of JSON, under 5 MiB — is **1,049,715** bytes of proto: accepted, then lost. §3.5 step 3 gains a second per-event limit: the **publish size**, `b` encoded by a `json.Encoder` with `SetEscapeHTML(false)` (so `<`, `>`, `&` count one byte, as in proto; the `'<'` event is 263,155 in proto), kept at most **1,000,000 bytes**; while either it or the 5 MiB one-element array is over, the existing (a)-(d) order sheds. The 5 MiB / 500 body cut is unchanged. Headroom sized from the ingestor's growth, checked in `enrich.go`, `bounds.go`, `service.go: publish` and the propagator: NATS header block (~85 bytes; ≤ ~9 KB with a client's `tracestate`/`baggage`), `user.geo` (only with an SDK-sent address), `User-Agent` contexts (UA ≤ 1,024 bytes), `received_at`, project name, markers (net negative for an SDK-bounded event) — under ~20 KB, so 48,576 bytes is over twice it. The measurement also found what it cannot see: numbers in `Struct` fields inflate ~5.5× in proto, recorded as §12 F10 (an ingestor/infra fix). P1.11 gains `TestLoneEventOverPublishLimitShedsHeaders` (4-byte-rune headers, shed) beside the `'<'` case (sent whole), a publish-limit family in `TestShedOrder` with an exact-1,000,000 and a 1,000,001 boundary, a server that fails the test on any element over the publish limit, `'<'` padding for the 5 MiB cases, and four new mutations; P1.8's bounded golden pins its header runes to ASCII and `"€"` and asserts it is under both limits, so the new limit cannot silently shed the headers it exists to show bounded. | header (infra line); §1.2 T39; §1.3 D3; §2 (item 6, "Does not"); §3.4 (closing paragraph); §3.5 step 3; §7.1 (behaviour-change line); §8 guarantee 3; §9 (`transport.go` row); §10.1 (drop and server rules); §10.2 P1.8, P1.11; §11.2 decision 26; §12 F2, F10 |

Spec review round 8 (against `43c938d`): 0 majors, one minor (five parts), 0 notes, plus one coordinator addition from
the parallel sdk-js review; all fixed, none dismissed. The five parts and the addition are one defect class — §3.5's
publish measure did not model what the ingestor publishes — so they share one fix, the **publish cost**, and each row
below says where its part landed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc8-publish-size-counts-server-shrunk-fields` (1): metadata and contexts counted before the server's 64 KiB shrink | minor | Verified (**run**, T40): `ValidatePayload`'s `shrinkMetadata` and `boundContexts` run before `MapPayloadToEvent` and `proto.Marshal`; `metadata` {`request_id`, 1.5 MB `debug_dump`} is 1,500,559 bytes of escaping-off JSON and **321** of protobuf; `contexts.app` of 1.2 MB, 296. §3.5's publish size is replaced by the **publish cost**, `publishCost(b, raw)`: `metadata` and `contexts` are decoded from `raw` as the ingestor decodes them and replayed through the server's shrink — `metadataExemptKeys` (18, copied) before the rest, largest `entrySize` first, ties by key, to 65,536; `contexts` with `nulMap`'s U+0000 handling, the 1-64 code-point name rule, well-known names last, ≤ 32 and 65,536 — without the markers, so the replay keeps every entry the server keeps (a marker-tipped entry is the only over-count). Both reviewer cases now cost 545 and 520 and are sent whole. The 5 MiB check still counts them whole (the body carries them). The wrong statements are fixed: §3.5's "reached by … `metadata`, `contexts` and frame `file`/`function`" now says what reaches each limit (after (c) only frame text holds an event over the publish limit, ~900 KB of estimate at most without it); §8 guarantee 3 and §12 F2 likewise, F2 no longer claiming v0.3.x lost such events at the publish. P1.11's publish family is re-padded (next row). A fractional-knapsack bound, which needs no copied order, was probed and rejected: on `metadata` {`x`: 32,000 zeros, `y`: 10,000 × `s`}, where the server evicts `x`, it counts ~353,500 bytes against the replay's 10,015 (decision 26, T40). | §1.2 T40 (new), T39; §2 items 6 and "Does not"; §3.4 ("Not bounded", closing paragraph); §3.5 step 3 and "The publish cost"; §8 guarantee 3; §9 (`publishcost.go`); §11.2 decision 26; §12 F2 |
| Coordinator addition, `sdkjs-rc-spec-node-event-over-nats-max-payload`: `Struct` numbers, bools, nulls and string entries inflate in protobuf, so JSON does not bound it | minor | Verified (**run**, T40) on this SDK's shapes: the sdk-js case (`metadata` of 32,710 zeros, kept by the server; 32 crumbs of 2,030 zeros, 31 kept) is 193,436 bytes of JSON and **1,053,347** of protobuf; with a 64 KiB context of zeros, 1,411,975 (the sdk-js reviewer's own event: 1,053,618 and 1,413,328). The publish cost counts the four `Struct`-typed fields — `metadata` (9), `contexts` (26), `Request.data` (5), `Breadcrumb.data` (6), checked in `error_event.proto` — at their exact protobuf size, from `struct.proto`'s encoding: a number 9 bytes in its `Value`, 11 in a list; a bool or null 2; a string `ld(len)`; a list or object length-delimited; an object entry `ld(ld(len(k)) + ld(V(v)))`. The typed fields keep the escaping-off JSON measure plus 6 bytes a map entry (proto's worst per-entry overhead for a §3.4-bounded header). Budget kept at **1,000,000** and stated why: the headroom now carries only what the ingestor adds (~21 KB: NATS headers, geo, `User-Agent` contexts, `received_at`, `project_id`, project name, markers ≤ 7,347), since what round 7 left to it is now counted. Against the real pipeline the estimate is 224-5,112 bytes over on every shape measured and never under on 4,000 random events. The sdk-js case now sheds its crumbs and is delivered (360,349); with the context, 718,977. | §1.2 T40, T39; §3.5 "The publish cost"; §7.1; §8 guarantee 3; §11.2 decision 26; §12 F10 |
| `sdkgo-rc8-…` (2): "every string's JSON is at least its proto bytes" and F10's "Unaffected: … strings, objects" | minor | Both removed. §3.5 now claims JSON ≥ protobuf only for the typed fields, with the `map<string, string>` entry named as the one exception and paid for (6 bytes an entry); inside a `Struct` a string entry is `k + v + 8` against `k + v + 6` (the reviewer's 6,000 empty strings: 77,190 bytes of protobuf, estimate 77,414, T40), and the estimate counts it that way. F10 is rewritten around what is still out of scope (the ingestor's own size check) and records round 7's wrong claim. | §3.5 part 1 and the size functions; §12 F10 |
| `sdkgo-rc8-…` (3): the tracking claim had no gated proof | minor | P1.8's root half gains `TestSDKGoPublishCostBoundsProto`: for five goldens — full, bounded, crumbs, a new **number-dense** one within every server bound (~860,000 of estimate, ~5.4× its JSON) and a new **shrunk** one (the server evicts `contexts.blob`, four equal contexts by name, then `b` and `a` before the exempt `request_id`) — it requires `len(proto.Marshal(mapped)) <= cost`, tolerance **0** (the estimate is an upper bound by construction and the test adds nothing `IngestAt` adds), and `cost - len <= 16,384`; the SDK half records each golden's cost in `testdata/wire_v2_publish_cost.json`. Mutations: a list element without its wrapper, a number as 8 bytes, the replay without the exempt set (all `n > cost`); `metadata`/`contexts` counted whole, `contexts` without the 32 rule (over the 16,384 tolerance). | §9 (testdata, `tests/unit`); §10.2 P1.8 |
| `sdkgo-rc8-…` (1, tests): TestShedOrder's publish family padded `metadata`, asserting a drop of a deliverable event | minor | Re-padded with what the ingestor publishes as sent: U+1F600 headers, frame `function` text and number-dense `request.data` / breadcrumb `data` — never `metadata` or `contexts`. (d) is now 100 frames of 11,000-byte `function`: 1,106,341 of estimate and **1,102,365** bytes of protobuf, which NATS refuses, so the drop loses nothing deliverable. New cases: `TestNumberDenseCrumbsShed` (the sdk-js numbers: crumbs shed, metadata and context whole), `TestServerShrunkFieldsSentWhole` (the reviewer's 1.5 MB `metadata` and 1.2 MB `contexts.app`: sent exactly as pushed, cost 2,837), `TestShrinkReplayKeepsExempt`. New mutations: `Struct` fields by their JSON, `metadata`/`contexts` counted whole, the replay without the exempt set. | §10.1; §10.2 P1.11 |
| `sdkgo-rc8-…` (4): §7.1 said "whose JSON is over 1,000,000 bytes" | minor | The CHANGELOG line now says the SDK estimates the protobuf from the JSON **without HTML escaping** (so `<`, `>`, `&` count one byte), with `metadata`, `contexts`, `request.data` and breadcrumb `data` counted as protobuf encodes them and `metadata`/`contexts` only as far as the server's 64 KiB bound keeps them. | §7.1 |
| `sdkgo-rc8-…` (5): what §3.4's `request.data` / breadcrumb `data` bound does when `json.Marshal` fails | minor | Stated: a value whose marshal fails counts as over its bound, so the field is set to nil and the event is sent (the crumb is kept); §3.5 step 2 now drops an event only for a `Metadata` or `Contexts` that fails. P1.6 gains `TestBoundEventUnmarshalableData` (`NaN` data and a `func` in crumb data: delivered without both, `droppedCount == 0`) and the mutation "leave it in place"; §7.1 notes the change. | §3.4 (intro, `request.data` and `breadcrumbs` rows, closing paragraph); §3.5 step 2; §7.1; §8 guarantee 3; §10.2 P1.6 |

Spec review round 9 (against `c7229ac`): round 8's fixes were verified; 0 majors, three minors, all fixed as the reviewer
proposed; none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc9-publish-cost-marker-and-replaced-id-overcount` | minor | Verified (**run**, new T41). (a) The `metadata` replay now counts the markers the server is sure to write, measured as `shrinkMetadata`'s `size()` measures them: `server_truncated` with `"metadata"` once the shrink runs, plus `"contexts"` when the `contexts` replay (now run first, as `applyBounds` runs `boundContexts` first) removed a context or a U+0000; `server_truncated_keys` with that replay's `contexts.<name>` removals in name order, then each `metadata` key it removes, each cut to 128 bytes (`truncateBytes`), the first 50 and then `"+N more"`. That is a lower bound of the server's markers, so the replay removes a prefix of the server's removals and is exact for an SDK-bounded event (only a U+0000 in `request.data` or crumb `data` makes the server write more). The reviewer's event — `metadata` {`A`: 70,000 × `x`, `B`: 32,740 zeros} and 100 frames of a 6,500-byte `function` — is measured: the server removes `A` and then `B` (65,487 bytes alone, 65,549 with the two markers) and publishes 651,790 bytes; round 8 estimated ~1,016,000 and dropped it, round 9 ~656,000, and P1.11's new `TestShrinkReplayCountsMarkers` requires it sent whole. (b) The typed-field part counts an `event_id` `resolveEventID` would re-mint (empty, over 64 code points, a control character) as a 36-character placeholder and `project_key` cut to 64 code points (`applyAuthenticatedScope` replaces it or refuses the item); `TestPublishCostCountsServerIdentifiers` (a 1,000,000-character `event_id`: sent whole). §3.5's "~900 KB" sentence now names both identifiers and the fixed-size `timestamp`/`trace_flags`, which it had silently assumed bounded. | header (revision); §1.2 T41; §3.4 ("Not bounded client-side"); §3.5 step 3 (the ~900 KB sentence, shed steps keep `m`), "The publish cost" (signature `publishCost(b, m)`, parts 1-5, accuracy and cost paragraphs); §8 guarantee 3; §9 (`publishcost.go`); §10.2 P1.8, P1.11; §11.2 decision 26 |
| `sdkgo-rc9-marshalable-undecodable-value-400s-body` | minor | Verified (**run**, T41): `handleBatchIngest` decodes the whole body at once and answers 400 on any error; a `*big.Int` of 10^400 marshals and fails the decode (`UnmarshalTypeError` into `float64`), and 9,998 nested lists in `metadata` marshal and fail it in a body (`exceeded max depth`; 9,997 is the deepest a body carries). §3.5 step 2 now decodes each event's `raw`, as the one-element array so every value sits at its body depth, into the ingestor-typed decode mirror `publishCost` reads; on an error it decodes each `Struct`-typed field alone at its body depth to find the culprit: `metadata` or `contexts` → the event is dropped alone (`droppedCount` +1, `OnError` naming its `event_id`, the field and the error); `request.data` or a crumb's `data` → set to nil on the copy, re-marshalled and re-decoded, and the event sent. `publishCost(b, m)` reads that mirror and never decodes, so it cannot fail; the shed steps keep `m` in step with `b`. P1.6 gains `TestOneUndecodableEventDropsAlone` (3 events against a server that decodes as the ingestor and answers 400: the 10^400 number in `metadata`, 10,001-deep nesting, 9,998-deep nesting that passes only a decode without the array, and the number in `contexts` — 2 delivered in one body, `droppedCount` 1 each) and `TestBoundEventUndecodableData`, and the mutations "skip the decode check" (the whole body gets 400: `droppedCount` 3), "decode without the enclosing array" and "drop instead of nil". | §2 item 6; §1.2 T41; §3.4 (`request.data` and `breadcrumbs` rows, closing paragraph); §3.5 step 2, step 3, "The publish cost"; §7.1; §8 guarantee 3; §9 (`transport.go`, `publishcost.go`); §10.1 (drop-expecting tests, decoding server); §10.2 P1.6, P1.8, P1.11; §11.2 decision 28 (new) |
| `sdkgo-rc9-referer-carries-unfiltered-query` | minor | §6.2: a kept header whose value is a URL gets the `query_string` rule — each value `url.Parse`d, its `RawQuery` filtered exactly as `query_string` (PII-named parameters → `[FILTERED]`), its fragment and userinfo removed, written back with `String()`; a value that does not parse drops the header. Treated: `Referer`, `Location`, `Content-Location`, `X-Original-Url`, `X-Original-Uri`, `X-Rewrite-Url`. Not treated, with why: `Origin` (no path or query), `X-Forwarded-Uri` (already dropped by the `forwarded` name pattern), `Link` (a list, not a URL: dropped whole). The closed list and its residue are stated. P3.5 gains `TestMiddlewareURLHeadersFiltered` (`Referer: …/reset?token=abc&x=1` → `…/reset?token=[FILTERED]&x=1`, userinfo and fragment removed, `X-Original-Url` and the other treated names, an unparsable `Referer` absent, `Origin` kept, `Link` and `X-Forwarded-Uri` absent, no marker anywhere in the body) and five mutations; the CHANGELOG's headers line states it. | §6.2 (headers, the URL-valued header paragraph); §7.1; §9 (`middleware.go`); §10.2 P3.5; §11.2 decision 29 (new) |

Spec review round 10 (against `f66a4d5`): round 9's three fixes were verified; 0 majors, two minors, both fixed —
the first by the coordinator's binding direction, which replaces the marker replay rather than repairing it again
(it had needed a fix in rounds 7, 8, 9 and 10 because it predicted the server's cut byte for byte); none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc10-marker-replay-overcounts-server-markers` | minor | **Size model replaced: "apply with headroom, then measure what is sent"** (coordinator direction). The transport now applies the server's own bounds to `metadata` and `contexts` client-side, 4,096 bytes under its 65,536: `contexts` in `boundContexts`' order (U+0000 stripped at every depth with `nulMap`'s collision rule, invalid names dropped, then ≤ 32 names and ≤ 61,440 bytes, non-well-known and largest first); `metadata` without a client's `server_truncated*`, then shrunk to ≤ 61,440 bytes in `shrinkMetadata`'s order (non-exempt, largest first, ties to the smallest key) measured **with the SDK's own marker**, the JS SDK's `truncated` / `truncated_keys` in `server_truncated*`'s shape (first 50 keys cut to 128 bytes, then `+N more`; collided and duplicate names recorded by `appendLost`'s rule, once). U+0000 is stripped, and key collisions resolved the server's way, in every other place the server strips it (tags, headers, env, `request.data`, crumb `data`), but not in `metadata`, which the server does not strip (§12 F11 records what that may cost server-side). Every size of those four `Struct`-typed fields is measured on the decoded value, and the decoded value is what is sent, so the server re-decodes exactly what was measured. **The margin** is proved in §3.5: the server's metadata shrink runs only over 65,536 with its markers, and for a bounded event those are empty — `server_truncated_keys` is written only by the three key-removing cuts, which §3.4 has already made — and at most `server_truncated` with all 35 fields, 558 bytes of JSON; 61,440 + 558 ≤ 65,536. `contexts` gains only enrichment's `User-Agent` contexts, which `ContextFits` inserts only when nothing is evicted. **The publish cost** is now the protobuf of exactly what is sent — parts 1 and 4-5 unchanged, parts 2-3 `ld(S)` of `metadata` and `contexts` as sent — with `event_id` and `project_key` counted as sent (`event_id` because §3.4 now re-mints an id the server would re-mint; `project_key`'s replacement name is in the allowance), plus the fixed 48,576-byte allowance (`max_payload` − 1,000,000) for what the ingestor adds, markers now ≤ 599 bytes of it. Deleted: the replay of `shrinkMetadata` and `boundContexts`, the marker prediction, T41's **Markers** paragraph and T40's replay measurements, and the tests that served only them (`TestShrinkReplayKeepsExempt`, `TestShrinkReplayCountsMarkers`, `TestPublishCostCountsServerIdentifiers`, `TestServerShrunkFieldsSentWhole` in its round-8 form, the shrunk golden's server-marker expectation). Measured (**run**, T40): the reviewer's three runs as the SDK now sends them publish 826,463 / 337,450 / 651,709 bytes with **no** server marker and the payload deep-equal after `ValidatePayload`, estimated 830,747 / 337,637 / 655,919; the estimate is never under on 2,000 random events. P1.11 gains `TestServerCutsNothing`, one case per run: (1) the collision resolved to `a`'s value, `m` removed client-side, every header kept, sent under the limit; (2) the duplicate empty names recorded once, so a `metadata` sized to exactly 61,440 with one record keeps `m`; (3) `request.data` sent without its U+0000, `m` removed, nothing shed. P1.6 gains `TestBoundContexts`, `TestBoundMetadata` (exempt last, ties, the exact 61,440 boundary, `+N more`, decoded sizes), `TestBoundDataNul`; P1.8's root half now requires **no** server marker on all six goldens — a new `wire_v2_nul.golden.json` with collisions, U+0000 at depth, `metadata` and `contexts` at the margin and number-dense data, and the shrunk golden, which the SDK now shrinks itself — and `n <= cost` with no marker deletion. The CHANGELOG states the trade: up to 4 KiB less `metadata` and `contexts` than the server keeps. A value in `metadata` or `contexts` that cannot be marshalled or decoded now removes its entry (recorded) instead of dropping the event (decision 28), since the SDK now removes entries of both anyway. | Header; §1.2 T39, T40, T41; §1.3 D3; §2 item 6, "Does not"; §3.4 (intro, `tags`, `request.*`, `request.data`, `breadcrumbs` rows, new `event_id`, `timestamp`, `contexts`, `metadata` rows, "The SDK's marker", "Not bounded client-side", rule table, closing paragraph); §3.5 (rewritten: steps 1-4, the publish cost, the allowance, the margin, the no-write paragraph); §4.2 (marker-key rows); §7.1 (the trade, timestamps and ids, entry removal); §8 guarantee 3; §9 (`bounds.go`, `publishcost.go`, `transport.go`, goldens); §10.1 (expected drops); §10.2 P1.6, P1.8, P1.10, P1.11; §11.2 decisions 26, 28; §11.3; §12 F2, F7, F10, F11 (new) |
| `sdkgo-rc10-out-of-range-timestamp-drops-event` | minor | Verified: `time.Time.MarshalJSON` fails for a year outside [0, 9999] and for a zone offset of 24 hours or more (T41). §3.4 gains a `timestamp` row — `.UTC()`, then a year outside [0, 9999] → `time.Now().UTC()`, as the ingestor would clamp it (`resolveTimestamp` R2/R3) — and the `breadcrumbs` row normalises each crumb's with `.UTC()` and replaces a zero or out-of-range one with the event's. P1.6 gains `TestBoundEventTimestamps`: the reviewer's case, a crumb at year 58,706 and an event at year 10,000, is sent with `droppedCount 0`, plus a 25-hour offset, a zero crumb timestamp and the year bounds; mutation "no timestamp rule" drops the event. **The audit** the direction asks for: no other field of `Event` or `Breadcrumb` has a type whose `MarshalJSON` can fail on a valid Go value (strings, `int`, `uint32`, `bool`, `map[string]string`, `[]Frame`; T41); the `interface{}`-typed `metadata`, `contexts`, `request.data` and crumb `data` can hold any value — an out-of-range `time.Time` included, beside `func`, `chan`, complex, `NaN`/±Inf, cyclic values, invalid `json.Number` / `json.RawMessage` and failing custom marshalers — and §3.5 step 2 now removes just the entry holding it, in all four fields, so no such value costs its event (`TestUnmarshalableEntryRemovedAlone` puts a year-10,000 `time.Time` in `metadata`). `event_id`, the one typed identifier the server replaces, gets the same client-side treatment (§3.4 `event_id` row, `TestBoundEventID`). | §1.2 T41; §3.4 (`timestamp`, `event_id`, `breadcrumbs` rows, closing paragraph); §3.5 step 2; §7.1 ("Timestamps and ids"); §8 guarantee 3; §10.2 P1.6; §11.2 decision 28 |

Spec review round 11 (against `c184023`): the size model passed with no finding; 0 majors, one minor, fixed in §10.2
only; none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc11-golden-cost-machine-dependent` | minor | (1) **The golden rule**, stated once at the head of §10.2: a test that compares with a committed file or integer uses P1.7's fixed `Config` and, after `NewEvent` (and `ApplyContext`) and **before `Push`**, assigns `EventID` (`00000000-0000-4000-8000-00000000000n`, equal length per event), `Timestamp` and every crumb timestamp (`2026-09-26T12:00:00Z`), the whole `Stacktrace`, and synthetic `contexts.runtime.version` / `os.name` / `device.arch` (`0.0.0-golden`, `golden-os`, `golden-arch`, so a golden regenerated without the assignment fails on the other leg); nothing in the received body is replaced. So the body the SDK bounded and measured is the body compared, and `wire_v2_publish_cost.json`'s integers hold on the go1.21.13 leg, the current-toolchain leg and any machine (`runtime.Version()`, GOOS, GOARCH and the hostname all vary in length). Re-checked against it: P1.7 (its post-hoc replacement of `event_id`, `timestamp`, crumb timestamps, `stacktrace` and the three context values is gone — assigned before `Push` instead); P1.8 (all six goldens, the bounded test's 120 frames included, cite the rule; the earlier crumb-timestamp/stacktrace-only sentence is replaced); P1.11 (every case follows the rule, so the sizes it computes before `Push` are the sizes sent and its quoted figures reproduce on both legs; the distinct fixed ids give the batching cases their push order). (2) `TestBoundDataNul`: `json.Marshal` writes U+0000 as `\u0000`, 6 bytes, so the case is a `request.data` of **16,390** bytes with one U+0000, **16,384** once stripped (both asserted), and the mutation "measure before stripping" names the 16,390-byte case. **The audit** of every other §10.2 size involving an escaped character: the zero-list, `json.Number` and marker sizes involve none and are unchanged (re-derived: 65,503 / 65,535 / 60,007 / 65,427 bytes); U+1F600 is 4 bytes in JSON and proto, unchanged; `'<'` is 6 bytes wherever a size is JSON (`jsonSize` is `json.Marshal`) and 1 in the publish cost — so P1.11 now states that an **exact** JSON size is reached with `'<'` and then 0-5 ASCII `a` (`TestBatchCutBySize`'s exactly-1,048,576 events and the exactly-5,242,880 event could not be hit in steps of 6), `TestLoneEventOverOneMiBSentWhole` gives the arithmetic (64 × 4,096 × 6 = 1,572,864 for the values; 1,573,569 is T38's whole event), and `TestShedOrder`'s 5 MiB (b) and (c) size `request.data` and crumbs of `'<'` in JSON (~2,700 characters near 16,384; ~49 KB for an 8,192-character crumb), so neither is dropped client-side before the case reaches its step. | Header (revision); §10.2 (the golden rule; P1.6 `TestBoundDataNul` and its mutation; P1.7; P1.8; P1.11 intro, `TestLoneEventOverOneMiBSentWhole`, `TestShedOrder` 5 MiB (b), (c) and the exact case, `TestBatchCutBySize`) |

Spec review round 12 (against `8b932f0`): 0 majors, two minors, both fixed; none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc12-invalid-utf8-size-divergence` | minor | One rule added to §3.5 step 1, before any typed row runs: every invalid UTF-8 byte of every typed string — and of every key and value of `tags`, `request.headers` and `request.env`, the maps the SDK bounds as strings — becomes **one U+FFFD per byte**, as `json.Marshal` writes it (`validUTF8`, not `strings.ToValidUTF8`, which collapses a run to one); a key the replacement makes collide is resolved by the U+0000 rule (the unreplaced key, else the lexically smallest). So every byte and code point the SDK measures is the server's. The four decoded fields (`metadata`, `contexts`, `request.data`, crumb `data`, keys included) need no rule: step 2's marshal and decode already make the replacement and step 3 sends that form. Swept: §3.4's intro, `message` row and `user.id` row (a 255-byte id holding `0xff` is **dropped**, 257 bytes as the server measures; a 253-byte one is sent as its 255-byte U+FFFD form); §3.5 step 3 and publish-cost part 1; the CHANGELOG. P1.6 gains `TestBoundEventInvalidUTF8`: both `user.id` cases, a two-byte run in a tag value (two U+FFFD), a header-key collision, and a crumb list with invalid bytes at exactly 131,072 as the server measures it (every crumb kept; 131,073 loses the oldest), with mutations for no replacement, `ToValidUTF8`, and replacing after the rows | §3.4, §3.5 step 1, §7.1, §10.2 P1.6 |
| `sdkgo-rc12-metadata-map-in-slice-shared` | minor | `ScrubPII` becomes a deep copy through maps **and** `[]interface{}` at any depth, scrubbing every map it rebuilds (so `{"items": [{"password": "x"}]}` is now filtered, stated in the CHANGELOG); other reference kinds (typed slices, `map[string]string`, pointers, structs) stay shared. The change would otherwise turn a self-holding list into a stack overflow (a self-holding map already is one today): `ScrubPII` keeps the maps and lists on its path and copies one it meets again by reference, and §3.5 step 2 removes that entry from what is sent. §8 guarantee 1's row now reads "`map[string]interface{}` or `[]interface{}`, at any depth — a map inside a list included" and is true; T37 records today's behaviour and the change; §6.1's list, §9 and §11.2 item 21 updated. Checked for every other field and stated in §8: `Contexts` holds no SDK-built list (its three SDK maps hold scalars; other contexts are the caller's), `Request.Data` is never set by the SDK (the caller's), and below a crumb's top-level `Data` a map inside a list is shared exactly as a map inside a map (§5.2, by design). P1.10 adds `WithContextMap({"items": []interface{}{map{"k": "v"}}})` to `TestEventReferencesArePerEvent`, `TestMetadataListRaceBeforeSend` under `-race`, and `TestScrubPIIThroughLists` (scrubbing through lists, typed slice shared, both cycles return and are removed from the body) | T37, §6.1, §6.2 item 3, §7.1, §8 guarantee 1, §9, §10.2 P1.10, §11.2 item 21 |

Repair round 13 (against `07013b8`, orchestration brief): two minors and one note, all fixed; none is dismissed.

| Finding | Severity | Fix | Where |
|---|---|---|---|
| `sdkgo-rc13-tenant-id-dropped-by-50-tag-cap` | minor | Over 50 tags the bounds step keeps 50: `tenant_id` when present, plus the lexically smallest other keys (49 beside it, else 50, `sort.Strings` order) — the sibling sdk-js spec's §3.5 rule 6. Without `tenant_id` this is exactly the server's cut; with it the server receives 50 keys and cuts nothing, and the processor's affected tenant (`tags["tenant_id"]`, T11), which metadata no longer carries, survives 50 keys that sort before it. The CHANGELOG states it. New P1.6 case `TestBoundTagsKeepsTenantID` (`WithTenant` + `k00`…`k49` → `tenant_id` and `k00`…`k48`; without `WithTenant` → `k00`…`k49`); mutation "plain 50 smallest" → `tenant_id` absent. | §3.4 `tags` row, §4.2 (`WithTenant` row, closing paragraph), §7.1, §10.2 P1.6, §11.2 item 12 |
| `sdkgo-rc13-taskfile-hunk-collides-with-sdk-js` | minor | The `test-unit` line `cd packages/sdk-go && GOWORK=off go test -race -count=1 ./...` is one new `cmds` line **directly after `go test ./apps/ingestor-go/...`**, not last: the sibling sdk-js run edits the two `git …` tunnel-fixture drift lines at the end. The `docs/sdk-specification.md` §5 sdk-go note is a new sub-heading, `### sdk-go: headers and query parameters`, inserted directly after the last bullet of `### What the server does NOT mask`; no existing line of §5 is edited, and the sdk-js URL-filter text is not written under it. | §1.3 D5, §9 (`Taskfile.yml`, `docs/sdk-specification.md` rows), §10.1 |
| `sdkgo-rc13-query-filter-edges-undefined` | note | `query_string` (and so every URL-valued header's query) is split on `&` **and** `;`, each part keeping the separator that followed it (parity with sdk-js §5.1 rule 2); a part's name is up to its first `=`, or the whole part; a name `url.QueryUnescape` rejects is matched on its raw form. P3.5 gains `?a=1;reset_token=X&b=2` → `a=1;reset_token=[FILTERED]&b=2` and `?api_key%zz=s&id=7` → `api_key%zz=[FILTERED]&id=7`, with mutations "split on `&` only" and "skip a name that does not unescape". | §6.2 (`query_string`), §10.2 P3.5 |
