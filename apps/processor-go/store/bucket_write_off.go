//go:build m2_nobucket

package store

// The §13 M2 baseline build (decision 20): `go test -tags m2_nobucket` compiles StoreEvent's step 4b
// out so run (i) measures shape (iii)'s inputs without the bucket upsert. Never set this tag in
// Dockerfile, CI or Taskfile.yml — a binary built with it stores occurrences and writes no bucket.
const writeOccurrenceBucket = false
