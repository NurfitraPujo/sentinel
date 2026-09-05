import { error } from '@sveltejs/kit';
import { and, eq, inArray } from 'drizzle-orm';
import type { PageServerLoad } from './$types';
import { db } from '$lib/server/db';
import { organizationMembers, users } from '$lib/db/schema';
import { ISSUE_WRITE_ROLES } from '$lib/server/issue-access';
import type { OncallCandidate, OncallResponse } from '$lib/types/oncall';

// sentinel-user-report-issue §4.4a / fe.md rows O1-O10 — the on-call schedule editor, built as a
// sibling of settings/keys and settings/members.
//
// **The write gate is decided here, not in the component.** §2 splits this surface: read is any
// recognised org member (viewers included), write is `owner`/`admin` ONLY — narrower than
// `ISSUE_WRITE_ROLES`, which every other write control on this dashboard uses. `canWrite` below is
// that bar, and it is the same one `requireOncallWriteAccess` enforces on the API routes, so a
// component rendering a control this loader did not authorise would still be refused. The component
// consumes this flag and renders no control that would 403; both gates exist on purpose.
//
// D39: authenticate BEFORE any lookup that reveals whether an organization exists — the same
// ordering settings/keys and settings/members carry, and for the same reason.
const ONCALL_WRITE_ROLES = ['owner', 'admin'] as const;

export const load: PageServerLoad = async ({ params, fetch, locals }) => {
	const session = await locals.auth();
	if (!session?.user?.id) {
		throw error(401, 'Unauthorized');
	}

	const currentOrg = locals.currentOrg;
	if (!currentOrg || currentOrg.slug !== params.orgSlug) {
		throw error(403, 'Forbidden: Unauthorized access to organization');
	}

	// `locals.orgRole` is set by `orgHandle` (hooks.server.ts) for the org in the URL. fe.md calls
	// this `memberRole`, which is the name of the local variable inside that hook, not of the field
	// it exposes.
	const role = locals.orgRole ?? null;
	const canWrite = role !== null && (ONCALL_WRITE_ROLES as readonly string[]).includes(role);

	const res = await fetch(`/api/organizations/${currentOrg.id}/oncall`);
	if (!res.ok) {
		const body = await res.json().catch(() => ({ message: 'Failed to load the on-call schedule' }));
		throw error(res.status, body.message || 'Failed to load the on-call schedule');
	}

	const { schedule, currentlyOnCall } = (await res.json()) as OncallResponse;

	// §4.4a's `400` bar for `memberIds` and for an override's `userId`: an org member holding an
	// `ISSUE_WRITE_ROLES` role. Offering anyone else in the picker would build a form whose only
	// possible outcome is a 400, so the narrowing happens before the list reaches the page.
	//
	// A member already ON the rotation whose role has since dropped is deliberately NOT here: the
	// schedule carries them with `eligible: false`, which is how they stay visible and skipped
	// rather than silently deleted (O4). They are simply no longer addable.
	const candidateRows = await db
		.select({
			userId: organizationMembers.userId,
			role: organizationMembers.role,
			name: users.name,
			email: users.email,
		})
		.from(organizationMembers)
		.innerJoin(users, eq(users.id, organizationMembers.userId))
		.where(
			and(
				eq(organizationMembers.organizationId, currentOrg.id),
				inArray(organizationMembers.role, [...ISSUE_WRITE_ROLES])
			)
		);

	const candidates: OncallCandidate[] = candidateRows.map((row) => ({
		userId: row.userId,
		name: row.name ?? row.email,
		role: row.role,
	}));

	return {
		orgId: currentOrg.id,
		orgSlug: currentOrg.slug,
		role,
		canWrite,
		schedule,
		currentlyOnCall,
		candidates,
	};
};
