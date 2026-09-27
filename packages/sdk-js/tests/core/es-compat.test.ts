// The Safari-12 floor (TASK-03, spec §5.1 *Regex syntax*, *Built-in API floor*): plain-text scans of every file
// under src/, so a post-12 construct is caught at commit time rather than by a bug report from an old iOS. No
// parsing — a literal substring scan is enough for every token this list names, and is exactly what would have
// caught each of the shown-red mutations named across this feature's later commits (§4.4's crumb copy as
// `structuredClone`, §3.10's surrogate repair as `s.toWellFormed()`, `URL_IN_TEXT` written with `(?<![\w/])`).

import { readdirSync, readFileSync, statSync } from 'node:fs';
import { dirname, join, relative } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { URL_IN_TEXT } from '../../src/core/url.js';

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'src');

function allFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir)) {
    const path = join(dir, entry);
    if (statSync(path).isDirectory()) out.push(...allFiles(path));
    else if (entry.endsWith('.ts')) out.push(path);
  }
  return out;
}

const FILES = allFiles(SRC);

/** Files exempt from the built-in floor: the sourcemap-upload CLI, node-only, whose walk.ts uses matchAll today. */
function isExemptFromBuiltinFloor(path: string): boolean {
  const rel = relative(SRC, path).replace(/\\/g, '/');
  return rel.startsWith('cli/') || rel.startsWith('upload/');
}

describe('es-compat: regex syntax (Safari-12 floor, spec §5.1)', () => {
  const LOOKBEHIND_OR_UNICODE_CLASS = ['(?<=', '(?<!', '\\p{', '\\P{'];

  it('no file under src/ uses a lookbehind or a \\p{}/\\P{} class', () => {
    for (const path of FILES) {
      const content = readFileSync(path, 'utf8');
      for (const token of LOOKBEHIND_OR_UNICODE_CLASS) {
        expect(content.includes(token), `${relative(SRC, path)} contains ${JSON.stringify(token)}`).toBe(false);
      }
    }
  });

  it('no file under src/ calls .replaceAll(', () => {
    for (const path of FILES) {
      const content = readFileSync(path, 'utf8');
      expect(content.includes('replaceAll('), `${relative(SRC, path)} calls replaceAll(`).toBe(false);
    }
  });

  it("URL_IN_TEXT.flags is exactly 'g' (no lookbehind literal snuck in behind a differently-flagged pattern)", () => {
    expect(URL_IN_TEXT.flags).toBe('g');
  });
});

describe('es-compat: the built-in API floor (spec §5.1), src/cli/** and src/upload/** exempt', () => {
  const BUILTIN_FLOOR = [
    'structuredClone(',
    '.toWellFormed(',
    '.isWellFormed(',
    '.at(',
    '.findLast(',
    'Object.hasOwn(',
    'Object.fromEntries(',
    '.matchAll(',
    'Promise.allSettled(',
    'Promise.any(',
    'AggregateError',
    'WeakRef',
    'FinalizationRegistry',
    'URL.canParse(',
    'Object.groupBy(',
    '.toSorted(',
    '.toReversed(',
    '.toSpliced(',
  ];

  it('no file outside the exempt directories uses a post-12 built-in', () => {
    for (const path of FILES) {
      if (isExemptFromBuiltinFloor(path)) continue;
      const content = readFileSync(path, 'utf8');
      for (const token of BUILTIN_FLOOR) {
        expect(content.includes(token), `${relative(SRC, path)} contains ${JSON.stringify(token)}`).toBe(false);
      }
    }
  });

  // `globalThis` (Safari 12.1+, not 12) is not in BUILTIN_FLOOR above because the pre-existing browser code
  // (getSlot, every integration) already depends on it throughout — grandfathering it outright would make the
  // scan blind to it forever. Instead every current site is recorded here, by file, so the scan still catches
  // what "neither introduces nor extends" (spec §5.1) means: a NEW file using it, or an EXISTING file's count
  // going UP. `.findLastIndex(` has zero recorded sites, so any use at all is caught immediately, like every
  // other BUILTIN_FLOOR token.
  const GLOBALTHIS_BASELINE: Readonly<Record<string, number>> = {
    'core/scope.ts': 1,
    'edge.ts': 1,
    'core/index.ts': 1,
    'browser/integrations/click.ts': 1,
    'core/capture.ts': 1,
    'core/handlers/browser.ts': 2,
    'core/client.ts': 2,
    'core/handlers/node.ts': 1,
    'core/options.ts': 3,
    'browser/integrations/xhr.ts': 3,
    'core/report.ts': 1,
    'browser/integrations/navigation.ts': 3,
    'core/init.ts': 1,
    'core/state.ts': 5,
    'browser/contexts.ts': 6,
    'core/transport/transport.ts': 2,
    'nextjs/route.ts': 2,
    'browser/integrations/fetch.ts': 3,
    'browser/integrations/console.ts': 1,
  };

  it("globalThis is recorded per file and never grows beyond its baseline (spec §5.1 F6 'neither introduces nor extends')", () => {
    for (const path of FILES) {
      if (isExemptFromBuiltinFloor(path)) continue;
      const rel = relative(SRC, path).replace(/\\/g, '/');
      const content = readFileSync(path, 'utf8');
      const count = (content.match(/globalThis/g) ?? []).length;
      const baseline = GLOBALTHIS_BASELINE[rel] ?? 0;
      expect(count, `${rel}: ${count} globalThis site(s), baseline ${baseline}`).toBeLessThanOrEqual(baseline);
    }
  });

  it('no file outside the exempt directories calls .findLastIndex( (zero recorded sites, unlike globalThis)', () => {
    for (const path of FILES) {
      if (isExemptFromBuiltinFloor(path)) continue;
      const content = readFileSync(path, 'utf8');
      expect(content.includes('.findLastIndex('), `${relative(SRC, path)} calls .findLastIndex(`).toBe(false);
    }
  });
});
