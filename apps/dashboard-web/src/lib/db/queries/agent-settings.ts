import { db } from '$lib/server/db';
import { projectAgentSettings, projectRepoConnections } from '$lib/db/schema';
import { eq, inArray } from 'drizzle-orm';
import { AGENT_REPO_PROVIDERS } from '$lib/constants/agent-repo';

/**
 * N10 part 1 (docs/plans/AGENT_WORKER_PLAN.md rev 4 SS4.5, DECISIONS.md D23): server-side per-project agent
 * settings + the one-per-project (v1) repo connection. Source of truth for the schema is
 * 1724000000_add_project_agent_settings.sql.
 *
 * Tenant scope: these functions all key off `projectId`, which callers must have already resolved
 * from the authenticated caller's own org/project membership (B7) -- nothing here re-derives scope,
 * matching agent-reads.ts's convention of taking scope as an explicit argument.
 *
 * No credentials of any kind live here on purpose -- a SIBLING task owns the encrypted
 * git-credentials store; this module only prefixes its tables project_agent_* / project_repo_* to
 * keep the namespaces coordinated.
 */

const VALID_PROVIDERS = AGENT_REPO_PROVIDERS;
export type RepoProvider = (typeof VALID_PROVIDERS)[number];

export class AgentSettingsValidationError extends Error {
	constructor(message: string) {
		super(message);
		this.name = 'AgentSettingsValidationError';
	}
}

export interface ProjectAgentSettingsRow {
	projectId: string;
	fixEnabled: boolean;
	maxPrsPerDay: number | null;
	createdAt: Date | null;
	updatedAt: Date | null;
}

export interface RepoConnectionRow {
	projectId: string;
	provider: RepoProvider;
	owner: string;
	repo: string;
	defaultBranch: string;
	testCmd: string;
	agentCmd: string | null;
	cloneDepth: number | null;
	createdAt: Date | null;
	updatedAt: Date | null;
}

export interface RepoConnectionInput {
	provider: RepoProvider;
	owner: string;
	repo: string;
	defaultBranch: string;
	testCmd: string;
	agentCmd?: string | null;
	cloneDepth?: number | null;
}

export interface AgentSettingsInput {
	fixEnabled: boolean;
	maxPrsPerDay?: number | null;
}

function validateMaxPrsPerDay(maxPrsPerDay: number | null | undefined): void {
	if (maxPrsPerDay === undefined || maxPrsPerDay === null) return;
	if (!Number.isInteger(maxPrsPerDay) || maxPrsPerDay <= 0) {
		throw new AgentSettingsValidationError('maxPrsPerDay must be a positive integer when provided');
	}
}

function validateAgentSettingsInput(settings: AgentSettingsInput): void {
	if (typeof settings.fixEnabled !== 'boolean') {
		throw new AgentSettingsValidationError('fixEnabled must be a boolean');
	}
	validateMaxPrsPerDay(settings.maxPrsPerDay);
}

function validateRepoConnectionInput(conn: RepoConnectionInput): void {
	if (!VALID_PROVIDERS.includes(conn.provider)) {
		throw new AgentSettingsValidationError(`provider must be one of: ${VALID_PROVIDERS.join(', ')}`);
	}
	if (!conn.owner || !conn.owner.trim()) {
		throw new AgentSettingsValidationError('owner must not be empty');
	}
	if (!conn.repo || !conn.repo.trim()) {
		throw new AgentSettingsValidationError('repo must not be empty');
	}
	if (!conn.defaultBranch || !conn.defaultBranch.trim()) {
		throw new AgentSettingsValidationError('defaultBranch must not be empty');
	}
	if (!conn.testCmd || !conn.testCmd.trim()) {
		throw new AgentSettingsValidationError('testCmd must not be empty');
	}
	if (conn.cloneDepth !== undefined && conn.cloneDepth !== null) {
		if (!Number.isInteger(conn.cloneDepth) || conn.cloneDepth <= 0) {
			throw new AgentSettingsValidationError('cloneDepth must be a positive integer when provided');
		}
	}
}

