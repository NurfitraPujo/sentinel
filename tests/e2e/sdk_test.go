//go:build e2e

// BUILD TAG, and why only this file in the package has one:
//
// tests/e2e is part of the ROOT module; packages/sdk-go is a SEPARATE module. This file can import it only
// in workspace mode (the committed go.work, P0-3). CI's `go-root` job deliberately runs with GOWORK=off so
// it exercises the root module exactly as a real `go get` would see it (constraint A2 in
// docs/memory/ARCHITECTURE.md) — and under GOWORK=off, `go vet ./...` fails here with "no required module
// provides package .../packages/sdk-go". `go build ./...` does not, because it never compiles _test.go
// files, which is precisely why this is the kind of break that reaches CI unnoticed.
//
// tests/contract solves the same problem the same way (`//go:build contract`). The tag is on THIS FILE
// ONLY, not the whole package, so every other file under tests/e2e keeps its default `go vet ./...`
// coverage — one broken test file silently disabling a whole Go package is B4, and tagging the package
// wholesale would opt the entire suite out of the check that catches it.
//
// The cost is that a plain `go test ./tests/e2e/` omits U8-U10. sdk_tag_guard_test.go makes that omission
// loud instead of silent.
//
// This file covers matrix rows U8-U10 from docs/plans/E2E_RECOVERY_PLAN.md's P7 section: it drives the
// REAL, published packages/sdk-go client against the REAL ingestor over HTTP, all the way through NATS and
// the processor into Postgres. Every existing SDK test (packages/sdk-go/integration_test.go,
// tests/contract/sdk_ingestor_test.go) either talks to an httptest mock server or stops at the
// wire-contract/proto layer - neither one proves the SDK can talk to the deployed ingestor binary and land a
// row in the database. That gap is exactly how S4 (SDK could not talk to the ingestor at all) and S5/B6
// (release_version destroyed by normalization) shipped undetected.
//
// Package fact: tests/e2e is part of the ROOT module. packages/sdk-go is a SEPARATE module. This file can
// import it only because the repo-root go.work (P0-3) puts both in workspace mode locally.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	sentinel "github.com/NurfitraPujo/sentinel/packages/sdk-go"
)

