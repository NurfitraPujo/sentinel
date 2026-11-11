// The module-level scope — `setUser` / `setTenant` / `setTag` / `setTags` / `setContext` / `setExtra` /
// `setExtras` (spec §3.4-§3.7) — held on the `globalThis` slot, so it is global in the browser, per process in
// node and edge, survives a second `init` and is kept from calls made before the first one.

import { addBreadcrumbToScope } from './breadcrumbs.js';
import { debug } from './report.js';
import { sanitize } from './serialize.js';
import { getSlot } from './state.js';
import type { BreadcrumbHint, User } from './types.js';

/**
 * The names `setExtra` / `setExtras` may not set (spec §3.7): the SDK-owned metadata keys and the identity keys
 * `setUser` / `setTenant` own — `use setUser / setTenant` in the debug line, `setTag` for a plain tag.
 */
export const RESERVED_EXTRA_KEYS: ReadonlySet<string> = new Set(['runtime', 'source', 'level', 'truncated', 'truncated_keys', 'user_id', 'tenant_id']);

/**
 * `tenant_id` is the only name `setTag` / `setTags` may not set (spec §3.5 rule 4): reserved for `setTenant`. The
 * other nine names 0.1.x reserved are freed — they reserved metadata keys, and tags no longer live in metadata.
 */
export const RESERVED_TAG_KEYS: ReadonlySet<string> = new Set(['tenant_id']);

export const TAG_KEY_PATTERN = /^[a-zA-Z0-9_.:-]{1,32}$/;
const MAX_TAG_VALUE_CODEPOINTS = 200;
const MAX_USER_ID_BYTES = 255;
const MAX_EMAIL_CODEPOINTS = 254;
const MAX_IP_ADDRESS_CODEPOINTS = 45;
const MAX_USERNAME_CODEPOINTS = 128;
const MAX_CONTEXT_NAME_CODEPOINTS = 64;

function codePoints(s: string): string[] {
  return Array.from(s);
}

function cutCodePoints(s: string, max: number): string {
  const cps = codePoints(s);
  return cps.length > max ? cps.slice(0, max).join('') : s;
}

function utf8ByteLength(s: string): number {
  return new TextEncoder().encode(s).byteLength;
}

/**
 * Rule 4's runtime coercion (contract §4.3): a non-empty string stands; a finite number or a boolean is
 * `String(v)`; `''`, `null`, `undefined`, `NaN`, `±Infinity`, an object, an array, a symbol, a `bigint`, a function
 * are all `''` — and `''` means DELETE the key, never send it. `undefined` here is "delete".
 */
export function coerceIdentity(value: unknown): string | undefined {
  if (typeof value === 'string') return value === '' ? undefined : value;
  if (typeof value === 'number') return Number.isFinite(value) ? String(value) : undefined;
  if (typeof value === 'boolean') return String(value);
  return undefined;
}

