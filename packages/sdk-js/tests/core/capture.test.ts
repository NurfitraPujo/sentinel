// The capture-level rows of flows.md against contract §4.3: B8 (`beforeSend` returning null), B10 (sampling with
// Math.random stubbed, dedup marked first), B11 (the 64 KiB shrink), B12 (scope, `setTag`'s rules, the per-call
// `ctx.metadata` table, the runtime coercion), B21 (object-identity dedup), N10 (`release` over 100 bytes), N11
// (`environment` normalisation), N12 (`captureMessage`'s shape), N13 (`fingerprint`). Shuffle-safe: every test
// starts from a deleted `globalThis` slot.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { captureException, captureMessage } from '../../src/core/capture.js';
import { MAX_COMPONENT_STACK_BYTES, MAX_MESSAGE_BYTES, MAX_METADATA_BYTES } from '../../src/core/event.js';
import { getClient } from '../../src/core/init.js';
import { setTag, setTenant, setUser } from '../../src/core/scope.js';
import { metadataWireBytes } from '../../src/core/serialize.js';
import { resetPipeline, setup, SLOT } from '../helpers/pipeline.js';

beforeEach(resetPipeline);

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

describe('B8: beforeSend returning null drops the event', () => {
  it("B8: web's filter — message.includes('Request failed with status code 401') → null → nothing enqueued", () => {
    const p = setup({
      beforeSend: (event) => (event.message.includes('Request failed with status code 401') ? null : event),
    });
    const id = captureException(new Error('Request failed with status code 401'));
    captureException(new Error('Request failed with status code 500'));
    expect(id).toMatch(UUID);
    expect(p.events().map((e) => e.message)).toEqual(['Request failed with status code 500']);
    expect(p.errors).toEqual([]);
  });
});

describe('B10: sampling — Math.random stubbed to a fixed sequence', () => {
  it('B10: sampleRate 0.5 over 1 000 distinct captures keeps exactly the rolls under 0.5, before beforeSend', () => {
    // Alternating 0.25 / 0.75: exactly half the rolls pass.
    let n = 0;
    vi.spyOn(Math, 'random').mockImplementation(() => (n++ % 2 === 0 ? 0.25 : 0.75));
    const hook = vi.fn((event) => event);
    const p = setup({ sampleRate: 0.5, beforeSend: hook });
    for (let i = 0; i < 1000; i++) captureException(new Error(`e${i}`));
    expect(p.events()).toHaveLength(500);
    expect(hook).toHaveBeenCalledTimes(500);
    expect(Math.random).toHaveBeenCalledTimes(1000);
  });

  it('B10: a second report of an already-seen Error is not re-rolled — 1 000 boundary + window pairs yield 500, not 750', () => {
    let n = 0;
    vi.spyOn(Math, 'random').mockImplementation(() => (n++ % 2 === 0 ? 0.25 : 0.75));
    const p = setup({ sampleRate: 0.5 });
    for (let i = 0; i < 1000; i++) {
      const err = new Error(`e${i}`);
      const first = captureException(err); // the boundary
      const second = captureException(err); // window.onerror
      expect(second).toBe(first);
    }
    expect(Math.random).toHaveBeenCalledTimes(1000);
    expect(p.events()).toHaveLength(500);
  });

  it('B10: sampleRate 1 rolls nothing away and sampleRate 0 drops everything silently, no onError', () => {
    const p = setup({ sampleRate: 0 });
    vi.spyOn(Math, 'random').mockReturnValue(0);
    captureException(new Error('x'));
    expect(p.events()).toHaveLength(0);
    expect(p.errors).toEqual([]);
  });
});

