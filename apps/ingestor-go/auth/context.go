package auth

import "context"

// ctxKey is a package-private context key type. Bare string keys ("project_key", "api_key_hash", …)
// were used previously, which Go vet flags and which collide silently across packages — any other
// package storing a value under the same string would overwrite the authenticated identity.
type ctxKey int

const (
	ctxKeyProjectKey ctxKey = iota
	ctxKeyProjectID
	ctxKeyRateLimitRPM
	ctxKeyAPIKeyHash
	ctxKeyOrganizationID
	ctxKeyOrgWide
)

// WithIdentity stores the identity resolved from the presented API key.
//
// This is the authenticated tenant scope. It MUST be the only source of tenancy for a request:
// anything read from the request body is attacker-controlled. See VERIFIED_STATE.md S6/S16 and B7 —
// the identity used to be computed here and then thrown away, so a holder of any valid ingest key
// could write into any other tenant's project simply by naming it in the JSON body.
//
// orgWide reports whether the credential is an organization-wide key (project_api_keys.project_id
// IS NULL) rather than a project-scoped one. It must be recorded here rather than inferred later:
// once an org-wide key resolves a target project from X-Project-Key, the resulting identity is
// indistinguishable from a project-scoped one, and the two have different rules about a body that
// names a project.
func WithIdentity(ctx context.Context, projectKey, projectID, orgID, apiKeyHash string, rateLimitRPM int, orgWide bool) context.Context {
	ctx = context.WithValue(ctx, ctxKeyOrgWide, orgWide)
	ctx = context.WithValue(ctx, ctxKeyProjectKey, projectKey)
	ctx = context.WithValue(ctx, ctxKeyProjectID, projectID)
	ctx = context.WithValue(ctx, ctxKeyOrganizationID, orgID)
	ctx = context.WithValue(ctx, ctxKeyAPIKeyHash, apiKeyHash)
	ctx = context.WithValue(ctx, ctxKeyRateLimitRPM, rateLimitRPM)
	return ctx
}

// ProjectKeyFromContext returns the authenticated project name and whether one was present.
func ProjectKeyFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyProjectKey).(string)
	return v, ok && v != ""
}

// ProjectIDFromContext returns the authenticated project UUID and whether one was present.
// Empty for organization-wide keys, which resolve by project name instead.
func ProjectIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyProjectID).(string)
	return v, ok && v != ""
}

// IsOrgWideKey reports whether the authenticated credential is an organization-wide key.
func IsOrgWideKey(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyOrgWide).(bool)
	return v
}

// OrganizationIDFromContext returns the authenticated organization UUID.
func OrganizationIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyOrganizationID).(string)
	return v, ok && v != ""
}

// APIKeyHashFromContext returns the sha256 hash of the presented API key.
func APIKeyHashFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyAPIKeyHash).(string)
	return v, ok && v != ""
}

// RateLimitRPMFromContext returns the per-key rate limit.
func RateLimitRPMFromContext(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(ctxKeyRateLimitRPM).(int)
	return v, ok
}
