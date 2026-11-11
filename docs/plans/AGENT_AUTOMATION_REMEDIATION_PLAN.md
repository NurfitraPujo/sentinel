# N7 — Agent-Automation Audit Remediation Plan (A01–A15 + R1, R2)

Source: docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md. All paths relative to repo root
/home/fitrapujo/oss/sentinel/.claude/worktrees/hungry-goldwasser-0f76ed.

Recurring "event-type chain" (repeat in every phase adding an event type):
1. New migration widening the issue_activity CHECK (idempotent DO-block, drop+re-add constraint)
2. AGENT_EVENT_TYPES in apps/dashboard-web/src/lib/server/agent-events.ts
3. agent-api-spec/schemas.ts enum + registry if routes change
4. `pnpm openapi:agent` regenerate docs/agents/openapi.agent.yaml (openapi-drift.test.ts gates)

Recurring "route chain" for any new/changed /api/agent route:
registry.ts + schemas.ts + regenerated YAML + completeness/contract tests + CLI (tools/sentinel-cli)
+ SENTINEL_AGENT_GUIDE.md + sentinel-agent skill.

Migration numbers reserved: 1723400000, 1723500000, 1723600000, 1723700000.

---

## Phase N7a — Discovery: creation + occurrence events (A01, A06, R2)  [highest leverage]

**Migration 1723400000_widen_event_types_created_occurrence.sql**
- DO-block: drop+recreate issue_activity check_event_type adding 'created' and 'occurrence_burst'.

**Processor (Go), apps/processor-go/store/store.go**
- In StoreEvent's else-branch (~line 397-430), after the upsert RETURNING id:
  - If `wasNewIssue` (already computed at ~line 341): INSERT issue_activity
    (actor_type 'system', actor_id 'sentinel-processor', event_type 'created', new_value = small
    JSON {errorClass, projectId}) inside the SAME tx, mirroring the regressed insert at ~line 390.
    Note the outcome-inexactness caveat (store.go:309-315): under a first-insert race two deliveries
    can both see wasNewIssue — but wasNewIssue here is the pre-upsert SELECT in the same tx under
    READ COMMITTED; a duplicate 'created' row is possible only in a genuine race. Accept and
    document (consumers key on issue id; a rare duplicate 'created' is harmless), OR use xmax=0 on
    the RETURNING row for exactness. Recommend: RETURNING (xmax = 0) AS inserted — exact and cheap.
  - R2 occurrence events, non-spammy design (recommended): **count-threshold + throttle hybrid**.
    In the repeat-occurrence path (not new, not regressed), emit ONE 'occurrence_burst'
    issue_activity row when BOTH hold: (a) no prior 'occurrence_burst'/'created' row for this issue
    within OCCURRENCE_EVENT_MIN_INTERVAL (default 1h, env on processor), and (b) issues.count has
    crossed a power-of-ten-ish threshold ladder (10, 100, 1000, ...) OR the interval elapsed with
    any new occurrences. Simplest correct SQL: single INSERT ... SELECT guarded by
    NOT EXISTS (activity row for issue with event_type IN ('created','occurrence_burst','regressed')
    AND created_at > now() - interval) — one extra statement, same tx, same single-issue-row lock
    discipline (no new contended locks; the NOT EXISTS reads only). new_value JSON: {count, lastSeen}.
    Anti-spam bound: max 1 event per issue per interval, worst-case org-wide event volume =
    active_issues / interval — bounded and tunable.
- Do NOT touch the 2s lag guard (consumer-side, agent-events.ts:37) — nothing to change.
- Backfill for existing issues: **NO**. Document in the migration header and guide: agents
  bootstrap via GET /api/agent/issues (N7b makes that viable); synthesizing 'created' rows would
  produce a seq stampede and misdated events.

**Dashboard chain:** agent-events.ts adds 'created', 'occurrence_burst'; schemas.ts event enum;
regenerate YAML; SENTINEL_AGENT_GUIDE.md discovery section rewritten ("watch for 'created'").
Webhooks (1723300000 wiring) share the issue_activity source, so they get the new types free —
verify agent-webhook event-type filter (if any allowlist exists) includes them.

**Tests**
- Go: store_test — new issue writes exactly one 'created' row in-tx (kills A01/A06); duplicate
  delivery writes none; regression writes 'regressed' not 'created'; burst thresholds/throttle
  (kills R2 gap); rollback on occurrence-dupe also rolls back activity rows.
