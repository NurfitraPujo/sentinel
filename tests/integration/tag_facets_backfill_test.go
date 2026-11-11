package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/tagfacets"
)

// sentinel-issue-tag-facets spec §4 and §10.2 (TASK-01 backfill): the backfill counts every occurrence
// stored before the triggers, exactly once, against concurrent processing and the retention sweep; a
// re-run changes nothing; a cut-short run resumes; it refuses without the triggers; one runner at a time;
// its candidate read is index-bounded.

// tfBackfillSeed seeds n occurrences over the issues BEFORE the tag-facets migration runs (so every one is
// unmarked by construction), the first `aged` of them 40 days old. It returns the seed list in order.
func tfBackfillSeed(t *testing.T, env *tagFacetsEnv, projectID string, issues []string, n, aged int) []tfOcc {
	t.Helper()
	var seed []tfOcc
	for i := 0; i < n; i++ {
		o := tfOcc{IssueID: issues[i%len(issues)], ProjectID: projectID, Release: []string{"1.0", "1.1", "2.0"}[i%3],
			User: tfUser(fmt.Sprintf("u%d", i%37)), Tags: fmt.Sprintf(`{"shard":"s%d"}`, i%11)}
		if i%4 != 0 {
			o.Contexts = fmt.Sprintf(`{"device":{"model":%q}}`, []string{"SM-A115F", "Pixel 7"}[i%2])
		}
		if i < aged {
			o.CreatedAt = time.Now().UTC().Add(-40 * 24 * time.Hour).Add(time.Duration(i) * time.Millisecond)
		}
		seed = append(seed, o.withDefaults())
	}
	for off := 0; off < len(seed); off += 2000 {
		end := off + 2000
		if end > len(seed) {
			end = len(seed)
		}
		tfInsertOccs(t, env.Pool, seed[off:end])
	}
	return seed
}

// tfBackfillEnv is a database migrated up to the tag-facets migration's predecessor, seeded, then
// migrated the rest of the way.
func tfBackfillEnv(t *testing.T, nIssues, n, aged int) (*tagFacetsEnv, string, []string, []tfOcc) {
	t.Helper()
	_, _, prev := tagFacetsMigrationFile(t)
	env := tagFacetsDB(t, tagFacetsOpts{UpTo: prev})
	projectID := tfSeedProject(t, env.Pool)
	var issues []string
	for i := 0; i < nIssues; i++ {
		issues = append(issues, tfSeedIssue(t, env.Pool, projectID, "production"))
	}
	seed := tfBackfillSeed(t, env, projectID, issues, n, aged)
	env.migrateRest()
	if u := tfScalar(t, env.Pool, `SELECT count(*) FROM error_occurrences WHERE tag_rollup_applied IS NULL`); u != int64(n) {
		t.Fatalf("precondition: %d unmarked rows, want %d", u, n)
	}
	return env, projectID, issues, seed
}

func tfState(t *testing.T, env *tagFacetsEnv) string {
	t.Helper()
	var s string
	if err := env.Pool.QueryRow(context.Background(), `SELECT row_to_json(b)::text FROM issue_tag_values_backfill b`).Scan(&s); err != nil {
		t.Fatalf("state: %v", err)
	}
	return s
}

func tfFastOpts(logger *slog.Logger) tagfacets.Options {
	return tagfacets.Options{Batch: 100, Pause: -1, Duty: 1, Logger: logger, RetryDelay: 10 * time.Millisecond}
}

