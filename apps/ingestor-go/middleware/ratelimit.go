package middleware

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	libredis "github.com/redis/go-redis/v9"
)

var strictMode = os.Getenv("RATELIMIT_STRICT_MODE") == "true"

type RateLimiter struct {
	client *libredis.Client
}

func NewRateLimiter(client *libredis.Client) *RateLimiter {
	return &RateLimiter{
		client: client,
	}
}

func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKeyHash, ok := r.Context().Value("api_key_hash").(string)
		if !ok || apiKeyHash == "" {
			next.ServeHTTP(w, r)
			return
		}

		rateLimitRPM, ok := r.Context().Value("rate_limit_rpm").(int)
		if !ok || rateLimitRPM <= 0 {
			rateLimitRPM = 5000 // default
		}

		if rl.client == nil {
			next.ServeHTTP(w, r)
			return
		}

		// Sliding window using ZSET
		now := time.Now()
		windowStart := now.Add(-time.Minute).UnixNano()
		nowUnix := now.UnixNano()
		redisKey := "ratelimit:" + apiKeyHash

		ctx := r.Context()
		
		// Remove old entries
		rl.client.ZRemRangeByScore(ctx, redisKey, "0", fmt.Sprintf("%d", windowStart))
		
		// Count current entries
		count, err := rl.client.ZCard(ctx, redisKey).Result()
		if err != nil {
			if strictMode {
				http.Error(w, "Rate limit check failed", http.StatusInternalServerError)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rateLimitRPM))
		remaining := rateLimitRPM - int(count) - 1
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Minute).Unix(), 10))

		if count >= int64(rateLimitRPM) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		// Add current request
		rl.client.ZAdd(ctx, redisKey, libredis.Z{
			Score:  float64(nowUnix),
			Member: strconv.FormatInt(nowUnix, 10),
		})
		rl.client.Expire(ctx, redisKey, time.Minute)

		next.ServeHTTP(w, r)
	})
}