export async function getProjectAgentSettings(projectId: string): Promise<ProjectAgentSettingsRow | null> {
	const [row] = await db
		.select()
		.from(projectAgentSettings)
		.where(eq(projectAgentSettings.projectId, projectId));
	return (row as ProjectAgentSettingsRow) ?? null;
}

export async function upsertProjectAgentSettings(
	projectId: string,
	settings: AgentSettingsInput
): Promise<ProjectAgentSettingsRow> {
	validateAgentSettingsInput(settings);

	const [row] = await db
		.insert(projectAgentSettings)
		.values({
			projectId,
			fixEnabled: settings.fixEnabled,
			maxPrsPerDay: settings.maxPrsPerDay ?? null,
			updatedAt: new Date(),
		})
		.onConflictDoUpdate({
			target: projectAgentSettings.projectId,
			set: {
				fixEnabled: settings.fixEnabled,
				maxPrsPerDay: settings.maxPrsPerDay ?? null,
				updatedAt: new Date(),
			},
		})
		.returning();

	return row as ProjectAgentSettingsRow;
}

export async function getRepoConnection(projectId: string): Promise<RepoConnectionRow | null> {
	const [row] = await db
		.select()
		.from(projectRepoConnections)
		.where(eq(projectRepoConnections.projectId, projectId));
	return (row as RepoConnectionRow) ?? null;
}

export async function upsertRepoConnection(
	projectId: string,
	conn: RepoConnectionInput
): Promise<RepoConnectionRow> {
	validateRepoConnectionInput(conn);

	const normalized = {
		provider: conn.provider,
		owner: conn.owner.trim(),
		repo: conn.repo.trim(),
		defaultBranch: conn.defaultBranch.trim(),
		testCmd: conn.testCmd.trim(),
		agentCmd: conn.agentCmd?.trim() || null,
		cloneDepth: conn.cloneDepth ?? null,
	};

	const [row] = await db
		.insert(projectRepoConnections)
		.values({
			projectId,
			...normalized,
			updatedAt: new Date(),
		})
		.onConflictDoUpdate({
			target: projectRepoConnections.projectId,
			set: {
				...normalized,
				updatedAt: new Date(),
			},
		})
		.returning();

	return row as RepoConnectionRow;
}

export async function deleteRepoConnection(projectId: string): Promise<void> {
	await db.delete(projectRepoConnections).where(eq(projectRepoConnections.projectId, projectId));
}

export interface AgentProjectSettings {
	fixEnabled: boolean;
	maxPrsPerDay: number | null;
	repo: RepoConnectionRow | null;
}

/**
 * Batch read backing `GET /api/agent/projects` (worker polls this to discover what to work on).
 * Returns a map keyed by every id in `projectIds`, defaulting entries with no settings/repo row to
 * `{fixEnabled: false, maxPrsPerDay: null, repo: null}` so the worker never has to special-case a
 * missing row.
 */
export async function getAgentSettingsForProjects(
	projectIds: string[]
): Promise<Map<string, AgentProjectSettings>> {
	const result = new Map<string, AgentProjectSettings>();
	for (const id of projectIds) {
		result.set(id, { fixEnabled: false, maxPrsPerDay: null, repo: null });
	}
	if (projectIds.length === 0) return result;

	const settingsRows = await db
		.select()
		.from(projectAgentSettings)
		.where(inArray(projectAgentSettings.projectId, projectIds));
	for (const row of settingsRows as ProjectAgentSettingsRow[]) {
		const entry = result.get(row.projectId);
		if (entry) {
			entry.fixEnabled = row.fixEnabled;
			entry.maxPrsPerDay = row.maxPrsPerDay;
		}
	}

	const repoRows = await db
		.select()
		.from(projectRepoConnections)
		.where(inArray(projectRepoConnections.projectId, projectIds));
	for (const row of repoRows as RepoConnectionRow[]) {
		const entry = result.get(row.projectId);
		if (entry) {
			entry.repo = row;
		}
	}

	return result;
}
