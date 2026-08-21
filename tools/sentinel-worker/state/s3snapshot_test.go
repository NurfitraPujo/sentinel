package state

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// fakeS3 is a minimal in-memory S3 double: PUT stores the body under its path, GET returns it
// (404 if absent), keyed on the full request path (bucket/key). It records every request so tests
// can assert on signing headers and PUT order.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	requests []*http.Request
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: make(map[string][]byte)}
}

func (f *fakeS3) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		// Clone request essentials (body already consumed by server framework timing, so capture
		// headers/method/path only -- sufficient for the assertions below).
		f.requests = append(f.requests, r.Clone(r.Context()))
		f.mu.Unlock()

		switch r.Method {
		case http.MethodPut:
			data, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.objects[r.URL.Path] = data
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			f.mu.Lock()
			data, ok := f.objects[r.URL.Path]
			f.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func newTestSnapshotter(t *testing.T, srv *httptest.Server) *S3Snapshotter {
	t.Helper()
	cfg := S3Config{
		Endpoint:  srv.URL,
		Bucket:    "test-bucket",
		Prefix:    "worker-1",
		Region:    "us-east-1",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: "secretkeyexample",
	}
	return NewS3Snapshotter(cfg, srv.Client())
}

// TestS3Snapshotter_UploadPutsTarballThenLatestPointer proves the plan §2.8 write order: the
// tarball object is PUT before the `latest` pointer, and the pointer body names that generation.
func TestS3Snapshotter_UploadPutsTarballThenLatestPointer(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	s := newTestSnapshotter(t, srv)

	if err := s.Upload(context.Background(), 3, []byte("tarball-bytes")); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) < 2 {
		t.Fatalf("expected at least 2 requests (tarball PUT + latest PUT), got %d", len(fake.requests))
	}
	var sawTar, sawLatest bool
	var tarIdx, latestIdx int
	for i, r := range fake.requests {
		if r.Method != http.MethodPut {
			continue
		}
		switch r.URL.Path {
		case "/test-bucket/worker-1/state-3.tar":
			sawTar = true
			tarIdx = i
		case "/test-bucket/worker-1/latest":
			sawLatest = true
			latestIdx = i
		}
	}
	if !sawTar {
		t.Fatalf("tarball object was never PUT")
	}
	if !sawLatest {
		t.Fatalf("latest pointer object was never PUT")
	}
	if tarIdx > latestIdx {
		t.Fatalf("latest pointer PUT (index %d) happened before the tarball PUT (index %d) -- plan §2.8 requires the pointer written LAST", latestIdx, tarIdx)
	}

	if got := string(fake.objects["/test-bucket/worker-1/state-3.tar"]); got != "tarball-bytes" {
		t.Fatalf("tarball body = %q, want %q", got, "tarball-bytes")
	}
	if got := string(fake.objects["/test-bucket/worker-1/latest"]); got != "3" {
		t.Fatalf("latest pointer body = %q, want %q", got, "3")
	}

	// Requests must be SigV4-signed.
	for _, r := range fake.requests {
		if r.Header.Get("Authorization") == "" {
			t.Fatalf("request %s %s missing Authorization header -- not signed", r.Method, r.URL.Path)
		}
		if r.Header.Get("X-Amz-Date") == "" {
			t.Fatalf("request %s %s missing X-Amz-Date header", r.Method, r.URL.Path)
		}
	}
}

