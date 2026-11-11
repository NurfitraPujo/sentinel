// The browser keepalive trim (spec §4.5) and the NATS publish shed (spec §4.6), wired through captureException /
// enqueue. Shuffle-safe: every test starts from a deleted slot.

import { beforeEach, describe, expect, it } from 'vitest';
import { captureException } from '../../src/core/capture.js';
import { applyBrowserKeepaliveTrim, applyPublishShed, KEEPALIVE_BUDGET, TUNNEL_LAST_RESORT_BUDGET } from '../../src/core/size.js';
import { NATS_PUBLISH_BUDGET, protoSizeEstimate } from '../../src/core/proto-size.js';
import { addBreadcrumb, setContext, setTag, setTenant } from '../../src/core/scope.js';
import { getInternalClient } from '../../src/core/state.js';
import type { SentinelEvent } from '../../src/core/types.js';
import { resetPipeline, setup } from '../helpers/pipeline.js';

beforeEach(resetPipeline);

function jsonBytes(event: unknown): number {
  return new TextEncoder().encode(JSON.stringify(event)).byteLength;
}

describe('the browser keepalive trim (spec §4.5)', () => {
  it('100 crumbs of ~2 KiB each are trimmed oldest-first until the event fits 61 440 bytes', () => {
    const p = setup({ maxBreadcrumbs: 100 }, 'browser');
    for (let i = 0; i < 100; i++) addBreadcrumb({ message: `${i}-${'x'.repeat(2000)}` });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(jsonBytes(event) + 2).toBeLessThanOrEqual(KEEPALIVE_BUDGET);
    expect(event?.breadcrumbs?.length).toBeGreaterThan(0);
    expect(event?.breadcrumbs?.length).toBeLessThan(100);
    // Oldest dropped: the kept crumbs are the most recent ones, ending at 99.
    const last = event?.breadcrumbs?.at(-1)?.message;
    expect(last?.startsWith('99-')).toBe(true);
  });

  it('the same crumbs on node are left untrimmed (no keepalive cap off-browser)', () => {
    const p = setup({ maxBreadcrumbs: 100 }, 'node');
    for (let i = 0; i < 40; i++) addBreadcrumb({ message: `${i}-${'x'.repeat(2000)}` });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.breadcrumbs?.length).toBe(40);
  });

  it('rule 4: still over the budget with no breadcrumbs left is sent alone', () => {
    const p = setup({}, 'browser');
    // Near the metadata cap (spec §3.7's own example): big enough to push the whole event over 61 440
    // even with no breadcrumbs at all — applyBounds cuts message/error_class, so those cannot do this.
    captureException(new Error('x'), { metadata: { big: 'y'.repeat(65_480) } });
    const [event] = p.events();
    expect(event?.breadcrumbs).toBeUndefined();
    expect(jsonBytes(event) + 2).toBeGreaterThan(KEEPALIVE_BUDGET);
  });

  // With no breadcrumbs, metadata (≤ 65 536), contexts (≤ 65 536) and tags (≤ ~11 600) alone can never reach
  // 262 142 — a 100-frame stacktrace (≤ ~820 bytes/frame) and a 10 000-byte message close the gap, which is
  // why rule 5 is tested here on a hand-built event rather than through captureException (whose own bounds
  // already keep every field this small in practice).
  function bigEvent(): SentinelEvent {
    return {
      event_id: '00000000-0000-4000-8000-000000000001',
      platform: 'javascript',
      environment: 'production',
      error_class: 'Error',
      message: 'x'.repeat(150_000),
      stacktrace: Array.from({ length: 100 }, (_v, i) => ({ file: 'https://app/' + 'f'.repeat(495) + `${i}.js`, function: 'g'.repeat(255), in_app: true, line: 1 })),
      metadata: { runtime: 'browser', big: 'y'.repeat(65_480) },
      timestamp: new Date().toISOString(),
      level: 'error',
      sdk: { name: 'sentinel.javascript.browser', version: '0.2.0' },
    };
  }

  it('rule 5 last resort: contexts then every tag but tenant_id are removed once over 262 142 bytes', () => {
    setup({}, 'browser');
    const client = getInternalClient();
    if (client === undefined) throw new Error('no client');
    const event = bigEvent();
    event.contexts = { big: { blob: 'y'.repeat(65_500) } };
    event.tags = { tenant_id: 'org_a', feature: 'chats' };
    expect(jsonBytes(event)).toBeGreaterThan(TUNNEL_LAST_RESORT_BUDGET);
    applyBrowserKeepaliveTrim(client, event);
    expect(jsonBytes(event)).toBeLessThanOrEqual(TUNNEL_LAST_RESORT_BUDGET);
    expect(event.contexts).toBeUndefined();
    expect(event.tags).toEqual({ tenant_id: 'org_a' });
    expect(event.metadata.truncated).toEqual(expect.arrayContaining(['contexts', 'tags']));
  });

  it('an event with no other tag and no contexts, still over the last-resort budget, has nothing left to remove', () => {
    setup({}, 'browser');
    const client = getInternalClient();
    if (client === undefined) throw new Error('no client');
    const event = bigEvent();
    applyBrowserKeepaliveTrim(client, event);
    expect(event.contexts).toBeUndefined();
    expect(event.tags).toBeUndefined();
  });

  it("rule 5, tenant_id only: when tags is already exactly { tenant_id }, nothing is removed and no 'tags' marker is written", () => {
    setup({}, 'browser');
    const client = getInternalClient();
    if (client === undefined) throw new Error('no client');
    const event = bigEvent();
    event.tags = { tenant_id: 't' };
    applyBrowserKeepaliveTrim(client, event);
    expect(event.tags).toEqual({ tenant_id: 't' });
    expect(event.metadata.truncated ?? []).not.toContain('tags');
  });
});

