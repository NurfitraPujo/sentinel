// `captureException` / `captureMessage` (contract §4.3), in the pinned order of operations: dedup check → build
// the event (bounds and the shrink applied, the safe serialisation used for every byte count) → sampling → PII
// scrub → `beforeSend` → re-stamp the SDK-owned fields → bounds, shrink and scrub re-applied → serialise once →
// enqueue the string. Neither call ever throws, and both return the `event_id` whether or not the event was sent.

import { boundBreadcrumbsTotal, copyBreadcrumbs, normaliseBreadcrumb } from './breadcrumbs.js';
import { applyBrowserKeepaliveTrim, applyPublishShed } from './size.js';
import { filterRequestInfo, filterUrlsInText } from './url.js';
import {
  applyBounds,
  assembleMetadata,
  classify,
  decideFrames,
  LEVELS,
  parseTraceparent,
  readFingerprint,
  readLevel,
  shrinkMetadata,
  type TruncatedField,
  TRUNCATED_VOCABULARY,
} from './event.js';
import { scrubPII } from './pii.js';
import { debug, reportError } from './report.js';
import { type JsonValue, metadataWireBytes, sanitize } from './serialize.js';
import { utf8Bytes } from './bytes.js';
import { buildUser, coerceTagValue, tagValueProblem, TAG_KEY_PATTERN } from './scope.js';
import {
  boundContexts,
  boundRequestStrings,
  boundStringMap,
  MAX_REQUEST_ENV_VALUE_CODEPOINTS,
  MAX_REQUEST_HEADER_KEY_CODEPOINTS,
  MAX_REQUEST_HEADER_VALUE_CODEPOINTS,
  MAX_REQUEST_HEADERS,
} from './server-bounds.js';
import { currentPageTrace, getSlot, type InternalClient, type Scope } from './state.js';
import type { CaptureContext, CaptureSource, Level, RequestInfo, SentinelEvent, StackFrame, User } from './types.js';
import { SDK_VERSION } from './version.js';
import { MAX_RELEASE_BYTES } from './init.js';

const MAX_TAGS_PER_EVENT = 50;
const MAX_TRANSACTION_CODEPOINTS = 200;
const MAX_SERVER_NAME_CODEPOINTS = 256;
const MAX_REQUEST_DATA_BYTES = 16_384;
const ENVIRONMENT_PATTERN = /^[a-z0-9][a-z0-9._-]{0,49}$/;

function codePoints(s: string): string[] {
  return Array.from(s);
}

function cutCodePoints(s: string, max: number): string {
  const cps = codePoints(s);
  return cps.length > max ? cps.slice(0, max).join('') : s;
}

