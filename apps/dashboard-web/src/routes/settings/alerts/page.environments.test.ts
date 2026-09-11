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
//     Pinned by the BUG-18 block at the bottom of this file; the E2/E11 case in this block only
//     covers the initial GET.

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

// BUG-05 — the third state. Before this, `configEnvironments` had exactly two values and neither of
// them meant "not known yet": `loadConfigEnvironments()` returned silently on `!res.ok` and swallowed
// every throw, leaving the map `{}`, and `environmentsOf()` answers `null` for an absent key. `null`
// is the FACT "All environments" — so a failed read rendered the widest possible scope for every
// rule in the table AND seeded `startEdit`'s picker from it, which is a save that unscopes a live
// rule while looking like it worked. These tests exist because no test file covered that path at all;
// that absence was part of the finding.
describe('alert settings: the stored environment scope is not known yet (BUG-05)', () => {
	function failingFetch(kind: 'status' | 'throw', registry: string[] = ['production']) {
		const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
			if (url === '/api/alerts' && (!init || !init.method || init.method === 'GET')) {
				if (kind === 'throw') throw new TypeError('Failed to fetch');
				return { ok: false, status: 503, json: async () => ({}) };
			}
			if (url.startsWith(`/api/organizations/${ORG_ID}/projects/${PROJECT_ID}/environments`)) {
				return {
					ok: true,
					status: 200,
					json: async () => ({ environments: registry.map((name) => ({ name })) }),
				};
			}
			return { ok: true, status: 200, json: async () => ({ id: 'cfg-1' }) };
		});
		vi.stubGlobal('fetch', fetchMock);
		return fetchMock;
	}

	it('never renders "All environments" when the GET fails — it renders an unknown marker', async () => {
		failingFetch('status');
		render(Page, { data: pageData({ 'cfg-1': null }) as unknown as AlertsPageData });

		// Wait for the failure to have landed, not merely for the marker: while the GET is in flight the
		// same cell reads "Loading…", and the assertion that matters is the one after it has failed.
		await waitFor(() => expect(screen.getByTestId('config-environments-error')).toBeTruthy());
		expect(screen.getByTestId('config-environments-unknown')).toBeTruthy();
		const cells = screen.getAllByTestId('config-environments').map((c) => c.textContent?.trim());
		expect(cells).not.toContain('All environments');
		expect(cells).toContain('Unknown');
	});

	it('surfaces the failure with a retry, and the retry populates the real scope', async () => {
		const fetchMock = failingFetch('status');
		render(Page, { data: pageData({ 'cfg-1': null }) as unknown as AlertsPageData });

		await waitFor(() => expect(screen.getByTestId('config-environments-error')).toBeTruthy());
		expect(screen.getByTestId('config-environments-error').textContent).toContain('503');

		// The read now succeeds. Nothing else on the page changed, so a retry is the whole recovery.
		fetchMock.mockImplementation(async (url: string, init?: RequestInit) => {
			if (url === '/api/alerts' && (!init || !init.method || init.method === 'GET')) {
				return { ok: true, status: 200, json: async () => [{ id: 'cfg-1', environments: ['staging'] }] };
			}
			return { ok: true, status: 200, json: async () => ({ environments: [] }) };
		});
		await fireEvent.click(screen.getByTestId('config-environments-retry'));

		await waitFor(() =>
			expect(screen.getByTestId('config-environments').textContent?.trim()).toBe('staging')
		);
		expect(screen.queryByTestId('config-environments-error')).toBeNull();
	});

	it('swallows nothing when the fetch throws either — the marker and the retry still appear', async () => {
		failingFetch('throw');
		render(Page, { data: pageData({ 'cfg-1': null }) as unknown as AlertsPageData });

		await waitFor(() => expect(screen.getByTestId('config-environments-error')).toBeTruthy());
		expect(screen.getByTestId('config-environments-unknown')).toBeTruthy();
	});

	it('offers no environment picker on edit while the stored scope is unknown', async () => {
		failingFetch('status');
		render(Page, { data: pageData({ 'cfg-1': null }) as unknown as AlertsPageData });
		await waitFor(() => expect(screen.getByTestId('config-environments-error')).toBeTruthy());

		await fireEvent.click(screen.getByRole('button', { name: /^edit$/i }));

		// The picker is absent rather than pre-checked on "All environments": a radio checked there is
		// itself the claim that the rule is unscoped, and it is the claim that loses scope on save.
		await waitFor(() => expect(screen.getByTestId('edit-scope-unknown')).toBeTruthy());
		expect(screen.queryByLabelText('All environments')).toBeNull();
		expect(screen.queryByLabelText('Only these environments')).toBeNull();
		expect(screen.getByTestId('edit-scope-retry')).toBeTruthy();
	});

	it('saves an unrelated field with the environments key OMITTED while the scope is unknown', async () => {
		// Decision 32's first rule is what makes this edit safe: an omitted key leaves the stored scope
		// exactly as it is, so a threshold change does not need to know what that scope was.
		const fetchMock = failingFetch('status');
		render(Page, { data: pageData({ 'cfg-1': null }) as unknown as AlertsPageData });
		await waitFor(() => expect(screen.getByTestId('config-environments-error')).toBeTruthy());

		await fireEvent.click(screen.getByRole('button', { name: /^edit$/i }));
		await waitFor(() => expect(screen.getByTestId('edit-scope-unknown')).toBeTruthy());
		await fireEvent.input(screen.getByLabelText(/threshold \(errors per window\)/i), {
			target: { value: '99' },
		});
		await fireEvent.click(screen.getByRole('button', { name: /update alert/i }));

		await waitFor(() => expect(savedBody(fetchMock)).toBeTruthy());
		const body = savedBody(fetchMock);
		expect('environments' in body).toBe(false);
		expect(body.frequencyThreshold).toBe(99);
	});
});

