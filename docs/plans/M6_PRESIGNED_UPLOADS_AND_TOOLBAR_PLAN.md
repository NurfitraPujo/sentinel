# M6 Sub-Plan — Presigned Large Uploads + Toolbar Markdown Editor

Status: **DONE 2026-08-12** (branch `feat/manual-issues`). All stages implemented and verified:
dashboard gates green (`pnpm build`; `pnpm check` 0/0; `pnpm test --sequence.shuffle` 531 passed),
drift 26/26 against a real migrated Postgres, `reports.presign.flow.integration.test.ts` green against
real compose Postgres+MinIO (presign→PUT→finalize→claim; pending-cannot-link gate; 415+cleanup), and
full-stack e2e held at 76 passed / 0 skipped. Real-MinIO/deployment gotchas surfaced only against
real infra — three SigV4/checksum signing issues **plus a browser-reachability bug** (presign URLs
were signed against the in-cluster `S3_ENDPOINT` the browser can't reach; fixed with a separate
`S3_PUBLIC_ENDPOINT` signing client) — all recorded in `VERIFIED_STATE.md` → "M6 (partial)". Presign/
finalize also emit structured `uploads.*` logs (the dashboard's obs model; no span API to wire).
Toolbar shipped as a dependency-free Markdown-syntax toolbar (Tiptap WYSIWYG remains deferred).
Scope: two of the deferred
M6 backlog items from `MANUAL_ISSUES_DESIGN.md` §12 — **presigned large uploads** (§4/Q4) and the
**toolbar Markdown editor** (§3/Q3). The other M6 items (SSE realtime, NATS push discovery, AI triage,
release/move notification kinds) stay deferred and out of scope here.

Acceptance bar (same as every prior phase): every defect/behaviour proven **red-first**; migration
replayed twice against a disposable Postgres and idempotent; `schema.ts` mirrored and drift test green
(`SCHEMA_DRIFT_REQUIRED=1`); all three dashboard gates (`pnpm build && pnpm check && pnpm test
--sequence.shuffle`); route→code call path exists (B3); real compose-stack integration proof for the
upload flow; full e2e suite stays 76+/0 skips. No repo-wide `go build ./...` / `go test ./...` — this
phase touches no root-module Go code.

---

## Feature A — Presigned large uploads (§4/Q4)

### Problem

Every upload today proxies through SvelteKit (`/api/uploads` → `handleAttachmentUpload`), which buffers
the whole file in memory (`Buffer.from(await file.arrayBuffer())`) and is capped at **25 MB**. The design
named a **presigned large-video path** as the deferred escape hatch for files that exceed that cap.

### Design

Two-cap model. Files **≤ 25 MB** keep the existing proxied path unchanged. Files **> 25 MB** (up to a new
`MAX_PRESIGNED_UPLOAD_BYTES = 500 MB`) go direct-to-bucket via a presigned PUT, with server-side
validation moved to a **finalize** step after the bytes land.

**The magic-byte guarantee is preserved.** The server can't sniff bytes it never receives, so validation
moves *after* the direct upload: at finalize the server does a ranged GET of the object's first bytes,
runs the exact same `sniffContentType` + `resolveContentType` allowlist as the proxy path, and **deletes
the object + row** if it fails. An object is never trusted on the client's declared type alone.

**A `pending` object can never be linked.** New column `attachments.status` (`'pending'` | `'ready'`).
Presigned rows start `'pending'`; only finalize flips them to `'ready'`. `claimDraftAttachmentsOnto`
(the single chokepoint for linking a draft onto an issue/comment) additionally requires
`status = 'ready'`, in both the pre-check and the conditional UPDATE WHERE. **This is the load-bearing
security property** — prove it red-first: a `pending` attachment must be un-linkable.

#### Schema — migration `1723100000_add_attachment_status.sql`
- `ALTER TABLE attachments ADD COLUMN IF NOT EXISTS status varchar(16) NOT NULL DEFAULT 'ready';`
  Existing rows (all validated inline) default to `'ready'` — correct.
- CHECK `status IN ('pending','ready')` added via a `pg_constraint` catalog guard (A1 idempotency —
  `ADD CONSTRAINT` has no `IF NOT EXISTS`; guard in a `DO $$ ... $$` block).
- Partial index `attachments(created_at) WHERE status = 'pending'` is **not** needed — the reaper already
  scans by `(issue_id IS NULL AND comment_id IS NULL, created_at)`, which covers pending rows. Skip it.
- Symmetric `Down`. Idempotent; replayed under every goose ledger (A1). Mirror in `schema.ts`; drift test.

#### Storage (`$lib/server/storage.ts`)
- Add dep `@aws-sdk/s3-request-presigner` (companion to the already-present `@aws-sdk/client-s3`).
- `createPresignedPutUrl(key, contentType, expiresSeconds)` → `getSignedUrl(client, new PutObjectCommand(...))`.
- `headObject(key)` → returns `{ contentLength }` (real object size for the cap re-check).
- `getObjectRangeBytes(key, length)` → ranged GET (`Range: bytes=0-<length-1>`), buffered, for sniffing.

#### Server core (`$lib/server/upload-core.ts`)
- `MAX_PRESIGNED_UPLOAD_BYTES = 500 * 1024 * 1024`.
- `SNIFF_BYTES = 4096` (enough for every signature in `attachment-sniff.ts`).
- `createPresignedAttachment({ organizationId, uploaderId, filename, declaredContentType, sizeBytes })`:
  validate `declaredContentType` ∈ allowlist (declared is all we have pre-upload; re-verified at finalize),
  `0 < sizeBytes ≤ MAX_PRESIGNED_UPLOAD_BYTES`; insert row `status:'pending'`, storageKey
  `org/<orgId>/<uuid>`, declared contentType, declared sizeBytes; return
  `{ attachmentId, uploadUrl, expiresAt }`. Opportunistic reap as the proxy path does.
- `finalizePresignedAttachment({ attachmentId, organizationId, uploaderId })`:
  load row; reject if not same org / not this uploader (user) / not `status:'pending'` / already linked.
  `headObject` → if missing → 409 (`upload not completed`); if `contentLength > cap` → delete object+row → 413.
  `getObjectRangeBytes(SNIFF_BYTES)` → `resolveContentType(sniffContentType(buf), declared)`; if `null` →
  delete object+row → 415. Else UPDATE row `status:'ready'`, `contentType:resolved`,
  `sizeBytes:contentLength`; return the standard `UploadCoreResult` shape.

#### Routes
- `POST /api/uploads/presign` — session auth + `requireReportAccess(userId, organizationId, 'create')`;
  JSON body `{ organizationId, filename, contentType, sizeBytes }`; → `createPresignedAttachment`; 201.
- `POST /api/uploads/[id]/finalize` — session auth; resolve attachment's org, `requireReportAccess(create)`;
  → `finalizePresignedAttachment`; 200. (Uploader identity re-checked inside core.)
- Scope note: presigned path is **session-only** (users upload large media). Agents keep the proxy
  `/api/agent/uploads` (they post generated Markdown + small artifacts). Documented, not a gap.

#### `claimDraftAttachmentsOnto` (`$lib/db/queries/reports.ts`)
- Select `status`; skip row unless `status === 'ready'`; add `eq(attachments.status,'ready')` to the
  UPDATE WHERE. **Red-first proof required.**

#### Reaper — no change
An un-finalized `pending` row is unlinked, so the existing 24 h orphan sweep already deletes it and its
object. Confirm with a test; write no new reaper code.

#### Frontend (`UploadZone.svelte`)
- Files ≤ 25 MB: unchanged proxy POST. Files > 25 MB: `presign` → **XHR** PUT direct to `uploadUrl`
  (XHR so a real byte-percentage bar shows for large videos — worth the deviation from the fetch-only
  convention here, since a 300 MB upload with only "Uploading…" is poor UX) → `finalize`. Track percent in
  the existing `TrackedFile` status machine (add `progress?: number`). On any failure, surface the message;
  a failed/never-finalized draft is reaped.
- Update the dropzone hint copy (25 MB → "up to 25 MB proxied, large video up to 500 MB").

---

## Feature B — Toolbar Markdown editor (§3/Q3)

### Decision

The design says "toolbar editor later … e.g. Tiptap … **no data migration**." The binding constraint is
*stays Markdown* (no migration). A **dependency-free Markdown-syntax toolbar** over the existing textareas
satisfies that exactly, with zero new runtime deps and no sanitization/serialization surface. Full Tiptap
WYSIWYG remains deferred. This is a deliberate, lower-risk reading of the backlog item.

### Design

#### Pure helper (`$lib/markdown-toolbar.ts`) — fully unit-tested
`applyMarkdownAction(text, selStart, selEnd, action): { text, selStart, selEnd }`, pure, no DOM.
Actions: `bold` (`**`), `italic` (`_`), `code` (`` ` ``), `strikethrough` (`~~`) — wrap selection (or
insert a placeholder + select it when empty); `heading` (`## ` line prefix), `quote` (`> ` line prefix),
`ul` (`- `), `ol` (`1. `) — per-line prefix over the selected lines; `link` → `[selection](url)` with the
`url` placeholder selected when absent. Returns the new selection so the caller can restore it.

#### Component (`$lib/components/issues/MarkdownToolbar.svelte`)
Row of `type="button"` buttons (each `aria-label`ed, keyboard reachable). Props:
`textarea: HTMLTextAreaElement | undefined`, `value: string`, `onchange: (v: string) => void`. On click:
read `textarea.selectionStart/End`, call the helper, `onchange(next.text)`, then restore focus + selection
after the DOM updates (`await tick()`). No upload logic here — it composes alongside the existing
`UploadZone`.

#### Wiring (all keep their Write/Preview toggle and stored-Markdown contract — no data migration)
- `CommentComposer.svelte` — above the textarea (covers root + every reply).
- `reports/new/+page.svelte` — above the body textarea.
- `reports/[issueId]/+page.svelte` — above the R11 edit-body textarea.

Component test (mock nothing DOM-heavy; jsdom textarea + fire clicks) asserting a couple of actions
mutate the bound value and selection correctly; plus exhaustive unit tests on the pure helper.

---

## Delivery / stages (Sonnet implements, Opus validates each, Fable holistic review at end)

- **Stage A — schema + link gate**: migration `1723100000`, `schema.ts` mirror + drift, the
  `claimDraftAttachmentsOnto` `status='ready'` gate (red-first: pending un-linkable), `db-migrations`
  go test, replay-twice idempotency.
- **Stage B — presigned server**: `s3-request-presigner` dep, storage helpers, `createPresignedAttachment`
  + `finalizePresignedAttachment` (red-first: oversized object → 413, bad magic bytes → 415 + object
  deleted, happy path → ready), the two routes.
- **Stage C — toolbar**: pure helper + exhaustive unit tests, `MarkdownToolbar.svelte`, wire the 3 surfaces.
- **Stage D — frontend upload routing + gates + e2e**: `UploadZone` two-path routing with XHR progress
  and its component test; all three dashboard gates shuffled; compose stack up + a real-MinIO integration
  proof of presign→PUT→finalize→claim (and the pending-cannot-link negative); full e2e 76+/0 skips.

Done = every item proven with its named proof, all gates green, e2e stable, docs + memory synced
(VERIFIED_STATE.md, WORKLOG.md, the §12 checkboxes, this file, the rollout memory), then commit.
