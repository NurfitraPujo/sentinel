import { db } from '$lib/server/db';
import { issues, issueActivity, issueRelations } from '$lib/db/schema';
import { eq, and, desc, sql, inArray } from 'drizzle-orm';
import semver from 'semver';

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
) {
	return await db.transaction(async (tx) => {
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

		await tx.update(issues)
			.set(updateData)
			.where(eq(issues.id, issueId));

		await tx.insert(issueActivity).values({
			issueId,
			eventType: 'status_change',
			actorType: actorType || 'system',
			actorId: actorId || 'system',
			metadata: { status, resolvedInVersion },
		});
	});
}

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
	return await db.transaction(async (tx) => {
		const updateData: any = {};
		let eventType = 'status_change';

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
				eventType = options.assignedTo ? 'assigned' : 'unassigned';
				break;
		}

		await tx.update(issues)
			.set(updateData)
			.where(
				and(
					eq(issues.projectId, projectId),
					inArray(issues.id, issueIds)
				)
			);

		const activityRows = issueIds.map((issueId) => ({
			issueId,
			eventType,
			actorType: options.actorType || 'system',
			actorId: options.actorId || 'system',
			metadata: { action, ...options },
		}));

		if (activityRows.length > 0) {
			await tx.insert(issueActivity).values(activityRows);
		}

		return issueIds.length;
	});
}

export async function assignIssue(
	issueId: string,
	assigneeType: 'user' | 'agent' | null,
	assignedTo: string | null,
	actorType: 'user' | 'agent',
	actorId: string
) {
	return await db.transaction(async (tx) => {
		await tx.update(issues)
			.set({ assigneeType, assignedTo })
			.where(eq(issues.id, issueId));

		const eventType = assignedTo ? 'assigned' : 'unassigned';
		
		await tx.insert(issueActivity).values({
			issueId,
			eventType,
			actorType,
			actorId,
			metadata: { assigneeType, assignedTo },
		});
	});
}

export async function createIssueRelation(
	sourceIssueId: string,
	targetIssueId: string,
	relationType: 'linked_to' | 'caused_by' | 'duplicate_of',
	createdByType: 'user' | 'agent' | 'system',
	createdBy: string
) {
	return await db.transaction(async (tx) => {
		await tx.insert(issueRelations).values({
			sourceIssueId,
			targetIssueId,
			relationType,
		});

		await tx.insert(issueActivity).values({
			issueId: sourceIssueId,
			eventType: 'linked',
			actorType: createdByType,
			actorId: createdBy,
			metadata: { targetIssueId, relationType },
		});
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
	return await db
		.select()
		.from(issueRelations)
		.where(eq(issueRelations.sourceIssueId, issueId));
}

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
					metadata: { releaseVersion, previousResolvedVersion: issue.resolvedInVersion },
				});
			}
		}
	});
}
