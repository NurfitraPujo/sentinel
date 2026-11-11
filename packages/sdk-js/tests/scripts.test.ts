// scripts/size.mjs (contract §4.1's pinned method, §11, flows D3 — report-only, decision 40) and
// scripts/assert-no-dependencies.mjs (§11's gate), each driven as a black box through `node` in a temp
// directory, so the assertions are on what `pnpm size` / `pnpm check:no-dependencies` print and exit with.
// Order-independent: every case builds its own temp directory.

import { spawnSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';
import { afterEach, describe, expect, it } from 'vitest';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const dirs: string[] = [];

function scratch(): string {
  const dir = mkdtempSync(join(tmpdir(), 'sdk-js-scripts-'));
  dirs.push(dir);
  return dir;
}

function run(script: string, cwd: string) {
  const r = spawnSync(process.execPath, [join(root, 'scripts', script)], { cwd, encoding: 'utf8' });
  return { status: r.status, stdout: r.stdout, stderr: r.stderr };
}

afterEach(() => {
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});

describe('scripts/size.mjs', () => {
  it('prints the default-level gzip byte count of the raw dist/nextjs/client.js and exits 0', () => {
    const dir = scratch();
    mkdirSync(join(dir, 'dist/nextjs'), { recursive: true });
    const content = 'export const x = 1;\n'.repeat(200);
    writeFileSync(join(dir, 'dist/nextjs/client.js'), content);
    const expected = gzipSync(Buffer.from(content)).length;

    const r = run('size.mjs', dir);
    expect(r.status).toBe(0);
    expect(r.stdout).toContain(`sdk_js_client_entry_gzip_bytes=${expected}`);
    expect(r.stdout).toContain('dist/nextjs/client.js');
  });

  it('prints a note naming the missing dist/nextjs/client.js and exits 0 (the one no-measurement outcome)', () => {
    const dir = scratch();
    const r = run('size.mjs', dir);
    expect(r.status).toBe(0);
    expect(r.stdout).toContain('dist/nextjs/client.js');
    expect(r.stdout).not.toContain('sdk_js_client_entry_gzip_bytes=');
  });
});

describe('scripts/assert-no-dependencies.mjs', () => {
  it('exits 0 when package.json has no dependencies key', () => {
    const dir = scratch();
    writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'x', devDependencies: { a: '1.0.0' } }));
    const r = run('assert-no-dependencies.mjs', dir);
    expect(r.status).toBe(0);
  });

  it('exits 1 when a dependencies key is present — even empty', () => {
    const dir = scratch();
    writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'x', dependencies: {} }));
    const r = run('assert-no-dependencies.mjs', dir);
    expect(r.status).toBe(1);
    expect(r.stderr).toContain('dependencies');
  });
});
