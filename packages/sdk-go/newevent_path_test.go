package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNewEventPushPathPopulatesEveryTypedField is P1.2: the NewEvent +
// ApplyContext + Transport.Push path (core's path, §8's call order) matches
// the Client path field for field, including the metadata routing P1.3
// covers, and P2.5's "readable without Client" breadcrumb.
func TestNewEventPushPathPopulatesEveryTypedField(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	cfg := Config{ProjectKey: "pk_test", Endpoint: rs.URL, ServerName: "test-server", MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond}
	transport := newTestTransport(t, cfg)

	handler := NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		AddBreadcrumb(ctx, Breadcrumb{Message: "before capture", Category: "test"})
		ctx = WithUserInfo(ctx, UserInfo{ID: "u-1", Username: "alice", Email: "alice@example.test"})
		ctx = WithTenant(ctx, "tenant-1")
		ctx = WithTag(ctx, "region", "eu")
		ctx = WithTag(ctx, "request_id", "req-1")
		ctx = WithContextMap(ctx, map[string]interface{}{"order_ref": "o-1"})
		ctx = WithLevel(ctx, "warn")

		ctxTags := getMetadataStore(ctx)
		ev := NewEvent(cfg, errors.New("boom"), ctxTags)
		ApplyContext(ctx, ev)
		transport.Push(ev)
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest("GET", "/items/42?id=7", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !transport.Flush(2 * time.Second) {
		t.Fatal("flush timed out")
	}
	if len(rs.bodies) != 1 || len(rs.bodies[0]) != 1 {
		t.Fatalf("expected one event, got %d bodies", len(rs.bodies))
	}

	var ev map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &ev)

	if ev["level"] != "warning" {
		t.Errorf("level = %v, want warning", ev["level"])
	}
	sdk, _ := ev["sdk"].(map[string]interface{})
	if sdk == nil || sdk["name"] != "sentinel.go" || sdk["version"] != "0.4.0" {
		t.Errorf("sdk = %v, want {sentinel.go 0.4.0}", ev["sdk"])
	}
	if ev["server_name"] != "test-server" {
		t.Errorf("server_name = %v, want test-server", ev["server_name"])
	}
	if ev["transaction"] != "/items/42" {
		t.Errorf("transaction = %v, want /items/42", ev["transaction"])
	}
	user, _ := ev["user"].(map[string]interface{})
	if user == nil || user["id"] != "u-1" || user["username"] != "alice" || user["email"] != "alice@example.test" || user["ip_address"] != "203.0.113.7" {
		t.Errorf("user = %v, want {u-1 alice alice@example.test 203.0.113.7}", ev["user"])
	}
	tags, _ := ev["tags"].(map[string]interface{})
	if tags["tenant_id"] != "tenant-1" || tags["region"] != "eu" {
		t.Errorf("tags = %v, want tenant_id=tenant-1, region=eu", ev["tags"])
	}
	md, _ := ev["metadata"].(map[string]interface{})
	if md["request_id"] != "req-1" || md["order_ref"] != "o-1" {
		t.Errorf("metadata = %v, want request_id=req-1, order_ref=o-1", md)
	}
	ctxs, _ := ev["contexts"].(map[string]interface{})
	for _, want := range []string{"runtime", "os", "device"} {
		if _, ok := ctxs[want]; !ok {
			t.Errorf("contexts missing %q: %v", want, ctxs)
		}
	}
	reqMap, _ := ev["request"].(map[string]interface{})
	if reqMap == nil {
		t.Fatal("request missing")
	}
	crumbs, _ := ev["breadcrumbs"].([]interface{})
	if len(crumbs) != 1 {
		t.Fatalf("breadcrumbs = %v, want 1", crumbs)
	}
}

// TestNewEventPathNilCtxTagsThenCoreHook is P1.2's second case: nil ctxTags,
// ApplyContext against an empty context (adds no metadata key, so the map
// written is NewEvent's own), and a BeforeSend-shaped hook that writes a
// metadata key onto it.
func TestNewEventPathNilCtxTagsThenCoreHook(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	cfg := Config{
		ProjectKey: "pk_test", Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond,
		BeforeSend: func(ev *Event) *Event {
			ev.Metadata["core_hook"] = "1"
			return ev
		},
	}
	transport := newTestTransport(t, cfg)

	ev := NewEvent(cfg, errors.New("boom"), nil)
	ApplyContext(context.Background(), ev)
	ev = cfg.ApplyBeforeSend(ev)
	transport.Push(ev)

	if !transport.Flush(2 * time.Second) {
		t.Fatal("flush timed out")
	}
	var got map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &got)
	md, _ := got["metadata"].(map[string]interface{})
	if md["core_hook"] != "1" {
		t.Errorf("metadata.core_hook = %v, want 1", md)
	}
}

// TestNewEventAloneHasNoMetadataKey proves the other half: NewEvent(cfg,
// err, nil) marshalled alone has no metadata key at all (spec §3.2).
func TestNewEventAloneHasNoMetadataKey(t *testing.T) {
	ev := NewEvent(Config{}, errors.New("boom"), nil)
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["metadata"]; ok {
		t.Errorf("metadata key present on a fresh NewEvent with nil ctxTags: %s", raw)
	}
}
