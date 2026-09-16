// The CLI rows of contract §4.8 (flows C1–C13; §11 *The CLI's walker and pairing*) against a stubbed
// `POST /api/sourcemaps`: the pairing, batches by count and by bytes, the exit codes, the failure classes of
// decision 34 (every 5xx and network failure one retry-then-exit-1 class; the code printed, never branched on),
// `--delete` per successful batch, the release refusals before any request, `--dry-run` / `--json`, the
// `--batch-size` posture, and C11's secrecy grep. `runCli` runs in-process with the ladder's sleeps recorded and
// skipped; C13's synthetic sizes go through a recording `send` that reads the declared `Content-Length` and never
// a byte of body. Shuffle-safe: every test builds its own tree and stub.

import { closeSync, existsSync, ftruncateSync, mkdirSync, openSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, describe, expect, it } from 'vitest';
import { REACHABILITY_LINE, USAGE } from '../../src/cli/main.js';
import { INDEX_MAP_CHECK_MAX_BYTES, indexMapKind, MAX_REQUEST_BYTES, parseSourceMappingUrl, uploadSourcemaps, walkSourcemaps } from '../../src/upload/index.js';
import {
  API_KEY,
  cleanupTrees,
  copyTree,
  envelope,
  makeTree,
  recordingSend,
  refusedUrl,
  RELEASE,
  run,
  type RunResult,
  type SourcemapStub,
  sourcemapJson,
  startSourcemapStub,
  stored,
  uploadArgs,
} from './helpers.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const FIXTURE_STATIC = join(root, 'tests/fixtures-build/.next/static');
const TURBOPACK = join(root, 'tests/fixtures-build/turbopack/.next');
const stubs: SourcemapStub[] = [];
/** Every run's output in this file, for C11's grep (the C11 test also drives its own rows, so shuffle cannot empty it). */
const outputs: RunResult[] = [];

async function stub(responder?: Parameters<typeof startSourcemapStub>[0]): Promise<SourcemapStub> {
  const s = await startSourcemapStub(responder);
  stubs.push(s);
  return s;
}

async function cli(argv: string[], opts: Parameters<typeof run>[1] = {}): Promise<RunResult> {
  const r = await run(argv, opts);
  outputs.push(r);
  return r;
}

afterEach(async () => {
  for (const s of stubs.splice(0)) await s.close();
  cleanupTrees();
});

function mapsUnder(dir: string): string[] {
  const out: string[] = [];
  const visit = (d: string): void => {
    for (const e of readdirSync(d, { withFileTypes: true })) {
      const p = join(d, e.name);
      if (e.isDirectory()) visit(p);
      else if (e.name.endsWith('.map')) out.push(p);
    }
  };
  if (existsSync(dir)) visit(dir);
  return out.sort();
}

describe('the walker and the pairing (contract §4.8 step 1 — docs/sdk-specification.md §8’s loop)', () => {
  it('C1: `.next/static /_next/static` pairs `chunks/app/page-9b0d.js.map` to `/_next/static/chunks/app/page-9b0d.js`, walks only *.js.map / *.mjs.map, in sorted walk order', async () => {
    const pairs = await walkSourcemaps(FIXTURE_STATIC, '/_next/static');
    expect(pairs.map((p) => p.minifiedPath)).toEqual([
      '/_next/static/chunks/0f1e2d3c4b5a6978.mjs',
      '/_next/static/chunks/app/layout-7a8b.js',
      '/_next/static/chunks/app/page-9b0d.js',
      '/_next/static/chunks/framework-3c4d.js',
      '/_next/static/chunks/main-app-1a2b.js',
      '/_next/static/chunks/polyfills-5e6f.js',
      '/_next/static/chunks/webpack-9c0d.js',
    ]);
    expect(pairs.find((p) => p.minifiedPath.endsWith('webpack-9c0d.js'))?.siblingExists).toBe(false);
    expect(pairs.find((p) => p.minifiedPath.endsWith('webpack-9c0d.js'))?.pairedBy).toBe('name');
    expect(pairs.filter((p) => !p.minifiedPath.endsWith('webpack-9c0d.js')).every((p) => p.siblingExists && p.pairedBy === 'comment')).toBe(true);
    for (const p of pairs) expect(p.sizeBytes).toBe(readFileSync(p.path).byteLength);
  });

  it('C1: a trailing slash on the prefix is stripped, `/` alone pairs to `/assets/x.js`, and a filesystem prefix (`/app/.next/server`, decision 18) pairs byte-identically', async () => {
    const dir = makeTree({ extra: ['assets/index-3f2a1c.js', 'server/app/page.js'] });
    expect((await walkSourcemaps(join(dir, 'assets'), '/')).map((p) => p.minifiedPath)).toEqual(['/index-3f2a1c.js']);
    expect((await walkSourcemaps(dir, '/')).map((p) => p.minifiedPath)).toEqual(['/assets/index-3f2a1c.js', '/server/app/page.js']);
    expect((await walkSourcemaps(join(dir, 'server'), '/app/.next/server/')).map((p) => p.minifiedPath)).toEqual(['/app/.next/server/app/page.js']);
  });

  it('C1: the CLI posts the seven fixture pairs positionally in one multipart request — release_version first, X-API-Key, X-Project-Key, no Origin — and prints `<outcome>  <path>  <sha256[:12]>  <size>` per file, exit 0', async () => {
    const s = await stub();
    const r = await cli(['upload', FIXTURE_STATIC, '/_next/static', '--release', RELEASE, '--dashboard-url', s.url, '--api-key', API_KEY, '--project-key', 'org-key-1']);
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(1);
    const req = s.requests[0]!;
    expect(req.method).toBe('POST');
    expect(req.url).toBe('/api/sourcemaps');
    expect(req.headers['x-api-key']).toBe(API_KEY);
    expect(req.headers['x-project-key']).toBe('org-key-1');
    expect(req.headers.origin).toBeUndefined();
    expect(req.headers['content-type']).toMatch(/^multipart\/form-data; boundary=/);
    expect(req.contentLength).toBe(req.receivedBytes);
    expect(req.release).toBe(RELEASE);
    expect(req.paths).toHaveLength(7);
    expect(req.paths[2]).toBe('/_next/static/chunks/app/page-9b0d.js');
    expect(req.files).toHaveLength(7);
    expect(req.files[2]!.name).toBe('page-9b0d.js.map');
    expect(JSON.parse(req.files[2]!.text)).toMatchObject({ version: 3, file: 'page-9b0d.js' });
    const lines = r.stdout.trim().split('\n');
    expect(lines).toHaveLength(7);
    expect(lines[2]).toBe(`created  /_next/static/chunks/app/page-9b0d.js  0003${'ab'.repeat(4)}  ${req.files[2]!.size}`);
    expect(r.stderr).toMatch(/webpack-9c0d\.js beside it/);
    expect(r.stderr).toMatch(/7 file\(s\) stored in 1 request\(s\)/);
    // Nothing was deleted without --delete.
    expect(mapsUnder(FIXTURE_STATIC)).toHaveLength(8);
  });
});

