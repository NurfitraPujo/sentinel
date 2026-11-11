// N6's script (contract §4.6, §11 *Node exit behaviour*) — and N16's, run under --unhandled-rejections=warn: ONE
// file, `init()` gated by SENTINEL_TEST_INIT, then an unhandled rejection with an Error reason and no other
// listener. With `init` the SDK is the sole listener: it captures (the synchronous `beforeSend` writes the event
// to SENTINEL_TEST_EVENT_FILE) and re-raises on the next tick, so node crashes as it would have — the
// source-context header names the re-raise, everything from the first `Error:` line down is the reason's own.

import { writeFileSync } from 'node:fs';
import { init } from '../../../src/node.js';

if (process.env.SENTINEL_TEST_INIT === '1') {
  init({
    endpoint: 'http://127.0.0.1:9/ingest',
    apiKey: 'test-key',
    beforeSend: (event) => {
      writeFileSync(process.env.SENTINEL_TEST_EVENT_FILE ?? '/dev/null', JSON.stringify(event));
      return event;
    },
  });
}

Promise.reject(new Error('y'));
