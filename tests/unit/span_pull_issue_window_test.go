package unit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NurfitraPujo/sentinel/apps/processor-go/spanpull"
)

// sentinel-aws-error-reporting TASK-02: at most one span pull per issue per SPAN_PULL_ISSUE_WINDOW. These cover the
// Go half (the order of the checks, the window arithmetic handed to the statement, the outcomes, window 0, errors);
// the statement's own atomicity across replicas is asserted against a real Postgres in
// tests/integration/span_ingest_test.go (TestSpanPull_IssueWindow*).

// windowDB fakes the three statements MaybeEnqueue issues. The span_pull_issue_window upsert is answered the way its
// WHERE reads: the row comes back (allowed) when the issue has no row or its last_enqueued_at <= the cutoff ($3), and
// then last_enqueued_at becomes $2.
type windowDB struct {
	org       string               // every project's organization
	last      map[string]time.Time // span_pull_issue_window
	queued    map[string]bool      // span_pull_queue, by trace id
	windowErr error
	calls     []string // "org" | "window" | "queue", in order
}

func newWindowDB(org string) *windowDB {
	return &windowDB{org: org, last: map[string]time.Time{}, queued: map[string]bool{}}
}

type idRow struct {
	id  string
	err error
}

func (r idRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*string)) = r.id
	return nil
}

func (f *windowDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "FROM projects"):
		f.calls = append(f.calls, "org")
		return fakeRow{org: f.org}
	case strings.Contains(sql, "span_pull_issue_window"):
		f.calls = append(f.calls, "window")
		if f.windowErr != nil {
			return idRow{err: f.windowErr}
		}
		id, now, cutoff := args[0].(string), args[1].(time.Time), args[2].(time.Time)
		if prev, ok := f.last[id]; ok && prev.After(cutoff) {
			return idRow{err: pgx.ErrNoRows}
		}
		f.last[id] = now
		return idRow{id: id}
	}
	return idRow{err: errors.New("unexpected query: " + sql)}
}

func (f *windowDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !strings.Contains(sql, "INSERT INTO span_pull_queue") {
		return pgconn.CommandTag{}, errors.New("unexpected exec: " + sql)
	}
	f.calls = append(f.calls, "queue")
	trace := args[1].(string)
	if f.queued[trace] {
		return pgconn.NewCommandTag("INSERT 0 0"), nil
	}
	f.queued[trace] = true
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (f *windowDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("Query is not used by MaybeEnqueue")
}

const (
	winOrg     = "11111111-1111-4111-8111-111111111111"
	winCore    = "33333333-3333-4333-8333-333333333333"
	winIssueA  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	winIssueB  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	winIssueC  = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	winProject = winCore
)

func winTrace(n int) string { return strings.Repeat("0", 31) + string(rune('0'+n)) }

func windowQueue(db *windowDB, window time.Duration, clock *time.Time) *spanpull.Queue {
	cfg := spanpull.Config{Scopes: map[string]string{winOrg: winCore}, Kind: spanpull.KindTempo, URL: "http://t", IssueWindow: window}
	return spanpull.NewQueue(db, cfg).WithClock(func() time.Time { return *clock })
}

func TestSpanPullIssueWindow_OnePullPerIssuePerWindow(t *testing.T) {
	ctx := context.Background()
	db := newWindowDB(winOrg)
	start := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	clock := start
	q := windowQueue(db, 10*time.Minute, &clock)

	assert.Equal(t, "queued", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(1)), "the issue's first occurrence queues its trace")
	assert.Equal(t, []string{"org", "window", "queue"}, db.calls, "allow-list first, then the throttle, then the queue insert")

	clock = start.Add(time.Minute)
	assert.Equal(t, "throttled", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(2)), "a second occurrence of the same issue inside the window, with a different trace")
	assert.False(t, db.queued[winTrace(2)], "a throttled trace is not queued")

	assert.Equal(t, "queued", q.MaybeEnqueue(ctx, winProject, winIssueB, winTrace(3)), "another issue has a window of its own")

	clock = start.Add(10*time.Minute - time.Second)
	assert.Equal(t, "throttled", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(4)), "one second before the window ends")
	assert.Equal(t, start, db.last[winIssueA], "a throttled occurrence does not extend the window")

	clock = start.Add(10 * time.Minute)
	assert.Equal(t, "queued", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(5)), "once the window has elapsed the issue queues again")
	assert.Equal(t, clock, db.last[winIssueA], "and starts a new window")

	clock = start.Add(11 * time.Minute)
	assert.Equal(t, "throttled", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(6)))
}

