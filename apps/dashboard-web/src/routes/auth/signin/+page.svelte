<script lang="ts">
	import { signIn } from '@auth/sveltekit/client';
	import { page } from '$app/stores';

	// If we have an error from auth, show it
	const error = $page.url.searchParams.get('error');
</script>

<div class="flex min-h-screen items-center justify-center">
	<div class="text-center">
		<h1 class="text-2xl font-bold mb-4">Sign in to Sentinel</h1>
		{#if error}
			<p class="text-red-500 mb-4">Authentication error: {error}</p>
		{/if}
		<button
			on:click={() => signIn('google', { callbackUrl: '/' })}
			class="px-6 py-3 bg-blue-600 text-white rounded-lg hover:bg-blue-700"
		>
			Sign in with Google
		</button>
	</div>
</div>