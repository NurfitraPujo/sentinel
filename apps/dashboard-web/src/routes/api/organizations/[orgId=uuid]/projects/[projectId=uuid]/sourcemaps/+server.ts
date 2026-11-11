import { json, error } from '@sveltejs/kit';
import { and, eq } from 'drizzle-orm';
import type { RequestHandler } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { requireProjectAccess } from '$lib/server/issue-access';
import { resolveEnvironmentScope } from '$lib/server/environment-access';
import { listReleaseSourcemaps } from '$lib/db/queries/sourcemaps';
import { requireOrgMembership } from '../../../keys/_shared';

// GET /api/organizations/[orgId]/projects/[projectId]/sourcemaps?release_version=…
// sentinel-sourcemaps §4.2 — the maps one release has, fetched on demand by the releases page's
// expand control (§4.6, flows R2/R3). `camelCase`, no paging (a release's list is bounded by §4.1's
// per-release cap), `files: []` shown — never `null` — when the release has none, and NO
// `storageKey`: nothing serves map bytes to a browser (§1, *NOT building*); the query never selects
// the column, so it cannot reach this response.
//
// Auth is §2.2's: the Auth.js session, then the guards in the ORDER THE RELEASES LOADER RUNS THEM
// (routes/[orgSlug]/projects/[projectId=uuid]/releases/+page.server.ts) — authenticate, then the
// organization (`403`), then the project under that organization (`404 Project not found`), then
// `requireProjectAccess(…, 'read')`, then the environment dimension. This is an `[orgId]` route, so
// the organization step is the caller's own membership row for `orgId` (`requireOrgMembership`, as
// every route in this tree), not `locals.currentOrg` — under `/api/**` the hook resolves that from
// the last-active preference, never from the URL.
//
// **One string, three causes**, the loader's rule: `404 Project not found` — the SAME literal
// `requireProjectAccess` uses — answers an unknown project, a project of ANOTHER organization under
// this `orgId` (never `403`: a `403` says "there is a project here you may not see"), and a member
// whose environment scope is non-null (decision 20 (b) of `sentinel-releases-snooze`, inherited by
// §4.2 unchanged: a sourcemap is a project-level fact, the table has no environment column, so there
// is no "the maps you may see" to serve a scoped member). Never this tree's own `Project not found in
// this organization`, which is why `requireProjectRoute` (../_shared.ts) is not used here — it
// carries that string and gates on the org role rather than on `requireProjectAccess`.
export const GET: RequestHandler = async ({ params, locals, url }) => {
	const { orgId, projectId } = params;

	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const membership = await requireOrgMembership(session.user.id, orgId);
	if (!membership) {
		throw error(403, 'Forbidden: not a member of this organization');
	}

	const [project] = await db
		.select({ id: projects.id })
		.from(projects)
		.where(and(eq(projects.id, projectId), eq(projects.organizationId, orgId)));
	if (!project) {
		throw error(404, 'Project not found');
	}

	// §2.2: org write roles pass, a `project_members` row passes, an org `viewer` with neither is the
	// helper's `403`. The organization is the one just verified, so the helper's own lookup is skipped.
	await requireProjectAccess(session.user.id, projectId, 'read', { organizationId: orgId });

	// The ENVIRONMENT dimension — the unknown-project string, never `403` (see above).
	const scope = await resolveEnvironmentScope(session.user.id, projectId);
	if (scope !== null) {
		throw error(404, 'Project not found');
	}

	// §4.2: required, exact — byte-for-byte against the stored string, which is what CI sent
	// (decision 25). No trim: a version with surrounding whitespace is a different string, and
	// §4.1 already refused to store one, so it simply has no maps.
	const releaseVersion = url.searchParams.get('release_version');
	if (!releaseVersion) {
		throw error(400, 'release_version is required');
	}

	const rows = await listReleaseSourcemaps(projectId, releaseVersion);

	return json({
		releaseVersion,
		files: rows.map((row) => ({
			id: row.id,
			minifiedPath: row.minifiedPath,
			sizeBytes: row.sizeBytes,
			sha256: row.sha256,
			// RFC 3339 UTC with `Z` (§3).
			uploadedAt: row.uploadedAt.toISOString(),
		})),
	});
};
