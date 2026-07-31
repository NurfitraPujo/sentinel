//go:build contract

// Package contract is the durable fix for bug pattern B5 (see
// docs/memory/ARCHITECTURE.md / docs/memory/BUGS.md): cross-boundary
// payloads — SDK JSON, the ingestor's decode target, and the proto message —
// have no compiler checking that their field names agree. That is exactly
// how S3 (docs/memory/VERIFIED_STATE.md) and S4/S11 shipped and went
// undetected for a full release.
//
// This test builds a fully-populated event with the REAL
// packages/sdk-go API, marshals it exactly as the SDK's own transport does,
// decodes it with the REAL apps/ingestor-go/validation.ErrorPayload using
// json.Decoder.DisallowUnknownFields() (so a future silent rename fails the
// build instead of silently dropping data), maps it with the REAL
// apps/ingestor-go/mapping.MapPayloadToEvent, and validates the result with
// the REAL protovalidate validator built from the REAL proto. No mapping
// logic, no field list, and no ErrorPayload/Event shape is reimplemented
// here.
//
// Only this file's own package is affected: it MUST start with the
// `//go:build contract` tag above. CI's `go-root` job runs with GOWORK=off
// (see docs/memory/ARCHITECTURE.md A2 / E2E_RECOVERY_PLAN.md C2), under
// which an import of packages/sdk-go from the root module does not resolve
// at all — an untagged file here would turn `go-root` red with a confusing
// "no required module provides package" error, and would also make this
// look like it broke P0-3's go.work. The `contract` CI job runs in
// workspace mode with `-tags=contract` specifically to allow this import;
// see .github/workflows/ci.yml's `contract` job for the mechanism.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"buf.build/go/protovalidate"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

// buildFullyPopulatedEvent constructs a *sentinel.Event using only the real,
// exported packages/sdk-go API — sentinel.NewEvent, exactly as
// Client.CaptureErrorContext (client.go) calls it — never a hand-written
// struct literal. TraceID/SpanID are filled in the same way
// CaptureErrorContext fills them after NewEvent returns (it assigns
// event.TraceID/event.SpanID from sentinel.ExtractTraceIDs(ctx) itself), so
// every exported field on Event ends up populated with a non-zero value.
func buildFullyPopulatedEvent(t *testing.T) *sentinel.Event {
	t.Helper()

	cfg := sentinel.Config{
		// APIKey is the secret (header only); ProjectKey is the project's unique name (body).
		APIKey:         "sent_live_contract_test_0000000000",
		ProjectKey:     "contract-test-project",
		Endpoint:       "http://localhost:8080/ingest",
		Environment:    "production",
		ReleaseVersion: "1.4.2",
	}

	probeErr := errors.New("contract probe: simulated failure for wire-contract verification")
	ctxTags := map[string]interface{}{
		"user_id": "u_00000000-0000-0000-0000-000000000000",
		"plan":    "enterprise",
		"region":  "us-east-1",
	}

	event := sentinel.NewEvent(cfg, probeErr, ctxTags)

	// CaptureErrorContext (client.go) does exactly this after NewEvent:
	//   traceID, spanID := ExtractTraceIDs(ctx)
	//   event.TraceID = traceID
	//   event.SpanID = spanID
	// Reproduced here with fixed values (rather than a real OTel span
	// context) so the event is fully populated without pulling the OTel SDK
	// into this probe just to mint one.
	event.TraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	event.SpanID = "00f067aa0ba902b7"

	if len(event.Stacktrace) == 0 {
		t.Fatalf("buildFullyPopulatedEvent: ExtractStacktrace produced zero frames from within the test process; the test can no longer prove S11 (in_app propagation) with an empty stacktrace")
	}

	return event
}

// requiredWireKeys are exactly the validation.ErrorPayload / ErrorEvent
// fields the proto marks `(buf.validate.field).required = true`
// (packages/proto/sentinel/v1/error_event.proto:37-39,48): project_key,
// platform, environment, error_class. Their absence is a hard 400, not a
// merely-missing optional — this is the set S3/S4 broke.
//
// project_key is client-sent again as of v0.2.0, but as the project's unique NAME (projects.name),
// never as the credential — the secret travels only in the X-API-Key header (Config.APIKey). The
// server VALIDATES the body value against the credential rather than trusting it, which is what
// keeps S6 closed while still letting an organization-wide key select its target project.
var requiredWireKeys = []string{"project_key", "platform", "environment", "error_class"}

