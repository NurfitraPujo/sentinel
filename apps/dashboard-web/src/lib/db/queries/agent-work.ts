import { db } from '$lib/server/db';
import { issues, projects, manualIssueReports, issueActivity } from '$lib/db/schema';
import { and, desc, eq, gte, isNull, sql } from 'drizzle-orm';
import { notifyIssueEvent, type NotifiedUser } from '$lib/server/notify';
import { AGENT_DEDUPE_WINDOW_MS } from '$lib/server/agent-dedupe';

/**
 * Manual Issues M5 stage 2 (design §7 step 1): `GET /api/agent/issues` -- the one deliberate
 * bridge across Q9's strict issue_type separation. Spans BOTH `user_report` and `system_error`
 * issues, scoped ONLY to the calling agent's organization (B7 -- `organizationId` here MUST
 * always come from `AgentAuthContext`, never a request param). Deliberately its own query
 * module, not an addition to `reports.ts` (user_report-only, per its own header) or `issues.ts`
 * (system_error-oriented dashboard queries) -- neither is the right home for a cross-type list.
 *
 * N7b (A02): `since`/`sort`/`limit`/`cursor` bolt bootstrap-friendly pagination onto what was an
 * always-unbounded list. Keyset on `(sortColumn, id)` -- NOT offset -- because rows churn (new
 * issues arrive, `lastSeen` bumps on every occurrence) and an offset page would skip or repeat
 * rows across polls; id as the tiebreaker keeps the page stable even when two rows share the same
 * `firstSeen`/`lastSeen` timestamp (matches the events feed's cursor philosophy, events.ts's
 * header). `limit` undefined ⇒ no `.limit()` clause at all and no `nextCursor` in the result --
 * byte-identical to pre-N7b behavior for callers that pass nothing (backward compat).
 *
 * N9 (AGENT_WORKER_PLAN C12): adds `claimedByAgentId` (`claimed=me` -- this agent's own claims,
 * resolved from the credential per B7) and a `waitingSince` list field (when the current blocking
 * question was asked), so an agent can enumerate and age its own waiting questions directly.
 */
export const AGENT_ISSUES_MAX_LIMIT = 200;

export type AgentIssuesSort = 'firstSeen' | 'lastSeen';

export interface AgentIssuesCursor {
	/** The value of the sort column (firstSeen or lastSeen) on the last row of the prior page. */
	sortValue: Date;
	id: string;
}

export interface ListAgentIssuesOptions {
	organizationId: string;
	type?: 'user_report' | 'system_error';
	claimed?: boolean;
	/**
	 * N9 (AGENT_WORKER_PLAN C12): `claimed=me` -- restrict to issues claimed by THIS agent
	 * (assigneeType='agent' AND assignedTo=agentId), mirroring the events feed's claimed=me
	 * (events.ts). B7: the route resolves this from the credential's agentId, never a request param.
	 * Mutually exclusive with `claimed` at the route layer; if both arrive here this wins.
	 */
	claimedByAgentId?: string;
	projectId?: string;
	waiting?: boolean;
	/** Only issues whose firstSeen >= since. */
	since?: Date;
	/** Sort column; default 'lastSeen' matches pre-N7b behavior. Always ordered descending. */
	sort?: AgentIssuesSort;
	/** No `.limit()` applied when omitted -- legacy unbounded behavior. Caller must pre-clamp to
	 *  [1, AGENT_ISSUES_MAX_LIMIT]; this function clamps defensively too. */
	limit?: number;
	cursor?: AgentIssuesCursor;
}

export interface AgentIssueListItem {
	id: string;
	projectId: string;
	projectName: string;
	isInbox: boolean;
	issueType: string;
	message: string;
	errorClass: string;
	status: string;
	assigneeType: string | null;
	assignedTo: string | null;
	waitingOn: string | null;
	/** N9 (AGENT_WORKER_PLAN C12): when the current blocking question was asked -- null unless the
	 *  issue is currently waiting (waitingOn set). Lets an agent nag stale questions by age without
	 *  reconstructing timing from the comment thread. */
	waitingSince: Date | null;
	firstSeen: Date | null;
	lastSeen: Date | null;
	count: number;
	severity: string | null;
	reporterId: string | null;
	isWaiting: boolean;
	/** N7e (A07): when the CURRENT claim (assigneeType/assignedTo) was made -- null if unclaimed. */
	claimedAt: Date | null;
}

export interface ListAgentIssuesResult {
	issues: AgentIssueListItem[];
	/** Present only when `options.limit` was supplied and more rows exist beyond this page. */
	nextCursor?: string;
}

/** Opaque cursor codec -- base64url of `{v: ISO timestamp, id}`. Exported so the route can decode
 *  an incoming `cursor` param and the CLI/tests can round-trip a `nextCursor` value. */
export function encodeAgentIssuesCursor(sortValue: Date, id: string): string {
	return Buffer.from(JSON.stringify({ v: sortValue.toISOString(), id }), 'utf8').toString('base64url');
}

