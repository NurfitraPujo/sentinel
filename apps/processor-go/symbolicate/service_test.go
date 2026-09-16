package symbolicate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- test doubles -------------------------------------------------------------------------------

// stubStore is a Store over an in-memory set of rows keyed by the lookup triple, recording every
// call so a test can assert the query was issued once (§4.4 step 3) and with the NORMALISED path.
type stubStore struct {
	mu    sync.Mutex
	rows  map[negKey]*Sourcemap
	calls []negKey
	err   error
}

func newStubStore() *stubStore { return &stubStore{rows: map[negKey]*Sourcemap{}} }

func (s *stubStore) add(row *Sourcemap) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[negKey{row.ProjectID, row.ReleaseVersion, row.MinifiedPath}] = row
}

func (s *stubStore) Lookup(_ context.Context, projectID, releaseVersion, minifiedPath string) (*Sourcemap, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := negKey{projectID, releaseVersion, minifiedPath}
	s.calls = append(s.calls, k)
	if s.err != nil {
		return nil, s.err
	}
	return s.rows[k], nil
}

func (s *stubStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// stubFetcher serves bytes by storage key, counts calls, and can be made slower than the deadline
// (decision 37's case) or fail (fetch_error).
type stubFetcher struct {
	objects map[string][]byte
	delay   time.Duration
	err     error
	calls   atomic.Int32
}

func (f *stubFetcher) Fetch(ctx context.Context, key string) ([]byte, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	raw, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("no object %s", key)
	}
	return raw, nil
}

// recordingObserver counts cache events per kind and keeps the last byte total.
type recordingObserver struct {
	mu     sync.Mutex
	events map[CacheEventKind]int
	bytes  int64
	fetch  int
}

func newRecordingObserver() *recordingObserver {
	return &recordingObserver{events: map[CacheEventKind]int{}}
}

func (o *recordingObserver) CacheEvent(kind CacheEventKind) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events[kind]++
}
func (o *recordingObserver) CacheBytes(n int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bytes = n
}
func (o *recordingObserver) FetchDuration(time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fetch++
}
func (o *recordingObserver) count(kind CacheEventKind) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.events[kind]
}

// fakeClock drives the TTLs without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness is one Service over the doubles with the fixture map seeded as project P, release R,
// path fixturePath.
type harness struct {
	svc     *Service
	store   *stubStore
	fetcher *stubFetcher
	obs     *recordingObserver
	clock   *fakeClock
	cfg     Config
}

const (
	projectA    = "11111111-1111-1111-1111-111111111111"
	releaseR    = "1.2.3"
	fixturePath = "/assets/index-d_kdqh_W.js"
	fixtureKey  = "sourcemaps/p/r/fixture.map"

	// The committed Turbopack-shaped map (testdata/turbopack/README.md): twenty sources, one
	// resolvable segment each at wire (n, 1) → sources[n-1] 1:1, seeded under a browser chunk
	// path of Turbopack's `<16-hex>.js` shape.
	turbopackMapPath = "testdata/turbopack/ac0ebedaba77188c.js.map"
	turbopackPath    = "/_next/static/chunks/ac0ebedaba77188c.js"
	turbopackKey     = "sourcemaps/p/r/turbopack.map"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// rowFor builds a `sourcemaps` row for raw bytes exactly as the upload route would write it.
func rowFor(id, project, release, path, key string, raw []byte) *Sourcemap {
	return &Sourcemap{
		ID: id, ProjectID: project, ReleaseVersion: release, MinifiedPath: path,
		StorageKey: key, SizeBytes: int64(len(raw)), SHA256: sha256Hex(raw),
	}
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Deadline = 2 * time.Second
	if mutate != nil {
		mutate(&cfg)
	}
	h := &harness{
		store:   newStubStore(),
		fetcher: &stubFetcher{objects: map[string][]byte{}},
		obs:     newRecordingObserver(),
		clock:   &fakeClock{t: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)},
		cfg:     cfg,
	}
	svc, err := New(cfg, h.store, h.fetcher, WithObserver(h.obs), withClock(h.clock.now))
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	t.Cleanup(svc.Close)
	return h
}

// seedFixture adds the committed Vite map under (projectA, releaseR, fixturePath).
func (h *harness) seedFixture(t *testing.T) *Sourcemap {
	t.Helper()
	raw := readFixtureMap(t)
	h.fetcher.objects[fixtureKey] = raw
	row := rowFor("map-fixture", projectA, releaseR, fixturePath, fixtureKey, raw)
	h.store.add(row)
	return row
}

// seedTurbopack adds the committed Turbopack-shaped map under (projectA, releaseR, path).
func (h *harness) seedTurbopack(t *testing.T, id, path string) *Sourcemap {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(turbopackMapPath))
	if err != nil {
		t.Fatalf("read turbopack fixture map: %v", err)
	}
	key := turbopackKey + "/" + id
	h.fetcher.objects[key] = raw
	row := rowFor(id, projectA, releaseR, path, key, raw)
	h.store.add(row)
	return row
}

// seedCrafted adds a hand-written map under its own path.
func (h *harness) seedCrafted(id, path, mapJSON string) *Sourcemap {
	raw := []byte(mapJSON)
	key := "sourcemaps/p/r/" + id + ".map"
	h.fetcher.objects[key] = raw
	row := rowFor(id, projectA, releaseR, path, key, raw)
	h.store.add(row)
	return row
}

func frame(file string, line, column int32) Frame {
	return Frame{File: file, Line: line, Column: column, Function: "minified", InApp: true}
}

// waitFills blocks until every background fill has landed (decision 37's "continues in the
// background and fills the cache"). Close is the exported form; the Service keeps working after it.
func (h *harness) waitFills() { h.svc.Close() }

// ---- the fixture table: hand-computed positions (contract §11, be.md task 5) -------------------

