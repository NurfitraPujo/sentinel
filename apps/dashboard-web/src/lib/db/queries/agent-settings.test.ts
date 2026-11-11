import { describe, it, expect, vi, beforeEach } from 'vitest';

// N10 part 1 -- co-located unit tests for agent-settings.ts. Mirrors agents.test.ts's
// single-`then`-resolves-the-chain db double, extended with insert/onConflictDoUpdate/delete.
//
// drizzle-orm is mocked (organizations.test.ts's pattern) so `where(eq(col, val))` calls can be
// asserted on their actual arguments instead of merely "was called" -- an unscoped or
// wrongly-scoped delete/update must fail these tests (defects 1 & 2, round 1 review).

vi.mock('drizzle-orm', () => {
	const eq = (col: any, val: any) => ({ __op: 'eq', col, val });
	const inArray = (col: any, vals: any) => ({ __op: 'inArray', col, vals });
	return { eq, inArray };
});

vi.mock('$lib/server/db', () => {
	const dbMock: any = {
		select: vi.fn(),
		from: vi.fn(),
		where: vi.fn(),
		insert: vi.fn(),
		values: vi.fn(),
		onConflictDoUpdate: vi.fn(),
		returning: vi.fn(),
		delete: vi.fn(),
		then: vi.fn(),
	};
	for (const key of ['select', 'from', 'where', 'insert', 'values', 'onConflictDoUpdate', 'returning', 'delete']) {
		dbMock[key].mockReturnValue(dbMock);
	}
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return { db: dbMock };
});

vi.mock('$lib/db/schema', () => ({
	projectAgentSettings: {
		projectId: 'projectId',
		fixEnabled: 'fixEnabled',
		maxPrsPerDay: 'maxPrsPerDay',
	},
	projectRepoConnections: {
		projectId: 'projectId',
		provider: 'provider',
		owner: 'owner',
		repo: 'repo',
		defaultBranch: 'defaultBranch',
		testCmd: 'testCmd',
		agentCmd: 'agentCmd',
		cloneDepth: 'cloneDepth',
	},
}));

const {
	getProjectAgentSettings,
	upsertProjectAgentSettings,
	getRepoConnection,
	upsertRepoConnection,
	deleteRepoConnection,
	getAgentSettingsForProjects,
	AgentSettingsValidationError,
} = await import('./agent-settings');

const { db } = (await import('$lib/server/db')) as any;

beforeEach(() => {
	vi.clearAllMocks();
	db.then.mockReset();
	db.then.mockImplementation((resolve: any) => resolve([]));
});

describe('getProjectAgentSettings', () => {
	it('returns null when no row exists', async () => {
		expect(await getProjectAgentSettings('p1')).toBeNull();
	});

	it('returns the row when found', async () => {
		db.then.mockImplementationOnce((resolve: any) => resolve([{ projectId: 'p1', fixEnabled: true, maxPrsPerDay: 3 }]));
		expect(await getProjectAgentSettings('p1')).toEqual({ projectId: 'p1', fixEnabled: true, maxPrsPerDay: 3 });
	});
});

