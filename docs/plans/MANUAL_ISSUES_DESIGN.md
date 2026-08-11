# Manual (User-Reported) Issues — Design

Status: **Agreed** (research 2026-08-11; all decisions grilled and confirmed with the
user 2026-08-11; nothing implemented yet)
Scope: user-created issues with their own dashboard, rich-text reports, attachments,
issue↔issue linking, claiming by users or AI agents, reporter notifications, a
Slack-like discussion thread per issue, an explicit activity/history system, and an
agent work-loop (pull → claim → work → progress updates → blocking questions →
resolve).

---

## 0. Decision register (grilled 2026-08-11, all confirmed)

| # | Decision |
|---|---|
| Q1 | Reporters are **org members only**; all agent↔human conversation happens in the **dashboard thread** (email is only a pointer back to the thread, never a reply channel). No public portal, no inbound-email parsing. |
| Q2 | A manual issue **is an `issues` row** (`issue_type='user_report'`) + 1:1 companion `manual_issue_reports`. AI duplicate-triage later via existing `relation_type='duplicate_of'`. |
| Q3 | **Markdown everywhere** (report bodies, thread messages, agent posts). Textarea + preview in v1; toolbar editor later, no data migration. |
| Q4 | **MinIO + proxied uploads**; images, documents, and short video, all under a **25 MB cap**; presigned large-file upload deferred. |
| Q5 | Agents are a **dedicated `agents` table** + a new `'agent'` scope on `project_api_keys`. **Every agent action is auditable** (`issue_activity` + `audit_logs`). |
| Q6 | Agents discover work by **pull with atomic claim** (`WHERE assigned_to IS NULL`, loser gets 409). Manual release; admin force-release; no auto-expiry timer in v1. NATS push is v2. |
| Q7 | **Participation-based subscriptions**; email only for `commented`/`claimed`/`status_changed`/`resolved` with a per-issue throttle; agent `progress_update`s are in-app only. |
| Q8 | Permission matrix in §9: any member (viewers included) can create reports and comment on readable issues; claim/resolve/link need `ISSUE_WRITE_ROLES`; agent management is owner/admin. |
| Q9 | **Strict separation**: error dashboard/search/alerting filter `issue_type='system_error'`; `/reports` shows only `user_report`. Linked-issues panel is the only bridge. Prevents noise both ways. |
| Q10 | **Polling in v1** (~10 s while visible) for threads + unread count; SSE (LISTEN/NOTIFY or NATS-fed) is a marked later phase behind the same client API. |
| Q11 | Explicit **`issues.waiting_on`** flag (`'reporter'|'team'`, NULL = not blocked) set by an agent's blocking question, auto-cleared on any human reply; "needs your input" filter; blocking questions bypass the email throttle. |
| Q12 | `project_id` stays **NOT NULL**; project picker is optional on the form — unassigned reports land in a lazily auto-provisioned per-org **"Triage" inbox project** (excluded from error dashboard + alerting); a write-role **"move to project"** action routes them; AI triage can later suggest/perform the move. |

## 1. What the codebase already gives us (research findings)

The schema **already anticipates this feature** — designed in
`1721900000_add_issue_lifecycle_and_relations.sql`, mirrored in
`apps/dashboard-web/src/lib/db/schema.ts`, unused by any dashboard query today:

