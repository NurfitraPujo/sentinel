// N5's script (contract §4.6, §11 *Node exit behaviour*): ONE file, `init()` gated by SENTINEL_TEST_INIT — two
// files would shift every file:line:col and the stderr comparison would not hold — then a top-level throw. The
// synchronous `beforeSend` writes the event to SENTINEL_TEST_EVENT_FILE: on the sole-listener path the crash
// event is never delivered (the worker's timers are unref()ed and node exits without another loop turn), so
// the file is the only channel that can carry `metadata.source` out (decision 22). Bundled by the test.

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

throw new Error('x');
