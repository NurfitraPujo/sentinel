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
 *
 * sentinel-sourcemaps §2.1 added a second `X-API-Key` route family on this host --
 * `POST /api/sourcemaps`, admitting `{ingest, admin}` -- and this file was generalised to carry
 * BOTH allowlists (`authenticateApiKey` + an `ApiKeyRoutePolicy` per route) rather than gaining a
 * `sourcemap-auth.ts` twin: one identity check, one 401 collapse, one scope-before-limiter order,
 * one envelope, and the allowlist the only thing a route chooses. `resolveReportProject` is shared
 * unchanged (decision 24).
 */

/** Scopes admitted by the intake routes. Mirrors `auth.ScopesReport` in the ingestor. */
export const REPORT_SCOPES: readonly string[] = ['report', 'admin'];

/**
 * sentinel-sourcemaps contract §2.1 (decision D2): scopes admitted by `POST /api/sourcemaps`. The
 * same key that ingests the events is the key that uploads their maps, so this is
 * `apps/ingestor-go/auth/apikey.go: ScopesIngest` -- `{ingest, admin}` -- and the two must stay
 * ONE rule: a `report`, `read` or `agent` key is `403 forbidden_scope` here exactly as it is on
 * `/ingest`. An allowlist per route, never a denylist, never a shared process-wide set, for the
 * fail-closed reason that file's `ScopeSet` comment gives. Held beside `REPORT_SCOPES` rather than
 * in a `sourcemap-auth.ts` twin so there is one authenticator and two allowlists, the shape the
 * ingestor has.
 */
export const SOURCEMAP_SCOPES: readonly string[] = ['ingest', 'admin'];

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
	| 'internal'
	// sentinel-sourcemaps §4.1 / §7 -- `POST /api/sourcemaps` uses this same envelope (§3).
	| 'too_many_files'
	| 'invalid_sourcemap'
	| 'release_full'
	| 'storage_unavailable';

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
	// SvelteKit's `error()` carries the text in `body.message`; the Node adapter's own throws
	// (`SvelteKitError(413, 'Payload Too Large', 'Content-length of … exceeds limit of … bytes.')`,
	// the BODY_SIZE_LIMIT rejection sentinel-sourcemaps §4.1 wraps) carry it in `message` on an
	// Error whose `status` is set. Both are client-facing by construction; a 500's message is not.
	const errMessage = (err as { message?: unknown })?.message;
	const message =
		(err as { body?: { message?: string } })?.body?.message ??
		(status !== 500 && typeof errMessage === 'string' && errMessage.length > 0 ? errMessage : undefined) ??
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

/**
 * The credential alone, before any project has been resolved.
 *
 * Split out from `ReportAuthContext` because §4.1 lets an org-wide key name its project in the
 * **body** (`project_key`) as well as in `X-Project-Key`, and the body cannot be read before the
 * credential is known good -- §7 orders a `401` ahead of every `400`. `POST /api/report`
 * therefore authenticates first, parses second, and resolves the project third; §4.5's multipart
 * route has no body-borne project key and composes the two steps in one call below.
 */
export interface ReportKeyCredential {
	keyId: string;
	organizationId: string;
	/** The project this key is pinned to, or null for an org-wide key. */
	fixedProjectId: string | null;
	scope: string;
	rateLimitRpm: number;
	orgWide: boolean;
}

/** Hashed exactly like `apikeys.ts`'s `createApiKey` and the ingestor: sha256 hex of the secret. */
function hashKey(secretToken: string): string {
	return crypto.createHash('sha256').update(secretToken).digest('hex');
}

/**
 * What a route brings to `authenticateApiKey`: its own scope allowlist (`ScopeSet`'s rule -- one
 * per route), the sentence its 403 carries, and the limiter key prefix that keeps its rate-limit
 * bucket separate from every other route's (a report flood must not spend the sourcemap
 * budget, nor the reverse -- the two routes are two callers behind two keys).
 */
export interface ApiKeyRoutePolicy {
	scopes: readonly string[];
	forbiddenMessage: string;
	rateLimitPrefix: string;
}