// sdkNewClient builds a *sentinel.Client wired at the live ingestor for fixture f, with low-latency
// batching defaults for a test (small batch, short wait) so events reach the wire quickly without relying
// on Flush timing alone. Every helper in this file is prefixed sdk* to avoid colliding with another agent's
// file in this package.
func sdkNewClient(f *fixture, overrides func(*sentinel.Config)) *sentinel.Client {
	f.t.Helper()

	cfg := sentinel.Config{
		APIKey:         f.APIKey,      // secret -> X-API-Key header (never in the body, D11)
		ProjectKey:     f.ProjectName, // project's unique NAME -> body project_key
		Endpoint:       sdkIngestEndpoint(),
		Environment:    "e2e",
		ReleaseVersion: "1.2.3-sdk-e2e",
		BatchSize:      5,
		BatchWait:      200 * time.Millisecond,
		MaxBufferSize:  200,
	}
	if overrides != nil {
		overrides(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		f.t.Fatalf("sdkNewClient: invalid config: %v", err)
	}
	return sentinel.Init(cfg)
}

// sdkIngestEndpoint returns the live ingestor's single-event endpoint. sentinel.Config.Endpoint is the
// FULL endpoint path (transport.go's sendBatch appends "/batch" itself for multi-event batches), matching
// the shape of the SDK's own withDefaults() default ("http://localhost:8080/ingest").
func sdkIngestEndpoint() string {
	return cfg.IngestorURL + "/ingest"
}

// sdkFlush flushes the SDK's default-client transport and fails the test if the flush did not complete
// within the timeout. Flush's own timeout is a legitimate use of a deadline here (unlike the async NATS
// hop, which must use waitFor) because the SDK client itself defines what "done" means.
func sdkFlush(t *testing.T, timeout time.Duration) {
	t.Helper()
	if !sentinel.Flush(timeout) {
		t.Fatalf("sentinel.Flush(%s) timed out before the transport drained", timeout)
	}
}

// sdkMetaKeys is a small formatting helper for failure messages.
func sdkMetaKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Note on occurrenceRow.IssueMessage (harness_test.go): error_occurrences has no message column of its
// own in the real migrated schema - the message lives on issues.message, set once at issue creation
// (apps/processor-go/store/store.go's INSERT INTO issues). f.occurrences() joins that in as
// IssueMessage, which is what TestU8 below asserts on.

// ---------------------------------------------------------------------------------------------------------
// U8 - the real SDK talks to the real ingestor.
// ---------------------------------------------------------------------------------------------------------

// TestU8_RealSDKEndToEndPipeline proves matrix row U8: the real packages/sdk-go client, talking to the real
// ingestor over HTTP (not an httptest mock server), produces an error_occurrences row with a non-empty
// message, populated metadata, platform="go", and the configured release_version surviving intact.
//
// This is the bar packages/sdk-go's own TestGoSDKEndToEndPipeline (integration_test.go) does not clear
// (VERIFIED_STATE.md: it passes only against an httptest mock). S4 was "the official SDK cannot talk to the
// ingestor at all"; S5/B6 was release_version being destroyed by the processor's Normalize() because it was
// smuggled through metadata instead of being a first-class field - this test asserts both are actually
// fixed on the deployed binaries, not just in a package test.
func TestU8_RealSDKEndToEndPipeline(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	var (
		mu           sync.Mutex
		onErrorCalls int
	)
	sdkNewClient(f, func(cfg *sentinel.Config) {
		cfg.Debug = true
		cfg.OnError = func(err error) {
			mu.Lock()
			onErrorCalls++
			mu.Unlock()
			t.Logf("unexpected OnError during U8 (should be a clean accept): %v", err)
		}
	})

	ctx := context.Background()
	ctx = sentinel.WithUser(ctx, "u8-user")
	ctx = sentinel.WithTag(ctx, "case", "u8-real-sdk-pipeline")
	sentinel.CaptureErrorContext(ctx, fmt.Errorf("u8 real sdk end-to-end probe %s", uniqueSuffix()))

	sdkFlush(t, 10*time.Second)

	f.waitForOccurrences(1)

	occs := f.occurrences()
	if len(occs) != 1 {
		t.Fatalf("expected exactly 1 occurrence row, got %d", len(occs))
	}
	occ := occs[0]
	if occ.IssueMessage == "" {
		t.Error("issue.message is empty - S4 regression: the real ingestor did not receive/store the SDK's message field")
	}
	if occ.Platform != "go" {
		t.Errorf("occurrence.platform = %q, want \"go\"", occ.Platform)
	}
	if occ.ReleaseVersion == nil || *occ.ReleaseVersion != "1.2.3-sdk-e2e" {
		got := "<nil>"
		if occ.ReleaseVersion != nil {
			got = *occ.ReleaseVersion
		}
		t.Errorf("occurrence.release_version = %s, want \"1.2.3-sdk-e2e\" (S5/B6: release_version must survive Normalize as a first-class field, not via metadata)", got)
	}
	if len(occ.Metadata) == 0 {
		t.Error("occurrence.metadata is empty - want the SDK's context tags (user_id, case) to have survived to Postgres")
	} else {
		var meta map[string]any
		if err := json.Unmarshal(occ.Metadata, &meta); err != nil {
			t.Fatalf("occurrence.metadata was not valid JSON: %v\n  raw: %s", err, occ.Metadata)
		}
		if _, ok := meta["user_id"]; !ok {
			t.Errorf("occurrence.metadata missing \"user_id\" tag set via sentinel.WithUser; got keys: %v", sdkMetaKeys(meta))
		}
	}

	mu.Lock()
	calls := onErrorCalls
	mu.Unlock()
	if calls != 0 {
		t.Errorf("OnError fired %d time(s) for what should have been a clean, accepted event", calls)
	}
}

// ---------------------------------------------------------------------------------------------------------
// U9 - the SDK surfaces a 4xx through its error hook instead of swallowing it.
// ---------------------------------------------------------------------------------------------------------

// TestU9_SDKSurfaces4xxThroughOnError proves matrix row U9: when the real ingestor rejects a request with a
// 4xx (here: a revoked API key, which apps/ingestor-go/auth/apikey.go answers with 401 "Invalid API key"),
// the SDK surfaces that failure through Config.OnError - not merely "does not panic", which proves nothing.
// It also asserts nothing lands in Postgres for the rejected event, and that a well-formed request with the
// fixture's own active key still succeeds, so this is known to be exercising a real rejection rather than a
// broken ingestor that rejects everything.
func TestU9_SDKSurfaces4xxThroughOnError(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	revokedSecret := newSecret(t)
	now := time.Now()
	f.addKey(keySpec{
		Name:          "revoked-for-u9",
		Secret:        revokedSecret,
		ProjectScoped: true,
		Status:        "revoked",
		RevokedAt:     &now,
	})

	var (
		mu          sync.Mutex
		onErrorErrs []error
	)
	sdkNewClient(f, func(cfg *sentinel.Config) {
		cfg.APIKey = revokedSecret
		cfg.Debug = true
		cfg.OnError = func(err error) {
			mu.Lock()
			onErrorErrs = append(onErrorErrs, err)
			mu.Unlock()
		}
		// Keep this to a single request/response round trip: BatchSize=1 with a short wait so the one
		// captured error is sent as its own batch immediately rather than waiting to coalesce with
		// anything else.
		cfg.BatchSize = 1
		cfg.BatchWait = 50 * time.Millisecond
	})

	sentinel.CaptureError(fmt.Errorf("u9 revoked-key probe %s", uniqueSuffix()))

	sdkFlush(t, 10*time.Second)

	waitFor(t, 5*time.Second, "OnError to fire for the revoked-key rejection", func() (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		return len(onErrorErrs) > 0, fmt.Sprintf("%d OnError call(s) so far", len(onErrorErrs))
	})

	mu.Lock()
	errs := append([]error(nil), onErrorErrs...)
	mu.Unlock()
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 OnError call for the single rejected event, got %d: %v", len(errs), errs)
	}
	if errs[0] == nil {
		t.Fatal("OnError fired with a nil error")
	}

	// Nothing should have reached Postgres: the event was rejected at auth, before it ever touched
	// validation, NATS, or the processor.
	if got := f.occurrenceCount(); got != 0 {
		t.Errorf("expected 0 occurrences from a request rejected at auth, got %d", got)
	}

	// Negative control: the fixture's own active key must still work, proving the ingestor is not simply
	// rejecting everything.
	sdkNewClient(f, func(cfg *sentinel.Config) {
		cfg.BatchSize = 1
		cfg.BatchWait = 50 * time.Millisecond
	})
	sentinel.CaptureError(fmt.Errorf("u9 control probe with valid key %s", uniqueSuffix()))
	sdkFlush(t, 10*time.Second)
	f.waitForOccurrences(1)
}

