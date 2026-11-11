// The import-time inertness probe (contract §4.7, §11 *Import-time inertness*; flows D7), run by node itself —
// not through vitest's module runner, whose own transform would touch timers and `process.env` and hide the
// module body's behaviour. Usage: node probe.mjs <dist file> <mode>, mode `node` (a plain node global — no
// `window`) or `no-globals` (`globalThis.window` and `globalThis.process` deleted before the import, as the
// server / edge builds are evaluated wherever Next bundles them). Counts every `addEventListener`,
// `process.on`, `setTimeout` / `setInterval` and `fetch` call and every `process.env` key read, imports the file,
// snapshots the counts, then — `process` restored — calls `init` and snapshots again, so the test can assert
// "nothing until init, and something after". Prints one JSON object on stdout.

import { pathToFileURL } from 'node:url';

const [, , file, mode] = process.argv;
const proc = process;
const counts = { addEventListener: 0, processOn: 0, setTimeout: 0, setInterval: 0, fetch: 0, envReads: [] };
const snapshot = () => ({ ...counts, envReads: [...counts.envReads] });

const realOn = proc.on;
proc.on = function on(...args) {
  counts.processOn++;
  return realOn.apply(this, args);
};
const realSetTimeout = globalThis.setTimeout;
globalThis.setTimeout = function setTimeout(...args) {
  counts.setTimeout++;
  return realSetTimeout.apply(this, args);
};
const realSetInterval = globalThis.setInterval;
globalThis.setInterval = function setInterval(...args) {
  counts.setInterval++;
  return realSetInterval.apply(this, args);
};
const realFetch = globalThis.fetch;
globalThis.fetch = function fetch(...args) {
  counts.fetch++;
  return realFetch.apply(this, args);
};
const realAdd = EventTarget.prototype.addEventListener;
EventTarget.prototype.addEventListener = function addEventListener(...args) {
  counts.addEventListener++;
  return realAdd.apply(this, args);
};
proc.env = new Proxy(proc.env, {
  get(target, key) {
    if (typeof key === 'string') counts.envReads.push(key);
    return target[key];
  },
  has(target, key) {
    if (typeof key === 'string') counts.envReads.push(key);
    return key in target;
  },
});

// Node's own ESM loader reads `process.env` (WATCH_REPORT_DEPENDENCIES on node 20 / 22) on every import: an
// empty module imported first, inside the spies, is the loader's baseline, subtracted from the entry's reads —
// the count is the MODULE BODY's, which is what D7 is about.
await import('data:text/javascript,export {}');
const loaderBaseline = snapshot();
const minusLoader = (c) => ({
  addEventListener: c.addEventListener - loaderBaseline.addEventListener,
  processOn: c.processOn - loaderBaseline.processOn,
  setTimeout: c.setTimeout - loaderBaseline.setTimeout,
  setInterval: c.setInterval - loaderBaseline.setInterval,
  fetch: c.fetch - loaderBaseline.fetch,
  envReads: c.envReads.filter((k) => !loaderBaseline.envReads.includes(k)),
});

if (mode === 'no-globals') {
  delete globalThis.window;
  delete globalThis.process;
}
const globalsAtImport = { window: typeof globalThis.window, process: typeof globalThis.process };

let mod;
let importError = null;
try {
  mod = await import(pathToFileURL(file).href);
} catch (e) {
  importError = String(e && e.stack ? e.stack : e);
}
const afterImport = minusLoader(snapshot());

// The "until init" half: `process` back, `init` called with an absolute endpoint and a key (the server entries
// read SENTINEL_PROJECT_KEY and install their hooks; the browser entries under node have no origin and degrade).
globalThis.process = proc;
let initError = null;
let initReports = [];
if (mod && typeof mod.init === 'function') {
  try {
    mod.init({ endpoint: 'http://127.0.0.1:9/ingest', apiKey: 'probe-key', onError: (e) => initReports.push(e.code + (e.option ? ':' + e.option : '')) });
  } catch (e) {
    initError = String(e);
  }
}
const afterInit = minusLoader(snapshot());
if (mod && typeof mod.close === 'function') await mod.close(10);

proc.stdout.write(
  JSON.stringify({ file, mode, globalsAtImport, importError, exports: mod ? Object.keys(mod).sort() : [], loaderBaseline, afterImport, initError, initReports, afterInit }) + '\n',
);
