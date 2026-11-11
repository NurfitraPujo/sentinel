package unit

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"buf.build/go/protovalidate"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/mapping"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/validation"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"
)

// sentinel-event-schema-v2 TASK-02 (spec §4.1, §9.2 V2a/V2c): every v2 field the ingestor decodes reaches the
// wire, and a JSON fingerprint reaches ComputeFingerprint.

const eventV2FullFixture = "../fixtures/events/event-v2-full.json"

// loadEventV2Full strict-decodes the shared fixture (DisallowUnknownFields, so the fixture cannot carry a key
// ErrorPayload lacks and silently prove nothing) and stamps the credential's tenancy as
// main.applyAuthenticatedScope does.
func loadEventV2Full(t *testing.T) *validation.ErrorPayload {
	t.Helper()
	raw, err := os.ReadFile(eventV2FullFixture)
	require.NoError(t, err)
	var payload validation.ErrorPayload
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&payload), "strict decode of %s", eventV2FullFixture)
	payload.ProjectKey = "proj-event-v2"
	payload.ProjectID = "0f7c1a2e-3b4d-4c5e-8f6a-7b8c9d0e1f2a"
	return &payload
}

// eventV2WireBytes is the V2a chain up to the NATS message: ValidatePayload -> MapPayloadToEvent ->
// protovalidate -> proto.Marshal.
func eventV2WireBytes(t *testing.T, payload *validation.ErrorPayload) []byte {
	t.Helper()
	result := validation.ValidatePayload(payload)
	require.True(t, result.Valid, "ValidatePayload: %s", result.ToJSON())
	msg := mapping.MapPayloadToEvent(payload)
	v, err := protovalidate.New()
	require.NoError(t, err)
	require.NoError(t, v.Validate(msg), "protovalidate")
	data, err := proto.Marshal(msg)
	require.NoError(t, err)
	return data
}

