import { sequence } from '@sveltejs/kit/hooks';
import { handle as authHandle } from '$lib/server/auth-config';
import { db } from '$lib/server/db';
import { organizations, organizationMembers, userSessionPreferences, users } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import { error } from '@sveltejs/kit';

const orgHandle = async ({ event, resolve }: any) => {
	const session = await event.locals.auth();
	if (!session?.user?.email) {
		return resolve(event);
	}

	const userResult = await db.select({ id: users.id }).from(users).where(eq(users.email, session.user.email)).limit(1);
	if (userResult.length === 0) {
		return resolve(event);
	}
	const userId = userResult[0].id;

	const pathParts = event.url.pathname.split('/').filter(Boolean);
	// 'signin' added alongside the /auth/signin -> /signin move (see auth-config.ts) so the
	// top-level custom sign-in page is still treated as reserved, not mistaken for an org slug.
	const reservedRoutes = ['api', 'auth', 'issues', 'search', 'settings', 'admin', 'docs', 'billing', 'support', 'signin'];
	let orgSlugFromUrl = null;

	if (pathParts.length > 0 && !reservedRoutes.includes(pathParts[0].toLowerCase())) {
		orgSlugFromUrl = pathParts[0];
	}

	let activeOrg = null;
	let memberRole = null;

	if (orgSlugFromUrl) {
		const result = await db.select({
			org: organizations,
			member: organizationMembers
		}).from(organizations)
		  .leftJoin(organizationMembers, eq(organizationMembers.organizationId, organizations.id))
		  .where(and(eq(organizations.slug, orgSlugFromUrl), eq(organizationMembers.userId, userId)))
		  .limit(1);

		if (result.length > 0 && result[0].member) {
			activeOrg = result[0].org;
			memberRole = result[0].member.role;
		} else {
			error(403, 'Unauthorized access to organization');
		}
	} else {
		const prefsResult = await db.select({
			orgId: userSessionPreferences.lastActiveOrganizationId
		}).from(userSessionPreferences)
		  .where(eq(userSessionPreferences.userId, userId))
		  .limit(1);

		let fallbackOrgId = prefsResult.length > 0 ? prefsResult[0].orgId : null;

		if (fallbackOrgId) {
			const orgResult = await db.select({
				org: organizations,
				member: organizationMembers
			}).from(organizations)
			  .leftJoin(organizationMembers, eq(organizationMembers.organizationId, organizations.id))
			  .where(and(eq(organizations.id, fallbackOrgId), eq(organizationMembers.userId, userId)))
			  .limit(1);

			if (orgResult.length > 0 && orgResult[0].member) {
				activeOrg = orgResult[0].org;
				memberRole = orgResult[0].member.role;
			}
		}

		if (!activeOrg) {
			const firstOrgResult = await db.select({
				org: organizations,
				member: organizationMembers
			}).from(organizationMembers)
			  .innerJoin(organizations, eq(organizations.id, organizationMembers.organizationId))
			  .where(eq(organizationMembers.userId, userId))
			  .limit(1);

			if (firstOrgResult.length > 0) {
				activeOrg = firstOrgResult[0].org;
				memberRole = firstOrgResult[0].member.role;
			}
		}
	}

	if (activeOrg) {
		event.locals.currentOrg = activeOrg;
		event.locals.orgRole = memberRole;
	}

	return resolve(event);
};

export const handle = sequence(authHandle, orgHandle);
