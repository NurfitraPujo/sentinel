import { env } from '$env/dynamic/private';
import { db } from '$lib/server/db';
import { projects, projectEnvironments } from '$lib/db/schema';
import { createRun, databendConfigFromEnv } from './databend';
import { parseServiceSliConfig } from '../sli/config';
import { environmentForCluster } from '../sli/collect';
import { createInsightsState, start, render } from './insights-state';
import type { InsightsDeps } from './insights';
import { log } from '$lib/server/observability/log';

/**
 * Wires D20's refresh to the process: project names and declared environments come from Sentinel's Postgres (`projects`
 * is not in Databend, D2), the SLI set from SERVICE_SLI_CONFIG, Databend from DATABEND_*.
 */
export const insightsState = createInsightsState();

export async function insightsDeps(): Promise<InsightsDeps> {
	const [projectRows, envRows] = await Promise.all([
		db.select({ id: projects.id, name: projects.name }).from(projects),
		db.select({ projectId: projectEnvironments.projectId, name: projectEnvironments.name }).from(projectEnvironments),
	]);
	const envsByProject = new Map<string, string[]>();
	for (const e of envRows) envsByProject.set(e.projectId, [...(envsByProject.get(e.projectId) ?? []), e.name]);
	return {
		run: createRun(databendConfigFromEnv(), fetch, 30_000),
		projects: projectRows.map((p) => ({ id: p.id, name: p.name, environments: envsByProject.get(p.id) ?? [] })),
		slis: parseServiceSliConfig(env.SERVICE_SLI_CONFIG),
		sliEnvironment: env.SLI_CLUSTER ? environmentForCluster(env.SLI_CLUSTER) : null,
		now: new Date(),
	};
}

export function ensureInsightsRefreshing(): void {
	start(insightsState, insightsDeps, (event, fields) => log.error(event, fields));
}

export const renderInsights = () => render(insightsState);
