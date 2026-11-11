package unit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
)

// scVectorsPath is the golden vector file both the Go and TS halves read (spec §3.4).
const scVectorsPath = "../fixtures/sentry/project-id-vectors.json"

type scVectors struct {
	Valid []struct {
		UUID            string `json:"uuid"`
		SentryProjectID string `json:"sentry_project_id"`
	} `json:"valid"`
	Invalid []string `json:"invalid"`
}

func scLoadVectors(t *testing.T) scVectors {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(scVectorsPath))
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var v scVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	return v
}

// P2.2
func TestSentryCompatKeyPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		header  map[string]string
		query   string
		want    string
		wantErr string
	}{
		{"header only", map[string]string{"X-Sentry-Auth": "Sentry sentry_key=HK"}, "", "HK", ""},
		{"query only", nil, "sentry_key=QK", "QK", ""},
		{"x-api-key only", map[string]string{"X-API-Key": "AK"}, "", "AK", ""},
		{"all three: header wins", map[string]string{"X-Sentry-Auth": "Sentry sentry_key=HK", "X-API-Key": "AK"}, "sentry_key=QK", "HK", ""},
		{"query beats x-api-key", map[string]string{"X-API-Key": "AK"}, "sentry_key=QK", "QK", ""},
		{"malformed header with valid query is 401, no fall-through", map[string]string{"X-Sentry-Auth": "Basic abc"}, "sentry_key=QK", "", "Invalid X-Sentry-Auth header"},
		{"empty query key with valid x-api-key is 401", map[string]string{"X-API-Key": "AK"}, "sentry_key=", "", "Missing API key"},
		{"whitespace query key is missing", nil, "sentry_key=%20", "", "Missing API key"},
		{"none", nil, "", "", "Missing API key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/1/envelope/?"+tc.query, nil)
			for k, v := range tc.header {
				r.Header.Set(k, v)
			}
			got, err := auth.SentryKey(r)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// P2.3
func TestParseSentryAuth(t *testing.T) {
	ok := []struct{ in, want string }{
		{"Sentry sentry_version=7, sentry_client=sentry.dart.flutter/9.6.0, sentry_key=K", "K"},
		{"Sentry sentry_key=K,sentry_version=7", "K"},
		{`sentry   SENTRY_KEY = "K" `, "K"},
		{"Sentry sentry_key=K, sentry_secret=S", "K"},
		{"Sentry sentry_key=, sentry_key=K", "K"},
		{"Sentry sentry_key=A, sentry_key=B", "A"},
		{"Sentry\tsentry_key=K", "K"},
		{"Sentry sentry_key=a=b", "a=b"},
	}
	for _, tc := range ok {
		got, err := auth.ParseSentryAuth(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseSentryAuth(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	bad := []string{"Sentry sentry_version=7", "Basic abc", "sentry_key=k", "SentryX sentry_key=k", "Sentry", "", "Sentry sentry_key=", "Sentry noequals"}
	for _, in := range bad {
		if got, err := auth.ParseSentryAuth(in); err == nil {
			t.Errorf("ParseSentryAuth(%q) = %q, want error", in, got)
		}
	}
}

// P2.6
func TestSentryProjectIDGolden(t *testing.T) {
	v := scLoadVectors(t)
	for _, row := range v.Valid {
		got, err := auth.SentryProjectID(row.UUID)
		if err != nil || got != row.SentryProjectID {
			t.Errorf("SentryProjectID(%q) = %q, %v; want %q", row.UUID, got, err, row.SentryProjectID)
		}
	}
	for _, in := range v.Invalid {
		if got, err := auth.SentryProjectID(in); err == nil {
			t.Errorf("SentryProjectID(%q) = %q, want error", in, got)
		}
	}
	if got, _ := auth.SentryProjectID("ffffffff-ffff-ffff-ffff-ffffffffffff"); got != "4503599627370495" {
		t.Errorf("maximum = %q", got)
	}
}

// P6.3
func TestSentryProjectIDVectorsShared(t *testing.T) {
	v := scLoadVectors(t)
	if len(v.Valid) < 7 {
		t.Fatalf("vector file has %d valid rows, want at least 7", len(v.Valid))
	}
	if len(v.Invalid) < 7 {
		t.Fatalf("vector file has %d invalid rows, want at least 7", len(v.Invalid))
	}
	// The dashboard reads the same file by this relative path; it must not be a private copy.
	if _, err := os.Stat("../../apps/dashboard-web/tests/fixtures/sentry/project-id-vectors.json"); err == nil {
		t.Fatal("a private copy of the vector file exists under apps/dashboard-web")
	}
}

func scLimitedRequest(rpm int) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/1/envelope/", nil)
	ctx := auth.WithIdentity(r.Context(), "proj", "0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6", "org", "hash-sc-limit", rpm, false)
	return r.WithContext(ctx)
}

// P2.7
func TestSentryCompatRateLimitHeaders(t *testing.T) {
	rl := middleware.NewRateLimiter(newMiniRedisClient(t))
	h := rl.SentryMiddleware(okHandler(nil))
	first := httptest.NewRecorder()
	h.ServeHTTP(first, scLimitedRequest(1))
	if first.Code != http.StatusOK {
		t.Fatalf("first = %d", first.Code)
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, scLimitedRequest(1))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429", second.Code)
	}
	if got := second.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q", got)
	}
	if got := second.Header().Get("X-Sentry-Rate-Limits"); got != "60::organization" {
		t.Errorf("X-Sentry-Rate-Limits = %q", got)
	}
	if first.Header().Get("X-Sentry-Rate-Limits") != "" {
		t.Error("X-Sentry-Rate-Limits sent on an admitted request")
	}
}

// P2.8
func TestSentryCompatRateLimitStrictMode(t *testing.T) {
	rl := middleware.NewRateLimiter(nil)
	h := rl.SentryMiddleware(okHandler(nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, scLimitedRequest(1))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("strict nil client = %d, want 500", w.Code)
	}
	if w.Header().Get("X-Sentry-Rate-Limits") != "" {
		t.Error("Sentry header on a strict-mode failure")
	}
	t.Setenv("RATELIMIT_STRICT_MODE", "false")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, scLimitedRequest(1))
	if w.Code != http.StatusOK {
		t.Fatalf("non-strict nil client = %d, want 200", w.Code)
	}
}

