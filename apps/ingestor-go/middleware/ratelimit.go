package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/NurfitraPujo/sentinel/packages/shared-go/redis"
	libredis "github.com/redis/go-redis/v9"
)

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
		return true // Fallback if Redis is not configured
	}

	redisKey := redis.GetWindowKey("ratelimit", key, rl.window)

	count, err := rl.client.Incr(ctx, redisKey).Result()
	if err != nil {
		return true // Fail open
	}

	if count == 1 {
		rl.client.Expire(ctx, redisKey, rl.window*2) // Keep key longer than window to be safe
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
