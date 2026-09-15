#!/usr/bin/env node
// The event generator behind apps/dashboard-web/scripts/sourcemaps-seed.sh (sentinel-sourcemaps
// contract §13, be.md task 11). It derives every event from the COMMITTED builds beside it, so a
// frame's (file, line, column) always sits on a real segment of a real map and the seed is about
// the pipeline, not about hand-typed positions that happen to miss:
//
//   events-a/NNN.json     1,000 events for build A under release R1  (10 frames each, ≥ 5 chunks)
//   events-b/NNN.json     the SAME 1,000 events for build B under R1 — the same source positions,
//                         build B's chunk names and generated columns (flows S1/S2)
//   events-r0a/NNN.json   build A's events again under R0, which has no maps (flow S5)
//   events-r0b/NNN.json   build B's events again under R0
//   partial.json          one extra R1 event with a no-column frame and a frame on an unmapped
//                         chunk, so a partially symbolicated occurrence exists (fe's D2 shot)
//   touch-small.json      one R1 event with one frame on every COMMITTED map (both builds, 16
//                         frames) — M2's cold-fetch event for the small maps
//   touch-capsize.json    one R1 event with one frame on the cap-size map alone — M2's second
//                         event, so the RSS delta of that map is read on its own
//   manifest.json         N, the anchors, the triples, every output's sha256
//
// Each events-* directory holds the 1,000 events in batches of 50 (NNN = 000..019), one
// /ingest/batch body each: the ingestor caps a batch body at 5 MiB and a batch at 500 items.
//
// How N is fixed. An ANCHOR is one top-level function of the app: the segment of build A's map
// whose `name` is that function, the same source position found again in build B's map, and the
// minified identifier the generated code carries at each — a browser's raw `function`. A TRIPLE
// is an ordered choice of three distinct anchors; the first three frames of every event are its
// triple with in_app true, so `fingerprint.Compute` (MaxAppFrames = 3) hashes exactly
// (file, function) × 3 of it and nothing else. N = the number of distinct triples = the number of
// distinct fingerprints the 1,000 events must produce — with maps (source path + original name)
// and without (normalised chunk name + minified name), which is only true when build B's minified
// identifier equals build A's for every anchor: this generator refuses to run when it does not.
//
// Deterministic: a fixed-seed LCG, no clock, no environment. No event_id is set, so the ingestor
// mints one per posting and a re-run of the seed adds occurrences without adding issues.
//
//   usage: node generate-events.mjs <out dir> [project_key]
//
// project_key (default `sourcemaps-seed`) is the NAME of the seed's project, the wire field the
// ingestor requires on every event; the credential, never this field, decides the tenant.
import { readdirSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { decodeMappings } from './vlq.mjs';
import { MINIFIED_PATH as CAPSIZE_PATH } from './generate-capsize-map.mjs';

const here = dirname(fileURLToPath(import.meta.url));

export const RELEASE_R1 = 'R1';
export const RELEASE_R0 = 'R0';
export const PLATFORM = 'javascript';
export const ENVIRONMENT = 'production';
export const EVENTS = 1000;
export const BATCH = 50;
export const TRIPLES = 60;
export const UNMAPPED_PATH = '/assets/legacy-00ffee11.js';
export const DEFAULT_PROJECT_KEY = 'sourcemaps-seed';

const ERROR_CLASSES = ['TypeError', 'RangeError', 'Error', 'SyntaxError'];

function lcg(seed) {
  let s = seed >>> 0;
  return () => {
    s = (Math.imul(s, 1664525) + 1013904223) >>> 0;
    return s;
  };
}

/** The app's top-level function names, read from the committed source. */
function functionNames() {
  const names = new Set();
  const walk = (dir) => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const p = join(dir, entry.name);
      if (entry.isDirectory()) walk(p);
      else if (entry.name.endsWith('.ts')) {
        for (const m of readFileSync(p, 'utf8').matchAll(/^(?:export )?(?:async )?function (\w+)/gm)) names.add(m[1]);
      }
    }
  };
  walk(join(here, 'app', 'src'));
  return names;
}

/** Reads one build: every map decoded, with the generated identifier at each named segment. */
export function readBuild(dist) {
  const assets = join(here, dist, 'assets');
  const maps = [];
  for (const file of readdirSync(assets).sort()) {
    if (!file.endsWith('.js.map')) continue;
    const raw = readFileSync(join(assets, file));
    const map = JSON.parse(raw);
    const js = readFileSync(join(assets, file.slice(0, -4)), 'utf8').split('\n');
    const lines = decodeMappings(map.mappings);
    const segments = [];
    for (const segs of lines) {
      for (const s of segs) {
        if (s.source < 0) continue;
        const text = js[s.genLine] ?? '';
        const m = /^[A-Za-z_$][\w$]*/.exec(text.slice(s.genCol));
        segments.push({
          ...s,
          sourcePath: map.sources[s.source],
          name: s.name >= 0 ? map.names[s.name] : null,
          token: m ? m[0] : null,
        });
      }
    }
    maps.push({
      file: file.slice(0, -4),
      minifiedPath: `/assets/${file.slice(0, -4)}`,
      mapFile: file,
      bytes: raw.length,
      sha256: createHash('sha256').update(raw).digest('hex'),
      segments,
    });
  }
  return maps;
}

