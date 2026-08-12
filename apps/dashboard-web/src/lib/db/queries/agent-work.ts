import { db } from '$lib/server/db';
import { issues, projects, manualIssueReports, issueActivity } from '$lib/db/schema';
import { and, desc, eq, isNull, sql } from 'drizzle-orm';
import { notifyIssueEvent, type NotifiedUser } from '$lib/server/notify';

/**
 * Manual Issues M5 stage 2 (design §7 step 1): `GET /api/agent/issues` -- the one deliberate
 * bridge across Q9's strict issue_type separation. Spans BOTH `user_report` and `system_error`
 * issues, scoped ONLY to the calling agent's organization (B7 -- `organizationId` here MUST
 * always come from `AgentAuthContext`, never a request param). Deliberately its own query
 * module, not an addition to `reports.ts` (user_report-only, per its own header) or `issues.ts`
 * (system_error-oriented dashboard queries) -- neither is the right home for a cross-type list.
 */
export interface ListAgentIssuesOptions {
	organizationId: string;
	type?: 'user_report' | 'system_error';
	claimed?: boolean;
	projectId?: string;
	waiting?: boolean;
}

export async function listAgentIssues(options: ListAgentIssuesOptions) {
	const conditions = [eq(projects.organizationId, options.organizationId)];

	if (options.type) {
		conditions.push(eq(issues.issueType, options.type));
	}
	if (options.claimed === true) {
		conditions.push(sql`${issues.assignedTo} IS NOT NULL`);
	} else if (options.claimed === false) {
		conditions.push(isNull(issues.assignedTo));
	}
	if (options.projectId) {
		conditions.push(eq(issues.projectId, options.projectId));
	}
	if (options.waiting === true) {
		conditions.push(sql`${issues.waitingOn} IS NOT NULL`);
	}

	const rows = await db
		.select({
			id: issues.id,
			projectId: issues.projectId,
			projectName: projects.name,
			isInbox: projects.isInbox,
			issueType: issues.issueType,
			message: issues.message,
			errorClass: issues.errorClass,
			status: issues.status,
			assigneeType: issues.assigneeType,
			assignedTo: issues.assignedTo,
			waitingOn: issues.waitingOn,
			firstSeen: issues.firstSeen,
			lastSeen: issues.lastSeen,
			count: issues.count,
			// Manual reports only: null for system_error rows (left join).
			severity: manualIssueReports.severity,
			reporterId: manualIssueReports.reporterId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.leftJoin(manualIssueReports, eq(manualIssueReports.issueId, issues.id))
		.where(and(...conditions))
		.orderBy(desc(issues.lastSeen));

	return rows.map((row) => ({
		...row,
		isWaiting: row.waitingOn !== null,
	}));
}

/**
 * Manual Issues M5 stage 2 (design §7 step 3): `POST /api/agent/issues/[id]/progress` --
 * `progress_update` activity row + in-app-only notification (Q7: agent progress updates never
 * email, `notify.ts`'s `EMAILABLE_KINDS` deliberately omits 'progress_update'). D18: one
 * transaction, throw on failure.
 */
export async function recordAgentProgress(
	issueId: string,
	agentId: string,
	messageMd: string
): Promise<{ notified: NotifiedUser[] }> {
	return await db.transaction(async (tx) => {
		await tx.insert(issueActivity).values({
			issueId,
			eventType: 'progress_update',
			actorType: 'agent',
			actorId: agentId,
			newValue: { messageMd },
		});

		const notified = await notifyIssueEvent(tx, {
			issueId,
			kind: 'progress_update',
			actorType: 'agent',
			actorId: agentId,
			payload: { messageMd },
		});

		return { notified };
	});
}
