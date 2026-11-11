package load

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type LoadTestConfig struct {
	IngesterURL  string
	APIKey       string
	NumWorkers   int
	EventsPerSec int
	DurationSec  int
	ProjectKey   string
	// Environment is the environment every generated event claims. Empty means
	// defaultLoadEnvironment. It is a field rather than a constant because
	// sentinel-project-environments §13's M3 has to drive the SAME load twice — once at an
	// environment the project has declared (§4.8's registry UPDATE matches one hot row) and once at
	// an environment it has not (the UPDATE matches zero) — and the difference between those two
	// runs is the whole measurement.
	Environment string
	// UserPool is sentinel-affected-users §13's `SENTINEL_LOAD_USER_POOL`: when > 0, event n carries
	// `user_id` = "load-user-<n mod pool>" and `tenant_id` = "load-tenant-<n mod pool>" in its
	// metadata, so the §4.3 rollup runs on the full stack; 0 (the default) sends neither key, which
	// is exactly what this harness sent before it was configurable. It is an OPTIONAL full-stack
	// extra (decision 29): §13's M2 criterion is read on the store-boundary loop in
	// tests/integration/affected_m2_timing_test.go, not on anything this harness produces.
	UserPool int
}

// defaultLoadEnvironment preserves the value this harness sent before it was configurable.
const defaultLoadEnvironment = "load-test"

type ErrorPayload struct {
	ProjectKey  string                 `json:"project_key"`
	Platform    string                 `json:"platform"`
	Environment string                 `json:"environment"`
	Message     string                 `json:"message"`
	ErrorClass  string                 `json:"error_class"`
	TraceID     string                 `json:"trace_id"`
	SpanID      string                 `json:"span_id"`
	Stacktrace  []StackFrame           `json:"stacktrace"`
	Metadata    map[string]interface{} `json:"metadata"`
	Timestamp   time.Time              `json:"timestamp"`
	TraceFlags  uint32                 `json:"trace_flags"`
}

type StackFrame struct {
	File     string `json:"file"`
	Function string `json:"function"`
	Line     int32  `json:"line"`
	InApp    bool   `json:"in_app"`
}

type LoadResult struct {
	TotalSent     int64
	TotalAccepted int64
	TotalRejected int64
	TotalErrors   int64
	SuccessRate   float64
	EventsPerSec  float64
	LatencyAvgMs  float64
	LatencyP99Ms  float64
}

