import { pgTable, uuid, varchar, text, timestamp, bigint, jsonb, index, uniqueIndex, integer, boolean, smallint } from 'drizzle-orm/pg-core';
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
});

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
	createdAt: timestamp('created_at').defaultNow(),
}, (table) => ({
	idxOccurrencesIssueRelease: index('idx_occurrences_issue_release').on(table.issueId, table.releaseVersion),
}));

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
export const alertConfigs = pgTable('alert_configs', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id),
	projectId: uuid('project_id').references(() => projects.id),
	channel: varchar('channel', { length: 20 }).notNull(),
	channelConfig: jsonb('channel_config').notNull().default({}),
	frequencyThreshold: integer('frequency_threshold').notNull().default(50),
	frequencyWindowSeconds: integer('frequency_window_seconds').notNull().default(60),
	enabled: boolean('enabled').notNull().default(true),
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
}, (table) => ({
	idxAgentsOrg: index('idx_agents_org').on(table.orgId),
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
}, (table) => ({
	idxRepoCredentialsOrg: index('idx_repo_credentials_org').on(table.organizationId),
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
