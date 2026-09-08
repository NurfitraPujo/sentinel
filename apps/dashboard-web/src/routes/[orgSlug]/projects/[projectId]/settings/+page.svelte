<script lang="ts">
	import { onMount } from 'svelte';
	import { AGENT_REPO_PROVIDERS } from '$lib/constants/agent-repo';

	interface AgentSettings {
		fixEnabled: boolean;
		maxPrsPerDay: number | null;
	}

	interface RepoConnection {
		provider: string;
		owner: string;
		repo: string;
		defaultBranch: string;
		testCmd: string;
		agentCmd: string | null;
		cloneDepth: number | null;
	}

	// sentinel-project-environments §4.1 — one registry row, as the management view returns it
	// (`includeHidden=true`). `name` is the join key against `issues.environment` and is immutable;
	// `displayName` is the only editable label (§4.2).
	interface EnvironmentRow {
		id: string;
		name: string;
		displayName: string;
		isDefault: boolean;
		hidden: boolean;
		firstSeenAt: string | null;
		lastSeenAt: string | null;
		createdAt: string;
	}

	// §4.10's `members` array, added by decision 31 — every person who can reach this project's
	// issues by any path, NOT just the narrowed ones. `environments: null` means UNRESTRICTED and is
	// deliberately `null` rather than `[]`: `[]` is a state the `PUT` refuses to store, so on this
	// wire "sees nothing" and "sees everything" would otherwise be the same value.
	interface MemberRow {
		userId: string;
		displayName: string;
		projectRole: string | null;
		orgRole: string | null;
		environments: string[] | null;
	}

	interface Props {
		data: {
			orgId: string;
			orgSlug: string;
			projectId: string;
			projectName: string;
			canManageAgents: boolean;
			agentSettings: AgentSettings;
			repoConnection: RepoConnection | null;
			// §2.1's two permissions for the sections below. The loader is BE's under contract §6a and
			// does not carry them yet, so they are optional and both sections render when they are
			// absent — hiding a control is a courtesy and the routes reject regardless (flows `P9`,
			// `G7`). Recorded in `status/fe.md` as a message to BE, not worked around with a query
			// here.
			canWrite?: boolean;
			canManageMembers?: boolean;
		};
	}

	let { data }: Props = $props();

	let canWrite = $derived(data.canWrite !== false);
	let canManageMembers = $derived(data.canManageMembers !== false);

	// All of the $state initializers below deliberately seed once from server-loaded props, same
	// pattern as settings/agents/+page.svelte's `agents` state -- each needs its own
	// svelte-ignore, the directive does not carry across statements.
	// svelte-ignore state_referenced_locally
	let fixEnabled = $state(data.agentSettings.fixEnabled);
	// svelte-ignore state_referenced_locally
	let maxPrsPerDay = $state<string>(data.agentSettings.maxPrsPerDay?.toString() ?? '');
	let savingSettings = $state(false);

	// svelte-ignore state_referenced_locally
	let repoConnection = $state<RepoConnection | null>(data.repoConnection);
	// svelte-ignore state_referenced_locally
	let provider = $state<string>(data.repoConnection?.provider ?? AGENT_REPO_PROVIDERS[0]);
	// svelte-ignore state_referenced_locally
	let owner = $state(data.repoConnection?.owner ?? '');
	// svelte-ignore state_referenced_locally
	let repo = $state(data.repoConnection?.repo ?? '');
	// svelte-ignore state_referenced_locally
	let defaultBranch = $state(data.repoConnection?.defaultBranch ?? 'main');
	// svelte-ignore state_referenced_locally
	let testCmd = $state(data.repoConnection?.testCmd ?? '');
	// svelte-ignore state_referenced_locally
	let agentCmd = $state(data.repoConnection?.agentCmd ?? '');
	// svelte-ignore state_referenced_locally
	let cloneDepth = $state<string>(data.repoConnection?.cloneDepth?.toString() ?? '');
	let savingRepo = $state(false);
	let disconnecting = $state(false);

	let toastMessage = $state<string | null>(null);
	let toastType = $state<'error' | 'success'>('error');

	function showToast(message: string, type: 'error' | 'success' = 'error') {
		toastMessage = message;
		toastType = type;
		setTimeout(() => {
			if (toastMessage === message) toastMessage = null;
		}, 5000);
	}

	async function saveAgentSettings() {
		savingSettings = true;
		try {
			const trimmedMax = maxPrsPerDay.trim();
			const res = await fetch(`/api/organizations/${data.orgId}/projects/${data.projectId}/agent-settings`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					fixEnabled,
					maxPrsPerDay: trimmedMax === '' ? null : Number(trimmedMax),
				}),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to save agent settings' }));
				showToast(err.message || `Error ${res.status}: failed to save agent settings`);
				return;
			}
			const body = await res.json();
			fixEnabled = body.settings.fixEnabled;
			maxPrsPerDay = body.settings.maxPrsPerDay?.toString() ?? '';
			showToast('Agent settings saved', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while saving agent settings');
		} finally {
			savingSettings = false;
		}
	}

	async function saveRepoConnection() {
		savingRepo = true;
		try {
			const trimmedDepth = cloneDepth.trim();
			const res = await fetch(`/api/organizations/${data.orgId}/projects/${data.projectId}/repo-connection`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					provider,
					owner,
					repo,
					defaultBranch,
					testCmd,
					agentCmd: agentCmd.trim() === '' ? null : agentCmd,
					cloneDepth: trimmedDepth === '' ? null : Number(trimmedDepth),
				}),
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to save repo connection' }));
				showToast(err.message || `Error ${res.status}: failed to save repo connection`);
				return;
			}
			const body = await res.json();
			repoConnection = body.connection;
			showToast('Repo connection saved', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while saving repo connection');
		} finally {
			savingRepo = false;
		}
	}

	// ---------------------------------------------------------------------------------------------
	// sentinel-project-environments tasks 8-11 — the Environments registry and the Environment
	// access sections. Contract §4.1-§4.3 and §4.10, flows `P1`-`P11` and `G1`-`G8`.
	// ---------------------------------------------------------------------------------------------

	let environments = $state<EnvironmentRow[]>([]);
	let environmentsLoaded = $state(false);
	let environmentsError = $state<string | null>(null);

	let members = $state<MemberRow[]>([]);
	let membersLoaded = $state(false);
	let membersError = $state<string | null>(null);

	let newEnvironmentName = $state('');
	let addingEnvironment = $state(false);
	let addEnvironmentError = $state<string | null>(null);

	let editingLabelFor = $state<string | null>(null);
	let labelDraft = $state('');
	let busyEnvironmentId = $state<string | null>(null);
	let environmentActionError = $state<string | null>(null);

	let limitingUserId = $state<string | null>(null);
	let limitSelection = $state<string[]>([]);
	let limitError = $state<string | null>(null);
	let busyMemberId = $state<string | null>(null);

	let environmentsBase = $derived(
		`/api/organizations/${data.orgId}/projects/${data.projectId}/environments`
	);
	let grantsBase = $derived(
		`/api/organizations/${data.orgId}/projects/${data.projectId}/environment-grants`
	);

	// Flow `P1` and criterion 12. Decision 33 made this line the PERSISTENT home of decision 11's
	// warning: "no default is set" is a property of the PROJECT, so it is true here and stays true,
	// whereas the alert-settings notice is a one-time creation event. It is rendered as a visible
	// condition, never as an absent marker — an unset default is the state in which every alert
	// config created from now on silently covers every environment.
	let hasDefaultEnvironment = $derived(environments.some((env) => env.isDefault));

	// §7: this app has no `{error:{code,message}}` envelope — SvelteKit's `error(status, message)`
	// yields `{ "message": "…" }`. Branch on the STATUS, never on the message text.
	async function messageFrom(res: Response, fallback: string): Promise<string> {
		const body = await res.json().catch(() => ({}) as { message?: string });
		return typeof body?.message === 'string' && body.message ? body.message : fallback;
	}

	async function loadEnvironments() {
		environmentsError = null;
		try {
			// §4.1 with `includeHidden=true` — the MANAGEMENT view. It takes `write` and is not
			// environment-scope filtered: an environment's name is not protected, its data is
			// (§12 *Known limits* 3), and hiding the rows from the person who administers them makes
			// the registry unmanageable by exactly the people this section is for.
			const res = await fetch(`${environmentsBase}?includeHidden=true`);
			if (!res.ok) {
				environmentsError = await messageFrom(res, `Could not load environments (${res.status}).`);
				return;
			}
			const body = await res.json();
			environments = (body.environments ?? []) as EnvironmentRow[];
		} catch {
			environmentsError = 'Could not reach the server to load this project\u2019s environments.';
		} finally {
			environmentsLoaded = true;
		}
	}

	async function loadMembers() {
		membersError = null;
		try {
			const res = await fetch(grantsBase);
			if (!res.ok) {
				membersError = await messageFrom(res, `Could not load environment access (${res.status}).`);
				return;
			}
			const body = await res.json();
			// Decision 31: the rows come from `members`, never from `grants`. `grants` holds only the
			// people who ARE narrowed, so a screen built from it has nobody to put them beside — and on
			// a real deployment it would be empty even where people are narrowed.
			members = (body.members ?? []) as MemberRow[];
		} catch {
			membersError = 'Could not reach the server to load environment access.';
		} finally {
			membersLoaded = true;
		}
	}

	onMount(() => {
		if (canWrite) loadEnvironments();
		if (canManageMembers) loadMembers();
	});

	async function addEnvironment() {
		const name = newEnvironmentName.trim();
		addEnvironmentError = null;
		if (!name) {
			addEnvironmentError = 'Give the environment the name your services report, e.g. eu-west-1.';
			return;
		}
		addingEnvironment = true;
		try {
			const res = await fetch(environmentsBase, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ name }),
			});
			if (!res.ok) {
				const message = await messageFrom(res, 'Could not add that environment.');
				// Flows `P6` and `P7`: a person can act on a sentence, not on a status code. Branch on
				// the status (§7) and add what the reader is supposed to do next.
				if (res.status === 409) {
					addEnvironmentError = `${name} is already in this project\u2019s list above — there is nothing more to add.`;
				} else if (res.status === 422) {
					addEnvironmentError = `${message} Hide an environment you no longer report from to make room, or ask an admin to raise the limit.`;
				} else {
					addEnvironmentError = message;
				}
				return;
			}
			const created = (await res.json()) as EnvironmentRow;
			environments = [...environments, created].sort((a, b) => a.name.localeCompare(b.name));
			newEnvironmentName = '';
			showToast(`Added ${created.name}`, 'success');
		} catch {
			addEnvironmentError = 'Could not reach the server to add that environment.';
		} finally {
			addingEnvironment = false;
		}
	}

	async function patchEnvironment(
		env: EnvironmentRow,
		updates: { displayName?: string; isDefault?: boolean; hidden?: boolean }
	) {
		busyEnvironmentId = env.id;
		environmentActionError = null;
		try {
			const res = await fetch(`${environmentsBase}/${env.id}`, {
				method: 'PATCH',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify(updates),
			});
			if (!res.ok) {
				environmentActionError = await messageFrom(res, 'Could not save that change.');
				return;
			}
			const updated = (await res.json()) as EnvironmentRow;
			// `isDefault: true` clears every other default in the same transaction (§4.2), so the whole
			// list is re-derived rather than the one row patched — otherwise two rows read as default
			// until the next reload.
			environments = environments.map((row) =>
				row.id === updated.id
					? updated
					: updates.isDefault === true
						? { ...row, isDefault: false }
						: row
			);
		} catch {
			environmentActionError = 'Could not reach the server to save that change.';
		} finally {
			busyEnvironmentId = null;
		}
	}

	function startLabelEdit(env: EnvironmentRow) {
		editingLabelFor = env.id;
		labelDraft = env.displayName;
	}

	async function saveLabel(env: EnvironmentRow) {
		const displayName = labelDraft.trim();
		if (!displayName) {
			environmentActionError = 'A label cannot be empty. Clear the edit to keep the current one.';
			return;
		}
		await patchEnvironment(env, { displayName });
		if (!environmentActionError) editingLabelFor = null;
	}

	function startLimit(member: MemberRow) {
		limitingUserId = member.userId;
		// Nothing is invented here: the control opens on what the member already holds, and on the
		// empty selection for an unrestricted member — never pre-filled with a guess.
		limitSelection = member.environments ? [...member.environments] : [];
		limitError = null;
	}

	function toggleLimitSelection(name: string) {
		limitSelection = limitSelection.includes(name)
			? limitSelection.filter((n) => n !== name)
			: [...limitSelection, name];
		limitError = null;
	}

	async function saveLimit(member: MemberRow) {
		// Flow `G3`: `[]` is refused BEFORE the request goes out — a member who can see nothing should
		// be removed from the project, not stored as a grant that hides everything from them. The
		// server's own `400` is handled below in case it ever gets there another way.
		if (limitSelection.length === 0) {
			limitError =
				'Choose at least one environment. To give this person every environment, use “Remove limit” instead.';
			return;
		}
		busyMemberId = member.userId;
		limitError = null;
		try {
			const res = await fetch(`${grantsBase}/${member.userId}`, {
				method: 'PUT',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ environments: limitSelection }),
			});
			if (!res.ok) {
				limitError = await messageFrom(res, 'Could not save that limit.');
				return;
			}
			const saved = [...limitSelection];
			members = members.map((m) =>
				m.userId === member.userId ? { ...m, environments: saved } : m
			);
			limitingUserId = null;
			showToast(`${member.displayName} is now limited to ${saved.join(', ')}`, 'success');
		} catch {
			limitError = 'Could not reach the server to save that limit.';
		} finally {
			busyMemberId = null;
		}
	}

	async function removeLimit(member: MemberRow) {
		busyMemberId = member.userId;
		limitError = null;
		try {
			// §4.10: `DELETE` restores unrestricted access, because absence of a grant row is what
			// "every environment" means. `204` on somebody who has no grant either.
			const res = await fetch(`${grantsBase}/${member.userId}`, { method: 'DELETE' });
			if (!res.ok) {
				limitError = await messageFrom(res, 'Could not remove that limit.');
				return;
			}
			members = members.map((m) => (m.userId === member.userId ? { ...m, environments: null } : m));
			if (limitingUserId === member.userId) limitingUserId = null;
			showToast(`${member.displayName} can see all environments again`, 'success');
		} catch {
			limitError = 'Could not reach the server to remove that limit.';
		} finally {
			busyMemberId = null;
		}
	}

	function formatSeen(value: string | null): string {
		if (!value) return 'never';
		return new Date(value).toLocaleString('en-US', {
			month: 'short',
			day: 'numeric',
			hour: '2-digit',
			minute: '2-digit',
		});
	}

	async function disconnectRepo() {
		disconnecting = true;
		try {
			const res = await fetch(`/api/organizations/${data.orgId}/projects/${data.projectId}/repo-connection`, {
				method: 'DELETE',
			});
			if (!res.ok) {
				const err = await res.json().catch(() => ({ message: 'Failed to disconnect repo' }));
				showToast(err.message || `Error ${res.status}: failed to disconnect repo`);
				return;
			}
			repoConnection = null;
			owner = '';
			repo = '';
			defaultBranch = 'main';
			testCmd = '';
			agentCmd = '';
			cloneDepth = '';
			provider = AGENT_REPO_PROVIDERS[0];
			showToast('Repo disconnected', 'success');
		} catch (err: any) {
			showToast(err?.message || 'Network error while disconnecting repo');
		} finally {
			disconnecting = false;
		}
	}
