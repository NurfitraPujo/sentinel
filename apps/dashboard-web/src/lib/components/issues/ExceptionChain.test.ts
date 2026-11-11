import { describe, it, expect } from 'vitest';
import { render, fireEvent, screen, within } from '@testing-library/svelte';
import ExceptionChain from './ExceptionChain.svelte';
import { FLUTTER_DETAIL, JS_CHAIN_DETAIL, LONG_DETAIL } from './occurrence-fixtures';

// sentinel-occurrence-detail-panels §9 (TASK-05): the chain, outermost first, below the kept top-level
// stack; Sentry's collapse beyond three; a "Not symbolicated" label on JavaScript/browser chain stacks.

const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;

describe('ExceptionChain', () => {
	it('X1: three values, outermost first, each with its own frames and mechanism', () => {
		render(ExceptionChain, { exception: FLUTTER_DETAIL.exception, platform: 'dart' });
		expect(screen.getByRole('heading', { level: 4 }).textContent).toBe('Exception Chain');
		const values = screen.getAllByTestId('chain-value');
		expect(values).toHaveLength(3);
		expect(values.map((v) => v.querySelector('h5')!.textContent)).toEqual([
			'CheckoutException: payment confirmation failed for order 88412',
			'ApiException: POST /v2/orders/88412/confirm failed',
			'SocketException: Connection reset by peer (OS Error: errno = 104), address = api.example.com, port = 443',
		]);
		values.forEach((v) => expect(within(v).getAllByTestId('frames')).toHaveLength(1));
		expect(values.map((v) => within(v).getByTestId('mechanism').textContent)).toEqual([
			'mechanism: generic · handled: false',
			'mechanism: chained · handled: true',
			'mechanism: chained · handled: true',
		]);
		expect(screen.queryAllByTestId('chain-expand')).toHaveLength(0);
		expect(screen.queryAllByTestId('chain-unsymbolicated')).toHaveLength(0);
	});

	it.each([
		['one value', { values: [{ type: 'E', value: 'v', stacktrace: [{ file: 'a', line: 1, function: 'f' }] }] }],
		['no values', { values: [] }],
		['one object among non-objects', { values: [null, 'x', { type: 'E' }] }],
		['values not an array', { values: { type: 'E' } }],
		['null', null],
		['a string', 'boom'],
	])('X2: %s renders nothing', (_name, exception) => {
		const { container } = render(ExceptionChain, { exception, platform: 'dart' });
		expect(screen.queryAllByTestId('chain-value')).toHaveLength(0);
		expect(container.querySelector('h4')).toBeNull();
	});

	it('X3: a JavaScript chain labels every stack; the fourth value starts collapsed', async () => {
		render(ExceptionChain, { exception: JS_CHAIN_DETAIL.exception, platform: 'javascript' });
		const values = screen.getAllByTestId('chain-value');
		expect(values.map((v) => v.querySelector('h5')!.textContent)).toEqual([
			'Error4: failure 4',
			'Error3: failure 3',
			'Error2: failure 2',
			'Error1: failure 1',
		]);
		const fourth = values[3];
		expect(within(fourth).queryByTestId('frames')).toBeNull();
		expect(within(fourth).queryByTestId('mechanism')).toBeNull();
		const expand = within(fourth).getByTestId('chain-expand');
		expect(expand.textContent).toBe('Show exception');
		expect(expand.getAttribute('aria-expanded')).toBe('false');
		expect(screen.getAllByTestId('chain-expand')).toHaveLength(1);

		await fireEvent.click(expand);
		expect(within(fourth).getByTestId('frames')).toBeTruthy();
		expect(within(fourth).getByTestId('mechanism')).toBeTruthy();
		expect(expand.getAttribute('aria-expanded')).toBe('true');

		const labels = screen.getAllByTestId('chain-unsymbolicated');
		expect(labels).toHaveLength(4);
		for (const v of values) {
			const label = within(v).getByTestId('chain-unsymbolicated');
			expect(label.textContent).toBe('Not symbolicated');
			expect(label.getAttribute('title')).toBe(
				'Chained exception frames are stored as received; only the top-level stack trace is symbolicated.',
			);
		}
	});

	it('X3: the same chain on a dart occurrence carries no label; browser does', () => {
		const { unmount } = render(ExceptionChain, { exception: JS_CHAIN_DETAIL.exception, platform: 'dart' });
		expect(screen.queryAllByTestId('chain-unsymbolicated')).toHaveLength(0);
		unmount();
		render(ExceptionChain, { exception: JS_CHAIN_DETAIL.exception, platform: 'browser' });
		expect(screen.getAllByTestId('chain-unsymbolicated')).toHaveLength(3);
	});

	it('a symbolicated frame, or an empty stack, removes the label; an empty heading is numbered', () => {
		render(ExceptionChain, {
			exception: {
				values: [
					{ type: '', value: '', stacktrace: [] },
					{ type: 'B', stacktrace: [{ file: 'a', line: 1, function: 'f', symbolicated: true }] },
				],
			},
			platform: 'javascript',
		});
		expect(screen.queryAllByTestId('chain-unsymbolicated')).toHaveLength(0);
		const headings = screen.getAllByTestId('chain-value').map((v) => v.querySelector('h5')!.textContent);
		expect(headings).toEqual(['B', 'Exception 2 of 2']);
	});

	it('mechanism parts: an absent handled prints nothing; neither part omits the paragraph', () => {
		render(ExceptionChain, {
			exception: {
				values: [
					{ type: 'A', mechanism: { type: 'generic' } },
					{ type: 'B', mechanism: { handled: false } },
					{ type: 'C', mechanism: {} },
				],
			},
			platform: 'go',
		});
		const values = screen.getAllByTestId('chain-value');
		expect(values.map((v) => within(v).queryByTestId('mechanism')?.textContent ?? null)).toEqual([
			null,
			'handled: false',
			'mechanism: generic',
		]);
	});

	it('TR6: every long heading part, mechanism type and chain frame line collapses', () => {
		render(ExceptionChain, { exception: LONG_DETAIL.exception, platform: 'dart' });
		const values = screen.getAllByTestId('chain-value');
		expect(values).toHaveLength(2);
		for (const v of values) {
			const parts = Array.from(v.querySelectorAll('h5 .expandable-text')).map((e) => e.firstChild!.textContent!);
			expect(parts).toHaveLength(2);
			for (const part of parts) expect(codePoints(part)).toBe(257);
			const mech = within(v).getByTestId('mechanism').querySelector('.expandable-text')!.firstChild!.textContent!;
			expect(codePoints(mech)).toBe(257);
			const line = v.querySelector('.frame-line .expandable-text')!.firstChild!.textContent!;
			expect(codePoints(line)).toBe(2049);
		}
		expect(screen.getAllByTestId('expand-text')).toHaveLength(8);
	});
});
