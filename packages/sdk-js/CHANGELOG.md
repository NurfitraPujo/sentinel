# Changelog

All notable changes to `packages/sdk-js` (`@sentinel/sdk-js`) are documented here — one entry per
tag (`packages/sdk-js/vX.Y.Z`). The package is major-zero: a minor bump may change shape, and every
wire-vocabulary change is named here because the dashboard reads `metadata` as-is.

## v0.3.1 — Fix: `traceparent` no longer leaves the caller's Request body unread

### Fixed

- **Adding `traceparent` to a same-origin `fetch(request)` no longer sends `request.clone()`** when the Request has a
  body. The clone left the caller's original unread, so `request.bodyUsed` stayed `false` (a second `fetch(request)`
  succeeded where it throws without the SDK) and a streaming (`duplex: 'half'`) upload was buffered by the tee. The
  outgoing request is now built from the original, which transfers and consumes its body exactly as a plain
  `fetch(request)` does; method, credentials, referrer and referrerPolicy are kept. Body-less Requests are unchanged.

## v0.3.0 — Browser trace propagation (`tracePropagationTargets`)

- **The browser SDK starts a trace and sends it.** One trace id per page load, renewed on each navigation to a
  new **pathname** (a query- or hash-only `pushState`/`replaceState` keeps the trace); every captured browser event without an explicit `traceparent` now carries it as
  `trace_id` / `span_id` / `trace_flags: 1`, so a browser error and the server error the same API call caused
  share one trace id in Sentinel. `fetch` and `XMLHttpRequest` requests to matching URLs get
  `traceparent: 00-<trace>-<fresh span>-01`; an existing `traceparent` is never overwritten, and
  `tracestate`, `baggage` and `sentry-trace` are never added.
- **New `init` option `tracePropagationTargets: Array<string | RegExp>`** (browser and `./nextjs/client`).
  **Default: the page's own origin only** — a behaviour change for same-origin requests, which now carry a
  `traceparent` header; cross-origin requests are untouched unless you list them. String targets match the request's origin exactly, then the path
  segment-aware (`'/api'` is not `/apiary`); a `ctx.traceparent` of `undefined`/`null`/`''` counts as absent.
  Details in `docs/sdk-specification.md` *Trace propagation* and the README. `tracePropagationTargets: []` turns it off.
- Server runtimes (node, edge, Next server) and the wire format are unchanged.

## v0.2.0 — Typed `level` / `sdk` / `user` / `tags` / `contexts` / `request` / `breadcrumbs`; identity and tags leave `metadata`

#### Behaviour change

- **`tags`, `user_id`, `tenant_id` and `level` leave `metadata`.** They now live in their own typed
  wire fields: `setTag` / `setTags` / `setTenant` write `tags` (a per-event copy of the scope's map,
  capped at 50 keys — see below); `setUser` and a per-call `ctx.user` write `user` (`id`, `username`,
  `email`, `ip_address`, replace semantics); `level` is a top-level field, `'fatal' | 'error' |
  'warning' | 'info' | 'debug'`, defaulting to `'error'` for `captureException` and `'info'` for
  `captureMessage` (which now also takes a level string as its second argument). A per-call
  `ctx.metadata.user_id` / `tenant_id` / `level` is dropped with a debug line — use `ctx.user` /
  `ctx.tags` / `ctx.level` instead. **0.1.x apps keep working**: the server accepts both the old
  metadata-carried shape and the new typed one from every SDK version at once.
