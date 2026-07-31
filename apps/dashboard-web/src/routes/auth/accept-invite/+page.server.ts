import { error, redirect, fail } from '@sveltejs/kit';
import type { PageServerLoad, Actions } from './$types';
import { env } from '$env/dynamic/private';
import { getInvitationByToken, deleteInvitationById, upsertOrganizationMember, updateUserLastActiveOrg } from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { organizationMembers, users } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import { signIn } from '$lib/server/auth-config';

export const load: PageServerLoad = async ({ url, locals }) => {
	const token = url.searchParams.get('token');
	if (!token) {
		return { status: 'invalid_token' };
	}

	const result = await getInvitationByToken(token);
	if (!result) {
		return { status: 'invalid_token' };
	}

	const { invitation, organization } = result;

	if (invitation.status !== 'pending' || new Date(invitation.expiresAt) < new Date()) {
		return { status: 'expired_or_invalid' };
	}

	const session = await locals.auth();
	const user = session?.user ?? null;

	if (user?.email) {
		const userEmail = user.email.toLowerCase();
		const inviteEmail = invitation.email.toLowerCase();

		if (userEmail !== inviteEmail) {
			// Security guard: Hide org & invitation details on email mismatch
			return { status: 'email_mismatch' };
		}

		const userResult = await db
			.select({ id: users.id })
			.from(users)
			.where(eq(users.email, user.email))
			.limit(1);

		const userId = userResult.length > 0 ? userResult[0].id : null;

		// Check if user is already a member
		const [existingMember] = userId
			? await db
					.select()
					.from(organizationMembers)
					.where(
						and(
							eq(organizationMembers.organizationId, organization.id),
							eq(organizationMembers.userId, userId)
						)
					)
			: [null];

		if (existingMember && existingMember.role === invitation.role) {
			return {
				status: 'already_member',
				organization: { name: organization.name, slug: organization.slug },
				role: existingMember.role,
				token,
			};
		}

		return {
			status: 'valid_authenticated',
			invitation: {
				id: invitation.id,
				email: invitation.email,
				role: invitation.role,
				token: invitation.token,
			},
			organization: {
				id: organization.id,
				name: organization.name,
				slug: organization.slug,
			},
			userEmail: user.email,
			token,
		};
	}

	return {
		status: 'valid_unauthenticated',
		invitation: {
			email: invitation.email,
			role: invitation.role,
		},
		organization: {
			name: organization.name,
		},
		emailConfigured: !!env.EMAIL_SERVER,
		token,
	};
};

export const actions: Actions = {
	accept: async ({ request, locals, url }) => {
		const session = await locals.auth();
		if (!session?.user?.email) {
			throw error(401, 'Unauthorized session');
		}

		const userResult = await db
			.select({ id: users.id })
			.from(users)
			.where(eq(users.email, session.user.email))
			.limit(1);

		if (userResult.length === 0) {
			throw error(401, 'User account not found');
		}
		const userId = userResult[0].id;

		const formData = await request.formData();
		const token = formData.get('token')?.toString();

		if (!token) {
			return fail(400, { error: 'Missing invitation token' });
		}

		const result = await getInvitationByToken(token);
		if (!result) {
			return fail(404, { error: 'Invitation token not found or already redeemed' });
		}

		const { invitation, organization } = result;

		if (invitation.status !== 'pending' || new Date(invitation.expiresAt) < new Date()) {
			return fail(400, { error: 'Invitation token has expired or is invalid' });
		}

		if (session.user.email.toLowerCase() !== invitation.email.toLowerCase()) {
			return fail(403, { error: 'Logged in account email does not match invitation email' });
		}

		// Provision / upgrade member role in organization
		await upsertOrganizationMember(
			organization.id,
			userId,
			invitation.role as 'owner' | 'admin' | 'engineer' | 'support' | 'viewer'
		);

		// Set active organization preference
		await updateUserLastActiveOrg(userId, organization.id);

		// Delete redeemed invitation token
		await deleteInvitationById(invitation.id);

		// Redirect to organization dashboard
		throw redirect(303, `/${organization.slug}`);
	},

	google: async ({ url }) => {
		const token = url.searchParams.get('token') ?? '';
		const callbackUrl = `/auth/accept-invite?token=${encodeURIComponent(token)}`;
		await (signIn as unknown as (provider: string, options: Record<string, unknown>) => Promise<unknown>)(
			'google',
			{ redirectTo: callbackUrl }
		);
	},

	magiclink: async ({ request, url }) => {
		const formData = await request.formData();
		const email = formData.get('email')?.toString() ?? '';
		const token = url.searchParams.get('token') ?? '';
		const callbackUrl = `/auth/accept-invite?token=${encodeURIComponent(token)}`;

		if (!email) {
			return fail(400, { error: 'Email address is required' });
		}

		try {
			await (signIn as unknown as (provider: string, options: Record<string, unknown>) => Promise<unknown>)(
				'email',
				{ email, callbackUrl }
			);
			return { magicLinkSent: true, email };
		} catch (err) {
			return fail(500, { error: 'Failed to send magic link' });
		}
	},
};
