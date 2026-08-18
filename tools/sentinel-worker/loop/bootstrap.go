package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
)

// BootstrapSeq is the synthetic TriggerSeq used for jobIds minted by Bootstrap (plan §2.1's
// "stable jobId = hash(\"triage\"+issueId+\"bootstrap\")"). Real events-feed seqs are positive
// (server-assigned, monotonic from 1), so a negative sentinel can never collide with one, and
// re-running Bootstrap against the same issue after a crash reproduces the identical jobId --
// the journal's normal dedupe (state/journal.go) absorbs the replay exactly like any other job.
const BootstrapSeq int64 = -1

// bootstrapIssue is the subset of one GET /api/agent/issues row Bootstrap needs. FirstSeen is
// needed (validator major finding: "sweep-window delta re-list double-enqueues TRIAGE") to bound
// the sweep-window delta pass by when the issue actually first appeared, not merely by whether
// step 1 already saw it.
type bootstrapIssue struct {
	ID           string  `json:"id"`
	Status       string  `json:"status"`
	AssigneeType *string `json:"assigneeType"`
	FirstSeen    string  `json:"firstSeen"`
}

// IssueRef is one unresolved/unclaimed issue id plus its firstSeen timestamp, as returned by
// IssuesLister.ListUnresolvedUnclaimed. FirstSeen is zero-valued if the row's firstSeen field was
// absent or unparsable -- callers must treat a zero FirstSeen as "unknown, treat as new" (i.e. NOT
// safely older than any cutoff), never as "very old".
type IssueRef struct {
	ID        string
	FirstSeen time.Time
}

type issuesPage struct {
	Issues     []bootstrapIssue `json:"issues"`
	NextCursor *string          `json:"nextCursor"`
}

// IssuesLister is the seam Bootstrap uses to backfill unresolved, unclaimed issues (plan §2.1
// step 1: "GET /api/agent/issues?since=<now-WORKER_BACKFILL_HOURS>&sort=firstSeen&limit=200,
// keyset-paged"). Kept as an interface so unit tests can drive it without HTTP.
type IssuesLister interface {
	ListUnresolvedUnclaimed(ctx context.Context, since time.Time) ([]IssueRef, error)
	// ListClaimedByMe implements plan §2.1 step 2: one keyset-paged
	// GET /api/agent/issues?claimed=me pass, seeding the sweep's view of claims already held from
	// a previous life (before jobs/sweep.go exists to consume it -- see loop.Bootstrap's doc
	// comment). Returns the claimed issue ids.
	ListClaimedByMe(ctx context.Context) ([]string, error)
}

// httpIssuesLister adapts a *sentinel.Client to IssuesLister via GET /api/agent/issues,
// claimed=false (server-side unclaimed filter) + sort=firstSeen, keyset-paged via nextCursor.
type httpIssuesLister struct {
	c *sentinel.Client
}

// NewIssuesLister wraps a sentinel.Client for use by Bootstrap.
func NewIssuesLister(c *sentinel.Client) IssuesLister { return httpIssuesLister{c: c} }

func (h httpIssuesLister) ListUnresolvedUnclaimed(ctx context.Context, since time.Time) ([]IssueRef, error) {
	var ids []IssueRef
	cursor := ""
	for {
		// Routed through sentinel.Client.ListIssues (not a second hand-rolled h.c.Do call) so the
		// wire shape this package actually sends is covered by client_test.go's goldens.
		res, err := h.c.ListIssues(ctx, sentinel.IssuesListOptions{
			Since:   since.UTC().Format(time.RFC3339),
			Sort:    "firstSeen",
			Limit:   200,
			Cursor:  cursor,
			Claimed: "false",
		})
		if err != nil {
			return ids, err
		}
		if res.Status < 200 || res.Status >= 300 {
			return ids, fmt.Errorf("GET /api/agent/issues: %d %s", res.Status, sentinel.ErrorMessage(res.Body))
		}
		var page issuesPage
		if err := json.Unmarshal(res.Body, &page); err != nil {
			return ids, fmt.Errorf("parsing issues page: %w", err)
		}
		for _, iss := range page.Issues {
			if iss.Status != "unresolved" {
				continue
			}
			// A missing/unparsable firstSeen decodes to the zero time.Time, which
			// Bootstrap's sweep-window-delta cutoff comparison treats as "not safely older
			// than the cutoff" (see IssueRef's doc comment) -- fail toward leaving it to the
			// feed rather than risking a double-enqueue.
			var fs time.Time
			if iss.FirstSeen != "" {
				if parsed, err := time.Parse(time.RFC3339, iss.FirstSeen); err == nil {
					fs = parsed
				}
			}
			ids = append(ids, IssueRef{ID: iss.ID, FirstSeen: fs})
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return ids, nil
		}
		cursor = *page.NextCursor
	}
}

