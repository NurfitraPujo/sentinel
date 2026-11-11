// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render } from 'svelte/server';
import { JSDOM } from 'jsdom';
import Page from './+page.svelte';
import ExpandableText from '$lib/components/issues/ExpandableText.svelte';
import OccurrenceHeader from '$lib/components/issues/OccurrenceHeader.svelte';
import StackFrames from '$lib/components/issues/StackFrames.svelte';
import TagsTable from '$lib/components/issues/TagsTable.svelte';
import RequestPanel from '$lib/components/issues/RequestPanel.svelte';
import UserPanel from '$lib/components/issues/UserPanel.svelte';
import ExceptionChain from '$lib/components/issues/ExceptionChain.svelte';
import BreadcrumbTimeline from '$lib/components/issues/BreadcrumbTimeline.svelte';
import ContextCards from '$lib/components/issues/ContextCards.svelte';
import { tagSequence, reparse } from '$lib/components/issues/html-structure';
import { BLOCK_LIMIT, INLINE_LIMIT } from '$lib/components/issues/occurrence-display';
import {
	pageData,
	flutterDetailRow,
	FLUTTER_OCCURRENCE,
	FLUTTER_DETAIL,
	HOSTILE_OCCURRENCE,
	HOSTILE_DETAIL,
	LONG_OCCURRENCE,
	LONG_DETAIL,
	PRE_V2_OCCURRENCE,
	PRE_V2_OCCURRENCE_NULLS,
	V2_MALFORMED_OCCURRENCE,
} from '$lib/components/issues/occurrence-fixtures';

/**
 * sentinel-occurrence-detail-panels §4.6 (SSR1-SSR3): markup nesting under server rendering.
 *
 * The browser's HTML parser repairs invalid nesting before hydration (`<p><pre>` becomes
 * `<p></p><pre>…</pre><p></p>`), and Svelte's compiler catches that only inside one component. So
 * every panel, and the whole card, is server-rendered with `svelte/server`, re-parsed with jsdom's
 * parse5, and its tag sequence must come back unchanged.
 */

let warnings: unknown[][] = [];

beforeEach(() => {
	warnings = [];
	vi.spyOn(console, 'warn').mockImplementation((...args) => void warnings.push(args));
	vi.spyOn(console, 'error').mockImplementation((...args) => void warnings.push(args));
});

afterEach(() => {
	expect(
		warnings.filter((args) => args.some((a) => String(a).includes('node_invalid_placement'))),
		'svelte warned about invalid placement',
	).toEqual([]);
	vi.restoreAllMocks();
});

function expectNestingKept(body: string) {
	expect(body.length).toBeGreaterThan(0);
	expect(tagSequence(reparse(body))).toEqual(tagSequence(body));
}

function renderBody(component: any, props: Record<string, unknown>): string {
	return render(component, { props }).body;
}

const FIXTURES = [
	['hostile', HOSTILE_OCCURRENCE],
	['flutter', FLUTTER_OCCURRENCE],
	['long', LONG_OCCURRENCE],
] as const;

describe('SSR1: each panel alone', () => {
	it.each([
		['inline, under the limit', { text: 'short', limit: INLINE_LIMIT }],
		['inline, over the limit', { text: 'x'.repeat(300), limit: INLINE_LIMIT }],
		['block, under the limit', { text: 'a\nb', limit: BLOCK_LIMIT, block: true }],
		['block, over the limit', { text: 'x'.repeat(3000), limit: BLOCK_LIMIT, block: true }],
	])('ExpandableText %s', (_name, props) => {
		expectNestingKept(renderBody(ExpandableText, props));
	});

	it.each(FIXTURES)('OccurrenceHeader, %s', (_name, occurrence) => {
		expectNestingKept(renderBody(OccurrenceHeader, { occurrence, issueMessage: 'Issue message' }));
	});

	it.each(FIXTURES)('StackFrames, %s', (_name, occurrence) => {
		for (const showRaw of [false, true]) {
			expectNestingKept(renderBody(StackFrames, { frames: occurrence.stacktrace, showRaw }));
		}
	});
});

