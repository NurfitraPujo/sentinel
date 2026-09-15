# `tests/fixtures/sourcemaps/` — the sourcemap fixture bundle

The committed fixtures of `sentinel-sourcemaps` (contract §13, be.md task 11, decision 39):
one real Vite + TypeScript app, its two builds with their maps, a shared path-normalisation
fixture, and two deterministic generators the seed runs. Nothing here is loaded by the services;
the seed (`apps/dashboard-web/scripts/sourcemaps-seed.sh`) uploads the maps through
`POST /api/sourcemaps` and posts the events through `POST /ingest/batch`, and the e2e case
(`tests/e2e/sourcemaps_test.go`) reads the maps from disk. A root `.dockerignore` keeps this
directory out of every image build context.

| Path | What it is |
| --- | --- |
| `path-normalisation.json` | decision 22's shared fixture; the Go and TypeScript normalisers both pin to it (TASK-02/TASK-04) |
| `app/` | the source: `src/main.ts` + five lazy routes (`cart`, `checkout`, `search`, `auth`, `analytics`) + `src/lib/{money,http}.ts`; `vite.config.ts` says how the two builds differ. `pnpm install && pnpm build` rebuilds both (vite 5.4.21, typescript 5.9.3, pinned; `pnpm-lock.yaml` pins the rest) |
| `dist-a/` | **build A** — `assets/[name]-[hash].js` with `hashCharacters: 'hex'`; **8 chunks, 8 maps, 18,030 bytes of maps** (15,245 bytes of JS beside them) |
| `dist-b/` | **build B** — the same source, `assets/[name]-[hash]_b2.js`; **8 chunks, 8 maps, 18,054 bytes of maps**. Every path differs from build A's (the `_b2` suffix), so uploading it under the same release `create`s 8 rows; two chunks (`money`, `http`) keep their content hash because their content did not change — the suffix is what makes them new `minified_path`s |
| `vlq.mjs` | base64 VLQ encode/decode, no dependency; shared by the two generators |
| `generate-capsize-map.mjs` | the **cap-size map generator** (decision 39): `node generate-capsize-map.mjs <out>` writes a valid v3 map of **51,904,515 bytes** = `SOURCEMAP_MAX_BYTES` (52,428,800) − 524,285, whose `mappings` string (9,973,070 segments over 4,988 lines) is the padding — never `sourcesContent`. Never committed; the seed runs it |
| `generate-events.mjs` | the **event generator**: `node generate-events.mjs <out dir> [project_key]` derives the four 1,000-event sets (20 batch files of 50 each, one `/ingest/batch` body per file), the partial event, M2's two touch events and a `manifest.json` from the committed builds (below) |

**Map count and total size, as the committed app builds them:** 16 maps (8 per build),
**36,084 bytes** in all; the whole directory without `app/node_modules` is ~106 KB. A ~30-file app
does not yield 20 maps or 40 MB, and no number here pretends it does (§13); the cap-size map is
what exercises the caps, and it is generated.

## The generators are byte-identical across runs

Both use a fixed-seed LCG, no clock, no environment, so a re-run of the seed re-uploads bytes the
route reports as `unchanged` (§4.1). Two consecutive runs on 2026-09-15:

```
$ node generate-capsize-map.mjs cap1.map && node generate-capsize-map.mjs cap2.map && sha256sum cap1.map cap2.map
wrote cap1.map: 51904515 bytes, minified_path /assets/capsize-9b7f3e1a2c4d5e6f.js
wrote cap2.map: 51904515 bytes, minified_path /assets/capsize-9b7f3e1a2c4d5e6f.js
4572d38234dadb6ff9c9c9499fc5f6162f781aa815863a1d176b40092f68e1ba  cap1.map
4572d38234dadb6ff9c9c9499fc5f6162f781aa815863a1d176b40092f68e1ba  cap2.map

$ node generate-events.mjs ev1 && node generate-events.mjs ev2 && diff -r ev1 ev2 && echo IDENTICAL
IDENTICAL
$ (cd ev1 && find . -type f -name '*.json' | sort | xargs sha256sum | sha256sum)
ddc335c71cdee75d92be46e5284c5aff467abd92d1e103edd97df0753879cb77  -
```

