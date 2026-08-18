package llm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// scriptedChat plays a fixed sequence of Complete responses, matching plan §8's "scripted fake
// Chat" pattern. Calling Complete past the end of the script fails the test loudly (a sentinel
// error) rather than looping — that's how cap tests prove no extra call was made.
type scriptedChat struct {
	steps []func(Request) (Response, error)
	calls int
}

func (s *scriptedChat) Complete(_ context.Context, req Request) (Response, error) {
	if s.calls >= len(s.steps) {
		return Response{}, errors.New("scriptedChat: unexpected call past end of script")
	}
	f := s.steps[s.calls]
	s.calls++
	return f(req)
}

func textStep(text string, usage Usage) func(Request) (Response, error) {
	return func(Request) (Response, error) {
		return Response{Text: text, Usage: usage, StopReason: StopEndTurn}, nil
	}
}

var decisionSchema = Schema{
	Type:     "object",
	Required: []string{"disposition"},
	Properties: map[string]Schema{
		"disposition": {Type: "string", Enum: []string{"comment_only", "fixable"}},
	},
}

func TestRunLoop_ToolRoundTripAndTruncation(t *testing.T) {
	longOutput := strings.Repeat("a", 10_000)
	var sawTruncated bool

	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{
				ToolCalls: []ToolCall{{ID: "1", Name: "get_issue", Arguments: `{}`}},
				Usage:     Usage{InputTokens: 10, OutputTokens: 5},
			}, nil
		},
		func(req Request) (Response, error) {
			// This is the turn right after the tool executed: assert the truncation marker made
			// it into what the model is shown.
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 {
				t.Fatalf("expected 1 tool result in last message, got %d", len(last.ToolResults))
			}
			if !strings.Contains(last.ToolResults[0].Content, "[...truncated]") {
				t.Fatal("expected truncation marker in tool result content")
			}
			sawTruncated = true
			return Response{Text: `{"disposition":"comment_only"}`, Usage: Usage{InputTokens: 20, OutputTokens: 8}}, nil
		},
	}}

	tools := map[string]ToolFunc{
		"get_issue": func(context.Context, string) (string, error) { return longOutput, nil },
	}

	res, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, tools, Caps{ToolResultByteCap: 100}, nil)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if !sawTruncated {
		t.Fatal("second Complete call was never reached")
	}
	if res.Text != `{"disposition":"comment_only"}` {
		t.Fatalf("Text = %q", res.Text)
	}
	if res.Usage.InputTokens != 30 || res.Usage.OutputTokens != 13 {
		t.Fatalf("Usage summed wrong: %+v", res.Usage)
	}
	if res.Turns != 2 {
		t.Fatalf("Turns = %d, want 2", res.Turns)
	}
	if res.Provider != "primary" {
		t.Fatalf("Provider = %q, want primary", res.Provider)
	}
}

func TestRunLoop_UnknownToolReportsErrorWithoutCrashing(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{ToolCalls: []ToolCall{{ID: "1", Name: "nonexistent", Arguments: `{}`}}}, nil
		},
		func(req Request) (Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if !last.ToolResults[0].IsError {
				t.Fatal("unknown tool result should be marked IsError")
			}
			return Response{Text: `{"disposition":"comment_only"}`}, nil
		},
	}}
	_, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, map[string]ToolFunc{}, Caps{}, nil)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
}

func TestRunLoop_MaxTurnsCapTripsBeforeExtraCall(t *testing.T) {
	// Only ONE step is scripted: turn 1 succeeds and returns ToolCalls, so the loop wants a turn
	// 2 — with MaxTurns=1 that must be rejected by the cap BEFORE any second Complete call (proof:
	// if the cap check were missing/wrong, Complete would run past the script and fail with the
	// "unexpected call" sentinel error instead of CapExceededError).
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{ToolCalls: []ToolCall{{ID: "1", Name: "noop", Arguments: `{}`}}}, nil
		},
	}}
	tools := map[string]ToolFunc{"noop": func(context.Context, string) (string, error) { return "ok", nil }}

	_, err := RunLoop(context.Background(), chat, nil, Request{}, tools, Caps{MaxTurns: 1}, nil)
	var capErr *CapExceededError
	if !errors.As(err, &capErr) || capErr.Cap != "max_turns" {
		t.Fatalf("want CapExceededError{max_turns}, got %v (%T)", err, err)
	}
	if chat.calls != 1 {
		t.Fatalf("expected exactly 1 Complete call, got %d", chat.calls)
	}
}

