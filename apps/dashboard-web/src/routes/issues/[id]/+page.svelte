<script lang="ts">
	import type { PageData } from './$types';
	import { untrack } from 'svelte';
	import { SvelteSet } from 'svelte/reactivity';

	let { data } = $props();

	// SvelteSet ensures .add() and .delete() trigger Svelte 5 fine-grained reactivity.
	const expandedMetadata = new SvelteSet<string>();

	// sentinel-sourcemaps §4.7, flow `D4` — the occurrences currently showing their RAW frames. Per
	// occurrence, never page-wide (fe.md decisions: two occurrences on one issue can differ in what
	// was symbolicated). A `SvelteSet`, not `$state(new Set())`: `$state` proxies plain objects and
	// arrays only, so `.add`/`.delete` on a raw `Set` re-renders nothing.
	const rawFrames = new SvelteSet<string>();

	function toggleRawFrames(id: string) {
		if (rawFrames.has(id)) {
			rawFrames.delete(id);
		} else {
			rawFrames.add(id);
		}
	}

	function formatDate(date: string | Date | null): string {
		if (!date) return 'Unknown';
		return new Date(date).toLocaleString('en-US', {
			year: 'numeric',
			month: 'short',
			day: 'numeric',
			hour: '2-digit',
			minute: '2-digit',
			second: '2-digit',
		});
	}

	function toggleMetadata(id: string) {
		if (expandedMetadata.has(id)) {
			expandedMetadata.delete(id);
		} else {
			expandedMetadata.add(id);
		}
	}

	// sentinel-sourcemaps §4.7 / flow `D5`: `:column` is appended when present and > 0 — `0` is the
	// wire's "unknown" (§4.3) and a pre-feature frame has no key at all, so both render exactly the
	// pre-feature string. The 50-char truncation stays (decision 8: original paths are verbatim).
	function formatStackFrame(frame: { file: string; line: number; column?: number; function: string }): string {
		let file = frame.file;
		if (file.length > 50) {
			const start = file.slice(0, 20);
			const end = file.slice(-25);
			file = start + '...' + end;
		}
		const column = frame.column && frame.column > 0 ? `:${frame.column}` : '';
		return `at ${frame.function} (${file}:${frame.line}${column})`;
	}

	// §4.7 — the per-occurrence symbolication state, derived client-side from the frames (contract
	// D6: there is no occurrence-level field). `k` counts `symbolicated === true` exactly; a frame
	// with the key false, or without it, is not symbolicated.
	function countSymbolicated(frames: StackFrame[]): number {
		return frames.filter((frame) => frame.symbolicated === true).length;
	}

	// The badge's text: *Symbolicated* when every frame is, *Partially symbolicated (k of n)* when
	// some are, `null` — no badge — when none (flows `D1`–`D3`).
	function symbolicationBadge(frames: StackFrame[]): string | null {
		const n = frames.length;
		const k = countSymbolicated(frames);
		if (n === 0 || k === 0) return null;
		if (k === n) return 'Symbolicated';
		return `Partially symbolicated (${k} of ${n})`;
	}

	// `D4`: in raw mode a frame with `raw` renders `raw`'s values; a frame without `raw` renders as
	// itself in both modes (it was never rewritten, so its raw form IS its form).
	function displayedFrame(frame: StackFrame, raw: boolean): { file: string; line: number; column?: number; function: string } {
		return raw && frame.raw ? frame.raw : frame;
	}

	function getDateGroup(dateStr: string | Date | null): string {
		if (!dateStr) return 'Unknown';
		const date = new Date(dateStr);
		const today = new Date();
		const yesterday = new Date(today);
		yesterday.setDate(yesterday.getDate() - 1);

		if (date.toDateString() === today.toDateString()) return 'Today';
		if (date.toDateString() === yesterday.toDateString()) return 'Yesterday';
		return date.toLocaleDateString('en-US', { month: 'long', day: 'numeric', year: 'numeric' });
	}

	import IssueRelations from '$lib/components/issues/IssueRelations.svelte';
	import TrendBarChart from '$lib/components/issues/TrendBarChart.svelte';
	import { filterKnownRelationTypes } from '$lib/types/relation-type';

	// sentinel-issue-trend §3 "chart clocks", decision 11. The HOURLY chart writes each bucket's
	// start in the viewer's local time through `formatDate` above — a UTC hour bucket rendered
	// locally is still that hour's start, so no "UTC" word appears on it. The DAILY chart is UTC
	// calendar days and says so: its heading carries "(UTC)" and every title and label is the UTC
	// date — NEVER `formatDate`, which is `toLocaleString` with a time of day and renders
	// `2026-09-13T00:00:00Z` as `Sep 12, 2026, 7:00 PM` for a UTC−5 viewer: every bar one date early.
	function formatUtcDate(start: string): string {
		return new Date(start).toLocaleDateString('en-US', {
			timeZone: 'UTC',
			month: 'short',
			day: 'numeric',
			year: 'numeric',
		});
	}

	// Guidance, not acceptance (§4.2): the short axis labels — the local hour, the UTC day.
	function formatLocalHour(start: string): string {
		return new Date(start).toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' });
	}

	function formatUtcDay(start: string): string {
		return new Date(start).toLocaleDateString('en-US', {
			timeZone: 'UTC',
			month: 'short',
			day: 'numeric',
		});
	}

	// Decision 16: below 30 days of retention the leading days are zero by construction and the day
	// the cutoff falls in is partial; the heading says so, after the sum. `retentionDays` is page
	// data (affected-users decision 21) — never a literal 30 here.
	let dailyHeadingSuffix = $derived(
		data.retentionDays < 30 ? ` (data kept for ${data.retentionDays} days)` : ''
	);

	// The frame as the JSONB stores it. `column`, `symbolicated` and `raw` are sentinel-sourcemaps
	// §4.7's additions, all optional: a pre-feature occurrence carries none of them.
	interface StackFrame {
		file: string;
		line: number;
		function: string;
		column?: number;
		symbolicated?: boolean;
		raw?: { file: string; line: number; column?: number; function: string };
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
		createdAt: string | Date | null;
	}

	interface OccurrenceGroup {
		date: string;
		occurrences: Occurrence[];
	}

	let occurrenceGroups = $derived((data.occurrences as Occurrence[]).reduce((groups: OccurrenceGroup[], occ) => {
		const groupLabel = getDateGroup(occ.createdAt as string | Date | null);
		const existing = groups.find(g => g.date === groupLabel);
		if (existing) {
			existing.occurrences.push(occ);
		} else {
			groups.push({ date: groupLabel, occurrences: [occ] });
		}
		return groups;
	}, []));

	let activeOccurrenceId = $state<string | null>(null);
	let allOccurrencesModalOpen = $state(false);

	let selectedOccurrence = $derived.by(() => {
		const occs = (data.occurrences as Occurrence[]) || [];
		if (occs.length === 0) return null;
		if (!activeOccurrenceId) return occs[0];
		return occs.find((o) => o.id === activeOccurrenceId) ?? occs[0];
	});

	function selectOccurrence(id: string) {
		activeOccurrenceId = id;
	}

	function toggleAllOccurrencesModal() {
		allOccurrencesModalOpen = !allOccurrencesModalOpen;
	}

	// data.relations comes straight off the issue_relations DB column, which is a plain varchar
	// with no DB-level enum — TypeScript only ever knows it as `relationType: string`. Narrow to
	// the three values the migration's CHECK constraint (and the relations API) actually permit
	// here, at the boundary where DB data enters IssueRelations' typed props, dropping (and
	// logging) anything unrecognized rather than casting it through.
	let knownRelations = $derived(filterKnownRelationTypes(data.relations || []));

	// sentinel-releases-snooze §4.1 "The controls", flows `D3`–`D12`. ONE submit helper for every
	// status write on this page — the three controls below and `IssueRelations.svelte`'s "Mark
	// Resolved" prompt (`D12`, which keeps sending `{ status: 'resolved' }` and nothing else).
	//
	// It reloads ONLY on a `2xx`. The helper this replaced reloaded after ANY response and only
	// `console.error`ed a rejected fetch, so a refused write (`400 snoozedUntil must be a future
	// timestamp`) looked exactly like a no-op. Three failure branches, none reloading (`D11`): a
	// non-`2xx` whose body is JSON with a string `message` (SvelteKit's `error()` shape, §7 — every
	// `4xx` here) renders that string verbatim and is never matched on; a non-`2xx` with any other
	// body (a `5xx` HTML page, an empty body, non-JSON) renders `Request failed (HTTP {status})`; a
	// rejected `fetch` (network down, aborted) renders `Network error`. The reload itself is kept on
	// purpose — it goes back through the loader's guards and picks up the fresh row.
	type StatusBody = {
		status: 'unresolved' | 'resolved' | 'ignored';
		resolvedInVersion?: string;
		snoozedUntil?: string;
		snoozeOccurrences?: number;
	};

	let openDialog = $state<'resolve' | 'ignore' | null>(null);
	let submitting = $state(false);
	let submitError = $state<string | null>(null);

	async function failureMessage(response: Response): Promise<string> {
		try {
			const parsed: unknown = await response.json();
			const message = (parsed as { message?: unknown } | null)?.message;
			if (typeof message === 'string') return message;
		} catch {
			// not JSON — fall through to the generic string
		}
		return `Request failed (HTTP ${response.status})`;
	}

	async function submitStatus(body: StatusBody): Promise<void> {
		// Pinned at submit time: the issue this write is FOR. Same-route navigation reuses this
		// component (see the reset effect below), so by the time the response lands `data.issue.id`
		// may already be another issue's — its outcome is then not this page's to render or reload.
		const issueId = data.issue.id;
		submitting = true;
		submitError = null;
		let response: Response;
		try {
			response = await fetch(`/api/issues/${issueId}/status`, {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body),
			});
		} catch {
			if (data.issue.id === issueId) submitError = 'Network error';
			submitting = false;
			return;
		}
		if (data.issue.id !== issueId) {
			submitting = false;
			return;
		}
		if (response.ok) {
			window.location.reload();
			return;
		}
		submitError = await failureMessage(response);
		submitting = false;
	}

	// §4.1: the control block renders for `canWrite` callers only — a UX nicety, the endpoint
	// re-checks. `releases` is the picker's list. Both are guarded against ABSENCE (a stale loader
	// build): `pnpm build` does not type-check, so a missing key would `500` the page rather than
	// fail a gate — no controls / a disabled select, never a throw.
	let canWrite = $derived(data.canWrite === true);
	let pickerReleases = $derived(data.releases ?? []);
	// Decision 43: a manual (`user_report`) issue has no occurrences and never passes `StoreEvent`,
	// and the processor is the only thing that ends a snooze (decision 19) — so a snooze on it
	// could never end. Ignore there is a plain ignore and the dialog offers neither condition.
	let snoozeable = $derived(data.issue.issueType !== 'user_report');

	// The Resolve dialog (`D3`–`D6`): *next release* by default.
	let resolveChoice = $state<'next' | 'release' | 'typed'>('next');
	let resolveRelease = $state('');
	let resolveTyped = $state('');
	// The Ignore dialog (`D7`–`D9`): both optional, neither = indefinitely.
	let ignoreUntil = $state('');
	// `bind:value` on a `type="number"` input is a number, and `null` while the input is blank.
	let ignoreOccurrences = $state<number | null>(null);

	function resetDrafts() {
		submitError = null;
		resolveChoice = 'next';
		resolveRelease = pickerReleases[0]?.version ?? '';
		resolveTyped = '';
		ignoreUntil = '';
		ignoreOccurrences = null;
	}

	function showDialog(which: 'resolve' | 'ignore') {
		resetDrafts();
		openDialog = which;
	}

	function closeDialog() {
		openDialog = null;
		submitError = null;
	}

	// The dialogs are non-modal (`<dialog open>`, no `showModal()`), and `IssueRelations` renders
	// same-route `<a href="/issues/{id}">` links that SvelteKit serves by REUSING this component
	// with new `data` — so a draft opened on issue A would otherwise survive the click on a related
	// issue behind it, sit open on B, and its confirm would PATCH B with A's picked version (B may
	// be in another project, whose picker never offered it). Every dialog and draft belongs to ONE
	// issue: when `data.issue.id` changes, close what is open and discard the draft. `data` is
	// replaced wholesale on every navigation, including one that lands on the SAME issue with a
	// refreshed row, so the id is compared against the one last shown, not merely tracked; the
	// resets are untracked so this never re-runs on the state it writes. The initial capture is
	// deliberately a snapshot (`untrack`), not a reactive reference: it is the "last shown" memo.
	let shownIssueId = untrack(() => data.issue.id);
	$effect(() => {
		const issueId = data.issue.id;
		if (issueId === shownIssueId) return;
		shownIssueId = issueId;
		untrack(() => {
			openDialog = null;
			submitting = false;
			resetDrafts();
		});
	});

	// §4.1's table, exactly: `next`, a picked version, a typed version — and an EMPTY typed value
	// sends no `resolvedInVersion` at all (today's write; the dialog says it regresses on the next
	// occurrence). Trimmed here as `validateResolvedInVersion` would trim it, so blank is blank.
	function submitResolve() {
		const body: StatusBody = { status: 'resolved' };
		if (resolveChoice === 'next') {
			body.resolvedInVersion = 'next';
		} else if (resolveChoice === 'release') {
			body.resolvedInVersion = resolveRelease;
		} else {
			const typed = resolveTyped.trim();
			if (typed !== '') body.resolvedInVersion = typed;
		}
		void submitStatus(body);
	}

	// A `datetime-local` value is a zone-less wall-clock string (`2026-09-20T16:00`); the endpoint
	// refuses exactly that shape (decision 38), so it is converted to the full UTC instant with `Z`
	// through `new Date(value).toISOString()` — the viewer's own zone, as they read the picker. A
	// blank input's key is OMITTED, never sent as `''` (§3 treats a blank as omitted too, but the
	// body §4.1 shows is the one without the key). On a `user_report` issue neither input exists,
	// so this is the plain `{ status: 'ignored' }`.
	function submitIgnore() {
		const body: StatusBody = { status: 'ignored' };
		if (snoozeable) {
			if (ignoreUntil !== '') body.snoozedUntil = new Date(ignoreUntil).toISOString();
			if (ignoreOccurrences !== null && Number.isFinite(ignoreOccurrences)) {
				body.snoozeOccurrences = ignoreOccurrences;
			}
		}
		void submitStatus(body);
	}

	// The three facts (`D1`), strings verbatim from §4.1. `Resolved in:` only while `resolved`,
	// `Ignored:` only while `ignored`; the `{#if}`s are in the markup. Numbers through
	// `toLocaleString()` like `Occurrences:`; `snooze_count_at` is `NULL` iff the target is (the
	// migrated `check_snooze_pair`), so the subtraction never sees a lone null.
	let resolvedInFact = $derived(
		data.issue.resolvedInVersion === 'next'
			? 'next release (waiting for it)'
			: (data.issue.resolvedInVersion ?? 'no version')
	);
	let ignoredFact = $derived.by(() => {
		const parts: string[] = [];
		if (data.issue.snoozedUntil) parts.push(`until ${formatDate(data.issue.snoozedUntil)}`);
		if (data.issue.snoozeOccurrenceTarget != null) {
			const soFar = Number(data.issue.count) - Number(data.issue.snoozeCountAt ?? 0);
			parts.push(
				`for ${Number(data.issue.snoozeOccurrenceTarget).toLocaleString()} more occurrences (${soFar.toLocaleString()} so far)`
			);
		}
		return parts.length === 0 ? 'indefinitely' : parts.join(' or ');
	});
