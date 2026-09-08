import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';

vi.mock('$app/navigation', () => ({ invalidateAll: vi.fn(async () => {}) }));

const { default: Page } = await import('./+page.svelte');

// `session` and `orgSlug` come from the root layout and nothing under test reads them, so each
// fixture below carries the §4.7 fields it is about and is cast once on the way in.
type AlertsPageData = Parameters<typeof Page>[1]['data'];

// sentinel-project-environments task 15's deselect-everything case on the alert picker (flow `E6`),
// plus decision 32's three wire rules — which are the highest-risk thing in task 12, because each
// one is a way to unscope a live alert rule that looks on screen exactly like a successful save:
//
//   - an untouched picker must OMIT the key entirely (leave unchanged on a PUT; apply the project
//     default on a POST — flows `E3`/`E4`);
//   - "All environments" must send `null`, not `[]` (a `400`) and not an omitted key;
//   - the picker must re-render from the RESPONSE, so a name the server merged back — unregistered,
//     hidden, or outside this caller's grant — is not silently dropped by the next save (`E11`).

const ORG_ID = 'org-1';
const PROJECT_ID = 'proj-1';

function pageData(configEnvironments: Record<string, string[] | null> = {}) {
	const configs = Object.keys(configEnvironments).map((id) => ({
		id,
		scope: 'project' as const,
		organizationId: ORG_ID,
		projectId: PROJECT_ID,
		channel: 'email',
		channelTarget: 'oncall@example.com',
		frequencyThreshold: 50,
		windowSeconds: 60,
		enabled: true,
		createdAt: null,
	}));
	return {
		alertConfigs: configs,
		editableAlertConfigs: configs,
		projects: [{ id: PROJECT_ID, name: 'Checkout API', organizationId: ORG_ID }],
		projectRoles: { [PROJECT_ID]: 'admin' },
		orgRoles: { [ORG_ID]: 'admin' },
		canManageOrgAlerts: false,
		userOrganizations: [],
	};
}

function stubFetch(options: {
	registry?: string[];
	stored?: Record<string, string[] | null>;
	saveResponse?: Record<string, unknown>;
}) {
	const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
		if (url === '/api/alerts' && (!init || !init.method || init.method === 'GET')) {
			return {
				ok: true,
				status: 200,
				json: async () =>
					Object.entries(options.stored ?? {}).map(([id, environments]) => ({ id, environments })),
			};
		}
		if (url.startsWith(`/api/organizations/${ORG_ID}/projects/${PROJECT_ID}/environments`)) {
			return {
				ok: true,
				status: 200,
				json: async () => ({ environments: (options.registry ?? []).map((name) => ({ name })) }),
			};
		}
		if (url === '/api/alerts') {
			return { ok: true, status: 200, json: async () => options.saveResponse ?? { id: 'cfg-new' } };
		}
		return { ok: true, status: 200, json: async () => ({}) };
	});
	vi.stubGlobal('fetch', fetchMock);
	return fetchMock;
}

function savedBody(fetchMock: ReturnType<typeof stubFetch>) {
	const call = [...fetchMock.mock.calls]
		.reverse()
		.find(([url, init]) => url === '/api/alerts' && ['POST', 'PUT'].includes((init as RequestInit)?.method ?? ''));
	expect(call).toBeTruthy();
	return JSON.parse((call?.[1] as RequestInit).body as string);
}

async function fillRequiredFields() {
	await fireEvent.change(screen.getByLabelText(/target project/i), {
		target: { value: PROJECT_ID },
	});
	await fireEvent.input(screen.getByLabelText(/email address/i), {
		target: { value: 'oncall@example.com' },
	});
}