describe('B11: the 64 KiB shrink', () => {
  it('B11: a 100 KiB componentStack → metadata.react_component_stack ≤ 4 KiB, recorded in truncated, metadata ≤ 64 KiB', () => {
    const p = setup();
    const componentStack = '\n    at Page (webpack-internal:///./src/app/page.tsx:1:1)'.repeat(2000);
    expect(Buffer.byteLength(componentStack)).toBeGreaterThan(100 * 1024);
    captureException(new Error('boundary'), { contexts: { react: { componentStack } } });
    const [event] = p.events();
    const stack = event?.metadata.react_component_stack as string;
    expect(Buffer.byteLength(stack)).toBeLessThanOrEqual(MAX_COMPONENT_STACK_BYTES);
    expect(componentStack.startsWith(stack)).toBe(true);
    expect(event?.metadata.truncated).toEqual(['react_component_stack']);
    expect(event?.metadata.truncated_keys).toBeUndefined();
    expect(metadataWireBytes(JSON.stringify(event?.metadata))).toBeLessThanOrEqual(MAX_METADATA_BYTES);
  });

  it('a caller-supplied CaptureContext.request is filtered exactly like an automatic one — no raw cookie/authorization or reset_token query', () => {
    const p = setup();
    captureException(new Error('x'), {
      request: {
        url: '/reset',
        query_string: 'reset_token=X',
        headers: { Cookie: 'sid=1', Authorization: 'Bearer x', 'x-csrf-token': 'tok', referer: 'https://h/r?reset_token=Y' },
      },
    });
    const [event] = p.events();
    expect(event?.request?.url).toBe('/reset');
    expect(event?.request?.query_string).toBe('reset_token=[Filtered]');
    expect(event?.request?.headers?.referer).not.toContain('Y');
    expect(event?.request?.headers?.referer).toContain('[Filtered]');
    expect(event?.request?.headers).not.toHaveProperty('Cookie');
    expect(event?.request?.headers).not.toHaveProperty('Authorization');
    expect(event?.request?.headers?.['x-csrf-token']).toBe('[Filtered]');
    const serialized = JSON.stringify(event);
    expect(serialized).not.toContain('sid=1');
    expect(serialized).not.toContain('Bearer x');
    expect(serialized).not.toContain('reset_token=X');
  });

  it('CaptureContext.contexts only accepts the react shape — a per-call custom context name is a type error and, forced through, is silently dropped (spec §2, §3.9)', () => {
    const p = setup();
    // @ts-expect-error CaptureContext.contexts is { react?: { componentStack } }, not an arbitrary context map.
    captureException(new Error('x'), { contexts: { order: { id: 1 } } });
    const [event] = p.events();
    expect(event?.contexts).toBeUndefined();
  });

  it('B11: { a: 30 KiB, request_id, b: 30 KiB, c: 30 KiB, route } — a removed first (tie by insertion), exempt keys never', () => {
    const p = setup();
    const big = 'x'.repeat(30 * 1024);
    captureException(new Error('x'), { metadata: { a: big, request_id: 'req_1', b: big, c: big, route: '/x' } });
    const [event] = p.events();
    // 90 KiB → remove `a` (largest; `a` before `c` on the tie) → 60 KiB + overhead fits: nothing else removed.
    expect(event?.metadata.truncated_keys).toEqual(['a']);
    expect(Object.keys(event?.metadata ?? {})).toEqual(['request_id', 'b', 'c', 'route', 'runtime', 'truncated_keys']);
    expect(event?.metadata).toMatchObject({ request_id: 'req_1', route: '/x', runtime: 'browser' });
    expect(metadataWireBytes(JSON.stringify(event?.metadata))).toBeLessThanOrEqual(MAX_METADATA_BYTES);
  });

  it('B11: the tie is broken by insertion order — c before a when c was inserted first', () => {
    const p = setup();
    const big = 'x'.repeat(40 * 1024);
    captureException(new Error('x'), { metadata: { c: big, small: 'y', a: big } });
    expect(p.events()[0]?.metadata.truncated_keys).toEqual(['c']);
    expect(p.events()[0]?.metadata.a).toBe(big);
  });

  it('B11: { route: 70 KiB } alone → never dropped (spec §3.7): route removed, truncated_keys: [route], small kept, no onError', () => {
    const p = setup();
    const id = captureException(new Error('x'), { metadata: { route: '/'.repeat(70 * 1024), small: 'kept?' } });
    expect(id).toMatch(UUID);
    const [event] = p.events();
    expect(event?.metadata.route).toBeUndefined();
    expect(event?.metadata.small).toBe('kept?');
    expect(event?.metadata.truncated_keys).toEqual(['route']);
    expect(metadataWireBytes(JSON.stringify(event?.metadata))).toBeLessThanOrEqual(MAX_METADATA_BYTES);
    expect(p.errors).toEqual([]);
  });

  it("B11: the validator's ruler — html with 2 000 '<' and a U+2028 is under 65 536 naively and over it by the Go count → html removed", () => {
    const p = setup();
    const html = ('<' + 'p'.repeat(24)).repeat(2000) + '\u2028';
    const pad = 'y'.repeat(14 * 1024 + 1000);
    const naive = { html, pad, user_id: 'usr_1', runtime: 'browser' };
    const naiveBytes = Buffer.byteLength(JSON.stringify(naive));
    const goBytes = metadataWireBytes(JSON.stringify(naive));
    expect(naiveBytes).toBeLessThan(MAX_METADATA_BYTES);
    expect(goBytes).toBeGreaterThan(MAX_METADATA_BYTES);
    captureException(new Error('x'), { metadata: { html, pad, user_id: 'usr_1' } });
    const [event] = p.events();
    expect(event?.metadata.html).toBeUndefined();
    expect(event?.metadata.pad).toBe(pad);
    expect(event?.metadata.truncated_keys).toEqual(['html']);
    expect(metadataWireBytes(JSON.stringify(event?.metadata))).toBeLessThanOrEqual(MAX_METADATA_BYTES);
  });
});

