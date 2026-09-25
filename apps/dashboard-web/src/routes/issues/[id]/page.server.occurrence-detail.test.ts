import { describe, it, expect, vi, beforeEach } from 'vitest';
import * as devalue from 'devalue';
import { PROTO_KEY_ROWS } from '$lib/components/issues/occurrence-fixtures';

/**
 * sentinel-occurrence-detail-panels §3.2 / §3.4 (L1-L12): the selected occurrence and its detail read.
 *
 * `?occurrence=<id>` selects an occurrence OF THIS ISSUE (listed, or older than the newest 100); an
 * id of another issue answers `404 Occurrence not found`; anything else (empty, malformed, unknown)
 * is ignored and renders the default, the newest listed occurrence, as today's page ignores unknown
 * parameters. The parameter is read only after both access checks and after `occurrences`.
 */

const authMock = vi.fn();
const requireProjectAccessMock = vi.fn();
const resolveEnvironmentScopeMock = vi.fn();
const resolveEnvironmentScopesMock = vi.fn(() => Promise.resolve(new Map()));
const getIssueByIdMock = vi.fn();
const getOccurrencesByIssueIdMock = vi.fn();
const getOccurrenceDetailMock = vi.fn();
const occurrenceExistsOutsideIssueMock = vi.fn();

vi.mock('$env/dynamic/private', () => ({ env: {} }));
vi.mock('$lib/server/issue-access', () => ({ requireProjectAccess: requireProjectAccessMock }));
vi.mock('$lib/server/environment-access', () => ({
	resolveEnvironmentScope: resolveEnvironmentScopeMock,
	resolveEnvironmentScopes: resolveEnvironmentScopesMock,
}));
vi.mock('$lib/db/queries/environments', () => ({
	resolveEnvironmentDisplayName: vi.fn(async () => 'production'),
}));
vi.mock('$lib/server/queries/issue-queries', () => ({
	issueQueries: {
		getIssueById: getIssueByIdMock,
		getProjectById: vi.fn(async () => ({ id: 'proj-1', name: 'P' })),
		getOccurrencesByIssueId: getOccurrencesByIssueIdMock,
		countOccurrencesByIssueId: vi.fn(async () => 2),
		trendForIssue: vi.fn(async () => ({ hourly: [], daily: [] })),
		getOccurrenceDetail: getOccurrenceDetailMock,
		occurrenceExistsOutsideIssue: occurrenceExistsOutsideIssueMock,
	},
}));
vi.mock('$lib/db/queries/issues', () => ({ getIssueRelations: vi.fn(async () => []) }));
vi.mock('$lib/db/queries/releases', () => ({ listPickerReleases: vi.fn(async () => []) }));

const { load } = await import('./+page.server');

const ISSUE = {
	id: '11111111-1111-4111-8111-111111111111',
	projectId: 'proj-1',
	message: 'boom',
	environment: 'production',
	status: 'unresolved',
};

const FIRST = 'aaaaaaaa-0000-4000-8000-000000000001';
const SECOND = 'aaaaaaaa-0000-4000-8000-000000000002';
const AGED_OUT = 'bbbbbbbb-0000-4000-8000-000000000001';
const OTHER_ISSUES = 'cccccccc-0000-4000-8000-000000000001';
const UNKNOWN = 'dddddddd-0000-4000-8000-000000000001';

function listedRow(id: string, level: string | null) {
	return { id, issueId: ISSUE.id, level, stacktrace: [], metadata: {}, sdk: null, createdAt: new Date() };
}

function makeEvent(query: string | null, extra: Record<string, unknown> = {}) {
	const event: Record<string, unknown> = {
		params: { id: ISSUE.id },
		locals: { auth: authMock },
		...extra,
	};
	if (query !== null) event.url = new URL(`http://localhost/issues/${ISSUE.id}${query}`);
	return event as any;
}

async function loadWith(query: string | null, extra: Record<string, unknown> = {}) {
	return (await load(makeEvent(query, extra))) as any;
}

