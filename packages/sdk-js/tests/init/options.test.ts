// `init`'s option degradation on the node entry (contract §4.2 *Numeric options degrade, never throw*, the
// `endpoint` / `environment` rows, *Unconfigured server runtime*): B25, N15, N11, N4. Through the real
// `@sentinel/sdk-js/node` `init` with `fetch` mocked, so "nothing is ever posted" is "the spy was never called".
// Shuffle-safe: every test starts from a deleted slot with the previous client's hooks uninstalled.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { captureException, captureMessage, close, flush, getClient, init } from '../../src/node.js';
import { errorRecorder, mockFetch } from '../helpers/fetch-mock.js';
import { resetPipeline } from '../helpers/pipeline.js';
import { advance, useFakeClock } from '../helpers/transport.js';

const ENDPOINT = 'http://127.0.0.1:1/ingest';
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const ENV_KEYS = ['SENTINEL_ENDPOINT', 'SENTINEL_API_KEY', 'SENTINEL_PROJECT_KEY'] as const;
let savedEnv: Record<string, string | undefined> = {};

beforeEach(() => {
  resetPipeline();
  savedEnv = Object.fromEntries(ENV_KEYS.map((k) => [k, process.env[k]]));
  for (const k of ENV_KEYS) delete process.env[k];
});

afterEach(async () => {
  vi.useRealTimers();
  await close(50);
  for (const k of ENV_KEYS) {
    if (savedEnv[k] === undefined) delete process.env[k];
    else process.env[k] = savedEnv[k];
  }
});

describe('B25: numeric options degrade, never throw', () => {
  it('B25: init({ maxBufferSize: 0, batchSize: 500, batchWaitMs: NaN, sampleRate: NaN }) then flush(-1) — the client is live on the defaults, batchSize 50, sampleRate 1; invalid_option once per option, asserted as a set; flush used its 2000 ms default', async () => {
    useFakeClock();
    const f = mockFetch();
    f.hold();
    const r = errorRecorder();
    expect(() => init({ endpoint: ENDPOINT, apiKey: 'k', maxBufferSize: 0, batchSize: 500, batchWaitMs: Number.NaN, sampleRate: Number.NaN, onError: r.onError })).not.toThrow();
    const client = getClient();
    expect(client).toMatchObject({ runtime: 'node', options: { maxBufferSize: 100, batchSize: 50, batchWaitMs: 1000, sampleRate: 1 } });

    // The client is live: an event is enqueued and posted (held by the mock so the timeout is observable).
    captureMessage('live');
    const flushed = flush(-1);
    await advance(1999);
    let resolved: boolean | undefined;
    void flushed.then((v) => (resolved = v));
    await advance(0);
    expect(resolved).toBeUndefined();
    await advance(1);
    await expect(flushed).resolves.toBe(false);
    expect(f.posts).toHaveLength(1);

    // The SET of options reported, one report each — never the order (contract §11).
    const reported = r.errors.filter((e) => e.code === 'invalid_option').map((e) => e.option);
    expect(new Set(reported)).toEqual(new Set(['maxBufferSize', 'batchSize', 'batchWaitMs', 'sampleRate', 'timeoutMs']));
    expect(reported).toHaveLength(5);
    expect(r.errors.every((e) => e.code === 'invalid_option')).toBe(true);
  });

  it.each([
    [1.5, 1],
    [-1, 0],
  ])('B25: sampleRate %s → clamped to %s with invalid_option sampleRate once', (given, expected) => {
    mockFetch();
    const r = errorRecorder();
    init({ endpoint: ENDPOINT, apiKey: 'k', sampleRate: given, onError: r.onError });
    expect(getClient()?.options.sampleRate).toBe(expected);
    expect(r.codes()).toEqual(['invalid_option:sampleRate']);
  });

  it('B25: a string batchSize, Infinity maxBufferSize and a negative batchWaitMs each fall back with one report; valid values stand silently', () => {
    mockFetch();
    const r = errorRecorder();
    init({ endpoint: ENDPOINT, apiKey: 'k', maxBufferSize: Number.POSITIVE_INFINITY, batchSize: '20' as unknown as number, batchWaitMs: -5, onError: r.onError });
    expect(getClient()?.options).toMatchObject({ maxBufferSize: 100, batchSize: 10, batchWaitMs: 1000 });
    expect(new Set(r.codes())).toEqual(new Set(['invalid_option:maxBufferSize', 'invalid_option:batchSize', 'invalid_option:batchWaitMs']));
    expect(r.codes()).toHaveLength(3);
    resetPipeline();
    mockFetch();
    const ok = errorRecorder();
    init({ endpoint: ENDPOINT, apiKey: 'k', maxBufferSize: 7, batchSize: 3, batchWaitMs: 250, sampleRate: 0.5, onError: ok.onError });
    expect(getClient()?.options).toMatchObject({ maxBufferSize: 7, batchSize: 3, batchWaitMs: 250, sampleRate: 0.5 });
    expect(ok.errors).toEqual([]);
  });
});

