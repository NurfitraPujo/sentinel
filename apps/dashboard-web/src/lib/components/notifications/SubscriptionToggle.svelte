<script lang="ts">
	// Manual Issues M4 (docs/plans/MANUAL_ISSUES_DESIGN.md §8/§10): the manual subscribe/unsubscribe
	// toggle mounted on both issue detail pages ([orgSlug]/reports/[issueId] and
	// [orgSlug]/projects/[projectId]/issues/[issueId]). Talks to
	// `GET/PUT/DELETE /api/issues/[issueId]/subscription`, which always records reason 'manual' on
	// subscribe (see that route's own comment for why an existing stronger reason survives).
	interface Props {
		issueId: string;
		/** Server-loaded initial state, so the button doesn't flash "Subscribe" before its own GET resolves. */
		initialSubscribed?: boolean | null;
	}

	let { issueId, initialSubscribed = null }: Props = $props();

	// svelte-ignore state_referenced_locally -- deliberate: this seeds the initial state once from
	// the prop at mount, exactly like CommentThread's own `$state` initializers; it is not meant to
	// track later prop changes (the caller doesn't change `initialSubscribed` after mount).
	let subscribed = $state<boolean | null>(initialSubscribed);
	let loading = $state(false);
	let error = $state<string | null>(null);

	async function loadState() {
		try {
			const res = await fetch(`/api/issues/${issueId}/subscription`);
			if (!res.ok) return;
			const body = await res.json();
			subscribed = Boolean(body.subscribed);
		} catch {
			// leave whatever state we have -- best-effort, not the primary source of truth
		}
	}

	async function toggle() {
		error = null;
		loading = true;
		const wasSubscribed = subscribed;
		try {
			const res = await fetch(`/api/issues/${issueId}/subscription`, {
				method: wasSubscribed ? 'DELETE' : 'PUT',
			});
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to update subscription (${res.status})`);
			}
			const body = await res.json();
			subscribed = Boolean(body.subscribed);
		} catch (err) {
			error = err instanceof Error ? err.message : 'Failed to update subscription';
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		if (initialSubscribed === null) {
			void loadState();
		}
	});
</script>

<div class="subscription-toggle">
	<button
		type="button"
		class="toggle-btn"
		class:is-subscribed={subscribed === true}
		onclick={toggle}
		disabled={loading || subscribed === null}
	>
		{#if subscribed === null}
			…
		{:else if subscribed}
			🔔 Unsubscribe
		{:else}
			🔕 Subscribe
		{/if}
	</button>
	{#if error}<span class="toggle-error" role="alert">{error}</span>{/if}
</div>

<style>
	.subscription-toggle {
		display: flex;
		align-items: center;
		gap: 0.5rem;
	}

	.toggle-btn {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.toggle-btn:hover {
		background: var(--bg-surface-hover);
	}

	.toggle-btn:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.toggle-btn.is-subscribed {
		border-color: var(--color-primary);
		color: var(--color-primary);
	}

	.toggle-error {
		color: #ef4444;
		font-size: 0.75rem;
	}
</style>
