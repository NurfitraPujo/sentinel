// The tunnel fixtures (sentinel-event-enrichment spec §4.5): the Next.js tunnel's REAL forwards, captured from the
// handler with a stub upstream and committed under tests/tunnel-fixtures/ (a SIBLING of tests/fixtures/, whose
// *.json glob is the golden fingerprint corpus). tests/unit/sdk_js_tunnel_fixture_test.go replays them through the
// ingestor's resolver and enrichment, and tests/integration/enrichment_test.go posts them to the real ingestor, so
// the Go side never needs node.
//
// Written deterministically and only when content changes (the fixtures.test.ts convention) — with one difference:
// when `process.env.CI` is set this suite NEVER writes. It compares what it captured with the committed file and
// fails, naming the file, on any difference or a missing file. That is the red signal for drift in CI; locally it
// rewrites and the unit gate's `git diff` / untracked-file checks catch the rewrite.
//
// Each route fixture: { name, inbound: { headers, body }, forwarded: { path, headers, body }, expect: { client_ip,
// user_agent } }, bodies as the exact strings sent. tunnel-user-rebuild.json carries the exported applyClientIp's
// real output instead, because ITEM_KEYS removes `user` before it runs on the route today (T10).

import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureException } from '../src/core/capture.js';
import type { SentinelEvent } from '../src/core/types.js';
import { applyClientIp, createSentinelRouteHandler } from '../src/nextjs/route.js';
import { resetPipeline, setup } from './helpers/pipeline.js';
import { request, stubUpstream, type Upstream } from './route/helpers.js';

const DIR = join(dirname(fileURLToPath(import.meta.url)), 'tunnel-fixtures');
const FIXTURE_KEY = 'sk_tunnel_fixture_key';
const FIXTURE_ENDPOINT = 'http://ingestor.fixture:8080/ingest';
const CHROME_WINDOWS_UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36';
const LONDON = '81.2.69.142';

let upstream: Upstream;

beforeEach(() => {
  resetPipeline();
  vi.useFakeTimers({ now: Date.UTC(2026, 8, 16, 12, 0, 0), toFake: ['Date'] });
  vi.stubEnv('SENTINEL_ENDPOINT', FIXTURE_ENDPOINT);
  vi.stubEnv('SENTINEL_API_KEY', FIXTURE_KEY);
  vi.stubEnv('SENTINEL_PROJECT_KEY', '');
});

afterEach(() => {
  vi.unstubAllEnvs();
  vi.restoreAllMocks();
  vi.useRealTimers();
});

/** A browser-runtime sdk-js event with a per-fixture event_id prefix, so no two fixtures share an id (the ingestor dedups on it). */
function browserEvent(idTag: number, message: string): SentinelEvent {
  let n = 0;
  vi.spyOn(globalThis.crypto, 'randomUUID').mockImplementation(
    () => `00000000-0000-4000-8${String(idTag).padStart(3, '0')}-${String(++n).padStart(12, '0')}` as `${string}-${string}-${string}-${string}-${string}`,
  );
  const p = setup({}, 'browser');
  const err = new Error(message);
  err.name = 'TypeError';
  err.stack = [`TypeError: ${message}`, '    at render (https://app/_next/static/chunks/app/page-9b0d4e.js:12:34)'].join('\n');
  captureException(err);
  const events = p.events();
  expect(events).toHaveLength(1);
  resetPipeline();
  return events[0] as SentinelEvent;
}

/** Write (locally) or compare (under CI) one fixture file. */
function commit(file: string, value: unknown): void {
  const path = join(DIR, file);
  const content = JSON.stringify(value, null, 2) + '\n';
  const committed = existsSync(path) ? readFileSync(path, 'utf8') : undefined;
  if (process.env.CI) {
    expect(committed, `tests/tunnel-fixtures/${file} is missing — run the sdk-js suite locally and commit it`).toBeDefined();
    expect(content, `tests/tunnel-fixtures/${file} drifted from the route's real output — regenerate and commit it`).toBe(committed);
    return;
  }
  mkdirSync(DIR, { recursive: true });
  if (committed !== content) writeFileSync(path, content);
}

interface RouteCase {
  name: string;
  idTag: number;
  xff: string | null;
  user?: Record<string, unknown>;
  clientIp: string;
}

