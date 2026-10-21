import { pgTable, uuid, varchar, text, timestamp, bigint, jsonb, index, uniqueIndex, integer, boolean, smallint, time, primaryKey } from 'drizzle-orm/pg-core';
import { sql } from 'drizzle-orm';

export const organizations = pgTable('organizations', {
	id: uuid('id').primaryKey().defaultRandom(),
	name: varchar('name', { length: 255 }).notNull(),
	slug: varchar('slug', { length: 255 }).notNull().unique(),
	avatarUrl: text('avatar_url'),
	createdAt: timestamp('created_at').defaultNow(),
	updatedAt: timestamp('updated_at').defaultNow(),
});

export const organizationMembers = pgTable('organization_members', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	userId: varchar('user_id', { length: 255 }).notNull(),
	role: varchar('role', { length: 20 }).notNull().default('viewer'),
	joinedAt: timestamp('joined_at').defaultNow(),
}, (table) => ({
	uniqueUserOrg: index('organization_members_user_org_unique').on(table.userId, table.organizationId),
}));

// D06: the raw invitation token is NEVER stored. `tokenHash` is sha256(token) hex-encoded (64 chars);
// invitations/organizations.ts's createOrganizationInvitation/getInvitationByToken hash on the way in
// and on the way out, so no code path in this file's callers should ever compare against a plaintext
// token. See packages/db-migrations/migrations/1722300000_invitation_token_hash_and_user_email_unique.sql
// for the migration that replaced the old plaintext `token` column with this one (and deleted every
// then-pending invitation, since plaintext cannot be backfilled to a hash).
export const organizationInvitations = pgTable('organization_invitations', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	email: varchar('email', { length: 255 }).notNull(),
	role: varchar('role', { length: 20 }).notNull().default('viewer'),
	tokenHash: varchar('token_hash', { length: 64 }).unique(),
	status: varchar('status', { length: 20 }).notNull().default('pending'),
	expiresAt: timestamp('expires_at').notNull(),
	createdAt: timestamp('created_at').defaultNow(),
	// D07: set atomically by the single conditional UPDATE that claims a redemption.
	acceptedAt: timestamp('accepted_at'),
	// D31 (residual): who issued this invitation, so claimInvitation can re-check at redemption
	// time that the inviter still holds authority to grant `role` -- see the migration's comment
	// (1722500000) for why a demoted/removed inviter's outstanding grant must not still honor.
	invitedBy: varchar('invited_by', { length: 255 }).references(() => users.id, { onDelete: 'set null' }),
	// sentinel-project-environments §4.10a, decision 28. NULL = the invitation carries no scope and
	// the accepted member is unrestricted; every existing row is NULL and none is backfilled.
	// JSONB rather than a child table: the value is a short list of {projectId, environments}
	// entries that exists only between sending an invitation and accepting it, is read exactly once,
	// and a child table would need its own cascade on a row that is already deleted on acceptance.
	//
	// NOTE: contract §4.9/§4.10a call this table `invitations`. It is `organization_invitations`
	// here and there is no table by the other name -- see the migration's comment and status/be.md.
	environmentScopes: jsonb('environment_scopes'),
});

export const userSessionPreferences = pgTable('user_session_preferences', {
	userId: varchar('user_id', { length: 255 }).primaryKey(),
	lastActiveOrganizationId: uuid('last_active_organization_id').references(() => organizations.id, { onDelete: 'set null' }),
	updatedAt: timestamp('updated_at').defaultNow(),
});

export const projects = pgTable('projects', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').references(() => organizations.id, { onDelete: 'cascade' }),
	name: varchar('name', { length: 255 }).notNull(),
	apiKey: varchar('api_key', { length: 64 }).notNull(),
	apiKeyHash: varchar('api_key_hash', { length: 128 }).notNull(),
	createdAt: timestamp('created_at').defaultNow(),
	// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §2, Q12): marks the lazily
	// auto-provisioned per-org Triage inbox project that unassigned reports land in.
	// Durable flag, not a name convention. Inbox projects are excluded from the error
	// dashboard and alert evaluation and get no API key.
	isInbox: boolean('is_inbox').notNull().default(false),
});

export const projectMembers = pgTable('project_members', {
	id: uuid('id').primaryKey().defaultRandom(),
	projectId: uuid('project_id').notNull().references(() => projects.id),
	userId: varchar('user_id', { length: 255 }).notNull(),
	role: varchar('role', { length: 20 }).notNull(),
	createdAt: timestamp('created_at').defaultNow(),
}, (table) => ({
	uniqueUserProject: index('project_members_user_project_unique').on(table.userId, table.projectId),
}));

export const issues = pgTable('issues', {
	id: uuid('id').primaryKey().defaultRandom(),
	projectId: uuid('project_id').notNull().references(() => projects.id),
	// sentinel-project-environments §4.9: part of the issue's IDENTITY, not a display field. The
	// migrated column is NOT NULL with no DEFAULT and the table's unique constraint is
	// (project_id, environment, fingerprint), so every insert must supply it -- all three writers do
	// (store.go, and reports.ts's createManualIssue/createExternalIssueReport via
	// resolveReportEnvironment). Declared without a default deliberately: a default here would let a
	// new write path omit it and still compile.
	environment: varchar('environment', { length: 50 }).notNull(),
	fingerprint: varchar('fingerprint', { length: 64 }).notNull(),
	message: text('message').notNull(),
	errorClass: varchar('error_class', { length: 255 }).notNull(),
	status: varchar('status', { length: 20 }).notNull().default('unresolved'),
	regressionStatus: varchar('regression_status', { length: 20 }).notNull().default('none'),
	issueType: varchar('issue_type', { length: 50 }).notNull().default('system_error'),
	sourceChannel: varchar('source_channel', { length: 50 }).notNull().default('ingestion_sdk'),
	assigneeType: varchar('assignee_type', { length: 20 }),
	assignedTo: varchar('assigned_to', { length: 255 }),
	// N7c (1723500000_add_claimed_at.sql, A03): when the CURRENT claim (assigneeType/assignedTo)
	// was made. Set by claimIssue, cleared by releaseClaim (both non-force and force). NULL means
	// either unclaimed, or a pre-migration claim -- reapStaleClaims (retention.ts) treats NULL as
	// stale-eligible. Not exposed in any /api/agent response yet (that's N7e).
	claimedAt: timestamp('claimed_at', { withTimezone: true }),
	resolvedInVersion: varchar('resolved_in_version', { length: 100 }),
	resolvedAt: timestamp('resolved_at', { withTimezone: true }),
	resolvedByType: varchar('resolved_by_type', { length: 20 }),
	resolvedBy: varchar('resolved_by', { length: 255 }),
	regressionCount: integer('regression_count').notNull().default(0),
	lastRegressedAt: timestamp('last_regressed_at', { withTimezone: true }),
	firstSeen: timestamp('first_seen').defaultNow(),
	lastSeen: timestamp('last_seen').defaultNow(),
	count: bigint('count', { mode: 'number' }).notNull().default(1),
	// Manual Issues M1 (§2, Q11): set by an agent's blocking question, auto-cleared on any
	// human reply. Nullable CHECK ('reporter'|'team'); NULL = not blocked. Not a new `status`
	// value -- the existing check_status constraint on `status` is untouched.
	waitingOn: varchar('waiting_on', { length: 20 }),
	// N9 (AGENT_WORKER_PLAN C12): when the CURRENT blocking question was asked. Set alongside
	// waitingOn (comments.ts blocking branch), cleared to NULL alongside it (human reply, or
	// resolve/ignore in issues.ts). Only meaningful while waitingOn is non-NULL.
	waitingSince: timestamp('waiting_since', { withTimezone: true }),
	// sentinel-affected-users §4.6 (1724500000_add_issue_affected_rollup.sql). By definition
	// (decision 12) the cardinality of issueAffectedUsers / issueAffectedTenants for this issue,
	// within the retention window: the processor bumps it only when its side-table upsert INSERTs
	// (§4.3) and the retention sweep recomputes it in the same transaction as its DELETE (§4.5), so
	// the counts can go down. `.default(0)` mirrors the migrated DEFAULT 0 and is deliberately KEPT
	// (unlike `environment`'s): 0 is the correct value for every row that predates the count
	// (decision 4 -- no backfill) and for every INSERT path that does not know about it
	// (reports.ts's manual/external issues). Integer, never null: the agent API declares both as
	// `z.number().int()` (IssueRowSchema / AgentIssueListItemSchema, decision 14).
	affectedUserCount: integer('affected_user_count').notNull().default(0),
	affectedTenantCount: integer('affected_tenant_count').notNull().default(0),
	// sentinel-releases-snooze §4.5 (1724900000_add_releases_and_issue_snooze.sql). A snooze is
	// `status = 'ignored'` with a condition -- there is no fourth status (decision 2): a date
	// (`snoozedUntil`) and/or an occurrence target (`snoozeOccurrenceTarget`, with the `count` it
	// was set at in `snoozeCountAt` -- the migrated CHECK check_snooze_pair makes them travel
	// together, and check_snooze_target_positive rejects a target of 0). The migrated CHECK
	// check_snooze_only_when_ignored is enforced on every UPDATE: a write that changes `status` off
	// `ignored` must null all three IN THE SAME STATEMENT (§4.2's resolve/unresolve arms, §4.3's
	// regression arm) or it is a class-23 error. Nullable with NO `.default()` deliberately (unlike
	// `affectedUserCount`'s): NULL is the correct value for every pre-feature row and for every
	// write path that does not know about them (reports.ts's manual/external issues), and a default
	// here would let a new write path invent a condition and still compile. TIMESTAMPTZ / BIGINT
	// mirror the migrated types (`snoozeOccurrenceTarget` compares against `count`, a bigint).
	snoozedUntil: timestamp('snoozed_until', { withTimezone: true }),
	snoozeOccurrenceTarget: bigint('snooze_occurrence_target', { mode: 'number' }),
	snoozeCountAt: bigint('snooze_count_at', { mode: 'number' }),
	// sentinel-releases-snooze §4.5: the release the issue's FIRST occurrence carried, set once by
	// the processor on insert (§4.3) and once by the 1725000000 backfill from the earliest
	// RETAINED occurrence (decision 31 -- regardless of version, kept only if it had one). NULL
	// when that occurrence carried no version, and NULL for good for an issue created by an
	// old-tag pod in the rollout window (§8); the detail page renders `—`. Never rewritten. The
	// releases page counts "issues introduced" on it (§4.4, idx_issues_project_first_release).
	firstReleaseVersion: varchar('first_release_version', { length: 100 }),
});