func TestSpanPullIssueWindow_TraceAlreadyQueuedSpendsTheWindow(t *testing.T) {
	ctx := context.Background()
	db := newWindowDB(winOrg)
	clock := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	q := windowQueue(db, 10*time.Minute, &clock)

	require.Equal(t, "queued", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(1)))
	// Issue C's first occurrence carries a trace issue A already queued: the window row is recorded and the queue
	// insert is a no-op. That trace is being pulled, so nothing is lost.
	assert.Equal(t, "duplicate", q.MaybeEnqueue(ctx, winProject, winIssueC, winTrace(1)))
	assert.Contains(t, db.last, winIssueC)
	assert.Equal(t, "throttled", q.MaybeEnqueue(ctx, winProject, winIssueC, winTrace(2)))
}

func TestSpanPullIssueWindow_ZeroDisablesTheThrottle(t *testing.T) {
	ctx := context.Background()
	db := newWindowDB(winOrg)
	clock := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	q := windowQueue(db, 0, &clock)

	for i := 1; i <= 3; i++ {
		assert.Equal(t, "queued", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(i)), "window 0 queues every trace, as before the throttle")
	}
	assert.NotContains(t, db.calls, "window", "window 0 never touches span_pull_issue_window")
}

func TestSpanPullIssueWindow_FailuresAndRefusalsQueueNothing(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)

	t.Run("a failing throttle statement is counted as error and enqueues nothing", func(t *testing.T) {
		db := newWindowDB(winOrg)
		db.windowErr = errors.New("connection refused")
		q := windowQueue(db, 10*time.Minute, &clock)
		assert.Equal(t, "error", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(1)))
		assert.Empty(t, db.queued)
	})
	t.Run("an organization that is not allow-listed never writes a window row", func(t *testing.T) {
		db := newWindowDB("22222222-2222-4222-8222-222222222222")
		q := windowQueue(db, 10*time.Minute, &clock)
		assert.Equal(t, "not_allowed", q.MaybeEnqueue(ctx, winProject, winIssueA, winTrace(1)))
		assert.Equal(t, []string{"org"}, db.calls)
	})
	t.Run("a non-canonical trace id is not a candidate", func(t *testing.T) {
		db := newWindowDB(winOrg)
		q := windowQueue(db, 10*time.Minute, &clock)
		assert.Empty(t, q.MaybeEnqueue(ctx, winProject, winIssueA, "not-a-trace"))
		assert.Empty(t, db.calls)
	})
}

func TestSpanPullConfig_IssueWindow(t *testing.T) {
	base := map[string]string{"SPAN_PULL_SCOPES": winOrg + ":" + winCore, "TRACE_STORE_KIND": "tempo", "TRACE_STORE_URL": "http://t"}
	parse := func(window *string) (spanpull.Config, error) {
		env := map[string]string{}
		for k, v := range base {
			env[k] = v
		}
		if window != nil {
			env["SPAN_PULL_ISSUE_WINDOW"] = *window
		}
		return spanpull.ParseConfig(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	}
	str := func(s string) *string { return &s }

	cfg, err := parse(nil)
	require.NoError(t, err)
	assert.Equal(t, 10*time.Minute, cfg.IssueWindow, "unset = the 10m default")
	assert.Equal(t, spanpull.DefaultIssueWindow, cfg.IssueWindow)

	cfg, err = parse(str("  "))
	require.NoError(t, err)
	assert.Equal(t, spanpull.DefaultIssueWindow, cfg.IssueWindow, "empty = the default")

	cfg, err = parse(str("0"))
	require.NoError(t, err)
	assert.Zero(t, cfg.IssueWindow, "0 turns the throttle off")

	cfg, err = parse(str("90s"))
	require.NoError(t, err)
	assert.Equal(t, 90*time.Second, cfg.IssueWindow)

	for _, bad := range []string{"-1m", "10", "ten minutes", "1h-"} {
		_, err = parse(str(bad))
		assert.Error(t, err, "%q: a bad window stops the process", bad)
	}
}