- **New scope APIs and options.** `setContext(name, value | null)` writes `contexts`, merged under
  each runtime's automatic contexts (below), app-set fields winning field by field.
  `setExtra` / `setExtras` (Sentry's names) write arbitrary `metadata` keys. `addBreadcrumb(crumb,
  hint?)` and the automatic browser integrations (below) write `breadcrumbs`. New options:
  `maxBreadcrumbs` (0-100, default 100), `beforeBreadcrumb`, `breadcrumbs` (the automatic
  integrations' switch), `autoIpAddress` (browser only, default `true`), `serverName` (node only).
- **Automatic contexts and request, per runtime.** The browser sends `request.url` / `query_string`
  / a `User-Agent` header (filtered, see below) and `contexts.culture` / `.device`; node sends
  `contexts.runtime` / `.os` and `server_name`; the edge sends `contexts.runtime` only. **Neither
  runtime ever sends `contexts.browser` or `contexts.os` directly** — the ingestor derives both from
  the browser's `User-Agent` header itself, which is the one place that parsing already lives, so the
  SDK does not duplicate it (D2).
- **Automatic browser breadcrumbs**: console, `fetch`, `XMLHttpRequest`, `history.pushState` /
  `replaceState` / `popstate`, and clicks — each independently switchable through the `breadcrumbs`
  option, each on by default.
- **The URL privacy filter.** Every URL the SDK records — breadcrumb `data.url` / `.from` / `.to`,
  `request.url` / `.query_string`, every `stacktrace[].file`, and the event `message` — has a
  sensitive query value replaced with the literal `[Filtered]` (distinct from metadata's `[FILTERED]`,
  which is Go-side PII scrubbing parity). A key is sensitive when it contains `password`, `passwd`,
  `pass`, `token`, `bearer`, `secret`, `authorization`, `auth`, `credit_card`, `card_number`, `cvv`,
  `ssn`, `social_security`, `api_key` or `apikey` as a substring, or when one of its `-`/`_`/`.`/`[`/`]`-
  delimited segments is exactly `code`, `state`, `key`, `signature`, `sig`, `jwt`, `otp` or
  `credential`. Userinfo (`user:pass@host`) is always removed. See *URL privacy* below for what is
  deliberately **not** filtered.
- **The tunnel forwards seven more typed fields**: `level`, `sdk`, `user`, `tags`, `contexts`,
  `request`, `breadcrumbs` join the exact-key allow-list `ITEM_KEYS` already forwards.
- **New `metadata.truncated` values** `breadcrumbs`, `contexts` and `tags`, alongside the existing
  ones, recorded whenever the SDK itself removes one of those fields to fit a bound (see the next
  three points).
- **The NATS publish bound.** Every event is now estimated against the ingestor's own protobuf
  encoding (not just its JSON size) before it is queued, because a `Struct` value can cost up to 5.5×
  its JSON size on the wire. Over a 1,000,000-byte estimate (which assumes the ingestor's own
  enrichment adds at most ~48 KB after the SDK), breadcrumbs are shed oldest first, then contexts
  (custom names before the automatic ones, largest first), then non-exempt metadata keys — in that
  order, each shed only as far as needed. In the browser this runs after an existing keepalive trim
  (breadcrumbs shed to fit a 60 KiB single-event body, then, as a last resort for the tunnel's 256 KiB
  cap, `contexts` and every tag but `tenant_id`).
- **The metadata shrink's new end.** The server's own 18 exempt keys (`user_id`, `userId`, `user`,
  `tenant_id`, `tenantId`, `organization_id`, `trace_id`, `traceId`, `trace-id`, `span_id`, `spanId`,
  `span-id`, `request_id`, `requestId`, `request-id`, `release_version`, `truncated`,
  `truncated_keys`) replace the SDK's own exempt list; `route` is no longer exempt, because the server
  never protected it either. Once every non-exempt key is gone, the exempt keys are now removed too
  (largest first) instead of dropping the event — **the shrink never drops an event any more**, and
  `metadata_too_large` is no longer emitted (the code stays in `SentinelSdkErrorCode` so an
  exhaustive `switch` still compiles). `truncated_keys` is written in the server's own capped shape:
  its first 50 removed-key names, each cut to 128 bytes, then one `"+N more"` entry for the rest.
- **The SDK's own 50-tag cap.** An event's effective tag count (the scope's, merged with a per-call
  `ctx.tags`) over 50 keeps `tenant_id` and the 49 lexically smallest other keys — mirroring what the
  server would keep, so its own cut is a no-op on what the SDK sends.
- **The SDK's own contexts and request-map bounds**, applied before every send so the server's own
  cut never removes anything: `contexts` to 32 names / 64 KiB (custom names removed before the six
  automatic ones, largest first, a size tie to the lexically smallest name); `request.headers` /
  `.env` to 64 entries, values cut to 4096 / 1024 code points.

#### Changed

- **The tunnel route writes the browser's IP into every forwarded item's `user.ip_address`**
  (`sentinel-event-enrichment` TASK-06). The IP is `clientKey(req)` when given, else the
  `X-Forwarded-For` entry at `len − trustedProxyHops` (new option, default `1`, an integer ≥ 0;
  anything else → the default with one `warn`), in canonical form (IPv4-mapped IPv6 → dotted
  IPv4; anything that is not an IP → no address). It **overwrites** any value the browser sent. A
  browser-sent `user` is **rebuilt** from its exact `id` / `username` / `email` string values, then
  `ip_address`; no other key, no `geo`, and no case-folded spelling of either survives. With no
  address, no `user` is created and an existing one is rebuilt without an IP.
- **The forward carries two new headers**: the browser's `user-agent` (1–1024 bytes, no control
  character), so the ingestor can fill `contexts.browser` / `contexts.os`, and
  `x-sentinel-derive-ip: off` on every forward, so the ingestor never fills an item from the web
  server's own address. It still forwards **no** `X-Forwarded-For` and no other client header.
- **The default rate-limit key moved from the leftmost `X-Forwarded-For` hop to the same
  derivation** (the rightmost-trusted entry). The leftmost hop is written by the browser, so any
  client could mint a fresh bucket per request. `clientKey`, when given, keys exactly as before.
- New exports from `@sentinel/sdk-js/nextjs/route`: `applyClientIp`, `canonicalIp`,
  `clientAddress`, `USER_KEYS`, `DEFAULT_TRUSTED_PROXY_HOPS`, `DERIVE_IP_HEADER`,
  `MAX_FORWARDED_USER_AGENT_BYTES`. **No wire-vocabulary change** to `metadata`.

**`trustedProxyHops`:**

> `trustedProxyHops` must equal the number of reverse proxies in front of your app that append to `X-Forwarded-For`. The default, 1,
> assumes exactly one. A value larger than your real proxy count lets a client choose the IP your events are stored under.
>
> `0` means "never read `X-Forwarded-For`; use the socket address on the request". **The Next.js App Router exposes no socket**: a
> route handler gets a plain `Request`, and `NextRequest.ip` was removed in Next 15. So under the App Router, `0` makes the tunnel
> derive **no IP for any event**, and the default rate-limit key falls back to one shared bucket for every client. That is safe (no
> event is stored under a wrong address), but it turns the feature off. Next's own server fills `X-Forwarded-For` from the socket
> only when the request carries none, and keeps a client-sent header as it is. So with **no** proxy in front of the app, no hop count
> is trustworthy: `1` would store an address the client can choose. To get a real IP there, put a proxy that appends
> `X-Forwarded-For` in front of the app and count it, or pass a `clientKey` that reads an address you trust.

## v0.1.2 — Index maps upload

```sh
pnpm add 'git+https://github.com/tuju-ai/sentinel.git#packages/sdk-js/v0.1.2&path:packages/sdk-js'
```

### Changed

- **The sourcemap walker uploads index maps** (`sections`, no string `mappings`) — the shape
  Turbopack writes for every server entry stub and for its own runtime — instead of leaving them
  out with a `warn` each (`sentinel-sourcemaps-index-maps`; the route and processor accept
  `sections` since the same feature). The runtime's frames now symbolicate to
  `turbopack:///[turbopack]/…`. An empty stub (`{"version":3,"sources":[],"sections":[]}`) is
  acknowledged by the route as `skipped_empty` and not stored. **The one exception**: an index map
  any of whose sections carries a `url` key is still skipped, with one `warn` naming the file —
  the route rejects `url` sections for the whole batch, and a shape the route does not serve must
  not fail a build. Every other rejection an index map can draw is a malformed map, which fails
  the build exactly as a malformed flat map does.
