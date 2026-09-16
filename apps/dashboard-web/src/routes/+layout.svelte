<script lang="ts">
	import '../app.css';
	import type { LayoutData } from './$types';
	import NotificationBell from '$lib/components/notifications/NotificationBell.svelte';
	import { onNavigate } from '$app/navigation';
	import { updated } from '$app/state';

	let { data, children } = $props();

	onNavigate(async (navigation) => {
		if (!navigation.to) return;
		try {
			if (await updated.check()) {
				// New app version detected on navigation: force a full reload so the client
				// loads the newly deployed assets and chunk hashes cleanly.
				location.href = navigation.to.url.href;
			}
		} catch {
			// updated.check() can fail if offline or network glitch; let normal navigation proceed
		}
	});
</script>

<div class="app">
	<header class="app-header">
		<div class="header-content">
			<div class="brand">
				<a href="/" class="logo">
					<span class="logo-icon">⚡</span>
					<span class="logo-text">SENTINEL</span>
				</a>
			</div>
			<nav class="nav-menu">
				<a href="/" class="nav-link active">Dashboard</a>
				<a href="/issues" class="nav-link">Issues</a>
				<!-- /search had exactly one inbound link in the entire app: its own "Clear" button.
				     The page works and is substantial (trace/span/request/tenant/user filters), it
				     just could not be found from anywhere. -->
				<a href="/search" class="nav-link">Search</a>
				{#if data.orgSlug}
					<a href="/{data.orgSlug}/reports" class="nav-link">Reports</a>
					<a href="/{data.orgSlug}/projects" class="nav-link">Projects</a>
				{/if}
				<!-- Settings points at the ORGANIZATION's settings when the user has one. Every
				     org-scoped page — API keys above all — used to be reachable only by typing its
				     URL, because this link went to the global /settings (alerts, auth,
				     observability) and nothing anywhere linked to the org pages. The org settings
				     index lists the global sections too, so nothing became unreachable in trade. -->
				<a href={data.orgSlug ? `/${data.orgSlug}/settings` : '/settings'} class="nav-link">Settings</a>
			</nav>
			<div class="user-menu">
				{#if data.session}
					<NotificationBell />
					<span class="user-email">{data.session.user?.email}</span>
					<form action="/auth/signout" method="post">
						<button type="submit" class="btn-signout">Sign Out</button>
					</form>
				{:else}
					<a href="/signin" class="btn-signin">Sign In</a>
				{/if}
			</div>
		</div>
	</header>

	<main class="app-main">
		{@render children()}
	</main>
</div>

<style>
	.app {
		min-height: 100vh;
		display: flex;
		flex-direction: column;
		background-color: var(--bg-root);
		color: var(--text-primary);
	}

	.app-header {
		height: 48px;
		background-color: var(--bg-surface);
		border-bottom: 1px solid var(--border-color);
		display: flex;
		align-items: center;
		padding: 0 1.5rem;
	}

	.header-content {
		width: 100%;
		display: flex;
		justify-content: space-between;
		align-items: center;
	}

	.brand {
		display: flex;
		align-items: center;
	}

	.logo {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		text-decoration: none;
		color: var(--text-primary);
	}

	.logo-icon {
		font-size: 1rem;
	}

	.logo-text {
		font-family: var(--font-mono);
		font-weight: 700;
		font-size: 0.875rem;
		letter-spacing: 0.1em;
		color: var(--text-primary);
	}

	.nav-menu {
		display: flex;
		gap: 1.5rem;
		align-items: center;
	}

	.nav-link {
		color: var(--text-muted);
		text-decoration: none;
		font-size: 0.875rem;
		font-weight: 500;
		transition: color 0.15s ease;
	}

	.nav-link:hover, .nav-link.active {
		color: var(--text-primary);
	}

	.user-menu {
		display: flex;
		align-items: center;
		gap: 1rem;
	}

	.user-email {
		font-family: var(--font-mono);
		font-size: 0.75rem;
		color: var(--text-subtle);
	}

	.btn-signout, .btn-signin {
		background: transparent;
		border: 1px solid var(--border-color);
		color: var(--text-muted);
		padding: 0.25rem 0.625rem;
		border-radius: var(--radius-sm);
		font-size: 0.75rem;
		font-weight: 500;
		cursor: pointer;
		text-decoration: none;
		transition: background 0.15s ease, color 0.15s ease, border-color 0.15s ease;
	}

	.btn-signout:hover, .btn-signin:hover {
		background-color: var(--bg-surface-hover);
		color: var(--text-primary);
		border-color: var(--text-muted);
	}

	.app-main {
		flex: 1;
		padding: 1.5rem;
	}
</style>
