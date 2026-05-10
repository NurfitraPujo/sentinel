import type { Session } from '@auth/core/types';
import type { RequestEvent } from '@sveltejs/kit';

export interface AuthUser {
	id: string;
	email: string;
	name: string;
}

export async function getUser(event: RequestEvent): Promise<AuthUser | null> {
	const session = await event.locals.auth();
	if (!session?.user?.email) {
		return null;
	}

	return {
		id: session.user.email,
		email: session.user.email,
		name: session.user.name ?? session.user.email,
	};
}

export async function requireAuth(event: RequestEvent): Promise<AuthUser> {
	const user = await getUser(event);
	if (!user) {
		throw new Error('Authentication required');
	}
	return user;
}