import { error } from '@sveltejs/kit';
import { and, eq } from 'drizzle-orm';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { projects } from '$lib/db/schema';
import { requireProjectAccess } from '$lib/server/issue-access';
import { resolveEnvironmentScope } from '$lib/server/environment-access';
import { listProjectReleases } from '$lib/db/queries/releases';

// sentinel-releases-snooze §4.4 — the project's releases (flows R1–R5), reached by the third link on
// each row of the projects list (decision 25). Guards are §2's, in the settings loader's order
// (settings/+page.server.ts): authenticate BEFORE any org/project lookup (D39), then the org slug,
// then the project, then `requireProjectAccess`, then the environment dimension.
//
// **One string, three causes.** `404 Project not found` — the SAME literal `requireProjectAccess`
// uses for an unknown project — answers (1) a project id that does not exist, (2) a project of
// ANOTHER organization under this org's slug, and (3) a member whose environment scope is non-null
// (decision 20 (b)). Never the settings loader's own `Project not found in this organization`, and
// never a `403` for (3): a `403` says "there is a project here you may not see", which is the fact
// the environment boundary exists to withhold (the `sentinel-project-environments` rule). The
// contractor with a `staging`-only grant sees the *Releases* link and gets this `404` on click —
// accepted, decision 28.
//
// (2) is the check `requireProjectAccess` does NOT make: it verifies membership in the PROJECT's
// organization, not that the project sits under `orgSlug`'s. Without it a member of two
// organizations could open `/{orgA}/projects/{orgB-project}/releases` and read B's releases under
// A's chrome — both orgs are theirs, so not a tenant leak, but two implementers would write two
// pages. The lookup below is the settings loader's own next check, with the shared string.
//
// No caller with a non-null scope reaches the query, which is why `issuesIntroduced` is counted
// across every environment with no scope clause (releases.ts, decision 36).
export const load: PageServerLoad = async ({ params, locals }) => {
	const { orgSlug, projectId } = params;

	const session = await locals.auth();
	if (!session?.user?.email || !session.user.id) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	const [project] = await db
		.select({ id: projects.id, name: projects.name })
		.from(projects)
		.where(and(eq(projects.id, projectId), eq(projects.organizationId, currentOrg.id)));

	if (!project) {
		throw error(404, 'Project not found');
	}

	// §2: org write roles pass, a `project_members` row passes, an org `viewer` with neither is the
	// helper's `403`. The organization is the one just verified, so the helper's own project lookup
	// is skipped — it would only re-find the row above.
	await requireProjectAccess(session.user.id, projectId, 'read', {
		organizationId: currentOrg.id,
	});

	// Decision 20 (b) — the ENVIRONMENT dimension. A release is a project-level fact (§3): the table
	// has no environment column, so there is no "the releases you may see" to serve a scoped member,
	// and the page is withheld whole. `404`, the unknown-project string, never `403`.
	const scope = await resolveEnvironmentScope(session.user.id, projectId);
	if (scope !== null) {
		throw error(404, 'Project not found');
	}

	const { releases, truncated } = await listProjectReleases(projectId);

	return {
		org: { slug: currentOrg.slug, name: currentOrg.name },
		project: { id: project.id, name: project.name },
		// §4.4: `first_seen DESC`, at most 200 (decision 24); `[]` when none — shown, never `null`.
		releases,
		// `true` iff the project has more than 200 releases; the page says *Showing the 200 most recent*.
		truncated,
	};
};
