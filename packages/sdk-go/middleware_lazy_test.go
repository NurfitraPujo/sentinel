package sentinel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestMiddlewareScopeIsLazy proves P3.10: the middleware eagerly allocates
// only the scope and a buffer header; the ring is built by the first
// AddBreadcrumb and the snapshot by the first capture, and a second capture
// reuses the same snapshot (spec §6.2, "why lazy").
func TestMiddlewareScopeIsLazy(t *testing.T) {
	var (
		snapshotBeforeAnything *RequestInfo
		ringBeforeAnything     bool
		snapshotAfterCrumb     *RequestInfo
		ringAfterCrumb         bool
		snapshotAfterCapture1  *RequestInfo
		snapshotAfterCapture2  *RequestInfo
	)

	handler := NewHTTPMiddleware(MiddlewareOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := requestScopeFromContext(r.Context())
		if scope == nil {
			t.Fatal("no request scope on context")
		}
		snapshotBeforeAnything = scope.snapshot
		ringBeforeAnything = ringOf(r.Context())

		AddBreadcrumb(r.Context(), Breadcrumb{Message: "x"})
		snapshotAfterCrumb = scope.snapshot
		ringAfterCrumb = ringOf(r.Context())

		scope.ensureBuilt()
		snapshotAfterCapture1 = scope.snapshot
		scope.ensureBuilt()
		snapshotAfterCapture2 = scope.snapshot

		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/items/42", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if snapshotBeforeAnything != nil {
		t.Errorf("snapshot built before any capture: %v", snapshotBeforeAnything)
	}
	if ringBeforeAnything {
		t.Error("ring allocated before any AddBreadcrumb")
	}
	if snapshotAfterCrumb != nil {
		t.Errorf("snapshot built by AddBreadcrumb alone: %v", snapshotAfterCrumb)
	}
	if !ringAfterCrumb {
		t.Error("ring not allocated after AddBreadcrumb")
	}
	if snapshotAfterCapture1 == nil {
		t.Fatal("snapshot not built after first capture")
	}
	if snapshotAfterCapture1 != snapshotAfterCapture2 {
		t.Error("snapshot rebuilt on second capture: scope.once did not dedupe")
	}
}

// TestMiddlewareScopeUntouchedWhenNothingHappens proves the "leaves both nil"
// half of P3.10: a request that never adds a crumb or captures builds
// neither.
func TestMiddlewareScopeUntouchedWhenNothingHappens(t *testing.T) {
	var snapshot *RequestInfo
	var ring bool

	handler := NewHTTPMiddleware(MiddlewareOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := requestScopeFromContext(r.Context())
		snapshot = scope.snapshot
		ring = ringOf(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/items/42", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if snapshot != nil {
		t.Errorf("snapshot built without a capture: %v", snapshot)
	}
	if ring {
		t.Error("ring allocated without an AddBreadcrumb")
	}
}

// ringOf reports whether the buffer's ring has been allocated yet (nil
// until the first AddBreadcrumb). Returns a bare bool, not a slice, so a
// nil []Breadcrumb wrapped in interface{} cannot masquerade as non-nil.
func ringOf(ctx context.Context) bool {
	buf := breadcrumbBufferFromContext(ctx)
	if buf == nil {
		return false
	}
	buf.mu.Lock()
	defer buf.mu.Unlock()
	return buf.ring != nil
}
