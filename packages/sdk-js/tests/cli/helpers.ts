// Helpers for the CLI rows (contract §4.8, §11 *The CLI's walker and pairing*; flows C1–C13): a temp-dir
// fixture tree builder, a `node:http` stub of `POST /api/sourcemaps` that parses the multipart body the way the
// route does (`Response.formData()` — undici's parser, the same one SvelteKit's `request.formData()` uses) and
// answers per a script, and an in-process runner around `runCli` that captures stdout / stderr and injects a
// no-op `sleep`, recording the delays the ladder asked for so they can be asserted ≤ 60 s.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { cpSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import type { AddressInfo } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { type CliIo, runCli } from '../../src/cli/main.js';
import type { SendFn, UploadRequest } from '../../src/upload/index.js';

export const API_KEY = 'sk_test_never_printed_9f8e7d6c5b4a';
export const RELEASE = '2.34.0';

/** A minimal but valid sourcemap v3 (what the route's structural check reads), ≈ 150 bytes. */
export function sourcemapJson(file: string): string {
  return `${JSON.stringify({ version: 3, file, sources: [`src/${file}.ts`], names: ['x'], mappings: 'AAAA,MAAMA,CAAC' })}\n`;
}

export interface TreeSpec {
  /** Client maps under `static/chunks/`, named `chunk-<i>.js`. */
  count?: number;
  /** Also drop a `static/css/styles.css.map`. */
  css?: boolean;
  /** Extra relative map paths (without the `.map`) to create, e.g. `static/chunks/app/page-9b0d.js`. */
  extra?: string[];
  /** Server maps under `server/`, named `app/route-<i>.js`. */
  server?: number;
  /** Write no sibling `.js` for these relative paths. */
  orphan?: string[];
}

const scratchDirs: string[] = [];

/** A fresh temp dir holding a `.next`-shaped tree; removed by `cleanupTrees`. Returns the dir that stands for `distDir`. */
export function makeTree(spec: TreeSpec = {}): string {
  const dir = mkdtempSync(join(tmpdir(), 'sdk-js-cli-'));
  scratchDirs.push(dir);
  const rels: string[] = [];
  for (let i = 0; i < (spec.count ?? 0); i++) rels.push(`static/chunks/chunk-${String(i).padStart(3, '0')}.js`);
  for (let i = 0; i < (spec.server ?? 0); i++) rels.push(`server/app/route-${String(i).padStart(3, '0')}.js`);
  rels.push(...(spec.extra ?? []));
  for (const rel of rels) {
    const js = join(dir, rel);
    mkdirSync(dirname(js), { recursive: true });
    if (!(spec.orphan ?? []).includes(rel)) writeFileSync(js, `export const x=1;\n//# sourceMappingURL=${rel.split('/').pop()}.map\n`);
    writeFileSync(`${js}.map`, sourcemapJson(rel.split('/').pop() ?? rel));
  }
  if (spec.css) {
    mkdirSync(join(dir, 'static/css'), { recursive: true });
    writeFileSync(join(dir, 'static/css/styles.css'), 'body{margin:0}\n');
    writeFileSync(join(dir, 'static/css/styles.css.map'), `${JSON.stringify({ version: 3, file: 'styles.css', sources: ['src/globals.css'], mappings: 'AAAA' })}\n`);
  }
  return dir;
}

/** A fresh temp copy of a committed fixture tree (`tests/fixtures-build/<name>/.next`), for the rows that delete; removed by `cleanupTrees`. */
export function copyTree(fixtureDistDir: string): string {
  const dir = mkdtempSync(join(tmpdir(), 'sdk-js-fixture-'));
  scratchDirs.push(dir);
  cpSync(fixtureDistDir, dir, { recursive: true });
  return dir;
}

export function cleanupTrees(): void {
  for (const dir of scratchDirs.splice(0)) rmSync(dir, { recursive: true, force: true });
}

export interface StubRequest {
  readonly n: number;
  readonly method: string;
  readonly url: string;
  readonly headers: IncomingMessage['headers'];
  /** The declared `Content-Length`, as a number. */
  readonly contentLength: number;
  /** Bytes actually received. */
  readonly receivedBytes: number;
  /** The parsed form — `release_version`, the positional `minified_path` parts and the `file` parts. */
  readonly release: string | null;
  readonly paths: string[];
  readonly files: Array<{ name: string; size: number; text: string }>;
}

export interface StubResponse {
  status: number;
  body?: string;
  headers?: Record<string, string>;
}

/** Decides the answer for request `n` (1-based) — after the body was read and parsed; `'hold'` never answers (the deadline row). */
export type Responder = (req: StubRequest) => StubResponse | 'hold';

