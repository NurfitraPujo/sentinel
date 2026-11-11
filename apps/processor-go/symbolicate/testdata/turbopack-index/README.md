# The committed real Turbopack index map and stub (sentinel-sourcemaps-index-maps contract §4.3, §4.6, D8)

Four files taken **verbatim** from a real `next build --turbopack` of `example-org/web` — the
**Sentry-wrapped baseline build** named in the contract's *Basis*
(`~/works/worktrees/ws-web-sentinel-baseline/web` @ `8ec24a0fb`, built 2026-09-17, copied
2026-09-18 from its `.next/server/`). Nothing here is hand-edited and there is no build step: to
regenerate, rebuild `web` at that commit and copy the same four paths. Contract §4.6 commits the
same four files a second time under `tests/fixtures/sourcemaps/turbopack/` for the e2e (D8, a
separate work item); the sha256s below are the guard that the two copies agree.

Because the build is Sentry-wrapped (not sdk-js-wrapped), the fixture has the *baseline* shape the
contract's §4.6 describes as one of the two valid choices: the stub's map carries a `debugId` key
(`sources: null`, `sections: []` — the route's R2 shape), and both `.js` files carry a Sentry
debug-id prelude as line 1 and a trailing `//# debugId=` line. The chunk is therefore **759 lines**,
and the §4.3 rows I1–I12 are computed against exactly this file — for the baseline's files they are
the contract's rows verbatim (D9).

| path (under this directory) | bytes | sha256 |
| --- | ---: | --- |
| `chunks/[turbopack]_runtime.js` | 28 814 | `5bea1bdae81fd6c2787aefd28a9aa5faa60862c27409eea616e365b8bb895878` |
| `chunks/[turbopack]_runtime.js.map` | 48 500 | `a7ecaf6d11192504778cfaea46ba3fb7114a02edcc138f84e170998545ae26e1` |
| `app/page.js` | 1 418 | `a25eae849738b162eecd7f24f7b3d74f02c95648610bc48ab55d0d91dcca1ba5` |
| `app/page.js.map` | 91 | `ef70533167e6e9c1a1d0d1893cd67933ba6816f0af0e26afbecca99ae5939714` |

Check with `sha256sum chunks/* app/*` from this directory.

## `chunks/[turbopack]_runtime.js.map` — the index map

`{"version":3,"sources":[],"sections":[…]}` with **five sections**, each `{ offset, map }` where
`map` is a v3 flat map (`version`, `sources` (one entry), `names: []`, `mappings`, plus
`sourcesContent` and `ignoreList`, which the decoder drops — decision 35). No section has a
`sourceRoot`, so `file` on a rewritten frame is the `sources` entry verbatim, and every `names` is
empty, so `function` is always kept from the frame. The five offsets are **0-based generated
positions** (ECMA-426 §5), all `column: 0`:

| # | offset `(line, column)` | `sources[0]` | inner map lines |
| --- | --- | --- | ---: |
| 0 | `(3, 0)` | `turbopack:///[turbopack]/shared/runtime-utils.ts` | 467 |
| 1 | `(470, 0)` | `turbopack:///[turbopack]/shared-node/base-externals-utils.ts` | 41 |
| 2 | `(511, 0)` | `turbopack:///[turbopack]/shared-node/node-externals-utils.ts` | 21 |
| 3 | `(532, 0)` | `turbopack:///[turbopack]/shared-node/node-wasm-utils.ts` | 21 |
| 4 | `(553, 0)` | `turbopack:///[turbopack]/nodejs/runtime.ts` | 201 |

So on the WIRE (1-based) the first section starts at line 4: wire lines 1–3 precede every section
and are `no_section` (I1, I2); wire line 471 is section 1's first line (I7); wire line 554 is
section 4's first line, and its 201 inner lines end at wire line 754 — a frame at wire 755 or 760
is past the section's map and `no_mapping` (I11, I12). `TestParse_IndexMapSections`
(`sourcemap_test.go`) pins the five offsets and sources; `TestSymbolicate_IndexMap`
(`service_test.go`) drives I1–I17 through `Service.Symbolicate`.

The chunk's own text is committed so the "what line N says" column of the contract's table can be
re-read — `sed -n 600p 'chunks/[turbopack]_runtime.js'` is `const chunkCache = new Map();` (I8 →
`nodejs/runtime.ts:77:7`), line 300 is `let moduleId = chunkModules[i];` (I5 →
`shared/runtime-utils.ts:456:9`), line 1 is the Sentry prelude, line 2 is
`const RUNTIME_PUBLIC_PATH = …` (the contract's I1 description names line 1 by the un-wrapped
build's numbering; the ROW — `(1, 1)` is `no_section` — holds for either, since both precede the
first offset).

## `app/page.js.map` — the empty stub

`{"version":3,"sources":null,"sections":[],"debugId":"…"}` — 91 bytes, zero sections. Under D12 the
route never stores one of these, so the processor never fetches it; the parser still accepts it
as a valid zero-section index map (`TestParse_IndexMapZeroSections`, flow P9), and the service
table's I14 seeds **no row** for its path and asserts `no_map` with the fetcher uncalled.

`manifest.json` / `generate-events.mjs` under `tests/fixtures/sourcemaps/` do **not** know about
these files; the seed is unchanged (D15).
