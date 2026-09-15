package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
)

const (
	MaxAppFrames = 3
)

type FingerprintConfig struct {
	CustomFingerprint string
	// Environment is folded into the hash input (sentinel-project-environments §4.9) so the
	// fingerprint VALUE itself is environment-distinct, and idx_issues_fingerprint — a
	// fingerprint-only, non-unique index from 1716508800_init.sql — cannot be read as
	// cross-environment identity by a future query. It is defence in depth, not the mechanism:
	// the mechanism is the UNIQUE (project_id, environment, fingerprint) constraint.
	Environment string
	ErrorClass  string
	Stacktrace  []StackFrame
	// Platform is the event's platform (sentinel-sourcemaps §4.4, "Fingerprint input
	// normalisation"): when it is in NormalisePlatforms, every frame's File is passed through
	// NormalizeHashInput — symbolicate.NormalizePath, then the basename and directory rules —
	// INSIDE Compute only, so a build-hash-renamed chunk hashes to the same identity across
	// builds. The stored frame is untouched. Outside the set (Go, Python, an empty Platform) the
	// hash input is byte-identical to what it was before the field existed, which is why every
	// pre-feature issue of those platforms keeps its fingerprint (decision 7 of
	// sentinel-project-environments had to guard a migration against exactly that kind of split).
	Platform string
	// NormalisePlatforms is the gate set — symbolicate.Config.Platforms, i.e. SYMBOLICATE_PLATFORMS
	// (unset = {javascript, browser}; set and empty = off, decision 27's kill switch turns the
	// normalisation off with the stage). Nil or empty means no platform is normalised; callers
	// that want the contract default pass symbolicate.DefaultPlatforms explicitly.
	NormalisePlatforms []string
}

type StackFrame struct {
	File     string
	Line     int32
	Function string
	InApp    bool
}

func Compute(cfg FingerprintConfig) string {
	var input string
	// §4.9: the CustomFingerprint early return is deliberately NOT environment-prefixed. Prefixing a
	// client-chosen fingerprint would change identity for exactly the clients who chose it, which is
	// the one thing a custom fingerprint exists to prevent. Two environments sending the SAME custom
	// fingerprint still get two issue rows, because the unique key is what splits identity.
	if cfg.CustomFingerprint != "" {
		return cfg.CustomFingerprint
	}

	// §4.4: the normalisation applies to the hash input of every frame Compute reads — in_app or
	// fallback, symbolicated or not — and to nothing else: never Function, never the stored frame.
	normalise := cfg.normalises()
	hashFile := func(frame StackFrame) string {
		if normalise {
			return NormalizeHashInput(frame.File)
		}
		return frame.File
	}

	var appFrames []string
	for _, frame := range cfg.Stacktrace {
		if frame.InApp {
			appFrames = append(appFrames, fmt.Sprintf("%s:%s", hashFile(frame), frame.Function))
			if len(appFrames) >= MaxAppFrames {
				break
			}
		}
	}

	// P4-3 / VERIFIED_STATE.md S11: when a client marks no frames in_app, the hash input used to
	// degenerate to ErrorClass alone, collapsing every error of a class in a project into ONE issue.
	// Clients that do not set in_app (anything other than the Go SDK today) still deserve grouping, so
	// fall back to the top frames regardless of the flag. Frames are still capped at MaxAppFrames, so
	// the fingerprint stays stable as deeper stack detail changes.
	if len(appFrames) == 0 {
		for _, frame := range cfg.Stacktrace {
			appFrames = append(appFrames, fmt.Sprintf("%s:%s", hashFile(frame), frame.Function))
			if len(appFrames) >= MaxAppFrames {
				break
			}
		}
	}

	input = cfg.Environment + "|" + cfg.ErrorClass
	if len(appFrames) > 0 {
		input += "|" + strings.Join(appFrames, "|")
	}

	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])[:16]
}

// normalises is the platform gate: Platform ∈ NormalisePlatforms, exact string match on the
// lowercase, trimmed values symbolicate.ParsePlatforms produced (the proto admits only
// `^[a-z0-9]+$`, so no case-folding is needed on the event side).
func (cfg FingerprintConfig) normalises() bool {
	for _, p := range cfg.NormalisePlatforms {
		if p == cfg.Platform {
			return true
		}
	}
	return false
}

