<script lang="ts">
	import type { PageData } from './$types';

	let { data } = $props();

	let expandedMetadata = $state(new Set<string>());

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

	function formatStackFrame(frame: { file: string; line: number; function: string }): string {
		let file = frame.file;
		if (file.length > 50) {
			const start = file.slice(0, 20);
			const end = file.slice(-25);
			file = start + '...' + end;
		}
		return `at ${frame.function} (${file}:${frame.line})`;
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

	interface StackFrame {
		file: string;
		line: number;
		function: string;
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
		submitting = true;
		submitError = null;
		let response: Response;
		try {
			response = await fetch(`/api/issues/${data.issue.id}/status`, {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(body),
			});
		} catch {
			submitError = 'Network error';
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

	function showDialog(which: 'resolve' | 'ignore') {
		submitError = null;
		resolveChoice = 'next';
		resolveRelease = pickerReleases[0]?.version ?? '';
		resolveTyped = '';
		ignoreUntil = '';
		ignoreOccurrences = null;
		openDialog = which;
	}

	function closeDialog() {
		openDialog = null;
		submitError = null;
	}

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

	<div class="issue-info">
		<p class="message">{data.issue.message}</p>
		<div class="meta-info">
			<span class="meta-item"><strong class="meta-label">Project:</strong> {data.project?.name ?? 'Unknown'}</span>
			<!--
				sentinel-project-environments task 2, flows `D1`/`D3`. `environmentDisplayName` is the
				registry's label falling back to the raw `issues.environment` name, and the FALLBACK IS
				THE NORMAL PATH — under decisions 8 and 9 an environment can be present in the data and
				absent from the registry indefinitely, so there is deliberately no warning, no
				placeholder and no "unregistered" marker here. A page that flagged the miss would be
				shouting on every environment's first day.
			-->
			<span class="meta-item"><strong class="meta-label">Environment:</strong> {data.environmentDisplayName ?? data.issue.environment}</span>
			<span class="meta-item"><strong class="meta-label">First seen:</strong> {formatDate(data.issue.firstSeen)}</span>
			<span class="meta-item"><strong class="meta-label">Last seen:</strong> {formatDate(data.issue.lastSeen)}</span>
			<span class="meta-item"><strong class="meta-label">Occurrences:</strong> {Number(data.issue.count).toLocaleString()}</span>
			<!--
				sentinel-affected-users §4.2, flow `D1` (decisions 12, 15, 16, 21). Two more items after
				Occurrences, each label carrying the window — `retentionDays` is page data from the
				loader, never a literal 30, because a template cannot read `$env/dynamic/private`. A `0`
				takes the list card's zero shape: the dash replaces only the number, on its own span, and
				its `title` says why — no usable id inside the window is not "nobody was affected".
			-->
			<span class="meta-item" data-testid="issue-affected-users"><strong class="meta-label">Affected users (last {data.retentionDays} days):</strong> {@render affectedFigure(data.issue.affectedUserCount, 'user')}</span>
			<span class="meta-item" data-testid="issue-affected-tenants"><strong class="meta-label">Affected tenants (last {data.retentionDays} days):</strong> {@render affectedFigure(data.issue.affectedTenantCount, 'tenant')}</span>
			<!--
				sentinel-releases-snooze §4.1, flow `D1`. Three facts after the two affected counts, in
				this order. `Introduced in:` is always there (`—` when the issue's first occurrence
				carried no version, or retention swept it before the backfill ran — decision 22).
				`Resolved in:` only while `resolved`: the version, *next release (waiting for it)* for the
				reserved literal (decision 21), *no version* for `null`. `Ignored:` only while `ignored`:
				*until …*, *for N more occurrences (n so far)*, both joined with *or*, or *indefinitely*.
			-->
			<span class="meta-item" data-testid="issue-introduced-in"><strong class="meta-label">Introduced in:</strong> {data.issue.firstReleaseVersion ?? '—'}</span>
			{#if data.issue.status === 'resolved'}
				<span class="meta-item" data-testid="issue-resolved-in"><strong class="meta-label">Resolved in:</strong> {resolvedInFact}</span>
			{/if}
			{#if data.issue.status === 'ignored'}
				<span class="meta-item" data-testid="issue-ignored"><strong class="meta-label">Ignored:</strong> {ignoredFact}</span>
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
		<!--
			sentinel-project-environments task 1, flow `D2`. This read `({data.occurrences.length})`,
			which is the PAGE SIZE — `getOccurrencesByIssueId` caps at 100 — presented as a total. The
			heading now carries the loader's real `COUNT(*)` and the line below says how many of them
			are actually listed, so the two numbers can differ on screen without either one lying.
		-->
		<h2 class="section-title">Occurrence History ({Number(data.occurrenceTotal ?? data.occurrences.length).toLocaleString()})</h2>
		{#if (data.occurrenceTotal ?? data.occurrences.length) > data.occurrences.length}
			<p class="occurrence-window">
				Showing the {data.occurrences.length.toLocaleString()} most recent.
			</p>
		{/if}

		{#each occurrenceGroups as group}
			<div class="date-group">
				<h3 class="date-header">{group.date}</h3>
				{#each group.occurrences as occurrence}
					<div class="occurrence-card">
						<div class="occurrence-header">
							<span class="timestamp">{formatDate(occurrence.createdAt)}</span>
							<span class="badge env">{occurrence.environment}</span>
							<span class="badge platform">{occurrence.platform}</span>
						</div>

						{#if occurrence.traceId}
							<div class="trace-info">
								<span class="trace-id">Trace: {occurrence.traceId}</span>
								{#if occurrence.spanId}
									<span class="span-id">Span: {occurrence.spanId}</span>
								{/if}
							</div>
						{/if}

						{#if occurrence.stacktrace && occurrence.stacktrace.length > 0}
							<div class="stacktrace">
								<h4>Stack Trace</h4>
								<pre><code>{#each occurrence.stacktrace as frame}{formatStackFrame(frame)}
{/each}</code></pre>
							</div>
						{/if}

						<button class="metadata-toggle" onclick={() => toggleMetadata(occurrence.id)}>
							{expandedMetadata.has(occurrence.id) ? 'Hide' : 'Show'} Metadata
						</button>

						{#if expandedMetadata.has(occurrence.id) && occurrence.metadata}
							<div class="metadata">
								<h4>Metadata</h4>
								<pre><code>{JSON.stringify(occurrence.metadata, null, 2)}</code></pre>
							</div>
						{/if}
					</div>
				{/each}
			</div>
		{/each}
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
		transition: background 0.15s ease, color 0.15s ease;
	}

	.btn-status:hover:not(:disabled) {
		background: var(--bg-surface-hover);
	}

	.btn-status:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.btn-resolve { border-color: var(--status-resolved-text); color: var(--status-resolved-text); }
	.btn-resolve:hover:not(:disabled) { background: var(--status-resolved-bg); }
	.btn-ignore { border-color: var(--status-ignored-text); color: var(--status-ignored-text); }
	.btn-ignore:hover:not(:disabled) { background: var(--status-ignored-bg); }
	.btn-reopen { border-color: var(--status-open-text); color: var(--status-open-text); }
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
		background: var(--bg-surface);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		box-shadow: 0 20px 40px rgba(0, 0, 0, 0.45);
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
		padding: 1rem;
		margin-bottom: 0.75rem;
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
		padding: 0.875rem;
		border-radius: var(--radius-sm);
		overflow-x: auto;
	}

	.stacktrace h4 {
		color: var(--text-subtle);
		margin-bottom: 0.5rem;
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.stacktrace pre {
		margin: 0;
		font-family: var(--font-mono);
		font-size: 0.75rem;
		line-height: 1.4;
	}

	.metadata-toggle {
		margin-top: 0.75rem;
		padding: 0.25rem 0.625rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-root);
		color: var(--text-muted);
		font-size: 0.75rem;
		font-family: var(--font-mono);
		cursor: pointer;
		transition: background 0.15s ease, color 0.15s ease;
	}

	.metadata-toggle:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.metadata {
		margin-top: 0.75rem;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		padding: 0.875rem;
		border-radius: var(--radius-sm);
	}

	.metadata h4 {
		font-size: 0.6875rem;
		font-family: var(--font-mono);
		text-transform: uppercase;
		color: var(--text-subtle);
		margin-bottom: 0.5rem;
	}

	.metadata pre {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--text-muted);
		overflow-x: auto;
	}
</style>