// fixtureCases are WIRE positions (1-based line and column, as browsers print and §4.3 carries)
// against the committed Vite map, with the expected original position decoded BY HAND from the
// map's `mappings` before the decoder ran (testdata/vite/README.md records the derivation).
// Generated columns in the map are 0-based, so wire column N is the segment at generated column
// N-1; original line/column come back 1-based. The names are the map's `names` entries; "" means
// the segment carries none and the frame keeps its own function.
var fixtureCases = []struct {
	name         string
	column       int32 // wire column on generated line 1
	wantLine     int32
	wantColumn   int32
	wantFunction string // "" = keeps "minified"
}{
	// segment `AAKO`: genCol 0, origLine +5, origCol +7 → `function` at main.ts 6:8
	{"function keyword of subtotal", 1, 6, 8, ""},
	// segment `SAASA`: genCol 9, origCol +9 → 6:17, name[0] = subtotal
	{"identifier subtotal", 10, 6, 17, "subtotal"},
	// `c(` at genCol 68: applyCoupon at main.ts 14:17, name applyCoupon
	{"identifier applyCoupon", 69, 14, 17, "applyCoupon"},
	// two ADJACENT segments, genCol 58 (`}` closing subtotal → 12:1) and genCol 59 (`function` of
	// applyCoupon → 14:8): wire 59 and 60 must land on different origins, which is what pins the
	// 1-based wire ↔ 0-based map offset -- an off-by-one moves both to the neighbour
	{"closing brace of subtotal", 59, 12, 1, ""},
	{"function keyword of applyCoupon", 60, 14, 8, ""},
	// `new Error(` at genCol 134 → 19:11, no name
	{"new Error in applyCoupon", 135, 19, 11, ""},
	// `e{` at genCol 190: class Checkout at 24:14, name Checkout
	{"class Checkout", 191, 24, 14, "Checkout"},
	// `new RangeError` at genCol 276 → 31:13
	{"new RangeError in pay", 277, 31, 13, ""},
	// `u.pay()` at genCol 380 → 38:13, name checkout
	{"identifier checkout at the call site", 381, 38, 13, "checkout"},
}

func TestSymbolicate_FixtureTable(t *testing.T) {
	h := newHarness(t, nil)
	h.seedFixture(t)
	for _, c := range fixtureCases {
		t.Run(c.name, func(t *testing.T) {
			frames := []Frame{frame(fixturePath, 1, c.column)}
			res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
			if res.Outcomes[0] != OutcomeApplied {
				t.Fatalf("outcome %s, want applied", res.Outcomes[0])
			}
			f := frames[0]
			wantFn := c.wantFunction
			if wantFn == "" {
				wantFn = "minified"
			}
			if f.File != fixtureSource || f.Line != c.wantLine || f.Column != c.wantColumn || f.Function != wantFn {
				t.Fatalf("wire (1,%d) → %s:%d:%d %q; want %s:%d:%d %q",
					c.column, f.File, f.Line, f.Column, f.Function, fixtureSource, c.wantLine, c.wantColumn, wantFn)
			}
			if !f.Symbolicated || f.Raw == nil {
				t.Fatal("applied frame must carry symbolicated=true and raw")
			}
			if f.Raw.File != fixturePath || f.Raw.Line != 1 || f.Raw.Column != c.column || f.Raw.Function != "minified" {
				t.Fatalf("raw = %+v; want the values the SDK sent", *f.Raw)
			}
			// turbopack-grouping §4.2: the rule re-derives in_app from the resolved source; the
			// Vite fixture's `../../src/main.ts` is A13 → true, the same value the SDK sent, and
			// that value is recorded under raw.
			if !f.InApp || !f.Raw.InApp {
				t.Fatalf("in_app = %v, raw.in_app = %v; want true (A13) over the SDK's true", f.InApp, f.Raw.InApp)
			}
		})
	}
	if n := h.fetcher.calls.Load(); n != 1 {
		t.Fatalf("fetcher called %d times across the table; the map is fetched once and cached", n)
	}
	if n := h.store.callCount(); n != 1 {
		t.Fatalf("store queried %d times; once per (project, release, path) on a hit", n)
	}
}

// TestSymbolicate_ColumnBetweenSegments is the contract's named case (S13, decision 21): wire
// column 140 sits between the segments at generated columns 138 (`Error(` → 19:15) and 144; it
// resolves to the EARLIER one, never an exact-match miss. Likewise 300 between 291 (31:28) and 307.
func TestSymbolicate_ColumnBetweenSegments(t *testing.T) {
	h := newHarness(t, nil)
	h.seedFixture(t)
	cases := []struct{ column, wantLine, wantColumn int32 }{
		{140, 19, 15},
		{144, 19, 15},
		{300, 31, 28},
		{2, 6, 8}, // one past the first segment: still the first segment, not no_mapping
	}
	for _, c := range cases {
		frames := []Frame{frame(fixturePath, 1, c.column)}
		res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
		if res.Outcomes[0] != OutcomeApplied || frames[0].Line != c.wantLine || frames[0].Column != c.wantColumn {
			t.Fatalf("wire (1,%d): %s %d:%d; want applied %d:%d", c.column, res.Outcomes[0], frames[0].Line, frames[0].Column, c.wantLine, c.wantColumn)
		}
	}
}

