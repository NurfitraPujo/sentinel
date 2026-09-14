import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/svelte';
import type { Component } from 'svelte';
import Page from './+page.svelte';

// sentinel-releases-snooze fe.md task 1, contract §11's `@testing-library/svelte` row — flows
// `D1`–`D12`: the three facts in `.meta-info` and their strings, the control block rendered iff
// `canWrite` with each button's visibility per status, the Resolve dialog's three choices and its
// two hint strings, the Ignore dialog's two inputs (absent on a `user_report` issue, decision 43),
// each submit's request body through a mocked `fetch` (a `datetime-local` value sent as a `Z`
// instant, a blank input's key OMITTED), and the three failure branches — none of them reloading.
//
// Page data is a LITERAL typed by hand from contract §4.1 (never `$types`): `issue` carries the
// four new columns beside what the header and the facts read; `releases` and `canWrite` sit at the
// top level. No DB, no loader, no dev server. Layout, spacing and the dialogs' arrangement are the
// delegated visual decision and are NOT asserted.

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

// §4.1's page data, as the template reads it. `releases` and `canWrite` are optional ONLY so the
// stale-build case (a loader that does not send them) can be exercised — the contract makes both
// required.
interface DetailPageData {
	issue: DetailIssue;
	project: { id: string; name: string } | null;
	environmentDisplayName: string | null;
	occurrences: unknown[];
	occurrenceTotal: number;
	relations: unknown[];
	retentionDays: number;
	trend: { hourly: TrendPoint[]; daily: TrendPoint[] };
	releases?: Array<{ version: string; firstSeen: Date }>;
	canWrite?: boolean;
}

const DetailPage = Page as unknown as Component<{ data: DetailPageData }>;

const RELEASES = [
	{ version: 'v1.2.5', firstSeen: new Date('2026-09-13T08:00:00Z') },
	{ version: 'v1.2.4', firstSeen: new Date('2026-09-11T08:00:00Z') },
	{ version: 'a1b2c3d', firstSeen: new Date('2026-09-09T08:00:00Z') },
];

function issue(overrides: Partial<DetailIssue> = {}): DetailIssue {
	return {
		id: 'issue-1',
		projectId: 'proj-1',
		errorClass: 'TimeoutError',
		message: 'upstream timed out',
		status: 'unresolved',
		issueType: 'system_error',
		environment: 'production',
		count: 598,
		firstSeen: '2026-09-01T10:00:00.000Z',
		lastSeen: '2026-09-12T10:00:00.000Z',
		affectedUserCount: 12,
		affectedTenantCount: 3,
		resolvedInVersion: null,
		snoozedUntil: null,
		snoozeOccurrenceTarget: null,
		snoozeCountAt: null,
		firstReleaseVersion: null,
		...overrides,
	};
}

function pageData(overrides: Partial<DetailPageData> = {}): DetailPageData {
	return {
		issue: issue(),
		project: { id: 'proj-1', name: 'Checkout API' },
		environmentDisplayName: 'Production',
		occurrences: [],
		occurrenceTotal: 598,
		relations: [],
		retentionDays: 30,
		// sentinel-issue-trend's two dense series the template reads (never absent) — not this file's
		// subject; see `page.issue-trend.test.ts` beside it.
		trend: {
			hourly: Array.from({ length: 24 }, (_, i) => ({
				start: new Date(Date.UTC(2026, 8, 12, 15 + i)).toISOString().slice(0, 19) + 'Z',
				count: 0,
			})),
			daily: Array.from({ length: 30 }, (_, i) => ({
				start: new Date(Date.UTC(2026, 7, 15 + i)).toISOString().slice(0, 19) + 'Z',
				count: 0,
			})),
		},
		releases: RELEASES,
		canWrite: true,
		...overrides,
	};
}