The cap-size map's `sourceRoot` is empty, its 64 `sources` are `../../app/src/capsize/module-NN.ts`
and its 256 `names` `fn0`…`fn255`; every segment's indexes stay inside both arrays, so it decodes
without a `map_error` (checked against `apps/processor-go/symbolicate`'s decoder: 816 ms to parse,
a `resolve(1, 500)` that lands on `module-06.ts:32:26`). The first generated line carries 1,000
segments, so the frame every seed event puts on `/assets/capsize-9b7f3e1a2c4d5e6f.js` line 1
resolves; that is how M2 cold-parses it on the first event.

## N — what the 1,000 events span

**N = 60.** `generate-events.mjs` fixes it like this:

- An **anchor** is one of the app's 42 top-level function names as it appears in build A's maps:
  the first segment whose `name` is that function, the identifier the generated code carries at
  that generated position (the minified `function` a browser would print, e.g. `f` for
  `addToCart`), and the same `(source, original line, original column, name)` found again in
  build B's map with its own chunk name and generated column. An anchor is dropped unless build B's
  minified identifier equals build A's, and the generator refuses to run if any differs — that
  equality is what lets the no-maps run (flow S5) reach the same fingerprint for both builds.
- A **triple** is an ordered choice of three distinct anchors. The generator draws **60 distinct
  triples** and every event's first three frames are its triple with `in_app: true`; frames 4–9
  are six other anchors with `in_app: false`, spread so the ten frames touch at least five chunks
  (four of the build's plus the cap-size map); frame 10 is on the cap-size map. `error_class` is a
  function of the triple, `environment` is `production` for every event.
- `fingerprint.Compute` hashes `environment | error_class | (file:function) × 3` over the first
  three `in_app` frames (`MaxAppFrames = 3`) and never sees line or column, so **the 1,000 events
  produce exactly 60 fingerprints** — with maps, `(../../app/src/cart.ts, addToCart)`-shaped; without
  maps, `(/assets/cart-*.js, f)`-shaped after §4.4's normalisation — and the generator also refuses
  to run if two anchors share a raw `(normalised chunk, minified identifier)`, so the no-maps set
  has 60 members too. `manifest.json` in the output lists the anchors and the triples.

Both builds' events are the SAME 1,000 events: the same triples and the same source positions,
build B's chunk names and generated columns. That is M1's claim (S1/S2 with maps, S5 without):
after build A's events the project has 60 issues; after build B's, still 60.

## Two facts about Vite's naming worth knowing before changing `vite.config.ts`

- **`hashCharacters: 'hex'` is deliberate.** Rollup's default base64url hashes escaped §4.4's
  hash-token rule as first written whenever they carried no digit and were not all-hex — on this
  app, a default build names `analytics-CuqmStVf.js`, `cart-oQLsOyVD.js`, `money-Bcjv_cAO.js` and
  `search-BJccElTd.js` (4 of 8 chunks) — and a hash containing `-` (`index-Ab-3xyz.js`) split at
  the wrong separator. Decision 41 (BUG-03) widened the rule and decision 42 (BUG-06) fixed its
  measured reading (≥ 2 upper-case letters after position 0, no vowel run over 2, the `-` inside
  a token skipped unconditionally — `TestNormalizeHashInput_Base64urlMissRate` re-measures the
  residual, **3.72 %** as decision 44 records it, on 200,000 random names every run); those four
  names and the `-` cases are rows in
  `tests/unit/fingerprint_normalisation_test.go`. Hex stays here so the committed
  maps, their digests and the M1–M3 numbers in `status/be.md` remain byte-stable.
- **The build id sits inside the token.** `[name]-[hash]_b2.js` normalises to `<name>-*.js` because
  `<hash>_b2` is 6–64 chars of `[A-Za-z0-9_]` with a digit. `[name]-[hash]-b2.js` or
  `[name]-[hash].b2.js` would not (the rule examines one token, immediately before the extension
  run), and the no-maps run would then see two identities per source position.
