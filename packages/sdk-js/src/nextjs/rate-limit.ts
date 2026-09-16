// The tunnel route's token bucket (contract §4.5 step 4, decision 19): one in-memory bucket per client key,
// capacity `perMinute`, refilling `perMinute / 60` tokens per second (one per second at the default 60), a
// refused request answered with `Retry-After: ceil(seconds until the next token)` — `1` for a one-token wait,
// never `60`. Per handler instance, never shared across replicas.
//
// The store is bounded — a contract rule, not guidance: at most `MAX_KEYS` (10 000) buckets; a new key past
// that evicts the least recently seen one (the `Map` is kept in last-seen order); and a sweep removes every
// bucket that has been full for ≥ `FULL_IDLE_MS` (60 s) — an idle client costs nothing to forget, because a
// fresh bucket starts full. Without the bound a spoofed-first-hop flood grows the map until the pod is
// OOM-killed (T17). The sweep runs on insert, at most once per second of the clock, so a flood of inserts
// within one second pays for one pass over the map, not ten thousand.

export const MAX_KEYS = 10_000;
export const FULL_IDLE_MS = 60_000;
const SWEEP_EVERY_MS = 1000;

/** The shared bucket's key (contract §4.5 step 4's last fallback): never a value `clientKey` or a header can produce. */
/**
 * The store behind a handler's `POST`, for the suite that asserts the bound (T17): the factory returns `{ POST }`
 * and nothing else (contract §4.5), so the store is reachable only through this side table, never on the
 * public object. Weak: a discarded handler takes its store with it.
 */
export const storeOf = new WeakMap<object, TokenBucketStore>();

export const SHARED_KEY: unique symbol = Symbol('@sentinel/sdk-js shared bucket');

export type BucketKey = string | typeof SHARED_KEY;

interface Bucket {
  tokens: number;
  /** Clock reading of the last refill computation (ms). */
  at: number;
}

export type TakeResult = { ok: true } | { ok: false; retryAfterSeconds: number };

export class TokenBucketStore {
  private readonly buckets = new Map<BucketKey, Bucket>();
  private readonly capacity: number;
  /** Tokens per millisecond. */
  private readonly rate: number;
  private lastSweep = Number.NEGATIVE_INFINITY;

  constructor(
    perMinute: number,
    private readonly now: () => number = () => Date.now(),
  ) {
    this.capacity = perMinute;
    this.rate = perMinute / 60_000;
  }

  get size(): number {
    return this.buckets.size;
  }

  has(key: BucketKey): boolean {
    return this.buckets.has(key);
  }

  /** Spend one token of `key`'s bucket, or say how long until the next one. */
  take(key: BucketKey): TakeResult {
    const now = this.now();
    let bucket = this.buckets.get(key);
    if (bucket === undefined) {
      this.sweep(now);
      if (this.buckets.size >= MAX_KEYS) {
        // Least recently seen first: a `Map` iterates in insertion order and every touch re-inserts.
        const oldest = this.buckets.keys().next();
        if (!oldest.done) this.buckets.delete(oldest.value);
      }
      bucket = { tokens: this.capacity, at: now };
    } else {
      this.buckets.delete(key);
      this.refill(bucket, now);
    }
    this.buckets.set(key, bucket);
    if (bucket.tokens >= 1) {
      bucket.tokens -= 1;
      return { ok: true };
    }
    const waitMs = (1 - bucket.tokens) / this.rate;
    return { ok: false, retryAfterSeconds: Math.max(1, Math.ceil(waitMs / 1000)) };
  }

  private refill(bucket: Bucket, now: number): void {
    const elapsed = Math.max(0, now - bucket.at);
    bucket.tokens = Math.min(this.capacity, bucket.tokens + elapsed * this.rate);
    bucket.at = now;
  }

  /** Remove every bucket that has been full for ≥ 60 s; at most once per second of the clock. */
  sweep(now: number, force = false): void {
    if (!force && now - this.lastSweep < SWEEP_EVERY_MS) return;
    this.lastSweep = now;
    for (const [key, bucket] of this.buckets) {
      // When the bucket (re)filled: `at` plus the time its deficit took to refill.
      const fullAt = bucket.at + (this.capacity - bucket.tokens) / this.rate;
      if (now - fullAt >= FULL_IDLE_MS) this.buckets.delete(key);
    }
  }
}
