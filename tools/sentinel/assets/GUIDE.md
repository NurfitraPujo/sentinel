# Sentinel Agent Guide

This is the canonical, provider-agnostic guide for any AI agent, script, or bot that operates
**on** Sentinel (the error-tracking product) via `/api/agent/*`. It applies equally to Claude,
GPT, a cron job, or a shell script — nothing here assumes a particular agent framework or model.

If you are looking for guidance on *contributing to Sentinel's own codebase*, see the repo root
`AGENTS.md` / `CLAUDE.md` instead. This guide is about triaging issues as a registered agent
identity against a running Sentinel instance.

Everything in this guide is verified against the route handlers under
`apps/dashboard-web/src/routes/api/agent/**`, `$lib/server/agent-ops.ts`, `$lib/server/agent-events.ts`,
`apps/processor-go/webhooks/dispatcher.go`, and `tools/sentinel/`. Where this guide and any other
document disagree, trust this guide or the source files it cites.

`docs/agents/openapi.agent.yaml` is a machine-readable OpenAPI 3.1 companion to this guide. As of N6
it is **GENERATED** from `apps/dashboard-web/src/lib/server/agent-api-spec/` (`schemas.ts` +
`registry.ts`) via `pnpm --dir apps/dashboard-web openapi:agent` — never hand-edit the YAML, it will
be overwritten. `openapi-drift.test.ts` and `completeness.test.ts` (run as part of `pnpm test`) fail
CI if the committed YAML falls out of sync with the registry, or if the registry falls out of sync
with the actual routes under `src/routes/api/agent/**`.

---

## 1. Concepts

- **Agent identity.** An "agent" is a row in the `agents` table, scoped to one organization. It is
  a distinct actor type from a human user — comments, claims, status changes, and audit log entries
  it makes are attributed to `actorType: 'agent'`, `actorId: <agentId>`.
- **Org-scoped Bearer keys.** Agents authenticate with a Bearer API key of the form
  `sent_agent_<64 lowercase hex chars>`. Each key is a row in `project_api_keys` with `scope='agent'`,
  linked to exactly one agent and (through it) exactly one organization. You will never see or
  compute the raw key server-side again after it is issued — only its hash is stored.
- **Tenant scope always comes from the key, never from you.** Every `/api/agent/*` route derives
  `organizationId` from the authenticated key/agent, never from any URL param or request body field
  you supply. You cannot access another organization's data by passing a different id anywhere.
- **Audit trail.** Every mutating agent action writes an `audit_logs` row (action name like
  `agent.issue.status_changed`, `agent.issue.claimed`, `agent.issue.commented`, ...) plus an
  `issue_activity` row (the same feed `GET /api/agent/events` reads). Reads are not audited.

## 2. Authentication and your first request

Every request needs `Authorization: Bearer <key>`. The simplest way to check a key works AND see
which agent/org/key it resolves to is `GET /api/agent/self` (R1a, §13):

```bash
curl -s https://sentinel.example.com/api/agent/self \
  -H "Authorization: Bearer sent_agent_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
# {"agentId":"...","name":"...","organizationId":"...",
#  "key":{"id":"...","prefix":"sent_agent_","expiresAt":null,"lastUsedAt":null}}
```

A `401` with body `{"message":"Invalid API key"}` (or `"Missing or malformed Authorization header"`;
note the field is `message` — only per-route 400 validation bodies use `{"error": ...}`)
covers unknown key, wrong scope, revoked/expired key, and a disabled agent — deliberately the same
message for all of those, so the endpoint can't be used to probe which is true.

A `429` means you hit `project_api_keys.rate_limit_rpm` for this key. The limiter is a **fixed
60-second window**, not a sliding one — a burst can legitimately reach up to 2x your configured
rate across a window boundary (see DEPLOYMENT.md §9 for why this is the deliberate choice, not a
bug). Every 429 carries an accurate `Retry-After` header (seconds until that window resets, not a
fixed guess) — back off for exactly that long rather than polling immediately or applying your own
backoff heuristic on top.

**Key rotation.** `POST /api/agent/key/rotate` mints a new secret for the SAME agent this key
authenticates as and returns it in the response body ONCE — never persist the response, only the
secret. The OLD key is not revoked immediately: it stays valid (its `expires_at` is pulled forward)
for `AGENT_KEY_ROTATION_GRACE_HOURS` (operator-configured, default 24h, `0` = immediate), so an
unattended loop mid-task with the old key cached keeps working until you've had a chance to swap
it in. `sentinel key rotate` wraps this and prints the new secret to stdout with a warning to
stderr.

## 2a. Per-project agent settings (`GET /api/agent/projects`)

Every project in the response carries an `agentSettings` object, set by the dashboard operator
(`manage_agents` RBAC) and audited on every change:

```json
{
  "id": "project-1",
  "name": "Web",
  "isInbox": false,
  "agentSettings": {
    "fixEnabled": true,
    "maxPrsPerDay": 5,
    "repo": {
      "provider": "github",
      "owner": "acme",
      "repo": "widgets",
      "defaultBranch": "main",
      "testCmd": "npm test",
      "agentCmd": "npm run fix",
      "cloneDepth": 1
    }
  }
}
```

A project with no settings configured returns `{"fixEnabled": false, "maxPrsPerDay": null, "repo":
null}` — never a missing field.

**What this means for a worker:**
- Attempt an actual FIX (open a PR against `repo`) only when **both** `fixEnabled === true` **and**
  `repo` is non-null. Otherwise, operate propose-only — comment/report findings on the issue but do
  not clone or push.
- `maxPrsPerDay`, when set, is a rate cap the worker must enforce itself; `null` means no cap.
- `testCmd` (always present when `repo` is non-null) and `agentCmd` (optional) are operator-provided
  shell commands the worker executes verbatim inside its fix-container sandbox to validate/apply a
  fix — they are trusted operator input, not agent input. See `docs/memory/DECISIONS.md` D23 for why
  storing and executing an operator-supplied command server-side is an accepted design, and what the
  actual trust boundary is (the fix container, not the command string).
- One repo connection per project in v1 — `repo` is a single object, not a list.

## 2b. Personal keys (`sentinel login`)

There are two kinds of agent key, and `GET /api/agent/self` tells you which one you hold (`kind`, `owner`, §13):

- An **org agent key** (`kind: "ai" | "bot"`, `owner: null`). An org owner/admin mints it on *Settings → Agents* for
  an agent identity. It sees every project in the organization. This is unchanged.
- A **personal key** (`kind: "personal"`, `owner: {userId, name, email, role, canWrite}`). Any org member gets one
  with `sentinel login`: the CLI prints a code, the member approves it in the browser at `<dashboard>/cli/approve`,
  and the key arrives once. No admin is involved. A personal key **acts as that person and can never do more than they
  can in the dashboard**, re-checked on every request:
  - **Lists** (`issues`, `events`, `projects`, `claim-next` candidates, trace occurrences, relations) contain only the
    person's projects and environment grants. Anything else is a `404`, the same as another organization's object.
  - **Mutations** (status, claim/release, comments, questions, progress, severity, relations, uploads, batch ops)
    need a role with write access: `owner`, `admin` or `engineer`. A `support` or `viewer` member's key is
    read-only, and every mutation gets `403` *"…your organization role is read-only…"* (`owner.canWrite` is `false`).
  - **Never available to a personal key** (`403`): `members` (list/invite/remove) and `repo-credentials`.
  - It stops working (`401`) the moment the person leaves the organization. Removal also revokes it, so re-inviting
    them later does not bring it back. A role or grant change applies to the next request.
  - It expires after 90 days by default (`CLI_PERSONAL_KEY_TTL_DAYS`). `sentinel key rotate` rotates only that key:
    the old key stops working **immediately** (no grace window), the new one keeps the **same expiry** (only a new
    `sentinel login` renews it), and the CLI saves it into the profile instead of printing it. `sentinel logout` revokes it (`POST /api/agent/key/revoke`). A new `sentinel login` from the
    same device replaces it. The person sees and revokes their keys under *Settings → CLI sessions*; org
    owners/admins see everyone's under *Settings → Agents*.
  - Claims, comments and progress made with it show as **"<name> (CLI)"**.

The device flow itself is `POST /api/cli/device` → approve → poll `POST /api/cli/device/token` (RFC 8628). Those
routes are for the CLI and are documented in `docs/plans/CLI_BROWSER_LOGIN.md`, not in the agent OpenAPI.

## 3. Work discovery: the events feed

`GET /api/agent/events` is a **seq-cursored, org-scoped feed** over `issue_activity`, joined out to
the owning issue so you never need a second round trip per event.