export interface CaptureInput {
  kind: 'exception' | 'message';
  value: unknown;
  ctx: CaptureContext | undefined;
  /** Set only by a global handler (§4.6, decision 37); never by the two public capture calls. */
  source?: CaptureSource;
  /**
   * Decision 22: the two node global-handler captures call `beforeSend` synchronously and ignore a promise-returning
   * hook's result — the event is sent as built, with one `debug` line. Never set by the public calls.
   */
  syncHookOnly?: boolean;
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

/** `user` for one event (spec §3.4, §3.7): `ctx.user` replaces the scope user; `null` sends no user. */
function buildEventUser(client: InternalClient, scope: Scope, ctxUser: User | null | undefined): User | undefined {
  if (ctxUser === undefined) {
    return scope.user === undefined ? undefined : { ...scope.user };
  }
  if (ctxUser === null || !isPlainRecord(ctxUser)) return undefined;
  return buildUser(ctxUser as Record<string, unknown>, (line) => debug(client, `ctx.user: ${line}`));
}

/** The tag effective for one event and whether the 50-tag cap fired (spec §3.5 rule 6, §3.7). */
interface EventTags {
  tags?: Record<string, string>;
  capped: boolean;
}

/**
 * The scope's tags merged with `ctx.tags` (spec §3.7: `null` removes a scope tag; `tenant_id` is allowed here),
 * then capped to 50 keeping `tenant_id` (rule 6). Built by `forEach` into a fresh object (§5.1).
 */
function buildEventTags(client: InternalClient, scope: Scope, ctxTags: Record<string, string | number | boolean | null> | undefined): EventTags {
  const map = new Map(scope.tags);
  if (ctxTags !== undefined && ctxTags !== null && typeof ctxTags === 'object') {
    for (const key of Object.keys(ctxTags)) {
      const value = ctxTags[key];
      if (value === null || value === undefined) {
        map.delete(key);
        continue;
      }
      const coerced = coerceTagValue(value);
      if (coerced === undefined) {
        debug(client, `ctx.tags['${key}'] dropped: the value is not a string, finite number or boolean (spec §3.5 rule 2)`);
        map.delete(key);
        continue;
      }
      if (!TAG_KEY_PATTERN.test(key)) {
        debug(client, `ctx.tags['${key}'] dropped: the key does not match ^[a-zA-Z0-9_.:-]{1,32}$ (spec §3.5 rule 3)`);
        map.delete(key);
        continue;
      }
      const problem = tagValueProblem(coerced);
      if (problem !== undefined) {
        debug(client, `ctx.tags['${key}'] dropped: the value ${problem} (spec §3.5 rule 3)`);
        map.delete(key);
        continue;
      }
      map.set(key, coerced);
    }
  }
  const capped = cap50Tags(map, client);
  if (map.size === 0) return { capped };
  const tags: Record<string, string> = {};
  map.forEach((v, k) => {
    tags[k] = v;
  });
  return { tags, capped };
}

/** Rule 6's 50-tag cap, `tenant_id` kept, in place on `map`. Returns whether it fired. */
function cap50Tags(map: Map<string, string>, client: InternalClient | undefined): boolean {
  if (map.size <= MAX_TAGS_PER_EVENT) return false;
  const hasTenant = map.has('tenant_id');
  const others = [...map.keys()].filter((k) => k !== 'tenant_id').sort();
  const keepCount = hasTenant ? MAX_TAGS_PER_EVENT - 1 : MAX_TAGS_PER_EVENT;
  const kept = others.slice(0, keepCount);
  const dropped = others.slice(keepCount);
  if (dropped.length > 0) {
    debug(client, `the 50-tag cap dropped ${dropped.length} key(s): ${dropped.join(', ')} (spec §3.5 rule 6)`);
  }
  const next = new Map<string, string>();
  for (const k of kept) next.set(k, map.get(k) as string);
  if (hasTenant) next.set('tenant_id', map.get('tenant_id') as string);
  map.clear();
  for (const [k, v] of next) map.set(k, v);
  return dropped.length > 0;
}

/**
 * `tags` after `beforeSend` (spec §3.8 row): rule 2-3 re-applied per entry (a bad key/value dropped, no
 * previous-value-deletion semantics here — this is a fresh object, not the scope), then rule 6's cap.
 * Not a non-null, non-array object → `undefined` (removed).
 */
function revalidateHookTags(client: InternalClient, raw: unknown): { tags?: Record<string, string>; capped: boolean } {
  if (!isPlainRecord(raw)) return { capped: false };
  const map = new Map<string, string>();
  for (const key of Object.keys(raw)) {
    const value = (raw as Record<string, unknown>)[key];
    const coerced = coerceTagValue(value);
    if (coerced === undefined) {
      debug(client, `beforeSend tags['${key}'] dropped: the value is not a string, finite number or boolean (spec §3.5 rule 2)`);
      continue;
    }
    if (!TAG_KEY_PATTERN.test(key)) {
      debug(client, `beforeSend tags['${key}'] dropped: the key does not match ^[a-zA-Z0-9_.:-]{1,32}$ (spec §3.5 rule 3)`);
      continue;
    }
    const problem = tagValueProblem(coerced);
    if (problem !== undefined) {
      debug(client, `beforeSend tags['${key}'] dropped: the value ${problem} (spec §3.5 rule 3)`);
      continue;
    }
    map.set(key, coerced);
  }
  const capped = cap50Tags(map, client);
  if (map.size === 0) return { capped };
  const tags: Record<string, string> = {};
  map.forEach((v, k) => {
    tags[k] = v;
  });
  return { tags, capped };
}

/** `contexts` for one event (spec §3.6): a copy of the scope's map, built by `forEach` into a fresh object. */
function buildEventContexts(scope: Scope): Record<string, Record<string, unknown>> | undefined {
  if (scope.contexts.size === 0) return undefined;
  const contexts: Record<string, Record<string, unknown>> = {};
  scope.contexts.forEach((v, k) => {
    contexts[k] = v;
  });
  return contexts;
}

type Metadata = { [key: string]: JsonValue };

/**
 * §3.10's mirrored bounds — `contexts`, `request.headers` / `.env` / `.method` / `.url` / `.query_string` — run
 * before the metadata shrink so a `'contexts'` marker is measured (spec §3.10). Applied at `build` and again in
 * `afterHook`.
 */
function applyServerBounds(client: InternalClient, event: SentinelEvent, truncated: Set<TruncatedField>): void {
  if (event.contexts !== undefined) {
    const { contexts, removed } = boundContexts(event.contexts);
    if (removed.length > 0) {
      debug(client, `contexts bound: removed ${removed.join(', ')} (spec §3.10)`);
      truncated.add('contexts');
    }
    if (contexts === undefined) delete event.contexts;
    else event.contexts = contexts;
  }
  const request = event.request;
  if (request !== undefined) {
    const stringsChanged = boundRequestStrings(request);
    if (stringsChanged) debug(client, 'request.method/url/query_string bound (spec §3.10)');
    if (request.headers !== undefined) {
      const { map, changed } = boundStringMap(request.headers, MAX_REQUEST_HEADERS, MAX_REQUEST_HEADER_KEY_CODEPOINTS, MAX_REQUEST_HEADER_VALUE_CODEPOINTS);
      if (changed) debug(client, 'request.headers bound (spec §3.10)');
      if (map === undefined) delete request.headers;
      else request.headers = map;
    }
    if (request.env !== undefined) {
      const { map, changed } = boundStringMap(request.env, MAX_REQUEST_HEADERS, MAX_REQUEST_HEADER_KEY_CODEPOINTS, MAX_REQUEST_ENV_VALUE_CODEPOINTS);
      if (changed) debug(client, 'request.env bound (spec §3.10)');
      if (map === undefined) delete request.env;
      else request.env = map;
    }
    if (Object.keys(request).length === 0) delete event.request;
  }
}

/**
 * Apply §3.10's mirrored bounds, the bounds and the shrink to `event`, whose metadata is plain JSON. Never drops
 * the event (spec §3.7, spec review round 10): the shrink always leaves `metadata` within 65 536 bytes, since
 * `runtime` alone always fits. `metadata_too_large` stays in `SentinelSdkErrorCode` (an app's exhaustive `switch`
 * still compiles) but is no longer emitted.
 */
function boundAndShrink(client: InternalClient, event: SentinelEvent, truncated: Set<TruncatedField>, truncatedKeys: string[]): void {
  applyServerBounds(client, event, truncated);
  applyBounds(event, truncated);
  shrinkMetadata(event.metadata as Metadata, truncated, truncatedKeys);
}

/** The pre-hook event's SDK-owned fields — the hook may have returned a new object (contract §4.2). */
interface Stamp {
  event_id: string;
  platform: 'javascript';
  timestamp: string;
  runtime: JsonValue;
  level: Level;
  sdkName: string;
  environment: string;
}

/** `SentinelEvent`'s exact keys (spec §3.8 step 1) — every other key the hook left is not copied. */
const ALLOWED_EVENT_KEYS: readonly string[] = [
  'event_id',
  'platform',
  'environment',
  'release_version',
  'error_class',
  'message',
  'stacktrace',
  'metadata',
  'timestamp',
  'fingerprint',
  'trace_id',
  'span_id',
  'trace_flags',
  'level',
  'sdk',
  'user',
  'tags',
  'contexts',
  'request',
  'breadcrumbs',
  'transaction',
  'server_name',
];

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
  // Step 1: a fresh object from the allow-list — every other key the hook left is not copied (spec §3.8).
  const src = result as Record<string, unknown>;
  const droppedKeys = Object.keys(src).filter((k) => !ALLOWED_EVENT_KEYS.includes(k));
  if (droppedKeys.length > 0) debug(client, `beforeSend result: dropping unknown key(s) ${droppedKeys.join(', ')} (spec §3.8 step 1)`);
  const returned: Partial<SentinelEvent> & Record<string, unknown> = {};
  for (const k of ALLOWED_EVENT_KEYS) if (k in src) returned[k] = src[k];