func TestRunLoop_MaxOutputTokensCapTrips(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{"disposition":"comment_only"}`, Usage{OutputTokens: 1000}),
	}}
	_, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{MaxOutputTokens: 500}, nil)
	var capErr *CapExceededError
	if !errors.As(err, &capErr) || capErr.Cap != "max_output_tokens" {
		t.Fatalf("want CapExceededError{max_output_tokens}, got %v (%T)", err, err)
	}
}

func TestRunLoop_ReasksAtMostTwiceThenPermanentError(t *testing.T) {
	invalid := textStep(`{"disposition":"not_a_real_value"}`, Usage{})
	chat := &scriptedChat{steps: []func(Request) (Response, error){invalid, invalid, invalid}}

	_, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, nil)
	var perr *PermanentError
	if !errors.As(err, &perr) {
		t.Fatalf("want *PermanentError, got %v (%T)", err, err)
	}
	// Red-first proof: with maxReasks honored, exactly 3 Complete calls happen (1 initial + 2
	// re-asks); a 4th script step is deliberately absent, so a bug that re-asks a 3rd time would
	// fail with the scriptedChat sentinel error, not PermanentError.
	if chat.calls != 3 {
		t.Fatalf("expected exactly 3 Complete calls (initial + 2 re-asks), got %d", chat.calls)
	}
}

func TestRunLoop_ReaskSucceedsOnSecondAttempt(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{"disposition":"bogus"}`, Usage{}),
		textStep(`{"disposition":"fixable"}`, Usage{}),
	}}
	res, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, nil)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if res.Text != `{"disposition":"fixable"}` {
		t.Fatalf("Text = %q", res.Text)
	}
	if chat.calls != 2 {
		t.Fatalf("expected exactly 2 Complete calls, got %d", chat.calls)
	}
}

func TestRunLoop_ReaskUsageIsSummedIntoResult(t *testing.T) {
	// Kills the mutation that deletes total.InputTokens/OutputTokens += resp.Usage.* on the re-ask
	// path: both scripted steps report non-zero usage, so if the re-ask's usage is dropped the
	// summed total will be short.
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{"disposition":"bogus"}`, Usage{InputTokens: 100, OutputTokens: 50}),
		textStep(`{"disposition":"fixable"}`, Usage{InputTokens: 200, OutputTokens: 75}),
	}}
	res, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, nil)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if res.Usage.InputTokens != 300 || res.Usage.OutputTokens != 125 {
		t.Fatalf("Usage = %+v, want sum of initial+reask (300, 125)", res.Usage)
	}
}

func TestRunLoop_MaxOutputTokensCapTripsDuringReask(t *testing.T) {
	// Kills the mutation that neutralises the post-finalizeDecision cap check: the initial (invalid)
	// turn plus its re-ask (which succeeds validation) together exceed MaxOutputTokens
	// (400+400=800 > 500), but each individual step is under it, so this can only trip if the
	// re-ask's usage is actually added to the running total and re-checked after the decision
	// validates. Only two steps are scripted (the re-ask is valid, so no third reask is needed) —
	// a bug that lets the loop proceed past the cap would fail loudly via scriptedChat's "unexpected
	// call" sentinel instead of silently returning a decision.
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{"disposition":"bogus"}`, Usage{OutputTokens: 400}),
		textStep(`{"disposition":"fixable"}`, Usage{OutputTokens: 400}),
	}}
	_, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{MaxOutputTokens: 500}, nil)
	var capErr *CapExceededError
	if !errors.As(err, &capErr) || capErr.Cap != "max_output_tokens" {
		t.Fatalf("want CapExceededError{max_output_tokens}, got %v (%T)", err, err)
	}
	if chat.calls != 2 {
		t.Fatalf("expected exactly 2 Complete calls (initial + 1 reask), got %d", chat.calls)
	}
}

func TestRunLoop_MissingRequiredFieldFailsValidation(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{}`, Usage{}), textStep(`{}`, Usage{}), textStep(`{}`, Usage{}),
	}}
	_, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, nil)
	var perr *PermanentError
	if !errors.As(err, &perr) {
		t.Fatalf("want *PermanentError for missing required field, got %v (%T)", err, err)
	}
}

// TestRunLoop_StopErrorShortCircuitsBeforeReasking pins the finding: RunLoop used to ignore
// Response.StopReason entirely, so a provider-reported error stop reason (StopError) with
// JSONSchema set would fall straight into finalizeDecision's re-ask loop, burning up to
// maxReasks Complete calls trying to validate text that was never going to pass, before finally
// reporting *PermanentError. It must now short-circuit into *PermanentError immediately, spending
// zero re-asks — the scriptedChat's single scripted step proves this: a second call would fail
// the test outright ("unexpected call past end of script").
func TestRunLoop_StopErrorShortCircuitsBeforeReasking(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{Text: `{"disposition":"comment_only"}`, StopReason: StopError}, nil
		},
	}}
	result, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, nil)
	var perr *PermanentError
	if !errors.As(err, &perr) {
		t.Fatalf("want *PermanentError, got %v (%T)", err, err)
	}
	if chat.calls != 1 {
		t.Fatalf("calls = %d, want 1 (zero re-asks spent)", chat.calls)
	}
	if result.StopReason != StopError {
		t.Fatalf("Result.StopReason = %q, want %q", result.StopReason, StopError)
	}
}

// TestRunLoop_ResultStopReasonReflectsWinningTurn pins Result.StopReason as a new field: it must
// carry the winning (final, no-ToolCalls) turn's StopReason on a normal success path too, not
// just on the StopError short-circuit above.
func TestRunLoop_ResultStopReasonReflectsWinningTurn(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{"disposition":"comment_only"}`, Usage{}),
	}}
	result, err := RunLoop(context.Background(), chat, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, nil)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if result.StopReason != StopEndTurn {
		t.Fatalf("Result.StopReason = %q, want %q", result.StopReason, StopEndTurn)
	}
}

