import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import Page from '../../../routes/issues/[id=uuid]/+page.svelte';
import {
	pageData,
	hostileFill,
	HOSTILE_OCCURRENCE,
	HOSTILE_DETAIL,
	INVENTORY_IDS,
} from './occurrence-fixtures';

// sentinel-occurrence-detail-panels §4.2 / contract §4.7 (U1, U2): event data is untrusted. Every
// inventory field holds markup (and every URL-like one a `javascript:` URL); each must render, and
// render as text only.

const EXPANDERS = ['expand-text', 'show-frames', 'chain-expand', 'crumb-data-toggle']
	.map((id) => `[data-testid="${id}"]`)
	.join(', ');

beforeEach(() => {
	vi.restoreAllMocks();
	vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) }));
});

async function openEverything(root: Element) {
	const clicked = new Set<Element>();
	for (let round = 0; round < 20; round++) {
		const fresh = Array.from(root.querySelectorAll(EXPANDERS)).filter((b) => !clicked.has(b));
		if (fresh.length === 0) return clicked.size;
		for (const button of fresh) {
			clicked.add(button);
			await fireEvent.click(button);
		}
	}
	throw new Error('expanders kept appearing');
}

describe('U1: hostile content renders as text, and only as text', () => {
	it('executes nothing, links nothing, and renders every inventory field', async () => {
		const executableBefore = document.querySelectorAll('script, img, iframe, object, embed').length;
		const { container } = render(Page, {
			data: pageData({ occurrences: [HOSTILE_OCCURRENCE], occurrenceDetail: { ...HOSTILE_OCCURRENCE, ...HOSTILE_DETAIL } }),
		});
		const card = container.querySelector('.occurrence-card')!;
		expect(await openEverything(card)).toBeGreaterThan(0);

		// (1) no element was created from event markup
		expect(document.querySelectorAll('script, img, iframe, object, embed').length).toBe(executableBefore);

		const elements = Array.from(card.querySelectorAll('*'));
		// (2) no event-handler attribute
		for (const el of elements) {
			for (const attr of Array.from(el.attributes)) expect(attr.name.startsWith('on'), `${el.tagName} ${attr.name}`).toBe(false);
		}
		// (3) no javascript: link; every anchor is a tag link or a select link
		for (const a of Array.from(card.querySelectorAll('a'))) {
			expect((a.getAttribute('href') ?? '').trim().toLowerCase().startsWith('javascript:')).toBe(false);
			expect(['tag-link', 'select-occurrence']).toContain(a.getAttribute('data-testid'));
		}
		// (4) no src/style/srcdoc/formaction/xlink:href sink holds the payload
		for (const el of elements) {
			for (const name of ['src', 'style', 'srcdoc', 'formaction', 'xlink:href']) {
				expect(el.getAttribute(name) ?? '', `${el.tagName} ${name}`).not.toContain('alert');
			}
		}
		// (5) the only attributes that hold event text are the three allowed ones
		for (const el of elements) {
			for (const attr of Array.from(el.attributes)) {
				if (!attr.value.includes('alert')) continue;
				const allowed =
					attr.name === 'data-context-key' ||
					(el.tagName === 'OPTION' && attr.name === 'value') ||
					(attr.name === 'href' &&
						el.getAttribute('data-testid') === 'tag-link' &&
						attr.value.startsWith('/search?tag='));
				expect(allowed, `${el.tagName} ${attr.name}="${attr.value}"`).toBe(true);
			}
		}
		// (6) every inventory field rendered, as text
		const text = card.textContent ?? '';
		for (const id of INVENTORY_IDS) expect(text.includes(hostileFill(id)), id).toBe(true);
	});
});

// Built, not written: G6 greps these directories for the tag literally, and must not find this file.
const RAW_HTML_TAG = '{' + '@html';

describe('U2: no raw-HTML tag in the issue components or the issue page', () => {
	it('reads every .svelte file and finds none', () => {
		const dir = join(process.cwd(), 'src/lib/components/issues');
		const files = readdirSync(dir)
			.filter((f) => f.endsWith('.svelte'))
			.map((f) => join(dir, f))
			.concat(join(process.cwd(), 'src/routes/issues/[id=uuid]/+page.svelte'));
		expect(files.length).toBeGreaterThan(9);
		for (const file of files) expect(readFileSync(file, 'utf8').includes(RAW_HTML_TAG), file).toBe(false);
	});
});
