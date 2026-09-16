// `withSentinelConfig` (contract §4.9; §11 *`withSentinelConfig`*; flows W3–W8, W10–W15 — the unit halves) with
// the uploader stubbed at the `uploadSourcemaps` boundary: the stub walks the real fixture tree through the real
// walker (so an empty walk is the real `no_maps`, W13) and answers per a script — stored, a 4xx `rejected`, or an
// exhausted 5xx / network `unavailable` — while recording every call. The tree is a temp `distDir` with
// `.js.map`, `.mjs.map` and `.css.map` under `static/` and `.js.map` under `server/`. Messages are asserted by
// their named tokens (release, the walk's directory, the series name, the status or code), never by sentence
// (contract §11's guidance rule). Shuffle-safe: every test builds its own tree, script and env.

import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { UploadEvent, UploadReport, UploadSourcemapsOptions } from '../../src/upload/index.js';
import { cleanupTrees, copyTree, makeTree } from '../cli/helpers.js';

type Answer = 'ok' | { rejected: { status: number; code: string } } | { unavailable: { status?: number; code?: string; text?: string; network?: string } };
interface Script {
  browser: Answer;
  server: Answer;
}

const calls: UploadSourcemapsOptions[] = [];
/** Per-call pair counts, so a test can tell "called on an empty walk" from "called with maps". */
const walked: number[] = [];
let script: Script = { browser: 'ok', server: 'ok' };

vi.mock('../../src/upload/index.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../src/upload/index.js')>();
  const uploadSourcemaps = async (opts: UploadSourcemapsOptions): Promise<UploadReport> => {
    calls.push(opts);
    actual.validateRelease(opts.release);
    const pairs = await actual.walkSourcemaps(opts.buildDir, opts.publicPrefix, { onWarn: (line) => opts.onEvent?.({ type: 'warn', line }) });
    walked.push(pairs.length);
    if (pairs.length === 0) throw new actual.SourcemapUploadError('no_maps', `no *.js.map under ${opts.buildDir}`);
    const answer = opts.buildDir.endsWith('server') ? script.server : script.browser;
    if (answer !== 'ok' && 'rejected' in answer) {
      const { status, code } = answer.rejected;
      throw new actual.SourcemapUploadError('rejected', `${status} ${code}: refused {}`, { status, code, attempts: 1 });
    }
    if (answer !== 'ok' && 'unavailable' in answer) {
      const u = answer.unavailable;
      const line = u.network ? `connection failed (${u.network})` : u.code ? `${u.status} ${u.code}: down {}` : `${u.status} ${u.text ?? ''}`.trimEnd();
      for (let attempt = 1; attempt < 4; attempt++) opts.onEvent?.({ type: 'retry', attempt, delayMs: 2000 * 2 ** (attempt - 1), status: u.status, line });
      throw new actual.SourcemapUploadError('unavailable', `${line} after 4 attempts`, { status: u.status, code: u.code, text: u.text, attempts: 4 });
    }
    const files = pairs.map((p, i) => ({ minified_path: p.minifiedPath, sha256: `${String(i).padStart(4, '0')}${'cd'.repeat(30)}`, size_bytes: p.sizeBytes, outcome: 'created' }));
    const batch = { index: 0, status: 201, attempts: 1, contentLength: 0, sent: pairs, files, deleted: [] };
    opts.onEvent?.({ type: 'batch', batch } satisfies UploadEvent);
    return { dryRun: false, release: opts.release, pairs, requests: 1, batches: [batch], files, deleted: [] };
  };
  return { ...actual, uploadSourcemaps };
});

// Imported after the mock is registered (vitest hoists `vi.mock`, but the import must be in this module).
const { DEFAULT_SERVER_ROOT, defaultPublicPrefix, pruneTraces, withSentinelConfig } = await import('../../src/nextjs/config.js');
type NextConfigLike = import('../../src/nextjs/config.js').NextConfigLike;
type Hook = NonNullable<NonNullable<NextConfigLike['compiler']>['runAfterProductionCompile']>;

const TURBOPACK = resolve(dirname(fileURLToPath(import.meta.url)), '../fixtures-build/turbopack/.next');

const ENV_KEYS = ['NEXT_PUBLIC_APP_VERSION', 'SENTINEL_DASHBOARD_URL', 'SENTINEL_API_KEY', 'SENTINEL_PROJECT_KEY', 'SENTINEL_UPLOAD_SOURCEMAPS'] as const;
const savedEnv: Partial<Record<(typeof ENV_KEYS)[number], string | undefined>> = {};
const CREDS = { dashboardUrl: 'https://sentinel.example', apiKey: 'sk_wrapper_never_printed' };
const RELEASE = '2.34.0';

let warns: string[];
let logs: string[];

beforeEach(() => {
  calls.length = 0;
  walked.length = 0;
  script = { browser: 'ok', server: 'ok' };
  for (const k of ENV_KEYS) {
    savedEnv[k] = process.env[k];
    delete process.env[k];
  }
  warns = [];
  logs = [];
  vi.spyOn(console, 'warn').mockImplementation((...args: unknown[]) => void warns.push(args.map(String).join(' ')));
  vi.spyOn(console, 'log').mockImplementation((...args: unknown[]) => void logs.push(args.map(String).join(' ')));
});

afterEach(() => {
  vi.restoreAllMocks();
  for (const k of ENV_KEYS) {
    if (savedEnv[k] === undefined) delete process.env[k];
    else process.env[k] = savedEnv[k];
  }
  cleanupTrees();
});

