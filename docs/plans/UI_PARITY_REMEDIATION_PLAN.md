# UI Parity Remediation Plan — Making the Five Parity Features Actually Work

**Drafted**: 2026-08-01 · **Baseline commit**: `b9e2018` (branch `main`, clean tree)
**Evidence base**: five independent code reviews of commits `dc359cb`, `5639e64`, `f8d66ac`, `b3ccde9`,
`49c0307`, `b9e2018`, plus a measured baseline (`pnpm check`, `pnpm test`) and two defects reproduced
against the live stack.

> [!NOTE]
> **STATUS: COMPLETE — merged 2026-08-01 as PR #11 → `b895df1`.** All 47 findings are closed except the
> items explicitly listed as deliberately open in §9 and in `VERIFIED_STATE.md`'s "UI parity remediation"
> entry. Gates at the merge commit: `pnpm check` 1024 files / 0 errors / 0 warnings · `pnpm build` pass ·
> `pnpm test` 251 passed (order-independent across 8 shuffle seeds) · `go test ./tests/unit/...` 308
> passed · `SENTINEL_E2E=1 go test -tags=e2e ./tests/e2e/` 76 passed · **9/9 CI check runs green on `main`** (8 jobs; `go-sdk` is a 2-leg matrix) —
> the first green CI run this repository has had on `main`.
>
> This document is now **history plus rationale**, not a work queue. The durable lessons were promoted
> into `docs/memory/`: B10's addendum, B12, B13, decisions D17 and D18, and the dated
> `VERIFIED_STATE.md` entry. Read those first; come here for the per-finding detail.

