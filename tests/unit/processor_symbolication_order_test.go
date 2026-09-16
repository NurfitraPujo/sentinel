package unit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The pipeline's order since sentinel-sourcemaps §4.4 — Deserialize, resolve the project,
// event.Symbolicate, event.ComputeFingerprint — driven end to end without a database: the stage
// is the real symbolicate.Service over a stubbed Store (one row) and Fetcher (the committed Vite
// fixture map, apps/processor-go/symbolicate/testdata/vite). The positions come from that
// package's hand-computed table: wire column 10 on generated line 1 is `subtotal` at
// main.ts 6:17, and the map's one source is `../../src/main.ts`.

const (
	orderProjectID  = "11111111-1111-1111-1111-111111111111"
	orderRelease    = "1.2.3"
	orderMinified   = "/assets/index-d_kdqh_W.js"
	orderStorageKey = "sourcemaps/p/r/m-fixture.map"
	orderSource     = "../../src/main.ts"
)

// newFixtureStage builds a symbolicate.Service whose only map is the committed fixture, keyed
// under (orderProjectID, orderRelease, orderMinified). The stub Store returns that row for that
// triple only — any other project gets `no_map`, which is also §4.4 step 3's tenancy rule in
// miniature.
func newFixtureStage(t *testing.T) *symbolicate.Service {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "apps", "processor-go", "symbolicate", "testdata", "vite", "dist", "assets", "index-d_kdqh_W.js.map"))
	require.NoError(t, err, "the committed fixture map must be readable from tests/unit")
	sum := sha256.Sum256(raw)
	row := &symbolicate.Sourcemap{
		ID: "map-fixture", ProjectID: orderProjectID, ReleaseVersion: orderRelease, MinifiedPath: orderMinified,
		StorageKey: orderStorageKey, SizeBytes: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]),
	}
	store := symbolicate.StoreFunc(func(_ context.Context, projectID, releaseVersion, minifiedPath string) (*symbolicate.Sourcemap, error) {
		if projectID == row.ProjectID && releaseVersion == row.ReleaseVersion && minifiedPath == row.MinifiedPath {
			return row, nil
		}
		return nil, nil
	})
	fetcher := symbolicate.FetcherFunc(func(_ context.Context, key string) ([]byte, error) {
		require.Equal(t, orderStorageKey, key)
		return raw, nil
	})
	stage, err := symbolicate.New(symbolicate.DefaultConfig(), store, fetcher)
	require.NoError(t, err)
	t.Cleanup(stage.Close)
	return stage
}

// javascriptProtoEvent is a browser event of the fixture's release: one frame with a column that
// resolves (wire column 10 → `subtotal`), one frame the map cannot resolve because it has no
// column (`no_column`, passes through), so the occurrence is applied_partial.
func javascriptProtoEvent() *sentinelv1.ErrorEvent {
	e := validProtoEvent()
	e.Platform = "javascript"
	e.ProjectId = orderProjectID
	e.ReleaseVersion = orderRelease
	e.ErrorClass = "TypeError"
	e.Stacktrace = []*sentinelv1.StackFrame{
		{File: "https://app.example.com" + orderMinified + "?v=1", Line: 1, Column: 10, Function: "t", InApp: true},
		{File: "/assets/vendor-3f2a1c.js", Line: 1, Function: "n", InApp: true},
	}
	return e
}