/** A `distDir` with two `.js.map` + one `.mjs.map` + one `.css.map` under static/ and two `.js.map` under server/. */
function tree(): string {
  return makeTree({ count: 2, css: true, server: 2, extra: ['static/chunks/esm-1.mjs'] });
}

function allMaps(dir: string): { static: string[]; server: string[]; css: string[] } {
  const s = ['static/chunks/chunk-000.js.map', 'static/chunks/chunk-001.js.map', 'static/chunks/esm-1.mjs.map'].map((p) => join(dir, p)).filter(existsSync);
  const css = [join(dir, 'static/css/styles.css.map')].filter(existsSync);
  const server = ['server/app/route-000.js.map', 'server/app/route-001.js.map'].map((p) => join(dir, p)).filter(existsSync);
  return { static: s, server, css };
}

function hookOf(config: NextConfigLike): Hook {
  const hook = config.compiler?.runAfterProductionCompile;
  if (typeof hook !== 'function') throw new Error('no hook installed');
  return hook;
}

async function runHook(config: NextConfigLike, distDir: string): Promise<void> {
  await hookOf(config)({ projectDir: join(distDir, '..'), distDir });
}

describe('W1 (BUG-01, Turbopack shape): every stored browser path is a chunk the browser loads, never the map’s own differently-hashed name', () => {
  it('W1: the browser series reports `/_next/static/chunks/d69eb7d85499027c.js` for the map named `8f15d5d03090a0be.js.map`; the edge-wrapper and percent-encoded server pairs land under serverRoot; the skips are warns naming the series; every *.map is then deleted', async () => {
    const dir = copyTree(TURBOPACK);
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    await runHook(out, dir);
    const browser = logs.filter((l) => l.startsWith('Sentinel: browser  created  ')).map((l) => l.split(/\s{2,}/)[2]);
    expect(browser).toEqual(['/_next/static/chunks/9a8b7c6d5e4f3a2b.js', '/_next/static/chunks/d69eb7d85499027c.js', '/_next/static/chunks/turbopack-4364a8ae2a6af640.js']);
    expect(logs.join('\n')).not.toMatch(/\/_next\/static\/chunks\/(8f15d5d03090a0be|e96569a392fa278c)\.js/);
    const server = logs.filter((l) => l.startsWith('Sentinel: server  created  ')).map((l) => l.split(/\s{2,}/)[2]);
    expect(server).toEqual([
      `${DEFAULT_SERVER_ROOT}/chunks/[root-of-the-server]__a6734ae3._.js`,
      `${DEFAULT_SERVER_ROOT}/chunks/_0fe1378c._.js`,
      `${DEFAULT_SERVER_ROOT}/chunks/f9941_@sentinel_sdk-js_dist_nextjs_edge_d8f68d22.js`,
      `${DEFAULT_SERVER_ROOT}/edge/chunks/edge-wrapper_8937e6f3.js`,
      `${DEFAULT_SERVER_ROOT}/edge/chunks/turbopack-edge-wrapper_56e5ba59.js`,
    ]);
    // The walker's skip lines, through the hook's warn with the series named (the orphan's "beside it" line is the real uploader's, tested in tests/cli).
    expect(warns).toEqual([
      expect.stringMatching(/^Sentinel: browser sourcemaps: .*60394cfab7bcbc98\.js: its sourceMappingURL names 4c218c652b8c48af\.js\.map, which does not exist/),
      expect.stringMatching(/^Sentinel: browser sourcemaps: 1 script\(s\) under .* carry no sourceMappingURL comment .*3f9721961e8f2ba6\.js/),
      expect.stringMatching(/^Sentinel: server sourcemaps: 1 script\(s\) under .* carry no sourceMappingURL comment .*page_client-reference-manifest\.js/),
      // BUG-02 (decision 44): the entry stub's empty index map and the runtime's sections map are skipped, never posted, and the sweep below still deletes them.
      expect.stringMatching(/^Sentinel: server sourcemaps: .*server\/app\/page\.js\.map: empty index map \(no mappings, no sections\) — nothing to symbolicate; skipping it$/),
      expect.stringMatching(/^Sentinel: server sourcemaps: .*server\/chunks\/\[turbopack\]_runtime\.js\.map: index map \(sections, no mappings\) — the route stores flat maps only; skipping it$/),
    ]);
    expect(server).not.toContain(`${DEFAULT_SERVER_ROOT}/app/page.js`);
    expect(server).not.toContain(`${DEFAULT_SERVER_ROOT}/chunks/[turbopack]_runtime.js`);
    for (const rel of ['static/chunks/8f15d5d03090a0be.js.map', 'static/chunks/e96569a392fa278c.js.map', 'static/chunks/9a8b7c6d5e4f3a2b.js.map', 'static/chunks/20c20fcd5d02c4c2.css.map', 'server/edge/chunks/edge-wrapper_56e5ba59.js.map', 'server/chunks/[root-of-the-server]__a6734ae3._.js.map', 'server/app/page.js.map', 'server/chunks/[turbopack]_runtime.js.map']) {
      expect(existsSync(join(dir, rel)), rel).toBe(false);
    }
    expect(existsSync(join(dir, 'static/chunks/d69eb7d85499027c.js'))).toBe(true);
  });
});

