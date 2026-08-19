package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// ToolFunc is one harness-side tool the loop can execute on the model's behalf. arguments is the
// raw JSON object the model produced for this call (ToolCall.Arguments); the returned string is
// fed back to the model as the tool result's Content (after truncation — see Caps.ToolResultByteCap).
type ToolFunc func(ctx context.Context, arguments string) (string, error)

// defaultToolResultByteCap is the per-tool result truncation ceiling used when Caps.ToolResultByteCap
// is unset (plan §4.1: "Tool results are truncated to per-tool byte caps ... stacktraces: first N
// frames + tail" — the frame-aware truncation itself is a tool's own concern; this is the harness's
// last-resort hard byte cap so no single tool result can blow the loop's context budget).
const defaultToolResultByteCap = 4000

// defaultMaxTurns is the turn ceiling RunLoop applies when Caps.MaxTurns is left at its zero value.
// Caps{} is documented (see Caps below) as "0 == unlimited" for both MaxTurns and MaxOutputTokens —
// but unlike MaxOutputTokens (whose worst case is one adapter reporting a large-but-finite number),
// an actually-unbounded MaxTurns lets a misbehaving model that always emits ToolCalls drive RunLoop
// into a paid, wall-clock-unbounded (absent a Timeout) request loop forever. So, mirroring
// ToolResultByteCap's own "<=0 uses a default..." pattern, a zero/negative MaxTurns is NOT passed
// through as literally infinite: it is defensively capped at defaultMaxTurns. Callers who need a
// smaller ceiling still set Caps.MaxTurns explicitly; this only guards the case where it was left
// unset.
const defaultMaxTurns = 50

// maxReasks is the plan §4.1 re-ask ceiling: "validated ... and re-asked at most twice with the
// validation error (then Permanent)". This is not configurable — it is a fixed harness policy.
const maxReasks = 2

// Caps bounds one RunLoop invocation (plan §2.6, enforced in llm/toolloop.go):
//   - MaxTurns: the while-ToolCalls loop's turn ceiling (a "turn" is one Complete call that
//     returns ToolCalls and gets re-invoked; the final non-tool-call turn also counts). The zero
//     value is documented as "unlimited", but RunLoop does NOT take that literally: a zero/negative
//     MaxTurns is defensively capped at defaultMaxTurns (see its doc comment) so a caller that
//     forgets to set this cannot be driven into a paid, unbounded request loop by a model that
//     always emits ToolCalls. Set MaxTurns explicitly to a smaller (or, deliberately, a larger)
//     ceiling than defaultMaxTurns as needed. Re-ask Complete calls (bounded by maxReasks) are
//     NOT counted as turns and are not bounded by MaxTurns — MaxOutputTokens and Timeout are the
//     caps that cover them; Result.Turns likewise undercounts total Complete calls by up to
//     maxReasks.
//   - MaxOutputTokens: adapter-reported OutputTokens summed across every Complete call this loop
//     makes (including re-asks); exceeding it ends the loop with a CapExceededError. Unlike
//     MaxTurns, the zero value here IS literally unlimited — no defensive default is applied — so a
//     caller relying on MaxTurns alone to bound spend should still set this explicitly.
//   - Timeout: wall-clock budget for the whole loop, honored via ctx (a zero Timeout means the
//     caller's ctx alone governs).
//   - ToolResultByteCap: per-tool-result truncation cap in bytes; <=0 uses defaultToolResultByteCap.
type Caps struct {
	MaxTurns          int
	MaxOutputTokens   int
	Timeout           time.Duration
	ToolResultByteCap int
}

// Result is what RunLoop returns on success: the final validated decision text, the loop's summed
// usage, how many turns it took, which Chat (primary/fallback) produced the winning turn, and the
// winning turn's StopReason (the last non-ToolCalls turn's Response.StopReason — including a
// re-ask's, if finalizeDecision re-asked; the empty string if RunLoop never reached a final turn).
type Result struct {
	Text       string
	Usage      Usage
	Turns      int
	Provider   string // "primary" | "fallback" — plan §4.1: "the result notes which acted"
	StopReason string
}

