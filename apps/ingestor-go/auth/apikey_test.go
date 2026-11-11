package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIKeyAuthenticator_MissingKey(t *testing.T) {
	auth := NewAPIKeyAuthenticator(nil, nil, nil)

	handler := auth.Middleware(ScopesIngest, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", rr.Code)
	}
}

// The per-route scope allowlist (sentinel-user-report-issue contract §2 / OQ-3).
//
// `report` is a new project_api_keys.scope value. Before this change the middleware held ONE
// allowlist for the whole process, so the only way to let a report key reach a report route was to
// widen that list -- which would also have let it POST /ingest. These tests are the fail-closed
// half of that: they must go red if `ScopesIngest` ever grows `report`, or if `Middleware` stops
// consulting the allowlist it was handed.

func TestScopeSet_Allows(t *testing.T) {
	cases := []struct {
		name  string
		set   ScopeSet
		scope string
		want  bool
	}{
		{"ingest key on the ingest chain", ScopesIngest, "ingest", true},
		{"admin key on the ingest chain", ScopesIngest, "admin", true},
		{"report key on the ingest chain", ScopesIngest, "report", false},
		{"read key on the ingest chain", ScopesIngest, "read", false},
		{"agent key on the ingest chain", ScopesIngest, "agent", false},
		{"relay key on the ingest chain", ScopesIngest, "relay", false},
		{"report key on the report chain", ScopesReport, "report", true},
		{"admin key on the report chain", ScopesReport, "admin", true},
		{"ingest key on the report chain", ScopesReport, "ingest", false},
		{"read key on the report chain", ScopesReport, "read", false},
		{"agent key on the report chain", ScopesReport, "agent", false},
		{"a scope nobody has catalogued yet", ScopesIngest, "superuser", false},
		{"the empty scope is never allowed", ScopesIngest, "", false},
		{"an empty allowlist allows nothing", ScopeSet{}, "admin", false},
		{"a nil allowlist allows nothing", nil, "admin", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.set.Allows(tc.scope); got != tc.want {
				t.Errorf("ScopeSet(%v).Allows(%q) = %v, want %v", tc.set, tc.scope, got, tc.want)
			}
		})
	}
}

// stubLookup replaces the Postgres/Redis key lookup so the allowlist branch of Middleware -- and
// only that branch -- is what the test exercises. A project-scoped row is used so the org-wide
// X-Project-Key path (which does need a *pgxpool.Pool) is never entered.
func stubLookup(scope string) func(context.Context, string) (*APIKeyData, error) {
	projectID := "11111111-1111-4111-8111-111111111111"
	return func(context.Context, string) (*APIKeyData, error) {
		return &APIKeyData{
			ProjectID:      &projectID,
			OrganizationID: "22222222-2222-4222-8222-222222222222",
			Scope:          scope,
			RateLimitRPM:   60,
			ProjectName:    "demo",
		}, nil
	}
}

func TestMiddleware_EnforcesThePerChainAllowlist(t *testing.T) {
	cases := []struct {
		name     string
		allowed  ScopeSet
		keyScope string
		want     int
	}{
		{"/ingest admits an ingest key", ScopesIngest, "ingest", http.StatusOK},
		{"/ingest admits an admin key", ScopesIngest, "admin", http.StatusOK},
		{"/ingest rejects a report key", ScopesIngest, "report", http.StatusForbidden},
		{"/ingest rejects a read key", ScopesIngest, "read", http.StatusForbidden},
		{"/ingest rejects an agent key", ScopesIngest, "agent", http.StatusForbidden},
		{"/ingest rejects a relay key", ScopesIngest, "relay", http.StatusForbidden},
		{"a report chain admits a report key", ScopesReport, "report", http.StatusOK},
		{"a report chain admits an admin key", ScopesReport, "admin", http.StatusOK},
		{"a report chain rejects an ingest key", ScopesReport, "ingest", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewAPIKeyAuthenticator(nil, nil, nil)
			a.lookup = stubLookup(tc.keyScope)

			reached := false
			handler := a.Middleware(tc.allowed, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest("POST", "/ingest", nil)
			req.Header.Set("X-API-Key", "sent_"+tc.keyScope+"_secret")
			rr := httptest.NewRecorder()

			handler.ServeHTTP(rr, req)

			if rr.Code != tc.want {
				t.Errorf("status = %d, want %d", rr.Code, tc.want)
			}
			if wantReached := tc.want == http.StatusOK; reached != wantReached {
				t.Errorf("inner handler reached = %v, want %v", reached, wantReached)
			}
		})
	}
}
