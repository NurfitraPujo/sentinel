import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import {
	getReportDetail,
	updateManualIssueReport,
	deleteManualIssue,
	type ReportSeverity,
} from '$lib/db/queries/reports';
import { getReporterId, getOrgRole } from '$lib/server/report-access';
import { bestEffortDeleteObjects } from '$lib/server/retention';

/**
 * R11 (docs/plans/PR13_REVIEW_REMEDIATION_PLAN.md, §9): the design's permission matrix promises
 * "author edit/delete of own report body until resolved" but no route implemented it -- this is
 * that route.
 *
 * PATCH: author-only (NOT the broader ISSUE_WRITE_ROLES set `canEditReport` uses for editing
 * OTHERS' reports elsewhere in the matrix), 403 once the issue's status is 'resolved'. Updates
 * title/bodyMd/severity in one transaction with a `report_edited` activity row (R12 reserves that
 * event type for exactly this, now that creation writes `report_created`).
 *
 * DELETE: the author may delete their own report until it is resolved; an org owner/admin may
 * delete ANY report at any time (mirrors report-access.ts's force-release role gate). Deletes the
 * `issues` row -- FK `ON DELETE CASCADE` removes the `manual_issue_reports`/`issue_activity`/
 * `issue_comments`/`issue_subscriptions`/`attachments` rows with it -- but MinIO objects are NOT
 * transactional, so `deleteManualIssue` collects every attachment `storage_key` BEFORE the delete
 * (R6's pattern) and this route best-effort-deletes them AFTER the transaction commits, via the
 * SAME `bestEffortDeleteObjects` helper retention.ts's R6 fix uses.
 */

const VALID_SEVERITIES = ['low', 'medium', 'high', 'critical'] as const;

function isValidSeverity(value: unknown): value is ReportSeverity {
	return typeof value === 'string' && (VALID_SEVERITIES as readonly string[]).includes(value);
}

async function loadReportOrNotFound(orgId: string, issueId: string) {
	const detail = await getReportDetail(issueId);
	if (!detail || detail.organizationId !== orgId) {
		throw error(404, 'Report not found');
	}
	return detail;
}

export const PATCH: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, issueId } = params;
	if (!orgId || !issueId) {
		throw error(400, 'Missing organizationId or issueId');
	}

	const userId = session.user.id;
	const detail = await loadReportOrNotFound(orgId, issueId);

	const reporterId = await getReporterId(issueId);
	if (reporterId !== userId) {
		throw error(403, 'Forbidden: only the report author may edit this report');
	}
	if (detail.issue.status === 'resolved') {
		throw error(403, 'Forbidden: this report has been resolved and can no longer be edited');
	}

	const body = await request.json().catch(() => ({}));
	const { title, bodyMd, severity } = body;

	if (title === undefined && bodyMd === undefined && severity === undefined) {
		throw error(400, 'At least one of title, bodyMd, severity is required');
	}
	if (title !== undefined && (typeof title !== 'string' || title.trim().length === 0)) {
		throw error(400, 'title must be a non-empty string');
	}
	if (bodyMd !== undefined && (typeof bodyMd !== 'string' || bodyMd.trim().length === 0)) {
		throw error(400, 'bodyMd must be a non-empty string');
	}
	if (severity !== undefined && !isValidSeverity(severity)) {
		throw error(400, `severity must be one of: ${VALID_SEVERITIES.join(', ')}`);
	}

	const { issue, report } = await updateManualIssueReport({
		issueId,
		actorId: userId,
		title: typeof title === 'string' ? title.trim() : undefined,
		bodyMd: typeof bodyMd === 'string' ? bodyMd.trim() : undefined,
		severity: isValidSeverity(severity) ? severity : undefined,
	});

	return json({ issue, report });
};

export const DELETE: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, issueId } = params;
	if (!orgId || !issueId) {
		throw error(400, 'Missing organizationId or issueId');
	}

	const userId = session.user.id;
	const detail = await loadReportOrNotFound(orgId, issueId);

	const reporterId = await getReporterId(issueId);
	const role = await getOrgRole(userId, orgId);
	const isModerator = role === 'owner' || role === 'admin';
	const isAuthorAndUnresolved = reporterId === userId && detail.issue.status !== 'resolved';

	if (!isModerator && !isAuthorAndUnresolved) {
		throw error(
			403,
			'Forbidden: only the report author (before resolution) or an org owner/admin may delete this report'
		);
	}

	const { storageKeys } = await deleteManualIssue(issueId);
	await bestEffortDeleteObjects(storageKeys, 'manual_issue_delete');

	return json({ success: true });
};