  // Step 2: each copied key re-checked.
  returned.event_id = stamp.event_id;
  returned.platform = stamp.platform;
  returned.timestamp = stamp.timestamp;
  returned.sdk = { name: `sentinel.javascript.${stamp.sdkName}`, version: SDK_VERSION };

  if (typeof returned.environment !== 'string' || !ENVIRONMENT_PATTERN.test(returned.environment)) {
    if (returned.environment !== undefined) debug(client, `beforeSend returned an invalid environment ${JSON.stringify(returned.environment)}: the pre-hook value stands (spec §3.8)`);
    returned.environment = stamp.environment;
  }

  if (returned.release_version !== undefined) {
    const rv = returned.release_version;
    if (typeof rv !== 'string' || rv === '' || utf8Bytes(rv) > MAX_RELEASE_BYTES) {
      debug(client, 'beforeSend release_version removed: not a string of 1-100 UTF-8 bytes (spec §3.8)');
      delete returned.release_version;
    }
  }

  const fingerprint = readFingerprint(client, returned.fingerprint);
  if (fingerprint !== undefined) returned.fingerprint = fingerprint;
  else delete returned.fingerprint;

  for (const key of ['trace_id', 'span_id'] as const) {
    const v = returned[key];
    if (v !== undefined && (typeof v !== 'string' || codePoints(v).length > 64)) {
      debug(client, `beforeSend ${key} removed: not a string of ≤ 64 code points (spec §3.8)`);
      delete returned[key];
    }
  }
  if (returned.trace_flags !== undefined) {
    const v = returned.trace_flags;
    if (typeof v !== 'number' || !Number.isInteger(v) || v < 0 || v > 255) {
      debug(client, 'beforeSend trace_flags removed: not an integer 0-255 (spec §3.8)');
      delete returned.trace_flags;
    }
  }

