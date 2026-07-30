// Use-case rows U6–U7: the grouping/fingerprinting matrix in
// docs/plans/E2E_RECOVERY_PLAN.md, driven through the real HTTP ingest surfaces.
package e2e

import (
	"fmt"
	"net/http"
	"testing"
)

// groupingFrame builds a stack frame map with the given file/function and an explicit in_app value,
// so each U6 case can control both distinctness and the "nothing is in_app" precondition precisely.
func groupingFrame(file, function string, inApp bool) map[string]any {
	return map[string]any{"file": file, "line": 7, "function": function, "in_app": inApp}
}

// TestU6_NoInAppFramesStillProducesDistinctIssues proves U6, the S11 regression test. S11's bug: when
// no stack frame is marked in_app, the fingerprinter used to hash the error class ALONE, so every
// distinct crash within a class — even from entirely different call sites deep in a dependency —
// collapsed into one issue. The fix (apps/processor-go/fingerprint/fingerprint.go) falls back to the
// top stack frames regardless of the in_app flag. This test sends the SAME error_class with 3
// genuinely distinct stacktraces, none of them in_app, and asserts on the resulting issue COUNT — not
// on a fingerprint computed independently here, per the assignment's own warning that doing so would
// only prove this test's fingerprint function agrees with itself.
func TestU6_NoInAppFramesStillProducesDistinctIssues(t *testing.T) {
	f := newFixture(t)

	const errorClass = "E2EDependencyPanic"
	stacktraces := [][]map[string]any{
		{groupingFrame("vendor/dep_a/client.go", "Dial", false), groupingFrame("vendor/dep_a/pool.go", "acquire", false)},
		{groupingFrame("vendor/dep_b/codec.go", "Decode", false), groupingFrame("vendor/dep_b/frame.go", "readHeader", false)},
		{groupingFrame("vendor/dep_c/retry.go", "Backoff", false), groupingFrame("vendor/dep_c/clock.go", "Sleep", false)},
	}

	for i, frames := range stacktraces {
		ev := f.newEvent().with(event{
			"error_class": errorClass,
			"message":     fmt.Sprintf("dependency panic variant %d", i),
			"stacktrace":  frames,
		})
		res := f.ingest(ev)
		if res.Status != http.StatusAccepted {
			t.Fatalf("POST /ingest for stacktrace variant %d: got status %d, want 202\n  body: %s", i, res.Status, res.Body)
		}
	}

	f.waitForIssues(3)
	f.waitForOccurrences(3)

	// ingestIssueSummaries (ingest_test.go) reads only the columns that actually exist on the live
	// issues table — see its doc comment for why harness_test.go's issues()/onlyIssue() cannot be used
	// here (they select nonexistent title/first_release/last_release columns).
	for _, issue := range ingestIssueSummaries(t, f) {
		if issue.ErrorClass != errorClass {
			t.Fatalf("issue %s has ErrorClass %q, want %q", issue.ID, issue.ErrorClass, errorClass)
		}
	}
}

// TestU7_HighVolumeSameErrorCollapsesToOneIssueWithCorrectCount proves U7: the same error sent 100
// times through the real HTTP endpoint (batched, to keep wall-clock sane) produces exactly one issue
// whose `count` is exactly 100 — grouping must collapse identical errors, and the occurrence counter
// must track every single one of them, not lose or double any.
func TestU7_HighVolumeSameErrorCollapsesToOneIssueWithCorrectCount(t *testing.T) {
	f := newFixture(t)

	const n = 100
	events := make([]event, 0, n)
	for i := 0; i < n; i++ {
		// Deliberately do NOT vary error_class or stacktrace: identical shape is the point. newEvent()
		// still gives each one a unique trace_id/span_id, which must not affect grouping.
		events = append(events, f.newEvent())
	}

	res := f.ingestBatch(events)
	if res.Status != http.StatusAccepted {
		t.Fatalf("POST /ingest/batch: got status %d, want 202\n  body: %s", res.Status, res.Body)
	}

	body := f.decodeBatch(res)
	if body.Ingested != n {
		t.Fatalf("batch response Ingested = %d, want %d\n  body: %s", body.Ingested, n, res.Body)
	}
	if body.Failed != 0 {
		t.Fatalf("batch response Failed = %d, want 0\n  body: %s", body.Failed, res.Body)
	}

	f.waitForOccurrences(n)
	f.waitForIssues(1)

	issue := ingestOnlyIssueSummary(t, f)
	if issue.Count != n {
		t.Fatalf("issue.Count = %d, want %d", issue.Count, n)
	}
}