- **The summary line counts the acknowledged stubs.** The CLI's *"`<n>` file(s) stored in `<r>`
  request(s), release `<v>`"* and `withSentinelConfig`'s *"Sentinel: `<series>` sourcemaps —
  `<n>` file(s) stored in `<r>` request(s) under release `<v>`"* now count as stored only the
  entries whose `outcome` is not `skipped_empty`, and gain the clause *"`<k>` empty index map(s)
  acknowledged and not stored"* when `<k>` > 0 — read from the route's `files[].outcome`, never
  re-derived from the walk. With `<k>` = 0 both lines read as before. `--dry-run` is unchanged: it
  lists every pair, stubs included, because the walker does not know which the route will skip.
  `--json` is unchanged in shape — the per-file `outcome` is where a JSON consumer counts.
- The internal `IndexMapKind` type (`src/upload/index.ts`) gains `'url'`; no public API, option or
  export changes. **No wire-vocabulary change**: the ingest JSON is untouched, and `files[].outcome`
  on the upload route is the dashboard's surface, not the SDK's `metadata`.

## v0.1.1 — The tunnel route never falls back from an explicit `apiKey` option

```sh
pnpm add 'git+https://github.com/tuju-ai/sentinel.git#packages/sdk-js/v0.1.1&path:packages/sdk-js'
```

