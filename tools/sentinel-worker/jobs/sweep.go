// sweep.go implements the plan §2.7/§4.3 periodic sweep: claim heartbeat, nag, and reaped-claim
// reconcile. It runs on its own timer (WORKER_SWEEP_INTERVAL, default 1h — the caller, main.go's
// wiring, owns the ticker; this file's Sweep.Run is one pass) and is entirely separate from the
// per-job TRIAGE/FOLLOW-UP pipeline in loop/runner.go: nothing here consults an Advisor.
package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/sentinel"
	"github.com/NurfitraPujo/sentinel/tools/sentinel-worker/state"
)

// DefaultSweepInterval / DefaultClaimHeartbeat / DefaultNagDays are the plan §2.7/§4.3 defaults.
const (
	DefaultSweepInterval  = time.Hour
	DefaultClaimHeartbeat = 12 * time.Hour
	DefaultNagDays        = 3
)

// ValidateHeartbeatBelowStale is the plan §2.7 startup check: "Startup validates
// WORKER_CLAIM_HEARTBEAT < CLAIM_STALE_HOURS when the latter is known." staleHours<=0 means the
// stale threshold is not known to the caller (e.g. not yet fetched from the server) and the check
// is skipped — callers must not treat "unknown" as "invalid".
func ValidateHeartbeatBelowStale(heartbeat time.Duration, staleHours float64) error {
	if staleHours <= 0 {
		return nil
	}
	stale := time.Duration(staleHours * float64(time.Hour))
	if heartbeat >= stale {
		return fmt.Errorf("jobs: sweep: WORKER_CLAIM_HEARTBEAT (%s) must be less than CLAIM_STALE_HOURS (%s)", heartbeat, stale)
	}
	return nil
}

// heldClaim is one issue the sweep believes this worker currently holds a claim on, for the
// heartbeat pass (§2.7). LastActivity is when we last wrote anything to it (a comment, progress,
// question, or a prior heartbeat) — the sweep only heartbeats claims that have gone quiet.
type heldClaim struct {
	IssueID      string
	LastActivity time.Time
}

// HeldClaimSource supplies the sweep's view of claims this worker currently holds, keyed by
// issueID, with each claim's last-activity timestamp — normally backed by
// GET /api/agent/issues?claimed=me plus the journal's own record of our last write per issue
// (bootstrap's claimed=me seeding pass, plan §2.1, populates the same view at startup). Kept as an
// interface so tests can supply a fixed set without a fake server.
type HeldClaimSource interface {
	HeldClaims(ctx context.Context) ([]heldClaim, error)
}

// waitingIssue is one row of GET /api/agent/issues?waiting=true&claimed=me (plan §4.3's nag
// source), with waitingSince resolved per C12 (server-side field, journal fallback for pre-N9
// rows where it is null — ResolveWaitingSince below implements that fallback).
type waitingIssue struct {
	IssueID      string
	WaitingSince time.Time
}

// Sender used by sweep is the same narrow batch/question surface act.go's Sender already defines
// — reused so a test fake or the real *sentinel.Client satisfies both without a second interface.