describe('BUG-01: the pairing follows the chunk’s `//# sourceMappingURL=` comment (contract §4.8 step 1 as amended — decision 43)', () => {
  const TP_STATIC = join(TURBOPACK, 'static');
  const TP_SERVER = join(TURBOPACK, 'server');

  it('C1 (Turbopack): `d69eb7d85499027c.js` → its map `8f15d5d03090a0be.js.map` is stored under the CHUNK’s path; a chunk with no map and one whose comment names a missing file are skipped with a warn each; an unreferenced map keeps the name pairing', async () => {
    const warns: string[] = [];
    const pairs = await walkSourcemaps(TP_STATIC, '/_next/static', { onWarn: (line) => warns.push(line) });
    expect(pairs.map((p) => [p.minifiedPath, p.path.split('/').pop(), p.pairedBy, p.siblingExists])).toEqual([
      ['/_next/static/chunks/9a8b7c6d5e4f3a2b.js', '9a8b7c6d5e4f3a2b.js.map', 'name', false],
      ['/_next/static/chunks/d69eb7d85499027c.js', '8f15d5d03090a0be.js.map', 'comment', true],
      ['/_next/static/chunks/turbopack-4364a8ae2a6af640.js', 'e96569a392fa278c.js.map', 'comment', true],
    ]);
    // Never the map's own name — that is the path no browser frame carries (TASK-07 F3).
    expect(pairs.map((p) => p.minifiedPath)).not.toContain('/_next/static/chunks/8f15d5d03090a0be.js');
    expect(pairs.map((p) => p.minifiedPath)).not.toContain('/_next/static/chunks/e96569a392fa278c.js');
    expect(pairs[1]!.chunkPath).toBe(join(TP_STATIC, 'chunks/d69eb7d85499027c.js'));
    expect(pairs[1]!.sizeBytes).toBe(readFileSync(join(TP_STATIC, 'chunks/8f15d5d03090a0be.js.map')).byteLength);
    expect(warns).toHaveLength(2);
    expect(warns.find((w) => w.includes('60394cfab7bcbc98.js'))).toMatch(/names 4c218c652b8c48af\.js\.map, which does not exist; skipping/);
    expect(warns.find((w) => w.includes('3f9721961e8f2ba6.js'))).toMatch(/^1 script\(s\) under .* carry no sourceMappingURL comment/);
    expect(warns.join('\n')).not.toMatch(/css/);
  });

  it('C1 (Turbopack, server walk): the percent-encoded comments (`%5B`, `%40`) find the file beside the chunk, the edge-wrapper pair is stored under `turbopack-edge-wrapper_<h>.js`, and Next’s manifest .js is the one “no comment” line', async () => {
    const warns: string[] = [];
    const pairs = await walkSourcemaps(TP_SERVER, '/app/.next/server', { onWarn: (line) => warns.push(line) });
    expect(pairs.map((p) => [p.minifiedPath, p.path.split('/').pop(), p.pairedBy])).toEqual([
      ['/app/.next/server/chunks/[root-of-the-server]__a6734ae3._.js', '[root-of-the-server]__a6734ae3._.js.map', 'comment'],
      ['/app/.next/server/chunks/_0fe1378c._.js', '_0fe1378c._.js.map', 'comment'],
      ['/app/.next/server/chunks/f9941_@sentinel_sdk-js_dist_nextjs_edge_d8f68d22.js', 'f9941_@sentinel_sdk-js_dist_nextjs_edge_d8f68d22.js.map', 'comment'],
      ['/app/.next/server/edge/chunks/edge-wrapper_8937e6f3.js', 'edge-wrapper_8937e6f3.js.map', 'comment'],
      ['/app/.next/server/edge/chunks/turbopack-edge-wrapper_56e5ba59.js', 'edge-wrapper_56e5ba59.js.map', 'comment'],
    ]);
    expect(warns).toEqual([
      expect.stringMatching(/^1 script\(s\) under .* carry no sourceMappingURL comment .*: app\/page_client-reference-manifest\.js$/),
      expect.stringMatching(/app\/page\.js\.map: empty index map \(no mappings, no sections\) — nothing to symbolicate; skipping it$/),
      expect.stringMatching(/chunks\/\[turbopack\]_runtime\.js\.map: index map \(sections, no mappings\) — the route stores flat maps only; skipping it$/),
    ]);
    // Neither index map's path is ever stored (BUG-02): the entry stub's `{"version":3,"sources":[],"sections":[]}` has
    // nothing to symbolicate and the runtime's sections describe Turbopack's own runtime, which the route refuses anyway.
    expect(pairs.map((p) => p.minifiedPath)).not.toContain('/app/.next/server/app/page.js');
    expect(pairs.map((p) => p.minifiedPath)).not.toContain('/app/.next/server/chunks/[turbopack]_runtime.js');
  });

  it('C1 (Turbopack): the CLI posts the three pairs as the chunks’ paths with the differently-named map bytes, prints the stored chunk paths, and the two skip lines plus the orphan’s “beside it” on stderr; exit 0', async () => {
    const s = await stub();
    const r = await cli(['upload', TP_STATIC, '/_next/static', '--release', RELEASE, '--dashboard-url', s.url, '--api-key', API_KEY]);
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(1);
    const req = s.requests[0]!;
    expect(req.paths).toEqual(['/_next/static/chunks/9a8b7c6d5e4f3a2b.js', '/_next/static/chunks/d69eb7d85499027c.js', '/_next/static/chunks/turbopack-4364a8ae2a6af640.js']);
    expect(req.files.map((f) => f.name)).toEqual(['9a8b7c6d5e4f3a2b.js.map', '8f15d5d03090a0be.js.map', 'e96569a392fa278c.js.map']);
    expect(JSON.parse(req.files[1]!.text)).toMatchObject({ version: 3, sources: ['turbopack:///[project]/app/boundary/thrower.tsx'] });
    expect(JSON.parse(req.files[1]!.text)).not.toHaveProperty('file');
    const lines = r.stdout.trim().split('\n');
    expect(lines).toEqual([
      expect.stringMatching(/^created  \/_next\/static\/chunks\/9a8b7c6d5e4f3a2b\.js  /),
      expect.stringMatching(/^created  \/_next\/static\/chunks\/d69eb7d85499027c\.js  /),
      expect.stringMatching(/^created  \/_next\/static\/chunks\/turbopack-4364a8ae2a6af640\.js  /),
    ]);
    expect(r.stdout).not.toContain('8f15d5d03090a0be.js ');
    expect(r.stderr).toMatch(/warning: .*60394cfab7bcbc98\.js: its sourceMappingURL names 4c218c652b8c48af\.js\.map, which does not exist; skipping the chunk/);
    expect(r.stderr).toMatch(/warning: 1 script\(s\) under .* carry no sourceMappingURL comment .*: chunks\/3f9721961e8f2ba6\.js/);
    expect(r.stderr).toMatch(/warning: .*9a8b7c6d5e4f3a2b\.js\.map: no .*9a8b7c6d5e4f3a2b\.js beside it; uploading the map anyway/);
    expect(r.stderr).toMatch(/3 file\(s\) stored in 1 request\(s\)/);
    expect(mapsUnder(TP_STATIC)).toHaveLength(4);
  });

  it('C4 (Turbopack): --delete unlinks the map under its own name after the batch; the chunk, the orphan-less chunks and the .css.map stay', async () => {
    const dir = copyTree(TURBOPACK);
    const s = await stub();
    const r = await cli(uploadArgs(dir, s.url, '--delete'));
    expect(r.code).toBe(0);
    expect(mapsUnder(join(dir, 'static'))).toEqual([join(dir, 'static/chunks/20c20fcd5d02c4c2.css.map')]);
    expect(existsSync(join(dir, 'static/chunks/d69eb7d85499027c.js'))).toBe(true);
    expect(existsSync(join(dir, 'static/chunks/3f9721961e8f2ba6.js'))).toBe(true);
    expect(r.stderr).toMatch(/3 map\(s\) deleted/);
  });

  it('a map two chunks reference is posted once per chunk path and unlinked once, after the LAST batch that carried it (--batch-size 1 puts them in two requests)', async () => {
    const dir = makeTree({ extra: ['static/chunks/a-1111.js'] });
    writeFileSync(join(dir, 'static/chunks/b-2222.js'), 'export const y=2;\n//# sourceMappingURL=a-1111.js.map\n');
    const shared = join(dir, 'static/chunks/a-1111.js.map');
    const thereAtAnswer: boolean[] = [];
    const s = await stub((req) => {
      thereAtAnswer.push(existsSync(shared));
      return stored(req);
    });
    const r = await cli(uploadArgs(dir, s.url, '--delete', '--batch-size', '1'));
    expect(r.code).toBe(0);
    expect(s.requests.map((q) => q.paths)).toEqual([['/_next/static/chunks/a-1111.js'], ['/_next/static/chunks/b-2222.js']]);
    expect(s.requests.map((q) => q.files[0]!.name)).toEqual(['a-1111.js.map', 'a-1111.js.map']);
    expect(s.requests[1]!.files[0]!.text).toBe(sourcemapJson('a-1111.js'));
    // Still on disk when batch 1 was answered (batch 2 has yet to stream it); gone after batch 2.
    expect(thereAtAnswer).toEqual([true, true]);
    expect(existsSync(shared)).toBe(false);
    expect(r.stderr).toMatch(/batch 1: 201, 1 file\(s\)\n/);
    expect(r.stderr).toMatch(/batch 2: 201, 1 file\(s\), 1 map\(s\) deleted\n/);
    expect(r.stderr).not.toMatch(/ENOENT/);
  });

  it('a relative <build dir> pairs the same as an absolute one (the comment’s target is resolved against the chunk’s directory)', async () => {
    const dir = copyTree(TURBOPACK);
    const cwd = process.cwd();
    try {
      process.chdir(tmpdir());
      const rel = join(dir.slice(tmpdir().length + 1), 'static');
      expect(rel.startsWith('/')).toBe(false);
      const pairs = await walkSourcemaps(rel, '/_next/static');
      expect(pairs.map((p) => p.minifiedPath)).toEqual(['/_next/static/chunks/9a8b7c6d5e4f3a2b.js', '/_next/static/chunks/d69eb7d85499027c.js', '/_next/static/chunks/turbopack-4364a8ae2a6af640.js']);
      expect(pairs.map((p) => p.path)).toEqual([join(rel, 'chunks/9a8b7c6d5e4f3a2b.js.map'), join(dir, 'static/chunks/8f15d5d03090a0be.js.map'), join(dir, 'static/chunks/e96569a392fa278c.js.map')]);
    } finally {
      process.chdir(cwd);
    }
  });

  it('parseSourceMappingUrl: the last comment wins, `/*# … */` counts, a query is dropped, `%40` / `%5B` decode, a bad escape is kept, and data: / http(s): / a missing comment are undefined', () => {
    expect(parseSourceMappingUrl('x\n//# sourceMappingURL=a.js.map\n')).toBe('a.js.map');
    expect(parseSourceMappingUrl('//# sourceMappingURL=old.map\nmore\n//@ sourceMappingURL=new.map')).toBe('new.map');
    expect(parseSourceMappingUrl('body{}\n/*# sourceMappingURL=styles.css.map */\n')).toBe('styles.css.map');
    expect(parseSourceMappingUrl('//# sourceMappingURL=a.js.map?v=1#x')).toBe('a.js.map');
    expect(parseSourceMappingUrl('//# sourceMappingURL=f9941_%40sentinel_sdk-js_dist_nextjs_edge_d8f68d22.js.map')).toBe('f9941_@sentinel_sdk-js_dist_nextjs_edge_d8f68d22.js.map');
    expect(parseSourceMappingUrl('//# sourceMappingURL=%5Broot-of-the-server%5D__a6734ae3._.js.map')).toBe('[root-of-the-server]__a6734ae3._.js.map');
    expect(parseSourceMappingUrl('//# sourceMappingURL=bad%E0.map')).toBe('bad%E0.map');
    expect(parseSourceMappingUrl('//# sourceMappingURL=data:application/json;base64,eyJ2ZXJzaW9uIjozfQ==')).toBeUndefined();
    expect(parseSourceMappingUrl('//# sourceMappingURL=https://cdn.example/a.js.map')).toBeUndefined();
    expect(parseSourceMappingUrl('//# sourceMappingURL=')).toBeUndefined();
    expect(parseSourceMappingUrl('export const x=1;\n')).toBeUndefined();
    expect(parseSourceMappingUrl('')).toBeUndefined();
  });
});

