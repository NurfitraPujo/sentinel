package mapping

import (
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
)

// The mapper is the hop that was missing: the payload type now decodes these two
// fields, and they have to survive into the proto the processor reads. A field
// added to ErrorPayload but not mapped here is silently dropped.
func TestMapPayloadCarriesFingerprint(t *testing.T) {
	got := MapPayloadToEvent(&validation.ErrorPayload{
		ProjectKey:  "my-project",
		Platform:    "go",
		Environment: "production",
		ErrorClass:  "runtime.Error",
		Timestamp:   time.Now(),
		Fingerprint: "checkout-timeout",
	})

	if got.Fingerprint != "checkout-timeout" {
		t.Fatalf("fingerprint not carried into the proto: %q", got.Fingerprint)
	}
	if got.FingerprintOverride {
		t.Fatal("fingerprint_override must default to false, or the processor discards the value just set")
	}
}

func TestMapPayloadCarriesFingerprintOverride(t *testing.T) {
	got := MapPayloadToEvent(&validation.ErrorPayload{
		ProjectKey:          "my-project",
		Platform:            "go",
		Environment:         "production",
		ErrorClass:          "runtime.Error",
		Timestamp:           time.Now(),
		Fingerprint:         "ignored-because-override-is-set",
		FingerprintOverride: true,
	})

	if !got.FingerprintOverride {
		t.Fatal("fingerprint_override not carried into the proto")
	}
}