// Sweep runs the plan §2.7/§4.3 periodic maintenance pass: claim heartbeat, nag, and reaped-claim
// reconcile. One Sweep is built once and Run is called every WORKER_SWEEP_INTERVAL by the caller's
// own ticker (main.go, a later wiring phase) — Sweep itself does not schedule anything.
type Sweep struct {
	Client  *sentinel.Client
	Journal *state.Journal

	// Execute mirrors loop.Runner.DryRun (plan §5: "dry-run must send NOTHING"): Sweep.Run and
	// ReconcileReaped are no-ops when Execute is false. Without this, a dry-run worker's sweep
	// ticker would still POST heartbeats/nags/releases/re-claims via s.Client directly -- the
	// dry-run contract belongs to loop.Runner alone otherwise, and nothing stops a caller from
	// constructing a *Sweep and driving it without ever consulting cfg.WorkerExecute. Wired from
	// main.go as `Execute: cfg.WorkerExecute`, matching every other mutating path's gate.
	Execute bool

	// Heartbeat/NagAfter/NagRelease bound the sweep's timers (plan §2.7/§4.3):
	// Heartbeat: WORKER_CLAIM_HEARTBEAT (default DefaultClaimHeartbeat).
	// NagAfter: WORKER_NAG_DAYS as a duration (default DefaultNagDays days) — one reminder past
	// this age; NagRelease is "> 2x" per the plan, computed as 2*NagAfter when left zero.
	Heartbeat  time.Duration
	NagAfter   time.Duration
	NagRelease time.Duration

	// Now returns the current time; overridable for tests (plan §8: "nag thresholds (injected
	// clock)"). Defaults to time.Now.
	Now func() time.Time

	// MyAgentID is this worker's agent identity — used only for logging/journal payloads here;
	// the server's claimed=me/waiting=true&claimed=me filters already scope by credential (C1),
	// not by this field.
	MyAgentID string

	// FixPRStatusHook is the documented N8f seam (plan §4.3: "The FIX-PR-status poll is a
	// documented N8f seam ... stub the hook"): called once per held claim during the heartbeat
	// pass so a later phase can slot in PR-status polling for FIX-originated claims without
	// touching this file's control flow again. nil is a valid no-op.
	FixPRStatusHook func(ctx context.Context, issueID string)

	// OnHeartbeatPosted, when non-nil, is called once per issue immediately after heartbeatOne
	// successfully POSTs its heartbeat (plan §7 "heartbeats_posted"). Wired from main.go to
	// health.Status.Inc(health.MetricHeartbeatsPosted, 1). nil is a no-op, matching this repo's
	// "nil seam disables the feature" convention (Runner.OnOutcome/OnCircuitOpen etc.).
	OnHeartbeatPosted func(issueID string)
}

func (s *Sweep) heartbeat() time.Duration {
	if s.Heartbeat > 0 {
		return s.Heartbeat
	}
	return DefaultClaimHeartbeat
}

func (s *Sweep) nagAfter() time.Duration {
	if s.NagAfter > 0 {
		return s.NagAfter
	}
	return DefaultNagDays * 24 * time.Hour
}

func (s *Sweep) nagRelease() time.Duration {
	if s.NagRelease > 0 {
		return s.NagRelease
	}
	return 2 * s.nagAfter()
}

func (s *Sweep) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Result summarizes one Sweep.Run pass, for logging/metrics.
type Result struct {
	Heartbeats int
	Nags       int
	Releases   int
	Reconciled int
	Errors     []error
}

// Run executes one full sweep pass: heartbeat, then nag, then reconcile (plan §2.7 (a)/(b)/(c) —
// order matters only in that a reconciled re-claim should be eligible for the SAME pass's
// heartbeat/nag treatment on the NEXT run, not this one, so reconcile runs last). Errors from
// individual issues are collected, not fatal to the whole pass — one bad issue must not stop the
// sweep from heartbeating/nagging every other held claim.
func (s *Sweep) Run(ctx context.Context, held HeldClaimSource) Result {
	var res Result
	if !s.Execute {
		return res
	}

	claims, err := held.HeldClaims(ctx)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("jobs: sweep: listing held claims: %w", err))
	} else {
		for _, c := range claims {
			if err := s.heartbeatOne(ctx, c); err != nil {
				res.Errors = append(res.Errors, err)
				continue
			}
			if s.now().Sub(c.LastActivity) >= s.heartbeat() {
				res.Heartbeats++
			}
			if s.FixPRStatusHook != nil {
				s.FixPRStatusHook(ctx, c.IssueID)
			}
		}
	}

	waiting, err := s.listWaiting(ctx)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Errorf("jobs: sweep: listing waiting issues: %w", err))
	} else {
		for _, w := range waiting {
			age := s.now().Sub(w.WaitingSince)
			switch {
			case age >= s.nagRelease():
				if err := s.releaseWithHandback(ctx, w.IssueID); err != nil {
					res.Errors = append(res.Errors, err)
					continue
				}
				res.Releases++
			case age >= s.nagAfter():
				if err := s.nagOne(ctx, w.IssueID, w.WaitingSince); err != nil {
					res.Errors = append(res.Errors, err)
					continue
				}
				res.Nags++
			}
		}
	}

	return res
}