  if (typeof returned.level !== 'string' || !LEVELS.has(returned.level)) {
    if (returned.level !== undefined) debug(client, `beforeSend returned an invalid level ${JSON.stringify(returned.level)}: the pre-hook level stands (spec §3.8)`);
    returned.level = stamp.level;
  }

  const truncatedKeys: string[] = [];
  const truncated = new Set<TruncatedField>();

  if (returned.user !== undefined) {
    if (isPlainRecord(returned.user)) {
      const rebuilt = buildUser(returned.user, (line) => debug(client, `beforeSend user: ${line}`));
      if (rebuilt !== undefined) returned.user = rebuilt;
      else delete returned.user;
    } else {
      debug(client, 'beforeSend user removed: not a plain object (spec §3.8)');
      delete returned.user;
    }
  }

  if (returned.tags !== undefined) {
    const { tags, capped } = revalidateHookTags(client, returned.tags);
    if (capped) truncated.add('tags');
    if (tags !== undefined) returned.tags = tags;
    else delete returned.tags;
  }

  if (returned.contexts !== undefined) {
    if (isPlainRecord(returned.contexts)) {
      const out: Record<string, Record<string, unknown>> = {};
      for (const name of Object.keys(returned.contexts)) {
        const { value } = sanitize((returned.contexts as Record<string, unknown>)[name]);
        if (isPlainRecord(value)) out[name] = value as Record<string, unknown>;
        else debug(client, `beforeSend contexts.${name} removed: serialised to a non-object (spec §3.8)`);
      }
      if (Object.keys(out).length > 0) returned.contexts = out;
      else delete returned.contexts;
    } else {
      debug(client, 'beforeSend contexts removed: not a plain object (spec §3.8)');
      delete returned.contexts;
    }
  }

