// The browser keepalive trim (spec §4.5) and the NATS publish shed (spec §4.6), run in `enqueue` on the final
// event, after `beforeSend` / `afterHook` (and, in the browser, after §4.5). Each removal is followed at once by
// its `metadata.truncated` marker and a `shrinkMetadata` re-run (spec §4.5 rule 3) — which never fails, since
// §3.7's shrink removes the exempt keys last instead of giving up — then the size is fully recomputed before the
// next comparison. Recomputing on every removal (rather than the incremental bookkeeping the spec describes as
// an optimisation) gives the identical outcome — same crumbs and contexts shed, same markers — at the cost of
// re-walking a small, bounded structure (≤ 100 crumbs, ≤ 32 contexts) an extra time or two; simpler to get right
// than to hand-roll the arithmetic, and the spec's own cost note is about avoiding quadratic work on unbounded
// input, which this still is not.

import { fld, protoSizeEstimate, structBytes, utf8Bytes, val, NATS_PUBLISH_BUDGET } from './proto-size.js';
import { shrinkMetadata, TRUNCATED_VOCABULARY, type TruncatedField } from './event.js';
import { compareCodePoints, WELL_KNOWN_CONTEXTS } from './server-bounds.js';
import { debug } from './report.js';
import type { JsonValue } from './serialize.js';
import type { InternalClient } from './state.js';
import type { SentinelEvent } from './types.js';

export const KEEPALIVE_BUDGET = 61_440;
export const TUNNEL_LAST_RESORT_BUDGET = 262_142;

type Metadata = { [key: string]: JsonValue };

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function currentTruncated(event: SentinelEvent): Set<TruncatedField> {
  const raw = (event.metadata as Record<string, unknown>).truncated;
  return new Set(Array.isArray(raw) ? raw.filter((f): f is TruncatedField => (TRUNCATED_VOCABULARY as readonly string[]).includes(f as string)) : []);
}

function currentTruncatedKeys(event: SentinelEvent): string[] {
  const raw = (event.metadata as Record<string, unknown>).truncated_keys;
  return Array.isArray(raw) ? raw.filter((p): p is string => typeof p === 'string') : [];
}

/** Add `marker` to `metadata.truncated` and re-run the normal 65 536-bound shrink (spec §4.5 rule 3; never fails). */
function markAndReshrink(event: SentinelEvent, marker: TruncatedField): void {
  const truncated = currentTruncated(event);
  truncated.add(marker);
  const truncatedKeys = currentTruncatedKeys(event);
  shrinkMetadata(event.metadata as Metadata, truncated, truncatedKeys);
}

function jsonBytes(event: SentinelEvent): number {
  return utf8Bytes(JSON.stringify(event));
}

function estimate(event: SentinelEvent): number {
  return protoSizeEstimate(event as unknown as Record<string, unknown>);
}

/**
 * The browser keepalive trim (spec §4.5): breadcrumbs shed oldest first while `bytes + 2 > 61 440`; if still
 * over with none left, sent as is, without `keepalive` (rule 4); if still over 262 142 (the tunnel's body cap),
 * `contexts` then every tag but `tenant_id` are removed as a last resort (rule 5).
 */
export function applyBrowserKeepaliveTrim(client: InternalClient, event: SentinelEvent): void {
  if (jsonBytes(event) + 2 <= KEEPALIVE_BUDGET) return;
  let removed = 0;
  while (Array.isArray(event.breadcrumbs) && event.breadcrumbs.length > 0 && jsonBytes(event) + 2 > KEEPALIVE_BUDGET) {
    event.breadcrumbs.shift();
    removed++;
    if (event.breadcrumbs.length === 0) delete event.breadcrumbs;
    markAndReshrink(event, 'breadcrumbs');
  }
  if (removed > 0) debug(client, `browser keepalive: removed ${removed} breadcrumb(s) to fit ${KEEPALIVE_BUDGET} bytes (spec §4.5)`);

  if (jsonBytes(event) > TUNNEL_LAST_RESORT_BUDGET && event.contexts !== undefined) {
    delete event.contexts;
    markAndReshrink(event, 'contexts');
    debug(client, `browser last resort: contexts removed, still over ${TUNNEL_LAST_RESORT_BUDGET} bytes (spec §4.5 rule 5)`);
  }
  if (jsonBytes(event) > TUNNEL_LAST_RESORT_BUDGET && event.tags !== undefined) {
    const tenantId = event.tags.tenant_id;
    const otherKeysRemoved = Object.keys(event.tags).some((k) => k !== 'tenant_id');
    if (tenantId === undefined) delete event.tags;
    else event.tags = { tenant_id: tenantId };
    if (otherKeysRemoved || tenantId === undefined) {
      markAndReshrink(event, 'tags');
      debug(client, `browser last resort: every tag but tenant_id removed, still over ${TUNNEL_LAST_RESORT_BUDGET} bytes (spec §4.5 rule 5)`);
    }
  }
}

