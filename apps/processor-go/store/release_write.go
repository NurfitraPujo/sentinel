//go:build !m1_norelease

package store

// writeRelease gates StoreEvent's step 0 (the two plain `releases` reads) and step 4c (the
// `releases` upsert) — sentinel-releases-snooze §4.3, decision 11, the m2_nobucket precedent. It is
// a compile-time constant, not state: true in every build except §13 M1's baseline run, which flips
// it with `go test -tags m1_norelease` (release_write_off.go) so run (i) is the same test, same
// database and same inputs as run (iii) with one constant folded away — the shipped binary has no
// branch, no flag and no environment variable here. The tag is never set in Dockerfile, CI or
// Taskfile.yml; only the M1 driver's baseline invocation passes it. With the reads compiled out
// decideRegression sees rowR and rowX absent on every event and falls back to isRegressionVersion —
// exactly the pre-feature comparison (decision 3).
const writeRelease = true