  if (returned.request !== undefined) {
    if (isPlainRecord(returned.request)) {
      const raw = returned.request as Record<string, unknown>;
      const req: RequestInfo = {};
      if (typeof raw.method === 'string') req.method = raw.method;
      if (typeof raw.url === 'string') req.url = raw.url;
      if (typeof raw.query_string === 'string') req.query_string = raw.query_string;
      if (isPlainRecord(raw.headers)) {
        const headers: Record<string, string> = {};
        for (const k of Object.keys(raw.headers)) {
          const v = (raw.headers as Record<string, unknown>)[k];
          if (typeof v === 'string') headers[k] = v;
        }
        if (Object.keys(headers).length > 0) req.headers = headers;
      }
      if (isPlainRecord(raw.env)) {
        const env: Record<string, string> = {};
        for (const k of Object.keys(raw.env)) {
          const v = (raw.env as Record<string, unknown>)[k];
          if (typeof v === 'string') env[k] = v;
        }
        if (Object.keys(env).length > 0) req.env = env;
      }
      if (raw.data !== undefined) {
        const { value } = sanitize(raw.data);
        if (value !== undefined) {
          const bytes = metadataWireBytes(JSON.stringify(value));
          if (bytes <= MAX_REQUEST_DATA_BYTES) req.data = value;
          else debug(client, `beforeSend request.data removed: over ${MAX_REQUEST_DATA_BYTES} bytes (spec §3.8)`);
        }
      }
      if (Object.keys(req).length > 0) returned.request = filterRequestInfo(req);
      else delete returned.request;
    } else {
      debug(client, 'beforeSend request removed: not a plain object (spec §3.8)');
      delete returned.request;
    }
  }

  if (returned.breadcrumbs !== undefined) {
    if (Array.isArray(returned.breadcrumbs)) {
      const normalised = (returned.breadcrumbs as unknown[]).filter(isPlainRecord).map((c) => normaliseBreadcrumb(c, (line) => debug(client, `beforeSend breadcrumbs: ${line}`)));
      const capped = normalised.length > 100 ? normalised.slice(normalised.length - 100) : normalised;
      const kept = boundBreadcrumbsTotal(capped);
      if (kept.length > 0) returned.breadcrumbs = kept;
      else delete returned.breadcrumbs;
    } else {
      debug(client, 'beforeSend breadcrumbs removed: not an array (spec §3.8)');
      delete returned.breadcrumbs;
    }
  }

  if (returned.transaction !== undefined) {
    if (typeof returned.transaction === 'string') returned.transaction = cutCodePoints(returned.transaction, MAX_TRANSACTION_CODEPOINTS);
    else delete returned.transaction;
  }
  if (returned.server_name !== undefined) {
    if (typeof returned.server_name === 'string') returned.server_name = cutCodePoints(returned.server_name, MAX_SERVER_NAME_CODEPOINTS);
    else delete returned.server_name;
  }

  const rawMetadata = isPlainRecord(returned.metadata) ? returned.metadata : {};
  const metadata: Metadata = {};
  for (const path of Array.isArray(rawMetadata.truncated_keys) ? rawMetadata.truncated_keys.filter((p): p is string => typeof p === 'string') : []) {
    truncatedKeys.push(path);
  }
  for (const f of Array.isArray(rawMetadata.truncated) ? rawMetadata.truncated.filter((f): f is TruncatedField => (TRUNCATED_VOCABULARY as readonly string[]).includes(f as string)) : []) {
    truncated.add(f);
  }
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
        if (typeof f.line === 'number' && Number.isFinite(f.line) && f.line > 0 && f.line <= 2_147_483_647) frame.line = Math.trunc(f.line);
        if (typeof f.column === 'number' && Number.isFinite(f.column) && f.column > 0 && f.column <= 2_147_483_647) frame.column = Math.trunc(f.column);
        return frame;
      })
    : [];

  const event = returned as SentinelEvent;
  boundAndShrink(client, event, truncated, truncatedKeys);
  scrubPII(event.metadata as Metadata);
  return event;
}

/** Serialise once, with the safe replacer, and enqueue into the client current NOW (contract §4.2's pending-hook rule). */
function enqueue(event: SentinelEvent): void {
  const client = getSlot().client;
  if (client === undefined) return;
  if (client.runtime === 'browser') applyBrowserKeepaliveTrim(client, event);
  applyPublishShed(client, event);
  const { value } = sanitize(event);
  client.enqueue(JSON.stringify(value ?? null), event);
}