| Need | Existing mechanism |
|---|---|
| Distinguish manual from service issues | `issues.issue_type` CHECK `('system_error','user_report')`; `issues.source_channel` CHECK `('ingestion_sdk','manual_support','api')` |
| Link a manual issue to N service issues | `issue_relations` (`relation_type IN ('linked_to','caused_by','duplicate_of')`, `created_by_type IN ('user','agent','system')`) + `IssueRelations.svelte` |
| Claim by user **or** AI agent | `issues.assignee_type` CHECK `('user','agent')` + `assigned_to`; `assignIssue()` in `src/lib/db/queries/issues.ts` already threads `actorType` |
| Timeline / audit per issue | `issue_activity` (`event_type` incl. `assigned`, `status_changed`, `linked`, `ai_analysis`; `actor_type` `user|agent|system`) |
| Compliance audit trail | `audit_logs` (action, resourceType, resourceId, actorId, metadata jsonb) |
| Status lifecycle | `issues.status` (`unresolved|resolved|ignored`), `regression_status` |
| AuthZ | `src/lib/server/issue-access.ts` (`requireIssueAccess`, `ISSUE_WRITE_ROLES`), `rbac.ts` org roles `owner|admin|engineer|support|viewer` |
| Email delivery | `src/lib/server/email.ts` (nodemailer, `EMAIL_SERVER`/`EMAIL_FROM`, `delivered`-boolean pattern from invitations) |

