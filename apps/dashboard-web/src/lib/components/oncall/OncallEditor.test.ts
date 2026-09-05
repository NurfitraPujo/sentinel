import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import OncallEditor from './OncallEditor.svelte';
import {
	oncallCandidatesFixture,
	oncallEmptyResponseFixture,
	oncallNobodyOnCallFixture,
	oncallResponseFixture,
	oncallResponseViaOverrideFixture,
	oncallScheduleFixture,
} from '$lib/fixtures/oncall';

// sentinel-user-report-issue §4.4a, fe.md rows O1–O10.
//
// The load-bearing assertion is O2's permission split, and it is asserted as ABSENCE, not as
// disabled: fe.md says "controls absent or disabled, **no save button that will `403`**", and the
// contract's own framing is that on-call write is deliberately narrower than every other write
// control on this dashboard. A test asserting `disabled` would pass against a page that renders a
// Save an `engineer` can press and be refused.
//
// The second is O4: a member with `eligible: false` is shown greyed WITH THE REASON. The API
// returns them for exactly that purpose, so a component that filtered them out would look correct
// and silently change the rota behind the person who set it.

function props(overrides: Record<string, unknown> = {}) {
	return {
		orgId: 'org-1',
		schedule: oncallResponseFixture.schedule,
		currentlyOnCall: oncallResponseFixture.currentlyOnCall,
		candidates: oncallCandidatesFixture,
		canWrite: true,
		role: 'owner',
		...overrides,
	};
}

describe('OncallEditor — the permission split (O2)', () => {
	it('renders no write control at all for an engineer', () => {
		render(OncallEditor, props({ canWrite: false, role: 'engineer' }));

		// The schedule is readable...
		expect(screen.getByTestId('oncall-restatement')).toBeTruthy();
		expect(screen.getByTestId('oncall-current')).toBeTruthy();
		// ...and nothing that would 403 exists in the DOM.
		expect(screen.queryByTestId('oncall-save')).toBeNull();
		expect(screen.queryByTestId('oncall-deactivate')).toBeNull();
		expect(screen.queryByTestId('oncall-add-member')).toBeNull();
		expect(screen.queryByTestId('override-form')).toBeNull();
		expect(screen.queryByTestId('oncall-timezone')).toBeNull();
		expect(screen.queryByTestId('oncall-name')).toBeNull();
		expect(screen.queryByTestId('oncall-kind')).toBeNull();
		expect(screen.queryByTestId('oncall-handoff-time')).toBeNull();
		// No button anywhere on the page is a write: the read-only page has no buttons at all.
		expect(document.querySelectorAll('button')).toHaveLength(0);
	});

	it('names the role in the read-only notice, so the reader knows why', () => {
		render(OncallEditor, props({ canWrite: false, role: 'engineer' }));
		const notice = screen.getByTestId('oncall-readonly-notice').textContent ?? '';
		expect(notice).toContain('owners and admins');
		expect(notice).toContain('engineer');
	});

	it('offers the write controls to an owner', () => {
		render(OncallEditor, props());
		expect(screen.getByTestId('oncall-save')).toBeTruthy();
		expect(screen.getByTestId('oncall-deactivate')).toBeTruthy();
		expect(screen.getByTestId('oncall-timezone')).toBeTruthy();
	});

	it('offers an engineer no Create button on the empty state either', () => {
		render(
			OncallEditor,
			props({
				schedule: oncallEmptyResponseFixture.schedule,
				currentlyOnCall: null,
				canWrite: false,
				role: 'engineer',
			})
		);
		expect(screen.getByTestId('oncall-empty-state')).toBeTruthy();
		expect(screen.queryByTestId('oncall-create')).toBeNull();
	});
});

describe('OncallEditor — the empty state explains what a schedule does (O1)', () => {
	it('says what a schedule does and what happens without one, then offers Create', () => {
		render(OncallEditor, props({ schedule: null, currentlyOnCall: null }));

		const empty = screen.getByTestId('oncall-empty-state').textContent ?? '';
		expect(empty).toContain('carry the pager');
		expect(empty).toContain('Without one');
		expect(screen.getByTestId('oncall-create')).toBeTruthy();
		// Not a blank form: the rotation fields are not on the page until Create is pressed.
		expect(screen.queryByTestId('oncall-save')).toBeNull();
	});
});

describe('OncallEditor — the rotation (O4)', () => {
	it('shows an ineligible member greyed WITH THE REASON rather than dropping them', () => {
		render(OncallEditor, props());

		const ineligible = screen.getByTestId('rotation-member-ineligible');
		expect(ineligible.textContent).toContain('Andrew C.');
		expect(screen.getByTestId('rotation-ineligible-reason').textContent).toContain(
			'no longer allows issue write access'
		);
		expect(ineligible.className).toContain('ineligible');
		// Both members are on the list; the ineligible one is not filtered away.
		expect(screen.getByTestId('rotation-list').querySelectorAll('li')).toHaveLength(
			oncallScheduleFixture.members.length
		);
	});

	it('does not offer an ineligible member for re-adding, since the API would 400 it', () => {
		render(OncallEditor, props());
		const options = Array.from(
			screen.getByTestId('oncall-add-member').querySelectorAll('option')
		).map((o) => o.textContent ?? '');
		expect(options.join(' ')).not.toContain('Andrew C.');
		// Rina P. is an eligible member who is not yet on the rotation, so she is addable.
		expect(options.join(' ')).toContain('Rina P.');
	});
});

describe('OncallEditor — restatement, clock and who is on call (O3, O5, O6)', () => {
	it('restates the handoff in plain language matching the fields', () => {
		render(OncallEditor, props());
		expect(screen.getByTestId('oncall-restatement').textContent).toContain(
			'Hands over every Monday at 09:00 Asia/Jakarta.'
		);
	});

	it('shows the current local time in the selected zone beside the field', () => {
		render(OncallEditor, props());
		expect(screen.getByTestId('oncall-timezone-clock').textContent).toContain('there');
	});

	it('names who is on call now and via what', () => {
		const rotation = render(OncallEditor, props());
		expect(screen.getByTestId('oncall-current').textContent).toContain('Devni K.');
		expect(screen.getByTestId('oncall-current-via').textContent).toContain('rotation');
		rotation.unmount();

		render(OncallEditor, props({ currentlyOnCall: oncallResponseViaOverrideFixture.currentlyOnCall }));
		expect(screen.getByTestId('oncall-current-via').textContent).toContain('override');
	});

	it('says "nobody" plainly when nobody resolves', () => {
		render(OncallEditor, props({ currentlyOnCall: oncallNobodyOnCallFixture.currentlyOnCall }));
		expect(screen.queryByTestId('oncall-current')).toBeNull();
		expect(screen.getByTestId('oncall-current-nobody').textContent).toContain('Nobody');
	});
});

describe('OncallEditor — deactivation says what stops (O10)', () => {
	it('confirms by naming the consequence, not by asking "are you sure"', async () => {
		render(OncallEditor, props());

		screen.getByTestId('oncall-deactivate').click();

		const confirm = await screen.findByTestId('oncall-deactivate-confirm');
		expect(confirm.textContent).toContain('will no longer be assigned to anyone');
		expect(confirm.textContent).not.toContain('Are you sure');
	});
});