func TestTagFacets_BackfillMatchesGroupBy(t *testing.T) {
	env, projectID, issues, seed := tfBackfillEnv(t, 5, 3000, 500)
	ctx := context.Background()
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	agedSet := map[string]bool{}
	for _, o := range seed[:500] {
		agedSet[o.ID] = true
	}

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []error
		stored []tfEvent
		res    tagfacets.Result
	)
	fail := func(err error) { mu.Lock(); errs = append(errs, err); mu.Unlock() }
	wg.Add(3)
	go func() { // the backfill
		defer wg.Done()
		r, err := tagfacets.Run(ctx, env.Pool, tfFastOpts(nil))
		if err != nil {
			fail(fmt.Errorf("backfill: %w", err))
		}
		res = r
	}()
	go func() { // 400 StoreEvents onto the seeded issues
		defer wg.Done()
		for i := 0; i < 400; i++ {
			ev := tfEvent{Environment: "production", Fingerprint: "fp-tf-" + issues[i%5], Release: []string{"1.0", "3.0"}[i%2],
				EventID: fmt.Sprintf("bf-%d", i), Contexts: `{"device":{"model":"SM-A115F"}}`}
			if _, stored1, err := tfStoreErr(env, projectID, ev); err != nil || !stored1 {
				fail(fmt.Errorf("StoreEvent %d: stored=%v %v", i, stored1, err))
				return
			}
			mu.Lock()
			stored = append(stored, ev)
			mu.Unlock()
		}
	}()
	go func() { // the sweep twin deletes exactly the 500 aged rows
		defer wg.Done()
		c, err := env.Pool.Acquire(ctx)
		if err != nil {
			fail(err)
			return
		}
		defer c.Release()
		var after *tfSweepCursor
		total := 0
		for {
			n, next, err := tfSweepTwinBatch(ctx, c, cutoff, after, 37)
			if err != nil {
				fail(fmt.Errorf("sweep: %w", err))
				return
			}
			if next == nil {
				break
			}
			total += n
			after = next
		}
		if total != 500 {
			fail(fmt.Errorf("sweep deleted %d, want 500", total))
		}
	}()
	wg.Wait()
	for _, err := range errs {
		t.Fatal(err)
	}
	if !res.Done {
		t.Fatalf("the backfill did not finish: %+v", res)
	}
	tfCheck(t, env.Pool, issues...)

	// Hand counts, in Go, from the seed list and the StoreEvents.
	var relA, smB int64
	for _, o := range seed {
		if agedSet[o.ID] {
			continue
		}
		if o.IssueID == issues[0] && o.Release == "1.0" {
			relA++
		}
		if o.IssueID == issues[1] && strings.Contains(o.Contexts, "SM-A115F") {
			smB++
		}
	}
	for _, ev := range stored {
		if ev.Fingerprint == "fp-tf-"+issues[0] && ev.Release == "1.0" {
			relA++
		}
		if ev.Fingerprint == "fp-tf-"+issues[1] {
			smB++
		}
	}
	if got := tfCount(t, env.Pool, issues[0], "release", "1.0"); got != relA {
		t.Fatalf("A release=1.0: %d, want %d (hand count)", got, relA)
	}
	if got := tfCount(t, env.Pool, issues[1], "device.model", "SM-A115F"); got != smB {
		t.Fatalf("B device.model=SM-A115F: %d, want %d (hand count)", got, smB)
	}
	if res.RowsCounted < 2500 || res.RowsCounted > 3000 {
		t.Fatalf("rows_counted %d outside [2500, 3000]", res.RowsCounted)
	}
}

