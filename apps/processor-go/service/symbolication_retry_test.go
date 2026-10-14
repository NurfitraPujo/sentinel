package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/fingerprint"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	sentinelv1 "github.com/NurfitraPujo/sentinel/gen/sentinel/v1"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/event"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
	"github.com/NurfitraPujo/sentinel/packages/shared-go/nats"
)

func serviceWithStore(t *testing.T, lookup symbolicate.StoreFunc) *ProcessorService {
	t.Helper()
	stage, err := symbolicate.New(symbolicate.DefaultConfig(), lookup,
		symbolicate.FetcherFunc(func(context.Context, string) ([]byte, error) { return nil, errors.New("unused") }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stage.Close)
	return &ProcessorService{symbolicator: stage, symPlatforms: symbolicate.DefaultPlatforms}
}

func jsEvent() *event.ErrorEvent {
	return &event.ErrorEvent{
		Platform: "javascript", ReleaseVersion: "1.0.0",
		Stacktrace: []event.StackFrame{{File: "/assets/index-3f2a1c.js", Line: 1, Column: 10, Function: "e", InApp: true}},
	}
}

// A store that errors ends the frame as fetch_error; the stage reports it as transient so the
// caller can ask for redelivery (sentinel-grouping-v2 §1.4).
func TestSymbolicate_ReportsTransientOutcome(t *testing.T) {
	s := serviceWithStore(t, func(context.Context, string, string, string) (*symbolicate.Sourcemap, error) {
		return nil, errors.New("db down")
	})
	if !s.symbolicate(context.Background(), jsEvent(), "p1") {
		t.Fatal("fetch_error must be reported as transient")
	}
}

// no_map is a permanent answer (negatively cached): redelivering cannot change it.
func TestSymbolicate_NoMapIsNotTransient(t *testing.T) {
	s := serviceWithStore(t, func(context.Context, string, string, string) (*symbolicate.Sourcemap, error) { return nil, nil })
	if s.symbolicate(context.Background(), jsEvent(), "p1") {
		t.Fatal("no_map must not be reported as transient")
	}
}

// A platform outside the gate set is never symbolicated, so never transient.
func TestSymbolicate_SkippedPlatformIsNotTransient(t *testing.T) {
	s := serviceWithStore(t, func(context.Context, string, string, string) (*symbolicate.Sourcemap, error) {
		return nil, errors.New("db down")
	})
	e := jsEvent()
	e.Platform = "go"
	if s.symbolicate(context.Background(), e, "p1") {
		t.Fatal("a skipped platform must not be transient")
	}
}

// The budget is per message: deliveries 1 and 2 may be redelivered (the third is processed), and a
// context that never passed through the subscriber never retries.
func TestShouldRetrySymbolication_Budget(t *testing.T) {
	cases := []struct {
		n    uint64
		want bool
	}{{0, false}, {1, true}, {2, true}, {3, false}, {7, false}}
	for _, c := range cases {
		if got := shouldRetrySymbolication(nats.WithDeliveryCount(context.Background(), c.n)); got != c.want {
			t.Errorf("delivery %d: got %v, want %v", c.n, got, c.want)
		}
	}
	if shouldRetrySymbolication(context.Background()) {
		t.Error("a context without a delivery count must never retry")
	}
	if MaxSymbolicationRetries != 2 {
		t.Errorf("the contract bounds the retry at 2, got %d", MaxSymbolicationRetries)
	}
}

// ---- processEventInternal: the retry path end to end (sentinel-grouping-v2 repair R2, R4) --------

// retryStore is the IssueStore processEventInternal needs up to and including StoreEvent. Every
// other method is the embedded nil interface and panics if the retry path ever reaches it, which
// is the point: a retried delivery must return before anything is written. StoreEvent reports a
// duplicate, which ends processEventInternal right after the call.
type retryStore struct {
	store.IssueStore
	storeCalls int
	issue      *store.Issue
	occ        *store.ErrorOccurrence
}

func (r *retryStore) ResolveProjectID(context.Context, string, string) (string, error) {
	return "11111111-1111-1111-1111-111111111111", nil
}

func (r *retryStore) StoreEvent(_ context.Context, issue *store.Issue, occ *store.ErrorOccurrence, _ string) (store.IssueOutcome, bool, error) {
	r.storeCalls++
	r.issue, r.occ = issue, occ
	return 0, false, nil
}

// cartMap maps generated line 1 (any column) to src/cart.ts, function computeTotal.
const cartMap = `{"version":3,"file":"index.js","sources":["src/cart.ts"],"names":["computeTotal"],"mappings":"AAAAA"}`

const retryMinifiedFile = "/assets/index-3f2a1c.js"

func retryEventBytes(t *testing.T, customFingerprint string) []byte {
	t.Helper()
	b, err := proto.Marshal(&sentinelv1.ErrorEvent{
		ProjectKey: "web", ProjectId: "11111111-1111-1111-1111-111111111111", Platform: "javascript",
		Environment: "production", ErrorClass: "TypeError", Message: "x is undefined",
		ReleaseVersion: "1.0.0", Fingerprint: customFingerprint, Timestamp: timestamppb.Now(),
		Stacktrace: []*sentinelv1.StackFrame{{File: retryMinifiedFile, Line: 1, Column: 10, Function: "e", InApp: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func retryService(t *testing.T, lookup symbolicate.StoreFunc, fetch symbolicate.FetcherFunc) (*ProcessorService, *retryStore) {
	t.Helper()
	stage, err := symbolicate.New(symbolicate.DefaultConfig(), lookup, fetch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stage.Close)
	rs := &retryStore{}
	return &ProcessorService{store: rs, symbolicator: stage, symPlatforms: symbolicate.DefaultPlatforms}, rs
}

func cartRow() *symbolicate.Sourcemap {
	sum := sha256.Sum256([]byte(cartMap))
	return &symbolicate.Sourcemap{ID: "m1", ProjectID: "11111111-1111-1111-1111-111111111111", ReleaseVersion: "1.0.0",
		MinifiedPath: retryMinifiedFile, StorageKey: "k", SizeBytes: int64(len(cartMap)), SHA256: hex.EncodeToString(sum[:])}
}

func minifiedFingerprint() string {
	return fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production", ErrorClass: "TypeError", Message: "x is undefined", Platform: "javascript",
		NormalisePlatforms: symbolicate.DefaultPlatforms,
		Stacktrace:         []fingerprint.StackFrame{{File: retryMinifiedFile, Function: "e", InApp: true}},
	})
}

func sourceFingerprint() string {
	return fingerprint.Compute(fingerprint.FingerprintConfig{
		Environment: "production", ErrorClass: "TypeError", Message: "x is undefined", Platform: "javascript",
		NormalisePlatforms: symbolicate.DefaultPlatforms,
		Stacktrace:         []fingerprint.StackFrame{{File: "src/cart.ts", Function: "computeTotal", InApp: true, Symbolicated: true}},
	})
}

// A source-map STORE that fails: the first delivery returns a non-permanent error before
// StoreEvent is reached (so nothing is written and the subscriber's delayed Nak applies), and the
// third delivery — the budget spent — is processed with the minified hash.
func TestProcessEventInternal_StoreFailureRetriesBeforeStoreEvent(t *testing.T) {
	s, rs := retryService(t, func(context.Context, string, string, string) (*symbolicate.Sourcemap, error) {
		return nil, errors.New("db down")
	}, func(context.Context, string) ([]byte, error) { return nil, errors.New("unused") })
	data := retryEventBytes(t, "")

	for _, n := range []uint64{1, 2} {
		_, err := s.processEventInternal(nats.WithDeliveryCount(context.Background(), n), data)
		if !errors.Is(err, errSymbolicationRetry) || nats.IsPermanent(err) {
			t.Fatalf("delivery %d: err = %v; want the transient retry error, not a permanent one", n, err)
		}
		if rs.storeCalls != 0 {
			t.Fatalf("delivery %d reached StoreEvent; a retried delivery must write nothing", n)
		}
	}
	if _, err := s.processEventInternal(nats.WithDeliveryCount(context.Background(), 3), data); err != nil {
		t.Fatalf("delivery 3: %v; the budget is spent, the event is processed", err)
	}
	if rs.storeCalls != 1 || rs.issue.Fingerprint != minifiedFingerprint() {
		t.Fatalf("delivery 3: %d store calls, fingerprint %q; want one call with the minified-frame hash", rs.storeCalls, rs.issue.Fingerprint)
	}
}

// R2: a fetch that fails on the first delivery and succeeds on the second. The first delivery's
// fetch_error is negatively cached; the redelivery must bypass that entry, symbolicate, and hash on
// the SOURCE-level frames.
func TestProcessEventInternal_FetchErrorRetrySucceedsOnRedelivery(t *testing.T) {
	var fetches atomic.Int32
	s, rs := retryService(t,
		func(context.Context, string, string, string) (*symbolicate.Sourcemap, error) { return cartRow(), nil },
		func(context.Context, string) ([]byte, error) {
			if fetches.Add(1) == 1 {
				return nil, errors.New("connection refused")
			}
			return []byte(cartMap), nil
		})
	data := retryEventBytes(t, "")

	_, err := s.processEventInternal(nats.WithDeliveryCount(context.Background(), 1), data)
	if !errors.Is(err, errSymbolicationRetry) || rs.storeCalls != 0 {
		t.Fatalf("delivery 1: err = %v, %d store calls; want the retry error before StoreEvent", err, rs.storeCalls)
	}
	if _, err := s.processEventInternal(nats.WithDeliveryCount(context.Background(), 2), data); err != nil {
		t.Fatalf("delivery 2: %v", err)
	}
	if rs.storeCalls != 1 {
		t.Fatalf("delivery 2: %d store calls, want 1", rs.storeCalls)
	}
	if got, want := rs.issue.Fingerprint, sourceFingerprint(); got != want {
		t.Fatalf("delivery 2 fingerprint %s, want the source-frame hash %s (minified would be %s)", got, want, minifiedFingerprint())
	}
	if !strings.Contains(string(rs.occ.Stacktrace), `"symbolicated":true`) || !strings.Contains(string(rs.occ.Stacktrace), "src/cart.ts") {
		t.Fatalf("delivery 2 stored an unsymbolicated stacktrace: %s", rs.occ.Stacktrace)
	}
}

// no_map is permanent: it is not retried, and a redelivery does not bypass its negative entry.
func TestProcessEventInternal_NoMapIsNotRetried(t *testing.T) {
	var lookups atomic.Int32
	s, rs := retryService(t, func(context.Context, string, string, string) (*symbolicate.Sourcemap, error) {
		lookups.Add(1)
		return nil, nil
	}, func(context.Context, string) ([]byte, error) { return nil, errors.New("unused") })
	if _, err := s.processEventInternal(nats.WithDeliveryCount(context.Background(), 1), retryEventBytes(t, "")); err != nil || rs.storeCalls != 1 {
		t.Fatalf("no_map: err = %v, %d store calls; want processed on the first delivery", err, rs.storeCalls)
	}
	if _, err := s.processEventInternal(nats.WithDeliveryCount(context.Background(), 2), retryEventBytes(t, "")); err != nil {
		t.Fatal(err)
	}
	if lookups.Load() != 1 {
		t.Fatalf("%d store lookups; the redelivery must still be answered from the no_map entry", lookups.Load())
	}
}

// A client fingerprint does not depend on the frames, so it is never delayed.
func TestProcessEventInternal_ClientFingerprintIsNotDelayed(t *testing.T) {
	s, rs := retryService(t, func(context.Context, string, string, string) (*symbolicate.Sourcemap, error) {
		return nil, errors.New("db down")
	}, func(context.Context, string) ([]byte, error) { return nil, errors.New("unused") })
	if _, err := s.processEventInternal(nats.WithDeliveryCount(context.Background(), 1), retryEventBytes(t, "checkout-timeout")); err != nil {
		t.Fatalf("a client-fingerprinted event was delayed: %v", err)
	}
	if rs.storeCalls != 1 || rs.issue.Fingerprint != "checkout-timeout" {
		t.Fatalf("%d store calls, fingerprint %q; want one call with the verbatim client fingerprint", rs.storeCalls, rs.issue.Fingerprint)
	}
}

// The retry is a deliberate redelivery, not a store failure: withSpan leaves the span's status
// alone and tags it with the retry outcome.
func TestWithSpan_RetryIsNotRecordedAsFailure(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := tracer
	tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("test")
	t.Cleanup(func() { tracer = prev })

	if err := withSpan(context.Background(), "processor.store", func(context.Context) error { return errSymbolicationRetry }); !errors.Is(err, errSymbolicationRetry) {
		t.Fatalf("err = %v", err)
	}
	_ = withSpan(context.Background(), "processor.store", func(context.Context) error { return errors.New("db down") })

	ended := rec.Ended()
	if len(ended) != 2 {
		t.Fatalf("%d spans", len(ended))
	}
	if ended[0].Status().Code == codes.Error || len(ended[0].Events()) != 0 {
		t.Errorf("the retry span carries an error status or recorded error: %+v", ended[0].Status())
	}
	found := false
	for _, a := range ended[0].Attributes() {
		found = found || (a.Key == "processor.outcome" && a.Value.AsString() == outcomeSymbolicationRetry)
	}
	if !found {
		t.Error("the retry span lacks processor.outcome=symbolication_retry")
	}
	if ended[1].Status().Code != codes.Error {
		t.Error("a real failure must still mark the span as an error")
	}
}

// The negative-cache bypass is for delivery 2 only (a persistent outage costs one uncached fetch per
// event): delivery 1 and delivery 3 onwards, and a context with no count, use the normal cache.
func TestSymbolicate_FetchErrorCacheBypassOnlyOnDeliveryTwo(t *testing.T) {
	for _, c := range []struct {
		n      uint64
		bypass bool
	}{{1, false}, {2, true}, {3, false}, {7, false}} {
		calls := 0
		s := serviceWithStore(t, func(context.Context, string, string, string) (*symbolicate.Sourcemap, error) {
			calls++
			return &symbolicate.Sourcemap{ID: "m", StorageKey: "k", SizeBytes: 10, MinifiedPath: "/assets/index-3f2a1c.js"}, nil
		})
		ctx := nats.WithDeliveryCount(context.Background(), c.n)
		s.symbolicate(context.Background(), jsEvent(), "p1") // seeds the negative cache
		before := calls
		s.symbolicate(ctx, jsEvent(), "p1")
		if got := calls > before; got != c.bypass {
			t.Errorf("delivery %d: fetched again = %v, want %v", c.n, got, c.bypass)
		}
	}
}
