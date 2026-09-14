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
			{File: orderSource, Line: 6, Function: "subtotal", InApp: true},
			{File: "/assets/vendor-3f2a1c.js", Line: 1, Function: "n", InApp: true},
		},
	})
	assert.Equal(t, rewritten, evt.Fingerprint, "identity is computed over the symbolicated frames")
	assert.NotEqual(t, asSent, evt.Fingerprint, "and it differs from the raw-frame identity, so the order is observable")
}

// TestSymbolicate_StoredFrameShape pins §4.4 steps 6 and 7 on the JSON processEventInternal
// marshals into error_occurrences.stacktrace: an applied frame carries `symbolicated: true` and
// `raw` with the values the SDK sent (file, line, column, function), a passed-through frame carries
// NEITHER key — absent, not false — so a pre-feature occurrence and an unsymbolicated one are
// byte-identical. `in_app` is untouched on both.
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
		"file": "https://app.example.com" + orderMinified + "?v=1", "line": float64(1), "column": float64(10), "function": "t",
	}, applied["raw"], "raw is the SDK's values, before the rewrite")

	passed := frames[1]
	_, hasSymbolicated := passed["symbolicated"]
	_, hasRaw := passed["raw"]
	assert.False(t, hasSymbolicated, "a passed-through frame has no `symbolicated` key (absent, not false)")
	assert.False(t, hasRaw, "a passed-through frame has no `raw` key")
	assert.Equal(t, map[string]any{"file": "/assets/vendor-3f2a1c.js", "line": float64(1), "function": "n", "in_app": true}, passed,
		"byte-identical to a pre-feature frame: file, line, function, in_app and nothing else")
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
