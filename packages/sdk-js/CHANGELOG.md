# Changelog

All notable changes to `packages/sdk-js` (`@sentinel/sdk-js`) are documented here — one entry per
tag (`packages/sdk-js/vX.Y.Z`). The package is major-zero: a minor bump may change shape, and every
wire-vocabulary change is named here because the dashboard reads `metadata` as-is.

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
