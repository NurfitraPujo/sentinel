import { pgTable, uuid, varchar, text, timestamp, bigint, jsonb, index, integer, boolean } from 'drizzle-orm/pg-core';

export const projects = pgTable('projects', {
	id: uuid('id').primaryKey().defaultRandom(),
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
	status: varchar('status', { length: 20 }).notNull().default('open'),
	firstSeen: timestamp('first_seen').defaultNow(),
	lastSeen: timestamp('last_seen').defaultNow(),
	count: bigint('count', { mode: 'number' }).notNull().default(1),
});

export const errorOccurrences = pgTable('error_occurrences', {
	id: uuid('id').primaryKey().defaultRandom(),
	issueId: uuid('issue_id').notNull().references(() => issues.id),
	environment: varchar('environment', { length: 50 }).notNull(),
	platform: varchar('platform', { length: 50 }).notNull(),
	stacktrace: jsonb('stacktrace').notNull().default([]),
	metadata: jsonb('metadata').notNull().default({}),
	traceId: varchar('trace_id', { length: 64 }),
	spanId: varchar('span_id', { length: 64 }),
	createdAt: timestamp('created_at').defaultNow(),
});

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