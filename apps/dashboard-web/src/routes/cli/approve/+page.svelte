<script lang="ts">
	import { enhance } from '$app/forms';
	export let data;
	export let form;

	function when(iso: string): string {
		return new Date(iso).toLocaleString();
	}

	$: shownCode = form?.code ?? data.code;

	// The org the key would be for, and the live key of the same device name it would replace there (if any).
	let selectedOrg: string | null = data.signedIn ? data.defaultOrganizationId : null;
	$: replaced =
		data.signedIn && data.request && 'replaces' in data
			? (data.replaces ?? []).find((r) => r.organizationId === selectedOrg)
			: undefined;
</script>

<svelte:head>
	<title>Approve CLI login · Sentinel</title>
</svelte:head>

<div class="approve">
	<h1 class="page-title">Approve a CLI login</h1>

	{#if !data.signedIn}
		<div class="card">
			<p>Sign in to approve the code shown by <code>sentinel login</code>.</p>
			<a class="btn btn-primary" href={data.signInUrl}>Sign in</a>
		</div>
	{:else if form?.decided === 'approved'}
		<div class="card">
			<p class="code" aria-label="Approved code">{shownCode}</p>
			<p class="msg msg-success">Approved. Return to your terminal; <code>sentinel login</code> finishes on its own.</p>
			<p class="muted">You can see and revoke this key under <a href="/settings/cli">Settings → CLI sessions</a>.</p>
		</div>
	{:else if form?.decided === 'denied'}
		<div class="card">
			<p class="code" aria-label="Denied code">{shownCode}</p>
			<p class="msg">Denied. That terminal did not get a key.</p>
		</div>
	{:else}
		<div class="card warning" role="note">
			<strong>Only approve a code you started yourself, in your own terminal, just now.</strong>
			Approving gives that terminal a key that can do whatever you can do in Sentinel. If someone sent you this link or
			code, deny it.
		</div>

		{#if !data.code || data.codeError}
			<form method="GET" class="card stack">
				<label for="code" class="field-label">Code shown in your terminal</label>
				<input
					id="code"
					name="code"
					class="field-input code-input"
					placeholder="BCDF-GHJK"
					autocomplete="off"
					autocapitalize="characters"
					spellcheck="false"
					value={data.code ?? ''}
					required
				/>
				{#if data.codeError}<p class="msg msg-error">{data.codeError}</p>{/if}
				<div><button type="submit" class="btn btn-primary">Continue</button></div>
			</form>
		{:else if data.request && data.request.state !== 'pending'}
			<div class="card">
				<p class="code">{data.code}</p>
				{#if data.request.state === 'expired'}
					<p class="msg msg-error">This code has expired. Run <code>sentinel login</code> again.</p>
				{:else if data.request.state === 'denied'}
					<p class="msg msg-error">This code was denied.</p>
				{:else}
					<p class="msg msg-error">This code was already used. Run <code>sentinel login</code> again.</p>
				{/if}
			</div>
		{:else if data.request}
			<div class="card stack">
				<p class="muted">Check that this matches your terminal exactly:</p>
				<p class="code" aria-label="Code to approve">{data.code}</p>

				<dl class="details">
					<dt>Device</dt><dd>{data.request.deviceName}</dd>
					<dt>Requested from</dt><dd>{data.request.requestedIp || 'unknown address'}</dd>
					<dt>Requested at</dt><dd>{when(data.request.createdAt)} (expires {when(data.request.expiresAt)})</dd>
					<dt>Client</dt><dd>{data.request.client || 'unknown'} {data.request.clientVersion}</dd>
					{#if data.request.requestedProfile}<dt>CLI profile</dt><dd>{data.request.requestedProfile}</dd>{/if}
					<dt>Signed in as</dt><dd>{data.userEmail}</dd>
				</dl>

				{#if data.organizations.length === 0}
					<p class="msg msg-error">You are not a member of any organization, so there is nothing to approve a key for.</p>
				{:else}
					<form method="POST" action="?/approve" use:enhance class="stack">
						<input type="hidden" name="code" value={data.code} />
						<label for="organizationId" class="field-label">The key will be for organization</label>
						<select id="organizationId" name="organizationId" class="field-input" bind:value={selectedOrg}>
							{#each data.organizations as org (org.id)}
								<option value={org.id}>{org.name} ({org.slug}) — you are {org.role}</option>
							{/each}
						</select>
						{#if replaced}
							<p class="msg msg-warning" role="alert" data-testid="replaces-key">
								This replaces your CLI key for <strong>{data.request.deviceName}</strong>
								{#if replaced.createdAt}created {when(replaced.createdAt)}{/if}. That key stops working when you approve.
								If you did not just run <code>sentinel login</code> on that machine, deny this.
							</p>
						{/if}
						<p class="muted">
							The key can do exactly what your role allows in that organization, checked on every request, and
							expires on its own. Removing you from the organization stops it immediately.
						</p>
						<div class="actions">
							<button type="submit" class="btn btn-primary">Approve</button>
							<button type="submit" formaction="?/deny" class="btn btn-secondary">Deny</button>
						</div>
					</form>
				{/if}
			</div>
		{/if}

		{#if form?.error}<p class="msg msg-error" role="alert">{form.error}</p>{/if}
	{/if}
</div>

<style>
	.approve {
		max-width: 40rem;
		margin: 0 auto;
		padding: 1.5rem 1rem;
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.page-title {
		font-size: 1.375rem;
		font-weight: 700;
		color: var(--text-primary);
	}

	.card {
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-lg);
		padding: 1.25rem 1.5rem;
		color: var(--text-primary);
	}

	.warning {
		border-color: var(--severity-high-text, var(--border-color));
		font-size: 0.875rem;
		line-height: 1.5;
	}

	.stack {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.code {
		font-family: var(--font-mono, monospace);
		font-size: 2.25rem;
		font-weight: 700;
		letter-spacing: 0.15em;
		text-align: center;
		margin: 0.25rem 0;
	}

	.code-input {
		font-family: var(--font-mono, monospace);
		font-size: 1.25rem;
		letter-spacing: 0.1em;
		text-transform: uppercase;
	}

	.details {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.375rem 1rem;
		font-size: 0.875rem;
	}

	.details dt {
		color: var(--text-muted);
	}

	.details dd {
		margin: 0;
		word-break: break-word;
	}

	.field-label {
		font-size: 0.8125rem;
		font-weight: 500;
		color: var(--text-primary);
	}

	.field-input {
		display: block;
		width: 100%;
		padding: 0.5rem 0.75rem;
		font-size: 0.875rem;
	}

	.muted {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.msg {
		font-size: 0.875rem;
	}

	.msg-success {
		color: var(--status-resolved-text);
	}

	.msg-error {
		color: var(--severity-critical-text);
	}

	.msg-warning {
		color: var(--severity-high-text, var(--text-primary));
		border-left: 3px solid var(--severity-high-text, var(--border-color));
		padding-left: 0.75rem;
		line-height: 1.5;
	}

	.actions {
		display: flex;
		gap: 0.75rem;
	}

	.btn {
		display: inline-block;
		padding: 0.5rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.875rem;
		font-weight: 500;
		border: none;
		cursor: pointer;
		text-decoration: none;
	}

	.btn-primary {
		background-color: var(--color-primary);
		color: var(--text-primary);
	}

	.btn-primary:hover {
		background-color: var(--color-primary-hover);
	}

	.btn-secondary {
		background-color: transparent;
		border: 1px solid var(--border-color);
		color: var(--text-primary);
	}
</style>