Confirmed **absent** (must be built): comments table, notifications table/UI,
realtime plumbing, rich-text rendering, upload endpoints, object storage, agent
identities (the picker's "AutoFix Agent" is a hardcoded mock).

Constraints that shape every step below:

- Migrations: one flat goose dir replayed under multiple ledgers against the same DB
  (A1) → every migration idempotent (`IF NOT EXISTS`; catalog guards for constraints).
- `schema.ts` is a hand-maintained mirror; `tests/schema-drift.test.ts` enforces sync
  against a real migrated Postgres (CI sets `SCHEMA_DRIFT_REQUIRED=1`).
- New top-level route dirs must be added to `reservedRoutes` in `hooks.server.ts`
  (route-manifest-drift test enforces this).
- Dashboard gates: `pnpm build` AND `check` AND `test --sequence.shuffle` (B12/B13).
- Feature-complete claims require a call path from route → code (B3) and e2e rows.
- Never `return` early inside a `db.transaction` callback (D18); tenant scope
  derives from the credential, never the body (B7).

## 2. Storage model (Q2, Q12)

A manual issue is an `issues` row: `issue_type='user_report'`,
`source_channel='manual_support'` (or `'api'` when created by an agent),
`fingerprint` = random UUID hex (satisfies NOT NULL + `UNIQUE(project_id,
fingerprint)`; manual issues are never deduped), `error_class='user_report'`,
`message` = report title, `count`/`first_seen`/`last_seen` vestigial. The processor
and ingestion path (`StoreEvent`, D16) are untouched — these rows are created only
by the dashboard/agent API.

Companion table:

```sql
manual_issue_reports (
  issue_id uuid PK REFERENCES issues(id) ON DELETE CASCADE,
  reporter_id text NOT NULL REFERENCES "user"(id),
  body_md text NOT NULL,
  severity varchar(20) NOT NULL DEFAULT 'medium' CHECK (low|medium|high|critical),
  created_at / updated_at timestamptz NOT NULL DEFAULT now()
)
```

**Triage inbox (Q12):** `project_id` stays NOT NULL. The create form's project
picker is optional; when blank, the report lands in a per-org **Triage project**,
provisioned lazily on first use (no backfill migration). Mark it durably —
`projects.is_inbox boolean NOT NULL DEFAULT false` (guarded `ADD COLUMN`) — rather
than by name convention. Inbox projects are excluded from the error dashboard and
alert evaluation and get no API key. A write-role **move-to-project** endpoint
re-homes an issue (updates `project_id`, writes a `moved` activity event); AI triage
later suggests or performs the move and can mark duplicates via
`relation_type='duplicate_of'`.

**Waiting flag (Q11):** `issues.waiting_on varchar(20) NULL CHECK
('reporter','team')` — set by an agent's blocking question (§7), auto-cleared when
any human posts in the thread. Not a new `status` value; the status CHECK is
untouched. Both dashboards gain a "needs your input" filter/badge.

## 3. Rich text (Q3)

CommonMark stored as text. v1 UI: textarea + preview toggle + drag-drop upload that
inserts `![...](attachment-url)`. Rendered by one shared
`$lib/components/Markdown.svelte` using `marked` + `DOMPurify` — sanitize at render,
never trust stored content. Agents emit Markdown natively. A toolbar editor (e.g.
Tiptap with a Markdown serializer) can arrive later with no data migration.

## 4. Attachments (Q4)

The one genuinely new piece of infrastructure.

- **MinIO** added to `docker-compose.yml`; client is S3-compatible so prod can point
  at S3/R2. Envs: `S3_ENDPOINT/BUCKET/ACCESS_KEY/SECRET_KEY`.
- Table: `attachments(id uuid PK, org_id, issue_id NULL, comment_id NULL,
  uploader_type user|agent, uploader_id, filename, content_type, size_bytes,
  storage_key, created_at)`; CHECK enforces at most one parent (none while
  drafting).
- **Upload**: `POST /api/uploads` multipart through SvelteKit — auth, **25 MB cap**,
  content-type allowlist (images, short video webm/mp4, pdf/txt/log/doc/zip),
  validated by **magic bytes**, not the client header. No presigned direct-to-bucket
  in v1; presigned multipart is the deferred large-video path.
- **Download**: `GET /api/attachments/[id]` streams from MinIO after read-access
  check; bucket never exposed; `Content-Disposition: attachment` for non-image
  types (stored-XSS blunting).
- Orphans (never linked to issue/comment within 24 h) reaped, invitation-reaper
  pattern (D42).

## 5. Threads (Q1, Q10)

```sql
issue_comments (
  id uuid PK,
  issue_id uuid NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
  parent_id uuid NULL REFERENCES issue_comments(id) ON DELETE CASCADE,
  author_type varchar(20) NOT NULL CHECK (user|agent),
  author_id text NOT NULL,
  blocking boolean NOT NULL DEFAULT false,     -- agent question that sets waiting_on
  body_md text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  edited_at timestamptz NULL
)
```

- Root comments chronological; `parent_id` = Slack-style one-level threads (replies
  to replies attach to the same parent). Attachments via `attachments.comment_id`.
- Works on **all** issues — discussion on service issues is a free by-product, and
  is exactly where the agent talks to developers (Q1).
- **Freshness = polling** (Q10): `GET .../comments?after=<ts>` every ~10 s while the
  tab is visible; layout polls unread count on the same cadence. SSE later, same
  client API.

## 6. Activity / history system (explicit)

`issue_activity` is the single user-visible timeline for both issue types. New
event types (extend the CHECK with a catalog-guarded migration):
`commented`, `claimed`, `claim_released`, `progress_update`, `question_asked`,
`question_answered`, `moved` (project move), `attachment_added`, `report_edited` —
joining the existing `status_changed|assigned|unassigned|regressed|ai_analysis|linked`.

- Every mutation path appends exactly one activity row **in the same transaction**
  as the mutation (D18: throw, never early-return).
- New UI: `IssueTimeline.svelte` on both detail pages, rendering actor
  (user avatar / agent badge / system), event, old→new values from the jsonb
  columns, interleaved with thread comments in a unified view or as a separate tab
  (start as a separate "Activity" tab — simpler, defer interleaving).
- **Audit (Q5)**: every `/api/agent/*` mutation *also* writes `audit_logs`
  (action, resource, agent id, key prefix used, metadata). Agent create/revoke is
  audited like org API keys. Activity = product timeline; audit = compliance trail;
  they are written together but serve different readers and retention.

## 7. Agents: identity, credentials, and the work-loop (Q5, Q6, Q11)

**Identity**: `agents(id uuid PK, org_id NOT NULL, name, kind ('ai'|'bot'),
status ('active'|'disabled'), created_by, created_at)`. Not `user` rows — no
Auth.js pollution, no accidental sign-in path. UI (assignee picker, thread authors,
timeline) resolves `actor_type='agent'` ids against this table; the hardcoded
"AutoFix Agent" mock in `IssueAssigneePicker.svelte` is replaced by a real query.

**Credentials**: `project_api_keys` gains scope value `'agent'` and a nullable
`agent_id` FK. All existing machinery (prefix/hash, status, revocation, rate
limit) reused. Tenant scope derives from the key (B7). Keys are org-scoped
(`project_id NULL`) for agents, since they work across projects.

**The work-loop** (all endpoints under `/api/agent/`, key-authenticated, every
mutation → activity + audit):

1. **Pull**: `GET /api/agent/issues?status=unresolved&claimed=false&type=user_report|system_error|any&project=…`
   — spans both issue types; this is the one deliberate bridge across Q9's
   separation. Includes `waiting_on` and triage-inbox flags so agents can also be
   pointed at triage routing.
2. **Claim**: `POST /api/agent/issues/[id]/claim` — atomic
   `UPDATE … SET assignee_type='agent', assigned_to=$agent WHERE assigned_to IS NULL`;
   0 rows → 409. `claimed` activity; claimant auto-subscribed.
   `DELETE …/claim` releases (`claim_released`); org owner/admin can force-release
   from the UI. No auto-expiry in v1.
3. **Work + report progress**: `POST …/progress {message_md}` → `progress_update`
   activity (in-app notification only, no email — Q7). Optionally
   `PATCH …/status`, `POST …/relations` (link to service issues, mark
   `duplicate_of`), `POST /api/uploads` + attach.
4. **Ask when blocked**: `POST …/questions {body_md, audience: 'reporter'|'team'}`
   → a `blocking=true` comment + `question_asked` activity + sets
   `issues.waiting_on` (Q11). Notification **bypasses the email throttle** — a
   direct question deserves immediate email. Audience is informational: for
   `user_report` issues the reporter is the natural addressee; for `system_error`
   issues the subscribed developers are.
5. **Get unblocked**: any human comment on a waiting issue clears `waiting_on`,
   writes `question_answered`, notifies the agent's subscription (agents poll
   `GET …/issues/[id]/comments?after=` to read answers — pull model, Q6).
