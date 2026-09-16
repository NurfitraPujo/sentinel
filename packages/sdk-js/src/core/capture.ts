// `captureException` / `captureMessage` (contract §4.3), in the pinned order of operations: dedup check → build
// the event (bounds and the shrink applied, the safe serialisation used for every byte count) → sampling → PII
// scrub → `beforeSend` → re-stamp the SDK-owned fields → bounds, shrink and scrub re-applied → serialise once →
// enqueue the string. Neither call ever throws, and both return the `event_id` whether or not the event was sent.

import {
  applyBounds,
  assembleMetadata,
  classify,
  decideFrames,
  parseTraceparent,
  readFingerprint,
  readLevel,
  shrinkMetadata,
  type TruncatedField,
  TRUNCATED_VOCABULARY,
} from './event.js';
import { scrubPII } from './pii.js';
import { debug, reportError } from './report.js';
import { type JsonValue, sanitize } from './serialize.js';
import { getSlot, type InternalClient } from './state.js';
import type { CaptureContext, CaptureSource, SentinelEvent, StackFrame } from './types.js';

export interface CaptureInput {
  kind: 'exception' | 'message';
  value: unknown;
  ctx: CaptureContext | undefined;
  /** Set only by a global handler (§4.6, decision 37); never by the two public capture calls. */
  source?: CaptureSource;
}

function newEventId(): string {
  return globalThis.crypto.randomUUID();
}

/** The `Error` branch's message when `String(message)` throws — `String` of a symbol never does. */
function messageText(value: unknown): string {
  if (typeof value === 'symbol') return '[unserializable]';
  try {
    return String(value);
  } catch {
    return '[unserializable]';
  }
}

function isPlainRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

type Metadata = { [key: string]: JsonValue };

/**
 * Apply the bounds and the shrink to `event`, whose metadata is plain JSON. Returns `false` when the shrink
 * dropped the event (`metadata_too_large` reported here, once, with `count: 1`).
 */
function boundAndShrink(client: InternalClient, event: SentinelEvent, truncated: Set<TruncatedField>, truncatedKeys: string[]): boolean {
  applyBounds(event, truncated);
  if (shrinkMetadata(event.metadata as Metadata, truncated, truncatedKeys)) return true;
  reportError(client, 'metadata_too_large', 'metadata exceeds 65536 bytes on exempt keys alone; the event is dropped', { count: 1 });
  return false;
}

/** The pre-hook event's SDK-owned fields — the hook may have returned a new object (contract §4.2). */
interface Stamp {
  event_id: string;
  platform: 'javascript';
  timestamp: string;
  runtime: JsonValue;
}

/**
 * What `beforeSend` returned, normalised: the four SDK fields re-stamped from the pre-hook event, metadata
 * re-sanitised (new substitution paths appended to `truncated_keys`), the bounds, the shrink and the scrub
 * re-applied. Everything else the hook set stands. `undefined` means the event is dropped.
 */
function afterHook(client: InternalClient, result: unknown, stamp: Stamp): SentinelEvent | undefined {
  if (result === null) {
    debug(client, `beforeSend returned null: event ${stamp.event_id} dropped`);
    return undefined;
  }
  if (!isPlainRecord(result)) {
    debug(client, `beforeSend returned ${typeof result}: event ${stamp.event_id} dropped (a non-object non-null result is a drop)`);
    return undefined;
  }
  const returned = result as Partial<SentinelEvent> & Record<string, unknown>;
  returned.event_id = stamp.event_id;
  returned.platform = stamp.platform;
  returned.timestamp = stamp.timestamp;

  const rawMetadata = isPlainRecord(returned.metadata) ? returned.metadata : {};
  const metadata: Metadata = {};
  const truncatedKeys: string[] = Array.isArray(rawMetadata.truncated_keys)
    ? rawMetadata.truncated_keys.filter((p): p is string => typeof p === 'string')
    : [];
  const truncated = new Set<TruncatedField>(
    Array.isArray(rawMetadata.truncated)
      ? rawMetadata.truncated.filter((f): f is TruncatedField => (TRUNCATED_VOCABULARY as readonly string[]).includes(f as string))
      : [],
  );
  for (const key of Object.keys(rawMetadata)) {
    if (key === 'truncated' || key === 'truncated_keys') continue;
    const { value, substituted } = sanitize(rawMetadata[key], [key]);
    if (value !== undefined) metadata[key] = value;
    for (const path of substituted) if (!truncatedKeys.includes(path)) truncatedKeys.push(path);
  }
  metadata.runtime = stamp.runtime;
  returned.metadata = metadata;

  returned.error_class = messageText(returned.error_class ?? '');
  returned.message = messageText(returned.message ?? '');
  returned.stacktrace = Array.isArray(returned.stacktrace)
    ? returned.stacktrace.filter(isPlainRecord).map((f): StackFrame => {
        const frame: StackFrame = {
          file: typeof f.file === 'string' ? f.file : '',
          function: typeof f.function === 'string' ? f.function : '<anonymous>',
          in_app: f.in_app === true,
        };
        if (typeof f.line === 'number' && Number.isFinite(f.line) && f.line > 0) frame.line = Math.trunc(f.line);
        if (typeof f.column === 'number' && Number.isFinite(f.column) && f.column > 0) frame.column = Math.trunc(f.column);
        return frame;
      })
    : [];

  const event = returned as SentinelEvent;
  if (!boundAndShrink(client, event, truncated, truncatedKeys)) return undefined;
  scrubPII(event.metadata as Metadata);
  return event;
}

