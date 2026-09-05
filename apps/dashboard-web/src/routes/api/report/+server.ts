import type { RequestHandler } from './$types';
import {
	authenticateReportKey,
	resolveReportProject,
	reportErrorResponse,
	toReportError,
	type ReportKeyCredential,
} from '$lib/server/report-auth';
import {
	readReportBody,
	parseReportRequest,
	parseOccurredAt,
	type ParsedReportRequest,
} from '$lib/server/report-intake';
import { deriveReportTitle } from '$lib/server/report-title';
import {
	hashReportRequest,
	findReportIdempotency,
	deleteExpiredReportIdempotency,
	idempotencyCutoff,
	isDuplicateKeyViolation,
} from '$lib/server/report-idempotency';
import { upsertExternalReporter } from '$lib/db/queries/external-reporters';
import { createExternalIssueReport, ReportAttachmentNotFoundError } from '$lib/db/queries/reports';
import { runTriage, type TriageResult } from '$lib/server/triage';
import { escalateReportAtIntake, sendEscalationEmails } from '$lib/server/escalation';
import { recordReportReceived, REPORT_OUTCOME, type ReportOutcome } from '$lib/server/metrics';
import { log } from '$lib/server/observability/log';

/**
 * sentinel-user-report-issue contract §4.1 under OQ-1 variant (b) -- `POST /api/report`: one
 * user-reported issue from an instrumented service, created SYNCHRONOUSLY and answered `201`.
 *
 * The path is `/api/report`, not `/report`: `api` is in `hooks.server.ts`'s `reservedRoutes` and a
 * bare `/report` would be read as an organization slug by the `[orgSlug]` catch-all (§4.1's own
 * note on the prefix under variant (b)).
 *
 * **The order of the checks below is the contract, not a preference.** §7 lists a `401` for an
 * unknown key, a `403` for a scope, a `400` for the body and a `403` for the project, and a caller
 * must be able to tell them apart without the endpoint leaking which tenant owns what:
 *
 *   1. the credential -- 401 `unauthenticated`, 403 `forbidden_scope`, 429 `rate_limited`. An
 *      unknown key never reaches the body, so a probe cannot use body validation as an oracle for
 *      whether a key exists.
 *   2. the body -- `Content-Type`, the 1 MiB cap, JSON, then every §4.1 bound (400 / 413).
 *   3. the project -- `X-Project-Key` or the body's `project_key`, resolved INSIDE the key's
 *      organization only (403 `project_not_found`, deliberately indistinguishable from "no such
 *      project"). It runs after the body because §4.1 lets an org-wide key name its project there.
 *   4. idempotency -- 409 `idempotency_conflict`, or a replay of the first call's `report_id`.
 *   5. creation -- the `issues` row, its `external_issue_reports` companion, the attachment claim
 *      and the idempotency record, in ONE transaction.
 *
 * `manual_issue_reports` is never written here (OQ-6). Triage (§4.3) and escalation (§4.4) are not
 * part of this response; `triage` is `null` until the item that runs it inline lands, which is
 * exactly what §4.1-b's "`null` when triage is deferred" describes.
 */

