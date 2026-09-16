// The package contract of contract §4.1 (flows D1) as a test: the exports table, `bin`, `engines`, no
// `dependencies`, exact-pinned devDependencies, `prepare` through node / npm — and, once built, every
// subpath resolving to a dist file with its `.d.ts` beside it and every runtime entry `import()`ing cleanly
// (contract §11 "Import-time inertness", the module-load half). Order-independent: reads files only.

import { existsSync, readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { describe, expect, it } from 'vitest';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')) as {
  name: string;
  type: string;
  exports: Record<string, string | Record<string, string>>;
  bin: Record<string, string>;
  engines: Record<string, string>;
  scripts: Record<string, string>;
  devDependencies: Record<string, string>;
} & Record<string, unknown>;

// Contract §4.1's table, in its order.
const SUBPATHS = [
  '.',
  './browser',
  './node',
  './edge',
  './nextjs/client',
  './nextjs/server',
  './nextjs/edge',
  './nextjs',
  './nextjs/route',
  './nextjs/config',
  './package.json',
];

const NEXTJS_CONDITIONS: Record<string, string> = {
  types: './dist/nextjs/index.d.ts',
  browser: './dist/nextjs/client.js',
  'edge-light': './dist/nextjs/edge.js',
  worker: './dist/nextjs/edge.js',
  workerd: './dist/nextjs/edge.js',
  node: './dist/nextjs/server.js',
  default: './dist/nextjs/server.js',
};

/** Every `.js` target the exports table can resolve to — the runtime entries (not the bin). */
function runtimeEntryFiles(): string[] {
  const files = new Set<string>();
  for (const [subpath, target] of Object.entries(pkg.exports)) {
    if (subpath === './package.json') continue;
    const targets = typeof target === 'string' ? [target] : Object.values(target);
    for (const t of targets) if (t.endsWith('.js')) files.add(t);
  }
  return [...files].sort();
}

describe('package.json (contract §4.1, flows D1)', () => {
  it('is @sentinel/sdk-js, ESM', () => {
    expect(pkg.name).toBe('@sentinel/sdk-js');
    expect(pkg.type).toBe('module');
  });

  it('carries exactly the eleven subpaths of the exports table', () => {
    expect(Object.keys(pkg.exports)).toEqual(SUBPATHS);
  });

  it('exports the three nextjs init entries as fixed files with no conditions (decision 31)', () => {
    expect(pkg.exports['./nextjs/client']).toBe('./dist/nextjs/client.js');
    expect(pkg.exports['./nextjs/server']).toBe('./dist/nextjs/server.js');
    expect(pkg.exports['./nextjs/edge']).toBe('./dist/nextjs/edge.js');
  });

  it('exports ./nextjs with the runtime conditions and one union index.d.ts as its types', () => {
    expect(pkg.exports['./nextjs']).toEqual(NEXTJS_CONDITIONS);
    // `types` must come first: resolvers take the first matching condition in object order.
    expect(Object.keys(pkg.exports['./nextjs'] as Record<string, string>)[0]).toBe('types');
  });

  it('exports the remaining fixed subpaths and ./package.json', () => {
    expect(pkg.exports['.']).toBe('./dist/core/index.js');
    expect(pkg.exports['./browser']).toBe('./dist/browser.js');
    expect(pkg.exports['./node']).toBe('./dist/node.js');
    expect(pkg.exports['./edge']).toBe('./dist/edge.js');
    expect(pkg.exports['./nextjs/route']).toBe('./dist/nextjs/route.js');
    expect(pkg.exports['./nextjs/config']).toBe('./dist/nextjs/config.js');
    expect(pkg.exports['./package.json']).toBe('./package.json');
  });

  it('has bin.sentinel-sourcemaps → dist/cli/index.js and engines.node >=20', () => {
    expect(pkg.bin).toEqual({ 'sentinel-sourcemaps': 'dist/cli/index.js' });
    expect(pkg.engines).toEqual({ node: '>=20' });
  });

  it('has no dependencies key at all (contract §4.1, §11)', () => {
    expect(Object.prototype.hasOwnProperty.call(pkg, 'dependencies')).toBe(false);
  });

  it('exact-pins every devDependency — no ^, no ~, no range (contract §4.1 rule 2)', () => {
    const entries = Object.entries(pkg.devDependencies);
    expect(entries.length).toBeGreaterThan(0);
    for (const [name, spec] of entries) {
      expect(spec, `${name}@${spec}`).toMatch(/^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/);
    }
  });

  it('runs prepare through node or npm run, never pnpm (contract §4.1 rule 1)', () => {
    const prepare = pkg.scripts.prepare ?? '';
    expect(prepare).toMatch(/^(node|npm run)\b/);
    expect(prepare).not.toMatch(/\bpnpm\b/);
    // `npm run build` delegates to `build`, which must obey the same rule.
    if (prepare.startsWith('npm run ')) {
      const target = prepare.slice('npm run '.length).trim();
      expect(pkg.scripts[target], `scripts.${target}`).toMatch(/^node\b/);
    }
  });
});

describe('dist/ (after pnpm build)', () => {
  const distReady = existsSync(join(root, 'dist'));

  it('exists — run `pnpm build` before `pnpm test`', () => {
    expect(distReady, 'dist/ is missing: run pnpm build first').toBe(true);
  });

  it('has every exports target on disk with a .d.ts beside each .js', () => {
    for (const [subpath, target] of Object.entries(pkg.exports)) {
      const targets = typeof target === 'string' ? [target] : Object.values(target);
      for (const t of targets) {
        const file = join(root, t);
        expect(existsSync(file), `${subpath} → ${t}`).toBe(true);
        if (t.endsWith('.js')) {
          const dts = file.replace(/\.js$/, '.d.ts');
          expect(existsSync(dts), `${subpath} → ${t}: no .d.ts beside it`).toBe(true);
        }
      }
    }
  });

  it('has the bin file with a node shebang', () => {
    const bin = readFileSync(join(root, pkg.bin['sentinel-sourcemaps']!), 'utf8');
    expect(bin.startsWith('#!/usr/bin/env node\n')).toBe(true);
  });

  it('import()s every runtime entry cleanly on this node (contract §11, module-load half)', async () => {
    const files = runtimeEntryFiles();
    expect(files.length).toBe(9);
    for (const file of files) {
      const mod: unknown = await import(pathToFileURL(join(root, file)).href);
      expect(mod, file).toBeTypeOf('object');
    }
  });
});
