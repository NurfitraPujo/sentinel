import { db } from '$lib/server/db';
import { issues, issueActivity, issueRelations, projects } from '$lib/db/schema';
import { eq, and, desc, sql, inArray } from 'drizzle-orm';
import semver from 'semver';
import type { RelationType } from '$lib/types/relation-type';
import { subscribe } from '$lib/db/queries/subscriptions';
import { notifyIssueEvent, type NotifiedUser } from '$lib/server/notify';

// Robust semver comparison helper with fallback
function isRegression(releaseVersion: string, resolvedInVersion: string): boolean {
	try {
		const cleanRel = semver.clean(releaseVersion) || semver.coerce(releaseVersion)?.version;
		const cleanRes = semver.clean(resolvedInVersion) || semver.coerce(resolvedInVersion)?.version;
		if (cleanRel && cleanRes) {
			return semver.gte(cleanRel, cleanRes);
		}
	} catch (e) {
		// Fallback
	}

	const relParts = releaseVersion.replace(/[^0-9.]/g, '').split('.').map(Number);
	const resParts = resolvedInVersion.replace(/[^0-9.]/g, '').split('.').map(Number);
	for (let i = 0; i < Math.max(relParts.length, resParts.length); i++) {
		const rel = relParts[i] || 0;
		const res = resParts[i] || 0;
		if (rel > res) return true;
		if (rel < res) return false;
	}
	return releaseVersion.localeCompare(resolvedInVersion) >= 0;
}

export async function updateIssueStatus(
	issueId: string,
	status: 'unresolved' | 'resolved' | 'ignored',
	resolvedInVersion?: string,
	actorType?: 'user' | 'agent',
	actorId?: string
): Promise<{ changed: boolean; notified: NotifiedUser[] }> {
	return await db.transaction(async (tx) => {
		const [existing] = await tx
			.select({
				status: issues.status,
				waitingOn: issues.waitingOn,
				resolvedInVersion: issues.resolvedInVersion,
			})
			.from(issues)
			.where(eq(issues.id, issueId));

		// A05-status (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7d): a retried PATCH .../status
		// with the SAME status AND the same resolved_in_version is a natural no-op guard against a
		// dropped-response retry -- it must not insert a second `status_changed` activity row or
		// re-fire the resolved-notification email. Only `status` + `resolvedInVersion` are compared:
		// a different actor re-sending the identical transition is still a no-op by this contract
		// (the activity trail already has the original actor on the first, real write).
		const normalizedResolvedInVersion = status === 'resolved' ? resolvedInVersion || null : null;
		if (
			existing &&
			existing.status === status &&
			(existing.resolvedInVersion ?? null) === normalizedResolvedInVersion
		) {
			return { changed: false, notified: [] };
		}

		const updateData: any = { status };

		if (status === 'resolved') {
			updateData.resolvedInVersion = resolvedInVersion || null;
			updateData.resolvedAt = new Date();
			if (actorType) updateData.resolvedByType = actorType;
			if (actorId) updateData.resolvedBy = actorId;
		} else {
			updateData.resolvedInVersion = null;
			updateData.resolvedAt = null;
			updateData.resolvedByType = null;
			updateData.resolvedBy = null;
		}

		// R7 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md): resolving or ignoring an issue that was
		// `waiting_on` someone left it stuck in the "Needs input" tab forever -- nothing else ever
		// clears `waiting_on` once the issue leaves the unresolved state. Clearing it here mirrors
		// createComment's own clearing of `waiting_on` on a user reply (queries/comments.ts).
		if ((status === 'resolved' || status === 'ignored') && existing?.waitingOn) {
			updateData.waitingOn = null;
			// N9 (AGENT_WORKER_PLAN C12): clear waiting_since alongside waiting_on so a resolved issue
			// never surfaces a stale "waiting since" age if it is later re-opened without a new question.
			updateData.waitingSince = null;
		}

		await tx.update(issues)
			.set(updateData)
			.where(eq(issues.id, issueId));

		// event_type CHECK on issue_activity requires 'status_changed', not 'status_change'.
		await tx.insert(issueActivity).values({
			issueId,
			eventType: 'status_changed',
			actorType: actorType || 'system',
			actorId: actorId || 'system',
			oldValue: existing ? { status: existing.status, waitingOn: existing.waitingOn } : null,
			newValue: { status, resolvedInVersion },
		});

		// §8: 'resolved' gets its own notification kind (design's email-policy table lists it
		// separately from 'status_changed'); every other status transition (including back to
		// 'unresolved'/'ignored') is 'status_changed'.
		const notified = await notifyIssueEvent(tx, {
			issueId,
			kind: status === 'resolved' ? 'resolved' : 'status_changed',
			actorType: actorType || 'system',
			actorId: actorId || 'system',
			payload: { status, resolvedInVersion },
		});

		return { changed: true, notified };
	});
}

