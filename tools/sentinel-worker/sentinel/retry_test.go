package sentinel

import (
	"net/http"
	"testing"
	"time"
)

func TestClassifyStatus(t *testing.T) {
	cases := map[int]int{
		200: ClassOK, 201: ClassOK, 299: ClassOK,
		401: ClassAuth, 403: ClassAuth,
		404: ClassNotFound,
		409: ClassConflict,
		400: ClassValidation, 422: ClassValidation,
		429: ClassRateLimit,
		500: ClassNetwork, 502: ClassNetwork,
	}
	for status, want := range cases {
		if got := ClassifyStatus(status); got != want {
			t.Errorf("ClassifyStatus(%d) = %d, want %d", status, got, want)
		}
	}
}

func TestClassifyEnvelope(t *testing.T) {
	if ClassifyEnvelope(200, false, false) != ClassSuccess {
		t.Errorf("200 should be ClassSuccess")
	}
	if ClassifyEnvelope(429, false, false) != ClassRateLimited {
		t.Errorf("429 should be ClassRateLimited")
	}
	if ClassifyEnvelope(500, false, false) != ClassTransient {
		t.Errorf("500 should be ClassTransient")
	}
	if ClassifyEnvelope(401, false, false) != ClassAuthFailure {
		t.Errorf("401 should be ClassAuthFailure")
	}
	if ClassifyEnvelope(404, false, false) != ClassGone {
		t.Errorf("404 should be ClassGone")
	}
	if ClassifyEnvelope(400, false, false) != ClassPermanent {
		t.Errorf("400 should be ClassPermanent")
	}
}

// TestClassifyEnvelope_ClaimConflictIsForeign proves the ONE genuine C1 case: a non-batch 409 on
// the claim route is a foreign-claimant conflict, terminal as skipped(foreign-claim).
func TestClassifyEnvelope_ClaimConflictIsForeign(t *testing.T) {
	if got := ClassifyEnvelope(409, true, false); got != ClassConflictForeign {
		t.Fatalf("claim envelope 409 = %v, want ClassConflictForeign", got)
	}
	if !ClassConflictForeign.IsTerminal() {
		t.Fatalf("ClassConflictForeign must be terminal")
	}
}

// TestClassifyEnvelope_RelationConflictIsDroppable proves a non-batch relation 409
// (already-exists/cycle, agent-ops.ts RelationCycleError) is droppable, not a foreign claim.
func TestClassifyEnvelope_RelationConflictIsDroppable(t *testing.T) {
	if got := ClassifyEnvelope(409, false, true); got != ClassConflictDroppable {
		t.Fatalf("relation envelope 409 = %v, want ClassConflictDroppable", got)
	}
}

// TestClassifyEnvelope_QuestionOpMismatch409IsKeyMismatchNotForeignClaim is the red-first
// regression this finding exists to prevent: PostQuestion (blocking questions, sent OUTSIDE batch
// per plan §2.3) is neither the claim route nor a relation route, so its 409
// (IdempotencyKeyOpMismatchError, questions/+server.ts:62-64) must classify as the permanent
// key-mismatch bug plan §2.3 says must "fail loudly" and journal `failed` -- NOT as a foreign-claim
// conflict, which would silently journal `skipped(foreign-claim)` and hide the exact
// key-derivation bug this class exists to catch.
func TestClassifyEnvelope_QuestionOpMismatch409IsKeyMismatchNotForeignClaim(t *testing.T) {
	got := ClassifyEnvelope(409, false, false)
	if got == ClassConflictForeign {
		t.Fatalf("question 409 classified as ClassConflictForeign (silently skipped), want ClassConflictKeyMismatch (fails loudly)")
	}
	if got != ClassConflictKeyMismatch {
		t.Fatalf("question 409 = %v, want ClassConflictKeyMismatch", got)
	}
	if !got.IsTerminal() {
		t.Fatalf("ClassConflictKeyMismatch must be terminal (journal failed, no infinite retry)")
	}
}