/** The route's success body for the request's own pairs, `201` with every file `created` unless told otherwise. */
export function stored(req: StubRequest, outcome = 'created', status = 201): StubResponse {
  return {
    status,
    body: JSON.stringify({
      project_id: 'proj_1',
      release_version: req.release,
      files: req.paths.map((minified_path, i) => ({
        minified_path,
        sha256: `${(i + 1).toString(16).padStart(4, '0')}${'ab'.repeat(30)}`,
        size_bytes: req.files[i]?.size ?? 0,
        outcome,
      })),
      release_file_count: req.paths.length,
    }),
  };
}

export function envelope(status: number, code: string, message: string, details: Record<string, unknown> = {}): StubResponse {
  return { status, body: JSON.stringify({ error: { code, message, details } }) };
}

export interface SourcemapStub {
  readonly url: string;
  readonly requests: StubRequest[];
  respond(responder: Responder): void;
  close(): Promise<void>;
}

export async function startSourcemapStub(responder: Responder = (req) => stored(req)): Promise<SourcemapStub> {
  const requests: StubRequest[] = [];
  let current = responder;
  const server: Server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      void (async () => {
        const body = Buffer.concat(chunks);
        let release: string | null = null;
        const paths: string[] = [];
        const files: StubRequest['files'] = [];
        try {
          const form = await new Response(body, { headers: { 'content-type': req.headers['content-type'] ?? '' } }).formData();
          const r = form.get('release_version');
          release = typeof r === 'string' ? r : null;
          for (const p of form.getAll('minified_path')) if (typeof p === 'string') paths.push(p);
          for (const f of form.getAll('file')) {
            if (f instanceof File) files.push({ name: f.name, size: f.size, text: await f.text() });
          }
        } catch {
          // not multipart — recorded with empty parts
        }
        const entry: StubRequest = {
          n: requests.length + 1,
          method: req.method ?? '',
          url: req.url ?? '',
          headers: req.headers,
          contentLength: Number(req.headers['content-length'] ?? Number.NaN),
          receivedBytes: body.byteLength,
          release,
          paths,
          files,
        };
        requests.push(entry);
        const answer = current(entry);
        if (answer === 'hold') return;
        res.writeHead(answer.status, { 'content-type': 'application/json', ...(answer.headers ?? {}) });
        res.end(answer.body ?? '');
      })();
    });
  });
  server.keepAliveTimeout = 100;
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve));
  const { port } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${port}`,
    requests,
    respond(next) {
      current = next;
    },
    close() {
      return new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      });
    },
  };
}

/** A `http://127.0.0.1:<port>` nobody listens on — the connection-refused row (C8). */
export async function refusedUrl(): Promise<string> {
  const probe = createServer();
  await new Promise<void>((resolve) => probe.listen(0, '127.0.0.1', resolve));
  const { port } = probe.address() as AddressInfo;
  await new Promise<void>((resolve) => probe.close(() => resolve()));
  return `http://127.0.0.1:${port}`;
}

export interface RunResult {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
  /** The delays the ladder slept, in ms — guidance, asserted only as ≤ 60 s. */
  readonly sleeps: number[];
}

export interface RunOptions {
  env?: Record<string, string | undefined>;
  send?: SendFn;
}

/** `runCli` in-process: no real sleep, stdout / stderr captured. */
export async function run(argv: string[], opts: RunOptions = {}): Promise<RunResult> {
  let stdout = '';
  let stderr = '';
  const sleeps: number[] = [];
  const io: CliIo = {
    stdout: (t) => {
      stdout += t;
    },
    stderr: (t) => {
      stderr += t;
    },
    env: opts.env ?? {},
    send: opts.send,
    sleep: async (ms) => {
      sleeps.push(ms);
    },
  };
  const code = await runCli(argv, io);
  return { code, stdout, stderr, sleeps };
}

/** The common `upload <dir>/static /_next/static --release … --dashboard-url … --api-key …` argv. */
export function uploadArgs(dir: string, dashboardUrl: string, ...more: string[]): string[] {
  return ['upload', join(dir, 'static'), '/_next/static', '--release', RELEASE, '--dashboard-url', dashboardUrl, '--api-key', API_KEY, ...more];
}

/** A `SendFn` that records the declared `Content-Length` and never reads a byte of body (C13's synthetic sizes). */
export function recordingSend(answer: (req: UploadRequest, n: number) => { status: number; body: string }): { send: SendFn; requests: UploadRequest[] } {
  const requests: UploadRequest[] = [];
  const send: SendFn = async (req) => {
    requests.push(req);
    return answer(req, requests.length);
  };
  return { send, requests };
}