/** One anchor per (source, function name) present in BOTH builds with the same minified token. */
function anchors(buildA, buildB) {
  const fns = functionNames();
  const byPosB = new Map();
  for (const m of buildB) {
    for (const s of m.segments) {
      if (s.name && fns.has(s.name)) {
        const key = `${s.sourcePath}|${s.origLine}|${s.origCol}|${s.name}`;
        if (!byPosB.has(key)) byPosB.set(key, { map: m, seg: s });
      }
    }
  }
  const out = [];
  const seen = new Set();
  const mismatches = [];
  for (const m of buildA) {
    for (const s of m.segments) {
      if (!s.name || !fns.has(s.name) || !s.token) continue;
      const id = `${s.sourcePath}|${s.name}`;
      if (seen.has(id)) continue;
      const b = byPosB.get(`${s.sourcePath}|${s.origLine}|${s.origCol}|${s.name}`);
      if (!b || !b.seg.token) continue;
      if (b.seg.token !== s.token) {
        mismatches.push(`${id}: build A ${s.token} vs build B ${b.seg.token}`);
        continue;
      }
      seen.add(id);
      out.push({
        source: s.sourcePath,
        name: s.name,
        a: { path: m.minifiedPath, line: s.genLine + 1, column: s.genCol + 1, token: s.token },
        b: { path: b.map.minifiedPath, line: b.seg.genLine + 1, column: b.seg.genCol + 1, token: b.seg.token },
        original: { line: s.origLine + 1, column: s.origCol + 1 },
      });
    }
  }
  if (mismatches.length > 0) {
    throw new Error(`anchors whose minified identifier differs between builds (S5 cannot hold):\n  ${mismatches.join('\n  ')}`);
  }
  // Without maps the identity is (normalised chunk name, minified identifier); two anchors that
  // share both would collapse into one fingerprint and |A| would fall short of N on the R0 run.
  const raw = new Map();
  for (const a of out) {
    const key = `${a.a.path.replace(/-[0-9a-f]+(_b2)?\.js$/, '-*.js')}:${a.a.token}`;
    if (raw.has(key)) throw new Error(`anchors ${raw.get(key)} and ${a.source}:${a.name} share the raw identity ${key}`);
    raw.set(key, `${a.source}:${a.name}`);
  }
  return out;
}

function frame(anchorSide, fn, inApp) {
  return { file: anchorSide.path, line: anchorSide.line, column: anchorSide.column, function: fn, in_app: inApp };
}

function sha(s) {
  return createHash('sha256').update(s).digest('hex');
}

