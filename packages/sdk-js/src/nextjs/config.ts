// `@sentinel/sdk-js/nextjs/config` (contract §4.1, subpath `./nextjs/config`; §4.9): `withSentinelConfig`.
//
// Given the app's `next.config` — an object, a `(phase, { defaultConfig }) => config` function or an async one
// — it returns the same shape with exactly three things applied (step 7: nothing else is touched):
//   1. `productionBrowserSourceMaps: true` and `experimental.serverSourceMaps: true`, merged over the app's
//      own `experimental` (decisions 30, 18);
//   2. `compiler.runAfterProductionCompile` — Next's hook, so it runs under Turbopack and webpack alike —
//      chained after whatever hook the config already carries (the app's, or an inner wrapper's: under
//      `withSentryConfig(withSentinelConfig(…))` Sentry's proxy chains THIS hook first, decision 32);
//   3. the hook: two walks as two independent batch series through `uploadSourcemaps` (§4.8) — browser
//      (`distDir/static` under `(assetPrefix ?? '') + (basePath ?? '') + '/_next/static'`), then server
//      (`distDir/server` under `serverRoot`, default `/app/.next/server`, decision 18) — each map stored under
//      the path of the CHUNK whose `//# sourceMappingURL=` names it (BUG-01: Turbopack's map names are not its
//      chunk names; walk.ts) — then, when `deleteAfterUpload`, every `*.map` under both (the `.css.map` files
//      the walks never upload included) — and, because the hook runs BEFORE `output: 'standalone'`'s trace copy,
//      every `*.nft.json` under `distDir/server` is rewritten without the entries that named a deleted map
//      (BUG-03, the seat's decision 45: the sweep repairs the traces it invalidates, so a webpack standalone build
//      stays green and map-free; `pruneTraces` below).
//
// Failure policy (decisions 28, 29, 32, 34), all of it in the hook and nothing at import (D7):
//   - `upload` is tri-state: `false` never uploads; `undefined` uploads when `dashboardUrl` + `apiKey` resolve
//     and otherwise warns once ("not uploaded (not configured)") and still deletes; explicit `true` with either
//     unresolved THROWS before either walk;
//   - `release` empty or whitespace-padded with uploads on THROWS before either walk (never trimmed);
//   - a walk finding zero `*.js.map` with uploads on THROWS naming the walk and the flag;
//   - any 4xx from either series rejects at once — the server series is not started after a browser 4xx,
//     nothing is deleted, a stored browser series stays stored;
//   - a 5xx / network failure with the retries exhausted is one `warn` naming the series, the other series
//     still runs, every map is deleted when `deleteAfterUpload`, the build continues;
//   - `silent` hides the per-file report only; every `warn` prints through `console.warn`.
//
// The Next config type is transcribed structurally (the recognised keys of `next/dist/server/config-shared.d.ts`
// at 15.5.21) rather than imported, so the package builds with no `next` types (the same rule as
// request-error.ts); `withSentinelConfig` is generic over the shape it is given and returns that shape.

import { readFile, rm, writeFile } from 'node:fs/promises';
import { dirname, join, relative, resolve } from 'node:path';
import { isSourcemapUploadError, listMaps, listTraces, type UploadEvent, type UploadReport, uploadSourcemaps } from '../upload/index.js';

export { SDK_NAME } from '../core/index.js';
export type { BatchResult, StoredFile, UploadEvent, UploadReport, UploadSourcemapsOptions } from '../upload/index.js';

/** What Next passes `compiler.runAfterProductionCompile` (`config-shared.d.ts` at 15.5.21). */
export interface RunAfterProductionCompileContext {
  readonly projectDir: string;
  readonly distDir: string;
}

export type RunAfterProductionCompileHook = (ctx: RunAfterProductionCompileContext) => Promise<void> | void;

/** The keys of `NextConfig` this wrapper reads or writes; every other key passes through untouched. */
export interface NextConfigLike {
  assetPrefix?: string | undefined;
  basePath?: string | undefined;
  productionBrowserSourceMaps?: boolean | undefined;
  experimental?: { serverSourceMaps?: boolean | undefined } | undefined;
  compiler?: { runAfterProductionCompile?: RunAfterProductionCompileHook | undefined } | undefined;
}

/** `next.config.mjs`'s function form: `(phase, { defaultConfig }) => config`, sync or async. */
export type NextConfigFunction<C extends NextConfigLike = NextConfigLike> = (phase: string, context: { defaultConfig: C }) => C | Promise<C>;