describe('W7: the three input shapes yield one resolved config and return their own shape', () => {
  const input: NextConfigLike & { reactStrictMode: boolean; experimental: { ppr: boolean; serverSourceMaps?: boolean } } = {
    reactStrictMode: true,
    basePath: '',
    experimental: { ppr: true },
    compiler: {},
  };
  const expectResolved = (out: NextConfigLike): void => {
    expect(out.productionBrowserSourceMaps).toBe(true);
    expect(out.experimental).toEqual({ ppr: true, serverSourceMaps: true });
    expect((out as { reactStrictMode?: boolean }).reactStrictMode).toBe(true);
    expect(typeof out.compiler?.runAfterProductionCompile).toBe('function');
    expect(Object.keys(out).sort()).toEqual(['basePath', 'compiler', 'experimental', 'productionBrowserSourceMaps', 'reactStrictMode']);
  };

  it('W7: an object in → an object out, the input untouched', () => {
    const out = withSentinelConfig(input, { release: RELEASE });
    expect(typeof out).toBe('object');
    expectResolved(out);
    expect(input.productionBrowserSourceMaps).toBeUndefined();
    expect(input.experimental.serverSourceMaps).toBeUndefined();
    expect(input.compiler?.runAfterProductionCompile).toBeUndefined();
  });

  it('W7: a `(phase, { defaultConfig }) => config` function in → a function of the same signature out that calls the app’s', () => {
    const seen: unknown[] = [];
    const fn = (phase: string, ctx: { defaultConfig: NextConfigLike }): NextConfigLike => {
      seen.push(phase, ctx);
      return { ...input };
    };
    const out = withSentinelConfig(fn, { release: RELEASE });
    expect(typeof out).toBe('function');
    const ctx = { defaultConfig: {} };
    const resolved = out('phase-production-build', ctx);
    expect(resolved).not.toBeInstanceOf(Promise);
    expectResolved(resolved as NextConfigLike);
    expect(seen).toEqual(['phase-production-build', ctx]);
  });

  it('W7: an async function in → a function out whose result is awaited', async () => {
    const fn = async (_phase: string, _ctx: { defaultConfig: NextConfigLike }): Promise<NextConfigLike> => ({ ...input });
    const out = withSentinelConfig(fn, { release: RELEASE });
    expect(typeof out).toBe('function');
    const pending = out('phase-production-build', { defaultConfig: {} });
    expect(pending).toBeInstanceOf(Promise);
    expectResolved(await pending);
  });

  it('W7: the hook calls the uploader twice — distDir/static with publicPrefix, then distDir/server with serverRoot (default /app/.next/server) — with the release, credentials and project key', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: { ...CREDS, projectKey: 'org-1' } });
    await runHook(out, dir);
    expect(calls).toHaveLength(2);
    expect(calls[0]).toMatchObject({ buildDir: join(dir, 'static'), publicPrefix: '/_next/static', release: RELEASE, ...CREDS, projectKey: 'org-1' });
    expect(calls[1]).toMatchObject({ buildDir: join(dir, 'server'), publicPrefix: DEFAULT_SERVER_ROOT, release: RELEASE, ...CREDS, projectKey: 'org-1' });
    expect(DEFAULT_SERVER_ROOT).toBe('/app/.next/server');
    expect(walked).toEqual([3, 2]);
  });

  it('W7: `serverRoot` and `publicPrefix` options override both defaults', async () => {
    const dir = tree();
    const out = withSentinelConfig({ assetPrefix: 'https://cdn.daya.ai' }, { release: RELEASE, sourcemaps: { ...CREDS, publicPrefix: '/static', serverRoot: '/srv/app/.next/server' } });
    await runHook(out, dir);
    expect(calls.map((c) => c.publicPrefix)).toEqual(['/static', '/srv/app/.next/server']);
  });

  it('the env fallbacks — NEXT_PUBLIC_APP_VERSION, SENTINEL_DASHBOARD_URL, SENTINEL_API_KEY, SENTINEL_PROJECT_KEY — are read in the hook, not at import', async () => {
    const dir = tree();
    const out = withSentinelConfig({});
    process.env.NEXT_PUBLIC_APP_VERSION = '3.0.0+abc123';
    process.env.SENTINEL_DASHBOARD_URL = 'https://env.example';
    process.env.SENTINEL_API_KEY = 'sk_env';
    process.env.SENTINEL_PROJECT_KEY = 'org-env';
    await runHook(out, dir);
    expect(calls[0]).toMatchObject({ release: '3.0.0+abc123', dashboardUrl: 'https://env.example', apiKey: 'sk_env', projectKey: 'org-env' });
  });
});

describe('W5: an existing compiler.runAfterProductionCompile is chained — the app’s runs first', () => {
  it('W5: order is app hook, then the two walks', async () => {
    const dir = tree();
    const order: string[] = [];
    const out = withSentinelConfig(
      {
        compiler: {
          runAfterProductionCompile: async (ctx) => {
            // `calls.length` at this moment is how many walks ran BEFORE the app's hook — it must be none.
            order.push(`app:${ctx.distDir}:walks-before=${calls.length}`);
          },
        },
      },
      { release: RELEASE, sourcemaps: CREDS },
    );
    await runHook(out, dir);
    expect(order).toEqual([`app:${dir}:walks-before=0`]);
    expect(calls).toHaveLength(2);
  });
});

