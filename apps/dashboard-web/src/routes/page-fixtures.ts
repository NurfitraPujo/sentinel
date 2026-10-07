import { render as baseRender } from '@testing-library/svelte';

/**
 * Page-render fixtures for `/`. The loader always returns `facets` and the full `filters` echo; the
 * render tests build partial `data` on purpose (each asserts one thing), so `render` below fills in
 * whatever a test did not set — the page never reads a count off `data.issues`, so a missing
 * `facets` would otherwise be a crash rather than a zero.
 */
export const EMPTY_FACETS = {
	status: { all: 0, unresolved: 0, resolved: 0, ignored: 0 },
	age: { all: 0, new24h: 0 },
	environment: { all: 0, byEnvironment: {} as Record<string, number> },
	project: { all: 0, byProject: {} as Record<string, number> },
	claim: {
		total: 0,
		unclaimed: 0,
		claimedMe: 0,
		claimedAgent: 0,
		byClaimant: {} as Record<string, number>,
		claimantTypes: {} as Record<string, string | null>,
	},
};

export const FILTER_DEFAULTS = {
	status: 'all',
	sort: 'last_seen',
	new: 'all',
	claim: 'all',
	q: '',
	volume: 'all',
	release: 'all',
	type: 'all',
	source: 'all',
	regressed: false,
	waiting: 'all',
	tags: [] as { key: string; value: string; param: string }[],
	seen: 'all',
	seen_from: '',
	seen_to: '',
};

export function render(component: any, options: { data: any } & Record<string, unknown>) {
	const { data, ...rest } = options;
	return baseRender(component, {
		...rest,
		data: {
			facets: EMPTY_FACETS,
			...data,
			filters: { ...FILTER_DEFAULTS, ...data.filters },
		},
	} as any);
}