// sentinel-releases-snooze §4.5 (1724900000_add_releases_and_issue_snooze.sql): one row per
// (project, version) the processor has seen an occurrence for. The composite primary key is the ON
// CONFLICT target of the processor's upsert (§4.3, inside StoreEvent's transaction -- decision 5)
// and the lookup both readers make; ordered by `firstSeen`, never by parsing the version (decision
// 1), which is what idx_releases_project_first_seen serves for the detail page's picker (§4.1, the
// 50 newest) and the releases page (§4.4, the 200 most recent). Written by the processor
// (store.go) and once by the backfill migration (1725000000); the dashboard never inserts here.
//
// `version` is the exact string the occurrence carried (§1's empty-version rules stay: the
// processor maps '' to "absent" BEFORE the statement, and the migrated CHECK (version <> '') is the
// tripwire for a regression of that). VARCHAR(100) matches errorOccurrences.releaseVersion and
// issues.resolvedInVersion. `firstSeen`/`lastSeen` are the occurrence's event timestamp (decision
// 7), TIMESTAMPTZ like errorOccurrences.createdAt -- and backfilled `firstSeen` is exact only
// inside the retention window (decision 31). `occurrenceCount` is lifetime: nothing cascades from
// issues or error_occurrences, so a release outlives every occurrence that carried it and retention
// never decrements it (§4.4, §12); ON DELETE CASCADE from projects only. `.default(0)` mirrors the
// migrated DEFAULT 0.
export const releases = pgTable('releases', {
	projectId: uuid('project_id').notNull().references(() => projects.id, { onDelete: 'cascade' }),
	version: varchar('version', { length: 100 }).notNull(),
	firstSeen: timestamp('first_seen', { withTimezone: true }).notNull(),
	lastSeen: timestamp('last_seen', { withTimezone: true }).notNull(),
	occurrenceCount: bigint('occurrence_count', { mode: 'number' }).notNull().default(0),
}, (table) => ({
	pk: primaryKey({ columns: [table.projectId, table.version] }),
	byProjectFirstSeen: index('idx_releases_project_first_seen').on(table.projectId, table.firstSeen),
}));

// sentinel-sourcemaps §4.5 (1725100000_add_sourcemaps.sql): one row per (project, release,
// minified path) map that POST /api/sourcemaps (§4.1) has stored. UNIQUE (projectId,
// releaseVersion, minifiedPath) is that route's upsert target and the processor's symbolication
// lookup (§4.4); idx_sourcemaps_project_release serves §4.2's list and §4.6's grouped count per
// release. NO FK to `releases`: a map is uploaded BEFORE the first occurrence of its version exists
// (CI uploads at build time; `releases` rows are created by StoreEvent), so the FK would refuse
// the normal case. ON DELETE CASCADE from projects deletes rows; the objects stay (§4.1 *Orphan
// objects*, decision 12's follow-on owns the bucket walk).
//
// `releaseVersion` is stored exactly as CI sent it (decision 25 -- no trim, no `v` stripping),
// byte for byte the string the SDK sends on events, because it is the join key; VARCHAR(100) is
// errorOccurrences.releaseVersion's bound. `minifiedPath` is the frame path AFTER §4.4's
// normalisation (decision 22), VARCHAR(512) = the ingest path's MaxFilePathLength. `storageKey` is
// UNIQUE (idx_sourcemaps_storage_key), like attachments.storageKey: each row owns exactly one
// object, and the key folds the path in (decision 17) so two paths with identical bytes are two
// objects. `sha256` is the hex digest of the raw bytes -- the migrated column is CHAR(64), declared
// here with the same 64-character bound in the `string` family tests/schema-drift.test.ts compares
// (`character` and `character varying` are one family there); a 64-hex digest never pads.
// `sizeBytes` is the raw byte size (§4.4 accounts the cache on it; CHECK > 0 migrated). Written by
// the upload route only; the processor reads it, and nothing in this file's callers deletes rows.
export const sourcemaps = pgTable('sourcemaps', {
	id: uuid('id').primaryKey().defaultRandom(),
	projectId: uuid('project_id').notNull().references(() => projects.id, { onDelete: 'cascade' }),
	releaseVersion: varchar('release_version', { length: 100 }).notNull(),
	minifiedPath: varchar('minified_path', { length: 512 }).notNull(),
	storageKey: varchar('storage_key', { length: 1024 }).notNull(),
	sizeBytes: bigint('size_bytes', { mode: 'number' }).notNull(),
	sha256: varchar('sha256', { length: 64 }).notNull(),
	uploadedAt: timestamp('uploaded_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	projectReleasePath: uniqueIndex('sourcemaps_project_id_release_version_minified_path_key').on(table.projectId, table.releaseVersion, table.minifiedPath),
	byStorageKey: uniqueIndex('idx_sourcemaps_storage_key').on(table.storageKey),
	byProjectRelease: index('idx_sourcemaps_project_release').on(table.projectId, table.releaseVersion),
}));

// sentinel-affected-users §4.6: one row per DISTINCT (issue, user) pair seen within the retention
// window. The composite primary key is what makes "distinct" a database fact and is the ON
// CONFLICT target of the processor's upsert (§4.3); `issues.affectedUserCount` is its cardinality
// (decision 12). Written by the processor (store.go) and swept by retention.ts (§4.5, `last_seen <
// cutoff`, which is what the `last_seen` index serves); the dashboard never inserts here.
//
// `userId` is the exact byte string the SDK put in event metadata (decision 6 -- no
// normalisation), read BEFORE event.Normalize replaces it with `<UUID>`/`<EMAIL>` (decision 3),
// and bounded by decision 5 before the statement (non-empty, <= 255 bytes, no control characters);
// the migrated CHECK (user_id <> '') is the tripwire for a regression of that. VARCHAR(255) matches
// errorSearchIndex.userId, the column these values were always meant for. Timestamps are the
// occurrence's created_at, not now() (decision 9), so the sweep's cutoff means the same thing here
// as on error_occurrences.
export const issueAffectedUsers = pgTable('issue_affected_users', {
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	userId: varchar('user_id', { length: 255 }).notNull(),
	firstSeen: timestamp('first_seen', { withTimezone: true }).notNull(),
	lastSeen: timestamp('last_seen', { withTimezone: true }).notNull(),
}, (table) => ({
	pk: primaryKey({ columns: [table.issueId, table.userId] }),
	byLastSeen: index('idx_issue_affected_users_last_seen').on(table.lastSeen),
}));

// The tenant twin of issueAffectedUsers -- same shape, same writers, same sweep, keyed on the
// reporting application's tenant string (packages/sdk-go/context.go: WithTenant), which is NOT
// Sentinel's own organization/project tenancy (contract header).
export const issueAffectedTenants = pgTable('issue_affected_tenants', {
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	tenantId: varchar('tenant_id', { length: 255 }).notNull(),
	firstSeen: timestamp('first_seen', { withTimezone: true }).notNull(),
	lastSeen: timestamp('last_seen', { withTimezone: true }).notNull(),
}, (table) => ({
	pk: primaryKey({ columns: [table.issueId, table.tenantId] }),
	byLastSeen: index('idx_issue_affected_tenants_last_seen').on(table.lastSeen),
}));

