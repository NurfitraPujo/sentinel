<script lang="ts">
	import '../app.css';
	import type { LayoutData } from './$types';
	import NotificationBell from '$lib/components/notifications/NotificationBell.svelte';

	let { data, children } = $props();
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
				{#if data.orgSlug}
					<a href="/{data.orgSlug}/reports" class="nav-link">Reports</a>
				{/if}
				<a href="/settings" class="nav-link">Settings</a>
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
