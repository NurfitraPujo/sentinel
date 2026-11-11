package integration

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// sentinel-issue-tag-facets spec §3.5 and §10.2 "Lock proofs are deterministic, never timing-based": a
// holder takes the issues row lock and keeps its transaction open; the code under test runs on a second
// connection; the test polls until that backend waits on the holder, reads which relations it already
// holds locks on, lets the holder change the state in a way only the correct order survives, and asserts
// exact counts. A mutant that drops or reorders the lock either never waits (the poll fails) or already
// holds a lock it must not hold.

func TestTagFacets_InsertTriggerLocksIssueFirst(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	x := tfSeedIssue(t, env.Pool, projectID, "production")

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer htx.Rollback(ctx)
	// FOR NO KEY UPDATE does not conflict with the insert's FK KEY SHARE: only the trigger's lock waits.
	if _, err := htx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR NO KEY UPDATE`, x); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	b := tfAcquire(t, env.Pool)
	done := make(chan error, 1)
	go func() {
		_, err := tfInsertOccsErr(b, []tfOcc{{IssueID: x, ProjectID: projectID, Release: "b-release"}})
		done <- err
	}()
	tfWaitBlockedBy(t, env.Pool, tfPID(b), tfPID(holder))
	held := tfHeldRelationLocks(t, env.Pool, tfPID(b))
	if !held["error_occurrences:RowExclusiveLock"] {
		t.Fatalf("B should hold RowExclusiveLock on error_occurrences while it waits; holds %v", held)
	}
	for k := range held {
		if len(k) >= len("issue_tag_values:") && k[:len("issue_tag_values:")] == "issue_tag_values:" {
			t.Fatalf("B touched issue_tag_values before taking the issue lock: %v", held)
		}
	}
	// The holder inserts its own occurrence of X and commits; B then counts on top of it.
	tfInsertOccs(t, htx, []tfOcc{{IssueID: x, ProjectID: projectID, Release: "9.1"}})
	if err := htx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("B insert: %v", err)
	}
	if tfCount(t, env.Pool, x, "release", "9.1") != 1 || tfCount(t, env.Pool, x, "release", "b-release") != 1 {
		t.Fatalf("release 9.1 = %d, b-release = %d; want 1 and 1",
			tfCount(t, env.Pool, x, "release", "9.1"), tfCount(t, env.Pool, x, "release", "b-release"))
	}
	if got := tfCount(t, env.Pool, x, "environment", "production"); got != 2 {
		t.Fatalf("environment=production %d, want 2", got)
	}
	tfCheck(t, env.Pool, x)
}

func TestTagFacets_DeleteTriggerLocksIssueFirst(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	x := tfSeedIssue(t, env.Pool, projectID, "production")
	ids := tfInsertOccs(t, env.Pool, []tfOcc{
		{IssueID: x, ProjectID: projectID, Release: "1.0"},
		{IssueID: x, ProjectID: projectID, Release: "1.0"},
		{IssueID: x, ProjectID: projectID, Release: "1.0"},
	})

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer htx.Rollback(ctx)
	if _, err := htx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR NO KEY UPDATE`, x); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	b := tfAcquire(t, env.Pool)
	done := make(chan error, 1)
	go func() {
		_, err := b.Exec(ctx, `DELETE FROM error_occurrences WHERE id = $1`, ids[0])
		done <- err
	}()
	tfWaitBlockedBy(t, env.Pool, tfPID(b), tfPID(holder))
	for k := range tfHeldRelationLocks(t, env.Pool, tfPID(b)) {
		if len(k) >= len("issue_tag_values:") && k[:len("issue_tag_values:")] == "issue_tag_values:" {
			t.Fatalf("B touched issue_tag_values before taking the issue lock: %s", k)
		}
	}
	if _, err := htx.Exec(ctx, `DELETE FROM error_occurrences WHERE id = $1`, ids[1]); err != nil {
		t.Fatalf("holder delete: %v", err)
	}
	if err := htx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("B delete: %v", err)
	}
	if got := tfCount(t, env.Pool, x, "release", "1.0"); got != 1 {
		t.Fatalf("release=1.0 %d, want 1 (both deletes subtracted exactly)", got)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values_drift`); n != 0 {
		t.Fatalf("%d drift row(s); two deletes on one issue must not drift", n)
	}
	tfCheck(t, env.Pool, x)
}

func TestTagFacets_SweepTwinLocksIssuesBeforeDelete(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	x := tfSeedIssue(t, env.Pool, projectID, "production")
	y := tfSeedIssue(t, env.Pool, projectID, "production")
	aged := time.Now().UTC().Add(-40 * 24 * time.Hour)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	var occs []tfOcc
	for i := 0; i < 10; i++ {
		occs = append(occs, tfOcc{IssueID: []string{x, y}[i%2], ProjectID: projectID, Release: "old", CreatedAt: aged.Add(time.Duration(i) * time.Second)})
	}
	occs = append(occs, tfOcc{IssueID: x, ProjectID: projectID, Release: "new"})
	tfInsertOccs(t, env.Pool, occs)

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer htx.Rollback(ctx)
	if _, err := htx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR UPDATE`, x); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	b := tfAcquire(t, env.Pool)
	type res struct {
		n   int
		err error
	}
	done := make(chan res, 1)
	go func() {
		n, _, err := tfSweepTwinBatch(ctx, b, cutoff, nil, 200)
		done <- res{n, err}
	}()
	tfWaitBlockedBy(t, env.Pool, tfPID(b), tfPID(holder))
	held := tfHeldRelationLocks(t, env.Pool, tfPID(b))
	if held["error_occurrences:RowExclusiveLock"] || held["issue_tag_values:RowExclusiveLock"] {
		t.Fatalf("the sweep deleted before it held every issue lock: %v", held)
	}
	if err := htx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	r := <-done
	if r.err != nil || r.n != 10 {
		t.Fatalf("sweep batch: %d deleted, %v; want 10", r.n, r.err)
	}
	if tfCount(t, env.Pool, x, "release", "old") != 0 || tfCount(t, env.Pool, x, "release", "new") != 1 {
		t.Fatalf("after the batch: old %d (want 0), new %d (want 1)", tfCount(t, env.Pool, x, "release", "old"), tfCount(t, env.Pool, x, "release", "new"))
	}
	tfCheck(t, env.Pool, x, y)
}