func TestTagFacets_BackfillLocksIssueBeforeCounting(t *testing.T) {
	env, _, issues, seed := tfBackfillEnv(t, 5, 300, 0)
	ctx := context.Background()
	a := issues[0]
	var victim string
	for _, o := range seed {
		if o.IssueID == a {
			victim = o.ID
			break
		}
	}

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	defer htx.Rollback(ctx)
	if _, err := htx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR UPDATE`, a); err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	b := tfAcquire(t, env.Pool)
	type res struct {
		counted int
		err     error
	}
	done := make(chan res, 1)
	go func() {
		var n int
		err := b.QueryRow(ctx, `SELECT counted FROM issue_tag_values_backfill_batch(10000)`).Scan(&n)
		done <- res{n, err}
	}()
	tfWaitBlockedBy(t, env.Pool, tfPID(b), tfPID(holder))
	for k := range tfHeldRelationLocks(t, env.Pool, tfPID(b)) {
		if k == "issue_tag_values:RowExclusiveLock" {
			t.Fatalf("the backfill wrote the rollup before it held every candidate's issue lock")
		}
	}
	// The holder deletes one unmarked, still-uncounted candidate of A, then commits.
	if _, err := htx.Exec(ctx, `DELETE FROM error_occurrences WHERE id = $1`, victim); err != nil {
		t.Fatalf("holder delete: %v", err)
	}
	if err := htx.Commit(ctx); err != nil {
		t.Fatalf("holder commit: %v", err)
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("batch: %v", r.err)
	}
	if r.counted != 299 {
		t.Fatalf("counted %d, want 299 (the candidates minus the one deleted while the batch waited)", r.counted)
	}
	var relA int64
	for _, o := range seed {
		if o.IssueID == a && o.ID != victim && o.Release == "1.0" {
			relA++
		}
	}
	if got := tfCount(t, env.Pool, a, "release", "1.0"); got != relA {
		t.Fatalf("A release=1.0: %d, want %d (its survivors, by hand)", got, relA)
	}
	tfCheck(t, env.Pool, issues...)
}

// TestTagFacets_UpdateBeyondCursorDuringBackfillNotDoubleCounted kills mutants M10/M10b
// (tfr-upd-trigger-counted-filter-unproven): replacing either issue_tag_counted(...) filter in
// error_occurrences_tag_rollup_upd with `true` leaves every other test green. The new-side (ADD) filter's
// mutant is a real double count: an UPDATE of a row beyond the backfill's cursor (unmarked, backfill not
// done, so not yet "counted") would add its current tags to the rollup immediately, and the backfill
// would count that same row again when it later reaches it. Decision 9 sanctions this path — a batched
// future bulk UPDATE landing on an as-yet-unbackfilled row is live, not hypothetical.
func TestTagFacets_UpdateBeyondCursorDuringBackfillNotDoubleCounted(t *testing.T) {
	env, _, issues, seed := tfBackfillEnv(t, 1, 20, 0)
	ctx := context.Background()
	issueID := issues[0]

	ids := make([]string, len(seed))
	for i, o := range seed {
		ids[i] = o.ID
	}
	sort.Strings(ids) // uuid order equals the lowercase hex string order
	victim := ids[len(ids)-1]

	// Run one real batch, one short of the whole set, so the cursor genuinely advanced past every row
	// except the last: that one is legitimately unmarked and beyond the cursor, not synthesised.
	var counted int
	var doneEarly bool
	if err := env.Pool.QueryRow(ctx, `SELECT counted, done FROM issue_tag_values_backfill_batch($1)`, len(ids)-1).
		Scan(&counted, &doneEarly); err != nil {
		t.Fatalf("partial batch: %v", err)
	}
	if counted != len(ids)-1 || doneEarly {
		t.Fatalf("partial batch counted=%d done=%v; want %d, false", counted, doneEarly, len(ids)-1)
	}

	// Change the victim's tags while it is still beyond the cursor. With the filter intact this must
	// change nothing in the rollup yet (it is not "counted"); the backfill will count its CURRENT tags
	// once it reaches it.
	if _, err := env.Pool.Exec(ctx, `UPDATE error_occurrences SET tags = '{"t":"beyond-cursor"}' WHERE id = $1`, victim); err != nil {
		t.Fatalf("update victim beyond the cursor: %v", err)
	}
	if got := tfCount(t, env.Pool, issueID, "t", "beyond-cursor"); got != 0 {
		t.Fatalf("t=beyond-cursor counted %d before the backfill reached it; want 0 (not yet counted, still pending)", got)
	}

	if _, err := tagfacets.Run(ctx, env.Pool, tfFastOpts(nil)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := tfCount(t, env.Pool, issueID, "t", "beyond-cursor"); got != 1 {
		t.Fatalf("t=beyond-cursor counted %d after the backfill; want exactly 1 (counted once, by the backfill — a double count means the UPD trigger already added it while it was still beyond the cursor)", got)
	}
	tfCheck(t, env.Pool, issueID)
}

// TestTagFacets_UpdateBeyondCursorSubtractSideNotDoubleSubtracted kills the other half of
// tfr-upd-trigger-counted-filter-unproven: the OLD-side issue_tag_counted(...) filter in
// error_occurrences_tag_rollup_upd (migration :385, the tr_old/subtract leg), as opposed to the NEW-side
// filter (:393, proven by TestTagFacets_UpdateBeyondCursorDuringBackfillNotDoubleCounted). Replacing the
// old-side filter with `true` leaves every other test green. An UPDATE of a row still beyond the cursor
// (unmarked, never yet counted) must not subtract its OLD tags from the rollup either — those tags were
// never added in the first place, so a spurious subtract would undercount every OTHER already-counted
// occurrence that happens to share that tag value.
func TestTagFacets_UpdateBeyondCursorSubtractSideNotDoubleSubtracted(t *testing.T) {
	env, _, issues, seed := tfBackfillEnv(t, 1, 10, 0)
	ctx := context.Background()
	issueID := issues[0]

	ids := make([]string, len(seed))
	for i, o := range seed {
		ids[i] = o.ID
	}
	sort.Strings(ids) // uuid order equals the lowercase hex string order
	victim := ids[len(ids)-1]

	// Give every row, victim included, the exact same tag, device, release and user, so every one of the
	// victim's derived pairs overlaps a value counted rows already hold. This matters: issue_tag_values_subtract
	// self-heals real drift by rebuilding an issue's rollup from scratch (W11) whenever a subtracted pair has no
	// matching named row (e.g. a per-row-unique user id that was never counted). That rebuild is exact, so it
	// would silently repair the very corruption this test means to catch. Sharing every derived value with an
	// already-counted row keeps every subtracted pair inside the "named" (matched) path, so a spurious subtract
	// under the mutant shows up as a wrong count instead of being masked by a rebuild.
	if _, err := env.Pool.Exec(ctx,
		`UPDATE error_occurrences SET tags = '{"t":"shared"}', event_user = '{"id":"shared-user"}' WHERE issue_id = $1`,
		issueID); err != nil {
		t.Fatalf("homogenize tags: %v", err)
	}

	// One real batch, one short of the whole set: the cursor advances past every row except the victim,
	// counting "t":"shared" exactly len(ids)-1 times.
	var counted int
	var doneEarly bool
	if err := env.Pool.QueryRow(ctx, `SELECT counted, done FROM issue_tag_values_backfill_batch($1)`, len(ids)-1).
		Scan(&counted, &doneEarly); err != nil {
		t.Fatalf("partial batch: %v", err)
	}
	if counted != len(ids)-1 || doneEarly {
		t.Fatalf("partial batch counted=%d done=%v; want %d, false", counted, doneEarly, len(ids)-1)
	}
	want := int64(len(ids) - 1)
	if got := tfCount(t, env.Pool, issueID, "t", "shared"); got != want {
		t.Fatalf("t=shared counted %d after the partial batch; want %d", got, want)
	}

	// Change the still-uncounted victim's tags to a DIFFERENT value while it is beyond the cursor. With
	// both filters intact this must change nothing in the rollup: the victim was never counted, so its old
	// "shared" tag must not be subtracted, and its new "other" tag must not be added (that add-side case
	// is TestTagFacets_UpdateBeyondCursorDuringBackfillNotDoubleCounted).
	if _, err := env.Pool.Exec(ctx, `UPDATE error_occurrences SET tags = '{"t":"other"}' WHERE id = $1`, victim); err != nil {
		t.Fatalf("update victim beyond the cursor: %v", err)
	}
	if got := tfCount(t, env.Pool, issueID, "t", "shared"); got != want {
		t.Fatalf("t=shared counted %d after updating the still-uncounted victim; want unchanged %d (the old-side filter must not subtract an uncounted row's tags)", got, want)
	}
	if got := tfCount(t, env.Pool, issueID, "t", "other"); got != 0 {
		t.Fatalf("t=other counted %d before the backfill reached the victim; want 0 (not yet counted)", got)
	}

	if _, err := tagfacets.Run(ctx, env.Pool, tfFastOpts(nil)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := tfCount(t, env.Pool, issueID, "t", "shared"); got != want {
		t.Fatalf("t=shared counted %d after the backfill finished; want unchanged %d", got, want)
	}
	if got := tfCount(t, env.Pool, issueID, "t", "other"); got != 1 {
		t.Fatalf("t=other counted %d after the backfill finished; want exactly 1 (the victim, counted once with its current tags)", got)
	}
	tfCheck(t, env.Pool, issueID)
}

func TestTagFacets_BackfillRerunChangesNothing(t *testing.T) {
	env, _, issues, _ := tfBackfillEnv(t, 3, 500, 0)
	ctx := context.Background()
	if r, err := tagfacets.Run(ctx, env.Pool, tfFastOpts(nil)); err != nil || !r.Done || r.AlreadyDone {
		t.Fatalf("first run: %+v %v", r, err)
	}
	tfCheck(t, env.Pool, issues...)
	snap, st := tfSnapshot(t, env.Pool), tfState(t, env)
	r, err := tagfacets.Run(ctx, env.Pool, tfFastOpts(nil))
	if err != nil || !r.AlreadyDone || r.Batches != 0 {
		t.Fatalf("second run: %+v %v; want AlreadyDone and no batch", r, err)
	}
	if tfSnapshot(t, env.Pool) != snap || tfState(t, env) != st {
		t.Fatalf("a re-run changed the rollup or the state row")
	}
}

func TestTagFacets_BackfillResumes(t *testing.T) {
	env, _, issues, _ := tfBackfillEnv(t, 4, 3000, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := tfFastOpts(nil)
	sleeps := 0
	opts.Sleep = func(c context.Context, _ time.Duration) error {
		sleeps++
		if sleeps == 7 { // after the 7th committed batch
			cancel()
			return c.Err()
		}
		return nil
	}
	r, err := tagfacets.Run(ctx, env.Pool, opts)
	if !errors.Is(err, context.Canceled) || r.Batches != 7 {
		t.Fatalf("cut-short run: %+v %v; want 7 batches and context.Canceled", r, err)
	}
	var cursor *string
	var done bool
	var rows int64
	if err := env.Pool.QueryRow(context.Background(), `SELECT cursor_id::text, done, rows_counted FROM issue_tag_values_backfill`).Scan(&cursor, &done, &rows); err != nil {
		t.Fatalf("state: %v", err)
	}
	if cursor == nil || done || rows != 700 {
		t.Fatalf("persisted state: cursor %v done %v rows %d; want a cursor, not done, 700", cursor, done, rows)
	}
	r2, err := tagfacets.Run(context.Background(), env.Pool, tfFastOpts(nil))
	if err != nil || !r2.Done || r2.RowsCounted != 3000 {
		t.Fatalf("resumed run: %+v %v; want done with 3000 counted", r2, err)
	}
	tfCheck(t, env.Pool, issues...)
}

func TestTagFacets_BackfillRefusesWithoutTriggers(t *testing.T) {
	env, _, _, _ := tfBackfillEnv(t, 2, 50, 0)
	if _, err := env.Pool.Exec(context.Background(), `ALTER TABLE error_occurrences DISABLE TRIGGER trg_error_occurrences_tag_rollup_upd`); err != nil {
		t.Fatalf("disable: %v", err)
	}
	st := tfState(t, env)
	_, err := tagfacets.Run(context.Background(), env.Pool, tfFastOpts(nil))
	if err == nil || !strings.Contains(err.Error(), "trg_error_occurrences_tag_rollup_upd") {
		t.Fatalf("Run with a disabled trigger: %v; want an error naming it", err)
	}
	if tfState(t, env) != st {
		t.Fatalf("a refused run changed the state row")
	}
}

func TestTagFacets_BackfillSecondRunnerExits(t *testing.T) {
	env, _, issues, _ := tfBackfillEnv(t, 2, 200, 0)
	ctx := context.Background()
	other := tfAcquire(t, env.Pool)
	if _, err := other.Exec(ctx, `SELECT pg_advisory_lock(hashtext('sentinel.tag_facets.backfill'))`); err != nil {
		t.Fatalf("take lock: %v", err)
	}
	st := tfState(t, env)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	_, err := tagfacets.Run(ctx, env.Pool, tfFastOpts(logger))
	if !errors.Is(err, tagfacets.ErrAlreadyRunning) {
		t.Fatalf("second runner: %v; want ErrAlreadyRunning", err)
	}
	var line map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil && m["event"] == "tag_facets.backfill.already_running" {
			line = m
		}
	}
	if line == nil || int64(line["pid"].(float64)) != int64(tfPID(other)) {
		t.Fatalf("already_running line %v; want pid %d", line, tfPID(other))
	}
	if tfState(t, env) != st {
		t.Fatalf("the refused runner changed the state row (a batch ran)")
	}
	if _, err := other.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('sentinel.tag_facets.backfill'))`); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if r, err := tagfacets.Run(ctx, env.Pool, tfFastOpts(nil)); err != nil || !r.Done {
		t.Fatalf("after unlock: %+v %v", r, err)
	}
	tfCheck(t, env.Pool, issues...)
}

