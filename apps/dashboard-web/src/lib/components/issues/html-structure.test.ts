import { describe, it, expect } from 'vitest';
import { normaliseCardHtml, tagSequence, reparse } from './html-structure';

// sentinel-occurrence-detail-panels §12.3 N1-N4: the P4 golden's normaliser and the SSR1-SSR3 nesting
// check, tested on their own.

function normalise(html: string): string {
	const host = document.createElement('div');
	host.innerHTML = html;
	return normaliseCardHtml(host.firstElementChild!);
}

describe('normaliseCardHtml', () => {
	it("N1: a false {#if}'s anchor and the whitespace around it normalise away", () => {
		expect(normalise('<div><div>h</div> <!----> <div>t</div></div>')).toBe(
			normalise('<div><div>h</div> <div>t</div></div>'),
		);
		expect(normalise('<div><div>h</div>\n\t\t<!---->\n\t\t<div>t</div></div>')).toBe(
			normalise('<div><div>h</div> <div>t</div></div>'),
		);
	});

	it('N1: scoped class hashes are removed, and an emptied class attribute with them', () => {
		expect(normalise('<div class="h svelte-oaiw42">x</div>')).toBe('<div class="h">x</div>');
		expect(normalise('<div class="svelte-oaiw42">x</div>')).toBe('<div>x</div>');
	});

	it('N2: inside a <pre>, whitespace is content and is kept', () => {
		expect(normalise('<div><pre>a <!----> b</pre></div>')).not.toBe(normalise('<div><pre>a b</pre></div>'));
		expect(normalise('<div><pre><code>a <!----> b</code></pre></div>')).not.toBe(
			normalise('<div><pre><code>a b</code></pre></div>'),
		);
		// A whitespace-only run between elements inside a <pre> is the case collapsing would change: the
		// two examples above never form such a run, so they alone cannot see step 4 reach into a <pre>.
		expect(normalise('<div><pre><span>a</span>\n  <!---->\n  <span>b</span></pre></div>')).not.toBe(
			normalise('<div><pre><span>a</span> <span>b</span></pre></div>'),
		);
		expect(normalise('<div><pre><code><span>a</span>\n\n<span>b</span></code></pre></div>')).toContain(
			'<span>a</span>\n\n<span>b</span>',
		);
	});

	it('N3: content is never normalised away', () => {
		expect(normalise('<div><div>h</div> x <div>t</div></div>')).not.toBe(
			normalise('<div><div>h</div> <div>t</div></div>'),
		);
		expect(normalise('<div><span>a</span><span>b</span></div>')).not.toBe(
			normalise('<div><span>a</span> <span>b</span></div>'),
		);
		expect(normalise('<div><span>a</span> <span>b</span></div>')).not.toBe(
			normalise('<div><span>a</span> <span>b</span></div>'),
		);
		expect(normalise('<div class="badge svelte-x1">b</div>')).toBe('<div class="badge">b</div>');
	});
});

describe('the nesting check', () => {
	it('tagSequence lists start and end tags, skipping comments, void elements and quoted attributes', () => {
		expect(tagSequence('<div a="1"><!-- <p> --><br><img src="x"><span title="<pre>">t</span></div>')).toEqual([
			'div',
			'span',
			'/span',
			'/div',
		]);
	});

	it('N4: a <pre> inside a <p> goes red', () => {
		const html = '<p class="occurrence-message"><pre>x</pre></p>';
		expect(tagSequence(reparse(html))).not.toEqual(tagSequence(html));
	});

	it('N4: a <pre> inside a <div> does not', () => {
		const html = '<div class="occurrence-message"><pre>x</pre></div>';
		expect(tagSequence(reparse(html))).toEqual(tagSequence(html));
	});

	it('N4: a tag inside a quoted attribute is not a tag', () => {
		const html = '<p title="<pre>x</pre>">t</p>';
		expect(tagSequence(reparse(html))).toEqual(tagSequence(html));
	});

	it('N4: a <tr> directly inside <table> goes red (the parser inserts <tbody>)', () => {
		const html = '<table><tr><td>x</td></tr></table>';
		expect(tagSequence(reparse(html))).not.toEqual(tagSequence(html));
		const withBody = '<table><tbody><tr><td>x</td></tr></tbody></table>';
		expect(tagSequence(reparse(withBody))).toEqual(tagSequence(withBody));
	});
});
