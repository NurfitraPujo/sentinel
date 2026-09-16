// Plumbing for the tunnel-route suite (contract §4.5, flows T1–T17): a same-origin `Request` builder with the
// compose stack's headers (`Origin: http://localhost:3001` against `Host: localhost:3001`), a recording upstream
// `fetch` stub on `globalThis` (the route looks `globalThis.fetch` up per call), and a fresh module loader for
// the once-per-process rules (the numeric-option warns, the not-configured warn).

import { vi } from 'vitest';

export const KEY = 'sk_live_9f8e7d6c5b4a3210_supersecret';
export const PROJECT_KEY = 'proj_public_key_42';
export const ENDPOINT = 'http://ingest.internal:8080/ingest';
export const URL_ = 'http://localhost:3001/api/sentinel';

export const ITEM = {
  event_id: '11111111-1111-4111-8111-111111111111',
  platform: 'javascript',
  environment: 'production',
  error_class: 'TypeError',
  message: 'x is not a function',
  stacktrace: [],
  metadata: { runtime: 'browser' },
  timestamp: '2026-09-16T00:00:00.000Z',
};

export interface RequestSpec {
  method?: string;
  headers?: Record<string, string | null>;
  body?: string | ReadableStream<Uint8Array> | null;
  url?: string;
}

/** A same-origin JSON POST unless a header is overridden (`null` removes one of the defaults). */
export function request(spec: RequestSpec = {}): Request {
  const headers: Record<string, string> = {
    host: 'localhost:3001',
    origin: 'http://localhost:3001',
    'sec-fetch-site': 'same-origin',
    'content-type': 'application/json',
  };
  for (const [k, v] of Object.entries(spec.headers ?? {})) {
    if (v === null) delete headers[k.toLowerCase()];
    else headers[k.toLowerCase()] = v;
  }
  const method = spec.method ?? 'POST';
  const body = spec.body === undefined ? JSON.stringify([ITEM]) : spec.body;
  const init: RequestInit & { duplex?: 'half' } = { method, headers };
  if (method !== 'GET' && method !== 'HEAD' && body !== null) {
    init.body = body;
    if (typeof body !== 'string') init.duplex = 'half';
  }
  return new Request(spec.url ?? URL_, init);
}

export interface UpstreamCall {
  url: string;
  method: string;
  headers: Record<string, string>;
  body: string;
  items: unknown;
  signal: AbortSignal | null | undefined;
}

export interface Upstream {
  calls: UpstreamCall[];
  /** Answer every call from now on with this status / body / headers (default `202` and the ingestor's batch JSON). */
  answer(status: number, body?: string, headers?: Record<string, string>): void;
  /** Reject every call from now on (a network error). */
  fail(): void;
  /** Never answer; the call's `signal` abort rejects it (a timeout). */
  hang(): void;
}

function headersOf(init: RequestInit | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  const h = init?.headers;
  if (h === undefined) return out;
  if (h instanceof Headers) h.forEach((v, k) => (out[k.toLowerCase()] = v));
  else if (Array.isArray(h)) for (const [k, v] of h) out[String(k).toLowerCase()] = String(v);
  else for (const [k, v] of Object.entries(h)) out[k.toLowerCase()] = String(v);
  return out;
}

/** Install the recording upstream on `globalThis.fetch` (restored by `vi.restoreAllMocks()`). */
export function stubUpstream(): Upstream {
  const calls: UpstreamCall[] = [];
  type Mode = { kind: 'answer'; status: number; body: string | undefined; headers: Record<string, string> } | { kind: 'fail' } | { kind: 'hang' };
  let mode: Mode = { kind: 'answer', status: 202, body: undefined, headers: { 'content-type': 'application/json' } };
  vi.spyOn(globalThis, 'fetch').mockImplementation(((input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const body = typeof init?.body === 'string' ? init.body : '';
    let items: unknown;
    try {
      items = JSON.parse(body);
    } catch {
      items = undefined;
    }
    calls.push({ url: String(input), method: init?.method ?? 'GET', headers: headersOf(init), body, items, signal: init?.signal });
    return new Promise<Response>((resolve, reject) => {
      init?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')));
      if (mode.kind === 'hang') return;
      if (mode.kind === 'fail') {
        reject(new TypeError('fetch failed'));
        return;
      }
      const count = Array.isArray(items) ? items.length : 0;
      const text = mode.body ?? JSON.stringify({ ingested: count, failed: 0, errors: [], event_ids: [] });
      resolve(new Response(text, { status: mode.status, headers: mode.headers }));
    });
  }) as typeof fetch);
  return {
    calls,
    answer(status, body, headers = { 'content-type': 'application/json' }) {
      mode = { kind: 'answer', status, body, headers };
    },
    fail() {
      mode = { kind: 'fail' };
    },
    hang() {
      mode = { kind: 'hang' };
    },
  };
}

export interface Envelope {
  error: { code: string; message: string; details: Record<string, unknown> };
}

export async function envelope(res: Response): Promise<Envelope> {
  return (await res.json()) as Envelope;
}

/** A fresh instance of the route module — the once-per-process flags start clean. */
export async function freshRoute(): Promise<typeof import('../../src/nextjs/route.js')> {
  vi.resetModules();
  return import('../../src/nextjs/route.js');
}