// heartbeatOne posts a heartbeat issues.progress update for c when it has gone quiet longer than
// s.heartbeat() (plan §2.7: "posts an issues.progress heartbeat on every held claim older than
// WORKER_CLAIM_HEARTBEAT since our last activity"). The text embeds the current timestamp so C5's
// exact-body progress dedupe cannot swallow it as a duplicate of a prior heartbeat.
func (s *Sweep) heartbeatOne(ctx context.Context, c heldClaim) error {
	if s.now().Sub(c.LastActivity) < s.heartbeat() {
		return nil
	}
	now := s.now()
	text := s.heartbeatText(c.IssueID, now)
	key := sentinel.IdempotencyKey(heartbeatJobID(c.IssueID, now), 0)
	res, err := s.Client.PostProgress(ctx, c.IssueID, text, key)
	if err != nil {
		return fmt.Errorf("jobs: sweep: heartbeat for issue %s: %w", c.IssueID, err)
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("jobs: sweep: heartbeat for issue %s: status %d: %s", c.IssueID, res.Status, sentinel.ErrorMessage(res.Body))
	}
	if s.OnHeartbeatPosted != nil {
		s.OnHeartbeatPosted(c.IssueID)
	}
	return nil
}

// heartbeatText builds the timestamp-varied heartbeat body (plan §2.7).
func (s *Sweep) heartbeatText(issueID string, at time.Time) string {
	return fmt.Sprintf("🤖 still working this issue as of %s — claim heartbeat.", at.UTC().Format(time.RFC3339))
}

// heartbeatJobID derives a stable-per-heartbeat idempotency scope: same issue + same minute
// collapses to one key (protects against a caller invoking Run twice in quick succession from
// double-posting), while a genuinely new heartbeat cycle (a new minute) gets a fresh key so the
// text-varying dedup-avoidance above is backed by a distinct key too, not just a distinct body.
func heartbeatJobID(issueID string, at time.Time) string {
	return "sweep-heartbeat:" + issueID + ":" + at.UTC().Truncate(time.Minute).Format(time.RFC3339)
}

// nagOne posts the plan §4.3 one-time reminder comment for a stale waiting issue. The idempotency
// key is derived from issueID + waitingSince (not issueID alone): a fixed 'sweep-nag:<issueID>:0'
// key means a SECOND waiting episode on the same issue (question answered, then a new question
// asked later, waitingSince reset) collides with the first episode's key and gets deduped away
// silently -- the reporter never sees the second reminder. Scoping the key to the episode's
// waitingSince gives each episode its own one-shot key.
func (s *Sweep) nagOne(ctx context.Context, issueID string, waitingSince time.Time) error {
	body := "🤖 Following up: still waiting on a reply here. Let me know if you have an update, or I'll release this issue soon."
	key := sentinel.IdempotencyKey("sweep-nag:"+issueID+":"+waitingSince.UTC().Format(time.RFC3339), 0)
	res, err := s.Client.PostComment(ctx, issueID, body, key)
	if err != nil {
		return fmt.Errorf("jobs: sweep: nag for issue %s: %w", issueID, err)
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("jobs: sweep: nag for issue %s: status %d: %s", issueID, res.Status, sentinel.ErrorMessage(res.Body))
	}
	return nil
}

// releaseWithHandback posts a hand-back comment and releases the claim (plan §4.3: "> 2x ⇒
// release with a hand-back comment") as one batch, comment before release (§2.3's fixed op
// order). Per C3, a batch is HTTP 200 with per-op outcomes in results[] -- the envelope status
// alone says nothing about whether the release itself succeeded, so checkBatchResults (the SAME
// per-op classifier RealActor.Act uses) walks results[] and surfaces a failed op as an error
// instead of this returning a clean nil for a release that never actually landed.
func (s *Sweep) releaseWithHandback(ctx context.Context, issueID string) error {
	body := "🤖 Releasing this issue: no reply after multiple reminders. A human should pick this up."
	jobID := "sweep-release:" + issueID
	b := newOpBuilder(jobID)
	b.add("issues.comment", issueID, map[string]interface{}{"body_md": body})
	if err := b.addRelease(issueID); err != nil {
		return err
	}
	res, err := s.Client.PostBatch(ctx, sentinel.BatchRequest{Operations: b.ops, StopOnError: false})
	if err != nil {
		return fmt.Errorf("jobs: sweep: release for issue %s: %w", issueID, err)
	}
	if res.Status < 200 || res.Status >= 300 {
		return fmt.Errorf("jobs: sweep: release for issue %s: status %d: %s", issueID, res.Status, sentinel.ErrorMessage(res.Body))
	}
	if err := checkBatchResults(Compiled{Ops: b.ops}, res); err != nil {
		return fmt.Errorf("jobs: sweep: release for issue %s: %w", issueID, err)
	}
	return nil
}

