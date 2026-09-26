package sentinel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// captureHandler runs f (a NewEvent/ApplyContext/Push capture) then answers 202.
func captureHandler(transport *Transport, f func(r *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f(r)
		w.WriteHeader(http.StatusAccepted)
	}
}

func pushCaptured(t *testing.T, transport *Transport, r *http.Request) {
	t.Helper()
	ev := NewEvent(Config{}, nil, nil)
	ApplyContext(r.Context(), ev)
	transport.Push(ev)
}

func bodyContains(t *testing.T, body []byte, needle string) bool {
	t.Helper()
	return strings.Contains(string(body), needle)
}

func TestHTTPMiddlewarePanicRecovery(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()

	Init(Config{
		ProjectKey:    "pk_test",
		Endpoint:      rs.URL,
		MaxBufferSize: 10,
		BatchSize:     10,
		BatchWait:     10 * time.Millisecond,
	})

	panicHandler := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("something went terribly wrong")
	}))

	req := httptest.NewRequest("GET", "/panic", nil)
	w := httptest.NewRecorder()

	panicHandler.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", w.Code)
	}

	Flush(1 * time.Second)

	if len(rs.bodies) != 1 || len(rs.bodies[0]) != 1 {
		t.Fatalf("expected one event, got bodies=%v", rs.bodies)
	}
	var ev map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &ev)

	if ev["level"] != "fatal" {
		t.Fatalf("level = %v, want fatal", ev["level"])
	}
	req2, _ := ev["request"].(map[string]interface{})
	if req2 == nil || req2["method"] != "GET" {
		t.Fatalf("request.method missing/wrong: %v", ev["request"])
	}
	md, _ := ev["metadata"].(map[string]interface{})
	if md == nil || md["panic"] != true {
		t.Fatalf("metadata.panic != true: %v", ev["metadata"])
	}
	if s, ok := md["panic_stacktrace"].(string); !ok || s == "" {
		t.Fatalf("metadata.panic_stacktrace missing/empty: %v", md["panic_stacktrace"])
	}
}