func TestRunLoop_NonTransientErrorNeverFallsBackOrTripsBreaker(t *testing.T) {
	permErr := errors.New("bad request")
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) { return Response{}, permErr },
	}}
	fallback := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{"disposition":"comment_only"}`, Usage{}),
	}}
	breaker := sentinel.NewCircuitBreaker(sentinel.ScopeLLM("testprovider"))

	_, err := RunLoop(context.Background(), chat, fallback, Request{JSONSchema: &decisionSchema}, nil, Caps{}, NewSyncBreaker(breaker))
	if !errors.Is(err, permErr) {
		t.Fatalf("want the raw non-transient error to propagate, got %v", err)
	}
	if fallback.calls != 0 {
		t.Fatal("fallback must not be used for a non-transient error")
	}
	if breaker.State() != sentinel.CircuitClosed {
		t.Fatalf("breaker must stay closed on a non-transient error, got %v", breaker.State())
	}
}

func TestRunLoop_FallbackTakesOverOnlyAfterCircuitOpens(t *testing.T) {
	transientErr := &TransientError{Err: errors.New("upstream 503")}
	breaker := sentinel.NewCircuitBreaker(sentinel.ScopeLLM("testprovider"))

	// Drive the breaker open first, exactly like an independent series of failed loop calls would
	// (5 consecutive failures per sentinel.CircuitBreaker's documented threshold).
	for i := 0; i < 5; i++ {
		breaker.RecordFailure()
	}
	if breaker.State() != sentinel.CircuitOpen {
		t.Fatalf("precondition failed: breaker should be open, got %v", breaker.State())
	}

	primary := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			t.Fatal("primary must not be called while the breaker is open")
			return Response{}, nil
		},
	}}
	fallback := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			_ = transientErr // fallback itself succeeds; transientErr only documents why we got here
			return Response{Text: `{"disposition":"comment_only"}`}, nil
		},
	}}

	res, err := RunLoop(context.Background(), primary, fallback, Request{JSONSchema: &decisionSchema}, nil, Caps{}, NewSyncBreaker(breaker))
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if res.Provider != "fallback" {
		t.Fatalf("Provider = %q, want fallback", res.Provider)
	}
	if primary.calls != 0 {
		t.Fatalf("primary.calls = %d, want 0", primary.calls)
	}
	if fallback.calls != 1 {
		t.Fatalf("fallback.calls = %d, want 1", fallback.calls)
	}
}

func TestRunLoop_TransientFailureRecordsOnBreakerWithoutFallingBackImmediately(t *testing.T) {
	transientErr := &TransientError{Err: errors.New("upstream 503")}
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) { return Response{}, transientErr },
	}}
	fallback := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{"disposition":"comment_only"}`, Usage{}),
	}}
	breaker := sentinel.NewCircuitBreaker(sentinel.ScopeLLM("testprovider"))

	_, err := RunLoop(context.Background(), chat, fallback, Request{JSONSchema: &decisionSchema}, nil, Caps{}, NewSyncBreaker(breaker))
	if !errors.Is(err, transientErr) {
		t.Fatalf("want the transient error to propagate on the first failure (breaker not yet open), got %v", err)
	}
	if fallback.calls != 0 {
		t.Fatal("fallback must not be used before the circuit actually opens")
	}
	if breaker.State() != sentinel.CircuitClosed {
		t.Fatalf("one failure must not open the circuit (threshold is 5), got %v", breaker.State())
	}
}

func TestRunLoop_WallClockTimeout(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			time.Sleep(20 * time.Millisecond)
			return Response{ToolCalls: []ToolCall{{ID: "1", Name: "noop", Arguments: `{}`}}}, nil
		},
		func(Request) (Response, error) {
			t.Fatal("must not reach a second turn once the wall-clock timeout has elapsed")
			return Response{}, nil
		},
	}}
	tools := map[string]ToolFunc{"noop": func(context.Context, string) (string, error) { return "ok", nil }}

	_, err := RunLoop(context.Background(), chat, nil, Request{}, tools, Caps{Timeout: 5 * time.Millisecond}, nil)
	var capErr *CapExceededError
	if !errors.As(err, &capErr) || capErr.Cap != "timeout" {
		t.Fatalf("want CapExceededError{timeout}, got %v (%T)", err, err)
	}
}

