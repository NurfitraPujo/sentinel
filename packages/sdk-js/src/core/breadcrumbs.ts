// The breadcrumb ring (spec §4.1-§4.4, TASK-02): `addBreadcrumb`'s normalisation into the wire shape,
// `beforeBreadcrumb`'s fail-closed application, the ring's capacity and the total-bytes trim applied at
// capture. Nothing here is browser-specific: the automatic integrations (C10) call `addBreadcrumbToScope`
// with their own `hint`, exactly as a manual `addBreadcrumb` does.

import { debug } from './report.js';
import { type JsonValue, metadataWireBytes, sanitize } from './serialize.js';
import { scrubPII } from './pii.js';
import { filterUrl } from './url.js';
import type { InternalClient, Scope } from './state.js';
import type { BeforeBreadcrumb, Breadcrumb, BreadcrumbHint, Level } from './types.js';

export const DEFAULT_MAX_BREADCRUMBS = 100;
export const MAX_BREADCRUMBS_CAP = 100;
export const MAX_BREADCRUMB_TYPE_CODEPOINTS = 64;
export const MAX_BREADCRUMB_CATEGORY_CODEPOINTS = 128;
export const MAX_BREADCRUMB_MESSAGE_CODEPOINTS = 8192;
export const MAX_BREADCRUMB_DATA_BYTES = 4096;
/** The Go-ruler total (spec §4.4): dropped oldest-first while over, at `build` and again in `afterHook`. */
export const MAX_BREADCRUMBS_TOTAL_BYTES = 131_072;

const LEVELS: ReadonlySet<string> = new Set<Level>(['fatal', 'error', 'warning', 'info', 'debug']);

function codePoints(s: string): string[] {
  return Array.from(s);
}

function cutCodePoints(s: string, max: number): string {
  const cps = codePoints(s);
  return cps.length > max ? cps.slice(0, max).join('') : s;
}

