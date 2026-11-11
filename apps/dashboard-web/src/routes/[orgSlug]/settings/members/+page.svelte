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

<div class="members-page">
  <div class="page-inner">
    <div class="page-header">
      <div>
        <h1 class="page-title">
          <svg class="title-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 4.354a4 4 0 110 5.292M15 21H3v-1a6 6 0 0112 0v1zm0 0h6v-1a6 6 0 00-9-5.197M13 7a4 4 0 11-8 0 4 4 0 018 0z" />
          </svg>
          Organization Members
        </h1>
        <p class="page-subtitle">
          Manage team members, permissions, and roles within this organization.
        </p>
      </div>
      <button type="button" on:click={() => (showInviteModal = true)} class="btn btn-primary">
        <svg class="btn-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 4v16m8-8H4" />
        </svg>
        Invite Member
      </button>
    </div>

    {#if toastMessage}
      <div role="status" aria-live="polite" class="toast" class:toast-error={toastType === 'error'} class:toast-success={toastType !== 'error'}>
        <span>{toastMessage}</span>
        <button type="button" on:click={() => (toastMessage = null)} aria-label="Dismiss notification" class="toast-dismiss">
          ✕
        </button>
      </div>
    {/if}

    <div class="table-card">
      <div class="table-scroll">
        <table class="members-table">
          <thead>
            <tr>
              <th scope="col">User</th>
              <th scope="col">Email</th>
              <th scope="col">Role</th>
              <th scope="col" class="align-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            {#each members as member}
              <tr>
                <td class="cell-name">{member.user.name}</td>
                <td class="cell-email">{member.user.email}</td>
                <td>
                  <select
                    value={member.role}
                    disabled={updatingMemberId === member.id}
                    aria-label={`Role for ${member.user.name}`}
                    on:change={(e) => handleRoleChange(member.id, e.currentTarget.value)}
                    class="role-select"
                  >
                    {#each roles as role}
                      <option value={role}>{role}</option>
                    {/each}
                  </select>
                </td>
                <td class="align-right">
                  <button
                    type="button"
                    aria-label={`Revoke access for ${member.user.name}`}
                    on:click={() => promptRevokeAccess(member.id, member.user.name)}
                    class="link-danger"
                  >
                    Revoke Access
                  </button>
                </td>
              </tr>
            {/each}
            {#if members.length === 0}
              <tr>
                <td colspan="4" class="cell-empty">
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

<InviteMemberModal
  bind:show={showInviteModal}
  orgId={data.orgId}
  on:invited={handleInvited}
/>

{#if revokingMember}
  <div
    class="modal-backdrop"
    role="dialog"
    aria-modal="true"
    aria-labelledby="revoke-modal-title"
    aria-describedby="revoke-modal-desc"
    tabindex="-1"
    on:click|self={() => (revokingMember = null)}
    on:keydown={(e) => {
      if (e.key === 'Escape') revokingMember = null;
    }}
  >
    <div class="modal">
      <div class="modal-head">
        <svg class="modal-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z" />
        </svg>
        <h3 id="revoke-modal-title" class="modal-title">Revoke Organization Access</h3>
      </div>
      <p id="revoke-modal-desc" class="modal-body">
        Are you sure you want to revoke access for <strong>{revokingMember.name}</strong>? They will lose access to all organization resources and projects immediately.
      </p>
      <div class="modal-actions">
        <button type="button" disabled={isRevoking} on:click={() => (revokingMember = null)} class="btn btn-ghost">
          Cancel
        </button>
        <button type="button" disabled={isRevoking} on:click={confirmRevokeAccess} class="btn btn-danger">
          {#if isRevoking}
            <svg class="btn-icon spin" viewBox="0 0 24 24" fill="none" aria-hidden="true">
              <circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" opacity="0.25"></circle>
              <path fill="currentColor" opacity="0.75" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
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

<style>
  /* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind.
     The unconstrained `w-5 h-5` icons are why this page rendered with a full-width SVG. */
  .members-page {
    min-height: calc(100vh - 120px);
    padding: 1.5rem 1rem;
  }

  .page-inner {
    max-width: 72rem;
    margin: 0 auto;
  }

  .page-header {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 1rem;
    border-bottom: 1px solid var(--border-color);
    padding-bottom: 1rem;
    margin-bottom: 1.5rem;
  }

  .page-title {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 1.125rem;
    font-weight: 700;
    color: var(--text-primary);
  }

  /* The whole point of the original `w-5 h-5`: without a size an inline SVG fills its container. */
  .title-icon {
    width: 1.25rem;
    height: 1.25rem;
    flex-shrink: 0;
    color: var(--color-primary);
  }

  .page-subtitle {
    font-size: 0.75rem;
    color: var(--text-muted);
    margin-top: 0.25rem;
  }

  .btn {
    display: inline-flex;
    align-items: center;
    gap: 0.375rem;
    padding: 0.5rem 1rem;
    border-radius: var(--radius-sm);
    font-size: 0.75rem;
    font-weight: 600;
    border: none;
    cursor: pointer;
  }

  .btn:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }

  .btn-icon {
    width: 0.875rem;
    height: 0.875rem;
    flex-shrink: 0;
  }

  .spin {
    animation: spin 0.8s linear infinite;
  }

  @keyframes spin {
    to { transform: rotate(360deg); }
  }

  .btn-primary {
    background-color: var(--color-primary);
    color: var(--text-primary);
  }

  .btn-primary:hover:not(:disabled) {
    background-color: var(--color-primary-hover);
  }

  .btn-ghost {
    background: none;
    color: var(--text-muted);
    font-weight: 500;
  }

  .btn-ghost:hover:not(:disabled) {
    color: var(--text-primary);
  }

  .btn-danger {
    background-color: var(--severity-critical-text);
    color: var(--text-primary);
  }

  .toast {
    display: flex;
    justify-content: space-between;
    align-items: center;
    gap: 1rem;
    margin-bottom: 1rem;
    padding: 0.75rem 1rem;
    border-radius: var(--radius-sm);
    font-size: 0.75rem;
    font-weight: 500;
    border: 1px solid transparent;
  }

  .toast-error {
    background-color: var(--severity-critical-bg);
    border-color: var(--severity-critical-border);
    color: var(--severity-critical-text);
  }

  .toast-success {
    background-color: var(--status-resolved-bg);
    border-color: rgba(16, 185, 129, 0.3);
    color: var(--status-resolved-text);
  }

  .toast-dismiss {
    background: none;
    border: none;
    color: inherit;
    font-weight: 700;
    cursor: pointer;
    opacity: 0.75;
    padding: 0.25rem;
  }

  .toast-dismiss:hover {
    opacity: 1;
  }

  .table-card {
    background-color: var(--bg-surface);
    border: 1px solid var(--border-color);
    border-radius: var(--radius-md);
    overflow: hidden;
  }

  .table-scroll {
    overflow-x: auto;
  }

  .members-table {
    width: 100%;
    text-align: left;
    border-collapse: collapse;
    font-size: 0.75rem;
  }

  .members-table thead tr {
    background-color: var(--bg-root);
    border-bottom: 1px solid var(--border-color);
  }

  .members-table th {
    padding: 0.75rem 1rem;
    font-family: var(--font-mono);
    font-size: 0.6875rem;
    font-weight: 600;
    text-transform: uppercase;
    color: var(--text-muted);
  }

  .members-table td {
    padding: 0.625rem 1rem;
    border-top: 1px solid var(--border-color-muted);
  }

  .members-table tbody tr:hover {
    background-color: var(--bg-surface-hover);
  }

  .align-right {
    text-align: right;
  }

  .cell-name {
    font-weight: 500;
    color: var(--text-primary);
  }

  .cell-email {
    font-family: var(--font-mono);
    color: var(--text-muted);
    max-width: 320px;
    overflow: hidden;
    text-overflow: ellipsis;
  }

  .cell-empty {
    padding: 3rem 1rem;
    text-align: center;
    font-family: var(--font-mono);
    color: var(--text-subtle);
  }

  .role-select {
    font-family: var(--font-mono);
    font-size: 0.75rem;
    padding: 0.25rem 0.625rem;
  }

  .role-select:disabled {
    opacity: 0.5;
  }

  .link-danger {
    background: none;
    border: none;
    padding: 0.25rem 0.5rem;
    border-radius: var(--radius-sm);
    font-size: 0.75rem;
    font-weight: 500;
    color: var(--severity-critical-text);
    cursor: pointer;
  }

  .link-danger:hover {
    background-color: var(--severity-critical-bg);
  }

  .modal-backdrop {
    position: fixed;
    inset: 0;
    z-index: 50;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 1rem;
    background-color: rgba(15, 23, 42, 0.8);
  }

  .modal {
    width: 100%;
    max-width: 24rem;
    padding: 1.25rem;
    background-color: var(--bg-surface);
    border: 1px solid var(--border-color);
    border-radius: var(--radius-md);
    color: var(--text-primary);
  }

  .modal-head {
    display: flex;
    align-items: center;
    gap: 0.75rem;
    color: var(--severity-critical-text);
    margin-bottom: 1rem;
  }

  .modal-icon {
    width: 1.5rem;
    height: 1.5rem;
    flex-shrink: 0;
  }

  .modal-title {
    font-size: 0.875rem;
    font-weight: 600;
    color: var(--text-primary);
  }

  .modal-body {
    font-size: 0.75rem;
    color: var(--text-muted);
    line-height: 1.6;
    margin-bottom: 1rem;
  }

  .modal-body strong {
    color: var(--text-primary);
  }

  .modal-actions {
    display: flex;
    justify-content: flex-end;
    gap: 0.75rem;
  }
</style>
