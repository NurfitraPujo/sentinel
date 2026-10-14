# Sentinel Sentry-compatible ingest (envelope + store endpoints, Sentry → v2 mapper, Dart grouping, Sentry project id): implementation specification

**Feature:** `sentinel-sentry-compat-ingest` · **Repository:** `sentinel` (`tuju-ai/sentinel`), side `be`
(`apps/ingestor-go`; `apps/processor-go` is **not** changed, §7) and side `fe` (`apps/dashboard-web`, TASK-06 only) ·
**Base:** `main` @ `2812b23` (v0.2.39 production, v0.2.40-staging)
**Built against:** `prd/sentinel-sentry-compat-ingest/contract.md`, `be.md` and `fe.md` on prd `main` @ **`c14e9b9`**
(tuju-ai/prd#803, #804: both amendments; tuju-ai/prd#806: spec review 1's rulings, contract D13-D15 — all merged).
If this document and the contract disagree, the contract wins, except where §1.3 records that the contract's
description of the code is wrong or cannot be implemented as written. Each such row says what this spec does
instead and why.
**Revision:** 6 (2026-09-29) — planning (`plan.md`): header re-pinned to prd `c14e9b9` (`header-prd806-pending`);
§13 F5 corrected: a `401` is retryable for the relay (`f5-401-final-vs-relay`). No other change. Revision 5 (2026-09-27) — **spec review 1 repaired** under the coordinator's rulings (prd#806):
`nats-max-payload-unbounded` → D13, a publish budget on compat routes with a fixed shed order and `413` (§4.7,
P4.8-P4.10); `task07-cidrs-not-named` → D15, the compat CIDR list ships empty in `values.yaml` and every overlay, and
the RUNBOOK says how core's relay rollout derives it (§3.6.3-§3.6.4, P2.13); `task07-untrusted-proof-gap` → P2.11's
untrusted case; `relay-tags-body-spoofable` → D14, a body `tags.relay_auth` is always removed and `anonymous` also
removes the body's `user.id` / `tags.tenant_id` (§3.7.4, P7.9-P7.11); `stale-amendment-caveats` → this header and
§1.4 AM3/AM7/AM8, §12.3; `compat-cidr-error-text` → `Resolvers` names `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS` in its
errors (§3.6.1, P2.11). Revision 4 (2026-09-27) — **orchestrator amendment 2** (operator-approved design, prd#804, contract §4.7 / D12, card TASK-07): relay identity headers on compat routes, honoured only from a trusted-proxy peer (§3.7, §1.4 AM8, P7.x). Revision 3 (2026-09-27) — an independent review's 16 findings fixed (T21 narrowed to what sentry-dart itself does, since phones send through the native SDK; U+0000 stripped at decode; per-entry free-form decoding; one metric counting rule; the importable compat chain and resolvers so the wiring is proven in `tests/unit`; fixture anonymization completed; mutations made to bite). Revision 2 (2026-09-27) — the **orchestrator amendment** (operator decision 2026-09-27, contract §9 Q1 answered:
phones reach Sentinel through a relay in core, `core-sentinel-mobile-relay`) is applied; §1.4 records it. TASK-06
shrinks to the derived Sentry project id on project settings (no DSN display, no dashboard host setting), and side `be`
gains a compat-only trusted-proxy list so the ingestor believes the relay's `X-Forwarded-For` (§3.6). Revision 1: first
draft.
**Layout:** the brief names `sentinel-issue-tag-facets/spec.md` on `main` as the layout to follow. That file does not
exist: `git ls-tree origin/main sentinel-issue-tag-facets/` holds only `plan.md` (**run**). This document follows the
most recent spec on `main` instead, `sentinel-sdk-go-rich-context/spec.md` (problem → verified facts → deviations →
design by card → files → verification with criterion → proof → mutation → decisions → findings).

**Cards:** `TASK-02` (routes, Sentry key presentation, ingest-only keys, derived project id, Sentry rate-limit headers,
§3), `TASK-03` (body decoding, envelope/store parsing, item handling, metric, §4), `TASK-04` (Sentry event → v2
mapper, §5), `TASK-05` (Dart grouping, §6), `TASK-06` (the derived Sentry project id on project settings, §8; shrunk by §1.4). The relay's
forwarded client address (§3.6) belongs to `TASK-02` (routes and auth); the relay identity headers (§3.7) are
`TASK-07` (contract §4.7, D12, added by amendment 2). Docs are §9.
§11 maps every acceptance criterion to the command that proves it and the mutation that must turn that proof red.
**Outcome of the open questions:** no `NEEDS_DECISION`. Every rule the contract leaves to the spec is fixed in §3-§9
and listed in §12.1. §1.3 records six places where the contract's words and the code disagree; none of them needs a
product decision, and each says what this spec does. §1.4 records the orchestrator's amendments; the one choice the
first left to this spec (a compat-only trusted-proxy list rather than widening `TRUSTED_PROXY_CIDRS`, AM3) is ratified
as contract D10.

Every "today" claim cites a path and a symbol at `2812b23` (line numbers are approximate, `~L`, and never the only
identifier). Commands marked **run** were executed by this seat on 2026-09-27 in a clean worktree of `2812b23`.

**Risk surfaces:** **auth** — two new ways to present an API key (`X-Sentry-Auth`, `?sentry_key=`), on two new routes
only (§3.2); **credentials** — compat routes accept only project-scoped `ingest` keys (§3.3, D1); after the amendment
the key lives in core's environment, not in a mobile binary (§1.4), and D1 stands as the limit on what a leaked relay
key can do; **trust boundary** — a new, compat-only list of proxy ranges whose `X-Forwarded-For` and `X-Sentinel-Relay-*` identity
headers are believed (§3.6, §3.7), shipped empty (D15);
**untrusted compressed input** — gzip/deflate bodies behind a counting reader (§4.1). No tenancy
change (tenant scope still derives from the credential, B7), no migration, no payment surface. The surfaces are the
contract's declaration; D13-D15 add none (D14 narrows what the body can claim, D15 ships the trust list empty).
**No measurement owed:** the contract claims no performance outcome, and the new write path feeds the existing
`IngestAt` pipeline, unchanged but for the compat-only publish budget (§4.7). Volume is bounded by `TASK-01`'s cost check (daya-mobile: median 15 error events/day,
90-day peak 12,471/day; transactions are read past and discarded, §4.4). The per-request CPU a hostile body can cost is
bounded by the caps of §4.1 (20 MiB decompressed at most), which a unit test proves (§11, P3.4); the publish-budget
shed (§4.7) adds at most `MaxShedRounds` re-marshals of one over-budget event, and none for an event under it.
**No infra/alerting changes owed:** no service, queue, subject, bucket or job is added; events publish on the existing
`error_events` subject, and every compat message is held under NATS `max_payload` by the 1,000,000-byte publish budget
the SDKs already use (§4.7, D13), so no server setting changes. One counter is added (`sentinel_compat_items_total`,
§4.6) without an alert rule, because the traffic it counts does not exist until `mobile-sentinel-dual-write` ships, and
an alert threshold on zero traffic is invented. The only chart change is one ingestor value,
`config.ingestor.sentryCompatTrustedProxyCidrs`, shipped **empty in `values.yaml` and in every overlay** (§3.6.3;
contract §4.7, D15), which fails closed; `core-sentinel-mobile-relay` sets it per environment at its rollout, from the
RUNBOOK entry this change adds (§3.6.4). No ingress change: core reaches the ingestor on the private network it already
uses.

---

## 1. The problem

### 1.1 Who has it

The mobile team (daya, zasri and tuju Flutter apps) reports errors to Sentry only. The operator decided (2026-09-23)
that mobile reaches Sentinel through a Sentry-compatible endpoint, Sentry stays primary, and the app writes to both
(contract §1). Since the operator's decision of 2026-09-27 (§1.4), the phones' envelopes reach the ingestor through a
relay in core, which presents a project `ingest` key as `X-Sentry-Auth` and forwards the phone's address and
User-Agent. Today the ingestor cannot accept a single Sentry request: it has no route for one, does not read the
headers a Sentry client authenticates with, has no numeric project id to put in a DSN, cannot read a compressed body,
and rejects Sentry's field shapes (§1.2 T1-T11). The person who triages a Flutter crash in the dashboard therefore sees
nothing.

The measure the contract sets: fixture envelopes from sentry_flutter 9.6 and @sentry/nextjs 10 are accepted; a real
Flutter event round-trips with every field in its v2 column; the same error from two devices is one issue; a duplicate
delivery is one occurrence; a limited client sees `X-Sentry-Rate-Limits`.

### 1.2 What is true today (verified)

| # | Fact | Evidence |
|---|---|---|
| T1 | The ingestor registers `/ingest`, `/ingest/batch`, `/health` and (when providers exist) `/metrics` on the **default** `ServeMux` with method-less patterns, and serves it with `http.Server{Handler: nil}`. Nothing is under `/api/`. The root module declares `go 1.25.8`, so Go 1.22+ pattern syntax (a method prefix, `{name}` wildcards, `{$}`) is available and `httpmuxgo121` defaults to `0`: a request whose path matches a pattern registered only for other methods is answered `405` with an `Allow` header by the mux itself. | `apps/ingestor-go/main.go: main` (`http.Handle("/ingest", …)` ~L288-294, `srv := &http.Server{Addr: addr, Handler: nil}` ~L306); `go.mod` (`go 1.25.8`); `net/http/server.go: (*ServeMux).findHandler` (the 405 branch). **Run** (go1.26.4, a scratch module declaring `go 1.25.8`, §3.1's four patterns beside method-less `/ingest` and `/ingest/batch`): both spellings of a `POST` reach the handler with `PathValue("project_id")` intact after `r.WithContext`; `GET`, `HEAD`, `OPTIONS` → `405`, `Allow: POST`; `/api/1/envelope/x`, `/api/1/envelopes/`, `/api/envelope/` → `404`; `/api//envelope/` → `307` to the cleaned path (the mux's own path cleaning); `GET /ingest` still reaches `/ingest`. |
| T2 | Each ingest route's chain is, outermost first: `otelhttp.NewHandler(…, "<fixed operation name>")` → `observeMiddleware` (sets `X-Request-Id`, records `sentinel_ingest_requests_total{outcome}` from the final status) → `clientip.Middleware` → `authenticator.Middleware(auth.ScopesIngest, …)` → `rateLimiter.Middleware` → handler. | `apps/ingestor-go/main.go: ingestChain, batchChain, observeMiddleware, outcomeForStatus` (~L258-289, ~L396-414) |
| T3 | The key is read **only** from `X-API-Key`. Missing → `401 "Missing API key"`; `sha256` hex of the key → `a.lookup` (Redis `apikey:<hash>` cache, then Postgres `project_api_keys` joined to `projects`, `expires_at` enforced in SQL, `status` must be `active`) → error → `401 "Invalid API key"`; scope not on the chain's `ScopeSet` → `403 "Forbidden"`. A key is organization-wide when `project_id` is NULL or the joined project name is empty; it then resolves a target from `X-Project-Key`. `WithIdentity` stores project name, project UUID, org id, key hash, `rate_limit_rpm` and the org-wide flag under typed context keys. `ScopesIngest` is `{ingest, admin}`. `api_key.invalidated` deletes the Redis entry. | `apps/ingestor-go/auth/apikey.go: (*APIKeyAuthenticator).Middleware` (~L112-162), `getAPIKeyData`, `ScopesIngest` (~L68), `NewAPIKeyAuthenticator`; `apps/ingestor-go/auth/context.go: WithIdentity` |
| T4 | The dashboard mints keys as `sent_live_` (project-scoped), `sent_org_` (org-wide) or `sent_agent_` (scope `agent`, always org-wide) followed by 64 lowercase hex characters, stores only `sha256(token)` and returns the raw token once, in the create and rotate responses (`{ key: toPublicKey(row), token }`). `toPublicKey` strips only `keyHash`, so the response's `key` carries `scope` and `projectId`. Scopes in the database: `ingest`, `read`, `admin`, `agent`, `report`. | `apps/dashboard-web/src/lib/db/queries/apikeys.ts: createApiKey` (the `prefix` line ~L108), `toPublicKey` (~L336); `src/routes/api/organizations/[orgId]/keys/+server.ts: POST` (~L146); `.../keys/[keyId]/rotate/+server.ts: POST` |
| T5 | A limited request gets `429`, `Retry-After: 60`, `X-RateLimit-Limit`, `X-RateLimit-Reset`, `X-RateLimit-Remaining: 0` and the body `Rate limit exceeded`; no `X-Sentry-Rate-Limits`. The window is one minute per key hash (`ratelimit:<hash>`, Lua sliding window, default 5000 rpm when the key has none). A Redis failure goes through `failOrPass`: `500` unless `RATELIMIT_STRICT_MODE=false`. | `apps/ingestor-go/middleware/ratelimit.go: (*RateLimiter).Middleware` (the `allowed == 0` branch ~L153-157), `failOrPass`, `isStrictMode`, `slidingWindowScript` |
| T6 | Nothing in `apps/ingestor-go` reads `Content-Encoding` or imports `compress/*` (`rg -n 'Content-Encoding|gzip|zlib|flate' apps/ingestor-go`, **run**: no hit in Go code). `klauspost/compress` is only an indirect dependency; the standard library's `compress/gzip`, `compress/zlib`, `compress/flate` are what this spec uses. | `go.mod` (`github.com/klauspost/compress v1.18.5 // indirect`) |
| T7 | `validation.ErrorPayload`'s wire types: `event_id`, `project_key`, `platform`, `environment`, `message`, `error_class`, `trace_id`, `span_id`, `release_version`, `fingerprint`, `project_id`, `level`, `logger`, `server_name`, `transaction`, `dist`, `source` are `string`; `timestamp` is `time.Time` (RFC 3339 only); `stacktrace []StackFrame`; `metadata map[string]interface{}`; `sdk *SDKInfo{name, version}`; `user *UserInfo{id, username, email, ip_address, geo *GeoInfo{city, region, country_code, subdivision}}`; `tags map[string]string`; `contexts map[string]map[string]interface{}`; `request *RequestInfo{method, url, query_string string; headers map[string]string; data interface{}; env map[string]string}`; `breadcrumbs []Breadcrumb{timestamp time.Time, type, category, level, message string, data map[string]interface{}}`; `exception *ExceptionInterface{values []ExceptionValue{type, value string; mechanism *Mechanism{type string, handled *bool}; stacktrace []StackFrame}}`. `StackFrame{file string, line int32, function string, in_app bool, column int32, module, abs_path, context_line string, pre_context, post_context []string}`. `ExceptionValue` has **no** `module` field. `received_at` is `json:"-"`. | `apps/ingestor-go/validation/validator.go: ErrorPayload, SDKInfo, UserInfo, GeoInfo, RequestInfo, Breadcrumb, ExceptionInterface, ExceptionValue, Mechanism, StackFrame` (~L25-173); `docs/sdk-specification.md` §3 |
| T8 | `service.IngestAt(ctx, payload, receivedAt)`: `resolveEventID` (empty, over 64 code points, or holding a rune `< 0x20` / `0x7f` → a minted UUIDv4; otherwise verbatim), `resolveTimestamp` (zero, more than `MaxFutureSkew` = 60 s ahead or more than `MaxEventAge` = 30 days behind `receivedAt` → `receivedAt`), sets `ReceivedAt`, runs `enrich.ApplyWithBudget`, `validation.ValidatePayload`, `mapping.MapPayloadToEvent`, protovalidate, `proto.Marshal` and one JetStream publish on `error_events`. It returns an error prefixed `validation failed` for a bounds or protovalidate rejection and a bare error for a marshal or publish failure. `handleIngest` maps the prefix to `400` and everything else to `500`; `applyAuthenticatedScope` runs first and maps its error to `403`. | `apps/ingestor-go/service/service.go: IngestAt` (~L253-314), `resolveEventID`, `resolveTimestamp`, `MaxFutureSkew`, `MaxEventAge` (~L44-45); `apps/ingestor-go/main.go: handleIngest` (~L452-513), `applyAuthenticatedScope` (~L648-691) |
| T9 | **What still rejects an event built in Go** (so what the mapper must prevent). `ValidatePayload` bounds every v2 value in place and rejects only a **negative top-level** `stacktrace[i].column`. Everything else is protovalidate's, and each is a 400 for the whole event: `project_key` empty or over 64; `platform` not `^[a-z0-9]+$` or over 50; `environment` not `^[a-z0-9][a-z0-9._-]{0,49}$`; `message` over 10,000 code points; `error_class` empty (`required`) or over 255; `trace_id`, `span_id`, `fingerprint`, `event_id` over 64; `release_version` over 100; top-level `stacktrace` over 100 frames; any frame (top-level **or** chain) with `in_app` true and an empty `file` (`stack_frame.file_required`) — the bounds step repairs a chain frame's, never a top-level one's. `proto.Marshal` additionally fails (a bare error → `500`) on a proto3 string that is not valid UTF-8. | `packages/proto/sentinel/v1/error_event.proto` (every `cel` rule; `repeated.max_items = 100` on `stacktrace`); `apps/ingestor-go/validation/validator.go: ValidatePayload`; `apps/ingestor-go/validation/bounds.go: applyBounds, boundException` (the `f.InApp && f.File == ""` repair, chain only) |
| T10 | **What the server repairs and never rejects**: an invalid `level` or crumb `level` → `""` (stored as `error`, marked); `logger`/`server_name`/`transaction`/`dist`/`source`/`sdk.*`/`user.*` over bound → truncated or dropped; `tags` entries failing `^[a-zA-Z0-9_.:-]{1,32}$`, empty, over 200 code points or containing `\n` → dropped, 50 kept; `contexts` → U+0000 stripped, names 0 or over 64 code points dropped, ≤ 32 and ≤ 64 KiB; `request` maps and `data` bounded; `breadcrumbs` → the **last** 100, each field bounded, `data` over 4 KiB → nil, oldest dropped while over 128 KiB; `exception.values` → the **last** 20, chain frames → the **first** 100 (crash end), chain `file`/`function` truncated, chain `in_app` without file → false, chain `column < 0` → 0; every frame's `module`, `abs_path`, `context_line` truncated and `pre_context`/`post_context` cut to 5 lines. Each cut is recorded in `metadata.server_truncated`. `metadata` over 64 KiB is shrunk key by key. | `apps/ingestor-go/validation/bounds.go: applyBounds, boundTags, boundContexts, boundRequest, boundBreadcrumbs, boundException, boundFrameV2, shrinkMetadata`, the const block (~L20-99) |
| T11 | `mapping.MapPayloadToEvent` builds `metadata`, `contexts`, `request.data` and crumb `data` with `structpb.NewStruct` / `NewValue` and **ignores the error**: on failure `metadata` becomes nil, a context becomes `{}`, `request.data` and crumb `data` become nil — silently. `structpb.NewValue` (protobuf-go v1.36.11) accepts only `nil`, `bool`, the Go integer and float kinds, `json.Number`, valid-UTF-8 `string`, `[]byte`, `map[string]any` and `[]any`; a `map[string]string`, a `[]string` or a struct is `invalid type`. The comment on `mapContexts` ("decoded JSON cannot produce a value the constructor rejects") holds only because `/ingest` values come out of `encoding/json`. A crumb with a zero `Timestamp` is published without one. | `apps/ingestor-go/mapping/mapper.go: MapPayloadToEvent, mapContexts, mapRequest, mapBreadcrumbs`; `google.golang.org/protobuf@v1.36.11/types/known/structpb/struct.pb.go: NewValue` |
| T12 | Enrichment treats `dart` (and `flutter`, `cocoa`, `objc`, `browser`, non-server `javascript`) as a **client** platform: a `user.ip_address` of exactly `{{auto}}` becomes the caller's public IP (I1) or `""` when there is none (I2); a client platform with no IP gets the caller's (I3); `user.geo` is filled from the IP only when the body sent no `geo`; the connection's `User-Agent` is parsed into `contexts.browser` / `contexts.os` only when those are absent. | `apps/ingestor-go/enrich/enrich.go: IsClientPlatform` (~L136-146), `applyIP`, `applyGeo`, `userAgent`, `AutoIP` |
| T13 | **Grouping** (algorithm `v2`): `ComputeFingerprint` returns a non-empty client `fingerprint` verbatim (unless `fingerprint_override`), else `fingerprint.Compute` hashes `v2 | environment | normalize(error_class) | ` the first 5 usable frames of the **top-level** `stacktrace` **in array order** — library and platform frames skipped whatever `in_app` says (Dart: every `package:` other than the one whose `main.dart` is in the stack, else a known-library list), then the `in_app` frames, else any — as `file:function`, and returns 16 hex. The normalised message is appended when no usable frame exists, the top frame is a known wrapper, or (gated platforms) every hashed frame is unsymbolicated; an unsymbolicated gated frame contributes its build-hash-normalised file only, never its function. `line` is never an input. The build-hash path normalisation runs only for platforms in `SYMBOLICATE_PLATFORMS` (unset = `javascript`, `browser`), so a `dart` frame's `file` is hashed byte for byte. Issue identity is the unique `(project_id, environment, fingerprint)`. The exception chain never groups. | `apps/processor-go/fingerprint/fingerprint.go: Compute, normalises`; `apps/processor-go/fingerprint/grouping.go: MaxFrames, AlgorithmVersion, dartAppPackage, isLibraryFrame`; `apps/processor-go/event/event.go: ComputeFingerprint`; `docs/sdk-specification.md` §3 "Field semantics" |
| T14 | **Idempotency**: the processor stores one occurrence per `(project_id, event_id)` (`uq_error_occurrences_project_event`); a redelivery is recorded as `sentinel_process_events_total{outcome="duplicate"}` and changes no counter. Proven today by `tests/integration/event_idempotency_test.go: TestEventIdempotency_SameEventTwiceSequentially`, `TestStoreEvent_DedupProject_CrossIssueRedeliveryStoresNothing` (CI job `integration`) and `tests/e2e/idempotency_test.go: TestU36_EventIdempotency` (CI job `e2e`). | `packages/db-migrations/migrations/1725200000_add_error_occurrences_project_id_dedup.sql`; the named tests |
| T15 | v2 values land in `error_occurrences` columns `level`, `message`, `error_class`, `logger`, `server_name`, `transaction`, `dist`, `source`, `trace_flags`, `received_at`, `sdk`, `event_user`, `tags`, `contexts`, `request`, `breadcrumbs`, `exception` (JSONB for the structured ones), beside the pre-v2 `platform`, `environment`, `release_version`, `trace_id`, `span_id`, `stacktrace`, `metadata`, `timestamp`, `event_id`, `project_id`. | `packages/db-migrations/migrations/1725300000_add_error_occurrences_event_v2_fields.sql` and earlier |
| T16 | **CI gating**: `go-root` runs `go build ./...`, `go vet ./...` and, **only when the event is not `pull_request`**, `go test ./tests/unit/... -count=1` (GOWORK=off). **No CI step runs `go test ./apps/ingestor-go/...`**. The `dashboard` job runs `pnpm build` always and `pnpm check` / `pnpm test` only off `pull_request`. `integration`, `contract` and `e2e` run **only on `workflow_dispatch`**. `task test-unit` runs `go test ./tests/unit/... -v`, `go test ./apps/ingestor-go/...`, the sdk-go `-race` suite and both vitest suites. A dispatch of `ci.yml` on a branch runs every job (`gh workflow run ci.yml --ref <branch>`). | `.github/workflows/ci.yml: jobs.go-root` (~L78-106), `jobs.dashboard`, `jobs.integration`, `jobs.e2e` (guards `github.event_name == 'workflow_dispatch'`), header comment "PR scope (2026-09-06, operator)"; `Taskfile.yml: test-unit` (~L193-204) |
| T17 | `tests/unit` is one flat Go package in the root module. It already imports the ingestor's `validation`, `mapping`, `service`, `middleware`, `auth`, `enrich` and the processor's `event`, `fingerprint` packages, has a capturing `fakeEventPublisher` for `service.NewIngestService`, and uses `miniredis` for the rate limiter. `apps/dashboard-web`'s vitest includes `src/**/*.test.ts` and `tests/**/*.test.ts` (jsdom, `@testing-library/svelte`), and a test may read a file outside the app (`tests/tag-facets-mirror.test.ts` reads `packages/db-migrations/migrations`). | `tests/unit/ingestor_service_test.go: fakeEventPublisher`; `tests/unit/ingestor_middleware_test.go: newMiniRedisClient`; `apps/dashboard-web/vite.config.js` (`test.include`); `apps/dashboard-web/tests/tag-facets-mirror.test.ts` |
| T18 | The dashboard's project keys page shows the one-time token in its own `secret-tray` (`newlyCreatedToken`) after a successful create **or rotate**; `ApiKeyCreateModal` only collects name, scope (`ingest`, `read`, `admin`), rate limit and expiry, dispatches `create`, and is closed by the page on success — it never sees the token. The page passes `allowOrgWide={false}`, so every key it creates is project-scoped. | `apps/dashboard-web/src/routes/[orgSlug]/projects/[projectId]/settings/keys/+page.svelte: handleCreate, handleRotate`, the `secret-tray` block (the `secret-value` div ~L174); `src/lib/components/keys/ApiKeyCreateModal.svelte: availableScopes, handleSubmit` |
| T19 | Dashboard configuration is read server-side through `$env/dynamic/private` (55 imports); the chart feeds the dashboard pod from the shared ConfigMap by `envFrom` (`config.dashboard.*` values → `templates/configmap.yaml`), e.g. `INGESTOR_HEALTH_URL`. There is no dashboard setting that names a public ingestor URL. `tests/unit/helm_*_test.go` check chart values by parsing the YAML in Go (no `helm` binary). | `deploy/helm/sentinel/templates/configmap.yaml`; `templates/dashboard.yaml` (`envFrom: configMapRef`); `values.yaml: config.dashboard`; `tests/unit/helm_processor_memory_test.go: readHelmValues, helmPath` (the helpers; `helm_dashboard_cap_test.go` is a user of them) |
| T20 | `projects.id` is a `uuid` (`defaultRandom()` → `gen_random_uuid()`, version 4); the ingestor reads it as `pak.project_id` scanned into a Go string, which pgx renders as the canonical lowercase 36-character form. For a v4 UUID the 13th hex digit is the version nibble `4`, so the first 13 hex digits carry 48 random bits. | `apps/dashboard-web/src/lib/db/schema.ts: projects` (~L61-73); `apps/ingestor-go/auth/apikey.go: getAPIKeyData` |
| T21 | **What sentry-dart 9.6.0 produces and how its own HTTP transport behaves** (package source `~/.pub-cache/hosted/pub.dev/sentry-9.6.0`, **read**). **Scope of this row:** on Android and iOS, sentry_flutter 9.6.0 does **not** use this HTTP transport: `_initDefaultValues` installs `FileSystemTransport`, which serializes the envelope with the same `envelopeStream` below and hands the bytes to the native SDK (`_native.captureEnvelope`), so on phones the native SDK (sentry-java / sentry-cocoa) makes the request, with its own headers, compression, cache and retries (`sentry_flutter-9.6.0/lib/src/sentry_flutter.dart: _initDefaultValues`, `file_system_transport.dart: send`, **read**). After §1.4 no phone calls the ingestor at all; what this row still grounds is the **envelope bytes the Dart layer produces on every platform** and the header shapes a Sentry client uses. The rest of the row is the Dart HTTP transport (web and desktop): `Dsn.parse` takes the key from the URI's user info and the project id from the **last** path segment, without checking it is numeric; `postUri` is `<scheme>://<host>[:port]/[<leading path segments>/]api/<project_id>/envelope/`. Every request carries `X-Sentry-Auth: Sentry sentry_version=7, sentry_client=<sdk>/<version>, sentry_key=<key>` (plus `, sentry_secret=…` when the DSN has one). `compressPayload` defaults to `true` on IO platforms and sets `Content-Encoding: gzip`. The envelope is the header JSON, then for every item `\n` + item-header JSON + `\n` + payload — **no trailing newline** — and every item header carries `type` and `length` (and `content_type` when known). The envelope header may carry `event_id`, `sdk`, `trace`, `dsn` and `sent_at`. A `200` response's `id` is parsed and a missing one tolerated; `X-Sentry-Rate-Limits` is parsed as comma-separated `seconds:categories:scope…`, where an empty category list means **all** categories, and is preferred over `Retry-After`; the transport retries nothing itself on any status. | `sentry-9.6.0/lib/src/protocol/dsn.dart: Dsn.parse, postUri`; `transport/http_transport_request_handler.dart: _buildAuthHeader`; `transport/encode.dart: compressBody`; `sentry_options.dart: compressPayload`; `sentry_envelope.dart: envelopeStream`; `sentry_envelope_item_header.dart: toJson`; `sentry_envelope_header.dart: toJson`; `transport/http_transport.dart: send, _updateRetryAfterLimits`; `transport/rate_limit_parser.dart: parseRateLimitHeader` |
| T22 | The two anonymized source events (`~/.cache/orch/p4-fixtures/flutter-typeerror-unhandled.json`, 40,406 bytes; `flutter-dio-error.json`, 39,349 bytes; **read**) carry: `platform: "other"`, `sdk {name: "sentry.dart.flutter", version: "9.6.0", integrations, packages}`, float `timestamp` (`1790329312.838`, `1790329248.665`), `tags` as 22 `[key, value]` pairs, `breadcrumbs` as `{values: […]}` (70 and 55 crumbs, float timestamps, levels `info`/`warning`, `data` objects of at most 235 bytes, no `message`), 11 and 12 contexts (the second adds `API Request Info`, a name with spaces), `exception.values` of one value with `stacktrace.frames` oldest first and each frame carrying `function`, `package`, `filename`, `abs_path`, `lineno`, `platform: "dart"`, `in_app` and a server-added `data.client_in_app`, `fingerprint: ["{{ default }}"]`, `level` `fatal` / `error`, `logger: ""`, `modules` (412 entries), `user {id, ip_address: "{{auto}}", geo}`, and on the Dio event `request {url, method}` and `transaction`. They still carry real identifiers: both `event_id`s, both `contexts.device.id`s, the shared `contexts.trace.trace_id` and both `span_id`s, `user.id`, `user.geo.country_code`, one public git URL per file and one documentation URL in the Dio exception value (§10.1). Mapped, both stay far inside every server bound (crumbs 17,492 / 13,498 bytes of 131,072; contexts 2,458 / 2,646 bytes of 65,536; tag values ≤ 54 code points; **run**, a script over the files). | the two files |
| T23 | The `web` app pins `@sentry/nextjs` `^10.69.0`. | `~/works/web` `origin/main:package.json` (`"@sentry/nextjs": "^10.69.0"`, **read**) |
| T24 | Metric names live as constants in `packages/shared-go/obs/obs.go` ("exactly one place in this repo spells each name"); `obs.LabelOutcome` is `"outcome"`. `obs.Bootstrap` installs a View that strips the request-derived attributes `server.address`, `url.path`, `url.full`, `client.address`, … from every instrument. | `packages/shared-go/obs/obs.go` (the `Metric*` const block, `LabelOutcome`); `packages/shared-go/obs/provider.go: unboundedMetricAttributes, dropUnboundedAttributes` |
| T25 | **Client-address resolution.** `main.go` parses `TRUSTED_PROXY_CIDRS` once (`clientip.ParseTrusted`; an unparseable or IPv4-mapped entry is fatal at start-up) into **one** `clientip.Resolver`, and every ingest chain's `clientip.Middleware` uses that same resolver. `Resolve` starts at the connection's peer; only when the peer is inside a trusted prefix does it read `X-Forwarded-For` (every header line, comma-split) right to left, skipping trusted entries, and the first untrusted entry is the caller — kept only when `IsPublic` (private, loopback, link-local, CGNAT, NAT64 and similar are not; the documentation ranges are). No trusted peer → the peer itself, if public. A request carrying the derive-IP header gets no caller IP. `Middleware` also stores `req.UserAgent()` as the connection User-Agent enrichment parses (T12). The chart renders `TRUSTED_PROXY_CIDRS` from `config.ingestor.trustedProxyCidrs` as plain env on the ingestor container only; it is `[]` in `values.yaml` and both Huawei overlays, where `tests/unit/helm_geoip_test.go: TestHelmTrustedProxyCidrsEmptyOnHuawei` asserts it empty (sentinel-event-enrichment contract decision 11: "no forwarding header is believed on Huawei"). `ParseTrusted`'s errors are prefixed with the literal `TRUSTED_PROXY_CIDRS:` whatever list it was given. Nothing in the ingestor logs a connection's peer address. The chart's overlays are `values-huawei-staging.yml`, `values-huawei-production.yml`, `values.aws.staging.yml`, `values.aws.production.yml` and `values-prod-example.yaml`. | `apps/ingestor-go/clientip/clientip.go: ParseTrusted` (its three `fmt.Errorf("TRUSTED_PROXY_CIDRS: …")`), `NewResolver, Resolve, IsPublic, Middleware`; `apps/ingestor-go/main.go: main` (~L78-88, `clientResolver`); `deploy/helm/sentinel/templates/ingestor.yaml` (`TRUSTED_PROXY_CIDRS`); `values.yaml: config.ingestor.trustedProxyCidrs`; `values-huawei-production.yml` (~L124-127); `git ls-tree origin/main deploy/helm/sentinel/` (**run**) |
| T26 | **Nothing bounds a mapped event by NATS `max_payload`** (1,048,576 bytes, the server default). `IngestAt` marshals the event and publishes it with no size check; `IngestService.publish` returns `PublishWithHeaders`' error unchanged, and `PublishWithHeaders` wraps the JetStream error with `%w`, so a message over `max_payload` reaches the caller as an error for which `errors.Is(err, nats.ErrMaxPayload)` holds, and `handleIngest` answers it `500`. The bounds step measures **JSON** bytes, while `structpb` spends 11 proto bytes on a JSON `0` (2 bytes), and S1 / F1-F2 of §5 duplicate the titled exception's frames to the top level and `abs_path` into `file`: spec review 1 measured a 232,996-byte Sentry event (numeric arrays in crumb `data`, contexts and `extra`) at `proto.Size` 1,272,186, and an 850,095-byte one (70 frames with `abs_path` and context lines) at 1,760,755 — both under the 1 MiB item cap. The SDKs already hold to a **1,000,000-byte** publish budget (NATS's limit less 48,576 bytes for headers and framing). `MapPayloadToEvent` maps breadcrumbs, contexts, metadata keys and exception values one to one, so an element of the payload and its proto counterpart can be paired by index or key. | `apps/ingestor-go/service/service.go: IngestAt` (`proto.Marshal`, then `s.publish`), `publish`; `packages/shared-go/nats/nats.go: (*Publisher).PublishWithHeaders` (`fmt.Errorf("failed to publish message: %w", err)`); `packages/sdk-go/publishcost.go: maxPublishBytes`; `packages/sdk-js/src/core/proto-size.ts: NATS_PUBLISH_BUDGET`; `apps/ingestor-go/mapping/mapper.go: MapPayloadToEvent, mapBreadcrumbs, mapContexts, mapException`; finding `nats-max-payload-unbounded` (the two measurements, reviewer's) |

