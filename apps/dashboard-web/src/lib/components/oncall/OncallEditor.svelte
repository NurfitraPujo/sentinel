<script lang="ts">
	// sentinel-user-report-issue §4.4a, fe.md rows O1–O10 — the on-call schedule editor.
	//
	// **This page is the only way an organization ever gets escalation working.** Until a schedule
	// exists, §4.4 resolves nobody for every organization and every report falls through to the
	// existing pull loop. That is why O1 is an explanation and a Create button rather than a blank
	// form: a blank form does not say what the thing is for.
	//
	// **The permission split is the sharp edge.** Read is any recognised org member; write is
	// `owner`/`admin` ONLY — narrower than `ISSUE_WRITE_ROLES`, which every other write control on
	// this dashboard uses. `canWrite` is computed in the loader, not here, and every control that
	// would `403` is absent from the DOM rather than disabled: a disabled Save still says "you may
	// save, later", which is not true for an `engineer` and never will be.
	//
	// No dependency: an IANA `<datalist>` fed by `Intl.supportedValuesOf` and a native
	// `<input type="time">`, per contract §5 and fe.md's Library dependencies section.
	import { untrack } from 'svelte';
	import type {
		CurrentlyOnCall,
		OncallCandidate,
		OncallMember,
		OncallSchedule,
		RotationKind,
	} from '$lib/types/oncall';
	import OncallRotationList from './OncallRotationList.svelte';
	import OncallOverrides from './OncallOverrides.svelte';
	import {
		ISO_WEEKDAY_NAMES,
		handoffRestatement,
		listTimeZones,
		localTimeIn,
	} from './oncall-format';

	interface Props {
		orgId: string;
		schedule: OncallSchedule | null;
		currentlyOnCall: CurrentlyOnCall | null;
		candidates: OncallCandidate[];
		/** `owner` or `admin`. Decided in the loader (`+page.server.ts`), never re-derived here. */
		canWrite: boolean;
		/** The viewer's org role, so the read-only notice can name it. */
		role: string | null;
	}

	let { orgId, schedule, currentlyOnCall, candidates, canWrite, role }: Props = $props();

	const TIME_ZONES = listTimeZones();

	// The draft, seeded ONCE from the loaded schedule (or from a sensible default when creating
	// one) and thereafter owned by this component. `untrack` is that "once" said out loud: without
	// it Svelte warns that the initialiser captures only the prop's first value, which is exactly
	// what is wanted here — a later `schedule` arriving mid-edit must not overwrite what the user
	// has typed. Every write path reloads the page, so there is no stale-draft window to worry about.
	const seed = untrack(() => schedule);

	let creating = $state(false);
	let name = $state(seed?.name ?? 'Primary');
	let timezone = $state(
		seed?.timezone ?? Intl.DateTimeFormat().resolvedOptions().timeZone ?? 'UTC'
	);
	let kind = $state<RotationKind>(seed?.kind ?? 'weekly');
	let handoffTime = $state(seed?.handoffTime ?? '09:00');
	let handoffWeekday = $state<number>(seed?.handoffWeekday ?? 1);
	let members = $state<OncallMember[]>(seed ? [...seed.members] : []);
	let addMemberId = $state('');

	let saving = $state(false);
	let saveError = $state<string | null>(null);
	let overrideError = $state<string | null>(null);
	let confirmingDeactivate = $state(false);
	let deactivating = $state(false);

	// O3: the current wall-clock time in the selected zone, recomputed on a slow tick so a stale
	// clock never argues for a wrong zone. It fetches nothing.
	let now = $state(new Date());
	$effect(() => {
		const timer = setInterval(() => (now = new Date()), 30_000);
		return () => clearInterval(timer);
	});

	let zoneLocalTime = $derived(localTimeIn(timezone, now));

	let restatement = $derived(handoffRestatement({ kind, handoffTime, handoffWeekday, timezone }));

	let addableCandidates = $derived(
		candidates.filter((candidate) => !members.some((m) => m.userId === candidate.userId))
	);

	function addMember() {
		const candidate = candidates.find((c) => c.userId === addMemberId);
		if (!candidate) return;
		members = [
			...members,
			{
				userId: candidate.userId,
				name: candidate.name,
				position: members.length,
				eligible: true,
			},
		];
		addMemberId = '';
	}

	async function save() {
		saveError = null;
		saving = true;
		try {
			// §4.4a's `PUT` is create-or-replace, and `memberIds` is the ordered list — replace whole,
			// never a merge. An ineligible member stays in the list on purpose (O4): dropping them
			// here would be the silent removal the contract forbids, done by the client instead.
			const res = await fetch(`/api/organizations/${orgId}/oncall`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					name,
					timezone,
					kind,
					handoffTime,
					handoffWeekday: kind === 'weekly' ? handoffWeekday : null,
					memberIds: members.map((m) => m.userId),
				}),
			});
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to save (${res.status})`);
			}
			window.location.reload();
		} catch (err) {
			saveError = err instanceof Error ? err.message : 'Failed to save';
			saving = false;
		}
	}

	async function addOverride(input: { userId: string; startsAt: string; endsAt: string }) {
		overrideError = null;
		const res = await fetch(`/api/organizations/${orgId}/oncall/overrides`, {
			method: 'POST',
			headers: { 'Content-Type': 'application/json' },
			body: JSON.stringify(input),
		});
		if (!res.ok) {
			const body = await res.json().catch(() => ({}));
			overrideError = body.message || `Failed to add the override (${res.status})`;
			return;
		}
		window.location.reload();
	}

	async function removeOverride(overrideId: string) {
		overrideError = null;
		const res = await fetch(`/api/organizations/${orgId}/oncall/overrides/${overrideId}`, {
			method: 'DELETE',
		});
		if (!res.ok) {
			const body = await res.json().catch(() => ({}));
			overrideError = body.message || `Failed to remove the override (${res.status})`;
			return;
		}
		window.location.reload();
	}

	async function deactivate() {
		deactivating = true;
		try {
			const res = await fetch(`/api/organizations/${orgId}/oncall`, { method: 'DELETE' });
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to deactivate (${res.status})`);
			}
			window.location.reload();
		} catch (err) {
			saveError = err instanceof Error ? err.message : 'Failed to deactivate';
			deactivating = false;
			confirmingDeactivate = false;
		}
	}