// TestSymbolicate_SourcelessSegmentIsNoMapping is the second named case of decision 21: a
// one-field segment at the position is `no_mapping` and the search does NOT continue backwards
// to the earlier segment that has a source. Map by hand: `AAAA` (genCol 0 → a.ts 1:1), `E`
// (genCol 2, no source), `EAAC` (genCol 4 → a.ts 1:2).
func TestSymbolicate_SourcelessSegmentIsNoMapping(t *testing.T) {
	h := newHarness(t, nil)
	h.seedCrafted("map-sourceless", "/sourceless.js",
		`{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA,E,EAAC"}`)
	cases := []struct {
		column int32
		want   Outcome
		line   int32
		col    int32
	}{
		{1, OutcomeApplied, 1, 1},
		{2, OutcomeApplied, 1, 1},
		{3, OutcomeNoMapping, 0, 0}, // on the source-less segment
		{4, OutcomeNoMapping, 0, 0}, // still under it; the earlier sourced segment is NOT used
		{5, OutcomeApplied, 1, 2},
		{9, OutcomeApplied, 1, 2},
	}
	for _, c := range cases {
		frames := []Frame{frame("/sourceless.js", 1, c.column)}
		res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
		if res.Outcomes[0] != c.want {
			t.Fatalf("wire (1,%d): %s, want %s", c.column, res.Outcomes[0], c.want)
		}
		if c.want == OutcomeApplied && (frames[0].Line != c.line || frames[0].Column != c.col) {
			t.Fatalf("wire (1,%d): %d:%d, want %d:%d", c.column, frames[0].Line, frames[0].Column, c.line, c.col)
		}
		if c.want == OutcomeNoMapping && (frames[0].Symbolicated || frames[0].Raw != nil || frames[0].File != "/sourceless.js") {
			t.Fatalf("a no_mapping frame passes through untouched: %+v", frames[0])
		}
	}
	// A line the map does not have, and a column before the first segment on a line.
	frames := []Frame{frame("/sourceless.js", 2, 1)}
	if res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames); res.Outcomes[0] != OutcomeNoMapping {
		t.Fatalf("line past the map: %s, want no_mapping", res.Outcomes[0])
	}
	// no_mapping is never negatively cached: the map is fine.
	if _, ok := h.svc.cache.negative(negKey{projectA, releaseR, "/sourceless.js"}); ok {
		t.Fatal("no_mapping must not write a negative entry")
	}
}

// TestSymbolicate_SourceRootJoinedAsStrings is S14 / §4.4 step 6 (decision 21): a relative
// source under a non-empty sourceRoot yields the string-joined file, no URL resolution.
func TestSymbolicate_SourceRootJoinedAsStrings(t *testing.T) {
	h := newHarness(t, nil)
	h.seedCrafted("map-webpack", "/_next/static/chunks/app/page-9b0d4e.js",
		`{"version":3,"sourceRoot":"webpack:///","sources":["./src/app/page.tsx"],"names":["render"],"mappings":"AAAAA"}`)
	h.seedCrafted("map-src", "/assets/lib.js",
		`{"version":3,"sourceRoot":"src","sources":["../lib/x.ts"],"names":[],"mappings":"AAAA"}`)
	h.seedCrafted("map-noroot", "/assets/noroot.js",
		`{"version":3,"sources":["../../src/x.ts"],"names":[],"mappings":"AAAA"}`)
	cases := []struct{ path, wantFile, wantFn string }{
		{"/_next/static/chunks/app/page-9b0d4e.js", "webpack:///./src/app/page.tsx", "render"},
		{"/assets/lib.js", "src/../lib/x.ts", "minified"},
		{"/assets/noroot.js", "../../src/x.ts", "minified"},
	}
	for _, c := range cases {
		frames := []Frame{frame(c.path, 1, 1)}
		res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
		if res.Outcomes[0] != OutcomeApplied || frames[0].File != c.wantFile || frames[0].Function != c.wantFn {
			t.Fatalf("%s: %s file=%q fn=%q; want applied %q %q", c.path, res.Outcomes[0], frames[0].File, frames[0].Function, c.wantFile, c.wantFn)
		}
	}
}

// ---- in_app re-derived from the resolved source (turbopack-grouping contract §4.2) --------------

// inAppCases are contract §4.2's fixture table A1–A22 -- the rows ARE the spec. A rewritten row
// resolves through the committed Turbopack-shaped map at wire (line, 1) to `file` (the map's
// sources[line-1], verbatim; README.md derives the mappings by hand), and the stage must store
// `want` under in_app and the SDK's `sent` under raw.in_app. A18 and A19 are frames the stage does
// NOT rewrite (`no_map`: a minified path the stub Store has no row for; `no_column`): the SDK's
// value is kept byte-identical and there is no raw (flow P7). The rule column is contract §4.2's:
// 1 = `/node_modules/` anywhere or a leading `node_modules/`; 2 = a non-`[project]` namespace
// ROOT (decision 18); 3 = everything else is the app's.
var inAppCases = []struct {
	row     string
	line    int32 // wire line on the Turbopack chunk; 0 = not rewritten
	file    string
	sent    bool
	want    bool
	outcome Outcome
}{
	{"A1", 1, "turbopack:///[project]/node_modules/.pnpm/react-dom@19.2.4_react@19.2.4/node_modules/react-dom/cjs/react-dom-client.production.js", true, false, OutcomeApplied}, // rule 1
	{"A2", 2, "turbopack:///[project]/node_modules/.pnpm/next@15.5.21_…/node_modules/next/dist/client/components/app-router.js", true, false, OutcomeApplied},                   // rule 1
	{"A3", 3, "turbopack:///[project]/src/app/(dashboard)/chats/page.tsx", true, true, OutcomeApplied},                                                                          // rule 3
	{"A4", 4, "turbopack:///[project]/src/features/chats/components/ChatList.tsx", false, true, OutcomeApplied},                                                                 // rule 3: the map outranks an init.inApp false (flow P3)
	{"A5", 5, "turbopack:///[project]/instrumentation.ts", true, true, OutcomeApplied},                                                                                          // rule 3: a [project] file outside src/ (D6)
	{"A6", 6, "turbopack:///[project]/app/boundary/thrower.tsx", true, true, OutcomeApplied},                                                                                    // rule 3: the scratch app, no src/
	{"A7", 7, "turbopack:///[turbopack]/shared/runtime-utils.ts", true, false, OutcomeApplied},                                                                                  // rule 2
	{"A8", 8, "turbopack:///[next]/internal/font/google/roboto_5a85fab1.module.css", true, false, OutcomeApplied},                                                               // rule 2
	{"A9", 9, "turbopack:///[externals]/node:inspector", true, false, OutcomeApplied},                                                                                           // rule 2
	{"A10", 10, "webpack:///./node_modules/react-dom/cjs/react-dom.production.js", true, false, OutcomeApplied},                                                                 // rule 1
	{"A11", 11, "webpack://_N_E/./app/boundary/thrower.tsx", true, true, OutcomeApplied},                                                                                        // rule 3
	{"A12", 12, "webpack:///./src/app/page.tsx", true, true, OutcomeApplied},                                                                                                    // rule 3
	{"A13", 13, "../../src/main.ts", true, true, OutcomeApplied},                                                                                                                // rule 3: the Vite fixture's shape
	{"A14", 14, "../node_modules/.vite/deps/chunk-ABCDEF12.js", true, false, OutcomeApplied},                                                                                    // rule 1
	{"A15", 15, "src/lib/server.ts", true, true, OutcomeApplied},                                                                                                                // rule 3: bare relative, sourceRoot empty
	{"A16", 16, "webpack:///./src/vendor/[next]-like-name.ts", true, true, OutcomeApplied},                                                                                      // rule 3: the root is `.`, the bracket is later (decision 18)
	{"A17", 17, "turbopack:///[project]/src/node_modules_shim/x.ts", true, true, OutcomeApplied},                                                                                // rule 3: `node_modules_shim` is not the segment `node_modules`
	{"A18", 0, "/app/.next/server/chunks/_052cc036._.js", true, true, OutcomeNoMap},                                                                                             // not rewritten: kept
	{"A19", 0, "https://app.example.com/_next/static/chunks/5f982f94079ecf41.js", false, false, OutcomeNoColumn},                                                                // not rewritten: kept
	{"A20", 18, "turbopack:///[project]/src/app/(dashboard)/agents/[id]/page.tsx", true, true, OutcomeApplied},                                                                  // rule 3: `[id]` is a later segment (decision 18)
	{"A21", 19, "turbopack:///[project]/src/app/(dashboard)/agents/[...unmatched]/page.tsx", true, true, OutcomeApplied},                                                        // rule 3: `[...unmatched]` is a later segment (decision 18)
	{"A22", 20, "node_modules/react-dom/cjs/react-dom.production.js", true, false, OutcomeApplied},                                                                              // rule 1: the leading-segment clause
}

