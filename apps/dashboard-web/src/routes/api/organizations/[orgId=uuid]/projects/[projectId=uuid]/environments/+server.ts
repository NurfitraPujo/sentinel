import { json, error } from '@sveltejs/kit';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projectEnvironments } from '$lib/db/schema';
import { ENV_NAME_RE } from '$lib/db/queries/reports';
import {
	DISPLAY_NAME_MAX_LENGTH,
	MAX_ENVIRONMENTS_PER_PROJECT,
	clearOtherDefaults,
	listProjectEnvironments,
	toEnvironmentApiShape,
} from '$lib/db/queries/environments';
import { resolveEnvironmentScope } from '$lib/server/environment-access';
import { WRITE_FORBIDDEN, requireProjectRoute, wantsHidden } from '../../_shared';

// GET / POST /api/organizations/[orgId]/projects/[projectId]/environments
// sentinel-project-environments §4.1 (the registry) and §4.3 (declare one).
//
// The auth mechanism is `../[projectId]/agent-settings/+server.ts`'s, exactly: locals.auth() ->
// requireOrgMembership -> hasPermission -> project-in-org, with authority read from the caller's own
// membership row. Errors are SvelteKit's `error(status, message)`; this app has no
// `{error:{code}}` envelope and §7 says why one is not being introduced here.

/**
 * §4.1. Two modes, and the scope rule is the one place §2.2's boundary is deliberately not total:
 *
 * - `includeHidden=false` (default) — `read`, and the list is filtered to the caller's environment
 *   scope. This is what feeds every selector in §4.4, §4.5 and §4.7: a member granted `staging` is
 *   not offered a `production` filter, because a filter for data you cannot read is a control that
 *   does nothing.
 * - `includeHidden=true` — the project-settings management view and §4.10a's invitation picker.
 *   ONE gate for both callers, `write` (decision 44), and **not** scope-filtered: an environment's
 *   NAME is not protected, its DATA is (§12 *Known limits* 3).
 *
 * `{"environments": []}` — not `null`, not a `404` — is the normal state of every project on the
 * day this ships, because under decision 8 no registry row is ever created by traffic.
 */
export const GET: RequestHandler = async ({ params, locals, url }) => {
	const includeHidden = wantsHidden(url);
	const { userId, projectId } = await requireProjectRoute(
		locals,
		params,
		includeHidden ? 'write' : 'read',
		WRITE_FORBIDDEN
	);

	const rows = await listProjectEnvironments(projectId);

	if (includeHidden) {
		return json({ environments: rows.map(toEnvironmentApiShape) });
	}

	const scope = await resolveEnvironmentScope(userId, projectId);
	const visible = rows.filter(
		// §2.2: a null scope is unrestricted, not "nothing". `hidden` is display, never access —
		// §4.10 is access — so it only ever narrows this default selector feed.
		(row) => !row.hidden && (scope === null || scope.includes(row.name))
	);

	return json({ environments: visible.map(toEnvironmentApiShape) });
};

/**
 * §4.3 — the ONLY way a registry row is ever created. Traffic never creates one; that is the whole
 * content of decision 8's "declared only". Not idempotent: a second call with the same `name` is a
 * `409`, never a second row.
 */
export const POST: RequestHandler = async ({ params, request, locals }) => {
	const { projectId } = await requireProjectRoute(locals, params, 'write', WRITE_FORBIDDEN);

	const body = await request.json().catch(() => ({}) as Record<string, unknown>);

	const name = body?.name;
	if (typeof name !== 'string' || !ENV_NAME_RE.test(name)) {
		// The same accepted set the ingest edge enforces (§3, decision 10), so a name that can be
		// declared is a name that can arrive.
		throw error(400, 'name must match ^[a-z0-9][a-z0-9._-]{0,49}$');
	}

	let displayName = name;
	if (body?.displayName !== undefined) {
		if (typeof body.displayName !== 'string') {
			throw error(400, `displayName must be 1-${DISPLAY_NAME_MAX_LENGTH} characters`);
		}
		displayName = body.displayName.trim();
		if (displayName.length === 0 || displayName.length > DISPLAY_NAME_MAX_LENGTH) {
			throw error(400, `displayName must be 1-${DISPLAY_NAME_MAX_LENGTH} characters`);
		}
	}

	const isDefault = body?.isDefault === true;

	const existing = await listProjectEnvironments(projectId);
	if (existing.some((row) => row.name === name)) {
		throw error(409, 'Environment already exists');
	}
	if (existing.length >= MAX_ENVIRONMENTS_PER_PROJECT) {
		throw error(422, 'Environment limit reached for this project');
	}

	// `isDefault: true` clears the previous default in the SAME transaction as the insert — the
	// partial unique index on (project_id) WHERE is_default is what makes at-most-one a database
	// fact, and it fires if the two halves are ever separated.
	const created = await db.transaction(async (tx) => {
		if (isDefault) {
			await clearOtherDefaults(tx, projectId);
		}
		const [row] = await tx
			.insert(projectEnvironments)
			.values({ projectId, name, displayName, isDefault })
			.returning();
		return row;
	});

	return json(toEnvironmentApiShape(created), { status: 201 });
};