```
GET /api/agent/events?after=<seq>&limit=<n>&type=<t1,t2>&project=<projectId>&claimed=me
```

| Param | Meaning |
|---|---|
| `after` | Last `seq` you've already consumed. `0` or omitted = from the start. Results are strictly `seq > after`. |
| `limit` | Page size, clamped to `[1, 200]`, default `50`. |
| `type` | Comma-separated list of event types to filter to (see the type list below). Invalid types 400. |
| `project` | Restrict to one project id. |
| `claimed=me` | Only events on issues currently claimed by *your* agent id. `me` is the only accepted value. |

Response:

```json
{
  "events": [
    {
      "seq": 42,
      "eventType": "status_changed",
      "actorType": "agent",
      "actorId": "agt_...",
      "oldValue": { "status": "unresolved" },
      "newValue": { "status": "resolved" },
      "createdAt": "2026-08-14T12:00:00.000Z",
      "issue": {
        "id": "iss_...",
        "title": "...",
        "status": "resolved",
        "issueType": "system_error",
        "projectId": "prj_...",
        "assigneeType": "agent",
        "assignedTo": "agt_...",
        "claimedAt": "2026-08-14T11:59:00.000Z",
        "waitingOn": null
      }
    }
  ],
  "cursor": 42,
  "hasMore": false
}
```

The embedded `issue` object's `assigneeType`, `assignedTo`, `claimedAt`, and `waitingOn` reflect
the issue's **current** state at read time, **not** its state when the event occurred. A
dispatcher can therefore evaluate "is this issue claimed by me right now?" straight from the feed —
`assigneeType === "agent" && assignedTo === <your agent id>` — without a follow-up
`GET /api/agent/issues/:id` per event. `assignedTo`/`assigneeType`/`claimedAt` are `null` when the
issue is currently unclaimed; `waitingOn` is `null` unless an unanswered blocking question is
outstanding (`"reporter"` or `"team"`).

Valid `eventType` values (the full `issue_activity.event_type` set): `status_changed`, `assigned`,
`unassigned`, `regressed`, `ai_analysis`, `linked`, `commented`, `claimed`, `claim_released`,
`progress_update`, `question_asked`, `question_answered`, `moved`, `attachment_added`,
`report_edited`, `report_created`, `created`, `occurrence_burst`.

Note on `assigned`/`unassigned` vs `claimed`/`claim_released`: **assignment is user-only.** The
dashboard cannot assign an issue *to* an agent — the server rejects it with 400 — so
`assigneeType === "agent"` in the feed always means a real self-acquired claim (CONTEXT.md
"Claim", DECISIONS.md D24). The only path to agent ownership is your own
`POST /api/agent/issues/:id/claim`. A dashboard admin unassigning an agent-claimed issue is a
deliberate release override and appears as `claim_released` with `actorType: "user"`.

**Discovery events (`created` / `occurrence_burst`)** — written by the processor, not by any
dashboard mutation, so they are the primary signal for finding NEW work without full-table
polling:

- `created` fires exactly once, the moment a genuinely new issue (new fingerprint for the
  project) is first stored. `newValue` carries `{errorClass, projectId}`. Watch for this event
  type if you want to be notified of brand-new service errors as they happen.
- `occurrence_burst` fires on a *repeat* occurrence of an existing issue, throttled to at most one
  per issue per `OCCURRENCE_EVENT_MIN_INTERVAL_SECONDS` (processor env, default 1 hour) — it is a
  "this issue is still happening" heartbeat, not a per-occurrence event, so you will not be
  flooded during a traffic spike. `newValue` carries `{count, lastSeen}` (the issue's occurrence
  count and last-seen timestamp at emission time). No `occurrence_burst` is ever emitted in the
  same throttle window as a `created` or `regressed` row for that issue.
- **No backfill**: these events only start appearing for activity that happens after this feature
  is deployed. Pre-existing issues are still fully discoverable — bootstrap your initial view with
  `GET /api/agent/issues` (§5 step 1) rather than expecting the events feed to replay history.

**Cursor semantics you must understand:**

- The feed is **at-least-once**. Re-fetching the same `after` can return events you've already
  seen if you didn't advance your cursor; dedupe on `seq` client-side if that matters to you.
- Advance your cursor to `cursor` (the highest `seq` in the page) after processing a page, or to
  the max `seq` you've actually finished handling — not before.
- **2-second lag guard**: the query never returns rows created within the last 2 seconds of
  `now()`. This exists because `seq` is a bigint IDENTITY assigned at INSERT time, not necessarily
  in commit order under concurrent transactions — without the guard, a poller could read past a
  slightly-delayed commit and never see it. **Practical consequence: expect new events to appear
  in the feed roughly 2 seconds after they happened, not immediately.**
- `hasMore: true` means keep paging with the same filters and the new `cursor` before you go back
  to sleep/poll-interval — there's more backlog waiting right now.

## 3a. One issue's history: `GET /api/agent/issues/:id/activity`

The events feed is org-wide and moves forward. To read **one issue's** history — who claimed it, status changes,
comments, progress notes, regressions, occurrence bursts — page backwards through it:

```
GET /api/agent/issues/:id/activity?limit=<n>&before=<nextCursor>
```

- Newest first. `limit` is entries per page, clamped to `[1, 200]`, default `50`; a non-integer is `400`.
- `before` is the previous page's `nextCursor` (opaque; anything else is `400`). The response is
  `{ "entries": [...], "nextCursor": "..." | null, "hasMore": bool }`.
- Each entry has the events feed's item fields (`seq`, `eventType`, `actorType`, `actorId`, `oldValue`, `newValue`,
  `createdAt`) plus `id`, `actor: {type, id, name, personal}` — a user's name (else email), an org agent's name,
  `"<owner name> (CLI)"` for a personal CLI agent, a label for a system actor; `name` is `null` only for a deleted
  user/agent — and, by type, `comment: {id, excerpt, deleted}` (`commented`, `question_asked`; a 280-character
  excerpt), `assignee` (`assigned`) or `burst`.
- **Bursts are collapsed.** The processor writes at most one `occurrence_burst` row per issue per
  `OCCURRENCE_EVENT_MIN_INTERVAL_SECONDS`, and on a noisy issue they are most of the history. A run of consecutive
  burst rows (nothing else between them) is **one** entry with `burst: {rows, firstAt, lastAt, occurrencesFrom,
  occurrencesTo}` (the issue's total occurrence count at the run's oldest and newest row). A run is never split
  across pages: `limit` counts entries, and the cursor moves past the whole run.
- Authorized exactly like `GET /api/agent/issues/:id`: another organization's issue — or, for a personal key, one
  outside the owner's projects/environment grants — is `404`. A `linked` entry whose target the personal key cannot
  see is redacted as on the events feed (`targetIssueId: null, targetRedacted: true`).

CLI: `sentinel issue activity <issueId> [--limit N] [--before CURSOR] [--all] [--json]` (a table by default).

**Malformed ids.** Every `:id` in an `/api/agent/*` path (and every dashboard/API path id) must be a UUID; anything
else — `--help`, a truncated `42a26f64-` — matches no route and is a `404` `{"message":"Not found"}`, never a `500`.

## 4. Claims are advisory — always check `claimedBy`

**Claiming is NOT enforcement.** It is a signal, checked by nothing except the claim endpoints
themselves — a claimed issue's `status`/`comments`/`progress`/`relations` mutations still succeed
for ANY agent, claimant or not (A11, `docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md` — deliberate,
kept advisory rather than enforced so this stays consistent with how human collaborators already
work in this product; enforcing it would also break existing automation that mutates without
claiming first). **Do not assume a claim protects you from a collaborator.** Before mutating an
issue you did not just claim yourself, read its current `assignedTo`/`assigneeType`/`claimedAt`
(from `GET /api/agent/issues/:id` or the scope embedded in any mutation response) and treat a
non-null, non-you `assignedTo` as "someone else is actively on this — coordinate or back off",
even though the server will not stop you.

When a non-claimant agent mutates a claimed issue anyway, the server logs a structured
`agent.mutated_claimed_issue` warning server-side (observability only — this never changes the
response you get). If you want a REAL synchronization primitive, claim/release's 409 is it — see
below.

