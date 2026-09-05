import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import ReportReporter from './ReportReporter.svelte';
import {
	externalReporterFixture,
	orgMemberReporterFixture,
} from '$lib/fixtures/report-detail';

// sentinel-user-report-issue §4.2, fe.md row S1.
//
// The assertion that matters most is the NEGATIVE one: an external reporter's email must never
// render as a `mailto:` link. OQ-6 stores the address and never sends to it and §1 says there is no
// reply channel, so a link that composes a mail nobody may send is a trap. `queryByRole('link')`
// over the rendered fragment is how that is checked — a test that only asserted the address is
// visible would pass against the defect.

describe('ReportReporter — external (S1)', () => {
	it('renders the display name, the opaque service id and a first-seen indication', () => {
		render(ReportReporter, {
			reporter: externalReporterFixture,
			projectName: 'Widgets',
			reportedAt: '2026-09-04T09:12:00Z',
		});

		expect(screen.getByTestId('reporter-external').textContent).toContain('Sarah K.');
		expect(screen.getByTestId('reporter-external-id').textContent).toContain('u_8f3c41');
		// Reported weeks after `firstSeenAt`, so this is a returning reporter, not a first-timer.
		expect(screen.getByTestId('reporter-first-seen').textContent).toContain('reporting since');
	});

	it('calls a report that arrived with the reporter row a first report from that person', () => {
		render(ReportReporter, {
			reporter: externalReporterFixture,
			projectName: 'Widgets',
			reportedAt: externalReporterFixture.firstSeenAt,
		});

		expect(screen.getByTestId('reporter-first-seen').textContent).toContain(
			'first report from this person'
		);
	});

	it('falls back to a plain name rather than an empty span when displayName is null', () => {
		render(ReportReporter, {
			reporter: { ...externalReporterFixture, displayName: null },
			projectName: 'Widgets',
		});

		const line = screen.getByTestId('reporter-external').textContent ?? '';
		expect(line).toContain('Anonymous reporter');
		expect(line.trim().length).toBeGreaterThan('Reported by'.length);
	});

	it('never renders an external reporter’s email as a mailto: link', () => {
		const { container } = render(ReportReporter, {
			reporter: { ...externalReporterFixture, email: 'sarah@example.com' },
			projectName: 'Widgets',
		});

		expect(screen.getByTestId('reporter-email').textContent).toContain('sarah@example.com');
		expect(container.querySelector('a[href^="mailto:"]')).toBeNull();
		expect(container.querySelectorAll('a')).toHaveLength(0);
		// And it says why, so the absence of a link reads as deliberate rather than as an omission.
		expect(container.textContent).toContain('no reply channel');
	});
});

describe('ReportReporter — org member, unchanged (fe.md: additive branch, not a rewrite)', () => {
	it('renders the sentence the page rendered before this feature', () => {
		render(ReportReporter, {
			reporter: orgMemberReporterFixture,
			projectName: 'Widgets',
		});

		const line = screen.getByTestId('reporter-org-member').textContent ?? '';
		expect(line).toContain('Reported by Devni K.');
		expect(line).toContain('Widgets');
		// No external-only furniture leaks onto the org-member path.
		expect(screen.queryByTestId('reporter-external-id')).toBeNull();
		expect(screen.queryByTestId('reporter-first-seen')).toBeNull();
	});

	it('falls back name -> email -> unknown, exactly as before', () => {
		const { unmount } = render(ReportReporter, {
			reporter: { ...orgMemberReporterFixture, displayName: null },
			projectName: 'Widgets',
		});
		expect(screen.getByTestId('reporter-org-member').textContent).toContain(
			'devni@example.com'
		);
		unmount();

		render(ReportReporter, {
			reporter: { ...orgMemberReporterFixture, displayName: null, email: null },
			projectName: 'Widgets',
		});
		expect(screen.getByTestId('reporter-org-member').textContent).toContain('unknown');
	});
});