> [!IMPORTANT]
> This plan exists because the parity audit's five gaps were closed by commits that **merge cleanly, pass
> their own tests, and do not run**. Three of the five features are dead at runtime. This is the repo's
> documented characteristic failure (B3: "passing package tests have repeatedly coexisted with entirely
> unreachable code") reproducing itself one layer up, in the dashboard.
>
> Every work item below ends in **an acceptance command that must pass**. An item is not done when the code
> is written; it is done when its command passes on a clean checkout. Items that fix a runtime defect must
> also add a test that **fails before the fix** — a fix without a failing-first test does not close the item,
> because every defect below shipped past a green suite.

> [!WARNING]
> **Do not trust this document's "current state" paragraphs without re-verifying them against the tree in
> front of you.** They were accurate at `b9e2018`. If work has landed since, re-run the baseline in §0 first.

---

## 0. Baseline — measured 2026-08-01 at `b9e2018`

The branch is **red as committed**, before any of this plan's changes:

| Gate | Command | Status at `b9e2018` |
|---|---|---|
| B1 | `cd apps/dashboard-web && pnpm check` | ❌ **2 errors**, 15 warnings, 994 files |
| B2 | `cd apps/dashboard-web && pnpm test` | ❌ **2 failed / 118 passed** (13 files) |
| B3 | `cd apps/dashboard-web && pnpm build` | ⚪ unverified this pass |
| B4 | `rtk go build ./... && rtk go vet ./...` | ⚪ unverified this pass |
| B5 | `rtk go test ./tests/unit/...` | ⚪ unverified this pass |
| B6 | `SENTINEL_E2E=1 rtk go test -tags=e2e ./tests/e2e/ -count=1` | ⚪ unverified this pass (needs stack up) |

Re-run B3–B6 before starting; this plan assumes they were green at `b9e2018` per CLAUDE.md, but that claim
is exactly the kind this repo has been burned by.

### Definition of done for the whole plan

1. B1–B6 all green.
2. Every ❌ row in §1's defect register is closed, each by a test that failed before its fix.
3. No feature in the parity matrix is reachable from the UI while being non-functional — either it works or
   it is not rendered.
4. `docs/memory/VERIFIED_STATE.md` gains an entry per feature recording **the command that proved it**, not
   the commit that merged it.

---

## 1. Defect register

Severity is impact-if-shipped, not effort. **RT** = feature does not run at all. **SEC** = security.
**INT** = data integrity. **TEST** = missing or fraudulent coverage. **UX** = degraded but functional.

| ID | Class | Severity | Defect | Location |
|---|---|---|---|---|
| D01 | RT | Blocker | Invite link 403s for signed-in invitees — `invitations` missing from `reservedRoutes` | `hooks.server.ts:77` |
| D02 | RT | Blocker | `issues.id ILIKE` on a `uuid` column → 500 on every search; kills the entire relations flow | `lib/db/queries/issues.ts:344` |
| D03 | RT | Blocker | Relations UI on `[orgSlug]` issue page is mounted against mock data; route has no `+page.server.ts` | `[orgSlug]/…/issues/[issueId]/+page.svelte:10` |
| D04 | RT/INT | Critical | Processor keeps **one** alert config per org and per project (`map[string]*AlertConfig`); extra rules silently never fire | `processor-go/alerts/dispatcher.go:295-296,338-342` |
| D05 | SEC | Critical | `/settings/observability` has **no authentication** — anonymous access to DLQ depth, publish failures, stream names | `routes/settings/observability/+page.server.ts:43` |
| D06 | SEC | Critical | Invitation tokens stored **plaintext**; token moved from path into `?token=` query string | `schema.ts:27`, `invitations/[token]/+page.server.ts:6` |
| D07 | SEC/INT | Critical | Redemption is check-then-act over 5 statements, no transaction; not atomically single-use; `status` never set to `accepted`; **no revocation path exists** | `auth/accept-invite/+page.server.ts:126-152` |
| D08 | SEC/INT | Critical | `upsertOrganizationMember` unconditionally `set: { role }` — accepting a `viewer` invite **demotes an existing owner** | `lib/db/queries/organizations.ts:124-143` |
| D09 | SEC | Critical | `keyHash` (SHA-256 of live secret, the ingestor's Redis cache key) returned to the browser on key create + rotate | `lib/db/queries/apikeys.ts:89`, `keys/+server.ts:77`, `rotate/+server.ts:38` |
| D10 | SEC | High | New issue endpoints gate on org membership only, no role check: an org `viewer` can resolve issues one-at-a-time though bulk resolve denies them | `api/issues/[issueId]/status/+server.ts:50-62`, `api/issues/search/+server.ts:55-67` |
| D11 | RT | High | Unlinking an **incoming** relation always 404s (direction not reflected in the DELETE) | `IssueRelations.svelte:109-116` |
| D12 | SEC/UX | High | `/dlq` items array is **fabricated** — hardcoded `sequence: 1`, `event_id: "dlq_oldest_event"`, `retry_attempts: 7` — shown to operators as real parked events | `processor-go/main.go:394+` |
| D13 | TEST | High | Alerts loader test **re-implements the predicate inline**; never imports `load`. Deleting the loader leaves it green | `settings/alerts/page.server.test.ts:18-21,43-46` |
| D14 | TEST | High | `b3ccde9` (issue relations) added **zero test files** | — |
| D15 | TEST | High | Invitation **acceptance** half has zero tests (creation half is well covered) | — |
| D16 | TEST | High | Observability commit has zero tests, Go and TS both | — |
| D17 | RT | High | `pnpm check` red: `PageData` mismatch on org observability page | `[orgSlug]/settings/observability/+page.svelte:6` |
| D18 | RT | High | `pnpm check` red: `relationType: string` not assignable to the component union | `issues/[id]/+page.svelte:122` |
| D19 | TEST | High | `members.test.ts` red: expects 400, gets 403 (authz precedes body parse) | `members.test.ts:70` |
| D20 | TEST | High | `members.test.ts` red: asserts a mocked token the endpoint does not emit (fresh 64-hex each run) | `members.test.ts:352` |
| D21 | INT | Medium | No unique index on `alert_configs (organization_id, project_id)`; UI creates duplicates freely | `1722100000_add_alert_config_org_layer.sql` |
| D22 | INT | Medium | Relations: no self-relation guard, no cycle prevention, duplicate insert surfaces as 500 not 409 | `api/issues/[issueId]/relations/+server.ts:29-65` |
| D23 | SEC | Medium | Two divergent write paths for `issues.status` with different authz, tenant keys, and vocabularies | `status/+server.ts` vs `projects/[projectId]/issues/batch/+server.ts` |
| D24 | UX | Medium | Alerts edit mode exposes a scope toggle that PUT silently discards (200, no change) | `settings/alerts/+page.svelte:246-257`, `api/alerts/+server.ts:312-325` |
| D25 | UX | Medium | API keys: `keyPrefix` vs `prefix` field mismatch — Prefix column always renders the `sent_••••` fallback | `ApiKeyTable.svelte:5,79,138` |
| D26 | UX | Medium | Failed key create leaves modal permanently stuck (`isSubmitting` never reset) | `ApiKeyCreateModal.svelte:24` |
| D27 | UX/INT | Medium | Project keys page renders an "Org-Wide" option it ignores — silently mints a project key | `ApiKeyCreateModal.svelte:8,72` |
| D28 | INT | Medium | No validation on `rateLimitRpm`; unknown `scope` silently downgraded to `ingest` and reported as success | `keys/+server.ts:49,72` |
| D29 | SEC | Medium | Magic-link sign-in from invite page uses v4 `callbackUrl` (should be `redirectTo`) and its `try/catch` swallows the `Redirect` throw | `auth/accept-invite/+page.server.ts:167-186` |
| D30 | INT | Medium | `user.email` has no unique/citext constraint; email→userId resolution is `limit 1` on arbitrary order | `1716508800_init.sql:97` |
| D31 | SEC | Medium | Pending `owner` invite still redeems as owner for 7 days after the inviter is demoted/removed; no re-validation, no revoke | `auth/accept-invite/+page.server.ts:145` |
| D32 | INT | Low | Sole-owner guard is count-then-write, no transaction/row lock — concurrent demotions can orphan an org | `members/[memberId]/+server.ts:82,148` |
| D33 | INT | Low | Member target lookup `or(id = x, userId = x)` with no `limit`/`orderBy` — ambiguous row | `members/[memberId]/+server.ts:54-57` |
| D34 | UX | Low | Org alerts: `activeOrgId` can select an org the user has no org-level membership in → unexplained 403 | `settings/alerts/+page.svelte:45-48` |
| D35 | UX | Low | `canManageOrgAlerts` is an any-org boolean; select lists orgs guaranteed to 403 | `settings/alerts/+page.server.ts:101-103` |
| D36 | UX | Low | Key rotate replaces the old row instead of appending; revoked key vanishes until reload | `keys/+page.svelte:95` |
| D37 | UX | Low | Org key table shows a raw project UUID as "Target" though the loader has the names | `ApiKeyTable.svelte:88` |
| D38 | INT | Low | `resolveProjectInOrg`'s `length === 36` UUID heuristic mis-handles a 36-char project name | `keys/_shared.ts:43` |
| D39 | SEC | Low | Unauthenticated org-existence oracle: loaders query `organizations` by slug before auth | `keys/+page.server.ts:11` |
| D40 | INT | Low | Relations JSON body parsed without `.catch()` → 500 not 400 (status endpoint gets this right) | `relations/+server.ts:26,100` |
| D41 | SEC | Low | No rate limit on invitation creation; delivery failure swallowed but 201 returned | `invitations/+server.ts:90` |
| D42 | INT | Low | Expired invitations never reaped; plaintext tokens accumulate (amplifies D06) | — |
| D43 | UX | Low | `getIssueRelations` labels the incoming row's joined issue `targetIssue` when it is the *source* | `lib/db/queries/issues.ts:280-317` |
| D44 | UX | Low | 15 a11y warnings, incl. two `dialog` roles without `tabindex` and click handlers without keyboard equivalents | various |
| D45 | INT | Low | `process.env` used in a SvelteKit `load` rather than `$env/dynamic/private` (adapter-dependent) | `settings/observability/+page.server.ts:44-45` |
| D46 | UX | Low | Dead mock `relatedIssues` array superseded by the component | `[orgSlug]/…/[issueId]/+page.svelte:33-36` |
| D47 | SEC | Low | `docker-compose.yml:151` publishes processor `8081` to the host with no auth on `/health`, `/metrics`, or the new `/dlq` | `docker-compose.yml:151` |

---

## 2. Sequencing rationale

Five phases, ordered so that **each phase leaves the tree greener than it found it** and no phase depends on
a later one.

- **P0 turns the lights on.** The suite is red, so no later phase can distinguish "my change broke it" from
  "it was already broken." P0 is a prerequisite for honest verification of everything after it.
- **P1 closes the security holes** that are live right now on any deployed instance. D05 and D09 leak without
  anyone doing anything wrong; D06/D07/D08 turn a single leaked URL into an org takeover. These come before
  feature repair because a broken feature is safer than a working exploit.
- **P2 makes the three dead features actually run.** Deliberately after P1 so that D02/D03's search and link
  paths are not made reachable while D10's missing role gate is still open.
- **P3 fixes integrity and correctness** — the defects that produce wrong data rather than no data.
- **P4 is polish and hardening**, and can be split across contributors freely.

**Do not reorder P0 or P1.** P2–P4 items are largely independent; parallelize within a phase.

---

## 3. P0 — Restore an honest baseline

> Goal: `pnpm check` and `pnpm test` green, with no test weakened to get there.

### P0-1 — Fix the org observability `PageData` type mismatch (D17)

**Current state**: `[orgSlug]/settings/observability/+page.svelte:6` annotates `data` as
`{ session; observability }`, but the generated `PageData` is `{ [x: string]: undefined }` — a symptom that
the loader's return type is not flowing through. Diagnose before patching: an empty generated type usually
means the loader file is not being picked up as a route module, or `svelte-kit sync` is stale.

**Action**: run `pnpm exec svelte-kit sync` first and re-check. If it persists, align the loader's declared
return with the page's `$props()` type; do **not** silence it with `as any` — the loader return type is the
only contract between these two files.

**Acceptance**: `pnpm check` reports 0 errors for this file.

### P0-2 — Fix the `relationType` union mismatch (D18)

**Current state**: the DB returns `relationType: string`; `RelationItem` narrows to
`'linked_to' | 'caused_by' | 'duplicate_of'`. Note the audit's mention of `parent_of`/`child_of` is wrong —
the migration's CHECK permits exactly those three
(`1721900000_add_issue_lifecycle_and_relations.sql:72`).

**Action**: export a single `RelationType` union from one module, derive it from the same constant the
validation in `relations/+server.ts` uses, and have both the query layer and the component import it. Narrow
at the boundary with a runtime guard that drops (and logs) unrecognized values, rather than casting.

**Acceptance**: `pnpm check` → 0 errors; a unit test asserts an unknown `relationType` from the DB is
dropped rather than rendered.

### P0-3 — Correct the two false member tests (D19, D20)

**Current state**: both failures are **test bugs, not product bugs**, and both are informative.

- `members.test.ts:70` expects 400 on malformed JSON but gets 403. The endpoint authorizes *before* parsing
  the body — which is the correct order. **Fix the test**, and rename it to say so
  (`403s on malformed JSON when caller is unauthorized`). Then add the genuinely missing case: an
  *authorized* caller sending malformed JSON, which must 400.
- `members.test.ts:352` asserts the emailed URL contains `token123`, but the endpoint generates a fresh
  256-bit token internally and ignores the mock. Assert the **shape** (`/invitations/` + 64 hex chars) rather
  than a fixed value, or inject the token generator. Note this assertion becomes obsolete under **P1-3**,
  which changes what goes in the URL — sequence P0-3 to land first, then update it there.

**Acceptance**: `pnpm test` → 0 failures; the two rewritten tests fail if the authz order or the token
length regresses.

### P0-4 — Record the true baseline

**Action**: run B3–B6 from §0 and record actual numbers in `docs/memory/VERIFIED_STATE.md`, with the date and
the command. Do not copy CLAUDE.md's counts.

**Acceptance**: all six baseline gates have a measured status in `VERIFIED_STATE.md` dated this pass.

---

## 4. P1 — Close the live security holes

### P1-1 — Authenticate the observability pages (D05)

**Current state**: `settings` is in `reservedRoutes`, so `orgHandle` never guards it; the root
`+layout.server.ts` returns the session without enforcing it; the observability `load` never calls
`locals.auth()`. Anonymous visitors get DLQ depth, publish-failure counts, oldest-message age, JetStream
stream names, and error classes. Compare `[orgSlug]/settings/members/+page.server.ts:8-15`, which does both
checks — observability is the outlier.

**Action**:
1. Add `locals.auth()` + 401/redirect to `settings/observability/+page.server.ts`.
2. Decide and **document** who may see system-wide infrastructure state. These pages expose cross-tenant
   data (DLQ depth aggregates every org). Recommendation: gate on a platform-operator check, not org
   membership — an org admin should not see another tenant's failure volume. If that check does not exist
   yet, restrict the route to authenticated users **and** strip cross-tenant aggregates until it does.
3. Add a guarding `+layout.server.ts` under `src/routes/settings/` so the next page added there inherits the
   check instead of repeating this bug.
4. Audit every other route under `settings/` for the same gap — `reservedRoutes` disables `orgHandle` for
   all of them.

**Acceptance**: a route test asserts 401/redirect for an anonymous request to `/settings/observability`, and
a second asserts a non-operator authenticated user cannot read cross-tenant aggregates.

### P1-2 — Stop returning `keyHash` to the browser (D09)

**Current state**: `apikeys.ts:89` uses `.returning()` with no column list, so the created row carries every
column including `keyHash`; `keys/+server.ts:77` and `rotate/+server.ts:38` serialize it wholesale. This is
the exact value the ingestor's Redis cache is keyed on (`apikey.go:53`). `getOrganizationApiKeys` already
enumerates columns correctly — create and rotate simply don't.

**Action**: enumerate columns in `.returning()` for create and rotate, and add a shared `toPublicKey()`
serializer that both endpoints must go through. Consider making the raw-secret field the only place a secret
value can appear in a response.

**Acceptance**: a route test asserts the create response body and the rotate response body contain **no**
`keyHash` key at any depth. Add the same assertion for the list endpoint so it cannot regress.

### P1-3 — Harden the invitation token (D06, D42)

**Current state**: `crypto.randomBytes(32)` — entropy is fine, 256 bits, so timing-safe comparison is not
required. The problems are storage and transport: `schema.ts:27` stores the token verbatim, and
`invitations/[token]/+page.server.ts:6` moves the secret out of a path segment into `?token=…`, where it
lands in browser history and the `Referer` of any outbound asset, and is re-embedded in OAuth `redirectTo`.

**Action**:
1. Store `sha256(token)` and look up by hash. Migration: add `token_hash`, backfill is impossible for
   existing plaintext rows — **delete pending invitations** as part of the migration and re-issue. State that
   explicitly in the migration comment.
2. Keep the token in the **path**, never the query string. If the sign-in round trip needs to carry it, put
   it in a short-lived `HttpOnly` cookie rather than a URL.
3. Add a reaper for expired rows (a cron route beside `api/cron/retention`, or a `DELETE` in the same
   transaction as any invitation write).

**Acceptance**: a test asserts the DB column never equals the emailed token; a test asserts no route response
or redirect `Location` contains the raw token in a query string.

### P1-4 — Make redemption atomic and single-use (D07, D31)

**Current state**: five independent statements, no transaction. Two concurrent redemptions both pass the
check. If `upsertOrganizationMember` succeeds and `deleteInvitationById` fails, the token stays live until
`expiresAt`. `status` is never set to `accepted`, so the CHECK's `accepted`/`revoked`/`expired` values are
unreachable and the `status !== 'pending'` guards are dead code. There is **no revocation endpoint at all** —
the only way to kill a leaked token is to re-invite the same address.

**Action**:
1. Replace check-then-act with a single conditional claim:
   `UPDATE organization_invitations SET status='accepted', accepted_at=now() WHERE token_hash=$1 AND status='pending' AND expires_at > now() RETURNING *`.
   Act only if a row came back. Wrap the claim and the member insert in one `db.transaction`.
2. Stop deleting the row — the `status` lifecycle exists; use it. This also gives an audit trail.
3. Add a revoke endpoint (`DELETE /api/organizations/[orgId]/invitations/[id]`) with the same owner/admin
   gate as creation, plus a list endpoint so an admin can see what is outstanding.
4. Re-validate the role against the allowlist at redemption rather than casting
   (`as 'owner' | …`), so a tampered or stale DB value cannot grant an unmodelled role.

**Acceptance**: a concurrency test fires two simultaneous redemptions of one token and asserts exactly one
membership write; a test asserts a second redemption after success returns "already used"; a test asserts an
expired token is refused.

### P1-5 — Stop acceptance from demoting existing members (D08, D30)

**Current state**: `upsertOrganizationMember` does `onConflictDoUpdate … set: { role }` unconditionally. The
creation-side "already a member" guard compares a lowercased input against `users.email`, which stores
provider casing and has **no unique or citext constraint** — so a member stored as `Bob@X.com` is invisible
to it. An admin can then send that owner a `viewer` invite and demote them on click. The load handler even
presents this as a normal accept, reporting `already_member` only when the roles are equal.

**Action**:
1. Change the upsert to **never lower an existing role**. If a membership row already exists, either no-op
   (report `already_member` regardless of role) or take `max(existing, invited)` by an explicit rank. Pick
   one and write the rank down next to the code.
2. Add a unique index on `lower(email)` for `user`, and normalize on write. Handle the existing-duplicates
   case in the migration explicitly — do not assume there are none.
3. Make the creation-side membership check case-insensitive so the invite is refused up front too.

**Acceptance**: a test asserts an owner accepting a `viewer` invitation to their own org **remains owner**; a
test asserts the case-variant email is detected by the already-a-member guard.

### P1-6 — Add a role gate to the new issue endpoints (D10, D23)

**Current state**: `status` and `search` check only that an `organization_members` row exists — no role
check, no project check. Org `viewer` is a valid role. The bulk path
(`projects/[projectId]/issues/batch/+server.ts:33-36`) allows only `owner|admin|engineer|support`. So a
viewer who cannot bulk-resolve **can resolve one at a time**. Cross-*organization* access is correctly
blocked on both; the gap is within-org.

**Action**:
1. Gate `PATCH /api/issues/:id/status` behind the same role allowlist as the batch endpoint, and gate
   `search` on at least `read`.
2. Decide whether these endpoints are org-scoped or project-scoped and make them consistent with the page
   loads, which use `checkProjectAccess` against `project_members`. Two membership models coexisting is the
   root cause; extract one `requireIssueAccess(userId, issueId, permission)` helper that both new endpoints
   and the batch endpoint call, so there is a single place to get this right.
3. Validate `resolvedInVersion` (type + length against the `varchar(100)` column) instead of passing it
   through from the body.

**Acceptance**: a test asserts an org `viewer` gets 403 from `PATCH /status`; a test asserts a user in the
org but not the project cannot read that project's issues via `search`; a test asserts an oversized
`resolvedInVersion` yields 400, not 500.

---

## 5. P2 — Make the dead features run

### P2-1 — Add `invitations` to `reservedRoutes` (D01)

**Current state** (verified directly this pass): `hooks.server.ts:77` reads
`['api','auth','issues','search','settings','admin','docs','billing','support','signin']`. `orgHandle` runs
before route resolution, so for an authenticated session `/invitations/<token>` is read as an org slug, finds
nothing, and throws 403 at `:100`. Anonymous users escape via the early return at `:64` — so the emailed link
fails for exactly the common case, a colleague who is already signed in.

**Action**: add `'invitations'`. Then fix the underlying fragility: `reservedRoutes` is a hand-maintained
list that silently swallows any new top-level route. Either derive it from the route manifest or add a test
that enumerates top-level directories under `src/routes/` and fails when one is neither reserved nor
intentionally an org slug.

**Acceptance**: a test asserts an authenticated request to `/invitations/<token>` reaches the redirect rather
than throwing 403; the manifest-drift test fails when a new top-level route is added without a decision.

### P2-2 — Fix the uuid search (D02)

**Current state** (reproduced against live `sentinel-postgres`):
`select id from issues where id ilike '%abc%'` → `ERROR: operator does not exist: uuid ~~* unknown`. Every
search with `q.length >= 2` throws, and search is the only way to pick a link target, so the whole
link/duplicate flow is unusable.

**Action**: cast — `issues.id::text ILIKE`. Consider whether substring-matching a UUID is wanted at all; a
prefix match or an exact-id branch is likelier what a user means, and avoids a full scan.

**Acceptance**: an integration test runs the real query against a real Postgres and asserts a row comes back.
A mock-chained Drizzle unit test **cannot** close this item — the mock is what let it ship.

### P2-3 — Give the `[orgSlug]` issue page a real loader (D03, D46)

**Current state**: `[orgSlug]/projects/[projectId]/issues/[issueId]/` contains **only** `+page.svelte`
(verified). `data` is undefined, so the component falls through to a hardcoded `currentIssueId =
'ISSUE-123'`. Every request carries a non-UUID id and fails at the DB layer; the PATCH at `:97` has no
`res.ok` check and reloads the page regardless, so the failure is invisible.

**Action**: add `+page.server.ts` mirroring the working legacy loader (`routes/issues/[id]/+page.server.ts`),
with `checkProjectAccess`. Delete the mock constants and the dead `relatedIssues` array. Add `res.ok`
handling to every mutation in the component.

**Acceptance**: a loader test asserts real data reaches the page and that a non-member gets denied; a
component test asserts a failed PATCH surfaces an error instead of reloading.

### P2-4 — Fix incoming-relation unlink (D11, D43)

**Current state**: the component picks the correct counterpart id but always DELETEs
`/api/issues/${currentIssueId}/relations` with it as `targetIssueId`, while the handler treats
`params.issueId` as the **source** and `deleteIssueRelation` matches source+target exactly. For an incoming
relation the stored row is (source=other, target=current), so the reversed match deletes nothing and 404s.
Since `getIssueRelations` now returns incoming rows too, roughly half of what is rendered is un-removable.

**Action**: delete by the relation's own `id` — the row is already fetched and carries it. That removes the
directional reasoning entirely. Also fix `getIssueRelations` to label the counterpart honestly
(`relatedIssue`, not `targetIssue`) so the contract stops lying.

**Acceptance**: a test creates an incoming relation and asserts unlink succeeds and the row is gone.

### P2-5 — Fix the alert config map (D04, D21)

**Current state** (verified): `dispatcher.go:295-296` builds `map[string]*AlertConfig` keyed by project id
and org id; `:338-342` assigns into them. Nothing in the schema constrains one row per key — the migration
adds **no** unique index — and the new UI creates org-wide rules with no duplicate check. Create two org-wide
rules (email + telegram) and only whichever row Postgres returns last ever fires, silently, with no error
anywhere. Every existing org-wide test injects maps via `SetOrgConfigsForTest`, bypassing both the SQL and
the map keying — the defect sits precisely in the seam the tests skip.

**Action**: decide the intended semantics first, and write it down:
- **If multiple rules per scope are intended** (the UI implies they are): change to
  `map[string][]*AlertConfig` and have `resolveConfigs` union all matching rules, preserving the existing
  destination dedup.
- **If one rule per scope is intended**: add a unique index on `(organization_id, project_id)` — note this
  needs `COALESCE` or a partial index, since NULL never conflicts in Postgres — and have the API return 409
  on the second create.

Recommendation: the former. The UI already lets a user create N, and silently dropping N-1 is the worse
failure.

**Acceptance**: an **integration** test seeds two org-wide configs with different destinations through real
SQL, runs `refreshConfigs`, and asserts both fire. `seedAlertConfig` currently takes a non-null `projectID`
and must be extended to seed NULL-project rows.

### P2-6 — Make `/dlq` report real data or report nothing (D12)

**Current state**: aggregates (`total_depth`, `publish_failures`, `oldest_age_seconds`, `oldest_class`) are
real. The `items` array is **synthetic** — at most one entry with hardcoded `sequence: 1`,
`event_id: "dlq_oldest_event"`, `org_id: "system"`, `project_id: "processor"`, `retry_attempts: 7`, and a
hand-built `raw_payload` string that embeds `detail.Stats.Stream` unescaped into hand-concatenated JSON.

**Action**: either fetch real parked messages from JetStream, or remove `items` from the response and the
table from the UI. Do not ship a placeholder that an operator will read as a real parked event during an
incident. If real items are implemented, they carry tenant payloads — revisit P1-1's access decision, and
build the JSON with `encoding/json`, never string concatenation.

**Acceptance**: a Go test asserts `items` either reflects seeded DLQ messages or is absent; no test may
assert the placeholder shape.

---

## 6. P3 — Integrity and correctness

- **P3-1 (D22)** — Relations integrity. Add a `sourceIssueId !== targetIssueId` guard at the endpoint **and**
  a DB CHECK. Catch `23505` from the existing `issue_relations_unique` index and return 409. Decide on cycle
  policy: `A duplicate_of B` + `B duplicate_of A` currently both insert; since the UI reads `duplicate_of`
  semantically, prevent at least 2-cycles on `duplicate_of`. *Acceptance*: tests for self-relation → 400,
  re-link → 409, 2-cycle → 400.
- **P3-2 (D24)** — Alerts scope in edit mode. Either disable the scope toggle when editing, or have PUT
  reject a scope-changing body with 400. Silently returning 200 and changing nothing is the worst option.
  *Acceptance*: a test asserts a scope-changing PUT is refused.
- **P3-3 (D28)** — Validate `rateLimitRpm` (positive, bounded) and **reject** an unrecognized `scope` with
  400 instead of downgrading it to `ingest` and reporting success. *Acceptance*: tests for both.
- **P3-4 (D25, D27)** — Fix `keyPrefix`/`prefix` and remove the org-wide option from the project-scoped
  modal (or honour it). *Acceptance*: a table test rendered with a **real API-shaped row** — the absence of
  which is what let D25 through.
- **P3-5 (D29)** — Use `redirectTo` (Auth.js v5), and stop the `try/catch` swallowing `signIn`'s thrown
  `Redirect` — rethrow it. *Acceptance*: a test asserts the magic-link action redirects and preserves the
  invite context.
- **P3-6 (D32, D33)** — Sole-owner guard inside a transaction with `SELECT … FOR UPDATE`; disambiguate the
  member lookup (prefer `id`, fall back to `userId`, never `or` without a limit). *Acceptance*: a
  concurrency test asserts an org cannot be left ownerless.
- **P3-7 (D23 follow-through)** — Once P1-6's shared helper exists, converge the two status write paths onto
  one vocabulary and one activity-log shape, or delete the newer one.
- **P3-8 (D40)** — `.catch()` on `request.json()` in the relations endpoint → 400.
- **P3-9 (D41, D31)** — Rate-limit invitation creation; stop returning 201 when delivery failed, or return a
  body that distinguishes created-from-delivered.

---

## 7. P4 — Hardening and polish

- **P4-1 (D34, D35)** — Prefer `data.userOrganizations[0]?.id` for `activeOrgId`; filter the org select to
  orgs where the user actually holds `manage_keys`.
- **P4-2 (D26, D36, D37)** — Reset `isSubmitting` on error; append rather than replace on rotate; join
  project names for the Target column.
- **P4-3 (D38, D39)** — Replace the `length === 36` UUID heuristic with a real UUID regex or an explicit
  `projectId` vs `projectName` parameter; move the org-by-slug lookup after auth.
- **P4-4 (D45)** — `$env/dynamic/private` instead of `process.env` in loaders.
- **P4-5 (D44)** — Clear the 15 a11y warnings; the two `dialog` roles without `tabindex` are real keyboard
  traps.
- **P4-6 (D47)** — Decide whether processor `8081` should be published to the host at all, and whether
  `/metrics` and `/dlq` need auth. At minimum, do not publish `/dlq` unauthenticated once P2-6 makes it
  carry real tenant payloads.

---

## 8. Test debt — the through-line

Every blocker in this plan shipped past a green suite. The pattern is consistent and worth naming, because
fixing the individual defects without fixing the pattern guarantees a repeat.

| Anti-pattern | Where it bit | Rule going forward |
|---|---|---|
| Mock-chained Drizzle stubs never execute SQL | D02 (uuid ILIKE), D04 (map keying) | Any query with a non-trivial `WHERE`, a cast, or a NULL-semantics dependency needs an **integration** test against real Postgres |
| Tests re-implement the logic they claim to test | D13 (alerts loader) | A test must **import the real symbol**. Add a CI check or review rule: no test may inline a copy of production logic |
| Hand-written fixtures diverge from real API shapes | D25 (`keyPrefix`) | Fixtures derive from the response type, or the test renders real handler output |
| Test-only injection bypasses the production load path | D04 (`SetOrgConfigsForTest`) | Every such helper needs at least one test that goes through the real loader instead |
| Features merged with zero tests | D14, D15, D16 | No commit closing a parity gap merges without a test that fails before the fix |
| Route reachability never asserted | D01, D03 | Each new user-facing route gets one test that requests the route as a user would, through the hooks |

**Coverage to add, by feature** (consolidating D14–D16):

- *API keys*: both `+page.server.ts` loaders (currently zero tests), all six page handlers, `ApiKeyCreateModal`
  (no test file), rotate/revoke dispatch and the confirmation modals, `keyHash` exclusion, `resolveProjectInOrg`
  foreign-project 400, and the NATS `keyHash` camelCase contract with `apikey.go:53`.
- *Alerts*: the loader itself, cross-org IDOR with a `manage_keys`-holding caller posting a foreign
  `organizationId`, org-wide rows through real `refreshConfigs`, multiple configs per scope, NATS publish
  assertions for org-wide, `channelConfig` preservation on partial PUT.
- *Invitations*: the entire acceptance half — expiry, replay, concurrency, email mismatch in the **action**
  (not just the load), role assignment, duplicate membership, the `/invitations/[token]` redirect, both
  `signIn` actions.
- *Relations*: `IssueRelations.svelte` (no test file), both new endpoints, and the five untested query
  functions (`searchIssuesInOrg`, `getIssueRelations`, `createIssueRelation`, `deleteIssueRelation`,
  `updateIssueStatus`).
- *Observability*: anonymous access, cross-tenant aggregate exposure, the Go `/dlq` handler.
- *Members*: `email.ts` itself, `InviteMemberModal`, the members loader authz, and — importantly — the DB
  mock's inability to verify `where` clauses means the org-scoping that makes member RBAC sound is currently
  asserted **only by reading**. Add integration coverage for it.

---

## 9. What is actually good — do not regress it

Recorded so a later pass does not "fix" working code:

- **Member management RBAC** is the strongest work in this batch: self-escalation, admin-vs-owner, and
  last-owner removal are guarded on both PATCH and DELETE; targets are org-scoped via `and(organizationId, …)`;
  the caller is re-read from `locals.auth()` and never trusted from the body; `members.test.ts` asserts the
  **negatives** (`upsertOrganizationMember` not called), which is the assertion most suites omit.
- **API key authz** is correct end to end: route → `requireOrgMembership` → `hasPermission` → cross-org 404
  (no enumeration oracle). `hooks.server.ts` guards the page shell too, so the UI hides nothing the server
  doesn't also enforce.
- **API key cache invalidation** survives a NATS failure: revoke and rotate also set `expiresAt = now()`, and
  the ingestor query enforces expiry independently.
- **Raw secret handling** is clean: lives only in component state, never `localStorage`, never logged,
  cleared on dismiss.
- **Alert mutations route on the stored row's scope**, never the body — the right shape.
- **Alert `channel_config` double-encoding unwrap** in `dispatcher.go:318-331` is load-bearing; leave it.
- **`email.ts` genuinely sends** via nodemailer, and its two conditional behaviors (unset `EMAIL_SERVER`,
  `smtp://debug` json transport) are explicit and logged. Invitations also work with email off, since the
  modal builds a copy-paste link.
- **Cross-organization isolation on the new issue endpoints is correct** — the gap is within-org only.
- **Invitation email templating** escapes HTML and strips CRLF, blocking header injection.
- **Invitation identity binding** re-checks email in the action, not only the load.

---

## 10. Documentation to correct

- `CLAUDE.md` states "Invitation acceptance has no route ... nothing anywhere consumes an invitation token."
  **Stale** — the route exists (`src/routes/invitations/[token]/+page.server.ts`); it is merely unreachable
  for signed-in users (D01). Update after P2-1.
- `docs/plans/E2E_RECOVERY_PLAN.md` P9-4 should reference this plan for the acceptance-flow defects.
- `docs/memory/VERIFIED_STATE.md` — add one entry per feature after its phase completes, each with the
  command that proved it. Per this repo's own convention, a merge is not evidence.
- The five `summaries/architecture_review/*.md` files committed in `49c0307` predate these findings; either
  supersede them with a pointer here or delete them, so the next reader does not treat them as current.