// BUG-16 — the picker's selection outlived the scope target it was made against. The `$effect`
// re-reads `offeredEnvironments` whenever `scope` / `selectedProjectId` / `activeOrgId` change, but
// `environmentSelection` / `environmentMode` were only ever reset by `resetForm()` and `startEdit()`.
// So: pick project A, choose "Only these environments", check `production`; switch the dropdown to
// project B, whose registry has no `production`. The offered list reloads to B's names while the
// selection still holds A's — and because the name is no longer offered, it rendered through the
// decision-48 carry-forward block labelled "already on this rule", on a rule that does not exist yet
// and a name that was never on it, and was submitted as `payload.environments`, scoping the new
// project-B rule to another project's environment name.
describe('alert settings: switching the scope target mid-form (BUG-16)', () => {
	const PROJECT_A = 'proj-a';
	const PROJECT_B = 'proj-b';

	function twoProjectData() {
		return {
			alertConfigs: [],
			editableAlertConfigs: [],
			projects: [
				{ id: PROJECT_A, name: 'Checkout API', organizationId: ORG_ID },
				{ id: PROJECT_B, name: 'Billing', organizationId: ORG_ID },
			],
			projectRoles: { [PROJECT_A]: 'admin', [PROJECT_B]: 'admin' },
			orgRoles: { [ORG_ID]: 'admin' },
			canManageOrgAlerts: false,
			userOrganizations: [],
		};
	}

	function twoProjectFetch(registries: Record<string, string[]>) {
		const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
			if (url === '/api/alerts' && (!init || !init.method || init.method === 'GET')) {
				return { ok: true, status: 200, json: async () => [] };
			}
			const match = url.match(/\/projects\/([^/]+)\/environments/);
			if (match) {
				return {
					ok: true,
					status: 200,
					json: async () => ({
						environments: (registries[match[1]] ?? []).map((name) => ({ name })),
					}),
				};
			}
			return { ok: true, status: 200, json: async () => ({ id: 'cfg-new' }) };
		});
		vi.stubGlobal('fetch', fetchMock);
		return fetchMock;
	}

	async function pickProjectAndCheck(projectId: string, name: string) {
		await fireEvent.change(screen.getByLabelText(/target project/i), {
			target: { value: projectId },
		});
		await fireEvent.click(screen.getByLabelText('Only these environments'));
		await waitFor(() => expect(screen.getByLabelText(name)).toBeTruthy());
		await fireEvent.click(screen.getByLabelText(name));
	}

	it('drops the first project’s selection when the project dropdown changes', async () => {
		twoProjectFetch({ [PROJECT_A]: ['production', 'staging'], [PROJECT_B]: ['sandbox'] });
		render(Page, { data: twoProjectData() as unknown as AlertsPageData });

		await pickProjectAndCheck(PROJECT_A, 'production');

		await fireEvent.change(screen.getByLabelText(/target project/i), {
			target: { value: PROJECT_B },
		});

		// B's registry is what is offered now, and the picker is back to its untouched state: no
		// `production` checkbox at all, and none of it carried forward under "already on this rule",
		// which is a label about a rule that does not exist yet.
		await waitFor(() => expect(screen.getByLabelText('All environments')).toBeTruthy());
		expect((screen.getByLabelText('All environments') as HTMLInputElement).checked).toBe(true);
		// A carried-forward name renders as `<name> already on this rule`, so match the prefix rather
		// than the exact label — an exact match would pass on the buggy render too.
		expect(screen.queryByLabelText(/^production/)).toBeNull();
		expect(screen.queryByText('already on this rule')).toBeNull();
	});

	it('never submits the first project’s environment name against the second project', async () => {
		const fetchMock = twoProjectFetch({
			[PROJECT_A]: ['production', 'staging'],
			[PROJECT_B]: ['sandbox'],
		});
		render(Page, { data: twoProjectData() as unknown as AlertsPageData });

		await pickProjectAndCheck(PROJECT_A, 'production');

		await fireEvent.change(screen.getByLabelText(/target project/i), {
			target: { value: PROJECT_B },
		});
		await waitFor(() => expect(screen.getByLabelText('All environments')).toBeTruthy());

		await fireEvent.input(screen.getByLabelText(/email address/i), {
			target: { value: 'oncall@example.com' },
		});
		await fireEvent.click(screen.getByRole('button', { name: /create alert/i }));

		await waitFor(() => expect(savedBody(fetchMock)).toBeTruthy());
		const body = savedBody(fetchMock);
		expect(body.projectId).toBe(PROJECT_B);
		// The picker is untouched again, so decision 32's first rule applies: the key is omitted and
		// the server applies B's default. What must never happen is `['production']` going out here.
		expect('environments' in body).toBe(false);
	});

	it('still keeps a re-selection made against the NEW target', async () => {
		const fetchMock = twoProjectFetch({
			[PROJECT_A]: ['production', 'staging'],
			[PROJECT_B]: ['sandbox'],
		});
		render(Page, { data: twoProjectData() as unknown as AlertsPageData });

		await pickProjectAndCheck(PROJECT_A, 'production');
		await fireEvent.change(screen.getByLabelText(/target project/i), {
			target: { value: PROJECT_B },
		});
		await waitFor(() => expect(screen.getByLabelText('All environments')).toBeTruthy());

		await fireEvent.click(screen.getByLabelText('Only these environments'));
		await waitFor(() => expect(screen.getByLabelText('sandbox')).toBeTruthy());
		await fireEvent.click(screen.getByLabelText('sandbox'));
		await fireEvent.input(screen.getByLabelText(/email address/i), {
			target: { value: 'oncall@example.com' },
		});
		await fireEvent.click(screen.getByRole('button', { name: /create alert/i }));

		await waitFor(() => expect(savedBody(fetchMock)).toBeTruthy());
		expect(savedBody(fetchMock).environments).toEqual(['sandbox']);
	});
});

