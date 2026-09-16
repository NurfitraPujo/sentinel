// The local `node:http` stub of contract §11 (*The transport against a local HTTP stub*): it records every
// request — method, URL, headers, the parsed body, whether the client aborted before the response — and answers
// per a responder the test sets (a status + body + headers, or `hold` to never answer, so a per-attempt timeout
// or `close`'s abort can be observed). Waits use REAL timers so a suite under vitest's fake timers still sees I/O.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import type { AddressInfo } from 'node:net';
import { setTimeout as realDelay } from 'node:timers/promises';
import type { SentinelEvent } from '../../src/core/types.js';

export interface StubResponse {
  status: number;
  body?: string;
  headers?: Record<string, string>;
}

export type Responder = (req: StubRequest) => StubResponse | 'hold';

export interface StubRequest {
  readonly n: number;
  readonly method: string;
  readonly url: string;
  readonly headers: IncomingMessage['headers'];
  readonly body: string;
  /** The body parsed as the batch array (`[]` when it is not one). */
  readonly events: SentinelEvent[];
  /** `event_id`s in body order. */
  readonly ids: string[];
  /** The client went away before a response was written — the per-attempt timeout or `close`'s abort. */
  aborted: boolean;
  /** Answered by the responder, or later by `answer` for a held request. */
  answered: boolean;
  /** Answer a held request now. */
  answer(response: StubResponse): void;
}

export interface StubServer {
  readonly url: string;
  readonly requests: StubRequest[];
  /** Replace the responder; the default answers `202 {"ingested":n,"failed":0}`. */
  respond(responder: Responder): void;
  /** Resolves once `count` requests have been RECEIVED (bodies read), or rejects after `timeoutMs` real ms. */
  waitFor(count: number, timeoutMs?: number): Promise<StubRequest[]>;
  close(): Promise<void>;
}

export const ACCEPTED: Responder = (req) => ({ status: 202, body: JSON.stringify({ ingested: req.events.length, failed: 0 }) });

/** `port` re-listens on a known port — the connection-refused-then-back row (B14 (c)). */
export async function startStub(responder: Responder = ACCEPTED, port = 0): Promise<StubServer> {
  const requests: StubRequest[] = [];
  let current = responder;
  const server: Server = createServer((req: IncomingMessage, res: ServerResponse) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      const body = Buffer.concat(chunks).toString('utf8');
      let events: SentinelEvent[] = [];
      try {
        const parsed: unknown = JSON.parse(body);
        if (Array.isArray(parsed)) events = parsed as SentinelEvent[];
      } catch {
        // not JSON — recorded as text
      }
      const entry: StubRequest = {
        n: requests.length + 1,
        method: req.method ?? '',
        url: req.url ?? '',
        headers: req.headers,
        body,
        events,
        ids: events.map((e) => e.event_id),
        aborted: false,
        answered: false,
        answer(response: StubResponse) {
          if (entry.answered || res.writableEnded) return;
          entry.answered = true;
          res.writeHead(response.status, { 'content-type': 'application/json', ...(response.headers ?? {}) });
          res.end(response.body ?? '');
        },
      };
      res.on('close', () => {
        if (!res.writableFinished) entry.aborted = true;
      });
      requests.push(entry);
      const answer = current(entry);
      if (answer !== 'hold') entry.answer(answer);
    });
  });
  server.keepAliveTimeout = 100;
  await new Promise<void>((resolve) => server.listen(port, '127.0.0.1', resolve));
  const { port: bound } = server.address() as AddressInfo;
  return {
    url: `http://127.0.0.1:${bound}`,
    requests,
    respond(next: Responder) {
      current = next;
    },
    async waitFor(count: number, timeoutMs = 5000): Promise<StubRequest[]> {
      const started = performance.now();
      while (requests.length < count) {
        if (performance.now() - started > timeoutMs) throw new Error(`stub: ${requests.length} request(s) received, ${count} expected within ${timeoutMs} ms`);
        await realDelay(5);
      }
      return requests.slice(0, count);
    },
    close() {
      return new Promise<void>((resolve) => {
        server.closeAllConnections();
        server.close(() => resolve());
      });
    },
  };
}

/** A `http://127.0.0.1:<port>` nobody listens on — the connection-refused row. */
export async function refusedUrl(): Promise<string> {
  const probe = createServer();
  await new Promise<void>((resolve) => probe.listen(0, '127.0.0.1', resolve));
  const { port } = probe.address() as AddressInfo;
  await new Promise<void>((resolve) => probe.close(() => resolve()));
  return `http://127.0.0.1:${port}`;
}

/** Poll `predicate` with REAL timers until it holds, or throw after `timeoutMs`. */
export async function until(predicate: () => boolean, what = 'condition', timeoutMs = 5000): Promise<void> {
  const started = performance.now();
  while (!predicate()) {
    if (performance.now() - started > timeoutMs) throw new Error(`timed out waiting for ${what}`);
    await realDelay(5);
  }
}

/** Let real I/O callbacks run: a few real-timer turns. */
export async function settle(ms = 20): Promise<void> {
  await realDelay(ms);
}