</script>

<!-- Decision 16's zero shape, the same as the list card's: the dash replaces the number only. -->
{#snippet affectedFigure(count: number, kind: 'user' | 'tenant')}{#if count === 0}<span class="zero" title="no {kind} id reported in the last {data.retentionDays} days">—</span>{:else}{Number(count).toLocaleString()}{/if}{/snippet}

<div class="issue-detail">
	<header class="detail-nav">
		<a href="/" class="back-link">&larr; Back to Issues</a>
	</header>

	<div class="issue-header">
		<h1 class="error-title">{data.issue.errorClass}</h1>
		<!--
			sentinel-releases-snooze §4.1 "The controls", flows `D2`–`D11`. Right of the chip: the chip
			is the state, the controls change it. Rendered iff `canWrite` (`D2`); each button's
			visibility follows §4.1's table. The dialogs are plain `<dialog>`/form markup in this page's
			own CSS (§5) — their FIELDS, STRINGS and BODIES are acceptance; their arrangement is the
			delegated visual decision. A failure string sits beside the open dialog; when no dialog is
			open (the relations panel's prompt, `D12`) it sits here beside the buttons.
		-->
		<div class="status-block">
			<span class="status {data.issue.status}">{data.issue.status}</span>
			{#if canWrite}
				<div class="status-controls" data-testid="status-controls">
					{#if data.issue.status !== 'resolved'}
						<button type="button" class="btn-status btn-resolve" onclick={() => showDialog('resolve')}>Resolve</button>
					{/if}
					{#if data.issue.status !== 'ignored'}
						<button type="button" class="btn-status btn-ignore" onclick={() => showDialog('ignore')}>Ignore</button>
					{/if}
					{#if data.issue.status !== 'unresolved'}
						<button type="button" class="btn-status btn-reopen" disabled={submitting} onclick={() => submitStatus({ status: 'unresolved' })}>Reopen</button>
					{/if}
				</div>
			{/if}
			{#if submitError && openDialog === null}
				<p class="status-error" role="alert" data-testid="status-error">{submitError}</p>
			{/if}
		</div>
	</div>

	{#if openDialog === 'resolve'}
		<dialog class="status-dialog" open aria-labelledby="resolve-dialog-title" data-testid="resolve-dialog">
			<form class="dialog-form" onsubmit={(e) => { e.preventDefault(); submitResolve(); }}>
				<h2 class="dialog-title" id="resolve-dialog-title">Resolve issue</h2>
				<fieldset class="dialog-fieldset">
					<legend class="visually-hidden">Resolved in</legend>
					<label class="choice">
						<input type="radio" name="resolveChoice" value="next" bind:group={resolveChoice} />
						<span class="choice-label">next release</span>
						<span class="choice-hint">the first release this project has not seen yet, in any environment</span>
					</label>
					<label class="choice">
						<input type="radio" name="resolveChoice" value="release" bind:group={resolveChoice} disabled={pickerReleases.length === 0} />
						<span class="choice-label">a release</span>
						<select
							class="choice-input"
							aria-label="Release"
							bind:value={resolveRelease}
							disabled={pickerReleases.length === 0}
							onfocus={() => (resolveChoice = 'release')}
							onchange={() => (resolveChoice = 'release')}
						>
							{#each pickerReleases as release (release.version)}
								<option value={release.version}>{release.version}</option>
							{/each}
						</select>
						{#if pickerReleases.length === 0}
							<span class="choice-hint">No releases seen for this project yet — pick next release or type a version</span>
						{/if}
					</label>
					<label class="choice">
						<input type="radio" name="resolveChoice" value="typed" bind:group={resolveChoice} />
						<span class="choice-label">type a version</span>
						<input
							type="text"
							class="choice-input"
							aria-label="Version"
							maxlength="100"
							placeholder="2.0.0-rc1"
							bind:value={resolveTyped}
							onfocus={() => (resolveChoice = 'typed')}
							oninput={() => (resolveChoice = 'typed')}
						/>
						<span class="choice-hint">left empty, the issue regresses on the next occurrence</span>
					</label>
				</fieldset>
				{#if submitError}
					<p class="status-error" role="alert" data-testid="dialog-error">{submitError}</p>
				{/if}
				<div class="dialog-actions">
					<button type="submit" class="btn-status btn-resolve" disabled={submitting}>Resolve</button>
					<button type="button" class="btn-status btn-cancel" onclick={closeDialog}>Cancel</button>
				</div>
			</form>
		</dialog>
	{/if}

	{#if openDialog === 'ignore'}
		<dialog class="status-dialog" open aria-labelledby="ignore-dialog-title" data-testid="ignore-dialog">
			<form class="dialog-form" onsubmit={(e) => { e.preventDefault(); submitIgnore(); }}>
				<h2 class="dialog-title" id="ignore-dialog-title">Ignore issue</h2>
				{#if snoozeable}
					<fieldset class="dialog-fieldset">
						<legend class="visually-hidden">Ignore conditions</legend>
						<label class="choice">
							<span class="choice-label">until</span>
							<input type="datetime-local" class="choice-input" aria-label="Ignore until" bind:value={ignoreUntil} />
						</label>
						<label class="choice">
							<span class="choice-label">for N more occurrences</span>
							<input type="number" class="choice-input" aria-label="Ignore for N more occurrences" min="1" max="1000000" step="1" placeholder="100" bind:value={ignoreOccurrences} />
						</label>
						<p class="choice-hint">Leave both empty to ignore indefinitely. Either condition ends the ignore.</p>
					</fieldset>
				{:else}
					<p class="choice-hint" data-testid="ignore-manual-note">A manual issue has no occurrences, so it is ignored until someone reopens it.</p>
				{/if}
				{#if submitError}
					<p class="status-error" role="alert" data-testid="dialog-error">{submitError}</p>
				{/if}
				<div class="dialog-actions">
					<button type="submit" class="btn-status btn-ignore" disabled={submitting}>Ignore</button>
					<button type="button" class="btn-status btn-cancel" onclick={closeDialog}>Cancel</button>
				</div>
			</form>
		</dialog>
	{/if}

	<p class="issue-message">{data.issue.message}</p>

	<div class="issue-info-ribbon">
		<div class="ribbon-grid">
			<div class="ribbon-card">
				<span class="ribbon-label">Project & Environment</span>
				<strong class="ribbon-value">{data.project?.name ?? 'Unknown'} • {data.environmentDisplayName ?? data.issue.environment}</strong>
			</div>
			<div class="ribbon-card">
				<span class="ribbon-label">First Seen</span>
				<span class="ribbon-value mono">{formatDate(data.issue.firstSeen)}</span>
			</div>
			<div class="ribbon-card">
				<span class="ribbon-label">Last Seen</span>
				<span class="ribbon-value mono">{formatDate(data.issue.lastSeen)}</span>
			</div>
			<div class="ribbon-card">
				<span class="ribbon-label">Total Occurrences</span>
				<strong class="ribbon-value count-highlight">{Number(data.issue.count).toLocaleString()}</strong>
			</div>
			<div class="ribbon-card" data-testid="issue-affected-users">
				<span class="ribbon-label">Affected Users (30d)</span>
				<span class="ribbon-value mono">{@render affectedFigure(data.issue.affectedUserCount, 'user')}</span>
			</div>
			<div class="ribbon-card" data-testid="issue-affected-tenants">
				<span class="ribbon-label">Affected Tenants (30d)</span>
				<span class="ribbon-value mono">{@render affectedFigure(data.issue.affectedTenantCount, 'tenant')}</span>
			</div>
			<div class="ribbon-card" data-testid="issue-introduced-in">
				<span class="ribbon-label">Introduced In</span>
				<strong class="ribbon-value release-highlight">{data.issue.firstReleaseVersion ?? '—'}</strong>
			</div>
			{#if data.issue.status === 'resolved'}
				<div class="ribbon-card" data-testid="issue-resolved-in">
					<span class="ribbon-label">Resolved In</span>
					<span class="ribbon-value">{resolvedInFact}</span>
				</div>
			{/if}
			{#if data.issue.status === 'ignored'}
				<div class="ribbon-card" data-testid="issue-ignored">
					<span class="ribbon-label">Ignored</span>
					<span class="ribbon-value">{ignoredFact}</span>
				</div>
			{/if}
		</div>
	</div>

	<!--
		sentinel-issue-trend §4.2, flows `D1`–`D6` (decisions 7, 11, 13, 16). Between `.issue-info` and
		the Occurrence History: the two windowed series read as a summary of what the list below
		details. Each heading names its window — `Last 24 hours: n`, `Last 30 days (UTC): n` — so the
		three numbers on this page (those two and the lifetime `Occurrences:` above) can differ on
		screen without any of them lying (`D6`). Both series arrive dense from the loader; the daily
		one is already summed per UTC day; nothing is computed here but the sums in the headings.
		Side by side, stacking under the page's narrow breakpoint — guidance, not acceptance.
	-->
	<section class="trend" data-testid="issue-trend">
		<h2 class="section-title">Trend</h2>
		<div class="trend-charts">
			<TrendBarChart
				heading="Last 24 hours"
				points={data.trend.hourly}
				formatStart={formatDate}
				formatLabel={formatLocalHour}
				lastSuffix=" (current hour)"
				labelEvery={6}
				testId="trend-hourly"
			/>
			<TrendBarChart
				heading="Last 30 days (UTC)"
				headingSuffix={dailyHeadingSuffix}
				points={data.trend.daily}
				formatStart={formatUtcDate}
				formatLabel={formatUtcDay}
				labelEvery={7}
				testId="trend-daily"
			/>
		</div>
	</section>

	<IssueRelations
		currentIssueId={data.issue.id}
		initialRelations={knownRelations}
		onStatusChangeRequest={(status) => submitStatus({ status })}
	/>

	<section class="occurrences">
		<div class="occurrences-section-header">
			<h2 class="section-title">Occurrence Workspace ({Number(data.occurrenceTotal ?? data.occurrences.length).toLocaleString()})</h2>
			{#if (data.occurrenceTotal ?? data.occurrences.length) > data.occurrences.length}
				<p class="occurrence-window">
					Showing the {data.occurrences.length.toLocaleString()} most recent.
				</p>
			{/if}
		</div>

		{#if (data.occurrences as Occurrence[]).length === 0}
			<div class="occurrences-empty-state">
				<p class="empty-title">No occurrences recorded</p>
				<p class="empty-desc">This issue was created without attached runtime occurrences (e.g. user report or manual intake).</p>
			</div>
		{:else if selectedOccurrence}
			<div class="occurrence-workspace-layout">
				<!-- Left Main: Active Selected Occurrence -->
				<div class="occurrence-main-panel">
					<!-- Active Occurrence Hero Banner -->
					<div class="occurrence-hero-banner">
						<div class="hero-top-row">
							<div class="hero-id-badge">
								<strong class="hero-id">#{selectedOccurrence.id.slice(0, 10)}</strong>
								{#if selectedOccurrence.id === (data.occurrences[0]?.id)}
									<span class="badge new">LATEST</span>
								{/if}
							</div>
							<span class="hero-time">{formatDate(selectedOccurrence.createdAt)}</span>
						</div>
						<div class="hero-meta-row">
							<span>Env: <strong class="highlight-text">{selectedOccurrence.environment}</strong></span>
							<span>Platform: <strong class="highlight-text">{selectedOccurrence.platform}</strong></span>
							{#if selectedOccurrence.traceId}
								<span>Trace: <strong class="trace-id">{selectedOccurrence.traceId}</strong></span>
								{#if selectedOccurrence.spanId}
									<span>Span: <strong class="span-id">{selectedOccurrence.spanId}</strong></span>
								{/if}
							{/if}
						</div>
					</div>

					<!-- Stack Trace Viewer -->
					{#if selectedOccurrence.stacktrace && selectedOccurrence.stacktrace.length > 0}
						{@const badge = symbolicationBadge(selectedOccurrence.stacktrace)}
						{@const showRaw = rawFrames.has(selectedOccurrence.id)}
						<div class="stacktrace-container">
							<div class="stacktrace-heading">
								<h3>Stack Trace</h3>
								<div class="stacktrace-tools">
									{#if badge}
										<span class="badge symbolication" data-testid="symbolication-badge">{badge}</span>
										<button
											type="button"
											class="raw-frames-toggle"
											data-testid="raw-frames-toggle"
											aria-pressed={showRaw}
											onclick={() => toggleRawFrames(selectedOccurrence!.id)}
										>
											{showRaw ? 'Show symbolicated frames' : 'Show raw frames'}
										</button>
									{/if}
								</div>
							</div>
							<pre class="stacktrace-code"><code>{#each selectedOccurrence.stacktrace as frame}{formatStackFrame(displayedFrame(frame, showRaw))}
{/each}</code></pre>
						</div>
					{/if}

					<!-- Runtime Request & Metadata Box -->
					<div class="metadata-container">
						<div class="metadata-heading">
							<h3>Request & Runtime Metadata</h3>
							<button
								type="button"
								class="metadata-toggle"
								data-testid="metadata-toggle"
								aria-pressed={expandedMetadata.has(selectedOccurrence.id)}
								onclick={() => toggleMetadata(selectedOccurrence!.id)}
							>
								{expandedMetadata.has(selectedOccurrence.id) ? 'Collapse' : 'Expand'}
							</button>
						</div>

						<div class="metadata-code-block" data-testid="occurrence-metadata">
							{#if selectedOccurrence.metadata && Object.keys(selectedOccurrence.metadata).length > 0}
								<pre><code>{JSON.stringify(selectedOccurrence.metadata, null, 2)}</code></pre>
							{:else}
								<p class="metadata-empty">No metadata recorded for this event.</p>
							{/if}
						</div>
					</div>
				</div>

				<!-- Right Sidebar: Occurrence History Timeline -->
				<aside class="occurrence-sidebar-panel">
					<div class="sidebar-header">
						<h3 class="sidebar-title">Recent Timeline</h3>
						<span class="sidebar-count">{data.occurrences.length} events</span>
					</div>

					<div class="sidebar-list">
						{#each (data.occurrences as Occurrence[]).slice(0, 10) as occ, i}
							<button
								type="button"
								class="sidebar-item"
								class:active={selectedOccurrence.id === occ.id}
								onclick={() => selectOccurrence(occ.id)}
							>
								<div class="sidebar-item-top">
									<strong class="sidebar-item-id">
										#{occ.id.slice(0, 10)}
										{#if i === 0}<span class="latest-tag">(Latest)</span>{/if}
									</strong>
									<span class="sidebar-item-time">{formatDate(occ.createdAt)}</span>
								</div>
								<div class="sidebar-item-meta">
									<span class="badge env-micro">{occ.environment}</span>
									{#if occ.traceId}
										<span class="trace-micro">trace: {occ.traceId.slice(0, 8)}...</span>
									{/if}
								</div>
							</button>
						{/each}
					</div>

					{#if data.occurrences.length > 0}
						<div class="sidebar-actions">
							<button
								type="button"
								class="btn-status btn-view-all"
								onclick={toggleAllOccurrencesModal}
							>
								View All {data.occurrences.length} Occurrences
							</button>
						</div>
					{/if}
				</aside>
			</div>
		{/if}

		<!-- All Occurrences Modal / Drawer -->
		{#if allOccurrencesModalOpen}
			<!-- svelte-ignore a11y_click_events_have_key_events -->
			<div class="modal-backdrop" onclick={toggleAllOccurrencesModal} role="presentation">
				<div class="modal-dialog" onclick={(e) => e.stopPropagation()} role="dialog" aria-modal="true" tabindex="-1" aria-labelledby="all-occ-modal-title">
					<div class="modal-header">
						<div>
							<h2 id="all-occ-modal-title" class="modal-title">All Occurrences History</h2>
							<p class="modal-subtitle">Full recorded events for {data.issue.errorClass}</p>
						</div>
						<button type="button" class="btn-status btn-close" onclick={toggleAllOccurrencesModal}>✕ Close</button>
					</div>

					<div class="modal-body">
						{#each (data.occurrences as Occurrence[]) as occ, i}
							<div class="modal-occ-row" class:active={selectedOccurrence?.id === occ.id}>
								<div class="modal-occ-info">
									<div class="modal-occ-tags">
										<strong class="modal-occ-id">#{occ.id}</strong>
										<span class="badge env">{occ.environment}</span>
										<span class="badge platform">{occ.platform}</span>
									</div>
									<div class="modal-occ-details">
										{#if occ.traceId}
											<span>Trace: <code>{occ.traceId}</code></span>
										{/if}
										<span>• {formatDate(occ.createdAt)}</span>
									</div>
								</div>
								<button
									type="button"
									class="btn-status btn-inspect"
									onclick={() => {
										selectOccurrence(occ.id);
										toggleAllOccurrencesModal();
									}}
								>
									Inspect
								</button>
							</div>
						{/each}
					</div>

					<div class="modal-footer">
						<span>Showing {data.occurrences.length} events</span>
						<button type="button" class="btn-status" onclick={toggleAllOccurrencesModal}>Done</button>
					</div>
				</div>
			</div>
		{/if}
	</section>
</div>

<style>
	.issue-detail {
		max-width: 1200px;
		margin: 0 auto;
	}

	.detail-nav {
		margin-bottom: 1.5rem;
	}

	.back-link {
		color: var(--color-primary);
		text-decoration: none;
		font-size: 0.875rem;
		font-weight: 500;
	}

	.back-link:hover {
		text-decoration: underline;
	}

	.issue-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 1rem;
	}

	.error-title {
		font-family: var(--font-mono);
		color: var(--severity-critical-text);
		font-size: 1.5rem;
		font-weight: 600;
	}

	.status {
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		text-transform: uppercase;
		font-weight: 600;
		letter-spacing: 0.05em;
	}

	.status.open { background: var(--status-open-bg); color: var(--status-open-text); }
	.status.resolved { background: var(--status-resolved-bg); color: var(--status-resolved-text); }
	.status.ignored { background: var(--status-ignored-bg); color: var(--status-ignored-text); }

	/* sentinel-releases-snooze §4.1 — the control block beside the chip, and its two dialogs (§5). */
	.status-block {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		flex-wrap: wrap;
		justify-content: flex-end;
	}

	.status-controls {
		display: flex;
		gap: 0.5rem;
	}

	.btn-status {
		padding: 0.375rem 0.75rem;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border-color);
		background: var(--bg-surface);
		color: var(--text-primary);
		font-size: 0.75rem;
		font-weight: 500;
		cursor: pointer;
		transition: background-color var(--transition-fast), color var(--transition-fast), border-color var(--transition-fast);
	}

	.btn-status:hover:not(:disabled) {
		background: var(--bg-surface-hover);
		border-color: var(--border-color-focus);
	}

	.btn-status:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.btn-resolve { border-color: var(--status-resolved-border); color: var(--status-resolved-text); }
	.btn-resolve:hover:not(:disabled) { background: var(--status-resolved-bg); }
	.btn-ignore { border-color: var(--status-ignored-border); color: var(--status-ignored-text); }
	.btn-ignore:hover:not(:disabled) { background: var(--status-ignored-bg); }
	.btn-reopen { border-color: var(--status-open-border); color: var(--status-open-text); }
	.btn-reopen:hover:not(:disabled) { background: var(--status-open-bg); }
	.btn-cancel { color: var(--text-muted); }

	.status-error {
		flex-basis: 100%;
		text-align: right;
		font-size: 0.8125rem;
		color: var(--severity-critical-text);
	}

	.status-dialog {
		position: fixed;
		inset: 0;
		margin: auto;
		width: min(28rem, calc(100vw - 2rem));
		max-height: calc(100vh - 2rem);
		overflow: auto;
		padding: 1.25rem;
		background: var(--bg-surface-elevated);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		box-shadow: 0 20px 40px oklch(0 0 0 / 0.5);
		z-index: 50;
	}

	.dialog-title {
		font-size: 1rem;
		font-weight: 600;
		margin-bottom: 1rem;
	}

	.dialog-fieldset {
		border: none;
		padding: 0;
		margin: 0 0 1rem 0;
		display: flex;
		flex-direction: column;
		gap: 0.875rem;
	}

	.choice {
		display: grid;
		grid-template-columns: auto 1fr;
		column-gap: 0.5rem;
		row-gap: 0.25rem;
		align-items: center;
		font-size: 0.8125rem;
	}

	.choice > input[type='radio'] {
		grid-column: 1;
		margin: 0;
	}

	.choice-label {
		grid-column: 2;
		font-weight: 500;
		color: var(--text-primary);
	}

	.choice-input {
		grid-column: 2;
		width: 100%;
		padding: 0.375rem 0.5rem;
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		font-family: var(--font-mono);
	}

	.choice-input:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.choice-hint {
		grid-column: 2;
		font-size: 0.75rem;
		color: var(--text-subtle);
		line-height: 1.4;
	}

	.dialog-actions {
		display: flex;
		gap: 0.5rem;
		justify-content: flex-end;
	}

	.dialog-form .status-error {
		text-align: left;
		margin-bottom: 0.75rem;
	}

	.visually-hidden {
		position: absolute;
		width: 1px;
		height: 1px;
		padding: 0;
		margin: -1px;
		overflow: hidden;
		clip: rect(0, 0, 0, 0);
		white-space: nowrap;
		border: 0;
	}

	.issue-info {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		padding: 1.25rem;
		border-radius: var(--radius-md);
		margin-bottom: 2rem;
	}

	.message {
		font-size: 1rem;
		line-height: 1.5;
		margin-bottom: 1rem;
		color: var(--text-primary);
	}

	.meta-info {
		display: flex;
		flex-wrap: wrap;
		gap: 1.5rem;
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.meta-label {
		color: var(--text-subtle);
		font-weight: 500;
	}

	.meta-item .zero {
		cursor: help;
	}

	.section-title {
		font-size: 1.125rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 1rem;
	}

	.trend {
		margin-bottom: 2rem;
	}

	.trend-charts {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 1.5rem;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1.25rem;
	}

	@media (max-width: 768px) {
		.trend-charts {
			grid-template-columns: 1fr;
		}
	}

	.occurrence-window {
		font-size: 0.8125rem;
		color: var(--text-subtle);
		margin-top: -0.5rem;
		margin-bottom: 1rem;
	}

	.date-group {
		margin-bottom: 1.5rem;
	}

	.date-header {
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		text-transform: uppercase;
		letter-spacing: 0.05em;
		border-bottom: 1px solid var(--border-color);
		padding-bottom: 0.375rem;
		margin-bottom: 0.75rem;
	}

	.occurrence-card {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1.125rem;
		margin-bottom: 0.75rem;
		box-shadow: 0 2px 8px oklch(0 0 0 / 0.15);
	}

	.occurrence-header {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-bottom: 0.75rem;
		font-size: 0.75rem;
	}

	.timestamp {
		font-family: var(--font-mono);
		color: var(--text-muted);
	}

	.badge {
		padding: 0.125rem 0.375rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		background: var(--bg-root);
		color: var(--text-muted);
		border: 1px solid var(--border-color);
	}

	.trace-info {
		display: flex;
		gap: 1rem;
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
		margin-bottom: 0.75rem;
	}

	.stacktrace {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		padding: 1rem;
		border-radius: var(--radius-sm);
		overflow-x: auto;
	}

		/* sentinel-sourcemaps §4.7 — the heading line: the h4, then the badge, then the toggle. */
	.stacktrace-heading {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
		margin-bottom: 0.5rem;
	}

	.badge.symbolication {
		background: var(--status-resolved-bg);
		color: var(--status-resolved-text);
		border-color: var(--status-resolved-border);
	}

	.raw-frames-toggle {
		margin-left: auto;
		padding: 0.125rem 0.5rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-surface);
		color: var(--text-muted);
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		cursor: pointer;
		transition: background-color var(--transition-fast), color var(--transition-fast), border-color var(--transition-fast);
	}

	.raw-frames-toggle:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
		border-color: var(--border-color-focus);
	}

	.metadata-toggle {
		padding: 0.25rem 0.625rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		color: var(--text-muted);
		font-size: 0.75rem;
		font-family: var(--font-mono);
		cursor: pointer;
		transition: background-color var(--transition-fast), color var(--transition-fast), border-color var(--transition-fast);
	}

	.metadata-toggle:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
		border-color: var(--border-color-focus);
	}

	.metadata-empty {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--text-subtle);
		margin: 0;
	}

	/* Metadata Ribbon Grid */
	.issue-info-ribbon {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1.125rem;
		margin-bottom: 2rem;
	}

	.ribbon-grid {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
		gap: 1rem;
		font-size: 0.8125rem;
	}

	.ribbon-card {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
	}

	.ribbon-label {
		color: var(--text-subtle);
		font-size: 0.7rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.ribbon-value {
		color: var(--text-primary);
		font-size: 0.8125rem;
	}

	.ribbon-value.mono {
		font-family: var(--font-mono);
		color: var(--text-muted);
	}

	.ribbon-value.count-highlight {
		font-family: var(--font-mono);
		font-size: 1rem;
		color: var(--text-primary);
		font-weight: 700;
	}

	.ribbon-value.release-highlight {
		font-family: var(--font-mono);
		color: var(--color-primary);
		font-weight: 600;
	}

	/* 2-Column Occurrence Workspace Layout */
	.occurrences-section-header {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
		margin-bottom: 1rem;
	}

	.occurrences-empty-state {
		background: var(--bg-surface);
		border: 1px dashed var(--border-color);
		border-radius: var(--radius-md);
		padding: 3rem 1.5rem;
		text-align: center;
	}

	.empty-title {
		font-size: 1rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.5rem;
	}

	.empty-desc {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.occurrence-workspace-layout {
		display: grid;
		grid-template-columns: 1fr 340px;
		gap: 1.5rem;
		align-items: start;
	}

	@media (max-width: 900px) {
		.occurrence-workspace-layout {
			grid-template-columns: 1fr;
		}
	}

	.occurrence-hero-banner {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1rem 1.25rem;
		margin-bottom: 1rem;
	}

	.hero-top-row {
		display: flex;
		justify-content: space-between;
		align-items: center;
		margin-bottom: 0.5rem;
	}

	.hero-id-badge {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}

	.hero-id {
		font-family: var(--font-mono);
		font-size: 0.95rem;
		color: var(--color-primary);
	}

	.hero-time {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--text-subtle);
	}

	.hero-meta-row {
		display: flex;
		flex-wrap: wrap;
		gap: 1.25rem;
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
	}

	.highlight-text {
		color: var(--text-primary);
	}

	.stacktrace-container {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		margin-bottom: 1rem;
		overflow: hidden;
	}

	.stacktrace-container .stacktrace-heading {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: 0.75rem 1rem;
		border-bottom: 1px solid var(--border-color);
		background: var(--bg-surface-hover);
	}

	.stacktrace-container .stacktrace-heading h3 {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0;
	}

	.stacktrace-tools {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}

	.stacktrace-code {
		padding: 1rem;
		margin: 0;
		font-family: var(--font-mono);
		font-size: 0.75rem;
		line-height: 1.5;
		color: var(--text-secondary);
		overflow-x: auto;
		background: var(--bg-surface);
	}

	.metadata-container {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		overflow: hidden;
	}

	.metadata-heading {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: 0.75rem 1rem;
		border-bottom: 1px solid var(--border-color);
		background: var(--bg-surface-hover);
	}

	.metadata-heading h3 {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--text-primary);
		margin: 0;
	}

	.metadata-code-block {
		padding: 1rem;
		font-family: var(--font-mono);
		font-size: 0.75rem;
		line-height: 1.45;
		color: var(--text-secondary);
		background: var(--bg-surface);
		overflow-x: auto;
	}

	.metadata-code-block pre {
		margin: 0;
	}

	/* Sidebar Timeline */
	.occurrence-sidebar-panel {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1rem;
		position: sticky;
		top: 1rem;
	}

	.sidebar-header {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
		margin-bottom: 0.75rem;
		padding-bottom: 0.5rem;
		border-bottom: 1px solid var(--border-color);
	}

	.sidebar-title {
		font-size: 0.8125rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-primary);
		font-weight: 700;
		margin: 0;
	}

	.sidebar-count {
		font-size: 0.7rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
	}

	.sidebar-list {
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.sidebar-item {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		padding: 0.625rem 0.75rem;
		border: 1px solid var(--border-color);
		background: var(--bg-root);
		border-radius: var(--radius-sm);
		text-align: left;
		cursor: pointer;
		transition: all var(--transition-fast);
		width: 100%;
	}

	.sidebar-item:hover {
		background: var(--bg-surface-hover);
		border-color: var(--border-color-focus);
	}

	.sidebar-item.active {
		border-color: var(--border-color-focus);
		background: var(--color-primary-subtle);
	}

	.sidebar-item-top {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
		font-size: 0.75rem;
		font-family: var(--font-mono);
	}

	.sidebar-item-id {
		color: var(--color-primary);
	}

	.latest-tag {
		color: var(--status-open-text);
		font-size: 0.6875rem;
		margin-left: 0.25rem;
	}

	.sidebar-item-time {
		font-size: 0.6875rem;
		color: var(--text-subtle);
	}

	.sidebar-item-meta {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
	}

	.badge.env-micro {
		padding: 0.05rem 0.35rem;
		font-size: 0.625rem;
		border-radius: var(--radius-sm);
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
	}

	.trace-micro {
		color: var(--text-muted);
	}

	.sidebar-actions {
		margin-top: 1rem;
		text-align: center;
	}

	.btn-view-all {
		width: 100%;
		font-size: 0.75rem;
		background: var(--color-primary);
		color: var(--text-primary);
		font-weight: 600;
	}

	/* Modal Dialog */
	.modal-backdrop {
		position: fixed;
		inset: 0;
		z-index: 1000;
		background: rgba(0, 0, 0, 0.75);
		backdrop-filter: blur(4px);
		display: flex;
		align-items: center;
		justify-content: center;
	}

	.modal-dialog {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-lg);
		width: 90%;
		max-width: 850px;
		max-height: 85vh;
		display: flex;
		flex-direction: column;
		overflow: hidden;
		box-shadow: var(--shadow-xl);
	}

	.modal-header {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: 1.25rem 1.5rem;
		border-bottom: 1px solid var(--border-color);
	}

	.modal-title {
		font-size: 1.125rem;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0 0 0.25rem 0;
	}

	.modal-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.modal-body {
		padding: 1.25rem 1.5rem;
		overflow-y: auto;
		display: flex;
		flex-direction: column;
		gap: 0.625rem;
	}

	.modal-occ-row {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: 0.75rem 1rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		transition: background var(--transition-fast);
	}

	.modal-occ-row.active {
		border-color: var(--border-color-focus);
		background: var(--color-primary-subtle);
	}

	.modal-occ-info {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
	}

	.modal-occ-tags {
		display: flex;
		align-items: center;
		gap: 0.625rem;
	}

	.modal-occ-id {
		font-family: var(--font-mono);
		font-size: 0.8125rem;
		color: var(--color-primary);
	}

	.modal-occ-details {
		font-size: 0.75rem;
		font-family: var(--font-mono);
		color: var(--text-subtle);
	}

	.modal-occ-details code {
		color: var(--text-secondary);
	}

	.btn-inspect {
		padding: 0.25rem 0.625rem;
		font-size: 0.75rem;
	}

	.modal-footer {
		padding: 1rem 1.5rem;
		border-top: 1px solid var(--border-color);
		display: flex;
		justify-content: space-between;
		align-items: center;
		background: var(--bg-root);
		font-size: 0.8125rem;
		color: var(--text-subtle);
	}
</style>