// TestSymbolicate_InAppTable drives A1–A22 through Symbolicate over the committed Turbopack map:
// the returned Frame.InApp is the table's stored column and, on a rewritten row, Frame.Raw.InApp
// is what the SDK sent -- recorded whether true or false (D4). The minified name the frame came
// in with and its function are never read by the rule (rule 3 rows resolve from a chunk name that
// says nothing about the app); TestSymbolicate_InAppReadsOnlyTheResolvedFile pins that.
func TestSymbolicate_InAppTable(t *testing.T) {
	h := newHarness(t, nil)
	h.seedTurbopack(t, "map-turbopack", turbopackPath)
	for _, c := range inAppCases {
		t.Run(c.row, func(t *testing.T) {
			var fr Frame
			switch c.outcome {
			case OutcomeApplied:
				fr = frame(turbopackPath, c.line, 1)
			case OutcomeNoMap:
				fr = frame(c.file, 1, 314) // a column, but no `sourcemaps` row for this path
			case OutcomeNoColumn:
				fr = frame(c.file, 1, 0)
			default:
				t.Fatalf("table row %s names an outcome the test cannot drive: %s", c.row, c.outcome)
			}
			fr.InApp = c.sent
			frames := []Frame{fr}
			res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
			f := frames[0]
			if res.Outcomes[0] != c.outcome {
				t.Fatalf("outcome %s, want %s", res.Outcomes[0], c.outcome)
			}
			if f.File != c.file {
				t.Fatalf("file = %q, want %q", f.File, c.file)
			}
			if f.InApp != c.want {
				t.Fatalf("in_app = %v, want %v (SDK sent %v)", f.InApp, c.want, c.sent)
			}
			if c.outcome != OutcomeApplied {
				if f.Symbolicated || f.Raw != nil {
					t.Fatalf("a frame the stage did not rewrite carries no raw and no symbolicated: %+v", f)
				}
				return
			}
			if !f.Symbolicated || f.Raw == nil {
				t.Fatal("applied frame must carry symbolicated=true and raw")
			}
			if f.Raw.InApp != c.sent {
				t.Fatalf("raw.in_app = %v, want the SDK's %v", f.Raw.InApp, c.sent)
			}
			if f.Raw.File != turbopackPath || f.Raw.Line != c.line || f.Raw.Column != 1 || f.Raw.Function != "minified" {
				t.Fatalf("raw = %+v; want the values the SDK sent", *f.Raw)
			}
			if f.Line != 1 || f.Column != 1 {
				t.Fatalf("resolved to %d:%d, want 1:1 (README.md's mappings)", f.Line, f.Column)
			}
		})
	}
	if n := h.fetcher.calls.Load(); n != 1 {
		t.Fatalf("fetcher called %d times across the table; the map is fetched once and cached", n)
	}
}

// TestSymbolicate_InAppReadsOnlyTheResolvedFile: contract §4.2 -- "the rule never reads the
// minified name and never reads function". The same map is seeded under a minified path rule 1
// would reject and the frame's function is a string rule 2 would reject; an app row (A3) is still
// true and a vendor row (A1) still false, and the function reaches the stored frame untouched
// where the segment carries no name.
func TestSymbolicate_InAppReadsOnlyTheResolvedFile(t *testing.T) {
	h := newHarness(t, nil)
	const vendorLookingChunk = "/node_modules/.pnpm/next@15.5.21/dist/chunks/ac0ebedaba77188c.js"
	h.seedTurbopack(t, "map-vendor-named", vendorLookingChunk)
	frames := []Frame{
		{File: vendorLookingChunk, Line: 3, Column: 1, Function: "[next]/node_modules/x", InApp: false},
		{File: vendorLookingChunk, Line: 1, Column: 1, Function: "[project]/src/App", InApp: true},
	}
	res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
	if res.Outcomes[0] != OutcomeApplied || res.Outcomes[1] != OutcomeApplied {
		t.Fatalf("%v, want [applied applied]", res.Outcomes)
	}
	if !frames[0].InApp || frames[0].Function != "[next]/node_modules/x" {
		t.Fatalf("A3 under a vendor-looking chunk name: in_app=%v fn=%q; want true, function untouched", frames[0].InApp, frames[0].Function)
	}
	if frames[1].InApp || frames[1].Function != "renderWithHooks" {
		t.Fatalf("A1 with an app-looking function: in_app=%v fn=%q; want false, the map's name", frames[1].InApp, frames[1].Function)
	}
}

