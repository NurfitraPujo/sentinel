// `uploadSourcemaps(opts): Promise<UploadReport>` — the one exported uploader of contract §4.8, which the CLI
// (src/cli) and `withSentinelConfig` (src/nextjs/config.ts) both call IN-PROCESS; nothing shells out. The walk
// and the pairing (walk.ts), the packing to ≤ 50 files and ≤ 104 857 600 serialised bytes per request
// (multipart.ts), the POST with no `Origin` and a 60 s deadline (http.ts), and here: the release refusals, the
// `batchSize` clamp, the retry ladder and the failure classes (errors.ts).
//
// Failure policy, decision 34: the status class decides — 2xx is success; ANY 4xx throws `rejected` at once
// with the envelope printed, the later batches never sent, nothing of that batch deleted; every 5xx and every
// network failure, whatever the body, is ONE class, retried up to three more times (4 attempts; 2 s / 4 s / 8 s
// guidance, each ≤ 60 s) and then thrown as `unavailable`. The envelope `code` is printed, never branched on.
//
// `deleteAfterUpload` here is the CLI's `--delete` (step 6): a batch's own `.js.map` / `.mjs.map` files are
// unlinked only after THAT batch's 2xx and nothing else is touched — a map two chunks reference (walk.ts) is
// unlinked once, after the last batch that carried it, so the later batch can still stream it; the wrapper's
// every-`*.map` sweep is its own (§4.9 step 4, `listMaps`). The key is never part of any message or event.

import { unlink } from 'node:fs/promises';
import { describeResponse, SourcemapUploadError } from './errors.js';
import { DEFAULT_TIMEOUT_MS, type SendFn, sendWithNodeHttp, type UploadRequest, type UploadResponse } from './http.js';
import { batchBody, MAX_FILES_PER_REQUEST, MAX_REQUEST_BYTES, newBoundary, packBatches, type PackedBatch } from './multipart.js';
import { type MapPair, walkSourcemaps } from './walk.js';

export { describeResponse, isSourcemapUploadError, MAX_PRINTED_BODY_BYTES, SourcemapUploadError } from './errors.js';
export type { DescribedResponse, UploadFailureKind } from './errors.js';
export { DEFAULT_TIMEOUT_MS, sendWithNodeHttp } from './http.js';
export type { SendFn, UploadRequest, UploadResponse } from './http.js';
export { MAX_FILES_PER_REQUEST, MAX_REQUEST_BYTES, newBoundary, packBatches } from './multipart.js';
export type { PackedBatch, PackOptions } from './multipart.js';
export { INDEX_MAP_CHECK_MAX_BYTES, indexMapKind, isScript, isSourcemapOfScript, listMaps, listTraces, pairMinifiedPath, parseSourceMappingUrl, SOURCEMAPPING_URL_TAIL_BYTES, walkSourcemaps } from './walk.js';
export type { IndexMapKind, MapPair, WalkOptions } from './walk.js';

/** Total sends per batch — the first plus at most three retries (contract §4.8 step 5). */
export const MAX_ATTEMPTS = 4;
/** The ladder's guidance: 2 s, doubled per retry, never above 60 s. */
export const RETRY_BASE_MS = 2000;
export const RETRY_CAP_MS = 60_000;

export type SleepFn = (ms: number) => Promise<void>;

/** One stored file as the route reports it (`files[]` of `+server.ts`'s body), snake_case as on the wire. */
export interface StoredFile {
  readonly minified_path: string;
  readonly sha256: string;
  readonly size_bytes: number;
  readonly outcome: string;
}

export interface BatchResult {
  /** 0-based, in send order. */
  readonly index: number;
  readonly status: number;
  /** Sends it took, 1..4. */
  readonly attempts: number;
  readonly contentLength: number;
  readonly sent: readonly MapPair[];
  /** The route's `files[]`, in request order (`[]` when a 2xx body was not the expected JSON). */
  readonly files: readonly StoredFile[];
  /** The maps unlinked after this batch's 2xx (`deleteAfterUpload`). */
  readonly deleted: readonly string[];
}

export type UploadEvent =
  | { readonly type: 'warn'; readonly line: string }
  | { readonly type: 'retry'; readonly attempt: number; readonly delayMs: number; readonly status: number | undefined; readonly line: string }
  | { readonly type: 'batch'; readonly batch: BatchResult };

export interface UploadReport {
  readonly dryRun: boolean;
  readonly release: string;
  readonly pairs: readonly MapPair[];
  /** Requests packed — sent, or that would be sent under `dryRun`. */
  readonly requests: number;
  readonly batches: readonly BatchResult[];
  readonly files: readonly StoredFile[];
  readonly deleted: readonly string[];
}