// TestSymbolicate_ClientFingerprintIsStoredVerbatimWithSymbolicatedFrames is flow S7 — the case
// contract §4.4's first paragraph names: an event carrying a client fingerprint and NO override
// has its frames symbolicated anyway, and the stored fingerprint is the client's value verbatim.
// The custom-fingerprint early return is untouched by the stage; the frames are not.
func TestSymbolicate_ClientFingerprintIsStoredVerbatimWithSymbolicatedFrames(t *testing.T) {
	stage := newFixtureStage(t)

	protoEvent := javascriptProtoEvent()
	protoEvent.Fingerprint = "checkout-timeout"
	protoEvent.FingerprintOverride = false

	evt, err := event.Deserialize(marshalProtoEvent(t, protoEvent))
	require.NoError(t, err)

	res := evt.Symbolicate(context.Background(), stage, orderProjectID)
	require.Equal(t, []symbolicate.Outcome{symbolicate.OutcomeApplied, symbolicate.OutcomeNoColumn}, res.Outcomes)
	require.Equal(t, symbolicate.EventAppliedPartial, res.EventOutcome())

	// The frames WERE rewritten — this is not the early return skipping the stage.
	require.True(t, evt.Stacktrace[0].Symbolicated)
	assert.Equal(t, orderSource, evt.Stacktrace[0].File)
	assert.Equal(t, int32(6), evt.Stacktrace[0].Line)
	assert.Equal(t, int32(17), evt.Stacktrace[0].Column)
	assert.Equal(t, "subtotal", evt.Stacktrace[0].Function)

	evt.ComputeFingerprint(symbolicate.DefaultPlatforms)
	assert.Equal(t, "checkout-timeout", evt.Fingerprint, "S7: the client value is stored verbatim whether or not the frames were rewritten")
}

// TestSymbolicate_FingerprintIsComputedOnTheRewrittenFrames is the order the contract fixes —
// "symbolicated after the project id is known and before fingerprint.Compute is called" — as a
// value: the computed fingerprint equals Compute over the REWRITTEN frames (the original source
// path and the mapping's name), not over the frames as sent. Move ComputeFingerprint before
// Symbolicate and the first assertion fails while the second passes.
func TestSymbolicate_FingerprintIsComputedOnTheRewrittenFrames(t *testing.T) {
	stage := newFixtureStage(t)

	evt, err := event.Deserialize(marshalProtoEvent(t, javascriptProtoEvent()))
	require.NoError(t, err)
	require.Empty(t, evt.Fingerprint, "Deserialize no longer computes identity")

	asSent := fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production", ErrorClass: "TypeError", Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
		Stacktrace: []fingerprint.StackFrame{
			{File: "https://app.example.com" + orderMinified + "?v=1", Line: 1, Function: "t", InApp: true},
			{File: "/assets/vendor-3f2a1c.js", Line: 1, Function: "n", InApp: true},
		},
	})

	evt.Symbolicate(context.Background(), stage, orderProjectID)
	evt.ComputeFingerprint(symbolicate.DefaultPlatforms)

	rewritten := fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production", ErrorClass: "TypeError", Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
		Stacktrace: []fingerprint.StackFrame{
			{File: orderSource, Line: 6, Function: "subtotal", InApp: true, Symbolicated: true},
			{File: "/assets/vendor-3f2a1c.js", Line: 1, Function: "n", InApp: true},
		},
	})
	assert.Equal(t, rewritten, evt.Fingerprint, "identity is computed over the symbolicated frames")
	assert.NotEqual(t, asSent, evt.Fingerprint, "and it differs from the raw-frame identity, so the order is observable")
}

// TestSymbolicate_ComputeFingerprintCarriesTheSymbolicatedFlag is decision 44 on the call path
// (B3): ComputeFingerprint must hand each frame's step-6 record to fingerprint.Compute, or the
// gate in Compute is unreachable and a symbolicated `UserProfileCard.tsx` is hash-stripped
// anyway. The committed fixture map's one source is `main.ts`, which no clause strips, so the
// rewrite is applied by hand here exactly as the stage writes it (File replaced, Raw kept,
// Symbolicated set): the fingerprint must be the hash over that source path verbatim next to the
// un-rewritten vendor frame stripped as before. Drop `Symbolicated: f.Symbolicated` from
// ComputeFingerprint and this goes red.
func TestSymbolicate_ComputeFingerprintCarriesTheSymbolicatedFlag(t *testing.T) {
	evt, err := event.Deserialize(marshalProtoEvent(t, javascriptProtoEvent()))
	require.NoError(t, err)
	f := &evt.Stacktrace[0]
	f.Raw = &symbolicate.RawFrame{File: f.File, Line: f.Line, Column: f.Column, Function: f.Function}
	f.File, f.Line, f.Column, f.Function, f.Symbolicated = "src/components/UserProfileCard.tsx", 42, 7, "onClick", true

	evt.ComputeFingerprint(symbolicate.DefaultPlatforms)

	want := fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production", ErrorClass: "TypeError", Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
		Stacktrace: []fingerprint.StackFrame{
			{File: "src/components/UserProfileCard.tsx", Line: 42, Function: "onClick", InApp: true, Symbolicated: true},
			{File: "/assets/vendor-3f2a1c.js", Line: 1, Function: "n", InApp: true},
		},
	})
	assert.Equal(t, want, evt.Fingerprint)
	stripped := fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production", ErrorClass: "TypeError", Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
		Stacktrace: []fingerprint.StackFrame{
			{File: "src/components/UserProfileCard.tsx", Line: 42, Function: "onClick", InApp: true},
			{File: "/assets/vendor-3f2a1c.js", Line: 1, Function: "n", InApp: true},
		},
	})
	assert.NotEqual(t, stripped, evt.Fingerprint, "the flag reached Compute: the source path was not read as `src/components/*.tsx`")
	assert.Equal(t, "src/components/UserProfileCard.tsx", evt.Stacktrace[0].File, "the stored frame is untouched")
}