func TestMiddlewareRequestHeaders(t *testing.T) {
	for _, captureIP := range []bool{false, true} {
		t.Run(boolLabel(captureIP), func(t *testing.T) {
			rs := newRecordingServer(t)
			defer rs.Close()
			transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})

			mw := NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: captureIP})
			handler := mw(captureHandler(transport, func(r *http.Request) { pushCaptured(t, transport, r) }))

			req := httptest.NewRequest("POST", "/items/42", strings.NewReader("body-marker-should-never-appear"))
			h := req.Header
			// credential headers (a): the five names
			h.Set("Authorization", "Bearer secret-a")
			h.Set("Proxy-Authorization", "Bearer secret-b")
			h.Set("Cookie", "session=secret-c")
			h.Set("Set-Cookie", "session=secret-d")
			h.Set("X-Api-Key", "secret-e")
			// credential headers not in the SDK PII substring list, still dropped by name
			h.Set("X-Auth-Token", "secret-f")
			h.Set("X-Db-Credential", "secret-g")
			// credential headers (b): patterns
			h.Set("Cf-Access-Jwt-Assertion", "hdr.eyJlbWFpbCI6Im1hcmtlckBleGFtcGxlLnRlc3QifQ.sig")
			h.Set("X-Amzn-Oidc-Data", "hdr.eyJlbWFpbCI6Im1hcmtlckBleGFtcGxlLnRlc3QifQ.sig")
			h.Set("X-Jwt-Payload", "jwt-marker")
			h.Set("X-Saml-Assertion", "assertion-marker")
			h.Set("X-Ms-Client-Principal", "principal-marker")
			h.Set("X-Session-Id", "session-marker")
			// credential headers (c): -key suffix
			h.Set("Ocp-Apim-Subscription-Key", "key-marker-1")
			h.Set("X-Functions-Key", "key-marker-2")
			// client-IP headers: seven names
			h.Set("X-Forwarded-For", "198.51.100.1")
			h.Set("Forwarded", "for=198.51.100.2")
			h.Set("X-Real-Ip", "198.51.100.3")
			h.Set("True-Client-Ip", "198.51.100.4")
			h.Set("Cf-Connecting-Ip", "198.51.100.5")
			h.Set("X-Client-Ip", "198.51.100.6")
			h.Set("X-Cluster-Client-Ip", "198.51.100.7")
			// client-IP headers: name patterns
			h.Add("X-Original-Forwarded-For", "198.51.100.8")
			h.Set("X-Envoy-External-Address", "198.51.100.9")
			h.Set("X-Appengine-User-Ip", "198.51.100.10")
			h.Set("X-ClientIP", "198.51.100.11")
			h.Set("X-Nf-Client-Connection-Ip", "198.51.100.12")
			h.Set("X-Originating-Ip", "198.51.100.13")
			h.Set("X-Remote-Ip", "198.51.100.14")
			h.Set("X-Remote-Addr", "198.51.100.15")
			h.Set("X-Forwarded-Host", "evil.example.test")
			// value-rule-only
			h.Set("X-Custom-Peer", "198.51.100.20, [2001:db8::7]:443")
			h.Set("X-Via-Addr", "198.51.100.21:8080")
			// non-canonical case duplicate
			h.Set("x-real-ip", "198.51.100.30")
			h.Set("x-original-forwarded-for", "198.51.100.31")
			// kept
			h.Set("X-Build", "1.2.3")
			h.Set("Accept", "application/json")
			h.Set("User-Agent", "test-agent/1.0")

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			transport.Flush(1 * time.Second)

			if len(rs.bodies) != 1 || len(rs.bodies[0]) != 1 {
				t.Fatalf("expected one event, got %d bodies", len(rs.bodies))
			}
			raw := rs.bodies[0][0]
			var ev map[string]interface{}
			mustUnmarshal(t, raw, &ev)
			reqMap, _ := ev["request"].(map[string]interface{})
			if reqMap == nil {
				t.Fatal("request missing")
			}
			hdrs, _ := reqMap["headers"].(map[string]interface{})
			want := map[string]bool{"Accept": true, "User-Agent": true, "X-Build": true, "Host": true}
			for k := range hdrs {
				if !want[k] {
					t.Errorf("unexpected header present: %s = %v", k, hdrs[k])
				}
			}
			for k := range want {
				if _, ok := hdrs[k]; !ok {
					t.Errorf("expected header missing: %s", k)
				}
			}
			markers := []string{
				"secret-a", "secret-b", "secret-c", "secret-d", "secret-e", "secret-f", "secret-g",
				"jwt-marker", "assertion-marker", "principal-marker", "session-marker",
				"key-marker-1", "key-marker-2",
				"marker@example.test", "body-marker-should-never-appear",
				"198.51.100.1", "198.51.100.2", "198.51.100.3", "198.51.100.4", "198.51.100.5",
				"198.51.100.6", "198.51.100.7", "198.51.100.8", "198.51.100.9", "198.51.100.10",
				"198.51.100.11", "198.51.100.12", "198.51.100.13", "198.51.100.14", "198.51.100.15",
				"198.51.100.20", "198.51.100.21", "198.51.100.30", "198.51.100.31", "2001:db8::7",
			}
			for _, m := range markers {
				if bodyContains(t, raw, m) {
					t.Errorf("marker %q leaked into body", m)
				}
			}
			rs.bodies = nil
		})
	}
}

func boolLabel(b bool) string {
	if b {
		return "captureClientIP=true"
	}
	return "captureClientIP=false"
}

