//go:build !m2_nobucket

package store

// writeOccurrenceBucket gates StoreEvent's step 4b (sentinel-issue-trend §4.3, decision 20). It is
// a compile-time constant, not state: true in every build except §13 M2's baseline run, which
// flips it with `go test -tags m2_nobucket` (bucket_write_off.go) so run (i) is the same test, same
// database and same inputs as run (iii) with one constant folded away — the shipped binary has no
// branch, no flag and no environment variable here. The tag is never set in Dockerfile, CI or
// Taskfile.yml; only the M2 sibling loop's baseline invocation passes it.
const writeOccurrenceBucket = true
