import { error } from '@sveltejs/kit';
import type { Session } from '@auth/core/types';
import type { RequestEvent } from '@sveltejs/kit';

export interface AuthUser {
	id: string;
	email: string;
	name?: string;
}

export async function getUser(event: RequestEvent): Promise<AuthUser | null> {
	const session = await event.locals.auth();
	if (!session?.user?.email) {
		return null;
	}
	return {
		id: (session.user as any).id || session.user.email,
		email: session.user.email,
		name: session.user.name || undefined,
	};
}

// Throws SvelteKit's HttpError, NOT a bare Error. A bare Error is an unhandled exception as far as
// SvelteKit is concerned, so an unauthenticated GET /search answered 500 ("Error: Authentication
// required" in the pod log) where every other page either redirects (/) or 401s (/settings/*).
// error(401) is handled natively on both sides of the app: a page renders the error boundary, and a
// +server.ts route serialises `{"message":"Authentication required"}` with the 401 status — which is
// exactly the response the /api/alerts handlers were reconstructing by string-matching the message.
export async function requireAuth(event: RequestEvent): Promise<AuthUser> {
	const user = await getUser(event);
	if (!user) {
		error(401, 'Authentication required');
	}
	return user;
}