const REPORT_KEY_POLICY: ApiKeyRoutePolicy = {
	scopes: REPORT_SCOPES,
	forbiddenMessage: "This API key's scope may not submit user reports.",
	rateLimitPrefix: 'report-key',
};

/**
 * sentinel-sourcemaps §2.1: `{ingest, admin}` under its own limiter prefix; `resolveReportProject`
 * is reused as it is (decision 24), so the project answers are the same three the intake gives.
 */
const SOURCEMAP_KEY_POLICY: ApiKeyRoutePolicy = {
	scopes: SOURCEMAP_SCOPES,
	forbiddenMessage: "This API key's scope may not upload source maps.",
	rateLimitPrefix: 'sourcemap-key',
};

/**
 * Validates the `X-API-Key` credential: identity (401), scope (403 `forbidden_scope`), then the
 * key's own `rate_limit_rpm` (429). The order is deliberate -- an unknown key must not be able to
 * burn a real key's limit, and a 401 must not be shadowed by a coincidental 429.
 *
 * Throws a `Response` (never SvelteKit's `error()`) so every failure carries §7's envelope.
 */
export async function authenticateReportKey(request: Request): Promise<ReportKeyCredential> {
	return authenticateApiKey(request, REPORT_KEY_POLICY);
}

/**
 * The one `X-API-Key` authenticator, parameterised by the route's allowlist. Generalised from
 * `authenticateReportKey` for sentinel-sourcemaps (be.md task 7) rather than copied: the identity
 * check, the 401 collapse, the scope-before-limiter order and the §7 envelope are one policy, and
 * only the allowlist, the 403 sentence and the limiter prefix are the route's.
 */
export async function authenticateApiKey(
	request: Request,
	policy: ApiKeyRoutePolicy
): Promise<ReportKeyCredential> {
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

	// §2: the route's allowlist and nothing else -- for the intake `report` or `admin`, so `ingest`,
	// `read` and `agent` keys are 403 there, the mirror image of the ingestor refusing a `report`
	// key on /ingest; for the sourcemap upload `ingest` or `admin` (SOURCEMAP_SCOPES).
	if (!policy.scopes.includes(keyRow.scope)) {
		throw reportErrorResponse(403, 'forbidden_scope', policy.forbiddenMessage, { scope: keyRow.scope });
	}

	// The key's own rate_limit_rpm, checked only after the credential is known good (SR-1: the
	// shipped in-process limiter, per replica; §4.5's Postgres byte budget is the durable ceiling).
	const rateLimit = checkRateLimitWithLimit(`${policy.rateLimitPrefix}:${keyRow.id}`, keyRow.rateLimitRpm);
	if (!rateLimit.allowed) {
		throw reportErrorResponse(429, 'rate_limited', 'Rate limit exceeded.', {}, {
			'Retry-After': String(rateLimit.retryAfter ?? 60),
		});
	}

	return {
		keyId: keyRow.id,
		organizationId: keyRow.organizationId,
		fixedProjectId: keyRow.projectId ?? null,
		scope: keyRow.scope,
		rateLimitRpm: keyRow.rateLimitRpm,
		orgWide: keyRow.projectId === null,
	};
}

/**
 * Resolves the project this request writes into, **within the credential's organization only**
 * (§2, B7).
 *
 * `requestedProjectKey` is `X-Project-Key` or §4.1's body `project_key` -- the caller decides which
 * it read; this function does not know and must not care.
 *
 * - **Org-wide key:** the name is required (there is nowhere else the project could come from,
 *   since §4.6 identifies an external reporter per PROJECT) and is looked up by
 *   `(name, organizationId)`. A name that does not resolve there is `403 project_not_found`.
 * - **Project-scoped key:** the key fixes its own project and a supplied name can never redirect
 *   the write. A supplied name is still CHECKED against the key's organization, and one that does
 *   not resolve there is `403 project_not_found`, byte-identical to the answer for a project that
 *   does not exist.
 *
 *   That check is stricter than §4.1's request table, whose `project_key` row reads "Ignored for a
 *   project-scoped key" -- taken literally, naming another tenant's project would be a `201`. It
 *   follows TASK-03's acceptance criterion 2 instead ("a project-scoped key naming another
 *   organization's project in the body or in `X-Project-Key` gets a `403` … and creates nothing"),
 *   and it keeps "ignored" true in the sense that matters: a name inside the key's own
 *   organization is still ignored, and no name of any kind can move the write. Recorded as a
 *   proved contract deviation in `status/be.md` rather than resolved silently either way.
 */
