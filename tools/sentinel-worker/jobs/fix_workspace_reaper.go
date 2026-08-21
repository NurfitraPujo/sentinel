// jobs/fix_workspace_reaper.go implements the FIX engine's workspace cleanup that plan §6
// ("Cleanup is the worker's own job — nothing external prunes for it") requires but WORKER_
// WORKSPACE_RETENTION_DAYS previously never consumed (finding 6): a startup orphan sweep, plus a
// periodic reaper honoring the configured retention window for workspaces FixRunner.KeepFailed
// left on disk.
//
// A FIX workspace lives at $WORKER_WORKSPACE_DIR/<jobId> (jobs/fix_workspace.go's
// PrepareFixWorkspace) — a directory named after the exact jobId journalFixRunning journals under
// state.StateFixRunning. That symmetry is what lets both passes below decide a directory's fate
// purely from its name and the journal, without parsing anything inside it.
package jobs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// SweepOrphanWorkspaces deletes every workspace directory under workspaceRoot whose name (the
// jobId PrepareFixWorkspace created it under) is journal-terminal or entirely unknown to the
// journal (finding 6's startup pass). A workspace whose jobId's LATEST journal record is
// non-terminal (state.StateFixRunning — the plan §4.4 step-3b in-flight marker) is left alone:
// main.go's boot-time recovery scan (resumeInFlightJob) needs it to still be there.
//
// "Unknown to the journal" covers the two ways an orphan can appear with no live record at all: a
// crash before journalFixRunning's very first write for that attempt ever landed, or leftover
// clutter from an unrelated/manual directory under the workspace root. Both are safe to remove —
// nothing durable references them.
//
// Best-effort: an individual directory's removal failing does not abort the sweep of the rest; all
// such failures are joined into the returned error, and every entry this call DID successfully
// remove is still reported via removed. A missing workspaceRoot (fresh install, FIX engine never
// run there) is not an error.
func SweepOrphanWorkspaces(workspaceRoot string, journal *state.Journal) (removed []string, err error) {
	entries, readErr := os.ReadDir(workspaceRoot)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, nil
		}
		return nil, fmt.Errorf("jobs: SweepOrphanWorkspaces: reading %s: %w", workspaceRoot, readErr)
	}
	latest, latestErr := journal.LatestByJobID()
	if latestErr != nil {
		return nil, fmt.Errorf("jobs: SweepOrphanWorkspaces: reading journal: %w", latestErr)
	}

	var errs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		jobID := e.Name()
		if rec, known := latest[jobID]; known && !rec.State.IsTerminal() {
			continue // still in-flight (or an in-flight FIX marker) -- keep it for recovery
		}
		if rmErr := os.RemoveAll(filepath.Join(workspaceRoot, jobID)); rmErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", jobID, rmErr))
			continue
		}
		removed = append(removed, jobID)
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("jobs: SweepOrphanWorkspaces: %s", strings.Join(errs, "; "))
	}
	return removed, nil
}

// ReapRetainedWorkspaces deletes workspace directories under workspaceRoot whose modification
// time is older than retentionDays (finding 6's periodic reaper): FixRunner.KeepFailed leaves a
// failed attempt's workspace on disk indefinitely for human inspection, and nothing else ever
// prunes it — this is what makes WORKER_WORKSPACE_RETENTION_DAYS an actually-consumed knob rather
// than a dead one. retentionDays <= 0 disables this pass entirely, matching this codebase's other
// <=0-means-unlimited conventions (e.g. llm.DailyCounter). now is injected (not time.Now()) so
// tests can assert the retention boundary deterministically.
//
// Deliberately mtime-based rather than journal-state-based: a kept-failed workspace's jobId
// typically has NO terminal journal record of its own (fix.go's package doc notes the FIX job's
// jobId is only ever marked terminal via journalFixPRClosed on the PR-merge path — a failed
// attempt that never opened a PR leaves its jobId sitting at state.StateFixRunning forever), so
// SweepOrphanWorkspaces' journal-terminal test alone would never reclaim it. Directory mtime is
// the one signal available for "how long has this actually been sitting here."
func ReapRetainedWorkspaces(workspaceRoot string, retentionDays int, now time.Time) (removed []string, err error) {
	if retentionDays <= 0 {
		return nil, nil
	}
	entries, readErr := os.ReadDir(workspaceRoot)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return nil, nil
		}
		return nil, fmt.Errorf("jobs: ReapRetainedWorkspaces: reading %s: %w", workspaceRoot, readErr)
	}

	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	var errs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		info, infoErr := e.Info()
		if infoErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name(), infoErr))
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if rmErr := os.RemoveAll(filepath.Join(workspaceRoot, e.Name())); rmErr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", e.Name(), rmErr))
			continue
		}
		removed = append(removed, e.Name())
	}
	if len(errs) > 0 {
		return removed, fmt.Errorf("jobs: ReapRetainedWorkspaces: %s", strings.Join(errs, "; "))
	}
	return removed, nil
}
