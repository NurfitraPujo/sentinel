#!/usr/bin/env node
// The cap-size map generator of sentinel-sourcemaps decision 39: a VALID v3 sourcemap whose
// `mappings` string — not `sourcesContent` — is padded to within 1 MiB below SOURCEMAP_MAX_BYTES
// (52428800), the memory-worst shape §4.4's decoder can be handed (every segment expands to a
// struct; `sourcesContent` is dropped at parse and would cost nothing). Never committed: the seed
// (apps/dashboard-web/scripts/sourcemaps-seed.sh) runs this into a temp dir and uploads the result
// beside the committed builds, so `git clone` stays small and M2/M3 still see the largest map the
// processor and the upload route admit.
//
// Deterministic by construction — a fixed-seed LCG and no clock, no randomness, no environment —
// so two runs are byte-identical and a re-upload is `unchanged` (§4.1). The README records the
// sha256 of the output as this file stands; change anything here and that digest moves.
//
//   usage: node generate-capsize-map.mjs <out file>
//
// Shape: the first generated line carries 1,000 ordinary segments (an R1 frame at line 1, any
// column ≥ 1, resolves against it — the seed's events reference this map on every event so M2
// cold-parses it); every further line carries 2,000 segments until the byte budget is met. Every
// segment is 4 or 5 fields with small deltas, the way a minifier emits them; source and name
// indexes always stay inside `sources` / `names`, so the map decodes without a map_error.
import { writeFileSync } from 'node:fs';
import { encodeVLQ } from './vlq.mjs';

export const SOURCEMAP_MAX_BYTES = 52428800; // decision 10: the upload cap (50 MiB)
export const TARGET_BYTES = SOURCEMAP_MAX_BYTES - 512 * 1024; // within 1 MiB below the cap
export const MINIFIED_PATH = '/assets/capsize-9b7f3e1a2c4d5e6f.js';

const SOURCES = Array.from({ length: 64 }, (_, i) => `../../app/src/capsize/module-${String(i).padStart(2, '0')}.ts`);
const NAMES = Array.from({ length: 256 }, (_, i) => `fn${i}`);
const FIRST_LINE_SEGMENTS = 1000;
const LINE_SEGMENTS = 2000;

function lcg(seed) {
  let s = seed >>> 0;
  return () => {
    s = (Math.imul(s, 1664525) + 1013904223) >>> 0;
    return s;
  };
}

export function generateCapsizeMap() {
  const rand = lcg(0x5e1f1e1d);
  const head = `{"version":3,"file":"capsize-9b7f3e1a2c4d5e6f.js","sources":${JSON.stringify(SOURCES)},"sourceRoot":"","names":${JSON.stringify(NAMES)},"mappings":"`;
  const tail = '"}';
  const budget = TARGET_BYTES - Buffer.byteLength(head) - Buffer.byteLength(tail);

  // Running state of the v3 encoding: the four "relative to the previous segment that carried
  // them" fields (source, origLine, origCol, name); genCol resets per line.
  let source = 0;
  let origLine = 0;
  let origCol = 0;
  let name = 0;
  const lines = [];
  let bytes = 0;
  let lineIdx = 0;
  while (bytes < budget) {
    const want = lineIdx === 0 ? FIRST_LINE_SEGMENTS : LINE_SEGMENTS;
    const segs = [];
    let lineBytes = lineIdx === 0 ? 0 : 1; // the ';' separator
    for (let i = 0; i < want; i++) {
      const r = rand();
      const dGenCol = i === 0 ? 0 : 1 + (r % 12);
      const nextSource = (r >>> 8) % 16 === 0 ? (source + 1) % SOURCES.length : source;
      const dSource = nextSource - source;
      source = nextSource;
      const dOrigLine = ((r >>> 12) % 4) - 1; // -1..2, mostly staying near the current line
      const nextOrigLine = Math.max(0, origLine + dOrigLine);
      const dOrigLineReal = nextOrigLine - origLine;
      origLine = nextOrigLine;
      const dOrigCol = ((r >>> 16) % 9) - 4;
      const nextOrigCol = Math.max(0, origCol + dOrigCol);
      const dOrigColReal = nextOrigCol - origCol;
      origCol = nextOrigCol;
      let enc = encodeVLQ(dGenCol) + encodeVLQ(dSource) + encodeVLQ(dOrigLineReal) + encodeVLQ(dOrigColReal);
      if ((r >>> 20) % 5 === 0) {
        const nextName = (name + 1 + ((r >>> 24) % 7)) % NAMES.length;
        enc += encodeVLQ(nextName - name);
        name = nextName;
      }
      segs.push(enc);
      lineBytes += enc.length + (i === 0 ? 0 : 1);
      if (bytes + lineBytes >= budget) break;
    }
    lines.push(segs.join(','));
    bytes += lineBytes;
    lineIdx++;
  }
  return head + lines.join(';') + tail;
}

if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  const out = process.argv[2];
  if (!out) {
    console.error('usage: node generate-capsize-map.mjs <out file>');
    process.exit(2);
  }
  const map = generateCapsizeMap();
  writeFileSync(out, map);
  const size = Buffer.byteLength(map);
  if (size > SOURCEMAP_MAX_BYTES || size < SOURCEMAP_MAX_BYTES - 1024 * 1024) {
    console.error(`generated ${size} bytes, outside [${SOURCEMAP_MAX_BYTES - 1024 * 1024}, ${SOURCEMAP_MAX_BYTES}]`);
    process.exit(1);
  }
  console.error(`wrote ${out}: ${size} bytes, minified_path ${MINIFIED_PATH}`);
}
