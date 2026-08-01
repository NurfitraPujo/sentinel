import { describe, it, expect, vi, beforeEach } from 'vitest';

// A chainable Drizzle-query double, mirroring src/routes/api/organizations/keys.test.ts's approach:
// select/insert/update/delete chains all resolve through the same queued `then` implementation, so
// each test queues one mockImplementationOnce per db round-trip the route under test is expected to
// make, in order.
function makeDbMock() {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		insert: vi.fn(),
		values: vi.fn(),
		returning: vi.fn(),
		update: vi.fn(),
		set: vi.fn(),
		delete: vi.fn(),
		then: vi.fn(),
	};
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.insert.mockReturnValue(dbMock);
	dbMock.values.mockReturnValue(dbMock);
	dbMock.returning.mockReturnValue(dbMock);
	dbMock.update.mockReturnValue(dbMock);
	dbMock.set.mockReturnValue(dbMock);
	dbMock.delete.mockReturnValue(dbMock);
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

vi.mock('$lib/server/db', () => ({ db: dbMock }));

vi.mock('$lib/db/schema', () => ({
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
	projectMembers: {
		projectId: 'projectId',
		userId: 'userId',
		role: 'role',
	},
	projects: {
		id: 'id',
		organizationId: 'organizationId',
	},
	organizationMembers: {
		organizationId: 'organizationId',
		userId: 'userId',
		role: 'role',
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

const natsPublish = vi.fn().mockResolvedValue(undefined);
vi.mock('$lib/db/queries/apikeys', () => ({
	createNatsPublisher: vi.fn(() => ({ publish: natsPublish })),
}));

const { GET, POST, PUT, DELETE } = await import('./+server');

function locals(userId: string | null) {
	return { auth: async () => (userId ? { user: { id: userId } } : null) } as any;
}

function projectMembershipRow(role: string) {
	return { role };
}

function orgMembershipRow(role: string) {
	return { role };
}

function queueResults(...results: unknown[]) {
	for (const result of results) {
		dbMock.then.mockImplementationOnce((resolve: any) => resolve(result));
	}
}

const ORG_ID = 'org-1';
const PROJECT_ID = 'proj-1';

describe('POST /api/alerts — authorization split between org-wide and project-scoped configs', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.insert.mockReturnValue(dbMock);
		dbMock.values.mockReturnValue(dbMock);
		dbMock.returning.mockReturnValue(dbMock);
		dbMock.update.mockReturnValue(dbMock);
		dbMock.set.mockReturnValue(dbMock);
		dbMock.delete.mockReturnValue(dbMock);
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	function postRequest(body: unknown) {
		return new Request('http://x/api/alerts', { method: 'POST', body: JSON.stringify(body) });
	}

	it('a project-level-only member (developer, has "write") CAN create a project-scoped config', async () => {
		// requireProjectAlertAccess: project membership lookup
		queueResults([projectMembershipRow('developer')]);
		// organizationId lookup for the named project
		queueResults([{ organizationId: ORG_ID }]);
		// insert...returning
		queueResults([
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
		]);

		const res = await POST({
			request: postRequest({ projectId: PROJECT_ID, channel: 'email', channelTarget: 'a@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(201);
		const body = await res.json();
		expect(body.scope).toBe('project');
		expect(body.projectId).toBe(PROJECT_ID);
	});

	it('a project-level-only member CANNOT create an org-wide config (no organization membership at all)', async () => {
		// requireOrgAlertAccess: no membership row found for this org
		queueResults([]);

		const res = await POST({
			request: postRequest({ organizationId: ORG_ID, channel: 'email', channelTarget: 'a@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(403);
		expect(dbMock.insert).not.toHaveBeenCalled();
	});

	it('an org member whose role only grants read (support) CANNOT create an org-wide config', async () => {
		queueResults([orgMembershipRow('support')]);

		const res = await POST({
			request: postRequest({ organizationId: ORG_ID, channel: 'email', channelTarget: 'a@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(403);
		expect(dbMock.insert).not.toHaveBeenCalled();
	});

	it.each(['owner', 'admin', 'engineer'])(
		'an org-level member with role %s CAN create an org-wide config',
		async (role) => {
			queueResults([orgMembershipRow(role)]);
			queueResults([
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
			]);

			const res = await POST({
				request: postRequest({ organizationId: ORG_ID, channel: 'email', channelTarget: 'org@b.com' }),
				locals: locals('user-1'),
			} as any);

			expect(res.status).toBe(201);
			const body = await res.json();
			expect(body.scope).toBe('organization');
			expect(body.projectId).toBeNull();
			expect(body.organizationId).toBe(ORG_ID);
			// projectId is '' for the org-wide NATS payload, per the documented unknown/bulk convention.
			expect(natsPublish).toHaveBeenCalledWith('alert_config.changed', { projectId: '', configId: 'cfg-org-1' });
		}
	);

	it('rejects an org-wide request with no organizationId', async () => {
		const res = await POST({
			request: postRequest({ channel: 'email', channelTarget: 'a@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect(dbMock.insert).not.toHaveBeenCalled();
	});
});

describe('PUT/DELETE /api/alerts — org-wide configs cannot be touched by project-only rights', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.insert.mockReturnValue(dbMock);
		dbMock.values.mockReturnValue(dbMock);
		dbMock.returning.mockReturnValue(dbMock);
		dbMock.update.mockReturnValue(dbMock);
		dbMock.set.mockReturnValue(dbMock);
		dbMock.delete.mockReturnValue(dbMock);
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	const orgWideConfig = {
		id: 'cfg-org-1',
		organizationId: ORG_ID,
		projectId: null,
		channel: 'email',
		channelConfig: { to: 'org@b.com' },
		frequencyThreshold: 50,
		frequencyWindowSeconds: 60,
		enabled: true,
		createdAt: null,
	};

	function mutationRequest(method: 'PUT' | 'DELETE', body: unknown) {
		return new Request('http://x/api/alerts', { method, body: JSON.stringify(body) });
	}

	it('PUT: a user with ONLY a project-level role (no org membership) cannot edit an org-wide config', async () => {
		queueResults([orgWideConfig]); // existing config lookup
		queueResults([]); // requireOrgAlertAccess: no org membership found

		const res = await PUT({
			request: mutationRequest('PUT', { id: 'cfg-org-1', channelTarget: 'new@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(403);
		expect(dbMock.update).not.toHaveBeenCalled();
	});

	it('PUT: an org-level member with manage_keys (engineer) CAN edit an org-wide config', async () => {
		queueResults([orgWideConfig]); // existing config lookup
		queueResults([orgMembershipRow('engineer')]); // requireOrgAlertAccess
		queueResults([{ ...orgWideConfig, channelConfig: { to: 'new@b.com' } }]); // update...returning

		const res = await PUT({
			request: mutationRequest('PUT', { id: 'cfg-org-1', channelTarget: 'new@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body.scope).toBe('organization');
		expect(body.channelTarget).toBe('new@b.com');
	});

	it('DELETE: a user with ONLY a project-level role cannot delete an org-wide config', async () => {
		queueResults([orgWideConfig]);
		queueResults([]); // no org membership

		const res = await DELETE({
			request: mutationRequest('DELETE', { id: 'cfg-org-1' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(403);
		expect(dbMock.delete).not.toHaveBeenCalled();
	});

	it('DELETE: an org member without manage_keys (support) cannot delete an org-wide config even though support has "read"', async () => {
		queueResults([orgWideConfig]);
		queueResults([orgMembershipRow('support')]);

		const res = await DELETE({
			request: mutationRequest('DELETE', { id: 'cfg-org-1' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(403);
		expect(dbMock.delete).not.toHaveBeenCalled();
	});

	it('DELETE: an org owner CAN delete an org-wide config', async () => {
		queueResults([orgWideConfig]);
		queueResults([orgMembershipRow('owner')]);
		queueResults(undefined); // delete has no meaningful resolved value

		const res = await DELETE({
			request: mutationRequest('DELETE', { id: 'cfg-org-1' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
		expect(dbMock.delete).toHaveBeenCalled();
	});

	const projectScopedConfig = {
		id: 'cfg-proj-1',
		organizationId: ORG_ID,
		projectId: PROJECT_ID,
		channel: 'email',
		channelConfig: { to: 'proj@b.com' },
		frequencyThreshold: 50,
		frequencyWindowSeconds: 60,
		enabled: true,
		createdAt: null,
	};

	it('PUT: project-scoped configs are unaffected — a project developer can still edit their own project config', async () => {
		queueResults([projectScopedConfig]);
		queueResults([projectMembershipRow('developer')]);
		queueResults([{ ...projectScopedConfig, channelConfig: { to: 'new@b.com' } }]);

		const res = await PUT({
			request: mutationRequest('PUT', { id: 'cfg-proj-1', channelTarget: 'new@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
		const body = await res.json();
		expect(body.scope).toBe('project');
	});

	it('PUT: an org owner with no row in this project cannot edit a project-scoped config via org membership alone', async () => {
		queueResults([projectScopedConfig]);
		queueResults([]); // requireProjectAlertAccess looks up project_members, not organization_members — none found

		const res = await PUT({
			request: mutationRequest('PUT', { id: 'cfg-proj-1', channelTarget: 'new@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(403);
		expect(dbMock.update).not.toHaveBeenCalled();
	});

	// D24: the edit form used to seed the scope toggle from the config and submit it live, while PUT
	// silently discarded projectId/organizationId — a scope-changing edit returned 200 and changed
	// nothing. PUT must now refuse instead of silently no-op-ing.
	const ORG_B = 'org-2';

	it('PUT: rejects a body that tries to move an org-wide config to a different organization', async () => {
		queueResults([orgWideConfig]); // existing config lookup
		queueResults([orgMembershipRow('owner')]); // requireMutationAccess passes for the STORED org

		const res = await PUT({
			request: mutationRequest('PUT', {
				id: 'cfg-org-1',
				organizationId: ORG_B,
				channelTarget: 'new@b.com',
			}),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect(dbMock.update).not.toHaveBeenCalled();
	});

	it('PUT: rejects a body that tries to move a project-scoped config to a different project', async () => {
		queueResults([projectScopedConfig]); // existing config lookup
		queueResults([projectMembershipRow('developer')]); // requireMutationAccess passes for the STORED project

		const res = await PUT({
			request: mutationRequest('PUT', {
				id: 'cfg-proj-1',
				projectId: 'proj-other',
				channelTarget: 'new@b.com',
			}),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect(dbMock.update).not.toHaveBeenCalled();
	});

	it('PUT: rejects a body that tries to convert an org-wide config to a project-scoped one', async () => {
		queueResults([orgWideConfig]);
		queueResults([orgMembershipRow('owner')]);

		const res = await PUT({
			request: mutationRequest('PUT', {
				id: 'cfg-org-1',
				projectId: PROJECT_ID,
				channelTarget: 'new@b.com',
			}),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect(dbMock.update).not.toHaveBeenCalled();
	});

	it('PUT: a same-value scope field (no actual change) is accepted', async () => {
		queueResults([orgWideConfig]);
		queueResults([orgMembershipRow('owner')]);
		queueResults([{ ...orgWideConfig, channelConfig: { to: 'new@b.com' } }]);

		const res = await PUT({
			request: mutationRequest('PUT', {
				id: 'cfg-org-1',
				organizationId: ORG_ID, // same as stored — not a scope change
				channelTarget: 'new@b.com',
			}),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
	});

	// Cross-org IDOR fence: the caller IS a manage_keys member of org A, but tries to create an
	// org-wide config scoped to org B, where they hold no membership at all. requireOrgAlertAccess
	// looks up membership by the SPECIFIC organizationId in the body, not "any org the caller manages",
	// so this must 403 even though the caller has manage_keys somewhere.
	it('POST: an org A owner cannot create an org-wide config for org B where they have no membership (IDOR fence)', async () => {
		// requireOrgAlertAccess looks up organizationMembers for (user-1, ORG_B) specifically — no row.
		queueResults([]);

		const res = await POST({
			request: new Request('http://x/api/alerts', {
				method: 'POST',
				body: JSON.stringify({ organizationId: ORG_B, channel: 'email', channelTarget: 'x@b.com' }),
			}),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(403);
		expect(dbMock.insert).not.toHaveBeenCalled();
	});
});

describe('GET /api/alerts — returns both layers with an explicit scope field', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	it('marks org-wide and project-scoped rows with different explicit scope values', async () => {
		queueResults([{ projectId: PROJECT_ID }]); // project memberships
		queueResults([{ organizationId: ORG_ID }]); // org memberships
		queueResults([
			{
				id: 'cfg-proj-1',
				organizationId: ORG_ID,
				projectId: PROJECT_ID,
				channel: 'email',
				channelConfig: { to: 'proj@b.com' },
				frequencyThreshold: 50,
				frequencyWindowSeconds: 60,
				enabled: true,
				createdAt: null,
			},
			{
				id: 'cfg-org-1',
				organizationId: ORG_ID,
				projectId: null,
				channel: 'telegram',
				channelConfig: { chat_id: '123' },
				frequencyThreshold: 50,
				frequencyWindowSeconds: 60,
				enabled: true,
				createdAt: null,
			},
		]);

		const res = await GET({ locals: locals('user-1') } as any);
		const body = await res.json();

		expect(body).toHaveLength(2);
		const byId = Object.fromEntries(body.map((c: any) => [c.id, c]));
		expect(byId['cfg-proj-1'].scope).toBe('project');
		expect(byId['cfg-proj-1'].projectId).toBe(PROJECT_ID);
		expect(byId['cfg-org-1'].scope).toBe('organization');
		expect(byId['cfg-org-1'].projectId).toBeNull();
	});

	it('returns an empty list when the caller has no project or org memberships, without querying alert_configs', async () => {
		queueResults([]); // project memberships
		queueResults([]); // org memberships

		const res = await GET({ locals: locals('user-1') } as any);
		const body = await res.json();

		expect(body).toEqual([]);
	});
});
