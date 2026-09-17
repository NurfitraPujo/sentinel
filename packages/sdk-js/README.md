# `@sentinel/sdk-js`

The Sentinel error-tracking SDK for JavaScript and TypeScript: browser, node and edge entries, a
Next.js adapter, a tunnel route for the browser, the `sentinel-sourcemaps` CLI and the
`withSentinelConfig` build wrapper. Every runtime sends `platform: "javascript"` and says which it
is in `metadata.runtime`; the wire is `docs/sdk-specification.md`'s `ErrorEvent`, nothing more.

Every application code block below is a file that was built and run against the compose stack before it was
pasted here (the *Bundle size* and *Local bench* numbers were measured the same way); the
`CHANGELOG.md` names what shipped in each tag.

## Next.js quickstart

### 1. Install

```sh
pnpm add 'git+https://github.com/tuju-ai/sentinel.git#packages/sdk-js/v0.1.1&path:packages/sdk-js'
```

The package is a **git dependency** of the `sentinel` repo, pinned to a tag, with `path:` pointing
at the package directory — there is no registry. `tuju-ai/sentinel` is private, so the machine
that installs needs a token with `Contents: read` on it: a developer's `gh auth` credential does,
and CI or a Dockerfile rewrites the URL with `git config url."https://x-access-token:${TOKEN}@github.com/".insteadOf "https://github.com/"`
before `pnpm install` (the same rewrite `web` already does for `@loom/ui`). **Node ≥ 20 at
runtime, node ≥ 20 to build**: the runtime entries use nothing newer than node 20's baseline
because `web`'s pods run `node:20-alpine`; the CLI and the wrapper run on the build runner under
the same floor.

**The mechanism, in four lines** — pnpm runs a git dependency's `prepare` through **npm**, so:

1. `prepare` calls `node` / `npm run`, never `pnpm` — a `"prepare": "pnpm build"` is a failed
   install for every Docker consumer, where pnpm is not on the path at install time.
2. Every `devDependency` is exact-pinned — the consumer's npm-driven build resolves them fresh
   from `package.json`, so a caret would compile the package with a different toolchain than CI.
3. `pnpm-lock.yaml` is CI's only — it governs the `sdk-js-node` job's `--frozen-lockfile` and is
   never read on the consumer path.
4. `npm install && npm run prepare` in a clean clone of `packages/sdk-js`, on node 20 and 22, is
   the reproducibility check — it is what a consumer's install actually runs, and CI runs it too.

### 2. The three init files

Three files, **each importing its own subpath**. The three strings differ because each subpath is
a fixed file with its own `.d.ts` and no export conditions, so the options type is the file's own
runtime — `tunnel` exists only on `@sentinel/sdk-js/nextjs/client`, `endpoint` / `apiKey` only on
`/nextjs/server` and `/nextjs/edge` — and `init({ apiKey })` in a client file is a compile error
rather than a key in the browser bundle (TypeScript under `moduleResolution: bundler` never
matches a `browser` / `edge-light` condition, so one conditional entry could not give you that).

<!-- snippet: instrumentation-client.ts -->
```ts
import * as Sentinel from '@sentinel/sdk-js/nextjs/client';

Sentinel.init({
  tunnel: '/api/sentinel',
  release: process.env.NEXT_PUBLIC_APP_VERSION ?? '',
  environment: process.env.NEXT_PUBLIC_ENVIRONMENT || 'development',
  beforeSend(event, hint) {
    const error = hint.originalException;
    const message = typeof error === 'string' ? error : error instanceof Error ? error.message : '';
    if (message.includes('Request failed with status code 403') || message.includes('Request failed with status code 401')) return null;
    return event;
  },
});

export const onRouterTransitionStart = Sentinel.captureRouterTransitionStart;
```

<!-- snippet: sentinel.server.config.ts -->
```ts
import * as Sentinel from '@sentinel/sdk-js/nextjs/server';

Sentinel.init({
  endpoint: process.env.SENTINEL_ENDPOINT,
  apiKey: process.env.SENTINEL_API_KEY,
  tracesSampleRate: 0,
  release: process.env.NEXT_PUBLIC_APP_VERSION ?? '',
  environment: process.env.NEXT_PUBLIC_ENVIRONMENT || 'development',
  beforeSend(event, hint) {
    const error = hint.originalException;
    const message = typeof error === 'string' ? error : error instanceof Error ? error.message : '';
    if (message.includes('Request failed with status code 403') || message.includes('Request failed with status code 401')) return null;
    return event;
  },
});
```