export const POST: RequestHandler = async ({ request, url }) => {
	// 1. Credential.
	let credential: ReportKeyCredential;
	try {
		credential = await authenticateReportKey(request);
	} catch (err) {
		return finish(toReportError(err));
	}

	// 2. Body.
	let parsed: ParsedReportRequest;
	try {
		const { raw } = await readReportBody(request);
		parsed = parseReportRequest(raw);
	} catch (err) {
		return finish(toReportError(err));
	}

	// 3. Project. `X-Project-Key` wins over the body's `project_key` when both are present: a
	// header is the more specific statement, and §4.1 offers the body field as an "alternative to
	// X-Project-Key", not as an override of it.
	let project: { projectId: string; projectName: string };
	try {
		project = await resolveReportProject(
			credential,
			request.headers.get('x-project-key') ?? parsed.projectKey
		);
	} catch (err) {
		return finish(toReportError(err));
	}

	// 4. Idempotency. The digest covers the body AND the resolved project (see
	// `hashReportRequest`); an absent `idempotency_key` means no dedupe at all, and §4.1 says so:
	// "a retried request creates a second report".
	const requestHash = parsed.idempotencyKey
		? hashReportRequest(parsed.raw, project.projectId)
		: null;

	if (parsed.idempotencyKey && requestHash) {
		try {
			const replay = await checkIdempotency(credential.keyId, parsed.idempotencyKey, requestHash);
			if (replay) return finish(replay.response, replay.outcome);
		} catch (err) {
			log.error('report_intake.idempotency_lookup_failed', {
				organizationId: credential.organizationId,
				keyId: credential.keyId,
				error: err,
			});
			return finish(reportErrorResponse(500, 'internal', 'Internal error.'));
		}
	}

	// The reporter row is an UPSERT on (project_id, external_id) and is deliberately outside the
	// creation transaction: it is identity, not report content, §4.5's upload path creates the same
	// row before any report exists, and a rolled-back report must not delete a reporter another
	// report already references. A reporter row with no report is exactly what a draft upload
	// leaves behind anyway.
	let externalReporterId: string;
	try {
		const reporter = await upsertExternalReporter({
			organizationId: credential.organizationId,
			projectId: project.projectId,
			externalId: parsed.reporter.externalId,
			displayName: parsed.reporter.displayName,
			email: parsed.reporter.email,
		});
		externalReporterId = reporter.id;
	} catch (err) {
		log.error('report_intake.reporter_upsert_failed', {
			organizationId: credential.organizationId,
			projectId: project.projectId,
			error: err,
		});
		return finish(reportErrorResponse(500, 'internal', 'Failed to record the reporter.'));
	}

	// §4.5a: a caller-supplied title wins; otherwise derive one. `issues.message` is NOT NULL and
	// the derivation is total, so this always produces a value.
	const title = parsed.title ?? deriveReportTitle({ text: parsed.text, page: parsed.page });

	// 5. Creation.
	try {
		const { issue } = await createExternalIssueReport({
			organizationId: credential.organizationId,
			projectId: project.projectId,
			externalReporterId,
			title,
			bodyMd: parsed.text,
			severity: parsed.severity,
			context: parsed.context,
			idempotencyKey: parsed.idempotencyKey,
			attachmentIds: parsed.attachments,
			idempotency:
				parsed.idempotencyKey && requestHash
					? { apiKeyId: credential.keyId, idempotencyKey: parsed.idempotencyKey, requestHash }
					: null,
		});

		log.info('report_intake.created', {
			reportId: issue.id,
			organizationId: credential.organizationId,
			projectId: project.projectId,
			externalReporterId,
			severity: parsed.severity,
			attachments: parsed.attachments.length,
			deduplicated: false,
		});

		// §4.3 runs INSIDE this request (OQ-1 variant (b): "§4.2's `triage` may be inline") and
		// AFTER creation has committed, never inside its transaction. That ordering is the whole of
		// §12's "a forced triage failure yields `triage: null` and a created report, never a 500 and
		// never a lost report": the report is already durable when the matcher runs, so the matcher
		// has nothing left it can take away. `triageForReport` swallows its own failure.
		const triage = await triageForReport({
			issueId: issue.id,
			projectId: project.projectId,
			occurredAt: parseOccurredAt(parsed.context.occurred_at) ?? new Date(),
			context: parsed.context,
		});

		// §4.4, and §8's "escalation is never simultaneous with the response": it runs after the
		// report is durable, for the same reason triage does. `escalateReportAtIntake` absorbs its
		// own failure and returns a null escalation, so an organization with no on-call schedule,
		// a rota that resolves to nobody, and a database error during assignment are all the same
		// thing from here — a created report that is simply unassigned, with a metric to say so.
		// Its first observable effect is `escalation` in §4.2; §4.1-b's body does not carry it.
		//
		// The outer `catch` on this block is not redundant with that. The report is COMMITTED by
		// the time this runs, so any throw from here — including one from reading the request's
		// origin — must not fall through to the `catch` below and be reported as a `500`
		// `create_failed`. That would tell a client its report was lost when it was not, and a
		// retry would create a second one.
		try {
			const escalation = await escalateReportAtIntake({
				issueId: issue.id,
				organizationId: credential.organizationId,
			});
			// SR-2's email, post-commit and best-effort, exactly like the notification fan-out's.
			await sendEscalationEmails(escalation.notified, url.origin);
		} catch (err) {
			log.error('report_intake.escalation_failed', { reportId: issue.id, error: err });
		}

		return finish(createdResponse(issue.id, false, triage), REPORT_OUTCOME.Created);
	} catch (err) {
		if (err instanceof ReportAttachmentNotFoundError) {
			// One code for unknown, claimed and another tenant's, so the endpoint cannot probe another
			// tenant's attachment ids (§4.1). The transaction rolled back: nothing was created.
			return finish(
				reportErrorResponse(400, 'attachment_not_found', 'One or more attachments could not be claimed.', {
					field: 'attachments',
				})
			);
		}

		// A concurrent request carrying the same `idempotency_key` committed first. This
		// transaction created nothing; replay the winner's result rather than reporting a 500.
		if (parsed.idempotencyKey && requestHash && isDuplicateKeyViolation(err)) {
			try {
				const replay = await checkIdempotency(credential.keyId, parsed.idempotencyKey, requestHash);
				if (replay) return finish(replay.response, replay.outcome);
			} catch {
				// Fall through to the 500 below -- a lookup that fails here says nothing about the
				// caller's request and must not be reported as a successful replay.
			}
		}

		log.error('report_intake.create_failed', {
			organizationId: credential.organizationId,
			projectId: project.projectId,
			error: err,
		});
		return finish(reportErrorResponse(500, 'internal', 'Failed to create the report.'));
	}
};