- TS: events feed returns 'created'/'occurrence_burst' (contract + enum completeness tests catch
  drift automatically).

**Risks:** extra statements inside the hot StoreEvent tx (read D16 comments first; NOT EXISTS is a
read, no added lock waits). Duplicate 'created' under race if xmax approach skipped.

---

## Phase N7b — Discovery: issue-list since/sort/pagination (A02)

**Files:** apps/dashboard-web/src/lib/db/queries/agent-work.ts,
apps/dashboard-web/src/routes/api/agent/issues/+server.ts, registry.ts/schemas.ts/YAML,
tools/sentinel-cli (list flags), guide.

- New params: `since` (ISO, filters firstSeen >= since), `sort=firstSeen|lastSeen` (default
  lastSeen = current behavior), `limit` (default 50, max 200), keyset cursor `before`
  (recommended: keyset on (sortColumn, id) — stable under inserts, matches the events feed's
  cursor philosophy; offset pagination drifts as rows churn). Response gains `nextCursor` ONLY
  when limit was supplied; **params absent ⇒ byte-identical current behavior** (no limit applied)
  for backward compat.
- CLI: `sentinel issues list --since --sort --limit --cursor`.

**Tests:** query unit tests (since boundary, keyset stability, cap at 200, default unchanged) —
kills A02; contract/completeness/drift auto-gate spec.

**Risks:** none material; watch that unlimited default stays documented as legacy.

---

## Phase N7c — Lifecycle safety: claimed_at + stale-claim reaper (A03), retention guards (A04)

**Migration 1723500000_add_claimed_at.sql** — ALTER TABLE issues ADD COLUMN IF NOT EXISTS
claimed_at timestamptz; (idempotent; no backfill needed — reaper treats claims with NULL
claimed_at as stale-eligible only via the activity check, see below).

**A03 files:** schema.ts (issues.claimedAt), reports.ts claimIssue sets claimed_at=now()
(and releaseClaim/force clears it), src/lib/server/retention.ts gains `reapStaleClaims()`,
cron route calls it; agent-events already has 'claim_released' (no enum change).
- Reaper logic: force-release claims where assignee_type='agent' (agent-only; human claims stay
  manual) AND the claim is old enough (claimed_at < now() - N hours, OR claimed_at IS NULL for
  pre-migration claims) AND the claimant has written no issue_activity row on that issue
  (actor_id = assigned_to) within the last N hours. The claimed_at age check prevents reaping a
  freshly made claim whose agent simply hasn't produced activity yet; N = env CLAIM_STALE_HOURS,
  default 24. Writes issue_activity 'claim_released' with actor_type 'system',
  actor_id 'sentinel-claim-reaper', new_value JSON {previousAssignee, reason:'stale'} — visible in
  the agent feed (closes the loop: agents see their claim was reaped).
  D18: no early-return inside the transaction.

**A04 files:** retention.ts, cron route, docker-compose*.yml, .env.example, DEPLOYMENT.md.
- New env MANUAL_ISSUE_RETENTION_DAYS default 365 → second cutoff for the occurrence-less delete.
- orphanCondition gains guards: AND status = 'resolved' OR 'ignored' (never delete 'unresolved')
  AND assigned_to IS NULL (never delete claimed), AND firstSeen < manualCutoff. RetentionResult
  gains manualRetentionDays.

**Tests:** integration test in retention/agent-work suites: (1) stale claim with old activity is
force-released + 'claim_released' system event exists (kills A03); (2) active claimant's recent
progress_update protects the claim; (3) unresolved/claimed occurrence-less issue survives any age,
resolved+unclaimed manual issue deleted only past 365d (kills A04).

**Risks:** reaper vs in-flight agent race (agent writes status right after reap → succeeds
unclaimed; acceptable, advisory model). Clock-based tests need injected now/cutoff.

---

## Phase N7d — Mutation robustness: idempotency (A05), release idempotence (A13), reverse-relation guard (A12)

**A05 (natural guards only):**
- updateIssueStatus (apps/dashboard-web/src/lib/db/queries/issues.ts:32-86): read current status
  first (inside its tx); if unchanged AND resolved_in_version unchanged → return no-op marker; no
  activity insert, no notify, and agent-ops.ts issuesStatus skips notification emails + returns
  200 {success:true, status, changed:false} with current row. Both single route and batch share
  the registry handler so parity is automatic. Document no-op semantics in schemas.ts description.
