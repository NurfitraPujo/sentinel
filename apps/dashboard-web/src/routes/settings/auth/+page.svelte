<script lang="ts">
    import { enhance } from '$app/forms';
    export let data;
    export let form;
</script>

<div class="auth-settings">
    <h1 class="page-title">Auth Settings</h1>

    <div class="card">
        <h2 class="card-title">SMTP Configuration</h2>
        <p class="card-subtitle">Configure SMTP for magic link and alert emails.</p>

        <form method="POST" action="?/save" use:enhance class="stack">
            <div>
                <label for="smtpServer" class="field-label">SMTP Server URL</label>
                <input
                    type="text"
                    name="smtpServer"
                    id="smtpServer"
                    value={data.smtpServer}
                    placeholder="smtp://user:pass@host:port"
                    class="field-input"
                />
            </div>

            <div>
                <label for="smtpFrom" class="field-label">Sender Email (From)</label>
                <input
                    type="email"
                    name="smtpFrom"
                    id="smtpFrom"
                    value={data.smtpFrom}
                    placeholder="noreply@sentinel.local"
                    class="field-input"
                />
            </div>

            {#if form?.success}
                <p class="msg msg-success">Settings saved successfully!</p>
            {/if}

            {#if form?.message}
                <p class="msg msg-error">{form.message}</p>
            {/if}

            <div>
                <button type="submit" class="btn btn-primary">
                    Save Settings
                </button>
            </div>
        </form>
    </div>
</div>

<style>
    /* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind.
       Note the original asked for `bg-white` and `text-gray-700`: a light card with dark labels,
       inside a dark application. Those are the tokens a Tailwind starter template ships with, not
       a choice anyone made for this app, so the rewrite uses the surface tokens instead. */
    .auth-settings {
        max-width: 42rem;
        margin: 0 auto;
        padding: 1.5rem 1rem;
    }

    .page-title {
        font-size: 1.375rem;
        font-weight: 700;
        color: var(--text-primary);
        margin-bottom: 1.5rem;
    }

    .card {
        background-color: var(--bg-surface);
        border: 1px solid var(--border-color);
        border-radius: var(--radius-lg);
        padding: 1.5rem;
    }

    .card-title {
        font-size: 1rem;
        font-weight: 600;
        color: var(--text-primary);
        margin-bottom: 0.5rem;
    }

    .card-subtitle {
        font-size: 0.8125rem;
        color: var(--text-muted);
        margin-bottom: 1.5rem;
    }

    .stack {
        display: flex;
        flex-direction: column;
        gap: 1rem;
    }

    .field-label {
        display: block;
        font-size: 0.8125rem;
        font-weight: 500;
        color: var(--text-primary);
        margin-bottom: 0.375rem;
    }

    .field-input {
        display: block;
        width: 100%;
        padding: 0.5rem 0.75rem;
        font-size: 0.875rem;
    }

    .msg {
        font-size: 0.8125rem;
    }

    .msg-success {
        color: var(--status-resolved-text);
    }

    .msg-error {
        color: var(--severity-critical-text);
    }

    .btn {
        padding: 0.5rem 1rem;
        border-radius: var(--radius-sm);
        font-size: 0.875rem;
        font-weight: 500;
        border: none;
        cursor: pointer;
    }

    .btn-primary {
        background-color: var(--color-primary);
        color: var(--text-primary);
    }

    .btn-primary:hover {
        background-color: var(--color-primary-hover);
    }
</style>