// sentinel-issue-trend §4.6 (1724700000_add_issue_occurrence_buckets.sql): one row per (issue, UTC
// hour) that saw at least one occurrence, `count` being how many. The composite primary key is the ON
// CONFLICT target of the processor's upsert (§4.3, inside StoreEvent's transaction -- decision 1) and
// the range scan both readers make (`issue_id = … AND bucket_start BETWEEN …`); nothing on `issues`
// is denormalised from it (decision 3), so the series is always read from here. Written by the
// processor (store.go) and once by the backfill migration (1724800000); swept by retention.ts (§4.5,
// `bucket_start < cutoff`, which is what the `bucket_start` index serves); the dashboard never
// inserts here.
//
// `bucketStart` is the occurrence's event timestamp truncated to the UTC hour IN GO
// (`occ.CreatedAt.UTC().Truncate(time.Hour)`, §3 / decision 2), never now() and never a SQL
// date_trunc on a timestamptz (session-zone dependent) -- the same clock as `issues.lastSeen` and
// as retention's `errorOccurrences.createdAt` comparison, so buckets and the occurrences behind them
// age out together. `withTimezone: true` mirrors the migrated TIMESTAMPTZ so `< cutoff` is the same
// comparison retention makes on error_occurrences. `count` is INTEGER NOT NULL with a migrated CHECK
// (count > 0): a row exists because an occurrence did; a series entry may be 0, a row may not. No
// default on purpose -- a row is written with its count or not at all.
export const issueOccurrenceBuckets = pgTable('issue_occurrence_buckets', {
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	bucketStart: timestamp('bucket_start', { withTimezone: true }).notNull(),
	count: integer('count').notNull(),
}, (table) => ({
	pk: primaryKey({ columns: [table.issueId, table.bucketStart] }),
	byBucketStart: index('idx_issue_occurrence_buckets_bucket_start').on(table.bucketStart),
}));

// Matches packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql — the table
// has old_value/new_value JSONB columns, NOT a single `metadata` column. actor_type/actor_id are NOT NULL
// with a CHECK on actor_type ('user'|'agent'|'system'). event_type CHECK allows
// 'status_changed'|'assigned'|'unassigned'|'regressed'|'ai_analysis'|'linked' (note: 'status_changed', not
// 'status_change' — see queries/issues.ts), extended by
// 1722600000_add_manual_issue_reports_and_comments.sql (Manual Issues M1, design §6) with
// 'commented'|'claimed'|'claim_released'|'progress_update'|'question_asked'|'question_answered'|'moved'|
// 'attachment_added'|'report_edited'.
export const issueActivity = pgTable('issue_activity', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	eventType: varchar('event_type', { length: 50 }).notNull(), // see CHECK above for the full allowed set
	actorType: varchar('actor_type', { length: 20 }).notNull(), // 'user' | 'agent' | 'system'
	actorId: varchar('actor_id', { length: 255 }).notNull(),
	oldValue: jsonb('old_value'),
	newValue: jsonb('new_value'),
	createdAt: timestamp('created_at').defaultNow(),
	// N1a (1723200000_add_activity_seq.sql), source of truth is that migration: GENERATED ALWAYS
	// AS IDENTITY cursor for the agent events feed. Gaps and brief commit-order inversion are
	// expected -- consumers apply a 2s created_at lag guard. Never set on insert; every
	// issueActivity insert site uses an explicit column list and does not reference `seq`.
	// NOT NULL at the database level (identity columns always are). drizzle-orm 0.30.10 predates
	// `.generatedAlwaysAsIdentity()` (added 0.32), so there is no first-class way to mark this
	// column "database-generated" for insert-type purposes. `.default(sql\`...\`)` below is a
	// type-only stand-in for that: it makes drizzle treat `seq` as optional on `.values()` (every
	// existing issueActivity insert site uses an explicit column list that omits it, and must keep
	// compiling) without emitting anything at runtime that matters, since goose -- not
	// drizzle-kit -- owns the real DDL. The expression itself is the accurate description of an
	// identity column's default: `nextval` over the sequence Postgres created for it.
	seq: bigint('seq', { mode: 'number' })
		.notNull()
		.default(sql`nextval(pg_get_serial_sequence('issue_activity', 'seq'))`),
});

// N8 (docs/audits/AGENT_AUTOMATION_AUDIT_2026-08-14.md A04, DECISIONS.md D20): a terminal marker
// that OUTLIVES the deleted issue it describes. It deliberately has NO FK back to `issues`
// (issueActivity's FK is exactly why activity rows cascade away with the issue), so
// organizationId/projectId/message/type/assignee are denormalized snapshots taken at deletion time.
// Surfaced in the agent events feed (queries/events.ts) via UNION with a synthetic eventType
// 'issue_deleted'. Migration: 1723700000_add_issue_tombstones.sql.
export const issueTombstones = pgTable('issue_tombstones', {
	id: uuid('id').primaryKey().defaultRandom(),
	// The id of the now-deleted issue. Not a FK -- the referenced row is gone by construction.
	issueId: uuid('issue_id').notNull(),
	organizationId: uuid('organization_id').notNull(),
	projectId: uuid('project_id').notNull(),
	issueMessage: text('issue_message'),
	issueType: varchar('issue_type', { length: 50 }),
	// Snapshot of the claim at deletion time, so a claim-holding agent still discovers the deletion
	// via `?claimed=me` even though the assignment lived on the (now deleted) issues row.
	assigneeType: varchar('assignee_type', { length: 20 }),
	assignedTo: varchar('assigned_to', { length: 255 }),
	reason: varchar('reason', { length: 50 }).notNull().default('retention'),
	deletedAt: timestamp('deleted_at', { withTimezone: true }).notNull().defaultNow(),
	// Shares issue_activity's IDENTITY sequence (see migration) so tombstones interleave into the
	// one monotonic seq order the events-feed cursor reads by. Same drizzle type-only default trick
	// as issueActivity.seq above: the real DDL is owned by goose, not drizzle-kit.
	seq: bigint('seq', { mode: 'number' })
		.notNull()
		.default(sql`nextval(pg_get_serial_sequence('issue_activity', 'seq'))`),
});

// Manual Issues M1 (design §2, §5): a manual issue is an `issues` row (issue_type='user_report') plus
// this 1:1 companion. reporterId references "user".id (better-auth's TEXT id) — VARCHAR(255) to match
// every other *_user_id column in this schema. severity CHECK allows 'low'|'medium'|'high'|'critical'.
export const manualIssueReports = pgTable('manual_issue_reports', {
	issueId: uuid('issue_id').primaryKey().references(() => issues.id, { onDelete: 'cascade' }),
	reporterId: varchar('reporter_id', { length: 255 }).notNull().references(() => users.id),
	bodyMd: text('body_md').notNull(),
	severity: varchar('severity', { length: 20 }).notNull().default('medium'), // 'low' | 'medium' | 'high' | 'critical'
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
});

// Manual Issues M1 (design §5): Slack-like one-level threads on ANY issue (both issue_type values).
// parent_id NULL = root comment; a reply's parent_id points at the SAME parent as the comment it
// replies to (one level deep, not infinitely nested). author_type CHECK allows 'user'|'agent'.
// `blocking` marks an agent question that also sets issues.waitingOn (design §7, Q11).
export const issueComments = pgTable('issue_comments', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	parentId: uuid('parent_id').references((): any => issueComments.id, { onDelete: 'cascade' }),
	authorType: varchar('author_type', { length: 20 }).notNull(), // 'user' | 'agent'
	authorId: varchar('author_id', { length: 255 }).notNull(),
	blocking: boolean('blocking').notNull().default(false),
	bodyMd: text('body_md').notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	editedAt: timestamp('edited_at', { withTimezone: true }),
});