export interface SentinelSourcemapsOptions {
  /** Tri-state (decision 32): `false` never uploads; `undefined` uploads when configured; `true` fails the build when not. */
  upload?: boolean | undefined;
  /** Default `process.env.SENTINEL_DASHBOARD_URL`. */
  dashboardUrl?: string | undefined;
  /** Default `process.env.SENTINEL_API_KEY`. */
  apiKey?: string | undefined;
  /** Default `process.env.SENTINEL_PROJECT_KEY`. */
  projectKey?: string | undefined;
  /** Default `true`; governs EVERY path — `false` never deletes, uploaded or not (§4.9 step 4). */
  deleteAfterUpload?: boolean | undefined;
  /** Default `(assetPrefix ?? '') + (basePath ?? '') + '/_next/static'`, slashes normalised. */
  publicPrefix?: string | undefined;
  /** Default `/app/.next/server` — the container's runtime path of `.next/server` (decision 18). */
  serverRoot?: string | undefined;
}

export interface WithSentinelConfigOptions {
  /** Default `process.env.NEXT_PUBLIC_APP_VERSION`; must equal `init.release` and be unique per build when uploads are on (decision 28). */
  release?: string | undefined;
  sourcemaps?: SentinelSourcemapsOptions | undefined;
  /** Hides the per-file report only; every `warn` still prints. */
  silent?: boolean | undefined;
}

export const DEFAULT_SERVER_ROOT = '/app/.next/server';
export const DEFAULT_PUBLIC_PREFIX = '/_next/static';

type Series = 'browser' | 'server';

function stripTrailingSlashes(value: string | undefined): string {
  return (value ?? '').replace(/\/+$/, '');
}

/** `(assetPrefix ?? '') + (basePath ?? '') + '/_next/static'` with a trailing `/` on either stripped — never `//_next` (W6). */
export function defaultPublicPrefix(config: Pick<NextConfigLike, 'assetPrefix' | 'basePath'>): string {
  return `${stripTrailingSlashes(config.assetPrefix)}${stripTrailingSlashes(config.basePath)}${DEFAULT_PUBLIC_PREFIX}`;
}

function warn(line: string): void {
  console.warn(`Sentinel: ${line}`);
}

function env(name: string): string | undefined {
  // Guarded: the module body never touches `process` (D7); the hook runs under `next build`, where it exists.
  const value = typeof process !== 'undefined' ? process.env?.[name] : undefined;
  return typeof value === 'string' ? value : undefined;
}

function isThenable<T>(value: T | Promise<T>): value is Promise<T> {
  return typeof (value as { then?: unknown } | null)?.then === 'function';
}

/** Every `*.map` under `dirs`, of any kind — the sweep of §4.9 step 4. Missing directories are skipped. */
async function deleteEveryMap(dirs: readonly string[]): Promise<string[]> {
  const deleted: string[] = [];
  for (const dir of dirs) {
    for (const path of await listMaps(dir)) {
      await rm(path, { force: true });
      deleted.push(path);
    }
  }
  return deleted;
}

/** The shape of a `*.nft.json` (`@vercel/nft` version 1): entries relative to the trace file's own directory. */
interface TraceFile {
  version?: unknown;
  files?: unknown;
}

/** One rewritten trace: the file and the entries dropped from it. */
export interface PrunedTrace {
  readonly path: string;
  readonly removed: readonly string[];
}

/**
 * BUG-03 (the seat's decision 45): under webpack, `next-trace-entrypoints-plugin` wrote every server entry's
 * `<entry>.js.nft.json` at compile time, and with `experimental.serverSourceMaps` each one lists the `.js.map`
 * beside every chunk the entry needs (`instrumentation.js.nft.json` →
 * `["../package.json","chunks/260.js","chunks/260.js.map",…]`). `next build`'s *Collecting build traces* and the
 * `output: 'standalone'` copy (`copyTracedFiles`) run AFTER this hook and copy every listed file — one that the
 * sweep deleted is `⚠ Failed to copy traced files` per page and a hard ENOENT on `instrumentation.js.map`, exit 1,
 * no `.next/standalone/server.js`. So the sweep repairs what it invalidated: every `*.nft.json` under
 * `distDir/server` is rewritten without the entries that resolve to a path THIS run deleted — never a generic
 * `*.map` filter — and nothing else in the file changes: key order, the remaining entries' order and the file's
 * indentation are kept; a file that is not JSON, not an object or has no `files` array is left alone (the copy then
 * reports it itself). Turbopack's traces never list a map, so there the rewrite is a no-op. Next's own
 * `next-server.js.nft.json` (written later, under `distDir`) ignores `**\/*.map` on its own.
 */
