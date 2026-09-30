package sentrycompat

import (
	"net/http"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
)

// Chain wraps h in the compat routes' client-address, auth and rate-limit layers, in that order (outermost
// first); relayClientIP sits after auth because it needs the key's scope. main.go adds observeMiddleware and otelhttp around it, as it does for /ingest. It lives here, not in
// main.go, so tests/unit can prove the wiring.
func Chain(resolver *clientip.Resolver, a *auth.APIKeyAuthenticator, rl *middleware.RateLimiter, h http.Handler) http.Handler {
	return clientip.Middleware(resolver, a.SentryMiddleware(relayClientIP(rl.SentryMiddleware(h))))
}

// relayClientIP replaces the resolved caller IP with the relayed one (rightmost public X-Forwarded-For entry) for
// a relay-scoped key, and only for one. For any other key Info is exactly what clientip.Middleware resolved. When
// the relayed entry is missing or not public, no client IP is recorded: the union resolver's answer is discarded,
// because a relay key's peer is core's node, never the phone.
func relayClientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if scope, _ := auth.ScopeFromContext(r.Context()); scope == auth.ScopeRelay {
			info, _ := clientip.RequestInfoFromContext(r.Context())
			info.ClientIP, info.HasClientIP = clientip.RelayedClientIP(r)
			r = r.WithContext(clientip.WithRequestInfo(r.Context(), info))
		}
		next.ServeHTTP(w, r)
	})
}