// TestS3Snapshotter_GenerationGuardRejectsStaleUpload proves the stale-writer guard (plan §2.8):
// once generation G has been uploaded, a later Upload call with a generation <= G must be
// rejected and must not touch S3 at all.
func TestS3Snapshotter_GenerationGuardRejectsStaleUpload(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	s := newTestSnapshotter(t, srv)

	if err := s.Upload(context.Background(), 5, []byte("gen5")); err != nil {
		t.Fatalf("Upload gen 5: %v", err)
	}
	fake.mu.Lock()
	before := len(fake.requests)
	fake.mu.Unlock()

	if err := s.Upload(context.Background(), 5, []byte("gen5-again")); err == nil {
		t.Fatalf("Upload of the SAME generation must be rejected by the stale-writer guard")
	}
	if err := s.Upload(context.Background(), 3, []byte("gen3-late")); err == nil {
		t.Fatalf("Upload of a LOWER generation must be rejected by the stale-writer guard")
	}

	fake.mu.Lock()
	after := len(fake.requests)
	fake.mu.Unlock()
	if after != before {
		t.Fatalf("a rejected upload must never reach S3: request count went from %d to %d", before, after)
	}

	// A higher generation still succeeds.
	if err := s.Upload(context.Background(), 6, []byte("gen6")); err != nil {
		t.Fatalf("Upload gen 6 (higher than guard) should succeed: %v", err)
	}
	if got := string(fake.objects["/test-bucket/worker-1/latest"]); got != "6" {
		t.Fatalf("latest pointer body after gen 6 = %q, want %q", got, "6")
	}
}

// TestS3Snapshotter_RestoreLatestFindsNothingOnEmptyBucket proves a fresh bucket (no `latest`
// object yet) reports found=false, not an error.
func TestS3Snapshotter_RestoreLatestFindsNothingOnEmptyBucket(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	s := newTestSnapshotter(t, srv)

	tarball, generation, found, err := s.RestoreLatest(context.Background())
	if err != nil {
		t.Fatalf("RestoreLatest on empty bucket: %v", err)
	}
	if found {
		t.Fatalf("expected found=false on an empty bucket")
	}
	if tarball != nil || generation != 0 {
		t.Fatalf("expected zero-value results, got tarball=%v generation=%d", tarball, generation)
	}
}

// TestS3Snapshotter_RestoreLatestFetchesGenerationNamedByPointer proves RestoreLatest follows the
// `latest` pointer to the correct generation's tarball, and that the guard is updated so a
// subsequent Upload of that same generation is rejected (restore-then-upload-same-gen must not
// silently clobber what was just restored).
func TestS3Snapshotter_RestoreLatestFetchesGenerationNamedByPointer(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	writer := newTestSnapshotter(t, srv)
	if err := writer.Upload(context.Background(), 42, []byte("the-real-state")); err != nil {
		t.Fatalf("seed Upload: %v", err)
	}

	// A FRESH snapshotter (simulating a new pod) restores.
	reader := newTestSnapshotter(t, srv)
	tarball, generation, found, err := reader.RestoreLatest(context.Background())
	if err != nil {
		t.Fatalf("RestoreLatest: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true")
	}
	if generation != 42 {
		t.Fatalf("generation = %d, want 42", generation)
	}
	if string(tarball) != "the-real-state" {
		t.Fatalf("tarball = %q, want %q", tarball, "the-real-state")
	}

	// The restored generation now guards this reader's own future uploads.
	if err := reader.Upload(context.Background(), 42, []byte("stale")); err == nil {
		t.Fatalf("Upload of the restored generation must be rejected (never re-uploads what was just restored)")
	}
}