### 1.3 Deviations from the contract

| # | Contract says | What is true | This spec |
|---|---|---|---|
| V1 | **Superseded by §1.4 AM2** (no DSN is shown). Kept for the record: §4.5 "The key-create result (project keys page `…/keys/+page.svelte:174-175`, `ApiKeyCreateModal.svelte`) shows, beside the one-time secret, a copyable Sentry DSN". | The secret is shown in the **page's** `secret-tray`, after create and after rotate; the modal never sees the token and is closed on success (T18). | — (was: render the DSN in the page's `secret-tray`). Nothing on the keys page or in `ApiKeyCreateModal.svelte` changes. |
| V2 | §4.3 "`error_class` = its `type` (or `module.type` if `type` is empty)". | With `type` empty, `module.type` is just `module` plus a dot; the phrase cannot be read literally. `ExceptionValue` has no `module` field (T7). Sentry titles an exception without a `type` by its `value`. | `error_class` = the trimmed `type`; when that is empty, the first non-blank line of the exception's `value`; when that is empty too, the no-exception rules (message, then `UnknownError`). `module` is dropped (§5.3 rows E2-E4). |
| V3 | §4.4 "A Dart frame … with `in_app` **unset** gets `in_app = false` for `dart:` and `package:flutter/` paths; other `package:` paths keep whatever the SDK sent." | v2 `in_app` is a `bool` (T7): "unset" and `false` are the same stored value, and "keep whatever the SDK sent" for an unset flag is also `false`. The rule has one observable outcome for every path. | One rule for every frame, Dart or not: `in_app` is `true` only when the frame sent JSON `true` **and** has a `file` (§5.4 F7). §6.1 records why that is the contract's rule, and P5.4 proves a Dart frame with no `in_app` is stored `false`. |
| V4 | §4.2 "`/store/` takes one event JSON (same encodings and limits)". | The 1 MiB per-event rule is written for an envelope item ("that item is dropped and counted, the rest of the envelope is still processed"); a store body has no rest. | On `/store/`, a decompressed body over 1 MiB is `413` (and counted `too_large`), because dropping the only event with a `200` would hide the loss (§4.5). |
| V5 | §4.2 "A publish failure → `500` (the client retries)". | After §1.4 the client is core's relay consumer, not a Sentry SDK (on phones the native SDK's retry and offline cache applied, T21; the Dart HTTP transport retries nothing). Whether a `500` is retried is the relay's choice (a JetStream consumer NAKs and redelivers). | The status is `500` as specified, **except** a message NATS refuses as over `max_payload`, which D13 makes a `413` (§4.7). The parenthesis is not relied on anywhere in this spec; a re-send, when it happens, is idempotent (T14). |
| V6 | §4.2 metric `sentinel_compat_items_total{type,outcome}` with `type` the item type, including "any unknown type". | `type` is client-controlled; used verbatim it is an unbounded label (T24's View does not cover it). | `type` is one of 17 known item types or `unknown` (§4.6). The constant lives in `apps/ingestor-go/sentrycompat`, not `packages/shared-go/obs` (T24's convention), because `obs` is outside `be.md`'s scope; §12.2 item 10 records it. |

### 1.4 Amendments (operator decision 2026-09-27, contract §9 Q1)

