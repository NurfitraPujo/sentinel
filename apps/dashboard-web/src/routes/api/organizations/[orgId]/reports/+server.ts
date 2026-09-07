import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { eq } from 'drizzle-orm';
import {
	createManualIssue,
	listReports,
	ENV_NAME_RE,
	REPORT_SOURCE_CHANNELS,
	type ReportTab,
	type ReportSourceChannel,
} from '$lib/db/queries/reports';
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

// sentinel-user-report-issue OQ-8: `?source=` narrows the list to one `issues.source_channel`.
// Absent means every source, so a caller that sends nothing sees exactly what it saw before.
function isValidSource(value: unknown): value is ReportSourceChannel {
	return typeof value === 'string' && (REPORT_SOURCE_CHANNELS as readonly string[]).includes(value);
}

// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §2, §9, §10). Manual-validation style
// (allowlists + throw error(status)) matching the invitations endpoint — no schema library.

// sentinel-project-environments §4.8a: the header is the alternate channel for a non-browser
// client. The header NAME is case-insensitive, as HTTP requires; the VALUE is case-sensitive like
// every other environment name (§3).
const ENVIRONMENT_HEADER = 'x-sentinel-environment';

/**
 * §4.8a's precedence chain: body field, then header, then nothing.
 *
 * "Present" means **a non-empty string and nothing else**. An explicit `"environment": null`, an
 * explicit `""` and an omitted key are all NOT assertions — they fall through to the header and
 * then to `production`. This is `report-intake.ts`'s own `optionalString` convention on the sibling
 * route, and it is spelled out because a chain that treated `null` as an assertion would file a
 * report under the literal string `"null"`, or 23502 on it.
 *
 * Body over header, and when both are present and differ the header is simply ignored — NOT a 400:
 * a proxy that adds a default header must not break a client that was explicit.
 *
 * A present value that fails §3's pattern is a 400 (§7). This field is NEW on this route, so that
 * rejection breaks no existing client — which is exactly the difference from `POST /api/report`,
 * where `context.environment` has accepted any string since it shipped and still does.
 *
 * It is NOT checked against `project_environments`: under decisions 8 and 9 an environment may
 * legitimately exist in the data and not in the registry, so an override naming an unregistered
 * environment is stored, not rejected.
 */
function resolveAssertedEnvironment(bodyValue: unknown, request: Request): string | null {
	if (typeof bodyValue === 'string' && bodyValue.length > 0) {
		if (!ENV_NAME_RE.test(bodyValue)) {
			throw error(400, `environment must match ${ENV_NAME_RE.source}`);
		}
		return bodyValue;
	}
	if (bodyValue !== undefined && bodyValue !== null && typeof bodyValue !== 'string') {
		throw error(400, 'environment must be a string');
	}

	const headerValue = request.headers.get(ENVIRONMENT_HEADER);
	if (headerValue !== null && headerValue.length > 0) {
		if (!ENV_NAME_RE.test(headerValue)) {
			throw error(400, `environment must match ${ENV_NAME_RE.source}`);
		}
		return headerValue;
	}

	// Neither channel asserted anything. createManualIssue resolves this to `production`.
	return null;
}

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

	const sourceParam = url.searchParams.get('source');
	if (sourceParam !== null && !isValidSource(sourceParam)) {
		throw error(400, `source must be one of: ${REPORT_SOURCE_CHANNELS.join(', ')}`);
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
		source: sourceParam ?? undefined,
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
	const { title, bodyMd, severity, projectId, attachmentIds, environment } = body;

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

	// §4 linking: an optional list of DRAFT attachment ids (from POST /api/uploads) to claim onto
	// the report, inside the SAME transaction as creation (createManualIssue does the claiming).
	let resolvedAttachmentIds: string[] | undefined;
	if (attachmentIds !== undefined) {
		if (
			!Array.isArray(attachmentIds) ||
			!attachmentIds.every((value: unknown) => typeof value === 'string')
		) {
			throw error(400, 'attachmentIds must be an array of strings');
		}
		resolvedAttachmentIds = attachmentIds;
	}

	const { issue, report } = await createManualIssue({
		organizationId: orgId,
		projectId: resolvedProjectId,
		reporterId: userId,
		title,
		bodyMd,
		severity,
		environment: resolveAssertedEnvironment(environment, request),
		attachmentIds: resolvedAttachmentIds,
	});

	return json({ issue, report }, { status: 201 });
};
