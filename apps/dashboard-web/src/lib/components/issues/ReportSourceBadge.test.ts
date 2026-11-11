import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import ReportSourceBadge from './ReportSourceBadge.svelte';
import { REPORT_SOURCE_CHANNELS } from '$lib/types/report-detail';

// sentinel-user-report-issue OQ-8, fe.md rows L1/L2: "marking both is marking neither". The badge
// marks `user_report_api` and nothing else — including the two channels that are neither this
// feature's door nor an org-member report.

describe('ReportSourceBadge', () => {
	it('badges a report that came through the user-report API', () => {
		render(ReportSourceBadge, { sourceChannel: 'user_report_api' });
		expect(screen.getByTestId('source-badge').textContent?.trim()).toBe('external');
	});

	it('badges nothing else — every other channel, and a null channel, render no badge at all', () => {
		for (const channel of REPORT_SOURCE_CHANNELS.filter((c) => c !== 'user_report_api')) {
			const { unmount } = render(ReportSourceBadge, { sourceChannel: channel });
			expect(screen.queryByTestId('source-badge')).toBeNull();
			unmount();
		}

		render(ReportSourceBadge, { sourceChannel: null });
		expect(screen.queryByTestId('source-badge')).toBeNull();
	});
});
