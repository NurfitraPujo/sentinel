// The safe serialisation of contract §4.3 *Nothing in captureException / captureMessage throws* and the one
// counting function beside it (*The 64 KiB shrink*, step 1).
//
// `sanitize` turns any value into plain JSON data the way `JSON.stringify` would — `toJSON` honoured, functions
// and `undefined` omitted from objects and `null` in arrays, non-finite numbers `null` — except that nothing
// throws: a cycle becomes `"[Circular]"`, a `BigInt` its decimal string, a throwing `toJSON` / getter or a symbol
// `"[unserializable]"`, and every substitution is recorded as the dotted path of the key it replaced (`req.socket`;
// an array element by its numeric index, `items.0.socket`; keys verbatim). The result is what the byte counts
// are taken on and what the wire carries.

import { utf8Bytes } from './bytes.js';

export const CIRCULAR = '[Circular]';
export const UNSERIALIZABLE = '[unserializable]';

export type JsonValue = string | number | boolean | null | JsonValue[] | { [key: string]: JsonValue };

export interface Sanitized {
  value: JsonValue | undefined;
  /** Dotted paths, in traversal order, of every value that was substituted. */
  substituted: string[];
}

function pathOf(segments: (string | number)[]): string {
  return segments.join('.');
}

interface Walk {
  substituted: string[];
  ancestors: Set<object>;
}

function walk(value: unknown, path: (string | number)[], w: Walk, skipToJSON: boolean): JsonValue | undefined {
  // The type dispatch is an if-chain ON PURPOSE. It was a `switch (typeof value)` whose `case 'object': break;`
  // fell through to the object handling below, and SWC's minifier (Next 15.5.21's bundled one and @swc/core
  // 1.13.5 alike) merges that `break` case with the `default: return undefined` arm and then drops everything
  // after the switch as unreachable — so in every Next production bundle (Turbopack and webpack, browser, node
  // and edge) `sanitize(event)` returned `undefined`, every event serialised to `null` and the tunnel / ingestor
  // rejected each batch as `[null]`. tests/minified/swc.test.ts minifies the built entries with @swc/core and
  // sends through them; keep this shape (and any new dispatch) free of a `break`-then-fall-out `switch`.
  const kind = typeof value;
  if (kind === 'string' || kind === 'boolean') return value as string | boolean;
  if (kind === 'number') return Number.isFinite(value as number) ? (value as number) : null;
  if (kind === 'bigint') {
    w.substituted.push(pathOf(path));
    return (value as bigint).toString();
  }
  if (kind === 'symbol') {
    w.substituted.push(pathOf(path));
    return UNSERIALIZABLE;
  }
  if (kind !== 'object') return undefined; // undefined, function
  if (value === null) return null;
  const obj = value as object;
  if (w.ancestors.has(obj)) {
    w.substituted.push(pathOf(path));
    return CIRCULAR;
  }
  if (!skipToJSON) {
    let toJSON: unknown;
    try {
      toJSON = (obj as { toJSON?: unknown }).toJSON;
    } catch {
      w.substituted.push(pathOf(path));
      return UNSERIALIZABLE;
    }
    if (typeof toJSON === 'function') {
      let replaced: unknown;
      try {
        replaced = (toJSON as (this: object, key: string) => unknown).call(obj, String(path[path.length - 1] ?? ''));
      } catch {
        w.substituted.push(pathOf(path));
        return UNSERIALIZABLE;
      }
      // JSON.stringify serialises toJSON's result without asking it for a toJSON of its own.
      return walk(replaced, path, w, true);
    }
  }
  w.ancestors.add(obj);
  try {
    if (Array.isArray(obj)) {
      const out: JsonValue[] = [];
      for (let i = 0; i < obj.length; i++) {
        let element: unknown;
        try {
          element = obj[i];
        } catch {
          w.substituted.push(pathOf([...path, i]));
          out.push(UNSERIALIZABLE);
          continue;
        }
        const v = walk(element, [...path, i], w, false);
        out.push(v === undefined ? null : v);
      }
      return out;
    }
    const out: { [key: string]: JsonValue } = {};
    let keys: string[];
    try {
      keys = Object.keys(obj);
    } catch {
      w.substituted.push(pathOf(path));
      return UNSERIALIZABLE;
    }
    for (const key of keys) {
      let member: unknown;
      try {
        member = (obj as Record<string, unknown>)[key];
      } catch {
        w.substituted.push(pathOf([...path, key]));
        out[key] = UNSERIALIZABLE;
        continue;
      }
      const v = walk(member, [...path, key], w, false);
      if (v !== undefined) out[key] = v;
    }
    return out;
  } finally {
    w.ancestors.delete(obj);
  }
}

/** Sanitise `value`, recording substitution paths relative to `prefix` (the key the value sits under). */
export function sanitize(value: unknown, prefix: (string | number)[] = []): Sanitized {
  const w: Walk = { substituted: [], ancestors: new Set() };
  const out = walk(value, prefix, w, false);
  return { value: out, substituted: w.substituted };
}

/** `JSON.stringify` of the sanitised value — never throws, and `undefined` (a bare function) serialises as `null`. */
export function safeStringify(value: unknown): string {
  const { value: plain } = sanitize(value);
  return JSON.stringify(plain === undefined ? null : plain);
}

/**
 * The ruler of the 64 KiB shrink (contract §4.3 step 1): the UTF-8 length of a JSON string as the ingestor's
 * validator will count it after re-marshalling — `validator.go: ValidatePayload` measures
 * `len(json.Marshal(payload.Metadata))`, and Go's encoder escapes `<`, `>` and `&` as `\u003c` (6 bytes for a
 * 1-byte character, +5) and U+2028 / U+2029 as `\u2028` (6 for a 3-byte one, +3) where `JSON.stringify` leaves
 * them literal. Nothing else differs on Go 1.25's encoder. The three characters cannot occur outside a JSON
 * string, so a scan of the serialised text needs no parsing. The wire bytes are unchanged — this only counts.
 */
export function metadataWireBytes(json: string): number {
  let extra = 0;
  for (let i = 0; i < json.length; i++) {
    const c = json.charCodeAt(i);
    if (c === 0x3c || c === 0x3e || c === 0x26) extra += 5;
    else if (c === 0x2028 || c === 0x2029) extra += 3;
  }
  return utf8Bytes(json) + extra;
}