const ROUTE_CASES: RouteCase[] = [
  { name: 'tunnel-clean', idTag: 1, xff: '198.51.100.23', clientIp: '198.51.100.23' },
  { name: 'tunnel-spoofed-xff', idTag: 2, xff: '6.6.6.6, 198.51.100.23', clientIp: '198.51.100.23' },
  { name: 'tunnel-auto-ip', idTag: 3, xff: '198.51.100.23', user: { ip_address: '{{auto}}' }, clientIp: '198.51.100.23' },
  { name: 'tunnel-browser-sent-ip', idTag: 4, xff: '198.51.100.23', user: { ip_address: '6.6.6.6' }, clientIp: '198.51.100.23' },
  { name: 'tunnel-no-ip', idTag: 5, xff: null, user: { ip_address: '6.6.6.6' }, clientIp: '' },
];

describe('tunnel fixtures: the route’s real forwards, committed for the Go side (spec §4.5)', () => {
  for (const c of ROUTE_CASES) {
    it(`fixture: ${c.name}`, async () => {
      const event = browserEvent(c.idTag, `tunnel fixture ${c.name}`) as unknown as Record<string, unknown>;
      expect((event.metadata as Record<string, unknown>).runtime).toBe('browser');
      if (c.user !== undefined) event.user = c.user;
      const inboundBody = JSON.stringify([event]);
      const inboundHeaders: Record<string, string> = {
        host: 'app-staging.daya.ai',
        origin: 'https://app-staging.daya.ai',
        'sec-fetch-site': 'same-origin',
        'content-type': 'application/json',
        'user-agent': CHROME_WINDOWS_UA,
      };
      if (c.xff !== null) inboundHeaders['x-forwarded-for'] = c.xff;

      // After browserEvent: its resetPipeline restores every spy, the upstream stub included.
      upstream = stubUpstream();
      const { POST } = createSentinelRouteHandler();
      const res = await POST(request({ url: 'https://app-staging.daya.ai/api/sentinel', headers: inboundHeaders, body: inboundBody }));
      expect(res.status).toBe(202);
      expect(upstream.calls).toHaveLength(1);
      const call = upstream.calls[0];
      if (call === undefined) throw new Error('no forward');

      // The fixture's own invariants, checked before it is written.
      expect(call.headers).not.toHaveProperty('x-forwarded-for');
      expect(call.headers['x-sentinel-derive-ip']).toBe('off');
      expect(call.headers['user-agent']).toBe(CHROME_WINDOWS_UA);
      const items = call.items as Record<string, unknown>[];
      for (const item of items) {
        if (c.clientIp === '') expect(item).not.toHaveProperty('user');
        else expect(item.user).toEqual({ ip_address: c.clientIp });
      }
      expect(call.body).not.toContain('6.6.6.6');

      commit(`${c.name}.json`, {
        name: c.name,
        inbound: { headers: inboundHeaders, body: inboundBody },
        forwarded: { path: new URL(call.url).pathname, headers: call.headers, body: call.body },
        expect: { client_ip: c.clientIp, user_agent: CHROME_WINDOWS_UA },
      });
    });
  }
});

describe('tunnel fixtures: applyClientIp’s real output (tunnel-user-rebuild.json, spec §4.5)', () => {
  it('fixture: tunnel-user-rebuild', () => {
    const cases: { name: string; ip: string | null; user: Record<string, unknown> }[] = [
      { name: 'dup-exact-and-upper', ip: LONDON, user: { ip_address: '', IP_ADDRESS: '6.6.6.6' } },
      { name: 'dup-both-set', ip: LONDON, user: { ip_address: '6.6.6.6', IP_ADDRESS: '6.6.6.6' } },
      { name: 'mixed-case', ip: LONDON, user: { Ip_Address: '6.6.6.6' } },
      { name: 'long-s', ip: LONDON, user: { 'ip_addreſſ': '6.6.6.6' } },
      { name: 'browser-geo', ip: LONDON, user: { id: 'u1', geo: { country_code: 'XX', city: 'Fake' }, GEO: { country_code: 'XY', city: 'Fake2' } } },
      { name: 'no-ip-folded', ip: null, user: { Ip_Address: '6.6.6.6', GEO: { country_code: 'XX' } } },
    ];
    const out = cases.map((c, i) => {
      const event = browserEvent(10 + i, `tunnel rebuild ${c.name}`) as unknown as Record<string, unknown>;
      event.user = c.user;
      const input = [JSON.parse(JSON.stringify(event)) as Record<string, unknown>];
      const items = JSON.parse(JSON.stringify(input)) as Record<string, unknown>[];
      applyClientIp(items, c.ip);
      const output = JSON.stringify(items);
      expect(output).not.toContain('6.6.6.6');
      expect(output).not.toContain('Fake');
      return { name: c.name, ip: c.ip, input, output };
    });
    commit('tunnel-user-rebuild.json', { name: 'tunnel-user-rebuild', cases: out });
  });
});