// applyAuthenticatedScopeForTest mirrors main.applyAuthenticatedScope: the ingestor overwrites the
// payload's tenancy from the credential before validation. The contract must be checked against the
// payload as the SERVER sees it, not as it arrives off the wire — otherwise this test asserts a
// shape that never reaches protovalidate in production.
const testAuthenticatedProjectKey = "contract-test-project"

func applyAuthenticatedScopeForTest(payload *validation.ErrorPayload) {
	if payload.ProjectKey == "" {
		payload.ProjectKey = testAuthenticatedProjectKey
	}
}

// decodeStrict decodes body into dst with DisallowUnknownFields, which is
// the entire point of this test suite: a future rename on the SDK side of
// any field that a real ErrorPayload column depends on turns a silently
// dropped field (the pre-P2-3 state of S4) into a hard build/test failure.
func decodeStrict(t *testing.T, body []byte, dst interface{}) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		t.Fatalf("strict decode into %T failed: %v\nbody: %s", dst, err, body)
	}
}

// mustValidator builds the real protovalidate.Validator the same way
// apps/ingestor-go/service/service.go's NewIngestService does.
func mustValidator(t *testing.T) protovalidate.Validator {
	t.Helper()
	v, err := protovalidate.New()
	if err != nil {
		t.Fatalf("protovalidate.New: %v", err)
	}
	return v
}

// assertPayloadMatchesEvent is the SDK -> ErrorPayload half of the contract:
// every field the SDK sent must have actually landed on the ErrorPayload,
// unchanged.
func assertPayloadMatchesEvent(t *testing.T, event *sentinel.Event, payload *validation.ErrorPayload) {
	t.Helper()

	// ProjectKey must survive the wire as the project NAME the SDK was configured with.
	if payload.EventID != event.EventID {
		t.Errorf("EventID: got %q, want %q (docs/plans/IDEMPOTENCY_PLAN.md D-a: event_id must round-trip byte-identical from the SDK's UUIDv4 through to ErrorPayload)", payload.EventID, event.EventID)
	}
	if payload.ProjectKey != event.ProjectKey {
		t.Errorf("ProjectKey: got %q, want %q", payload.ProjectKey, event.ProjectKey)
	}
	if payload.Platform != event.Platform {
		t.Errorf("Platform: got %q, want %q", payload.Platform, event.Platform)
	}
	if payload.Environment != event.Environment {
		t.Errorf("Environment: got %q, want %q", payload.Environment, event.Environment)
	}
	if payload.Message != event.Message {
		t.Errorf("Message: got %q, want %q (this is exactly the S4 error_message/message drift - if this fails, someone renamed one side without the other)", payload.Message, event.Message)
	}
	if payload.ErrorClass != event.ErrorClass {
		t.Errorf("ErrorClass: got %q, want %q", payload.ErrorClass, event.ErrorClass)
	}
	if payload.TraceID != event.TraceID {
		t.Errorf("TraceID: got %q, want %q", payload.TraceID, event.TraceID)
	}
	if payload.SpanID != event.SpanID {
		t.Errorf("SpanID: got %q, want %q", payload.SpanID, event.SpanID)
	}
	if payload.ReleaseVersion != event.ReleaseVersion {
		t.Errorf("ReleaseVersion: got %q, want %q (this is exactly S5 - release_version must survive as a first-class field)", payload.ReleaseVersion, event.ReleaseVersion)
	}
	if len(payload.Metadata) == 0 {
		t.Errorf("Metadata: empty after decode, want the SDK's %d-key metadata to have survived (this is exactly S4's context/metadata drift)", len(event.Metadata))
	}
	for k, v := range event.Metadata {
		got, ok := payload.Metadata[k]
		if !ok {
			t.Errorf("Metadata: key %q present on the SDK event but missing after decode", k)
			continue
		}
		gotStr := formatJSONValue(t, got)
		wantStr := formatJSONValue(t, v)
		if gotStr != wantStr {
			t.Errorf("Metadata[%q]: got %s, want %s", k, gotStr, wantStr)
		}
	}
	if len(payload.Stacktrace) != len(event.Stacktrace) {
		t.Fatalf("Stacktrace: got %d frames, want %d", len(payload.Stacktrace), len(event.Stacktrace))
	}
	sawInApp := false
	for i, wantFrame := range event.Stacktrace {
		gotFrame := payload.Stacktrace[i]
		if gotFrame.File != wantFrame.File || gotFrame.Line != int32(wantFrame.Line) || gotFrame.Function != wantFrame.Function || gotFrame.InApp != wantFrame.InApp {
			t.Errorf("Stacktrace[%d]: got %+v, want %+v", i, gotFrame, wantFrame)
		}
		if gotFrame.InApp {
			sawInApp = true
		}
	}
	if !sawInApp {
		t.Errorf("Stacktrace: no frame decoded with in_app==true - this is exactly S11 (fingerprint collapse when every frame is InApp=false)")
	}
}

