package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestNew_UnknownProvider(t *testing.T) {
	_, err := New("chatgpt-9000", Config{})
	var uerr *UnknownProviderError
	if !errors.As(err, &uerr) {
		t.Fatalf("want *UnknownProviderError, got %v (%T)", err, err)
	}
}

func TestNew_KnownButNotImplemented(t *testing.T) {
	// openai.go, anthropic.go, and gemini.go (all N8b) register real adapters via init(); none of
	// the three recognized provider names is left unimplemented as of this stage. This test is
	// kept (with an empty table) as the documented home for that assertion — a future provider
	// name added to knownProviders without a registered factory would belong here.
	for _, p := range []string{} {
		_, err := New(p, Config{Model: "x"})
		var nerr *NotImplementedError
		if !errors.As(err, &nerr) {
			t.Fatalf("provider %s: want *NotImplementedError, got %v (%T)", p, err, err)
		}
		if nerr.Provider != p {
			t.Fatalf("provider %s: NotImplementedError.Provider = %q", p, nerr.Provider)
		}
	}
}

func TestNew_KnownProvidersAreAllRegistered(t *testing.T) {
	for _, p := range []string{"openai", "anthropic", "gemini"} {
		chat, err := New(p, Config{Model: "x", APIKey: "y"})
		if err != nil {
			t.Fatalf("provider %s: New: %v", p, err)
		}
		if chat == nil {
			t.Fatalf("provider %s: New returned nil Chat with nil error", p)
		}
	}
}

func TestNew_RegisteredFactory(t *testing.T) {
	// Use a synthetic provider name rather than "openai"/"anthropic"/"gemini": those three now
	// carry real init()-registered factories (llm/openai.go, llm/anthropic.go, llm/gemini.go), and
	// overwriting-then-deleting one of those entries would leave it unregistered for every test
	// that runs afterward in this package (Go runs a package's tests in a single process).
	const providerName = "registered-factory-test-provider"
	t.Cleanup(func() {
		providerFactoriesMu.Lock()
		delete(providerFactories, providerName)
		providerFactoriesMu.Unlock()
	})
	sentinelChat := fakeChat{}
	RegisterProvider(providerName, func(cfg Config) (Chat, error) { return sentinelChat, nil })

	chat, err := New(providerName, Config{Model: "gpt-x"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if chat == nil {
		t.Fatal("New returned nil Chat with nil error")
	}
}

type fakeChat struct{}

func (fakeChat) Complete(_ context.Context, _ Request) (Response, error) {
	return Response{Text: "ok"}, nil
}

// TestRegisterProvider_ConcurrentRegisterAndNewIsRace-safe is finding 7's regression test:
// providerFactories is a package-global mutated by the exported RegisterProvider, read by New —
// many goroutines calling both concurrently (adapter package init()s, concurrent test packages,
// or later a concurrently-configured multi-provider caller) must not race. Run with -race; before
// the sync.RWMutex guard this reliably reports WARNING: DATA RACE on the bare map.
func TestRegisterProvider_ConcurrentRegisterAndNew(t *testing.T) {
	const providerName = "race-test-provider"
	t.Cleanup(func() {
		providerFactoriesMu.Lock()
		delete(providerFactories, providerName)
		providerFactoriesMu.Unlock()
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			RegisterProvider(providerName, func(cfg Config) (Chat, error) { return fakeChat{}, nil })
		}()
		go func() {
			defer wg.Done()
			_, _ = New(providerName, Config{})
		}()
	}
	wg.Wait()
}
