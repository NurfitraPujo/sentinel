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
});

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