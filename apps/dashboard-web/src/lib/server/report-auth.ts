import crypto from 'crypto';
import { and, eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { projectApiKeys, projects } from '$lib/db/schema';
import { checkRateLimitWithLimit } from '$lib/rate-limit';

/**
 * sentinel-user-report-issue contract §2 (auth) and OQ-1 variant (b): the `report`-scope
 * `X-API-Key` credential for the user-report intake endpoints (§4.1 `POST /api/report`, §4.5
 * `POST /api/report/attachments`).
 *
 * Deliberately a THIRD authenticator rather than a widening of `agent-auth.ts`. That module is
 * `Authorization: Bearer` + `scope='agent'`, and it collapses every failure to 401 on purpose;
 * this one reads `X-API-Key` (the header the ingestor already reads, and the one the contract
 * makes normative for intake under both OQ-1 variants) and must distinguish 401 from 403
 * `forbidden_scope` because §7's error table says so. Widening `agent-auth.ts` would have meant
 * one function with two header shapes and two error policies.
 *
 * The allowlist is `{report, admin}` and nothing else -- the dashboard-side half of the same
 * fail-closed, per-route rule `apps/ingestor-go/auth/apikey.go`'s `ScopeSet` enforces for
 * `/ingest`. An `ingest`-scoped key reaching an intake route is a 403 here exactly as a `report`
 * key reaching `/ingest` is a 403 there.
 *
 * B7: the organization and the project come from the KEY ROW, never from the request body or the
 * multipart form. §4.5 states it directly ("No `organizationId` part"), and it is the rule the
 * ingestor's `ResolveProjectInOrg` exists to enforce after VERIFIED_STATE.md S6.
 */

/** Scopes admitted by the intake routes. Mirrors `auth.ScopesReport` in the ingestor. */
export const REPORT_SCOPES: readonly string[] = ['report', 'admin'];

/** §7's error envelope. The intake endpoints return no bare string and no HTML error page. */
export type ReportErrorCode =
	| 'invalid_request'
	| 'attachment_not_found'
	| 'unauthenticated'
	| 'forbidden_scope'
	| 'project_not_found'
	| 'idempotency_conflict'
	| 'payload_too_large'
	| 'unsupported_media_type'
	| 'rate_limited'
	| 'internal';

/**
 * Builds a §7 response. Returned as a raw `Response` rather than SvelteKit's `error()` because
 * `error()` produces `{"message": "..."}` -- the envelope this contract explicitly says the intake
 * endpoints do NOT use -- and because it cannot attach the `Retry-After` header a 429 needs.
 */
export function reportErrorResponse(
	status: number,
	code: ReportErrorCode,
	message: string,
	details: Record<string, unknown> = {},
	headers: Record<string, string> = {}
): Response {
	return new Response(JSON.stringify({ error: { code, message, details } }), {
		status,
		headers: { 'Content-Type': 'application/json', ...headers },
	});
}

/**
 * Maps a non-§7 failure thrown by shared code (SvelteKit `error()` from `upload-core.ts`, or an
 * unexpected throw) onto §7's envelope, so an intake route cannot leak `{"message": ...}`.
 *
 * The status is preserved rather than remapped: 411 and 503 are not in §7's table but they are
 * true, and §7 constrains the BODY shape of every non-2xx, not the set of statuses. Anything with
 * no listed code becomes `internal`.
 */
export function toReportError(err: unknown): Response {
	if (err instanceof Response) return err;

	const status = typeof (err as { status?: unknown })?.status === 'number' ? (err as { status: number }).status : 500;
	const message =
		(err as { body?: { message?: string } })?.body?.message ??
		(status === 500 ? 'Internal error' : 'Request rejected');

	const code: ReportErrorCode =
		status === 413
			? 'payload_too_large'
			: status === 415
				? 'unsupported_media_type'
				: status === 429
					? 'rate_limited'
					: status >= 400 && status < 500
						? 'invalid_request'
						: 'internal';

	return reportErrorResponse(status, code, message);
}

export interface ReportAuthContext {
	/** `project_api_keys.id` -- the rate-limit and idempotency key space, never the raw secret. */
	keyId: string;
	/** Always from the key row (B7). */
	organizationId: string;
	/**
	 * The project this request writes into. Fixed by a project-scoped key; for an org-wide key it
	 * is resolved from `X-Project-Key` WITHIN the key's organization (§2), and a name outside that
	 * organization is `403 project_not_found`, deliberately indistinguishable from "no such
	 * project" so the endpoint cannot enumerate other tenants.
	 */
	projectId: string;
	projectName: string;
	/** The key's own scope, echoed into §7's `forbidden_scope` details. */
	scope: string;
	rateLimitRpm: number;
	/** True when the key names no project of its own and `X-Project-Key` decided it. */
	orgWide: boolean;
}

/** Hashed exactly like `apikeys.ts`'s `createApiKey` and the ingestor: sha256 hex of the secret. */
function hashKey(secretToken: string): string {
	return crypto.createHash('sha256').update(secretToken).digest('hex');
}

/**
 * Authenticates an `X-API-Key` request for the intake routes.
 *
 * Throws a `Response` (never SvelteKit's `error()`) so every failure carries §7's envelope.
 * Order is deliberate: identity first (401), then scope (403), then the key's own
 * `rate_limit_rpm` (429). An unknown key must not be able to burn a real key's limit, and a 401
 * must not be shadowed by a coincidental 429.
 */
export async function authenticateReportRequest(request: Request): Promise<ReportAuthContext> {
	const rawKey = request.headers.get('x-api-key')?.trim();
	if (!rawKey) {
		throw reportErrorResponse(401, 'unauthenticated', 'Missing X-API-Key header.');
	}

	const keyHash = hashKey(rawKey);

	const [keyRow] = await db
		.select({
			id: projectApiKeys.id,
			organizationId: projectApiKeys.organizationId,
			projectId: projectApiKeys.projectId,
			scope: projectApiKeys.scope,
			status: projectApiKeys.status,
			expiresAt: projectApiKeys.expiresAt,
			revokedAt: projectApiKeys.revokedAt,
			rateLimitRpm: projectApiKeys.rateLimitRpm,
		})
		.from(projectApiKeys)
		.where(eq(projectApiKeys.keyHash, keyHash));

	// Unknown, revoked, disabled and expired all collapse to the same 401: which one it was is not
	// the caller's business, and telling them apart is a key-probing oracle.
	if (!keyRow || keyRow.status !== 'active' || keyRow.revokedAt) {
		throw reportErrorResponse(401, 'unauthenticated', 'Unknown or inactive API key.');
	}
	if (keyRow.expiresAt && keyRow.expiresAt.getTime() <= Date.now()) {
		throw reportErrorResponse(401, 'unauthenticated', 'Unknown or inactive API key.');
	}

	// §2: `report` or `admin`, and nothing else. `ingest`, `read` and `agent` keys are 403 here --
	// the mirror image of the ingestor refusing a `report` key on /ingest.
	if (!REPORT_SCOPES.includes(keyRow.scope)) {
		throw reportErrorResponse(
			403,
			'forbidden_scope',
			"This API key's scope may not submit user reports.",
			{ scope: keyRow.scope }
		);
	}

	// Resolve the project. A project-scoped key fixes its own; an org-wide key names one, and the
	// name is resolved only within the key's organization.
	let projectId: string;
	let projectName: string;
	const orgWide = keyRow.projectId === null;

	if (!orgWide) {
		const [projectRow] = await db
			.select({ id: projects.id, name: projects.name })
			.from(projects)
			.where(eq(projects.id, keyRow.projectId as string));
		if (!projectRow) {
			throw reportErrorResponse(403, 'project_not_found', 'Project not found in this organization.');
		}
		projectId = projectRow.id;
		projectName = projectRow.name;
	} else {
		const requested = request.headers.get('x-project-key')?.trim();
		if (!requested) {
			// An external reporter is identified per PROJECT (§4.6's UNIQUE (project_id,
			// external_id)), so an org-wide key that names no project has nowhere to write.
			throw reportErrorResponse(400, 'invalid_request', 'X-Project-Key is required for an organization-wide key.', {
				field: 'X-Project-Key',
			});
		}
		const [projectRow] = await db
			.select({ id: projects.id, name: projects.name })
			.from(projects)
			.where(and(eq(projects.name, requested), eq(projects.organizationId, keyRow.organizationId)));
		if (!projectRow) {
			throw reportErrorResponse(403, 'project_not_found', 'Project not found in this organization.');
		}
		projectId = projectRow.id;
		projectName = projectRow.name;
	}

	// The key's own rate_limit_rpm, checked only after the credential is known good (SR-1: the
	// shipped in-process limiter, per replica; §4.5's Postgres byte budget is the durable ceiling).
	const rateLimit = checkRateLimitWithLimit(`report-key:${keyRow.id}`, keyRow.rateLimitRpm);
	if (!rateLimit.allowed) {
		throw reportErrorResponse(429, 'rate_limited', 'Rate limit exceeded.', {}, {
			'Retry-After': String(rateLimit.retryAfter ?? 60),
		});
	}

	return {
		keyId: keyRow.id,
		organizationId: keyRow.organizationId,
		projectId,
		projectName,
		scope: keyRow.scope,
		rateLimitRpm: keyRow.rateLimitRpm,
		orgWide,
	};
}
