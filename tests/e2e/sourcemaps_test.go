//go:build e2e

// sentinel-sourcemaps flows S1/S2 (contract §11's tests/e2e row, be.md task 12): the one place the
// order "symbolicate BEFORE fingerprint" is proved end to end — real ingestor, real NATS hop, real
// processor reading a real map out of MinIO through the real upload route on the dashboard.
//
// Needs, beyond the stack every other row needs: MinIO up, the processor carrying the S3 env, and
// SOURCEMAP_NEGATIVE_TTL short (docker-compose.yml sets 5s) — the case posts an event BEFORE the
// map exists, which negatively caches (project, R1, path) as `no_map` for that TTL, and the second
// posting must land after it has expired. With the chart default (60s) the wait is a minute.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The committed fixture builds (tests/fixtures/sourcemaps/README.md). The three in_app frames are
// three anchors of the seed's generator — a function's declaration site in build A's map, the same
// source position under build B's chunk name and generated column — pinned here as literals so the
// case says exactly which frame resolved to what. Rebuilding the fixture app moves the columns and
// the chunk hashes; this test then fails loudly rather than drifting, and the README's
// `manifest.json` (from `node generate-events.mjs <dir>`) is where the new values come from.
const (
	sourcemapsFixtureDir = "../fixtures/sourcemaps"
	sourcemapsRelease    = "R1"
)

type sourcemapAnchor struct {
	// build A: chunk path and 1-based generated column; the minified identifier a browser prints.
	fileA   string
	columnA int32
	// build B: the same source position under build B's chunk name.
	fileB   string
	columnB int32
	minName string
	// what §4.4 step 6 must write: the map's `source` verbatim, its `name`, the original 1-based position.
	source string
	name   string
	line   int32
	column int32
}

var sourcemapAnchors = []sourcemapAnchor{
	{fileA: "/assets/cart-52a4d957.js", columnA: 109, fileB: "/assets/cart-540c0391_b2.js", columnB: 115, minName: "f",
		source: "../../app/src/cart.ts", name: "addToCart", line: 12, column: 17},
	{fileA: "/assets/checkout-33d1a3d0.js", columnA: 89, fileB: "/assets/checkout-b747d932_b2.js", columnB: 95, minName: "c",
		source: "../../app/src/checkout.ts", name: "validateAddress", line: 11, column: 17},
	{fileA: "/assets/money-c4af03b2.js", columnA: 10, fileB: "/assets/money-c4af03b2_b2.js", columnB: 10, minName: "o",
		source: "../../app/src/lib/money.ts", name: "formatMoney", line: 7, column: 17},
	// Not in_app: symbolicated like every other frame (§4.4 "all frames, not only the first three")
	// but never hashed.
	{fileA: "/assets/search-5b98ebd0.js", columnA: 56, fileB: "/assets/search-6daa4abe_b2.js", columnB: 59, minName: "i",
		source: "../../app/src/search.ts", name: "tokenize", line: 9, column: 17},
}

// storedFrame is the JSONB shape §4.4 steps 6-7 write into error_occurrences.stacktrace.
type storedFrame struct {
	File         string `json:"file"`
	Line         int32  `json:"line"`
	Column       int32  `json:"column"`
	Function     string `json:"function"`
	InApp        bool   `json:"in_app"`
	Symbolicated *bool  `json:"symbolicated"`
	Raw          *struct {
		File     string `json:"file"`
		Line     int32  `json:"line"`
		Column   int32  `json:"column"`
		Function string `json:"function"`
		InApp    bool   `json:"in_app"`
	} `json:"raw"`
}

// sourcemapUploadResponse is §4.1's success body.
type sourcemapUploadResponse struct {
	ProjectID        string `json:"project_id"`
	ReleaseVersion   string `json:"release_version"`
	ReleaseFileCount int    `json:"release_file_count"`
	Files            []struct {
		MinifiedPath string `json:"minified_path"`
		SHA256       string `json:"sha256"`
		SizeBytes    int    `json:"size_bytes"`
		Outcome      string `json:"outcome"`
	} `json:"files"`
}

// seedInApp is the in_app value the seed sends for frame i: the three anchors true, the rest false.
// Shared by sourcemapEvent and assertSymbolicatedFrames so the seed and the raw.in_app expectation
// cannot drift apart.
func seedInApp(i int) bool { return i < 3 }

