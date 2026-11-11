# Agent-Automation Blocker Audit — 2026-08-14

## Remediation status (N7, 2026-08-14)

All findings below were addressed in branch `feat/agent-remediation`, phases N7a–f (plan:
`docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md`). Verified against `rtk git log main..HEAD
--oneline` and the cited code, not just commit messages.

| Finding | Outcome | Where |
|---|---|---|
| A01 (new-issue creation writes no event) | Remediated in N7a | `eafb88c` — processor `StoreEvent` writes a `created` issue_activity row on race-exact new-issue detection (`RETURNING xmax=0`) |
| A02 (no since/pagination on issue list) | Remediated in N7b | `fd752ee` — `since`/`sort`/`limit`/keyset `cursor` added to `GET /api/agent/issues` |
| A03 (stuck claims, no reaper) | Remediated in N7c | `fd752ee` — `issues.claimed_at` + `reapStaleClaims()` in the retention cron, `CLAIM_STALE_HOURS` (default 24) |
| A04 (retention deletes claimed/unresolved manual issues) | Remediated in N7c | `fd752ee` — occurrence-less deletion now requires resolved/ignored AND unclaimed, plus a separate `MANUAL_ISSUE_RETENTION_DAYS` cutoff |
| A05 (no idempotency on mutations) | Remediated in N7d | `3661d96` — exact-retry no-op on `updateIssueStatus`; natural-key dedupe on `createComment`/`recordAgentProgress` (2min window); blocking questions excluded by design |
| A06 (IssueOutcome computed but discarded) | Remediated in N7a | `eafb88c` — `IssueOutcomeNew` now drives the `created` row instead of being discarded |
| A07 (no in_progress status / claim visibility) | Accepted-with-docs | N7e — `claimedAt` exposed in agent list/detail + UI "agent working" badge; no new status added (deliberate — see plan) |
| A08 (agents can't edit/delete own comments) | Remediated in N7e | `faf6f34` — `comments.edit`/`comments.delete` ops + `PATCH`/`DELETE /api/agent/issues/:id/comments/:commentId`, ownership-gated (403/404) |
| A09 (agents can't set severity on manual issues) | Remediated in N7e | `faf6f34` — `issues.report.severity` op, `user_report`-only (400 on `system_error`) |
| A10 (fixed-window rate limiter allows ~2x burst at boundary) | Accepted-with-docs | `faf6f34` — behavior unchanged by design; `Retry-After` now emitted accurately from `resetAt`, and the guide/DEPLOYMENT.md document the fixed-window trade-off explicitly rather than presenting it as a hard ceiling |
| A11 (claim is advisory, no ownership re-check on mutations) | Accepted-with-docs | `faf6f34` — enforcement deliberately NOT added (would break existing human-parity mutation semantics); 409 bodies enriched with `claimedBy`/`claimedAt`, a structured `agent.mutated_claimed_issue` warn log added, and the guide's §4 states plainly that claims are advisory and must be checked, not trusted |
| A12 (caused_by relation cycles unguarded) | Remediated in N7d | `3661d96` — reverse-pair `caused_by` insert rejected 409 (`RelationCycleError`); explicitly a 2-cycle guard only, documented as such |
| A13 (release retry indistinguishable from real conflict) | Remediated in N7d | `3661d96` — releasing an already-unclaimed issue is now idempotent 200; only a genuine other-claimant still 409s |
| A14 (no scripted agent/key provisioning path) | Deferred-with-runbook | `faf6f34` — no Credentials/headless auth path added (would require a new auth provider, out of scope for this phase); `DEPLOYMENT.md` gained an agent-provisioning runbook documenting the one-time human bootstrap step instead |
| A15 (upload has no issue association at creation) | Remediated in N7f | `faf6f34` — CLI gained `upload <file> --issue <id> [--comment <text>]` one-shot (upload + attach via comment in one call); the old two-positional form is kept but deprecated with a warning; server-side `POST /api/agent/uploads` itself still takes no `issueId` (unchanged, documented) |
| R1 (refuted: no whoami endpoint; no self-service key rotation) | Remediated in N7f | `faf6f34` — `GET /api/agent/self` added (R1a); CLI `whoami` now calls it instead of probing reachability. `POST /api/agent/key/rotate` added (R1b) with `AGENT_KEY_ROTATION_GRACE_HOURS` grace window |
| R2 (refuted: repeat occurrences produce no event) | Remediated in N7a | `eafb88c` — `occurrence_burst` event added, throttled to 1/issue/`OCCURRENCE_EVENT_MIN_INTERVAL_SECONDS` (default 1h) |



Repo-wide audit answering: **what blocks or degrades an AI agent doing continuous, unattended
automation on service errors (`system_error`) and manual issues (`user_report`)?** Run immediately
after the agent-native layer merged (PR #17, N1–N6).

Method: 5 parallel audit lenses (discovery/event-coverage, work-loop lifecycle, operational
continuity, multi-agent concurrency, provisioning/auth) produced 22 raw findings; every finding was
then independently, adversarially verified against the cited code by a separate reviewer instructed
to refute it. **15 confirmed, 7 refuted.** Severities were recalibrated by the verifier
(blocker = continuous automation impossible/silently broken; major = degrades or eventually halts;
minor = friction). Findings are numbered A01–A15, majors first.

**Bottom line: no absolute blocker, but A01+A02 combine into a near-blocker for the primary use
case** — brand-new service errors produce no events, and the fallback issue list cannot express
"new since X", so event-driven discovery of new service errors does not work and poll-and-diff
degrades unboundedly with volume.

## Recommended remediation order (proposed N7)

1. **A01/A06** — consume `IssueOutcomeNew` in `StoreEvent` and write a `created` `issue_activity`
   row (the outcome is already computed and discarded at the exact right spot; the N6 spec gates
   will force the event-enum/OpenAPI update automatically).
2. **A02** — add `since` (firstSeen) + `limit` cursor pagination to `GET /api/agent/issues`.
3. **A03** — add `claimed_at`; stale-claim auto-release (reaper next to the retention cron sweeps,
   threshold configurable, e.g. default 24h without a progress/comment heartbeat).
4. **A04** — exclude `user_report` issues (or any claimed/unresolved issue) from occurrence-based
   retention deletion.
5. **A05** — optional `Idempotency-Key` header on agent mutations (dedupe table with TTL).

## Refuted findings (for the record)

Seven findings did not survive adversarial verification — the verifier found the behavior already
mitigated, documented, or the scenario unable to occur:

- Repeat (non-regressed) occurrences on an existing issue produce no event
- Agent can reopen resolved issues / no status state-machine guard
- Regression retains the claim but the assignee is not distinctly re-notified
- No whoami/identity-echo endpoint (agent id is discoverable from webhook payloads and its own
  writes' `actorId`)
- No documented warning about self-triggered feedback loops
- In-process rate limiter incorrect under multi-instance dashboard deployment
- No self-service key rotation / expiry notification

Refuted ≠ nonexistent: several were downgraded because a workaround exists, not because the
underlying limitation is false. Re-read the verifier notes in the workflow journal before citing
one of these as "fine".

---

## Confirmed findings

### A01 — New system_error issue creation (first occurrence) writes zero issue_activity rows

**Severity: major** (lens: discovery)

**Evidence:** apps/processor-go/store/store.go:397-430 (StoreEvent's non-regressed branch, which covers BOTH `wasNewIssue` and repeat-occurrence cases) does an `INSERT INTO issues ... ON CONFLICT DO UPDATE` and nothing else — no `INSERT INTO issue_activity`. The only issue_activity write anywhere in StoreEvent/UpsertIssueWithOutcome is inside the `isRegressed` branch (store.go:390-396 and the duplicate at store.go:233-239 in UpsertIssueWithOutcome, whose own outcome value is documented as 'consumed by nobody', store.go:309-315). agent-events.ts's AGENT_EVENT_TYPES enum (apps/dashboard-web/src/lib/server/agent-events.ts:10-26) has no 'created'/'new_issue' event type at all, confirming no such event is ever intended to be emitted from this path.

**Failure scenario:** Service A starts throwing a brand-new error class for the first time. The processor upserts a new `issues` row and a new `error_occurrences` row via StoreEvent's else-branch, but writes no issue_activity row. A triage agent polling GET /api/agent/events (backed by issue_activity.seq) never sees any event for this issue. It will only ever discover the issue if it separately, blindly re-lists GET /api/agent/issues (which itself has no 'since' filter — see next finding) and diffs client-side. Pure event-driven continuous automation on new service errors is silently broken from day one of the agent-native layer.

**Verifier note:** Behavior confirmed: StoreEvent's else-branch (store.go:397-430) writes only `issues`, no `issue_activity`; the only issue_activity INSERTs are regression-only (store.go:234, 391); the events feed reads solely from issueActivity (events.ts:60-61), so new system_error issues (no seq) never surface in GET /api/agent/events, and webhooks share that source. Downgraded from blocker to major because the guide's triage recipe (SENTINEL_AGENT_GUIDE.md:132) documents GET /api/agent/issues?claimed=false, which queries the issues table directly (agent-work.ts:60, spans system_error, returns firstSeen) so new service errors ARE discoverable by polling+diff — automation is degraded, not impossible.

### A02 — GET /api/agent/issues has no since/createdAt filter or pagination, so it cannot substitute for the missing creation event

**Severity: major** (lens: discovery)

**Evidence:** apps/dashboard-web/src/lib/db/queries/agent-work.ts:14-64 (`ListAgentIssuesOptions`/`listAgentIssues`) only supports `type`, `claimed`, `projectId`, `waiting` filters and unconditionally `.orderBy(desc(issues.lastSeen))` (line 64) — no `since`/`firstSeen` filter, no `limit`/`offset`, no cursor of any kind. apps/dashboard-web/src/routes/api/agent/issues/+server.ts:9-47 exposes exactly those same query params and nothing else, so every call returns the org's ENTIRE matching issue set, ordered by lastSeen (which is bumped by any recurrence, not just creation).

**Failure scenario:** Given finding #1 (no creation event) and #2 (no recurrence event), an agent's only fallback for discovering new work is to fetch the full unfiltered issue list on every poll and diff it client-side against a previously seen id set — expensive, unbounded (no page size cap), and still ordered by lastSeen not firstSeen, so 'new since X' cannot even be expressed as a query. At any nontrivial issue volume this is not a viable substitute for the events feed, so end-to-end unattended discovery of new system_error issues has no working mechanism at all.

**Verifier note:** Verified: agent-work.ts:14-20/64 supports only type/claimed/projectId/waiting and always orders by desc(lastSeen) with no since/limit/cursor; +server.ts:12-49 exposes nothing more. Premise also holds — processor StoreEvent writes issue_activity ONLY on regression (store.go:390-393); the new-issue/recurrence branch (store.go:397-430) inserts no activity row, so new system_error issues never hit the events feed. But a working discovery path exists (poll issues?type=system_error&claimed=false, diff client-side; firstSeen is returned at agent-work.ts:53 so 'new since X' is filterable client-side). It returns correct/complete data and is not silently broken — it degrades at volume because responses are unbounded with no page cap. That is major (degrades/eventually halts), not blocker; the finding's "no working mechanism at all" overstates it.

### A03 — No claim TTL, reaper, or auto-expiry -- a crashed agent's claim is stuck forever except human force-release

**Severity: major** (lens: lifecycle)

**Evidence:** reports.ts:640-683 claimIssue sets assignedTo with no expiry/heartbeat field. releaseClaim (reports.ts:699-746) only clears a claim on an explicit call by the current claimant, or `force:true` which report-access.ts:20/115-117 gates to org owner/admin only via requireIssueAccessAnyType(...,'force-release'), reached only through the human route apps/dashboard-web/src/routes/api/issues/[issueId]/claim/+server.ts:63-72. Repo-wide grep for reaper/stale-claim/TTL logic (attachment-reaper.ts, cron/retention/+server.ts) shows reaping exists only for attachments and invitations, never for issue claims.

**Failure scenario:** In continuous unattended automation, if an agent process dies (crash, OOM, network partition) after calling issues.claim but before issues.status/claim.release, the issue is permanently locked to that agent -- assignedTo never clears. No other agent can claim it (409 ClaimConflictError), and there is no scheduled job to detect or expire it; only a human owner/admin manually calling force-release via the dashboard UI can free it. For a system meant to run without a human in the loop, this silently halts triage on any issue an agent was working when it died.

**Verifier note:** Confirmed: claimIssue (queries/reports.ts:648) sets assignee_type/assigned_to with no timestamp; issues schema (schema.ts:86-87) has no claimed_at/expiry column; re-claim blocked by WHERE assigned_to IS NULL (line 649, 409); agents cannot force (agent-ops.ts:110-112 passes no force; force-release gated owner/admin at report-access.ts:117-119); retention cron reaps only invitations/attachments/occurrences, never claims. Recalibrated to major: mechanism is real but only leaks one stuck issue per agent-crash-during-work needing human force-release — degrades and requires periodic human recovery rather than making automation impossible.

### A04 — Retention job silently deletes manual (user_report) issues an agent may still be working, because they never have error_occurrences rows

**Severity: major** (lens: operations)

**Evidence:** apps/dashboard-web/src/lib/db/queries/reports.ts:145-155 createManualIssue inserts only into `issues` (issueType='user_report') and `manual_issue_reports` — it never inserts an `errorOccurrences` row. apps/dashboard-web/src/lib/server/retention.ts:70-76 orphanCondition deletes any issue with zero rows in error_occurrences whose `firstSeen < cutoffDate` (default 30 days, apps/dashboard-web/src/routes/api/cron/retention/+server.ts:29 `DATA_RETENTION_DAYS ?? '30'`), with NO status/claimed check. schema.ts:113 `issueActivity.issueId` and issueComments.issueId (schema.ts:155) are `onDelete: 'cascade'`, so the delete also wipes all activity/comments/attachments for that issue.

**Failure scenario:** An agent claims a user_report issue, posts a `question_asked` comment, and sets status to waiting_on_user (P9-4/D-style flow). If the human doesn't respond within 30 days, the next retention cron run (POST /api/cron/retention) deletes the issue, its manual_issue_reports row, every issue_activity/issue_comments row, and any attachments — with no age check on last activity, no exclusion for open/claimed/in_progress issues, and no way for the agent to detect this beyond the issue vanishing from GET /api/agent/issues/{id} (404) and its events disappearing from the seq feed. This applies to EVERY manual issue older than the retention window regardless of state, not just genuinely orphaned/stale ones.

**Verifier note:** Confirmed: createManualIssue (reports.ts:145-155) writes no error_occurrences row, and retention.ts:70-76 deletes any occurrence-less issue with firstSeen<cutoff and NO type/status/claim guard; errorOccurrences.issueId is notNull (schema.ts:242) so the NOT IN genuinely matches manual issues, and cascades (schema.ts:113,141,155) wipe report+activity+comments. firstSeen is creation-time and never updated (schema.ts:94), so it deletes EVERY manual issue past the window regardless of activity, not just idle ones. Downgraded to major because retention is operator-opt-in (external cron + CRON_SECRET, DEPLOYMENT.md:193; nothing in-app schedules it) and the in-window loop works — it silently destroys the long-lived/waiting-on-user manual-issue subset rather than making continuous automation categorically impossible.

### A05 — No idempotency-key mechanism on any agent mutation endpoint — comment/status/progress/progress-question retries duplicate side effects

**Severity: major** (lens: concurrency)

**Evidence:** grep for 'idempotency-key' across apps/dashboard-web/src and docs/agents returns zero matches. createComment (apps/dashboard-web/src/lib/db/queries/comments.ts:56-198) has no dedup key and unconditionally inserts a new issueComments row + fires notifyIssueEvent (line 189) on every call. updateIssueStatus (apps/dashboard-web/src/lib/db/queries/issues.ts:32-86) never compares the new status to existing.status before writing — it always inserts a fresh issue_activity row (line 72) and fires notification logic regardless of whether the status actually changed. recordAgentProgress (apps/dashboard-web/src/lib/db/queries/agent-work.ts:78-102) is the same: unconditional insert every call.

**Failure scenario:** An unattended agent posts PATCH .../status {status:'resolved'} or POST .../comments {body_md:'Investigating.'}, the HTTP response is lost to a network blip/timeout, and the agent's retry logic (standard for continuous automation) resends the identical request. The server has no way to recognize it as a retry: it inserts a second, duplicate comment (visible to humans and emailed to subscribers per the guide's own table in §7), or writes a duplicate 'status_changed' activity row and re-fires the resolved-notification email — spamming subscribers and polluting the audit timeline every time a retry occurs, which is expected behavior for any at-least-once automation loop.

**Verifier note:** Confirmed: no server-side idempotency exists for agent mutations (grep finds only receiver-side webhook dedupe SENTINEL_AGENT_GUIDE.md:300,328 and the seq cursor). createComment unconditionally inserts+notifies (comments.ts:109-119,189); updateIssueStatus never compares status to existing.status and always writes activity+notifies (issues.ts:40-79); recordAgentProgress inserts every call (agent-work.ts:84-90). Retries duplicate comment rows, issue_activity rows, and in-app notifications. One nuance: the 15-min email throttle (notify.ts:140-155) suppresses duplicate emails for commented/status_changed/resolved, but NOT the visible duplicate rows/in-app notifications, and question_asked retries bypass the throttle entirely (notify.ts:123,207,227). Major (degrades data/audit quality of an unattended loop) rather than blocker (loop still functions).

### A06 — IssueOutcome (New/Regressed/Existing) is computed by StoreEvent but discarded by every caller, foreclosing a cheap fix

**Severity: minor** (lens: discovery)

**Evidence:** store.go:309-315's own comment: 'The IssueOutcome return is BEST-EFFORT and currently consumed by nobody — every call site discards it (verified at ship time).' The value that could trivially drive an `issue_activity` 'created' insert on `IssueOutcomeNew` already exists in the function and is thrown away by its callers.

**Failure scenario:** Not a runtime failure by itself, but confirms the gap in finding #1 is not a fundamental data-availability problem — the processor already knows 'this was a brand-new issue' at the exact point it would need to write the missing activity row, it just doesn't use that information for anything today.

**Verifier note:** Confirmed: store.go:309-315 documents the outcome is consumed by nobody, and the sole production caller processor_service.go:245 discards it (`_, storeResult, err := s.store.StoreEvent(...)`); all test callers discard it too. IssueOutcomeNew is live at store.go:478, and the transaction already writes an issue_activity 'regressed' row (store.go:390-394), proving a 'created' insert on the New path would be co-located and trivial. Discovery-lens note, correctly minor (the value is even documented as inexact under races).

### A07 — No 'in_progress' status exists; claim does not change issue status

**Severity: minor** (lens: lifecycle)

**Evidence:** agent-ops.ts:50 VALID_STATUSES = ['unresolved','resolved','ignored'] (no in_progress). issuesClaim (agent-ops.ts:88-104) calls claimIssue (reports.ts:640-683) which only sets assigneeType/assignedTo -- it never touches issues.status. DB-level check_status constraint (packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql:53) enforces the same 3-value enum, so an in_progress status cannot even be added without a migration.

**Failure scenario:** A human viewing the issue list/detail sees status='unresolved' for both an untouched issue and one an agent has claimed and is actively working. Distinguishing 'agent working on it' from 'nobody has looked at it' requires cross-referencing assignedTo plus the issue_activity timeline (claimed/progress_update rows) rather than the primary status field -- degrades operator trust/visibility during continuous unattended automation, though it does not block the loop mechanically.

**Verifier note:** Facts hold: agent-ops.ts:50 VALID_STATUSES has no in_progress; claimIssue (reports.ts:645-650) sets only assigneeType/assignedTo, never issues.status; migration line 53 check_status enforces the 3-value enum. But it neither blocks nor degrades the agent loop (finding concedes this), and the visibility gap is largely mitigated: assignedTo/assigneeType are first-class issue columns returned in issue detail (reports.ts:648), so a claimed issue is distinguishable from an untouched one by assignment alone — status/assignment separation is standard lifecycle design, so this is human-side friction, not major.

### A08 — Agents cannot edit or delete their own comments

**Severity: minor** (lens: lifecycle)

**Evidence:** Only one agent comment route exists: apps/dashboard-web/src/routes/api/agent/issues/[issueId]/comments/+server.ts, exposing GET (list) and POST (agentOpRoute('issues.comment') -> agent-ops.ts:127-172). No /api/agent/issues/[issueId]/comments/[commentId] route exists for agents (the only [commentId] route under apps/dashboard-web/src/routes/api/issues/[issueId]/comments/[commentId] is session/human-authenticated, not under /api/agent).

**Failure scenario:** An agent that posts a comment with a typo, stale info, or an outdated status summary has no way to correct or retract it -- it can only post a follow-up comment. Friction only, not automation-blocking.

**Verifier note:** No [commentId] route exists under /api/agent; the only edit/delete route (apps/dashboard-web/src/routes/api/issues/[issueId]/comments/[commentId]/+server.ts) requires a browser session (locals.auth() -> 401 for Bearer agents, lines 25-28/67-70) AND gates edit/delete on authorType==='user' (lines 39, 81), while agent comments are stored with authorType:'agent' (agent-ops.ts:145). So an agent cannot correct/retract its own comment, only post a follow-up via issuesComment -- friction, not automation-blocking.

### A09 — Agents cannot set severity on manual (user_report) issues

**Severity: minor** (lens: lifecycle)

**Evidence:** No write path for severity under the agent API surface: grep of apps/dashboard-web/src/routes/api/agent and agent-ops.ts/agent-route.ts for 'severity' only matches read-only test fixtures (agent-issue-detail.test.ts:81,88 asserting a GET response echoes severity). agentOps registry (agent-ops.ts:292-300) has no severity-related op.

**Failure scenario:** Triage automation cannot re-prioritize a manual issue's severity based on investigation findings (e.g. escalate a user_report from 'low' to 'critical' after reproducing real impact) -- it must ask a human to do it or work around it via a comment, degrading autonomous triage but not blocking the core claim/work/resolve loop.

**Verifier note:** Confirmed: agentOps registry (agent-ops.ts:292-300) has no severity op, and updateManualIssueReport (reports.ts:518, edits severity per R11) is imported only by the human route routes/api/organizations/[orgId]/reports/[issueId]/+server.ts, never under routes/api/agent/; agent schema treats severity as read-only (agent-api-spec/schemas.ts:87). Minor is correct — the claim/work/comment/status/resolve loop is intact and escalation can still be voiced via comment/progress, so it is friction, not a halt.

### A10 — Rate limiter is fixed-window (reset-based), not sliding, allowing bursty double-rate around window boundaries

**Severity: minor** (lens: operations)

**Evidence:** apps/dashboard-web/src/lib/rate-limit.ts:40-60 `checkRateLimitWithLimit`: on window expiry it resets `count:1, resetAt: now+windowMs`; there is no rolling/sliding accounting.

**Failure scenario:** An agent (or its polling+work-call combination) can send `rate_limit_rpm` requests in the last second of one 60s window and another `rate_limit_rpm` in the first second of the next window, achieving up to 2x the nominal rpm briefly. Not a continuity blocker (default rate_limit_rpm=5000 per apikeys.ts:114 is generous relative to a poll-every-few-seconds + occasional work-call pattern), but it means the documented per-key rpm is not a hard ceiling.

**Verifier note:** Confirmed: checkRateLimitWithLimit (rate-limit.ts:48-59) resets {count:1, resetAt:now+windowMs} on expiry with no sliding accounting — a fixed window that permits ~2x nominal rpm across a boundary; wired at agent-auth.ts:124 with default rate_limit_rpm=5000 (migration 1722000000_add_api_key_management.sql:17). Behavior is real and unmitigated. Minor is correct: the flaw makes the limiter more permissive, so it never blocks/degrades the agent's own automation — it only weakens the operator-facing rpm ceiling.

### A11 — Claim is advisory only — mutations never verify the caller holds it, so two agents can work the same issue concurrently

**Severity: minor** (lens: concurrency)

**Evidence:** apps/dashboard-web/src/lib/server/agent-ops.ts: issuesStatus (line 57), issuesComment (127), issuesProgress (174), issuesRelationsAdd/Remove (205/255) all call resolveAgentIssueScope(issueId, ctx.organizationId) (apps/dashboard-web/src/lib/server/agent-issue-scope.ts:25-49), which returns assignedTo/assigneeType on the scope object but NONE of the handlers ever read or check it against ctx.agentId. Same gap in the separate questions route: apps/dashboard-web/src/routes/api/agent/issues/[issueId]/questions/+server.ts:20-46 calls createComment with no ownership check. Only claimIssue/releaseClaim (apps/dashboard-web/src/lib/db/queries/reports.ts:640-746) enforce anything atomically, and only for the claim/release operation itself. The agent guide (docs/agents/SENTINEL_AGENT_GUIDE.md:113) even calls this 'etiquette': 'Claiming is how you signal I'm working on this and stops other agents from double-handling it' — but nothing in the code actually stops it.

**Failure scenario:** Agent A claims issue X (succeeds, 200). Agent B never sees the claim (didn't re-poll, or claim already occurred before B's crash-loop retries a stale view) and calls PATCH /api/agent/issues/X/status, POST .../comments, POST .../progress, or POST .../relations directly — all succeed with 200, exactly as if B held the claim. Two agents can post conflicting comments, set conflicting statuses, or add conflicting relations on the same issue with zero server-side conflict signal. Because these ops are also reachable individually or via /api/agent/batch without ever calling issues.claim first, an unattended automation loop that (incorrectly, or due to a bug) skips the claim step is not blocked from mutating issues it does not own — the isolation the whole claim/release design exists to provide is not actually enforced outside the claim endpoint itself.

**Verifier note:** Code state is accurate: none of the mutation handlers (agent-ops.ts:57,127,174,205,255; questions/+server.ts:20) compare resolveAgentIssueScope's assignedTo (agent-issue-scope.ts:32) against ctx.agentId. But severity is overstated — claimIssue is an atomic UPDATE...WHERE assigned_to IS NULL that returns 409 on a lost race (reports.ts:649-653, agent-ops.ts:99-100), so a protocol-following agent gets an authoritative, race-safe coordination signal and continuous automation works fine; double-work only arises if an agent skips claim or ignores the 409, which the finding itself concedes. Missing ownership re-check on mutations is a defense-in-depth gap (and comment/status by any org agent mirrors human collaborators), not a blocker.

### A12 — Relation cycles (A caused_by B, B caused_by A) are not prevented at any layer

**Severity: minor** (lens: concurrency)

**Evidence:** packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql:68-77 defines issue_relations with only UNIQUE(source_issue_id, target_issue_id, relation_type) — no cross-row cycle guard. packages/db-migrations/migrations/1722400000_add_issue_relations_no_self_check.sql adds only a self-relation CHECK (source_issue_id <> target_issue_id). apps/dashboard-web/src/lib/server/agent-ops.ts:205-253 (issuesRelationsAdd) checks self-relation (line 217) and catches unique-violation (409) / check-violation (400) but performs no query for the REVERSE relation before inserting, and createIssueRelation (apps/dashboard-web/src/lib/db/queries/issues.ts:233-271) is a plain insert with no reverse-direction lookup either.

**Failure scenario:** Agent (or two agents racing) creates issue A 'caused_by' B, then independently creates issue B 'caused_by' A — both inserts succeed (different source/target ordering means the UNIQUE constraint never fires), producing a causal cycle in the relations graph. Any downstream tooling that walks caused_by edges to find a root cause (a very likely thing for triage automation to do) can loop forever or produce nonsensical root-cause chains, and nothing in the API or DB will reject or even flag it.

**Verifier note:** Confirmed real: caused_by has no cross-row cycle guard — UNIQUE(source,target,type) at 1721900000_...sql:77, only a self CHECK at 1722400000, and both createIssueRelation (issues.ts:241) and issuesRelationsAdd (agent-ops.ts:224) insert without a reverse lookup. But NOT "at any layer": the REST route guards duplicate_of 2-cycles (+server.ts:101-115) and explicitly declines caused_by as "directional-but-harmless" (:99-100). Severity is minor, not major: no shipped code recursively traverses relations (UI filters one hop, IssueRelations.svelte:164; no WITH RECURSIVE anywhere), so the "loops forever" harm requires hypothetical external tooling lacking a visited-set — the triage/claim/work/update/follow-up loop is untouched by relation cycles.

### A13 — Release-claim retry cannot be told apart from a genuine conflict — a plain network retry after a successful release surfaces as the same 409 ClaimConflictError as someone else grabbing the issue

**Severity: minor** (lens: concurrency)

**Evidence:** apps/dashboard-web/src/lib/db/queries/reports.ts:699-746 (releaseClaim): the non-force path is `WHERE id = issueId AND assigned_to = actorId AND assignee_type = actorType`; 0 rows updated always throws `ClaimConflictError('Issue is not claimed by this actor')` (line 722) regardless of WHY zero rows matched — already-released-by-self (safe to treat as success) and never-held/already-reassigned-to-someone-else (a real problem) both produce the identical 409/message.

**Failure scenario:** Agent calls DELETE .../claim, the response is dropped by the network, agent retries per its standard retry policy. First call actually succeeded and cleared assigned_to. Second call gets 409 'Issue is not claimed by this actor' — indistinguishable in the response from a case where the release never happened and someone else now holds the issue. An unattended agent that treats any 409 on release as 'stop, something's wrong' will unnecessarily halt or alert on a no-op retry of a call that already succeeded.

**Verifier note:** Confirmed: reports.ts:711-713 scopes the non-force UPDATE to assigned_to=actorId AND assignee_type=actorType, and :721-723 throws a single static ClaimConflictError('Issue is not claimed by this actor') for any zero-row result; agent-ops.ts:119-124 maps it to an undifferentiated 409, so a retry-after-successful-release is wire-indistinguishable from a genuine conflict. Minor is correct — an agent can disambiguate out-of-band via issue detail / events feed (assigned_to==null), and release sits at the tail of a work cycle, so it is recoverable friction, not a halt.

### A14 — No API-scripted path to provision an agent identity/key — requires an interactive human session

**Severity: minor** (lens: dx-auth)

**Evidence:** apps/dashboard-web/src/routes/api/organizations/[orgId]/agents/+server.ts:14-31 and :33-66 (POST creates agent) both start with `const session = await locals.auth(); if (!session?.user?.id) throw error(401)` and then `requireOrgMembership` + `hasPermission(membership.role, 'manage_agents')`. Auth.js is configured (apps/dashboard-web/src/lib/server/auth-config.ts:2-3,23,35,45) with only `Google` (OAuth) and `Email` (magic-link) providers — no Credentials/password provider exists anywhere in the codebase (grep for 'Provider|Credentials' in auth-config.ts returns only Google/Email). Both providers require a browser round-trip (OAuth consent screen, or clicking a link delivered to an inbox); there is no way to mint an Auth.js session from a script.

**Failure scenario:** An operator wants to stand up a fleet of N agents across M orgs purely from a script/Terraform/CI job. There is no endpoint under /api/agent/* or elsewhere that a bearer-key-only or headless credential can call to create an agent or an agent API key — every such endpoint sits behind `locals.auth()` session cookies from Google OAuth or email magic-link. Each new agent, for each new org, requires a human to click through a browser flow once. This blocks fully unattended fleet provisioning/rotation (e.g. automatically registering a new triage bot when a new org signs up), though it does not affect an already-provisioned agent's steady-state operation.

**Verifier note:** Behavior holds: agent creation (agents/+server.ts:14-18,33-37) and agent-key creation (keys/+server.ts:11-13,30-33) both require locals.auth()+requireOrgMembership, and auth-config.ts:2-3,19-52 wires only Google OAuth + Email magic-link (no Credentials provider), so no session is mintable headlessly — confirmed by SENTINEL_AGENT_GUIDE.md:121,261. But this is one-time bootstrap friction, not a steady-state defect: createApiKey (apikeys.ts:105-119) sets no expiresAt, so agent keys never expire and the continuous triage/claim/work loop never degrades or halts once provisioned. Downgraded major→minor.

### A15 — Attachment upload has no issue association at creation time — must be linked via a subsequent comment, and the CLI's issueId argument is a documented no-op

**Severity: minor** (lens: dx-auth)

**Evidence:** apps/dashboard-web/src/routes/api/agent/uploads/+server.ts:11-34: POST /api/agent/uploads takes only `organizationId` (from ctx, B7) and multipart form data — no issueId parameter at all; the resulting attachment is unassociated until later passed as `attachment_ids` to POST /api/agent/issues/:id/comments (agent-ops.ts:134-141). docs/agents/SENTINEL_AGENT_GUIDE.md:400-402 documents this explicitly: 'the server's POST /api/agent/uploads does not take an issueId at all ... The CLI's <issueId> argument is never sent to the server.'

**Failure scenario:** An agent (or a developer copying the CLI's `sentinel upload <issueId> <file>` signature literally) can reasonably but wrongly assume the upload is scoped/attached to the issue at upload time; if the agent crashes or the process restarts between upload and the follow-up comment call, the attachment is silently orphaned (uploaded, never linked, invisible on the issue) with no cleanup or listing endpoint shown in the reviewed routes to recover orphaned attachment ids. This is friction/degradation for an unattended loop doing multi-step upload+comment sequences, not a hard blocker since the documented two-step flow does work.

**Verifier note:** Core claims verified: uploads/+server.ts:11-26 accepts no issueId and inserts issueId:null/commentId:null (upload-core.ts:80-81); linking is a separate step via attachment_ids on comments (agent-ops.ts:134-141); CLI issueId is a documented no-op (SENTINEL_AGENT_GUIDE.md:400-402). The scenario's 'no cleanup' claim is partly wrong — attachment-reaper.ts GCs orphans (>24h, issueId+commentId NULL) opportunistically and via cron, so orphans are bounded, not a permanent leak; there is genuinely no re-link/listing recovery path, but retry just re-uploads. Friction only — minor is correct.
