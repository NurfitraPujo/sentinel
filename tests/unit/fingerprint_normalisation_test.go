package unit

import (
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hashNormalisationCases is sentinel-sourcemaps decision 6's deliverable, filed verbatim in prd
// `sentinel-sourcemaps/status/be.md` for ratification: the ≥ 20-case fixture table for §4.4's
// "Fingerprint input normalisation" — every negative §4.4 names, decision 29's three hard cases,
// `1234-abcdef.js`, decision 41's Rollup base64url names with their negatives, and the edges the
// rule's wording leaves to the implementer, stated as cases so the ratification is on them. Each row is asserted TWICE below: on
// fingerprint.NormalizeHashInput directly, and through fingerprint.Compute — where platform
// `javascript` must hash the input as the expected output would, and platform `golang` must be
// a byte-identical no-op on the same input (flows S5 / S6).
//
// Column `rule` names which part of §4.4 decides the row: path (step 1's NormalizePath),
// basename, directory, token (the hash-token definition), extension (decision 29's run), or
// none (a negative that must survive byte-identical).
var hashNormalisationCases = []struct {
	name, in, out, rule string
}{
	// --- the contract's own examples (§4.4, basename rule) ---
	{"vite chunk, dash separator", "chunk-a3f9b2.js", "chunk-*.js", "basename"},
	{"vite entry, dash separator", "/assets/index-3f2a1c.js", "/assets/index-*.js", "basename"},
	{"webpack entry, dot separator", "main.a1b2c3d4.js", "main.*.js", "basename"},
	{"next.js page, 16-hex token", "page-9b0d4e7f1a2b3c4d.js", "page-*.js", "basename"},
	{"vite css, mixed-case token with a digit", "index-DkLmN3Qa.css", "index-*.css", "basename"},
	{"webpack all-hex token, no digit", "vendor-abcdef.js", "vendor-*.js", "token"},
	// --- decision 29's three hard cases ---
	{"CRA: extension run is .chunk.js", "main.a1b2c3d4.chunk.js", "main.*.chunk.js", "extension"},
	{"next.js 15: bare-hash basename, whole stem is the token", "8f7a6b5c3d2e1f0a.js", "*.js", "basename"},
	{"multiple candidates: only the token before the run", "vendor-react-a3f9b2.js", "vendor-react-*.js", "basename"},
	// --- the negatives §4.4 says must survive ---
	{"below the token minimum", "1234.js", "1234.js", "none"},
	{"no token at all", "main.chunk.js", "main.chunk.js", "none"},
	{"a word after the separator", "polyfills-legacy.js", "polyfills-legacy.js", "none"},
	{"a word stem: vendor", "vendor.js", "vendor.js", "none"},
	{"a word stem with a capital: Header", "Header.js", "Header.js", "none"},
	{"a word stem: useAuth", "useAuth.js", "useAuth.js", "none"},
	{"a word stem: polyfills", "polyfills.js", "polyfills.js", "none"},
	// --- the numeric-id case §4.4 names ---
	{"next.js numeric chunk id is kept", "1234-abcdef.js", "1234-*.js", "basename"},
	// --- directory rule ---
	{"next.js build id segment", "/_next/static/AbC123xyz_-9pQrStUv/_buildManifest.js", "/_next/static/*/_buildManifest.js", "directory"},
	{"build id AND hashed chunk in one path", "/_next/static/AbC123xyz_-9pQrStUv/chunks/main-a1b2c3d4.js", "/_next/static/*/chunks/main-*.js", "directory+basename"},
	{"a 16-char segment without a digit is not a build id", "/static/abcdefghijklmnop/x.js", "/static/abcdefghijklmnop/x.js", "none"},
	{"a 15-char segment with digits is below the bound", "/static/abcdefghij12345/x.js", "/static/abcdefghij12345/x.js", "none"},
	{"a segment with a dot is not a build id", "/static/abc.def123456789012/x.js", "/static/abc.def123456789012/x.js", "none"},
	{"the basename is the basename rule's alone (a bare 20-char id with no extension; decision 42 joins its inner `-`)", "/static/AbC123xyz_-9pQrStUvW", "/static/*", "basename"},
	// --- step 1 (path normalisation) composes with the rules ---
	{"scheme and host dropped, then the basename rule", "https://app.example.com/assets/index-3f2a1c.js", "/assets/index-*.js", "path+basename"},
	{"cache-buster query dropped, then the basename rule", "/assets/index-3f2a1c.js?v=1725100000", "/assets/index-*.js", "path+basename"},
	{"webpack:// scheme is byte-identical under step 1; a symbolicated source has no token", "webpack:///./src/app/page.tsx", "webpack:///./src/app/page.tsx", "none"},
	{"a symbolicated source path is unchanged", "src/app/page.tsx", "src/app/page.tsx", "none"},
	// --- extension run edges (decision 29) ---
	{".js.map is one run", "chunk-a3f9b2.js.map", "chunk-*.js.map", "extension"},
	{".min.js is one run", "lodash-4a1b2c.min.js", "lodash-*.min.js", "extension"},
	{"a 9-letter suffix is not an extension: the stem ends there and carries no token", "bundle-a1b2c3.abcdefghi", "bundle-a1b2c3.abcdefghi", "extension"},
	{"an upper-case suffix is not an extension", "main-a1b2c3.JS", "main-a1b2c3.JS", "extension"},
	{"no extension: whole stem examined", "a1b2c3d4", "*", "basename"},
	// --- token edges ---
	{"a 5-char token is below the minimum", "x-a1b2c.js", "x-a1b2c.js", "token"},
	{"a 6-char token with a digit is the minimum", "x-a1b2c3.js", "x-*.js", "token"},
	{"a 64-char token is the maximum", "x-" + strings.Repeat("a1b2c3d4", 8) + ".js", "x-*.js", "token"},
	{"a 65-char token is over the maximum", "x-" + strings.Repeat("a1b2c3d4", 8) + "e.js", "x-" + strings.Repeat("a1b2c3d4", 8) + "e.js", "token"},
	{"underscore is a token character", "x-a1_b2c3.js", "x-*.js", "token"},
	{"a dash inside a digit-bearing candidate is skipped too (decision 42); `x` is a word, never joined", "x-a1b2c3-d4e5f6.js", "x-*.js", "token"},
	{"consequence of the all-hex clause, stated: a 6-letter hex word strips", "decade.js", "*.js", "token"},
	// --- decision 41: Rollup's default base64url hashes (digitless, non-hex, mixed-case) ---
	{"vite default chunk: digitless mixed-case token", "analytics-CuqmStVf.js", "analytics-*.js", "token"},
	{"vite default chunk: digitless mixed-case token, upper-case run", "cart-oQLsOyVD.js", "cart-*.js", "token"},
	{"the committed vite fixture's own name: underscores and a trailing capital", "index-d_kdqh_W.js", "index-*.js", "token"},
	{"TASK-07 finding 1's other two chunks", "money-Bcjv_cAO.js", "money-*.js", "token"},
	{"TASK-07 finding 1's other two chunks (ii)", "search-BJccElTd.js", "search-*.js", "token"},
	{"a `-` inside the base64url token is skipped: the last piece alone is too short", "cart-oQ-sOyVD.js", "cart-*.js", "token"},
	{"a `-` inside the token when the last piece is a word-shaped fragment", "chunk-CuqmSt-Vfab.js", "chunk-*.js", "token"},
	{"a `-` inside a digit-bearing base64url token", "http-Bx-3kQz9.js", "http-*.js", "token"},
	{"the last piece wins when it is a token by itself: the neighbour is never joined", "vendor-react-a3f9b2.js", "vendor-react-*.js", "token"},
	{"decision 41 negative: a kebab word pair does not strip", "polyfills-legacy.js", "polyfills-legacy.js", "none"},
	{"decision 41 negative: a capitalised word is not a hash (.tsx is an extension)", "Header.tsx", "Header.tsx", "none"},
	{"decision 41 negative: camelCase with a vowel run of 3 (eAu)", "useAuth.js", "useAuth.js", "none"},
	{"decision 41 negative: camelCase whose pieces are syllables", "getUser.js", "getUser.js", "none"},
	{"decision 42 accepted false positive, stated: an acronym tail is ≥ 2 upper-case letters after position 0", "parseHTML.js", "*.js", "token"},
	{"decision 41 negative: next.js's own manifest names", "/_next/static/AbC123xyz_-9pQrStUv/_ssgManifest.js", "/_next/static/*/_ssgManifest.js", "directory"},
	{"decision 41 negative: a word before the `-` is never joined into a token", "my-cool-Component.js", "my-cool-Component.js", "none"},
	{"decision 41 negative: a word before the `-` and a short digit piece", "main-a1b2c.js", "main-a1b2c.js", "none"},
	{"decision 41 negative: all lower-case, no digit, not hex", "qmstvfxk.js", "qmstvfxk.js", "none"},
	{"decision 41 negative: a 5-char mixed-case piece is below the minimum and `x` is no fragment", "x-CuqmS.js", "x-CuqmS.js", "none"},
	// --- decision 42: the measured reading -- ≥ 2 upper-case letters after position 0 and no vowel run over 2; the `-` inside a token is skipped unconditionally ---
	{"decision 42: letter-only base64url hash (the review's FksjBCVK)", "chunk-FksjBCVK.js", "chunk-*.js", "token"},
	{"decision 42: letter-only base64url hash (the review's LVliPaoS)", "chunk-LVliPaoS.js", "chunk-*.js", "token"},
	{"decision 42: letter-only base64url hash (the review's IkGvixYa)", "chunk-IkGvixYa.js", "chunk-*.js", "token"},
	{"decision 42: letter-only base64url hash (the review's qkpCfiLo)", "chunk-qkpCfiLo.js", "chunk-*.js", "token"},
	{"decision 42: letter-only base64url hash (the review's TMVamrKO)", "chunk-TMVamrKO.js", "chunk-*.js", "token"},
	{"decision 42: a leading `-` in the hash is an empty fragment, not a second separator", "chunk--rv42314.js", "chunk-*.js", "token"},
	{"decision 42: a `-` at index 1 of the hash is skipped although the tail is a token by itself", "chunk-A-xyz9Qw.js", "chunk-*.js", "token"},
	{"decision 42: a `-` at index 2 of the hash is skipped although the tail is a token by itself", "chunk-Ab-xyz9Qw.js", "chunk-*.js", "token"},
	{"decision 42 negative: one upper-case letter after position 0 is a capitalised word", "chunk-Abcdefgh.js", "chunk-Abcdefgh.js", "none"},
	{"decision 42 negative: a vowel run of 3 (eau) refuses a token with two upper-case letters", "chunk-BeauTyQx.js", "chunk-BeauTyQx.js", "none"},
	{"decision 42 negative: the directory rule's own basename has one upper-case letter after position 0", "_buildManifest.js", "_buildManifest.js", "none"},
	{"decision 42 negative: a vowel-less word before the `-` is not a fragment", "http-Bx-3kQz9.js", "http-*.js", "token"},
	{"decision 42 negative: a numeric chunk id before the `-` is not a fragment", "1234-Ab-CdEfGh.js", "1234-*.js", "token"},
	{"function is never touched (asserted through Compute only; the path here is a control)", "/assets/x.js", "/assets/x.js", "none"},
	// --- sentinel-sourcemaps-turbopack-grouping §4.3 (D5, decisions 9–10): the Turbopack `_<8-hex>` suffix rule, `._` admitted into the extension run; T1–T30 verbatim, `rule` as the contract's column ---
	{"T1 turbopack: the bare `_<8-hex>._.js` server chunk (249 of 919)", "/app/.next/server/chunks/_052cc036._.js", "/app/.next/server/chunks/_*._.js", "turbopack"},
	{"T2 turbopack: the pnpm store chunk", "/app/.next/server/chunks/node_modules__pnpm_52f51f25._.js", "/app/.next/server/chunks/node_modules__pnpm_*._.js", "turbopack"},
	{"T3 turbopack: `[root-of-the-server]__<8-hex>`", "/app/.next/server/chunks/[root-of-the-server]__0be42a8b._.js", "/app/.next/server/chunks/[root-of-the-server]__*._.js", "turbopack"},
	{"T4 turbopack: `<words>_<8-hex>` under `ssr/`", "/app/.next/server/chunks/ssr/messages_en_bea6f528._.js", "/app/.next/server/chunks/ssr/messages_en_*._.js", "turbopack"},
	{"T5 turbopack: `<words>_<8-hex>` with an extension-shaped word inside the stem", "/app/.next/server/chunks/sentry_server_config_ts_8712b202._.js", "/app/.next/server/chunks/sentry_server_config_ts_*._.js", "turbopack"},
	{"T6 turbopack: the `<5-hex>_` directory-hash prefix is kept (decision 10)", "/app/.next/server/chunks/0264e_next_093e3181._.js", "/app/.next/server/chunks/0264e_next_*._.js", "turbopack"},
	{"T7 turbopack: a scoped package with a `-` inside the stem", "/app/.next/server/chunks/169ae_@opentelemetry_sdk-trace_build_esm_a119968e._.js", "/app/.next/server/chunks/169ae_@opentelemetry_sdk-trace_build_esm_*._.js", "turbopack"},
	{"T8 turbopack: `[externals]_node:<mod>_<8-hex>`", "/app/.next/server/chunks/[externals]_node:inspector_7a4283c6._.js", "/app/.next/server/chunks/[externals]_node:inspector_*._.js", "turbopack"},
	{"T9 turbopack (was `*.js`): no `._`, the `-` widening no longer crosses `app`", "/app/.next/server/chunks/0264e_next_dist_esm_build_templates_app-route_f5a91902.js", "/app/.next/server/chunks/0264e_next_dist_esm_build_templates_app-route_*.js", "turbopack"},
	{"T10 turbopack (was `172b5_@rc-*.js`): a scoped package with a `-`, no `._`", "/app/.next/server/chunks/ssr/172b5_@rc-component_trigger_es_index_1d603611.js", "/app/.next/server/chunks/ssr/172b5_@rc-component_trigger_es_index_*.js", "turbopack"},
	{"T11 turbopack (was unchanged): a scoped package without a `-`, the `@` no longer matters", "/app/.next/server/chunks/ssr/0e6f7_@sentry_replay_build_npm_esm_index_576b294f.js", "/app/.next/server/chunks/ssr/0e6f7_@sentry_replay_build_npm_esm_index_*.js", "turbopack"},
	{"T12 turbopack (was `*.js`): a double `_` and a PascalCase piece inside the stem", "/app/.next/server/chunks/ssr/82842_antd_es__util_PurePanel_d13f8184.js", "/app/.next/server/chunks/ssr/82842_antd_es__util_PurePanel_*.js", "turbopack"},
	{"T13 turbopack (was `turbopack-edge-*.js`): the edge wrapper keeps its whole name", "/app/.next/server/edge/chunks/turbopack-edge-wrapper_8be3eecb.js", "/app/.next/server/edge/chunks/turbopack-edge-wrapper_*.js", "turbopack"},
	{"T14 turbopack: `._` joins a multi-segment run", "_052cc036._.js.map", "_*._.js.map", "turbopack"},
	{"T15 turbopack: the `next dev` browser shape", "/_next/static/chunks/_a0ff3932._.js", "/_next/static/chunks/_*._.js", "turbopack"},
	{"T16 turbopack: the `next dev` browser shape with a source prefix", "/_next/static/chunks/src_common_ad9be9b9._.js", "/_next/static/chunks/src_common_*._.js", "turbopack"},
	{"T17 basename: the browser assertion the intake asks for (path + decision 29's bare hash)", "https://app.tuju.ai/_next/static/chunks/5f982f94079ecf41.js", "/_next/static/chunks/*.js", "basename"},
	{"T18 basename: the browser runtime chunk", "https://app.tuju.ai/_next/static/chunks/turbopack-96c50ac0e47224c6.js", "/_next/static/chunks/turbopack-*.js", "basename"},
	{"T19 none: `[turbopack]_runtime.js` has no hash; stable by construction", "/app/.next/server/chunks/[turbopack]_runtime.js", "/app/.next/server/chunks/[turbopack]_runtime.js", "none"},
	{"T20 none: an app route", "/app/.next/server/app/api/session/route.js", "/app/.next/server/app/api/session/route.js", "none"},
	{"T21 none: middleware", "/app/.next/server/middleware.js", "/app/.next/server/middleware.js", "none"},
	{"T22 basename, unchanged behaviour stated: no `_<8-hex>` suffix, so the existing whole-stem collapse still applies", "/_next/static/chunks/0264e_next_dist_build_polyfills_polyfill-nomodule.js", "/_next/static/chunks/*.js", "basename"},
	{"T23 none: a `_` prefix with no hex suffix", "_buildManifest.js", "_buildManifest.js", "none"},
	{"T24 token: the committed Vite fixture's own name; `_W` is not `_<8-hex>`, the existing rule still fires", "index-d_kdqh_W.js", "index-*.js", "token"},
	{"T25 token: `_b2c3` is 4 hex, not 8", "x-a1_b2c3.js", "x-*.js", "token"},
	{"T26 token: upper-case hex is not the Turbopack suffix; the existing whole-stem rule applies as today", "a_DEADBEEF.js", "*.js", "token"},
	{"T27 token: 7 chars after `_`; existing rule as today", "_1234567.js", "*.js", "token"},
	{"T28 turbopack, accepted false positive stated: 8 digits are 8 hex (the `decade.js` class)", "user_12345678.js", "user_*.js", "turbopack"},
	{"T29 turbopack, residual stated: a 9+-char base64url token ending in `_<8-hex>` strips partially (Rollup's default is 8 chars)", "chunk-Ab_deadbeef.js", "chunk-Ab_*.js", "turbopack"},
	{"T30 basename: `._` admitted into the run, then the existing `.` separator rule (no `_<8-hex>` suffix)", "main.a1b2c3d4._.js", "main.*._.js", "basename"},
}

// symbolicatedFrameCases is the fixture table's decision 44 block (review R8 / BUG-08): the
// symbolicated-frame NEGATIVES. Decision 42's digitless clause reads a three-word camelCase or
// PascalCase identifier as a hash token, which is right for a minified name and wrong for a
// source path — and a symbolicated frame's File IS a source path (the map's `sources` entry),
// with no build hash to strip. So §4.4 now runs the basename and directory rules only on a frame
// symbolication did not rewrite; a symbolicated one gets step 1 alone. Each row states BOTH
// readings so the fix is pinned where decision 44 put it — in Compute's gate, not in the token
// rule: `minified` is what NormalizeHashInput still does to the bare name (decision 42 stands,
// BUG-06 is not undone), and through Compute the same File with Symbolicated set hashes
// byte-identical. The three names are the ones the decision lists; `src/app/page.tsx` is §4.4's
// own sentence.
var symbolicatedFrameCases = []struct {
	name, file, minified string
}{
	{"decision 44 negative: a three-word PascalCase component (U, P, C after position 0; no vowel run over 2)", "src/components/UserProfileCard.tsx", "src/components/*.tsx"},
	{"decision 44 negative: a three-word camelCase hook", "src/hooks/useLocalStorage.ts", "src/hooks/*.ts"},
	{"decision 44 negative: a three-word camelCase function module", "src/api/fetchUserData.ts", "src/api/*.ts"},
	{"decision 44 negative: §4.4's own example, which no clause strips either way", "src/app/page.tsx", "src/app/page.tsx"},
	{"decision 44 negative: a webpack source URL with a directory-rule-shaped segment", "webpack:///./src/features/Ab12Cd34Ef56Gh78/OrderSummaryPanel.tsx", "webpack:///./src/features/*/*.tsx"},
}

// TestCompute_SymbolicatedFrameIsNeverHashStripped is decision 44 through Compute, the layer the
// decision names: for platform `javascript`, a frame carrying Symbolicated hashes over its File
// verbatim (step 1's path rule aside), while the SAME File on a frame the stage did not rewrite
// still gets decision 42's reading. Two component files in one directory stay two identities.
// Delete the Symbolicated arm of Compute's hashFile and every row here goes red on the first
// assertion; narrow isHashToken instead and the `minified` assertion goes red.
func TestCompute_SymbolicatedFrameIsNeverHashStripped(t *testing.T) {
	compute := func(file string, symbolicated bool) string {
		return fingerprint.Compute(fingerprint.FingerprintConfig{
			Environment: "production", ErrorClass: "TypeError",
			Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
			Stacktrace: []fingerprint.StackFrame{{File: file, Line: 12, Function: "onClick", InApp: true, Symbolicated: symbolicated}},
		})
	}
	for _, c := range symbolicatedFrameCases {
		t.Run(c.name, func(t *testing.T) {
			verbatim := ccc52aaHash("production", "TypeError", []fingerprint.StackFrame{{File: c.file, Function: "onClick", InApp: true}})
			stripped := ccc52aaHash("production", "TypeError", []fingerprint.StackFrame{{File: c.minified, Function: "onClick", InApp: true}})

			assert.Equal(t, verbatim, compute(c.file, true), "symbolicated: the source path is the hash input, untouched")
			assert.Equal(t, c.minified, fingerprint.NormalizeHashInput(c.file), "decision 42's reading of the same name as a minified one stands")
			assert.Equal(t, stripped, compute(c.file, false), "not rewritten: the rules still run")
		})
	}

	// The review's failure scenario, as a value: two defects in two components of one directory.
	assert.NotEqual(t,
		compute("src/components/UserProfileCard.tsx", true),
		compute("src/components/OrderSummaryPanel.tsx", true),
		"two symbolicated components in one directory are two identities")
	assert.Equal(t,
		compute("src/components/UserProfileCard.tsx", false),
		compute("src/components/OrderSummaryPanel.tsx", false),
		"control: the same two names as minified frames DO collapse — the fix is the gate, not the rule")
}

// TestCompute_SymbolicatedFrameStillGetsStepOne: decision 44 gates the basename and directory
// rules, not step 1 — a symbolicated File that is an http(s) URL still loses scheme, host, query
// and fragment, so the flag never widens identity beyond what the path rule already joined.
func TestCompute_SymbolicatedFrameStillGetsStepOne(t *testing.T) {
	compute := func(file string) string {
		return fingerprint.Compute(fingerprint.FingerprintConfig{
			Environment: "production", ErrorClass: "TypeError",
			Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
			Stacktrace: []fingerprint.StackFrame{{File: file, Line: 1, Function: "t", InApp: true, Symbolicated: true}},
		})
	}
	want := ccc52aaHash("production", "TypeError", []fingerprint.StackFrame{{File: "/src/components/UserProfileCard.tsx", Function: "t", InApp: true}})
	assert.Equal(t, want, compute("https://app.example.com/src/components/UserProfileCard.tsx?v=2"))
	assert.Equal(t, want, compute("/src/components/UserProfileCard.tsx"))
}

// TestCompute_SymbolicatedFlagIsInertOutsideTheGateSet: for `golang` the flag changes nothing —
// the hash input is ccc52aa's either way, so no pre-feature identity moves.
func TestCompute_SymbolicatedFlagIsInertOutsideTheGateSet(t *testing.T) {
	for _, c := range symbolicatedFrameCases {
		frames := []fingerprint.StackFrame{{File: c.file, Line: 1, Function: "t", InApp: true, Symbolicated: true}}
		got := fingerprint.Compute(fingerprint.FingerprintConfig{
			Environment: "production", ErrorClass: "TypeError",
			Platform: "golang", NormalisePlatforms: symbolicate.DefaultPlatforms, Stacktrace: frames,
		})
		assert.Equal(t, ccc52aaHash("production", "TypeError", frames), got, c.name)
	}
}

// ccc52aaHash is fingerprint.Compute's formula at the branch point, re-implemented here so the
// "byte-identical no-op for golang" claim is measured against what the hash input WAS, not against
// another call into the same function: environment, error class, then `file:function` for the
// first three in_app frames (fallback: the first three frames), '|'-joined, SHA-256, 16 hex.
func ccc52aaHash(environment, errorClass string, frames []fingerprint.StackFrame) string {
	var parts []string
	for _, f := range frames {
		if f.InApp {
			parts = append(parts, f.File+":"+f.Function)
			if len(parts) >= fingerprint.MaxAppFrames {
				break
			}
		}
	}
	if len(parts) == 0 {
		for _, f := range frames {
			parts = append(parts, f.File+":"+f.Function)
			if len(parts) >= fingerprint.MaxAppFrames {
				break
			}
		}
	}
	input := environment + "|" + errorClass
	if len(parts) > 0 {
		input += "|" + strings.Join(parts, "|")
	}
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

// TestNormalizeHashInput_FixtureTable is decision 6's table on the function itself (§11's unit
// boundary: "a pure function of FingerprintConfig ... asserted here, per platform in and out of
// the gate set"). At least 20 rows, the contract's negatives, decision 29's three, `1234-abcdef.js`.
func TestNormalizeHashInput_FixtureTable(t *testing.T) {
	require.GreaterOrEqual(t, len(hashNormalisationCases), 20, "decision 6: the table is at least 20 cases")
	for _, c := range hashNormalisationCases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.out, fingerprint.NormalizeHashInput(c.in), "rule: %s; in: %q", c.rule, c.in)
		})
	}
}

