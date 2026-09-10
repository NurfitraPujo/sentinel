<script lang="ts">
	import { onMount } from 'svelte';
	import { invalidateAll } from '$app/navigation';
	import type { PageData } from './$types';

	let { data } = $props();

	interface AlertConfig {
		id: string;
		scope: 'organization' | 'project';
		organizationId: string;
		projectId: string | null;
		channel: string;
		channelTarget: string;
		frequencyThreshold: number;
		windowSeconds: number;
		enabled: boolean;
		createdAt: Date | null;
	}

	let scope = $state<'project' | 'organization'>('project');
	let selectedProjectId = $state('');
	let channel = $state('email');
	let channelTarget = $state('');
	let frequencyThreshold = $state(50);
	let windowSeconds = $state(60);
	let enabled = $state(true);
	let isSubmitting = $state(false);
	let editMode = $state(false);
	let editingConfig = $state<AlertConfig | null>(null);

	// Inline validation errors (HTTP 400 / 422) mapped to field names
	let fieldErrors = $state<Record<string, string>>({});
	// System / Permission errors (HTTP 403 / 500) shown via Toast / Banner
	let toastMessage = $state<{ text: string; type: 'error' | 'success' } | null>(null);

	// Inline delete confirmation tracking
	let deletingConfigId = $state<string | null>(null);
	let isDeleting = $state(false);

	let selectedOrgId = $state('');

	// ---------------------------------------------------------------------------------------------
	// sentinel-project-environments tasks 12-13 — the environment picker and the no-default notice.
	// Contract §4.7 and decisions 32 and 33, flows `E1`-`E13`.
	// ---------------------------------------------------------------------------------------------

	// The names this picker can OFFER: §4.1's registry (scope-filtered, hidden rows excluded) for a
	// project-scoped rule, and §4.3a's guarded cross-project union for an org-wide one. There is no
	// free-text path anywhere — flow `E8`: a typed environment name in an alert config silently means
	// "never alert", and nothing in the system would report that.
	let offeredEnvironments = $state<string[]>([]);
	let offeredLoaded = $state(false);

	// `null` = all environments; an array = exactly those names.
	let environmentMode = $state<'all' | 'specific'>('all');
	let environmentSelection = $state<string[]>([]);
	// Decision 32's second rule: an OMITTED key means "leave unchanged", and that is the shape that
	// cannot go wrong for a save that is not about environments. So the field is sent only when the
	// picker was actually touched — which is also what makes flows `E3`/`E4` work on creation, where
	// an untouched picker is what asks the server to apply the project's default.
	let environmentTouched = $state(false);
	// BUG-16. The selection is only meaningful against ONE scope target: project A's `production` is
	// not project B's, and the server refuses (or worse, stores) a name from the wrong registry. The
	// `$effect` below already reloads the OFFERED names on every target change, so without this the
	// two drift apart — the picker showing B's registry while `environmentSelection` still holds A's
	// choice, which then renders through the decision-48 carry-forward block and is submitted as
	// `payload.environments`. This records which target the current selection was made against;
	// `null` means "no selection to protect", which is how `resetForm()` hands over to the effect.
	let selectionScopeKey = $state<string | null>(null);

	function scopeTargetKey(): string {
		return scope === 'project' ? `project:${selectedProjectId}` : `organization:${activeOrgId}`;
	}

	// Decision 32's third rule: the server MERGES what is sent with the names the caller could not
	// have been offered — unregistered, hidden, or outside their grant — and returns the merged
	// array. Re-rendering from the RESPONSE rather than from what was sent is what stops the picker
	// showing fewer environments than the config holds and turning the next save into a deselection
	// nobody made. This map is that response, per config id, and the page loader does not carry the
	// column (it is BE's file under §6a), so it is also seeded from `GET /api/alerts`.
	let configEnvironments = $state<Record<string, string[] | null>>({});
	// BUG-05. `configEnvironments` alone has only two values, and neither of them is "not known
	// yet": an empty map is indistinguishable from a map of rules that all read `null`, and `null`
	// is the FACT "All environments". So a failed or in-flight GET rendered — and seeded the edit
	// picker with — the widest possible scope for every rule, which is exactly the guess the comment
	// below used to say it must never make. These two flags are the third state.
	let configEnvironmentsLoaded = $state(false);
	let configEnvironmentsLoading = $state(false);
	let configEnvironmentsError = $state<string | null>(null);

	// Task 13 / flow `E4`. `defaultEnvironmentApplied: false` is a field on the POST response and is
	// NOT persisted anywhere — no column carries it, deliberately — so this is an event reported
	// once, at creation, on the screen the rule was created from. It is not a toast that disappears
	// on a timer and there is nothing to re-render after a reload. The standing form of the same
	// warning lives on project settings' "no default set" line (decision 33).
	let noDefaultNotice = $state<string | null>(null);

	async function loadConfigEnvironments() {
		// Every failure path here leaves `configEnvironmentsLoaded` false, which is what makes the
		// table cell and the edit picker render "unknown" rather than the stated fact. Nothing is
		// swallowed: the message is surfaced with a retry, because the alternative is a page that
		// looks correct and has silently unscoped every rule on it.
		configEnvironmentsLoading = true;
		try {
			const res = await fetch('/api/alerts');
			if (!res.ok) {
				configEnvironmentsError = `Could not read the environment scope of these rules (HTTP ${res.status}).`;
				return;
			}
			const rows = (await res.json()) as { id: string; environments: string[] | null }[];
			const next: Record<string, string[] | null> = {};
			for (const row of rows) next[row.id] = row.environments ?? null;
			configEnvironments = next;
			configEnvironmentsLoaded = true;
			configEnvironmentsError = null;
		} catch {
			configEnvironmentsError = 'Could not read the environment scope of these rules.';
		} finally {
			configEnvironmentsLoading = false;
		}
	}

	async function loadOfferedEnvironments() {
		offeredLoaded = false;
		offeredEnvironments = [];
		try {
			let url: string | null = null;
			if (scope === 'project' && selectedProjectId) {
				const project = data.projects.find((p) => p.id === selectedProjectId);
				if (project?.organizationId) {
					url = `/api/organizations/${project.organizationId}/projects/${selectedProjectId}/environments`;
				}
			} else if (scope === 'organization' && activeOrgId) {
				// §4.3a. There is no single project to take a registry from, so the names are the
				// guarded union across the projects this caller can reach in that organization.
				url = `/api/environments?organizationId=${encodeURIComponent(activeOrgId)}`;
			}
			if (!url) return;
			const res = await fetch(url);
			if (!res.ok) return;
			const body = await res.json();
			offeredEnvironments = ((body.environments ?? []) as { name: string }[]).map((e) => e.name);
		} catch {
			offeredEnvironments = [];
		} finally {
			offeredLoaded = true;
		}
	}

	$effect(() => {
		// Re-read whenever the scope target changes: a project's registry and an organization's union
		// are different lists, and offering one where the other applies is how a rule gets scoped to a
		// name the server will refuse.
		void scope;
		void selectedProjectId;
		void activeOrgId;
		const key = scopeTargetKey();
		// BUG-16. The offered names and the selection move together or not at all. A selection made
		// against a DIFFERENT target is discarded here rather than carried across, and the picker goes
		// back to untouched — which under decision 32's first rule omits the `environments` key
		// entirely, so the server applies the NEW target's default (flows `E3`/`E4`) instead of the
		// previous target's names. `startEdit()` seeds `selectionScopeKey` itself, so re-opening a rule
		// for edit is not mistaken for a target change and does not lose the stored scope.
		if (selectionScopeKey !== null && selectionScopeKey !== key) {
			environmentMode = 'all';
			environmentSelection = [];
			environmentTouched = false;
			delete fieldErrors.environments;
			fieldErrors = { ...fieldErrors };
		}
		selectionScopeKey = key;
		loadOfferedEnvironments();
	});

	onMount(loadConfigEnvironments);

	function environmentsOf(configId: string): string[] | null {
		return configEnvironments[configId] ?? null;
	}

	// BUG-05. While the stored scope is unknown the picker is not shown at all, let alone seeded: a
	// picker seeded from an unknown value cannot produce a safe save, and a radio pre-checked on
	// "All environments" is itself the claim that the rule is unscoped. Creation is unaffected — a
	// new rule has no stored scope to be unknown about.
	let storedScopeUnknown = $derived(editMode && editingConfig !== null && !configEnvironmentsLoaded);

	function toggleEnvironment(name: string) {
		environmentTouched = true;
		environmentSelection = environmentSelection.includes(name)
			? environmentSelection.filter((n) => n !== name)
			: [...environmentSelection, name];
		delete fieldErrors.environments;
		fieldErrors = { ...fieldErrors };
	}

	function setEnvironmentMode(mode: 'all' | 'specific') {
		environmentTouched = true;
		environmentMode = mode;
		if (mode === 'all') environmentSelection = [];
		delete fieldErrors.environments;
		fieldErrors = { ...fieldErrors };
	}

	let editableAlertConfigIds = $derived(
		new Set(data.editableAlertConfigs.map((c: AlertConfig) => c.id))
	);

	let activeOrgId = $derived(
		selectedOrgId || data.userOrganizations[0]?.id || data.projects[0]?.organizationId || ''
	);

	function getProjectName(projectId: string | null): string {
		if (!projectId) return 'Organization-Wide';
		const project = data.projects.find((p: { id: string; name: string }) => p.id === projectId);
		return project?.name ?? projectId;
	}

	function startEdit(config: AlertConfig) {
		editMode = true;
		editingConfig = config;
		scope = config.scope;
		selectedProjectId = config.projectId ?? '';
		selectedOrgId = config.organizationId ?? '';
		channel = config.channel;
		channelTarget = config.channelTarget;
		frequencyThreshold = config.frequencyThreshold;
		windowSeconds = config.windowSeconds;
		enabled = config.enabled;
		// Flow `E2`: a config untouched since the migration carries `NULL`, which is exactly today's
		// behaviour and reads as "All environments" — not as an empty scope and not as missing data.
		// Everything else opens on what the config actually holds, per decision 32's re-render rule.
		// BUG-05: only when the GET actually succeeded. Until then `environmentsOf` would answer
		// `null` for a rule that may hold a narrow scope, and seeding "all" from that is the silent
		// unscoping this whole file is written against.
		const stored = configEnvironmentsLoaded ? environmentsOf(config.id) : null;
		environmentMode = stored === null ? 'all' : 'specific';
		environmentSelection = stored === null ? [] : [...stored];
		environmentTouched = false;
		// BUG-16: these names came back from the server for THIS config, so they belong to this
		// config's target. Claiming the target here is what stops the effect — which is about to run
		// because `scope`/`selectedProjectId` just changed — from treating the seeding as a switch.
		selectionScopeKey =
			config.scope === 'project'
				? `project:${config.projectId ?? ''}`
				: `organization:${config.organizationId ?? ''}`;
		noDefaultNotice = null;
		fieldErrors = {};
		toastMessage = null;
	}

	function cancelEdit() {
		editMode = false;
		editingConfig = null;
		resetForm();
	}

	function resetForm() {
		scope = 'project';
		selectedProjectId = '';
		selectedOrgId = '';
		channel = 'email';
		channelTarget = '';
		frequencyThreshold = 50;
		windowSeconds = 60;
		enabled = true;
		// Nothing is pre-selected on a fresh form: an untouched picker sends no `environments` key at
		// all, which is what lets §4.7 apply the project's default (flow `E3`) or report that there is
		// none (`E4`).
		environmentMode = 'all';
		environmentSelection = [];
		environmentTouched = false;
		// BUG-16: nothing to protect, so the effect's next run just records the new target.
		selectionScopeKey = null;
		fieldErrors = {};
		toastMessage = null;
	}

	function clearToast() {
		toastMessage = null;
	}

	async function handleSubmit(event: SubmitEvent) {
		event.preventDefault();
		isSubmitting = true;
		fieldErrors = {};
		toastMessage = null;

		// Client-side validation checks
		if (scope === 'project' && !selectedProjectId) {
			fieldErrors.projectId = 'Please select a project';
			isSubmitting = false;
			return;
		}
		if (scope === 'organization' && data.userOrganizations.length > 1 && !activeOrgId) {
			fieldErrors.organizationId = 'Please select an organization';
			isSubmitting = false;
			return;
		}
		if (!channelTarget.trim()) {
			fieldErrors.channelTarget = 'Notification destination target is required';
			isSubmitting = false;
			return;
		}
		// Flow `E6`: deselecting everything is refused BEFORE the request goes out. `[]` is a `400`
		// (§4.7) because an alert that can never fire is not a state to store — and the server's own
		// `400` is still handled below, in case one arrives another way.
		if (environmentMode === 'specific' && environmentSelection.length === 0) {
			fieldErrors.environments =
				'Choose at least one environment, or switch this rule back to All environments.';
			isSubmitting = false;
			return;
		}

		const payload: Record<string, unknown> = {
			channel,
			channelTarget,
			frequencyThreshold,
			windowSeconds,
			enabled,
		};

		// Decision 32, all three rules in one place, because each of them is a way to unscope a live
		// rule by accident:
		//   - untouched picker  -> the key is OMITTED entirely, meaning "leave unchanged" on a PUT and
		//     "apply the project default" on a POST. A save that changes a threshold must not carry an
		//     `environments` array.
		//   - "All environments" -> `null`. NOT `[]` (a `400`) and NOT an omitted key, which would
		//     leave the rule scoped while looking like it had been widened.
		//   - specific          -> the chosen names, which the server then MERGES with the ones this
		//     caller could not have been offered.
		//   - stored scope unknown -> the key is OMITTED too (BUG-05). The picker is not rendered in
		//     that state, so `environmentTouched` cannot be set through the UI; this is the belt to
		//     that brace, so a future control cannot reintroduce a save built on a guess.
		if (environmentTouched && !storedScopeUnknown) {
			payload.environments = environmentMode === 'all' ? null : environmentSelection;
		}

		let url = '/api/alerts';
		let method = 'POST';

		if (editMode && editingConfig) {
			// Scope (project vs organization, and which one) is immutable via PUT — the toggle is disabled
			// while editing, and the payload intentionally omits projectId/organizationId so there is nothing
			// for the API's scope-change guard to reject. See api/alerts/+server.ts PUT.
			payload.id = editingConfig.id;
			method = 'PUT';
		} else if (scope === 'project') {
			payload.projectId = selectedProjectId;
		} else {
			payload.projectId = null;
			payload.organizationId = activeOrgId;
		}

		try {
			const response = await fetch(url, {
				method,
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(payload),
			});

			const result = await response.json();

			if (!response.ok) {
				// HTTP 400 / 422: Render inline field errors if specific, else toast
				if (response.status === 400 || response.status === 422) {
					const errMsg = typeof result.error === 'string' ? result.error : '';
					if (errMsg.toLowerCase().includes('project') || errMsg.toLowerCase().includes('projectid')) {
						fieldErrors.projectId = errMsg;
					} else if (errMsg.toLowerCase().includes('org') || errMsg.toLowerCase().includes('organizationid')) {
						fieldErrors.organizationId = errMsg;
					} else if (errMsg.toLowerCase().includes('environment')) {
						fieldErrors.environments = errMsg;
					} else if (errMsg.toLowerCase().includes('target') || errMsg.toLowerCase().includes('channel')) {
						fieldErrors.channelTarget = errMsg;
					} else {
						toastMessage = { text: errMsg || 'Validation error occurred', type: 'error' };
					}
				} else if (response.status === 403) {
					toastMessage = {
						text: result.error ?? 'Insufficient permissions to perform this action',
						type: 'error',
					};
				} else {
					toastMessage = {
						text: result.error ?? 'An unexpected error occurred. Please try again.',
						type: 'error',
					};
				}
			} else {
				toastMessage = {
					text: editMode ? 'Alert rule updated successfully' : 'Alert rule created successfully',
					type: 'success',
				};

				// Decision 32's third rule: re-render from what came BACK, never from what was sent. A
				// rule may legitimately hold an environment this picker could not offer — unregistered,
				// hidden, or outside this caller's grant — and the server preserves those on the merge.
				// Re-rendering from the request would show fewer environments than the rule has, and the
				// next save would look like a deselection nobody made.
				if (result?.id) {
					configEnvironments = {
						...configEnvironments,
						[result.id]: result.environments ?? null,
					};
				}

				// Task 13 / flow `E4`, decision 33. Reported once, at creation, on this screen — the
				// project has no default environment, so §4.7's fallback left the new rule covering
				// everything. It is not persisted and there is nothing to look for after a reload; the
				// standing condition is project settings' "no default set" line.
				noDefaultNotice =
					!editMode && result?.defaultEnvironmentApplied === false
						? 'This project has no default environment set, so this rule was created covering every environment. Set a default in the project\u2019s settings, or scope this rule explicitly above.'
						: null;

				resetForm();
				editMode = false;
				editingConfig = null;
				await invalidateAll();
			}
		} catch (err) {
			toastMessage = { text: 'Network error occurred', type: 'error' };
		} finally {
			isSubmitting = false;
		}
	}

	async function handleDelete(id: string) {
		isDeleting = true;
		try {
			const response = await fetch('/api/alerts', {
				method: 'DELETE',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ id }),
			});

			const result = await response.json();

			if (!response.ok) {
				toastMessage = {
					text: result.error ?? 'Failed to delete alert rule',
					type: 'error',
				};
			} else {
				toastMessage = { text: 'Alert rule deleted', type: 'success' };
				deletingConfigId = null;
				await invalidateAll();
			}
		} catch (err) {
			toastMessage = { text: 'Network error occurred during deletion', type: 'error' };
		} finally {
			isDeleting = false;
		}
	}
