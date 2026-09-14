package symbolicate

import "context"

// Sourcemap is one `sourcemaps` row (contract §4.5) as the stage needs it: the id keys the
// positive cache, size_bytes is the cache cost AND the parse-cap check (decisions 11, 38),
// storage_key is what the Fetcher GETs and sha256 what the bytes must hash to (§4.4 step 4).
type Sourcemap struct {
	ID             string
	ProjectID      string
	ReleaseVersion string
	MinifiedPath   string
	StorageKey     string
	SizeBytes      int64
	SHA256         string
}

// Store is §4.4 step 3: the lookup on §4.5's UNIQUE (project_id, release_version, minified_path).
// projectID is the RESOLVED project of the event (store.ResolveProjectID's return), never anything
// read from a frame -- the tenancy boundary of §11, proved in tests/integration against a seeded
// database. minifiedPath is the frame's file AFTER NormalizePath.
//
// Lookup returns (nil, nil) when no row matches: that is `no_map`, negatively cached. An error is
// a store failure, not a miss -- the stage reports the map's frames `fetch_error` and does NOT
// negatively cache it (a database blip should not make the map unreachable for the negative
// TTL after the database is back).
type Store interface {
	Lookup(ctx context.Context, projectID, releaseVersion, minifiedPath string) (*Sourcemap, error)
}

// StoreFunc adapts a method with Lookup's signature -- store.pgStore.LookupSourcemap, which lives
// beside ResolveProjectID in apps/processor-go/store -- into a Store, the way http.HandlerFunc
// adapts a function into a Handler. The processor's store cannot itself be named Store here:
// symbolicate must not import apps/processor-go/store (store → procmetrics → … and, once task 6
// lands, event → fingerprint → symbolicate would cycle).
type StoreFunc func(ctx context.Context, projectID, releaseVersion, minifiedPath string) (*Sourcemap, error)

// Lookup calls f.
func (f StoreFunc) Lookup(ctx context.Context, projectID, releaseVersion, minifiedPath string) (*Sourcemap, error) {
	return f(ctx, projectID, releaseVersion, minifiedPath)
}

// Fetcher is §4.4 step 4's object GET: the raw bytes stored under a `sourcemaps.storage_key`.
// The sha256 verification against the row is the stage's, not the Fetcher's, so a stub that
// returns the wrong bytes exercises the `map_error` path (§11). S3Fetcher is the production
// implementation; tests stub it.
type Fetcher interface {
	Fetch(ctx context.Context, storageKey string) ([]byte, error)
}

// FetcherFunc adapts a function into a Fetcher.
type FetcherFunc func(ctx context.Context, storageKey string) ([]byte, error)

// Fetch calls f.
func (f FetcherFunc) Fetch(ctx context.Context, storageKey string) ([]byte, error) {
	return f(ctx, storageKey)
}
