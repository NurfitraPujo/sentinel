package integration

import (
	"fmt"
	"testing"
	"time"
)

// sentinel-issue-tag-facets spec §5 and §10.2 (TASK-01 retention, the Go twin, T18): the exact statements
// deleteAgedOccurrences issues (tfSweepCandidates*, tfSweepLockIssues, tfSweepDelete in
// tag_facets_db_test.go), run with a batch of 7 over 50 aged rows: the rollup equals GROUP BY over the
// survivors, rows that reach 0 are deleted, and every seeded aged row is gone. The dashboard suite
// (retention.tag-facets.integration.test.ts) proves the same through the real cleanupRetainedData; the
// lock-order twin is TestTagFacets_SweepTwinLocksIssuesBeforeDelete.
func TestTagFacets_RetentionTwinMatchesGroupBy(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	projectID := tfSeedProject(t, env.Pool)
	a := tfSeedIssue(t, env.Pool, projectID, "production")
	capIssue := tfSeedIssue(t, env.Pool, projectID, "production")
	allAged := tfSeedIssue(t, env.Pool, projectID, "production")
	aged := time.Now().UTC().Add(-31 * 24 * time.Hour)
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)

	// 50 aged rows: a value present only on aged rows, a key whose overflow row holds only aged rows, and
	// an issue whose occurrences all age out.
	var agedOccs []tfOcc
	for i := 0; i < 10; i++ {
		agedOccs = append(agedOccs, tfOcc{IssueID: a, ProjectID: projectID, Release: "only-aged", CreatedAt: aged.Add(time.Duration(i) * time.Second)})
	}
	for i := 0; i < 30; i++ {
		agedOccs = append(agedOccs, tfOcc{IssueID: capIssue, ProjectID: projectID, User: tfUser(fmt.Sprintf("z%02d", i)), CreatedAt: aged.Add(time.Duration(10+i) * time.Second)})
	}
	for i := 0; i < 10; i++ {
		agedOccs = append(agedOccs, tfOcc{IssueID: allAged, ProjectID: projectID, Release: "gone", CreatedAt: aged.Add(time.Duration(40+i) * time.Second)})
	}
	// Fresh rows: 1000 named user ids on capIssue FIRST (so the aged z-values overflow), and survivors on a.
	var fresh []tfOcc
	for i := 0; i < 1000; i++ {
		fresh = append(fresh, tfOcc{IssueID: capIssue, ProjectID: projectID, User: tfUser(fmt.Sprintf("u%04d", i))})
	}
	for i := 0; i < 4; i++ {
		fresh = append(fresh, tfOcc{IssueID: a, ProjectID: projectID, Release: "both"})
	}
	tfInsertOccs(t, env.Pool, fresh)
	agedIDs := tfInsertOccs(t, env.Pool, agedOccs)
	if got := tfCount(t, env.Pool, capIssue, "user.id", ""); got != 30 {
		t.Fatalf("fixture: capIssue's overflow row is %d, want 30", got)
	}
	tfCheck(t, env.Pool, a, capIssue, allAged)

	c := tfAcquire(t, env.Pool)
	deleted, batches := tfSweepTwin(t, c, cutoff, 7)
	if deleted != 50 || batches != 8 {
		t.Fatalf("the twin deleted %d rows in %d batches, want 50 in 8", deleted, batches)
	}
	tfCheck(t, env.Pool, a, capIssue, allAged)
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM error_occurrences WHERE id = ANY($1::uuid[])`, agedIDs); n != 0 {
		t.Fatalf("%d seeded aged row(s) survived", n)
	}
	if got := tfCount(t, env.Pool, a, "release", "only-aged"); got != 0 {
		t.Fatalf("the aged-only value still has a row (%d)", got)
	}
	if got := tfCount(t, env.Pool, a, "release", "both"); got != 4 {
		t.Fatalf("release=both %d, want its 4 survivors", got)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key = 'user.id' AND value = ''`, capIssue); n != 0 {
		t.Fatalf("the overflow row survived its last occurrence")
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key <> 'environment'`, allAged); n != 0 {
		t.Fatalf("an issue whose occurrences all aged out kept %d rollup row(s)", n)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1`, allAged); n != 0 {
		t.Fatalf("an issue whose occurrences all aged out kept %d rollup row(s) (environment included)", n)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE count <= 0`); n != 0 {
		t.Fatalf("%d rollup row(s) at 0", n)
	}
}
