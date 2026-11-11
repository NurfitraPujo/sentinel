// A recording `fetch` for the init / handler suites: every call's URL, headers, parsed batch and abort signal
// recorded, answered `202 { ingested, failed: 0 }` by default, or held forever (`hold`) so a `flush` / `close`
// timeout can be observed. The transport looks `globalThis.fetch` up per send, so a spy installed before `init`
// sees every POST — and a no-op client's silence is "the spy was never called".

import { vi } from 'vitest';
import type { SentinelEvent } from '../../src/core/types.js';

export interface RecordedPost {
  readonly n: number;
  readonly url: string;
  readonly method: string;
  readonly headers: Record<string, string>;
  readonly body: string;
  readonly events: SentinelEvent[];
  aborted: boolean;
}

export interface FetchMock {
  readonly posts: RecordedPost[];
  /** Hold every call from now on (never answers). */
  hold(): void;
  /** Answer every call from now on with `status` and `body`. */
  answer(status: number, body?: string): void;
}

function headersOf(init: RequestInit | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  const h = init?.headers;
  if (h === undefined) return out;
  if (h instanceof Headers) {
    h.forEach((v, k) => {
      out[k.toLowerCase()] = v;
    });
  } else if (Array.isArray(h)) {
    for (const [k, v] of h) out[String(k).toLowerCase()] = String(v);
  } else {
    for (const [k, v] of Object.entries(h)) out[k.toLowerCase()] = String(v);
  }
  return out;
}

/** Install the recording `fetch` on `globalThis` (restored by `vi.restoreAllMocks()` in the suite's reset). */
export function mockFetch(): FetchMock {
  const posts: RecordedPost[] = [];
  let mode: { kind: 'answer'; status: number; body: string } | { kind: 'hold' } = { kind: 'answer', status: 202, body: '' };
  const impl = (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const body = typeof init?.body === 'string' ? init.body : '';
    let events: SentinelEvent[] = [];
    try {
      const parsed: unknown = JSON.parse(body);
      if (Array.isArray(parsed)) events = parsed as SentinelEvent[];
    } catch {
      // not a batch body
    }
    const post: RecordedPost = { n: posts.length + 1, url: String(input), method: init?.method ?? 'GET', headers: headersOf(init), body, events, aborted: false };
    posts.push(post);
    return new Promise<Response>((resolve, reject) => {
      init?.signal?.addEventListener('abort', () => {
        post.aborted = true;
        reject(new DOMException('aborted', 'AbortError'));
      });
      if (mode.kind === 'hold') return;
      const text = mode.body === '' ? JSON.stringify({ ingested: events.length, failed: 0 }) : mode.body;
      resolve(new Response(text, { status: mode.status, headers: { 'content-type': 'application/json' } }));
    });
  };
  vi.spyOn(globalThis, 'fetch').mockImplementation(impl as typeof fetch);
  return {
    posts,
    hold() {
      mode = { kind: 'hold' };
    },
    answer(status: number, body = '') {
      mode = { kind: 'answer', status, body };
    },
  };
}

/** The `onError` recorder every init suite passes, with the `[code, option]` pairs B25 asserts as a set. */
export function errorRecorder(): { errors: import('../../src/core/types.js').SentinelSdkError[]; onError: (e: import('../../src/core/types.js').SentinelSdkError) => void; codes(): string[] } {
  const errors: import('../../src/core/types.js').SentinelSdkError[] = [];
  return {
    errors,
    onError: (e) => {
      errors.push(e);
    },
    codes: () => errors.map((e) => (e.option === undefined ? e.code : `${e.code}:${e.option}`)),
  };
}
