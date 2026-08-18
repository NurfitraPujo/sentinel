package state

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AgentLogPath returns the path a Fix Executor's stdout log for jobID should be written to, under
// $WORKER_STATE_DIR/agent-logs/ (plan §2.2: "Coding-agent stdout does NOT go into the journal — it
// streams to $WORKER_STATE_DIR/agent-logs/<jobId>.log"). It does not create the directory or file;
// callers append to it as the Fix Executor produces output.
func AgentLogPath(stateDir, jobID string) string {
	return filepath.Join(stateDir, "agent-logs", jobID+".log")
}

// ReapAgentLogs enforces the plan §2.2 agent-logs retention rule: any log file whose jobId has a
// terminal journal record older than olderThan is deleted, mirroring journal Compact's 7-day
// reaping. It also enforces the size cap (WORKER_AGENT_LOG_MAX_MB, maxBytesPerFile): any log file
// exceeding maxBytesPerFile is truncated to keep only its final maxBytesPerFile bytes — Fix
// Executor output is a live tail an operator reads for diagnostics, so the most recent bytes
// matter more than the oldest ones. A missing agent-logs directory is not an error.
//
// terminal is the {jobId: latest terminal record time} view a caller derives from
// Journal.LatestByJobID (a log for a jobId that is not terminal, or not present at all — e.g. a
// job the journal itself already compacted away — is left alone if it is not old enough to be
// unambiguously orphaned; ReapAgentLogs only ever deletes logs the caller explicitly names as
// terminal-and-old via terminalJobIDs).
func ReapAgentLogs(stateDir string, terminalJobIDs map[string]time.Time, olderThan time.Time, maxBytesPerFile int64) (deleted int, truncated int, err error) {
	dir := filepath.Join(stateDir, "agent-logs")
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("reading %s: %w", dir, readErr)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		jobID := name[:len(name)-len(filepath.Ext(name))]
		path := filepath.Join(dir, name)

		if at, ok := terminalJobIDs[jobID]; ok && at.Before(olderThan) {
			if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
				return deleted, truncated, fmt.Errorf("removing %s: %w", path, rmErr)
			}
			deleted++
			continue
		}

		if maxBytesPerFile <= 0 {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return deleted, truncated, fmt.Errorf("stat %s: %w", path, statErr)
		}
		if info.Size() <= maxBytesPerFile {
			continue
		}
		if truncErr := truncateKeepingTail(path, info.Size(), maxBytesPerFile); truncErr != nil {
			return deleted, truncated, truncErr
		}
		truncated++
	}
	return deleted, truncated, nil
}

// truncateKeepingTail rewrites path (tmp+rename, same crash-safety pattern as cursor.go) to keep
// only its last keep bytes.
func truncateKeepingTail(path string, size, keep int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	if _, err := f.Seek(size-keep, 0); err != nil {
		return fmt.Errorf("seeking %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".agent-log-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := tmp.ReadFrom(f); err != nil {
		tmp.Close()
		return fmt.Errorf("copying tail of %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, path, err)
	}
	fsyncDir(dir)
	return nil
}
