// README.md and CHANGELOG.md (contract §4.10, §3 Versioning; flows D5): the quickstart's sections in the order
// §4.10 lists them, the two env-var rules verbatim, the per-release cap cited by variable name and never as a
// bare number, and one CHANGELOG entry per tag with the package's own version first. D5 is reviewed in the diff;
// this pins what a later edit could silently reorder or drop. Order-independent: read-only over committed files.

import { readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const readme = readFileSync(join(root, 'README.md'), 'utf8');
const changelog = readFileSync(join(root, 'CHANGELOG.md'), 'utf8');
const { version } = JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')) as { version: string };

/** §4.10's order, as heading text prefixes (the numbered quickstart steps carry their number). */
const SECTIONS_IN_ORDER = [
  '### 1. Install',
  '### 2. The three init files',
  '### 3. `instrumentation.ts`',
  '### 4. The tunnel route',
  '### 5. `next.config.mjs`',
  '### 6. Environment variables',
  '### 7. The CI step',
  '## Naming the release',
  '## Server sourcemaps and `serverRoot`',
  '## When the upload fails',
  "## The tunnel's rate limit",
  '## Which frames are in-app',
  '## Node crashes',
  '## Timestamps',
  '## Coexisting with `@sentry/nextjs`',
  '## Edge',
  '## What is not implemented',
  '## Bundle size',
  '## Local bench',
  '## CLI reference',
  '## Plain node',
  '## Plain browser',
];

describe('README.md (contract §4.10, D5)', () => {
  it('has every §4.10 section, in that order', () => {
    const headings = readme.split('\n').filter((line) => line.startsWith('#'));
    const positions = SECTIONS_IN_ORDER.map((section) => headings.findIndex((h) => h.startsWith(section)));
    positions.forEach((pos, i) => expect(pos, SECTIONS_IN_ORDER[i]).toBeGreaterThanOrEqual(0));
    expect(positions).toEqual([...positions].sort((a, b) => a - b));
  });

  it('carries the two env-var rules verbatim and the three subpath import strings', () => {
    expect(readme).toContain('The build-time key is a job `env:` var, never written to the generated `.env`.');
    expect(readme).toContain('The runtime key is a pod secret, never a `NEXT_PUBLIC_*`.');
    for (const subpath of ['@sentinel/sdk-js/nextjs/client', '@sentinel/sdk-js/nextjs/server', '@sentinel/sdk-js/nextjs/edge']) {
      expect(readme).toContain(`from '${subpath}'`);
    }
  });

  it('cites the per-release cap by variable name, never as a bare number', () => {
    expect(readme).toContain('SOURCEMAP_MAX_FILES_PER_RELEASE');
    expect(readme).not.toMatch(/500 (files )?per release/);
  });

  it("*What is not implemented* has no breadcrumbs entry, and still lists tracing and per-request server scope (spec §8)", () => {
    const section = readme.slice(readme.indexOf('## What is not implemented'), readme.indexOf('## Bundle size'));
    expect(section).not.toMatch(/breadcrumbs/i);
    expect(section).toContain('Tracing');
    expect(section).toContain('Per-request server scope');
  });

  it('the *Not filtered* list names every residual (spec §12.2(3)), and addBreadcrumb is documented', () => {
    for (const token of ['scheme-less', 'location.search', 'location.hash', 'more than 3', 'keyless', 'next=reset_token%3DX', '#access_token=X', 'q=John Doe&token=abc']) {
      expect(readme, token).toContain(token);
    }
    expect(readme).toContain('addBreadcrumb');
  });
});

describe('CHANGELOG.md (contract §3 Versioning — one entry per tag)', () => {
  it('lists the package version as its first entry', () => {
    const entries = changelog.split('\n').filter((line) => line.startsWith('## '));
    expect(entries[0]).toMatch(new RegExp(`^## v${version.replaceAll('.', '\\.')}\\b`));
  });
});