The orchestrator relayed this decision on 2026-09-27; it is merged into the contract (prd#803, #804, prd `main` @ `7994e51`), and spec review 1's rulings follow as D13-D15 (prd#806). Contract §9 Q1 ("how does a phone
reach the ingestor?") is **answered: option (b), a relay in core** (`core-sentinel-mobile-relay`, a separate feature).
No Sentry client talks to the ingestor directly. Core publishes each app envelope to its own JetStream stream; a core
consumer POSTs it to the ingestor's `/api/{project_id}/envelope/` with a project-scoped `ingest` key held in core's
environment, presented as `X-Sentry-Auth`, and sends the phone's IP as `X-Forwarded-For` and its User-Agent as
`User-Agent`.

| # | Amendment | What changes in this spec |
|---|---|---|
| AM1 | The caller of the compat routes is core's relay consumer, on the private network. | Nothing in §3.1-§5 changes: both routes, all three key sources (§3.2), D1 (§3.3), D2 (§3.4), the rate-limit headers (§3.5), the reader, the mapper and Dart grouping stand. The rate-limit headers now reach core, which decides what to tell the phones; every phone of a project shares the relay key's `rate_limit_rpm` budget (§3.5 note). The envelope handler still accepts a direct Sentry client, which is what the staging check (§10.3) and the e2e test (P4.7) do. |
| AM2 | **TASK-06 shrinks.** No DSN display, no dashboard setting for a public ingestor host. Keep the derived numeric Sentry project id on project settings, the TS derivation and the shared Go/TS golden vector, because core's relay configuration needs the id. | §8 is rewritten: one `Sentry project id` section on project settings and `src/lib/sentry-project-id.ts`. Dropped: `INGESTOR_PUBLIC_URL`, `config.dashboard.ingestorPublicUrl`, the compose and `.env.example` lines, `SentryDsnRow.svelte`, `ingestor-public-url.ts`, `buildSentryDsn`, and their proofs. V1 is superseded. |
| AM3 | **The ingestor must believe core's `X-Forwarded-For` on compat routes; `/ingest` behaviour is unchanged.** The orchestrator named `trustedProxyCidrs` / `TRUSTED_PROXY_CIDRS`. | `TRUSTED_PROXY_CIDRS` feeds the **one** resolver every ingest chain shares (T25), so adding core's range there would also make `/ingest` and `/ingest/batch` believe `X-Forwarded-For` from that range — a behaviour change for `/ingest`, and a breach of the Huawei decision-11 test (T25) wherever the value is set. This spec therefore adds a **compat-only** list, `config.ingestor.sentryCompatTrustedProxyCidrs` → `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS`, read with the same parser; the compat chains use a resolver over **both** lists, `/ingest` keeps its resolver over `TRUSTED_PROXY_CIDRS` alone (§3.6). `/ingest` is then unchanged **by construction**, not by an argument about which traffic comes from core. **Ratified** as contract D10 (§4.7: "ratified 2026-09-27 from the spec, its AM3"). |
| AM4 | Contract §2 / `be.md` "no helm change" is lifted for this one value. | `values.yaml` and `templates/ingestor.yaml` gain the value, and the two AWS overlays an explicit empty one (§3.6.3). By D15 it ships **empty everywhere**; `core-sentinel-mobile-relay` sets it per environment at its rollout, from the RUNBOOK entry of §3.6.4. |
| AM5 | Core presents the key via `X-Sentry-Auth`. | K1 of §3.2 is the production path; K2 and K3 stay (contract §4.1, unchanged by the amendment). |
| AM6 | Keys no longer ship in a mobile binary. | D1 is kept as decided (the amendment says so): a relay key can still do no more than ingest into one project. §3.3's rationale is updated. |
| AM7 | **The contract of record now carries both amendments** (prd `main` @ `7994e51`): TASK-06 is the derived project id only (§4.5, D9), §4.7 and D10-D12 are in, and `be.md` lifts "no helm change" for the compat CIDR value. | The header's "the contract wins" reads against that contract plus D13-D15 (prd#806). Nothing in this spec depends on a pending amendment. |
| AM8 | **Amendment 2 (operator-approved design, prd#804; contract §4.7, D12; card TASK-07).** Core does not rewrite the envelope. It sends `X-Sentinel-Relay-Auth: user\|anonymous`, and with `user` also `X-Sentinel-Relay-User-Id` and `X-Sentinel-Relay-Tenant-Id` (core's user and company ids). The ingestor honours them only when the request comes from a trusted-proxy address (the test it uses for `X-Forwarded-For`), and then overrides the mapped event: `user.id` = the relay user id (other user fields kept), `tags.tenant_id` = the relay tenant id, `tags.relay_auth` = the auth value. From any other address the headers are ignored. | §3.7 specifies it; P7.1-P7.8 prove it. D14 (prd#806) adds that a body-supplied `tags.relay_auth` is always removed and that `anonymous` also removes the body's `user.id` and `tags.tenant_id` (§3.7.4, P7.9-P7.11). |
| AM9 | **Spec review 1 rulings (prd#806, contract D13-D15).** D13: a mapped compat event must fit NATS `max_payload`; over the 1,000,000-byte budget it is shed in an order the spec fixes, and what still does not fit is `413` `too_large`, never `500`. D14: body-supplied relay tags are not trusted. D15: the per-environment CIDRs are `core-sentinel-mobile-relay`'s rollout step, not this run's; this run ships the value empty everywhere with a RUNBOOK entry. | D13 → §4.7, §4.2 step 6.3, §4.5, §4.6, P4.8-P4.10. D14 → §3.7.4, P7.9-P7.11. D15 → §3.6.3, §3.6.4, §9, P2.13; the TASK-07 chart criterion is now "the chart exposes `sentryCompatTrustedProxyCidrs`, empty everywhere, and the RUNBOOK says how to derive it". |

---

## 2. What this change does, and does not do

**Does:**
1. Registers `POST /api/{project_id}/envelope/` and `POST /api/{project_id}/store/`, each with and without the trailing
   slash, on the default `ServeMux`, behind the existing `otelhttp → observe → clientip` chain (§3.1).
2. Adds a compat-only authenticator: `X-Sentry-Auth`, then `?sentry_key=`, then `X-API-Key`; only project-scoped
   `ingest` keys; the path's numeric project id must equal the one derived from the key's project UUID (§3.2-§3.4).
3. Adds a compat-only rate-limit response: `Retry-After: 60` and `X-Sentry-Rate-Limits: 60::organization` (§3.5).
4. Adds a bounded body decoder (identity, gzip, zlib and raw deflate) and an envelope/store reader with every contract
   limit and a metric per item (§4).
5. Adds a Sentry event → `validation.ErrorPayload` mapper that emits only values the ingestor accepts, and hands each
   mapped event to `svc.IngestAt`'s pipeline, unchanged except for a publish budget only compat callers pass (§5,
   §4.7).
6. Holds every compat event under the 1,000,000-byte NATS publish budget: an over-budget event is shed in a fixed
   order and recorded in `metadata.server_truncated_keys`; one that still does not fit, or that NATS refuses, is
   `413` `too_large`, never `500` (§4.7, D13).
7. Groups Dart errors correctly with no processor change: frames reversed to crash-first, `file` = the full
   `package:` URI (§6).
8. Believes the relay's `X-Forwarded-For` on compat routes only, through a compat-only trusted-proxy list, so a Flutter
   event's `{{auto}}` address becomes the phone's (§3.6). The list ships empty everywhere (fail closed, D15);
   `core-sentinel-mobile-relay` sets it at its rollout, by the RUNBOOK entry this change adds.
9. Applies the relay's identity headers (`X-Sentinel-Relay-*`) to every mapped event, only from a trusted-proxy peer,
   and never lets the event body claim relay identity (§3.7, TASK-07, D14).
10. Shows the derived Sentry project id on project settings (§8).
11. Documents the surface (§9).

**Does not** (contract §2, plus what this spec declines):
- change `/ingest` or `/ingest/batch` (their chains, scopes, headers, responses, trusted proxies and publish path are
  byte-identical — no publish budget, no shedding; P2.9, P2.10, P2.12, P4.10);
- name or set a per-environment value of `sentryCompatTrustedProxyCidrs` (D15: `core-sentinel-mobile-relay`'s rollout);
- show a DSN, or add a dashboard setting for an ingestor host (§1.4 AM2);
- store transactions, spans, sessions, replays, attachments, client reports, profiles, check-ins, logs, statsd,
  feedback or user reports (§4.4); symbolicate anything; serve CORS or `OPTIONS` (`405`, D8);
- add a migration, a column, an index, a NATS subject, an ingress, or a public endpoint (contract §2, §9 Q1);
- change `apps/processor-go` (§6.3: Dart grouping needs no processor change);
- trust the envelope header's `dsn` as a key, or log a key or a DSN anywhere (§3.2);
- set `fingerprint_override`, `trace_flags` or `project_key` from the Sentry body (identity comes from the key, §5.2).

---

## 3. Routes, auth and rate limiting (TASK-02)

### 3.1 Route registration

A new package `apps/ingestor-go/sentrycompat` owns the four patterns:

```go
// routes.go
const (
	PatternEnvelope       = "POST /api/{project_id}/envelope/{$}"
	PatternEnvelopeNoSlash = "POST /api/{project_id}/envelope"
	PatternStore          = "POST /api/{project_id}/store/{$}"
	PatternStoreNoSlash    = "POST /api/{project_id}/store"
	PathValueProjectID    = "project_id"
)

// Mount registers envelope and store on mux, each under both spellings of its path. envelope and store are the
// FULL chains (otelhttp outermost); Mount adds nothing around them.
func Mount(mux *http.ServeMux, envelope, store http.Handler) {
	mux.Handle(PatternEnvelope, envelope)
	mux.Handle(PatternEnvelopeNoSlash, envelope)
	mux.Handle(PatternStore, store)
	mux.Handle(PatternStoreNoSlash, store)
}
```

Why these exact patterns:
- `{$}` anchors the slash form, so `/api/1/envelope/anything` is a `404`, not the envelope handler (a pattern ending in
  `/` without `{$}` matches its whole subtree).
- Registering the slash-less form explicitly means the mux never answers a `POST /api/1/envelope` with its `307` to
  the slash form (it redirects only when no pattern matches the path as given; **run**: `307`, `Location:
  /api/1/envelope/`). A `307` keeps the method, but a caller that does not follow redirects — an HTTP client
  configured not to, as a relay may be — would lose the event, and one that does pays a second round trip with the
  body re-sent.
- The method prefix makes the mux itself answer every other method (`GET`, `HEAD`, `PUT`, `OPTIONS`, …) with
  `405 Method Not Allowed` and `Allow: POST` (T1). That `405` is written before the chain runs, so it is not counted in
  `sentinel_ingest_requests_total` and needs no key. This is the contract's "other methods → 405" and D8's "no CORS".
- `{project_id}` matches exactly one non-empty path segment of any content; its validity is judged by auth (§3.4),
  never by the mux, so every malformed id is a `403`. A path with an empty segment (`/api//envelope/`) is answered by
  the mux's existing path cleaning (`307` to `/api/envelope/`, then `404`), before any of this code runs; no well-formed
  caller produces one.
- The four patterns do not overlap `/ingest`, `/ingest/batch`, `/health` or `/metrics`, so `mux.Handle` cannot panic on
  a conflict at start-up (P2.1 builds a mux with all eight registrations).

`main.go` builds two chains and mounts them beside the existing routes, after the `/ingest/batch` line:

```go
compat := sentrycompat.Config{
	Ingest:     ingestService,                       // *service.IngestService satisfies sentrycompat.Ingester
	Scope:      func(ctx context.Context, p *validation.ErrorPayload) error { return applyAuthenticatedScope(ctx, db, p) },
	Logger:     logger,
	RecordItem: compatItems,                          // sentrycompat.NewItemRecorder(meter), §4.6
}
// sentrycompat.Chain is the importable part of the chain (so tests/unit proves the wiring, P2.11):
//   clientip.Middleware(resolver, authenticator.SentryMiddleware(rateLimiter.SentryMiddleware(h)))
envelopeChain := observeMiddleware(ingestRequestCounter,
	sentrycompat.Chain(compatResolver, authenticator, rateLimiter, sentrycompat.NewEnvelopeHandler(compat)))
storeChain := observeMiddleware(ingestRequestCounter,
	sentrycompat.Chain(compatResolver, authenticator, rateLimiter, sentrycompat.NewStoreHandler(compat)))
sentrycompat.Mount(http.DefaultServeMux,
	otelhttp.NewHandler(envelopeChain, "POST /api/{project_id}/envelope/"),
	otelhttp.NewHandler(storeChain, "POST /api/{project_id}/store/"))
```

```go
// chain.go
// Chain wraps h in the compat routes' client-address, auth and rate-limit layers, in that order (outermost first).
// main.go adds observeMiddleware and otelhttp around it, as it does for /ingest.
func Chain(resolver *clientip.Resolver, a *auth.APIKeyAuthenticator, rl *middleware.RateLimiter, h http.Handler) http.Handler
```

What stays in `main.go` and is proven only end to end (U40, P4.7): the `observeMiddleware`/`otelhttp` wrap, the
`Mount` call, and passing `compatResolver` (not `clientResolver`) — three lines of wiring `tests/unit` cannot import
(package `main`). Everything they compose is proven in `tests/unit`.

The otelhttp operation names are fixed strings, never the request path (the path carries the project id; T24).
`r.PathValue` survives every layer: the mux sets it on the request it dispatches, and `Request.WithContext` (used by
the auth and clientip middlewares) copies the matched pattern with the request.

### 3.2 Key presentation and precedence

`apps/ingestor-go/auth/sentry.go` (new):

```go
var (
	ErrSentryKeyMissing   = errors.New("Missing API key")
	ErrSentryAuthMalformed = errors.New("Invalid X-Sentry-Auth header")
)

// SentryKey returns the API key a Sentry-compatible request presents. The FIRST source that is present is the ONLY
// source consulted; a present-but-unusable source is an error, never a fall-through.
func SentryKey(r *http.Request) (string, error)

// ParseSentryAuth extracts sentry_key from an X-Sentry-Auth value.
func ParseSentryAuth(header string) (string, error)
```

**Precedence** (compat routes only; `/ingest` keeps reading `X-API-Key` alone):

| # | Source | "Present" means | Present and usable | Present and unusable |
|---|---|---|---|---|
| K1 | `X-Sentry-Auth` header | `strings.TrimSpace(r.Header.Get("X-Sentry-Auth")) != ""` | its `sentry_key` | `401 "Invalid X-Sentry-Auth header"` |
| K2 | `sentry_key` query parameter | `r.URL.Query().Has("sentry_key")` | `r.URL.Query().Get("sentry_key")` when non-empty after `TrimSpace` | empty → `401 "Missing API key"` |
| K3 | `X-API-Key` header | non-empty | its value | — |
| — | none of the three | | | `401 "Missing API key"` |

- K1 wins over K2 and K3 even when they are present too; K2 wins over K3. A client that sends a header means it, and
  never mixing sources means a request cannot be authenticated by one key while naming another.
- The envelope header's `dsn` is **never** read as a key (it is not even parsed for one, §4.3). `sentry_secret`,
  `sentry_version`, `sentry_client`, `sentry_timestamp` and any other parameter are ignored, in the header and in the
  query.
- Only the first value of a repeated header or query parameter is read (`Header.Get`, `Query().Get`).
- The key itself is never logged, echoed or put on a span or metric; error bodies are the fixed strings above.

**The `X-Sentry-Auth` parser** (`ParseSentryAuth`), tolerant by design:
1. `v := strings.TrimSpace(header)`. The first whitespace-delimited word must equal `Sentry` case-insensitively and be
   followed by at least one space or tab; otherwise `ErrSentryAuthMalformed`. (`"Sentry sentry_key=k"` passes;
   `"sentry_key=k"`, `"SentryX sentry_key=k"` and `"Sentry"` alone do not.)
2. The rest is split on `,`. Each part is `TrimSpace`d; a part with no `=` is skipped; otherwise the name is the text
   before the **first** `=` (trimmed, compared case-insensitively) and the value the text after it (trimmed, then one
   surrounding pair of `"` removed if both ends have one).
3. The first part whose name is `sentry_key` and whose value is non-empty gives the key. No such part →
   `ErrSentryAuthMalformed`. Later `sentry_key` parts are ignored.

Examples fixed by P2.3: `Sentry sentry_version=7, sentry_client=sentry.dart.flutter/9.6.0, sentry_key=K` → `K`;
`Sentry sentry_key=K,sentry_version=7` → `K`; `sentry   SENTRY_KEY = "K" ` → `K`; `Sentry sentry_key=K, sentry_secret=S`
→ `K`; `Sentry sentry_key=, sentry_key=K` → `K`; `Sentry sentry_key=A, sentry_key=B` → `A`; `Sentry sentry_version=7` → malformed; `Basic abc` → malformed.

### 3.3 Which keys: the ingest-only rule (D1), without touching `/ingest`

```go
// ScopesSentryCompat guards the Sentry-compatible routes: project-scoped ingest keys only (contract D1).
var ScopesSentryCompat = ScopeSet{"ingest"}

// SentryMiddleware authenticates a Sentry-compatible request. It never consults X-Project-Key and never touches
// the database beyond the key lookup Middleware uses.
func (a *APIKeyAuthenticator) SentryMiddleware(next http.Handler) http.Handler
```

`apikey.go` is refactored so both middlewares share one key-resolution step, and `Middleware`'s behaviour stays
byte-identical (P2.10):

```go
// resolveKey hashes apiKey and looks it up (a.lookup: Redis cache, then Postgres). The hash is returned for
// WithIdentity and the rate limiter.
func (a *APIKeyAuthenticator) resolveKey(ctx context.Context, apiKey string) (*APIKeyData, string, error)

// NewAPIKeyAuthenticatorWithLookup builds an authenticator over an injected lookup, with no database, Redis or
// invalidation subscriber. For tests: tests/unit is outside package auth and cannot set the unexported field.
func NewAPIKeyAuthenticatorWithLookup(lookup func(ctx context.Context, hashStr string) (*APIKeyData, error)) *APIKeyAuthenticator
```

`SentryMiddleware`, in order (first failing step answers):

| Step | Check | Answer on failure |
|---|---|---|
| A1 | `SentryKey(r)` | `401` with the error's fixed text (§3.2) |
| A2 | `a.resolveKey` | `401 "Invalid API key"` (unknown, revoked, expired or inactive — indistinguishable, as today) |
| A3 | `ScopesSentryCompat.Allows(data.Scope)` | `403 "Forbidden"` — `admin`, `read`, `agent`, `report`, and any future scope |
| A4 | project-scoped: `data.ProjectID != nil && data.ProjectName != ""` | `403 "Forbidden: a Sentry-compatible key must be project-scoped"` — every `sent_org_` key, and `sent_agent_` keys (already refused at A3) |
| A5 | `SentryProjectID(*data.ProjectID)` equals `r.PathValue("project_id")` exactly (string compare; §3.4) | `403 "Forbidden: project id does not match the key's project"`; a UUID the derivation rejects is the same `403` |
| A6 | — | `next.ServeHTTP(w, r.WithContext(WithIdentity(ctx, data.ProjectName, *data.ProjectID, data.OrganizationID, hash, data.RateLimitRPM, false)))` |

Why a separate method rather than a third `ScopeSet` on `Middleware`: `Middleware` admits org-wide keys and consults
`X-Project-Key` (T3); D1 must refuse both, and A5 needs the path. A new method keeps `/ingest`'s code path unchanged
(contract §2) and lets `ScopesIngest` stay `{ingest, admin}`. `admin` is refused on compat routes because D1 limits a
compat key to ingesting into one project. The contract's reason was a key readable by anyone holding the app binary;
after §1.4 the key lives in core's environment, and the rule stands so that a leaked relay key can do no more (AM6).

Status codes keep today's meaning (contract §4.1): `401` missing or invalid, `403` scope or project. Both mean a
misconfigured caller; retrying cannot fix either. After §1.4 the caller is core's relay, which decides what a `4xx`
means for the envelope it holds.

### 3.4 The derived numeric project id (D2)

```go
// SentryProjectID derives the numeric Sentry project id of a Sentinel project: the decimal value of the first 13
// hex digits of its UUID, dashes removed. 13 hex digits are 52 bits, below 2^53, so every JS runtime holds the
// value exactly. It is derived, never stored (contract D2).
func SentryProjectID(projectUUID string) (string, error)
```

Rules, identical in Go (`apps/ingestor-go/auth/sentry.go`) and TS (`apps/dashboard-web/src/lib/sentry-project-id.ts:
sentryProjectId`, §8.2):
1. Remove every `-`; lower-case. The result must be exactly 32 characters of `[0-9a-f]`, and the input must have been
   either 32 hex digits or the canonical `8-4-4-4-12` form; anything else is an error (Go) / throws (TS).
2. Take the first 13 characters; parse as base 16 (`strconv.ParseUint(h, 16, 64)` / `Number.parseInt(h, 16)`).
3. Format as base-10 with no sign, no leading zeros (`"0"` for zero) (`strconv.FormatUint(n, 10)` / `String(n)`).

The golden vector is one committed file both languages read, `tests/fixtures/sentry/project-id-vectors.json`:

```json
{
  "valid": [
    {"uuid": "00000000-0000-0000-0000-000000000000", "sentry_project_id": "0"},
    {"uuid": "ffffffff-ffff-ffff-ffff-ffffffffffff", "sentry_project_id": "4503599627370495"},
    {"uuid": "FFFFFFFF-FFFF-4FFF-BFFF-FFFFFFFFFFFF", "sentry_project_id": "4503599627370484"},
    {"uuid": "0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6", "sentry_project_id": "27690631804612"},
    {"uuid": "7c9e6679-7425-40de-944b-e07fc1f90ae7", "sentry_project_id": "2192316254601812"},
    {"uuid": "00000000-0001-4000-8000-000000000000", "sentry_project_id": "20"},
    {"uuid": "a3bb189e8bf938889912ace4e6543002",     "sentry_project_id": "2880383476088723"}
  ],
  "invalid": ["", "not-a-uuid", "0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f", "0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6a",
              "0192f3a4_5b6c_4d7e_8f90_a1b2c3d4e5f6", "g192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6", " 0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6"]
}
```

(Values computed by this seat, **run**: `int(uuid.replace("-","").lower()[:13], 16)`; the maximum,
4,503,599,627,370,495, is 2^52 − 1.)

The path comparison (A5) is an exact string compare against the canonical decimal, so `/api/0042/…` for project 42 is a
`403`: the id project settings shows (§8) never has leading zeros, and one spelling per project keeps the check trivial.
Two projects deriving the same id (48 random bits for a v4 UUID, T20) is harmless: nothing is ever looked up **by**
the derived id; it is only compared with the derivation of the key's own project.

### 3.5 Rate limiting on compat routes

`middleware/ratelimit.go` gains one method and one constant pair; `Middleware` is unchanged in behaviour:

```go
const (
	// RetryAfterSeconds is the Retry-After both chains send on a 429: the sliding window's length.
	RetryAfterSeconds = 60
	// SentryRateLimitsValue is X-Sentry-Rate-Limits on a compat 429: all categories (empty list), organization scope.
	SentryRateLimitsValue = "60::organization"
)

// SentryMiddleware is Middleware for the Sentry-compatible routes: the same per-key sliding window, the same
// RATELIMIT_STRICT_MODE behaviour, and on a 429 also X-Sentry-Rate-Limits.
func (rl *RateLimiter) SentryMiddleware(next http.Handler) http.Handler
```

Implementation: the body of `Middleware` moves into `func (rl *RateLimiter) limit(next http.Handler, onLimited
func(http.Header)) http.Handler`; `Middleware` calls it with a no-op `onLimited`, `SentryMiddleware` with one that sets
`X-Sentry-Rate-Limits: 60::organization`. `onLimited` runs after the existing `X-RateLimit-*` and `Retry-After` headers
are set and before `http.Error` writes the `429`. `Retry-After` stays the literal `"60"` on both chains
(`strconv.Itoa(RetryAfterSeconds)`), so `/ingest` is byte-identical.

Why 60: the window is one minute and the Lua script returns no oldest-entry time, so 60 is the only value the limiter
can promise without a Redis round trip, and it is today's `Retry-After`. sentry-dart reads `60::organization` as
"every category for 60 s" (T21). `RATELIMIT_STRICT_MODE` behaves as on `/ingest`: a Redis failure is `500` (strict) or
a pass (`false`), with no Sentry header either way. The limit is per key (`ratelimit:<hash>`), shared with any `/ingest`
traffic that uses the same key. After §1.4 every phone of a project arrives through the relay's one key, so that key's
`rate_limit_rpm` is the project's whole mobile budget (daya-mobile's 90-day peak, 12,471 events/day, is under 9 a minute
on average; bursts are what the limit is for), and the `429` with `X-Sentry-Rate-Limits` reaches core, not a phone.

### 3.6 The relay's forwarded client address (§1.4 AM3)

#### 3.6.1 Two resolvers

`apps/ingestor-go/clientip/clientip.go` gains one function; the behaviour of `ParseTrusted`, `NewResolver`, `Resolve`
and `Middleware` is unchanged:

```go
// Resolvers parses TRUSTED_PROXY_CIDRS (globalSpec) and SENTRY_COMPAT_TRUSTED_PROXY_CIDRS (compatSpec) and returns
// the two resolvers main.go uses: ingest over the global prefixes ALONE (for /ingest and /ingest/batch, exactly
// today's resolver), compat over the global prefixes plus the compat ones. Either spec invalid is an error that
// names the variable it came from, which main.go treats exactly as today's invalid TRUSTED_PROXY_CIDRS: log
// clientip.config_invalid and exit 1.
func Resolvers(globalSpec, compatSpec string) (ingest, compat *Resolver, err error)
```

**Error text.** `ParseTrusted` prefixes every error with the literal `TRUSTED_PROXY_CIDRS:` (T25), so reusing it as is
for the compat list would make a typo in `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS` exit naming the wrong variable. Its body
therefore moves into an unexported `parseTrustedList(envName, spec string) ([]netip.Prefix, error)` that prefixes its
errors with `envName + ":"`; `ParseTrusted(spec)` becomes `parseTrustedList("TRUSTED_PROXY_CIDRS", spec)`, so its
messages stay byte-identical, and `Resolvers` parses `compatSpec` with `parseTrustedList(
"SENTRY_COMPAT_TRUSTED_PROXY_CIDRS", compatSpec)`. An invalid compat entry therefore fails start-up with, for example,
`SENTRY_COMPAT_TRUSTED_PROXY_CIDRS: "10.20.0.0/33" is not a CIDR or IP address` (P2.11 asserts the name, and that it
does not start `TRUSTED_PROXY_CIDRS:`).

`main.go` replaces its `ParseTrusted` + `NewResolver` lines with one call:

```go
clientResolver, compatResolver, err := clientip.Resolvers(os.Getenv("TRUSTED_PROXY_CIDRS"), os.Getenv("SENTRY_COMPAT_TRUSTED_PROXY_CIDRS"))
if err != nil { /* the same two lines as today's TRUSTED_PROXY_CIDRS failure */ }
// the existing "trusted proxy ranges" log line (clientResolver.Trusted()), plus one for compatResolver.Trusted()
```

The compat chains get `compatResolver` (§3.1, through `sentrycompat.Chain`); `ingestChain` and `batchChain` keep
`clientResolver`, which is the same resolver over the same prefixes as today. Why the union and
not the compat list alone: a future public ingress configured in `TRUSTED_PROXY_CIDRS` must not make compat requests
through it lose their caller address; compat routes believe at least what `/ingest` believes.

#### 3.6.2 What the relay's request resolves to

Once `core-sentinel-mobile-relay`'s rollout has set the compat list (§3.6.4; in this run it is empty everywhere and
nothing below happens), the relay consumer's connection reaches the ingestor pod from a peer inside that list. The
walk (T25) passes over every trusted hop and takes the first untrusted entry — the phone's IP
the relay wrote — when it is public. Enrichment then fills a Flutter event's `user.ip_address: "{{auto}}"` with it (I1,
T12) and its `user.geo` when the event sent none; `User-Agent` (the phone's, forwarded by the relay) is the connection
User-Agent enrichment parses into `contexts.browser` / `contexts.os` only when those are absent (T12; sentry_flutter
always sends `contexts.os`). Nothing else in the chain reads the address. When the relay sends no `X-Forwarded-For`, or
a private one, the event gets no caller address and `{{auto}}` is blanked (I2) — never core's own address.

#### 3.6.3 Chart

`deploy/helm/sentinel/values.yaml`, directly after `config.ingestor.trustedProxyCidrs`:

```yaml
    # SENTRY_COMPAT_TRUSTED_PROXY_CIDRS (sentinel-sentry-compat-ingest spec §3.6) -- CIDR prefixes whose
    # X-Sentinel-Relay-* identity headers the Sentry-compatible routes (/api/{project_id}/envelope/, /store/)
    # believe -- this list ALONE grants relay identity -- and whose X-Forwarded-For they believe on top of
    # trustedProxyCidrs: the immediate peer core's mobile relay (core-sentinel-mobile-relay) connects from. Every host inside it can assert any app user and
    # tenant: narrowest CIDR only (RUNBOOK "Sentry-compatible ingest: trusting core's relay"). /ingest and
    # /ingest/batch never read this list. Same syntax and start-up check as trustedProxyCidrs. Empty (the
    # default, contract D15) = fail closed: every relay header is ignored, whatever trustedProxyCidrs holds; the
    # compat routes' X-Forwarded-For handling is then exactly /ingest's.
    sentryCompatTrustedProxyCidrs: []
```

`templates/ingestor.yaml`, directly after the `TRUSTED_PROXY_CIDRS` entry, always rendered:

```yaml
            # sentinel-sentry-compat-ingest spec §3.6: compat routes only; empty = no extra trust.
            - name: SENTRY_COMPAT_TRUSTED_PROXY_CIDRS
              value: {{ join "," .Values.config.ingestor.sentryCompatTrustedProxyCidrs | quote }}
```

**Empty everywhere (D15).** `values.yaml` declares the list `[]`. The two AWS overlays, where core's relay will roll
out, gain an explicit empty value under their existing `config.ingestor:` block, so the rollout edits a line that
already exists and a reviewer sees the fail-closed default:

```yaml
    # sentinel-sentry-compat-ingest (contract D15): empty = fail closed -- the relay's X-Forwarded-For and
    # X-Sentinel-Relay-* headers are ignored. core-sentinel-mobile-relay sets this at its rollout, by the
    # RUNBOOK entry "Sentry-compatible ingest: trusting core's relay". Narrowest CIDR only.
    sentryCompatTrustedProxyCidrs: []
```

The Huawei overlays and `values-prod-example.yaml` do not mention it and inherit `[]` (Huawei staging is frozen at 0
replicas; Huawei production is under decision 11). This spec names **no** value for any environment: which address the
ingestor sees as core's consumer is observable only once the relay is deployed, and the cloud migration is still moving
both services (contract §4.7, D15). With the list empty, the compat routes' `X-Forwarded-For` handling is exactly `/ingest`'s (no
`X-Forwarded-For` anywhere today, T25), no request is `PeerTrusted`, and every `X-Sentinel-Relay-*` header is ignored
(§3.7.2), whatever `TRUSTED_PROXY_CIDRS` holds. `trustedProxyCidrs` stays `[]` everywhere, so `TestHelmTrustedProxyCidrsEmptyOnHuawei` keeps passing
unchanged. P2.13 asserts the list is empty in `values.yaml` **and in every overlay file** of the chart.

#### 3.6.4 Deriving the value (a RUNBOOK entry; the rollout is `core-sentinel-mobile-relay`'s)

This run adds the procedure, not the values. `deploy/helm/sentinel/RUNBOOK.md` §4 gains `### Sentry-compatible ingest:
trusting core's relay (sentinel-sentry-compat-ingest)`, which says:

1. **What the value is.** The CIDR of the **immediate peer** the ingestor pod sees on the relay's connections — the
   address in the connection's `RemoteAddr`, never an address inside `X-Forwarded-For`. That one address decides both
   whether `X-Forwarded-For` is read and whether the `X-Sentinel-Relay-*` identity headers are honoured (§3.7.2), so
   **every host inside the CIDR can assert any app user and tenant**. Use the narrowest CIDR that covers the relay's
   peer addresses — a `/32` per address where the addresses are stable — never a VPC, a subnet "to be safe", or the
   ingestor's own range.
2. **How to observe it.** With the list still empty, send one relayed event. Every compat request that carries
   `X-Sentinel-Relay-Auth` from an untrusted peer is logged by the ingestor at Info as
   `event=sentrycompat.relay_peer_untrusted peer=<ip>` (§3.7.3; the peer address only, never a header value). The
   `peer` values seen for the relay's traffic are the candidates.
3. **Which topology produced it**, and what that means:
   - **Direct to the ingestor's Service** (core's consumer in the same cluster calls the chart's `<fullname>-ingestor`
     ClusterIP Service, `templates/ingestor.yaml`): the peer is the consumer **pod's** IP. Pod IPs move on every
     reschedule, so the narrowest stable value is the range core's consumer pods are scheduled from (its node group's
     pod range); prefer a dedicated one. This is the topology to choose: only core's pods are trusted.
   - **Through an internal load balancer** (for example `api-sentinel.staging.tuju.ai`, contract §9 Q1): the peer is
     the load balancer's private address, and **every caller of that load balancer becomes a trusted relay** — it can
     set `X-Sentinel-Relay-*` and `X-Forwarded-For` at will. Do not trust a shared load balancer; route the consumer to
     the Service directly instead. If there is no alternative, the load balancer must append the caller's address to
     `X-Forwarded-For` and core's range must be listed as well (the walk skips only trusted hops, T25), and the
     operator records that every caller of that load balancer is trusted.
   - **Across clusters or VPCs through NAT**: the peer is the NAT gateway's egress address; list exactly those
     addresses (`/32`), and note that everything else egressing through the same NAT is trusted too.
