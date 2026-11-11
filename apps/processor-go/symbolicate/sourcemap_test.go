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

	// The real Turbopack index map and empty stub (testdata/turbopack-index/README.md records the
	// build, sizes and sha256s; sentinel-sourcemaps-index-maps §4.3, §4.6, D8).
	indexMapPath  = "testdata/turbopack-index/chunks/[turbopack]_runtime.js.map"
	indexStubPath = "testdata/turbopack-index/app/page.js.map"
)

func readTestdata(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}

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

// TestParse_Rejects is the parse-error table: every row is a `map_error` at the caller. The
// index-map rows are sentinel-sourcemaps-index-maps §4.3 step 4 and D5/D6 (flow P8): a `url`
// section, a nested inner map, offsets that are equal, descending or negative, an inner map
// without `mappings`, and a top-level map carrying both `mappings` and `sections`. A zero-section
// index map is NOT here -- it is a valid parse (TestParse_IndexMapZeroSections).
func TestParse_Rejects(t *testing.T) {
	const inner = `{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA"}`
	cases := map[string]string{
		"not json":                         `{`,
		"version 2":                        `{"version":2,"sources":[],"names":[],"mappings":"AAAA"}`,
		"missing mappings":                 `{"version":3,"sources":[],"names":[]}`,
		"index map: url section":           `{"version":3,"sections":[{"offset":{"line":0,"column":0},"url":"other.map"}]}`,
		"index map: url and map section":   `{"version":3,"sections":[{"offset":{"line":0,"column":0},"url":"other.map","map":` + inner + `}]}`,
		"index map: section without map":   `{"version":3,"sections":[{"offset":{"line":0,"column":0}}]}`,
		"index map: nested inner map":      `{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":{"version":3,"sections":[]}}]}`,
		"index map: nested with mappings":  `{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA","sections":[]}}]}`,
		"index map: equal offsets":         `{"version":3,"sections":[{"offset":{"line":5,"column":0},"map":` + inner + `},{"offset":{"line":5,"column":0},"map":` + inner + `}]}`,
		"index map: descending line":       `{"version":3,"sections":[{"offset":{"line":5,"column":0},"map":` + inner + `},{"offset":{"line":4,"column":0},"map":` + inner + `}]}`,
		"index map: descending column":     `{"version":3,"sections":[{"offset":{"line":5,"column":40},"map":` + inner + `},{"offset":{"line":5,"column":0},"map":` + inner + `}]}`,
		"index map: negative line":         `{"version":3,"sections":[{"offset":{"line":-1,"column":0},"map":` + inner + `}]}`,
		"index map: negative column":       `{"version":3,"sections":[{"offset":{"line":0,"column":-1},"map":` + inner + `}]}`,
		"index map: inner map no mappings": `{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":{"version":3,"sources":["a.ts"],"names":[]}}]}`,
		"index map: inner map version 2":   `{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":{"version":2,"sources":["a.ts"],"names":[],"mappings":"AAAA"}}]}`,
		"index map: inner map bad vlq":     `{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA,!!!!"}}]}`,
		"index map: sections not an array": `{"version":3,"sections":5}`,
		"both mappings and sections":       `{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA","sections":[]}`,
		"both mappings and one section":    `{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA","sections":[{"offset":{"line":0,"column":0},"map":` + inner + `}]}`,
		"malformed vlq byte":               `{"version":3,"sources":["a"],"names":[],"mappings":"AAAA,!!!!"}`,
		"vlq ends mid value":               `{"version":3,"sources":["a"],"names":[],"mappings":"AAAg"}`,
		"two-field segment":                `{"version":3,"sources":["a"],"names":[],"mappings":"AA"}`,
		"three-field segment":              `{"version":3,"sources":["a"],"names":[],"mappings":"AAA"}`,
		"six-field segment":                `{"version":3,"sources":["a"],"names":[],"mappings":"AAAAAA"}`,
		"vlq overflows int32":              `{"version":3,"sources":["a"],"names":[],"mappings":"AAAA,gggggggggA"}`,
		"mappings not a string":            `{"version":3,"sources":["a"],"names":[],"mappings":5}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseSourcemap([]byte(raw)); err == nil {
				t.Fatalf("parse accepted %s", raw)
			}
		})
	}
}

// TestParse_IndexMapSections parses the committed real Turbopack runtime map (index-maps §4.3):
// five sections at the 0-based offsets the contract names, each with the contract's one source,
// no sourceRoot and empty names, decoded through the same decoder as a flat map -- section 4's
// inner map has the 201 lines the contract counts. And nothing of any section's sourcesContent
// survives (decision 35 per section).
func TestParse_IndexMapSections(t *testing.T) {
	raw := readTestdata(t, indexMapPath)
	pm, err := parseSourcemap(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !pm.index {
		t.Fatal("the runtime map is the sections form; parsed as flat")
	}
	want := []struct {
		line, column int32
		source       string
		lines        int
	}{
		{3, 0, "turbopack:///[turbopack]/shared/runtime-utils.ts", 467},
		{470, 0, "turbopack:///[turbopack]/shared-node/base-externals-utils.ts", 41},
		{511, 0, "turbopack:///[turbopack]/shared-node/node-externals-utils.ts", 21},
		{532, 0, "turbopack:///[turbopack]/shared-node/node-wasm-utils.ts", 21},
		{553, 0, "turbopack:///[turbopack]/nodejs/runtime.ts", 201},
	}
	if len(pm.sections) != len(want) {
		t.Fatalf("parsed %d sections, want %d", len(pm.sections), len(want))
	}
	for i, w := range want {
		s := pm.sections[i]
		if s.line != w.line || s.column != w.column {
			t.Errorf("section %d offset = (%d,%d), want (%d,%d)", i, s.line, s.column, w.line, w.column)
		}
		if len(s.pm.sources) != 1 || s.pm.sources[0] != w.source {
			t.Errorf("section %d sources = %v, want [%s]", i, s.pm.sources, w.source)
		}
		if s.pm.sourceRoot != "" || len(s.pm.names) != 0 {
			t.Errorf("section %d: sourceRoot %q names %v; want none", i, s.pm.sourceRoot, s.pm.names)
		}
		if len(s.pm.lines) != w.lines {
			t.Errorf("section %d decoded %d generated lines, want %d", i, len(s.pm.lines), w.lines)
		}
		if s.pm.index || s.pm.sections != nil {
			t.Errorf("section %d's map must be flat", i)
		}
	}
	// Each section carries sourcesContent in the file; the parsed form retains none of it.
	var probe struct {
		Sections []struct {
			Map map[string]json.RawMessage `json:"map"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe.Sections[4].Map["sourcesContent"]; !ok {
		t.Fatal("fixture sections must carry sourcesContent for this check to mean anything")
	}
	const needle = "const chunkCache = new Map()" // runtime.ts's line 77, present in section 4's sourcesContent
	for i, s := range pm.sections {
		for _, str := range append(append([]string{s.pm.sourceRoot}, s.pm.sources...), s.pm.names...) {
			if strings.Contains(str, needle) {
				t.Fatalf("section %d retained source content in %q", i, str)
			}
		}
	}
	// The top-level map's own arrays are empty: sources/names/lines are per section (§3).
	if len(pm.sources) != 0 || len(pm.names) != 0 || len(pm.lines) != 0 {
		t.Fatalf("index map must carry no top-level sources/names/lines: %d/%d/%d", len(pm.sources), len(pm.names), len(pm.lines))
	}
}

// TestParse_IndexMapZeroSections: the committed real stub (`sections: []`, a `debugId`, `sources:
// null`) is a VALID parse with zero sections (index-maps §4.3 step 4, flow P9) -- no error, and
// every position is no_section. The route never stores one (D12); the parser has no undefined
// case regardless.
func TestParse_IndexMapZeroSections(t *testing.T) {
	pm, err := parseSourcemap(readTestdata(t, indexStubPath))
	if err != nil {
		t.Fatalf("a zero-section index map is a valid parse: %v", err)
	}
	if !pm.index || len(pm.sections) != 0 {
		t.Fatalf("index=%v sections=%d; want an index map with zero sections", pm.index, len(pm.sections))
	}
	for _, pos := range [][2]int32{{1, 1}, {600, 12}, {1 << 20, 1}} {
		if _, st := pm.resolveFrame(pos[0], pos[1]); st != resolveNoSection {
			t.Fatalf("(%d,%d): status %d, want no section", pos[0], pos[1], st)
		}
	}
	// The bare shape without the Sentry keys parses the same way.
	if pm, err := parseSourcemap([]byte(`{"version":3,"sources":[],"sections":[]}`)); err != nil || !pm.index || len(pm.sections) != 0 {
		t.Fatalf("bare stub: %v %+v", err, pm)
	}
}

// TestSection pins the section pick on its own: greatest offset <= (gl, gc) lexicographically over
// offsets (3,0), (5,10), (5,40), (9,0), with the column comparison mattering only on the
// offset's line. The binary search's boundaries are the cases a linear scan gets right by luck.
func TestSection(t *testing.T) {
	const inner = `{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA"}`
	pm, err := parseSourcemap([]byte(`{"version":3,"sections":[` +
		`{"offset":{"line":3,"column":0},"map":` + inner + `},` +
		`{"offset":{"line":5,"column":10},"map":` + inner + `},` +
		`{"offset":{"line":5,"column":40},"map":` + inner + `},` +
		`{"offset":{"line":9,"column":0},"map":` + inner + `}]}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		gl, gc int32
		want   int
		ok     bool
	}{
		{0, 0, 0, false},
		{2, 999, 0, false}, // the line before the first offset, any column
		{3, 0, 0, true},    // exactly the first offset
		{3, 5, 0, true},
		{4, 0, 0, true},
		{5, 0, 0, true},  // on section 1's line but before its column: still section 0
		{5, 9, 0, true},  //
		{5, 10, 1, true}, // exactly (5,10)
		{5, 39, 1, true},
		{5, 40, 2, true}, // exactly (5,40)
		{5, 41, 2, true},
		{6, 0, 2, true}, // the line after a column-offset section: the column no longer matters
		{8, 999, 2, true},
		{9, 0, 3, true},
		{1000, 0, 3, true},
	}
	for _, c := range cases {
		got, ok := pm.section(c.gl, c.gc)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("section(%d,%d) = (%d,%v), want (%d,%v)", c.gl, c.gc, got, ok, c.want, c.ok)
		}
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