describe('BUG-02: an index map is skipped with one warn, never posted (contract §4.8 step 1 as amended — decision 44)', () => {
  const EMPTY_INDEX = '{"version":3,"sources":[],"sections":[]}';
  /** Turbopack's `[turbopack]_runtime.js.map` shape: sections over its own runtime, `ignoreList`, no top-level `mappings`. */
  const RUNTIME_INDEX = JSON.stringify({
    version: 3,
    sources: [],
    sections: [{ offset: { line: 3, column: 0 }, map: { version: 3, sources: ['turbopack:///[turbopack]/shared/runtime-utils.ts'], names: [], mappings: 'AAAA', ignoreList: [0] } }],
  });

  it('C1 (Turbopack, server walk): the entry stub’s `{"version":3,"sources":[],"sections":[]}` beside a real chunk map is skipped with a warn naming the file; the real map is posted and printed; exit 0', async () => {
    const dir = makeTree({ server: 1 });
    // Turbopack 15.5.21's one-line entry stub and its index map — the shape of 10 of the scratch app's 62 server maps.
    mkdirSync(join(dir, 'server/app/route'), { recursive: true });
    writeFileSync(join(dir, 'server/app/route/route.js'), 'module.exports = runtime.getOrInstantiateRuntimeModule("[project]/app/route/route.ts").exports;\n//# sourceMappingURL=route.js.map\n');
    writeFileSync(join(dir, 'server/app/route/route.js.map'), EMPTY_INDEX);
    const s = await stub();
    const r = await cli(['upload', join(dir, 'server'), '/app/.next/server', '--release', RELEASE, '--dashboard-url', s.url, '--api-key', API_KEY]);
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(1);
    expect(s.requests[0]!.paths).toEqual(['/app/.next/server/app/route-000.js']);
    expect(s.requests[0]!.files.map((f) => f.name)).toEqual(['route-000.js.map']);
    expect(r.stdout.trim().split('\n')).toEqual([expect.stringMatching(/^created  \/app\/.next\/server\/app\/route-000\.js  /)]);
    expect(r.stderr).toMatch(/warning: .*server\/app\/route\/route\.js\.map: empty index map \(no mappings, no sections\) — nothing to symbolicate; skipping it/);
    expect(r.stderr.match(/empty index map/g)).toHaveLength(1);
    expect(r.stderr).toMatch(/1 file\(s\) stored in 1 request\(s\)/);
    // Not uploaded, so not the CLI's to delete (step 6); the wrapper's every-*.map sweep removes it (§4.9 step 4).
    expect(existsSync(join(dir, 'server/app/route/route.js.map'))).toBe(true);
  });

  it('C4: --delete leaves the skipped index map in place and unlinks only the map it uploaded', async () => {
    const dir = makeTree({ server: 1 });
    writeFileSync(join(dir, 'server/app/page.js'), 'module.exports = 1;\n//# sourceMappingURL=page.js.map\n');
    writeFileSync(join(dir, 'server/app/page.js.map'), EMPTY_INDEX);
    const s = await stub();
    const r = await cli(['upload', join(dir, 'server'), '/app/.next/server', '--release', RELEASE, '--dashboard-url', s.url, '--api-key', API_KEY, '--delete']);
    expect(r.code).toBe(0);
    expect(mapsUnder(join(dir, 'server'))).toEqual([join(dir, 'server/app/page.js.map')]);
  });

  it('C1 (Turbopack, server walk): the non-empty `[turbopack]_runtime.js.map` (sections over Turbopack’s own runtime) is skipped with its own warn; the real map beside it is posted; exit 0', async () => {
    const dir = makeTree({ server: 1 });
    writeFileSync(join(dir, 'server/app/[turbopack]_runtime.js'), 'const RUNTIME_PUBLIC_PATH = "server/app/[turbopack]_runtime.js";\n//# sourceMappingURL=%5Bturbopack%5D_runtime.js.map\n');
    writeFileSync(join(dir, 'server/app/[turbopack]_runtime.js.map'), RUNTIME_INDEX);
    const s = await stub();
    const r = await cli(['upload', join(dir, 'server'), '/app/.next/server', '--release', RELEASE, '--dashboard-url', s.url, '--api-key', API_KEY]);
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(1);
    expect(s.requests[0]!.paths).toEqual(['/app/.next/server/app/route-000.js']);
    expect(r.stderr).toMatch(/warning: .*server\/app\/\[turbopack\]_runtime\.js\.map: index map \(sections, no mappings\) — the route stores flat maps only; skipping it/);
    expect(r.stderr).not.toMatch(/empty index map/);
    expect(existsSync(join(dir, 'server/app/[turbopack]_runtime.js.map'))).toBe(true);
  });

  it('a name-paired empty index map (no chunk references it) is skipped the same way, and a tree of only index maps is `no *.js.map` (exit 2)', async () => {
    const dir = makeTree({ server: 1 });
    writeFileSync(join(dir, 'server/app/orphan.js.map'), EMPTY_INDEX);
    const warns: string[] = [];
    const pairs = await walkSourcemaps(join(dir, 'server'), '/app/.next/server', { onWarn: (line) => warns.push(line) });
    expect(pairs.map((p) => p.minifiedPath)).toEqual(['/app/.next/server/app/route-000.js']);
    expect(warns).toEqual([expect.stringMatching(/orphan\.js\.map: empty index map .*; skipping it$/)]);

    const only = makeTree();
    mkdirSync(join(only, 'server/app'), { recursive: true });
    writeFileSync(join(only, 'server/app/page.js'), 'module.exports = 1;\n//# sourceMappingURL=page.js.map\n');
    writeFileSync(join(only, 'server/app/page.js.map'), EMPTY_INDEX);
    const s = await stub();
    const r = await cli(['upload', join(only, 'server'), '/app/.next/server', '--release', RELEASE, '--dashboard-url', s.url, '--api-key', API_KEY]);
    expect(r.code).toBe(2);
    expect(r.stderr).toMatch(/no \*\.js\.map under /);
    expect(s.requests).toHaveLength(0);
  });

  it('only a JSON object with no string `mappings` is skipped: `mappings: ""`, a non-JSON map, an array and one over INDEX_MAP_CHECK_MAX_BYTES are all uploaded as written for the route to judge', async () => {
    const dir = makeTree();
    mkdirSync(join(dir, 'server/app'), { recursive: true });
    const write = (name: string, body: string): void => {
      writeFileSync(join(dir, `server/app/${name}`), `module.exports = 1;\n//# sourceMappingURL=${name}.map\n`);
      writeFileSync(join(dir, `server/app/${name}.map`), body);
    };
    write('sections.js', RUNTIME_INDEX);
    write('blank.js', JSON.stringify({ version: 3, sources: [], names: [], mappings: '' }));
    write('broken.js', '{"version":3,"sources":[],"sections":[');
    write('array.js', '[]');
    write('big.js', `{"version":3,"sources":[],"sections":[],"x":"${'a'.repeat(INDEX_MAP_CHECK_MAX_BYTES)}"}`);
    write('empty.js', EMPTY_INDEX);
    write('nosections.js', '{"version":3,"sources":[]}');
    const warns: string[] = [];
    const pairs = await walkSourcemaps(join(dir, 'server'), '/app/.next/server', { onWarn: (line) => warns.push(line) });
    expect(pairs.map((p) => p.minifiedPath)).toEqual(['/app/.next/server/app/array.js', '/app/.next/server/app/big.js', '/app/.next/server/app/blank.js', '/app/.next/server/app/broken.js']);
    expect(warns).toEqual([
      expect.stringMatching(/empty\.js\.map: empty index map \(no mappings, no sections\) — nothing to symbolicate; skipping it$/),
      expect.stringMatching(/nosections\.js\.map: empty index map \(no mappings, no sections\) — nothing to symbolicate; skipping it$/),
      expect.stringMatching(/sections\.js\.map: index map \(sections, no mappings\) — the route stores flat maps only; skipping it$/),
    ]);
    await expect(indexMapKind(join(dir, 'server/app/empty.js.map'), EMPTY_INDEX.length)).resolves.toBe('empty');
    await expect(indexMapKind(join(dir, 'server/app/sections.js.map'), RUNTIME_INDEX.length)).resolves.toBe('sections');
    await expect(indexMapKind(join(dir, 'server/app/blank.js.map'), 60)).resolves.toBeUndefined();
    await expect(indexMapKind(join(dir, 'server/app/big.js.map'), INDEX_MAP_CHECK_MAX_BYTES + 1)).resolves.toBeUndefined();
    await expect(indexMapKind(join(dir, 'server/app/array.js.map'), 2)).resolves.toBeUndefined();
    await expect(indexMapKind(join(dir, 'server/app/broken.js.map'), 40)).resolves.toBeUndefined();
  });
});

