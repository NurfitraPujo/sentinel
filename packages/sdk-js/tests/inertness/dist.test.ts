// Import-time inertness (contract §4.7, §11; flows D7), on the BUILT entries: dist/browser.js and
// dist/nextjs/client.js evaluated under node with no `window`; dist/node.js, dist/edge.js, dist/nextjs/server.js,
// dist/nextjs/edge.js and dist/nextjs/route.js under a globalThis with `window` and `process` deleted. Each is
// import()ed by a bare node child (tests/inertness/probe.mjs — node's own loader, so vitest's module runner
// cannot mask a module body's side effect) with counters on `addEventListener`, `process.on`, `setTimeout` /
// `setInterval`, `fetch` and every `process.env` read: all untouched by the import, and — the other half —
// touched by `init` where the entry has one. Runs under whatever node runs the suite (the node 20 / 22 matrix).

import { spawnSync } from 'node:child_process';
import { existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const probe = join(root, 'tests/inertness/probe.mjs');

interface Counts {
  addEventListener: number;
  processOn: number;
  setTimeout: number;
  setInterval: number;
  fetch: number;
  envReads: string[];
}

interface Probe {
  file: string;
  mode: string;
  globalsAtImport: { window: string; process: string };
  importError: string | null;
  exports: string[];
  afterImport: Counts;
  initError: string | null;
  initReports: string[];
  afterInit: Counts;
}

const UNTOUCHED: Counts = { addEventListener: 0, processOn: 0, setTimeout: 0, setInterval: 0, fetch: 0, envReads: [] };

function run(file: string, mode: 'node' | 'no-globals'): Probe {
  const path = join(root, file);
  expect(existsSync(path), `${file} is missing: run pnpm build first`).toBe(true);
  const r = spawnSync(process.execPath, [probe, path, mode], { encoding: 'utf8', timeout: 20_000, env: { ...process.env, SENTINEL_ENDPOINT: 'http://env.example/ingest' } });
  expect(r.status, `probe exited ${String(r.status)}:\n${r.stderr}`).toBe(0);
  return JSON.parse(r.stdout.trim().split('\n').pop() ?? '{}') as Probe;
}

describe(`D7: dist/browser.js and dist/nextjs/client.js under node with no window (${process.version})`, () => {
  it.each(['dist/browser.js', 'dist/nextjs/client.js'])('D7: %s evaluates without throwing and registers nothing — no addEventListener, process.on, timer, fetch or process.env read until init', (file) => {
    const p = run(file, 'node');
    expect(p.importError).toBeNull();
    expect(p.globalsAtImport.window).toBe('undefined');
    expect(p.afterImport).toEqual(UNTOUCHED);
    expect(p.initError).toBeNull();
  });

  it('D7: the browser entry’s init under node degrades to the no-op client (no origin to be same-origin with) and still installs nothing', () => {
    const p = run('dist/browser.js', 'node');
    expect(p.exports).toContain('init');
    expect(p.initReports).toEqual(['invalid_option:tunnel']);
    expect(p.afterInit).toEqual(UNTOUCHED);
  });
});

describe(`D7: every runtime entry under a plain node global — process present and its env proxied, so a module-body env read is visible (${process.version})`, () => {
  it.each(['dist/core/index.js', 'dist/browser.js', 'dist/node.js', 'dist/edge.js', 'dist/nextjs/client.js', 'dist/nextjs/server.js', 'dist/nextjs/edge.js', 'dist/nextjs/route.js', 'dist/nextjs/config.js'])(
    'D7: %s reads no process.env key, registers no process listener, sets no timer and calls no fetch at import',
    (file) => {
      const p = run(file, 'node');
      expect(p.importError).toBeNull();
      expect(p.afterImport).toEqual(UNTOUCHED);
    },
  );
});

describe(`D7: dist/node.js, dist/edge.js and the nextjs server / edge / route builds under a globalThis with window and process deleted (${process.version})`, () => {
  it.each(['dist/node.js', 'dist/edge.js', 'dist/nextjs/server.js', 'dist/nextjs/edge.js', 'dist/nextjs/route.js'])('D7: %s evaluates without throwing and registers nothing until init', (file) => {
    const p = run(file, 'no-globals');
    expect(p.importError).toBeNull();
    expect(p.globalsAtImport).toEqual({ window: 'undefined', process: 'undefined' });
    expect(p.afterImport).toEqual(UNTOUCHED);
    expect(p.initError).toBeNull();
  });

  it('D7 (the other half): after init the node entry reads SENTINEL_PROJECT_KEY and installs its two process hooks; the edge entry reads the variable and installs nothing', () => {
    const node = run('dist/node.js', 'no-globals');
    expect(node.initReports).toEqual([]);
    expect(node.afterInit.processOn).toBe(2);
    expect(node.afterInit.envReads).toContain('SENTINEL_PROJECT_KEY');
    expect(node.afterInit.addEventListener).toBe(0);
    expect(node.afterInit.fetch).toBe(0);
    const edge = run('dist/edge.js', 'no-globals');
    expect(edge.initReports).toEqual([]);
    expect(edge.afterInit.processOn).toBe(0);
    expect(edge.afterInit.envReads).toContain('SENTINEL_PROJECT_KEY');
    expect(edge.afterInit.addEventListener).toBe(0);
    expect(edge.afterInit.fetch).toBe(0);
  });
});