describe('upsertProjectAgentSettings', () => {
	it('rejects a non-positive maxPrsPerDay before touching the db', async () => {
		await expect(upsertProjectAgentSettings('p1', { fixEnabled: true, maxPrsPerDay: 0 })).rejects.toThrow(
			AgentSettingsValidationError
		);
		expect(db.insert).not.toHaveBeenCalled();
	});

	it('rejects a non-integer maxPrsPerDay', async () => {
		await expect(upsertProjectAgentSettings('p1', { fixEnabled: true, maxPrsPerDay: 1.5 })).rejects.toThrow(
			AgentSettingsValidationError
		);
	});

	it('rejects a non-boolean fixEnabled', async () => {
		await expect(
			upsertProjectAgentSettings('p1', { fixEnabled: 'false' as any })
		).rejects.toThrow(AgentSettingsValidationError);
		expect(db.insert).not.toHaveBeenCalled();
	});

	it('allows a null/undefined maxPrsPerDay (no cap)', async () => {
		db.then.mockImplementationOnce((resolve: any) => resolve([{ projectId: 'p1', fixEnabled: true, maxPrsPerDay: null }]));
		const row = await upsertProjectAgentSettings('p1', { fixEnabled: true });
		expect(row.maxPrsPerDay).toBeNull();
		expect(db.insert).toHaveBeenCalled();
		expect(db.onConflictDoUpdate).toHaveBeenCalled();
	});

	it('writes the exact fixEnabled/maxPrsPerDay payload to both the insert values and the onConflict set', async () => {
		db.then.mockImplementationOnce((resolve: any) => resolve([{ projectId: 'p1', fixEnabled: false, maxPrsPerDay: null }]));
		await upsertProjectAgentSettings('p1', { fixEnabled: false, maxPrsPerDay: null });

		const insertValues = (db as any).values.mock.calls[0][0];
		expect(insertValues.projectId).toBe('p1');
		expect(insertValues.fixEnabled).toBe(false);
		expect(insertValues.maxPrsPerDay).toBeNull();

		const conflictArg = (db as any).onConflictDoUpdate.mock.calls[0][0];
		expect(conflictArg.set.fixEnabled).toBe(false);
		expect(conflictArg.set.maxPrsPerDay).toBeNull();
	});

	it('does not silently coerce a truthy fixEnabled into the stored row', async () => {
		db.then.mockImplementationOnce((resolve: any) => resolve([{ projectId: 'p1', fixEnabled: true, maxPrsPerDay: null }]));
		await upsertProjectAgentSettings('p1', { fixEnabled: true, maxPrsPerDay: null });

		const insertValues = (db as any).values.mock.calls[0][0];
		expect(insertValues.fixEnabled).toBe(true);
		const conflictArg = (db as any).onConflictDoUpdate.mock.calls[0][0];
		expect(conflictArg.set.fixEnabled).toBe(true);
	});
});

describe('getRepoConnection / deleteRepoConnection', () => {
	it('returns null when no connection exists', async () => {
		expect(await getRepoConnection('p1')).toBeNull();
	});

	it('deleteRepoConnection issues a delete scoped to projectId', async () => {
		await deleteRepoConnection('p1');
		expect(db.delete).toHaveBeenCalled();
		const whereArg = (db as any).where.mock.calls[0][0];
		expect(whereArg).toEqual({ __op: 'eq', col: 'projectId', val: 'p1' });
	});
});

