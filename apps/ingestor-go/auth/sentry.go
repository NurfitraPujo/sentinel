package auth

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
)

var (
	ErrSentryKeyMissing    = errors.New("Missing API key")
	ErrSentryAuthMalformed = errors.New("Invalid X-Sentry-Auth header")
)

// ScopeRelay is the key scope only core's mobile relay holds. It is the sole credential that makes the ingestor
// believe the relay identity headers and a relayed client IP (sentinel-relay-key-trust §4.1, K1).
const ScopeRelay = "relay"

// ScopesSentryCompat guards the Sentry-compatible routes: project-scoped ingest and relay keys only (contract D1;
// sentinel-relay-key-trust K2). ScopesIngest deliberately does not list relay, so /ingest refuses a relay key.
var ScopesSentryCompat = ScopeSet{"ingest", ScopeRelay}

// SentryKey returns the API key a Sentry-compatible request presents. The FIRST source that is present is the ONLY
// source consulted; a present-but-unusable source is an error, never a fall-through.
//
// Order: X-Sentry-Auth header (K1), the sentry_key query parameter (K2), X-API-Key (K3).
func SentryKey(r *http.Request) (string, error) {
	if h := r.Header.Get("X-Sentry-Auth"); strings.TrimSpace(h) != "" {
		return ParseSentryAuth(h)
	}
	if q := r.URL.Query(); q.Has("sentry_key") {
		k := strings.TrimSpace(q.Get("sentry_key"))
		if k == "" {
			return "", ErrSentryKeyMissing
		}
		return k, nil
	}
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k, nil
	}
	return "", ErrSentryKeyMissing
}

// ParseSentryAuth extracts sentry_key from an X-Sentry-Auth value. It is tolerant by design: the "Sentry" word is
// case-insensitive, parts are comma separated, names are case-insensitive, one pair of surrounding quotes is
// removed, and the first non-empty sentry_key wins. Every other parameter is ignored.
func ParseSentryAuth(header string) (string, error) {
	v := strings.TrimSpace(header)
	i := strings.IndexAny(v, " \t")
	if i < 0 || !strings.EqualFold(v[:i], "Sentry") {
		return "", ErrSentryAuthMalformed
	}
	for _, part := range strings.Split(v[i:], ",") {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(part[:eq]), "sentry_key") {
			continue
		}
		val := strings.TrimSpace(part[eq+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		if val != "" {
			return val, nil
		}
	}
	return "", ErrSentryAuthMalformed
}

var errSentryProjectID = errors.New("invalid project uuid")

// SentryProjectID derives the numeric Sentry project id of a Sentinel project: the decimal value of the first 13
// hex digits of its UUID, dashes removed. 13 hex digits are 52 bits, below 2^53, so every JS runtime holds the
// value exactly. It is derived, never stored (contract D2).
func SentryProjectID(projectUUID string) (string, error) {
	s := projectUUID
	if len(s) == 36 {
		if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			return "", errSentryProjectID
		}
		s = s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]
	}
	if len(s) != 32 {
		return "", errSentryProjectID
	}
	s = strings.ToLower(s)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return "", errSentryProjectID
		}
	}
	n, err := strconv.ParseUint(s[:13], 16, 64)
	if err != nil {
		return "", errSentryProjectID
	}
	return strconv.FormatUint(n, 10), nil
}

// SentryMiddleware authenticates a Sentry-compatible request. It never consults X-Project-Key and never touches
// the database beyond the key lookup Middleware uses.
func (a *APIKeyAuthenticator) SentryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, err := SentryKey(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		data, hash, err := a.resolveKey(r.Context(), key)
		if err != nil {
			http.Error(w, "Invalid API key", http.StatusUnauthorized)
			return
		}
		if !ScopesSentryCompat.Allows(data.Scope) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if data.ProjectID == nil || data.ProjectName == "" {
			http.Error(w, "Forbidden: a Sentry-compatible key must be project-scoped", http.StatusForbidden)
			return
		}
		want, err := SentryProjectID(*data.ProjectID)
		if err != nil || want != r.PathValue("project_id") {
			http.Error(w, "Forbidden: project id does not match the key's project", http.StatusForbidden)
			return
		}
		ctx := WithIdentity(r.Context(), data.ProjectName, *data.ProjectID, data.OrganizationID, hash, data.RateLimitRPM, false)
		ctx = WithScope(ctx, data.Scope)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
