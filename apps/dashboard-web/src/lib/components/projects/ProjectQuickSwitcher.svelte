<script lang="ts">
  export let activeProject: { id: string; name: string; slug: string } | null = null;
  export let projects: Array<{ id: string; name: string; slug: string }> = [];
  export let orgSlug: string;

  let isOpen = false;

  function toggle() {
    isOpen = !isOpen;
  }
</script>

<div class="switcher">
  <button type="button" class="switcher-trigger" on:click={toggle}>
    {activeProject ? activeProject.name : 'Select Project'}
    <span class="kbd">⌘K</span>
  </button>

  {#if isOpen}
    <div class="menu">
      <div class="menu-search">
        <input type="text" class="search-input" placeholder="Search projects..." />
      </div>
      <div class="menu-list">
        {#each projects as project}
          <a href="/{orgSlug}/{project.slug}" class="menu-item">
            {project.name}
          </a>
        {/each}
        {#if projects.length === 0}
          <div class="menu-empty">No projects found.</div>
        {/if}
      </div>
    </div>
  {/if}
</div>

<style>
  /* Rewritten from Tailwind utility classes, which never resolved — this project has no Tailwind. */
  .switcher {
    position: relative;
    display: inline-block;
    text-align: left;
  }

  .switcher-trigger {
    display: inline-flex;
    align-items: center;
    gap: 0.5rem;
    padding: 0.5rem 0.75rem;
    border-radius: var(--radius-md);
    background-color: var(--bg-surface);
    border: 1px solid var(--border-color);
    color: var(--text-primary);
    font-size: 0.875rem;
    font-weight: 500;
    cursor: pointer;
  }

  .switcher-trigger:hover {
    background-color: var(--bg-surface-hover);
  }

  .kbd {
    font-family: var(--font-mono);
    font-size: 0.6875rem;
    color: var(--text-muted);
    border: 1px solid var(--border-color);
    border-radius: var(--radius-sm);
    padding: 0 0.25rem;
    margin-left: 0.5rem;
  }

  .menu {
    position: absolute;
    left: 0;
    z-index: 10;
    margin-top: 0.5rem;
    width: 16rem;
    transform-origin: top left;
    border-radius: var(--radius-md);
    background-color: var(--bg-surface);
    border: 1px solid var(--border-color);
    box-shadow: 0 10px 25px rgba(0, 0, 0, 0.4);
  }

  .menu-search {
    padding: 0.5rem;
  }

  .search-input {
    display: block;
    width: 100%;
    padding: 0.375rem 0.625rem;
    font-size: 0.875rem;
  }

  .menu-list {
    max-height: 15rem;
    overflow-y: auto;
    padding: 0.25rem 0;
  }

  .menu-item {
    display: block;
    padding: 0.5rem 1rem;
    font-size: 0.875rem;
    color: var(--text-primary);
    text-decoration: none;
  }

  .menu-item:hover {
    background-color: var(--bg-surface-hover);
  }

  .menu-empty {
    padding: 0.5rem 1rem;
    font-size: 0.875rem;
    color: var(--text-muted);
  }
</style>
