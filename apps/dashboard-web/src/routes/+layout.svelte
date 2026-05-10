<script lang="ts">
	import '../app.css';
	import { session } from '$app/stores';
	import type { LayoutData } from './$types';

	export let data: LayoutData;
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
		<slot />
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
