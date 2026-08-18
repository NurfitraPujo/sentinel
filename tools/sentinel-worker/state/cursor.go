// Package state implements sentinel-worker's on-disk durability layer (plan §2): the events-feed
// cursor and the append-only job journal, both written with the atomic tmp-file + os.Rename
// pattern copied from tools/dlq/state.go (see cursor.go/journal.go headers) so a process killed
// mid-write never leaves a corrupt file behind.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Cursor is the on-disk shape of cursor.json (plan §2.1): the last events-feed seq the worker has
// fully enqueued into the journal, advanced ONLY after that enqueue completes.
type Cursor struct {
	Seq       int64     `json:"cursor"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// LoadCursor reads $WORKER_STATE_DIR/cursor.json. A missing file is not an error — it signals a
// fresh install or a lost state volume, and the caller (loop/poll.go) must run the bootstrap sweep
// (plan §2.1) instead of paging from seq 0. A file that fails to parse is treated the same way
// (corrupt cursor.json), but the error is returned so the caller can log it loudly.
func LoadCursor(path string) (*Cursor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &c, nil
}

// renameFunc performs the final atomic-publish step of SaveCursor. It is a package variable
// (rather than a hardcoded os.Rename call) purely so tests can observe the tmp-file-exists,
// target-not-yet-updated window that exists between the write and the rename — the actual
// production path always uses os.Rename (see the default below).
var renameFunc = os.Rename

// SaveCursor writes cursor.json atomically: temp file in the same directory, then os.Rename over
// the target (copied from tools/dlq/state.go:saveState — explicitly NOT the CLI's non-atomic
// os.WriteFile, per plan §2.1).
func SaveCursor(path string, seq int64) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	c := Cursor{Seq: seq, UpdatedAt: time.Now().UTC()}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling cursor: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".cursor-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds; cleans up on any early return

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := renameFunc(tmpPath, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpPath, path, err)
	}
	fsyncDir(dir)
	return nil
}

// fsyncDir fsyncs a directory so a rename into it is durable across a crash, not just the file it
// points at. Best-effort: some platforms/filesystems don't support fsync on a directory handle, and
// failing the whole write over that would be worse than the (already rare) window it closes.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
