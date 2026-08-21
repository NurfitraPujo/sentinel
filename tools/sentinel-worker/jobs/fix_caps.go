// jobs/fix_caps.go implements the FIX engine's plan §2.6 volume caps (N8f "fix-pr-resume-caps"):
// WORKER_MAX_FIX_JOBS_PER_DAY, WORKER_MAX_PRS_PER_DAY (per-repo AND total), and
// WORKER_MAX_FIX_ATTEMPTS counted per jobID (a crash-resume of the SAME job does NOT count again;
// a fresh FIX start does — CLAUDE.md). All windows reset at 00:00 UTC, reusing llm/budget.go's
// DailyCounter rather than reimplementing day-rollover bookkeeping a third time.
//
// Exhausted caps mean the caller queues/skips the job with a metric, never a crash (CLAUDE.md).
// FixCaps itself never panics or errors on exhaustion — every check returns a plain bool (or, for
// AllowPR, false) and, when OnExhausted is set, reports a stable reason string so a caller can
// wire it straight to a metrics sink without string-matching an error.
package jobs

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/llm"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// Plan §2.6/§5 defaults for the FIX engine's volume caps and per-attempt timeout.
const (
	DefaultMaxFixJobsPerDay = 10
	DefaultMaxPRsPerDay     = 10
	DefaultMaxFixAttempts   = 2
	DefaultFixTimeout       = 30 * time.Minute
)

// FixCaps enforces the plan §2.6 FIX volume caps for one worker process. It is safe for
// concurrent use — every method locks internally, and AllowPR's total+per-repo pair is checked
// and committed atomically under one critical section so two concurrently racing PRs for
// different repos can never both observe headroom in the shared total counter and then both
// commit, pushing it over the configured limit (llm.DailyCounter.Allow-then-TryIncrement as two
// separate calls would have exactly that race — see AllowPR's doc).
type FixCaps struct {
	jobsPerDay     *llm.DailyCounter // WORKER_MAX_FIX_JOBS_PER_DAY
	prsTotal       *llm.DailyCounter // WORKER_MAX_PRS_PER_DAY, total across all repos
	prsPerDayLimit int               // same numeric limit, applied per-repo too (plan §2.6: "per-repo AND total")
	maxAttempts    int               // WORKER_MAX_FIX_ATTEMPTS
	clock          llm.Clock

	mu            sync.Mutex
	prsPerRepo    map[string]*llm.DailyCounter // lazily created, one per repoKey
	prsPerProject map[string]*projectDayCount  // lazily created, one per projectID (finding 4, C15)
	attempts      map[string]int               // jobID -> attempts started (RecordAttempt calls)
	// issueAttemptDay is the UTC calendar day (dayKeyUTC) issueAttempts was last reset for (finding
	// 8): without a reset, issueAttempts accumulated across every day the worker process stayed up,
	// so an issue that hit WORKER_MAX_FIX_ATTEMPTS once stayed permanently blocked instead of getting
	// a fresh budget every UTC day like every other cap in this file (jobsPerDay/prsTotal/prsPerRepo
	// all reset via llm.DailyCounter's own dayKey rollover; prsPerProject resets via projectCounter's
	// own day field). Reset happens lazily, the same "first call to observe a new day rolls it over"
	// convention every other counter here uses.
	issueAttemptDay string
	// issueAttempts is finding 6's (fix-lifecycle remediation round 2) real PER-ISSUE cap:
	// issueID -> attempts started across EVERY jobID that has ever attempted a FIX for that issue.
	// attempts above is keyed by jobID = JobID(FixKind, issue, triggerSeq) (state.JobID), so
	// WORKER_MAX_FIX_ATTEMPTS as enforced by AllowAttempt/attempts was really "per-trigger-seq, not
	// per-issue" (plan §2.6 says "per-issue"): a single issue that re-triggers a FIX from several
	// different events (occurrence_burst, then regressed, then another occurrence_burst, ...) mints
	// a fresh jobID each time, and each fresh jobID gets its OWN full WORKER_MAX_FIX_ATTEMPTS budget
	// under attempts alone -- unbounded attempts for one issue over enough re-triggers.
	// AllowIssueAttempt/RecordIssueAttempt below add the real per-issue cap ALONGSIDE (not instead
	// of) the existing per-jobID one; both must allow before an attempt proceeds.
	issueAttempts map[string]int

	// OnExhausted, when non-nil, is called once per rejected check with a stable reason:
	// "fix-jobs-per-day" | "prs-per-day" | "prs-per-day-repo:<repoKey>" | "fix-attempts". Never
	// called on a successful check. nil is a valid no-op (metrics wiring is optional).
	OnExhausted func(reason string)
}

