package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/NurfitraPujo/sentinel/apps/processor-go/symbolicate"
	"github.com/stretchr/testify/require"
)

// sentinel-sourcemaps contract §11, the tenancy case of the `POST /api/sourcemaps` row, asserted
// through `symbolicate`'s lookup on a seeded database (be.md task 5): a `sourcemaps` row seeded
// under project A for (R, path) is NOT returned by Store.Lookup(B, R, path). §4.4 step 3 makes the
// processor read one tenant's uploaded source paths and function names into another tenant's
// issue if this lookup were ever keyed wrongly (risk surface `tenancy`), so the query is pinned
// here against the real table and the real index, not a stub.
//
// Runs under FORCE_TESTCONTAINERS=1 on a disposable Postgres (never the shared compose one --
// CLAUDE.md: the goose ledgers share one physical database). It creates every row it asserts on.
func TestSymbolicateStore_LookupIsScopedToTheResolvedProject(t *testing.T) {
	s, pool := storeFromTest(t)
	projectA, _ := seedProjectU13(t, pool)
	projectB, _ := seedProjectU13(t, pool)
	require.NotEqual(t, projectA, projectB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const (
		release = "1.2.3"
		path    = "/assets/index-3f2a1c.js"
	)
	raw := []byte(`{"version":3,"sources":["../../src/main.ts"],"names":[],"mappings":"AAAA"}`)
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	storageKey := fmt.Sprintf("sourcemaps/%s/r/%d-%s.map", projectA, time.Now().UnixNano(), sha[:16])

	var rowID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO sourcemaps (project_id, release_version, minified_path, storage_key, size_bytes, sha256)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id::text`,
		projectA, release, path, storageKey, len(raw), sha,
	).Scan(&rowID), "seed sourcemap under project A")
	// The projects' ON DELETE CASCADE (seedProjectU13's cleanup) removes the row.

	// The processor wires the pg method as a symbolicate.Store exactly like this (task 6); the
	// assertion goes through that interface so it is the stage's own lookup being pinned.
	var lookup symbolicate.Store = symbolicate.StoreFunc(s.LookupSourcemap)

	t.Run("project A finds its own row", func(t *testing.T) {
		got, err := lookup.Lookup(ctx, projectA, release, path)
		require.NoError(t, err)
		require.NotNil(t, got, "the seeded row must be returned for its own project")
		require.Equal(t, rowID, got.ID)
		require.Equal(t, projectA, got.ProjectID)
		require.Equal(t, release, got.ReleaseVersion)
		require.Equal(t, path, got.MinifiedPath)
		require.Equal(t, storageKey, got.StorageKey)
		require.Equal(t, int64(len(raw)), got.SizeBytes)
		require.Equal(t, sha, got.SHA256)
	})

	t.Run("project B does not see project A's row for the same release and path", func(t *testing.T) {
		got, err := lookup.Lookup(ctx, projectB, release, path)
		require.NoError(t, err, "a missing row is (nil, nil), never an error")
		require.Nil(t, got, "project A's map must be unreachable from project B's resolved id")
	})

	t.Run("the other two key columns are exact too", func(t *testing.T) {
		got, err := lookup.Lookup(ctx, projectA, release+"-rc1", path)
		require.NoError(t, err)
		require.Nil(t, got, "a different release must not match")
		got, err = lookup.Lookup(ctx, projectA, release, "/assets/index-9b0d4e.js")
		require.NoError(t, err)
		require.Nil(t, got, "a different path must not match")
		got, err = lookup.Lookup(ctx, projectA, release, "https://app.example.com"+path)
		require.NoError(t, err)
		require.Nil(t, got, "the lookup is exact on the NORMALISED path; the caller normalises (decision 22)")
	})
}

// Compile-time pin: the store package's IssueStore carries LookupSourcemap with symbolicate's
// Lookup signature, so main.go can wire symbolicate.StoreFunc(store.LookupSourcemap) without an
// adapter of its own.
var _ = func(s store.IssueStore) symbolicate.Store { return symbolicate.StoreFunc(s.LookupSourcemap) }
