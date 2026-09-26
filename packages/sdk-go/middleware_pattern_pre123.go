//go:build !go1.23

package sentinel

import "net/http"

// transactionFromRequest on Go 1.21/1.22 has no r.Pattern to read (T15,
// T30): net/http's ServeMux does not expose it before 1.23, so the path is
// the only transaction available (spec §6.3).
func transactionFromRequest(r *http.Request) string {
	return r.URL.Path
}
