package unit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

// The wire, cross-language (sentinel-sdk-js contract §11 row 1, flows D6, decision 30): every JSON event the
// JavaScript SDK's vitest suite serialises into packages/sdk-js/tests/fixtures/*.json — committed, so this side
// never needs node — is fed through exactly what the ingestor runs on a request: a strict decode into
// validation.ErrorPayload, the tenancy stamp of main.go's applyAuthenticatedScope, validation.ValidatePayload,
// mapping.MapPayloadToEvent and protovalidate over the compiled descriptor. This is the only mechanical check
// that the SDK's output is what the ingestor accepts; a runtime 400 is otherwise silent to the host (Fact 3).
//
// The decoder runs with DisallowUnknownFields (the tests/contract convention): a key the ingestor does not decode
// — a top-level `source`, which is a proto field but not an ErrorPayload one, or a renamed field — fails the test
// instead of being silently dropped on the wire. `project_key` and `project_id` ARE ErrorPayload fields, so the
// "never sent" rule of contract §4.3 is asserted separately on the raw object below.
//
// Lives in tests/unit (root module, no build tag) on purpose: go-root runs `go test ./tests/unit/...` on push to
// main; tests/contract carries `//go:build contract` and runs only from workflow_dispatch.

const sdkJSFixtureDir = "../../packages/sdk-js/tests/fixtures"
const sdkJSGoldenEstimateDir = "../../packages/sdk-js/tests/fixture-estimates/golden"

// sdkJSGoldenEstimate loads the committed sidecar for a golden fixture (spec §7.3): the estimator's
// protoSizeEstimate over the event `emit` wrote, an upper bound proto.Size must never exceed.
func sdkJSGoldenEstimate(t *testing.T, fixtureName string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sdkJSGoldenEstimateDir, fixtureName+".json"))
	require.NoError(t, err, "%s: missing estimate sidecar under %s — run the sdk-js suite locally and commit it", fixtureName, sdkJSGoldenEstimateDir)
	var sidecar struct {
		ProtoSizeEstimate int `json:"protoSizeEstimate"`
	}
	require.NoError(t, json.Unmarshal(raw, &sidecar))
	return sidecar.ProtoSizeEstimate
}

// The lower bound contract §11 lists: one fixture per named rule (be.md acceptance criterion 2).
const sdkJSFixtureMinimum = 13

// sdkJSStampAuthenticatedScope mirrors main.applyAuthenticatedScope for a PROJECT-scoped key (the SDK's server
// path, contract §2.1): the middleware resolved the project from the credential, the body names no project, and
// the handler overwrites both fields from the context before svc.Ingest validates. Without the stamp,
// ValidatePayload returns `project_key is required` — the one error a key-authenticated request never sees.
func sdkJSStampAuthenticatedScope(t *testing.T, name string, payload *validation.ErrorPayload) {
	t.Helper()
	const authProjectKey = "proj-sdk-js-fixture"
	const authProjectID = "0f7c1a2e-3b4d-4c5e-8f6a-7b8c9d0e1f2a"
	// A project-scoped key rejects a body naming a DIFFERENT project (VERIFIED_STATE.md S6); the SDK sends
	// neither field, which is what makes the stamp unconditional here.
	require.Empty(t, payload.ProjectKey, "%s: the SDK must never send project_key (contract §4.3)", name)
	require.Empty(t, payload.ProjectID, "%s: the SDK must never send project_id (contract §4.3)", name)
	payload.ProjectKey = authProjectKey
	payload.ProjectID = authProjectID
}

func TestSDKJSFixtures_AcceptedByIngestor(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(sdkJSFixtureDir, "*.json"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(paths), sdkJSFixtureMinimum,
		"contract §11 row 1 lists at least %d fixtures; found %d under %s", sdkJSFixtureMinimum, len(paths), sdkJSFixtureDir)

	v, err := protovalidate.New()
	require.NoError(t, err)

	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)

			// The "never sent" keys (contract §4.3's event table, decision 21): project_key / project_id are
			// decodable ErrorPayload fields and `source` is a proto field, so none of them is caught by the
			// strict decode alone.
			var top map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw, &top))
			for _, key := range []string{"project_key", "project_id", "fingerprint_override", "source"} {
				_, present := top[key]
				require.False(t, present, "%s: top-level %q must never be on the wire", name, key)
			}

			var payload validation.ErrorPayload
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			require.NoError(t, dec.Decode(&payload), "%s: strict decode into validation.ErrorPayload", name)

			sdkJSStampAuthenticatedScope(t, name, &payload)

			result := validation.ValidatePayload(&payload)
			require.True(t, result.Valid, "%s: ValidatePayload errors: %s", name, result.ToJSON())
			require.Empty(t, result.Errors, "%s: ValidatePayload must report zero errors", name)

			// MapPayloadToEvent swallows structpb.NewStruct's error and sends a nil metadata in its place — a
			// metadata value the proto cannot carry would vanish silently, so it is checked on its own.
			if payload.Metadata != nil {
				_, err := structpb.NewStruct(payload.Metadata)
				require.NoError(t, err, "%s: metadata must be representable as a proto Struct", name)
			}

			event := mapping.MapPayloadToEvent(&payload)
			require.NoError(t, v.Validate(event), "%s: protovalidate must report zero violations", name)

			// The estimate against Go's proto.Size (spec §7.3, R2): the SDK never sends project_key / project_id
			// — sdkJSStampAuthenticatedScope's fill is the ingestor's, not §4.6's budget to cover — so they are
			// cleared before the comparison.
			clone := proto.Clone(event).(*sentinelv1.ErrorEvent)
			clone.ProjectKey = ""
			clone.ProjectId = ""
			estimate := sdkJSGoldenEstimate(t, strings.TrimSuffix(name, ".json"))
			require.LessOrEqualf(t, proto.Size(clone), estimate, "%s: proto.Size must not exceed the committed protoSizeEstimate", name)

			// What the SDK writes on every event (contract §4.3): platform is the constant "javascript" on every
			// runtime (D3) and metadata.runtime names the runtime.
			require.Equal(t, "javascript", event.GetPlatform(), "%s: platform", name)
			require.Contains(t, []string{"browser", "node", "edge"}, payload.Metadata["runtime"], "%s: metadata.runtime", name)
		})
	}
}