// TestSymbolicate_StoredFrameShape pins §4.4 steps 6 and 7 on the JSON processEventInternal
// marshals into error_occurrences.stacktrace: an applied frame carries `symbolicated: true` and
// `raw` with the values the SDK sent (file, line, column, function and — since
// sentinel-sourcemaps-turbopack-grouping §4.2, D4 — in_app), a passed-through frame carries
// NEITHER key — absent, not false — so a pre-feature occurrence and an unsymbolicated one are
// byte-identical. `in_app` on the applied frame is the re-derived value: the fixture's
// `../../src/main.ts` is A13 → true, the same value the SDK sent here; the pass-through frame's
// is the SDK's, untouched.
func TestSymbolicate_StoredFrameShape(t *testing.T) {
	stage := newFixtureStage(t)
	evt, err := event.Deserialize(marshalProtoEvent(t, javascriptProtoEvent()))
	require.NoError(t, err)
	evt.Symbolicate(context.Background(), stage, orderProjectID)

	stored, err := json.Marshal(evt.Stacktrace)
	require.NoError(t, err)
	var frames []map[string]any
	require.NoError(t, json.Unmarshal(stored, &frames))
	require.Len(t, frames, 2)

	applied := frames[0]
	assert.Equal(t, true, applied["symbolicated"])
	assert.Equal(t, orderSource, applied["file"])
	assert.Equal(t, float64(6), applied["line"])
	assert.Equal(t, float64(17), applied["column"])
	assert.Equal(t, "subtotal", applied["function"])
	assert.Equal(t, true, applied["in_app"])
	assert.Equal(t, map[string]any{
		"file": "https://app.example.com" + orderMinified + "?v=1", "line": float64(1), "column": float64(10), "function": "t", "in_app": true,
	}, applied["raw"], "raw is the SDK's values, before the rewrite")

	passed := frames[1]
	_, hasSymbolicated := passed["symbolicated"]
	_, hasRaw := passed["raw"]
	assert.False(t, hasSymbolicated, "a passed-through frame has no `symbolicated` key (absent, not false)")
	assert.False(t, hasRaw, "a passed-through frame has no `raw` key")
	assert.Equal(t, map[string]any{"file": "/assets/vendor-3f2a1c.js", "line": float64(1), "function": "n", "in_app": true}, passed,
		"byte-identical to a pre-feature frame: file, line, function, in_app and nothing else")
}