// formatJSONValue normalizes a decoded interface{} for comparison by
// round-tripping it through JSON, since map[string]interface{} values that
// started as e.g. a Go string decode back as the same Go string but a
// nested map decodes as map[string]interface{} with potentially different
// key iteration - comparing the canonical JSON form sidesteps that.
func formatJSONValue(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("formatJSONValue: %v", err)
	}
	return string(b)
}

// assertSemanticFieldsSurvived is the acceptance check from item 6 of this
// work item: message non-empty, platform=="go", metadata non-empty,
// release_version correct, at least one in_app==true frame - checked on the
// final, mapped, protovalidate-passing proto message, i.e. what actually
// gets published to NATS.
func assertSemanticFieldsSurvived(t *testing.T, event *sentinelv1.ErrorEvent, wantReleaseVersion string, wantEventID string) {
	t.Helper()

	if event.GetEventId() != wantEventID {
		t.Errorf("proto ErrorEvent.event_id = %q, want %q (docs/plans/IDEMPOTENCY_PLAN.md D-a: the client's id must survive to the published proto verbatim when it is usable)", event.GetEventId(), wantEventID)
	}
	if event.GetMessage() == "" {
		t.Error("proto ErrorEvent.message is empty - S4 regression")
	}
	if event.GetPlatform() != "go" {
		t.Errorf("proto ErrorEvent.platform = %q, want \"go\" - S4 regression", event.GetPlatform())
	}
	if event.GetMetadata() == nil || len(event.GetMetadata().GetFields()) == 0 {
		t.Error("proto ErrorEvent.metadata is empty - S4 regression")
	}
	if event.GetReleaseVersion() != wantReleaseVersion {
		t.Errorf("proto ErrorEvent.release_version = %q, want %q - S5 regression", event.GetReleaseVersion(), wantReleaseVersion)
	}
	sawInApp := false
	for _, frame := range event.GetStacktrace() {
		if frame.GetInApp() {
			sawInApp = true
			break
		}
	}
	if !sawInApp {
		t.Error("proto ErrorEvent.stacktrace has no in_app==true frame - S11 regression")
	}
}

// TestSDKToIngestorContract_SingleEvent covers the single-event shape: the
// SDK POSTs a bare object (not an array) to {endpoint} when the batch has
// exactly one event (transport.go sendBatch).
func TestSDKToIngestorContract_SingleEvent(t *testing.T) {
	event := buildFullyPopulatedEvent(t)

	// Marshaled exactly as transport.go's sendBatch does for len(batch)==1:
	// body, err = json.Marshal(batch[0])
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal(event): %v", err)
	}

	var payload validation.ErrorPayload
	decodeStrict(t, body, &payload)

	assertPayloadMatchesEvent(t, event, &payload)

	applyAuthenticatedScopeForTest(&payload)

	protoEvent := mapping.MapPayloadToEvent(&payload)

	if err := mustValidator(t).Validate(protoEvent); err != nil {
		t.Fatalf("protovalidate rejected a real SDK-produced single event (this is exactly S3/S4): %v", err)
	}

	assertSemanticFieldsSurvived(t, protoEvent, event.ReleaseVersion, event.EventID)
}

