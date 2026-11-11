//go:build m1_norelease

package store

// The §13 M1 baseline build (decision 11): `go test -tags m1_norelease` compiles StoreEvent's step 0
// reads and step 4c upsert out so run (i) measures shape (iii)'s inputs without the `releases`
// hot-row write. Never set this tag in Dockerfile, CI or Taskfile.yml — a binary built with it
// stores occurrences, stamps first_release_version, and writes no `releases` row.
const writeRelease = false