<!-- snippet: sentinel.edge.config.ts -->
```ts
import * as Sentinel from '@sentinel/sdk-js/nextjs/edge';

Sentinel.init({
  endpoint: process.env.SENTINEL_ENDPOINT,
  apiKey: process.env.SENTINEL_API_KEY,
  tracesSampleRate: 0,
  release: process.env.NEXT_PUBLIC_APP_VERSION ?? '',
  environment: process.env.NEXT_PUBLIC_ENVIRONMENT || 'development',
  beforeSend(event, hint) {
    const error = hint.originalException;
    const message = typeof error === 'string' ? error : error instanceof Error ? error.message : '';
    if (message.includes('Request failed with status code 403') || message.includes('Request failed with status code 401')) return null;
    return event;
  },
});
```

The `beforeSend` shape is `@sentry/nextjs`'s, and **`hint.originalException` is defined on every
path: the thrown value as passed for `captureException` (an `Error`, a plain object, a
primitive), the message string for `captureMessage`, and `ev.message` for the message-only
`ErrorEvent` capture** (a cross-origin `"Script error."`) — so `hint.originalException?.message`
is `undefined` on a string and a filter written against Sentry falls through to its `event.message`
branch exactly as before. `tracesSampleRate` is accepted and ignored so an init object copied
from Sentry compiles; `dsn`, `integrations` and every other Sentry option are type errors, so the
swap is conscious. `release` and `environment` are the same constants in all three files (see
*Naming the release*).

### 3. `instrumentation.ts`

Bundled for every runtime, so it — and every other file that is not an init file — imports the
conditional `@sentinel/sdk-js/nextjs`, whose union `.d.ts` has no `init` and only what every
runtime provides; the bundler's export conditions pick the client, server or edge build per bundle.

<!-- snippet: instrumentation.ts -->
```ts
import * as Sentinel from '@sentinel/sdk-js/nextjs';

export async function register() {
  if (process.env.NEXT_RUNTIME === 'nodejs') await import('./sentinel.server.config');
  if (process.env.NEXT_RUNTIME === 'edge') await import('./sentinel.edge.config');
}

export const onRequestError = Sentinel.captureRequestError;
```

`captureRequestError` is Next's `onRequestError` signature exactly and is the only path by which
a server-render or route error reaches Sentinel under Turbopack; it stamps `metadata.route`,
`http_method`, `http_url`, `router_kind`, `route_type` (and `render_source` / `revalidate_reason`
when Next gives them) and no `metadata.source`. `web`'s two utilities keep their bodies and swap
the import the same way — the error-boundary helper is `captureException` with the Sentry
`contexts.react.componentStack` shape (→ `metadata.react_component_stack`, ≤ 4 KiB):

<!-- snippet: src/common/utils/report-boundary-error.ts -->
```ts
import * as Sentinel from '@sentinel/sdk-js/nextjs';

export function reportBoundaryError(error: unknown, componentStack?: string | null): string {
  return Sentinel.captureException(error, { contexts: { react: { componentStack } } });
}
```

<!-- snippet: src/common/utils/stale-deploy.ts -->
```ts
import * as Sentinel from '@sentinel/sdk-js/nextjs';

export const STALE_DEPLOY_FLUSH_TIMEOUT_MS = 2000;

export async function reloadForStaleDeploy(): Promise<void> {
  await Sentinel.flush(STALE_DEPLOY_FLUSH_TIMEOUT_MS);
  window.location.reload();
}
```

### 4. The tunnel route — `app/api/sentinel/route.ts`

The browser never holds a key: it posts to a route on the app's own origin, which adds the key
and forwards to the ingestor. The one-line mount:

<!-- snippet: app/api/sentinel-one-line/route.ts -->
```ts
import { createSentinelRouteHandler } from '@sentinel/sdk-js/nextjs/route';

export const { POST } = createSentinelRouteHandler();
```

**The factory exports `{ POST }` only, and Next serves its own `405` for every other method**
before the handler runs — there is no `GET` export to route a `GET` into. The mount `web` uses
passes its own same-origin rule and its own client-IP reader, and **the tunnel's own ingest key**
(*Environment variables*, below):

<!-- snippet: app/api/sentinel/route.ts -->
```ts
import { createSentinelRouteHandler } from '@sentinel/sdk-js/nextjs/route';
import { getClientIp } from '@/app/api/client-ip';
import { isCrossSiteRequest } from '@/app/api/same-origin';

export const { POST } = createSentinelRouteHandler({
  apiKey: process.env.SENTINEL_TUNNEL_API_KEY,
  isCrossSite: isCrossSiteRequest,
  clientKey: getClientIp,
});
```

