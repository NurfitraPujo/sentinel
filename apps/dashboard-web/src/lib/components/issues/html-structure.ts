// sentinel-occurrence-detail-panels §12.2 / §12.3: TEST SUPPORT ONLY, imported only by tests.
//
// - `normaliseCardHtml` is the P4 golden's normaliser: it removes what Svelte adds that is not content
//   (comment anchors, scoped class hashes, and the whitespace-only text a false `{#if}` leaves behind),
//   and nothing else.
// - `tagSequence` and `reparse` are the SSR1-SSR3 nesting check: the browser's HTML parser repairs
//   invalid nesting (`<p><pre>` becomes `<p></p><pre>…</pre><p></p>`), so server-rendered markup whose
//   tag sequence changes when it is re-parsed is markup hydration would walk wrongly.
import { JSDOM } from 'jsdom';

const ELEMENT_NODE = 1;
const TEXT_NODE = 3;
const COMMENT_NODE = 8;
const HTML_WHITESPACE = /^[ \t\n\f\r]+$/;
const SVELTE_CLASS = /^svelte-[a-z0-9]+$/;

function removeComments(node: Node) {
	for (const child of Array.from(node.childNodes)) {
		if (child.nodeType === COMMENT_NODE) child.remove();
		else removeComments(child);
	}
}

function stripSvelteClasses(root: Element) {
	for (const el of [root, ...Array.from(root.querySelectorAll('[class]'))]) {
		const tokens = (el.getAttribute('class') ?? '').split(/[ \t\n\f\r]+/).filter((t) => t !== '');
		if (tokens.length === 0 && !el.hasAttribute('class')) continue;
		const kept = tokens.filter((t) => !SVELTE_CLASS.test(t));
		if (kept.length === tokens.length) continue;
		if (kept.length === 0) el.removeAttribute('class');
		else el.setAttribute('class', kept.join(' '));
	}
}

function removeEmptyText(node: Node) {
	for (const child of Array.from(node.childNodes)) {
		if (child.nodeType === TEXT_NODE && (child.nodeValue ?? '') === '') child.remove();
		else removeEmptyText(child);
	}
}

function insidePre(node: Node): boolean {
	for (let n: Node | null = node; n; n = n.parentNode) {
		if (n.nodeType === ELEMENT_NODE && (n as Element).localName === 'pre') return true;
	}
	return false;
}

function collapseWhitespaceRuns(node: Node) {
	if (!insidePre(node)) {
		const children = Array.from(node.childNodes);
		let i = 0;
		while (i < children.length) {
			if (children[i].nodeType !== TEXT_NODE) {
				i++;
				continue;
			}
			// A maximal run of adjacent sibling text nodes.
			let j = i;
			while (j < children.length && children[j].nodeType === TEXT_NODE) j++;
			const run = children.slice(i, j);
			if (run.every((t) => HTML_WHITESPACE.test(t.nodeValue ?? ''))) {
				const first = run[0];
				first.nodeValue = ' ';
				for (const extra of run.slice(1)) extra.remove();
			}
			i = j;
		}
	}
	for (const child of Array.from(node.childNodes)) {
		if (child.nodeType === ELEMENT_NODE) collapseWhitespaceRuns(child);
	}
}

/**
 * The `outerHTML` of a deep clone of `card` after exactly these steps, in this order (spec §12.2):
 * 1. remove every comment node;
 * 2. remove every `class` token matching `^svelte-[a-z0-9]+$`, and a `class` attribute this empties;
 * 3. remove every empty text node;
 * 4. replace every maximal run of adjacent sibling text nodes that are ALL whitespace-only (HTML
 *    whitespace; a no-break space is content) with one `' '`, unless inside a `<pre>`.
 */
export function normaliseCardHtml(card: Element): string {
	const clone = card.cloneNode(true) as Element;
	removeComments(clone);
	stripSvelteClasses(clone);
	removeEmptyText(clone);
	collapseWhitespaceRuns(clone);
	return clone.outerHTML;
}

const VOID_ELEMENTS = new Set([
	'area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'source', 'track', 'wbr',
]);

/**
 * The start and end tag names in `html`, lower-cased, in document order (`div`, `/div`). Comments,
 * doctypes and void elements are skipped, and attribute values are skipped whole, quoted or not, so an
 * attribute holding `<pre>` is not a tag.
 */
export function tagSequence(html: string): string[] {
	const out: string[] = [];
	let i = 0;
	const n = html.length;
	const isNameChar = (c: string) => /[A-Za-z0-9:-]/.test(c);
	while (i < n) {
		if (html[i] !== '<') {
			i++;
			continue;
		}
		if (html.startsWith('<!--', i)) {
			const end = html.indexOf('-->', i + 4);
			i = end === -1 ? n : end + 3;
			continue;
		}
		if (html[i + 1] === '!' || html[i + 1] === '?') {
			const end = html.indexOf('>', i);
			i = end === -1 ? n : end + 1;
			continue;
		}
		const closing = html[i + 1] === '/';
		let j = i + (closing ? 2 : 1);
		if (!/[A-Za-z]/.test(html[j] ?? '')) {
			i++;
			continue;
		}
		let name = '';
		while (j < n && isNameChar(html[j])) name += html[j++];
		// Skip attributes up to the closing `>`, stepping over quoted values whole.
		while (j < n && html[j] !== '>') {
			const c = html[j];
			if (c === '"' || c === "'") {
				const end = html.indexOf(c, j + 1);
				j = end === -1 ? n : end + 1;
			} else {
				j++;
			}
		}
		i = j + 1;
		name = name.toLowerCase();
		if (VOID_ELEMENTS.has(name)) continue;
		out.push(closing ? '/' + name : name);
	}
	return out;
}

/** `html` parsed as a document body by jsdom (parse5, the HTML standard's tree builder), re-serialised. */
export function reparse(html: string): string {
	return new JSDOM('<!doctype html><html><head></head><body>' + html + '</body></html>').window.document.body
		.innerHTML;
}