func TestRunLoop_NilPrimaryRejected(t *testing.T) {
	_, err := RunLoop(context.Background(), nil, nil, Request{}, nil, Caps{}, nil)
	if err == nil {
		t.Fatal("expected an error for nil primary Chat")
	}
}

// raceChat is a Chat whose Complete alternates transient failure / success across calls, driven by
// an atomic counter, so concurrent RunLoop callers hammer a shared breaker's Allow/RecordSuccess/
// RecordFailure from many goroutines at once (the loop/queue.go WORKER_CONCURRENCY shape the
// blocker finding describes). Safe for concurrent use itself, unlike scriptedChat, precisely so any
// race caught by -race is attributable to the breaker, not to this fake.
type raceChat struct {
	n int64
}

func (c *raceChat) Complete(context.Context, Request) (Response, error) {
	// odd calls fail transiently, even calls succeed — every goroutine sees both outcomes.
	if v := atomic.AddInt64(&c.n, 1); v%2 == 1 {
		return Response{}, &TransientError{Err: errors.New("upstream 503")}
	}
	return Response{Text: `{"disposition":"comment_only"}`}, nil
}

// TestRunLoop_ConcurrentRunLoopsShareBreakerSafely is the loop-concurrency race test the blocker
// finding says was missing: N goroutines run RunLoop concurrently, all sharing ONE breaker gate,
// exactly like loop/queue.go's per-runner goroutines sharing one llm:<provider> breaker
// (WORKER_CONCURRENCY, default 2, main.go:233). Sharing the raw *sentinel.CircuitBreaker across
// these goroutines is a data race (sentinel.CircuitBreaker has no mutex); this test shares a
// *SyncBreaker instead and must pass cleanly under -race — deleting the SyncBreaker wrapping (i.e.
// passing the raw *sentinel.CircuitBreaker to every goroutine here) reproduces the blocker's
// reported WARNING: DATA RACE at RecordSuccess/RecordFailure.
func TestRunLoop_ConcurrentRunLoopsShareBreakerSafely(t *testing.T) {
	raw := sentinel.NewCircuitBreaker(sentinel.ScopeLLM("testprovider"))
	shared := NewSyncBreaker(raw)

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			chat := &raceChat{}
			fallback := &scriptedChat{steps: []func(Request) (Response, error){
				textStep(`{"disposition":"comment_only"}`, Usage{}),
				textStep(`{"disposition":"comment_only"}`, Usage{}),
				textStep(`{"disposition":"comment_only"}`, Usage{}),
			}}
			for j := 0; j < 3; j++ {
				_, _ = RunLoop(context.Background(), chat, fallback, Request{JSONSchema: &decisionSchema}, nil, Caps{}, shared)
			}
		}()
	}
	wg.Wait()
}

func TestRunLoop_UsageReportedOnErrorPaths(t *testing.T) {
	// A turn that reports real spend, then trips MaxTurns=1 on the would-be second turn, must still
	// surface that spend via Result.Usage even though RunLoop also returns an error. Before the
	// fix, every error path returned Result{} and this assertion fails with Usage == Usage{}.
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{
				ToolCalls: []ToolCall{{ID: "1", Name: "noop", Arguments: `{}`}},
				Usage:     Usage{InputTokens: 9000, OutputTokens: 400},
			}, nil
		},
	}}
	tools := map[string]ToolFunc{"noop": func(context.Context, string) (string, error) { return "ok", nil }}

	res, err := RunLoop(context.Background(), chat, nil, Request{}, tools, Caps{MaxTurns: 1}, nil)
	var capErr *CapExceededError
	if !errors.As(err, &capErr) || capErr.Cap != "max_turns" {
		t.Fatalf("want CapExceededError{max_turns}, got %v (%T)", err, err)
	}
	if res.Usage.InputTokens != 9000 || res.Usage.OutputTokens != 400 {
		t.Fatalf("pre-cap spend lost: Usage = %+v, want {9000 400}", res.Usage)
	}
}

func TestValidateAgainstSchema_NullRejectedUnlessNullable(t *testing.T) {
	schema := Schema{
		Type:     "object",
		Required: []string{"disposition", "severity", "confidence"},
		Properties: map[string]Schema{
			"disposition": {Type: "string", Enum: []string{"comment_only", "fixable", "needs_human"}},
			"severity":    {Type: "string", Nullable: true},
			"confidence":  {Type: "number"},
		},
	}

	// All-null must still fail: required fields are null, and none of them is Nullable except
	// severity — so disposition and confidence being null must each be rejected.
	if err := validateAgainstSchema(`{"disposition":null,"severity":null,"confidence":null}`, schema); err == nil {
		t.Fatal("all-null decision must fail validation, got nil error")
	}

	// A single non-nullable required field being null must fail on its own.
	if err := validateAgainstSchema(`{"disposition":null,"severity":"low","confidence":0.5}`, schema); err == nil {
		t.Fatal("null for a non-nullable required field must fail validation")
	}

	// severity is explicitly Nullable, so null there alone must be accepted.
	if err := validateAgainstSchema(`{"disposition":"fixable","severity":null,"confidence":0.9}`, schema); err != nil {
		t.Fatalf("nullable field should accept null, got %v", err)
	}
}

