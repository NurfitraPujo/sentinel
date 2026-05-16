<script lang="ts">
	import { enhance } from '$app/forms';
	import { page } from '$app/stores';
	import type { PageData, ActionData } from './$types';

	export let data: PageData;
	export let form: ActionData;

	const error = $page.url.searchParams.get('error');
	const errorParam = $page.url.searchParams.get('error_description');

	let loading = false;
</script>

<div class="flex min-h-screen items-center justify-center">
	<div class="text-center">
		<h1 class="text-2xl font-bold mb-4">Sign in to Sentinel</h1>
		{#if error || errorParam}
			<p class="text-red-500 mb-4">Authentication error: {error || errorParam}</p>
		{/if}
		{#if form?.success}
			<div class="p-4 bg-green-50 text-green-700 rounded-lg mb-4">
				<p class="font-medium">Check your email</p>
				<p class="text-sm">A magic link has been sent to your email address.</p>
			</div>
		{:else}
			<div class="flex flex-col gap-4">
				<form method="POST" action="?/google" use:enhance={() => {
					loading = true;
					return async ({ update }) => {
						await update();
						loading = false;
					};
				}}>
					<button
						type="submit"
						disabled={loading}
						class="px-6 py-3 bg-blue-600 text-white rounded-lg hover:bg-blue-700 disabled:opacity-50"
					>
						Sign in with Google
					</button>
				</form>
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
						method="POST"
						action="?/magiclink"
						use:enhance={() => {
							loading = true;
							return async ({ update }) => {
								await update();
								loading = false;
							};
						}}
						class="flex flex-col gap-2"
					>
						{#if form?.error}
							<p class="text-red-500 text-sm">{form.error}{#if form.retryAfter} Retry after {form.retryAfter}s.{/if}</p>
						{/if}
						<input
							type="email"
							name="email"
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