func TestEventV2_AllFieldsSurviveDecodeToDeserialize(t *testing.T) {
	payload := loadEventV2Full(t)
	receivedAt := time.Date(2026, 9, 23, 12, 0, 1, 0, time.UTC)
	payload.ReceivedAt = receivedAt // IngestAt's stamp; TestEventV2_ReceivedAtIsServerStamped proves IngestAt does it
	data := eventV2WireBytes(t, payload)

	t.Run("the NATS message carries every field", func(t *testing.T) {
		var wire sentinelv1.ErrorEvent
		require.NoError(t, proto.Unmarshal(data, &wire))

		require.Equal(t, "onerror", wire.GetSource())
		require.Equal(t, "warning", wire.GetLevel())
		require.Equal(t, "checkout", wire.GetLogger())
		require.Equal(t, "web-7f9c-pod-2", wire.GetServerName())
		require.Equal(t, "/checkout/pay", wire.GetTransaction())
		require.Equal(t, "4127", wire.GetDist())
		require.Equal(t, "sentinel.javascript.browser", wire.GetSdk().GetName())
		require.Equal(t, "1.8.0", wire.GetSdk().GetVersion())

		u := wire.GetUser()
		require.Equal(t, "user-typed-42", u.GetId())
		require.Equal(t, "alice", u.GetUsername())
		require.Equal(t, "alice@example.com", u.GetEmail())
		require.Equal(t, "2001:db8::1", u.GetIpAddress())
		require.Equal(t, "Berlin", u.GetGeo().GetCity())
		require.Equal(t, "Berlin", u.GetGeo().GetRegion())
		require.Equal(t, "DE", u.GetGeo().GetCountryCode())
		require.Equal(t, "BE", u.GetGeo().GetSubdivision())

		require.Equal(t, map[string]string{"tenant_id": "tenant-acme", "region": "eu-central-1", "feature.flag": "new-checkout"}, wire.GetTags())

		require.Len(t, wire.GetContexts(), 6)
		device := wire.GetContexts()["device"].AsMap()
		require.Equal(t, "Pixel 8", device["model"])
		require.Equal(t, 8589934592.0, device["memory_size"])
		require.Equal(t, 57.5, device["battery_level"])
		require.Equal(t, true, device["online"])
		require.Equal(t, "Europe/Berlin", wire.GetContexts()["culture"].AsMap()["timezone"])

		req := wire.GetRequest()
		require.Equal(t, "POST", req.GetMethod())
		require.Equal(t, "https://shop.example.com/checkout/pay?step=2", req.GetUrl())
		require.Equal(t, "step=2", req.GetQueryString())
		require.Equal(t, "Bearer eyJhbGciOiJIUzI1NiJ9.payload.sig", req.GetHeaders()["Authorization"])
		require.Equal(t, "sid=abc123; theme=dark", req.GetHeaders()["Cookie"])
		require.Equal(t, "sk_live_51H8xyz", req.GetHeaders()["X-API-Key"])
		require.Len(t, req.GetHeaders(), 4)
		require.Equal(t, map[string]interface{}{"amount": 4200.0, "currency": "EUR", "password": "hunter2hunter2"}, req.GetData().AsInterface())
		require.Equal(t, map[string]string{"REMOTE_ADDR": "10.1.2.3"}, req.GetEnv())

		crumbs := wire.GetBreadcrumbs()
		require.Len(t, crumbs, 2)
		require.Equal(t, time.Date(2026, 9, 23, 11, 59, 58, 0, time.UTC), crumbs[1].GetTimestamp().AsTime())
		require.Equal(t, "http", crumbs[1].GetType())
		require.Equal(t, "xhr", crumbs[1].GetCategory())
		require.Equal(t, "error", crumbs[1].GetLevel())
		require.Equal(t, "POST /api/pay 402", crumbs[1].GetMessage())
		require.Equal(t, map[string]interface{}{"password": "p", "status_code": 402.0}, crumbs[1].GetData().AsMap())

		values := wire.GetException().GetValues()
		require.Len(t, values, 2)
		require.Equal(t, "PaymentDeclined", values[0].GetType(), "Sentry's order: the root cause first")
		require.Equal(t, "TypeError", values[1].GetType(), "the raised exception last")
		require.Equal(t, "Cannot read properties of undefined (reading 'id')", values[1].GetValue())
		require.Equal(t, "onerror", values[1].GetMechanism().GetType())
		require.NotNil(t, values[1].GetMechanism().Handled, "handled is tri-state: false must be present")
		require.False(t, values[1].GetMechanism().GetHandled())
		require.True(t, values[0].GetMechanism().GetHandled())
		chainFrame := values[1].GetStacktrace()[0]
		require.Equal(t, "pay.checkout", chainFrame.GetModule())
		require.Equal(t, "https://shop.example.com/assets/checkout.js", chainFrame.GetAbsPath())
		require.Equal(t, "const id = order.customer.id;", chainFrame.GetContextLine())

		frame := wire.GetStacktrace()[0]
		require.Equal(t, "pay.checkout", frame.GetModule())
		require.Equal(t, "https://shop.example.com/assets/checkout.js", frame.GetAbsPath())
		require.Equal(t, "const id = order.customer.id;", frame.GetContextLine())
		require.Equal(t, []string{"function pay(order) {", "  // resolve the customer"}, frame.GetPreContext())
		require.Equal(t, []string{"  return charge(id);", "}"}, frame.GetPostContext())

		require.Equal(t, receivedAt, wire.GetReceivedAt().AsTime())
	})

	// The processor half (spec §6.1): Deserialize copies every v2 field off the proto, source and
	// received_at included, and the stored shapes are §5.2's. Masked values are asserted as masked.
	t.Run("event.Deserialize carries every field", func(t *testing.T) {
		evt, err := event.Deserialize(data)
		require.NoError(t, err)

		require.Equal(t, "onerror", evt.Source)
		require.Equal(t, "warning", evt.Level)
		require.Equal(t, "checkout", evt.Logger)
		require.Equal(t, "web-7f9c-pod-2", evt.ServerName)
		require.Equal(t, "/checkout/pay", evt.Transaction)
		require.Equal(t, "4127", evt.Dist)
		require.Equal(t, uint32(1), evt.TraceFlags)
		require.Equal(t, receivedAt, evt.ReceivedAt)
		require.Equal(t, &event.SDK{Name: "sentinel.javascript.browser", Version: "1.8.0"}, evt.SDK)
		require.Equal(t, &event.User{
			ID: "user-typed-42", Username: "alice", Email: "alice@example.com", IPAddress: "2001:db8::1",
			Geo: &event.Geo{City: "Berlin", Region: "Berlin", CountryCode: "DE", Subdivision: "BE"},
		}, evt.User, "user is stored as sent (decision 1)")
		require.Equal(t, map[string]string{"tenant_id": "tenant-acme", "region": "eu-central-1", "feature.flag": "new-checkout"}, evt.Tags)

		require.Len(t, evt.Contexts, 6)
		require.Equal(t, map[string]interface{}{"model": "Pixel 8", "memory_size": 8589934592.0, "battery_level": 57.5, "online": true}, evt.Contexts["device"])

		require.Equal(t, "POST", evt.Request.Method)
		require.Equal(t, "https://shop.example.com/checkout/pay?step=2", evt.Request.URL)
		require.Equal(t, "step=2", evt.Request.QueryString)
		require.Equal(t, "***REDACTED***", evt.Request.Headers["Authorization"], "P3")
		require.Equal(t, "***REDACTED***", evt.Request.Headers["Cookie"], "P10")
		require.Equal(t, "***REDACTED***", evt.Request.Headers["X-API-Key"], "P10")
		require.Equal(t, "Mozilla/5.0 (Linux; Android 14; Pixel 8) Chrome/126.0.6478.71", evt.Request.Headers["User-Agent"])
		require.Equal(t, map[string]interface{}{"amount": 4200.0, "currency": "EUR", "password": "***REDACTED***"}, evt.Request.Data)
		require.Equal(t, map[string]string{"REMOTE_ADDR": "10.1.2.3"}, evt.Request.Env)

		require.Len(t, evt.Breadcrumbs, 2)
		require.Equal(t, event.Breadcrumb{
			Timestamp: "2026-09-23T11:59:58Z", Type: "http", Category: "xhr", Level: "error", Message: "POST /api/pay 402",
			Data: map[string]interface{}{"password": "***REDACTED***", "status_code": 402.0},
		}, evt.Breadcrumbs[1], "P4")

		require.NotNil(t, evt.Exception)
		require.Len(t, evt.Exception.Values, 2)
		require.Equal(t, "PaymentDeclined", evt.Exception.Values[0].Type)
		raised := evt.Exception.Values[1]
		require.Equal(t, "TypeError", raised.Type)
		require.Equal(t, "Cannot read properties of undefined (reading 'id')", raised.Value)
		require.Equal(t, "onerror", raised.Mechanism.Type)
		require.NotNil(t, raised.Mechanism.Handled)
		require.False(t, *raised.Mechanism.Handled)
		require.Equal(t, event.StackFrame{
			File: "src/pay/checkout.ts", Line: 88, Column: 15, Function: "pay", InApp: true,
			Module: "pay.checkout", AbsPath: "https://shop.example.com/assets/checkout.js",
			ContextLine: "const id = order.customer.id;", PreContext: []string{"function pay(order) {"}, PostContext: []string{"  return charge(id);"},
		}, raised.Stacktrace[0])

		require.Equal(t, "pay.checkout", evt.Stacktrace[0].Module)
		require.Equal(t, []string{"function pay(order) {", "  // resolve the customer"}, evt.Stacktrace[0].PreContext)
		require.Equal(t, []string{"  return charge(id);", "}"}, evt.Stacktrace[0].PostContext)

		// Stored verbatim, masked where a rule applies (decision 5, TASK-08): the PAN in the message is
		// redacted, the UUID in it and the email/request id in metadata are not normalised.
		require.Equal(t, "charge [REDACTED_PAN] failed for order 7c9e6679-7425-40de-944b-e07fc1f90ae7", evt.Message, "P11")
		require.Equal(t, "TypeError", evt.ErrorClass)
		require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", evt.TraceID, "P9")
		require.Equal(t, "alice@example.com", evt.Metadata["email"], "P6")
		require.Equal(t, "7c9e6679-7425-40de-944b-e07fc1f90ae7", evt.Metadata["request_id"], "P9")

		// OQ-3 (spec §6.3): the typed user.id wins over metadata.user_id; the tenant_id tag is read.
		require.Equal(t, "user-typed-42", evt.AffectedUserID)
		require.Equal(t, "tenant-acme", evt.AffectedTenantID)

		// §5.2 shapes: the JSONB bodies are the wire JSON, snake_case keys, empty values omitted.
		userJSON, err := json.Marshal(evt.User)
		require.NoError(t, err)
		require.JSONEq(t, `{"id":"user-typed-42","username":"alice","email":"alice@example.com","ip_address":"2001:db8::1",
			"geo":{"city":"Berlin","region":"Berlin","country_code":"DE","subdivision":"BE"}}`, string(userJSON))
		excJSON, err := json.Marshal(evt.Exception)
		require.NoError(t, err)
		require.Contains(t, string(excJSON), `"values":[`)
		require.Contains(t, string(excJSON), `"handled":false`, "handled=false is kept: tri-state")
		frameJSON, err := json.Marshal(evt.Stacktrace[1])
		require.NoError(t, err)
		require.JSONEq(t, `{"file":"src/pay/button.ts","line":20,"function":"onClick","in_app":true,"column":3}`, string(frameJSON),
			"a frame without v2 fields serialises exactly as before")
	})
}

