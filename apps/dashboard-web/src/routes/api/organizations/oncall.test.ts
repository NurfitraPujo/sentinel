import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-user-report-issue §4.4a — the six on-call schedule routes.
 *
 * §11 does not name these routes as a boundary (only `whoIsOnCall` is), so acceptance is captured
 * as live HTTP responses against the running stack. This file is the fast gate underneath that: it
 * drives the handlers directly, with the QUERY layer mocked and the ACCESS layer real.
 *
 * That split is the point. `requireReportAccess` and `requireOncallWriteAccess` run their actual
 * code here against a mocked `db`, so the per-method permission split — read at any org member,
 * write at `owner`/`admin` only — is asserted rather than assumed. A test that mocked the access
 * helpers too would pass just as happily with the gate deleted.
 */

// A db mock that answers each awaited query from a queue, rather than returning itself forever:
// the access helpers issue exactly one `select … from organization_members` each, and a mock that
// cannot tell one query from the next cannot tell "checked the role" from "did nothing" (B10).
const dbResults: unknown[][] = [];
const dbMock: {
	select: ReturnType<typeof vi.fn>;
	from: ReturnType<typeof vi.fn>;
	where: ReturnType<typeof vi.fn>;
	then: ReturnType<typeof vi.fn>;
} = {
	select: vi.fn(),
	from: vi.fn(),
	where: vi.fn(),
	then: vi.fn(),
};
dbMock.select.mockReturnValue(dbMock);
dbMock.from.mockReturnValue(dbMock);
dbMock.where.mockReturnValue(dbMock);
dbMock.then.mockImplementation((resolve: (rows: unknown[]) => unknown) =>
	resolve(dbResults.shift() ?? [])
);

vi.mock('$lib/server/db', () => ({ db: dbMock }));

const queries = {
	getActiveSchedule: vi.fn(),
	getEligibleMemberIds: vi.fn(),
	replaceSchedule: vi.fn(),
	patchSchedule: vi.fn(),
	deactivateSchedule: vi.fn(),
	hasOverlappingOverride: vi.fn(),
	createOverride: vi.fn(),
	deleteOverride: vi.fn(),
};

// `resolveCurrentlyOnCall` and `toResolutionInputs` are deliberately NOT stubbed: `currentlyOnCall`
// and its `via` are computed by the real `whoIsOnCall` from the schedule the mock returns, so the
// GET shape assertion below is about the response the contract specifies and not about a fixture.
vi.mock('$lib/db/queries/oncall', async (importOriginal) => {
	const actual = await importOriginal<typeof import('$lib/db/queries/oncall')>();
	return { ...actual, ...queries };
});

const { GET, PUT, PATCH, DELETE } = await import('./[orgId]/oncall/+server');
const { POST: POST_OVERRIDE } = await import('./[orgId]/oncall/overrides/+server');
const { DELETE: DELETE_OVERRIDE } = await import(
	'./[orgId]/oncall/overrides/[overrideId]/+server'
);

const ORG = '11111111-1111-4111-8111-111111111111';
const OVERRIDE_ID = '22222222-2222-4222-8222-222222222222';

function locals(userId: string | null) {
	return { auth: async () => (userId ? { user: { id: userId } } : null) } as never;
}

/** Queues the single `organization_members` row the access helpers will read. */
function callerRole(role: string | null) {
	dbResults.length = 0;
	dbResults.push(role === null ? [] : [{ role }]);
}

function jsonRequest(method: string, body: unknown) {
	return new Request('http://x', { method, body: JSON.stringify(body) });
}

const VALID_PUT = {
	name: 'Primary',
	timezone: 'Asia/Jakarta',
	kind: 'weekly',
	handoffTime: '09:00',
	handoffWeekday: 1,
	memberIds: ['u-a', 'u-b'],
};

/** A loaded schedule, in the shape `getActiveSchedule` returns. */
function schedule(overrides: Record<string, unknown> = {}) {
	return {
		id: '33333333-3333-4333-8333-333333333333',
		name: 'Primary',
		timezone: 'Asia/Jakarta',
		kind: 'weekly',
		handoffTime: '09:00',
		handoffWeekday: 1,
		rotationAnchorAt: new Date('2026-09-02T00:00:00Z'),
		members: [
			{ userId: 'u-a', name: 'Devni K.', position: 0, eligible: true },
			{ userId: 'u-b', name: 'Andrew C.', position: 1, eligible: false },
		],
		overrides: [],
		...overrides,
	};
}