// TestS3Snapshotter_ConcurrentUploadsAreRaceFree drives Upload from multiple goroutines against
// ONE S3Snapshotter instance (finding 1, core-robustness round 3: main.go's runPeriodic,
// journalMaintenanceLoop, and the SIGTERM handler all call Upload against the SAME instance from
// separate goroutines) with -race, and asserts the stale-writer guard's own invariant survives
// the concurrency: baseGeneration only ever advances, uploads are strictly increasing relative to
// what actually landed, and the fake S3's `latest` pointer never regresses below the highest
// generation that was ever accepted. Run this test file with `go test -race` (per CLAUDE.md's
// gate) -- without S3Snapshotter.mu, `go test -race` flags the concurrent baseGeneration
// read/writes even though the assertions below might still pass by luck.
func TestS3Snapshotter_ConcurrentUploadsAreRaceFree(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	s := newTestSnapshotter(t, srv)

	// Concurrent goroutines racing on generation numbers 1..n do NOT all necessarily succeed --
	// the stale-writer guard is explicitly allowed (and expected, per its own doc comment) to
	// reject a genuinely-lower generation that loses the race against a higher one that landed
	// first; that is the guard doing its job, not a bug. What this test proves is (a) -race finds
	// no data race on baseGeneration under this concurrency (the primary point of finding 1), and
	// (b) whichever uploads DID succeed leave the fake S3's `latest` pointer and this instance's
	// baseGeneration consistent with each other and with the true maximum successfully-uploaded
	// generation -- i.e. no torn read/write ever let baseGeneration and the actual persisted state
	// diverge.
	const n = 50
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(gen int64) {
			defer wg.Done()
			errs[gen-1] = s.Upload(context.Background(), gen, []byte("tarball"))
		}(int64(i + 1))
	}
	wg.Wait()

	var maxSucceeded int64
	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
			if gen := int64(i + 1); gen > maxSucceeded {
				maxSucceeded = gen
			}
		}
	}
	if succeeded == 0 {
		t.Fatalf("expected at least one of %d concurrent uploads to succeed, got 0", n)
	}

	// baseGeneration must equal the highest generation that actually succeeded -- not less (a
	// successful Upload's baseGeneration write got lost/overwritten by a stale one) and not more
	// (baseGeneration advanced past a generation whose Upload never actually succeeded).
	if s.baseGeneration != maxSucceeded {
		t.Fatalf("baseGeneration = %d, want %d (highest of %d successful concurrent uploads)", s.baseGeneration, maxSucceeded, succeeded)
	}
	fake.mu.Lock()
	latestBody := fake.objects["/test-bucket/worker-1/latest"]
	fake.mu.Unlock()
	wantLatest := strconv.FormatInt(maxSucceeded, 10)
	if string(latestBody) != wantLatest {
		t.Fatalf("latest pointer = %q, want %q (must name the same generation baseGeneration settled on)", latestBody, wantLatest)
	}

	// Every subsequent Upload of any generation <= what's already landed must still be rejected --
	// the guard's post-concurrency steady-state behavior must be exactly as if the winning
	// generation had been uploaded non-concurrently all along.
	if err := s.Upload(context.Background(), maxSucceeded, []byte("stale-replay")); err == nil {
		t.Fatalf("re-uploading the winning generation %d after the race settled must still be rejected", maxSucceeded)
	}
}

// TestS3Snapshotter_SeedGenerationAdvancesBaseGeneration is the mutation-test proof for finding 2
// (core-robustness round 3): SeedGeneration must fetch the S3 `latest` pointer and raise
// baseGeneration to match, independent of whether RestoreLatest was ever called -- the scenario
// this exists for is a restart whose LOCAL state dir survived (so restoreIfEmpty's restore-on-
// empty trigger never fires), but another writer has since pushed S3's latest generation higher
// than this process's own local nextGen counter.
func TestS3Snapshotter_SeedGenerationAdvancesBaseGeneration(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	writer := newTestSnapshotter(t, srv)
	if err := writer.Upload(context.Background(), 7, []byte("state")); err != nil {
		t.Fatalf("seed Upload: %v", err)
	}

	// A FRESH snapshotter (simulating a restart whose LOCAL state survived, so RestoreLatest was
	// never called) seeds from S3's latest without ever restoring anything.
	fresh := newTestSnapshotter(t, srv)
	seeded, err := fresh.SeedGeneration(context.Background())
	if err != nil {
		t.Fatalf("SeedGeneration: %v", err)
	}
	if seeded != 7 {
		t.Fatalf("SeedGeneration returned %d, want 7", seeded)
	}
	if fresh.baseGeneration != 7 {
		t.Fatalf("baseGeneration = %d, want 7 (mutation: SeedGeneration not actually advancing baseGeneration)", fresh.baseGeneration)
	}
	// Proves the seed is load-bearing, not decorative: a local nextGen counter that (wrongly)
	// still thinks generation 3 is next must now be rejected by the stale-writer guard, since S3's
	// real latest is 7.
	if err := fresh.Upload(context.Background(), 3, []byte("collision")); err == nil {
		t.Fatalf("Upload(gen=3) after seeding from a higher S3 latest (7) must be rejected by the stale-writer guard")
	}
}