// I11 and spec §3.1 field 30: received_at is T_recv, stamped by IngestAt, and a client value is never decoded.
func TestEventV2_ReceivedAtIsServerStamped(t *testing.T) {
	svc, pub := newTestIngestService(t)
	payload := decodeIngestPayload(t, `{"project_key":"proj-svc-test","platform":"go","environment":"test","error_class":"E","received_at":"2001-01-01T00:00:00Z"}`)
	require.True(t, payload.ReceivedAt.IsZero(), "received_at must not be decodable from a client body")
	receivedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	require.NoError(t, svc.IngestAt(context.Background(), payload, receivedAt))
	require.Equal(t, receivedAt, publishedEvent(t, pub).GetReceivedAt().AsTime())
}

// A pre-v2 payload publishes no v2 field except the server stamp (I1).
func TestEventV2_PreV2PayloadPublishesOnlyTheStamp(t *testing.T) {
	svc, pub := newTestIngestService(t)
	payload := decodeIngestPayload(t, `{"project_key":"proj-svc-test","platform":"go","environment":"test","error_class":"E","message":"m","metadata":{"a":"b"}}`)
	receivedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	require.NoError(t, svc.IngestAt(context.Background(), payload, receivedAt))
	wire := publishedEvent(t, pub)
	require.NotNil(t, wire.GetReceivedAt())
	wire.ReceivedAt = nil
	want := &sentinelv1.ErrorEvent{
		EventId: wire.GetEventId(), ProjectKey: "proj-svc-test", Platform: "go", Environment: "test",
		ErrorClass: "E", Message: "m", Metadata: wire.GetMetadata(), Timestamp: wire.GetTimestamp(),
	}
	require.True(t, proto.Equal(want, wire), "a pre-v2 payload must publish no v2 field: %v", wire)
}