// TestCompute_NormalisationAppliesForJavascript is flow S5 on every row: for platform `javascript`
// (in symbolicate.DefaultPlatforms) the hash input is the normalised path, so the raw input hashes
// exactly as its expected output does — two builds of one chunk are one issue — and the function
// name and the environment prefix are untouched by it.
func TestCompute_NormalisationAppliesForJavascript(t *testing.T) {
	for _, c := range hashNormalisationCases {
		t.Run(c.name, func(t *testing.T) {
			raw := fingerprint.Compute(fingerprint.FingerprintConfig{
				Environment: "production", ErrorClass: "TypeError",
				Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
				Stacktrace: []fingerprint.StackFrame{{File: c.in, Line: 1, Function: "onClick", InApp: true}},
			})
			// The expected value is ccc52aa's formula over the NORMALISED file: this pins that
			// Compute rewrites File and nothing else, not merely that it changed something.
			want := ccc52aaHash("production", "TypeError", []fingerprint.StackFrame{{File: c.out, Function: "onClick", InApp: true}})
			assert.Equal(t, want, raw, "rule: %s; in: %q → %q", c.rule, c.in, c.out)
		})
	}
}

// TestCompute_NormalisationIsANoOpForGolang is flow S6 on every row: platform `golang` is outside
// the gate set, so the SAME input hashes exactly as it did at ccc52aa — byte-identical hash input,
// every existing Go issue keeps its fingerprint. Both the in_app pass and the P4-3 fallback pass
// are checked, because both read File.
func TestCompute_NormalisationIsANoOpForGolang(t *testing.T) {
	for _, c := range hashNormalisationCases {
		t.Run(c.name, func(t *testing.T) {
			inApp := []fingerprint.StackFrame{{File: c.in, Line: 1, Function: "onClick", InApp: true}}
			fallback := []fingerprint.StackFrame{{File: c.in, Line: 1, Function: "onClick", InApp: false}}
			for _, frames := range [][]fingerprint.StackFrame{inApp, fallback} {
				got := fingerprint.Compute(fingerprint.FingerprintConfig{
					Environment: "production", ErrorClass: "TypeError",
					Platform: "golang", NormalisePlatforms: symbolicate.DefaultPlatforms,
					Stacktrace: frames,
				})
				assert.Equal(t, ccc52aaHash("production", "TypeError", frames), got, "rule: %s; in: %q", c.rule, c.in)
			}
		})
	}
}

