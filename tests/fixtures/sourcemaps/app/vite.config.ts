// The fixture's two builds (sentinel-sourcemaps contract §13, decision 39). One source tree, two
// output namings:
//
//   FIXTURE_BUILD=a  ->  ../dist-a  assets/[name]-[hash].js          (build A: plain Vite naming)
//   FIXTURE_BUILD=b  ->  ../dist-b  assets/[name]-[hash]_b2.js       (build B: the same source with a
//                                                                      build-id suffix in the name)
//
// Build B exists so a second upload under the same release_version CREATES rows rather than
// reporting `unchanged`: a chunk whose content did not change keeps its content hash, so without
// the suffix its minified_path would be byte-identical to build A's. The suffix is the build id,
// carried by entryFileNames/chunkFileNames as the contract says; it sits INSIDE the hash token
// (`[hash]_b2` is 6-64 chars of [A-Za-z0-9_] containing a digit) so §4.4's basename rule still
// reduces both builds to `<name>-*.js` and the no-maps run (flow S5) can prove A ∪ B = A.
//
// hashCharacters is `hex` on purpose. Rollup's default is base64url, whose 8-char hashes contain
// `-` about 12% of the time and contain NO digit about 26% of the time; either shape escapes the
// contract's hash-token rule (`-` is not in [A-Za-z0-9_]; a digitless non-hex token is a word).
// Hex hashes are always a token, which keeps the fixture about the rule as written; the finding
// itself is reported in status/be.md, not silently absorbed here.
import { defineConfig } from 'vite';

const build = process.env.FIXTURE_BUILD === 'b' ? 'b' : 'a';
const suffix = build === 'b' ? '_b2' : '';

export default defineConfig({
  build: {
    outDir: `../dist-${build}`,
    emptyOutDir: true,
    sourcemap: true,
    minify: 'esbuild',
    reportCompressedSize: false,
    modulePreload: { polyfill: false },
    rollupOptions: {
      output: {
        hashCharacters: 'hex',
        entryFileNames: `assets/[name]-[hash]${suffix}.js`,
        chunkFileNames: `assets/[name]-[hash]${suffix}.js`,
        assetFileNames: `assets/[name]-[hash]${suffix}[extname]`,
      },
    },
  },
});
