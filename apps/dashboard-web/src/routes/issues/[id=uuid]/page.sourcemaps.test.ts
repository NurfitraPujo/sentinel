import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, fireEvent, within } from '@testing-library/svelte';
import type { Component } from 'svelte';
import Page from './+page.svelte';

// sentinel-sourcemaps fe.md tasks 3, 4, 5 and 6 — contract §4.7, flows `D1`–`D5`: `formatStackFrame`
// appends `:column` when `column > 0` (D5) and renders a frame with no `column` and no
// `symbolicated` key BYTE-IDENTICALLY to `ccc52aa`'s string, with no badge and no toggle (D3); the
// per-occurrence badge is *Symbolicated* when every frame is `symbolicated === true`, *Partially
// symbolicated (k of n)* for 1 ≤ k < n, nothing for k = 0 (D1/D2/D3); the *Show raw frames* toggle
// is rendered only when k > 0, swaps each raw-bearing frame to `formatStackFrame(frame.raw)` and
// flips its label, per occurrence (D4).
//
// Page data is a LITERAL typed by hand (never `$types`); the frames are contract §4.7's JSONB keys
// as §4.4 writes them. No DB, no loader, no dev server. `fetch` is stubbed only so the status
// controls the page also renders cannot reach the network. Where the badge and the toggle sit
// relative to `<h4>Stack Trace</h4>` is the delegated visual decision (screenshot owed, item 8)
// and is NOT asserted.

interface RawFrame {
	file: string;
	line: number;
	column?: number;
	function: string;
}

interface StackFrame {
	file: string;
	line: number;
	function: string;
	column?: number;
	symbolicated?: boolean;
	raw?: RawFrame;
}

interface Occurrence {
	id: string;
	issueId: string;
	environment: string;
	platform: string;
	stacktrace: StackFrame[];
	metadata: Record<string, unknown>;
	traceId: string | null;
	spanId: string | null;
	createdAt: string;
}

interface DetailIssue {
	id: string;
	projectId: string;
	errorClass: string;
	message: string;
	status: 'unresolved' | 'resolved' | 'ignored';
	issueType: 'system_error' | 'user_report';
	environment: string;
	count: number;
	firstSeen: string;
	lastSeen: string;
	affectedUserCount: number;
	affectedTenantCount: number;
	resolvedInVersion: string | null;
	snoozedUntil: Date | null;
	snoozeOccurrenceTarget: number | null;
	snoozeCountAt: number | null;
	firstReleaseVersion: string | null;
}

interface TrendPoint {
	start: string;
	count: number;
}

interface DetailPageData {
	issue: DetailIssue;
	project: { id: string; name: string } | null;
	environmentDisplayName: string | null;
	occurrences: Occurrence[];
	occurrenceTotal: number;
	relations: unknown[];
	retentionDays: number;
	trend: { hourly: TrendPoint[]; daily: TrendPoint[] };
	releases: Array<{ version: string; firstSeen: Date }>;
	canWrite: boolean;
}

const DetailPage = Page as unknown as Component<{ data: DetailPageData }>;

// `ccc52aa`'s `formatStackFrame`, verbatim — the oracle for D3's byte-identity and for D5's
// "today's string plus `:column`". Kept as a literal copy on purpose: if the page's helper drifts,
// this does not drift with it.
function formatStackFrameAtCcc52aa(frame: { file: string; line: number; function: string }): string {
	let file = frame.file;
	if (file.length > 50) {
		const start = file.slice(0, 20);
		const end = file.slice(-25);
		file = start + '...' + end;
	}
	return `at ${frame.function} (${file}:${frame.line})`;
}

// §4.7's own example, as §4.4 writes it: rewritten values on the frame, the minified ones under
// `raw`.
const SYMBOLICATED_FRAME: StackFrame = {
	file: 'src/app/page.tsx',
	line: 42,
	column: 17,
	function: 'handleClick',
	symbolicated: true,
	raw: { file: '/assets/index-3f2a1c.js', line: 1, column: 1234, function: 't' },
};

const SYMBOLICATED_FRAME_2: StackFrame = {
	file: 'src/lib/api/client.ts',
	line: 88,
	column: 5,
	function: 'post',
	symbolicated: true,
	raw: { file: '/assets/vendor-9a0d4e.js', line: 3, column: 91820, function: 'p' },
};

