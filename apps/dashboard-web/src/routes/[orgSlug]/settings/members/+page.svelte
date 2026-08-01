<script lang="ts">
  import InviteMemberModal from '$lib/components/members/InviteMemberModal.svelte';

  export let data: {
    orgId: string;
    orgSlug: string;
    members: Array<{ id: string; userId?: string; user: { name: string; email: string }; role: string }>;
  };

  const roles = ['owner', 'admin', 'engineer', 'support', 'viewer'];

  let members = data.members || [];
  $: members = data.members || [];

  let showInviteModal = false;
  let revokingMember: { id: string; name: string } | null = null;
  let updatingMemberId: string | null = null;
  let isRevoking = false;

  let toastMessage: string | null = null;
  let toastType: 'error' | 'success' = 'error';
  let toastTimer: ReturnType<typeof setTimeout> | null = null;

  function showToast(message: string, type: 'error' | 'success' = 'error') {
    if (toastTimer) clearTimeout(toastTimer);
    toastMessage = message;
    toastType = type;
    toastTimer = setTimeout(() => {
      if (toastMessage === message) toastMessage = null;
    }, 5000);
  }

  function handleKeydown(event: KeyboardEvent) {
    if (event.key === 'Escape' && revokingMember) {
      revokingMember = null;
    }
  }

  async function handleRoleChange(memberId: string, newRole: string) {
    updatingMemberId = memberId;
    try {
      const response = await fetch(`/api/organizations/${data.orgId}/members/${memberId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ role: newRole }),
      });

      let result: any = {};
      try {
        result = await response.json();
      } catch {
        result = { message: `HTTP ${response.status}: ${response.statusText}` };
      }

      if (!response.ok) {
        showToast(result.message || result.error || 'Failed to update role');
        members = [...members];
        return;
      }

      members = members.map((m) => (m.id === memberId ? { ...m, role: newRole } : m));
      showToast('Member role updated successfully', 'success');
    } catch (err: any) {
      showToast(err.message || 'Network error updating member role');
    } finally {
      updatingMemberId = null;
    }
  }

  function promptRevokeAccess(memberId: string, memberName: string) {
    revokingMember = { id: memberId, name: memberName };
  }

  async function confirmRevokeAccess() {
    if (!revokingMember || isRevoking) return;
    const targetId = revokingMember.id;
    isRevoking = true;

    try {
      const response = await fetch(`/api/organizations/${data.orgId}/members/${targetId}`, {
        method: 'DELETE',
      });

      let result: any = {};
      try {
        result = await response.json();
      } catch {
        result = { message: `HTTP ${response.status}: ${response.statusText}` };
      }

      if (!response.ok) {
        showToast(result.message || result.error || 'Failed to revoke access');
        return;
      }

      members = members.filter((m) => m.id !== targetId);
      showToast('Member access revoked successfully', 'success');
    } catch (err: any) {
      showToast(err.message || 'Network error revoking member access');
    } finally {
      isRevoking = false;
      revokingMember = null;
    }
  }

  function handleInvited(event: CustomEvent<{ email: string; role: string }>) {
    showToast(`Invitation created for ${event.detail.email}`, 'success');
  }
</script>

<svelte:window on:keydown={handleKeydown} />

<div class="members-container min-h-screen bg-slate-950 text-slate-100 p-6">
  <div class="max-w-6xl mx-auto">
    <!-- Header -->
    <div class="flex justify-between items-start border-b border-slate-800 pb-4 mb-6">
      <div>
        <h1 class="text-xl font-bold tracking-tight text-slate-100 flex items-center gap-2">
          <svg class="w-5 h-5 text-blue-500" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 4.354a4 4 0 110 5.292M15 21H3v-1a6 6 0 0112 0v1zm0 0h6v-1a6 6 0 00-9-5.197M13 7a4 4 0 11-8 0 4 4 0 018 0z" />
          </svg>
          Organization Members
        </h1>
        <p class="text-xs text-slate-400 mt-1">
          Manage team members, permissions, and roles within this organization.
        </p>
      </div>
      <button
        type="button"
        on:click={() => (showInviteModal = true)}
        class="px-4 py-2 bg-blue-600 hover:bg-blue-500 text-white font-semibold rounded-md text-xs transition-colors flex items-center gap-1.5 focus:outline-hidden focus:ring-2 focus:ring-blue-500"
      >
        <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 4v16m8-8H4" />
        </svg>
        Invite Member
      </button>
    </div>

    <!-- Toast Notification Banner -->
    {#if toastMessage}
      <div
        role="status"
        aria-live="polite"
        class={`mb-4 px-4 py-3 rounded-md text-xs font-medium border flex justify-between items-center ${toastType === 'error' ? 'bg-red-950/80 border-red-800 text-red-200' : 'bg-emerald-950/80 border-emerald-800 text-emerald-200'}`}
      >
        <span>{toastMessage}</span>
        <button
          type="button"
          on:click={() => (toastMessage = null)}
          aria-label="Dismiss notification"
          class="text-xs font-bold opacity-75 hover:opacity-100 p-1"
        >
          ✕
        </button>
      </div>
    {/if}

    <!-- Members Table -->
    <div class="bg-slate-900 border border-slate-800 rounded-md overflow-hidden">
      <div class="overflow-x-auto">
        <table class="w-full text-left border-collapse">
          <thead>
            <tr class="bg-slate-900/90 border-b border-slate-800 text-[11px] font-mono uppercase text-slate-400">
              <th scope="col" class="py-3 px-4 font-semibold">User</th>
              <th scope="col" class="py-3 px-4 font-semibold">Email</th>
              <th scope="col" class="py-3 px-4 font-semibold">Role</th>
              <th scope="col" class="py-3 px-4 text-right font-semibold">Actions</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-slate-800/60 text-xs">
            {#each members as member}
              <tr class="hover:bg-slate-800/40 transition-colors h-10">
                <td class="py-2.5 px-4 font-medium text-slate-200">{member.user.name}</td>
                <td class="py-2.5 px-4 font-mono text-slate-400 truncate max-w-[200px] md:max-w-[320px]">
                  {member.user.email}
                </td>
                <td class="py-2.5 px-4">
                  <select
                    value={member.role}
                    disabled={updatingMemberId === member.id}
                    aria-label={`Role for ${member.user.name}`}
                    on:change={(e) => handleRoleChange(member.id, e.currentTarget.value)}
                    class="bg-slate-950 border border-slate-800 text-slate-200 font-mono text-xs rounded px-2.5 py-1 focus:outline-hidden focus:border-blue-500 focus:ring-1 focus:ring-blue-500 disabled:opacity-50"
                  >
                    {#each roles as role}
                      <option value={role}>{role}</option>
                    {/each}
                  </select>
                </td>
                <td class="py-2.5 px-4 text-right">
                  <button
                    type="button"
                    aria-label={`Revoke access for ${member.user.name}`}
                    on:click={() => promptRevokeAccess(member.id, member.user.name)}
                    class="text-red-400 hover:text-red-300 font-medium text-xs transition-colors px-2 py-1 rounded hover:bg-red-950/40"
                  >
                    Revoke Access
                  </button>
                </td>
              </tr>
            {/each}
            {#if members.length === 0}
              <tr>
                <td colspan="4" class="py-12 text-center text-xs text-slate-500 font-mono">
                  No organization members found.
                </td>
              </tr>
            {/if}
          </tbody>
        </table>
      </div>
    </div>
  </div>
</div>

<!-- Invite Member Modal -->
<InviteMemberModal
  bind:show={showInviteModal}
  orgId={data.orgId}
  on:invited={handleInvited}
/>

<!-- Revoke Confirmation Modal -->
{#if revokingMember}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center p-4 bg-slate-950/80"
    role="dialog"
    aria-modal="true"
    aria-labelledby="revoke-modal-title"
    aria-describedby="revoke-modal-desc"
    on:click|self={() => (revokingMember = null)}
  >
    <div class="w-full max-w-sm bg-slate-900 border border-slate-800 rounded-md p-5 text-slate-100 space-y-4">
      <div class="flex items-center gap-3 text-red-400">
        <svg class="w-6 h-6 shrink-0" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
        </svg>
        <h3 id="revoke-modal-title" class="text-sm font-semibold text-slate-100">Revoke Organization Access</h3>
      </div>
      <p id="revoke-modal-desc" class="text-xs text-slate-300">
        Are you sure you want to revoke access for <strong class="text-slate-100">{revokingMember.name}</strong>? They will lose access to all organization resources and projects immediately.
      </p>
      <div class="flex justify-end gap-3 pt-2">
        <button
          type="button"
          disabled={isRevoking}
          on:click={() => (revokingMember = null)}
          class="px-3 py-1.5 text-xs font-medium text-slate-400 hover:text-slate-200 transition-colors disabled:opacity-50"
        >
          Cancel
        </button>
        <button
          type="button"
          disabled={isRevoking}
          on:click={confirmRevokeAccess}
          class="px-3 py-1.5 text-xs font-semibold bg-red-600 hover:bg-red-500 text-white rounded transition-colors flex items-center gap-1.5 disabled:opacity-50"
        >
          {#if isRevoking}
            <svg class="w-3.5 h-3.5 animate-spin" viewBox="0 0 24 24" fill="none">
              <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
              <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
            </svg>
            Revoking...
          {:else}
            Revoke Access
          {/if}
        </button>
      </div>
    </div>
  </div>
{/if}