describe('batches (contract §4.8 step 3)', () => {
  it('C2: 120 maps → three requests of 50, 50 and 20 files, in walk order; exit 0', async () => {
    const dir = makeTree({ count: 120 });
    const s = await stub();
    const r = await cli(uploadArgs(dir, s.url));
    expect(r.code).toBe(0);
    expect(s.requests.map((q) => q.paths.length)).toEqual([50, 50, 20]);
    const all = s.requests.flatMap((q) => q.paths);
    expect(all).toEqual((await walkSourcemaps(join(dir, 'static'), '/_next/static')).map((p) => p.minifiedPath));
    expect(r.stdout.trim().split('\n')).toHaveLength(120);
    for (const q of s.requests) expect(q.contentLength).toBe(q.receivedBytes);
  });

  it('C13: `--batch-size 0` → 1 with one warn, `500` → 50 with one warn; `many` and `""` → exit 2 before any request', async () => {
    const dir = makeTree({ count: 7 });
    const s = await stub();
    const zero = await cli(uploadArgs(dir, s.url, '--batch-size', '0'));
    expect(zero.code).toBe(0);
    expect(s.requests).toHaveLength(7);
    expect(zero.stderr.match(/warning: batch size 0/g)).toHaveLength(1);

    s.requests.length = 0;
    const big = await cli(uploadArgs(dir, s.url, '--batch-size', '500'));
    expect(big.code).toBe(0);
    expect(s.requests).toHaveLength(1);
    expect(big.stderr.match(/warning: batch size 500 .*using 50/g)).toHaveLength(1);

    s.requests.length = 0;
    const many = await cli(uploadArgs(dir, s.url, '--batch-size', 'many'));
    expect(many.code).toBe(2);
    expect(many.stderr).toMatch(/--batch-size must be a number, got "many"/);
    const empty = await cli(uploadArgs(dir, s.url, '--batch-size', ''));
    expect(empty.code).toBe(2);
    expect(s.requests).toHaveLength(0);
  });

  it('C13: the 30-size table is packed to ≤ 104 857 600 serialised bytes per request, the 120 MiB map alone — the stub reads only the declared Content-Length', async () => {
    const dir = makeTree();
    const MiB = 1024 * 1024;
    // 27 small maps of 128 KiB × (1..27) ≈ 47.25 MiB, then two of 60 MiB and one of 120 MiB — sparse files:
    // `ftruncate` sets `stat().size` without writing a block, and the recording send never opens them.
    const table: Array<[string, number]> = [];
    for (let i = 1; i <= 27; i++) table.push([`static/chunks/a-${String(i).padStart(2, '0')}.js.map`, i * 128 * 1024]);
    table.push(['static/chunks/b-60-1.js.map', 60 * MiB], ['static/chunks/b-60-2.js.map', 60 * MiB], ['static/chunks/c-120.js.map', 120 * MiB]);
    mkdirSync(join(dir, 'static/chunks'), { recursive: true });
    for (const [rel, size] of table) {
      const fd = openSync(join(dir, rel), 'w');
      ftruncateSync(fd, size);
      closeSync(fd);
    }
    const { send, requests } = recordingSend(() => ({ status: 201, body: JSON.stringify({ files: [] }) }));
    const r = await cli(uploadArgs(dir, 'http://stub.invalid'), { send });
    expect(r.code).toBe(0);
    expect(r.stderr).toMatch(/4 request\(s\)/);
    // [27 small] — the first 60 MiB would push it to ≈ 107 MiB; [60]; [60]; [120, alone, over the budget by itself].
    expect(requests).toHaveLength(4);
    const declared = requests.map((q) => q.contentLength);
    expect(declared[0]).toBeGreaterThan(47 * MiB);
    expect(declared[0]).toBeLessThanOrEqual(MAX_REQUEST_BYTES);
    expect(declared[1]).toBeGreaterThan(60 * MiB);
    expect(declared[1]).toBeLessThanOrEqual(MAX_REQUEST_BYTES);
    expect(declared[2]).toBeGreaterThan(60 * MiB);
    expect(declared[2]).toBeLessThanOrEqual(MAX_REQUEST_BYTES);
    expect(declared[3]).toBeGreaterThan(120 * MiB);
    expect(MAX_REQUEST_BYTES).toBe(104_857_600);
    for (const q of requests) {
      expect(q.headers['content-type']).toMatch(/^multipart\/form-data; boundary=----SentinelSourcemaps[0-9a-f]{32}$/);
      expect(q.headers.origin).toBeUndefined();
    }
  });

  it('C13: one real ≈ 1 MB map goes through the multipart path as a well-formed request the stub parses, Content-Length equal to the bytes received', async () => {
    const dir = makeTree();
    mkdirSync(join(dir, 'static/chunks'), { recursive: true });
    const big = JSON.stringify({ version: 3, file: 'big.js', sources: ['src/big.ts'], names: [], mappings: 'AAAA;'.repeat(200_000) });
    writeFileSync(join(dir, 'static/chunks/big.js.map'), big);
    expect(big.length).toBeGreaterThan(1_000_000);
    const s = await stub();
    const r = await cli(uploadArgs(dir, s.url));
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(1);
    const q = s.requests[0]!;
    expect(q.contentLength).toBe(q.receivedBytes);
    expect(q.paths).toEqual(['/_next/static/chunks/big.js']);
    expect(q.files[0]!.size).toBe(Buffer.byteLength(big));
    expect(q.files[0]!.text).toBe(big);
  });
});

