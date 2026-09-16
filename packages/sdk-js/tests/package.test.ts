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

/**
 * In THIS order. Resolvers take the first key in object order that is in their condition set, and Next's edge
 * compiler (webpack's `edgeConditionNames`, Turbopack's edge environment) enables `browser` AS WELL AS
 * `edge-light` — so with `browser` listed first the middleware bundle resolved `./nextjs` to the client build,
 * whose `captureRequestError` is a no-op, and no middleware error ever reached Sentinel (N2 on the compose
 * walk of TASK-07). The edge conditions come before `browser`, exactly as @sentry/nextjs 10.69.0's own map
 * (`edge`, `edge-light`, `worker`, `workerd`, then `browser`) — the shape contract §4.1 transcribes.
 */
const NEXTJS_CONDITIONS: Record<string, string> = {
  types: './dist/nextjs/index.d.ts',
  'edge-light': './dist/nextjs/edge.js',
  worker: './dist/nextjs/edge.js',
  workerd: './dist/nextjs/edge.js',
  browser: './dist/nextjs/client.js',
  node: './dist/nextjs/server.js',
  default: './dist/nextjs/server.js',
};

/** The exports-map rule every resolver follows: the first key, in object order, that the condition set contains. */
function resolveConditions(map: Record<string, string>, conditions: ReadonlySet<string>): string | undefined {
  for (const [condition, target] of Object.entries(map)) if (conditions.has(condition)) return target;
  return undefined;
}

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

  it('resolves ./nextjs per bundle target — the edge conditions are listed BEFORE browser (N2)', () => {
    const map = pkg.exports['./nextjs'] as Record<string, string>;
    const keys = Object.keys(map);
    for (const edge of ['edge-light', 'worker', 'workerd']) expect(keys.indexOf(edge)).toBeLessThan(keys.indexOf('browser'));
    // Next's edge compiler enables browser AND edge-light (webpack: `['edge-light', '...']` on a web target;
    // Turbopack: the edge environment's `browser` + `edge-light`): the middleware bundle must get edge.js.
    expect(resolveConditions(map, new Set(['edge-light', 'worker', 'browser', 'import', 'module', 'default']))).toBe('./dist/nextjs/edge.js');
    // The client compiler: browser without any edge condition → client.js.
    expect(resolveConditions(map, new Set(['browser', 'import', 'module', 'default']))).toBe('./dist/nextjs/client.js');
    // The node server compiler: node → server.js; a bare resolver with no conditions → default → server.js.
    expect(resolveConditions(map, new Set(['node', 'import', 'module', 'default']))).toBe('./dist/nextjs/server.js');
    expect(resolveConditions(map, new Set(['default']))).toBe('./dist/nextjs/server.js');
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