</script>

<div class="oncall-page">
	<div class="page-header">
		<div>
			<h1 class="page-title">On-call schedule</h1>
			<p class="page-subtitle">
				Who a user report is escalated to when it arrives, and who it moves to when nobody
				acknowledges it.
			</p>
		</div>
	</div>

	{#if !canWrite}
		<!-- O2. Not a disabled Save: no write control is rendered at all, anywhere below. -->
		<p class="readonly-notice" data-testid="oncall-readonly-notice">
			You can see this schedule but not change it. Editing the rota is limited to organization
			owners and admins{role ? `, and your role here is ${role}` : ''}.
		</p>
	{/if}

	{#if schedule === null && !creating}
		<!-- O1: an empty state that explains what a schedule DOES before offering Create. -->
		<section class="empty-state" data-testid="oncall-empty-state">
			<h2 class="empty-title">No on-call schedule yet</h2>
			<p class="empty-body">
				A schedule names the engineers who carry the pager and the order they carry it in. With
				one, a user report that arrives from a service is assigned the moment it lands, the person
				on duty is notified, and the report moves to the next engineer if nobody acknowledges it
				within the deadline.
			</p>
			<p class="empty-body">
				<strong>Without one, none of that runs.</strong> Reports arrive unassigned and wait for
				somebody to notice them.
			</p>
			{#if canWrite}
				<button
					type="button"
					class="btn-primary"
					data-testid="oncall-create"
					onclick={() => (creating = true)}
				>
					Create a schedule
				</button>
			{/if}
		</section>
	{:else}
		<section class="panel">
			<h2 class="section-heading">Who is on call now</h2>
			{#if currentlyOnCall}
				<p class="on-call-now" data-testid="oncall-current">
					<strong>{currentlyOnCall.name ?? currentlyOnCall.userId}</strong>
					<span class="via-tag" data-testid="oncall-current-via">via {currentlyOnCall.via}</span>
				</p>
			{:else}
				<p class="on-call-nobody" data-testid="oncall-current-nobody">
					Nobody. Nothing will be escalated until the rotation resolves someone.
				</p>
			{/if}
		</section>

		<section class="panel">
			<h2 class="section-heading">Rotation</h2>

			{#if saveError}
				<p class="error-banner" role="alert" data-testid="oncall-save-error">{saveError}</p>
			{/if}

			<div class="field-grid">
				<label class="field">
					<span class="field-label">Name</span>
					{#if canWrite}
						<input type="text" bind:value={name} data-testid="oncall-name" />
					{:else}
						<span class="field-static">{name}</span>
					{/if}
				</label>

				<label class="field">
					<span class="field-label">Timezone</span>
					{#if canWrite}
						<input
							type="text"
							list="oncall-timezones"
							bind:value={timezone}
							data-testid="oncall-timezone"
						/>
						<datalist id="oncall-timezones">
							{#each TIME_ZONES as zone}
								<option value={zone}></option>
							{/each}
						</datalist>
					{:else}
						<span class="field-static">{timezone}</span>
					{/if}
					<!-- O3: the current local time in the chosen zone, so a wrong pick is visible before
					     it is saved. A fixed offset formats as nothing here, which is the honest answer:
					     §3 takes IANA names only. -->
					<span class="zone-clock" data-testid="oncall-timezone-clock">
						{#if zoneLocalTime}
							now {zoneLocalTime} there
						{:else}
							not an IANA zone name — the API will refuse this
						{/if}
					</span>
				</label>

				<label class="field">
					<span class="field-label">Hands over</span>
					{#if canWrite}
						<select bind:value={kind} data-testid="oncall-kind">
							<option value="daily">Every day</option>
							<option value="weekly">Every week</option>
						</select>
					{:else}
						<span class="field-static">{kind}</span>
					{/if}
				</label>

				{#if kind === 'weekly'}
					<label class="field">
						<span class="field-label">On</span>
						{#if canWrite}
							<select bind:value={handoffWeekday} data-testid="oncall-weekday">
								{#each Object.entries(ISO_WEEKDAY_NAMES) as [value, label]}
									<option value={Number(value)}>{label}</option>
								{/each}
							</select>
						{:else}
							<span class="field-static">{ISO_WEEKDAY_NAMES[handoffWeekday] ?? '—'}</span>
						{/if}
					</label>
				{/if}

				<label class="field">
					<span class="field-label">At</span>
					{#if canWrite}
						<input type="time" bind:value={handoffTime} data-testid="oncall-handoff-time" />
					{:else}
						<span class="field-static">{handoffTime}</span>
					{/if}
				</label>
			</div>

			<!-- O5: kind + time + weekday is three fields nobody reads back correctly. -->
			<p class="restatement" data-testid="oncall-restatement">{restatement}</p>

			<OncallRotationList
				members={members}
				canWrite={canWrite}
				onreorder={(next) => (members = next)}
				onremove={(userId) =>
					(members = members
						.filter((m) => m.userId !== userId)
						.map((m, position) => ({ ...m, position })))}
			/>

			{#if canWrite}
				<div class="add-member" data-testid="oncall-add-member">
					<select bind:value={addMemberId} aria-label="Add a member to the rotation">
						<option value="">Add a member…</option>
						{#each addableCandidates as candidate (candidate.userId)}
							<option value={candidate.userId}>{candidate.name} ({candidate.role})</option>
						{/each}
					</select>
					<button type="button" class="btn-secondary" disabled={!addMemberId} onclick={addMember}>
						Add
					</button>
				</div>

				<div class="save-row">
					<button
						type="button"
						class="btn-primary"
						data-testid="oncall-save"
						disabled={saving}
						onclick={save}
					>
						{saving ? 'Saving…' : schedule ? 'Save schedule' : 'Create schedule'}
					</button>
				</div>
			{/if}
		</section>

		{#if schedule}
			<section class="panel">
				<OncallOverrides
					overrides={schedule.overrides}
					candidates={candidates}
					canWrite={canWrite}
					onadd={addOverride}
					onremove={removeOverride}
					addError={overrideError}
				/>
			</section>

			{#if canWrite}
				<section class="panel danger-panel">
					<h2 class="section-heading">Deactivate</h2>
					{#if confirmingDeactivate}
						<!-- O10: the confirmation says what STOPS, not "are you sure?". -->
						<p class="danger-body" data-testid="oncall-deactivate-confirm">
							Deactivating this schedule stops escalation for this organization. New user reports
							will no longer be assigned to anyone, nobody will be notified that a report arrived,
							and the acknowledgement deadline will stop moving reports along. Reports already
							assigned keep their assignee.
						</p>
						<div class="danger-actions">
							<button
								type="button"
								class="btn-danger"
								disabled={deactivating}
								onclick={deactivate}
							>
								{deactivating ? 'Deactivating…' : 'Deactivate the schedule'}
							</button>
							<button
								type="button"
								class="btn-secondary"
								onclick={() => (confirmingDeactivate = false)}
							>
								Keep it
							</button>
						</div>
					{:else}
						<button
							type="button"
							class="btn-danger"
							data-testid="oncall-deactivate"
							onclick={() => (confirmingDeactivate = true)}
						>
							Deactivate
						</button>
					{/if}
				</section>
			{/if}
		{/if}
	{/if}
</div>

<style>
	.oncall-page {
		max-width: 56rem;
		margin: 0 auto;
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.page-title {
		font-size: 1.25rem;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0 0 0.25rem 0;
	}

	.page-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.readonly-notice {
		font-size: 0.75rem;
		color: var(--text-muted);
		background: var(--bg-surface);
		border: 1px dashed var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.75rem;
		margin: 0;
	}

	.panel,
	.empty-state {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 0.875rem 1rem;
	}

	.danger-panel {
		border-color: var(--severity-critical-text, #ef4444);
	}

	.section-heading {
		font-size: 0.75rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-subtle);
		font-weight: 600;
		margin: 0 0 0.625rem 0;
	}

	.empty-title {
		font-size: 1rem;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0 0 0.5rem 0;
	}

	.empty-body {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0 0 0.625rem 0;
	}

	.on-call-now {
		font-size: 0.875rem;
		color: var(--text-primary);
		margin: 0;
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}

	.via-tag {
		font-size: 0.625rem;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		border: 1px solid var(--border-color);
		border-radius: 3px;
		padding: 0 0.25rem;
		color: var(--text-muted);
	}

	.on-call-nobody {
		font-size: 0.8125rem;
		color: var(--text-subtle);
		margin: 0;
	}

	.field-grid {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
		margin-bottom: 0.625rem;
	}

	.field {
		display: flex;
		flex-direction: column;
		gap: 0.1875rem;
	}

	.field-label {
		font-size: 0.6875rem;
		text-transform: uppercase;
		letter-spacing: 0.02em;
		color: var(--text-subtle);
	}

	.field input,
	.field select,
	.add-member select {
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8125rem;
		padding: 0.3125rem 0.5rem;
	}

	.field-static {
		font-size: 0.8125rem;
		color: var(--text-primary);
		padding: 0.3125rem 0;
	}

	.zone-clock {
		font-size: 0.6875rem;
		color: var(--text-subtle);
		font-family: var(--font-mono);
	}

	.restatement {
		font-size: 0.8125rem;
		color: var(--text-primary);
		background: var(--bg-root);
		border-left: 3px solid var(--color-primary);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		margin: 0 0 0.75rem 0;
	}

	.add-member {
		display: flex;
		gap: 0.375rem;
		margin-top: 0.625rem;
	}

	.save-row {
		margin-top: 0.75rem;
	}

	.danger-body {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0 0 0.625rem 0;
	}

	.danger-actions {
		display: flex;
		gap: 0.5rem;
	}

	.btn-primary {
		background: var(--color-primary);
		color: #fff;
		border: none;
		border-radius: var(--radius-sm);
		padding: 0.4375rem 0.875rem;
		font-size: 0.8125rem;
		font-weight: 600;
		cursor: pointer;
	}

	.btn-primary:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.btn-secondary {
		background: transparent;
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.75rem;
		font-size: 0.8125rem;
		cursor: pointer;
	}

	.btn-danger {
		background: transparent;
		color: var(--severity-critical-text);
		border: 1px solid var(--severity-critical-text);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.75rem;
		font-size: 0.8125rem;
		font-weight: 600;
		cursor: pointer;
	}

	.error-banner {
		background: var(--severity-critical-bg);
		color: var(--severity-critical-text);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		font-size: 0.75rem;
		margin: 0 0 0.625rem 0;
	}
</style>