// TestFixtureEstimatesHaveFixtures (spec §7.3): a sidecar whose fixture was deleted is a stale file, and fails
// here rather than silently going unchecked (the golden loop above only ever iterates the fixtures it finds).
func TestFixtureEstimatesHaveFixtures(t *testing.T) {
	sidecars, err := filepath.Glob(filepath.Join(sdkJSGoldenEstimateDir, "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, sidecars)
	for _, sidecar := range sidecars {
		name := strings.TrimSuffix(filepath.Base(sidecar), ".json")
		_, err := os.Stat(filepath.Join(sdkJSFixtureDir, name+".json"))
		require.NoErrorf(t, err, "%s: sidecar has no matching fixture under %s", name, sdkJSFixtureDir)
	}

	tunnelSidecars, err := filepath.Glob(filepath.Join("../../packages/sdk-js/tests/fixture-estimates/tunnel", "*.json"))
	require.NoError(t, err)
	require.NotEmpty(t, tunnelSidecars)
	for _, sidecar := range tunnelSidecars {
		name := strings.TrimSuffix(filepath.Base(sidecar), ".json")
		_, err := os.Stat(filepath.Join(tunnelFixtureDir, name+".json"))
		require.NoErrorf(t, err, "%s: sidecar has no matching tunnel fixture under %s", name, tunnelFixtureDir)
	}
}

// The two rules the fixture set exists to pin on THIS side of the wire, asserted by file so a regenerated set
// cannot lose them silently: the hex radix of trace_flags (contract §4.3 — `10` is 16, not 10) and the shrink's
// ruler (a metadata object the naive count admits and the validator's re-marshal would reject, emitted after the
// shrink and accepted here).
func TestSDKJSFixtures_PinnedValues(t *testing.T) {
	load := func(t *testing.T, file string) validation.ErrorPayload {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(sdkJSFixtureDir, file))
		require.NoError(t, err, "%s must be committed", file)
		var payload validation.ErrorPayload
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		require.NoError(t, dec.Decode(&payload))
		return payload
	}

	t.Run("traceparent flags 01 → trace_flags 1", func(t *testing.T) {
		payload := load(t, "traceparent-flags-01.json")
		require.Equal(t, uint32(1), payload.TraceFlags)
		require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", payload.TraceID)
		require.Equal(t, "00f067aa0ba902b7", payload.SpanID)
	})

	t.Run("traceparent flags 10 → trace_flags 16, the hex radix sdk-go sends as uint32(TraceFlags())", func(t *testing.T) {
		payload := load(t, "traceparent-flags-10.json")
		require.Equal(t, uint32(16), payload.TraceFlags)
	})

	t.Run("the near-the-cap metadata fits the validator's ruler after the shrink removed html", func(t *testing.T) {
		payload := load(t, "metadata-near-the-cap.json")
		marshalled, err := json.Marshal(payload.Metadata)
		require.NoError(t, err)
		require.LessOrEqual(t, len(marshalled), validation.MaxMetadataSize)
		_, hasHTML := payload.Metadata["html"]
		require.False(t, hasHTML, "the shrink must have removed html, the largest key by the Go count")
		require.Equal(t, []interface{}{"html"}, payload.Metadata["truncated_keys"])
		require.Equal(t, "req_1", payload.Metadata["request_id"], "request_id is exempt from the shrink (spec §3.7 — user_id no longer lives in metadata)")
	})

	t.Run("the 20 000-char message was cut on a code-point boundary to under the validator's 10 000 bytes", func(t *testing.T) {
		payload := load(t, "message-cut-code-point.json")
		require.LessOrEqual(t, len(payload.Message), validation.MaxMessageLength)
		require.True(t, json.Valid([]byte(`"`+payload.Message+`"`)), "the cut message must be valid UTF-8")
		require.Equal(t, 9999, len(payload.Message), "3 333 three-byte code points: the longest prefix ≤ 10 000 bytes")
	})

	t.Run("the PII fixture: nested objects scrubbed, the array not descended (packages/sdk-go/pii.go parity)", func(t *testing.T) {
		payload := load(t, "pii-object-with-array.json")
		require.Equal(t, "[FILTERED]", payload.Metadata["password"])
		require.Equal(t, "[FILTERED]", payload.Metadata["author"])
		nested, ok := payload.Metadata["nested"].(map[string]interface{})
		require.True(t, ok)
		require.Equal(t, "[FILTERED]", nested["api_key"])
		items, ok := payload.Metadata["items"].([]interface{})
		require.True(t, ok)
		require.Equal(t, map[string]interface{}{"password": "in-array"}, items[0])
	})

	t.Run("v2-rich-context: every typed field in its typed place, metadata carries none of it (spec §3.7, C4)", func(t *testing.T) {
		payload := load(t, "v2-rich-context.json")
		require.Equal(t, "error", payload.Level)
		require.NotNil(t, payload.SDK)
		require.Equal(t, "sentinel.javascript.node", payload.SDK.Name)
		require.NotNil(t, payload.User)
		require.Equal(t, "usr_1", payload.User.ID)
		require.Equal(t, "ada", payload.User.Username)
		require.Equal(t, "ada@example.com", payload.User.Email)
		require.Equal(t, map[string]string{"tenant_id": "org_a", "feature": "chats"}, payload.Tags)
		require.Equal(t, map[string]interface{}{"a": float64(1)}, payload.Contexts["ok"])
		require.Len(t, payload.Breadcrumbs, 1)
		require.Equal(t, "ui.click", payload.Breadcrumbs[0].Category)
		require.NotContains(t, payload.Metadata, "user_id")
		require.NotContains(t, payload.Metadata, "tenant_id")
		require.NotContains(t, payload.Metadata, "feature")
	})

	t.Run("v2-hostile-before-send: no hook can make the ingestor 400 the event (spec §3.8, C6)", func(t *testing.T) {
		payload := load(t, "v2-hostile-before-send.json")
		require.Empty(t, payload.Logger)
		require.Empty(t, payload.Dist)
		require.Empty(t, payload.Source)
		require.Nil(t, payload.Exception)
		require.Equal(t, "production", payload.Environment, "the pre-hook value stands (an invalid 42 is rejected)")
		require.Empty(t, payload.ReleaseVersion)
		require.Empty(t, payload.Fingerprint)
		require.Empty(t, payload.TraceID)
		require.Equal(t, uint32(0), payload.TraceFlags)
		require.Equal(t, int32(0), payload.Stacktrace[0].Line, "1e12 is over the int32 bound and was removed")
		require.Contains(t, payload.Contexts, "ok")
		require.NotContains(t, payload.Contexts, "when")
		require.NotContains(t, payload.Contexts, "url")
		require.NotContains(t, payload.Contexts, "arr")
		require.Nil(t, payload.Breadcrumbs[0].Data)
		require.Nil(t, payload.Request, "its only member (headers.a, a number) was removed, so the key is omitted")
	})
}

// V1c's second row (sentinel-event-schema-v2 spec §3.5, §9.2): the Go SDK's event is the other old-payload
// producer. tests/fixtures/events/sdk-go-contract-event.json is a COMMITTED JSON copy of the event
// tests/contract/sdk_ingestor_test.go builds with packages/sdk-go — copied, not imported, because CI's go-root
// job runs with GOWORK=off and one unresolvable import disables this whole package (B4). Unlike the JS SDK the
// Go SDK sends project_key (the project's name), so the stamp here only adds the credential's project id.
func TestSDKGoContractFixture_AcceptedByIngestor(t *testing.T) {
	raw, err := os.ReadFile("../fixtures/events/sdk-go-contract-event.json")
	require.NoError(t, err)

	var payload validation.ErrorPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&payload), "strict decode into validation.ErrorPayload")
	require.NotEmpty(t, payload.ProjectKey, "the Go SDK sends project_key as the project name")
	require.Empty(t, payload.ProjectID, "no SDK sends project_id")
	payload.ProjectID = "0f7c1a2e-3b4d-4c5e-8f6a-7b8c9d0e1f2a"

	result := validation.ValidatePayload(&payload)
	require.True(t, result.Valid, "ValidatePayload errors: %s", result.ToJSON())

	v, err := protovalidate.New()
	require.NoError(t, err)
	event := mapping.MapPayloadToEvent(&payload)
	require.NoError(t, v.Validate(event), "protovalidate must report zero violations")
	require.Equal(t, "go", event.GetPlatform())
	require.NotEmpty(t, event.GetStacktrace())
}