// TestClassifyOp_DeduplicatedIsAlwaysSuccess proves the C4 replay rule: `deduplicated: true` on a
// per-op result is success regardless of its raw status -- tabled over a 2xx AND non-2xx status so
// a mutation that deletes the `if r.Deduplicated` guard cannot hide behind the 2xx case reaching
// ClassSuccess anyway.
func TestClassifyOp_DeduplicatedIsAlwaysSuccess(t *testing.T) {
	for _, status := range []int{201, 409, 500} {
		r := OpResult{Status: status, Deduplicated: true, Op: "issues.comment"}
		if got := ClassifyOp(r, false, false, false); got != ClassSuccess {
			t.Errorf("status %d: deduplicated result should classify success, got %v", status, got)
		}
	}
}

func TestClassifyOp_ClaimConflictIsForeign(t *testing.T) {
	r := OpResult{Status: 409, Op: "issues.claim"}
	if got := ClassifyOp(r, true, false, false); got != ClassConflictForeign {
		t.Errorf("claim 409 should be ClassConflictForeign, got %v", got)
	}
}

func TestClassifyOp_RelationConflictIsDroppable(t *testing.T) {
	r := OpResult{Status: 409, Op: "issues.relations.add"}
	if got := ClassifyOp(r, false, true, false); got != ClassConflictDroppable {
		t.Errorf("relation 409 should be ClassConflictDroppable, got %v", got)
	}
}

// TestClassifyOp_IdempotencyKeyMismatchIsPermanent proves the op-mismatch 409 (key reused across
// op types) is a permanent client bug, never retried (plan §2.3).
func TestClassifyOp_IdempotencyKeyMismatchIsPermanent(t *testing.T) {
	r := OpResult{Status: 409, Op: "issues.progress"}
	if got := ClassifyOp(r, false, false, false); got != ClassConflictKeyMismatch {
		t.Errorf("bare op 409 (not claim, not relation) should be key-mismatch, got %v", got)
	}
}

func TestClassifyOp_SeverityOn400IsPermanent(t *testing.T) {
	r := OpResult{Status: 400, Op: "issues.report.severity"}
	if got := ClassifyOp(r, false, false, true); got != ClassPermanent {
		t.Errorf("severity 400 should be ClassPermanent, got %v", got)
	}
}

// --- batch: two-level classification (C3) -------------------------------------------------------

// TestClassifyBatch_200EnvelopeWithFailedOpIsFailure proves the C3 trap this suite must close: a
// batch response that is HTTP 200 at the envelope level but carries a failed op in results[] must
// classify as an overall failure, not success -- an implementation that only checks the envelope
// status would wrongly pass this.
func TestClassifyBatch_200EnvelopeWithFailedOpIsFailure(t *testing.T) {
	body := []byte(`{"results":[{"ok":true,"status":201},{"ok":false,"status":500,"error":"boom"}],"completed":1}`)
	overall, perOp, retryOps := ClassifyBatch(200, body, nil)
	if overall == ClassSuccess {
		t.Fatalf("overall = %v, want a failure class despite 200 envelope", overall)
	}
	if len(perOp) != 2 || perOp[0] != ClassSuccess || perOp[1] != ClassTransient {
		t.Fatalf("perOp = %v, want [Success Transient]", perOp)
	}
	if len(retryOps) != 1 || retryOps[0] != 1 {
		t.Fatalf("retryOps = %v, want [1]", retryOps)
	}
}

// TestClassifyBatch_DeduplicatedOpIsSuccess proves Deduplicated is read out of result, not a
// top-level field.
func TestClassifyBatch_DeduplicatedOpIsSuccess(t *testing.T) {
	body := []byte(`{"results":[{"ok":true,"status":409,"result":{"comment":{},"deduplicated":true}}],"completed":1}`)
	overall, perOp, retryOps := ClassifyBatch(200, body, nil)
	if overall != ClassSuccess {
		t.Fatalf("overall = %v, want ClassSuccess", overall)
	}
	if len(perOp) != 1 || perOp[0] != ClassSuccess {
		t.Fatalf("perOp = %v, want [Success]", perOp)
	}
	if len(retryOps) != 0 {
		t.Fatalf("retryOps = %v, want none", retryOps)
	}
}