// Matches the same migration: relation_type has no default and its CHECK only allows
// 'linked_to'|'caused_by'|'duplicate_of' (never 'related'/'duplicate' — a default of 'related' violates
// the constraint on every insert that omits it). created_by_type/created_by are NOT NULL.
export const issueRelations = pgTable('issue_relations', {
	id: uuid('id').primaryKey().defaultRandom(),
	sourceIssueId: uuid('source_issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	targetIssueId: uuid('target_issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	relationType: varchar('relation_type', { length: 50 }).notNull(), // 'linked_to' | 'caused_by' | 'duplicate_of'
	createdByType: varchar('created_by_type', { length: 20 }).notNull(), // 'user' | 'agent' | 'system'
	createdBy: varchar('created_by', { length: 255 }).notNull(),
	createdAt: timestamp('created_at').defaultNow(),
});

// Manual Issues M2 (design §4): matches
// 1722700000_add_attachments.sql. orgId NOT NULL — tenant scope for the download access check
// must never depend on walking through issueId/commentId, both nullable while drafting.
// issueId/commentId are mutually exclusive-or-neither (attachments_single_parent_check):
// unlinked while drafting, then linked to exactly one of an issue or a comment, never both.
// uploaderType CHECK allows 'user'|'agent'. storageKey is UNIQUE (idx_attachments_storage_key).
export const attachments = pgTable('attachments', {
	id: uuid('id').primaryKey().defaultRandom(),
	orgId: uuid('org_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	issueId: uuid('issue_id').references(() => issues.id, { onDelete: 'cascade' }),
	commentId: uuid('comment_id').references(() => issueComments.id, { onDelete: 'cascade' }),
	uploaderType: varchar('uploader_type', { length: 20 }).notNull(), // 'user' | 'agent'
	uploaderId: varchar('uploader_id', { length: 255 }).notNull(),
	filename: varchar('filename', { length: 512 }).notNull(),
	contentType: varchar('content_type', { length: 255 }).notNull(),
	sizeBytes: bigint('size_bytes', { mode: 'number' }).notNull(),
	storageKey: varchar('storage_key', { length: 1024 }).notNull(),
	// M6 Feature A (1723100000_add_attachment_status.sql): 'pending' | 'ready'. Presigned-upload
	// rows start 'pending' and only flip to 'ready' at finalize, after the same
	// sniffContentType/resolveContentType validation the proxy path runs inline. A 'pending'
	// object must never be linkable -- claimDraftAttachmentsOnto in queries/reports.ts enforces
	// this as the single chokepoint. Defaults to 'ready' for the existing proxied-upload path.
	status: varchar('status', { length: 16 }).notNull().default('ready'),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
});

// Manual Issues M4 (design §8): matches
// 1722800000_add_notifications_and_subscriptions.sql. subscriberId is NOT an FK -- it names a
// user or an agent depending on subscriberType, exactly like issueComments' authorType/authorId
// pair. UNIQUE(issueId, subscriberType, subscriberId) backs the idempotent upsert in
// queries/subscriptions.ts.
export const issueSubscriptions = pgTable('issue_subscriptions', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	subscriberType: varchar('subscriber_type', { length: 20 }).notNull(), // 'user' | 'agent'
	subscriberId: varchar('subscriber_id', { length: 255 }).notNull(),
	reason: varchar('reason', { length: 20 }).notNull(), // 'reporter' | 'claimant' | 'participant' | 'manual'
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
});

// Manual Issues M4 (design §8): matches 1722800000_add_notifications_and_subscriptions.sql.
// userId IS a real FK ("user".id) -- unlike issueSubscriptions, a notifications row is always a
// user's inbox entry in M4 (agent subscribers get no row -- they poll, see notify.ts). kind CHECK
// allows 'commented'|'claimed'|'status_changed'|'resolved'|'linked'|'progress_update'|
// 'question_asked'. actorType CHECK allows 'user'|'agent'|'system'.
export const notifications = pgTable('notifications', {
	id: uuid('id').primaryKey().defaultRandom(),
	userId: text('user_id').notNull().references(() => users.id, { onDelete: 'cascade' }),
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	kind: varchar('kind', { length: 30 }).notNull(),
	actorType: varchar('actor_type', { length: 20 }).notNull(),
	actorId: varchar('actor_id', { length: 255 }).notNull(),
	payload: jsonb('payload'),
	readAt: timestamp('read_at', { withTimezone: true }),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	// PR13 remediation R5 (1723000000_pr13_remediation.sql): set post-send by
	// sendIssueNotificationEmails on the rows it actually emailed. isThrottled queries
	// max(emailedAt) within the 15-min window instead of counting notification rows (which
	// included rows that were themselves throttled and never emailed).
	emailedAt: timestamp('emailed_at', { withTimezone: true }),
});

export const errorOccurrences = pgTable('error_occurrences', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id),
	environment: varchar('environment', { length: 50 }).notNull(),
	platform: varchar('platform', { length: 50 }).notNull(),
	releaseVersion: varchar('release_version', { length: 100 }),
	stacktrace: jsonb('stacktrace').notNull().default([]),
	metadata: jsonb('metadata').notNull().default({}),
	traceId: varchar('trace_id', { length: 64 }),
	spanId: varchar('span_id', { length: 64 }),
	// Idempotency key (P9-3). Matches 1722200000_add_error_occurrences_event_id.sql: nullable, '' never
	// stored (NULLIF at the insert site maps proto3's absent-field default to NULL; a DB CHECK is the
	// tripwire if that mapping is ever bypassed), partial-unique on (issue_id, event_id). Hand-maintained
	// mirror — this file has drifted from the real schema before (see the alertConfigs comment above), so
	// keep it named after the migration that is the actual source of truth. No dashboard code reads this
	// yet; that is expected at this stage of the plan.
	eventId: varchar('event_id', { length: 64 }),
	// sentinel-occurrence-dedup-project §4.2 (1725200000_add_error_occurrences_project_id_dedup.sql):
	// nullable, no default, no FK — rows written before that release carry NULL until its batched
	// backfill (issues.project_id via issue_id) reaches them, and the processor is the only writer.
	// Widens the occurrence dedup key from (issue_id, event_id) to (project_id, event_id); the partial
	// unique index is declared below. Same hand-maintained-mirror caveat as eventId: the migration is
	// the source of truth, not this file. No dashboard code reads this yet.
	projectId: uuid('project_id'),
	// sentinel-event-schema-v2 §5.2/§5.5 (1725300000_add_error_occurrences_event_v2_fields.sql): the 17
	// per-occurrence v2 columns, all nullable, no default, and NULL on every row written before that
	// release. The processor is the only writer. Same hand-maintained-mirror caveat as eventId: the
	// migration is the source of truth (tests/event-v2-mirror.test.ts compares the two). Two choices:
	// traceFlags is bigint in 'number' mode, as issues.count is, because without it the driver returns
	// a string (a W3C trace-flags value is at most 255, so number is exact); and the user column is
	// event_user, because `user` is a reserved word in PostgreSQL. The partial GIN index on tags is NOT
	// mirrored: drizzle-orm 0.30.x cannot name an access method, so the only declaration available would
	// claim a btree index that does not exist.
	level: varchar('level', { length: 16 }),
	message: text('message'),
	errorClass: varchar('error_class', { length: 255 }),
	logger: varchar('logger', { length: 64 }),
	serverName: varchar('server_name', { length: 256 }),
	transaction: varchar('transaction', { length: 200 }),
	dist: varchar('dist', { length: 64 }),
	source: varchar('source', { length: 64 }),
	traceFlags: bigint('trace_flags', { mode: 'number' }),
	receivedAt: timestamp('received_at', { withTimezone: true }),
	sdk: jsonb('sdk'),
	eventUser: jsonb('event_user'),
	tags: jsonb('tags'),
	contexts: jsonb('contexts'),
	request: jsonb('request'),
	breadcrumbs: jsonb('breadcrumbs'),
	exception: jsonb('exception'),
	createdAt: timestamp('created_at').defaultNow(),
	// sentinel-issue-tag-facets §3.6 (the *_add_issue_tag_values.sql migration): the "counted at insert"
	// marker. Nullable, no default: rows stored before that migration are NULL and are counted by the
	// backfill; the BEFORE INSERT trigger sets TRUE on every row inserted since, whatever the insert names.
	// No dashboard code reads or writes it (the agent API projects occurrence columns explicitly).
	tagRollupApplied: boolean('tag_rollup_applied'),
	// sentinel-reporting-and-dashboard D18 (1726000000_add_error_occurrences_service_fields.sql): where the error came from,
	// promoted out of `tags` by the processor at store time. `tagSource` is tags.source (core's capture site) and is NOT the
	// `source` column above, which is the wire's capture origin (window.onerror, sentry-compat) and keeps its meaning.
	// Nothing in the dashboard writes them; the list reads the `service` facet from issue_tag_values, and Databend reads
	// them through CDC.
	service: text('service'),
	tagSource: text('tag_source'),
	requestScoped: boolean('request_scoped').notNull().default(false),
}, (table) => ({
	idxOccurrencesIssueRelease: index('idx_occurrences_issue_release').on(table.issueId, table.releaseVersion),
	// 1725200000: partial unique index on (project_id, event_id), built CONCURRENTLY by the migration.
	// The .where(...) predicate is load-bearing — without it this would claim a plain UNIQUE over every
	// pre-release NULL row and every NULL event_id, which is not what the database enforces. The older
	// uq_error_occurrences_issue_event (1722200000) still exists in the database and is deliberately
	// not mirrored here, as before.
	uqOccurrencesProjectEvent: uniqueIndex('uq_error_occurrences_project_event')
		.on(table.projectId, table.eventId)
		.where(sql`project_id IS NOT NULL AND event_id IS NOT NULL`),
	// sentinel-user-report-issue §4.3/§4.6 (1724100000_add_user_report_intake_and_oncall.sql): the triage
	// matcher scans a candidate issue's recent occurrences newest-first inside
	// REPORT_MATCH_WINDOW_MINUTES of the report. The release-keyed index above cannot serve that scan.
	idxOccurrencesIssueCreated: index('idx_occurrences_issue_created').on(table.issueId, table.createdAt),
}));

