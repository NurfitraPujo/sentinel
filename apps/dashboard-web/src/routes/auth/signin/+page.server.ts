import { env } from '$env/dynamic/private';
import { fail } from '@sveltejs/kit';
import { z } from 'zod';
import { checkRateLimit } from '$lib/rate-limit';
import { logAuditEvent } from '$lib/server/audit';
import { signIn } from '$lib/auth';
import type { PageServerLoad, Actions } from './$types';

const emailSchema = z.object({
	email: z.string().email('Invalid email address'),
});

export const load: PageServerLoad = async () => {
	return {
		emailConfigured: !!env.EMAIL_SERVER,
	};
};

export const actions: Actions = {
	magiclink: async ({ request, getClientAddress }) => {
		const formData = await request.formData();
		const email = formData.get('email')?.toString() ?? '';

		const validation = emailSchema.safeParse({ email });
		if (!validation.success) {
			return fail(400, { error: 'Invalid email address', email });
		}

		const rateLimit = checkRateLimit(email);
		if (!rateLimit.allowed) {
			logAuditEvent({
				type: 'magic_link_requested',
				email,
				ip: getClientAddress(),
				error: `Rate limit exceeded - retry after ${rateLimit.retryAfter}s`,
			});
			return fail(429, {
				error: 'Too many requests. Please try again later.',
				retryAfter: rateLimit.retryAfter,
				email,
			});
		}

		logAuditEvent({
			type: 'magic_link_requested',
			email,
			ip: getClientAddress(),
		});

		try {
			await signIn('email', { email, callbackUrl: '/' });
			return { success: true, email };
		} catch (err) {
			logAuditEvent({
				type: 'magic_link_failed',
				email,
				ip: getClientAddress(),
				error: err instanceof Error ? err.message : 'Unknown error',
			});
			return fail(500, { error: 'Failed to send magic link', email });
		}
	},
};