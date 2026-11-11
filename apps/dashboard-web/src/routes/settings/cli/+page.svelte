<script lang="ts">
	import { enhance } from '$app/forms';
	export let data;
	export let form;

	function day(iso: string | null): string {
		return iso ? new Date(iso).toLocaleDateString() : '—';
	}
</script>

<svelte:head>
	<title>CLI sessions · Sentinel</title>
</svelte:head>

<div class="cli-sessions">
	<h1 class="page-title">CLI sessions</h1>
	<p class="subtitle">
		Keys created by <code>sentinel login</code> on your devices. Each acts as you, can never do more than you can in that
		organization, and expires on its own. Revoke any you no longer use; <code>sentinel logout</code> on the device does the
		same.
	</p>

	{#if form?.error}<p class="msg msg-error" role="alert">{form.error}</p>{/if}
	{#if form?.revoked}<p class="msg msg-success">Session revoked.</p>{/if}

	{#if data.sessions.length === 0}
		<div class="card">
			<p>No CLI sessions yet. Run <code>sentinel login --url {'<this dashboard>'}</code> on your machine.</p>
		</div>
	{:else}
		<table class="card table">
			<thead>
				<tr><th>Device</th><th>Organization</th><th>Key</th><th>Created</th><th>Expires</th><th>Status</th><th></th></tr>
			</thead>
			<tbody>
				{#each data.sessions as s (s.keyId)}
					<tr>
						<td>{s.deviceName}</td>
						<td>{s.organizationName} <span class="muted">({s.organizationSlug})</span></td>
						<td><code>{s.keyPrefix}…</code></td>
						<td>{day(s.createdAt)}</td>
						<td>{day(s.expiresAt)}</td>
						<td><span class="status" class:inactive={s.state !== 'active'}>{s.state}</span></td>
						<td>
							{#if s.state === 'active'}
								<form method="POST" action="?/revoke" use:enhance>
									<input type="hidden" name="keyId" value={s.keyId} />
									<button type="submit" class="btn btn-danger">Revoke</button>
								</form>
							{/if}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</div>

<style>
	.cli-sessions {
		max-width: 60rem;
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

	.subtitle,
	.muted {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.card {
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-lg);
		padding: 1rem 1.25rem;
		color: var(--text-primary);
	}

	.table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.8125rem;
	}

	.table th,
	.table td {
		text-align: left;
		padding: 0.5rem;
		border-bottom: 1px solid var(--border-color);
	}

	.status {
		color: var(--status-resolved-text);
	}

	.status.inactive {
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

	.btn {
		padding: 0.25rem 0.75rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		cursor: pointer;
		border: 1px solid var(--border-color);
		background: transparent;
		color: var(--text-primary);
	}

	.btn-danger {
		color: var(--severity-critical-text);
	}
</style>
