// Node exit behaviour (contract §4.6, §11 *Node exit behaviour*; flows N5, N6, N7, N16): spawns node on the
// three scripts under tests/node-exit/scripts/ (bundled with esbuild first, one file per row so the with- and
// without-`init` runs share every file:line:col) and compares exit codes and stderr. Runs under whatever node
// runs the suite — `process.execPath`, the node 20 / node 22 matrix; locally `fnm exec --using=20` and `=22`.
// The stub ingestor is N7's only: on the sole-listener path nothing is ever delivered (§4.6), so N5 / N6 read
// `metadata.source` from the file the synchronous `beforeSend` wrote. Set SDK_JS_EXIT_VERBOSE=1 to print every
// run's stdout / stderr / exit code (what status/be.md pastes).

import { build } from 'esbuild';
import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, unlinkSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterAll, beforeAll, describe, expect, it, vi } from 'vitest';

vi.setConfig({ testTimeout: 30_000 });
import type { SentinelEvent } from '../../src/core/types.js';
import { startStub, type StubServer } from '../helpers/stub-server.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const SCRIPTS = ['uncaught', 'rejection', 'host-listener'] as const;
type Script = (typeof SCRIPTS)[number];
const verbose = process.env.SDK_JS_EXIT_VERBOSE === '1';

let dir = '';
const bundled: Record<Script, string> = { uncaught: '', rejection: '', 'host-listener': '' };
let runs = 0;

beforeAll(async () => {
  dir = mkdtempSync(join(tmpdir(), 'sdk-js-exit-'));
  for (const name of SCRIPTS) {
    bundled[name] = join(dir, `${name}.mjs`);
    await build({
      absWorkingDir: root,
      entryPoints: [`tests/node-exit/scripts/${name}.ts`],
      bundle: true,
      format: 'esm',
      platform: 'node',
      target: 'node20',
      outfile: bundled[name],
      logLevel: 'silent',
    });
  }
});

afterAll(() => {
  rmSync(dir, { recursive: true, force: true });
});

interface Run {
  status: number | null;
  signal: string | null;
  stdout: string;
  stderr: string;
  /** The event the synchronous `beforeSend` wrote, when it ran. */
  event: SentinelEvent | undefined;
  elapsedMs: number;
}

interface RunOptions {
  init: boolean;
  nodeArgs?: string[];
  nodeOptions?: string;
  endpoint?: string;
}

/**
 * Spawn `node` on the bundled script and wait for it to exit — asynchronously, never `spawnSync`: N7's stub
 * ingestor lives in this process, and a blocked event loop could not answer the child's POST (the child would
 * sit in its 5 s request timeout instead of exiting when its own timer fires).
 */