function text(el: Element | null | undefined): string {
	return (el?.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function metaItems(): string[] {
	return Array.from(document.querySelectorAll('.meta-info .meta-item')).map(text);
}

// The page's own `formatDate`, reproduced so `until …` is asserted in the viewer's local time
// whatever TZ the test runs under.
function formatDateLikeThePage(date: Date): string {
	return date.toLocaleString('en-US', {
		year: 'numeric',
		month: 'short',
		day: 'numeric',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
	});
}

function controlButtons(): string[] {
	const block = screen.queryByTestId('status-controls');
	return Array.from(block?.querySelectorAll('button') ?? []).map((b) => text(b));
}

// `window.location.reload` is unforgeable on jsdom's `Location`, so the whole `location` is
// replaced for the test with one that records the call. Restored after each test.
const reloadSpy = vi.fn();
const originalLocation = window.location;
beforeEach(() => {
	reloadSpy.mockReset();
	Object.defineProperty(window, 'location', {
		configurable: true,
		writable: true,
		value: { ...originalLocation, reload: reloadSpy },
	});
});
afterEach(() => {
	Object.defineProperty(window, 'location', {
		configurable: true,
		writable: true,
		value: originalLocation,
	});
	vi.unstubAllGlobals();
});

function stubFetch(impl: (...args: unknown[]) => Promise<unknown>) {
	const fetchMock = vi.fn(impl);
	vi.stubGlobal('fetch', fetchMock);
	return fetchMock;
}

function okResponse() {
	return new Response(JSON.stringify({ success: true, status: 'resolved', changed: true }), {
		status: 200,
		headers: { 'Content-Type': 'application/json' },
	});
}

async function openResolveDialog() {
	await fireEvent.click(screen.getByRole('button', { name: 'Resolve' }));
	return screen.getByTestId('resolve-dialog');
}

async function openIgnoreDialog() {
	await fireEvent.click(screen.getByRole('button', { name: 'Ignore' }));
	return screen.getByTestId('ignore-dialog');
}

function submitButtonOf(dialog: HTMLElement): HTMLButtonElement {
	const button = dialog.querySelector('button[type="submit"]');
	expect(button).not.toBeNull();
	return button as HTMLButtonElement;
}

function sentBody(fetchMock: ReturnType<typeof vi.fn>): Record<string, unknown> {
	expect(fetchMock).toHaveBeenCalledTimes(1);
	const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
	expect(url).toBe('/api/issues/issue-1/status');
	expect(init.method).toBe('PATCH');
	return JSON.parse(String(init.body)) as Record<string, unknown>;
}

describe('/issues/{id} the three facts (contract §4.1, flow D1)', () => {
	it('renders Introduced in: after the two affected counts, with the version or — ', () => {
		render(DetailPage, { data: pageData({ issue: issue({ firstReleaseVersion: 'v1.2.3' }) }) });
		const items = metaItems();
		const occurrences = items.indexOf('Occurrences: 598');
		const tenants = items.findIndex((i) => i.startsWith('Affected tenants'));
		const introduced = items.indexOf('Introduced in: v1.2.3');
		expect(occurrences).toBeGreaterThanOrEqual(0);
		expect(tenants).toBeGreaterThan(occurrences);
		expect(introduced).toBe(tenants + 1);
	});

	it('renders Introduced in: — when firstReleaseVersion is null', () => {
		render(DetailPage, { data: pageData() });
		expect(text(screen.getByTestId('issue-introduced-in'))).toBe('Introduced in: —');
	});

	it('shows no Resolved in: and no Ignored: on an unresolved issue', () => {
		render(DetailPage, { data: pageData() });
		expect(screen.queryByTestId('issue-resolved-in')).toBeNull();
		expect(screen.queryByTestId('issue-ignored')).toBeNull();
	});

	it('renders Resolved in: {version} only while resolved', () => {
		render(DetailPage, {
			data: pageData({ issue: issue({ status: 'resolved', resolvedInVersion: 'v1.2.4' }) }),
		});
		expect(text(screen.getByTestId('issue-resolved-in'))).toBe('Resolved in: v1.2.4');
		expect(screen.queryByTestId('issue-ignored')).toBeNull();
	});

	it('renders "next release (waiting for it)" for the reserved literal next', () => {
		render(DetailPage, {
			data: pageData({ issue: issue({ status: 'resolved', resolvedInVersion: 'next' }) }),
		});
		expect(text(screen.getByTestId('issue-resolved-in'))).toBe(
			'Resolved in: next release (waiting for it)'
		);
	});

	it('renders "no version" for a resolve without one', () => {
		render(DetailPage, {
			data: pageData({ issue: issue({ status: 'resolved', resolvedInVersion: null }) }),
		});
		expect(text(screen.getByTestId('issue-resolved-in'))).toBe('Resolved in: no version');
	});

	it('does not render Resolved in: on an ignored issue that still carries a stale version', () => {
		render(DetailPage, {
			data: pageData({ issue: issue({ status: 'ignored', resolvedInVersion: 'v1.0.0' }) }),
		});
		expect(screen.queryByTestId('issue-resolved-in')).toBeNull();
	});

	it('renders Ignored: until {formatDate(snoozedUntil)}', () => {
		const until = new Date('2026-09-20T09:00:00Z');
		render(DetailPage, {
			data: pageData({ issue: issue({ status: 'ignored', snoozedUntil: until }) }),
		});
		expect(text(screen.getByTestId('issue-ignored'))).toBe(
			`Ignored: until ${formatDateLikeThePage(until)}`
		);
	});

	it('renders Ignored: for {target} more occurrences ({count − countAt} so far) through toLocaleString', () => {
		render(DetailPage, {
			data: pageData({
				issue: issue({
					status: 'ignored',
					count: 1537,
					snoozeOccurrenceTarget: 1100,
					snoozeCountAt: 1500,
				}),
			}),
		});
		expect(text(screen.getByTestId('issue-ignored'))).toBe(
			'Ignored: for 1,100 more occurrences (37 so far)'
		);
	});

	it('joins both conditions with "or" when both are set', () => {
		const until = new Date('2026-09-20T09:00:00Z');
		render(DetailPage, {
			data: pageData({
				issue: issue({
					status: 'ignored',
					count: 635,
					snoozedUntil: until,
					snoozeOccurrenceTarget: 100,
					snoozeCountAt: 598,
				}),
			}),
		});
		expect(text(screen.getByTestId('issue-ignored'))).toBe(
			`Ignored: until ${formatDateLikeThePage(until)} or for 100 more occurrences (37 so far)`
		);
	});

	it('renders Ignored: indefinitely when neither condition is set', () => {
		render(DetailPage, { data: pageData({ issue: issue({ status: 'ignored' }) }) });
		expect(text(screen.getByTestId('issue-ignored'))).toBe('Ignored: indefinitely');
		expect(screen.queryByTestId('issue-resolved-in')).toBeNull();
	});

	it('renders "0 so far" the moment a count snooze is set (flow D8)', () => {
		render(DetailPage, {
			data: pageData({
				issue: issue({ status: 'ignored', count: 598, snoozeOccurrenceTarget: 100, snoozeCountAt: 598 }),
			}),
		});
		expect(text(screen.getByTestId('issue-ignored'))).toBe(
			'Ignored: for 100 more occurrences (0 so far)'
		);
	});
});

describe('/issues/{id} the control block (contract §4.1, §2, flow D2)', () => {
	it('renders the block right of the status chip iff canWrite', () => {
		render(DetailPage, { data: pageData({ canWrite: true }) });
		const header = document.querySelector('.issue-header');
		const chip = header?.querySelector('.status');
		const block = screen.getByTestId('status-controls');
		expect(chip).not.toBeNull();
		expect(header?.contains(block)).toBe(true);
		// The chip precedes the controls in document order.
		expect(chip!.compareDocumentPosition(block) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
	});

	it('renders no controls when canWrite is false — the chip and the facts only', () => {
		render(DetailPage, { data: pageData({ canWrite: false }) });
		expect(screen.queryByTestId('status-controls')).toBeNull();
		expect(screen.queryByRole('button', { name: 'Resolve' })).toBeNull();
		expect(screen.queryByRole('button', { name: 'Ignore' })).toBeNull();
		expect(screen.queryByRole('button', { name: 'Reopen' })).toBeNull();
		expect(document.querySelector('.issue-header .status')).not.toBeNull();
	});

	it('renders no controls, and does not throw, when canWrite is absent (a stale loader build)', () => {
		const data = pageData();
		delete data.canWrite;
		expect(() => render(DetailPage, { data })).not.toThrow();
		expect(screen.queryByTestId('status-controls')).toBeNull();
	});

	it('shows Resolve and Ignore, not Reopen, on an unresolved issue', () => {
		render(DetailPage, { data: pageData({ issue: issue({ status: 'unresolved' }) }) });
		expect(controlButtons()).toEqual(['Resolve', 'Ignore']);
	});

	it('shows Ignore and Reopen, not Resolve, on a resolved issue', () => {
		render(DetailPage, { data: pageData({ issue: issue({ status: 'resolved' }) }) });
		expect(controlButtons()).toEqual(['Ignore', 'Reopen']);
	});

	it('shows Resolve and Reopen, not Ignore, on an ignored issue', () => {
		render(DetailPage, { data: pageData({ issue: issue({ status: 'ignored' }) }) });
		expect(controlButtons()).toEqual(['Resolve', 'Reopen']);
	});
});

describe('/issues/{id} the Resolve dialog (contract §4.1, flows D3–D6)', () => {
	it('offers three choices with next release selected and its hint (decision 32)', async () => {
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		const radios = Array.from(dialog.querySelectorAll('input[type="radio"]')) as HTMLInputElement[];
		expect(radios.map((r) => r.value)).toEqual(['next', 'release', 'typed']);
		expect(radios[0].checked).toBe(true);
		expect(text(dialog)).toContain('next release');
		expect(text(dialog)).toContain(
			'the first release this project has not seen yet, in any environment'
		);
		expect(text(dialog)).toContain('a release');
		expect(text(dialog)).toContain('type a version');
		// The typed input is bounded at 100 characters, as `validateResolvedInVersion` is.
		expect(screen.getByLabelText('Version').getAttribute('maxlength')).toBe('100');
		// The empty-typed case is explained beside its choice.
		expect(text(dialog)).toContain('regresses on the next occurrence');
	});

	it('populates the select from releases, newest first as the loader ordered them (D5)', async () => {
		render(DetailPage, { data: pageData() });
		await openResolveDialog();
		const select = screen.getByLabelText('Release') as HTMLSelectElement;
		expect(select.disabled).toBe(false);
		expect(Array.from(select.options).map((o) => o.value)).toEqual(['v1.2.5', 'v1.2.4', 'a1b2c3d']);
		expect(screen.queryByText(/No releases seen for this project yet/)).toBeNull();
	});

	it('disables the select with its hint on releases: [] (decisions 26, 47)', async () => {
		render(DetailPage, { data: pageData({ releases: [] }) });
		const dialog = await openResolveDialog();
		const select = screen.getByLabelText('Release') as HTMLSelectElement;
		expect(select.disabled).toBe(true);
		expect(text(dialog)).toContain(
			'No releases seen for this project yet — pick next release or type a version'
		);
		// The other two choices remain offered.
		const radios = Array.from(dialog.querySelectorAll('input[type="radio"]')) as HTMLInputElement[];
		expect(radios[0].disabled).toBe(false);
		expect(radios[2].disabled).toBe(false);
	});

	it('disables the select without throwing when data.releases is absent (a stale build)', async () => {
		const data = pageData();
		delete data.releases;
		expect(() => render(DetailPage, { data })).not.toThrow();
		const dialog = await openResolveDialog();
		const select = screen.getByLabelText('Release') as HTMLSelectElement;
		expect(select.disabled).toBe(true);
		expect(text(dialog)).toContain(
			'No releases seen for this project yet — pick next release or type a version'
		);
	});

	it('sends { status: resolved, resolvedInVersion: next } for the default choice (D4)', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		expect(sentBody(fetchMock)).toEqual({ status: 'resolved', resolvedInVersion: 'next' });
		await waitFor(() => expect(reloadSpy).toHaveBeenCalledTimes(1));
	});

	it('sends the picked version for "a release" (D5)', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		const select = screen.getByLabelText('Release') as HTMLSelectElement;
		await fireEvent.change(select, { target: { value: 'v1.2.4' } });
		const releaseRadio = dialog.querySelector('input[type="radio"][value="release"]') as HTMLInputElement;
		await fireEvent.click(releaseRadio);
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		expect(sentBody(fetchMock)).toEqual({ status: 'resolved', resolvedInVersion: 'v1.2.4' });
	});

	it('sends the typed version for "type a version" (D6)', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		const typedRadio = dialog.querySelector('input[type="radio"][value="typed"]') as HTMLInputElement;
		await fireEvent.click(typedRadio);
		await fireEvent.input(screen.getByLabelText('Version'), { target: { value: '2.0.0-rc1' } });
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		expect(sentBody(fetchMock)).toEqual({ status: 'resolved', resolvedInVersion: '2.0.0-rc1' });
	});

	it('sends { status: resolved } with no resolvedInVersion key for an empty typed value (D6)', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		const typedRadio = dialog.querySelector('input[type="radio"][value="typed"]') as HTMLInputElement;
		await fireEvent.click(typedRadio);
		await fireEvent.input(screen.getByLabelText('Version'), { target: { value: '   ' } });
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const body = sentBody(fetchMock);
		expect(body).toEqual({ status: 'resolved' });
		expect('resolvedInVersion' in body).toBe(false);
	});
});