describe('--delete (contract §4.8 step 6)', () => {
  it('C4: removes only the uploaded .js.map files, only after the 2xx; styles.css.map stays; a second run exits 2 with `no *.js.map`', async () => {
    const dir = makeTree({ count: 7, css: true });
    const stillThereAtAnswer: boolean[] = [];
    const s = await stub((req) => {
      stillThereAtAnswer.push(existsSync(join(dir, 'static/chunks/chunk-000.js.map')));
      return stored(req);
    });
    const first = await cli(uploadArgs(dir, s.url, '--delete'));
    expect(first.code).toBe(0);
    expect(stillThereAtAnswer).toEqual([true]);
    expect(mapsUnder(join(dir, 'static'))).toEqual([join(dir, 'static/css/styles.css.map')]);
    expect(existsSync(join(dir, 'static/chunks/chunk-000.js'))).toBe(true);
    expect(first.stderr).toMatch(/7 map\(s\) deleted/);

    const second = await cli(uploadArgs(dir, s.url, '--delete'));
    expect(second.code).toBe(2);
    expect(second.stderr).toMatch(/no \*\.js\.map under /);
    expect(s.requests).toHaveLength(1);
  });
});

describe('the release refusals — exit 2 before any request (contract §4.8 steps 2, 7)', () => {
  it('C5: `--release "2.34.0\\n"` (the $(cat VERSION) newline) exits 2 naming the whitespace and the newline cause, never trims, sends nothing', async () => {
    const dir = makeTree({ count: 2 });
    const s = await stub();
    const r = await cli(['upload', join(dir, 'static'), '/_next/static', '--release', '2.34.0\n', '--dashboard-url', s.url, '--api-key', API_KEY]);
    expect(r.code).toBe(2);
    expect(r.stderr).toMatch(/release .* whitespace/);
    expect(r.stderr).toMatch(/newline/);
    expect(s.requests).toHaveLength(0);
  });

  it('C12: `--release ""` and `SENTINEL_RELEASE=` empty with no flag both exit 2 with the release-less line; a missing release is a usage error too', async () => {
    const dir = makeTree({ count: 2 });
    const s = await stub();
    const flag = await cli(['upload', join(dir, 'static'), '/_next/static', '--release', '', '--dashboard-url', s.url, '--api-key', API_KEY]);
    expect(flag.code).toBe(2);
    expect(flag.stderr).toMatch(/release is empty/);
    const env = await cli(['upload', join(dir, 'static'), '/_next/static'], { env: { SENTINEL_RELEASE: '', SENTINEL_DASHBOARD_URL: s.url, SENTINEL_API_KEY: API_KEY } });
    expect(env.code).toBe(2);
    expect(env.stderr).toMatch(/release is empty/);
    const missing = await cli(['upload', join(dir, 'static'), '/_next/static'], { env: { SENTINEL_DASHBOARD_URL: s.url, SENTINEL_API_KEY: API_KEY } });
    expect(missing.code).toBe(2);
    expect(missing.stderr).toMatch(/--release/);
    expect(s.requests).toHaveLength(0);
  });

  it('the env fallbacks work for every option, and the flag wins over the env', async () => {
    const dir = makeTree({ count: 1 });
    const s = await stub();
    const r = await cli(['upload', join(dir, 'static'), '/_next/static', '--release', '9.9.9'], {
      env: { SENTINEL_RELEASE: RELEASE, SENTINEL_DASHBOARD_URL: s.url, SENTINEL_API_KEY: API_KEY, SENTINEL_PROJECT_KEY: 'org-key-env' },
    });
    expect(r.code).toBe(0);
    expect(s.requests[0]!.release).toBe('9.9.9');
    expect(s.requests[0]!.headers['x-api-key']).toBe(API_KEY);
    expect(s.requests[0]!.headers['x-project-key']).toBe('org-key-env');
  });

  it('usage errors: no command, a wrong command, missing positionals, an unknown flag, a missing dashboard url / api key, an unreadable dir, an invalid url — all exit 2 with nothing sent', async () => {
    const dir = makeTree({ count: 1 });
    const s = await stub();
    const base = ['--release', RELEASE, '--dashboard-url', s.url, '--api-key', API_KEY];
    for (const argv of [
      [...base],
      ['download', join(dir, 'static'), '/_next/static', ...base],
      ['upload', join(dir, 'static'), ...base],
      ['upload', join(dir, 'static'), '/_next/static', 'extra', ...base],
      ['upload', join(dir, 'static'), '/_next/static', '--bogus', ...base],
      ['upload', join(dir, 'static'), '/_next/static', '--release', RELEASE, '--api-key', API_KEY],
      ['upload', join(dir, 'static'), '/_next/static', '--release', RELEASE, '--dashboard-url', s.url],
      ['upload', join(dir, 'missing'), '/_next/static', ...base],
      ['upload', join(dir, 'static'), '/_next/static', '--release', RELEASE, '--dashboard-url', 'sentinel.daya.ai', '--api-key', API_KEY],
    ]) {
      const r = await cli(argv);
      expect(r.code, argv.join(' ')).toBe(2);
    }
    expect(s.requests).toHaveLength(0);
  });
});

