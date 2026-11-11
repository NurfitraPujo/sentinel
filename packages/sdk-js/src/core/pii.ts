// PII scrubbing — mirrors `packages/sdk-go/pii.go: ScrubPII` exactly (contract §4.3, D8): a key is sensitive
// when its lower-cased form CONTAINS any of the substrings below (so `author` and `passenger` are filtered too,
// kept for Go parity), the value becomes `"[FILTERED]"` whatever it was, and the scrub recurses over nested
// OBJECTS ONLY — arrays are not descended, because `ScrubPII` recurses `map[string]interface{}` and nothing else.

import type { JsonValue } from './serialize.js';

export const FILTERED = '[FILTERED]';

// packages/sdk-go/pii.go: defaultPIIKeys, in its order. Also the substring half of the URL filter's key
// test (spec §5.1 rule 3).
export const PII_KEYS = [
  'password',
  'passwd',
  'pass',
  'token',
  'bearer',
  'secret',
  'authorization',
  'auth',
  'credit_card',
  'card_number',
  'cvv',
  'ssn',
  'social_security',
  'api_key',
  'apikey',
];

export function isSensitiveKey(key: string): boolean {
  const lower = key.toLowerCase();
  return PII_KEYS.some((k) => lower.includes(k));
}

function isPlainObject(v: JsonValue): v is { [key: string]: JsonValue } {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** Scrub in place and return the same object. Operates on sanitised (plain JSON) data. */
export function scrubPII(metadata: { [key: string]: JsonValue }): { [key: string]: JsonValue } {
  for (const key of Object.keys(metadata)) {
    if (isSensitiveKey(key)) {
      metadata[key] = FILTERED;
      continue;
    }
    const value = metadata[key];
    if (value !== undefined && isPlainObject(value)) scrubPII(value);
  }
  return metadata;
}