export async function pruneTraces(serverDir: string, deleted: ReadonlySet<string>): Promise<PrunedTrace[]> {
  const pruned: PrunedTrace[] = [];
  if (deleted.size === 0) return pruned;
  for (const path of await listTraces(serverDir)) {
    let text: string;
    let trace: TraceFile;
    try {
      text = await readFile(path, 'utf8');
      trace = JSON.parse(text) as TraceFile;
    } catch {
      continue;
    }
    if (trace === null || typeof trace !== 'object' || !Array.isArray(trace.files)) continue;
    const dir = dirname(path);
    const removed: string[] = [];
    const kept = trace.files.filter((entry) => {
      if (typeof entry !== 'string' || !deleted.has(resolve(dir, entry))) return true;
      removed.push(entry);
      return false;
    });
    if (removed.length === 0) continue;
    await writeFile(path, JSON.stringify({ ...trace, files: kept }, null, detectIndent(text)) + (text.endsWith('\n') ? '\n' : ''));
    pruned.push({ path, removed });
  }
  return pruned;
}

/** The indentation a JSON text was written with (Next writes its traces compact), so the rewrite keeps it. */
function detectIndent(text: string): string {
  return /^\s*\{\r?\n([ \t]+)"/.exec(text)?.[1] ?? '';
}

/** Step 4 in full: the sweep over both series' directories, then the trace repair it makes necessary, one line per rewritten trace. */
async function sweep(distDir: string, silent: boolean): Promise<void> {
  const serverDir = join(distDir, 'server');
  const deleted = await deleteEveryMap([join(distDir, 'static'), serverDir]);
  for (const { path, removed } of await pruneTraces(serverDir, new Set(deleted))) {
    if (!silent) console.log(`Sentinel: build trace ${relative(distDir, path)} — ${removed.length} deleted map entr${removed.length === 1 ? 'y' : 'ies'} removed`);
  }
}

interface SeriesPlan {
  readonly series: Series;
  readonly buildDir: string;
  readonly publicPrefix: string;
  /** The Next option that makes the walk non-empty — named by the W13 throw. */
  readonly flag: string;
}

/** The hook of step 3: resolves everything at run time, then the three loud checks, the two series, the sweep. */
async function runSentinelHook(config: NextConfigLike, options: WithSentinelConfigOptions, ctx: RunAfterProductionCompileContext): Promise<void> {
  const sm = options.sourcemaps ?? {};
  const release = options.release ?? env('NEXT_PUBLIC_APP_VERSION') ?? '';
  const dashboardUrl = sm.dashboardUrl ?? env('SENTINEL_DASHBOARD_URL');
  const apiKey = sm.apiKey ?? env('SENTINEL_API_KEY');
  const projectKey = sm.projectKey ?? env('SENTINEL_PROJECT_KEY');
  const deleteAfterUpload = sm.deleteAfterUpload ?? true;
  const silent = options.silent === true;
  const distDir = resolve(ctx.projectDir, ctx.distDir);
  const staticDir = join(distDir, 'static');
  const serverDir = join(distDir, 'server');
  const configured = typeof dashboardUrl === 'string' && dashboardUrl !== '' && typeof apiKey === 'string' && apiKey !== '';

  // The tri-state `upload` (decision 32).
  let uploads: boolean;
  let quietReason: string;
  if (sm.upload === false) {
    uploads = false;
    quietReason = 'upload is false';
  } else if (sm.upload === true) {
    if (!configured) {
      throw new Error(
        'Sentinel: `sourcemaps.upload` is true but `dashboardUrl` / `apiKey` is not set — set `SENTINEL_DASHBOARD_URL` / `SENTINEL_API_KEY`, or leave `upload` undefined',
      );
    }
    uploads = true;
    quietReason = '';
  } else {
    uploads = configured;
    quietReason = 'not configured';
  }

  if (!uploads) {
    if (deleteAfterUpload) {
      warn(`sourcemaps not uploaded (${quietReason}); deleting them anyway`);
      await sweep(distDir, silent);
    } else {
      warn(`sourcemaps not uploaded (${quietReason}); \`deleteAfterUpload\` is false, so the maps stay in place`);
    }
    return;
  }

  // The release refusals (decisions 28, 32) — before either walk; never trimmed.
  if (release === '') {
    throw new Error('Sentinel: `release` is empty and sourcemap upload is on — set `release` / `NEXT_PUBLIC_APP_VERSION`, or `SENTINEL_UPLOAD_SOURCEMAPS=false`');
  }
  if (release !== release.trim()) {
    throw new Error(
      `Sentinel: \`release\` ${JSON.stringify(release)} has leading or trailing whitespace and sourcemap upload is on — the \`$(…)\` newline is the usual cause; ` +
        'set `release` / `NEXT_PUBLIC_APP_VERSION` to the exact string (it is never trimmed), or `SENTINEL_UPLOAD_SOURCEMAPS=false`',
    );
  }

  const plans: SeriesPlan[] = [
    { series: 'browser', buildDir: staticDir, publicPrefix: sm.publicPrefix ?? defaultPublicPrefix(config), flag: 'productionBrowserSourceMaps' },
    { series: 'server', buildDir: serverDir, publicPrefix: sm.serverRoot ?? DEFAULT_SERVER_ROOT, flag: 'experimental.serverSourceMaps' },
  ];
  // Two independent series (decision 34): a warn ends one, a 4xx ends the hook.
  for (const plan of plans) {
    await runSeries(plan, { release, dashboardUrl: dashboardUrl as string, apiKey: apiKey as string, projectKey, silent, deleteAfterUpload });
  }

  if (deleteAfterUpload) await sweep(distDir, silent);
}

interface SeriesContext {
  readonly release: string;
  readonly dashboardUrl: string;
  readonly apiKey: string;
  readonly projectKey: string | undefined;
  readonly silent: boolean;
  readonly deleteAfterUpload: boolean;
}

async function runSeries(plan: SeriesPlan, ctx: SeriesContext): Promise<UploadReport | undefined> {
  const { series } = plan;
  const onEvent = (event: UploadEvent): void => {
    switch (event.type) {
      case 'warn':
        warn(`${series} sourcemaps: ${event.line}`);
        return;
      case 'retry':
        warn(`${series} sourcemap upload attempt ${event.attempt} failed (${event.line}); retrying in ${event.delayMs / 1000} s`);
        return;
      case 'batch':
        if (ctx.silent) return;
        for (const file of event.batch.files) console.log(`Sentinel: ${series}  ${file.outcome}  ${file.minified_path}  ${file.sha256.slice(0, 12)}  ${file.size_bytes}`);
        return;
    }
  };
  try {
    const report = await uploadSourcemaps({
      buildDir: plan.buildDir,
      publicPrefix: plan.publicPrefix,
      release: ctx.release,
      dashboardUrl: ctx.dashboardUrl,
      apiKey: ctx.apiKey,
      projectKey: ctx.projectKey,
      onEvent,
    });
    if (!ctx.silent) console.log(`Sentinel: ${series} sourcemaps — ${report.files.length} file(s) stored in ${report.requests} request(s) under release ${report.release}`);
    return report;
  } catch (err) {
    if (!isSourcemapUploadError(err)) throw err;
    if (err.kind === 'no_maps') {
      throw new Error(
        `Sentinel: no \`*.js.map\` under ${plan.buildDir} (the ${series} walk) — \`${plan.flag}\` did not take effect; is another wrapper overriding it, or deleting the maps before this hook?`,
        { cause: err },
      );
    }
    if (err.kind === 'unavailable') {
      // Decision 34: printed, never branched on — the status or the envelope's code, or the network error.
      const what = err.status === undefined ? err.message.replace(/ after \d+ attempts$/, '') : `${err.status}${err.code ? ` ${err.code}` : ''}`;
      const tail = ctx.deleteAfterUpload ? 'deleting maps anyway' : '`deleteAfterUpload` is false, so the maps stay in place';
      warn(`${series} sourcemap upload failed (${what}) after ${err.attempts ?? 0} attempts — ${tail}; release ${ctx.release} will not symbolicate`);
      return undefined;
    }
    // `rejected` (any 4xx) and `usage`: loud, at once, nothing deleted (decision 29).
    throw new Error(`Sentinel: ${series} sourcemap upload ${err.kind === 'rejected' ? 'rejected' : 'refused'} — ${err.message}`, { cause: err });
  }
}

/** Steps 1, 2 and 7 on one resolved config object: a shallow copy with the three keys applied and nothing else touched. */
function applyToConfig<C extends NextConfigLike>(config: C, options: WithSentinelConfigOptions): C {
  const existing = config.compiler?.runAfterProductionCompile;
  const hook: RunAfterProductionCompileHook = async (ctx) => {
    if (typeof existing === 'function') await existing(ctx);
    await runSentinelHook(config, options, ctx);
  };
  return {
    ...config,
    productionBrowserSourceMaps: true,
    experimental: { ...config.experimental, serverSourceMaps: true },
    compiler: { ...config.compiler, runAfterProductionCompile: hook },
  };
}

export function withSentinelConfig<F extends NextConfigFunction>(nextConfig: F, options?: WithSentinelConfigOptions): F;
export function withSentinelConfig<C extends NextConfigLike>(nextConfig: C, options?: WithSentinelConfigOptions): C;
export function withSentinelConfig(nextConfig: NextConfigLike | NextConfigFunction, options: WithSentinelConfigOptions = {}): NextConfigLike | NextConfigFunction {
  if (typeof nextConfig === 'function') {
    const wrapped: NextConfigFunction = (phase, context) => {
      const result = nextConfig(phase, context);
      return isThenable(result) ? result.then((resolved) => applyToConfig(resolved, options)) : applyToConfig(result, options);
    };
    return wrapped;
  }
  return applyToConfig(nextConfig, options);
}