export function generate(outDir, projectKey = DEFAULT_PROJECT_KEY) {
  const buildA = readBuild('dist-a');
  const buildB = readBuild('dist-b');
  const pool = anchors(buildA, buildB);
  if (pool.length < 12) throw new Error(`only ${pool.length} anchors; the fixture app is too small`);

  const rand = lcg(0x50d3a5ed);
  const pick = (n) => rand() % n;

  // TRIPLES distinct ordered triples of distinct anchors.
  const triples = [];
  const tripleKeys = new Set();
  while (triples.length < TRIPLES) {
    const i = pick(pool.length);
    let j = pick(pool.length);
    while (j === i) j = pick(pool.length);
    let k = pick(pool.length);
    while (k === i || k === j) k = pick(pool.length);
    const key = `${i},${j},${k}`;
    if (tripleKeys.has(key)) continue;
    tripleKeys.add(key);
    triples.push([i, j, k]);
  }

  // The 1,000 events as anchor indexes, so builds A and B are literally the same events.
  const plan = [];
  for (let e = 0; e < EVENTS; e++) {
    const triple = triples[e % TRIPLES];
    const errorClass = ERROR_CLASSES[triple[0] % ERROR_CLASSES.length];
    let others;
    // Six non-in_app frames from other anchors; resample until the ten frames span ≥ 5 chunks
    // (the triple's, theirs, and the cap-size map's) as §13 asks.
    for (;;) {
      others = [];
      while (others.length < 6) {
        const i = pick(pool.length);
        if (!triple.includes(i) && !others.includes(i)) others.push(i);
      }
      const chunks = new Set([...triple, ...others].map((i) => pool[i].a.path));
      if (chunks.size >= 4) break; // + the cap-size map = 5
    }
    plan.push({ triple, others, errorClass, capsizeColumn: 1 + pick(400) });
  }

  const build = (side, release) =>
    plan.map((p) => ({
      project_key: projectKey,
      platform: PLATFORM,
      environment: ENVIRONMENT,
      release_version: release,
      error_class: p.errorClass,
      message: `${p.errorClass}: ${pool[p.triple[0]].name} failed`,
      stacktrace: [
        ...p.triple.map((i) => frame(pool[i][side], pool[i][side].token, true)),
        ...p.others.map((i) => frame(pool[i][side], pool[i][side].token, false)),
        { file: CAPSIZE_PATH, line: 1, column: p.capsizeColumn, function: 'v', in_app: false },
      ],
      metadata: { seed: 'sourcemaps', build: side },
    }));

  const sets = {
    'events-a': build('a', RELEASE_R1),
    'events-b': build('b', RELEASE_R1),
    'events-r0a': build('a', RELEASE_R0),
    'events-r0b': build('b', RELEASE_R0),
  };

  const t = pool[triples[0][0]];
  const partial = {
    project_key: projectKey,
    platform: PLATFORM,
    environment: ENVIRONMENT,
    release_version: RELEASE_R1,
    error_class: 'TypeError',
    message: 'TypeError: partially symbolicated (one frame without a column, one on an unmapped chunk)',
    stacktrace: [
      frame(t.a, t.a.token, true),
      { file: pool[triples[0][1]].a.path, line: pool[triples[0][1]].a.line, function: pool[triples[0][1]].a.token, in_app: true },
      { file: UNMAPPED_PATH, line: 1, column: 77, function: 'q', in_app: true },
      frame(pool[triples[0][2]].a, pool[triples[0][2]].a.token, false),
    ],
    metadata: { seed: 'sourcemaps', build: 'partial' },
  };

  const touch = (build, message, frames) => ({
    project_key: projectKey,
    platform: PLATFORM,
    environment: ENVIRONMENT,
    release_version: RELEASE_R1,
    error_class: 'Error',
    message,
    stacktrace: frames,
    metadata: { seed: 'sourcemaps', build },
  });
  const touchSmall = touch('touch-small', 'Error: one frame on every committed map (M2 cold fetch, small maps)',
    [...buildA, ...buildB].map((m) => {
      const s = m.segments.find((x) => x.token) ?? m.segments[0];
      return { file: m.minifiedPath, line: s.genLine + 1, column: s.genCol + 1, function: s.token ?? 'a', in_app: false };
    }));
  const touchCapsize = touch('touch-capsize', 'Error: one frame on the cap-size map (M2 cold fetch, cap-size map alone)',
    [{ file: CAPSIZE_PATH, line: 1, column: 500, function: 'v', in_app: false }]);

  mkdirSync(outDir, { recursive: true });
  const outputs = {};
  for (const [name, events] of Object.entries(sets)) {
    mkdirSync(join(outDir, name), { recursive: true });
    for (let b = 0; b * BATCH < events.length; b++) {
      const rel = `${name}/${String(b).padStart(3, '0')}.json`;
      const body = JSON.stringify(events.slice(b * BATCH, (b + 1) * BATCH));
      writeFileSync(join(outDir, rel), body);
      outputs[rel] = sha(body);
    }
  }
  for (const [rel, body] of [
    ['partial.json', JSON.stringify(partial)],
    ['touch-small.json', JSON.stringify(touchSmall)],
    ['touch-capsize.json', JSON.stringify(touchCapsize)],
  ]) {
    writeFileSync(join(outDir, rel), body);
    outputs[rel] = sha(body);
  }

  const manifest = {
    project_key: projectKey,
    n: TRIPLES,
    events: EVENTS,
    batch: BATCH,
    releases: { r1: RELEASE_R1, r0: RELEASE_R0 },
    capsize: { minified_path: CAPSIZE_PATH },
    unmapped_path: UNMAPPED_PATH,
    maps: {
      a: buildA.map(({ minifiedPath, mapFile, bytes, sha256 }) => ({ minified_path: minifiedPath, map: `dist-a/assets/${mapFile}`, bytes, sha256 })),
      b: buildB.map(({ minifiedPath, mapFile, bytes, sha256 }) => ({ minified_path: minifiedPath, map: `dist-b/assets/${mapFile}`, bytes, sha256 })),
    },
    anchors: pool,
    triples: triples.map((tr) => tr.map((i) => `${pool[i].source}:${pool[i].name}`)),
    outputs,
  };
  writeFileSync(join(outDir, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
  return manifest;
}

if (process.argv[1] && import.meta.url === new URL(`file://${process.argv[1]}`).href) {
  const out = process.argv[2];
  if (!out) {
    console.error('usage: node generate-events.mjs <out dir> [project_key]');
    process.exit(2);
  }
  const m = generate(out, process.argv[3] || DEFAULT_PROJECT_KEY);
  console.error(`wrote ${Object.keys(m.outputs).length} files under ${out}: N=${m.n} triples over ${m.anchors.length} anchors, ${m.maps.a.length}+${m.maps.b.length} maps`);
}
