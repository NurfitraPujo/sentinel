// @vitest-environment jsdom
//
// TASK-01's typed-field API (spec §3.1-§3.9): `setUser` (§3.4), `setTag` / `setTags` / `setTenant` (§3.5),
// `setContext` (§3.6), `level` per capture path (§3.2), `sdk.name` per entry and `sdk.version` (§3.3),
// `autoIpAddress` (§3.4). jsdom gives every entry a `window` (browser, nextjs/client) while node's `process`
// stays reachable in the same process, so all six entries and `initCore` are exercised in one file.
// Shuffle-safe: every test starts from a deleted slot with the previous client's handlers uninstalled.

import { readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { beforeEach, describe, expect, it } from 'vitest';
import { captureException, captureMessage } from '../../src/core/capture.js';
import { initCore } from '../../src/core/init.js';
import { setContext, setTag, setTags, setTenant, setUser } from '../../src/core/scope.js';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent } from '../../src/core/types.js';
import { SDK_VERSION } from '../../src/core/version.js';
import { mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline, setup } from '../helpers/pipeline.js';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '../..');
const pkgVersion = (JSON.parse(readFileSync(join(root, 'package.json'), 'utf8')) as { version: string }).version;

beforeEach(resetPipeline);

function queued(): SentinelEvent[] {
  return (getInternalClient()?.queue ?? []).map((json) => JSON.parse(json) as SentinelEvent);
}

describe('setUser (spec §3.4)', () => {
  it('replaces the scope user whole (Sentry semantics); null and an all-unusable object clear it', () => {
    const p = setup({ autoIpAddress: false });
    setUser({ id: 'usr_1', email: 'a@b.com' });
    captureException(new Error('1'));
    setUser({ username: 'only-username' });
    captureException(new Error('2'));
    setUser(null);
    captureException(new Error('3'));
    setUser({ id: '' });
    captureException(new Error('4'));
    const [e1, e2, e3, e4] = p.events();
    expect(e1?.user).toEqual({ id: 'usr_1', email: 'a@b.com' });
    expect(e2?.user).toEqual({ username: 'only-username' });
    expect(e3?.user).toBeUndefined();
    expect(e4?.user).toBeUndefined();
  });

  it('id over 255 UTF-8 bytes is OMITTED, not truncated; 255 bytes exactly is kept', () => {
    const p = setup({ autoIpAddress: false });
    setUser({ id: 'x'.repeat(255) });
    captureException(new Error('kept'));
    setUser({ id: 'x'.repeat(256) });
    captureException(new Error('omitted'));
    expect(p.events()[0]?.user?.id).toBe('x'.repeat(255));
    expect(p.events()[1]?.user).toBeUndefined();
    expect(p.debugLines.some((l) => l.includes('over 255 UTF-8 bytes'))).toBe(true);
  });

  it('email over 254 code points and ip_address over 45 are omitted; username over 128 is cut', () => {
    const p = setup({ autoIpAddress: false });
    setUser({ email: 'a'.repeat(250) + '@b.com' });
    captureException(new Error('1'));
    setUser({ ip_address: '1'.repeat(46) });
    captureException(new Error('2'));
    setUser({ username: 'u'.repeat(200) });
    captureException(new Error('3'));
    const [e1, e2, e3] = p.events();
    expect(e1?.user).toBeUndefined();
    expect(e2?.user).toBeUndefined();
    expect(e3?.user?.username).toBe('u'.repeat(128));
  });

  it('a finite number or boolean id is coerced with String(v); an unrecognised key is ignored with a debug line', () => {
    const p = setup({ autoIpAddress: false });
    setUser({ id: 123 as unknown as string, geo: { country: 'ID' } } as unknown as { id: number });
    captureException(new Error('x'));
    expect(p.events()[0]?.user).toEqual({ id: '123' });
    expect(p.debugLines.some((l) => l.includes('ignoring unknown key(s) geo'))).toBe(true);
  });
});