// TestCompute_NormalisationFollowsTheGateSet pins the gate's three edges: `browser` is in the
// default set (decision 9) and normalises; an EMPTY set — SYMBOLICATE_PLATFORMS set and empty,
// decision 27's kill switch — turns the normalisation off for `javascript` too; and an operator
// admitting `node` without a release (the point of the knob) gets the rule for it.
func TestCompute_NormalisationFollowsTheGateSet(t *testing.T) {
	frames := []fingerprint.StackFrame{{File: "/assets/index-3f2a1c.js", Line: 1, Function: "t", InApp: true}}
	normalised := ccc52aaHash("production", "TypeError", []fingerprint.StackFrame{{File: "/assets/index-*.js", Function: "t", InApp: true}})
	verbatim := ccc52aaHash("production", "TypeError", frames)
	require.NotEqual(t, normalised, verbatim)

	compute := func(platform string, set []string) string {
		return fingerprint.Compute(fingerprint.FingerprintConfig{
			Environment: "production", ErrorClass: "TypeError",
			Platform: platform, NormalisePlatforms: set, Stacktrace: frames,
		})
	}
	assert.Equal(t, normalised, compute("browser", symbolicate.DefaultPlatforms), "browser is in the default set")
	assert.Equal(t, verbatim, compute("javascript", nil), "a nil set is the stage off: no normalisation")
	assert.Equal(t, verbatim, compute("javascript", symbolicate.ParsePlatforms("")), "SYMBOLICATE_PLATFORMS set and empty: off")
	assert.Equal(t, verbatim, compute("node", symbolicate.DefaultPlatforms), "node is not in the default set (decision 9)")
	assert.Equal(t, normalised, compute("node", symbolicate.ParsePlatforms("javascript, node")), "an operator can admit node without a release")
	assert.Equal(t, verbatim, compute("", symbolicate.DefaultPlatforms), "an empty platform is never in the set")
}