// TestInApp pins the pure rule on the strings contract §4.2's prose names beside the A-rows, so a
// regression in the regex is reported by the string, not by a table row's position.
func TestInApp(t *testing.T) {
	cases := map[string]bool{
		"webpack-internal:///(app-pages-browser)/./node_modules/next/dist/client/x.js": false, // rule 1
		"file:///home/ci/web/node_modules/react/index.js":                              false, // rule 1
		"node_modules/react/index.js":                                                  false, // rule 1, leading segment
		"turbopack:///[project]/x/node_modules_shim/y.ts":                              true,  // not the segment
		"[next]/dist/x.js":                      false, // rule 2, bare path root
		"[project]/src/x.ts":                    true,  // rule 2 admits [project]
		"turbopack:///[project]/[id]/page.tsx":  true,  // decision 18: only the root
		"https://cdn.example.com/[vendor]/x.js": true,  // `://` is not `:///`: no namespace root
		"":                                      true,  // rule 3 is the default
	}
	for p, want := range cases {
		if got := inApp(p); got != want {
			t.Errorf("inApp(%q) = %v, want %v", p, got, want)
		}
	}
}

// ---- the failure outcomes and the negative cache ------------------------------------------------

// TestSymbolicate_Sha256MismatchIsMapError: the bytes fetched do not hash to the row's sha256.
func TestSymbolicate_Sha256MismatchIsMapError(t *testing.T) {
	h := newHarness(t, nil)
	row := h.seedFixture(t)
	row.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	frames := []Frame{frame(fixturePath, 1, 10)}
	res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
	if res.Outcomes[0] != OutcomeMapError {
		t.Fatalf("%s, want map_error", res.Outcomes[0])
	}
	if frames[0].Symbolicated || frames[0].File != fixturePath {
		t.Fatal("frame must pass through untouched")
	}
	res = h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)})
	if res.Outcomes[0] != OutcomeMapError {
		t.Fatalf("second call: %s, want the negative entry to re-emit map_error", res.Outcomes[0])
	}
	if n := h.fetcher.calls.Load(); n != 1 {
		t.Fatalf("fetcher called %d times; the negative entry stops the second fetch", n)
	}
	if h.obs.count(CacheNegativeHit) != 1 {
		t.Fatalf("negative_hit = %d, want 1", h.obs.count(CacheNegativeHit))
	}
}

// TestSymbolicate_CraftedMapRecoversAsMapError is decision 19 / S11 / S12 on the contract's two
// inputs: a malformed VLQ run (a parse error) and a segment index past `sources` (a panic in
// resolve, recovered per map). Both: map_error, negatively cached, the next call a negative_hit
// re-emitting map_error, the fetcher not called again, the goroutine alive -- and the good map's
// frames in the same event symbolicate.
func TestSymbolicate_CraftedMapRecoversAsMapError(t *testing.T) {
	h := newHarness(t, nil)
	h.seedFixture(t)
	h.seedCrafted("map-bad-vlq", "/bad-vlq.js",
		`{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAA,!!!!"}`)
	// `AGAA`: source index +3 into a one-element `sources` -- decodes, panics at resolve.
	h.seedCrafted("map-index-past-sources", "/past-sources.js",
		`{"version":3,"sources":["a.ts"],"names":[],"mappings":"AGAA"}`)
	// `AAAAG`: name index +3 into an empty `names`.
	h.seedCrafted("map-index-past-names", "/past-names.js",
		`{"version":3,"sources":["a.ts"],"names":[],"mappings":"AAAAG"}`)

	// Warm the good map so the per-subtest fetch count below is the crafted map's alone.
	if res := h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)}); res.Outcomes[0] != OutcomeApplied {
		t.Fatal(res.Outcomes[0])
	}
	for _, path := range []string{"/bad-vlq.js", "/past-sources.js", "/past-names.js"} {
		t.Run(path, func(t *testing.T) {
			before := h.fetcher.calls.Load()
			frames := []Frame{frame(path, 1, 1), frame(fixturePath, 1, 10), frame(path, 1, 2)}
			res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
			if res.Outcomes[0] != OutcomeMapError || res.Outcomes[2] != OutcomeMapError {
				t.Fatalf("crafted map frames: %v, want map_error", res.Outcomes)
			}
			if res.Outcomes[1] != OutcomeApplied || frames[1].Function != "subtotal" {
				t.Fatalf("the other chunk's frame must still symbolicate: %s %q", res.Outcomes[1], frames[1].Function)
			}
			if frames[0].Symbolicated || frames[0].File != path {
				t.Fatalf("crafted map frame must pass through untouched: %+v", frames[0])
			}
			if reason, ok := h.svc.cache.negative(negKey{projectA, releaseR, path}); !ok || reason != OutcomeMapError {
				t.Fatalf("negative entry = (%s, %v); want map_error", reason, ok)
			}
			negHits := h.obs.count(CacheNegativeHit)
			res = h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(path, 1, 1)})
			if res.Outcomes[0] != OutcomeMapError {
				t.Fatalf("next event: %s, want the negative entry to re-emit map_error", res.Outcomes[0])
			}
			if h.obs.count(CacheNegativeHit) != negHits+1 {
				t.Fatalf("negative_hit did not increment on the next event")
			}
			if got := h.fetcher.calls.Load() - before; got != 1 {
				t.Fatalf("fetcher called %d times for the crafted map; want exactly one", got)
			}
			if _, cached := h.svc.cache.pos["map-index-past-sources"]; cached && path == "/past-sources.js" {
				t.Fatal("a map whose resolve panicked must be dropped from the positive cache")
			}
		})
	}
}