// NewFixCaps builds a FixCaps. clock defaults to the real wall clock when nil (pass a fake in
// tests, plan §8: "injected clocks, no real sleeps"). maxAttempts <= 0 falls back to
// DefaultMaxFixAttempts — a zero-value config must not silently mean "unlimited attempts", which
// llm.DailyCounter's own <=0-means-unlimited convention would otherwise imply if reused verbatim
// here; attempts-per-jobID has no such "unlimited" reading in the plan.
func NewFixCaps(maxJobsPerDay, maxPRsPerDay, maxAttempts int, clock llm.Clock) *FixCaps {
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxFixAttempts
	}
	if clock == nil {
		// FixCaps.clock (unlike llm.DailyCounter, which defaults nil internally) is also read
		// directly by projectCounter's own day-rollover logic (finding 4) — it must never be nil.
		clock = llm.ClockFunc(time.Now)
	}
	return &FixCaps{
		jobsPerDay:     llm.NewDailyCounter(maxJobsPerDay, clock),
		prsTotal:       llm.NewDailyCounter(maxPRsPerDay, clock),
		prsPerDayLimit: maxPRsPerDay,
		maxAttempts:    maxAttempts,
		clock:          clock,
		prsPerRepo:     map[string]*llm.DailyCounter{},
		prsPerProject:  map[string]*projectDayCount{},
		attempts:       map[string]int{},
		issueAttempts:  map[string]int{},
	}
}

// FixJobsRemaining reports how many WORKER_MAX_FIX_JOBS_PER_DAY slots are left today (finding 10,
// plan §7 "cap remaining" gauge family) -- -1 means unlimited, mirroring llm.DailyCounter.
// Remaining's own convention.
func (c *FixCaps) FixJobsRemaining() int { return c.jobsPerDay.Remaining() }

// PRsRemaining reports how many WORKER_MAX_PRS_PER_DAY slots are left today against the TOTAL
// (cross-repo) counter (finding 10) -- -1 means unlimited. Per-repo/per-project remaining is not
// exposed here: those counters are lazily created per key (repoCounter/projectCounter) with no
// fixed, enumerable set of keys to report a flat gauge family for.
func (c *FixCaps) PRsRemaining() int { return c.prsTotal.Remaining() }

func (c *FixCaps) report(reason string) {
	if c.OnExhausted != nil {
		c.OnExhausted(reason)
	}
}

// AllowJobStart atomically checks and consumes one WORKER_MAX_FIX_JOBS_PER_DAY slot. Callers gate
// starting a FIX job (fresh workspace prep) on this — NOT on resuming an in-flight one (a resume
// isn't "one more job", it's the same job continuing).
func (c *FixCaps) AllowJobStart() bool {
	if c.jobsPerDay.TryIncrement() {
		return true
	}
	c.report("fix-jobs-per-day")
	return false
}

// repoCounter returns repoKey's per-repo PR counter, creating it (at the same prsPerDayLimit) on
// first use. Caller must hold c.mu.
func (c *FixCaps) repoCounter(repoKey string) *llm.DailyCounter {
	if rc, ok := c.prsPerRepo[repoKey]; ok {
		return rc
	}
	rc := llm.NewDailyCounter(c.prsPerDayLimit, c.clock)
	c.prsPerRepo[repoKey] = rc
	return rc
}

