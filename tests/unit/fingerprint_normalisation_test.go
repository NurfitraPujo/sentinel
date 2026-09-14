package unit

import (
	"crypto/sha256"
	"encoding/hex"
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
// `1234-abcdef.js`, and the edges the rule's wording leaves to the implementer, stated as cases
// so the ratification is on them. Each row is asserted TWICE below: on
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
	{"the basename is the basename rule's alone (a bare 20-char id with no extension)", "/static/AbC123xyz_-9pQrStUvW", "/static/AbC123xyz_-*", "basename"},
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
	{"a dash inside the candidate splits it: only the last piece is examined", "x-a1b2c3-d4e5f6.js", "x-a1b2c3-*.js", "token"},
	{"consequence of the all-hex clause, stated: a 6-letter hex word strips", "decade.js", "*.js", "token"},
	{"function is never touched (asserted through Compute only; the path here is a control)", "/assets/x.js", "/assets/x.js", "none"},
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