// sourcemapEvent builds the javascript event for one build: the three anchors in_app, the fourth
// not, every frame with a column, and an explicit event_id so "the same event with a fresh
// event_id" is literal (the ingestor dedups on it).
func (f *fixture) sourcemapEvent(build byte, errorClass string) event {
	frames := make([]map[string]any, 0, len(sourcemapAnchors))
	for i, a := range sourcemapAnchors {
		file, column := a.fileA, a.columnA
		if build == 'b' {
			file, column = a.fileB, a.columnB
		}
		frames = append(frames, map[string]any{
			"file": file, "line": 1, "column": column, "function": a.minName, "in_app": seedInApp(i),
		})
	}
	return f.newEvent().with(event{
		"platform":        "javascript",
		"release_version": sourcemapsRelease,
		"error_class":     errorClass,
		"message":         "sourcemaps e2e: " + errorClass,
		"event_id":        uuid.NewString(),
		"stacktrace":      frames,
	})
}

// sourcemapPart is one (minified_path, file) pair of a multipart POST /api/sourcemaps body.
type sourcemapPart struct {
	minifiedPath string
	fixturePath  string
}

// uploadFixtureBuild POSTs every map under tests/fixtures/sourcemaps/dist-<build>/assets to
// §4.1's route with the fixture's project-scoped ingest key, paired by position, and returns the
// decoded body. The minified_path is `/assets/<chunk>.js`, exactly what the browser reports and
// what the anchors above carry.
func (f *fixture) uploadFixtureBuild(build byte) (int, sourcemapUploadResponse, string) {
	f.t.Helper()
	dir := filepath.Join(sourcemapsFixtureDir, "dist-"+string(build), "assets")
	maps, err := filepath.Glob(filepath.Join(dir, "*.js.map"))
	if err != nil || len(maps) == 0 {
		f.t.Fatalf("no fixture maps under %s (err=%v) — run from tests/e2e, the fixture is committed", dir, err)
	}
	parts := make([]sourcemapPart, 0, len(maps))
	for _, m := range maps {
		chunk := strings.TrimSuffix(filepath.Base(m), ".map")
		parts = append(parts, sourcemapPart{minifiedPath: "/assets/" + chunk, fixturePath: m})
	}
	return f.postSourcemaps(parts)
}