- createComment (comments.ts) + recordAgentProgress (agent-work.ts): dedupe window — before
  insert, SELECT a row with same issueId+authorType+authorId+body created within last 2 min
  (const AGENT_DEDUPE_WINDOW_MS in $lib/server, B12); if found return existing row, skip
  insert/notify. Apply in the query layer so human routes get it too (verify human comment UX
  unaffected — legit rapid identical comments within 2min are vanishingly rare; document).
- Relations already UNIQUE (409) — no change.

**A13:** releaseClaim (reports.ts:699-746): on 0 rows updated, re-read the issue; if
assigned_to IS NULL → idempotent success (return current row, 200, no activity/notify); only if
assigned to someone else (or another type) throw ClaimConflictError. Check call sites: agent-ops
issuesClaimRelease and human claim route both flow through releaseClaim — behavior change is
uniform; update guide + spec response description.

**A12:** createIssueRelation (issues.ts:233-271) and/or issuesRelationsAdd: before insert of
'caused_by' (and mirror the human route's duplicate_of pair guard at
routes/api/issues/[issueId]/relations/+server.ts:101-115), SELECT the reverse pair
(target→source, same type); if exists → 409 'Reverse relation already exists (would create a
cycle)'. Put in the query layer so both human route and agent op are covered. 2-cycle only (matches
existing duplicate_of precedent); full graph-cycle detection out of scope, documented.

**Tests:** status retry writes zero new activity rows + no email call (kills A05-status); identical
comment/progress within window returns same row id (kills A05-comment/progress); release-after-
release returns 200 (kills A13) while release of other-agent claim still 409; A→B then B→A
caused_by rejected 409 both via agent op and human route (kills A12). Check existing e2e (79+/0)
for tests asserting the old 409-on-double-release; update expectations.

**Risks:** dedupe window may swallow an intentional identical repost (documented, 2min is short);
status no-op changes response shape slightly (additive `changed` field — non-breaking).

---

## Phase N7e — Agent capability gaps: comment edit/delete (A08), severity op (A09), visibility (A07)

**A08:** new route apps/dashboard-web/src/routes/api/agent/issues/[issueId]/comments/[commentId]/
+server.ts with PATCH+DELETE via two new registry ops 'comments.edit'/'comments.delete'
(agent-ops.ts handlers): resolveAgentIssueScope, then ownership gate authorType='agent' AND
authorId=ctx.agentId (mirror api/issues/[issueId]/comments/[commentId]/+server.ts:39,81), reuse
updateComment/deleteComment from queries/comments.ts. Route chain + CLI
(`sentinel comment edit|delete`) + guide. Decide batch inclusion: yes, they fit the mutations-only
registry.
**A09:** new op 'issues.report.severity' → route .../report/severity or fold into a PATCH; reuses
updateManualIssueReport (reports.ts:518) — verify it writes 'report_edited' activity (existing
enum, no migration). 400 when issue.issueType !== 'user_report'. Route chain + CLI flag.
**A07 (no new status — per decision):** UI-only. agent list/detail responses already return
assigneeType/assignedTo/claimedAt (add claimedAt to agent-work.ts select + issue detail). Dashboard
issue list badge "agent working" when assigneeType='agent' — confirm in
routes/(app)/.../issues +page.svelte list components; claimedAt tooltip. No migration, no CHECK
change.

**Tests:** agent edits/deletes own comment 200; other agent's comment 403; human comment 403
(kills A08). Severity op updates manual issue + 400 on system_error (kills A09). Svelte component
test or e2e assertion for the badge (kills A07).

**Risks:** comment edit ops widen batch surface — audit actions ('agent.comment.edited' etc.) must
be added to agent-audit vocabulary if it validates actions.

---

## Phase N7f — Auth/ops: self-rotation + whoami (R1), rate-limit docs (A10), provisioning (A14), upload UX (A15)

**R1:** agent keys live in project_api_keys (agent-auth.ts:72 reads expiresAt; enforced :92).
- GET /api/agent/self: returns {agentId, name, organizationId, key:{id, expiresAt, lastUsedAt}} —
  closes refuted whoami gap. New route + chain.