// PermanentError is a terminal RunLoop failure (plan §2.4's Permanent class): caps exceeded, a
// tool loop that never converges, or a decision that still fails schema validation after
// maxReasks re-asks. Callers (jobs/*) journal this as failed, not retried.
type PermanentError struct {
	Reason string
}

func (e *PermanentError) Error() string { return "llm: permanent: " + e.Reason }

// CapExceededError is a PermanentError sub-case naming which cap tripped, so callers/tests can
// distinguish "ran out of turns" from "ran out of token budget" from "timed out" without string
// matching Reason. It wraps a *PermanentError (Unwrap) so errors.As(err, &permErr) finds it too —
// callers following plan §2.4's Permanent-class routing must treat every cap breach as
// never-auto-retry, not merely "unclassified".
type CapExceededError struct {
	Cap string // "max_turns" | "max_output_tokens" | "timeout"
}

func (e *CapExceededError) Error() string { return "llm: cap exceeded: " + e.Cap }
func (e *CapExceededError) Unwrap() error { return &PermanentError{Reason: "cap exceeded: " + e.Cap} }

// CircuitOpenError reports that the llm:<provider> circuit is open and no fallback Chat is
// configured, so RunLoop refused to call primary at all (plan §2.4: an open circuit "pauses brain
// jobs; fallback provider takes over IF CONFIGURED" — pausing, not hammering primary, is the base
// behaviour). It is always wrapped in a *TransientError so the dispatcher re-queues the job instead
// of journaling it as a permanent failure.
type CircuitOpenError struct{}

func (e *CircuitOpenError) Error() string {
	return "llm: circuit open, no fallback configured"
}

// BreakerGate is the minimal circuit-breaker surface RunLoop needs (Allow/RecordSuccess/
// RecordFailure). It exists so RunLoop does not take a *sentinel.CircuitBreaker directly: even
// though sentinel.CircuitBreaker (sentinel/retry.go) is now internally mutex-guarded and safe for
// concurrent Allow/RecordSuccess/RecordFailure/State calls, BreakerGate additionally needs
// wasProbe — atomically detecting "this Allow() call consumed the single half-open probe slot" —
// which the raw type does not expose. Callers share a *SyncBreaker (below) across concurrent
// RunLoop calls for that reason, not because the raw type would race.
// BreakerGate.Allow reports whether a call may proceed, and separately whether granting it
// consumed the breaker's single half-open probe slot. wasProbe matters because
// sentinel.CircuitBreaker only clears its probing flag via RecordSuccess/RecordFailure (see
// SyncBreaker's doc comment) — a caller that is granted a probe MUST eventually call one of those,
// on every outcome (including non-transient errors and context cancellation), or the breaker is
// wedged open forever.
type BreakerGate interface {
	Allow() (allowed bool, wasProbe bool)
	RecordSuccess()
	RecordFailure()
}

// SyncBreaker wraps a *sentinel.CircuitBreaker to additionally serialize the State()+Allow() pair
// SyncBreaker.Allow performs, so the wasProbe detection below is atomic even though the underlying
// sentinel.CircuitBreaker is itself safe for plain concurrent use. Build one per configured
// provider via NewSyncBreaker(sentinel.NewCircuitBreaker(sentinel.ScopeLLM(provider))) and pass the
// same *SyncBreaker to every RunLoop call for that provider.
type SyncBreaker struct {
	mu sync.Mutex
	b  *sentinel.CircuitBreaker
}

// NewSyncBreaker wraps b for safe concurrent use. b must be non-nil.
func NewSyncBreaker(b *sentinel.CircuitBreaker) *SyncBreaker {
	return &SyncBreaker{b: b}
}