func TestCapExceededError_UnwrapsToPermanentError(t *testing.T) {
	err := error(&CapExceededError{Cap: "max_output_tokens"})
	var capErr *CapExceededError
	var permErr *PermanentError
	if !errors.As(err, &capErr) {
		t.Fatal("errors.As(*CapExceededError) failed")
	}
	if !errors.As(err, &permErr) {
		t.Fatal("errors.As(*PermanentError) failed — CapExceededError must be classified as Permanent (plan §2.4)")
	}
}

func TestRunLoop_CallerCancelDistinctFromOwnTimeout(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{}, errors.New("should not matter, ctx already cancelled")
		},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate SIGTERM-drain caller cancellation, per Drain (main.go:557)

	_, err := RunLoop(ctx, chat, nil, Request{}, nil, Caps{Timeout: time.Hour}, nil)
	var capErr *CapExceededError
	if errors.As(err, &capErr) {
		t.Fatalf("caller cancellation must not be reported as the loop's own cap, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want errors.Is(err, context.Canceled), got %v", err)
	}
}

func TestRunLoop_OwnTimeoutStillReportsCapExceeded(t *testing.T) {
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			time.Sleep(20 * time.Millisecond)
			return Response{ToolCalls: []ToolCall{{ID: "1", Name: "noop", Arguments: `{}`}}}, nil
		},
	}}
	tools := map[string]ToolFunc{"noop": func(context.Context, string) (string, error) { return "ok", nil }}

	_, err := RunLoop(context.Background(), chat, nil, Request{}, tools, Caps{Timeout: 5 * time.Millisecond}, nil)
	var capErr *CapExceededError
	if !errors.As(err, &capErr) || capErr.Cap != "timeout" {
		t.Fatalf("want CapExceededError{timeout} for the loop's own deadline, got %v (%T)", err, err)
	}
}

func TestRunLoop_OpenCircuitNoFallbackPausesInsteadOfHammeringPrimary(t *testing.T) {
	breaker := sentinel.NewCircuitBreaker(sentinel.ScopeLLM("testprovider"))
	for i := 0; i < 5; i++ {
		breaker.RecordFailure()
	}
	if breaker.State() != sentinel.CircuitOpen {
		t.Fatalf("precondition failed: breaker should be open, got %v", breaker.State())
	}

	primary := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			t.Fatal("primary must not be called while the breaker is open and no fallback is configured")
			return Response{}, nil
		},
	}}

	_, err := RunLoop(context.Background(), primary, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, NewSyncBreaker(breaker))
	if primary.calls != 0 {
		t.Fatalf("primary.calls = %d, want 0", primary.calls)
	}
	var circErr *CircuitOpenError
	if !errors.As(err, &circErr) {
		t.Fatalf("want *CircuitOpenError, got %v (%T)", err, err)
	}
	var transient *TransientError
	if !errors.As(err, &transient) {
		t.Fatalf("want the circuit-open error classified Transient (plan §2.4), got %v (%T)", err, err)
	}
}

// openHalfOpenBreaker builds a *sentinel.CircuitBreaker that is open and, per its injected clock,
// already past halfOpenProbeInterval — i.e. State() reports CircuitHalfOpen and the very next
// Allow() grants the single probe slot. Shared by the three wedge-regression tests below.
func openHalfOpenBreaker(t *testing.T) (*sentinel.CircuitBreaker, func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := sentinel.NewCircuitBreaker(sentinel.ScopeLLM("testprovider"))
	b.NowFunc = func() time.Time { return now }
	advance := func(d time.Duration) { now = now.Add(d) }
	for i := 0; i < 5; i++ {
		b.RecordFailure()
	}
	if b.State() != sentinel.CircuitOpen {
		t.Fatalf("precondition failed: breaker should be open, got %v", b.State())
	}
	advance(3 * time.Minute) // > halfOpenProbeInterval (2m)
	if b.State() != sentinel.CircuitHalfOpen {
		t.Fatalf("precondition failed: breaker should be half-open, got %v", b.State())
	}
	return b, advance
}

// assertProbeReleased advances the injected clock past another probe interval and drives one more
// RunLoop against a healthy primary through the same shared breaker, requiring it to actually
// reach primary (i.e. Allow() granted a probe again). Before the fix, the earlier probe's failure
// left `probing` stuck true forever, so this second Allow() always returned false and primary was
// never called — reproducing the blocker's "WEDGED" symptom — regardless of how much time passed.
func assertProbeReleased(t *testing.T, gate BreakerGate, advance func(time.Duration)) {
	t.Helper()
	advance(3 * time.Minute)
	healthy := &scriptedChat{steps: []func(Request) (Response, error){
		textStep(`{"disposition":"comment_only"}`, Usage{}),
	}}
	_, err := RunLoop(context.Background(), healthy, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, gate)
	if err != nil {
		t.Fatalf("RunLoop after probe resolution: %v", err)
	}
	if healthy.calls != 1 {
		t.Fatalf("breaker still wedged: primary.calls = %d, want 1 (Allow() never granted another probe)", healthy.calls)
	}
}

