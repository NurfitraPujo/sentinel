# fixtures-build

A `.next`-shaped output for the CLI and wrapper suites (contract §4.8, §4.9; flows C1, C9, W1's unit half) and
for item 7's compose run of C1 / C3 against the real `/api/sourcemaps`:

- `.next/static/chunks/**` — seven client maps (six `.js.map`, one `.mjs.map`), each a minimal but valid
  sourcemap v3 (`version: 3`, a `mappings` string, a `sources` array — what the route's structural check reads).
  `webpack-9c0d.js.map` deliberately has no sibling `.js`: the walk warns and uploads it anyway (§4.8 step 1).
- `.next/static/css/styles.css.map` — never walked by the CLI (C4), swept by the wrapper (W1).
- `.next/server/**` — two server maps for the second walk (decision 18), one with Turbopack's `_052cc036._.js`
  shape.

`sentinel-sourcemaps upload tests/fixtures-build/.next/static /_next/static --dry-run --release 2.34.0
--dashboard-url http://localhost:5173 --api-key x` prints the seven pairs and "1 request(s) would be sent".

## `turbopack/.next` — Turbopack 15.5.21's shape (BUG-01)

Turbopack names a client chunk's map by a **different hash** than the chunk and writes no `file` field: on the
TASK-07 scratch build `d69eb7d85499027c.js` ends in `//# sourceMappingURL=8f15d5d03090a0be.js.map` and no
`d69eb7d85499027c.js.map` exists (16 of 19 chunks; 3 have no map at all). The name-based pairing stored
`/_next/static/chunks/8f15d5d03090a0be.js`, a path no browser frame carries, so nothing symbolicated. The walker
now pairs by the chunk's own comment (contract §4.8 step 1 as amended — decision 43) and this tree pins every
shape it has to handle; the webpack tree above is unchanged, because webpack's comment names `<chunk>.js.map`.

- `static/chunks/d69eb7d85499027c.js` → `8f15d5d03090a0be.js.map`, `turbopack-4364a8ae2a6af640.js` →
  `e96569a392fa278c.js.map` — stored under the **chunk's** path (`/_next/static/chunks/d69eb7d85499027c.js`).
- `static/chunks/3f9721961e8f2ba6.js` — no comment, no map: skipped, named in the one "no sourceMappingURL"
  warn of the walk (Next's `*_client-reference-manifest.js` files under `server/` are the same case).
- `static/chunks/60394cfab7bcbc98.js` — its comment names `4c218c652b8c48af.js.map`, which does not exist:
  skipped with one warn naming the chunk and the missing file.
- `static/chunks/9a8b7c6d5e4f3a2b.js.map` — no chunk references it: the original name-based pairing, uploaded
  as `/_next/static/chunks/9a8b7c6d5e4f3a2b.js` with the "no … beside it" warn.
- `static/chunks/20c20fcd5d02c4c2.css.map` — never walked, swept by the wrapper.
- `server/chunks/_0fe1378c._.js` ↔ `_0fe1378c._.js.map` — Turbopack's server chunks DO share a name;
  `[root-of-the-server]__a6734ae3._.js` and `f9941_@sentinel_sdk-js_dist_nextjs_edge_d8f68d22.js` carry
  **percent-encoded** comments (`%5B`, `%40`) that must be decoded to find the file beside them.
- `server/edge/chunks/turbopack-edge-wrapper_56e5ba59.js` → `edge-wrapper_56e5ba59.js.map` — the edge series'
  differently-named pair, stored under `<serverRoot>/edge/chunks/turbopack-edge-wrapper_56e5ba59.js`;
  `edge-wrapper_8937e6f3.js` ↔ its own map.

`sentinel-sourcemaps upload tests/fixtures-build/turbopack/.next/static /_next/static --dry-run …` prints three
pairs (the two comment pairs and the orphan) and two warnings; the server walk prints five pairs and one.