// Same order of magnitude as the ingestor's own batch cap (apps/ingestor-go/main.go:47,
// maxBatchSize = 500) and the same reasoning: an uncapped inArray(...) built straight from a
// request body is both a lock-duration problem and a DoS-shaped one.
export const MAX_BATCH_ISSUE_IDS = 500;

export async function batchUpdateIssues(
	projectId: string,
	action: 'resolve' | 'ignore' | 'unresolve' | 'assign',
	issueIds: string[],
	options: {
		resolvedInVersion?: string;
		assigneeType?: 'user' | 'agent';
		assignedTo?: string;
		actorType?: 'user' | 'agent';
		actorId?: string;
	} = {}
) {
	if (issueIds.length > MAX_BATCH_ISSUE_IDS) {
		throw new Error(
			`Batch too large: ${issueIds.length} ids exceeds max of ${MAX_BATCH_ISSUE_IDS}`
		);
	}

	// CONTEXT.md "Claim" / DECISIONS.md D24: claims are only ever self-acquired — nothing assigns
	// an issue *to* an agent on its behalf. Checked before the transaction opens, like the cap.
	if (action === 'assign' && (options.assigneeType as string) === 'agent') {
		throw new AgentAssignmentError();
	}

	// NOTE for anyone arriving here to "fix the batch deadlock": there is a deadlock trace in this
	// repo's review history attributed to this function, and sorting issueIds was proposed as the
	// fix. It was probed against a real Postgres on 2026-07-31 and does NOT apply to this code:
	//
	//   - The UPDATE below plans as a Bitmap Heap Scan (verified with EXPLAIN), so Postgres locks the
	//     matched rows in PHYSICAL order — the IN list's order is irrelevant to lock acquisition.
	//     Two concurrent calls with reversed id lists could not be made to deadlock; neither could
	//     two with identical lists.
	//   - The issue_activity insert only touches rows this transaction has ALREADY locked
	//     exclusively via the UPDATE (see the RETURNING-scoped activityRows below), so its FK checks
	//     acquire nothing new and cannot participate in a cycle.
	//
	// The trace in the review history was reproduced against per-row UPDATEs in a loop — a different
	// statement shape from the single statement here. So no sort: an inert line under a confident
	// comment is worse than no line, because the next reader trusts it. If a real deadlock involving
	// this function is ever OBSERVED, capture the actual `pg_locks` cycle before changing anything —
	// the fix would likely be `SELECT ... FOR UPDATE ORDER BY id` (which does control lock order), not
	// reordering an IN list, and it may well involve a different statement elsewhere entirely.

	return await db.transaction(async (tx) => {
		const updateData: any = {};
		// event_type CHECK on issue_activity requires 'status_changed', not 'status_change'.
		let eventType = 'status_changed';

		switch (action) {
			case 'resolve':
				updateData.status = 'resolved';
				if (options.resolvedInVersion) {
					updateData.resolvedInVersion = options.resolvedInVersion;
				}
				break;
			case 'ignore':
				updateData.status = 'ignored';
				break;
			case 'unresolve':
				updateData.status = 'unresolved';
				updateData.resolvedInVersion = null;
				break;
			case 'assign':
				updateData.assigneeType = options.assigneeType;
				updateData.assignedTo = options.assignedTo;
				// An assignment is not a claim (CONTEXT.md "Claim"): a claim carries claimed_at and is
				// only ever written by claimIssue. Leaving a stale claimed_at behind here would make a
				// dashboard assignment look claim-like to the reaper and the events feed.
				updateData.claimedAt = null;
				eventType = options.assignedTo ? 'assigned' : 'unassigned';
				break;
		}

		const updated = await tx.update(issues)
			.set(updateData)
			.where(
				and(
					eq(issues.projectId, projectId),
					inArray(issues.id, issueIds)
				)
			)
			.returning({ id: issues.id });

		// Activity is written ONLY for issues the UPDATE above actually matched, which is why this
		// reads the RETURNING ids rather than mapping over the caller's issueIds.
		//
		// Mapping over the caller's ids — as this did until 2026-07-31 — writes an activity row for
		// every id the caller sent, while the UPDATE is correctly scoped by projectId. An id belonging to a
		// DIFFERENT project therefore got audit history appended to another tenant's issue (and an FK
		// lock taken on it) while its status was, correctly, left alone. That is a cross-tenant write
		// driven by request-body input — B7's rule is that tenant scope comes from the credential, not
		// the body, and it has to hold for every statement in the transaction, not just the first one.
		const updatedIds = updated.map((row) => row.id);
		const activityRows = updatedIds.map((issueId) => ({
			issueId,
			eventType,
			actorType: options.actorType || 'system',
			actorId: options.actorId || 'system',
			newValue: { action, ...options },
		}));

		if (activityRows.length > 0) {
			await tx.insert(issueActivity).values(activityRows);
		}

		// The count of issues ACTUALLY updated, not the number of ids the caller sent — those differ
		// whenever a request names an issue outside this project, and reporting the request's length
		// would tell the caller it had changed rows it never touched.
		return updatedIds.length;
	});
}

