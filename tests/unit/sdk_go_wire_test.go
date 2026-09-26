package unit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// protoMarshalSize is the real protobuf size of event, exactly as
// service.IngestAt marshals it before the NATS publish (T39).
func protoMarshalSize(event *sentinelv1.ErrorEvent) (int, error) {
	b, err := proto.Marshal(event)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// sdkGoWireGoldenDir holds packages/sdk-go/testdata/wire_v2_*.golden.json,
// committed by packages/sdk-go's own wire_golden_test.go (D6, spec §10.2
// P1.7-P1.8's golden bridge): an sdk-go test (GOWORK=off) proves the SDK
// emits exactly these bytes, and this test (root module, GOWORK=off)
// decodes them strictly into the real validation.ErrorPayload and runs the
// real bounds step, mapper and protovalidate - the only mechanical proof
// that what the transport bounded and measured is what the ingestor accepts
// unchanged (T18: tests/contract cannot run under GOWORK=off).
const sdkGoWireGoldenDir = "../../packages/sdk-go/testdata"

// sdkGoStampAuthenticatedScope mirrors main.applyAuthenticatedScope for a
// project-scoped key: sdk-go sends project_key as the project's own name
// (spec §3.4's closing table), which the server never trusts as-is; only
// project_id is stamped from the authenticated credential, as
// TestSDKGoContractFixture_AcceptedByIngestor already does for the older
// committed sdk-go fixture.
func sdkGoStampAuthenticatedScope(payload *validation.ErrorPayload) {
	payload.ProjectID = "0f7c1a2e-3b4d-4c5e-8f6a-7b8c9d0e1f2a"
}

// TestSDKGoBoundedWireNeedsNoServerCut is P1.8's root half: every committed
// sdk-go golden, run through the real ingestor bounds step, must come out
// byte-identical (no server_truncated / server_truncated_keys marker
// written, no context or metadata key removed) - the transport's own bounds
// left the server nothing to cut (spec §3.4, §3.5 step 3, "the margin").
func TestSDKGoBoundedWireNeedsNoServerCut(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join(sdkGoWireGoldenDir, "wire_v2_*.golden.json"))
	require.NoError(t, err)
	require.NotEmpty(t, paths, "no sdk-go wire goldens found under %s", sdkGoWireGoldenDir)

	v, err := protovalidate.New()
	require.NoError(t, err)

	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			require.NoError(t, err)

			var payload validation.ErrorPayload
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			require.NoError(t, dec.Decode(&payload), "%s: strict decode into validation.ErrorPayload", name)

			require.NotEmpty(t, payload.ProjectKey, "%s: sdk-go always sends project_key as the project's name", name)
			sdkGoStampAuthenticatedScope(&payload)

			before, err := json.Marshal(payload)
			require.NoError(t, err)

			result := validation.ValidatePayload(&payload)
			require.True(t, result.Valid, "%s: ValidatePayload errors: %s", name, result.ToJSON())
			require.Empty(t, result.Errors, "%s: ValidatePayload must report zero errors", name)

			_, hasTruncated := payload.Metadata["server_truncated"]
			_, hasTruncatedKeys := payload.Metadata["server_truncated_keys"]
			require.False(t, hasTruncated, "%s: the ingestor's own bounds step must cut nothing (server_truncated present)", name)
			require.False(t, hasTruncatedKeys, "%s: the ingestor's own bounds step must cut nothing (server_truncated_keys present)", name)

			after, err := json.Marshal(payload)
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after),
				"%s: ValidatePayload must change nothing in an SDK-bounded event", name)

			event := mapping.MapPayloadToEvent(&payload)
			require.NoError(t, v.Validate(event), "%s: protovalidate must report zero violations", name)
		})
	}
}

