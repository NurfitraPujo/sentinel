// Spawned by tests/transport/unref.test.ts (bundled with esbuild first): a node process with an SDK client whose
// endpoint refuses connections. `argv[2]` is the mode — `idle` inits and does nothing; `capture` captures one
// event so the first attempt fails and the ladder's backoff sleep begins — and `argv[3]` the endpoint. A ref'd
// 300 ms timer keeps the loop alive long enough for that first attempt; after it, only the SDK's timers remain,
// and they are unref()ed (contract §4.4 *Node process exit*), so the process exits without a second attempt.

import { captureMessage } from '../../src/core/capture.js';
import { initCore } from '../../src/core/init.js';

const mode = process.argv[2];
const endpoint = process.argv[3] ?? 'http://127.0.0.1:9/';

initCore({ endpoint, apiKey: 'k', debug: true, batchWaitMs: 1 }, 'node');
if (mode === 'capture') captureMessage('one event, then nothing');
setTimeout(() => {
  process.stdout.write('alive-timer fired\n');
}, 300);