// TestCompute_CustomFingerprintEarlyReturnIsUntouchedByNormalisation re-pins §11's "the
// custom-fingerprint early return is re-pinned": a client fingerprint is returned verbatim for a
// platform in the gate set, with frames that would otherwise be normalised.
func TestCompute_CustomFingerprintEarlyReturnIsUntouchedByNormalisation(t *testing.T) {
	got := fingerprint.Compute(fingerprint.FingerprintConfig{
		CustomFingerprint: "checkout-timeout",
		Environment:       "production", ErrorClass: "TypeError",
		Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
		Stacktrace: []fingerprint.StackFrame{{File: "/assets/index-3f2a1c.js", Line: 1, Function: "t", InApp: true}},
	})
	assert.Equal(t, "checkout-timeout", got)
}

// TestCompute_NormalisationNeverTouchesFunctionOrLine: the rule rewrites File in the hash input
// and nothing else — a hash-looking function name and the line survive (line was never in the
// input; function is). Two frames differing only in a hash-shaped function name must stay two
// identities.
func TestCompute_NormalisationNeverTouchesFunction(t *testing.T) {
	compute := func(fn string) string {
		return fingerprint.Compute(fingerprint.FingerprintConfig{
			Environment: "production", ErrorClass: "TypeError",
			Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
			Stacktrace: []fingerprint.StackFrame{{File: "/assets/index-3f2a1c.js", Line: 1, Function: fn, InApp: true}},
		})
	}
	assert.NotEqual(t, compute("a1b2c3"), compute("d4e5f6"), "function is never normalised")
	assert.Equal(t,
		ccc52aaHash("production", "TypeError", []fingerprint.StackFrame{{File: "/assets/index-*.js", Function: "a1b2c3", InApp: true}}),
		compute("a1b2c3"))
}

