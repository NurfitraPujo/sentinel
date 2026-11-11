package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	sharedredis "github.com/NurfitraPujo/sentinel/packages/shared-go/redis"
	tc "github.com/NurfitraPujo/sentinel/tests/integration/testcontainers"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func integrationRedisClient(t *testing.T) *redis.Client {
	t.Helper()

	env := tc.Setup(t, tc.WithResources(tc.RedisResource))
	require.NotEmpty(t, env.RedisConfig.Addr, "redis testcontainer must be initialized")

	client, err := sharedredis.NewClient(context.Background(), sharedredis.Config{
		Addr:     env.RedisConfig.Addr,
		Password: env.RedisConfig.Password,
		DB:       env.RedisConfig.DB,
	})
	require.NoError(t, err, "connect to redis testcontainer")
	t.Cleanup(func() { _ = client.Close() })
	return client
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

func TestRateLimiterMiddleware_AllowsUnderThreshold(t *testing.T) {
	client := integrationRedisClient(t)
	rl := middleware.NewRateLimiter(client)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ingest", nil)
	ctx := auth.WithIdentity(req.Context(), "test-project", "", "test-org", "hash_test_123", 5000, false)

	rl.Middleware(okHandler).ServeHTTP(rec, req.WithContext(ctx))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.Body.String())
}

func TestRateLimiterMiddleware_ReturnsTooManyRequests(t *testing.T) {
	client := integrationRedisClient(t)

	rl := middleware.NewRateLimiter(client)
	handler := rl.Middleware(okHandler)

	apiKeyHash := "hash_test_limit_exceeded"

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/ingest", nil)
		ctx := auth.WithIdentity(req.Context(), "test-project", "", "test-org", apiKeyHash, 1, false)

		handler.ServeHTTP(rec, req.WithContext(ctx))

		if i == 0 {
			require.Equal(t, http.StatusOK, rec.Code)
		} else {
			assert.Equal(t, http.StatusTooManyRequests, rec.Code)
			assert.Equal(t, "60", rec.Header().Get("Retry-After"))
		}
	}
}
