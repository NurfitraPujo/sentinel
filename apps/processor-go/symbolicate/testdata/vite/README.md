# The committed fixture map (contract §11, be.md task 5)

A real Vite build of a small TypeScript source, committed so the decoder's line/column
convention and the eight outcomes are proved against a known map, not a hand-written one.

- `src/main.ts` — the source (38 lines; `subtotal`, `applyCoupon`, `class Checkout`, two `throw`s).
- `dist/assets/index-d_kdqh_W.js` — Vite's output: ONE generated line, esbuild-minified.
- `dist/assets/index-d_kdqh_W.js.map` — its v3 map: `sources: ["../../src/main.ts"]`, no
  `sourceRoot`, nine `names`, and `sourcesContent` (present on purpose: `TestParse_DropsSourcesContent`
  proves decision 35).

Produced once with `vite v5.4.21` (the dashboard's own copy) from this directory's `index.html` and
`vite.config.ts`:

    vite build

The bytes are frozen — a rebuild with another Vite/esbuild produces different mappings, and the
hand-computed positions in `service_test.go` are derived from THESE bytes. Do not regenerate.

## How the expected positions were derived

Independently of the Go decoder, by decoding the `mappings` string by hand (base64 VLQ, five
fields per segment, the running-state rule of the v3 spec) — the first segments, for the record:

    AAKO   → genCol +0,  source +0, origLine +5,  origCol +7   ⇒ gen (1, col 0)  → main.ts line 6, col 8   (`function`)
    SAASA  → genCol +9,  source +0, origLine +0,  origCol +9, name +0 ⇒ gen (1, col 9) → line 6, col 17, name `subtotal`
    EAASC  → genCol +2,  source +0, origLine +0,  origCol +9, name +1 ⇒ gen (1, col 11) → line 6, col 26, name `cart`

Generated columns in the map are 0-based; the wire (browsers, §4.3) is 1-based on both line and
column, so a wire column is `genCol + 1` and an original position is `(origLine + 1, origCol + 1)`.
The test table states wire positions and expects 1-based origins; the conversion under test is the
decoder's, and the table was written before the decoder ran.
