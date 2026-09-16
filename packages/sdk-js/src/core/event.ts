// How a thrown value becomes the event (contract §4.3's event table): classification, the metadata assembly
// with decision 38's per-call table, the byte bounds with their `metadata.truncated` markers, and the 64 KiB
// shrink counted by the validator's ruler. Everything here works on sanitised (plain JSON) data and nothing
// throws — the safe serialiser (serialize.ts) has already replaced whatever would.

import { cutUtf8, utf8Bytes } from './bytes.js';
import { debug, reportError } from './report.js';
import { coerceIdentity } from './scope.js';
import { type JsonValue, metadataWireBytes, sanitize, safeStringify, UNSERIALIZABLE } from './serialize.js';
import { decideInApp, parseStack, type RawFrame } from './stack.js';
import type { InternalClient, Scope } from './state.js';
import type { CaptureContext, CaptureSource, Level, SentinelEvent, StackFrame } from './types.js';

export const MAX_ERROR_CLASS_BYTES = 255;
export const MAX_MESSAGE_BYTES = 10_000;
export const MAX_FRAMES = 100;
export const MAX_FILE_BYTES = 512;
export const MAX_FUNCTION_BYTES = 255;
export const MAX_COMPONENT_STACK_BYTES = 4096;
export const MAX_METADATA_BYTES = 65_536;
export const MAX_FINGERPRINT_BYTES = 64;

/** `metadata.truncated`'s fixed vocabulary, in the order it is emitted (contract §4.3 *Truncation markers*). */
export const TRUNCATED_VOCABULARY = [
  'message',
  'error_class',
  'react_component_stack',
  'stacktrace',
  'stacktrace.file',
  'stacktrace.function',
] as const;
export type TruncatedField = (typeof TRUNCATED_VOCABULARY)[number];

/** Never removed by the shrink (step 4): what the processor's indexer and the dashboard route on. */
export const SHRINK_EXEMPT_KEYS: ReadonlySet<string> = new Set([
  'user_id',
  'tenant_id',
  'runtime',
  'route',
  'truncated',
  'truncated_keys',
]);

/** Per-call `ctx.metadata` keys the SDK owns (decision 38): dropped with a `debug` line. */
export const SDK_OWNED_METADATA_KEYS: ReadonlySet<string> = new Set(['runtime', 'source', 'level', 'truncated', 'truncated_keys']);

const LEVELS: ReadonlySet<string> = new Set<Level>(['error', 'warning', 'info']);

// The pattern `web` uses (`src/app/api/trace-headers.ts: TRACEPARENT_PATTERN`).
const TRACEPARENT = /^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$/;

type Metadata = { [key: string]: JsonValue };

/** The `Error` test of decisions 39 and 42: the toString tag OR `instanceof` — cross-realm safe, no duck typing. */
export function isErrorLike(value: unknown): value is Error {
  return Object.prototype.toString.call(value) === '[object Error]' || value instanceof Error;
}

/** `String(value)`, or `"[unserializable]"` when that throws; a symbol is unserialisable by the rule (§4.3). */
function stringOrMarker(value: unknown): string {
  if (typeof value === 'symbol') return UNSERIALIZABLE;
  try {
    return String(value);
  } catch {
    return UNSERIALIZABLE;
  }
}

export interface Classified {
  error_class: string;
  message: string;
  frames: RawFrame[];
}

/** `error_class`, `message` and the raw frames for a `captureException` value (contract §4.3's rows). */
export function classify(value: unknown): Classified {
  if (isErrorLike(value)) {
    let name = '';
    try {
      name = stringOrMarker((value as { name?: unknown }).name ?? '');
    } catch {
      name = '';
    }
    if (name === '' || name === 'Error') {
      // `constructor.name` when the name is empty or `"Error"` on a subclass.
      let ctor = '';
      try {
        const c = (value as { constructor?: { name?: unknown } }).constructor;
        ctor = typeof c?.name === 'string' ? c.name : '';
      } catch {
        ctor = '';
      }
      if (ctor !== '') name = ctor;
    }
    if (name === '') name = 'Error';
    let message: string;
    try {
      message = stringOrMarker((value as { message?: unknown }).message);
    } catch {
      message = UNSERIALIZABLE;
    }
    let stack: unknown;
    try {
      stack = (value as { stack?: unknown }).stack;
    } catch {
      stack = undefined;
    }
    return { error_class: name, message, frames: parseStack(stack) };
  }
  if (typeof value === 'string') {
    return { error_class: 'Error', message: value, frames: [] };
  }
  const asString = stringOrMarker(value);
  return {
    error_class: 'NonError',
    message: asString === '[object Object]' ? safeStringify(value) : asString,
    frames: [],
  };
}

