// `pnpm size` — the bundle-size measurement, REPORT-ONLY in v0.1 (contract §4.1, §11; decision 40).
//
// The method is pinned because the number will only ever move by decision: gzip the raw
// dist/nextjs/client.js as built — the entry file the `browser` condition of `./nextjs` resolves to, already
// minified with everything it statically imports bundled in — at zlib's DEFAULT compression level, and print
// the byte count. Not a tree-shaken consumer bundle, not a `next build` chunk, not level 9.
//
// It never fails: the threshold (D12) is set from the first real measurement by a delegated decision before
// the v0.1.0 tag. The one no-measurement outcome — the entry file does not exist yet (wave 1 of contract
// §6a lands this script before wave 2 builds the file) — is a printed note naming the file and exit 0.

import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { gzipSync } from 'node:zlib';

const entry = 'dist/nextjs/client.js';
const path = join(process.cwd(), entry);

if (!existsSync(path)) {
  console.log(`pnpm size: ${entry} does not exist — nothing measured (report-only, contract §4.1; run pnpm build first)`);
  process.exit(0);
}

const raw = readFileSync(path);
const gzipBytes = gzipSync(raw).length; // default level — the pinned method
console.log(`sdk_js_client_entry_gzip_bytes=${gzipBytes}`);
console.log(`method: gzip of the raw ${entry} as built (${raw.length} bytes minified), zlib default level — report-only in v0.1 (decision 40)`);
