<script lang="ts">
	import type { PageData } from './$types';

	export let data: PageData;

	const orgSections = [
		{ href: 'settings/keys', title: 'API keys', desc: 'Issue, rotate and revoke organization-wide keys.' },
		{ href: 'settings/members', title: 'Members', desc: 'Who belongs to this organization, and their roles.' },
		{ href: 'settings/agents', title: 'Agents', desc: 'Agent identities and their webhooks.' },
		{ href: 'settings/on-call', title: 'On-call', desc: 'Rotation and overrides for escalations.' },
		{ href: 'settings/observability', title: 'Observability', desc: 'Tracing and telemetry for this organization.' },
	];

	// The global sections are listed here too. They are a different scope — instance and account
	// settings, not this organization's — but the header only has room for one "Settings", and a
	// user who clicks it should not lose the ability to reach the other half.
	const globalSections = [
		{ href: '/settings/alerts', title: 'Alerts', desc: 'Notification channels and thresholds.' },
		{ href: '/settings/auth', title: 'Auth', desc: 'Sign-in configuration.' },
		{ href: '/settings/observability', title: 'Observability', desc: 'Instance-wide telemetry.' },
	];
</script>

<div class="settings-index">
	<header class="page-header">
		<h1 class="page-title">{data.org.name} settings</h1>
		<p class="page-subtitle">Everything scoped to this organization.</p>
	</header>

	<div class="card-grid">
		{#each orgSections as section}
			<a class="card" href="/{data.org.slug}/{section.href}">
				<span class="card-title">{section.title}</span>
				<span class="card-desc">{section.desc}</span>
			</a>
		{/each}
	</div>

	<header class="page-header section-gap">
		<h2 class="section-title">Projects</h2>
		<p class="page-subtitle">
			A project's API key — the one an SDK reports with — lives in the project's own settings.
		</p>
	</header>

	{#if data.projects.length === 0}
		<p class="empty">No projects yet.</p>
	{:else}
		<div class="card-grid">
			{#each data.projects as project}
				<a class="card" href="/{data.org.slug}/projects/{project.id}/settings/keys">
					<span class="card-title">{project.name}</span>
					<span class="card-desc">API keys for this project</span>
				</a>
			{/each}
		</div>
	{/if}

	<header class="page-header section-gap">
		<h2 class="section-title">Account &amp; instance</h2>
		<p class="page-subtitle">Not scoped to this organization.</p>
	</header>

	<div class="card-grid">
		{#each globalSections as section}
			<a class="card" href={section.href}>
				<span class="card-title">{section.title}</span>
				<span class="card-desc">{section.desc}</span>
			</a>
		{/each}
	</div>
</div>

<style>
	.settings-index {
		max-width: 960px;
		margin: 0 auto;
		padding: 2rem 1rem;
	}

	.page-header {
		margin-bottom: 1rem;
	}

	.section-gap {
		margin-top: 2.5rem;
	}

	.page-title {
		font-size: 1.375rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.section-title {
		font-size: 1rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.page-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.card-grid {
		display: grid;
		grid-template-columns: repeat(auto-fill, minmax(240px, 1fr));
		gap: 0.75rem;
	}

	.card {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
		padding: 1rem;
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-sm);
		text-decoration: none;
		transition: border-color 0.15s ease;
	}

	.card:hover {
		border-color: var(--color-primary);
	}

	.card-title {
		font-size: 0.875rem;
		font-weight: 600;
		color: var(--text-primary);
	}

	.card-desc {
		font-size: 0.75rem;
		color: var(--text-muted);
		line-height: 1.4;
	}

	.empty {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}
</style>