// NormalizeHashInput is sentinel-sourcemaps §4.4's "Fingerprint input normalisation", applied to a
// frame's File inside Compute for the platforms in the gate set: symbolicate.NormalizePath first
// (step 1's scheme+host / query / fragment rule, so `https://app.example.com/assets/x.js` and
// `/assets/x.js` hash alike), then the directory rule on every directory segment and the basename
// rule on the last one. Everything the rules do not name is byte-identical. Exported so the
// fixture table (decision 6's deliverable, status/be.md) is asserted on the function AND through
// Compute.
//
// The rules, in the contract's words (decisions 6 and 29):
//
//   - Basename rule: in the last path segment, a separator `-` or `.` followed by a hash token
//     immediately before the extension(s) is replaced by `-*` / `.*`; a bare-hash basename with no
//     separator has its whole stem as the token (`8f7a6b5c3d2e1f0a.js` → `*.js`). Only the ONE
//     token immediately before the extension run is examined (`vendor-react-a3f9b2.js` →
//     `vendor-react-*.js`; `react` is never looked at).
//   - Hash token (decisions 41 and 42): 6–64 chars of [A-Za-z0-9_-] that is not a word — it
//     contains at least one digit, OR it is entirely [0-9a-f], OR it is digitless with ≥ 2
//     upper-case letters after position 0 and no vowel run longer than 2 (Rollup's default
//     base64url hashes: `CuqmStVf`, `oQLsOyVD`; `d_kdqh_W` by its inner `_`). The separator
//     search skips a `-` that sits INSIDE such a token unconditionally (`cart-oQ-sOyVD.js` →
//     `cart-*.js`, `chunk-Ab-xyz9Qw.js` → `chunk-*.js`, `chunk--rv42314.js` → `chunk-*.js`):
//     the candidate widens back across every `-` whose preceding piece is hash-shaped and the
//     widest candidate that is a token wins, so a word (`react`, `main`) or a numeric chunk id
//     (`1234`) is never absorbed.
//   - Extension run: the trailing sequence of `.<ext>` segments where each ext is 1–8 chars of
//     [a-z] with no digit (`.js`, `.chunk.js`, `.min.js`, `.js.map`); `main.a1b2c3d4.chunk.js` →
//     `main.*.chunk.js`.
//   - Directory rule: a whole DIRECTORY segment (every segment but the last — the basename is the
//     basename rule's alone) of ≥ 16 chars of [A-Za-z0-9_-] containing a digit is replaced by `*`
//     (`/_next/static/AbC123xyz_-9pQrStUv/_buildManifest.js` → `/_next/static/*/_buildManifest.js`).
func NormalizeHashInput(file string) string {
	file = symbolicate.NormalizePath(file)
	segments := strings.Split(file, "/")
	last := len(segments) - 1
	for i := 0; i < last; i++ {
		if isBuildIDSegment(segments[i]) {
			segments[i] = "*"
		}
	}
	segments[last] = normalizeBasename(segments[last])
	return strings.Join(segments, "/")
}

// normalizeBasename is the basename rule on one path segment.
func normalizeBasename(base string) string {
	stem, ext := splitExtensionRun(base)
	if stem == "" {
		return base
	}
	// The candidate starts as the piece after the LAST separator, then widens back across `-`
	// separators only (a `.` is never inside a token) for as long as the piece being crossed is
	// hash-shaped; the widest candidate that is a token wins (decision 42: the `-` inside a
	// base64url token is skipped unconditionally -- whether or not the last piece happens to be a
	// token on its own, so `chunk-Ab-xyz9Qw.js` and `cart-oQ-sOyVD.js` both strip to
	// `chunk-*.js` / `cart-*.js`, and `chunk-EF--2AAw.js` crosses an intermediate candidate that
	// is not one). A lower-case word (`react`, `main`, `http`) or a numeric chunk id (`1234`) is
	// never hash-shaped, so `vendor-react-a3f9b2.js` → `vendor-react-*.js`, `1234-abcdef.js` →
	// `1234-*.js` and `main-a1b2c.js` stay as §4.4 states.
	sep := strings.LastIndexAny(stem, "-.")
	tokenSep, found := sep, isHashToken(stem[sep+1:])
	for sep >= 0 && stem[sep] == '-' {
		prev := strings.LastIndexAny(stem[:sep], "-.")
		if !isHashFragment(stem[prev+1 : sep]) {
			break
		}
		sep = prev
		if isHashToken(stem[sep+1:]) {
			tokenSep, found = sep, true
		}
	}
	if !found {
		return base
	}
	return stem[:tokenSep+1] + "*" + ext
}

