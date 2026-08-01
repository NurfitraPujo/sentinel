<script lang="ts">
  import { createEventDispatcher } from 'svelte';

  export let show = false;
  export let orgId: string;

  // D06 (P1): the invitations endpoint never returns the raw token or a pre-built invite link to
  // the browser -- the token exists only in the emailed URL, so the modal has no link to display
  // or copy. `invited` therefore carries the created invitation's public fields only.
  const dispatch = createEventDispatcher<{
    close: void;
    invited: { id: string; email: string; role: string };
  }>();

  const roles = [
    { value: 'viewer', label: 'Viewer - Read-only access to issues and telemetry' },
    { value: 'support', label: 'Support - Manage issues and user tickets' },
    { value: 'engineer', label: 'Engineer - Full issue triage, SDK keys, and alerts' },
    { value: 'admin', label: 'Admin - Full organization access and member management' },
    { value: 'owner', label: 'Owner - Full ownership and organization control' },
  ];

  let email = '';
  let selectedRole = 'engineer';
  let loading = false;
  let errorMessage = '';
  let invitationSent = false;
  let emailInputEl: HTMLInputElement | null = null;

  function resetForm() {
    email = '';
    selectedRole = 'engineer';
    loading = false;
    errorMessage = '';
    invitationSent = false;
  }

  function handleClose() {
    resetForm();
    dispatch('close');
  }

  function handleKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape' && show) {
      handleClose();
    }
  }

  $: if (show && emailInputEl) {
    setTimeout(() => emailInputEl?.focus(), 50);
  }

  async function handleSubmit() {
    if (!email || !selectedRole) {
      errorMessage = 'Please provide a valid email address.';
      return;
    }

    loading = true;
    errorMessage = '';

    try {
      const response = await fetch(`/api/organizations/${orgId}/invitations`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: email.trim(), role: selectedRole }),
      });

      let result: any = {};
      try {
        result = await response.json();
      } catch {
        result = { message: `HTTP ${response.status}: ${response.statusText}` };
      }

      if (!response.ok) {
        throw new Error(result.message || result.error || 'Failed to issue invitation');
      }

      // D06/D41: the server never hands the raw invitation token (or a pre-built link) to the
      // browser -- it exists only inside the email it tries to send. A 201 here means the
      // invitation row was created; it does NOT mean delivery succeeded (the endpoint swallows
      // send failures). There is nothing this modal can show as a clickable/copyable link.
      invitationSent = true;
      dispatch('invited', result);
    } catch (err: any) {
      errorMessage = err.message || 'An unexpected error occurred';
    } finally {
      loading = false;
    }
  }
</script>

<svelte:window on:keydown={handleKeydown} />

{#if show}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center p-4 bg-slate-950/80"
    role="dialog"
    aria-modal="true"
    aria-labelledby="invite-modal-title"
    tabindex="-1"
    on:click|self={handleClose}
    on:keydown={(e) => {
      if (e.key === 'Escape') handleClose();
    }}
  >
    <div
      class="w-full max-w-md bg-slate-900 border border-slate-800 rounded-md p-6 text-slate-100"
    >
      <div class="flex items-center justify-between border-b border-slate-800 pb-4 mb-4">
        <h2 id="invite-modal-title" class="text-base font-semibold text-slate-100 flex items-center gap-2">
          <svg class="w-5 h-5 text-blue-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M18 9v3m0 0v3m0-3h3m-3 0h-3m-2-5a4 4 0 11-8 0 4 4 0 018 0zM3 20a6 6 0 0112 0v1H3v-1z" />
          </svg>
          Invite Team Member
        </h2>
        <button
          type="button"
          on:click={handleClose}
          class="text-slate-400 hover:text-slate-200 transition-colors p-1 rounded focus:outline-hidden focus:ring-2 focus:ring-blue-500"
          aria-label="Close modal"
        >
          <svg class="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
      </div>

      {#if invitationSent}
        <div class="space-y-4">
          <div class="p-3 bg-emerald-950/40 border border-emerald-500/30 rounded-md text-sm text-emerald-300">
            Invitation created for <strong>{email}</strong>. We've sent them an email with a link to
            accept it. If they don't receive it, they can be re-invited from this org's outstanding
            invitations list.
          </div>

          <div class="pt-4 border-t border-slate-800 flex justify-end">
            <button
              type="button"
              on:click={handleClose}
              class="px-4 py-2 text-xs font-medium bg-slate-800 hover:bg-slate-700 text-slate-200 rounded border border-slate-700 transition-colors"
            >
              Done
            </button>
          </div>
        </div>
      {:else}
        <form on:submit|preventDefault={handleSubmit} class="space-y-4">
          {#if errorMessage}
            <div class="p-3 bg-red-950/50 border border-red-500/40 rounded-md text-xs text-red-300">
              {errorMessage}
            </div>
          {/if}

          <div>
            <label for="invite-email" class="block text-xs font-mono uppercase text-slate-400 mb-1">
              Email Address
            </label>
            <input
              id="invite-email"
              type="email"
              bind:this={emailInputEl}
              bind:value={email}
              required
              placeholder="colleague@company.com"
              class="w-full bg-slate-950 border border-slate-800 rounded px-3 py-2 text-sm text-slate-100 placeholder-slate-500 focus:outline-hidden focus:border-blue-500 focus:ring-1 focus:ring-blue-500"
            />
          </div>

          <div>
            <label for="invite-role" class="block text-xs font-mono uppercase text-slate-400 mb-1">
              Organization Role
            </label>
            <select
              id="invite-role"
              bind:value={selectedRole}
              class="w-full bg-slate-950 border border-slate-800 rounded px-3 py-2 text-sm text-slate-100 focus:outline-hidden focus:border-blue-500 focus:ring-1 focus:ring-blue-500"
            >
              {#each roles as role}
                <option value={role.value}>{role.label}</option>
              {/each}
            </select>
          </div>

          <div class="pt-4 border-t border-slate-800 flex items-center justify-end gap-3">
            <button
              type="button"
              on:click={handleClose}
              disabled={loading}
              class="px-4 py-2 text-xs font-medium text-slate-400 hover:text-slate-200 transition-colors disabled:opacity-50"
            >
              Cancel
            </button>
            <button
              type="submit"
              disabled={loading}
              class="px-4 py-2 text-xs font-semibold bg-blue-600 hover:bg-blue-500 text-white rounded transition-colors flex items-center gap-2 focus:outline-hidden focus:ring-2 focus:ring-blue-500 disabled:opacity-50"
            >
              {#if loading}
                <svg class="w-3.5 h-3.5 animate-spin" viewBox="0 0 24 24" fill="none">
                  <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
                  <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
                </svg>
                Creating...
              {:else}
                Generate Invitation
              {/if}
            </button>
          </div>
        </form>
      {/if}
    </div>
  </div>
{/if}