func TestSymbolicate_FetchErrorIsNegativelyCached(t *testing.T) {
	h := newHarness(t, nil)
	h.seedFixture(t)
	h.fetcher.err = errors.New("connection refused")
	res := h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)})
	if res.Outcomes[0] != OutcomeFetchError {
		t.Fatalf("%s, want fetch_error", res.Outcomes[0])
	}
	h.fetcher.err = nil // storage is back -- but the negative entry stands for the TTL
	res = h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)})
	if res.Outcomes[0] != OutcomeFetchError || h.fetcher.calls.Load() != 1 {
		t.Fatalf("second call: %s after %d fetches; want fetch_error re-emitted from the negative entry", res.Outcomes[0], h.fetcher.calls.Load())
	}
	h.clock.advance(h.cfg.NegativeTTL + time.Second)
	res = h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)})
	if res.Outcomes[0] != OutcomeApplied {
		t.Fatalf("after the negative TTL: %s, want applied", res.Outcomes[0])
	}
}

func TestSymbolicate_NoMapIsNegativelyCached(t *testing.T) {
	h := newHarness(t, nil)
	frames := []Frame{frame("/assets/nothing.js", 1, 10), frame("/assets/nothing.js", 1, 20)}
	res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
	if res.Outcomes[0] != OutcomeNoMap || res.Outcomes[1] != OutcomeNoMap {
		t.Fatalf("%v, want no_map", res.Outcomes)
	}
	if res.EventOutcome() != EventNone {
		t.Fatalf("event outcome %s, want none", res.EventOutcome())
	}
	h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame("/assets/nothing.js", 1, 10)})
	if h.store.callCount() != 1 {
		t.Fatalf("store queried %d times; the negative entry answers the second event", h.store.callCount())
	}
	if h.fetcher.calls.Load() != 0 {
		t.Fatal("nothing to fetch on no_map")
	}
	// The CI-uploads-late case (Known limit 1): the map lands, the negative TTL passes, the next
	// event hits.
	h.seedCrafted("map-late", "/assets/nothing.js", `{"version":3,"sources":["late.ts"],"names":[],"mappings":"AAAA"}`)
	h.clock.advance(h.cfg.NegativeTTL + time.Millisecond)
	res = h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame("/assets/nothing.js", 1, 10)})
	if res.Outcomes[0] != OutcomeApplied {
		t.Fatalf("after the TTL: %s, want applied", res.Outcomes[0])
	}
}

func TestSymbolicate_StoreErrorIsFetchErrorNotCached(t *testing.T) {
	h := newHarness(t, nil)
	h.seedFixture(t)
	h.store.err = errors.New("db down")
	res := h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)})
	if res.Outcomes[0] != OutcomeFetchError {
		t.Fatalf("%s, want fetch_error", res.Outcomes[0])
	}
	h.store.err = nil
	res = h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)})
	if res.Outcomes[0] != OutcomeApplied {
		t.Fatalf("with the store back: %s, want applied -- a store failure is not negatively cached", res.Outcomes[0])
	}
}

func TestSymbolicate_NoColumnPassesThrough(t *testing.T) {
	h := newHarness(t, nil)
	h.seedFixture(t)
	frames := []Frame{frame(fixturePath, 1, 0), frame(fixturePath, 1, 10)}
	res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
	if res.Outcomes[0] != OutcomeNoColumn || res.Outcomes[1] != OutcomeApplied {
		t.Fatalf("%v, want [no_column applied]", res.Outcomes)
	}
	if frames[0].Symbolicated || frames[0].Raw != nil || frames[0].Line != 1 {
		t.Fatalf("no_column frame must be untouched: %+v", frames[0])
	}
	if res.EventOutcome() != EventAppliedPartial {
		t.Fatalf("event outcome %s, want applied_partial", res.EventOutcome())
	}
	// Only no_column frames: nothing is looked up at all.
	h2 := newHarness(t, nil)
	res = h2.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 0)})
	if res.Outcomes[0] != OutcomeNoColumn || h2.store.callCount() != 0 {
		t.Fatal("a no_column frame must not reach the store")
	}
}

// TestSymbolicate_LookupUsesNormalisedPath: the frame names an absolute URL with a cache-buster;
// the store is asked for the normalised path the upload route stored (decision 22).
func TestSymbolicate_LookupUsesNormalisedPath(t *testing.T) {
	h := newHarness(t, nil)
	h.seedFixture(t)
	frames := []Frame{
		frame("https://app.example.com"+fixturePath+"?v=123", 1, 10),
		frame("HTTP://cdn.example.com:8443"+fixturePath+"#x", 1, 69),
	}
	res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
	if res.Outcomes[0] != OutcomeApplied || res.Outcomes[1] != OutcomeApplied {
		t.Fatalf("%v, want applied", res.Outcomes)
	}
	if h.store.callCount() != 1 || h.store.calls[0].minifiedPath != fixturePath {
		t.Fatalf("store calls %v; want one lookup on %q", h.store.calls, fixturePath)
	}
	if frames[0].Raw.File != "https://app.example.com"+fixturePath+"?v=123" {
		t.Fatalf("raw.file must be the string the SDK sent, got %q", frames[0].Raw.File)
	}
}

// ---- the bounds: deadline, parse cap, cache bound, LRU, positive TTL --------------------------

// TestSymbolicate_TimeoutWithBackgroundFill is decision 37 / S10c: a Fetcher slower than the
// deadline yields `timeout`, NO negative entry is written, the fill completes in the background,
// and the next event on the map is applied from the cache with the Fetcher called exactly once.
func TestSymbolicate_TimeoutWithBackgroundFill(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Deadline = 50 * time.Millisecond })
	h.seedFixture(t)
	h.fetcher.delay = 500 * time.Millisecond

	start := time.Now()
	frames := []Frame{frame(fixturePath, 1, 10), frame(fixturePath, 1, 69)}
	res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
	elapsed := time.Since(start)
	if res.Outcomes[0] != OutcomeTimeout || res.Outcomes[1] != OutcomeTimeout {
		t.Fatalf("%v, want timeout", res.Outcomes)
	}
	if elapsed >= h.fetcher.delay {
		t.Fatalf("the event waited %v for a %v fetch; the deadline did not bound it", elapsed, h.fetcher.delay)
	}
	if frames[0].Symbolicated || frames[0].File != fixturePath {
		t.Fatal("a timeout frame passes through untouched")
	}
	if _, ok := h.svc.cache.negative(negKey{projectA, releaseR, fixturePath}); ok {
		t.Fatal("timeout must never write a negative entry")
	}
	if h.fetcher.calls.Load() != 1 {
		t.Fatalf("fetcher calls = %d, want the one in-flight fill", h.fetcher.calls.Load())
	}

	h.waitFills()
	if h.svc.CacheBytes() == 0 {
		t.Fatal("the background fill must have landed in the cache")
	}
	frames = []Frame{frame(fixturePath, 1, 10)}
	res = h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
	if res.Outcomes[0] != OutcomeApplied || frames[0].Function != "subtotal" {
		t.Fatalf("next event: %s %q; want applied from the cache", res.Outcomes[0], frames[0].Function)
	}
	if h.fetcher.calls.Load() != 1 {
		t.Fatalf("fetcher calls = %d after the next event; the fill must not run twice", h.fetcher.calls.Load())
	}
	if h.obs.count(CacheHit) != 1 {
		t.Fatalf("cache hit = %d, want 1", h.obs.count(CacheHit))
	}
}

