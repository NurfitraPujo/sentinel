<script lang="ts">
	import Markdown from '$lib/components/Markdown.svelte';
	import IssueRelations from '$lib/components/issues/IssueRelations.svelte';
	import IssueTimeline from '$lib/components/issues/IssueTimeline.svelte';
	import AttachmentList from '$lib/components/attachments/AttachmentList.svelte';
	import { filterKnownRelationTypes } from '$lib/types/relation-type';
	import type { PageData } from './$types';

	let { data }: { data: PageData } = $props();

	let activeTab = $state<'linked' | 'activity'>('linked');

	let relationsForPanel = $derived(filterKnownRelationTypes(data.relations || []));

	let claiming = $state(false);
	let claimError = $state<string | null>(null);
	let moveProjectId = $state('');
	let moving = $state(false);
	let moveError = $state<string | null>(null);

	function isClaimedByMe(): boolean {
		return data.detail.issue.assignedTo === data.userId && data.detail.issue.assigneeType === 'user';
	}

	async function handleClaim() {
		claimError = null;
		claiming = true;
		try {
			const res = await fetch(`/api/issues/${data.detail.issue.id}/claim`, { method: 'POST' });
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to claim (${res.status})`);
			}
			window.location.reload();
		} catch (err) {
			claimError = err instanceof Error ? err.message : 'Failed to claim';
		} finally {
			claiming = false;
		}
	}

	async function handleRelease() {
		claimError = null;
		claiming = true;
		try {
			const res = await fetch(`/api/issues/${data.detail.issue.id}/claim`, { method: 'DELETE' });
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to release (${res.status})`);
			}
			window.location.reload();
		} catch (err) {
			claimError = err instanceof Error ? err.message : 'Failed to release';
		} finally {
			claiming = false;
		}
	}

	async function handleMove() {
		if (!moveProjectId) return;
		moveError = null;
		moving = true;
		try {
			const res = await fetch(`/api/issues/${data.detail.issue.id}/move`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({ projectId: moveProjectId }),
			});
			if (!res.ok) {
				const body = await res.json().catch(() => ({}));
				throw new Error(body.message || `Failed to move (${res.status})`);
			}
			window.location.reload();
		} catch (err) {
			moveError = err instanceof Error ? err.message : 'Failed to move report';
		} finally {
			moving = false;
		}
	}
</script>

