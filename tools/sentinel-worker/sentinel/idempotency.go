package sentinel

import "strconv"

// IdempotencyKey derives the plan §2.2/§2.3 (C4) idempotency key for one compiled batch op:
// `<jobId>:<opIndex>`. It is stable across process restarts and replays — the SAME jobId (itself
// derived deterministically from kind+issueId+triggerSeq, see state.JobID) and the SAME opIndex
// within that job's compiled decision always produce the SAME key, which is exactly what lets a
// kill -9 between "POST sent" and "journal updated" replay safely: the server's idempotency-key
// index recognizes the retried POST as the original write and returns `deduplicated: true`
// instead of creating a second comment/question/progress row (plan §8's required test: "same
// inputs across calls/replays ⇒ identical key").
//
// A distinct opIndex (a different op within the same job's decision) always derives a distinct
// key, so two different mutating calls compiled from the same decision never collide on one
// idempotency slot.
func IdempotencyKey(jobID string, opIndex int) string {
	return jobID + ":" + strconv.Itoa(opIndex)
}
