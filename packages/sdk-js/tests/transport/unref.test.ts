// Contract §4.4 *Node process exit*: timers are unref()ed so an idle SDK never keeps a process alive (N9's last
// clause). Proven by spawning node on a bundled script (tests/helpers/idle-process.ts) whose client sits in the
// ladder's backoff sleep against a refused endpoint: the process must exit as soon as its own ref'd timer fires,
// with no second attempt on stderr. Runs under whatever node runs the suite — the node 20 / 22 matrix.

import { build } from 'esbuild';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { refusedUrl } from '../helpers/stub-server.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
let dir = '';
let script = '';

beforeAll(async () => {
  dir = mkdtempSync(join(tmpdir(), 'sdk-js-unref-'));
  script = join(dir, 'idle-process.mjs');
  await build({
    absWorkingDir: root,
    entryPoints: ['tests/helpers/idle-process.ts'],
    bundle: true,
    format: 'esm',
    platform: 'node',
    target: 'node20',
    outfile: script,
    logLevel: 'silent',
  });
});

afterAll(() => {
  rmSync(dir, { recursive: true, force: true });
});

function run(mode: string, endpoint: string) {
  const started = performance.now();
  const r = spawnSync(process.execPath, [script, mode, endpoint], { encoding: 'utf8', timeout: 20_000 });
  return { status: r.status, signal: r.signal, stdout: r.stdout, stderr: r.stderr, elapsedMs: performance.now() - started };
}

describe('N9 (tail): timers are unref()ed — a node process with an idle client exits', () => {
  it('a client that captured nothing does not keep the process alive', async () => {
    const r = run('idle', await refusedUrl());
    expect(r.signal).toBeNull();
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('alive-timer fired');
    expect(r.stderr).not.toContain('sending');
  });

  it('a client mid-backoff against a refused endpoint exits when the host is done: attempt 1 on stderr, never attempt 2', async () => {
    const r = run('capture', await refusedUrl());
    expect(r.signal).toBeNull();
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('alive-timer fired');
    expect(r.stderr).toContain('attempt 1/4');
    expect(r.stderr).toContain('retrying 1 event(s) in');
    expect(r.stderr).not.toContain('attempt 2/4');
    // The ladder's first sleep is ≈ 1 s and the host's timer 300 ms: a ref'd sleep would hold the process past it.
    expect(r.elapsedMs).toBeLessThan(5000);
  });
});