// TestSDKToIngestorContract_Batch covers the batch shape: an array of
// events POSTed to {endpoint}/batch (transport.go sendBatch, len(batch) > 1),
// decoded by handleBatchIngest into []validation.ErrorPayload.
func TestSDKToIngestorContract_Batch(t *testing.T) {
	eventA := buildFullyPopulatedEvent(t)
	eventB := buildFullyPopulatedEvent(t)
	eventB.ErrorClass = "*net.OpError"
	eventB.Message = "contract probe: second batch item, distinct error class"

	batch := []*sentinel.Event{eventA, eventB}

	// Marshaled exactly as transport.go's sendBatch does for len(batch) > 1:
	// body, err = json.Marshal(batch)
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatalf("json.Marshal(batch): %v", err)
	}

	// Decoded exactly as apps/ingestor-go/main.go's handleBatchIngest does:
	// var payloads []validation.ErrorPayload
	var payloads []validation.ErrorPayload
	decodeStrict(t, body, &payloads)

	if len(payloads) != len(batch) {
		t.Fatalf("decoded %d payloads, want %d", len(payloads), len(batch))
	}

	validator := mustValidator(t)
	for i, wantEvent := range batch {
		payload := &payloads[i]
		assertPayloadMatchesEvent(t, wantEvent, payload)

		// handleBatchIngest applies the authenticated scope per item, so mirror that here.
		applyAuthenticatedScopeForTest(payload)

		protoEvent := mapping.MapPayloadToEvent(payload)
		if err := validator.Validate(protoEvent); err != nil {
			t.Fatalf("protovalidate rejected batch item %d (a real SDK-produced event): %v", i, err)
		}
		assertSemanticFieldsSurvived(t, protoEvent, wantEvent.ReleaseVersion, wantEvent.EventID)
	}

	// The two items must fingerprint distinctly downstream: same platform
	// and environment, deliberately different error_class/message. Not this
	// test's job to run the fingerprinter, just to confirm the inputs it
	// needs (error_class, in-app stacktrace) actually differ and both
	// survived the boundary distinctly.
	if payloads[0].ErrorClass == payloads[1].ErrorClass {
		t.Fatalf("test setup bug: both batch items decoded with the same ErrorClass %q", payloads[0].ErrorClass)
	}
}

// TestSDK_ProducesAllErrorPayloadRequiredFields is the mirror direction
// (item 7): assert every ErrorPayload field the proto marks
// required=true (see requiredWireKeys) is actually present, and non-empty,
// in the SDK's own JSON wire output - not merely present after decoding
// into ErrorPayload, but present in the bytes the SDK itself produced. If
// the SDK ever stops emitting one of these keys, ingest starts rejecting
// every event again exactly as S3/S4 did, and this test is what catches it
// before the field-rename tests above even get a chance to run.
func TestSDK_ProducesAllErrorPayloadRequiredFields(t *testing.T) {
	event := buildFullyPopulatedEvent(t)

	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal(event): %v", err)
	}

	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatalf("unmarshal SDK event JSON: %v", err)
	}

	for _, key := range requiredWireKeys {
		raw, ok := generic[key]
		if !ok {
			t.Errorf("SDK event JSON is missing required wire field %q (proto: (buf.validate.field).required = true) - a real event with this key missing gets a hard 400", key)
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Errorf("field %q: not a JSON string: %v", key, err)
			continue
		}
		if s == "" {
			t.Errorf("SDK produced an empty value for required field %q", key)
		}
	}
}

// TestSDK_RejectsWithoutPlatform_WouldFailValidation is a negative control:
// it proves the protovalidate step in the tests above is actually capable of
// failing (i.e. is not a rubber stamp) by constructing an ErrorPayload with
// platform deliberately blanked out post-decode and confirming validation
// rejects it for exactly that reason. This does not touch the SDK or any
// out-of-scope file; it only exercises the real validator on a payload this
// test file controls entirely.
func TestSDK_RejectsWithoutPlatform_WouldFailValidation(t *testing.T) {
	event := buildFullyPopulatedEvent(t)
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("json.Marshal(event): %v", err)
	}
	var payload validation.ErrorPayload
	decodeStrict(t, body, &payload)

	payload.Platform = "" // simulate the pre-P2-3 SDK, which never sent platform at all

	protoEvent := mapping.MapPayloadToEvent(&payload)
	err = mustValidator(t).Validate(protoEvent)
	if err == nil {
		t.Fatal("expected protovalidate to reject an event with an empty platform, got nil error - the validator step in the tests above would not have caught a real S4 regression")
	}
}