describe('W6: the publicPrefix default — (assetPrefix ?? "") + (basePath ?? "") + "/_next/static", slashes normalised', () => {
  it.each([
    ['assetPrefix https://cdn.daya.ai', { assetPrefix: 'https://cdn.daya.ai' }, 'https://cdn.daya.ai/_next/static'],
    ['assetPrefix with a trailing slash', { assetPrefix: 'https://cdn.daya.ai/' }, 'https://cdn.daya.ai/_next/static'],
    ['basePath /app', { basePath: '/app' }, '/app/_next/static'],
    ['both', { assetPrefix: 'https://cdn.daya.ai', basePath: '/app' }, 'https://cdn.daya.ai/app/_next/static'],
    ['neither', {}, '/_next/static'],
  ] as Array<[string, NextConfigLike, string]>)('W6: %s → %s', async (_name, config, expected) => {
    expect(defaultPublicPrefix(config)).toBe(expected);
    const dir = tree();
    const out = withSentinelConfig(config, { release: RELEASE, sourcemaps: CREDS });
    await runHook(out, dir);
    expect(calls[0]?.publicPrefix).toBe(expected);
    expect(calls[0]?.publicPrefix).not.toContain('//_next');
  });

  it('W6: a trailing slash on basePath and an assetPrefix of "/" never produce "//_next"', () => {
    expect(defaultPublicPrefix({ basePath: '/app/' })).toBe('/app/_next/static');
    expect(defaultPublicPrefix({ assetPrefix: '/' })).toBe('/_next/static');
    expect(defaultPublicPrefix({ assetPrefix: 'https://cdn.daya.ai//', basePath: '/app//' })).toBe('https://cdn.daya.ai/app/_next/static');
  });
});

describe('W1 / W4 unit halves: the success path deletes every *.map; a 4xx in either series rejects and keeps both kinds', () => {
  it('W1: after both series every .js.map, .mjs.map and .css.map under static/ and server/ is gone; the per-file report and a summary per series are printed', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    await runHook(out, dir);
    expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
    expect(existsSync(join(dir, 'static/chunks/chunk-000.js'))).toBe(true);
    expect(logs.filter((l) => /^Sentinel: browser  created  /.test(l))).toHaveLength(3);
    expect(logs.filter((l) => /^Sentinel: server  created  /.test(l))).toHaveLength(2);
    expect(logs.some((l) => l.includes('browser sourcemaps') && l.includes(RELEASE))).toBe(true);
    expect(logs.some((l) => l.includes('server sourcemaps') && l.includes(RELEASE))).toBe(true);
    expect(warns).toEqual([]);
  });

  it('W4: a 401 in the browser series rejects at once with the status and code, the server series is not started, nothing is deleted', async () => {
    const dir = tree();
    script = { browser: { rejected: { status: 401, code: 'unauthorized' } }, server: 'ok' };
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    await expect(runHook(out, dir)).rejects.toThrow(/browser.*401.*unauthorized/);
    expect(calls).toHaveLength(1);
    expect(allMaps(dir)).toEqual({ static: expect.arrayContaining([expect.any(String)]), server: expect.arrayContaining([expect.any(String)]), css: [expect.any(String)] });
    expect(allMaps(dir).static).toHaveLength(3);
    expect(allMaps(dir).server).toHaveLength(2);
  });

  it('W4: a 422 release_full in the server series rejects after the browser series stored; nothing is deleted', async () => {
    const dir = tree();
    script = { browser: 'ok', server: { rejected: { status: 422, code: 'release_full' } } };
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    await expect(runHook(out, dir)).rejects.toThrow(/server.*422.*release_full/);
    expect(calls).toHaveLength(2);
    expect(allMaps(dir).static).toHaveLength(3);
    expect(allMaps(dir).server).toHaveLength(2);
    expect(allMaps(dir).css).toHaveLength(1);
  });
});

describe('W10 / W14: exhausted 5xx or network failure — one warn per series naming it, the other series still runs, maps deleted, the build continues (decisions 29, 34)', () => {
  it('W14: browser 503 → one warn naming `browser`, 503 and the release; the server series runs and is reported; every map deleted; no rejection', async () => {
    const dir = tree();
    script = { browser: { unavailable: { status: 503, code: 'storage_unavailable' } }, server: 'ok' };
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    await expect(runHook(out, dir)).resolves.toBeUndefined();
    const failed = warns.filter((w) => /upload failed/.test(w));
    expect(failed).toHaveLength(1);
    expect(failed[0]).toMatch(/browser/);
    expect(failed[0]).toMatch(/503/);
    expect(failed[0]).toMatch(/storage_unavailable/);
    expect(failed[0]).toMatch(/4 attempts/);
    expect(failed[0]).toContain(RELEASE);
    expect(failed[0]).toMatch(/deleting maps anyway/);
    expect(calls).toHaveLength(2);
    expect(logs.filter((l) => /^Sentinel: server  created  /.test(l))).toHaveLength(2);
    expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
  });

  it('W14 mirror: server 503 → the warn names `server`; browser was stored; every map deleted', async () => {
    const dir = tree();
    script = { browser: 'ok', server: { unavailable: { status: 503, code: 'storage_unavailable' } } };
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    await runHook(out, dir);
    const failed = warns.filter((w) => /upload failed/.test(w));
    expect(failed).toHaveLength(1);
    expect(failed[0]).toMatch(/server sourcemap upload failed/);
    expect(logs.filter((l) => /^Sentinel: browser  created  /.test(l))).toHaveLength(3);
    expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
  });

  it('W10: both series fail — a JSON 500, a plain-text 502, a refused connection — two warns each naming its series; the status or error printed, never branched on', async () => {
    for (const [b, s] of [
      [{ status: 500, code: 'internal' }, { status: 502, text: 'Bad Gateway' }],
      [{ status: 503, text: 'Service Unavailable' }, { network: 'ECONNREFUSED' }],
    ] as Array<[Extract<Answer, { unavailable: unknown }>['unavailable'], Extract<Answer, { unavailable: unknown }>['unavailable']]>) {
      const dir = tree();
      script = { browser: { unavailable: b }, server: { unavailable: s } };
      warns = [];
      const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
      await runHook(out, dir);
      const failed = warns.filter((w) => /upload failed/.test(w));
      expect(failed).toHaveLength(2);
      expect(failed[0]).toMatch(/browser/);
      expect(failed[1]).toMatch(/server/);
      expect(failed[0]).toContain(b.network ?? String(b.status));
      expect(failed[1]).toContain(s.network ?? String(s.status));
      for (const w of failed) expect(w).toContain(RELEASE);
      expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
    }
  });

  it('W10: `silent: true` hides the per-file report and never a warn', async () => {
    const dir = tree();
    script = { browser: { unavailable: { status: 503, code: 'storage_unavailable' } }, server: 'ok' };
    const out = withSentinelConfig({}, { release: RELEASE, silent: true, sourcemaps: CREDS });
    await runHook(out, dir);
    expect(logs).toEqual([]);
    expect(warns.filter((w) => /upload failed/.test(w))).toHaveLength(1);
    expect(warns.some((w) => /browser/.test(w) && /attempt 1/.test(w))).toBe(true);
  });
});