describe('setTag / setTags / setTenant (spec §3.5)', () => {
  it('null deletes; a finite number / boolean is coerced; NaN / Infinity / an object / a symbol / a function are dropped', () => {
    const p = setup({});
    setTag('a', 'v');
    setTag('a', null);
    setTag('n', 42);
    setTag('b', true);
    setTag('bad', NaN);
    setTag('bad2', {} as unknown as string);
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.tags).toEqual({ n: '42', b: 'true' });
    expect(p.debugLines.some((l) => l.includes("setTag('bad',") && l.includes('not a string'))).toBe(true);
  });

  it('an invalid key or value is dropped AND deletes a previous value under that key (rule 3)', () => {
    const p = setup({});
    setTag('k', 'first');
    setTag('k', 'y'.repeat(201)); // over 200 code points
    captureException(new Error('x'));
    expect(p.events()[0]?.tags?.k).toBeUndefined();
    setTag('k2', 'ok');
    setTag('k2', 'has\nnewline');
    captureException(new Error('y'));
    expect(p.events()[1]?.tags?.k2).toBeUndefined();
  });

  it("tenant_id is reserved to setTag/setTags — setTag('tenant_id', v) and setTags({tenant_id}) are dropped with a debug line", () => {
    const p = setup({});
    setTag('tenant_id', 'nope');
    setTags({ tenant_id: 'also-nope', ok: 'yes' });
    captureException(new Error('x'));
    expect(p.events()[0]?.tags).toEqual({ ok: 'yes' });
    expect(p.debugLines.filter((l) => l.includes("dropped: the key is reserved — use setTenant"))).toHaveLength(2);
  });

  it('setTenant writes tags.tenant_id; coercion and rule 3 apply; null/unusable deletes it', () => {
    const p = setup({});
    setTenant('org_a');
    captureException(new Error('1'));
    setTenant(null);
    captureException(new Error('2'));
    setTenant(42);
    captureException(new Error('3'));
    setTenant({} as unknown as string);
    captureException(new Error('4'));
    const [e1, e2, e3, e4] = p.events();
    expect(e1?.tags).toEqual({ tenant_id: 'org_a' });
    expect(e2?.tags).toBeUndefined();
    expect(e3?.tags).toEqual({ tenant_id: '42' });
    expect(e4?.tags).toBeUndefined();
  });

  it('the 50-tag cap keeps tenant_id: setTags of a00..a49 (50 keys) plus setTenant("c1") sends 50 tags without a49, with the tags marker', () => {
    const p = setup({});
    const tags: Record<string, string> = {};
    for (let i = 0; i < 50; i++) tags[`a${String(i).padStart(2, '0')}`] = String(i);
    setTags(tags);
    setTenant('c1');
    captureException(new Error('x'));
    const [event] = p.events();
    const keys = Object.keys(event?.tags ?? {});
    expect(keys).toHaveLength(50);
    expect(keys).toContain('tenant_id');
    expect(keys).not.toContain('a49');
    expect(event?.tags?.tenant_id).toBe('c1');
    expect(event?.metadata.truncated).toEqual(['tags']);
    expect(p.debugLines.some((l) => l.includes('a49') && l.includes('50-tag cap'))).toBe(true);
  });

  it('the same 50 keys WITHOUT setTenant are sent whole, with no marker', () => {
    const p = setup({});
    const tags: Record<string, string> = {};
    for (let i = 0; i < 50; i++) tags[`a${String(i).padStart(2, '0')}`] = String(i);
    setTags(tags);
    captureException(new Error('x'));
    const [event] = p.events();
    expect(Object.keys(event?.tags ?? {})).toHaveLength(50);
    expect(event?.metadata.truncated).toBeUndefined();
  });

  it('51 keys (a00..a50) without a tenant sends a00..a49, the server’s own choice', () => {
    const p = setup({});
    const tags: Record<string, string> = {};
    for (let i = 0; i <= 50; i++) tags[`a${String(i).padStart(2, '0')}`] = String(i);
    setTags(tags);
    captureException(new Error('x'));
    const [event] = p.events();
    const keys = Object.keys(event?.tags ?? {}).sort();
    expect(keys).toEqual(Array.from({ length: 50 }, (_, i) => `a${String(i).padStart(2, '0')}`));
    expect(keys).not.toContain('a50');
  });

  it('a per-call ctx.tags that takes the effective count past 50 is capped the same way', () => {
    const p = setup({});
    const scopeTags: Record<string, string> = {};
    for (let i = 0; i < 45; i++) scopeTags[`a${String(i).padStart(2, '0')}`] = String(i);
    setTags(scopeTags);
    setTenant('c1');
    const ctxTags: Record<string, string> = {};
    for (let i = 0; i < 10; i++) ctxTags[`b${i}`] = String(i);
    captureException(new Error('x'), { tags: ctxTags });
    const [event] = p.events();
    const keys = Object.keys(event?.tags ?? {});
    expect(keys).toHaveLength(50);
    expect(keys).toContain('tenant_id');
  });
});

