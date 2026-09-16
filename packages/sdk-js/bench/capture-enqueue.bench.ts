// `sdk_js_capture_enqueue_p95_us` (contract §13): the p95 of one `captureException(new Error(…))` call — building
// the event (§4.3: stack parsing, the bounds, the PII scrub over a 20-key metadata, the safe serialisation) and
// enqueuing it into the ring buffer — over 10 000 warm calls on node 22, each timed here with `performance.now()`
// around the call (vitest's reporter prints tinybench's own p75 / p99 beside it; it has no p95 and strips the
// samples before the teardown hook sees them). The transport's `fetch` is stubbed to a `202` so nothing leaves
// the process; the worker's sends are not in the measured window (the window is one synchronous
// `captureException` call).
//
// Reported, no threshold in v0.1 (a threshold is a decision once a number exists — contract §13). Run it with
// `pnpm bench`; it is not a CI step.

import { bench } from 'vitest';
import { captureException, close, init } from '../src/node.js';

const ITERATIONS = 10_000;
const STACK_FRAMES = 30;
const METADATA_KEYS = 20;

/** A 30-frame stack: the throw site is `STACK_FRAMES` calls deep and `Error.stackTraceLimit` caps it there. */
function deep(n: number): Error {
  return n === 0 ? new Error('bench: captureException enqueue') : deep(n - 1);
}

/** 20 keys, four of them PII by the substring rule (D8) so the scrub has work to do on every call. */
function metadata(i: number): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const names = ['password', 'authorization', 'api_token', 'session_secret'];
  for (let k = 0; k < METADATA_KEYS; k++) {
    const name = k < names.length ? names[k] : `key_${k}`;
    out[name as string] = `value-${i}-${k}-${'x'.repeat(24)}`;
  }
  return out;
}

function percentile(samplesMs: readonly number[], p: number): number {
  const sorted = [...samplesMs].sort((a, b) => a - b);
  const idx = Math.min(sorted.length - 1, Math.max(0, Math.ceil((p / 100) * sorted.length) - 1));
  return sorted[idx] as number;
}

let i = 0;
/** One entry per call of the current cycle (warmup or run), in ms — reset by `setup`. */
let timings: number[] = [];

bench(
  `captureException(new Error) — ${STACK_FRAMES}-frame stack, ${METADATA_KEYS}-key metadata, ${ITERATIONS} calls`,
  () => {
    const meta = metadata(i++);
    const t0 = performance.now();
    captureException(deep(STACK_FRAMES - 1), { metadata: meta });
    timings.push(performance.now() - t0);
  },
  {
    iterations: ITERATIONS,
    warmupIterations: 1_000,
    time: 0,
    warmupTime: 0,
    setup(_task, mode) {
      Error.stackTraceLimit = STACK_FRAMES;
      globalThis.fetch = (async () =>
        new Response(JSON.stringify({ ingested: 10, failed: 0 }), { status: 202, headers: { 'content-type': 'application/json' } })) as typeof fetch;
      init({
        endpoint: 'http://127.0.0.1:9/ingest',
        apiKey: 'bench-key',
        release: '0.0.0-bench',
        environment: 'bench',
        installGlobalHandlers: false,
        maxBufferSize: 100,
      });
      i = 0;
      timings = [];
      if (mode === 'run') console.log(`bench: node ${process.version}, ${ITERATIONS} iterations after 1 000 warmup`);
    },
    async teardown(task, mode) {
      await close(1_000);
      if (mode !== 'run' || task.result === undefined) return;
      const samples = timings;
      const p95Us = percentile(samples, 95) * 1_000;
      console.log(
        `sdk_js_capture_enqueue_p95_us=${p95Us.toFixed(1)} (n=${samples.length}, p50=${(percentile(samples, 50) * 1_000).toFixed(1)} us, ` +
          `mean=${(task.result.mean * 1_000).toFixed(1)} us, max=${(task.result.max * 1_000).toFixed(1)} us; ` +
          `${STACK_FRAMES}-frame stack, ${METADATA_KEYS}-key metadata, node ${process.version}, performance.now() around each call)`,
      );
    },
  },
);