4. **Set it** in that environment's overlay (`values.aws.staging.yml`, `values.aws.production.yml`), in the same change
   as the relay's rollout, and roll the ingestor.
5. **Check it**: one relayed event whose occurrence has `event_user.ip_address` equal to the test phone's public address
   (not empty, not core's) and `tags.relay_auth` set; and one request to the same route from another in-network host
   carrying `X-Sentinel-Relay-Auth: user` whose occurrence has **no** `relay_auth` tag.

A wrong value fails one of two ways: too narrow drops the phone's address and the relay's identity (the fail-closed
state this run ships); too wide lets another in-network caller forge both.

### 3.7 Relay identity headers (TASK-07; contract §4.7, D12; §1.4 AM8)

#### 3.7.1 The headers

| Header | Values | Read when |
|---|---|---|
| `X-Sentinel-Relay-Auth` | `user` or `anonymous` (after `strings.TrimSpace`, compared case-insensitively) | the peer is trusted (§3.7.2) |
| `X-Sentinel-Relay-User-Id` | core's user id, any non-empty string | the peer is trusted **and** the auth value is `user` |
| `X-Sentinel-Relay-Tenant-Id` | core's company id, any non-empty string | the peer is trusted **and** the auth value is `user` |

Constants in `apps/ingestor-go/sentrycompat/relay.go` (new): `HeaderRelayAuth`, `HeaderRelayUserID`,
`HeaderRelayTenantID`, `RelayAuthUser = "user"`, `RelayAuthAnonymous = "anonymous"`, `TagTenantID = "tenant_id"`,
`TagRelayAuth = "relay_auth"`. Only the first value of each header is read (`Header.Get`); values are
`TrimSpace`d and stripped of U+0000 (D-c).

#### 3.7.2 "From a trusted-proxy address"

The same test the `X-Forwarded-For` walk starts with (T25): the connection's peer (`RemoteAddr`, parsed as the walk
parses it) is inside a prefix of `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS` **alone** (§3.6.1). `TRUSTED_PROXY_CIDRS` never makes a peer
`PeerTrusted`: it widens only the compat resolver's `X-Forwarded-For` walk (the union of both lists), so an operator who
lists an ingress there for its `X-Forwarded-For` does not let that ingress's callers assert an app user or tenant. Only the immediate peer counts: an address inside `X-Forwarded-For` never makes
a request trusted.

`clientip` gains `func (r *Resolver) PeerTrusted(req *http.Request) bool`, and `Info` gains one field,
`PeerTrusted bool`, which `Middleware` fills with it. Enrichment does not read it; `/ingest` and `/ingest/batch` build
their `Info` with their own resolver and nothing in their path reads the field or the headers, so their behaviour is
unchanged. With both lists empty (every environment today) nothing is ever trusted and the headers are always ignored.

#### 3.7.3 Resolution, once per request

```go
// RelayIdentity is what the relay asserted about the app user, when it may assert anything.
type RelayIdentity struct {
	Auth     string // "user" or "anonymous"; "" means: apply nothing
	UserID   string // set only when Auth == "user" and the header is non-empty
	TenantID string // set only when Auth == "user" and the header is non-empty
}

// RelayIdentityFromRequest returns the zero RelayIdentity unless clientip.RequestInfoFromContext(r.Context()) has
// PeerTrusted; then it reads the three headers by §3.7.1. logger is Config.Logger (nil → slog.Default()).
func RelayIdentityFromRequest(r *http.Request, logger *slog.Logger) RelayIdentity
```

| Peer | `X-Sentinel-Relay-Auth` | Result |
|---|---|---|
| not trusted | absent | zero: nothing applied |
| not trusted | present (any value) | zero: every relay header ignored; logged once per request at Info as `event=sentrycompat.relay_peer_untrusted` with `peer` = the connection peer's address (the host part of `RemoteAddr`) and nothing else — no header value, no `X-Forwarded-For`, no key. This is how an operator observes the relay's immediate peer before setting the list (§3.6.4 step 2); the relay is the only planned sender of the header, so the line is as frequent as relayed envelopes while the list is wrong, and absent once it is right. |
| trusted | absent, empty, or any value other than `user` / `anonymous` | zero; an invalid non-empty value is logged once per request at Warn (`event=sentrycompat.relay_auth_invalid`, no header values) — a trusted relay sending one is misconfigured, but refusing the envelope would lose the app's event |
| trusted | `anonymous` | `{Auth: "anonymous"}`; the user-id and tenant-id headers are ignored even when present |
| trusted | `user` | `{Auth: "user", UserID: <header or "">, TenantID: <header or "">}` |

#### 3.7.4 Applying it to the mapped event (D12, D14)

**The body never claims relay identity (D14).** `MapEvent` drops a body tag whose key is exactly `relay_auth`
(`TagRelayAuth`), from the object and the pairs form alike (§5.2 M15), whatever the peer. So `tags.relay_auth` on a
published compat event is only ever the value `Apply` wrote from a trusted peer's header: its presence means core's
relay asserted it, and its absence means nothing was asserted.

`func (id RelayIdentity) Apply(p *validation.ErrorPayload)`, called by both handlers on every event right after
`MapEvent` and before `Config.Scope` (§4.2 step 6.1, §4.5). With `Auth == ""` (an untrusted peer, or a trusted one
that asserted nothing valid) it changes nothing: the body's `user` and `tags.tenant_id` stay as the app sent them,
which is what D1 already allows any holder of the key. Otherwise:

1. **`Auth == "user"`:** `UserID != ""` → `p.User.ID = UserID`, allocating `p.User` when it is nil; `username`,
   `email`, `ip_address` and `geo` are kept as the event sent them (the relay asserts identity, not profile).
   `TenantID != ""` → `p.Tags["tenant_id"] = TenantID` (allocating `p.Tags`), replacing any value the app sent. A
   header that is absent leaves the body's value in place (D12 overrides only what the relay states).
