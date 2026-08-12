// M6 Feature B (docs/plans/M6_PRESIGNED_UPLOADS_AND_TOOLBAR_PLAN.md §Feature B): pure, DOM-free
// Markdown-syntax transform used by MarkdownToolbar.svelte. Kept separate from the component so
// it can be exhaustively unit-tested without jsdom/textarea machinery -- the component's only job
// is reading/writing textarea selection and delegating to this function.

export type MarkdownToolbarAction =
	| 'bold'
	| 'italic'
	| 'code'
	| 'strikethrough'
	| 'heading'
	| 'quote'
	| 'ul'
	| 'ol'
	| 'link';

export interface MarkdownActionResult {
	text: string;
	selStart: number;
	selEnd: number;
}

const WRAP_MARKERS: Record<'bold' | 'italic' | 'code' | 'strikethrough', string> = {
	bold: '**',
	italic: '_',
	code: '`',
	strikethrough: '~~',
};

const LINE_PREFIXES: Record<'heading' | 'quote' | 'ul' | 'ol', string> = {
	heading: '## ',
	quote: '> ',
	ul: '- ',
	ol: '1. ',
};

const PLACEHOLDER = 'text';
const LINK_URL_PLACEHOLDER = 'url';

function isWrapAction(action: MarkdownToolbarAction): action is keyof typeof WRAP_MARKERS {
	return action in WRAP_MARKERS;
}

function isLinePrefixAction(action: MarkdownToolbarAction): action is keyof typeof LINE_PREFIXES {
	return action in LINE_PREFIXES;
}

function applyWrap(text: string, selStart: number, selEnd: number, marker: string): MarkdownActionResult {
	const selected = text.slice(selStart, selEnd);
	const hasSelection = selected.length > 0;
	const inner = hasSelection ? selected : PLACEHOLDER;

	const before = text.slice(0, selStart);
	const after = text.slice(selEnd);
	const nextText = `${before}${marker}${inner}${marker}${after}`;

	const innerStart = selStart + marker.length;
	const innerEnd = innerStart + inner.length;

	return { text: nextText, selStart: innerStart, selEnd: innerEnd };
}

function applyLinePrefix(text: string, selStart: number, selEnd: number, prefix: string): MarkdownActionResult {
	// Extend the range to the start of the first selected line and the end of the last selected
	// line so a prefix is applied per-line across a multi-line selection.
	let lineStart = text.lastIndexOf('\n', selStart - 1) + 1;
	let lineEndSearch = text.indexOf('\n', selEnd);
	let lineEnd = lineEndSearch === -1 ? text.length : lineEndSearch;

	const block = text.slice(lineStart, lineEnd);
	const lines = block.split('\n');
	const prefixed = lines.map((line) => prefix + line).join('\n');

	const before = text.slice(0, lineStart);
	const after = text.slice(lineEnd);
	const nextText = `${before}${prefixed}${after}`;

	const newSelStart = selStart + prefix.length;
	const addedChars = prefix.length * lines.length;
	const newSelEnd = selEnd + addedChars;

	return { text: nextText, selStart: newSelStart, selEnd: newSelEnd };
}

function applyLink(text: string, selStart: number, selEnd: number): MarkdownActionResult {
	const selected = text.slice(selStart, selEnd);
	const hasSelection = selected.length > 0;
	const label = hasSelection ? selected : PLACEHOLDER;
	const before = text.slice(0, selStart);
	const after = text.slice(selEnd);

	const inserted = `[${label}](${LINK_URL_PLACEHOLDER})`;
	const nextText = `${before}${inserted}${after}`;

	// Selection lands on the label when the caller had a real selection (so the user can keep
	// typing over their chosen text is unnecessary -- URL is the next thing to fill in), and on
	// the url placeholder either way, since that is always the next field to fill.
	const urlStart = selStart + `[${label}](`.length;
	const urlEnd = urlStart + LINK_URL_PLACEHOLDER.length;

	return { text: nextText, selStart: urlStart, selEnd: urlEnd };
}

export function applyMarkdownAction(
	text: string,
	selStart: number,
	selEnd: number,
	action: MarkdownToolbarAction
): MarkdownActionResult {
	if (isWrapAction(action)) {
		return applyWrap(text, selStart, selEnd, WRAP_MARKERS[action]);
	}
	if (isLinePrefixAction(action)) {
		return applyLinePrefix(text, selStart, selEnd, LINE_PREFIXES[action]);
	}
	if (action === 'link') {
		return applyLink(text, selStart, selEnd);
	}
	// Exhaustiveness guard -- MarkdownToolbarAction has no remaining members.
	const _exhaustive: never = action;
	return _exhaustive;
}