export interface UploadSourcemapsOptions {
  buildDir: string;
  publicPrefix: string;
  /** `release_version`, byte-for-byte; empty or whitespace-padded → `usage` before any request (steps 2, 7). */
  release: string;
  /** `https://sentinel.daya.ai` — `/api/sourcemaps` is appended. */
  dashboardUrl: string;
  apiKey: string;
  projectKey?: string | undefined;
  /** Default 50; outside `[1, 50]` → clamped, one `warn`; non-finite → 50, one `warn` (the `init` posture). */
  batchSize?: number | undefined;
  /** The CLI's `--delete`: unlink each batch's maps after its 2xx. */
  deleteAfterUpload?: boolean | undefined;
  /** Walk, pair and pack; send nothing. */
  dryRun?: boolean | undefined;
  /** Per-request deadline; default 60 000. */
  timeoutMs?: number | undefined;
  onEvent?: ((event: UploadEvent) => void) | undefined;
  /** Injected for tests; `node:http(s)` otherwise. */
  send?: SendFn | undefined;
  /** Injected for tests; `setTimeout` otherwise. */
  sleep?: SleepFn | undefined;
}

const defaultSleep: SleepFn = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

/** The refusals of contract §4.8 steps 2 and 7 — never trimmed, never defaulted. */
export function validateRelease(release: unknown): string {
  if (typeof release !== 'string' || release.length === 0) {
    throw new SourcemapUploadError('usage', 'release is empty — a release-less upload can never symbolicate anything');
  }
  if (release !== release.trim()) {
    throw new SourcemapUploadError(
      'usage',
      `release ${JSON.stringify(release)} has leading/trailing whitespace — the $(…) newline is the usual cause; it is passed byte-for-byte, never trimmed`,
    );
  }
  return release;
}

function resolveDashboardUrl(dashboardUrl: unknown): string {
  if (typeof dashboardUrl !== 'string' || dashboardUrl.length === 0) throw new SourcemapUploadError('usage', 'dashboard url is missing');
  let parsed: URL;
  try {
    parsed = new URL(dashboardUrl);
  } catch {
    throw new SourcemapUploadError('usage', `dashboard url ${JSON.stringify(dashboardUrl)} is not an absolute http(s) URL`);
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
    throw new SourcemapUploadError('usage', `dashboard url ${JSON.stringify(dashboardUrl)} is not an absolute http(s) URL`);
  }
  return `${dashboardUrl.replace(/\/+$/, '')}/api/sourcemaps`;
}

/** `[1, 50]`, the init posture (contract §4.2, §4.8 step 3): clamped to the nearer bound, one `warn`. */
function resolveBatchSize(given: number | undefined, warn: (line: string) => void): number {
  if (given === undefined) return MAX_FILES_PER_REQUEST;
  if (typeof given !== 'number' || !Number.isFinite(given)) {
    warn(`batch size ${String(given)} is not a finite number; using ${MAX_FILES_PER_REQUEST}`);
    return MAX_FILES_PER_REQUEST;
  }
  const whole = Math.trunc(given);
  if (whole < 1) {
    warn(`batch size ${given} is below 1; using 1`);
    return 1;
  }
  if (whole > MAX_FILES_PER_REQUEST) {
    warn(`batch size ${given} is above ${MAX_FILES_PER_REQUEST} (the route's per-request cap); using ${MAX_FILES_PER_REQUEST}`);
    return MAX_FILES_PER_REQUEST;
  }
  return whole;
}

function is2xx(status: number): boolean {
  return status >= 200 && status < 300;
}

function is4xx(status: number): boolean {
  return status >= 400 && status < 500;
}

function networkLine(err: unknown): string {
  const code = (err as { code?: unknown } | null)?.code;
  const detail = typeof code === 'string' ? code : err instanceof Error ? err.message : String(err);
  return `connection failed (${detail})`;
}

/** The route's success body, read leniently: a 2xx that is not the expected JSON is still a success with no rows to print. */
function readStoredFiles(body: string): StoredFile[] {
  try {
    const parsed: unknown = JSON.parse(body);
    const files = (parsed as { files?: unknown } | null)?.files;
    if (!Array.isArray(files)) return [];
    return files.map((f: unknown) => {
      const o = (typeof f === 'object' && f !== null ? f : {}) as Record<string, unknown>;
      return {
        minified_path: typeof o.minified_path === 'string' ? o.minified_path : '',
        sha256: typeof o.sha256 === 'string' ? o.sha256 : '',
        size_bytes: typeof o.size_bytes === 'number' ? o.size_bytes : 0,
        outcome: typeof o.outcome === 'string' ? o.outcome : '',
      };
    });
  } catch {
    return [];
  }
}

interface Attempted {
  readonly response: UploadResponse;
  readonly attempts: number;
}