describe('/issues/{id} the Ignore dialog (contract §4.1, flows D7–D9, decision 43)', () => {
	it('offers until and for N more occurrences on a system_error issue', async () => {
		render(DetailPage, { data: pageData({ issue: issue({ issueType: 'system_error' }) }) });
		await openIgnoreDialog();
		const until = screen.getByLabelText('Ignore until') as HTMLInputElement;
		const occurrences = screen.getByLabelText('Ignore for N more occurrences') as HTMLInputElement;
		expect(until.type).toBe('datetime-local');
		expect(occurrences.type).toBe('number');
		expect(occurrences.getAttribute('min')).toBe('1');
		expect(occurrences.getAttribute('max')).toBe('1000000');
		expect(occurrences.getAttribute('step')).toBe('1');
	});

	it('renders neither input on a user_report issue (decision 43)', async () => {
		render(DetailPage, { data: pageData({ issue: issue({ issueType: 'user_report' }) }) });
		const dialog = await openIgnoreDialog();
		expect(screen.queryByLabelText('Ignore until')).toBeNull();
		expect(screen.queryByLabelText('Ignore for N more occurrences')).toBeNull();
		expect(dialog.querySelector('input[type="datetime-local"]')).toBeNull();
		expect(dialog.querySelector('input[type="number"]')).toBeNull();
		// Ignore is still offered — as a plain ignore, with a note saying so.
		expect(submitButtonOf(dialog)).not.toBeNull();
		expect(screen.getByTestId('ignore-manual-note')).toBeTruthy();
	});

	it('sends { status: ignored } alone from a user_report issue', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData({ issue: issue({ issueType: 'user_report' }) }) });
		const dialog = await openIgnoreDialog();
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		expect(sentBody(fetchMock)).toEqual({ status: 'ignored' });
	});

	it('converts a datetime-local value to a UTC instant with Z (D7, decision 38)', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openIgnoreDialog();
		const local = '2026-09-20T16:00';
		await fireEvent.input(screen.getByLabelText('Ignore until'), { target: { value: local } });
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const body = sentBody(fetchMock);
		// The zone-less wall-clock string is what the endpoint refuses; what is sent is the viewer's
		// instant in UTC, milliseconds and `Z` — `new Date(local).toISOString()` in this TZ.
		expect(body).toEqual({ status: 'ignored', snoozedUntil: new Date(local).toISOString() });
		expect(String(body.snoozedUntil)).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
		expect(body.snoozedUntil).not.toBe(local);
		expect('snoozeOccurrences' in body).toBe(false);
	});

	it('sends snoozeOccurrences as a JSON integer (D8)', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openIgnoreDialog();
		await fireEvent.input(screen.getByLabelText('Ignore for N more occurrences'), {
			target: { value: '100' },
		});
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const body = sentBody(fetchMock);
		expect(body).toEqual({ status: 'ignored', snoozeOccurrences: 100 });
		expect(typeof body.snoozeOccurrences).toBe('number');
		expect('snoozedUntil' in body).toBe(false);
	});

	it('sends both keys when both inputs are filled', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openIgnoreDialog();
		const local = '2026-09-21T09:30';
		await fireEvent.input(screen.getByLabelText('Ignore until'), { target: { value: local } });
		await fireEvent.input(screen.getByLabelText('Ignore for N more occurrences'), {
			target: { value: '250' },
		});
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		expect(sentBody(fetchMock)).toEqual({
			status: 'ignored',
			snoozedUntil: new Date(local).toISOString(),
			snoozeOccurrences: 250,
		});
	});

	it('omits both keys — never sends "" — when both inputs are left blank (D9)', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openIgnoreDialog();
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const body = sentBody(fetchMock);
		expect(body).toEqual({ status: 'ignored' });
		expect('snoozedUntil' in body).toBe(false);
		expect('snoozeOccurrences' in body).toBe(false);
	});

	it('omits the blank key when only one input is cleared back to empty', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openIgnoreDialog();
		const occurrences = screen.getByLabelText('Ignore for N more occurrences');
		await fireEvent.input(occurrences, { target: { value: '5' } });
		await fireEvent.input(occurrences, { target: { value: '' } });
		const local = '2026-09-22T00:00';
		await fireEvent.input(screen.getByLabelText('Ignore until'), { target: { value: local } });
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const body = sentBody(fetchMock);
		expect(body).toEqual({ status: 'ignored', snoozedUntil: new Date(local).toISOString() });
		expect('snoozeOccurrences' in body).toBe(false);
	});
});

