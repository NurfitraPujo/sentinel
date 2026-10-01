// The one `globalThis[Symbol.for('@sentinel/sdk-js')]` slot (contract §4.1 *One state, however many entries*,
// §4.3): the current client, the scope and the dedup mark all live here and never in a module's own closure,
// because `.` and `./browser` bundled separately are two module instances that must see one SDK. The slot is
// created lazily on first access — reading it touches nothing but `globalThis` (import-time inertness, §11).

import type { QueuedEvent } from './queue.js';
import type { Transport } from './transport/transport.js';
import type { AutoContexts, Breadcrumb, ResolvedOptions, Runtime, SentinelEvent, User } from './types.js';

export const SDK_NAME = '@sentinel/sdk-js';

export const SLOT_KEY: unique symbol = Symbol.for(SDK_NAME);

/**
 * The client as the pipeline sees it: the resolved options, the transport of contract §4.4 when the runtime's
 * target (`endpoint` / `tunnel`) resolved — a client without one keeps the serialised events in a plain queue —
 * and the teardown hooks `close` runs.
 */
/** The browser page's current trace (spec *Trace propagation*): the trace id and the span id a captured event carries. */
export interface PageTrace {
  trace_id: string;
  span_id: string;
}

export interface InternalClient {
  readonly options: ResolvedOptions;
  readonly runtime: Runtime;
  /** `sdk.name`'s entry half — `browser`, `node`, `edge` or `nextjs` (spec §3.3): `sentinel.javascript.<sdkName>`. */
  readonly sdkName: string;
  /**
   * The runtime's automatic-context reader (spec §6), read fresh at every capture — `undefined` for a client
   * built directly through `initCore` (the test helpers), which sends none. Each guarded reader is the entry's
   * own (browser/contexts.ts, node/contexts.ts, edge.ts); a test's fixed provider replaces it (§7.3).
   */
  readonly autoContexts?: (() => AutoContexts) | undefined;
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
  /** Browser only: set by `installPageTrace`, renewed per navigation; `undefined` on the server runtimes. */
  pageTrace?: PageTrace | undefined;
}

export interface Scope {
  /** `setTag`'s tags in insertion order (the spread order, contract §4.3). Also carries `tenant_id` (§3.5 rule 5). */
  readonly tags: Map<string, string>;
  /** `setExtra` / `setExtras`' values in insertion order (spec §3.7), spread into `metadata` before `ctx.metadata`. */
  readonly extras: Map<string, unknown>;
  /** `setUser` (spec §3.4); replaced whole, never merged. */
  user?: User;
  /** `setContext` (spec §3.6), insertion order. */
  readonly contexts: Map<string, Record<string, unknown>>;
  /** The breadcrumb ring (spec §4.1), oldest first, capped at the current client's `maxBreadcrumbs`. */
  breadcrumbs: Breadcrumb[];
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
    slot = { scope: { tags: new Map(), extras: new Map(), contexts: new Map(), breadcrumbs: [] }, seen: new WeakMap(), pending: new Set() };
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
