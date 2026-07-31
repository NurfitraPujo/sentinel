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

<div class="auth-container">
	<div class="auth-box">
		<div class="auth-header">
			<div class="auth-logo">⚡ SENTINEL</div>
			<h1 class="auth-title">Sign in to Control Room</h1>
			<p class="auth-subtitle">Production error tracking and telemetry</p>
		</div>

		{#if error || errorParam}
			<div class="alert error">
				<span>Authentication error: {error || errorParam}</span>
			</div>
		{/if}

		{#if form?.success}
			<div class="alert success">
				<p class="alert-title">Check your email</p>
				<p class="alert-desc">A magic link has been dispatched to your email address.</p>
			</div>
		{:else}
			<div class="auth-form-group">
				<form method="POST" action="?/google" use:enhance={() => {
					loading = true;
					return async ({ update }) => {
						await update();
						loading = false;
					};
				}}>
					<button type="submit" disabled={loading} class="btn btn-primary btn-full">
						Sign in with Google
					</button>
				</form>

				{#if data.emailConfigured}
					<div class="divider">
						<span class="divider-text">OR</span>
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
						class="magic-form"
					>
						{#if form?.error}
							<p class="form-error">{form.error}{#if 'retryAfter' in form && form.retryAfter} Retry after {form.retryAfter}s.{/if}</p>
						{/if}

						<input
							type="email"
							name="email"
							placeholder="engineer@company.com"
							required
							class="input-email"
						/>
						<button type="submit" disabled={loading} class="btn btn-secondary btn-full">
							{loading ? 'Sending...' : 'Send Magic Link'}
						</button>
					</form>
				{/if}
			</div>
		{/if}
	</div>
</div>

<style>
	.auth-container {
		min-height: calc(100vh - 120px);
		display: flex;
		align-items: center;
		justify-content: center;
		padding: 2rem 1rem;
	}

	.auth-box {
		width: 100%;
		max-width: 400px;
		background-color: var(--bg-surface);
		border: 1px solid var(--border-color);
		border-radius: var(--radius-lg);
		padding: 2rem;
		box-shadow: 0 10px 30px rgba(0, 0, 0, 0.35);
	}

	.auth-header {
		text-align: center;
		margin-bottom: 1.5rem;
	}

	.auth-logo {
		font-family: var(--font-mono);
		font-weight: 700;
		font-size: 0.875rem;
		letter-spacing: 0.15em;
		color: var(--color-primary);
		margin-bottom: 0.75rem;
	}

	.auth-title {
		font-size: 1.25rem;
		font-weight: 600;
		color: var(--text-primary);
		margin-bottom: 0.25rem;
	}

	.auth-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted);
	}

	.alert {
		padding: 0.75rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.8125rem;
		margin-bottom: 1rem;
	}

	.alert.error {
		background-color: var(--severity-critical-bg);
		color: var(--severity-critical-text);
		border: 1px solid var(--severity-critical-border);
	}

	.alert.success {
		background-color: var(--status-resolved-bg);
		color: var(--status-resolved-text);
		border: 1px solid rgba(16, 185, 129, 0.3);
	}

	.alert-title {
		font-weight: 600;
		margin-bottom: 0.25rem;
	}

	.alert-desc {
		font-size: 0.75rem;
		opacity: 0.9;
	}

	.auth-form-group {
		display: flex;
		flex-direction: column;
		gap: 1rem;
	}

	.magic-form {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
	}

	.input-email {
		padding: 0.625rem 0.875rem;
		font-size: 0.875rem;
		width: 100%;
	}

	.divider {
		position: relative;
		text-align: center;
		margin: 0.5rem 0;
	}

	.divider::before {
		content: '';
		position: absolute;
		top: 50%;
		left: 0;
		right: 0;
		height: 1px;
		background-color: var(--border-color);
	}

	.divider-text {
		position: relative;
		background-color: var(--bg-surface);
		padding: 0 0.5rem;
		font-family: var(--font-mono);
		font-size: 0.6875rem;
		color: var(--text-subtle);
		letter-spacing: 0.05em;
	}

	.form-error {
		color: var(--severity-critical-text);
		font-size: 0.75rem;
	}

	.btn {
		padding: 0.625rem 1rem;
		border-radius: var(--radius-sm);
		font-size: 0.875rem;
		font-weight: 500;
		cursor: pointer;
		border: none;
		transition: background 0.15s ease, opacity 0.15s ease;
	}

	.btn-full {
		width: 100%;
	}

	.btn-primary {
		background-color: var(--color-primary);
		color: var(--text-primary);
	}

	.btn-primary:hover:not(:disabled) {
		background-color: var(--color-primary-hover);
	}

	.btn-secondary {
		background-color: var(--bg-root);
		color: var(--text-primary);
		border: 1px solid var(--border-color);
	}

	.btn-secondary:hover:not(:disabled) {
		background-color: var(--bg-surface-hover);
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}
</style>