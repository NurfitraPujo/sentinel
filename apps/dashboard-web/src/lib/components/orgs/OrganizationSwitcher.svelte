<script lang="ts">
  export let activeOrg: { id: string; name: string; avatarUrl?: string; slug: string };
  export let organizations: Array<{ id: string; name: string; avatarUrl?: string; slug: string }> = [];
  
  let isOpen = false;

  function toggle() {
    isOpen = !isOpen;
  }

  function handleKeydown(e: KeyboardEvent) {
    if (e.key === 'Escape') isOpen = false;
  }
</script>

<svelte:window on:keydown={handleKeydown} />

<div class="switcher">
  <button type="button" class="switcher-trigger" on:click={toggle} aria-expanded={isOpen} aria-haspopup="true">
    {#if activeOrg?.avatarUrl}
      <img src={activeOrg.avatarUrl} alt="" class="avatar" />
    {/if}
    {activeOrg?.name || 'Select Organization'}
    <svg class="chevron" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
      <path fill-rule="evenodd" d="M5.23 7.21a.75.75 0 011.06.02L10 11.168l3.71-3.938a.75.75 0 111.08 1.04l-4.25 4.5a.75.75 0 01-1.08 0l-4.25-4.5a.75.75 0 01.02-1.06z" clip-rule="evenodd" />
    </svg>
  </button>

  {#if isOpen}
    <div class="menu" role="menu" aria-orientation="vertical" tabindex="-1">
      {#each organizations as org}
        <a href="/{org.slug}" class="menu-item" role="menuitem" tabindex="-1">
          {org.name}
        </a>
      {/each}
      <div class="menu-divider"></div>
      <button class="menu-item menu-item-button" role="menuitem" tabindex="-1">
        Create New Org
      </button>
    </div>
  {/if}
</div>

<style>
  /* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind.
     The original asked for `bg-white` / `text-gray-900`: a light dropdown in a dark application,
     which is what a Tailwind starter ships, not a choice made for this app. */
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
    padding: 0.5rem 0.75rem;
    border-radius: var(--radius-md);
    background-color: var(--bg-surface);
    border: 1px solid var(--border-color);
    color: var(--text-primary);
    font-size: 0.875rem;
    font-weight: 600;
    cursor: pointer;
  }

  .switcher-trigger:hover {
    background-color: var(--bg-surface-hover);
  }

  .avatar {
    width: 1.25rem;
    height: 1.25rem;
    border-radius: 999px;
  }

  .chevron {
    width: 1.25rem;
    height: 1.25rem;
    color: var(--text-muted);
    margin-right: -0.25rem;
  }

  .menu {
    position: absolute;
    right: 0;
    z-index: 10;
    margin-top: 0.5rem;
    width: 14rem;
    padding: 0.25rem 0;
    transform-origin: top right;
    border-radius: var(--radius-md);
    background-color: var(--bg-surface);
    border: 1px solid var(--border-color);
    box-shadow: 0 10px 25px rgba(0, 0, 0, 0.4);
  }

  .menu-item {
    display: block;
    width: 100%;
    padding: 0.5rem 1rem;
    font-size: 0.875rem;
    color: var(--text-primary);
    text-decoration: none;
    text-align: left;
  }

  .menu-item:hover {
    background-color: var(--bg-surface-hover);
  }

  .menu-item-button {
    background: none;
    border: none;
    cursor: pointer;
    font: inherit;
  }

  .menu-divider {
    border-top: 1px solid var(--border-color);
    margin: 0.25rem 0;
  }
</style>
