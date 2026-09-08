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

          <!--
            Task 14's control. It is optional and deliberately quiet: an inviter who scrolls past it
            sends today's invitation and the new member is unrestricted. What it does is said in
            words beside it, because a silent optional control on an invite screen is a control
            nobody uses on purpose.
          -->
          <div data-testid="environment-scope-control">
            <span class="block text-xs font-mono uppercase text-slate-400 mb-1">
              Environment access (optional)
            </span>
            {#if inviteeIsOwner}
              <!-- Flow `I5`: no control at all for an owner, with the reason said once. -->
              <p class="text-xs text-slate-400" data-testid="owner-scope-note">
                An organization owner cannot be limited to specific environments.
              </p>
            {:else}
              <p class="text-xs text-slate-400 mb-2">
                Leave this alone and the new member sees every environment of every project they can
                reach — that is the normal invitation. Anything chosen here can be changed later in
                the project's settings.
              </p>

              {#if !scopeProjectsLoaded}
                <p class="text-xs text-slate-500">Loading projects…</p>
              {:else if scopeProjects.length === 0}
                <p class="text-xs text-slate-500">
                  There are no projects here you can limit access in.
                </p>
              {:else}
                <ul class="space-y-1">
                  {#each scopeProjects as project (project.id)}
                    <li class="border border-slate-800 rounded">
                      <button
                        type="button"
                        class="w-full flex items-center justify-between px-3 py-2 text-xs text-slate-200 hover:bg-slate-800/60"
                        on:click={() => toggleProject(project.id)}
                      >
                        <span>{project.name}</span>
                        <span class="text-slate-400">
                          {#if (selectedByProject[project.id] ?? []).length > 0}
                            {(selectedByProject[project.id] ?? []).join(', ')}
                          {:else}
                            All environments
                          {/if}
                        </span>
                      </button>

                      {#if expandedProjectId === project.id}
                        <div class="px-3 pb-2 border-t border-slate-800 pt-2">
                          {#if !environmentsLoadedFor[project.id]}
                            <p class="text-xs text-slate-500">Loading environments…</p>
                          {:else if (environmentsByProject[project.id] ?? []).length === 0}
                            <!--
                              Flow `I3`. Under decision 8 this is EVERY project on day one — no
                              registry row is ever created by traffic — so it is the state an inviter
                              sees first, and an empty multi-select here would read as broken.
                            -->
                            <p class="text-xs text-slate-400" data-testid="invite-registry-empty">
                              No environments have been declared in this project yet, so there is
                              nothing to limit access to. Declare them in the project's settings; until
                              then anyone with access to it sees everything it reports.
                            </p>
                          {:else}
                            {#each environmentsByProject[project.id] as name (name)}
                              <label class="flex items-center gap-2 text-xs text-slate-200 py-0.5">
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