// TestSymbolicate_OneFillPerKey: a burst of events on one cold map joins one fill.
func TestSymbolicate_OneFillPerKey(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Deadline = 2 * time.Second })
	h.seedFixture(t)
	h.fetcher.delay = 100 * time.Millisecond
	var wg sync.WaitGroup
	outcomes := make([]Outcome, 8)
	for i := range outcomes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res := h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)})
			outcomes[i] = res.Outcomes[0]
		}(i)
	}
	wg.Wait()
	for i, o := range outcomes {
		if o != OutcomeApplied {
			t.Fatalf("event %d: %s, want applied", i, o)
		}
	}
	if h.fetcher.calls.Load() != 1 {
		t.Fatalf("fetcher calls = %d for 8 concurrent events; want 1", h.fetcher.calls.Load())
	}
}

// TestSymbolicate_ParseCapNeverFetches is decision 38 / S10b: a row whose size_bytes exceeds
// SOURCEMAP_MAX_PARSE_BYTES is `oversize`, negatively cached, and the Fetcher is never called.
func TestSymbolicate_ParseCapNeverFetches(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxParseBytes = 1000 })
	row := h.seedFixture(t)
	row.SizeBytes = 1001
	for i := 0; i < 2; i++ {
		res := h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)})
		if res.Outcomes[0] != OutcomeOversize {
			t.Fatalf("call %d: %s, want oversize", i, res.Outcomes[0])
		}
	}
	if h.fetcher.calls.Load() != 0 {
		t.Fatalf("fetcher called %d times; an oversize row is never fetched", h.fetcher.calls.Load())
	}
	if reason, ok := h.svc.cache.negative(negKey{projectA, releaseR, fixturePath}); !ok || reason != OutcomeOversize {
		t.Fatalf("negative entry = (%s, %v), want oversize", reason, ok)
	}
	if h.store.callCount() != 1 || h.obs.count(CacheNegativeHit) < 1 {
		t.Fatal("the second call must be a negative hit, not a second query")
	}
	// Exactly at the cap is admitted.
	row.SizeBytes = 1000
	h.clock.advance(h.cfg.NegativeTTL + time.Second)
	if res := h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)}); res.Outcomes[0] != OutcomeApplied {
		t.Fatalf("at the cap: %s, want applied", res.Outcomes[0])
	}
}

// TestSymbolicate_CacheOversizeIsFetchedPerEvent is S10: a map over SOURCEMAP_CACHE_BYTES but
// within the parse cap is applied, never retained (cache_events_total{kind="oversize"}), and
// fetched again by the next event.
func TestSymbolicate_CacheOversizeIsFetchedPerEvent(t *testing.T) {
	raw := readFixtureMap(t)
	h := newHarness(t, func(c *Config) { c.CacheBytes = int64(len(raw)) - 1 })
	h.seedFixture(t)
	for i := 1; i <= 2; i++ {
		frames := []Frame{frame(fixturePath, 1, 10)}
		res := h.svc.Symbolicate(context.Background(), projectA, releaseR, frames)
		if res.Outcomes[0] != OutcomeApplied || frames[0].Function != "subtotal" {
			t.Fatalf("event %d: %s; want applied", i, res.Outcomes[0])
		}
		if h.fetcher.calls.Load() != int32(i) {
			t.Fatalf("event %d: fetcher calls = %d; an over-bound map is fetched per event", i, h.fetcher.calls.Load())
		}
	}
	if h.svc.CacheBytes() != 0 {
		t.Fatalf("cache bytes = %d; the map must not be retained", h.svc.CacheBytes())
	}
	if h.obs.count(CacheOversize) != 2 || h.obs.count(CacheHit) != 0 {
		t.Fatalf("oversize=%d hit=%d; want 2 and 0", h.obs.count(CacheOversize), h.obs.count(CacheHit))
	}
}

// TestSymbolicate_LRUEvictionUnderByteBound: three maps of one size into a bound that holds two.
// Touching A before C arrives makes B the least recently used, so B is evicted -- not A.
func TestSymbolicate_LRUEvictionUnderByteBound(t *testing.T) {
	const mapJSON = `{"version":3,"sources":["x.ts"],"names":[],"mappings":"AAAA"}`
	size := int64(len(mapJSON))
	h := newHarness(t, func(c *Config) { c.CacheBytes = 2 * size })
	h.seedCrafted("map-a", "/a.js", mapJSON)
	h.seedCrafted("map-b", "/b.js", mapJSON)
	h.seedCrafted("map-c", "/c.js", mapJSON)
	call := func(path string) {
		t.Helper()
		if res := h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(path, 1, 1)}); res.Outcomes[0] != OutcomeApplied {
			t.Fatalf("%s: %s", path, res.Outcomes[0])
		}
	}
	call("/a.js")
	call("/b.js")
	if h.svc.CacheBytes() != 2*size {
		t.Fatalf("cache bytes = %d, want %d", h.svc.CacheBytes(), 2*size)
	}
	call("/a.js") // A is now the most recently used
	call("/c.js") // needs room: B goes
	if h.svc.CacheBytes() != 2*size || h.obs.count(CacheEvict) != 1 {
		t.Fatalf("cache bytes = %d evict = %d; want %d and 1", h.svc.CacheBytes(), h.obs.count(CacheEvict), 2*size)
	}
	h.svc.cache.mu.Lock()
	_, hasA := h.svc.cache.pos["map-a"]
	_, hasB := h.svc.cache.pos["map-b"]
	_, hasC := h.svc.cache.pos["map-c"]
	h.svc.cache.mu.Unlock()
	if !hasA || hasB || !hasC {
		t.Fatalf("cache holds a=%v b=%v c=%v; want a and c", hasA, hasB, hasC)
	}
	fetches := h.fetcher.calls.Load()
	call("/b.js") // a miss again: fetched and B's arrival evicts the LRU (A, untouched since)
	if h.fetcher.calls.Load() != fetches+1 {
		t.Fatal("an evicted map is fetched again")
	}
	if h.obs.bytes != 2*size {
		t.Fatalf("observer bytes = %d, want %d", h.obs.bytes, 2*size)
	}
}

