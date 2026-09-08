package unit

import (
	"strings"
	"testing"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

// The THIRD of sentinel-project-environments §4.8's three enforcement points, and the reason §11
// lists it as a boundary of its own: the Go test over validation.ValidatePayload passes with the CEL
// rule unchanged. `apps/ingestor-go/service/service.go` validates every event with protovalidate
// before publishing it, so a `.proto` edit that was never followed by `buf generate` — the
// descriptor compiled into `gen/` is what protovalidate actually reads — would leave the wire path
// still rejecting `eu-west-1` while the Go path accepted it, and nothing else in this repo would
// notice.
//
// This exercises the compiled descriptor, not the .proto text, which is exactly the property that
// matters: it fails if the regenerated files were not committed.
func validErrorEvent() *sentinelv1.ErrorEvent {
	return &sentinelv1.ErrorEvent{
		ProjectKey:  "proj-abc123",
		Platform:    "go",
		Environment: "production",
		Message:     "boom",
		ErrorClass:  "TestError",
	}
}

func TestErrorEventCEL_EnvironmentWidenedSet(t *testing.T) {
	v, err := protovalidate.New()
	require.NoError(t, err)

	t.Run("accepted", func(t *testing.T) {
		for _, env := range []string{"pre-prod", "eu-west-1", "staging-2", "prod_eu", "production"} {
			t.Run(env, func(t *testing.T) {
				event := validErrorEvent()
				event.Environment = env
				assert.NoError(t, v.Validate(event), "decision 10's widened set must accept %q on the wire too", env)
			})
		}
	})

	t.Run("rejected", func(t *testing.T) {
		for name, env := range map[string]string{
			"uppercase":      "Production",
			"leading hyphen": "-prod",
			"51 characters":  strings.Repeat("a", 51),
			"slash":          "eu/west",
			"empty":          "",
		} {
			t.Run(name, func(t *testing.T) {
				event := validErrorEvent()
				event.Environment = env
				assert.Error(t, v.Validate(event), "expected the error_event.environment CEL rule to reject %q", env)
			})
		}
	})

	// platform is NOT widened (§4.8): its own CEL rule is unchanged, and this is the wire-side half
	// of the assertion tests/unit/ingestor_validation_test.go makes about isValidAlphanumeric.
	t.Run("platform unchanged", func(t *testing.T) {
		for _, platform := range []string{"pre-prod", "eu-west-1", "staging-2", "prod_eu"} {
			t.Run(platform, func(t *testing.T) {
				event := validErrorEvent()
				event.Platform = platform
				assert.Error(t, v.Validate(event), "error_event.platform must still reject %q", platform)
			})
		}
	})
}
