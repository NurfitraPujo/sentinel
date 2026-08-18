package state

import "context"

// Snapshotter is the plan §2.8 state-snapshot backend contract (`WORKER_SNAPSHOT_BACKEND = none |
// s3`). N8a defines the interface and ships only the no-op `none` implementation; the `s3` backend
// (hand-rolled SigV4, per plan §2.8) is a LATER phase.
//
// Upload persists one generation of a tarball of the state dir (cursor.json + jobs.journal;
// agent-key.json is explicitly excluded — plan §2.5 requires a restored snapshot never resurrect
// a rotated-away key, so callers must not include it in tarball). generation is a monotonically
// increasing counter the backend uses for the plan §2.8 stale-writer guard: a backend must never
// let an upload of generation G persist as "latest" once a higher generation has already been
// uploaded, so a late-dying old process can never clobber a newer snapshot.
//
// RestoreLatest fetches the newest uploaded tarball, or reports (nil, false, nil) when the backend
// has nothing to restore (e.g. first-ever boot against a fresh bucket, or the `none` backend).
type Snapshotter interface {
	Upload(ctx context.Context, generation int64, tarball []byte) error
	RestoreLatest(ctx context.Context) (tarball []byte, generation int64, found bool, err error)
}

// NoneSnapshotter is the `WORKER_SNAPSHOT_BACKEND=none` implementation: every operation is a no-op
// success. Used when durability across a lost state volume is not required (or not yet wired up),
// and as the zero-value default so wiring code never needs a nil check.
type NoneSnapshotter struct{}

var _ Snapshotter = NoneSnapshotter{}

// Upload discards the tarball and reports success — there is nowhere to put it.
func (NoneSnapshotter) Upload(ctx context.Context, generation int64, tarball []byte) error {
	return nil
}

// RestoreLatest always reports nothing found: the `none` backend has never stored anything.
func (NoneSnapshotter) RestoreLatest(ctx context.Context) ([]byte, int64, bool, error) {
	return nil, 0, false, nil
}
