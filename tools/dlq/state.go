package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// replayRecord tracks how many times drain mode (-drain) has replayed a given dead-lettered message's
// content, across process restarts and scheduled runs.
//
// Why this needs to exist at all: JetStream headers cannot carry this by themselves. When a replayed
// message fails again and is re-dead-lettered, packages/shared-go/nats.Subscriber.deadLetter builds a
// BRAND NEW nats.Header from scratch (X-Sentinel-Dlq-Reason/Attempts/Source-Subject/Class) and copies
// only msg.Data into the new dead letter — none of the previous cycle's headers, including anything this
// tool stamped on the message it republished, survive into the new dead letter. X-Sentinel-Dlq-Attempts
// is not a substitute either: it counts redelivery attempts within ONE dead-letter cycle (bounded by the
// consumer's server-side MaxDeliver, currently 7 — see subscriber.go's defaultMaxDeliver), not how many
// times this tool has replayed the message across separate drain runs.
//
// So the replay count is tracked here instead, outside JetStream, in a small JSON file keyed by a
// SHA-256 hash of the message body. That works because a replay republishes msg.Data byte-for-byte onto
// the original subject (see run()'s js.Publish call) — if the same underlying event fails and gets
// dead-lettered again, the new dead letter's Data is identical to what was replayed, so it hashes to the
// same key and finds the same record.
type replayRecord struct {
	Count          int       `json:"count"`
	LastReplayedAt time.Time `json:"last_replayed_at"`
	LastSeq        uint64    `json:"last_seq"`
}

// replayState is the on-disk shape of -state-file.
type replayState struct {
	// Records maps a hex-encoded SHA-256 of a dead-lettered message's body to what drain mode knows
	// about replaying it so far.
	Records map[string]replayRecord `json:"records"`
}

// contentHash identifies a dead-lettered message across replay cycles by the bytes of its body, since
// that is the only thing guaranteed to survive a replay -> re-dead-letter round trip unchanged.
func contentHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// loadState reads the drain state file. A missing file is not an error — it means drain has never run
// (or never replayed anything) against this path yet, so it starts from an empty record set.
func loadState(path string) (*replayState, error) {
	st := &replayState{Records: map[string]replayRecord{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if st.Records == nil {
		st.Records = map[string]replayRecord{}
	}
	return st, nil
}

// saveState writes the state file atomically: write to a temp file in the same directory, then rename
// over the target. A process killed mid-write (SIGKILL from an orchestrator, an OOM) must never leave a
// truncated/corrupt state file behind, because a corrupt state file would make drain mode fail closed on
// every subsequent run (loadState's json.Unmarshal would error), not silently lose the cap.
func saveState(path string, st *replayState) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".dlq-drain-state-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds; cleans up on any early return

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, path, err)
	}
	return nil
}
