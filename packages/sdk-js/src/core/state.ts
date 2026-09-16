// The one `globalThis[Symbol.for('@sentinel/sdk-js')]` slot (contract §4.1 *One state, however many entries*,
// §4.3): the current client, the scope and the dedup mark all live here and never in a module's own closure,
// because `.` and `./browser` bundled separately are two module instances that must see one SDK. The slot is
// created lazily on first access — reading it touches nothing but `globalThis` (import-time inertness, §11).

import type { ResolvedCoreOptions, Runtime, SentinelEvent } from './types.js';

export const SDK_NAME = '@sentinel/sdk-js';

export const SLOT_KEY: unique symbol = Symbol.for(SDK_NAME);

/**
 * The client as the pipeline sees it. The transport (contract §4.4) replaces `enqueue` with the ring buffer and
 * the batch worker; until then a queue of the serialised strings is what `enqueue` fills, and what a test reads.
 */
export interface InternalClient {
  readonly options: ResolvedCoreOptions;
  readonly runtime: Runtime;
  /** The serialised events in enqueue order — the string is what the transport sends (contract §4.3). */
  readonly queue: string[];
  enqueue(json: string, event: SentinelEvent): void;
}

export interface Scope {
  /** `setTag`'s tags in insertion order (the spread order, contract §4.3). */
  readonly tags: Map<string, unknown>;
  user_id?: string;
  tenant_id?: string;
}

export interface Slot {
  client?: InternalClient;
  readonly scope: Scope;
  /**
   * Decision 39's dedup mark — contract §4.3 calls it a `WeakSet`; a `WeakMap` is the same mark with the first
   * capture's `event_id` attached, which is what the second caller must be handed back.
   */
  readonly seen: WeakMap<object, string>;
}

type SlotHost = typeof globalThis & { [SLOT_KEY]?: Slot };

export function getSlot(): Slot {
  const host = globalThis as SlotHost;
  let slot = host[SLOT_KEY];
  if (slot === undefined) {
    slot = { scope: { tags: new Map() }, seen: new WeakMap() };
    host[SLOT_KEY] = slot;
  }
  return slot;
}

/** A test's `beforeEach` (contract §4.1 guidance): back to "before the first `init`". */
export function clearSlot(): void {
  delete (globalThis as SlotHost)[SLOT_KEY];
}

export function getInternalClient(): InternalClient | undefined {
  return getSlot().client;
}