// TestSDKGoWireGoldenMatchesIngestor is P1.7's root half: wire_v2_full,
// decoded strictly and mapped through the real ingestor, must carry exactly
// the fields the SDK sets non-empty on the mapped proto, and must carry none
// of the fields the SDK never sets (spec §10.2 P1.7).
func TestSDKGoWireGoldenMatchesIngestor(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(sdkGoWireGoldenDir, "wire_v2_full.golden.json"))
	require.NoError(t, err)

	var payload validation.ErrorPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&payload), "strict decode into validation.ErrorPayload")
	sdkGoStampAuthenticatedScope(&payload)

	result := validation.ValidatePayload(&payload)
	require.True(t, result.Valid, "ValidatePayload errors: %s", result.ToJSON())

	event := mapping.MapPayloadToEvent(&payload)
	v, err := protovalidate.New()
	require.NoError(t, err)
	require.NoError(t, v.Validate(event), "protovalidate must accept the full golden")

	require.NotEmpty(t, event.GetLevel())
	require.NotNil(t, event.GetSdk())
	require.NotEmpty(t, event.GetSdk().GetName())
	require.NotEmpty(t, event.GetSdk().GetVersion())
	require.NotEmpty(t, event.GetServerName())
	require.NotEmpty(t, event.GetTransaction())
	require.NotNil(t, event.GetUser())
	require.NotEmpty(t, event.GetUser().GetId())
	require.NotEmpty(t, event.GetUser().GetUsername())
	require.NotEmpty(t, event.GetUser().GetEmail())
	require.NotEmpty(t, event.GetUser().GetIpAddress())
	require.NotEmpty(t, event.GetTags())
	require.Contains(t, event.GetTags(), "tenant_id")
	require.NotEmpty(t, event.GetContexts())
	require.Contains(t, event.GetContexts(), "runtime")
	require.Contains(t, event.GetContexts(), "os")
	require.Contains(t, event.GetContexts(), "device")
	require.NotNil(t, event.GetRequest())
	require.NotEmpty(t, event.GetRequest().GetMethod())
	require.NotEmpty(t, event.GetRequest().GetUrl())
	require.NotEmpty(t, event.GetRequest().GetQueryString())
	require.NotEmpty(t, event.GetRequest().GetHeaders())
	require.NotEmpty(t, event.GetBreadcrumbs())

	require.Empty(t, event.GetException(), "the SDK never sets exception")
	require.Empty(t, event.GetLogger(), "the SDK never sets logger")
	require.Empty(t, event.GetDist(), "the SDK never sets dist")
	require.Empty(t, event.GetSource(), "the SDK never sets source")
	require.Nil(t, event.GetUser().GetGeo(), "the SDK never sets user.geo")
	require.Nil(t, event.GetRequest().GetData(), "the SDK never sets request.data")
	require.Empty(t, event.GetRequest().GetEnv(), "the SDK never sets request.env")
}

// TestSDKGoPublishCostBoundsProto is P1.8's zero-tolerance proof: for every
// entry of wire_v2_publish_cost.json, the publish cost packages/sdk-go
// computed must be >= the real protobuf size proto.Marshal produces for
// exactly what the SDK sent (spec §3.5, "The publish cost"; T39, T40).
func TestSDKGoPublishCostBoundsProto(t *testing.T) {
	costsRaw, err := os.ReadFile(filepath.Join(sdkGoWireGoldenDir, "wire_v2_publish_cost.json"))
	require.NoError(t, err)
	var costs map[string]struct {
		ArraySize   int `json:"array_size"`
		PublishCost int `json:"publish_cost"`
	}
	require.NoError(t, json.Unmarshal(costsRaw, &costs))
	require.NotEmpty(t, costs)

	v, err := protovalidate.New()
	require.NoError(t, err)

	for goldenName, want := range costs {
		t.Run(goldenName, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(sdkGoWireGoldenDir, goldenName))
			require.NoError(t, err)

			var payload validation.ErrorPayload
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			require.NoError(t, dec.Decode(&payload))
			sdkGoStampAuthenticatedScope(&payload)

			result := validation.ValidatePayload(&payload)
			require.True(t, result.Valid, "ValidatePayload errors: %s", result.ToJSON())

			event := mapping.MapPayloadToEvent(&payload)
			require.NoError(t, v.Validate(event))

			n, err := protoMarshalSize(event)
			require.NoError(t, err)

			require.LessOrEqualf(t, n, want.PublishCost,
				"%s: real protobuf size %d must be <= the SDK's publish cost %d (zero tolerance, spec §3.5)",
				goldenName, n, want.PublishCost)
			require.LessOrEqualf(t, want.PublishCost-n, 16384,
				"%s: publish cost %d must not exceed protobuf size %d by more than 16384 (an over-count that double-counts a Struct)",
				goldenName, want.PublishCost, n)

			arraySize := len(raw) + 2 // the enclosing brackets of a one-element array
			require.Equal(t, want.ArraySize, arraySize,
				"%s: recorded array_size must match the golden's own one-element-array size", goldenName)
		})
	}
}
