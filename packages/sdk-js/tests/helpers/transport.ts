// Test plumbing for the transport rows: a client installed through `initCore` against the stub's URL with
// `debug: true`, an `onError` recorder and a spy on `globalThis.fetch` that records what the stub cannot see
// from the wire — `keepalive`, `credentials` and the abort signal of every call — then forwards to the real one. The slot-clearing `beforeEach` is
// helpers/pipeline.ts's `resetPipeline`.

import { vi } from 'vitest';
import { settle } from './stub-server.js';
import { initCore } from '../../src/core/init.js';
import { getInternalClient } from '../../src/core/state.js';
import type { Transport } from '../../src/core/transport/transport.js';
import type { InitOptions, Runtime, SentinelSdkError } from '../../src/core/types.js';

export interface FetchCall {
  readonly n: number;
  readonly url: string;
  readonly keepalive: boolean | undefined;
  readonly credentials: RequestCredentials | undefined;
  readonly bodyBytes: number;
  readonly ids: string[];
  aborted: boolean;
  settled: boolean;
  outcome: 'response' | 'error' | undefined;
  status: number | undefined;
}

export interface Harness {
  transport: Transport;
  errors: SentinelSdkError[];
  debugLines: string[];
  /** Every `fetch` the transport issued, in order, as the wrapper saw it. */
  fetches: FetchCall[];
  /** Fetches whose `signal` fired. */
  aborted(): FetchCall[];
  /** The most fetches ever unsettled at once — 1 under the sequential rule. */
  maxInFlight(): number;
}

/** The real `fetch`, observed: the transport looks `globalThis.fetch` up per send, so a spy on it sees every call. */
function recordingFetch(real: typeof fetch, calls: FetchCall[], peak: { max: number }): typeof fetch {
  return async (input, init) => {
    const body = typeof init?.body === 'string' ? init.body : '';
    let ids: string[] = [];
    try {
      const parsed: unknown = JSON.parse(body);
      if (Array.isArray(parsed)) ids = parsed.map((e: { event_id?: string }) => e.event_id ?? '');
    } catch {
      // not a batch body
    }
    const call: FetchCall = {
      n: calls.length + 1,
      url: String(input),
      keepalive: init?.keepalive,
      credentials: init?.credentials,
      bodyBytes: Buffer.byteLength(body),
      ids,
      aborted: false,
      settled: false,
      outcome: undefined,
      status: undefined,
    };
    init?.signal?.addEventListener('abort', () => {
      call.aborted = true;
    });
    calls.push(call);
    peak.max = Math.max(peak.max, calls.filter((c) => !c.settled).length);
    try {
      const res = await real(input, init);
      call.outcome = 'response';
      call.status = res.status;
      return res;
    } catch (e) {
      call.outcome = 'error';
      throw e;
    } finally {
      call.settled = true;
    }
  };
}

/**
 * Install a client for `runtime` posting to `url` (`endpoint` on node / edge, `tunnel` on the browser) with
 * `options`, `debug: true`, an `onError` recorder and the recording `fetch`. Throws when no transport resolved.
 */
export function setupTransport(url: string, options: InitOptions = {}, runtime: Runtime = 'node'): Harness {
  const errors: SentinelSdkError[] = [];
  const debugLines: string[] = [];
  const fetches: FetchCall[] = [];
  vi.spyOn(console, 'debug').mockImplementation((line: unknown) => {
    debugLines.push(String(line));
  });
  vi.spyOn(process.stderr, 'write').mockImplementation(((chunk: unknown) => {
    debugLines.push(String(chunk).trimEnd());
    return true;
  }) as typeof process.stderr.write);
  const real = globalThis.fetch;
  const peak = { max: 0 };
  vi.spyOn(globalThis, 'fetch').mockImplementation(recordingFetch(real, fetches, peak));
  const onError = options.onError;
  const target: InitOptions = runtime === 'browser' ? { tunnel: url } : { endpoint: url, apiKey: options.apiKey ?? 'test-key' };
  initCore(
    {
      ...target,
      ...options,
      debug: true,
      onError: (err) => {
        errors.push(err);
        onError?.(err);
      },
    },
    runtime,
  );
  const transport = getInternalClient()?.transport;
  if (transport === undefined) throw new Error('setupTransport: no transport resolved for ' + url);
  return {
    transport,
    errors,
    debugLines,
    fetches,
    aborted: () => fetches.filter((f) => f.aborted),
    maxInFlight: () => peak.max,
  };
}

/** Fake `setTimeout` / `Date` only: I/O, microtasks and `node:timers/promises` stay real (the stub's waits). */
export function useFakeClock(): void {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
}

/**
 * Advance the fake clock by `ms` in `step`s, letting real I/O run between steps — a retry that fires inside
 * the window gets its connection refused / its response read before the next step.
 */
export async function advance(ms: number, step = Math.max(100, Math.ceil(ms / 30))): Promise<void> {
  if (ms === 0) {
    await vi.advanceTimersByTimeAsync(0);
    await settle(3);
    return;
  }
  let left = ms;
  while (left > 0) {
    const chunk = Math.min(step, left);
    await vi.advanceTimersByTimeAsync(chunk);
    await settle(3);
    left -= chunk;
  }
}

/** The delays the ladder announced, in ms, read from the debug lines. */
export function announcedDelays(debugLines: string[]): number[] {
  return debugLines
    .map((l) => /retrying \d+ event\(s\) in (\d+) ms/.exec(l))
    .filter((m): m is RegExpExecArray => m !== null)
    .map((m) => Number(m[1]));
}

/** The delay the ladder announced last, in ms — what to advance the fake clock by for the next attempt. */
export function lastAnnouncedDelay(debugLines: string[]): number {
  const delays = announcedDelays(debugLines);
  if (delays.length === 0) throw new Error('no delay announced yet');
  return delays[delays.length - 1] as number;
}

/** After a test: whatever the transport is still doing must stop before the next test's fake clock. */
export function stopTransport(): void {
  getInternalClient()?.transport?.close();
}