// ClientHeldClaims is the real HeldClaimSource (plan §2.7, main.go's N8d wiring): it lists claims
// via GET /api/agent/issues?claimed=me (server-scoped by credential per C1, no status filter — a
// held claim on a resolved issue is still a claim we hold) and resolves each issue's LastActivity
// from the journal's most recent record for that issue (our own last write), falling back to the
// zero Time when the journal has nothing for an issue (e.g. a claim held from before this
// worker's journal existed, or reclaimed via bootstrap's held-claims seed) — a zero LastActivity
// makes heartbeatOne's staleness check trivially true, so an unknown-activity claim is
// heartbeated on the very next sweep pass rather than silently skipped.
type ClientHeldClaims struct {
	Client  *sentinel.Client
	Journal *state.Journal
}

// HeldClaims implements HeldClaimSource.
func (c ClientHeldClaims) HeldClaims(ctx context.Context) ([]heldClaim, error) {
	res, err := c.Client.ListIssues(ctx, sentinel.IssuesListOptions{Claimed: "me", Limit: 200})
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, fmt.Errorf("GET /api/agent/issues?claimed=me: status %d: %s", res.Status, sentinel.ErrorMessage(res.Body))
	}
	var env issuesListEnvelope
	if err := json.Unmarshal(res.Body, &env); err != nil {
		return nil, fmt.Errorf("decoding issues list: %w", err)
	}
	lastActivity := c.lastActivityByIssue()
	out := make([]heldClaim, 0, len(env.Issues))
	for _, row := range env.Issues {
		out = append(out, heldClaim{IssueID: row.ID, LastActivity: lastActivity[row.ID]})
	}
	return out, nil
}

// lastActivityByIssue scans the journal's latest-by-jobId view and reduces it to the single most
// recent record timestamp per issueId, across every kind (TRIAGE/FOLLOW-UP/heartbeat jobIds all
// count as "activity" for staleness purposes). Errors reading the journal are treated as "no
// activity known" — HeldClaims still returns the claim list so the sweep degrades to
// heartbeating everything rather than failing outright.
func (c ClientHeldClaims) lastActivityByIssue() map[string]time.Time {
	out := map[string]time.Time{}
	if c.Journal == nil {
		return out
	}
	latest, err := c.Journal.LatestByJobID()
	if err != nil {
		return out
	}
	for _, rec := range latest {
		if cur, ok := out[rec.IssueID]; !ok || rec.At.After(cur) {
			out[rec.IssueID] = rec.At
		}
	}
	return out
}

// agentIssueListItemView decodes just the fields the sweep needs from one row of
// GET /api/agent/issues (agent-work.ts's AgentIssueListItem).
type agentIssueListItemView struct {
	ID           string     `json:"id"`
	WaitingOn    *string    `json:"waitingOn"`
	WaitingSince *time.Time `json:"waitingSince"`
}

type issuesListEnvelope struct {
	Issues []agentIssueListItemView `json:"issues"`
}

// listWaiting calls GET /api/agent/issues?waiting=true&claimed=me (both server-side per C12) and
// resolves each row's waitingSince, falling back to the journal for pre-N9 rows where the server
// field is null (plan §4.3: "journal fallback for pre-N9 rows where it is null").
func (s *Sweep) listWaiting(ctx context.Context) ([]waitingIssue, error) {
	res, err := s.Client.ListIssues(ctx, sentinel.IssuesListOptions{Claimed: "me", Waiting: true, Limit: 200})
	if err != nil {
		return nil, err
	}
	if res.Status < 200 || res.Status >= 300 {
		return nil, fmt.Errorf("status %d: %s", res.Status, sentinel.ErrorMessage(res.Body))
	}
	var env issuesListEnvelope
	if err := json.Unmarshal(res.Body, &env); err != nil {
		return nil, fmt.Errorf("decoding issues list: %w", err)
	}
	var out []waitingIssue
	for _, row := range env.Issues {
		if row.WaitingOn == nil {
			continue
		}
		since := s.resolveWaitingSince(row)
		if since.IsZero() {
			continue
		}
		out = append(out, waitingIssue{IssueID: row.ID, WaitingSince: since})
	}
	return out, nil
}