func TestMiddlewareQueryStringFiltered(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"id=7&api_key=s&token=t", "id=7&api_key=[FILTERED]&token=[FILTERED]"},
		{"a=1;reset_token=X&b=2", "a=1;reset_token=[FILTERED]&b=2"},
		{"api_key%zz=s&id=7", "api_key%zz=[FILTERED]&id=7"},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			rs := newRecordingServer(t)
			defer rs.Close()
			transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})
			mw := NewHTTPMiddleware(MiddlewareOptions{})
			handler := mw(captureHandler(transport, func(r *http.Request) { pushCaptured(t, transport, r) }))

			req := httptest.NewRequest("GET", "/items/42?"+c.raw, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			transport.Flush(1 * time.Second)

			var ev map[string]interface{}
			mustUnmarshal(t, rs.bodies[0][0], &ev)
			reqMap := ev["request"].(map[string]interface{})
			if got := reqMap["query_string"]; got != c.want {
				t.Errorf("query_string = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMiddlewareURLHeadersFiltered(t *testing.T) {
	cases := []struct {
		name, header, value string
		wantKey              bool
		want                  string
		marker                string
	}{
		{"referer", "Referer", "https://app.example.test/reset?token=abc&x=1", true, "https://app.example.test/reset?token=[FILTERED]&x=1", "abc"},
		{"referer-userinfo-fragment", "Referer", "https://u:pw-marker@app.example.test/p?x=1#frag-marker", true, "https://app.example.test/p?x=1", "pw-marker"},
		{"x-original-url", "X-Original-Url", "/reset?password=p-marker&id=7", true, "/reset?password=[FILTERED]&id=7", "p-marker"},
		{"referer-unparsable", "Referer", "https://app.example.test/%zz?token=bad-marker", false, "", "bad-marker"},
		{"origin", "Origin", "https://app.example.test", true, "https://app.example.test", ""},
		{"link", "Link", "<https://cdn.example.test/a.js?token=link-marker>; rel=preload", false, "", "link-marker"},
		{"x-forwarded-uri", "X-Forwarded-Uri", "/reset?token=fwd-marker", false, "", "fwd-marker"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs := newRecordingServer(t)
			defer rs.Close()
			transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})
			mw := NewHTTPMiddleware(MiddlewareOptions{})
			handler := mw(captureHandler(transport, func(r *http.Request) { pushCaptured(t, transport, r) }))

			req := httptest.NewRequest("GET", "/items/42", nil)
			req.Header.Set(c.header, c.value)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			transport.Flush(1 * time.Second)

			var ev map[string]interface{}
			mustUnmarshal(t, rs.bodies[0][0], &ev)
			reqMap, _ := ev["request"].(map[string]interface{})
			hdrs, _ := reqMap["headers"].(map[string]interface{})
			got, ok := hdrs[http.CanonicalHeaderKey(c.header)]
			if ok != c.wantKey {
				t.Fatalf("%s present=%v, want %v (value=%v)", c.header, ok, c.wantKey, got)
			}
			if c.wantKey && got != c.want {
				t.Errorf("%s = %q, want %q", c.header, got, c.want)
			}
			if c.marker != "" && bodyContains(t, rs.bodies[0][0], c.marker) {
				t.Errorf("marker %q leaked into body", c.marker)
			}
		})
	}
}

func TestMiddlewareRequestURL(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})
	mw := NewHTTPMiddleware(MiddlewareOptions{})
	handler := mw(captureHandler(transport, func(r *http.Request) { pushCaptured(t, transport, r) }))

	req := httptest.NewRequest("GET", "http://api.example.test/items/42?x=1#f", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	transport.Flush(1 * time.Second)

	var ev map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &ev)
	reqMap := ev["request"].(map[string]interface{})
	if got := reqMap["url"]; got != "http://api.example.test/items/42" {
		t.Errorf("url = %q, want http://api.example.test/items/42 (query/fragment must not appear, X-Forwarded-Proto must not be trusted)", got)
	}
}