// sentinel-issue-tag-facets §3.1 / §8.5: the per-issue tag rollup. WRITTEN ONLY BY THE error_occurrences
// TRIGGERS the migration creates (every insert, delete, update and truncate of an occurrence keeps it exact
// inside the same transaction); no dashboard code may insert, update or delete it. The panel and /search
// read it. `value = ''` is the overflow row ("Other"): past 1000 named values per (issue, key) the rest are
// counted there. first_seen / last_seen are BOUNDS, not exact values: the least and greatest created_at
// of the occurrences ADDED to the row since it was created; retention's subtraction does not move them, so
// after retention first_seen may predate every surviving occurrence. Nothing displays them.
export const issueTagValues = pgTable('issue_tag_values', {
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	key: varchar('key', { length: 32 }).notNull(),
	value: text('value').notNull(),
	count: bigint('count', { mode: 'number' }).notNull(),
	firstSeen: timestamp('first_seen', { withTimezone: true }).notNull(),
	lastSeen: timestamp('last_seen', { withTimezone: true }).notNull(),
}, (table) => ({
	pk: primaryKey({ columns: [table.issueId, table.key, table.value] }),
	idxIssueTagValuesKeyValue: index('idx_issue_tag_values_key_value').on(table.key, table.value, table.issueId),
}));

// The backfill's one-row state (§4.2): read by nothing in the dashboard; mirrored because
// schema-drift.test.ts compares every table.
export const issueTagValuesBackfill = pgTable('issue_tag_values_backfill', {
	singleton: boolean('singleton').primaryKey().default(true),
	cursorId: uuid('cursor_id'),
	done: boolean('done').notNull().default(false),
	rowsCounted: bigint('rows_counted', { mode: 'number' }).notNull().default(0),
	startedAt: timestamp('started_at', { withTimezone: true }),
	finishedAt: timestamp('finished_at', { withTimezone: true }),
	updatedAt: timestamp('updated_at', { withTimezone: true }),
});

// Repaired rollup drift (§3.4 W11), written by the triggers; the retention route reports it
// (reported_at), rebuilds deferred issues (rebuilt_at) and hands the too-large ones to the operator
// (operator_only_at). No FK, so a record outlives its issue. `id` is GENERATED ALWAYS AS IDENTITY in the
// database (drizzle-orm 0.30 cannot declare identity), so no dashboard code inserts into it.
export const issueTagValuesDrift = pgTable('issue_tag_values_drift', {
	id: bigint('id', { mode: 'number' }).primaryKey(),
	issueId: uuid('issue_id').notNull(),
	contributions: bigint('contributions', { mode: 'number' }).notNull(),
	source: text('source').notNull(),
	detectedAt: timestamp('detected_at', { withTimezone: true }).notNull().defaultNow(),
	reportedAt: timestamp('reported_at', { withTimezone: true }),
	rebuiltAt: timestamp('rebuilt_at', { withTimezone: true }),
	operatorOnlyAt: timestamp('operator_only_at', { withTimezone: true }),
});

export const errorSearchIndex = pgTable('error_search_index', {
	occurrenceId: uuid('occurrence_id').primaryKey().references(() => errorOccurrences.id, { onDelete: 'cascade' }),
	userId: varchar('user_id', { length: 255 }),
	tenantId: varchar('tenant_id', { length: 255 }),
	traceId: varchar('trace_id', { length: 64 }),
	spanId: varchar('span_id', { length: 64 }),
	requestId: varchar('request_id', { length: 255 }),
});

// Matches 1716508800_init.sql: the real columns are channel_config (JSONB) and
// frequency_window_seconds (integer) — NOT channel_target (varchar) / window_seconds. This drift made
// every GET/POST/PUT/DELETE on /api/alerts 500 (42703) in the same way as the issue_activity/issue_relations
// drift this file was already carrying; found and fixed alongside it (P6-3).
//
// organizationId/projectId shape matches 1722100000_add_alert_config_org_layer.sql, which in turn mirrors
// projectApiKeys above: organizationId NOT NULL, projectId nullable, NULL meaning organization-wide (applies
// to every project in the organization). A composite FK on (projectId, organizationId) enforces at the DB
// level that a project-scoped config's project actually belongs to the organization it names.
// sentinel-project-environments §4.9's registry. A row exists here because a PERSON declared it
// (decision 8): the processor TOUCHES this table (first_seen_at/last_seen_at, §4.8) and never
// inserts, and the migration creates no rows at all. Every project's registry is empty on day one
// by design, so §4.1's empty-collection case is the normal state, not an edge one.
//
// `name` is the join key and matches issues.environment and error_occurrences.environment. It is
// immutable (decision 3) -- `displayName` is what a rename changes -- which is what makes a
// name-keyed grant safe.
export const projectEnvironments = pgTable('project_environments', {
	id: uuid('id').primaryKey().defaultRandom(),
	projectId: uuid('project_id').notNull().references(() => projects.id, { onDelete: 'cascade' }),
	name: varchar('name', { length: 50 }).notNull(),
	displayName: varchar('display_name', { length: 64 }).notNull(),
	// Consumed by §4.7's new-config default (decision 11) and by nothing else. "At most one default
	// per project" is a database fact, not a hope in the handler: the migration carries a partial
	// unique index on (project_id) WHERE is_default.
	isDefault: boolean('is_default').notNull().default(false),
	// Display only, NEVER access. §4.10's grants are access.
	hidden: boolean('hidden').notNull().default(false),
	// NULL until §4.8's UPDATE first touches the row.
	firstSeenAt: timestamp('first_seen_at', { withTimezone: true }),
	lastSeenAt: timestamp('last_seen_at', { withTimezone: true }),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	uniqueProjectName: uniqueIndex('project_environments_project_id_name_key').on(table.projectId, table.name),
}));

// sentinel-project-environments §2.2's grant. The ABSENCE of a row is what makes this narrow
// nobody: no row for (user, project) means that user is unrestricted in that project, so the
// migration creates none and nothing about who sees what changes when it lands.
//
// `environments` is a name-keyed TEXT[] with a CHECK that it is non-empty -- §4.10 rejects `[]`
// outright, because an empty grant would silently mean "sees nothing" rather than "sees everything".
export const projectEnvironmentGrants = pgTable('project_environment_grants', {
	userId: text('user_id').notNull().references(() => users.id, { onDelete: 'cascade' }),
	projectId: uuid('project_id').notNull().references(() => projects.id, { onDelete: 'cascade' }),
	environments: text('environments').array().notNull(),
	createdBy: text('created_by').notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	pk: primaryKey({ columns: [table.userId, table.projectId] }),
	byProject: index('idx_project_environment_grants_project').on(table.projectId),
}));

export const alertConfigs = pgTable('alert_configs', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id),
	projectId: uuid('project_id').references(() => projects.id),
	channel: varchar('channel', { length: 20 }).notNull(),
	channelConfig: jsonb('channel_config').notNull().default({}),
	frequencyThreshold: integer('frequency_threshold').notNull().default(50),
	frequencyWindowSeconds: integer('frequency_window_seconds').notNull().default(60),
	enabled: boolean('enabled').notNull().default(true),
	// sentinel-project-environments §4.9, decisions 6 and 11. NULL = every environment, which is
	// what every existing row is: the migration adds the column with DEFAULT NULL and backfills
	// nothing, because a schema change must not silently change who is alerted. TEXT[] rather than a
	// join table so refreshConfigs' single SELECT (processor-go/alerts/dispatcher.go) stays a single
	// SELECT.
	environments: text('environments').array(),
	createdAt: timestamp('created_at', { withTimezone: true }).defaultNow(),
});

export const auditLogs = pgTable('audit_logs', {
	id: uuid('id').primaryKey().defaultRandom(),
	action: varchar('action', { length: 100 }).notNull(),
	resourceType: varchar('resource_type', { length: 50 }),
	resourceId: uuid('resource_id'),
	actorId: varchar('actor_id', { length: 255 }).notNull(),
	metadata: jsonb('metadata').notNull().default({}),
	createdAt: timestamp('created_at').defaultNow(),
});

export const settings = pgTable('settings', {
	key: varchar('key', { length: 255 }).primaryKey(),
	value: text('value').notNull(),
	createdAt: timestamp('created_at').defaultNow(),
	updatedAt: timestamp('updated_at').defaultNow(),
});

// D30: idx_user_email_lower_unique (added by
// 1722300000_invitation_token_hash_and_user_email_unique.sql — a raw-SQL expression index that this
// drizzle-orm version's schema builder cannot express directly, so it is not repeated here) enforces
// that no two rows share a case-insensitive email. Email/organization lookups (e.g. the
// already-a-member guard on invitation creation) MUST normalize to lower(email) to match it, and
// writes should normalize the stored value too rather than relying on this index alone.
export const users = pgTable('user', {
	id: text('id')
		.primaryKey()
		.$defaultFn(() => crypto.randomUUID()),
	name: text('name'),
	email: text('email').notNull(),
	emailVerified: timestamp('email_verified', { mode: 'date' }),
	image: text('image'),
});

export const accounts = pgTable(
	'account',
	{
		userId: text('user_id')
			.notNull()
			.references(() => users.id, { onDelete: 'cascade' }),
		type: text('type').notNull(),
		provider: text('provider').notNull(),
		providerAccountId: text('provider_account_id').notNull(),
		refresh_token: text('refresh_token'),
		access_token: text('access_token'),
		expires_at: integer('expires_at'),
		token_type: text('token_type'),
		scope: text('scope'),
		id_token: text('id_token'),
		session_state: text('session_state'),
	},
	(account) => ({
		compoundKey: index('account_provider_provider_account_id_index').on(
			account.provider,
			account.providerAccountId
		),
	})
);