export async function resolveReportProject(
	credential: ReportKeyCredential,
	requestedProjectKey: string | null
): Promise<{ projectId: string; projectName: string }> {
	const requested = requestedProjectKey?.trim() || null;

	if (!credential.orgWide) {
		if (requested) {
			const [named] = await db
				.select({ id: projects.id })
				.from(projects)
				.where(and(eq(projects.name, requested), eq(projects.organizationId, credential.organizationId)));
			if (!named) {
				throw reportErrorResponse(403, 'project_not_found', 'Project not found in this organization.');
			}
		}

		const [projectRow] = await db
			.select({ id: projects.id, name: projects.name })
			.from(projects)
			.where(eq(projects.id, credential.fixedProjectId as string));
		if (!projectRow) {
			throw reportErrorResponse(403, 'project_not_found', 'Project not found in this organization.');
		}
		return { projectId: projectRow.id, projectName: projectRow.name };
	}

	if (!requested) {
		// An external reporter is identified per PROJECT (§4.6's UNIQUE (project_id, external_id)),
		// so an org-wide key that names no project has nowhere to write.
		throw reportErrorResponse(400, 'invalid_request', 'X-Project-Key is required for an organization-wide key.', {
			field: 'X-Project-Key',
		});
	}

	const [projectRow] = await db
		.select({ id: projects.id, name: projects.name })
		.from(projects)
		.where(and(eq(projects.name, requested), eq(projects.organizationId, credential.organizationId)));
	if (!projectRow) {
		throw reportErrorResponse(403, 'project_not_found', 'Project not found in this organization.');
	}
	return { projectId: projectRow.id, projectName: projectRow.name };
}

/**
 * Authenticates an `X-API-Key` request for the intake routes AND resolves its project from
 * `X-Project-Key`, in one call.
 *
 * This is the whole story for §4.5's multipart upload, which has no body-borne project key.
 * §4.1's JSON endpoint uses the two halves separately, because it must read `project_key` out of a
 * body it may not parse until the credential has been accepted.
 */
export async function authenticateReportRequest(request: Request): Promise<ReportAuthContext> {
	const credential = await authenticateReportKey(request);
	const project = await resolveReportProject(credential, request.headers.get('x-project-key'));

	return {
		keyId: credential.keyId,
		organizationId: credential.organizationId,
		projectId: project.projectId,
		projectName: project.projectName,
		scope: credential.scope,
		rateLimitRpm: credential.rateLimitRpm,
		orgWide: credential.orgWide,
	};
}

/**
 * sentinel-sourcemaps §2.1: authenticates `POST /api/sourcemaps` -- `X-API-Key` on the
 * `{ingest, admin}` allowlist, then the project from the key row or from `X-Project-Key` within
 * the key's organization, in the intake's order 401 → 403 (scope) → 400/403 (project) → 429.
 *
 * `resolveReportProject` is reused exactly as it is (decision 24): a missing `X-Project-Key` on an
 * org-wide key is its `400 invalid_request { field: "X-Project-Key" }`, and a name outside the
 * organization is its `403 project_not_found` -- for a project-scoped key too -- so the route
 * cannot enumerate another tenant's projects. Nothing in the multipart body names the project (B7).
 */
export async function authenticateSourcemapRequest(request: Request): Promise<ReportAuthContext> {
	const credential = await authenticateApiKey(request, SOURCEMAP_KEY_POLICY);
	const project = await resolveReportProject(credential, request.headers.get('x-project-key'));

	return {
		keyId: credential.keyId,
		organizationId: credential.organizationId,
		projectId: project.projectId,
		projectName: project.projectName,
		scope: credential.scope,
		rateLimitRpm: credential.rateLimitRpm,
		orgWide: credential.orgWide,
	};
}
