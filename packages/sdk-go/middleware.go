package sentinel

import (
	"fmt"
	"net/http"
	"runtime/debug"
)

func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = WithTag(ctx, "http_method", r.Method)
		ctx = WithTag(ctx, "http_url", r.URL.String())
		ctx = WithTag(ctx, "http_user_agent", r.UserAgent())
		ctx = WithTag(ctx, "http_remote_addr", r.RemoteAddr)

		r = r.WithContext(ctx)

		defer func() {
			if rec := recover(); rec != nil {
				err, ok := rec.(error)
				if !ok {
					err = fmt.Errorf("%v", rec)
				}

				CaptureErrorContext(r.Context(), err, map[string]interface{}{
					"panic":            true,
					"panic_stacktrace": string(debug.Stack()),
				})

				http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
		}()

		next.ServeHTTP(w, r)
	})
}