function describe(v: unknown): string {
  if (typeof v === 'string') return `'${v}'`;
  if (typeof v === 'number' || typeof v === 'boolean' || typeof v === 'bigint') return String(v);
  if (typeof v === 'symbol') return 'a symbol';
  if (typeof v === 'function') return 'a function';
  if (Array.isArray(v)) return 'an array';
  return 'an object';
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

const USER_KNOWN_KEYS = ['id', 'username', 'email', 'ip_address'];

/**
 * The validation rules of §3.4, on a raw candidate — used at `setUser` write time and per-call for `ctx.user`
 * (§3.7). `say` receives one line per admitted, cut or omitted key; the caller decides what to do with it
 * (a scope write logs through `debug`, a per-call build logs the same way). Returns `undefined` when nothing
 * usable is left.
 */
export function buildUser(raw: Record<string, unknown>, say: (line: string) => void): User | undefined {
  const unknownKeys = Object.keys(raw).filter((k) => !USER_KNOWN_KEYS.includes(k));
  if (unknownKeys.length > 0) {
    say(`ignoring unknown key(s) ${unknownKeys.join(', ')} (spec §3.4)`);
  }

  const out: User = {};
  if (raw.id !== undefined) {
    const coerced = coerceIdentity(raw.id);
    if (coerced === undefined) {
      if (raw.id !== null) say(`id ${describe(raw.id)} is not a usable id — omitted (spec §3.4)`);
    } else if (utf8ByteLength(coerced) > MAX_USER_ID_BYTES) {
      say(`id is over ${MAX_USER_ID_BYTES} UTF-8 bytes — omitted, not truncated (spec §3.4)`);
    } else {
      out.id = coerced;
    }
  }
  if (raw.email !== undefined) {
    if (typeof raw.email === 'string' && raw.email !== '') {
      if (codePoints(raw.email).length > MAX_EMAIL_CODEPOINTS) {
        say(`email is over ${MAX_EMAIL_CODEPOINTS} code points — omitted (spec §3.4)`);
      } else {
        out.email = raw.email;
      }
    } else {
      say('email must be a non-empty string — omitted (spec §3.4)');
    }
  }
  if (raw.ip_address !== undefined) {
    if (typeof raw.ip_address === 'string' && raw.ip_address !== '') {
      if (codePoints(raw.ip_address).length > MAX_IP_ADDRESS_CODEPOINTS) {
        say(`ip_address is over ${MAX_IP_ADDRESS_CODEPOINTS} code points — omitted (spec §3.4)`);
      } else {
        out.ip_address = raw.ip_address;
      }
    } else {
      say('ip_address must be a non-empty string — omitted (spec §3.4)');
    }
  }
  if (raw.username !== undefined) {
    if (typeof raw.username === 'string' && raw.username !== '') {
      const cut = cutCodePoints(raw.username, MAX_USERNAME_CODEPOINTS);
      if (cut !== raw.username) say(`username cut to ${MAX_USERNAME_CODEPOINTS} code points (spec §3.4)`);
      out.username = cut;
    } else {
      say('username must be a non-empty string — omitted (spec §3.4)');
    }
  }

  return Object.keys(out).length === 0 ? undefined : out;
}

/**
 * `setUser(user)` (spec §3.4): REPLACES the scope's user (Sentry semantics). `null`, `undefined`, or an object
 * with no usable key after the rules below, clears it. `id` over 255 UTF-8 bytes, `email` over 254 code points
 * and `ip_address` over 45 code points are OMITTED (never truncated — a truncated id is a different user, the
 * server drops it by the same rule); `username` is CUT to 128 code points; an unrecognised key is ignored with
 * one debug line.
 */
export function setUser(user: User | null): void {
  const slot = getSlot();
  if (user === null || user === undefined || !isPlainObject(user)) {
    delete slot.scope.user;
    return;
  }
  const out = buildUser(user as Record<string, unknown>, (line) => debug(slot.client, `setUser: ${line}`));
  if (out === undefined) delete slot.scope.user;
  else slot.scope.user = out;
}

/** The problem with a tag value (spec §3.5 rule 3), or `undefined` when it is fine. */
export function tagValueProblem(value: string): string | undefined {
  if (value === '') return 'is empty';
  if (codePoints(value).length > MAX_TAG_VALUE_CODEPOINTS) return `is over ${MAX_TAG_VALUE_CODEPOINTS} code points`;
  if (value.includes('\n')) return 'contains a newline';
  return undefined;
}

/** Rule 2's coercion: a string stands; a finite number or boolean is `String(v)`; anything else is unusable. */
export function coerceTagValue(value: unknown): string | undefined {
  if (typeof value === 'string') return value;
  if (typeof value === 'number' && Number.isFinite(value)) return String(value);
  if (typeof value === 'boolean') return String(value);
  return undefined;
}

/**
 * `setTag(key, value)` (spec §3.5): `null` / `undefined` DELETES the tag; a reserved key (`tenant_id`, use
 * `setTenant`) is dropped with a debug line; a non-string/number/boolean value is dropped with a debug line
 * (the previous value under the key stands); a key failing the pattern, or a value that is empty, over 200 code
 * points or holds a newline, is dropped with a debug line AND deletes any previous value under that key (rule 3
 * — the event never carries a stale value the app tried to replace).
 */
export function setTag(key: string, value: string | number | boolean | null): void {
  const slot = getSlot();
  const name = String(key);
  if (RESERVED_TAG_KEYS.has(name)) {
    debug(slot.client, `setTag('${name}') dropped: the key is reserved — use setTenant (spec §3.5 rule 4)`);
    return;
  }
  if (value === null || value === undefined) {
    slot.scope.tags.delete(name);
    return;
  }
  const coerced = coerceTagValue(value);
  if (coerced === undefined) {
    debug(slot.client, `setTag('${name}', ${describe(value)}) dropped: the value is not a string, finite number or boolean (spec §3.5 rule 2)`);
    return;
  }
  if (!TAG_KEY_PATTERN.test(name)) {
    debug(slot.client, `setTag('${name}') dropped: the key does not match ^[a-zA-Z0-9_.:-]{1,32}$ (spec §3.5 rule 3)`);
    slot.scope.tags.delete(name);
    return;
  }
  const problem = tagValueProblem(coerced);
  if (problem !== undefined) {
    debug(slot.client, `setTag('${name}') dropped: the value ${problem} (spec §3.5 rule 3)`);
    slot.scope.tags.delete(name);
    return;
  }
  slot.scope.tags.set(name, coerced);
}

/** `setTags(tags)`: `setTag` for every own key of `tags`, in `Object.keys` order. */
export function setTags(tags: Record<string, string | number | boolean | null>): void {
  if (tags === null || typeof tags !== 'object') return;
  for (const key of Object.keys(tags)) setTag(key, tags[key] ?? null);
}

/** `setTenant(id) → tags.tenant_id` (spec §3.5 rule 5): `coerceIdentity`, then rule 3; unusable or `null` deletes it. */
export function setTenant(id: string | number | null): void {
  const slot = getSlot();
  const coerced = coerceIdentity(id);
  if (coerced === undefined) {
    if (id !== null && id !== undefined) {
      debug(slot.client, `setTenant: value ${describe(id)} is not a usable id — tenant_id deleted from scope (spec §3.5 rule 5)`);
    }
    slot.scope.tags.delete('tenant_id');
    return;
  }
  const problem = tagValueProblem(coerced);
  if (problem !== undefined) {
    debug(slot.client, `setTenant: the id ${problem} — tenant_id deleted from scope (spec §3.5 rule 3)`);
    slot.scope.tags.delete('tenant_id');
    return;
  }
  slot.scope.tags.set('tenant_id', coerced);
}

function isJsonObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * `setContext(name, context)` (spec §3.6): `null` / `undefined` deletes the name. The name must be 1-64 code
 * points; the value must be a non-null, non-array object AFTER `sanitize` (a `toJSON` — `Date`, `URL`, a
 * `dayjs()` value — that serialises to a string or array is dropped, not stored as that string/array), else the
 * call is dropped with a debug line and the scope is unchanged. The stored value is the sanitised copy, so a
 * later mutation of the caller's object never changes the scope.
 */
export function setContext(name: string, context: Record<string, unknown> | null): void {
  const slot = getSlot();
  const key = String(name);
  if (context === null || context === undefined) {
    slot.scope.contexts.delete(key);
    return;
  }
  const nameLength = codePoints(key).length;
  if (nameLength < 1 || nameLength > MAX_CONTEXT_NAME_CODEPOINTS) {
    debug(slot.client, `setContext('${key}') dropped: the name must be 1-${MAX_CONTEXT_NAME_CODEPOINTS} code points (spec §3.6)`);
    return;
  }
  if (!isPlainObject(context)) {
    debug(slot.client, `setContext('${key}') dropped: the value must be a plain object (spec §3.6)`);
    return;
  }
  const { value } = sanitize(context);
  if (!isJsonObject(value)) {
    const kind = value === undefined ? 'undefined' : Array.isArray(value) ? 'array' : typeof value;
    debug(slot.client, `setContext('${key}') dropped: the value serialised to "${kind}" (spec §3.6)`);
    return;
  }
  slot.scope.contexts.set(key, value);
}

/**
 * `setExtra(key, value) → metadata[key]` (Sentry's name, contract §4.1); `null` / `undefined` DELETES the key.
 * A reserved key (`RESERVED_EXTRA_KEYS`) is dropped with a `debug` line naming the API that owns it and no
 * `onError` (spec §3.7).
 */
export function setExtra(key: string, value: unknown): void {
  const slot = getSlot();
  const name = String(key);
  if (RESERVED_EXTRA_KEYS.has(name)) {
    debug(slot.client, `setExtra('${name}') dropped: the key is reserved — use setUser / setTenant / setTag for it (spec §3.7)`);
    return;
  }
  if (value === null || value === undefined) {
    slot.scope.extras.delete(name);
    return;
  }
  slot.scope.extras.set(name, value);
}

/** `setExtras(extras)`: `setExtra` for every own key of `extras`, in `Object.keys` order. */
export function setExtras(extras: Record<string, unknown>): void {
  if (extras === null || typeof extras !== 'object') return;
  for (const key of Object.keys(extras)) setExtra(key, (extras as Record<string, unknown>)[key]);
}

/**
 * `addBreadcrumb(crumb, hint?)` (spec §4.1-§4.3): normalised, run through `beforeBreadcrumb` (fail closed), then
 * pushed into the ring and trimmed to `maxBreadcrumbs` from the front. A no-op before the first `init` records
 * with the default capacity (100), same as every other scope write made before `init`.
 */
export function addBreadcrumb(crumb: unknown, hint: BreadcrumbHint = {}): void {
  const slot = getSlot();
  addBreadcrumbToScope(slot.client, slot.scope, crumb, hint);
}