// CONTEXT.md "Claim" / DECISIONS.md D24: a Claim is acquired atomically by the agent ITSELF
// (claimIssue in queries/reports.ts, POST /api/agent/issues/:id/claim) and carries claimed_at.
// Nothing assigns an issue *to* an agent on its behalf — a dashboard-assigned agent would produce
// a claim-like state with claimed_at NULL that the stale-claim reaper can never reap and that the
// agent never acquired, journaled, or heartbeats.
export class AgentAssignmentError extends Error {
	constructor(
		message = 'Issues cannot be assigned to agents. Agents claim issues themselves via POST /api/agent/issues/:id/claim.'
	) {
		super(message);
		this.name = 'AgentAssignmentError';
	}
}

export async function assignIssue(
	issueId: string,
	assigneeType: 'user' | 'agent' | null,
	assignedTo: string | null,
	actorType: 'user' | 'agent',
	actorId: string
) {
	// The 'agent' member of assigneeType's union survives only so callers get this error rather
	// than a silent type hole — see AgentAssignmentError above.
	if (assigneeType === 'agent') {
		throw new AgentAssignmentError();
	}

	return await db.transaction(async (tx) => {
		// Unassigning an agent-claimed issue from the dashboard is a deliberate admin override of a
		// claim — that's the release path, so it must be journaled as claim_released, not a generic
		// unassigned. Read the prior state to tell the two apart.
		let priorAgentClaim = false;
		if (!assignedTo) {
			const [current] = await tx
				.select({ assigneeType: issues.assigneeType, assignedTo: issues.assignedTo })
				.from(issues)
				.where(eq(issues.id, issueId));
			priorAgentClaim = current?.assigneeType === 'agent' && current.assignedTo !== null;
		}

		// claimedAt is always cleared: an assignment is not a claim (only claimIssue sets it), and
		// an unassign is a release, which clears it the same way releaseClaim does.
		await tx.update(issues)
			.set({ assigneeType, assignedTo, claimedAt: null })
			.where(eq(issues.id, issueId));

		const eventType = assignedTo ? 'assigned' : priorAgentClaim ? 'claim_released' : 'unassigned';

		await tx.insert(issueActivity).values({
			issueId,
			eventType,
			actorType,
			actorId,
			newValue: { assigneeType, assignedTo },
		});

		// §8 auto-subscribe: an assignee is treated like a claimant (reason 'claimant'). USER
		// assignees only -- agents get no notifications row in M4, so subscribing one here would
		// only grow issue_subscriptions with a row notifyIssueEvent always skips. No fan-out call:
		// design's fan-out wiring list does not include assignIssue (only auto-subscribe).
		if (assignedTo && assigneeType === 'user') {
			await subscribe({ issueId, subscriberType: 'user', subscriberId: assignedTo, reason: 'claimant' }, tx);
		}
	});
}

