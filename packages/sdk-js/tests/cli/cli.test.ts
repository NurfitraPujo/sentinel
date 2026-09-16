// The CLI rows of contract §4.8 (flows C1–C13; §11 *The CLI's walker and pairing*) against a stubbed
// `POST /api/sourcemaps`: the pairing, batches by count and by bytes, the exit codes, the failure classes of
// decision 34 (every 5xx and network failure one retry-then-exit-1 class; the code printed, never branched on),
// `--delete` per successful batch, the release refusals before any request, `--dry-run` / `--json`, the
// `--batch-size` posture, and C11's secrecy grep. `runCli` runs in-process with the ladder's sleeps recorded and
// skipped; C13's synthetic sizes go through a recording `send` that reads the declared `Content-Length` and never
// a byte of body. Shuffle-safe: every test builds its own tree and stub.

import { closeSync, existsSync, ftruncateSync, mkdirSync, openSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, describe, expect, it } from 'vitest';
import { REACHABILITY_LINE, USAGE } from '../../src/cli/main.js';
import { MAX_REQUEST_BYTES, uploadSourcemaps, walkSourcemaps } from '../../src/upload/index.js';
import {
  API_KEY,
  cleanupTrees,
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
    expect(pairs.filter((p) => !p.minifiedPath.endsWith('webpack-9c0d.js')).every((p) => p.siblingExists)).toBe(true);
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
