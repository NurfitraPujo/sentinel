package e2e

// This file drives U19-U20 of the P7 use-case matrix (docs/plans/E2E_RECOVERY_PLAN.md, "## P7 — The E2E
// proof harness") against the real ingestor and defect S10 (docs/memory/VERIFIED_STATE.md).
//
// Both rows assert behaviour the product does NOT currently implement, and both tests below are
// expected to FAIL. That failure is the deliverable: it turns "S10 is open" from a code-reading claim
// into a measured fact. Nothing here weakens an assertion, adds a skip, or retargets an expectation so
// the suite goes green — see the package comment in main_test.go on why a skip is treated as a lie.
//
// middleware/ratelimit.go does four unpipelined Redis round-trips per request — ZRemRangeByScore,
// ZCard, decide, ZAdd/Expire — so concurrent requests can all read the same count before any of them
// writes (U19). main.go:103 is `redisClient, _ := redis.NewClient(ctx, redisCfg)`, discarding the
// connection error, and ratelimit.go:44 is an unconditional `if rl.client == nil { next.ServeHTTP(...);
// return }` — a nil Redis client silently disables rate limiting for every request, regardless of
// RATELIMIT_STRICT_MODE (U20).
//
// R1 (VERIFIED_STATE.md) is worth remembering here: this exact middleware once silently bypassed rate
// limiting for 100% of requests because of a context-key type mismatch, and its own tests passed the
// whole time. If either test below sees ZERO limiting rather than sloppy limiting, that is the first
// thing to rule out — not assumed away.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/auth"
	"github.com/NurfitraPujo/sentinel/apps/ingestor-go/middleware"
	libredis "github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------------------------------------------
// U19 — concurrent requests must not exceed the configured limit
// ---------------------------------------------------------------------------------------------------

// TestU19_SequentialRequestsCutOverExactlyAtLimit proves the half of U19 that already works: fired one
// at a time, the limiter's cutover is exact. This must stay green — it is what R1's fix actually
// delivered, and a regression here would mean the concurrent failure below has a second, unrelated
// cause instead of the one this file documents.
func TestU19_SequentialRequestsCutOverExactlyAtLimit(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	const limit = 5
	const fire = limit + 3 // a few past the cutover, so 429 is observed to persist, not just start

	secret := newSecret(t)
	f.addKey(keySpec{Name: "u19-sequential", Secret: secret, ProjectScoped: true, RateLimitRPM: limit})

	results := ratelimitFireSequential(t, f, secret, fire)

	var accepted, limited int
	for i, res := range results {
		wantAccepted := i < limit
		switch {
		case wantAccepted && res.Status != http.StatusAccepted:
			t.Errorf("sequential request %d/%d (want 202, limit=%d): got %d\n  body: %s",
				i+1, fire, limit, res.Status, res.Body)
		case !wantAccepted && res.Status != http.StatusTooManyRequests:
			t.Errorf("sequential request %d/%d (want 429, limit=%d): got %d\n  body: %s",
				i+1, fire, limit, res.Status, res.Body)
		}
		if res.Status == http.StatusAccepted {
			accepted++
		}
		if res.Status == http.StatusTooManyRequests {
			limited++
			if res.Header.Get("Retry-After") == "" {
				t.Errorf("sequential request %d/%d: 429 response is missing a Retry-After header", i+1, fire)
			}
		}
	}

	t.Logf("U19 sequential: limit=%d, fired=%d, accepted(202)=%d, limited(429)=%d", limit, fire, accepted, limited)

	// The pipeline hop is real; confirm the accepted requests actually landed, not just that the HTTP
	// layer said 202.
	f.waitForOccurrences(accepted)
}

