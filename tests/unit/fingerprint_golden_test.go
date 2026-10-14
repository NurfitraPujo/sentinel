package unit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/enrich"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
)

// V6a (sentinel-event-schema-v2 spec §9.2, TASK-06 criterion 1): the fingerprint of every existing fixture
// event is byte-identical before and after normalization moves out of storage and into
// ComputeFingerprint (spec §6.2). golden.json was generated on the UNMODIFIED base (main @ bcbc208) by
// this same test with FINGERPRINT_GOLDEN_UPDATE=1, and committed with it, before any code changed.
//
// The pipeline is what a real event goes through, and deliberately nothing else: JSON decode -> the
// tenancy stamp -> mapping.MapPayloadToEvent -> proto.Marshal -> event.Deserialize (Normalize at the
// base, Prepare after) -> ComputeFingerprint(symbolicate.DefaultPlatforms). It never calls
// ValidatePayload or Prepare itself, so the same file compiles and passes at the base and after.
//
// sentinel-grouping-v2 (2026-10-13, regenerated again 2026-10-14 when an unsymbolicated gated frame stopped hashing its function): golden.json was regenerated, deliberately, when the hash
// input gained its `v2|` prefix, the frame cap moved from 3 to 5 and library frames began to be
// skipped; every value in it is now a v2 value, and `git diff` of the file is the list of fixtures
// whose identity moved. The invariant this test guards is unchanged: moving normalisation or
// enrichment must not move a fingerprint.
//
// normalizable-class.json exists because none of the 18 JS fixtures has an error_class with a token
// the normalizer rewrites: without it this test could not see the one normalization that moves (B10).
// Mutation check: delete the NormalizeString call on ErrorClass and that row goes red.

const fingerprintGoldenPath = "../fixtures/fingerprints/golden.json"

func fingerprintGoldenCorpus(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(sdkJSFixtureDir, "*.json"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(paths), sdkJSFixtureMinimum)
	paths = append(paths,
		"../fixtures/events/sdk-go-contract-event.json",
		"../fixtures/fingerprints/normalizable-class.json",
	)
	sort.Strings(paths)
	return paths
}

func fingerprintOfFixture(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var payload validation.ErrorPayload
	require.NoError(t, json.NewDecoder(bytes.NewReader(raw)).Decode(&payload), "%s: decode", path)
	// The tenancy stamp of main.applyAuthenticatedScope. Neither value is a fingerprint input.
	if payload.ProjectKey == "" {
		payload.ProjectKey = "proj-fingerprint-golden"
	}
	if payload.ProjectID == "" {
		payload.ProjectID = "0f7c1a2e-3b4d-4c5e-8f6a-7b8c9d0e1f2a"
	}

	data, err := proto.Marshal(mapping.MapPayloadToEvent(&payload))
	require.NoError(t, err, "%s: marshal", path)
	evt, err := event.Deserialize(data)
	require.NoError(t, err, "%s: deserialize", path)
	evt.ComputeFingerprint(symbolicate.DefaultPlatforms)
	require.NotEmpty(t, evt.Fingerprint, "%s: fingerprint", path)
	return evt.Fingerprint
}

func TestFingerprintGolden_ExistingFixturesUnchanged(t *testing.T) {
	got := map[string]string{}
	for _, path := range fingerprintGoldenCorpus(t) {
		got[filepath.Base(path)] = fingerprintOfFixture(t, path)
	}

	if os.Getenv("FINGERPRINT_GOLDEN_UPDATE") == "1" {
		b, err := json.MarshalIndent(got, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(fingerprintGoldenPath, append(b, '\n'), 0o644))
		t.Logf("wrote %d fingerprints to %s", len(got), fingerprintGoldenPath)
		return
	}

	raw, err := os.ReadFile(fingerprintGoldenPath)
	require.NoError(t, err, "golden.json must be committed")
	var want map[string]string
	require.NoError(t, json.Unmarshal(raw, &want))
	require.Len(t, got, len(want), "the corpus and golden.json must name the same fixtures")
	for name, fp := range want {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, fp, got[name], "%s: the fingerprint moved; existing issues would split or merge (contract §2)", name)
		})
	}
}

// TestFingerprintGolden_EnrichedFixturesUnchanged (sentinel-event-enrichment §7.3, §10.2 "fingerprints
// unchanged"): every golden fixture, run through enrich.Apply with a full request Info (public IP
// 81.2.69.142, a Chrome UA) and the fixture GeoLite2 database, still has its golden fingerprint. No
// enriched field is a fingerprint input. A fixture with no IP is given "{{auto}}" so the IP rule fills
// it on every platform (I1; a browser-runtime item is otherwise excluded by I3t), and the case asserts
// geo WAS filled and contexts were derived, so it is not vacuous.
func TestFingerprintGolden_EnrichedFixturesUnchanged(t *testing.T) {
	raw, err := os.ReadFile(fingerprintGoldenPath)
	require.NoError(t, err)
	var want map[string]string
	require.NoError(t, json.Unmarshal(raw, &want))

	_, enricher := enrReadyLoader(t)
	var geoFilled, uaFilled int
	for _, path := range fingerprintGoldenCorpus(t) {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(path)
			require.NoError(t, err)
			var payload validation.ErrorPayload
			require.NoError(t, json.NewDecoder(bytes.NewReader(b)).Decode(&payload))
			if payload.ProjectKey == "" {
				payload.ProjectKey = "proj-fingerprint-golden"
			}
			if payload.ProjectID == "" {
				payload.ProjectID = "0f7c1a2e-3b4d-4c5e-8f6a-7b8c9d0e1f2a"
			}
			sentGeo := payload.User != nil && payload.User.Geo != nil
			if payload.User == nil {
				payload.User = &validation.UserInfo{}
			}
			if payload.User.IPAddress == "" {
				payload.User.IPAddress = enrich.AutoIP
			}
			_, sentBrowser := payload.Contexts["browser"]

			enrich.Apply(&payload, enrFullInfo(), enricher)
			require.NotNil(t, payload.User.Geo, "geo is set (sent or filled)")
			if !sentGeo {
				require.Equal(t, "GB", payload.User.Geo.CountryCode, "the lookup filled geo")
				geoFilled++
			}
			if !sentBrowser && payload.Contexts["browser"] != nil {
				uaFilled++
			}

			data, err := proto.Marshal(mapping.MapPayloadToEvent(&payload))
			require.NoError(t, err)
			evt, err := event.Deserialize(data)
			require.NoError(t, err)
			evt.ComputeFingerprint(symbolicate.DefaultPlatforms)
			require.Equal(t, want[name], evt.Fingerprint, "%s: enrichment moved the fingerprint", name)
		})
	}
	assert.Positive(t, geoFilled, "at least one fixture had geo filled by the lookup")
	assert.Positive(t, uaFilled, "at least one fixture had a browser context derived")
}