// P2.9
func TestIngestRateLimitHasNoSentryHeader(t *testing.T) {
	rl := middleware.NewRateLimiter(newMiniRedisClient(t))
	h := rl.Middleware(okHandler(nil))
	h.ServeHTTP(httptest.NewRecorder(), scLimitedRequest(1))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, scLimitedRequest(1))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q", got)
	}
	if _, present := w.Header()["X-Sentry-Rate-Limits"]; present {
		t.Error("/ingest 429 carries X-Sentry-Rate-Limits")
	}
}

func scLookup(keys map[string]*auth.APIKeyData) func(context.Context, string) (*auth.APIKeyData, error) {
	return func(_ context.Context, hash string) (*auth.APIKeyData, error) {
		if d, ok := keys[hash]; ok {
			return d, nil
		}
		return nil, http.ErrNoCookie // any error: unknown key
	}
}

func scHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// P2.10
func TestIngestAuthUnchanged(t *testing.T) {
	pid := "0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6"
	mk := func(scope string) *auth.APIKeyData {
		return &auth.APIKeyData{ProjectID: &pid, OrganizationID: "org", Scope: scope, ProjectName: "proj"}
	}
	a := auth.NewAPIKeyAuthenticatorWithLookup(scLookup(map[string]*auth.APIKeyData{
		scHash("k-ingest"): mk("ingest"),
		scHash("k-admin"):  mk("admin"),
		scHash("k-read"):   mk("read"),
	}))
	calls := 0
	h := a.Middleware(auth.ScopesIngest, okHandler(&calls))
	do := func(mut func(*http.Request)) int {
		r := httptest.NewRequest(http.MethodPost, "/ingest", nil)
		mut(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := do(func(r *http.Request) { r.Header.Set("X-API-Key", "k-ingest") }); c != 200 {
		t.Errorf("ingest key = %d", c)
	}
	if c := do(func(r *http.Request) { r.Header.Set("X-API-Key", "k-admin") }); c != 200 {
		t.Errorf("admin key = %d", c)
	}
	if c := do(func(r *http.Request) { r.Header.Set("X-API-Key", "k-read") }); c != 403 {
		t.Errorf("read key = %d, want 403", c)
	}
	before := calls
	if c := do(func(r *http.Request) { r.Header.Set("X-Sentry-Auth", "Sentry sentry_key=k-ingest") }); c != 401 {
		t.Errorf("X-Sentry-Auth on /ingest = %d, want 401", c)
	}
	if c := do(func(r *http.Request) { r.URL.RawQuery = "sentry_key=k-ingest" }); c != 401 {
		t.Errorf("?sentry_key on /ingest = %d, want 401", c)
	}
	if calls != before {
		t.Error("handler ran for a Sentry-presented key on /ingest")
	}
}

// sentinel-relay-key-trust §4.1 / K2: a `relay` key is stored, works on the Sentry-compatible routes only, is
// project-scoped only, and /ingest refuses it.
func TestRelayKeyScope(t *testing.T) {
	pid := "0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6"
	pathID, err := auth.SentryProjectID(pid)
	require.NoError(t, err)
	a := auth.NewAPIKeyAuthenticatorWithLookup(scLookup(map[string]*auth.APIKeyData{
		scHash("k-relay"):     {ProjectID: &pid, OrganizationID: "org", Scope: "relay", ProjectName: "proj"},
		scHash("k-ingest"):    {ProjectID: &pid, OrganizationID: "org", Scope: "ingest", ProjectName: "proj"},
		scHash("k-relay-org"): {OrganizationID: "org", Scope: "relay"},
	}))

	// /ingest and /ingest/batch keep {ingest, admin}: a relay key is 403 and the handler never runs.
	assert.Equal(t, auth.ScopeSet{"ingest", "admin"}, auth.ScopesIngest)
	calls := 0
	h := a.Middleware(auth.ScopesIngest, okHandler(&calls))
	r := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	r.Header.Set("X-API-Key", "k-relay")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Zero(t, calls)

	// The compat routes accept it, with the D2 path check unchanged, and record the scope on the context.
	var gotScope string
	mux := scMountedAuth(t, a, func(ctx context.Context) { gotScope, _ = auth.ScopeFromContext(ctx) })
	do := func(path, key string) int {
		rr := httptest.NewRequest(http.MethodPost, path, nil)
		rr.Header.Set("X-Sentry-Auth", "Sentry sentry_key="+key)
		ww := httptest.NewRecorder()
		mux.ServeHTTP(ww, rr)
		return ww.Code
	}
	for _, route := range []string{"envelope", "store"} {
		gotScope = ""
		assert.Equal(t, http.StatusNoContent, do("/api/"+pathID+"/"+route+"/", "k-relay"), route)
		assert.Equal(t, "relay", gotScope, route)
		assert.Equal(t, http.StatusForbidden, do("/api/1/"+route+"/", "k-relay"), route+": another project's id")
		assert.Equal(t, http.StatusForbidden, do("/api/"+pathID+"/"+route+"/", "k-relay-org"), route+": an org-wide relay key")
	}
	assert.Equal(t, http.StatusNoContent, do("/api/"+pathID+"/envelope/", "k-ingest"))
	assert.Equal(t, "ingest", gotScope, "an ingest key's scope is not relay")
}
