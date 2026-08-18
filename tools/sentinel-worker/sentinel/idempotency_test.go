package sentinel

import "testing"

// TestIdempotencyKey_StableAcrossCallsAndReplays proves plan §8's required guarantee: the same
// (jobId, opIndex) pair always derives the identical key, across any number of calls -- this is
// what lets a replayed POST after a kill -9 land on the server's original idempotency-key slot
// instead of minting a fresh one.
func TestIdempotencyKey_StableAcrossCallsAndReplays(t *testing.T) {
	jobID := "abc123deadbeef"
	first := IdempotencyKey(jobID, 0)
	for i := 0; i < 5; i++ {
		if got := IdempotencyKey(jobID, 0); got != first {
			t.Fatalf("call %d: IdempotencyKey(%q, 0) = %q, want stable %q", i, jobID, got, first)
		}
	}
	if first != jobID+":0" {
		t.Fatalf("IdempotencyKey(%q, 0) = %q, want %q", jobID, first, jobID+":0")
	}
}

// TestIdempotencyKey_DistinctOpIndexDistinctKey proves the other half of §8's requirement:
// different opIndex values within the same job never collide on one idempotency slot.
func TestIdempotencyKey_DistinctOpIndexDistinctKey(t *testing.T) {
	jobID := "abc123deadbeef"
	seen := make(map[string]int)
	for i := 0; i < 8; i++ {
		key := IdempotencyKey(jobID, i)
		if prev, ok := seen[key]; ok {
			t.Fatalf("opIndex %d and %d both derived key %q", prev, i, key)
		}
		seen[key] = i
	}
}

// TestIdempotencyKey_DistinctJobIDDistinctKey guards against two different jobs colliding on the
// same opIndex.
func TestIdempotencyKey_DistinctJobIDDistinctKey(t *testing.T) {
	a := IdempotencyKey("job-a", 3)
	b := IdempotencyKey("job-b", 3)
	if a == b {
		t.Fatalf("distinct jobIDs produced the same key %q", a)
	}
}