6. **Resolve**: `PATCH …/status {status:'resolved', resolved_in_version?}` →
   existing resolution columns (`resolved_by_type='agent'`).

Human claiming is the same mechanics through the session-authenticated API: a
"Claim" button self-assigns with the identical atomic guard.

**Discovery stays pull** (Q6). v2: publish `issue.created`/`issue.claim_released`
to the existing NATS for push-driven agents.

## 8. Notifications (Q7, Q11)

```sql
issue_subscriptions (issue_id, subscriber_type user|agent, subscriber_id,
                     reason (reporter|claimant|participant|manual),
                     UNIQUE(issue_id, subscriber_type, subscriber_id))
notifications (id, user_id, issue_id, kind (commented|claimed|status_changed|
               resolved|linked|progress_update|question_asked),
               actor_type/actor_id, payload jsonb, read_at NULL, created_at)
```

- Auto-subscribe: reporter on create, claimant on claim, any commenter. Manual
  subscribe/unsubscribe toggle on the detail page. Service issues get subscribers
  only through participation (assignee, commenters, status-touchers) — never the
  whole org.
- Fan-out in the same request as the mutation: `notifications` insert inside the
  transaction; email after commit, best-effort, `delivered`-style result
  (invitation pattern). No queue in v1.
- **Email policy**: only `commented`, `claimed`, `status_changed`, `resolved`;
  per-issue per-user throttle (~15 min). `progress_update` is in-app only.
  `question_asked` (blocking) **bypasses the throttle**. Email links point at the
  thread; email is never a reply channel (Q1).
- UI: bell + unread count in the layout (polled, Q10), `/[orgSlug]/notifications`
  list, mark-read endpoint.

## 9. Permissions (Q8)

New helper `requireReportAccess` beside `issue-access.ts` — a deliberate deviation,
not a loosening of `requireIssueAccess`:

| Action | Who |
|---|---|
| Create a report | any org member, **including `viewer`** |
| Comment / reply in threads | any member who can read the issue (viewers included) |
| Edit/delete **own** report body or comments | author, until the issue is resolved |
| Claim / release / resolve / link / move / edit others' reports | `ISSUE_WRITE_ROLES` (`owner,admin,engineer,support`) + agents |
| Force-release a claim, delete others' comments | `owner`, `admin` |
| Manage agents + agent keys | `owner`, `admin` |

