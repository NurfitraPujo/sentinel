<script lang="ts">
	import type { PageData } from './$types';

	// Manual Issues M1 (docs/plans/MANUAL_ISSUES_DESIGN.md §10): tabs All / My reports / Claimed by
	// me / Unclaimed / Needs input / Triage. Columns: reporter, severity, claimant (user/agent
	// badge), comment count, linked issues, waiting badge, status. Comment count and linked-issue
	// count are M3/relation-count features not wired into `listReports` yet, so those two columns
	// render "–" for now rather than a fabricated number -- honest about what M1 actually delivers.
	let { data }: { data: PageData } = $props();

	const TABS: Array<{ id: string; label: string }> = [
		{ id: 'all', label: 'All' },
		{ id: 'mine', label: 'My reports' },
		{ id: 'claimed-by-me', label: 'Claimed by me' },
		{ id: 'unclaimed', label: 'Unclaimed' },
		{ id: 'needs-input', label: 'Needs input' },
		{ id: 'triage', label: 'Triage' },
	];

	function tabHref(tabId: string): string {
		return `/${data.orgSlug}/reports?tab=${tabId}`;
	}

	function formatDate(value: string | Date | null): string {
		if (!value) return 'unknown';
		const date = value instanceof Date ? value : new Date(value);
		if (Number.isNaN(date.getTime())) return 'unknown';
		return date.toLocaleDateString();
	}

	function claimantLabel(row: (typeof data.reports)[number]): string {
		if (!row.issue.assignedTo) return 'Unclaimed';
		return row.issue.assignedTo;
	}

	function claimantIcon(row: (typeof data.reports)[number]): string {
		if (!row.issue.assignedTo) return '—';
		return row.issue.assigneeType === 'agent' ? '\u{1F916}' : '\u{1F464}';
	}
</script>

<div class="reports-page">
	<div class="page-header">
		<div>
			<h1 class="page-title">Reports</h1>
			<p class="page-subtitle">User-reported issues, separate from the error dashboard.</p>
		</div>
		<a href="/{data.orgSlug}/reports/new" class="btn-new-report">New report</a>
	</div>

	<nav class="tabs" aria-label="Report filters">
		{#each TABS as t}
			<a href={tabHref(t.id)} class="tab" class:active={data.tab === t.id} aria-current={data.tab === t.id ? 'page' : undefined}>
				{t.label}
			</a>
		{/each}
	</nav>

	<div class="reports-table-wrap">
		<table class="reports-table">
			<thead>
				<tr>
					<th scope="col">Title</th>
					<th scope="col">Project</th>
					<th scope="col">Reporter</th>
					<th scope="col">Severity</th>
					<th scope="col">Claimant</th>
					<th scope="col">Comments</th>
					<th scope="col">Linked</th>
					<th scope="col">Waiting</th>
					<th scope="col">Status</th>
				</tr>
			</thead>
			<tbody>
				{#each data.reports as row (row.issue.id)}
					<tr>
						<td>
							<a href="/{data.orgSlug}/reports/{row.issue.id}" class="report-title-link">
								{row.issue.message}
							</a>
							<div class="report-meta">{formatDate(row.issue.firstSeen)}</div>
						</td>
						<td>
							{row.projectName}
							{#if row.projectIsInbox}
								<span class="inbox-tag">Triage</span>
							{/if}
						</td>
						<td>{row.reporterName ?? row.reporterEmail ?? 'unknown'}</td>
						<td><span class="severity-tag severity-{row.report.severity}">{row.report.severity}</span></td>
						<td>
							<span class="claimant" title={claimantLabel(row)}>
								<span aria-hidden="true">{claimantIcon(row)}</span>
								{claimantLabel(row)}
							</span>
						</td>
						<td class="dim-cell">–</td>
						<td class="dim-cell">–</td>
						<td>
							{#if row.issue.waitingOn}
								<span class="waiting-badge">waiting on {row.issue.waitingOn}</span>
							{/if}
						</td>
						<td><span class="status-tag status-{row.issue.status}">{row.issue.status}</span></td>
					</tr>
				{/each}

				{#if data.reports.length === 0}
					<tr>
						<td colspan="9" class="empty-row">No reports in this view.</td>
					</tr>
				{/if}
			</tbody>
		</table>
	</div>
</div>

<style>
	.reports-page {
		max-width: 80rem;
		margin: 0 auto;
	}

	.page-header {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		margin-bottom: 1.25rem;
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

	.btn-new-report {
		background: var(--color-primary);
		color: #fff;
		text-decoration: none;
		padding: 0.5rem 0.875rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		font-weight: 600;
	}

	.btn-new-report:hover {
		background: var(--color-primary-hover);
	}

	.tabs {
		display: flex;
		gap: 0.25rem;
		border-bottom: 1px solid var(--border-color);
		margin-bottom: 1rem;
		flex-wrap: wrap;
	}

	.tab {
		text-decoration: none;
		color: var(--text-muted);
		font-size: 0.8125rem;
		font-weight: 500;
		padding: 0.5rem 0.75rem;
		border-bottom: 2px solid transparent;
	}

	.tab:hover {
		color: var(--text-primary);
	}

	.tab.active {
		color: var(--text-primary);
		border-bottom-color: var(--color-primary);
	}

	.reports-table-wrap {
		background: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-md);
		overflow-x: auto;
	}

	.reports-table {
		width: 100%;
		border-collapse: collapse;
		font-size: 0.8125rem;
	}

	.reports-table thead tr {
		background: var(--bg-root);
		border-bottom: 1px solid var(--border-color);
	}

	.reports-table th {
		text-align: left;
		padding: 0.625rem 0.875rem;
		font-size: 0.6875rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--text-subtle);
		font-weight: 600;
	}

	.reports-table td {
		padding: 0.625rem 0.875rem;
		border-bottom: 1px solid var(--border-color-muted);
		color: var(--text-primary);
		vertical-align: top;
	}

	.report-title-link {
		color: var(--text-primary);
		text-decoration: none;
		font-weight: 500;
	}

	.report-title-link:hover {
		text-decoration: underline;
	}

	.report-meta {
		font-size: 0.6875rem;
		color: var(--text-subtle);
		margin-top: 0.125rem;
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

	.claimant {
		display: inline-flex;
		align-items: center;
		gap: 0.25rem;
		font-family: var(--font-mono);
		font-size: 0.75rem;
	}

	.dim-cell {
		color: var(--text-subtle);
	}

	.waiting-badge {
		font-size: 0.6875rem;
		background: rgba(245, 158, 11, 0.15);
		color: #f59e0b;
		border-radius: 3px;
		padding: 0.125rem 0.375rem;
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

	.empty-row {
		text-align: center;
		padding: 2rem 1rem;
		color: var(--text-subtle);
	}
</style>
