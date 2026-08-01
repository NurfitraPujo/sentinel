import { pgTable, uuid, varchar, text, timestamp, bigint, jsonb, index, integer, boolean } from 'drizzle-orm/pg-core';

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
	resolvedInVersion: varchar('resolved_in_version', { length: 100 }),
	resolvedAt: timestamp('resolved_at', { withTimezone: true }),
	resolvedByType: varchar('resolved_by_type', { length: 20 }),
	resolvedBy: varchar('resolved_by', { length: 255 }),
	regressionCount: integer('regression_count').notNull().default(0),
	lastRegressedAt: timestamp('last_regressed_at', { withTimezone: true }),
	firstSeen: timestamp('first_seen').defaultNow(),
	lastSeen: timestamp('last_seen').defaultNow(),
	count: bigint('count', { mode: 'number' }).notNull().default(1),
});

// Matches packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql — the table
// has old_value/new_value JSONB columns, NOT a single `metadata` column. actor_type/actor_id are NOT NULL
// with a CHECK on actor_type ('user'|'agent'|'system'). event_type CHECK allows
// 'status_changed'|'assigned'|'unassigned'|'regressed'|'ai_analysis'|'linked' (note: 'status_changed', not
// 'status_change' — see queries/issues.ts).
export const issueActivity = pgTable('issue_activity', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	eventType: varchar('event_type', { length: 50 }).notNull(), // 'status_changed' | 'assigned' | 'unassigned' | 'linked' | 'regressed' | 'ai_analysis'
	actorType: varchar('actor_type', { length: 20 }).notNull(), // 'user' | 'agent' | 'system'
	actorId: varchar('actor_id', { length: 255 }).notNull(),
	oldValue: jsonb('old_value'),
	newValue: jsonb('new_value'),
	createdAt: timestamp('created_at').defaultNow(),
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
}, (table) => ({
	idxApiKeysHashStatus: index('idx_api_keys_hash_status').on(table.keyHash, table.status),
	idxApiKeysOrgProject: index('idx_api_keys_org_project').on(table.organizationId, table.projectId),
}));