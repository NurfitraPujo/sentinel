// The one `globalThis[Symbol.for('@sentinel/sdk-js')]` slot (contract §4.1 *One state, however many entries*,
// §4.3): the current client, the scope and the dedup mark all live here and never in a module's own closure,
// because `.` and `./browser` bundled separately are two module instances that must see one SDK. The slot is
// created lazily on first access — reading it touches nothing but `globalThis` (import-time inertness, §11).

import type { QueuedEvent } from './queue.js';
import type { Transport } from './transport/transport.js';
import type { ResolvedOptions, Runtime, SentinelEvent } from './types.js';

export const SDK_NAME = '@sentinel/sdk-js';

export const SLOT_KEY: unique symbol = Symbol.for(SDK_NAME);

/**
 * The client as the pipeline sees it: the resolved options, the transport of contract §4.4 when the runtime's
 * target (`endpoint` / `tunnel`) resolved — a client without one keeps the serialised events in a plain queue —
 * and the teardown hooks `close` runs.
 */
export interface InternalClient {
  readonly options: ResolvedOptions;
  readonly runtime: Runtime;
  /**
   * The no-op client of contract §4.2 (`not_configured`, or an unusable `endpoint` / `tunnel`): every capture is
   * accepted and dropped, nothing is ever posted, no handler is installed.
   */
  readonly noop: boolean;
  readonly transport: Transport | undefined;
  /** The not-yet-sent serialised events in enqueue order — the string is what the transport sends (contract §4.3). */
  readonly queue: readonly string[];
  /** Serialise-once enqueue from the pipeline (contract §4.3): the string beside the event's `event_id`. */
  enqueue(json: string, event: SentinelEvent): void;
  /** An already-serialised event — the second-`init` drain (contract §4.2). */
  push(event: QueuedEvent): void;
  /**
   * Run by `close` (contract §4.3) and when a second `init` replaces the client — before the new client's
   * handlers are installed: the hide listeners' removal (client.ts) and the global handlers' uninstall
   * (handlers/browser.ts, handlers/node.ts). Each is called once and may not throw outward.
   */
  readonly teardown: Array<() => void>;
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
   * The async `beforeSend` hooks still pending (contract §4.2): each enqueues into the client current when it
   * settles, and `flush` waits for them up to its timeout (B9). Module-level, like the scope — a second `init`
   * in between does not lose them.
   */
  readonly pending: Set<Promise<void>>;
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
    slot = { scope: { tags: new Map() }, seen: new WeakMap(), pending: new Set() };
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
