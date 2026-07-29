package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
	"github.com/jackc/pgx/v5/pgxpool"
	libredis "github.com/redis/go-redis/v9"
)

type APIKeyData struct {
	ProjectID      *string    `json:"project_id"`
	OrganizationID string     `json:"organization_id"`
	Scope          string     `json:"scope"`
	RateLimitRPM   int        `json:"rate_limit_rpm"`
	ProjectName    string     `json:"project_name"`
	ExpiresAt      *time.Time `json:"expires_at"`
}

type APIKeyAuthenticator struct {
	db    *pgxpool.Pool
	redis *libredis.Client
	sub   *nats.Subscriber
}

func NewAPIKeyAuthenticator(db *pgxpool.Pool, redis *libredis.Client, sub *nats.Subscriber) *APIKeyAuthenticator {
	auth := &APIKeyAuthenticator{db: db, redis: redis, sub: sub}
	if sub != nil {
		go func() {
			_ = sub.Subscribe(context.Background(), func(msg []byte) error {
				var data map[string]string
				if err := json.Unmarshal(msg, &data); err == nil {
					// The dashboard (apps/dashboard-web/src/lib/db/queries/apikeys.ts)
					// publishes a JS object literal `{ keyId, keyHash }`, which
					// JSON.stringify serializes with camelCase keys as-is — NOT
					// `key_hash`. Reading "key_hash" here (the previous bug, S7) meant
					// this field was never present in the decoded map, the cache entry
					// was never deleted, and revocation silently waited out the 60s
					// Redis TTL instead of being instant. The Redis cache is keyed by
					// hash ("apikey:"+hash), not by the dashboard's row id, so keyHash
					// is what actually matters here; keyId is accepted too (and
					// harmless to ignore) so the wire payload stays self-describing for
					// any future consumer that needs to look the key up by id instead.
					if keyHash, ok := data["keyHash"]; ok && keyHash != "" {
						if auth.redis != nil {
							_ = auth.redis.Del(context.Background(), "apikey:"+keyHash)
						}
					}
				}
				return nil
			})
		}()
	}
	return auth
}