describe('B12: scope, setTag rules, the per-call ctx.metadata table and the runtime coercion', () => {
  it('B12: user and tags land in their typed fields, never in metadata (spec §3.7); password/author still filtered (D8)', () => {
    const p = setup({}, 'node');
    setUser({ id: 'usr_1' });
    setTenant('org_a');
    setTag('feature', 'chats');
    captureException(new Error('1'), { metadata: { password: 'hunter2', author: 'me', passenger: 'p' } });

    const [e1] = p.events();
    expect(e1?.user).toEqual({ id: 'usr_1' });
    expect(e1?.tags).toEqual({ feature: 'chats', tenant_id: 'org_a' });
    expect(e1?.metadata).toEqual({
      password: '[FILTERED]',
      author: '[FILTERED]',
      passenger: '[FILTERED]',
      runtime: 'node',
    });
    expect(p.errors).toEqual([]);
  });

  it('B12: decision 38 per-call ctx.metadata table — SDK-owned keys and identity keys dropped, everything else kept', () => {
    // node's debug goes to stderr, not console.debug (report.ts) — browser (with autoIpAddress off) keeps it
    // observable through the console.debug spy the pipeline helper installs.
    const p = setup({ autoIpAddress: false });
    captureException(new Error('2'), {
      metadata: { runtime: 'x', source: 'y', level: 'info', truncated: ['z'], truncated_keys: ['z'], user_id: 'usr_7', tenant_id: 'org_z', request_id: 'req_1' },
    });
    expect(p.events()[0]?.metadata).toEqual({ request_id: 'req_1', runtime: 'browser' });
    expect(p.events()[0]?.user).toBeUndefined();
    expect(p.events()[0]?.tags).toBeUndefined();
    const owned = p.debugLines.filter((l) => /ctx\.metadata\.(runtime|source|level|truncated|truncated_keys) dropped/.test(l));
    expect(owned).toHaveLength(5);
    const identity = p.debugLines.filter((l) => /ctx\.metadata\.(user_id|tenant_id) dropped: use ctx\.user \/ ctx\.tags/.test(l));
    expect(identity).toHaveLength(2);
  });

  it('B12: per-call ctx.user replaces the scope user for one event only; ctx.tags merges over the scope tags', () => {
    // node: no autoIpAddress, so `user` is exactly what setUser / ctx.user gave.
    const p = setup({}, 'node');
    setUser({ id: 'usr_1' });
    setTag('feature', 'chats');
    captureException(new Error('a'), { user: { id: 'usr_override' }, tags: { region: 'eu', feature: null } });
    captureException(new Error('b'));
    const [e1, e2] = p.events();
    expect(e1?.user).toEqual({ id: 'usr_override' });
    expect(e1?.tags).toEqual({ region: 'eu' });
    expect(e2?.user).toEqual({ id: 'usr_1' });
    expect(e2?.tags).toEqual({ feature: 'chats' });
  });

  it('B12: scope calls made before the first init are kept and stamp the first event after it', () => {
    setUser({ id: 'usr_early' });
    setTag('feature', 'chats');
    const p = setup({}, 'node');
    captureException(new Error('x'));
    expect(p.events()[0]?.user).toEqual({ id: 'usr_early' });
    expect(p.events()[0]?.tags).toEqual({ feature: 'chats' });
    expect(p.events()[0]?.metadata).toEqual({ runtime: 'node' });
  });
});