// A12 (docs/plans/AGENT_AUTOMATION_REMEDIATION_PLAN.md N7d): mirrors the human relations route's
// existing `duplicate_of` 2-cycle guard (routes/api/issues/[issueId]/relations/+server.ts:101-115)
// but for `caused_by`, and lives in the QUERY layer so both the human route and the agent op
// (issuesRelationsAdd -> createIssueRelation) get it uniformly instead of duplicating the check at
// each call site. 2-cycle only (A caused_by B, then B caused_by A) -- NOT full graph-cycle
// detection, which is out of scope (plan explicitly declines it).
export class RelationCycleError extends Error {}

export async function createIssueRelation(
	sourceIssueId: string,
	targetIssueId: string,
	relationType: RelationType,
	createdByType: 'user' | 'agent' | 'system',
	createdBy: string
): Promise<{ relation: typeof issueRelations.$inferSelect; notified: NotifiedUser[] }> {
	return await db.transaction(async (tx) => {
		if (relationType === 'caused_by') {
			const reverse = await tx
				.select({ id: issueRelations.id })
				.from(issueRelations)
				.where(
					and(
						eq(issueRelations.sourceIssueId, targetIssueId),
						eq(issueRelations.targetIssueId, sourceIssueId),
						eq(issueRelations.relationType, 'caused_by')
					)
				);

			if (reverse.length > 0) {
				throw new RelationCycleError('Reverse relation already exists (would create a cycle)');
			}
		}

		const [relation] = await tx.insert(issueRelations).values({
			sourceIssueId,
			targetIssueId,
			relationType,
			createdByType,
			createdBy,
		}).returning();

		await tx.insert(issueActivity).values({
			issueId: sourceIssueId,
			eventType: 'linked',
			actorType: createdByType,
			actorId: createdBy,
			newValue: { targetIssueId, relationType },
		});

		// §8: 'linked' fan-out is scoped to the SOURCE issue's subscribers only -- the target
		// issue's own subscribers already see the link via its own activity/comment feed once
		// something references it; this notification is specifically "something changed on an
		// issue you're subscribed to", not "your issue got mentioned elsewhere".
		const notified = await notifyIssueEvent(tx, {
			issueId: sourceIssueId,
			kind: 'linked',
			actorType: createdByType,
			actorId: createdBy,
			payload: { targetIssueId, relationType },
		});

		return { relation, notified };
	});
}