// A stress test, not a mutation proof (spec §10.2): concurrent StoreEvents on one issue while sweep
// batches delete its aged rows end with no deadlock and an exact rollup.
func TestTagFacets_ConcurrentSameIssue(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	projectID := tfSeedProject(t, env.Pool)
	first, _ := tfStore(t, env, projectID, tfEvent{Environment: "production", Fingerprint: "fp-hot", Release: "1.0", EventID: "seed"})
	aged := time.Now().UTC().Add(-40 * 24 * time.Hour)
	var occs []tfOcc
	for i := 0; i < 400; i++ {
		occs = append(occs, tfOcc{IssueID: first, ProjectID: projectID, Release: fmt.Sprintf("old-%d", i%7), CreatedAt: aged,
			Tags: fmt.Sprintf(`{"k":"v%d"}`, i%13)})
	}
	tfInsertOccs(t, env.Pool, occs)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				ev := tfEvent{Environment: "production", Fingerprint: "fp-hot", Release: fmt.Sprintf("1.%d", i%3),
					EventID: fmt.Sprintf("g%d-%d", g, i), Tags: fmt.Sprintf(`{"k":"v%d"}`, (g+i)%13), User: tfUser(fmt.Sprintf("u%d", i%5))}
				if _, _, err := tfStoreErr(env, projectID, ev); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := env.Pool.Acquire(context.Background())
		if err != nil {
			errs <- err
			return
		}
		defer c.Release()
		var after *tfSweepCursor
		for {
			_, next, err := tfSweepTwinBatch(context.Background(), c, time.Now().UTC().Add(-30*24*time.Hour), after, 25)
			if err != nil {
				errs <- err
				return
			}
			if next == nil {
				return
			}
			after = next
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("deadlock: %v", err)
		}
		t.Fatalf("concurrent run: %v", err)
	}
	tfCheck(t, env.Pool, first)
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM error_occurrences WHERE issue_id = $1`, first); n != 401 {
		t.Fatalf("%d occurrences left, want 401 (1 seed + 400 stored, every aged row swept)", n)
	}
}
