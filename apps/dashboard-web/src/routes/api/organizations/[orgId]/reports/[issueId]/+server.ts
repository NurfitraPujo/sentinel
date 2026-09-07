import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import {
	getReportDetail,
	updateManualIssueReport,
	deleteManualIssue,
	type ReportSeverity,
} from '$lib/db/queries/reports';
import { getReporterId, getOrgRole, requireReportAccess } from '$lib/server/report-access';
import { bestEffortDeleteObjects } from '$lib/server/retention';
import { listIssueAttachments, resolveReportEnvironment } from '$lib/db/queries/reports';
import { readTriage, type TriageResult } from '$lib/server/triage';
import { getLatestEscalation } from '$lib/server/escalation';
import { log } from '$lib/server/observability/log';

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

/**
 * sentinel-user-report-issue §4.2 — report detail, plus triage, reporter and attachments.
 *
 * **The correction §4.2 is built around is in `getReportDetail`, not here**: that query reached a
 * report through `.innerJoin(manual_issue_reports, …)`, so it returned `null` for every report
 * this feature creates and this route's `loadReportOrNotFound` guard 404'd. With the join fixed,
 * an externally-sourced report is returned at all — which is the behavioural half of §4.2 that is
 * not additive and is the thing to check first when reviewing this file.
 *
 * `triage` has three distinguishable states and a client must render them differently (§4.2):
 *   - `null`      — nothing was looked at. An org-member report (no `context`, so no window and no
 *                   release to match on), a report created before this feature shipped, or a run
 *                   that failed.
 *   - `{candidates: []}` — it was looked at and nothing matched.
 *   - a populated array, bounded at `max_candidates` = 5.
 *
 * Triage is recomputed here rather than read from a stored blob, and it **writes nothing**: a GET
 * must not mutate. `candidates[].linked` is read back from the `issue_relations` rows that already
 * exist, so it reports what the database holds — which is what the contract says the field means.
 * The links themselves are written once, by intake (`POST /api/report`).
 *
 * A triage failure is caught and degrades to `triage: null`. The report is the thing the reporter
 * gave us; a matcher that cannot run is not a reason to refuse to show it.
 */
export const GET: RequestHandler = async ({ params, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const { orgId, issueId } = params;
	if (!orgId || !issueId) {
		throw error(400, 'Missing organizationId or issueId');
	}

	// §4.2: unchanged auth — session, then `requireReportAccess(userId, orgId, 'read')`, then the
	// `loadReportOrNotFound` guard for the org mismatch (404, never a leak of another tenant's id).
	await requireReportAccess(session.user.id, orgId, 'read');
	const detail = await loadReportOrNotFound(orgId, issueId);

	const attachments = await listIssueAttachments(issueId);

	return json({
		issue: detail.issue,
		report: detail.report,
		projectId: detail.projectId,
		projectName: detail.projectName,
		projectIsInbox: detail.projectIsInbox,
		organizationId: detail.organizationId,
		reporterName: detail.reporterName,
		reporterEmail: detail.reporterEmail,
		commentCount: detail.commentCount,
		// §4.2's added fields.
		sourceChannel: detail.issue.sourceChannel,
		reporter: {
			kind: detail.reporterKind,
			displayName: detail.reporterDisplayName,
			email: detail.reporterEmailAddress,
			externalId: detail.reporterExternalId,
			firstSeenAt: detail.reporterFirstSeenAt,
		},
		context: detail.context,
		attachments: attachments.map((attachment) => ({
			id: attachment.id,
			filename: attachment.filename,
			contentType: attachment.contentType,
			sizeBytes: attachment.sizeBytes,
		})),
		triage: await loadTriage(detail),
		// §4.4's object, or `null` when escalation has not run or the organization has no active
		// schedule. The LATEST hop is the one §4.2 renders: `hop` is "incremented by each timer
		// reassignment", so an earlier row is a superseded state, not the current one.
		escalation: await loadEscalation(detail.issue.id),
	});
};

/**
 * §4.2's `escalation`, or `null`.
 *
 * Never allowed to fail the read: a report detail page that 500s because the escalation lookup
 * threw would be strictly worse than one that renders without the strip. `null` here means the
 * same thing §4.2 says it means — escalation has not run, or the organization has no active
 * schedule — and a lookup failure is logged rather than silently indistinguishable from it.
 */
async function loadEscalation(issueId: string) {
	try {
		const latest = await getLatestEscalation(issueId);
		if (!latest) return null;
		return {
			scheduleId: latest.scheduleId,
			assigneeId: latest.assigneeId,
			escalatedAt: latest.escalatedAt,
			ackDeadlineAt: latest.ackDeadlineAt,
			acknowledgedAt: latest.acknowledgedAt,
			hop: latest.hop,
			exhausted: latest.exhausted,
		};
	} catch (err) {
		log.error('report_detail.escalation_lookup_failed', { issueId, error: err });
		return null;
	}
}

/**
 * §4.2's `triage`, or `null`.
 *
 * `null` is returned for a report with no `context` at all — an org-member report has none, and
 * neither has any report created before this feature shipped — and for a run that threw. The
 * second case is logged rather than swallowed silently: "the matcher is broken" and "the matcher
 * found nothing" are the two states §4.2 insists a client can tell apart, and an operator has to
 * be able to tell them apart too.
 */
async function loadTriage(
	detail: Awaited<ReturnType<typeof loadReportOrNotFound>>
): Promise<TriageResult | null> {
	const context = detail.context;
	if (!context) {
		return null;
	}

	const occurredAtRaw = context.occurred_at;
	const occurredAt =
		typeof occurredAtRaw === 'string' && !Number.isNaN(new Date(occurredAtRaw).getTime())
			? new Date(occurredAtRaw)
			: (detail.externalReport?.createdAt ?? detail.issue.firstSeen ?? new Date());

	try {
		return await readTriage({
			issueId: detail.issue.id,
			projectId: detail.issue.projectId,
			occurredAt,
			context: {
				release: typeof context.release === 'string' ? context.release : null,
				// sentinel-project-environments decision 53, and this is the SECOND of the two
				// production consumers of findTriageCandidates — the one that appeared in no
				// document until round seven. It must hand over the RESOLVED environment, the same
				// value §4.8a wrote to `issues.environment`, never the raw nullable assertion:
				// decision 50 made this a hard `WHERE` predicate, so a `null` matches no row and
				// this detail panel would return zero candidates for every report that asserted
				// nothing — the majority path, filed as `production`.
				environment: resolveReportEnvironment(context.environment),
				traceId: typeof context.trace_id === 'string' ? context.trace_id : null,
				sessionId: typeof context.session_id === 'string' ? context.session_id : null,
			},
		});
	} catch (err) {
		log.error('report.triage_read_failed', { issueId: detail.issue.id, error: err });
		return null;
	}
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

	// §4.2 / SR-6: `getReporterId` is a tagged union now. An external report has no org-member
	// author, so `kind === 'org_member'` is what "there is an author to compare against" means --
	// comparing an `external_reporters.id` to a session user id would be a question with no meaning.
	const reporter = await getReporterId(issueId);
	if (reporter?.kind !== 'org_member' || reporter.userId !== userId) {
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

	const reporter = await getReporterId(issueId);
	const role = await getOrgRole(userId, orgId);
	const isModerator = role === 'owner' || role === 'admin';
	const isAuthorAndUnresolved =
		reporter?.kind === 'org_member' && reporter.userId === userId && detail.issue.status !== 'resolved';

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
