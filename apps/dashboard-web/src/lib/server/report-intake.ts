import { reportErrorResponse } from '$lib/server/report-auth';
import { isUuid } from '$lib/uuid';

/**
 * sentinel-user-report-issue contract §4.1 — the request half of `POST /api/report`: the 1 MiB
 * body cap, the 16 KiB `context` cap, and every field bound in §4.1's request table.
 *
 * Kept out of `+server.ts` so the bounds are testable without a Request/Response round trip and so
 * the route reads as the sequence of decisions §7 orders (401 → 403 scope → 400 → 403 project →
 * 409 → 201) rather than as 200 lines of `typeof`.
 *
 * Two conventions the whole file follows, both from §3:
 *
 * - **An omitted key and an explicit `null` are the same thing: absent** — and where a field has a
 *   default, absent means that default. There is no PATCH here, so the distinction carries no
 *   meaning and is deliberately not preserved.
 * - **Unknown top-level keys are IGNORED, not rejected**, so an SDK newer than the server does not
 *   fail closed on a field the server has not learned. Unknown keys inside `context` are ignored
 *   *and stored* — `external_issue_reports.context` takes the object verbatim.
 *
 * Every failure here is thrown as a `Response` carrying §7's envelope, never SvelteKit's
 * `error()`, which produces `{"message": ...}` — the shape the intake endpoints explicitly do not
 * use. `details.field` names the offending path, per §7's "`invalid_request` →
 * `{"field": "<json pointer-ish path>"}`".
 */

/** §3's size cap: the same 1 MiB `maxEventBodyBytes` (`apps/ingestor-go/main.go:38`) `/ingest` applies. */
export const MAX_REPORT_BODY_BYTES = 1 << 20;

/** §4.1: `context` capped at 16 KiB SERIALISED — measured on the JSON, not on the field count. */
export const MAX_CONTEXT_BYTES = 16 * 1024;

/** §4.1 field bounds, after trim. */
export const MAX_TEXT_CHARS = 8000;
export const MAX_TITLE_CHARS = 200;
export const MAX_IDEMPOTENCY_KEY_CHARS = 128;
export const MAX_EXTERNAL_ID_CHARS = 255;
export const MAX_DISPLAY_NAME_CHARS = 255;
export const MAX_EMAIL_CHARS = 320;
export const MAX_TRACE_OR_SESSION_CHARS = 128;
export const MAX_ATTACHMENTS = 5;

/** §4.1: `occurred_at` more than 24 h in the past, or any time in the future, is `400`. */
export const MAX_OCCURRED_AT_AGE_MS = 24 * 60 * 60 * 1000;

/** §4.1's `severity` enum. Absent OR `null` → `medium`, matching the column default. */
export const REPORT_SEVERITIES = ['low', 'medium', 'high', 'critical'] as const;
export type ReportSeverity = (typeof REPORT_SEVERITIES)[number];
export const DEFAULT_SEVERITY: ReportSeverity = 'medium';

/** Canonical UUID v4-shaped id, as §4.1's `attachments` array declares. */
const encoder = new TextEncoder();

/** §4.1's request body, after validation. Every optional field is normalised to `null`. */
export interface ParsedReportRequest {
	projectKey: string | null;
	text: string;
	title: string | null;
	severity: ReportSeverity;
	idempotencyKey: string | null;
	reporter: {
		externalId: string;
		displayName: string | null;
		email: string | null;
	};
	/** Stored verbatim in `external_issue_reports.context`; unknown keys kept, not rejected. */
	context: Record<string, unknown>;
	/** §4.5 draft attachment ids, at most 5. */
	attachments: string[];
	/** `context.page`, already narrowed to a usable string — §4.5a's title prefix reads this. */
	page: string | null;
	/** The exact bytes the caller sent, for the idempotency digest (§4.1's "different body"). */
	raw: unknown;
}

function invalid(message: string, field: string): Response {
	return reportErrorResponse(400, 'invalid_request', message, { field });
}

/**
 * Reads the request body under §3's 1 MiB cap and §4.1's `Content-Type` rule.
 *
 * The cap is enforced HERE and not left to the platform: adapter-node's `BODY_SIZE_LIMIT` is set
 * to 27M for §4.5's 25 MB uploads (see `docker-compose.yml`), which is 27× this endpoint's cap, so
 * without this check a 20 MB report body would be buffered and parsed before anything objected.
 * `Content-Length` is checked first because it lets an oversized body be refused without reading
 * it, and the decoded byte length is checked after because `Content-Length` is caller-supplied and
 * a chunked request has none.
 */
