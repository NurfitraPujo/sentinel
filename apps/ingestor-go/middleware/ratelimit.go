package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	libredis "github.com/redis/go-redis/v9"
)

// slidingWindowScript performs the entire sliding-window rate-limit decision as a single atomic
// operation on the Redis server: trim the window, count what's left, decide, and (if allowed) record
// the request — all inside one EVAL. Redis executes a Lua script to completion before processing any
// other command, so there is no gap in which two concurrent requests can both observe the pre-write
// count (docs/plans/E2E_RECOVERY_PLAN.md P3-3 / VERIFIED_STATE.md S10).
//
// The previous implementation issued four separate round-trips (ZREMRANGEBYSCORE, ZCARD, decide,
// ZADD/EXPIRE). Under real concurrency every one of those requests could read the same ZCARD result
// before any of their ZADDs landed, so the effective limit collapsed to "however many requests the
// server could accept in parallel" rather than the configured RPM (measured: 200 concurrent requests
// against limit=100 accepted 101-111).
//
// KEYS[1] = the sliding-window ZSET key
// ARGV[1] = window start cutoff (exclusive lower bound to trim, as a score)
// ARGV[2] = current time, used as the new entry's score
// ARGV[3] = configured limit (requests per window)
// ARGV[4] = TTL (seconds) to set on the key so it doesn't outlive the window it tracks
// ARGV[5] = a per-request random member suffix, so two requests landing in the same nanosecond never
//
//	collide on ZADD (a duplicate member is a no-op update, not a second entry, which would silently
//	undercount)
//
// Returns {count_before_this_request, allowed} where allowed is 1 if the request was admitted (and thus
// already recorded) or 0 if it was rejected (and thus NOT recorded).
const slidingWindowScript = `
local key = KEYS[1]
local window_start = ARGV[1]
local now = ARGV[2]
local limit = tonumber(ARGV[3])
local ttl = ARGV[4]
local member_suffix = ARGV[5]

redis.call('ZREMRANGEBYSCORE', key, '-inf', window_start)
local count = redis.call('ZCARD', key)

if count >= limit then
	return {count, 0}
end

redis.call('ZADD', key, now, now .. '-' .. member_suffix)
redis.call('EXPIRE', key, ttl)
return {count + 1, 1}
`

// isStrictMode reports whether a Redis failure (unreachable at request time, or never connected —
// nil client) must fail CLOSED (reject the request) rather than fail open (let it through unlimited).
//
// This is read fresh on every call rather than cached in a package-level var at init time, for two
// reasons: it lets tests toggle the decision within a single test binary via t.Setenv, and — more
// importantly — it means the decision is always actually consulted, including on the nil-client path,
// which is exactly what regressed before (docs/plans/E2E_RECOVERY_PLAN.md P3-3): the old nil-client
// check returned before strictMode was ever read.
//
// The default is now CLOSED (fail-closed) rather than open. Fail-open must be a decision an operator
// makes on purpose, never a default an outage falls into by accident — so it now requires the explicit,
// literal opt-out RATELIMIT_STRICT_MODE=false. Any other value (unset, "true", a typo) stays strict.
func isStrictMode() bool {
	return os.Getenv("RATELIMIT_STRICT_MODE") != "false"
}

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
		// Read via auth's typed accessors. These used to be bare string keys
		// (r.Context().Value("api_key_hash")); when auth switched to a private ctxKey type, the
		// string assertions here silently stopped matching — context.Value compares the dynamic
		// type too — so this middleware passed EVERY request through and rate limiting was
		// entirely bypassed. Never read this context by string literal again.
		apiKeyHash, ok := auth.APIKeyHashFromContext(r.Context())
		if !ok || apiKeyHash == "" {
			next.ServeHTTP(w, r)
			return
		}

		rateLimitRPM, ok := auth.RateLimitRPMFromContext(r.Context())
		if !ok || rateLimitRPM <= 0 {
			rateLimitRPM = 5000 // default
		}

		// A nil client is what main.go produces when Redis was unreachable at boot and an operator
		// explicitly opted into starting anyway (RATELIMIT_ALLOW_NO_REDIS=true) — see main.go. It is no
		// longer an unconditional bypass: it now goes through the exact same strictMode decision as a
		// Redis error at request time, below, instead of short-circuiting before that decision is ever
		// read (VERIFIED_STATE.md S10).
		if rl.client == nil {
			rl.failOrPass(w, r, next, "rate limiting unavailable: no Redis client configured")
			return
		}

		now := time.Now()
		windowStart := now.Add(-time.Minute).UnixNano()
		nowUnix := now.UnixNano()
		redisKey := "ratelimit:" + apiKeyHash

		ctx := r.Context()

		suffix, err := randomSuffix()
		if err != nil {
			// crypto/rand failing is effectively "the machine is broken"; treat it the same as a Redis
			// failure rather than falling through to nowUnix-only uniqueness, which would risk silently
			// merging two concurrent requests' entries into one ZSET member.
			rl.failOrPass(w, r, next, "rate limit check failed: "+err.Error())
			return
		}

		res, err := rl.client.Eval(ctx, slidingWindowScript,
			[]string{redisKey},
			strconv.FormatInt(windowStart, 10),
			strconv.FormatInt(nowUnix, 10),
			rateLimitRPM,
			60, // TTL seconds: one window's worth, refreshed every request
			suffix,
		).Result()
		if err != nil {
			rl.failOrPass(w, r, next, "rate limit check failed: "+err.Error())
			return
		}

		countAndAllowed, ok := res.([]interface{})
		if !ok || len(countAndAllowed) != 2 {
			rl.failOrPass(w, r, next, "rate limit check failed: unexpected script result shape")
			return
		}
		countBefore, _ := toInt64(countAndAllowed[0])
		allowed, _ := toInt64(countAndAllowed[1])

		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(rateLimitRPM))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Minute).Unix(), 10))

		if allowed == 0 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		remaining := rateLimitRPM - int(countBefore) - 1
		if remaining < 0 {
			remaining = 0
		}
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))

		next.ServeHTTP(w, r)
	})
}

// failOrPass is the single place that decides what happens when Redis cannot answer the rate-limit
// question at all — nil client, or an error out of the Lua EVAL. It used to be two different
// code paths (an unconditional pass for a nil client, a strictMode-gated choice for a ZCard error);
// now there is exactly one decision, and the nil-client path can no longer skip it.
func (rl *RateLimiter) failOrPass(w http.ResponseWriter, r *http.Request, next http.Handler, reason string) {
	if isStrictMode() {
		http.Error(w, "Rate limit check failed", http.StatusInternalServerError)
		return
	}
	next.ServeHTTP(w, r)
}

// randomSuffix returns a short random hex string used to disambiguate ZSET members that would
// otherwise share the same score (nanosecond timestamp). Collisions are rare but not impossible under
// genuine concurrency, and a colliding ZADD updates a member in place rather than adding a second entry
// — which would silently undercount concurrent requests in the same instant.
func randomSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// toInt64 converts a go-redis EVAL result element (typically int64 already, but kept defensive since
// the wire protocol / test doubles can surface other numeric types) to int64.
func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}