// TestNormalizeHashInput_Base64urlMissRate is decision 42's measurement, re-run on every test run:
// 200,000 seeded random 8-char base64url names (Rollup's default `hashCharacters: 'base64'`
// alphabet, `[A-Za-z0-9_-]`), each as `chunk-<name>.js` through NormalizeHashInput, and the share
// that does NOT come out as `chunk-*.js` is the residual -- a chunk whose identity would still
// split per build. The rate is logged (status/be.md quotes it) and bounded; the bound is a
// regression guard for the rule, not the decision's target, and the breakdown says which clause
// of the rule each miss failed on so a change to the bound is a deliberate one.
func TestNormalizeHashInput_Base64urlMissRate(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	const n = 200_000
	r := rand.New(rand.NewSource(20260915))
	miss, fewUppers, vowelRun, dash, other := 0, 0, 0, 0, 0
	var samples []string
	for i := 0; i < n; i++ {
		b := make([]byte, 8)
		for j := range b {
			b[j] = alphabet[r.Intn(len(alphabet))]
		}
		name := string(b)
		if fingerprint.NormalizeHashInput("chunk-"+name+".js") == "chunk-*.js" {
			continue
		}
		miss++
		switch {
		case !strings.ContainsAny(name, "0123456789") && hasVowelRunOver2(name):
			vowelRun++
		case !strings.ContainsAny(name, "0123456789"):
			fewUppers++
		case strings.Contains(name, "-"):
			dash++
		default:
			other++
		}
		if len(samples) < 12 {
			samples = append(samples, name)
		}
	}
	rate := 100 * float64(miss) / n
	t.Logf("decision 42 miss rate: %d / %d = %.2f%% -- digitless with a vowel run over 2: %d (%.2f%%); digitless with fewer than 2 upper-case letters after position 0: %d (%.2f%%); digit-bearing with a `-` after a piece that is not hash-shaped (a lower-case letter or word): %d (%.2f%%); other: %d; e.g. %s",
		miss, n, rate, vowelRun, 100*float64(vowelRun)/n, fewUppers, 100*float64(fewUppers)/n, dash, 100*float64(dash)/n, other, strings.Join(samples, " "))
	assert.Equal(t, 0, other, "every miss must be explained by one of the rule's named clauses")
	assert.LessOrEqual(t, rate, 4.0, "decision 42: the residual on Rollup's default names")
}

func hasVowelRunOver2(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		if strings.IndexByte("aeiouAEIOU", s[i]) >= 0 {
			run++
			if run > 2 {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}
