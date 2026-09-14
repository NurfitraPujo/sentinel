//go:build e2e

// Package e2e — sentinel-releases-snooze's lifecycle row beside U11/U12 (lifecycle_test.go): an issue
// resolved in `next` reads the next release's version once an UNRELATED fingerprint carries it
// (contract §11's last row; flows A7 and I11). Its own file, tagged `e2e`, because it drives the agent
// API through agent_work_loop_test.go's seedAgent/agentRequest helpers, which are `//go:build e2e`
// while lifecycle_test.go is compiled by the plain build as well — `SENTINEL_E2E=1 go test -tags=e2e
// ./tests/e2e/` runs both files together.
package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------------------------------
// U12b — resolve in `next`, then an UNRELATED issue's first occurrence on a new SHA names the release.
// ---------------------------------------------------------------------------------------------------

// TestU12b_ResolvedInNextIsRewrittenByTheNextRelease is sentinel-releases-snooze's e2e row (contract
// §11's last row; flows A7 and I11; decisions 15, 21): an issue resolved with the reserved value `next`
// reads the concrete version of the FIRST release seen after its resolution — a release carried by an
// UNRELATED fingerprint, since the resolved issue itself never recurred — and it is NOT a regression.
//
// The resolution goes through the real agent route (`PATCH /api/agent/issues/{id}/status` with
// `resolved_in_version: "next"` — what `sentinel status <id> resolved --resolved-in next` sends, A7),
// so the row also proves the literal is accepted and stored as-is.
//
// Two synchronization points, and the second is deliberately a POLL rather than a read: the
// occurrence landing (waitForOccurrences) proves the STORE committed the unrelated event, but the
// `next` rewrite is a separate post-commit statement — best-effort, its own short transaction, after
// StoreEvent returns — so the occurrence's arrival proves the store, not the rewrite. The row polls
// `GET /api/agent/issues/{id}` until `resolvedInVersion` equals the SHA or 10 s elapse, and a loaded
// runner must not read as a bug.
func TestU12b_ResolvedInNextIsRewrittenByTheNextRelease(t *testing.T) {
	requireStack(t)
	f := newFixture(t)
	admin := f.newDashboardUser("admin")
	_, bearer := f.seedAgent(t, admin, "next-release-agent")

	first := f.newEvent().with(map[string]any{"release_version": "1.0.0"})
	if res := f.ingest(first); res.Status != http.StatusAccepted {
		t.Fatalf("U12b: first ingest: got %d, want 202\n  body: %s", res.Status, res.Body)
	}
	f.waitForOccurrences(1)
	f.waitForIssues(1)
	fresh := f.onlyIssue()

	// A7: resolve in `next` through the agent route.
	resolveRes := agentRequest(t, http.MethodPatch, "/api/agent/issues/"+fresh.ID+"/status", bearer, map[string]any{
		"status":              "resolved",
		"resolved_in_version": "next",
	})
	if resolveRes.Status != http.StatusOK {
		t.Fatalf("U12b: PATCH resolve in next: got %d, want 200\n  body: %s", resolveRes.Status, resolveRes.Body)
	}
	resolved := f.onlyIssue()
	if resolved.Status != "resolved" || resolved.ResolvedInVersion == nil || *resolved.ResolvedInVersion != "next" {
		t.Fatalf("U12b: after the PATCH the row must read status=resolved resolved_in_version=next, got status=%q resolved_in_version=%v",
			resolved.Status, resolved.ResolvedInVersion)
	}
	if resolved.ResolvedAt == nil {
		t.Fatalf("U12b: resolved_at is NULL after the PATCH — the rewrite compares against it (decision 27), so a `next` with no resolved_at could never be rewritten")
	}

	// An UNRELATED fingerprint's first occurrence on a never-seen SHA: the release's first sighting.
	const sha = "9f8e7d6"
	unrelated := f.newEvent().with(map[string]any{
		"error_class":     "LifecycleNextReleaseUnrelatedError",
		"message":         "e2e unrelated error carrying the next release",
		"release_version": sha,
	})
	if res := f.ingest(unrelated); res.Status != http.StatusAccepted {
		t.Fatalf("U12b: unrelated ingest on %s: got %d, want 202\n  body: %s", sha, res.Status, res.Body)
	}
	f.waitForOccurrences(2)
	f.waitForIssues(2)

	// The rewrite is post-commit and best-effort: poll the agent read for up to 10 s.
	path := "/api/agent/issues/" + fresh.ID
	var last string
	waitFor(t, 10*time.Second, fmt.Sprintf("GET %s to report resolvedInVersion = %q (the post-commit next rewrite)", path, sha), func() (bool, string) {
		res := agentRequest(t, http.MethodGet, path, bearer, nil)
		if res.Status != http.StatusOK {
			return false, fmt.Sprintf("GET %s: %d %s", path, res.Status, res.Body)
		}
		var body struct {
			Issue struct {
				Status            string  `json:"status"`
				ResolvedInVersion *string `json:"resolvedInVersion"`
			} `json:"issue"`
		}
		if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
			return false, fmt.Sprintf("GET %s: body is not JSON: %v\n  raw: %s", path, err, res.Body)
		}
		if body.Issue.ResolvedInVersion == nil {
			last = "<nil>"
		} else {
			last = *body.Issue.ResolvedInVersion
		}
		return body.Issue.Status == "resolved" && last == sha, fmt.Sprintf("status=%q resolvedInVersion=%q", body.Issue.Status, last)
	})

	got := f.issues()[0] // oldest first: the resolved issue
	if got.ID != fresh.ID {
		t.Fatalf("U12b: expected the resolved issue %s first (oldest), got %s", fresh.ID, got.ID)
	}
	if got.Status != "resolved" || got.ResolvedInVersion == nil || *got.ResolvedInVersion != sha {
		t.Fatalf("U12b: the row must read status=resolved resolved_in_version=%q, got status=%q resolved_in_version=%v", sha, got.Status, got.ResolvedInVersion)
	}
	if got.RegressionStatus != "none" || got.RegressionCount != 0 || got.LastRegressedAt != nil {
		t.Fatalf("U12b: the rewrite must not regress the issue: regression_status=%q regression_count=%d last_regressed_at=%v",
			got.RegressionStatus, got.RegressionCount, got.LastRegressedAt)
	}
	if got.Count != 1 {
		t.Fatalf("U12b: issue count = %d, want 1 — the unrelated fingerprint's occurrence must not be counted against this issue", got.Count)
	}
	if activity := f.activity("regressed"); len(activity) != 0 {
		t.Fatalf("U12b: found %d issue_activity 'regressed' row(s), want 0 — naming the release is not a regression (decision 8)", len(activity))
	}
}
