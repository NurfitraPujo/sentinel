package sentinel

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// FailureClass is the plan §2.4 failure taxonomy: the outcome of classifying either an envelope
// HTTP status or a single batch op's results[i].status through the same table (C3).
type FailureClass int

const (
	// ClassSuccess covers a 2xx envelope/op AND a `deduplicated: true` op result (plan §2.3: "a
	// replay landed on its original write" counts as success, not a fresh failure).
	ClassSuccess FailureClass = iota
	ClassRateLimited
	ClassTransient
	ClassConflictForeign     // foreign-claimant 409 (C1) -> skipped(foreign-claim)
	ClassConflictDroppable   // relation 409 (already-exists/cycle) -> drop that op
	ClassConflictKeyMismatch // idempotency-key op-mismatch 409 -> permanent client bug, failed
	ClassPermanent
	ClassAuthFailure
	ClassGone // issue_deleted tombstone or 404 mid-job
)

// OpResult is the minimal shape of one entry in a batch response's results[] array (plan §2.3),
// enough for Classify to make its call without depending on the full batch response schema.
type OpResult struct {
	Status       int
	Deduplicated bool
	// Op identifies which agent-op this result belongs to (e.g. "issues.claim",
	// "issues.relations.add"), needed to distinguish the conflict sub-classes above.
	Op string
}