// Unlink counterpart to createIssueRelation. Deletes the matching issue_relations row and, if one
// was actually removed, logs an issue_activity row the same way the link path does.
//
// eventType is 'linked', not 'unlinked': issue_activity.event_type has a DB CHECK constraint
// (packages/db-migrations/migrations/1721900000_add_issue_lifecycle_and_relations.sql:88) that only
// permits 'status_changed' | 'assigned' | 'unassigned' | 'regressed' | 'ai_analysis' | 'linked' — an
// insert with any other value is rejected outright, and adding a new permitted value requires a
// migration outside this app's scope. The unlink is distinguished in the payload itself
// (newValue.action === 'unlink') rather than by a new event_type.
export async function deleteIssueRelation(
	sourceIssueId: string,
	targetIssueId: string,
	relationType: RelationType,
	createdByType: 'user' | 'agent' | 'system',
	createdBy: string
) {
	return await db.transaction(async (tx) => {
		const deleted = await tx
			.delete(issueRelations)
			.where(
				and(
					eq(issueRelations.sourceIssueId, sourceIssueId),
					eq(issueRelations.targetIssueId, targetIssueId),
					eq(issueRelations.relationType, relationType)
				)
			)
			.returning();

		if (deleted.length === 0) {
			return null;
		}

		await tx.insert(issueActivity).values({
			issueId: sourceIssueId,
			eventType: 'linked',
			actorType: createdByType,
			actorId: createdBy,
			newValue: { targetIssueId, relationType, action: 'unlink' },
		});

		return deleted[0];
	});
}

export async function getIssueActivity(issueId: string) {
	return await db
		.select()
		.from(issueActivity)
		.where(eq(issueActivity.issueId, issueId))
		.orderBy(desc(issueActivity.createdAt));
}

export async function getIssueRelations(issueId: string) {
	const outgoing = await db
		.select({
			id: issueRelations.id,
			sourceIssueId: issueRelations.sourceIssueId,
			targetIssueId: issueRelations.targetIssueId,
			relationType: issueRelations.relationType,
			createdByType: issueRelations.createdByType,
			createdBy: issueRelations.createdBy,
			createdAt: issueRelations.createdAt,
			direction: sql<'outgoing' | 'incoming'>`'outgoing'`,
			// D43: this joined issue IS the target for an outgoing row (issueId is the source), so
			// `relatedIssue` is accurate here too — it is simply named to match the incoming branch
			// below, where the same key would otherwise lie about which side of the relation it is.
			relatedIssue: {
				id: issues.id,
				errorClass: issues.errorClass,
				message: issues.message,
				status: issues.status,
				fingerprint: issues.fingerprint,
			},
		})
		.from(issueRelations)
		.innerJoin(issues, eq(issues.id, issueRelations.targetIssueId))
		.where(eq(issueRelations.sourceIssueId, issueId));

	const incoming = await db
		.select({
			id: issueRelations.id,
			sourceIssueId: issueRelations.sourceIssueId,
			targetIssueId: issueRelations.targetIssueId,
			relationType: issueRelations.relationType,
			createdByType: issueRelations.createdByType,
			createdBy: issueRelations.createdBy,
			createdAt: issueRelations.createdAt,
			direction: sql<'outgoing' | 'incoming'>`'incoming'`,
			// D43: this joined issue is issueRelations.sourceIssueId — the SOURCE of the relation, not
			// the target. The old key name `targetIssue` was simply wrong here; `relatedIssue` names
			// what it actually is: "the other issue in this relation", regardless of direction.
			relatedIssue: {
				id: issues.id,
				errorClass: issues.errorClass,
				message: issues.message,
				status: issues.status,
				fingerprint: issues.fingerprint,
			},
		})
		.from(issueRelations)
		.innerJoin(issues, eq(issues.id, issueRelations.sourceIssueId))
		.where(eq(issueRelations.targetIssueId, issueId));

	return [...outgoing, ...incoming];
}