// TestSymbolicate_PositiveTTL: a cached map is re-fetched after SOURCEMAP_POSITIVE_TTL (how a
// replaced map is picked up without cross-pod invalidation).
func TestSymbolicate_PositiveTTL(t *testing.T) {
	h := newHarness(t, nil)
	h.seedFixture(t)
	call := func() {
		t.Helper()
		if res := h.svc.Symbolicate(context.Background(), projectA, releaseR, []Frame{frame(fixturePath, 1, 10)}); res.Outcomes[0] != OutcomeApplied {
			t.Fatal(res.Outcomes[0])
		}
	}
	call()
	h.clock.advance(h.cfg.PositiveTTL - time.Second)
	call()
	if h.fetcher.calls.Load() != 1 {
		t.Fatalf("fetcher calls = %d inside the TTL, want 1", h.fetcher.calls.Load())
	}
	h.clock.advance(2 * time.Second)
	call()
	if h.fetcher.calls.Load() != 2 {
		t.Fatalf("fetcher calls = %d after the TTL, want 2", h.fetcher.calls.Load())
	}
	if h.obs.count(CacheEvict) != 1 || h.obs.count(CacheMiss) != 2 || h.obs.count(CacheHit) != 1 {
		t.Fatalf("evict=%d miss=%d hit=%d; want 1, 2, 1", h.obs.count(CacheEvict), h.obs.count(CacheMiss), h.obs.count(CacheHit))
	}
}

// ---- Gate, Result, Config -----------------------------------------------------------------------

func TestGate(t *testing.T) {
	cases := []struct {
		platforms []string
		platform  string
		release   string
		frames    int
		ok        bool
		skipped   EventOutcome
	}{
		{DefaultPlatforms, "javascript", "1.0", 1, true, ""},
		{DefaultPlatforms, "browser", "1.0", 3, true, ""},
		{DefaultPlatforms, "node", "1.0", 1, false, EventSkippedPlatform},
		{DefaultPlatforms, "golang", "1.0", 1, false, EventSkippedPlatform},
		{nil, "javascript", "1.0", 1, false, EventSkippedPlatform}, // set-and-empty: off
		{DefaultPlatforms, "javascript", "", 1, false, EventSkippedNoRelease},
		{DefaultPlatforms, "javascript", "1.0", 0, false, EventSkippedNoFrames},
		{[]string{"node"}, "node", "1.0", 1, true, ""},
	}
	for _, c := range cases {
		ok, skipped := Gate(c.platforms, c.platform, c.release, c.frames)
		if ok != c.ok || skipped != c.skipped {
			t.Errorf("Gate(%v, %q, %q, %d) = (%v, %q), want (%v, %q)", c.platforms, c.platform, c.release, c.frames, ok, skipped, c.ok, c.skipped)
		}
	}
}

func TestResult_EventOutcome(t *testing.T) {
	cases := []struct {
		outcomes []Outcome
		want     EventOutcome
	}{
		{nil, EventNone},
		{[]Outcome{OutcomeNoMap, OutcomeNoColumn}, EventNone},
		{[]Outcome{OutcomeApplied, OutcomeApplied}, EventAppliedAll},
		{[]Outcome{OutcomeApplied, OutcomeTimeout}, EventAppliedPartial},
	}
	for _, c := range cases {
		if got := (Result{Outcomes: c.outcomes}).EventOutcome(); got != c.want {
			t.Errorf("%v: %s, want %s", c.outcomes, got, c.want)
		}
	}
}

func TestParseConfig(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	cfg, err := ParseConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cfg.Platforms) != "[javascript browser]" || cfg.CacheBytes != 134217728 || cfg.MaxParseBytes != 52428800 ||
		cfg.NegativeTTL != 60*time.Second || cfg.PositiveTTL != 10*time.Minute || cfg.Deadline != 5*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}
	cfg, err = ParseConfig(env(map[string]string{EnvPlatforms: ""}))
	if err != nil || cfg.Enabled() {
		t.Fatalf("set-and-empty must turn the stage off: %+v %v", cfg, err)
	}
	cfg, err = ParseConfig(env(map[string]string{
		EnvPlatforms: " Node , web,", EnvCacheBytes: "1024", EnvMaxParseBytes: "2048",
		EnvNegativeTTL: "5s", EnvPositiveTTL: "1m", EnvDeadline: "250ms",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(cfg.Platforms) != "[node web]" || cfg.CacheBytes != 1024 || cfg.MaxParseBytes != 2048 ||
		cfg.NegativeTTL != 5*time.Second || cfg.PositiveTTL != time.Minute || cfg.Deadline != 250*time.Millisecond {
		t.Fatalf("parsed: %+v", cfg)
	}
	for _, bad := range []map[string]string{
		{EnvCacheBytes: "128Mi"},
		{EnvCacheBytes: "0"},
		{EnvMaxParseBytes: "50M"},
		{EnvDeadline: "5"},
		{EnvDeadline: "-1s"},
		{EnvNegativeTTL: "sixty seconds"},
	} {
		if _, err := ParseConfig(env(bad)); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
	if _, err := New(Config{}, newStubStore(), &stubFetcher{}); err == nil {
		t.Fatal("New must refuse a zero Config")
	}
}
