import { describe, it, expect, beforeAll, afterAll, beforeEach, vi } from 'vitest';
import postgres from 'postgres';
import { randomUUID } from 'node:crypto';

/**
 * sentinel-project-environments — contract §11's two EXISTING-route boundaries that this feature
 * adds a claim to:
 *
 *   - **The org member-removal transaction (§4.10, decision 40)** — that removing an org member also
 *     deletes that user's `project_environment_grants` rows for that organization's projects, **in
 *     the same transaction**, asserted beside the `issue_subscriptions` assertion because they are
 *     the same lifecycle. Two claims, and the second is the one a happy-path test misses: the rows
 *     are gone after a successful removal, **and a rollback of the transaction leaves both the
 *     membership and the grants intact**.
 *   - **`invitations/+server.ts` and `auth/accept-invite/` (§4.10a)** — and *"the claim that matters
 *     is the ABSENCE one"*: an invitation with no `environmentScopes` writes **no** grant row and
 *     the accepted member is unrestricted. Then the positive: a scoped invitation writes one grant
 *     row per entry in the same transaction as the membership, and a stale entry — a deleted
 *     PROJECT — is skipped rather than failing the acceptance.
 *
 * Against a real migrated Postgres, because every one of those is a claim about which rows exist
 * after a transaction settles. Same DATABASE_URL contract as the sibling integration files; set
 * `ENVIRONMENT_ROUTES_INTEGRATION_REQUIRED=1` to make an unreachable database a failure.
 */
const connectionString =
	process.env.DATABASE_URL ?? 'postgres://sentinel:changeme@localhost:5432/sentinel';
const required = process.env.ENVIRONMENT_ROUTES_INTEGRATION_REQUIRED === '1';

let dbReachable = false;
const probeSql = postgres(connectionString, { connect_timeout: 3, max: 1 });
try {
	await probeSql`select 1`;
	dbReachable = true;
} catch {
	dbReachable = false;
} finally {
	await probeSql.end({ timeout: 1 }).catch(() => {});
}

if (required && !dbReachable) {
	throw new Error(
		`ENVIRONMENT_ROUTES_INTEGRATION_REQUIRED=1 but no database answered at ${connectionString}.`
	);
}

const suffix = randomUUID().slice(0, 8);
const orgId = randomUUID();
const otherOrgId = randomUUID();
const projectId = randomUUID();
const secondProjectId = randomUUID();
const otherOrgProjectId = randomUUID();

const ownerId = randomUUID();
const memberId = randomUUID(); // the one removed / invited
const inviteeId = randomUUID(); // the one accepting invitations

function locals(userId: string | null) {
	return { auth: async () => (userId ? { user: { id: userId } } : null) } as never;
}