describe('setContext (spec §3.6)', () => {
  it('object only, name 1-64 code points, null deletes, snapshot at write (a later mutation does not change the scope)', () => {
    const p = setup({});
    const obj = { a: 1 };
    setContext('x', obj);
    obj.a = 2;
    captureException(new Error('1'));
    setContext('x', null);
    captureException(new Error('2'));
    setContext('', { a: 1 });
    captureException(new Error('3'));
    setContext('y', 'not-an-object' as unknown as Record<string, unknown>);
    captureException(new Error('4'));
    const [e1, e2, e3, e4] = p.events();
    expect(e1?.contexts).toEqual({ x: { a: 1 } });
    expect(e2?.contexts).toBeUndefined();
    expect(e3?.contexts).toBeUndefined();
    expect(e4?.contexts).toBeUndefined();
  });

  it('the value is checked with isJsonObject AFTER sanitize: Date / URL / an array-returning toJSON store nothing; an object-returning toJSON is stored', () => {
    const p = setup({});
    setContext('x', new Date() as unknown as Record<string, unknown>);
    captureException(new Error('1'));
    setContext('x', new URL('https://h/p') as unknown as Record<string, unknown>);
    captureException(new Error('2'));
    setContext('x', { toJSON: () => [1] } as unknown as Record<string, unknown>);
    captureException(new Error('3'));
    setContext('x', { toJSON: () => ({ a: 1 }) } as unknown as Record<string, unknown>);
    captureException(new Error('4'));
    const [e1, e2, e3, e4] = p.events();
    expect(e1?.contexts?.x).toBeUndefined();
    expect(e2?.contexts?.x).toBeUndefined();
    expect(e3?.contexts?.x).toBeUndefined();
    expect(e4?.contexts?.x).toEqual({ a: 1 });
  });
});

describe('level per capture path (spec §3.2)', () => {
  it('captureException: ctx.level when valid, else error; captureMessage: string or ctx.level, else info', () => {
    const p = setup({});
    captureException(new Error('a'));
    captureException(new Error('b'), { level: 'fatal' });
    captureException(new Error('c'), { level: 'not-a-level' as never });
    captureMessage('m1');
    captureMessage('m2', 'debug');
    captureMessage('m3', { level: 'warning' });
    const levels = p.events().map((e) => e.level);
    expect(levels).toEqual(['error', 'fatal', 'error', 'info', 'debug', 'warning']);
  });
});