// Flow D5: a frame with a column that was NOT symbolicated (an unmapped chunk) — no `raw`.
const UNSYMBOLICATED_COLUMN_FRAME: StackFrame = {
	file: '/assets/index-3f2a1c.js',
	line: 1,
	column: 1234,
	function: 't',
};

// A pre-feature frame: no `column`, no `symbolicated`, no `raw` — exactly what a stored occurrence
// from before this feature carries.
const PRE_FEATURE_FRAME: StackFrame = {
	file: 'internal/handler/checkout.go',
	line: 128,
	function: 'handleCheckout',
};

const LONG_PATH_FRAME: StackFrame = {
	file: '/very/long/path/that/goes/on/and/on/and/on/node_modules/@scope/pkg/dist/index.js',
	line: 7,
	function: 'x',
};

function occurrence(id: string, stacktrace: StackFrame[]): Occurrence {
	return {
		id,
		issueId: 'issue-1',
		environment: 'production',
		platform: 'javascript',
		stacktrace,
		metadata: {},
		traceId: null,
		spanId: null,
		createdAt: '2026-09-14T10:00:00.000Z',
	};
}

function issue(): DetailIssue {
	return {
		id: 'issue-1',
		projectId: 'proj-1',
		errorClass: 'TypeError',
		message: "Cannot read properties of undefined (reading 'id')",
		status: 'unresolved',
		issueType: 'system_error',
		environment: 'production',
		count: 3,
		firstSeen: '2026-09-14T09:00:00.000Z',
		lastSeen: '2026-09-14T10:00:00.000Z',
		affectedUserCount: 1,
		affectedTenantCount: 1,
		resolvedInVersion: null,
		snoozedUntil: null,
		snoozeOccurrenceTarget: null,
		snoozeCountAt: null,
		firstReleaseVersion: '2026.09.14-3f2a1c',
	};
}

function pageData(occurrences: Occurrence[]): DetailPageData {
	return {
		issue: issue(),
		project: { id: 'proj-1', name: 'Storefront' },
		environmentDisplayName: 'Production',
		occurrences,
		occurrenceTotal: occurrences.length,
		relations: [],
		retentionDays: 30,
		trend: {
			hourly: Array.from({ length: 24 }, (_, i) => ({
				start: new Date(Date.UTC(2026, 8, 13, 11 + i)).toISOString().slice(0, 19) + 'Z',
				count: 0,
			})),
			daily: Array.from({ length: 30 }, (_, i) => ({
				start: new Date(Date.UTC(2026, 7, 16 + i)).toISOString().slice(0, 19) + 'Z',
				count: 0,
			})),
		},
		releases: [{ version: '2026.09.14-3f2a1c', firstSeen: new Date('2026-09-14T09:00:00Z') }],
		canWrite: false,
	};
}

afterEach(() => {
	vi.unstubAllGlobals();
});

function renderPage(occurrences: Occurrence[]) {
	vi.stubGlobal(
		'fetch',
		vi.fn(async () => {
			throw new Error('no network in a component test');
		})
	);
	return render(DetailPage, { data: pageData(occurrences) });
}

function blocks(): HTMLElement[] {
	return Array.from(document.querySelectorAll<HTMLElement>('.occurrence-card .stacktrace'));
}

// The exact text of the block's `<code>` — every frame followed by a newline, as `ccc52aa` renders
// it. NOT whitespace-normalised: byte-identity is the claim.
function codeOf(block: HTMLElement): string {
	return block.querySelector('pre > code')?.textContent ?? '';
}

function linesOf(block: HTMLElement): string[] {
	return codeOf(block).split('\n').filter((line) => line.length > 0);
}

function badgeOf(block: HTMLElement): HTMLElement | null {
	return within(block).queryByTestId('symbolication-badge');
}

function toggleOf(block: HTMLElement): HTMLButtonElement | null {
	return within(block).queryByTestId('raw-frames-toggle') as HTMLButtonElement | null;
}

