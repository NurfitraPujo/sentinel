process.env.TZ = 'UTC';
// ^ First statement, on purpose (spec §8.2's timestamp audit). The card header's `formatDate` is
// `toLocaleString` with no `timeZone`, so its text depends on the process's time zone, and the golden
// was captured under UTC. vitest's default `forks` pool gives each file its own process, and Node
// re-reads TZ on assignment; P4 asserts the pin took, so a pin that did not fails loudly rather than
// as a golden mismatch.

import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { readFileSync } from 'node:fs';
import Page from './+page.svelte';
import { normaliseCardHtml } from '$lib/components/issues/html-structure';
import { pageData, PRE_V2_OCCURRENCE, PRE_V2_OCCURRENCE_NULLS } from '$lib/components/issues/occurrence-fixtures';

/**
 * P4 (sentinel-occurrence-detail-panels §10.1, §10.2, §10.4): a pre-v2 occurrence card renders the
 * same content as at the merge base, whether or not it is the selected one.
 *
 * The golden `tests/fixtures/issue-page-pre-v2-cards.golden.html` was captured from the MERGE BASE's
 * page (its first line names the commit), never from a pinned commit. To re-capture it (spec §12.2),
 * for example after a rebase onto a `main` that changed the pre-v2 card:
 *   1. BASE=$(git merge-base origin/main HEAD) (after `git fetch origin`), then
 *      git show "$BASE":'apps/dashboard-web/src/routes/issues/[id=uuid]/+page.svelte' > 'src/routes/issues/[id=uuid]/Baseline.tmp.svelte'
 *   2. With process.env.TZ = 'UTC' set first, render Baseline.tmp.svelte with
 *      pageData({ occurrences: [PRE_V2_OCCURRENCE, PRE_V2_OCCURRENCE_NULLS] }) and apply
 *      normaliseCardHtml to each `.occurrence-card` (joined with '\n'): once as rendered, and once after
 *      clicking the FIRST card's metadata toggle and then its raw-frames toggle.
 *   3. Write `<!-- captured from <the full $BASE sha> -->`, '\n', the collapsed cards, '\n', the
 *      expanded cards, '\n'. P4 drops the first line before comparing.
 *   4. Delete Baseline.tmp.svelte (and the harness); neither is ever committed.
 */

const GOLDEN_PATH = 'tests/fixtures/issue-page-pre-v2-cards.golden.html';

function golden(): string {
	const text = readFileSync(GOLDEN_PATH, 'utf8');
	expect(text.split('\n', 1)[0]).toMatch(/^<!-- captured from [0-9a-f]{40} -->$/);
	return text.slice(text.indexOf('\n') + 1);
}

beforeEach(() => {
	vi.restoreAllMocks();
	vi.stubGlobal('fetch', vi.fn().mockResolvedValue({ ok: true, status: 200, json: async () => ({}) }));
});

describe('a pre-v2 occurrence card is unchanged (P4)', () => {
	it('the time-zone pin took', () => {
		expect(new Date(0).getTimezoneOffset()).toBe(0);
	});

	it.each([
		['the first card selected', PRE_V2_OCCURRENCE.id],
		['the second card selected', PRE_V2_OCCURRENCE_NULLS.id],
		['no selection (an absent key, a stale loader build)', undefined],
	])('matches the golden with %s, before and after the metadata and raw toggles', async (_name, selected) => {
		expect(new Date(0).getTimezoneOffset()).toBe(0);
		const data = pageData({ occurrences: [PRE_V2_OCCURRENCE, PRE_V2_OCCURRENCE_NULLS] });
		if (selected === undefined) {
			delete data.selectedOccurrenceId;
			delete data.occurrenceDetail;
		} else {
			data.selectedOccurrenceId = selected;
		}
		const { container } = render(Page, { data });
		const cards = () =>
			Array.from(container.querySelectorAll('.occurrence-card')).map(normaliseCardHtml).join('\n');

		const collapsed = cards();
		const first = container.querySelector('.occurrence-card')!;
		await fireEvent.click(first.querySelector('[data-testid="metadata-toggle"]')!);
		await fireEvent.click(first.querySelector('[data-testid="raw-frames-toggle"]')!);
		const expanded = cards();

		expect(collapsed + '\n' + expanded + '\n').toBe(golden());
	});
});
