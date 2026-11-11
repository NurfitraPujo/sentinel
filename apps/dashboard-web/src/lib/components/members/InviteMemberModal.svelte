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

  // -----------------------------------------------------------------------------------------------
  // sentinel-project-environments task 14 — the optional environment scope control. Contract §4.10a,
  // decisions 28, 39 and 44, flows `I1`-`I5`.
  //
  // The SHAPE of this control is the whole point. Decision 28 is (c): a new member gets no grant row
  // and is unrestricted unless the inviter explicitly chooses otherwise. So nothing here is
  // pre-selected, an inviter who ignores the control sends exactly today's invitation body, and
  // `environmentScopes` is omitted ENTIRELY rather than sent as `[]` — a control that always sends a
  // value has quietly made this (b), the option the operator rejected, and would narrow people by
  // machine on a screen where nobody meant to.
  // -----------------------------------------------------------------------------------------------

  interface ScopeProject {
    id: string;
    name: string;
  }

  let scopeProjects: ScopeProject[] = [];
  let scopeProjectsLoaded = false;
  // Per project: the environment names the inviter has ticked. A project with no entry, or an empty
  // one, contributes nothing to the request — which is how "chose nothing" stays the default.
  let selectedByProject: Record<string, string[]> = {};
  // Per project: the registry, loaded on first expand.
  let environmentsByProject: Record<string, string[]> = {};
  let environmentsLoadedFor: Record<string, boolean> = {};
  let expandedProjectId: string | null = null;

  // Flow `I5`. An organization owner cannot be scoped — `422`, the same rule and the same reason as
  // the grants endpoint's: the person who can undo a narrowing must not be the one narrowed out.
  // The control is not rendered at all for that role, with the reason said once.
  $: inviteeIsOwner = selectedRole === 'owner';

  $: chosenScopes = Object.entries(selectedByProject)
    .filter(([, names]) => names.length > 0)
    .map(([projectId, environments]) => ({ projectId, environments }));

  function resetForm() {
    email = '';
    selectedRole = 'engineer';
    loading = false;
    errorMessage = '';
    invitationSent = false;
    selectedByProject = {};
    expandedProjectId = null;
  }

  async function loadScopeProjects() {
    try {
      // Decision 31 added this endpoint because §4.10a's control is per project and nothing listed an
      // organization's projects for a caller before. It returns the projects the caller can actually
      // reach, which is also flow `I4b`: a project the inviter could not scope is not offered at all.
      const res = await fetch(`/api/organizations/${orgId}/projects`);
      if (!res.ok) return;
      const body = await res.json();
      scopeProjects = (body.projects ?? []) as ScopeProject[];
    } catch {
      scopeProjects = [];
    } finally {
      scopeProjectsLoaded = true;
    }
  }

  async function loadEnvironments(projectId: string) {
    if (environmentsLoadedFor[projectId]) return;
    try {
      // §4.1 with `includeHidden=true` — the management list, gated on `write` (decision 44, one
      // gate). It is NOT filtered to the inviter's own scope: decision 39 reversed that, because
      // §4.10's grant `PUT` is explicitly not scope-guarded and would accept one screen later exactly
      // what a filtered list here would refuse.
      const res = await fetch(
        `/api/organizations/${orgId}/projects/${projectId}/environments?includeHidden=true`
      );
      if (!res.ok) return;
      const body = await res.json();
      environmentsByProject = {
        ...environmentsByProject,
        [projectId]: ((body.environments ?? []) as { name: string }[]).map((e) => e.name),
      };
    } catch {
      environmentsByProject = { ...environmentsByProject, [projectId]: [] };
    } finally {
      environmentsLoadedFor = { ...environmentsLoadedFor, [projectId]: true };
    }
  }

  function toggleProject(projectId: string) {
    expandedProjectId = expandedProjectId === projectId ? null : projectId;
    if (expandedProjectId) loadEnvironments(projectId);
  }

  function toggleEnvironment(projectId: string, name: string) {
    const current = selectedByProject[projectId] ?? [];
    const next = current.includes(name)
      ? current.filter((n) => n !== name)
      : [...current, name];
    // One entry per project, keyed by projectId — the request can never name the same project twice
    // (`I7`, `400 environmentScopes names the same project twice`), because the grant table is keyed
    // `(user_id, project_id)` and a duplicate would abort the whole membership transaction at
    // acceptance and leave the invitee unable to join at all.
    selectedByProject = { ...selectedByProject, [projectId]: next };
  }

  $: if (show && !scopeProjectsLoaded) {
    loadScopeProjects();
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
      // Flow `I1` and criterion 22: an untouched control sends NO `environmentScopes` field. The key
      // is added only when something was actually chosen, and never for an owner (`I5`).
      const payload: Record<string, unknown> = { email: email.trim(), role: selectedRole };
      if (!inviteeIsOwner && chosenScopes.length > 0) {
        payload.environmentScopes = chosenScopes;
      }

      const response = await fetch(`/api/organizations/${orgId}/invitations`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
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
    class="modal-backdrop"
    role="dialog"
    aria-modal="true"
    aria-labelledby="invite-modal-title"
    tabindex="-1"
    on:click|self={handleClose}
    on:keydown={(e) => {
      if (e.key === 'Escape') handleClose();
    }}
  >
    <div class="modal">
      <div class="modal-head">
        <h2 id="invite-modal-title" class="modal-title">
          <svg class="title-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M18 9v3m0 0v3m0-3h3m-3 0h-3m-2-5a4 4 0 11-8 0 4 4 0 018 0zM3 20a6 6 0 0112 0v1H3v-1z" />
          </svg>
          Invite Team Member
        </h2>
        <button type="button" on:click={handleClose} class="modal-close" aria-label="Close modal">
          <svg class="close-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24" aria-hidden="true">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
      </div>

      {#if invitationSent}
        <div class="stack">
          <div class="notice notice-success">
            Invitation created for <strong>{email}</strong>. We've sent them an email with a link to
            accept it. If they don't receive it, they can be re-invited from this org's outstanding
            invitations list.
          </div>

          <div class="modal-actions">
            <button type="button" on:click={handleClose} class="btn btn-secondary">
              Done
            </button>
          </div>
        </div>
      {:else}
        <form on:submit|preventDefault={handleSubmit} class="stack">
          {#if errorMessage}
            <div class="notice notice-error">
              {errorMessage}
            </div>
          {/if}

          <div>
            <label for="invite-email" class="field-label">
              Email Address
            </label>
            <input
              id="invite-email"
              type="email"
              bind:this={emailInputEl}
              bind:value={email}
              required
              placeholder="colleague@company.com"
              class="field-input"
            />
          </div>

          <div>
            <label for="invite-role" class="field-label">
              Organization Role
            </label>
            <select id="invite-role" bind:value={selectedRole} class="field-input">
              {#each roles as role}
                <option value={role.value}>{role.label}</option>
              {/each}
            </select>
          </div>

          <!--
            Task 14's control. It is optional and deliberately quiet: an inviter who scrolls past it
            sends today's invitation and the new member is unrestricted. What it does is said in
            words beside it, because a silent optional control on an invite screen is a control
            nobody uses on purpose.
          -->
          <div data-testid="environment-scope-control">
            <span class="field-label">
              Environment access (optional)
            </span>
            {#if inviteeIsOwner}
              <!-- Flow `I5`: no control at all for an owner, with the reason said once. -->
              <p class="scope-hint" data-testid="owner-scope-note">
                An organization owner cannot be limited to specific environments.
              </p>
            {:else}
              <p class="scope-hint scope-hint-lead">
                Leave this alone and the new member sees every environment of every project they can
                reach — that is the normal invitation. Anything chosen here can be changed later in
                the project's settings.
              </p>

              {#if !scopeProjectsLoaded}
                <p class="scope-note">Loading projects…</p>
              {:else if scopeProjects.length === 0}
                <p class="scope-note">
                  There are no projects here you can limit access in.
                </p>
              {:else}
                <ul class="scope-list">
                  {#each scopeProjects as project (project.id)}
                    <li class="scope-item">
                      <button
                        type="button"
                        class="scope-toggle"
                        on:click={() => toggleProject(project.id)}
                      >
                        <span>{project.name}</span>
                        <span class="scope-toggle-value">
                          {#if (selectedByProject[project.id] ?? []).length > 0}
                            {(selectedByProject[project.id] ?? []).join(', ')}
                          {:else}
                            All environments
                          {/if}
                        </span>
                      </button>

                      {#if expandedProjectId === project.id}
                        <div class="scope-body">
                          {#if !environmentsLoadedFor[project.id]}
                            <p class="scope-note">Loading environments…</p>
                          {:else if (environmentsByProject[project.id] ?? []).length === 0}
                            <!--
                              Flow `I3`. Under decision 8 this is EVERY project on day one — no
                              registry row is ever created by traffic — so it is the state an inviter
                              sees first, and an empty multi-select here would read as broken.
                            -->
                            <p class="scope-hint" data-testid="invite-registry-empty">
                              No environments have been declared in this project yet, so there is
                              nothing to limit access to. Declare them in the project's settings; until
                              then anyone with access to it sees everything it reports.
                            </p>
                          {:else}
                            {#each environmentsByProject[project.id] as name (name)}
                              <label class="scope-env">
                                <input
                                  type="checkbox"
                                  checked={(selectedByProject[project.id] ?? []).includes(name)}
                                  on:change={() => toggleEnvironment(project.id, name)}
                                />
                                <span>{name}</span>
                              </label>
                            {/each}
                          {/if}
                        </div>
                      {/if}
                    </li>
                  {/each}
                </ul>
              {/if}
            {/if}
          </div>

          <div class="modal-actions">
            <button type="button" on:click={handleClose} disabled={loading} class="btn btn-ghost">
              Cancel
            </button>
            <button type="submit" disabled={loading} class="btn btn-primary">
              {#if loading}
                <svg class="btn-icon spin" viewBox="0 0 24 24" fill="none" aria-hidden="true">
                  <circle cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" opacity="0.25"></circle>
                  <path fill="currentColor" opacity="0.75" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
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

<style>
  /* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind. */
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
    max-width: 28rem;
    padding: 1.5rem;
    background-color: var(--bg-surface);
    border: 1px solid var(--border-color);
    border-radius: var(--radius-md);
    color: var(--text-primary);
  }

  .modal-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    border-bottom: 1px solid var(--border-color);
    padding-bottom: 1rem;
    margin-bottom: 1rem;
  }

  .modal-title {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    font-size: 1rem;
    font-weight: 600;
  }

  .title-icon {
    width: 1.25rem;
    height: 1.25rem;
    flex-shrink: 0;
    color: var(--color-primary);
  }

  .modal-close {
    background: none;
    border: none;
    padding: 0.25rem;
    color: var(--text-muted);
    cursor: pointer;
  }

  .modal-close:hover {
    color: var(--text-primary);
  }

  .close-icon {
    width: 1.25rem;
    height: 1.25rem;
    display: block;
  }

  .stack {
    display: flex;
    flex-direction: column;
    gap: 1rem;
  }

  .notice {
    padding: 0.75rem;
    border-radius: var(--radius-sm);
    font-size: 0.8125rem;
    line-height: 1.5;
  }

  .notice-success {
    background-color: var(--status-resolved-bg);
    border: 1px solid rgba(16, 185, 129, 0.3);
    color: var(--status-resolved-text);
  }

  .notice-error {
    background-color: var(--severity-critical-bg);
    border: 1px solid var(--severity-critical-border);
    color: var(--severity-critical-text);
    font-size: 0.75rem;
  }

  .field-label {
    display: block;
    font-family: var(--font-mono);
    font-size: 0.75rem;
    text-transform: uppercase;
    color: var(--text-muted);
    margin-bottom: 0.25rem;
  }

  .field-input {
    width: 100%;
    padding: 0.5rem 0.75rem;
    font-size: 0.875rem;
  }

  /* Task 14's environment-scope control, in this file's idiom rather than Tailwind. */
  .scope-hint {
    font-size: 0.75rem;
    color: var(--text-muted);
  }

  .scope-hint-lead {
    margin-bottom: 0.5rem;
  }

  .scope-note {
    font-size: 0.75rem;
    color: var(--text-subtle);
  }

  .scope-list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: flex;
    flex-direction: column;
    gap: 0.25rem;
  }

  .scope-item {
    border: 1px solid var(--border-color);
    border-radius: var(--radius-sm);
  }

  .scope-toggle {
    width: 100%;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
    padding: 0.5rem 0.75rem;
    font-size: 0.75rem;
    color: var(--text-primary);
    background: none;
    border: none;
    cursor: pointer;
    text-align: left;
  }

  .scope-toggle:hover {
    background-color: var(--bg-surface-hover);
  }

  .scope-toggle-value {
    color: var(--text-muted);
  }

  .scope-body {
    padding: 0.5rem 0.75rem;
    border-top: 1px solid var(--border-color);
  }

  .scope-env {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.125rem 0;
    font-size: 0.75rem;
    color: var(--text-primary);
  }

  .modal-actions {
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 0.75rem;
    padding-top: 1rem;
    border-top: 1px solid var(--border-color);
  }

  .btn {
    display: inline-flex;
    align-items: center;
    gap: 0.5rem;
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

  .btn-secondary {
    background-color: var(--bg-surface-hover);
    color: var(--text-primary);
    border: 1px solid var(--border-color);
    font-weight: 500;
  }

  .btn-ghost {
    background: none;
    color: var(--text-muted);
    font-weight: 500;
  }

  .btn-ghost:hover:not(:disabled) {
    color: var(--text-primary);
  }
</style>
