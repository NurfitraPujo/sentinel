import { describe, it, expect, vi, beforeEach } from 'vitest';

// A chainable Drizzle-query double (same pattern as api/alerts/alerts.test.ts and
// organizations/keys.test.ts): every select/join/where chains through to `then`, and each test
// queues one resolved value per db round-trip the loader is expected to make, in order.
function makeDbMock() {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		innerJoin: vi.fn(),
		where: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	// `clearAllMocks` clears call records but NOT queued `mockImplementationOnce` entries, so an
	// un-consumed queued resolution leaks into the NEXT test and answers the wrong query, making
	// results order-dependent. `mockReset` on the queue-bearing mock drops the queue; the base
	// implementation is re-established on the next line.
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
	projects: { id: 'id', name: 'name', organizationId: 'organizationId' },
	projectMembers: { projectId: 'projectId', userId: 'userId', role: 'role' },
	organizationMembers: { organizationId: 'organizationId', userId: 'userId', role: 'role' },
	organizations: { id: 'id', name: 'name' },
	alertConfigs: {
		id: 'id',
		organizationId: 'organizationId',
		projectId: 'projectId',
		channel: 'channel',
		channelConfig: 'channelConfig',
		frequencyThreshold: 'frequencyThreshold',
		frequencyWindowSeconds: 'frequencyWindowSeconds',
		enabled: 'enabled',
		createdAt: 'createdAt',
	},
}));

vi.mock('$lib/server/auth', () => ({
	requireAuth: vi.fn(async ({ locals }: any) => {
		const session = await locals.auth?.();
		if (!session?.user?.id) {
			throw new Error('Authentication required');
		}
		return { id: session.user.id, email: 'user@example.com' };
	}),
}));

const { load } = await import('./+page.server');

function locals(userId: string | null) {
	return { auth: async () => (userId ? { user: { id: userId } } : null) } as any;
}

function queueResults(...results: unknown[]) {
	for (const result of results) {
		dbMock.then.mockImplementationOnce((resolve: any) => resolve(result));
	}
}

function resetDbMock() {
	vi.clearAllMocks();
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
}

const ORG_ID = 'org-1';
const PROJECT_ID = 'proj-1';

// The loader issues its db calls in this fixed order:
//   1. project memberships    2. org memberships
//   3. alert_configs (only if there is any visibility)
//   4. user's projects (only if projectIds.length > 0)
//   5. user's organizations (only if manageableOrgIds.length > 0, per D35)
async function runLoad(opts: {
	projectMemberships?: unknown[];
	orgMemberships?: unknown[];
	alertConfigs?: unknown[];
	userProjects?: unknown[];
	userOrgs?: unknown[];
}) {
	const projectMemberships = opts.projectMemberships ?? [];
	const orgMemberships = opts.orgMemberships ?? [];

	queueResults(projectMemberships);
	queueResults(orgMemberships);

	const hasVisibility = projectMemberships.length > 0 || orgMemberships.length > 0;
	if (hasVisibility) {
		queueResults(opts.alertConfigs ?? []);
	}
	if (projectMemberships.length > 0) {
		queueResults(opts.userProjects ?? []);
	}
	// userOrgs is only queried when at least one membership has manage_keys — callers that expect it
	// must pass an org role that grants manage_keys ('owner'/'admin'/'engineer').
	if (opts.userOrgs !== undefined) {
		queueResults(opts.userOrgs);
	}

	const result: any = await load({ locals: locals('user-1') } as any);
	return result;
}

describe('settings/alerts +page.server.ts load (D13 — exercises the real loader, not a re-implementation)', () => {
	beforeEach(() => {
		resetDbMock();
	});

	it('marks a project-scoped config editable when the caller has "write" on that project', async () => {
		const result = await runLoad({
			projectMemberships: [{ projectId: PROJECT_ID, role: 'developer' }],
			alertConfigs: [
				{
					id: 'cfg-1',
					organizationId: ORG_ID,
					projectId: PROJECT_ID,
					channel: 'email',
					channelConfig: { to: 'a@b.com' },
					frequencyThreshold: 50,
					frequencyWindowSeconds: 60,
					enabled: true,
					createdAt: null,
				},
			],
			userProjects: [{ id: PROJECT_ID, name: 'Proj One', organizationId: ORG_ID }],
		});

		expect(result.editableAlertConfigs.map((c: any) => c.id)).toEqual(['cfg-1']);
	});

	it('does NOT mark a project-scoped config editable when the caller only has "viewer" on that project', async () => {
		const result = await runLoad({
			projectMemberships: [{ projectId: PROJECT_ID, role: 'viewer' }],
			alertConfigs: [
				{
					id: 'cfg-1',
					organizationId: ORG_ID,
					projectId: PROJECT_ID,
					channel: 'email',
					channelConfig: { to: 'a@b.com' },
					frequencyThreshold: 50,
					frequencyWindowSeconds: 60,
					enabled: true,
					createdAt: null,
				},
			],
			userProjects: [{ id: PROJECT_ID, name: 'Proj One', organizationId: ORG_ID }],
		});

		expect(result.editableAlertConfigs).toEqual([]);
	});

	it('marks an org-wide config editable only for org roles that grant manage_keys', async () => {
		const result = await runLoad({
			orgMemberships: [{ organizationId: ORG_ID, role: 'engineer' }],
			alertConfigs: [
				{
					id: 'cfg-org-1',
					organizationId: ORG_ID,
					projectId: null,
					channel: 'email',
					channelConfig: { to: 'org@b.com' },
					frequencyThreshold: 50,
					frequencyWindowSeconds: 60,
					enabled: true,
					createdAt: null,
				},
			],
			userOrgs: [{ id: ORG_ID, name: 'Org One' }],
		});

		expect(result.editableAlertConfigs.map((c: any) => c.id)).toEqual(['cfg-org-1']);
		expect(result.canManageOrgAlerts).toBe(true);
		// D35: userOrganizations is filtered to orgs the caller can actually manage — a plain
		// re-implementation of the filter predicate (the old D13 tests) could not have caught this,
		// since it never touched this field at all.
		expect(result.userOrganizations.map((o: any) => o.id)).toEqual([ORG_ID]);
	});

	it('does NOT mark an org-wide config editable for an org role without manage_keys (support)', async () => {
		const result = await runLoad({
			orgMemberships: [{ organizationId: ORG_ID, role: 'support' }],
			alertConfigs: [
				{
					id: 'cfg-org-1',
					organizationId: ORG_ID,
					projectId: null,
					channel: 'email',
					channelConfig: { to: 'org@b.com' },
					frequencyThreshold: 50,
					frequencyWindowSeconds: 60,
					enabled: true,
					createdAt: null,
				},
			],
		});

		expect(result.editableAlertConfigs).toEqual([]);
		// D35: a 'support' membership does not grant manage_keys, so the org must not appear as a
		// selectable scope target even though the caller is a member of it.
		expect(result.canManageOrgAlerts).toBe(false);
		expect(result.userOrganizations).toEqual([]);
	});

	it('401s (throws) for an unauthenticated caller before touching the database', async () => {
		await expect(load({ locals: locals(null) } as any)).rejects.toThrow('Authentication required');
		expect(dbMock.select).not.toHaveBeenCalled();
	});
});