// TestS3Snapshotter_SeedGenerationOnEmptyBucketIsNoop proves SeedGeneration is a harmless no-op
// (0, nil) against a fresh bucket with no `latest` pointer yet -- first boot must not error out.
func TestS3Snapshotter_SeedGenerationOnEmptyBucketIsNoop(t *testing.T) {
	fake := newFakeS3()
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	s := newTestSnapshotter(t, srv)

	seeded, err := s.SeedGeneration(context.Background())
	if err != nil {
		t.Fatalf("SeedGeneration on empty bucket: %v", err)
	}
	if seeded != 0 {
		t.Fatalf("seeded = %d, want 0", seeded)
	}
	if s.baseGeneration != 0 {
		t.Fatalf("baseGeneration = %d, want 0", s.baseGeneration)
	}
}

// TestBuildStateTarball_ExcludesAgentKey proves the plan §2.5/§2.8 durability contract: a state
// dir containing agent-key.json alongside cursor.json/jobs.journal produces a tarball with NO
// agent-key.json entry, so restoring an old snapshot can never resurrect a rotated-away key.
func TestBuildStateTarball_ExcludesAgentKey(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "cursor.json"), `{"cursor":7}`)
	mustWriteFile(t, filepath.Join(dir, "jobs.journal"), `{"jobId":"a"}`+"\n")
	mustWriteFile(t, filepath.Join(dir, "agent-key.json"), `{"key":"super-secret"}`)

	tarball, err := BuildStateTarball(dir)
	if err != nil {
		t.Fatalf("BuildStateTarball: %v", err)
	}
	if len(tarball) == 0 {
		t.Fatalf("expected a non-empty tarball")
	}

	// Round-trip through extraction into a fresh dir and confirm agent-key.json never appears.
	restoreDir := t.TempDir()
	if err := ExtractStateTarball(restoreDir, tarball); err != nil {
		t.Fatalf("ExtractStateTarball: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restoreDir, "agent-key.json")); !os.IsNotExist(err) {
		t.Fatalf("agent-key.json must NEVER appear in a restored snapshot, stat err = %v", err)
	}
	cursorData, err := os.ReadFile(filepath.Join(restoreDir, "cursor.json"))
	if err != nil {
		t.Fatalf("reading restored cursor.json: %v", err)
	}
	if string(cursorData) != `{"cursor":7}` {
		t.Fatalf("restored cursor.json = %q, want the original content", cursorData)
	}
	journalData, err := os.ReadFile(filepath.Join(restoreDir, "jobs.journal"))
	if err != nil {
		t.Fatalf("reading restored jobs.journal: %v", err)
	}
	if string(journalData) != `{"jobId":"a"}`+"\n" {
		t.Fatalf("restored jobs.journal = %q, want the original content", journalData)
	}
}

// TestBuildStateTarball_MissingFilesSkipped proves a state dir with no journal yet (fresh
// install) still produces a valid (possibly empty) tarball rather than erroring.
func TestBuildStateTarball_MissingFilesSkipped(t *testing.T) {
	dir := t.TempDir()
	tarball, err := BuildStateTarball(dir)
	if err != nil {
		t.Fatalf("BuildStateTarball on empty dir: %v", err)
	}
	restoreDir := t.TempDir()
	if err := ExtractStateTarball(restoreDir, tarball); err != nil {
		t.Fatalf("ExtractStateTarball of an empty tarball: %v", err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