func TestTagFacets_BackfillCandidateIsIndexBounded(t *testing.T) {
	env, _, _, seed := tfBackfillEnv(t, 5, 20000, 0)
	ctx := context.Background()
	ids := make([]string, len(seed))
	for i, o := range seed {
		ids[i] = o.ID
	}
	sort.Strings(ids) // uuid order equals the lowercase hex string order
	if _, err := env.Pool.Exec(ctx, `UPDATE issue_tag_values_backfill SET cursor_id = $1`, ids[17999]); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	if _, err := env.Pool.Exec(ctx, `ANALYZE error_occurrences`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	conn, notices := tfPlanNotices(t, env)
	if _, err := conn.Exec(ctx, `SET plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatalf("plan_cache_mode: %v", err)
	}
	const limit = 200
	if _, err := conn.Exec(ctx, `SELECT * FROM issue_tag_values_backfill_batch($1)`, limit); err != nil {
		t.Fatalf("batch: %v", err)
	}
	var plan map[string]any
	for _, n := range *notices {
		if strings.Contains(n, "o.tag_rollup_applied IS NULL") && strings.Contains(n, "LIMIT p_limit") {
			i := strings.Index(n, "{")
			if json.Unmarshal([]byte(n[i:]), &plan) == nil {
				break
			}
		}
	}
	if plan == nil {
		t.Fatalf("no auto_explain plan for the candidate statement among %d notice(s)", len(*notices))
	}
	root, _ := plan["Plan"].(map[string]any)
	indexCond := false
	var maxRemoved float64
	tfWalkPlan(root, func(n map[string]any) {
		if n["Index Name"] == "error_occurrences_pkey" && strings.Contains(fmt.Sprint(n["Index Cond"]), "id >") {
			indexCond = true
		}
		if r, ok := n["Rows Removed by Filter"].(float64); ok && r > maxRemoved {
			maxRemoved = r
		}
	})
	if !indexCond {
		t.Fatalf("the candidate read is not an Index Cond (id > ...) on error_occurrences_pkey under a generic plan: %v", root)
	}
	if maxRemoved > limit {
		t.Fatalf("the candidate read removed %v rows by Filter (> p_limit %d): the cursor test is not sargable", maxRemoved, limit)
	}
}

// TestTagFacets_BackfillBatchLockHoldBounded is the reviewer's probe (tfb-backfill-batch-hold-unbounded):
// a batch that locks several issues in id order, each wait individually under lock_timeout, can still
// hold the FIRST (lowest-id) one for their SUM if nothing bounds the total. Contract §4.2: the backfill
// never holds a long lock on issues. B and C are held long enough, in sequence, that neither single wait
// trips LockWait alone, but their sum comfortably exceeds LockBudget — proving the fix bounds the TOTAL,
// not just each wait. An ingest-style FOR NO KEY UPDATE waiter on A (what StoreEvent's issue upsert takes)
// is the thing contract §4.2 actually protects; its own wait must never exceed the budget either.
func TestTagFacets_BackfillBatchLockHoldBounded(t *testing.T) {
	env, _, issues, _ := tfBackfillEnv(t, 3, 30, 0)
	ctx := context.Background()

	rows, err := env.Pool.Query(ctx, `SELECT id FROM issues WHERE id = ANY($1) ORDER BY id`, issues)
	if err != nil {
		t.Fatalf("order issues: %v", err)
	}
	var ordered []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ordered = append(ordered, id)
	}
	rows.Close()
	if len(ordered) != 3 {
		t.Fatalf("got %d issues, want 3", len(ordered))
	}
	a, b, c := ordered[0], ordered[1], ordered[2]

	lockIssue := func(id string) (*pgxpool.Conn, pgx.Tx) {
		conn := tfAcquire(t, env.Pool)
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("holder begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR UPDATE`, id); err != nil {
			t.Fatalf("holder lock %s: %v", id, err)
		}
		return conn, tx
	}
	bConn, bTx := lockIssue(b)
	cConn, cTx := lockIssue(c)
	defer bConn.Release()
	defer cConn.Release()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		time.Sleep(400 * time.Millisecond)
		_ = bTx.Commit(ctx) // best-effort: the batch's own transaction may already be gone by now
	}()
	go func() {
		defer wg.Done()
		time.Sleep(900 * time.Millisecond) // still held well past when the budget must have fired
		_ = cTx.Commit(ctx)
	}()
	defer wg.Wait()

	opts := tagfacets.Options{
		// MaxRetries: -1, not 0 — Options.withDefaults treats a zero MaxRetries as "use the default"
		// (the same convention Batch/Pause/Duty use), so disabling retries needs a negative sentinel.
		Batch: 30, Pause: -1, Duty: 1, MaxRetries: -1, RetryDelay: 10 * time.Millisecond,
		LockWait: 2 * time.Second, LockBudget: 600 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// The batch runs in its own goroutine so the test can wait, deterministically, until the batch's own
	// backend is actually blocked on B (tfWaitBlockedBy polls pg_stat_activity/pg_blocking_pids) before
	// starting the ingest-style waiter below — not until the waiter's connection merely exists. Without
	// this, the waiter could win the race to lock A itself, wait ~0, and the assertion below would pass
	// vacuously regardless of whether the batch's own lock budget is enforced.
	runDone := make(chan struct {
		res     tagfacets.Result
		err     error
		elapsed time.Duration
	}, 1)
	started := time.Now()
	go func() {
		res, err := tagfacets.Run(ctx, env.Pool, opts)
		runDone <- struct {
			res     tagfacets.Result
			err     error
			elapsed time.Duration
		}{res, err, time.Since(started)}
	}()
	tfWaitAnyBlockedBy(t, env.Pool, tfPID(bConn))

	ingestDone := make(chan time.Duration, 1)
	ingestErr := make(chan error, 1)
	go func() {
		w := tfAcquire(t, env.Pool)
		defer w.Release()
		wtx, err := w.Begin(ctx)
		if err != nil {
			ingestErr <- fmt.Errorf("ingest begin: %w", err)
			return
		}
		defer wtx.Rollback(ctx)
		t0 := time.Now()
		if _, err := wtx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR NO KEY UPDATE`, a); err != nil {
			ingestErr <- fmt.Errorf("ingest FOR NO KEY UPDATE on A: %w", err)
			return
		}
		ingestDone <- time.Since(t0)
	}()

	run := <-runDone
	res, runErr, elapsed := run.res, run.err, run.elapsed

	var pgErr *pgconn.PgError
	if !errors.As(runErr, &pgErr) || pgErr.Code != "57014" {
		t.Fatalf("Run error = %v; want a wrapped 57014 (statement_timeout: the lock budget was exceeded)", runErr)
	}
	if res.Batches != 0 {
		t.Fatalf("Batches = %d; want 0 (the only batch attempted was cancelled by its lock budget)", res.Batches)
	}
	const slack = 2 * time.Second
	if elapsed > opts.LockBudget+slack {
		t.Fatalf("Run took %s; the batch should have been cancelled at its %s lock budget, not held for B (400ms) + C (900ms)", elapsed, opts.LockBudget)
	}

	select {
	case err := <-ingestErr:
		t.Fatalf("ingest-style waiter: %v", err)
	case waited := <-ingestDone:
		if waited > opts.LockBudget+slack {
			t.Fatalf("ingest-style waiter on A waited %s; contract §4.2 bounds it to the batch's %s lock budget", waited, opts.LockBudget)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("ingest-style waiter on A never completed within 10s — still blocked behind the batch's transaction")
	}
}

// TestTagFacets_BackfillRetriesLockTimeoutThenSucceeds kills two tfb-backfill-safety-controls-unproven
// mutations at once: dropping SET LOCAL lock_timeout (without it, a held issue would hang the batch
// indefinitely instead of failing 55P03 within LockWait), and disabling the 40001/40P01/55P03 retry
// (without it, that 55P03 would end the run instead of being retried once the holder releases).
func TestTagFacets_BackfillRetriesLockTimeoutThenSucceeds(t *testing.T) {
	env, _, issues, _ := tfBackfillEnv(t, 1, 20, 0)
	ctx := context.Background()
	issueID := issues[0]

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := htx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR UPDATE`, issueID); err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = htx.Commit(ctx) // released well after the first attempt's lock_timeout, well before the test deadline
	}()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	opts := tagfacets.Options{
		Batch: 20, Pause: -1, Duty: 1, MaxRetries: 5, RetryDelay: 50 * time.Millisecond,
		LockWait: 100 * time.Millisecond, LockBudget: 2 * time.Second, Logger: logger,
	}

	started := time.Now()
	res, err := tagfacets.Run(ctx, env.Pool, opts)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Run: %v (want it to retry past the holder's 55P03 and succeed)", err)
	}
	if !res.Done {
		t.Fatalf("Run: Done = false, want true")
	}
	// Bounded above: without lock_timeout the first attempt would have hung until the holder released at
	// 300ms and then some — this asserts the retry path (fail fast at ~100ms, wait 50ms, retry, succeed
	// once the holder is gone), not a lucky single long wait.
	if elapsed > 2*time.Second {
		t.Fatalf("Run took %s; want well under 2s (lock_timeout + retry, not one long uninterrupted wait)", elapsed)
	}

	var sawRetry bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["event"] == "tag_facets.backfill.retry" && m["code"] == "55P03" {
			sawRetry = true
		}
	}
	if !sawRetry {
		t.Fatalf("no tag_facets.backfill.retry (code=55P03) log line; the retry path was never exercised\nlog:\n%s", buf.String())
	}
	tfCheck(t, env.Pool, issueID)
}

