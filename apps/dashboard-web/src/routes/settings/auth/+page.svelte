<script lang="ts">
    import { enhance } from '$app/forms';
    export let data;
    export let form;
</script>

<div class="max-w-2xl mx-auto p-6">
    <h1 class="text-2xl font-bold mb-6">Auth Settings</h1>

    <div class="bg-white shadow rounded-lg p-6">
        <h2 class="text-lg font-semibold mb-4">SMTP Configuration</h2>
        <p class="text-sm text-gray-600 mb-6">Configure SMTP for magic link and alert emails.</p>

        <form method="POST" action="?/save" use:enhance class="space-y-4">
            <div>
                <label for="smtpServer" class="block text-sm font-medium text-gray-700">SMTP Server URL</label>
                <input
                    type="text"
                    name="smtpServer"
                    id="smtpServer"
                    value={data.smtpServer}
                    placeholder="smtp://user:pass@host:port"
                    class="mt-1 block w-full border border-gray-300 rounded-md shadow-sm p-2"
                />
            </div>

            <div>
                <label for="smtpFrom" class="block text-sm font-medium text-gray-700">Sender Email (From)</label>
                <input
                    type="email"
                    name="smtpFrom"
                    id="smtpFrom"
                    value={data.smtpFrom}
                    placeholder="noreply@sentinel.local"
                    class="mt-1 block w-full border border-gray-300 rounded-md shadow-sm p-2"
                />
            </div>

            {#if form?.success}
                <p class="text-green-600 text-sm">Settings saved successfully!</p>
            {/if}

            {#if form?.message}
                <p class="text-red-600 text-sm">{form.message}</p>
            {/if}

            <button
                type="submit"
                class="bg-blue-600 text-white px-4 py-2 rounded-md hover:bg-blue-700 transition"
            >
                Save Settings
            </button>
        </form>
    </div>
</div>
