import { db } from '$lib/server/db';
import { users, accounts, sessions, verificationTokens } from '$lib/db/schema';
import { eq, and } from 'drizzle-orm';
import type { AdapterUser, AdapterAccount, AdapterSession, VerificationToken } from '@auth/core/adapters';

export async function createUser(user: Omit<AdapterUser, 'id'>) {
	const [newUser] = await db.insert(users).values(user).returning();
	return newUser as AdapterUser;
}

export async function getUser(id: string) {
	const [user] = await db.select().from(users).where(eq(users.id, id));
	return (user as AdapterUser) ?? null;
}

export async function getUserByEmail(email: string) {
	const [user] = await db.select().from(users).where(eq(users.email, email));
	return (user as AdapterUser) ?? null;
}

export async function getUserByAccount({ providerAccountId, provider }: Pick<AdapterAccount, 'provider' | 'providerAccountId'>) {
	const [row] = await db
		.select({ user: users })
		.from(users)
		.innerJoin(accounts, eq(accounts.userId, users.id))
		.where(
			and(
				eq(accounts.providerAccountId, providerAccountId),
				eq(accounts.provider, provider)
			)
		);
	return (row?.user as AdapterUser) ?? null;
}

export async function updateUser(user: Partial<AdapterUser> & { id: string }) {
	const [updatedUser] = await db
		.update(users)
		.set(user)
		.where(eq(users.id, user.id))
		.returning();
	return updatedUser as AdapterUser;
}

export async function deleteUser(userId: string) {
	await db.delete(users).where(eq(users.id, userId));
}

export async function linkAccount(account: AdapterAccount) {
	await db.insert(accounts).values(account);
}

export async function unlinkAccount({ providerAccountId, provider }: Pick<AdapterAccount, 'provider' | 'providerAccountId'>) {
	await db
		.delete(accounts)
		.where(
			and(
				eq(accounts.providerAccountId, providerAccountId),
				eq(accounts.provider, provider)
			)
		);
}

export async function createSession(session: { sessionToken: string; userId: string; expires: Date }) {
	const [newSession] = await db.insert(sessions).values(session).returning();
	return newSession as AdapterSession;
}

export async function getSessionAndUser(sessionToken: string) {
	const [row] = await db
		.select({
			session: sessions,
			user: users,
		})
		.from(sessions)
		.innerJoin(users, eq(sessions.userId, users.id))
		.where(eq(sessions.sessionToken, sessionToken));

	if (!row) return null;
	return {
		session: row.session as AdapterSession,
		user: row.user as AdapterUser,
	};
}

export async function updateSession(session: Partial<AdapterSession> & { sessionToken: string }) {
	const [updatedSession] = await db
		.update(sessions)
		.set(session)
		.where(eq(sessions.sessionToken, session.sessionToken))
		.returning();
	return updatedSession as AdapterSession;
}

export async function deleteSession(sessionToken: string) {
	await db.delete(sessions).where(eq(sessions.sessionToken, sessionToken));
}

export async function createVerificationToken(verificationToken: VerificationToken) {
	const [newToken] = await db
		.insert(verificationTokens)
		.values(verificationToken)
		.returning();
	return newToken as VerificationToken;
}

export async function useVerificationToken({ identifier, token }: { identifier: string; token: string }) {
	const [deletedToken] = await db
		.delete(verificationTokens)
		.where(
			and(
				eq(verificationTokens.identifier, identifier),
				eq(verificationTokens.token, token)
			)
		)
		.returning();
	return (deletedToken as VerificationToken) ?? null;
}