async function catchLoad(query: string | null) {
	try {
		await loadWith(query);
	} catch (e) {
		return e as any;
	}
	return undefined;
}

beforeEach(() => {
	vi.clearAllMocks();
	authMock.mockResolvedValue({ user: { id: 'user-1' } });
	getIssueByIdMock.mockResolvedValue(ISSUE);
	requireProjectAccessMock.mockResolvedValue({ projectId: 'proj-1', organizationId: 'org-1' });
	resolveEnvironmentScopeMock.mockResolvedValue(null);
	getOccurrencesByIssueIdMock.mockResolvedValue([listedRow(FIRST, 'error'), listedRow(SECOND, 'warning')]);
	getOccurrenceDetailMock.mockImplementation(async (_issueId: string, id: string) => ({ id, detail: true }));
	occurrenceExistsOutsideIssueMock.mockResolvedValue(false);
});

describe('S1: the default selection', () => {
	it('L1: no parameter selects the newest listed occurrence and reads its detail', async () => {
		const result = await loadWith('');
		expect(result.selectedOccurrenceId).toBe(FIRST);
		expect(getOccurrenceDetailMock).toHaveBeenCalledTimes(1);
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, FIRST);
		expect(result.occurrenceDetail).toEqual({ id: FIRST, detail: true });
		expect(occurrenceExistsOutsideIssueMock).not.toHaveBeenCalled();
	});

	it('L2: an empty ?occurrence= behaves as absent', async () => {
		const result = await loadWith('?occurrence=');
		expect(result.selectedOccurrenceId).toBe(FIRST);
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, FIRST);
	});

	it('an issue with no occurrences selects nothing and reads nothing', async () => {
		getOccurrencesByIssueIdMock.mockResolvedValue([]);
		const result = await loadWith('');
		expect(result.selectedOccurrenceId).toBeNull();
		expect(result.occurrenceDetail).toBeNull();
		expect(getOccurrenceDetailMock).not.toHaveBeenCalled();
	});

	it('L12: no url at all (the event shape of loadAs and page.tag-facets.test.ts) is S1, not a TypeError', async () => {
		const noUrl = (await load({ params: { id: ISSUE.id }, locals: { auth: authMock } } as any)) as any;
		expect(noUrl.selectedOccurrenceId).toBe(FIRST);
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, FIRST);

		vi.clearAllMocks();
		const undefinedUrl = await loadWith(null, { url: undefined });
		expect(undefinedUrl.selectedOccurrenceId).toBe(FIRST);
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, FIRST);
	});
});

describe('S2: a listed occurrence', () => {
	it('L3: selects the listed id, in any case, with no extra read', async () => {
		const result = await loadWith(`?occurrence=${SECOND}`);
		expect(result.selectedOccurrenceId).toBe(SECOND);
		expect(getOccurrenceDetailMock).toHaveBeenCalledTimes(1);
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, SECOND);
		expect(occurrenceExistsOutsideIssueMock).not.toHaveBeenCalled();

		vi.clearAllMocks();
		const upper = await loadWith(`?occurrence=${SECOND.toUpperCase()}`);
		expect(upper.selectedOccurrenceId).toBe(SECOND);
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, SECOND);
	});

	it('L6: a listed pre-v2 selection (level null) reads no detail', async () => {
		getOccurrencesByIssueIdMock.mockResolvedValue([listedRow(FIRST, null), listedRow(SECOND, 'error')]);
		const result = await loadWith('');
		expect(result.selectedOccurrenceId).toBe(FIRST);
		expect(result.occurrenceDetail).toBeNull();
		expect(getOccurrenceDetailMock).not.toHaveBeenCalled();

		const absentLevel = { ...listedRow(FIRST, null) } as Record<string, unknown>;
		delete absentLevel.level;
		getOccurrencesByIssueIdMock.mockResolvedValue([absentLevel]);
		const stale = await loadWith('');
		expect(stale.occurrenceDetail).toBeNull();
		expect(getOccurrenceDetailMock).not.toHaveBeenCalled();
	});

	it('L7: a listed v2 row that vanished before the detail read is no error', async () => {
		getOccurrenceDetailMock.mockResolvedValue(null);
		const result = await loadWith(`?occurrence=${FIRST}`);
		expect(result.selectedOccurrenceId).toBe(FIRST);
		expect(result.occurrenceDetail).toBeNull();
	});
});

