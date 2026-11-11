import { eq } from 'drizzle-orm';
import { db } from '$lib/server/db';
import { organizations } from '$lib/db/schema';
import { getAccessibleProjects } from '$lib/server/issue-access';
import { resolveEnvironmentScopes } from '$lib/server/environment-access';
import { countTraceOccurrences } from '$lib/server/queries/trace-queries';
import { isCanonicalTraceId } from '$lib/trace-id';

/**
 * sentinel-cross-repo-trace-view D5 — what the issue page needs to link an occurrence to its trace:
 * the organization's slug (the trace page lives at `/{orgSlug}/traces/{id}`; `/issues/[id]` is not
 * org-scoped, so the slug is read off the issue's own organization) and, for the SELECTED
 * occurrence only, how many errors the same trace holds in OTHER projects.
 *
 * The count composes the SAME guarded conditions as the trace page (current org's projects only,
 * the viewer's environment grants) via `countTraceOccurrences`, so a count can never advertise rows
 * the page would not show. Own project is excluded: "related errors in other projects".
 *
 * Lives here, not in the route's `+page.server.ts`, which may export only SvelteKit's allowlisted
 * names (B12) — and so the loader's unit tests stub one module instead of the whole access layer.
 */
export async function loadTraceLinks(opts: {
	userId: string;
	organizationId: string;
	projectId: string;
	/** The selected occurrence's stored trace id, when it has one. */
	selectedTraceId: string | null;
}): Promise<{ orgSlug: string | null; related: { count: number; capped: boolean } | null }> {
	const [org] = await db
		.select({ slug: organizations.slug })
		.from(organizations)
		.where(eq(organizations.id, opts.organizationId));

	// The STORED value must already be canonical: the trace page matches the lower-cased id by
	// equality, so a count for `ABC…` would advertise rows the page cannot show.
	const traceId = opts.selectedTraceId;
	if (!isCanonicalTraceId(traceId)) return { orgSlug: org?.slug ?? null, related: null };

	const accessible = await getAccessibleProjects(opts.userId, opts.organizationId);
	const { total, capped } = await countTraceOccurrences(
		traceId,
		{
			projectIds: accessible.map((p) => p.id),
			environmentScopes: await resolveEnvironmentScopes(opts.userId),
		},
		{ excludeProjectId: opts.projectId }
	);
	return { orgSlug: org?.slug ?? null, related: { count: total, capped } };
}