export const sessions = pgTable('session', {
	sessionToken: text('session_token').primaryKey(),
	userId: text('user_id')
		.notNull()
		.references(() => users.id, { onDelete: 'cascade' }),
	expires: timestamp('expires', { mode: 'date' }).notNull(),
});

export const verificationTokens = pgTable(
	'verification_token',
	{
		identifier: text('identifier').notNull(),
		token: text('token').notNull(),
		expires: timestamp('expires', { mode: 'date' }).notNull(),
	},
	(vt) => ({
		compoundKey: index('verification_token_identifier_token_index').on(vt.identifier, vt.token),
	})
);

export const projectApiKeys = pgTable('project_api_keys', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	projectId: uuid('project_id').references(() => projects.id, { onDelete: 'cascade' }),
	name: varchar('name', { length: 255 }).notNull(),
	keyPrefix: varchar('key_prefix', { length: 16 }).notNull(),
	keyHash: varchar('key_hash', { length: 128 }).notNull().unique(),
	scope: varchar('scope', { length: 20 }).notNull().default('ingest'),
	status: varchar('status', { length: 20 }).notNull().default('active'),
	rateLimitRpm: integer('rate_limit_rpm').notNull().default(5000),
	expiresAt: timestamp('expires_at', { withTimezone: true }),
	revokedAt: timestamp('revoked_at', { withTimezone: true }),
	createdBy: varchar('created_by', { length: 255 }).notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).defaultNow(),
	// 1722900000_add_agents.sql (M5 §7): populated only when scope='agent'. Agent keys are always
	// org-scoped (projectId NULL) since an agent works across every project in the org.
	agentId: uuid('agent_id').references(() => agents.id, { onDelete: 'cascade' }),
}, (table) => ({
	idxApiKeysHashStatus: index('idx_api_keys_hash_status').on(table.keyHash, table.status),
	idxApiKeysOrgProject: index('idx_api_keys_org_project').on(table.organizationId, table.projectId),
	idxApiKeysAgent: index('idx_api_keys_agent').on(table.agentId),
}));

// 1722900000_add_agents.sql (M5 §7, Q5): agent identity. issue_activity/issue_comments'
// actor_type='agent' ids resolve against this table; IssueAssigneePicker.svelte lists real rows
// from here instead of the M3 hardcoded "AutoFix Agent" mock.
export const agents = pgTable('agents', {
	id: uuid('id').primaryKey().defaultRandom(),
	orgId: uuid('org_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	name: varchar('name', { length: 255 }).notNull(),
	kind: varchar('kind', { length: 20 }).notNull(),
	status: varchar('status', { length: 20 }).notNull().default('active'),
	createdBy: varchar('created_by', { length: 255 }).notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).defaultNow(),
	// 1723900000_add_repo_credentials.sql (N10): admin-set delivery gate. Only agents explicitly
	// granted this flag may fetch decrypted repo credentials from GET /api/agent/repo-credentials;
	// a plain agent key gets 403. Default false, toggled in the agent management UI, audited.
	canAccessRepoCredentials: boolean('can_access_repo_credentials').notNull().default(false),
	// 1725900000_add_cli_device_login.sql (docs/plans/CLI_BROWSER_LOGIN.md): set iff kind = 'personal' (CHECK
	// agents_personal_owner_check). A personal agent acts for this user and is authorized against the user's LIVE
	// membership, projects and environment grants on every request (agent-auth.ts); org agents keep NULL.
	ownerUserId: text('owner_user_id').references(() => users.id, { onDelete: 'cascade' }),
}, (table) => ({
	idxAgentsOrg: index('idx_agents_org').on(table.orgId),
	uqAgentsPersonalOwner: uniqueIndex('uq_agents_personal_owner')
		.on(table.orgId, table.ownerUserId)
		.where(sql`owner_user_id IS NOT NULL`),
}));

// 1725900000_add_cli_device_login.sql: one row per `sentinel login` device authorization (RFC 8628). Holds the sha256 of
// the device_code, never the code itself, and never a key: the key is minted in the transaction that flips the row to
// 'delivered' (routes/api/cli/device/token). Expiry is `expiresAt`; there is no 'expired' status.
export const cliDeviceAuthorizations = pgTable('cli_device_authorizations', {
	id: uuid('id').primaryKey().defaultRandom(),
	deviceCodeHash: varchar('device_code_hash', { length: 64 }).notNull().unique(),
	userCode: varchar('user_code', { length: 8 }).notNull(),
	status: varchar('status', { length: 20 }).notNull().default('pending'),
	client: varchar('client', { length: 32 }).notNull().default(''),
	clientVersion: varchar('client_version', { length: 64 }).notNull().default(''),
	deviceName: varchar('device_name', { length: 64 }).notNull(),
	requestedProfile: varchar('requested_profile', { length: 32 }).notNull().default(''),
	requestedIp: varchar('requested_ip', { length: 64 }).notNull().default(''),
	requestedUserAgent: varchar('requested_user_agent', { length: 255 }).notNull().default(''),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	expiresAt: timestamp('expires_at', { withTimezone: true }).notNull(),
	pollIntervalSeconds: integer('poll_interval_seconds').notNull().default(5),
	lastPolledAt: timestamp('last_polled_at', { withTimezone: true }),
	approvedByUserId: text('approved_by_user_id').references(() => users.id, { onDelete: 'cascade' }),
	organizationId: uuid('organization_id').references(() => organizations.id, { onDelete: 'cascade' }),
	decidedAt: timestamp('decided_at', { withTimezone: true }),
	apiKeyId: uuid('api_key_id').references(() => projectApiKeys.id, { onDelete: 'set null' }),
	deliveredAt: timestamp('delivered_at', { withTimezone: true }),
}, (table) => ({
	uqPendingUserCode: uniqueIndex('uq_cli_device_authorizations_pending_user_code')
		.on(table.userCode)
		.where(sql`status = 'pending'`),
	idxExpiresAt: index('idx_cli_device_authorizations_expires_at').on(table.expiresAt),
}));

// N10 part 2 (docs/plans/AGENT_WORKER_PLAN.md §4.5), source of truth is
// 1723900000_add_repo_credentials.sql. Org-scoped git credentials for the sentinel-worker's push
// access. `encryptedSecret` is AES-256-GCM ciphertext under SENTINEL_ENCRYPTION_KEY (see
// $lib/server/repo-credential-crypto.ts) -- NEVER plaintext, a deliberate divergence from
// agentWebhooks.secret above: webhook secrets only SIGN, these authorize repository WRITES.
// The write-only UI shows label + secretPrefix only; the secret is never returned to any
// dashboard client after initial set. On revoke, encryptedSecret/nonce are overwritten with ''.
export const repoCredentials = pgTable('repo_credentials', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	provider: varchar('provider', { length: 20 }).notNull(), // 'github' | 'bitbucket'
	label: varchar('label', { length: 255 }).notNull(),
	secretPrefix: varchar('secret_prefix', { length: 16 }).notNull(),
	encryptedSecret: text('encrypted_secret').notNull(),
	nonce: text('nonce').notNull(),
	keyVersion: smallint('key_version').notNull().default(1),
	status: varchar('status', { length: 20 }).notNull().default('active'), // 'active' | 'revoked'
	createdBy: varchar('created_by', { length: 255 }).notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	revokedAt: timestamp('revoked_at', { withTimezone: true }),
	lastFetchedAt: timestamp('last_fetched_at', { withTimezone: true }),
	// 1724300000_add_repo_credential_project_scope.sql: NULL means org-wide, which is what every
	// row created before that migration is. A non-NULL value binds the credential to one project,
	// and it is then delivered only to agents holding a grant for that project. A DB trigger
	// enforces that the project belongs to organization_id — a CHECK cannot read another table,
	// and without it a foreign tenant's project id could be written here and would match that
	// tenant's grants.
	projectId: uuid('project_id').references(() => projects.id, { onDelete: 'cascade' }),
}, (table) => ({
	idxRepoCredentialsOrg: index('idx_repo_credentials_org').on(table.organizationId),
	idxRepoCredentialsProject: index('idx_repo_credentials_project').on(table.projectId),
}));

// 1724300000_add_repo_credential_project_scope.sql: which projects an agent may receive repo
// credentials for. The grant is the SCOPE; agents.can_access_repo_credentials is the outer master
// switch, kept so revoking everything for one agent stays a single toggle. An agent needs the flag
// either way; project-scoped credentials additionally need a matching grant.
export const agentRepoCredentialGrants = pgTable('agent_repo_credential_grants', {
	agentId: uuid('agent_id').notNull().references(() => agents.id, { onDelete: 'cascade' }),
	projectId: uuid('project_id').notNull().references(() => projects.id, { onDelete: 'cascade' }),
	createdBy: varchar('created_by', { length: 255 }).notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	pk: primaryKey({ columns: [table.agentId, table.projectId] }),
	idxAgentRepoCredentialGrantsProject: index('idx_agent_repo_credential_grants_project').on(table.projectId),
}));

