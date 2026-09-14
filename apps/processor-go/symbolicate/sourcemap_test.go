package symbolicate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureMapPath = "testdata/vite/dist/assets/index-d_kdqh_W.js.map"
	fixtureSource  = "../../src/main.ts"
)

func readFixtureMap(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(fixtureMapPath))
	if err != nil {
		t.Fatalf("read fixture map: %v", err)
	}
	return raw
}

// TestParse_DropsSourcesContent is decision 35: the fixture map carries sourcesContent (the
// tenant's source), and nothing of it survives the parse -- not in the struct, not reachable.
func TestParse_DropsSourcesContent(t *testing.T) {
	raw := readFixtureMap(t)
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe["sourcesContent"]; !ok {
		t.Fatal("fixture map must carry sourcesContent for this test to mean anything")
	}
	pm, err := parseSourcemap(raw)
	if err != nil {
		t.Fatal(err)
	}
	// The parsed form holds exactly: sources, sourceRoot, names, lines. Round-trip the source text
	// through every string the parsed map retains and prove none of it is the file's content.
	src, _ := os.ReadFile(filepath.FromSlash("testdata/vite/src/main.ts"))
	needle := "export function subtotal" // a line of the source, present in sourcesContent
	if !strings.Contains(string(src), needle) {
		t.Fatal("fixture source changed; pick another needle")
	}
	for _, s := range append(append([]string{pm.sourceRoot}, pm.sources...), pm.names...) {
		if strings.Contains(s, needle) {
			t.Fatalf("parsed map retained source content in %q", s)
		}
	}
	if len(pm.sources) != 1 || pm.sources[0] != fixtureSource {
		t.Fatalf("sources = %v, want [%s]", pm.sources, fixtureSource)
	}
	if len(pm.lines) != 1 {
		t.Fatalf("fixture output is one generated line; parsed %d", len(pm.lines))
	}
}

func TestParse_Rejects(t *testing.T) {
	cases := map[string]string{
		"not json":              `{`,
		"version 2":             `{"version":2,"sources":[],"names":[],"mappings":"AAAA"}`,
		"index map":             `{"version":3,"sections":[]}`,
		"missing mappings":      `{"version":3,"sources":[],"names":[]}`,
		"malformed vlq byte":    `{"version":3,"sources":["a"],"names":[],"mappings":"AAAA,!!!!"}`,
		"vlq ends mid value":    `{"version":3,"sources":["a"],"names":[],"mappings":"AAAg"}`,
		"two-field segment":     `{"version":3,"sources":["a"],"names":[],"mappings":"AA"}`,
		"three-field segment":   `{"version":3,"sources":["a"],"names":[],"mappings":"AAA"}`,
		"six-field segment":     `{"version":3,"sources":["a"],"names":[],"mappings":"AAAAAA"}`,
		"vlq overflows int32":   `{"version":3,"sources":["a"],"names":[],"mappings":"AAAA,gggggggggA"}`,
		"mappings not a string": `{"version":3,"sources":["a"],"names":[],"mappings":5}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSourcemap([]byte(raw)); err == nil {
				t.Fatalf("parse accepted %s", raw)
			}
		})
	}
}

// TestDecodeVLQ pins the alphabet and the sign bit on values decoded by hand: A=0, C=1, D=-1,
// K=5, O=7, S=9, gB = (32|0) then 1 → 1<<5 = 32 → value 16, hB = 1 | (1<<5) → 33 → sign bit → -16.
func TestDecodeVLQ(t *testing.T) {
	var out [5]int32
	cases := []struct {
		enc  string
		want []int32
	}{
		{"A", []int32{0}},
		{"C", []int32{1}},
		{"D", []int32{-1}},
		{"AAKO", []int32{0, 0, 5, 7}},
		{"SAASA", []int32{9, 0, 0, 9, 0}},
		{"gB", []int32{16}},
		{"hB", []int32{-16}},
		{"+A", []int32{15}},  // '+'=62: 62&31=30 with the continuation bit, then A=0 → 30 → sign 0 → 15
		{"/A", []int32{-15}}, // '/'=63: 31 with the continuation bit, then A → 31 → sign 1 → -15
	}
	for _, c := range cases {
		n, err := decodeVLQ(c.enc, out[:])
		if err != nil {
			t.Fatalf("%q: %v", c.enc, err)
		}
		if n != len(c.want) {
			t.Fatalf("%q: %d fields, want %d", c.enc, n, len(c.want))
		}
		for i, w := range c.want {
			if out[i] != w {
				t.Fatalf("%q: field %d = %d, want %d", c.enc, i, out[i], w)
			}
		}
	}
}

// TestResolve_Convention pins the wire↔map convention on a map written by hand:
// "AAAA;EACA,GAAC" is line 1: segment at genCol 0 → (src 0, line 0, col 0); line 2: segment at
// genCol 2 → (src 0, line 1, col 0), then genCol 5 → (line 1, col 1). So wire (2, 6) is
// generated column 5 → original (2, 2), and wire (2, 3) → genCol 2 → original (2, 1).
func TestResolve_Convention(t *testing.T) {
	pm, err := parseSourcemap([]byte(`{"version":3,"sources":["s.ts"],"names":[],"mappings":"AAAA;EACA,GAAC"}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		line, col   int32
		ok          bool
		oline, ocol int32
	}{
		{1, 1, true, 1, 1},
		{1, 999, true, 1, 1}, // nearest at-or-before on the line
		{2, 1, false, 0, 0},  // before the first segment on line 2: no mapping
		{2, 2, false, 0, 0},
		{2, 3, true, 2, 1},
		{2, 5, true, 2, 1}, // between the two segments → the earlier one
		{2, 6, true, 2, 2},
		{2, 7, true, 2, 2},
		{3, 1, false, 0, 0}, // a line the map does not have -- never the line before it
		{0, 1, false, 0, 0},
		{1, 0, false, 0, 0},
	}
	for _, c := range cases {
		r, ok := pm.resolve(c.line, c.col)
		if ok != c.ok {
			t.Fatalf("(%d,%d): ok=%v want %v", c.line, c.col, ok, c.ok)
		}
		if ok && (r.line != c.oline || r.column != c.ocol || r.file != "s.ts") {
			t.Fatalf("(%d,%d): got %s:%d:%d want s.ts:%d:%d", c.line, c.col, r.file, r.line, r.column, c.oline, c.ocol)
		}
	}
}

// TestJoinSourceRoot is §4.4 step 6's identity rule with the contract's three examples plus the
// one-slash boundary cases.
func TestJoinSourceRoot(t *testing.T) {
	cases := []struct{ root, source, want string }{
		{"", "../../src/x.ts", "../../src/x.ts"},
		{"webpack:///", "./src/app/page.tsx", "webpack:///./src/app/page.tsx"},
		{"src", "../lib/x.ts", "src/../lib/x.ts"},
		{"src/", "x.ts", "src/x.ts"},
		{"src", "/x.ts", "src/x.ts"},
		{"src/", "/x.ts", "src/x.ts"},
		{"https://cdn.example.com/app", "./main.ts", "https://cdn.example.com/app/./main.ts"},
	}
	for _, c := range cases {
		if got := JoinSourceRoot(c.root, c.source); got != c.want {
			t.Errorf("JoinSourceRoot(%q, %q) = %q, want %q", c.root, c.source, got, c.want)
		}
	}
}
