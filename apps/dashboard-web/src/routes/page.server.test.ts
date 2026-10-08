import { describe, it, expect, vi, beforeEach } from 'vitest';

// sentinel-reporting-and-dashboard D12/D11/D14, flows R1, R2, R8, H10, H11: the `/` loader. The 308 rule, the empty
// state (and that it runs no query), and that each section is its own promise.
const { getUserMock, loadShellMock, sections } = vi.hoisted(() => ({
	getUserMock: vi.fn(),
	loadShellMock: vi.fn(),
	sections: { freshness: vi.fn(), health: vi.fn(), timeline: vi.fn(), sliSection: vi.fn(), releases: vi.fn(), backlog: vi.fn(), response: vi.fn() },
}));
vi.mock('$lib/server/auth', () => ({ getUser: getUserMock }));
vi.mock('$lib/server/analytics/shell', () => ({ loadShell: loadShellMock }));
vi.mock('$lib/server/analytics/service', () => ({
	freshness: sections.freshness, health: sections.health, timeline: sections.timeline, sliSection: sections.sliSection,
	releases: sections.releases, backlog: sections.backlog, response: sections.response,
}));

import { load } from './+page.server';
import { LIST_ONLY_PARAMS } from '$lib/server/dashboard-routing';

const call = (qs: string) => load({ url: new URL(`https://sentinel.test/${qs}`), locals: {} } as never) as Promise<any>;
const input = { slis: [{ kind: 'http', service: 'api' }, { kind: 'jobs' }], project: 'all', environment: 'all', range: '7d' };

beforeEach(() => {
	vi.clearAllMocks();
	getUserMock.mockResolvedValue({ id: 'u1' });
	for (const f of Object.values(sections)) f.mockResolvedValue({ status: 'ok', data: {} });
	loadShellMock.mockResolvedValue({ input, projects: [{ id: 'p1', name: 'core' }], environments: [], databendConfigured: true, newestPostgresOccurrence: async () => null });
});

describe('GET / the 308 rule (R2, D12)', () => {
	// every list-only parameter, one at a time
	for (const key of LIST_ONLY_PARAMS) {
		it(`308s to /issues with the query string unchanged when ${key} is present`, async () => {
			await expect(call(`?${key}=x&project=abc`)).rejects.toMatchObject({ status: 308, location: `/issues?${key}=x&project=abc` });
		});
	}

	it('the contract list is all covered', () => {
		for (const k of ['status', 'claim', 'q', 'page', 'limit', 'sort', 'new', 'volume', 'seen', 'seen_from', 'seen_to', 'service']) expect(LIST_ONLY_PARAMS.has(k)).toBe(true);
	});

	it('the old links Sentinel and its scripts emit land on the list with the filter intact', async () => {
		await expect(call('?status=resolved')).rejects.toMatchObject({ status: 308, location: '/issues?status=resolved' });
		await expect(call('?project=p1&status=all&page=2')).rejects.toMatchObject({ location: '/issues?project=p1&status=all&page=2' });
		await expect(call('?service=jobs%20worker')).rejects.toMatchObject({ location: '/issues?service=jobs%20worker' });
	});

	it('does no session or database work before redirecting', async () => {
		await expect(call('?status=open')).rejects.toBeTruthy();
		expect(getUserMock).not.toHaveBeenCalled();
		expect(loadShellMock).not.toHaveBeenCalled();
	});

	it('the dashboard\'s own filters (project, environment, range) never redirect', async () => {
		const out = await call('?project=p1&environment=production&range=28d');
		expect(out.empty).toBe(false);
	});

	it('a bare / is the dashboard, and an unknown parameter is ignored', async () => {
		expect((await call('')).empty).toBe(false);
		expect((await call('?utm_source=mail')).empty).toBe(false);
	});
});

describe('GET / the dashboard (R1, R8)', () => {
	it('sends a signed-out user to /signin', async () => {
		getUserMock.mockResolvedValue(null);
		await expect(call('')).rejects.toMatchObject({ status: 303, location: '/signin' });
	});

	it('hands the viewer\'s selection to the shell and returns it as the echoed filters', async () => {
		await call('?project=p1&environment=staging&range=24h');
		expect(loadShellMock).toHaveBeenCalledWith('u1', { project: 'p1', environment: 'staging', range: '24h' });
	});

	it('returns every section as its own promise, so one can stream before another', async () => {
		const out = await call('');
		expect(Object.keys(out.sections).sort()).toEqual(['backlog', 'freshness', 'health', 'releases', 'response', 'slis', 'timeline']);
		for (const p of Object.values(out.sections)) expect(typeof (p as Promise<unknown>).then).toBe('function');
		expect(sections.health).toHaveBeenCalledTimes(1);
	});

	it('exposes the http SLI service so a count can link to the list', async () => {
		expect((await call('')).apiServices).toEqual(['api']);
	});
});

describe('GET / with no accessible project (H11, D11)', () => {
	it('is the empty state and runs no analytics query at all', async () => {
		loadShellMock.mockResolvedValue({ input, projects: [], environments: [], databendConfigured: true, newestPostgresOccurrence: async () => null });
		const out = await call('');
		expect(out.empty).toBe(true);
		expect(out.sections).toBeNull();
		for (const f of Object.values(sections)) expect(f).not.toHaveBeenCalled();
	});
});

describe('Databend down (H10, D14)', () => {
	it('an unavailable section is data, not an error: the loader still returns and the others still resolve', async () => {
		sections.health.mockResolvedValue({ status: 'unavailable' });
		const out = await call('');
		await expect(out.sections.health).resolves.toEqual({ status: 'unavailable' });
		await expect(out.sections.backlog).resolves.toEqual({ status: 'ok', data: {} });
	});
});