// N1a (AI-agent-native Sentinel), source of truth is
// 1723300000_add_agent_webhooks.sql. `secret` is stored in plaintext (deliberate divergence from
// project_api_keys, which only stores a hash) -- the server must SIGN outbound deliveries with the
// raw secret, not merely verify against it, so hashing would make delivery impossible.
// `secretPrefix` supports display/rotation UX without re-exposing the full secret. status CHECK
// allows 'active'|'disabled'|'failed'.
export const agentWebhooks = pgTable('agent_webhooks', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	agentId: uuid('agent_id').notNull().references(() => agents.id, { onDelete: 'cascade' }),
	url: text('url').notNull(),
	secret: text('secret').notNull(),
	secretPrefix: varchar('secret_prefix', { length: 16 }).notNull(),
	eventTypes: text('event_types').array().notNull().default([]),
	status: varchar('status', { length: 20 }).notNull().default('active'), // 'active' | 'disabled' | 'failed'
	lastDeliveredSeq: bigint('last_delivered_seq', { mode: 'number' }).notNull().default(0),
	consecutiveFailures: integer('consecutive_failures').notNull().default(0),
	lastAttemptAt: timestamp('last_attempt_at', { withTimezone: true }),
	lastError: text('last_error'),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	idxAgentWebhooksAgent: index('idx_agent_webhooks_agent').on(table.agentId),
}));

// N9 (docs/plans/AGENT_WORKER_PLAN.md C4/C5), source of truth is
// 1723800000_add_agent_idempotency_keys.sql. Client-supplied idempotency keys for agent write
// endpoints (D21). Scope is (agentId, idempotencyKey) -- the UNIQUE constraint below is what makes
// a concurrent duplicate lose the race (createComment/recordAgentProgress insert with
// onConflictDoNothing inside their own transaction and roll back on conflict). `op` guards against
// a key reused across two different operations; `commentId` is the only original-result reference
// needed to replay (NULL for progress). Aged out after 7 days by retention.ts's reaper.
export const agentIdempotencyKeys = pgTable('agent_idempotency_keys', {
	id: uuid('id').primaryKey().defaultRandom(),
	agentId: varchar('agent_id', { length: 255 }).notNull(),
	idempotencyKey: varchar('idempotency_key', { length: 255 }).notNull(),
	op: varchar('op', { length: 50 }).notNull(),
	commentId: uuid('comment_id'),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	uniqAgentKey: uniqueIndex('agent_idempotency_keys_agent_key_unique').on(table.agentId, table.idempotencyKey),
	idxCreatedAt: index('idx_agent_idempotency_keys_created_at').on(table.createdAt),
}));

// N10 part 1 (docs/plans/AGENT_WORKER_PLAN.md rev 4 SS4.5, DECISIONS.md D23), source of truth is
// 1724000000_add_project_agent_settings.sql. Dashboard-managed per-project agent switch; the N8
// worker reads this via getAgentSettingsForProjects. PK on projectId -- there is exactly one row
// per project (or none, meaning defaults: disabled, no cap).
export const projectAgentSettings = pgTable('project_agent_settings', {
	projectId: uuid('project_id').primaryKey().references(() => projects.id, { onDelete: 'cascade' }),
	fixEnabled: boolean('fix_enabled').notNull().default(false),
	maxPrsPerDay: integer('max_prs_per_day'),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
});

// N10 part 1, source of truth is 1724000000_add_project_agent_settings.sql. One repo connection
// per project in v1 -- enforced by PK on projectId rather than an app-level uniqueness check.
// `testCmd` is a server-stored command the N8 worker executes verbatim against the cloned repo;
// see the migration's comment for why that's an accepted boundary (manage_agents RBAC + audit
// trail, not this table). No credentials here -- a sibling task owns the encrypted git-credentials
// store separately.
export const projectRepoConnections = pgTable('project_repo_connections', {
	projectId: uuid('project_id').primaryKey().references(() => projects.id, { onDelete: 'cascade' }),
	provider: text('provider').notNull(),
	owner: text('owner').notNull(),
	repo: text('repo').notNull(),
	defaultBranch: text('default_branch').notNull(),
	testCmd: text('test_cmd').notNull(),
	agentCmd: text('agent_cmd'),
	cloneDepth: integer('clone_depth'),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
});

// ---------------------------------------------------------------------------------------------
// sentinel-user-report-issue §4.6 (contract 05b5a109). Source of truth for everything below is
// packages/db-migrations/migrations/1724100000_add_user_report_intake_and_oncall.sql — this file
// is a hand-maintained mirror and has drifted from the real schema before (see the alertConfigs
// and issueRelations comments above), so every table here names that migration.
// ---------------------------------------------------------------------------------------------

// §4.6, Decisions taken OQ-6: the external reporter is a SEPARATE table with its own FK, not a
// nullable reporter on manual_issue_reports (which is not touched) and not a synthesised "user"
// row. organizationId is denormalised NOT NULL for the reason attachments.orgId gives: a tenancy
// check must never depend on walking a join. UNIQUE (projectId, externalId) is the identity rule
// and the thing that makes intake an upsert. `email` is stored and NEVER used to send anything
// (MANUAL_ISSUES_DESIGN §0 Q1 — email is never a reply channel).
export const externalReporters = pgTable('external_reporters', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	projectId: uuid('project_id').notNull().references(() => projects.id, { onDelete: 'cascade' }),
	externalId: varchar('external_id', { length: 255 }).notNull(),
	displayName: varchar('display_name', { length: 255 }),
	email: varchar('email', { length: 320 }),
	firstSeenAt: timestamp('first_seen_at', { withTimezone: true }).notNull().defaultNow(),
	lastSeenAt: timestamp('last_seen_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	uniqProjectExternalId: uniqueIndex('external_reporters_project_external_id_unique').on(table.projectId, table.externalId),
	idxOrg: index('idx_external_reporters_org').on(table.organizationId),
	idxOrgLastSeen: index('idx_external_reporters_org_last_seen').on(table.organizationId, table.lastSeenAt),
}));

// §4.6: the 1:1 companion to an `issues` row, parallel to manualIssueReports. A separate table
// rather than seven more columns on manual_issue_reports: an org-member report has no page, no
// release and no brand. `severity` mirrors manualIssueReports' CHECK ('low'|'medium'|'high'|
// 'critical'). `context` is §4.1's context object verbatim, unknown keys kept; no GIN index on it
// in v1. `idempotencyKey` is nullable — absent means no dedupe.
export const externalIssueReports = pgTable('external_issue_reports', {
	issueId: uuid('issue_id').primaryKey().references(() => issues.id, { onDelete: 'cascade' }),
	externalReporterId: uuid('external_reporter_id').notNull().references(() => externalReporters.id),
	bodyMd: text('body_md').notNull(),
	severity: varchar('severity', { length: 20 }).notNull().default('medium'), // 'low' | 'medium' | 'high' | 'critical'
	context: jsonb('context').notNull().default({}),
	idempotencyKey: varchar('idempotency_key', { length: 128 }),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	idxReporter: index('idx_external_issue_reports_reporter').on(table.externalReporterId),
}));

// §4.1: POST /report's dedupe store. Deliberately NOT agentIdempotencyKeys — that table's key
// space is the calling AGENT's, this endpoint's is the calling API KEY ROW's id, and widening the
// existing unique constraint would make it mean two things. `requestHash` is what makes §7's
// `409 idempotency_conflict` ("same key, different body") detectable at all; `issueId` is the
// original result replayed on a hit and carries no FK, for the reason agentIdempotencyKeys.
// commentId gives — a retry must not resurrect a deleted issue. Aged out at 7 days (§8).
export const reportIdempotencyKeys = pgTable('report_idempotency_keys', {
	id: uuid('id').primaryKey().defaultRandom(),
	apiKeyId: uuid('api_key_id').notNull().references(() => projectApiKeys.id, { onDelete: 'cascade' }),
	idempotencyKey: varchar('idempotency_key', { length: 128 }).notNull(),
	requestHash: varchar('request_hash', { length: 64 }).notNull(),
	issueId: uuid('issue_id'),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	uniqApiKeyKey: uniqueIndex('report_idempotency_keys_key_unique').on(table.apiKeyId, table.idempotencyKey),
	idxCreatedAt: index('idx_report_idempotency_keys_created_at').on(table.createdAt),
}));