// TestRunLoop_NonTransientProbeFailureReleasesProbe is the blocker's first wedge trigger: a
// 400/401-shaped (non-transient, unwrapped) error on the half-open probe call must still resolve
// the probe via RecordFailure, not just skip it because errors.As(err, &TransientError) fails.
func TestRunLoop_NonTransientProbeFailureReleasesProbe(t *testing.T) {
	raw, advance := openHalfOpenBreaker(t)
	gate := NewSyncBreaker(raw)

	failing := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) { return Response{}, errors.New("400 bad request") },
	}}
	_, err := RunLoop(context.Background(), failing, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{}, gate)
	if err == nil {
		t.Fatal("expected the probe call's error to propagate")
	}
	if failing.calls != 1 {
		t.Fatalf("failing.calls = %d, want 1 (exactly the probe)", failing.calls)
	}
	assertProbeReleased(t, gate, advance)
}

// TestRunLoop_OwnTimeoutDuringProbeReleasesProbe is the blocker's second wedge trigger: RunLoop's
// own wall-clock timeout firing on the probe call rewrites the error to *CapExceededError before
// any errors.As(*TransientError) check could ever match — the probe must still be resolved.
func TestRunLoop_OwnTimeoutDuringProbeReleasesProbe(t *testing.T) {
	raw, advance := openHalfOpenBreaker(t)
	gate := NewSyncBreaker(raw)

	slow := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			time.Sleep(20 * time.Millisecond)
			return Response{}, errors.New("should not matter, timeout wins first")
		},
	}}
	_, err := RunLoop(context.Background(), slow, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{Timeout: 5 * time.Millisecond}, gate)
	var capErr *CapExceededError
	if !errors.As(err, &capErr) || capErr.Cap != "timeout" {
		t.Fatalf("want CapExceededError{timeout}, got %v (%T)", err, err)
	}
	assertProbeReleased(t, gate, advance)
}

// TestRunLoop_CallerCancelDuringProbeReleasesProbe is the blocker's third wedge trigger: a caller
// cancellation (SIGTERM drain) observed on the probe call must still resolve the probe.
func TestRunLoop_CallerCancelDuringProbeReleasesProbe(t *testing.T) {
	raw, advance := openHalfOpenBreaker(t)
	gate := NewSyncBreaker(raw)

	// The ctx must still be live when RunLoop calls breaker.Allow() (otherwise complete()'s
	// upfront ctx.Err() check returns before ever consuming the probe, which would not exercise
	// this wedge at all) and only become Done once the provider call is actually in flight — i.e.
	// the cancellation is observed AS the probe result, matching a SIGTERM drain landing mid-call.
	ctx, cancel := context.WithCancel(context.Background())
	cancelled := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			cancel()
			return Response{}, errors.New("should not matter, ctx cancelled mid-call")
		},
	}}
	_, err := RunLoop(ctx, cancelled, nil, Request{JSONSchema: &decisionSchema}, nil, Caps{Timeout: time.Hour}, gate)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want errors.Is(err, context.Canceled), got %v", err)
	}
	assertProbeReleased(t, gate, advance)
}

// TestRunLoop_RequestFieldsSurviveEveryReCompleteCall is the major finding's regression test:
// every re-Complete RunLoop issues (the ordinary next-turn call AND a finalizeDecision re-ask) must
// carry every Request field the caller set — not just the five fields toolloop.go used to
// reconstruct by hand — so a field like JSONSchemaName (or anything added later) is never silently
// dropped. It fails today only if a call site goes back to field-by-field reconstruction.
func TestRunLoop_RequestFieldsSurviveEveryReCompleteCall(t *testing.T) {
	req := Request{
		System:         "system prompt",
		JSONSchema:     &decisionSchema,
		JSONSchemaName: "triage_decision",
		MaxTokens:      777,
		Tools:          []ToolDef{{Name: "get_issue", Description: "fetch"}},
	}

	var sawTurn2, sawReask Request
	chat := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{ToolCalls: []ToolCall{{ID: "1", Name: "get_issue", Arguments: `{}`}}}, nil
		},
		func(r Request) (Response, error) {
			sawTurn2 = r
			return Response{Text: `{"disposition":"bogus"}`}, nil // fails schema -> triggers a re-ask
		},
		func(r Request) (Response, error) {
			sawReask = r
			return Response{Text: `{"disposition":"fixable"}`}, nil
		},
	}}
	tools := map[string]ToolFunc{"get_issue": func(context.Context, string) (string, error) { return "ok", nil }}

	res, err := RunLoop(context.Background(), chat, nil, req, tools, Caps{}, nil)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if res.Text != `{"disposition":"fixable"}` {
		t.Fatalf("Text = %q", res.Text)
	}

	check := func(label string, r Request) {
		t.Helper()
		if r.System != req.System {
			t.Errorf("%s: System = %q, want %q", label, r.System, req.System)
		}
		if r.JSONSchemaName != req.JSONSchemaName {
			t.Errorf("%s: JSONSchemaName = %q, want %q", label, r.JSONSchemaName, req.JSONSchemaName)
		}
		if r.MaxTokens != req.MaxTokens {
			t.Errorf("%s: MaxTokens = %d, want %d", label, r.MaxTokens, req.MaxTokens)
		}
		if r.JSONSchema != req.JSONSchema {
			t.Errorf("%s: JSONSchema pointer changed", label)
		}
		if len(r.Tools) != len(req.Tools) || (len(req.Tools) > 0 && r.Tools[0].Name != req.Tools[0].Name) {
			t.Errorf("%s: Tools = %+v, want %+v", label, r.Tools, req.Tools)
		}
	}
	check("turn 2", sawTurn2)
	check("re-ask", sawReask)
}

