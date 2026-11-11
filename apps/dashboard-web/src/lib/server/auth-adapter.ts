import type { Adapter } from '@auth/core/adapters';
import * as authQueries from './queries/auth';

export function CQRSAdapter(): Adapter {
	return {
		createUser: (user) => authQueries.createUser(user),
		getUser: (id) => authQueries.getUser(id),
		getUserByEmail: (email) => authQueries.getUserByEmail(email),
		getUserByAccount: (account) => authQueries.getUserByAccount(account),
		updateUser: (user) => authQueries.updateUser(user),
		deleteUser: (userId) => authQueries.deleteUser(userId),
		linkAccount: (account) => authQueries.linkAccount(account),
		unlinkAccount: (account) => authQueries.unlinkAccount(account),
		createSession: (session) => authQueries.createSession(session),
		getSessionAndUser: (sessionToken) => authQueries.getSessionAndUser(sessionToken),
		updateSession: (session) => authQueries.updateSession(session),
		deleteSession: (sessionToken) => authQueries.deleteSession(sessionToken),
		createVerificationToken: (verificationToken) => authQueries.createVerificationToken(verificationToken),
		useVerificationToken: (params) => authQueries.useVerificationToken(params),
	};
}
