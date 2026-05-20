<script lang="ts">
	import '../app.css';
	import type { LayoutData } from './$types';

	let { data, children } = $props();
</script>

<div class="app">
	<header>
		<nav>
			<a href="/">Dashboard</a>
			{#if data.session}
				<span>{data.session.user?.email}</span>
				<form action="/auth/signout" method="post">
					<button type="submit">Sign Out</button>
				</form>
			{/if}
		</nav>
	</header>

	<main>
		{@render children()}
	</main>
</div>

<style>
	.app {
		min-height: 100vh;
		display: flex;
		flex-direction: column;
	}
	header {
		padding: 1rem;
		border-bottom: 1px solid #ccc;
	}
	nav {
		display: flex;
		gap: 1rem;
		align-items: center;
	}
	main {
		flex: 1;
		padding: 1rem;
	}
</style>