// TestTagFacets_BackfillProgressAndDoneLogLines kills the tfb-backfill-safety-controls-unproven gap "no
// test reads the progress/done log lines": both must actually be emitted, with the fields the runbook's
// operator reads.
func TestTagFacets_BackfillProgressAndDoneLogLines(t *testing.T) {
	env, _, issues, _ := tfBackfillEnv(t, 1, 40, 0)
	ctx := context.Background()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	opts := tagfacets.Options{
		Batch: 10, Pause: -1, Duty: 1, ProgressEvery: time.Nanosecond, // every batch logs progress
		Logger: logger,
	}
	res, err := tagfacets.Run(ctx, env.Pool, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Done || res.Batches < 2 {
		t.Fatalf("Run: Done=%v Batches=%d; want Done and >= 2 batches (40 rows / batch 10)", res.Done, res.Batches)
	}

	var progressLines, doneLines int
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch m["event"] {
		case "tag_facets.backfill.progress":
			progressLines++
			for _, field := range []string{"batches", "rows_counted", "cursor_id", "progress_pct", "rows_per_sec", "batch_ms_p50", "batch_ms_max"} {
				if _, ok := m[field]; !ok {
					t.Fatalf("progress line missing %q: %v", field, m)
				}
			}
		case "tag_facets.backfill.done":
			doneLines++
			for _, field := range []string{"rows_counted", "batches", "elapsed"} {
				if _, ok := m[field]; !ok {
					t.Fatalf("done line missing %q: %v", field, m)
				}
			}
			if int64(m["rows_counted"].(float64)) != 40 {
				t.Fatalf("done line rows_counted = %v, want 40", m["rows_counted"])
			}
		}
	}
	if progressLines == 0 {
		t.Fatalf("no tag_facets.backfill.progress log line emitted\nlog:\n%s", buf.String())
	}
	if doneLines != 1 {
		t.Fatalf("tag_facets.backfill.done log lines = %d, want exactly 1\nlog:\n%s", doneLines, buf.String())
	}
	tfCheck(t, env.Pool, issues...)
}