describe('4xx — exit 1, the envelope printed, later batches not sent, nothing of the failing batch deleted (decisions 29, 34)', () => {
  const cases: Array<[string, Parameters<typeof envelope> | string]> = [
    ['401 unauthorized', [401, 'unauthorized', 'Missing API key.', {}]],
    ['403 forbidden_scope', [403, 'forbidden_scope', 'The key lacks the ingest scope.', { scope: 'read' }]],
    ['400 invalid_request', [400, 'invalid_request', 'release_version must not begin or end with whitespace.', { field: 'release_version', reason: 'whitespace' }]],
    ['422 release_full', [422, 'release_full', 'The release holds the maximum number of files.', { max: 500 }]],
    ['413 payload_too_large (envelope)', [413, 'payload_too_large', 'A file exceeds SOURCEMAP_MAX_BYTES.', { max_bytes: 52428800 }]],
    ['413 plain text (adapter-node)', 'Request Entity Too Large'],
  ];
  it.each(cases)('C6: %s on the second of three batches → exit 1, request 3 never sent, batch 2’s maps kept', async (_name, answer) => {
    const dir = makeTree({ count: 120 });
    const s = await stub((req) => {
      if (req.n !== 2) return stored(req);
      return typeof answer === 'string' ? { status: 413, body: answer, headers: { 'content-type': 'text/plain' } } : envelope(...answer);
    });
    const r = await cli(uploadArgs(dir, s.url, '--delete'));
    expect(r.code).toBe(1);
    expect(s.requests).toHaveLength(2);
    expect(r.sleeps).toEqual([]);
    if (typeof answer === 'string') {
      expect(r.stderr).toMatch(/413 Request Entity Too Large/);
    } else {
      const [status, code, message, details] = answer;
      expect(r.stderr).toContain(`${status} ${code}: ${message} ${JSON.stringify(details)}`);
    }
    // Batch 1's 50 maps were deleted after its 2xx; batch 2's and 3's 70 are all still there.
    expect(mapsUnder(join(dir, 'static'))).toHaveLength(70);
    expect(r.stdout.trim().split('\n')).toHaveLength(50);
  });

  it('C6: the code is printed, never branched on — an unknown 4xx code and a 4xx with a JSON body that is not the envelope both exit 1 at once with the body printed', async () => {
    const dir = makeTree({ count: 3 });
    const s = await stub(() => envelope(418, 'future_code', 'Something new.', { hint: 'x' }));
    const r = await cli(uploadArgs(dir, s.url));
    expect(r.code).toBe(1);
    expect(r.stderr).toContain('418 future_code: Something new. {"hint":"x"}');
    expect(s.requests).toHaveLength(1);

    const s2 = await stub(() => ({ status: 400, body: JSON.stringify({ message: 'not the envelope' }) }));
    const r2 = await cli(uploadArgs(dir, s2.url));
    expect(r2.code).toBe(1);
    expect(r2.stderr).toContain('400 {"message":"not the envelope"}');
    expect(s2.requests).toHaveLength(1);
  });

  it('a non-JSON body is printed capped at 4 KiB', async () => {
    const dir = makeTree({ count: 1 });
    const s = await stub(() => ({ status: 413, body: 'X'.repeat(10_000), headers: { 'content-type': 'text/html' } }));
    const r = await cli(uploadArgs(dir, s.url));
    expect(r.code).toBe(1);
    const printed = r.stderr.match(/413 (X+)/)?.[1] ?? '';
    expect(printed).toHaveLength(4096);
  });
});