describe('B21: deduplication is object identity, Error or not; primitives never', () => {
  it('B21: the same Error captured by the boundary and by the error listener → one event, one beforeSend call, the first id returned', () => {
    const hook = vi.fn((event) => event);
    const p = setup({ beforeSend: hook });
    const err = new Error('once');
    const first = captureException(err);
    const second = captureException(err, { metadata: { from: 'window.onerror' } });
    expect(second).toBe(first);
    expect(p.events()).toHaveLength(1);
    expect(hook).toHaveBeenCalledTimes(1);
  });

  it('B21: the same thrown { code: 42 } captured twice → one event (decision 39)', () => {
    const p = setup();
    const thrown = { code: 42 };
    const first = captureException(thrown);
    expect(captureException(thrown)).toBe(first);
    expect(p.events()).toHaveLength(1);
    // A different object with the same shape is a different event.
    expect(captureException({ code: 42 })).not.toBe(first);
    expect(p.events()).toHaveLength(2);
  });

  it("B21: the same thrown string 'nope' captured twice → two events", () => {
    const p = setup();
    const first = captureException('nope');
    const second = captureException('nope');
    expect(second).not.toBe(first);
    expect(p.events()).toHaveLength(2);
    expect(p.events().map((e) => e.event_id)).toEqual([first, second]);
  });

  it('B21: the mark survives a second init (module level, not per client)', () => {
    setup();
    const err = new Error('x');
    const first = captureException(err);
    const p = setup(); // drains the first client's one queued event into the new client
    expect(p.events()).toHaveLength(1);
    expect(captureException(err)).toBe(first);
    expect(p.events()).toHaveLength(1);
  });
});

describe('N10: release over 100 bytes', () => {
  it('N10: release_version omitted from every event, onError release_too_long once at init', () => {
    const p = setup({ release: 'x'.repeat(101) });
    captureException(new Error('a'));
    captureException(new Error('b'));
    expect(p.events().every((e) => !('release_version' in e))).toBe(true);
    expect(p.errors.map((e) => e.code)).toEqual(['release_too_long']);
  });

  it('N10: a release of exactly 100 bytes is sent verbatim, no trim, no v strip; empty → omitted', () => {
    const p = setup({ release: ' v' + 'x'.repeat(98) });
    captureException(new Error('a'));
    expect(p.events()[0]?.release_version).toBe(' v' + 'x'.repeat(98));
    expect(p.errors).toEqual([]);
    expect(getClient()?.options.release).toBe(' v' + 'x'.repeat(98));
    resetPipeline();
    const q = setup({ release: '' });
    captureException(new Error('b'));
    expect('release_version' in (q.events()[0] ?? {})).toBe(false);
  });
});