// TestSymbolicate_InAppCopiedBackForRewrittenFramesOnly is sentinel-sourcemaps-turbopack-grouping
// §4.2 on the call path (flows P3, P7): ErrorEvent.Symbolicate copies the stage's re-derived InApp
// back for a rewritten frame and ONLY for one. The SDK sends `false` on both frames (an
// init({ inApp }) refinement, A4's shape): the rewritten frame's `in_app` becomes the rule's
// `true` (`../../src/main.ts` is A13) with the SDK's `false` recorded under `raw.in_app` — no
// omitempty, so a false is a false (D4) — and the pass-through frame's JSON is byte-identical to
// what the SDK sent. Drop `e.Stacktrace[i].InApp = f.InApp` from Symbolicate and this goes red.
func TestSymbolicate_InAppCopiedBackForRewrittenFramesOnly(t *testing.T) {
	stage := newFixtureStage(t)
	proto := javascriptProtoEvent()
	proto.Stacktrace[0].InApp = false
	proto.Stacktrace[1].InApp = false
	evt, err := event.Deserialize(marshalProtoEvent(t, proto))
	require.NoError(t, err)
	before, err := json.Marshal(evt.Stacktrace[1])
	require.NoError(t, err)

	res := evt.Symbolicate(context.Background(), stage, orderProjectID)
	require.Equal(t, []symbolicate.Outcome{symbolicate.OutcomeApplied, symbolicate.OutcomeNoColumn}, res.Outcomes)

	assert.True(t, evt.Stacktrace[0].InApp, "the rewritten frame carries the rule's value, not the SDK's false")
	require.NotNil(t, evt.Stacktrace[0].Raw)
	assert.False(t, evt.Stacktrace[0].Raw.InApp, "the SDK's false is kept under raw")
	stored, err := json.Marshal(evt.Stacktrace[0])
	require.NoError(t, err)
	var applied map[string]any
	require.NoError(t, json.Unmarshal(stored, &applied))
	assert.Equal(t, true, applied["in_app"])
	assert.Equal(t, false, applied["raw"].(map[string]any)["in_app"], "raw.in_app is present when false: no omitempty")

	after, err := json.Marshal(evt.Stacktrace[1])
	require.NoError(t, err)
	assert.False(t, evt.Stacktrace[1].InApp)
	assert.Equal(t, string(before), string(after), "a pass-through frame is byte-identical to what the SDK sent, in_app included")
}

// TestSymbolicate_TenancyAtTheStage: the lookup is keyed by the RESOLVED project id the caller
// passes, never by anything in a frame — project B's event, with frames naming project A's exact
// minified path and release, gets `no_map` (§4.4 step 3; §11's tenancy case proves the same
// through the real Store on a seeded database in tests/integration).
func TestSymbolicate_TenancyAtTheStage(t *testing.T) {
	stage := newFixtureStage(t)
	evt, err := event.Deserialize(marshalProtoEvent(t, javascriptProtoEvent()))
	require.NoError(t, err)
	res := evt.Symbolicate(context.Background(), stage, "22222222-2222-2222-2222-222222222222")
	assert.Equal(t, []symbolicate.Outcome{symbolicate.OutcomeNoMap, symbolicate.OutcomeNoColumn}, res.Outcomes)
	assert.False(t, evt.Stacktrace[0].Symbolicated)
	assert.Nil(t, evt.Stacktrace[0].Raw)
}

// ---- sentinel-sourcemaps-turbopack-grouping §4.2 through the same harness (flows P5, P6) ------

// The committed Turbopack-shaped map (apps/processor-go/symbolicate/testdata/turbopack/README.md):
// twenty `sources`, contract §4.2's rewritten A-rows verbatim, no `sourceRoot`, one resolvable
// segment per generated line so wire position (n, 1) on the chunk resolves to sources[n-1] at
// 1:1. The rows this file drives, by wire line: 1 = A1 (react-dom-client, the map's one `names`
// entry `renderWithHooks`), 2 = A2 (next's app-router), 4 = A4 (`[project]/src/…/ChatList.tsx`),
// 7 = A7 (`[turbopack]/shared/runtime-utils.ts`).
const (
	turbopackMinified   = "/_next/static/chunks/ac0ebedaba77188c.js"
	turbopackStorageKey = "sourcemaps/p/r/turbopack-fixture.map"
	turbopackReactDOM   = "turbopack:///[project]/node_modules/.pnpm/react-dom@19.2.4_react@19.2.4/node_modules/react-dom/cjs/react-dom-client.production.js"
	turbopackAppRouter  = "turbopack:///[project]/node_modules/.pnpm/next@15.5.21_…/node_modules/next/dist/client/components/app-router.js"
	turbopackChatList   = "turbopack:///[project]/src/features/chats/components/ChatList.tsx"
	turbopackRuntime    = "turbopack:///[turbopack]/shared/runtime-utils.ts"
)