// ListClaimedByMe implements IssuesLister.ListClaimedByMe: GET /api/agent/issues?claimed=me,
// keyset-paged, no status filter (a held claim on a resolved issue is still a claim we hold).
func (h httpIssuesLister) ListClaimedByMe(ctx context.Context) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		res, err := h.c.ListIssues(ctx, sentinel.IssuesListOptions{
			Claimed: "me",
			Limit:   200,
			Cursor:  cursor,
		})
		if err != nil {
			return ids, err
		}
		if res.Status < 200 || res.Status >= 300 {
			return ids, fmt.Errorf("GET /api/agent/issues?claimed=me: %d %s", res.Status, sentinel.ErrorMessage(res.Body))
		}
		var page issuesPage
		if err := json.Unmarshal(res.Body, &page); err != nil {
			return ids, fmt.Errorf("parsing claimed-by-me issues page: %w", err)
		}
		for _, iss := range page.Issues {
			ids = append(ids, iss.ID)
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			return ids, nil
		}
		cursor = *page.NextCursor
	}
}

// Result is Bootstrap's outcome: the feed head seq to seed PollLoop with, how many synthetic
// TRIAGE jobs were backfilled (plan §7's "/metrics counts bootstrap-skipped issues" -- these are
// exactly the events that will NOT be replayed off the feed because they were already covered by
// this backfill), and the issue ids we already hold a claim on from a previous life (step 2, for
// jobs/sweep.go to consume once it exists).
type Result struct {
	HeadSeq           int64
	BootstrapJobCount int // synthetic TRIAGE jobs actually enqueued (plan §7 "bootstrap-enqueued")
	// SkippedCount is issues Bootstrap deliberately did NOT enqueue a synthetic job for -- step 4's
	// sweep-window-delta issues left to the feed because their firstSeen is not safely older than
	// the head-capture cutoff (plan §2.1's "bootstrap-SKIPPED", validator finding: this used to be
	// conflated with BootstrapJobCount under one metric).
	SkippedCount      int
	HeldClaimIssueIDs []string
}

