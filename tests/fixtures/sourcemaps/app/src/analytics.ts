import { postJson } from './lib/http';

interface Event {
  name: string;
  at: number;
  props: Record<string, string>;
}

const queue: Event[] = [];

export function track(name: string, props: Record<string, string> = {}): void {
  if (name.length === 0) {
    throw new RangeError('track: the event name is empty');
  }
  queue.push({ name, at: Date.now(), props });
}

export function pending(): number {
  return queue.length;
}

export async function flush(): Promise<number> {
  if (queue.length === 0) return 0;
  const batch = queue.splice(0, queue.length);
  await postJson('/api/events', batch);
  return batch.length;
}

export function pageView(path: string): void {
  track('page_view', { path });
}
