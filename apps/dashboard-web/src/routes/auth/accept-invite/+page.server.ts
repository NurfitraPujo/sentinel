import { error, redirect, fail } from '@sveltejs/kit';
import type { PageServerLoad, Actions } from './$types';
import { env } from '$env/dynamic/private';
import { getInvitationByToken, claimInvitation, updateUserLastActiveOrg } from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { organizationMembers, users } from '$lib/db/schema';
import { eq, and, sql } from 'drizzle-orm';
import { signIn } from '$lib/server/auth-config';

// D06: kept in sync with the same-named constant in routes/invitations/[token]/+page.server.ts,
// which is what sets this cookie. The raw token travels ONLY in that cookie (short-lived, HttpOnly)
// and the /invitations/<token> path segment that produced it -- never in a query string, form field
// trusted from the client, or OAuth redirectTo.
const INVITE_TOKEN_COOKIE = 'sentinel_invite_token';

export const load: PageServerLoad = async ({ cookies, locals }) => {
	const token = cookies.get(INVITE_TOKEN_COOKIE);
	if (!token) {
		return { status: 'invalid_token' };
	}

	const result = await getInvitationByToken(token);
	if (!result) {
		return { status: 'expired_or_invalid' };
	}

	const { invitation, organization } = result;

	const session = await locals.auth();
	const user = session?.user ?? null;

	if (user?.email) {
		const userEmail = user.email.toLowerCase();
		const inviteEmail = invitation.email.toLowerCase();

		if (userEmail !== inviteEmail) {
			// Security guard: Hide org & invitation details on email mismatch
			return { status: 'email_mismatch' };
		}

		// D30: match case-insensitively -- users.email carries provider casing, and
		// idx_user_email_lower_unique is the DB-side guarantee that lower(email) is unambiguous.
		const userResult = await db
			.select({ id: users.id })
			.from(users)
			.where(eq(sql`lower(${users.email})`, userEmail))
			.limit(1);

		const userId = userResult.length > 0 ? userResult[0].id : null;

		// Check if user is already a member. D08: this is a status message only -- the ACTUAL
		// no-downgrade guarantee lives in upsertOrganizationMember/claimInvitation's role-rank
		// upsert, not here, so reporting "already_member" is safe regardless of which role wins.
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

		if (existingMember) {
			return {
				status: 'already_member',
				organization: { name: organization.name, slug: organization.slug },
				role: existingMember.role,
			};
		}

		return {
			status: 'valid_authenticated',
			invitation: {
				id: invitation.id,
				email: invitation.email,
				role: invitation.role,
			},
			organization: {
				id: organization.id,
				name: organization.name,
				slug: organization.slug,
			},
			userEmail: user.email,
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
	};
};

export const actions: Actions = {
	accept: async ({ locals, cookies }) => {
		const session = await locals.auth();
		if (!session?.user?.email) {
			throw error(401, 'Unauthorized session');
		}

		const userResult = await db
			.select({ id: users.id })
			.from(users)
			.where(eq(sql`lower(${users.email})`, session.user.email.toLowerCase()))
			.limit(1);

		if (userResult.length === 0) {
			throw error(401, 'User account not found');
		}
		const userId = userResult[0].id;

		// D06: the token is read from the HttpOnly cookie set by /invitations/<token>, never from a
		// client-supplied form field -- a hidden input mirroring it back would be one more place the
		// raw secret could leak (view-source, extensions, logging middleware that dumps form bodies).
		const token = cookies.get(INVITE_TOKEN_COOKIE);
		if (!token) {
			return fail(400, { error: 'Missing invitation token' });
		}

		// Look up email/organization details before claiming, purely to give a clear error and to
		// enforce the email-match guard -- claimInvitation below is what actually, atomically,
		// single-use-claims the token (D07).
		const preCheck = await getInvitationByToken(token);
		if (!preCheck) {
			return fail(404, { error: 'Invitation token not found, already redeemed, or expired' });
		}
		if (session.user.email.toLowerCase() !== preCheck.invitation.email.toLowerCase()) {
			return fail(403, { error: 'Logged in account email does not match invitation email' });
		}

		const result = await claimInvitation(token, userId);
		if (!result.ok) {
			cookies.delete(INVITE_TOKEN_COOKIE, { path: '/' });
			if (result.reason === 'already_used') {
				return fail(409, { error: 'This invitation has already been used' });
			}
			if (result.reason === 'expired') {
				return fail(400, { error: 'This invitation has expired' });
			}
			return fail(404, { error: 'Invitation token not found or already redeemed' });
		}

		// Set active organization preference
		await updateUserLastActiveOrg(userId, result.organization.id);

		cookies.delete(INVITE_TOKEN_COOKIE, { path: '/' });

		// Redirect to organization dashboard
		throw redirect(303, `/${result.organization.slug}`);
	},

	google: async () => {
		// D06: redirectTo carries no token -- the invite token already lives in the HttpOnly cookie
		// set by /invitations/<token>, which survives this OAuth round trip on its own.
		await (signIn as unknown as (provider: string, options: Record<string, unknown>) => Promise<unknown>)(
			'google',
			{ redirectTo: '/auth/accept-invite' }
		);
	},

	magiclink: async ({ request }) => {
		const formData = await request.formData();
		const email = formData.get('email')?.toString() ?? '';

		if (!email) {
			return fail(400, { error: 'Email address is required' });
		}

		try {
			// D06: same reasoning as `google` above -- no token in the callback URL.
			await (signIn as unknown as (provider: string, options: Record<string, unknown>) => Promise<unknown>)(
				'email',
				{ email, callbackUrl: '/auth/accept-invite' }
			);
			return { magicLinkSent: true, email };
		} catch (err) {
			return fail(500, { error: 'Failed to send magic link' });
		}
	},
};
