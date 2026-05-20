package middleware

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/redis"
	libredis "github.com/redis/go-redis/v9"
)

var strictMode = os.Getenv("RATELIMIT_STRICT_MODE") == "true"

type RateLimiter struct {
	client *libredis.Client
	rate   int
	window time.Duration
}

func NewRateLimiter(client *libredis.Client, rate int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		client: client,
		rate:   rate,
		window: window,
	}
}

func (rl *RateLimiter) Allow(ctx context.Context, key string) bool {
	if rl.client == nil {
		if strictMode {
			return false
		}
		return true
	}

	redisKey := redis.GetWindowKey("ratelimit", key, rl.window)

	count, err := rl.client.Incr(ctx, redisKey).Result()
	if err != nil {
		if strictMode {
			return false
		}
		return true
	}

	if count == 1 {
		rl.client.Expire(ctx, redisKey, rl.window*2)
	}

	return count <= int64(rl.rate)
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		if !rl.Allow(r.Context(), apiKey) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		next.ServeHTTP(w, r)
	})
}
