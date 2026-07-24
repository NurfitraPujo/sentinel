package unit

import (
	"context"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRateLimiterNilClient covers the fail-open nil-client behavior. The
// strictMode package variable is initialized before tests run, so the strict
// branch requires a separate test binary started with RATELIMIT_STRICT_MODE=true.
func TestRateLimiterNilClient(t *testing.T) {
	ratelimiter := middleware.NewRateLimiter(nil, 1, time.Minute)
	require.NotNil(t, ratelimiter)

	t.Run("allows with non-nil context", func(t *testing.T) {
		assert.True(t, ratelimiter.Allow(context.Background(), "api-key"))
	})

	t.Run("accepts any key without Redis", func(t *testing.T) {
		for _, key := range []string{"", "api-key", "another-key"} {
			assert.True(t, ratelimiter.Allow(context.Background(), key), "key %q", key)
		}
	})
}