// TestClassifyBatch_OpMismatch409IsPermanentNotRetried proves an idempotency-key op-mismatch 409
// (bare op, not claim/relation) classifies permanent and is excluded from the retry set.
func TestClassifyBatch_OpMismatch409IsPermanentNotRetried(t *testing.T) {
	body := []byte(`{"results":[{"ok":false,"status":409}],"completed":0}`)
	overall, perOp, retryOps := ClassifyBatch(200, body, []BatchOpMeta{{}})
	if overall == ClassSuccess {
		t.Fatalf("overall = %v, want failure", overall)
	}
	if len(perOp) != 1 || perOp[0] != ClassConflictKeyMismatch {
		t.Fatalf("perOp = %v, want [ClassConflictKeyMismatch]", perOp)
	}
	if len(retryOps) != 0 {
		t.Fatalf("retryOps = %v, want none (permanent is not retried)", retryOps)
	}
}

// TestClassifyBatch_NonEnvelopeStatusSkipsPerOpWalk proves a non-2xx envelope (the batch route
// itself rejected the request, e.g. auth) short-circuits to envelope classification.
func TestClassifyBatch_NonEnvelopeStatusSkipsPerOpWalk(t *testing.T) {
	overall, perOp, retryOps := ClassifyBatch(401, []byte(`{}`), nil)
	if overall != ClassAuthFailure {
		t.Fatalf("overall = %v, want ClassAuthFailure", overall)
	}
	if perOp != nil || retryOps != nil {
		t.Fatalf("perOp/retryOps should be nil on a non-2xx envelope, got %v / %v", perOp, retryOps)
	}
}

func TestFailureClass_NeedsRetryAndIsTerminal(t *testing.T) {
	if !ClassTransient.NeedsRetry() || !ClassRateLimited.NeedsRetry() {
		t.Errorf("transient/rate-limited must need retry")
	}
	if ClassSuccess.NeedsRetry() || ClassPermanent.NeedsRetry() {
		t.Errorf("success/permanent must not need retry")
	}
	if !ClassPermanent.IsTerminal() || !ClassConflictForeign.IsTerminal() || !ClassGone.IsTerminal() {
		t.Errorf("permanent/foreign-conflict/gone must be terminal")
	}
	if ClassTransient.IsTerminal() || ClassSuccess.IsTerminal() {
		t.Errorf("transient/success must not be terminal")
	}
}

// --- rate limiting: Retry-After honored via injected clock (no real sleeps) -----------------------

// TestWaitRateLimit_HonorsRetryAfterHeader proves plan §2.4's "sleep exactly Retry-After" using an
// injected SleepFunc — no real sleep happens in this test.
func TestWaitRateLimit_HonorsRetryAfterHeader(t *testing.T) {
	h := http.Header{"Retry-After": []string{"17"}}
	var slept time.Duration
	fake := func(d time.Duration) { slept = d }

	got := WaitRateLimit(h, fake)
	if got != 17*time.Second {
		t.Fatalf("WaitRateLimit returned %v, want 17s", got)
	}
	if slept != 17*time.Second {
		t.Fatalf("sleep func received %v, want 17s", slept)
	}
}

// TestWaitRateLimit_DefaultsTo60sWhenHeaderMissing proves the plan §2.4 "(default 60)" fallback.
func TestWaitRateLimit_DefaultsTo60sWhenHeaderMissing(t *testing.T) {
	var slept time.Duration
	fake := func(d time.Duration) { slept = d }

	got := WaitRateLimit(http.Header{}, fake)
	if got != 60*time.Second || slept != 60*time.Second {
		t.Fatalf("got %v slept %v, want 60s both", got, slept)
	}
}

// --- exponential backoff ----------------------------------------------------------------------