beforeEach(() => {
	vi.restoreAllMocks();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('alert settings environment picker (contract §4.7, decisions 32-33)', () => {
	it('omits the environments key entirely when nobody touches the picker — flows E3/E4', async () => {
		// This is what lets the server apply the project's default environment on creation, and what
		// keeps a threshold-only edit from carrying a scope the user never asked to change.
		const fetchMock = stubFetch({ registry: ['production', 'staging'], saveResponse: { id: 'cfg-1', environments: ['production'] } });
		render(Page, { data: pageData() as unknown as AlertsPageData });

		await fillRequiredFields();
		await fireEvent.click(screen.getByRole('button', { name: /create alert/i }));

		await waitFor(() => expect(savedBody(fetchMock)).toBeTruthy());
		expect('environments' in savedBody(fetchMock)).toBe(false);
	});

	it('sends null — not [] and not an omitted key — for "All environments" — flow E12', async () => {
		const fetchMock = stubFetch({ registry: ['production'], saveResponse: { id: 'cfg-1', environments: null } });
		render(Page, { data: pageData() as unknown as AlertsPageData });

		await fillRequiredFields();
		// Touch the picker deliberately: pick "specific", then come back to "all". That is the user
		// who scoped a rule and changed their mind, and it is the case where an omitted key would
		// leave the rule scoped while the screen said it had been widened.
		await fireEvent.click(screen.getByLabelText('Only these environments'));
		await fireEvent.click(screen.getByLabelText('All environments'));
		await fireEvent.click(screen.getByRole('button', { name: /create alert/i }));

		await waitFor(() => expect(savedBody(fetchMock)).toBeTruthy());
		const body = savedBody(fetchMock);
		expect('environments' in body).toBe(true);
		expect(body.environments).toBeNull();
	});

	it('refuses a save with every environment deselected, before the request goes out — flow E6', async () => {
		const fetchMock = stubFetch({ registry: ['production', 'staging'] });
		render(Page, { data: pageData() as unknown as AlertsPageData });

		await fillRequiredFields();
		await fireEvent.click(screen.getByLabelText('Only these environments'));
		await waitFor(() => expect(screen.getByLabelText('production')).toBeTruthy());

		const callsBefore = fetchMock.mock.calls.length;
		await fireEvent.click(screen.getByRole('button', { name: /create alert/i }));

		await waitFor(() =>
			expect(screen.getByText(/choose at least one environment/i)).toBeTruthy()
		);
		// An alert that can never fire is not a state to store — and the refusal is this page's, not a
		// `400` the user has to interpret.
		expect(fetchMock.mock.calls.length).toBe(callsBefore);
	});

	it('shows a stored environment the picker could never have offered, and reads null as "All environments" — flows E2/E11', async () => {
		// `cfg-legacy` is a pre-migration row: its column is `NULL`, which is exactly today's
		// behaviour and must read as the stated fact, never as a blank. `cfg-merged` holds a name that
		// is not in the registry this caller is offered — the case where re-rendering from what was
		// SENT rather than from what came back would quietly drop it on the next save.
		stubFetch({
			registry: ['production'],
			stored: { 'cfg-legacy': null, 'cfg-merged': ['production', 'retired-region'] },
		});
		render(Page, { data: pageData({ 'cfg-legacy': null, 'cfg-merged': [] }) as unknown as AlertsPageData });

		await waitFor(() => {
			const cells = screen.getAllByTestId('config-environments').map((c) => c.textContent?.trim());
			expect(cells).toContain('All environments');
			expect(cells).toContain('production, retired-region');
		});
	});

	it('reports the no-default fallback once, at creation, when the POST says defaultEnvironmentApplied is false — flow E4', async () => {
		// Decision 33: an EVENT, reported by the response, on the screen the rule was created from.
		// Nothing persists `defaultEnvironmentApplied`, so there is deliberately nothing to re-render
		// on a reload; the standing condition lives on project settings' "no default set" line.
		stubFetch({
			registry: ['production'],
			saveResponse: { id: 'cfg-1', environments: null, defaultEnvironmentApplied: false },
		});
		render(Page, { data: pageData() as unknown as AlertsPageData });

		await fillRequiredFields();
		await fireEvent.click(screen.getByRole('button', { name: /create alert/i }));

		await waitFor(() => expect(screen.getByTestId('no-default-notice')).toBeTruthy());
		expect(screen.getByTestId('no-default-notice').textContent).toContain(
			'no default environment set'
		);
	});
});