// newTurbopackStage is newFixtureStage over the Turbopack-shaped map instead of the Vite one:
// the stub Store's only row is (orderProjectID, orderRelease, turbopackMinified).
func newTurbopackStage(t *testing.T) *symbolicate.Service {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "apps", "processor-go", "symbolicate", "testdata", "turbopack", "ac0ebedaba77188c.js.map"))
	require.NoError(t, err, "the committed Turbopack fixture map must be readable from tests/unit")
	sum := sha256.Sum256(raw)
	row := &symbolicate.Sourcemap{
		ID: "map-turbopack", ProjectID: orderProjectID, ReleaseVersion: orderRelease, MinifiedPath: turbopackMinified,
		StorageKey: turbopackStorageKey, SizeBytes: int64(len(raw)), SHA256: hex.EncodeToString(sum[:]),
	}
	store := symbolicate.StoreFunc(func(_ context.Context, projectID, releaseVersion, minifiedPath string) (*symbolicate.Sourcemap, error) {
		if projectID == row.ProjectID && releaseVersion == row.ReleaseVersion && minifiedPath == row.MinifiedPath {
			return row, nil
		}
		return nil, nil
	})
	fetcher := symbolicate.FetcherFunc(func(_ context.Context, key string) ([]byte, error) {
		require.Equal(t, turbopackStorageKey, key)
		return raw, nil
	})
	stage, err := symbolicate.New(symbolicate.DefaultConfig(), store, fetcher)
	require.NoError(t, err)
	t.Cleanup(stage.Close)
	return stage
}

// turbopackFrame is a browser frame on the Turbopack chunk at wire (line, 1), as the SDK would
// send it: the page's origin on the file, a minified function name, and in_app as the SDK's
// rule decided it.
func turbopackFrame(line int32, function string, inApp bool) *sentinelv1.StackFrame {
	return &sentinelv1.StackFrame{File: "https://app.example.com" + turbopackMinified, Line: line, Column: 1, Function: function, InApp: inApp}
}

// turbopackProtoEvent is a browser event of the fixture's release with the given frames.
func turbopackProtoEvent(frames ...*sentinelv1.StackFrame) *sentinelv1.ErrorEvent {
	e := validProtoEvent()
	e.Platform = "javascript"
	e.ProjectId = orderProjectID
	e.ReleaseVersion = orderRelease
	e.ErrorClass = "TypeError"
	e.Stacktrace = frames
	return e
}

func turbopackCompute(frames ...fingerprint.StackFrame) string {
	return fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production", ErrorClass: "TypeError", Platform: "javascript", NormalisePlatforms: symbolicate.DefaultPlatforms,
		Stacktrace: frames,
	})
}

