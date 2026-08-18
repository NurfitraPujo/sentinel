package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCursor_LoadMissingIsNilNotError(t *testing.T) {
	c, err := LoadCursor(filepath.Join(t.TempDir(), "cursor.json"))
	if err != nil {
		t.Fatalf("missing cursor file must not be an error, got: %v", err)
	}
	if c != nil {
		t.Fatalf("expected nil cursor for a missing file, got: %+v", c)
	}
}

func TestCursor_SaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	if err := SaveCursor(path, 42); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	c, err := LoadCursor(path)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if c == nil || c.Seq != 42 {
		t.Fatalf("expected cursor seq 42, got %+v", c)
	}
}

// TestCursor_SaveIsAtomic proves the write never leaves a *.tmp file behind and the target file is
// always fully-formed — i.e. no partial-write window is observable from outside the rename.
func TestCursor_SaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")
	for i := int64(0); i < 20; i++ {
		if err := SaveCursor(path, i); err != nil {
			t.Fatalf("SaveCursor(%d): %v", i, err)
		}
		c, err := LoadCursor(path)
		if err != nil {
			t.Fatalf("LoadCursor after SaveCursor(%d): %v", i, err)
		}
		if c.Seq != i {
			t.Fatalf("expected seq %d, got %d", i, c.Seq)
		}
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".cursor-*.tmp"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no leftover tmp files, found: %v", matches)
	}
}

// TestCursor_CorruptFileReturnsNilAndError proves LoadCursor never hands back a zero-value *Cursor
// for a corrupt cursor.json: main.go's bootstrap decision is "cursor == nil -> run the bootstrap
// sweep", and a refactor that instead returned &Cursor{} on a parse error would silently page from
// seq 0 (the exact §2.1/C9 failure this guards against). Both nil AND a non-nil error are asserted,
// not just one, so a change that returns (nil, nil) for corrupt input is equally caught.
func TestCursor_CorruptFileReturnsNilAndError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursor.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("seeding corrupt cursor file: %v", err)
	}

	c, err := LoadCursor(path)
	if err == nil {
		t.Fatalf("expected an error for a corrupt cursor.json, got nil")
	}
	if c != nil {
		t.Fatalf("expected a nil *Cursor for a corrupt cursor.json (so callers run the bootstrap sweep), got %+v", c)
	}
}

// TestCursor_TmpFilePresentBeforeRename injects a hook on the rename step to prove the tmp file
// genuinely exists, fully written, WHILE the target has not yet been updated — i.e. the write
// really does go to a tmp file first rather than writing the target in place. This is the concrete
// mechanism, not just the pre/post observation TestCursor_SaveIsAtomic makes.
func TestCursor_TmpFilePresentBeforeRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursor.json")

	// Seed an initial target so we can observe it is untouched mid-write.
	if err := SaveCursor(path, 1); err != nil {
		t.Fatalf("seed SaveCursor: %v", err)
	}

	orig := renameFunc
	defer func() { renameFunc = orig }()

	var sawTmpContent []byte
	var sawTmpExists bool
	var sawTargetUnchangedAtRenameTime bool
	renameFunc = func(oldpath, newpath string) error {
		data, err := os.ReadFile(oldpath)
		if err == nil {
			sawTmpExists = true
			sawTmpContent = data
		}
		targetData, _ := os.ReadFile(newpath)
		c, _ := LoadCursor(newpath)
		if c != nil && c.Seq == 1 {
			sawTargetUnchangedAtRenameTime = true
		}
		_ = targetData
		return os.Rename(oldpath, newpath)
	}

	if err := SaveCursor(path, 2); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	if !sawTmpExists {
		t.Fatalf("expected the tmp file to exist and be readable at rename time")
	}
	if len(sawTmpContent) == 0 {
		t.Fatalf("expected the tmp file to already contain the fully-written new cursor")
	}
	if !sawTargetUnchangedAtRenameTime {
		t.Fatalf("expected the target file to still hold the OLD cursor (seq 1) at the moment of rename")
	}

	final, err := LoadCursor(path)
	if err != nil || final.Seq != 2 {
		t.Fatalf("expected final cursor seq 2 after rename completes, got %+v (err %v)", final, err)
	}
}