describe('W3 / W12: the tri-state `upload` (decision 32)', () => {
  it('W12: explicit `upload: true` with no credentials throws before either walk naming `sourcemaps.upload`, `dashboardUrl` / `apiKey` and the env names; maps left in place', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: { upload: true } });
    await expect(runHook(out, dir)).rejects.toThrow(/sourcemaps\.upload.*dashboardUrl.*apiKey.*SENTINEL_DASHBOARD_URL.*SENTINEL_API_KEY/);
    expect(calls).toHaveLength(0);
    expect(allMaps(dir).static).toHaveLength(3);
    expect(allMaps(dir).server).toHaveLength(2);
    expect(allMaps(dir).css).toHaveLength(1);
  });

  it('W12: explicit `upload: true` with only one of the two set throws the same way', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: { upload: true, apiKey: 'sk_only' } });
    await expect(runHook(out, dir)).rejects.toThrow(/sourcemaps\.upload/);
    expect(calls).toHaveLength(0);
  });

  it('W3: `upload` undefined with no credentials → one warn "not uploaded (not configured); deleting them anyway" through console.warn even under `silent`, every map deleted, no throw, nothing walked', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { release: RELEASE, silent: true, sourcemaps: { upload: undefined } });
    await expect(runHook(out, dir)).resolves.toBeUndefined();
    expect(calls).toHaveLength(0);
    expect(warns).toHaveLength(1);
    expect(warns[0]).toMatch(/not uploaded/);
    expect(warns[0]).toMatch(/not configured/);
    expect(warns[0]).toMatch(/deleting them anyway/);
    expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
    expect(logs).toEqual([]);
  });

  it('W3: `upload: false` (the SENTINEL_UPLOAD_SOURCEMAPS=false line) never uploads even with credentials, one warn, every map deleted', async () => {
    const dir = tree();
    process.env.SENTINEL_UPLOAD_SOURCEMAPS = 'false';
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: { ...CREDS, upload: process.env.SENTINEL_UPLOAD_SOURCEMAPS === 'false' ? false : undefined } });
    await runHook(out, dir);
    expect(calls).toHaveLength(0);
    expect(warns).toHaveLength(1);
    expect(warns[0]).toMatch(/not uploaded/);
    expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
  });
});

describe('W11: the release refusals before either walk (decisions 28, 32)', () => {
  it('W11: an empty release with uploads on throws naming release, NEXT_PUBLIC_APP_VERSION and SENTINEL_UPLOAD_SOURCEMAPS=false; nothing walked, maps in place', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { sourcemaps: CREDS });
    await expect(runHook(out, dir)).rejects.toThrow(/release.*NEXT_PUBLIC_APP_VERSION.*SENTINEL_UPLOAD_SOURCEMAPS=false/);
    expect(calls).toHaveLength(0);
    expect(allMaps(dir).static).toHaveLength(3);
    expect(allMaps(dir).css).toHaveLength(1);
  });

  it('W11: `NEXT_PUBLIC_APP_VERSION="2.34.0\\n"` throws naming the newline cause and the env variable, never trimming; maps in place', async () => {
    const dir = tree();
    process.env.NEXT_PUBLIC_APP_VERSION = '2.34.0\n';
    const out = withSentinelConfig({}, { sourcemaps: CREDS });
    await expect(runHook(out, dir)).rejects.toThrow(/newline/);
    await expect(runHook(out, dir)).rejects.toThrow(/NEXT_PUBLIC_APP_VERSION/);
    await expect(runHook(out, dir)).rejects.toThrow(/whitespace/);
    expect(calls).toHaveLength(0);
    expect(allMaps(dir).static).toHaveLength(3);
  });

  it('W11: a whitespace-padded `release` option is refused the same way', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { release: ' 2.34.0', sourcemaps: CREDS });
    await expect(runHook(out, dir)).rejects.toThrow(/release.*whitespace/);
    expect(calls).toHaveLength(0);
  });

  it('W11: with uploads off an empty or padded release is fine — nothing uploaded, maps deleted as in W3', async () => {
    const dir = tree();
    process.env.NEXT_PUBLIC_APP_VERSION = '2.34.0\n';
    const out = withSentinelConfig({}, { sourcemaps: { upload: false } });
    await expect(runHook(out, dir)).resolves.toBeUndefined();
    expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
  });
});