export async function readReportBody(request: Request): Promise<{ raw: unknown; text: string }> {
	const contentType = request.headers.get('content-type') ?? '';
	// `application/json; charset=utf-8` is the same media type; only the type/subtype is matched.
	if (!/^application\/json\s*(;|$)/i.test(contentType.trim())) {
		throw invalid('Content-Type must be application/json.', 'Content-Type');
	}

	const declared = Number(request.headers.get('content-length'));
	if (Number.isFinite(declared) && declared > MAX_REPORT_BODY_BYTES) {
		throw reportErrorResponse(413, 'payload_too_large', 'Request body exceeds 1 MiB.');
	}

	let body: string;
	try {
		body = await request.text();
	} catch (err) {
		// adapter-node throws a 413 here when BODY_SIZE_LIMIT is hit; preserving the status keeps a
		// size rejection from being reported as a malformed one.
		const status = (err as { status?: unknown })?.status;
		if (status === 413) {
			throw reportErrorResponse(413, 'payload_too_large', 'Request body exceeds 1 MiB.');
		}
		throw invalid('Request body could not be read.', 'body');
	}

	if (encoder.encode(body).byteLength > MAX_REPORT_BODY_BYTES) {
		throw reportErrorResponse(413, 'payload_too_large', 'Request body exceeds 1 MiB.');
	}

	let raw: unknown;
	try {
		raw = JSON.parse(body);
	} catch {
		throw invalid('Request body is not valid JSON.', 'body');
	}

	if (raw === null || typeof raw !== 'object' || Array.isArray(raw)) {
		throw invalid('Request body must be a JSON object.', 'body');
	}

	return { raw, text: body };
}

/**
 * Validates §4.1's request table. Throws a §7 `Response` on the first failure; the `field` in
 * `details` is the path of that first failure, so a caller fixing one problem at a time converges.
 */
export function parseReportRequest(raw: unknown, now: Date = new Date()): ParsedReportRequest {
	const body = raw as Record<string, unknown>;

	const projectKey = optionalString(body.project_key, 'project_key');

	// §4.1: the design's one required field. "1-8000 chars after trim; empty after trim → 400".
	const textValue = body.text;
	if (typeof textValue !== 'string') {
		throw invalid('text is required and must be a string.', 'text');
	}
	const text = textValue.trim();
	if (text.length === 0) {
		throw invalid('text must not be empty.', 'text');
	}
	if (text.length > MAX_TEXT_CHARS) {
		throw invalid(`text must be at most ${MAX_TEXT_CHARS} characters.`, 'text');
	}

	// Absent or null → the server derives one by §4.5a. A supplied one is bounded at 200 because
	// that is what `issues.message` inherits.
	const title = optionalString(body.title, 'title');
	if (title !== null && title.length > MAX_TITLE_CHARS) {
		throw invalid(`title must be at most ${MAX_TITLE_CHARS} characters.`, 'title');
	}

	const severity = parseSeverity(body.severity);

	const idempotencyKey = optionalString(body.idempotency_key, 'idempotency_key');
	if (idempotencyKey !== null && idempotencyKey.length > MAX_IDEMPOTENCY_KEY_CHARS) {
		throw invalid(
			`idempotency_key must be at most ${MAX_IDEMPOTENCY_KEY_CHARS} characters.`,
			'idempotency_key'
		);
	}

	const reporter = parseReporter(body.reporter);
	const context = parseContext(body.context, now);
	const attachments = parseAttachments(body.attachments);

	const page = typeof context.page === 'string' ? context.page : null;

	return {
		projectKey,
		text,
		title,
		severity,
		idempotencyKey,
		reporter,
		context,
		attachments,
		page,
		raw,
	};
}

function parseSeverity(value: unknown): ReportSeverity {
	// Nullable on purpose (§4.1): §3 says absent and null are the same thing on intake, and the
	// column default is `medium`.
	if (value === undefined || value === null) return DEFAULT_SEVERITY;
	if (typeof value !== 'string' || !(REPORT_SEVERITIES as readonly string[]).includes(value)) {
		throw invalid(`severity must be one of ${REPORT_SEVERITIES.join(', ')}.`, 'severity');
	}
	return value as ReportSeverity;
}

function parseReporter(value: unknown): ParsedReportRequest['reporter'] {
	// Required and non-nullable — §4.1 marks it so, and §4.6's `external_issue_reports` carries a
	// NOT NULL FK to the reporter row.
	if (value === null || typeof value !== 'object' || Array.isArray(value)) {
		throw invalid('reporter is required and must be an object.', 'reporter');
	}
	const reporter = value as Record<string, unknown>;

	const externalIdValue = reporter.external_id;
	if (typeof externalIdValue !== 'string' || externalIdValue.trim().length === 0) {
		throw invalid('reporter.external_id is required.', 'reporter.external_id');
	}
	const externalId = externalIdValue.trim();
	if (externalId.length > MAX_EXTERNAL_ID_CHARS) {
		throw invalid(
			`reporter.external_id must be at most ${MAX_EXTERNAL_ID_CHARS} characters.`,
			'reporter.external_id'
		);
	}

	const displayName = optionalString(reporter.display_name, 'reporter.display_name');
	if (displayName !== null && displayName.length > MAX_DISPLAY_NAME_CHARS) {
		throw invalid(
			`reporter.display_name must be at most ${MAX_DISPLAY_NAME_CHARS} characters.`,
			'reporter.display_name'
		);
	}

	// Stored, and NEVER used to contact the reporter (OQ-6; §0 Q1 — email is never a reply
	// channel). Bounded, and deliberately not format-validated: this is attacker-controlled data,
	// not identity, and rejecting an odd-looking address would lose a report for nothing.
	const email = optionalString(reporter.email, 'reporter.email');
	if (email !== null && email.length > MAX_EMAIL_CHARS) {
		throw invalid(`reporter.email must be at most ${MAX_EMAIL_CHARS} characters.`, 'reporter.email');
	}

	return { externalId, displayName, email };
}

