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

// deserializeAndFingerprint is the pipeline's order since sentinel-sourcemaps §4.4 (be.md task
// 4): Deserialize, then — where processEventInternal has resolved the project and run the
// symbolication stage — ComputeFingerprint. These three tests pinned the semantics when the
// step lived at the end of Deserialize; they moved here with it, none deleted, and assert the
// same three things at the new call.
func deserializeAndFingerprint(t *testing.T, e *sentinelv1.ErrorEvent) *ErrorEvent {
	t.Helper()
	got, err := Deserialize(marshal(t, e))
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	got.ComputeFingerprint(nil)
	return got
}

// The end of the path this change opens: a client fingerprint must survive to
// issues.fingerprint unchanged. If it did not, "custom grouping" would mean
// nothing even with the ingestor now decoding the field.
func TestClientFingerprintIsUsedVerbatim(t *testing.T) {
	e := baseEvent()
	e.Fingerprint = "checkout-timeout"

	got := deserializeAndFingerprint(t, e)
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

	got := deserializeAndFingerprint(t, e)
	if got.Fingerprint == "checkout-timeout" {
		t.Fatal("fingerprint_override must force a recompute, not preserve the client value")
	}
	if got.Fingerprint == "" {
		t.Fatal("expected a computed fingerprint")
	}
}

// No fingerprint at all is the normal case: the server computes one.
func TestAbsentFingerprintIsComputed(t *testing.T) {
	got := deserializeAndFingerprint(t, baseEvent())
	if got.Fingerprint == "" {
		t.Fatal("expected a computed fingerprint when none was supplied")
	}
}

// The split itself (sentinel-sourcemaps §4.4): Deserialize no longer computes anything — it hands
// the client's value and the override flag through, and identity is ComputeFingerprint's alone.
// If Deserialize ever grows the computation back, the stage would run on frames whose identity
// was already fixed, and the symbolicated occurrence would land on the raw-frame issue.
func TestDeserializeLeavesTheFingerprintToComputeFingerprint(t *testing.T) {
	got, err := Deserialize(marshal(t, baseEvent()))
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if got.Fingerprint != "" {
		t.Fatalf("Deserialize must not compute a fingerprint any more; got %q", got.Fingerprint)
	}

	e := baseEvent()
	e.Fingerprint = "checkout-timeout"
	e.FingerprintOverride = true
	got, err = Deserialize(marshal(t, e))
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if got.Fingerprint != "checkout-timeout" || !got.FingerprintOverride {
		t.Fatalf("Deserialize must carry the client value and the override flag to the step: got %q, override=%v", got.Fingerprint, got.FingerprintOverride)
	}
}