describe('W13: a walk that finds zero *.js.map with uploads on throws naming the walk and the flag', () => {
  it('W13: an empty distDir/static → throws naming the static dir and productionBrowserSourceMaps; the server series is not started; nothing deleted', async () => {
    const dir = makeTree({ css: true, server: 2 });
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    const err = await runHook(out, dir).catch((e: unknown) => e as Error);
    expect(err).toBeInstanceOf(Error);
    expect((err as Error).message).toContain(join(dir, 'static'));
    expect((err as Error).message).toContain('productionBrowserSourceMaps');
    expect(calls).toHaveLength(1);
    expect(allMaps(dir).server).toHaveLength(2);
    expect(allMaps(dir).css).toHaveLength(1);
  });

  it('W13: an empty distDir/server → throws naming the server dir and experimental.serverSourceMaps after the browser series; nothing deleted', async () => {
    const dir = makeTree({ count: 2, css: true });
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    const err = await runHook(out, dir).catch((e: unknown) => e as Error);
    expect((err as Error).message).toContain(join(dir, 'server'));
    expect((err as Error).message).toContain('experimental.serverSourceMaps');
    expect(calls).toHaveLength(2);
    expect(allMaps(dir).static).toHaveLength(2);
    expect(allMaps(dir).css).toHaveLength(1);
  });
});

describe('W8 (unit half): composition with a stub outer wrapper that chains like Sentry’s proxy', () => {
  /** What `withSentryConfig` does with the hook it finds: run the pre-existing one first, then its own step — here, delete every *.map. */
  function withStubSentry<C extends NextConfigLike>(config: C): C {
    const existing = config.compiler?.runAfterProductionCompile;
    const step: Hook = async (ctx) => {
      if (existing) await existing(ctx);
      const { rm } = await import('node:fs/promises');
      const { listMaps } = await vi.importActual<typeof import('../../src/upload/walk.js')>('../../src/upload/walk.js');
      for (const dir of ['static', 'server']) for (const p of await listMaps(join(ctx.distDir, dir))) await rm(p);
    };
    return { ...config, compiler: { ...config.compiler, runAfterProductionCompile: step } };
  }

  it('W8: withStubSentry(withSentinelConfig(cfg)) runs Sentinel’s hook first — both walks see their maps, then Sentry’s step finds none and does not fail', async () => {
    const dir = tree();
    const out = withStubSentry(withSentinelConfig({}, { release: RELEASE, sourcemaps: { ...CREDS, deleteAfterUpload: false } }));
    await expect(runHook(out, dir)).resolves.toBeUndefined();
    expect(walked).toEqual([3, 2]);
    expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
  });

  it('W8 reverse: withSentinelConfig(withStubSentry(cfg)) runs Sentry’s delete first — the browser walk finds zero maps and fails loud (W13)', async () => {
    const dir = tree();
    const out = withSentinelConfig(withStubSentry({}), { release: RELEASE, sourcemaps: CREDS });
    const err = await runHook(out, dir).catch((e: unknown) => e as Error);
    expect((err as Error).message).toContain(join(dir, 'static'));
    expect((err as Error).message).toContain('productionBrowserSourceMaps');
    expect(walked).toEqual([0]);
  });
});

describe('W15: deleteAfterUpload: false governs every path', () => {
  it('W15 (a): the success path keeps every .js.map, .mjs.map and .css.map under static/ and server/', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: { ...CREDS, deleteAfterUpload: false } });
    await runHook(out, dir);
    expect(calls).toHaveLength(2);
    expect(allMaps(dir).static).toHaveLength(3);
    expect(allMaps(dir).server).toHaveLength(2);
    expect(allMaps(dir).css).toHaveLength(1);
  });

  it('W15 (b): the not-configured path keeps every map and still warns "not uploaded" — without "deleting them anyway"', async () => {
    const dir = tree();
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: { deleteAfterUpload: false } });
    await runHook(out, dir);
    expect(warns).toHaveLength(1);
    expect(warns[0]).toMatch(/not uploaded/);
    expect(warns[0]).not.toMatch(/deleting them anyway/);
    expect(allMaps(dir).static).toHaveLength(3);
    expect(allMaps(dir).server).toHaveLength(2);
    expect(allMaps(dir).css).toHaveLength(1);
  });

  it('W15 (c): the exhausted-5xx path keeps every map and still warns "upload failed" naming the series — without "deleting maps anyway"', async () => {
    const dir = tree();
    script = { browser: { unavailable: { status: 503, code: 'storage_unavailable' } }, server: { unavailable: { network: 'ECONNREFUSED' } } };
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: { ...CREDS, deleteAfterUpload: false } });
    await runHook(out, dir);
    const failed = warns.filter((w) => /upload failed/.test(w));
    expect(failed).toHaveLength(2);
    for (const w of failed) expect(w).not.toMatch(/deleting maps anyway/);
    expect(allMaps(dir).static).toHaveLength(3);
    expect(allMaps(dir).server).toHaveLength(2);
    expect(allMaps(dir).css).toHaveLength(1);
  });
});

