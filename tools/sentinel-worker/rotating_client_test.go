// rotating_client_test.go covers plan §2.5/B5's cross-boundary wire parsing for
// sentinelRotatingClient: GET /api/agent/self's NESTED key.createdAt/key.expiresAt and POST
// /api/agent/key/rotate's NESTED newKey.secret. Nothing exercised this parsing before (B5:
// "cross-boundary payloads have no compiler checking them"), so a wrong field path could silently
// disable every rotation trigger and the persist-before-use swap without any test noticing.
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// TestSentinelRotatingClient_SelfKeyInfo_ReadsNestedKeyFields proves SelfKeyInfo decodes
// createdAt/expiresAt from the NESTED `key` object (GET /api/agent/self's real shape), not a
// top-level field.
func TestSentinelRotatingClient_SelfKeyInfo_ReadsNestedKeyFields(t *testing.T) {
	createdAt := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	expiresAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/self" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agentId":"a1","key":{"createdAt":"2026-08-01T00:00:00Z","expiresAt":"2026-09-01T00:00:00Z"}}`))
	}))
	defer srv.Close()

	c := &sentinelRotatingClient{client: sentinel.NewClient(srv.URL, "test-key")}
	info, err := c.SelfKeyInfo(t.Context())
	if err != nil {
		t.Fatalf("SelfKeyInfo: %v", err)
	}
	if info.CreatedAt == nil || !info.CreatedAt.Equal(createdAt) {
		t.Fatalf("CreatedAt = %v, want %v", info.CreatedAt, createdAt)
	}
	if info.ExpiresAt == nil || !info.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("ExpiresAt = %v, want %v", info.ExpiresAt, expiresAt)
	}
}

// TestSentinelRotatingClient_SelfKeyInfo_NullKeyFieldsAreNil proves a null createdAt/expiresAt
// (C13: createdAt may be null) decodes to nil pointers, not zero-time values -- Evaluate's
// null-expiry age fallback depends on this distinction.
func TestSentinelRotatingClient_SelfKeyInfo_NullKeyFieldsAreNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"agentId":"a1","key":{"createdAt":null,"expiresAt":null}}`))
	}))
	defer srv.Close()

	c := &sentinelRotatingClient{client: sentinel.NewClient(srv.URL, "test-key")}
	info, err := c.SelfKeyInfo(t.Context())
	if err != nil {
		t.Fatalf("SelfKeyInfo: %v", err)
	}
	if info.CreatedAt != nil {
		t.Fatalf("CreatedAt = %v, want nil", info.CreatedAt)
	}
	if info.ExpiresAt != nil {
		t.Fatalf("ExpiresAt = %v, want nil", info.ExpiresAt)
	}
}

// TestSentinelRotatingClient_SelfKeyInfo_NonOKStatusErrors proves a non-2xx /self response is
// surfaced as an error rather than silently parsed as an empty KeyInfo.
func TestSentinelRotatingClient_SelfKeyInfo_NonOKStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer srv.Close()

	c := &sentinelRotatingClient{client: sentinel.NewClient(srv.URL, "test-key")}
	if _, err := c.SelfKeyInfo(t.Context()); err == nil {
		t.Fatal("SelfKeyInfo: expected an error for a 500 response")
	}
}

// TestSentinelRotatingClient_Rotate_ReadsNestedSecret proves Rotate decodes the new secret from
// the NESTED newKey.secret field (POST /api/agent/key/rotate's real shape per
// apps/dashboard-web/src/routes/api/agent/key/rotate/+server.ts), not a top-level `key` field.
func TestSentinelRotatingClient_Rotate_ReadsNestedSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/key/rotate" || r.Method != http.MethodPost {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"newKey":{"secret":"sk_new_rotated_secret"}}`))
	}))
	defer srv.Close()

	c := &sentinelRotatingClient{client: sentinel.NewClient(srv.URL, "test-key")}
	newKey, err := c.Rotate(t.Context())
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if newKey != "sk_new_rotated_secret" {
		t.Fatalf("Rotate returned %q, want sk_new_rotated_secret", newKey)
	}
}

// TestSentinelRotatingClient_Rotate_EmptySecretErrors proves an empty (or missing) newKey.secret
// is treated as an error, never silently swapped in as the live key.
func TestSentinelRotatingClient_Rotate_EmptySecretErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"newKey":{"secret":""}}`))
	}))
	defer srv.Close()

	c := &sentinelRotatingClient{client: sentinel.NewClient(srv.URL, "test-key")}
	if _, err := c.Rotate(t.Context()); err == nil {
		t.Fatal("Rotate: expected an error for an empty secret")
	}
}

// TestSentinelRotatingClient_Rotate_NonOKStatusErrors proves a non-2xx rotate response is
// surfaced as an error rather than silently parsed as an empty/zero secret.
func TestSentinelRotatingClient_Rotate_NonOKStatusErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer srv.Close()

	c := &sentinelRotatingClient{client: sentinel.NewClient(srv.URL, "test-key")}
	if _, err := c.Rotate(t.Context()); err == nil {
		t.Fatal("Rotate: expected an error for a 403 response")
	}
}
