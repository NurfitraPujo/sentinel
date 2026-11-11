package sentinel

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
)

const requestScopeContextKey contextKey = "sentinel_request_scope"

// MiddlewareOptions configures NewHTTPMiddleware.
type MiddlewareOptions struct {
	// CaptureClientIP sets user.ip_address from the connection's remote
	// address (r.RemoteAddr). Client-IP headers (X-Forwarded-For,
	// Forwarded, X-Real-Ip, any name containing forwarded, client-ip,
	// real-ip, connection-ip, originating-ip, remote-ip, remote-addr, ...,
	// and any header whose value is only IP addresses) are never read, and
	// never recorded in request.headers whatever this is set to. Off by
	// default.
	CaptureClientIP bool
	// MaxBreadcrumbs is the per-request buffer size; 0 -> DefaultMaxBreadcrumbs.
	MaxBreadcrumbs int
}

// requestScope is the per-request state HTTPMiddleware allocates eagerly
// but fills lazily, on the first capture that reads it (spec §6.2).
type requestScope struct {
	req  *http.Request
	opts MiddlewareOptions

	once        sync.Once
	snapshot    *RequestInfo
	transaction string
	clientIP    string
}

func (s *requestScope) ensureBuilt() {
	s.once.Do(func() {
		s.snapshot = buildRequestSnapshot(s.req)
		s.transaction = transactionFromRequest(s.req)
		if s.opts.CaptureClientIP {
			if host, _, err := net.SplitHostPort(s.req.RemoteAddr); err == nil && host != "" {
				s.clientIP = host
			}
		}
	})
}

func requestScopeFromContext(ctx context.Context) *requestScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(requestScopeContextKey).(*requestScope)
	return scope
}

// NewHTTPMiddleware returns HTTP middleware that, per request, allocates a
// request scope and a breadcrumb buffer eagerly, and defers building the
// request snapshot, transaction and client IP to the first capture that
// reads them (spec §6.2). A nested middleware call reuses the outer scope
// and buffer, adding only its own recover.
func NewHTTPMiddleware(opts MiddlewareOptions) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requestScopeFromContext(r.Context()) != nil {
				serveWithRecover(w, r, next)
				return
			}

			scope := &requestScope{opts: opts}
			ctx := context.WithValue(r.Context(), requestScopeContextKey, scope)
			ctx = WithBreadcrumbBuffer(ctx, opts.MaxBreadcrumbs)
			r2 := r.WithContext(ctx)
			scope.req = r2

			serveWithRecover(w, r2, next)
		})
	}
}

// HTTPMiddleware is NewHTTPMiddleware(MiddlewareOptions{})(next).
func HTTPMiddleware(next http.Handler) http.Handler {
	return NewHTTPMiddleware(MiddlewareOptions{})(next)
}

// serveWithRecover is the recovered-panic path (spec §6.5): the capture
// context carries level: fatal, so the event carries the request,
// transaction and breadcrumbs. It needs an Init'd default client to reach
// the program's project; without one it is answered 500 and never reaches
// any project, exactly as v0.3.x's unconfigured default client did.
func serveWithRecover(w http.ResponseWriter, r *http.Request, next http.Handler) {
	defer func() {
		if rec := recover(); rec != nil {
			err, ok := rec.(error)
			if !ok {
				err = fmt.Errorf("%v", rec)
			}

			ctx := WithLevel(r.Context(), LevelFatal)
			CaptureErrorContext(ctx, err, map[string]interface{}{
				"panic":            true,
				"panic_stacktrace": string(debug.Stack()),
			})

			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		}
	}()

	next.ServeHTTP(w, r)
}

// buildRequestSnapshot builds the RequestInfo the request scope's first
// capture takes (spec §6.2). It reads r as the handler chain holds it at
// that moment; net/http forbids a handler to modify the *http.Request it
// was given, so a capture racing an inner middleware's own header writes
// before this snapshot is built is the caller's race (spec §6.2, §6.3).
func buildRequestSnapshot(r *http.Request) *RequestInfo {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	url := r.URL.EscapedPath()
	if r.Host != "" {
		url = scheme + "://" + r.Host + url
	}

	return &RequestInfo{
		Method:      r.Method,
		URL:         url,
		QueryString: filterQueryString(r.URL.RawQuery),
		Headers:     filterRequestHeaders(r),
	}
}

func deepCopyRequestInfo(r *RequestInfo) *RequestInfo {
	if r == nil {
		return nil
	}
	cp := *r
	if r.Headers != nil {
		cp.Headers = make(map[string]string, len(r.Headers))
		for k, v := range r.Headers {
			cp.Headers[k] = v
		}
	}
	return &cp
}