beforeEach(() => {
	vi.clearAllMocks();
	dbResults.length = 0;
	dbMock.select.mockReturnValue(dbMock);
	dbMock.from.mockReturnValue(dbMock);
	dbMock.where.mockReturnValue(dbMock);
	dbMock.then.mockImplementation((resolve: (rows: unknown[]) => unknown) =>
		resolve(dbResults.shift() ?? [])
	);
	queries.getEligibleMemberIds.mockResolvedValue(new Set(['u-a', 'u-b', 'u-c']));
	queries.replaceSchedule.mockResolvedValue(schedule().id);
	queries.patchSchedule.mockResolvedValue(undefined);
	queries.deactivateSchedule.mockResolvedValue(undefined);
	queries.hasOverlappingOverride.mockResolvedValue(false);
	queries.deleteOverride.mockResolvedValue(true);
});

describe('§4.4a auth — every route needs a session', () => {
	it('401s all six handlers when unauthenticated', async () => {
		// The handlers' declared return is `MaybePromise<Response>`; every one of these actually
		// rejects, and `Promise.resolve` is what lets `.rejects` see that through the union type.
		const calls: Array<unknown> = [
			GET({ params: { orgId: ORG }, locals: locals(null) } as never),
			PUT({ params: { orgId: ORG }, request: jsonRequest('PUT', VALID_PUT), locals: locals(null) } as never),
			PATCH({ params: { orgId: ORG }, request: jsonRequest('PATCH', {}), locals: locals(null) } as never),
			DELETE({ params: { orgId: ORG }, locals: locals(null) } as never),
			POST_OVERRIDE({ params: { orgId: ORG }, request: jsonRequest('POST', {}), locals: locals(null) } as never),
			DELETE_OVERRIDE({ params: { orgId: ORG, overrideId: OVERRIDE_ID }, locals: locals(null) } as never),
		];
		for (const call of calls) {
			await expect(Promise.resolve(call)).rejects.toMatchObject({ status: 401 });
		}
	});
});

describe('§4.4a GET — the response shape a client types against', () => {
	it('returns schedule.members[].eligible, overrides, and currentlyOnCall with via', async () => {
		callerRole('viewer');
		queries.getActiveSchedule.mockResolvedValue(
			schedule({
				overrides: [
					{
						id: OVERRIDE_ID,
						userId: 'u-b',
						startsAt: new Date('2000-01-01T00:00:00Z'),
						endsAt: new Date('2100-01-01T00:00:00Z'),
					},
				],
			})
		);

		const response = await GET({ params: { orgId: ORG }, locals: locals('viewer-1') } as never);
		expect(response.status).toBe(200);
		const body = await response.json();

		expect(body.schedule).toMatchObject({
			name: 'Primary',
			timezone: 'Asia/Jakarta',
			kind: 'weekly',
			handoffTime: '09:00',
			handoffWeekday: 1,
		});
		expect(body.schedule.members).toEqual([
			{ userId: 'u-a', name: 'Devni K.', position: 0, eligible: true },
			{ userId: 'u-b', name: 'Andrew C.', position: 1, eligible: false },
		]);
		expect(body.schedule.overrides).toHaveLength(1);
		// The open-ended override covers "now", so `via` is `override` — computed by the real
		// `whoIsOnCall`, not by a stub.
		expect(body.currentlyOnCall).toEqual({ userId: 'u-b', name: 'Andrew C.', via: 'override' });
		// The anchor is an internal phase reference and is NOT part of §4.4a's response.
		expect(body.schedule.rotationAnchorAt).toBeUndefined();
	});

	it('reports via "rotation" when no override covers the instant', async () => {
		callerRole('engineer');
		queries.getActiveSchedule.mockResolvedValue(schedule());
		const response = await GET({ params: { orgId: ORG }, locals: locals('eng-1') } as never);
		const body = await response.json();
		// u-b is ineligible, so the rotation resolves to the only eligible member either way.
		expect(body.currentlyOnCall).toEqual({ userId: 'u-a', name: 'Devni K.', via: 'rotation' });
	});

	it('returns nulls rather than 404 when the organization has no active schedule', async () => {
		callerRole('viewer');
		queries.getActiveSchedule.mockResolvedValue(null);
		const response = await GET({ params: { orgId: ORG }, locals: locals('viewer-1') } as never);
		expect(response.status).toBe(200);
		expect(await response.json()).toEqual({ schedule: null, currentlyOnCall: null });
	});

	it('403s a caller who is not a member of the organization at all', async () => {
		callerRole(null);
		await expect(
			GET({ params: { orgId: ORG }, locals: locals('stranger') } as never)
		).rejects.toMatchObject({ status: 403 });
		expect(queries.getActiveSchedule).not.toHaveBeenCalled();
	});
});

