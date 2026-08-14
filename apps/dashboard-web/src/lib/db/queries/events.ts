import { db } from '$lib/server/db';
import { issueActivity, issues, projects } from '$lib/db/schema';
import { and, asc, eq, gt, inArray, sql } from 'drizzle-orm';
import { EVENTS_LAG_GUARD_INTERVAL, type AgentEventType } from '$lib/server/agent-events';

/**
 * N1b (events feed): `GET /api/agent/events` -- an org-scoped, seq-cursored feed over
 * `issue_activity`, joined out to the owning issue/project so a poller never has to make a
 * second round trip per event. Deliberately its own query module (not an addition to
 * `agent-work.ts`, which is issue-shaped, not activity-shaped).
 *
 * Cursor semantics: `after` is the last `seq` the caller has consumed (0 means "from the
 * start"); results are strictly `seq > after`, ordered ascending, so the highest `seq` in the
 * response is the caller's next `after`. `hasMore` is computed by fetching `limit + 1` rows and
 * slicing the extra one off, rather than a second COUNT query.
 *
 * Lag guard: `seq` is a bigint IDENTITY column (see schema.ts's `issueActivity.seq` comment) --
 * assigned at INSERT time by Postgres, not necessarily in commit order under concurrent
 * transactions. A poller reading strictly by `seq > after` could observe seq=105 committed
 * before seq=104, advance its cursor past 104, and never see it. Excluding rows younger than
 * `EVENTS_LAG_GUARD_INTERVAL` gives concurrent inserts time to commit before they become
 * visible to the feed, at the cost of a small fixed delay on every event.
 */
export interface ListOrgActivityOptions {
	organizationId: string;
	after: number;
	limit: number;
	eventTypes?: AgentEventType[];
	projectId?: string;
	claimedByAgentId?: string;
}

export interface OrgActivityEvent {
	seq: number;
	eventType: string;
	actorType: string;
	actorId: string;
	oldValue: unknown;
	newValue: unknown;
	createdAt: Date | null;
	issue: {
		id: string;
		title: string;
		status: string;
		issueType: string;
		projectId: string;
	};
}

export interface ListOrgActivityResult {
	events: OrgActivityEvent[];
	cursor: number;
	hasMore: boolean;
}

export async function listOrgActivity(
	options: ListOrgActivityOptions
): Promise<ListOrgActivityResult> {
	const conditions = [
		eq(projects.organizationId, options.organizationId),
		gt(issueActivity.seq, options.after),
		sql`${issueActivity.createdAt} < now() - interval '${sql.raw(EVENTS_LAG_GUARD_INTERVAL)}'`,
	];

	if (options.eventTypes && options.eventTypes.length > 0) {
		conditions.push(inArray(issueActivity.eventType, options.eventTypes));
	}
	if (options.projectId) {
		conditions.push(eq(issues.projectId, options.projectId));
	}
	if (options.claimedByAgentId) {
		conditions.push(eq(issues.assigneeType, 'agent'));
		conditions.push(eq(issues.assignedTo, options.claimedByAgentId));
	}

	const rows = await db
		.select({
			seq: issueActivity.seq,
			eventType: issueActivity.eventType,
			actorType: issueActivity.actorType,
			actorId: issueActivity.actorId,
			oldValue: issueActivity.oldValue,
			newValue: issueActivity.newValue,
			createdAt: issueActivity.createdAt,
			issueId: issues.id,
			issueMessage: issues.message,
			issueStatus: issues.status,
			issueType: issues.issueType,
			issueProjectId: issues.projectId,
		})
		.from(issueActivity)
		.innerJoin(issues, eq(issues.id, issueActivity.issueId))
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(and(...conditions))
		.orderBy(asc(issueActivity.seq))
		.limit(options.limit + 1);

	const hasMore = rows.length > options.limit;
	const page = hasMore ? rows.slice(0, options.limit) : rows;

	const events: OrgActivityEvent[] = page.map((row) => ({
		seq: row.seq,
		eventType: row.eventType,
		actorType: row.actorType,
		actorId: row.actorId,
		oldValue: row.oldValue,
		newValue: row.newValue,
		createdAt: row.createdAt,
		issue: {
			id: row.issueId,
			title: row.issueMessage,
			status: row.issueStatus,
			issueType: row.issueType,
			projectId: row.issueProjectId,
		},
	}));

	const cursor = events.length > 0 ? events[events.length - 1].seq : options.after;

	return { events, cursor, hasMore };
}