// Allow reports whether a call may proceed, and whether doing so consumed the breaker's single
// half-open probe slot. It detects the probe by comparing State() (which resolves Open->HalfOpen
// only when no probe is currently in flight and the probe interval has elapsed — exactly the
// condition under which the underlying Allow() grants the one probe call) taken just before the
// call against the boolean Allow() itself returns, all under the same lock so the two reads are
// consistent. sentinel.CircuitBreaker exposes no other way to observe its unexported `probing`
// flag, and this package must not restructure that N8a-validated package to add one.
func (s *SyncBreaker) Allow() (allowed bool, wasProbe bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.b.State()
	allowed = s.b.Allow()
	if !allowed {
		return false, false
	}
	wasProbe = before == sentinel.CircuitHalfOpen
	return allowed, wasProbe
}

func (s *SyncBreaker) RecordSuccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.RecordSuccess()
}

func (s *SyncBreaker) RecordFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.RecordFailure()
}

// TransientError marks a Chat.Complete failure as belonging to the plan §2.4 Transient class —
// adapters (llm/<provider>.go, a later stage) wrap network/5xx/timeout failures in this so RunLoop
// knows to record it against the circuit breaker and consider falling back, instead of treating
// every error identically. An unwrapped error from Complete is treated as non-transient (fails the
// loop outright, no fallback, no breaker involvement) — adapters must opt in explicitly.
type TransientError struct {
	Err error
}

