<script lang="ts">
	import type { RelationType } from '$lib/types/relation-type';

	interface RelatedIssue {
		id: string;
		errorClass: string;
		message: string;
		status: string;
		fingerprint: string;
	}

	interface RelationItem {
		id: string;
		sourceIssueId: string;
		targetIssueId: string;
		relationType: RelationType;
		direction: 'outgoing' | 'incoming';
		// D43: the other issue in the relation, regardless of direction — never necessarily the
		// "target" (for an incoming relation it is the source).
		relatedIssue: RelatedIssue;
	}

	interface Props {
		currentIssueId: string;
		initialRelations?: RelationItem[];
		onStatusChangeRequest?: (status: 'resolved') => void;
	}

	let { currentIssueId, initialRelations = [], onStatusChangeRequest }: Props = $props();

	// Seeded empty, not from `initialRelations` (the declaration referencing a prop directly is what
	// state_referenced_locally flags) -- the $effect below runs immediately on mount and assigns the
	// real initial value, then re-runs whenever `initialRelations` itself changes later. Without the
	// effect, `relations` would only ever reflect what was true the moment this component was
	// created: if the parent's loader data changes without a full remount (e.g. an `invalidateAll()`
	// triggered by the status-change handler this component calls into via onStatusChangeRequest),
	// `relations` would silently drift from what the server now has. Local optimistic updates
	// (link/unlink below) mutate `relations` directly and never touch `initialRelations`, so
	// re-syncing on every actual prop change is safe -- it never clobbers an in-flight local edit,
	// it only fires when NEW data arrives from the server.
	let relations = $state<RelationItem[]>([]);
	$effect(() => {
		relations = initialRelations;
	});
	let searchQuery = $state('');
	let searchResults = $state<RelatedIssue[]>([]);
	let selectedRelationType = $state<RelationType>('linked_to');
	let isSearching = $state(false);
	let isSubmitting = $state(false);
	let errorMessage = $state<string | null>(null);
	let promptResolveTarget = $state<RelatedIssue | null>(null);

	let debounceTimer: ReturnType<typeof setTimeout>;

	function handleSearchInput(e: Event) {
		const val = (e.target as HTMLInputElement).value;
		searchQuery = val;
		clearTimeout(debounceTimer);

		if (!val.trim()) {
			searchResults = [];
			return;
		}

		debounceTimer = setTimeout(async () => {
			isSearching = true;
			try {
				const res = await fetch(`/api/issues/search?q=${encodeURIComponent(val)}&issueId=${currentIssueId}`);
				if (res.ok) {
					const data = await res.json();
					searchResults = data.issues || [];
				}
			} catch (err) {
				console.error('Failed to search issues:', err);
			} finally {
				isSearching = false;
			}
		}, 250);
	}

	async function linkIssue(target: RelatedIssue) {
		isSubmitting = true;
		errorMessage = null;

		try {
			const res = await fetch(`/api/issues/${currentIssueId}/relations`, {
				method: 'POST',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					targetIssueId: target.id,
					relationType: selectedRelationType,
				}),
			});

			if (!res.ok) {
				const errData = await res.json().catch(() => ({}));
				throw new Error(errData.message || 'Failed to link issue');
			}

			const createdRelation = await res.json();

			relations = [
				...relations,
				{
					...createdRelation,
					direction: 'outgoing',
					relatedIssue: target,
				},
			];

			searchQuery = '';
			searchResults = [];

			// Smart duplicate resolution helper
			if (selectedRelationType === 'duplicate_of' && target.status === 'resolved') {
				promptResolveTarget = target;
			}
		} catch (err: any) {
			errorMessage = err.message || 'Error linking issue';
		} finally {
			isSubmitting = false;
		}
	}

	// D11: the DELETE handler at /api/issues/[issueId]/relations always treats params.issueId as the
	// relation's SOURCE and the body's targetIssueId as the relation's TARGET, matching the stored
	// row by (source, target, relationType) exactly (see deleteIssueRelation). For an INCOMING
	// relation the stored row is (source=rel.sourceIssueId, target=currentIssueId) — always calling
	// the endpoint at /api/issues/{currentIssueId}/relations put currentIssueId in the source slot
	// no matter what targetIssueId was sent, which never matches an incoming row and always 404s.
	//
	// Rather than change the endpoint's contract (out of scope here — see D11's note), call it on
	// whichever issue is actually the relation's source, with the other issue as targetIssueId. That
	// reproduces the exact (source, target, relationType) triple already stored, for both directions,
	// with no directional reasoning left in this function beyond picking the right two ids.
	async function unlinkIssue(rel: RelationItem) {
		const endpointIssueId = rel.direction === 'outgoing' ? currentIssueId : rel.sourceIssueId;
		const targetId = rel.direction === 'outgoing' ? rel.targetIssueId : currentIssueId;

		// Optimistic removal
		const prevRelations = [...relations];
		relations = relations.filter((r) => r.id !== rel.id);

		try {
			const res = await fetch(`/api/issues/${endpointIssueId}/relations`, {
				method: 'DELETE',
				headers: { 'Content-Type': 'application/json' },
				body: JSON.stringify({
					targetIssueId: targetId,
					relationType: rel.relationType,
				}),
			});

			if (!res.ok) {
				throw new Error('Failed to unlink issue');
			}
		} catch (err: any) {
			relations = prevRelations; // Rollback
			errorMessage = err.message || 'Failed to unlink issue';
		}
	}

	let duplicates = $derived(relations.filter((r) => r.relationType === 'duplicate_of'));
	let causes = $derived(relations.filter((r) => r.relationType === 'caused_by'));
	let linked = $derived(relations.filter((r) => r.relationType === 'linked_to'));

	function handleResolvePromptConfirm() {
		if (onStatusChangeRequest) {
			onStatusChangeRequest('resolved');
		}
		promptResolveTarget = null;
	}
