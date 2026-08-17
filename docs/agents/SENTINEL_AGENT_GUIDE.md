# Sentinel Agent Guide

This is the canonical, provider-agnostic guide for any AI agent, script, or bot that operates
**on** Sentinel (the error-tracking product) via `/api/agent/*`. It applies equally to Claude,
GPT, a cron job, or a shell script — nothing here assumes a particular agent framework or model.

If you are looking for guidance on *contributing to Sentinel's own codebase*, see the repo root
`AGENTS.md` / `CLAUDE.md` instead. This guide is about triaging issues as a registered agent
identity against a running Sentinel instance.

Everything in this guide is verified against the route handlers under
`apps/dashboard-web/src/routes/api/agent/**`, `$lib/server/agent-ops.ts`, `$lib/server/agent-events.ts`,
`apps/processor-go/webhooks/dispatcher.go`, and `tools/sentinel-cli/`. Where this guide and any other
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
      "issue": { "id": "iss_...", "title": "...", "status": "resolved", "issueType": "system_error", "projectId": "prj_..." }
    }
  ],
  "cursor": 42,
  "hasMore": false
}
```

Valid `eventType` values (the full `issue_activity.event_type` set): `status_changed`, `assigned`,
`unassigned`, `regressed`, `ai_analysis`, `linked`, `commented`, `claimed`, `claim_released`,
`progress_update`, `question_asked`, `question_answered`, `moved`, `attachment_added`,
`report_edited`, `report_created`, `created`, `occurrence_burst`.

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
   stays stable even as new issues arrive mid-page.
2. **Claim** — `POST /api/agent/issues/:id/claim`. Stop here (409) if someone beat you to it.
3. **Get full detail** — `GET /api/agent/issues/:id` returns:
   ```json
   {
     "issue": { "id": "...", "issueType": "system_error", "status": "unresolved", "message": "...", "waitingOn": null, ... },
     "report": null,
     "latestOccurrence": { "stacktrace": {...}, "metadata": {...}, "environment": "...", "platform": "...", "traceId": "...", "createdAt": "..." },
     "relations": [ ... ]
   }
   ```
   - For `issueType: "system_error"`: `report` is `null`; read `latestOccurrence.stacktrace` (and
     `.metadata`, `.traceId`) for the crash detail. Page further back with
     `GET /api/agent/issues/:id/occurrences?limit=&before=` (newest-first, `limit` clamped to
     `[1, 50]`, default 20; `before` is an exclusive ISO-timestamp cursor).
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
| `PATCH /api/agent/issues/:id/status` | An exact retry (same `status` **and** the same `resolved_in_version`) is recognized as a no-op: no second `status_changed` activity row, no repeat notification email. The response gains a `changed` field — `false` on the no-op path, `true` when a real transition happened — so you can tell the two apart if you care. A retry with a *different* `resolved_in_version` is treated as a real change, not a no-op. |
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

**Install:**
```bash
go install github.com/NurfitraPujo/sentinel/tools/sentinel-cli@latest
# or, from a checkout:
cd tools/sentinel-cli && go build -o sentinel .
```

**Config** (highest priority first): `-url`/`-key` flags → `SENTINEL_URL`/`SENTINEL_AGENT_KEY` env
vars → `$XDG_CONFIG_HOME/sentinel/config.json` (falls back to `~/.config/sentinel/config.json`,
`{"url": "...", "agent_key": "sent_agent_..."}`, should be `chmod 600`). The key is never logged.

| Command | HTTP call |
|---|---|
| `sentinel issues list [--type T] [--claimed true\|false] [--project ID] [--waiting true] [--since TS] [--sort firstSeen\|lastSeen] [--limit N] [--cursor C]` | `GET /api/agent/issues` |
| `sentinel issues get <issueId>` | `GET /api/agent/issues/:id` |
| `sentinel issues occurrences <issueId> [--limit N] [--before TS]` | `GET /api/agent/issues/:id/occurrences` |
| `sentinel claim <issueId>` | `POST /api/agent/issues/:id/claim` (409 on conflict) |
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
| `sentinel whoami` | `GET /api/agent/self` — real identity as of N7f (R1a), see below |
| `sentinel key rotate` | `POST /api/agent/key/rotate` — new secret printed to stdout ONCE (N7f, R1b) |
| `sentinel events [--after N] [--limit N] [--type T] [--project ID] [--claimed-me]` | `GET /api/agent/events` (one page) |
| `sentinel events --follow [--interval SEC]` | polls `GET /api/agent/events`, NDJSON to stdout, cursor persisted |
| `sentinel batch -f ops.json\|- [--stop-on-error=false]` | `POST /api/agent/batch` |
| `sentinel upload <file> --issue <id> [--comment <text>]` | upload + comment-with-attachment in one call (N7f, A15) |
| `sentinel upload <issueId> <file>` | deprecated two-positional form — plain upload, `issueId` ignored, warns (see below) |

Global flags (`-url`, `-key`, `-format json|table`) go before the subcommand. `-format table`
renders list-shaped responses as a column table.

**Exit codes:** `0` ok · `1` network/5xx · `2` usage error (no request made) · `3` auth failure
(401/403) · `4` not found (404) · `5` conflict (409) · `6` validation error (400/422).

**Known CLI/server mismatches** (server wins in every case — see `tools/sentinel-cli/README.md`
for the full explanation):
- `comment --parent <id>` is accepted and sent, but the server-side comment op ignores it — there
  is no threading support in the agent comment path today.
- `unlink` takes `<issueId> <targetIssueId> --type>`, not a relation id — there is no relation-id
  delete endpoint.
- `upload <issueId> <file>` (the old, deprecated two-positional form) — `POST /api/agent/uploads`
  does not take an `issueId` at all; this form does a plain upload with no issue association. Use
  `upload <file> --issue <id> [--comment <text>]` instead, which chains the follow-up
  `comment --attachment <id>` call for you.

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
  "key": { "id": "...", "prefix": "sent_agent_", "expiresAt": null, "lastUsedAt": null }
}
```

`key.lastUsedAt` is always `null` — `project_api_keys` tracks no such column; it's kept in the
shape so a future column addition wouldn't be a breaking response-shape change. Use this to
confirm which agent/org a key resolves to (e.g. at the start of an unattended loop, or after
rotating — see §2), instead of the old CLI-side reachability probe against `/api/agent/issues`.

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

# List issues
curl -s "$SENTINEL_URL/api/agent/issues?type=system_error&claimed=false&waiting=false" -H "$AUTH"

# Issue detail
curl -s "$SENTINEL_URL/api/agent/issues/$ISSUE_ID" -H "$AUTH"

# Occurrences (paged, newest-first)
curl -s "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/occurrences?limit=20" -H "$AUTH"

# Claim / release
curl -s -X POST "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/claim" -H "$AUTH"
curl -s -X DELETE "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/claim" -H "$AUTH"

# Status
curl -s -X PATCH "$SENTINEL_URL/api/agent/issues/$ISSUE_ID/status" -H "$AUTH" \
  -H 'Content-Type: application/json' -d '{"status":"resolved","resolved_in_version":"v1.2.3"}'

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
