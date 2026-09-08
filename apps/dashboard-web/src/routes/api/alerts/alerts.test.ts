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
	alertConfigs: {
		id: 'id',
		organizationId: 'organizationId',
		projectId: 'projectId',
		channel: 'channel',
		channelConfig: 'channelConfig',
		frequencyThreshold: 'frequencyThreshold',
		frequencyWindowSeconds: 'frequencyWindowSeconds',
		enabled: 'enabled',
		environments: 'environments',
		createdAt: 'createdAt',
	},
	projectEnvironments: {
		projectId: 'projectId',
		name: 'name',
		hidden: 'hidden',
		isDefault: 'isDefault',
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

// sentinel-project-environments §4.7's two scope resolvers are mocked rather than driven through the
// db double, for the reason §11 gives for keeping them separate boundaries: they have their own
// tests (environment-access.integration.test.ts, and the §4.3a route), and threading their queries
// through this file's positional `then` queue would make every alert test depend on the internal
// query order of a module it is not testing. What IS asserted here is which one is consulted for
// which config scope, and with what arguments — decision 26's whole content.
const resolveEnvironmentScope = vi.fn(async (_userId: string, _projectId: string) => null as string[] | null);
vi.mock('$lib/server/environment-access', () => ({
	resolveEnvironmentScope: (...args: [string, string]) => resolveEnvironmentScope(...args),
}));

const resolveEnvironmentUnion = vi.fn(async (_userId: string, _organizationId?: string) => [] as string[]);
vi.mock('$lib/server/environment-union', () => ({
	resolveEnvironmentUnion: (...args: [string, string?]) => resolveEnvironmentUnion(...args),
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
		dbMock.then.mockReset();
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
		// decision 11: `environments` was omitted, so the route looks up the project's is_default
		// environment before inserting.
		queueResults([{ name: 'production' }]);
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
		dbMock.then.mockReset();
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
		dbMock.then.mockReset();
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

// ---------------------------------------------------------------------------
// sentinel-project-environments §4.7 — environment scoping (flow E9).
// ---------------------------------------------------------------------------

describe('POST /api/alerts — decision 11: the default scope for a NEW config', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.insert.mockReturnValue(dbMock);
		dbMock.values.mockReturnValue(dbMock);
		dbMock.returning.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
		resolveEnvironmentScope.mockReset();
		resolveEnvironmentScope.mockResolvedValue(null);
		resolveEnvironmentUnion.mockReset();
		resolveEnvironmentUnion.mockResolvedValue([]);
	});

	function postRequest(body: unknown) {
		return new Request('http://x/api/alerts', { method: 'POST', body: JSON.stringify(body) });
	}

	function insertedRow(overrides: Record<string, unknown> = {}) {
		return {
			id: 'cfg-new',
			organizationId: ORG_ID,
			projectId: PROJECT_ID,
			channel: 'email',
			channelConfig: { to: 'a@b.com' },
			frequencyThreshold: 50,
			frequencyWindowSeconds: 60,
			enabled: true,
			environments: null,
			createdAt: null,
			...overrides,
		};
	}

	it('a new config in a project WITH a default is scoped to it, and reports no fallback', async () => {
		queueResults([projectMembershipRow('developer')]);
		queueResults([{ organizationId: ORG_ID }]);
		queueResults([{ name: 'production' }]); // the project's is_default environment
		queueResults([insertedRow({ environments: ['production'] })]);

		const res = await POST({
			request: postRequest({ projectId: PROJECT_ID, channel: 'email', channelTarget: 'a@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(201);
		expect(dbMock.values).toHaveBeenCalledWith(expect.objectContaining({ environments: ['production'] }));
		const body = await res.json();
		expect(body.environments).toEqual(['production']);
		// The notice is an EVENT and only reports the FALLBACK — a config that was scoped to the
		// default must not carry it, or the page would warn about the case that worked.
		expect(body).not.toHaveProperty('defaultEnvironmentApplied');
	});

	it('a new config in a project WITHOUT a default covers every environment and says so, once', async () => {
		queueResults([projectMembershipRow('developer')]);
		queueResults([{ organizationId: ORG_ID }]);
		queueResults([]); // no is_default row for this project
		queueResults([insertedRow({ environments: null })]);

		const res = await POST({
			request: postRequest({ projectId: PROJECT_ID, channel: 'email', channelTarget: 'a@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(201);
		// NULL, not []: §4.7 refuses an empty scope because a config that can never fire is a config
		// somebody thinks is working.
		expect(dbMock.values).toHaveBeenCalledWith(expect.objectContaining({ environments: null }));
		const body = await res.json();
		expect(body.environments).toBeNull();
		expect(body.defaultEnvironmentApplied).toBe(false);
	});

	it('an org-wide config covers every environment and carries no notice — that is not a fallback', async () => {
		queueResults([orgMembershipRow('owner')]);
		queueResults([insertedRow({ id: 'cfg-org-new', projectId: null, environments: null })]);

		const res = await POST({
			request: postRequest({ organizationId: ORG_ID, channel: 'email', channelTarget: 'a@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(201);
		const body = await res.json();
		expect(body.environments).toBeNull();
		// There is no single project and therefore no single default; deriving one from every
		// project's default would produce a list that changes as projects are added.
		expect(body).not.toHaveProperty('defaultEnvironmentApplied');
	});

	it('an explicitly supplied value is never overridden by the project default', async () => {
		queueResults([projectMembershipRow('developer')]);
		queueResults([{ organizationId: ORG_ID }]);
		queueResults([insertedRow({ environments: ['staging'] })]);

		const res = await POST({
			request: postRequest({
				projectId: PROJECT_ID,
				channel: 'email',
				channelTarget: 'a@b.com',
				environments: ['staging'],
			}),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(201);
		expect(dbMock.values).toHaveBeenCalledWith(expect.objectContaining({ environments: ['staging'] }));
	});

	it('rejects [] with §7\'s message rather than storing a config that can never fire', async () => {
		const res = await POST({
			request: postRequest({
				projectId: PROJECT_ID,
				channel: 'email',
				channelTarget: 'a@b.com',
				environments: [],
			}),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect((await res.json()).error).toBe('environments must be null or a non-empty list');
		expect(dbMock.insert).not.toHaveBeenCalled();
	});

	it('rejects a name outside the caller\'s grant on a project-scoped config', async () => {
		resolveEnvironmentScope.mockResolvedValue(['staging']);
		queueResults([projectMembershipRow('developer')]);
		queueResults([{ organizationId: ORG_ID }]);

		const res = await POST({
			request: postRequest({
				projectId: PROJECT_ID,
				channel: 'email',
				channelTarget: 'a@b.com',
				environments: ['production'],
			}),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect((await res.json()).error).toBe(
			'environments contains an environment you do not have access to'
		);
		expect(dbMock.insert).not.toHaveBeenCalled();
	});
});

describe('PUT /api/alerts — decision 32/48: the patch rule, the guard and the merge', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.update.mockReturnValue(dbMock);
		dbMock.set.mockReturnValue(dbMock);
		dbMock.returning.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
		resolveEnvironmentScope.mockReset();
		resolveEnvironmentScope.mockResolvedValue(null);
		resolveEnvironmentUnion.mockReset();
		resolveEnvironmentUnion.mockResolvedValue([]);
	});

	const projectConfig = (environments: string[] | null) => ({
		id: 'cfg-proj-1',
		organizationId: ORG_ID,
		projectId: PROJECT_ID,
		channel: 'email',
		channelConfig: { to: 'proj@b.com' },
		frequencyThreshold: 50,
		frequencyWindowSeconds: 60,
		enabled: true,
		environments,
		createdAt: null,
	});

	const orgConfig = (environments: string[] | null) => ({
		...projectConfig(environments),
		id: 'cfg-org-1',
		projectId: null,
	});

	function putRequest(body: unknown) {
		return new Request('http://x/api/alerts', { method: 'PUT', body: JSON.stringify(body) });
	}

	function setPayload() {
		return dbMock.set.mock.calls.at(-1)?.[0] ?? {};
	}

	it('leaves environments alone when the key is omitted, and does not run the guard', async () => {
		queueResults([projectConfig(['production'])]);
		queueResults([projectMembershipRow('developer')]);
		queueResults([projectConfig(['production'])]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-proj-1', channelTarget: 'new@b.com' }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
		// `?? existing` would also have "worked" here and would be wrong: it cannot tell "leave
		// unchanged" from "set to every environment". The key must be ABSENT from the update.
		expect(setPayload()).not.toHaveProperty('environments');
		// A guard on a change nobody requested would fail a save of an unrelated field.
		expect(resolveEnvironmentScope).not.toHaveBeenCalled();
		expect(resolveEnvironmentUnion).not.toHaveBeenCalled();
	});

	it('stores null — every environment — when the key is explicitly null, unguarded by design', async () => {
		resolveEnvironmentScope.mockResolvedValue(['staging']);
		queueResults([projectConfig(['production'])]);
		queueResults([projectMembershipRow('developer')]);
		queueResults([projectConfig(null)]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-proj-1', environments: null }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
		expect(setPayload().environments).toBeNull();
		// §4.7 calls this asymmetry intended: a member narrowed to {staging} may widen a config to
		// every environment by sending null, while naming `production` explicitly is a 400. Refusing
		// null from a scoped caller would make "All environments" unreachable from the picker for
		// exactly the people most likely to be narrowed.
		expect(resolveEnvironmentScope).not.toHaveBeenCalled();
	});

	it('merges: a stored name the caller could not have been offered survives, an offerable one they dropped does not', async () => {
		// The caller is narrowed to {staging, eu-west-1}; the project registry offers exactly those
		// two. `production` is stored on the config and is NOT offerable to them — hidden,
		// unregistered or outside their grant, §4.7 lists all three as the same case.
		resolveEnvironmentScope.mockResolvedValue(['staging', 'eu-west-1']);
		queueResults([projectConfig(['production', 'staging', 'eu-west-1'])]);
		queueResults([projectMembershipRow('developer')]);
		queueResults([{ name: 'staging' }, { name: 'eu-west-1' }]); // offerable, per §4.1
		queueResults([projectConfig(['staging', 'production'])]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-proj-1', environments: ['staging'] }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
		const stored = setPayload().environments as string[];
		expect(stored).toContain('staging');
		// Preserved: a client that posted back what its picker showed would otherwise silently widen
		// this config back to paging for production.
		expect(stored).toContain('production');
		// Removed: this one WAS offerable and the caller did not send it. That is a real deselection
		// and it has to work, or the picker cannot narrow anything.
		expect(stored).not.toContain('eu-west-1');
	});

	it('accepts a stored name the caller cannot reach when they simply re-submit it (decision 48)', async () => {
		resolveEnvironmentScope.mockResolvedValue(['staging']);
		queueResults([projectConfig(['production', 'staging'])]);
		queueResults([projectMembershipRow('developer')]);
		queueResults([{ name: 'staging' }]); // offerable
		queueResults([projectConfig(['production', 'staging'])]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-proj-1', environments: ['production', 'staging'] }),
			locals: locals('user-1'),
		} as any);

		// The guarded set is `supplied \ existing`. This caller was SHOWN both names by the merged
		// response the contract told the client to render; answering their re-submission with a 400
		// would leave them no way to edit this config from the picker at all.
		expect(res.status).toBe(200);
	});

	it('rejects a name that is NEW to the config and outside the caller\'s project grant', async () => {
		resolveEnvironmentScope.mockResolvedValue(['staging']);
		queueResults([projectConfig(['staging'])]);
		queueResults([projectMembershipRow('developer')]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-proj-1', environments: ['staging', 'production'] }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect((await res.json()).error).toBe(
			'environments contains an environment you do not have access to'
		);
		expect(dbMock.update).not.toHaveBeenCalled();
		expect(resolveEnvironmentScope).toHaveBeenCalledWith('user-1', PROJECT_ID);
	});

	it('a caller with no grant row (unrestricted) may scope a project config to anything', async () => {
		resolveEnvironmentScope.mockResolvedValue(null);
		queueResults([projectConfig(null)]);
		queueResults([projectMembershipRow('developer')]);
		queueResults([projectConfig(['production'])]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-proj-1', environments: ['production'] }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
		expect(setPayload().environments).toEqual(['production']);
	});

	it('an ORG-WIDE config is checked against §4.3a\'s union, not against a project grant', async () => {
		resolveEnvironmentUnion.mockResolvedValue(['production', 'staging']);
		queueResults([orgConfig(null)]);
		queueResults([orgMembershipRow('owner')]);
		queueResults([orgConfig(['production'])]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-org-1', environments: ['production'] }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(200);
		// `resolveEnvironmentScope` is keyed (user_id, project_id) and an org-wide config names no
		// project — the sentence this replaces had no resolvable subject at all (decision 26).
		expect(resolveEnvironmentScope).not.toHaveBeenCalled();
		expect(resolveEnvironmentUnion).toHaveBeenCalledWith('user-1', ORG_ID);
	});

	it('an ORG-WIDE config rejects a name outside that union', async () => {
		resolveEnvironmentUnion.mockResolvedValue(['staging']);
		queueResults([orgConfig(null)]);
		queueResults([orgMembershipRow('owner')]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-org-1', environments: ['production'] }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect((await res.json()).error).toBe(
			'environments contains an environment you do not have access to'
		);
		expect(dbMock.update).not.toHaveBeenCalled();
	});

	it('returns the MERGED array so the picker re-renders what is actually stored', async () => {
		resolveEnvironmentScope.mockResolvedValue(null);
		queueResults([projectConfig(['production'])]);
		queueResults([projectMembershipRow('developer')]);
		queueResults([{ name: 'staging' }]);
		queueResults([projectConfig(['staging', 'production'])]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-proj-1', environments: ['staging'] }),
			locals: locals('user-1'),
		} as any);

		const body = await res.json();
		expect(body.environments).toEqual(['staging', 'production']);
	});

	it('rejects [] on update for the same reason it rejects it on create', async () => {
		queueResults([projectConfig(['production'])]);
		queueResults([projectMembershipRow('developer')]);

		const res = await PUT({
			request: putRequest({ id: 'cfg-proj-1', environments: [] }),
			locals: locals('user-1'),
		} as any);

		expect(res.status).toBe(400);
		expect((await res.json()).error).toBe('environments must be null or a non-empty list');
		expect(dbMock.update).not.toHaveBeenCalled();
	});
});

describe('GET /api/alerts — the stored scope is on every returned row', () => {
	beforeEach(() => {
		vi.clearAllMocks();
		dbMock.select.mockReturnValue(dbMock);
		dbMock.from.mockReturnValue(dbMock);
		dbMock.where.mockReturnValue(dbMock);
		dbMock.then.mockReset();
		dbMock.then.mockImplementation((resolve: any) => resolve([]));
	});

	it('returns null for an unscoped row and the array for a scoped one', async () => {
		queueResults([{ projectId: PROJECT_ID }]);
		queueResults([{ organizationId: ORG_ID }]);
		queueResults([
			{
				id: 'cfg-unscoped',
				organizationId: ORG_ID,
				projectId: PROJECT_ID,
				channel: 'email',
				channelConfig: { to: 'a@b.com' },
				frequencyThreshold: 50,
				frequencyWindowSeconds: 60,
				enabled: true,
				environments: null,
				createdAt: null,
			},
			{
				id: 'cfg-scoped',
				organizationId: ORG_ID,
				projectId: PROJECT_ID,
				channel: 'email',
				channelConfig: { to: 'b@b.com' },
				frequencyThreshold: 50,
				frequencyWindowSeconds: 60,
				enabled: true,
				environments: ['production', 'eu-west-1'],
				createdAt: null,
			},
		]);

		const res = await GET({ locals: locals('user-1') } as any);
		const byId = Object.fromEntries((await res.json()).map((c: any) => [c.id, c]));

		// null, never [] — the two mean opposite things and only one of them can be stored.
		expect(byId['cfg-unscoped'].environments).toBeNull();
		expect(byId['cfg-scoped'].environments).toEqual(['production', 'eu-west-1']);
	});
});