/** `String(value)`, or the unserialisable marker when that throws; a symbol is unserialisable by the rule. */
function stringOrMarker(value: unknown): string {
  if (typeof value === 'symbol') return '[unserializable]';
  try {
    return String(value);
  } catch {
    return '[unserializable]';
  }
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** `timestamp` (spec §4.2): a Date, Unix seconds, or a Date.parse-able string → toISOString(); unusable → now. */
function normaliseTimestamp(value: unknown): string {
  const now = (): string => new Date().toISOString();
  let ms: number | undefined;
  if (value instanceof Date) {
    ms = value.getTime();
  } else if (typeof value === 'number' && Number.isFinite(value)) {
    ms = value * 1000;
  } else if (typeof value === 'string') {
    const parsed = Date.parse(value);
    ms = Number.isNaN(parsed) ? undefined : parsed;
  }
  if (ms === undefined || !Number.isFinite(ms)) return now();
  let iso: string;
  try {
    iso = new Date(ms).toISOString();
  } catch {
    return now();
  }
  // toISOString() emits an expanded year ("+010000-...") outside 0000-9999, which Go's RFC 3339 decoder
  // rejects (S1) — treat that the same as any other unusable timestamp.
  return /^\d{4}-/.test(iso) ? iso : now();
}

/**
 * §4.2's normalisation, run at `addBreadcrumb` time, on a `beforeBreadcrumb` result and on every element of a
 * `beforeSend` result's `breadcrumbs` (§3.8) — the same function so the post-`sanitize` `data` check holds on
 * all three paths. `say` gets one line per cut / omitted / dropped field.
 */
export function normaliseBreadcrumb(raw: Record<string, unknown>, say: (line: string) => void): Breadcrumb {
  const out: Breadcrumb = { timestamp: normaliseTimestamp(raw.timestamp) };

  if (raw.type !== undefined) {
    const type = stringOrMarker(raw.type);
    const cut = cutCodePoints(type, MAX_BREADCRUMB_TYPE_CODEPOINTS);
    if (cut !== type) say(`type cut to ${MAX_BREADCRUMB_TYPE_CODEPOINTS} code points (spec §4.2)`);
    out.type = cut;
  } else {
    out.type = 'default';
  }

  if (raw.category !== undefined) {
    const category = stringOrMarker(raw.category);
    const cut = cutCodePoints(category, MAX_BREADCRUMB_CATEGORY_CODEPOINTS);
    if (cut !== category) say(`category cut to ${MAX_BREADCRUMB_CATEGORY_CODEPOINTS} code points (spec §4.2)`);
    out.category = cut;
  }

  if (raw.message !== undefined) {
    const message = stringOrMarker(raw.message);
    const cut = cutCodePoints(message, MAX_BREADCRUMB_MESSAGE_CODEPOINTS);
    if (cut !== message) say(`message cut to ${MAX_BREADCRUMB_MESSAGE_CODEPOINTS} code points (spec §4.2)`);
    out.message = cut;
  }

  out.level = typeof raw.level === 'string' && LEVELS.has(raw.level) ? (raw.level as Level) : 'info';

  if (raw.data !== undefined && raw.data !== null) {
    if (!isPlainObject(raw.data)) {
      say('data dropped: not a plain object (spec §4.2)');
    } else {
      const { value } = sanitize(raw.data);
      if (typeof value !== 'object' || value === null || Array.isArray(value)) {
        say('data dropped: serialised to a non-object (spec §4.2)');
      } else {
        const scrubbed = scrubPII(value as { [key: string]: JsonValue });
        // spec §5.1: url / from / to run through the URL privacy filter for a manual crumb exactly as they
        // do for every automatic one (xhr/fetch's `url`, navigation's `from`/`to`).
        for (const key of ['url', 'from', 'to'] as const) {
          const v = scrubbed[key];
          if (typeof v === 'string') scrubbed[key] = filterUrl(v);
        }
        const bytes = metadataWireBytes(JSON.stringify(scrubbed));
        if (bytes > MAX_BREADCRUMB_DATA_BYTES) {
          say(`data removed: over ${MAX_BREADCRUMB_DATA_BYTES} bytes (spec §4.2)`);
        } else {
          out.data = scrubbed as Record<string, unknown>;
        }
      }
    }
  }

  return out;
}

/** `crumb` is not a non-null, non-array object → `addBreadcrumb` is a no-op with a debug line (spec §4.2). */
function isUsableCrumb(crumb: unknown): crumb is Record<string, unknown> {
  return isPlainObject(crumb);
}

/**
 * `addBreadcrumb`'s full path (spec §4.1-§4.3): normalise, run `beforeBreadcrumb` (fail closed — `null`, a
 * throw, or anything but a non-null non-array non-thenable object drops the crumb), re-normalise the hook's
 * result, then push into the ring and trim to `maxBreadcrumbs` from the front (oldest dropped).
 */
export function addBreadcrumbToScope(client: InternalClient | undefined, scope: Scope, crumb: unknown, hint: BreadcrumbHint): void {
  const say = (line: string): void => debug(client, `addBreadcrumb: ${line}`);
  if (!isUsableCrumb(crumb)) {
    say('the crumb is not a plain object — no-op');
    return;
  }
  const maxBreadcrumbs = client?.options.maxBreadcrumbs ?? DEFAULT_MAX_BREADCRUMBS;
  if (maxBreadcrumbs === 0) return;
  let normalised: Breadcrumb | undefined = normaliseBreadcrumb(crumb, say);
  const hook = client?.options.beforeBreadcrumb;
  if (typeof hook === 'function') {
    normalised = runBeforeBreadcrumb(hook, normalised, hint, say);
  }
  if (normalised === undefined) return;
  scope.breadcrumbs.push(normalised);
  if (scope.breadcrumbs.length > maxBreadcrumbs) {
    scope.breadcrumbs.splice(0, scope.breadcrumbs.length - maxBreadcrumbs);
  }
}

function isThenable(v: unknown): v is PromiseLike<unknown> {
  return typeof v === 'object' && v !== null && typeof (v as { then?: unknown }).then === 'function';
}

/** §4.3's hook application; `undefined` return means the crumb is dropped. */
function runBeforeBreadcrumb(hook: BeforeBreadcrumb, crumb: Breadcrumb, hint: BreadcrumbHint, say: (line: string) => void): Breadcrumb | undefined {
  let result: unknown;
  try {
    result = hook(crumb, hint);
  } catch {
    say('beforeBreadcrumb threw — the crumb is dropped (spec §4.3)');
    return undefined;
  }
  if (result === null) return undefined;
  if (!isPlainObject(result) || isThenable(result)) {
    say('beforeBreadcrumb returned something other than a plain object — the crumb is dropped (spec §4.3)');
    return undefined;
  }
  return normaliseBreadcrumb(result, say);
}

/** `client.options`'s current `maxBreadcrumbs`; trims the ring (oldest dropped) — a second `init` (spec §4.1). */
export function trimBreadcrumbs(scope: Scope, maxBreadcrumbs: number): void {
  if (scope.breadcrumbs.length > maxBreadcrumbs) {
    scope.breadcrumbs.splice(0, scope.breadcrumbs.length - maxBreadcrumbs);
  }
}

/**
 * On a new client with a `beforeBreadcrumb`, every retained crumb (recorded before the first `init`, or under a
 * previous client) is passed through it, in order, and replaced or dropped (spec §4.1) — so nothing in the
 * buffer has skipped the hook the current client installed.
 */
export function replayBeforeBreadcrumb(client: InternalClient, scope: Scope): void {
  const hook = client.options.beforeBreadcrumb;
  if (typeof hook !== 'function') return;
  const say = (line: string): void => debug(client, `addBreadcrumb: ${line}`);
  const next: Breadcrumb[] = [];
  for (const crumb of scope.breadcrumbs) {
    const replayed = runBeforeBreadcrumb(hook, crumb, {}, say);
    if (replayed !== undefined) next.push(replayed);
  }
  scope.breadcrumbs = next;
  trimBreadcrumbs(scope, client.options.maxBreadcrumbs);
}

/**
 * The event's `breadcrumbs` (spec §4.4): a copy of the buffer, each crumb copied as
 * `{ ...crumb, data: sanitize(crumb.data).value }` (never `structuredClone`, §5.1), omitted when empty.
 */
export function copyBreadcrumbs(scope: Scope): Breadcrumb[] | undefined {
  if (scope.breadcrumbs.length === 0) return undefined;
  return scope.breadcrumbs.map((crumb) => {
    const copy: Breadcrumb = { ...crumb };
    if (crumb.data !== undefined) {
      const { value } = sanitize(crumb.data);
      if (value !== undefined && typeof value === 'object' && value !== null && !Array.isArray(value)) {
        copy.data = value as Record<string, unknown>;
      } else {
        delete copy.data;
      }
    }
    return copy;
  });
}

/** The server's `Breadcrumb` struct, all six keys filled, for the Go-ruler measure (spec §4.4). */
function fullCrumbBytes(crumb: Breadcrumb): number {
  const full = {
    timestamp: crumb.timestamp,
    type: crumb.type ?? '',
    category: crumb.category ?? '',
    message: crumb.message ?? '',
    level: crumb.level ?? '',
    data: crumb.data ?? null,
  };
  return metadataWireBytes(JSON.stringify(full));
}

/**
 * The 131 072-byte total (spec §4.4, Go ruler): drop the OLDEST crumbs while over. `arraySize = sum + 2 +
 * (n − 1)` mirrors the server's own array-size accounting so the SDK never keeps what the server would trim.
 */
export function boundBreadcrumbsTotal(crumbs: Breadcrumb[]): Breadcrumb[] {
  if (crumbs.length === 0) return crumbs;
  const sizes = crumbs.map(fullCrumbBytes);
  let sum = sizes.reduce((a, b) => a + b, 0);
  let start = 0;
  let n = crumbs.length;
  const total = (): number => (n === 0 ? 0 : sum + 2 + (n - 1));
  while (total() > MAX_BREADCRUMBS_TOTAL_BYTES && n > 0) {
    sum -= sizes[start] as number;
    start += 1;
    n -= 1;
  }
  return start === 0 ? crumbs : crumbs.slice(start);
}
