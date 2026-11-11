import { inArray } from 'drizzle-orm';
import { getIssueRelations } from '$lib/db/queries/issues';
import { issues } from '$lib/db/schema';
import { db } from '$lib/server/db';
import type { AgentAuthContext } from '$lib/server/agent-auth';
import { agentVisibility, isVisibleToAgent } from '$lib/server/agent-access';
import { isUuid } from '$lib/uuid';

/**
 * sentinel-cli-browser-login: an issue's relations as this key may see them. Unchanged for an org agent; for a personal key,
 * a related issue outside the owner's projects or environment grants is redacted the way the dashboard's issue page
 * redacts it (decision 37's placeholder). Used by GET /api/agent/issues/{id} and POST /api/agent/issues/claim-next.
 */
export function agentIssueRelations(ctx: AgentAuthContext, issueId: string) {
	const visibility = agentVisibility(ctx);
	return visibility
		? getIssueRelations(issueId, visibility.environmentScopes, visibility.projectIds)
		: getIssueRelations(issueId);
}

/**
 * sentinel-cli-browser-login (security review, repair round 1): the events feed's `linked` events name the OTHER end of
 * the relation in `newValue.targetIssueId`, and the feed is filtered by the SOURCE issue only. For a personal key whose
 * owner cannot see the target issue (another project, an environment outside their grant, or an issue since deleted),
 * that id is redacted the way the issue page redacts the same relation (decision 37: the far end is nulled, a
 * `redacted` marker says why) -- `targetIssueId: null, targetRedacted: true`. Every other field is kept. Unchanged for
 * an org agent. One query for the whole page of events.
 */
export async function redactLinkedEventTargets<E extends { eventType: string; newValue: unknown }>(
	ctx: AgentAuthContext,
	events: E[]
): Promise<E[]> {
	if (!ctx.owner) return events;
	const targetOf = (event: E): string | null => {
		if (event.eventType !== 'linked' || !event.newValue || typeof event.newValue !== 'object') return null;
		const target = (event.newValue as Record<string, unknown>).targetIssueId;
		return typeof target === 'string' ? target : null;
	};
	const ids = [...new Set(events.map(targetOf).filter((id): id is string => id !== null))];
	if (ids.length === 0) return events;

	const lookup = ids.filter((id) => isUuid(id));
	const rows = lookup.length
		? await db.select({ id: issues.id, projectId: issues.projectId, environment: issues.environment }).from(issues).where(inArray(issues.id, lookup))
		: [];
	const visible = new Set(rows.filter((r) => isVisibleToAgent(ctx, r.projectId, r.environment)).map((r) => r.id));

	return events.map((event) => {
		const target = targetOf(event);
		if (target === null || visible.has(target)) return event;
		return { ...event, newValue: { ...(event.newValue as Record<string, unknown>), targetIssueId: null, targetRedacted: true } };
	});
}