// BUG-03 (the seat's decision 45): webpack's trace plugin writes `<entry>.js.nft.json` beside every server entry at
// compile time listing the `.js.map` beside every chunk it needs, and `output: 'standalone'`'s copy reads them AFTER
// the hook — so the sweep must rewrite the traces it invalidates or the copy ENOENTs (W2). The tree here carries
// webpack's shape: `server/app/route-000.js.nft.json` and `server/instrumentation.js.nft.json` as Next 15.5.21 writes
// them (compact, `{"version":1,"files":[…]}`, entries relative to the trace's own directory, `../` for the chunks).
describe('W2 (BUG-03, webpack + standalone): the sweep rewrites the *.nft.json traces under distDir/server without the entries it deleted', () => {
  /** A webpack-shaped tree: `tree()` plus `server/chunks/260.js` (+ map) and two traces listing the maps beside what they need. */
  function webpackTree(): { dir: string; routeTrace: string; instrumentationTrace: string; nodeModulesMap: string } {
    const dir = tree();
    mkdirSync(join(dir, 'server/chunks'), { recursive: true });
    writeFileSync(join(dir, 'server/chunks/260.js'), 'module.exports={};\n//# sourceMappingURL=260.js.map\n');
    writeFileSync(join(dir, 'server/chunks/260.js.map'), '{"version":3,"file":"260.js","sources":["src/260.ts"],"names":[],"mappings":"AAAA"}\n');
    // A map OUTSIDE the sweep (under distDir, beside static/ and server/) that a trace lists — the rule is "what this
    // run deleted", never `*.map` (constraint 1).
    mkdirSync(join(dir, 'node_modules/dep/dist'), { recursive: true });
    const nodeModulesMap = join(dir, 'node_modules/dep/dist/index.js.map');
    writeFileSync(nodeModulesMap, '{"version":3,"sources":[],"names":[],"mappings":""}\n');
    const routeTrace = join(dir, 'server/app/route-000.js.nft.json');
    writeFileSync(
      routeTrace,
      JSON.stringify({ version: 1, files: ['../../../package.json', '../chunks/260.js', '../chunks/260.js.map', 'route-000.js.map', '../../node_modules/dep/dist/index.js', '../../node_modules/dep/dist/index.js.map', 'route-001.js', 'route-001.js.map'] }),
    );
    const instrumentationTrace = join(dir, 'server/instrumentation.js.nft.json');
    writeFileSync(instrumentationTrace, JSON.stringify({ version: 1, files: ['../package.json', 'chunks/260.js', 'chunks/260.js.map', 'instrumentation.js.map', 'webpack-runtime.js', 'webpack-runtime.js.map'] }));
    return { dir, routeTrace, instrumentationTrace, nodeModulesMap };
  }

  function files(trace: string): unknown {
    return JSON.parse(readFileSync(trace, 'utf8'));
  }

  it('W2: after the success path every map is gone and each trace lists only what still exists — compact, `version` before `files`, the remaining entries in their original order; one log line per rewritten trace with the count', async () => {
    const { dir, routeTrace, instrumentationTrace, nodeModulesMap } = webpackTree();
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    await runHook(out, dir);
    expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
    expect(existsSync(join(dir, 'server/chunks/260.js.map'))).toBe(false);
    expect(readFileSync(routeTrace, 'utf8')).toBe(
      '{"version":1,"files":["../../../package.json","../chunks/260.js","../../node_modules/dep/dist/index.js","../../node_modules/dep/dist/index.js.map","route-001.js"]}',
    );
    expect(existsSync(nodeModulesMap)).toBe(true);
    // `instrumentation.js.map` and `webpack-runtime.js.map` never existed in this tree: not this run's to remove (constraint 1).
    expect(files(instrumentationTrace)).toEqual({ version: 1, files: ['../package.json', 'chunks/260.js', 'instrumentation.js.map', 'webpack-runtime.js', 'webpack-runtime.js.map'] });
    const lines = logs.filter((l) => l.startsWith('Sentinel: build trace '));
    expect(lines).toHaveLength(2);
    expect(lines.find((l) => l.includes('server/app/route-000.js.nft.json'))).toMatch(/ 3 deleted map entries removed$/);
    expect(lines.find((l) => l.includes('server/instrumentation.js.nft.json'))).toMatch(/ 1 deleted map entry removed$/);
    expect(warns).toEqual([]);
  });

  it('W2 / W3: the upload-off and not-configured paths sweep the same way, so a webpack standalone build without credentials is green too', async () => {
    for (const sourcemaps of [{ upload: false as const, ...CREDS }, {}]) {
      const { dir, routeTrace } = webpackTree();
      const out = withSentinelConfig({}, { release: RELEASE, sourcemaps });
      await runHook(out, dir);
      expect(allMaps(dir)).toEqual({ static: [], server: [], css: [] });
      expect((files(routeTrace) as { files: string[] }).files).not.toContain('../chunks/260.js.map');
      expect((files(routeTrace) as { files: string[] }).files).toContain('../chunks/260.js');
      expect(logs.filter((l) => l.startsWith('Sentinel: build trace '))).toHaveLength(2);
      logs.length = 0;
    }
  });

  it('W2 / W14: the exhausted-5xx path deletes anyway and repairs the traces anyway', async () => {
    const { dir, routeTrace } = webpackTree();
    script = { browser: 'ok', server: { unavailable: { status: 503, code: 'storage_unavailable' } } };
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS });
    await runHook(out, dir);
    expect(allMaps(dir).server).toEqual([]);
    expect((files(routeTrace) as { files: string[] }).files).not.toContain('route-000.js.map');
  });

  it('W2 / W15: `deleteAfterUpload: false` leaves every trace byte-identical — nothing was deleted, nothing is rewritten', async () => {
    const { dir, routeTrace, instrumentationTrace } = webpackTree();
    const before = [readFileSync(routeTrace, 'utf8'), readFileSync(instrumentationTrace, 'utf8')];
    const out = withSentinelConfig({}, { release: RELEASE, sourcemaps: { ...CREDS, deleteAfterUpload: false } });
    await runHook(out, dir);
    expect([readFileSync(routeTrace, 'utf8'), readFileSync(instrumentationTrace, 'utf8')]).toEqual(before);
    expect(logs.filter((l) => l.startsWith('Sentinel: build trace '))).toEqual([]);
  });

  it('W2 / W4: a 4xx deletes nothing, so no trace is touched', async () => {
    const { dir, routeTrace } = webpackTree();
    const before = readFileSync(routeTrace, 'utf8');
    script = { browser: { rejected: { status: 401, code: 'unauthorized' } }, server: 'ok' };
    await expect(runHook(withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS }), dir)).rejects.toThrow(/401/);
    expect(readFileSync(routeTrace, 'utf8')).toBe(before);
  });

  it('W2: `silent: true` hides the per-trace line (it is part of the report, not a warn)', async () => {
    const { dir, routeTrace } = webpackTree();
    await runHook(withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS, silent: true }), dir);
    expect(logs).toEqual([]);
    expect((files(routeTrace) as { files: string[] }).files).not.toContain('route-000.js.map');
  });

  it('W2 (Turbopack shape): traces that list no map — Turbopack’s — are left byte-identical and print nothing', async () => {
    const dir = copyTree(TURBOPACK);
    const trace = join(dir, 'server/app/page.js.nft.json');
    mkdirSync(dirname(trace), { recursive: true });
    const text = JSON.stringify({ version: 1, files: ['../../../package.json', '../../chunks/ssr/[turbopack]_runtime.js', 'page.js'] });
    writeFileSync(trace, text);
    await runHook(withSentinelConfig({}, { release: RELEASE, sourcemaps: CREDS }), dir);
    expect(readFileSync(trace, 'utf8')).toBe(text);
    expect(logs.filter((l) => l.startsWith('Sentinel: build trace '))).toEqual([]);
  });

  describe('pruneTraces — the rewrite keeps everything but the deleted entries', () => {
    it('keeps key order, unknown keys, a pretty-printed file’s indentation and its trailing newline (constraint 2)', async () => {
      const dir = makeTree({ server: 1 });
      const trace = join(dir, 'server/app/route-000.js.nft.json');
      writeFileSync(trace, `{\n  "version": 1,\n  "files": [\n    "route-000.js.map",\n    "route-000.js"\n  ],\n  "reasons": {"route-000.js": "entry"}\n}\n`);
      const deleted = new Set([join(dir, 'server/app/route-000.js.map')]);
      const pruned = await pruneTraces(join(dir, 'server'), deleted);
      expect(pruned).toEqual([{ path: trace, removed: ['route-000.js.map'] }]);
      expect(readFileSync(trace, 'utf8')).toBe(`{\n  "version": 1,\n  "files": [\n    "route-000.js"\n  ],\n  "reasons": {\n    "route-000.js": "entry"\n  }\n}\n`);
    });

    it('matches by the resolved path — the same map named two ways from two directories is removed from both, a different `.map` is not', async () => {
      const dir = makeTree({ server: 1 });
      const a = join(dir, 'server/app/route-000.js.nft.json');
      const b = join(dir, 'server/instrumentation.js.nft.json');
      writeFileSync(a, JSON.stringify({ version: 1, files: ['route-000.js.map', './route-000.js.map', 'other.js.map'] }));
      writeFileSync(b, JSON.stringify({ version: 1, files: ['app/route-000.js.map', 'app/../app/route-000.js.map', 'app/other.js.map'] }));
      const pruned = await pruneTraces(join(dir, 'server'), new Set([join(dir, 'server/app/route-000.js.map')]));
      expect(pruned.map((p) => p.removed)).toEqual([
        ['route-000.js.map', './route-000.js.map'],
        ['app/route-000.js.map', 'app/../app/route-000.js.map'],
      ]);
      expect(JSON.parse(readFileSync(a, 'utf8'))).toEqual({ version: 1, files: ['other.js.map'] });
      expect(JSON.parse(readFileSync(b, 'utf8'))).toEqual({ version: 1, files: ['app/other.js.map'] });
    });

    it('leaves a non-JSON, non-object or files-less trace byte-identical, and reports nothing for a trace with nothing to remove or an empty deleted set', async () => {
      const dir = makeTree({ server: 1 });
      const texts: Record<string, string> = {
        'server/app/broken.js.nft.json': '{"version":1,"files":[',
        'server/app/array.js.nft.json': '["route-000.js.map"]',
        'server/app/nofiles.js.nft.json': '{"version":1}',
        'server/app/strings.js.nft.json': '{"version":1,"files":"route-000.js.map"}',
        'server/app/clean.js.nft.json': '{"version":1,"files":["route-000.js"]}',
      };
      for (const [rel, text] of Object.entries(texts)) writeFileSync(join(dir, rel), text);
      const deleted = new Set([join(dir, 'server/app/route-000.js.map')]);
      expect(await pruneTraces(join(dir, 'server'), deleted)).toEqual([]);
      for (const [rel, text] of Object.entries(texts)) expect(readFileSync(join(dir, rel), 'utf8')).toBe(text);
      writeFileSync(join(dir, 'server/app/route-000.js.nft.json'), '{"version":1,"files":["route-000.js.map"]}');
      expect(await pruneTraces(join(dir, 'server'), new Set())).toEqual([]);
      expect(readFileSync(join(dir, 'server/app/route-000.js.nft.json'), 'utf8')).toBe('{"version":1,"files":["route-000.js.map"]}');
      expect(await pruneTraces(join(dir, 'does-not-exist'), deleted)).toEqual([]);
    });
  });
});