describe('/issues/{id} Reopen (contract §4.1, flow D10)', () => {
	it('sends { status: unresolved } and reloads on a 2xx', async () => {
		const fetchMock = stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData({ issue: issue({ status: 'resolved' }) }) });
		await fireEvent.click(screen.getByRole('button', { name: 'Reopen' }));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		expect(sentBody(fetchMock)).toEqual({ status: 'unresolved' });
		await waitFor(() => expect(reloadSpy).toHaveBeenCalledTimes(1));
	});
});

describe('/issues/{id} the three failure branches — none reloading (contract §4.1, flow D11)', () => {
	it('renders a 400 JSON message verbatim beside the dialog and does not reload', async () => {
		const fetchMock = stubFetch(
			async () =>
				new Response(JSON.stringify({ message: 'snoozedUntil must be a future timestamp' }), {
					status: 400,
					headers: { 'Content-Type': 'application/json' },
				})
		);
		render(DetailPage, { data: pageData() });
		const dialog = await openIgnoreDialog();
		await fireEvent.input(screen.getByLabelText('Ignore until'), {
			target: { value: '2020-01-01T00:00' },
		});
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const error = await screen.findByTestId('dialog-error');
		expect(text(error)).toBe('snoozedUntil must be a future timestamp');
		expect(error.getAttribute('role')).toBe('alert');
		// The dialog stays open, beside its error.
		expect(screen.getByTestId('ignore-dialog').contains(error)).toBe(true);
		expect(reloadSpy).not.toHaveBeenCalled();
	});

	it('renders "Request failed (HTTP 500)" for a 5xx with an HTML body and does not reload', async () => {
		const fetchMock = stubFetch(
			async () =>
				new Response('<!doctype html><html><body>Internal Error</body></html>', {
					status: 500,
					headers: { 'Content-Type': 'text/html' },
				})
		);
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const error = await screen.findByTestId('dialog-error');
		expect(text(error)).toBe('Request failed (HTTP 500)');
		expect(screen.getByTestId('resolve-dialog').contains(error)).toBe(true);
		expect(reloadSpy).not.toHaveBeenCalled();
	});

	it('renders "Request failed (HTTP 502)" for an empty non-JSON body', async () => {
		stubFetch(async () => new Response('', { status: 502 }));
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		await fireEvent.click(submitButtonOf(dialog));
		const error = await screen.findByTestId('dialog-error');
		expect(text(error)).toBe('Request failed (HTTP 502)');
		expect(reloadSpy).not.toHaveBeenCalled();
	});

	it('renders "Request failed (HTTP 403)" for JSON without a string message', async () => {
		stubFetch(
			async () =>
				new Response(JSON.stringify({ error: 'nope' }), {
					status: 403,
					headers: { 'Content-Type': 'application/json' },
				})
		);
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		await fireEvent.click(submitButtonOf(dialog));
		const error = await screen.findByTestId('dialog-error');
		expect(text(error)).toBe('Request failed (HTTP 403)');
		expect(reloadSpy).not.toHaveBeenCalled();
	});

	it('renders "Network error" for a rejected fetch and does not reload', async () => {
		const fetchMock = stubFetch(async () => {
			throw new TypeError('Failed to fetch');
		});
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(fetchMock).toHaveBeenCalled());
		const error = await screen.findByTestId('dialog-error');
		expect(text(error)).toBe('Network error');
		expect(reloadSpy).not.toHaveBeenCalled();
	});

	it('renders the failure beside the buttons for Reopen, which has no dialog, and does not reload', async () => {
		stubFetch(
			async () =>
				new Response(JSON.stringify({ message: 'Forbidden: Insufficient permissions to modify these issues' }), {
					status: 403,
					headers: { 'Content-Type': 'application/json' },
				})
		);
		render(DetailPage, { data: pageData({ issue: issue({ status: 'ignored' }) }) });
		await fireEvent.click(screen.getByRole('button', { name: 'Reopen' }));
		const error = await screen.findByTestId('status-error');
		expect(text(error)).toBe('Forbidden: Insufficient permissions to modify these issues');
		expect(document.querySelector('.issue-header')?.contains(error)).toBe(true);
		expect(reloadSpy).not.toHaveBeenCalled();
	});

	it('reloads exactly once on a 2xx and renders no error', async () => {
		stubFetch(async () => okResponse());
		render(DetailPage, { data: pageData() });
		const dialog = await openResolveDialog();
		await fireEvent.click(submitButtonOf(dialog));
		await waitFor(() => expect(reloadSpy).toHaveBeenCalledTimes(1));
		expect(screen.queryByTestId('dialog-error')).toBeNull();
		expect(screen.queryByTestId('status-error')).toBeNull();
	});
});
