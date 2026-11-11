// Base64 VLQ, the sourcemap v3 encoding of `mappings` — shared by the two generators beside this
// file. No dependency on purpose: the fixture must build from a checkout with nothing installed.

const CHARS = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/';
const DECODE = new Map([...CHARS].map((c, i) => [c, i]));

/** Encodes one signed integer as base64 VLQ. */
export function encodeVLQ(value) {
  let vlq = value < 0 ? (-value << 1) | 1 : value << 1;
  let out = '';
  do {
    let digit = vlq & 31;
    vlq >>>= 5;
    if (vlq > 0) digit |= 32;
    out += CHARS[digit];
  } while (vlq > 0);
  return out;
}

/** Decodes one VLQ segment (e.g. "AAgBC") into its integer fields. */
export function decodeSegment(enc) {
  const fields = [];
  let shift = 0;
  let value = 0;
  for (const c of enc) {
    const digit = DECODE.get(c);
    if (digit === undefined) throw new Error(`vlq: bad character ${JSON.stringify(c)} in ${enc}`);
    value += (digit & 31) << shift;
    if (digit & 32) {
      shift += 5;
      continue;
    }
    fields.push(value & 1 ? -(value >>> 1) : value >>> 1);
    shift = 0;
    value = 0;
  }
  if (shift !== 0) throw new Error(`vlq: truncated segment ${enc}`);
  return fields;
}

/**
 * Decodes a v3 `mappings` string into lines of absolute segments:
 * { genLine, genCol, source, origLine, origCol, name } with 0-based positions and -1 for "none".
 */
export function decodeMappings(mappings) {
  const lines = [];
  let source = 0;
  let origLine = 0;
  let origCol = 0;
  let name = 0;
  mappings.split(';').forEach((line, genLine) => {
    const segs = [];
    let genCol = 0;
    if (line !== '') {
      for (const enc of line.split(',')) {
        if (enc === '') continue;
        const f = decodeSegment(enc);
        genCol += f[0];
        if (f.length === 1) {
          segs.push({ genLine, genCol, source: -1, origLine: -1, origCol: -1, name: -1 });
          continue;
        }
        source += f[1];
        origLine += f[2];
        origCol += f[3];
        let n = -1;
        if (f.length === 5) {
          name += f[4];
          n = name;
        }
        segs.push({ genLine, genCol, source, origLine, origCol, name: n });
      }
    }
    lines.push(segs);
  });
  return lines;
}