// TestU19_ConcurrentRequestsExceedLimit is the row's actual assertion, and is expected to FAIL.
//
// docs/plans/E2E_RECOVERY_PLAN.md P3-3's acceptance criterion is explicit: "an integration test firing
// 200 concurrent requests against a limit of 100 observes ≤100 accepted (currently it will observe
// ~200)." middleware/ratelimit.go's ZRemRangeByScore -> ZCard -> decide -> ZAdd sequence is four
// separate Redis round-trips with no pipelining or Lua atomicity, so under genuine concurrency every
// request can read the ZCard count before any request's ZAdd has landed — the effective limit collapses
// to "however many requests the server can accept in parallel," not the configured RPM.
func TestU19_ConcurrentRequestsExceedLimit(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	const limit = 100
	const fire = 200

	secret := newSecret(t)
	f.addKey(keySpec{Name: "u19-concurrent", Secret: secret, ProjectScoped: true, RateLimitRPM: limit})

	results := ratelimitFireConcurrent(t, f, secret, fire)

	var accepted, limited, other, missingRetryAfter int
	for i, res := range results {
		switch res.Status {
		case http.StatusAccepted:
			accepted++
		case http.StatusTooManyRequests:
			limited++
			if res.Header.Get("Retry-After") == "" {
				missingRetryAfter++
				t.Errorf("concurrent request %d: 429 response is missing a Retry-After header", i)
			}
		default:
			other++
			t.Errorf("concurrent request %d: unexpected status %d\n  body: %s", i, res.Status, res.Body)
		}
	}
	if accepted+limited+other != fire {
		t.Fatalf("test bug: tallied %d responses but fired %d requests", accepted+limited+other, fire)
	}

	t.Logf("U19 concurrent: limit=%d, fired=%d, accepted(202)=%d, limited(429)=%d, other=%d, 429s missing Retry-After=%d",
		limit, fire, accepted, limited, other, missingRetryAfter)

	// Confirm the acceptances are real database writes, not merely an HTTP-layer artifact, before this
	// fixture's cleanup runs.
	f.waitForOccurrences(accepted)

	if accepted > limit {
		t.Errorf("S10 confirmed open (U19): fired %d genuinely concurrent requests against a key with "+
			"rate_limit_rpm=%d; want at most %d accepted (202), observed %d accepted and %d rejected (429). "+
			"middleware/ratelimit.go's ZRemRangeByScore -> ZCard -> decide -> ZAdd is four unpipelined Redis "+
			"round-trips, so concurrent requests read the same count before any of them writes, making the "+
			"effective limit unbounded under real concurrency.", fire, limit, limit, accepted, limited)
	}
}

// ratelimitFireSequential posts n events one at a time, waiting for each response before sending the
// next, so the limiter's decision for request i is guaranteed to see request i-1's write.
func ratelimitFireSequential(t *testing.T, f *fixture, apiKey string, n int) []ingestResult {
	t.Helper()
	results := make([]ingestResult, n)
	for i := 0; i < n; i++ {
		results[i] = f.ingest(f.newEvent(), ingestOpts{APIKey: apiKey})
	}
	return results
}

