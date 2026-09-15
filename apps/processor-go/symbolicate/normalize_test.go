package symbolicate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// sharedFixturePath is tests/fixtures/sourcemaps/path-normalisation.json (contract §4.4 step 1,
// decision 22): the ONE file the Go and TypeScript normalisers both pin to. The TypeScript twin
// (apps/dashboard-web's upload route) reads the same file, so the two cannot drift.
const sharedFixturePath = "../../../tests/fixtures/sourcemaps/path-normalisation.json"

type normalisationCase struct {
	In  string `json:"in"`
	Out string `json:"out"`
}

func loadNormalisationFixture(t *testing.T) []normalisationCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(sharedFixturePath))
	if err != nil {
		t.Fatalf("read shared fixture: %v", err)
	}
	var cases []normalisationCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("decode shared fixture: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("shared fixture is empty")
	}
	return cases
}

// TestNormalizePath_SharedFixture is table-driven over the file, every row, read at test time --
// not a copy of it -- so an edit to the fixture is an edit to this test.
func TestNormalizePath_SharedFixture(t *testing.T) {
	for _, c := range loadNormalisationFixture(t) {
		t.Run(c.In, func(t *testing.T) {
			if got := NormalizePath(c.In); got != c.Out {
				t.Fatalf("NormalizePath(%q) = %q, want %q", c.In, got, c.Out)
			}
		})
	}
}

// TestNormalizePath_FixtureCoversContractCases pins that the shared file carries every case §4.4
// step 1 lists by name, so neither side can quietly drop one: the seven byte-identical shapes, a
// port, host-with-no-path, an upper-case scheme, a query containing a slash, a fragment, a query
// AND a fragment, and a cache-buster on an absolute URL.
func TestNormalizePath_FixtureCoversContractCases(t *testing.T) {
	cases := loadNormalisationFixture(t)
	have := map[string]string{}
	for _, c := range cases {
		have[c.In] = c.Out
	}
	required := map[string]string{
		"webpack://ns/./src/x.ts":           "webpack://ns/./src/x.ts",
		"webpack:///./src/x.ts":             "webpack:///./src/x.ts",
		"file:///Users/a/x.js":              "file:///Users/a/x.js",
		"blob:https://app.example.com/uuid": "blob:https://app.example.com/uuid",
		"chrome-extension://abc/x.js":       "chrome-extension://abc/x.js",
		"/assets/x.js":                      "/assets/x.js",
		"x.js":                              "x.js",
		"https://app.example.com:8443/a.js": "/a.js",
		"https://app.example.com":           "/",
		"https://app.example.com?x=1":       "/",
		"HTTPS://app.example.com/a.js":      "/a.js",
		"/a.js?x=/b":                        "/a.js",
		"/a.js#L1":                          "/a.js",
		"/a.js?x=1#L1":                      "/a.js",
		"https://app.example.com/assets/index-3f2a1c.js?v=1725100000": "/assets/index-3f2a1c.js",
	}
	for in, out := range required {
		got, ok := have[in]
		if !ok {
			t.Errorf("shared fixture is missing the contract case %q", in)
			continue
		}
		if got != out {
			t.Errorf("shared fixture has %q -> %q, the contract says %q", in, got, out)
		}
	}
}

// TestNormalizePath_Idempotent: applying the rule to its own output changes nothing, which is
// what lets the upload route store a normalised path and the processor normalise a frame and
// meet on the unique index.
func TestNormalizePath_Idempotent(t *testing.T) {
	for _, c := range loadNormalisationFixture(t) {
		if again := NormalizePath(c.Out); again != c.Out {
			t.Errorf("NormalizePath(%q) = %q; the rule is not idempotent on its own output", c.Out, again)
		}
	}
}