// TestSymbolicationOrder_P5_OneAppFrameAmongVendor is contract §4.2's worked example (flow P5)
// on the fingerprint value, end to end: a browser render error whose SDK frames are
// [react-dom ×6, src/…/ChatList.tsx, react-dom ×2], every one sent `in_app: true` (the browser
// SDK's rule marks the page's own chunks in-app). After the stage the six react-dom frames and
// the two below are `false` (A1, rule 1), ChatList.tsx is `true` (A4's path, rule 3), and
// Compute's appFrames loop reaches ChatList as the FIRST in_app frame — so the hash input is
// `<env>|TypeError|…/ChatList.tsx:ChatList`, one app frame (MaxAppFrames is a cap, not a
// minimum). Drop the InApp copy-back from ErrorEvent.Symbolicate, or the rule from the stage,
// and the fingerprint becomes the react-dom triple instead.
func TestSymbolicationOrder_P5_OneAppFrameAmongVendor(t *testing.T) {
	stage := newTurbopackStage(t)
	proto := turbopackProtoEvent(
		turbopackFrame(1, "s2", true), turbopackFrame(1, "s2", true), turbopackFrame(1, "s2", true),
		turbopackFrame(1, "s2", true), turbopackFrame(1, "s2", true), turbopackFrame(1, "s2", true),
		turbopackFrame(4, "ChatList", true),
		turbopackFrame(1, "s2", true), turbopackFrame(1, "s2", true),
	)
	evt, err := event.Deserialize(marshalProtoEvent(t, proto))
	require.NoError(t, err)
	require.Len(t, evt.Stacktrace, 9)

	res := evt.Symbolicate(context.Background(), stage, orderProjectID)
	require.Equal(t, symbolicate.EventAppliedAll, res.EventOutcome(), "every frame resolves through the fixture")
	for i, f := range evt.Stacktrace {
		require.True(t, f.Symbolicated, "frame %d", i)
		require.NotNil(t, f.Raw, "frame %d", i)
		assert.True(t, f.Raw.InApp, "frame %d: the SDK's true is under raw.in_app", i)
		if i == 6 {
			assert.Equal(t, turbopackChatList, f.File)
			assert.True(t, f.InApp, "the one [project]/src frame is the app's (A4's path, rule 3)")
			continue
		}
		assert.Equal(t, turbopackReactDOM, f.File, "frame %d", i)
		assert.Equal(t, "renderWithHooks", f.Function, "frame %d: the map's one name", i)
		assert.False(t, f.InApp, "frame %d: a node_modules source is vendor (A1, rule 1)", i)
	}

	evt.ComputeFingerprint(symbolicate.DefaultPlatforms)

	want := turbopackCompute(fingerprint.StackFrame{File: turbopackChatList, Line: 1, Function: "ChatList", InApp: true, Symbolicated: true})
	assert.Equal(t, want, evt.Fingerprint, "P5: the hash input is env, class and the single src frame")

	// The two identities this must NOT be: the frames as sent (in_app true on all nine, so the
	// react-dom triple at the top), and the rewritten paths with the SDK's flag still in place.
	asSent := turbopackCompute(
		fingerprint.StackFrame{File: "https://app.example.com" + turbopackMinified, Line: 1, Function: "s2", InApp: true},
		fingerprint.StackFrame{File: "https://app.example.com" + turbopackMinified, Line: 1, Function: "s2", InApp: true},
		fingerprint.StackFrame{File: "https://app.example.com" + turbopackMinified, Line: 1, Function: "s2", InApp: true},
	)
	assert.NotEqual(t, asSent, evt.Fingerprint)
	rewrittenSDKFlag := turbopackCompute(
		fingerprint.StackFrame{File: turbopackReactDOM, Line: 1, Function: "renderWithHooks", InApp: true, Symbolicated: true},
		fingerprint.StackFrame{File: turbopackReactDOM, Line: 1, Function: "renderWithHooks", InApp: true, Symbolicated: true},
		fingerprint.StackFrame{File: turbopackReactDOM, Line: 1, Function: "renderWithHooks", InApp: true, Symbolicated: true},
	)
	assert.NotEqual(t, rewrittenSDKFlag, evt.Fingerprint, "the re-derived flag reached Compute, not the SDK's")
}

