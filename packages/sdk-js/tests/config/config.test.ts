// `withSentinelConfig` (contract §4.9; §11 *`withSentinelConfig`*; flows W3–W8, W10–W15 — the unit halves) with
// the uploader stubbed at the `uploadSourcemaps` boundary: the stub walks the real fixture tree through the real
// walker (so an empty walk is the real `no_maps`, W13) and answers per a script — stored, a 4xx `rejected`, or an
// exhausted 5xx / network `unavailable` — while recording every call. The tree is a temp `distDir` with
// `.js.map`, `.mjs.map` and `.css.map` under `static/` and `.js.map` under `server/`. Messages are asserted by
// their named tokens (release, the walk's directory, the series name, the status or code), never by sentence
// (contract §11's guidance rule). Shuffle-safe: every test builds its own tree, script and env.

import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { UploadEvent, UploadReport, UploadSourcemapsOptions } from '../../src/upload/index.js';
import { cleanupTrees, makeTree } from '../cli/helpers.js';

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
    const pairs = await actual.walkSourcemaps(opts.buildDir, opts.publicPrefix);
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
const { DEFAULT_SERVER_ROOT, defaultPublicPrefix, withSentinelConfig } = await import('../../src/nextjs/config.js');
type NextConfigLike = import('../../src/nextjs/config.js').NextConfigLike;
type Hook = NonNullable<NonNullable<NextConfigLike['compiler']>['runAfterProductionCompile']>;

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
