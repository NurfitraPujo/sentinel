package event

import (
	"testing"

	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func marshal(t *testing.T, e *sentinelv1.ErrorEvent) []byte {
	t.Helper()
	b, err := proto.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func baseEvent() *sentinelv1.ErrorEvent {
	return &sentinelv1.ErrorEvent{
		ProjectKey:  "my-project",
		Platform:    "go",
		Environment: "production",
		ErrorClass:  "runtime.Error",
		Message:     "boom",
		Timestamp:   timestamppb.Now(),
	}
}

// The end of the path this change opens: a client fingerprint must survive to
// issues.fingerprint unchanged. If it did not, "custom grouping" would mean
// nothing even with the ingestor now decoding the field.
func TestClientFingerprintIsUsedVerbatim(t *testing.T) {
	e := baseEvent()
	e.Fingerprint = "checkout-timeout"

	got, err := Deserialize(marshal(t, e))
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if got.Fingerprint != "checkout-timeout" {
		t.Fatalf("expected the client fingerprint verbatim, got %q", got.Fingerprint)
	}
}

// The trap in the name, pinned: fingerprint_override does NOT mean "my
// fingerprint wins". It forces a server-side recompute and DISCARDS the value
// sent with it. The SDK deliberately does not expose it for this reason, and
// this test is what stops the semantics drifting silently.
func TestFingerprintOverrideDiscardsTheClientValue(t *testing.T) {
	e := baseEvent()
	e.Fingerprint = "checkout-timeout"
	e.FingerprintOverride = true

	got, err := Deserialize(marshal(t, e))
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if got.Fingerprint == "checkout-timeout" {
		t.Fatal("fingerprint_override must force a recompute, not preserve the client value")
	}
	if got.Fingerprint == "" {
		t.Fatal("expected a computed fingerprint")
	}
}

// No fingerprint at all is the normal case: the server computes one.
func TestAbsentFingerprintIsComputed(t *testing.T) {
	got, err := Deserialize(marshal(t, baseEvent()))
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if got.Fingerprint == "" {
		t.Fatal("expected a computed fingerprint when none was supplied")
	}
}
