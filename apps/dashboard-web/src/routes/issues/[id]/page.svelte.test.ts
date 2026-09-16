import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent, screen } from '@testing-library/svelte';
import Page from './+page.svelte';

function makePageData(overrides: Record<string, unknown> = {}) {
	return {
		issue: {
			id: 'issue-1',
			projectId: 'proj-1',
			environment: 'production',
			errorClass: 'Error',
			message: 'Test error message',
			status: 'unresolved',
			firstSeen: new Date('2026-09-16T00:00:00Z'),
			lastSeen: new Date('2026-09-16T00:00:00Z'),
			count: 1,
			issueType: 'system_error',
		},
		project: {
			id: 'proj-1',
			name: 'Test Project',
		},
		occurrences: [
			{
				id: 'occ-1',
				issueId: 'issue-1',
				environment: 'production',
				platform: 'go',
				stacktrace: [],
				metadata: { foo: 'bar', answer: 42 },
				traceId: 'trace-123',
				spanId: 'span-456',
				createdAt: new Date('2026-09-16T09:00:00Z'),
			},
			{
				id: 'occ-2',
				issueId: 'issue-1',
				environment: 'production',
				platform: 'go',
				stacktrace: [],
				metadata: {},
				traceId: null,
				spanId: null,
				createdAt: new Date('2026-09-16T08:00:00Z'),
			},
		],
		occurrenceTotal: 2,
		environmentDisplayName: 'Production',
		relations: [],
		trend: { hourly: [], daily: [] },
		retentionDays: 30,
		releases: [],
		canWrite: false,
		...overrides,
		// eslint-disable-next-line @typescript-eslint/no-explicit-any
	} as any;
}

describe('Issue Detail Page — occurrence metadata toggle (BUG-01)', () => {
	beforeEach(() => {
		vi.restoreAllMocks();
		vi.stubGlobal(
			'fetch',
			vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) })
		);
	});

	it('toggles metadata expansion when Show/Hide Metadata is clicked', async () => {
		render(Page, { data: makePageData() });

		const toggles = screen.getAllByTestId('metadata-toggle');
		expect(toggles.length).toBe(2);

		const firstToggle = toggles[0];
		expect(firstToggle.textContent?.trim()).toBe('Show Metadata');
		expect(screen.queryByTestId('occurrence-metadata')).toBeNull();

		// Click to expand metadata on occ-1
		await fireEvent.click(firstToggle);

		expect(firstToggle.textContent?.trim()).toBe('Hide Metadata');
		const metadataBox = screen.getByTestId('occurrence-metadata');
		expect(metadataBox).toBeTruthy();
		expect(metadataBox.textContent).toContain('"foo": "bar"');
		expect(metadataBox.textContent).toContain('"answer": 42');

		// Click to hide metadata on occ-1
		await fireEvent.click(firstToggle);

		expect(firstToggle.textContent?.trim()).toBe('Show Metadata');
		expect(screen.queryByTestId('occurrence-metadata')).toBeNull();
	});

	it('displays empty metadata message when occurrence metadata is empty', async () => {
		render(Page, { data: makePageData() });

		const toggles = screen.getAllByTestId('metadata-toggle');
		const secondToggle = toggles[1]; // occ-2 has empty metadata: {}
		expect(secondToggle.textContent?.trim()).toBe('Show Metadata');

		await fireEvent.click(secondToggle);

		expect(secondToggle.textContent?.trim()).toBe('Hide Metadata');
		const metadataBox = screen.getByTestId('occurrence-metadata');
		expect(metadataBox).toBeTruthy();
		expect(metadataBox.textContent).toContain('No metadata recorded.');
	});
});
