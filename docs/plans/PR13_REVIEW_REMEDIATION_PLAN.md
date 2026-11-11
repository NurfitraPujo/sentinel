# PR #13 Review Remediation Plan

Status: **DONE 2026-08-12** — all R1–R20 implemented and verified (red-first per defect; gates + full
e2e green; targeted integration proofs for R1/R2/R6/R11 against real Postgres+MinIO). Two accepted
residuals: R13's Go test asserts the predicate constant rather than a DB-level alert proof, and the
M5 agent e2e test remains env-gated (skips by default, runs in CI/with env). Notable process finding:
a mid-workflow implementor falsely reported R13 complete (package didn't compile); the Opus validation
layer caught it and forced a real fix — recorded here because it is this repo's characteristic failure
(status recorded optimistically) appearing inside an AI workflow.

Original plan follows.

---

(plan authored 2026-08-12 from the three-axis review of `main...feat/manual-issues`)
Source review: Standards axis, Spec axis (vs `MANUAL_ISSUES_DESIGN.md`), and a defect hunt — findings
below carry stable IDs `R1`–`R20`. Reference these IDs in commits. Acceptance bar for every fix that is
a defect: **prove it red first** (a test that fails against the current code), then green.

## Defects (fix on this branch, pre-merge)

| ID | Finding | Fix | Proof |
|---|---|---|---|
| R1 | **Blocker.** Ex-members keep receiving notifications/emails: fan-out never re-checks org membership, and removing a member leaves their `issue_subscriptions` rows | Filter fan-out targets in `notifyIssueEvent` against current org membership (join through issue→project→org_members), AND delete a user's subscriptions for that org's issues when they are removed (`removeMember` path) | Test: subscribe → remove member → mutate → no notification row, no email |
| R2 | **Major.** Triage inbox race: SELECT-then-INSERT with no uniqueness → two concurrent creates make two inboxes | New migration: partial unique index `projects(organization_id) WHERE is_inbox`; `findOrCreateTriageProject` uses `onConflictDoNothing` + re-select | Migration replay + concurrent-create test (or unit proof of the conflict path) |
| R3 | **Major.** Non-ISO-8859-1 filename (e.g. `スクショ.png`) makes `Content-Disposition` `headers.set` throw → permanent 500 on download; trailing `\` malforms the header | RFC 5987/6266: ASCII fallback + `filename*=UTF-8''<pct-encoded>`; strip `\` | Unit test with unicode + backslash filenames (red first) |
| R4 | **Major.** Agent claim on a `system_error` issue can never be force-released by a human (session release 404s on non-reports) | Session claim DELETE route dispatches access per issue type (reuse the download route's dispatch); force still owner/admin | Test: agent claims system_error → owner force-release succeeds |
| R5 | **Major.** Email throttle counts notification rows incl. throttled attempts → sub-15-min cadence emails exactly once ever | Track actual sends: `notifications.emailed_at timestamptz NULL` (same new migration); throttle = "an `emailed_at` within 15 min", set post-send | Unit test: events at t0/t10/t20 → emails at t0 and t20 (red first) |
| R6 | **Major.** Issue delete / retention cron cascades attachment rows away without deleting MinIO objects → invisible orphans forever | Retention/delete paths collect `storage_key`s BEFORE delete and best-effort-delete objects after commit (pattern already in `deleteComment`) | Test: delete issue with attachment → object gone (integration) or storage.deleteObject asserted (unit) |
| R7 | **Minor.** `waiting_on` never cleared on resolve → resolved issues stuck in "Needs input" tab | Clear `waitingOn` in `updateIssueStatus` when status becomes `resolved`/`ignored` AND exclude resolved from the needs-input tab filter | Unit test both halves |
| R8 | **Minor.** Chunked/invalid `Content-Length` bodies fully buffered before size check → memory abuse | Stream-limit: read the multipart body through a byte-counting guard (or reject missing/NaN Content-Length ≥ cap before `formData()`) so >25 MB aborts without full buffering | Unit test: NaN/absent Content-Length handled; oversized aborts |
| R9 | **Minor.** Comment edits/deletes never reach pollers (`after` filters `createdAt` only) | `listComments(after)` also returns threads whose `editedAt > after`; poll merge replaces changed threads; deletions detected via returned thread (root delete cascades — include a `deletedIds` or re-send parent thread) — pick the simplest correct contract and test it | Unit test: edit-only and delete-only changes propagate through the after-filter + merge |
| R10 | **Improvement.** Actor matching by id only | Add `uploaderType` to draft-attachment checks and `assigneeType` to `releaseClaim`'s conditional UPDATE | Existing tests extended |

## Spec gaps

| ID | Finding | Fix |
|---|---|---|
| R11 | §9 grants author **edit/delete of own report body until resolved** — no path exists | `PATCH/DELETE /api/organizations/[orgId]/reports/[issueId]` (author-only, blocked once resolved; owner/admin may delete): PATCH updates `body_md`/`severity`/title with a real `report_edited` activity; DELETE removes the issue (cascades; MinIO objects cleaned per R6). Edit UI on the detail page for the author |
| R12 | `report_edited` activity mislabels **creation** | New event type `report_created` (CHECK swap in the new migration, catalog-guarded); `createManualIssue` writes it; timeline labels it "created"; `report_edited` reserved for R11 edits |
| R13 | §10 alert-evaluation `system_error` filter only structural | Add the explicit `issue_type = 'system_error'` predicate to the processor's alert-evaluation query (apps/processor-go) + a package-scoped Go test proving a `user_report` row never alerts. Do NOT run repo-wide go build/test — scope to that package |
| R14 | Project-scoped admins get `manage_agents` via the shared `ROLE_PERMISSIONS.admin` entry | Split org-admin vs project-admin permission sets so `manage_agents` is org-level only; keep behavior for org admins; test the negative |

Accepted as-is (documented, no change): `/api/agent/uploads` parallel endpoint, `GET /api/issues/[issueId]/activity`, inbox placeholder credential (§2 approximation — never surfaced, unusable at ingest).

## Standards / hardening

| ID | Finding | Fix |
|---|---|---|
| R15 | `tx: any` across query modules | Export a `Tx` type (Drizzle transaction type) from `$lib/server/db` and use it in reports/comments/notify/subscriptions signatures |
| R16 | 7 duplicated agent-route preambles | `withAgentIssue(handler)` helper in `$lib/server/agent-route.ts`: auth → issueId guard → scope resolve → handler → audit + error mapping; migrate all `/api/agent/issues/[issueId]/*` routes onto it |
| R17 | Duplicated per-issue-type access dispatch | Extract `requireIssueReadAccessAnyType(userId, issueId)` (and write variant) used by attachments route, comments access, and R4's release dispatch |
| R18 | Mixed `body_md`/`attachmentIds` casing in agent comments API | Standardize the agent API bodies on snake_case (`body_md`, `attachment_ids`, `message_md`…), accepting the old camel key for `attachmentIds` is NOT needed (no external consumers yet — clean break); update the e2e test |
| R19 | Agent surface unthrottled | Enforce `project_api_keys.rate_limit_rpm` on `/api/agent/*` in `authenticateAgentRequest` using the existing `$lib/rate-limit` machinery keyed by key hash; 429 with Retry-After; unit tests |
| R20 | Missing hot-path indexes | Same new migration: `issues(assigned_to)` and partial `issues(waiting_on) WHERE waiting_on IS NOT NULL`, plus anything the tab queries need per EXPLAIN sanity check |

## Delivery

One migration file (`1723000000_pr13_remediation.sql`, idempotent, guarded, symmetric Down) carries all
schema changes: R2 partial unique index, R5 `emailed_at`, R12 CHECK swap adding `report_created`,
R20 indexes. `schema.ts` mirrored; drift test green.

Stages (Sonnet implements, Opus validates each, Fable holistic review at the end):
- **Stage A — schema + subscriptions/notifications**: migration, R1, R2, R5, R20
- **Stage B — server defects**: R3, R4, R6, R7, R8, R9, R10
- **Stage C — spec + standards**: R11, R12, R13 (scoped Go), R14, R15–R19
- **Stage D — gates & e2e**: db-migrations go test, drift test (`SCHEMA_DRIFT_REQUIRED=1`), all three
  dashboard gates shuffled, compose stack + full e2e (76+/0 skips), targeted integration proofs for
  R1/R2/R6, processor package test for R13. No repo-wide `go build ./...`/`go test ./...`.

Done = every R-ID checked off with its proof named in the commit message, all gates green, e2e stable.