/** One batch through the ladder: 2xx returns; 4xx throws `rejected`; anything else is retried, then `unavailable`. */
async function sendWithRetry(request: UploadRequest, timeoutMs: number, send: SendFn, sleep: SleepFn, onEvent: (e: UploadEvent) => void): Promise<Attempted> {
  let last: { status: number | undefined; line: string; code?: string | undefined; details?: unknown; text?: string | undefined; cause?: unknown } | undefined;
  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
    let response: UploadResponse | undefined;
    try {
      response = await send(request, timeoutMs);
    } catch (err) {
      last = { status: undefined, line: networkLine(err), cause: err };
    }
    if (response) {
      if (is2xx(response.status)) return { response, attempts: attempt };
      const described = describeResponse(response.status, response.body);
      const fields = { status: described.status, code: described.code, details: described.details, text: described.text };
      if (is4xx(response.status)) throw new SourcemapUploadError('rejected', described.line, { ...fields, attempts: attempt });
      last = { ...fields, line: described.line };
    }
    if (attempt < MAX_ATTEMPTS) {
      const delayMs = Math.min(RETRY_BASE_MS * 2 ** (attempt - 1), RETRY_CAP_MS);
      onEvent({ type: 'retry', attempt, delayMs, status: last?.status, line: last?.line ?? '' });
      await sleep(delayMs);
    }
  }
  const failure = last ?? { status: undefined, line: 'no attempt made' };
  throw new SourcemapUploadError('unavailable', `${failure.line} after ${MAX_ATTEMPTS} attempts`, {
    status: failure.status,
    attempts: MAX_ATTEMPTS,
    code: failure.code,
    details: failure.details,
    text: failure.text,
    cause: failure.cause,
  });
}

export async function uploadSourcemaps(opts: UploadSourcemapsOptions): Promise<UploadReport> {
  const onEvent = opts.onEvent ?? (() => {});
  const warn = (line: string): void => onEvent({ type: 'warn', line });
  const release = validateRelease(opts.release);
  const url = resolveDashboardUrl(opts.dashboardUrl);
  if (typeof opts.apiKey !== 'string' || opts.apiKey.length === 0) throw new SourcemapUploadError('usage', 'api key is missing');
  const batchSize = resolveBatchSize(opts.batchSize, warn);

  const pairs = await walkSourcemaps(opts.buildDir, opts.publicPrefix, { onWarn: warn });
  if (pairs.length === 0) throw new SourcemapUploadError('no_maps', `no *.js.map under ${opts.buildDir}`);
  for (const pair of pairs) {
    if (!pair.siblingExists) warn(`${pair.path}: no ${pair.chunkPath} beside it; uploading the map anyway`);
  }

  const boundary = newBoundary();
  const batches = packBatches(pairs, { boundary, release, maxFiles: batchSize, maxBytes: MAX_REQUEST_BYTES });
  if (opts.dryRun) return { dryRun: true, release, pairs, requests: batches.length, batches: [], files: [], deleted: [] };

  const headers: Record<string, string> = { 'content-type': `multipart/form-data; boundary=${boundary}`, 'x-api-key': opts.apiKey };
  if (opts.projectKey) headers['x-project-key'] = opts.projectKey;
  const send = opts.send ?? sendWithNodeHttp;
  const sleep = opts.sleep ?? defaultSleep;
  const timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;

  const lastBatchOf = lastBatchByPath(batches);
  const results: BatchResult[] = [];
  for (const [index, batch] of batches.entries()) {
    const request: UploadRequest = { url, headers, contentLength: batch.contentLength, body: () => batchBody(batch, { boundary, release }) };
    const { response, attempts } = await sendWithRetry(request, timeoutMs, send, sleep, onEvent);
    const deleted = opts.deleteAfterUpload ? await unlinkBatch(batch, (path) => lastBatchOf.get(path) === index) : [];
    const result: BatchResult = { index, status: response.status, attempts, contentLength: batch.contentLength, sent: batch.pairs, files: readStoredFiles(response.body), deleted };
    results.push(result);
    onEvent({ type: 'batch', batch: result });
  }
  return {
    dryRun: false,
    release,
    pairs,
    requests: batches.length,
    batches: results,
    files: results.flatMap((r) => r.files),
    deleted: results.flatMap((r) => r.deleted),
  };
}

/** The index of the last batch carrying each map file — a map two chunks reference may sit in two batches. */
function lastBatchByPath(batches: readonly PackedBatch[]): Map<string, number> {
  const last = new Map<string, number>();
  batches.forEach((batch, index) => {
    for (const pair of batch.pairs) last.set(pair.path, index);
  });
  return last;
}

/** Step 6: only this batch's maps, only after its 2xx, each file once and only when no later batch still needs it. */
async function unlinkBatch(batch: PackedBatch, isLastUse: (path: string) => boolean): Promise<string[]> {
  const deleted: string[] = [];
  for (const pair of batch.pairs) {
    if (deleted.includes(pair.path) || !isLastUse(pair.path)) continue;
    await unlink(pair.path);
    deleted.push(pair.path);
  }
  return deleted;
}