func TestBackoffForAttempt_FollowsLadderAndCaps(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 1 * time.Second}, // clamped to attempt 1
		{1, 1 * time.Second},
		{2, 5 * time.Second},
		{3, 30 * time.Second},
		{4, 2 * time.Minute},
		{5, 5 * time.Minute},
		{6, 5 * time.Minute}, // capped
		{100, 5 * time.Minute},
	}
	for _, tc := range cases {
		if got := BackoffForAttempt(tc.attempt); got != tc.want {
			t.Errorf("BackoffForAttempt(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

// --- circuit breaker -----------------------------------------------------------------------------

func TestCircuitBreaker_OpensAfterFiveConsecutiveFailures(t *testing.T) {
	b := NewCircuitBreaker(ScopeSentinelAPI)
	for i := 0; i < 4; i++ {
		b.RecordFailure()
		if b.State() != CircuitClosed {
			t.Fatalf("after %d failures, state = %v, want closed", i+1, b.State())
		}
		if !b.Allow() {
			t.Fatalf("after %d failures, Allow() = false, want true (still closed)", i+1)
		}
	}
	b.RecordFailure() // 5th consecutive failure
	if b.State() != CircuitOpen {
		t.Fatalf("after 5 failures, state = %v, want open", b.State())
	}
	if b.Allow() {
		t.Fatalf("Allow() = true immediately after opening, want false")
	}
}

func TestCircuitBreaker_SuccessResetsConsecutiveCount(t *testing.T) {
	b := NewCircuitBreaker(ScopeSentinelAPI)
	for i := 0; i < 4; i++ {
		b.RecordFailure()
	}
	b.RecordSuccess()
	for i := 0; i < 4; i++ {
		b.RecordFailure()
	}
	if b.State() != CircuitClosed {
		t.Fatalf("state = %v, want closed (success should have reset the streak)", b.State())
	}
}

// TestCircuitBreaker_HalfOpenProbeEvery2m proves plan §2.4: after opening, no call is allowed
// until 2m elapses, then exactly one probe call is allowed; a failed probe re-opens and restarts
// the timer, a successful probe closes the circuit. All timing is via an injected clock.
func TestCircuitBreaker_HalfOpenProbeEvery2m(t *testing.T) {
	now := time.Unix(0, 0)
	b := NewCircuitBreaker(ScopeSentinelAPI)
	b.NowFunc = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		b.RecordFailure()
	}
	if b.State() != CircuitOpen {
		t.Fatalf("state = %v, want open", b.State())
	}

	now = now.Add(90 * time.Second) // < 2m: still blocked
	if b.Allow() {
		t.Fatalf("Allow() = true before 2m elapsed, want false")
	}

	now = now.Add(31 * time.Second) // now at 121s: >= 2m
	if b.State() != CircuitHalfOpen {
		t.Fatalf("state = %v, want half-open at >=2m", b.State())
	}
	if !b.Allow() {
		t.Fatalf("Allow() = false at half-open probe window, want true (the probe)")
	}
	if b.Allow() {
		t.Fatalf("second Allow() during the same probe window = true, want false (only one probe in flight)")
	}

	// Failed probe: re-opens and restarts the 2m timer.
	b.RecordFailure()
	if b.State() != CircuitOpen {
		t.Fatalf("state after failed probe = %v, want open", b.State())
	}
	if b.Allow() {
		t.Fatalf("Allow() = true immediately after a failed probe re-opened the circuit, want false")
	}

	now = now.Add(2 * time.Minute)
	if !b.Allow() {
		t.Fatalf("Allow() = false at second probe window, want true")
	}
	b.RecordSuccess() // successful probe closes the circuit
	if b.State() != CircuitClosed {
		t.Fatalf("state after successful probe = %v, want closed", b.State())
	}
	if !b.Allow() {
		t.Fatalf("Allow() = false once closed, want true")
	}
}

func TestScopeLLM_and_ScopeGit_BuildDynamicScopeNames(t *testing.T) {
	if got := ScopeLLM("openai"); got != "llm:openai" {
		t.Errorf("ScopeLLM = %q", got)
	}
	if got := ScopeGit("github"); got != "git:github" {
		t.Errorf("ScopeGit = %q", got)
	}
}
