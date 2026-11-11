// The NATS publish bound's estimator (spec §4.6): an upper bound on the `ErrorEvent` proto the ingestor would
// marshal from the event as sent — `proto.Size(mapping.MapPayloadToEvent(p))` on the Go side. `metadata`, each
// context value and each breadcrumb's `data` are `google.protobuf.Struct`, whose wire cost a JSON byte count does
// not track (a JSON number costs 11 bytes as a `Value`, 2 as JSON), so those pieces are measured with the exact
// proto3 wire formulas below and everything else is bounded by the JSON of a "skeleton" with those pieces
// emptied out — a proto string field, a `Timestamp` and a varint all cost at most what their JSON does (see the
// spec for the per-field accounting). Checked against Go's `proto.Size` on every fixture (spec §7.3); reused, not
// re-implemented, by every later size rule (C9).

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** NATS `max_payload` minus the ingestor's own additions (spec §4.6): the event stops shedding once its estimate is at or under this. */
export const NATS_PUBLISH_BUDGET = 1_000_000;

/** UTF-8 byte length by `TextEncoder` — a lone surrogate costs 3 bytes, as Go's U+FFFD replacement does. */
export function utf8Bytes(s: string): number {
  return new TextEncoder().encode(s).byteLength;
}

/** A protobuf varint's wire length. */
export function vl(n: number): number {
  return n < 2 ** 7 ? 1 : n < 2 ** 14 ? 2 : n < 2 ** 21 ? 3 : 4;
}

/** One length-delimited field: a 1-byte tag, the length varint, then `n` bytes of content. */
export function fld(n: number): number {
  return 1 + vl(n) + n;
}

/** The proto3 wire size of a `google.protobuf.Value` holding `v`. A `Value` always writes its oneof. */
export function val(v: unknown): number {
  if (v === null || typeof v === 'boolean') return 2;
  if (typeof v === 'number') return 9;
  if (typeof v === 'string') return fld(utf8Bytes(v));
  if (Array.isArray(v)) {
    let sum = 0;
    for (const element of v) sum += fld(val(element));
    return fld(sum);
  }
  return fld(structBytes(isPlainObject(v) ? v : {}));
}

/** The proto3 wire size of a `google.protobuf.Struct`'s map entries. A map entry always writes its key and value. */
export function structBytes(o: Record<string, unknown> | null | undefined): number {
  if (!isPlainObject(o)) return 0;
  let sum = 0;
  for (const key of Object.keys(o)) {
    sum += fld(fld(utf8Bytes(key)) + fld(val(o[key])));
  }
  return sum;
}

/** `e` with `metadata`, every context value and every breadcrumb's `data` emptied, and `request.data` nulled. */
function skeletonOf(e: Record<string, unknown>): unknown {
  const clone = JSON.parse(JSON.stringify(e)) as Record<string, unknown>;
  if (clone.metadata !== undefined) clone.metadata = {};
  const contexts = clone.contexts;
  if (isPlainObject(contexts)) {
    for (const key of Object.keys(contexts)) contexts[key] = {};
  }
  const breadcrumbs = clone.breadcrumbs;
  if (Array.isArray(breadcrumbs)) {
    for (const crumb of breadcrumbs) {
      if (isPlainObject(crumb) && crumb.data !== undefined) crumb.data = {};
    }
  }
  const request = clone.request;
  if (isPlainObject(request)) request.data = null;
  return clone;
}

/**
 * An upper bound on the `ErrorEvent` proto size the ingestor would marshal from `event` as sent (spec §4.6).
 * Takes a plain object rather than `SentinelEvent` because it runs on events from every revision this feature
 * lands (`tags` / `contexts` / `breadcrumbs` / `request.headers` / `request.env` are all optional here).
 */
export function protoSizeEstimate(event: Record<string, unknown>): number {
  let n = utf8Bytes(JSON.stringify(skeletonOf(event)));

  n += structBytes(isPlainObject(event.metadata) ? event.metadata : undefined);

  let entries = 0;
  const contexts = event.contexts;
  if (isPlainObject(contexts)) {
    for (const key of Object.keys(contexts)) {
      n += structBytes(isPlainObject(contexts[key]) ? (contexts[key] as Record<string, unknown>) : undefined);
      entries += 1;
    }
  }

  const breadcrumbs = event.breadcrumbs;
  if (Array.isArray(breadcrumbs)) {
    for (const crumb of breadcrumbs) {
      if (isPlainObject(crumb) && crumb.data !== undefined) {
        n += structBytes(isPlainObject(crumb.data) ? (crumb.data as Record<string, unknown>) : undefined);
      }
    }
  }

  const tags = event.tags;
  if (isPlainObject(tags)) entries += Object.keys(tags).length;

  const request = event.request;
  if (isPlainObject(request)) {
    if (request.data !== undefined) n += val(request.data);
    const headers = request.headers;
    if (isPlainObject(headers)) entries += Object.keys(headers).length;
    const env = request.env;
    if (isPlainObject(env)) entries += Object.keys(env).length;
  }

  n += 8 * entries;
  return n;
}
