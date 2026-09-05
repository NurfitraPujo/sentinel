import { describe, it, expect, vi, beforeEach } from 'vitest';

// sentinel-user-report-issue §4.4a / fe.md rows O2 and O9.
//
// The loader is the OTHER half of the permission split. `OncallEditor.test.ts` asserts the
// component renders no control an `engineer` cannot use; this asserts the loader is where that
// decision is taken, so the component is consuming a server answer rather than re-deriving one
// from a role string it was handed. fe.md: "gate in the loader as well as in the component".
//
// The bar is `owner`/`admin` — NARROWER than `ISSUE_WRITE_ROLES`, which is the whole point: an
// `engineer` who may resolve and assign issues may not edit the rota. A test that only checked
// "a viewer cannot write" would pass against a loader using the wrong (wider) constant.

// A chainable Drizzle-query double, the pattern this repo's other loader tests use (see
// settings/keys/page.server.test.ts).
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
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve([]));
	return dbMock;
}

const dbMock = makeDbMock();

vi.mock('$lib/server/db', () => ({ db: dbMock }));
vi.mock('$lib/db/schema', () => ({
	organizationMembers: { userId: 'userId', role: 'role', organizationId: 'organizationId' },
	users: { id: 'id', name: 'name', email: 'email' },
}));

const { load } = await import('./+page.server');

const ORG = { id: 'org-1', name: 'Acme', slug: 'acme' };

function resetDbMock(rows: any[] = []) {
	vi.clearAllMocks();
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.innerJoin.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockReset();
	dbMock.then.mockImplementation((resolve: any) => resolve(rows));
}

function locals(
	session: { id: string } | null,
	orgRole: string | null,
	currentOrg: typeof ORG | undefined = ORG
) {
	return {
		auth: async () => (session ? { user: { id: session.id } } : null),
		currentOrg,
		orgRole,
	} as any;
}

function fetchReturning(body: unknown, ok = true, status = 200) {
	return vi.fn().mockResolvedValue({ ok, status, json: async () => body }) as any;
}

const SCHEDULE_BODY = {
	schedule: {
		id: 'sched-1',
		name: 'Primary',
		timezone: 'Asia/Jakarta',
		kind: 'weekly',
		handoffTime: '09:00',
		handoffWeekday: 1,
		members: [],
		overrides: [],
	},
	currentlyOnCall: null,
};

describe('[orgSlug]/settings/on-call +page.server load — the write gate', () => {
	beforeEach(() => {
		resetDbMock();
	});

	it.each([
		['owner', true],
		['admin', true],
		// The three roles that are IN `ISSUE_WRITE_ROLES` but NOT on this surface. If `canWrite` ever
		// starts coming from `ISSUE_WRITE_ROLES`, these two rows go red and nothing else does.
		['engineer', false],
		['support', false],
		['viewer', false],
	])('gives %s canWrite=%s', async (role, expected) => {
		const fetch = fetchReturning(SCHEDULE_BODY);
		const result: any = await load({
			params: { orgSlug: 'acme' },
			fetch,
			locals: locals({ id: 'user-1' }, role),
		} as any);

		expect(result.canWrite).toBe(expected);
		expect(result.role).toBe(role);
	});

	it('gives canWrite=false when no role is resolved at all', async () => {
		const result: any = await load({
			params: { orgSlug: 'acme' },
			fetch: fetchReturning(SCHEDULE_BODY),
			locals: locals({ id: 'user-1' }, null),
		} as any);

		expect(result.canWrite).toBe(false);
		expect(result.role).toBeNull();
	});
});

describe('[orgSlug]/settings/on-call +page.server load — auth and org scope', () => {
	beforeEach(() => {
		resetDbMock();
	});

	// D39, the ordering settings/keys and settings/members already carry: authenticate before any
	// lookup that reveals whether an organization exists.
	it('401s an anonymous request before fetching anything', async () => {
		const fetch = fetchReturning(SCHEDULE_BODY);
		await expect(
			load({ params: { orgSlug: 'acme' }, fetch, locals: locals(null, 'owner') } as any)
		).rejects.toMatchObject({ status: 401 });
		expect(fetch).not.toHaveBeenCalled();
		expect(dbMock.select).not.toHaveBeenCalled();
	});

	it('403s when the session org does not match the slug in the URL', async () => {
		const fetch = fetchReturning(SCHEDULE_BODY);
		await expect(
			load({
				params: { orgSlug: 'someone-else' },
				fetch,
				locals: locals({ id: 'user-1' }, 'owner'),
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(fetch).not.toHaveBeenCalled();
	});

	it('surfaces the API status when the on-call read fails', async () => {
		await expect(
			load({
				params: { orgSlug: 'acme' },
				fetch: fetchReturning({ message: 'nope' }, false, 500),
				locals: locals({ id: 'user-1' }, 'owner'),
			} as any)
		).rejects.toMatchObject({ status: 500 });
	});
});

describe('[orgSlug]/settings/on-call +page.server load — the member picker', () => {
	// §4.4a's 400 bar: `memberIds` and an override's `userId` must be org members holding an
	// `ISSUE_WRITE_ROLES` role. Offering anyone else builds a form whose only outcome is a 400.
	it('returns only ISSUE_WRITE_ROLES members as candidates, named by name or email', async () => {
		resetDbMock([
			{ userId: 'u1', role: 'engineer', name: 'Devni K.', email: 'devni@example.com' },
			{ userId: 'u2', role: 'admin', name: null, email: 'rina@example.com' },
		]);

		const result: any = await load({
			params: { orgSlug: 'acme' },
			fetch: fetchReturning(SCHEDULE_BODY),
			locals: locals({ id: 'user-1' }, 'owner'),
		} as any);

		expect(result.candidates).toEqual([
			{ userId: 'u1', name: 'Devni K.', role: 'engineer' },
			{ userId: 'u2', name: 'rina@example.com', role: 'admin' },
		]);
	});

	it('passes the schedule and currentlyOnCall through unchanged', async () => {
		resetDbMock();
		const result: any = await load({
			params: { orgSlug: 'acme' },
			fetch: fetchReturning({
				...SCHEDULE_BODY,
				currentlyOnCall: { userId: 'u1', name: 'Devni K.', via: 'override' },
			}),
			locals: locals({ id: 'user-1' }, 'owner'),
		} as any);

		expect(result.schedule).toEqual(SCHEDULE_BODY.schedule);
		expect(result.currentlyOnCall).toEqual({ userId: 'u1', name: 'Devni K.', via: 'override' });
	});
});