// BatchOpResult is the REAL wire shape of one entry in POST /api/agent/batch's results[] array
// (apps/dashboard-web/src/routes/api/agent/batch/+server.ts:44-54): `{ok, status, result?, error?,
// skipped?, claimedBy?, claimedAt?}`. Critically, `deduplicated` is NOT a sibling of `status` -- it
// is a field INSIDE `result` (the op's own success body, e.g. agent-ops.ts:252
// `{comment, deduplicated: true}`), so it must be decoded out of Result, not read as a top-level
// field the way the old (wrong) OpResult modeled it.
type BatchOpResult struct {
	Ok        bool            `json:"ok"`
	Status    int             `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Skipped   bool            `json:"skipped,omitempty"`
	ClaimedBy *string         `json:"claimedBy,omitempty"`
	ClaimedAt *string         `json:"claimedAt,omitempty"`
}

// Deduplicated reports whether this op's own result body carries `deduplicated: true` (C4: a
// replay landing on its original write). Malformed/absent Result decodes to false, matching a
// fresh (non-deduplicated) result.
func (r BatchOpResult) Deduplicated() bool {
	var d struct {
		Deduplicated bool `json:"deduplicated"`
	}
	_ = json.Unmarshal(r.Result, &d)
	return d.Deduplicated
}

// BatchResponse is the POST /api/agent/batch response envelope
// (apps/dashboard-web/src/routes/api/agent/batch/+server.ts).
type BatchResponse struct {
	Results   []BatchOpResult `json:"results"`
	Completed int             `json:"completed"`
}

// BatchOpMeta disambiguates one op's index for ClassifyBatch's per-op walk, since the server's
// results[] entries carry no `op` field to identify themselves by (BatchOpResult above has none) --
// the caller must correlate index i of the response with index i of the request it sent.
type BatchOpMeta struct {
	IsClaimOp    bool
	IsRelationOp bool
	IsSeverityOp bool
}

// ClassifyEnvelope applies the plan §2.4 table to a top-level (non-batch) response status. Only
// the plan §2.3 claim route (POST .../claim) is a genuine foreign-claimant conflict (C1) at the
// envelope level; every OTHER single-route write the worker sends (PostQuestion, PostComment,
// PostProgress, relation-add outside a batch, ...) is a non-batch call whose 409 means something
// else entirely per the SAME server contract ClassifyOp already disambiguates for batch ops:
//   - a relation op (already-exists/cycle, agent-ops.ts RelationCycleError) -> droppable, not fatal
//   - anything else -> an idempotency-key op-mismatch (reuse of a key across a different op),
//     which plan §2.3 calls "a permanent client bug ... journal failed" and must fail loudly
//     rather than being silently treated as an expected, benign foreign-claim skip.
//
// isClaimOp/isRelationOp let the caller disambiguate without this package needing the full
// op-name vocabulary, exactly like ClassifyOp's booleans.
func ClassifyEnvelope(status int, isClaimOp, isRelationOp bool) FailureClass {
	switch {
	case status >= 200 && status < 300:
		return ClassSuccess
	case status == 429:
		return ClassRateLimited
	case status == 401 || status == 403:
		return ClassAuthFailure
	case status == 404:
		return ClassGone
	case status == 409 && isClaimOp:
		return ClassConflictForeign
	case status == 409 && isRelationOp:
		return ClassConflictDroppable
	case status == 409:
		return ClassConflictKeyMismatch
	case status == 400 || status == 422:
		return ClassPermanent
	case status >= 500:
		return ClassTransient
	default:
		return ClassTransient
	}
}

// ClassifyOp applies the plan §2.3 per-op batch classification rules. isClaimOp/isRelationOp let
// the caller disambiguate a 409 without this package needing to know the full op-name vocabulary.
func ClassifyOp(r OpResult, isClaimOp, isRelationOp, isSeverityOp bool) FailureClass {
	if r.Deduplicated {
		return ClassSuccess
	}
	switch {
	case r.Status >= 200 && r.Status < 300:
		return ClassSuccess
	case r.Status == 409 && isClaimOp:
		return ClassConflictForeign
	case r.Status == 409 && isRelationOp:
		return ClassConflictDroppable
	case r.Status == 409:
		// Idempotency-key op-mismatch (reuse of a key across a different op) — plan §2.3 says this
		// must be unreachable given the "<jobId>:<opIndex>" derivation, but is classified here so a
		// bug that DOES reach it fails loudly instead of retrying forever.
		return ClassConflictKeyMismatch
	case r.Status == 400 && isSeverityOp:
		// severity op on a non-user_report issue (C8) — compile-time bug, unreachable by construction.
		return ClassPermanent
	case r.Status == 400 || r.Status == 422:
		return ClassPermanent
	case r.Status == 401 || r.Status == 403:
		return ClassAuthFailure
	case r.Status == 404:
		return ClassGone
	case r.Status >= 500 || r.Status == 0:
		return ClassTransient
	default:
		return ClassTransient
	}
}

// ClassifyBatch composes envelope classification with the required per-op results[] walk (C3: a
// batch is "partial-completion and always HTTP 200", so a caller that only checks the envelope
// status can miss every per-op failure). meta[i] disambiguates op i's conflict sub-class the same
// way ClassifyOp's booleans do; a short/absent meta slice defaults to "none of the above" for the
// remaining ops. Returns:
//   - overall: ClassSuccess only if the envelope was 2xx AND every op classified as ClassSuccess;
//     otherwise the envelope's own class if the envelope itself was non-2xx, or ClassTransient as a
//     generic "something in this batch failed" marker when the envelope was 2xx but ops were not.
//   - perOp: one FailureClass per results[] entry, in order (nil if the envelope wasn't 2xx or the
//     body didn't parse).
//   - retryOps: indices into perOp whose class NeedsRetry() -- the "narrow re-send" set (plan §2.3).
func ClassifyBatch(envelopeStatus int, body json.RawMessage, meta []BatchOpMeta) (overall FailureClass, perOp []FailureClass, retryOps []int) {
	if envelopeStatus < 200 || envelopeStatus >= 300 {
		// The batch envelope itself is never the claim route (C1's foreign-claimant 409 is a
		// single-route concept), so neither disambiguating flag applies here.
		return ClassifyEnvelope(envelopeStatus, false, false), nil, nil
	}
	var resp BatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		// A 2xx envelope with an unparsable body is a server/client contract bug, not a transient
		// condition -- surface it as permanent rather than retrying forever.
		return ClassPermanent, nil, nil
	}
	perOp = make([]FailureClass, len(resp.Results))
	overall = ClassSuccess
	for i, r := range resp.Results {
		var m BatchOpMeta
		if i < len(meta) {
			m = meta[i]
		}
		c := ClassifyOp(OpResult{Status: r.Status, Deduplicated: r.Deduplicated()}, m.IsClaimOp, m.IsRelationOp, m.IsSeverityOp)
		perOp[i] = c
		if c != ClassSuccess {
			overall = ClassTransient
		}
		if c.NeedsRetry() {
			retryOps = append(retryOps, i)
		}
	}
	return overall, perOp, retryOps
}

// NeedsRetry reports whether the given per-op result should be included in the "narrow re-send"
// of a partially-failed batch (plan §2.3: "re-sending only the ops that did not return ok").
func (c FailureClass) NeedsRetry() bool {
	return c == ClassTransient || c == ClassRateLimited
}

// IsTerminal reports whether this class ends the job outright (journal `failed`/`skipped`) rather
// than looping back through retry/backoff.
func (c FailureClass) IsTerminal() bool {
	switch c {
	case ClassConflictForeign, ClassConflictKeyMismatch, ClassPermanent, ClassGone:
		return true
	default:
		return false
	}
}

// --- rate limiting (plan §2.4 "Rate limited" row) -------------------------------------------------

// SleepFunc abstracts time.Sleep so callers (and tests) can inject a fake clock instead of
// blocking for real. time.Sleep itself satisfies this signature.
type SleepFunc func(time.Duration)

// WaitRateLimit sleeps for exactly the Retry-After duration read from resp headers (default 60s
// per plan §2.4) using the given SleepFunc, and returns the duration slept. A 429 handled this way
// never counts as a failure (plan §2.4: "Never counts as a failure") — callers must not feed this
// path into a CircuitBreaker or backoff attempt counter.
func WaitRateLimit(h http.Header, sleep SleepFunc) time.Duration {
	d := RetryAfter(h, 60*time.Second)
	sleep(d)
	return d
}

// CtxSleepFunc is a context-aware sleep: it MUST return once ctx is Done even if d has not yet
// elapsed. Every retry/rate-limit wait in this package uses this shape (not the bare SleepFunc
// above) so a graceful shutdown signal (SIGTERM) interrupts a pending backoff/rate-limit wait
// instead of blocking uncancellably for up to the longest step on BackoffSchedule, or up to
// MaxRetryAfter on a 429 (validator finding: "every retry/rate-limit wait in this package is an
// uncancellable time.Sleep ... this turns every graceful K8s reschedule/drain/eviction during a
// backoff into a SIGKILL"). time.Sleep does NOT satisfy this signature on purpose -- SleepCtx below
// is the production implementation.
type CtxSleepFunc func(context.Context, time.Duration)

// SleepCtx is the default CtxSleepFunc: sleeps d, or returns early the instant ctx is Done.
func SleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// WaitRateLimitCtx is WaitRateLimit's context-aware counterpart: honors Retry-After (capped at
// MaxRetryAfter, see client.go) but returns as soon as ctx is Done rather than blocking for the
// full duration. Callers should check ctx.Err() immediately after this returns, since an early
// return here does not by itself mean the wait was satisfied.
func WaitRateLimitCtx(ctx context.Context, h http.Header, sleep CtxSleepFunc) time.Duration {
	d := RetryAfter(h, 60*time.Second)
	sleep(ctx, d)
	return d
}

// --- exponential backoff (plan §2.4 "Transient" row) -----------------------------------------------

// BackoffSchedule is the plan §2.4 ladder for the Transient failure class: 1s -> 5s -> 30s -> 2m ->
// 5m, capped at the last step for any attempt beyond the schedule's length.
var BackoffSchedule = []time.Duration{
	1 * time.Second,
	5 * time.Second,
	30 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

// BackoffForAttempt returns the sleep duration before retry attempt N (1-indexed: the delay before
// the FIRST retry, after the first failure, is attempt 1). Attempts beyond the schedule's length
// are capped at the last (longest) step.
func BackoffForAttempt(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	idx := attempt - 1
	if idx >= len(BackoffSchedule) {
		idx = len(BackoffSchedule) - 1
	}
	return BackoffSchedule[idx]
}

// --- circuit breaker (plan §2.4 "Transient" row: "Circuits are per-dependency with enumerated
// scopes ... 5 consecutive failures open a circuit; half-open probe every 2m") -------------------

// CircuitState is the state of one CircuitBreaker.
type CircuitState int

const (
	CircuitClosed CircuitState = iota
	CircuitOpen
	CircuitHalfOpen
)

func (s CircuitState) String() string {
	switch s {
	case CircuitClosed:
		return "closed"
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

const (
	// consecutiveFailuresToOpen is plan §2.4: "5 consecutive failures open a circuit".
	consecutiveFailuresToOpen = 5
	// halfOpenProbeInterval is plan §2.4: "half-open probe every 2m".
	halfOpenProbeInterval = 2 * time.Minute
)

// Circuit scopes enumerated by plan §2.4: sentinel-api is the one fixed scope; llm:<provider> and
// git:<provider> are built by the caller per configured provider via ScopeLLM/ScopeGit.
const ScopeSentinelAPI = "sentinel-api"

// ScopeLLM builds the "llm:<provider>" circuit scope (plan §2.4: "pauses brain jobs; fallback
// provider takes over if configured").
func ScopeLLM(provider string) string { return "llm:" + provider }

// ScopeGit builds the "git:<provider>" circuit scope (plan §2.4: "pauses FIX + PR polls for that
// provider's repos").
func ScopeGit(provider string) string { return "git:" + provider }

// CircuitBreaker tracks consecutive-failure state for one dependency scope. NowFunc is injectable
// so tests can control time without real sleeps; it defaults to time.Now when nil.
//
// CircuitBreaker is safe for concurrent use: the dispatcher shares ONE instance per scope across
// every per-issue runner goroutine (plan §2.4 "SyncBreaker (one shared instance)") plus the gauge
// publisher's concurrent State() reads, so all four methods take an internal mutex. NowFunc itself
// must still be safe to call concurrently (time.Now is; injected fakes in tests must be too).
type CircuitBreaker struct {
	Scope   string
	NowFunc func() time.Time

	mu                  sync.Mutex
	state               CircuitState
	consecutiveFailures int
	openedAt            time.Time
	// probing is true while a half-open probe call is in flight, so Allow lets exactly ONE call
	// through per open-then-elapsed window rather than every caller during it.
	probing bool
}

// NewCircuitBreaker builds a closed CircuitBreaker for the given scope.
func NewCircuitBreaker(scope string) *CircuitBreaker {
	return &CircuitBreaker{Scope: scope}
}

func (b *CircuitBreaker) now() time.Time {
	if b.NowFunc != nil {
		return b.NowFunc()
	}
	return time.Now()
}

// State returns the breaker's current state, resolving Open -> HalfOpen once the probe interval
// has elapsed. This is a pure read (repeated calls don't consume the single probe slot); Allow is
// what actually grants the probe.
func (b *CircuitBreaker) State() CircuitState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == CircuitOpen && !b.probing && b.now().Sub(b.openedAt) >= halfOpenProbeInterval {
		return CircuitHalfOpen
	}
	return b.state
}

// Allow reports whether a call through this breaker's scope may proceed right now. Closed always
// allows. Open blocks until the probe interval elapses, then allows exactly one call through
// (marking it the in-flight probe) and blocks any others until that probe resolves via
// RecordSuccess/RecordFailure.
func (b *CircuitBreaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if b.probing {
			return false
		}
		if b.now().Sub(b.openedAt) >= halfOpenProbeInterval {
			b.probing = true
			return true
		}
		return false
	default:
		return true
	}
}

// RecordSuccess resets failure tracking and closes the circuit — a successful half-open probe
// closes it, same as any other success.
func (b *CircuitBreaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutiveFailures = 0
	b.state = CircuitClosed
	b.probing = false
}

// RecordFailure increments the consecutive-failure count, opening the circuit at the threshold. A
// failed half-open probe re-opens it and restarts the 2m probe timer without touching the
// consecutive-failure count (the circuit was already open; this is a probe result, not a new
// streak).
func (b *CircuitBreaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == CircuitOpen && b.probing {
		b.openedAt = b.now()
		b.probing = false
		return
	}
	b.consecutiveFailures++
	if b.consecutiveFailures >= consecutiveFailuresToOpen {
		b.state = CircuitOpen
		b.openedAt = b.now()
		b.probing = false
	}
}
