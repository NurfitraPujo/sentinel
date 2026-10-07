import { fail } from '@sveltejs/kit';
import type { Actions, PageServerLoad, RequestEvent } from './$types';
import { checkRateLimitWithLimit } from '$lib/rate-limit';
import { getUserOrganizations } from '$lib/db/queries/organizations';
import {
	decideDeviceAuthorization,
	findDeviceAuthorization,
	formatUserCode,
	normalizeUserCode,
} from '$lib/server/cli-device';
import { liveKeysReplacedByDevice } from '$lib/server/cli-personal-keys';
import { approveRequestIsSameOrigin, cliApproveSignInUrl, parseApproveOrganizationId } from '$lib/server/cli-approve';
import { APPROVE_LOOKUP_LIMIT_PER_USER, APPROVE_LOOKUP_WINDOW_MS, clientIp } from '$lib/server/cli-device-http';

// sentinel-cli-browser-login (docs/plans/CLI_BROWSER_LOGIN.md). /cli/approve -- where a signed-in member approves (or
// denies) the user code `sentinel login` printed. `/cli` is in hooks.server.ts's reservedRoutes, so this is never read as
// an org slug; the session check is HERE (load and both actions), not inherited.
//
// Against phishing (someone else's code): the code is shown large, with the device name, requesting IP, time, CLI version
// and target org, and a plain warning; nothing is approved without an explicit click on Approve. Against CSRF: SvelteKit
// form actions behind the hook's cross-site form check, PLUS an explicit same-origin Origin check in each action (enforced
// in dev too), PLUS Auth.js's SameSite=Lax session cookie. Against code guessing: every code lookup by a user is counted,
// APPROVE_LOOKUP_LIMIT_PER_USER per 10 minutes.

function lookupAllowed(userId: string): boolean {
	return checkRateLimitWithLimit(`cli-approve:user:${userId}`, APPROVE_LOOKUP_LIMIT_PER_USER, APPROVE_LOOKUP_WINDOW_MS)
		.allowed;
}

export const load: PageServerLoad = async ({ locals, url }) => {
	const code = normalizeUserCode(url.searchParams.get('code'));
	const session = await locals.auth();
	const userId = session?.user?.id;
	if (!userId) {
		return { signedIn: false as const, signInUrl: cliApproveSignInUrl(code) };
	}

	const organizations = (await getUserOrganizations(userId)).map((o) => ({ id: o.id, name: o.name, slug: o.slug, role: o.role }));
	const base = {
		signedIn: true as const,
		userEmail: session.user?.email ?? '',
		organizations,
		defaultOrganizationId:
			organizations.find((o) => o.id === locals.currentOrg?.id)?.id ?? organizations[0]?.id ?? null,
	};

	const rawCode = url.searchParams.get('code');
	if (!code) {
		return { ...base, code: null, codeError: rawCode ? 'That is not a valid code. Codes look like BCDF-GHJK.' : null, request: null };
	}
	if (!lookupAllowed(userId)) {
		return { ...base, code: formatUserCode(code), codeError: 'Too many code lookups. Wait a few minutes and try again.', request: null };
	}

	const found = await findDeviceAuthorization(code);
	if (!found) {
		return { ...base, code: formatUserCode(code), codeError: 'No login request has this code. Check the code in your terminal.', request: null };
	}

	// What approving would REPLACE: the user's live key of the same device name, per org (mint revokes it).
	const replaces = found.state === 'pending' ? await liveKeysReplacedByDevice(userId, found.deviceName) : [];

	return {
		...base,
		code: formatUserCode(code),
		codeError: null,
		replaces,
		request: {
			state: found.state,
			client: found.client,
			clientVersion: found.clientVersion,
			deviceName: found.deviceName,
			requestedProfile: found.requestedProfile,
			requestedIp: found.requestedIp,
			createdAt: found.createdAt.toISOString(),
			expiresAt: found.expiresAt.toISOString(),
		},
	};
};

const REASONS: Record<string, string> = {
	not_found: 'No login request has this code.',
	expired: 'This code has expired. Run `sentinel login` again.',
	already_decided: 'This code was already used or denied. Run `sentinel login` again.',
	not_a_member: 'Pick an organization you are a member of.',
};

async function decide(event: RequestEvent, decision: 'approve' | 'deny') {
	if (!approveRequestIsSameOrigin(event.request, event.url.origin)) {
		return fail(403, { error: 'Cross-site request refused.' });
	}
	const session = await event.locals.auth();
	const userId = session?.user?.id;
	if (!userId) {
		return fail(401, { error: 'Sign in first.' });
	}
	if (!lookupAllowed(userId)) {
		return fail(429, { error: 'Too many attempts. Wait a few minutes and try again.' });
	}

	const form = await event.request.formData();
	const code = normalizeUserCode(form.get('code'));
	if (!code) {
		return fail(400, { error: 'That is not a valid code.' });
	}
	const organizationId = parseApproveOrganizationId(form.get('organizationId'));
	if (decision === 'approve' && !organizationId) {
		return fail(400, { error: REASONS.not_a_member });
	}

	const result = await decideDeviceAuthorization({
		userCode: code,
		userId,
		organizationId: decision === 'approve' ? organizationId : null,
		decision,
		ip: clientIp(event),
	});
	if (!result.ok) {
		return fail(result.reason === 'not_a_member' ? 403 : 400, { error: REASONS[result.reason] });
	}
	return { decided: result.state, code: formatUserCode(code) };
}

export const actions: Actions = {
	approve: (event) => decide(event, 'approve'),
	deny: (event) => decide(event, 'deny'),
};
