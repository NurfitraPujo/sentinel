package state

import (
	"context"
	"testing"
)

// TestNoneSnapshotter_UploadIsNoop proves the WORKER_SNAPSHOT_BACKEND=none implementation never
// errors and never claims to have stored anything durable.
func TestNoneSnapshotter_UploadIsNoop(t *testing.T) {
	var s Snapshotter = NoneSnapshotter{}
	if err := s.Upload(context.Background(), 1, []byte("tarball")); err != nil {
		t.Fatalf("NoneSnapshotter.Upload must never error, got: %v", err)
	}
}

func TestNoneSnapshotter_RestoreLatestFindsNothing(t *testing.T) {
	var s Snapshotter = NoneSnapshotter{}
	tarball, generation, found, err := s.RestoreLatest(context.Background())
	if err != nil {
		t.Fatalf("RestoreLatest must never error, got: %v", err)
	}
	if found {
		t.Fatalf("NoneSnapshotter must never report a snapshot found")
	}
	if tarball != nil || generation != 0 {
		t.Fatalf("expected zero-value results when nothing found, got tarball=%v generation=%d", tarball, generation)
	}
}