// ---------------------------------------------------------------------------------------------------------
// U10 - Flush(timeout) before process exit loses no events.
// ---------------------------------------------------------------------------------------------------------

// TestU10_FlushLosesNoEvents proves matrix row U10: enqueuing N events and calling Flush(timeout) - the
// pattern an application is expected to run immediately before process exit - delivers exactly N
// occurrences, with none dropped and none duplicated. N is deliberately not a multiple of BatchSize, so
// Flush must drain a partial final batch in addition to full ones, and MaxBufferSize is raised above N so a
// real loss (not merely a slow delivery, and not an unrelated buffer-overflow eviction) is what would be
// caught.
func TestU10_FlushLosesNoEvents(t *testing.T) {
	requireStack(t)
	f := newFixture(t)

	const n = 37

	var (
		mu       sync.Mutex
		dropErrs []error
	)
	sdkNewClient(f, func(cfg *sentinel.Config) {
		cfg.BatchSize = 10
		cfg.BatchWait = 5 * time.Second // long enough that Flush, not the ticker, is what forces delivery
		cfg.MaxBufferSize = n + 10
		cfg.Debug = true
		cfg.OnError = func(err error) {
			mu.Lock()
			dropErrs = append(dropErrs, err)
			mu.Unlock()
		}
	})

	for i := 0; i < n; i++ {
		sentinel.CaptureError(fmt.Errorf("u10 flush-loses-no-events probe %d/%d %s", i, n, uniqueSuffix()))
	}

	sdkFlush(t, 20*time.Second)

	mu.Lock()
	drops := append([]error(nil), dropErrs...)
	mu.Unlock()
	if len(drops) != 0 {
		t.Errorf("OnError reported %d drop(s) during a plain Flush with no injected failures: %v", len(drops), drops)
	}

	f.waitForOccurrences(n)
}
