package validation

import (
	"strings"
	"testing"
)

func validPayload() *ErrorPayload {
	return &ErrorPayload{
		ProjectKey:  "my-project",
		Platform:    "go",
		Environment: "production",
		ErrorClass:  "runtime.Error",
	}
}

// The proto has carried fingerprint and fingerprint_override since the start and
// the processor honours them, but nothing decoded them from JSON — so custom
// grouping was unreachable over the only ingest path that exists.
func TestFingerprintIsAccepted(t *testing.T) {
	p := validPayload()
	p.Fingerprint = "checkout-timeout"

	if res := ValidatePayload(p); !res.Valid {
		t.Fatalf("expected a custom fingerprint to be accepted, got %+v", res.Errors)
	}
}

// Exactly at the column width must pass: an off-by-one here would reject
// legitimate keys.
func TestFingerprintAtLimitIsAccepted(t *testing.T) {
	p := validPayload()
	p.Fingerprint = strings.Repeat("a", MaxFingerprintLength)

	if res := ValidatePayload(p); !res.Valid {
		t.Fatalf("expected %d characters to be accepted, got %+v", MaxFingerprintLength, res.Errors)
	}
}

// One over must fail. issues.fingerprint is VARCHAR(64) and the value is stored
// unhashed, so accepting this would blow up at the INSERT — after the event had
// been accepted and published, which is far worse than a 400 here.
func TestFingerprintOverLimitIsRejected(t *testing.T) {
	p := validPayload()
	p.Fingerprint = strings.Repeat("a", MaxFingerprintLength+1)

	res := ValidatePayload(p)
	if res.Valid {
		t.Fatal("expected an oversized fingerprint to be rejected")
	}
	found := false
	for _, e := range res.Errors {
		if e.Field == "fingerprint" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an error naming the fingerprint field, got %+v", res.Errors)
	}
}

// The overwhelmingly common case: no fingerprint at all, server computes one.
func TestEmptyFingerprintIsAccepted(t *testing.T) {
	if res := ValidatePayload(validPayload()); !res.Valid {
		t.Fatalf("expected an empty fingerprint to be fine, got %+v", res.Errors)
	}
}
