package sentinel

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClientPathPopulatesEveryTypedField is P1.1: the Client path, driven
// through a real HTTPMiddleware request (a bare handler, so transaction is
// the request path on every toolchain, spec §6.3), populates every typed
// field spec §8 lists (P1.3(a), P2.5 included: the breadcrumb is attached by
// CaptureErrorContext and readable without Client).
func TestClientPathPopulatesEveryTypedField(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	c := Init(Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		ServerName:    "test-server",
		MaxBufferSize: 10,
		BatchSize:     10,
		BatchWait:     10 * time.Millisecond,
	})

	handler := NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		AddBreadcrumb(ctx, Breadcrumb{Message: "before capture", Category: "test"})
		ctx = WithUserInfo(ctx, UserInfo{ID: "u-1", Username: "alice", Email: "alice@example.test"})
		ctx = WithTenant(ctx, "tenant-1")
		ctx = WithTag(ctx, "region", "eu")
		ctx = WithTag(ctx, "request_id", "req-1")
		ctx = WithContextMap(ctx, map[string]interface{}{"order_ref": "o-1"})
		ctx = WithLevel(ctx, "warn")

		c.CaptureErrorContext(ctx, errors.New("boom"), nil)
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest("GET", "/items/42?id=7", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !c.transport.Flush(2 * time.Second) {
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
	if _, ok := tags["request_id"]; ok {
		t.Errorf("request_id must not route to tags: %v", tags)
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
	for _, want := range []string{"method", "url", "query_string", "headers"} {
		if _, ok := reqMap[want]; !ok {
			t.Errorf("request missing %q: %v", want, reqMap)
		}
	}
	crumbs, _ := ev["breadcrumbs"].([]interface{})
	if len(crumbs) != 1 {
		t.Fatalf("breadcrumbs = %v, want 1", crumbs)
	}
}
