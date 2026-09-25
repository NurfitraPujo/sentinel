package integration

import (
	"context"
	"database/sql"
	"testing"
)

// sentinel-issue-tag-facets spec §3.4 W11 (contract decision 9, "drift is repaired, not only warned
// about"): a subtraction that finds nothing, or too little, to subtract from records a drift row and
// rebuilds the issue inline within a 10,000-occurrence budget; a larger issue is deferred.

type tfDriftRow struct {
	Contributions int64
	Source        string
	Rebuilt       bool
}

func tfDriftRows(t *testing.T, env *tagFacetsEnv, issueID string) []tfDriftRow {
	t.Helper()
	rows, err := env.Pool.Query(context.Background(),
		`SELECT contributions, source, rebuilt_at IS NOT NULL FROM issue_tag_values_drift WHERE issue_id = $1 ORDER BY id`, issueID)
	if err != nil {
		t.Fatalf("drift rows: %v", err)
	}
	defer rows.Close()
	var out []tfDriftRow
	for rows.Next() {
		var r tfDriftRow
		if err := rows.Scan(&r.Contributions, &r.Source, &r.Rebuilt); err != nil {
			t.Fatalf("drift scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func TestTagFacets_DriftSelfHeals(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	x := tfSeedIssue(t, env.Pool, projectID, "production")
	var occs []tfOcc
	for _, r := range []string{"1.3", "1.3", "1.3", "1.3", "1.4", "1.4", "1.5"} {
		occs = append(occs, tfOcc{IssueID: x, ProjectID: projectID, Release: r})
	}
	ids := tfInsertOccs(t, env.Pool, occs)
	tfCheck(t, env.Pool, x)

	// Drift by hand: 1.3's row at 0, 1.4's row gone.
	if _, err := env.Pool.Exec(ctx, `UPDATE issue_tag_values SET count = 0 WHERE issue_id = $1 AND key = 'release' AND value = '1.3'`, x); err != nil {
		t.Fatalf("tamper 1.3: %v", err)
	}
	if _, err := env.Pool.Exec(ctx, `DELETE FROM issue_tag_values WHERE issue_id = $1 AND key = 'release' AND value = '1.4'`, x); err != nil {
		t.Fatalf("tamper 1.4: %v", err)
	}
	// Delete 2 occurrences of 1.3 and 1 of 1.4 in one statement.
	if _, err := env.Pool.Exec(ctx, `DELETE FROM error_occurrences WHERE id = ANY($1::uuid[])`, []string{ids[0], ids[1], ids[4]}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	dr := tfDriftRows(t, env, x)
	if len(dr) != 1 || dr[0].Contributions != 3 || dr[0].Source != "delete" || !dr[0].Rebuilt {
		t.Fatalf("drift rows %+v, want one {3 delete rebuilt}", dr)
	}
	if tfCount(t, env.Pool, x, "release", "1.3") != 2 || tfCount(t, env.Pool, x, "release", "1.4") != 1 {
		t.Fatalf("after the self-heal: 1.3 %d (want 2), 1.4 %d (want 1)", tfCount(t, env.Pool, x, "release", "1.3"), tfCount(t, env.Pool, x, "release", "1.4"))
	}
	tfCheck(t, env.Pool, x)
}

func TestTagFacets_DriftSelfHealsOnUpdate(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	x := tfSeedIssue(t, env.Pool, projectID, "production")
	ids := tfInsertOccs(t, env.Pool, []tfOcc{
		{IssueID: x, ProjectID: projectID, Release: "1.5"},
		{IssueID: x, ProjectID: projectID, Release: "1.5"},
		{IssueID: x, ProjectID: projectID, Release: "1.6"},
	})
	if _, err := env.Pool.Exec(ctx, `DELETE FROM issue_tag_values WHERE issue_id = $1 AND key = 'release' AND value = '1.5'`, x); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := env.Pool.Exec(ctx, `UPDATE error_occurrences SET release_version = '9.9' WHERE id = $1`, ids[0]); err != nil {
		t.Fatalf("update: %v", err)
	}
	dr := tfDriftRows(t, env, x)
	if len(dr) != 1 || dr[0].Source != "update" || dr[0].Contributions != 1 || !dr[0].Rebuilt {
		t.Fatalf("drift rows %+v, want one {1 update rebuilt}", dr)
	}
	// The rebuild already counted the post-update row: 9.9 must count exactly 1, never 2.
	if got := tfCount(t, env.Pool, x, "release", "9.9"); got != 1 {
		t.Fatalf("release=9.9 %d, want 1", got)
	}
	if got := tfCount(t, env.Pool, x, "release", "1.5"); got != 1 {
		t.Fatalf("release=1.5 %d, want 1", got)
	}
	tfCheck(t, env.Pool, x)
}

func TestTagFacets_DriftDeferredForLargeIssue(t *testing.T) {
	env := tagFacetsDB(t, tagFacetsOpts{})
	ctx := context.Background()
	projectID := tfSeedProject(t, env.Pool)
	small := tfSeedIssue(t, env.Pool, projectID, "production")
	large := tfSeedIssue(t, env.Pool, projectID, "production")
	seed := func(issue string, n int) string {
		var drifted string
		for off := 0; off < n; off += 2000 {
			var occs []tfOcc
			for i := off; i < n && i < off+2000; i++ {
				r := "bulk"
				if i < 3 {
					r = "drifted"
				}
				occs = append(occs, tfOcc{IssueID: issue, ProjectID: projectID, Release: r})
			}
			ids := tfInsertOccs(t, env.Pool, occs)
			if off == 0 {
				drifted = ids[0]
			}
		}
		return drifted
	}
	smallOcc := seed(small, 200)
	largeOcc := seed(large, 10500)
	tfCheck(t, env.Pool, small, large)
	if _, err := env.Pool.Exec(ctx, `DELETE FROM issue_tag_values WHERE issue_id = ANY($1::uuid[]) AND key = 'release' AND value = 'drifted'`,
		[]string{small, large}); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	// ONE statement removes a drifted occurrence from each issue.
	if _, err := env.Pool.Exec(ctx, `DELETE FROM error_occurrences WHERE id = ANY($1::uuid[])`, []string{smallOcc, largeOcc}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	ds, dl := tfDriftRows(t, env, small), tfDriftRows(t, env, large)
	if len(ds) != 1 || !ds[0].Rebuilt {
		t.Fatalf("small issue drift %+v, want one row rebuilt inline", ds)
	}
	tfCheck(t, env.Pool, small)
	if len(dl) != 1 || dl[0].Rebuilt {
		t.Fatalf("large issue drift %+v, want one row deferred (rebuilt_at NULL)", dl)
	}
	if n := tfScalar(t, env.Pool, `SELECT count(*) FROM issue_tag_values WHERE issue_id = $1 AND key = 'release' AND value = 'drifted'`, large); n != 0 {
		t.Fatalf("the large issue was rebuilt inside the statement (its drifted row is back)")
	}
	var total sql.NullInt64
	if err := env.Pool.QueryRow(ctx, `SELECT issue_tag_values_rebuild($1)`, large).Scan(&total); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	tfCheck(t, env.Pool, small, large)
	if got := tfCount(t, env.Pool, large, "release", "drifted"); got != 2 {
		t.Fatalf("large release=drifted %d, want 2", got)
	}
	if dl := tfDriftRows(t, env, large); len(dl) != 1 || !dl[0].Rebuilt {
		t.Fatalf("after the operator rebuild the large issue's drift row is %+v", dl)
	}
}
