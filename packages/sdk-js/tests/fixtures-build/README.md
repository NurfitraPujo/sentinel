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
