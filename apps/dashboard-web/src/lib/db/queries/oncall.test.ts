import { describe, it, expect, vi, beforeEach } from 'vitest';

/**
 * sentinel-user-report-issue §4.4a — the on-call query layer.
 *
 * The rows are mocked; what is asserted is the two things this module decides rather than reads:
 * how an org role becomes `eligible`, and how a loaded schedule becomes `whoIsOnCall`'s inputs.
 * The SQL itself is exercised live against the stack (see the item's captured responses).
 */

const dbResults: unknown[][] = [];
const dbMock: Record<string, ReturnType<typeof vi.fn>> = {
	select: vi.fn(),
	from: vi.fn(),
	leftJoin: vi.fn(),
	where: vi.fn(),
	orderBy: vi.fn(),
	limit: vi.fn(),
	then: vi.fn(),
};
for (const key of ['select', 'from', 'leftJoin', 'where', 'orderBy', 'limit']) {
	dbMock[key].mockReturnValue(dbMock);
}
dbMock.then.mockImplementation((resolve: (rows: unknown[]) => unknown) =>
	resolve(dbResults.shift() ?? [])
);

vi.mock('$lib/server/db', () => ({ db: dbMock }));

const {
	getActiveSchedule,
	resolveCurrentlyOnCall,
	toResolutionInputs,
	getEligibleMemberIds,
	hasOverlappingOverride,
} = await import('./oncall');

const SCHEDULE_ROW = {
	id: 'sch-1',
	name: 'Primary',
	timezone: 'Asia/Jakarta',
	kind: 'weekly',
	// Postgres renders a TIME column as HH:MM:SS; §3's wire format is HH:MM.
	handoffTime: '09:00:00',
	handoffWeekday: 1,
	rotationAnchorAt: new Date('2026-09-02T00:00:00Z'),
};

beforeEach(() => {
	dbResults.length = 0;
	for (const key of ['select', 'from', 'leftJoin', 'where', 'orderBy', 'limit']) {
		dbMock[key].mockClear();
		dbMock[key].mockReturnValue(dbMock);
	}
});

describe('getActiveSchedule', () => {
	it('returns null when the organization has no active schedule', async () => {
		dbResults.push([]);
		expect(await getActiveSchedule('org-1')).toBeNull();
	});

	it('maps an ISSUE_WRITE_ROLES role to eligible and everything else to not', async () => {
		dbResults.push([SCHEDULE_ROW]);
		dbResults.push([
			{ userId: 'u-owner', position: 0, name: 'Owner', role: 'owner' },
			{ userId: 'u-admin', position: 1, name: 'Admin', role: 'admin' },
			{ userId: 'u-eng', position: 2, name: 'Eng', role: 'engineer' },
			{ userId: 'u-sup', position: 3, name: 'Sup', role: 'support' },
			{ userId: 'u-viewer', position: 4, name: 'Viewer', role: 'viewer' },
			// The left join's miss: no organization_members row at all — the user left the org.
			{ userId: 'u-gone', position: 5, name: null, role: null },
		]);
		dbResults.push([]);

		const schedule = await getActiveSchedule('org-1');
		expect(schedule?.members.map((m) => [m.userId, m.eligible])).toEqual([
			['u-owner', true],
			['u-admin', true],
			['u-eng', true],
			['u-sup', true],
			['u-viewer', false],
			['u-gone', false],
		]);
		// Shown, skipped, never silently deleted (§4.4): the departed member is still in the list.
		expect(schedule?.members).toHaveLength(6);
	});

	it("normalises Postgres's TIME rendering to §3's HH:MM", async () => {
		dbResults.push([SCHEDULE_ROW]);
		dbResults.push([]);
		dbResults.push([]);
		expect((await getActiveSchedule('org-1'))?.handoffTime).toBe('09:00');
	});
});

describe('toResolutionInputs / resolveCurrentlyOnCall', () => {
	const loaded = {
		id: 'sch-1',
		name: 'Primary',
		timezone: 'Asia/Jakarta',
		kind: 'weekly' as const,
		handoffTime: '09:00',
		handoffWeekday: 1,
		rotationAnchorAt: new Date('2026-09-02T00:00:00Z'),
		members: [
			{ userId: 'u-a', name: 'Devni K.', position: 0, eligible: true },
			{ userId: 'u-b', name: 'Andrew C.', position: 1, eligible: true },
		],
		overrides: [],
	};

	it('carries the anchor through — it is the phase, and dropping it re-phases the rotation', () => {
		expect(toResolutionInputs(loaded).spec.rotationAnchorAt).toEqual(
			new Date('2026-09-02T00:00:00Z')
		);
	});

	it('resolves the rotation and names the member', () => {
		expect(resolveCurrentlyOnCall(loaded, new Date('2026-09-07T02:00:00Z'))).toEqual({
			userId: 'u-b',
			name: 'Andrew C.',
			via: 'rotation',
		});
	});

	it('takes `at` as a parameter — the same schedule resolves differently at a different instant', () => {
		expect(resolveCurrentlyOnCall(loaded, new Date('2026-09-07T01:59:59Z'))?.userId).toBe('u-a');
	});

	it('returns a null name for an override naming someone off the rotation', () => {
		const withOutsider = {
			...loaded,
			overrides: [
				{
					id: 'ovr-1',
					userId: 'u-outside',
					startsAt: new Date('2026-09-08T00:00:00Z'),
					endsAt: new Date('2026-09-11T00:00:00Z'),
				},
			],
		};
		expect(resolveCurrentlyOnCall(withOutsider, new Date('2026-09-09T00:00:00Z'))).toEqual({
			userId: 'u-outside',
			name: null,
			via: 'override',
		});
	});

	it('returns null when nobody resolves', () => {
		const noneEligible = {
			...loaded,
			members: loaded.members.map((m) => ({ ...m, eligible: false })),
		};
		expect(resolveCurrentlyOnCall(noneEligible, new Date('2026-09-07T02:00:00Z'))).toBeNull();
	});
});

describe('getEligibleMemberIds', () => {
	it('does not query at all for an empty list', async () => {
		expect(await getEligibleMemberIds('org-1', [])).toEqual(new Set());
		expect(dbMock.select).not.toHaveBeenCalled();
	});

	it('returns only the ids the query came back with', async () => {
		dbResults.push([{ userId: 'u-a' }]);
		expect(await getEligibleMemberIds('org-1', ['u-a', 'u-b'])).toEqual(new Set(['u-a']));
		expect(dbMock.select).toHaveBeenCalledTimes(1);
	});
});

describe('hasOverlappingOverride', () => {
	it('is true when the query finds a row and false when it does not', async () => {
		dbResults.push([{ id: 'ovr-1' }]);
		expect(
			await hasOverlappingOverride('sch-1', new Date('2026-09-10T00:00:00Z'), new Date('2026-09-14T00:00:00Z'))
		).toBe(true);

		dbResults.push([]);
		expect(
			await hasOverlappingOverride('sch-1', new Date('2026-09-10T00:00:00Z'), new Date('2026-09-14T00:00:00Z'))
		).toBe(false);
	});
});