describe('5xx and network failures — one class, retried, then exit 1 (decision 34)', () => {
  it('C7: 503 storage_unavailable twice then 201 → exit 0, three attempts counted, delays ≤ 60 s', async () => {
    const dir = makeTree({ count: 3 });
    const s = await stub((req) => (req.n <= 2 ? envelope(503, 'storage_unavailable', 'Object store unreachable.', { retry: true }) : stored(req)));
    const r = await cli(uploadArgs(dir, s.url));
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(3);
    expect(r.sleeps).toHaveLength(2);
    for (const ms of r.sleeps) expect(ms).toBeLessThanOrEqual(60_000);
    expect(r.stderr).toMatch(/attempt 1\/4 failed: 503 storage_unavailable: Object store unreachable\. \{"retry":true\}; retrying/);
    expect(r.stderr).toMatch(/attempt 2\/4 failed: 503 storage_unavailable/);
    expect(r.stderr).toMatch(/batch 1: 201, 3 file\(s\) after 3 attempts/);
    expect(r.stdout.trim().split('\n')).toHaveLength(3);
  });

  it('C7: a JSON 500 internal once then 201 → exit 0, two attempts', async () => {
    const dir = makeTree({ count: 3 });
    const s = await stub((req) => (req.n === 1 ? envelope(500, 'internal', 'Internal error.') : stored(req)));
    const r = await cli(uploadArgs(dir, s.url));
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(2);
    expect(r.stderr).toMatch(/attempt 1\/4 failed: 500 internal: Internal error\./);
    expect(r.stderr).toMatch(/after 2 attempts/);
  });

  it('C7: a plain-text 502 Bad Gateway (a proxy) once then 201 → exit 0, two attempts, the text printed', async () => {
    const dir = makeTree({ count: 3 });
    const s = await stub((req) => (req.n === 1 ? { status: 502, body: '<html>Bad Gateway</html>', headers: { 'content-type': 'text/html' } } : stored(req)));
    const r = await cli(uploadArgs(dir, s.url));
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(2);
    expect(r.stderr).toMatch(/attempt 1\/4 failed: 502 <html>Bad Gateway<\/html>; retrying/);
  });

  it('every 5xx exhausted — a 503 envelope, a 504, a plain-text ELB 503 — is 4 attempts then exit 1 with the last status printed; --delete removes nothing', async () => {
    for (const answer of [envelope(503, 'storage_unavailable', 'Object store unreachable.'), { status: 504, body: '' }, { status: 503, body: 'Service Unavailable', headers: { 'content-type': 'text/plain' } }]) {
      const dir = makeTree({ count: 2 });
      const s = await stub(() => answer);
      const r = await cli(uploadArgs(dir, s.url, '--delete'));
      expect(r.code).toBe(1);
      expect(s.requests).toHaveLength(4);
      expect(r.sleeps).toHaveLength(3);
      expect(r.stderr).toMatch(new RegExp(`${answer.status} .*after 4 attempts`));
      expect(mapsUnder(join(dir, 'static'))).toHaveLength(2);
      await s.close();
      stubs.pop();
    }
  });

  it('C8: connection refused → 4 attempts, exit 1, a line naming the reachability rule (internal ELB, self-hosted pool)', async () => {
    const dir = makeTree({ count: 2 });
    const url = await refusedUrl();
    const r = await cli(uploadArgs(dir, url));
    expect(r.code).toBe(1);
    expect(r.sleeps).toHaveLength(3);
    for (const ms of r.sleeps) expect(ms).toBeLessThanOrEqual(60_000);
    expect(r.stderr).toMatch(/connection failed \(ECONNREFUSED\) after 4 attempts/);
    expect(r.stderr).toContain(REACHABILITY_LINE);
    expect(r.stderr).toMatch(/internal ELB/);
    expect(r.stderr).toMatch(/self-hosted/);
  });

  it('the per-request deadline is a network failure of the same class: a route that never answers → ETIMEDOUT, 4 attempts, `unavailable`', async () => {
    const dir = makeTree({ count: 1 });
    const s = await stub(() => 'hold');
    const sleeps: number[] = [];
    await expect(
      uploadSourcemaps({ buildDir: join(dir, 'static'), publicPrefix: '/_next/static', release: RELEASE, dashboardUrl: s.url, apiKey: API_KEY, timeoutMs: 50, sleep: async (ms) => void sleeps.push(ms) }),
    ).rejects.toMatchObject({ name: 'SourcemapUploadError', kind: 'unavailable', attempts: 4, exitCode: 1, message: expect.stringMatching(/ETIMEDOUT/) });
    expect(sleeps).toEqual([2000, 4000, 8000]);
  });
});

