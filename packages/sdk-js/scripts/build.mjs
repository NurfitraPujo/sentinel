// Builds dist/ (contract §4.1): one self-contained, minified ESM file per entry of the exports table via
// esbuild, `.d.ts` beside each via tsc, and the hand-written src/nextjs/index.d.ts copied as the `types`
// condition of `./nextjs`.
//
// This is what `prepare` runs — and `prepare` runs under npm when pnpm installs the package as a git
// dependency (contract §4.1 rules 1–4), so this script invokes tsc through `node` and the esbuild API
// directly: no `pnpm`, no `npx`, nothing that is not in node_modules after `npm install`.
//
// Node 20 is the floor: the esbuild target is node20 and the script itself uses nothing newer.

import { build } from 'esbuild';
import { spawnSync } from 'node:child_process';
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync } from 'node:fs';
import { createRequire } from 'node:module';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const dist = join(root, 'dist');

// The runtime entries of the exports table (contract §4.1). `./nextjs` has no file of its own: its
// conditions resolve to client/server/edge below and its types are the copied index.d.ts.
const entries = [
  'src/core/index.ts',
  'src/browser.ts',
  'src/node.ts',
  'src/edge.ts',
  'src/nextjs/client.ts',
  'src/nextjs/server.ts',
  'src/nextjs/edge.ts',
  'src/nextjs/route.ts',
  'src/nextjs/config.ts',
];

const common = {
  absWorkingDir: root,
  bundle: true,
  format: 'esm',
  target: 'node20',
  minify: true,
  outdir: dist,
  outbase: 'src',
  // Bundle relative imports only: node builtins and the optional peers (`next`, `react`) stay external, and
  // there are no runtime dependencies to bundle (contract §4.1, asserted by scripts/assert-no-dependencies.mjs).
  packages: 'external',
  charset: 'utf8',
  logLevel: 'warning',
};

// Two lines of provenance, for the consumer path: on a `pnpm add git+…` install this runs under npm
// (npm_execpath names npm-cli.js and npm has written a package-lock.json beside us) with the devDependencies
// resolved fresh from package.json — which is the mechanism contract §4.1 rules 1–4 exist for, and the
// second line is where a floated compiler would show.
const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8'));
const installed = Object.keys(pkg.devDependencies ?? {}).map((name) => {
  const manifest = join(root, 'node_modules', name, 'package.json');
  return `${name}@${existsSync(manifest) ? JSON.parse(readFileSync(manifest, 'utf8')).version : '(missing)'}`;
});
console.log(
  `@sentinel/sdk-js build: node ${process.version}, npm_execpath=${process.env.npm_execpath ?? '(unset)'}, ` +
    `package-lock.json ${existsSync(join(root, 'package-lock.json')) ? 'present' : 'absent'}`,
);
console.log(`@sentinel/sdk-js build: devDependencies ${installed.join(' ')}`);

rmSync(dist, { recursive: true, force: true });
mkdirSync(dist, { recursive: true });

// `platform: 'neutral'` keeps the runtime entries free of node-only assumptions; the CLI is node's.
await build({ ...common, entryPoints: entries, platform: 'neutral', mainFields: ['module', 'main'] });
await build({
  ...common,
  entryPoints: ['src/cli/index.ts'],
  platform: 'node',
  banner: { js: '#!/usr/bin/env node' },
});

// Declarations: tsc invoked through the running node binary, never through a package manager.
const require = createRequire(import.meta.url);
const tsc = require.resolve('typescript/bin/tsc');
const result = spawnSync(process.execPath, [tsc, '-p', join(root, 'tsconfig.build.json')], {
  cwd: root,
  stdio: 'inherit',
});
if (result.status !== 0) {
  process.exit(result.status ?? 1);
}

copyFileSync(join(root, 'src/nextjs/index.d.ts'), join(dist, 'nextjs/index.d.ts'));