describe('N11: environment normalisation', () => {
  // Contract §4.2's rule applied literally: lower-cased, `-` for every disallowed character, leading non-[a-z0-9]
  // stripped, cut to 50 bytes, empty → production. The rule does not trim, so a trailing space becomes `-`.
  it.each([
    ['Production ', 'production-'],
    ['pre prod', 'pre-prod'],
    ['_Staging', 'staging'],
    ['', 'production'],
    ['---', 'production'],
    ['Éu/West 1', 'u-west-1'],
    ['a'.repeat(60), 'a'.repeat(50)],
  ])('N11: environment %j → %j sent, invalid_option environment once, debug shows before/after', (given, expected) => {
    const p = setup({ environment: given });
    captureException(new Error('x'));
    expect(p.events()[0]?.environment).toBe(expected);
    expect(p.errors.map((e) => [e.code, e.option])).toEqual([['invalid_option', 'environment']]);
    expect(p.debugLines.some((l) => l.includes(JSON.stringify(given)) && l.includes(JSON.stringify(expected)))).toBe(true);
  });

  it('N11: an already-valid environment fires nothing; omitted → production silently', () => {
    const p = setup({ environment: 'eu-west-1' });
    captureException(new Error('x'));
    expect(p.events()[0]?.environment).toBe('eu-west-1');
    expect(p.errors).toEqual([]);
    resetPipeline();
    const q = setup();
    captureException(new Error('x'));
    expect(q.events()[0]?.environment).toBe('production');
    expect(q.errors).toEqual([]);
  });
});

describe("N12: captureMessage's shape", () => {
  it('N12: level warning → error_class Message, message as given, top-level level warning, stacktrace []', () => {
    const p = setup();
    captureMessage('cache miss storm', { level: 'warning' });
    expect(p.events()[0]).toMatchObject({ error_class: 'Message', message: 'cache miss storm', stacktrace: [], level: 'warning', metadata: { runtime: 'browser' } });
  });

  it("N12: a string second argument is the level (D5) — captureMessage('x', 'debug')", () => {
    const p = setup();
    captureMessage('x', 'debug');
    expect(p.events()[0]?.level).toBe('debug');
  });

  it("N12: captureMessage('plain') defaults to level info (Sentry's default, D5)", () => {
    const p = setup();
    captureMessage('plain');
    expect(p.events()[0]?.level).toBe('info');
    expect(p.events()[0]?.metadata).toEqual({ runtime: 'browser' });
  });

  it('N12: captureException honours an explicit ctx.level (spec §3.2 — the 0.1.x ignore is removed)', () => {
    const p = setup();
    captureException(new Error('x'), { level: 'info' });
    expect(p.events()[0]?.level).toBe('info');
    expect(p.events()[0]?.metadata).toEqual({ runtime: 'browser' });
  });

  it('N12: captureException with no level defaults to error', () => {
    const p = setup();
    captureException(new Error('x'));
    expect(p.events()[0]?.level).toBe('error');
  });

  it('N12: beforeSend receives hint.originalException = the message string for captureMessage, the value for captureException', () => {
    const hints: unknown[] = [];
    setup({
      beforeSend: (event, hint) => {
        hints.push(hint.originalException);
        return event;
      },
    });
    const thrown = { code: 1 };
    captureMessage('msg');
    captureException(thrown);
    expect(hints).toEqual(['msg', thrown]);
  });
});

describe('N13: fingerprint', () => {
  it('N13: fingerprint sent verbatim, fingerprint_override absent, project_key / project_id never sent', () => {
    const p = setup();
    captureException(new Error('x'), { fingerprint: 'checkout-timeout' });
    const [event] = p.events();
    expect(event?.fingerprint).toBe('checkout-timeout');
    expect(Object.keys(event ?? {})).not.toContain('fingerprint_override');
    expect(Object.keys(event ?? {})).not.toContain('project_key');
    expect(Object.keys(event ?? {})).not.toContain('project_id');
    expect(Object.keys(event ?? {})).not.toContain('source');
  });

  it('N13: a 65-byte fingerprint twice → the field omitted, both events sent, fingerprint_too_long once per capture', () => {
    const p = setup();
    captureException(new Error('a'), { fingerprint: 'x'.repeat(65) });
    captureException(new Error('b'), { fingerprint: 'x'.repeat(65) });
    expect(p.events()).toHaveLength(2);
    expect(p.events().every((e) => !('fingerprint' in e))).toBe(true);
    expect(p.errors.map((e) => e.code)).toEqual(['fingerprint_too_long', 'fingerprint_too_long']);
  });

  it('N13: a 64-byte fingerprint is the bound, and bytes count — 22 three-byte characters are 66 bytes', () => {
    const p = setup();
    captureException(new Error('a'), { fingerprint: 'x'.repeat(64) });
    captureException(new Error('b'), { fingerprint: '€'.repeat(22) });
    expect(p.events().map((e) => e.fingerprint)).toEqual(['x'.repeat(64), undefined]);
    expect(p.errors.map((e) => e.code)).toEqual(['fingerprint_too_long']);
  });
});

