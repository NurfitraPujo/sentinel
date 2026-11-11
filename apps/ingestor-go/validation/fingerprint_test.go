// External test package: these rows go through service.IngestAt, and service imports validation, so an
// internal (package validation) test importing service would be an import cycle.
package validation_test

import (
	"context"
	"strings"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/service"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

type discardPublisher struct{ n int }

func (d *discardPublisher) PublishWithHeaders(context.Context, []byte, nats.Header) error {
	d.n++
	return nil
}

func validPayload() *validation.ErrorPayload {
	return &validation.ErrorPayload{
		ProjectKey:  "my-project",
		Platform:    "go",
		Environment: "production",
		ErrorClass:  "runtime.Error",
	}
}

// ingest runs p through the live path. sentinel-event-schema-v2 spec §4.3: the fingerprint bound is
// enforced by protovalidate's error_event.fingerprint rule alone (in CODE POINTS), no longer duplicated
// in ValidatePayload (where it counted bytes), so these rows assert it where it is enforced.
func ingest(t *testing.T, p *validation.ErrorPayload) (error, int) {
	t.Helper()
	pub := &discardPublisher{}
	svc, err := service.NewIngestService(pub)
	if err != nil {
		t.Fatal(err)
	}
	err = svc.Ingest(context.Background(), p)
	return err, pub.n
}

// The proto has carried fingerprint and fingerprint_override since the start and
// the processor honours them, but nothing decoded them from JSON — so custom
// grouping was unreachable over the only ingest path that exists.
func TestFingerprintIsAccepted(t *testing.T) {
	p := validPayload()
	p.Fingerprint = "checkout-timeout"

	if err, n := ingest(t, p); err != nil || n != 1 {
		t.Fatalf("expected a custom fingerprint to be accepted, got err=%v published=%d", err, n)
	}
}

// Exactly at the column width must pass: an off-by-one here would reject
// legitimate keys.
func TestFingerprintAtLimitIsAccepted(t *testing.T) {
	p := validPayload()
	p.Fingerprint = strings.Repeat("a", validation.MaxFingerprintLength)

	if err, _ := ingest(t, p); err != nil {
		t.Fatalf("expected %d characters to be accepted, got %v", validation.MaxFingerprintLength, err)
	}
}

// The bound is in code points, as issues.fingerprint VARCHAR(64) counts: 64 multi-byte characters
// (192 bytes) fit the column and must be accepted.
func TestFingerprintAtLimitInCodePointsIsAccepted(t *testing.T) {
	p := validPayload()
	p.Fingerprint = strings.Repeat("界", validation.MaxFingerprintLength)

	if err, _ := ingest(t, p); err != nil {
		t.Fatalf("expected %d code points to be accepted, got %v", validation.MaxFingerprintLength, err)
	}
}

// One over must fail. issues.fingerprint is VARCHAR(64) and the value is stored
// unhashed, so accepting this would blow up at the INSERT — after the event had
// been accepted and published, which is far worse than a 400 here.
func TestFingerprintOverLimitIsRejected(t *testing.T) {
	p := validPayload()
	p.Fingerprint = strings.Repeat("a", validation.MaxFingerprintLength+1)

	err, n := ingest(t, p)
	if err == nil || !strings.HasPrefix(err.Error(), "validation failed") {
		t.Fatalf("expected a validation failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "fingerprint must not exceed 64 characters") {
		t.Fatalf("expected the error_event.fingerprint violation, got %v", err)
	}
	if n != 0 {
		t.Fatal("a rejected event must not be published")
	}
}

// The overwhelmingly common case: no fingerprint at all, server computes one.
func TestEmptyFingerprintIsAccepted(t *testing.T) {
	if err, _ := ingest(t, validPayload()); err != nil {
		t.Fatalf("expected an empty fingerprint to be fine, got %v", err)
	}
}