- `POST /api/agent/issues/:id/claim` — atomic conditional UPDATE. Succeeds (200) with the updated
  issue (which includes `assignedTo`/`claimedAt`) if unclaimed or already claimed by you; **409**
  if claimed by someone else. Self-reclaim is **idempotent as of N9**: re-claiming an issue you
  already hold returns 200 with `"alreadyClaimed": true` on the body (no new activity row, no
  notification), so a plain retry-on-timeout of a claim is safe to resend — same guarantee release
  already gives. The flag is present only on that self-reclaim path; a fresh claim omits it. As of
  N7f (A11) the 409 body is enriched with the current claim
  state so you don't need a second read: `{"message": "...", "claimedBy": "<agentId or userId>",
  "claimedAt": "<ISO timestamp or null>"}`. Back off on 409 — don't retry-loop, don't force-claim
  (there is no force option for agents; force-release is owner/admin-only through the
  session-authenticated UI).
- `DELETE /api/agent/issues/:id/claim` — releases **your own** claim only. The underlying query's
  `WHERE assigned_to = <your agentId>` (sourced from your credential, never a request param) is
  what enforces this; you cannot release someone else's claim by any means. **Idempotent as of
  N7d**: if the conditional UPDATE matches zero rows, the server re-checks — an issue that is now
  simply unclaimed (by anyone) means your release already succeeded on an earlier attempt whose
  response you never saw, and this call returns 200 with the current issue, no new activity row,
  no notification. Only a REAL conflict (the issue is claimed by a different agent/user now) still
  409s, with the same `{message, claimedBy, claimedAt}` enrichment as claim's 409. This means a
  plain retry-on-timeout of a release call is always safe to resend.
- Release when you're done, or when you're blocked and waiting (see §6) so another agent — or a
  human — can pick it up if you never come back.

**Claim staleness — you don't have to remember to release.** A scheduled reaper protects against
an unattended loop that crashes or hangs mid-claim: if your claim (agent-type only; human claims
are never touched) goes older than `CLAIM_STALE_HOURS` (default 24) with no activity from you —
no comment, progress update, question, or status change — on that issue in that same window, it is
force-released automatically. This shows up in the events feed as a `claim_released` event with
`actor_type: "system"`, `actor_id: "sentinel-claim-reaper"`, and
`new_value: {"previousAssignee": "<your agentId>", "reason": "stale"}` — poll for it (or for the
issue simply reappearing in `GET /api/agent/issues?claimed=false`) if you resume after a gap and
aren't sure whether you still hold a claim you made. Posting any activity (a progress update is the
cheapest) on an issue you're actively working resets the clock, so a genuinely long-running triage
is safe as long as you check in within the window; don't rely on this as a heartbeat substitute for
actually finishing or releasing when you're done.

## 5. Triage recipe

A minimal, working loop:

1. **Discover** — poll `GET /api/agent/events` for `created` (new issue) and `occurrence_burst`
   (existing issue still active) events (see §3), or `GET /api/agent/issues?claimed=false` to
   enumerate current unclaimed work directly. The events feed is the low-latency path for new
   work; the issues list is the reliable path for anything that existed before you started
   polling (no backfill — see §3), and it's the recommended way to bootstrap: page through
   `GET /api/agent/issues?limit=50&sort=firstSeen` (add `&since=<ISO timestamp>` on a resumed
   bootstrap to skip issues you've already seen) and follow `nextCursor` — present in the response
   only when you passed `limit` — via `&cursor=<nextCursor>` until it's absent, then switch to the
   events feed for ongoing discovery. Omitting `limit`/`sort`/`since`/`cursor` entirely keeps the
   original unbounded, `lastSeen`-descending list (no pagination) for backward compatibility;
   `limit` is capped at 200 server-side. The keyset cursor (on `(sortColumn, id)`, not an offset)
   stays stable even as new issues arrive mid-page. Filter by claim state with `&claimed=false`
   (unclaimed, by anyone), `&claimed=true` (claimed, by anyone), or `&claimed=me` (claimed by
   **your** agent id specifically — resolved from your credential, never a request param) — the
   last is how you re-enumerate your own in-flight work after a restart. `&waiting=true` narrows
   to issues currently blocked on a question (see §6).
2. **Claim** — `POST /api/agent/issues/:id/claim`. Stop here (409) if someone beat you to it.

   **Or do steps 1–3 in one call** — `POST /api/agent/issues/claim-next` selects the next
   unclaimed, **unresolved** issue in your org, claims it for you, and returns the SAME body as
   step 3's `GET /api/agent/issues/:id` (with `issue.assignedTo` already set to you). The select
   and the claim are one statement (`FOR UPDATE SKIP LOCKED`), so N agents calling it at once get
   N different issues — there is no 409 to handle. "Next" is the first row
   `GET /api/agent/issues?claimed=false` would show (`lastSeen` descending, `id` tiebreak); the
   optional JSON body takes the list route's own filters — `type`, `project`, `environment`,
   `source` — plus `sort` (`firstSeen` | `lastSeen`). An empty or absent body means "any". A
   **404** (`{"message": "No unclaimed issue matches"}`) means the queue is empty for those
   filters, not that the route is missing — loop until you see it. Resolved and ignored issues
   are never handed out, and the single-issue claim/release routes are unchanged.
3. **Get full detail** — `GET /api/agent/issues/:id` returns:
   ```json
   {
     "issue": { "id": "...", "issueType": "system_error", "status": "unresolved", "message": "...", "waitingOn": null, "affectedUserCount": 612, "affectedTenantCount": 7, ... },
     "report": null,
     "latestOccurrence": { "stacktrace": {...}, "metadata": {...}, "environment": "...", "platform": "...", "traceId": "...", "createdAt": "..." },
     "relations": [ ... ]
   }
   ```
   - `affectedUserCount` / `affectedTenantCount` (also on every row of `GET /api/agent/issues`)
     are how many **distinct** users and tenants this issue's occurrences have touched within the
     retention window — the blast radius, as opposed to `count`, which is how many times it fired.
     A loop hitting one user ten thousand times has `count: 10000, affectedUserCount: 1`; an outage
     hitting six hundred users once has `count: 600, affectedUserCount: 600`. Both are integers,
     never null; `0` means no occurrence in the window carried a `user_id` / `tenant_id`. See §15.
   - For `issueType: "system_error"`: `report` is `null`; read `latestOccurrence.stacktrace` (and
     `.metadata`, `.traceId`) for the crash detail. Page further back with
     `GET /api/agent/issues/:id/occurrences?limit=&before=` (newest-first, `limit` clamped to
     `[1, 50]`, default 20; `before` is an exclusive ISO-timestamp cursor).
   - **Follow a trace across projects** — an occurrence's `traceId` is shared by every project that
     handled the same request (mobile → web → core and its workers). `GET /api/agent/traces/:traceId`
     returns **every occurrence in that trace across your key's organization**, oldest first by the
     client's event time (`createdAt`), as `{ "occurrences": [...], "nextCursor": "..." }`. Each
     occurrence has the usual fields plus `projectId`, `projectName`, `issueId` and `spanId`, so you
     can open the issue it belongs to. `:traceId` must be 32 hexadecimal characters (anything else is a `404`). A trace your key's
     organization has no occurrence of — never seen, another organization's, or aged out — is a
     `200` with `{"occurrences": []}`; the cases are not distinguishable. `?limit=` is `[1, 200]` (default 50); pass the returned `nextCursor` back as
     `?cursor=` for the next page (absent on the last page; the value is opaque). `?environment=` is a
     filter. Like every agent read it is organization-scoped, **not** environment-scoped. Occurrences
     older than the retention window are gone.
   - For `issueType: "user_report"`: `latestOccurrence` is `null`; read `report.bodyMd` (the
     reporter's Markdown description) and `report.severity`.
4. **Post a triage summary** — `POST /api/agent/issues/:id/comments` with
   `{"body_md": "...", "attachment_ids": ["..."]}` (both optional except `body_md`). This is a
   normal, non-blocking comment — it emails subscribers like any human comment would.
   - **Edit or delete your own comment** — `PATCH`/`DELETE /api/agent/issues/:id/comments/:commentId`
     (N7e). `PATCH` takes `{"body_md": "..."}`. Both 403 if the comment wasn't authored by YOUR
     agent id (no moderator carve-out for agents — that's a human-only role), 404 if the comment
     belongs to a different issue or no longer exists.
   - **Set a `user_report` issue's severity** — `PATCH /api/agent/issues/:id/report/severity` with
     `{"severity": "low"|"medium"|"high"|"critical"}` (N7e). 400 on a `system_error` issue (it has
     no `manual_issue_reports` row to set severity on).
5. **Resolve, or ask a blocking question** (§6) if you can't proceed without more info.
6. **Set status** — `PATCH /api/agent/issues/:id/status` with
   `{"status": "resolved"|"unresolved"|"ignored", "resolved_in_version": "v1.2.3"}` (the version
   field is optional and only meaningful when resolving).
   - **`resolved_in_version: "next"` is reserved** — "resolved in the next release, whichever it
     turns out to be". Sentinel rewrites it to the first release the **project** sees after the
     resolve (releases are recorded from `release_version` on ingested events, ordered by when
     they were first seen — never by parsing the version). **A release is a project-level fact**,
     not a per-environment one: a version that was already on staging when you resolved is *not*
     the next release, and the version `next` becomes may have been first seen in an environment
     you cannot list issues for. The value is trimmed and compared byte-for-byte after trimming:
     `Next` is an ordinary version, `next ` is `next`. A project that literally names a release
     `next` cannot resolve "in `next`" — the literal is documented, never validated against.
     Until the release arrives, `GET /api/agent/issues/:id` reads `resolvedInVersion: "next"`; a
     consumer that parsed this field as semver must expect a non-version string (it was always free
     text). The same literal works on the dashboard's bulk resolve.
   - **Snooze — an ignore with a condition.** `status: "ignored"` takes two optional keys, both only
     valid with `ignored` (any other status → `400 snoozedUntil is only valid with status ignored` /
     `400 snoozeOccurrences is only valid with status ignored`):
     - `snoozed_until`: ignore until this instant. It must be a **full ISO 8601
       date-time with seconds and a zone** — `2026-09-20T09:00:00Z` or
       `2026-09-20T16:00:00+07:00` (offsets are accepted; both are the same instant) — and be
       strictly in the future. A date-only `2026-09-20`, a zone-less `2026-09-20T09:00`, an
       unparseable value and a past one all get the one string
       `400 snoozedUntil must be a future timestamp`. On the events feed and in the issue's
       activity, `new_value.snoozedUntil` is **always the normalised UTC instant with `Z`**
       (`2026-09-20T09:00:00.000Z`), whatever offset you sent — parse it, never string-compare it.
     - `snooze_occurrences`: ignore until this many **more** occurrences have been stored — a JSON
       integer in `[1, 1000000]` (`0`, `1000001` and the string `"12"` are refused with
       `400 snoozeOccurrences must be an integer between 1 and 1000000`). Stored as
       `snoozeOccurrenceTarget` with `snoozeCountAt` = the issue's `count` at the time, so the
       detail can read "100 more · 37 so far".
     - Either condition ends the snooze; both may be sent together. Neither present (or `""` for
       either — an empty string is treated as omitted) is the indefinite ignore it always was. When
       a snooze ends, the issue goes back to `unresolved` and a `status_changed` activity row is
       written with a `reason` (`snoozed_until_passed` / `snooze_occurrences_reached`) — **no
       notification is sent** for the ending (subscribers were told "ignored"; they are not told
       "back"; watch `/api/agent/events` for it). A resolve or a reopen also ends a snooze.
     - **A snooze on a manual (`user_report`) issue never ends** — nothing ingests for it, so
       neither condition can ever be met. Use a plain ignore on one and reopen it by hand; the
       endpoint does not stop you, it just cannot help you.
   - **Ignored issues no longer page.** An ignored issue's occurrences — snoozed or not, ignored
     before this shipped or after — do not reach alert configs at all, and the alert dispatcher's
     frequency window is not fed during a snooze: the occurrence that ends one starts that window
     cold.
7. **Release your claim** once the issue is resolved/ignored, or immediately if you're now
   blocked and waiting on someone else.

## 6. The question / waiting_on loop

A **blocking** question is different from a comment: it sets `issues.waiting_on` and forces an
immediate email, bypassing the normal 15-minute notification throttle.

```
POST /api/agent/issues/:id/questions
{ "body_md": "Can you share the request payload that triggered this?", "audience": "reporter" }
```

`audience` must be `reporter` or `team`. This writes a `blocking: true` comment row, sets
`issues.waiting_on = audience`, and fans out a `question_asked` activity event.

**Clearing:** ANY user (human) reply — regardless of who set `waiting_on` or which audience it
targeted — clears `waiting_on` back to `null` and writes a `question_answered` activity event, in
the same transaction as the reply comment. Agent replies never clear it (a blocking question is
always agent-authored, a clearing reply is always user-authored — these are mutually exclusive).

**How to notice the answer — poll, don't assume push:**
- Poll `GET /api/agent/issues/:id/comments?after=<ISO timestamp>` for new replies, or
- Poll `GET /api/agent/events?type=question_answered&claimed=me` (subject to the 2s lag guard, §3)
  for the clearing event itself.

There is no push notification to you as the agent for this — you must poll one of the two.

**Knowing how long a question has been waiting.** Each row from `GET /api/agent/issues` carries a
`waitingSince` field: an ISO timestamp of when the current blocking question was asked while the
issue is waiting (`waitingOn`/`isWaiting` set), and `null` otherwise. Combine it with
`?claimed=me&waiting=true` to list your own still-unanswered questions and decide which to nag —
without reconstructing timing from the comment thread. `waitingSince` is cleared (back to `null`)
the moment the question is answered, or the issue is resolved/ignored; there is no backfill, so a
question that was already in flight before this field shipped reports `null`.

## 7. Progress updates vs. comments

Both attach to an issue's timeline, but they are **not** interchangeable:

| | Endpoint | Emails subscribers? | Use for |
|---|---|---|---|
| Comment | `POST /api/agent/issues/:id/comments` `{body_md, attachment_ids?}` | Yes | Findings meant for a human to read |
| Progress update | `POST /api/agent/issues/:id/progress` `{message_md}` | **No** — in-app only | "Still working on it" narration, intermediate steps, noise you don't want to spam inboxes with |

`progress_update` is deliberately excluded from the notification system's emailable-kinds list.
Prefer progress updates for anything that isn't meant to interrupt a human.

## 8. Relations

```
POST   /api/agent/issues/:id/relations   { "target_issue_id": "...", "relation_type": "linked_to"|"caused_by"|"duplicate_of" }
DELETE /api/agent/issues/:id/relations   { "target_issue_id": "...", "relation_type": "..." }
```

- Both the source and target issue must resolve within your own organization.
- Self-relation (`target_issue_id === issueId`) is rejected with 400.
- A duplicate relation (same source/target/type already exists) is rejected with 409.
- **Cycle rule (`caused_by`, N7d):** if the REVERSE pair already exists — you already have
  `B caused_by A` and you POST `A caused_by B` — this is rejected with 409
  ("Reverse relation already exists (would create a cycle)"), the same way `duplicate_of` has
  always rejected its own reverse pair. This is a **2-cycle guard only**: it catches the direct
  A→B/B→A case, not longer cycles through a third issue (A→B→C→A) — there is no full graph-cycle
  detection. `linked_to` has no such guard (it's symmetric-ish by design).
- `DELETE` identifies the relation to remove **by `{target_issue_id, relation_type}`**, the same
  way `POST` identifies one to create — there is no relation-id-based delete. 404 if no such
  relation exists.

## 8a. Idempotency and retry semantics (N7d)

An unattended agent's standard retry policy — resend on a dropped response, a timeout, a network
blip — will produce genuine duplicate requests. As of N7d, the mutation endpoints below have
natural, built-in guards against the most common shapes of that, so you generally do **not** need
your own client-side idempotency key or dedupe layer for these:

| Endpoint | Retry-safe how |
|---|---|
| `PATCH /api/agent/issues/:id/status` | An exact retry (same `status`, the same `resolved_in_version`, **and the same `snoozed_until` instant and `snooze_occurrences`**) is recognized as a no-op: no second `status_changed` activity row, no repeat notification email. The response gains a `changed` field — `false` on the no-op path, `true` when a real transition happened — so you can tell the two apart if you care. A retry with a *different* `resolved_in_version` or a different snooze condition is treated as a real change, not a no-op (a different `snooze_occurrences` restarts the count from the current `count`; the same one with a new date keeps the count it had). |
| `POST /api/agent/issues/:id/comments` (plain, non-blocking only) | An identical retry (same issue, same author, same `body_md`, within a short window) returns the existing comment instead of inserting a duplicate. **Blocking questions (`POST .../questions`) are deliberately excluded from this** — a question's `waiting_on` side effect must be predictable on every call, so it is never silently skipped. |
| `POST /api/agent/issues/:id/progress` | Same natural-key dedupe as plain comments (same issue, same agent, same `message_md`, within the window). |
| `DELETE /api/agent/issues/:id/claim` | See §4 — releasing an already-unclaimed issue is 200, not 409. |

The comment/progress dedupe window is a fixed, short duration (2 minutes) measured from the
original insert. This means a **deliberate** identical re-post — you genuinely want to say
"Investigating." twice in quick succession — can get silently absorbed into the first one. This is
a known, accepted trade-off (documented in the plan as a risk, not treated as a bug): if you need a
second, distinguishable entry close together, vary the wording slightly, or wait for the window to
pass.

None of this applies across a **process restart with no memory of what you already sent** — these
guards protect against a retry of a request you just made, not against re-deriving the same action
independently after forgetting you already did it. For that, poll the issue's current state
(`GET /api/agent/issues/:id`, `GET /api/agent/issues/:id/comments`) before re-acting rather than
assuming.

## 9. Batch endpoint

`POST /api/agent/batch` folds up to 20 mutations into one HTTP round trip:

```json
{
  "operations": [
    { "op": "issues.claim", "issueId": "iss_1" },
    { "op": "issues.comment", "issueId": "iss_1", "params": { "body_md": "Investigating." } },
    { "op": "issues.status", "issueId": "iss_1", "params": { "status": "resolved" } }
  ],
  "stopOnError": true
}
```

Available `op` values (mutations only — no GETs, no `issues.question.*`, no upload): `issues.status`,
`issues.claim`, `issues.claim.release`, `issues.comment`, `comments.edit`, `comments.delete`,
`issues.report.severity`, `issues.progress`, `issues.relations.add`, `issues.relations.remove`.
Each op's `params` shape is exactly the JSON body its single-route equivalent takes (see §5–§8, and
the per-endpoint reference in §14) — with one exception: `comments.edit`/`comments.delete` also
need `comment_id` in `params` (there's no second `:id` slot in a batch op), since their single-route
form gets it from the URL instead.

**Partial-completion semantics — read this before relying on batch for anything transactional:**

- One `authenticateAgentRequest` call for the whole batch — it counts as **one** request against
  your key's rate limit, not one per op.
- Ops run **sequentially**, each in its own underlying transaction (exactly what N sequential
  single-route calls would give you). **There is no outer transaction spanning the batch.** If op 3
  of 5 fails, ops 1–2 are already committed, full stop — design for that, don't assume atomicity.
- `stopOnError` (default `true`): stop after the first non-ok op; every op after it is reported
  `{ "ok": false, "status": 0, "skipped": true }` without being executed. Set `false` to run every
  op regardless of earlier failures.
- The HTTP response is **always 200** as long as the envelope is well-formed and auth succeeds.
  Per-op outcome lives in the body — never assume "200 means everything succeeded":
  ```json
  {
    "results": [
      { "ok": true, "status": 200, "result": { "success": true, "issue": {...} } },
      { "ok": true, "status": 201, "result": { "comment": {...} } },
      { "ok": false, "status": 409, "error": "Issue already claimed by another agent" }
    ],
    "completed": 2
  }
  ```
  Check every `results[i].ok`, not the batch's own status code.

## 10. Webhooks

Webhooks are how Sentinel **pushes** events to you instead of you polling `/api/agent/events`.

### Registering

Webhook registration is session-authenticated (owner/admin only), done from the dashboard's agent
settings UI or directly:

```
POST /api/organizations/:orgId/agents/:agentId/webhooks
{ "url": "https://your-receiver.example.com/sentinel-webhook", "eventTypes": ["status_changed", "commented"] }
```

`eventTypes` is optional (omit or `[]` for all types, from the same list in §3). The response is
the **only time the signing secret is ever shown**:

```json
{ "webhook": { "id": "wh_...", "url": "...", "eventTypes": [...], "status": "active", ... },
  "secret": "whsec_..." }
