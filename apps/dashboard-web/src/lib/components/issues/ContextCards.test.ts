import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import ContextCards from './ContextCards.svelte';
import { FLUTTER_DETAIL, LONG_DETAIL, protoContexts } from './occurrence-fixtures';

// sentinel-occurrence-detail-panels §7 (TASK-01): the context cards, well-known first.

const codePoints = (s: string | null | undefined) => Array.from(s ?? '').length;
const cards = () => screen.queryAllByTestId('context-card');
const card = (key: string) => cards().find((c) => c.getAttribute('data-context-key') === key)!;
const title = (el: Element) => el.querySelector('h5')!.textContent;
const rows = (el: Element) =>
	Array.from(el.querySelectorAll('dt')).map((dt) => [dt.textContent, dt.nextElementSibling!.textContent]);

describe('ContextCards', () => {
	it('C1: the Flutter contexts in the contract order, with titles, headlines and rows', () => {
		render(ContextCards, { contexts: FLUTTER_DETAIL.contexts });
		expect(screen.getByRole('heading', { level: 4 }).textContent).toBe('Contexts');
		expect(cards().map((c) => c.getAttribute('data-context-key'))).toEqual([
			'device', 'os', 'app', 'runtime', 'culture', 'trace', 'flutter',
		]);
		expect(cards().map(title)).toEqual([
			'Device', 'Operating System', 'App', 'Runtime', 'Culture', 'Trace', 'flutter',
		]);
		expect(cards().map((c) => c.querySelector('.context-headline')?.textContent ?? null)).toEqual([
			'Pixel 7', 'Android 14', 'Daya 2.14.0', 'Dart 3.3.0 (stable)', 'id_ID', null, null,
		]);
		const device = rows(card('device'));
		expect(device.slice(0, 3)).toEqual([
			['Name', 'Pixel 7'],
			['Family', 'Pixel'],
			['Model', 'Pixel 7'],
		]);
		expect(device.at(-1)).toEqual(['custom_field', 'custom value']);
		expect(device.map(([dt]) => dt)).not.toContain('type');
		expect(device).toContainEqual(['Simulator', 'false']);
		expect(device).toContainEqual(['Memory Size', '8000000000']);
	});

	it('C2: an unknown card renders its keys generically', () => {
		render(ContextCards, { contexts: FLUTTER_DETAIL.contexts });
		expect(rows(card('flutter'))).toEqual([
			['has_render_view', 'true'],
			['renderer', 'Impeller'],
		]);
	});

	it('C3: os and client_os are two cards', () => {
		render(ContextCards, {
			contexts: { client_os: { type: 'os', name: 'Windows' }, os: { name: 'Android', version: '14' } },
		});
		expect(cards().map(title)).toEqual(['Operating System', 'Client Operating System']);
	});

	it('C4: a redacted context is shown as stored; type equal to the key is skipped, type os under client_os is not', () => {
		render(ContextCards, {
			contexts: { device: '***REDACTED***', os: { type: 'os', name: 'Android' }, client_os: { type: 'os', name: 'Windows' } },
		});
		const scalar = card('device').querySelector('.context-scalar')!;
		expect(scalar.textContent).toBe('***REDACTED***');
		expect(scalar.tagName).toBe('DIV');
		expect(rows(card('os'))).toEqual([['Name', 'Android']]);
		expect(rows(card('client_os'))).toEqual([
			['Name', 'Windows'],
			['type', 'os'],
		]);
	});

	it.each([[null], [{}], [{ device: {} }], [{ os: { type: 'os' } }], ['ctx'], [[{ device: { name: 'd' } }]]])(
		'C5: %j renders nothing',
		(contexts) => {
			const { container } = render(ContextCards, { contexts });
			expect(cards()).toHaveLength(0);
			expect(container.querySelector('h4')).toBeNull();
		},
	);

	it('C6: prototype keys are unknown cards and rows, and no function source renders', () => {
		const { container } = render(ContextCards, { contexts: protoContexts() });
		expect(cards().map((c) => c.getAttribute('data-context-key'))).toEqual([
			'device', '__proto__', 'constructor', 'hasOwnProperty', 'toString',
		]);
		expect(cards().map(title)).toEqual(['Device', '__proto__', 'constructor', 'hasOwnProperty', 'toString']);
		expect(rows(card('constructor'))).toEqual([['name', 'c']]);
		expect(card('hasOwnProperty').querySelector('.context-scalar')!.textContent).toBe('x');
		expect(rows(card('device'))).toEqual([
			['Model', 'P7'],
			['constructor', 'Pixel'],
		]);
		expect(rows(card('__proto__'))).toEqual([['a', '1']]);
		expect(container.textContent).not.toContain('function');
		expect(container.textContent).not.toContain('[native code]');
	});

	it('TR3: every long context string collapses', () => {
		render(ContextCards, { contexts: LONG_DETAIL.contexts });
		const device = card('device');
		const headline = device.querySelector('.context-headline')!;
		const shown = headline.querySelector('.expandable-text')!.firstChild!.textContent!;
		expect(codePoints(shown)).toBe(257);
		expect(shown.endsWith('…')).toBe(true);
		expect(headline.querySelector('[data-testid="expand-text"]')!.textContent).toBe('Show all (65536 characters)');

		const model = Array.from(device.querySelectorAll('dt')).find((dt) => dt.textContent === 'Model')!.nextElementSibling!;
		const unknown = cards().find((c) => !['device', 'os'].includes(c.getAttribute('data-context-key')!))!;
		for (const el of [model, unknown.querySelector('h5')!, unknown.querySelector('dt')!, unknown.querySelector('dd')!]) {
			const text = el.querySelector('.expandable-text')!.firstChild!.textContent!;
			expect(codePoints(text)).toBe(257);
			expect(el.querySelector('[data-testid="expand-text"]')).not.toBeNull();
		}

		const pre = card('os').querySelector('.context-scalar pre')!;
		expect(codePoints(pre.textContent)).toBe(2049);
		expect(screen.getAllByTestId('expand-text')).toHaveLength(6);
	});
});

// Review finding odp-rv1-context-mapping-rules-untested: two §7.2 mapping rules no fixture reached.
describe('ContextCards §7.2 mapping rules', () => {
	const headline = (key: string) => card(key).querySelector('.context-headline')?.textContent ?? null;

	it('C7: the device headline falls back from model to name, and a model still wins over a name', () => {
		const { unmount } = render(ContextCards, { contexts: { device: { name: 'iPhone', family: 'iOS' } } });
		expect(headline('device')).toBe('iPhone');
		unmount();

		render(ContextCards, { contexts: { device: { name: 'Work phone', model: 'iPhone15,2' } } });
		expect(headline('device')).toBe('iPhone15,2');
	});

	it('C8: every well-known culture field carries its label, in the contract order', () => {
		render(ContextCards, {
			contexts: {
				culture: {
					is_24_hour_format: true,
					timezone: 'Asia/Jakarta',
					calendar: 'GregorianCalendar',
					display_name: 'Indonesian (Indonesia)',
					locale: 'id_ID',
				},
			},
		});
		expect(rows(card('culture'))).toEqual([
			['Locale', 'id_ID'],
			['Display Name', 'Indonesian (Indonesia)'],
			['Calendar', 'GregorianCalendar'],
			['Timezone', 'Asia/Jakarta'],
			['24-Hour Format', 'true'],
		]);
	});
});
