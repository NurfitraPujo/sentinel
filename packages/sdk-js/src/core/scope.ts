// The module-level scope — `setUser` / `setTenant` / `setTag` (contract §4.3) — held on the `globalThis` slot, so
// it is global in the browser, per process in node and edge, survives a second `init` and is kept from calls
// made before the first one. The rules exist because the processor reads the identity keys by precedence
// (`apps/processor-go/indexer/indexer.go: UserIDFromMetadata` — `user_id > userId > user`, a type-asserted string
// at the first PRESENT key, an empty string included), so a stray or empty key blanks the affected-users rollup.

import { debug } from './report.js';
import { getSlot } from './state.js';

/**
 * The names `setTag` may not set (contract §4.3 rule 2): the identity keys and their aliases belong to
 * `setUser` / `setTenant` alone, and the SDK owns the other four.
 */
export const RESERVED_TAG_KEYS: ReadonlySet<string> = new Set([
  'user_id',
  'userId',
  'user',
  'tenant_id',
  'tenantId',
  'organization_id',
  'runtime',
  'level',
  'truncated',
  'truncated_keys',
]);

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

function writeIdentity(key: 'user_id' | 'tenant_id', raw: unknown, writer: string): void {
  const slot = getSlot();
  const coerced = coerceIdentity(raw);
  if (coerced === undefined) {
    if (raw !== null && raw !== undefined) {
      debug(slot.client, `${writer}: value ${describe(raw)} is not a usable id — ${key} deleted from scope (contract §4.3 rule 4)`);
    }
    delete slot.scope[key];
    return;
  }
  if (typeof raw !== 'string') debug(slot.client, `${writer}: ${key} coerced to '${coerced}'`);
  slot.scope[key] = coerced;
}

function describe(v: unknown): string {
  if (typeof v === 'string') return `'${v}'`;
  if (typeof v === 'number' || typeof v === 'boolean' || typeof v === 'bigint') return String(v);
  if (typeof v === 'symbol') return 'a symbol';
  if (typeof v === 'function') return 'a function';
  if (Array.isArray(v)) return 'an array';
  return 'an object';
}

/** `setUser({ id }) → metadata.user_id`; `null` (or an unusable id) deletes it (contract §4.3). */
export function setUser(user: { id: string } | null): void {
  const id = user !== null && typeof user === 'object' ? (user as { id?: unknown }).id : user;
  writeIdentity('user_id', id, 'setUser');
}

/** `setTenant(tenantId) → metadata.tenant_id`; `null` (or an unusable id) deletes it (contract §4.3). */
export function setTenant(tenantId: string | null): void {
  writeIdentity('tenant_id', tenantId, 'setTenant');
}

/**
 * `setTag(key, value) → metadata[key]`; `null` DELETES the tag (never sent as `null`); a reserved name is dropped
 * with a `debug` line and no `onError`; an empty string is kept — a plain tag may legitimately be empty (B12).
 */
export function setTag(key: string, value: string | number | boolean | null): void {
  const slot = getSlot();
  const name = String(key);
  if (RESERVED_TAG_KEYS.has(name)) {
    debug(slot.client, `setTag('${name}') dropped: the key is reserved (contract §4.3 rule 2)`);
    return;
  }
  if (value === null) {
    slot.scope.tags.delete(name);
    return;
  }
  slot.scope.tags.set(name, value);
}
