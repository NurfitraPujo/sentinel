import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import Page from './+page.svelte';

// P6.2 (sentinel-sentry-compat-ingest spec §8.1, §11.2, plus contract §4.5's copy button): project settings shows the
// derived numeric Sentry project id inside #sentry-sdk. The harness is page.environments.test.ts's: the page's
// onMount fetches the environment registry and grants, which are stubbed empty.

const PROJECT_UUID = '0192f3a4-5b6c-4d7e-8f90-a1b2c3d4e5f6';
const SENTRY_ID = '27690631804612';

function pageData(projectId = PROJECT_UUID) {
	return {
		orgId: 'org-1',
		orgSlug: 'acme',
		projectId,
		projectName: 'Checkout API',
		canManageAgents: false,
		agentSettings: { fixEnabled: false, maxPrsPerDay: null },
		repoConnection: null
	};
}

function stubFetch() {
	vi.stubGlobal(
		'fetch',
		vi.fn(async (url: string) => {
			if (url.includes('/environment-grants')) {
				return { ok: true, status: 200, json: async () => ({ grants: [], members: [] }) };
			}
			if (url.includes('/environments')) {
				return { ok: true, status: 200, json: async () => ({ environments: [] }) };
			}
			return { ok: true, status: 200, json: async () => ({}) };
		})
	);
}

function stubClipboard(writeText: (text: string) => Promise<void>) {
	Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
}

beforeEach(() => {
	vi.restoreAllMocks();
	stubFetch();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('project settings — Sentry project id (spec §8.1)', () => {
	it('shows the derived id inside #sentry-sdk, not the UUID', async () => {
		render(Page, { data: pageData() });
		const value = await screen.findByTestId('sentry-project-id');
		expect(value.textContent?.trim()).toBe(SENTRY_ID);
		expect(value.closest('#sentry-sdk')).not.toBeNull();
		expect(document.querySelector('#sentry-sdk')?.textContent).not.toContain(PROJECT_UUID);
		expect(screen.getByText('Sentry project id')).toBeTruthy();
	});

	it('sits before the agent automation section, and shows for a viewer who cannot manage agents', async () => {
		render(Page, { data: { ...pageData(), canManageAgents: true } });
		await screen.findByTestId('sentry-project-id');
		const sentry = document.querySelector('#sentry-sdk')!;
		const agent = document.querySelector('.agent-automation')!;
		expect(sentry.compareDocumentPosition(agent) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
	});

	it('copy button writes the derived id to the clipboard and says so', async () => {
		const writeText = vi.fn(async () => {});
		stubClipboard(writeText);
		render(Page, { data: pageData() });
		const button = await screen.findByRole('button', { name: 'Copy' });
		await fireEvent.click(button);
		expect(writeText).toHaveBeenCalledTimes(1);
		expect(writeText).toHaveBeenCalledWith(SENTRY_ID);
		await waitFor(() => expect(screen.getByRole('button', { name: '✓ Copied' })).toBeTruthy());
	});

	it('a failed clipboard write is a toast, not a silent no-op', async () => {
		stubClipboard(vi.fn(async () => Promise.reject(new Error('denied'))));
		render(Page, { data: pageData() });
		await fireEvent.click(await screen.findByRole('button', { name: 'Copy' }));
		await waitFor(() => expect(screen.getByText('Failed to copy to clipboard')).toBeTruthy());
	});

	it('a malformed project id shows a dash and no copy button rather than breaking the page', async () => {
		render(Page, { data: pageData('not-a-uuid') });
		const value = await screen.findByTestId('sentry-project-id');
		expect(value.textContent?.trim()).toBe('—');
		expect(screen.queryByRole('button', { name: 'Copy' })).toBeNull();
	});
});
