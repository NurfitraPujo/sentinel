import { db } from '$lib/server/db';
import { issueActivity, issues, issueTombstones, projects } from '$lib/db/schema';
import { and, asc, eq, gt, inArray, sql } from 'drizzle-orm';
import { EVENTS_LAG_GUARD_INTERVAL, type AgentEventType } from '$lib/server/agent-events';

// N8 (DECISIONS.md D20): a deleted issue surfaces on the feed as this synthetic eventType. The
// tombstone is authored by retention (retention.ts), so the actor is fixed here, not stored.
const ISSUE_DELETED_EVENT_TYPE = 'issue_deleted';
const TOMBSTONE_ACTOR_TYPE = 'system';
const TOMBSTONE_ACTOR_ID = 'sentinel-retention';

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
 *
 * N8 (DECISIONS.md D20): the feed is the UNION of two sources -- `issue_activity` (joined to its
 * still-existing issue) and `issue_tombstones` (rows that OUTLIVE the deleted issue, so a
 * claim-holding / question-awaiting agent gets a terminal 'issue_deleted' signal instead of a
 * silent 404). Both share the one `issue_activity` identity sequence, so `seq` orders across them.
 * Rather than a single SQL UNION (which would force the whole query out of the builder that the
 * co-located unit tests assert on), each source is fetched independently -- each ordered by `seq`
 * asc and capped at `limit + 1` -- then merged by `seq` in JS. A merge of two ascending lists each
 * truncated to N still yields the correct smallest N of the combined set, so cursor/hasMore hold.
 * The tombstone source is skipped entirely when an `eventTypes` filter excludes 'issue_deleted'.
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
		// N9 (C2): CURRENT claim/waiting state at read time -- NOT the state when this event
		// occurred. Lets an agent dispatcher evaluate "claimed by me" per-event without a
		// follow-up GET /api/agent/issues/:id.
		assigneeType: string | null;
		assignedTo: string | null;
		claimedAt: Date | null;
		waitingOn: string | null;
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
			issueAssigneeType: issues.assigneeType,
			issueAssignedTo: issues.assignedTo,
			issueClaimedAt: issues.claimedAt,
			issueWaitingOn: issues.waitingOn,
		})
		.from(issueActivity)
		.innerJoin(issues, eq(issues.id, issueActivity.issueId))
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(and(...conditions))
		.orderBy(asc(issueActivity.seq))
		.limit(options.limit + 1);

	const activityEvents: OrgActivityEvent[] = rows.map((row) => ({
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
			assigneeType: row.issueAssigneeType,
			assignedTo: row.issueAssignedTo,
			claimedAt: row.issueClaimedAt,
			waitingOn: row.issueWaitingOn,
		},
	}));

	const tombstoneEvents = await listOrgTombstones(options);

	// Merge the two seq-ordered sources and re-apply cursor/hasMore over the combined set. Each
	// source already returned at most `limit + 1` in ascending seq order, so the combined smallest
	// `limit + 1` is a subset of what we have -- sorting and slicing here is exact, not approximate.
	const merged = [...activityEvents, ...tombstoneEvents].sort((a, b) => a.seq - b.seq);
	const hasMore = merged.length > options.limit;
	const events = hasMore ? merged.slice(0, options.limit) : merged;

	const cursor = events.length > 0 ? events[events.length - 1].seq : options.after;

	return { events, cursor, hasMore };
}

/**
 * N8 (DECISIONS.md D20): the tombstone half of the events feed. Returns at most `limit + 1` rows in
 * ascending `seq` order so its caller can merge it with the issue_activity half and re-derive
 * cursor/hasMore over the union. Scoped by the credential's organization (B7) exactly like the
 * activity half, and honouring the same project/claimed filters -- assignee_type/assigned_to are
 * snapshotted onto the tombstone precisely so `?claimed=me` can still surface a deleted claim.
 * Skipped (returns []) when an explicit `eventTypes` filter does not include 'issue_deleted'.
 */
async function listOrgTombstones(options: ListOrgActivityOptions): Promise<OrgActivityEvent[]> {
	if (
		options.eventTypes &&
		options.eventTypes.length > 0 &&
		!(options.eventTypes as string[]).includes(ISSUE_DELETED_EVENT_TYPE)
	) {
		return [];
	}

	const conditions = [
		eq(issueTombstones.organizationId, options.organizationId),
		gt(issueTombstones.seq, options.after),
		sql`${issueTombstones.deletedAt} < now() - interval '${sql.raw(EVENTS_LAG_GUARD_INTERVAL)}'`,
	];

	if (options.projectId) {
		conditions.push(eq(issueTombstones.projectId, options.projectId));
	}
	if (options.claimedByAgentId) {
		conditions.push(eq(issueTombstones.assigneeType, 'agent'));
		conditions.push(eq(issueTombstones.assignedTo, options.claimedByAgentId));
	}

	const rows = await db
		.select({
			seq: issueTombstones.seq,
			reason: issueTombstones.reason,
			deletedAt: issueTombstones.deletedAt,
			issueId: issueTombstones.issueId,
			issueMessage: issueTombstones.issueMessage,
			issueType: issueTombstones.issueType,
			issueProjectId: issueTombstones.projectId,
		})
		.from(issueTombstones)
		.where(and(...conditions))
		.orderBy(asc(issueTombstones.seq))
		.limit(options.limit + 1);

	return rows.map((row) => ({
		seq: row.seq,
		eventType: ISSUE_DELETED_EVENT_TYPE,
		actorType: TOMBSTONE_ACTOR_TYPE,
		actorId: TOMBSTONE_ACTOR_ID,
		oldValue: null,
		newValue: { reason: row.reason, deletedAt: row.deletedAt },
		createdAt: row.deletedAt,
		issue: {
			id: row.issueId,
			title: row.issueMessage ?? '',
			// The issue no longer exists; 'deleted' is a synthetic terminal status distinct from the
			// real check_status values so a consumer can tell "gone" from "resolved".
			status: 'deleted',
			issueType: row.issueType ?? '',
			projectId: row.issueProjectId,
			// N9 (C2): the issue is gone, so there is no CURRENT claim/waiting state to report --
			// these are null for every tombstone event (a deleted issue is not claimed or waiting).
			assigneeType: null,
			assignedTo: null,
			claimedAt: null,
			waitingOn: null,
		},
	}));
}
