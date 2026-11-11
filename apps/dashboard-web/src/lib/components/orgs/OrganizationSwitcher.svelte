<script lang="ts">
  export let activeOrg: { id: string; name: string; avatarUrl?: string | null; slug: string } | null = null;
  export let organizations: Array<{ id: string; name: string; avatarUrl?: string | null; slug: string }> = [];
  
  let isOpen = false;
  let switcherEl: HTMLElement;

  function toggle() {
    isOpen = !isOpen;
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') isOpen = false;
  }

  function handleClickOutside(e: MouseEvent) {
    if (isOpen && switcherEl && !switcherEl.contains(e.target as Node)) {
      isOpen = false;
    }
  }
</script>

<svelte:window on:keydown={handleKeydown} on:click={handleClickOutside} />

<div class="switcher" bind:this={switcherEl}>
  <button
    type="button"
    class="switcher-trigger"
    on:click|stopPropagation={toggle}
    aria-expanded={isOpen}
    aria-haspopup="true"
    aria-label="Switch organization"
  >
    {#if activeOrg?.avatarUrl}
      <img src={activeOrg.avatarUrl} alt="" class="avatar" />
    {/if}
    <span class="org-name">{activeOrg?.name || 'Select Organization'}</span>
    <svg class="chevron" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
      <path fill-rule="evenodd" d="M5.23 7.21a.75.75 0 011.06.02L10 11.168l3.71-3.938a.75.75 0 111.08 1.04l-4.25 4.5a.75.75 0 01-1.08 0l-4.25-4.5a.75.75 0 01.02-1.06z" clip-rule="evenodd" />
    </svg>
  </button>

  {#if isOpen}
    <div class="menu" role="menu" aria-orientation="vertical" tabindex="-1">
      <div class="menu-label">Organizations</div>
      {#each organizations as org}
        <a
          href="/{org.slug}/projects"
          class="menu-item {activeOrg?.id === org.id || activeOrg?.slug === org.slug ? 'active' : ''}"
          role="menuitem"
          tabindex="-1"
          on:click={() => (isOpen = false)}
        >
          {#if org.avatarUrl}
            <img src={org.avatarUrl} alt="" class="avatar-sm" />
          {/if}
          <span class="menu-item-text">{org.name}</span>
          {#if activeOrg?.id === org.id || activeOrg?.slug === org.slug}
            <span class="active-badge" aria-hidden="true">✓</span>
          {/if}
        </a>
      {/each}
      <div class="menu-divider"></div>
      <a
        href="/onboarding"
        class="menu-item menu-item-create"
        role="menuitem"
        tabindex="-1"
        on:click={() => (isOpen = false)}
      >
        <span class="create-icon">+</span> Create New Organization
      </a>
    </div>
  {/if}
</div>

<style>
  .switcher {
    position: relative;
    display: inline-block;
    text-align: left;
  }

  .switcher-trigger {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 0.375rem;
    padding: 0.375rem 0.625rem;
    border-radius: var(--radius-sm, 4px);
    background-color: var(--bg-surface-hover, #1e293b);
    border: 1px solid var(--border-color, #334155);
    color: var(--text-primary, #f8fafc);
    font-size: 0.8125rem;
    font-weight: 500;
    cursor: pointer;
    transition: background-color var(--transition-fast, 150ms), border-color var(--transition-fast, 150ms);
  }

  .switcher-trigger:hover {
    background-color: var(--bg-root, #0f172a);
    border-color: var(--color-primary, #3b82f6);
  }

  .avatar {
    width: 1.125rem;
    height: 1.125rem;
    border-radius: 999px;
    object-fit: cover;
  }

  .avatar-sm {
    width: 1rem;
    height: 1rem;
    border-radius: 999px;
    object-fit: cover;
  }

  .org-name {
    max-width: 10rem;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .chevron {
    width: 1rem;
    height: 1rem;
    color: var(--text-muted, #94a3b8);
  }

  .menu {
    position: absolute;
    left: 0;
    z-index: 50;
    margin-top: 0.5rem;
    width: 15rem;
    padding: 0.375rem 0;
    transform-origin: top left;
    border-radius: var(--radius-md, 6px);
    background-color: var(--bg-surface, #1e293b);
    border: 1px solid var(--border-color, #334155);
    box-shadow: 0 10px 25px rgba(0, 0, 0, 0.5);
  }

  .menu-label {
    padding: 0.25rem 0.75rem;
    font-size: 0.6875rem;
    font-weight: 600;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    color: var(--text-muted, #94a3b8);
  }

  .menu-item {
    display: flex;
    align-items: center;
    gap: 0.5rem;
    width: 100%;
    padding: 0.5rem 0.75rem;
    font-size: 0.8125rem;
    color: var(--text-primary, #f8fafc);
    text-decoration: none;
    text-align: left;
    transition: background-color var(--transition-fast, 150ms);
  }

  .menu-item:hover {
    background-color: var(--bg-surface-hover, #334155);
  }

  .menu-item.active {
    font-weight: 600;
    color: var(--color-primary, #3b82f6);
  }

  .menu-item-text {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .active-badge {
    color: var(--color-primary, #3b82f6);
    font-weight: 700;
    font-size: 0.875rem;
  }

  .menu-item-create {
    color: var(--text-muted, #94a3b8);
  }

  .menu-item-create:hover {
    color: var(--text-primary, #f8fafc);
  }

  .create-icon {
    font-weight: 700;
    color: var(--color-primary, #3b82f6);
  }

  .menu-divider {
    border-top: 1px solid var(--border-color, #334155);
    margin: 0.375rem 0;
  }
</style>