### Fixed

- **`createSentinelRouteHandler({ apiKey })` with the option present but unset** (`undefined` or
  `''` — `apiKey: process.env.SENTINEL_TUNNEL_API_KEY` with the variable unset) no longer falls back
  to `SENTINEL_API_KEY`. The route answers `503 sentinel_not_configured` (the same envelope as
  before) and warns once per process — *the route apiKey option is unset; not falling back to
  SENTINEL_API_KEY* — without reading `SENTINEL_API_KEY` at all. Before, the browser silently spent
  the server/edge key's 5 000 rpm budget against the shared `ERROR_EVENTS` stream, which is exactly
  what a separate tunnel key (README §4, §6) exists to prevent; `web` had to carry its own guard.
  Presence is the test: only a mount that omits `apiKey` entirely — the one-line mount — reads
  `SENTINEL_API_KEY`, as documented. No wire-vocabulary change.

## v0.1.0 — First release

Installed as a git dependency of the `sentinel` repo, built on install by `prepare`:

```sh
pnpm add 'git+https://github.com/tuju-ai/sentinel.git#packages/sdk-js/v0.1.0&path:packages/sdk-js'
```

### Added

- **The package** — ESM only, `"type": "module"`, **no runtime dependencies**, `engines.node >= 20`,
  and one `exports` table: `.` (the runtime-agnostic API: `captureException`, `captureMessage`,
  `flush`, `close`, `setUser`, `setTenant`, `setTag`, `getClient`, the types), `./browser`,
  `./node`, `./edge`, the three **fixed typed subpaths** `./nextjs/client`, `./nextjs/server`,
  `./nextjs/edge` (each `init` typed for its own runtime — `tunnel` on the client, `endpoint` /
  `apiKey` on the server and edge, so a bundled key is a compile error), the conditional `./nextjs`
  (resolved per bundle by `edge-light` / `worker` / `workerd` / `browser` / `node` / `default`, with
  a union `.d.ts` that has no `init`), `./nextjs/route`, `./nextjs/config` and `./package.json`.
  Every entry is inert at import time: no listener, timer, `fetch` or env read until `init`.
- **`init(options)`** — synchronous, idempotent, never throws; degrades to a no-op client when
  unconfigured (`onError` `not_configured`) or when a `tunnel` / `endpoint` is unusable
  (`invalid_option`); a second `init` (Next dev HMR) drains the old queue into the new client and
  re-installs the global handlers once. Options: `release`, `environment`, `sampleRate`,
  `beforeSend`, `debug`, `onError`, `maxBufferSize`, `batchSize`, `batchWaitMs`, `inApp`,
  `tracesSampleRate` (accepted and ignored), `tunnel` (browser), `endpoint` / `apiKey` /
  `projectKey` (node, edge), `installGlobalHandlers`, `flushOnHide`. Every other Sentry option is
  a type error.
- **Capture** — `captureException(error, ctx?)`, `captureMessage(message, ctx?)` returning the
  `event_id`; `setUser` / `setTenant` / `setTag` on one module-level scope (kept across a second
  `init`, kept from calls made before the first); `flush(timeoutMs?)` and `close(timeoutMs?)`,
  both `Promise<boolean>`, never rejecting; `getClient()` — `undefined` before the first `init`
  and after `close`. `ctx` carries `metadata`, `contexts.react.componentStack`, `fingerprint`,
  `traceparent` and (for `captureMessage`) `level`. Every field the ingestor rejects on is bounded
  in bytes before send, metadata is shrunk to 64 KiB as the validator counts it, PII keys are
  scrubbed with the Go SDK's substring rule, an unserialisable input is kept with a marker, and a
  thrown object is deduplicated by identity so an error boundary and `window.onerror` report it
  once.
- **The transport** — a 100-event ring buffer, batches of 10 every second (or when full) in one
  `POST` of `[event, …]`, one batch in flight at a time, 4 attempts with backoff capped at 60 s,
  `Retry-After` honoured, the same `event_id`s on every retry, a page-hide send with `keepalive`,
  batches packed to ≤ 60 KiB. The edge build drains on the next microtask.
