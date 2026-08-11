import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq } from 'drizzle-orm';
import { createManualIssue, listReports, type ReportTab } from '$lib/db/queries/reports';
import { requireReportAccess } from '$lib/server/report-access';
import { getAccessibleProjectIds } from '$lib/server/issue-access';

const VALID_SEVERITIES = ['low', 'medium', 'high', 'critical'] as const;
type Severity = (typeof VALID_SEVERITIES)[number];

function isValidSeverity(value: unknown): value is Severity {
	return typeof value === 'string' && (VALID_SEVERITIES as readonly string[]).includes(value);
}

const VALID_TABS = ['all', 'mine', 'claimed-by-me', 'unclaimed', 'needs-input', 'triage'] as const;

function isValidTab(value: unknown): value is ReportTab {
	return typeof value === 'string' && (VALID_TABS as readonly string[]).includes(value);
}

// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §2, §9, §10). Manual-validation style
// (allowlists + throw error(status)) matching the invitations endpoint — no schema library.

export const GET: RequestHandler = async ({ params, url, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId } = params;
	if (!orgId) {
		throw error(400, 'Missing organizationId');
	}

	const userId = session.user.id;
	// §9: read (list) is available to any recognized org member, viewers included.
	const { role } = await requireReportAccess(userId, orgId, 'read');

	const tabParam = url.searchParams.get('tab') ?? 'all';
	if (!isValidTab(tabParam)) {
		throw error(400, `tab must be one of: ${VALID_TABS.join(', ')}`);
	}

	// A viewer sees only reports in projects they are actually a member of (mirrors D10's
	// project-membership scoping for the error-issue search path); write roles see the whole org,
	// since an org-level write role is itself an org-wide grant (see issue-access.ts's comment).
	const isOrgWideRole = role !== 'viewer';
	const accessibleProjectIds = isOrgWideRole ? null : await getAccessibleProjectIds(userId, orgId);

	const reports = await listReports({
		organizationId: orgId,
		tab: tabParam,
		userId,
		accessibleProjectIds,
	});

	return json({ reports });
};

export const POST: RequestHandler = async ({ params, request, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId } = params;
	if (!orgId) {
		throw error(400, 'Missing organizationId');
	}

	const userId = session.user.id;
	// §9 Q8: any org member, including viewer, may create a report.
	await requireReportAccess(userId, orgId, 'create');

	const body = await request.json().catch(() => ({}));
	const { title, bodyMd, severity, projectId } = body;

	if (typeof title !== 'string' || title.trim().length === 0) {
		throw error(400, 'title is required');
	}
	if (typeof bodyMd !== 'string' || bodyMd.trim().length === 0) {
		throw error(400, 'bodyMd is required');
	}
	if (!isValidSeverity(severity)) {
		throw error(400, `severity must be one of: ${VALID_SEVERITIES.join(', ')}`);
	}

	let resolvedProjectId: string | null = null;
	if (projectId !== undefined && projectId !== null && projectId !== '') {
		if (typeof projectId !== 'string') {
			throw error(400, 'projectId must be a string');
		}

		// The picker is optional (§2/§12), but a supplied projectId must actually belong to this
		// org — otherwise a caller could attach a report to another tenant's project (B7).
		const projectRows = await db
			.select({ id: projects.id, organizationId: projects.organizationId })
			.from(projects)
			.where(eq(projects.id, projectId));

		if (projectRows.length === 0 || projectRows[0].organizationId !== orgId) {
			throw error(400, 'projectId does not belong to this organization');
		}

		resolvedProjectId = projectId;
	}

	const { issue, report } = await createManualIssue({
		organizationId: orgId,
		projectId: resolvedProjectId,
		reporterId: userId,
		title,
		bodyMd,
		severity,
	});

	return json({ issue, report }, { status: 201 });
};
