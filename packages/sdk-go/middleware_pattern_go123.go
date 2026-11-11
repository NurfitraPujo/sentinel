//go:build go1.23

package sentinel

import "net/http"

// transactionFromRequest reads r.Pattern (Go 1.23+, T15), non-empty only
// when ServeMux routed the request with the Go 1.22+ matcher
// (httpmuxgo121=0, which defaults from the main module's go line, T30).
// Falls back to the path otherwise (spec §6.3).
func transactionFromRequest(r *http.Request) string {
	if r.Pattern != "" {
		return r.Pattern
	}
	return r.URL.Path
}
