<script lang="ts">
	import { enhance } from '$app/forms';
	import type { PageData, ActionData } from './$types';

	export let data: PageData;
	export let form: ActionData;

	let loading = false;
</script>

<div class="auth-container">
	<div class="auth-box">
		<div class="auth-header">
			<div class="auth-logo">⚡ SENTINEL</div>
			{#if data.status === 'valid_authenticated'}
				<h1 class="auth-title">Organization Invitation</h1>
				<p class="auth-subtitle">You have been invited to join an engineering workspace</p>
			{:else if data.status === 'valid_unauthenticated'}
				<h1 class="auth-title">Join {data.organization?.name}</h1>
				<p class="auth-subtitle">Sign in or register to accept your invitation</p>
			{:else if data.status === 'already_member'}
				<h1 class="auth-title">Already a Member</h1>
				<p class="auth-subtitle">You already have access to this workspace</p>
			{:else if data.status === 'email_mismatch'}
				<h1 class="auth-title">Access Denied</h1>
				<p class="auth-subtitle">Credential mismatch</p>
			{:else}
				<h1 class="auth-title">Invitation Invalid</h1>
				<p class="auth-subtitle">Token validation failed</p>
			{/if}
		</div>

		{#if form?.error}
			<div class="alert error">
				<span>{form.error}</span>
			</div>
		{/if}

		{#if data.status === 'valid_authenticated'}
			<div class="invite-details">
				<div class="detail-row">
					<span class="detail-label">Organization</span>
					<span class="detail-val org-name">{data.organization?.name}</span>
				</div>
				<div class="detail-row">
					<span class="detail-label">Invited Email</span>
					<span class="detail-val mono">{data.invitation?.email}</span>
				</div>
				<div class="detail-row">
					<span class="detail-label">Assigned Role</span>
					<span class="badge role-badge">{data.invitation?.role}</span>
				</div>
			</div>

			<form method="POST" action="?/accept" use:enhance={() => {
				loading = true;
				return async ({ update }) => {
					await update();
					loading = false;
				};
			}}>
				<input type="hidden" name="token" value={data.token || ''} />
				<button type="submit" disabled={loading} class="btn btn-primary btn-full">
					{loading ? 'Joining Organization...' : 'Accept & Join Organization'}
				</button>
			</form>

		{:else if data.status === 'valid_unauthenticated'}
			{#if form?.magicLinkSent}
				<div class="alert success">
					<p class="alert-title">Check your email</p>
					<p class="alert-desc">A magic link has been sent to <strong>{form.email}</strong> to complete your sign-in.</p>
				</div>
			{:else}
				<div class="invite-summary">
					<p class="invite-text">
						An invitation to join <strong>{data.organization?.name}</strong> as <span class="badge role-badge">{data.invitation?.role}</span> was issued for <code class="mono">{data.invitation?.email}</code>.
					</p>
				</div>

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
							<span class="divider-text">OR MAGIC LINK</span>
						</div>

						<form method="POST" action="?/magiclink" use:enhance={() => {
							loading = true;
							return async ({ update }) => {
								await update();
								loading = false;
							};
						}} class="magic-form">
							<input
								type="email"
								name="email"
								value={data.invitation?.email}
								placeholder="engineer@company.com"
								required
								class="input-email"
							/>
							<button type="submit" disabled={loading} class="btn btn-secondary btn-full">
								{loading ? 'Sending Magic Link...' : 'Send Magic Link to Accept'}
							</button>
						</form>
					{/if}
				</div>
			{/if}

		{:else if data.status === 'already_member'}
			<div class="alert info">
				<p>You are already a member of <strong>{data.organization?.name}</strong> with role <span class="badge role-badge">{data.role}</span>.</p>
			</div>
			<a href="/{data.organization?.slug}" class="btn btn-primary btn-full text-center">
				Go to Workspace Dashboard
			</a>

		{:else if data.status === 'email_mismatch'}
			<div class="alert error">
				<p class="alert-title">Email Mismatch</p>
				<p class="alert-desc">Failed to accept invitation: You are logged in with different credentials than the invited recipient.</p>
			</div>
			<div class="actions-group">
				<a href="/auth/signout" class="btn btn-secondary btn-full text-center">
					Sign Out & Switch Account
				</a>
			</div>

		{:else}
			<div class="alert error">
				<p class="alert-title">Invalid or Expired Invitation</p>
				<p class="alert-desc">This invitation link is invalid, expired, or has already been redeemed.</p>
			</div>
			<a href="/" class="btn btn-secondary btn-full text-center">
				Return to Dashboard
			</a>
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
		max-width: 440px;
		background-color: var(--bg-surface, #1e293b);
		border: 1px solid var(--border-color, #334155);
		border-radius: var(--radius-md, 6px);
		padding: 2rem;
		box-shadow: 0 10px 30px rgba(0, 0, 0, 0.4);
	}

	.auth-header {
		text-align: center;
		margin-bottom: 1.5rem;
	}

	.auth-logo {
		font-family: var(--font-mono, monospace);
		font-weight: 700;
		font-size: 0.875rem;
		letter-spacing: 0.15em;
		color: var(--color-primary, #3b82f6);
		margin-bottom: 0.75rem;
	}

	.auth-title {
		font-size: 1.25rem;
		font-weight: 600;
		color: var(--text-primary, #f8fafc);
		margin-bottom: 0.25rem;
	}

	.auth-subtitle {
		font-size: 0.8125rem;
		color: var(--text-muted, #94a3b8);
	}

	.invite-details {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
		background-color: var(--bg-root, #0f172a);
		border: 1px solid var(--border-color, #334155);
		border-radius: var(--radius-sm, 4px);
		padding: 1rem;
		margin-bottom: 1.5rem;
	}

	.detail-row {
		display: flex;
		justify-content: space-between;
		align-items: center;
		font-size: 0.8125rem;
	}

	.detail-label {
		color: var(--text-muted, #94a3b8);
	}

	.detail-val {
		color: var(--text-primary, #f8fafc);
		font-weight: 500;
	}

	.org-name {
		font-weight: 600;
		color: var(--color-primary, #3b82f6);
	}

	.invite-summary {
		background-color: var(--bg-root, #0f172a);
		border: 1px solid var(--border-color, #334155);
		border-radius: var(--radius-sm, 4px);
		padding: 1rem;
		margin-bottom: 1.25rem;
		font-size: 0.8125rem;
		color: var(--text-muted, #94a3b8);
		line-height: 1.5;
	}

	.invite-text strong {
		color: var(--text-primary, #f8fafc);
	}

	.mono {
		font-family: var(--font-mono, monospace);
		font-size: 0.8125rem;
	}

	.badge {
		display: inline-flex;
		align-items: center;
		padding: 2px 8px;
		border-radius: 4px;
		font-family: var(--font-mono, monospace);
		font-size: 0.75rem;
		font-weight: 600;
		text-transform: uppercase;
	}

	.role-badge {
		background-color: rgba(59, 130, 246, 0.15);
		color: #3b82f6;
		border: 1px solid rgba(59, 130, 246, 0.3);
	}

	.alert {
		padding: 0.875rem 1rem;
		border-radius: var(--radius-sm, 4px);
		font-size: 0.8125rem;
		margin-bottom: 1.25rem;
		line-height: 1.4;
	}

	.alert.error {
		background-color: rgba(239, 68, 68, 0.15);
		color: #ef4444;
		border: 1px solid rgba(239, 68, 68, 0.3);
	}

	.alert.success {
		background-color: rgba(16, 185, 129, 0.15);
		color: #10b981;
		border: 1px solid rgba(16, 185, 129, 0.3);
	}

	.alert.info {
		background-color: rgba(59, 130, 246, 0.15);
		color: #3b82f6;
		border: 1px solid rgba(59, 130, 246, 0.3);
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
		background-color: var(--bg-root, #0f172a);
		border: 1px solid var(--border-color, #334155);
		color: var(--text-primary, #f8fafc);
		border-radius: var(--radius-sm, 4px);
	}

	.input-email:focus {
		outline: none;
		border-color: var(--color-primary, #3b82f6);
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
		background-color: var(--border-color, #334155);
	}

	.divider-text {
		position: relative;
		background-color: var(--bg-surface, #1e293b);
		padding: 0 0.5rem;
		font-family: var(--font-mono, monospace);
		font-size: 0.6875rem;
		color: var(--text-muted, #94a3b8);
		letter-spacing: 0.05em;
	}

	.btn {
		display: inline-flex;
		align-items: center;
		justify-content: center;
		padding: 0.625rem 1rem;
		border-radius: var(--radius-sm, 4px);
		font-size: 0.875rem;
		font-weight: 500;
		cursor: pointer;
		border: none;
		text-decoration: none;
		transition: background 0.15s ease, opacity 0.15s ease;
	}

	.btn-full {
		width: 100%;
	}

	.text-center {
		text-align: center;
	}

	.btn-primary {
		background-color: var(--color-primary, #3b82f6);
		color: #ffffff;
	}

	.btn-primary:hover:not(:disabled) {
		background-color: #1d4ed8;
	}

	.btn-secondary {
		background-color: var(--bg-root, #0f172a);
		color: var(--text-primary, #f8fafc);
		border: 1px solid var(--border-color, #334155);
	}

	.btn-secondary:hover:not(:disabled) {
		background-color: #334155;
	}

	.btn:disabled {
		opacity: 0.5;
		cursor: not-allowed;
	}

	.actions-group {
		margin-top: 1rem;
	}
</style>