// TestHTTPMiddlewareDefaultNoIP pins HTTPMiddleware's own IP-off default
// (middleware.go:88, HTTPMiddleware = NewHTTPMiddleware(MiddlewareOptions{}))
// through HTTPMiddleware itself, not through NewHTTPMiddleware with an
// explicit zero-value MiddlewareOptions - TestMiddlewareClientIPOptIn's
// default_no_ip case exercises the latter, which would stay green even if
// HTTPMiddleware were changed to pass CaptureClientIP: true.
func TestHTTPMiddlewareDefaultNoIP(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.Close()
	transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})
	handler := HTTPMiddleware(captureHandler(transport, func(r *http.Request) { pushCaptured(t, transport, r) }))

	req := httptest.NewRequest("GET", "/items/42", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	transport.Flush(1 * time.Second)

	var ev map[string]interface{}
	mustUnmarshal(t, rs.bodies[0][0], &ev)
	if u, ok := ev["user"].(map[string]interface{}); ok {
		if _, has := u["ip_address"]; has {
			t.Fatalf("ip_address present without opt-in: %v", u)
		}
	}
}

func TestMiddlewareClientIPOptIn(t *testing.T) {
	t.Run("default_no_ip", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})
		mw := NewHTTPMiddleware(MiddlewareOptions{})
		handler := mw(captureHandler(transport, func(r *http.Request) { pushCaptured(t, transport, r) }))

		req := httptest.NewRequest("GET", "/items/42", nil)
		req.RemoteAddr = "203.0.113.7:5555"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		transport.Flush(1 * time.Second)

		var ev map[string]interface{}
		mustUnmarshal(t, rs.bodies[0][0], &ev)
		if ev["user"] != nil {
			if u, ok := ev["user"].(map[string]interface{}); ok {
				if _, has := u["ip_address"]; has {
					t.Fatalf("ip_address present without opt-in: %v", u)
				}
			}
		}
	})

	t.Run("opt_in", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})
		mw := NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: true})
		handler := mw(captureHandler(transport, func(r *http.Request) { pushCaptured(t, transport, r) }))

		req := httptest.NewRequest("GET", "/items/42", nil)
		req.RemoteAddr = "203.0.113.7:5555"
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		req.Header.Set("X-Real-Ip", "198.51.100.2")
		req.Header.Set("X-Original-Forwarded-For", "198.51.100.9")
		req.Header.Set("X-Envoy-External-Address", "198.51.100.10")
		req.Header.Set("X-Nf-Client-Connection-Ip", "198.51.100.11")
		req.Header.Set("X-Originating-Ip", "198.51.100.12")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		transport.Flush(1 * time.Second)

		var ev map[string]interface{}
		mustUnmarshal(t, rs.bodies[0][0], &ev)
		u, _ := ev["user"].(map[string]interface{})
		if u == nil || u["ip_address"] != "203.0.113.7" {
			t.Fatalf("ip_address = %v, want 203.0.113.7", ev["user"])
		}
		reqMap, _ := ev["request"].(map[string]interface{})
		hdrs, _ := reqMap["headers"].(map[string]interface{})
		for _, addr := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.9", "198.51.100.10", "198.51.100.11", "198.51.100.12"} {
			if bodyContains(t, rs.bodies[0][0], addr) {
				t.Errorf("client-IP header value %q leaked into body", addr)
			}
		}
		_ = hdrs
	})

	t.Run("with_user_info_wins", func(t *testing.T) {
		rs := newRecordingServer(t)
		defer rs.Close()
		transport := newTestTransport(t, Config{Endpoint: rs.URL, MaxBufferSize: 10, BatchSize: 10, BatchWait: 10 * time.Millisecond})
		mw := NewHTTPMiddleware(MiddlewareOptions{CaptureClientIP: true})
		handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithUserInfo(r.Context(), UserInfo{IPAddress: "10.0.0.9"})
			ev := NewEvent(Config{}, nil, nil)
			ApplyContext(ctx, ev)
			transport.Push(ev)
			w.WriteHeader(http.StatusAccepted)
		}))

		req := httptest.NewRequest("GET", "/items/42", nil)
		req.RemoteAddr = "203.0.113.7:5555"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		transport.Flush(1 * time.Second)

		var ev map[string]interface{}
		mustUnmarshal(t, rs.bodies[0][0], &ev)
		u := ev["user"].(map[string]interface{})
		if u["ip_address"] != "10.0.0.9" {
			t.Errorf("ip_address = %v, want 10.0.0.9 (WithUserInfo must win)", u["ip_address"])
		}
	})
}