func RunLoadTest(cfg LoadTestConfig) (*LoadResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.DurationSec)*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var sent, accepted, rejected, errors int64

	latencies := make([]float64, 0, cfg.NumWorkers*cfg.EventsPerSec*cfg.DurationSec)
	var latencyMu sync.Mutex

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	sentPerSec := make([]int64, cfg.DurationSec)
	var secIdx int64

	go func() {
		for {
			select {
			case <-ticker.C:
				secIdx++
			case <-ctx.Done():
				return
			}
		}
	}()

	environment := cfg.Environment
	if environment == "" {
		environment = defaultLoadEnvironment
	}

	// With no user pool every event is byte-identical, so it is marshalled once; with a pool each
	// event's ids depend on its sequence number and it is marshalled per send (loadPayload).
	var payloadBytes []byte
	if cfg.UserPool <= 0 {
		var err error
		payloadBytes, err = json.Marshal(loadPayload(cfg, environment, 0))
		if err != nil {
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}
	}
	var seq int64 // the per-event sequence number the pool draws from, shared by every worker

	client := &http.Client{Timeout: 5 * time.Second}

	workerRate := cfg.EventsPerSec / cfg.NumWorkers
	if workerRate < 1 {
		workerRate = 1
	}
	interval := time.Second / time.Duration(workerRate)

	for w := 0; w < cfg.NumWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					body := payloadBytes
					if cfg.UserPool > 0 {
						n := atomic.AddInt64(&seq, 1) - 1
						var mErr error
						body, mErr = json.Marshal(loadPayload(cfg, environment, n))
						if mErr != nil {
							atomic.AddInt64(&errors, 1)
							continue
						}
					}

					start := time.Now()

					req, err := http.NewRequest(http.MethodPost, cfg.IngesterURL+"/ingest", bytes.NewReader(body))
					if err != nil {
						atomic.AddInt64(&errors, 1)
						continue
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-API-Key", cfg.APIKey)

					resp, err := client.Do(req)
					latency := float64(time.Since(start).Milliseconds())

					latencyMu.Lock()
					latencies = append(latencies, latency)
					latencyMu.Unlock()

					atomic.AddInt64(&sent, 1)
					if secIdx < int64(len(sentPerSec)) {
						atomic.AddInt64(&sentPerSec[secIdx], 1)
					}

					if err != nil {
						atomic.AddInt64(&errors, 1)
						continue
					}

					if resp.StatusCode == http.StatusAccepted {
						atomic.AddInt64(&accepted, 1)
					} else {
						atomic.AddInt64(&rejected, 1)
					}
					resp.Body.Close()
				}
			}
		}(w)
	}

	wg.Wait()

	var totalSent, totalAccepted, totalRejected, totalErrors int64
	for i := 0; i < len(sentPerSec); i++ {
		totalSent += sentPerSec[i]
	}
	totalSent = sent
	totalAccepted = accepted
	totalRejected = rejected
	totalErrors = errors

	totalRequests := totalSent
	successRate := 0.0
	if totalRequests > 0 {
		successRate = float64(totalAccepted) / float64(totalRequests) * 100
	}

	var latencySum, latencyP99 float64
	latencyCount := len(latencies)
	if latencyCount > 0 {
		for _, l := range latencies {
			latencySum += l
		}
		latencyAvg := latencySum / float64(latencyCount)

		p99Idx := int(float64(latencyCount) * 0.99)
		if p99Idx >= latencyCount {
			p99Idx = latencyCount - 1
		}

		var sorted []float64
		sorted = append(sorted, latencies...)
		for i := 0; i < len(sorted)-1; i++ {
			for j := i + 1; j < len(sorted); j++ {
				if sorted[j] < sorted[i] {
					sorted[i], sorted[j] = sorted[j], sorted[i]
				}
			}
		}
		latencyP99 = sorted[p99Idx]

		return &LoadResult{
			TotalSent:     totalSent,
			TotalAccepted: totalAccepted,
			TotalRejected: totalRejected,
			TotalErrors:   totalErrors,
			SuccessRate:   successRate,
			EventsPerSec:  float64(totalSent) / float64(cfg.DurationSec),
			LatencyAvgMs:  latencyAvg,
			LatencyP99Ms:  latencyP99,
		}, nil
	}

	return &LoadResult{
		TotalSent:     totalSent,
		TotalAccepted: totalAccepted,
		TotalRejected: totalRejected,
		TotalErrors:   totalErrors,
		SuccessRate:   successRate,
		EventsPerSec:  float64(totalSent) / float64(cfg.DurationSec),
	}, nil
}

// loadPayload is the event this harness sends. Everything but the metadata is constant — one
// fingerprint, one environment — so every event lands on the same issue and the only thing a run
// varies is what §4.3's rollup sees. With cfg.UserPool > 0 the metadata carries `user_id` and
// `tenant_id` drawn from the pool by seq (the keys indexer.ExtractSearchFields reads first, so the
// precedence is the indexer's); the values stay under six digits so the normaliser's
// <NUMERIC_ID> rule is not what a reader of error_search_index sees. With a pool of 0 neither key
// is present — not empty, ABSENT — so decision 5's `empty` counter does not move either.
func loadPayload(cfg LoadTestConfig, environment string, seq int64) ErrorPayload {
	metadata := map[string]interface{}{"load_test": true}
	if cfg.UserPool > 0 {
		n := seq % int64(cfg.UserPool)
		metadata["user_id"] = fmt.Sprintf("load-user-%d", n)
		metadata["tenant_id"] = fmt.Sprintf("load-tenant-%d", n)
	}
	return ErrorPayload{
		ProjectKey:  cfg.ProjectKey,
		Platform:    "golang",
		Environment: environment,
		Message:     "Load test error message",
		ErrorClass:  "LoadTestError",
		TraceID:     "load-test-trace",
		SpanID:      "load-test-span",
		Stacktrace: []StackFrame{
			{File: "load_test.go", Line: 100, Function: "RunLoadTest", InApp: true},
			{File: "processor.go", Line: 50, Function: "ProcessEvent", InApp: true},
			{File: "handler.go", Line: 25, Function: "HandleRequest", InApp: true},
		},
		Metadata:   metadata,
		Timestamp:  time.Now(),
		TraceFlags: 0,
	}
}

