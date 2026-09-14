//go:build m1_norelease

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoreEvent_ReleaseWriteCompiledOut exists only in the sentinel-releases-snooze §13 M1
// baseline build (`go test -tags m1_norelease`, decision 11): with store's writeRelease constant
// folded to false, StoreEvent stores the occurrence, stamps first_release_version (the INSERT
// column is not gated — only step 0's reads and step 4c's upsert are), and writes NO `releases`
// row. It is the one case the item's acceptance names for the tag; the M1 driver's run (i) is the
// timing take on the same build. In the shipped build this file does not compile in, and
// TestStoreEvent_ReleaseFirstOccurrenceInsertsRowAndStampsIssue asserts the opposite — the two
// cannot both be green in one build, which is the point: neither build can silently run the
// other's shape (the trend_m2_timing_test.go precedent).
func TestStoreEvent_ReleaseWriteCompiledOut(t *testing.T) {
	s, pool := newEIDStoreAndPoolFromEnv(t)
	projectID := eidSeedProject(t, pool, "m1norelease")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	fp := eidFingerprint(t, "m1norelease")
	const release = "v9.9.9"
	occ := releaseOccurrence(release, time.Now().UTC())
	issue := eidIssueIn(projectID, "production", fp)
	outcome, stored, err := s.StoreEvent(ctx, issue, occ, release)
	require.NoError(t, err)
	require.True(t, stored, "the occurrence is stored")
	assert.Equal(t, store.IssueOutcomeNew, outcome)
	assert.False(t, occ.ReleaseInserted, "no upsert ran, so nothing was inserted")

	var occurrences int
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM error_occurrences WHERE issue_id = $1`, issue.ID).Scan(&occurrences))
	assert.Equal(t, 1, occurrences)

	assert.False(t, releaseRowOf(t, ctx, pool, projectID, release).found,
		"-tags m1_norelease: a stored occurrence with no releases row")

	st := issueSnoozeStateOf(t, ctx, pool, issue.ID)
	require.NotNil(t, st.firstReleaseVersion, "first_release_version is stamped on every build")
	assert.Equal(t, release, *st.firstReleaseVersion)
}