describe('the NATS publish shed (spec §4.6)', () => {
  it('an ordinary event is never touched — protoSizeEstimate stops at step 1', () => {
    const p = setup({}, 'node');
    addBreadcrumb({ message: 'one' });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(protoSizeEstimate(event as unknown as Record<string, unknown>)).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
  });

  it('breadcrumbs are shed first, oldest first, when the estimate is over the budget (spec review round 7\'s case)', () => {
    // A proto number costs 9 bytes against its JSON 1-2 (§4.6): 32 crumbs of data: { a: Array(2030).fill(0) }
    // (~4062 JSON bytes each, under the 4096 per-crumb cap) plus setExtra('a', Array(32_700).fill(0)) is
    // spec's own worked example — 194 490 JSON bytes, 1 053 681 as proto.
    const p = setup({ maxBreadcrumbs: 100 }, 'node');
    for (let i = 0; i < 32; i++) addBreadcrumb({ message: `${i}`, data: { a: Array(2030).fill(0) } });
    captureException(new Error('x'), { metadata: { a: Array(32_700).fill(0) } });
    const [event] = p.events();
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
    expect((event?.breadcrumbs?.length ?? 0)).toBeLessThan(32);
    expect(event?.metadata.truncated).toContain('breadcrumbs');
  });

  it('case (1b): a publishable event is sent whole — 28 crumbs stay under the budget without shedding (spec review round 10)', () => {
    // The same setExtra as case (1), but four fewer crumbs: 988 224 model bytes, 62 026 below 1 048 576 —
    // step 1 stops. Shown red by revision 11's 900 000 budget, which would remove 4 crumbs here.
    const p = setup({ maxBreadcrumbs: 100 }, 'node');
    for (let i = 0; i < 28; i++) addBreadcrumb({ message: `${i}`, data: { a: Array(2030).fill(0) } });
    captureException(new Error('x'), { metadata: { a: Array(32_700).fill(0) } });
    const [event] = p.events();
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
    expect(event?.breadcrumbs?.length).toBe(28);
    expect(event?.metadata.truncated).toBeUndefined();
  });

  it('case (3): contexts are shed before metadata, non-well-known first, when breadcrumbs alone are not enough', () => {
    const p = setup(
      {
        beforeSend: (event) => {
          const headers: Record<string, string> = {};
          for (let i = 0; i < 64; i++) headers[`h${String(i).padStart(2, '0')}`] = '€'.repeat(2000);
          return { ...event, request: { headers } };
        },
      },
      'node',
      () => ({ contexts: { runtime: { name: 'node' }, os: { name: 'linux' } } }),
    );
    setContext('c', { a: Array(32_700).fill(0) });
    captureException(new Error('x'), { metadata: { a: Array(32_700).fill(0) } });
    const [event] = p.events();
    expect(event?.contexts?.c).toBeUndefined();
    expect(event?.contexts?.runtime).toBeDefined();
    expect(event?.contexts?.os).toBeDefined();
    expect(event?.metadata.truncated).toContain('contexts');
    expect(event?.metadata.truncated ?? []).not.toContain('breadcrumbs');
    expect(event?.metadata.a).toBeDefined();
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
  });

  it('case (7): the browser, corrected (spec review round 8) — a metadata extra that alone rules out keepalive still leaves contexts and metadata untouched by §4.6', () => {
    // metadata.a alone is ~65 400 JSON bytes (within the 65 536 contexts/metadata build bound), so no event
    // carrying it fits 61 440: §4.5 removes every crumb and sends the event alone, without keepalive. §4.6
    // step 1 then stops on its own (the model's estimate is well under budget), so there is no 'contexts'
    // marker and metadata.a survives. Revision 9 claimed §4.5 could trim this event to fit 61 440, which it
    // cannot.
    const p = setup(
      { maxBreadcrumbs: 100 },
      'browser',
      () => ({ contexts: { device: { arch: 'x64' } } }),
    );
    for (let i = 0; i < 32; i++) addBreadcrumb({ message: `${i}`, data: { a: Array(2030).fill(0) } });
    captureException(new Error('x'), { metadata: { a: Array(32_700).fill(0) } });
    const [event] = p.events();
    expect(event?.breadcrumbs).toBeUndefined();
    expect(jsonBytes(event) + 2).toBeGreaterThan(KEEPALIVE_BUDGET);
    expect(event?.metadata.truncated).toEqual(['breadcrumbs']);
    expect(event?.metadata.a).toBeDefined();
    expect(event?.contexts).toBeDefined();
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
  });

  it('case (8): a context over the server\'s bound is shed by §3.10 at build time and costs no breadcrumbs (spec review round 8)', () => {
    const p = setup(
      { maxBreadcrumbs: 100 },
      'node',
      () => ({ contexts: { runtime: { name: 'node' }, os: { name: 'linux' } } }),
    );
    for (let i = 0; i < 100; i++) addBreadcrumb({ message: 'm'.repeat(1_190) });
    setContext('big', { s: 'x'.repeat(1_200_000) });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.breadcrumbs?.length).toBe(100);
    expect(event?.contexts?.big).toBeUndefined();
    expect(event?.contexts?.runtime).toBeDefined();
    expect(event?.contexts?.os).toBeDefined();
    expect(event?.metadata.truncated).toContain('contexts');
    expect(event?.metadata.truncated ?? []).not.toContain('breadcrumbs');
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
  });

  it('case (8), the numeric-array variant: the same holds for a ~1.1 MB proto context', () => {
    const p = setup({ maxBreadcrumbs: 100 }, 'node');
    for (let i = 0; i < 100; i++) addBreadcrumb({ message: 'm'.repeat(1_190) });
    setContext('big', { a: Array(100_000).fill(0) });
    captureException(new Error('x'));
    const [event] = p.events();
    expect(event?.breadcrumbs?.length).toBe(100);
    expect(event?.contexts?.big).toBeUndefined();
    expect(event?.metadata.truncated).toContain('contexts');
    expect(event?.metadata.truncated ?? []).not.toContain('breadcrumbs');
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
  });

  // Given §3.7's 65 536-byte metadata cap and §3.10's 65 536-byte contexts cap (both already enforced at
  // build/afterHook, before this step ever runs) and the ~5.5x worst-case proto/JSON ratio, metadata and
  // contexts together can never exceed the 1 000 000 budget on their own — only breadcrumbs (capped at
  // 131 072 JSON bytes) can push the total that high, and removing them is always enough to satisfy it. So
  // step 3 (contexts) is exercised directly here, on a hand-built event that bypasses §3.10 entirely — the
  // shape a `beforeSend` could still momentarily produce before `afterHook`'s own §3.10 pass runs.
  it('applyPublishShed removes the largest non-well-known context first when still over budget', () => {
    setup({}, 'node');
    const client = getInternalClient();
    if (client === undefined) throw new Error('no client');
    const event: SentinelEvent = {
      event_id: '00000000-0000-4000-8000-000000000001',
      platform: 'javascript',
      environment: 'production',
      error_class: 'Error',
      message: 'x',
      stacktrace: [],
      metadata: { runtime: 'node' },
      timestamp: new Date().toISOString(),
      level: 'error',
      sdk: { name: 'sentinel.javascript.node', version: '0.2.0' },
      contexts: {
        small: { v: 'x' },
        big: { a: Array(100_000).fill(0) },
      },
    };
    expect(protoSizeEstimate(event as unknown as Record<string, unknown>)).toBeGreaterThan(NATS_PUBLISH_BUDGET);
    applyPublishShed(client, event);
    expect(protoSizeEstimate(event as unknown as Record<string, unknown>)).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
    expect(event.contexts?.big).toBeUndefined();
    expect(event.contexts?.small).toEqual({ v: 'x' });
    expect(event.metadata.truncated).toContain('contexts');
  });

  it('metadata is shed through shrinkMetadata with the estimate measure once breadcrumbs and contexts are gone', () => {
    const p = setup({}, 'node');
    const big: Record<string, unknown> = {};
    for (let i = 0; i < 50; i++) big[`k${i}`] = Array(5000).fill(0);
    captureException(new Error('x'), { metadata: big });
    const [event] = p.events();
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
    expect(event?.metadata.runtime).toBe('node');
  });

  // §11.2 TASK-02 case (4): no breadcrumbs and only one non-well-known context name (`a`, kept whole by §3.10 —
  // its metadata JSON is under the 65 536 build-time bound, so this exercises step 4 for real, unlike the
  // vacuous case above where afterHook's own shrink had already done the work.
  it('case (4): with nothing else to shed, both well-known contexts are removed largest first, then the shrink removes the metadata key', () => {
    const p = setup(
      {
        beforeSend: (event) => {
          const headers: Record<string, string> = {};
          for (let i = 0; i < 64; i++) headers[`h${String(i).padStart(2, '0')}`] = '€'.repeat(4096);
          return { ...event, request: { headers } };
        },
      },
      'node',
    );
    captureException(new Error('x'), { metadata: { a: Array(32_700).fill(0) } });
    const [event] = p.events();
    const unshedCrumbsAndContexts = event?.contexts;
    void unshedCrumbsAndContexts;
    expect(event?.contexts).toBeUndefined();
    expect(event?.metadata.truncated_keys).toContain('a');
    expect(event?.metadata).not.toHaveProperty('a');
    expect(event?.metadata.runtime).toBe('node');
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
  });

  it('S7/M11: a well-known context is shed only after every non-well-known one, even when either removal alone would fit the budget', () => {
    setup({}, 'node');
    const client = getInternalClient();
    if (client === undefined) throw new Error('no client');
    function eventWith(contexts: Record<string, Record<string, unknown>>): SentinelEvent {
      return {
        event_id: '00000000-0000-4000-8000-000000000004',
        platform: 'javascript',
        environment: 'production',
        error_class: 'Error',
        message: 'x',
        stacktrace: [],
        metadata: { runtime: 'node' },
        timestamp: new Date().toISOString(),
        level: 'error',
        sdk: { name: 'sentinel.javascript.node', version: '0.2.0' },
        contexts,
      };
    }
    // "os" (well-known) is bigger than "custom" (not well-known), and removing EITHER ONE ALONE would already
    // bring the estimate under budget — so the outcome only depends on which one is tried first, and the spec
    // says non-well-known goes first, however small, even though "os" alone would have been the bigger win.
    const os = { a: Array(60_000).fill(0) };
    const custom = { a: Array(45_000).fill(0) };
    const full = eventWith({ os, custom });
    expect(protoSizeEstimate(full as unknown as Record<string, unknown>)).toBeGreaterThan(NATS_PUBLISH_BUDGET);
    expect(protoSizeEstimate(eventWith({ custom }) as unknown as Record<string, unknown>)).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
    expect(protoSizeEstimate(eventWith({ os }) as unknown as Record<string, unknown>)).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
    applyPublishShed(client, full);
    expect(full.contexts?.custom).toBeUndefined();
    expect(full.contexts?.os).toBeDefined();
  });

  it('case (5) / S5\', M7: an event still over the budget once every shed is exhausted is sent as it stands, never dropped', () => {
    const p = setup(
      {
        beforeSend: (event) => {
          const headers: Record<string, string> = {};
          const env: Record<string, string> = {};
          for (let i = 0; i < 80; i++) {
            headers[`h${String(i).padStart(2, '0')}`] = '€'.repeat(4096);
            env[`e${String(i).padStart(2, '0')}`] = '€'.repeat(1024);
          }
          return { ...event, request: { headers, env, query_string: '€'.repeat(4096) } };
        },
      },
      'node',
    );
    captureException(new Error('€'.repeat(3_333)), { metadata: { a: Array(32_700).fill(0) } });
    const [event] = p.events();
    // The server's own bound (§3.10) already cut both maps to 64 entries before this step ever runs.
    expect(Object.keys(event?.request?.headers ?? {})).toHaveLength(64);
    expect(Object.keys(event?.request?.env ?? {})).toHaveLength(64);
    expect(event?.request?.query_string).toBeDefined();
    expect(event?.message?.length).toBeGreaterThan(0);
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeGreaterThan(NATS_PUBLISH_BUDGET);
    // Not dropped: the event is still exactly one in the queue, still carrying its message, with no onError.
    expect(p.events()).toHaveLength(1);
    expect(p.errors).toEqual([]);
  });

  it("S1/M1: the 'breadcrumbs' marker is measured INSIDE the keepalive loop — a removal that fits without the marker but not with it removes one more crumb", () => {
    setup({}, 'browser');
    const client = getInternalClient();
    if (client === undefined) throw new Error('no client');
    function crumb(i: number): SentinelEvent['breadcrumbs'] extends (infer T)[] | undefined ? T : never {
      return { timestamp: new Date().toISOString(), type: 'default', message: `c${i}`, level: 'info' } as never;
    }
    function candidate(padLen: number): SentinelEvent {
      return {
        event_id: '00000000-0000-4000-8000-000000000003',
        platform: 'javascript',
        environment: 'production',
        error_class: 'Error',
        message: 'x',
        stacktrace: [],
        metadata: { runtime: 'browser', pad: 'p'.repeat(padLen) },
        timestamp: new Date().toISOString(),
        level: 'error',
        sdk: { name: 'sentinel.javascript.browser', version: '0.2.0' },
        breadcrumbs: [crumb(0), crumb(1), crumb(2)],
      };
    }
    // Binary-search a pad length so that, with all three crumbs, the event is over budget; after removing
    // the oldest crumb it is under budget WITHOUT the marker but the marker (~28 bytes) would tip it back
    // over — the exact boundary §11.2 TASK-02 pins. Search over the boundary directly on the model, then
    // build the real event and run it through the real function.
    let lo = 0;
    let hi = 65_000;
    let padLen = -1;
    while (lo <= hi) {
      const mid = Math.floor((lo + hi) / 2);
      const full = candidate(mid);
      const oneRemoved = candidate(mid);
      oneRemoved.breadcrumbs = (oneRemoved.breadcrumbs ?? []).slice(1);
      const bytesFull = jsonBytes(full) + 2;
      const bytesOneRemovedNoMarker = jsonBytes(oneRemoved) + 2;
      if (bytesFull > KEEPALIVE_BUDGET && bytesOneRemovedNoMarker <= KEEPALIVE_BUDGET && bytesOneRemovedNoMarker > KEEPALIVE_BUDGET - 28) {
        padLen = mid;
        break;
      }
      if (bytesOneRemovedNoMarker > KEEPALIVE_BUDGET) hi = mid - 1;
      else lo = mid + 1;
    }
    expect(padLen, 'could not locate the marker boundary window').toBeGreaterThanOrEqual(0);
    const event = candidate(padLen);
    applyBrowserKeepaliveTrim(client, event);
    // With the marker measured inside the loop, the boundary crumb is also removed: two are shed, one kept.
    expect(event.breadcrumbs?.length).toBe(1);
    expect(event.breadcrumbs?.[0]?.message).toBe('c2');
    expect(event.metadata.truncated).toEqual(['breadcrumbs']);
    expect(jsonBytes(event) + 2).toBeLessThanOrEqual(KEEPALIVE_BUDGET);
  });

  it('M2/M5: near the metadata cap, the keepalive marker\'s shrinkMetadata re-run removes the offending key so crumbs are kept, not all shed', () => {
    const p = setup({}, 'browser');
    const client = getInternalClient();
    if (client === undefined) throw new Error('no client');
    const event: SentinelEvent = {
      event_id: '00000000-0000-4000-8000-000000000002',
      platform: 'javascript',
      environment: 'production',
      error_class: 'Error',
      message: 'x',
      stacktrace: [],
      metadata: { runtime: 'browser', route: 'x'.repeat(65_480) },
      timestamp: new Date().toISOString(),
      level: 'error',
      sdk: { name: 'sentinel.javascript.browser', version: '0.2.0' },
      breadcrumbs: [
        { timestamp: new Date().toISOString(), type: 'default', message: 'one', level: 'info' },
        { timestamp: new Date().toISOString(), type: 'default', message: 'two', level: 'info' },
        { timestamp: new Date().toISOString(), type: 'default', message: 'three', level: 'info' },
      ],
    };
    applyBrowserKeepaliveTrim(client, event);
    // Deleting the re-run would shed every crumb chasing a 65 KB metadata that never shrinks; with the
    // re-run, `route` is dropped instead and crumbs survive.
    expect(event.breadcrumbs?.length).toBeGreaterThan(0);
    expect(event.metadata).not.toHaveProperty('route');
    expect(event.metadata.truncated_keys).toContain('route');
    expect(event.metadata.truncated).toContain('breadcrumbs');
    expect(jsonBytes(event) + 2).toBeLessThanOrEqual(KEEPALIVE_BUDGET);
    void p;
  });

  it("case (10): the metadata shrink never drops — a browser event with a near-cap extra keeps two of three crumbs, not none (spec review rounds 9 and 10)", () => {
    // setExtra('route', 'x'.repeat(65_480)) leaves metadata (with runtime) at 65 512 bytes. The event cannot
    // fit 61 440, so §4.5 removes the first crumb; the 'breadcrumbs' marker takes metadata to 65 540, and the
    // re-run removes route instead of every remaining crumb. Shown red by revision 11's rule (the marker
    // withdrawn, every crumb removed, sent without keepalive) and revision 10's (dropped).
    const p = setup({ maxBreadcrumbs: 100 }, 'browser');
    addBreadcrumb({ message: 'one' });
    addBreadcrumb({ message: 'two' });
    addBreadcrumb({ message: 'three' });
    captureException(new Error('x'), { metadata: { route: 'x'.repeat(65_480) } });
    const [event] = p.events();
    expect(event?.breadcrumbs?.length).toBe(2);
    expect(event?.breadcrumbs?.[0]?.message).toBe('two');
    expect(event?.breadcrumbs?.[1]?.message).toBe('three');
    expect(Object.keys(event?.metadata ?? {}).sort()).toEqual(['runtime', 'truncated', 'truncated_keys']);
    expect(event?.metadata.truncated).toEqual(['breadcrumbs']);
    expect(event?.metadata.truncated_keys).toEqual(['route']);
    expect(jsonBytes(event) + 2).toBeLessThanOrEqual(KEEPALIVE_BUDGET);
    // No metadata_too_large report anywhere on this path.
    expect(p.errors).toEqual([]);
  });

  it('case (10), node through §4.6 step 2: the same near-cap extra survives with the newest crumbs and the context kept', () => {
    const p = setup({ maxBreadcrumbs: 100 }, 'node');
    setContext('c', { a: Array(32_700).fill(0) });
    for (let i = 0; i < 32; i++) addBreadcrumb({ message: `${i}`, data: { a: Array(2030).fill(0) } });
    captureException(new Error('x'), { metadata: { route: 'x'.repeat(65_480) } });
    const [event] = p.events();
    expect((event?.breadcrumbs?.length ?? 0)).toBeGreaterThan(0);
    expect((event?.breadcrumbs?.length ?? 0)).toBeLessThan(32);
    expect(event?.contexts?.c).toBeDefined();
    expect(event?.metadata.truncated).toContain('breadcrumbs');
    expect(event?.metadata.truncated_keys).toContain('route');
    expect(event?.metadata).not.toHaveProperty('route');
    const estimate = protoSizeEstimate(event as unknown as Record<string, unknown>);
    expect(estimate).toBeLessThanOrEqual(NATS_PUBLISH_BUDGET);
    expect(p.errors).toEqual([]);
  });

  it("case (10), the coordinator's pin: a lone near-cap extra is sent from build with the extra removed, never dropped", () => {
    const p = setup({}, 'browser');
    captureException(new Error('x'), { metadata: { route: 'x'.repeat(70_000) } });
    const [event] = p.events();
    expect(event).toBeDefined();
    expect(event?.metadata).not.toHaveProperty('route');
    expect(event?.metadata.truncated_keys).toContain('route');
    expect(event?.metadata.runtime).toBe('browser');
    expect(p.errors).toEqual([]);
  });
});