</script>

<div class="alerts-page">
	<header class="page-header">
		<h1>Alert Configuration</h1>
		<p class="subtitle">Set up real-time notifications for error spikes and threshold breaches</p>
	</header>

	{#if toastMessage}
		<div class="toast-banner" class:toast-error={toastMessage.type === 'error'} class:toast-success={toastMessage.type === 'success'}>
			<span>{toastMessage.text}</span>
			<button type="button" class="toast-dismiss" onclick={clearToast}>&times;</button>
		</div>
	{/if}

	{#if data.projects.length === 0 && data.userOrganizations.length === 0}
		<p class="no-projects">You don't have access to any projects or organizations yet.</p>
	{:else}
		<form onsubmit={handleSubmit}>
			<div class="form-section">
				<h2>{editMode ? 'Edit Alert Configuration' : 'Create New Alert'}</h2>

				<!--
					Task 13 / flow `E4`, decision 33. Reported at the moment of creation, from the POST
					response's `defaultEnvironmentApplied: false`, on the screen the rule was created
					from. It is deliberately NOT a toast on a timer, and equally deliberately not
					persistent: nothing stores this field, so there is nothing to render on a reload. The
					standing form of the warning is project settings' "no default set" line, which is
					where the condition is actually true and stays true.
				-->
				{#if noDefaultNotice}
					<div class="no-default-notice" role="status" data-testid="no-default-notice">
						<span>{noDefaultNotice}</span>
						<button type="button" class="toast-dismiss" onclick={() => (noDefaultNotice = null)}>&times;</button>
					</div>
				{/if}

				<!-- Form Scope Switcher -->
				<div class="form-group">
					<!--
						A bare `<label>` above a group of buttons (not one control it can associate with)
						was flagged by a11y_label_has_associated_control. This is a two-way toggle between
						buttons, not a set of radio inputs, so fieldset/legend doesn't fit either -- a
						labeled ARIA group is the correct semantics here.
					-->
					<span id="alert-rule-scope-label" class="form-group-label">Alert Rule Scope</span>
					<div class="segmented-control" role="group" aria-labelledby="alert-rule-scope-label">
						<button
							type="button"
							class="segmented-btn"
							class:active={scope === 'project'}
							disabled={editMode}
							title={editMode ? 'Scope cannot be changed when editing an existing rule' : ''}
							onclick={() => (scope = 'project')}
						>
							Project Alert
						</button>
						<button
							type="button"
							class="segmented-btn"
							class:active={scope === 'organization'}
							disabled={editMode || !data.canManageOrgAlerts}
							title={editMode
								? 'Scope cannot be changed when editing an existing rule'
								: !data.canManageOrgAlerts
									? 'Requires manage_keys org permission'
									: ''}
							onclick={() => (scope = 'organization')}
						>
							Organization-Wide Alert
							{#if !data.canManageOrgAlerts}
								<span class="lock-icon">🔒</span>
							{/if}
						</button>
					</div>
					{#if editMode}
						<p class="scope-hint">Scope cannot be changed when editing an existing rule.</p>
					{:else if !data.canManageOrgAlerts}
						<p class="scope-hint">Organization-wide rules require owner, admin, or engineer org role.</p>
					{/if}
				</div>

				{#if scope === 'project'}
					<div class="form-group">
						<label for="projectId">Target Project</label>
						<select
							id="projectId"
							name="projectId"
							bind:value={selectedProjectId}
							required
							class:input-error={!!fieldErrors.projectId}
						>
							<option value="">Select a project</option>
							{#each data.projects as project}
								<option value={project.id}>{project.name}</option>
							{/each}
						</select>
						{#if fieldErrors.projectId}
							<p class="field-error-text">{fieldErrors.projectId}</p>
						{/if}
					</div>
				{:else}
					<div class="form-group">
						<!--
							`for="organizationId"` associates this with the real select below when it renders
							(userOrganizations.length > 1). When it doesn't (the org-badge branch), there is no
							form control to associate with at all -- nothing here needs a label then either.
						-->
						<label for="organizationId">Target Scope</label>
						{#if data.userOrganizations.length > 1}
							<select
								id="organizationId"
								name="organizationId"
								bind:value={selectedOrgId}
								required
								class:input-error={!!fieldErrors.organizationId}
							>
								<option value="">Select an organization</option>
								{#each data.userOrganizations as org}
									<option value={org.id}>{org.name}</option>
								{/each}
							</select>
							{#if fieldErrors.organizationId}
								<p class="field-error-text">{fieldErrors.organizationId}</p>
							{/if}
						{:else}
							<div class="org-scope-box">
								<span class="org-badge-large">[ORG-WIDE]</span>
								<span class="org-scope-desc">Applies automatically to all current and future projects in your organization</span>
							</div>
						{/if}
					</div>
				{/if}


				<!--
					sentinel-project-environments task 12, flows `E1`, `E5`-`E9`, `E11`-`E13`. A choice
					between "every environment" and a set picked from the server's list — there is no
					free-text path here on purpose (`E8`): a typed name that matches nothing means
					"never alert", and nothing anywhere would report that it had happened.
				-->
				<div class="form-group">
					<span id="environment-scope-label" class="form-group-label">Environments</span>
					<div role="group" aria-labelledby="environment-scope-label" class="environment-picker">
						{#if storedScopeUnknown}
							<!--
								BUG-05. The stored scope of this rule could not be read, so there is nothing
								honest to seed a picker from. Editing everything else on the rule stays open —
								an omitted `environments` key leaves the scope exactly as it is (decision 32) —
								and the picker comes back as soon as the read succeeds.
							-->
							<p class="environment-hint" data-testid="edit-scope-unknown">
								{configEnvironmentsLoading
									? 'Reading this rule’s environments…'
									: 'This rule’s environments could not be read, so they cannot be changed here. Everything else on the rule can still be saved and its environments are left untouched.'}
							</p>
							{#if !configEnvironmentsLoading}
								<button
									type="button"
									class="btn-action"
									data-testid="edit-scope-retry"
									onclick={loadConfigEnvironments}
								>
									Retry
								</button>
							{/if}
						{:else}
						<label class="environment-mode">
							<input
								type="radio"
								name="environmentMode"
								checked={environmentMode === 'all'}
								onchange={() => setEnvironmentMode('all')}
							/>
							<span>All environments</span>
						</label>
						<label class="environment-mode">
							<input
								type="radio"
								name="environmentMode"
								checked={environmentMode === 'specific'}
								onchange={() => setEnvironmentMode('specific')}
							/>
							<span>Only these environments</span>
						</label>

						{#if environmentMode === 'specific'}
							{#if !offeredLoaded}
								<p class="environment-hint">Loading environments…</p>
							{:else if offeredEnvironments.length === 0}
								<!--
									Under decision 8 a project has no declared environments until somebody adds
									one, so this is the ordinary day-one state rather than a fault. An empty
									multi-select would read as broken.
								-->
								<p class="environment-hint">
									{scope === 'project'
										? 'This project has no declared environments yet, so there is nothing to narrow to. Declare one in the project’s settings, or leave this rule covering every environment.'
										: 'None of the projects you can reach in this organization have declared environments yet.'}
								</p>
							{:else}
								<div class="environment-options">
									{#each offeredEnvironments as name (name)}
										<label class="environment-option">
											<input
												type="checkbox"
												checked={environmentSelection.includes(name)}
												onchange={() => toggleEnvironment(name)}
											/>
											<span>{name}</span>
										</label>
									{/each}
								</div>
							{/if}
							<!--
								Decision 48 / flow `E11a`: names the picker could not offer — unregistered,
								hidden, or outside this caller's grant — are preserved by the server's merge
								and still shown here, and re-submitting them is explicitly allowed. They are
								not stripped, because stripping them is the silent unscoping decision 32
								exists to prevent.
							-->
							<!--
								BUG-16: `editMode` and `offeredLoaded` are both load-bearing. "already on this rule"
								is a statement about names the SERVER merged onto an EXISTING rule — on the create
								path there is no rule and no server answer, so any name reaching this block there
								is a leftover from a different scope target, and rendering it invites submitting it.
								And while the offered list is still in flight it is `[]`, so every selected name
								would momentarily qualify as un-offerable.
							-->
							{#each editMode && offeredLoaded ? environmentSelection.filter((n) => !offeredEnvironments.includes(n)) : [] as name (name)}
								<label class="environment-option">
									<input type="checkbox" checked onchange={() => toggleEnvironment(name)} />
									<span>{name}</span>
									<span class="environment-carried">already on this rule</span>
								</label>
							{/each}
						{/if}
						{/if}
					</div>
					{#if fieldErrors.environments}
						<p class="field-error-text">{fieldErrors.environments}</p>
					{/if}
				</div>

				<div class="form-group">
					<label for="channel">Notification Channel</label>
					<select id="channel" name="channel" bind:value={channel} required>
						<option value="email">Email</option>
						<option value="telegram">Telegram</option>
					</select>
				</div>

				<div class="form-group">
					<label for="channelTarget">
						{channel === 'email' ? 'Email Address' : 'Telegram Bot Token / Chat ID'}
					</label>
					<input
						id="channelTarget"
						name="channelTarget"
						type="text"
						bind:value={channelTarget}
						placeholder={channel === 'email' ? 'user@example.com' : 'bot_token:chat_id'}
						required
						class:input-error={!!fieldErrors.channelTarget}
					/>
					{#if fieldErrors.channelTarget}
						<p class="field-error-text">{fieldErrors.channelTarget}</p>
					{/if}
				</div>

				<div class="form-row">
					<div class="form-group">
						<label for="frequencyThreshold">Threshold (errors per window)</label>
						<input
							id="frequencyThreshold"
							name="frequencyThreshold"
							type="number"
							min="1"
							bind:value={frequencyThreshold}
							required
						/>
					</div>

					<div class="form-group">
						<label for="windowSeconds">Time Window (seconds)</label>
						<input
							id="windowSeconds"
							name="windowSeconds"
							type="number"
							min="1"
							bind:value={windowSeconds}
							required
						/>
					</div>
				</div>

				<div class="form-group checkbox-group">
					<label>
						<input type="checkbox" bind:checked={enabled} />
						<span>Enabled</span>
					</label>
				</div>

				<div class="form-actions">
					{#if editMode}
						<button type="button" class="btn btn-cancel" onclick={cancelEdit}>Cancel</button>
					{/if}
					<button type="submit" class="btn btn-submit" disabled={isSubmitting}>
						{editMode ? 'Update Alert' : 'Create Alert'}
					</button>
				</div>
			</div>
		</form>

		{#if data.alertConfigs.length > 0}
			<div class="configs-section">
				<h2>Existing Alert Configurations</h2>
				<!--
					BUG-05. The read that carries every rule's environment scope failed. It used to fail
					silently and the table below went on stating "All environments" for every row.
				-->
				{#if configEnvironmentsError}
					<p class="environments-error" role="alert" data-testid="config-environments-error">
						<span>{configEnvironmentsError}</span>
						<button
							type="button"
							class="btn-action"
							data-testid="config-environments-retry"
							disabled={configEnvironmentsLoading}
							onclick={loadConfigEnvironments}
						>
							Retry
						</button>
					</p>
				{/if}
				<table class="configs-table">
					<thead>
						<tr>
							<th>Scope / Target</th>
							<th>Environments</th>
							<th>Channel</th>
							<th>Target</th>
							<th>Threshold</th>
							<th>Window</th>
							<th>Status</th>
							<th>Actions</th>
						</tr>
					</thead>
					<tbody>
						{#each data.alertConfigs as config}
							<tr>
								<td class="project-cell">
									{#if config.scope === 'organization'}
										<span class="org-badge">[ORG-WIDE]</span>
									{:else}
										<span class="project-name">{getProjectName(config.projectId)}</span>
									{/if}
								</td>
								<!--
									Flow `E2`: `null` reads as "All environments", which is exactly what every
									pre-migration row carries and exactly what it has always done. Never a blank
									cell — a blank reads as missing data rather than as the stated fact.
								-->
								<td class="environments-cell" data-testid="config-environments">
									{#if !configEnvironmentsLoaded}
										<!--
											BUG-05. There is no stated fact to show yet, so none is shown. "All
											environments" here would be a guess, and it is the one guess that reads
											as a wider scope than the rule may actually have.
										-->
										<span class="environments-unknown" data-testid="config-environments-unknown">
											{configEnvironmentsLoading ? 'Loading…' : 'Unknown'}
										</span>
									{:else if environmentsOf(config.id) === null}
										All environments
									{:else}
										{environmentsOf(config.id)?.join(', ')}
									{/if}
								</td>
								<td class="channel-cell">{config.channel}</td>
								<td class="channel-target">{config.channelTarget}</td>
								<td class="mono-cell">{config.frequencyThreshold} errors</td>
								<td class="mono-cell">{config.windowSeconds}s</td>
								<td>
									<span class="status-badge" class:enabled={config.enabled} class:disabled={!config.enabled}>
										{config.enabled ? 'Enabled' : 'Disabled'}
									</span>
								</td>
								<td class="actions-cell">
									{#if editableAlertConfigIds.has(config.id)}
										{#if deletingConfigId === config.id}
											<div class="delete-confirm-box">
												<span class="confirm-text">Confirm delete?</span>
												<button
													type="button"
													class="btn-confirm-yes"
													disabled={isDeleting}
													onclick={() => handleDelete(config.id)}
												>
													Yes
												</button>
												<button
													type="button"
													class="btn-confirm-no"
													onclick={() => (deletingConfigId = null)}
												>
													No
												</button>
											</div>
										{:else}
											<button type="button" class="btn-action" onclick={() => startEdit(config)}>Edit</button>
											<button
												type="button"
												class="btn-action btn-delete"
												onclick={() => (deletingConfigId = config.id)}
											>
												Delete
											</button>
										{/if}
									{:else}
										<span class="no-permission">View only</span>
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{:else}
			<p class="no-configs">No alert configurations found.</p>
		{/if}
	{/if}
</div>
<style>
	.alerts-page {
		max-width: 1100px;
		margin: 0 auto;
	}

	.page-header {
		border-bottom: 1px solid var(--border-color);
		padding-bottom: 0.75rem;
		margin-bottom: 1.5rem;
	}

	h1 {
		font-size: 1.25rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.subtitle {
		color: var(--text-muted);
		font-size: 0.8125rem;
	}

	h2 {
		font-size: 0.9375rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 1rem;
	}

	/* Toast / Banner Messages */
	.toast-banner {
		display: flex;
		align-items: center;
		justify-content: space-between;
		padding: 0.625rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		margin-bottom: 1.25rem;
	}

	.toast-error {
		background: rgba(239, 68, 68, 0.12);
		border: 1px solid rgba(239, 68, 68, 0.3);
		color: #ef4444;
	}

	.toast-success {
		background: rgba(16, 185, 129, 0.12);
		border: 1px solid rgba(16, 185, 129, 0.3);
		color: #10b981;
	}

	.environment-picker {
		display: flex;
		flex-direction: column;
		gap: 0.375rem;
	}

	.environment-mode,
	.environment-option {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.85rem;
	}

	.environment-options {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		padding-left: 1.25rem;
	}

	.environment-hint {
		font-size: 0.75rem;
		opacity: 0.75;
		line-height: 1.45;
		margin: 0.25rem 0 0 1.25rem;
	}

	.environment-carried {
		font-size: 0.7rem;
		opacity: 0.7;
	}

	.environments-unknown {
		color: #6b7280;
		font-style: italic;
	}

	.environments-error {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin: 0 0 0.75rem;
		padding: 0.5rem 0.75rem;
		border: 1px solid #fca5a5;
		border-radius: 4px;
		background: #fef2f2;
		color: #991b1b;
	}

	.environments-cell {
		font-size: 0.8rem;
	}

	.no-default-notice {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 0.75rem;
		border: 1px solid var(--border-color);
		border-left-width: 3px;
		border-radius: var(--radius-sm);
		padding: 0.625rem 0.75rem;
		font-size: 0.8rem;
		line-height: 1.45;
		margin-bottom: 1rem;
	}

	.toast-dismiss {
		background: transparent;
		border: none;
		color: inherit;
		font-size: 1.125rem;
		cursor: pointer;
		padding: 0 0.25rem;
	}

	.form-section {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		padding: 1.25rem;
		border-radius: var(--radius-md);
		margin-bottom: 2rem;
	}

	.form-group {
		margin-bottom: 1rem;
	}

	.form-row {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 1rem;
	}

	label,
	.form-group-label {
		display: block;
		margin-bottom: 0.375rem;
		font-size: 0.75rem;
		color: var(--text-muted);
		font-weight: 500;
	}

	/* Segmented Scope Control */
	.segmented-control {
		display: flex;
		gap: 0.25rem;
		background: var(--bg-root);
		padding: 0.25rem;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		width: fit-content;
	}

	.segmented-btn {
		background: transparent;
		border: none;
		color: var(--text-muted);
		padding: 0.375rem 0.75rem;
		font-size: 0.8125rem;
		font-weight: 500;
		border-radius: var(--radius-sm);
		cursor: pointer;
		transition: all 0.15s ease;
		display: flex;
		align-items: center;
		gap: 0.375rem;
	}

	.segmented-btn.active {
		background: var(--bg-surface);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
	}

	.segmented-btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.scope-hint {
		font-size: 0.75rem;
		color: var(--text-subtle);
		margin-top: 0.375rem;
	}

	.lock-icon {
		font-size: 0.75rem;
	}

	.org-scope-box {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		padding: 0.5rem 0.75rem;
		background: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
	}

	.org-badge-large {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		font-weight: 600;
		padding: 0.2rem 0.5rem;
		border-radius: var(--radius-sm);
		background: rgba(59, 130, 246, 0.15);
		color: #3b82f6;
		border: 1px solid rgba(59, 130, 246, 0.3);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.org-scope-desc {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	select,
	input[type='text'],
	input[type='number'] {
		width: 100%;
		padding: 0.45rem 0.625rem;
		background-color: var(--bg-root);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		color: var(--text-primary);
		font-size: 0.8125rem;
	}

	select.input-error,
	input.input-error {
		border-color: #ef4444;
	}

	.field-error-text {
		color: #ef4444;
		font-size: 0.75rem;
		margin-top: 0.25rem;
	}

	.checkbox-group label {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		cursor: pointer;
		font-size: 0.8125rem;
		color: var(--text-primary);
	}

	.form-actions {
		display: flex;
		gap: 0.75rem;
		margin-top: 1.25rem;
	}

	.btn {
		padding: 0.45rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		font-weight: 500;
		cursor: pointer;
		border: none;
		transition: background 0.15s ease;
	}

	.btn-submit {
		background: var(--color-primary);
		color: var(--text-primary);
	}

	.btn-submit:hover:not(:disabled) {
		background: var(--color-primary-hover);
	}

	.btn-submit:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.btn-cancel {
		background: var(--bg-root);
		color: var(--text-muted);
		border: 1px solid var(--border-color);
	}

	.btn-cancel:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.configs-section {
		margin-top: 2rem;
	}

	.configs-table {
		width: 100%;
		border-collapse: collapse;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		overflow: hidden;
	}

	.configs-table th,
	.configs-table td {
		padding: 0.625rem 0.875rem;
		text-align: left;
		font-size: 0.8125rem;
		border-bottom: 1px solid var(--border-color);
	}

	.configs-table th {
		background: var(--bg-root);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		text-transform: uppercase;
		color: var(--text-subtle);
		letter-spacing: 0.05em;
	}

	.project-name {
		color: var(--text-primary);
		font-weight: 500;
	}

	.org-badge {
		display: inline-block;
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		font-weight: 600;
		padding: 0.125rem 0.4rem;
		border-radius: var(--radius-sm);
		background: rgba(59, 130, 246, 0.15);
		color: #3b82f6;
		border: 1px solid rgba(59, 130, 246, 0.3);
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.channel-target,
	.mono-cell {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--text-muted);
		font-variant-numeric: tabular-nums;
	}

	.channel-target {
		max-width: 180px;
		overflow: hidden;
		text-overflow: ellipsis;
		white-space: nowrap;
	}

	.status-badge {
		display: inline-block;
		padding: 0.125rem 0.5rem;
		border-radius: var(--radius-sm);
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
	}

	.status-badge.enabled {
		background: var(--status-resolved-bg);
		color: var(--status-resolved-text);
	}

	.status-badge.disabled {
		background: var(--severity-critical-bg);
		color: var(--severity-critical-text);
	}

	.actions-cell {
		white-space: nowrap;
	}

	.btn-action {
		background: transparent;
		border: 1px solid var(--border-color);
		color: var(--text-muted);
		padding: 0.25rem 0.5rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		cursor: pointer;
		margin-right: 0.25rem;
	}

	.btn-action:hover {
		background: var(--bg-surface-hover);
		color: var(--text-primary);
	}

	.btn-delete:hover {
		border-color: rgba(239, 68, 68, 0.5);
		color: #ef4444;
	}

	.delete-confirm-box {
		display: inline-flex;
		align-items: center;
		gap: 0.375rem;
		font-size: 0.75rem;
	}

	.confirm-text {
		color: #ef4444;
		font-weight: 500;
	}

	.btn-confirm-yes {
		background: #ef4444;
		color: white;
		border: none;
		padding: 0.2rem 0.4rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		cursor: pointer;
	}

	.btn-confirm-no {
		background: var(--bg-root);
		color: var(--text-muted);
		border: 1px solid var(--border-color);
		padding: 0.2rem 0.4rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		cursor: pointer;
	}

	.no-projects,
	.no-configs {
		color: var(--text-subtle);
		margin-top: 2rem;
		font-size: 0.875rem;
	}

	.no-permission {
		color: var(--text-subtle);
		font-size: 0.75rem;
	}
</style>