/**
 * §4.1's idempotency decision for a stored record, or `null` when the request should proceed.
 *
 * Three outcomes, in the order §4.1 and §8 fix them:
 * - a record older than the 7-day window is deleted and the request proceeds ("a repeat … creates
 *   a second report"). The delete carries its own `created_at < cutoff` predicate, so it cannot
 *   drop a row a concurrent request has just written.
 * - a record whose `request_hash` differs is `409 idempotency_conflict` -- "the same key used with
 *   a DIFFERENT body", and §7 tells that client not to retry with the same key.
 * - a matching record replays the first call's `report_id` with `deduplicated: true`, creating
 *   nothing.
 *
 * A record with a NULL `issue_id` cannot be observed: the row and the issue are written in one
 * transaction, so an uncommitted one is invisible and a committed one always has its id. The guard
 * is kept anyway and treated as "proceed", because the alternative -- replaying `report_id: null`
 * -- would break §4.1-b's non-nullable field.
 */
async function checkIdempotency(
	apiKeyId: string,
	idempotencyKey: string,
	requestHash: string
): Promise<{ response: Response; outcome: ReportOutcome } | null> {
	const existing = await findReportIdempotency(apiKeyId, idempotencyKey);
	if (!existing) return null;

	if (existing.createdAt.getTime() < idempotencyCutoff().getTime()) {
		await deleteExpiredReportIdempotency(apiKeyId, idempotencyKey);
		return null;
	}

	if (existing.requestHash !== requestHash) {
		return {
			response: reportErrorResponse(
				409,
				'idempotency_conflict',
				'This idempotency_key was already used with a different body.'
			),
			outcome: REPORT_OUTCOME.Conflict,
		};
	}

	if (!existing.issueId) return null;

	return {
		response: createdResponse(existing.issueId, true),
		outcome: REPORT_OUTCOME.Deduplicated,
	};
}

/**
 * §4.3, run for a report that has just been created, with its failure absorbed.
 *
 * The contract makes this the difference between a feature that degrades and one that loses data:
 * the report exists and is committed by the time this is called, so a matcher that throws costs a
 * `triage` object and nothing else. `null` here is the same `null` §4.2 defines -- "not looked
 * at" -- and is distinguishable by a client from `{"candidates": []}`, which is "looked, found
 * nothing".
 */
async function triageForReport(input: {
	issueId: string;
	projectId: string;
	occurredAt: Date;
	context: Record<string, unknown>;
}): Promise<TriageResult | null> {
	try {
		return await runTriage({
			issueId: input.issueId,
			projectId: input.projectId,
			occurredAt: input.occurredAt,
			context: {
				release: typeof input.context.release === 'string' ? input.context.release : null,
				environment: typeof input.context.environment === 'string' ? input.context.environment : null,
				// Accepted, stored, and inert in v1 (§4.3, OQ-4).
				traceId: typeof input.context.trace_id === 'string' ? input.context.trace_id : null,
				sessionId: typeof input.context.session_id === 'string' ? input.context.session_id : null,
			},
		});
	} catch (err) {
		log.error('report_intake.triage_failed', { reportId: input.issueId, error: err });
		return null;
	}
}

/**
 * §4.1-b's success body. `issue_id` repeats `report_id` deliberately -- "present so a client need
 * not know they are the same row". `triage` is the §4.2 object inline, or `null` -- for a
 * deduplicated replay (nothing was created, so nothing was triaged in this request) or for a run
 * that failed.
 */
function createdResponse(
	reportId: string,
	deduplicated: boolean,
	triage: TriageResult | null = null
): Response {
	return new Response(
		JSON.stringify({
			report_id: reportId,
			issue_id: reportId,
			status: 'created',
			deduplicated,
			triage,
		}),
		{ status: 201, headers: { 'Content-Type': 'application/json' } }
	);
}

/**
 * Records `sentinel_reports_received_total{outcome}` (§12) and returns the response unchanged.
 *
 * Every exit from this handler goes through here, so the counter cannot drift from what the caller
 * actually received: the outcome is derived from the response's own status unless the caller names
 * one (the two `201` shapes differ only in `deduplicated`, which the status cannot express).
 */
function finish(response: Response, outcome?: ReportOutcome): Response {
	recordReportReceived(outcome ?? outcomeForStatus(response.status));
	return response;
}

function outcomeForStatus(status: number): ReportOutcome {
	if (status === 401) return REPORT_OUTCOME.Unauthorized;
	if (status === 403) return REPORT_OUTCOME.Forbidden;
	if (status === 409) return REPORT_OUTCOME.Conflict;
	if (status === 429) return REPORT_OUTCOME.RateLimited;
	if (status >= 400 && status < 500) return REPORT_OUTCOME.Rejected;
	if (status >= 500) return REPORT_OUTCOME.Error;
	return REPORT_OUTCOME.Created;
}