**Passing `apiKey` turns the `SENTINEL_API_KEY` default off, even when its value is unset.** The
factory looks at whether the property is *present*, not at its value: with `SENTINEL_TUNNEL_API_KEY`
unset, the mount above hands the route `apiKey: undefined`, and the route answers every request
`503 sentinel_not_configured` (one `console.warn` per process: *the route apiKey option is unset;
not falling back to SENTINEL_API_KEY*) rather than quietly picking up the server/edge key and letting
the browser spend that key's rate-limit budget (*Environment variables*, below). Only a mount that
omits the option entirely — the one-line mount — reads `SENTINEL_API_KEY`. So a missing tunnel key
is a loud `503` in the browser SDK's `onError` and in the server log, not a silent budget merge.

`isCrossSite` replaces the built-in origin check (which is `web`'s `isCrossSiteRequest` transcribed
minus its `NEXT_PUBLIC_FRONTEND_URL` lookup — pass the real one to get byte-identical behaviour);
`clientKey` replaces the rate-limit key (*The tunnel's rate limit*). A callback that throws fails
closed: `isCrossSite` → `403`, `clientKey` → the shared bucket. The route refuses a cross-site
request with `403 cross_site_request`, anything but `application/json` with `415`, more than
256 KiB or 50 items with `413` / `400`, deletes `project_key` / `project_id` from every item,
forwards **no** client header, never retries its single forward (the browser SDK owns retries) and
relays the ingestor's status and body verbatim — so a `429` reaches the browser SDK with its
`Retry-After`. Its own errors use `{ "error": { "code", "message", "details" } }`.

### 5. `next.config.mjs`

<!-- snippet: next.config.mjs -->
```js
import { readFileSync } from 'node:fs';
import { withSentinelConfig } from '@sentinel/sdk-js/nextjs/config';

// ONE constant names the release: withSentinelConfig.release here and init.release in the three init files
// (through NEXT_PUBLIC_APP_VERSION). Production builds keep <version>; a build that can happen more than once
// per version (staging, a branch) sets APP_RELEASE=<version>+<sha> — see "Naming the release" in the README.
const { version } = JSON.parse(readFileSync(new URL('./package.json', import.meta.url), 'utf8'));
const release = process.env.APP_RELEASE || version;

/** @type {import('next').NextConfig} */
const nextConfig = {
  output: 'standalone',
  env: { NEXT_PUBLIC_APP_VERSION: release },
};

export default withSentinelConfig(nextConfig, {
  release,
  sourcemaps: {
    // tri-state: leave `upload` UNDEFINED unless the env says off — undefined means "upload when
    // dashboardUrl + apiKey resolve, warn and still delete the maps when they do not"; an explicit `true`
    // with either unresolved FAILS the build; `false` never uploads (the maps are still deleted).
    upload: process.env.SENTINEL_UPLOAD_SOURCEMAPS === 'false' ? false : undefined,
    dashboardUrl: process.env.SENTINEL_DASHBOARD_URL,
    apiKey: process.env.SENTINEL_API_KEY,
    projectKey: process.env.SENTINEL_PROJECT_KEY,
    deleteAfterUpload: true,
    publicPrefix: '/_next/static',
    serverRoot: '/app/.next/server',
  },
  silent: false,
});
```

`withSentinelConfig` accepts every shape Next accepts (an object, a `(phase, { defaultConfig })`
function, an async one) and returns the same shape. It sets `productionBrowserSourceMaps` and
`experimental.serverSourceMaps`, chains `compiler.runAfterProductionCompile` after any hook the
config already has, uploads the browser and server maps in that hook (Turbopack **and** webpack),
then deletes every `*.map` — see *Server sourcemaps and `serverRoot`* and *When the upload fails*.
It touches nothing else in the config: no `env`, no headers, no CSP (`connect-src 'self'` already
covers the tunnel).

### 6. Environment variables

| Variable | Read by | Meaning |
| --- | --- | --- |
| `SENTINEL_ENDPOINT` | the server and edge `init`, the route | the ingest URL, e.g. `https://api-sentinel.daya.ai/ingest`; batches go to `{endpoint}/batch` |
| `SENTINEL_API_KEY` | the server and edge `init`, the route (default — only when no `apiKey` option is passed), the CLI, the wrapper | a project-scoped `ingest` key — **the runtime key** in the pods, **the build key** in CI |
| `SENTINEL_PROJECT_KEY` | the same | the project **name**, sent as `X-Project-Key`; only for an organization-wide (`sent_org_…`) key |
| `SENTINEL_DASHBOARD_URL` | the CLI, the wrapper | the dashboard host the maps are uploaded to, e.g. `https://sentinel.daya.ai` |
| `SENTINEL_UPLOAD_SOURCEMAPS` | your `next.config.mjs` (above) | `false` turns the upload off; the maps are still deleted |
| `SENTINEL_TUNNEL_API_KEY` | your `route.ts` (above) — an app-side name | the tunnel's own ingest key (below); unset with the option passed → `503`, never `SENTINEL_API_KEY` |
| `NEXT_PUBLIC_SENTINEL_TUNNEL` | your `instrumentation-client.ts`, if you want one | an app-side convention: `init({ tunnel: process.env.NEXT_PUBLIC_SENTINEL_TUNNEL ?? '/api/sentinel' })`. The library reads no `NEXT_PUBLIC_*` itself |

The browser build reads **no** environment; the server-side code reads its variables once, at
`init` / startup. Two rules, because `web`'s obvious CI move leaks the key — `release.yml`
generates `.env` from secrets before `pnpm build`, Next 15.5.21's `writeStandaloneDirectory`
copies `.env` / `.env.production` into `.next/standalone`, and `Dockerfile.swr` does
`COPY .next/standalone ./`:

- **The build-time key is a job `env:` var, never written to the generated `.env`.**
- **The runtime key is a pod secret, never a `NEXT_PUBLIC_*`.**

**Give the tunnel its own ingest key.** The ingestor's 5 000 requests/minute limit is **per key**,
so a tunnel and the server/edge `init`s sharing one `SENTINEL_API_KEY` share one budget: a browser
error storm (≈ 85 concurrent users at one batch per second) 429s the server-render and middleware
reporting paths for the same minute — exactly when SSR reporting matters. The mount above passes
`apiKey: process.env.SENTINEL_TUNNEL_API_KEY` and keeps `SENTINEL_API_KEY` for the server/edge SDK
and the build; both are project-scoped ingest keys of the same project. **Create the tunnel's key
with `rate_limit_rpm` ≤ 600**: the ingestor's `ERROR_EVENTS` stream (10 GiB) is shared by **every
producer of the instance**, and a browser flood through a key at the 5 000 rpm default could fill
it in minutes — **after which every producer of the instance `400`s — `tuju-ai/core` included —
for up to 24 h or until an operator purges the stream** (which also discards the legitimate
events still in it). At ≤ 600 rpm — ≈ 100 events/s, ≈ 2 MiB/s worst case — filling the stream
takes ≈ 1 h instead. The key's own limit is the only per-producer cap in front of that stream; the
value is set when the key is created (`project_api_keys.rate_limit_rpm`). Before mounting the
tunnel, confirm that `sentinel_ingest_publish_failures_total` is alerted on.

**Create both keys without an expiry (`expiresInDays` blank), or record the expiry as a
release-blocking date**: an expired key is a `401` identical to an unknown one, so the **build** key
turns a release red on a calendar date (*When the upload fails*) and the **tunnel / server** key
turns every batch into a silent `batch_dropped` 401 in `onError`, with nothing in Sentinel.

### 7. The CI step

The upload runs inside `pnpm build`, on the **self-hosted runner pool** — the dashboard host is on
the internal ELB, and a GitHub-hosted runner sees a connection timeout, not a `4xx`. The key is a
step `env:` from a secret (rule 1 above); `SENTINEL_UPLOAD_SOURCEMAPS=false` is the local override
for a build that must not upload (a laptop, a fork):

<!-- snippet: ci/release-step.yml -->
```yaml
      - name: Build Next.js
        # Self-hosted runner: the dashboard host is on the internal ELB (docs/sdk-specification.md §8, Reachability).
        # The build-time key is a job `env:` var, never written to the generated .env — Next copies .env into
        # .next/standalone, and the Dockerfile copies .next/standalone into the image.
        env:
          SENTINEL_DASHBOARD_URL: ${{ secrets.SENTINEL_DASHBOARD_URL }}
          SENTINEL_API_KEY: ${{ secrets.SENTINEL_API_KEY }}
          # SENTINEL_UPLOAD_SOURCEMAPS=false turns the upload off (maps still deleted) — the local override.
        run: |
          rm -rf .next
          pnpm build
```

## Naming the release

**One constant** names the release: `withSentinelConfig.release` and `init.release` in the three
init files must be byte-identical (the `next.config.mjs` above injects it as
`NEXT_PUBLIC_APP_VERSION`, the way `web` does), because the processor looks a frame's map up by the
exact `release_version` the event carries. **And when uploads are on the release must be unique
per build**: a production release keeps `<version>` (`2.34.0`, built once per version); any build
of a version that can happen more than once — `web`'s staging is `workflow_dispatch` from any
branch; a branch build — uses `<version>+<sha>` (`2.34.0+5318bc6`, `APP_RELEASE` in the snippet),
because Turbopack chunk names are content hashes and a second build of one version would exceed
the per-release cap or silently `replace` the ~128 unhashed server entries
(`.next/server/app/page.js` carries no hash) with another build's source. `+` is legal in
`release_version` (≤ 100 bytes; a longer value is omitted from every event with one `onError`).

An **empty release with uploads on** fails the build before either walk (a release-less upload can
never symbolicate anything); with uploads off it is fine. A release with leading or trailing
whitespace — the `$(…)` newline — fails the same way; nothing trims it.

**One project shared across environments** gets a project-level `releases` row and ≈ 105 MB of
maps (89 MB server + 16 MB client for `web`) **per staging or branch build**, each listed in the
releases picker's newest-50 beside production's — at `web`'s ≈ 3 builds a day that is ≈ 9 GB a
month with no retention and a picker that covers ≈ 2.5 weeks. **Recommend a separate Sentinel
project (or at least a separate key) per environment.**

## Server sourcemaps and `serverRoot`

The wrapper uploads the server maps too, under
`<serverRoot>/<path relative to .next/server>` — so `.next/server/app/page.js.map` is stored as
`/app/.next/server/app/page.js`. A node frame's `file` is the **absolute filesystem path** V8
prints (Next's production server installs its own `Error.prepareStackTrace`, deliberately not
source-mapped, and the SDK reads `error.stack` as a string and never replaces it), and the
processor matches it byte for byte: **`serverRoot` must equal the container's runtime path of
`.next/server`**. The default `/app/.next/server` is the standalone image's `WORKDIR /app` plus
`.next/server`. To read the real value off a deployment, open any server-side issue
(`metadata.runtime = "node"`) and look at the raw `file` of an app frame — everything before
`/app/…` or `/pages/…` is the `serverRoot`. A wrong `serverRoot` costs unsymbolicated server
frames, never a failed build.

Server maps count against **`SOURCEMAP_MAX_FILES_PER_RELEASE`** — **2 000 by default since
`sentinel-sourcemaps-turbopack-grouping`** — and `web`'s Turbopack build holds ≈ 794 server + 447
client ≈ 1 240 maps per release, so a `web` release fits with room; the
`SOURCEMAP_MAX_FILES_PER_RELEASE + 1`th file is still `422 release_full`, which fails the build. They also
cost ≈ +90 MB of object store per `web` release (server maps carry full `sourcesContent` of server
code — private source at rest in the bucket) and run 1–7 MB each against the processor's 64 MiB
positive cache, so cache-miss and `SYMBOLICATE_DEADLINE` timeouts on server frames are expected
until that cache is re-sized. The processor strips Turbopack's `_<8-hex>` suffix from an
unsymbolicated server chunk name (`_052cc036._.js` → `_*._.js`, `node_modules__pnpm_52f51f25._.js`
→ `node_modules__pnpm_*._.js`), so a server frame that symbolication does **not** rewrite groups
across builds (since `sentinel-sourcemaps-turbopack-grouping`). Turbopack also emits **index
maps** (`sections`, no `mappings`) for its server entry stubs and its own runtime; the walk leaves
those out with one `warn` each, because the route stores flat maps only — nothing that could
symbolicate is lost.

Under webpack with `output: 'standalone'`, Next's per-page build traces list the server maps; the
wrapper rewrites those `.nft.json` files after deleting the maps (one `Sentinel: build trace …
removed` line each), so the standalone copy never looks for a deleted map and the image ships
none. Turbopack's traces list no maps, so nothing is rewritten there.

## When the upload fails

- **Any 4xx fails the build** — `401` / `403` (the key, its scope, its expiry), `400`, `413`,
  `422 release_full` at the per-release cap: the hook rejects with the envelope, **no** map is
  deleted, and a series that already stored stays stored (a re-run is `unchanged`). Loud on purpose.
- **Any 5xx or network failure — whatever the body** — a `503 storage_unavailable` envelope, a
  `500`, a proxy's `502` / `504`, a plain-text ELB `503`, a refused connection, a timeout — is one
  class: retried 4 times, then **one `warn` per series** (`Sentinel: <browser|server> sourcemap
  upload failed (<status or error>) after <n> attempts — deleting maps anyway; release <release>
  will not symbolicate`), the other series still runs, every `*.map` is deleted when
  `deleteAfterUpload`, and **the build continues**. The release then never symbolicates for that
  series; recover by rebuilding under the same `release` (identical bytes re-upload as
  `unchanged`). The status or `code` is printed, never branched on.
- **Three more loud cases**: `upload: true` with `dashboardUrl` / `apiKey` unresolved; a walk that
  finds zero `*.js.map` with uploads on (another wrapper overrode `productionBrowserSourceMaps` /
  `experimental.serverSourceMaps`); an empty or whitespace-padded `release` — each throws before
  either walk and leaves the maps in place.
- `deleteAfterUpload: false` never deletes, whatever happened; `silent: true` hides the per-file
  report only — **every `warn` above still prints**.

## The tunnel's rate limit

The route rate-limits per client with an in-memory token bucket: **60 requests per minute per
client per instance** by default (capacity 60, one token per second back), a refused request
answering `429 rate_limited` with `Retry-After: <seconds until the next token>`. It is per pod, not
shared across replicas, bounded at 10 000 keys per instance (least-recently-seen eviction, idle
buckets swept), and an abuse speed bump rather than a guarantee — the ingestor's own per-key
limit is the real cap. `rateLimit: false` disables it; `rateLimit: { perMinute: n }` resizes it.

The default key is the first non-empty hop of `X-Forwarded-For`. Next 15.5 fills a missing
`x-forwarded-for` from the socket, so behind a proxy that does not transfer the client IP **every
user of the app shares one 60/min bucket per pod** — and the first hop is caller-writable. So
**log one request's `x-forwarded-for` on the tunnel before trusting the default**; pass
`clientKey: getClientIp` (`web`'s `src/app/api/client-ip.ts`, rightmost-by-`TRUSTED_PROXY_HOPS`,
what its other routes key on — the mount above); and pass `rateLimit: false` or a higher
`perMinute` if the logged header shows the proxy's address.

## Which frames are in-app

A frame is `in_app: false` when its `file` is empty, contains `/node_modules/`, starts with
`node:`, or is a webpack-named framework / runtime chunk
(`/_next/static/chunks/(framework|main|main-app|webpack|polyfills)[-.]`, `…/chunks/pages/_app-`,
`webpack-internal:///(…)/./node_modules/`); **`true` otherwise**. That resolves a dev build, a
plain-node stack and a webpack production build. **On a Turbopack production build every bundle
frame is `in_app: true`**: every client chunk is a content-hashed `<16-hex>.js` (409 of 409 in
`web`'s build), and the rule cannot see through the name. **`init({ inApp })` governs the frames
the processor did not symbolicate.** A symbolicated frame's `in_app` is the processor's — set from
the original path (`/node_modules/` and a non-`[project]` Turbopack namespace root are not in-app;
everything under `[project]` is) — and the value the SDK sent is kept beside it under `raw.in_app`.
`inApp` is a per-frame refinement called with the default already set, `frame => frame.in_app &&
!frame.file.includes('/vendor/')`, `undefined` to keep the default; a throwing `inApp` keeps the
default and never loses an event.

## Node crashes

The node entries observe uncaught exceptions through `process.on('uncaughtExceptionMonitor')`,
which does not stop the process from exiting, and register `process.on('unhandledRejection')`
to capture — then, **when the SDK's listener is the only one**, re-raise the reason on the next
tick so the default `--unhandled-rejections=throw` behaviour crashes the process exactly as before,
now with the event captured first. Consequences, stated:

- **The crash header points at the SDK's re-raise.** The re-raise is `process.nextTick(() => {
  throw reason })`, so node's source-context header (the lines before `Error:`) names the SDK's
  line; the exit code and everything from the first `Error:` line down are identical to the
  script without the SDK. A **primitive rejection reason** is re-thrown as-is — exit code 1 still,
  but node's own `ERR_UNHANDLED_REJECTION` wrapper for a primitive is not reproduced.
- **Under `--unhandled-rejections=warn` or `=none` the SDK's presence makes an unhandled rejection
  fatal**: without the SDK such a process exits `0`; the sole-listener re-raise turns it into an
  uncaught exception, exit `1`.
- **When the SDK is the sole listener, the crash event itself is never delivered**: the capture
  enqueues and returns, the worker runs on `unref()`ed timers, and node's fatal path exits without
  another loop turn — deterministically, not "the last batch may be lost". A crash capture is
  delivered only on the host-listener branch, where the host already has an `unhandledRejection`
  listener and the process keeps running: **`web`'s server runs that branch** (`@sentry/node-core`
  inside `@sentry/nextjs` registers one) **and is restarted by Kubernetes**.
- `beforeSend` is called **synchronously** on both node global-handler captures; a
  promise-returning hook's result is ignored there and the event is sent as built.

The SDK installs no `beforeExit` / `SIGTERM` hook — the host owns shutdown; `await flush(2000)`
on your shutdown path. Timers are `unref()`ed, so an idle SDK never keeps a process alive.

## Timestamps

**The SDK always sends `timestamp`** (`new Date().toISOString()` at capture); a skewed browser
clock — more than 60 s ahead of or 30 days behind the ingestor's clock — gets it replaced with the
ingestor's receipt time, the effective value is echoed per item in the batch response's
`event_ids[]`, and the SDK ignores the echo (it reads only `errors[]`).

## Coexisting with `@sentry/nextjs`

Both SDKs run side by side in one app: the browser listeners are added with `addEventListener`,
never assigned to `window.onerror`, so Sentry's wrapping and this SDK's listeners both fire and
neither swallows the other; neither sees the other's `captureException` calls; `beforeSend`
filters are per SDK; nothing here imports, detects or patches Sentry.

When both wrappers are composed and Sentry's `deleteSourcemapsAfterUpload` is on, **the one
composition order is `withSentryConfig(withSentinelConfig(nextConfig, sentinelOptions), sentryOptions)`**:
each wrapper chains the `runAfterProductionCompile` hook it finds on the config it receives, so
the inner (Sentinel) hook runs first — it uploads and deletes, then Sentry's step finds no maps
and does not fail. The reverse order runs Sentry's delete before Sentinel's walks, which is a red
build (zero maps found). **Sentry's hook rewrites every chunk after Sentinel's — `injectDebugIds`
prepends its snippet when a map is still present and appends it when the map is gone — and the
uploaded maps stay valid because Sentinel deleted the maps first, so by the time Sentry rewrites,
the positions the stored maps describe are the positions the served chunks still have.** Without a
`SENTRY_AUTH_TOKEN`, Sentry's step still runs `injectDebugIds` / `deleteArtifacts` and prints
*"no auth token provided"* — expected, not a failure. `web`'s Sentry init stays beside the Sentinel
one in each init file for as long as it keeps Sentry (session replay), with a dummy DSN or its own.

## Edge

**`captureRequestError` on the edge returns the send's promise, which Next awaits, so during an
ingestor incident a middleware error response is held for up to the per-attempt request timeout
(5 s; the first attempt only — retries run on timers after the promise resolves) before Next's 500
goes out.** Bounded, error path only. The edge build drains on the next microtask — events captured
in one tick go out in one `POST`, a lone capture is its own one-element batch (`batchWaitMs` is
ignored there) — so a runtime that stops the isolate at the response still gets the event. No
global handlers exist to install on the edge; `captureRequestError` is the path.

## What is not implemented

- **Breadcrumbs** — the wire has no field; `max_breadcrumbs` is not an option.
- **Tracing** — `tracesSampleRate` is accepted and ignored; trace ids come only from a
  `traceparent` string you pass (`ctx.traceparent`, or the request header in `captureRequestError`).
- **Per-request server scope** — `setUser` on the server leaks to the next request on the same
  worker; pass `metadata: { user_id }` on the capture call instead (`captureRequestError` never
  calls `setUser`).
- `withSentinelErrorBoundary` — Next's `error.tsx` / `global-error.tsx` call the boundary helper
  above.
- Index-map (`sections`) uploads — skipped with a `warn` until the route and processor accept them.

## Bundle size

**`sdk_js_client_entry_gzip_bytes` = 11 198** — the first real measurement, taken by `pnpm size`
at `365863c` on node 22 and re-measured unchanged at `735345f`, the head this README was written from: **gzip of the raw
`dist/nextjs/client.js` as built (32 043 bytes minified — the file the `browser` condition of
`./nextjs` resolves to, everything it statically imports bundled in), at `zlib`'s default
compression level**. Not a tree-shaken consumer bundle, not a `next build` chunk, not level 9.
`pnpm size` is **report-only in v0.1**: it prints the number and exits `0`; **the threshold is a
decision taken on this number before the tag, never a CI value edited in place.**

## Local bench

**`sdk_js_capture_enqueue_p95_us` ≈ 470** (the median of three runs: 664 / 336 / 470 µs; p50
261–316 µs) — from a **local `pnpm bench`, not a CI step; there is no bench artefact to look for
in CI**. `bench/capture-enqueue.bench.ts`, vitest's bench runner on node 22: 10 000 warm calls
after 1 000 warm-up, each `captureException(new Error(…))` with a 30-frame stack and a 20-key
metadata (four PII keys for the scrub), `fetch` stubbed to a `202`, each call timed with
`performance.now()`. Taken on a shared machine under load — a number to record, not to gate on;
no threshold in v0.1.

## CLI reference

`sentinel-sourcemaps` is the package's `bin` (node ≥ 20), for a pipeline that does not build with
Next — the wrapper calls the same `uploadSourcemaps()` in-process.

<!-- snippet: cli --help -->
```
sentinel-sourcemaps upload <build dir> <public prefix> [options]
  --release <string>        release_version, VERBATIM (env SENTINEL_RELEASE)      required
  --dashboard-url <url>     https://sentinel.daya.ai (env SENTINEL_DASHBOARD_URL) required
  --api-key <string>        (env SENTINEL_API_KEY)                                required
  --project-key <string>    X-Project-Key for an org-wide key (env SENTINEL_PROJECT_KEY)
  --delete                  delete each uploaded *.js.map / *.mjs.map after ITS batch succeeded
  --batch-size <n>          default 50 (the route's per-request file cap); outside [1, 50] → clamped, one warn;
                            non-numeric → exit 2; ≤ 100 MiB serialised per request regardless
  --dry-run                 print the pairs and exit 0 without posting
  --json                    one JSON object per file on stdout
  --help                    this text

Pairs every *.js / *.mjs under <build dir> to the map its trailing //# sourceMappingURL= comment names, under
<public prefix>/<relative path of the chunk> (Turbopack names a map by a different hash than its chunk); a map no
chunk references is paired by its own name, <public prefix>/<relative path without .map>. Posts them to
<dashboard url>/api/sourcemaps in batches of ≤ 50 files and ≤ 100 MiB. Exit 0 when every batch was
stored; 1 on any 4xx (the envelope is printed) or a 5xx / network failure after 4 attempts; 2 for a
usage error, an unreadable dir or zero maps.
```

- **Pairing:** every `*.js` / `*.mjs` whose trailing `//# sourceMappingURL=` comment names a map
  that exists is uploaded under `<public prefix>/<the chunk's relative path>` (this is what makes a
  Turbopack build symbolicate — its map names are not its chunk names); a map no chunk references
  keeps the name-based pairing. `sentinel-sourcemaps upload .next/static /_next/static` pairs
  browser maps; `sentinel-sourcemaps upload .next/server /app/.next/server` pairs server maps to
  the absolute path the node runtime prints.
- **Exit codes:** `0` every batch 2xx; `1` any 4xx (the envelope printed, later batches not sent,
  earlier ones kept, nothing deleted) or a 5xx / network failure after 4 attempts; `2` a usage
  error before any request — a missing or empty option, an empty or whitespace-padded `--release`,
  a non-numeric `--batch-size`, an unreadable dir, zero maps.
- **Output:** `<outcome>  <minified_path as stored>  <sha256[:12]>  <size>` per file (`--json`:
  one object per line), so a script can diff sent vs stored. The key is never printed.

## Plain node

Two entries, one state: the runtime entry (`/node`) carries `init`; the runtime-agnostic API is
on the root entry, and both see the same client and scope (the state lives on one `globalThis`
slot, not in a module's closure).

<!-- snippet: plain/node.mjs -->
```js
import { init } from '@sentinel/sdk-js/node';
import { captureException, flush, setUser } from '@sentinel/sdk-js';

// endpoint and apiKey default to SENTINEL_ENDPOINT / SENTINEL_API_KEY; unconfigured → a no-op client that says so once.
init({ release: '1.0.0', environment: 'production', onError: (err) => console.error('sentinel:', err.code, err.message) });
setUser({ id: 'usr_1' });

try {
  JSON.parse('{');
} catch (err) {
  captureException(err, { metadata: { job: 'nightly-report', tenant_id: 'org_a' } });
}

await flush(2000); // on your shutdown path — the SDK installs no exit hook and its timers never keep the process alive
```

`init` defaults `endpoint` / `apiKey` / `projectKey` from `SENTINEL_ENDPOINT` /
`SENTINEL_API_KEY` / `SENTINEL_PROJECT_KEY`; the global handlers of *Node crashes* are installed by
default (`installGlobalHandlers: false` to opt out).

## Plain browser

Same shape with the `/browser` entry: `tunnel` must resolve to the page's own origin (a path, or an
absolute URL on that origin — anything else is refused and `init` becomes a no-op that says so in
`onError`), the `error` / `unhandledrejection` listeners are installed by default, and a hidden
tab sends its queue with `keepalive` (`flushOnHide: false` to opt out). The tunnel is the same
`createSentinelRouteHandler` mounted on whatever serves the page — it is a plain
`(Request) => Promise<Response>` with no Next import.

<!-- snippet: plain/browser.ts -->
```ts
import { init } from '@sentinel/sdk-js/browser';
import { captureException, setUser } from '@sentinel/sdk-js';

// `tunnel` is a path on the page's own origin (the default is '/api/sentinel'); an off-origin URL is refused.
init({ tunnel: '/api/sentinel', release: '1.0.0', environment: 'production' });
setUser({ id: 'usr_1' });

document.querySelector('#save')?.addEventListener('click', () => {
  try {
    JSON.parse('{');
  } catch (err) {
    captureException(err, { metadata: { feature: 'editor' } });
  }
});
```