function text(el: Element | null | undefined): string {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

describe('/issues/{id} formatStackFrame and the column (contract §4.7, flow D5)', () => {
	it('a frame with column > 0 renders at fn (file:line:column)', () => {
		renderPage([occurrence('occ-1', [UNSYMBOLICATED_COLUMN_FRAME])]);
		const [block] = blocks();
		expect(linesOf(block)).toEqual(['at t (/assets/index-3f2a1c.js:1:1234)']);
		// …which is today's string with `:column` appended, nothing else moved.
		expect(linesOf(block)[0]).toBe(formatStackFrameAtCcc52aa(UNSYMBOLICATED_COLUMN_FRAME).replace(/\)$/, ':1234)'));
	});

	it('a symbolicated frame renders its rewritten values with the column — §4.7\'s own example', () => {
		renderPage([occurrence('occ-1', [SYMBOLICATED_FRAME])]);
		expect(linesOf(blocks()[0])).toEqual(['at handleClick (src/app/page.tsx:42:17)']);
	});

	it('column: 0 (the wire\'s "unknown", §4.3) renders no trailing :0', () => {
		renderPage([occurrence('occ-1', [{ ...PRE_FEATURE_FRAME, column: 0 }])]);
		expect(linesOf(blocks()[0])).toEqual([formatStackFrameAtCcc52aa(PRE_FEATURE_FRAME)]);
		expect(codeOf(blocks()[0])).not.toContain(':0');
	});

	it('keeps the 50-char truncation (decision 8) and still appends the column after it', () => {
		renderPage([occurrence('occ-1', [{ ...LONG_PATH_FRAME, column: 9 }])]);
		const expected = formatStackFrameAtCcc52aa(LONG_PATH_FRAME).replace(/\)$/, ':9)');
		expect(expected).toContain('...');
		expect(linesOf(blocks()[0])).toEqual([expected]);
	});
});

describe('/issues/{id} a pre-feature occurrence (contract §4.7, flow D3)', () => {
	it('a frame with no column and no symbolicated key renders byte-identical to ccc52aa\'s string, with no badge and no toggle', () => {
		renderPage([occurrence('occ-1', [PRE_FEATURE_FRAME, LONG_PATH_FRAME])]);
		const [block] = blocks();

		// The whole <code> text, unnormalised: `${frame}\n` per frame, as `ccc52aa` renders it.
		expect(codeOf(block)).toBe(
			formatStackFrameAtCcc52aa(PRE_FEATURE_FRAME) + '\n' + formatStackFrameAtCcc52aa(LONG_PATH_FRAME) + '\n'
		);
		expect(codeOf(block)).toBe(
			'at handleCheckout (internal/handler/checkout.go:128)\nat x (/very/long/path/that.../@scope/pkg/dist/index.js:7)\n'
		);
		expect(badgeOf(block)).toBeNull();
		expect(toggleOf(block)).toBeNull();
		expect(text(block.querySelector('h4'))).toBe('Stack Trace');
	});

	it('symbolicated: false on every frame is D3 too — no badge, no toggle', () => {
		renderPage([
			occurrence('occ-1', [
				{ ...UNSYMBOLICATED_COLUMN_FRAME, symbolicated: false },
				{ ...PRE_FEATURE_FRAME, symbolicated: false },
			]),
		]);
		const [block] = blocks();
		expect(badgeOf(block)).toBeNull();
		expect(toggleOf(block)).toBeNull();
		expect(linesOf(block)).toEqual([
			'at t (/assets/index-3f2a1c.js:1:1234)',
			'at handleCheckout (internal/handler/checkout.go:128)',
		]);
	});
});

describe('/issues/{id} the symbolication badge (contract §4.7, flows D1, D2)', () => {
	it('renders Symbolicated when every frame is symbolicated === true (D1)', () => {
		renderPage([occurrence('occ-1', [SYMBOLICATED_FRAME, SYMBOLICATED_FRAME_2])]);
		const [block] = blocks();
		expect(text(badgeOf(block))).toBe('Symbolicated');
		expect(text(block.querySelector('h4'))).toBe('Stack Trace');
	});

	it('renders Partially symbolicated (k of n) for 1 ≤ k < n (D2)', () => {
		renderPage([
			occurrence('occ-1', [SYMBOLICATED_FRAME, UNSYMBOLICATED_COLUMN_FRAME, PRE_FEATURE_FRAME]),
		]);
		expect(text(badgeOf(blocks()[0]))).toBe('Partially symbolicated (1 of 3)');
	});

	it('counts symbolicated === true exactly — false and absent both count against k', () => {
		renderPage([
			occurrence('occ-1', [
				SYMBOLICATED_FRAME,
				SYMBOLICATED_FRAME_2,
				{ ...UNSYMBOLICATED_COLUMN_FRAME, symbolicated: false },
				PRE_FEATURE_FRAME,
			]),
		]);
		expect(text(badgeOf(blocks()[0]))).toBe('Partially symbolicated (2 of 4)');
	});

	it('is derived per occurrence: two occurrences on one issue can differ', () => {
		renderPage([
			occurrence('occ-full', [SYMBOLICATED_FRAME, SYMBOLICATED_FRAME_2]),
			occurrence('occ-partial', [SYMBOLICATED_FRAME, PRE_FEATURE_FRAME]),
			occurrence('occ-none', [PRE_FEATURE_FRAME]),
		]);
		const [full, partial, none] = blocks();
		expect(text(badgeOf(full))).toBe('Symbolicated');
		expect(text(badgeOf(partial))).toBe('Partially symbolicated (1 of 2)');
		expect(badgeOf(none)).toBeNull();
	});
});