func (a *APIKeyAuthenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			http.Error(w, "Missing API key", http.StatusUnauthorized)
			return
		}

		hash := sha256.Sum256([]byte(apiKey))
		hashStr := hex.EncodeToString(hash[:])

		data, err := a.getAPIKeyData(r.Context(), hashStr)
		if err != nil {
			http.Error(w, "Invalid API key", http.StatusUnauthorized)
			return
		}

		// Allowlist, not a denylist. specs/008-api-key-management/spec.md FR-003 defines
		// exactly three scopes: `ingest` (ingestion endpoints only), `read` (dashboard/query
		// APIs only, never ingestion), `admin` (full access, including ingestion). This
		// handler only ever serves /ingest and /ingest/batch, so only `ingest` and `admin`
		// may pass. The previous form (`if data.Scope == "read" { 403 }`) happened to allow
		// the same two scopes today only because those are the only other values that exist —
		// it fails open for any future scope value instead of rejecting it, which is exactly
		// backwards for an auth check.
		if data.Scope != "ingest" && data.Scope != "admin" {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		var projectKey, projectID string
		orgWide := data.ProjectID == nil || data.ProjectName == ""

		if !orgWide {
			// Project-scoped key: the project is fixed by the credential itself.
			projectKey = data.ProjectName
			projectID = *data.ProjectID
		} else {
			// Organization-wide key: the caller names the target project. Whatever it names is
			// CLIENT-SUPPLIED and untrusted — resolving it globally would just relocate S6's
			// cross-tenant write from the body to a header, so resolution is always scoped to the
			// key's own organization and a name outside it is a 403.
			//
			// spec 008 allows an org-wide key to name its target project EITHER via this header
			// OR via the body's project_key. The body is not decoded yet at middleware time, so
			// an absent header is not an error here — it defers to the handler, which resolves
			// payload.ProjectKey through ResolveProjectInOrg with the same org scoping.
			projectKey = r.Header.Get("X-Project-Key")
			if projectKey != "" {
				var err error
				projectID, err = ResolveProjectInOrg(r.Context(), a.db, projectKey, data.OrganizationID)
				if err != nil {
					http.Error(w, "Project not found in this organization", http.StatusForbidden)
					return
				}
			}
		}

		ctx := WithIdentity(r.Context(), projectKey, projectID, data.OrganizationID, hashStr, data.RateLimitRPM, orgWide)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ResolveProjectInOrg maps a project's unique name to its id WITHIN one organization.
//
// The organization scope is the whole point: resolving a client-supplied project name globally is
// what let any key write into any tenant's project (VERIFIED_STATE.md S6). The error is deliberately
// indistinguishable between "no such project" and "belongs to another organization", so it cannot be
// used to enumerate other tenants' project names.
func ResolveProjectInOrg(ctx context.Context, db *pgxpool.Pool, projectName, organizationID string) (string, error) {
	var projectID string
	err := db.QueryRow(ctx,
		`SELECT id::text FROM projects WHERE name = $1 AND organization_id = $2`,
		projectName, organizationID,
	).Scan(&projectID)
	if err != nil {
		return "", fmt.Errorf("project not found in organization")
	}
	return projectID, nil
}

// defaultCacheTTL is the normal Redis cache lifetime for a validated API key. Capped down to
// (expires_at - now) below when a key's own expiry is sooner, so an about-to-expire key cannot
// be cached past its expiry (see the comment in getAPIKeyData).
const defaultCacheTTL = 60 * time.Second

// cacheTTL bounds how long a validity change made OUTSIDE the application (direct SQL, a migration)
// can go unnoticed. Application-driven revocation and rotation do not wait for it — they publish
// api_key.invalidated, which deletes the entry immediately. Lower it if out-of-band edits are common.
func cacheTTL() time.Duration {
	if v := os.Getenv("APIKEY_CACHE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		log.Printf("auth: ignoring unparseable APIKEY_CACHE_TTL=%q; using default", v)
	}
	return defaultCacheTTL
}

// CacheTTLForLogging exposes the effective cache TTL so callers can report the revocation-latency
// they are actually offering when invalidation is unavailable.
func CacheTTLForLogging() time.Duration { return cacheTTL() }

func (a *APIKeyAuthenticator) getAPIKeyData(ctx context.Context, hashStr string) (*APIKeyData, error) {
	cacheKey := "apikey:" + hashStr
	if a.redis != nil {
		if val, err := a.redis.Get(ctx, cacheKey).Result(); err == nil {
			var data APIKeyData
			if err := json.Unmarshal([]byte(val), &data); err == nil {
				// This catches an expiry that was ALREADY KNOWN when the entry was cached.
				//
				// It does NOT catch an expiry written or shortened AFTER caching — the cached
				// payload still says expires_at=null, so the key stays valid for the remainder of
				// the TTL. Do not read the TTL cap below as closing that: it can only ever see
				// expires_at as of the moment of caching. The actual closure for app-driven changes
				// is the api_key.invalidated message that revokeApiKey/rotateApiKey publish, which
				// deletes this entry outright. An out-of-band `UPDATE project_api_keys SET
				// expires_at = ...` in psql bypasses that and IS bounded only by cacheTTL — which
				// is true of any cache and is why cacheTTL is env-tunable below.
				if data.ExpiresAt == nil || data.ExpiresAt.After(time.Now()) {
					return &data, nil
				}
				_ = a.redis.Del(ctx, cacheKey)
			}
		}
	}

	var data APIKeyData
	var status string
	var projID *string
	var projName *string
	var expiresAt *time.Time

	// expires_at is enforced in the query itself, not just checked in Go after the fact:
	// a row that is present but expired must be indistinguishable from "no such key" to the
	// caller (both end up as pgx.ErrNoRows below -> 401 "Invalid API key"), and filtering here
	// means an expired key can never accidentally be scanned into `data` and cached.
	err := a.db.QueryRow(ctx,
		`SELECT pak.project_id, pak.organization_id, pak.scope, pak.rate_limit_rpm, pak.status, pak.expires_at, p.name
		 FROM project_api_keys pak
		 LEFT JOIN projects p ON p.id = pak.project_id
		 WHERE pak.key_hash = $1 AND (pak.expires_at IS NULL OR pak.expires_at > now())`,
		hashStr,
	).Scan(&projID, &data.OrganizationID, &data.Scope, &data.RateLimitRPM, &status, &expiresAt, &projName)

	if err != nil {
		return nil, err
	}

	if status != "active" {
		return nil, errors.New("key is not active")
	}

	if projID != nil {
		pid := *projID
		data.ProjectID = &pid
	}
	if projName != nil {
		data.ProjectName = *projName
	}
	data.ExpiresAt = expiresAt

	if a.redis != nil {
		if b, err := json.Marshal(data); err == nil {
			// A key cached at defaultCacheTTL that expires sooner than that would
			// otherwise stay valid (from the cache's point of view) for up to the full
			// 60s after expires_at has passed — the exact gap this fix closes. Cap the
			// cache entry's own TTL at the key's remaining lifetime so it either falls
			// out of Redis or fails the ExpiresAt check above by the time it matters.
			ttl := cacheTTL()
			if data.ExpiresAt != nil {
				if remaining := time.Until(*data.ExpiresAt); remaining < ttl {
					ttl = remaining
				}
			}
			if ttl > 0 {
				_ = a.redis.Set(ctx, cacheKey, b, ttl)
			}
		}
	}

	return &data, nil
}