/** §4.6 step 3's per-context contribution: `utf8Bytes(JSON.stringify(name)) + 4 (":{}" and a comma) + structBytes(value) + 8`. */
function contextContribution(name: string, value: unknown): number {
  return utf8Bytes(JSON.stringify(name)) + 4 + structBytes(isPlainObject(value) ? value : {}) + 8;
}

/** The next context to remove: non-well-known before well-known, largest contribution first, a size tie to the lexically smallest name. */
function pickContextVictim(contexts: Record<string, Record<string, unknown>>): string | undefined {
  let victim: string | undefined;
  let victimLast = false;
  let victimSize = -1;
  for (const name of Object.keys(contexts)) {
    const last = WELL_KNOWN_CONTEXTS.has(name);
    const size = contextContribution(name, contexts[name]);
    if (victim === undefined) {
      victim = name;
      victimLast = last;
      victimSize = size;
      continue;
    }
    if (victimLast !== last) {
      if (last) continue; // the current victim (non-well-known) still goes first
      victim = name;
      victimLast = last;
      victimSize = size;
      continue;
    }
    if (size > victimSize || (size === victimSize && compareCodePoints(name, victim) < 0)) {
      victim = name;
      victimLast = last;
      victimSize = size;
    }
  }
  return victim;
}

function shedContexts(client: InternalClient, event: SentinelEvent): void {
  let removed = 0;
  while (estimate(event) > NATS_PUBLISH_BUDGET && isPlainObject(event.contexts)) {
    const victim = pickContextVictim(event.contexts);
    if (victim === undefined) break;
    delete event.contexts[victim];
    removed++;
    if (Object.keys(event.contexts).length === 0) delete event.contexts;
    markAndReshrink(event, 'contexts');
  }
  if (removed > 0) debug(client, `NATS publish shed: removed ${removed} context(s) (spec §4.6 step 3)`);
}

function shedBreadcrumbsForPublish(client: InternalClient, event: SentinelEvent): void {
  let removed = 0;
  while (estimate(event) > NATS_PUBLISH_BUDGET && Array.isArray(event.breadcrumbs) && event.breadcrumbs.length > 0) {
    event.breadcrumbs.shift();
    removed++;
    if (event.breadcrumbs.length === 0) delete event.breadcrumbs;
    markAndReshrink(event, 'breadcrumbs');
  }
  if (removed > 0) debug(client, `NATS publish shed: removed ${removed} breadcrumb(s) (spec §4.6 step 2)`);
}

/** §4.6 step 4: `shrinkMetadata` with the estimate's own measure and a stop test against the publish budget. */
function shedMetadataForPublish(event: SentinelEvent): void {
  const metadata = event.metadata as Metadata;
  const est = estimate(event);
  const rest = est - structBytes(metadata);
  const measure = (key: string, value: JsonValue): number => fld(fld(utf8Bytes(key)) + fld(val(value)));
  const fits = (m: Metadata): boolean => rest + structBytes(m) <= NATS_PUBLISH_BUDGET;
  const truncated = currentTruncated(event);
  const truncatedKeys = currentTruncatedKeys(event);
  shrinkMetadata(metadata, truncated, truncatedKeys, measure, fits);
}

/**
 * The NATS publish bound (spec §4.6), on every runtime: breadcrumbs, then contexts, then metadata, each shed
 * only while `estimate(event)` is over 1 000 000. If still over once metadata has no victim left, the
 * event is sent as it stands (step 5) — the estimate is an upper bound, so dropping it here could drop one the
 * ingestor would have published.
 */
export function applyPublishShed(client: InternalClient, event: SentinelEvent): void {
  if (estimate(event) <= NATS_PUBLISH_BUDGET) return;
  shedBreadcrumbsForPublish(client, event);
  if (estimate(event) > NATS_PUBLISH_BUDGET) shedContexts(client, event);
  if (estimate(event) > NATS_PUBLISH_BUDGET) shedMetadataForPublish(event);
  const finalEstimate = estimate(event);
  if (finalEstimate > NATS_PUBLISH_BUDGET) {
    debug(client, `NATS publish estimate ${finalEstimate} is over ${NATS_PUBLISH_BUDGET}; the event is sent as it stands (spec §4.6 step 5)`);
  }
}