func (e *TransientError) Error() string { return "llm: transient: " + e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// RunLoop drives the plan §4.1 "while resp.ToolCalls" loop: it calls chat.Complete, executes any
// requested tools via the tools map, appends results, and re-Completes, until the model returns a
// turn with no ToolCalls (its final decision) or a cap trips.
//
// primary is required; fallback is optional (nil disables fallback). breaker, when non-nil, gates
// and records primary-call outcomes under its scope. Build it via
// llm.NewSyncBreaker(sentinel.NewCircuitBreaker(sentinel.ScopeLLM(provider))), one *SyncBreaker per
// configured provider, and pass the SAME *SyncBreaker to every concurrent RunLoop call for that
// provider — loop/queue.go runs Advisor loops "concurrently across N runners" (WORKER_CONCURRENCY);
// SyncBreaker exists so the wasProbe detection stays atomic under that concurrency (the underlying
// sentinel.CircuitBreaker is itself safe for plain concurrent Allow/RecordSuccess/RecordFailure).
// Plan §4.1:
// "on transient-class primary failure after the circuit opens ... the fallback takes over".
// Concretely: before each Complete, RunLoop asks breaker.Allow(); while the breaker is closed (or
// half-open, probing primary), the call goes to primary, and a TransientError result is recorded
// via breaker.RecordFailure() (a run of these is what opens the circuit — it does NOT by itself
// reroute THIS call to fallback, matching "after the circuit opens"). Once the breaker is open,
// Allow() returns false; if fallback is configured, the call routes to fallback instead, with
// Provider reported as "fallback" on a successful outcome. On an ERROR return, Result.Provider
// names the last Chat that SUCCEEDED, not the one whose call failed. If fallback is nil, RunLoop makes no
// primary call at all and returns a *TransientError wrapping *CircuitOpenError instead (plan §2.4:
// an open circuit "pauses brain jobs" — the base behaviour without a configured fallback — rather
// than hammering a primary that just failed 5 consecutive times). A successful primary call while
// the breaker allowed it records a success (closing the circuit if it was half-open).
//
// When req.JSONSchema is set, the final (no-ToolCalls) turn's Text is validated against it (see
// validateAgainstSchema for exactly which JSON-schema subset is enforced); a failing validation is
// re-asked (the invalid text plus the validation error appended as a fresh user turn) at most
// maxReasks times, after which RunLoop returns a *PermanentError.
func RunLoop(ctx context.Context, primary, fallback Chat, req Request, tools map[string]ToolFunc, caps Caps, breaker BreakerGate) (Result, error) {
	if primary == nil {
		return Result{}, errors.New("llm: RunLoop requires a non-nil primary Chat")
	}
	parentCtx := ctx
	ownTimeout := caps.Timeout > 0
	if ownTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, caps.Timeout)
		defer cancel()
	}

	byteCap := caps.ToolResultByteCap
	if byteCap <= 0 {
		byteCap = defaultToolResultByteCap
	}
	maxTurns := caps.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	msgs := append([]Msg(nil), req.Messages...)
	var total Usage
	provider := "primary"
	turns := 0

	// classifyCtxErr distinguishes the caller's own cancellation/deadline (parentCtx) from the
	// wall-clock cap RunLoop itself installed (ownTimeout): only the latter is reported as
	// CapExceededError{"timeout"}; the former is returned wrapped so errors.Is(err,
	// context.Canceled) / errors.Is(err, context.DeadlineExceeded) still holds for the caller's own
	// condition, matching the crash-resume story (plan §4.4) rather than journaling a shutdown as a
	// permanent cap breach.
	classifyCtxErr := func(err error) error {
		if perr := parentCtx.Err(); perr != nil {
			return fmt.Errorf("llm: context ended: %w", perr)
		}
		if ownTimeout && errors.Is(err, context.DeadlineExceeded) {
			return &CapExceededError{Cap: "timeout"}
		}
		return fmt.Errorf("llm: context ended: %w", err)
	}

	complete := func(r Request) (Response, error) {
		if err := ctx.Err(); err != nil {
			return Response{}, classifyCtxErr(err)
		}
		chat := primary
		usedFallback := false
		wasProbe := false
		if breaker != nil {
			var allowed bool
			allowed, wasProbe = breaker.Allow()
			if !allowed {
				if fallback == nil {
					return Response{}, &TransientError{Err: &CircuitOpenError{}}
				}
				chat = fallback
				usedFallback = true
			}
		}
		resp, err := chat.Complete(ctx, r)
		if err == nil {
			// Set the label per-call (not just once, "sticky"), so Result.Provider always names
			// the Chat that produced the WINNING (last successful) turn: a primary call that
			// succeeds after an earlier turn used fallback must flip the label back to "primary",
			// not leave it stuck at "fallback" from the earlier turn.
			if usedFallback {
				provider = "fallback"
			} else {
				provider = "primary"
				if breaker != nil {
					breaker.RecordSuccess()
				}
			}
			return resp, nil
		}
		// Resolve any half-open probe this call consumed BEFORE the error is reclassified below,
		// and on every outcome — not just *TransientError — so a 400/401, a caller cancellation,
		// or RunLoop's own timeout can never leave the breaker's probing flag stuck true (which
		// would wedge Allow() into returning false forever; see this file's package doc). For an
		// already-open breaker, RecordFailure only resets openedAt/clears probing and does NOT
		// touch consecutiveFailures, so a non-transient probe failure still can't open a closed
		// circuit — TestRunLoop_NonTransientErrorNeverFallsBackOrTripsBreaker's property holds.
		if !usedFallback && breaker != nil && wasProbe {
			breaker.RecordFailure()
		}
		if cerr := ctx.Err(); cerr != nil {
			err = classifyCtxErr(cerr)
		}
		if !usedFallback && breaker != nil && !wasProbe {
			var transient *TransientError
			if errors.As(err, &transient) {
				breaker.RecordFailure()
			}
		}
		return Response{}, err
	}

	result := func() Result {
		return Result{Usage: total, Turns: turns, Provider: provider}
	}

	for {
		turns++
		if turns > maxTurns {
			return result(), &CapExceededError{Cap: "max_turns"}
		}

		turnReq := req
		turnReq.Messages = msgs
		resp, err := complete(turnReq)
		if err != nil {
			return result(), err
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		if caps.MaxOutputTokens > 0 && total.OutputTokens > caps.MaxOutputTokens {
			return result(), &CapExceededError{Cap: "max_output_tokens"}
		}

		if len(resp.ToolCalls) == 0 {
			if resp.StopReason == StopError {
				// Short-circuit BEFORE finalizeDecision spends any re-asks: a provider-reported
				// error stop reason (e.g. gemini/anthropic's safety/content-block mapping, or a
				// backend that never recovers) is never going to pass schema validation by asking
				// again, so re-asking against it would just burn maxReasks calls for no benefit —
				// plan §4.1's re-ask budget is for a well-formed-but-invalid decision, not this.
				r := result()
				r.StopReason = resp.StopReason
				return r, &PermanentError{Reason: "llm: provider reported an error stop reason"}
			}
			text, usage, stopReason, err := finalizeDecision(complete, msgs, req, resp.Text, resp.StopReason)
			total.InputTokens += usage.InputTokens
			total.OutputTokens += usage.OutputTokens
			if caps.MaxOutputTokens > 0 && total.OutputTokens > caps.MaxOutputTokens {
				r := result()
				r.StopReason = stopReason
				return r, &CapExceededError{Cap: "max_output_tokens"}
			}
			if err != nil {
				r := result()
				r.StopReason = stopReason
				return r, err
			}
			r := result()
			r.StopReason = stopReason
			r.Text = text
			return r, nil
		}

		msgs = append(msgs, Msg{Role: RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})
		results := make([]ToolResult, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			results = append(results, runTool(ctx, tools, tc, byteCap))
		}
		msgs = append(msgs, Msg{Role: RoleTool, ToolResults: results})
	}
}