func TestValidateAgainstSchema(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{"valid", `{"disposition":"fixable"}`, false},
		{"not json", `not json`, true},
		{"missing required", `{}`, true},
		{"enum violation", `{"disposition":"nope"}`, true},
		{"wrong type", `{"disposition":42}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgainstSchema(tc.text, decisionSchema)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAgainstSchema(%q) error = %v, wantErr %v", tc.text, err, tc.wantErr)
			}
		})
	}
}

// §4.2-shaped schema (a TRIAGE decision carries a numeric confidence field) used to exercise the
// type-check branches TestValidateAgainstSchema's decisionSchema never touches: boolean, array
// (including Items recursion), null, number-mismatch, and object-mismatch.
var triageSchema = Schema{
	Type:     "object",
	Required: []string{"disposition", "confidence", "auto_fixable", "labels"},
	Properties: map[string]Schema{
		"disposition":  {Type: "string", Enum: []string{"comment_only", "fixable"}},
		"confidence":   {Type: "number"},
		"auto_fixable": {Type: "boolean"},
		"labels":       {Type: "array", Items: &Schema{Type: "string"}},
		"note":         {Type: "null", Nullable: true},
	},
}

func TestValidateAgainstSchema_TypeChecks(t *testing.T) {
	valid := `{"disposition":"fixable","confidence":0.9,"auto_fixable":true,"labels":["a","b"]}`
	cases := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{"all types ok", valid, false},
		// boolean
		{"boolean ok (explicit false)", `{"disposition":"fixable","confidence":0.9,"auto_fixable":false,"labels":[]}`, false},
		{"boolean mismatch", `{"disposition":"fixable","confidence":0.9,"auto_fixable":"true","labels":[]}`, true},
		// array
		{"array ok empty", `{"disposition":"fixable","confidence":0.9,"auto_fixable":true,"labels":[]}`, false},
		{"array mismatch (object where array required)", `{"disposition":"fixable","confidence":0.9,"auto_fixable":true,"labels":{}}`, true},
		{"array element mismatch via Items", `{"disposition":"fixable","confidence":0.9,"auto_fixable":true,"labels":["ok",42]}`, true},
		// number — the exact case the validator finding calls out: a model emitting confidence as a
		// string must be rejected, not silently accepted.
		{"number mismatch (string confidence)", `{"disposition":"fixable","confidence":"0.9","auto_fixable":true,"labels":[]}`, true},
		// object
		{"object mismatch (scalar at root)", `42`, true},
		// null — only valid where Nullable is set
		{"null ok on nullable field", `{"disposition":"fixable","confidence":0.9,"auto_fixable":true,"labels":[],"note":null}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgainstSchema(tc.text, triageSchema)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAgainstSchema(%q) error = %v, wantErr %v", tc.text, err, tc.wantErr)
			}
		})
	}
}

// TestRunLoop_ProviderLabelReflectsWinningTurnNotSticky is the sticky-Provider-label regression
// test (minor finding 3): once a call uses fallback, Result.Provider must not stay "fallback"
// forever — it must name whichever Chat produced the FINAL winning turn. This test drives a
// tool-call turn through fallback (breaker denies primary once), then flips the gate to allow
// primary again for the decision turn, and asserts Provider == "primary". Before the fix,
// `provider` was set once on the fallback branch and never reset on a later primary success, so
// this would report "fallback" even though primary produced the winning turn.
type toggleGate struct {
	allowed []bool // consumed in order by successive Allow() calls
	i       int
}

func (g *toggleGate) Allow() (bool, bool) {
	a := g.allowed[g.i]
	g.i++
	return a, false
}
func (g *toggleGate) RecordSuccess() {}
func (g *toggleGate) RecordFailure() {}