// urlValuedHeaders get the query_string rule applied to their own query,
// their fragment and userinfo stripped (spec §6.2).
var urlValuedHeaders = map[string]bool{
	"Referer":          true,
	"Location":         true,
	"Content-Location": true,
	"X-Original-Url":   true,
	"X-Original-Uri":   true,
	"X-Rewrite-Url":    true,
}

func filterRequestHeaders(r *http.Request) map[string]string {
	out := make(map[string]string, len(r.Header)+1)
	for name, values := range r.Header {
		canonical := http.CanonicalHeaderKey(name)
		lower := strings.ToLower(canonical)

		if isCredentialHeaderName(lower) {
			continue
		}
		if isClientIPHeader(lower, values) {
			continue
		}
		if isSensitiveKey(lower) {
			continue
		}

		joined := strings.Join(values, ", ")

		if urlValuedHeaders[canonical] {
			filtered, ok := filterURLValuedHeader(joined)
			if !ok {
				continue // does not parse as a URL: drop the whole header
			}
			joined = filtered
		} else if canonical == "Link" {
			continue // a list of <URI>; param entries, dropped whole rather than parsed
		}

		out[canonical] = joined
	}
	if r.Host != "" {
		out["Host"] = r.Host
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// isCredentialHeaderName reports whether lower (already lower-cased) names
// a credential header (spec §6.2).
func isCredentialHeaderName(lower string) bool {
	switch lower {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key":
		return true
	}
	for _, sub := range []string{"jwt", "assertion", "oidc", "principal", "session"} {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return strings.HasSuffix(lower, "-key")
}

// isClientIPHeader reports whether lower (already lower-cased) names, or
// values hold, a client IP address (spec §6.2, §6.4).
func isClientIPHeader(lower string, values []string) bool {
	switch lower {
	case "x-forwarded-for", "forwarded", "x-real-ip", "true-client-ip",
		"cf-connecting-ip", "x-client-ip", "x-cluster-client-ip":
		return true
	}
	for _, sub := range []string{
		"forwarded", "client-ip", "clientip", "real-ip", "connecting-ip",
		"connection-ip", "originating-ip", "remote-ip", "remote-addr",
		"external-address", "user-ip", "socketip",
	} {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return valuesAreOnlyIPs(values)
}

func valuesAreOnlyIPs(values []string) bool {
	joined := strings.Join(values, ", ")
	tokens := strings.Split(joined, ",")
	if len(tokens) == 0 {
		return false
	}
	for _, tok := range tokens {
		tok = strings.TrimSpace(tok)
		if host, _, err := net.SplitHostPort(tok); err == nil {
			tok = host
		}
		tok = strings.TrimPrefix(tok, "[")
		tok = strings.TrimSuffix(tok, "]")
		if tok == "" || net.ParseIP(tok) == nil {
			return false
		}
	}
	return true
}

// filterQueryString applies the query_string rule to raw: a parameter whose
// unescaped name matches the SDK PII list keeps its name and gets the value
// [FILTERED]; order and every other byte preserved. The query is split on
// "&" and ";" (spec §6.2).
func filterQueryString(raw string) string {
	if raw == "" {
		return raw
	}
	var b strings.Builder
	i := 0
	for i < len(raw) {
		j := i
		for j < len(raw) && raw[j] != '&' && raw[j] != ';' {
			j++
		}
		b.WriteString(filterQueryParam(raw[i:j]))
		if j < len(raw) {
			b.WriteByte(raw[j])
		}
		i = j + 1
	}
	return b.String()
}

func filterQueryParam(part string) string {
	eq := strings.IndexByte(part, '=')
	name := part
	hasValue := eq >= 0
	if hasValue {
		name = part[:eq]
	}
	matchName := name
	if unescaped, err := url.QueryUnescape(name); err == nil {
		matchName = unescaped
	}
	if !isSensitiveKey(matchName) {
		return part
	}
	if !hasValue {
		return part
	}
	return name + "=[FILTERED]"
}

// filterURLValuedHeader parses value as a URL (absolute or relative),
// applies the query_string rule to its query, strips its fragment and
// userinfo, and returns it rewritten. ok is false when value does not
// parse, in which case the whole header must be dropped (spec §6.2).
func filterURLValuedHeader(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil {
		return "", false
	}
	u.RawQuery = filterQueryString(u.RawQuery)
	u.Fragment = ""
	u.RawFragment = ""
	u.User = nil
	return u.String(), true
}