// projectDayCount is a per-UTC-day PR count for one project, checked against a LIMIT SUPPLIED PER
// CALL rather than fixed at construction (finding 4): unlike repoCounter/prsTotal, whose numeric
// limit is one fixed WORKER_MAX_PRS_PER_DAY value for the whole process, a project's own limit
// (settings.ProjectSettings.MaxPRsPerDay, C15) can differ per project and can change between
// refreshes — reusing llm.DailyCounter (whose limit is baked in at construction) would freeze the
// first-seen limit for the counter's lifetime. day is reset (count zeroed) the first time a call
// observes a new UTC calendar day, the same rollover rule llm.DailyCounter itself uses.
type projectDayCount struct {
	day   string
	count int
}

// dayKeyUTC identifies the UTC calendar day t falls in — mirrors llm's own unexported dayKey (kept
// private to that package), needed here because projectDayCount's rollover can't reuse
// llm.DailyCounter's internals.
func dayKeyUTC(t time.Time) string { return t.UTC().Format("2006-01-02") }

// projectCounter returns projectID's projectDayCount, resetting it to zero if the UTC day has
// rolled over since it was last touched, and creating it on first use. Caller must hold c.mu.
func (c *FixCaps) projectCounter(projectID string) *projectDayCount {
	key := dayKeyUTC(c.clock.Now())
	pc, ok := c.prsPerProject[projectID]
	if !ok {
		pc = &projectDayCount{day: key}
		c.prsPerProject[projectID] = pc
	} else if pc.day != key {
		pc.day = key
		pc.count = 0
	}
	return pc
}

// AllowPR atomically checks repoKey's per-repo counter, projectID's per-project counter (C15:
// settings.ProjectSettings.MaxPRsPerDay, keyed by projectID — projectLimit nil means the server
// reported no per-project cap, so the global WORKER_MAX_PRS_PER_DAY limit is reused for that
// project's own counter too), and the total WORKER_MAX_PRS_PER_DAY counter, consuming one slot
// from each ONLY if all three currently have headroom (plan §2.6: "10, per-repo AND total", plus
// C15's per-project cap layered on top). repoKey/projectID are stable caller-chosen identifiers,
// not interpreted here. Checking and committing all three counters under one held c.mu (rather
// than separate llm.DailyCounter.TryIncrement calls) is what prevents concurrent AllowPR calls
// from both reading a counter as under-limit and both committing, pushing spend over the
// configured cap.
//
// The per-repo counter is checked FIRST, deliberately: since the total counter's spend is always
// >= any single repo's own spend (every PR increments both), a repo that has independently reached
// the shared numeric limit will always have driven the total counter to at least that same limit
// too — so if AllowPR checked the total counter first, the more specific and actionable
// "prs-per-day-repo:<repoKey>" report would never be reachable (the total check would always fire
// first once any one repo saturates it). The per-project counter is checked second, ahead of the
// total, for the identical reason — a per-project cap is the most specific gate available once the
// repo itself has headroom.
func (c *FixCaps) AllowPR(repoKey, projectID string, projectLimit *int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	rc := c.repoCounter(repoKey)
	if !rc.Allow() {
		c.report("prs-per-day-repo:" + repoKey)
		return false
	}
	pc := c.projectCounter(projectID)
	// finding 7: projectLimit's zero value is ambiguous between "unset" (nil, the server reported no
	// per-project cap -- fall back to the global WORKER_MAX_PRS_PER_DAY) and an explicit
	// server-supplied 0 (settings.ProjectSettings.MaxPRsPerDay==0, meaning "zero fix PRs allowed for
	// this project"). Only a nil pointer means unset here -- *int lets the C15 wire format
	// distinguish the two, and treating an explicit 0 the same as nil (as `limit > 0` used to, by
	// folding both into "no cap enforced") let a project an operator had explicitly zeroed out still
	// get PRs via the global fallback.
	if projectLimit != nil {
		limit := *projectLimit
		if pc.count >= limit { // limit==0 correctly blocks unconditionally (pc.count is always >= 0)
			c.report("prs-per-day-project:" + projectID)
			return false
		}
	} else if limit := c.prsPerDayLimit; limit > 0 && pc.count >= limit {
		c.report("prs-per-day-project:" + projectID)
		return false
	}
	if !c.prsTotal.Allow() {
		c.report("prs-per-day")
		return false
	}
	rc.TryIncrement()
	pc.count++
	c.prsTotal.TryIncrement()
	return true
}