2. **`Auth == "anonymous"` (D14):** the anonymous route is open to anyone, so the event must not be attributed to a user
   or a tenant. `p.User.ID` is set to `""` when `p.User` is non-nil (`username`, `email`, `ip_address` and `geo` stay:
   D14 removes only the id), and `tags["tenant_id"]` is deleted. Because the processor falls back to metadata when the
   typed values are absent (`readAffectedIDs`: `user.id`, else `indexer.UserIDFromMetadata` — `user_id`, `userId`,
   `user`; `tags["tenant_id"]`, else `indexer.TenantIDFromMetadata` — `tenant_id`, `tenantId`, `organization_id`),
   those six `p.Metadata` keys (the body's `extra`, M19) are deleted too; without that the same attribution would
   arrive through `extra`. No marker is written for these removals: they are identity the relay revoked, not data cut
   for size.
3. Always `p.Tags["relay_auth"] = Auth`.
4. **Room for the relay tags.** The bounds step keeps only the 50 lexically **smallest** tag keys (T10;
   `bounds.go: boundTags`), which would cut `relay_auth` and `tenant_id` first on a tag-heavy event. So when, after
   steps 1-3, `p.Tags` has more than 50 entries, the other keys are removed lexically **largest first** until 50
   remain; the relay's keys are never removed here. Every removed key is one the bounds step would otherwise have
   removed anyway or one it keeps in exchange for an identity key, and identity wins.

| Peer and assertion | body `tags.relay_auth` | body `user.id` | body `tags.tenant_id` and the six metadata fallback keys | published `tags.relay_auth` |
|---|---|---|---|---|
| untrusted peer (any headers), or trusted with no valid `Auth` | removed (`MapEvent`) | kept | kept | absent |
| trusted, `anonymous` | removed | set to `""` | removed | `anonymous` |
| trusted, `user` | removed | replaced by `User-Id` when sent, else kept | `tenant_id` replaced by `Tenant-Id` when sent, else kept; metadata kept | `user` |

What then happens downstream is today's behaviour, unchanged: the processor takes the affected user from `user.id`
and the affected tenant from `tags["tenant_id"]` when present, else from metadata (`apps/processor-go/event/event.go:
readAffectedIDs`), before any masking. A relay value the bounds step would reject — a user id over 255 bytes, a tenant
id over 200 code points or containing `\n` — is dropped there and marked, as for any SDK (T10); the event is never
refused for it.

---

## 4. Body decoding, envelope and store parsing (TASK-03)

### 4.1 Encodings and size caps

`apps/ingestor-go/sentrycompat/decode.go`:

```go
const (
	MaxCompressedBodyBytes   = 5 << 20  // the request body as sent: 413
	MaxDecompressedBodyBytes = 20 << 20 // the envelope after decoding: 413
	MaxEventItemBytes        = 1 << 20  // one event item's payload (envelope: dropped; store: 413)
	MaxEnvelopeItems         = 100      // item headers per envelope: 413 on the 101st
	MaxHeaderLineBytes       = 64 << 10 // the envelope header line and each item header line: 400 when longer
)

var (
	ErrUnsupportedEncoding = errors.New("unsupported Content-Encoding")     // 415
	ErrBodyTooLarge        = errors.New("request body too large")          // 413
	ErrCorruptBody         = errors.New("invalid compressed request body") // 400
)

// DecodeBody returns a reader over the decoded body that yields at most max bytes and then fails with
// ErrBodyTooLarge. body must already be wrapped by http.MaxBytesReader(w, r.Body, MaxCompressedBodyBytes).
func DecodeBody(body io.Reader, contentEncoding string, max int64) (io.Reader, error)
```

`Content-Encoding` → decoder (after `strings.ToLower(strings.TrimSpace(…))`):

| Value | Decoder |
|---|---|
| absent, `""`, `identity` | the body as is |
| `gzip`, `x-gzip` | `gzip.NewReader` (multistream, the standard library default) |
| `deflate` | **zlib or raw, by sniffing**: wrap the body in a `bufio.Reader`, `Peek(2)`. If two bytes are available and `b0&0x0F == 8`, `b0>>4 <= 7` and `(uint16(b0)<<8 \| uint16(b1)) % 31 == 0` (RFC 1950's CMF/FLG check), `zlib.NewReader`; otherwise `flate.NewReader` (RFC 1951 raw). HTTP's `deflate` is defined as zlib, but clients send both. A raw stream passes the check only if its first block is a non-final **stored** block (low nibble 8 needs `BFINAL=0`, `BTYPE=00`) and then only when the 16-bit check also holds (1 in 31); compressors open with a fixed or dynamic block unless the data is incompressible. Such a stream is then read as zlib and fails → `400`, never a wrong decode. |
| anything else, including a list (`gzip, br`), `br`, `zstd`, `compress` | `ErrUnsupportedEncoding` → `415 "Unsupported Content-Encoding"` |

**The counting reader.** Every decoded body (identity included) is wrapped:

```go
type capReader struct {
	r   io.Reader // io.LimitReader(decoder, max+1): the decoder is never asked for more than max+1 bytes
	n   int64     // bytes returned so far
	max int64
}
// Read returns the decoder's bytes until n reaches max; the call that would take n past max returns the bytes up
// to max and ErrBodyTooLarge, and every later call returns 0, ErrBodyTooLarge.
```

So a decompression bomb stops after at most `max + 1` decoded bytes (plus the decoder's internal window), whatever its
ratio (P3.4 measures it). `max` is `MaxDecompressedBodyBytes` on `/envelope/` and `MaxEventItemBytes` on `/store/`.

**Error → status**, in the handlers (one function, `statusFor(err)`, so both routes agree):

| Error (checked with `errors.Is` / `errors.As`) | Status | Body |
|---|---|---|
| `ErrUnsupportedEncoding` | `415` | `Unsupported Content-Encoding` |
| `*http.MaxBytesError` (over 5 MiB as sent) or `ErrBodyTooLarge` (over the decoded cap) or `ErrTooManyItems` | `413` | `Request body too large` / `Too many envelope items` |
| `gzip.ErrHeader`, `gzip.ErrChecksum`, `zlib.ErrHeader`, `zlib.ErrChecksum`, `zlib.ErrDictionary`, `flate.CorruptInputError`, or `io.ErrUnexpectedEOF` from a decoder → wrapped as `ErrCorruptBody` | `400` | `invalid compressed request body` |
| `ErrMalformedEnvelope` (§4.3) | `400` | `malformed envelope: <reason>` |

`DecodeBody` and `capReader.Read` classify every error they see, so `ReadEnvelope` never needs to know which decoder
ran: an `*http.MaxBytesError` is returned **unchanged** (a decoder passes its source's error through; **run**:
`errors.As(err, &*http.MaxBytesError)` holds after `gzip.NewReader` + `io.Copy` over a `MaxBytesReader`); `io.EOF` is
passed through; every other decoder error — including `io.EOF` from `gzip.NewReader` / `zlib.NewReader` on an **empty**
body, which `DecodeBody` sees because those constructors read the header eagerly — becomes `ErrCorruptBody`. A size
error always wins over a malformed-envelope error found later, because the reader returns it the moment it happens.

### 4.2 The envelope handler, end to end

```go
// Ingester is the slice of *service.IngestService the handlers call: IngestAt with a publish budget (§4.7).
type Ingester interface {
	IngestAtWithin(ctx context.Context, payload *validation.ErrorPayload, receivedAt time.Time,
		budget int, shed service.Shedder) error
}

type Config struct {
	Ingest        Ingester
	Scope         func(ctx context.Context, p *validation.ErrorPayload) error // main.go: applyAuthenticatedScope
	Logger        *slog.Logger                                               // nil → slog.Default()
	RecordItem    func(ctx context.Context, itemType, outcome string)         // nil → no-op (§4.6)
	PublishBudget int                                                        // 0 → service.MaxPublishBytes (§4.7); tests pass a smaller one
}
```

`NewEnvelopeHandler(cfg Config) http.Handler`:

1. `body := http.MaxBytesReader(w, r.Body, MaxCompressedBodyBytes)`.
2. `rd, err := DecodeBody(body, r.Header.Get("Content-Encoding"), MaxDecompressedBodyBytes)` → on error, `statusFor`.
3. `env, err := ReadEnvelope(rd)` (§4.3) → on error, `statusFor`. **The whole envelope is read before any event is
   ingested**, so every `4xx` of §4.1 and §4.3 is decided before anything is published. Memory is bounded by the
   decoded cap: event payloads are at most 1 MiB each and nothing else is buffered.
4. For every item, in order: a non-event item is counted `discarded`; an oversized event item `too_large` (§4.6).
5. `receivedAt := time.Now().UTC()` (one reading per request, as `/ingest/batch`) and
   `ctx := enrich.WithBudget(r.Context(), enrich.NewBudget())`.
6. For every kept event item, in order:
   1. `p, err := MapEvent(item.Payload, env.Header)` (§5). `ErrInvalidEvent` (the payload is not a JSON object) →
      count `rejected`, answer `400 "invalid event payload"`, stop. Then `relay.Apply(p)`, where
      `relay := RelayIdentityFromRequest(r, cfg.Logger)` was resolved once before the loop (§3.7).
   2. `cfg.Scope(ctx, p)` (`applyAuthenticatedScope`) → error → count `rejected`, `403` with its text, stop. (It cannot fail for a key
      that passed A4, because the mapper never sets `project_key`; the branch is kept so the handler has one shape.)
   3. `cfg.Ingest.IngestAtWithin(ctx, p, receivedAt, budget, Shed)` (§4.7; `budget` is `cfg.PublishBudget`, or
      `service.MaxPublishBytes` when that is 0) → the error is classified in this order: `errors.Is(err,
      service.ErrEventTooLarge)` **or** `errors.Is(err, nats.ErrMaxPayload)` (NATS refused the message, T26) → count
      `too_large`, `413 "Event too large"`, stop (D13: never a `500`, so core's relay does not redeliver a poison
      envelope); text containing `validation failed` → count `rejected`, `400` with the error text (as
      `handleIngest`); any other error → count `failed`, `500 "Failed to ingest event"`, stop. On success count
      `accepted` and remember `p.EventID` (the **effective** id `IngestAt` stamped).
7. `200`, `Content-Type: application/json`, body `{"id":"<first accepted effective id>"}`, or `{}` when no event was
   accepted (no event item, or only oversized ones). The body is written with `json.NewEncoder(w).Encode`.

A failure in step 6 leaves the events before it published. A client that re-sends the envelope re-sends the same
`event_id`s, which the processor stores once (T14); sentry clients send one event per envelope, so this is an edge.
Logging follows `handleIngest`: `logger.ErrorContext(r.Context(), …)` on a failure with the error and `project_key`,
never the key, the DSN or the body.

### 4.3 The envelope reader

`apps/ingestor-go/sentrycompat/envelope.go`:

```go
var (
	ErrMalformedEnvelope = errors.New("malformed envelope") // wrapped with a reason: fmt.Errorf("%w: …", …)
	ErrTooManyItems      = errors.New("too many envelope items")
)

type EnvelopeHeader struct {
	EventID string              // header "event_id" when it is a JSON string, else ""
	SDK     *validation.SDKInfo // header "sdk" {name, version} when an object with a string name or version, else nil
}

type Item struct {
	Type      string // the item header's "type", verbatim
	Payload   []byte // set only for a kept event item
	Oversized bool   // an event item over MaxEventItemBytes: read past, not kept
}

type Envelope struct {
	Header EnvelopeHeader
	Items  []Item
}

func ReadEnvelope(r io.Reader) (*Envelope, error)
```

The reader wraps `r` in a `bufio.Reader` and uses two line helpers. Both return the bytes up to (not including) the
next `\n` or EOF and strip one trailing `\r`; they differ only past `max`:
- `readHeaderLine(max int) ([]byte, error)` returns `ErrMalformedEnvelope` ("line too long") once the line exceeds
  `max` bytes — for the envelope header and item headers;
- `readPayloadLine(max int) (line []byte, over bool, err error)` never fails for length: past `max` it stops
  buffering, discards the rest of the line, and returns `over = true` — for newline-framed event payloads.

Errors from the underlying reader (§4.1's size and corruption errors) are returned unchanged from every read.

1. **Envelope header.** `readHeaderLine(MaxHeaderLineBytes)`. An empty body (EOF with no bytes) → `ErrMalformedEnvelope`
   ("empty envelope"). The line must `json.Unmarshal` into `map[string]json.RawMessage` (so it must be a JSON object;
   `{}` is valid) → else `ErrMalformedEnvelope` ("invalid envelope header"). Only `event_id` and `sdk` are read; `dsn`,
   `sent_at`, `trace` and every other key are ignored and never logged.
2. **Items**, until EOF:
   1. `readHeaderLine(MaxHeaderLineBytes)` → at EOF with no bytes, stop. A line that is empty or whitespace only is skipped
      (a stray blank line between items, or a trailing newline, is tolerated).
   2. Count it: the 101st item header → `ErrTooManyItems` (`413`).
   3. The line must be a JSON object whose `type` is a non-empty JSON string → else `ErrMalformedEnvelope` ("invalid
      item header"). `length`, when present, must be a JSON number that is an integer in `[0, MaxDecompressedBodyBytes]`
      → else `ErrMalformedEnvelope` ("invalid item length"). Other keys (`content_type`, `filename`, `attachment_type`,
      …) are ignored.
   4. **With `length`**: exactly `length` payload bytes follow. An `event` item with `length ≤ MaxEventItemBytes` is
      read with `io.ReadFull` into `Payload`; every other item (and an `event` over the cap, marked `Oversized`) is
      skipped with `io.CopyN(io.Discard, …, length)`. Fewer bytes than `length` before EOF → `ErrMalformedEnvelope`
      ("truncated item"). Then one byte is read: EOF ends the envelope; `\n` continues; `\r` followed by `\n` continues;
      anything else → `ErrMalformedEnvelope` ("missing newline after item").
   5. **Without `length`**: the payload runs to the next `\n` or EOF. An `event` item is read with
      `readPayloadLine(MaxEventItemBytes)`; `over` marks the item `Oversized` (not an error). Every other item is discarded to the next `\n` without buffering.
3. The type comparison is exact and case-sensitive (`"event"`); `"Event"` is an unknown type.

A header-only envelope (no items) is valid and yields `200 {}`. Both framings, a missing trailing newline, and a mix
of `length` and newline items in one envelope are accepted (P3.1-P3.3).

### 4.4 Item types

| Item type | Handling | Metric `type` |
|---|---|---|
| `event` | mapped (§5) and ingested; over 1 MiB → dropped | `event` |
| `transaction`, `span`, `session`, `sessions`, `client_report`, `replay_event`, `replay_recording`, `replay_video`, `attachment`, `profile`, `profile_chunk`, `check_in`, `log`, `statsd`, `feedback`, `user_report` | read past, discarded, never an error | the type itself |
| anything else (including `security`, `otel_log`, a misspelt `Event`) | read past, discarded, never an error | `unknown` |

Non-event items never change the response: an envelope of only a transaction and a client report answers `200 {}`
(contract D7).

### 4.5 The store handler

`NewStoreHandler(cfg Config) http.Handler`: steps 1-2 of §4.2 with `max = MaxEventItemBytes`; `io.ReadAll(rd)`; an
over-cap body is `413` and counted `too_large` (V4); then steps 5-7 of §4.2 with the body as the one event item and an
empty `EnvelopeHeader`, the publish budget included. The response shapes are the envelope's: `200 {"id": …}`, `400`,
`403`, `413` (§4.7), `500`.

### 4.6 The metric

```go
// metrics.go
const (
	MetricItems = "sentinel_compat_items_total"
	LabelType   = "type" // one of ItemTypes, or "unknown"
	// outcomes (label obs.LabelOutcome = "outcome")
	OutcomeAccepted  = "accepted"  // an event item IngestAt published
	OutcomeDiscarded = "discarded" // a non-event item, read past
	OutcomeTooLarge  = "too_large" // an event item over MaxEventItemBytes (envelope) or a store body over it, or a
	                               // mapped event over the publish budget after shedding / refused by NATS (§4.7, D13)
	OutcomeRejected  = "rejected"  // not a JSON object, refused by Config.Scope, or an IngestAt validation failure
	OutcomeFailed    = "failed"    // any other IngestAt error (publish, marshal)
	OutcomeSkipped   = "skipped"   // a kept event item after the one that stopped the envelope (§4.2 step 6)
)

var ItemTypes = []string{"event", "transaction", "span", "session", "sessions", "client_report", "replay_event",
	"replay_recording", "replay_video", "attachment", "profile", "profile_chunk", "check_in", "log", "statsd",
	"feedback", "user_report"}

// ItemTypeLabel maps a client item type onto the bounded label set.
func ItemTypeLabel(t string) string

// NewItemRecorder creates the Int64Counter MetricItems ("Sentry-compatible envelope items by type and outcome",
// unit "{item}") and returns the function the handlers call. A creation error is returned; main.go logs it and
// passes a no-op recorder, as it does for sentinel_ingest_requests_total.
func NewItemRecorder(meter metric.Meter) (func(ctx context.Context, itemType, outcome string), error)
```

`Config.RecordItem` is that function (nil is a no-op). **The one counting rule:** every item `ReadEnvelope` returned
is counted exactly once, with the first outcome that applies — `discarded` (a non-event item) and `too_large` (an
oversized event item) at §4.2 step 4, before any event is ingested; then each kept event item in step 6: `rejected`
(its payload is not a JSON object, `Config.Scope` refused it, or `IngestAt` failed validation), `too_large` (over the
publish budget after shedding, or `nats.ErrMaxPayload`, §4.7), `failed` (any other `IngestAt` error), `accepted`; and
every kept event item after the one that stopped step 6 is `skipped`. An event that was shed and then published is
`accepted` (the shed is recorded in its markers and a log line, §4.7, not in this counter). A request
refused before `ReadEnvelope` returned (`400`/`413`/`415`) returned no items and counts nothing; the whole request is
already counted by `sentinel_ingest_requests_total{outcome="rejected"}` (T2). On `/store/` the body **is** the one
item, so the same rule counts it: an over-cap store body is its `too_large` (V4), a store body that cannot be read
(`400`/`415`, or `413` from the 5 MiB compressed cap) counts nothing. No label carries a project, a key or a path.

### 4.7 The publish budget (D13)

A mapped event can be far under the 1 MiB item cap and still marshal past NATS `max_payload` (T26). Contract D13: on
compat routes only, an event whose proto would exceed the SDKs' 1,000,000-byte publish budget is **shed** toward it in
a fixed order; what still does not fit is `413` `too_large`, never `500`. `/ingest` and `/ingest/batch` are unchanged:
no budget, no measurement, no shedding (P4.10).

**In the service** (`apps/ingestor-go/service/service.go`), because the proto exists only inside `IngestAt`:

```go
// MaxPublishBytes is the largest message a budgeted caller publishes: NATS's default max_payload (1,048,576) less
// 48,576 bytes for headers and framing -- the budget sdk-go (publishcost.go maxPublishBytes) and sdk-js
// (proto-size.ts NATS_PUBLISH_BUDGET) already hold to.
const MaxPublishBytes = 1_000_000

// MaxShedRounds caps how many times IngestAtWithin sheds and re-marshals one event.
const MaxShedRounds = 4

// ErrEventTooLarge: the marshaled event is still over the caller's budget when the Shedder has nothing left to
// remove or MaxShedRounds is spent.
var ErrEventTooLarge = errors.New("event too large to publish")

// Shedder removes content from a payload that has been through the bounds step, in a fixed order, aiming to shrink
// its proto by at least over bytes. ev is p as MapPayloadToEvent mapped it, for measuring. It reports whether it
// removed anything.
type Shedder func(p *validation.ErrorPayload, ev *sentinelv1.ErrorEvent, over int) bool

// IngestAtWithin is IngestAt for a caller that must publish under budget bytes (the sentry-compat routes, D13).
func (s *IngestService) IngestAtWithin(ctx context.Context, payload *validation.ErrorPayload, receivedAt time.Time,
	budget int, shed Shedder) error
```

`IngestAt`'s body moves unchanged into an unexported `ingestAt(ctx, payload, receivedAt, budget, shed)`. `IngestAt`
calls it with `(0, nil)`, and a budget of `0` skips the measurement entirely, so `/ingest` and `/ingest/batch` run
exactly today's statements (P4.10). With a budget, after the first `proto.Marshal`:

```go
for round := 0; budget > 0 && len(data) > budget; round++ {
	if shed == nil || round == MaxShedRounds || !shed(payload, event, len(data)-budget) {
		return fmt.Errorf("%w: %d bytes, budget %d", ErrEventTooLarge, len(data), budget)
	}
	event = mapping.MapPayloadToEvent(payload)
	if err := s.validator.Validate(event); err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	if data, err = proto.Marshal(event); err != nil {
		return err
	}
}
// then, only when round > 0: one Warn line, event=ingest.event_shed, with project_key, event_id, bytes before
// and after -- never content -- and the unchanged s.publish(ctx, data).
```

The bounds step is not re-run: shedding only removes values, so everything it guaranteed still holds, and the
protovalidate re-check is there to prove it (a failure would be a `400`, never a publish). The markers the shedder adds
may take `metadata` a few KiB past `MaxMetadataSize`; that bound is a shrink, not a rejection (T10), so nothing refuses
it. `recordFieldsTruncated` has already run by then and does not count what was shed; the markers and the log line are
the record. The re-marshal cost is bounded: at most `MaxShedRounds` further marshals of a message the 1 MiB item cap keeps to a
few MB (T26's expansion), and in practice one, because each round aims at the whole overage.

**The shedder** (`apps/ingestor-go/sentrycompat/shed.go`, new): `func Shed(p *validation.ErrorPayload, ev
*sentinelv1.ErrorEvent, over int) bool`, the `Shedder` both handlers pass. One call walks the steps below **in
order**, removing elements until the sum of their estimates reaches `target = over + ShedSlack` (`ShedSlack = 16 <<
10`, room for the markers it writes), and moves to the next step only when a step has nothing left; it returns
`true` if it removed anything. A later round (only when the re-marshal is still over) continues from what remains.
Each estimate is a **lower bound** of the bytes the removal saves (removing an element also saves its tag and length
prefix), so one round normally suffices.

| # | Removes | Order within the step | Estimate per element | Marked in `server_truncated` / entries in `server_truncated_keys` |
|---|---|---|---|---|
| B1 | breadcrumbs | oldest first (the bounds step keeps them oldest first, so index 0) | `proto.Size(ev.Breadcrumbs[i])` | `breadcrumbs` / one entry `breadcrumbs:<n>`, n = crumbs removed in this call |
| B2 | `metadata` keys (the body's `extra`, M19) | largest estimate first, ties to the lexically smallest key; **never** the two markers or a key `validation.IsMetadataExemptKey` names (the keys the processor routes on) | `len(k) + proto.Size(ev.Metadata.Fields[k])` | `metadata` / each key, bare (as the bounds step's `shrinkMetadata` records it) |
| B3 | contexts other than the well-known `device`, `os`, `app`, `runtime`, `browser`, `culture` | largest estimate first, ties to the lexically smallest name | `len(name) + proto.Size(ev.Contexts[name])` | `contexts` / `contexts.<name>` (as `boundContexts` records it) |
| B4 | exception values other than the **last** one (the titled one, D3) | oldest first | `proto.Size(ev.Exception.Values[i])` | `exception` / one entry `exception.values:<n>` |
| B5 | every frame's `context_line`, `pre_context` and `post_context`, chain and top-level | all at once (one step) | — | `stacktrace.context` / one entry `stacktrace.context` |

**Never shed**, so what the event is and how it groups survive: the top-level stacktrace's frames and their `file`,
`function`, `line`, `in_app`, `module`, `abs_path` (T13's grouping inputs and their neighbours); the titled
exception's `type`, `value` and `mechanism`; `error_class`, `message`, `level`, `environment`, `release_version`,
`platform`, `trace_id`, `span_id`, `user`, `tags` (the relay's identity included, §3.7.4), `request`, `sdk`, the
well-known contexts, the exempt metadata keys and the markers. When B5 is done and the message is still over, the
event is `ErrEventTooLarge` → `413`.

The markers are written with one new exported helper in `apps/ingestor-go/validation/bounds.go` (the bounds step
itself is unchanged): `func MarkServerTruncated(p *ErrorPayload, field string, keys ...string)` adds `field` to
`server_truncated` (vocabulary order, no duplicates) and appends `keys` to `server_truncated_keys` under the bounds
step's own cap (`MaxServerTruncatedKeys` entries of at most `MaxServerTruncatedKeyBytes`, then one `+N more`, whose `N`
it reads back from an existing list). A second helper, `func IsMetadataExemptKey(k string) bool`, exposes
`metadataExemptKeys` so B2 and the bounds step exempt the same keys. The entries of B1, B4 and B5 are fixed patterns
(`breadcrumbs:<n>`, `exception.values:<n>`, `stacktrace.context`) beside the bounds step's `tags.<k>`, `contexts.<k>`
and bare metadata keys.

**What the two measured cases become** (T26; P4.8 rebuilds both): the numeric-array event sheds breadcrumbs only, oldest
first, until it fits, and keeps its `extra` and contexts; the 70-frame event has too little in B1-B4 to matter, so it
sheds those (every crumb, the non-well-known contexts) and then every frame's context lines, and publishes all 70
frames crash-first with `file` and `function` intact. Both answer `200`; neither can answer `500`.

**Belt and braces.** A message NATS still refuses (a server configured below 1,048,576, or header growth the budget
did not foresee) returns `nats.ErrMaxPayload` wrapped with `%w` (T26), and §4.2 step 6.3 maps it to the same `413`
`too_large`. `/ingest` keeps answering that error `500` (contract: unchanged).

---

## 5. Sentry event → ErrorEvent v2 (TASK-04)

### 5.1 Shape of the mapper

```go
// mapper.go
var ErrInvalidEvent = errors.New("invalid event payload")

// Source is the value MapEvent writes to ErrorPayload.Source.
const Source = "sentry-compat"

// MapEvent maps one Sentry event JSON onto a validation.ErrorPayload that IngestAt accepts. It fails only when raw
// is not a JSON object. project_key and project_id are left empty: identity comes from the key (Config.Scope).
func MapEvent(raw []byte, env EnvelopeHeader) (*validation.ErrorPayload, error)
```

**Decoding discipline** — these four rules are what make "never a 400" and "never silently dropped" true (T9, T11):
- **D-a. Field by field.** `raw` is decoded once into `map[string]json.RawMessage`; each field below is decoded on its
  own. A field of the wrong JSON type is treated as absent — it never fails the event. (A duplicate key: the last
  wins, `encoding/json`'s rule.)
- **D-b. Free-form values come only from `encoding/json` into `interface{}`** (`map[string]interface{}`,
  `[]interface{}`, `string`, `float64`, `bool`, `nil`), never as a typed Go map or slice (`structpb` rejects those,
  T11) and never with `UseNumber`: a `json.Number` such as `1e400` decodes, then fails `structpb.NewValue`
  (`Float64` overflows), and `MapPayloadToEvent` drops the **whole** field silently (T11). Decoding into `float64`
  instead fails at decode time, where the mapper can confine the loss. **The unit of loss is the smallest entry**:
  `extra` is decoded key by key and `contexts` context by context (each first into `map[string]json.RawMessage`), so a
  value `encoding/json` cannot hold (a number beyond float64, nesting over 10,000) drops that one `extra` key or that
  one context; `request.data` and each crumb's `data` are single values and are dropped whole.
- **D-c. Strings are valid UTF-8 and carry no U+0000 — from the moment they are decoded.** `encoding/json` already
  replaces invalid UTF-8 with U+FFFD. U+0000 is removed **inside the decoding helpers** (`str`, `stringify`, and the
  free-form decode, keys included), so every rule below — E2's empty check, F1's file, F7's `in_app`, M7's regex —
  reads the cleaned value: an exception `type` of `"\u0000"` is empty (E2 falls through), and a frame whose
  `abs_path` is `"\u0000"` has no file (F7 → `in_app` false). The one exception is `event_id` (M1), passed raw so
  `resolveEventID` re-mints it (T8). Every truncation below cuts on a rune boundary, so `proto.Marshal` never meets
  invalid UTF-8 (T9).
- **D-d. Every protovalidate rule is satisfied by construction** (§5.6). The bounds step is left to do what it does for
  every SDK (T10); the mapper pre-empts it only where the server would reject instead.

Helpers used below (all strip U+0000, D-c): `str(v)` — a JSON string, else `""`; `num(v)` — a JSON number as
`float64`, else absent; `stringify(v)` — a JSON string as is, a number as its **literal JSON text** (the raw bytes, so
`12345678901234567890` and `1.0` survive unchanged; never through `float64`), a bool as `"true"`/`"false"`, anything
else absent; `truncRunes(s, n)` — at most `n` code points; `truncBytes(s, n)` — at most `n` bytes, cut back to
a rune boundary; `firstLine(s)` — the first line of `s` whose `strings.TrimSpace` is non-empty, trimmed, or `""`.

### 5.2 Event-level fields

| # | Sentry (JSON type accepted) | `ErrorPayload` field (Go type) | Conversion |
|---|---|---|---|
| M1 | `event_id` (string) | `EventID string` | verbatim; absent or not a string → `env.EventID` (the envelope header's); still empty → `""` (IngestAt mints, T8). A 32-hex id, a dashed UUID or anything else is passed as sent; `resolveEventID` re-mints what is over 64 code points or holds a control character. |
| M2 | `timestamp` (number **or** string) | `Timestamp time.Time` | §5.5 `parseSentryTime`; zero when absent or unparseable → IngestAt replaces it with the receipt time (`resolveTimestamp`'s first rule, T8). |
| M3 | `platform` (string) | `Platform string` | `p := str(platform)`; if `p` fails `^[a-z0-9]+$` or is over 50 bytes (so also when empty) → `"other"`; then if `p == "other"` and the SDK name (M14's, else `env.SDK.Name`) starts with `sentry.dart` → `"dart"` (D6). No lower-casing: `"Dart"` → `"other"` → (sentry.dart) `"dart"`. |
| M4 | `level` (string) | `Level string` | `l := strings.ToLower(strings.TrimSpace(str(level)))`; `"log"` → `"info"`; every other value as is. The bounds step blanks and marks anything outside `fatal/error/warning/info/debug` (T10); absent → `""`, stored as `error`. |
| M5 | `logger`, `server_name`, `transaction`, `dist` (string) | `Logger`, `ServerName`, `Transaction`, `Dist string` | `str(…)`; the bounds step truncates (T10). |
| M6 | `release` (string) | `ReleaseVersion string` | `truncRunes(str(release), 100)` — over 100 is a 400 otherwise (T9). |
| M7 | `environment` (string) | `Environment string` | `e := strings.ToLower(strings.TrimSpace(str(environment)))`; if `e` fails `^[a-z0-9][a-z0-9._-]{0,49}$` (so also when absent) → `"production"`, Sentry's default. |
| M8 | `message` (string, **or** object `{formatted, message}`) and `logentry` (object `{formatted, message}`) | used by M9/M10 | "message text" = the first non-empty of `logentry.formatted`, `logentry.message`, `message` (a string), `message.formatted`, `message.message`. `params` are not substituted. |
| M9 | `exception` — see §5.3 | `Exception *ExceptionInterface`, `ErrorClass`, `Message`, `Stacktrace` | §5.3. |
| M10 | no usable exception | `Message string`, `ErrorClass string` | `Message = truncRunes(message text, 10000)`; `ErrorClass = truncRunes(firstLine(message text), 255)`, or `"UnknownError"` when that is empty. |
| M11 | `threads` (object `{values: [...]}` or array) | `Stacktrace []StackFrame` (fallback only) | §5.3 S2. |
| M12 | `breadcrumbs` (object `{values: [...]}` **or** array) | `Breadcrumbs []Breadcrumb` | each element that is an object → `{Timestamp: parseSentryTime(timestamp), Type: str(type), Category: str(category), Level: M4's rule on its level, Message: str(message), Data: data when a JSON object (D-b), else nil}`; other elements skipped; order kept (oldest first, as the bounds step expects). Empty → nil. A crumb with a zero timestamp is published without one (T11). |
| M13 | `user` (object) | `User *UserInfo` | `ID: stringify(id)` (Sentry allows a numeric id); `Username`, `Email`, `IPAddress: str(…)` (`"{{auto}}"` passes through verbatim; enrichment resolves it, T12); `Geo: &GeoInfo{City, Region, CountryCode, Subdivision: str(…)}` when `geo` is an object, else nil. Other user keys (`name`, `segment`, `data`, …) are dropped. Nil when every field is empty and `geo` is nil. |
| M14 | `sdk` (object) | `SDK *SDKInfo` | `{Name: str(name), Version: str(version)}`; `integrations`, `packages` and the rest dropped. When the event has no `sdk` object, `env.SDK`. Nil when both are empty. |
| M15 | `tags` (object **or** array of `[key, value]` pairs) | `Tags map[string]string` | object: each value `stringify`d, absent ones dropped. Array: each element that is a 2-element array whose first element is a string → `key: stringify(second)`; other elements skipped; a later pair for the same key wins. The key `relay_auth` is then deleted, in both forms (D14, §3.7.4). The bounds step drops what fails the key or value rule (T10). Empty → nil. |
| M16 | `contexts` (object) | `Contexts map[string]map[string]interface{}` | every entry whose value is a JSON object (D-b), verbatim; other entries dropped. Empty → nil. |
| M17 | `contexts.trace.trace_id`, `contexts.trace.span_id` (string) | `TraceID`, `SpanID string` | when a string of 1-64 code points, else `""`. The `trace` context itself stays in `Contexts`. |
| M18 | `request` (object) | `Request *RequestInfo` | §5.4 Q1-Q5. |
| M19 | `extra` (object) | `Metadata map[string]interface{}` | verbatim (D-b) when a JSON object; else nil. The bounds step shrinks it over 64 KiB (T10). |
| M20 | `fingerprint` (array, **or** a string) | `Fingerprint string` | D5: collect the elements (a lone string is one element) that `stringify`; none → `""`. If any element, with every whitespace character removed and lower-cased, equals `{{default}}` → `""` (the server computes). Otherwise `hex(sha256(strings.Join(elements, "\n")))[:32]`. Examples: `["{{ default }}"]` → `""`; `["checkout", "timeout"]` → `eb3452a2abffd1c9c66915517b6ac1b0`; `["{{ default }}", "x"]` → `""` (no partial-default composition, D5). |
| M21 | — | `Source string` | `"sentry-compat"`. |
| M22 | `modules`, `debug_meta`, `_meta`, `sdk.integrations`, `sdk.packages`, `errors`, `grouping_config`, `culprit`, `type`, `received`, `server_name`-less host keys, `spans`, `measurements`, and every key not named above | — | dropped. |
| M23 | — | `ProjectKey`, `ProjectID`, `TraceFlags`, `FingerprintOverride`, `ReceivedAt` | left zero; `Config.Scope` sets the first two from the key, `IngestAt` the last. |

### 5.3 Exceptions, frames order, and the top-level stacktrace

| # | Rule |
|---|---|
| E1 | `exception` is an object with a `values` array, or an array. Each element that is an object becomes an `ExceptionValue{Type: str(type), Value: str(value), Mechanism, Stacktrace}` (the bounds step truncates `type` to 255 and `value` to 10,000 code points, T10); non-object elements are skipped. `Mechanism` is `{Type: str(mechanism.type), Handled: *bool}` when `mechanism` is an object — `Handled` set only when `mechanism.handled` is a JSON bool — else nil. `Stacktrace` is `stacktrace.frames` mapped by §5.4 and **reversed** to crash-first. Order of the values is kept (oldest first; the raised one last). No element → `Exception` nil and M10 applies. |
| E2 | The **last** value (D3) is the one the event is titled by. `ErrorClass = truncRunes(t, 255)` where `t = strings.TrimSpace(last.Type)`; when `t` is empty, `t = firstLine(last.Value)` (V2); when that is empty too, M10's `ErrorClass` rule over the message text, then `"UnknownError"`. |
| E3 | `Message = truncRunes(last.Value, 10000)`; when `last.Value` is empty, the message text (M8), truncated the same way. |
| E4 | `module` of an exception value is dropped (T7). |
| S1 | Top-level `Stacktrace` = the last value's reversed frames, when it has at least one. |
| S2 | Otherwise the **threads fallback**: from `threads` (`{values: […]}` or an array), the first thread object with `crashed: true` and at least one frame; else the first with `current: true` and at least one frame; else the first with at least one frame. Its `stacktrace.frames`, mapped by §5.4 and reversed, become the top-level `Stacktrace`. No such thread → nil. Threads are otherwise dropped (no `threads` field exists in v2). |
| S3 | The top-level `Stacktrace` keeps its **first 100** frames (the crash end; the bounds step keeps chain frames the same way, T10): over 100 is a 400 otherwise (T9). |

### 5.4 Frames and request

**Frames** (`mapFrame(obj map[string]json.RawMessage) validation.StackFrame`), for top-level and chain frames alike:

| # | Sentry frame | `StackFrame` | Rule |
|---|---|---|---|
| F1 | `abs_path`, `filename` (string) | `File string` | `abs_path` when non-empty, else `filename` (D4); then `truncBytes(…, 512)` (`docs/sdk-specification.md` §3's SDK-side rule for `stacktrace[].file`, which the server does not enforce on top-level frames). |
| F2 | `abs_path` | `AbsPath string` | verbatim (bounds truncate to 1,024). |
| F3 | `function` | `Function string` | `truncBytes(str(function), 255)` (the SDK-side rule for `stacktrace[].function`). |
| F4 | `module`, `context_line` | `Module`, `ContextLine string` | `str(…)` (bounds truncate). |
| F5 | `lineno`, `colno` (number) | `Line`, `Column int32` | `n := num(…)`; when present, finite and `1 ≤ n ≤ 2147483647`, `int32(math.Trunc(n))`; otherwise `0`. So a negative `colno` never reaches `ValidatePayload`'s top-level `column ≥ 0` rule (T9), and a value outside `int32` never exists. |
| F6 | `pre_context`, `post_context` (array) | `PreContext`, `PostContext []string` | each element `str(…)` (a non-string element becomes `""`, so line positions survive); not an array → nil (bounds keep 5 lines). |
| F7 | `in_app` (bool) | `InApp bool` | `true` only when the value is JSON `true` **and** F1's `File` is non-empty (an in-app frame without a file is never a 400, T9; V3). |
| F8 | `platform`, `package`, `data`, `vars`, `instruction_addr`, `symbol_addr`, `image_addr`, `raw_function`, `symbol`, `lock`, and every other key | — | dropped. |

**Request** (`request` object → `*RequestInfo`; nil when every field is empty):

| # | Sentry | `RequestInfo` | Rule |
|---|---|---|---|
| Q1 | `method`, `url` (string) | `Method`, `URL string` | `str(…)` (bounds truncate). |
| Q2 | `query_string` (string, object, or array of pairs) | `QueryString string` | string → verbatim with one leading `?` removed; object → keys sorted, `url.QueryEscape(k) + "=" + url.QueryEscape(stringify(v))` joined by `&`; pairs → the same, in array order. |
| Q3 | `headers` (object, or array of `[name, value]` pairs) | `Headers map[string]string` | object → `stringify`d values; pairs → `name: stringify(value)`, a repeated name's values joined with `", "` in array order. Names kept as sent. |
| Q4 | `data` (any JSON) | `Data interface{}` | the decoded value (D-b); JSON `null` or absent → nil. |
| Q5 | `env` (object) | `Env map[string]string` | `stringify`d values. `cookies`, `fragment`, `inferred_content_type`, `api_target` and the rest are dropped. |

### 5.5 Timestamps

```go
// parseSentryTime converts a Sentry timestamp (event or breadcrumb) to UTC; the zero time means "none".
func parseSentryTime(raw json.RawMessage) time.Time
```

1. A JSON **number** `f` (Unix seconds, fractional): if `f ≤ 0` or `f ≥ 253402300800` (year 10000), zero; else
   `time.UnixMicro(int64(math.Round(f * 1e6))).UTC()` — microsecond precision, rounded, so `1790329312.838` is exactly
   `2026-09-25T09:41:52.838Z` (float64's error at this magnitude is below 1 µs; the rounding removes it).
2. A JSON **string** `s`: `time.Parse(time.RFC3339Nano, s)` (any offset, converted to UTC); else
   `time.Parse("2006-01-02T15:04:05.999999999", s)` read as UTC (Sentry accepts a zone-less ISO time as UTC); else
   `strconv.ParseFloat(s, 64)` and rule 1; else zero.
3. Anything else → zero.

The year bound keeps `time.Time` inside what `timestamppb` and `MarshalJSON` accept. The event timestamp is then
clamped by `IngestAt` (`resolveTimestamp`, T8); crumb timestamps are stored as mapped.

### 5.6 Where a 400 could still come from, and how the mapper prevents it

Every rule of T9, against what §5.2-§5.5 guarantee. A mapped event is therefore never rejected by `IngestAt` for its
content; P4.5 proves it on a table of hostile events. Its **size** is §4.7's: shed to the publish budget, else `413`,
never a `500` (P4.8, P4.9).

| Rule that rejects (T9) | Prevented by |
|---|---|
| `error_class` empty (`required`) | E2 / M10: always non-empty (`UnknownError` last) |
| `error_class` over 255 | E2 / M10 `truncRunes(…, 255)` |
| `message` over 10,000 code points | E3 / M10 `truncRunes(…, 10000)` |
| `platform` not `^[a-z0-9]+$` or over 50 | M3 |
| `environment` not `^[a-z0-9][a-z0-9._-]{0,49}$` | M7 (fallback `production`) |
| `release_version` over 100 | M6 |
| `trace_id` / `span_id` over 64 | M17 (dropped when over) |
| `fingerprint` over 64 | M20 (always `""` or 32 hex) |
| `event_id` over 64 or with control characters | not prevented — `resolveEventID` re-mints it (T8); never a 400 |
| `project_key` empty or over 64 | set from the authenticated key by `Config.Scope` |
| top-level `stacktrace` over 100 frames | S3 |
| `in_app` true with an empty `file` (top-level or chain) | F7 |
| top-level `column < 0` (`ValidatePayload`) | F5 |
| a JSON value of the wrong type (the `/ingest` decode) | not applicable: the payload is built in Go (D-a) |
| invalid UTF-8 at `proto.Marshal` (a `500`) | D-c |
| a message over NATS `max_payload` at publish (a `500` on `/ingest`, T26) | §4.7: shed to 1,000,000 bytes, else `413` `too_large`; `nats.ErrMaxPayload` is `413` too |
| `level`, tags, contexts, breadcrumbs (count, size, level), exception values (count, type/value length), user, sdk, request, frame v2 fields, metadata size | not a rejection: the bounds step repairs and marks them (T10) |

---

## 6. Dart grouping (TASK-05)

### 6.1 What the mapper does for Dart frames

- **Order.** Sentry sends frames outermost first (`main`, zone runners, isolate plumbing, then the app, then the
  crash). §5.3 reverses every frame list, so the processor's first usable in-app frames in array order (T13; algorithm `v2` hashes 5 of them, see
  `docs/sdk-specification.md` "How the server groups") are the ones **nearest the crash**. Without the reversal every error reached through one entry point hashes the
  same outer frames and collapses into one issue (contract §1 item 6); P5.2 is built to go red exactly then.
- **Identity of a frame.** `file` = `abs_path` (F1, D4), which for Dart is the full `package:<pkg>/<path>` or
  `dart:<lib>/<path>` URI, so `package:a/utils.dart` and `package:b/utils.dart` never collide (P5.3). `line` is never a
  grouping input (T13), so the same error at a different line in a later build is still one issue. `dart` is not in
  `SYMBOLICATE_PLATFORMS`, so the URI is hashed byte for byte (T13).
- **`in_app`.** F7: the SDK's `in_app` is kept when it is `true` and the frame has a file; everything else is `false`.
  For the contract's §4.4 first bullet this is the whole rule (V3): an unset flag on a `dart:` or `package:flutter/`
  frame becomes `false`, and on any other `package:` frame it is kept — and a kept "unset" is `false` in v2. sentry_flutter
  sets `in_app` on every frame in the real events (T22: `dart:` frames `false`; `package:daya/` frames `true`; the
  app's `inAppInclude` also marks `package:dio/` frames `true`, which Sentinel keeps as sent in the stored frame).
  Under grouping `v2` the processor skips `dart:` frames and every `package:` frame but the app's own (the package of the
  `main.dart` frame; without one, the well-known libraries `dio`, `flutter`, …) when choosing the frames to hash,
  whatever the flag says, so a `dio` frame flagged in-app no longer decides identity. When a
  client sends no in-app frame at all, the processor falls back to the first 5 usable frames (T13) — after the reversal,
  the crash end.
- **Platform.** `other` + `sentry.dart*` → `dart` (M3, D6), which enrichment treats as a client platform (T12).

### 6.2 Obfuscation

Not needed: mobile release builds are not obfuscated (contract §2, TASK-05 note 2026-09-27: no `--obfuscate` in
`build_and_test.sh` or CI). Recorded on TASK-05; nothing to build.

### 6.3 No processor change

Superseded by sentinel-grouping-v2 (2026-10-13). This section originally recorded that `Compute` needed no change for
Dart; the processor's rules have since changed (library-frame skipping, 5 frames, message for stackless or wrapper-top
events, the `v2` version token), and §6.1 above describes them. The mapper is unchanged: frames stay crash-first and
`file` stays the `package:` URI.

---

## 7. Processor

Unchanged (§6.3). Idempotency is the existing `(project_id, event_id)` unique insert (T14): a Sentry client's retry of
the same event carries the same `event_id`, the mapper passes it through verbatim (M1), and the processor stores one
occurrence.

---

## 8. The Sentry project id on project settings (TASK-06, side `fe`; shrunk by §1.4 AM2)

### 8.1 What renders, and where

`src/routes/[orgSlug]/projects/[projectId]/settings/+page.svelte` gains a `<section class="sentry-sdk"
id="sentry-sdk">` directly before the "Agent automation" section, titled `Sentry SDK`, showing the label
`Sentry project id` and the value `sentryProjectId(data.projectId)` in the page's monospace style
(`data-testid="sentry-project-id"`), with one line of help: "The numeric project id the Sentry-compatible ingest
routes expect for this project (`/api/<id>/envelope/`). Core's mobile relay is configured with it." The id is not
secret (T20) and is derived in the browser from `data.projectId`, which the page's load already returns; the load
function is not changed. When `sentryProjectId` throws (it cannot for a database UUID), the value shows `—`.

Nothing else in the dashboard changes: no DSN, no key-page change, no `ApiKeyCreateModal.svelte` change, no
configuration or chart value (§1.4 AM2).

### 8.2 The TS derivation

`apps/dashboard-web/src/lib/sentry-project-id.ts` (new):

```ts
/** §3.4's derivation: the decimal value of the first 13 hex digits of a project UUID, dashes removed. Throws on
 *  anything that is not a 32-hex or canonical 8-4-4-4-12 UUID. Must equal auth.SentryProjectID (Go) on
 *  tests/fixtures/sentry/project-id-vectors.json. */
export function sentryProjectId(uuid: string): string;
```

`Number.parseInt(hex13, 16)` is exact: 13 hex digits are below 2^53 (§3.4).

## 9. Docs

- `docs/sdk-specification.md` gains **`## 10. Sentry-compatible ingest`**, appended after `## 9. Directory Placement`
  (no existing section is renumbered or edited): the four routes and the 405 rule (§3.1); key presentation and
  precedence, the ingest-only rule, the status codes (§3.2-§3.3); the derived project id with two vector rows (§3.4); the rate-limit headers (§3.5); accepted encodings, the five limits and their status codes
  (§4.1-§4.3); the 1,000,000-byte publish budget, the shed order and its `413` (§4.7); the compat-only trusted-proxy
  list, empty by default (§3.6); the relay identity headers, when they are honoured, and that a body `relay_auth` tag
  is never kept (§3.7); the discarded item types (§4.4); the mapping tables (§5.2-§5.5, condensed to Sentry field → v2
  field → rule); the response shapes (§4.2 step 7); the metric (§4.6).
- `deploy/helm/sentinel/RUNBOOK.md` §4 Troubleshooting gains two entries:
  - `### Sentry-compatible ingest (sentinel-sentry-compat-ingest)`: what `sentinel_compat_items_total{type,outcome}`
    counts, that `rejected` or `failed` above zero means lost Sentry events (read the ingestor's `Failed to ingest`
    lines), that `too_large` means an event over 1 MiB as sent **or** one still over the 1,000,000-byte publish budget
    after shedding (§4.7; `413`, which core's relay must not retry), that `event=ingest.event_shed` Warn lines mean
    events were published with parts removed (the occurrence's `metadata.server_truncated_keys` says which), that a
    `403` on a compat route is a key that is not a project `ingest` key or a path project id that does not match the
    key's project, and where the derived id is shown (project settings, §8).
  - `### Sentry-compatible ingest: trusting core's relay (sentinel-sentry-compat-ingest)`: §3.6.4's procedure,
    verbatim in substance — what the value is and that every host inside it can assert any user and tenant, how to
    observe the immediate peer (`event=sentrycompat.relay_peer_untrusted`), the three topologies and what each
    implies, the **narrowest-CIDR** warning, where to set it (the AWS overlays' explicit `[]`), and the two-sided check.
    It names no CIDR value (D15).
- `README.md` "More documentation" gains one bullet pointing to `docs/sdk-specification.md` §10.

---

## 10. Fixtures

All under `tests/fixtures/sentry/` (new), plus `tests/fixtures/sentry/README.md` describing each file, its source and
the regeneration commands below.

### 10.1 The two real events, anonymized

`flutter-typeerror-unhandled.json` and `flutter-dio-error.json` are copied from `~/.cache/orch/p4-fixtures/` and then
edited **only** as follows (every other byte kept, so the shape stays real; the files are re-serialized with 2-space
indentation and keys in their original order):

| Field | Original (not reproduced) | Fixture value |
|---|---|---|
| `event_id` | two real 32-hex ids | `11111111111141118111111111111111` (typeerror), `22222222222242228222222222222222` (dio) |
| `contexts.device.id` | two real Android ids | `0000000000000001`, `0000000000000002` |
| `contexts.trace.trace_id` | one real trace id, shared | `0000000000000000000000000000c0de` in both (the relation is kept) |
| `contexts.trace.span_id` | two real span ids | `00000000000000a1`, `00000000000000a2` |
| `user.id` | one pseudonymous user id, shared | `user-00000001` in both (same user, as in the source) |
| `user.geo.country_code` | a real country | `ZZ` (a user-assigned ISO 3166 code); `city` and `region` are already `Example City` / `Example Region` |
| `contexts.flutter0.raw_description` | a public git URL (both files) | host replaced: `https://git.example.invalid/flutter/flutter.git` |
| `exception.values[0].value` (Dio event only) | one documentation URL on a real host | host replaced, path kept: `https://docs.example.invalid/en-US/docs/Web/HTTP/Status` |
| `contexts.os.kernel_version` | a kernel build string with a git hash | `5.10.237-android12-9-00000-g000000000000-ab00000000` |

Everything else is already fabricated or not an identifier (T22): request and crumb URLs are on `example.invalid`,
`os.build` uses `203.0.113.7` (RFC 5737 documentation space), `API Request Info` values are `REDACTED`, and the
remaining values describe a device model, an app build or public Flutter/Dart versions. `user.ip_address` stays the
`{{auto}}` literal.

**The anonymization check** (`tests/unit/sentry_compat_fixtures_test.go: TestSentryFixturesAreAnonymized`) walks every
file under `tests/fixtures/sentry/` (JSON, envelopes, and the compressed variants decompressed) and fails on:
- any IPv4 address outside `127.0.0.0/8`, `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24`, and any IPv6 address
  outside `2001:db8::/32` and `::1`;
- any e-mail address: a token containing `@` that `net/mail.ParseAddress` accepts **and** whose domain matches
  `(?i)^([a-z0-9-]+\.)+[a-z]{2,}$` — so the release `zasri.zasri@1.5.28+216` (which `ParseAddress` accepts, **run**,
  but whose "domain" has no alphabetic label) is not one — except the envelope header's fabricated `dsn`
  (`sent_live_fixture@ingest.example.invalid`);
- any URL host that is not `localhost`, not `example.invalid` or a subdomain of it, not `example.test` or a subdomain,
  and not `example.com` (the source files hold exactly three hosts, **run**: `api.example.invalid`, one `github.com` per
  file and one `developer.mozilla.org`; the last two are replaced above);
- any 16-, 32- or 64-hex token whose SHA-256 is in a committed deny list of the **hashes** of the original identifiers
  (the originals themselves are never written into the repository; the implementer computes the hashes from the source
  files once and commits only the hashes);
- `user.geo` values other than `Example City`, `Example Region`, `ZZ`.

### 10.2 Envelope fixtures

| File | Built from | Shape |
|---|---|---|
| `envelopes/flutter-9.6.0.envelope` | `flutter-typeerror-unhandled.json` | the framing of sentry-dart's `SentryEnvelope.envelopeStream` exactly (T21: the bytes the Dart layer produces on every platform; whatever re-framing a native SDK or the relay applies is outside this run, and §4.3 accepts both framings): header `{"event_id":"11111111111141118111111111111111","sdk":{"name":"sentry.dart.flutter","version":"9.6.0"},"trace":{"trace_id":"0000000000000000000000000000c0de","public_key":"sent_live_fixture"},"dsn":"https://sent_live_fixture@ingest.example.invalid/27690631804612","sent_at":"2026-09-25T09:41:53.000Z"}`; then `\n` + `{"content_type":"application/json","type":"event","length":N}` + `\n` + the event serialized compactly (N its byte length); then a `transaction` item (a minimal transaction JSON with its own `event_id` `33333333333343338333333333333333`) and a `client_report` item (`{"timestamp":"2026-09-25T09:41:53.000Z","discarded_events":[{"reason":"sample_rate","category":"transaction","quantity":1}]}`), each with `length`; **no trailing newline**. |
| `envelopes/flutter-9.6.0.envelope.gz` | the file above | `gzip -9n` |
| `envelopes/flutter-9.6.0.envelope.zlib` | the file above | `python3 -c 'import sys,zlib; sys.stdout.buffer.write(zlib.compress(sys.stdin.buffer.read(), 9))'` |
| `envelopes/flutter-9.6.0.envelope.deflate` | the file above | raw deflate: the same with `zlib.compressobj(9, zlib.DEFLATED, -15)` |
| `envelopes/nextjs-10.envelope` | hand-built from Sentry's envelope and event docs, shaped as `@sentry/nextjs` 10.69.0 sends it (T23) | header `{"event_id":"44444444444444448444444444444444","sent_at":"2026-09-25T10:00:00.000Z","sdk":{"name":"sentry.javascript.nextjs","version":"10.69.0"}}`; an item header **without** `length` (`{"type":"event"}`) and a one-line event: `platform: "javascript"`, `timestamp` float, `level: "error"`, `environment: "production"`, `release`, `exception.values` with one `TypeError` whose frames use `filename: "app:///_next/server/app/page.js"`, `function`, `lineno`, `colno`, `in_app`, `breadcrumbs` as a **plain array**, `tags` as an **object**, `request {method, url: "https://app.example.invalid/checkout", headers: {"User-Agent": "…", "Accept": "…"}}`, `contexts {trace, runtime: {name: "node", version: "v22.0.0"}}`, `sdk`; then a `{"type":"session"}` item and its one-line payload, **no trailing newline**. |
| `envelopes/nextjs-10.envelope.gz` | the file above | `gzip -9n` |
| `project-id-vectors.json` | §3.4 | as shown there |
| `flutter-typeerror-unhandled.payload.golden.json` | P4.1 | the mapper's output for the typeerror fixture, `json.MarshalIndent(payload, "", "  ")` |

The compressed variants are produced by non-Go tools on purpose, so the decoders are proven against another
implementation's output; `TestSentryEnvelopeVariantsMatch` (P3.2) proves each decompresses byte-identical to its
source, so a regenerated identity envelope with stale variants fails.

### 10.3 The staging envelope (contract §5, run by the orchestrator from inside the VPN)

After the release carrying this change is on AWS staging, with a project `ingest` key `KEY` created there and its
project UUID `PUUID`:

```bash
PID=$(python3 -c 'import sys;print(int(sys.argv[1].replace("-","").lower()[:13],16))' "$PUUID")
EID=$(python3 -c 'import uuid;print(uuid.uuid4().hex)')
NOW=$(python3 -c 'import time;print(f"{time.time():.3f}")')      # same byte length as the fixture's 1790329312.838
sed -e "s/11111111111141118111111111111111/$EID/g" -e "s/1790329312\.838/$NOW/" \
    tests/fixtures/sentry/envelopes/flutter-9.6.0.envelope > /tmp/staging.envelope
gzip -9nc /tmp/staging.envelope | curl -sS -X POST "https://api-sentinel.staging.tuju.ai/api/$PID/envelope/" \
  -H "X-Sentry-Auth: Sentry sentry_version=7, sentry_client=sentry.dart.flutter/9.6.0, sentry_key=$KEY" \
  -H 'Content-Type: application/x-sentry-envelope' -H 'Content-Encoding: gzip' --data-binary @- -w '\n%{http_code}\n'
# expect: {"id":"<EID>"} and 200
```

The replacements keep every item's byte length, so the `length` headers stay right. Then, on the staging database:

```sql
SELECT platform, level, error_class, release_version, source, sdk, event_user, tags, contexts, request,
       jsonb_array_length(breadcrumbs) AS crumbs, exception, trace_id
  FROM error_occurrences WHERE project_id = '<PUUID>' AND event_id = '<EID>';
-- expect one row: platform dart, level fatal, error_class TypeError, source sentry-compat, crumbs 70, and sdk,
-- event_user, tags, contexts, exception non-null (request is null for this event)
```


### 10.4 Files touched (the whole change's surface)

| File | Change | Side |
|---|---|---|
| `apps/ingestor-go/sentrycompat/routes.go` (new) | patterns, `Mount` (§3.1) | be |
| `apps/ingestor-go/sentrycompat/chain.go` (new) | `Chain` (§3.1) | be |
| `apps/ingestor-go/sentrycompat/decode.go` (new) | limits, `DecodeBody`, `capReader`, error classes (§4.1) | be |
| `apps/ingestor-go/sentrycompat/envelope.go` (new) | `ReadEnvelope`, `readHeaderLine`, `readPayloadLine` (§4.3) | be |
| `apps/ingestor-go/sentrycompat/handler.go` (new) | `Ingester`, `Config`, `NewEnvelopeHandler`, `NewStoreHandler`, `statusFor` (§4.2, §4.5) | be |
| `apps/ingestor-go/sentrycompat/mapper.go` (new) | `MapEvent`, `parseSentryTime`, the helpers (§5) | be |
| `apps/ingestor-go/sentrycompat/metrics.go` (new) | metric constants, `ItemTypes`, `ItemTypeLabel`, `NewItemRecorder` (§4.6) | be |
| `apps/ingestor-go/auth/sentry.go` (new) | `SentryKey`, `ParseSentryAuth`, `SentryProjectID`, `ScopesSentryCompat`, `SentryMiddleware` (§3.2-§3.4) | be |
| `apps/ingestor-go/auth/apikey.go` | `resolveKey` factored out of `Middleware` (behaviour unchanged); `NewAPIKeyAuthenticatorWithLookup` (§3.3) | be |
| `apps/ingestor-go/middleware/ratelimit.go` | `limit` core, `SentryMiddleware`, `RetryAfterSeconds`, `SentryRateLimitsValue` (§3.5) | be |
| `apps/ingestor-go/clientip/clientip.go` | `Resolvers`, `parseTrustedList` (`ParseTrusted`'s body, messages unchanged) (§3.6.1); `PeerTrusted`, `Info.PeerTrusted` (§3.7.2) | be |
| `apps/ingestor-go/sentrycompat/relay.go` (new) | header constants, `RelayIdentity`, `RelayIdentityFromRequest` (with the untrusted-peer log), `Apply` (§3.7) | be |
| `apps/ingestor-go/sentrycompat/shed.go` (new) | `Shed`, `ShedSlack` (§4.7) | be |
| `apps/ingestor-go/service/service.go` | `ingestAt` factored out of `IngestAt` (behaviour unchanged); `IngestAtWithin`, `Shedder`, `MaxPublishBytes`, `MaxShedRounds`, `ErrEventTooLarge`, the `ingest.event_shed` log (§4.7) | be |
| `apps/ingestor-go/validation/bounds.go` | `MarkServerTruncated`, `IsMetadataExemptKey` (§4.7); `applyBounds` unchanged | be |
| `apps/ingestor-go/main.go` | `Resolvers` call, `compat` config, `NewItemRecorder`, the two chains and `Mount` (§3.1, §3.6.1) | be |
| `deploy/helm/sentinel/values.yaml`, `templates/ingestor.yaml` | `config.ingestor.sentryCompatTrustedProxyCidrs: []` → `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS` (§3.6.3; §1.4 AM4) | be |
| `deploy/helm/sentinel/values.aws.staging.yml`, `values.aws.production.yml` | an explicit `sentryCompatTrustedProxyCidrs: []` with its comment (§3.6.3, D15) | be |
| `tests/unit/sentry_compat_{auth,envelope,mapper,budget,grouping,relay,fixtures,docs}_test.go`, `helm_sentry_compat_trusted_proxy_test.go` (new) | §11 P2-P5, P7, P6.3, docs and names | be |
| `tests/e2e/sentry_compat_test.go` (new) | U40 (P4.7) | be |
| `tests/fixtures/sentry/**` (new) | §10.1-§10.2 | be |
| `docs/sdk-specification.md`, `deploy/helm/sentinel/RUNBOOK.md`, `README.md` | §9 | be |
| `apps/dashboard-web/src/lib/sentry-project-id.ts` (+ `.test.ts`) (new) | §8.2, P6.1 | fe |
| `apps/dashboard-web/src/routes/[orgSlug]/projects/[projectId]/settings/+page.svelte` (+ `page.sentry-project-id.test.ts`) | §8.1, P6.2 | fe |

Not touched: `apps/processor-go/**` (§6.3), every migration, `/ingest` handlers, `ApiKeyCreateModal.svelte` and the
keys pages (§1.4 AM2), `packages/shared-go/**` (the NATS publisher and `obs` are read, not changed), the Huawei
overlays and `values-prod-example.yaml` (they inherit the empty value, §3.6.3). No overlay carries a non-empty
`sentryCompatTrustedProxyCidrs` (D15).

---

## 11. Verification

### 11.1 The gate

All from the repository root of the implementing worktree. Every command must pass with **no test skipped**: none of
the tests below calls `t.Skip`, reads an opt-in environment variable, or depends on Postgres, NATS, Redis (miniredis is
in-process) or the network.

```bash
GOWORK=off go build ./... && GOWORK=off go vet ./...
GOWORK=off go test ./tests/unit/... -count=1            # every load-bearing Go proof (P2.x-P5.x, P6.3, P7.x) lives here
go test ./apps/ingestor-go/... -count=1                  # existing package tests, incl. auth/apikey_test.go (T16: not in CI)
cd apps/dashboard-web && pnpm build && pnpm check && pnpm test && pnpm test --sequence.shuffle   # P6.x (B12, B13)
task test-unit                                           # the plane's unit gate: runs the Go and vitest suites above
SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/ -run 'TestU40' -count=1   # supplementary, needs the compose stack
```

**Where each is gated** (T16): the Go proofs are in `tests/unit`, which CI's `go-root` job runs on every push to `main`
and `task test-unit` runs everywhere; the vitest proofs run in CI's `dashboard` job on push to `main` and in
`task test-unit`. A pull request runs only builds. **Pre-merge proof:** the implementing seat dispatches `ci.yml` on its
branch (`gh workflow run ci.yml --ref <branch>`), which runs `go-root`'s unit step, `dashboard`'s `pnpm test`, and the
dispatch-only `integration` and `e2e` jobs (U40 included), and records the run URL.

### 11.2 Criterion → proof

**Card criteria → proofs** (contract §5 as amended by D13-D15):

| Card | Criterion | Proofs |
|---|---|---|
| TASK-02 | DSN-built `ingest` project key authenticates on both routes, header and query | P2.1-P2.4 |
| TASK-02 | mismatched path project → 403; org/admin/agent/report keys → 403 | P2.5, P2.6 |
| TASK-02 | limited request carries `Retry-After` and `X-Sentry-Rate-Limits` | P2.7-P2.9 |
| TASK-03 | Flutter 9.6 and Next.js 10 fixtures parse, gzip and deflate included | P3.1, P3.2 |
| TASK-03 | unknown item types skipped without error | P3.3, P3.5, P3.8 |
| TASK-03 | bomb and size caps answer 413 | P3.4, P3.5, P3.7 |
| TASK-04 | real Flutter event round-trips with every field in its v2 column (golden) | P4.1, P4.2 (P4.3-P4.5 the rows) |
| TASK-04 | duplicate delivery of one `event_id` is one occurrence | P4.6, P4.7 |
| TASK-04 (D13) | a mapped event over the publish budget is shed in the fixed order, else 413 `too_large`, never 500; `/ingest` unchanged | P4.8, P4.9, P4.10 |
| TASK-05 | one issue across two devices, two issues for two errors | P5.1, P5.2 (P5.3, P5.4) |
| TASK-06 | project settings shows the derived id; TS and Go agree on the golden vector | P6.1-P6.3, P2.6 |
| TASK-07 | a trusted-CIDR request's `X-Forwarded-For` becomes the client IP, an untrusted one's does not | P2.11 (both cases), P2.12 |
| TASK-07 | relay identity headers override from a trusted address, are ignored from any other; a body `relay_auth` never survives and `anonymous` strips the body's user/tenant (D14) | P7.1-P7.11 |
| TASK-07 (D15) | the chart exposes `sentryCompatTrustedProxyCidrs`, **empty everywhere**, and the RUNBOOK says how to derive it | P2.13 (`values.yaml` and every overlay), `TestSentryCompatDocumented` (the RUNBOOK entry) |
| Staging | one sentry_flutter-shaped envelope lands with typed panels | §10.3 (orchestrator, from inside the VPN) |

"Mutation" is the production change that must turn the proof red; the implementer runs each once and records it.
Test files are new files in `tests/unit` (one flat package: every new helper is prefixed `sc`, and existing helpers —
`newTestIngestService`, `fakeEventPublisher`, `newMiniRedisClient`, `readHelmValues`, `helmPath` — are reused, never
redeclared, B4).

**TASK-02** — `tests/unit/sentry_compat_auth_test.go`

| # | Proof | Mutation |
|---|---|---|
| P2.1 | `TestSentryCompatRoutes`: a fresh `ServeMux` with the four existing registrations and `sentrycompat.Mount(stub, stub)`: `POST /api/27690631804612/envelope/` and `…/envelope` reach the envelope stub with `PathValue("project_id") == "27690631804612"`; same for `store`; `GET`, `HEAD`, `PUT`, `OPTIONS` on each → `405` with `Allow` containing `POST` and the stub not called; `POST /api/1/envelope/x`, `/api/1/envelopes/`, `/api/envelope/` → `404`; `POST /ingest` still reaches its handler. | drop `{$}` (the `/x` case reaches the stub); drop the no-slash pattern (`307` instead of the stub); register without `POST` (`GET` reaches the stub) |
| P2.2 | `TestSentryCompatKeyPrecedence`: a table over `SentryKey`: header only, query only, `X-API-Key` only, header + query + `X-API-Key` (header wins), query + `X-API-Key` (query wins), a malformed header with a valid query (**401**, no fall-through), `?sentry_key=` empty with a valid `X-API-Key` (**401**), none (401). | reorder K1/K2; fall through on a malformed header |
| P2.3 | `TestParseSentryAuth`: every example of §3.2, accepted and refused. | require an exact `Sentry ` prefix; stop trimming quotes; take the last `sentry_key` |
| P2.4 | `TestSentryCompatAuthenticatesDSNKey`: `auth.NewAPIKeyAuthenticatorWithLookup` over a fake table (key hash → `APIKeyData`); a project `ingest` key via `X-Sentry-Auth` and via `?sentry_key=` on both routes (through `Mount`, so `PathValue` is real) → the next handler runs and sees `auth.ProjectIDFromContext` = the key's project and `IsOrgWideKey` false. | skip `WithIdentity`; read `X-API-Key` only |
| P2.5 | `TestSentryCompatRejectsWrongKeys`: same harness; path id of another project → `403`; path `0027690631804612` → `403`; `admin`, `read`, `agent`, `report` project keys → `403`; an org-wide `ingest` key (`ProjectID` nil) with and without `X-Project-Key` → `403`; unknown key → `401`; the next handler never runs. | remove A5's compare; use `ScopesIngest` on compat; drop A4 |
| P2.6 | `TestSentryProjectIDGolden`: reads `tests/fixtures/sentry/project-id-vectors.json`; every `valid` row equals `auth.SentryProjectID`, every `invalid` row errors; the maximum is below 2^53. | take 14 digits; skip lower-casing; accept a non-hex |
| P2.7 | `TestSentryCompatRateLimitHeaders`: `middleware.NewRateLimiter(miniredis client)`, identity with `RateLimitRPM` 1; through `SentryMiddleware` the second request is `429` with `Retry-After: 60` and `X-Sentry-Rate-Limits: 60::organization`. | drop the header; send it on `Middleware` instead |
| P2.8 | `TestSentryCompatRateLimitStrictMode`: nil Redis client → `500` through `SentryMiddleware` with no Sentry header; with `t.Setenv("RATELIMIT_STRICT_MODE", "false")` → passes. | fail open on nil client |
| P2.9 | `TestIngestRateLimitHasNoSentryHeader`: the same limited identity through `Middleware` → `429`, `Retry-After: 60`, **no** `X-Sentry-Rate-Limits`. | set the header in the shared core |
| P2.10 | `TestIngestAuthUnchanged`: through `Middleware(auth.ScopesIngest, …)` with the lookup seam and project-scoped keys only (§12.3): an `ingest` and an `admin` key presented as `X-API-Key` pass; a `read` key → `403`; the same `ingest` key presented only as `X-Sentry-Auth` or only as `?sentry_key=` → `401 "Missing API key"`. | read `X-Sentry-Auth` or `sentry_key` in `Middleware`; narrow `ScopesIngest` |
| P2.11 | `TestSentryCompatResolverBelievesRelay`: `_, compat, _ := clientip.Resolvers("", "10.20.0.0/16")`; a request with `RemoteAddr` `10.20.3.4:5555`, `X-Forwarded-For: 203.0.113.9` and `User-Agent: Dart/3.11 (dart:io)` through `clientip.Middleware` → `Info{ClientIP 203.0.113.9, HasClientIP true, UserAgent "Dart/3.11 (dart:io)"}`; with `X-Forwarded-For: 203.0.113.9, 10.20.9.9` (an internal hop appended) → the same; with a private XFF → no caller IP; **an untrusted peer** — `RemoteAddr` `198.51.100.20:5555` with `X-Forwarded-For: 203.0.113.9` — through the same compat resolver → `ClientIP 198.51.100.20` (the socket address; the header is not read; contract TASK-07 "an untrusted one's does not"); an invalid `compatSpec` (`"10.20.0.0/33"`, `"::ffff:10.0.0.0/104"`) → an error whose text starts `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS:` (not `TRUSTED_PROXY_CIDRS:`), and an invalid `globalSpec` → one starting `TRUSTED_PROXY_CIDRS:`. Then the Flutter envelope with a valid `X-Sentry-Auth` through **`sentrycompat.Chain(compat, lookup-seam authenticator, miniredis limiter, NewEnvelopeHandler(newTestIngestService…))`** → the published proto's `user.ip_address` is `203.0.113.9`, and a request without the key → `401` (so `Chain` includes `SentryMiddleware`). | `Chain` omits `clientip.Middleware` or uses `Middleware` instead of `SentryMiddleware`; read `X-Forwarded-For` from an untrusted peer; parse `compatSpec` with `ParseTrusted` (wrong variable named) |
| P2.12 | `TestIngestResolverIgnoresCompatList`: `ingest, compat, _ := clientip.Resolvers("", "10.20.0.0/16")`: `ingest` on the same relay-shaped request → no caller IP (the peer is private), and `ingest.Trusted()` is empty; `clientip.Resolvers("192.0.2.0/24", "")` → both resolvers trust exactly `192.0.2.0/24`, and a request from `192.0.2.7` with `X-Forwarded-For: 203.0.113.5` resolves to `203.0.113.5` through both. | put the compat prefixes into `ingest`; drop the union (compat loses the global prefixes) |
| P2.13 | `tests/unit/helm_sentry_compat_trusted_proxy_test.go: TestHelmSentryCompatTrustedProxyCidrs` (D15): `values.yaml` declares `config.ingestor.sentryCompatTrustedProxyCidrs` as an empty list; **every overlay** — every `values*.yml` / `values*.yaml` file in `deploy/helm/sentinel/` other than `values.yaml`, found by glob so a new overlay is covered, and the test fails if the glob finds fewer than the five of T25 — either omits the key or sets it to an empty list, and `values.aws.staging.yml` and `values.aws.production.yml` set it explicitly to `[]`; `templates/ingestor.yaml` contains the `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS` entry joined from that value; `trustedProxyCidrs` is still `[]` in `values.yaml` and both Huawei overlays. | rename one side; seed a range in `values.yaml`; seed a range in any one overlay |

**TASK-03** — `tests/unit/sentry_compat_envelope_test.go`

| # | Proof | Mutation |
|---|---|---|
| P3.1 | `TestSentryEnvelopeFixturesParse`: `ReadEnvelope` on `flutter-9.6.0.envelope` → header `event_id`, sdk `sentry.dart.flutter`/`9.6.0`, items `event` (payload byte-equal to the compact typeerror JSON), `transaction`, `client_report`; on `nextjs-10.envelope` → items `event` (newline-framed) and `session`. | require a trailing newline; mis-read `length` by one |
| P3.2 | `TestSentryEnvelopeVariantsMatch`: `DecodeBody` with `gzip`, `x-gzip`, `deflate` (the `.zlib` file) and `deflate` (the `.deflate` file) yields bytes identical to the identity envelope; each also parses as P3.1. | drop the zlib sniff (raw file fails, or zlib file fails) |
| P3.3 | `TestSentryEnvelopeFraming`: a table of hand-written envelopes: header only → 0 items; blank lines between items; `\r\n` line ends; mixed `length`/newline items; `length: 0`; an unknown `type` and `"Event"` → read past; a `length` item followed by `x` → malformed; truncated `length` → malformed; `length: -1`, `1.5`, `"3"` → malformed; item header without `type`, with `type: 7`, not JSON → malformed; header line not an object → malformed; empty body → malformed; a 65 KiB header line → malformed. | tolerate a missing `type`; skip the post-payload newline check |
| P3.4 | `TestSentryDecodeBombStopsAtCap`: 64 MiB of zeros gzip-compressed in the test (≈ 64 KiB); `io.Copy(io.Discard, DecodeBody(…, MaxDecompressedBodyBytes))` returns `ErrBodyTooLarge` after **exactly** `MaxDecompressedBodyBytes` bytes; the same for `deflate` (zlib and raw). | remove `capReader`; use `max` without `+1` accounting |
| P3.5 | `TestSentryEnvelopeHandlerStatuses`: `NewEnvelopeHandler` with a fake `Ingester` and `Scope`: `Content-Encoding: br` → `415`; a 5 MiB + 1 byte identity body (via `http.MaxBytesReader` as the handler applies it) → `413`; a 21 MiB decoded gzip body → `413`; 101 items → `413` and **no** `IngestAt` call; 100 items → `200`; corrupt gzip → `400`; malformed item header → `400` and no `IngestAt` call; an event item of 1 MiB + 1 byte (length-framed and newline-framed) beside a normal event → `200 {"id":…}`, one `IngestAt` call, the big one counted `too_large`; an envelope of only a transaction and a client report → `200 {}` and no `IngestAt`. | ingest before reading the whole envelope; answer `413` for an oversized event item |
| P3.6 | `TestSentryEnvelopeHandlerResponses`: fake `Ingester` returning nil (and stamping `EventID`) → `200 {"id":"<stamped>"}`; returning `validation failed: x` → `400` whose body contains it; returning `nats: timeout` → `500`; event payload `[1,2]` → `400`; the envelope header's `event_id` reaches `IngestAt` when the payload has none. | map every error to 400; ignore the header `event_id` |
| P3.7 | `TestSentryStoreHandler`: a JSON event → `200 {"id"}` via the fake; gzip body; 1 MiB + 1 decoded → `413`, counted `too_large`. | reuse the 20 MiB cap on store |
| P3.8 | `TestSentryCompatItemMetric`: `NewItemRecorder` over an `sdkmetric.NewManualReader` provider; one envelope with an event, a transaction, a `security` item and an oversized event → collected series `sentinel_compat_items_total` `{type=event,outcome=accepted}` 1, `{type=transaction,outcome=discarded}` 1, `{type=unknown,outcome=discarded}` 1, `{type=event,outcome=too_large}` 1; no other attribute keys. A second envelope of two events whose first fails validation (fake `Ingester`) → `rejected` 1, `skipped` 1. | label the raw type; drop the counter name constant; leave items after a stop uncounted |

**TASK-04** — `tests/unit/sentry_compat_mapper_test.go`

| # | Proof | Mutation |
|---|---|---|
| P4.1 | `TestSentryCompatFlutterGolden`: `MapEvent(flutter-typeerror-unhandled.json)`, `json.MarshalIndent`, byte-equal to `flutter-typeerror-unhandled.payload.golden.json`. With `-args -update` the test rewrites the file **and fails** ("golden rewritten; rerun without -update"), so an update can never pass silently. | any mapping row change |
| P4.2 | `TestSentryCompatFlutterFieldsLand`: `MapEvent` on the fixture, the test's scope stub sets `project_key`/`project_id`, then the **real** `IngestAt` of `newTestIngestService(t)` (the existing `fakeEventPublisher` helper, T17) with `receivedAt` `2026-09-25T10:00:00Z` (inside the fixture's clamp window, T8); the one captured message is `proto.Unmarshal`ed into `sentinelv1.ErrorEvent` and each field the processor writes to a column (T15) is asserted by value, with **no** `server_truncated` key in `metadata`: `platform` `dart`; `level` `fatal`; `error_class` `TypeError`; `message` `Null check operator used on a null value`; `environment` `production`; `release_version` `zasri.zasri@1.5.28+216`; `dist` `216`; `source` `sentry-compat`; `timestamp` `2026-09-25T09:41:52.838Z`; `trace_id` `0000000000000000000000000000c0de`; `span_id` `00000000000000a1`; `sdk` `{sentry.dart.flutter, 9.6.0}`; `user` `{id user-00000001, ip_address "", geo {Example City, Example Region, ZZ}}` (the mapper passes `{{auto}}`, P4.1's golden shows it; enrichment then blanks it because the unit context carries no caller IP, T12 I2 — in production it becomes the phone's public IP); `tags` 22 entries incl. `mechanism` → `PlatformDispatcher.onError`; `contexts` 11 names incl. `device.family` `Infinix`; `breadcrumbs` 70, first `{2026-09-25T07:20:41.195Z, navigation, app.lifecycle, info, data.state foreground}`; `exception.values` 1 `{TypeError, …, mechanism {PlatformDispatcher.onError, handled false}}`; top-level `stacktrace` `[State.setState (package:flutter/src/widgets/framework.dart, line 1219, in_app false), _VisitationCameraState._disposeCamera (package:daya/presentation/screens/visitation/visitation_camera.dart, line 91, in_app true)]`; `fingerprint` `""`; `metadata` without `server_truncated`. Asserting on the published proto keeps the processor's masking (existing, every SDK) out of this proof; the stored columns are U40's (P4.7). The same for the Dio event (no `mechanism`), including `request {POST, https://api.example.invalid/api/v1/auths/refresh-token}`, `transaction` `POST /auths`, 12 contexts incl. `API Request Info`, and a 12-frame crash-first stacktrace starting `DioMixin.request`. | drop any row; skip the reversal; stringify `timestamp` wrong |
| P4.3 | `TestSentryCompatMappingRows`: one table case per row of §5.2-§5.5 (M1-M23, E1-E4, S1-S3, F1-F8, Q1-Q5, the timestamp rules): object vs pairs for `tags` and `headers`, `{values}` vs array for `breadcrumbs`/`exception`/`threads`, float vs RFC 3339 vs zone-less vs numeric-string vs garbage timestamps for the event and for a crumb, `log` level, `Production ` env, env `staging area` → `production`, platform `Flutter!` → `other`, `other` + `sentry.dart.flutter` → `dart`, numeric `user.id`, `logentry` vs `message` object vs string, exception without `type`, threads fallback (crashed, current, first), `in_app` true without file, `colno` −3, `lineno` 3e9, fingerprint vectors of M20. | per row |
| P4.4 | `TestSentryCompatFreeFormSurvivesStructpb`: contexts, `extra`, `request.data` and crumb `data` with nested objects, arrays, numbers, bools and nulls → after `MapPayloadToEvent` every one is non-nil in the proto and round-trips to the same values (the silent-drop path of T11 is never taken). A second case adds `extra.huge: 1e400` and a context `bad: {"n": 1e400}` beside good keys and contexts: only `huge` and `bad` are missing, every other key and context is present (D-b's unit of loss). | decode with `UseNumber` (`metadata` becomes nil); decode `extra` or `contexts` whole (every key or context lost); decode `extra` into `map[string]string` |
| P4.5 | `TestSentryCompatNeverRejected`: a table of hostile events (every field the wrong JSON type; 150 frames; 25 exception values; `error_class` source 400 chars; message 20,000 code points; release 150; env `PROD ENV`; platform `Flutter.Mobile`; `trace_id` 100 chars; `in_app` true on every frame with no file, top-level and chain; `colno` −1; U+0000 in every string incl. `extra` keys; invalid UTF-8 bytes; an empty object `{}`; plus a frame `abs_path` of 600 bytes of `é` and a `function` of 300 bytes of `日`, an exception `type` of `"\u0000"`, and a frame with `in_app: true` and `abs_path: "\u0000"`) → after the scope stub sets `project_key`/`project_id`, `newTestIngestService(t)`'s `IngestAt` returns nil for each and publishes one message; in every published proto no string (walked recursively through `proto.Message` reflection, `Struct` values included) contains U+0000 or is invalid UTF-8, and the long `file`/`function` are ≤ 512 / 255 bytes; `{}` maps to `error_class UnknownError`, `platform other`, `environment production`. | remove any §5.6 guard; strip U+0000 after E2/F7 instead of at decode; cut `truncBytes` on a byte, not a rune boundary |
| P4.6 | `TestSentryCompatDuplicateDeliveryPublishesSameIdentity`: the Flutter envelope with its **header** `event_id` changed in the test to `55555555555545558555555555555555` (same length, so `length` holds), through `NewEnvelopeHandler` twice, with the real `IngestService` over `fakeEventPublisher` and a `Scope` that stamps a fixed project: both published protos carry the payload's `event_id` (`11111111111141118111111111111111`) and the same `project_id` — the processor's unique key (T14). A second case removes the payload's `event_id`: both publishes carry the header's. | mint an id in the mapper; prefer the envelope header's id over the payload's; ignore the header's |
| P4.7 | **Duplicate delivery is one occurrence** — `tests/e2e/sentry_compat_test.go: TestU40_SentryCompatEnvelopeDedupsAndGroups` (**supplementary; CI job `e2e`, `workflow_dispatch` only**, T16): with the e2e fixture's project `ingest` key, POST the Flutter envelope (fresh `event_id`, current timestamp) gzip-compressed to `/api/<derived>/envelope/` with `X-Sentry-Auth`, twice; wait for `sentinel_process_events_total{outcome="duplicate"}` to rise (U36's pattern); assert `occurrenceCount() == 1`, `onlyIssue().Count == 1`, and the occurrence's `platform`, `level`, `source`, `sdk`, `event_user`, `tags`, `contexts`, `breadcrumbs`, `exception` columns are set. The underlying unique insert is proven by `tests/integration/event_idempotency_test.go: TestEventIdempotency_SameEventTwiceSequentially` and `TestStoreEvent_DedupProject_CrossIssueRedeliveryStoresNothing` (CI job `integration`, dispatch-only). | — (end-to-end; the unit proof is P4.6) |

**TASK-04, publish budget (D13)** — `tests/unit/sentry_compat_budget_test.go`. Each case measures its own construction
first and asserts it is **over** its budget mapped (1,000,000 bytes unless the case sets `Config.PublishBudget`; a
guard, so the case cannot pass by not exercising the budget). P4.8 and P4.9 run through `NewEnvelopeHandler` with the
real `newTestIngestService(t)` (the `fakeEventPublisher` enforces no limit, so an unshed message would be captured
whole) and a `Scope` stub.

| # | Proof | Mutation |
|---|---|---|
| P4.8 | `TestSentryCompatPublishBudgetReviewerCases` — the reviewer's two cases (T26), rebuilt from the typeerror fixture. **A, numeric arrays (~233 KB JSON):** its breadcrumbs replaced by 55 crumbs each with `data {"n": [0, …]}` of 1,000 zeros; a context `samples {"n": [15,000 zeros]}`; `extra {"samples": [28,000 zeros]}` (every one under its JSON bound, T10) → `200`, one published message of **at most 1,000,000 bytes**, the newest crumb present and the crumbs a suffix of the original order (oldest removed), `extra.samples` and `contexts.samples` present, `server_truncated` contains `breadcrumbs` and `server_truncated_keys` a `breadcrumbs:<n>` entry whose `n` equals the crumbs removed. **B, 70 frames (~850 KB JSON):** the exception's frames replaced by 70 frames, each `abs_path` `package:daya/` + 990 × `a` + `.dart`, `function` `f<i>`, `lineno` `i+1`, `in_app` true, `context_line` 1,000 × `c`, `pre_context` / `post_context` 5 lines of 1,000 bytes → `200`, at most 1,000,000 bytes, all 70 top-level frames present crash-first with `file`, `function`, `line`, `in_app` unchanged, every frame's context lines empty, no breadcrumb and no non-well-known context left, `device` and `os` present, `server_truncated` contains `stacktrace.context`. For both: never `500`, `sentinel_compat_items_total{type=event,outcome=accepted}` 1. | remove the budget loop (a > 1,000,000-byte message is captured); shed B5 before B1 (B keeps its crumbs: red); shed B2 or B3 before B1 (A loses `extra.samples` or `contexts.samples`: red); drop the markers |
| P4.9 | `TestSentryCompatShedOrderAnd413`: one event whose every sheddable element is far larger than `ShedSlack` — 10 crumbs with 8,000-character messages, `extra {big: 40,000 chars, small: 15,000 chars, user_id: "u1"}`, contexts `device` and `custom` (30,000 chars), 3 exception values with 10,000-character values, 20 frames with 5+5 context lines of 1,000 bytes. The test maps it once and, from the same estimates `Shed` uses, sets `Config.PublishBudget` per case so that `over + ShedSlack` lies inside step k's total and beyond steps 1..k-1's → k = 1: only crumbs removed, oldest first, the rest intact; k = 2: every crumb, `big` before `small`, `user_id` kept; k = 3: `custom` gone, `device` kept; k = 4: only the last exception value left, its `type`, `value` and `mechanism` unchanged; k = 5: every context line empty, every top-level frame's `file`/`function`/`line` unchanged. In the k = 5 case `server_truncated_keys` is `breadcrumbs:10`, `big`, `small`, `contexts.custom`, `exception.values:2`, `stacktrace.context`, in that order. A budget below what B5 reaches → `413 "Event too large"`, `too_large` 1, **nothing published**, never `500`; the same through `NewStoreHandler` → `413`. A fake `Ingester` returning `fmt.Errorf("failed to publish message: %w", nats.ErrMaxPayload)` → `413`, `too_large`; one returning `service.ErrEventTooLarge` wrapped → `413`. | reorder any two steps; shed a well-known context, an exempt metadata key or the titled exception; map `ErrEventTooLarge` or `nats.ErrMaxPayload` to `500`; skip `MaxShedRounds` (a shedder that removes nothing must not spin: a stub `Shedder` returning `true` without removing ends in `413` after 4 rounds) |
| P4.10 | `TestIngestHasNoPublishBudget`: case A of P4.8, mapped by `MapEvent` and the scope stub, through `IngestAt` (the `/ingest` path) → one message **over** 1,000,000 bytes captured, byte-identical to `proto.Marshal(MapPayloadToEvent(…))` of the bounded payload, and no shed marker in `metadata`. So `/ingest` measures nothing and sheds nothing (contract §2, D13). | give `IngestAt` a budget; shed on `/ingest` |

**TASK-05** — `tests/unit/sentry_compat_grouping_test.go`. Each case maps events with `MapEvent`, runs
`ValidatePayload`, `MapPayloadToEvent`, `proto.Marshal`, `event.Deserialize` and `ComputeFingerprint(symbolicate.DefaultPlatforms)`,
and compares the fingerprints (issue identity within one project and environment, T13).

| # | Proof | Mutation |
|---|---|---|
| P5.1 | `TestSentryDartSameErrorTwoDevicesOneIssue`: the typeerror fixture and a copy with a different `event_id`, `timestamp`, `contexts.device` (model, id, memory), `user.id`, 12 fewer breadcrumbs, and every `lineno` + 7 → equal fingerprints. | add `line` to the hash input; add a device or user value to it |
| P5.2 | `TestSentryDartSameEntryTwoErrorsTwoIssues`: two `TypeError` events with the same message and the same outer frames `package:daya/main.dart:main`, `package:daya/app.dart:App.run`, `package:daya/router.dart:Router.go` (in-app, outermost first) and the same `package:flutter/src/widgets/framework.dart:State.setState` (not in-app) at the crash end, differing only in the in-app frame just before it (`…/visitation_camera.dart:_VisitationCameraState._disposeCamera` vs `…/profile_screen.dart:_ProfileState.load`) → different fingerprints. | **skip the reversal** (both hash `main`, `App.run`, `Router.go` → equal → red) |
| P5.3 | `TestSentryDartPackagesDoNotCollide`: two events identical except the crash frame's `abs_path` `package:alpha/utils.dart` vs `package:beta/utils.dart` (same `filename` `utils.dart`) → different fingerprints. | `file` = `filename` first |
| P5.4 | `TestSentryDartInAppDefaults`: frames `dart:async/zone.dart`, `package:flutter/src/widgets/framework.dart` and `package:daya/x.dart` with no `in_app` → all `false`; `package:daya/x.dart` with `in_app: true` → `true`; `in_app: true` and no `abs_path`/`filename` → `false`. | default a missing `in_app` to `true` |

**TASK-07** — `tests/unit/sentry_compat_relay_test.go`. Harness: `_, compat, _ := clientip.Resolvers("",
"10.20.0.0/16")`; requests go through `sentrycompat.Chain(compat, lookup-seam authenticator, miniredis limiter,
NewEnvelopeHandler(…newTestIngestService…))` with a valid `X-Sentry-Auth`, carrying the Flutter envelope (whose event
sends `user {id user-00000001, ip_address {{auto}}, geo …}` and 22 tags, none of them `tenant_id` or `relay_auth`);
assertions read the published proto. "Trusted peer" is `RemoteAddr 10.20.3.4:5555`; "untrusted peer" is
`198.51.100.20:5555`.

| # | Proof | Mutation |
|---|---|---|
| P7.1 | `TestRelayIdentityUserFromTrustedPeer`: trusted peer, `Auth: user`, `User-Id: core-user-42`, `Tenant-Id: company-7` → `user.id` `core-user-42`, `user.geo` and `user.ip_address` as they would be without the headers, `tags.tenant_id` `company-7`, `tags.relay_auth` `user`, the other 22 tags unchanged. | skip `Apply`; replace the whole `user` |
| P7.2 | `TestRelayIdentityAnonymousFromTrustedPeer` (D14): trusted peer, `Auth: anonymous` plus stray `User-Id`/`Tenant-Id` headers, on the Flutter event with `tags.tenant_id: app-tenant` and `extra {user_id: u1, userId: u2, user: u3, tenant_id: t1, tenantId: t2, organization_id: t3, other: x}` added → `tags.relay_auth` `anonymous`, **no** `tenant_id` tag, `user.id` `""` while `user.geo` and `user.ip_address` are as without the headers, none of the six metadata fallback keys present and `other` present; and after `event.Deserialize` + `Normalize` in the processor's `event` package, `AffectedUserID` and `AffectedTenantID` are empty. | read the id headers for `anonymous`; keep the body's `user.id` or `tenant_id`; leave the metadata fallback keys (the processor attributes through `extra`) |
| P7.3 | `TestRelayIdentityIgnoredFromUntrustedPeer`: untrusted peer, all three headers (`user`) → published proto identical to the same request without the headers (no `relay_auth`, no `tenant_id`, `user.id` the event's). Also a trusted **address inside `X-Forwarded-For`** from an untrusted peer (`X-Forwarded-For: 10.20.3.4`) → ignored. | test `X-Forwarded-For` instead of the peer; drop the `PeerTrusted` check |
| P7.4 | `TestRelayIdentityInvalidAuthValue`: trusted peer, `Auth: admin` / `USER ` / empty → `USER ` is treated as `user` (trim, case-insensitive); `admin` and empty apply nothing and the request still answers `200`. | reject the envelope; accept any value as `user` |
| P7.5 | `TestRelayIdentityPartialUserHeaders`: trusted peer, `Auth: user` with only `User-Id` → `user.id` set, `relay_auth` `user`, no `tenant_id`; with only `Tenant-Id` → `tenant_id` set, `user.id` the event's; an event that itself sent `tags.tenant_id: app-value` with `Tenant-Id: company-7` → `company-7`. | require both headers; keep the app's `tenant_id` |
| P7.6 | `TestRelayIdentityTagsSurviveBounds`: the event carries 60 tags `a00`…`a59`; trusted peer, `user` with both ids → after `IngestAt` (bounds included) the published tags contain `relay_auth` and `tenant_id` and 48 of the `a*` keys (`a00`…`a47`). | omit step 4 (bounds keep `a00`…`a49`, both relay tags cut) |
| P7.7 | `TestRelayIdentityOnStore`: the same as P7.1 through `NewStoreHandler`. | apply only in the envelope handler |
| P7.8 | `TestRelayHeadersOnlyInCompat`: `clientip.Resolvers("10.20.0.0/16", "")`'s **ingest** resolver builds `Info` with `PeerTrusted` true for a trusted peer (the field is additive), and a source scan asserts the three header names appear in no Go file outside `apps/ingestor-go/sentrycompat/` and `tests/` — so `/ingest`, which uses neither `RelayIdentityFromRequest` nor `Apply`, cannot read them. | read the headers in `main.go: handleIngest` |
| P7.9 | `TestRelayAuthTagFromBodyAlwaysRemoved` (D14): `MapEvent` on events whose `tags` carry `relay_auth: user`, once as an object and once as pairs → no `relay_auth` key in the payload. Through the chain: untrusted peer with no relay headers → published tags have no `relay_auth`; trusted peer with `Auth` absent, and with `Auth: admin` → no `relay_auth`; trusted peer with `Auth: anonymous` → `relay_auth` `anonymous` (the header's, not the body's `user`). | delete `relay_auth` only when `Apply` writes one; delete it from the object form only |
| P7.10 | `TestRelayUntrustedPeerKeepsBodyIdentity` (D14, D1): untrusted peer carrying all three headers (`user`, `core-user-42`, `company-7`) on the event with `tags {tenant_id: app-tenant, relay_auth: user}` and `extra.user_id` → published `user.id` `user-00000001` and `tags.tenant_id` `app-tenant` (the body's, kept), `extra.user_id` kept, **no** `relay_auth`. | apply `anonymous` stripping to untrusted peers; honour the headers from an untrusted peer |
| P7.11 | `TestRelayUntrustedPeerLogged`: with a capturing `slog` handler as `Config.Logger`, an untrusted peer `198.51.100.20:5555` sending `X-Sentinel-Relay-Auth: user`, `User-Id`, `Tenant-Id` and `X-Forwarded-For: 203.0.113.9` → exactly one Info record with `event=sentrycompat.relay_peer_untrusted` and `peer=198.51.100.20`, and no attribute value anywhere in the record equals a header value (`user`, the ids, `203.0.113.9`) or the key; the same request without `X-Sentinel-Relay-Auth` → no such record; a trusted peer → no such record. | log header values; log for every compat request; drop the log |

**TASK-06** — vitest, `apps/dashboard-web`, plus the shared vector's Go half (P2.6)

| # | Proof | Mutation |
|---|---|---|
| P6.1 | `src/lib/sentry-project-id.test.ts: sentryProjectId matches the Go golden vector` reads `../../../../tests/fixtures/sentry/project-id-vectors.json` (the file P2.6 reads): every `valid` row equal, every `invalid` row throws. | take 14 digits; `parseInt` without radix 16; accept a non-hex |
| P6.2 | `src/routes/[orgSlug]/projects/[projectId]/settings/page.sentry-project-id.test.ts`: render the settings page (the `page.environments.test.ts` harness, `fetch` stubbed) with `projectId` `0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6` → `data-testid="sentry-project-id"` shows `27690631804612` inside `#sentry-sdk`. | drop the section; render `data.projectId` raw |
| P6.3 | `TestSentryProjectIDVectorsShared` (Go, `tests/unit`): the vector file exists at the path both halves read and has at least the seven `valid` rows of §3.4, so neither half can pass against a private copy. | move the file under `apps/dashboard-web` |

**Docs** — `tests/unit/sentry_compat_docs_test.go: TestSentryCompatDocumented`: `docs/sdk-specification.md` has the
`## 10. Sentry-compatible ingest` heading and mentions each of `/api/{project_id}/envelope/`,
`/api/{project_id}/store/`, `X-Sentry-Auth`, `sentry_key`, `X-Sentry-Rate-Limits`, `60::organization`, the five limit
values, `sentinel_compat_items_total`, `1,000,000` and `413` (the publish budget, §4.7); `RUNBOOK.md` mentions
`sentinel_compat_items_total`, `ingest.event_shed`, `sentryCompatTrustedProxyCidrs`,
`sentrycompat.relay_peer_untrusted` and the word `narrowest`, and the relay entry contains no address literal (no match
for `\b\d{1,3}(\.\d{1,3}){3}\b` or an IPv6 prefix; D15: the entry names no value). Mutation: delete either section; write a CIDR into the relay entry.

**Pinned names** — `TestSentryCompatNamesPinned`: `sentrycompat.MetricItems == "sentinel_compat_items_total"`,
`LabelType == "type"`, the four patterns, `middleware.SentryRateLimitsValue == "60::organization"`,
`auth.ScopesSentryCompat` equals `{ingest}`, the three relay header names and two tag keys of §3.7.1,
`service.MaxPublishBytes == 1_000_000`, `service.MaxShedRounds == 4`, `sentrycompat.ShedSlack == 16 << 10`.

---

## 12. Decisions this spec made

### 12.1 Every rule the contract left to the spec

| Contract leaves to the spec | Fixed as | Derived from |
|---|---|---|
| Route registration, trailing slash, 405 | four method patterns, `{$}` on the slash form, the mux's own 405 (§3.1) | T1; Go 1.22 mux semantics |
| `X-Sentry-Auth` parser | §3.2: `Sentry` prefix case-insensitive, comma list, first `=`, trimmed, quotes stripped, first non-empty `sentry_key`; else 401 | T21 (the header sentry-dart sends); contract §4.1 |
| Precedence | first present source only: header, query, `X-API-Key`; no fall-through | contract §4.1 order; no mixed sources |
| D1 without changing `/ingest` | `ScopesSentryCompat = {ingest}` + `SentryMiddleware` (A1-A6); `Middleware` untouched | T3; contract §2 |
| Go derivation, golden vector, 403 | `auth.SentryProjectID`, `tests/fixtures/sentry/project-id-vectors.json`, exact string compare (§3.4) | contract D2 |
| `X-Sentry-Rate-Limits` value, `Retry-After` | `60::organization`, `60`, compat chain only (§3.5) | T5 (window and today's value); T21 (parser) |
| Envelope reader: length vs newline, caps, statuses, counting reader, zlib vs raw | §4.1-§4.3 | contract §4.2; T21 (framing); RFC 1950/1951 |
| Metric name and labels | `sentinel_compat_items_total{type,outcome}`, 18 type values, 6 outcomes (§4.6) | contract §4.2; V6 |
| Every mapping row as Go types | §5.2-§5.5 | T7, T9, T11, T22 |
| Fingerprint hashing (D5) | M20 | contract D5 |
| Dart `in_app` defaults (§4.4) | F7 (V3) | T7, T22 |
| Dashboard env var and helm value; where the DSN renders | **Superseded by §1.4 AM2**: no DSN and no host setting; the Sentry project id renders on project settings (§8) | amendment (operator decision 2026-09-27, contract §9 Q1) |
| (amendment, D10) trusting the relay's `X-Forwarded-For` | compat-only `SENTRY_COMPAT_TRUSTED_PROXY_CIDRS` / `config.ingestor.sentryCompatTrustedProxyCidrs`, unioned with `TRUSTED_PROXY_CIDRS` for compat routes only; its errors name the compat variable (§3.6.1) | T25; §1.4 AM3-AM4 |
| (D15) the per-environment values | none named: `[]` in `values.yaml`, explicit `[]` in both AWS overlays, inherited elsewhere; a RUNBOOK procedure from the observed immediate peer, with the narrowest-CIDR warning; `core-sentinel-mobile-relay` sets them (§3.6.3-§3.6.4) | contract §4.7, D15 |
| (amendment 2, D12; D14) relay identity headers | §3.7: honoured only when the immediate peer is in the compat resolver's prefixes; `user` overrides `user.id` and `tags.tenant_id`; `anonymous` strips the body's `user.id`, `tags.tenant_id` and the six metadata fallback keys; a body `relay_auth` is always dropped; room reserved in the 50-tag bound | §1.4 AM8-AM9; T10, T25; `processor event.go: readAffectedIDs`, `indexer.go: UserIDFromMetadata, TenantIDFromMetadata` |
| (D13) the shed order and the 413 | §4.7: budget 1,000,000 in the service, compat callers only; B1 crumbs oldest first, B2 non-exempt metadata keys largest first, B3 non-well-known contexts largest first, B4 non-titled exception values oldest first, B5 context lines; markers as the bounds step's; `ErrEventTooLarge` and `nats.ErrMaxPayload` → `413` `too_large` | T26; contract D13 |
| Proofs that cannot skip | §11: `tests/unit` + vitest; U40 supplementary | T16, T17 |

### 12.2 Decisions the design needed

1. **The whole envelope is read before anything is ingested** (§4.2 step 3), so every `4xx` precedes every publish and
   a `413` never follows a published event. Memory stays bounded by the decoded cap.
2. **Header lines are capped at 64 KiB** (`MaxHeaderLineBytes`). The contract caps payloads, not header lines; without
   a cap an item "header" could be a 20 MiB line buffered in memory. Real headers are under 1 KiB.
3. **Blank lines between items are tolerated**, and `\r\n` is accepted, because they cost nothing and some HTTP tools
   add them. A non-newline byte straight after a length-framed payload is still malformed, since it means the
   `length` is wrong.
4. **The first event failure stops the envelope** (§4.2 step 6). Sentry clients send one event per envelope; stopping
   keeps the response honest about what happened, and re-sending is idempotent (T14).
5. **The response `id` is the effective id** `IngestAt` stamped, not the one sent, as `/ingest` echoes (T8).
6. **Level is lower-cased and passed through** (M4), so an unknown level is blanked **and marked** by the bounds step
   rather than silently by the mapper.
7. **An empty exception `value` falls back to the message text** for `message` (E3), and an empty `type` to the value's
   first line for `error_class` (E2, V2), so an event is never titled by nothing while it has words to show.
8. **`file`/`function` are truncated to 512/255 bytes** (F1, F3): `docs/sdk-specification.md` §3 makes these
   SDK-side MUST rules, and the mapper is the SDK here. The cut is deterministic, so grouping is stable.
9. **U+0000 is removed from every emitted string** (D-c), `metadata` included, which the server itself does not strip
   (sdk-go spec §12 F11); a NUL there dead-letters the event at the `jsonb` insert.
10. **The metric constant lives in `sentrycompat`** (V6) to stay inside `be.md`'s scope; there is still exactly one
    place that spells it, and `TestSentryCompatNamesPinned` pins it.
11. **A compat-only trusted-proxy list, unioned with the global one** (§3.6, §1.4 AM3): it keeps `/ingest` byte-identical
    by construction and keeps the Huawei decision-11 test meaningful, at the cost of one more value to set per
    environment.
12. **The Sentry project id is derived in the browser** from `data.projectId` (§8.1), so the settings load function is
    not touched and the derivation has exactly one TS implementation, the one P6.1 pins.
13. **The publish budget lives in the service, the shed order in `sentrycompat`** (§4.7): the proto exists only inside
    `IngestAt`, and a pre-check outside it would marshal every event twice and still miss what enrichment adds. A
    `Shedder` callback keeps the Sentry-specific order out of the shared service, and `IngestAt` passing no budget
    keeps `/ingest` byte-identical.
14. **Shed just enough, by lower-bound estimates** (§4.7): each element's `proto.Size` never exceeds what removing it
    saves, so one round normally lands under the budget; `ShedSlack` (16 KiB) covers the markers the round writes, and
    `MaxShedRounds` bounds the re-marshal cost of a hostile event. The order follows the contract's example (D13)
    with breadcrumbs first because they are the largest and least specific part of a Flutter event (70 per fixture,
    T22), and context lines last because they are what a reader of one frame wants most after the frame itself.
15. **A 413 stops the envelope** like any other event failure (§12.2 item 4): contract D13 answers `413`, and a
    Sentry client sends one event per envelope.
16. **`anonymous` also removes the metadata fallback keys** (§3.7.4): D14 removes the body's `user.id` and
    `tags.tenant_id` so an anonymous event is attributed to no user or tenant; the processor would otherwise read the
    same identity from `extra` (`readAffectedIDs` → `UserIDFromMetadata` / `TenantIDFromMetadata`), so D14's purpose
    needs those six keys gone too. Nothing else in `extra` is touched.
17. **The untrusted-peer log** (§3.7.3) exists so §3.6.4's procedure can be followed from the ingestor's own logs:
    nothing logged a peer address before (T25), and D15 derives the value from the observed immediate peer. It logs
    only the peer's address, only when a relay header is present, at Info.

### 12.3 Notes for the spec reviewer

- **The auth seam** (`NewAPIKeyAuthenticatorWithLookup`) is exported production code that exists for tests, because
  the load-bearing proofs must live in `tests/unit` (T16) and that package cannot set `lookup`. It builds an
  authenticator with no DB; `Middleware`'s org-wide `X-Project-Key` branch would dereference a nil pool on such an
  authenticator, so P2.10 uses only project-scoped keys on `Middleware`. The doc comment says so.
- **B5 (cross-boundary payloads)**: the derivation is duplicated in Go and TS by the contract's design; the shared
  vector file is the check (P2.6, P6.1). The envelope framing is a third-party wire format; its fixtures are real
  (Flutter) or follow the published format with a real SDK version (Next.js).
- **`task test-unit` runs `go test ./apps/ingestor-go/...`, CI does not** (T16). No proof in §11 lives there; the
  existing `apps/ingestor-go/auth/apikey_test.go` keeps passing because `Middleware`'s behaviour is unchanged.
- **The compat routes stay private** (§1.4): the relay in core is their production caller. Nothing here makes them
  reachable from a phone. `mobile-sentinel-dual-write` depends on `core-sentinel-mobile-relay` as well as this run.
- **AM3 is ratified** as contract D10 (the compat-only list); nothing here is pending on it.
- **D13 touches the shared service** (`IngestAtWithin`, `ingestAt`) and `validation` (two exported helpers). `/ingest`
  stays byte-identical because `IngestAt` passes no budget; P4.10 is the proof, and the existing service and bounds
  tests keep passing unchanged.
- **§3.7.4 step 2's metadata removal goes one step past D14's letter** (decision 16), to meet its stated purpose;
  if the coordinator reads D14 as `user.id` and `tags.tenant_id` only, drop the six-key deletion and P7.2's
  `AffectedUserID`/`AffectedTenantID` assertion.
- **D15: no CIDR is named anywhere in this spec or the RUNBOOK entry**, by ruling; the topology guidance (§3.6.4) names
  the load-balancer hostname only as an example of a topology to avoid.

---

## 13. Findings outside this scope (recorded, not fixed)

| # | Finding | Why not here |
|---|---|---|
| F1 | `mapping.MapPayloadToEvent` ignores `structpb` errors and silently drops `metadata`, a context's content, `request.data` or crumb `data` (T11). Any producer that is not `encoding/json`-decoded (a future Go-side caller) loses data without a log line or metric. | Pre-existing; this spec avoids it by construction (D-b). A counter on that path is an ingestor change of its own. |
| F2 | The ingestor strips U+0000 from v2 fields but not from `metadata`; a NUL there dead-letters the event at the `jsonb` insert (sdk-go spec §12 F11). | Pre-existing; compat events are protected by D-c; `/ingest` is out of scope (contract §2). |
| F3 | `projects.api_key` / `projects.api_key_hash` still exist and the e2e harness writes a plaintext `api_key` into `projects`; nothing in the ingestor reads them. | Unrelated legacy column; not asked. |
| F5 | The compat routes never retry for the caller: an app envelope core's relay fails to accept is lost unless the relay persists it (its JetStream stream) and redelivers on a `500`/`429`/`401`. A `413` (§4.1's caps, or D13's publish budget, §4.7) and every `400`/`403` are final: redelivering them cannot succeed. A `401` is **retryable**: on a compat route it can be transient (the Redis key cache, or a key rotation in flight), and every redelivery is idempotent by `event_id` (T14). | Relay behaviour; recorded for `core-sentinel-mobile-relay`. |