describe('sdk (spec §3.3): name per entry, version === package.json', () => {
  it('SDK_VERSION equals package.json version', () => {
    expect(SDK_VERSION).toBe(pkgVersion);
  });

  it('no entry (initCore directly): sdk.name is sentinel.javascript.<runtime>', () => {
    for (const runtime of ['browser', 'node', 'edge'] as const) {
      resetPipeline();
      const p = setup({}, runtime);
      captureException(new Error('x'));
      expect(p.events()[0]?.sdk).toEqual({ name: `sentinel.javascript.${runtime}`, version: SDK_VERSION });
    }
  });

  it('@sentinel/sdk-js/browser: sentinel.javascript.browser', async () => {
    resetPipeline();
    mockFetch();
    const { init, captureException: capture } = await import('../../src/browser.js');
    init({ tunnel: '/api/sentinel' });
    capture(new Error('x'));
    expect(queued()[0]?.sdk).toEqual({ name: 'sentinel.javascript.browser', version: SDK_VERSION });
  });

  it('@sentinel/sdk-js/node: sentinel.javascript.node', async () => {
    resetPipeline();
    mockFetch();
    const { init, captureException: capture } = await import('../../src/node.js');
    init({ endpoint: 'http://127.0.0.1:1/ingest', apiKey: 'k' });
    capture(new Error('x'));
    expect(queued()[0]?.sdk).toEqual({ name: 'sentinel.javascript.node', version: SDK_VERSION });
  });

  it('@sentinel/sdk-js/edge: sentinel.javascript.edge', async () => {
    resetPipeline();
    mockFetch();
    const { init } = await import('../../src/edge.js');
    const { captureException: capture } = await import('../../src/core/index.js');
    init({ endpoint: 'http://127.0.0.1:1/ingest', apiKey: 'k' });
    capture(new Error('x'));
    expect(queued()[0]?.sdk).toEqual({ name: 'sentinel.javascript.edge', version: SDK_VERSION });
  });

  it('@sentinel/sdk-js/nextjs/client, /server, /edge: sentinel.javascript.nextjs (spec §3.3 — Sentry names all three the same)', async () => {
    resetPipeline();
    mockFetch();
    const client = await import('../../src/nextjs/client.js');
    client.init({ tunnel: '/api/sentinel' });
    client.captureException(new Error('x'));
    expect(queued()[0]?.sdk).toEqual({ name: 'sentinel.javascript.nextjs', version: SDK_VERSION });

    resetPipeline();
    const server = await import('../../src/nextjs/server.js');
    server.init({ endpoint: 'http://127.0.0.1:1/ingest', apiKey: 'k' });
    server.captureException(new Error('x'));
    expect(queued()[0]?.sdk).toEqual({ name: 'sentinel.javascript.nextjs', version: SDK_VERSION });

    resetPipeline();
    const edge = await import('../../src/nextjs/edge.js');
    edge.init({ endpoint: 'http://127.0.0.1:1/ingest', apiKey: 'k' });
    void edge.captureRequestError(new Error('x'), { path: '/x', method: 'GET', headers: {} }, { routerKind: 'Pages Router', routePath: '/x', routeType: 'render', revalidateReason: undefined });
    expect(queued()[0]?.sdk).toEqual({ name: 'sentinel.javascript.nextjs', version: SDK_VERSION });
  });
});

describe('autoIpAddress (spec §3.4)', () => {
  it('defaults true on the browser runtime: a user with no ip_address gets "{{auto}}"; creates user = { ip_address } when none set', () => {
    const p = setup({}, 'browser');
    captureException(new Error('no user'));
    expect(p.events()[0]?.user).toEqual({ ip_address: '{{auto}}' });
    setUser({ id: 'usr_1' });
    captureException(new Error('with user'));
    expect(p.events()[1]?.user).toEqual({ id: 'usr_1', ip_address: '{{auto}}' });
  });

  it('does not override an explicit ip_address', () => {
    const p = setup({}, 'browser');
    setUser({ ip_address: '203.0.113.9' });
    captureException(new Error('x'));
    expect(p.events()[0]?.user?.ip_address).toBe('203.0.113.9');
  });

  it('false: the SDK never adds it', () => {
    const p = setup({ autoIpAddress: false }, 'browser');
    captureException(new Error('x'));
    expect(p.events()[0]?.user).toBeUndefined();
  });

  it('node and edge never send {{auto}} regardless of the option (the option does not exist on their types)', () => {
    for (const runtime of ['node', 'edge'] as const) {
      resetPipeline();
      const p = setup({}, runtime);
      captureException(new Error('x'));
      expect(p.events()[0]?.user).toBeUndefined();
    }
  });

  it('a non-boolean value degrades to the default true with invalid_option', () => {
    const p = setup({ autoIpAddress: 'yes' as unknown as boolean }, 'browser');
    captureException(new Error('x'));
    expect(p.events()[0]?.user).toEqual({ ip_address: '{{auto}}' });
    expect(p.errors.map((e) => e.code)).toContain('invalid_option');
  });
});

describe('every event this file builds', () => {
  it('metadata carries no user_id, tenant_id, level, or any key set through setTag', () => {
    const p = setup({});
    setUser({ id: 'usr_1' });
    setTag('feature', 'chats');
    setTenant('org_a');
    captureException(new Error('x'), { metadata: { extra: 1 } });
    const metadata = p.events()[0]?.metadata ?? {};
    expect(metadata).not.toHaveProperty('user_id');
    expect(metadata).not.toHaveProperty('tenant_id');
    expect(metadata).not.toHaveProperty('level');
    expect(metadata).not.toHaveProperty('feature');
  });
});
