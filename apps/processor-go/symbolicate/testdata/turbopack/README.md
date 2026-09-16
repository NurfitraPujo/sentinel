# The committed Turbopack-shaped fixture map (sentinel-sourcemaps-turbopack-grouping contract §4.2, §11)

A hand-written v3 map whose `sources` array carries the twenty rewritten rows of contract §4.2's
fixture table **verbatim** — A1–A17 and A20–A22, one entry each, in that order — so
`TestSymbolicate_InAppTable` (`service_test.go`) can drive every A-row through
`Service.Symbolicate` with the real resolver in the loop and assert the returned `Frame.InApp` and
`Frame.Raw.InApp`. A18 (`no_map`) and A19 (`no_column`) are frames the stage does not rewrite, so
they need no `sources` entry and are driven from the test alone.

- `ac0ebedaba77188c.js` — a stand-in for a Turbopack browser chunk
  (`/_next/static/chunks/<16-hex>.js`): twenty generated lines, one per source, plus the
  `sourceMappingURL` comment. Nothing reads its text; it exists so the fixture has the shape of a
  chunk and its map, as `testdata/vite/` does.
- `ac0ebedaba77188c.js.map` — its v3 map: twenty `sources`, **no `sourceRoot`** (so
  `JoinSourceRoot` returns each entry verbatim — the webpack, Vite and bare-relative rows are
  strings in this array, not a second fixture), one `names` entry (`renderWithHooks`, on A1 only,
  matching contract §4.2's stored-shape example), no `sourcesContent`.

**Not** the output of a build: no `next build --turbopack` produces a map whose `sources` mix
`turbopack:///`, `webpack:///`, `webpack://_N_E/` and Vite's `../../` forms, and the rule under test
reads only the resolved string, so the strings are what is pinned. There is no build step and
nothing to regenerate. A2's `…` (U+2026) is the contract's own elision, kept verbatim as the row
is written there.

## The mappings, by hand

The pattern is `sourcemap_test.go`'s `TestResolve_Convention` (`AAAA;EACA,GAAC`) and
`TestSymbolicate_SourcelessSegmentIsNoMapping` (`AAAA,E,EAAC`): base64 VLQ, `;` between
generated lines, the v3 running state (the generated column resets per line; source, original
line, original column and name carry across lines).

    AAAAA  → genCol +0, source +0, origLine +0, origCol +0, name +0  ⇒ gen (1, col 0) → sources[0]  1:1, name `renderWithHooks`
    ACAA   → genCol +0, source +1, origLine +0, origCol +0           ⇒ gen (2, col 0) → sources[1]  1:1
    ACAA   → …                                                       ⇒ gen (3, col 0) → sources[2]  1:1
    …  (nineteen `ACAA` lines in all)
    ACAA                                                             ⇒ gen (20, col 0) → sources[19] 1:1

So wire position `(n, 1)` — 1-based line and column, as browsers print and §4.3 carries — resolves
to `sources[n-1]` at original line 1, column 1 for every `n` in 1…20 (the generated column is
0-based in the map, so wire column 1 is the segment at generated column 0). The test states wire
positions and expects the row's `file`; the conversion under test is the decoder's.
