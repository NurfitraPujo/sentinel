import { error, redirect, fail, isRedirect, isHttpError } from '@sveltejs/kit';
import type { PageServerLoad, Actions } from './$types';
import { env } from '$env/dynamic/private';
import { getInvitationByToken, claimInvitation, updateUserLastActiveOrg } from '$lib/db/queries/organizations';
import { db } from '$lib/server/db';
import { organizationMembers, users } from '$lib/db/schema';
import { eq, and, sql } from 'drizzle-orm';
import { signIn } from '$lib/server/auth-config';

// D06: the raw token travels ONLY in this cookie (short-lived, HttpOnly) and the
// /invitations/<token> path segment that produced it -- never in a query string, a form field
// trusted from the client, or an OAuth redirectTo. The name is shared with the route that SETS the
// cookie rather than duplicated and kept in sync by comment.
import { INVITE_TOKEN_COOKIE } from '$lib/server/invite-cookie';

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
			if (result.reason === 'inviter_no_longer_authorized') {
				// D31 (residual): the invitation itself is untouched -- claimInvitation rolled back the
				// claim, so it is still 'pending' and can be redeemed later if the inviter's authority is
				// restored. Deleting the cookie here only ends this browser round trip; it does not burn
				// the token, so revisiting the emailed link tries again rather than dead-ending.
				return fail(403, {
					error:
						'The person who sent this invitation no longer has permission to grant that role. Ask a current owner or admin to re-invite you.'
				});
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
			// D29: this used to pass `callbackUrl`, an Auth.js v4 option name. This project is on
			// @auth/sveltekit ^1.11.2 (v5), where the option is `redirectTo` -- v5 ignores
			// `callbackUrl` outright, so the post-verification destination was silently dropped and
			// the user landed on the default page instead of back here to finish accepting.
			//
			// D06: same reasoning as `google` above -- no token in the redirect URL. The invite token
			// lives in the HttpOnly cookie set by /invitations/<token>, which survives the round trip.
			await (signIn as unknown as (provider: string, options: Record<string, unknown>) => Promise<unknown>)(
				'email',
				{ email, redirectTo: '/auth/accept-invite' }
			);
			return { magicLinkSent: true, email };
		} catch (err) {
			// D29: `signIn` signals its outcome by THROWING a redirect. Swallowing everything here
			// turned that success signal into a 500, so the magic-link flow could not complete even
			// once the option name was right. Redirects (and SvelteKit HttpErrors) must propagate;
			// only a genuine send failure becomes a 500.
			if (isRedirect(err) || isHttpError(err)) {
				throw err;
			}
			return fail(500, { error: 'Failed to send magic link' });
		}
	},
};
