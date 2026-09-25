import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import UserPanel from './UserPanel.svelte';
import { FLUTTER_DETAIL, LONG_DETAIL } from './occurrence-fixtures';

// sentinel-occurrence-detail-panels §6.1 (TASK-03).

const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;

function rows(container: HTMLElement) {
	return Array.from(container.querySelectorAll('[data-testid="user-panel"] dt')).map((dt) => [
		dt.textContent,
		dt.nextElementSibling!.textContent,
	]);
}

describe('UserPanel', () => {
	it('US1: renders ID, Username, Email, IP Address and Geo', () => {
		const { container } = render(UserPanel, { user: FLUTTER_DETAIL.user });
		expect(screen.getByRole('heading', { level: 4 }).textContent).toBe('User');
		expect(rows(container)).toEqual([
			['ID', 'u_29181'],
			['Username', 'rina.s'],
			['Email', 'rina@example.com'],
			['IP Address', '203.0.113.42'],
			['Geo', 'Jakarta, Jakarta, Indonesia (ID)'],
		]);
	});

	it('US1: renders only the rows with a value', () => {
		const { container } = render(UserPanel, { user: { email: 'a@x', id: '', geo: { country_code: 'ID' } } });
		expect(rows(container)).toEqual([
			['Email', 'a@x'],
			['Geo', 'ID'],
		]);
	});

	it.each([[null], [{}], [{ geo: {} }], ['u1'], [{ id: 7 }]])('US1: %j renders nothing', (user) => {
		const { container } = render(UserPanel, { user });
		expect(screen.queryByTestId('user-panel')).toBeNull();
		expect(container.querySelector('h4')).toBeNull();
		expect(container.querySelector('section')).toBeNull();
	});

	it('TR8: each long row shows 256 code points and a button', () => {
		const { container } = render(UserPanel, { user: LONG_DETAIL.user });
		const dds = Array.from(container.querySelectorAll('[data-testid="user-panel"] dd'));
		expect(dds).toHaveLength(5);
		for (const dd of dds) {
			const text = dd.querySelector('.expandable-text')!.firstChild!.textContent!;
			expect(codePoints(text)).toBe(257);
			expect(text.endsWith('…')).toBe(true);
		}
		expect(screen.getAllByTestId('expand-text')).toHaveLength(5);
	});
});