function isThenable(v: unknown): v is PromiseLike<unknown> {
  return typeof v === 'object' && v !== null && typeof (v as { then?: unknown }).then === 'function';
}

/** The pipeline after the event is built and sampled: scrub, hook, re-stamp, re-apply, enqueue. */
function send(client: InternalClient, event: SentinelEvent, originalException: unknown, syncHookOnly: boolean): void {
  scrubPII(event.metadata as Metadata);
  const hook = client.options.beforeSend;
  if (typeof hook !== 'function') {
    enqueue(event);
    return;
  }
  // The pre-hook copy: what "sent unmodified" means when the hook throws, and where the re-stamp reads from.
  const pre = JSON.parse(JSON.stringify(event)) as SentinelEvent;
  const stamp: Stamp = {
    event_id: pre.event_id,
    platform: 'javascript',
    timestamp: pre.timestamp,
    runtime: pre.metadata.runtime as JsonValue,
    level: pre.level,
    sdkName: client.sdkName,
    environment: pre.environment,
  };
  const threw = (cause: unknown): void => {
    reportError(client, 'before_send_threw', 'beforeSend threw; the event is sent unmodified', { cause });
    enqueue(pre);
  };
  const settle = (result: unknown): void => {
    let normalised: SentinelEvent | undefined;
    try {
      normalised = afterHook(client, result, stamp);
    } catch (cause) {
      // A beforeSend result whose own property access throws (e.g. `get user() { throw }`) is a throwing
      // result, not a throwing hook — but the fail-open rule covers it the same way (spec §3.8): fall back
      // to the pre-hook event rather than losing it silently.
      threw(cause);
      return;
    }
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
    if (syncHookOnly) {
      // Decision 22: on the node crash path there is no async window — the promise's result is ignored and the
      // event goes as built (the pre-hook copy), with one `debug` line (contract §4.6).
      debug(client, `beforeSend returned a promise on a node global-handler capture: its result is ignored and event ${stamp.event_id} is sent as built (decision 22)`);
      Promise.resolve(result).then(undefined, () => undefined);
      enqueue(pre);
      return;
    }
    // A pending hook holds its event out of the queue until it settles, then enqueues into the client current
    // at that moment (contract §4.2); a rejection ≡ a throw. Tracked on the slot so `flush` can wait for it (B9).
    const pending = getSlot().pending;
    const tracked: Promise<void> = Promise.resolve(result)
      .then(settle, threw)
      .catch(() => undefined)
      .then(() => {
        pending.delete(tracked);
      });
    pending.add(tracked);
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
  if (client.noop) {
    // The no-op client (contract §4.2 — `not_configured`, or a refused `endpoint` / `tunnel`): accepted and dropped.
    const id = newEventId();
    debug(client, `no-op client: ${input.kind} ${id} accepted and dropped`);
    return id;
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
  const scope = getSlot().scope;

  let error_class: string;
  let message: string;
  let frames: StackFrame[];
  let originalException: unknown;
  if (input.kind === 'message') {
    error_class = 'Message';
    message = messageText(input.value);
    frames = [];
    originalException = message;
  } else {
    const classified = classify(input.value);
    error_class = classified.error_class;
    message = classified.message;
    const decided = decideFrames(classified.frames, opts.inApp);
    if (decided.threw) debug(client, `init.inApp threw for event ${eventId}; the default in_app stands`);
    frames = decided.frames;
    originalException = input.value;
  }
  // The URL privacy filter runs on every capture path, before boundAndShrink (spec §5.1). hint.originalException
  // (above) keeps the app's own value; only the wire message is filtered.
  message = filterUrlsInText(message);
  // captureException: ctx.level when valid, else 'error' (spec §3.2). captureMessage: ctx.level when valid,
  // else 'info' (Sentry's default, D5).
  const level: Level = readLevel(ctx) ?? (input.kind === 'message' ? 'info' : 'error');

  const assembled = assembleMetadata(client, scope, { ctx, runtime: client.runtime, source: input.source });

  const event: SentinelEvent = {
    event_id: eventId,
    platform: 'javascript',
    environment: opts.environment,
    error_class,
    message,
    stacktrace: frames,
    metadata: assembled.metadata,
    timestamp: new Date().toISOString(),
    level,
    sdk: { name: `sentinel.javascript.${client.sdkName}`, version: SDK_VERSION },
  };
  if (opts.release !== '') event.release_version = opts.release;
  const fingerprint = readFingerprint(client, ctx?.fingerprint);
  if (fingerprint !== undefined) event.fingerprint = fingerprint;
  // A non-empty traceparent (even a malformed one) is the caller's word; only a capture with none — undefined,
  // null (`headers.get('traceparent')`) or '' — gets the browser page's trace (spec *Trace propagation*).
  const tp = ctx?.traceparent as unknown;
  const absent = tp === undefined || tp === null || tp === '';
  const page = absent ? currentPageTrace(client) : undefined;
  const trace = page !== undefined ? { ...page, trace_flags: 1 } : parseTraceparent(tp);
  if (trace !== undefined) {
    event.trace_id = trace.trace_id;
    event.span_id = trace.span_id;
    event.trace_flags = trace.trace_flags;
  }

  let user = buildEventUser(client, scope, ctx?.user);
  // autoIpAddress (spec §3.4): browser only; a browser event with no ip_address gets "{{auto}}".
  if (client.runtime === 'browser' && opts.autoIpAddress && (user === undefined || user.ip_address === undefined)) {
    user = { ...user, ip_address: '{{auto}}' };
  }
  if (user !== undefined) event.user = user;

  const truncated = new Set<TruncatedField>();
  const eventTags = buildEventTags(client, scope, ctx?.tags);
  if (eventTags.tags !== undefined) event.tags = eventTags.tags;
  if (eventTags.capped) truncated.add('tags');

  // Automatic contexts (spec §6) merged under the app's setContext values: for each name, a shallow
  // { ...automatic[name], ...setContext[name] } — user-set fields win, a name only one side has is taken as
  // is (spec §3.6). A context the event already carries (a per-call ctx.request/transaction, below) is never
  // overwritten by an automatic one.
  const auto = client.autoContexts?.() ?? {};
  const appContexts = buildEventContexts(scope);
  const contextNames = new Set<string>([...Object.keys(auto.contexts ?? {}), ...Object.keys(appContexts ?? {})]);
  if (contextNames.size > 0) {
    const merged: Record<string, Record<string, unknown>> = {};
    for (const name of contextNames) {
      const a = auto.contexts?.[name];
      const b = appContexts?.[name];
      merged[name] = a !== undefined && b !== undefined ? { ...a, ...b } : ((b ?? a) as Record<string, unknown>);
    }
    event.contexts = merged;
  }

  const request = ctx?.request ?? auto.request;
  if (request !== undefined) event.request = filterRequestInfo(request);
  if (ctx?.transaction !== undefined) event.transaction = ctx.transaction;
  if (auto.server_name !== undefined) event.server_name = auto.server_name;

  const crumbs = copyBreadcrumbs(scope);
  if (crumbs !== undefined) {
    const bounded = boundBreadcrumbsTotal(crumbs);
    if (bounded.length > 0) event.breadcrumbs = bounded;
  }

  boundAndShrink(client, event, truncated, [...assembled.substituted]);

  // Sampling, before the scrub and before `beforeSend` (one roll per event; a deduplicated call never gets here).
  if (opts.sampleRate < 1 && !(Math.random() < opts.sampleRate)) {
    debug(client, `event ${eventId} not sampled (sampleRate ${opts.sampleRate})`);
    return;
  }

  send(client, event, originalException, input.syncHookOnly === true);
}

export function captureException(error: unknown, ctx?: CaptureContext): string {
  return capture({ kind: 'exception', value: error, ctx });
}

export function captureMessage(message: string, levelOrContext?: Level | CaptureContext): string {
  const ctx: CaptureContext | undefined = typeof levelOrContext === 'string' ? { level: levelOrContext } : levelOrContext;
  return capture({ kind: 'message', value: message, ctx });
}