// AllowAttempt reports whether jobID may start (a fresh attempt) or continue (a resume) without
// exceeding WORKER_MAX_FIX_ATTEMPTS. It does not itself consume anything — see RecordAttempt.
func (c *FixCaps) AllowAttempt(jobID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.attempts[jobID] >= c.maxAttempts {
		c.report("fix-attempts")
		return false
	}
	return true
}

// RecordAttempt increments jobID's attempt count. Callers MUST call this exactly once per FRESH
// FIX attempt start (a brand-new workspace/clone) and MUST NOT call it for a resume of the same
// attempt (CLAUDE.md: "a crash-resume of the same job does NOT count again; a fresh FIX start
// does") — a patch-apply-failure clean restart of a resume is likewise not a fresh start in this
// sense; whether it re-consumes an attempt is the caller's policy choice, this type only tracks
// what it's told.
func (c *FixCaps) RecordAttempt(jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts[jobID]++
}

// AttemptCount returns jobID's current attempt count (test/observability convenience — not itself
// load-bearing for any gate).
func (c *FixCaps) AttemptCount(jobID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts[jobID]
}

// resetIssueAttemptsIfNewDay rolls issueAttempts back to empty the first time it observes a new
// UTC calendar day since it was last touched (finding 8) -- caller must hold c.mu.
func (c *FixCaps) resetIssueAttemptsIfNewDay() {
	key := dayKeyUTC(c.clock.Now())
	if key != c.issueAttemptDay {
		c.issueAttemptDay = key
		c.issueAttempts = map[string]int{}
	}
}

// AllowIssueAttempt reports whether issueID may start (a fresh attempt, under a NEW jobID) or
// continue (a resume, same jobID) a FIX attempt without exceeding WORKER_MAX_FIX_ATTEMPTS counted
// PER ISSUE (finding 6, fix-lifecycle remediation round 2) -- the real "per-issue" cap the plan
// documents, alongside (not instead of) AllowAttempt's existing per-jobID cap. Callers MUST check
// both AllowAttempt(jobID) AND AllowIssueAttempt(issueID) before proceeding, and MUST call both
// RecordAttempt(jobID) AND RecordIssueAttempt(issueID) on a fresh attempt start (never on a resume,
// same rule as RecordAttempt's own doc comment). It does not itself consume anything.
func (c *FixCaps) AllowIssueAttempt(issueID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIssueAttemptsIfNewDay()
	if c.issueAttempts[issueID] >= c.maxAttempts {
		c.report("fix-attempts-issue")
		return false
	}
	return true
}

// RecordIssueAttempt increments issueID's attempt count -- see AllowIssueAttempt's doc comment for
// when callers must (and must not) call this.
func (c *FixCaps) RecordIssueAttempt(issueID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIssueAttemptsIfNewDay()
	c.issueAttempts[issueID]++
}

// IssueAttemptCount returns issueID's current per-issue attempt count (test/observability
// convenience — not itself load-bearing for any gate).
func (c *FixCaps) IssueAttemptCount(issueID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetIssueAttemptsIfNewDay()
	return c.issueAttempts[issueID]
}

