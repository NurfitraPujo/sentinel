<script lang="ts">
  export let activeProject: { id: string; name: string; slug: string } | null = null;
  export let projects: Array<{ id: string; name: string; slug: string }> = [];
  export let orgSlug: string;

  let isOpen = false;

  function toggle() {
    isOpen = !isOpen;
  }
</script>

<div class="relative inline-block text-left">
  <button
    type="button"
    class="inline-flex items-center gap-x-2 rounded-md bg-gray-100 px-3 py-2 text-sm font-medium text-gray-900 hover:bg-gray-200 focus:outline-none focus:ring-2 focus:ring-indigo-500"
    on:click={toggle}
  >
    {activeProject ? activeProject.name : 'Select Project'}
    <span class="text-xs text-gray-500 ml-2 border border-gray-300 rounded px-1">⌘K</span>
  </button>

  {#if isOpen}
    <div class="absolute left-0 z-10 mt-2 w-64 origin-top-left rounded-md bg-white shadow-lg ring-1 ring-black ring-opacity-5 focus:outline-none">
      <div class="p-2">
        <input
          type="text"
          class="block w-full rounded-md border-0 py-1.5 text-gray-900 shadow-sm ring-1 ring-inset ring-gray-300 placeholder:text-gray-400 focus:ring-2 focus:ring-inset focus:ring-indigo-600 sm:text-sm sm:leading-6"
          placeholder="Search projects..."
        />
      </div>
      <div class="max-h-60 overflow-y-auto py-1">
        {#each projects as project}
          <a
            href="/{orgSlug}/{project.slug}"
            class="block px-4 py-2 text-sm text-gray-700 hover:bg-gray-100"
          >
            {project.name}
          </a>
        {/each}
        {#if projects.length === 0}
          <div class="px-4 py-2 text-sm text-gray-500">No projects found.</div>
        {/if}
      </div>
    </div>
  {/if}
</div>
