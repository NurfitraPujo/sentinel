<script lang="ts">
	import { signIn } from '@auth/sveltekit/client';
	import { page } from '$app/stores';
	import type { PageData } from './$types';

	export let data: PageData;

	const error = $page.url.searchParams.get('error');
	const errorParam = $page.url.searchParams.get('error_description');

	let email = '';
	let loading = false;
	let magicLinkSent = false;

	async function handleMagicLinkSubmit(e: Event) {
		e.preventDefault();
		loading = true;
		try {
			await signIn('email', { email, callbackUrl: '/' });
			magicLinkSent = true;
		} catch (err) {
			console.error('Magic link error:', err);
		} finally {
			loading = false;
		}
	}
</script>

<div class="flex min-h-screen items-center justify-center">
	<div class="text-center">
		<h1 class="text-2xl font-bold mb-4">Sign in to Sentinel</h1>
		{#if error || errorParam}
			<p class="text-red-500 mb-4">Authentication error: {error || errorParam}</p>
		{/if}
		{#if magicLinkSent}
			<div class="p-4 bg-green-50 text-green-700 rounded-lg mb-4">
				<p class="font-medium">Check your email</p>
				<p class="text-sm">A magic link has been sent to your email address.</p>
			</div>
		{:else}
			<div class="flex flex-col gap-4">
				<button
					on:click={() => signIn('google', { callbackUrl: '/' })}
					class="px-6 py-3 bg-blue-600 text-white rounded-lg hover:bg-blue-700"
				>
					Sign in with Google
				</button>
				{#if data.emailConfigured}
					<div class="relative">
						<div class="absolute inset-0 flex items-center">
							<div class="w-full border-t border-gray-300"></div>
						</div>
						<div class="relative flex justify-center text-sm">
							<span class="px-2 bg-white text-gray-500">or</span>
						</div>
					</div>
					<form
						on:submit={handleMagicLinkSubmit}
						class="flex flex-col gap-2"
					>
						<input
							type="email"
							bind:value={email}
							placeholder="Enter your email"
							required
							class="px-4 py-2 border border-gray-300 rounded-lg focus:ring-2 focus:ring-blue-500 focus:border-transparent"
						/>
						<button
							type="submit"
							disabled={loading}
							class="px-6 py-3 bg-gray-100 text-gray-700 rounded-lg hover:bg-gray-200 disabled:opacity-50"
						>
							{loading ? 'Sending...' : 'Send Magic Link'}
						</button>
					</form>
				{/if}
			</div>
		{/if}
	</div>
</div>