## 10. Routes and dashboards (Q9)

Org-scoped, imitating `[orgSlug]/projects/.../issues/[issueId]`:

- `/[orgSlug]/reports` — manual-issues dashboard: tabs *All / My reports / Claimed
  by me / Unclaimed / Needs input / Triage*; columns reporter, severity, claimant
  (user or agent badge), comment count, linked issues, waiting badge, status.
- `/[orgSlug]/reports/new` — optional project picker ("Not sure? → Triage"),
  title, severity, Markdown body, attachments.
- `/[orgSlug]/reports/[issueId]` — report body, attachments, linked service issues
  (reuse `IssueRelations.svelte`), claim control, Activity tab (§6), thread (§5).
- `/[orgSlug]/notifications` — no `reservedRoutes` change needed (org-scoped).
- **Strict separation**: existing issues list/search and alert evaluation add
  `issue_type='system_error'`; triage-inbox projects additionally excluded there.

Session APIs: `POST /api/projects/[projectId]/reports` (and an org-level create
that defaults to Triage), `GET/POST /api/issues/[issueId]/comments`,
`POST /api/issues/[issueId]/claim`, `POST /api/issues/[issueId]/move`,
`POST /api/uploads`, `GET /api/attachments/[id]`, `GET/PATCH /api/notifications` —
manual-validation style (const allowlists + `throw error(status)`), matching the
invitations endpoint.

## 11. Phased delivery (each phase independently shippable + e2e-gated)

| Phase | Delivers | Key risks it retires |
|---|---|---|
| **M1 Core CRUD + activity — DONE 2026-08-11** (see VERIFIED_STATE.md "Manual issues — M1"; the `issue_comments` table shipped early in the M1 migration, deliberately inert until M3) | Migrations (`manual_issue_reports`, `is_inbox`, `waiting_on`, activity event types), Triage auto-provisioning, create/list/detail routes, Markdown render, claim/release by user, move-to-project, linking, `IssueTimeline` | Schema + drift test; proves reuse-`issues` and Triage decisions |
| **M2 Attachments — DONE 2026-08-11** (see VERIFIED_STATE.md "M2 attachments") | MinIO in compose, `attachments`, upload proxy (25 MB, magic bytes), streaming download, orphan reaper | The only new infra; download access control |
| **M3 Threads — DONE 2026-08-11** (see VERIFIED_STATE.md "M3 threads") | `issue_comments`, thread UI (replies + attachments), polling refresh | UI complexity; shuffle-safe component tests |
| **M4 Notifications — DONE 2026-08-11** (see VERIFIED_STATE.md "M4 notifications"; claim-release reuses kind `claimed`+`payload.released`, move sends none) | subscriptions + notifications tables, bell UI, email fan-out + throttle + blocking bypass | Transaction discipline (D18); throttle correctness |
| **M5 Agents — DONE 2026-08-12** (see VERIFIED_STATE.md "M5 agents"; agent-key rate limiting deliberately not wired yet — follow-up) | `agents` table, `'agent'` key scope + `agent_id`, `/api/agent/*` work-loop (pull/claim/progress/ask/resolve), `waiting_on` wiring, audit logging, real assignee picker | Credential scoping (B7); atomic claim under concurrency; replaces the mock |
| **M6 Later** | SSE (LISTEN/NOTIFY or NATS), NATS push discovery, AI triage (project routing + `duplicate_of`), presigned large uploads, toolbar editor | All deliberately deferred |

Per-phase definition of done: migration replayed twice against a disposable
Postgres; `schema.ts` updated, drift test green; `pnpm build && pnpm check &&
pnpm test --sequence.shuffle`; route→code call path exists (B3); new e2e matrix
rows green under `SENTINEL_E2E=1 -tags=e2e`; fixes proven red-first. M5
additionally needs a concurrency test for the atomic claim (two agents, one 409).