/** Serialise once, with the safe replacer, and enqueue into the client current NOW (contract §4.2's pending-hook rule). */
function enqueue(event: SentinelEvent): void {
  const client = getSlot().client;
  if (client === undefined) return;
  const { value } = sanitize(event);
  client.enqueue(JSON.stringify(value ?? null), event);
}

function isThenable(v: unknown): v is PromiseLike<unknown> {
  return typeof v === 'object' && v !== null && typeof (v as { then?: unknown }).then === 'function';
}

/** The pipeline after the event is built and sampled: scrub, hook, re-stamp, re-apply, enqueue. */
function send(client: InternalClient, event: SentinelEvent, originalException: unknown): void {
  scrubPII(event.metadata as Metadata);
  const hook = client.options.beforeSend;
  if (typeof hook !== 'function') {
    enqueue(event);
    return;
  }
  // The pre-hook copy: what "sent unmodified" means when the hook throws, and where the re-stamp reads from.
  const pre = JSON.parse(JSON.stringify(event)) as SentinelEvent;
  const stamp: Stamp = { event_id: pre.event_id, platform: 'javascript', timestamp: pre.timestamp, runtime: pre.metadata.runtime as JsonValue };
  const threw = (cause: unknown): void => {
    reportError(client, 'before_send_threw', 'beforeSend threw; the event is sent unmodified', { cause });
    enqueue(pre);
  };
  const settle = (result: unknown): void => {
    const normalised = afterHook(client, result, stamp);
    if (normalised !== undefined) enqueue(normalised);
  };
  let result: unknown;
  try {
    result = hook(event, { originalException });
  } catch (cause) {
    threw(cause);
    return;
  }
  if (isThenable(result)) {
    // A pending hook holds its event out of the queue until it settles, then enqueues into the client current
    // at that moment (contract §4.2); a rejection ≡ a throw.
    void Promise.resolve(result).then(settle, threw);
    return;
  }
  settle(result);
}

/** The one capture path behind `captureException`, `captureMessage` and the global handlers. */
export function capture(input: CaptureInput): string {
  const slot = getSlot();
  const client = slot.client;
  if (client === undefined) {
    // Before the first `init` or after `close` (contract §4.2): a fresh id, the event dropped, nothing marked.
    return newEventId();
  }
  const isObject = input.kind === 'exception' && (typeof input.value === 'object' || typeof input.value === 'function') && input.value !== null;
  if (isObject) {
    const seen = slot.seen.get(input.value as object);
    if (seen !== undefined) {
      debug(client, `capture of an already-seen object deduplicated to ${seen}`);
      return seen;
    }
  }
  const eventId = newEventId();
  if (isObject) slot.seen.set(input.value as object, eventId);
  try {
    build(client, input, eventId);
  } catch (cause) {
    // Belt and braces: the pipeline is written not to throw; if it ever does, the caller must not see it.
    debug(client, `capture of ${eventId} failed inside the SDK: ${messageText(cause)}`);
  }
  return eventId;
}

function build(client: InternalClient, input: CaptureInput, eventId: string): void {
  const opts = client.options;
  const ctx = input.ctx;

  let error_class: string;
  let message: string;
  let frames: StackFrame[];
  let level = undefined as ReturnType<typeof readLevel>;
  let originalException: unknown;
  if (input.kind === 'message') {
    error_class = 'Message';
    message = messageText(input.value);
    frames = [];
    level = readLevel(ctx);
    originalException = message;
  } else {
    const classified = classify(input.value);
    error_class = classified.error_class;
    message = classified.message;
    const decided = decideFrames(classified.frames, opts.inApp);
    if (decided.threw) debug(client, `init.inApp threw for event ${eventId}; the default in_app stands`);
    frames = decided.frames;
    if (ctx?.level !== undefined) debug(client, 'ctx.level is ignored by captureException (metadata.level comes from captureMessage only)');
    originalException = input.value;
  }

  const assembled = assembleMetadata(client, getSlot().scope, { ctx, runtime: client.runtime, source: input.source, level });

  const event: SentinelEvent = {
    event_id: eventId,
    platform: 'javascript',
    environment: opts.environment,
    error_class,
    message,
    stacktrace: frames,
    metadata: assembled.metadata,
    timestamp: new Date().toISOString(),
  };
  if (opts.release !== '') event.release_version = opts.release;
  const fingerprint = readFingerprint(client, ctx?.fingerprint);
  if (fingerprint !== undefined) event.fingerprint = fingerprint;
  const trace = parseTraceparent(ctx?.traceparent);
  if (trace !== undefined) {
    event.trace_id = trace.trace_id;
    event.span_id = trace.span_id;
    event.trace_flags = trace.trace_flags;
  }

  if (!boundAndShrink(client, event, new Set<TruncatedField>(), [...assembled.substituted])) return;

  // Sampling, before the scrub and before `beforeSend` (one roll per event; a deduplicated call never gets here).
  if (opts.sampleRate < 1 && !(Math.random() < opts.sampleRate)) {
    debug(client, `event ${eventId} not sampled (sampleRate ${opts.sampleRate})`);
    return;
  }

  send(client, event, originalException);
}

export function captureException(error: unknown, ctx?: CaptureContext): string {
  return capture({ kind: 'exception', value: error, ctx });
}

export function captureMessage(message: string, ctx?: CaptureContext): string {
  return capture({ kind: 'message', value: message, ctx });
}