</script>

<div class="settings-page">
	<header class="settings-header">
		<div>
			<span class="project-badge">Project: {data.projectName}</span>
			<h1>Project settings</h1>
		</div>
	</header>

	{#if toastMessage}
		<div class="toast" class:toast-error={toastType === 'error'} class:toast-success={toastType === 'success'} role="status">
			<span>{toastMessage}</span>
			<button type="button" onclick={() => (toastMessage = null)}>✕</button>
		</div>
	{/if}

	{#if data.canManageAgents}
		<section class="agent-automation">
			<h2>Agent automation</h2>
			<p class="subtitle">
				Let Sentinel's fix agent open pull requests for this project's issues. Owner/admin only.
			</p>

			<div class="field-row">
				<label class="toggle-label">
					<input type="checkbox" bind:checked={fixEnabled} />
					Enable automated fixes
				</label>
			</div>

			<div class="field-row">
				<label for="max-prs">Max PRs per day (optional)</label>
				<input id="max-prs" type="number" min="1" step="1" bind:value={maxPrsPerDay} placeholder="unlimited" />
			</div>

			<button type="button" disabled={savingSettings} onclick={saveAgentSettings}>
				{savingSettings ? 'Saving…' : 'Save agent settings'}
			</button>

			<h3>Repository connection</h3>
			{#if !repoConnection}
				<p class="empty-state">No repository connected yet.</p>
			{:else}
				<p class="repo-summary">
					Connected: <code>{repoConnection.provider}:{repoConnection.owner}/{repoConnection.repo}</code>
					(branch <code>{repoConnection.defaultBranch}</code>)
				</p>
			{/if}

			<div class="field-grid">
				<div class="field-row">
					<label for="provider">Provider</label>
					<select id="provider" bind:value={provider}>
						{#each AGENT_REPO_PROVIDERS as p (p)}
							<option value={p}>{p}</option>
						{/each}
					</select>
				</div>
				<div class="field-row">
					<label for="owner">Owner</label>
					<input id="owner" type="text" bind:value={owner} placeholder="org-or-user" />
				</div>
				<div class="field-row">
					<label for="repo">Repo</label>
					<input id="repo" type="text" bind:value={repo} placeholder="repo-name" />
				</div>
				<div class="field-row">
					<label for="default-branch">Default branch</label>
					<input id="default-branch" type="text" bind:value={defaultBranch} placeholder="main" />
				</div>
				<div class="field-row wide">
					<label for="test-cmd">Test command</label>
					<input id="test-cmd" type="text" bind:value={testCmd} placeholder="pnpm test" />
				</div>
				<div class="field-row wide">
					<label for="agent-cmd">Agent command (optional)</label>
					<input id="agent-cmd" type="text" bind:value={agentCmd} placeholder="defaults to the worker's built-in agent" />
				</div>
				<div class="field-row">
					<label for="clone-depth">Clone depth (optional)</label>
					<input id="clone-depth" type="number" min="1" step="1" bind:value={cloneDepth} placeholder="full history" />
				</div>
			</div>

			<div class="repo-actions">
				<button type="button" disabled={savingRepo} onclick={saveRepoConnection}>
					{savingRepo ? 'Saving…' : repoConnection ? 'Update repo connection' : 'Connect repo'}
				</button>
				{#if repoConnection}
					<button type="button" class="danger" disabled={disconnecting} onclick={disconnectRepo}>
						{disconnecting ? 'Disconnecting…' : 'Disconnect'}
					</button>
				{/if}
			</div>
		</section>
	{/if}

	<!--
		sentinel-project-environments tasks 8-9, flows `P1`-`P11`. Placement (`P10`) is delegated to
		the implementer: this sits directly after "Agent automation" and reuses that section's frame,
		because these are two project-level settings groups and inventing a second visual idiom for
		one of them would be a redesign the brief explicitly rules out.
	-->
	{#if canWrite}
		<section class="environments" id="environments">
			<h2>Environments</h2>
			<p class="subtitle">
				The environments this project reports from. Every environment selector in Sentinel is
				built from this list.
			</p>

			{#if !environmentsLoaded}
				<p class="empty-state">Loading environments…</p>
			{:else if environmentsError}
				<p class="section-error" role="status">{environmentsError}</p>
			{:else if environments.length === 0}
				<!--
					Flow `P11`, and under decision 8 this is EVERY project's first visit — no registry row
					is ever created by traffic, only here. It has to read as a starting point rather than
					as an error or an outage, and it has to say the thing a reader will otherwise assume
					is untrue: the errors are arriving and being stored either way.
				-->
				<div class="registry-empty" data-testid="environments-empty">
					<p class="registry-empty-title">No environments have been declared yet.</p>
					<p>
						An environment exists here because somebody added it. Nothing creates one
						automatically, so this is where every project starts.
					</p>
					<p>
						Errors from this project are still arriving and are still being stored with the
						environment name they were reported from — declaring one here does not change what
						is collected, it makes the name selectable in filters, alerts and access limits.
					</p>
				</div>
			{:else}
				{#if !hasDefaultEnvironment}
					<!-- Criterion 12 / flow `P1`: a visible condition, not an absent marker. -->
					<p class="no-default" data-testid="no-default-set">
						<strong>No default set.</strong> Until one is, an alert rule created for this project
						covers <em>every</em> environment rather than one.
					</p>
				{/if}

				<ul class="environment-list">
					{#each environments as env (env.id)}
						<li class="environment-row" class:hidden-env={env.hidden}>
							<div class="environment-identity">
								{#if editingLabelFor === env.id}
									<input
										class="label-input"
										type="text"
										bind:value={labelDraft}
										aria-label="Label for {env.name}"
									/>
									<button type="button" disabled={busyEnvironmentId === env.id} onclick={() => saveLabel(env)}>
										Save label
									</button>
									<button type="button" class="link-button" onclick={() => (editingLabelFor = null)}>
										Cancel
									</button>
								{:else}
									<span class="environment-label">{env.displayName}</span>
									<code class="environment-name">{env.name}</code>
									{#if env.isDefault}
										<span class="tag tag-default">default</span>
									{/if}
									{#if env.hidden}
										<span class="tag tag-hidden">hidden</span>
									{/if}
								{/if}
							</div>

							<p class="environment-seen">
								First seen {formatSeen(env.firstSeenAt)} · last seen {formatSeen(env.lastSeenAt)}
							</p>

							{#if editingLabelFor !== env.id}
								<div class="environment-actions">
									<button type="button" class="link-button" onclick={() => startLabelEdit(env)}>
										Edit label
									</button>
									{#if !env.isDefault}
										<button
											type="button"
											class="link-button"
											disabled={busyEnvironmentId === env.id}
											onclick={() => patchEnvironment(env, { isDefault: true })}
										>
											Make default
										</button>
									{/if}
									<button
										type="button"
										class="link-button"
										disabled={busyEnvironmentId === env.id}
										onclick={() => patchEnvironment(env, { hidden: !env.hidden })}
									>
										{env.hidden ? 'Unhide' : 'Hide'}
									</button>
								</div>
							{/if}
						</li>
					{/each}
				</ul>

				{#if environmentActionError}
					<p class="section-error" role="status">{environmentActionError}</p>
				{/if}
			{/if}

			<div class="add-environment">
				<label for="new-environment">Add an environment</label>
				<div class="add-row">
					<input
						id="new-environment"
						type="text"
						bind:value={newEnvironmentName}
						placeholder="eu-west-1"
					/>
					<button type="button" disabled={addingEnvironment} onclick={addEnvironment}>
						{addingEnvironment ? 'Adding…' : 'Add'}
					</button>
				</div>
				<p class="hint">
					Use the exact name your services send — lower-case letters, digits, and
					<code>. _ -</code>. It cannot be changed afterwards, because it is what every stored
					issue is filed under; the label above it can.
				</p>
				{#if addEnvironmentError}
					<p class="section-error" role="status">{addEnvironmentError}</p>
				{/if}
			</div>

			<!--
				Task 8's three things that have to be said in WORDS, because each is a control a reader
				would otherwise assume does something it does not.
			-->
			<div class="environment-notes">
				<p>
					<strong>Hiding</strong> takes an environment out of the filter and alert selectors. It
					does not stop its errors arriving, does not delete anything already stored, and is
					<em>not</em> a permission — Environment access below is the permission.
				</p>
				<p>
					<strong>The default</strong> is used for exactly one thing: it is the environment a
					newly created alert rule is scoped to. It does not pre-select any filter anywhere.
				</p>
			</div>
		</section>
	{/if}

	<!--
		sentinel-project-environments tasks 10-11, flows `G1`-`G8`. This is what makes the environment
		boundary settable by a person instead of by a `psql` session.
	-->
	{#if canManageMembers}
		<section class="environment-access">
			<h2>Environment access</h2>
			<p class="subtitle">
				Everyone who can reach this project's issues. By default a person sees every
				environment; limiting somebody hides the others from their issue lists, search,
				notifications and alerts.
			</p>

			{#if !membersLoaded}
				<p class="empty-state">Loading environment access…</p>
			{:else if membersError}
				<p class="section-error" role="status">{membersError}</p>
			{:else if environments.length === 0}
				<!--
					Flow `G6`. A limit names environments, and this project has none declared, so there is
					nothing a limit could be expressed in. An empty multi-select here would read as broken;
					the honest thing is to say what has to happen first and point at it.
				-->
				<div class="registry-empty" data-testid="environment-access-empty">
					<p class="registry-empty-title">
						Access cannot be limited until this project has environments.
					</p>
					<p>
						A limit is a list of environment names, so there has to be a list to choose from
						first. <a href="#environments">Declare one in Environments above</a>, then come
						back — until then everybody here sees everything, which is the same as it has
						always been.
					</p>
				</div>
			{:else if members.length === 0}
				<p class="empty-state">Nobody has access to this project yet.</p>
			{:else}
				<ul class="member-list">
					{#each members as member (member.userId)}
						<li class="member-row">
							<div class="member-identity">
								<span class="member-name">{member.displayName}</span>
								{#if member.orgRole}<span class="tag">{member.orgRole}</span>{/if}
								{#if member.projectRole}<span class="tag">{member.projectRole}</span>{/if}
							</div>

							<!--
								Flow `G1`, decision 31: `environments: null` is UNRESTRICTED and is rendered as
								the stated fact "All environments" — never as a blank cell, which reads as
								missing data rather than as an answer.
							-->
							<p class="member-scope" data-testid="member-scope">
								{#if member.environments === null}
									All environments
								{:else}
									Limited to {member.environments.join(', ')}
								{/if}
							</p>

							{#if member.orgRole === 'owner'}
								<!--
									Flow `G4`. No control at all, and the reason said once: an owner is the person
									who can undo a narrowing, so narrowing them is the one change nobody could
									reverse. The server refuses it with `422` regardless (§4.10).
								-->
								<p class="member-note" data-testid="owner-note">
									An organization owner cannot be limited to specific environments.
								</p>
							{:else if limitingUserId === member.userId}
								<div class="limit-editor">
									<fieldset>
										<legend>Limit {member.displayName} to</legend>
										{#each environments as env (env.id)}
											<label class="limit-option">
												<input
													type="checkbox"
													checked={limitSelection.includes(env.name)}
													onchange={() => toggleLimitSelection(env.name)}
												/>
												{env.displayName}
												{#if env.hidden}<span class="tag tag-hidden">hidden</span>{/if}
											</label>
										{/each}
									</fieldset>
									<!--
										Decision 49: a hidden environment is still grantable — hiding removes a name
										from selectors, not from grants — so every registry row is offered here,
										`hidden` included, and a member may legitimately end up limited to a name
										that appears in no selector on this page.
									-->
									<p class="hint">
										Hidden environments can still be granted: hiding removes a name from
										selectors, not from access.
									</p>
									{#if limitError}
										<p class="section-error" role="status">{limitError}</p>
									{/if}
									<div class="limit-actions">
										<button
											type="button"
											disabled={busyMemberId === member.userId}
											onclick={() => saveLimit(member)}
										>
											Save limit
										</button>
										<button type="button" class="link-button" onclick={() => (limitingUserId = null)}>
											Cancel
										</button>
									</div>
								</div>
							{:else}
								<div class="member-actions">
									<button type="button" class="link-button" onclick={() => startLimit(member)}>
										{member.environments === null ? 'Limit to environments' : 'Change limit'}
									</button>
									{#if member.environments !== null}
										<button
											type="button"
											class="link-button"
											disabled={busyMemberId === member.userId}
											onclick={() => removeLimit(member)}
										>
											Remove limit
										</button>
									{/if}
								</div>
							{/if}
						</li>
					{/each}
				</ul>
			{/if}
		</section>
	{/if}
</div>

<style>
	.settings-page {
		max-width: 40rem;
		margin: 0 auto;
		padding: 1.5rem;
		display: flex;
		flex-direction: column;
		gap: 1.25rem;
		color: var(--text-primary);
	}

	.project-badge {
		display: inline-block;
		font-size: 0.7rem;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		opacity: 0.7;
		margin-bottom: 0.25rem;
	}

	.settings-header h1 {
		margin: 0;
		font-size: 1.25rem;
		font-weight: 700;
	}

	.toast {
		display: flex;
		justify-content: space-between;
		align-items: center;
		padding: 0.75rem 1rem;
		border-radius: var(--radius-sm);
		border: 1px solid var(--border-color);
		font-size: 0.8rem;
	}

	.toast-error {
		border-color: #b91c1c;
		color: #fca5a5;
	}

	.toast-success {
		border-color: #15803d;
		color: #86efac;
	}

	.toast button {
		background: none;
		border: none;
		color: inherit;
		cursor: pointer;
		font-weight: 700;
	}

	.agent-automation,
	.environments,
	.environment-access {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		background: var(--bg-surface);
		padding: 1rem 1.25rem;
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.registry-empty {
		border: 1px dashed var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.875rem 1rem;
		font-size: 0.8rem;
		line-height: 1.5;
		opacity: 0.9;
	}

	.registry-empty p {
		margin: 0 0 0.5rem;
	}

	.registry-empty p:last-child {
		margin-bottom: 0;
	}

	.registry-empty-title {
		font-weight: 600;
	}

	.registry-empty a {
		color: var(--color-primary, #60a5fa);
	}

	.no-default {
		font-size: 0.8rem;
		border-left: 3px solid var(--border-color);
		padding: 0.375rem 0.75rem;
		margin: 0;
	}

	.environment-list,
	.member-list {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
	}

	.environment-row,
	.member-row {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.625rem 0.75rem;
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
	}

	.environment-row.hidden-env {
		opacity: 0.65;
	}

	.environment-identity,
	.member-identity,
	.environment-actions,
	.member-actions,
	.limit-actions,
	.add-row {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.5rem;
	}

	.environment-label,
	.member-name {
		font-weight: 600;
		font-size: 0.85rem;
	}

	.environment-name {
		font-size: 0.75rem;
		opacity: 0.7;
	}

	.environment-seen,
	.member-scope,
	.member-note {
		margin: 0;
		font-size: 0.75rem;
		opacity: 0.75;
	}

	.member-scope {
		opacity: 1;
		font-size: 0.8rem;
	}

	.tag {
		font-size: 0.65rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.05rem 0.35rem;
		opacity: 0.8;
	}

	.link-button {
		background: none;
		border: none;
		padding: 0;
		font-size: 0.75rem;
		color: var(--color-primary, #60a5fa);
		text-decoration: underline;
		cursor: pointer;
	}

	.link-button:disabled {
		opacity: 0.5;
		cursor: default;
	}

	.section-error {
		margin: 0;
		font-size: 0.78rem;
		color: #fca5a5;
	}

	.hint {
		margin: 0;
		font-size: 0.72rem;
		opacity: 0.7;
		line-height: 1.45;
	}

	.environment-notes {
		font-size: 0.75rem;
		line-height: 1.5;
		opacity: 0.8;
		border-top: 1px solid var(--border-color);
		padding-top: 0.75rem;
	}

	.environment-notes p {
		margin: 0 0 0.5rem;
	}

	.environment-notes p:last-child {
		margin-bottom: 0;
	}

	.add-environment {
		display: flex;
		flex-direction: column;
		gap: 0.375rem;
	}

	.limit-editor fieldset {
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.75rem;
		margin: 0 0 0.5rem;
	}

	.limit-editor legend {
		font-size: 0.75rem;
		padding: 0 0.25rem;
	}

	.limit-option {
		display: flex;
		align-items: center;
		gap: 0.375rem;
		font-size: 0.8rem;
		padding: 0.125rem 0;
	}

	.agent-automation h2 {
		margin: 0;
		font-size: 1rem;
		font-weight: 700;
	}

	.agent-automation h3 {
		margin: 0.5rem 0 0;
		font-size: 0.85rem;
		font-weight: 700;
		border-top: 1px dashed var(--border-color);
		padding-top: 0.75rem;
	}

	.subtitle {
		margin: 0;
		font-size: 0.8rem;
		opacity: 0.7;
	}

	.field-row {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		font-size: 0.8rem;
	}

	.toggle-label {
		flex-direction: row;
		align-items: center;
		display: flex;
		gap: 0.5rem;
	}

	.field-grid {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0.75rem;
	}

	.field-row.wide {
		grid-column: 1 / -1;
	}

	.field-row input,
	.field-row select {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.625rem;
		font-size: 0.8rem;
	}

	.empty-state {
		font-size: 0.8rem;
		opacity: 0.7;
		margin: 0;
	}

	.repo-summary {
		font-size: 0.8rem;
		margin: 0;
	}

	.repo-actions {
		display: flex;
		gap: 0.5rem;
	}

	button {
		background: var(--bg-surface);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.5rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
		align-self: flex-start;
	}

	button:hover {
		background: var(--bg-surface-hover);
	}

	button:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	button.danger {
		color: #f87171;
		border-color: #b91c1c;
	}
</style>