// TestTagFacets_BackfillLimitRecoversAfterTransientTimeout proves tfr2-backfill-halved-limit-never-recovers
// fixed: a 57014 must not permanently shrink the rest of the Job. This is the reviewer's scratch
// reproduction — 2000 rows, Batch 200, one issue held 700ms at the start of the run with LockBudget 300ms
// — which, without the fix, halves the limit twice (200 -> 100 -> 50) and never restores it, so nearly
// all 2000 rows run at limit 50 (~40 batches, the reviewer's own measurement). With the fix, each
// successful batch after the hold releases doubles the limit back toward opts.Batch, so the run finishes
// in far fewer batches.
func TestTagFacets_BackfillLimitRecoversAfterTransientTimeout(t *testing.T) {
	env, _, issues, _ := tfBackfillEnv(t, 5, 2000, 0)
	ctx := context.Background()

	rows, err := env.Pool.Query(ctx, `SELECT id FROM issues WHERE id = ANY($1) ORDER BY id`, issues)
	if err != nil {
		t.Fatalf("order issues: %v", err)
	}
	var ordered []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ordered = append(ordered, id)
	}
	rows.Close()
	if len(ordered) != 5 {
		t.Fatalf("got %d issues, want 5", len(ordered))
	}
	// The batch locks candidate issues in id order, so holding the lowest-id issue guarantees the very
	// first batch attempt blocks on it.
	victim := ordered[0]

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := htx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR UPDATE`, victim); err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = htx.Commit(ctx)
	}()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	opts := tagfacets.Options{
		Batch: 200, Pause: -1, Duty: 1, MaxRetries: 10, RetryDelay: 20 * time.Millisecond,
		LockWait: 2 * time.Second, LockBudget: 300 * time.Millisecond,
		Logger: logger,
	}

	res, err := tagfacets.Run(ctx, env.Pool, opts)
	if err != nil {
		t.Fatalf("Run: %v (want it to survive the transient hold and finish)", err)
	}
	if !res.Done || res.RowsCounted != 2000 {
		t.Fatalf("Run: Done=%v RowsCounted=%d; want Done and 2000", res.Done, res.RowsCounted)
	}

	var timeouts int
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["event"] == "tag_facets.backfill.lock_budget_exceeded" {
			timeouts++
		}
	}
	if timeouts == 0 {
		t.Fatalf("no tag_facets.backfill.lock_budget_exceeded log line; the hold never produced a 57014\nlog:\n%s", buf.String())
	}
	// Without the fix, the limit halves at each timeout and never recovers: at limit 50 the remaining
	// ~1,900 rows alone take ~38 batches, for ~40 total (the reviewer's measurement). With the fix, the
	// limit doubles back toward 200 after each successful batch, so the whole run finishes in well under
	// half that.
	if res.Batches >= 20 {
		t.Fatalf("Run took %d batches; want well under 20 (the limit must recover toward Batch=200 after the transient hold releases, not stay halved for the rest of the run)", res.Batches)
	}
	tfCheck(t, env.Pool, issues...)
}

// TestTagFacets_Backfill57014RetryHalvesThenSucceeds proves tfr2-backfill-57014-retry-unproven: a transient
// hold that exceeds LockBudget must be retried at HALF the limit (not the same limit, and not abandoned),
// and the run must still finish exactly. Deleting the whole 57014 retry branch, or removing the halving
// (limit = limit instead of limit / 2), both leave TestTagFacets_BackfillBatchLockHoldBounded green because
// that test runs with MaxRetries -1 (no retry at all); this test is the one that needs a positive
// MaxRetries and inspects the retry's own log line to tell the difference.
func TestTagFacets_Backfill57014RetryHalvesThenSucceeds(t *testing.T) {
	env, _, issues, _ := tfBackfillEnv(t, 2, 50, 0)
	ctx := context.Background()

	rows, err := env.Pool.Query(ctx, `SELECT id FROM issues WHERE id = ANY($1) ORDER BY id`, issues)
	if err != nil {
		t.Fatalf("order issues: %v", err)
	}
	var ordered []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ordered = append(ordered, id)
	}
	rows.Close()
	victim := ordered[0]

	holder := tfAcquire(t, env.Pool)
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := htx.Exec(ctx, `SELECT id FROM issues WHERE id = $1 FOR UPDATE`, victim); err != nil {
		t.Fatalf("holder lock: %v", err)
	}
	// Held for longer than LockBudget (so the first attempt's statement_timeout fires) but short enough
	// that the retry, at half the limit, comfortably finishes once the hold releases.
	go func() {
		time.Sleep(350 * time.Millisecond)
		_ = htx.Commit(ctx)
	}()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	opts := tagfacets.Options{
		Batch: 50, Pause: -1, Duty: 1, MaxRetries: 5, RetryDelay: 100 * time.Millisecond,
		LockWait: 2 * time.Second, LockBudget: 200 * time.Millisecond,
		Logger: logger,
	}

	res, err := tagfacets.Run(ctx, env.Pool, opts)
	if err != nil {
		t.Fatalf("Run: %v (want it to retry past the transient hold and finish)", err)
	}
	if !res.Done || res.RowsCounted != 50 {
		t.Fatalf("Run: Done=%v RowsCounted=%d; want Done and 50", res.Done, res.RowsCounted)
	}

	var sawHalvedRetry bool
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil || m["event"] != "tag_facets.backfill.lock_budget_exceeded" {
			continue
		}
		limit, ok := m["limit"].(float64)
		if !ok {
			t.Fatalf("lock_budget_exceeded line missing numeric limit: %v", m)
		}
		if int(limit) != opts.Batch/2 {
			t.Fatalf("lock_budget_exceeded line logged limit=%v, want %d (the batch, halved) — the retry did not shrink, or shrank to the wrong size", limit, opts.Batch/2)
		}
		sawHalvedRetry = true
	}
	if !sawHalvedRetry {
		t.Fatalf("no tag_facets.backfill.lock_budget_exceeded log line; the 57014 retry path was never exercised\nlog:\n%s", buf.String())
	}
	tfCheck(t, env.Pool, issues...)
}

// tfAfterBackfillStillOnce runs the backfill over a database whose rows were all inserted after the
// triggers: nothing is unmarked, so nothing may be counted twice (W6's mutation: without the BEFORE mark
// trigger the row stays NULL and the backfill counts it a second time).
func tfAfterBackfillStillOnce(t *testing.T, env *tagFacetsEnv, issueIDs ...string) {
	t.Helper()
	before := tfSnapshot(t, env.Pool)
	if _, err := tagfacets.Run(context.Background(), env.Pool, tfFastOpts(nil)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if got := tfSnapshot(t, env.Pool); got != before {
		t.Fatalf("the backfill counted an already-counted row again:\n%s\nvs\n%s", got, before)
	}
	tfCheck(t, env.Pool, issueIDs...)
}