export function decodeAgentIssuesCursor(cursor: string): AgentIssuesCursor {
	let parsed: unknown;
	try {
		parsed = JSON.parse(Buffer.from(cursor, 'base64url').toString('utf8'));
	} catch {
		throw new Error('invalid cursor');
	}
	if (
		typeof parsed !== 'object' ||
		parsed === null ||
		typeof (parsed as { v?: unknown }).v !== 'string' ||
		typeof (parsed as { id?: unknown }).id !== 'string'
	) {
		throw new Error('invalid cursor');
	}
	const sortValue = new Date((parsed as { v: string }).v);
	if (Number.isNaN(sortValue.getTime())) {
		throw new Error('invalid cursor');
	}
	return { sortValue, id: (parsed as { id: string }).id };
}

export async function listAgentIssues(options: ListAgentIssuesOptions): Promise<ListAgentIssuesResult> {
	const sort: AgentIssuesSort = options.sort ?? 'lastSeen';
	const sortColumn = sort === 'firstSeen' ? issues.firstSeen : issues.lastSeen;

	const conditions = [eq(projects.organizationId, options.organizationId)];

	if (options.type) {
		conditions.push(eq(issues.issueType, options.type));
	}
	if (options.claimedByAgentId) {
		// N9 (C12): `claimed=me` -- this agent's own claims only. Mirrors events.ts's claimed=me
		// (assigneeType='agent' AND assignedTo=agentId). Takes precedence over the boolean `claimed`.
		conditions.push(eq(issues.assigneeType, 'agent'));
		conditions.push(eq(issues.assignedTo, options.claimedByAgentId));
	} else if (options.claimed === true) {
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
	if (options.since) {
		conditions.push(gte(issues.firstSeen, options.since));
	}
	if (options.cursor) {
		// Keyset predicate on the (sortColumn, id) tuple -- strictly older than the cursor row,
		// matching the descending order below. Postgres row comparison is lexicographic: this is
		// equivalent to `sortColumn < v OR (sortColumn = v AND id < id)` but a single index-usable
		// expression, and stable under concurrent inserts/updates elsewhere in the table (unlike
		// OFFSET, which drifts as rows are added/removed ahead of the page).
		conditions.push(
			sql`(${sortColumn}, ${issues.id}) < (${options.cursor.sortValue.toISOString()}::timestamp, ${options.cursor.id})`
		);
	}

	const hasLimit = options.limit !== undefined;
	const clampedLimit = hasLimit
		? Math.max(1, Math.min(options.limit as number, AGENT_ISSUES_MAX_LIMIT))
		: undefined;

	const baseQuery = db
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
			claimedAt: issues.claimedAt,
			waitingOn: issues.waitingOn,
			waitingSince: issues.waitingSince,
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
		.orderBy(desc(sortColumn), desc(issues.id));

	// Fetch one extra row (when paginating) to detect "more pages exist" without a second COUNT
	// query -- mirrors events.ts's listOrgActivity.
	const rows = clampedLimit !== undefined ? await baseQuery.limit(clampedLimit + 1) : await baseQuery;

	const hasMore = clampedLimit !== undefined && rows.length > clampedLimit;
	const page = hasMore ? rows.slice(0, clampedLimit) : rows;

	const mapped: AgentIssueListItem[] = page.map((row: (typeof rows)[number]) => ({
		...row,
		isWaiting: row.waitingOn !== null,
		// N9 (C12): only surface waitingSince for a currently-waiting issue -- a cleared question can
		// leave a stale timestamp behind (issues.ts backfill/re-open paths), so gate on waitingOn.
		waitingSince: row.waitingOn !== null ? row.waitingSince : null,
	}));

	let nextCursor: string | undefined;
	if (hasLimit && hasMore) {
		const last = page[page.length - 1];
		const lastSortValue: Date | null = sort === 'firstSeen' ? last.firstSeen : last.lastSeen;
		if (lastSortValue) {
			nextCursor = encodeAgentIssuesCursor(lastSortValue, last.id);
		}
	}

	return { issues: mapped, nextCursor };
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
		// A05-comment/progress (N7d): dedupe a retried progress post by natural key (same
		// issue+agent+message within AGENT_DEDUPE_WINDOW_MS) -- mirrors createComment's plain-comment
		// dedupe. `newValue` is jsonb; `->>'messageMd'` does a text comparison against the stored key.
		const recentDuplicates = await tx
			.select({ id: issueActivity.id })
			.from(issueActivity)
			.where(
				and(
					eq(issueActivity.issueId, issueId),
					eq(issueActivity.eventType, 'progress_update'),
					eq(issueActivity.actorType, 'agent'),
					eq(issueActivity.actorId, agentId),
					sql`${issueActivity.newValue}->>'messageMd' = ${messageMd}`,
					gte(issueActivity.createdAt, new Date(Date.now() - AGENT_DEDUPE_WINDOW_MS))
				)
			);

		if (recentDuplicates.length > 0) {
			return { notified: [] };
		}

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