function parseContext(value: unknown, now: Date): Record<string, unknown> {
	if (value === undefined || value === null) return {};
	if (typeof value !== 'object' || Array.isArray(value)) {
		throw invalid('context must be an object.', 'context');
	}
	const context = value as Record<string, unknown>;

	// §4.1: "Capped at 16 KiB serialised → over that is 400 invalid_request." Measured on the
	// re-serialised object rather than on the caller's bytes, so pretty-printing a small object
	// cannot be rejected for its whitespace.
	if (encoder.encode(JSON.stringify(context)).byteLength > MAX_CONTEXT_BYTES) {
		throw invalid('context exceeds 16 KiB when serialised.', 'context');
	}

	// The named keys are type-checked. Every OTHER key is kept untouched and stored verbatim —
	// §3's "unknown keys inside `context` are also ignored, and are stored".
	for (const key of ['page', 'release', 'brand', 'environment'] as const) {
		optionalString(context[key], `context.${key}`);
	}

	for (const key of ['trace_id', 'session_id'] as const) {
		const parsed = optionalString(context[key], `context.${key}`);
		// Accepted, stored, and INERT in v1 (OQ-4) — no v1 client sends one and no score reads one.
		if (parsed !== null && parsed.length > MAX_TRACE_OR_SESSION_CHARS) {
			throw invalid(
				`context.${key} must be at most ${MAX_TRACE_OR_SESSION_CHARS} characters.`,
				`context.${key}`
			);
		}
	}

	parseOccurredAt(context.occurred_at, now);

	return context;
}

/**
 * §4.1: "More than 24 h in the past or any time in the future → 400". Enforced literally, with no
 * skew allowance — the contract's wording is "any time in the future", and inventing a tolerance
 * would be a shape this file decided rather than one it was given.
 *
 * Returns the parsed instant, or null when absent (the server then uses receipt time).
 */
export function parseOccurredAt(value: unknown, now: Date = new Date()): Date | null {
	if (value === undefined || value === null) return null;
	if (typeof value !== 'string') {
		throw invalid('context.occurred_at must be an RFC 3339 timestamp.', 'context.occurred_at');
	}
	const parsed = new Date(value);
	if (Number.isNaN(parsed.getTime())) {
		throw invalid('context.occurred_at must be an RFC 3339 timestamp.', 'context.occurred_at');
	}
	if (parsed.getTime() > now.getTime()) {
		throw invalid('context.occurred_at must not be in the future.', 'context.occurred_at');
	}
	if (now.getTime() - parsed.getTime() > MAX_OCCURRED_AT_AGE_MS) {
		throw invalid('context.occurred_at must not be more than 24 hours in the past.', 'context.occurred_at');
	}
	return parsed;
}

function parseAttachments(value: unknown): string[] {
	if (value === undefined || value === null) return [];
	if (!Array.isArray(value)) {
		throw invalid('attachments must be an array of attachment ids.', 'attachments');
	}
	if (value.length > MAX_ATTACHMENTS) {
		throw invalid(`attachments must contain at most ${MAX_ATTACHMENTS} ids.`, 'attachments');
	}
	const ids: string[] = [];
	for (const entry of value) {
		// A malformed id is `invalid_request`, not `attachment_not_found`: the latter is reserved for
		// an id that is well-formed but unknown/claimed/another tenant's, and it must stay a single
		// undifferentiated answer so the endpoint cannot probe another tenant's attachment ids.
		if (typeof entry !== 'string' || !isUuid(entry)) {
			throw invalid('attachments must contain attachment ids.', 'attachments');
		}
		ids.push(entry);
	}
	return ids;
}

/**
 * §3: absent and `null` are the same thing. A present non-string is a type error; a string that is
 * empty after trim is treated as absent, so `""` cannot become a stored empty display name.
 */
function optionalString(value: unknown, field: string): string | null {
	if (value === undefined || value === null) return null;
	if (typeof value !== 'string') {
		throw invalid(`${field} must be a string.`, field);
	}
	const trimmed = value.trim();
	return trimmed.length === 0 ? null : trimmed;
}
