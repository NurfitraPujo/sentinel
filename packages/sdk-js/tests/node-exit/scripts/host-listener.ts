// N7's script (contract §4.6, §11 *Node exit behaviour*): the host's own `process.on('unhandledRejection')` is
// present in BOTH runs, as is a plain ref'd setTimeout that keeps the process alive past batchWaitMs. With
// `init` the SDK captures and does nothing else (the host's policy stands — no re-raise); the process continues,
// so this is the one branch where the crash event is actually delivered: the batch goes to the test's stub
// ingestor at SENTINEL_TEST_ENDPOINT (`web`'s branch — @sentry/node-core always has a listener there).

import { writeFileSync } from 'node:fs';
import { init } from '../../../src/node.js';

process.on('unhandledRejection', (reason) => {
  process.stdout.write(`host listener ran: ${reason instanceof Error ? reason.message : String(reason)}\n`);
});

if (process.env.SENTINEL_TEST_INIT === '1') {
  init({
    endpoint: process.env.SENTINEL_TEST_ENDPOINT ?? 'http://127.0.0.1:9/ingest',
    apiKey: 'test-key',
    batchWaitMs: 50,
    beforeSend: (event) => {
      writeFileSync(process.env.SENTINEL_TEST_EVENT_FILE ?? '/dev/null', JSON.stringify(event));
      return event;
    },
  });
}

Promise.reject(new Error('z'));

setTimeout(() => {
  process.stdout.write('alive-timer fired\n');
}, 1500);
