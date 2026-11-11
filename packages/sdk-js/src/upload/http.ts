// One POST through `node:http` / `node:https` (contract §4.8 step 3): `Content-Length` declared up front from
// the packer's arithmetic, the body streamed, a per-request deadline, and NO `Origin` header — the route is
// CSRF-exempt for exactly this. The response is read as `{ status, body }` with the body capped; the caller
// decides the class by status alone (decision 34). A network failure — refused, reset, DNS, the deadline —
// rejects with the error, whose `code` is what gets printed.
//
// `SendFn` is the seam: the uploader takes one so a test can read the declared `Content-Length` of a synthetic
// 120 MiB request without a byte of body crossing a socket (C13), the way the transport takes `fetch`.

import type { ClientRequest, IncomingMessage } from 'node:http';

export interface UploadRequest {
  readonly url: string;
  /** `content-type` (with the boundary), `x-api-key`, optional `x-project-key` — never logged. */
  readonly headers: Readonly<Record<string, string>>;
  readonly contentLength: number;
  /** A fresh body stream per call — a retry re-reads the files. */
  readonly body: () => AsyncIterable<Uint8Array>;
}

export interface UploadResponse {
  readonly status: number;
  readonly body: string;
}

export type SendFn = (req: UploadRequest, timeoutMs: number) => Promise<UploadResponse>;

/** The 60 s per-request timeout of contract §4.8 step 3. */
export const DEFAULT_TIMEOUT_MS = 60_000;
/** How much of a response is read: a success body for 50 files is ≈ 8 KiB; an envelope is smaller; a proxy's error page is not ours. */
export const MAX_RESPONSE_BYTES = 1024 * 1024;

function readBody(res: IncomingMessage): Promise<string> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    res.on('data', (chunk: Buffer) => {
      if (total >= MAX_RESPONSE_BYTES) return;
      const room = MAX_RESPONSE_BYTES - total;
      const piece = chunk.byteLength > room ? chunk.subarray(0, room) : chunk;
      chunks.push(piece);
      total += piece.byteLength;
    });
    res.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    res.on('error', reject);
    res.on('aborted', () => reject(Object.assign(new Error('response aborted'), { code: 'ECONNRESET' })));
  });
}

/** Back-pressure, or the socket going away under us — whichever comes first ends the wait; listeners are removed either way. */
function drainOrGone(client: ClientRequest): Promise<void> {
  return new Promise((resolve) => {
    const done = (): void => {
      client.off('drain', done);
      client.off('close', done);
      client.off('error', done);
      resolve();
    };
    client.once('drain', done);
    client.once('close', done);
    client.once('error', done);
  });
}

/**
 * The default `SendFn`: `node:http(s).request` with the declared length and the deadline. The two modules are
 * imported here, per send, not at the top: `node:http`'s own initialisation reads `process.env` (undici's
 * `UNDICI_NO_FG`, `NODE_V8_COVERAGE`, …), and `dist/nextjs/config.js` bundles this file — its import must read
 * nothing (contract §11 *Import-time inertness*; D7).
 */
export const sendWithNodeHttp: SendFn = async (req, timeoutMs) => {
  const url = new URL(req.url);
  const { request: make } = url.protocol === 'https:' ? await import('node:https') : await import('node:http');
  return new Promise<UploadResponse>((resolve, reject) => {
    let settled = false;
    const settle = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      fn();
    };
    const client = make(url, { method: 'POST', headers: { ...req.headers, 'content-length': String(req.contentLength) } });
    const timer = setTimeout(() => {
      client.destroy(Object.assign(new Error(`request timed out after ${timeoutMs} ms`), { code: 'ETIMEDOUT' }));
    }, timeoutMs);
    timer.unref();
    client.on('response', (res) => {
      readBody(res).then(
        (body) => settle(() => resolve({ status: res.statusCode ?? 0, body })),
        (err: unknown) => settle(() => reject(err)),
      );
    });
    client.on('error', (err) => settle(() => reject(err)));
    void (async () => {
      try {
        for await (const chunk of req.body()) {
          if (client.destroyed) return;
          if (!client.write(chunk)) await drainOrGone(client);
        }
        client.end();
      } catch (err) {
        // A file that vanished mid-stream, or the server hanging up while we write (EPIPE after an early 413):
        // the 'response' / 'error' listeners above decide the outcome; this only stops the writer.
        client.destroy(err instanceof Error ? err : new Error(String(err)));
      }
    })();
  });
};