<div class="report-detail-page">
	<div class="report-header">
		<div>
			<h1 class="report-title">{data.detail.issue.message}</h1>
			<p class="report-meta">
				Reported by {data.detail.reporterName ?? data.detail.reporterEmail ?? 'unknown'} in
				<strong>{data.detail.projectName}</strong>
				{#if data.detail.projectIsInbox}<span class="inbox-tag">Triage</span>{/if}
			</p>
		</div>
		<div class="header-badges">
			<span class="severity-tag severity-{data.detail.report.severity}">{data.detail.report.severity}</span>
			<span class="status-tag status-{data.detail.issue.status}">{data.detail.issue.status}</span>
			{#if data.detail.issue.waitingOn}
				<span class="waiting-badge">waiting on {data.detail.issue.waitingOn}</span>
			{/if}
		</div>
	</div>

	{#if claimError}
		<div class="error-banner" role="alert">{claimError}</div>
	{/if}

	<div class="claim-box">
		{#if data.detail.issue.assignedTo}
			<span class="claim-status">
				Claimed by
				<strong>{data.detail.issue.assigneeType === 'agent' ? '\u{1F916}' : '\u{1F464}'} {data.detail.issue.assignedTo}</strong>
			</span>
			{#if isClaimedByMe() || data.canWrite}
				<button type="button" class="btn-secondary" onclick={handleRelease} disabled={claiming}>
					{claiming ? 'Releasing…' : 'Release claim'}
				</button>
			{/if}
		{:else}
			<span class="claim-status">Unclaimed</span>
			{#if data.canWrite}
				<button type="button" class="btn-primary" onclick={handleClaim} disabled={claiming}>
					{claiming ? 'Claiming…' : 'Claim'}
				</button>
			{/if}
		{/if}
	</div>

	<div class="report-body-panel">
		<Markdown source={data.detail.report.bodyMd} />
	</div>

	<div class="attachments-panel">
		<h2 class="section-heading">Attachments</h2>
		<AttachmentList attachments={data.attachments} />
	</div>

	{#if data.canWrite}
		<div class="move-box">
			<span class="move-label">Move to project</span>
			<select bind:value={moveProjectId} class="move-select">
				<option value="">Select a project…</option>
				{#each data.projects as project}
					{#if project.id !== data.detail.projectId}
						<option value={project.id}>{project.name}{project.isInbox ? ' (Triage)' : ''}</option>
					{/if}
				{/each}
			</select>
			<button type="button" class="btn-secondary" onclick={handleMove} disabled={!moveProjectId || moving}>
				{moving ? 'Moving…' : 'Move'}
			</button>
			{#if moveError}<span class="move-error">{moveError}</span>{/if}
		</div>
	{/if}

	<div class="tabs" role="tablist" aria-label="Report detail tabs">
		<button type="button" class="tab-btn" class:active={activeTab === 'linked'} onclick={() => (activeTab = 'linked')}>
			Linked issues
		</button>
		<button type="button" class="tab-btn" class:active={activeTab === 'activity'} onclick={() => (activeTab = 'activity')}>
			Activity
		</button>
	</div>

	{#if activeTab === 'linked'}
		<IssueRelations currentIssueId={data.detail.issue.id} initialRelations={relationsForPanel} />
	{:else}
		<IssueTimeline activity={data.activity} />
	{/if}
</div>

<style>
	.report-detail-page {
		max-width: 56rem;
		margin: 0 auto;
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.report-header {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		gap: 1rem;
		flex-wrap: wrap;
	}

	.report-title {
		font-size: 1.25rem;
		font-weight: 700;
		color: var(--text-primary);
		margin: 0 0 0.25rem 0;
	}

	.report-meta {
		font-size: 0.8125rem;
		color: var(--text-muted);
		margin: 0;
	}

	.inbox-tag {
		margin-left: 0.375rem;
		font-size: 0.625rem;
		text-transform: uppercase;
		color: var(--text-muted);
		border: 1px solid var(--border-color);
		border-radius: 3px;
		padding: 0 0.25rem;
	}

	.header-badges {
		display: flex;
		gap: 0.5rem;
		align-items: center;
		flex-wrap: wrap;
	}

	.error-banner {
		background: rgba(239, 68, 68, 0.15);
		border: 1px solid rgba(239, 68, 68, 0.3);
		color: #ef4444;
		padding: 0.5rem 0.75rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
	}

	.claim-box {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 0.75rem 1rem;
		font-size: 0.8125rem;
	}

	.claim-status {
		color: var(--text-muted);
	}

	.report-body-panel {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1rem;
	}

	.attachments-panel {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		padding: 1rem;
	}

	.section-heading {
		font-size: 0.8125rem;
		font-weight: 600;
		color: var(--text-muted);
		margin: 0 0 0.75rem 0;
		text-transform: uppercase;
		letter-spacing: 0.02em;
	}

	.move-box {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
		font-size: 0.8125rem;
	}

	.move-label {
		color: var(--text-muted);
	}

	.move-select {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.5rem;
		font-size: 0.8125rem;
	}

	.move-error {
		color: #ef4444;
		font-size: 0.75rem;
	}

	.btn-primary,
	.btn-secondary {
		border: none;
		border-radius: var(--radius-sm);
		padding: 0.375rem 0.75rem;
		font-size: 0.75rem;
		font-weight: 600;
		cursor: pointer;
	}

	.btn-primary {
		background: var(--color-primary);
		color: #fff;
	}

	.btn-primary:hover {
		background: var(--color-primary-hover);
	}

	.btn-secondary {
		background: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
	}

	.btn-secondary:hover {
		background: var(--bg-surface-hover);
	}

	.btn-primary:disabled,
	.btn-secondary:disabled {
		opacity: 0.6;
		cursor: not-allowed;
	}

	.tabs {
		display: flex;
		gap: 0.25rem;
		border-bottom: 1px solid var(--border-color);
	}

	.tab-btn {
		background: none;
		border: none;
		color: var(--text-muted);
		font-size: 0.8125rem;
		font-weight: 500;
		padding: 0.5rem 0.75rem;
		border-bottom: 2px solid transparent;
		cursor: pointer;
	}

	.tab-btn:hover {
		color: var(--text-primary);
	}

	.tab-btn.active {
		color: var(--text-primary);
		border-bottom-color: var(--color-primary);
	}

	.severity-tag,
	.status-tag {
		font-size: 0.6875rem;
		text-transform: capitalize;
		border-radius: 3px;
		padding: 0.125rem 0.4rem;
		font-weight: 600;
	}

	.severity-low { background: var(--status-resolved-bg); color: var(--status-resolved-text); }
	.severity-medium { background: var(--status-open-bg); color: var(--status-open-text); }
	.severity-high { background: rgba(249, 115, 22, 0.15); color: #f97316; }
	.severity-critical { background: var(--severity-critical-bg); color: var(--severity-critical-text); }

	.status-unresolved { background: var(--status-open-bg); color: var(--status-open-text); }
	.status-resolved { background: var(--status-resolved-bg); color: var(--status-resolved-text); }
	.status-ignored { background: var(--status-ignored-bg); color: var(--status-ignored-text); }

	.waiting-badge {
		font-size: 0.6875rem;
		background: rgba(245, 158, 11, 0.15);
		color: #f59e0b;
		border-radius: 3px;
		padding: 0.125rem 0.375rem;
	}
</style>