describe('upsertRepoConnection validation', () => {
	const base = {
		provider: 'github' as const,
		owner: 'acme',
		repo: 'widgets',
		defaultBranch: 'main',
		testCmd: 'npm test',
	};

	it('rejects an unknown provider', async () => {
		await expect(upsertRepoConnection('p1', { ...base, provider: 'gitlab' as any })).rejects.toThrow(
			AgentSettingsValidationError
		);
	});

	it('rejects an empty owner', async () => {
		await expect(upsertRepoConnection('p1', { ...base, owner: '  ' })).rejects.toThrow(AgentSettingsValidationError);
	});

	it('rejects an empty repo', async () => {
		await expect(upsertRepoConnection('p1', { ...base, repo: '' })).rejects.toThrow(AgentSettingsValidationError);
	});

	it('rejects an empty defaultBranch', async () => {
		await expect(upsertRepoConnection('p1', { ...base, defaultBranch: '' })).rejects.toThrow(
			AgentSettingsValidationError
		);
	});

	it('rejects an empty testCmd', async () => {
		await expect(upsertRepoConnection('p1', { ...base, testCmd: '   ' })).rejects.toThrow(AgentSettingsValidationError);
	});

	it('rejects a non-positive cloneDepth', async () => {
		await expect(upsertRepoConnection('p1', { ...base, cloneDepth: 0 })).rejects.toThrow(AgentSettingsValidationError);
	});

	it('accepts a valid connection and upserts', async () => {
		db.then.mockImplementationOnce((resolve: any) => resolve([{ projectId: 'p1', ...base, agentCmd: null, cloneDepth: null }]));
		const row = await upsertRepoConnection('p1', base);
		expect(row.provider).toBe('github');
		expect(db.onConflictDoUpdate).toHaveBeenCalled();
	});

	it('writes each field to its own column in both the insert values and the onConflict set', async () => {
		db.then.mockImplementationOnce((resolve: any) => resolve([{ projectId: 'p1', ...base, agentCmd: null, cloneDepth: null }]));
		await upsertRepoConnection('p1', base);

		const insertValues = (db as any).values.mock.calls[0][0];
		expect(insertValues).toMatchObject({
			projectId: 'p1',
			provider: 'github',
			owner: 'acme',
			repo: 'widgets',
			defaultBranch: 'main',
			testCmd: 'npm test',
			agentCmd: null,
			cloneDepth: null,
		});

		const conflictArg = (db as any).onConflictDoUpdate.mock.calls[0][0];
		expect(conflictArg.set).toMatchObject({
			provider: 'github',
			owner: 'acme',
			repo: 'widgets',
			defaultBranch: 'main',
			testCmd: 'npm test',
			agentCmd: null,
			cloneDepth: null,
		});
	});

	it('trims owner/repo/defaultBranch/testCmd before writing, so surrounding whitespace never reaches the db', async () => {
		db.then.mockImplementationOnce((resolve: any) =>
			resolve([{ projectId: 'p1', ...base, agentCmd: null, cloneDepth: null }])
		);
		await upsertRepoConnection('p1', {
			...base,
			owner: '  acme  ',
			repo: '  widgets  ',
			defaultBranch: '  main  ',
			testCmd: '  npm test  ',
		});

		const insertValues = (db as any).values.mock.calls[0][0];
		expect(insertValues.owner).toBe('acme');
		expect(insertValues.repo).toBe('widgets');
		expect(insertValues.defaultBranch).toBe('main');
		expect(insertValues.testCmd).toBe('npm test');
	});
});

describe('getAgentSettingsForProjects', () => {
	it('returns an empty map for an empty input without touching the db', async () => {
		const result = await getAgentSettingsForProjects([]);
		expect(result.size).toBe(0);
		expect(db.select).not.toHaveBeenCalled();
	});

	it('defaults entries with no settings/repo row', async () => {
		const result = await getAgentSettingsForProjects(['p1', 'p2']);
		expect(result.get('p1')).toEqual({ fixEnabled: false, maxPrsPerDay: null, repo: null });
		expect(result.get('p2')).toEqual({ fixEnabled: false, maxPrsPerDay: null, repo: null });
	});

	it('merges settings and repo rows into the map, leaving unmatched entries default', async () => {
		db.then
			.mockImplementationOnce((resolve: any) => resolve([{ projectId: 'p1', fixEnabled: true, maxPrsPerDay: 5 }]))
			.mockImplementationOnce((resolve: any) =>
				resolve([
					{
						projectId: 'p1',
						provider: 'github',
						owner: 'acme',
						repo: 'widgets',
						defaultBranch: 'main',
						testCmd: 'npm test',
						agentCmd: null,
						cloneDepth: null,
					},
				])
			);

		const result = await getAgentSettingsForProjects(['p1', 'p2']);
		expect(result.get('p1')).toEqual({
			fixEnabled: true,
			maxPrsPerDay: 5,
			repo: {
				projectId: 'p1',
				provider: 'github',
				owner: 'acme',
				repo: 'widgets',
				defaultBranch: 'main',
				testCmd: 'npm test',
				agentCmd: null,
				cloneDepth: null,
			},
		});
		expect(result.get('p2')).toEqual({ fixEnabled: false, maxPrsPerDay: null, repo: null });
	});
});