// TestSymbolicationOrder_P6_AllVendorFallsBackToTopThree is flow P6: a pure vendor stack (a Next
// runtime error with no app frame), every frame sent `in_app: true` and every frame re-derived
// `false`. Compute's existing P4-3 fallback takes the top three frames regardless of the flag —
// today's behaviour for a Go stack with no in_app frame, unchanged — so the fingerprint is the
// hash over the first three REWRITTEN frames, not env+class alone and not the frames as sent.
func TestSymbolicationOrder_P6_AllVendorFallsBackToTopThree(t *testing.T) {
	stage := newTurbopackStage(t)
	proto := turbopackProtoEvent(
		turbopackFrame(1, "s2", true), // A1: react-dom-client
		turbopackFrame(2, "a7", true), // A2: next's app-router
		turbopackFrame(7, "r", true),  // A7: [turbopack]/shared/runtime-utils.ts
		turbopackFrame(1, "s2", true), // deeper detail the cap must drop
		turbopackFrame(2, "a7", true),
	)
	evt, err := event.Deserialize(marshalProtoEvent(t, proto))
	require.NoError(t, err)

	res := evt.Symbolicate(context.Background(), stage, orderProjectID)
	require.Equal(t, symbolicate.EventAppliedAll, res.EventOutcome())
	for i, f := range evt.Stacktrace {
		require.True(t, f.Symbolicated, "frame %d", i)
		assert.False(t, f.InApp, "frame %d: every frame is vendor after the rule", i)
		assert.True(t, f.Raw.InApp, "frame %d: the SDK's true is under raw.in_app", i)
	}

	evt.ComputeFingerprint(symbolicate.DefaultPlatforms)

	topThree := turbopackCompute(
		fingerprint.StackFrame{File: turbopackReactDOM, Line: 1, Function: "renderWithHooks", InApp: false, Symbolicated: true},
		fingerprint.StackFrame{File: turbopackAppRouter, Line: 1, Function: "a7", InApp: false, Symbolicated: true},
		fingerprint.StackFrame{File: turbopackRuntime, Line: 1, Function: "r", InApp: false, Symbolicated: true},
	)
	assert.Equal(t, topThree, evt.Fingerprint, "P6: the P4-3 fallback over the top three rewritten frames")
	assert.NotEqual(t, turbopackCompute(), evt.Fingerprint, "not env+class alone: the fallback ran")
	asSent := turbopackCompute(
		fingerprint.StackFrame{File: "https://app.example.com" + turbopackMinified, Line: 1, Function: "s2", InApp: true},
		fingerprint.StackFrame{File: "https://app.example.com" + turbopackMinified, Line: 2, Function: "a7", InApp: true},
		fingerprint.StackFrame{File: "https://app.example.com" + turbopackMinified, Line: 7, Function: "r", InApp: true},
	)
	assert.NotEqual(t, asSent, evt.Fingerprint, "the fallback reads the rewritten frames, not the frames as sent")
}

// TestSymbolicationOrder_StoredFrameKeys pins contract §4.2's two stored-shape examples on their
// KEYS, over the Turbopack fixture: a rewritten frame (A1's react-dom frame the SDK sent as
// in_app) carries `in_app`, `symbolicated` and `raw` with `raw.in_app` inside; a frame the
// stage did not rewrite (A18's shape, `/app/.next/server/chunks/_052cc036._.js` — a minified
// path the stub Store has no row for, `no_map`) carries neither `raw` nor `symbolicated` and is
// byte-identical to today: file, line, column, function, in_app and nothing else.
func TestSymbolicationOrder_StoredFrameKeys(t *testing.T) {
	stage := newTurbopackStage(t)
	proto := turbopackProtoEvent(
		turbopackFrame(1, "s2", true),
		&sentinelv1.StackFrame{File: "/app/.next/server/chunks/_052cc036._.js", Line: 1, Column: 314, Function: "c", InApp: true},
	)
	evt, err := event.Deserialize(marshalProtoEvent(t, proto))
	require.NoError(t, err)

	res := evt.Symbolicate(context.Background(), stage, orderProjectID)
	require.Equal(t, []symbolicate.Outcome{symbolicate.OutcomeApplied, symbolicate.OutcomeNoMap}, res.Outcomes)

	stored, err := json.Marshal(evt.Stacktrace)
	require.NoError(t, err)
	var frames []map[string]any
	require.NoError(t, json.Unmarshal(stored, &frames))
	require.Len(t, frames, 2)

	rewritten := frames[0]
	assert.ElementsMatch(t, []string{"file", "line", "column", "function", "in_app", "symbolicated", "raw"}, keysOf(rewritten),
		"the rewritten frame: the contract's first example, on keys")
	assert.Equal(t, false, rewritten["in_app"], "the re-derived value")
	assert.Equal(t, true, rewritten["symbolicated"])
	raw, ok := rewritten["raw"].(map[string]any)
	require.True(t, ok, "raw is an object")
	assert.ElementsMatch(t, []string{"file", "line", "column", "function", "in_app"}, keysOf(raw), "raw carries in_app beside the four position keys")
	assert.Equal(t, true, raw["in_app"], "the value the SDK sent")

	passed := frames[1]
	assert.ElementsMatch(t, []string{"file", "line", "column", "function", "in_app"}, keysOf(passed),
		"the pass-through frame: the contract's second example — no raw, no symbolicated")
	assert.Equal(t, map[string]any{"file": "/app/.next/server/chunks/_052cc036._.js", "line": float64(1), "column": float64(314), "function": "c", "in_app": true}, passed)
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