describe.skipIf(!dbReachable)('grant lifecycle (integration, real Postgres)', () => {
	let sql: ReturnType<typeof postgres>;

	beforeAll(async () => {
		sql = postgres(connectionString, { connect_timeout: 5, max: 1 });

		for (const [id, label] of [
			[orgId, 'grantlife'],
			[otherOrgId, 'grantlife-other'],
		] as const) {
			await sql`insert into organizations (id, name, slug) values (${id}, ${`${label}-${suffix}`}, ${`${label}-${suffix}`})`;
		}
		for (const [id, org, label] of [
			[projectId, orgId, 'p1'],
			[secondProjectId, orgId, 'p2'],
			[otherOrgProjectId, otherOrgId, 'po'],
		] as const) {
			await sql`
				insert into projects (id, name, organization_id, api_key, api_key_hash)
				values (${id}, ${`grantlife-${label}-${suffix}`}, ${org}, ${`k_${label}_${suffix}`}, ${`h_${label}_${suffix}`})
			`;
		}
		await sql`insert into "user" (id, name, email) values (${ownerId}, ${'Owner'}, ${`gl-owner-${suffix}@example.test`})`;
		await sql`insert into "user" (id, name, email) values (${memberId}, ${'Member'}, ${`gl-member-${suffix}@example.test`})`;
		await sql`insert into "user" (id, name, email) values (${inviteeId}, ${'Invitee'}, ${`gl-invitee-${suffix}@example.test`})`;
		await sql`insert into organization_members (organization_id, user_id, role) values (${orgId}, ${ownerId}, ${'owner'})`;
		await sql`insert into organization_members (organization_id, user_id, role) values (${otherOrgId}, ${ownerId}, ${'owner'})`;
	});

	afterAll(async () => {
		if (!sql) return;
		try {
			await sql`delete from project_environment_grants where user_id in (${memberId}, ${inviteeId}, ${ownerId})`;
			await sql`delete from organization_invitations where organization_id in (${orgId}, ${otherOrgId})`;
			await sql`delete from project_environments where project_id in (${projectId}, ${secondProjectId}, ${otherOrgProjectId})`;
			await sql`delete from organization_members where organization_id in (${orgId}, ${otherOrgId})`;
			await sql`delete from projects where organization_id in (${orgId}, ${otherOrgId})`;
			await sql`delete from user_session_preferences where user_id in (${memberId}, ${inviteeId})`;
			await sql`delete from "user" where id in (${ownerId}, ${memberId}, ${inviteeId})`;
			await sql`delete from organizations where id in (${orgId}, ${otherOrgId})`;
		} finally {
			await sql.end({ timeout: 5 }).catch(() => {});
		}
	});

	beforeEach(async () => {
		vi.restoreAllMocks();
		await sql`delete from project_environment_grants where user_id in (${memberId}, ${inviteeId}, ${ownerId})`;
		await sql`delete from organization_invitations where organization_id in (${orgId}, ${otherOrgId})`;
		await sql`delete from organization_members where organization_id = ${orgId} and user_id in (${memberId}, ${inviteeId})`;
		await sql`delete from project_environments where project_id in (${projectId}, ${secondProjectId}, ${otherOrgProjectId})`;
		// Every scenario below narrows this member in one of this org's projects and in another
		// organization's project, so the delete's scoping is observable rather than assumed.
		await sql`insert into organization_members (organization_id, user_id, role) values (${orgId}, ${memberId}, ${'engineer'})`;
		await sql`
			insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${memberId}, ${projectId}, ${sql.array(['staging'])}, ${ownerId})
		`;
		await sql`
			insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${memberId}, ${otherOrgProjectId}, ${sql.array(['staging'])}, ${ownerId})
		`;
	});

	// ------------------------------------------------- decision 40, criterion 3 (first half)

	it('removing an org member deletes their grants for that org\'s projects, and leaves other organizations\' grants alone', async () => {
		const { DELETE } = await import('./[orgId=uuid]/members/[memberId]/+server');
		const res = await DELETE({
			params: { orgId, memberId },
			locals: locals(ownerId),
		} as never);
		expect(res.status).toBe(200);

		const remaining = await sql<{ project_id: string }[]>`
			select project_id from project_environment_grants where user_id = ${memberId}
		`;
		// The org's project is gone; the OTHER organization's is not — the delete is scoped to
		// "projects of this organization", not to the user.
		expect(remaining.map((r) => r.project_id)).toEqual([otherOrgProjectId]);

		const membership = await sql`
			select 1 from organization_members where organization_id = ${orgId} and user_id = ${memberId}
		`;
		expect(membership.length).toBe(0);
	});

	it('a ROLLBACK of that transaction leaves BOTH the membership and the grants intact', async () => {
		const { db } = await import('$lib/server/db');
		const realTransaction = db.transaction.bind(db);

		// Wrap the route's real transaction: run everything it does, then abort. Nothing the route
		// wrote may survive -- neither the membership delete nor the grant delete -- which is the
		// claim "in the SAME transaction" makes and the one a happy-path test cannot see.
		class Abort extends Error {}
		const spy = vi.spyOn(db, 'transaction').mockImplementation((async (cb: never) => {
			return await realTransaction(async (tx) => {
				await (cb as unknown as (tx: unknown) => Promise<unknown>)(tx);
				throw new Abort('rollback');
			});
		}) as never);

		const { DELETE } = await import('./[orgId=uuid]/members/[memberId]/+server');
		await expect(
			DELETE({ params: { orgId, memberId }, locals: locals(ownerId) } as never)
		).rejects.toBeInstanceOf(Abort);
		spy.mockRestore();

		const membership = await sql`
			select 1 from organization_members where organization_id = ${orgId} and user_id = ${memberId}
		`;
		expect(membership.length).toBe(1);

		const grants = await sql<{ project_id: string }[]>`
			select project_id from project_environment_grants where user_id = ${memberId} order by project_id
		`;
		expect(grants.map((r) => r.project_id).sort()).toEqual(
			[projectId, otherOrgProjectId].sort()
		);
	});

	// -------------------------------------------------- §4.10a, criterion 3 (second half)

	async function invite(environmentScopes?: unknown) {
		const { POST } = await import('./[orgId=uuid]/invitations/+server');
		const res = await POST({
			params: { orgId },
			request: new Request('http://x', {
				method: 'POST',
				body: JSON.stringify({
					email: `gl-invitee-${suffix}@example.test`,
					role: 'engineer',
					...(environmentScopes === undefined ? {} : { environmentScopes }),
				}),
			}),
			locals: locals(ownerId),
			url: new URL('http://x/api/organizations/o/invitations'),
		} as never);
		return res;
	}

	/** The raw token never leaves the POST, by design (D06) — mint one the same way it does. */
	async function inviteAndClaim(environmentScopes?: unknown) {
		const res = await invite(environmentScopes);
		expect(res.status).toBe(201);

		const { hashInvitationToken, claimInvitation } = await import('$lib/db/queries/organizations');
		const token = randomUUID() + randomUUID();
		await sql`
			update organization_invitations set token_hash = ${hashInvitationToken(token)}
			where organization_id = ${orgId} and status = 'pending'
		`;
		return await claimInvitation(token, inviteeId);
	}

	async function declareEnv(on: string, name: string) {
		const { POST } = await import('./[orgId=uuid]/projects/[projectId=uuid]/environments/+server');
		const res = await POST({
			params: { orgId, projectId: on },
			request: new Request('http://x', { method: 'POST', body: JSON.stringify({ name }) }),
			locals: locals(ownerId),
		} as never);
		expect(res.status).toBe(201);
	}

	it('THE ABSENCE CLAIM: an invitation with no environmentScopes writes NO grant row and the accepted member is unrestricted', async () => {
		const claim = await inviteAndClaim();
		expect(claim.ok).toBe(true);

		const [row] = await sql<{ environment_scopes: unknown }[]>`
			select environment_scopes from organization_invitations where organization_id = ${orgId}
		`;
		expect(row.environment_scopes).toBeNull();

		const grants = await sql`select 1 from project_environment_grants where user_id = ${inviteeId}`;
		expect(grants.length).toBe(0);

		const { resolveEnvironmentScope } = await import('$lib/server/environment-access');
		expect(await resolveEnvironmentScope(inviteeId, projectId)).toBeNull();
		expect(await resolveEnvironmentScope(inviteeId, secondProjectId)).toBeNull();
	});

	it('a scoped invitation writes one grant row per entry, in the same transaction as the membership', async () => {
		await declareEnv(projectId, 'staging');
		await declareEnv(secondProjectId, 'production');

		const claim = await inviteAndClaim([
			{ projectId, environments: ['staging'] },
			{ projectId: secondProjectId, environments: ['production'] },
		]);
		expect(claim.ok).toBe(true);

		const grants = await sql<{ project_id: string; environments: string[]; created_by: string }[]>`
			select project_id, environments, created_by from project_environment_grants
			where user_id = ${inviteeId} order by project_id
		`;
		expect(grants.length).toBe(2);
		const byProject = new Map(grants.map((g) => [g.project_id, g]));
		expect(byProject.get(projectId)?.environments).toEqual(['staging']);
		expect(byProject.get(secondProjectId)?.environments).toEqual(['production']);
		// created_by is the INVITER, not the accepting user: the person who chose the narrowing.
		expect(byProject.get(projectId)?.created_by).toBe(ownerId);

		// The membership landed too — the grants are written inside claimInvitation's transaction,
		// so either both are there or neither is.
		const membership = await sql`
			select 1 from organization_members where organization_id = ${orgId} and user_id = ${inviteeId}
		`;
		expect(membership.length).toBe(1);
	});

	it('a STALE entry — the project is gone by acceptance time — is skipped, not fatal', async () => {
		await declareEnv(projectId, 'staging');
		await declareEnv(secondProjectId, 'production');

		const res = await invite([
			{ projectId, environments: ['staging'] },
			{ projectId: secondProjectId, environments: ['production'] },
		]);
		expect(res.status).toBe(201);

		// The project is deleted AFTER the invitation is sent. project_environments cascades with it.
		await sql`delete from projects where id = ${secondProjectId}`;

		const { hashInvitationToken, claimInvitation } = await import('$lib/db/queries/organizations');
		const token = randomUUID() + randomUUID();
		await sql`
			update organization_invitations set token_hash = ${hashInvitationToken(token)}
			where organization_id = ${orgId} and status = 'pending'
		`;
		const claim = await claimInvitation(token, inviteeId);

		// The acceptance SUCCEEDS. An invitation that refuses to be accepted because of a settings
		// change made after it was sent is worse than a member who is wider than intended.
		expect(claim.ok).toBe(true);

		const grants = await sql<{ project_id: string }[]>`
			select project_id from project_environment_grants where user_id = ${inviteeId}
		`;
		expect(grants.map((g) => g.project_id)).toEqual([projectId]);

		// Restore the project for the other tests' beforeEach.
		await sql`
			insert into projects (id, name, organization_id, api_key, api_key_hash)
			values (${secondProjectId}, ${`grantlife-p2-${suffix}`}, ${orgId}, ${`k_p2b_${suffix}`}, ${`h_p2b_${suffix}`})
		`;
	});

	it('acceptance is an UPSERT: an invitee who already holds a grant in that project can still join', async () => {
		await declareEnv(projectId, 'staging');
		await sql`
			insert into project_environment_grants (user_id, project_id, environments, created_by)
			values (${inviteeId}, ${projectId}, ${sql.array(['production'])}, ${ownerId})
		`;

		const claim = await inviteAndClaim([{ projectId, environments: ['staging'] }]);
		// A plain INSERT would raise 23505 inside the membership transaction and the person could
		// not join at all. The invitation's scope wins.
		expect(claim.ok).toBe(true);

		const [row] = await sql<{ environments: string[] }[]>`
			select environments from project_environment_grants
			where user_id = ${inviteeId} and project_id = ${projectId}
		`;
		expect(row.environments).toEqual(['staging']);
	});

	it('§4.10a POST validation: unknown environment 400, duplicate project 400, org owner 422, foreign project 404', async () => {
		await declareEnv(projectId, 'staging');

		await expect(invite([{ projectId, environments: ['prodution'] }])).rejects.toMatchObject({
			status: 400,
			body: { message: 'Unknown environment: prodution' },
		});
		await expect(invite([{ projectId, environments: [] }])).rejects.toMatchObject({
			status: 400,
			body: { message: 'environments must contain at least one environment' },
		});
		await expect(
			invite([
				{ projectId, environments: ['staging'] },
				{ projectId, environments: ['staging'] },
			])
		).rejects.toMatchObject({
			status: 400,
			body: { message: 'environmentScopes names the same project twice' },
		});
		// Decision 56: an invitation's environmentScopes may name only that organization's projects.
		await expect(
			invite([{ projectId: otherOrgProjectId, environments: ['staging'] }])
		).rejects.toMatchObject({ status: 404 });

		const { POST } = await import('./[orgId=uuid]/invitations/+server');
		await expect(
			POST({
				params: { orgId },
				request: new Request('http://x', {
					method: 'POST',
					body: JSON.stringify({
						email: `gl-owner-invite-${suffix}@example.test`,
						role: 'owner',
						environmentScopes: [{ projectId, environments: ['staging'] }],
					}),
				}),
				locals: locals(ownerId),
				url: new URL('http://x/i'),
			} as never)
		).rejects.toMatchObject({
			status: 422,
			body: { message: 'An organization owner cannot be restricted to specific environments' },
		});
	});
});
