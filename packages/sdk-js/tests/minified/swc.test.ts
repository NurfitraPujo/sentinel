// @vitest-environment jsdom
//
// The built entries survive the CONSUMER's minifier. A Next production build re-minifies everything it bundles
// with SWC, and SWC's minifier (Next 15.5.21's bundled binary and @swc/core 1.13.5 alike) dropped the whole
// object branch of src/core/serialize.ts's `walk` — a `switch (typeof v)` whose `case 'object': break;` fell
// out to the code after the switch — so every event in every Next bundle serialised to `null` and the tunnel /
// ingestor rejected each batch as `[null]` (found on the compose walk of sentinel-sdk-js/TASK-07). This suite
// minifies each runtime entry of dist/ with @swc/core at Next's settings (compress + mangle, ESM), imports the
// minified file and sends one capture through it: the POST body must be a one-element array carrying the
// event, not `[null]`. A miscompiled dispatch anywhere on the capture path shows here as a wrong body or no
// POST at all. Needs dist/ (`pnpm build`, as tests/package.test.ts does). Shuffle-safe: the slot is cleared
// before and after every test and each minified entry is a fresh module instance.

import { minify } from '@swc/core';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const dist = join(root, 'dist');

interface Entry {
  file: string;
  /** How `init` reaches a transport: the browser builds through `tunnel`, the others through endpoint + key. */
  target: 'tunnel' | 'endpoint';
}

const ENTRIES: Entry[] = [
  { file: 'node.js', target: 'endpoint' },
  { file: 'edge.js', target: 'endpoint' },
  { file: 'browser.js', target: 'tunnel' },
  { file: 'nextjs/server.js', target: 'endpoint' },
  { file: 'nextjs/edge.js', target: 'endpoint' },
  { file: 'nextjs/client.js', target: 'tunnel' },
];

interface Sdk {
  init(options: Record<string, unknown>): void;
  captureException(error: unknown, ctx?: Record<string, unknown>): string;
  flush(timeoutMs?: number): Promise<boolean>;
  close(timeoutMs?: number): Promise<void>;
}

// Under the package root: vitest's module runner refuses a file: import from outside it.
const out = join(root, 'node_modules', '.cache', 'sdk-js-swc');
mkdirSync(out, { recursive: true });

/** Minify one built entry the way a Next production build does and return a file: URL to import it from. */
async function minified(entry: string): Promise<string> {
  const source = readFileSync(join(dist, entry), 'utf8');
  const result = await minify(source, { compress: true, mangle: true, module: true });
  const path = join(out, `${basename(entry, '.js')}-${entry.includes('nextjs') ? 'nextjs' : 'plain'}.min.mjs`);
  writeFileSync(path, result.code);
  return pathToFileURL(path).href;
}

describe('the built entries minified by SWC (the consumer bundler) still serialise the event', () => {
  beforeEach(() => {
    resetPipeline();
    expect(existsSync(join(dist, 'nextjs/client.js')), 'dist/ missing — run pnpm build first').toBe(true);
  });
  afterEach(() => resetPipeline());

  for (const entry of ENTRIES) {
    it(`dist/${entry.file}: one captureException through the minified entry posts the event, not [null]`, async () => {
      const sdk = (await import(await minified(entry.file))) as Sdk;
      const fetch = mockFetch();
      sdk.init({
        ...(entry.target === 'tunnel' ? { tunnel: '/api/sentinel' } : { endpoint: 'http://127.0.0.1:9/ingest', apiKey: 'k' }),
        release: '1.2.3',
        environment: 'test',
        installGlobalHandlers: false,
        batchWaitMs: 10,
      });
      const id = sdk.captureException(new Error('through the minified entry'), { metadata: { password: 'hunter2', route: '/x' } });
      expect(await sdk.flush(2000)).toBe(true);
      await sdk.close(500);

      expect(fetch.posts.length, 'exactly one POST').toBe(1);
      const post = fetch.posts[0]!;
      expect(post.body.startsWith('[{'), `body ${post.body.slice(0, 80)}`).toBe(true);
      expect(post.events).toHaveLength(1);
      const event = post.events[0]!;
      expect(event).not.toBeNull();
      expect(event.event_id).toBe(id);
      expect(event.platform).toBe('javascript');
      expect(event.error_class).toBe('Error');
      expect(event.message).toBe('through the minified entry');
      expect(event.release_version).toBe('1.2.3');
      expect(event.metadata).toMatchObject({ password: '[FILTERED]', route: '/x' });
      expect(Array.isArray(event.stacktrace) && event.stacktrace.length > 0).toBe(true);
    });
  }
});
