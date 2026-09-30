package sentrycompat

import (
	"net/http"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/clientip"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
)

// Chain wraps h in the compat routes' client-address, auth and rate-limit layers, in that order (outermost
// first). main.go adds observeMiddleware and otelhttp around it, as it does for /ingest. It lives here, not in
// main.go, so tests/unit can prove the wiring.
func Chain(resolver *clientip.Resolver, a *auth.APIKeyAuthenticator, rl *middleware.RateLimiter, h http.Handler) http.Handler {
	return clientip.Middleware(resolver, a.SentryMiddleware(rl.SentryMiddleware(h)))
}
