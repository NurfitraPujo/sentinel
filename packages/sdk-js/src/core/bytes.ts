// Byte arithmetic for contract §4.3's bounds: every limit is in UTF-8 bytes (D4), and a cut lands on a
// code-point boundary — never a partial sequence, never a lone surrogate — so a cut string is valid UTF-8 and the
// fixture bytes §11 pins are stable across engines.

/** UTF-8 length of one code point. A lone surrogate counts as U+FFFD (3), which is what an encoder emits for it. */
function codePointBytes(cp: number): number {
  if (cp < 0x80) return 1;
  if (cp < 0x800) return 2;
  if (cp >= 0xd800 && cp <= 0xdfff) return 3;
  if (cp < 0x10000) return 3;
  return 4;
}

/** UTF-8 byte length of a string, lone surrogates counted as an encoder would emit them (3 bytes). */
export function utf8Bytes(s: string): number {
  let n = 0;
  for (const ch of s) n += codePointBytes(ch.codePointAt(0) as number);
  return n;
}

/**
 * The longest prefix of `s` that is ≤ `max` UTF-8 bytes and does not split a code point (§4.3 *Truncation
 * markers*). Returns `s` itself when it already fits, so `cut === s` says "nothing was cut".
 */
export function cutUtf8(s: string, max: number): string {
  let bytes = 0;
  let end = 0;
  for (const ch of s) {
    const size = codePointBytes(ch.codePointAt(0) as number);
    if (bytes + size > max) return s.slice(0, end);
    bytes += size;
    end += ch.length;
  }
  return s;
}
