// `Retry-After` (contract §4.4 *Retry*): honoured in the delay-seconds and HTTP-date forms and capped at 60 s —
// `packages/sdk-go/transport.go: parseRetryAfter`, ported. The three degenerate forms are one rule: a value that
// does not parse (`soon`, an empty header), one that is ≤ 0 (`0`, `-5`) and an HTTP-date in the past are all
// IGNORED — the caller falls back to the ladder's delay for that attempt — never an immediate resend and never a
// forced 60 s; the cap is what an over-cap value (`120`, a date an hour out) gets.

export const MAX_RETRY_AFTER_MS = 60_000;

/** Milliseconds to wait, capped at 60 s, or `undefined` when the header is absent or degenerate. */
export function parseRetryAfter(header: string | null | undefined, now: number = Date.now()): number | undefined {
  if (header === null || header === undefined) return undefined;
  const value = header.trim();
  if (value === '') return undefined;

  // delay-seconds: a non-negative integer (RFC 9110 §10.2.3); `strconv.Atoi` also admits a sign, so `-5` parses
  // and is then refused as ≤ 0 rather than tried as a date.
  if (/^[+-]?\d+$/.test(value)) {
    const secs = Number(value);
    if (!Number.isFinite(secs) || secs <= 0) return undefined;
    return Math.min(secs * 1000, MAX_RETRY_AFTER_MS);
  }

  const when = Date.parse(value);
  if (Number.isNaN(when)) return undefined;
  const ms = when - now;
  if (ms <= 0) return undefined;
  return Math.min(ms, MAX_RETRY_AFTER_MS);
}