- POST /api/agent/key/rotate: create new key row (reuse createApiKey), set OLD key
  expires_at = now() + grace (env AGENT_KEY_ROTATION_GRACE_HOURS default 24), status stays
  'active' — expiry IS enforced (S7 fix), so dual-valid window is safe, unlike session rotateApiKey
  (apikeys.ts:146) which revokes immediately (keep that behavior for humans; add a separate
  rotateAgentKeyWithGrace in apikeys.ts). Return secret ONCE. Audit 'agent.key.rotated'. Rate-limit
  note: rotation inherits per-key rpm. Guide section "key hygiene" + CLI `sentinel key rotate`.
**A10 (keep fixed window — justified: single-instance deployment, generous 5000 rpm default,
limiter errs permissive so never blocks automation):** verify 429 path emits accurate Retry-After
from resetAt (rate-limit.ts / agent-auth.ts:124); add it if missing; document burst-2x behavior in
guide + DEPLOYMENT.md.
**A11 (decision: keep advisory, document loudly + enrich errors — chosen over 409-enforcement
because enforcement would break the existing e2e suite and the human-collaborator parity the
verifier noted; claim already gives a race-safe 409 signal to protocol-followers):** add
claimedBy/claimedAt to mutation success/error response context where scope already fetched
(agent-issue-scope returns assignedTo — surface it), and a prominent guide warning replacing the
"etiquette" phrasing. Optionally log a structured warning when a non-claimant agent mutates a
claimed issue (observability, no behavior change).
**A14 (defer with runbook — recommended):** full headless provisioning API is out of scope;
document a runbook in DEPLOYMENT.md: one-time human creation of agent + key per org via dashboard,
plus (optional small follow-up) org-owner-mintable one-time provisioning token design sketch filed
as a spec, not built now.
**A15 (CLI-only):** tools/sentinel-cli commands.go: remove/repurpose the no-op <issueId> arg;
`sentinel upload <file> --issue <id> --comment "text"` does upload → comment-with-attachment_ids in
one command; README + guide update.

**Tests:** rotate → old key valid until grace, new key works, secret shown once; /self contract
test (kills R1); Retry-After header assertion (kills A10 doc-gap); CLI upload+comment integration
(kills A15). e2e count stays 79+/0.

**Risks:** dual-valid grace means a leaked old key lives ≤24h post-rotation — document; grace env
lets operators set 0.

---

## Phase N7g — Documentation & bookkeeping close-out

- docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md: per-finding status table (Remediated in N7x /
  Accepted-with-docs (A10, A11, A14) / Deferred (A14 provisioning API)); note R1/R2 promoted from
  refuted list.
- VERIFIED_STATE.md + WORKLOG (wherever the repo keeps them — summaries/) entries per phase.
- SENTINEL_AGENT_GUIDE.md full pass: new discovery recipe (created/occurrence_burst events + since
  pagination), claim staleness contract, idempotency semantics, key rotation, upload one-shot.
- .claude sentinel-agent skill updated to match new ops/flags.
- Final `pnpm openapi:agent` + dashboard build+check+test + e2e 79+/0 + Go tests as the gate.

## Sequencing rationale
N7a/N7b first (discovery is the near-blocker per audit). N7c next (unattended-loop survival).
N7d (retry safety) before N7e (new surface) so new ops inherit the guards. N7f independent. N7g last.

### Critical Files for Implementation
- /home/fitrapujo/oss/sentinel/.claude/worktrees/hungry-goldwasser-0f76ed/apps/processor-go/store/store.go
- /home/fitrapujo/oss/sentinel/.claude/worktrees/hungry-goldwasser-0f76ed/apps/dashboard-web/src/lib/server/agent-ops.ts
- /home/fitrapujo/oss/sentinel/.claude/worktrees/hungry-goldwasser-0f76ed/apps/dashboard-web/src/lib/db/queries/agent-work.ts
- /home/fitrapujo/oss/sentinel/.claude/worktrees/hungry-goldwasser-0f76ed/apps/dashboard-web/src/lib/server/retention.ts
- /home/fitrapujo/oss/sentinel/.claude/worktrees/hungry-goldwasser-0f76ed/apps/dashboard-web/src/lib/server/agent-api-spec/registry.ts