describe('S3: a well-formed id that is not listed', () => {
	it('L8 (S3a): an aged-out occurrence of this issue renders selected, without the probe', async () => {
		getOccurrenceDetailMock.mockImplementation(async (_i: string, id: string) =>
			id === AGED_OUT ? { id: AGED_OUT, level: 'error' } : null,
		);
		const result = await loadWith(`?occurrence=${AGED_OUT}`);
		expect(result.selectedOccurrenceId).toBe(AGED_OUT);
		expect(result.occurrenceDetail).toEqual({ id: AGED_OUT, level: 'error' });
		expect(getOccurrenceDetailMock).toHaveBeenCalledTimes(1);
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, AGED_OUT);
		expect(occurrenceExistsOutsideIssueMock).not.toHaveBeenCalled();
	});

	it('L8 (S3a): an aged-out PRE-v2 occurrence is selected too, and not read again', async () => {
		getOccurrenceDetailMock.mockImplementation(async (_i: string, id: string) =>
			id === AGED_OUT ? { id: AGED_OUT, level: null } : null,
		);
		const result = await loadWith(`?occurrence=${AGED_OUT}`);
		expect(result.selectedOccurrenceId).toBe(AGED_OUT);
		expect(result.occurrenceDetail).toEqual({ id: AGED_OUT, level: null });
		expect(getOccurrenceDetailMock).toHaveBeenCalledTimes(1);
	});

	it("L4 (S3b): another issue's (or project's) occurrence is 404 Occurrence not found, probed only after the scoped read", async () => {
		getOccurrenceDetailMock.mockResolvedValue(null);
		occurrenceExistsOutsideIssueMock.mockResolvedValue(true);
		const err = await catchLoad(`?occurrence=${OTHER_ISSUES}`);
		expect(err?.status).toBe(404);
		expect(err?.body?.message).toBe('Occurrence not found');
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, OTHER_ISSUES);
		expect(occurrenceExistsOutsideIssueMock).toHaveBeenCalledWith(ISSUE.id, OTHER_ISSUES);
		expect(getOccurrenceDetailMock.mock.invocationCallOrder[0]).toBeLessThan(
			occurrenceExistsOutsideIssueMock.mock.invocationCallOrder[0],
		);
	});

	it('L10 (S3c): an unknown id renders the default, no error', async () => {
		getOccurrenceDetailMock.mockImplementation(async (_i: string, id: string) =>
			id === UNKNOWN ? null : { id, detail: true },
		);
		const result = await loadWith(`?occurrence=${UNKNOWN}`);
		expect(result.selectedOccurrenceId).toBe(FIRST);
		expect(result.occurrenceDetail).toEqual({ id: FIRST, detail: true });
		expect(occurrenceExistsOutsideIssueMock).toHaveBeenCalledWith(ISSUE.id, UNKNOWN);
	});
});

describe('S4: anything not UUID-shaped', () => {
	it.each([
		['not-a-uuid'],
		["'; drop table x;--"],
		['{0b7e3f2a-5c1d-4e8f-9a6b-3c2d1e0f9a8b}'],
		[SECOND.replace(/-/g, '')],
	])('L9: %s renders the default and never reaches a query', async (raw) => {
		const result = await loadWith(`?occurrence=${encodeURIComponent(raw)}`);
		expect(result.selectedOccurrenceId).toBe(FIRST);
		for (const call of [...getOccurrenceDetailMock.mock.calls, ...occurrenceExistsOutsideIssueMock.mock.calls]) {
			expect(call[1]).not.toBe(raw);
			expect(call[1]).not.toBe(raw.toLowerCase());
		}
		expect(occurrenceExistsOutsideIssueMock).not.toHaveBeenCalled();
		expect(getOccurrenceDetailMock).toHaveBeenCalledWith(ISSUE.id, FIRST);
	});
});

