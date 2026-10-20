import { and, inArray, sql } from 'drizzle-orm';
import { env } from '$env/dynamic/private';
import { db } from '$lib/server/db';
import { issues, projectEnvironments } from '$lib/db/schema';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes, scopeForProject } from '$lib/server/environment-access';
import { environmentScopeCondition } from '$lib/server/environment-access';
import { parseServiceSliConfig, type ServiceSli } from '../sli/config';
import { environmentForCluster } from '../sli/collect';
import { createRun, databendConfigFromEnv, type Run } from './databend';
import type { Scope } from './access';
import { coerceProject, type DashboardInput, type ProjectInfo } from './service';
import { coerceRange, coerceSplit, isUuid } from './literals';

/**
 * sentinel-reporting-and-dashboard D11/D12/D14: everything the `/` loader needs from Sentinel's Postgres before any
 * Databend query runs. The accessible set and the per-project environment scopes come from the SAME resolvers the issue
 * list uses (`getAccessibleProjects`, `resolveEnvironmentScopes`); this module only reshapes them into the analytics
 * module's `Scope`s, so there is one derivation of "which projects and environments may this user see".
 */

export interface DashboardShell {
	input: DashboardInput;
	projects: ProjectInfo[];
	/** Declared, non-hidden environments of the selected project the viewer may see (for the environment control). */
	environments: { name: string; displayName: string }[];
	databendConfigured: boolean;
	newestPostgresOccurrence: () => Promise<Date | null>;
}

export function loadSliConfig(): ServiceSli[] {
	try {
		return parseServiceSliConfig(env.SERVICE_SLI_CONFIG);
	} catch {
		return []; // an invalid config is reported by the cron route and the start-up check, never by a page render
	}
}

export function sliEnvironmentFromEnv(): string | null {
	try {
		return env.SLI_CLUSTER ? environmentForCluster(env.SLI_CLUSTER) : null;
	} catch {
		return null;
	}
}

/** VictoriaMetrics as configured for `sli-collect` (same env); null when either is unset. */
export function vmFromEnv(): { url: string; cluster: string } | null {
	return env.VICTORIAMETRICS_URL && env.SLI_CLUSTER ? { url: env.VICTORIAMETRICS_URL, cluster: env.SLI_CLUSTER } : null;
}

export async function loadShell(userId: string, params: { project: string | null; environment: string | null; range: string | null; split?: string | null }, run?: Run): Promise<DashboardShell> {
	const accessible = await getAccessibleProjects(userId);
	const projects: ProjectInfo[] = accessible.map((p) => ({ id: p.id, name: p.name }));
	const scopeMap = await resolveEnvironmentScopes(userId);
	const scopes: Scope[] = projects.map((p) => ({ projectId: p.id, environments: scopeForProject(scopeMap, p.id) }));
	const ids = projects.map((p) => p.id);

	// The allow-list for an environment NAME that reaches SQL (D14): declared names, the names the viewer's issues carry,
	// and the names a grant lists. A name outside it is dropped to 'all' here, so it never reaches a literal.
	const known = new Set<string>();
	if (ids.length > 0) {
		const [declared, seen] = await Promise.all([
			db.select({ projectId: projectEnvironments.projectId, name: projectEnvironments.name, displayName: projectEnvironments.displayName, hidden: projectEnvironments.hidden })
				.from(projectEnvironments).where(inArray(projectEnvironments.projectId, ids)),
			db.selectDistinct({ environment: issues.environment }).from(issues).where(and(inArray(issues.projectId, ids), environmentScopeCondition(scopeMap))),
		]);
		for (const d of declared) known.add(d.name);
		for (const s of seen) known.add(s.environment);
		for (const sc of scopes) for (const e of sc.environments ?? []) known.add(e);
		const project = coerceProject(params.project, projects);
		const selectable = project === 'all' ? [] : declared
			.filter((d) => d.projectId === project && !d.hidden)
			.filter((d) => { const sc = scopes.find((s) => s.projectId === project)!; return sc.environments === null || sc.environments.includes(d.name); })
			.map((d) => ({ name: d.name, displayName: d.displayName }));
		return finish(userId, projects, scopes, [...known], project, selectable, params, run);
	}
	return finish(userId, projects, scopes, [], 'all', [], params, run);
}

function finish(
	userId: string, projects: ProjectInfo[], scopes: Scope[], known: string[], project: string,
	environments: { name: string; displayName: string }[], params: { environment: string | null; range: string | null; split?: string | null }, run?: Run
): DashboardShell {
	void userId;
	const requestedEnv = params.environment ?? 'all';
	// D12: the environment filter is only meaningful inside one project; an unknown name is 'all', never an error.
	const environment = project !== 'all' && known.includes(requestedEnv) ? requestedEnv : 'all';
	const cfg = databendConfigFromEnv();
	const input: DashboardInput = {
		range: coerceRange(params.range),
		split: coerceSplit(params.split),
		project,
		environment,
		now: new Date(),
		projects,
		scopes,
		knownEnvironments: known,
		slis: loadSliConfig(),
		sliEnvironment: sliEnvironmentFromEnv(),
		vm: vmFromEnv(),
		run: run ?? createRun(cfg),
	};
	const ids = scopes.filter((s) => (project === 'all' || s.projectId === project) && s.environments?.length !== 0).map((s) => s.projectId);
	return {
		input,
		projects,
		environments,
		databendConfigured: cfg !== null,
		// the newest `last_seen` among the viewer's own issues: what Postgres holds as "newest occurrence", scoped like everything else
		newestPostgresOccurrence: async () => {
			if (ids.length === 0) return null;
			const [row] = await db.select({ at: sql<Date | null>`max(${issues.lastSeen})` }).from(issues).where(and(inArray(issues.projectId, ids), environmentScopeCondition(new Map(scopes.filter((s) => s.environments !== null).map((s) => [s.projectId, s.environments!])))));
			return row?.at ? new Date(row.at as unknown as string) : null;
		},
	};
}

export { isUuid };