describe('N15: endpoint must parse as an absolute http(s) URL', () => {
  it.each(['localhost:8080/ingest', '/ingest', 'ftp://x/ingest'])('N15: init({ endpoint: %j }) → the no-op client, invalid_option endpoint once, captures return ids, nothing posted', async (endpoint) => {
    const f = mockFetch();
    const r = errorRecorder();
    expect(() => init({ endpoint, apiKey: 'k', onError: r.onError })).not.toThrow();
    expect(getClient()).toMatchObject({ runtime: 'node' });
    expect(r.codes()).toEqual(['invalid_option:endpoint']);
    expect(captureException(new Error('dropped'))).toMatch(UUID);
    expect(captureMessage('dropped too')).toMatch(UUID);
    await expect(flush(100)).resolves.toBe(true);
    expect(f.posts).toHaveLength(0);
    expect(r.codes()).toEqual(['invalid_option:endpoint']);
  });

  it('N15: init({ endpoint: "http://localhost:8080/ingest/" }) is accepted and posts to …/ingest/batch with X-API-Key and X-Project-Key', async () => {
    const f = mockFetch();
    const r = errorRecorder();
    init({ endpoint: 'http://localhost:8080/ingest/', apiKey: 'key-1', projectKey: 'proj-1', onError: r.onError });
    const id = captureMessage('sent');
    await expect(flush(2000)).resolves.toBe(true);
    expect(f.posts).toHaveLength(1);
    expect(f.posts[0]?.url).toBe('http://localhost:8080/ingest/batch');
    expect(f.posts[0]?.method).toBe('POST');
    expect(f.posts[0]?.headers).toMatchObject({ 'x-api-key': 'key-1', 'x-project-key': 'proj-1', 'content-type': 'application/json' });
    expect(f.posts[0]?.events.map((e) => e.event_id)).toEqual([id]);
    expect(r.errors).toEqual([]);
    // Decision 36: the apiKey is not on the opaque client.
    expect('apiKey' in (getClient()?.options ?? {})).toBe(false);
    expect(getClient()?.options.endpoint).toBe('http://localhost:8080/ingest/');
  });
});

describe('N11: environment normalisation through init', () => {
  // Contract §4.2's rule in its stated order — no trim step, so a trailing space becomes `-` (recorded in
  // status/be.md as TASK-02's reported deviation from the flow row's `production`).
  it.each([
    ['Production ', 'production-'],
    ['pre prod', 'pre-prod'],
    ['_Staging', 'staging'],
    ['', 'production'],
    ['---', 'production'],
  ])('N11: environment %j → %j on the client and on the event, invalid_option environment once, debug shows before/after', async (given, expected) => {
    const f = mockFetch();
    const r = errorRecorder();
    const lines: string[] = [];
    vi.spyOn(process.stderr, 'write').mockImplementation(((chunk: unknown) => {
      lines.push(String(chunk));
      return true;
    }) as typeof process.stderr.write);
    init({ endpoint: ENDPOINT, apiKey: 'k', environment: given, debug: true, onError: r.onError });
    expect(getClient()?.options.environment).toBe(expected);
    expect(r.codes()).toEqual(['invalid_option:environment']);
    expect(lines.some((l) => l.includes(JSON.stringify(given)) && l.includes(JSON.stringify(expected)))).toBe(true);
    captureMessage('env');
    await flush(2000);
    expect(f.posts[0]?.events[0]?.environment).toBe(expected);
  });
});

describe('N4: init() with no env vars (next dev)', () => {
  it('N4: the no-op client — onError not_configured once, captures return ids and vanish, nothing posted, no console noise without debug', async () => {
    const f = mockFetch();
    const r = errorRecorder();
    const consoleSpies = ['debug', 'log', 'warn', 'error', 'info'].map((m) => vi.spyOn(console, m as 'log').mockImplementation(() => undefined));
    const stderr = vi.spyOn(process.stderr, 'write').mockImplementation((() => true) as typeof process.stderr.write);
    expect(() => init({ onError: r.onError })).not.toThrow();
    expect(getClient()).toMatchObject({ runtime: 'node' });
    expect(r.codes()).toEqual(['not_configured']);
    expect(r.errors[0]?.message).toMatch(/SENTINEL_ENDPOINT/);
    const a = captureException(new Error('vanishes'));
    const b = captureMessage('vanishes too');
    expect(a).toMatch(UUID);
    expect(b).toMatch(UUID);
    expect(a).not.toBe(b);
    await expect(flush(100)).resolves.toBe(true);
    expect(f.posts).toHaveLength(0);
    expect(r.codes()).toEqual(['not_configured']);
    for (const s of consoleSpies) expect(s).not.toHaveBeenCalled();
    expect(stderr).not.toHaveBeenCalled();
  });

  it('N4: with debug on, the no-op is explained on stderr; an apiKey without an endpoint is still not_configured; the env vars are the defaults', () => {
    mockFetch();
    const lines: string[] = [];
    vi.spyOn(process.stderr, 'write').mockImplementation(((chunk: unknown) => {
      lines.push(String(chunk));
      return true;
    }) as typeof process.stderr.write);
    const r = errorRecorder();
    init({ apiKey: 'k', debug: true, onError: r.onError });
    expect(r.codes()).toEqual(['not_configured']);
    expect(lines.some((l) => l.includes('not_configured') && l.includes('no-op'))).toBe(true);
    captureMessage('dropped');
    expect(lines.some((l) => l.includes('no-op client') && l.includes('dropped'))).toBe(true);

    resetPipeline();
    mockFetch();
    process.env.SENTINEL_ENDPOINT = 'https://api-sentinel.example/ingest';
    process.env.SENTINEL_API_KEY = 'env-key';
    process.env.SENTINEL_PROJECT_KEY = 'env-proj';
    const ok = errorRecorder();
    init({ onError: ok.onError });
    expect(ok.errors).toEqual([]);
    expect(getClient()?.options).toMatchObject({ endpoint: 'https://api-sentinel.example/ingest', projectKey: 'env-proj' });
    expect('apiKey' in (getClient()?.options ?? {})).toBe(false);
  });
});
