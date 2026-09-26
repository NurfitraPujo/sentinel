package sentinel

import (
	"context"
	"sync"
	"time"
)

const (
	// DefaultMaxBreadcrumbs is Config.MaxBreadcrumbs' default when unset (also core's, T24).
	DefaultMaxBreadcrumbs = 50
	// MaxBreadcrumbsLimit is the ingestor's cap (apps/ingestor-go/validation/bounds.go: MaxBreadcrumbs).
	MaxBreadcrumbsLimit = 100
)

const breadcrumbBufferContextKey contextKey = "sentinel_breadcrumb_buffer"

// breadcrumbBuffer is one heap object shared by every context derived from
// the WithBreadcrumbBuffer call that created it: a ring of max slots behind
// a mutex. The ring itself is allocated lazily, by the first AddBreadcrumb,
// so a request that never adds a crumb pays nothing per slot (spec §5.2).
type breadcrumbBuffer struct {
	mu    sync.Mutex
	max   int
	ring  []Breadcrumb
	start int
	count int
}

func (b *breadcrumbBuffer) add(crumb Breadcrumb) {
	if crumb.Timestamp.IsZero() {
		crumb.Timestamp = time.Now().UTC()
	}
	crumb.Level = NormalizeLevel(crumb.Level)
	if len(crumb.Data) == 0 {
		crumb.Data = nil
	} else {
		data := make(map[string]interface{}, len(crumb.Data))
		for k, v := range crumb.Data {
			data[k] = v
		}
		crumb.Data = data
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ring == nil {
		b.ring = make([]Breadcrumb, b.max)
	}
	if b.count < b.max {
		b.ring[(b.start+b.count)%b.max] = crumb
		b.count++
	} else if b.max > 0 {
		b.ring[b.start] = crumb
		b.start = (b.start + 1) % b.max
	}
}

func (b *breadcrumbBuffer) snapshot() []Breadcrumb {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ring == nil || b.count == 0 {
		return nil
	}
	out := make([]Breadcrumb, b.count)
	for i := 0; i < b.count; i++ {
		out[i] = b.ring[(b.start+i)%b.max]
		if out[i].Data != nil {
			data := make(map[string]interface{}, len(out[i].Data))
			for k, v := range out[i].Data {
				data[k] = v
			}
			out[i].Data = data
		}
	}
	return out
}

func clampMaxBreadcrumbs(max int) int {
	if max <= 0 {
		return DefaultMaxBreadcrumbs
	}
	if max > MaxBreadcrumbsLimit {
		return MaxBreadcrumbsLimit
	}
	return max
}

// WithBreadcrumbBuffer returns a context carrying a new, empty buffer
// holding at most max breadcrumbs (max <= 0 -> DefaultMaxBreadcrumbs, max >
// MaxBreadcrumbsLimit -> MaxBreadcrumbsLimit). It shadows any buffer ctx
// already carries.
func WithBreadcrumbBuffer(ctx context.Context, max int) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	buf := &breadcrumbBuffer{max: clampMaxBreadcrumbs(max)}
	return context.WithValue(ctx, breadcrumbBufferContextKey, buf)
}

func breadcrumbBufferFromContext(ctx context.Context) *breadcrumbBuffer {
	if ctx == nil {
		return nil
	}
	buf, _ := ctx.Value(breadcrumbBufferContextKey).(*breadcrumbBuffer)
	return buf
}

// AddBreadcrumb appends b to ctx's buffer, dropping the oldest when full.
// Without a buffer (or with a nil ctx) it does nothing. b.Data is
// shallow-copied at the top level; nested maps or slices in it are shared
// by the buffer and by every event that snapshots it and must not be
// modified after the call.
func AddBreadcrumb(ctx context.Context, b Breadcrumb) {
	buf := breadcrumbBufferFromContext(ctx)
	if buf == nil {
		return
	}
	buf.add(b)
}

// BreadcrumbsFromContext returns a copy of ctx's buffer, oldest first, or
// nil when ctx has none. Each returned crumb has its own top-level copy of
// Data, so the caller (and BeforeSend) may add, replace or delete Data keys
// without touching the buffer or another event's crumbs; values nested
// inside Data are shared and read-only.
func BreadcrumbsFromContext(ctx context.Context) []Breadcrumb {
	buf := breadcrumbBufferFromContext(ctx)
	if buf == nil {
		return nil
	}
	return buf.snapshot()
}