describe('§4.4a PUT — validation', () => {
	beforeEach(() => {
		queries.getActiveSchedule.mockResolvedValue(schedule());
	});

	it('400s a fixed offset timezone such as "+07:00"', async () => {
		callerRole('owner');
		await expect(
			PUT({
				params: { orgId: ORG },
				request: jsonRequest('PUT', { ...VALID_PUT, timezone: '+07:00' }),
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
		expect(queries.replaceSchedule).not.toHaveBeenCalled();
	});

	it('400s a handoffTime that is not HH:MM', async () => {
		callerRole('owner');
		await expect(
			PUT({
				params: { orgId: ORG },
				request: jsonRequest('PUT', { ...VALID_PUT, handoffTime: '9am' }),
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
		expect(queries.replaceSchedule).not.toHaveBeenCalled();
	});

	it('400s a memberIds entry without an ISSUE_WRITE_ROLES role', async () => {
		callerRole('owner');
		queries.getEligibleMemberIds.mockResolvedValue(new Set(['u-a']));
		await expect(
			PUT({
				params: { orgId: ORG },
				request: jsonRequest('PUT', { ...VALID_PUT, memberIds: ['u-a', 'u-viewer'] }),
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
		expect(queries.replaceSchedule).not.toHaveBeenCalled();
	});

	it('400s a weekly rotation without an ISO handoffWeekday, and 0 is not one', async () => {
		callerRole('owner');
		await expect(
			PUT({
				params: { orgId: ORG },
				request: jsonRequest('PUT', { ...VALID_PUT, handoffWeekday: 0 }),
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
	});

	it('accepts ISO 7 (Sunday), the value the pre-SR-4 CHECK could not store', async () => {
		callerRole('owner');
		const response = await PUT({
			params: { orgId: ORG },
			request: jsonRequest('PUT', { ...VALID_PUT, handoffWeekday: 7 }),
			locals: locals('owner-1'),
		} as never);
		expect(response.status).toBe(200);
		expect(queries.replaceSchedule).toHaveBeenCalledWith(
			ORG,
			expect.objectContaining({ handoffWeekday: 7, kind: 'weekly' }),
			'owner-1'
		);
	});

	it('400s a daily rotation that carries a handoffWeekday', async () => {
		callerRole('owner');
		await expect(
			PUT({
				params: { orgId: ORG },
				request: jsonRequest('PUT', { ...VALID_PUT, kind: 'daily', handoffWeekday: 3 }),
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
	});

	it('passes memberIds through in the order they were sent', async () => {
		callerRole('owner');
		await PUT({
			params: { orgId: ORG },
			request: jsonRequest('PUT', { ...VALID_PUT, memberIds: ['u-c', 'u-a', 'u-b'] }),
			locals: locals('owner-1'),
		} as never);
		expect(queries.replaceSchedule).toHaveBeenCalledWith(
			ORG,
			expect.objectContaining({ memberIds: ['u-c', 'u-a', 'u-b'] }),
			'owner-1'
		);
	});

	it('400s a duplicated memberIds entry rather than letting the primary key raise', async () => {
		callerRole('owner');
		await expect(
			PUT({
				params: { orgId: ORG },
				request: jsonRequest('PUT', { ...VALID_PUT, memberIds: ['u-a', 'u-a'] }),
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
	});
});

describe('§4.4a PATCH — absent leaves alone, null clears', () => {
	beforeEach(() => {
		queries.getActiveSchedule.mockResolvedValue(schedule());
	});

	it('sends only the keys the body carried', async () => {
		callerRole('admin');
		await PATCH({
			params: { orgId: ORG },
			request: jsonRequest('PATCH', { name: 'Renamed' }),
			locals: locals('admin-1'),
		} as never);
		expect(queries.patchSchedule).toHaveBeenCalledWith(schedule().id, { name: 'Renamed' });
	});

	it('400s a null on a NOT NULL field instead of silently ignoring it', async () => {
		callerRole('admin');
		await expect(
			PATCH({
				params: { orgId: ORG },
				request: jsonRequest('PATCH', { timezone: null }),
				locals: locals('admin-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
		expect(queries.patchSchedule).not.toHaveBeenCalled();
	});

	it('clears handoffWeekday when the rotation is switched to daily', async () => {
		callerRole('admin');
		await PATCH({
			params: { orgId: ORG },
			request: jsonRequest('PATCH', { kind: 'daily', handoffWeekday: null }),
			locals: locals('admin-1'),
		} as never);
		expect(queries.patchSchedule).toHaveBeenCalledWith(schedule().id, {
			kind: 'daily',
			handoffWeekday: null,
		});
	});

	it('400s a switch to daily that leaves the weekday behind', async () => {
		callerRole('admin');
		await expect(
			PATCH({
				params: { orgId: ORG },
				request: jsonRequest('PATCH', { kind: 'daily' }),
				locals: locals('admin-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
	});

	it('404s when the organization has no active schedule', async () => {
		callerRole('admin');
		queries.getActiveSchedule.mockResolvedValue(null);
		await expect(
			PATCH({
				params: { orgId: ORG },
				request: jsonRequest('PATCH', { name: 'x' }),
				locals: locals('admin-1'),
			} as never)
		).rejects.toMatchObject({ status: 404 });
	});
});

describe('§4.4a DELETE /oncall — 204, deactivate not delete', () => {
	it('204s and deactivates', async () => {
		callerRole('owner');
		queries.getActiveSchedule.mockResolvedValue(schedule());
		const response = await DELETE({ params: { orgId: ORG }, locals: locals('owner-1') } as never);
		expect(response.status).toBe(204);
		expect(queries.deactivateSchedule).toHaveBeenCalledWith(schedule().id);
	});

	it('404s when there is nothing active to deactivate', async () => {
		callerRole('owner');
		queries.getActiveSchedule.mockResolvedValue(null);
		await expect(
			DELETE({ params: { orgId: ORG }, locals: locals('owner-1') } as never)
		).rejects.toMatchObject({ status: 404 });
		expect(queries.deactivateSchedule).not.toHaveBeenCalled();
	});
});

describe('§4.4a overrides', () => {
	const WINDOW = {
		userId: 'u-b',
		startsAt: '2026-09-10T00:00:00Z',
		endsAt: '2026-09-14T00:00:00Z',
	};

	beforeEach(() => {
		queries.getActiveSchedule.mockResolvedValue(schedule());
		queries.createOverride.mockResolvedValue({
			id: OVERRIDE_ID,
			userId: 'u-b',
			startsAt: new Date(WINDOW.startsAt),
			endsAt: new Date(WINDOW.endsAt),
		});
	});

	it('201s on create', async () => {
		callerRole('owner');
		const response = await POST_OVERRIDE({
			params: { orgId: ORG },
			request: jsonRequest('POST', WINDOW),
			locals: locals('owner-1'),
		} as never);
		expect(response.status).toBe(201);
		expect(await response.json()).toEqual({
			override: {
				id: OVERRIDE_ID,
				userId: 'u-b',
				startsAt: '2026-09-10T00:00:00.000Z',
				endsAt: '2026-09-14T00:00:00.000Z',
			},
		});
	});

	it('400s when endsAt is not after startsAt', async () => {
		callerRole('owner');
		for (const endsAt of [WINDOW.startsAt, '2026-09-09T00:00:00Z']) {
			callerRole('owner');
			await expect(
				POST_OVERRIDE({
					params: { orgId: ORG },
					request: jsonRequest('POST', { ...WINDOW, endsAt }),
					locals: locals('owner-1'),
				} as never)
			).rejects.toMatchObject({ status: 400 });
		}
		expect(queries.createOverride).not.toHaveBeenCalled();
	});

	it('409s an overlapping override', async () => {
		callerRole('owner');
		queries.hasOverlappingOverride.mockResolvedValue(true);
		await expect(
			POST_OVERRIDE({
				params: { orgId: ORG },
				request: jsonRequest('POST', WINDOW),
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 409 });
		expect(queries.createOverride).not.toHaveBeenCalled();
	});

	it('400s a userId that is not an org member with an issue-write role', async () => {
		callerRole('owner');
		queries.getEligibleMemberIds.mockResolvedValue(new Set());
		await expect(
			POST_OVERRIDE({
				params: { orgId: ORG },
				request: jsonRequest('POST', WINDOW),
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 400 });
	});

	it('204s on delete, scoped to this organization\'s schedule', async () => {
		callerRole('owner');
		const response = await DELETE_OVERRIDE({
			params: { orgId: ORG, overrideId: OVERRIDE_ID },
			locals: locals('owner-1'),
		} as never);
		expect(response.status).toBe(204);
		expect(queries.deleteOverride).toHaveBeenCalledWith(schedule().id, OVERRIDE_ID);
	});

	it('404s a delete that removed nothing, rather than reporting a 204 it did not do', async () => {
		callerRole('owner');
		queries.deleteOverride.mockResolvedValue(false);
		await expect(
			DELETE_OVERRIDE({
				params: { orgId: ORG, overrideId: OVERRIDE_ID },
				locals: locals('owner-1'),
			} as never)
		).rejects.toMatchObject({ status: 404 });
	});
});

/**
 * Criterion 3's permission split, per method. `engineer` is the role that matters: it IS on
 * `ISSUE_WRITE_ROLES` and is NOT on `ONCALL_WRITE_ROLES`, so it is exactly the caller the narrower
 * bar exists to refuse.
 */
describe('§2 permission split — read at any member, write at owner/admin only', () => {
	beforeEach(() => {
		queries.getActiveSchedule.mockResolvedValue(schedule());
	});

	const writes: Array<[string, () => unknown]> = [
		[
			'PUT /oncall',
			() =>
				PUT({
					params: { orgId: ORG },
					request: jsonRequest('PUT', VALID_PUT),
					locals: locals('caller'),
				} as never),
		],
		[
			'PATCH /oncall',
			() =>
				PATCH({
					params: { orgId: ORG },
					request: jsonRequest('PATCH', { name: 'x' }),
					locals: locals('caller'),
				} as never),
		],
		['DELETE /oncall', () => DELETE({ params: { orgId: ORG }, locals: locals('caller') } as never)],
		[
			'POST /oncall/overrides',
			() =>
				POST_OVERRIDE({
					params: { orgId: ORG },
					request: jsonRequest('POST', {
						userId: 'u-b',
						startsAt: '2026-09-10T00:00:00Z',
						endsAt: '2026-09-14T00:00:00Z',
					}),
					locals: locals('caller'),
				} as never),
		],
		[
			'DELETE /oncall/overrides/{id}',
			() =>
				DELETE_OVERRIDE({
					params: { orgId: ORG, overrideId: OVERRIDE_ID },
					locals: locals('caller'),
				} as never),
		],
	];

	it('lets an engineer READ', async () => {
		callerRole('engineer');
		const response = await GET({ params: { orgId: ORG }, locals: locals('caller') } as never);
		expect(response.status).toBe(200);
	});

	for (const [label, call] of writes) {
		it(`403s an engineer on ${label}`, async () => {
			callerRole('engineer');
			await expect(Promise.resolve(call())).rejects.toMatchObject({ status: 403 });
		});
	}

	for (const [label, call] of writes) {
		it(`lets an owner through ${label}`, async () => {
			callerRole('owner');
			queries.createOverride.mockResolvedValue({
				id: OVERRIDE_ID,
				userId: 'u-b',
				startsAt: new Date('2026-09-10T00:00:00Z'),
				endsAt: new Date('2026-09-14T00:00:00Z'),
			});
			const response = (await Promise.resolve(call())) as Response;
			expect([200, 201, 204]).toContain(response.status);
		});
	}

	for (const [label, call] of writes) {
		it(`lets an admin through ${label}`, async () => {
			callerRole('admin');
			queries.createOverride.mockResolvedValue({
				id: OVERRIDE_ID,
				userId: 'u-b',
				startsAt: new Date('2026-09-10T00:00:00Z'),
				endsAt: new Date('2026-09-14T00:00:00Z'),
			});
			const response = (await Promise.resolve(call())) as Response;
			expect([200, 201, 204]).toContain(response.status);
		});
	}

	it('403s a support role too — it is on ISSUE_WRITE_ROLES but not on ONCALL_WRITE_ROLES', async () => {
		callerRole('support');
		await expect(
			PUT({
				params: { orgId: ORG },
				request: jsonRequest('PUT', VALID_PUT),
				locals: locals('caller'),
			} as never)
		).rejects.toMatchObject({ status: 403 });
	});

	it('403s a viewer on a write', async () => {
		callerRole('viewer');
		await expect(
			DELETE({ params: { orgId: ORG }, locals: locals('caller') } as never)
		).rejects.toMatchObject({ status: 403 });
	});
});