func PrintResult(r *LoadResult) {
	fmt.Println("=== Load Test Results ===")
	fmt.Printf("Total Sent:       %d\n", r.TotalSent)
	fmt.Printf("Total Accepted:   %d\n", r.TotalAccepted)
	fmt.Printf("Total Rejected:   %d\n", r.TotalRejected)
	fmt.Printf("Total Errors:     %d\n", r.TotalErrors)
	fmt.Printf("Success Rate:     %.2f%%\n", r.SuccessRate)
	fmt.Printf("Events/sec:       %.2f\n", r.EventsPerSec)
	if r.LatencyAvgMs > 0 {
		fmt.Printf("Latency Avg:     %.2f ms\n", r.LatencyAvgMs)
		fmt.Printf("Latency P99:     %.2f ms\n", r.LatencyP99Ms)
	}

	if r.SuccessRate >= 99.0 {
		fmt.Println("\n✓ PASS: Success rate >= 99%")
	} else {
		fmt.Println("\n✗ FAIL: Success rate < 99%")
	}
}

// TestLoadIngest is the entry point this package did not have.
//
// sentinel-project-environments §13's M3 names `tests/load/` as its driver, and until now that was
// not something anyone could run: the package declared LoadTestConfig, RunLoadTest and PrintResult
// and NO TestXxx function at all, so `go test ./tests/load/... -v -count=1` compiled it and ran
// zero tests, and LoadTestConfig was read from no environment variable, no flag and no caller
// anywhere in the repository.
//
// The t.Skip is the load-bearing half. `go test ./...` on a machine with no stack must stay green
// and must not fire ten thousand HTTP requests at whatever happens to be listening — so this drives
// traffic only for somebody who has deliberately pointed it at a stack by setting both
// SENTINEL_LOAD_INGESTER_URL and SENTINEL_LOAD_API_KEY.
//
// M3's two cases are the same invocation with SENTINEL_LOAD_ENVIRONMENT set to an environment the
// project has declared and then to one it has not. The number M3 reports comes from the PROCESSOR's
// own `sentinel_process_duration_seconds` histogram, scraped from its /metrics before and after
// each run — this harness publishes at the ingest HTTP edge and cannot see StoreEvent, which runs
// downstream of NATS. Any latency printed below is publish latency, not processing latency.
func TestLoadIngest(t *testing.T) {
	ingesterURL := os.Getenv("SENTINEL_LOAD_INGESTER_URL")
	apiKey := os.Getenv("SENTINEL_LOAD_API_KEY")
	if ingesterURL == "" || apiKey == "" {
		t.Skip("SENTINEL_LOAD_INGESTER_URL and SENTINEL_LOAD_API_KEY are unset — " +
			"set both to point this harness at a running stack (§13 M3)")
	}

	cfg := LoadTestConfig{
		IngesterURL:  ingesterURL,
		APIKey:       apiKey,
		ProjectKey:   os.Getenv("SENTINEL_LOAD_PROJECT_KEY"),
		NumWorkers:   loadEnvInt(t, "SENTINEL_LOAD_WORKERS", 10),
		EventsPerSec: loadEnvInt(t, "SENTINEL_LOAD_EVENTS_PER_SEC", 200),
		DurationSec:  loadEnvInt(t, "SENTINEL_LOAD_DURATION_SEC", 50),
		Environment:  os.Getenv("SENTINEL_LOAD_ENVIRONMENT"),
		UserPool:     loadEnvNonNegInt(t, "SENTINEL_LOAD_USER_POOL", 0),
	}

	result, err := RunLoadTest(cfg)
	if err != nil {
		t.Fatalf("RunLoadTest: %v", err)
	}
	PrintResult(result)
}

// loadEnvNonNegInt is loadEnvInt for a value whose zero is meaningful: SENTINEL_LOAD_USER_POOL=0 is
// "send no ids" (§13's pool (i)), not a typo, so only a negative or malformed value fails.
func loadEnvNonNegInt(t *testing.T, name string, def int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 0 {
		t.Fatalf("%s must be a non-negative integer, got %q", name, raw)
	}
	return v
}

// loadEnvInt reads a positive integer override, falling back to def. A malformed or non-positive
// value fails the test rather than silently reverting to the default: a typo'd worker count that
// quietly runs the default is a measurement nobody can reproduce.
func loadEnvInt(t *testing.T, name string, def int) int {
	t.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		t.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return v
}