// resolveWaitingSince implements the C12 fallback: prefer the server-supplied waitingSince; when
// null, fall back to the journal's most recent StateQuestioned record's timestamp for this issue
// (the "questioned" journal marker per plan §2.2), which is when THIS worker asked the still-open
// question. A zero time is returned when neither source has an answer — the caller skips the row
// rather than nag off a fabricated age.
func (s *Sweep) resolveWaitingSince(row agentIssueListItemView) time.Time {
	if row.WaitingSince != nil && !row.WaitingSince.IsZero() {
		return *row.WaitingSince
	}
	if s.Journal == nil {
		return time.Time{}
	}
	records, _, err := s.Journal.Load()
	if err != nil {
		return time.Time{}
	}
	var at time.Time
	for _, r := range records {
		if r.IssueID == row.ID && r.State == state.StateQuestioned {
			if r.At.After(at) {
				at = r.At
			}
		}
	}
	return at
}

// ReconcileReaped implements plan §2.7(c): when a claim_released(previousAssignee=me, reason=stale)
// event arrives, re-claim (not re-triage) IF the journal shows an open question or an open fix-PR
// for the issue — otherwise the caller should let normal dispatch handle it (a healthy release we
// intentionally made, e.g. needs_human, must never be reconciled back). Returns whether a
// re-claim was warranted and performed.
func (s *Sweep) ReconcileReaped(ctx context.Context, issueID string) (reclaimed bool, err error) {
	if !s.Execute {
		return false, nil
	}
	if s.Journal == nil {
		return false, fmt.Errorf("jobs: sweep: reconcile: nil Journal")
	}
	openQuestion, err := s.Journal.HasOpenQuestion(issueID)
	if err != nil {
		return false, fmt.Errorf("jobs: sweep: reconcile: checking open question for issue %s: %w", issueID, err)
	}
	openFix := s.hasOpenFix(issueID)
	if !openQuestion && !openFix {
		return false, nil
	}
	res, conflict, err := s.Client.ClaimIssue(ctx, issueID)
	if err != nil {
		return false, fmt.Errorf("jobs: sweep: reconcile: re-claiming issue %s: %w", issueID, err)
	}
	if conflict != nil {
		// A foreign claimant beat us to it since the reap — nothing to reconcile onto anymore.
		return false, nil
	}
	if res.Status < 200 || res.Status >= 300 {
		return false, fmt.Errorf("jobs: sweep: reconcile: re-claiming issue %s: status %d: %s", issueID, res.Status, sentinel.ErrorMessage(res.Body))
	}
	return true, nil
}

// openFixState is the journal state a FIX-originated job sits in while its PR is out for review.
// As of N8f, jobs/fix.go's JournalFixPROpen (jobs/fix_pr.go) appends exactly this Kind/State pair
// once RunFix opens a PR, so this hook is LIVE: ReconcileReaped's fix-PR arm now actually fires off
// real FIX jobs, not just the hand-injected records fix_pr_test.go/sweep_test.go use to exercise it
// in isolation.
const openFixKind = state.FixKind

// hasOpenFix reports whether issueID has a FIX-kind job whose latest journal record is non-terminal
// (an in-flight FIX, e.g. workspace prep or PR-out-for-review). Live as of N8f: JournalFixPROpen
// (jobs/fix_pr.go) appends a Kind=openFixKind/State=StateActed record when RunFix opens a PR, and
// journalFixPRClosed marks it terminal once the PR is merged/closed — so this reflects real FIX
// jobs' journal state, not just test fixtures.
func (s *Sweep) hasOpenFix(issueID string) bool {
	open, err := s.Journal.HasOpenKind(issueID, openFixKind)
	if err != nil {
		return false
	}
	return open
}