describe('access checks come first (L5)', () => {
	it('an environment-scoped caller gets 404 Issue not found even with ?occurrence=, and no occurrence read runs', async () => {
		resolveEnvironmentScopeMock.mockResolvedValue(['staging']);
		const err = await catchLoad(`?occurrence=${OTHER_ISSUES}`);
		expect(err?.status).toBe(404);
		expect(err?.body?.message).toBe('Issue not found');
		expect(getOccurrencesByIssueIdMock).not.toHaveBeenCalled();
		expect(getOccurrenceDetailMock).not.toHaveBeenCalled();
		expect(occurrenceExistsOutsideIssueMock).not.toHaveBeenCalled();
	});

	it("requireProjectAccess's error propagates unchanged, and no occurrence read runs", async () => {
		const forbidden = { status: 403, body: { message: 'Forbidden' } };
		requireProjectAccessMock.mockRejectedValue(forbidden);
		const err = await catchLoad(`?occurrence=${OTHER_ISSUES}`);
		expect(err).toBe(forbidden);
		expect(getOccurrenceDetailMock).not.toHaveBeenCalled();
		expect(occurrenceExistsOutsideIssueMock).not.toHaveBeenCalled();
	});
});

describe('no __proto__ key reaches page data (L11)', () => {
	function assertNoProtoKey(value: unknown, path: string) {
		if (Array.isArray(value)) {
			value.forEach((v, i) => assertNoProtoKey(v, `${path}[${i}]`));
		} else if (value !== null && typeof value === 'object' && !(value instanceof Date)) {
			expect(Object.hasOwn(value, '__proto__'), `${path} still has a __proto__ key`).toBe(false);
			for (const [k, v] of Object.entries(value)) assertNoProtoKey(v, `${path}.${k}`);
		}
	}

	it('the load result serialises with devalue, keeps every sibling key and keeps createdAt a Date', async () => {
		const listed = PROTO_KEY_ROWS.listed();
		getOccurrencesByIssueIdMock.mockResolvedValue(listed);
		getOccurrenceDetailMock.mockResolvedValue(PROTO_KEY_ROWS.detail());

		const result = await loadWith('');
		expect(() => devalue.stringify(result)).not.toThrow();
		expect(() => devalue.uneval(result)).not.toThrow();

		result.occurrences.forEach((row: unknown, i: number) => assertNoProtoKey(row, `occurrences[${i}]`));
		assertNoProtoKey(result.occurrenceDetail, 'occurrenceDetail');

		const [v2, preV2] = result.occurrences;
		expect(preV2.metadata).toEqual({ request_id: 'r-1' });
		expect(v2.metadata).toEqual({ request_id: 'r-1' });
		expect(preV2.stacktrace[0]).toEqual({ file: 'main.go', line: 1, function: 'main', in_app: true });
		expect(preV2.sdk).toEqual({ name: 'sentinel.go', version: '1.0.0' });
		expect(preV2.createdAt).toBeInstanceOf(Date);

		const d = result.occurrenceDetail;
		expect(d.tags).toEqual({ env: 'prod' });
		expect(d.contexts).toEqual({ device: { model: 'Pixel 7' } });
		expect(d.request.headers).toEqual({ Accept: '*/*' });
		expect(d.breadcrumbs[0].data).toEqual({ url: '/a' });
		expect(d.user).toEqual({ id: 'u1' });
		expect(d.exception.values[0].mechanism).toEqual({ type: 'generic' });
		expect(d.sdk).toEqual({ name: 'sentinel.go', version: '1.0.0' });
		expect(d.createdAt).toBeInstanceOf(Date);
	});
});
