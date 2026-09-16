package unit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
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

			// What the SDK writes on every event (contract §4.3): platform is the constant "javascript" on every
			// runtime (D3) and metadata.runtime names the runtime.
			require.Equal(t, "javascript", event.GetPlatform(), "%s: platform", name)
			require.Contains(t, []string{"browser", "node", "edge"}, payload.Metadata["runtime"], "%s: metadata.runtime", name)
		})
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
		require.Equal(t, "usr_1", payload.Metadata["user_id"], "user_id is exempt from the shrink")
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
}
