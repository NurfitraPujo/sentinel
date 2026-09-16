// The ring buffer of contract §4.4 *Queue*: a bounded FIFO of `maxBufferSize` serialised events. On overflow the
// OLDEST is dropped (spec §6 rule 2, `packages/sdk-go/transport.go: Push`) and a counter increments; the worker
// reads and resets that counter at its next dequeue, which is where the one `buffer_overflow` report comes from
// (B15). The buffer holds the JSON string the event was serialised to once, at enqueue, beside its `event_id`
// and its UTF-8 byte length — the worker packs by those bytes and never re-serialises a live object.

import { utf8Bytes } from './bytes.js';

export interface QueuedEvent {
  readonly id: string;
  /** The event's own serialisation — what the wire carries, verbatim. */
  readonly json: string;
  /** `utf8Bytes(json)`, computed once. */
  readonly bytes: number;
}

/** The keepalive body cap the worker packs under (contract §4.4 *Never blocks the caller*): ≤ 60 KiB serialised. */
export const MAX_BATCH_BYTES = 60 * 1024;

export function queuedEvent(id: string, json: string): QueuedEvent {
  return { id, json, bytes: utf8Bytes(json) };
}

/** The body of a batch is `[` + the strings joined by `,` + `]` — always an array, even for one (contract §4.4). */
export function batchBytes(events: readonly QueuedEvent[]): number {
  let n = 2 + Math.max(0, events.length - 1);
  for (const e of events) n += e.bytes;
  return n;
}

export class RingBuffer {
  private items: QueuedEvent[] = [];
  /** Events dropped by overflow since the counter was last read (contract §4.4 *Queue*). */
  private droppedSinceRead = 0;

  constructor(readonly capacity: number) {}

  get size(): number {
    return this.items.length;
  }

  /** O(1) amortised, synchronous, never throws: a full buffer sheds its oldest event first. */
  push(event: QueuedEvent): void {
    if (this.items.length >= this.capacity) {
      this.items.shift();
      this.droppedSinceRead++;
    }
    this.items.push(event);
  }

  /** The overflow count since the last call, and the counter reset — read once per dequeue (B15). */
  takeDropped(): number {
    const n = this.droppedSinceRead;
    this.droppedSinceRead = 0;
    return n;
  }

  /**
   * Remove and return the head batch: up to `maxItems` events whose body stays ≤ `maxBytes`. The first event is
   * always taken — one event can itself exceed the cap and cannot be split (contract §4.4: it goes as its own
   * batch, without `keepalive`); an event that would push the batch over starts the next one.
   */
  takeBatch(maxItems: number, maxBytes: number = MAX_BATCH_BYTES): QueuedEvent[] {
    if (this.items.length === 0) return [];
    let count = 1;
    let bytes = 2 + (this.items[0] as QueuedEvent).bytes;
    while (count < maxItems && count < this.items.length) {
      const next = (this.items[count] as QueuedEvent).bytes + 1;
      if (bytes + next > maxBytes) break;
      bytes += next;
      count++;
    }
    return this.items.splice(0, count);
  }

  /** Everything still queued, in order, and the buffer emptied — the second-`init` drain (contract §4.2). */
  drain(): QueuedEvent[] {
    const out = this.items;
    this.items = [];
    return out;
  }

  /** A read-only view, in enqueue order. */
  snapshot(): readonly QueuedEvent[] {
    return this.items;
  }
}
