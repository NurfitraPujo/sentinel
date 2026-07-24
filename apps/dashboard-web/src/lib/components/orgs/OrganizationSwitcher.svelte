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

<div class="relative inline-block text-left">
  <div>
    <button
      type="button"
      class="inline-flex w-full justify-center gap-x-1.5 rounded-md bg-white px-3 py-2 text-sm font-semibold text-gray-900 shadow-sm ring-1 ring-inset ring-gray-300 hover:bg-gray-50"
      on:click={toggle}
      aria-expanded={isOpen}
      aria-haspopup="true"
    >
      {#if activeOrg?.avatarUrl}
        <img src={activeOrg.avatarUrl} alt="" class="h-5 w-5 rounded-full" />
      {/if}
      {activeOrg?.name || 'Select Organization'}
      <svg class="-mr-1 h-5 w-5 text-gray-400" viewBox="0 0 20 20" fill="currentColor" aria-hidden="true">
        <path fill-rule="evenodd" d="M5.23 7.21a.75.75 0 011.06.02L10 11.168l3.71-3.938a.75.75 0 111.08 1.04l-4.25 4.5a.75.75 0 01-1.08 0l-4.25-4.5a.75.75 0 01.02-1.06z" clip-rule="evenodd" />
      </svg>
    </button>
  </div>

  {#if isOpen}
    <div
      class="absolute right-0 z-10 mt-2 w-56 origin-top-right rounded-md bg-white shadow-lg ring-1 ring-black ring-opacity-5 focus:outline-none"
      role="menu"
      aria-orientation="vertical"
      tabindex="-1"
    >
      <div class="py-1" role="none">
        {#each organizations as org}
          <a
            href="/{org.slug}"
            class="text-gray-700 block px-4 py-2 text-sm hover:bg-gray-100"
            role="menuitem"
            tabindex="-1"
          >
            {org.name}
          </a>
        {/each}
        <div class="border-t border-gray-100"></div>
        <button
          class="text-gray-700 block w-full px-4 py-2 text-left text-sm hover:bg-gray-100"
          role="menuitem"
          tabindex="-1"
        >
          Create New Org
        </button>
      </div>
    </div>
  {/if}
</div>