describe('the event shape and the pinned invariants', () => {
  it('event_id is a v4 UUID, platform javascript on every runtime, timestamp ISO-8601, and the wire key set is ErrorPayload minus the three never sent', () => {
    for (const runtime of ['browser', 'node', 'edge'] as const) {
      resetPipeline();
      const p = setup({ release: '1.2.3' }, runtime);
      captureException(new Error('x'), { traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01', fingerprint: 'f' });
      const [event] = p.events();
      expect(event?.event_id).toMatch(UUID);
      expect(event?.platform).toBe('javascript');
      expect(event?.metadata.runtime).toBe(runtime);
      expect(new Date(event?.timestamp ?? '').toISOString()).toBe(event?.timestamp);
      const expectedKeys = [
        'event_id',
        'platform',
        'environment',
        'release_version',
        'error_class',
        'message',
        'stacktrace',
        'metadata',
        'timestamp',
        'fingerprint',
        'trace_id',
        'span_id',
        'trace_flags',
        'level',
        'sdk',
      ];
      // autoIpAddress (spec §3.4) defaults on and adds a user only on the browser runtime.
      if (runtime === 'browser') expectedKeys.push('user');
      expect(Object.keys(event ?? {}).sort()).toEqual(expectedKeys.sort());
    }
  });

  it('traceparent: flags 01 → trace_flags 1, flags 10 → 16 (parseInt hex), malformed → the three keys absent', () => {
    const p = setup();
    captureException(new Error('a'), { traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' });
    captureException(new Error('b'), { traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-10' });
    captureException(new Error('c'), { traceparent: '00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01' });
    captureException(new Error('d'), { traceparent: 'nonsense' });
    const [a, b, c, d] = p.events();
    expect(a).toMatchObject({ trace_id: '4bf92f3577b34da6a3ce929d0e0e4736', span_id: '00f067aa0ba902b7', trace_flags: 1 });
    expect(b?.trace_flags).toBe(16);
    for (const e of [c, d]) {
      expect(e).not.toHaveProperty('trace_id');
      expect(e).not.toHaveProperty('span_id');
      expect(e).not.toHaveProperty('trace_flags');
    }
  });

  it('message is cut to 10 000 bytes on a code-point boundary and recorded in truncated; error_class to 255', () => {
    const p = setup();
    const err = new Error('€'.repeat(20_000));
    err.name = 'N'.repeat(300);
    captureException(err);
    const [event] = p.events();
    expect(Buffer.byteLength(event?.message ?? '')).toBe(9999);
    expect(event?.message).toBe('€'.repeat(3333));
    expect(Buffer.byteLength(event?.error_class ?? '')).toBe(255);
    expect(event?.metadata.truncated).toEqual(['message', 'error_class']);
  });

  it('more than 100 frames → the innermost 100 kept and recorded; file cut to 512, function to 255', () => {
    const p = setup();
    const err = new Error('deep');
    const lines = ['Error: deep'];
    for (let i = 1; i <= 120; i++) lines.push(`    at ${'f'.repeat(i === 1 ? 300 : 2)}${i} (https://app/${'p'.repeat(i === 1 ? 600 : 1)}.js:${i}:1)`);
    err.stack = lines.join('\n');
    captureException(err);
    const [event] = p.events();
    expect(event?.stacktrace).toHaveLength(100);
    expect(event?.stacktrace[0]?.line).toBe(1);
    expect(event?.stacktrace[99]?.line).toBe(100);
    expect(Buffer.byteLength(event?.stacktrace[0]?.file ?? '')).toBe(512);
    expect(Buffer.byteLength(event?.stacktrace[0]?.function ?? '')).toBe(255);
    expect(event?.metadata.truncated).toEqual(['stacktrace', 'stacktrace.file', 'stacktrace.function']);
  });

  it('before the first init a capture returns a fresh id and drops the event without marking it', () => {
    const err = new Error('early');
    const early = captureException(err);
    expect(early).toMatch(UUID);
    expect(((globalThis as Record<symbol, unknown>)[SLOT] as { client?: unknown }).client).toBeUndefined();
    const p = setup();
    const later = captureException(err);
    expect(later).not.toBe(early);
    expect(p.events()).toHaveLength(1);
  });

  it('a throwing beforeSend → before_send_threw with cause, the event sent unmodified', () => {
    const boom = new Error('hook broke');
    const p = setup({
      beforeSend: (event) => {
        event.message = 'mutated before the throw';
        throw boom;
      },
    });
    captureException(new Error('original'));
    expect(p.events().map((e) => e.message)).toEqual(['original']);
    expect(p.errors.map((e) => [e.code, e.cause])).toEqual([['before_send_threw', boom]]);
  });

  it('a beforeSend result whose own property access throws (not the hook itself) is not lost silently — before_send_threw, sent unmodified', () => {
    const boom = new Error('getter broke');
    const p = setup({
      beforeSend: (event) =>
        ({
          ...event,
          get user(): never {
            throw boom;
          },
        }) as unknown as typeof event,
    });
    captureException(new Error('original'));
    expect(p.events().map((e) => e.message)).toEqual(['original']);
    expect(p.errors.map((e) => [e.code, e.cause])).toEqual([['before_send_threw', boom]]);
  });

  it('a hook returning a new object with event_id deleted / platform node / metadata.token / runtime x / source custom → re-stamped, re-scrubbed, source kept', () => {
    const p = setup({
      beforeSend: (event) => ({
        ...event,
        event_id: undefined as unknown as string,
        platform: 'node' as 'javascript',
        timestamp: 'yesterday',
        metadata: { ...event.metadata, token: 'abc', runtime: 'x', source: 'custom' },
      }),
    });
    const id = captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.event_id).toBe(id);
    expect(event?.platform).toBe('javascript');
    expect(new Date(event?.timestamp ?? '').toISOString()).toBe(event?.timestamp);
    expect(event?.metadata).toMatchObject({ token: '[FILTERED]', runtime: 'browser', source: 'custom' });
  });

  it('a hook returning undefined / false / a string drops the event with a debug line and no onError', () => {
    for (const result of [undefined, false, 'x']) {
      resetPipeline();
      const p = setup({ beforeSend: () => result as unknown as null });
      captureException(new Error('x'));
      expect(p.events()).toHaveLength(0);
      expect(p.errors).toEqual([]);
      expect(p.debugLines.some((l) => l.includes('dropped'))).toBe(true);
    }
  });
});

describe('§11.2 TASK-01/02: a hostile beforeSend result is re-checked, not trusted', () => {
  it('A3: user is rebuilt from the returned object — an unknown key dropped, id coerced, not smuggled through verbatim', () => {
    const p = setup({
      beforeSend: (event) => ({ ...event, user: { id: 123, unknown_key: 'x', email: 'a@b.c' } }) as unknown as typeof event,
    });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.user).toEqual({ id: '123', email: 'a@b.c' });
  });

  it('A4: tags are re-validated and re-capped at 50 — a hook returning 61 tags (z00..z60) keeps only 50, with the tags marker', () => {
    const tags: Record<string, string> = {};
    for (let i = 0; i <= 60; i++) tags[`z${String(i).padStart(2, '0')}`] = String(i);
    const p = setup({ beforeSend: (event) => ({ ...event, tags }) as unknown as typeof event });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(Object.keys(event?.tags ?? {})).toHaveLength(50);
    expect(event?.metadata.truncated).toContain('tags');
  });

  it('A5: hook breadcrumbs over 100 are cut to the LAST 100, not the first', () => {
    const breadcrumbs = Array.from({ length: 150 }, (_, i) => ({ timestamp: new Date().toISOString(), type: 'default', message: `m${i}`, level: 'info' }));
    const p = setup({ beforeSend: (event) => ({ ...event, breadcrumbs }) as unknown as typeof event });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.breadcrumbs).toHaveLength(100);
    expect(event?.breadcrumbs?.[0]?.message).toBe('m50');
    expect(event?.breadcrumbs?.[99]?.message).toBe('m149');
  });

  it('A6: sdk is re-stamped from the pre-hook value — a hook-supplied name/version is discarded', () => {
    const p = setup({ beforeSend: (event) => ({ ...event, sdk: { name: 'evil.sdk', version: '0.0.0-evil' } }) });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.sdk).toEqual({ name: 'sentinel.javascript.browser', version: expect.any(String) });
    expect(event?.sdk?.name).not.toBe('evil.sdk');
  });

  it('H1: applyServerBounds runs again after the hook — a 65-code-point context key and one with U+0000 are removed', () => {
    const longKey = 'k'.repeat(65);
    const p = setup({
      beforeSend: (event) => ({ ...event, contexts: { [longKey]: { a: 1 }, 'bad\u0000name': { b: 2 }, ok: { c: 3 } } }) as unknown as typeof event,
    });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.contexts).toEqual({ ok: { c: 3 } });
  });

  it('H2/M9: boundRequestStrings runs again after the hook — a request.url over 2048 code points is cut back down', () => {
    const hugeUrl = '/' + 'a'.repeat(3000);
    const p = setup({ beforeSend: (event) => ({ ...event, request: { url: hugeUrl } }) as unknown as typeof event });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(Array.from(event?.request?.url ?? '').length).toBeLessThanOrEqual(2048);
  });

  it('spec §4.4: the 131 072-byte breadcrumb total is applied to the REBUILT breadcrumbs in afterHook, not just at build', () => {
    const breadcrumbs = Array.from({ length: 100 }, (_, i) => ({ timestamp: new Date().toISOString(), type: 'default', message: 'm'.repeat(4000), level: 'info' as const }));
    const p = setup({ beforeSend: (event) => ({ ...event, breadcrumbs }) as unknown as typeof event }, 'node');
    captureException(new Error('x'));
    const [event] = p.events();
    // 100 crumbs of ~4000 chars is ~411 KB of Go-ruler bytes, far over the 131 072 total — must be trimmed.
    expect(event?.breadcrumbs?.length ?? 0).toBeLessThan(100);
  });
});

describe('the URL privacy filter runs on every capture path (spec §5.1)', () => {
  it('captureException: the error message is filtered', () => {
    const p = setup();
    captureException(new Error('failed at /reset-password?reset_token=X'));
    expect(p.events()[0]?.message).toBe('failed at /reset-password?reset_token=[Filtered]');
  });

  it('captureMessage: a message with a fragment is filtered (the fragment dropped)', () => {
    const p = setup();
    captureMessage('see https://h/q?token=t#frag');
    expect(p.events()[0]?.message).toBe('see https://h/q?token=[Filtered]');
  });

  it('a message of exactly MAX_MESSAGE_BYTES ending in a token is filtered, then cut, with the truncated marker', () => {
    const p = setup();
    const message = 'x'.repeat(MAX_MESSAGE_BYTES - 11) + ' /r?token=X';
    expect(Buffer.byteLength(message)).toBe(MAX_MESSAGE_BYTES);
    captureException(new Error(message));
    const [event] = p.events();
    expect(Buffer.byteLength(event?.message ?? '')).toBeLessThanOrEqual(MAX_MESSAGE_BYTES);
    expect(event?.metadata.truncated).toContain('message');
  });

  it('a beforeSend that sets message is not filtered again — the app owns what it writes', () => {
    const p = setup({ beforeSend: (event) => ({ ...event, message: '/r?token=t' }) });
    captureException(new Error('x'));
    expect(p.events()[0]?.message).toBe('/r?token=t');
  });

  it('stacktrace[].file is filtered, then cut (spec §5.1)', () => {
    const p = setup();
    const err = new Error('boom');
    err.stack = 'Error: boom\n    at f (https://app/reset-password?reset_token=X:1:2)';
    captureException(err);
    expect(p.events()[0]?.stacktrace[0]?.file).toBe('https://app/reset-password?reset_token=[Filtered]');
  });
});