// postSourcemaps POSTs the given parts, in order, as ONE multipart request to §4.1's route under
// sourcemapsRelease with the fixture's project-scoped ingest key, and returns the status, the
// decoded body and the raw body.
func (f *fixture) postSourcemaps(parts []sourcemapPart) (int, sourcemapUploadResponse, string) {
	f.t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("release_version", sourcemapsRelease); err != nil {
		f.t.Fatal(err)
	}
	for _, p := range parts {
		if err := w.WriteField("minified_path", p.minifiedPath); err != nil {
			f.t.Fatal(err)
		}
		part, err := w.CreateFormFile("file", filepath.Base(p.fixturePath))
		if err != nil {
			f.t.Fatal(err)
		}
		raw, err := os.ReadFile(p.fixturePath)
		if err != nil {
			f.t.Fatalf("reading fixture %s: %v — run from tests/e2e, the fixture is committed", p.fixturePath, err)
		}
		if _, err := part.Write(raw); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		f.t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, cfg.DashboardURL+"/api/sourcemaps", &body)
	if err != nil {
		f.t.Fatal(err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("X-API-Key", f.APIKey)
	// No Origin header on purpose: a CI curl sends none, and §2.1's CSRF exemption is what lets
	// the multipart POST through (flow U17).
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatalf("POST /api/sourcemaps: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out sourcemapUploadResponse
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out, string(raw)
}

// storedFrames decodes one occurrence's stacktrace JSONB.
func storedFrames(t *testing.T, row occurrenceRow) []storedFrame {
	t.Helper()
	var frames []storedFrame
	if err := json.Unmarshal(row.Stacktrace, &frames); err != nil {
		t.Fatalf("decoding stacktrace of occurrence %s: %v\n  jsonb: %s", row.ID, err, row.Stacktrace)
	}
	return frames
}

// assertRawFrames asserts §4.4 step 7: a frame that passed through carries NO `symbolicated` key
// (absent, not false) and no `raw`, so it is byte-identical to a pre-feature frame.
func assertRawFrames(t *testing.T, frames []storedFrame) {
	t.Helper()
	if len(frames) != len(sourcemapAnchors) {
		t.Fatalf("stored %d frames, want %d", len(frames), len(sourcemapAnchors))
	}
	for i, fr := range frames {
		if fr.Symbolicated != nil || fr.Raw != nil {
			t.Fatalf("frame %d was symbolicated before any map existed: %+v", i, fr)
		}
		if fr.File != sourcemapAnchors[i].fileA || fr.Function != sourcemapAnchors[i].minName {
			t.Fatalf("frame %d stored as %s:%s, want the raw %s:%s", i, fr.File, fr.Function, sourcemapAnchors[i].fileA, sourcemapAnchors[i].minName)
		}
	}
}

// assertSymbolicatedFrames asserts §4.4 step 6 on every frame: file ← the map's source verbatim,
// function ← the map's name, line/column ← the original 1-based position, `symbolicated: true`,
// and `raw` holding exactly what the SDK sent for the given build — including `raw.in_app`.
//
// in_app itself is turbopack-grouping §4.2 (decision 19): a rewritten frame's in_app is
// RE-DERIVED from the resolved source, and every source of the Vite fixture
// (`../../app/src/<x>.ts` — bare relative, no node_modules, no bracketed root) is `true` by rule 3,
// whatever the seed sent. So a symbolicated frame asserts `in_app == true` and `raw.in_app` equal
// to the seed's value; only an UNREWRITTEN frame would keep the seed's value as its own in_app.
func assertSymbolicatedFrames(t *testing.T, frames []storedFrame, build byte) {
	t.Helper()
	if len(frames) != len(sourcemapAnchors) {
		t.Fatalf("stored %d frames, want %d", len(frames), len(sourcemapAnchors))
	}
	for i, fr := range frames {
		a := sourcemapAnchors[i]
		rawFile, rawColumn := a.fileA, a.columnA
		if build == 'b' {
			rawFile, rawColumn = a.fileB, a.columnB
		}
		if fr.Symbolicated == nil || !*fr.Symbolicated || fr.Raw == nil {
			t.Fatalf("frame %d (%s col %d) was not symbolicated: %+v", i, rawFile, rawColumn, fr)
		}
		if fr.File != a.source || fr.Function != a.name || fr.Line != a.line || fr.Column != a.column {
			t.Fatalf("frame %d resolved to %s:%s:%d:%d, want %s:%s:%d:%d", i,
				fr.File, fr.Function, fr.Line, fr.Column, a.source, a.name, a.line, a.column)
		}
		if fr.Raw.File != rawFile || fr.Raw.Column != rawColumn || fr.Raw.Line != 1 || fr.Raw.Function != a.minName {
			t.Fatalf("frame %d raw = %+v, want %s:1:%d %s", i, *fr.Raw, rawFile, rawColumn, a.minName)
		}
		if !fr.InApp {
			t.Fatalf("frame %d in_app = false, want true: symbolicated source %s is in_app by §4.2 rule 3", i, fr.File)
		}
		if fr.Raw.InApp != seedInApp(i) {
			t.Fatalf("frame %d raw.in_app = %v, want the seed's %v", i, fr.Raw.InApp, seedInApp(i))
		}
	}
}

// TestS1S2_SymbolicateBeforeFingerprint proves flows S1 and S2 — and documents Known limit 1 on
// the way:
//
//  1. a javascript event for R1 with columns, posted BEFORE any upload, lands RAW on issue 1;
//  2. build A's maps are uploaded (§4.1, all `created`); the SAME event with a fresh event_id,
//     posted after SOURCEMAP_NEGATIVE_TTL has passed, lands SYMBOLICATED — on a SECOND issue.
//     That second issue is Known limit 1 (§12): nothing re-symbolicates the first occurrence, and
//     the fingerprint is computed on the frames as stored, so the raw-frame identity and the
//     source-position identity are two issues. The product's mitigation is §4.8 rule 1 (upload
//     before the deploy switches traffic), not anything in the server;
//  3. build B's maps are uploaded under the same R1 — `created`, never `replaced`, because its
//     chunk names differ — and build B's event (new chunk hash and generated column, the same
//     source position, fresh event_id) lands on issue 2: the fingerprint saw the symbolicated
//     frames, never the chunk hash. That is the feature's hypothesis (§1) in one assertion;
//  4. the issue-detail loader serves the symbolicated frame, and /metrics carries the counters;
//  5. (sentinel-sourcemaps-index-maps §4.6, D8, D12, D15) the real Turbopack index map and the
//     empty stub go up under R1 in ONE two-part request — `created` + `skipped_empty`, and
//     `release_file_count` 17 because the stub is not a row — and an event with a frame inside
//     the map's fifth section, a frame before its first section and a frame on the stub lands
//     with exactly the first one rewritten through the section (`applied_partial`), the second
//     `no_section` and the third `no_map`.
func TestS1S2_SymbolicateBeforeFingerprint(t *testing.T) {
	f := newFixture(t)

	// --- 1. before any upload: raw, issue 1 ------------------------------------------------------
	first := f.sourcemapEvent('a', "TypeError")
	if res := f.ingest(first); res.Status != http.StatusAccepted {
		t.Fatalf("POST /ingest (before upload): got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForIssues(1)
	f.waitForOccurrences(1)
	issue1 := f.onlyIssue()
	assertRawFrames(t, storedFrames(t, f.occurrences()[0]))
	t.Logf("issue 1 (raw frames, before the upload): id=%s fingerprint=%s", issue1.ID, issue1.Fingerprint)

	// --- 2. upload build A, wait out the negative cache, post again: symbolicated, issue 2 -------
	status, up, raw := f.uploadFixtureBuild('a')
	if status != http.StatusCreated {
		t.Fatalf("POST /api/sourcemaps (build A): got %d, want 201\n  body: %s", status, raw)
	}
	if up.ReleaseVersion != sourcemapsRelease || up.ProjectID != f.ProjectID || len(up.Files) != 8 || up.ReleaseFileCount != 8 {
		t.Fatalf("build A upload body: %s", raw)
	}
	for _, uf := range up.Files {
		if uf.Outcome != "created" {
			t.Fatalf("build A: %s was %q, want created\n  body: %s", uf.MinifiedPath, uf.Outcome, raw)
		}
	}
	t.Logf("build A uploaded: %d files, release_file_count=%d", len(up.Files), up.ReleaseFileCount)

	// The first event negatively cached every (project, R1, path) as `no_map` (§4.4 step 4). The
	// TTL is not observable from outside, so this is the one wait in the suite that is a sleep:
	// the compose processor sets SOURCEMAP_NEGATIVE_TTL=5s (docker-compose.yml); honour an
	// override so a run against a differently configured stack still passes.
	ttl := 5 * time.Second
	if v := os.Getenv("SOURCEMAP_NEGATIVE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("SOURCEMAP_NEGATIVE_TTL=%q is not a Go duration", v)
		}
		ttl = d
	}
	time.Sleep(ttl + time.Second)

	second := f.sourcemapEvent('a', "TypeError")
	if second["event_id"] == first["event_id"] {
		t.Fatal("the second posting must carry a fresh event_id")
	}
	if res := f.ingest(second); res.Status != http.StatusAccepted {
		t.Fatalf("POST /ingest (after upload): got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(2)
	// Known limit 1: TWO issues, not one — documented above and in contract §12.
	f.waitForIssues(2)
	var issue2 issueRow
	for _, iss := range f.issues() {
		if iss.ID != issue1.ID {
			issue2 = iss
		}
	}
	if issue2.ID == "" || issue2.Fingerprint == issue1.Fingerprint {
		t.Fatalf("expected a second issue with a different fingerprint; issues: %+v", f.issues())
	}
	occ := f.occurrences()
	if occ[1].IssueID != issue2.ID {
		t.Fatalf("the symbolicated occurrence landed on issue %s, want the new issue %s", occ[1].IssueID, issue2.ID)
	}
	assertSymbolicatedFrames(t, storedFrames(t, occ[1]), 'a')
	t.Logf("issue 2 (symbolicated frames, after the upload): id=%s fingerprint=%s — Known limit 1: the same event is on two issues", issue2.ID, issue2.Fingerprint)

	// --- 3. build B: new paths `created` under R1; its event lands on issue 2 -------------------
	status, up, raw = f.uploadFixtureBuild('b')
	if status != http.StatusCreated {
		t.Fatalf("POST /api/sourcemaps (build B): got %d, want 201\n  body: %s", status, raw)
	}
	if len(up.Files) != 8 || up.ReleaseFileCount != 16 {
		t.Fatalf("build B upload body: %s", raw)
	}
	for _, uf := range up.Files {
		if uf.Outcome != "created" {
			t.Fatalf("build B: %s was %q, want created (different chunk hashes are different minified_paths, never a replace)\n  body: %s", uf.MinifiedPath, uf.Outcome, raw)
		}
	}

	third := f.sourcemapEvent('b', "TypeError")
	if res := f.ingest(third); res.Status != http.StatusAccepted {
		t.Fatalf("POST /ingest (build B): got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(3)
	f.waitForIssues(2) // still two: build B created no issue
	occ = f.occurrences()
	if occ[2].IssueID != issue2.ID {
		t.Fatalf("build B's occurrence landed on issue %s, want issue 2 (%s) — the fingerprint saw the chunk hash, not the source position", occ[2].IssueID, issue2.ID)
	}
	assertSymbolicatedFrames(t, storedFrames(t, occ[2]), 'b')
	for _, iss := range f.issues() {
		if iss.ID == issue2.ID && iss.Count != 2 {
			t.Fatalf("issue 2 count = %d, want 2 (build A's and build B's symbolicated occurrences)", iss.Count)
		}
	}
	t.Logf("build B's event (%s col %d) landed on issue 2 %s: fingerprint %s unchanged across builds",
		sourcemapAnchors[0].fileB, sourcemapAnchors[0].columnB, issue2.ID, issue2.Fingerprint)

	// --- 4. the detail loader and the counters --------------------------------------------------
	owner := f.newDashboardUser("owner")
	page := dashboardRequest(t, http.MethodGet, "/issues/"+issue2.ID+"/__data.json", owner, nil)
	if page.Status != http.StatusOK {
		t.Fatalf("GET /issues/%s/__data.json: got %d, want 200\n  body: %s", issue2.ID, page.Status, page.Body)
	}
	for _, want := range []string{`"symbolicated"`, sourcemapAnchors[0].source, sourcemapAnchors[0].name, sourcemapAnchors[0].fileB} {
		if !strings.Contains(page.Body, want) {
			t.Fatalf("the issue-detail loader data does not carry %q\n  body: %.2000s", want, page.Body)
		}
	}

	metrics := readProcessorMetrics(t)
	for _, series := range []string{`sentinel_symbolication_frames_total{`, `outcome="applied"`, `outcome="no_map"`, `sentinel_symbolication_events_total{`, `outcome="applied_all"`, `outcome="none"`, `sentinel_sourcemap_cache_events_total{`} {
		if !strings.Contains(metrics, series) {
			t.Fatalf("/metrics does not carry %s after the run", series)
		}
	}
	t.Logf("processor /metrics after the run:\n%s", metrics)

	// --- 5. the Turbopack index map and the empty stub, through the sections -------------------
	// No SOURCEMAP_NEGATIVE_TTL sleep is owed here: the negative cache is keyed by (project, R1,
	// normalised path), and neither `/app/.next/server/chunks/[turbopack]_runtime.js` nor
	// `/app/.next/server/app/page.js` was looked up in steps 1–4 (every event so far carried only
	// the `/assets/…` anchors), so there is no `no_map` entry to wait out.
	before := metrics
	status, up, raw = f.postSourcemaps([]sourcemapPart{
		{minifiedPath: indexMapRuntimePath, fixturePath: filepath.Join(sourcemapsFixtureDir, "turbopack", "chunks", "[turbopack]_runtime.js.map")},
		{minifiedPath: indexMapStubPath, fixturePath: filepath.Join(sourcemapsFixtureDir, "turbopack", "app", "page.js.map")},
	})
	if status != http.StatusCreated {
		t.Fatalf("POST /api/sourcemaps (index map + stub): got %d, want 201\n  body: %s", status, raw)
	}
	if len(up.Files) != 2 || up.Files[0].Outcome != "created" || up.Files[1].Outcome != "skipped_empty" || up.ReleaseFileCount != 17 {
		t.Fatalf("index map + stub upload: want files[0] created, files[1] skipped_empty, release_file_count 17 (8 + 8 + 1: the stub is not a row, D12)\n  body: %s", raw)
	}
	if up.Files[0].MinifiedPath != indexMapRuntimePath || up.Files[1].MinifiedPath != indexMapStubPath {
		t.Fatalf("index map + stub upload: files[] out of order\n  body: %s", raw)
	}
	t.Logf("step 5 upload response body: %s", raw)

	// M2, through the test's pool. Scoped to the fixture's project: R1 is the release name every
	// run of this case uses, and the `sourcemaps` rows of a previous run's project are only gone
	// once that project's cleanup cascaded.
	var releaseRows, stubRows int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sourcemaps WHERE project_id = $1 AND release_version = $2`,
		f.ProjectID, sourcemapsRelease).Scan(&releaseRows); err != nil {
		t.Fatalf("counting sourcemaps rows of R1: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sourcemaps WHERE project_id = $1 AND release_version = $2 AND minified_path = $3`,
		f.ProjectID, sourcemapsRelease, indexMapStubPath).Scan(&stubRows); err != nil {
		t.Fatalf("counting sourcemaps rows of the stub: %v", err)
	}
	t.Logf("SELECT count(*) FROM sourcemaps WHERE project_id = '%s' AND release_version = 'R1' → %d", f.ProjectID, releaseRows)
	t.Logf("SELECT count(*) FROM sourcemaps WHERE project_id = '%s' AND release_version = 'R1' AND minified_path = '%s' → %d", f.ProjectID, indexMapStubPath, stubRows)
	if releaseRows != 17 {
		t.Fatalf("sourcemaps rows for R1 = %d, want 17 (build A 8 + build B 8 + the runtime index map)", releaseRows)
	}
	if stubRows != 0 {
		t.Fatalf("sourcemaps rows for the stub %s = %d, want 0: a zero-section index map is never stored (D12)", indexMapStubPath, stubRows)
	}

	fifth := f.indexMapEvent("ReferenceError")
	if res := f.ingest(fifth); res.Status != http.StatusAccepted {
		t.Fatalf("POST /ingest (index map event): got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(4)
	occ = f.occurrences()
	frames := storedFrames(t, occ[3])
	if len(frames) != 3+len(sourcemapAnchors) {
		t.Fatalf("stored %d frames, want %d", len(frames), 3+len(sourcemapAnchors))
	}
	// Logged as the JSONB bytes themselves, so "no `symbolicated`/`raw` key" is visible verbatim.
	var rawFrames []json.RawMessage
	if err := json.Unmarshal(occ[3].Stacktrace, &rawFrames); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		t.Logf("step 5 stored frame (%s): %s", []string{"i", "ii", "iii"}[i], rawFrames[i])
	}
	// (i) inside section 4 (offset line 553): wire 600:12 → nodejs/runtime.ts:77:7. in_app is
	// RE-DERIVED from the resolved source (turbopack-grouping §4.2): a `turbopack:///[turbopack]/…`
	// root is false, whatever the SDK sent; `raw.in_app` keeps the SDK's true.
	fr := frames[0]
	if fr.Symbolicated == nil || !*fr.Symbolicated || fr.Raw == nil {
		t.Fatalf("frame (i) was not symbolicated through the section: %+v", fr)
	}
	if fr.File != indexMapResolvedSource || fr.Line != 77 || fr.Column != 7 || fr.Function != indexMapFunction {
		t.Fatalf("frame (i) resolved to %s:%s:%d:%d, want %s:%s:77:7", fr.File, fr.Function, fr.Line, fr.Column, indexMapResolvedSource, indexMapFunction)
	}
	if fr.InApp {
		t.Fatalf("frame (i) in_app = true, want false: %s is a bracketed-root source", fr.File)
	}
	if fr.Raw.File != indexMapRuntimePath || fr.Raw.Line != 600 || fr.Raw.Column != 12 || fr.Raw.Function != indexMapFunction || !fr.Raw.InApp {
		t.Fatalf("frame (i) raw = %+v, want %s:600:12 %s in_app=true", *fr.Raw, indexMapRuntimePath, indexMapFunction)
	}
	// (ii) before the first section (I1) and (iii) on the stub, for which nothing is stored (I14):
	// byte-identical pass-throughs, no `symbolicated`, no `raw`.
	for i, want := range []struct {
		file         string
		line, column int32
	}{{indexMapRuntimePath, 1, 1}, {indexMapStubPath, 1, 5}} {
		fr := frames[1+i]
		if fr.Symbolicated != nil || fr.Raw != nil {
			t.Fatalf("frame %d was rewritten, want a pass-through: %+v", 1+i, fr)
		}
		if fr.File != want.file || fr.Line != want.line || fr.Column != want.column || fr.Function != indexMapFunction || !fr.InApp {
			t.Fatalf("frame %d stored as %+v, want %s:%d:%d %s in_app=true unrewritten", 1+i, fr, want.file, want.line, want.column, indexMapFunction)
		}
	}
	assertSymbolicatedFrames(t, frames[3:], 'a')

	after := readProcessorMetrics(t)
	noSection := metricValue(t, after, `sentinel_symbolication_frames_total`, "no_section")
	noMapDelta := metricValue(t, after, `sentinel_symbolication_frames_total`, "no_map") - metricValue(t, before, `sentinel_symbolication_frames_total`, "no_map")
	partialDelta := metricValue(t, after, `sentinel_symbolication_events_total`, "applied_partial") - metricValue(t, before, `sentinel_symbolication_events_total`, "applied_partial")
	for _, l := range strings.Split(after, "\n") {
		if strings.Contains(l, `outcome="no_section"`) || strings.Contains(l, `outcome="no_map"`) || strings.Contains(l, `outcome="applied_partial"`) {
			t.Logf("step 5 /metrics: %s", l)
		}
	}
	if noSection < 1 {
		t.Fatalf("frames_total{outcome=\"no_section\"} = %v, want >= 1 (frame ii)", noSection)
	}
	if noMapDelta < 1 {
		t.Fatalf("frames_total{outcome=\"no_map\"} grew by %v over step 5, want >= 1 (frame iii)", noMapDelta)
	}
	if partialDelta != 1 {
		t.Fatalf("events_total{outcome=\"applied_partial\"} grew by %v over step 5, want exactly 1", partialDelta)
	}
}

// The Turbopack index-map fixture (tests/fixtures/sourcemaps/README.md, `turbopack/`): the two
// minified paths under the sdk-js wrapper's `serverRoot` shape (contract D18), the source the
// map's fifth section resolves wire 600:12 to, and the `function` every step-5 frame carries —
// the map's `names` are all empty, so it must come back verbatim (I8).
const (
	indexMapRuntimePath    = "/app/.next/server/chunks/[turbopack]_runtime.js"
	indexMapStubPath       = "/app/.next/server/app/page.js"
	indexMapResolvedSource = "turbopack:///[turbopack]/nodejs/runtime.ts"
	indexMapFunction       = "loadChunk"
)

// indexMapEvent builds step 5's javascript event: (i) inside the map's last section, (ii) before
// its first section, (iii) on the stub — all in_app as a Node server frame would be — followed by
// build A's four anchors, so the anchors' assertions still hold on the same occurrence.
func (f *fixture) indexMapEvent(errorClass string) event {
	frames := []map[string]any{
		{"file": indexMapRuntimePath, "line": 600, "column": 12, "function": indexMapFunction, "in_app": true},
		{"file": indexMapRuntimePath, "line": 1, "column": 1, "function": indexMapFunction, "in_app": true},
		{"file": indexMapStubPath, "line": 1, "column": 5, "function": indexMapFunction, "in_app": true},
	}
	for i, a := range sourcemapAnchors {
		frames = append(frames, map[string]any{
			"file": a.fileA, "line": 1, "column": a.columnA, "function": a.minName, "in_app": seedInApp(i),
		})
	}
	return f.newEvent().with(event{
		"platform":        "javascript",
		"release_version": sourcemapsRelease,
		"error_class":     errorClass,
		"message":         "sourcemaps e2e (index map): " + errorClass,
		"event_id":        uuid.NewString(),
		"stacktrace":      frames,
	})
}

// metricValue returns the sample of `<name>{…outcome="<outcome>"…}` in a readProcessorMetrics
// dump, or 0 when the series has not been emitted yet (a counter that was never incremented is
// absent from the exposition, not 0).
func metricValue(t *testing.T, metrics, name, outcome string) float64 {
	t.Helper()
	for _, l := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(l, name+"{") || !strings.Contains(l, `outcome="`+outcome+`"`) {
			continue
		}
		fields := strings.Fields(l)
		var v float64
		if _, err := fmt.Sscanf(fields[len(fields)-1], "%g", &v); err != nil {
			t.Fatalf("parsing %q: %v", l, err)
		}
		return v
	}
	return 0
}

// readProcessorMetrics returns the sentinel_symbolication_* and sentinel_sourcemap_* lines of the
// processor's /metrics, the counters §12 names.
func readProcessorMetrics(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ProcessorHealth+"/metrics", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s/metrics: %v", cfg.ProcessorHealth, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "sentinel_symbolication_") || strings.HasPrefix(l, "sentinel_sourcemap_") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("(no sentinel_symbolication_*/sentinel_sourcemap_* series; %d bytes of /metrics)", len(raw))
	}
	return strings.Join(lines, "\n")
}