export interface Assembly {
  metadata: Metadata;
  /** The safe serialiser's substitution paths under `metadata`. */
  substituted: string[];
}

export interface AssembleInput {
  ctx: CaptureContext | undefined;
  runtime: string;
  source: CaptureSource | undefined;
  /** `captureMessage`'s `ctx.level`; `captureException` passes nothing (its `level` is ignored). */
  level: Level | undefined;
}

/**
 * `{ ...tags, user_id?, tenant_id?, ...ctx.metadata, runtime, source?, react_component_stack?, level? }` in that
 * order, with decision 38's per-call table applied to `ctx.metadata`, then sanitised value by value.
 */
export function assembleMetadata(client: InternalClient, scope: Scope, input: AssembleInput): Assembly {
  const raw: Record<string, unknown> = {};
  for (const [key, value] of scope.tags) raw[key] = value;
  if (scope.user_id !== undefined) raw.user_id = scope.user_id;
  if (scope.tenant_id !== undefined) raw.tenant_id = scope.tenant_id;

  const perCall = input.ctx?.metadata;
  if (perCall !== null && typeof perCall === 'object') {
    let keys: string[] = [];
    try {
      keys = Object.keys(perCall);
    } catch {
      keys = [];
    }
    for (const key of keys) {
      let value: unknown;
      try {
        value = (perCall as Record<string, unknown>)[key];
      } catch {
        value = UNSERIALIZABLE;
      }
      if (SDK_OWNED_METADATA_KEYS.has(key)) {
        debug(client, `ctx.metadata.${key} dropped: the SDK owns that key (decision 38)`);
        continue;
      }
      if (key === 'user_id' || key === 'tenant_id') {
        const coerced = coerceIdentity(value);
        if (coerced === undefined) {
          debug(client, `ctx.metadata.${key} deleted: not a usable id (contract §4.3 rule 4)`);
          delete raw[key];
        } else {
          raw[key] = coerced;
        }
        continue;
      }
      raw[key] = value;
    }
  }

  raw.runtime = input.runtime;
  if (input.source !== undefined) raw.source = input.source;
  const componentStack = input.ctx?.contexts?.react?.componentStack;
  if (typeof componentStack === 'string') raw.react_component_stack = componentStack;
  if (input.level !== undefined) raw.level = input.level;

  const metadata: Metadata = {};
  const substituted: string[] = [];
  for (const key of Object.keys(raw)) {
    const { value, substituted: paths } = sanitize(raw[key], [key]);
    if (value !== undefined) metadata[key] = value;
    substituted.push(...paths);
  }
  return { metadata, substituted };
}

/** `ctx.level` when it is one of the three, else nothing (contract §4.3: `metadata.level` only from `captureMessage`). */
export function readLevel(ctx: CaptureContext | undefined): Level | undefined {
  const level = ctx?.level;
  return typeof level === 'string' && LEVELS.has(level) ? (level as Level) : undefined;
}

export interface TraceContext {
  trace_id: string;
  span_id: string;
  trace_flags: number;
}

/** W3C `traceparent` → the three wire fields; `trace_flags = parseInt(flags, 16)` (`01` → 1, `10` → 16). */
export function parseTraceparent(traceparent: unknown): TraceContext | undefined {
  if (typeof traceparent !== 'string') return undefined;
  const m = TRACEPARENT.exec(traceparent);
  if (!m) return undefined;
  return { trace_id: m[1] as string, span_id: m[2] as string, trace_flags: parseInt(m[3] as string, 16) };
}

/** The frames with `in_app` decided; `threw` says whether `init.inApp` threw for any of them (one `debug` line per capture). */
export function decideFrames(
  frames: RawFrame[],
  inApp: ((frame: StackFrame) => boolean | undefined) | undefined,
): { frames: StackFrame[]; threw: boolean } {
  let threw = false;
  const out: StackFrame[] = [];
  for (const raw of frames) {
    const decided = decideInApp(raw, inApp);
    threw = threw || decided.threw;
    out.push(decided.frame);
  }
  return { frames: out, threw };
}