// ratelimitFireConcurrent posts n events truly concurrently: every request is built and its goroutine
// parked on a shared gate BEFORE any of them is allowed to fire, and all are released in the same
// instant. This guards against the exact test bug the assignment warns about — an http.Client whose
// transport caps concurrent connections per host would silently serialize "concurrent" requests into a
// queue of a couple of reused connections, making a broken limiter look correct. MaxConnsPerHost is
// explicitly left unlimited and MaxIdleConnsPerHost is sized past n so the transport is never the
// bottleneck being measured.
func ratelimitFireConcurrent(t *testing.T, f *fixture, apiKey string, n int) []ingestResult {
	t.Helper()

	transport := &http.Transport{
		MaxIdleConns:        n * 2,
		MaxIdleConnsPerHost: n * 2,
		MaxConnsPerHost:     0, // unlimited — do not let the client be the thing that serializes this
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	results := make([]ingestResult, n)
	var ready sync.WaitGroup
	var wg sync.WaitGroup
	start := make(chan struct{})
	ready.Add(n)
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()

			payload, err := json.Marshal(f.newEvent())
			if err != nil {
				t.Errorf("marshalling concurrent event %d: %v", i, err)
				ready.Done()
				return
			}
			req, err := http.NewRequest(http.MethodPost, cfg.IngestorURL+"/ingest", bytes.NewReader(payload))
			if err != nil {
				t.Errorf("building concurrent request %d: %v", i, err)
				ready.Done()
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", apiKey)

			ready.Done()
			<-start // every goroutine blocks here until the gate below releases them all at once

			resp, err := client.Do(req)
			if err != nil {
				t.Errorf("concurrent request %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			results[i] = ingestResult{Status: resp.StatusCode, Body: string(raw), Header: resp.Header}
		}(i)
	}

	ready.Wait() // every request is built and every goroutine is parked on <-start
	close(start) // release all n at the same instant
	wg.Wait()

	return results
}

// ---------------------------------------------------------------------------------------------------
// U20 — an unreachable Redis at boot must refuse to start or log an explicit opt-out
// ---------------------------------------------------------------------------------------------------

// TestU20_IngestorProceedsPastDeadRedisWithNoAttributedRefusal builds and runs the REAL ingestor binary
// from source (not the compose container, which has a healthy Redis) against a genuinely unreachable
// Redis address, using the same Postgres and NATS the compose stack itself uses. It is expected to FAIL.
//
// Environment wrinkle, documented rather than hidden: main.go hardcodes `Addr: ":8080"` — there is no
// PORT (or similar) environment variable the real code honors, and host port 8080 is already held by
// the running `sentinel-ingestor` compose container (confirmed via `docker ps`; several agents share
// this stack and stopping that container is out of bounds for this test). So this standalone process's
// own ListenAndServe call ALSO fails with "address already in use", and the process does exit non-zero
// — but for a reason that has nothing to do with Redis.
//
// Asserting only "exit code != 0" here would be exactly the kind of test bug the assignment warns
// about: green, and proving nothing about Redis. So this test instead requires the exit (or, short of
// exiting, the log) be attributable to Redis by an explicit, greppable, application-level message — not
// go-redis's own internal dial-retry logging, which prints regardless and is not a decision main.go
// made. It separately confirms the process reaches "Starting ingestor" (i.e. begins serving) while
// Redis is still down and un-consulted, which is the direct evidence that boot proceeds silently past a
// broken Redis with no decision ever logged about it.
//
// The second half of U20 — that a request against a live process is not silently accepted — is proven
// by TestU20_NilRedisClientSilentlyBypassesRateLimit below, at the package boundary, because a live HTTP
// call to THIS standalone process is not obtainable while its port is taken.
func TestU20_IngestorProceedsPastDeadRedisWithNoAttributedRefusal(t *testing.T) {
	requireStack(t)

	bin := ratelimitBuildIngestor(t)

	overrides, err := ratelimitIngestorEnv(cfg.DatabaseURL, cfg.NATSURL)
	if err != nil {
		t.Fatalf("building environment for standalone ingestor: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	cmd := osexec.CommandContext(ctx, bin)
	cmd.Env = ratelimitEnvOverlay(overrides)
	var log ratelimitSyncBuffer
	cmd.Stdout = &log
	cmd.Stderr = &log

	if err := cmd.Start(); err != nil {
		t.Fatalf("starting standalone ingestor: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	})

	var exited bool
	var exitCode int
	select {
	case waitErr := <-done:
		exited = true
		if waitErr == nil {
			exitCode = 0
		} else {
			var exitErr *osexec.ExitError
			if errors.As(waitErr, &exitErr) {
				exitCode = exitErr.ExitCode()
			} else {
				exitCode = -1
			}
		}
	case <-time.After(20 * time.Second):
		exited = false // still running after 20s — treated as "serving", see below
	}

	rawLog := log.String()
	appLines := ratelimitApplicationLogLines(rawLog)

	reachedStart := false
	explicitOptOut := false
	for _, line := range appLines {
		if strings.Contains(line, "Starting ingestor") {
			reachedStart = true
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "redis") && ratelimitLooksLikeExplicitDecision(lower) {
			explicitOptOut = true
		}
	}

	t.Logf("U20 process probe: exited=%v exitCode=%d reachedStartLine=%v explicitRedisOptOutLogged=%v\n"+
		"--- captured log ---\n%s--- end log ---", exited, exitCode, reachedStart, explicitOptOut, rawLog)

	if !explicitOptOut {
		reason := "it is still running (serving) with Redis unreachable and nothing logged about it"
		if exited {
			reason = "it exited non-zero for an unrelated reason — see the captured log's final lines " +
				"(host port 8080 is already held by the compose ingestor container in this environment)"
		}
		t.Errorf("S10 confirmed open (U20): standalone ingestor given an unreachable Redis "+
			"(REDIS_ADDR=127.0.0.1:1) never logged any application-level decision about it — no refusal, "+
			"no explicit opt-out. reachedStartLine=%v (it proceeded to \"Starting ingestor\" while Redis was "+
			"still down); %s. main.go:103's `redisClient, _ := redis.NewClient(...)` discards the connection "+
			"error outright, and ratelimit.go:44's `if rl.client == nil { next.ServeHTTP(...); return }` is "+
			"unconditional — nothing in this codepath ever decides to refuse to start or to log an opt-out.",
			reachedStart, reason)
	}
}

// TestU20_NilRedisClientSilentlyBypassesRateLimit is the second half of U20's proof: given a nil Redis
// client — the EXACT state main.go produces at apps/ingestor-go/main.go:103 when redis.NewClient's
// connection error is discarded — does an authenticated request get silently accepted with no rate
// limiting applied? It is expected to FAIL.
//
// This exercises the real `middleware.RateLimiter` type from apps/ingestor-go/middleware, wired exactly
// as main.go wires it (`middleware.NewRateLimiter(redisClient)` with `redisClient == nil`), and the real
// `auth.WithIdentity` context the authentication middleware would have already populated for a valid,
// authenticated request. Nothing here is reimplemented or mocked; it is the shipped code with the one
// input (a nil client) that a Redis outage at boot actually produces.
func TestU20_NilRedisClientSilentlyBypassesRateLimit(t *testing.T) {
	requireStack(t)

	var nilRedisClient *libredis.Client
	rl := middleware.NewRateLimiter(nilRedisClient)

	var innerHits atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		innerHits.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	handler := rl.Middleware(inner)

	const rpm = 1 // a real Redis-backed limiter would reject every request here after the first
	const requests = 20

	var accepted, limited int
	for i := 0; i < requests; i++ {
		ctx := auth.WithIdentity(context.Background(),
			"u20-project", "u20-project-id", "u20-org-id", "u20-apikeyhash", rpm, false)
		req := httptest.NewRequest(http.MethodPost, "/ingest", nil).WithContext(ctx)
		rec := httptest.NewRecorder()

		handler.ServeHTTP(rec, req)

		switch rec.Code {
		case http.StatusOK:
			accepted++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Errorf("request %d: unexpected status %d", i, rec.Code)
		}
		if got := rec.Header().Get("X-RateLimit-Limit"); got != "" {
			t.Errorf("request %d: got X-RateLimit-Limit=%q from a nil Redis client — the middleware cannot "+
				"have computed this without ever talking to Redis", i, got)
		}
	}

	if int64(accepted) != innerHits.Load() {
		t.Fatalf("test bug: inner handler was hit %d times but %d requests were tallied as accepted",
			innerHits.Load(), accepted)
	}

	t.Logf("U20 nil-client path: rpm=%d, requests=%d, accepted=%d, 429s=%d", rpm, requests, accepted, limited)

	if accepted != requests {
		t.Errorf("S10 confirmed open (U20): with rate_limit_rpm=%d and Redis unreachable (nil client — the "+
			"exact state main.go produces when redis.NewClient's error is discarded), a real Redis-backed "+
			"limiter would reject every request but the first; instead %d/%d requests were silently accepted "+
			"with no limiting applied at all (%d were 429). This is unconditional on RATELIMIT_STRICT_MODE: "+
			"ratelimit.go's `if rl.client == nil { next.ServeHTTP(...); return }` returns before strictMode "+
			"is ever consulted.", rpm, accepted, requests, limited)
	}
}

// ratelimitSyncBuffer is a mutex-guarded io.Writer so a subprocess's stdout and stderr can share one
// buffer without a data race — exec.Cmd copies each stream in its own goroutine.
type ratelimitSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *ratelimitSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *ratelimitSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ratelimitApplicationLogLines strips go-redis's own internal dial/retry logging (identifiable by its
// "pool.go:" source-file marker) out of a captured log, leaving only lines the ingestor's own code
// wrote. The library retries and logs those retries on its own regardless of what the application
// decides to do about it, so they must not be mistaken for an application-level decision.
func ratelimitApplicationLogLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		if line == "" || strings.Contains(line, "pool.go:") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// ratelimitLooksLikeExplicitDecision reports whether a lowercased log line reads like a deliberate
// application decision about Redis (a refusal or a documented opt-out), as opposed to incidental
// mention. Used only on lines that already passed ratelimitApplicationLogLines.
func ratelimitLooksLikeExplicitDecision(lowerLine string) bool {
	for _, kw := range []string{"refus", "disab", "opt-out", "opt out", "unreachable", "unavailable", "fatal"} {
		if strings.Contains(lowerLine, kw) {
			return true
		}
	}
	return false
}

// ratelimitBuildIngestor compiles the real apps/ingestor-go binary from source into a temp directory and
// returns its path. Building from source (rather than reusing the compose image) is what lets this test
// point a single process at a deliberately dead Redis without touching the shared container.
func ratelimitBuildIngestor(t *testing.T) string {
	t.Helper()

	root := ratelimitRepoRoot(t)
	bin := filepath.Join(t.TempDir(), "ingestor-u20")

	cmd := osexec.Command("go", "build", "-o", bin, "./apps/ingestor-go")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("building apps/ingestor-go from source for U20: %v\n%s", err, out)
	}
	return bin
}

// ratelimitRepoRoot walks upward from the working directory looking for go.work, so the build command
// above has a directory to run from regardless of how `go test` was invoked.
func ratelimitRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find repo root (looked for go.work walking up from the working directory)")
	return ""
}

// ratelimitIngestorEnv builds the environment overrides for a standalone ingestor process: the real
// Postgres (parsed out of databaseURL, since main.go wants discrete POSTGRES_* vars rather than a URL)
// and the real NATS the compose stack itself uses, a deliberately dead Redis address, and
// RATELIMIT_STRICT_MODE=true — set on purpose, to demonstrate that the nil-client fail-open path in
// ratelimit.go is unconditional on it. APIKEY_INVALIDATION_REQUIRED is turned off to isolate the
// variable this test is actually about; U20 is not about the NATS invalidation subscriber.
func ratelimitIngestorEnv(databaseURL, natsURL string) (map[string]string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return nil, err
	}
	password, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}

	return map[string]string{
		"POSTGRES_HOST":                u.Hostname(),
		"POSTGRES_PORT":                port,
		"POSTGRES_USER":                u.User.Username(),
		"POSTGRES_PASSWORD":            password,
		"POSTGRES_DB":                  strings.TrimPrefix(u.Path, "/"),
		"NATS_URL":                     natsURL,
		"REDIS_ADDR":                   "127.0.0.1:1", // nothing listens here — a genuinely dead Redis
		"REDIS_PASSWORD":               "",
		"REDIS_DB":                     "0",
		"RATELIMIT_STRICT_MODE":        "true",
		"APIKEY_INVALIDATION_REQUIRED": "false",
	}, nil
}

// ratelimitEnvOverlay layers overrides on top of the current process's environment, dropping any
// existing entry for a key that is being overridden so there is never an ambiguous duplicate.
func ratelimitEnvOverlay(overrides map[string]string) []string {
	skip := make(map[string]bool, len(overrides))
	for k := range overrides {
		skip[k] = true
	}

	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 && skip[kv[:i]] {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range overrides {
		env = append(env, k+"="+v)
	}
	return env
}
