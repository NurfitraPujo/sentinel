import { fail, redirect } from '@sveltejs/kit';
import { z } from 'zod';
import { requireAuth } from '$lib/server/auth';
import {
	getUserOrganizations,
	createOrganization,
	createProject,
	RESERVED_SLUGS,
} from '$lib/db/queries/organizations';
import type { PageServerLoad, Actions } from './$types';

// First run. A magic-link sign-in creates a `users` row and nothing else — no organization, no
// membership, no project — and the dashboard is scoped to `getUserProjectAccess`, so such a user
// saw an empty page with no way out. Organization creation had an API (POST /api/organizations)
// that no UI called; project creation had neither. This page is the missing path.
//
// NOTE: 'onboarding' must stay in hooks.server.ts's `reservedRoutes`, or orgHandle reads it as an
// organization slug and 403s the very users this page exists for.

const orgSchema = z.object({
	name: z.string().trim().min(1, 'Organization name is required').max(255),
	slug: z
		.string()
		.trim()
		.toLowerCase()
		.min(1, 'URL slug is required')
		.max(255)
		// The slug becomes a top-level path segment (/{slug}/reports), so keep it to what is safe
		// and readable in a URL.
		.regex(/^[a-z0-9][a-z0-9-]*$/, 'Use lowercase letters, numbers and hyphens'),
});

const projectSchema = z.object({
	name: z.string().trim().min(1, 'Project name is required').max(255),
});

export const load: PageServerLoad = async (event) => {
	const user = await requireAuth(event);
	const orgs = await getUserOrganizations(user.id);

	return {
		// Step 2 only exists once step 1 has happened. Both live on one page so a new user sees the
		// whole path in front of them rather than being bounced between screens.
		organization: orgs[0] ? { id: orgs[0].id, name: orgs[0].name, slug: orgs[0].slug } : null,
	};
};

export const actions: Actions = {
	createOrg: async (event) => {
		const user = await requireAuth(event);

		const form = Object.fromEntries(await event.request.formData());
		const parsed = orgSchema.safeParse(form);
		if (!parsed.success) {
			return fail(400, {
				error: parsed.error.issues[0].message,
				values: { name: String(form.name ?? ''), slug: String(form.slug ?? '') },
			});
		}

		const { name, slug } = parsed.data;
		if (RESERVED_SLUGS.includes(slug)) {
			return fail(400, { error: `'${slug}' is reserved — pick another slug`, values: { name, slug } });
		}

		try {
			await createOrganization(user.id, { name, slug });
		} catch (err) {
			// `organizations.slug` is UNIQUE. A taken slug is the one failure a user can actually
			// act on, so name it instead of reporting a generic 500.
			const message = err instanceof Error ? err.message : '';
			if (/unique|duplicate/i.test(message)) {
				return fail(409, { error: `The slug '${slug}' is already taken`, values: { name, slug } });
			}
			throw err;
		}

		return { created: 'organization' as const };
	},

	createProject: async (event) => {
		const user = await requireAuth(event);

		const orgs = await getUserOrganizations(user.id);
		const org = orgs[0];
		if (!org) {
			return fail(400, { error: 'Create an organization first' });
		}

		const form = Object.fromEntries(await event.request.formData());
		const parsed = projectSchema.safeParse(form);
		if (!parsed.success) {
			return fail(400, { error: parsed.error.issues[0].message, values: { name: String(form.name ?? '') } });
		}

		await createProject(user.id, org.id, { name: parsed.data.name });

		// The dashboard is now non-empty for this user — the project membership createProject
		// wrote is exactly what getUserProjectAccess reads.
		redirect(303, '/');
	},
};
