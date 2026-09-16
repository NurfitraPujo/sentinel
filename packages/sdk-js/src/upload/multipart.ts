// The request body of contract §4.8 step 3 — `multipart/form-data` with one `release_version` text part, then
// positional `minified_path` / `file` pairs — and the packer that decides where one request ends: a batch holds
// ≤ `maxFiles` (50, the route's per-request cap) AND ≤ `maxBytes` (100 × 1024 × 1024 = 104 857 600 bytes)
// SERIALISED, multipart overhead included; a map that would push the batch over starts the next one, and a map
// larger than the budget on its own goes alone in its own request (the route answers 413 for one over
// `SOURCEMAP_MAX_BYTES`, decision 24). The budget sits under the dashboard host's 110M `BODY_SIZE_LIMIT`
// (adapter-node's plain-text 413 — C13).
//
// The body is never materialised: `contentLength` is computed from the part headers and `stat().size`, and
// `batchBody` streams the files. The boundary has a fixed length, so every size below is exact.

import { randomBytes } from 'node:crypto';
import { createReadStream } from 'node:fs';
import { basename } from 'node:path';
import type { MapPair } from './walk.js';

export const MAX_FILES_PER_REQUEST = 50;
/** 100 MiB, 1024-based — the same unit as every other KiB/MiB figure in the contract (§4.8 step 3). */
export const MAX_REQUEST_BYTES = 100 * 1024 * 1024;

const CRLF = '\r\n';

export interface PackedBatch {
  readonly pairs: readonly MapPair[];
  /** The exact serialised size — what `Content-Length` declares. */
  readonly contentLength: number;
}

export interface PackOptions {
  readonly boundary: string;
  readonly release: string;
  readonly maxFiles: number;
  readonly maxBytes: number;
}

/** `----SentinelSourcemaps<32 hex>` — fixed length, so the size arithmetic is stable across requests. */
export function newBoundary(): string {
  return `----SentinelSourcemaps${randomBytes(16).toString('hex')}`;
}

function bytes(text: string): number {
  return Buffer.byteLength(text, 'utf8');
}

/** RFC 7578 §4.2: `"` and CR/LF cannot appear raw in a quoted filename. Next never emits one, but a walk is not Next's. */
function quoteFilename(name: string): string {
  return name.replace(/["\r\n]/g, (c) => encodeURIComponent(c));
}

function textPart(boundary: string, name: string, value: string): string {
  return `--${boundary}${CRLF}Content-Disposition: form-data; name="${name}"${CRLF}${CRLF}${value}${CRLF}`;
}

function fileHead(boundary: string, pair: MapPair): string {
  return `--${boundary}${CRLF}Content-Disposition: form-data; name="file"; filename="${quoteFilename(basename(pair.path))}"${CRLF}Content-Type: application/json${CRLF}${CRLF}`;
}

function closing(boundary: string): string {
  return `--${boundary}--${CRLF}`;
}

/** The `release_version` part — once per request, first. */
export function preambleBytes(boundary: string, release: string): number {
  return bytes(textPart(boundary, 'release_version', release));
}

/** One `minified_path` text part plus the `file` part around the map's bytes. */
export function filePartBytes(boundary: string, pair: MapPair): number {
  return bytes(textPart(boundary, 'minified_path', pair.minifiedPath)) + bytes(fileHead(boundary, pair)) + pair.sizeBytes + bytes(CRLF);
}

export function closingBytes(boundary: string): number {
  return bytes(closing(boundary));
}

/**
 * Greedy, in walk order: a pair joins the current batch unless it would be the (maxFiles + 1)th file or push the
 * serialised size over `maxBytes`; then it starts the next batch — alone if it is over the budget by itself.
 */
export function packBatches(pairs: readonly MapPair[], opts: PackOptions): PackedBatch[] {
  const fixed = preambleBytes(opts.boundary, opts.release) + closingBytes(opts.boundary);
  const batches: PackedBatch[] = [];
  let current: MapPair[] = [];
  let currentBytes = fixed;
  const flush = (): void => {
    if (current.length > 0) batches.push({ pairs: current, contentLength: currentBytes });
    current = [];
    currentBytes = fixed;
  };
  for (const pair of pairs) {
    const part = filePartBytes(opts.boundary, pair);
    if (current.length > 0 && (current.length >= opts.maxFiles || currentBytes + part > opts.maxBytes)) flush();
    current.push(pair);
    currentBytes += part;
  }
  flush();
  return batches;
}

/** Streams the body `contentLength` describes, chunk by chunk — the files through `open`, `createReadStream` by default. */
export async function* batchBody(
  batch: PackedBatch,
  opts: Pick<PackOptions, 'boundary' | 'release'>,
  open: (path: string) => AsyncIterable<Uint8Array> = (path) => createReadStream(path),
): AsyncGenerator<Uint8Array> {
  yield Buffer.from(textPart(opts.boundary, 'release_version', opts.release), 'utf8');
  for (const pair of batch.pairs) {
    yield Buffer.from(textPart(opts.boundary, 'minified_path', pair.minifiedPath) + fileHead(opts.boundary, pair), 'utf8');
    yield* open(pair.path);
    yield Buffer.from(CRLF, 'utf8');
  }
  yield Buffer.from(closing(opts.boundary), 'utf8');
}
