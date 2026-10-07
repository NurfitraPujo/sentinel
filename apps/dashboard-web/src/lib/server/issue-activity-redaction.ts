import { and, eq, inArray } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { issues, projects } from '$lib/db/schema';
import { resolveEnvironmentScopes, scopeForProject } from '$lib/server/environment-access';
import { isUuid } from '$lib/uuid';
import type { ActivityEntry } from '$lib/issue-activity';

/**
 * sentinel-issue-activity TASK-01: a `linked` entry names the OTHER end of the relation in `newValue.targetIssueId`. For a
 * signed-in viewer that id is redacted exactly where the issue page's relations panel redacts the same relation
 * (getIssueRelations, decision 37): a target in an environment outside the viewer's grant, in another organization, or
 * deleted since. The same `targetIssueId: null, targetRedacted: true` marker as the agent API's
 * `redactLinkedEventTargets` (agent-relations.ts), which the agent route uses for a personal key. One grants read and
 * one issues read for the whole page, and neither when the page has no `linked` entry.
 */
export async function redactLinkedEntriesForUser(
	userId: string,
	organizationId: string,
	entries: ActivityEntry[]
): Promise<ActivityEntry[]> {
	const targetOf = (e: ActivityEntry): string | null => {
		if (e.eventType !== 'linked' || !e.newValue || typeof e.newValue !== 'object') return null;
		const t = (e.newValue as Record<string, unknown>).targetIssueId;
		return typeof t === 'string' ? t : null;
	};
	const ids = [...new Set(entries.map(targetOf).filter((id): id is string => id !== null))];
	if (ids.length === 0) return entries;

	const lookup = ids.filter((id) => isUuid(id));
	const [rows, scopes] = await Promise.all([
		lookup.length
			? db
					.select({ id: issues.id, projectId: issues.projectId, environment: issues.environment })
					.from(issues)
					.innerJoin(projects, eq(projects.id, issues.projectId))
					.where(and(inArray(issues.id, lookup), eq(projects.organizationId, organizationId)))
			: Promise.resolve([]),
		resolveEnvironmentScopes(userId),
	]);
	const visible = new Set(
		rows
			.filter((r) => {
				const scope = scopeForProject(scopes, r.projectId);
				return scope === null || scope.includes(r.environment);
			})
			.map((r) => r.id)
	);

	return entries.map((e) => {
		const target = targetOf(e);
		if (target === null || visible.has(target)) return e;
		return { ...e, newValue: { ...(e.newValue as Record<string, unknown>), targetIssueId: null, targetRedacted: true } };
	});
}