// SeedToday reconstructs today's (UTC, per `now`) fix-job/PR/attempt counts from journal records
// (finding 2): without this, restarting the worker process reset every FixCaps counter to zero,
// so "per day" was really "per process life" and WORKER_MAX_FIX_ATTEMPTS/WORKER_MAX_FIX_JOBS_PER_
// DAY/WORKER_MAX_PRS_PER_DAY all silently reset on a crash-loop. Must be called exactly once at
// boot, BEFORE any AllowJobStart/AllowPR/AllowAttempt call, with the FULL journal record sequence
// (state.Journal.Load's first return value) — not just the latest-per-jobId view, since a single
// jobId can carry more than one FixKind record in a day (one fix_running record per attempt).
//
// Per record, Kind must be FixKind (jobs/fix_pr.go's exported alias of the FIX engine's journal
// Kind) and At must fall on `now`'s UTC calendar day:
//   - State==state.StateFixRunning: one FIX attempt started for that record's JobID
//     (journalFixRunning is called once per attempt, fresh or resumed — see fix.go's package doc).
//     Each distinct JobID with at least one such record counts as one WORKER_MAX_FIX_JOBS_PER_DAY
//     slot (a resume of the SAME jobId is not "one more job"); every occurrence increments that
//     jobId's WORKER_MAX_FIX_ATTEMPTS attempt count.
//   - State==state.StateActed (JournalFixPROpen's marker, jobs/fix_pr.go): one PR opened today,
//     counted against the per-repo counter (repoKey derived from the payload's own RepoRef, not a
//     caller-supplied string, since SeedToday runs before any per-request repoKey is available),
//     the per-project counter (projectID recovered from the SAME JobID's fix_running record's
//     FixRunningPayload.Input.ProjectID, joined by JobID — FixPRPayload itself carries no
//     projectID), and the shared total counter.
//
// Corrupt/unparseable payloads are skipped rather than treated as fatal, matching state.Journal's
// own "a bad line degrades observability, never crashes recovery" posture.
func (c *FixCaps) SeedToday(records []state.Record, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Pin issueAttemptDay to `now`'s UTC day BEFORE seeding (finding 8): SeedToday runs once at
	// boot, before any AllowIssueAttempt/RecordIssueAttempt call, and resetIssueAttemptsIfNewDay
	// would otherwise wipe out exactly the counts this loop is about to seed the very next time
	// either is called with a `now` a hair later than this loop's own `now`.
	c.issueAttemptDay = dayKeyUTC(now)

	year, month, day := now.UTC().Date()
	isToday := func(t time.Time) bool {
		y, m, d := t.UTC().Date()
		return y == year && m == month && d == day
	}

	jobsStartedToday := map[string]bool{}
	projectByJob := map[string]string{}

	for _, r := range records {
		if r.Kind != FixKind || !isToday(r.At) {
			continue
		}
		switch r.State {
		case state.StateFixRunning:
			// jobsStartedToday counts the JOB (one WORKER_MAX_FIX_JOBS_PER_DAY slot) regardless of
			// Resumed -- a set, not a sum, so a resumed record for a jobID already seen today never
			// adds a second slot.
			jobsStartedToday[r.JobID] = true
			payload, decodeErr := DecodeFixRunningPayload(r.Payload)
			// Finding 5: only a FRESH attempt (Resumed==false) consumes a WORKER_MAX_FIX_ATTEMPTS
			// slot -- a crash-resume of the SAME attempt (Resumed==true, journaled by ResumeFix) must
			// not count again (CLAUDE.md: "a crash-resume of the same job does NOT count again"). A
			// record that fails to decode (or predates the Resumed field, which zero-values to false)
			// is treated as fresh -- the safer default, matching every pre-finding-5 record already in
			// a journal.
			resumed := decodeErr == nil && payload.Resumed
			if !resumed {
				c.attempts[r.JobID]++
				// Finding 6: seed the real per-issue cap too, keyed by r.IssueID (state.Record already
				// carries it directly -- no payload decode needed for this, unlike projectByJob below
				// which needs FixRunningPayload.Input.ProjectID).
				if r.IssueID != "" {
					c.issueAttempts[r.IssueID]++
				}
			}
			if decodeErr == nil {
				projectByJob[r.JobID] = payload.Input.ProjectID
			}
		case state.StateActed:
			var payload FixPRPayload
			if len(r.Payload) == 0 {
				continue
			}
			if err := json.Unmarshal(r.Payload, &payload); err != nil {
				continue // not a FixPRPayload record (e.g. a fix_running marker sharing the Kind)
			}
			repoKey := payload.Provider.Provider + "/" + payload.Provider.Owner + "/" + payload.Provider.Repo
			c.repoCounter(repoKey).SeedCount(c.repoCounter(repoKey).Count() + 1)
			c.prsTotal.SeedCount(c.prsTotal.Count() + 1)
			if projectID, ok := projectByJob[r.JobID]; ok && projectID != "" {
				c.projectCounter(projectID).count++
			}
		}
	}

	c.jobsPerDay.SeedCount(c.jobsPerDay.Count() + len(jobsStartedToday))
}