describe('--dry-run and --json (contract §4.8 steps 4, 8)', () => {
  it('C9: --dry-run prints every pair and "3 request(s) would be sent" for 120 maps, exit 0, no network', async () => {
    const dir = makeTree({ count: 120 });
    const s = await stub();
    const r = await cli(uploadArgs(dir, s.url, '--dry-run'));
    expect(r.code).toBe(0);
    expect(s.requests).toHaveLength(0);
    const lines = r.stdout.trim().split('\n');
    expect(lines).toHaveLength(121);
    expect(lines[0]).toBe(`/_next/static/chunks/chunk-000.js  ${join(dir, 'static/chunks/chunk-000.js.map')}  ${Buffer.byteLength(sourcemapJson('chunk-000.js'))}`);
    expect(lines[120]).toBe(`3 request(s) would be sent for 120 map(s), release ${RELEASE}`);
  });

  it('C10: --json prints one object per stored file on stdout — { minified_path, outcome, sha256, size_bytes } — with the diagnostics on stderr', async () => {
    const dir = makeTree({ count: 3, orphan: ['static/chunks/chunk-001.js'] });
    const s = await stub((req) => stored(req, 'unchanged', 200));
    const r = await cli(uploadArgs(dir, s.url, '--json'));
    expect(r.code).toBe(0);
    const objects = r.stdout.trim().split('\n').map((l) => JSON.parse(l) as Record<string, unknown>);
    expect(objects).toHaveLength(3);
    for (const [i, o] of objects.entries()) {
      expect(Object.keys(o).sort()).toEqual(['minified_path', 'outcome', 'sha256', 'size_bytes']);
      expect(o).toMatchObject({ minified_path: `/_next/static/chunks/chunk-00${i}.js`, outcome: 'unchanged' });
    }
    expect(r.stderr).toMatch(/chunk-001\.js beside it/);
    expect(r.stderr).toMatch(/3 file\(s\) stored/);
  });

  it('C9 + C10: --dry-run --json prints the pairs as objects on stdout and the count on stderr', async () => {
    const dir = makeTree({ count: 2 });
    const r = await cli(uploadArgs(dir, 'http://stub.invalid', '--dry-run', '--json'));
    expect(r.code).toBe(0);
    const objects = r.stdout.trim().split('\n').map((l) => JSON.parse(l) as Record<string, unknown>);
    expect(objects.map((o) => Object.keys(o).sort())).toEqual([['minified_path', 'path', 'size_bytes'], ['minified_path', 'path', 'size_bytes']]);
    expect(r.stderr).toMatch(/1 request\(s\) would be sent/);
  });
});

describe('C11: the key is never printed', () => {
  it('greps stdout + stderr of a success, a 401, an exhausted 5xx, a refused connection, a dry run, a --json run and a usage error — and of every run in this file', async () => {
    const dir = makeTree({ count: 3 });
    const ok = await stub();
    const rows: RunResult[] = [];
    rows.push(await cli(uploadArgs(dir, ok.url)));
    rows.push(await cli(uploadArgs(dir, ok.url, '--json')));
    rows.push(await cli(uploadArgs(dir, ok.url, '--dry-run')));
    const denied = await stub(() => envelope(401, 'unauthorized', 'Missing API key.'));
    rows.push(await cli(uploadArgs(dir, denied.url)));
    const down = await stub(() => envelope(503, 'storage_unavailable', 'Down.'));
    rows.push(await cli(uploadArgs(dir, down.url)));
    rows.push(await cli(uploadArgs(dir, await refusedUrl())));
    rows.push(await cli(uploadArgs(dir, ok.url, '--batch-size', 'many')));
    rows.push(await cli(['upload', join(dir, 'static'), '/_next/static', '--release', RELEASE, '--dashboard-url', ok.url, '--api-key', API_KEY, '--bogus']));
    expect(rows.map((r) => r.code)).toEqual([0, 0, 0, 1, 1, 1, 2, 2]);
    for (const r of [...rows, ...outputs]) {
      expect(r.stdout).not.toContain(API_KEY);
      expect(r.stderr).not.toContain(API_KEY);
    }
  });
});

describe('the bin (contract §4.1, §4.8) — node dist/cli/index.js', () => {
  const bin = join(root, 'dist/cli/index.js');

  it(`upload --help prints the usage and exits 0 (${process.version})`, () => {
    const r = spawnSync(process.execPath, [bin, 'upload', '--help'], { encoding: 'utf8' });
    expect(r.status).toBe(0);
    expect(r.stdout).toBe(USAGE);
  });

  it(`upload --dry-run on the fixture tree prints the seven pairs and one request, exits 0 (${process.version})`, () => {
    const r = spawnSync(process.execPath, [bin, 'upload', FIXTURE_STATIC, '/_next/static', '--dry-run', '--release', RELEASE, '--dashboard-url', 'http://localhost:5173', '--api-key', API_KEY], {
      encoding: 'utf8',
    });
    expect(r.status).toBe(0);
    const lines = r.stdout.trim().split('\n');
    expect(lines).toHaveLength(8);
    expect(lines[2]).toMatch(/^\/_next\/static\/chunks\/app\/page-9b0d\.js  /);
    expect(lines[7]).toBe(`1 request(s) would be sent for 7 map(s), release ${RELEASE}`);
    expect(r.stdout + r.stderr).not.toContain(API_KEY);
  });

  it(`upload --dry-run on the Turbopack tree prints the three chunk paths, exits 0 (${process.version})`, () => {
    const r = spawnSync(process.execPath, [bin, 'upload', join(TURBOPACK, 'static'), '/_next/static', '--dry-run', '--release', RELEASE, '--dashboard-url', 'http://localhost:5173', '--api-key', API_KEY], {
      encoding: 'utf8',
    });
    expect(r.status).toBe(0);
    const lines = r.stdout.trim().split('\n');
    expect(lines).toHaveLength(4);
    expect(lines[1]).toMatch(/^\/_next\/static\/chunks\/d69eb7d85499027c\.js  .*8f15d5d03090a0be\.js\.map  /);
    expect(lines[3]).toBe(`1 request(s) would be sent for 3 map(s), release ${RELEASE}`);
    expect(r.stderr).toMatch(/warning: /);
  });

  it('with no arguments exits 2 and points at --help', () => {
    const r = spawnSync(process.execPath, [bin], { encoding: 'utf8' });
    expect(r.status).toBe(2);
    expect(r.stderr).toMatch(/--help/);
  });

  it('calls the uploader in-process: no child_process import under src/cli, src/upload, src/nextjs/config.ts, nor in the built bin and config entries', () => {
    const files = [
      ...readdirSync(join(root, 'src/cli')).map((f) => join(root, 'src/cli', f)),
      ...readdirSync(join(root, 'src/upload')).map((f) => join(root, 'src/upload', f)),
      join(root, 'src/nextjs/config.ts'),
      join(root, 'dist/cli/index.js'),
      join(root, 'dist/nextjs/config.js'),
    ];
    expect(files.length).toBeGreaterThanOrEqual(9);
    for (const file of files) expect(readFileSync(file, 'utf8'), file).not.toMatch(/child_process/);
  });
});