async function run(script: Script, opts: RunOptions): Promise<Run> {
  const eventFile = join(dir, `event-${++runs}.json`);
  const env: NodeJS.ProcessEnv = { ...process.env, SENTINEL_TEST_INIT: opts.init ? '1' : '0', SENTINEL_TEST_EVENT_FILE: eventFile };
  delete env.NODE_OPTIONS;
  if (opts.nodeOptions !== undefined) env.NODE_OPTIONS = opts.nodeOptions;
  if (opts.endpoint !== undefined) env.SENTINEL_TEST_ENDPOINT = opts.endpoint;
  const started = performance.now();
  const r = await new Promise<{ status: number | null; signal: NodeJS.Signals | null; stdout: string; stderr: string }>((resolvePromise, reject) => {
    const child = spawn(process.execPath, [...(opts.nodeArgs ?? []), bundled[script]], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let stdout = '';
    let stderr = '';
    child.stdout.setEncoding('utf8').on('data', (c: string) => (stdout += c));
    child.stderr.setEncoding('utf8').on('data', (c: string) => (stderr += c));
    const killer = setTimeout(() => child.kill('SIGKILL'), 20_000);
    child.on('error', reject);
    child.on('close', (status, signal) => {
      clearTimeout(killer);
      resolvePromise({ status, signal, stdout, stderr });
    });
  });
  const elapsedMs = Math.round(performance.now() - started);
  const event = existsSync(eventFile) ? (JSON.parse(readFileSync(eventFile, 'utf8')) as SentinelEvent) : undefined;
  if (existsSync(eventFile)) unlinkSync(eventFile);
  const out: Run = { status: r.status, signal: r.signal, stdout: r.stdout, stderr: r.stderr, event, elapsedMs };
  if (verbose) {
    const label = `${script} init=${opts.init ? 'on' : 'off'}${opts.nodeArgs ? ' ' + opts.nodeArgs.join(' ') : ''}${opts.nodeOptions ? ' NODE_OPTIONS=' + opts.nodeOptions : ''}`;
    process.stdout.write(
      `\n===== ${label} (${process.version}) =====\nexit: ${String(r.status)} signal: ${String(r.signal)} elapsed: ${elapsedMs} ms\n--- stdout ---\n${r.stdout}--- stderr ---\n${r.stderr}--- event file ---\n${event === undefined ? '(none)' : JSON.stringify({ event_id: event.event_id, error_class: event.error_class, message: event.message, source: event.metadata.source })}\n`,
    );
  }
  return out;
}

/** Everything from the first `Error:` line down (contract §4.6: the header above it may name the SDK's re-raise). */
function fromFirstErrorLine(stderr: string): string {
  const m = /^Error: /m.exec(stderr);
  expect(m, `no "Error:" line in stderr:\n${stderr}`).not.toBeNull();
  return stderr.slice(m?.index ?? 0);
}

function header(stderr: string): string {
  const m = /^Error: /m.exec(stderr);
  return stderr.slice(0, m?.index ?? stderr.length);
}

describe(`N5: an uncaught exception, with and without init (${process.version})`, () => {
  it('N5: exit code and stderr identical between the two runs; the event was captured with metadata.source uncaughtException, read from the written file', async () => {
    const off = await run('uncaught', { init: false });
    const on = await run('uncaught', { init: true });
    expect(off.signal).toBeNull();
    expect(on.signal).toBeNull();
    expect(off.status).toBe(1);
    expect(on.status).toBe(off.status);
    expect(on.stderr).toBe(off.stderr);
    expect(on.stdout).toBe(off.stdout);
    expect(off.event).toBeUndefined();
    expect(on.event).toBeDefined();
    expect(on.event).toMatchObject({ error_class: 'Error', message: 'x', platform: 'javascript', metadata: { source: 'uncaughtException', runtime: 'node' } });
    expect(on.event?.stacktrace.length).toBeGreaterThan(0);
  });
});

describe(`N6: an unhandled rejection (Error reason), sole listener, default --unhandled-rejections=throw (${process.version})`, () => {
  it('N6: captured once with metadata.source unhandledRejection (read from the file); re-raised on next tick; exit code identical; stderr identical from the first Error: line down, the header naming the SDK re-raise', async () => {
    const off = await run('rejection', { init: false });
    const on = await run('rejection', { init: true });
    expect(off.signal).toBeNull();
    expect(on.signal).toBeNull();
    expect(off.status).toBe(1);
    expect(on.status).toBe(off.status);
    expect(fromFirstErrorLine(on.stderr)).toBe(fromFirstErrorLine(off.stderr));
    expect(fromFirstErrorLine(off.stderr)).toMatch(/^Error: y\n/);
    // The source-context header: the script's own line without the SDK, the SDK's re-raise with it.
    expect(header(off.stderr)).toContain('Promise.reject(new Error("y"))');
    expect(header(on.stderr)).toContain('throw reason');
    expect(on.event).toMatchObject({ error_class: 'Error', message: 'y', metadata: { source: 'unhandledRejection', runtime: 'node' } });
    expect(on.event?.metadata.source).not.toBe('uncaughtException');
    expect(off.event).toBeUndefined();
  });
});

describe(`N7: the host has its own unhandledRejection listener; the process is kept alive past batchWaitMs (${process.version})`, () => {
  let stub: StubServer;
  beforeAll(async () => {
    stub = await startStub();
  });
  afterAll(async () => {
    await stub.close();
  });

  it('N7: captured (the sync beforeSend ran), NOT re-raised, the host listener honoured, exit code identical; the batch reaches the stub ingestor with metadata.source unhandledRejection', async () => {
    const off = await run('host-listener', { init: false, endpoint: `${stub.url}/ingest` });
    expect(off.status).toBe(0);
    expect(off.stdout).toContain('host listener ran: z');
    expect(off.stdout).toContain('alive-timer fired');
    expect(stub.requests).toHaveLength(0);

    const on = await run('host-listener', { init: true, endpoint: `${stub.url}/ingest` });
    expect(on.signal).toBeNull();
    // The batch was answered and the SDK held nothing ref'd: the process left when its own 1.5 s timer fired,
    // not at the transport's 5 s request timeout.
    expect(on.elapsedMs).toBeLessThan(4000);
    expect(on.status).toBe(off.status);
    expect(on.stdout).toBe(off.stdout);
    expect(on.stderr).toBe(off.stderr);
    expect(on.event).toMatchObject({ message: 'z', metadata: { source: 'unhandledRejection' } });
    const [req] = await stub.waitFor(1, 1000);
    expect(req?.url).toBe('/ingest/batch');
    expect(req?.headers['x-api-key']).toBe('test-key');
    expect(req?.events).toHaveLength(1);
    expect(req?.events[0]).toMatchObject({ event_id: on.event?.event_id, error_class: 'Error', message: 'z', metadata: { source: 'unhandledRejection', runtime: 'node' } });
    expect(stub.requests).toHaveLength(1);
  });
});

describe(`N16: N6's script under --unhandled-rejections=warn — documented behaviour, asserted as such (${process.version})`, () => {
  it.each([
    ['the flag', { nodeArgs: ['--unhandled-rejections=warn'] }],
    ['NODE_OPTIONS', { nodeOptions: '--unhandled-rejections=warn' }],
  ])('N16 via %s: without init the process exits 0 with node’s warning; with init the sole-listener re-raise makes it exit 1 (the SDK does not detect the mode)', async (_label, extra) => {
    const off = await run('rejection', { init: false, ...extra });
    const on = await run('rejection', { init: true, ...extra });
    expect(off.signal).toBeNull();
    expect(on.signal).toBeNull();
    expect(off.status).toBe(0);
    expect(off.stderr).toMatch(/UnhandledPromiseRejectionWarning|Unhandled promise rejection/);
    expect(on.status).toBe(1);
    expect(fromFirstErrorLine(on.stderr)).toMatch(/^Error: y\n/);
    expect(header(on.stderr)).toContain('throw reason');
    expect(on.event).toMatchObject({ message: 'y', metadata: { source: 'unhandledRejection' } });
  });
});