// §4.4/§4.6: the on-call schedule. An organization has AT MOST ONE ACTIVE schedule, enforced by
// the partial unique index idx_oncall_schedules_one_active (organization_id) WHERE active —
// by the database, not by a handler. `timezone` is an IANA zone name; handoff is wall-clock in it,
// so a DST handoff day is 23 or 25 hours long, deliberately. `kind` CHECK allows 'daily'|'weekly';
// `handoffWeekday` is null unless weekly, enforced by the multi-column
// oncall_schedules_handoff_weekday_check — which 1724200000_add_oncall_rotation_anchor.sql
// re-encodes to ISO 8601 (1 = Monday .. 7 = Sunday) per SR-4.
export const oncallSchedules = pgTable('oncall_schedules', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	name: varchar('name', { length: 255 }).notNull(),
	timezone: varchar('timezone', { length: 64 }).notNull().default('UTC'),
	kind: varchar('kind', { length: 20 }).notNull(), // 'daily' | 'weekly'
	handoffTime: time('handoff_time').notNull(),
	handoffWeekday: smallint('handoff_weekday'), // ISO 1-7 (1 = Monday), null unless kind = 'weekly'
	// SR-4's explicit anchor (1724200000): the rotation index counts wall-clock handoff boundaries
	// at or before T since the first boundary at or after THIS column — deliberately not createdAt,
	// because §4.4a's PUT is create-or-replace and an edit must not re-phase future handoffs.
	rotationAnchorAt: timestamp('rotation_anchor_at', { withTimezone: true }).notNull().defaultNow(),
	active: boolean('active').notNull().default(true),
	createdBy: varchar('created_by', { length: 255 }).notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	updatedAt: timestamp('updated_at', { withTimezone: true }).notNull().defaultNow(),
});

// §4.4: the ordered member list. Eligibility is NOT a column — a member whose org role later drops
// below ISSUE_WRITE_ROLES is skipped at resolve time, never silently removed, so it is a live join
// against organization_members. The composite primary key is §4.6's UNIQUE (scheduleId, userId);
// idx_oncall_schedule_members_position is the second uniqueness rule.
export const oncallScheduleMembers = pgTable('oncall_schedule_members', {
	scheduleId: uuid('schedule_id').notNull().references(() => oncallSchedules.id, { onDelete: 'cascade' }),
	userId: text('user_id').notNull().references(() => users.id, { onDelete: 'cascade' }),
	position: smallint('position').notNull(),
}, (table) => ({
	pk: primaryKey({ columns: [table.scheduleId, table.userId] }),
	uniqPosition: uniqueIndex('idx_oncall_schedule_members_position').on(table.scheduleId, table.position),
}));

// §4.4: overrides, in absolute UTC. An override wins over the rotation for its interval — without
// one, escalation assigns to whoever the arithmetic says is on duty even when that person is on
// holiday. oncall_overrides_window_check enforces endsAt > startsAt in the database.
export const oncallOverrides = pgTable('oncall_overrides', {
	id: uuid('id').primaryKey().defaultRandom(),
	scheduleId: uuid('schedule_id').notNull().references(() => oncallSchedules.id, { onDelete: 'cascade' }),
	userId: text('user_id').notNull().references(() => users.id, { onDelete: 'cascade' }),
	startsAt: timestamp('starts_at', { withTimezone: true }).notNull(),
	endsAt: timestamp('ends_at', { withTimezone: true }).notNull(),
	createdBy: varchar('created_by', { length: 255 }).notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
}, (table) => ({
	idxSchedule: index('idx_oncall_overrides_schedule').on(table.scheduleId, table.startsAt),
}));

// §4.4: one row per escalation hop on a report. hop 1 is written at intake; §4.4b's every-minute
// sweep adds one row per reassignment and stops at ESCALATION_MAX_HOPS with `exhausted` true.
// UNIQUE (issueId, hop) is what makes "escalation at intake is idempotent per report" true in the
// database rather than in a handler. idx_issue_escalations_due is the sweep's partial index
// (ack_deadline_at) WHERE acknowledged_at IS NULL AND NOT exhausted.
export const issueEscalations = pgTable('issue_escalations', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	scheduleId: uuid('schedule_id').notNull().references(() => oncallSchedules.id, { onDelete: 'cascade' }),
	assigneeId: text('assignee_id').notNull().references(() => users.id, { onDelete: 'cascade' }),
	hop: smallint('hop').notNull(),
	escalatedAt: timestamp('escalated_at', { withTimezone: true }).notNull().defaultNow(),
	ackDeadlineAt: timestamp('ack_deadline_at', { withTimezone: true }).notNull(),
	acknowledgedAt: timestamp('acknowledged_at', { withTimezone: true }),
	exhausted: boolean('exhausted').notNull().default(false),
}, (table) => ({
	uniqIssueHop: uniqueIndex('issue_escalations_issue_hop_unique').on(table.issueId, table.hop),
	idxDue: index('idx_issue_escalations_due').on(table.ackDeadlineAt),
}));

// sentinel-span-ingest D1 (1725600000_add_spans.sql): spans for the trace waterfall, PARTITIONED BY DAY on
// start_ts (the processor writes; the dashboard only reads, and the retention cron creates and drops day
// partitions through spans_ensure_partitions / spans_drop_expired). Hand-maintained mirror like every table
// here — the migration is the source of truth. org_id carries no FK and project_id is nullable: a span pulled
// from core's trace store belongs to an organization, never to a Sentinel project.
export const spans = pgTable('spans', {
	orgId: uuid('org_id').notNull(),
	projectId: uuid('project_id'),
	environment: text('environment').notNull().default(''),
	source: text('source').notNull(),
	serviceName: text('service_name').notNull().default(''),
	traceId: text('trace_id').notNull(),
	spanId: text('span_id').notNull(),
	parentSpanId: text('parent_span_id').notNull().default(''),
	name: text('name').notNull().default(''),
	op: text('op').notNull().default(''),
	kind: text('kind').notNull().default(''),
	startTs: timestamp('start_ts', { withTimezone: true }).notNull(),
	durationUs: bigint('duration_us', { mode: 'number' }).notNull().default(0),
	status: text('status').notNull().default(''),
	attributes: jsonb('attributes').notNull().default({}),
	// D8: the configured CORE project that governs a PULLED span (SPAN_PULL_SCOPES). NULL for mobile spans.
	scopeProjectId: uuid('scope_project_id'),
}, (table) => ({
	pk: primaryKey({ columns: [table.orgId, table.traceId, table.spanId, table.startTs] }),
}));

// D3: the durable pull queue, one row per (organization, trace); rows age out with the 14-day span window.
export const spanPullQueue = pgTable('span_pull_queue', {
	orgId: uuid('org_id').notNull(),
	traceId: text('trace_id').notNull(),
	stage: smallint('stage').notNull().default(0),
	attempts: integer('attempts').notNull().default(0),
	nextFetchAt: timestamp('next_fetch_at', { withTimezone: true }).notNull(),
	createdAt: timestamp('created_at', { withTimezone: true }).notNull().defaultNow(),
	lastError: text('last_error').notNull().default(''),
	triggerProjectId: uuid('trigger_project_id'),
	// sentinel-occurrence-spans TASK-02 (1725800000_add_span_pull_on_demand.sql): the last (re)arm came from a person or
	// agent, who, when, and what the worker's last fetch ended in ('' | stored | not_found | too_large | not_allowed | gave_up).
	onDemand: boolean('on_demand').notNull().default(false),
	requestedBy: text('requested_by').notNull().default(''),
	requestedByKind: text('requested_by_kind').notNull().default(''),
	requestedAt: timestamp('requested_at', { withTimezone: true }),
	outcome: text('outcome').notNull().default(''),
}, (table) => ({
	pk: primaryKey({ columns: [table.orgId, table.traceId] }),
}));

// sentinel-occurrence-spans TASK-02: one row per ACCEPTED on-demand pull request — the audit trail (who asked for which
// trace, when) and the per-organization rate-limit ledger (count of the last hour, under an advisory lock).
export const spanPullRequests = pgTable('span_pull_requests', {
	id: uuid('id').primaryKey().defaultRandom(),
	orgId: uuid('org_id').notNull(),
	traceId: text('trace_id').notNull(),
	action: text('action').notNull(),
	requestedBy: text('requested_by').notNull(),
	requestedByKind: text('requested_by_kind').notNull(),
	requestedAt: timestamp('requested_at', { withTimezone: true }).notNull().defaultNow(),
});

// sentinel-reporting-and-dashboard D6 (1726100000_add_sli_hourly.sql): the hourly denominators of the SLIs, written by
// POST /api/cron/sli-collect and replicated to Databend through CDC (D2). One row per (sli, environment, hour); `fast` is
// NULL for kinds with no latency objective.
export const sliHourly = pgTable('sli_hourly', {
	sliId: text('sli_id').notNull(),
	environment: text('environment').notNull(),
	hourStart: timestamp('hour_start', { withTimezone: true }).notNull(),
	total: bigint('total', { mode: 'number' }).notNull(),
	errors: bigint('errors', { mode: 'number' }).notNull(),
	fast: bigint('fast', { mode: 'number' }),
	// 1726200000_add_sli_hourly_latency_total.sql: the latency ratio's denominator; NULL for jobs/nats and for rows written
	// before it existed (readers coalesce to `total`).
	latencyTotal: bigint('latency_total', { mode: 'number' }),
}, (table) => ({
	pk: primaryKey({ columns: [table.sliId, table.environment, table.hourStart] }),
}));