// V2c (ruling 2): a fingerprint sent over JSON is the one ComputeFingerprint keeps; with fingerprint_override it
// is recomputed.
func TestEventV2_JSONFingerprintReachesComputeFingerprint(t *testing.T) {
	run := func(t *testing.T, body string) *event.ErrorEvent {
		t.Helper()
		var payload validation.ErrorPayload
		dec := json.NewDecoder(bytes.NewReader([]byte(body)))
		dec.DisallowUnknownFields()
		require.NoError(t, dec.Decode(&payload))
		data, err := proto.Marshal(mapping.MapPayloadToEvent(&payload))
		require.NoError(t, err)
		evt, err := event.Deserialize(data)
		require.NoError(t, err)
		evt.ComputeFingerprint(symbolicate.DefaultPlatforms)
		return evt
	}
	const base = `"project_key":"p","platform":"go","environment":"production","error_class":"E","stacktrace":[{"file":"a.go","function":"f","line":1,"in_app":true}]`

	evt := run(t, `{`+base+`,"fingerprint":"fp-json-1"}`)
	require.Equal(t, "fp-json-1", evt.Fingerprint)

	overridden := run(t, `{`+base+`,"fingerprint":"fp-json-1","fingerprint_override":true}`)
	computed := run(t, `{`+base+`}`)
	require.NotEqual(t, "fp-json-1", overridden.Fingerprint, "fingerprint_override discards the client value")
	require.Equal(t, computed.Fingerprint, overridden.Fingerprint, "and recomputes the server's hash")
}
