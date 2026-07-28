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

export const organizationInvitations = pgTable('organization_invitations', {
	id: uuid('id').primaryKey().defaultRandom(),
	organizationId: uuid('organization_id').notNull().references(() => organizations.id, { onDelete: 'cascade' }),
	email: varchar('email', { length: 255 }).notNull(),
	role: varchar('role', { length: 20 }).notNull().default('viewer'),
	token: varchar('token', { length: 128 }).notNull().unique(),
	status: varchar('status', { length: 20 }).notNull().default('pending'),
	expiresAt: timestamp('expires_at').notNull(),
	createdAt: timestamp('created_at').defaultNow(),
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

export const issueActivity = pgTable('issue_activity', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	eventType: varchar('event_type', { length: 50 }).notNull(), // 'status_change' | 'assigned' | 'unassigned' | 'linked' | 'regressed'
	actorType: varchar('actor_type', { length: 20 }), // 'user' | 'agent' | 'system'
	actorId: varchar('actor_id', { length: 255 }),
	metadata: jsonb('metadata').notNull().default({}),
	createdAt: timestamp('created_at').defaultNow(),
});

export const issueRelations = pgTable('issue_relations', {
	id: uuid('id').primaryKey().defaultRandom(),
	sourceIssueId: uuid('source_issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	targetIssueId: uuid('target_issue_id').notNull().references(() => issues.id, { onDelete: 'cascade' }),
	relationType: varchar('relation_type', { length: 20 }).notNull().default('related'), // 'related', 'duplicate'
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

export const alertConfigs = pgTable('alert_configs', {
	id: uuid('id').primaryKey().defaultRandom(),
	projectId: uuid('project_id').notNull().references(() => projects.id),
	channel: varchar('channel', { length: 20 }).notNull(),
	channelTarget: varchar('channel_target', { length: 255 }).notNull(),
	frequencyThreshold: integer('frequency_threshold').notNull().default(50),
	windowSeconds: integer('window_seconds').notNull().default(60),
	enabled: boolean('enabled').notNull().default(true),
	createdAt: timestamp('created_at').defaultNow(),
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