describe('/issues/{id} the raw toggle (contract §4.7, flow D4)', () => {
	it('is rendered only when k > 0 — beside the badge, absent with it', () => {
		renderPage([
			occurrence('occ-full', [SYMBOLICATED_FRAME]),
			occurrence('occ-partial', [SYMBOLICATED_FRAME, PRE_FEATURE_FRAME]),
			occurrence('occ-none', [UNSYMBOLICATED_COLUMN_FRAME]),
		]);
		const [full, partial, none] = blocks();
		expect(text(toggleOf(full))).toBe('Show raw frames');
		expect(text(toggleOf(partial))).toBe('Show raw frames');
		expect(toggleOf(none)).toBeNull();
	});

	it('swaps each raw-bearing frame to formatStackFrame(frame.raw) and flips its label; a frame without raw renders as itself in both modes', async () => {
		renderPage([occurrence('occ-1', [SYMBOLICATED_FRAME, PRE_FEATURE_FRAME, SYMBOLICATED_FRAME_2])]);
		const [block] = blocks();
		const toggle = toggleOf(block) as HTMLButtonElement;

		expect(linesOf(block)).toEqual([
			'at handleClick (src/app/page.tsx:42:17)',
			'at handleCheckout (internal/handler/checkout.go:128)',
			'at post (src/lib/api/client.ts:88:5)',
		]);
		expect(toggle.getAttribute('aria-pressed')).toBe('false');

		await fireEvent.click(toggle);

		expect(text(toggleOf(block))).toBe('Show symbolicated frames');
		expect(toggleOf(block)?.getAttribute('aria-pressed')).toBe('true');
		expect(linesOf(block)).toEqual([
			'at t (/assets/index-3f2a1c.js:1:1234)',
			'at handleCheckout (internal/handler/checkout.go:128)',
			'at p (/assets/vendor-9a0d4e.js:3:91820)',
		]);
		// The badge is a fact about the stored frames, not about the mode — it stays.
		expect(text(badgeOf(block))).toBe('Partially symbolicated (2 of 3)');

		await fireEvent.click(toggleOf(block) as HTMLButtonElement);

		expect(text(toggleOf(block))).toBe('Show raw frames');
		expect(linesOf(block)).toEqual([
			'at handleClick (src/app/page.tsx:42:17)',
			'at handleCheckout (internal/handler/checkout.go:128)',
			'at post (src/lib/api/client.ts:88:5)',
		]);
	});

	it('a raw frame without a column renders without one — the raw form goes through the same formatter', async () => {
		renderPage([
			occurrence('occ-1', [
				{ ...SYMBOLICATED_FRAME, raw: { file: '/assets/index-3f2a1c.js', line: 1, function: 't' } },
			]),
		]);
		const [block] = blocks();
		await fireEvent.click(toggleOf(block) as HTMLButtonElement);
		expect(linesOf(block)).toEqual(['at t (/assets/index-3f2a1c.js:1)']);
	});

	it('is per occurrence: toggling one leaves the other in the symbolicated state', async () => {
		renderPage([
			occurrence('occ-a', [SYMBOLICATED_FRAME]),
			occurrence('occ-b', [SYMBOLICATED_FRAME_2]),
		]);
		const [a, b] = blocks();

		await fireEvent.click(toggleOf(a) as HTMLButtonElement);

		expect(linesOf(a)).toEqual(['at t (/assets/index-3f2a1c.js:1:1234)']);
		expect(text(toggleOf(a))).toBe('Show symbolicated frames');
		expect(linesOf(b)).toEqual(['at post (src/lib/api/client.ts:88:5)']);
		expect(text(toggleOf(b))).toBe('Show raw frames');
	});
});