// BUG-16, the other half of the finding: the decision-48 carry-forward block is about names the
// SERVER merged back onto an existing rule. On the create path there is no server answer to carry
// anything forward from, so the block belongs to the edit path only — where the names really did
// come back from `GET /api/alerts`.
describe('alert settings: the decision-48 carry-forward is the edit path only (BUG-16)', () => {
	it('still shows a merged name the picker cannot offer when EDITING — flow E11a', async () => {
		stubFetch({ registry: ['production'], stored: { 'cfg-1': ['production', 'retired-region'] } });
		render(Page, { data: pageData({ 'cfg-1': [] }) as unknown as AlertsPageData });

		await waitFor(() =>
			expect(screen.getByTestId('config-environments').textContent?.trim()).toBe(
				'production, retired-region'
			)
		);
		await fireEvent.click(screen.getByRole('button', { name: /^edit$/i }));

		await waitFor(() => expect(screen.getByLabelText(/^retired-region/)).toBeTruthy());
		expect((screen.getByLabelText(/^retired-region/) as HTMLInputElement).checked).toBe(true);
		expect(screen.getByText('already on this rule')).toBeTruthy();
	});
});

// BUG-18 — decision 32's third rule had no fence. The header above names it, and the E2/E11 case
// renders the merged name from the initial GET, but nothing issued a PUT and looked at what the page
// did with the RESPONSE: the `configEnvironments` assignment in `handleSubmit` could be deleted and
// the suite stayed green. These two cases drive the path the rule is about. The picker is TOUCHED
// on purpose — an untouched picker omits the key, and a server that merges nothing answers with
// exactly what the GET already seeded, which is a case no re-render bug can fail. The merge only
// happens when something is sent, so something is sent, and the stub answers with one more name
// than the client did: a hidden one this caller could not have been offered.
describe('alert settings: the row re-renders from the PUT response (BUG-18, decision 32 rule 3)', () => {
	function mergedSaveFetch() {
		return stubFetch({
			registry: ['production', 'staging'],
			stored: { 'cfg-1': ['production', 'retired-region'] },
			// What the server holds after the merge: the three names the client sent, plus one the
			// picker never offered and the client never sent.
			saveResponse: {
				id: 'cfg-1',
				environments: ['production', 'retired-region', 'staging', 'hidden-region'],
			},
		});
	}

	async function editAndAddStaging() {
		await waitFor(() =>
			expect(screen.getByTestId('config-environments').textContent?.trim()).toBe(
				'production, retired-region'
			)
		);
		await fireEvent.click(screen.getByRole('button', { name: /^edit$/i }));
		await waitFor(() => expect(screen.getByLabelText('staging')).toBeTruthy());
		await fireEvent.click(screen.getByLabelText('staging'));
		await fireEvent.click(screen.getByRole('button', { name: /update alert/i }));
	}

	it('shows the name the server merged back, which the client never sent — flow E11', async () => {
		const fetchMock = mergedSaveFetch();
		render(Page, { data: pageData({ 'cfg-1': [] }) as unknown as AlertsPageData });

		await editAndAddStaging();

		await waitFor(() => expect(savedBody(fetchMock)).toBeTruthy());
		// What went out is the picker's state — `hidden-region` is not in it, so the only way it can
		// reach the screen is through the response.
		expect(savedBody(fetchMock).environments).toEqual(['production', 'retired-region', 'staging']);
		await waitFor(() =>
			expect(screen.getByTestId('config-environments').textContent?.trim()).toBe(
				'production, retired-region, staging, hidden-region'
			)
		);
	});

	it('carries the merged name into the next edit and the next save, instead of dropping it', async () => {
		const fetchMock = mergedSaveFetch();
		render(Page, { data: pageData({ 'cfg-1': [] }) as unknown as AlertsPageData });

		await editAndAddStaging();
		await waitFor(() =>
			expect(screen.getByTestId('config-environments').textContent?.trim()).toContain('hidden-region')
		);

		// The next edit seeds its picker from the same map, so the merged name is pre-checked under
		// "already on this rule" — and a save that touches something else still sends it.
		await fireEvent.click(screen.getByRole('button', { name: /^edit$/i }));
		await waitFor(() => expect(screen.getByLabelText(/^hidden-region/)).toBeTruthy());
		expect((screen.getByLabelText(/^hidden-region/) as HTMLInputElement).checked).toBe(true);

		await fireEvent.click(screen.getByLabelText('staging'));
		await fireEvent.click(screen.getByRole('button', { name: /update alert/i }));

		await waitFor(() => expect(fetchMock.mock.calls.filter(([, i]) => (i as RequestInit)?.method === 'PUT').length).toBe(2));
		const body = savedBody(fetchMock);
		expect(body.environments).toContain('hidden-region');
		expect(body.environments).toContain('retired-region');
		expect(body.environments).not.toContain('staging');
	});
});