const DETAILS = [
	['hostile', HOSTILE_DETAIL],
	['flutter', FLUTTER_DETAIL],
	['long', LONG_DETAIL],
] as const;

describe('SSR1: the TASK-03 panels', () => {
	it.each(DETAILS)('TagsTable, %s', (_name, detail) => {
		expectNestingKept(renderBody(TagsTable, { tags: detail.tags }));
	});

	it.each(DETAILS)('RequestPanel, %s', (_name, detail) => {
		expectNestingKept(renderBody(RequestPanel, { request: detail.request }));
	});

	it.each(DETAILS)('ExceptionChain, %s', (_name, detail) => {
		for (const platform of ['dart', 'javascript']) {
			expectNestingKept(renderBody(ExceptionChain, { exception: detail.exception, platform }));
		}
	});

	it.each(DETAILS)('UserPanel, %s', (_name, detail) => {
		expectNestingKept(renderBody(UserPanel, { user: detail.user }));
	});
});

describe('SSR1: the TASK-01 panel', () => {
	it.each(DETAILS)('ContextCards, %s', (_name, detail) => {
		expectNestingKept(renderBody(ContextCards, { contexts: detail.contexts }));
	});
});

describe('SSR1: the TASK-02 panel', () => {
	it.each(DETAILS)('BreadcrumbTimeline, %s', (_name, detail) => {
		for (const occurredAt of [detail.createdAt, new Date(String(detail.createdAt)), null]) {
			expectNestingKept(renderBody(BreadcrumbTimeline, { breadcrumbs: detail.breadcrumbs, occurredAt }));
		}
	});
});

describe('SSR2: the whole card, server-rendered as the page', () => {
	it('(a) the hostile occurrence, listed and selected', () => {
		const body = renderBody(Page, {
			data: pageData({ occurrences: [HOSTILE_OCCURRENCE], occurrenceDetail: { ...HOSTILE_OCCURRENCE, ...HOSTILE_DETAIL } }),
		});
		expect(body).toContain('data-testid="contexts"');
		expectNestingKept(body);
	});

	it('(c) the Flutter detail as an S3a selection outside the list', () => {
		const outside = flutterDetailRow('0b7e3f2a-5c1d-4e8f-9a6b-000000000003');
		const body = renderBody(Page, {
			data: pageData({ occurrences: [FLUTTER_OCCURRENCE], selectedOccurrenceId: outside.id, occurrenceDetail: outside }),
		});
		expect(body).toContain('data-testid="selected-outside-window"');
		expect(body).toContain('data-testid="breadcrumbs"');
		expectNestingKept(body);
	});

	it('(b) the Flutter occurrence, listed and selected', () => {
		const body = renderBody(Page, {
			data: pageData({ occurrences: [FLUTTER_OCCURRENCE], occurrenceDetail: flutterDetailRow() }),
		});
		expect(body).toContain('data-testid="occurrence-facts"');
		expectNestingKept(body);
	});

	it('(d) a v2 occurrence with malformed frames renders two frames and does not throw', () => {
		const body = renderBody(Page, { data: pageData({ occurrences: [V2_MALFORMED_OCCURRENCE] }) });
		expectNestingKept(body);
		expect(body.match(/data-testid="frame"/g)).toHaveLength(2);
		const doc = new JSDOM(body).window.document;
		expect(doc.body.textContent).toContain('at <unknown> (<unknown>:3)');
	});
});

describe('SSR3: the pre-v2 page', () => {
	it('server-renders with its nesting unchanged', () => {
		const body = renderBody(Page, {
			data: pageData({ occurrences: [PRE_V2_OCCURRENCE, PRE_V2_OCCURRENCE_NULLS] }),
		});
		expect(body).toContain('class="occurrence-card');
		expectNestingKept(body);
	});
});