function cutRecorded(value: string, max: number, field: TruncatedField, truncated: Set<TruncatedField>): string {
  const cut = cutUtf8(value, max);
  if (cut !== value) truncated.add(field);
  return cut;
}

/**
 * Every byte bound of contract §4.3's table, applied in place and recorded in `truncated`: `error_class` 255,
 * `message` 10 000, the innermost 100 frames, `file` 512, `function` 255, `react_component_stack` 4 KiB.
 */
export function applyBounds(event: SentinelEvent, truncated: Set<TruncatedField>): void {
  event.error_class = cutRecorded(event.error_class, MAX_ERROR_CLASS_BYTES, 'error_class', truncated);
  if (event.error_class === '') event.error_class = 'Error';
  event.message = cutRecorded(event.message, MAX_MESSAGE_BYTES, 'message', truncated);
  if (event.stacktrace.length > MAX_FRAMES) {
    event.stacktrace = event.stacktrace.slice(0, MAX_FRAMES);
    truncated.add('stacktrace');
  }
  for (const frame of event.stacktrace) {
    frame.file = cutRecorded(frame.file, MAX_FILE_BYTES, 'stacktrace.file', truncated);
    frame.function = cutRecorded(frame.function, MAX_FUNCTION_BYTES, 'stacktrace.function', truncated);
    if (frame.function === '') frame.function = '<anonymous>';
    if (frame.file === '') frame.in_app = false;
  }
  const componentStack = event.metadata.react_component_stack;
  if (typeof componentStack === 'string') {
    event.metadata.react_component_stack = cutRecorded(componentStack, MAX_COMPONENT_STACK_BYTES, 'react_component_stack', truncated);
  }
}

function pairBytes(key: string, value: JsonValue): number {
  return metadataWireBytes(JSON.stringify(key) + ':' + JSON.stringify(value));
}

/** Write the two marker arrays (absent when empty) at the end of the object, where the SDK's keys go. */
function writeMarkers(metadata: Metadata, truncated: Set<TruncatedField>, truncatedKeys: string[]): void {
  if (truncated.size > 0) metadata.truncated = TRUNCATED_VOCABULARY.filter((f) => truncated.has(f));
  else delete metadata.truncated;
  if (truncatedKeys.length > 0) metadata.truncated_keys = [...truncatedKeys];
  else delete metadata.truncated_keys;
}

/**
 * The 64 KiB shrink (contract §4.3 *The 64 KiB shrink*, steps 3–5): while the serialised `metadata` exceeds 65 536
 * bytes BY THE VALIDATOR'S COUNT, remove the largest non-exempt key (largest by the same count, ties by insertion
 * order), replacing every substituted sub-path under it in `truncated_keys` by the bare key name. Returns `false`
 * when only exempt keys remain and the object is still over — the caller drops the event (`metadata_too_large`).
 */
export function shrinkMetadata(metadata: Metadata, truncated: Set<TruncatedField>, truncatedKeys: string[]): boolean {
  for (;;) {
    writeMarkers(metadata, truncated, truncatedKeys);
    if (metadataWireBytes(JSON.stringify(metadata)) <= MAX_METADATA_BYTES) return true;
    let victim: string | undefined;
    let victimBytes = -1;
    for (const key of Object.keys(metadata)) {
      if (SHRINK_EXEMPT_KEYS.has(key)) continue;
      const bytes = pairBytes(key, metadata[key] as JsonValue);
      // Strictly larger only: the earlier-inserted key wins a tie.
      if (bytes > victimBytes) {
        victim = key;
        victimBytes = bytes;
      }
    }
    if (victim === undefined) return false;
    delete metadata[victim];
    const prefix = victim + '.';
    const kept = truncatedKeys.filter((path) => path !== victim && !path.startsWith(prefix));
    truncatedKeys.length = 0;
    truncatedKeys.push(...kept, victim);
  }
}

/** `fingerprint` ≤ 64 bytes; over → the FIELD is omitted, the event is sent, `fingerprint_too_long` per capture. */
export function readFingerprint(client: InternalClient, fingerprint: unknown): string | undefined {
  if (typeof fingerprint !== 'string' || fingerprint === '') return undefined;
  if (utf8Bytes(fingerprint) > MAX_FINGERPRINT_BYTES) {
    reportError(client, 'fingerprint_too_long', `fingerprint is ${utf8Bytes(fingerprint)} bytes, over ${MAX_FINGERPRINT_BYTES}; the field is omitted and the event sent`);
    return undefined;
  }
  return fingerprint;
}