- **Global handlers** — the browser's `error` / `unhandledrejection` listeners (added, never
  assigned, so `@sentry/nextjs` beside them still fires) and node's `uncaughtExceptionMonitor` +
  `unhandledRejection`, which re-raises when the SDK is the sole listener so the process exits as
  it did without the SDK.
- **The Next.js adapter** — `captureRequestError` with Next's `onRequestError` signature (the edge
  build returns the send's promise), `onRouterTransitionStart` / `captureRouterTransitionStart`
  (no-ops), and `contexts.react.componentStack` → `metadata.react_component_stack`.
- **The tunnel route** — `createSentinelRouteHandler(options)` from `./nextjs/route`, returning
  `{ POST }` only: a plain `(Request) => Promise<Response>` that refuses cross-site requests by
  `web`'s same-origin rule, rate-limits per client (60/min per instance by default, `rateLimit:
  false` to disable, `clientKey` / `isCrossSite` overrides), caps the body at 256 KiB and 50 items,
  strips `project_key` / `project_id`, forwards to `{endpoint}/batch` with the server's key and
  relays the ingestor's status and body verbatim. Its own errors use the `{ error: { code, message,
  details } }` envelope: `method_not_allowed`, `cross_site_request`, `unsupported_media_type`,
  `payload_too_large`, `invalid_request`, `rate_limited`, `sentinel_not_configured`,
  `upstream_unavailable`.
- **The CLI** — `sentinel-sourcemaps upload <build dir> <public prefix>`: pairs every chunk with
  the map its `//# sourceMappingURL=` comment names (a Turbopack build symbolicates), skips index
  maps with a warn, posts batches of ≤ 50 files and ≤ 100 MiB to `POST /api/sourcemaps`, prints
  the stored path per file, exits `0` / `1` (any 4xx, or a 5xx / network failure after 4 attempts)
  / `2` (usage), and `--delete`s only what it uploaded.
- **The build wrapper** — `withSentinelConfig(nextConfig, options)` from `./nextjs/config`: enables
  browser and server sourcemaps, uploads both series in `compiler.runAfterProductionCompile`
  (Turbopack and webpack), deletes every `*.map` afterwards and repairs the `*.nft.json` traces
  that named them, fails the build on any 4xx and warns-and-continues on a 5xx. `upload` is
  tri-state; `release` must equal `init.release` and be unique per build when uploads are on.
- **A local `pnpm bench`** for `sdk_js_capture_enqueue_p95_us` and a report-only `pnpm size` for
  `sdk_js_client_entry_gzip_bytes` (no threshold in v0.1).

### Wire vocabulary the dashboard reads as-is

- `platform` is always `"javascript"`; **`metadata.runtime`** is `browser` | `node` | `edge`.
- **`metadata.source`** is set only by a global handler and is exactly one of `window.onerror` |
  `unhandledrejection` | `uncaughtException` | `unhandledRejection`; absent on every explicit
  capture and on `captureRequestError`.
- **`metadata.level`** is `error` | `warning` | `info`, only from `captureMessage`.
- **`metadata.truncated`** (`string[]`) names every field the SDK cut: `message`, `error_class`,
  `react_component_stack`, `stacktrace`, `stacktrace.file`, `stacktrace.function`.
- **`metadata.truncated_keys`** (`string[]`) names the metadata keys the 64 KiB shrink removed and
  the dotted paths the safe serialiser substituted (`req.socket`, `items.0.socket`).
- `captureRequestError` writes `metadata.route`, `http_method`, `http_url`, `router_kind`,
  `route_type`, `render_source`, `revalidate_reason`; `contexts.react.componentStack` lands in
  `metadata.react_component_stack`; `setUser` / `setTenant` land in `metadata.user_id` /
  `metadata.tenant_id`.
- `onError` codes: `not_configured`, `invalid_option`, `before_send_threw`,
  `handler_install_failed`, `item_rejected`, `batch_dropped`, `buffer_overflow`,
  `fingerprint_too_long`, `release_too_long`, `metadata_too_large`.

### Not in this release

Breadcrumbs, tracing, per-request server scope, `withSentinelErrorBoundary`, index-map
(`sections`) uploads, and a threshold on the bundle size — see the README's *What is not
implemented*.