```

Store it immediately — it is never re-derivable from the API afterward (list/get responses only
return a `secretPrefix`).

### Delivery

A background dispatcher (`apps/processor-go/webhooks`) polls every active webhook on an interval
(default 5s, `WEBHOOK_DISPATCH_INTERVAL`) and, for each one with events past its cursor, POSTs up
to 100 events per delivery:

```json
{
  "webhookId": "wh_...",
  "agentId": "agt_...",
  "events": [ /* same shape as GET /api/agent/events's "events" array, camelCase */ ],
  "cursor": 42
}
```

### Verifying `X-Sentinel-Signature` — exact recipe

Every delivery carries:

- `X-Sentinel-Signature: t=<unix seconds>,v1=<hex hmac-sha256>`
- `X-Sentinel-Delivery-Id: <uuid>` (unique per delivery attempt; useful for receiver-side dedupe/logging)
- `Content-Type: application/json`

The signed message is **`"<t>." + <raw request body bytes>`**, HMAC-SHA256'd with your webhook's
raw secret, hex-encoded:

```
mac = HMAC-SHA256(secret, ascii(t) + "." + raw_body_bytes)
signature = "t=" + t + ",v1=" + hex(mac)
```

**Worked example** (computed for real — see §12 for runnable node/python versions):

- `secret` = `whsec_example_5f3c9a1b2d4e6f70`
- `t` = `1755000000`
- `body` (exact bytes, single line, no reformatting):
  ```json
  {"webhookId":"wh_01HXAMPLE0000000000000000","agentId":"agt_01HXAMPLE0000000000000001","events":[{"seq":42,"eventType":"status_changed","actorType":"agent","actorId":"agt_01HXAMPLE0000000000000001","oldValue":{"status":"unresolved"},"newValue":{"status":"resolved"},"createdAt":"2026-08-14T12:00:00Z","issue":{"id":"iss_01HXAMPLE0000000000000002","title":"NPE in checkout handler","status":"resolved","issueType":"system_error","projectId":"prj_01HXAMPLE0000000000000003"}}],"cursor":42}
  ```
- expected header value:
  ```
  t=1755000000,v1=0eaae859046e1eaa0b1b11ea58df505c693eb9435e009693ad5da8b608d5e6af
  ```

Compare using constant-time comparison (`hmac.compare_digest` / `crypto.timingSafeEqual`), not `==`.

**Replay window advice:** the header does not itself expire — Sentinel does not enforce a max age.
Receivers should reject deliveries whose `t` is further than a few minutes from their own clock
(e.g. 5 minutes) to bound replay risk, and should dedupe on `X-Sentinel-Delivery-Id` since delivery
is at-least-once (retries on failure, §"failure semantics" below, can resend the same events).

### Failure / auto-disable semantics

- On a non-2xx response (or network error), the dispatcher retries the same delivery **up to 3
  times** with backoff `1s / 5s / 30s` before giving up on that tick.
- A failed delivery increments `consecutive_failures`. Once that streak reaches
  `WEBHOOK_FAILURE_THRESHOLD` (default **20**), the webhook's `status` flips to `failed` and the
  dispatcher stops attempting delivery to it entirely.
- A successful delivery resets nothing about the streak count implicitly during normal operation —
  only re-enabling (below) resets it.

### Resume on re-enable

`PATCH /api/organizations/:orgId/agents/:agentId/webhooks/:webhookId { "status": "active" }` on a
webhook that was `failed` or `disabled` clears `consecutive_failures` and `last_error`, **but
deliberately leaves the delivery cursor (`last_delivered_seq`) untouched.** Re-enabling resumes
delivery exactly where it left off — it does not replay from the beginning and does not skip
straight to "now." Expect a burst of backlog events on the first tick after re-enabling if the
webhook was down for a while.

## 11. The `sentinel` CLI

A zero-dependency Go CLI wrapping `/api/agent/*`, built for scripting triage loops.

**Install** (the binary is named `sentinel`; the version is the deployed dashboard's release tag):
```bash
curl -fsSL https://<your-sentinel-host>/install.sh | sh   # served by the dashboard, sha256-verified, no sudo
sentinel login --url https://<your-sentinel-host>         # prints a code, opens the browser; approve it -> a personal key (§2b)
sentinel agent install                                    # installs the sentinel-agent skill for Claude Code / Codex / agy
```
`sentinel login` needs no admin and works over SSH: open the printed URL on any device where you are signed in. For CI
or an admin-issued org agent key, use `sentinel login --key-stdin`: it reads one line from stdin, or prompts with
echo off on a terminal.
Developers can instead `go install github.com/tuju-ai/sentinel/tools/sentinel@latest` (private repo — needs
`GOPRIVATE=github.com/tuju-ai` and `git config --global url."git@github.com:".insteadOf "https://github.com/"`)
or `cd tools/sentinel && go build -o sentinel .` from a checkout. `sentinel version` prints the version.

**Config — profiles.** `config.json` (`$XDG_CONFIG_HOME/sentinel/`, else `~/.config/sentinel/`, `0600`, written by
`sentinel login`) holds named profiles: `{"current_profile": "production", "profiles": {"production": {"url":
"...", "agent_key": "sent_agent_..."}, "staging": {...}}}` (the old flat `{"url","agent_key"}` file loads as
profile `default`). Profile selection: `--profile P` (global flag) > `SENTINEL_PROFILE` > `current_profile` >
`default`; an unknown profile exits 2. URL and key are never mixed across sources:

| Situation | URL / key used |
|---|---|
| `-url` / `-key` flags | the flag, per field |
| `--profile` / `SENTINEL_PROFILE` | that profile (beats env credentials) |
| `SENTINEL_AGENT_KEY` set, no explicit profile | the pair `SENTINEL_URL` + `SENTINEL_AGENT_KEY` (key without URL = exit 2) |
| `SENTINEL_URL` set without `SENTINEL_AGENT_KEY` | ignored, with a warning naming the profile used |
| otherwise | `current_profile`, else `default` |

`sentinel whoami` / `sentinel config` show the profile, where url and key came from (never the key) and whose key it is
(`identity: personal key of <name> (<role>, read-write|read-only)` or `identity: org agent "<name>"`);
`sentinel profile list|use|remove` manage profiles. The key is never logged.

| Command | HTTP call |
|---|---|
| `sentinel issues list [--type T] [--claimed true\|false] [--project ID] [--waiting true] [--since TS] [--sort firstSeen\|lastSeen] [--limit N] [--cursor C]` | `GET /api/agent/issues` |
| `sentinel issues get <issueId>` (also `sentinel issue show <issueId>`) | `GET /api/agent/issues/:id` |
| `sentinel issue activity <issueId> [--limit N] [--before CURSOR] [--all] [--json]` | `GET /api/agent/issues/:id/activity` — the issue's history as a table (WHEN, WHO, WHAT, DETAIL; a burst run is one line); `--all` follows every page; `--json` prints the response (merged with `--all`); see §3a |
| `sentinel issues occurrences <issueId> [--limit N] [--before TS] [--with-spans]` | `GET /api/agent/issues/:id/occurrences[?hasSpans=true]` — each row carries `spansStatus`/`hasSpans`; see §11a |
| `sentinel spans pull <traceId\|occurrenceId> [--wait] [--timeout 3m]` | `POST /api/agent/traces/:traceId/pull` (an occurrence UUID is first resolved with `GET /api/agent/occurrences/:id`); `--wait` polls `GET /api/agent/traces/:traceId` (`spans`) — see §11a |
| *(no CLI verb yet)* | `GET /api/agent/traces/:traceId[?limit=&cursor=&environment=]` — see §5 step 3 |
| `sentinel claim <issueId>` | `POST /api/agent/issues/:id/claim` (409 on conflict) |
| `sentinel claim-next [--type T] [--project ID] [--environment E] [--source S] [--sort firstSeen\|lastSeen]` | `POST /api/agent/issues/claim-next` — pick + claim + full detail in one call; exit 4 when the queue is empty |
| `sentinel release <issueId>` | `DELETE /api/agent/issues/:id/claim` (own claim only) |
| `sentinel status <issueId> <unresolved\|resolved\|ignored> [--resolved-in VERSION]` | `PATCH /api/agent/issues/:id/status` |
| `sentinel comment <issueId> --body <md> [--attachment <id> ...]` | `POST /api/agent/issues/:id/comments` |
| `sentinel comment edit <issueId> <commentId> --body <md>` | `PATCH /api/agent/issues/:id/comments/:commentId` (own comment only) |
| `sentinel comment delete <issueId> <commentId>` | `DELETE /api/agent/issues/:id/comments/:commentId` (own comment only) |
| `sentinel comments <issueId> [--after <ts>]` | `GET /api/agent/issues/:id/comments` |
| `sentinel question <issueId> --body <md> --waiting-on <reporter\|team>` | `POST /api/agent/issues/:id/questions` |
| `sentinel progress <issueId> --body <md>` | `POST /api/agent/issues/:id/progress` |
| `sentinel severity <issueId> <low\|medium\|high\|critical>` | `PATCH /api/agent/issues/:id/report/severity` (`user_report` only) |
| `sentinel link <issueId> <targetIssueId> --type <linked_to\|caused_by\|duplicate_of>` | `POST /api/agent/issues/:id/relations` |
| `sentinel unlink <issueId> <targetIssueId> --type <...>` | `DELETE /api/agent/issues/:id/relations` |
| `sentinel projects` | `GET /api/agent/projects` |
| `sentinel login [--profile P] [--url U] [--device-name N] [--no-browser] [--use] [--yes]` | browser login (§2b): `POST /api/cli/device`, approve at `/cli/approve`, poll `POST /api/cli/device/token`; then verifies via `GET /api/agent/self` and writes profile P (`0600`); never prints the key |
| `sentinel login --key-stdin [--profile P] [--url U] [--use] [--yes]` | paste an existing key (CI / org agent): verifies via `GET /api/agent/self`, then writes profile P |
| `sentinel logout [--profile P]` | `POST /api/agent/key/revoke` (personal keys; an org agent key gets 403 and stays valid), then removes the profile |
| `sentinel profile list` / `use <name>` / `remove <name>` | *(local)* manage profiles |
| `sentinel config` | *(local)* profile in use + where url / key come from; the key itself is never shown |
| `sentinel agent install [--claude] [--codex] [--agy] [--dry-run] [--force] [--uninstall]` | *(local)* installs the embedded sentinel-agent skill globally for each agent tool |
| `sentinel version`, `sentinel guide` | *(local)* version; prints this guide |
| `sentinel help [command]`, `sentinel <command> --help` | *(local)* a command's usage; `-h`/`--help` anywhere prints it and runs nothing |
| `sentinel whoami` | `GET /api/agent/self` — real identity as of N7f (R1a), see below |
| `sentinel key rotate` | `POST /api/agent/key/rotate` — new secret printed to stdout ONCE (N7f, R1b) |
| `sentinel events [--after N] [--limit N] [--type T] [--project ID] [--claimed-me]` | `GET /api/agent/events` (one page) |
| `sentinel events --follow [--interval SEC]` | polls `GET /api/agent/events`, NDJSON to stdout, cursor persisted |
| `sentinel batch -f ops.json\|- [--stop-on-error=false]` | `POST /api/agent/batch` |
| `sentinel upload <file> --issue <id> [--comment <text>]` | upload + comment-with-attachment in one call (N7f, A15) |
| `sentinel upload <issueId> <file>` | deprecated two-positional form — plain upload, `issueId` ignored, warns (see below) |

Global flags (`--profile`, `-url`, `-key`, `-format json|table`) go before the subcommand. `-format table`
renders list-shaped responses as a column table. `issues` and `issue` are the same command.

**Ids are checked before any request.** Every issue, comment, attachment and target id argument must be a UUID
(either case); anything else — a flag in the id's place, a truncated id — is a usage error (exit `2`, nothing sent),
with a message naming the argument. `members remove` also accepts a user id, so it only refuses a flag-looking value.

**Exit codes:** `0` ok (including `--help`) · `1` network/5xx · `2` usage error, including an invalid id (no request made) · `3` auth failure
(401/403) · `4` not found (404) · `5` conflict (409) · `6` validation error (400/422).

**Known CLI/server mismatches** (server wins in every case — see `tools/sentinel/README.md`
for the full explanation):
- `comment --parent <id>` is accepted and sent, but the server-side comment op ignores it — there
  is no threading support in the agent comment path today.
- `unlink` takes `<issueId> <targetIssueId> --type>`, not a relation id — there is no relation-id
  delete endpoint.
- `upload <issueId> <file>` (the old, deprecated two-positional form) — `POST /api/agent/uploads`
  does not take an `issueId` at all; this form does a plain upload with no issue association. Use
  `upload <file> --issue <id> [--comment <text>]` instead, which chains the follow-up
  `comment --attachment <id>` call for you.

## 11a. Spans: which occurrences have them, and pulling a trace on demand

Only a small share of traced occurrences have spans: the automatic pull is throttled to one trace per issue per window.

- **Status.** Every occurrence from `GET /api/agent/issues/:id/occurrences` and `GET /api/agent/traces/:traceId` carries
  `spansStatus` — `available` (spans you can open exist), `pending` (a pull is queued or running) or `none` — and
  `hasSpans` (`spansStatus == "available"`). `spansStatus` is `null` when the occurrence has no canonical 32-hex trace id.
  `GET /api/agent/traces/:traceId` also returns `spans: {status, reason}`; for `none`, `reason` is `not_pulled`,
  `not_found` (the trace store has no such trace), `too_large`, `failed`, `not_allowed` or `not_visible`.
- **Filter.** `?hasSpans=true` (CLI `--with-spans`) keeps only occurrences with spans. It walks the issue's newest 2,000
  occurrences, so the response adds `spansScan: {scanned, exhausted}` and, when older ones may remain, `nextBefore`
  (pass it back as `before`).
- **Pull now.** `POST /api/agent/traces/:traceId/pull` (CLI `sentinel spans pull <traceId|occurrenceId> [--wait]`) asks the
  server to fetch the trace's spans immediately. Idempotent; returns `{status, reason, action, requestId}`. It works only
  for organizations allow-listed for span pull (`403` otherwise), needs an occurrence of that trace in your organization
  (`404`), and is limited per organization (default 30 accepted pulls per hour; `429` + `Retry-After`). That budget is **shared by every person and every agent of the organization**, so one busy agent can use up what the dashboard users would have. A trace already marked too large is returned as such and costs nothing. `--wait` polls every
  5 s up to `--timeout` (default 3m): exit `0` when available, `4` when the pull finished with no spans (the reason is
  printed), `1` on timeout.

## 12. Runnable examples

See `docs/agents/examples/`:
- `triage-batch.json` — a valid `POST /api/agent/batch` body.
- `triage-loop.sh` — a POSIX shell triage loop using the CLI (`events --follow` → claim → get →
  comment → status/question).
- `webhook-receiver.md` — ~20-line Node and Python signature-verifying receivers, matching the
  recipe in §10 exactly.

## 13. Identity: `GET /api/agent/self`

R1a (`docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md` N7f) closed what used to be a real gap:
`authenticateAgentRequest` resolves an `AgentAuthContext` (agent id, org id, agent name, plus the
key row) server-side for every request, and `GET /api/agent/self` now echoes exactly that back:

```json
{
  "agentId": "...",
  "name": "Triage Bot",
  "organizationId": "...",
  "kind": "personal",
  "owner": { "userId": "...", "name": "Ada", "email": "ada@example.com", "role": "engineer", "canWrite": true },
  "key": {
    "id": "...",
    "prefix": "sent_agent_",
    "createdAt": "2026-08-01T00:00:00.000Z",
    "expiresAt": null,
    "lastUsedAt": null
  }
}
```

`kind` is `ai`/`bot` for an org agent and `personal` for a `sentinel login` key. `owner` is `null` for an org
agent. For a personal key it is the person the key acts for, with their role **as of this request** and whether that
role may write (§2b).

`key.createdAt` (N9) is the key's mint time, ISO-8601 or `null` for keys created before the column
was populated. `key.expiresAt` is the enforced expiry (`null` = non-expiring); once it passes, the
key stops authenticating and every `/api/agent/*` call — including `/self` — returns `401`. Together
they let an unattended agent do **age-based rotation without guessing**: rotate proactively when
`expiresAt` is near, or when `now - createdAt` exceeds your own policy for keys that never expire.
Rotate via `POST /api/agent/key/rotate` (§2) and the new key inherits the old key's lifetime, so the
schedule is steady-state rather than one-shot.

`key.lastUsedAt` is always `null` — `project_api_keys` tracks no such column; it's kept in the
shape so a future column addition wouldn't be a breaking response-shape change. Use this to
confirm which agent/org a key resolves to (e.g. at the start of an unattended loop, or after
rotating — see §2), instead of the old CLI-side reachability probe against `/api/agent/issues`.

## 13a. Repo credentials: `GET /api/agent/repo-credentials` (N10)

Server-held git credentials for the fix worker (push branches, open PRs). Not part of the normal
triage surface — most agents never call this and never can:

- **403 by default.** A valid agent key is not enough; an org owner/admin must enable **Repo
  credential access** on your agent card (`agents.can_access_repo_credentials`). Revocation is
  immediate — the flag is re-read on every request.
- **503** means the server has no `SENTINEL_ENCRYPTION_KEY` configured and refuses to serve
  credentials; that is an operator problem, not yours — do not retry-loop it aggressively.
- The response is the only place the decrypted secret ever appears. `Cache-Control: no-store`;
  hold it in memory only — never journal, snapshot, or log it.
- Every credential you fetch is audited (your agent id, the credential id, timestamp).

```json
{
  "credentials": [
    { "id": "...", "provider": "github", "label": "CI fix bot", "secret": { "token": "ghp_..." } },
    { "id": "...", "provider": "bitbucket", "label": "BB", "secret": { "username": "devbot", "appPassword": "..." } }
  ]
}
```

`secret` is either `{token}` (GitHub PAT or Bitbucket access token) or `{username, appPassword}`
(Bitbucket app password pair). Revoked credentials disappear from the list; a `401`/empty-list
transition mid-run means re-fetch before your next push, not "reuse the cached one forever".

## 14. Raw-curl appendix

Set once:
```bash
export SENTINEL_URL=https://sentinel.example.com
export SENTINEL_AGENT_KEY=sent_agent_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
AUTH="Authorization: Bearer $SENTINEL_AGENT_KEY"
```

```bash
# Identity
curl -s "$SENTINEL_URL/api/agent/self" -H "$AUTH"

# Rotate this key (prints the new secret ONCE — capture it, don't just print-and-discard)
curl -s -X POST "$SENTINEL_URL/api/agent/key/rotate" -H "$AUTH"

# Repo credentials (403 unless an admin granted your agent repo-credential access — §13a)
curl -s "$SENTINEL_URL/api/agent/repo-credentials" -H "$AUTH"

# List issues
curl -s "$SENTINEL_URL/api/agent/issues?type=system_error&claimed=false&waiting=false" -H "$AUTH"

# Issue detail
curl -s "$SENTINEL_URL/api/agent/issues/$ISSUE_ID" -H "$AUTH"

# Occurrences (paged, newest-first)
curl -s "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/occurrences?limit=20" -H "$AUTH"

# The issue's activity history (newest first, bursts collapsed; pass nextCursor back as before=)
curl -s "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/activity?limit=50" -H "$AUTH"

# Claim / release
curl -s -X POST "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/claim" -H "$AUTH"
curl -s -X DELETE "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/claim" -H "$AUTH"

# Claim the next unclaimed unresolved issue and get its detail in one call (404 = queue empty)
curl -s -X POST "$SENTINEL_URL/api/agent/issues/claim-next" -H "$AUTH" -H "Content-Type: application/json" \
  -d '{"type":"system_error"}'

# Status
curl -s -X PATCH "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/status" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"status":"resolved","resolved_in_version":"v1.2.3"}'
# ... in the next release, whichever it turns out to be (§5 step 6)
curl -s -X PATCH "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/status" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"status":"resolved","resolved_in_version":"next"}'
# ... snooze: ignore until an instant and/or until N more occurrences (§5 step 6)
curl -s -X PATCH "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/status" -H "$AUTH" \
  -H 'Content-Type: application/json' \
  -d '{"status":"ignored","snoozed_until":"2026-09-20T09:00:00Z","snooze_occurrences":100}'

# Comment
curl -s -X POST "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/comments" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"body_md":"Root cause: null pointer in checkout handler."}'

# List comments since a timestamp
curl -s "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/comments?after=2026-08-14T00:00:00Z" -H "$AUTH"

# Blocking question
curl -s -X POST "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/questions" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"body_md":"What request payload triggered this?","audience":"reporter"}'

# Progress update (no email)
curl -s -X POST "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/progress" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"message_md":"Bisecting the release history now."}'

# Relations
curl -s -X POST "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/relations" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"target_issue_id":"'"$OTHER_ID"'","relation_type":"duplicate_of"}'
curl -s -X DELETE "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/relations" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"target_issue_id":"'"$OTHER_ID"'","relation_type":"duplicate_of"}'

# Projects
curl -s "$SENTINEL_URL/api/agent/projects" -H "$AUTH"

# Events feed
curl -s "$SENTINEL_URL/api/agent/events?after=0&limit=50" -H "$AUTH"

# Upload an attachment (multipart — see field name in upload-core.ts if it changes)
curl -s -X POST "$SENTINEL_URL/api/agent/uploads" -H "$AUTH" -F "file=@./screenshot.png"

# Batch
curl -s -X POST "$SENTINEL_URL/api/agent/batch" -H "$AUTH" \
  -H 'Content-Type: application/json' --data-binary @docs/agents/examples/triage-batch.json
```

---

## 15. Changelog

Wire-visible changes to `/api/agent/*`, newest first. Only additions and behaviour changes a
deployed agent can observe are listed here; see `docs/agents/openapi.agent.yaml` for the exact
shapes.

### Issue activity history and malformed ids (sentinel-issue-activity)

- **New `GET /api/agent/issues/:id/activity?limit=&before=`** (§3a): one issue's history, newest first, keyset-paged
  by an opaque `nextCursor`, runs of consecutive `occurrence_burst` rows collapsed into one entry with `burst`, actors
  resolved to names. Authorized like `GET /api/agent/issues/:id` (personal keys scoped to the owner, `404` out of
  scope). Not available as a `/api/agent/batch` op (batch ops are mutations).
- **Malformed ids are `404`, never `500`:** a path id that is not a UUID (`/api/agent/issues/--help`,
  `/api/agent/issues/42a26f64-`) matches no route and answers `404 {"message":"Not found"}` without authenticating.
  Every well-formed request is unchanged.
- **CLI:** new `sentinel issue activity`; `issue` is an alias of `issues` and `show` of `get`; `-h`/`--help` and
  `sentinel help [command]` print usage and exit `0` (a command without flags used to send `--help` as the issue id);
  every id argument is validated as a UUID before any request (exit `2`).

### Personal keys and browser login (sentinel-cli-browser-login)

- **`GET /api/agent/self`** additively returns `kind` and `owner` (null for org agents). The schema stays strict.
- **New `POST /api/agent/key/revoke`** revokes the calling personal key (`sentinel logout`). An org agent key gets
  `403` and stays valid.
- **`POST /api/agent/key/rotate` on a personal key** revokes the calling key at once (no
  `AGENT_KEY_ROTATION_GRACE_HOURS` window, so `oldKey.expiresAt` is the revocation time) and the new key keeps the
  old key's expiry. Org agent keys are unchanged.
- **`GET /api/agent/events` for a personal key:** a `linked` event whose target issue the owner cannot see has
  `newValue.targetIssueId: null` and `newValue.targetRedacted: true`, as the issue page redacts the relation.
- **Personal keys** (`kind: "personal"`, minted by `sentinel login`) are scoped to their owner on every route (§2b):
  filtered lists, `404` outside the owner's projects and environment grants, `403` on mutations for a read-only role,
  and `403` on `members` and `repo-credentials`. **Org agent keys behave exactly as before.**
- **New unauthenticated dashboard routes** for the CLI: `POST /api/cli/device` and `POST /api/cli/device/token`,
  plus the `/cli/approve` page.
- **CLI:** `sentinel login` without `--key-stdin` now runs the browser flow (including on a non-terminal). The paste
  flow needs `--key-stdin`. New: `--device-name`, `--no-browser`, `sentinel logout`, and identity lines in
  `whoami`/`config`.
- **Admin routes:** rotating an agent key through `/api/organizations/{org}/keys/{id}/rotate` now needs owner/admin
  (it used to accept `manage_keys`, so an engineer could take an org agent's new secret). A personal key is never
  rotated there.

### `sentinel` CLI distribution (sentinel-cli-install)

- **No `/api/agent/*` wire change.** The CLI module moved `tools/sentinel-cli` → `tools/sentinel`
  (module `github.com/tuju-ai/sentinel/tools/sentinel`), so the binary is `sentinel`, not `sentinel-cli`.
- **New unauthenticated dashboard routes:** `GET /install.sh`, `GET /cli/download/<os>/<arch>`
  (`linux|darwin` × `amd64|arm64`), `GET /cli/checksums.txt`. The binaries carry the dashboard's release tag.
- **New CLI commands:** `version`/`--version`, `login`, `config`, `profile`, `agent install`, `guide`; config is now
  named **profiles** (`--profile` / `SENTINEL_PROFILE`), and a lone `SENTINEL_URL` no longer overrides the file.
  `GET /api/agent/self` additively returns `cliProfile` (the deployment's suggested profile name) when configured. The HTTP
  `User-Agent` is now `sentinel/<version>` (it was Go's default; the server does not key on it).

### `POST /api/agent/issues/claim-next` (sentinel-agent-claim-next-issue)

- **New route.** Atomically selects the next unclaimed, unresolved issue in the calling key's org,
  claims it for the calling agent, and returns the `GET /api/agent/issues/{issueId}` body in one
  round trip (§5 step 2). Optional JSON body: `type`, `project`, `environment`, `source`, `sort`.
  `404` when nothing matches. Concurrent callers never receive the same issue. Records the same
  `claimed` activity row, claimant subscription, notification and `agent.issue.claimed` audit
  entry a single-issue claim does. Additive — nothing existing changed. CLI: `sentinel claim-next`.

### Releases, snooze and `resolved_in_version: "next"` (sentinel-releases-snooze)

- **Two optional request keys on `PATCH /api/agent/issues/{issueId}/status`** (and the batch op
  `issues.status`): `snoozed_until` and `snooze_occurrences`, both only with `status: "ignored"`.
  See §5 step 6 for the rules and the four `400` strings. Additive: a body without them is
  byte-identical in effect to before, except that the retry no-op now also compares the two keys.
- **`resolved_in_version: "next"` means something.** The field accepts what it always accepted; this
  one value is now rewritten to the first release the project sees after the resolve (§5 step 6).
  Any consumer reading `resolvedInVersion` back should expect the string `next` as a possible value.
- **Four new nullable fields on every full issue row** (`GET /api/agent/issues/{issueId}`, the
  claim/release responses): `snoozedUntil`, `snoozeOccurrenceTarget`, `snoozeCountAt`,
  `firstReleaseVersion` — `null` when unset, never absent. `GET /api/agent/issues` rows are
  unchanged.
- **A snooze ending is a `status_changed` event** on `GET /api/agent/events` whose `new_value`
  carries `reason`; no new event type. `new_value.snoozedUntil`, from any writer, is the UTC
  instant with `Z`.
- **Behaviour change, no request change: ignored issues stop paging.** Occurrences of an ignored
  issue (snoozed or plain, including issues ignored before this shipped) no longer reach alert
  configs, and the dispatcher's frequency window is not fed during a snooze. A snooze ending sends
  no notification.

### `affectedUserCount` / `affectedTenantCount` (sentinel-affected-users, decision 14)

- **Two new integer fields on every issue shape.** `affectedUserCount` and `affectedTenantCount`
  appear on the issue object returned by `GET /api/agent/issues/{issueId}`,
  `POST /api/agent/issues/{issueId}/claim` and `DELETE /api/agent/issues/{issueId}/claim`, and
  on every row of `GET /api/agent/issues`. Each is an integer `>= 0` and is never `null`.
  Additive: nothing existing changes shape or meaning.
- **What they count.** The number of **distinct** `user_id` / `tenant_id` values (as the SDK put
  them in event metadata — `WithUser` / `WithTenant`) seen on this issue's occurrences **within
  the retention window**, per environment (an issue already belongs to exactly one). They are
  exact as of the last committed event: an occurrence from a user already counted moves nothing.
- **They can go down.** The hourly retention sweep drops users and tenants whose newest
  occurrence has aged out of the window and recomputes both counts from what remains, so a value
  read now may be lower later. Treat them as "in the last N days", not lifetime totals.
- **`0` is a real value, not "unknown".** An issue whose occurrences carry no `user_id` /
  `tenant_id` (or only unusable ones — empty, over 255 bytes, or containing control characters,
  which the processor drops rather than stores) reads `0`. Issues that predate this field also
  read `0`; there is no backfill.
- **No sort on this surface.** `GET /api/agent/issues?sort=` still accepts only `firstSeen` and
  `lastSeen`; rank by these fields client-side if you want blast radius first.

### `environment` (sentinel-project-environments, decision 54)

- **`environment` is now a declared field on an issue.** It appears on the issue object returned by
  `GET /api/agent/issues/{issueId}`, `POST /api/agent/issues/{issueId}/claim` and
  `DELETE /api/agent/issues/{issueId}/claim`, and on every row of `GET /api/agent/issues`. It is a
  non-empty string and is never null.
- **The result set of `GET /api/agent/issues` changes.** An environment is part of an issue's
  identity, so the same error seen in two environments is now **two** issues — two rows, each
  independently claimable. `environment` is the only field that tells two such siblings apart.
- **`GET /api/agent/issues?environment=<name>` filters the list** by exact, case-sensitive match.
  Omitting it returns every environment, byte-for-byte the previous behaviour. An unrecognised name
  is not an error; it returns an empty list.