// Bootstrap implements plan §2.1's bootstrap sweep, run exactly once when no cursor.json exists
// (fresh install) or it failed to parse (lost/corrupt state volume). Bootstrap never REPLAYS
// history AS JOBS -- that is explicitly forbidden by the plan ("'page from seq 0' is wrong" --
// the feed has no pre-N7a history and no time filter, C9) because it would turn the entire org
// activity history into a re-triage storm. That does not mean Bootstrap never pages the feed: step
// 3 below pages GET /api/agent/events from after=0 to its head, purely to discover the current head
// seq, because the API exposes no cheaper way to ask "what is the current head?" -- every page it
// reads is discarded, never enqueued. On a busy org with a deep feed this is O(history) requests
// before the worker can start polling forward; see the TODO on step 3 for a cheaper alternative.
// Bootstrap instead:
//
//  1. Backfills a synthetic TRIAGE job for every unresolved, unclaimed issue first-seen within
//     the last backfillHours, with a stable jobId (kind+issueID+BootstrapSeq) so re-running
//     Bootstrap after a second lost state volume is a dedupe no-op, not a re-triage storm.
//  2. Pages GET /api/agent/issues?claimed=me once (keyset-paged) to seed the held-claims view --
//     issues we already hold a claim on from a previous life, so a lost state volume doesn't
//     forget them even though nothing enqueues jobs for them here.
//  3. Pages the events feed from after=0 to its head WITHOUT enqueuing any of it as jobs, purely
//     to discover the feed's current head seq -- the events themselves are already covered (or
//     superseded) by step 1's issues-list backfill, so replaying them as jobs would be redundant
//     work, exactly what the plan forbids. The paging itself (not the replay-as-jobs) is the O(history)
//     cost noted above.
//
// It returns the head seq the caller should seed PollLoop.SetCursor with, so the very first real
// PollOnce call starts polling forward from "now", never from history.
//
// sleep, when provided (variadic so existing call sites compile unchanged), overrides the
// sentinel.CtxSleepFunc step 3's head-seek uses for rate-limit/backoff waits (plan §2.4) -- tests
// inject a non-blocking fake; production leaves it unset and gets sentinel.SleepCtx, which returns
// early on ctx cancellation instead of blocking for the full wait (validator finding: Bootstrap's
// head-seek loop used to walk the ENTIRE backoff ladder -- ~7m36s -- uncancellably on a cancelled
// ctx, because it never checked ctx.Err() at all).
func Bootstrap(ctx context.Context, lister IssuesLister, events EventsClient, enqueue Enqueuer, backfillHours int, log *slog.Logger, sleep ...sentinel.CtxSleepFunc) (Result, error) {
	sl := sentinel.CtxSleepFunc(sentinel.SleepCtx)
	if len(sleep) > 0 && sleep[0] != nil {
		sl = sleep[0]
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	since := time.Now().Add(-time.Duration(backfillHours) * time.Hour)

	ids, err := lister.ListUnresolvedUnclaimed(ctx, since)
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap: listing unresolved/unclaimed issues: %w", err)
	}
	seen := make(map[string]bool, len(ids))
	jobCount := 0
	for _, ref := range ids {
		seen[ref.ID] = true
		if err := enqueueBootstrapTriage(enqueue, ref.ID); err != nil {
			return Result{}, fmt.Errorf("bootstrap: enqueuing triage for issue %s: %w", ref.ID, err)
		}
		jobCount++
	}
	if log != nil {
		log.Info("bootstrap: backfilled unresolved/unclaimed issues", "count", len(ids), "sinceHours", backfillHours)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	// Step 2: seed the held-claims view (plan §2.1 step 2).
	held, err := lister.ListClaimedByMe(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap: listing claimed-by-me issues: %w", err)
	}
	if log != nil {
		log.Info("bootstrap: seeded held-claims view", "count", len(held))
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	// Step 3: discover the feed's current head WITHOUT dispatching any of its events as jobs --
	// this is a head-seek, not a replay (plan §2.1: "read the feed once and set the cursor to its
	// current head... Never replay feed history"). It still pages the whole feed from after=0
	// because GET /api/agent/events exposes no direct "give me the head" query.
	// TODO(N8x): if/when the events feed API grows a head-only affordance (e.g.
	// order=desc&limit=1), use it here instead of paging from after=0 -- this loop is O(history)
	// requests on a fresh start in a busy org.
	var head int64
	after := int64(0)
	attempt := 0
	for {
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		page, err := events.GetEvents(ctx, after)
		if err != nil {
			// A page fetch failing mid-seek must NOT abort the whole sweep on the first hiccup
			// (plan §2.4's classify-and-back-off table applies here exactly like the poll loop's
			// own Run does): a 429 sleeps Retry-After and never counts as a failure; a transient
			// (5xx/network) error backs off on the same ladder, bounded, before giving up for real.
			// `after`/`head` are untouched by a failed attempt, so the retry resumes from exactly
			// where it left off -- forward progress already made in this seek is never re-walked.
			var statusErr *sentinel.StatusError
			if errors.As(err, &statusErr) {
				class := sentinel.ClassifyEnvelope(statusErr.Status, false, false)
				if class == sentinel.ClassRateLimited {
					if log != nil {
						log.Warn("bootstrap: seeking feed head, rate limited, honoring Retry-After", "error", err)
					}
					sentinel.WaitRateLimitCtx(ctx, statusErr.Header, sl)
					if ctx.Err() != nil {
						return Result{}, ctx.Err()
					}
					continue // never counts as a failure/attempt, per plan §2.4
				}
			}
			attempt++
			if attempt > len(sentinel.BackoffSchedule) {
				// Retries exhausted -- give up for real rather than looping forever, but the
				// TODO below (and the caller's own retry-with-full-relist, main.go) still means a
				// persistently broken feed eventually gets retried from `after=0` again. That
				// remaining gap (full crash-resumability of the seek across process restarts) is
				// out of N8a's scope; see the TODO.
				return Result{}, fmt.Errorf("bootstrap: seeking feed head: %w (giving up after %d attempts)", err, attempt)
			}
			if log != nil {
				log.Error("bootstrap: seeking feed head, retrying after backoff", "error", err, "attempt", attempt)
			}
			sl(ctx, sentinel.BackoffForAttempt(attempt))
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			continue
		}
		attempt = 0
		for _, e := range page.Events {
			if e.Seq > head {
				head = e.Seq
			}
		}
		if page.Cursor > head {
			head = page.Cursor
		}
		if !page.HasMore {
			break
		}
		after = head
	}
	headCapturedAt := time.Now()
	if log != nil {
		log.Info("bootstrap: seeked events feed to head", "head", head)
	}
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	// eventsLagGuard mirrors the server's own guard against reordered/late-committed rows landing
	// behind a cursor a client already consumed (apps/dashboard-web/src/lib/server/agent-events.ts:
	// EVENTS_LAG_GUARD_INTERVAL = '2 seconds'). An issue whose firstSeen is within this window of
	// headCapturedAt is NOT provably covered by `head` yet -- its `created` event may still land
	// with seq > head once the guard clears, so PollLoop is guaranteed to deliver it.
	const eventsLagGuard = 2 * time.Second
	cutoff := headCapturedAt.Add(-eventsLagGuard)

	// Step 4 (validator major finding: "sweep-window delta re-list double-enqueues TRIAGE for any
	// issue whose `created` feed event lands after the head was captured"). The head-seek in step 3
	// is an O(history/limit) walk, not the single-request head probe plan §2.1 originally assumed --
	// on a deep feed it can take minutes. An issue first-seen AFTER step 1's list request but BEFORE
	// the head was captured is (a) absent from `ids` above and (b) MAY have a `created` event whose
	// seq is <= head, in which case PollLoop would never see it. Re-run the SAME unresolved/unclaimed
	// listing (same `since`, so the query window only grows) now that the sweep is otherwise done,
	// but enqueue ONLY ids whose firstSeen is strictly older than `cutoff`: those are guaranteed to
	// already be behind `head` (the lag guard has cleared), so PollLoop can never redeliver them and
	// this pass is their only path to a job. Anything newer than `cutoff` is deliberately LEFT to
	// PollLoop -- enqueuing it here too would mint a second, differently-seq'd jobId
	// (kind+issueId+<realSeq> vs. kind+issueId+BootstrapSeq) that the journal's jobId dedupe cannot
	// coalesce, producing a genuine double-triage (double Advisor invocation, double public act())
	// once N8d wires in the real Advisor -- not the false "dedupe no-op" this comment used to claim.
	delta, err := lister.ListUnresolvedUnclaimed(ctx, since)
	if err != nil {
		return Result{}, fmt.Errorf("bootstrap: re-listing unresolved/unclaimed issues to cover the sweep window: %w", err)
	}
	newCount := 0
	skippedToFeed := 0
	for _, ref := range delta {
		if seen[ref.ID] {
			continue
		}
		// A zero/unparsable FirstSeen (IssueRef's doc comment) is never treated as "safely older
		// than cutoff" -- fail toward leaving it to the feed, not toward a double-enqueue.
		if ref.FirstSeen.IsZero() || !ref.FirstSeen.Before(cutoff) {
			skippedToFeed++
			continue
		}
		id := ref.ID
		seen[id] = true
		if err := enqueueBootstrapTriage(enqueue, id); err != nil {
			return Result{}, fmt.Errorf("bootstrap: enqueuing triage for issue %s (sweep-window delta): %w", id, err)
		}
		jobCount++
		newCount++
	}
	if log != nil {
		log.Info("bootstrap: covered issues created during the sweep window", "count", newCount, "leftToFeed", skippedToFeed)
	}

	return Result{HeadSeq: head, BootstrapJobCount: jobCount, SkippedCount: skippedToFeed, HeldClaimIssueIDs: held}, nil
}

// enqueueBootstrapTriage builds and enqueues one synthetic TRIAGE job for a backfilled issue id,
// carrying BootstrapSeq so its jobId (kind+issueId+BootstrapSeq) is stable across Bootstrap runs
// (plan §2.1) regardless of which of Bootstrap's two listing passes discovered it.
func enqueueBootstrapTriage(enqueue Enqueuer, id string) error {
	synthetic := Event{
		Seq:  BootstrapSeq,
		Type: "created",
		Issue: &EventIssue{
			ID:     id,
			Status: "unresolved",
		},
	}
	return enqueue.Enqueue(synthetic, KindTriage)
}