func TestRunLoop_ProviderLabelReflectsWinningTurnNotSticky(t *testing.T) {
	gate := &toggleGate{allowed: []bool{false, true}} // turn 1: primary denied -> fallback; turn 2: primary allowed
	primary := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{Text: `{"disposition":"comment_only"}`}, nil
		},
	}}
	fallback := &scriptedChat{steps: []func(Request) (Response, error){
		func(Request) (Response, error) {
			return Response{ToolCalls: []ToolCall{{ID: "1", Name: "noop", Arguments: `{}`}}}, nil
		},
	}}
	tools := map[string]ToolFunc{"noop": func(context.Context, string) (string, error) { return "ok", nil }}

	res, err := RunLoop(context.Background(), primary, fallback, Request{JSONSchema: &decisionSchema}, tools, Caps{}, gate)
	if err != nil {
		t.Fatalf("RunLoop: %v", err)
	}
	if fallback.calls != 1 {
		t.Fatalf("fallback.calls = %d, want 1 (turn 1 only)", fallback.calls)
	}
	if primary.calls != 1 {
		t.Fatalf("primary.calls = %d, want 1 (turn 2 only)", primary.calls)
	}
	if res.Provider != "primary" {
		t.Fatalf("Provider = %q, want %q (primary produced the winning turn)", res.Provider, "primary")
	}
}

// TestRunLoop_MaxTurnsZeroValueIsDefensivelyBounded proves Caps{} (MaxTurns unset) does not let a
// model that always emits ToolCalls loop forever: a chat scripted with defaultMaxTurns+1 identical
// ToolCalls turns must trip CapExceededError{max_turns} instead of running off the end of the
// script (which would fail with scriptedChat's "unexpected call" sentinel error instead of a cap
// error — the red-first proof that the defensive default, not just the documented "0 = unlimited",
// is actually enforced).
func TestRunLoop_MaxTurnsZeroValueIsDefensivelyBounded(t *testing.T) {
	toolCallStep := func(Request) (Response, error) {
		return Response{ToolCalls: []ToolCall{{ID: "1", Name: "noop", Arguments: `{}`}}}, nil
	}
	steps := make([]func(Request) (Response, error), defaultMaxTurns)
	for i := range steps {
		steps[i] = toolCallStep
	}
	chat := &scriptedChat{steps: steps}
	tools := map[string]ToolFunc{"noop": func(context.Context, string) (string, error) { return "ok", nil }}

	_, err := RunLoop(context.Background(), chat, nil, Request{}, tools, Caps{}, nil)
	var capErr *CapExceededError
	if !errors.As(err, &capErr) || capErr.Cap != "max_turns" {
		t.Fatalf("want CapExceededError{max_turns} from the defensive default, got %v (%T)", err, err)
	}
	if chat.calls != defaultMaxTurns {
		t.Fatalf("expected exactly %d Complete calls (the defensive cap), got %d", defaultMaxTurns, chat.calls)
	}
}

// TestValidateAgainstSchema_TypeNullImpliesNullable is finding 4's regression test: a property
// declared Type:"null" without also setting Nullable must still accept a JSON null value — Type
// alone is enough to make the field nullable, matching the doc comment on validateValue's object
// case. Before the fix, this property was unsatisfiable: no value ever passes (non-null values fail
// the "null" type check; null itself is intercepted by the val==nil branch before validateValue's
// "null" case ever runs, and that branch only exempted Nullable, not Type=="null").
func TestValidateAgainstSchema_TypeNullImpliesNullable(t *testing.T) {
	schema := Schema{
		Type: "object",
		Properties: map[string]Schema{
			"note": {Type: "null"}, // Nullable deliberately left unset
		},
	}
	if err := validateAgainstSchema(`{"note":null}`, schema); err != nil {
		t.Fatalf("Type:\"null\" property should accept null without Nullable set, got %v", err)
	}
}

// TestTruncateBytes_DoesNotSplitMultiByteRune is finding 6's regression test: truncating at a byte
// count that falls mid-rune must walk back to a rune boundary instead of slicing through it,
// leaving the result valid UTF-8. "日本語" is 3 bytes per rune; cutting at 4 bytes lands squarely
// inside the second rune if truncateBytes does a naive s[:n].
func TestTruncateBytes_DoesNotSplitMultiByteRune(t *testing.T) {
	s := "日本語" // 9 bytes, 3 runes of 3 bytes each
	out, truncated := truncateBytes(s, 4)
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if !utf8.ValidString(out) {
		t.Fatalf("truncateBytes produced invalid UTF-8: %q (bytes %v)", out, []byte(out))
	}
	if out != "日" {
		t.Fatalf("out = %q, want exactly the first complete rune %q", out, "日")
	}
}

func TestValidateValue_UnsupportedTypeHitsDefaultBranch(t *testing.T) {
	err := validateValue("x", Schema{Type: "not_a_real_type"}, "$")
	if err == nil {
		t.Fatal("want error for unsupported schema type, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported schema type") {
		t.Fatalf("error = %v, want mention of unsupported schema type", err)
	}
}