// runTool executes one ToolCall (or reports "unknown tool" if it isn't registered), truncating an
// oversized result to byteCap bytes with a trailing marker noting the truncation (plan §4.1:
// "truncation is enforced in the harness, not requested in the prompt").
func runTool(ctx context.Context, tools map[string]ToolFunc, tc ToolCall, byteCap int) ToolResult {
	fn, ok := tools[tc.Name]
	if !ok {
		return ToolResult{ToolCallID: tc.ID, Content: fmt.Sprintf("unknown tool: %s", tc.Name), IsError: true}
	}
	out, err := fn(ctx, tc.Arguments)
	isErr := false
	if err != nil {
		out = err.Error()
		isErr = true
	}
	out, truncated := truncateBytes(out, byteCap)
	if truncated {
		out += "\n[...truncated]"
	}
	return ToolResult{ToolCallID: tc.ID, Content: out, IsError: isErr}
}

// truncateBytes cuts s to at most n bytes, reporting whether it did. The cut point is walked back
// to a UTF-8 rune boundary (never splitting a multi-byte rune in half) — a naive s[:n] byte slice
// can land mid-rune for non-ASCII tool output (stacktraces/log lines with non-ASCII identifiers,
// paths, or messages are common), producing invalid UTF-8 that corrupts whatever the caller does
// with it next (JSON-encode it into a Msg, log it, …).
func truncateBytes(s string, n int) (string, bool) {
	if n <= 0 || len(s) <= n {
		return s, false
	}
	cut := n
	// utf8.RuneStart(b) is true for an ASCII byte or the first byte of a multi-byte rune; false for
	// a continuation byte (10xxxxxx). Walk back at most utf8.UTFMax-1 bytes to find one.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// finalizeDecision validates text against req.JSONSchema (a no-op success if JSONSchema is nil)
// and, on validation failure, re-asks the model up to maxReasks times with the validation error
// appended as a fresh user turn, returning the summed usage of those re-ask calls, the winning
// (or last-attempted) turn's StopReason, and the final text or a *PermanentError. stopReason is
// the initiating turn's Response.StopReason (RunLoop's caller already short-circuits StopError
// before calling this, so on entry it is never StopError); a re-ask response that itself reports
// StopError is likewise short-circuited immediately rather than validated/re-asked further.
func finalizeDecision(complete func(Request) (Response, error), msgs []Msg, req Request, text, stopReason string) (string, Usage, string, error) {
	var total Usage
	if req.JSONSchema == nil {
		return text, total, stopReason, nil
	}
	for attempt := 0; ; attempt++ {
		verr := validateAgainstSchema(text, *req.JSONSchema)
		if verr == nil {
			return text, total, stopReason, nil
		}
		if attempt >= maxReasks {
			return "", total, stopReason, &PermanentError{Reason: fmt.Sprintf("decision failed schema validation after %d re-asks: %v", maxReasks, verr)}
		}
		msgs = append(msgs,
			Msg{Role: RoleAssistant, Text: text},
			Msg{Role: RoleUser, Text: "Your previous response did not match the required schema: " + verr.Error() + ". Respond again with a corrected decision matching the schema."},
		)
		reaskReq := req
		reaskReq.Messages = msgs
		resp, err := complete(reaskReq)
		if err != nil {
			return "", total, stopReason, err
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		text = resp.Text
		stopReason = resp.StopReason
		if stopReason == StopError {
			return "", total, stopReason, &PermanentError{Reason: "llm: provider reported an error stop reason during re-ask"}
		}
	}
}

// validateAgainstSchema enforces a deliberately minimal JSON-schema subset against text (parsed as
// JSON first — a parse failure is itself a validation error): object/array/string/number/boolean/
// null type checks, object "required" field presence, and string "enum" membership. It does NOT
// enforce: numeric ranges (minimum/maximum), string patterns/formats/length, array length/uniqueness,
// additionalProperties, oneOf/anyOf/allOf, or $ref. This is intentionally narrow — plan §4.1 asks
// only for "required fields, enum membership, type checks" and documenting the boundary here is
// the spec for it.
//
// Nullability is explicit, not implicit: JSON null is valid for a property only when that
// property's Schema.Nullable is true (a required field with Nullable unset is NOT satisfied by
// null — required-presence and non-null-ness are both enforced). Without this, null would silently
// pass type/enum checks for every field, including required ones, which is exactly the value a
// confused model is most likely to emit for the field it's unsure about.
func validateAgainstSchema(text string, schema Schema) error {
	var data any
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return fmt.Errorf("response is not valid JSON: %w", err)
	}
	return validateValue(data, schema, "$")
}

func validateValue(v any, schema Schema, path string) error {
	switch schema.Type {
	case "", "object":
		obj, ok := v.(map[string]any)
		if !ok {
			if schema.Type == "" {
				return nil // untyped schema node: only recurse if it looks like an object
			}
			return fmt.Errorf("%s: expected object, got %T", path, v)
		}
		for _, req := range schema.Required {
			if _, present := obj[req]; !present {
				return fmt.Errorf("%s: missing required field %q", path, req)
			}
		}
		for name, propSchema := range schema.Properties {
			val, present := obj[name]
			if !present {
				continue // required-ness already checked above; absent optional fields are fine
			}
			if val == nil {
				// Type:"null" implies Nullable here: without this, a property declared
				// Type:"null" (without also setting Nullable) would be unsatisfiable — the only
				// value that could ever match Type:"null" (JSON null) never reaches
				// validateValue's "null" case at all, because it's intercepted right here first.
				if propSchema.Nullable || propSchema.Type == "null" {
					continue // explicitly nullable: null satisfies this field, required or not
				}
				return fmt.Errorf("%s.%s: null is not allowed for this field", path, name)
			}
			if err := validateValue(val, propSchema, path+"."+name); err != nil {
				return err
			}
		}
		return nil
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: expected string, got %T", path, v)
		}
		if len(schema.Enum) > 0 && !stringInSlice(s, schema.Enum) {
			return fmt.Errorf("%s: %q is not one of %v", path, s, schema.Enum)
		}
		return nil
	case "number":
		if _, ok := v.(float64); !ok {
			return fmt.Errorf("%s: expected number, got %T", path, v)
		}
		return nil
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: expected boolean, got %T", path, v)
		}
		return nil
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: expected array, got %T", path, v)
		}
		if schema.Items != nil {
			for i, elem := range arr {
				if err := validateValue(elem, *schema.Items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
		return nil
	case "null":
		if v != nil {
			return fmt.Errorf("%s: expected null, got %T", path, v)
		}
		return nil
	default:
		return fmt.Errorf("%s: unsupported schema type %q", path, schema.Type)
	}
}

func stringInSlice(s string, list []string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