</script>

<div class="issue-relations-panel">
	<div class="panel-header">
		<h3 class="panel-title">Issue Relations</h3>
		<span class="count-badge">{relations.length}</span>
	</div>

	{#if errorMessage}
		<div class="error-banner">
			<span>{errorMessage}</span>
			<button class="close-err" onclick={() => (errorMessage = null)} aria-label="Dismiss error">×</button>
		</div>
	{/if}

	{#if promptResolveTarget}
		<div class="resolve-prompt">
			<p class="prompt-text">
				Linked as duplicate of resolved issue <strong class="mono">{promptResolveTarget.id}</strong>. Mark this issue as resolved too?
			</p>
			<div class="prompt-actions">
				<button class="btn-resolve-confirm" onclick={handleResolvePromptConfirm}>Mark Resolved</button>
				<button class="btn-resolve-dismiss" onclick={() => (promptResolveTarget = null)}>Dismiss</button>
			</div>
		</div>
	{/if}

	<!-- Add Relation Search Bar -->
	<div class="add-relation-box">
		<div class="input-row">
			<select bind:value={selectedRelationType} class="relation-select" aria-label="Relation type">
				<option value="linked_to">Relates to</option>
				<option value="caused_by">Caused by</option>
				<option value="duplicate_of">Duplicate of</option>
			</select>
			<div class="search-input-wrapper">
				<input
					type="text"
					value={searchQuery}
					oninput={handleSearchInput}
					placeholder="Search issue ID or title..."
					class="search-input"
					aria-label="Search issue to link"
				/>
				{#if isSearching}
					<span class="spinner"></span>
				{/if}
			</div>
		</div>

		<!-- Autocomplete Dropdown -->
		{#if searchResults.length > 0}
			<ul class="autocomplete-dropdown">
				{#each searchResults as item}
					<li>
						<button type="button" class="autocomplete-item" onclick={() => linkIssue(item)} disabled={isSubmitting}>
							<div class="item-main">
								<span class="item-class">{item.errorClass}</span>
								<span class="item-msg">{item.message}</span>
							</div>
							<div class="item-meta">
								<span class="mono-id">{item.id}</span>
								<span class="status-tag {item.status}">{item.status}</span>
							</div>
						</button>
					</li>
				{/each}
			</ul>
		{/if}
	</div>

	<!-- Relations Lists -->
	<div class="relations-groups">
		<!-- Duplicates -->
		{#if duplicates.length > 0}
			<div class="group-section">
				<h4 class="group-title">
					<span class="group-name">Duplicates</span>
					<span class="badge">{duplicates.length}</span>
				</h4>
				<ul class="relation-list">
					{#each duplicates as rel}
						<li class="relation-card">
							<div class="card-content">
								<a href="/issues/{rel.relatedIssue.id}" class="issue-link">
									<span class="error-class">{rel.relatedIssue.errorClass}</span>
									<span class="issue-msg">{rel.relatedIssue.message}</span>
								</a>
								<div class="card-meta">
									<span class="mono-id">{rel.relatedIssue.id}</span>
									<span class="status-tag {rel.relatedIssue.status}">{rel.relatedIssue.status}</span>
									{#if rel.direction === 'incoming'}
										<span class="dir-tag">incoming</span>
									{/if}
								</div>
							</div>
							<button class="btn-unlink" onclick={() => unlinkIssue(rel)} title="Unlink issue" aria-label="Unlink issue">
								&times;
							</button>
						</li>
					{/each}
				</ul>
			</div>
		{/if}

		<!-- Causes & Blockers -->
		{#if causes.length > 0}
			<div class="group-section">
				<h4 class="group-title">
					<span class="group-name">Causes & Blockers</span>
					<span class="badge">{causes.length}</span>
				</h4>
				<ul class="relation-list">
					{#each causes as rel}
						<li class="relation-card">
							<div class="card-content">
								<a href="/issues/{rel.relatedIssue.id}" class="issue-link">
									<span class="error-class">{rel.relatedIssue.errorClass}</span>
									<span class="issue-msg">{rel.relatedIssue.message}</span>
								</a>
								<div class="card-meta">
									<span class="mono-id">{rel.relatedIssue.id}</span>
									<span class="status-tag {rel.relatedIssue.status}">{rel.relatedIssue.status}</span>
									{#if rel.direction === 'incoming'}
										<span class="dir-tag">incoming</span>
									{/if}
								</div>
							</div>
							<button class="btn-unlink" onclick={() => unlinkIssue(rel)} title="Unlink issue" aria-label="Unlink issue">
								&times;
							</button>
						</li>
					{/each}
				</ul>
			</div>
		{/if}

		<!-- Related Issues -->
		{#if linked.length > 0}
			<div class="group-section">
				<h4 class="group-title">
					<span class="group-name">Related Issues</span>
					<span class="badge">{linked.length}</span>
				</h4>
				<ul class="relation-list">
					{#each linked as rel}
						<li class="relation-card">
							<div class="card-content">
								<a href="/issues/{rel.relatedIssue.id}" class="issue-link">
									<span class="error-class">{rel.relatedIssue.errorClass}</span>
									<span class="issue-msg">{rel.relatedIssue.message}</span>
								</a>
								<div class="card-meta">
									<span class="mono-id">{rel.relatedIssue.id}</span>
									<span class="status-tag {rel.relatedIssue.status}">{rel.relatedIssue.status}</span>
									{#if rel.direction === 'incoming'}
										<span class="dir-tag">incoming</span>
									{/if}
								</div>
							</div>
							<button class="btn-unlink" onclick={() => unlinkIssue(rel)} title="Unlink issue" aria-label="Unlink issue">
								&times;
							</button>
						</li>
					{/each}
				</ul>
			</div>
		{/if}

		{#if relations.length === 0}
			<div class="empty-state">
				<p>No linked issues or duplicates.</p>
			</div>
		{/if}
	</div>
</div>

<style>
	.issue-relations-panel {
		background: var(--bg-surface, #1e293b);
		border: 1px solid var(--border-color, #334155);
		border-radius: var(--radius-md, 6px);
		padding: 1rem;
		margin-bottom: 1.5rem;
		color: var(--text-primary, #f8fafc);
	}

	.panel-header {
		display: flex;
		align-items: center;
		justify-content: space-between;
		margin-bottom: 0.875rem;
	}

	.panel-title {
		font-size: 0.875rem;
		font-weight: 600;
		text-transform: uppercase;
		letter-spacing: 0.05em;
		color: var(--text-muted, #94a3b8);
		font-family: var(--font-mono, 'JetBrains Mono', monospace);
		margin: 0;
	}

	.count-badge {
		font-size: 0.75rem;
		font-family: var(--font-mono, monospace);
		background: var(--bg-root, #0f172a);
		color: var(--text-muted, #94a3b8);
		border: 1px solid var(--border-color, #334155);
		padding: 0.125rem 0.5rem;
		border-radius: 4px;
	}

	.error-banner {
		display: flex;
		align-items: center;
		justify-content: space-between;
		background: rgba(239, 68, 68, 0.15);
		border: 1px solid rgba(239, 68, 68, 0.3);
		color: #ef4444;
		padding: 0.5rem 0.75rem;
		border-radius: 4px;
		font-size: 0.75rem;
		margin-bottom: 0.75rem;
	}

	.close-err {
		background: none;
		border: none;
		color: inherit;
		cursor: pointer;
		font-size: 1rem;
	}

	.resolve-prompt {
		background: rgba(59, 130, 246, 0.1);
		border: 1px solid rgba(59, 130, 246, 0.3);
		border-radius: 4px;
		padding: 0.625rem;
		margin-bottom: 0.75rem;
	}

	.prompt-text {
		font-size: 0.75rem;
		color: #f8fafc;
		margin: 0 0 0.5rem 0;
	}

	.prompt-actions {
		display: flex;
		gap: 0.5rem;
	}

	.btn-resolve-confirm {
		background: #3b82f6;
		color: #fff;
		border: none;
		padding: 0.25rem 0.5rem;
		border-radius: 4px;
		font-size: 0.75rem;
		font-weight: 500;
		cursor: pointer;
	}

	.btn-resolve-dismiss {
		background: transparent;
		color: #94a3b8;
		border: 1px solid #334155;
		padding: 0.25rem 0.5rem;
		border-radius: 4px;
		font-size: 0.75rem;
		cursor: pointer;
	}

	.add-relation-box {
		position: relative;
		margin-bottom: 1rem;
	}

	.input-row {
		display: flex;
		gap: 0.5rem;
	}

	.relation-select {
		background: var(--bg-root, #0f172a);
		color: var(--text-primary, #f8fafc);
		border: 1px solid var(--border-color, #334155);
		border-radius: 4px;
		font-size: 0.75rem;
		padding: 0.375rem 0.5rem;
		outline: none;
	}

	.search-input-wrapper {
		position: relative;
		flex: 1;
	}

	.search-input {
		width: 100%;
		background: var(--bg-root, #0f172a);
		color: var(--text-primary, #f8fafc);
		border: 1px solid var(--border-color, #334155);
		border-radius: 4px;
		font-size: 0.75rem;
		padding: 0.375rem 0.625rem;
		outline: none;
	}

	.search-input:focus {
		border-color: #3b82f6;
	}

	.spinner {
		position: absolute;
		right: 8px;
		top: 50%;
		transform: translateY(-50%);
		width: 12px;
		height: 12px;
		border: 2px solid #334155;
		border-top-color: #3b82f6;
		border-radius: 50%;
		animation: spin 0.6s linear infinite;
	}

	@keyframes spin {
		to { transform: translateY(-50%) rotate(360deg); }
	}

	.autocomplete-dropdown {
		position: absolute;
		top: 100%;
		left: 0;
		right: 0;
		z-index: 20;
		background: var(--bg-surface, #1e293b);
		border: 1px solid var(--border-color, #334155);
		border-radius: 4px;
		margin-top: 4px;
		max-height: 200px;
		overflow-y: auto;
		list-style: none;
		padding: 0;
		box-shadow: 0 4px 12px rgba(0, 0, 0, 0.4);
	}

	.autocomplete-item {
		width: 100%;
		text-align: left;
		background: none;
		border: none;
		border-bottom: 1px solid rgba(51, 65, 85, 0.5);
		padding: 0.5rem 0.625rem;
		cursor: pointer;
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
	}

	.autocomplete-item:hover {
		background: var(--border-color, #334155);
	}

	.item-class {
		font-size: 0.75rem;
		font-weight: 600;
		color: #ef4444;
		font-family: var(--font-mono, monospace);
	}

	.item-msg {
		font-size: 0.75rem;
		color: #f8fafc;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.item-meta {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.6875rem;
	}

	.mono-id {
		font-family: var(--font-mono, monospace);
		color: #94a3b8;
	}

	.status-tag {
		font-family: var(--font-mono, monospace);
		font-size: 0.625rem;
		text-transform: uppercase;
		padding: 1px 4px;
		border-radius: 2px;
	}

	.status-tag.open, .status-tag.unresolved { background: rgba(239, 68, 68, 0.15); color: #ef4444; }
	.status-tag.resolved { background: rgba(16, 185, 129, 0.15); color: #10b981; }
	.status-tag.ignored { background: rgba(148, 163, 184, 0.15); color: #94a3b8; }

	.dir-tag {
		font-size: 0.625rem;
		font-family: var(--font-mono, monospace);
		color: #3b82f6;
		background: rgba(59, 130, 246, 0.1);
		padding: 1px 4px;
		border-radius: 2px;
	}

	.group-section {
		margin-bottom: 0.875rem;
	}

	.group-title {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.75rem;
		font-weight: 600;
		color: var(--text-muted, #94a3b8);
		margin: 0 0 0.5rem 0;
	}

	.badge {
		font-size: 0.625rem;
		font-family: var(--font-mono, monospace);
		background: var(--bg-root, #0f172a);
		color: var(--text-muted, #94a3b8);
		padding: 1px 5px;
		border-radius: 10px;
		border: 1px solid var(--border-color, #334155);
	}

	.relation-list {
		list-style: none;
		padding: 0;
		margin: 0;
		display: flex;
		flex-direction: column;
		gap: 0.375rem;
	}

	.relation-card {
		display: flex;
		align-items: center;
		justify-content: space-between;
		background: var(--bg-root, #0f172a);
		border: 1px solid var(--border-color, #334155);
		border-radius: 4px;
		padding: 0.5rem 0.625rem;
	}

	.card-content {
		display: flex;
		flex-direction: column;
		gap: 0.125rem;
		overflow: hidden;
	}

	.issue-link {
		text-decoration: none;
		display: flex;
		align-items: center;
		gap: 0.5rem;
		overflow: hidden;
	}

	.issue-link:hover .issue-msg {
		text-decoration: underline;
	}

	.error-class {
		font-size: 0.75rem;
		font-weight: 600;
		color: #ef4444;
		font-family: var(--font-mono, monospace);
	}

	.issue-msg {
		font-size: 0.75rem;
		color: #f8fafc;
		white-space: nowrap;
		overflow: hidden;
		text-overflow: ellipsis;
	}

	.card-meta {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		font-size: 0.6875rem;
	}

	.btn-unlink {
		background: none;
		border: none;
		color: #94a3b8;
		font-size: 1.125rem;
		line-height: 1;
		cursor: pointer;
		padding: 0.125rem 0.375rem;
		border-radius: 2px;
	}

	.btn-unlink:hover {
		color: #ef4444;
		background: rgba(239, 68, 68, 0.1);
	}

	.empty-state {
		text-align: center;
		padding: 0.75rem 0;
		color: var(--text-muted, #94a3b8);
		font-size: 0.75rem;
	}
</style>
