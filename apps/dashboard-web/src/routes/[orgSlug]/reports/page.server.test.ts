import { describe, it, expect, vi } from 'vitest';
import { load } from './+page.server';

// sentinel-user-report-issue OQ-8 / fe.md rows L3-L4 — the `source` filter's validation and its
// composition with `tab`.
//
// Three properties are asserted, and each is a way the filter could look right and be wrong:
//   1. It VALIDATES against an allowlist, the same shape `VALID_TABS` already sets.
//   2. It FALLS BACK on an unknown value rather than throwing, because a pasted or stale URL
//      should show the unfiltered list rather than an error page — `tab` has always behaved that
//      way and a filter that 400s the page would be a new failure mode on an old URL.
//   3. It COMPOSES with `tab`: `tab` reaches the endpoint on every request, `source` narrows on top
//      of it, and both come back on `data` so every link the page renders can carry both. A filter
//      that replaced the tab would be the "seventh tab" the brief says it must not be.

const ORG = { id: 'org-1', name: 'Acme', slug: 'acme' };

const EXTERNAL_ROW = {
	issue: { id: 'i-ext', sourceChannel: 'user_report_api' },
	reporterKind: 'external',
};
const ORG_MEMBER_ROW = {
	issue: { id: 'i-org', sourceChannel: 'manual_support' },
	reporterKind: 'org_member',
};

function call(search: string, reports: unknown[] = [EXTERNAL_ROW, ORG_MEMBER_ROW]) {
	const fetch = vi.fn().mockResolvedValue({ ok: true, json: async () => ({ reports }) });
	const event = {
		params: { orgSlug: 'acme' },
		url: new URL(`http://localhost/acme/reports${search}`),
		fetch,
		locals: {
			auth: async () => ({ user: { id: 'user-1' } }),
			currentOrg: ORG,
		},
	} as any;
	return { promise: load(event), fetch };
}

describe('[orgSlug]/reports +page.server load — the source filter (L3, L4)', () => {
	it('defaults to all when no source is given, and asks the endpoint for every source', async () => {
		const { promise, fetch } = call('');
		const data: any = await promise;

		expect(data.source).toBe('all');
		expect(fetch.mock.calls[0][0]).toBe('/api/organizations/org-1/reports?tab=all');
		expect(data.reports).toHaveLength(2);
	});

	it('maps external onto the endpoint’s source_channel value', async () => {
		const { promise, fetch } = call('?source=external', [EXTERNAL_ROW]);
		const data: any = await promise;

		expect(data.source).toBe('external');
		expect(fetch.mock.calls[0][0]).toBe(
			'/api/organizations/org-1/reports?tab=all&source=user_report_api'
		);
		expect(data.reports).toEqual([EXTERNAL_ROW]);
	});

	// The endpoint takes one channel and cannot express "not this one", so this half is narrowed on
	// the row's own reporter kind. Asserted here because it is the only half of the filter whose
	// work happens in the loader rather than in the query.
	it('narrows org-members on the reporter kind, dropping the externally-sourced row', async () => {
		const { promise, fetch } = call('?source=org-members');
		const data: any = await promise;

		expect(data.source).toBe('org-members');
		// No `source` on the wire: the endpoint would have to be asked for one channel, and there is
		// more than one org-member channel.
		expect(fetch.mock.calls[0][0]).toBe('/api/organizations/org-1/reports?tab=all');
		expect(data.reports).toEqual([ORG_MEMBER_ROW]);
	});

	it('falls back to all on an unknown source rather than throwing', async () => {
		const { promise, fetch } = call('?source=not-a-source');
		const data: any = await promise;

		expect(data.source).toBe('all');
		expect(fetch.mock.calls[0][0]).toBe('/api/organizations/org-1/reports?tab=all');
		expect(data.reports).toHaveLength(2);
	});

	// L4: both axes at once. The endpoint sees the tab AND the channel; the page gets both back so
	// every link it renders can carry both.
	it('composes with tab: unclaimed + external narrows on both axes', async () => {
		const { promise, fetch } = call('?tab=unclaimed&source=external', [EXTERNAL_ROW]);
		const data: any = await promise;

		expect(data.tab).toBe('unclaimed');
		expect(data.source).toBe('external');
		expect(fetch.mock.calls[0][0]).toBe(
			'/api/organizations/org-1/reports?tab=unclaimed&source=user_report_api'
		);
	});

	it('keeps the source when the tab is the invalid one, and vice versa', async () => {
		const both: any = await call('?tab=nonsense&source=external').promise;
		expect(both.tab).toBe('all');
		expect(both.source).toBe('external');

		const other: any = await call('?tab=unclaimed&source=nonsense').promise;
		expect(other.tab).toBe('unclaimed');
		expect(other.source).toBe('all');
	});
});

describe('[orgSlug]/reports +page.server load — auth, unchanged', () => {
	it('401s an anonymous request before fetching', async () => {
		const fetch = vi.fn();
		await expect(
			load({
				params: { orgSlug: 'acme' },
				url: new URL('http://localhost/acme/reports'),
				fetch,
				locals: { auth: async () => null, currentOrg: ORG },
			} as any)
		).rejects.toMatchObject({ status: 401 });
		expect(fetch).not.toHaveBeenCalled();
	});

	it('403s when the session org does not match the slug', async () => {
		const fetch = vi.fn();
		await expect(
			load({
				params: { orgSlug: 'other' },
				url: new URL('http://localhost/other/reports'),
				fetch,
				locals: { auth: async () => ({ user: { id: 'user-1' } }), currentOrg: ORG },
			} as any)
		).rejects.toMatchObject({ status: 403 });
		expect(fetch).not.toHaveBeenCalled();
	});
});