// isHashFragment says whether a `-`-separated piece may be the front part of one hash token whose
// `-` the separator search must skip (decision 42): empty (a leading `-`, `chunk--rv42314.js`),
// or carrying an upper-case letter (`A`, `Ab`, `oQ`, `CuqmSt`), or carrying a digit without being
// all digits (`a1b2c3`, `rv42`). A word is never a fragment, and neither is a vowel-less one
// (`http`, `css`, `x`) nor a numeric chunk id (`1234`): `http-Bx-3kQz9.js` → `http-*.js`,
// `1234-abcdef.js` → `1234-*.js`, `vendor-react-a3f9b2.js` → `vendor-react-*.js`.
func isHashFragment(s string) bool {
	hasDigit, allDigits := false, true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
			continue
		case c >= 'A' && c <= 'Z':
			return true
		}
		allDigits = false
	}
	if allDigits {
		return len(s) == 0
	}
	return hasDigit
}

func isVowel(c byte) bool {
	switch c {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return true
	}
	return false
}

// splitExtensionRun splits a basename into its stem and the trailing run of `.<ext>` segments,
// each ext 1–8 chars of [a-z]. `main.a1b2c3d4.chunk.js` → (`main.a1b2c3d4`, `.chunk.js`);
// `README` → (`README`, an empty run). A leading dot-file (`.env`) has an empty stem and is left alone.
func splitExtensionRun(base string) (stem, ext string) {
	stem = base
	for {
		i := strings.LastIndexByte(stem, '.')
		if i < 0 {
			return stem, ext
		}
		candidate := stem[i+1:]
		if !isExtension(candidate) {
			return stem, ext
		}
		ext = stem[i:] + ext
		stem = stem[:i]
	}
}

func isExtension(s string) bool {
	if len(s) < 1 || len(s) > 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

// isHashToken is the contract's hash-token definition (§4.4, decisions 41 and 42): 6–64 chars of
// [A-Za-z0-9_-], and not a word — at least one digit, OR entirely [0-9a-f], OR digitless with
// ≥ 2 upper-case letters after position 0 AND no vowel run longer than 2 (decision 42's measured
// reading of decision 41's "mixed-case": Rollup's default `hashCharacters: 'base64'` names
// `CuqmStVf`, `oQLsOyVD`, `FksjBCVK`, `LVliPaoS`). The contract's own negatives are what fix the
// reading: `Header` has no upper-case letter after position 0, `getUser` and `_buildManifest`
// (the directory rule's example) have one, `useAuth` has one and the run `eAu`. `parseHTML`
// strips -- an accepted false positive of the same class as `decade.js` under the all-hex clause.
//
// One digitless shape the case count refuses is kept from decision 41 because §4.4 names it as a
// hash: an inner `_` in a mixed-case token (`d_kdqh_W` -- a single upper-case letter after
// position 0, but snake and camel do not mix in a word). Both clauses still require no vowel run
// over 2.
func isHashToken(s string) bool {
	if len(s) < 6 || len(s) > 64 {
		return false
	}
	hasDigit, allHex, hasLower, innerUnderscore, longVowelRun := false, true, false, false, false
	uppersAfterFirst, vowelRun := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'a' && c <= 'f':
			hasLower = true
		case c >= 'g' && c <= 'z':
			allHex = false
			hasLower = true
		case c >= 'A' && c <= 'Z':
			allHex = false
			if i > 0 {
				uppersAfterFirst++
			}
		case c == '_':
			allHex = false
			innerUnderscore = innerUnderscore || i > 0
		case c == '-':
			allHex = false
		default:
			return false
		}
		if isVowel(c) {
			vowelRun++
		} else {
			vowelRun = 0
		}
		if vowelRun > 2 {
			longVowelRun = true
		}
	}
	if hasDigit || allHex {
		return true
	}
	if longVowelRun {
		return false
	}
	return uppersAfterFirst >= 2 || (innerUnderscore && hasLower && uppersAfterFirst >= 1)
}

// isBuildIDSegment is the directory rule's predicate: ≥ 16 chars of [A-Za-z0-9_-] with a digit.
func isBuildIDSegment(s string) bool {
	if len(s) < 16 {
		return false
	}
	hasDigit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == '-':
		default:
			return false
		}
	}
	return hasDigit
}