// issues.id is `uuid` (schema.ts:59), and Postgres has no ILIKE operator for uuid — every search
// with the raw column in the ILIKE clause threw `operator does not exist: uuid ~~* unknown` (D02),
// which made this the ONLY way to pick a link target for the relations UI, so the entire link/
// duplicate flow was unusable.
//
// Cast to text (`issues.id::text ILIKE ...`) rather than leaving the column out of the id match:
// a user pasting a full or partial UUID into the search box is a real, common case (copy-pasting an
// id from another tab, a linked Slack message, etc.), and dropping id matching entirely would
// silently break that. Substring matching over a UUID's hex digits is a broad scan, but this table
// is already filtered to a single organization via the projects join and capped with .limit(10), so
// the match set stays bounded; a prefix-only match (`id::text ILIKE query || '%'`) would miss the
// "pasted the middle of an id" case for no real performance win at this scale, so full substring
// matching was kept for consistency with the other ILIKE columns here.
// Manual Issues M1 (design §9/§10, Q9): `issueType` is optional and defaults to unfiltered,
// because this function is also the search behind the linked-issues panel — the DELIBERATE
// bridge across the system_error/user_report split (§9: "Linked-issues panel is the only
// bridge"). The error dashboard's own search bar (routes/api/issues/search) passes
// issueType='system_error' explicitly; the relations endpoint leaves it unset so a manual
// report can find and link a service issue (and vice versa).
export async function searchIssuesInOrg(
	orgId: string,
	query: string,
	excludeIssueId?: string,
	issueType?: string
) {
	const sanitized = query.trim().replace(/[%_\\]/g, '\\$&');
	const searchTerm = `%${sanitized}%`;
	const baseQuery = db
		.select({
			id: issues.id,
			errorClass: issues.errorClass,
			message: issues.message,
			status: issues.status,
			fingerprint: issues.fingerprint,
			projectId: issues.projectId,
		})
		.from(issues)
		.innerJoin(projects, eq(projects.id, issues.projectId))
		.where(
			and(
				eq(projects.organizationId, orgId),
				excludeIssueId ? sql`${issues.id} != ${excludeIssueId}` : sql`1=1`,
				issueType ? eq(issues.issueType, issueType) : sql`1=1`,
				sql`(${issues.id}::text ILIKE ${searchTerm} OR ${issues.errorClass} ILIKE ${searchTerm} OR ${issues.message} ILIKE ${searchTerm} OR ${issues.fingerprint} ILIKE ${searchTerm})`
			)
		)
		.limit(10);

	return await baseQuery;
}

// No callers as of this writing (grep the repo before assuming otherwise). Left unfixed
// deliberately: the SELECT below reads `issue` and the UPDATE further down writes it back based on
// that read, with no `FOR UPDATE` and no retry/optimistic-lock check in between. That's a plain
// read-then-write race — two concurrent calls for the same issueId can both read the pre-update
// status, both decide a regression applies, and both write, silently double-incrementing
// regressionCount / losing an interleaved status change. Add `.for('update')` on the SELECT (or a
// WHERE that re-checks the read status atomically) before wiring this up to a real caller.
export async function detectAndHandleRegression(issueId: string, releaseVersion: string) {
	return await db.transaction(async (tx) => {
		// MUST NOT query issue_relations here
		const [issue] = await tx.select().from(issues).where(eq(issues.id, issueId));
		if (!issue) return;

		if (issue.status === 'resolved' && issue.resolvedInVersion) {
			if (isRegression(releaseVersion, issue.resolvedInVersion)) {
				await tx.update(issues)
					.set({
						status: 'unresolved',
						regressionStatus: 'regressed',
						regressionCount: sql`${issues.regressionCount} + 1`,
						lastRegressedAt: new Date(),
						resolvedInVersion: null,
						resolvedAt: null,
						resolvedByType: null,
						resolvedBy: null
					})
					.where(eq(issues.id, issueId));

				await tx.insert(issueActivity).values({
					issueId,
					eventType: 'regressed',
					actorType: 'system',
					actorId: 'system',
					oldValue: { status: issue.status, resolvedInVersion: issue.resolvedInVersion },
					newValue: { releaseVersion, status: 'unresolved' },
				});
			}
		}
	});
}
