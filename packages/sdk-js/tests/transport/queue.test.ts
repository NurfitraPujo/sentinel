// The ring buffer and the batch packing of contract §4.4 *Queue* / *Never blocks the caller* (unit): overflow
// drops the oldest and counts it for the next dequeue (B15), a batch is packed to ≤ 60 KiB by the enqueue-time
// JSON string's UTF-8 bytes, and one oversize event goes alone (B24). Order-independent: no shared state.

import { describe, expect, it } from 'vitest';
import { batchBytes, MAX_BATCH_BYTES, queuedEvent, RingBuffer } from '../../src/core/queue.js';

function event(id: string, bytes: number) {
  // `{"event_id":"<id>","p":"<pad>"}` — padded to exactly `bytes` UTF-8 bytes.
  const head = `{"event_id":"${id}","p":"`;
  const tail = '"}';
  return queuedEvent(id, head + 'x'.repeat(bytes - head.length - tail.length) + tail);
}

describe('B15 (unit): the ring buffer drops the oldest on overflow and reports the count once, on the next dequeue', () => {
  it('B15: 150 pushes into a buffer of 100 keep the newest 100; takeDropped() is 50 once, then 0', () => {
    const q = new RingBuffer(100);
    for (let i = 0; i < 150; i++) q.push(event(`e${i}`, 100));
    expect(q.size).toBe(100);
    expect(q.snapshot()[0]?.id).toBe('e50');
    expect(q.snapshot()[99]?.id).toBe('e149');
    expect(q.takeDropped()).toBe(50);
    expect(q.takeDropped()).toBe(0);
  });

  it('B15: takeBatch(10) removes the head 10 — 90 remain queued, and the batch no longer counts toward the capacity', () => {
    const q = new RingBuffer(100);
    for (let i = 0; i < 100; i++) q.push(event(`e${i}`, 100));
    const batch = q.takeBatch(10);
    expect(batch.map((e) => e.id)).toEqual(Array.from({ length: 10 }, (_, i) => `e${i}`));
    expect(q.size).toBe(90);
    q.push(event('fresh', 100));
    expect(q.takeDropped()).toBe(0);
    expect(q.size).toBe(91);
  });
});

describe('the ≤ 60 KiB batch packing by the enqueue-time serialisation (contract §4.4)', () => {
  it('counts the body as [ + strings joined by , + ] in UTF-8 bytes — a multi-byte event by its bytes, not its length', () => {
    const e = queuedEvent('m', '{"event_id":"m","s":"é€😀"}');
    expect(e.bytes).toBe(Buffer.byteLength(e.json));
    expect(e.json.length).not.toBe(e.bytes);
    expect(batchBytes([e, e])).toBe(2 + e.bytes * 2 + 1);
  });

  it('an event that would push the batch over 60 KiB starts the next one; the first event is always taken', () => {
    const q = new RingBuffer(100);
    // Ten 6 KiB events: nine fit (9 × 6144 + 8 + 2 = 55 306), the tenth would make 61 451 > 61 440.
    for (let i = 0; i < 10; i++) q.push(event(`e${i}`, 6 * 1024));
    const first = q.takeBatch(10);
    expect(first).toHaveLength(9);
    expect(batchBytes(first)).toBeLessThanOrEqual(MAX_BATCH_BYTES);
    expect(batchBytes([...first, event('e9', 6 * 1024)])).toBeGreaterThan(MAX_BATCH_BYTES);
    expect(q.takeBatch(10).map((e) => e.id)).toEqual(['e9']);
  });

  it('B24 (unit): an event over 60 KiB by itself is taken alone, ahead of and behind small ones', () => {
    const q = new RingBuffer(100);
    q.push(event('small-1', 300));
    q.push(event('huge', 120 * 1024));
    q.push(event('small-2', 300));
    expect(q.takeBatch(10).map((e) => e.id)).toEqual(['small-1']);
    const alone = q.takeBatch(10);
    expect(alone.map((e) => e.id)).toEqual(['huge']);
    expect(batchBytes(alone)).toBeGreaterThan(MAX_BATCH_BYTES);
    expect(q.takeBatch(10).map((e) => e.id)).toEqual(['small-2']);
    expect(q.takeBatch(10)).toEqual([]);
  });

  it('batchSize caps the count before the bytes do', () => {
    const q = new RingBuffer(100);
    for (let i = 0; i < 30; i++) q.push(event(`e${i}`, 100));
    expect(q.takeBatch(10)).toHaveLength(10);
    expect(q.takeBatch(50)).toHaveLength(20);
  });
});
